// @author: kevinkelly
// @license: Apache Licence
// @file: redis_test.go
// @time: 2026/10/28

package query

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	redisx "dbtool/internal/engine/redis"
	"dbtool/internal/engine/redis/redistest"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// 用 redistest 假服务端 + 真实的 redis 引擎测在线查询的 Redis 分支；MySQL / SQL Server 的回归由其余测试保证。

func redisHandler() redistest.Handler {
	return func(a []string) any {
		switch strings.ToUpper(a[0]) {
		case "SELECT":
			return redistest.Simple("OK")
		case "GET":
			switch a[1] {
			case "user:1":
				return "alice"
			case "bin":
				return "\xff\xfe\x00x\x01"
			}
			return nil
		case "MGET":
			return []any{"1", nil}
		case "HLEN":
			if a[1] == "h:big" {
				return int64(9000)
			}
			return int64(2)
		case "HGETALL":
			return []any{"f1", "v1", "f2", "v2"}
		case "SCARD":
			return int64(3)
		case "SMEMBERS":
			return []any{"a", "b", "c"}
		case "LLEN":
			return int64(5)
		case "ZCARD":
			return int64(2)
		case "LRANGE":
			return []any{"e0", "e1", "e2", "e3", "e4"}
		case "ZRANGE":
			return []any{"m1", "1.5", "m2", "2"}
		case "SCAN":
			return []any{"42", []any{"k1", "k2"}}
		case "HSCAN":
			return []any{"7", []any{"f", "v"}}
		case "TYPE":
			if strings.HasPrefix(a[1], "h") {
				return redistest.Simple("hash")
			}
			return redistest.Simple("string")
		case "DBSIZE":
			return int64(11)
		case "TTL":
			return int64(-1)
		case "XRANGE":
			return []any{[]any{"1-0", []any{"a", "1", "b", "2"}}, []any{"2-0", []any{"a", "3"}}}
		case "GEOSEARCH":
			return []any{[]any{"p1", "1.5", []any{"13.1", "38.2"}}}
		case "GEOPOS":
			return []any{[]any{"13.1", "38.2"}, nil}
		case "CONFIG":
			return []any{"databases", "4"}
		case "INFO":
			return "# Keyspace\r\ndb0:keys=10,expires=2,avg_ttl=0\r\ndb3:keys=1,expires=0,avg_ttl=0\r\n"
		}
		return redistest.Err("ERR unknown command '" + a[0] + "'")
	}
}

func redisRepo(t *testing.T) (*evRepo, *redistest.Server) {
	t.Helper()
	srv, err := redistest.New(redisHandler())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	r := newRepo()
	r.insts[3] = &model.Instance{ID: 3, InstanceName: "redis-01", DBType: "redis", Type: "master", Host: srv.Host(), Port: srv.Port(),
		Tags: []model.InstanceTag{{TagCode: "can_read", TagName: "支持查询", Active: true}}}
	r.perms[7] = map[string]bool{"query.run": true, "query.view": true}
	r.userInst[7] = map[uint]bool{3: true}
	r.privs = []model.QueryPrivilege{{UserName: "dev", InstanceID: 3, PrivType: 1, DBName: "3", LimitNum: 4, ValidDate: time.Now().AddDate(0, 0, 10)}}
	return &evRepo{histRepo: &histRepo{fakeRepo: r}, ready: true}, srv
}

func redisSvc(r Repo) *Service { return NewService(r, nil, nil) }

func rexec(s *Service, p Principal, db, cmd string) (*Result, error) {
	return s.Execute(context.Background(), p, Request{InstanceID: 3, DB: db, SQL: cmd})
}

func TestRedisSupportsQuery(t *testing.T) {
	if !SupportsQuery("redis") {
		t.Fatal("redis 应支持在线查询")
	}
	if SupportsQuery("cassandra") || SupportsQuery("elasticsearch") || SupportsQuery("tdengine") {
		t.Error("其他类型的支持情况不应变")
	}
	for _, ty := range []string{"pgsql", "mongo", "clickhouse", "oracle", "dm"} { // 阶段 2 接入，见 multi_test.go；oracle / dm 见 oracle_test.go、dm_test.go
		if !SupportsQuery(ty) {
			t.Errorf("%s 应支持在线查询", ty)
		}
	}
}

