// @author: kevinkelly
// @license: Apache Licence
// @file: meta_test.go
// @time: 2026/10/28

package clickhouse

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestVersionAndTestConnection(t *testing.T) {
	f := newFake(t)
	f.onJC("SELECT version()", [][2]string{{"v", "String"}}, [][]any{{"24.3.2.23"}})
	e := f.engine()
	if err := e.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	v, err := e.ServerVersion(context.Background())
	if err != nil || v != "24.3.2.23" {
		t.Errorf("v=%q err=%v", v, err)
	}
	// 元数据请求同样带 readonly 与限额，且不带 database 参数（库名写在语句里）
	for _, r := range f.requests() {
		if r.Query.Get("readonly") != "1" || r.Query.Get("max_execution_time") == "" || r.Query.Get("max_result_rows") == "" || r.Query.Get("database") != "" {
			t.Errorf("元数据请求参数: %v", r.Query)
		}
	}
	// SELECT 1 必须恰好一行
	f2 := newFake(t)
	f2.onJC("SELECT 1", [][2]string{{"x", "UInt8"}}, nil)
	if err := f2.engine().TestConnection(context.Background()); err == nil {
		t.Error("空结果应判为失败")
	}
}

func TestAllDatabasesTablesColumns(t *testing.T) {
	f := newFake(t)
	f.onJC("FROM system.databases", [][2]string{{"name", "String"}}, [][]any{{"app"}, {"default"}})
	f.onJC("FROM system.tables WHERE database", [][2]string{{"name", "String"}}, [][]any{{"a"}, {"b"}})
	f.onJC("FROM system.columns", [][2]string{{"name", "String"}}, [][]any{{"id"}, {"v"}})
	e := f.engine()
	dbs, err := e.AllDatabases(context.Background())
	if err != nil || !reflect.DeepEqual(dbs, []string{"app", "default"}) {
		t.Errorf("dbs=%v err=%v", dbs, err)
	}
	tbs, err := e.AllTables(context.Background(), "app")
	if err != nil || !reflect.DeepEqual(tbs, []string{"a", "b"}) {
		t.Errorf("tbs=%v err=%v", tbs, err)
	}
	cols, err := e.AllColumns(context.Background(), "app", "a")
	if err != nil || !reflect.DeepEqual(cols, []string{"id", "v"}) {
		t.Errorf("cols=%v err=%v", cols, err)
	}
	reqs := f.requests()
	if !strings.Contains(reqs[0].Body, "NOT IN ('system', 'INFORMATION_SCHEMA', 'information_schema')") {
		t.Errorf("AllDatabases 语句: %s", reqs[0].Body)
	}
	if !strings.Contains(reqs[1].Body, "database = 'app'") || !strings.Contains(reqs[2].Body, "database = 'app' AND table = 'a'") {
		t.Errorf("语句: %s | %s", reqs[1].Body, reqs[2].Body)
	}
	// 名字里的引号被转义；非法名字不发请求
	n := len(f.requests())
	_, _ = e.AllTables(context.Background(), "a'b")
	if b := f.requests()[n].Body; !strings.Contains(b, `database = 'a\'b'`) {
		t.Errorf("没有转义: %s", b)
	}
	for _, bad := range []string{"", "a\nb", "a\x00b", strings.Repeat("x", 600)} {
		if _, err := e.AllTables(context.Background(), bad); err == nil {
			t.Errorf("库名 %q 应被拒绝", bad)
		}
		if _, err := e.AllColumns(context.Background(), "app", bad); err == nil {
			t.Errorf("表名 %q 应被拒绝", bad)
		}
	}
	if len(f.requests()) != n+1 {
		t.Errorf("非法名字不应发出请求")
	}
}

