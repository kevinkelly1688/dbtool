// @author: kevinkelly
// @license: Apache Licence
// @file: engines_test.go
// @time: 2026/10/28

package dict

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"dbtool/internal/engine"
	"dbtool/internal/engine/clickhouse"
	mongox "dbtool/internal/engine/mongo"
	"dbtool/internal/engine/mongo/mongotest"
	"dbtool/internal/engine/postgres"
	"dbtool/internal/engine/postgres/pgtest"
	"dbtool/internal/model"
)

// PostgreSQL / MongoDB / ClickHouse 的数据字典：用真实的引擎包连进程内假后端（pgtest / mongotest / httptest）。

// rvOn 给实例打上旧标签 read_view：该标签只是 can_read 的同义词，不再限制任何访问。
func rvOn(inst model.Instance) model.Instance { inst.Tags = rvTag(); return inst }

type chRoute struct {
	sub  string
	cols [][2]string
	rows [][]any
}

// newCH 启动一个按语句片段返回预置结果的 ClickHouse HTTP 假服务。
func newCH(t *testing.T, routes ...chRoute) engine.Conn {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		mu.Lock()
		defer mu.Unlock()
		cols, rows := [][2]string{{"x", "UInt8"}}, [][]any{{1}}
		for _, rt := range routes {
			if strings.Contains(body, rt.sub) {
				cols, rows = rt.cols, rt.rows
				break
			}
		}
		meta := make([]map[string]string, len(cols))
		for i, c := range cols {
			meta[i] = map[string]string{"name": c[0], "type": c[1]}
		}
		if rows == nil {
			rows = [][]any{}
		}
		out, _ := json.Marshal(map[string]any{"meta": meta, "data": rows, "rows": len(rows), "statistics": map[string]any{"elapsed": 0.001, "rows_read": 1, "bytes_read": 1}})
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	host, portS, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portS)
	return engine.Conn{Host: host, Port: port, User: "reader", Password: "pw"}
}

func pgDictHandler(sql string, params []string) *pgtest.Result {
	switch {
	case strings.Contains(sql, "FROM pg_catalog.pg_database"):
		return &pgtest.Result{Cols: pgtest.TextCols("datname"), Rows: [][]*string{pgtest.Cell("appdb"), pgtest.Cell("postgres")}}
	case strings.Contains(sql, "pg_table_size"):
		return &pgtest.Result{Cols: pgtest.TextCols("a", "b", "c", "d", "e", "f", "g", "h", "i"), Rows: [][]*string{
			pgtest.Cell("public", "orders", "r", "heap", "订单表", "app", "12345", "8192000", "1024000"),
			pgtest.Cell("public", "v_orders", "v", "", "", "app", "-1", "0", "0"),
			pgtest.Cell("reporting", "daily", "p", "", pgtest.NullCell, "app", "0", "0", "16384"),
		}}
	case strings.Contains(sql, "attidentity"):
		return &pgtest.Result{Cols: pgtest.TextCols("a", "b", "c", "d", "e", "f", "g", "h", "i"), Rows: [][]*string{
			pgtest.Cell("1", "id", "bigint", "false", pgtest.NullCell, "主键", "1", "d", ""),
			pgtest.Cell("2", "name", "character varying(64)", "true", "'anon'::character varying", "", "0", "", ""),
		}}
	case strings.Contains(sql, "pg_get_indexdef(i.indexrelid)") && strings.Contains(sql, "indisprimary"):
		return &pgtest.Result{Cols: pgtest.TextCols("a", "b", "c", "d", "e", "f", "g", "h"), Rows: [][]*string{
			pgtest.Cell("orders_pkey", "true", "true", "true", "btree", "CREATE UNIQUE INDEX orders_pkey ON public.orders USING btree (id)", "16384", `["id"]`),
		}}
	case strings.Contains(sql, "c.relkind::text, pg_catalog.quote_ident(n.nspname)"):
		return &pgtest.Result{Cols: pgtest.TextCols("a", "b", "c", "d", "e", "f", "g", "h", "i", "j"), Rows: [][]*string{
			pgtest.Cell("16385", "r", "public", "orders", "订单表", "", "", "", "", ""),
		}}
	case strings.Contains(sql, "FROM pg_catalog.pg_attribute a LEFT JOIN"):
		return &pgtest.Result{Cols: pgtest.TextCols("a", "b", "c", "d", "e", "f", "g"), Rows: [][]*string{
			pgtest.Cell("id", "bigint", "true", "", "", "", "主键"),
		}}
	}
	return nil
}