func TestRedisResultShaping(t *testing.T) {
	repo, _ := redisRepo(t)
	s := redisSvc(repo)
	cases := []struct {
		cmd     string
		cols    string
		rows    string // 各行用 | 连接
		kind    string
		cursor  string
		wantErr string
	}{
		{cmd: "GET user:1", cols: "value", rows: "alice"},
		{cmd: "get nokey", cols: "value", rows: "(nil)"},
		{cmd: "TTL user:1", cols: "value", rows: "-1"},
		{cmd: "DBSIZE", cols: "value", rows: "11"},
		{cmd: "HGETALL h", cols: "field,value", rows: "f1,v1|f2,v2"},
		{cmd: "SMEMBERS s", cols: "index,member", rows: "0,a|1,b|2,c"},
		{cmd: "LRANGE l 0 4", cols: "index,value", rows: "0,e0|1,e1|2,e2|3,e3|4,e4"},
		{cmd: "ZRANGE z 0 -1 WITHSCORES", cols: "member,score", rows: "m1,1.5|m2,2"},
		{cmd: "ZRANGE z 0 -1", cols: "index,member", rows: "0,m1|1,1.5|2,m2|3,2"}, // 假服务端固定回 4 个元素
		{cmd: "MGET a b", cols: "key,value", rows: "a,1|b,(nil)"},
		{cmd: "SCAN 0", cols: "index,key", rows: "(cursor),42|1,k1|2,k2", cursor: "42"},
		{cmd: "HSCAN h 0", cols: "field,value", rows: "(cursor),7|f,v", cursor: "7"},
		{cmd: "XRANGE x - +", cols: "id,field,value", rows: "1-0,a,1|1-0,b,2|2-0,a,3"},
		{cmd: "GEOSEARCH g FROMMEMBER a BYRADIUS 5 km WITHDIST WITHCOORD", cols: "member,distance,longitude,latitude", rows: "p1,1.5,13.1,38.2"},
		{cmd: "GEOPOS g a b", cols: "member,longitude,latitude", rows: "a,13.1,38.2|b,(nil),(nil)"},
	}
	for _, c := range cases {
		res, err := rexec(s, admin, "3", c.cmd)
		if err != nil {
			t.Errorf("%q: %v", c.cmd, err)
			continue
		}
		if res.Kind != "redis" || strings.Join(res.Columns, ",") != c.cols {
			t.Errorf("%q: kind=%s columns=%v, want %s", c.cmd, res.Kind, res.Columns, c.cols)
		}
		var rows []string
		for _, r := range res.Rows {
			var cells []string
			for _, v := range r {
				cells = append(cells, fmt.Sprint(v))
			}
			rows = append(rows, strings.Join(cells, ","))
		}
		if got := strings.Join(rows, "|"); got != c.rows {
			t.Errorf("%q 结果行 = %q, want %q", c.cmd, got, c.rows)
		}
		if res.Cursor != c.cursor {
			t.Errorf("%q 游标 = %q, want %q", c.cmd, res.Cursor, c.cursor)
		}
		if res.Truncated {
			t.Errorf("%q 不应截断", c.cmd)
		}
	}
	// 管理员的查询写日志：库编号规范化、记录最终发出的命令
	if n := len(repo.logs); n != len(cases) {
		t.Fatalf("dbt_query_log 条数 = %d", n)
	}
	last := repo.logs[len(repo.logs)-1]
	if last.DBName != "3" || last.InstanceName != "redis-01" || last.Username != "root" || !strings.HasPrefix(last.SQLLog, "GEOPOS") {
		t.Errorf("日志: %+v", last)
	}
}

