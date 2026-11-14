// @author: kevinkelly
// @license: Apache Licence
// @file: exec_test.go
// @time: 2026/10/28

package redis_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"dbtool/internal/engine"
	"dbtool/internal/engine/redis"
	"dbtool/internal/engine/redis/redistest"
)

func TestTokenizeLine(t *testing.T) {
	ok := map[string][]string{
		``:                      nil,
		`   `:                   nil,
		`SET a b`:               {"SET", "a", "b"},
		"  set   a\tb  ":        {"set", "a", "b"},
		`SET "a b" 'c d'`:       {"SET", "a b", "c d"},
		`SET k "x\ty\n\"q\"\\"`: {"SET", "k", "x\ty\n\"q\"\\"},
		`SET k "\xe4\xbd\xa0"`:  {"SET", "k", "你"},
		`SET k "\x41\x4a"`:      {"SET", "k", "AJ"},
		`SET k 'it\'s'`:         {"SET", "k", "it's"},
		`SET k 'a\nb'`:          {"SET", "k", `a\nb`},
		`SET k ""`:              {"SET", "k", ""},
		`SET k "\x4"`:           {"SET", "k", "x4"},
		`DEL "a"`:               {"DEL", "a"},
		`SET k "\q"`:            {"SET", "k", "q"},
		"SET k \"\\xff\"":       {"SET", "k", "\xff"},
	}
	for in, want := range ok {
		got, err := redis.TokenizeLine(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if len(got) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q = %q，期望 %q", in, got, want)
		}
	}
	for _, in := range []string{`SET "abc`, `SET 'abc`, `SET "a"b`, `SET 'a'b`, `SET k "a\"`} {
		if _, err := redis.TokenizeLine(in); err == nil {
			t.Errorf("%q 应报错", in)
		}
	}
}

func TestParseCommandKeys(t *testing.T) {
	cases := []struct {
		line string
		keys string
	}{
		{`SET a 1`, "a"}, {`set a 1 EX 10`, "a"}, {`SETEX a 10 v`, "a"}, {`GETDEL a`, "a"}, {`INCRBY n 5`, "n"},
		{`DEL a b c`, "a,b,c"}, {`unlink a`, "a"}, {`MSET a 1 b 2 c 3`, "a,b,c"}, {`MSETNX a 1`, "a"},
		{`RENAME a b`, "a,b"}, {`COPY a b REPLACE`, "a,b"}, {`SMOVE s d m`, "s,d"}, {`LMOVE s d LEFT RIGHT`, "s,d"},
		{`PFMERGE d s1 s2`, "d,s1,s2"}, {`SUNIONSTORE d a b`, "d,a,b"}, {`BITOP AND dest k1 k2`, "dest,k1,k2"},
		{`HSET h f v f2 v2`, "h"}, {`HDEL h f1 f2`, "h"}, {`EXPIRE k 60`, "k"}, {`EXPIRE k 60 NX`, "k"}, {`PERSIST k`, "k"},
		{`LPUSH l a b`, "l"}, {`SADD s a b`, "s"}, {`ZADD z 1 a 2 b`, "z"}, {`XADD st * f v`, "st"},
	}
	for _, c := range cases {
		args, err := redis.TokenizeLine(c.line)
		if err != nil {
			t.Fatalf("%s: %v", c.line, err)
		}
		p, err := redis.ParseCommand(args)
		if err != nil {
			t.Errorf("%s: %v", c.line, err)
			continue
		}
		if got := strings.Join(p.Keys, ","); got != c.keys {
			t.Errorf("%s 的键 = %s，期望 %s", c.line, got, c.keys)
		}
	}
}

