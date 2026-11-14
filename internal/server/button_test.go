// @author: kevinkelly
// @license: Apache Licence
// @file: button_test.go
// @time: 2026/10/28

package server_test

import (
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"dbtool/internal/btnperm"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// fakeButtons 是内存的按钮权限检查器：按用户 ID 列出拥有的按钮。
type fakeButtons struct {
	allow map[uint]map[string]bool
	set   btnperm.Setting
}

func (f *fakeButtons) HasButton(uid uint, code string) (bool, error) { return f.allow[uid][code], nil }
func (f *fakeButtons) AllowedButtons(uid uint) (map[string]bool, error) {
	return f.allow[uid], nil
}
func (f *fakeButtons) Setting() (btnperm.Setting, error) { return f.set.Normalize(), nil }

func grant(f *fakeButtons, uid uint, codes ...string) {
	if f.allow[uid] == nil {
		f.allow[uid] = map[string]bool{}
	}
	for _, c := range codes {
		f.allow[uid][c] = true
	}
}

// SQL 上线：比原权限码更细的按钮在接口里单独校验（需要装配按钮权限检查器）。
func TestSQLWorkflowButtonChecks(t *testing.T) {
	fb := &fakeButtons{allow: map[uint]map[string]bool{}}
	e := newSQLEnvWith(t, func(o *server.Options) { o.Buttons = fb })
	dev, leader := e.login(t, "dev"), e.login(t, "leader")
	// dev（原权限 sql_submit / sql_execute）：只有提交、自己执行、终止自己的；leader（sql_review）：只有审批
	grant(fb, 1, "sqlworkflow.submit", "sqlworkflow.execute_own", "sqlworkflow.cancel_own", "sqlworkflow.view_own")
	grant(fb, 2, "sqlworkflow.approve")

	w := dev.do("POST", "/api/v1/sqlworkflow", submitBody("update t set a=1"))
	id := itoa(dataOf(t, w)["workflow_id"].(float64))

	// 审批：approve / alter_window / reject 是三个按钮
	if w := leader.do("POST", "/api/v1/sqlworkflow/"+id+"/run-date", map[string]any{"run_date_start": "", "run_date_end": ""}); w.Code != 403 {
		t.Errorf("没有 alter_window: %d", w.Code)
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/pass", map[string]any{"remark": "x"}); w.Code != 403 {
		t.Errorf("没有 approve: %d", w.Code)
	}
	if dd := dataOf(t, leader.do("GET", "/api/v1/sqlworkflow/"+id, nil)); dd["can_review"] != true {
		t.Errorf("leader 有 approve，详情应可审批: %v", dd["can_review"])
	}
	// 详情页是否出现“修改可执行时间”看 alter_window：leader 在当前节点上，但没有这个按钮，页面上不显示
	if body := leader.do("GET", "/sqlworkflow/"+id, nil).Body.String(); strings.Contains(body, `id="act-rundate"`) {
		t.Error("没有 alter_window，页面不应出现“修改可执行时间”")
	}
	grant(fb, 2, "sqlworkflow.alter_window")
	if body := leader.do("GET", "/sqlworkflow/"+id, nil).Body.String(); !strings.Contains(body, `id="act-rundate"`) {
		t.Error("有 alter_window 且在当前审批节点上，页面应出现“修改可执行时间”")
	}
	delete(fb.allow[2], "sqlworkflow.alter_window")
	// 去掉 approve 后详情里的 can_review 随之为 false
	delete(fb.allow[2], "sqlworkflow.approve")
	if dd := dataOf(t, leader.do("GET", "/api/v1/sqlworkflow/"+id, nil)); dd["can_review"] != false {
		t.Errorf("没有 approve 按钮，can_review 应为 false: %v", dd["can_review"])
	}
	grant(fb, 2, "sqlworkflow.approve")
	if w := leader.do("POST", "/api/v1/sqlworkflow/"+id+"/pass", map[string]any{"remark": "ok"}); w.Code != 200 {
		t.Fatalf("通过: %d %s", w.Code, w.Body.String())
	}

	// 执行：定时、手工确认结束是独立按钮；回滚语句也是
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/schedule", map[string]any{"run_at": "2099-01-01T10:00"}); w.Code != 403 {
		t.Errorf("没有 schedule: %d", w.Code)
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/execute", map[string]any{"mode": "manual"}); w.Code != 403 {
		t.Errorf("没有 manual_finish: %d", w.Code)
	}
	if w := dev.do("GET", "/api/v1/sqlworkflow/"+id+"/rollback", nil); w.Code != 403 {
		t.Errorf("没有 rollback: %d", w.Code)
	}
	if w := dev.do("GET", "/api/v1/sqlworkflow/"+id+"/rollback/download", nil); w.Code != 403 {
		t.Errorf("没有 rollback（下载）: %d", w.Code)
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/osc", map[string]any{"command": "pause"}); w.Code != 403 {
		t.Errorf("没有 osc: %d", w.Code)
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/batch/12/retry", nil); w.Code != 403 {
		t.Errorf("没有 batch_op: %d", w.Code)
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/batch/12/close", nil); w.Code != 403 {
		t.Errorf("没有 batch_op（关闭）: %d", w.Code)
	}
	// 授予 schedule 后：校验通过（定时设置成功）
	grant(fb, 1, "sqlworkflow.schedule")
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/schedule", map[string]any{"run_at": "2099-01-01T10:00"}); w.Code != 200 {
		t.Errorf("授予 schedule 后: %d %s", w.Code, w.Body.String())
	}
	// 系统执行：execute_group / execute_own 任一即可；都没有则 403
	delete(fb.allow[1], "sqlworkflow.execute_own")
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/execute", map[string]any{"mode": "auto"}); w.Code != 403 {
		t.Errorf("没有执行按钮: %d", w.Code)
	}
	if dd := dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/"+id, nil)); dd["can_execute"] != false {
		t.Errorf("没有执行按钮，can_execute 应为 false: %v", dd["can_execute"])
	}
	grant(fb, 1, "sqlworkflow.execute_own")
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/execute", map[string]any{"mode": "auto"}); w.Code != 200 {
		t.Errorf("授予 execute_own 后: %d %s", w.Code, w.Body.String())
	}
}

