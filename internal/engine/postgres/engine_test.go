// @author: kevinkelly
// @license: Apache Licence
// @file: engine_test.go
// @time: 2026/10/28

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"dbtool/internal/engine"
)

// route 按子串匹配语句（先匹配先返回）。
type route struct {
	sub string
	res *fakeResult
}

func routes(rs ...route) fakeHandler {
	return func(sql string, params []string) *fakeResult {
		for _, r := range rs {
			if strings.Contains(sql, r.sub) {
				return r.res
			}
		}
		return nil
	}
}

func intRows(n int) *fakeResult {
	res := &fakeResult{Cols: []fakeCol{{"id", 23}, {"name", 25}}}
	for i := 1; i <= n; i++ {
		res.Rows = append(res.Rows, cell(fmt.Sprint(i), fmt.Sprintf("name%d", i)))
	}
	return res
}

func newEngine(t *testing.T, fs *fakeServer, user, password, db string) *Engine {
	t.Helper()
	e, err := New(fs.conn(user, password, db))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func mustCheck(t *testing.T, sql string) *Checked {
	t.Helper()
	c, err := CheckUserQuery(sql, CheckOptions{})
	if err != nil {
		t.Fatalf("%q: %v", sql, err)
	}
	return c
}

func TestRegistered(t *testing.T) {
	found := false
	for _, n := range engine.Supported() {
		if n == "pgsql" {
			found = true
		}
	}
	if !found {
		t.Fatal("pgsql 没有注册")
	}
	e, err := engine.New("pgsql", engine.Conn{Host: "127.0.0.1", Port: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.Name() != "pgsql" {
		t.Fatal(e.Name())
	}
	if _, err := New(engine.Conn{}); err == nil {
		t.Fatal("地址为空应报错")
	}
}

func TestConnectStartupParams(t *testing.T) {
	fs := newFakeServer(t, routes(route{"SELECT 1", &fakeResult{Cols: []fakeCol{{"?column?", 23}}, Rows: [][]*string{cell("1")}}}))
	fs.password = "s3cret-pw"
	e := newEngine(t, fs, "alice", "s3cret-pw", "appdb")
	if err := e.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	ps := fs.startupParams()
	if len(ps) == 0 {
		t.Fatal("没有收到启动包")
	}
	p := ps[0]
	want := map[string]string{
		"user": "alice", "database": "appdb", "default_transaction_read_only": "on", "client_encoding": "UTF8", "standard_conforming_strings": "on",
		"application_name": "dbtool", "lock_timeout": "5000", "idle_in_transaction_session_timeout": "60000", "bytea_output": "hex",
		"statement_timeout": "30000", "DateStyle": "ISO, MDY", "IntervalStyle": "postgres", "extra_float_digits": "3",
	}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("启动参数 %s = %q，want %q", k, p[k], v)
		}
	}
	if _, ok := p["password"]; ok {
		t.Error("启动包不应带密码")
	}
	// 未勾选 SSL：先尝试 TLS（被假服务端拒绝）再回落明文
	fs.mu.Lock()
	ssl := fs.ssl
	fs.mu.Unlock()
	if ssl == 0 {
		t.Error("未勾选 SSL 时应先尝试 TLS（sslmode=prefer 语义）")
	}
}

func TestAuthFailureDoesNotLeakPassword(t *testing.T) {
	fs := newFakeServer(t, nil)
	fs.password = "right-password-123"
	e := newEngine(t, fs, "alice", "wrong-password-456", "appdb")
	err := e.TestConnection(context.Background())
	if err == nil {
		t.Fatal("密码错误应失败")
	}
	for _, pw := range []string{"wrong-password-456", "right-password-123"} {
		if strings.Contains(err.Error(), pw) {
			t.Fatalf("错误信息含密码：%v", err)
		}
	}
	var ue *UserError
	if !errors.As(err, &ue) || ue.Code != "28P01" {
		t.Fatalf("应是认证失败的 UserError：%v", err)
	}
}

func TestDialFailureDoesNotLeakPassword(t *testing.T) {
	fs := newFakeServer(t, nil)
	c := fs.conn("bob", "very-secret-pass-789", "db")
	fs.close() // 端口已关闭
	e, _ := New(c)
	defer e.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := e.TestConnection(ctx)
	if err == nil {
		t.Fatal("连接已关闭的端口应失败")
	}
	if strings.Contains(err.Error(), "very-secret-pass-789") {
		t.Fatalf("错误信息含密码：%v", err)
	}
	if _, qerr := e.AllDatabases(ctx); qerr == nil || strings.Contains(qerr.Error(), "very-secret-pass-789") {
		t.Fatalf("AllDatabases 的错误信息含密码或没报错：%v", qerr)
	}
}

func TestRejectUnsafeServerSettings(t *testing.T) {
	fs := newFakeServer(t, nil)
	fs.scs = "off"
	e := newEngine(t, fs, "u", "", "d")
	if err := e.TestConnection(context.Background()); err == nil || !strings.Contains(err.Error(), "standard_conforming_strings") {
		t.Fatalf("服务端报告 standard_conforming_strings=off 时应拒绝连接：%v", err)
	}
	fs2 := newFakeServer(t, nil)
	fs2.encoding = "SJIS"
	e2 := newEngine(t, fs2, "u", "", "d")
	if err := e2.TestConnection(context.Background()); err == nil || !strings.Contains(err.Error(), "client_encoding") {
		t.Fatalf("client_encoding 不是 UTF8 时应拒绝连接：%v", err)
	}
}

func TestRunUserBasic(t *testing.T) {
	fs := newFakeServer(t, routes(route{"from t", &fakeResult{
		Cols: []fakeCol{{"id", 20}, {"name", 25}, {"amount", 1700}, {"ts", 1184}, {"flag", 16}, {"doc", 3802}, {"tags", 1009}},
		Rows: [][]*string{
			cell("1", "alice", "12.50", "2024-05-06 07:08:09.123+08", "t", `{"a": 1, "b": [1, 2]}`, `{a,"b c",NULL}`),
			cell("9007199254740993", nullCell, "0.1000000000000000055511151231257827", "2024-05-06 07:08:09+05:30", "f", `[]`, `{}`),
		},
	}}))
	e := newEngine(t, fs, "u", "", "appdb")
	sh, err := e.RunUser(context.Background(), "appdb", mustCheck(t, "select * from t"), QueryLimits{MaxRows: 10, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got := sh.ColumnNames(); strings.Join(got, ",") != "id,name,amount,ts,flag,doc,tags" {
		t.Fatalf("列：%v", got)
	}
	wantTypes := []string{"int8", "text", "numeric", "timestamptz", "bool", "jsonb", "text[]"}
	for i, c := range sh.Columns {
		if c.Type != wantTypes[i] {
			t.Errorf("列 %s 类型 %q，want %q", c.Name, c.Type, wantTypes[i])
		}
	}
	r0, r1 := sh.Rows[0], sh.Rows[1]
	if r0[0] != int64(1) || r0[1] != "alice" || r0[2] != "12.50" || r0[3] != "2024-05-06T07:08:09.123+08:00" || r0[4] != true ||
		r0[5] != `{"a":1,"b":[1,2]}` || r0[6] != `["a","b c",null]` {
		t.Errorf("第一行：%#v", r0)
	}
	if r1[0] != "9007199254740993" || r1[1] != nil || r1[3] != "2024-05-06T07:08:09+05:30" || r1[4] != false || r1[5] != "[]" || r1[6] != "[]" {
		t.Errorf("第二行：%#v", r1)
	}
	if sh.Truncated {
		t.Error("不应截断")
	}
	if !strings.HasSuffix(sh.ExecutedSQL, " LIMIT 11") {
		t.Errorf("应自动补 LIMIT 11：%q", sh.ExecutedSQL)
	}
	if len(sh.Warnings) == 0 || !strings.Contains(sh.Warnings[0], "LIMIT") {
		t.Errorf("应有自动限制行数的提示：%v", sh.Warnings)
	}

	// 事务与会话设置的顺序
	stmts := fs.statements()
	idx := func(sub string) int {
		for i, s := range stmts {
			if strings.Contains(s, sub) {
				return i
			}
		}
		return -1
	}
	b, to, sp, d, f, rb := idx("BEGIN READ ONLY"), idx("SET LOCAL statement_timeout = 5000"), idx(`SET LOCAL search_path = "public", pg_catalog`),
		idx("DECLARE db_tool_cur NO SCROLL CURSOR FOR select * from t LIMIT 11"), idx("FETCH FORWARD"), idx("ROLLBACK")
	if b < 0 || to < 0 || sp < 0 || d < 0 || f < 0 || rb < 0 || !(b < to && to < sp && sp < d && d < f && f < rb) {
		t.Fatalf("语句顺序不对：%q", stmts)
	}
	if rb != len(stmts)-1 {
		t.Errorf("ROLLBACK 应是最后一条：%q", stmts)
	}
}

func TestRunUserKeepSearchPath(t *testing.T) {
	fs := newFakeServer(t, routes(route{"select 1", intRows(1)}))
	e := newEngine(t, fs, "u", "", "d")
	c, err := CheckUserQuery("select 1", CheckOptions{KeepServerSearchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunUser(context.Background(), "d", c, QueryLimits{}); err != nil {
		t.Fatal(err)
	}
	if fs.has("search_path") {
		t.Errorf("KeepServerSearchPath 时不应设置 search_path：%q", fs.statements())
	}
	c2, _ := CheckUserQuery(`select 1`, CheckOptions{DefaultSchema: `we"ird`})
	if _, err := e.RunUser(context.Background(), "d", c2, QueryLimits{}); err != nil {
		t.Fatal(err)
	}
	if !fs.has(`SET LOCAL search_path = "we""ird", pg_catalog`) {
		t.Errorf("schema 名应按标识符转义：%q", fs.statements())
	}
}

func TestRunUserRowTruncation(t *testing.T) {
	fs := newFakeServer(t, routes(route{"from t", intRows(10)}, route{"from few", intRows(3)}))
	e := newEngine(t, fs, "u", "", "d")
	// 服务端返回 10 行，上限 3 行（补的 LIMIT 是 4，但假服务端不理会，客户端仍然要截断）
	sh, err := e.RunUser(context.Background(), "d", mustCheck(t, "select * from t"), QueryLimits{MaxRows: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(sh.Rows) != 3 || !sh.Truncated || sh.TruncatedBy != TruncRows {
		t.Fatalf("rows=%d truncated=%v by=%q", len(sh.Rows), sh.Truncated, sh.TruncatedBy)
	}
	if !strings.Contains(strings.Join(sh.Warnings, "|"), "截断") {
		t.Errorf("应有截断提示：%v", sh.Warnings)
	}
	// 恰好等于上限：不算截断
	sh, err = e.RunUser(context.Background(), "d", mustCheck(t, "select * from few"), QueryLimits{MaxRows: 3})
	if err != nil || len(sh.Rows) != 3 || sh.Truncated {
		t.Fatalf("恰好 3 行不应截断：%v %v %v", err, len(sh.Rows), sh.Truncated)
	}
	// 用户自己写的 LIMIT 不被改写
	sh, _ = e.RunUser(context.Background(), "d", mustCheck(t, "select * from few limit 100"), QueryLimits{MaxRows: 3})
	if strings.Contains(sh.ExecutedSQL, "LIMIT 4") {
		t.Errorf("已有 LIMIT 时不应补：%q", sh.ExecutedSQL)
	}
	if len(sh.Rows) != 3 {
		t.Errorf("rows=%d", len(sh.Rows))
	}
}

func TestRunUserFetchBatches(t *testing.T) {
	fs := newFakeServer(t, routes(route{"from big", intRows(1300)}))
	e := newEngine(t, fs, "u", "", "d")
	sh, err := e.RunUser(context.Background(), "d", mustCheck(t, "select * from big"), QueryLimits{MaxRows: 1200})
	if err != nil {
		t.Fatal(err)
	}
	if len(sh.Rows) != 1200 || !sh.Truncated {
		t.Fatalf("rows=%d truncated=%v", len(sh.Rows), sh.Truncated)
	}
	fetches := 0
	for _, s := range fs.statements() {
		if strings.HasPrefix(s, "FETCH FORWARD") {
			fetches++
		}
	}
	if fetches != 3 { // 500 + 500 + 201
		t.Errorf("FETCH 次数 = %d，want 3：%q", fetches, fs.statements())
	}
}

func TestRunUserByteLimitAndCellCut(t *testing.T) {
	big := strings.Repeat("x", 100<<10)
	res := &fakeResult{Cols: []fakeCol{{"v", 25}}}
	for i := 0; i < 50; i++ {
		res.Rows = append(res.Rows, cell(big))
	}
	fs := newFakeServer(t, routes(route{"from blobs", res}))
	e := newEngine(t, fs, "u", "", "d")
	sh, err := e.RunUser(context.Background(), "d", mustCheck(t, "select * from blobs"), QueryLimits{MaxRows: 100, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if !sh.Truncated || sh.TruncatedBy != TruncBytes {
		t.Fatalf("应按字节上限截断：%v %q", sh.Truncated, sh.TruncatedBy)
	}
	// 每个单元格先被截成 64KB（+ 提示），1MB 上限里放得下约 15 行
	if len(sh.Rows) < 10 || len(sh.Rows) > 17 {
		t.Errorf("rows=%d", len(sh.Rows))
	}
	v := sh.Rows[0][0].(string)
	if len(v) > MaxCellBytes+200 || !strings.Contains(v, "已截断，共 102400 字节") {
		t.Errorf("单元格应被截断并注明总长：len=%d tail=%q", len(v), v[len(v)-40:])
	}
	if sh.CutCells == 0 {
		t.Error("CutCells 应大于 0")
	}
}

func TestRunUserBytea(t *testing.T) {
	small := `\x` + strings.Repeat("ab", 10)
	huge := `\x` + strings.Repeat("cd", 100<<10) // 100KB
	fs := newFakeServer(t, routes(route{"from files", &fakeResult{Cols: []fakeCol{{"b", 17}}, Rows: [][]*string{cell(small), cell(huge), cell(nullCell)}}}))
	e := newEngine(t, fs, "u", "", "d")
	sh, err := e.RunUser(context.Background(), "d", mustCheck(t, "select b from files"), QueryLimits{MaxRows: 10, MaxBytes: 10 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if sh.Rows[0][0] != small {
		t.Errorf("小 bytea 应原样：%v", sh.Rows[0][0])
	}
	h := sh.Rows[1][0].(string)
	if !strings.HasPrefix(h, `\xcdcd`) || !strings.Contains(h, "已截断，共 102400 字节") || len(h) > 2+2*MaxCellBytes+100 {
		t.Errorf("大 bytea 应截断到 64KB 并注明：len=%d", len(h))
	}
	if sh.Rows[2][0] != nil {
		t.Error("NULL 应为 nil")
	}
	if sh.BinaryCells != 2 {
		t.Errorf("BinaryCells=%d", sh.BinaryCells)
	}
}

func TestRunUserServerErrors(t *testing.T) {
	fs := newFakeServer(t, routes(
		route{"nosuch", pgErr("42P01", `relation "nosuch" does not exist`)},
		route{"bad syntax", &fakeResult{Err: &pgproto3.ErrorResponse{Severity: "ERROR", Code: "42601", Message: `syntax error at or near "syntax"`, Position: 5, Hint: "try harder"}}},
		route{"denied", pgErr("42501", "permission denied for table secret")},
	))
	e := newEngine(t, fs, "u", "", "d")
	cases := []struct{ sql, code, sub string }{
		{"select * from nosuch", "42P01", "does not exist"},
		{"select bad syntax", "42601", "位置 5"},
		{"select * from denied", "42501", "permission denied"},
	}
	for _, c := range cases {
		_, err := e.RunUser(context.Background(), "d", mustCheck(t, c.sql), QueryLimits{})
		var ue *UserError
		if !errors.As(err, &ue) || ue.Code != c.code || !strings.Contains(ue.Msg, c.sub) {
			t.Errorf("%q：%v", c.sql, err)
		}
	}
	// 出错后连接仍可复用（事务已回滚）
	if _, err := e.RunUser(context.Background(), "d", mustCheck(t, "select 1"), QueryLimits{}); err != nil {
		t.Fatal(err)
	}
	if n := fs.conns.Load(); n != 1 {
		t.Errorf("服务端 SQL 错误后应复用同一条连接，实际建了 %d 条", n)
	}
}

func TestRunUserTimeout(t *testing.T) {
	fs := newFakeServer(t, routes(route{"slow", &fakeResult{Cols: []fakeCol{{"a", 23}}, Rows: [][]*string{cell("1")}, Delay: 5 * time.Second}}))
	e := newEngine(t, fs, "u", "", "d")
	start := time.Now()
	_, err := e.RunUser(context.Background(), "d", mustCheck(t, "select * from slow"), QueryLimits{Timeout: 300 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) || !IsTimeout(err) {
		t.Fatalf("应返回 ErrTimeout：%v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("超时应在 1 秒内返回，用了 %v", d)
	}
	if !fs.has("SET LOCAL statement_timeout = 300") {
		t.Errorf("应设置 statement_timeout=300：%q", fs.statements())
	}
	// 服务端报超时后连接仍可复用
	if _, err := e.RunUser(context.Background(), "d", mustCheck(t, "select 1"), QueryLimits{}); err != nil {
		t.Fatal(err)
	}
	if n := fs.conns.Load(); n != 1 {
		t.Errorf("服务端超时后应复用同一条连接，实际建了 %d 条", n)
	}
}

func TestRunUserCancel(t *testing.T) {
	fs := newFakeServer(t, routes(route{"slow", &fakeResult{Cols: []fakeCol{{"a", 23}}, Rows: [][]*string{cell("1")}, Delay: 10 * time.Second}}))
	e := newEngine(t, fs, "u", "", "d")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := e.RunUser(ctx, "d", mustCheck(t, "select * from slow"), QueryLimits{Timeout: time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled：%v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("取消应很快返回，用了 %v", d)
	}
	// 向服务端发了 CancelRequest
	deadline := time.Now().Add(2 * time.Second)
	for fs.cancels.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if fs.cancels.Load() == 0 {
		t.Error("取消时应向服务端发送 CancelRequest")
	}
	// 引擎仍可用
	if _, err := e.RunUser(context.Background(), "d", mustCheck(t, "select 1"), QueryLimits{}); err != nil {
		t.Fatalf("取消后引擎应仍可用：%v", err)
	}
}

func TestRunUserRevalidates(t *testing.T) {
	fs := newFakeServer(t, nil)
	e := newEngine(t, fs, "u", "", "d")
	for _, sql := range []string{"insert into t values (1)", "select pg_sleep(1)", "select 1; select 2", "drop table t", "select * from pg_authid", ""} {
		if _, err := e.RunUser(context.Background(), "d", &Checked{Kind: KindSelect, SQL: sql}, QueryLimits{}); err == nil {
			t.Errorf("手工构造的 Checked(%q) 应被拒绝", sql)
		}
	}
	if _, err := e.RunUser(context.Background(), "d", nil, QueryLimits{}); err == nil {
		t.Error("nil 应被拒绝")
	}
	if n := fs.conns.Load(); n != 0 {
		t.Errorf("被拒绝的语句不应建立连接，实际 %d", n)
	}
}

func TestReadOnlyTransactionEnforced(t *testing.T) {
	// 即使静态检查被绕过，假服务端也会像真实服务端一样在 READ ONLY 事务里拒绝写操作。
	fs := newFakeServer(t, nil)
	e := newEngine(t, fs, "u", "", "d")
	err := e.readTx(context.Background(), "d", time.Second, nil, func(s *session) error {
		_, err := s.exec(context.Background(), "INSERT INTO t VALUES (1)")
		return err
	})
	if err == nil || !IsReadOnlyViolation(err) {
		t.Fatalf("只读事务里的写操作应被拒绝：%v", err)
	}
	// 之后连接已回滚，仍可使用
	if _, err := e.ServerVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := fs.conns.Load(); n != 1 {
		t.Errorf("conns=%d", n)
	}
}

func TestExplainAndShowDirect(t *testing.T) {
	fs := newFakeServer(t, routes(
		route{"explain", &fakeResult{Cols: textCols("QUERY PLAN"), Rows: [][]*string{cell("Seq Scan on t  (cost=0.00..1.00 rows=1 width=4)")}}},
		route{"show all", &fakeResult{Cols: textCols("name", "setting", "description"), Rows: [][]*string{cell("a", "1", "x"), cell("b", "2", "y"), cell("c", "3", "z")}}},
	))
	e := newEngine(t, fs, "u", "", "d")
	sh, err := e.RunUser(context.Background(), "d", mustCheck(t, "explain select * from t"), QueryLimits{})
	if err != nil || len(sh.Rows) != 1 || sh.Columns[0].Name != "QUERY PLAN" {
		t.Fatalf("%v %+v", err, sh)
	}
	if fs.has("DECLARE") {
		t.Error("EXPLAIN 不应走游标")
	}
	sh, err = e.RunUser(context.Background(), "d", mustCheck(t, "show all"), QueryLimits{MaxRows: 2})
	if err != nil || len(sh.Rows) != 2 || !sh.Truncated {
		t.Fatalf("SHOW 应按行数上限截断：%v %+v", err, sh)
	}
}

func TestUnknownTypeNamesResolved(t *testing.T) {
	fs := newFakeServer(t, routes(
		route{"pg_catalog.pg_type", &fakeResult{Cols: textCols("oid", "format_type"), Rows: [][]*string{cell("99999", "citext")}}},
		route{"from ext", &fakeResult{Cols: []fakeCol{{"c", 99999}, {"d", 88888}}, Rows: [][]*string{cell("Hello", "(1,2)")}}},
	))
	e := newEngine(t, fs, "u", "", "d")
	sh, err := e.RunUser(context.Background(), "d", mustCheck(t, "select * from ext"), QueryLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if sh.Columns[0].Type != "citext" || sh.Columns[1].Type != "oid:88888" {
		t.Errorf("%+v", sh.Columns)
	}
	if sh.Rows[0][0] != "Hello" || sh.Rows[0][1] != "(1,2)" {
		t.Errorf("未知类型应原样文本：%v", sh.Rows[0])
	}
}

func TestHugeRowAborted(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	huge := strings.Repeat("y", maxMessageBytes+1<<20)
	fs := newFakeServer(t, routes(route{"from huge", &fakeResult{Cols: textCols("v"), Rows: [][]*string{cell(huge)}}}))
	e := newEngine(t, fs, "u", "", "d")
	_, err := e.RunUser(context.Background(), "d", mustCheck(t, "select * from huge"), QueryLimits{})
	if err == nil || !strings.Contains(err.Error(), "单行结果超过") {
		t.Fatalf("超大单行应中止：%v", err)
	}
	// 连接已作废，下一次查询重新建连
	if _, err := e.RunUser(context.Background(), "d", mustCheck(t, "select 1"), QueryLimits{}); err != nil {
		t.Fatal(err)
	}
}

func TestPoolReuseAndConcurrency(t *testing.T) {
	fs := newFakeServer(t, routes(route{"from t", func() *fakeResult { r := intRows(5); r.Delay = 30 * time.Millisecond; return r }()}))
	e := newEngine(t, fs, "u", "", "d")
	for i := 0; i < 5; i++ {
		if _, err := e.RunUser(context.Background(), "d", mustCheck(t, "select * from t"), QueryLimits{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := fs.conns.Load(); n != 1 {
		t.Fatalf("串行查询应复用一条连接，实际 %d", n)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sh, err := e.RunUser(context.Background(), "d", mustCheck(t, "select * from t"), QueryLimits{})
			if err == nil && len(sh.Rows) != 5 {
				err = fmt.Errorf("rows=%d", len(sh.Rows))
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := fs.conns.Load(); n > int32(maxConnsPerDB) {
		t.Errorf("同一个库的连接数应不超过 %d，实际累计 %d", maxConnsPerDB, n)
	}
	if n := fs.open.Load(); n > int32(maxConnsPerDB) {
		t.Errorf("同时打开 %d 条连接", n)
	}
}

func TestPerDatabaseConnections(t *testing.T) {
	fs := newFakeServer(t, routes(route{"select 1", intRows(1)}))
	e := newEngine(t, fs, "u", "", "defaultdb")
	for _, db := range []string{"a", "b", "a", "", "defaultdb"} {
		if _, err := e.RunUser(context.Background(), db, mustCheck(t, "select 1"), QueryLimits{}); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]bool{}
	for _, p := range fs.startupParams() {
		got[p["database"]] = true
	}
	if len(got) != 3 || !got["a"] || !got["b"] || !got["defaultdb"] || fs.conns.Load() != 3 {
		t.Errorf("databases=%v conns=%d", got, fs.conns.Load())
	}
}

func TestPoolEvictionLimit(t *testing.T) {
	fs := newFakeServer(t, routes(route{"select 1", intRows(1)}))
	e := newEngine(t, fs, "u", "", "d0")
	for i := 0; i < maxDBPools+4; i++ {
		if _, err := e.RunUser(context.Background(), fmt.Sprintf("db%d", i), mustCheck(t, "select 1"), QueryLimits{}); err != nil {
			t.Fatal(err)
		}
	}
	e.mu.Lock()
	n := len(e.pools)
	e.mu.Unlock()
	if n > maxDBPools {
		t.Errorf("缓存的库数 %d 超过上限 %d", n, maxDBPools)
	}
}

func TestStaleIdleConnectionRetried(t *testing.T) {
	fs := newFakeServer(t, routes(route{"select 1", intRows(1)}))
	e := newEngine(t, fs, "u", "", "d")
	if _, err := e.RunUser(context.Background(), "d", mustCheck(t, "select 1"), QueryLimits{}); err != nil {
		t.Fatal(err)
	}
	// 服务端把空闲连接全部断开
	fs.mu.Lock()
	for _, c := range fs.live {
		_ = c.nc.Close()
	}
	fs.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	if _, err := e.RunUser(context.Background(), "d", mustCheck(t, "select 1"), QueryLimits{}); err != nil {
		t.Fatalf("空闲连接被服务端断开后应换新连接重试：%v", err)
	}
	if fs.conns.Load() != 2 {
		t.Errorf("conns=%d", fs.conns.Load())
	}
}

func TestEngineQueryInterface(t *testing.T) {
	fs := newFakeServer(t, routes(route{"from t", intRows(10)}))
	e := newEngine(t, fs, "u", "", "d")
	var eng engine.Engine = e
	rs, err := eng.Query(context.Background(), "select * from t", engine.QueryOptions{DBName: "d", Limit: 4, MaxExecMs: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Rows) != 4 || len(rs.ColumnList) != 2 || rs.ColumnList[0] != "id" {
		t.Fatalf("%+v", rs)
	}
	if !fs.has("SET LOCAL statement_timeout = 5000") {
		t.Errorf("MaxExecMs 应转成 statement_timeout：%q", fs.statements())
	}
	rs, err = eng.Query(context.Background(), "select * from t", engine.QueryOptions{DBName: "d"})
	if err != nil || len(rs.Rows) != 10 {
		t.Fatalf("Limit<=0 应返回全部行：%v %+v", err, rs)
	}
	if _, err := eng.Query(context.Background(), "delete from t", engine.QueryOptions{DBName: "d"}); err == nil {
		t.Fatal("Query 不应执行写语句")
	}
	if _, err := eng.Query(context.Background(), "select pg_sleep(1)", engine.QueryOptions{DBName: "d"}); err == nil {
		t.Fatal("Query 不应执行危险函数")
	}
}

func TestCloseIdempotent(t *testing.T) {
	fs := newFakeServer(t, routes(route{"select 1", intRows(1)}))
	e, _ := New(fs.conn("u", "", "d"))
	if _, err := e.RunUser(context.Background(), "d", mustCheck(t, "select 1"), QueryLimits{}); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fs.open.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if fs.open.Load() != 0 {
		t.Errorf("Close 后应关闭所有空闲连接，仍有 %d 条", fs.open.Load())
	}
	if _, err := e.RunUser(context.Background(), "d", mustCheck(t, "select 1"), QueryLimits{}); err == nil {
		t.Error("Close 后不应再能查询")
	}
}

func TestContextAlreadyDone(t *testing.T) {
	fs := newFakeServer(t, nil)
	e := newEngine(t, fs, "u", "", "d")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.RunUser(ctx, "d", mustCheck(t, "select 1"), QueryLimits{}); err == nil {
		t.Fatal("ctx 已取消应报错")
	}
}

func TestQueryLimitsNormalize(t *testing.T) {
	l := QueryLimits{}.normalize()
	if l.MaxRows != DefaultMaxRows || l.MaxBytes != DefaultMaxBytes || l.Timeout != DefaultTimeout {
		t.Fatalf("%+v", l)
	}
	l = QueryLimits{MaxRows: 1 << 40, MaxBytes: 1 << 40, Timeout: 100 * time.Hour}.normalize()
	if l.MaxRows != HardMaxRows || l.MaxBytes != HardMaxBytes || l.Timeout != HardTimeout {
		t.Fatalf("%+v", l)
	}
}
