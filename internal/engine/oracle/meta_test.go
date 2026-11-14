// @author: kevinkelly
// @license: Apache Licence
// @file: meta_test.go
// @time: 2026/10/28

package oracle

import (
	"context"
	"database/sql/driver"
	"errors"
	"net/url"
	"strings"
	"testing"

	"dbtool/internal/engine"
)

func TestNewValidation(t *testing.T) {
	good := engine.Conn{Host: "h", Port: 1521, User: "u", Password: "p", ServiceName: "svc"}
	if e, err := New(good); err != nil || e.Name() != "oracle" {
		t.Fatalf("%v", err)
	}
	for _, c := range []struct {
		name string
		mut  func(*engine.Conn)
		want string
	}{
		{"没有地址", func(c *engine.Conn) { c.Host = " " }, "地址为空"},
		{"端口越界", func(c *engine.Conn) { c.Port = 70000 }, "端口"},
		{"没有 service_name 也没有 SID", func(c *engine.Conn) { c.ServiceName, c.SID = "", " " }, "service_name 或 SID"},
		{"没有用户名", func(c *engine.Conn) { c.User = "" }, "用户名"},
	} {
		c2 := good
		c.mut(&c2)
		if _, err := New(c2); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：%v", c.name, err)
		}
	}
	c3 := good
	c3.Port = 0
	if e, _ := New(c3); e.conn.Port != 1521 {
		t.Errorf("缺省端口: %d", e.conn.Port)
	}
}

func TestDSN(t *testing.T) {
	e, _ := New(engine.Conn{Host: "db.example.com", Port: 1522, User: "u@x", Password: "p:w/d@#?%", ServiceName: "ORCLPDB1"})
	u, err := url.Parse(e.dsn())
	if err != nil {
		t.Fatalf("%v: %s", err, e.dsn())
	}
	pw, _ := u.User.Password()
	if u.Scheme != "oracle" || u.Host != "db.example.com:1522" || u.User.Username() != "u@x" || pw != "p:w/d@#?%" || strings.Trim(u.Path, "/") != "ORCLPDB1" {
		t.Errorf("DSN 解析不回原值: %s", e.dsn())
	}
	q := u.Query()
	if q.Get("LOB FETCH") != "PRE" || q.Get("CONNECT TIMEOUT") != "10" || q.Get("SID") != "" || q.Get("SSL") != "" {
		t.Errorf("DSN 选项: %v", q)
	}
	// 只有 SID：走 SID 选项，路径里没有服务名
	e, _ = New(engine.Conn{Host: "h", User: "u", Password: "p", SID: "ORCL"})
	u, _ = url.Parse(e.dsn())
	if u.Query().Get("SID") != "ORCL" || strings.Trim(u.Path, "/") != "" {
		t.Errorf("SID 方式: %s", e.dsn())
	}
	// SSL
	e, _ = New(engine.Conn{Host: "h", User: "u", Password: "p", ServiceName: "s", SSL: true, VerifySSL: true})
	if q := mustQuery(t, e.dsn()); q.Get("SSL") != "true" || q.Get("SSL VERIFY") != "true" {
		t.Errorf("SSL 方式: %v", q)
	}
	e, _ = New(engine.Conn{Host: "h", User: "u", Password: "p", ServiceName: "s", SSL: true})
	if q := mustQuery(t, e.dsn()); q.Get("SSL") != "true" || q.Get("SSL VERIFY") != "false" {
		t.Errorf("SSL 不校验证书: %v", q)
	}
}

func mustQuery(t *testing.T, dsn string) url.Values {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestTestConnectionAndVersion(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "SELECT 1 FROM DUAL", cols: []string{"1"}, rows: [][]driver.Value{{int64(1)}}})
	s.add(fakeRule{match: "FROM v$version", cols: []string{"BANNER"}, rows: [][]driver.Value{{"Oracle Database 19c Enterprise Edition Release 19.0.0.0.0 - Production"}}})
	e := newFakeEngine(t, s)
	if err := e.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	v, err := e.ServerVersion(context.Background())
	if err != nil || !strings.HasPrefix(v, "Oracle Database 19c") {
		t.Errorf("版本: %q %v", v, err)
	}
	// V$VERSION 没权限：退到 PRODUCT_COMPONENT_VERSION
	s2 := &fakeScript{}
	s2.add(fakeRule{match: "FROM v$version", err: errors.New("ORA-00942: table or view does not exist")})
	s2.add(fakeRule{match: "FROM product_component_version", cols: []string{"VERSION"}, rows: [][]driver.Value{{"19.0.0.0.0"}}})
	e2 := newFakeEngine(t, s2)
	if v, err := e2.ServerVersion(context.Background()); err != nil || v != "19.0.0.0.0" {
		t.Errorf("退到 PRODUCT_COMPONENT_VERSION: %q %v", v, err)
	}
	// 都读不到：报错
	e3 := newFakeEngine(t, &fakeScript{})
	if _, err := e3.ServerVersion(context.Background()); err == nil {
		t.Error("读不到版本应报错")
	}
}

