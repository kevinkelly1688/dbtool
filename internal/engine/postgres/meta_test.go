// @author: kevinkelly
// @license: Apache Licence
// @file: meta_test.go
// @time: 2026/10/28

package postgres

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestMetadataBasics(t *testing.T) {
	var gotParams [][]string
	h := func(sql string, params []string) *fakeResult {
		switch {
		case strings.Contains(sql, "SHOW server_version"):
			return &fakeResult{Cols: textCols("server_version"), Rows: [][]*string{cell("16.2 (Debian 16.2-1.pgdg120+2)")}}
		case strings.Contains(sql, "FROM pg_catalog.pg_database"):
			return &fakeResult{Cols: textCols("datname"), Rows: [][]*string{cell("appdb"), cell("postgres")}}
		case strings.Contains(sql, "n.nspname || '.' || c.relname"):
			return &fakeResult{Cols: textCols("t"), Rows: [][]*string{cell("public.orders"), cell("public.users"), cell("reporting.daily")}}
		case strings.Contains(sql, "SELECT a.attname"):
			gotParams = append(gotParams, params)
			return &fakeResult{Cols: textCols("attname"), Rows: [][]*string{cell("id"), cell("name")}}
		}
		return nil
	}
	fs := newFakeServer(t, h)
	e := newEngine(t, fs, "u", "", "appdb")
	ctx := context.Background()

	v, err := e.ServerVersion(ctx)
	if err != nil || v != "16.2 (Debian 16.2-1.pgdg120+2)" {
		t.Fatalf("%q %v", v, err)
	}
	dbs, err := e.AllDatabases(ctx)
	if err != nil || !reflect.DeepEqual(dbs, []string{"appdb", "postgres"}) {
		t.Fatalf("%v %v", dbs, err)
	}
	tabs, err := e.AllTables(ctx, "appdb")
	if err != nil || !reflect.DeepEqual(tabs, []string{"public.orders", "public.users", "reporting.daily"}) {
		t.Fatalf("%v %v", tabs, err)
	}
	cols, err := e.AllColumns(ctx, "appdb", "reporting.daily")
	if err != nil || !reflect.DeepEqual(cols, []string{"id", "name"}) {
		t.Fatalf("%v %v", cols, err)
	}
	if _, err := e.AllColumns(ctx, "appdb", "orders"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AllColumns(ctx, "appdb", `odd.name.with.dots`); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"reporting", "daily"}, {"public", "orders"}, {"odd", "name.with.dots"}}
	if !reflect.DeepEqual(gotParams, want) {
		t.Fatalf("参数应走绑定变量、按第一个点切分，不拼进 SQL：%v", gotParams)
	}
	// 排除系统库 / 系统 schema 的过滤条件在 SQL 里
	var dbSQL, tabSQL string
	for _, s := range fs.statements() {
		if strings.Contains(s, "pg_database") {
			dbSQL = s
		}
		if strings.Contains(s, "n.nspname || '.'") {
			tabSQL = s
		}
	}
	for _, sub := range []string{"template0", "template1", "datallowconn", "has_database_privilege"} {
		if !strings.Contains(dbSQL, sub) {
			t.Errorf("AllDatabases 的 SQL 缺少 %q", sub)
		}
	}
	for _, sub := range []string{"information_schema", "!~ '^pg_'", "has_any_column_privilege", "relispartition"} {
		if !strings.Contains(tabSQL, sub) {
			t.Errorf("AllTables 的 SQL 缺少 %q", sub)
		}
	}
	// 元数据查询同样在只读事务里
	if !fs.has("BEGIN READ ONLY") || !fs.has("ROLLBACK") {
		t.Error("元数据查询应在只读事务里执行")
	}
}

func TestTableInfos(t *testing.T) {
	h := routes(route{"pg_table_size", &fakeResult{Cols: textCols("a", "b", "c", "d", "e", "f", "g", "h", "i"), Rows: [][]*string{
		cell("public", "orders", "r", "heap", "订单表", "app", "12345", "8192000", "1024000"),
		cell("public", "v_orders", "v", "", "", "app", "-1", "0", "0"),
		cell("reporting", "daily", "p", "", nullCell, "app", "0", "0", "16384"),
	}}})
	fs := newFakeServer(t, h)
	e := newEngine(t, fs, "u", "", "appdb")
	infos, err := e.TableInfos(context.Background(), "appdb")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 {
		t.Fatalf("%+v", infos)
	}
	o := infos[0]
	if o.FullName != "public.orders" || o.Type != "BASE TABLE" || o.Engine != "heap" || o.Comment != "订单表" || o.Owner != "app" ||
		o.RowsEstimate != 12345 || o.DataBytes != 8192000 || o.IndexBytes != 1024000 || o.CreateTime != nil || o.UpdateTime != nil {
		t.Errorf("%+v", o)
	}
	if infos[1].Type != "VIEW" || infos[1].RowsEstimate != -1 {
		t.Errorf("%+v", infos[1])
	}
	if infos[2].Type != "PARTITIONED TABLE" || infos[2].Comment != "" {
		t.Errorf("%+v", infos[2])
	}
}

