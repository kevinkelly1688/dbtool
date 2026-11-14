// @author: kevinkelly
// @license: Apache Licence
// @file: sshtunnel_test.go
// @time: 2026/10/28

package sshtunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	drv "github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/ssh"

	"dbtool/internal/crypto"
	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// ---- 假目标：回显服务 ----

func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// ---- 假 SSH 服务器：密码或公钥登录，只支持 direct-tcpip 转发 ----

type fakeSSH struct {
	addr   string
	logins atomic.Int32 // 成功登录次数
	mu     sync.Mutex
	conns  []net.Conn
	ln     net.Listener
}

func (s *fakeSSH) hostPort(t *testing.T) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(s.addr)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

// killConns 模拟跳板机重启 / 中间设备掐断空闲连接：关掉全部已建立的 SSH 连接（监听仍在，可以重新登录）。
func (s *fakeSSH) killConns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
}

func (s *fakeSSH) stop() {
	_ = s.ln.Close()
	s.killConns()
}

func startSSH(t *testing.T, password string, key ssh.PublicKey) *fakeSSH {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if password != "" && c.User() == "tunnel" && string(pw) == password {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if key != nil && c.User() == "tunnel" && string(k.Marshal()) == string(key.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSSH{addr: ln.Addr().String(), ln: ln}
	t.Cleanup(s.stop)
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					nc.Close()
					return
				}
				s.logins.Add(1)
				s.mu.Lock()
				s.conns = append(s.conns, nc)
				s.mu.Unlock()
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					if nch.ChannelType() != "direct-tcpip" {
						_ = nch.Reject(ssh.UnknownChannelType, "no")
						continue
					}
					var p struct {
						Host     string
						Port     uint32
						OrigHost string
						OrigPort uint32
					}
					_ = ssh.Unmarshal(nch.ExtraData(), &p)
					dst, err := net.Dial("tcp", net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port))))
					if err != nil {
						_ = nch.Reject(ssh.ConnectionFailed, err.Error())
						continue
					}
					ch, chReqs, _ := nch.Accept()
					go ssh.DiscardRequests(chReqs)
					go func() { defer ch.Close(); defer dst.Close(); _, _ = io.Copy(ch, dst) }()
					go func() { defer ch.Close(); defer dst.Close(); _, _ = io.Copy(dst, ch) }()
				}
				_ = sc.Close()
			}()
		}
	}()
	return s
}

func newMgr(t *testing.T, o Options) *Manager {
	t.Helper()
	m := NewManager(o)
	t.Cleanup(m.CloseAll)
	return m
}

func cfgFor(t *testing.T, s *fakeSSH, password string) Config {
	h, p := s.hostPort(t)
	return Config{Host: h, Port: p, User: "tunnel", Password: password}
}

// roundTrip 经转发器连到目标并做一次回显。
func roundTrip(t *testing.T, host string, port int, payload string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		t.Fatalf("连接转发器: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != payload {
		t.Fatalf("回显: %q %v", buf, err)
	}
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

// ---- 测试 ----

func TestForwardPassword(t *testing.T) {
	echo := startEcho(t)
	eh, ep := splitAddr(t, echo)
	srv := startSSH(t, "tunnel-pw", nil)
	m := newMgr(t, Options{})
	h, p, err := m.Forward(cfgFor(t, srv, "tunnel-pw"), eh, ep, ProtoNone)
	if err != nil {
		t.Fatal(err)
	}
	if h != "127.0.0.1" || p == 0 {
		t.Fatalf("应返回本机回环地址与端口: %s:%d", h, p)
	}
	if srv.logins.Load() != 0 {
		t.Fatal("Forward 本身不应连接跳板机（懒连接）")
	}
	roundTrip(t, h, p, "hello")
	if srv.logins.Load() != 1 {
		t.Fatalf("应登录一次: %d", srv.logins.Load())
	}
	// 同样的配置和目标复用同一个转发器
	h2, p2, _ := m.Forward(cfgFor(t, srv, "tunnel-pw"), eh, ep, ProtoNone)
	if h2 != h || p2 != p {
		t.Fatalf("同配置同目标应复用: %d vs %d", p, p2)
	}
	if st := m.Stats(); len(st) != 1 || !st[0].Connected || st[0].Target != echo {
		t.Fatalf("%+v", st)
	}
}

func TestForwardPrivateKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	echo := startEcho(t)
	eh, ep := splitAddr(t, echo)
	srv := startSSH(t, "", sshPub)
	sh, sp := srv.hostPort(t)
	m := newMgr(t, Options{})

	plain, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	h, p, _ := m.Forward(Config{Host: sh, Port: sp, User: "tunnel", Pkey: string(pem.EncodeToMemory(plain))}, eh, ep, ProtoNone)
	roundTrip(t, h, p, "key-plain")

	enc, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("key-pass"))
	if err != nil {
		t.Fatal(err)
	}
	h, p, _ = m.Forward(Config{Host: sh, Port: sp, User: "tunnel", Pkey: string(pem.EncodeToMemory(enc)), PkeyPassword: "key-pass"}, eh, ep, ProtoNone)
	roundTrip(t, h, p, "key-passphrase")
}

