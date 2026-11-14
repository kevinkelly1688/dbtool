// @author: kevinkelly
// @license: Apache Licence
// @file: engine_test.go
// @time: 2026/10/28

package clickhouse

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dbtool/internal/engine"
)

func mustCheck(t *testing.T, q, db string) *Checked {
	t.Helper()
	ck, err := CheckUserQuery(q, CheckOptions{DB: db})
	if err != nil {
		t.Fatalf("检查 %q: %v", q, err)
	}
	return ck
}

func TestRegistered(t *testing.T) {
	f := newFake(t)
	e, err := engine.New("clickhouse", f.conn())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.Name() != "clickhouse" {
		t.Errorf("Name = %q", e.Name())
	}
	if _, ok := e.(*Engine); !ok {
		t.Errorf("engine.New 返回 %T, 上层需要能断言成 *clickhouse.Engine", e)
	}
	if _, err := NewEngine(engine.Conn{}); err == nil {
		t.Error("空地址应报错")
	}
}

func TestRequestParams(t *testing.T) {
	f := newFake(t)
	e := f.engine()
	ck := mustCheck(t, "SELECT x FROM t FORMAT JSON;", "app")
	lim := QueryLimits{MaxRows: 50, MaxBytes: 1 << 20, Timeout: 7 * time.Second, MaxRowsToRead: 1234, MaxBytesToRead: 5678, MaxMemory: 1 << 30, MaxThreads: 3}
	if _, err := e.RunUser(context.Background(), ck, lim); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 2 { // 引擎核对 + 主查询
		t.Fatalf("请求数 = %d", len(reqs))
	}
	main := reqs[1]
	if main.Body != "SELECT x FROM t" {
		t.Errorf("body = %q（FORMAT 与分号应被去掉）", main.Body)
	}
	if main.Method != http.MethodPost {
		t.Errorf("method = %s", main.Method)
	}
	want := map[string]string{
		"database": "app", "readonly": "1", "max_execution_time": "7", "timeout_overflow_mode": "throw",
		"max_result_rows": "51", "max_result_bytes": strconv.Itoa(1 << 20), "result_overflow_mode": "break",
		"max_rows_to_read": "1234", "max_bytes_to_read": "5678", "read_overflow_mode": "throw",
		"max_memory_usage": strconv.Itoa(1 << 30), "max_threads": "3", "default_format": "JSONCompact", "wait_end_of_query": "1",
		"cancel_http_readonly_queries_on_client_close": "1", "output_format_json_quote_64bit_integers": "1",
		"output_format_json_quote_decimals": "1",
	}
	for k, v := range want {
		if got := main.Query.Get(k); got != v {
			t.Errorf("参数 %s = %q, want %q", k, got, v)
		}
	}
	if !strings.HasPrefix(main.Query.Get("query_id"), "dbtool-") {
		t.Errorf("query_id = %q", main.Query.Get("query_id"))
	}
	// 认证：HTTP Basic，密码不在 URL / 其它头里
	auth := main.Header.Get("Authorization")
	dec, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	if string(dec) != "reader:s3cr3t-Pa55" {
		t.Errorf("Authorization 解码 = %q", dec)
	}
	for _, r := range reqs {
		if strings.Contains(r.Query.Encode(), "s3cr3t") {
			t.Error("密码出现在 URL 里")
		}
		if r.Query.Get("readonly") == "" || r.Query.Get("max_execution_time") == "" || r.Query.Get("max_result_rows") == "" || r.Query.Get("max_rows_to_read") == "" {
			t.Errorf("引擎核对请求也要带 readonly / 超时 / 行数限制: %v", r.Query)
		}
	}
	// 核对请求本身
	if !strings.Contains(reqs[0].Body, "system.tables") || !strings.Contains(reqs[0].Body, "('app', 't')") {
		t.Errorf("引擎核对语句 = %q", reqs[0].Body)
	}
}

