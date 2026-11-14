// @author: kevinkelly
// @license: Apache Licence
// @file: userquery_test.go
// @time: 2026/10/28

package redis_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/engine/redis"
	"dbtool/internal/engine/redis/redistest"
)

func parse(t *testing.T, line string) (*redis.UserCommand, error) {
	t.Helper()
	args, err := redis.TokenizeLine(line)
	if err != nil {
		t.Fatalf("分词 %q: %v", line, err)
	}
	return redis.ParseUserCommand(args)
}

func TestUserWhitelistAllowed(t *testing.T) {
	ok := []string{
		// 字符串
		"GET k", "get k", "MGET a b c", "STRLEN k", "GETRANGE k 0 10", "EXISTS a b", "TYPE k", "TTL k", "PTTL k", "EXPIRETIME k", "PEXPIRETIME k",
		// 哈希
		"HGET h f", "HMGET h a b", "HGETALL h", "HLEN h", "HKEYS h", "HVALS h", "HEXISTS h f", "HSTRLEN h f", "HSCAN h 0", "HSCAN h 0 MATCH a* COUNT 10 NOVALUES", "HRANDFIELD h", "HRANDFIELD h 3 WITHVALUES",
		// 列表
		"LRANGE l 0 10", "LLEN l", "LINDEX l 0", "LPOS l x", "LPOS l x RANK 1 COUNT 2 MAXLEN 10",
		// 集合
		"SMEMBERS s", "SCARD s", "SISMEMBER s a", "SMISMEMBER s a b", "SRANDMEMBER s", "SRANDMEMBER s 5", "SSCAN s 0", "SINTER a b", "SUNION a b", "SDIFF a b",
		// 有序集合
		"ZRANGE z 0 10", "ZRANGE z 0 10 WITHSCORES", "ZRANGE z (1 5 BYSCORE LIMIT 0 10", "ZRANGE z - + BYLEX", "ZRANGE z 0 -1 REV", "ZREVRANGE z 0 10 WITHSCORES",
		"ZRANGEBYSCORE z -inf +inf", "ZREVRANGEBYSCORE z +inf -inf WITHSCORES LIMIT 0 5", "ZRANGEBYLEX z - +", "ZCARD z", "ZSCORE z m", "ZMSCORE z a b",
		"ZRANK z m", "ZREVRANK z m WITHSCORE", "ZCOUNT z 0 1", "ZLEXCOUNT z - +", "ZSCAN z 0",
		// 其他
		"SCAN 0", "SCAN 0 MATCH user:* COUNT 100 TYPE hash", "DBSIZE", "XLEN x", "XRANGE x - +", "XREVRANGE x + - COUNT 10", "BITCOUNT k", "BITCOUNT k 0 -1 BIT", "GETBIT k 7", "PFCOUNT a b",
		"OBJECT ENCODING k", "object idletime k", "OBJECT FREQ k", "MEMORY USAGE k", "MEMORY USAGE k SAMPLES 5", "GEOPOS g a b", "GEODIST g a b km",
		"GEORADIUS_RO g 15 37 200 km WITHDIST COUNT 5", "GEOSEARCH g FROMMEMBER a BYRADIUS 10 km ASC COUNT 3 WITHCOORD",
	}
	for _, line := range ok {
		if _, err := parse(t, line); err != nil {
			t.Errorf("应放行 %q: %v", line, err)
		}
	}
	// 白名单固定：数量与分类（字符串 10 + 哈希 10 + 列表 4 + 集合 9 + 有序集合 13 + 其他 14 = 60，OBJECT / MEMORY 各算一个命令名）
	if n := len(redis.UserCommandNames()); n != 60 {
		t.Errorf("白名单命令数 = %d, want 60: %v", n, redis.UserCommandNames())
	}
}

