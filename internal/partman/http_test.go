// @author: kevinkelly
// @license: Apache Licence
// @file: http_test.go
// @time: 2026/10/28

package partman

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

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

func call(t *testing.T, c *servertest.Client, method, path string, body any) (int, apiResp) {
	t.Helper()
	w := c.Do(method, path, body)
	var r apiResp
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	return w.Code, r
}

func TestHTTPAuthAndPermissions(t *testing.T) {
	h := newHTTPEnv(t)
	for _, p := range []string{"/api/v1/partman/overview", "/api/v1/partman/configs"} {
		if w := h.web.Anonymous("GET", p); w.Code != 401 {
			t.Errorf("未登录 %s 应 401: %d", p, w.Code)
		}
	}
	if w := h.web.Anonymous("GET", "/partman"); w.Code != 302 {
		t.Errorf("未登录页面应跳转登录: %d", w.Code)
	}
	if w := h.web.Anonymous("POST", "/api/v1/partman/preview"); w.Code != 401 {
		t.Errorf("未登录 POST 应 401: %d", w.Code)
	}

	bob := h.web.Login("bob")
	if code, _ := bob.Page("/partman"); code != 403 {
		t.Errorf("没有 partman.view 不能打开页面: %d", code)
	}
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/v1/partman/overview"}, {"GET", "/api/v1/partman/configs"}, {"POST", "/api/v1/partman/preview"},
		{"POST", "/api/v1/partman/configs"}, {"GET", "/api/v1/partman/configs/1/logs"},
	} {
		if code, _ := call(t, bob, c.m, c.p, map[string]any{}); code != 403 {
			t.Errorf("无菜单权限 %s %s 应 403: %d", c.m, c.p, code)
		}
	}

	h.grant(2, permMenu)
	if code, body := bob.Page("/partman"); code != 200 || !strings.Contains(body, "分区维护") || !strings.Contains(body, "/static/js/partman/partman.js") || !strings.Contains(body, `id="pm-rows"`) || !strings.Contains(body, "/static/css/partman.css") {
		t.Errorf("页面: %d", code)
	}
	// 只有菜单权限：可以看，不能增删改、不能执行
	for _, c := range []struct{ m, p string }{
		{"POST", "/api/v1/partman/configs"}, {"PUT", "/api/v1/partman/configs/1"}, {"DELETE", "/api/v1/partman/configs/1"}, {"POST", "/api/v1/partman/configs/1/run"},
	} {
		if code, _ := call(t, bob, c.m, c.p, map[string]any{}); code != 403 {
			t.Errorf("只读用户 %s %s 应 403: %d", c.m, c.p, code)
		}
	}
	code, r := call(t, bob, "GET", "/api/v1/partman/overview", nil)
	var ov Overview
	_ = json.Unmarshal(r.Data, &ov)
	if code != 200 || len(ov.Instances) != 5 || ov.CanAdd || ov.CanEdit || ov.CanDelete {
		t.Errorf("概览: %d %+v", code, ov)
	}
	// 页面里不应该有内联脚本 / 事件（CSP 不允许）
	_, body := bob.Page("/partman")
	for _, tag := range regexp.MustCompile(`(?i)<script[^>]*>`).FindAllString(body, -1) {
		if !strings.Contains(tag, "src=") {
			t.Errorf("内联脚本: %s", tag)
		}
	}
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(body) {
		t.Error("页面不能有内联事件处理属性")
	}
}

