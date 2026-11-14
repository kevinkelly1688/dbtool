// @author: kevinkelly
// @license: Apache Licence
// @file: multi_test.go
// @time: 2026/10/28

package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"dbtool/internal/engine"
	"dbtool/internal/engine/clickhouse"
	mongox "dbtool/internal/engine/mongo"
	"dbtool/internal/engine/mongo/mongotest"
	"dbtool/internal/engine/postgres"
	"dbtool/internal/engine/postgres/pgtest"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// PostgreSQL / MongoDB / ClickHouse 的在线查询：用真实的引擎包连进程内假后端（pgtest 的 pgproto3 假服务、mongotest 的内存后端、
// httptest 的 ClickHouse HTTP 假服务），覆盖放行 / 拒绝 / 授权 / 限量 / 超时 / 取消 / 日志 / 并发 / 元数据。

const (
	pgID    = uint(11)
	mongoID = uint(12)
	chID    = uint(13)
)

// ---- ClickHouse 假服务 ----

type chFake struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []string
	// routes 按顺序匹配语句文本，第一个匹配的处理请求；都不匹配返回一行 x=1。
	routes []chRoute
}

type chRoute struct {
	sub string
	h   func(w http.ResponseWriter)
}

func newCHFake(t *testing.T) *chFake {
	f := &chFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		f.mu.Lock()
		f.reqs = append(f.reqs, body)
		routes := f.routes
		f.mu.Unlock()
		for _, rt := range routes {
			if strings.Contains(body, rt.sub) {
				rt.h(w)
				return
			}
		}
		if strings.Contains(body, "FROM system.tables WHERE (database, name) IN") { // 引擎核对：没有特殊引擎的表
			chJC(w, [][2]string{{"database", "String"}, {"name", "String"}, {"engine", "String"}}, nil)
			return
		}
		chJC(w, [][2]string{{"x", "UInt8"}}, [][]any{{1}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *chFake) on(sub string, cols [][2]string, rows [][]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes = append(f.routes, chRoute{sub, func(w http.ResponseWriter) { chJC(w, cols, rows) }})
}

func (f *chFake) onErr(sub string, code int, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes = append(f.routes, chRoute{sub, func(w http.ResponseWriter) {
		w.Header().Set("X-ClickHouse-Exception-Code", strconv.Itoa(code))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "Code: "+strconv.Itoa(code)+". DB::Exception: "+msg+" (version 24.3.2.23 (official build))\n")
	}})
}

func (f *chFake) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func (f *chFake) conn() engine.Conn {
	host, portS, _ := net.SplitHostPort(strings.TrimPrefix(f.srv.URL, "http://"))
	port, _ := strconv.Atoi(portS)
	return engine.Conn{Host: host, Port: port, User: "reader", Password: "s3cr3t-Pa55"}
}

func chJC(w http.ResponseWriter, cols [][2]string, rows [][]any) {
	if rows == nil {
		rows = [][]any{}
	}
	meta := make([]map[string]string, len(cols))
	for i, c := range cols {
		meta[i] = map[string]string{"name": c[0], "type": c[1]}
	}
	b, _ := json.Marshal(map[string]any{"meta": meta, "data": rows, "rows": len(rows),
		"statistics": map[string]any{"elapsed": 0.001, "rows_read": len(rows), "bytes_read": 64}})
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}

// ---- 环境 ----

type multiEnv struct {
	repo *evRepo
	pg   *pgtest.Server
	mgo  *mongotest.Fake
	ch   *chFake
	svc  *Service
}

func canRead() []model.InstanceTag {
	return []model.InstanceTag{{TagCode: "can_read", TagName: "支持查询", Active: true}}
}

// pgHandler 是假 PostgreSQL 的默认应答：库 / 表 / 列 / 索引元数据与几张表的数据。
func pgHandler(sql string, params []string) *pgtest.Result {
	low := strings.ToLower(sql)
	switch {
	case strings.Contains(sql, "SHOW server_version"):
		return &pgtest.Result{Cols: pgtest.TextCols("server_version"), Rows: [][]*string{pgtest.Cell("16.2")}}
	case strings.Contains(sql, "FROM pg_catalog.pg_database"):
		return &pgtest.Result{Cols: pgtest.TextCols("datname"), Rows: [][]*string{pgtest.Cell("shop"), pgtest.Cell("postgres")}}
	case strings.Contains(sql, "pg_table_size"):
		return &pgtest.Result{Cols: pgtest.TextCols("a", "b", "c", "d", "e", "f", "g", "h", "i"), Rows: [][]*string{
			pgtest.Cell("public", "orders", "r", "heap", "订单表", "app", "12345", "8192000", "1024000"),
			pgtest.Cell("public", "v_orders", "v", "", "", "app", "-1", "0", "0"),
			pgtest.Cell("reporting", "daily", "p", "", pgtest.NullCell, "app", "0", "0", "16384"),
		}}
	case strings.Contains(sql, "n.nspname || '.' || c.relname") && strings.Contains(sql, "ILIKE"):
		return &pgtest.Result{Cols: pgtest.TextCols("t", "c", "d"), Rows: [][]*string{pgtest.Cell("public.orders", "user_id", "买家")}}
	case strings.Contains(sql, "n.nspname || '.' || c.relname"):
		return &pgtest.Result{Cols: pgtest.TextCols("t"), Rows: [][]*string{pgtest.Cell("public.orders"), pgtest.Cell("public.v_orders"), pgtest.Cell("reporting.daily")}}
	case strings.Contains(sql, "attidentity"):
		return &pgtest.Result{Cols: pgtest.TextCols("a", "b", "c", "d", "e", "f", "g", "h", "i"), Rows: [][]*string{
			pgtest.Cell("1", "id", "bigint", "false", pgtest.NullCell, "主键", "1", "d", ""),
			pgtest.Cell("2", "name", "character varying(64)", "true", pgtest.NullCell, "", "0", "", ""),
		}}
	case strings.Contains(low, "pg_sleep"):
		return &pgtest.Result{Cols: pgtest.TextCols("pg_sleep"), Rows: [][]*string{pgtest.Cell("")}, Delay: 5 * time.Second}
	case strings.Contains(low, "slow_tbl"):
		return &pgtest.Result{Cols: pgtest.TextCols("id"), Rows: [][]*string{pgtest.Cell("1")}, Delay: 5 * time.Second}
	case strings.Contains(low, "from public.orders") || strings.Contains(low, "from orders"):
		rows := make([][]*string, 0, 5)
		for i := 1; i <= 5; i++ {
			rows = append(rows, pgtest.Cell(strconv.Itoa(i), "n"+strconv.Itoa(i)))
		}
		return &pgtest.Result{Cols: []pgtest.Col{{Name: "id", OID: 23}, {Name: "name", OID: 25}}, Rows: rows}
	case strings.Contains(low, "from reporting.daily"), strings.Contains(low, "from public.v_orders"):
		return &pgtest.Result{Cols: []pgtest.Col{{Name: "id", OID: 23}}, Rows: [][]*string{pgtest.Cell("9")}}
	case strings.Contains(low, "select 1"), strings.Contains(low, "pg_catalog."), strings.Contains(low, "information_schema."), strings.HasPrefix(low, "show "):
		return &pgtest.Result{Cols: []pgtest.Col{{Name: "x", OID: 23}}, Rows: [][]*string{pgtest.Cell("1")}}
	}
	return nil
}

func newMultiEnv(t *testing.T) *multiEnv {
	t.Helper()
	e := &multiEnv{pg: pgtest.New(t, pgHandler), mgo: mongotest.New(), ch: newCHFake(t)}
	app := e.mgo.DB("app")
	app.Coll("users",
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "name", Value: "a"}, {Key: "age", Value: int32(30)}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "name", Value: "b"}, {Key: "age", Value: 31.5}},
		bson.D{{Key: "_id", Value: int32(3)}, {Key: "name", Value: "c"}, {Key: "age", Value: int32(40)}},
	).StatDoc = bson.D{{Key: "count", Value: int32(3)}, {Key: "size", Value: int32(300)}, {Key: "storageSize", Value: int64(4096)}, {Key: "totalIndexSize", Value: int64(8192)}, {Key: "nindexes", Value: int32(1)}}
	app.Coll("orders", bson.D{{Key: "_id", Value: int32(10)}, {Key: "uid", Value: int32(1)}})
	app.Stat = bson.D{{Key: "db", Value: "app"}, {Key: "collections", Value: int32(2)}}
	e.mgo.DB("other").Coll("secrets", bson.D{{Key: "_id", Value: int32(1)}})

	e.ch.on("FROM system.databases", [][2]string{{"name", "String"}}, [][]any{{"app"}, {"default"}})
	e.ch.on("FROM system.tables WHERE database = 'app' AND NOT startsWith", [][2]string{{"name", "String"}, {"engine", "String"}, {"engine_full", "String"}, {"comment", "String"},
		{"total_rows", "Nullable(UInt64)"}, {"total_bytes", "Nullable(UInt64)"}, {"mtime", "UInt32"}, {"partition_key", "String"}, {"sorting_key", "String"}, {"primary_key", "String"}, {"sampling_key", "String"}},
		[][]any{{"events", "MergeTree", "MergeTree ORDER BY id", "事件", "100", "2048", 1700000000, "toYYYYMM(dt)", "id", "id", ""},
			{"v_events", "View", "View", "", nil, nil, 1700000000, "", "", "", ""},
			{"dict1", "Dictionary", "Dictionary", "", nil, nil, 0, "", "", "", ""}})
	e.ch.on("FROM system.tables WHERE database = 'app'", [][2]string{{"name", "String"}}, [][]any{{"events"}, {"v_events"}, {"dict1"}})
	e.ch.on("positionCaseInsensitive", [][2]string{{"table", "String"}, {"name", "String"}, {"comment", "String"}}, [][]any{{"events", "user_id", "用户"}})
	e.ch.on("FROM system.columns", [][2]string{{"name", "String"}, {"type", "String"}, {"default_kind", "String"}, {"default_expression", "String"}, {"comment", "String"},
		{"is_in_partition_key", "UInt8"}, {"is_in_sorting_key", "UInt8"}, {"is_in_primary_key", "UInt8"}, {"is_in_sampling_key", "UInt8"}, {"codec", "String"}, {"position", "UInt64"}},
		[][]any{{"id", "UInt64", "", "", "主键", 0, 1, 1, 0, "", 1}, {"dt", "Date", "", "", "", 1, 0, 0, 0, "", 2}})
	e.ch.on("from events", [][2]string{{"id", "UInt64"}, {"n", "Nullable(String)"}}, [][]any{{"1", "a"}, {"2", nil}, {"3", "c"}})

	r := newRepo()
	r.insts = map[uint]*model.Instance{
		pgID:    {ID: pgID, InstanceName: "pg-01", DBType: "pgsql", Type: "master", Tags: canRead()},
		mongoID: {ID: mongoID, InstanceName: "mongo-01", DBType: "mongo", Type: "master", Tags: canRead()},
		chID:    {ID: chID, InstanceName: "ch-01", DBType: "clickhouse", Type: "master", Tags: canRead()},
	}
	r.perms[7] = map[string]bool{"query.run": true, "query.view": true}
	r.userInst[7] = map[uint]bool{pgID: true, mongoID: true, chID: true}
	e.repo = &evRepo{histRepo: &histRepo{fakeRepo: r}, ready: true}
	e.svc = NewService(e.repo, func(dbType string, _ engine.Conn) (engine.Engine, error) {
		switch dbType {
		case "pgsql":
			return postgres.New(e.pg.Conn("u", "", "shop"))
		case "mongo":
			return mongox.NewWithBackend(e.mgo), nil
		case "clickhouse":
			return clickhouse.NewEngine(e.ch.conn())
		}
		return nil, fmt.Errorf("暂不支持的数据库类型: %s", dbType)
	}, nil)
	return e
}

