// @author: kevinkelly
// @license: Apache Licence
// @file: meta_test.go
// @time: 2026/10/28

package dm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	dmdrv "gitee.com/chunanyong/dm"

	"dbtool/internal/engine"
)

func TestNewValidation(t *testing.T) {
	good := engine.Conn{Host: "h", Port: 5236, User: "u", Password: "p"}
	if e, err := New(good); err != nil || e.Name() != "dm" {
		t.Fatalf("%v", err)
	}
	for _, c := range []struct {
		name string
		mut  func(*engine.Conn)
		want string
	}{
		{"没有地址", func(c *engine.Conn) { c.Host = " " }, "地址为空"},
		{"端口越界", func(c *engine.Conn) { c.Port = 70000 }, "端口"},
		{"没有用户名", func(c *engine.Conn) { c.User = "" }, "用户名"},
		{"用户名含 @", func(c *engine.Conn) { c.User = "a@b" }, "用户名不能包含"},
		{"用户名含 :", func(c *engine.Conn) { c.User = "a:b" }, "用户名不能包含"},
		{"用户名含 /", func(c *engine.Conn) { c.User = "a/b" }, "用户名不能包含"},
		{"用户名含 ?", func(c *engine.Conn) { c.User = "a?b" }, "用户名不能包含"},
		{"SSL 暂不支持", func(c *engine.Conn) { c.SSL = true }, "SSL"},
	} {
		c2 := good
		c.mut(&c2)
		if _, err := New(c2); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：%v", c.name, err)
		}
	}
	c3 := good
	c3.Port = 0
	if e, _ := New(c3); e.conn.Port != 5236 {
		t.Errorf("缺省端口: %d", e.conn.Port)
	}
	// service_name / SID 是 Oracle 的概念，达梦不需要
	c4 := good
	c4.ServiceName, c4.SID = "x", "y"
	if _, err := New(c4); err != nil {
		t.Errorf("达梦忽略 service_name / SID: %v", err)
	}
}

func TestDSN(t *testing.T) {
	const pw = "p:w/d@#?%&=+x"
	e, err := New(engine.Conn{Host: "db.example.com", Port: 5237, User: "DBT_RO", Password: pw})
	if err != nil {
		t.Fatal(err)
	}
	want := "dm://DBT_RO:" + pw + "@db.example.com:5237?connectTimeout=10000&escapeProcess=false"
	if e.dsn() != want {
		t.Fatalf("DSN:\n got %s\nwant %s", e.dsn(), want)
	}
	if strings.Contains(e.dsn(), "LobMode") {
		t.Error("不能设 LobMode=2：会把整个大字段读进内存")
	}
	// 用达梦驱动自己的解析器核对：用户名、口令、地址都原样解析出来（口令里的 @ ? : / & 不会被拆错）
	db, err := sql.Open("dm", e.dsn())
	if err != nil {
		t.Fatalf("驱动不认这个连接串: %v", err)
	}
	defer db.Close()
	dc, ok := db.Driver().(driver.DriverContext)
	if !ok {
		t.Skip("驱动不提供 OpenConnector")
	}
	conn, err := dc.OpenConnector(e.dsn())
	if err != nil {
		t.Fatalf("驱动解析连接串失败: %v", err)
	}
	cn, ok := conn.(*dmdrv.DmConnector)
	if !ok {
		t.Skip("OpenConnector 返回的不是 *DmConnector")
	}
	u, err := url.Parse(cn.BuildDSN()) // BuildDSN 把驱动解析出的用户名 / 口令转义后写回
	if err != nil {
		t.Fatal(err)
	}
	gotPw, _ := u.User.Password()
	if u.Scheme != "dm" || u.User.Username() != "DBT_RO" || gotPw != pw || u.Host != "db.example.com:5237" {
		t.Errorf("驱动解析结果与输入不一致: %s", cn.BuildDSN())
	}
	// IPv6 地址要带方括号
	e6, _ := New(engine.Conn{Host: "::1", Port: 5236, User: "u", Password: "p"})
	if !strings.Contains(e6.dsn(), "@[::1]:5236?") {
		t.Errorf("IPv6: %s", e6.dsn())
	}
}

