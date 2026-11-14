// @author: kevinkelly
// @license: Apache Licence
// @file: meta_test.go
// @time: 2026/10/28

package mongo

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func metaFixture() (*Engine, *fakeBackend) {
	be := newFake()
	d := be.db("app")
	u := d.coll("users",
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "name", Value: "a"}, {Key: "age", Value: int32(30)}, {Key: "tag", Value: nil}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "name", Value: "b"}, {Key: "age", Value: 31.5}, {Key: "email", Value: "x@y"}},
		bson.D{{Key: "_id", Value: int32(3)}, {Key: "name", Value: int32(5)}, {Key: "age", Value: int32(40)}},
	)
	u.stats = bson.D{{Key: "count", Value: int32(3)}, {Key: "size", Value: int32(300)}, {Key: "storageSize", Value: int64(4096)}, {Key: "totalIndexSize", Value: int64(8192)}, {Key: "nindexes", Value: int32(2)}}
	u.indexes = []bson.D{
		{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "_id", Value: int32(1)}}}, {Key: "name", Value: "_id_"}},
		{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "email", Value: int32(1)}, {Key: "age", Value: int32(-1)}}}, {Key: "name", Value: "email_1_age_-1"}, {Key: "unique", Value: true}, {Key: "sparse", Value: true}, {Key: "partialFilterExpression", Value: bson.D{{Key: "age", Value: bson.D{{Key: "$gt", Value: int32(18)}}}}}},
		{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "ts", Value: int32(1)}}}, {Key: "name", Value: "ts_1"}, {Key: "expireAfterSeconds", Value: int32(3600)}},
		{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "body", Value: "text"}}}, {Key: "name", Value: "body_text"}},
	}
	d.coll("system.views")
	d.infos = append(d.infos, CollectionInfo{Name: "v1", Type: "view", Options: bson.D{{Key: "viewOn", Value: "users"}, {Key: "pipeline", Value: bson.A{}}}})
	d.coll("cap").stats = bson.D{{Key: "count", Value: int32(0)}}
	for i := range d.infos {
		if d.infos[i].Name == "cap" {
			d.infos[i].Options = bson.D{{Key: "capped", Value: true}, {Key: "size", Value: int32(1048576)}}
		}
	}
	return NewWithBackend(be), be
}

func TestAllDatabasesAndTables(t *testing.T) {
	e, be := metaFixture()
	be.db("zeta")
	dbs, err := e.AllDatabases(context.Background())
	if err != nil || strings.Join(dbs, ",") != "app,zeta" {
		t.Fatalf("dbs=%v err=%v", dbs, err)
	}
	tbs, err := e.AllTables(context.Background(), "app")
	if err != nil || strings.Join(tbs, ",") != "cap,users,v1" {
		t.Fatalf("tables=%v err=%v", tbs, err)
	}
	if _, err := e.AllTables(context.Background(), "admin"); err == nil {
		t.Error("系统库应拒绝")
	}
	be.listErr = errors.New("boom")
	if _, err := e.AllDatabases(context.Background()); err == nil {
		t.Error("应上抛错误")
	}
}

func TestTableInfos(t *testing.T) {
	e, be := metaFixture()
	infos, err := e.TableInfos(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]TableInfo{}
	for _, ti := range infos {
		m[ti.Name] = ti
	}
	if len(infos) != 3 {
		t.Fatalf("infos=%d", len(infos))
	}
	u := m["users"]
	if !u.HasStats || u.RowsEstimate != 3 || u.DataBytes != 300 || u.StorageBytes != 4096 || u.IndexBytes != 8192 || u.IndexCount != 2 || u.Type != "collection" || u.CreateTime != nil {
		t.Errorf("users=%+v", u)
	}
	v := m["v1"]
	if v.Type != "view" || v.HasStats || !strings.Contains(v.Comment, "users") || v.Engine != "view" {
		t.Errorf("v1=%+v", v)
	}
	c := m["cap"]
	if c.Engine != "capped" || c.Type != "collection" || !strings.Contains(c.Options, `"capped":true`) {
		t.Errorf("cap=%+v", c)
	}
	for _, call := range be.other {
		if call == "collStats:v1" {
			t.Error("视图不应执行 collStats")
		}
	}
}

