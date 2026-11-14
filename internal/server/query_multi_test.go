// @author: kevinkelly
// @license: Apache Licence
// @file: query_multi_test.go
// @time: 2026/10/28

package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"dbtool/internal/config"
	"dbtool/internal/crypto"
	"dbtool/internal/engine"
	"dbtool/internal/engine/postgres/pgtest"
	"dbtool/internal/model"
)

// multiServerRepo 在测试仓库上加一个指向假 PostgreSQL 的实例（ID 4，带 can_read 标签），ID 5 是带旧标签 read_view 的同类实例。
type multiServerRepo struct {
	*fakeRepo
	host string
	port int
}

func (r *multiServerRepo) extra() []model.Instance {
	tag := []model.InstanceTag{{TagCode: "can_read", TagName: "支持查询", Active: true}}
	rv := []model.InstanceTag{{TagCode: "read_view", TagName: "只读视图", Active: true}}
	return []model.Instance{
		{ID: 4, InstanceName: "pg-01", DBType: "pgsql", Type: "master", Host: r.host, Port: r.port, Tags: tag},
		{ID: 5, InstanceName: "pg-rv", DBType: "pgsql", Type: "master", Host: r.host, Port: r.port, Tags: rv},
	}
}
func (r *multiServerRepo) ListInstances() ([]model.Instance, error) {
	l, _ := r.fakeRepo.ListInstances()
	return append(l, r.extra()...), nil
}
func (r *multiServerRepo) GetInstance(id uint) (*model.Instance, error) {
	for _, i := range r.extra() {
		if i.ID == id {
			return &i, nil
		}
	}
	return r.fakeRepo.GetInstance(id)
}
func (r *multiServerRepo) EngineConn(i *model.Instance) engine.Conn {
	return engine.Conn{Host: i.Host, Port: i.Port, User: "u", DBName: "shop"}
}

func TestQueryMultiEngineAPI(t *testing.T) {
	pg := pgtest.New(t, func(sql string, _ []string) *pgtest.Result {
		low := strings.ToLower(sql)
		switch {
		case strings.Contains(sql, "FROM pg_catalog.pg_database"):
			return &pgtest.Result{Cols: pgtest.TextCols("datname"), Rows: [][]*string{pgtest.Cell("shop")}}
		case strings.Contains(sql, "n.nspname || '.' || c.relname"):
			return &pgtest.Result{Cols: pgtest.TextCols("t"), Rows: [][]*string{pgtest.Cell("public.orders")}}
		case strings.Contains(low, "from public.orders"):
			return &pgtest.Result{Cols: []pgtest.Col{{Name: "id", OID: 23}, {Name: "name", OID: 25}}, Rows: [][]*string{pgtest.Cell("1", "alice"), pgtest.Cell("2", pgtest.NullCell)}}
		}
		return nil
	})
	hash, err := crypto.MakeDjangoPassword("pw-123", "salty", 1000)
	if err != nil {
		t.Fatal(err)
	}
	base := &fakeRepo{users: map[string]*model.User{
		"root": {ID: 1, Username: "root", Password: hash, IsActive: true, IsSuperuser: true, Display: "管理员"},
	}}
	host, port := pg.Addr()
	repo := &multiServerRepo{fakeRepo: base, host: host, port: port}
	h, err := New(&config.Config{SecretKey: "unit-test-secret-key-unit-test-secret-key"}, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	e := &env{h: h, repo: base}
	sc, csrf, _ := e.login(t, "root", "pw-123")
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(sc)
		return e.do(req)
	}
	post := func(inst int, db, sql string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"instance_id": inst, "db": db, "sql": sql})
		req := httptest.NewRequest("POST", "/api/v1/query/execute", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		req.AddCookie(sc)
		return e.do(req)
	}

	if w := get("/api/v1/query/instances"); w.Code != 200 || !strings.Contains(w.Body.String(), `"db_type":"pgsql"`) {
		t.Errorf("实例列表: %d %s", w.Code, w.Body.String())
	}
	if w := get("/api/v1/query/instances/4/databases"); w.Code != 200 || !strings.Contains(w.Body.String(), `"data":["shop"]`) {
		t.Errorf("库列表: %d %s", w.Code, w.Body.String())
	}
	if w := get("/api/v1/query/instances/4/databases/shop/tables"); w.Code != 200 || !strings.Contains(w.Body.String(), `"data":["public.orders"]`) {
		t.Errorf("表列表: %d %s", w.Code, w.Body.String())
	}
	w := post(4, "shop", "select id, name from public.orders")
	var out struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
		t.Fatalf("执行: %d %s", w.Code, w.Body.String())
	}
	d := out.Data
	if d["kind"] != "pgsql" || d["stmt_kind"] != "select" || d["unmasked"] != true || d["row_count"] != float64(2) || d["truncated"] != false {
		t.Errorf("响应字段: %v", d)
	}
	cols, _ := json.Marshal(d["columns"])
	types, _ := json.Marshal(d["column_types"])
	rows, _ := json.Marshal(d["rows"])
	if string(cols) != `["id","name"]` || string(types) != `["int4","text"]` || string(rows) != `[[1,"alice"],[2,null]]` {
		t.Errorf("结果: %s %s %s", cols, types, rows)
	}
	if _, has := d["notice"]; has {
		t.Errorf("新类型不返回 notice 字段: %v", d)
	}
	if ws, _ := d["warnings"].([]any); len(ws) == 0 {
		t.Errorf("应带 warnings（自动补 LIMIT）: %v", d)
	}
	if base.logs != 1 {
		t.Errorf("成功的查询写 dbt_query_log: %d", base.logs)
	}
	// 写语句：400 bad_query，不到达服务端
	if w = post(4, "shop", "delete from public.orders"); w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"bad_query"`) {
		t.Errorf("写语句应被拒绝: %d %s", w.Code, w.Body.String())
	}
	if pg.Has("delete") {
		t.Error("被拒绝的语句不应发给服务端")
	}
	// 带旧标签 read_view 的实例与 can_read 实例一样可用，该标签不再限制任何入口
	if w := get("/api/v1/query/instances/5/databases"); w.Code != 200 || !strings.Contains(w.Body.String(), `"data":["shop"]`) {
		t.Errorf("read_view 库列表: %d %s", w.Code, w.Body.String())
	}
	if w := get("/api/v1/query/instances/5/databases/shop/tables"); w.Code != 200 || !strings.Contains(w.Body.String(), `"data":["public.orders"]`) {
		t.Errorf("read_view 表列表: %d %s", w.Code, w.Body.String())
	}
	if w = post(5, "shop", "select id, name from public.orders"); w.Code != 200 || strings.Contains(w.Body.String(), "read_view_only") {
		t.Errorf("read_view 执行: %d %s", w.Code, w.Body.String())
	}
	// 老类型的响应没有新字段
	if w := get("/api/v1/query/instances/1/databases/shop/meta"); strings.Contains(w.Body.String(), `"unmasked"`) {
		t.Errorf("MySQL 的元数据不应带 unmasked: %s", w.Body.String())
	}
}