func TestHTTPCSRF(t *testing.T) {
	h := newHTTPEnv(t)
	// 手工登录拿到会话 Cookie，但不带 CSRF 头
	w := httptest.NewRecorder()
	h.web.H.ServeHTTP(w, httptest.NewRequest("GET", "/login", nil))
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
	h.web.H.ServeHTTP(w, req)
	var sc *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "dba_session" {
			sc = c
		}
	}
	if sc == nil {
		t.Fatal("登录失败")
	}
	req = httptest.NewRequest("POST", "/api/v1/partman/configs", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sc)
	w = httptest.NewRecorder()
	h.web.H.ServeHTTP(w, req)
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"csrf"`) {
		t.Fatalf("缺少 CSRF 头应 403: %d %s", w.Code, w.Body.String())
	}
	if len(h.repo.configs) != 0 {
		t.Fatal("CSRF 失败不应写入")
	}
}

func TestHTTPConfigLifecycleAndRun(t *testing.T) {
	h := newHTTPEnv(t)
	root := h.web.Login("root")
	h.tgt.tables["log.access_log"] = dailyTable("2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06", "2026-10-07", "2026-10-08", "2026-10-09", "2026-10-10")

	// 创建：非法输入 400；正常 200
	in := goodInput()
	in.Retain = 2
	bad := in
	bad.TbName = "x y"
	if code, r := call(t, root, "POST", "/api/v1/partman/configs", bad); code != 400 || r.Code != "bad_request" {
		t.Fatalf("非法表名: %d %+v", code, r)
	}
	if code, _ := call(t, root, "POST", "/api/v1/partman/configs", "not json"); code != 400 {
		t.Fatalf("坏 JSON: %d", code)
	}
	code, r := call(t, root, "POST", "/api/v1/partman/configs", in)
	var cv ConfigView
	_ = json.Unmarshal(r.Data, &cv)
	if code != 200 || cv.ID == 0 || cv.CnfID != 1 {
		t.Fatalf("创建: %d %+v", code, r)
	}
	id := itoa(cv.ID)
	if code, r = call(t, root, "POST", "/api/v1/partman/configs", in); code != 409 || r.Code != "duplicate" {
		t.Fatalf("重复: %d %+v", code, r)
	}

	// 列表带 total
	code, r = call(t, root, "GET", "/api/v1/partman/configs?isenable=1&limit=10", nil)
	var rows []ConfigView
	_ = json.Unmarshal(r.Data, &rows)
	if code != 200 || r.Total != 1 || len(rows) != 1 || rows[0].InstanceName != "prod-mysql-01" {
		t.Fatalf("列表: %d %+v", code, r)
	}

	// 预览（已保存与草稿）
	code, r = call(t, root, "POST", "/api/v1/partman/preview", map[string]any{"id": cv.ID})
	var pv PlanView
	_ = json.Unmarshal(r.Data, &pv)
	if code != 200 || len(pv.Drops) != 3 || pv.Drops[0] != "p20261001" {
		t.Fatalf("预览: %d %+v", code, r)
	}
	if code, _ = call(t, root, "POST", "/api/v1/partman/preview", map[string]any{"draft": in}); code != 200 {
		t.Fatalf("草稿预览: %d", code)
	}
	if len(h.tgt.executed) != 0 {
		t.Fatal("预览不得执行")
	}

	// 不带确认执行：409 且响应里带计划
	code, r = call(t, root, "POST", "/api/v1/partman/configs/"+id+"/run", map[string]any{})
	var data PlanView
	_ = json.Unmarshal(r.Data, &data)
	if code != 409 || r.Code != "confirm_required" || len(data.Drops) != 3 {
		t.Fatalf("未确认的删除应 409: %d %+v", code, r)
	}
	// 确认后执行
	code, r = call(t, root, "POST", "/api/v1/partman/configs/"+id+"/run", map[string]any{"confirm_drops": pv.Drops})
	var rr RunResult
	_ = json.Unmarshal(r.Data, &rr)
	if code != 200 || rr.Failed || len(rr.Statements) != 3 {
		t.Fatalf("执行: %d %+v", code, r)
	}
	// 日志
	code, r = call(t, root, "GET", "/api/v1/partman/configs/"+id+"/logs", nil)
	var logs []LogView
	_ = json.Unmarshal(r.Data, &logs)
	if code != 200 || len(logs) != 1 || len(logs[0].Items) != 3 || !strings.Contains(logs[0].Items[0].SQL, "DROP PARTITION") {
		t.Fatalf("日志: %d %+v", code, r)
	}
	// 编辑 / 删除
	in.Retain = 5
	if code, _ = call(t, root, "PUT", "/api/v1/partman/configs/"+id, in); code != 200 {
		t.Fatalf("编辑: %d", code)
	}
	if code, _ = call(t, root, "PUT", "/api/v1/partman/configs/abc", in); code != 400 {
		t.Fatalf("编号非法: %d", code)
	}
	if code, _ = call(t, root, "DELETE", "/api/v1/partman/configs/"+id, nil); code != 200 {
		t.Fatalf("删除: %d", code)
	}
	if code, r = call(t, root, "GET", "/api/v1/partman/configs/"+id+"/logs", nil); code != 404 {
		t.Fatalf("删除后日志: %d %+v", code, r)
	}
}

func TestHTTPDropNeedsDeletePermission(t *testing.T) {
	h := newHTTPEnv(t)
	h.grant(2, permMenu, permChange, btnRun)
	bob := h.web.Login("bob")
	h.tgt.tables["log.access_log"] = dailyTable("2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06", "2026-10-07", "2026-10-08", "2026-10-09", "2026-10-10")
	in := goodInput()
	in.Retain = 2
	cfg := h.addConfig(in)
	code, r := call(t, bob, "POST", "/api/v1/partman/configs/"+itoa(cfg.ID)+"/run", map[string]any{"confirm_drops": []string{"p20261001", "p20261002", "p20261003"}})
	if code != 403 || r.Code != "forbidden" {
		t.Fatalf("没有 delete 权限不能删分区: %d %+v", code, r)
	}
	if len(h.tgt.executed) != 0 || len(h.repo.audits) != 0 {
		t.Fatal("被拒绝时不得执行、不写审计")
	}
	h.grant(2, permDelete)
	if code, _ = call(t, bob, "POST", "/api/v1/partman/configs/"+itoa(cfg.ID)+"/run", map[string]any{"confirm_drops": []string{"p20261001", "p20261002", "p20261003"}}); code != 200 {
		t.Fatalf("授权后应成功: %d", code)
	}
}

func TestHTTPTargetErrorIs502(t *testing.T) {
	h := newHTTPEnv(t)
	root := h.web.Login("root")
	h.addConfig(goodInput())
	h.tgt.inspectEr = errAny("dial tcp 10.0.0.1:3306: i/o timeout")
	code, r := call(t, root, "POST", "/api/v1/partman/preview", map[string]any{"id": 1})
	// 目标库不可达是 502 target_error，带上原因方便排查
	if code != 502 || r.Code != "target_error" {
		t.Fatalf("%d %+v", code, r)
	}
}

type errAny string

func (e errAny) Error() string { return string(e) }

func itoa(n uint) string {
	b, _ := json.Marshal(n)
	return string(b)
}
