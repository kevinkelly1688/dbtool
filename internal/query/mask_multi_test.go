// @author: kevinkelly
// @license: Apache Licence
// @file: mask_multi_test.go
// @time: 2026/10/28

package query

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"dbtool/internal/engine"
	"dbtool/internal/engine/clickhouse"
	mongox "dbtool/internal/engine/mongo"
	"dbtool/internal/engine/mongo/mongotest"
	"dbtool/internal/engine/mssql"
	"dbtool/internal/engine/postgres"
	"dbtool/internal/engine/postgres/pgtest"
	"dbtool/internal/masking"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// 全引擎数据脱敏：PostgreSQL（结果列来源）、ClickHouse（保守匹配）、MongoDB（字段路径）、SQL Server（来源分析 + 降级）。

func keepCfg(db, table, col string, n, m int) model.DataMaskingColumn {
	return model.DataMaskingColumn{Active: true, TableSchema: db, TableName_: table, ColumnName: col, Kind: masking.KindKeep, KeepPrefix: n, KeepSuffix: m}
}

func wholeCfg(db, table, col string) model.DataMaskingColumn {
	return model.DataMaskingColumn{Active: true, TableSchema: db, TableName_: table, ColumnName: col, Kind: masking.KindWhole}
}

type maskEnv struct {
	repo *evRepo
	svc  *Service
	pg   *pgtest.Server
	mgo  *mongotest.Fake
	ch   *chFake
}

