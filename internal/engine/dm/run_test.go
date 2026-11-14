// @author: kevinkelly
// @license: Apache Licence
// @file: run_test.go
// @time: 2026/10/28

package dm

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	dmdrv "gitee.com/chunanyong/dm"
)

func runCheck(t *testing.T, e *Engine, sqlText string, lim QueryLimits) (*Shaped, error) {
	t.Helper()
	ck, err := CheckUserQuery(sqlText, CheckOptions{DB: "HR"})
	if err != nil {
		t.Fatal(err)
	}
	return e.RunUser(context.Background(), ck, lim)
}

func empScript() *fakeScript {
	s := &fakeScript{}
	s.add(verifyRule([2]string{"HR", "EMPLOYEES"}))
	return s
}

func TestRunSelectStatementSequenceAndShape(t *testing.T) {
	s := empScript()
	hired := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	s.add(fakeRule{match: "from employees", cols: []string{"ID", "NAME", "SAL", "HIRED", "NOTE", "PIC", "FLAG", "LV"},
		types: []string{"INT", "VARCHAR", "DECIMAL", "DATETIME", "CLOB", "BLOB", "TINYINT", "SMALLINT"},
		rows: [][]driver.Value{
			{int32(1), "张三", decimalLike("1234.50"), hired, nil, []byte{0xde, 0xad, 0xbe, 0xef}, uint8(255), int16(7)},
			{int32(2), "李四", decimalLike("0"), hired, fakeClob{"备注"}, []byte{}, uint8(3), int16(-8)},
		}})
	e := newFakeEngine(t, s)
	sh, err := runCheck(t, e, "select id, name, sal, hired, note, pic, flag, lv from employees", QueryLimits{MaxRows: 10})
	if err != nil {
		t.Fatal(err)
	}
	// 语句顺序：切 schema → 核对对象 → 只读事务 → 用户语句 → 回滚
	order := []string{`SET SCHEMA "HR"`, "FROM all_objects", "BEGIN", "SET TRANSACTION READ ONLY", "from employees", "ROLLBACK"}
	prev := -1
	for _, sub := range order {
		i := s.index(sub)
		if i < 0 || i <= prev {
			t.Fatalf("语句顺序不对（%q 在 %d，前一条在 %d）：%q", sub, i, prev, s.log())
		}
		prev = i
	}
	if s.has("COMMIT") {
		t.Errorf("不应提交: %q", s.log())
	}
	if got := strings.Join(sh.ColumnList, ","); got != "ID,NAME,SAL,HIRED,NOTE,PIC,FLAG,LV" {
		t.Errorf("列名: %s", got)
	}
	if got := strings.Join(sh.ColumnTypes, ","); got != "INT,VARCHAR,DECIMAL,DATETIME,CLOB,BLOB,TINYINT,SMALLINT" {
		t.Errorf("列类型: %s", got)
	}
	if len(sh.Rows) != 2 || sh.Truncated {
		t.Fatalf("行: %+v", sh)
	}
	r0 := sh.Rows[0]
	// 整数统一成 int64（INT / SMALLINT 驱动返回 int32 / int16）；DECIMAL 是 *DmDecimal，转成字符串保精度；TINYINT 是有符号的，驱动按字节给，要还原符号
	if r0[0] != int64(1) || r0[1] != "张三" || r0[2] != "1234.50" || r0[3] != "2024-05-06 07:08:09" || r0[4] != nil || r0[5] != "0xDEADBEEF" || r0[6] != int64(-1) || r0[7] != int64(7) {
		t.Errorf("第一行: %#v", r0)
	}
	if sh.Rows[1][4] != "备注" || sh.Rows[1][5] != "0x" || sh.Rows[1][6] != int64(3) || sh.Rows[1][7] != int64(-8) {
		t.Errorf("第二行: %#v", sh.Rows[1])
	}
	if sh.Elapsed < 0 {
		t.Error("Elapsed")
	}
}