func TestForwardConcurrentSharesOneSSHConnection(t *testing.T) {
	echo := startEcho(t)
	eh, ep := splitAddr(t, echo)
	srv := startSSH(t, "pw", nil)
	m := newMgr(t, Options{})
	h, p, _ := m.Forward(cfgFor(t, srv, "pw"), eh, ep, ProtoNone)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			roundTrip(t, h, p, fmt.Sprintf("conn-%d", i))
		}(i)
	}
	wg.Wait()
	if n := srv.logins.Load(); n != 1 {
		t.Fatalf("40 个并发连接应复用同一条 SSH 连接，实际登录 %d 次", n)
	}
	// 并发调用 Forward 本身也只建一个转发器
	var wg2 sync.WaitGroup
	ports := make(chan int, 30)
	for i := 0; i < 30; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			_, p2, _ := m.Forward(cfgFor(t, srv, "pw"), eh, ep, ProtoNone)
			ports <- p2
		}()
	}
	wg2.Wait()
	close(ports)
	for p2 := range ports {
		if p2 != p {
			t.Fatalf("并发 Forward 应得到同一个端口: %d vs %d", p2, p)
		}
	}
	if len(m.Stats()) != 1 {
		t.Fatalf("%+v", m.Stats())
	}
}

func TestForwardReconnectsAfterSSHDrop(t *testing.T) {
	echo := startEcho(t)
	eh, ep := splitAddr(t, echo)
	srv := startSSH(t, "pw", nil)
	m := newMgr(t, Options{})
	h, p, _ := m.Forward(cfgFor(t, srv, "pw"), eh, ep, ProtoNone)
	roundTrip(t, h, p, "before")
	if srv.logins.Load() != 1 {
		t.Fatal("首次登录")
	}
	srv.killConns() // 跳板机把连接掐断
	// 立即使用：即使转发器还没发现连接已断，也要自动丢弃旧连接、重连并成功
	roundTrip(t, h, p, "after-drop")
	if srv.logins.Load() != 2 {
		t.Fatalf("断线后应重连登录，登录次数 %d", srv.logins.Load())
	}
	// 再来一次，不重复登录
	roundTrip(t, h, p, "again")
	if srv.logins.Load() != 2 {
		t.Fatalf("重连后应继续复用: %d", srv.logins.Load())
	}
}