// 终止工单：cancel_own 只能终止自己的；驳回他人需要 reject。
func TestSQLWorkflowCancelButtons(t *testing.T) {
	fb := &fakeButtons{allow: map[uint]map[string]bool{}}
	e := newSQLEnvWith(t, func(o *server.Options) { o.Buttons = fb })
	dev, leader := e.login(t, "dev"), e.login(t, "leader")
	grant(fb, 1, "sqlworkflow.submit", "sqlworkflow.cancel_own")
	grant(fb, 2, "sqlworkflow.cancel_own") // 只有“终止自己的”

	w := dev.do("POST", "/api/v1/sqlworkflow", submitBody("update t set a=1"))
	id := itoa(dataOf(t, w)["workflow_id"].(float64))
	if w := leader.do("POST", "/api/v1/sqlworkflow/"+id+"/cancel", map[string]any{"remark": "不同意"}); w.Code != 403 {
		t.Errorf("只有 cancel_own 不能驳回他人的工单: %d", w.Code)
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/cancel", map[string]any{"remark": "撤回"}); w.Code != 200 {
		t.Errorf("终止自己的: %d %s", w.Code, w.Body.String())
	}
	// 没有 cancel_own 也没有 reject：403
	w = dev.do("POST", "/api/v1/sqlworkflow", submitBody("update t set a=2"))
	id2 := itoa(dataOf(t, w)["workflow_id"].(float64))
	delete(fb.allow[1], "sqlworkflow.cancel_own")
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id2+"/cancel", map[string]any{"remark": "撤回"}); w.Code != 403 {
		t.Errorf("没有任何终止按钮: %d", w.Code)
	}
	// 有 reject：驳回他人的工单
	grant(fb, 2, "sqlworkflow.reject")
	if w := leader.do("POST", "/api/v1/sqlworkflow/"+id2+"/cancel", map[string]any{"remark": "驳回"}); w.Code != 200 {
		t.Errorf("reject: %d %s", w.Code, w.Body.String())
	}
}