func TestUserWhitelistDenied(t *testing.T) {
	cases := []struct{ line, want string }{
		{"SET k v", "修改数据"}, {"DEL k", "修改数据"}, {"HSET h f v", "修改数据"}, {"LPUSH l 1", "修改数据"}, {"ZADD z 1 a", "修改数据"}, {"EXPIRE k 10", "修改数据"},
		{"INCR k", "修改数据"}, {"RENAME a b", "修改数据"}, {"SINTERSTORE d a b", "修改数据"}, {"XADD x * a 1", "修改数据"}, {"GEORADIUS g 1 2 3 km STORE d", "修改数据"},
		{"KEYS *", "SCAN"}, {"keys user:*", "SCAN"},
		{"SORT k STORE d", "STORE"}, {"SORT k", "STORE"}, {"SORT_RO k", "STORE"},
		{"BLPOP l 0", "阻塞"}, {"BRPOP l 0", "阻塞"}, {"XREAD BLOCK 0 STREAMS x $", "阻塞"}, {"WAIT 1 0", "阻塞"},
		{"MONITOR", "订阅"}, {"SUBSCRIBE c", "订阅"}, {"PSUBSCRIBE c*", "订阅"},
		{"EVAL return 0", "脚本"}, {"EVALSHA abc 0", "脚本"}, {"EVAL_RO return 0", "脚本"}, {"SCRIPT FLUSH", "脚本"}, {"FUNCTION LIST", "脚本"},
		{"CONFIG GET maxmemory", "运维"}, {"CONFIG SET maxmemory 1", "运维"}, {"INFO", "运维"}, {"CLIENT LIST", "运维"}, {"DEBUG SLEEP 1", "运维"},
		{"FLUSHALL", "清空"}, {"FLUSHDB", "清空"}, {"SELECT 1", "库由页面"}, {"AUTH x", "认证"}, {"AUTH u p", "认证"}, {"HELLO 3", "认证"},
		{"SAVE", "持久化"}, {"BGSAVE", "持久化"}, {"BGREWRITEAOF", "持久化"}, {"SLAVEOF no one", "复制"}, {"REPLICAOF h 1", "复制"}, {"MIGRATE h 1 k 0 1", "复制"},
		{"MODULE LOAD x", "运维"}, {"SHUTDOWN", "运维"}, {"MULTI", "事务"}, {"EXEC", "事务"},
		{"PING", "白名单"}, {"FOOBAR", "白名单"}, {"ECHO x", "白名单"},
		// 子命令限制
		{"OBJECT HELP", "OBJECT"}, {"OBJECT", "OBJECT"}, {"MEMORY STATS", "MEMORY"}, {"MEMORY PURGE", "MEMORY"}, {"MEMORY", "MEMORY"},
		// 参数里夹带 AUTH
		{"GET AUTH", "AUTH"}, {"MGET a auth", "AUTH"},
		// 非法形态
		{"GET", "参数个数"}, {"GET a b", "参数个数"}, {"HGET h", "参数个数"}, {"LRANGE l 0", "参数个数"}, {"LRANGE l a b", "整数"},
		{"SCAN x", "游标"}, {"SCAN 0 COUNT 0", "正整数"}, {"SCAN 0 COUNT", "缺少参数"}, {"SCAN 0 FOO bar", "不支持选项"},
		{"ZRANGE z 0 1 LIMIT 0 1", "BYSCORE"}, {"ZRANGE z 0 1 BYSCORE BYLEX", "同时"}, {"ZREVRANGE z 0 1 FOO", "WITHSCORES"},
		{"MEMORY USAGE k SAMPLES x", "SAMPLES"}, {"GEODIST g a b parsec", "单位"}, {"GEORADIUS_RO g 1 2 3 km STORE d", "不支持选项"},
		{"GEOSEARCH g FROMMEMBER a BYRADIUS 1 km STOREDIST", "不支持选项"}, {"BITCOUNT k 1", "参数个数"}, {"ZRANK z m FOO", "WITHSCORE"},
		{"LPOS l x COUNT -1", "不能为负"}, {"HRANDFIELD h 1 FOO", "WITHVALUES"},
	}
	for _, c := range cases {
		args, err := redis.TokenizeLine(c.line)
		if err != nil {
			t.Fatalf("分词 %q: %v", c.line, err)
		}
		_, err = redis.ParseUserCommand(args)
		var ue *redis.UserError
		if !errors.As(err, &ue) {
			t.Errorf("应拒绝 %q，实际 err=%v", c.line, err)
			continue
		}
		if !strings.Contains(ue.Msg, c.want) {
			t.Errorf("%q 的拒绝原因 %q 应含 %q", c.line, ue.Msg, c.want)
		}
	}
	if _, err := redis.ParseUserCommand(nil); err == nil {
		t.Error("空命令应拒绝")
	}
}

