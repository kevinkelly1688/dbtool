// @author: kevinkelly
// @license: Apache Licence
// @file: http_ext_test.go
// @time: 2026/10/28

package sysadmin

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHTTPExtPagesRender(t *testing.T) {
	env, _ := newEnv(t)
	root := env.Login("root")
	for path, want := range map[string][]string{
		"/settings/audit-flow":      {`id="af-glist"`, `id="af-kpis"`, `data-tab="chart"`, `id="af-timeline"`, `class="diagram"`},
		"/settings/resource-groups": {`id="rg-kpis"`, `id="re-leader"`, `id="re-remark"`, `id="m-res-busy"`, `id="rel-save"`},
		"/settings/users":           {`id="us-kpis"`, `id="ue-dept"`, `id="ue-authtype"`, `id="up-mode-gen"`, `id="us-drawer"`, `id="m-user-ovr"`, `id="us-sort"`},
		"/settings/auth-groups":     {`id="ag-desc"`, `id="bu-flt"`, `id="bu-sum"`, `id="bp-bulk-all"`, `id="bp-bulk-view"`, `id="bp-bulk-none"`},
	} {
		code, body := root.Page(path)
		if code != 200 {
			t.Fatalf("%s = %d", path, code)
		}
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%s 缺少 %s", path, w)
			}
		}
		if strings.Contains(body, "onclick=") || strings.Contains(body, "<script>") {
			t.Errorf("%s 含内联脚本", path)
		}
	}
}

func TestHTTPExtAPIs(t *testing.T) {
	env, f := newEnv(t)
	root := env.Login("root")
	bob := env.Login("bob")
	f.grant(10, "sqlworkflow.approve")

	// 普通用户不能访问新接口
	for _, w := range []struct{ m, p string }{{"GET", "/api/v1/settings/resource-groups/summary"}, {"GET", "/api/v1/settings/users/summary"},
		{"DELETE", "/api/v1/settings/resource-groups/1/purge"}, {"POST", "/api/v1/settings/resource-groups/1/relations/preview"}} {
		if code, _, _ := call(t, bob, w.m, w.p, map[string]any{}); code != 403 {
			t.Errorf("普通用户 %s %s = %d，期望 403", w.m, w.p, code)
		}
	}

	code, r, body := call(t, root, "GET", "/api/v1/settings/resource-groups/summary", nil)
	var sum ResGroupSummary
	if code != 200 || json.Unmarshal(r.Data, &sum) != nil || sum.Enabled != 2 {
		t.Fatalf("%d %s", code, body)
	}
	code, r, body = call(t, root, "GET", "/api/v1/settings/users/summary", nil)
	var us UserSummary
	if code != 200 || json.Unmarshal(r.Data, &us) != nil || us.Total != 4 || us.Super != 1 {
		t.Fatalf("%d %s", code, body)
	}

	// 负责人 / 备注走接口
	code, _, body = call(t, root, "PUT", "/api/v1/settings/resource-groups/1", map[string]any{"name": "研发组", "leader": "alice", "remark": "研发"})
	if code != 200 || f.xs().rg[1].Leader != "alice" {
		t.Fatalf("%d %s", code, body)
	}
	code, r, _ = call(t, root, "GET", "/api/v1/settings/resource-groups?search=研发", nil)
	var list []ResGroupItem
	if code != 200 || json.Unmarshal(r.Data, &list) != nil || len(list) != 1 || list[0].Leader != "alice" || list[0].LeaderDisplay != "alice名" {
		t.Fatalf("%d %+v", code, list)
	}

	// 彻底删除：未停用 → 409；组名错 → 400
	if code, r, _ = call(t, root, "DELETE", "/api/v1/settings/resource-groups/2/purge", map[string]any{"confirm_name": "数据组"}); code != 409 {
		t.Fatalf("未停用应 409: %d %s", code, r.Message)
	}
	call(t, root, "DELETE", "/api/v1/settings/resource-groups/2", nil)
	if code, _, _ = call(t, root, "DELETE", "/api/v1/settings/resource-groups/2/purge", map[string]any{"confirm_name": "nope"}); code != 400 {
		t.Fatalf("组名不对应 400: %d", code)
	}
	if code, _, body = call(t, root, "DELETE", "/api/v1/settings/resource-groups/2/purge", map[string]any{"confirm_name": "数据组"}); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if _, ok := f.rgs[2]; ok {
		t.Fatal("资源组应被删除")
	}

	// 停用被拒时返回工单明细
	f.openWF[1] = 1
	f.xs().open[1] = []OpenWorkflow{{Kind: "sql", Label: "SQL 上线", ID: 9, Name: "改表", Status: "待审批", Href: "/sqlworkflow/9"}}
	code, _, body = call(t, root, "DELETE", "/api/v1/settings/resource-groups/1", nil)
	if code != 409 || !strings.Contains(body, `"open_workflows"`) || !strings.Contains(body, "/sqlworkflow/9") {
		t.Fatalf("%d %s", code, body)
	}

	// 关联预览
	code, r, body = call(t, root, "POST", "/api/v1/settings/resource-groups/1/relations/preview", map[string]any{"kind": "users", "remove": []uint{2}})
	if code != 200 || !strings.Contains(string(r.Data), `"new_breaks"`) {
		t.Fatalf("%d %s", code, body)
	}

	// 用户：排序白名单、超级用户筛选
	if code, _, _ = call(t, root, "GET", "/api/v1/settings/users?sort=bogus", nil); code != 400 {
		t.Fatalf("非法排序应 400: %d", code)
	}
	if code, r, _ = call(t, root, "GET", "/api/v1/settings/users?status=superuser&sort=last_login", nil); code != 200 || !strings.Contains(string(r.Data), `"root"`) {
		t.Fatalf("%d", code)
	}

	// 重置密码：系统生成的临时密码只在这一次响应里返回
	w := root.Do("POST", "/api/v1/settings/users/3/reset-password", map[string]any{"generate": true})
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	var pr struct {
		Data struct {
			Password    string `json:"password"`
			ForceChange bool   `json:"force_change"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &pr)
	if pr.Data.Password == "" || !pr.Data.ForceChange || !f.xs().profiles[3].MustChangePassword {
		t.Fatalf("%s", w.Body.String())
	}
	for _, a := range f.audits {
		if strings.Contains(a.ExtraInfo, pr.Data.Password) || strings.Contains(a.Action, pr.Data.Password) {
			t.Fatal("审计里不能有临时密码")
		}
	}
	// 手输密码的响应里没有 password 字段
	w = root.Do("POST", "/api/v1/settings/users/3/reset-password", map[string]any{"password": "Manual-Pass-123"})
	if w.Code != 200 || strings.Contains(w.Body.String(), "Manual-Pass-123") || strings.Contains(w.Body.String(), `"password"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// LDAP 用户拒绝重置
	f.xs().kinds[2] = []string{"ldap"}
	if code, _, _ = call(t, root, "POST", "/api/v1/settings/users/2/reset-password", map[string]any{"generate": true}); code != 409 {
		t.Fatalf("LDAP 用户应 409: %d", code)
	}
}
