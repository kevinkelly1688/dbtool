// @author: kevinkelly
// @license: Apache Licence
// @file: run_test.go
// @time: 2026/10/28

package oracle

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
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
	s.add(fakeRule{match: "from employees", cols: []string{"ID", "NAME", "SAL", "HIRED", "NOTE", "PIC"},
		types: []string{"NUMBER", "VARCHAR2", "NUMBER", "DATE", "CLOB", "BLOB"},
		rows: [][]driver.Value{
			{int64(1), "张三", 1234.5, hired, nil, []byte{0xde, 0xad, 0xbe, 0xef}},
			{int64(2), "李四", float64(0), hired, "备注", []byte{}},
		}})
	e := newFakeEngine(t, s)
	sh, err := runCheck(t, e, "select id, name, sal, hired, note, pic from employees", QueryLimits{MaxRows: 10})
	if err != nil {
		t.Fatal(err)
	}
	// 语句顺序：切 schema → 核对对象 → 只读事务 → 用户语句 → 回滚
	order := []string{`ALTER SESSION SET CURRENT_SCHEMA = "HR"`, "FROM all_objects", "BEGIN", "SET TRANSACTION READ ONLY", "from employees", "ROLLBACK"}
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
	if got := strings.Join(sh.ColumnList, ","); got != "ID,NAME,SAL,HIRED,NOTE,PIC" {
		t.Errorf("列名: %s", got)
	}
	if got := strings.Join(sh.ColumnTypes, ","); got != "NUMBER,VARCHAR2,NUMBER,DATE,CLOB,BLOB" {
		t.Errorf("列类型: %s", got)
	}
	if len(sh.Rows) != 2 || sh.Truncated {
		t.Fatalf("行: %+v", sh)
	}
	r0 := sh.Rows[0]
	if r0[0] != int64(1) || r0[1] != "张三" || r0[2] != 1234.5 || r0[3] != "2024-05-06 07:08:09" || r0[4] != nil || r0[5] != "0xDEADBEEF" {
		t.Errorf("第一行: %#v", r0)
	}
	if sh.Rows[1][5] != "0x" {
		t.Errorf("空 BLOB: %#v", sh.Rows[1][5])
	}
	if sh.Elapsed < 0 {
		t.Error("Elapsed")
	}
}

func TestRunTruncatesByRows(t *testing.T) {
	s := empScript()
	var rows [][]driver.Value
	for i := 0; i < 5; i++ {
		rows = append(rows, []driver.Value{int64(i)})
	}
	s.add(fakeRule{match: "from employees", cols: []string{"ID"}, types: []string{"NUMBER"}, rows: rows})
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
		rows = append(rows, []driver.Value{big})
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
	s.add(fakeRule{match: "DBMS_XPLAN.DISPLAY", cols: []string{"PLAN_TABLE_OUTPUT"}, types: []string{"VARCHAR2"},
		rows: [][]driver.Value{{"Plan hash value: 1"}, {"| Id | Operation |"}}})
	e := newFakeEngine(t, s)
	sh, err := runCheck(t, e, "explain plan for select * from employees", QueryLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sh.Rows) != 2 || sh.Rows[0][0] != "Plan hash value: 1" {
		t.Errorf("计划文本: %+v", sh.Rows)
	}
	if s.index("EXPLAIN PLAN SET STATEMENT_ID") < 0 || s.index("DBMS_XPLAN.DISPLAY") < s.index("EXPLAIN PLAN SET STATEMENT_ID") {
		t.Errorf("语句顺序: %q", s.log())
	}
	if s.has("SET TRANSACTION READ ONLY") {
		t.Errorf("EXPLAIN PLAN 要写会话私有的 PLAN_TABLE，不放在只读事务里: %q", s.log())
	}
	if !s.has("ROLLBACK") {
		t.Errorf("最后要回滚: %q", s.log())
	}
	if len(sh.Notices) == 0 {
		t.Error("应提示执行计划只是估算")
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

func TestRunSetSchemaErrors(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "ALTER SESSION SET CURRENT_SCHEMA", err: errors.New("ORA-01435: user does not exist")})
	e := newFakeEngine(t, s)
	_, err := runCheck(t, e, "select 1 from dual", QueryLimits{})
	var ue *UserError
	if err == nil || !errors.As(err, &ue) || !strings.Contains(err.Error(), "schema HR 不存在") {
		t.Errorf("schema 不存在应是业务错误: %v", err)
	}
}