func TestUserLimitsRewrite(t *testing.T) {
	cases := []struct{ line, want string }{
		// SCAN 系列：缺省补 COUNT、超限夹到 1000
		{"SCAN 0", "SCAN 0 COUNT 100"},
		{"scan 0 match user:* count 100000", "SCAN 0 match user:* count 1000"},
		{"SCAN 12 COUNT 50", "SCAN 12 COUNT 50"},
		{"HSCAN h 0", "HSCAN h 0 COUNT 100"},
		{"SSCAN s 0 COUNT 5000", "SSCAN s 0 COUNT 1000"},
		{"ZSCAN z 0 MATCH a*", "ZSCAN z 0 MATCH a* COUNT 100"},
		// 下标区间：两端非负时直接收窄
		{"LRANGE l 0 99", "LRANGE l 0 99"},
		{"LRANGE l 0 99999", "LRANGE l 0 999"},
		{"LRANGE l 500 100000", "LRANGE l 500 1499"},
		{"ZRANGE z 10 999999 WITHSCORES", "ZRANGE z 10 1009 WITHSCORES"},
		{"ZREVRANGE z 0 5000", "ZREVRANGE z 0 999"},
		// 分数 / 字典序区间：补 / 夹 LIMIT
		{"ZRANGEBYSCORE z -inf +inf", "ZRANGEBYSCORE z -inf +inf LIMIT 0 1000"},
		{"ZRANGEBYSCORE z 0 1 LIMIT 0 -1", "ZRANGEBYSCORE z 0 1 LIMIT 0 1000"},
		{"ZRANGEBYSCORE z 0 1 LIMIT 5 50000", "ZRANGEBYSCORE z 0 1 LIMIT 5 1000"},
		{"ZRANGEBYSCORE z 0 1 LIMIT 0 10", "ZRANGEBYSCORE z 0 1 LIMIT 0 10"},
		{"ZRANGEBYLEX z - +", "ZRANGEBYLEX z - + LIMIT 0 1000"},
		{"ZRANGE z 0 10 BYSCORE", "ZRANGE z 0 10 BYSCORE LIMIT 0 1000"},
		// Stream
		{"XRANGE x - +", "XRANGE x - + COUNT 1000"},
		{"XREVRANGE x + - COUNT 99999", "XREVRANGE x + - COUNT 1000"},
		// 随机取样、LPOS
		{"SRANDMEMBER s -99999", "SRANDMEMBER s -1000"},
		{"SRANDMEMBER s 99999", "SRANDMEMBER s 1000"},
		{"HRANDFIELD h 5000 WITHVALUES", "HRANDFIELD h 1000 WITHVALUES"},
		{"LPOS l x", "LPOS l x MAXLEN 100000"},
		{"LPOS l x COUNT 0 MAXLEN 0", "LPOS l x COUNT 1000 MAXLEN 100000"},
		// 地理
		{"GEORADIUS_RO g 15 37 200 km", "GEORADIUS_RO g 15 37 200 km COUNT 1000"},
		{"GEOSEARCH g FROMLONLAT 15 37 BYBOX 1 1 km COUNT 99999", "GEOSEARCH g FROMLONLAT 15 37 BYBOX 1 1 km COUNT 1000"},
		// OBJECT 子命令统一大写
		{"object encoding k", "OBJECT ENCODING k"},
	}
	for _, c := range cases {
		cmd, err := parse(t, c.line)
		if err != nil {
			t.Errorf("%q: %v", c.line, err)
			continue
		}
		if got := redis.FormatUserCommand(cmd.Args); got != c.want {
			t.Errorf("%q → %q, want %q", c.line, got, c.want)
		}
	}
	// 改写要给提示
	cmd, _ := parse(t, "SCAN 0 COUNT 99999")
	if len(cmd.Notices) == 0 || !strings.Contains(cmd.Notices[0], "COUNT") {
		t.Errorf("COUNT 夹到上限应有提示: %v", cmd.Notices)
	}
	cmd, _ = parse(t, "LRANGE l 0 99999")
	if len(cmd.Notices) == 0 || !strings.Contains(cmd.Notices[0], "收窄") {
		t.Errorf("区间收窄应有提示: %v", cmd.Notices)
	}
	cmd, _ = parse(t, "LRANGE l 200 300")
	if len(cmd.Notices) != 0 || cmd.IndexBase != 200 {
		t.Errorf("未改写不应有提示，IndexBase=200: %v %d", cmd.Notices, cmd.IndexBase)
	}
	// 标志位
	cmd, _ = parse(t, "ZRANGE z 0 1 WITHSCORES")
	if !cmd.Flags["WITHSCORES"] {
		t.Error("应记下 WITHSCORES")
	}
}