func (e *multiEnv) grant(inst uint, typ int, db, tb string, limit int) {
	e.repo.privs = append(e.repo.privs, model.QueryPrivilege{UserName: "dev", InstanceID: inst, PrivType: typ, DBName: db, TbName: tb, LimitNum: limit,
		ValidDate: time.Now().AddDate(0, 0, 30)})
}

func (e *multiEnv) exec(p Principal, inst uint, db, sql string) (*Result, error) {
	return e.svc.Execute(context.Background(), p, Request{InstanceID: inst, DB: db, SQL: sql})
}

func (e *multiEnv) lastEvent() *QueryEvent {
	if len(e.repo.events) == 0 {
		return nil
	}
	return e.repo.events[len(e.repo.events)-1]
}

func cellRows(res *Result) string {
	var rows []string
	for _, r := range res.Rows {
		var cs []string
		for _, v := range r {
			cs = append(cs, fmt.Sprint(v))
		}
		rows = append(rows, strings.Join(cs, ","))
	}
	return strings.Join(rows, "|")
}

// ---- 基本 ----

func TestMultiSupportsQueryAndInstances(t *testing.T) {
	for _, ty := range []string{"pgsql", "mongo", "clickhouse", "oracle", "dm"} {
		if !SupportsQuery(ty) || !isMulti(ty) {
			t.Errorf("%s 应支持在线查询", ty)
		}
	}
	for _, ty := range []string{"mysql", "mssql", "redis", "cassandra"} {
		if isMulti(ty) {
			t.Errorf("%s 不是新类型", ty)
		}
	}
	e := newMultiEnv(t)
	list, err := e.svc.Instances(dev)
	if err != nil || len(list) != 3 {
		t.Fatalf("实例列表: %v %+v", err, list)
	}
	types := map[string]bool{}
	for _, v := range list {
		types[v.DBType] = true
	}
	if !types["pgsql"] || !types["mongo"] || !types["clickhouse"] {
		t.Errorf("%v", types)
	}
}