func TestTestConnectionAndVersion(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "SELECT 1 FROM DUAL", cols: []string{"1"}, rows: [][]driver.Value{{int64(1)}}})
	s.add(fakeRule{match: "FROM v$version", cols: []string{"BANNER"}, rows: [][]driver.Value{{"DM Database Server 64 V8"}}})
	e := newFakeEngine(t, s)
	if err := e.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	v, err := e.ServerVersion(context.Background())
	if err != nil || v != "DM Database Server 64 V8" {
		t.Errorf("版本: %q %v", v, err)
	}
	// V$VERSION 没权限：退到 @@VERSION
	s2 := &fakeScript{}
	s2.add(fakeRule{match: "FROM v$version", err: errors.New("[CODE:-2106]无效的表或视图名[V$VERSION]")})
	s2.add(fakeRule{match: "SELECT @@VERSION", cols: []string{"@@VERSION"}, rows: [][]driver.Value{{"8.1.2.128-Build(2021.11.2-143456)ENT"}}})
	e2 := newFakeEngine(t, s2)
	if v, err := e2.ServerVersion(context.Background()); err != nil || v != "8.1.2.128-Build(2021.11.2-143456)ENT" {
		t.Errorf("退到 @@VERSION: %q %v", v, err)
	}
	// 都读不到：报错
	e3 := newFakeEngine(t, &fakeScript{})
	if _, err := e3.ServerVersion(context.Background()); err == nil {
		t.Error("读不到版本应报错")
	}
}

func TestAllDatabasesTablesColumns(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "FROM all_users", cols: []string{"USERNAME"}, rows: [][]driver.Value{{"CTISYS"}, {"HR"}, {"SALES"}, {"SYS"}, {"SYSAUDITOR"}, {"SYSDBA"}, {"SYSSSO"}}})
	s.add(fakeRule{match: "FROM all_objects", cols: []string{"OBJECT_NAME"}, rows: [][]driver.Value{{"DEPARTMENTS"}, {"EMPLOYEES"}}})
	s.add(fakeRule{match: "FROM all_tab_columns", cols: []string{"COLUMN_NAME"}, rows: [][]driver.Value{{"ID"}, {"NAME"}}})
	e := newFakeEngine(t, s)
	dbs, err := e.AllDatabases(context.Background())
	if err != nil || strings.Join(dbs, ",") != "HR,SALES,SYSDBA" {
		t.Errorf("schema 列表应去掉系统 schema（SYSDBA 是普通 DBA 账号，保留）: %v %v", dbs, err)
	}
	if !s.has("UNION SELECT owner FROM all_objects") {
		t.Errorf("schema 列表应并上账号看得到的表所属的 schema: %q", s.log())
	}
	tables, err := e.AllTables(context.Background(), "HR")
	if err != nil || strings.Join(tables, ",") != "DEPARTMENTS,EMPLOYEES" {
		t.Errorf("表: %v %v", tables, err)
	}
	if !s.has("owner = 'HR'") {
		t.Errorf("表查询应限定 owner: %q", s.log())
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
		{"EMPLOYEES", "TABLE"}, {"EMP_V", "VIEW"}, {"MV_SALES", "MATERIALIZED VIEW"}, {"MV_SALES", "TABLE"}, {"MV2", "MVIEW"}, {"NOSTATS", "TABLE"},
	}})
	s.add(fakeRule{match: "FROM all_tab_comments", cols: []string{"TABLE_NAME", "COMMENTS"}, rows: [][]driver.Value{{"EMPLOYEES", "员工表"}}})
	s.add(fakeRule{match: "FROM all_tables", cols: []string{"TABLE_NAME", "NUM_ROWS"}, rows: [][]driver.Value{{"EMPLOYEES", int64(107)}, {"MV_SALES", int32(5)}}})
	e := newFakeEngine(t, s)
	infos, err := e.TableInfos(context.Background(), "HR")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 5 {
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
	if i := by["MV2"]; i.Kind != "matview" || !i.IsView {
		t.Errorf("MVIEW 类型也是物化视图: %+v", i)
	}
	if i := by["NOSTATS"]; i.Rows != nil {
		t.Errorf("没收集统计信息的表行数未知: %+v", i)
	}
}

