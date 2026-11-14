// @author: kevinkelly
// @license: Apache Licence
// @file: client.go
// @time: 2026/10/28

// Package redis 是 Redis 的最小只读客户端与 internal/engine 的 redis 引擎。
//
// 没有引入第三方依赖：自己实现了 RESP2 的读写（resp.go），在其上只放行只读命令（whitelist.go）。
// 用途：实例管理的连通性测试 / 健康探测、性能巡检（INFO / CONFIG GET / SLOWLOG / SCAN 抽样）、慢查询采集（SLOWLOG GET）。
package redis

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config 是连接参数。密码已解密；错误信息里不会出现密码。
type Config struct {
	Host        string
	Port        int
	Username    string // Redis 6+ ACL 用户；为空表示只用密码（default 用户）
	Password    string
	DB          int // 连接后 SELECT 的库，默认 0
	TLS         bool
	VerifyTLS   bool // TLS 时是否校验服务端证书
	DialTimeout time.Duration
	CmdTimeout  time.Duration // 单条命令的读写超时（也受 ctx 截止时间约束）
}

const (
	defaultDialTimeout = 5 * time.Second
	defaultCmdTimeout  = 10 * time.Second
	defaultPort        = 6379
)

// 全进程的连接数上限：巡检、慢日志采集、健康探测共用，避免对 Redis 同时开出大量连接。
var connSem = struct {
	mu  sync.Mutex
	ch  chan struct{}
	cap int
}{ch: make(chan struct{}, 64), cap: 64}

// SetMaxConns 设置全进程同时打开的 Redis 连接上限（默认 64）。只应在启动时调用。
func SetMaxConns(n int) {
	if n < 1 {
		n = 1
	}
	connSem.mu.Lock()
	defer connSem.mu.Unlock()
	connSem.ch = make(chan struct{}, n)
	connSem.cap = n
}

func acquireConn(ctx context.Context) (func(), error) {
	connSem.mu.Lock()
	ch := connSem.ch
	connSem.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("等待 Redis 连接名额时取消: %w", ctx.Err())
	case <-time.After(10 * time.Second):
		return nil, errors.New("Redis 连接数已达上限，请稍后重试")
	}
}

// Client 是到一个 Redis 实例的单个连接，非并发安全（同一时刻一个命令）。
type Client struct {
	cfg     Config
	conn    net.Conn
	r       *bufio.Reader
	w       *bufio.Writer
	release func()
	mu      sync.Mutex
	broken  bool
}

// Dial 连接、认证并选库。失败时返回的错误不含密码。
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if cfg.CmdTimeout <= 0 {
		cfg.CmdTimeout = defaultCmdTimeout
	}
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, errors.New("Redis 地址为空")
	}
	release, err := acquireConn(ctx)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	d := net.Dialer{Timeout: cfg.DialTimeout}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		release()
		return nil, fmt.Errorf("无法连接 %s: %s", addr, scrub(err.Error(), cfg))
	}
	var conn net.Conn = raw
	if cfg.TLS {
		tc := tls.Client(raw, &tls.Config{ServerName: cfg.Host, InsecureSkipVerify: !cfg.VerifyTLS, MinVersion: tls.VersionTLS12}) //nolint:gosec // 是否校验证书由实例配置决定
		_ = raw.SetDeadline(time.Now().Add(cfg.DialTimeout))
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			release()
			return nil, fmt.Errorf("TLS 握手失败 %s: %s", addr, scrub(err.Error(), cfg))
		}
		conn = tc
	}
	c := &Client{cfg: cfg, conn: conn, r: bufio.NewReaderSize(conn, 32<<10), w: bufio.NewWriterSize(conn, 4<<10), release: release}
	if cfg.Password != "" {
		args := []string{"AUTH", cfg.Password}
		if cfg.Username != "" {
			args = []string{"AUTH", cfg.Username, cfg.Password}
		}
		if _, err := c.exec(ctx, args); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("Redis 认证失败: %s", scrub(err.Error(), cfg))
		}
	}
	if cfg.DB > 0 {
		if _, err := c.exec(ctx, []string{"SELECT", strconv.Itoa(cfg.DB)}); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("选择库 %d 失败: %s", cfg.DB, scrub(err.Error(), cfg))
		}
	}
	return c, nil
}

