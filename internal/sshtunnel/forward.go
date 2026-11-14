// @author: kevinkelly
// @license: Apache Licence
// @file: forward.go
// @time: 2026/10/28

package sshtunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Proto 是目标服务的协议，只用于“隧道失败”时把错误信息以对应协议的错误报文回给客户端，
// 让调用方（数据库驱动 / Redis 客户端）的错误里能看出是隧道的问题；不认识的协议只关闭连接。
type Proto string

const (
	ProtoNone  Proto = ""
	ProtoMySQL Proto = "mysql"
	ProtoRedis Proto = "redis"
)

// ProtoFor 按 dbt_sql_instance.db_type 推断协议。
func ProtoFor(dbType string) Proto {
	switch strings.ToLower(dbType) {
	case "mysql", "mariadb", "tidb", "polardb", "polardb-x", "selectdb", "doris", "adb", "oceanbase", "goldendb":
		return ProtoMySQL
	case "redis":
		return ProtoRedis
	}
	return ProtoNone
}

// Options 是转发器管理器的参数；零值取默认。
type Options struct {
	// ListenHost 转发器的监听地址，默认 127.0.0.1。要让别的主机上的 goInception 等也走隧道时可改为 0.0.0.0，
	// 这会把到内网数据库的转发暴露给能访问该端口的所有人，务必配合防火墙。
	ListenHost string
	// AdvertiseHost 写进 engine.Conn.Host 的地址，默认 127.0.0.1。
	AdvertiseHost string
	IdleTimeout   time.Duration // 没有任何连接超过这么久就关闭转发器，默认 10 分钟
	KeepAlive     time.Duration // SSH 保活间隔，默认 30 秒
	DialTimeout   time.Duration // 登录跳板机的超时，默认 10 秒
	CheckInterval time.Duration // 空闲检查间隔，默认 IdleTimeout/4（最少 1 秒，最多 1 分钟）
	MaxForwarders int           // 同时存在的转发器上限，默认 512
	// Dial 登录跳板机；默认 sshtunnel.Dial。测试用。
	Dial func(ctx context.Context, c Config, timeout time.Duration) (*ssh.Client, error)
}

func (o Options) withDefaults() Options {
	if o.ListenHost == "" {
		o.ListenHost = "127.0.0.1"
	}
	if o.AdvertiseHost == "" {
		o.AdvertiseHost = "127.0.0.1"
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 10 * time.Minute
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = 30 * time.Second
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 10 * time.Second
	}
	if o.CheckInterval <= 0 {
		o.CheckInterval = min(max(o.IdleTimeout/4, time.Second), time.Minute)
	}
	if o.MaxForwarders <= 0 {
		o.MaxForwarders = 512
	}
	if o.Dial == nil {
		o.Dial = Dial
	}
	return o
}

// Manager 管理所有转发器：按“隧道配置指纹 + 目标 host:port + 协议”缓存，并发安全。
type Manager struct {
	opt Options

	mu      sync.Mutex
	fws     map[string]*forwarder
	janitor bool
}

// NewManager 创建管理器。
func NewManager(o Options) *Manager {
	return &Manager{opt: o.withDefaults(), fws: map[string]*forwarder{}}
}

// Forward 返回一个本地地址（host, port）：连它等于经隧道连 target（host:port）。同样的配置和目标复用同一个转发器。
// 返回时不会真正连接跳板机（懒连接），所以跳板机不可达 / 认证失败体现为之后的连接失败，而不是这里的错误。
func (m *Manager) Forward(cfg Config, host string, port int, proto Proto) (string, int, error) {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	key := "ssh|" + cfg.Fingerprint() + "|" + target + "|" + string(proto)
	return m.get(key, cfg, nil, target, proto)
}

// Fail 返回一个“总是失败”的本地地址：每个入站连接都会收到 err 的说明（隧道配置缺失 / 不可用等），
// 让没有错误返回值的 EngineConn 也能把原因带给调用方。id 用来区分不同的失败来源。
func (m *Manager) Fail(id string, err error, host string, port int, proto Proto) (string, int, error) {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	key := "fail|" + id + "|" + err.Error() + "|" + target + "|" + string(proto)
	return m.get(key, Config{}, err, target, proto)
}

func (m *Manager) get(key string, cfg Config, initErr error, target string, proto Proto) (string, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f, ok := m.fws[key]; ok {
		f.touch()
		return m.opt.AdvertiseHost, f.port, nil
	}
	if len(m.fws) >= m.opt.MaxForwarders {
		return "", 0, fmt.Errorf("SSH 隧道转发器数量已达上限（%d）", m.opt.MaxForwarders)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(m.opt.ListenHost, "0"))
	if err != nil {
		return "", 0, fmt.Errorf("SSH 隧道本地监听失败：%w", err)
	}
	f := &forwarder{m: m, key: key, cfg: cfg, initErr: initErr, target: target, proto: proto, ln: ln,
		port: ln.Addr().(*net.TCPAddr).Port, lastUse: time.Now(), conns: map[net.Conn]struct{}{}}
	m.fws[key] = f
	go f.acceptLoop()
	if !m.janitor {
		m.janitor = true
		go m.sweepLoop()
	}
	if initErr == nil {
		slog.Info("SSH 隧道转发器已建立", "target", target, "local", ln.Addr().String(), "fp", cfg.Fingerprint()[:8])
	}
	return m.opt.AdvertiseHost, f.port, nil
}