func TestForwardKeepAliveDropsDeadConnection(t *testing.T) {
	echo := startEcho(t)
	eh, ep := splitAddr(t, echo)
	srv := startSSH(t, "pw", nil)
	m := newMgr(t, Options{KeepAlive: 50 * time.Millisecond})
	h, p, _ := m.Forward(cfgFor(t, srv, "pw"), eh, ep, ProtoNone)
	roundTrip(t, h, p, "x")
	srv.killConns()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := m.Stats(); len(st) == 1 && !st[0].Connected {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("连接断开后应被保活 / Wait 发现并摘除")
}

func TestForwardIdleCloseAndRecreate(t *testing.T) {
	echo := startEcho(t)
	eh, ep := splitAddr(t, echo)
	srv := startSSH(t, "pw", nil)
	m := newMgr(t, Options{IdleTimeout: 150 * time.Millisecond, CheckInterval: 20 * time.Millisecond})
	h, p, _ := m.Forward(cfgFor(t, srv, "pw"), eh, ep, ProtoNone)
	roundTrip(t, h, p, "x")

	// 有连接打开时不算空闲
	held, err := net.Dial("tcp", net.JoinHostPort(h, strconv.Itoa(p)))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if len(m.Stats()) != 1 {
		t.Fatal("还有打开的连接时不应关闭转发器")
	}
	_, _ = held.Write([]byte("ping"))
	buf := make([]byte, 4)
	_ = held.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(held, buf); err != nil {
		t.Fatalf("长连接应仍可用: %v", err)
	}
	held.Close()

	deadline := time.Now().Add(3 * time.Second)
	for len(m.Stats()) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(m.Stats()) != 0 {
		t.Fatal("无连接超过空闲时限后应关闭转发器")
	}
	released := false
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(h, strconv.Itoa(p)), time.Second)
		if err != nil {
			released = true
			break
		}
		c.Close()
	}
	if !released {
		t.Fatal("空闲关闭后监听应已释放")
	}
	// 再次 Forward 重新建立并可用
	h2, p2, err := m.Forward(cfgFor(t, srv, "pw"), eh, ep, ProtoNone)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, h2, p2, "recreated")
}

func TestCloseAllClosesListenersAndConnections(t *testing.T) {
	echo := startEcho(t)
	eh, ep := splitAddr(t, echo)
	srv := startSSH(t, "pw", nil)
	m := NewManager(Options{})
	h, p, _ := m.Forward(cfgFor(t, srv, "pw"), eh, ep, ProtoNone)
	roundTrip(t, h, p, "x")
	held, err := net.Dial("tcp", net.JoinHostPort(h, strconv.Itoa(p)))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, _ = held.Write([]byte("ok"))
	_ = held.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(held, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	m.CloseAll()
	m.CloseAll() // 可重复调用
	_ = held.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := held.Read(make([]byte, 1)); err == nil {
		t.Fatal("CloseAll 后已建立的连接应被关闭")
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort(h, strconv.Itoa(p)), time.Second); err == nil {
		c.Close()
		t.Fatal("CloseAll 后监听应已释放")
	}
	if len(m.Stats()) != 0 {
		t.Fatal(m.Stats())
	}
	// 关闭后仍可重新使用
	h2, p2, err := m.Forward(cfgFor(t, srv, "pw"), eh, ep, ProtoNone)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, h2, p2, "again")
	m.CloseAll()
}

func TestFingerprintChangeCreatesNewForwarder(t *testing.T) {
	echo := startEcho(t)
	eh, ep := splitAddr(t, echo)
	srv := startSSH(t, "pw", nil)
	m := newMgr(t, Options{})
	base := cfgFor(t, srv, "pw")
	_, p1, _ := m.Forward(base, eh, ep, ProtoNone)
	_, p1b, _ := m.Forward(base, eh, ep, ProtoNone)
	if p1 != p1b {
		t.Fatal("相同配置应复用")
	}
	changed := base
	changed.Password = "pw2" // 改了隧道密码
	_, p2, _ := m.Forward(changed, eh, ep, ProtoNone)
	if p2 == p1 {
		t.Fatal("隧道配置指纹变化后应得到新的转发器")
	}
	_, p3, _ := m.Forward(base, eh, ep+1, ProtoNone) // 目标端口不同
	if p3 == p1 || p3 == p2 {
		t.Fatal("目标不同应有各自的转发器")
	}
	if len(m.Stats()) != 3 {
		t.Fatalf("%+v", m.Stats())
	}
	// 旧转发器（旧凭据）仍按旧凭据登录；新凭据的错误密码登录失败
	roundTrip(t, "127.0.0.1", p1, "old")
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p2)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write([]byte("x"))
	if n, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatalf("错误密码的转发器不应转发数据: %d", n)
	}
}