// scrub 去掉错误文本里可能出现的账号密码。
func scrub(msg string, cfg Config) string {
	for _, s := range []string{cfg.Password, cfg.Username} {
		if len(s) >= 3 {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	return msg
}

// Close 关闭连接并归还名额；可重复调用。
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	if c.release != nil {
		c.release()
		c.release = nil
	}
	return err
}

// exec 不做白名单校验地发送一条命令（仅限包内使用：AUTH / SELECT / SlowlogReset）。
func (c *Client) exec(ctx context.Context, args []string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil || c.broken {
		return nil, errors.New("Redis 连接已关闭")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(c.cfg.CmdTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = c.conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetDeadline(time.Now()) }) // 取消时立刻打断阻塞的读
	defer stop()
	if err := writeCommand(c.w, args); err != nil {
		c.broken = true
		return nil, c.wrap(ctx, err)
	}
	v, err := readReply(c.r)
	if err != nil {
		var se *ServerError
		if !errors.As(err, &se) {
			c.broken = true // 协议错误 / 超时后连接状态不可知
		}
		return nil, c.wrap(ctx, err)
	}
	return v, nil
}

func (c *Client) wrap(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var se *ServerError
	if errors.As(err, &se) {
		return se
	}
	return errors.New(scrub(err.Error(), c.cfg))
}

// Do 发送一条只读命令（先过白名单），返回原始回复。
func (c *Client) Do(ctx context.Context, args ...string) (any, error) {
	if err := Check(args); err != nil {
		return nil, err
	}
	return c.exec(ctx, args)
}

// ---- 类型化的读取 ----

// asString 把回复转成字符串（整数也转）。
func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return ""
}

func asInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	}
	return 0
}

// Ping 测试连通性。
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Do(ctx, "PING")
	return err
}

// Info 读取 INFO 的某一段（空表示默认段），解析成 key → value。注释行（# Server）与空行忽略。
func (c *Client) Info(ctx context.Context, section string) (map[string]string, error) {
	args := []string{"INFO"}
	if section != "" {
		args = append(args, section)
	}
	v, err := c.Do(ctx, args...)
	if err != nil {
		return nil, err
	}
	return ParseInfo(asString(v)), nil
}

// ParseInfo 解析 INFO 文本。
func ParseInfo(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if ok {
			out[k] = v
		}
	}
	return out
}