func TestColumnInfos(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "char_length, char_used", cols: []string{"COLUMN_NAME", "DATA_TYPE", "DATA_LENGTH", "DATA_PRECISION", "DATA_SCALE", "NULLABLE", "COLUMN_ID", "CHAR_LENGTH", "CHAR_USED"},
		rows: [][]driver.Value{
			{"ID", "INT", int32(4), nil, nil, "N", int32(1), int32(0), nil},
			{"NAME", "VARCHAR", int32(200), nil, nil, "Y", int32(2), int32(50), "C"},
			{"SAL", "DECIMAL", int32(0), int32(8), int32(2), "Y", int32(3), int32(0), nil},
			{"CODE", "CHAR", int32(3), nil, nil, "Y", int32(4), int32(3), "B"},
			{"HIRED", "DATE", int32(3), nil, int32(0), "Y", int32(5), int32(0), nil},
			{"TS", "TIMESTAMP", int32(8), nil, int32(6), "Y", int32(6), int32(0), nil},
			{"ANYNUM", "NUMBER", int32(0), nil, nil, "Y", int32(7), int32(0), nil},
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
		{"ID", "INT", false, true, ""},
		{"NAME", "VARCHAR(50)", true, false, "姓名"},
		{"SAL", "DECIMAL(8,2)", true, false, ""},
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
	s.add(fakeRule{match: "char_length, char_used", err: errors.New("[CODE:-2111]无效的列名[CHAR_USED]")})
	s.add(fakeRule{match: "FROM all_tab_columns", cols: []string{"COLUMN_NAME", "DATA_TYPE", "DATA_LENGTH", "DATA_PRECISION", "DATA_SCALE", "NULLABLE", "COLUMN_ID"},
		rows: [][]driver.Value{{"NAME", "VARCHAR", int32(50), nil, nil, "Y", int32(1)}}})
	e := newFakeEngine(t, s)
	cols, err := e.ColumnInfos(context.Background(), "HR", "T")
	if err != nil || len(cols) != 1 || cols[0].Type != "VARCHAR(50)" {
		t.Errorf("退回只用 data_length: %+v %v", cols, err)
	}
}

func TestIndexesAndCreateStatement(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "FROM all_indexes", cols: []string{"INDEX_NAME", "UNIQUENESS", "COLUMN_NAME", "COLUMN_POSITION"}, rows: [][]driver.Value{
		{"PK_EMP", "UNIQUE", "ID", int32(1)}, {"IX_NAME", "NONUNIQUE", "NAME", int32(1)}, {"IX_NAME", "NONUNIQUE", "HIRED", int32(2)},
	}})
	s.add(fakeRule{match: "DBMS_METADATA.GET_DDL('TABLE', 'EMPLOYEES', 'HR')", cols: []string{"DDL"}, rows: [][]driver.Value{{"\n  CREATE TABLE \"HR\".\"EMPLOYEES\" (\"ID\" INT)\n"}}})
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

// fakeClob 模拟驱动返回的 *DmClob：GET_DDL 的结果是 CLOB，要按接口读出来。
type fakeClob struct{ s string }

func (c fakeClob) ReadString(pos, length int) (string, error) {
	r := []rune(c.s)
	if pos-1 >= len(r) {
		return "", io.EOF // 驱动在取不到内容时返回 io.EOF
	}
	r = r[pos-1:]
	if length < len(r) {
		r = r[:length]
	}
	return string(r), nil
}

func TestCreateStatementFromClob(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "DBMS_METADATA.GET_DDL", cols: []string{"DDL"}, rows: [][]driver.Value{{fakeClob{"CREATE TABLE \"HR\".\"T\" (\"ID\" INT)"}}}})
	e := newFakeEngine(t, s)
	ddl, err := e.CreateStatement(context.Background(), "HR", "T", false)
	if err != nil || ddl != `CREATE TABLE "HR"."T" ("ID" INT)` {
		t.Errorf("CLOB 形式的 DDL: %q %v", ddl, err)
	}
}

func TestCreateStatementFallback(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "DBMS_METADATA", err: errors.New("[CODE:-4000]没有权限")})
	s.add(fakeRule{match: "SP_TABLEDEF", err: errors.New("[CODE:-2106]无效的函数名")})
	s.add(fakeRule{match: "char_length, char_used", cols: []string{"COLUMN_NAME", "DATA_TYPE", "DATA_LENGTH", "DATA_PRECISION", "DATA_SCALE", "NULLABLE", "COLUMN_ID", "CHAR_LENGTH", "CHAR_USED"},
		rows: [][]driver.Value{
			{"ID", "INT", int32(4), nil, nil, "N", int32(1), int32(0), nil},
			{"NAME", "VARCHAR", int32(200), nil, nil, "Y", int32(2), int32(50), "C"},
		}})
	s.add(fakeRule{match: "constraint_type = 'P'", cols: []string{"COLUMN_NAME"}, rows: [][]driver.Value{{"ID"}}})
	e := newFakeEngine(t, s)
	ddl, err := e.CreateStatement(context.Background(), "HR", "EMP", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"近似的建表语句", `CREATE TABLE "HR"."EMP"`, `"ID" INT NOT NULL`, `"NAME" VARCHAR(50)`, `PRIMARY KEY ("ID")`} {
		if !strings.Contains(ddl, sub) {
			t.Errorf("近似 DDL 缺少 %q：\n%s", sub, ddl)
		}
	}
	// 视图读不到定义：没有近似可用，直接报错
	if _, err := e.CreateStatement(context.Background(), "HR", "V", true); err == nil {
		t.Error("读不到视图定义应报错")
	}
}