func TestRunMasksPasswordInErrors(t *testing.T) {
	s := empScript()
	s.add(fakeRule{match: "from employees", err: errors.New("ORA-00942: table or view does not exist; password s3cr3t-pw leaked")})
	e := newFakeEngine(t, s)
	_, err := runCheck(t, e, "select * from employees", QueryLimits{})
	if err == nil || strings.Contains(err.Error(), "s3cr3t-pw") {
		t.Fatalf("错误里不应出现口令: %v", err)
	}
	var se *ServerError
	if !errors.As(err, &se) || se.Code != 942 || IsInfra(err) || IsTimeout(err) {
		t.Errorf("错误分类: %+v infra=%v", se, IsInfra(err))
	}
}

func TestErrorClassification(t *testing.T) {
	for _, c := range []struct {
		err   error
		infra bool
		code  int
	}{
		{errors.New("ORA-12541: TNS:no listener"), true, 12541},
		{errors.New("ORA-01017: invalid username/password; logon denied"), true, 1017},
		{errors.New("ORA-28000: the account is locked"), true, 28000},
		{errors.New("ORA-03113: end-of-file on communication channel"), true, 3113},
		{errors.New("ORA-00942: table or view does not exist"), false, 942},
		{errors.New("ORA-00933: SQL command not properly ended"), false, 933},
		{driver.ErrBadConn, true, 0},
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
	if !IsCanceled(mapError(errors.New("ORA-01013: user requested cancel of current operation"))) {
		t.Error("ORA-01013 应算取消")
	}
	if !IsReadOnlyViolation(mapError(errors.New("ORA-01456: may not perform insert/delete/update operation inside a READ ONLY transaction"))) {
		t.Error("ORA-01456 应算只读违规")
	}
	// 超时 / 取消是期限问题，不是实例故障（context.DeadlineExceeded 自己也满足 net.Error）
	if IsInfra(mapError(context.DeadlineExceeded)) || IsInfra(mapError(context.Canceled)) || !IsTimeout(mapError(context.DeadlineExceeded)) {
		t.Error("context 的超时 / 取消不应算实例故障")
	}
}

func TestConnectFailureIsMasked(t *testing.T) {
	s := &fakeScript{connErr: errors.New("ORA-12541: TNS:no listener (tried with password s3cr3t-pw)")}
	e := newFakeEngine(t, s)
	err := e.TestConnection(context.Background())
	if err == nil || strings.Contains(err.Error(), "s3cr3t-pw") || !IsInfra(err) {
		t.Errorf("连接失败: %v infra=%v", err, IsInfra(err))
	}
}

func TestCellFormatting(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	ts := time.Date(2024, 1, 2, 3, 4, 5, 123456000, loc)
	if got, _, _ := shapeValue(ts, "TIMESTAMP(6)", 100); got != "2024-01-02 03:04:05.123456" {
		t.Errorf("TIMESTAMP: %v", got)
	}
	if got, _, _ := shapeValue(ts, "TIMESTAMP(6) WITH TIME ZONE", 100); got != "2024-01-02 03:04:05.123456 +08:00" {
		t.Errorf("带时区: %v", got)
	}
	if got, _, _ := shapeValue(ts.Truncate(time.Second), "DATE", 100); got != "2024-01-02 03:04:05" {
		t.Errorf("DATE: %v", got)
	}
	long := strings.Repeat("a", 50)
	if got, n, cut := shapeValue(long, "VARCHAR2", 10); !cut || got != strings.Repeat("a", 10)+"…" || n != len(got.(string)) {
		t.Errorf("截断: %v %d %v", got, n, cut)
	}
	// UTF-8 边界：不能把一个汉字截成两半
	if got, _, cut := shapeValue("一二三四", "VARCHAR2", 4); !cut || got != "一…" {
		t.Errorf("UTF-8 边界: %q", got)
	}
	if got, _, _ := shapeValue(true, "", 10); got != true {
		t.Errorf("bool: %v", got)
	}
	nan := 0.0
	nan = nan / nan
	if got, _, _ := shapeValue(nan, "BINARY_DOUBLE", 10); got != "NaN" {
		t.Errorf("NaN: %v", got)
	}
}