func newMaskEnv(t *testing.T, pgH pgtest.Handler, cols ...model.DataMaskingColumn) *maskEnv {
	t.Helper()
	e := &maskEnv{mgo: mongotest.New(), ch: newCHFake(t)}
	if pgH != nil {
		e.pg = pgtest.New(t, pgH)
	}
	r := newRepo()
	r.cfg[settings.KeyQueryMaskEnabled] = "true"
	r.maskCols = cols
	r.insts = map[uint]*model.Instance{
		pgID:    {ID: pgID, InstanceName: "pg-01", DBType: "pgsql", Type: "master", Tags: canRead()},
		mongoID: {ID: mongoID, InstanceName: "mongo-01", DBType: "mongo", Type: "master", Tags: canRead()},
		chID:    {ID: chID, InstanceName: "ch-01", DBType: "clickhouse", Type: "master", Tags: canRead()},
	}
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

func (e *maskEnv) run(t *testing.T, inst uint, db, sql string) *Result {
	t.Helper()
	res, err := e.svc.Execute(context.Background(), admin, Request{InstanceID: inst, DB: db, SQL: sql})
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return res
}

// ---- PostgreSQL ----

func pgMaskHandler(sql string, params []string) *pgtest.Result {
	low := strings.ToLower(sql)
	switch {
	case strings.Contains(low, "get_phone("): // 自定义函数：结果列没有来源表
		return &pgtest.Result{
			Cols:    []pgtest.Col{{Name: "id", OID: 20}, {Name: "p", OID: 25}},
			Rows:    [][]*string{pgtest.Cell("1", "13812345678")},
			Origins: []pgtest.Origin{{Table: 16384, Att: 1}, {}},
		}
	case strings.Contains(sql, "pg_catalog.pg_attribute"):
		return &pgtest.Result{Cols: pgtest.TextCols("rel", "att", "nsp", "tbl", "col", "kind"), Rows: [][]*string{
			pgtest.Cell("16384", "1", "public", "account", "id", "r"),
			pgtest.Cell("16384", "3", "public", "account", "phone", "r"),
			pgtest.Cell("16501", "5", "public", "orders", "phone", "r"),
			pgtest.Cell("16600", "2", "reporting", "v_account", "email", "v"),
		}}
	case strings.Contains(low, "from public.account"):
		return &pgtest.Result{
			Cols:    []pgtest.Col{{Name: "id", OID: 20}, {Name: "phone", OID: 25}, {Name: "email", OID: 25}, {Name: "initial", OID: 25}, {Name: "extra", OID: 3802}},
			Rows:    [][]*string{pgtest.Cell("1", "13812345678", "a@corp.com", "13", `{"phone":"13912345678","n":1}`)},
			Origins: []pgtest.Origin{{Table: 16384, Att: 1}, {Table: 16384, Att: 3}, {Table: 16600, Att: 2}, {}, {}},
		}
	case strings.Contains(low, "from public.orders"):
		return &pgtest.Result{
			Cols:    []pgtest.Col{{Name: "phone", OID: 25}},
			Rows:    [][]*string{pgtest.Cell("13700000000")},
			Origins: []pgtest.Origin{{Table: 16501, Att: 5}},
		}
	}
	return nil
}

func TestPGMaskBySource(t *testing.T) {
	e := newMaskEnv(t, pgMaskHandler,
		keepCfg("shop", "public.account", "phone", 3, 4),
		keepCfg("shop", "reporting.base_account", "email", 2, 0), // 配置写在视图的底层表上
		keepCfg("shop", "public.account", "extra", 0, 0),
	)
	res := e.run(t, pgID, "shop", "select id, phone, email, substr(phone,1,2) as initial, extra from public.account")
	// phone 来自 public.account.phone：精确命中；id 不动；email 来自视图，按列名保守命中；initial 是引用了 phone 的表达式列；extra 是 jsonb
	row := res.Rows[0]
	if fmt.Sprint(row[0]) != "1" || row[1] != "138***5678" || row[2] != "a@***" || row[3] != "1***" {
		t.Errorf("row=%v", row)
	}
	if got := fmt.Sprint(row[4]); got != `{"n":"***","phone":"***"}` {
		t.Errorf("jsonb 应逐个叶子脱敏: %v", row[4])
	}
	if res.MaskMode != "source" || res.Unmasked || !reflect.DeepEqual(res.MaskedColumns, []string{"phone", "email", "initial", "extra"}) {
		t.Errorf("mode=%q unmasked=%v masked=%v", res.MaskMode, res.Unmasked, res.MaskedColumns)
	}
	if len(e.repo.logs) == 0 || !e.repo.logs[len(e.repo.logs)-1].HitRule || !e.repo.logs[len(e.repo.logs)-1].Masking {
		t.Errorf("日志应标记命中脱敏: %+v", e.repo.logs)
	}
	// 同名字段在别的表里：精确来源不是配置的表，不应被误伤
	res = e.run(t, pgID, "shop", "select phone from public.orders")
	if res.Rows[0][0] != "13700000000" || len(res.MaskedColumns) != 0 {
		t.Errorf("别的表的 phone 不应脱敏: %v %v", res.Rows[0], res.MaskedColumns)
	}
}

// 自定义函数的结果列没有来源表：整列按最严规则脱敏；同一语句里有来源的普通列不受影响。
func TestPGMaskUserFunction(t *testing.T) {
	e := newMaskEnv(t, pgMaskHandler, keepCfg("shop", "public.account", "phone", 3, 4))
	res := e.run(t, pgID, "shop", "select id, public.get_phone(1) as p from public.account")
	row := res.Rows[0]
	if fmt.Sprint(row[0]) != "1" || row[1] != "138***5678" || !reflect.DeepEqual(res.MaskedColumns, []string{"p"}) {
		t.Errorf("row=%v masked=%v mode=%q", row, res.MaskedColumns, res.MaskMode)
	}
}

func TestPGMaskSourcesUnknownFallsBackToNames(t *testing.T) {
	// 取不到来源（反查 pg_class 被拒）：全部按列名保守匹配
	h := func(sql string, params []string) *pgtest.Result {
		if strings.Contains(sql, "pg_catalog.pg_attribute") {
			return pgtest.Err("42501", "permission denied")
		}
		return pgMaskHandler(sql, params)
	}
	e := newMaskEnv(t, h, keepCfg("shop", "public.account", "phone", 3, 4))
	res := e.run(t, pgID, "shop", "select phone from public.orders")
	if res.MaskMode != "column-name" || res.Rows[0][0] != "137***0000" {
		t.Errorf("来源未知时应按列名脱敏: mode=%q row=%v", res.MaskMode, res.Rows[0])
	}
}

// ---- ClickHouse ----

func TestCHMaskConservative(t *testing.T) {
	e := newMaskEnv(t, nil, keepCfg("*", "*", "phone", 3, 4), wholeCfg("*", "*", "id_card"))
	e.ch.on("from events", [][2]string{{"id", "UInt64"}, {"p", "String"}, {"c", "String"}, {"tag", "Map(String,String)"}, {"name", "String"}},
		[][]any{{"1", "13812345678", "13812345678x", `{"k":"13812345678"}`, "zs"}})
	res := e.run(t, chID, "app", "select id, phone as p, concat(phone,'x') as c, mapFilter((k,v)->1, tag) as tag, name from events")
	row := res.Rows[0]
	if row[0] != "1" || row[1] != "138***5678" || row[2] != "138***678x" || row[4] != "zs" {
		t.Errorf("row=%v", row)
	}
	if res.MaskMode != "column-name" || res.Unmasked {
		t.Errorf("mode=%q unmasked=%v", res.MaskMode, res.Unmasked)
	}
	// 没有引用任何敏感字段的语句：什么都不动
	e.ch.on("from t2", [][2]string{{"a", "String"}}, [][]any{{"13812345678"}})
	res = e.run(t, chID, "app", "select a from t2")
	if res.Rows[0][0] != "13812345678" || len(res.MaskedColumns) != 0 {
		t.Errorf("无关列不应脱敏: %v", res.Rows[0])
	}
}

// ---- MongoDB ----

func TestMongoMasker(t *testing.T) {
	cols := []masking.Column{
		{Schema: "app", Table: "users", Column: "profile.phone", Rule: &masking.Rule{Kind: masking.KindKeep, Prefix: 3, Suffix: 4}},
		{Schema: "app", Table: "users", Column: "contacts[].mobile", Rule: &masking.Rule{Kind: masking.KindKeep, Prefix: 3, Suffix: 4}},
		{Schema: "*", Table: "*", Column: "id_card", Rule: &masking.Rule{Kind: masking.KindWhole}},
		{Schema: "app", Table: "orders", Column: "receiver.addr", Rule: &masking.Rule{Kind: masking.KindWhole}}, // 与本次语句无关的集合
		{Schema: "other", Table: "users", Column: "name", Rule: &masking.Rule{Kind: masking.KindWhole}},         // 别的库
	}
	pipeline := []bson.D{
		{{Key: "$project", Value: bson.D{{Key: "p", Value: "$profile.phone"}, {Key: "name", Value: 1}}}},
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$p"}, {Key: "first", Value: bson.D{{Key: "$first", Value: "$profile.phone"}}}}}},
	}
	m := newMongoMasker(cols, nil, "app", []string{"users"}, pipeline)
	if m == nil {
		t.Fatal("应有适用配置")
	}
	doc := bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "name", Value: "张三"},
		{Key: "Profile", Value: bson.D{{Key: "Phone", Value: "13812345678"}, {Key: "id_card", Value: "110101199003071234"}}}, // 大小写不敏感
		{Key: "contacts", Value: bson.A{bson.D{{Key: "mobile", Value: "13987654321"}, {Key: "name", Value: "李四"}}, bson.D{{Key: "mobile", Value: int64(13700000000)}}}},
		{Key: "p", Value: "13812345678"}, // $project 改名
		{Key: "o", Value: bson.A{bson.D{{Key: "profile", Value: bson.D{{Key: "phone", Value: "13611112222"}}}}}}, // $lookup 之后嵌套
		{Key: "orders", Value: bson.D{{Key: "receiver", Value: bson.D{{Key: "addr", Value: "北京市海淀区"}}}}},         // 其它集合的配置不适用
	}
	got := m.Doc(doc)
	flat := fmt.Sprint(got)
	for _, leak := range []string{"13812345678", "13987654321", "13700000000", "110101199003071234", "13611112222"} {
		if strings.Contains(flat, leak) {
			t.Errorf("泄露了 %s：%v", leak, got)
		}
	}
	for _, want := range []string{"138***5678", "139***4321", "137***0000", "***", "136***2222", "北京市海淀区", "张三", "李四"} {
		if !strings.Contains(flat, want) {
			t.Errorf("缺少 %q：%v", want, got)
		}
	}
	if !reflect.DeepEqual(m.Columns(), []string{"Profile", "_id", "contacts", "o", "p"}) {
		t.Errorf("已脱敏的顶层字段 = %v", m.Columns())
	}
	// distinct 取值
	if v := m.Distinct("profile.phone", "13812345678"); v != "138***5678" {
		t.Errorf("distinct: %v", v)
	}
	if v := m.Distinct("age", int32(30)); v != int32(30) {
		t.Errorf("无关字段 distinct 不应变: %v", v)
	}
	// 没有适用配置：返回 nil（调用方据此不装脱敏器）
	if newMongoMasker(cols[:1], nil, "app", []string{"orders"}, nil) != nil {
		t.Error("集合不相关时不应创建脱敏器")
	}
}