type engEnv struct {
	svc *Service
	pg  *pgtest.Server
	mgo *mongotest.Fake
	chc engine.Conn
}

func newEngEnv(t *testing.T) *engEnv {
	t.Helper()
	e := &engEnv{pg: pgtest.New(t, pgDictHandler), mgo: mongotest.New()}
	app := e.mgo.DB("app")
	u := app.Coll("users",
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "name", Value: "a"}, {Key: "age", Value: int32(30)}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "name", Value: "b"}, {Key: "email", Value: "x@y"}},
	)
	u.StatDoc = bson.D{{Key: "count", Value: int32(2)}, {Key: "size", Value: int32(300)}, {Key: "storageSize", Value: int64(4096)}, {Key: "totalIndexSize", Value: int64(8192)}, {Key: "nindexes", Value: int32(2)}}
	u.IndexDoc = []bson.D{
		{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "_id", Value: int32(1)}}}, {Key: "name", Value: "_id_"}},
		{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "email", Value: int32(1)}}}, {Key: "name", Value: "email_1"}, {Key: "unique", Value: true}, {Key: "expireAfterSeconds", Value: int32(60)}},
	}
	app.Infos = append(app.Infos, mongox.CollectionInfo{Name: "v1", Type: "view", Options: bson.D{{Key: "viewOn", Value: "users"}}})
	e.mgo.DB("other").Coll("secrets")

	e.chc = newCH(t,
		chRoute{"FROM system.databases", [][2]string{{"name", "String"}}, [][]any{{"app"}, {"default"}}},
		chRoute{"SELECT partition_key, sorting_key, primary_key FROM system.tables", [][2]string{{"partition_key", "String"}, {"sorting_key", "String"}, {"primary_key", "String"}}, [][]any{{"toYYYYMM(dt)", "id, dt", "id"}}},
		chRoute{"FROM system.data_skipping_indices", [][2]string{{"name", "String"}, {"type", "String"}, {"expr", "String"}, {"granularity", "UInt64"}}, [][]any{{"idx_n", "minmax", "n", "4"}}},
		chRoute{"create_table_query", [][2]string{{"q", "String"}}, [][]any{{"CREATE TABLE app.events (id UInt64, dt Date) ENGINE = MergeTree ORDER BY id"}}},
		chRoute{"FROM system.parts", [][2]string{{"t", "String"}, {"m", "UInt64"}, {"p", "UInt64"}}, [][]any{{"events", "100", "28"}}},
		chRoute{"FROM system.columns", [][2]string{{"name", "String"}, {"type", "String"}, {"default_kind", "String"}, {"default_expression", "String"}, {"comment", "String"},
			{"is_in_partition_key", "UInt8"}, {"is_in_sorting_key", "UInt8"}, {"is_in_primary_key", "UInt8"}, {"is_in_sampling_key", "UInt8"}, {"compression_codec", "String"}, {"position", "UInt64"}},
			[][]any{{"id", "UInt64", "", "", "主键", 0, 1, 1, 0, "", 1}, {"dt", "Date", "DEFAULT", "today()", "", 1, 0, 0, 0, "CODEC(ZSTD(1))", 2}}},
		chRoute{"FROM system.tables WHERE database", [][2]string{{"name", "String"}, {"engine", "String"}, {"engine_full", "String"}, {"comment", "String"},
			{"total_rows", "Nullable(UInt64)"}, {"total_bytes", "Nullable(UInt64)"}, {"mtime", "UInt32"}, {"partition_key", "String"}, {"sorting_key", "String"}, {"primary_key", "String"}, {"sampling_key", "String"}},
			[][]any{{"events", "MergeTree", "MergeTree ORDER BY id", "事件", "100", "2048", 1700000000, "toYYYYMM(dt)", "id", "id", ""},
				{"v_events", "View", "View", "", nil, nil, 1700000000, "", "", "", ""},
				{"dict1", "Dictionary", "Dictionary", "", nil, nil, 0, "", "", "", ""}}},
	)
	src := &fakeSource{instances: []model.Instance{
		{ID: 21, InstanceName: "pg-01", DBType: "pgsql"},
		{ID: 22, InstanceName: "mongo-01", DBType: "mongo"},
		{ID: 23, InstanceName: "ch-01", DBType: "clickhouse"},
		rvOn(model.Instance{ID: 24, InstanceName: "rv-pg", DBType: "pgsql"}),
		{ID: 25, InstanceName: "filtered-mongo", DBType: "mongo", DeniedDBNameRegex: "^other"},
	}}
	access := &fakeAccess{allowed: map[uint]map[uint]bool{2: {21: true, 22: true, 23: true}}}
	open := func(_ context.Context, dbType string, _ engine.Conn, _ string) (Querier, error) {
		switch dbType {
		case "pgsql":
			eng, err := postgres.New(e.pg.Conn("u", "", "appdb"))
			if err != nil {
				return nil, err
			}
			return NewEngineQuerier(eng), nil
		case "mongo":
			return NewEngineQuerier(mongox.NewWithBackend(e.mgo)), nil
		case "clickhouse":
			eng, err := clickhouse.NewEngine(e.chc)
			if err != nil {
				return nil, err
			}
			return NewEngineQuerier(eng), nil
		}
		return DefaultOpener(context.Background(), dbType, engine.Conn{}, "")
	}
	e.svc = NewService(src, access, open)
	return e
}

