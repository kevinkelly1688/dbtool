// @author: kevinkelly
// @license: Apache Licence
// @file: button_test.go
// @time: 2026/10/28

package perf

import (
	"testing"

	"dbtool/internal/btnperm/btnpermtest"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

func newBtnEnv(t *testing.T) (*servertest.Env, *fx, *btnpermtest.Checker) {
	t.Helper()
	f := newFx(t)
	chk := btnpermtest.New()
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){chk.Hook,
		func(s *server.Server, rt server.Routes) { registerWith(s, rt, f.svc, f.repo) }}})
	return env, f, chk
}

// 工单操作按钮：每个操作对应一个按钮，只有查看按钮不够。
func TestActionButtons(t *testing.T) {
	env, f, chk := newBtnEnv(t)
	id := f.newIssue()
	chk.Grant(2, btnView)
	bob := env.Login("bob")
	path := "/api/v1/perf/issues/" + itoaU(id) + "/actions"

	for action, button := range actionButtons {
		if code, _ := bob.JSON("POST", path, map[string]any{"action": action}, nil); code != 403 {
			t.Errorf("操作 %s：没有按钮 %s 应 403，得到 %d", action, button, code)
		}
	}
	// 授予后通过按钮校验（之后可能因业务规则返回 400 / 409，但不是 403）
	chk.Grant(2, "perf.comment")
	if code, _ := bob.JSON("POST", path, map[string]any{"action": "comment", "remark": "看了一下"}, nil); code != 200 {
		t.Errorf("授予 perf.comment 后评论: %d", code)
	}
	chk.Grant(2, "perf.assign")
	if code, _ := bob.JSON("POST", path, map[string]any{"action": "assign"}, nil); code == 403 {
		t.Error("授予 perf.assign 后不应是 403")
	}
	// 建单 / 重新诊断
	if code, _ := bob.JSON("POST", "/api/v1/perf/issues", map[string]any{}, nil); code != 403 {
		t.Errorf("没有 perf.create: %d", code)
	}
	if code, _ := bob.JSON("POST", "/api/v1/perf/issues/from-slowlog", map[string]any{}, nil); code != 403 {
		t.Errorf("没有 perf.create（慢日志建单）: %d", code)
	}
	chk.Grant(2, "perf.create")
	if code, _ := bob.JSON("POST", "/api/v1/perf/issues", map[string]any{}, nil); code == 403 {
		t.Error("授予 perf.create 后不应是 403")
	}
	if code, _ := bob.JSON("POST", "/api/v1/perf/issues/"+itoaU(id)+"/diagnose", map[string]any{}, nil); code != 403 {
		t.Errorf("没有 perf.reopen: %d", code)
	}
}

// 配置页面与接口按 perfcfg.* 按钮判断。
func TestConfigButtons(t *testing.T) {
	env, _, chk := newBtnEnv(t)
	bob := env.Login("bob")
	if code, _ := bob.JSON("GET", "/api/v1/perf/config/sla", nil, nil); code != 403 {
		t.Errorf("没有 perfcfg.view: %d", code)
	}
	if code, _ := bob.Page("/perf/config/sla"); code != 403 {
		t.Errorf("页面没有 perfcfg.view: %d", code)
	}
	chk.Grant(2, "perfcfg.view")
	if code, _ := bob.JSON("GET", "/api/v1/perf/config/sla", nil, nil); code != 200 {
		t.Errorf("授予 perfcfg.view: %d", code)
	}
	if code, _ := bob.Page("/perf/config/sla"); code != 200 {
		t.Errorf("页面授予 perfcfg.view: %d", code)
	}
	for _, c := range []struct{ method, path, button string }{
		{"PUT", "/api/v1/perf/config/sla", "perfcfg.sla"},
		{"POST", "/api/v1/perf/config/collect", "perfcfg.collect"},
		{"POST", "/api/v1/perf/config/teams", "perfcfg.team"},
		{"POST", "/api/v1/perf/config/routes", "perfcfg.team"},
	} {
		if code, _ := bob.JSON(c.method, c.path, map[string]any{}, nil); code != 403 {
			t.Errorf("%s %s 没有 %s: %d", c.method, c.path, c.button, code)
		}
		chk.Grant(2, c.button)
		if code, _ := bob.JSON(c.method, c.path, map[string]any{}, nil); code == 403 {
			t.Errorf("%s %s 授予 %s 后不应是 403", c.method, c.path, c.button)
		}
		chk.Revoke(2, c.button)
	}
}
