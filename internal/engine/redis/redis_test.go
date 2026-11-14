// @author: kevinkelly
// @license: Apache Licence
// @file: redis_test.go
// @time: 2026/10/28

package redis_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/engine/redis"
	"dbtool/internal/engine/redis/redistest"
)

func handler(password string) redistest.Handler {
	authed := password == ""
	return func(a []string) any {
		cmd := strings.ToUpper(a[0])
		if cmd == "AUTH" {
			if len(a) == 2 && a[1] == password {
				authed = true
				return redistest.Simple("OK")
			}
			if len(a) == 3 && a[1] == "ops" && a[2] == password {
				authed = true
				return redistest.Simple("OK")
			}
			return redistest.Err("WRONGPASS invalid username-password pair or user is disabled.")
		}
		if !authed {
			return redistest.Err("NOAUTH Authentication required.")
		}
		switch cmd {
		case "PING":
			return redistest.Simple("PONG")
		case "SELECT":
			return redistest.Simple("OK")
		case "INFO":
			return "# Server\r\nredis_version:7.0.11\r\nuptime_in_seconds:100\r\n# Keyspace\r\ndb0:keys=10,expires=2,avg_ttl=0\r\ndb3:keys=1,expires=0,avg_ttl=0\r\n"
		case "CONFIG":
			if strings.EqualFold(a[1], "GET") && a[2] == "databases" {
				return []any{"databases", "4"}
			}
			return []any{}
		case "SLOWLOG":
			switch strings.ToUpper(a[1]) {
			case "LEN":
				return int64(2)
			case "GET":
				return []any{
					[]any{int64(7), int64(1700000000), int64(25000), []any{"HGETALL", "user:1"}, "10.0.0.5:51234", "app"},
					[]any{int64(6), int64(1699999000), int64(12000), []any{"KEYS", "*"}},
				}
			case "RESET":
				return redistest.Simple("OK")
			}
		case "SCAN":
			return []any{"0", []any{"a", "b"}}
		case "TYPE":
			return redistest.Simple("string")
		case "MEMORY":
			return int64(1234)
		case "ROLE":
			return []any{"master", int64(100), []any{[]any{"10.0.0.2", "6379", "100"}}}
		case "COMMAND":
			return []any{nil, []any{"config"}}
		case "CLIENT":
			return "id=3 addr=10.0.0.5:51234 name=app cmd=get\nid=4 addr=10.0.0.6:1 name= cmd=client\n"
		}
		return redistest.Err("ERR unknown command '" + a[0] + "'")
	}
}

func dial(t *testing.T, srv *redistest.Server, user, pass string) (*redis.Client, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return redis.Dial(ctx, redis.Config{Host: srv.Host(), Port: srv.Port(), Username: user, Password: pass, DB: 2})
}