func TestRedisBinaryAndCells(t *testing.T) {
	repo, _ := redisRepo(t)
	s := redisSvc(repo)
	res, err := rexec(s, admin, "db3", "GET bin") // db3 与 3 等价
	if err != nil {
		t.Fatal(err)
	}
	if v := fmt.Sprint(res.Rows[0][0]); v != `\xff\xfe\x00x\x01` {
		t.Errorf("二进制应转 \\xHH: %q", v)
	}
	if !strings.Contains(res.Notice, "不可见字符") {
		t.Errorf("应提示含不可见字符: %q", res.Notice)
	}
	// redisText 的边界
	for in, want := range map[string]string{"plain": "plain", "中文": "中文", "tab\tnl\n": "tab\tnl\n", "\x00": `\x00`, "a\x7fb": `a\x7fb`, "\xc3\x28": `\xc3(`} {
		if got, _ := redisText(in); got != want {
			t.Errorf("redisText(%q) = %q, want %q", in, got, want)
		}
	}
	if _, bin := redisText("ok"); bin {
		t.Error("普通文本不是二进制")
	}
	// 超长单元格截断
	sh := &redisShape{max: 10}
	sh.add(strings.Repeat("x", redisMaxCell+10))
	if sh.Cut != 1 || !strings.Contains(fmt.Sprint(sh.Rows[0][0]), "已截断") {
		t.Errorf("超长单元格应截断: cut=%d", sh.Cut)
	}
	// 回复字节上限
	sh = &redisShape{}
	big := strings.Repeat("y", 60<<10)
	for i := 0; i < 100; i++ {
		if !sh.add(big) {
			break
		}
	}
	if !sh.Truncated || len(sh.Rows) >= 100 || len(sh.Rows) < 60 {
		t.Errorf("字节上限: truncated=%v rows=%d", sh.Truncated, len(sh.Rows))
	}
}

func TestRedisRowLimitTruncates(t *testing.T) {
	repo, _ := redisRepo(t)
	s := redisSvc(repo)
	// 普通用户：库权限上限 4 行；LRANGE 返回 5 个元素 → 截断
	res, err := rexec(s, dev, "3", "LRANGE l 0 4")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 4 || !res.Truncated || res.EffectiveLimit != 4 || res.LimitSource != "db3 库权限" || !strings.Contains(res.Notice, "截断") {
		t.Errorf("行数上限: rows=%d truncated=%v limit=%d src=%q notice=%q", len(res.Rows), res.Truncated, res.EffectiveLimit, res.LimitSource, res.Notice)
	}
	// 请求里的 limit 更小时取小
	res, err = s.Execute(context.Background(), dev, Request{InstanceID: 3, DB: "3", SQL: "LRANGE l 0 4", Limit: 2})
	if err != nil || len(res.Rows) != 2 || !res.Truncated || res.EffectiveLimit != 2 {
		t.Errorf("请求 limit: %v %+v", err, res)
	}
	// 区间被收窄的提示、实际执行的命令
	res, err = rexec(s, admin, "3", "LRANGE l 0 99999")
	if err != nil || res.ExecutedSQL != "LRANGE l 0 999" || !strings.Contains(res.Notice, "收窄") {
		t.Errorf("区间收窄: %v %q %q", err, res.ExecutedSQL, res.Notice)
	}
	if res, err = rexec(s, admin, "3", "SCAN 0"); err != nil || res.NextCmd != "SCAN 42 COUNT 100" {
		t.Errorf("SCAN 下一页命令: %v %q", err, res.NextCmd)
	}
	if res, err = rexec(s, admin, "3", "HSCAN h:a 0 MATCH f*"); err != nil || res.NextCmd != "HSCAN h:a 7 MATCH f* COUNT 100" {
		t.Errorf("HSCAN 下一页命令: %v %q", err, res.NextCmd)
	}
	res, err = rexec(s, admin, "3", "SCAN 0 MATCH user:*")
	if err != nil || res.ExecutedSQL != "SCAN 0 MATCH user:* COUNT 100" || !strings.Contains(res.Notice, "COUNT") {
		t.Errorf("SCAN 补 COUNT: %v %q %q", err, res.ExecutedSQL, res.Notice)
	}
}