func TestRequestDefaultsAndHardCaps(t *testing.T) {
	f := newFake(t)
	e := f.engine()
	ck := mustCheck(t, "SELECT 1", "")
	if _, err := e.RunUser(context.Background(), ck, QueryLimits{MaxRows: 99_999_999, MaxBytes: 1 << 40, Timeout: time.Hour, MaxMemory: 1 << 50, MaxThreads: 1000}); err != nil {
		t.Fatal(err)
	}
	q := f.requests()[0].Query
	if q.Get("max_result_rows") != strconv.Itoa(HardMaxRows+1) || q.Get("max_result_bytes") != strconv.Itoa(HardMaxBytes) ||
		q.Get("max_execution_time") != "600" || q.Get("max_memory_usage") != strconv.Itoa(HardMaxMemory) || q.Get("max_threads") != "16" {
		t.Errorf("硬上限没生效: %v", q)
	}
	if q.Get("database") != "app" { // 来自连接配置
		t.Errorf("database = %q", q.Get("database"))
	}
	f2 := newFake(t)
	e2 := f2.engine()
	if _, err := e2.RunUser(context.Background(), mustCheck(t, "SELECT 1", "other"), QueryLimits{}); err != nil {
		t.Fatal(err)
	}
	q = f2.requests()[0].Query
	if q.Get("max_result_rows") != "1001" || q.Get("max_execution_time") != "30" || q.Get("max_rows_to_read") != strconv.Itoa(DefaultMaxRowsToRead) ||
		q.Get("max_bytes_to_read") != strconv.FormatInt(DefaultMaxBytesToRead, 10) || q.Get("max_threads") != "4" || q.Get("database") != "other" {
		t.Errorf("默认值不对: %v", q)
	}
}

func TestReadonlyLevel(t *testing.T) {
	f := newFake(t)
	c := f.conn()
	e, _ := NewWithConfig(Config{Host: c.Host, Port: c.Port, User: c.User, Password: c.Password, Readonly: 2})
	defer e.Close()
	if _, err := e.RunUser(context.Background(), mustCheck(t, "SELECT 1", ""), QueryLimits{}); err != nil {
		t.Fatal(err)
	}
	if got := f.requests()[0].Query.Get("readonly"); got != "2" {
		t.Errorf("readonly = %s", got)
	}
	f2 := newFake(t)
	c = f2.conn()
	e2, _ := NewWithConfig(Config{Host: c.Host, Port: c.Port, Readonly: 7}) // 不合法的值回落到 1
	defer e2.Close()
	if _, err := e2.RunUser(context.Background(), mustCheck(t, "SELECT 1", ""), QueryLimits{}); err != nil {
		t.Fatal(err)
	}
	r := f2.requests()[0]
	if r.Query.Get("readonly") != "1" {
		t.Errorf("readonly = %s", r.Query.Get("readonly"))
	}
	// 用户名为空用 default
	dec, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(r.Header.Get("Authorization"), "Basic "))
	if string(dec) != "default:" {
		t.Errorf("auth = %q", dec)
	}
}