var ctxb = context.Background()

func TestEngineInstancesAreSupported(t *testing.T) {
	e := newEngEnv(t)
	all, err := e.svc.Instances(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]InstanceView{}
	for _, v := range all {
		got[v.Name] = v
	}
	for _, n := range []string{"pg-01", "mongo-01", "ch-01"} {
		v := got[n]
		if !v.Supported || len(v.Kinds) != 2 || v.Kinds[0] != KindTable || v.Kinds[1] != KindView {
			t.Errorf("%s: %+v", n, v)
		}
	}
	// 带旧标签 read_view 的实例与普通实例一样：类别、库列表、对象列表都不受限制
	if v := got["rv-pg"]; !v.Supported || len(v.Kinds) != 2 {
		t.Errorf("rv-pg: %+v", v)
	}
	if dbs, err := e.svc.Databases(ctxb, root, 24); err != nil || len(dbs) == 0 {
		t.Errorf("带旧标签 read_view 的实例库列表: %v %v", dbs, err)
	}
	if items, err := e.svc.Objects(ctxb, root, 24, "appdb", KindTable); err != nil || len(items) == 0 {
		t.Errorf("带旧标签 read_view 的实例对象: %v %v", items, err)
	}
	for _, tp := range []string{"pgsql", "mongo", "clickhouse", "oracle", "dm"} {
		if dialectFor(tp) == nil {
			t.Errorf("%s 应有元数据实现", tp)
		}
	}
	if dialectFor("cassandra") != nil || dialectFor("elasticsearch") != nil {
		t.Error("cassandra / elasticsearch 没有元数据实现")
	}
	// 未关联实例仍拒绝
	if _, err := e.svc.Databases(ctxb, bob, 25); code(err) != "no_privilege" {
		t.Errorf("%v", err)
	}
}