func TestAllDatabasesTablesColumns(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "FROM all_users", cols: []string{"USERNAME"}, rows: [][]driver.Value{{"APEX_040200"}, {"HR"}, {"SALES"}, {"SYS"}, {"SYSTEM"}, {"XDB"}, {"FLOWS_FILES"}}})
	s.add(fakeRule{match: "FROM all_objects", cols: []string{"OBJECT_NAME"}, rows: [][]driver.Value{{"DEPARTMENTS"}, {"EMPLOYEES"}}})
	s.add(fakeRule{match: "FROM all_tab_columns", cols: []string{"COLUMN_NAME"}, rows: [][]driver.Value{{"ID"}, {"NAME"}}})
	e := newFakeEngine(t, s)
	dbs, err := e.AllDatabases(context.Background())
	if err != nil || strings.Join(dbs, ",") != "HR,SALES" {
		t.Errorf("schema 列表应去掉系统 schema: %v %v", dbs, err)
	}
	tables, err := e.AllTables(context.Background(), "HR")
	if err != nil || strings.Join(tables, ",") != "DEPARTMENTS,EMPLOYEES" {
		t.Errorf("表: %v %v", tables, err)
	}
	if !s.has("owner = 'HR'") || !s.has("BIN$%") {
		t.Errorf("表查询应限定 owner 并排除回收站: %q", s.log())
	}
	cols, err := e.AllColumns(context.Background(), "HR", "EMPLOYEES")
	if err != nil || strings.Join(cols, ",") != "ID,NAME" {
		t.Errorf("列: %v %v", cols, err)
	}
	if !s.has("table_name = 'EMPLOYEES'") {
		t.Errorf("列查询应限定表名: %q", s.log())
	}
	// 名字里的单引号翻倍，不能拼出注入
	if _, err := e.AllColumns(context.Background(), "HR", "X' OR '1'='1"); err != nil {
		t.Fatal(err)
	}
	if !s.has("table_name = 'X'' OR ''1''=''1'") {
		t.Errorf("单引号应翻倍: %q", s.log())
	}
	if _, err := e.AllTables(context.Background(), `A"B`); err == nil {
		t.Error("非法 schema 名应拒绝")
	}
}

func TestTableInfos(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "SELECT object_name, object_type FROM all_objects", cols: []string{"OBJECT_NAME", "OBJECT_TYPE"}, rows: [][]driver.Value{
		{"EMPLOYEES", "TABLE"}, {"EMP_V", "VIEW"}, {"MV_SALES", "MATERIALIZED VIEW"}, {"MV_SALES", "TABLE"}, {"NOSTATS", "TABLE"},
	}})
	s.add(fakeRule{match: "FROM all_tab_comments", cols: []string{"TABLE_NAME", "COMMENTS"}, rows: [][]driver.Value{{"EMPLOYEES", "员工表"}}})
	s.add(fakeRule{match: "FROM all_tables", cols: []string{"TABLE_NAME", "NUM_ROWS"}, rows: [][]driver.Value{{"EMPLOYEES", int64(107)}, {"MV_SALES", float64(5)}}})
	e := newFakeEngine(t, s)
	infos, err := e.TableInfos(context.Background(), "HR")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 4 {
		t.Fatalf("物化视图的容器表要和物化视图合并: %+v", infos)
	}
	by := map[string]TableInfo{}
	for _, i := range infos {
		by[i.Name] = i
	}
	if i := by["EMPLOYEES"]; i.Kind != "table" || i.IsView || i.Comment != "员工表" || i.Rows == nil || *i.Rows != 107 {
		t.Errorf("EMPLOYEES: %+v", i)
	}
	if i := by["EMP_V"]; i.Kind != "view" || !i.IsView || i.Rows != nil {
		t.Errorf("EMP_V: %+v", i)
	}
	if i := by["MV_SALES"]; i.Kind != "matview" || !i.IsView || i.Rows != nil {
		t.Errorf("MV_SALES: %+v", i)
	}
	if i := by["NOSTATS"]; i.Rows != nil {
		t.Errorf("没收集统计信息的表行数未知: %+v", i)
	}
}