func TestParseCommandRejects(t *testing.T) {
	forbidden := []string{`FLUSHALL`, `flushdb`, `FLUSHDB ASYNC`, `SHUTDOWN`, `CONFIG SET maxmemory 1`, `DEBUG SLEEP 1`, `KEYS *`, `SCRIPT FLUSH`,
		`EVAL "return 1" 0`, `EVALSHA abc 0`, `EVAL_RO x 0`, `FUNCTION FLUSH`, `MODULE LOAD x`, `SLAVEOF NO ONE`, `REPLICAOF NO ONE`, `ACL SETUSER x`,
		`CLIENT KILL x`, `MONITOR`, `SAVE`, `BGSAVE`, `BGREWRITEAOF`, `MIGRATE h 1 k 0 1`, `SWAPDB 0 1`, `SELECT 1`, `AUTH x`, `WAIT 1 0`,
		`FAILOVER`, `PSYNC a b`, `SYNC`, `SUBSCRIBE c`, `PSUBSCRIBE c*`, `SORT l STORE d`, `sort l ALPHA store d`, `MULTI`, `MOVE k 1`, `BLPOP l 0`}
	for _, line := range forbidden {
		args, _ := redis.TokenizeLine(line)
		_, err := redis.ParseCommand(args)
		var hf *redis.ErrHardForbidden
		if !errors.As(err, &hf) {
			t.Errorf("%s 应是硬禁止，得到 %v", line, err)
		}
		if _, err := redis.Approve(args); err == nil {
			t.Errorf("%s 不应能创建 Approved", line)
		}
	}
	// 无键命令 / 读命令 / 未知命令：不在支持清单
	for _, line := range []string{`PUBLISH c m`, `PING`, `INFO`, `DBSIZE`, `GET a`, `HGETALL h`, `TTL a`, `FOOBAR x`, `SORT l ALPHA`, `ECHO x`} {
		args, _ := redis.TokenizeLine(line)
		_, err := redis.ParseCommand(args)
		var un *redis.ErrUnsupported
		if !errors.As(err, &un) {
			t.Errorf("%s 应是不支持，得到 %v", line, err)
		}
	}
	// 语法错误
	for _, line := range []string{`SET a`, `DEL`, `MSET a`, `MSET a 1 b`, `HSET h f`, `RENAME a`, `SETEX a x v`, `SET a 1 EX`, `SET a 1 EX 0`, `SET a 1 EX 1 PX 1`,
		`SET a 1 FOO`, `COPY a b DB 3`, `BITOP XNOR d k`, `EXPIRE k notnum`, `INCR a b`} {
		args, _ := redis.TokenizeLine(line)
		_, err := redis.ParseCommand(args)
		var hf *redis.ErrHardForbidden
		var un *redis.ErrUnsupported
		if err == nil || errors.As(err, &hf) || errors.As(err, &un) {
			t.Errorf("%s 应是语法错误，得到 %v", line, err)
		}
	}
	if _, err := redis.ParseCommand(nil); err == nil {
		t.Error("空命令应报错")
	}
	big := []string{"SET", "k", strings.Repeat("x", redis.MaxCommandBytes)}
	if _, err := redis.ParseCommand(big); err == nil {
		t.Error("超大命令应报错")
	}
}

func TestHardForbiddenListAndSupported(t *testing.T) {
	l := redis.HardForbiddenList()
	for _, want := range []string{"FLUSHALL", "FLUSHDB", "CONFIG", "KEYS", "EVAL", "SORT … STORE", "AUTH", "SELECT"} {
		found := false
		for _, x := range l {
			if x == want {
				found = true
			}
		}
		if !found {
			t.Errorf("硬禁止清单缺少 %s", want)
		}
	}
	for _, name := range redis.SupportedCmds() {
		if redis.IsHardForbiddenName(name) {
			t.Errorf("受支持的命令 %s 不应同时是硬禁止", name)
		}
	}
	if !redis.IsHardForbiddenName(" flushall ") || redis.IsHardForbiddenName("SET") {
		t.Error("IsHardForbiddenName")
	}
}

func openExec(t *testing.T, kv *redistest.KV, db int) (*redis.ExecClient, *redistest.Server) {
	t.Helper()
	srv, err := kv.Serve()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	e := redis.New(engine.Conn{Host: srv.Host(), Port: srv.Port()})
	x, err := e.OpenExec(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = x.Close() })
	return x, srv
}

func approve(t *testing.T, line string) redis.Approved {
	t.Helper()
	args, err := redis.TokenizeLine(line)
	if err != nil {
		t.Fatal(err)
	}
	a, err := redis.Approve(args)
	if err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	return a
}

func TestExecClient(t *testing.T) {
	kv := redistest.NewKV()
	kv.SetString(3, "cache:a", "old")
	x, srv := openExec(t, kv, 3)
	ctx := context.Background()

	v, err := x.Exec(ctx, approve(t, `SET cache:b hello EX 60`))
	if err != nil || redis.Summarize(v) != "OK" {
		t.Fatalf("SET: %v %v", v, err)
	}
	if got, ok := kv.Get(3, "cache:b"); !ok || got != "hello" {
		t.Errorf("没有写进第 3 库: %q %v", got, ok)
	}
	if kv.Exists(0, "cache:b") {
		t.Error("不应写进第 0 库")
	}
	v, err = x.Exec(ctx, approve(t, `DEL cache:a cache:b cache:zz`))
	if err != nil || redis.Summarize(v) != "(integer) 2" || redis.ReplyInt(v) != 2 {
		t.Errorf("DEL: %v %v", v, err)
	}
	// 服务端错误回复
	kv.Fail["INCR"] = "ERR value is not an integer or out of range"
	_, err = x.Exec(ctx, approve(t, `INCR cache:n`))
	var se *redis.ServerError
	if !errors.As(err, &se) {
		t.Errorf("INCR 应返回服务端错误: %v", err)
	}
	// 连接时选的库
	found := false
	for _, c := range srv.Commands() {
		if len(c) == 2 && c[0] == "SELECT" && c[1] == "3" {
			found = true
		}
	}
	if !found {
		t.Error("应在连接时 SELECT 3")
	}
	// 零值 Approved 不能执行
	if _, err := x.Exec(ctx, redis.Approved{}); err == nil {
		t.Error("零值 Approved 不应能执行")
	}
	for _, c := range srv.Commands() {
		if redis.IsHardForbidden(c) && c[0] != "SELECT" && c[0] != "AUTH" {
			t.Errorf("服务端收到了硬禁止命令 %v", c)
		}
	}
}

