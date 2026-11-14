// @author: kevinkelly
// @license: Apache Licence
// @file: view_mask_multi_test.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"dbtool/internal/masking"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// ClickHouse 与 MongoDB 的视图脱敏（SQL 引擎的见 view_mask_test.go）。

var chViewCols = [][2]string{{"database", "String"}, {"name", "String"}, {"engine", "String"}, {"as_select", "String"}}

func colIdx(t *testing.T, res *Result, name string) int {
	t.Helper()
	for i, c := range res.Columns {
		if c == name {
			return i
		}
	}
	t.Fatalf("结果里没有列 %q: %v", name, res.Columns)
	return -1
}

// ---- ClickHouse ----

func TestCHMaskViewRenamedColumn(t *testing.T) {
	e := newMaskEnv(t, nil, keepCfg("*", "*", "phone", 3, 4))
	e.ch.on("as_select", chViewCols, [][]any{
		{"app", "v_users", "View", "SELECT id, phone AS mobile FROM app.users"},
		{"app", "mv_daily", "MaterializedView", "SELECT toDate(ts) AS d, count() AS c FROM app.events GROUP BY d"},
	})
	e.ch.on("from v_users", [][2]string{{"id", "UInt64"}, {"mobile", "String"}}, [][]any{{"1", "13812345678"}})
	res := e.run(t, chID, "app", "select id, mobile from v_users")
	if row := res.Rows[0]; row[0] != "1" || row[1] != "138***5678" || !reflect.DeepEqual(res.MaskedColumns, []string{"mobile"}) {
		t.Errorf("视图把 phone 改名为 mobile 也应脱敏: row=%v masked=%v warnings=%v", row, res.MaskedColumns, res.Warnings)
	}
	var ask string
	for _, b := range e.ch.requests() {
		if strings.Contains(b, "as_select") {
			ask = b
			break
		}
	}
	if !strings.Contains(ask, "(database = 'app' AND name = 'v_users')") {
		t.Errorf("应带着语句里的视图名和当前库去查 system.tables: %s", ask)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("不该有提示: %v", res.Warnings)
	}

	// 物化视图里的表达式列、不含敏感字段的视图不误伤
	e.ch.on("from mv_daily", [][2]string{{"d", "Date"}, {"c", "UInt64"}}, [][]any{{"2026-10-22", "7"}})
	res = e.run(t, chID, "app", "select d, c from mv_daily")
	if res.Rows[0][1] != "7" || len(res.MaskedColumns) != 0 {
		t.Errorf("不含敏感字段的视图: %v %v", res.Rows[0], res.MaskedColumns)
	}
}

func TestCHMaskViewNestedAndExpression(t *testing.T) {
	e := newMaskEnv(t, nil, keepCfg("*", "*", "phone", 3, 4))
	e.ch.on("as_select", chViewCols, [][]any{
		{"app", "v2", "View", "SELECT concat(m, '') AS m2 FROM app.v1"},
		{"app", "v1", "View", "SELECT toString(phone) AS m FROM app.users"},
	})
	e.ch.on("from v2", [][2]string{{"m2", "String"}}, [][]any{{"13812345678"}})
	res := e.run(t, chID, "app", "select m2 from v2")
	if res.Rows[0][0] != "138***5678" {
		t.Errorf("视图套视图、视图里套函数: %v warnings=%v", res.Rows[0], res.Warnings)
	}
}