func TestColumnInfos(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "char_length, char_used", cols: []string{"COLUMN_NAME", "DATA_TYPE", "DATA_LENGTH", "DATA_PRECISION", "DATA_SCALE", "NULLABLE", "COLUMN_ID", "CHAR_LENGTH", "CHAR_USED"},
		rows: [][]driver.Value{
			{"ID", "NUMBER", int64(22), int64(10), int64(0), "N", int64(1), int64(0), nil},
			{"NAME", "VARCHAR2", int64(200), nil, nil, "Y", int64(2), int64(50), "C"},
			{"SAL", "NUMBER", int64(22), int64(8), int64(2), "Y", int64(3), int64(0), nil},
			{"CODE", "CHAR", int64(3), nil, nil, "Y", int64(4), int64(3), "B"},
			{"HIRED", "DATE", int64(7), nil, nil, "Y", int64(5), int64(0), nil},
			{"TS", "TIMESTAMP(6)", int64(11), nil, int64(6), "Y", int64(6), int64(0), nil},
			{"ANYNUM", "NUMBER", int64(22), nil, nil, "Y", int64(7), int64(0), nil},
		}})
	s.add(fakeRule{match: "FROM all_col_comments", cols: []string{"COLUMN_NAME", "COMMENTS"}, rows: [][]driver.Value{{"NAME", "姓名"}}})
	s.add(fakeRule{match: "constraint_type = 'P'", cols: []string{"COLUMN_NAME"}, rows: [][]driver.Value{{"ID"}}})
	e := newFakeEngine(t, s)
	cols, err := e.ColumnInfos(context.Background(), "HR", "EMPLOYEES")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name, typ string
		nullable  bool
		pk        bool
		comment   string
	}{
		{"ID", "NUMBER(10)", false, true, ""},
		{"NAME", "VARCHAR2(50 CHAR)", true, false, "姓名"},
		{"SAL", "NUMBER(8,2)", true, false, ""},
		{"CODE", "CHAR(3)", true, false, ""},
		{"HIRED", "DATE", true, false, ""},
		{"TS", "TIMESTAMP(6)", true, false, ""},
		{"ANYNUM", "NUMBER", true, false, ""},
	}
	if len(cols) != len(want) {
		t.Fatalf("%+v", cols)
	}
	for i, w := range want {
		c := cols[i]
		if c.Name != w.name || c.Type != w.typ || c.Nullable != w.nullable || c.IsPrimaryKey != w.pk || c.Comment != w.comment || c.Position != i+1 {
			t.Errorf("第 %d 列: %+v，期望 %+v", i, c, w)
		}
	}
}

func TestColumnInfosWithoutCharLength(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "char_length, char_used", err: errors.New("ORA-00904: \"CHAR_USED\": invalid identifier")})
	s.add(fakeRule{match: "FROM all_tab_columns", cols: []string{"COLUMN_NAME", "DATA_TYPE", "DATA_LENGTH", "DATA_PRECISION", "DATA_SCALE", "NULLABLE", "COLUMN_ID"},
		rows: [][]driver.Value{{"NAME", "VARCHAR2", int64(50), nil, nil, "Y", int64(1)}}})
	e := newFakeEngine(t, s)
	cols, err := e.ColumnInfos(context.Background(), "HR", "T")
	if err != nil || len(cols) != 1 || cols[0].Type != "VARCHAR2(50)" {
		t.Errorf("退回只用 data_length: %+v %v", cols, err)
	}
}

func TestIndexesAndCreateStatement(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "FROM all_indexes", cols: []string{"INDEX_NAME", "UNIQUENESS", "COLUMN_NAME", "COLUMN_POSITION"}, rows: [][]driver.Value{
		{"PK_EMP", "UNIQUE", "ID", int64(1)}, {"IX_NAME", "NONUNIQUE", "NAME", int64(1)}, {"IX_NAME", "NONUNIQUE", "HIRED", int64(2)},
	}})
	s.add(fakeRule{match: "DBMS_METADATA.GET_DDL('TABLE', 'EMPLOYEES', 'HR')", cols: []string{"DDL"}, rows: [][]driver.Value{{"\n  CREATE TABLE \"HR\".\"EMPLOYEES\" (\"ID\" NUMBER(10,0))\n"}}})
	e := newFakeEngine(t, s)
	idx, err := e.Indexes(context.Background(), "HR", "EMPLOYEES")
	if err != nil || len(idx) != 2 || !idx[0].Unique || idx[1].Unique || strings.Join(idx[1].Columns, ",") != "NAME,HIRED" {
		t.Errorf("索引: %+v %v", idx, err)
	}
	ddl, err := e.CreateStatement(context.Background(), "HR", "EMPLOYEES", false)
	if err != nil || !strings.HasPrefix(ddl, "CREATE TABLE") {
		t.Errorf("DDL: %q %v", ddl, err)
	}
}