func TestClientHelpers(t *testing.T) {
	srv, err := redistest.New(handler(""))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, err := dial(t, srv, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := c.Info(ctx, "")
	if err != nil || info["redis_version"] != "7.0.11" || info["db0"] != "keys=10,expires=2,avg_ttl=0" {
		t.Fatalf("info=%v err=%v", info, err)
	}
	cfg, err := c.ConfigGet(ctx, "databases")
	if err != nil || cfg["databases"] != "4" {
		t.Fatalf("cfg=%v err=%v", cfg, err)
	}
	logs, err := c.SlowlogGet(ctx, 10)
	if err != nil || len(logs) != 2 {
		t.Fatalf("slowlog=%v err=%v", logs, err)
	}
	if logs[0].ID != 7 || logs[0].DurationMicros != 25000 || strings.Join(logs[0].Args, " ") != "HGETALL user:1" || logs[0].Client != "10.0.0.5:51234" || logs[0].ClientName != "app" {
		t.Errorf("第一条解析不对: %+v", logs[0])
	}
	if logs[1].Client != "" { // Redis 4.0 以前没有客户端地址
		t.Errorf("缺字段应为空: %+v", logs[1])
	}
	if n, err := c.SlowlogLen(ctx); err != nil || n != 2 {
		t.Errorf("len=%d err=%v", n, err)
	}
	next, keys, err := c.Scan(ctx, "0", 100)
	if err != nil || next != "0" || len(keys) != 2 {
		t.Errorf("scan=%s %v %v", next, keys, err)
	}
	if typ, err := c.Type(ctx, "a"); err != nil || typ != "string" {
		t.Errorf("type=%s %v", typ, err)
	}
	if n, err := c.MemoryUsage(ctx, "a"); err != nil || n != 1234 {
		t.Errorf("memory=%d %v", n, err)
	}
	role, err := c.Role(ctx)
	if err != nil || role.Role != "master" || len(role.Replicas) != 1 || role.Replicas[0].Host != "10.0.0.2" {
		t.Errorf("role=%+v %v", role, err)
	}
	avail, err := c.CommandAvailable(ctx, "flushall", "config")
	if err != nil || avail["flushall"] || !avail["config"] {
		t.Errorf("avail=%v %v", avail, err)
	}
	cl, err := c.ClientList(ctx)
	if err != nil || len(cl) != 2 || cl[0]["name"] != "app" {
		t.Errorf("clients=%v %v", cl, err)
	}
	// 连接时按配置 SELECT 了库 2
	var sawSelect bool
	for _, cmd := range srv.Commands() {
		if cmd[0] == "SELECT" && cmd[1] == "2" {
			sawSelect = true
		}
	}
	if !sawSelect {
		t.Error("应 SELECT 2")
	}
}

func TestAuth(t *testing.T) {
	srv, _ := redistest.New(handler("s3cret!"))
	defer srv.Close()
	if _, err := dial(t, srv, "", "wrong-password"); err == nil || strings.Contains(err.Error(), "wrong-password") {
		t.Fatalf("错误密码应失败且错误里不含密码: %v", err)
	}
	if c0, err := redis.Dial(context.Background(), redis.Config{Host: srv.Host(), Port: srv.Port()}); err != nil {
		// 不带密码可以连上（连接本身不要求认证），但第一条命令会被 NOAUTH 拒绝
		t.Fatalf("无密码拨号不应失败: %v", err)
	} else {
		c0.Close()
	}
	c, err := dial(t, srv, "", "s3cret!")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	c, err = dial(t, srv, "ops", "s3cret!")
	if err != nil {
		t.Fatalf("ACL 用户认证: %v", err)
	}
	c.Close()
	// 无密码拨号后第一条命令被拒绝
	srv2, _ := redistest.New(handler("s3cret!")) // 假服务端的认证状态是全局的，换一个新的
	defer srv2.Close()
	c, _ = redis.Dial(context.Background(), redis.Config{Host: srv2.Host(), Port: srv2.Port()})
	if err := c.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "NOAUTH") {
		t.Errorf("应 NOAUTH: %v", err)
	}
	c.Close()
	// 认证命令必须是 AUTH 用户名 密码 的形式
	var acl bool
	for _, cmd := range srv.Commands() {
		if cmd[0] == "AUTH" && len(cmd) == 3 && cmd[1] == "ops" {
			acl = true
		}
	}
	if !acl {
		t.Error("应发出 AUTH ops 密码")
	}
}

func TestWhitelist(t *testing.T) {
	ok := [][]string{
		{"INFO"}, {"info", "memory"}, {"CONFIG", "GET", "maxmemory"}, {"config", "get", "*"},
		{"SLOWLOG", "GET", "128"}, {"SLOWLOG", "LEN"}, {"DBSIZE"}, {"SCAN", "0", "MATCH", "a*", "COUNT", "100"}, {"SCAN", "0"},
		{"TYPE", "k"}, {"MEMORY", "USAGE", "k"}, {"MEMORY", "USAGE", "k", "SAMPLES", "5"}, {"CLIENT", "LIST"}, {"LATENCY", "LATEST"},
		{"ROLE"}, {"COMMAND", "INFO", "flushall"}, {"SELECT", "3"}, {"PING"}, {"STRLEN", "k"},
	}
	for _, a := range ok {
		if err := redis.Check(a); err != nil {
			t.Errorf("%v 应放行: %v", a, err)
		}
	}
	bad := [][]string{
		{"FLUSHALL"}, {"flushdb"}, {"FLUSHALL", "ASYNC"}, {"CONFIG", "SET", "maxmemory", "1"}, {"CONFIG", "REWRITE"}, {"CONFIG", "RESETSTAT"},
		{"DEBUG", "SLEEP", "10"}, {"KEYS", "*"}, {"keys", "a*"}, {"SHUTDOWN"}, {"SLOWLOG", "RESET"}, {"SET", "a", "b"}, {"GET", "a"},
		{"DEL", "a"}, {"EVAL", "return 1", "0"}, {"SAVE"}, {"BGSAVE"}, {"REPLICAOF", "NO", "ONE"}, {"CLIENT", "KILL", "ID", "1"},
		{"CLIENT", "PAUSE", "1000"}, {"ACL", "LIST"}, {"MIGRATE", "h", "1", "k", "0", "1"}, {"MODULE", "LOAD", "x"}, {"MONITOR"},
		{"INFO", "bogus"}, {"SELECT"}, {"SELECT", "-1"}, {"SCAN", "x"}, {"SCAN", "0", "COUNT", "100000"}, {"SCAN", "0", "FOO", "1"},
		{"SLOWLOG", "GET", "999999"}, {"SLOWLOG"}, {"MEMORY", "PURGE"}, {"OBJECT", "FREQ"}, {}, {"AUTH", "x"}, {"HGETALL", "k"},
	}
	for _, a := range bad {
		if err := redis.Check(a); err == nil {
			t.Errorf("%v 应被拒绝", a)
		}
	}
	var na *redis.ErrNotAllowed
	if err := redis.Check([]string{"keys", "*"}); !errors.As(err, &na) || na.Cmd != "KEYS" {
		t.Errorf("KEYS 应返回 ErrNotAllowed: %v", err)
	}
}