func TestColumnInfos(t *testing.T) {
	var gotParams []string
	h := func(sql string, params []string) *fakeResult {
		if strings.Contains(sql, "attidentity") {
			gotParams = params
			return &fakeResult{Cols: textCols("a", "b", "c", "d", "e", "f", "g", "h", "i"), Rows: [][]*string{
				cell("1", "id", "bigint", "false", nullCell, "主键", "1", "d", ""),
				cell("2", "tenant", "integer", "false", nullCell, "", "2", "", ""),
				cell("3", "name", "character varying(64)", "true", "'anon'::character varying", "", "0", "", ""),
				cell("4", "total", "numeric(10,2)", "true", "(price * qty)", "", "0", "", "s"),
				cell("5", "seq", "integer", "false", nullCell, "", "0", "a", ""),
			}}
		}
		return nil
	}
	fs := newFakeServer(t, h)
	e := newEngine(t, fs, "u", "", "appdb")
	cols, err := e.ColumnInfos(context.Background(), "appdb", "public.orders")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotParams, []string{"public", "orders"}) {
		t.Fatalf("%v", gotParams)
	}
	if len(cols) != 5 {
		t.Fatalf("%+v", cols)
	}
	c0 := cols[0]
	if c0.Name != "id" || c0.Position != 1 || c0.Type != "bigint" || c0.Nullable || c0.Default != nil || c0.Comment != "主键" || !c0.IsPrimaryKey || c0.PKOrdinal != 1 || c0.Identity != "by default" {
		t.Errorf("%+v", c0)
	}
	if !cols[1].IsPrimaryKey || cols[1].PKOrdinal != 2 {
		t.Errorf("%+v", cols[1])
	}
	if c := cols[2]; !c.Nullable || c.Default == nil || *c.Default != "'anon'::character varying" || c.IsPrimaryKey {
		t.Errorf("%+v", c)
	}
	if cols[3].Generated != "stored" || cols[4].Identity != "always" {
		t.Errorf("%+v %+v", cols[3], cols[4])
	}
}

func TestIndexes(t *testing.T) {
	h := routes(route{"pg_get_indexdef(i.indexrelid)", &fakeResult{Cols: textCols("a", "b", "c", "d", "e", "f", "g", "h"), Rows: [][]*string{
		cell("orders_pkey", "true", "true", "true", "btree", "CREATE UNIQUE INDEX orders_pkey ON public.orders USING btree (id)", "16384", `["id"]`),
		cell("orders_expr_idx", "false", "false", "false", "btree", "CREATE INDEX orders_expr_idx ON public.orders USING btree (lower(name), tenant)", "8192", `["lower((name)::text)","tenant"]`),
	}}})
	fs := newFakeServer(t, h)
	e := newEngine(t, fs, "u", "", "appdb")
	idx, err := e.Indexes(context.Background(), "appdb", "public.orders")
	if err != nil || len(idx) != 2 {
		t.Fatalf("%v %+v", err, idx)
	}
	if i := idx[0]; i.Name != "orders_pkey" || !i.Unique || !i.Primary || !i.Valid || i.Method != "btree" || i.SizeBytes != 16384 || !reflect.DeepEqual(i.Columns, []string{"id"}) {
		t.Errorf("%+v", i)
	}
	if i := idx[1]; i.Unique || i.Valid || !reflect.DeepEqual(i.Columns, []string{"lower((name)::text)", "tenant"}) {
		t.Errorf("%+v", i)
	}
}

