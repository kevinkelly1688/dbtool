// @author: kevinkelly
// @license: Apache Licence
// @file: http_test.go
// @time: 2026/10/28

package sysadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// 用户 ID 与 servertest 的默认仓库对齐：1 root（超级用户）、2 普通用户（登录名 bob）。
func newEnv(t *testing.T) (*servertest.Env, *fakeRepo) {
	t.Helper()
	svc, f := setup()
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){registerWith(svc)}})
	return env, f
}

type apiResp struct {
	Data    json.RawMessage `json:"data"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
}

func call(t *testing.T, c *servertest.Client, method, path string, body any) (int, apiResp, string) {
	t.Helper()
	w := c.Do(method, path, body)
	var r apiResp
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	return w.Code, r, w.Body.String()
}

func TestHTTPAuthAndPermissions(t *testing.T) {
	env, _ := newEnv(t)
	for _, p := range []string{"/api/v1/settings/users", "/api/v1/settings/resource-groups", "/api/v1/settings/auth-groups", "/api/v1/settings/audit-flow", "/api/v1/audit/log"} {
		if w := env.Anonymous("GET", p); w.Code != 401 {
			t.Errorf("未登录 GET %s = %d，期望 401", p, w.Code)
		}
	}
	for _, p := range []string{"/settings/users", "/settings/resource-groups", "/settings/auth-groups", "/settings/audit-flow", "/audit"} {
		if w := env.Anonymous("GET", p); w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
			t.Errorf("未登录页面 %s = %d %s，期望跳转登录", p, w.Code, w.Header().Get("Location"))
		}
	}

	bob := env.Login("bob")
	for _, p := range []string{"/api/v1/settings/users", "/api/v1/settings/resource-groups", "/api/v1/settings/auth-groups", "/api/v1/settings/audit-flow", "/api/v1/audit/log", "/api/v1/audit/options"} {
		if code, r, _ := call(t, bob, "GET", p, nil); code != 403 || r.Code != "forbidden" {
			t.Errorf("普通用户 GET %s = %d %q，期望 403", p, code, r.Code)
		}
	}
	for _, p := range []string{"/settings/users", "/settings/resource-groups", "/settings/auth-groups", "/settings/audit-flow", "/audit"} {
		if code, _ := bob.Page(p); code != 403 {
			t.Errorf("普通用户页面 %s = %d，期望 403", p, code)
		}
	}
	for _, w := range []struct{ m, p string }{{"POST", "/api/v1/settings/users"}, {"POST", "/api/v1/settings/users/3/reset-password"}, {"PUT", "/api/v1/settings/audit-flow"}, {"DELETE", "/api/v1/settings/resource-groups/1"}} {
		if code, _, _ := call(t, bob, w.m, w.p, map[string]any{}); code != 403 {
			t.Errorf("普通用户 %s %s = %d，期望 403", w.m, w.p, code)
		}
	}

	// 拥有 audit.view 的普通用户：能看系统审计，仍不能管理用户
	env.Repo.Perms[2] = map[string]bool{"audit.view": true}
	if code, body := bob.Page("/audit"); code != 200 || !strings.Contains(body, "审计日志") {
		t.Fatalf("audit.view 应能打开 /audit: %d", code)
	} else if !strings.Contains(body, `href="/audit"`) || strings.Contains(body, `href="/settings/users"`) {
		t.Fatal("菜单：audit.view 用户应只看到系统审计")
	}
	if code, _, _ := call(t, bob, "GET", "/api/v1/audit/log", nil); code != 200 {
		t.Fatalf("audit.view 读审计日志 = %d", code)
	}
	if code, _, _ := call(t, bob, "GET", "/api/v1/settings/users", nil); code != 403 {
		t.Fatalf("audit.view 不应能读用户管理: %d", code)
	}
}

func TestHTTPCSRF(t *testing.T) {
	env, _ := newEnv(t)
	// 手工登录，拿到会话 Cookie 但故意不带 CSRF 头
	w := httptest.NewRecorder()
	env.H.ServeHTTP(w, httptest.NewRequest("GET", "/login", nil))
	var lc *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "dba_login_csrf" {
			lc = c
		}
	}
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	form := url.Values{"username": {"root"}, "password": {servertest.Password}, "csrf": {m[1]}, "next": {"/query"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(lc)
	w = httptest.NewRecorder()
	env.H.ServeHTTP(w, req)
	var sc *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "dba_session" {
			sc = c
		}
	}
	if sc == nil {
		t.Fatal("登录失败")
	}
	req = httptest.NewRequest("POST", "/api/v1/settings/resource-groups", strings.NewReader(`{"name":"X"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sc)
	w = httptest.NewRecorder()
	env.H.ServeHTTP(w, req)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "csrf") {
		t.Fatalf("缺少 CSRF 头应 403: %d %s", w.Code, w.Body.String())
	}
}