func TestStaticRejectionNeverReachesServer(t *testing.T) {
	f := newFake(t)
	e := f.engine()
	for _, q := range []string{"SELECT 1 SETTINGS readonly = 0", "SELECT 1 FORMAT CSV SETTINGS x=1", "INSERT INTO t VALUES (1)", "SELECT sleep(10)",
		"SELECT * FROM file('/etc/passwd', 'CSV', 'a String')", "SELECT * FROM system.users", "SELECT 1; SELECT 2"} {
		_, err := e.Query(context.Background(), q, engine.QueryOptions{DBName: "app"})
		var ue *UserError
		if !errors.As(err, &ue) {
			t.Errorf("%q: err = %v", q, err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("被静态检查拒绝的语句不应到达服务端，收到 %d 个请求", n)
	}
}

func TestShapeTypes(t *testing.T) {
	f := newFake(t)
	cols := [][2]string{
		{"i8", "Int8"}, {"u32", "UInt32"}, {"i64", "Int64"}, {"u64", "UInt64"}, {"u256", "UInt256"}, {"dec", "Decimal(18, 4)"},
		{"f", "Float64"}, {"f32n", "Nullable(Float32)"}, {"nul", "Nullable(String)"}, {"s", "LowCardinality(String)"}, {"b", "Bool"},
		{"d", "Date"}, {"dt", "DateTime"}, {"dtz", "DateTime('Asia/Shanghai')"}, {"dt64", "DateTime64(3, 'UTC')"}, {"dt64n", "Nullable(DateTime64(6))"},
		{"arr", "Array(Int32)"}, {"map", "Map(String, UInt64)"}, {"tup", "Tuple(Int8, String)"}, {"uuid", "UUID"}, {"ip", "IPv6"}, {"en", "Enum8('a' = 1)"},
		{"nan", "Float64"}, {"obj", "Nested(a UInt8)"},
	}
	row := []any{
		raw("-5"), raw("4000000000"), "9223372036854775807", "18446744073709551615", "115792089237316195423570985008687907853269984665640564039457584007913129639935", "12345.6789",
		raw("1.5"), raw("2.25"), nil, "hello", raw("true"),
		"2024-01-02", "2024-01-02 03:04:05", "2024-01-02 03:04:05", "2024-01-02 03:04:05.123", "2024-01-02 03:04:05.123456",
		raw(`[1, 2,3]`), raw(`{"a": "1", "b":"18446744073709551615"}`), raw(`[1, "x"]`), "61f0c404-5cb3-11e7-907b-a6006ad3dba0", "::1", "a",
		"nan", raw(`[[1],[2]]`),
	}
	f.onJC("SELECT types", cols, [][]any{row})
	e := f.engine()
	sh, err := e.RunUser(context.Background(), mustCheck(t, "SELECT types", ""), QueryLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sh.Rows) != 1 || len(sh.ColumnList) != len(cols) || sh.ColumnTypes[0] != "Int8" {
		t.Fatalf("结果形状不对: %+v", sh)
	}
	got := sh.Rows[0]
	want := []any{
		int64(-5), int64(4000000000), "9223372036854775807", "18446744073709551615", "115792089237316195423570985008687907853269984665640564039457584007913129639935", "12345.6789",
		1.5, 2.25, nil, "hello", true,
		"2024-01-02", "2024-01-02T03:04:05", "2024-01-02T03:04:05+08:00", "2024-01-02T03:04:05.123Z", "2024-01-02T03:04:05.123456",
		"[1,2,3]", `{"a":"1","b":"18446744073709551615"}`, `[1,"x"]`, "61f0c404-5cb3-11e7-907b-a6006ad3dba0", "::1", "a",
		"nan", "[[1],[2]]",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("列 %s (%s): %#v, want %#v", cols[i][0], cols[i][1], got[i], want[i])
		}
	}
	if sh.RowsRead != 10 || sh.BytesRead != 4096 {
		t.Errorf("statistics: %d/%d", sh.RowsRead, sh.BytesRead)
	}
	rs := sh.ToResultSet()
	if len(rs.Rows) != 1 || rs.ColumnList[0] != "i8" {
		t.Errorf("ToResultSet: %+v", rs)
	}
}

func TestShapeBinaryAndLongCells(t *testing.T) {
	f := newFake(t)
	long := strings.Repeat("中", 40000) // 120000 字节
	f.onJC("SELECT bin", [][2]string{{"bin", "String"}, {"fx", "FixedString(4)"}, {"long", "String"}, {"arr", "Array(String)"}, {"bad", "String"}},
		[][]any{{"a\x00b\x01", "\u0001\u0002AB", long, raw(`["` + strings.Repeat("x", 70000) + `"]`), "ok�bad"}})
	e := f.engine()
	sh, err := e.RunUser(context.Background(), mustCheck(t, "SELECT bin", ""), QueryLimits{})
	if err != nil {
		t.Fatal(err)
	}
	r := sh.Rows[0]
	if r[0] != `\x61\x00\x62\x01` || r[1] != `\x01\x02\x41\x42` {
		t.Errorf("二进制: %#v %#v", r[0], r[1])
	}
	s2 := r[2].(string)
	if !strings.Contains(s2, "已截断，原长 120000 字节") || len(s2) > DefaultMaxCellBytes+100 || !strings.HasPrefix(s2, "中中") {
		t.Errorf("长字符串没有截断或截断位置不在字符边界: len=%d", len(s2))
	}
	for _, c := range s2[:len(s2)-len("…（已截断，原长 120000 字节）")] {
		if c != '中' {
			t.Fatalf("截断后出现非法字符 %q", c)
		}
	}
	if s3 := r[3].(string); !strings.Contains(s3, "已截断") {
		t.Errorf("长数组没有截断")
	}
	joined := strings.Join(sh.Warnings, "\n")
	for _, w := range []string{"含二进制内容", "已截断", "U+FFFD"} {
		if !strings.Contains(joined, w) {
			t.Errorf("警告里缺少 %q: %v", w, sh.Warnings)
		}
	}
	// 自定义单元格上限
	sh, err = e.RunUser(context.Background(), mustCheck(t, "SELECT bin", ""), QueryLimits{MaxCellBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if s := sh.Rows[0][2].(string); !strings.HasPrefix(s, "中中中") || strings.HasPrefix(s, "中中中中") {
		t.Errorf("MaxCellBytes=10: %q", s)
	}
}

func manyRows(n int) [][]any {
	rows := make([][]any, n)
	for i := range rows {
		rows[i] = []any{i, "row-" + strconv.Itoa(i)}
	}
	return rows
}

func TestTruncationByRows(t *testing.T) {
	f := newFake(t)
	cols := [][2]string{{"n", "UInt32"}, {"s", "String"}}
	f.onJC("SELECT five", cols, manyRows(5))
	f.onJC("SELECT three", cols, manyRows(3))
	f.onJC("SELECT four", cols, manyRows(4)) // max_result_rows=limit+1 → 服务端多给 1 行
	e := f.engine()
	sh, err := e.RunUser(context.Background(), mustCheck(t, "SELECT five", ""), QueryLimits{MaxRows: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(sh.Rows) != 3 || !sh.Truncated || sh.TruncatedBy != "rows" || len(sh.Warnings) != 1 || !strings.Contains(sh.Warnings[0], "3 行") {
		t.Errorf("rows=%d truncated=%v by=%q warnings=%v", len(sh.Rows), sh.Truncated, sh.TruncatedBy, sh.Warnings)
	}
	sh, err = e.RunUser(context.Background(), mustCheck(t, "SELECT four", ""), QueryLimits{MaxRows: 3})
	if err != nil || len(sh.Rows) != 3 || !sh.Truncated {
		t.Errorf("limit+1 行应判为截断: %v %+v", err, sh)
	}
	sh, err = e.RunUser(context.Background(), mustCheck(t, "SELECT three", ""), QueryLimits{MaxRows: 3})
	if err != nil || len(sh.Rows) != 3 || sh.Truncated || len(sh.Warnings) != 0 {
		t.Errorf("刚好 limit 行不应截断: %v %+v", err, sh)
	}
}

func TestTruncationByBytes(t *testing.T) {
	f := newFake(t)
	rows := make([][]any, 100)
	for i := range rows {
		rows[i] = []any{strings.Repeat("a", 1000)}
	}
	f.onJC("SELECT big", [][2]string{{"s", "String"}}, rows)
	e := f.engine()
	sh, err := e.RunUser(context.Background(), mustCheck(t, "SELECT big", ""), QueryLimits{MaxBytes: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	if !sh.Truncated || sh.TruncatedBy != "bytes" || len(sh.Rows) < 1 || len(sh.Rows) > 11 {
		t.Errorf("rows=%d truncated=%v by=%q", len(sh.Rows), sh.Truncated, sh.TruncatedBy)
	}
	// 第一行本身超过字节上限也要返回（至少一行），而不是空结果
	sh, err = e.RunUser(context.Background(), mustCheck(t, "SELECT big", ""), QueryLimits{MaxBytes: 10})
	if err != nil || len(sh.Rows) != 1 || !sh.Truncated {
		t.Errorf("err=%v rows=%d", err, len(sh.Rows))
	}
}

// 服务端无视 max_result_* 疯狂输出：客户端按读取上限截断并停止读取。
func TestHugeResponseIsCapped(t *testing.T) {
	f := newFake(t)
	var written atomic.Int64
	f.on(contains("SELECT huge"), func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"meta":[{"name":"s","type":"String"}],"data":[`)
		chunk := `["` + strings.Repeat("x", 100) + `"],`
		for i := 0; i < 5_000_000; i++ { // 约 500MB，远超上限
			n, err := io.WriteString(w, chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	})
	e := f.engine()
	start := time.Now()
	sh, err := e.RunUser(context.Background(), mustCheck(t, "SELECT huge", ""), QueryLimits{MaxRows: 1_000_000, MaxBytes: 100_000})
	if err != nil {
		t.Fatal(err)
	}
	if !sh.Truncated || (sh.TruncatedBy != "bytes" && sh.TruncatedBy != "response") {
		t.Errorf("truncated=%v by=%q rows=%d", sh.Truncated, sh.TruncatedBy, len(sh.Rows))
	}
	if len(sh.Rows) > 2000 {
		t.Errorf("返回了 %d 行，远超字节上限", len(sh.Rows))
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("耗时 %v：应当尽早停止读取", time.Since(start))
	}
}

// 响应在读取上限处被截断（半个 JSON）：保留已收到的整行，标记 response 截断。
func TestResponseCapMidStream(t *testing.T) {
	var cr capReader
	body := `{"meta":[{"name":"n","type":"UInt8"}],"data":[[1],[2],[3],[4],[5]],"rows":5}`
	cr = capReader{r: strings.NewReader(body), left: 55}
	p, err := parseJSONCompact(&cr, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !p.truncated || p.by != "response" || len(p.rows) < 1 || len(p.rows) >= 5 {
		t.Errorf("p = %+v", p)
	}
	// 连 meta 都没读完：报错而不是返回空结果
	cr = capReader{r: strings.NewReader(body), left: 20}
	if _, err := parseJSONCompact(&cr, 100, 1<<20); err == nil {
		t.Error("应报错")
	}
}

func TestServerErrors(t *testing.T) {
	cases := []struct {
		code int
		msg  string
		want string
	}{
		{60, "Table app.nope does not exist. (UNKNOWN_TABLE)", "表不存在"},
		{81, "Database nope does not exist. (UNKNOWN_DATABASE)", "库不存在"},
		{47, "Unknown identifier 'zz' (UNKNOWN_IDENTIFIER)", "未知的列"},
		{62, "Syntax error: failed at position 8. (SYNTAX_ERROR)", "语法错误"},
		{516, "reader: Authentication failed: password is incorrect, or there is no user with such name. (AUTHENTICATION_FAILED)", "认证失败"},
		{497, "reader: Not enough privileges. (ACCESS_DENIED)", "没有权限"},
		{159, "Timeout exceeded: elapsed 30.001 seconds, maximum: 30. (TIMEOUT_EXCEEDED)", "查询超时"},
		{158, "Limit for rows (controlled by 'max_rows_to_read' setting) exceeded. (TOO_MANY_ROWS)", "读取行数"},
		{241, "Memory limit (for query) exceeded. (MEMORY_LIMIT_EXCEEDED)", "内存"},
		{164, "Cannot modify 'max_execution_time' setting in readonly mode. (READONLY)", "readonly=2"},
		{999, "Something odd", "服务端错误"},
	}
	for _, c := range cases {
		f := newFake(t)
		f.on(contains("SELECT err"), chError(c.code, c.msg))
		e := f.engine()
		_, err := e.RunUser(context.Background(), mustCheck(t, "SELECT err", ""), QueryLimits{})
		var se *ServerError
		if !errors.As(err, &se) {
			t.Errorf("code %d: err = %v", c.code, err)
			continue
		}
		if se.Code != c.code || se.Status != 500 || !strings.Contains(err.Error(), c.want) {
			t.Errorf("code %d: %+v / %v", c.code, se, err)
		}
		if strings.Contains(err.Error(), "version 24") {
			t.Errorf("版本尾巴应被去掉: %v", err)
		}
		if got := errors.Is(err, ErrTimeout); got != (c.code == 159) {
			t.Errorf("code %d: Is(ErrTimeout) = %v", c.code, got)
		}
	}
}

func TestErrorsDoNotLeakCredentials(t *testing.T) {
	f := newFake(t)
	// 服务端把密码回显在错误里（有的版本会在语法错误里带出语句片段）
	f.on(contains("SELECT leak"), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-ClickHouse-Exception-Code", "62")
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "Code: 62. DB::Exception: Syntax error near IDENTIFIED BY 'hunter22' password='s3cr3t-Pa55' url=http://reader:s3cr3t-Pa55@host/x Authorization: Basic cmVhZGVyOnMzY3IzdA==")
	})
	e := f.engine()
	_, err := e.RunUser(context.Background(), mustCheck(t, "SELECT leak", ""), QueryLimits{})
	if err == nil {
		t.Fatal("应报错")
	}
	for _, bad := range []string{"s3cr3t", "hunter22", "cmVhZGVy", "Pa55"} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("错误信息泄露了 %q: %v", bad, err)
		}
	}
	// 连接失败：地址不含密码也不含 URL 查询串
	c := f.conn()
	f.srv.Close()
	e2, _ := NewEngine(c)
	_, err = e2.RunUser(context.Background(), mustCheck(t, "SELECT 1", ""), QueryLimits{})
	if err == nil {
		t.Fatal("应报错")
	}
	msg := err.Error()
	if strings.Contains(msg, "s3cr3t") || strings.Contains(msg, "readonly=") || strings.Contains(msg, "?") && strings.Contains(msg, "max_") {
		t.Errorf("连接错误泄露了凭据 / URL: %v", msg)
	}
	if !strings.Contains(msg, "无法连接") {
		t.Errorf("msg = %v", msg)
	}
	if err := e2.TestConnection(context.Background()); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("TestConnection err = %v", err)
	}
}

func TestScrub(t *testing.T) {
	cases := []struct{ in, notWant string }{
		{"create user u identified by 'abc123'", "abc123"},
		{"IDENTIFIED WITH sha256_password BY 'abc123'", "abc123"},
		{"password = 'abc123'", "abc123"},
		{"PASSWORD: abc123", "abc123"},
		{"mysql('h', 'd', 't', 'u', 'p') url=http://user:abc123@host", "abc123"},
		{"Authorization: Basic YWJjOmFiYzEyMw==", "YWJjOm"},
		{"token=abc123", "abc123"},
	}
	for _, c := range cases {
		if out := scrub(c.in); strings.Contains(out, c.notWant) {
			t.Errorf("scrub(%q) = %q 仍含 %q", c.in, out, c.notWant)
		}
	}
	if out := scrub("my secret pw is hunter2 ok", "hunter2"); strings.Contains(out, "hunter2") {
		t.Error("配置的密码应被替换")
	}
	long := scrub(strings.Repeat("长", 5000))
	if n := len([]rune(long)); n != maxErrDetail+1 {
		t.Errorf("长度 = %d", n)
	}
}

func TestExceptionTextInBody(t *testing.T) {
	// 200 状态但正文是异常文本（未启用 wait_end_of_query 的旧行为 / 代理改写）
	f := newFake(t)
	f.on(contains("SELECT txt"), func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "Code: 159. DB::Exception: Timeout exceeded: elapsed 1.0 seconds. (TIMEOUT_EXCEEDED) (version 24.3)\n")
	})
	f.on(contains("SELECT jsonexc"), func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"meta":[{"name":"n","type":"UInt8"}],"data":[[1]],"exception":"Code: 241. DB::Exception: Memory limit exceeded. (MEMORY_LIMIT_EXCEEDED)"}`)
	})
	e := f.engine()
	_, err := e.RunUser(context.Background(), mustCheck(t, "SELECT txt", ""), QueryLimits{})
	var se *ServerError
	if !errors.As(err, &se) || se.Code != 159 || !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v", err)
	}
	_, err = e.RunUser(context.Background(), mustCheck(t, "SELECT jsonexc", ""), QueryLimits{})
	if !errors.As(err, &se) || se.Code != 241 {
		t.Errorf("err = %v", err)
	}
	f.on(contains("SELECT junk"), func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "<html>proxy error</html>") })
	if _, err = e.RunUser(context.Background(), mustCheck(t, "SELECT junk", ""), QueryLimits{}); err == nil || !strings.Contains(err.Error(), "无法解析") {
		t.Errorf("err = %v", err)
	}
}

func TestHTTPStatusWithoutCode(t *testing.T) {
	f := newFake(t)
	f.on(contains("SELECT gw"), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "bad gateway")
	})
	e := f.engine()
	_, err := e.RunUser(context.Background(), mustCheck(t, "SELECT gw", ""), QueryLimits{})
	var se *ServerError
	if !errors.As(err, &se) || se.Status != 502 || se.Code != 0 || !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("err = %v", err)
	}
}