// ConfigGet 读取配置项（支持通配符）。键名统一为小写。
func (c *Client) ConfigGet(ctx context.Context, patterns ...string) (map[string]string, error) {
	args := append([]string{"CONFIG", "GET"}, patterns...)
	v, err := c.Do(ctx, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	switch x := v.(type) {
	case []any: // RESP2：[k, v, k, v ...]
		for i := 0; i+1 < len(x); i += 2 {
			out[strings.ToLower(asString(x[i]))] = asString(x[i+1])
		}
	}
	return out, nil
}

// DBSize 返回当前库的 key 数。
func (c *Client) DBSize(ctx context.Context) (int64, error) {
	v, err := c.Do(ctx, "DBSIZE")
	return asInt(v), err
}

// SlowEntry 是 SLOWLOG GET 的一条。
type SlowEntry struct {
	ID             int64
	Time           time.Time // 命令开始执行的时间
	DurationMicros int64
	Args           []string // 命令及参数（Redis 会截断过长的参数）
	Client         string   // 客户端地址 ip:port（Redis 4.0+）
	ClientName     string
}

// SlowlogLen 返回当前慢日志条数。
func (c *Client) SlowlogLen(ctx context.Context) (int64, error) {
	v, err := c.Do(ctx, "SLOWLOG", "LEN")
	return asInt(v), err
}

// SlowlogGet 取最近 n 条慢日志（最新在前）。
func (c *Client) SlowlogGet(ctx context.Context, n int) ([]SlowEntry, error) {
	if n <= 0 {
		n = 128
	}
	v, err := c.Do(ctx, "SLOWLOG", "GET", strconv.Itoa(n))
	if err != nil {
		return nil, err
	}
	arr, _ := v.([]any)
	out := make([]SlowEntry, 0, len(arr))
	for _, it := range arr {
		f, ok := it.([]any)
		if !ok || len(f) < 4 {
			continue
		}
		e := SlowEntry{ID: asInt(f[0]), Time: time.Unix(asInt(f[1]), 0), DurationMicros: asInt(f[2])}
		if args, ok := f[3].([]any); ok {
			for _, a := range args {
				e.Args = append(e.Args, asString(a))
			}
		}
		if len(f) > 4 {
			e.Client = asString(f[4])
		}
		if len(f) > 5 {
			e.ClientName = asString(f[5])
		}
		out = append(out, e)
	}
	return out, nil
}

// SlowlogReset 清空慢日志。这是本包唯一的写操作，不在只读白名单里：调用方必须自己做权限控制与审计。
func (c *Client) SlowlogReset(ctx context.Context) error {
	_, err := c.exec(ctx, []string{"SLOWLOG", "RESET"})
	return err
}

// Scan 迭代一批 key：返回下一个游标（"0" 表示结束）与本批 key。count 会被夹在 [1, 1000]。
func (c *Client) Scan(ctx context.Context, cursor string, count int) (string, []string, error) {
	count = min(max(count, 1), maxScanCount)
	v, err := c.Do(ctx, "SCAN", cursor, "COUNT", strconv.Itoa(count))
	if err != nil {
		return "", nil, err
	}
	arr, ok := v.([]any)
	if !ok || len(arr) != 2 {
		return "", nil, errors.New("SCAN 回复格式不对")
	}
	var keys []string
	if ks, ok := arr[1].([]any); ok {
		for _, k := range ks {
			keys = append(keys, asString(k))
		}
	}
	return asString(arr[0]), keys, nil
}

// Type 返回 key 的类型（string / list / set / zset / hash / stream / none）。
func (c *Client) Type(ctx context.Context, key string) (string, error) {
	v, err := c.Do(ctx, "TYPE", key)
	return asString(v), err
}

// MemoryUsage 返回 key 占用的字节数（Redis 4.0+）；key 不存在返回 -1。
func (c *Client) MemoryUsage(ctx context.Context, key string) (int64, error) {
	v, err := c.Do(ctx, "MEMORY", "USAGE", key, "SAMPLES", "5")
	if err != nil {
		return 0, err
	}
	if v == nil {
		return -1, nil
	}
	return asInt(v), nil
}

// ElementCount 返回容器类 key 的元素个数（MEMORY USAGE 不可用时的退路）；string 返回字节长度。
func (c *Client) ElementCount(ctx context.Context, typ, key string) (int64, error) {
	cmd := map[string]string{"string": "STRLEN", "list": "LLEN", "hash": "HLEN", "set": "SCARD", "zset": "ZCARD", "stream": "XLEN"}[typ]
	if cmd == "" {
		return 0, fmt.Errorf("不支持的类型 %q", typ)
	}
	v, err := c.Do(ctx, cmd, key)
	return asInt(v), err
}

// ClientList 返回 CLIENT LIST 解析后的每个连接（字段名 → 值）。
func (c *Client) ClientList(ctx context.Context) ([]map[string]string, error) {
	v, err := c.Do(ctx, "CLIENT", "LIST")
	if err != nil {
		return nil, err
	}
	var out []map[string]string
	for _, line := range strings.Split(asString(v), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := map[string]string{}
		for _, kv := range strings.Fields(line) {
			if k, val, ok := strings.Cut(kv, "="); ok {
				m[k] = val
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// LatencyEvent 是 LATENCY LATEST 的一行。
type LatencyEvent struct {
	Event    string
	Time     time.Time
	LatestMs int64
	MaxMs    int64
}

// LatencyLatest 读取延迟监控的最新事件（latency-monitor-threshold 为 0 时是空）。
func (c *Client) LatencyLatest(ctx context.Context) ([]LatencyEvent, error) {
	v, err := c.Do(ctx, "LATENCY", "LATEST")
	if err != nil {
		return nil, err
	}
	var out []LatencyEvent
	arr, _ := v.([]any)
	for _, it := range arr {
		f, ok := it.([]any)
		if !ok || len(f) < 4 {
			continue
		}
		out = append(out, LatencyEvent{Event: asString(f[0]), Time: time.Unix(asInt(f[1]), 0), LatestMs: asInt(f[2]), MaxMs: asInt(f[3])})
	}
	return out, nil
}

// RoleInfo 是 ROLE 命令的结果。
type RoleInfo struct {
	Role   string // master / slave / sentinel
	Offset int64
	// master：从库列表
	Replicas []ReplicaInfo
	// slave：主库地址与状态
	MasterHost string
	MasterPort int64
	LinkState  string // connect / connecting / sync / connected
}

// ReplicaInfo 是 master 视角下的一个从库。
type ReplicaInfo struct {
	Host   string
	Port   string
	Offset string
}

// Role 读取角色。
func (c *Client) Role(ctx context.Context) (*RoleInfo, error) {
	v, err := c.Do(ctx, "ROLE")
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil, errors.New("ROLE 回复格式不对")
	}
	ri := &RoleInfo{Role: asString(arr[0])}
	switch ri.Role {
	case "master":
		if len(arr) > 1 {
			ri.Offset = asInt(arr[1])
		}
		if len(arr) > 2 {
			if reps, ok := arr[2].([]any); ok {
				for _, r := range reps {
					if f, ok := r.([]any); ok && len(f) >= 3 {
						ri.Replicas = append(ri.Replicas, ReplicaInfo{Host: asString(f[0]), Port: asString(f[1]), Offset: asString(f[2])})
					}
				}
			}
		}
	case "slave":
		if len(arr) >= 5 {
			ri.MasterHost, ri.MasterPort, ri.LinkState, ri.Offset = asString(arr[1]), asInt(arr[2]), asString(arr[3]), asInt(arr[4])
		}
	}
	return ri, nil
}

// CommandAvailable 询问哪些命令在这个实例上可用（COMMAND INFO）：被 rename-command 改名 / 禁用的命令查不到。
// 返回 命令名(小写) → 是否可用。实例不支持 COMMAND（Redis 2.8 以下）时返回错误。
func (c *Client) CommandAvailable(ctx context.Context, names ...string) (map[string]bool, error) {
	args := append([]string{"COMMAND", "INFO"}, names...)
	v, err := c.Do(ctx, args...)
	if err != nil {
		return nil, err
	}
	arr, _ := v.([]any)
	out := map[string]bool{}
	for i, n := range names {
		out[strings.ToLower(n)] = i < len(arr) && arr[i] != nil
	}
	return out, nil
}

// IsUnknownCommand 判断错误是否“命令不存在或被改名 / 禁用”。
func IsUnknownCommand(err error) bool {
	var se *ServerError
	if errors.As(err, &se) {
		return strings.Contains(strings.ToLower(se.Msg), "unknown command") || strings.Contains(strings.ToLower(se.Msg), "unknown subcommand")
	}
	return false
}

// IsNoPerm 判断错误是否“没有权限执行该命令”（ACL）。
func IsNoPerm(err error) bool {
	var se *ServerError
	return errors.As(err, &se) && (se.Kind() == "NOPERM" || strings.Contains(strings.ToLower(se.Msg), "no permissions"))
}