func TestQuoteRoundTrip(t *testing.T) {
	for _, s := range []string{"plain", "has space", `quo"te`, `back\slash`, "tab\there", "nl\nx", "\x00\x01\xff bin", "中文 key", "it's", "", "a'b\"c"} {
		q := redis.QuoteCLIArg(s)
		back, err := redis.TokenizeLine("CMD " + q)
		if err != nil || len(back) != 2 || back[1] != s {
			t.Errorf("往返失败 %q → %s → %q (%v)", s, q, back, err)
		}
	}
	if redis.QuoteCLIArg("user:1001") != "user:1001" {
		t.Error("普通 key 不应加引号")
	}
}

// ---- 假服务端上的探测与浏览 ----

type fakeData struct {
	sizes     map[string]int64  // key → LLEN/HLEN/SCARD/ZCARD/STRLEN
	types     map[string]string // key → type
	scanPages map[string][]any  // 游标 → 回复
	configErr bool
	keyspace  string
}

func (d *fakeData) handler() redistest.Handler {
	return func(a []string) any {
		switch strings.ToUpper(a[0]) {
		case "SELECT":
			return redistest.Simple("OK")
		case "LLEN", "HLEN", "SCARD", "ZCARD", "STRLEN":
			return d.sizes[a[1]]
		case "TYPE":
			if t, ok := d.types[a[1]]; ok {
				return redistest.Simple(t)
			}
			return redistest.Simple("none")
		case "SCAN":
			if p, ok := d.scanPages[a[1]]; ok {
				return p
			}
			return []any{"0", []any{}}
		case "CONFIG":
			if d.configErr {
				return redistest.Err("ERR unknown command 'CONFIG'")
			}
			return []any{"databases", "4"}
		case "INFO":
			return d.keyspace
		}
		return []any{"echo"}
	}
}