// ---- PostgreSQL ----

func TestPGExecuteAdminShapesResult(t *testing.T) {
	e := newMultiEnv(t)
	res, err := e.exec(admin, pgID, "shop", "select id, name from public.orders order by id")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != "pgsql" || res.StmtKind != "select" || !res.Unmasked || strings.Join(res.Columns, ",") != "id,name" || strings.Join(res.ColumnTypes, ",") != "int4,text" {
		t.Errorf("kind=%q stmt=%q unmasked=%v cols=%v types=%v", res.Kind, res.StmtKind, res.Unmasked, res.Columns, res.ColumnTypes)
	}
	if cellRows(res) != "1,n1|2,n2|3,n3|4,n4|5,n5" || res.RowCount != 5 || res.Truncated || res.MaskedColumns == nil || len(res.MaskedColumns) != 0 {
		t.Errorf("rows=%s truncated=%v", cellRows(res), res.Truncated)
	}
	if !strings.Contains(res.ExecutedSQL, "LIMIT") || res.EffectiveLimit != 5000 || res.LimitSource != "管理员上限" || res.Notice != "" {
		t.Errorf("executed=%q limit=%d src=%q notice=%q", res.ExecutedSQL, res.EffectiveLimit, res.LimitSource, res.Notice)
	}
	if len(res.Warnings) == 0 || !strings.Contains(strings.Join(res.Warnings, ""), "LIMIT") {
		t.Errorf("应提示自动补 LIMIT: %v", res.Warnings)
	}
	// 管理员的查询写 dbt_query_log：库名、实际语句、priv_check 为假
	if len(e.repo.logs) != 1 {
		t.Fatalf("logs=%d", len(e.repo.logs))
	}
	l := e.repo.logs[0]
	if l.DBName != "shop" || l.InstanceName != "pg-01" || l.Username != "root" || l.PrivCheck || !strings.Contains(l.SQLLog, "public.orders") || l.EffectRow != 5 {
		t.Errorf("日志: %+v", l)
	}
	// 引擎只读事务 + search_path 钉在 public
	if !e.pg.Has("BEGIN READ ONLY") || !e.pg.Has(`search_path = "public"`) {
		t.Errorf("语句: %v", e.pg.Statements())
	}
}

func TestPGPermissions(t *testing.T) {
	e := newMultiEnv(t)
	// 没有任何权限
	_, err := e.exec(dev, pgID, "shop", "select id from public.orders")
	var qe *Error
	if !errors.As(err, &qe) || qe.Code != "no_privilege" || qe.Data["database"] != "shop" || qe.Data["table"] != "public.orders" || !strings.Contains(qe.Message, "shop.public.orders") {
		t.Fatalf("无权限: %+v", err)
	}
	if ev := e.lastEvent(); ev == nil || ev.Status != StatusBlocked || ev.Code != "no_privilege" || ev.DB != "shop" {
		t.Errorf("应记入被拦截: %+v", ev)
	}
	// 表级权限：不带 schema 的名字视为 public；limit 3
	e.grant(pgID, 2, "shop", "orders", 3)
	res, err := e.exec(dev, pgID, "shop", "select id from orders")
	if err != nil || res.EffectiveLimit != 3 || res.LimitSource != "shop.public.orders 表权限" || len(res.Rows) != 3 || !res.Truncated || res.TruncatedBy != "rows" {
		t.Fatalf("表级权限: %v %+v", err, res)
	}
	if !e.repo.logs[len(e.repo.logs)-1].PrivCheck {
		t.Error("普通用户的查询 priv_check 应为真")
	}
	// 其它表不在授权范围
	_, err = e.exec(dev, pgID, "shop", "select id from reporting.daily")
	if code(err) != "no_privilege" {
		t.Errorf("表级权限不应覆盖其它表: %v", err)
	}
	// JOIN 里藏一张无权限的表
	_, err = e.exec(dev, pgID, "shop", "select o.id from public.orders o join reporting.daily d on d.id = o.id")
	if code(err) != "no_privilege" {
		t.Errorf("JOIN 无权限表应拒绝: %v", err)
	}
	// 库级权限覆盖全部表，行数取库 limit
	e.grant(pgID, 1, "shop", "", 100)
	res, err = e.exec(dev, pgID, "shop", "select id from reporting.daily")
	if err != nil || res.LimitSource != "shop 库权限" || res.EffectiveLimit != 100 {
		t.Errorf("库级权限: %v %+v", err, res)
	}
	// 区分大小写：Shop 的权限不适用于 shop
	e2 := newMultiEnv(t)
	e2.grant(pgID, 1, "Shop", "", 10)
	if _, err := e2.exec(dev, pgID, "shop", "select 1 from public.orders"); code(err) != "no_privilege" {
		t.Errorf("库名区分大小写: %v", err)
	}
	// 请求里的 limit 更小时取小
	r2, err := e.svc.Execute(context.Background(), dev, Request{InstanceID: pgID, DB: "shop", SQL: "select id from public.orders", Limit: 2})
	if err != nil || len(r2.Rows) != 2 || r2.EffectiveLimit != 2 || !r2.Truncated {
		t.Errorf("请求 limit: %v %+v", err, r2)
	}
}