func TestRunTruncatesByRows(t *testing.T) {
	s := empScript()
	var rows [][]driver.Value
	for i := 0; i < 5; i++ {
		rows = append(rows, []driver.Value{int32(i)})
	}
	s.add(fakeRule{match: "from employees", cols: []string{"ID"}, types: []string{"INT"}, rows: rows})
	e := newFakeEngine(t, s)
	sh, err := runCheck(t, e, "select id from employees", QueryLimits{MaxRows: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(sh.Rows) != 3 || !sh.Truncated || sh.TruncatedBy != TruncRows {
		t.Errorf("按行数截断: rows=%d truncated=%v by=%q", len(sh.Rows), sh.Truncated, sh.TruncatedBy)
	}
	// 正好等于上限：不算截断
	sh, err = runCheck(t, e, "select id from employees", QueryLimits{MaxRows: 5})
	if err != nil || len(sh.Rows) != 5 || sh.Truncated {
		t.Errorf("正好 5 行: %+v %v", sh, err)
	}
}

func TestRunTruncatesByBytesAndCutsCells(t *testing.T) {
	s := empScript()
	big := strings.Repeat("字", 2000) // 6000 字节
	var rows [][]driver.Value
	for i := 0; i < 10; i++ {
		rows = append(rows, []driver.Value{fakeClob{big}})
	}
	s.add(fakeRule{match: "from employees", cols: []string{"NOTE"}, types: []string{"CLOB"}, rows: rows})
	e := newFakeEngine(t, s)
	sh, err := runCheck(t, e, "select note from employees", QueryLimits{MaxRows: 100, MaxBytes: 20000, MaxCellBytes: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if !sh.Truncated || sh.TruncatedBy != TruncBytes || len(sh.Rows) < 1 || len(sh.Rows) >= 10 {
		t.Errorf("按字节数截断: rows=%d by=%q", len(sh.Rows), sh.TruncatedBy)
	}
	cell := sh.Rows[0][0].(string)
	if !strings.HasSuffix(cell, "…") || len(cell) > 3000+len("…") {
		t.Errorf("单元格应截断: %d 字节", len(cell))
	}
	if !strings.Contains(strings.Join(sh.Warnings, "|"), "已截断") {
		t.Errorf("应有单元格截断的提示: %v", sh.Warnings)
	}
}

func TestRunVerifyRejects(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		script func() *fakeScript
		want   string
	}{
		{"对象不存在", "select * from employees", func() *fakeScript { return (&fakeScript{}).add(verifyRule()) }, "不存在"},
		{"其它 schema 的对象不存在", "select * from sales.depts", func() *fakeScript {
			return (&fakeScript{}).add(verifyRule([2]string{"HR", "DEPTS"}))
		}, "SALES.DEPTS"},
		{"数据字典视图被当前 schema 同名对象盖住", "select * from all_tables", func() *fakeScript {
			return (&fakeScript{}).add(fakeRule{match: "FROM all_objects", cols: []string{"K", "O", "N"}, rows: [][]driver.Value{{"D", "HR", "ALL_TABLES"}}})
		}, "SYS.ALL_TABLES"},
		{"内置函数被同名对象盖住", "select nvl(a, 0) from employees", func() *fakeScript {
			return (&fakeScript{}).add(fakeRule{match: "FROM all_objects", cols: []string{"K", "O", "N"},
				rows: [][]driver.Value{{"T", "HR", "EMPLOYEES"}, {"C", "HR", "NVL"}}})
		}, "NVL"},
		{"裸名字是同名函数", "select purge_all from employees", func() *fakeScript {
			return (&fakeScript{}).add(fakeRule{match: "FROM all_objects", cols: []string{"K", "O", "N"},
				rows: [][]driver.Value{{"T", "HR", "EMPLOYEES"}, {"B", "HR", "PURGE_ALL"}}})
		}, "无参函数"},
	}
	for _, c := range cases {
		s := c.script()
		s.add(fakeRule{match: "select", cols: []string{"X"}, rows: [][]driver.Value{{int64(1)}}})
		e := newFakeEngine(t, s)
		_, err := runCheck(t, e, c.sql, QueryLimits{})
		var ue *UserError
		if err == nil || !errors.As(err, &ue) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：期望业务错误含 %q，得到 %v", c.name, c.want, err)
		}
		if s.has("SET TRANSACTION READ ONLY") || s.index(c.sql) >= 0 {
			t.Errorf("%s：核对没通过，用户语句不应执行: %q", c.name, s.log())
		}
	}
}

func TestRunVerifyQueryContent(t *testing.T) {
	s := (&fakeScript{}).add(verifyRule([2]string{"HR", "EMPLOYEES"}, [2]string{"SALES", "ORDERS"}))
	s.add(fakeRule{match: "from hr.employees", cols: []string{"X"}, rows: [][]driver.Value{{int64(1)}}})
	e := newFakeEngine(t, s)
	_, err := runCheck(t, e, "select nvl(a, 0), b from hr.employees e, sales.orders o where e.id = o.id and exists (select 1 from all_tables)", QueryLimits{})
	if err != nil {
		t.Fatal(err)
	}
	var verify string
	for _, q := range s.log() {
		if strings.Contains(q, "FROM all_objects") {
			verify = q
		}
	}
	for _, sub := range []string{
		"owner = 'HR' AND object_name IN ('EMPLOYEES')", "owner = 'SALES' AND object_name IN ('ORDERS')", // 表
		"'MVIEW'", // 达梦的物化视图类型
		"SELECT 'D', owner, object_name FROM all_objects WHERE owner = 'HR'", "'ALL_TABLES'", // 数据字典
		"SELECT 'C', owner, object_name", "'NVL'", // 函数名
		"SELECT 'B', owner, object_name", "all_synonyms", "'A'", "'B'", // 裸名字
	} {
		if !strings.Contains(verify, sub) {
			t.Errorf("核对语句缺少 %q：\n%s", sub, verify)
		}
	}
}

func TestRunExplain(t *testing.T) {
	s := empScript()
	s.add(fakeRule{match: "EXPLAIN select * from employees", cols: []string{"PLAN"}, types: []string{"VARCHAR"},
		rows: [][]driver.Value{{"#NSET2: [1, 107, 80]"}, {"  #PRJT2: [1, 107, 80]"}}})
	e := newFakeEngine(t, s)
	for _, in := range []string{"explain select * from employees", "explain for select * from employees", "explain plan for select * from employees"} {
		sh, err := runCheck(t, e, in, QueryLimits{})
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if len(sh.Rows) != 2 || sh.Rows[0][0] != "#NSET2: [1, 107, 80]" {
			t.Errorf("%s 计划文本: %+v", in, sh.Rows)
		}
		if len(sh.Notices) == 0 {
			t.Errorf("%s 应提示执行计划只是估算", in)
		}
	}
	if !s.has("EXPLAIN select * from employees") {
		t.Errorf("发给服务端的是 EXPLAIN <查询>: %q", s.log())
	}
	for _, q := range s.log() {
		if strings.Contains(q, "EXPLAIN PLAN") || strings.Contains(q, "EXPLAIN FOR") {
			t.Errorf("PLAN / FOR 不应原样发给服务端: %q", q)
		}
	}
}

func TestRunTimeout(t *testing.T) {
	s := empScript()
	s.add(fakeRule{match: "from employees", block: true})
	e := newFakeEngine(t, s)
	start := time.Now()
	_, err := runCheck(t, e, "select * from employees", QueryLimits{Timeout: 80 * time.Millisecond})
	if err == nil || !IsTimeout(err) {
		t.Fatalf("应是超时错误: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("超时没有及时生效: %v", time.Since(start))
	}
}

// 驱动在 context 到期时关闭连接，之后的错误是“连接已关闭”：要还原成超时，而不是当成实例故障。
func TestRunTimeoutRestoredFromClosedConnection(t *testing.T) {
	s := empScript()
	s.add(fakeRule{match: "from employees", err: &dmdrv.DmError{ErrCode: 6060, ErrText: "connection closed"}, delay: 150 * time.Millisecond})
	e := newFakeEngine(t, s)
	_, err := runCheck(t, e, "select * from employees", QueryLimits{Timeout: 50 * time.Millisecond})
	if err == nil || !IsTimeout(err) || IsInfra(err) {
		t.Fatalf("应还原成超时: %v infra=%v", err, IsInfra(err))
	}
}

func TestRunSetSchemaErrors(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "SET SCHEMA", err: &dmdrv.DmError{ErrCode: -2106, ErrText: "无效的模式名[HR]"}})
	e := newFakeEngine(t, s)
	_, err := runCheck(t, e, "select 1 from dual", QueryLimits{})
	var ue *UserError
	if err == nil || !errors.As(err, &ue) || !strings.Contains(err.Error(), "无法切换到 schema HR") || !strings.Contains(err.Error(), "无效的模式名") {
		t.Errorf("schema 不存在应是业务错误: %v", err)
	}
	// 切 schema 时连接断了：是实例故障，不是用户的问题
	s2 := &fakeScript{}
	s2.add(fakeRule{match: "SET SCHEMA", err: &dmdrv.DmError{ErrCode: 6001, ErrText: "communication error"}})
	e2 := newFakeEngine(t, s2)
	_, err = runCheck(t, e2, "select 1 from dual", QueryLimits{})
	if err == nil || errors.As(err, &ue) || !IsInfra(err) {
		t.Errorf("通信错误应是连接层面的错误: %v", err)
	}
}

func TestRunMasksPasswordInErrors(t *testing.T) {
	s := empScript()
	s.add(fakeRule{match: "from employees", err: &dmdrv.DmError{ErrCode: -2106, ErrText: "无效的表或视图名[EMPLOYEES]; password s3cr3t-pw leaked"}})
	e := newFakeEngine(t, s)
	_, err := runCheck(t, e, "select * from employees", QueryLimits{})
	if err == nil || strings.Contains(err.Error(), "s3cr3t-pw") {
		t.Fatalf("错误里不应出现口令: %v", err)
	}
	var se *ServerError
	if !errors.As(err, &se) || se.Code != -2106 || IsInfra(err) || IsTimeout(err) {
		t.Errorf("错误分类: %+v infra=%v", se, IsInfra(err))
	}
}

func TestErrorClassification(t *testing.T) {
	wrapped := fmt.Errorf("读取失败: %w", &dmdrv.DmError{ErrCode: 6060, ErrText: "connection closed"})
	for _, c := range []struct {
		err   error
		infra bool
		code  int
	}{
		{&dmdrv.DmError{ErrCode: 6001, ErrText: "communication error"}, true, 6001},
		{&dmdrv.DmError{ErrCode: 6060, ErrText: "connection closed"}, true, 6060},
		{&dmdrv.DmError{ErrCode: 20004, ErrText: "fatal"}, true, 20004},
		{wrapped, true, 6060},
		{&dmdrv.DmError{ErrCode: -2106, ErrText: "无效的表或视图名[CONNECTION_LOG]"}, false, -2106}, // 对象名里带 connection 不能算连接故障
		{&dmdrv.DmError{ErrCode: -2007, ErrText: "语法分析出错"}, false, -2007},
		{errors.New("dial tcp 10.0.0.1:5236: connect: connection refused"), true, 0},
		{errors.New("通信链路中断"), true, 0},
		{driver.ErrBadConn, true, 0},
		{errors.New("something else"), false, 0},
	} {
		me := mapError(c.err, "pw")
		if IsInfra(me) != c.infra {
			t.Errorf("%v：infra = %v，期望 %v", c.err, IsInfra(me), c.infra)
		}
		var se *ServerError
		if errors.As(me, &se) && se.Code != c.code {
			t.Errorf("%v：错误号 = %d，期望 %d", c.err, se.Code, c.code)
		}
	}
	if !IsReadOnlyViolation(mapError(errors.New("事务只读，不能执行写操作"))) || !IsReadOnlyViolation(mapError(errors.New("Cannot modify in READ ONLY transaction"))) {
		t.Error("只读事务里写入应算只读违规")
	}
	if IsReadOnlyViolation(mapError(errors.New("语法分析出错"))) {
		t.Error("普通错误不是只读违规")
	}
	if !IsCanceled(mapError(context.Canceled)) || !IsTimeout(mapError(context.DeadlineExceeded)) {
		t.Error("context 的取消 / 超时要原样保留")
	}
}

func TestConnectFailureIsMasked(t *testing.T) {
	s := &fakeScript{connErr: errors.New("dial tcp: connection refused (tried with password s3cr3t-pw)")}
	e := newFakeEngine(t, s)
	err := e.TestConnection(context.Background())
	if err == nil || strings.Contains(err.Error(), "s3cr3t-pw") || !IsInfra(err) {
		t.Errorf("连接失败: %v infra=%v", err, IsInfra(err))
	}
}

func TestCellFormatting(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	ts := time.Date(2024, 1, 2, 3, 4, 5, 123456000, loc)
	for _, c := range []struct {
		typ  string
		in   time.Time
		want string
	}{
		{"TIMESTAMP", ts, "2024-01-02 03:04:05.123456"},
		{"DATETIME", ts.Truncate(time.Second), "2024-01-02 03:04:05"},
		{"TIMESTAMP WITH TIME ZONE", ts, "2024-01-02 03:04:05.123456 +08:00"},
		{"DATETIME WITH TIME ZONE", ts.Truncate(time.Second), "2024-01-02 03:04:05 +08:00"},
		{"TIME", ts, "03:04:05.123456"},
		{"TIME WITH TIME ZONE", ts.Truncate(time.Second), "03:04:05 +08:00"},
		{"DATE", time.Date(2024, 1, 2, 0, 0, 0, 0, loc), "2024-01-02"},
		{"DATE", ts.Truncate(time.Second), "2024-01-02 03:04:05"}, // Oracle 兼容模式下 DATE 带时间
	} {
		if got, _, _ := shapeValue(c.in, c.typ, 100); got != c.want {
			t.Errorf("%s: %v，期望 %q", c.typ, got, c.want)
		}
	}
	long := strings.Repeat("a", 50)
	if got, n, cut := shapeValue(long, "VARCHAR", 10); !cut || got != strings.Repeat("a", 10)+"…" || n != len(got.(string)) {
		t.Errorf("截断: %v %d %v", got, n, cut)
	}
	// UTF-8 边界：不能把一个汉字截成两半
	if got, _, cut := shapeValue("一二三四", "VARCHAR", 4); !cut || got != "一…" {
		t.Errorf("UTF-8 边界: %q", got)
	}
	if got, _, _ := shapeValue(true, "BIT", 10); got != true {
		t.Errorf("bool: %v", got)
	}
	nan := 0.0
	nan = nan / nan
	if got, _, _ := shapeValue(nan, "DOUBLE", 10); got != "NaN" {
		t.Errorf("NaN: %v", got)
	}
	if got, _, _ := shapeValue(float32(1.5), "REAL", 10); got != 1.5 {
		t.Errorf("REAL: %v", got)
	}
	// 大字段：只取开头 maxCell+1 个字符，多了就截断；读失败不拖垮整个结果
	if got, _, cut := shapeValue(fakeClob{strings.Repeat("x", 100)}, "CLOB", 10); !cut || got != strings.Repeat("x", 10)+"…" {
		t.Errorf("CLOB 截断: %v %v", got, cut)
	}
	if got, _, cut := shapeValue(fakeClob{""}, "CLOB", 10); cut || got != "" {
		t.Errorf("空 CLOB: %q %v", got, cut)
	}
	if got, _, _ := shapeValue(&fakeBlob{data: make([]byte, 200)}, "BLOB", 10); got != "0x"+strings.Repeat("00", blobPreviewBytes)+"…" {
		t.Errorf("BLOB 预览: %v", got)
	}
	if got, _, _ := shapeValue(&fakeBlob{data: []byte{1, 2}}, "BLOB", 10); got != "0x0102" {
		t.Errorf("短 BLOB: %v", got)
	}
}

// fakeBlob 模拟驱动的 *DmBlob：按 Read 读出。
type fakeBlob struct {
	data []byte
	off  int
}

func (b *fakeBlob) Read(p []byte) (int, error) {
	if b.off >= len(b.data) {
		return 0, io.EOF // 驱动在读完后返回 io.EOF
	}
	n := copy(p, b.data[b.off:])
	b.off += n
	return n, nil
}