func TestCancelAndTimeout(t *testing.T) {
	f := newFake(t)
	block := func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	f.on(contains("SELECT slow"), block)
	e := f.engine()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(80*time.Millisecond, cancel)
	start := time.Now()
	_, err := e.RunUser(ctx, mustCheck(t, "SELECT slow", ""), QueryLimits{})
	if !IsCanceled(err) || !errors.Is(err, ErrCanceled) || IsTimeout(err) {
		t.Errorf("取消: err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("取消耗时 %v", time.Since(start))
	}

	old := timeoutGrace
	timeoutGrace = 0
	defer func() { timeoutGrace = old }()
	start = time.Now()
	_, err = e.RunUser(context.Background(), mustCheck(t, "SELECT slow", ""), QueryLimits{Timeout: 300 * time.Millisecond})
	if !IsTimeout(err) || !errors.Is(err, ErrTimeout) || IsCanceled(err) {
		t.Errorf("超时: err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("超时耗时 %v", time.Since(start))
	}
	// 调用方自己的 deadline
	ctx, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	_, err = e.RunUser(ctx, mustCheck(t, "SELECT slow", ""), QueryLimits{})
	if !IsTimeout(err) {
		t.Errorf("deadline: err = %v", err)
	}
	// 已经取消的上下文：不发请求
	ctx, cancel3 := context.WithCancel(context.Background())
	cancel3()
	n := len(f.requests())
	if _, err = e.RunUser(ctx, mustCheck(t, "SELECT 1", ""), QueryLimits{}); !IsCanceled(err) {
		t.Errorf("err = %v", err)
	}
	if len(f.requests()) != n {
		t.Error("已取消的上下文不应发出请求")
	}
}

func TestEngineCheck(t *testing.T) {
	f := newFake(t)
	engines := map[string]string{"good": "MergeTree", "evil": "File", "ext": "MySQL", "q": "Kafka", "dist": "Distributed", "v": "View"}
	f.on(contains("FROM system.tables WHERE (database, name) IN"), func(w http.ResponseWriter, r *http.Request) {
		// 只返回语句里问到的表，和真实服务端一致
		var rows [][]any
		for name, eng := range engines {
			if f.lastBodyHas("('app', '" + name + "')") {
				rows = append(rows, []any{"app", name, eng})
			}
		}
		writeJC(w, [][2]string{{"database", "String"}, {"name", "String"}, {"engine", "String"}}, rows)
	})
	f.onJC("SELECT main", [][2]string{{"x", "UInt8"}}, [][]any{{1}})
	e := f.engine()
	run := func(q string) error {
		_, err := e.RunUser(context.Background(), mustCheck(t, q, "app"), QueryLimits{})
		return err
	}
	before := len(f.requests())
	if err := run("SELECT main FROM good JOIN dist ON 1 JOIN v ON 1"); err != nil {
		t.Errorf("正常引擎应放行: %v", err)
	}
	if n := len(f.requests()) - before; n != 2 {
		t.Errorf("请求数 = %d", n)
	}
	for _, q := range []string{"SELECT main FROM evil", "SELECT main FROM good, ext", "SELECT main FROM (SELECT * FROM q)", "SELECT main FROM good WHERE x IN evil"} {
		before = len(f.requests())
		err := run(q)
		var ue *UserError
		if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "引擎") {
			t.Errorf("%q: err = %v", q, err)
		}
		if n := len(f.requests()) - before; n != 1 {
			t.Errorf("%q: 被拒绝时只应有引擎核对这 1 个请求，实际 %d", q, n)
		}
	}
	// 没有涉及表（或只有 system 表）：不做引擎核对
	before = len(f.requests())
	if err := run("SELECT main"); err != nil {
		t.Fatal(err)
	}
	if err := run("SELECT main FROM system.tables"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests()) - before; n != 2 {
		t.Errorf("请求数 = %d", n)
	}
}

func TestEngineCheckFailsClosed(t *testing.T) {
	f := newFake(t)
	f.on(contains("FROM system.tables WHERE (database, name) IN"), chError(497, "reader: Not enough privileges. (ACCESS_DENIED)"))
	e := f.engine()
	before := len(f.requests())
	_, err := e.RunUser(context.Background(), mustCheck(t, "SELECT x FROM t", "app"), QueryLimits{})
	if err == nil || !strings.Contains(err.Error(), "无法确认") || !strings.Contains(err.Error(), "没有权限") {
		t.Errorf("err = %v", err)
	}
	if n := len(f.requests()) - before; n != 1 {
		t.Errorf("引擎核对失败后不应再发主查询，请求数 = %d", n)
	}
}

func TestEngineCheckEscapesNames(t *testing.T) {
	f := newFake(t)
	e := f.engine()
	ck := mustCheck(t, "SELECT * FROM `a'b`.`c d`", "")
	if _, err := e.RunUser(context.Background(), ck, QueryLimits{}); err != nil {
		t.Fatal(err)
	}
	if b := f.requests()[0].Body; !strings.Contains(b, `('a\'b', 'c d')`) {
		t.Errorf("核对语句没有转义: %s", b)
	}
}

func TestQueryViaEngineInterface(t *testing.T) {
	f := newFake(t)
	f.onJC("SELECT rows", [][2]string{{"n", "UInt32"}, {"s", "String"}}, manyRows(10))
	var eng engine.Engine = f.engine()
	rs, err := eng.Query(context.Background(), "SELECT rows FROM t", engine.QueryOptions{DBName: "app", Limit: 4, MaxExecMs: 9000})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Rows) != 4 || rs.ColumnList[1] != "s" {
		t.Errorf("rs = %+v", rs)
	}
	main := f.requests()[1]
	if main.Query.Get("max_result_rows") != "5" || main.Query.Get("max_execution_time") != "9" || main.Query.Get("database") != "app" {
		t.Errorf("参数没有传递: %v", main.Query)
	}
}