func TestCHMaskViewFailures(t *testing.T) {
	const q = "select id, mobile from v_users"
	newEnv := func() *maskEnv {
		e := newMaskEnv(t, nil, keepCfg("*", "*", "phone", 3, 4))
		e.ch.on("from v_users", [][2]string{{"id", "UInt64"}, {"mobile", "String"}}, [][]any{{"1", "13812345678"}})
		return e
	}
	// 没有权限看定义（as_select 为空）：默认放行并提示，开启 query.block_on_mask_error 阻断
	e := newEnv()
	e.ch.on("as_select", chViewCols, [][]any{{"app", "v_users", "View", ""}})
	res := e.run(t, chID, "app", q)
	if res.Rows[0][1] != "13812345678" || !strings.Contains(strings.Join(res.Warnings, "\n"), "app.v_users") {
		t.Errorf("定义读不到应放行并提示: row=%v warnings=%v", res.Rows[0], res.Warnings)
	}
	e.repo.cfg[settings.KeyQueryBlockOnMaskError] = "true"
	if _, err := e.svc.Execute(context.Background(), admin, Request{InstanceID: chID, DB: "app", SQL: q}); code(err) != "masking_error" {
		t.Errorf("应阻断: %v", err)
	}

	// 目录查询没有权限（497）同样是失败
	e = newEnv()
	e.ch.onErr("as_select", 497, "reader: Not enough privileges")
	res = e.run(t, chID, "app", q)
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "读取视图定义失败") {
		t.Errorf("目录查询出错应提示: %v", res.Warnings)
	}

	// 老版本没有 as_select 这一列（47）：不展开，不算失败，普通列照常脱敏
	e = newEnv()
	e.repo.cfg[settings.KeyQueryBlockOnMaskError] = "true"
	e.ch.onErr("as_select", 47, "Missing columns: 'as_select' while processing query")
	e.ch.on("from events", [][2]string{{"phone", "String"}}, [][]any{{"13812345678"}})
	res = e.run(t, chID, "app", "select phone from events")
	if res.Rows[0][0] != "138***5678" || len(res.Warnings) != 0 {
		t.Errorf("目录不可用不应当失败: %v %v", res.Rows[0], res.Warnings)
	}
}

// ---- MongoDB ----

func mongoPhoneCfg(coll string) model.DataMaskingColumn {
	return model.DataMaskingColumn{Active: true, TableSchema: "app", TableName_: coll, ColumnName: "profile.phone", Kind: masking.KindKeep, KeepPrefix: 3, KeepSuffix: 4}
}

func project(kv ...any) bson.D {
	d := bson.D{}
	for i := 0; i+1 < len(kv); i += 2 {
		d = append(d, bson.E{Key: kv[i].(string), Value: kv[i+1]})
	}
	return bson.D{{Key: "$project", Value: d}}
}

func TestMongoMaskViewRenamedField(t *testing.T) {
	e := newMaskEnv(t, nil, mongoPhoneCfg("users"))
	db := e.mgo.DB("app")
	db.Coll("users", bson.D{{Key: "_id", Value: int32(1)}, {Key: "profile", Value: bson.D{{Key: "phone", Value: "13812345678"}}}})
	// v1 把 profile.phone 改名为 p；v2 建在 v1 上，再改名为 q；两个视图查到的文档都是经过管道之后的样子
	db.View("v1", "users", []bson.D{project("p", "$profile.phone", "name", int32(1))},
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "p", Value: "13812345678"}, {Key: "name", Value: "a"}})
	db.View("v2", "v1", []bson.D{project("q", "$p")}, bson.D{{Key: "_id", Value: int32(1)}, {Key: "q", Value: "13812345678"}})

	res := e.run(t, mongoID, "app", `db.v1.find({})`)
	if got := res.Rows[0][colIdx(t, res, "p")]; got != "138***5678" || !reflect.DeepEqual(res.MaskedColumns, []string{"p"}) || res.MaskMode != "path" {
		t.Errorf("视图 v1 把 profile.phone 改名为 p: p=%v masked=%v warnings=%v", got, res.MaskedColumns, res.Warnings)
	}
	if got := res.Rows[0][colIdx(t, res, "name")]; got != "a" {
		t.Errorf("不敏感的字段不动: %v", got)
	}
	res = e.run(t, mongoID, "app", `db.v2.find({})`)
	if got := res.Rows[0][colIdx(t, res, "q")]; got != "138***5678" {
		t.Errorf("视图套视图: q=%v masked=%v warnings=%v", got, res.MaskedColumns, res.Warnings)
	}
	// 对视图自己的聚合：视图的管道在前，语句自己的管道在后，再改名也认得出
	db.View("v3", "users", []bson.D{project("p", "$profile.phone")}, bson.D{{Key: "_id", Value: int32(1)}, {Key: "z", Value: "13812345678"}})
	res = e.run(t, mongoID, "app", `db.v3.aggregate([{$project: {z: "$p"}}])`)
	if len(res.Rows) == 0 {
		t.Fatalf("没有结果: %+v", res)
	}
	if got := res.Rows[0][colIdx(t, res, "z")]; got != "138***5678" {
		t.Errorf("语句自己的管道改名: z=%v masked=%v warnings=%v", got, res.MaskedColumns, res.Warnings)
	}
}