func TestCreateStatementTable(t *testing.T) {
	h := func(sql string, params []string) *fakeResult {
		switch {
		case strings.Contains(sql, "c.relkind::text, pg_catalog.quote_ident(n.nspname)"):
			if len(params) != 2 || params[0] != "public" || params[1] != "orders" {
				return pgErr("XX000", "bad params")
			}
			return &fakeResult{Cols: textCols("a", "b", "c", "d", "e", "f", "g", "h", "i", "j"), Rows: [][]*string{
				cell("16385", "p", "public", "orders", "订单'表", "{fillfactor=70}", "RANGE (created_at)", "", "", ""),
			}}
		case strings.Contains(sql, "FROM pg_catalog.pg_attribute a LEFT JOIN"):
			return &fakeResult{Cols: textCols("a", "b", "c", "d", "e", "f", "g"), Rows: [][]*string{
				cell("id", "bigint", "true", "", "d", "", "主键"),
				cell("name", "character varying(64)", "false", "'anon'::character varying", "", "", ""),
				cell("total", "numeric(10,2)", "false", "(price * qty)", "", "s", ""),
				cell("created_at", "timestamp with time zone", "true", "now()", "", "", "创建时间"),
			}}
		case strings.Contains(sql, "pg_get_constraintdef"):
			return &fakeResult{Cols: textCols("a", "b"), Rows: [][]*string{
				cell("orders_pkey", "PRIMARY KEY (id, created_at)"),
				cell("orders_total_check", "CHECK (total >= 0::numeric)"),
				cell("orders_user_fk", "FOREIGN KEY (name) REFERENCES users(name)"),
			}}
		case strings.Contains(sql, "pg_get_indexdef(i.indexrelid) FROM"):
			return &fakeResult{Cols: textCols("a"), Rows: [][]*string{cell("CREATE INDEX orders_name_idx ON ONLY public.orders USING btree (name)")}}
		}
		return nil
	}
	fs := newFakeServer(t, h)
	e := newEngine(t, fs, "u", "", "appdb")
	ddl, err := e.CreateStatement(context.Background(), "appdb", "public.orders")
	if err != nil {
		t.Fatal(err)
	}
	want := ApproxDDLNotice + `
CREATE TABLE public.orders (
    id bigint GENERATED BY DEFAULT AS IDENTITY NOT NULL,
    name character varying(64) DEFAULT 'anon'::character varying,
    total numeric(10,2) GENERATED ALWAYS AS ((price * qty)) STORED,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT orders_pkey PRIMARY KEY (id, created_at),
    CONSTRAINT orders_total_check CHECK (total >= 0::numeric),
    CONSTRAINT orders_user_fk FOREIGN KEY (name) REFERENCES users(name)
) PARTITION BY RANGE (created_at) WITH (fillfactor=70);
CREATE INDEX orders_name_idx ON ONLY public.orders USING btree (name);
COMMENT ON TABLE public.orders IS '订单''表';
COMMENT ON COLUMN public.orders.id IS '主键';
COMMENT ON COLUMN public.orders.created_at IS '创建时间';
`
	if ddl != want {
		t.Errorf("DDL 不一致：\n--- got ---\n%s\n--- want ---\n%s", ddl, want)
	}
	if !strings.HasPrefix(ddl, "-- 近似 DDL") {
		t.Error("应声明是近似 DDL")
	}
}

func TestCreateStatementViewAndMissing(t *testing.T) {
	h := func(sql string, params []string) *fakeResult {
		switch {
		case strings.Contains(sql, "c.relkind::text, pg_catalog.quote_ident(n.nspname)"):
			if params[1] == "nosuch" {
				return &fakeResult{Cols: textCols("a")}
			}
			return &fakeResult{Cols: textCols("a", "b", "c", "d", "e", "f", "g", "h", "i", "j"), Rows: [][]*string{
				cell("16390", "v", "public", "v_orders", "", "", "", "", "", " SELECT orders.id\n   FROM orders;"),
			}}
		case strings.Contains(sql, "pg_get_indexdef(i.indexrelid) FROM"):
			return &fakeResult{Cols: textCols("a")}
		}
		return nil
	}
	fs := newFakeServer(t, h)
	e := newEngine(t, fs, "u", "", "appdb")
	ddl, err := e.CreateStatement(context.Background(), "appdb", "v_orders")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "CREATE VIEW public.v_orders AS\nSELECT orders.id\n   FROM orders;\n") {
		t.Errorf("%s", ddl)
	}
	if _, err := e.CreateStatement(context.Background(), "appdb", "public.nosuch"); err == nil || !strings.Contains(err.Error(), "找不到") {
		t.Errorf("不存在的表应报错：%v", err)
	}
}

func TestQuoteLiteral(t *testing.T) {
	if got := quoteLiteral(`a'b''c`); got != `'a''b''''c'` {
		t.Fatal(got)
	}
}
