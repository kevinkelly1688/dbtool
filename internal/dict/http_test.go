// @author: kevinkelly
// @license: Apache Licence
// @file: http_test.go
// @time: 2026/10/28

package dict

import (
	"encoding/json"
	"testing"

	"dbtool/internal/btnperm/btnpermtest"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

type httpEnv struct {
	*env
	web *servertest.Env
}

func newHTTPEnv(t *testing.T) *httpEnv {
	e := newEnv()
	web := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){
		func(s *server.Server, rt server.Routes) { register(s, rt, e.svc) },
	}})
	return &httpEnv{env: e, web: web}
}

func (h *httpEnv) grant(uid uint, codes ...string) {
	if h.web.Repo.Perms[uid] == nil {
		h.web.Repo.Perms[uid] = map[string]bool{}
	}
	for _, c := range codes {
		h.web.Repo.Perms[uid][c] = true
	}
}

type apiResp struct {
	Data    json.RawMessage `json:"data"`
	Total   int             `json:"total"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
}

func get(c *servertest.Client, path string) (int, apiResp) {
	w := c.Do("GET", path, nil)
	var r apiResp
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	return w.Code, r
}

// 装配了按钮权限解析器时（正式环境）：没有 query.objinfo 的用户看不了对象信息与可编程对象，授予后可以。
func TestHTTPObjectInfoNeedsButton(t *testing.T) {
	chk := btnpermtest.New()
	e := newEnv()
	web := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){chk.Hook,
		func(s *server.Server, rt server.Routes) { register(s, rt, e.svc) }}})
	paths := []string{
		"/api/v1/query/instances/1/databases/shop/object-info?name=orders",
		"/api/v1/query/instances/1/databases/shop/programmables",
	}
	bob := web.Login("bob") // 用户 ID 2
	for _, p := range paths {
		if code, r := get(bob, p); code != 403 {
			t.Errorf("没有 query.objinfo 应 403: %s -> %d %+v", p, code, r)
		}
	}
	chk.Grant(2, "query.objinfo")
	for _, p := range paths {
		if code, _ := get(bob, p); code == 403 {
			t.Errorf("授予 query.objinfo 后不应 403: %s", p)
		}
	}
}