func TestMongoMaskViewThroughLookup(t *testing.T) {
	e := newMaskEnv(t, nil, mongoPhoneCfg("users"))
	db := e.mgo.DB("app")
	// orders 的文档里嵌着 $lookup 进来的视图文档（假后端不求值管道，直接给出来）
	db.Coll("orders", bson.D{{Key: "_id", Value: int32(1)}, {Key: "u", Value: bson.A{bson.D{{Key: "p", Value: "13812345678"}}}}})
	db.View("v_users", "users", []bson.D{project("p", "$profile.phone")})
	res := e.run(t, mongoID, "app", `db.orders.aggregate([{$lookup: {from: "v_users", localField: "uid", foreignField: "_id", as: "u"}}])`)
	if got := fmtAny(res.Rows[0][colIdx(t, res, "u")]); !strings.Contains(got, "138***5678") || strings.Contains(got, "13812345678") {
		t.Errorf("$lookup 进来的视图文档里改了名的字段: %s warnings=%v", got, res.Warnings)
	}

	// 视图自己的管道里 $lookup 到配置过的集合：那个集合的配置也要生效
	e2 := newMaskEnv(t, nil, mongoPhoneCfg("users"))
	db2 := e2.mgo.DB("app")
	db2.Coll("users")
	db2.View("v_orders", "orders", []bson.D{{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "users"}, {Key: "localField", Value: "uid"}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: "u"}}}}},
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "u", Value: bson.A{bson.D{{Key: "profile", Value: bson.D{{Key: "phone", Value: "13812345678"}}}}}}})
	res = e2.run(t, mongoID, "app", `db.v_orders.find({})`)
	if got := fmtAny(res.Rows[0][colIdx(t, res, "u")]); !strings.Contains(got, "138***5678") || strings.Contains(got, "13812345678") {
		t.Errorf("视图管道里 $lookup 到配置过的集合: %s warnings=%v", got, res.Warnings)
	}
}

func fmtAny(v any) string { return strings.TrimSpace(strings.ReplaceAll(toText(v), "\n", " ")) }

func toText(v any) string {
	b, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: v}}, false, false)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestMongoMaskViewFailures(t *testing.T) {
	newEnv := func() *maskEnv {
		e := newMaskEnv(t, nil, mongoPhoneCfg("users"))
		db := e.mgo.DB("app")
		db.Coll("users", bson.D{{Key: "_id", Value: int32(1)}, {Key: "profile", Value: bson.D{{Key: "phone", Value: "13812345678"}}}})
		db.Coll("broken", bson.D{{Key: "_id", Value: int32(1)}, {Key: "p", Value: "13812345678"}})
		for i := range db.Infos { // 视图的 options 里没有 viewOn：读不到定义
			if db.Infos[i].Name == "broken" {
				db.Infos[i].Type = "view"
				db.Infos[i].Options = bson.D{{Key: "pipeline", Value: bson.A{}}}
			}
		}
		return e
	}
	e := newEnv()
	res := e.run(t, mongoID, "app", `db.broken.find({})`)
	if res.Rows[0][colIdx(t, res, "p")] != "13812345678" || !strings.Contains(strings.Join(res.Warnings, "\n"), "broken") {
		t.Errorf("定义读不到应放行并提示: row=%v warnings=%v", res.Rows[0], res.Warnings)
	}
	e.repo.cfg[settings.KeyQueryBlockOnMaskError] = "true"
	if _, err := e.svc.Execute(context.Background(), admin, Request{InstanceID: mongoID, DB: "app", SQL: `db.broken.find({})`}); code(err) != "masking_error" {
		t.Errorf("应阻断: %v", err)
	}

	// listCollections 出错（没有权限等）：其余的字段照常按配置脱敏，提示读取失败
	e = newEnv()
	e.mgo.DB("app").Err = errors.New("not authorized on app to execute command { listCollections }")
	res = e.run(t, mongoID, "app", `db.users.find({})`)
	if got := fmtAny(res.Rows[0][colIdx(t, res, "profile")]); !strings.Contains(got, "138***5678") {
		t.Errorf("普通集合照常脱敏: %s", got)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "读取视图定义失败") {
		t.Errorf("目录查询出错应提示: %v", res.Warnings)
	}
	e.repo.cfg[settings.KeyQueryBlockOnMaskError] = "true"
	if _, err := e.svc.Execute(context.Background(), admin, Request{InstanceID: mongoID, DB: "app", SQL: `db.users.find({})`}); code(err) != "masking_error" {
		t.Errorf("应阻断: %v", err)
	}
}