func dialUser(t *testing.T, d *fakeData) (*redis.Client, *redistest.Server, *redis.Engine) {
	t.Helper()
	srv, err := redistest.New(d.handler())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	e := redis.New(engine.Conn{Host: srv.Host(), Port: srv.Port()})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	c, err := e.DialDB(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, srv, e
}

func lastCmd(srv *redistest.Server) string {
	cs := srv.Commands()
	return strings.Join(cs[len(cs)-1], " ")
}

func TestRunUserProbes(t *testing.T) {
	d := &fakeData{sizes: map[string]int64{"big": 10000, "small": 3, "list3": 3, "list100k": 100000, "s1": 3000, "s2": 3000, "str": 100 << 20}}
	c, srv, _ := dialUser(t, d)
	ctx := context.Background()
	run := func(line string) (*redis.UserResult, error) {
		cmd, err := parse(t, line)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		return c.RunUser(ctx, cmd)
	}
	// DialDB 先 SELECT 3
	if cs := srv.Commands(); len(cs) == 0 || strings.Join(cs[0], " ") != "SELECT 3" {
		t.Errorf("应先选库: %v", cs)
	}
	// 全量命令：超阈值拒绝并提示改用 *SCAN，且没有发出全量命令
	for _, c := range []struct{ line, hint string }{{"HGETALL big", "HSCAN big 0 COUNT 100"}, {"HKEYS big", "HSCAN"}, {"HVALS big", "HSCAN"}, {"SMEMBERS big", "SSCAN big 0 COUNT 100"}} {
		_, err := run(c.line)
		var ue *redis.UserError
		if !errors.As(err, &ue) || !strings.Contains(ue.Msg, c.hint) || !strings.Contains(ue.Msg, "10000") {
			t.Errorf("%q 应因集合过大被拒绝并提示 %s: %v", c.line, c.hint, err)
		}
	}
	for _, cs := range srv.Commands() {
		if n := strings.ToUpper(cs[0]); n == "HGETALL" || n == "SMEMBERS" || n == "HKEYS" || n == "HVALS" {
			t.Fatalf("超阈值时不应发出全量命令: %v", cs)
		}
	}
	// 小集合放行并真正发出命令
	if r, err := run("HGETALL small"); err != nil || lastCmd(srv) != "HGETALL small" || r == nil {
		t.Errorf("小哈希应放行: %v %s", err, lastCmd(srv))
	}
	// 多个集合求交并：长度合计超阈值拒绝
	if _, err := run("SINTER s1 s2"); err == nil {
		t.Error("两个集合合计 6000 > 5000 应拒绝")
	}
	if _, err := run("SINTER s1 small"); err != nil {
		t.Errorf("3003 个元素应放行: %v", err)
	}
	// 负下标先探测长度再换算
	if r, err := run("LRANGE list3 0 -1"); err != nil || lastCmd(srv) != "LRANGE list3 0 2" || r.IndexBase != 0 {
		t.Errorf("0 -1 应换算成 0 2: %v %s", err, lastCmd(srv))
	}
	if r, err := run("LRANGE list100k -50 -1"); err != nil || lastCmd(srv) != "LRANGE list100k 99950 99999" || r.IndexBase != 99950 {
		t.Errorf("-50 -1 应换算成绝对下标: %v %s", err, lastCmd(srv))
	}
	r, err := run("LRANGE list100k 0 -1")
	if err != nil || lastCmd(srv) != "LRANGE list100k 0 999" || len(r.Notices) == 0 || !strings.Contains(strings.Join(r.Notices, ""), "100000") {
		t.Errorf("大列表 0 -1 应收窄到 1000 条并提示总数: %v %s %v", err, lastCmd(srv), r)
	}
	if _, err := run("ZRANGE small 0 -1 WITHSCORES"); err != nil || lastCmd(srv) != "ZRANGE small 0 2 WITHSCORES" {
		t.Errorf("ZRANGE 负下标换算: %v %s", err, lastCmd(srv))
	}
	// 空列表：原样发送
	if _, err := run("LRANGE nokey 0 -1"); err != nil || lastCmd(srv) != "LRANGE nokey 0 -1" {
		t.Errorf("空列表原样发送: %v %s", err, lastCmd(srv))
	}
	// 不带区间的 BITCOUNT：超长字符串拒绝
	if _, err := run("BITCOUNT str"); err == nil || !strings.Contains(err.Error(), "区间") {
		t.Errorf("BITCOUNT 大字符串应拒绝: %v", err)
	}
	if _, err := run("BITCOUNT str 0 100"); err != nil {
		t.Errorf("带区间的 BITCOUNT 应放行: %v", err)
	}
	// 取消
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	cmd, _ := parse(t, "GET a")
	if _, err := c.RunUser(cctx, cmd); !errors.Is(err, context.Canceled) {
		t.Errorf("已取消的 ctx: %v", err)
	}
}

func TestBrowseKeys(t *testing.T) {
	d := &fakeData{
		types: map[string]string{"a": "string", "b": "hash", "c": "list", "d": "zset", "e": "set"},
		scanPages: map[string][]any{
			"0":  {"17", []any{"a"}},
			"17": {"33", []any{}}, // 空批：继续
			"33": {"0", []any{"b", "c", "d", "e"}},
		},
	}
	c, srv, _ := dialUser(t, d)
	page, err := c.BrowseKeys(context.Background(), "*", "0", 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range page.Keys {
		got = append(got, k.Key+":"+k.Type)
	}
	if strings.Join(got, ",") != "a:string,b:hash,c:list,d:zset,e:set" || page.Cursor != "0" || page.Scanned != 3 {
		t.Errorf("浏览结果: %v cursor=%s scanned=%d", got, page.Cursor, page.Scanned)
	}
	// SCAN 都带 COUNT，且不超过 500；MATCH 非 * 时才带
	for _, cs := range srv.Commands() {
		if strings.ToUpper(cs[0]) != "SCAN" {
			continue
		}
		line := strings.Join(cs, " ")
		n := -1
		for i, a := range cs {
			if strings.EqualFold(a, "COUNT") && i+1 < len(cs) {
				n, _ = strconv.Atoi(cs[i+1])
			}
		}
		if n < 1 || n > 500 || strings.Contains(strings.ToUpper(line), "MATCH") {
			t.Errorf("SCAN 参数不对: %s", line)
		}
	}
	// 非 * 模式带 MATCH；want 够了就停
	d.scanPages = map[string][]any{"0": {"9", []any{"user:1", "user:2"}}, "9": {"0", []any{"user:3"}}}
	d.types = map[string]string{"user:1": "string", "user:2": "string", "user:3": "hash"}
	page, err = c.BrowseKeys(context.Background(), "user:*", "0", 2)
	if err != nil || len(page.Keys) != 2 || page.Cursor != "9" {
		t.Errorf("够数就停并返回游标: %v %+v", err, page)
	}
	found := false
	for _, cs := range srv.Commands() {
		if strings.ToUpper(cs[0]) == "SCAN" && strings.Contains(strings.Join(cs, " "), "MATCH user:*") {
			found = true
		}
	}
	if !found {
		t.Error("应带 MATCH user:*")
	}
	if _, err := c.BrowseKeys(context.Background(), "*", "abc", 10); err == nil {
		t.Error("非法游标应拒绝")
	}
	if _, err := c.BrowseKeys(context.Background(), strings.Repeat("a", 600), "0", 10); err == nil {
		t.Error("过长的 MATCH 应拒绝")
	}
}

func TestDatabaseList(t *testing.T) {
	d := &fakeData{keyspace: "# Keyspace\r\ndb0:keys=10,expires=2,avg_ttl=0\r\ndb2:keys=7,expires=0,avg_ttl=0\r\n"}
	_, _, e := dialUser(t, d)
	list, err := e.DatabaseList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 || list[0].Keys != 10 || list[1].Keys != 0 || list[2].Keys != 7 || list[3].DB != 3 {
		t.Errorf("库列表: %+v", list)
	}
	// CONFIG GET 失败：回落 16 个库
	d.configErr = true
	if list, err = e.DatabaseList(context.Background()); err != nil || len(list) != 16 {
		t.Errorf("CONFIG 失败应回落 16: %v %d", err, len(list))
	}
}