func TestHTTPPagesRender(t *testing.T) {
	env, _ := newEnv(t)
	root := env.Login("root")
	want := map[string][]string{
		"/settings/audit-flow":      {"审批流程", "总览矩阵", "audit_flow.js"},
		"/settings/resource-groups": {"资源组", "新建资源组", "resource_groups.js"},
		"/settings/users":           {"用户", "新建用户", "users.js"},
		"/settings/auth-groups":     {"权限组", "无权限按钮的显示方式", "bp-tree", "m-user-perm", "auth_groups.js"},
		"/audit":                    {"审计日志", "SQL 上线审计", "audit.js"},
	}
	for p, keys := range want {
		code, body := root.Page(p)
		if code != 200 {
			t.Errorf("%s = %d", p, code)
			continue
		}
		for _, k := range keys {
			if !strings.Contains(body, k) {
				t.Errorf("%s 缺少 %q", p, k)
			}
		}
		if !strings.Contains(body, "sysadmin.css") || !strings.Contains(body, `name="csrf-token"`) {
			t.Errorf("%s 缺少样式或 CSRF meta", p)
		}
		// CSP 很严：模板里不能有内联脚本或内联事件
		inline := false
		for _, tag := range regexp.MustCompile(`(?i)<script[^>]*>`).FindAllString(body, -1) {
			inline = inline || !strings.Contains(tag, "src=")
		}
		if inline || regexp.MustCompile(`(?i)\son(click|submit|change|load|input)=`).MatchString(body) {
			t.Errorf("%s 含内联脚本/事件", p)
		}
	}
	// 菜单：访问控制、安全合规两个分组按顺序出现
	_, body := root.Page("/settings/users")
	order := []string{"用户", "权限组", "资源组", "审批流程", "审计日志"}
	last := -1
	for _, name := range order {
		i := strings.Index(body, ">"+name+"</a>")
		if i < 0 {
			t.Errorf("菜单缺少 %s", name)
			continue
		}
		if i < last {
			t.Errorf("菜单顺序不对: %s", name)
		}
		last = i
	}
}