func TestTableInfosStatsFailureTolerated(t *testing.T) {
	e, be := metaFixture()
	be.db("app").colls["users"].err = &codeErr{code: 13, msg: "not authorized"}
	infos, err := e.TableInfos(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	for _, ti := range infos {
		if ti.Name == "users" && ti.HasStats {
			t.Error("collStats 失败时 HasStats 应为 false")
		}
	}
}

func TestColumnInfos(t *testing.T) {
	e, be := metaFixture()
	cols, err := e.ColumnInfos(context.Background(), "app", "users")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ColumnInfo{}
	var order []string
	for _, c := range cols {
		got[c.Name] = c
		order = append(order, c.Name)
	}
	if strings.Join(order, ",") != "_id,name,age,tag,email" {
		t.Fatalf("字段顺序=%v", order)
	}
	if c := got["_id"]; c.Type != "int" || !c.IsPK || c.KeySeq != 1 || c.Nullable {
		t.Errorf("_id=%+v", c)
	}
	if c := got["name"]; c.Type != "string|int" || c.Nullable {
		t.Errorf("name=%+v", c)
	}
	if c := got["age"]; c.Type != "int|double" || c.Nullable || !strings.Contains(c.Comment, "采样 3 个文档，3 个含该字段") {
		t.Errorf("age=%+v", c)
	}
	if c := got["tag"]; c.Type != "null" || !c.Nullable {
		t.Errorf("tag=%+v", c)
	}
	if c := got["email"]; !c.Nullable || !strings.Contains(c.Comment, "1 个含该字段") {
		t.Errorf("email=%+v", c)
	}
	names, err := e.AllColumns(context.Background(), "app", "users")
	if err != nil || strings.Join(names, ",") != "_id,name,age,tag,email" {
		t.Errorf("AllColumns=%v %v", names, err)
	}
	// 采样只取前 100 个文档
	if f := be.finds[0].Spec; f.Limit != SampleDocs {
		t.Errorf("采样 limit=%d", f.Limit)
	}
	if _, err := e.ColumnInfos(context.Background(), "app", "system.users"); err == nil {
		t.Error("系统集合应拒绝")
	}
	if _, err := e.ColumnInfos(context.Background(), "local", "x"); err == nil {
		t.Error("系统库应拒绝")
	}
}

func TestColumnInfosSampleCap(t *testing.T) {
	be := newFake()
	be.db("app").coll("many", mkDocs(300)...)
	e := NewWithBackend(be)
	cols, err := e.ColumnInfos(context.Background(), "app", "many")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cols[0].Comment, "采样 100 个文档") {
		t.Errorf("comment=%q", cols[0].Comment)
	}
}

func TestIndexes(t *testing.T) {
	e, _ := metaFixture()
	idx, err := e.Indexes(context.Background(), "app", "users")
	if err != nil || len(idx) != 4 {
		t.Fatalf("idx=%v err=%v", idx, err)
	}
	if idx[0].Name != "_id_" || len(idx[0].Keys) != 1 || idx[0].Keys[0] != (IndexKey{"_id", "1"}) {
		t.Errorf("idx0=%+v", idx[0])
	}
	i1 := idx[1]
	if !i1.Unique || !i1.Sparse || len(i1.Keys) != 2 || i1.Keys[1] != (IndexKey{"age", "-1"}) || !strings.Contains(i1.Partial, `"$gt":18`) {
		t.Errorf("idx1=%+v", i1)
	}
	if idx[2].TTLSeconds == nil || *idx[2].TTLSeconds != 3600 {
		t.Errorf("idx2=%+v", idx[2])
	}
	if idx[3].Keys[0].Order != "text" {
		t.Errorf("idx3=%+v", idx[3])
	}
	if !json.Valid([]byte(idx[1].Definition)) {
		t.Errorf("Definition 应是合法 JSON：%s", idx[1].Definition)
	}
}

func TestCreateStatement(t *testing.T) {
	e, _ := metaFixture()
	s, err := e.CreateStatement(context.Background(), "app", "users")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatalf("应是合法 JSON：%v\n%s", err, s)
	}
	if !strings.Contains(doc["note"].(string), "不是可执行语句") || doc["collection"] != "users" || len(doc["indexes"].([]any)) != 4 {
		t.Errorf("doc=%v", doc)
	}
	// 视图：没有索引，带 viewOn
	s, err = e.CreateStatement(context.Background(), "app", "v1")
	if err != nil || !strings.Contains(s, `"viewOn": "users"`) || !strings.Contains(s, `"type": "view"`) {
		t.Errorf("view: %v\n%s", err, s)
	}
	if _, err := e.CreateStatement(context.Background(), "app", "nope"); err == nil {
		t.Error("不存在的集合应报错")
	}
}

func TestServerVersionAndPing(t *testing.T) {
	e, be := metaFixture()
	if v, err := e.ServerVersion(context.Background()); err != nil || v != "7.0.5" {
		t.Errorf("%q %v", v, err)
	}
	if err := e.TestConnection(context.Background()); err != nil {
		t.Error(err)
	}
	be.pingErr = errors.New("no reachable servers")
	if err := e.TestConnection(context.Background()); err == nil {
		t.Error("应返回错误")
	}
}