func TestRedisRejectedAndRecorded(t *testing.T) {
	repo, srv := redisRepo(t)
	s := redisSvc(repo)
	bad := []struct{ cmd, why string }{
		{"SET k v", "修改数据"}, {"DEL k", "修改数据"}, {"KEYS *", "SCAN"}, {"FLUSHALL", "清空"}, {"EVAL return 0", "脚本"},
		{"CONFIG GET *", "运维"}, {"INFO", "运维"}, {"SELECT 1", "库由页面"}, {"AUTH pass", "认证"}, {"BLPOP l 0", "阻塞"},
		{"GET a\nGET b", "一次只能执行一条"}, {"", ""}, {"GET 'unclosed", "命令解析失败"}, {"GET AUTH", "AUTH"},
	}
	for _, c := range bad {
		_, err := rexec(s, admin, "3", c.cmd)
		if err == nil {
			t.Errorf("应拒绝 %q", c.cmd)
			continue
		}
		if c.why != "" && !strings.Contains(err.Error(), c.why) {
			t.Errorf("%q 的拒绝原因 %q 应含 %q", c.cmd, err.Error(), c.why)
		}
	}
	// 被拒绝的命令不能到达 Redis（只允许 SELECT 之类的连接动作；这里一条都不应有）
	if cs := srv.Commands(); len(cs) != 0 {
		t.Errorf("被拒绝的命令不应连到 Redis: %v", cs)
	}
	// 拦截记录：除“请求不完整”（空命令）外都是 blocked / bad_query，并带原因
	blocked := 0
	for _, e := range repo.events {
		if e.Status == StatusBlocked && e.Code == "bad_query" && e.InstanceID == 3 && e.DB == "3" && e.Reason != "" {
			blocked++
		}
	}
	if blocked != len(bad)-1 {
		t.Errorf("拦截记录 %d 条, want %d: %+v", blocked, len(bad)-1, repo.events)
	}
	if len(repo.logs) != 0 {
		t.Error("被拦截的命令不应写 dbt_query_log")
	}
	// 缺库
	if _, err := rexec(s, admin, "", "GET a"); code(err) != "bad_request" {
		t.Errorf("缺库: %v", err)
	}
	if _, err := rexec(s, admin, "abc", "GET a"); code(err) != "bad_request" {
		t.Errorf("库编号非法: %v", err)
	}
}

func TestRedisBigCollectionRejected(t *testing.T) {
	repo, srv := redisRepo(t)
	s := redisSvc(repo)
	_, err := rexec(s, admin, "3", "HGETALL h:big")
	if code(err) != "bad_query" || !strings.Contains(err.Error(), "HSCAN h:big 0 COUNT 100") {
		t.Fatalf("大哈希应拒绝并提示 HSCAN: %v", err)
	}
	for _, c := range srv.Commands() {
		if strings.EqualFold(c[0], "HGETALL") {
			t.Fatal("不应发出 HGETALL")
		}
	}
	if n := len(repo.events); n != 1 || repo.events[0].Status != StatusBlocked {
		t.Errorf("应记一条拦截记录: %+v", repo.events)
	}
}

func TestRedisDBPrivilege(t *testing.T) {
	repo, _ := redisRepo(t)
	s := redisSvc(repo)
	// 有 3 库权限
	if _, err := rexec(s, dev, "3", "GET user:1"); err != nil {
		t.Fatal(err)
	}
	// 其他库：无权限，带结构化的库名方便前端申请
	_, err := rexec(s, dev, "4", "GET user:1")
	qe, ok := err.(*Error)
	if !ok || qe.Code != "no_privilege" || qe.Data["database"] != "4" || !strings.Contains(qe.Message, "db4") {
		t.Fatalf("无权限: %v", err)
	}
	// 没关联实例
	repo.userInst[7] = map[uint]bool{}
	if _, err := rexec(s, dev, "3", "GET a"); code(err) != "no_privilege" {
		t.Errorf("未关联实例: %v", err)
	}
	repo.userInst[7] = map[uint]bool{3: true}
	// 缺 query.run
	repo.perms[7] = map[string]bool{"query.view": true}
	if _, err := rexec(s, dev, "3", "GET a"); code(err) != "no_privilege" {
		t.Errorf("缺 query.run: %v", err)
	}
	repo.perms[7] = map[string]bool{"query.run": true}
	// "*" 库权限放行任意库；表权限（priv_type=2）对 Redis 无效
	repo.privs = []model.QueryPrivilege{{UserName: "dev", InstanceID: 3, PrivType: 2, DBName: "3", TbName: "x", LimitNum: 9, ValidDate: time.Now().AddDate(0, 0, 3)}}
	if _, err := rexec(s, dev, "3", "GET a"); code(err) != "no_privilege" {
		t.Errorf("表权限不应放行 Redis: %v", err)
	}
	repo.privs = []model.QueryPrivilege{{UserName: "dev", InstanceID: 3, PrivType: 1, DBName: "*", LimitNum: 9, ValidDate: time.Now().AddDate(0, 0, 3)}}
	if res, err := rexec(s, dev, "7", "GET a"); err != nil || res.EffectiveLimit != 9 {
		t.Errorf("* 库权限: %v %+v", err, res)
	}
	// 命令里的 key 看起来像 SQL 函数不影响权限判断（不走 MySQL 的危险函数检查）
	if _, err := rexec(s, admin, "3", "GET sleep(1)"); err != nil {
		t.Errorf("key 名不应被当作 SQL 函数: %v", err)
	}
	if _, err := rexec(s, dev, "3", "GET benchmark(1)"); err != nil {
		t.Errorf("key 名不应被当作 SQL 函数: %v", err)
	}
	// 权限校验失败写拦截记录
	found := false
	for _, e := range repo.events {
		if e.Code == "no_privilege" && e.DB == "4" && e.UserName == "dev" {
			found = true
		}
	}
	if !found {
		t.Errorf("无权限应记拦截记录: %+v", repo.events)
	}
}