func TestPGDict(t *testing.T) {
	e := newEngEnv(t)
	dbs, err := e.svc.Databases(ctxb, bob, 21)
	if err != nil || strings.Join(dbs, ",") != "appdb,postgres" {
		t.Fatalf("库: %v %v", dbs, err)
	}
	items, notes, err := e.svc.ObjectsEx(ctxb, bob, 21, "appdb", KindTable)
	if err != nil || len(items) != 2 || len(notes) != 1 || !strings.Contains(notes[0], "估算") {
		t.Fatalf("表: %v %+v %v", err, items, notes)
	}
	o := items[0]
	if o.Schema != "public" || o.Name != "orders" || o.Comment != "订单表" || o.RowsEst == nil || *o.RowsEst != 12345 || o.SizeBytes == nil || *o.SizeBytes != 9216000 || o.Type != "BASE TABLE" {
		t.Errorf("orders: %+v", o)
	}
	if d := items[1]; d.Schema != "reporting" || d.Name != "daily" || !d.Partitioned || d.RowsEst == nil || *d.RowsEst != 0 {
		t.Errorf("daily: %+v", d)
	}
	views, _ := e.svc.Objects(ctxb, bob, 21, "appdb", KindView)
	if len(views) != 1 || views[0].Name != "v_orders" || views[0].RowsEst != nil || views[0].SizeBytes != nil {
		t.Errorf("视图: %+v", views)
	}
	det, err := e.svc.Detail(ctxb, bob, DetailRequest{InstanceID: 21, DB: "appdb", Kind: KindTable, Schema: "public", Name: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if det.Schema != "public" || det.Name != "orders" || len(det.Notes) != 1 || len(det.Sections) != 4 {
		t.Fatalf("%+v", det)
	}
	titles := []string{"基本信息", "列信息", "索引信息", "建表语句"}
	for i, s := range det.Sections {
		if s.Title != titles[i] {
			t.Errorf("段 %d 标题 %q", i, s.Title)
		}
	}
	kv := map[string]string{}
	for _, p := range det.Sections[0].Pairs {
		kv[p.Key] = p.Value
	}
	if kv["类型"] != "BASE TABLE" || kv["属主"] != "app" || kv["访问方法"] != "heap" || kv["行数（估算）"] != "12345" || kv["注释"] != "订单表" || kv["数据大小(KB)"] != "8000.00" {
		t.Errorf("基本信息: %+v", kv)
	}
	cols := det.Sections[1]
	if len(cols.Rows) != 2 || cols.Rows[0][0] != "id" || cols.Rows[0][2] != "否" || cols.Rows[0][4] != "PRI" || cols.Rows[0][5] != "identity by default" || cols.Rows[1][3] != "'anon'::character varying" || cols.Rows[1][2] != "是" {
		t.Errorf("列: %+v", cols.Rows)
	}
	if idx := det.Sections[2].Rows; len(idx) != 1 || idx[0][0] != "orders_pkey" || idx[0][1] != "id" || idx[0][2] != "是" || !strings.Contains(idx[0][3], "btree") {
		t.Errorf("索引: %+v", idx)
	}
	if det.Sections[3].Type != "code" || !strings.Contains(det.Sections[3].Text, "CREATE TABLE public.orders") || !strings.HasPrefix(det.Sections[3].Text, "-- 近似 DDL") {
		t.Errorf("DDL: %q", det.Sections[3].Text)
	}
	// 不存在的对象、种类不匹配
	if _, err := e.svc.Detail(ctxb, bob, DetailRequest{InstanceID: 21, DB: "appdb", Kind: KindTable, Schema: "public", Name: "nosuch"}); code(err) != "not_found" {
		t.Errorf("不存在: %v", err)
	}
	if _, err := e.svc.Detail(ctxb, bob, DetailRequest{InstanceID: 21, DB: "appdb", Kind: KindView, Schema: "public", Name: "orders"}); code(err) != "not_found" {
		t.Errorf("表当视图: %v", err)
	}
	// 其它对象类别不支持
	if _, err := e.svc.Objects(ctxb, bob, 21, "appdb", KindTrigger); code(err) != "unsupported_kind" {
		t.Errorf("触发器: %v", err)
	}
	// 脱敏配置对 PostgreSQL 同样生效：命中的列在“列信息”里标敏感（这里的桩配置是“任意库表的 name 字段”）
	e.svc.WithMasking(maskStub{})
	det, _ = e.svc.Detail(ctxb, bob, DetailRequest{InstanceID: 21, DB: "appdb", Kind: KindTable, Schema: "public", Name: "orders"})
	if len(det.SensitiveColumns) != 1 || det.SensitiveColumns[0] != "name" || len(det.Sections[1].Columns) != 8 {
		t.Errorf("应标出 name 为敏感列: %v %+v", det.SensitiveColumns, det.Sections[1].Columns)
	}
	// 系统库
	if _, err := e.svc.Objects(ctxb, bob, 21, "template1", KindTable); code(err) != "db_not_allowed" {
		t.Errorf("template1: %v", err)
	}
	// 不连 mysql 协议：引擎连接在请求结束时关闭（不泄漏连接池）
}

type maskStub struct{}

func (maskStub) ActiveMaskingColumns(uint) ([]model.DataMaskingColumn, error) {
	return []model.DataMaskingColumn{{TableSchema: "*", TableName_: "*", ColumnName: "name"}}, nil
}

func TestMongoDict(t *testing.T) {
	e := newEngEnv(t)
	dbs, err := e.svc.Databases(ctxb, root, 22)
	if err != nil || strings.Join(dbs, ",") != "app,other" {
		t.Fatalf("库: %v %v", dbs, err)
	}
	dbs, _ = e.svc.Databases(ctxb, root, 25)
	if strings.Join(dbs, ",") != "app" {
		t.Errorf("库名过滤: %v", dbs)
	}
	tbl, notes, err := e.svc.ObjectsEx(ctxb, bob, 22, "app", KindTable)
	if err != nil || len(tbl) != 1 || tbl[0].Name != "users" || tbl[0].Type != "collection" || tbl[0].RowsEst == nil || *tbl[0].RowsEst != 2 || len(notes) != 1 || !strings.Contains(notes[0], "采样") {
		t.Fatalf("集合: %v %+v %v", err, tbl, notes)
	}
	views, _ := e.svc.Objects(ctxb, bob, 22, "app", KindView)
	if len(views) != 1 || views[0].Name != "v1" || views[0].Type != "view" || views[0].RowsEst != nil {
		t.Errorf("视图: %+v", views)
	}
	det, err := e.svc.Detail(ctxb, bob, DetailRequest{InstanceID: 22, DB: "app", Kind: KindTable, Name: "users"})
	if err != nil {
		t.Fatal(err)
	}
	cols := det.Sections[1].Rows
	if len(cols) != 4 || cols[0][0] != "_id" || cols[0][4] != "PRI" || !strings.Contains(cols[0][6], "采样 2 个文档") {
		t.Errorf("字段: %+v", cols)
	}
	idx := det.Sections[2].Rows
	if len(idx) != 2 || idx[1][0] != "email_1" || idx[1][1] != "email(1)" || idx[1][2] != "是" || !strings.Contains(idx[1][3], "TTL 60 秒") {
		t.Errorf("索引: %+v", idx)
	}
	if !strings.Contains(det.Sections[3].Text, "不是 DDL") && !strings.Contains(det.Sections[3].Text, "note") {
		t.Errorf("Mongo 的“建表语句”应声明不是 DDL: %q", det.Sections[3].Text)
	}
	if _, err := e.svc.Objects(ctxb, bob, 22, "admin", KindTable); code(err) != "db_not_allowed" {
		t.Errorf("admin 库: %v", err)
	}
	if _, err := e.svc.Detail(ctxb, bob, DetailRequest{InstanceID: 22, DB: "app", Kind: KindTable, Name: "nosuch"}); code(err) != "not_found" {
		t.Errorf("不存在: %v", err)
	}
}

func TestCHDict(t *testing.T) {
	e := newEngEnv(t)
	dbs, err := e.svc.Databases(ctxb, bob, 23)
	if err != nil || strings.Join(dbs, ",") != "app,default" {
		t.Fatalf("库: %v %v", dbs, err)
	}
	tbl, notes, err := e.svc.ObjectsEx(ctxb, bob, 23, "app", KindTable)
	if err != nil || len(tbl) != 2 || len(notes) != 1 || !strings.Contains(notes[0], "创建时间") {
		t.Fatalf("表: %v %+v %v", err, tbl, notes)
	}
	if tbl[0].Name != "events" || tbl[0].Type != "MergeTree" || tbl[0].RowsEst == nil || *tbl[0].RowsEst != 100 || tbl[0].SizeBytes == nil || *tbl[0].SizeBytes != 2048+128 {
		t.Errorf("events: %+v", tbl[0])
	}
	if tbl[1].Name != "dict1" || tbl[1].Type != "Dictionary" || tbl[1].RowsEst != nil || tbl[1].SizeBytes != nil {
		t.Errorf("字典归在表里，行数 / 大小取不到时为空: %+v", tbl[1])
	}
	views, _ := e.svc.Objects(ctxb, bob, 23, "app", KindView)
	if len(views) != 1 || views[0].Name != "v_events" {
		t.Errorf("视图: %+v", views)
	}
	det, err := e.svc.Detail(ctxb, bob, DetailRequest{InstanceID: 23, DB: "app", Kind: KindTable, Name: "events"})
	if err != nil {
		t.Fatal(err)
	}
	kv := map[string]string{}
	for _, p := range det.Sections[0].Pairs {
		kv[p.Key] = p.Value
	}
	if kv["引擎"] != "MergeTree" || kv["分区键"] != "toYYYYMM(dt)" || kv["排序键"] != "id" || kv["行数"] != "100" || !strings.HasPrefix(kv["最近一次 DDL"], "2023-11-1") || kv["注释"] != "事件" {
		t.Errorf("基本信息: %+v", kv)
	}
	if _, has := kv["创建时间"]; has {
		t.Error("ClickHouse 没有创建时间")
	}
	cols := det.Sections[1].Rows
	if len(cols) != 2 || cols[0][4] != "PRI" || !strings.Contains(cols[0][5], "排序键") || cols[1][3] != "today()" || !strings.Contains(cols[1][5], "DEFAULT") || !strings.Contains(cols[1][5], "分区键") {
		t.Errorf("列: %+v", cols)
	}
	idx := det.Sections[2].Rows
	if len(idx) < 2 || idx[0][0] != "PRIMARY" || !strings.Contains(idx[0][3], "主键") || idx[len(idx)-1][0] != "idx_n" || !strings.Contains(idx[len(idx)-1][3], "minmax") {
		t.Errorf("索引: %+v", idx)
	}
	if det.Sections[3].Text != "CREATE TABLE app.events (id UInt64, dt Date) ENGINE = MergeTree ORDER BY id" {
		t.Errorf("DDL: %q", det.Sections[3].Text)
	}
	if _, err := e.svc.Objects(ctxb, bob, 23, "system", KindTable); code(err) != "db_not_allowed" {
		t.Errorf("system 库: %v", err)
	}
}

// 老类型（MySQL / SQL Server）的响应里没有 PostgreSQL / MongoDB / ClickHouse 才有的新字段。
func TestItemDetailJSONOmitNewFields(t *testing.T) {
	b, _ := json.Marshal(Item{Name: "t"})
	if strings.Contains(string(b), `"type"`) {
		t.Errorf("Item 不应带 type 字段: %s", b)
	}
	b, _ = json.Marshal(Detail{Name: "t"})
	if strings.Contains(string(b), "notes") {
		t.Errorf("Detail 不应带 notes 字段: %s", b)
	}
}