func TestHTTPEndToEnd(t *testing.T) {
	env, f := newEnv(t)
	root := env.Login("root")
	const hook = "https://oapi.dingtalk.com/robot/send?access_token=SECRET-TOKEN-XYZ"
	const pw = "T7#kd9-Qz-secret"

	// 资源组：创建后任何响应都不含 webhook 明文
	code, r, body := call(t, root, "POST", "/api/v1/settings/resource-groups", map[string]any{"name": "财务组", "ding_webhook": hook})
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var created struct{ ID uint }
	_ = json.Unmarshal(r.Data, &created)
	for _, p := range []string{"/api/v1/settings/resource-groups", "/api/v1/settings/resource-groups?deleted=1", "/api/v1/settings/resource-groups/" + itoa(created.ID)} {
		code, _, body = call(t, root, "GET", p, nil)
		if code != 200 || strings.Contains(body, "SECRET-TOKEN") || strings.Contains(body, "oapi.dingtalk.com") {
			t.Fatalf("%s 泄露 webhook 或失败: %d %s", p, code, body)
		}
	}
	if !strings.Contains(body, `"ding_set":true`) || !strings.Contains(body, `"slack_set":false`) {
		t.Fatalf("应只返回是否已配置: %s", body)
	}
	if code, r, _ := call(t, root, "POST", "/api/v1/settings/resource-groups", map[string]any{"name": "财务组"}); code != 409 || r.Code != "conflict" {
		t.Fatalf("重名应 409: %d %+v", code, r)
	}
	if code, r, _ := call(t, root, "POST", "/api/v1/settings/resource-groups", map[string]any{"name": "Y", "slack_webhook": "javascript:alert(1)"}); code != 400 || r.Code != "bad_request" {
		t.Fatalf("非法 webhook 应 400: %d %+v", code, r)
	}
	if code, _, _ := call(t, root, "POST", "/api/v1/settings/resource-groups", "not json{"); code != 400 {
		t.Fatalf("坏 JSON 应 400: %d", code)
	}
	if code, _, _ := call(t, root, "GET", "/api/v1/settings/resource-groups/abc", nil); code != 400 {
		t.Fatalf("非法 ID 应 400: %d", code)
	}
	if code, _, _ := call(t, root, "GET", "/api/v1/settings/resource-groups/999", nil); code != 404 {
		t.Fatalf("不存在应 404: %d", code)
	}

	// 用户：密码与哈希永远不出现在响应里
	code, r, body = call(t, root, "POST", "/api/v1/settings/users", map[string]any{"username": "dave", "display": "戴夫", "email": "dave@example.com",
		"password": pw, "is_active": true, "auth_group_ids": []uint{30}, "resource_group_ids": []uint{1}})
	if code != 200 || strings.Contains(body, pw) {
		t.Fatalf("%d %s", code, body)
	}
	var du struct{ ID uint }
	_ = json.Unmarshal(r.Data, &du)
	stored := f.users[du.ID].Password
	for _, p := range []string{"/api/v1/settings/users", "/api/v1/settings/users?search=dave", "/api/v1/settings/users/" + itoa(du.ID), "/api/v1/settings/users/options"} {
		code, _, body = call(t, root, "GET", p, nil)
		if code != 200 || strings.Contains(body, pw) || strings.Contains(body, "pbkdf2") || strings.Contains(strings.ToLower(body), "password") {
			t.Fatalf("%s 泄露密码或失败: %d %s", p, code, body)
		}
	}
	// 编辑接口忽略 password 字段
	code, _, _ = call(t, root, "PUT", "/api/v1/settings/users/"+itoa(du.ID), map[string]any{"display": "戴夫2", "email": "dave@example.com", "password": "Hijack-123456", "is_active": true,
		"auth_group_ids": []uint{30}, "resource_group_ids": []uint{1}})
	if code != 200 || f.users[du.ID].Password != stored || f.users[du.ID].Display != "戴夫2" {
		t.Fatalf("编辑不应改密码: %d", code)
	}
	// 重置密码弱口令 400；不能停用自己 403
	if code, _, _ := call(t, root, "POST", "/api/v1/settings/users/"+itoa(du.ID)+"/reset-password", map[string]any{"password": "weak"}); code != 400 {
		t.Fatalf("弱口令应 400: %d", code)
	}
	if code, r, _ := call(t, root, "POST", "/api/v1/settings/users/1/disable", nil); code != 403 || r.Code != "forbidden" {
		t.Fatalf("不能停用自己: %d %+v", code, r)
	}
	if code, _, _ := call(t, root, "POST", "/api/v1/settings/users/"+itoa(du.ID)+"/unlock", nil); code != 409 {
		t.Fatalf("无需解锁应 409: %d", code)
	}

	// 审批流
	save := map[string]any{"group_id": 1, "workflow_type": 2, "auth_group_ids": []uint{10, 20}}
	code, r, body = call(t, root, "PUT", "/api/v1/settings/audit-flow", save)
	if code != 200 || !strings.Contains(body, `"after":"TL → DBA"`) {
		t.Fatalf("%d %s", code, body)
	}
	if f.settings[[2]int{1, 2}].AuditAuthGroups != "10,20" {
		t.Fatalf("存库格式不对: %+v", f.settings)
	}
	if code, r, _ = call(t, root, "PUT", "/api/v1/settings/audit-flow", map[string]any{"group_id": 1, "workflow_type": 2, "auth_group_ids": []uint{10, 10}}); code != 400 {
		t.Fatalf("重复应 400: %d", code)
	}
	if code, _, _ = call(t, root, "PUT", "/api/v1/settings/audit-flow", map[string]any{"group_id": 1, "workflow_type": 2, "auth_group_ids": []uint{}}); code != 400 {
		t.Fatalf("空应 400: %d", code)
	}
	code, _, body = call(t, root, "GET", "/api/v1/settings/audit-flow/detail?group_id=1&workflow_type=2", nil)
	if code != 200 || !strings.Contains(body, `"text":"TL → DBA"`) || !strings.Contains(body, "该级无人可审") {
		t.Fatalf("%d %s", code, body)
	}
	if code, _, _ = call(t, root, "GET", "/api/v1/settings/audit-flow/detail?group_id=x&workflow_type=2", nil); code != 400 {
		t.Fatalf("非法参数应 400: %d", code)
	}
	if code, _, _ = call(t, root, "POST", "/api/v1/settings/audit-flow/preview", save); code != 200 {
		t.Fatalf("preview %d", code)
	}
	if code, _, body = call(t, root, "GET", "/api/v1/settings/audit-flow", nil); code != 200 || !strings.Contains(body, `"matrix"`) {
		t.Fatalf("%d %s", code, body)
	}
	if code, _, _ = call(t, root, "DELETE", "/api/v1/settings/audit-flow?group_id=1&workflow_type=2", nil); code != 200 {
		t.Fatalf("delete flow %d", code)
	}

	// 权限组：只管名称 / 说明 / 成员，权限（按钮）在按钮权限页里授予；早期的原权限码接口已经去掉
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/settings/users/" + itoa(du.ID) + "/permissions"}, {"PUT", "/api/v1/settings/users/" + itoa(du.ID) + "/permissions"},
	} {
		if code, _, _ := call(t, root, c.method, c.path, map[string]any{"add": []string{"audit.view"}, "reason": "临时"}); code != 404 {
			t.Errorf("%s %s 应已不存在: %d", c.method, c.path, code)
		}
	}
	if code, _, _ = call(t, root, "GET", "/api/v1/settings/auth-groups/permissions", nil); code == 200 {
		t.Errorf("权限码目录接口应已不存在")
	}
	code, r, _ = call(t, root, "POST", "/api/v1/settings/auth-groups", map[string]any{"name": "QA", "description": "测试组"})
	if code != 200 {
		t.Fatalf("create auth group %d", code)
	}
	var ag struct{ ID uint }
	_ = json.Unmarshal(r.Data, &ag)
	if code, _, body = call(t, root, "GET", "/api/v1/settings/auth-groups/"+itoa(ag.ID), nil); code != 200 || !strings.Contains(body, `"description":"测试组"`) || strings.Contains(body, "perm_codes") {
		t.Fatalf("%d %s", code, body)
	}
	if code, _, _ = call(t, root, "DELETE", "/api/v1/settings/auth-groups/"+itoa(ag.ID), nil); code != 200 {
		t.Fatalf("delete auth group %d", code)
	}

	// 所有写操作都进了审计，且审计里没有密码 / webhook
	f.audits = append(f.audits, model.AuditEntry{ID: 900, UserName: "=cmd|calc", UserDisplay: "+x", Action: "@act", ExtraInfo: "-1+1", ActionTime: time.Now()})
	_, _, body = call(t, root, "GET", "/api/v1/audit/log?page_size=200", nil)
	for _, a := range []string{"新建资源组", "新建用户", "修改审批流", "新建权限组", "删除权限组", "删除审批流"} {
		if !strings.Contains(body, a) {
			t.Errorf("审计里缺少动作 %q", a)
		}
	}
	for _, s := range []string{pw, "SECRET-TOKEN", "pbkdf2", "Hijack"} {
		if strings.Contains(body, s) {
			t.Errorf("审计里不应出现 %q", s)
		}
	}
	// 筛选与日期校验
	if code, _, body = call(t, root, "GET", "/api/v1/audit/log?action="+url.QueryEscape("新建用户"), nil); code != 200 || !strings.Contains(body, `"total":1`) {
		t.Fatalf("%d %s", code, body)
	}
	if code, r, _ = call(t, root, "GET", "/api/v1/audit/log?start_date=bad", nil); code != 400 {
		t.Fatalf("非法日期应 400: %d", code)
	}
	if code, _, body = call(t, root, "GET", "/api/v1/audit/options", nil); code != 200 || !strings.Contains(body, "新建用户") || !strings.Contains(body, "workflow_finish") {
		t.Fatalf("%d %s", code, body)
	}
	if code, _, _ = call(t, root, "GET", "/api/v1/audit/sql-workflows?status=bogus", nil); code != 400 {
		t.Fatalf("非法状态应 400: %d", code)
	}
	if code, _, _ = call(t, root, "GET", "/api/v1/audit/sql-workflows", nil); code != 200 {
		t.Fatalf("sql-workflows %d", code)
	}
	if code, _, _ = call(t, root, "GET", "/api/v1/audit/query-logs", nil); code != 200 {
		t.Fatalf("query-logs %d", code)
	}

	// CSV 导出：UTF-8 BOM、公式注入防护，且导出本身被审计
	w := root.Do("GET", "/api/v1/audit/log/export", nil)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") || !strings.HasPrefix(w.Body.String(), "\xEF\xBB\xBF用户名,") {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Content-Type"))
	}
	csv := w.Body.String()
	for _, want := range []string{"'=cmd|calc", "'+x", "'@act", "'-1+1"} {
		if !strings.Contains(csv, want) {
			t.Errorf("CSV 应转义公式开头: 缺 %q\n%s", want, csv)
		}
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Error("应作为附件下载")
	}
	if last := f.audits[len(f.audits)-1]; last.Action != "导出审计日志" {
		t.Fatalf("导出应写审计: %+v", last)
	}
}