func TestRedisDatabasesAndMeta(t *testing.T) {
	repo, _ := redisRepo(t)
	s := redisSvc(repo)
	ctx := context.Background()
	dbs, err := s.Databases(ctx, admin, 3)
	if err != nil || strings.Join(dbs, ",") != "0,1,2,3" {
		t.Fatalf("库列表: %v %v", err, dbs)
	}
	items, err := s.RedisDatabases(ctx, admin, 3)
	if err != nil || len(items) != 4 || items[0].Keys != 10 || items[1].Keys != 0 || items[3].Keys != 1 {
		t.Errorf("带键数的库列表: %v %+v", err, items)
	}
	// 实例的库名过滤规则同样适用
	repo.insts[3].DeniedDBNameRegex = "^[12]$"
	if dbs, _ = s.Databases(ctx, admin, 3); strings.Join(dbs, ",") != "0,3" {
		t.Errorf("过滤后: %v", dbs)
	}
	repo.insts[3].DeniedDBNameRegex = ""
	// 非 redis 实例不能走 redis 库列表
	if _, err = s.RedisDatabases(ctx, admin, 1); err == nil {
		t.Error("mysql 实例不能走 redis 库列表")
	}
	// 元数据：没有表，带限制策略
	m, err := s.DBMeta(ctx, dev, 3, "3")
	if err != nil || len(m.Tables) != 0 || m.Policy.Mode != "priv" || !m.Policy.AllTables || m.Policy.MaxRows != 4 {
		t.Errorf("meta: %v %+v", err, m)
	}
	if m, _ = s.DBMeta(ctx, dev, 3, "5"); m.Policy.Mode != "none" {
		t.Errorf("无权限库: %+v", m.Policy)
	}
	if m, _ = s.DBMeta(ctx, admin, 3, "0"); m.Policy.Mode != "admin" {
		t.Errorf("管理员: %+v", m.Policy)
	}
	// 实例列表里出现 redis；只有被关联的用户看得到
	ins, _ := s.Instances(dev)
	if len(ins) != 1 || ins[0].DBType != "redis" || ins[0].ID != 3 {
		t.Errorf("dev 可见实例: %+v", ins)
	}
}

