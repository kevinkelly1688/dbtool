// @author: kevinkelly
// @license: Apache Licence
// @file: button_test.go
// @time: 2026/10/28

package partman

import (
	"testing"

	"dbtool/internal/btnperm/btnpermtest"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// “立即执行维护”（partman.run）与“编辑配置”（partman.edit）是两个按钮。
func TestRunButton(t *testing.T) {
	e := newEnv()
	chk := btnpermtest.New()
	web := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){chk.Hook,
		func(s *server.Server, rt server.Routes) { register(s, rt, e.svc) }}})
	chk.Grant(2, permMenu, permChange)
	bob := web.Login("bob")
	if code, _ := bob.JSON("POST", "/api/v1/partman/configs/1/run", map[string]any{}, nil); code != 403 {
		t.Errorf("没有 partman.run: %d", code)
	}
	chk.Grant(2, "partman.run")
	if code, _ := bob.JSON("POST", "/api/v1/partman/configs/1/run", map[string]any{}, nil); code == 403 {
		t.Error("授予 partman.run 后不应是 403")
	}
}

// S2-C1：执行维护只认 partman.run，不要求 partman.edit（编辑）；编辑 / 启停只认 partman.edit。
func TestRunButtonIndependentOfEdit(t *testing.T) {
	e := newEnv()
	chk := btnpermtest.New()
	web := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){chk.Hook,
		func(s *server.Server, rt server.Routes) { register(s, rt, e.svc) }}})
	chk.Grant(2, permMenu) // 只有查看，没有编辑
	bob := web.Login("bob")
	type ov struct {
		CanEdit bool `json:"can_edit"`
		CanRun  bool `json:"can_run"`
	}
	var o ov
	bob.GetJSON("/api/v1/partman/overview", &o)
	if o.CanRun || o.CanEdit {
		t.Fatalf("只读用户: %+v", o)
	}
	chk.Grant(2, "partman.run")
	if code, _ := bob.JSON("POST", "/api/v1/partman/configs/1/run", map[string]any{}, nil); code == 403 {
		t.Error("只有 partman.run（没有编辑权限）应能执行，不应 403")
	}
	bob.GetJSON("/api/v1/partman/overview", &o)
	if !o.CanRun || o.CanEdit {
		t.Fatalf("授予 partman.run 后: can_run 应为真、can_edit 仍为假: %+v", o)
	}
	// 执行按钮码不让用户编辑 / 启停
	if code, _ := bob.JSON("PUT", "/api/v1/partman/configs/1", map[string]any{}, nil); code != 403 {
		t.Errorf("partman.run 不应允许编辑: %d", code)
	}
	if code, _ := bob.JSON("POST", "/api/v1/partman/configs/1/enable", map[string]any{"enable": true}, nil); code != 403 {
		t.Errorf("partman.run 不应允许启停: %d", code)
	}
}