func TestTableInfos(t *testing.T) {
	f := newFake(t)
	f.onJC("FROM system.tables WHERE database", [][2]string{{"name", "String"}, {"engine", "String"}, {"engine_full", "String"}, {"comment", "String"},
		{"total_rows", "Nullable(UInt64)"}, {"total_bytes", "Nullable(UInt64)"}, {"mtime", "UInt32"}, {"partition_key", "String"}, {"sorting_key", "String"},
		{"primary_key", "String"}, {"sampling_key", "String"}},
		[][]any{
			{"events", "MergeTree", "MergeTree PARTITION BY toYYYYMM(d) ORDER BY (a, b)", "事件表", "123456789012", "5000000000", raw("1700000000"), "toYYYYMM(d)", "a, b", "a, b", ""},
			{"v1", "View", "", "", nil, nil, raw("1700000100"), "", "", "", ""},
		})
	f.onJC("FROM system.parts", [][2]string{{"t", "String"}, {"m", "UInt64"}, {"p", "UInt64"}}, [][]any{{"events", "1000", "234"}, {"gone", "1", "1"}})
	e := f.engine()
	infos, err := e.TableInfos(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("infos = %+v", infos)
	}
	a := infos[0]
	if a.Name != "events" || a.Engine != "MergeTree" || a.Comment != "事件表" || a.Rows == nil || *a.Rows != 123456789012 || a.DataBytes == nil || *a.DataBytes != 5000000000 ||
		a.IndexBytes == nil || *a.IndexBytes != 1234 || a.UpdateTime == nil || !a.UpdateTime.Equal(time.Unix(1700000000, 0)) || a.CreateTime != nil ||
		a.PartitionKey != "toYYYYMM(d)" || a.SortingKey != "a, b" || a.PrimaryKey != "a, b" || !strings.HasPrefix(a.EngineFull, "MergeTree PARTITION") {
		t.Errorf("events = %+v", a)
	}
	v := infos[1]
	if v.Rows != nil || v.DataBytes != nil || v.IndexBytes != nil || v.Engine != "View" {
		t.Errorf("v1 = %+v", v)
	}
	// system.parts 不可用时只丢失索引大小
	f2 := newFake(t)
	f2.onJC("FROM system.tables WHERE database", [][2]string{{"name", "String"}, {"engine", "String"}}, [][]any{{"events", "MergeTree"}})
	f2.on(contains("FROM system.parts"), chError(497, "Not enough privileges. (ACCESS_DENIED)"))
	infos, err = f2.engine().TableInfos(context.Background(), "app")
	if err != nil || len(infos) != 1 || infos[0].IndexBytes != nil {
		t.Errorf("infos=%+v err=%v", infos, err)
	}
}

func TestColumnInfos(t *testing.T) {
	f := newFake(t)
	f.onJC("SELECT partition_key, sorting_key, primary_key FROM system.tables", [][2]string{{"partition_key", "String"}, {"sorting_key", "String"}, {"primary_key", "String"}},
		[][]any{{"toYYYYMM(d)", "a, `b c`, toDate(d)", "a, `b c`"}})
	f.onJC("FROM system.columns", [][2]string{{"name", "String"}, {"type", "String"}, {"default_kind", "String"}, {"default_expression", "String"}, {"comment", "String"},
		{"is_in_partition_key", "UInt8"}, {"is_in_sorting_key", "UInt8"}, {"is_in_primary_key", "UInt8"}, {"is_in_sampling_key", "UInt8"}, {"compression_codec", "String"}, {"position", "UInt64"}},
		[][]any{
			{"a", "UInt64", "", "", "主键 a", 0, 1, 1, 0, "", "1"},
			{"b c", "LowCardinality(Nullable(String))", "DEFAULT", "'x'", "", 0, 1, 1, 0, "CODEC(ZSTD(3))", "2"},
			{"d", "Date", "", "", "", 1, 1, 0, 0, "", "3"},
			{"n", "Nullable(Int8)", "MATERIALIZED", "1 + 1", "", 0, 0, 0, 0, "", "4"},
		})
	e := f.engine()
	cols, err := e.ColumnInfos(context.Background(), "app", "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 4 {
		t.Fatalf("cols = %+v", cols)
	}
	a, b, d, n := cols[0], cols[1], cols[2], cols[3]
	if !a.IsPrimaryKey || a.KeyOrdinal != 1 || a.Nullable || a.Comment != "主键 a" || !a.IsSortingKey || a.Position != 1 {
		t.Errorf("a = %+v", a)
	}
	if !b.IsPrimaryKey || b.KeyOrdinal != 2 || !b.Nullable || b.DefaultKind != "DEFAULT" || b.Default != "'x'" || b.Codec != "CODEC(ZSTD(3))" {
		t.Errorf("b = %+v", b)
	}
	if d.IsPrimaryKey || d.KeyOrdinal != 0 || !d.IsPartitionKey || !d.IsSortingKey {
		t.Errorf("d = %+v", d)
	}
	if !n.Nullable || n.DefaultKind != "MATERIALIZED" {
		t.Errorf("n = %+v", n)
	}
	// 表不存在
	f2 := newFake(t)
	f2.onJC("FROM system.tables", [][2]string{{"partition_key", "String"}, {"sorting_key", "String"}, {"primary_key", "String"}}, nil)
	if _, err := f2.engine().ColumnInfos(context.Background(), "app", "nope"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Errorf("err = %v", err)
	}
}