// 管理员被降级 / 停用后，旧会话里的“超级用户”标记不能再用来管理。
func TestHTTPStaleSuperuserSession(t *testing.T) {
	env, f := newEnv(t)
	root := env.Login("root")
	if code, _, _ := call(t, root, "GET", "/api/v1/settings/users", nil); code != 200 {
		t.Fatal(code)
	}
	u := f.users[1]
	u.IsSuperuser = false
	f.users[1] = u
	if code, r, _ := call(t, root, "GET", "/api/v1/settings/users", nil); code != 403 || r.Code != "forbidden" {
		t.Fatalf("被降级后应 403: %d %+v", code, r)
	}
	if code, _, _ := call(t, root, "POST", "/api/v1/settings/resource-groups", map[string]any{"name": "X"}); code != 403 {
		t.Fatalf("被降级后写操作应 403: %d", code)
	}
	if len(f.rgs) != 2 {
		t.Fatal("不应创建资源组")
	}
}

func TestHTTPRelationsAndLifecycle(t *testing.T) {
	env, f := newEnv(t)
	root := env.Login("root")
	if code, _, body := call(t, root, "GET", "/api/v1/settings/resource-groups/1/relations", nil); code != 200 || !strings.Contains(body, `"linked"`) || !strings.Contains(body, "prod-1") {
		t.Fatalf("%d %s", code, body)
	}
	if code, _, _ := call(t, root, "PUT", "/api/v1/settings/resource-groups/1/instances", map[string]any{"add": []uint{1}}); code != 200 || !f.rgInst[1][1] {
		t.Fatalf("关联实例失败 %d", code)
	}
	if code, _, _ := call(t, root, "PUT", "/api/v1/settings/resource-groups/1/users", map[string]any{"remove": []uint{3}}); code != 200 || f.userRGs[3][1] {
		t.Fatalf("移除用户失败 %d", code)
	}
	if code, _, _ := call(t, root, "DELETE", "/api/v1/settings/resource-groups/2", nil); code != 200 || f.rgs[2].IsDeleted != 1 {
		t.Fatalf("停用失败 %d", code)
	}
	if code, _, _ := call(t, root, "POST", "/api/v1/settings/resource-groups/2/restore", nil); code != 200 || f.rgs[2].IsDeleted != 0 {
		t.Fatalf("恢复失败 %d", code)
	}
	// 用户启停 / 解锁 / 2FA
	f.twofa[3] = []string{"totp"}
	u := f.users[3]
	u.FailedLoginCount = 6
	f.users[3] = u
	for _, p := range []string{"unlock", "reset-2fa", "disable", "enable"} {
		if code, _, body := call(t, root, "POST", "/api/v1/settings/users/3/"+p, nil); code != 200 {
			t.Fatalf("%s: %d %s", p, code, body)
		}
	}
	if f.users[3].FailedLoginCount != 0 || len(f.twofa[3]) != 0 || !f.users[3].IsActive {
		t.Fatalf("%+v", f.users[3])
	}
	if code, _, body := call(t, root, "GET", "/api/v1/settings/users?status=bogus", nil); code != 400 {
		t.Fatalf("%d %s", code, body)
	}
}
