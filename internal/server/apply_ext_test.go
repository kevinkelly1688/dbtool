// @author: kevinkelly
// @license: Apache Licence
// @file: apply_ext_test.go
// @time: 2026/10/28

package server_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"dbtool/internal/model"
)

func openExt(e *applyEnv) {
	e.repo.ExtOn = true
	for id, u := range e.repo.Users {
		u.IsActive = true
		e.repo.Users[id] = u
	}
	d := e.repo.Users[1]
	d.Display = "开发者"
	e.repo.Users[1] = d
}

func pageGet(e *applyEnv, s *session, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(s.cookie)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestApplyReasonAndTabsHTTP(t *testing.T) {
	e := newApplyEnv(t)
	openExt(e)
	dev, leader := e.login(t, "dev"), e.login(t, "leader")

	if w := dev.do("GET", "/api/v1/query/apply/options", nil); !strings.Contains(w.Body.String(), `"reason_required":true`) {
		t.Errorf("选项应声明需要理由: %s", w.Body.String())
	}
	// 缺理由 / 理由太短 → 400；填了 → 成功
	if w := dev.do("POST", "/api/v1/query/privilege-applies", createBody()); w.Code != 400 || !strings.Contains(w.Body.String(), "申请理由") {
		t.Errorf("缺理由: %d %s", w.Code, w.Body.String())
	}
	body := createBody()
	body["reason"] = "客诉排查订单金额"
	id := applyID(t, dev.do("POST", "/api/v1/query/privilege-applies", body))
	detail := "/api/v1/query/privilege-applies/" + itoa(id)
	w := leader.do("GET", detail, nil)
	for _, want := range []string{`"reason":"客诉排查订单金额"`, `"timeline":[`, `"state":"current"`, `"granted":[]`, `"eff_status":0`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("详情缺 %s: %s", want, w.Body.String())
		}
	}
	// 页面：详情显示理由与时间线；列表按页签
	if r := pageGet(e, leader, "/query/applies/"+itoa(id)); !strings.Contains(r.Body.String(), "客诉排查订单金额") || !strings.Contains(r.Body.String(), "组长审批") {
		t.Errorf("详情页应显示理由与时间线: %d", r.Code)
	}
	r := pageGet(e, leader, "/query/applies")
	if r.Code != 200 || !strings.Contains(r.Body.String(), "待我审批") || !strings.Contains(r.Body.String(), "审批进度") || !strings.Contains(r.Body.String(), "组长") {
		t.Errorf("列表页: %d", r.Code)
	}
	// 申请人自己：可撤回按钮；没有待我审批页签
	r = pageGet(e, dev, "/query/applies")
	if !strings.Contains(r.Body.String(), `data-abort-id="`+itoa(id)+`"`) || strings.Contains(r.Body.String(), "待我审批") {
		t.Errorf("申请人列表页: %s", r.Body.String())
	}
	// JSON：带 tab 参数返回页签数量
	w = leader.do("GET", "/api/v1/query/privilege-applies?tab=todo", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"todo":1`) || !strings.Contains(w.Body.String(), `"tab":"todo"`) || !strings.Contains(w.Body.String(), `"progress":{"current":"组长"`) {
		t.Errorf("tab=todo: %d %s", w.Code, w.Body.String())
	}
	// 不带 tab：旧行为
	w = dev.do("GET", "/api/v1/query/privilege-applies", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"counts"`) {
		t.Errorf("旧行为: %s", w.Body.String())
	}
	// 重新申请：被驳回后详情给出预填链接
	if w := leader.do("POST", detail+"/audit", map[string]string{"action": "reject", "remark": "范围太大"}); w.Code != 200 {
		t.Fatalf("驳回: %d", w.Code)
	}
	if w := dev.do("GET", detail, nil); !strings.Contains(w.Body.String(), `"reapply":"db=shop`) && !strings.Contains(w.Body.String(), "reapply") {
		t.Errorf("重新申请: %s", w.Body.String())
	}
	r = pageGet(e, dev, "/query/applies/"+itoa(id))
	if !strings.Contains(r.Body.String(), "重新申请") || !strings.Contains(r.Body.String(), "/query/applies?new=1") {
		t.Errorf("驳回后详情页应有重新申请入口: %s", r.Body.String())
	}
}

func TestDirectGrantHTTP(t *testing.T) {
	e := newApplyEnv(t)
	openExt(e)
	dev, leader := e.login(t, "dev"), e.login(t, "leader")
	grant := map[string]any{"username": "dev", "instance_id": 1, "priv_type": 2, "db_name": "shop", "table_list": []string{"orders"},
		"valid_date": tomorrowStr(), "limit_num": 50, "reason": "临时排查故障"}

	if w := dev.do("POST", "/api/v1/query/privileges", grant); w.Code != 403 {
		t.Errorf("普通用户不能授予: %d %s", w.Code, w.Body.String())
	}
	if w := dev.do("GET", "/api/v1/query/privileges/grant-options", nil); w.Code != 403 {
		t.Errorf("普通用户不能取授予选项: %d", w.Code)
	}
	w := leader.do("GET", "/api/v1/query/privileges/grant-options", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"username":"dev"`) || !strings.Contains(w.Body.String(), "prod-mysql-01") {
		t.Errorf("授予选项: %d %s", w.Code, w.Body.String())
	}
	bad := map[string]any{"username": "dev", "instance_id": 1, "priv_type": 2, "db_name": "shop", "table_list": []string{"orders"}, "valid_date": tomorrowStr(), "limit_num": 50}
	if w := leader.do("POST", "/api/v1/query/privileges", bad); w.Code != 400 || !strings.Contains(w.Body.String(), "授予原因") {
		t.Errorf("缺原因: %d %s", w.Code, w.Body.String())
	}
	if w := leader.do("POST", "/api/v1/query/privileges", grant); w.Code != 200 || !strings.Contains(w.Body.String(), `"privilege_ids":[1]`) {
		t.Fatalf("授予: %d %s", w.Code, w.Body.String())
	}
	if len(e.repo.Privs) != 1 || e.repo.Privs[0].UserName != "dev" || len(e.repo.AuditEntries) != 1 {
		t.Errorf("应写权限与审计: %+v %d", e.repo.Privs, len(e.repo.AuditEntries))
	}
	// 列表：来源、状态、KPI；页面有“直接授予”按钮（管理者）而申请人没有
	w = leader.do("GET", "/api/v1/query/privileges", nil)
	for _, want := range []string{`"source":"grant"`, `"granted_by":"leader"`, `"state":"soon"`, `"stats":{`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("权限列表缺 %s: %s", want, w.Body.String())
		}
	}
	r := pageGet(e, leader, "/query/privileges")
	for _, want := range []string{"直接授予", "id=\"grant-open\"", "即将到期", "管理员直接授予"} {
		if !strings.Contains(r.Body.String(), want) {
			t.Errorf("管理页缺 %s", want)
		}
	}
	if r := pageGet(e, dev, "/query/privileges"); strings.Contains(r.Body.String(), "grant-open") {
		t.Error("申请人页面不应有直接授予按钮")
	}
	// 过期的权限也能查到
	e.repo.Privs[0].ValidDate = e.repo.Privs[0].ValidDate.AddDate(0, 0, -10)
	if w := leader.do("GET", "/api/v1/query/privileges", nil); strings.Contains(w.Body.String(), `"privilege_id":1`) {
		t.Error("默认不显示已过期")
	}
	if w := leader.do("GET", "/api/v1/query/privileges?state=expired&user=dev&inst=1&db=shop", nil); !strings.Contains(w.Body.String(), `"privilege_id":1`) || !strings.Contains(w.Body.String(), `"state":"expired"`) {
		t.Errorf("state=expired: %s", w.Body.String())
	}
	_ = model.User{}
}

func TestApplyPagesDegradeWithoutExtTables(t *testing.T) {
	e := newApplyEnv(t) // ExtOn=false：旁路表没创建
	dev, leader := e.login(t, "dev"), e.login(t, "leader")
	id := applyID(t, dev.do("POST", "/api/v1/query/privilege-applies", createBody()))
	if w := dev.do("GET", "/api/v1/query/apply/options", nil); !strings.Contains(w.Body.String(), `"reason_required":false`) {
		t.Errorf("旁路表缺失时不要求理由: %s", w.Body.String())
	}
	for _, p := range []string{"/query/applies", "/query/applies/" + itoa(id), "/query/privileges"} {
		if r := pageGet(e, leader, p); r.Code != 200 {
			t.Errorf("%s 应照常打开: %d", p, r.Code)
		}
	}
}