func TestMongoMaskEndToEnd(t *testing.T) {
	e := newMaskEnv(t, nil,
		model.DataMaskingColumn{Active: true, TableSchema: "app", TableName_: "users", ColumnName: "profile.phone", Kind: masking.KindKeep, KeepPrefix: 3, KeepSuffix: 4},
	)
	e.mgo.DB("app").Coll("users",
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "name", Value: "a"}, {Key: "profile", Value: bson.D{{Key: "phone", Value: "13812345678"}, {Key: "city", Value: "北京"}}}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "name", Value: "b"}},
	)
	res := e.run(t, mongoID, "app", `db.users.find({})`)
	if res.MaskMode != "path" || res.Unmasked || !reflect.DeepEqual(res.MaskedColumns, []string{"profile"}) {
		t.Errorf("mode=%q unmasked=%v masked=%v", res.MaskMode, res.Unmasked, res.MaskedColumns)
	}
	profile := fmt.Sprint(res.Rows[0][2])
	if strings.Contains(profile, "13812345678") || !strings.Contains(profile, "138***5678") || !strings.Contains(profile, "北京") {
		t.Errorf("profile=%s", profile)
	}
	if !e.repo.logs[len(e.repo.logs)-1].HitRule {
		t.Error("日志应标记命中")
	}
}