func TestCreateStatementSpTabledef(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "DBMS_METADATA", err: errors.New("[CODE:-2106]无效的包名")})
	s.add(fakeRule{match: "SP_TABLEDEF('HR', 'EMP')", cols: []string{"DDL"}, rows: [][]driver.Value{{"CREATE TABLE \"HR\".\"EMP\"(\"ID\" INT)"}}})
	e := newFakeEngine(t, s)
	ddl, err := e.CreateStatement(context.Background(), "HR", "EMP", false)
	if err != nil || !strings.HasPrefix(ddl, "CREATE TABLE") {
		t.Errorf("退到 SP_TABLEDEF: %q %v", ddl, err)
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
		{"NUMBER", i(0), nil, nil, nil, "", "NUMBER"},
		{"NUMBER", i(0), i(10), i(0), nil, "", "NUMBER(10)"},
		{"DECIMAL", i(0), i(10), i(2), nil, "", "DECIMAL(10,2)"},
		{"INT", i(4), nil, nil, nil, "", "INT"},
		{"BIGINT", i(8), nil, nil, nil, "", "BIGINT"},
		{"FLOAT", i(8), i(53), nil, nil, "", "FLOAT(53)"},
		{"DOUBLE", i(8), nil, nil, nil, "", "DOUBLE"},
		{"VARCHAR", i(200), nil, nil, i(50), "C", "VARCHAR(50)"},
		{"VARCHAR", i(50), nil, nil, nil, "", "VARCHAR(50)"},
		{"VARCHAR2", i(50), nil, nil, i(50), "B", "VARCHAR2(50)"},
		{"CHAR", i(3), nil, nil, nil, "", "CHAR(3)"},
		{"VARBINARY", i(16), nil, nil, nil, "", "VARBINARY(16)"},
		{"DATE", i(3), nil, nil, nil, "", "DATE"},
		{"DATE", i(3), nil, i(0), nil, "", "DATE"},
		{"TIMESTAMP", i(8), nil, i(6), nil, "", "TIMESTAMP(6)"},
		{"DATETIME", i(8), nil, i(3), nil, "", "DATETIME(3)"},
		{"TIME", i(5), nil, i(0), nil, "", "TIME(0)"},
		{"TIMESTAMP(6) WITH TIME ZONE", i(13), nil, i(6), nil, "", "TIMESTAMP(6) WITH TIME ZONE"},
		{"CLOB", i(4000), nil, nil, nil, "", "CLOB"},
		{"TEXT", i(2147483647), nil, nil, nil, "", "TEXT"},
	} {
		if got := formatType(c.dt, c.length, c.prec, c.scale, c.charLen, c.used); got != c.want {
			t.Errorf("%s：%q，期望 %q", c.dt, got, c.want)
		}
	}
}

func TestAsIntAndAsString(t *testing.T) {
	type dec struct{ s string } // 模拟 *DmDecimal：有 String 方法
	for _, c := range []struct {
		in   any
		want int64
		ok   bool
	}{
		{int64(7), 7, true}, {int32(7), 7, true}, {int16(7), 7, true}, {int8(7), 7, true}, {uint8(7), 7, true}, {7, 7, true},
		{float64(7), 7, true}, {"7", 7, true}, {"7.0", 7, true}, {[]byte("7"), 7, true}, {nil, 0, false}, {"x", 0, false},
		{decimalLike("107"), 107, true},
	} {
		got, ok := asInt(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("asInt(%#v) = %d,%v，期望 %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
	_ = dec{}
	if asString(nil) != "" || asString([]byte("a")) != "a" || asString(decimalLike("1.50")) != "1.50" || asString(int32(3)) != "3" {
		t.Error("asString 基本转换")
	}
	if asString(fakeClob{"你好，达梦"}) != "你好，达梦" {
		t.Error("asString 要能读 CLOB")
	}
}

type decimalLike string

func (d decimalLike) String() string { return string(d) }

func TestQueryViaEngineInterface(t *testing.T) {
	s := empScript()
	s.add(fakeRule{match: "from employees", cols: []string{"ID"}, types: []string{"INT"}, rows: [][]driver.Value{{int32(1)}, {int32(2)}, {int32(3)}}})
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
		t.Errorf("dm 引擎应注册到 engine 注册表: %v", engine.Supported())
	}
	if _, err := engine.New(DBType, engine.Conn{Host: "h", User: "u"}); err != nil {
		t.Error(err)
	}
}
