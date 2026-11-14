// @author: kevinkelly
// @license: Apache Licence
// @file: button_test.go
// @time: 2026/10/28

package inspect

import (
	"testing"

	"dbtool/internal/btnperm/btnpermtest"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// 原仅超管的按钮（立即巡检 / 删除报告）现在可以单独授予；定时计划的三个按钮（新建 / 编辑、暂停 / 启用 / 删除、立即运行）各自单独校验。
func TestButtonsGrantable(t *testing.T) {
	e := newEnv()
	chk := btnpermtest.New()
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){chk.Hook, registerWith(e.svc, testHooks())}})
	chk.Grant(2, "inspect.view")
	bob := env.Login("bob")

	cases := []struct{ method, path, button string }{
		{"POST", "/api/v1/inspection/reports/run", "inspect.run"},
		{"POST", "/api/v1/inspection/reports/delete", "inspect.delete"},
		{"POST", "/api/v1/inspection/plans", "inspect.plan_edit"},
		{"POST", "/api/v1/inspection/plans/preview", "inspect.plan_edit"},
		{"PUT", "/api/v1/inspection/plans/1", "inspect.plan_edit"},
		{"DELETE", "/api/v1/inspection/plans/1", "inspect.plan_toggle"},
		{"POST", "/api/v1/inspection/plans/1/enable", "inspect.plan_toggle"},
		{"POST", "/api/v1/inspection/plans/1/run", "inspect.plan_run"},
	}
	for _, c := range cases {
		if code, _ := bob.JSON(c.method, c.path, map[string]any{}, nil); code != 403 {
			t.Errorf("%s %s：没有按钮 %s 应 403，得到 %d", c.method, c.path, c.button, code)
		}
		chk.Grant(2, c.button)
		if code, _ := bob.JSON(c.method, c.path, map[string]any{}, nil); code == 403 {
			t.Errorf("%s %s：授予 %s 后不应再是 403", c.method, c.path, c.button)
		}
		chk.Revoke(2, c.button)
	}

	// 取消巡检：超级用户 / inspect.cancel
	if code, _ := bob.JSON("POST", "/api/v1/inspection/reports/cancel", map[string]any{"ids": []string{}}, nil); code != 403 {
		t.Errorf("没有 inspect.cancel：%d", code)
	}
	chk.Grant(2, "inspect.cancel")
	if code, _ := bob.JSON("POST", "/api/v1/inspection/reports/cancel", map[string]any{"ids": []string{}}, nil); code == 403 {
		t.Error("授予 inspect.cancel 后应能取消")
	}
	// options 里的 can_run / can_cancel 随按钮变化
	var opt struct {
		CanRun    bool `json:"can_run"`
		CanCancel bool `json:"can_cancel"`
		CanPlan   bool `json:"can_plan"`
		CanDelete bool `json:"can_delete"`
	}
	bob.GetJSON("/api/v1/inspection/options", &opt)
	if opt.CanRun || !opt.CanCancel {
		t.Errorf("options = %+v", opt)
	}
	chk.Grant(2, "inspect.run")
	bob.GetJSON("/api/v1/inspection/options", &opt)
	if !opt.CanRun {
		t.Errorf("授予 inspect.run 后 can_run: %+v", opt)
	}
	// 详情页的“删除报告”按钮靠 can_delete 显示
	bob.GetJSON("/api/v1/inspection/options", &opt)
	if opt.CanDelete {
		t.Errorf("没有 inspect.delete 时 can_delete 应为假: %+v", opt)
	}
	chk.Grant(2, "inspect.delete")
	bob.GetJSON("/api/v1/inspection/options", &opt)
	if !opt.CanDelete {
		t.Errorf("授予 inspect.delete 后 can_delete: %+v", opt)
	}
	// 超级用户不受影响
	root := env.Login("root")
	if code, _ := root.JSON("POST", "/api/v1/inspection/reports/delete", map[string]any{"ids": []string{}}, nil); code == 403 {
		t.Error("超级用户恒通过")
	}
}

// 定时计划的三个按钮在 options 里各有一个标记（页面上的“新建 / 编辑”“启用开关 / 删除”“立即运行”分别显示）。
func TestPlanButtonFlagsAreSeparate(t *testing.T) {
	e := newEnv()
	chk := btnpermtest.New()
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){chk.Hook, registerWith(e.svc, testHooks())}})
	chk.Grant(2, "inspect.view")
	bob := env.Login("bob")
	var o struct {
		CanPlan       bool `json:"can_plan"`
		CanPlanToggle bool `json:"can_plan_toggle"`
		CanPlanRun    bool `json:"can_plan_run"`
	}
	for _, c := range []struct {
		button string
		want   [3]bool
	}{{"inspect.plan_edit", [3]bool{true, false, false}}, {"inspect.plan_toggle", [3]bool{true, true, false}}, {"inspect.plan_run", [3]bool{true, true, true}}} {
		chk.Grant(2, c.button)
		bob.GetJSON("/api/v1/inspection/options", &o)
		if got := [3]bool{o.CanPlan, o.CanPlanToggle, o.CanPlanRun}; got != c.want {
			t.Errorf("授予 %s 后 can_plan / toggle / run = %v，期望 %v", c.button, got, c.want)
		}
	}
}