// ---- SQL Server ----

type mssqlMaskEngine struct {
	maskEngine
	base, views []mssql.ColumnSource
	err         error
}

func (e *mssqlMaskEngine) DescribeSources(context.Context, string, string) ([]mssql.ColumnSource, []mssql.ColumnSource, error) {
	return e.base, e.views, e.err
}

// 标量 UDF 与自定义表值函数的结果列服务端分析不出来源表：整列按最严规则脱敏，有来源的普通列不动。
func TestMSSQLMaskUserFunction(t *testing.T) {
	src := func(i int, db, sch, tbl, col string) mssql.ColumnSource {
		return mssql.ColumnSource{Ordinal: i, Name: col, DB: db, Schema: sch, Table: tbl, Column: col}
	}
	eng := &mssqlMaskEngine{
		maskEngine: maskEngine{cols: []string{"BuyerId", "P"}, data: [][]any{{1, "13812345678"}}},
		base:       []mssql.ColumnSource{src(1, "ERP", "dbo", "Buyer", "BuyerId"), {Ordinal: 2, Name: "P"}},
	}
	r := newRepo()
	r.cfg[settings.KeyQueryMaskEnabled] = "true"
	r.insts = map[uint]*model.Instance{5: {ID: 5, InstanceName: "erp-01", DBType: "mssql", Type: "master", Tags: canRead()}}
	r.maskCols = []model.DataMaskingColumn{keepCfg("ERP", "dbo.Buyer", "Mobile", 3, 4)}
	s := NewService(r, func(string, engine.Conn) (engine.Engine, error) { return eng, nil }, NewPrivAuthorizer(r, NewAutoExtractor(r)))
	res, err := s.Execute(context.Background(), admin, Request{InstanceID: 5, DB: "ERP", SQL: "select e.BuyerId, dbo.fn_get_phone(e.BuyerId) as P from dbo.Buyer e"})
	if err != nil {
		t.Fatal(err)
	}
	if row := res.Rows[0]; row[0] != 1 || row[1] != "138***5678" || !reflect.DeepEqual(res.MaskedColumns, []string{"P"}) {
		t.Errorf("标量 UDF: row=%v masked=%v mode=%q", row, res.MaskedColumns, res.MaskMode)
	}

	// 自定义表值函数（多语句的返回的表变量没有来源）：select * 展开的每一列都脱敏
	eng.cols, eng.data = []string{"mobile_no", "nm"}, [][]any{{"13812345678", "张三"}}
	eng.base = []mssql.ColumnSource{{Ordinal: 1, Name: "mobile_no"}, {Ordinal: 2, Name: "nm"}}
	res, err = s.Execute(context.Background(), admin, Request{InstanceID: 5, DB: "ERP", SQL: "select * from dbo.tvf_buyer(1)"})
	if err != nil {
		t.Fatal(err)
	}
	if row := res.Rows[0]; row[0] != "138***5678" || row[1] != "张***" {
		t.Errorf("表值函数: row=%v masked=%v mode=%q", row, res.MaskedColumns, res.MaskMode)
	}
}