// 没有配置脱敏字段时不去查视图；语句只涉及普通集合时也只查一次。
func TestMongoMaskViewLookupCost(t *testing.T) {
	e := newMaskEnv(t, nil) // 没有任何脱敏配置
	db := e.mgo.DB("app")
	db.Coll("users", bson.D{{Key: "_id", Value: int32(1)}})
	e.run(t, mongoID, "app", `db.users.find({})`)
	if db.ListCalls != 0 {
		t.Errorf("没有脱敏配置不该查视图: %d 次", db.ListCalls)
	}
	e = newMaskEnv(t, nil, mongoPhoneCfg("users"))
	db = e.mgo.DB("app")
	db.Coll("users", bson.D{{Key: "_id", Value: int32(1)}})
	e.run(t, mongoID, "app", `db.users.find({})`)
	if db.ListCalls != 1 {
		t.Errorf("普通集合只查一次: %d 次", db.ListCalls)
	}
}

// 视图成环（MongoDB 不允许，但分析不能依赖这一点）：不死循环。
func TestExpandMongoViewsCycle(t *testing.T) {
	e := newMaskEnv(t, nil, mongoPhoneCfg("users"))
	db := e.mgo.DB("app")
	db.View("a", "b", nil)
	db.View("b", "a", nil)
	eng, err := e.svc.open("mongo", e.repo.EngineConn(e.repo.insts[mongoID]))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	x, verr := e.svc.expandMongoViews(context.Background(), eng, "app", []string{"a"})
	if verr != nil || x == nil || len(x.collections) != 2 {
		t.Errorf("成环: x=%+v err=%v", x, verr)
	}
}

// 对象名去重的键：ClickHouse / PostgreSQL 区分大小写（Users 与 users 是两个对象），MySQL 系 / SQL Server 不区分。
func TestExpandViewsKeyCaseSensitivity(t *testing.T) {
	s := newViewSvc(viewRepo(), &viewEngine{}, nil)
	for _, c := range []struct {
		dbType, sql string
		want        int
	}{
		{"clickhouse", "select 1 from Users, users", 2},
		{"pgsql", "select 1 from Users, users", 1}, // 没加引号的名字服务端折成小写，是同一个对象
		{"pgsql", `select 1 from "Users", users`, 2},
		{"mysql", "select 1 from Users, users", 1},
		{"mssql", "select 1 from Users, users", 1},
	} {
		e := &viewEngine{}
		if _, err := s.expandViews(context.Background(), e, c.dbType, "app", c.sql); err != nil {
			t.Fatal(err)
		}
		if len(e.calls) != 1 || len(e.calls[0]) != c.want {
			t.Errorf("%s %s: 目录查询 %+v，期望一次带 %d 个名字", c.dbType, c.sql, e.calls, c.want)
		}
	}
}