// sweepLoop 周期性关闭空闲的转发器；没有转发器时退出，下次建立时再启动。
func (m *Manager) sweepLoop() {
	t := time.NewTicker(m.opt.CheckInterval)
	defer t.Stop()
	for range t.C {
		var idle []*forwarder
		m.mu.Lock()
		for k, f := range m.fws {
			if f.idleFor() > m.opt.IdleTimeout {
				delete(m.fws, k)
				idle = append(idle, f)
			}
		}
		empty := len(m.fws) == 0
		if empty {
			m.janitor = false
		}
		m.mu.Unlock()
		for _, f := range idle {
			slog.Info("SSH 隧道转发器空闲，已关闭", "target", f.target)
			f.close()
		}
		if empty {
			return
		}
	}
}

// CloseAll 关闭全部转发器及其连接（进程退出时调用）；之后仍可再次使用，会重新建立。
func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := make([]*forwarder, 0, len(m.fws))
	for k, f := range m.fws {
		all = append(all, f)
		delete(m.fws, k)
	}
	m.mu.Unlock()
	for _, f := range all {
		f.close()
	}
}

// Stat 是一个转发器的运行状态（诊断 / 测试用，不含任何凭据）。
type Stat struct {
	Target      string `json:"target"`
	Local       string `json:"local"`
	Fingerprint string `json:"fingerprint"` // 前 8 位
	Active      int    `json:"active"`      // 当前连接数
	Connected   bool   `json:"connected"`   // 是否持有到跳板机的 SSH 连接
	LastError   string `json:"last_error,omitempty"`
}

// Stats 返回全部转发器的状态，按目标排序。
func (m *Manager) Stats() []Stat {
	m.mu.Lock()
	fs := make([]*forwarder, 0, len(m.fws))
	for _, f := range m.fws {
		fs = append(fs, f)
	}
	m.mu.Unlock()
	out := make([]Stat, 0, len(fs))
	for _, f := range fs {
		f.mu.Lock()
		s := Stat{Target: f.target, Local: f.ln.Addr().String(), Active: f.active, Connected: f.cli != nil, LastError: f.lastErr}
		f.mu.Unlock()
		if f.initErr == nil {
			s.Fingerprint = f.cfg.Fingerprint()[:8]
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Target != out[j].Target {
			return out[i].Target < out[j].Target
		}
		return out[i].Local < out[j].Local
	})
	return out
}

var errClosed = errors.New("SSH 隧道转发器已关闭")

type forwarder struct {
	m       *Manager
	key     string
	cfg     Config
	initErr error // 非空表示固定失败的转发器
	target  string
	proto   Proto
	ln      net.Listener
	port    int

	dialMu sync.Mutex // 串行化登录跳板机，避免并发连接各登录一次

	mu      sync.Mutex
	cli     *ssh.Client
	active  int
	lastUse time.Time
	closed  bool
	lastErr string
	conns   map[net.Conn]struct{}
}

func (f *forwarder) touch() {
	f.mu.Lock()
	f.lastUse = time.Now()
	f.mu.Unlock()
}

func (f *forwarder) idleFor() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active > 0 {
		return 0
	}
	return time.Since(f.lastUse)
}

func (f *forwarder) acceptLoop() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			f.mu.Lock()
			closed := f.closed
			f.mu.Unlock()
			if closed {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			slog.Warn("SSH 隧道转发器监听中止", "target", f.target, "err", err)
			f.m.remove(f)
			f.close()
			return
		}
		go f.handle(c)
	}
}

func (m *Manager) remove(f *forwarder) {
	m.mu.Lock()
	if m.fws[f.key] == f {
		delete(m.fws, f.key)
	}
	m.mu.Unlock()
}

func (f *forwarder) track(c net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.conns[c] = struct{}{}
	return true
}

func (f *forwarder) untrack(c net.Conn) {
	f.mu.Lock()
	delete(f.conns, c)
	f.mu.Unlock()
}

func (f *forwarder) handle(in net.Conn) {
	defer in.Close()
	if !f.track(in) {
		return
	}
	defer f.untrack(in)
	f.mu.Lock()
	f.active++
	f.lastUse = time.Now()
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.lastUse = time.Now()
		f.mu.Unlock()
	}()

	out, err := f.dialTarget()
	if err != nil {
		f.mu.Lock()
		f.lastErr = err.Error()
		f.mu.Unlock()
		if f.initErr == nil {
			slog.Warn("SSH 隧道连接失败", "target", f.target, "err", err)
		}
		f.reject(in, err)
		return
	}
	f.mu.Lock()
	f.lastErr = ""
	f.mu.Unlock()
	if !f.track(out) {
		_ = out.Close()
		return
	}
	defer f.untrack(out)
	defer out.Close()
	pipe(in, out)
}