func TestFingerprintHidesSecretsAndIsInjective(t *testing.T) {
	a := Config{Host: "h", Port: 22, User: "u", Password: "super-secret-pw"}
	fp := a.Fingerprint()
	if strings.Contains(fp, "super-secret") || len(fp) != 64 {
		t.Fatalf("%s", fp)
	}
	// 字段边界不同不能撞车
	b := Config{Host: "h", Port: 22, User: "up", Password: "uper-secret-pw"}
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("指纹应区分字段边界")
	}
}

func TestForwardTargetUnreachableMentionsTunnel(t *testing.T) {
	srv := startSSH(t, "pw", nil)
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	dh, dp := splitAddr(t, dead.Addr().String())
	dead.Close()
	m := newMgr(t, Options{})
	h, p, _ := m.Forward(cfgFor(t, srv, "pw"), dh, dp, ProtoRedis)
	c, err := net.Dial("tcp", net.JoinHostPort(h, strconv.Itoa(p)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write([]byte("PING\r\n"))
	line, _ := io.ReadAll(c)
	if !strings.HasPrefix(string(line), "-ERR SSH 隧道") || !strings.Contains(string(line), "无法从跳板机连到") {
		t.Fatalf("目标不可达时应回 Redis 错误并说明是隧道: %q", line)
	}
	// 这是目标的问题，不是 SSH 连接坏了：不应重连
	if srv.logins.Load() != 1 {
		t.Fatalf("目标不可达不应触发重连: %d", srv.logins.Load())
	}
}

func TestMySQLClientSeesTunnelError(t *testing.T) {
	srv := startSSH(t, "pw", nil)
	m := newMgr(t, Options{})
	bad := cfgFor(t, srv, "wrong-password")
	h, p, _ := m.Forward(bad, "10.9.9.9", 3306, ProtoMySQL)
	cfg := drv.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr = "u", "p", "tcp", net.JoinHostPort(h, strconv.Itoa(p))
	cfg.Timeout = 5 * time.Second
	connector, err := drv.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	err = db.Ping()
	if err == nil {
		t.Fatal("隧道认证失败时连接应失败")
	}
	if !strings.Contains(err.Error(), "SSH 隧道") {
		t.Fatalf("MySQL 驱动的错误里应能看出是隧道失败: %v", err)
	}
	if strings.Contains(err.Error(), "wrong-password") {
		t.Fatalf("错误信息不能带密码: %v", err)
	}
}

func TestNoAuthMethodMentionsTunnel(t *testing.T) {
	m := newMgr(t, Options{})
	h, p, _ := m.Forward(Config{Host: "127.0.0.1", Port: 1, User: "u"}, "10.0.0.1", 6379, ProtoRedis)
	c, err := net.Dial("tcp", net.JoinHostPort(h, strconv.Itoa(p)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write([]byte("PING\r\n"))
	out, _ := io.ReadAll(c)
	if !strings.Contains(string(out), "没有可用的认证方式") {
		t.Fatalf("%q", out)
	}
}

func TestMaxForwarders(t *testing.T) {
	m := newMgr(t, Options{MaxForwarders: 2})
	for i := 0; i < 2; i++ {
		if _, _, err := m.Forward(Config{Host: "j", Port: 22, User: "u", Password: "p"}, "t", 1000+i, ProtoNone); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := m.Forward(Config{Host: "j", Port: 22, User: "u", Password: "p"}, "t", 2000, ProtoNone); err == nil {
		t.Fatal("超过上限应报错")
	}
}

func TestListenHostAdvertiseHost(t *testing.T) {
	m := newMgr(t, Options{ListenHost: "127.0.0.1", AdvertiseHost: "tunnel.local"})
	h, p, err := m.Forward(Config{Host: "j", Port: 22, User: "u", Password: "p"}, "t", 1, ProtoNone)
	if err != nil || h != "tunnel.local" || p == 0 {
		t.Fatalf("%s %d %v", h, p, err)
	}
	o := OptionsFromEnv(func(k string) string {
		return map[string]string{envListenHost: "127.0.0.1", envAdvertiseHost: " 10.1.2.3 ", envIdleTimeout: "30m"}[k]
	})
	if o.AdvertiseHost != "10.1.2.3" || o.IdleTimeout != 30*time.Minute || o.ListenHost != "127.0.0.1" {
		t.Fatalf("%+v", o)
	}
	if o := OptionsFromEnv(func(k string) string { return map[string]string{envIdleTimeout: "10s"}[k] }); o.IdleTimeout != 0 {
		t.Fatalf("小于 1 分钟的空闲时限无效，使用默认值: %+v", o)
	}
}

// ---- Resolver：把 model.Instance 改写成走隧道的连接信息 ----

func mirage(t *testing.T) *crypto.Mirage {
	t.Helper()
	m, err := crypto.NewMirage("0123456789abcdef0123456789abcdef", "ECB", "")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func ptr(s string) *string { return &s }

func TestResolverNoTunnelUnchanged(t *testing.T) {
	m := newMgr(t, Options{})
	r := &Resolver{Mgr: m, Decrypt: mirage(t).Decrypt}
	in := engine.Conn{Host: "10.0.0.5", Port: 3306, User: "u", Password: "p", DBName: "d", Charset: "utf8mb4", SSL: true, VerifySSL: true}
	got := r.Apply(&model.Instance{ID: 1, DBType: "mysql", Host: "10.0.0.5", Port: 3306}, in)
	if got != in {
		t.Fatalf("未配置隧道时连接信息必须原样返回: %+v", got)
	}
	if len(m.Stats()) != 0 {
		t.Fatal("不应创建任何转发器")
	}
	if Enabled(nil) || Enabled(&model.Instance{}) {
		t.Fatal("Enabled")
	}
}

func TestResolverRewritesHostPortAndKeepsOtherFields(t *testing.T) {
	echo := startEcho(t)
	eh, ep := splitAddr(t, echo)
	srv := startSSH(t, "tunnel-pw", nil)
	sh, sp := srv.hostPort(t)
	mg := mirage(t)
	m := newMgr(t, Options{})
	tid := uint(3)
	inst := &model.Instance{ID: 1, DBType: "mysql", Host: eh, Port: ep, TunnelID: &tid,
		Tunnel: &model.Tunnel{ID: 3, Host: sh, Port: sp, User: ptr(mg.Encrypt("tunnel")), Password: ptr(mg.Encrypt("tunnel-pw"))}}
	r := &Resolver{Mgr: m, Decrypt: mg.Decrypt}
	in := engine.Conn{Host: eh, Port: ep, User: "u", Password: "p", DBName: "d", Charset: "gbk", SSL: true, VerifySSL: false}
	got := r.Apply(inst, in)
	if got.Host != "127.0.0.1" || got.Port == ep || got.Port == 0 {
		t.Fatalf("Host/Port 应改写为本地转发器: %+v", got)
	}
	if got.User != "u" || got.Password != "p" || got.DBName != "d" || got.Charset != "gbk" || !got.SSL || got.VerifySSL {
		t.Fatalf("其余字段不变: %+v", got)
	}
	roundTrip(t, got.Host, got.Port, "via-resolver")
}

func TestResolverLazyLoadsTunnelByID(t *testing.T) {
	echo := startEcho(t)
	eh, ep := splitAddr(t, echo)
	srv := startSSH(t, "tunnel-pw", nil)
	sh, sp := srv.hostPort(t)
	mg := mirage(t)
	m := newMgr(t, Options{})
	var loads atomic.Int32
	r := &Resolver{Mgr: m, Decrypt: mg.Decrypt, Load: func(id uint) (*model.Tunnel, error) {
		loads.Add(1)
		if id != 7 {
			return nil, nil
		}
		return &model.Tunnel{ID: 7, Host: sh, Port: sp, User: ptr(mg.Encrypt("tunnel")), Password: ptr(mg.Encrypt("tunnel-pw"))}, nil
	}}
	tid := uint(7)
	// 慢日志等模块列出的实例没有预加载 Tunnel，只有 tunnel_id
	got := r.Apply(&model.Instance{ID: 2, DBType: "redis", Host: eh, Port: ep, TunnelID: &tid}, engine.Conn{Host: eh, Port: ep})
	if got.Host != "127.0.0.1" || got.Port == ep {
		t.Fatalf("%+v", got)
	}
	if loads.Load() != 1 {
		t.Fatal("应按 tunnel_id 读取一次隧道配置")
	}
	roundTrip(t, got.Host, got.Port, "lazy")
}

func TestResolverMissingTunnelFailsLoudlyNotDirect(t *testing.T) {
	m := newMgr(t, Options{})
	r := &Resolver{Mgr: m, Decrypt: mirage(t).Decrypt, Load: func(uint) (*model.Tunnel, error) { return nil, nil }}
	tid := uint(9)
	got := r.Apply(&model.Instance{ID: 3, DBType: "mysql", Host: "10.1.1.1", Port: 3306, TunnelID: &tid}, engine.Conn{Host: "10.1.1.1", Port: 3306})
	if got.Host == "10.1.1.1" {
		t.Fatalf("隧道配置缺失时不能静默直连目标: %+v", got)
	}
	cfg := drv.NewConfig()
	cfg.User, cfg.Net, cfg.Addr, cfg.Timeout = "u", "tcp", net.JoinHostPort(got.Host, strconv.Itoa(got.Port)), 5*time.Second
	connector, _ := drv.NewConnector(cfg)
	db := sql.OpenDB(connector)
	defer db.Close()
	err := db.Ping()
	if err == nil || !strings.Contains(err.Error(), "SSH 隧道") || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("应报出隧道配置不存在: %v", err)
	}

	// 读取失败同理
	r.Load = func(uint) (*model.Tunnel, error) { return nil, errors.New("db down") }
	got = r.Apply(&model.Instance{ID: 3, DBType: "mysql", Host: "10.1.1.1", Port: 3306, TunnelID: &tid}, engine.Conn{Host: "10.1.1.1", Port: 3306})
	if got.Host == "10.1.1.1" {
		t.Fatalf("读取隧道配置失败时也不能直连: %+v", got)
	}
	r.Load = nil
	got = r.Apply(&model.Instance{ID: 3, DBType: "mysql", Host: "10.1.1.1", Port: 3306, TunnelID: &tid}, engine.Conn{Host: "10.1.1.1", Port: 3306})
	if got.Host == "10.1.1.1" {
		t.Fatalf("无法读取时也不能直连: %+v", got)
	}
}

func TestResolverChangedTunnelPasswordUsesNewForwarder(t *testing.T) {
	echo := startEcho(t)
	eh, ep := splitAddr(t, echo)
	srv := startSSH(t, "pw-new", nil)
	sh, sp := srv.hostPort(t)
	mg := mirage(t)
	m := newMgr(t, Options{})
	r := &Resolver{Mgr: m, Decrypt: mg.Decrypt}
	tid := uint(1)
	mk := func(pw string) *model.Instance {
		return &model.Instance{ID: 1, DBType: "mysql", Host: eh, Port: ep, TunnelID: &tid,
			Tunnel: &model.Tunnel{ID: 1, Host: sh, Port: sp, User: ptr(mg.Encrypt("tunnel")), Password: ptr(mg.Encrypt(pw))}}
	}
	old := r.Apply(mk("pw-old"), engine.Conn{Host: eh, Port: ep})
	cur := r.Apply(mk("pw-new"), engine.Conn{Host: eh, Port: ep})
	if old.Port == cur.Port {
		t.Fatal("管理员改了隧道密码后，应使用新的转发器")
	}
	roundTrip(t, cur.Host, cur.Port, "new-creds")
}

func TestProtoFor(t *testing.T) {
	for in, want := range map[string]Proto{"mysql": ProtoMySQL, "MySQL": ProtoMySQL, "doris": ProtoMySQL, "redis": ProtoRedis, "mssql": ProtoNone, "": ProtoNone} {
		if got := ProtoFor(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestDialErrors(t *testing.T) {
	if _, err := Dial(context.Background(), Config{Host: "127.0.0.1", Port: 1, User: "u"}, time.Second); err == nil || !strings.Contains(err.Error(), "没有可用的认证方式") {
		t.Fatalf("%v", err)
	}
	if _, err := Dial(context.Background(), Config{Host: "127.0.0.1", Port: 1, User: "u", Pkey: "not a key"}, time.Second); err == nil || !strings.Contains(err.Error(), "私钥") {
		t.Fatalf("%v", err)
	}
}