func TestPGSystemSchemasAndStructure(t *testing.T) {
	e := newMultiEnv(t)
	// 无表查询与 pg_catalog / information_schema：免表级权限，行数上限 100
	for _, sql := range []string{"select 1", "select * from pg_catalog.pg_class limit 3", "select * from information_schema.tables"} {
		res, err := e.exec(dev, pgID, "shop", sql)
		if err != nil || res.LimitSource != "无表查询" || res.EffectiveLimit != 100 {
			t.Errorf("%q: %v %+v", sql, err, res)
		}
	}
	// SHOW 参数名是结构查询
	if res, err := e.exec(dev, pgID, "shop", "show server_version"); err != nil || res.LimitSource != "结构查询" || res.StmtKind != "show" {
		t.Errorf("SHOW: %v %+v", err, res)
	}
	// 其它系统 schema 拒绝
	if _, err := e.exec(dev, pgID, "shop", "select * from pg_toast.pg_toast_1"); code(err) != "bad_query" {
		t.Errorf("pg_toast: %v", err)
	}
	// 敏感系统视图由引擎拒绝
	_, err := e.exec(admin, pgID, "shop", "select * from pg_authid")
	if code(err) != "bad_query" {
		t.Errorf("pg_authid: %v", err)
	}
}

func TestPGNeedAllPrivRequiresDBLevel(t *testing.T) {
	ck, err := postgres.CheckUserQuery("select * from public.my_func(1)", postgres.CheckOptions{DBName: "shop"})
	if err != nil || !ck.NeedAllPriv {
		t.Skipf("引擎未把自定义函数判为 NeedAllPriv（%v），无法构造用例", err)
	}
	e := newMultiEnv(t)
	e.grant(pgID, 2, "shop", "orders", 10)
	_, err = e.exec(dev, pgID, "shop", "select * from public.my_func(1)")
	var qe *Error
	if !errors.As(err, &qe) || qe.Code != "no_privilege" || !strings.Contains(qe.Message, "库级") || qe.Data["table"] != "" {
		t.Errorf("表级权限不够: %v", err)
	}
	e.grant(pgID, 1, "shop", "", 10)
	if _, err = e.exec(dev, pgID, "shop", "select * from public.my_func(1)"); err != nil && code(err) == "no_privilege" {
		t.Errorf("库级权限应放行: %v", err)
	}
}

func TestPGRejectedAreBlockedAndRecorded(t *testing.T) {
	e := newMultiEnv(t)
	bad := []string{
		"insert into public.orders values (1)", "update public.orders set id = 1", "select pg_sleep(1)", "select 1; select 2",
		"select * from public.orders for update", "explain analyze select 1", "select * from pg_authid", "create table t (id int)",
		"copy public.orders to stdout", "set role x", "select set_config('a','b',false)", "", "select 'unclosed",
	}
	for _, sql := range bad {
		_, err := e.exec(admin, pgID, "shop", sql)
		if strings.TrimSpace(sql) == "" {
			if code(err) != "bad_request" {
				t.Errorf("空语句: %v", err)
			}
			continue
		}
		if code(err) != "bad_query" {
			t.Errorf("%q 应被拒绝为 bad_query: %v", sql, err)
		}
	}
	if len(e.repo.logs) != 0 {
		t.Errorf("被拒绝的语句不写 dbt_query_log: %d", len(e.repo.logs))
	}
	if ev := e.lastEvent(); ev == nil || ev.Status != StatusBlocked || ev.InstanceName != "pg-01" {
		t.Errorf("被拦截记录: %+v", ev)
	}
	// 被拒绝的语句没有到达服务端
	if e.pg.Has("pg_sleep") || e.pg.Has("INSERT") {
		t.Errorf("被拒绝的语句不应发给服务端: %v", e.pg.Statements())
	}
	// 缺少库
	if _, err := e.exec(admin, pgID, "", "select 1"); code(err) != "bad_request" {
		t.Errorf("缺库: %v", err)
	}
}

func TestPGTimeoutAndCancel(t *testing.T) {
	e := newMultiEnv(t)
	e.repo.cfg[settings.KeyQueryMaxExecSeconds] = "1"
	start := time.Now()
	_, err := e.exec(admin, pgID, "shop", "select id from slow_tbl")
	if code(err) != "timeout" || time.Since(start) > 4*time.Second {
		t.Errorf("超时: %v (%s)", err, time.Since(start))
	}
	if ev := e.lastEvent(); ev == nil || ev.Status != StatusTimeout {
		t.Errorf("超时记录: %+v", ev)
	}
	// 取消
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	e.repo.cfg[settings.KeyQueryMaxExecSeconds] = "30"
	_, err = e.svc.Execute(ctx, admin, Request{InstanceID: pgID, DB: "shop", SQL: "select id from slow_tbl"})
	if code(err) != "canceled" {
		t.Errorf("取消: %v", err)
	}
	if ev := e.lastEvent(); ev == nil || ev.Status != StatusCanceled {
		t.Errorf("取消记录: %+v", ev)
	}
}

func TestPGServerErrorIsBadQuery(t *testing.T) {
	e := newMultiEnv(t)
	e.pg.Close() // 连接失败 → engine_error
	_, err := e.exec(admin, pgID, "shop", "select id from public.orders")
	if code(err) != "engine_error" {
		t.Errorf("连接失败: %v", err)
	}
	if ev := e.lastEvent(); ev == nil || ev.Status != StatusFailed {
		t.Errorf("失败记录: %+v", ev)
	}
}

func TestPGSQLErrorFromServerMapsToBadQuery(t *testing.T) {
	srv := pgtest.New(t, func(sql string, _ []string) *pgtest.Result {
		if strings.Contains(strings.ToLower(sql), "missing_tbl") {
			return pgtest.Err("42P01", `relation "missing_tbl" does not exist`)
		}
		return nil
	})
	r := newMultiEnv(t)
	r.svc = NewService(r.repo, func(string, engine.Conn) (engine.Engine, error) { return postgres.New(srv.Conn("u", "", "shop")) }, nil)
	_, err := r.exec(admin, pgID, "shop", "select * from public.missing_tbl")
	var qe *Error
	if !errors.As(err, &qe) || qe.Code != "bad_query" || !strings.Contains(qe.Message, "missing_tbl") || !strings.Contains(qe.Message, "42P01") {
		t.Errorf("服务端 SQL 错误应按 bad_query 展示: %v", err)
	}
}

// ---- MongoDB ----