func TestExecAuthAndDB(t *testing.T) {
	kv := redistest.NewKV()
	srv, _ := kv.Serve()
	defer srv.Close()
	e := redis.New(engine.Conn{Host: srv.Host(), Port: srv.Port(), Password: "s3cret-pw"})
	x, err := e.OpenExec(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	x.Close()
	if _, err := e.OpenExec(context.Background(), 5000); err == nil {
		t.Error("库编号超范围应报错")
	}
	kv.AuthErr = "WRONGPASS invalid username-password pair s3cret-pw"
	_, err = e.OpenExec(context.Background(), 0)
	if err == nil || strings.Contains(err.Error(), "s3cret-pw") {
		t.Errorf("认证失败的错误不能含密码: %v", err)
	}
}

func TestExecReplyCap(t *testing.T) {
	big := strings.Repeat("a", redis.MaxReplyBytes+1000)
	srv, err := redistest.New(func(a []string) any {
		switch strings.ToUpper(a[0]) {
		case "GETDEL":
			return big
		}
		return redistest.Simple("OK")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	e := redis.New(engine.Conn{Host: srv.Host(), Port: srv.Port()})
	x, err := e.OpenExec(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	if _, err := x.Exec(context.Background(), approve(t, `GETDEL k`)); err == nil || !strings.Contains(err.Error(), "大小上限") {
		t.Errorf("过大的回复应被拒绝: %v", err)
	}
}

func TestSnapshot(t *testing.T) {
	kv := redistest.NewKV()
	kv.SetString(1, "s:1", "hello")
	kv.SetString(1, "s:big", strings.Repeat("x", redis.SnapshotValueMax+1))
	kv.SetHash(1, "h:1", map[string]string{"a": "1", "b": "2", "c": "3"})
	x, _ := openExec(t, kv, 1)
	ctx := context.Background()
	out := x.Snapshot(ctx, approve(t, `DEL s:1 s:big s:none h:1`))
	for _, want := range []string{`类型 string`, `值 "hello"`, `未记录`, `执行前不存在`, `类型 hash`, `3 个字段`} {
		if !strings.Contains(out, want) {
			t.Errorf("快照缺少 %q：\n%s", want, out)
		}
	}
	out = x.Snapshot(ctx, approve(t, `HSET h:1 a 9 z 8`))
	if !strings.Contains(out, `"a"="1"`) || !strings.Contains(out, `"z"=(nil)`) {
		t.Errorf("哈希点名字段的原值: %s", out)
	}
	// 最多记录 10 个键
	args := []string{"DEL"}
	for i := 0; i < 25; i++ {
		args = append(args, "k"+string(rune('a'+i)))
	}
	a, _ := redis.Approve(args)
	if out := x.Snapshot(ctx, a); !strings.Contains(out, "其余 15 个键未记录") {
		t.Errorf("键数上限: %s", out)
	}
}

func TestSummarizeAndEscape(t *testing.T) {
	if redis.Summarize(nil) != "(nil)" || redis.Summarize(int64(7)) != "(integer) 7" || redis.Summarize("OK") != "OK" {
		t.Error("基础回复")
	}
	if got := redis.Summarize([]any{"a", int64(1), nil, "d"}); got != `(array, 4 项) "a", (integer) 1, (nil), …` {
		t.Errorf("数组: %s", got)
	}
	if got := redis.Summarize("a\x00b\xffc\n"); got != `"a\x00b\xffc\n"` {
		t.Errorf("二进制: %s", got)
	}
	long := redis.Summarize(strings.Repeat("好", 100))
	if !strings.HasSuffix(long, "…") || len(long) > 260 {
		t.Errorf("长字符串应截断: %d", len(long))
	}
}

func TestRedact(t *testing.T) {
	if got := redis.Redact([]string{"set", "cache:a", "v"}); got != "SET cache:a v" {
		t.Errorf("普通命令: %s", got)
	}
	long := strings.Repeat("v", 100)
	if got := redis.Redact([]string{"SET", "k", long}); !strings.Contains(got, "…(100 字节)") || strings.Contains(got, long) {
		t.Errorf("长值应截断: %s", got)
	}
	got := redis.Redact([]string{"SET", "user:password:1", "hunter2"})
	if strings.Contains(got, "hunter2") || !strings.Contains(got, "***") {
		t.Errorf("疑似密码的键，值不能记录: %s", got)
	}
	got = redis.Redact([]string{"MSET", "token:1", "abc", "k2", "def"})
	if strings.Contains(got, "abc") || strings.Contains(got, "def") {
		t.Errorf("MSET 含令牌键: %s", got)
	}
	if got := redis.Redact([]string{"DEL", "a", "b"}); got != "DEL a b" {
		t.Errorf("DEL: %s", got)
	}
}