// 危险命令在发出前就被拦截：假服务端一条都收不到。
func TestDangerousNeverSent(t *testing.T) {
	srv, _ := redistest.New(handler(""))
	defer srv.Close()
	c, err := dial(t, srv, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	before := len(srv.Commands())
	for _, a := range [][]string{{"FLUSHALL"}, {"KEYS", "*"}, {"CONFIG", "SET", "maxmemory", "0"}, {"DEBUG", "SEGFAULT"}} {
		if _, err := c.Do(context.Background(), a...); err == nil {
			t.Errorf("%v 应被拒绝", a)
		}
	}
	if got := len(srv.Commands()); got != before {
		t.Errorf("被拒绝的命令不应到达服务端: %v", srv.Commands()[before:])
	}
}

func TestSlowlogReset(t *testing.T) {
	srv, _ := redistest.New(handler(""))
	defer srv.Close()
	c, _ := dial(t, srv, "", "")
	defer c.Close()
	if err := c.SlowlogReset(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := srv.Commands()[len(srv.Commands())-1]
	if strings.Join(last, " ") != "SLOWLOG RESET" {
		t.Errorf("last=%v", last)
	}
}

func TestServerErrorsAndTimeout(t *testing.T) {
	srv, _ := redistest.New(handler(""))
	defer srv.Close()
	c, _ := dial(t, srv, "", "")
	defer c.Close()
	_, err := c.Do(context.Background(), "LATENCY", "LATEST") // 假服务端不认识
	if !redis.IsUnknownCommand(err) {
		t.Errorf("应识别为未知命令: %v", err)
	}
	// 服务端错误不会弄坏连接
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("服务端错误后连接应可继续使用: %v", err)
	}
	// 已取消的 ctx
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Ping(ctx); err == nil {
		t.Error("已取消的 ctx 应失败")
	}
}

func TestReadTimeout(t *testing.T) {
	srv, _ := redistest.New(func(a []string) any {
		time.Sleep(600 * time.Millisecond)
		return redistest.Simple("PONG")
	})
	defer srv.Close()
	ctx := context.Background()
	c, err := redis.Dial(ctx, redis.Config{Host: srv.Host(), Port: srv.Port(), CmdTimeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	if err := c.Ping(ctx); err == nil {
		t.Fatal("应超时")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("超时过慢: %v", time.Since(start))
	}
	if err := c.Ping(ctx); err == nil { // 超时后连接作废，不再复用
		t.Error("超时后的连接应不可再用")
	}
}

func TestDialFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := redis.Dial(ctx, redis.Config{Host: "127.0.0.1", Port: 1, Password: "topsecret"})
	if err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Errorf("连接失败应报错且不含密码: %v", err)
	}
}

func TestEngine(t *testing.T) {
	srv, _ := redistest.New(handler("pw1234"))
	defer srv.Close()
	e, err := engine.New("redis", engine.Conn{Host: srv.Host(), Port: srv.Port(), Password: "pw1234"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ctx := context.Background()
	if e.Name() != "redis" {
		t.Error(e.Name())
	}
	if err := e.TestConnection(ctx); err != nil {
		t.Fatal(err)
	}
	if v, err := e.ServerVersion(ctx); err != nil || v != "7.0.11" {
		t.Errorf("version=%q %v", v, err)
	}
	dbs, err := e.AllDatabases(ctx)
	if err != nil || len(dbs) != 4 || dbs[0] != "db0" {
		t.Errorf("dbs=%v %v", dbs, err)
	}
	rs, err := e.Query(ctx, `SLOWLOG LEN`, engine.QueryOptions{})
	if err != nil || len(rs.Rows) != 1 {
		t.Fatalf("rs=%+v err=%v", rs, err)
	}
	if _, err := e.Query(ctx, `FLUSHALL`, engine.QueryOptions{}); err == nil {
		t.Error("Query 应拒绝 FLUSHALL")
	}
	// 密码错误
	bad, _ := engine.New("redis", engine.Conn{Host: srv.Host(), Port: srv.Port(), Password: "nope-nope"})
	if err := bad.TestConnection(ctx); err == nil || strings.Contains(err.Error(), "nope-nope") {
		t.Errorf("密码错误应失败且不泄露: %v", err)
	}
}

func TestSplitCommand(t *testing.T) {
	got, err := redis.SplitCommand(`CONFIG GET "max memory"`)
	if err != nil || len(got) != 3 || got[2] != "max memory" {
		t.Errorf("got=%v err=%v", got, err)
	}
	if _, err := redis.SplitCommand(`GET "abc`); err == nil {
		t.Error("引号未闭合应报错")
	}
	if _, err := redis.SplitCommand("   "); err == nil {
		t.Error("空命令应报错")
	}
}

func TestConnLimit(t *testing.T) {
	srv, _ := redistest.New(handler(""))
	defer srv.Close()
	redis.SetMaxConns(1)
	defer redis.SetMaxConns(64)
	c1, err := dial(t, srv, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := redis.Dial(ctx, redis.Config{Host: srv.Host(), Port: srv.Port()}); err == nil {
		t.Error("超过连接上限应等待后失败")
	}
	c1.Close()
	c2, err := dial(t, srv, "", "")
	if err != nil {
		t.Fatalf("释放后应可再连: %v", err)
	}
	c2.Close()
}

func TestFingerprint(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"HGETALL", "user:1"}, "HGETALL ?"},
		{[]string{"hgetall", "user:99999"}, "HGETALL ?"},
		{[]string{"SET", "k", "v", "EX", "10"}, "SET ? ? EX ?"},
		{[]string{"MGET", "a", "b", "c", "d"}, "MGET ? ? ..."},
		{[]string{"MGET", "a", "b"}, "MGET ? ?"},
		{[]string{"keys", "user:*"}, "KEYS ?"},
		{[]string{"CONFIG", "GET", "maxmemory"}, "CONFIG GET ?"},
		{[]string{"ZRANGEBYSCORE", "z", "0", "100", "LIMIT", "0", "10", "WITHSCORES"}, "ZRANGEBYSCORE ? ? ... LIMIT ? ? WITHSCORES"},
		{[]string{"SCAN", "0", "MATCH", "a*", "COUNT", "100"}, "SCAN ? MATCH ? COUNT ?"},
		{[]string{"DEL", "a", "b", "c", "... (97 more arguments)"}, "DEL ? ? ..."},
		{[]string{"PING"}, "PING"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := redis.Fingerprint(c.args); got != c.want {
			t.Errorf("Fingerprint(%v)=%q，期望 %q", c.args, got, c.want)
		}
	}
	// 同一类命令（key 不同）指纹相同；指纹里不含 key 名
	if redis.Fingerprint([]string{"GET", "secret-token"}) != redis.Fingerprint([]string{"GET", "other"}) {
		t.Error("key 不同的同类命令应有相同指纹")
	}
	if strings.Contains(redis.Fingerprint([]string{"SET", "password:alice", "hunter2"}), "hunter2") {
		t.Error("指纹不应包含值")
	}
	if got := redis.CommandName([]string{"config", "get", "x"}); got != "CONFIG GET" {
		t.Errorf("CommandName=%q", got)
	}
	if got := redis.CommandName([]string{"hgetall", "k"}); got != "HGETALL" {
		t.Errorf("CommandName=%q", got)
	}
}