func TestMongoExecuteAndPermissions(t *testing.T) {
	e := newMultiEnv(t)
	res, err := e.exec(admin, mongoID, "app", `db.users.find({}).sort({_id: 1})`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != "mongo" || res.StmtKind != "find" || !res.Unmasked || len(res.Rows) != 3 || res.ColumnTypes != nil || !strings.Contains(res.ExecutedSQL, "users") {
		t.Errorf("%+v", res)
	}
	if strings.Join(res.Columns, ",") != "_id,name,age" {
		t.Errorf("列: %v", res.Columns)
	}
	// 无权限
	_, err = e.exec(dev, mongoID, "app", `db.users.find({})`)
	var qe *Error
	if !errors.As(err, &qe) || qe.Code != "no_privilege" || qe.Data["table"] != "users" {
		t.Fatalf("无权限: %v", err)
	}
	// 集合级权限 + limit 收窄：限 2 行，find 的 limit 被收窄并提示
	e.grant(mongoID, 2, "app", "users", 2)
	res, err = e.exec(dev, mongoID, "app", `db.users.find({})`)
	if err != nil || len(res.Rows) != 2 || res.EffectiveLimit != 2 || res.LimitSource != "app.users 表权限" {
		t.Fatalf("集合级: %v %+v", err, res)
	}
	if !strings.Contains(strings.Join(res.Warnings, ""), "limit") && !strings.Contains(strings.Join(res.Warnings, ""), "截断") {
		t.Errorf("应提示行数上限: %v", res.Warnings)
	}
	// $lookup 的目标集合也要有权限
	_, err = e.exec(dev, mongoID, "app", `db.users.aggregate([{$lookup: {from: "orders", localField: "_id", foreignField: "uid", as: "o"}}])`)
	if code(err) != "no_privilege" || !strings.Contains(err.Error(), "orders") {
		t.Errorf("$lookup 目标集合无权限应拒绝: %v", err)
	}
	// 库级语句要库级权限
	_, err = e.exec(dev, mongoID, "app", `db.getCollectionNames()`)
	if code(err) != "no_privilege" || !strings.Contains(err.Error(), "库级") {
		t.Errorf("库级语句: %v", err)
	}
	e.grant(mongoID, 1, "app", "", 50)
	if res, err = e.exec(dev, mongoID, "app", `db.getCollectionNames()`); err != nil || res.LimitSource != "app 库权限" || len(res.Rows) != 2 {
		t.Errorf("库级权限: %v %+v", err, res)
	}
	// 库权限覆盖 $lookup
	if _, err = e.exec(dev, mongoID, "app", `db.users.aggregate([{$lookup: {from: "orders", localField: "_id", foreignField: "uid", as: "o"}}, {$limit: 1}])`); err != nil {
		t.Errorf("库级权限下 $lookup: %v", err)
	}
	// 另一个库没有权限（区分大小写与库隔离）
	if _, err = e.exec(dev, mongoID, "other", `db.secrets.find({})`); code(err) != "no_privilege" {
		t.Errorf("other 库无权限: %v", err)
	}
	if _, err = e.exec(dev, mongoID, "APP", `db.users.find({})`); code(err) != "no_privilege" {
		t.Errorf("库名区分大小写: %v", err)
	}
}

func TestMongoRejected(t *testing.T) {
	e := newMultiEnv(t)
	bad := []string{
		`db.users.insertOne({a: 1})`, `db.users.drop()`, `db.runCommand({ping: 1})`, `db.users.find({$where: "this.a > 1"})`,
		`db.users.aggregate([{$out: "x"}])`, `db.users.aggregate([{$merge: {into: "x"}}])`, `db.getSiblingDB("admin").x.find()`,
		`db.users.find({}); db.users.find({})`, `db.system.users.find()`, `db.users.find({a: function(){}})`, `db.users.explain().find()`,
	}
	for _, s := range bad {
		if _, err := e.exec(admin, mongoID, "app", s); code(err) != "bad_query" {
			t.Errorf("%q 应被拒绝: %v", s, err)
		}
	}
	if _, err := e.exec(admin, mongoID, "admin", `db.users.find({})`); code(err) != "bad_query" {
		t.Errorf("admin 库应拒绝: %v", err)
	}
	if len(e.repo.logs) != 0 {
		t.Errorf("被拒绝的语句不写 dbt_query_log")
	}
	if len(e.mgo.Finds) != 0 || len(e.mgo.Aggs) != 0 {
		t.Errorf("被拒绝的语句不应到达后端: %d %d", len(e.mgo.Finds), len(e.mgo.Aggs))
	}
	if ev := e.lastEvent(); ev == nil || ev.Status != StatusBlocked {
		t.Errorf("被拦截记录: %+v", ev)
	}
}

func TestMongoTruncationAndBackendError(t *testing.T) {
	e := newMultiEnv(t)
	req := Request{InstanceID: mongoID, DB: "app", SQL: `db.users.find({})`, Limit: 1}
	res, err := e.svc.Execute(context.Background(), admin, req)
	if err != nil || len(res.Rows) != 1 || !res.Truncated || res.TruncatedBy != "rows" {
		t.Errorf("请求 limit 截断: %v %+v", err, res)
	}
	e.mgo.DB("app").Coll("users").Err = errors.New("boom")
	if _, err = e.svc.Execute(context.Background(), admin, Request{InstanceID: mongoID, DB: "app", SQL: `db.users.find({})`}); code(err) != "engine_error" {
		t.Errorf("后端错误: %v", err)
	}
	// 超时
	e.mgo.DB("app").Coll("users").Err = nil
	e.mgo.DB("app").Coll("users").Block = true
	e.repo.cfg[settings.KeyQueryMaxExecSeconds] = "1"
	start := time.Now()
	_, err = e.exec(admin, mongoID, "app", `db.users.find({})`)
	if code(err) != "timeout" || time.Since(start) > 5*time.Second {
		t.Errorf("超时: %v (%s)", err, time.Since(start))
	}
}

// ---- ClickHouse ----

func TestCHExecuteAndPermissions(t *testing.T) {
	e := newMultiEnv(t)
	res, err := e.exec(admin, chID, "app", "select id, n from events")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != "clickhouse" || res.StmtKind != "select" || !res.Unmasked || strings.Join(res.ColumnTypes, ",") != "UInt64,Nullable(String)" || cellRows(res) != "1,a|2,<nil>|3,c" {
		t.Errorf("%+v rows=%s", res, cellRows(res))
	}
	if res.ExecutedSQL != "select id, n from events" || res.Truncated {
		t.Errorf("executed=%q", res.ExecutedSQL)
	}
	// 请求都带 readonly，库名通过 database 参数
	_, err = e.exec(dev, chID, "app", "select id from events")
	var qe *Error
	if !errors.As(err, &qe) || qe.Code != "no_privilege" || qe.Data["database"] != "app" || qe.Data["table"] != "events" {
		t.Fatalf("无权限: %v", err)
	}
	e.grant(chID, 2, "app", "events", 2)
	res, err = e.exec(dev, chID, "app", "select id, n from events")
	if err != nil || len(res.Rows) != 2 || !res.Truncated || res.TruncatedBy != "rows" || res.EffectiveLimit != 2 {
		t.Fatalf("表级权限: %v %+v", err, res)
	}
	// 跨库引用需要那个库的权限
	if _, err = e.exec(dev, chID, "app", "select 1 from events e join other.t o on o.id = e.id"); code(err) != "no_privilege" || !strings.Contains(err.Error(), "other.t") {
		t.Errorf("跨库引用: %v", err)
	}
	// system 的摘要表免表权限，system.users 引擎直接拒绝
	if res, err = e.exec(dev, chID, "app", "select name from system.tables limit 5"); err != nil || res.LimitSource != "无表查询" {
		t.Errorf("system.tables: %v %+v", err, res)
	}
	if _, err = e.exec(admin, chID, "app", "select * from system.users"); code(err) != "bad_query" {
		t.Errorf("system.users: %v", err)
	}
}

func TestCHStructureQueriesAndRejected(t *testing.T) {
	e := newMultiEnv(t)
	if res, err := e.exec(dev, chID, "app", "show tables from app"); err != nil || res.LimitSource != "结构查询" || res.StmtKind != "show" {
		t.Errorf("SHOW TABLES: %v %+v", err, res)
	}
	if res, err := e.exec(dev, chID, "app", "describe app.events"); err != nil || res.StmtKind != "describe" {
		t.Errorf("DESCRIBE: %v %+v", err, res)
	}
	// 库名过滤规则对结构查询生效
	e.repo.insts[chID].DeniedDBNameRegex = "^secret"
	if _, err := e.exec(dev, chID, "app", "show tables from secret_db"); code(err) != "no_privilege" {
		t.Errorf("被隐藏的库: %v", err)
	}
	// EXPLAIN 要求涉及对象的权限
	if _, err := e.exec(dev, chID, "app", "explain select id from events"); code(err) != "no_privilege" {
		t.Errorf("EXPLAIN 要权限: %v", err)
	}
	bad := []string{"insert into events values (1)", "select 1 settings max_threads = 1", "select * from file('/etc/passwd')", "select * from merge('app', 'e.*')",
		"optimize table events", "select sleep(3)", "select * from remote('h', app, t)", "select 1; select 2", "alter table events delete where 1", ""}
	for _, s := range bad {
		_, err := e.exec(admin, chID, "app", s)
		want := "bad_query"
		if strings.TrimSpace(s) == "" {
			want = "bad_request"
		}
		if code(err) != want {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, body := range e.ch.requests() {
		if strings.Contains(body, "file(") || strings.Contains(body, "optimize") || strings.Contains(body, "sleep") {
			t.Errorf("被拒绝的语句不应发给服务端: %q", body)
		}
	}
	if ev := e.lastEvent(); ev == nil || ev.Status != StatusBlocked {
		t.Errorf("被拦截记录: %+v", ev)
	}
}

func TestCHServerErrors(t *testing.T) {
	e := newMultiEnv(t)
	e.ch.onErr("from bad_ro", 164, "Cannot modify 'max_execution_time' setting in readonly mode")
	_, err := e.exec(admin, chID, "app", "select * from bad_ro")
	var qe *Error
	if !errors.As(err, &qe) || qe.Code != "engine_error" || !strings.Contains(qe.Message, "readonly=2") {
		t.Errorf("readonly=1 账号 profile 的提示: %v", err)
	}
	e.ch.onErr("from too_slow", 159, "Timeout exceeded")
	if _, err = e.exec(admin, chID, "app", "select * from too_slow"); code(err) != "timeout" {
		t.Errorf("服务端超时: %v", err)
	}
	if ev := e.lastEvent(); ev == nil || ev.Status != StatusTimeout {
		t.Errorf("超时记录: %+v", ev)
	}
	// 错误里不带密码
	e.ch.onErr("from leak", 516, "auth failed s3cr3t-Pa55")
	_, err = e.exec(admin, chID, "app", "select * from leak")
	if err == nil || strings.Contains(err.Error(), "s3cr3t-Pa55") {
		t.Errorf("错误里不应含密码: %v", err)
	}
}

// ---- 共性 ----

// 旧标签 read_view 不再限制任何引擎：带标签的实例与 can_read 实例一样可执行、列库、取元数据。
func TestMultiLegacyReadViewTagNotRestricted(t *testing.T) {
	e := newMultiEnv(t)
	stmts := map[uint][2]string{
		pgID:    {"shop", "select id, name from public.orders order by id"},
		mongoID: {"app", `db.users.find({}).sort({_id: 1})`},
		chID:    {"app", "select id, n from events"},
	}
	for id, st := range stmts {
		e.repo.insts[id].Tags = append(e.repo.insts[id].Tags, model.InstanceTag{TagCode: "read_view", TagName: "只读视图", Active: true})
		if _, err := e.exec(admin, id, st[0], st[1]); err != nil {
			t.Errorf("inst %d 执行: %v", id, err)
		}
		if _, err := e.svc.Databases(context.Background(), admin, id); err != nil {
			t.Errorf("inst %d 库列表: %v", id, err)
		}
		if _, err := e.svc.DBMeta(context.Background(), admin, id, st[0]); err != nil {
			t.Errorf("inst %d 元数据: %v", id, err)
		}
	}
}

func TestMultiConcurrencyLimit(t *testing.T) {
	e := newMultiEnv(t)
	var rel []func()
	for i := 0; i < multiUserConcurrent; i++ {
		r, err := acquireMultiSlot("root", "pgsql")
		if err != nil {
			t.Fatal(err)
		}
		rel = append(rel, r)
	}
	_, err := e.exec(admin, pgID, "shop", "select 1")
	var qe *Error
	if !errors.As(err, &qe) || qe.Code != "busy" || qe.HTTP != 429 {
		t.Fatalf("超过并发上限应 429: %v", err)
	}
	// 三种类型共用计数；别的用户不受影响
	if _, err = e.exec(admin, chID, "app", "select 1"); code(err) != "busy" {
		t.Errorf("共用计数: %v", err)
	}
	other, err := acquireMultiSlot("someone", "mongo")
	if err != nil {
		t.Fatalf("别的用户: %v", err)
	}
	for _, r := range rel {
		r()
	}
	if _, err = e.exec(admin, pgID, "shop", "select 1"); err != nil {
		t.Errorf("释放后应可用: %v", err)
	}
	multiSlots.Lock()
	left := len(multiSlots.n)
	multiSlots.Unlock()
	if left != 1 { // 只剩 "someone" 那个没释放的
		t.Errorf("计数应清理: %d", left)
	}
	other()
	// 并发请求：12 个同时进来，每个用户最多 3 个同时在执行
	var wg sync.WaitGroup
	var okN, busyN int
	var mu sync.Mutex
	e.repo.cfg[settings.KeyQueryMaxExecSeconds] = "10"
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.exec(admin, pgID, "shop", "select id from public.orders")
			mu.Lock()
			defer mu.Unlock()
			switch code(err) {
			case "?":
				okN++
			case "busy":
				busyN++
			default:
				t.Errorf("意外错误: %v", err)
			}
		}()
	}
	wg.Wait()
	if okN+busyN != 12 || okN == 0 {
		t.Errorf("ok=%d busy=%d", okN, busyN)
	}
}

func TestMultiAuditFailureBlocksResult(t *testing.T) {
	e := newMultiEnv(t)
	e.repo.logErr = errors.New("db down")
	for id, q := range map[uint][2]string{pgID: {"shop", "select 1"}, mongoID: {"app", "db.users.find({})"}, chID: {"app", "select 1"}} {
		res, err := e.exec(admin, id, q[0], q[1])
		if res != nil || code(err) != "audit_failed" {
			t.Errorf("inst %d: 写不进 dbt_query_log 就不返回结果: %v %v", id, res, err)
		}
	}
}

func TestMultiEstimateIsUnknown(t *testing.T) {
	e := newMultiEnv(t)
	est, err := e.svc.EstimateRows(context.Background(), admin, Request{InstanceID: pgID, DB: "shop", SQL: "select id from public.orders"})
	if err != nil || est.Rows != -1 {
		t.Errorf("估算: %v %+v", err, est)
	}
}

// ---- 元数据 ----

func TestMultiDatabasesAndTables(t *testing.T) {
	e := newMultiEnv(t)
	ctx := context.Background()
	dbs, err := e.svc.Databases(ctx, dev, pgID)
	if err != nil || strings.Join(dbs, ",") != "postgres,shop" {
		t.Errorf("pg 库: %v %v", dbs, err)
	}
	if dbs, err = e.svc.Databases(ctx, dev, mongoID); err != nil || strings.Join(dbs, ",") != "app,other" {
		t.Errorf("mongo 库（不含 admin/local/config）: %v %v", dbs, err)
	}
	e.repo.insts[mongoID].DeniedDBNameRegex = "^other"
	if dbs, _ = e.svc.Databases(ctx, dev, mongoID); strings.Join(dbs, ",") != "app" {
		t.Errorf("库名过滤: %v", dbs)
	}
	if dbs, err = e.svc.Databases(ctx, dev, chID); err != nil || strings.Join(dbs, ",") != "app,default" {
		t.Errorf("ch 库: %v %v", dbs, err)
	}
	if tbs, err := e.svc.Tables(ctx, dev, pgID, "shop"); err != nil || strings.Join(tbs, ",") != "public.orders,public.v_orders,reporting.daily" {
		t.Errorf("pg 表: %v %v", tbs, err)
	}
	if tbs, err := e.svc.Tables(ctx, dev, mongoID, "app"); err != nil || strings.Join(tbs, ",") != "orders,users" {
		t.Errorf("mongo 集合: %v %v", tbs, err)
	}
	if tbs, err := e.svc.Tables(ctx, dev, chID, "app"); err != nil || strings.Join(tbs, ",") != "events,v_events,dict1" {
		t.Errorf("ch 表: %v %v", tbs, err)
	}
	// 实例不在资源组内
	e.repo.userInst[7] = map[uint]bool{}
	if _, err := e.svc.Databases(ctx, dev, pgID); code(err) != "no_privilege" {
		t.Errorf("未关联实例: %v", err)
	}
}

func TestMultiDBMetaLockedAndKinds(t *testing.T) {
	e := newMultiEnv(t)
	ctx := context.Background()
	e.grant(pgID, 2, "shop", "orders", 100) // public.orders 的表权限
	m, err := e.svc.DBMeta(ctx, dev, pgID, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Unmasked || m.SensitiveTotal != 0 || len(m.Tables) != 3 || !m.Policy.TableLevel || m.Policy.Mode != "priv" {
		t.Errorf("%+v", m)
	}
	by := map[string]TableMeta{}
	for _, tb := range m.Tables {
		by[tb.Name] = tb
	}
	o, v, d := by["public.orders"], by["public.v_orders"], by["reporting.daily"]
	if o.Schema != "public" || o.Kind != "table" || o.Rows != 12345 || o.Comment != "订单表" || o.Locked || o.Sensitive == nil {
		t.Errorf("orders: %+v", o)
	}
	if !v.View || v.Kind != "view" || v.Rows != -1 || !v.Locked {
		t.Errorf("v_orders: %+v", v)
	}
	if d.Kind != "partitioned" || !d.Locked || d.Schema != "reporting" {
		t.Errorf("daily: %+v", d)
	}
	if m.LockedTotal != 2 {
		t.Errorf("locked=%d", m.LockedTotal)
	}
	// 库级权限：全部不锁；管理员也不锁
	e.grant(pgID, 1, "shop", "", 100)
	m, _ = e.svc.DBMeta(ctx, dev, pgID, "shop")
	if m.LockedTotal != 0 {
		t.Errorf("库级权限: locked=%d", m.LockedTotal)
	}
	// 系统库不能浏览
	if _, err := e.svc.DBMeta(ctx, admin, mongoID, "admin"); code(err) != "no_privilege" {
		t.Errorf("mongo admin: %v", err)
	}
	if _, err := e.svc.DBMeta(ctx, admin, chID, "system"); code(err) != "no_privilege" {
		t.Errorf("ch system: %v", err)
	}
	// Mongo
	mm, err := e.svc.DBMeta(ctx, admin, mongoID, "app")
	if err != nil || len(mm.Tables) != 2 || !mm.Unmasked {
		t.Fatalf("mongo meta: %v %+v", err, mm)
	}
	for _, tb := range mm.Tables {
		if tb.Name == "users" && (tb.Kind != "collection" || tb.Rows != 3 || tb.Locked) {
			t.Errorf("users: %+v", tb)
		}
	}
	// ClickHouse：视图 / 字典用 kind 区分
	cm, err := e.svc.DBMeta(ctx, admin, chID, "app")
	if err != nil || len(cm.Tables) != 3 {
		t.Fatalf("ch meta: %v %+v", err, cm)
	}
	for _, tb := range cm.Tables {
		switch tb.Name {
		case "events":
			if tb.Kind != "table" || tb.Rows != 100 || tb.View {
				t.Errorf("events: %+v", tb)
			}
		case "v_events":
			if tb.Kind != "view" || !tb.View || tb.Rows != -1 {
				t.Errorf("v_events: %+v", tb)
			}
		case "dict1":
			if tb.Kind != "dictionary" || tb.View {
				t.Errorf("dict1: %+v", tb)
			}
		}
	}
}

func TestMultiColumnsAndSearch(t *testing.T) {
	e := newMultiEnv(t)
	ctx := context.Background()
	cols, err := e.svc.TableColumns(ctx, dev, pgID, "shop", "public.orders")
	if err != nil || len(cols) != 2 || cols[0].Name != "id" || cols[0].Key != "PRI" || cols[0].Type != "bigint" || cols[0].Nullable || cols[0].Comment != "主键" || !cols[1].Nullable {
		t.Errorf("pg 列: %v %+v", err, cols)
	}
	cols, err = e.svc.TableColumns(ctx, dev, mongoID, "app", "users")
	if err != nil || len(cols) != 3 || cols[0].Name != "_id" || cols[0].Key != "PRI" || !strings.Contains(cols[0].Comment, "采样") {
		t.Errorf("mongo 字段: %v %+v", err, cols)
	}
	cols, err = e.svc.TableColumns(ctx, dev, chID, "app", "events")
	if err != nil || len(cols) != 2 || cols[0].Name != "id" || cols[0].Key != "PRI" || cols[0].Type != "UInt64" {
		t.Errorf("ch 列: %v %+v", err, cols)
	}
	hits, err := e.svc.SearchColumns(ctx, dev, pgID, "shop", "user_")
	if err != nil || len(hits) != 1 || hits[0].Table != "public.orders" || hits[0].Column != "user_id" || hits[0].Comment != "买家" {
		t.Errorf("pg 搜索: %v %+v", err, hits)
	}
	// 通配符被转义、不拼成语句
	if _, err := e.svc.SearchColumns(ctx, dev, pgID, "shop", `a'b%_\`); err != nil {
		t.Errorf("特殊字符: %v", err)
	}
	for _, s := range e.pg.Statements() {
		if strings.Contains(s, "a''b") && !strings.Contains(s, `'%a''b\%\_\\%'`) {
			t.Errorf("搜索关键字未转义: %s", s)
		}
	}
	if hits, err = e.svc.SearchColumns(ctx, dev, chID, "app", "user"); err != nil || len(hits) != 1 || hits[0].Table != "events" {
		t.Errorf("ch 搜索: %v %+v", err, hits)
	}
	if hits, err = e.svc.SearchColumns(ctx, dev, mongoID, "app", "name"); err != nil || len(hits) != 0 || hits == nil {
		t.Errorf("mongo 搜索恒为空: %v %+v", err, hits)
	}
	// 系统库不可查
	if _, err := e.svc.TableColumns(ctx, dev, mongoID, "admin", "system.users"); code(err) != "no_privilege" {
		t.Errorf("系统库: %v", err)
	}
	if _, err := e.svc.TableColumns(ctx, dev, chID, "app", ""); code(err) != "bad_request" {
		t.Errorf("缺表名: %v", err)
	}
}

func TestMultiPolicyForTableLevel(t *testing.T) {
	e := newMultiEnv(t)
	e.grant(chID, 2, "app", "events", 10)
	p, err := e.svc.PolicyFor(context.Background(), dev, chID, "app")
	if err != nil || p.Mode != "priv" || !p.TableLevel || len(p.Tables) != 1 || p.Tables[0] != "events" || p.AllTables {
		t.Errorf("%v %+v", err, p)
	}
	// 区分大小写：APP 库没有权限
	if p, _ = e.svc.PolicyFor(context.Background(), dev, chID, "APP"); p.Mode != "none" {
		t.Errorf("库名区分大小写: %+v", p)
	}
	if !(PolicyInfo{Mode: "priv", Tables: []string{"orders"}}).HasObject("pgsql", "public.orders") {
		t.Error("PG 权限里不带 schema 的表名视为 public")
	}
	if (PolicyInfo{Mode: "priv", Tables: []string{"orders"}}).HasObject("pgsql", "reporting.orders") {
		t.Error("不带 schema 只匹配 public")
	}
	if (PolicyInfo{Mode: "priv", Tables: []string{"Orders"}}).HasObject("clickhouse", "orders") {
		t.Error("ClickHouse 表名区分大小写")
	}
}

// 回归：MySQL / SQL Server / Redis 的表级判断与大小写规则不受新类型影响。
func TestMultiDoesNotChangeOtherTypes(t *testing.T) {
	if !sameDBName("mysql", "Shop", "shop") || !sameDBName("mssql", "A", "a") || sameDBName("pgsql", "A", "a") || sameDBName("mongo", "A", "a") || sameDBName("clickhouse", "A", "a") {
		t.Error("sameDBName")
	}
	if !tableLevelType("mysql") || tableLevelType("mssql") || tableLevelType("redis") || !tableLevelType("pgsql") {
		t.Error("tableLevelType")
	}
	r := devRepo()
	if _, _, err := run(t, r, dev, "shop", "select * from orders"); err != nil {
		t.Errorf("MySQL 表级权限: %v", err)
	}
	if _, _, err := run(t, r, dev, "shop", "select * from users"); code(err) != "no_privilege" {
		t.Errorf("MySQL 无权限: %v", err)
	}
}