func TestConcurrentRunUser(t *testing.T) {
	f := newFake(t)
	f.on(contains("SELECT c"), func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
		n := strings.TrimSpace(strings.TrimPrefix(r.URL.Query().Get("query_id"), "dbtool-"))
		_ = n
		writeJC(w, [][2]string{{"n", "UInt32"}}, [][]any{{1}, {2}})
	})
	e := f.engine()
	var wg sync.WaitGroup
	var bad atomic.Int32
	ids := sync.Map{}
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sh, err := e.RunUser(context.Background(), mustCheck(t, fmt.Sprintf("SELECT c%d", i), ""), QueryLimits{MaxRows: 10})
			if err != nil || len(sh.Rows) != 2 {
				bad.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Errorf("%d 个并发查询失败", bad.Load())
	}
	for _, r := range f.requests() {
		id := r.Query.Get("query_id")
		if _, dup := ids.LoadOrStore(id, true); dup {
			t.Errorf("query_id 重复: %s", id)
		}
	}
}

func TestTLS(t *testing.T) {
	f := newFakeTLS(t)
	c := f.conn()
	c.SSL, c.VerifySSL = true, true
	e, _ := NewEngine(c)
	defer e.Close()
	_, err := e.RunUser(context.Background(), mustCheck(t, "SELECT 1", ""), QueryLimits{})
	if err == nil || !strings.Contains(err.Error(), "证书") {
		t.Errorf("校验证书时应因自签名证书失败: %v", err)
	}
	if strings.Contains(fmt.Sprint(err), "s3cr3t") {
		t.Error("泄露密码")
	}
	c.VerifySSL = false
	e2, _ := NewEngine(c)
	defer e2.Close()
	if _, err := e2.RunUser(context.Background(), mustCheck(t, "SELECT 1", ""), QueryLimits{}); err != nil {
		t.Errorf("不校验证书应成功: %v", err)
	}
	if err := e2.TestConnection(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestDefaultPorts(t *testing.T) {
	if p := (Config{}).port(); p != 8123 {
		t.Errorf("http 默认端口 = %d", p)
	}
	if p := (Config{TLS: true}).port(); p != 8443 {
		t.Errorf("https 默认端口 = %d", p)
	}
	c := NewClient(Config{Host: "::1", Port: 9000})
	if c.base != "http://[::1]:9000/" {
		t.Errorf("base = %s", c.base)
	}
	c = NewClient(Config{Host: "ch.local", TLS: true})
	if c.base != "https://ch.local:8443/" {
		t.Errorf("base = %s", c.base)
	}
}

func TestNormalizeLimits(t *testing.T) {
	d := (QueryLimits{}).Normalize()
	if d.MaxRows != DefaultMaxRows || d.MaxBytes != DefaultMaxBytes || d.Timeout != DefaultTimeout || d.MaxThreads != DefaultMaxThreads || d.MaxCellBytes != DefaultMaxCellBytes {
		t.Errorf("默认值: %+v", d)
	}
	h := QueryLimits{MaxRows: -1, MaxBytes: -5, Timeout: -time.Second}.Normalize()
	if h.MaxRows != DefaultMaxRows || h.MaxBytes != DefaultMaxBytes || h.Timeout != DefaultTimeout {
		t.Errorf("负值: %+v", h)
	}
	if (QueryLimits{Timeout: 1500 * time.Millisecond}).Normalize().Timeout != 1500*time.Millisecond {
		t.Error("小超时被改写")
	}
}

func TestRunUserRejectsEmpty(t *testing.T) {
	f := newFake(t)
	e := f.engine()
	if _, err := e.RunUser(context.Background(), nil, QueryLimits{}); err == nil {
		t.Error("nil Checked 应报错")
	}
	if _, err := e.RunUser(context.Background(), &Checked{}, QueryLimits{}); err == nil {
		t.Error("空语句应报错")
	}
}

func TestParseColType(t *testing.T) {
	cases := []struct {
		in       string
		base     string
		nullable bool
		tz       string
	}{
		{"UInt8", "UInt8", false, ""},
		{"Nullable(Int32)", "Int32", true, ""},
		{"LowCardinality(Nullable(String))", "String", true, ""},
		{"DateTime('Asia/Shanghai')", "DateTime", false, "Asia/Shanghai"},
		{"Nullable(DateTime64(3, 'UTC'))", "DateTime64", true, "UTC"},
		{"DateTime64(6)", "DateTime64", false, ""},
		{"Array(Nullable(Int8))", "Array", false, ""},
		{"Decimal(18, 4)", "Decimal", false, ""},
		{"Map(String, DateTime('UTC'))", "Map", false, ""},
	}
	for _, c := range cases {
		got := parseColType(c.in)
		if got.base != c.base || got.nullable != c.nullable || got.tz != c.tz {
			t.Errorf("%q: %+v", c.in, got)
		}
	}
}

func TestSQLString(t *testing.T) {
	cases := map[string]string{"abc": `'abc'`, "a'b": `'a\'b'`, "a\\b": "'a\\\\b'", "\\'": "'\\\\\\''", "a\x00b": `'a\0b'`}
	for in, want := range cases {
		if got := sqlString(in); got != want {
			t.Errorf("sqlString(%q) = %s, want %s", in, got, want)
		}
	}
}