func TestCreateStatementFallback(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "DBMS_METADATA", err: errors.New("ORA-31603: object not found or no privilege")})
	s.add(fakeRule{match: "char_length, char_used", cols: []string{"COLUMN_NAME", "DATA_TYPE", "DATA_LENGTH", "DATA_PRECISION", "DATA_SCALE", "NULLABLE", "COLUMN_ID", "CHAR_LENGTH", "CHAR_USED"},
		rows: [][]driver.Value{
			{"ID", "NUMBER", int64(22), int64(10), int64(0), "N", int64(1), int64(0), nil},
			{"NAME", "VARCHAR2", int64(200), nil, nil, "Y", int64(2), int64(50), "C"},
		}})
	s.add(fakeRule{match: "constraint_type = 'P'", cols: []string{"COLUMN_NAME"}, rows: [][]driver.Value{{"ID"}}})
	e := newFakeEngine(t, s)
	ddl, err := e.CreateStatement(context.Background(), "HR", "EMP", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"近似的建表语句", `CREATE TABLE "HR"."EMP"`, `"ID" NUMBER(10) NOT NULL`, `"NAME" VARCHAR2(50 CHAR)`, `PRIMARY KEY ("ID")`} {
		if !strings.Contains(ddl, sub) {
			t.Errorf("近似 DDL 缺少 %q：\n%s", sub, ddl)
		}
	}
	// 视图读不到定义：没有近似可用，直接报错
	if _, err := e.CreateStatement(context.Background(), "HR", "V", true); err == nil {
		t.Error("读不到视图定义应报错")
	}
}

func TestFormatType(t *testing.T) {
	i := func(n int64) *int64 { return &n }
	for _, c := range []struct {
		dt                           string
		length, prec, scale, charLen *int64
		used                         string
		want                         string
	}{
		{"NUMBER", i(22), nil, nil, nil, "", "NUMBER"},
		{"NUMBER", i(22), i(10), i(0), nil, "", "NUMBER(10)"},
		{"NUMBER", i(22), i(10), i(2), nil, "", "NUMBER(10,2)"},
		{"NUMBER", i(22), nil, i(2), nil, "", "NUMBER(*,2)"},
		{"FLOAT", i(22), i(126), nil, nil, "", "FLOAT(126)"},
		{"VARCHAR2", i(200), nil, nil, i(50), "C", "VARCHAR2(50 CHAR)"},
		{"VARCHAR2", i(50), nil, nil, i(50), "B", "VARCHAR2(50)"},
		{"NVARCHAR2", i(100), nil, nil, i(50), "", "NVARCHAR2(50)"},
		{"CHAR", i(3), nil, nil, nil, "", "CHAR(3)"},
		{"RAW", i(16), nil, nil, nil, "", "RAW(16)"},
		{"DATE", i(7), nil, nil, nil, "", "DATE"},
		{"TIMESTAMP(6) WITH TIME ZONE", i(13), nil, i(6), nil, "", "TIMESTAMP(6) WITH TIME ZONE"},
		{"CLOB", i(4000), nil, nil, nil, "", "CLOB"},
	} {
		if got := formatType(c.dt, c.length, c.prec, c.scale, c.charLen, c.used); got != c.want {
			t.Errorf("%s：%q，期望 %q", c.dt, got, c.want)
		}
	}
}

func TestQueryViaEngineInterface(t *testing.T) {
	s := empScript()
	s.add(fakeRule{match: "from employees", cols: []string{"ID"}, types: []string{"NUMBER"}, rows: [][]driver.Value{{int64(1)}, {int64(2)}, {int64(3)}}})
	e := newFakeEngine(t, s)
	rs, err := e.Query(context.Background(), "select id from employees", engine.QueryOptions{DBName: "HR", Limit: 10})
	if err != nil || len(rs.Rows) != 3 || rs.ColumnList[0] != "ID" {
		t.Fatalf("%+v %v", rs, err)
	}
	// 超过 Limit：丢弃多出来的行，不报错；Limit<=0 不限制但受硬上限保护
	rs, err = e.Query(context.Background(), "select id from employees", engine.QueryOptions{DBName: "HR", Limit: 2})
	if err != nil || len(rs.Rows) != 2 {
		t.Errorf("Limit=2: %+v %v", rs, err)
	}
	// 语句仍会过检查
	if _, err := e.Query(context.Background(), "delete from employees", engine.QueryOptions{DBName: "HR"}); err == nil {
		t.Error("Query 不信任调用方，写语句要拒绝")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	e := newFakeEngine(t, empScript())
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AllDatabases(context.Background()); err == nil {
		t.Error("关闭后不能再用")
	}
}

func TestRegisteredInEngineRegistry(t *testing.T) {
	found := false
	for _, n := range engine.Supported() {
		found = found || n == DBType
	}
	if !found {
		t.Errorf("oracle 引擎应注册到 engine 注册表: %v", engine.Supported())
	}
	if _, err := engine.New(DBType, engine.Conn{Host: "h", User: "u", ServiceName: "s"}); err != nil {
		t.Error(err)
	}
}
