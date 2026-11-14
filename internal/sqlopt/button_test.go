// @author: kevinkelly
// @license: Apache Licence
// @file: button_test.go
// @time: 2026/10/28

package sqlopt

import (
	"testing"

	"dbtool/internal/btnperm/btnpermtest"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// 优化工具页的“执行计划 / 索引建议 / 格式化”由 optimize.explain 控制；单独授予 slowquery.advise（也满足 menu_sqladvisor）不能用。
func TestExplainButton(t *testing.T) {
	h := newHTTPForButtons(t)
	chk := h.chk
	h.env.Repo.Perms[2] = map[string]bool{PermMenuAdvisor: true}
	bob := h.env.Login("bob")
	for _, p := range []string{"/api/v1/sqlopt/explain", "/api/v1/sqlopt/advise", "/api/v1/sqlopt/format"} {
		if code, _ := bob.JSON("POST", p, map[string]any{}, nil); code != 403 {
			t.Errorf("%s 没有 optimize.explain: %d", p, code)
		}
	}
	if code, _ := bob.Page("/sqladvisor"); code != 403 {
		t.Errorf("页面没有 optimize.explain: %d", code)
	}
	chk.Grant(2, "optimize.explain")
	for _, p := range []string{"/api/v1/sqlopt/explain", "/api/v1/sqlopt/advise", "/api/v1/sqlopt/format"} {
		if code, _ := bob.JSON("POST", p, map[string]any{}, nil); code == 403 {
			t.Errorf("%s 授予后不应是 403", p)
		}
	}
	if code, _ := bob.Page("/sqladvisor"); code != 200 {
		t.Errorf("页面授予后: %d", code)
	}
}

type btnEnv struct {
	env *servertest.Env
	chk *btnpermtest.Checker
}

func newHTTPForButtons(t *testing.T) *btnEnv {
	t.Helper()
	chk := btnpermtest.New()
	var svc *Service
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){chk.Hook,
		func(s *server.Server, rt server.Routes) {
			svc = NewService(s.Repo(), nil, nil, nil)
			register(s, rt, svc)
		}}})
	return &btnEnv{env: env, chk: chk}
}