func TestRedisKeysBrowse(t *testing.T) {
	repo, srv := redisRepo(t)
	s := redisSvc(repo)
	ctx := context.Background()
	res, err := s.RedisKeys(ctx, dev, 3, "3", RedisKeysReq{Match: "user:*", Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Keys) != 2 || res.Keys[0].Key != "k1" || res.Keys[0].Type != "string" || res.Cursor != "42" || res.Done || res.Match != "user:*" {
		t.Errorf("key 浏览: %+v", res)
	}
	sawMatch := false
	for _, c := range srv.Commands() {
		if strings.EqualFold(c[0], "SCAN") && strings.Contains(strings.Join(c, " "), "MATCH user:*") {
			sawMatch = true
		}
	}
	if !sawMatch {
		t.Error("SCAN 应带 MATCH")
	}
	// 需要库权限
	if _, err = s.RedisKeys(ctx, dev, 3, "4", RedisKeysReq{}); code(err) != "no_privilege" {
		t.Errorf("无权限库不能浏览 key: %v", err)
	}
	if _, err = s.RedisKeys(ctx, dev, 1, "3", RedisKeysReq{}); err == nil {
		t.Error("非 redis 实例不能浏览 key")
	}
	if _, err = s.RedisKeys(ctx, admin, 3, "x", RedisKeysReq{}); code(err) != "bad_request" {
		t.Errorf("库编号非法: %v", err)
	}
	if _, err = s.RedisKeys(ctx, admin, 3, "0", RedisKeysReq{Cursor: "abc"}); err == nil {
		t.Error("非法游标应拒绝")
	}
	// 二进制 key：显示转义，Arg 可直接放进命令
	text, bin := redisText("a\xffb")
	if !bin || text != `a\xffb` || redisx.QuoteCLIArg("a\xffb") != `"a\xffb"` {
		t.Errorf("二进制 key: %q %v %q", text, bin, redisx.QuoteCLIArg("a\xffb"))
	}
}

func TestRedisUserConcurrency(t *testing.T) {
	var rel []func()
	for i := 0; i < redisUserConcurrency; i++ {
		r, err := acquireRedisSlot("conc-user")
		if err != nil {
			t.Fatal(err)
		}
		rel = append(rel, r)
	}
	if _, err := acquireRedisSlot("conc-user"); code(err) != "busy" {
		t.Errorf("超过并发上限: %v", err)
	}
	if r, err := acquireRedisSlot("other-user"); err != nil {
		t.Errorf("其他用户不受影响: %v", err)
	} else {
		r()
	}
	rel[0]()
	if r, err := acquireRedisSlot("conc-user"); err != nil {
		t.Errorf("释放后可再用: %v", err)
	} else {
		r()
	}
	for _, r := range rel[1:] {
		r()
	}
	redisSlots.Lock()
	n := len(redisSlots.n)
	redisSlots.Unlock()
	if n != 0 {
		t.Errorf("全部释放后不应残留: %d", n)
	}
}

func TestRedisTimeoutAndCancel(t *testing.T) {
	block := make(chan struct{})
	srv, err := redistest.New(func(a []string) any {
		if strings.EqualFold(a[0], "GET") {
			<-block
		}
		return redistest.Simple("OK")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	defer close(block)
	repo, _ := redisRepo(t)
	repo.insts[3].Host, repo.insts[3].Port = srv.Host(), srv.Port()
	repo.cfg[settings.KeyQueryMaxExecSeconds] = "1"
	s := redisSvc(repo)
	if _, err := rexec(s, admin, "0", "GET a"); code(err) != "timeout" {
		t.Errorf("超时: %v", err)
	}
	repo.cfg[settings.KeyQueryMaxExecSeconds] = "30"
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	if _, err := s.Execute(ctx, admin, Request{InstanceID: 3, DB: "0", SQL: "GET a"}); code(err) != "canceled" {
		t.Errorf("取消: %v", err)
	}
	var got []string
	for _, e := range repo.events {
		got = append(got, e.Status)
	}
	if strings.Join(got, ",") != StatusTimeout+","+StatusCanceled {
		t.Errorf("超时 / 取消记录: %v", got)
	}
}

func TestRedisCheckFor(t *testing.T) {
	s := redisSvc(newRepo())
	stmt, notice, err := s.checkFor("redis", `get "a b"`, "0", Policy{})
	if err != nil || stmt != `GET "a b"` || notice != "" {
		t.Errorf("checkFor: %q %q %v", stmt, notice, err)
	}
	if _, _, err = s.checkFor("redis", "set a 1", "0", Policy{}); code(err) != "bad_query" {
		t.Errorf("写命令: %v", err)
	}
	// MySQL / SQL Server 的检查不受影响
	if _, _, err = s.checkFor("mysql", "select 1", "", Policy{}); err != nil {
		t.Errorf("mysql: %v", err)
	}
	if _, _, err = s.checkFor("mssql", "select 1", "", Policy{}); err != nil {
		t.Errorf("mssql: %v", err)
	}
}

func TestRedisEstimateIsUnknown(t *testing.T) {
	repo, _ := redisRepo(t)
	est, err := redisSvc(repo).EstimateRows(context.Background(), admin, Request{InstanceID: 3, DB: "0", SQL: "SCAN 0"})
	if err != nil || est.Rows != -1 {
		t.Errorf("Redis 不估算行数: %v %+v", err, est)
	}
}

func TestRedisAuditFailureBlocksResult(t *testing.T) {
	repo, _ := redisRepo(t)
	repo.logErr = fmt.Errorf("db down")
	if _, err := rexec(redisSvc(repo), admin, "0", "GET user:1"); code(err) != "audit_failed" {
		t.Errorf("审计写不进去不应返回结果: %v", err)
	}
}