func TestMSSQLMaskBySource(t *testing.T) {
	src := func(i int, db, sch, tbl, col string) mssql.ColumnSource {
		return mssql.ColumnSource{Ordinal: i, Name: col, DB: db, Schema: sch, Table: tbl, Column: col}
	}
	eng := &mssqlMaskEngine{
		maskEngine: maskEngine{cols: []string{"BuyerId", "Mobile", "Email", "Head"}, data: [][]any{{1, "13812345678", "a@corp.com", "13"}}},
		// 第 3 列 Email：基表是 dbo.Buyer.Email，视图层是 crm.v_buyer.Email；第 4 列是表达式
		base:  []mssql.ColumnSource{src(1, "ERP", "dbo", "Buyer", "BuyerId"), src(2, "ERP", "dbo", "Buyer", "Mobile"), src(3, "ERP", "dbo", "Buyer", "Email"), {Ordinal: 4, Name: "Head"}},
		views: []mssql.ColumnSource{src(1, "ERP", "dbo", "Buyer", "BuyerId"), src(2, "ERP", "dbo", "Buyer", "Mobile"), src(3, "ERP", "crm", "v_buyer", "Email"), {Ordinal: 4, Name: "Head"}},
	}
	r := newRepo()
	r.cfg[settings.KeyQueryMaskEnabled] = "true"
	r.insts = map[uint]*model.Instance{5: {ID: 5, InstanceName: "erp-01", DBType: "mssql", Type: "master", Tags: canRead()}}
	r.maskCols = []model.DataMaskingColumn{
		keepCfg("ERP", "dbo.Buyer", "Mobile", 3, 4),
		keepCfg("ERP", "crm.v_buyer", "Email", 2, 0), // 配置写在视图上
		keepCfg("ERP", "dbo.Customer", "Head", 1, 0),
	}
	s := NewService(r, func(string, engine.Conn) (engine.Engine, error) { return eng, nil }, NewPrivAuthorizer(r, NewAutoExtractor(r)))
	res, err := s.Execute(context.Background(), admin, Request{InstanceID: 5, DB: "ERP", SQL: "select e.BuyerId, e.Mobile, v.Email, left(e.Mobile, 2) as Head from dbo.Buyer e join crm.v_buyer v on 1=1"})
	if err != nil {
		t.Fatal(err)
	}
	row := res.Rows[0]
	if row[0] != 1 || row[1] != "138***5678" || row[2] != "a@***" || row[3] != "1***" {
		t.Errorf("row=%v masked=%v mode=%q notice=%q", row, res.MaskedColumns, res.MaskMode, res.Notice)
	}
	if res.MaskMode != "source" {
		t.Errorf("mode=%q", res.MaskMode)
	}

	// 服务端无法分析（临时表等）：降级为按列名保守匹配，并提示
	eng.err = fmt.Errorf("sp_describe_first_result_set: 无法确定结果集")
	eng.data = [][]any{{1, "13812345678", "a@corp.com", "13"}}
	res, err = s.Execute(context.Background(), admin, Request{InstanceID: 5, DB: "ERP", SQL: "select BuyerId, Mobile, Email, left(Mobile, 2) as Head from #t"})
	if err != nil {
		t.Fatal(err)
	}
	if res.MaskMode != "column-name" || res.Rows[0][1] != "138***5678" || res.Rows[0][2] != "a@***" || !strings.Contains(res.Notice, "保守") {
		t.Errorf("降级: mode=%q row=%v notice=%q", res.MaskMode, res.Rows[0], res.Notice)
	}
}