// dialTarget 经跳板机连到目标。SSH 连接断了（空闲被防火墙掐掉等）时丢弃并重连一次。
func (f *forwarder) dialTarget() (net.Conn, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*f.m.opt.DialTimeout)
		cli, err := f.client(ctx)
		if err != nil {
			cancel()
			return nil, err
		}
		c, err := DialFunc(cli)(ctx, "tcp", f.target)
		cancel()
		if err == nil {
			return c, nil
		}
		var oce *ssh.OpenChannelError
		switch {
		case errors.As(err, &oce):
			// 跳板机明确拒绝（目标不可达 / 被禁止转发）：SSH 连接本身是好的
			return nil, fmt.Errorf("SSH 隧道已连上跳板机，但无法从跳板机连到 %s：%s", f.target, oce.Message)
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("SSH 隧道经跳板机连接 %s 超时", f.target)
		}
		// 其余错误视为 SSH 连接已不可用：丢弃后重连重试
		lastErr = err
		f.drop(cli)
	}
	return nil, fmt.Errorf("SSH 隧道连接断开且重连后仍失败：%w", lastErr)
}

// client 返回可用的 SSH 连接，没有就登录跳板机。
func (f *forwarder) client(ctx context.Context) (*ssh.Client, error) {
	f.mu.Lock()
	c, closed := f.cli, f.closed
	f.mu.Unlock()
	if closed {
		return nil, errClosed
	}
	if c != nil {
		return c, nil
	}
	if f.initErr != nil {
		return nil, f.initErr
	}
	f.dialMu.Lock()
	defer f.dialMu.Unlock()
	f.mu.Lock()
	c = f.cli
	f.mu.Unlock()
	if c != nil {
		return c, nil
	}
	cli, err := f.m.opt.Dial(ctx, f.cfg, f.m.opt.DialTimeout)
	if err != nil {
		return nil, fmt.Errorf("SSH 隧道连接失败：%w", err)
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		_ = cli.Close()
		return nil, errClosed
	}
	f.cli = cli
	f.mu.Unlock()
	go f.watch(cli)
	return cli, nil
}

func (f *forwarder) drop(cli *ssh.Client) {
	f.mu.Lock()
	if f.cli == cli {
		f.cli = nil
	}
	f.mu.Unlock()
	_ = cli.Close()
}

// watch 对一条 SSH 连接做保活；连接关闭（含保活失败）后把它从转发器上摘掉。
func (f *forwarder) watch(cli *ssh.Client) {
	gone := make(chan struct{})
	go func() { _ = cli.Wait(); close(gone) }()
	t := time.NewTicker(f.m.opt.KeepAlive)
	defer t.Stop()
	for {
		select {
		case <-gone:
			f.drop(cli)
			return
		case <-t.C:
			ok := make(chan bool, 1)
			go func() { _, _, err := cli.SendRequest("keepalive@openssh.com", true, nil); ok <- err == nil }()
			select {
			case good := <-ok:
				if !good {
					_ = cli.Close()
				}
			case <-time.After(f.m.opt.KeepAlive):
				_ = cli.Close() // 保活没有应答：连接已死
			case <-gone:
			}
		}
	}
}

func (f *forwarder) close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	cli := f.cli
	f.cli = nil
	conns := make([]net.Conn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mu.Unlock()
	_ = f.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	if cli != nil {
		_ = cli.Close()
	}
}

type closeWriter interface{ CloseWrite() error }

// pipe 双向转发，一端读完（EOF）就对另一端半关闭，出错则两端都关。
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		if _, err := io.Copy(dst, src); err != nil {
			_ = dst.Close()
			_ = src.Close()
			return
		}
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// reject 把隧道失败的原因按目标协议回给客户端，然后关闭连接。
func (f *forwarder) reject(c net.Conn, err error) {
	msg := sanitize(err.Error())
	var pkt []byte
	switch f.proto {
	case ProtoMySQL:
		pkt = mysqlErrPacket(msg)
	case ProtoRedis:
		pkt = []byte("-ERR " + msg + "\r\n")
	}
	if len(pkt) > 0 {
		_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = c.Write(pkt)
		if cw, ok := c.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
		// 先等客户端把已发的内容读完再关，避免 RST 让客户端丢掉错误报文
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		_, _ = io.Copy(io.Discard, io.LimitReader(c, 64<<10))
	}
}

func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 {
			return ' '
		}
		return r
	}, s)
	if len(s) > 400 {
		s = strings.ToValidUTF8(s[:400], "") + "…"
	}
	return s
}

// mysqlErrPacket 构造握手阶段的 ERR 包（错误码 1105，SQLSTATE HY000）：MySQL 客户端连上后第一个读到的就是它，
// 会把消息作为连接错误报告出来。
func mysqlErrPacket(msg string) []byte {
	payload := append([]byte{0xff, 0x51, 0x04, '#'}, "HY000"...)
	payload = append(payload, msg...)
	n := len(payload)
	return append([]byte{byte(n), byte(n >> 8), byte(n >> 16), 0}, payload...)
}