// 没有装配按钮权限检查器（测试 / 演示）：HasButton 直接问仓库的 HasButton；中间件与 ButtonOrFail 照常按按钮校验。
func TestButtonWithoutChecker(t *testing.T) {
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){func(s *server.Server, rt server.Routes) {
		rt.API.GET("/t/has", func(c *gin.Context) {
			out := map[string]bool{}
			for _, code := range strings.Split(c.Query("codes"), ",") {
				out[code] = s.HasButton(c, code)
			}
			server.APIOK(c, out)
		})
		rt.API.GET("/t/any", s.RequireButtonAPI("qpriv.manage", "export.download"), func(c *gin.Context) { server.APIOK(c, "ok") })
		rt.API.GET("/t/audited", s.RequireButtonAPIAudited("export.download"), func(c *gin.Context) { server.APIOK(c, "ok") })
		rt.API.GET("/t/orfail", func(c *gin.Context) {
			if s.ButtonOrFail(c, "partman.run") {
				server.APIOK(c, "ok")
			}
		})
		rt.Pages.GET("/t/page", s.RequireButtonPage("archive.view"), func(c *gin.Context) { c.String(200, "页面") })
	}}})
	root, bob := env.Login("root"), env.Login("bob")
	has := func(c *servertest.Client, codes ...string) map[string]bool {
		var out map[string]bool
		c.GetJSON("/api/v1/t/has?codes="+strings.Join(codes, ","), &out)
		return out
	}
	all := []string{"qpriv.manage", "sqlworkflow.cancel_own", "inspect.run", "user.edit", "no.such", "sqlworkflow.schedule"}
	r := has(root, all...)
	if !r["qpriv.manage"] || !r["sqlworkflow.cancel_own"] || !r["inspect.run"] || !r["sqlworkflow.schedule"] || r["no.such"] {
		t.Errorf("超级用户: 已登记的按钮恒真、未登记的为假: %v", r)
	}
	if r["user.edit"] != true {
		t.Errorf("超级用户可用 Locked 按钮: %v", r)
	}
	b := has(bob, all...)
	if b["qpriv.manage"] || !b["sqlworkflow.cancel_own"] || b["inspect.run"] || b["user.edit"] || b["no.such"] || b["sqlworkflow.schedule"] {
		t.Errorf("普通用户无权限时（基础按钮除外）: %v", b)
	}
	env.Repo.Perms[2] = map[string]bool{"qpriv.manage": true, "sqlworkflow.schedule": true, "export.download": true}
	b = has(bob, all...)
	if !b["qpriv.manage"] || !b["sqlworkflow.schedule"] || b["inspect.run"] {
		t.Errorf("按仓库里的授权: %v", b)
	}

	// 中间件
	env.Repo.Perms[2] = map[string]bool{}
	if code, _ := bob.JSON("GET", "/api/v1/t/any", nil, nil); code != 403 {
		t.Errorf("任一按钮: 无权限应 403，得到 %d", code)
	}
	env.Repo.Perms[2] = map[string]bool{"export.download": true}
	if code, _ := bob.JSON("GET", "/api/v1/t/any", nil, nil); code != 200 {
		t.Errorf("任一按钮: 有 export.download 应 200，得到 %d", code)
	}
	if code, _ := bob.JSON("GET", "/api/v1/t/audited", nil, nil); code != 200 {
		t.Errorf("审计变体: %d", code)
	}
	if code, _ := bob.JSON("GET", "/api/v1/t/orfail", nil, nil); code != 403 {
		t.Errorf("ButtonOrFail 没有 partman.run 应 403: %d", code)
	}
	env.Repo.Perms[2]["partman.run"] = true
	if code, _ := bob.JSON("GET", "/api/v1/t/orfail", nil, nil); code != 200 {
		t.Errorf("ButtonOrFail 授予后应 200: %d", code)
	}
	if code, _ := root.JSON("GET", "/api/v1/t/any", nil, nil); code != 200 {
		t.Errorf("超级用户: %d", code)
	}
	if code, _ := env.Login("bob").Page("/t/page"); code != 403 {
		t.Errorf("页面: 没有 archive.view 应 403，得到 %d", code)
	}
	env.Repo.Perms[2]["archive.view"] = true
	if code, body := env.Login("bob").Page("/t/page"); code != 200 || body != "页面" {
		t.Errorf("页面: %d %s", code, body)
	}
	if w := env.Anonymous("GET", "/api/v1/t/any"); w.Code != 401 {
		t.Errorf("未登录: %d", w.Code)
	}
}

// 装配了检查器：HasButton / 中间件按检查器判断，ButtonOrFail 生效；可授予与 Locked 的区别。
func TestButtonWithChecker(t *testing.T) {
	fb := &fakeButtons{allow: map[uint]map[string]bool{}}
	grant(fb, 2, "export.download", "user.edit") // user.edit 是 Locked，检查器误放行也不能用
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){func(s *server.Server, rt server.Routes) {
		s.SetButtonChecker(fb)
		rt.API.GET("/t/has", func(c *gin.Context) {
			out := map[string]bool{}
			for _, code := range strings.Split(c.Query("codes"), ",") {
				out[code] = s.HasButton(c, code)
			}
			server.APIOK(c, out)
		})
		rt.API.GET("/t/orfail", func(c *gin.Context) {
			if s.ButtonOrFail(c, "partman.run") {
				server.APIOK(c, "ok")
			}
		})
	}}})
	bob := env.Login("bob")
	var out map[string]bool
	bob.GetJSON("/api/v1/t/has?codes=export.download,user.edit,archive.view", &out)
	if !out["export.download"] || out["user.edit"] || out["archive.view"] {
		t.Errorf("按检查器判断: %v", out)
	}
	if code, _ := bob.JSON("GET", "/api/v1/t/orfail", nil, nil); code != 403 {
		t.Errorf("ButtonOrFail 有检查器时应校验: %d", code)
	}
	grant(fb, 2, "partman.run")
	if code, _ := bob.JSON("GET", "/api/v1/t/orfail", nil, nil); code != 200 {
		t.Errorf("授予后: %d", code)
	}
}
