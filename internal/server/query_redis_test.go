// @author: kevinkelly
// @license: Apache Licence
// @file: query_redis_test.go
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
	"dbtool/internal/engine/redis/redistest"
	"dbtool/internal/model"
)

// redisServerRepo 在测试仓库上加一个指向假 Redis 的实例（ID 3）。
type redisServerRepo struct {
	*fakeRepo
	host string
	port int
}

func (r *redisServerRepo) inst() model.Instance {
	return model.Instance{ID: 3, InstanceName: "redis-01", DBType: "redis", Type: "master", Host: r.host, Port: r.port}
}
func (r *redisServerRepo) ListInstances() ([]model.Instance, error) {
	l, _ := r.fakeRepo.ListInstances()
	return append(l, r.inst()), nil
}
func (r *redisServerRepo) GetInstance(id uint) (*model.Instance, error) {
	if id == 3 {
		i := r.inst()
		return &i, nil
	}
	return r.fakeRepo.GetInstance(id)
}
func (r *redisServerRepo) EngineConn(i *model.Instance) engine.Conn {
	return engine.Conn{Host: i.Host, Port: i.Port}
}

func TestQueryRedisAPI(t *testing.T) {
	srv, err := redistest.New(func(a []string) any {
		switch strings.ToUpper(a[0]) {
		case "SELECT":
			return redistest.Simple("OK")
		case "GET":
			return "alice"
		case "CONFIG":
			return []any{"databases", "2"}
		case "INFO":
			return "# Keyspace\r\ndb0:keys=5,expires=0,avg_ttl=0\r\n"
		case "SCAN":
			return []any{"0", []any{"user:1", "user:2"}}
		case "TYPE":
			return redistest.Simple("string")
		}
		return redistest.Err("ERR unknown command")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	hash, err := crypto.MakeDjangoPassword("pw-123", "salty", 1000)
	if err != nil {
		t.Fatal(err)
	}
	base := &fakeRepo{users: map[string]*model.User{
		"root": {ID: 1, Username: "root", Password: hash, IsActive: true, IsSuperuser: true, Display: "管理员"},
		"bob":  {ID: 2, Username: "bob", Password: hash, IsActive: true},
	}}
	repo := &redisServerRepo{fakeRepo: base, host: srv.Host(), port: srv.Port()}
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

	if w := e.do(httptest.NewRequest("GET", "/api/v1/query/instances/3/redis/databases", nil)); w.Code != 401 {
		t.Errorf("未登录应 401: %d", w.Code)
	}
	// 实例列表里有 redis
	if w := get("/api/v1/query/instances"); w.Code != 200 || !strings.Contains(w.Body.String(), `"db_type":"redis"`) {
		t.Errorf("实例列表: %d %s", w.Code, w.Body.String())
	}
	// 库列表：编号 + 键数；通用库列表接口返回 "0".."N-1"
	w := get("/api/v1/query/instances/3/redis/databases")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `{"db":"0","keys":5}`) || !strings.Contains(w.Body.String(), `{"db":"1","keys":0}`) {
		t.Errorf("redis 库列表: %d %s", w.Code, w.Body.String())
	}
	if w = get("/api/v1/query/instances/3/databases"); w.Code != 200 || !strings.Contains(w.Body.String(), `"data":["0","1"]`) {
		t.Errorf("通用库列表: %d %s", w.Code, w.Body.String())
	}
	// key 浏览
	w = get("/api/v1/query/instances/3/databases/0/keys?match=user:*&count=10")
	var out struct {
		Data struct {
			Keys []struct{ Key, Arg, Type string } `json:"keys"`
			Done bool                              `json:"done"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 || len(out.Data.Keys) != 2 || out.Data.Keys[0].Key != "user:1" || out.Data.Keys[0].Type != "string" || !out.Data.Done {
		t.Errorf("key 浏览: %d %s", w.Code, w.Body.String())
	}
	if w = get("/api/v1/query/instances/3/databases/zz/keys"); w.Code != 400 {
		t.Errorf("库编号非法应 400: %d", w.Code)
	}
	// 执行：只读命令放行，写命令 400 且不连 Redis
	post := func(sql string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"instance_id": 3, "db": "0", "sql": sql})
		req := httptest.NewRequest("POST", "/api/v1/query/execute", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		req.AddCookie(sc)
		return e.do(req)
	}
	if w = post("get user:1"); w.Code != 200 || !strings.Contains(w.Body.String(), `"kind":"redis"`) || !strings.Contains(w.Body.String(), `alice`) || !strings.Contains(w.Body.String(), `"executed_sql":"GET user:1"`) {
		t.Errorf("执行 GET: %d %s", w.Code, w.Body.String())
	}
	if w = post("SET user:1 x"); w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"bad_query"`) || !strings.Contains(w.Body.String(), "修改数据") {
		t.Errorf("写命令应被拒绝: %d %s", w.Code, w.Body.String())
	}
	if base.logs != 1 {
		t.Errorf("只有成功的命令写 dbt_query_log: %d", base.logs)
	}
	// 非管理员且没有关联实例
	bsc, _, _ := e.login(t, "bob", "pw-123")
	req := httptest.NewRequest("GET", "/api/v1/query/instances/3/databases/0/keys", nil)
	req.AddCookie(bsc)
	if w = e.do(req); w.Code != 403 {
		t.Errorf("未关联实例应 403: %d %s", w.Code, w.Body.String())
	}
}