func TestIndexes(t *testing.T) {
	f := newFake(t)
	f.onJC("SELECT partition_key, sorting_key, primary_key FROM system.tables", [][2]string{{"partition_key", "String"}, {"sorting_key", "String"}, {"primary_key", "String"}},
		[][]any{{"", "a, b, c", "a"}})
	f.onJC("FROM system.data_skipping_indices", [][2]string{{"name", "String"}, {"type", "String"}, {"expr", "String"}, {"granularity", "UInt64"}},
		[][]any{{"idx_b", "bloom_filter(0.01)", "b", "4"}, {"idx_c", "minmax", "c", "1"}})
	e := f.engine()
	idx, err := e.Indexes(context.Background(), "app", "t")
	if err != nil {
		t.Fatal(err)
	}
	want := []IndexInfo{
		{Name: "PRIMARY", Kind: "primary", Expression: "a", Columns: []string{"a"}},
		{Name: "ORDER BY", Kind: "sorting", Expression: "a, b, c", Columns: []string{"a", "b", "c"}},
		{Name: "idx_b", Kind: "skipping", Type: "bloom_filter(0.01)", Expression: "b", Granularity: 4},
		{Name: "idx_c", Kind: "skipping", Type: "minmax", Expression: "c", Granularity: 1},
	}
	if !reflect.DeepEqual(idx, want) {
		t.Errorf("idx = %+v", idx)
	}
	// 主键未显式指定（等于排序键）：只列一条
	f2 := newFake(t)
	f2.onJC("SELECT partition_key, sorting_key, primary_key FROM system.tables", [][2]string{{"partition_key", "String"}, {"sorting_key", "String"}, {"primary_key", "String"}},
		[][]any{{"", "id", ""}})
	f2.onJC("FROM system.data_skipping_indices", [][2]string{{"name", "String"}}, nil)
	idx, err = f2.engine().Indexes(context.Background(), "app", "t")
	if err != nil || len(idx) != 1 || idx[0].Name != "PRIMARY" || !reflect.DeepEqual(idx[0].Columns, []string{"id"}) {
		t.Errorf("idx=%+v err=%v", idx, err)
	}
}

func TestCreateStatement(t *testing.T) {
	f := newFake(t)
	f.onJC("create_table_query AS q", [][2]string{{"q", "String"}}, [][]any{{"CREATE TABLE app.t\n(\n    `id` UInt64\n)\nENGINE = MergeTree\nORDER BY id"}})
	e := f.engine()
	q, err := e.CreateStatement(context.Background(), "app", "t")
	if err != nil || !strings.HasPrefix(q, "CREATE TABLE app.t") || !strings.Contains(q, "\n") {
		t.Errorf("q=%q err=%v", q, err)
	}
	f2 := newFake(t)
	f2.onJC("create_table_query AS q", [][2]string{{"q", "String"}}, nil)
	if _, err := f2.engine().CreateStatement(context.Background(), "app", "nope"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Errorf("err = %v", err)
	}
	f3 := newFake(t)
	f3.onJC("create_table_query AS q", [][2]string{{"q", "String"}}, [][]any{{""}})
	if _, err := f3.engine().CreateStatement(context.Background(), "app", "t"); err == nil {
		t.Error("空 DDL 应报错")
	}
}

func TestMetaTruncatedIsError(t *testing.T) {
	f := newFake(t)
	old := metaLimits
	metaLimits.MaxRows = 2
	defer func() { metaLimits = old }()
	f.onJC("FROM system.databases", [][2]string{{"name", "String"}}, [][]any{{"a"}, {"b"}, {"c"}, {"d"}})
	if _, err := f.engine().AllDatabases(context.Background()); err == nil || !strings.Contains(err.Error(), "过大") {
		t.Errorf("err = %v", err)
	}
}

func TestSplitTopLevelAndKeys(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a, b", []string{"a", "b"}},
		{"toYYYYMM(d), a", []string{"toYYYYMM(d)", "a"}},
		{"cityHash64(a, b), c", []string{"cityHash64(a, b)", "c"}},
		{"`a,b`, c", []string{"`a,b`", "c"}},
		{"'x,y', c", []string{"'x,y'", "c"}},
		{"(a, b)", []string{"(a, b)"}},
		{"", nil},
	}
	for _, c := range cases {
		if got := splitTopLevel(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitTopLevel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	pk := keyPositions("a, `b c`, toDate(d), \"e\"")
	if pk["a"] != 1 || pk["b c"] != 2 || pk["e"] != 4 || pk["d"] != 0 || len(pk) != 3 {
		t.Errorf("keyPositions = %v", pk)
	}
}
