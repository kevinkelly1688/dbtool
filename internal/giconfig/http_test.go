// @author: kevinkelly
// @license: Apache Licence
// @file: http_test.go
// @time: 2026/10/28

package giconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

const api = "/api/v1/settings/goinception"

type httpEnv struct {
	*env
	te   *servertest.Env
	root *servertest.Client
}

func newHTTPEnv(t *testing.T) *httpEnv {
	e := newEnv(t)
	te := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){registerWith(e.svc)}})
	return &httpEnv{env: e, te: te, root: te.Login("root")}
}

// body 解码响应：{"data":…, "code":…, "message":…}
type apiBody struct {
	Data    json.RawMessage `json:"data"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
}

func do(c *servertest.Client, method, path string, body any) (int, apiBody) {
	w := c.Do(method, path, body)
	var b apiBody
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	return w.Code, b
}

func TestHTTPAuthAndPermissions(t *testing.T) {
	h := newHTTPEnv(t)
	routes := []struct{ method, path string }{
		{"GET", api}, {"GET", api + "/status"}, {"PUT", api + "/connection"}, {"POST", api + "/connection/test"},
		{"POST", api + "/file/validate"}, {"POST", api + "/file/params"}, {"POST", api + "/file/patch"}, {"POST", api + "/file/diff"},
		{"POST", api + "/file/save"}, {"POST", api + "/file/import"}, {"GET", api + "/versions/1"}, {"GET", api + "/versions/1/diff"},
		{"POST", api + "/versions/1/rollback"}, {"POST", api + "/restart"},
		{"GET", api + "/restart/status"}, {"PUT", api + "/deploy"}, {"POST", api + "/deploy/preview"}, {"POST", api + "/deploy/check"},
	}
	bob := h.te.Login("bob")
	h.te.Repo.Perms[2] = map[string]bool{"param.open": true, "sqlworkflow.approve": true} // 有别的权限也不行：只有超级用户
	for _, r := range routes {
		if w := h.te.Anonymous(r.method, r.path); w.Code != http.StatusUnauthorized {
			t.Errorf("未登录 %s %s = %d，期望 401", r.method, r.path, w.Code)
		}
		code, b := do(bob, r.method, r.path, map[string]any{})
		if code != http.StatusForbidden || b.Code != "forbidden" {
			t.Errorf("普通用户 %s %s = %d %s，期望 403", r.method, r.path, code, b.Code)
		}
	}
	// 页面：未登录跳转登录，普通用户 403，超级用户 200
	if w := h.te.Anonymous("GET", "/settings/goinception"); w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Location"), "/login") {
		t.Errorf("未登录访问页面：%d %s", w.Code, w.Header().Get("Location"))
	}
	if code, body := bob.Page("/settings/goinception"); code != http.StatusForbidden || strings.Contains(body, "gi-src") {
		t.Errorf("普通用户访问页面 = %d", code)
	}
	// 普通用户的写请求不能产生任何副作用
	if len(h.repo.versions) != 0 || len(h.repo.audits) != 0 {
		t.Fatal("被拒绝的请求不能写数据")
	}
}

func TestHTTPCSRFRequired(t *testing.T) {
	h := newHTTPEnv(t)
	// 带会话 Cookie 但不带 X-CSRF-Token 的写请求必须被拒
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/login", nil)
	h.te.H.ServeHTTP(rec, req)
	var lc *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "dba_login_csrf" {
			lc = c
		}
	}
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(rec.Body.String())
	if lc == nil || m == nil {
		t.Fatal("登录页缺少 CSRF")
	}
	form := strings.NewReader("username=root&password=" + servertest.Password + "&csrf=" + m[1] + "&next=/query")
	req = httptest.NewRequest("POST", "/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(lc)
	rec = httptest.NewRecorder()
	h.te.H.ServeHTTP(rec, req)
	var sc *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "dba_session" {
			sc = c
		}
	}
	if sc == nil {
		t.Fatalf("登录失败 %d", rec.Code)
	}
	for _, p := range []string{"/connection/test", "/file/save", "/restart", "/versions/1/rollback"} {
		req := httptest.NewRequest("POST", api+p, strings.NewReader("{}"))
		req.AddCookie(sc)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.te.H.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "csrf") {
			t.Errorf("缺少 CSRF 令牌的 POST %s = %d %s", p, w.Code, w.Body.String())
		}
	}
	// GET 不需要 CSRF 令牌
	req = httptest.NewRequest("GET", api, nil)
	req.AddCookie(sc)
	w := httptest.NewRecorder()
	h.te.H.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("GET = %d", w.Code)
	}
}

func TestHTTPPageAndAssets(t *testing.T) {
	h := newHTTPEnv(t)
	code, body := h.root.Page("/settings/goinception")
	if code != 200 {
		t.Fatalf("页面 = %d", code)
	}
	for _, want := range []string{"审核引擎", `id="gi-src"`, "保存并重启 goInception", "/static/js/giconfig/giconfig.js", "/static/css/giconfig.css", `id="m-gi-restart"`, "csrf-token"} {
		if !strings.Contains(body, want) {
			t.Errorf("页面缺少 %q", want)
		}
	}
	if strings.Contains(body, "onclick=") || regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]`).MatchString(body) {
		t.Error("页面不能有内联脚本或内联事件（CSP）")
	}
	// 重启方式只剩 docker：页面上有容器名、sudo、预览，没有任何方式选择、systemd / 命令 / SSH / 手动确认的输入
	for _, want := range []string{`id="gi-d-container"`, `id="gi-d-sudo"`, `id="gi-d-preview"`, "docker restart"} {
		if !strings.Contains(body, want) {
			t.Errorf("页面缺少 %q", want)
		}
	}
	for _, bad := range []string{`id="gi-d-mode"`, `id="gi-d-unit"`, `id="gi-d-command"`, `id="gi-d-ssh`, `id="gi-d-fp"`, "systemd", "SSH", "手动重启", "确认已重启", "mark-applied"} {
		if strings.Contains(body, bad) {
			t.Errorf("页面不应再出现 %q", bad)
		}
	}
	for _, p := range []string{"giconfig.js", "ui.js", "state.js", "file.js", "deploy.js", "restart.js"} {
		if code, js := h.root.Page("/static/js/giconfig/" + p); code != 200 || !strings.Contains(js, "export") && p != "giconfig.js" {
			t.Errorf("静态脚本 %s = %d", p, code)
		}
	}
	if code, css := h.root.Page("/static/css/giconfig.css"); code != 200 || !strings.Contains(css, ".gi-cols") {
		t.Errorf("样式 = %d", code)
	}
}

func TestHTTPOverviewShape(t *testing.T) {
	h := newHTTPEnv(t)
	h.seed()
	var o struct {
		TableReady bool `json:"table_ready"`
		Conn       struct {
			Host        string `json:"host"`
			HasPassword bool   `json:"has_password"`
		} `json:"conn"`
		File struct {
			Path   string `json:"path"`
			Editor string `json:"editor"`
			Drift  struct {
				Kind string `json:"kind"`
			} `json:"drift"`
		} `json:"file"`
		Effective struct {
			Version int `json:"version"`
		} `json:"effective"`
		LatestVersion int `json:"latest_version"`
		Catalog       []struct {
			Title string `json:"title"`
			Items []struct {
				Key  string `json:"key"`
				Kind string `json:"kind"`
			} `json:"items"`
		} `json:"catalog"`
		Deploy struct {
			Container  string `json:"container"`
			Configured bool   `json:"configured"`
			Preview    string `json:"preview"`
		} `json:"deploy"`
		Versions []struct {
			Version int    `json:"version"`
			Label   string `json:"label"`
		} `json:"versions"`
	}
	h.root.GetJSON(api, &o)
	if !o.TableReady || o.Conn.Host != "10.20.9.30" || !o.Conn.HasPassword || o.File.Path != testPath || o.File.Drift.Kind != "none" ||
		o.Effective.Version != 1 || o.LatestVersion != 1 || len(o.Catalog) != 4 || o.Deploy.Container != "" || o.Deploy.Configured || o.Deploy.Preview != "" || len(o.Versions) != 1 || o.Versions[0].Label != "当前生效" {
		t.Fatalf("%+v", o)
	}
	// 原始响应里不能出现任何密码或密文
	w := h.root.Do("GET", api, nil)
	for _, secret := range []string{"pw-main", "s3cret"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("响应泄露了 %q", secret)
		}
	}
	// 部署设置视图里只有 docker 方式的字段：没有 mode / unit / command / ssh / 重启方式列表 / 标签 / SSH 凭据标记
	var raw struct {
		Data struct {
			Deploy map[string]json.RawMessage `json:"deploy"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"container", "use_sudo", "restart_timeout_sec", "health_timeout_sec", "auto_rollback", "wait_tasks", "wait_idle_max_sec",
		"config_path", "config_path_source", "configured", "preview"} {
		if _, ok := raw.Data.Deploy[k]; !ok {
			t.Errorf("部署设置视图缺少 %q：%v", k, raw.Data.Deploy)
		}
	}
	for _, k := range []string{"mode", "unit", "command", "ssh", "modes", "labels", "has_ssh_password", "has_ssh_private_key"} {
		if _, ok := raw.Data.Deploy[k]; ok {
			t.Errorf("部署设置视图不应再有 %q", k)
		}
	}
}

func TestHTTPConnectionFlow(t *testing.T) {
	h := newHTTPEnv(t)
	code, b := do(h.root, "PUT", api+"/connection", map[string]any{"target": "main", "host": "10.9.9.9", "port": "6669", "user": "gi", "password": "np"})
	if code != 200 || h.repo.cfg[cfgHost] != "10.9.9.9" || h.repo.cfg[cfgPass] != "np" {
		t.Fatalf("%d %+v %v", code, b, h.repo.cfg)
	}
	code, b = do(h.root, "PUT", api+"/connection", map[string]any{"target": "main", "host": "10.9.9.9", "port": "99999"})
	if code != 400 || b.Code != "bad_request" {
		t.Fatalf("%d %+v", code, b)
	}
	var tr TestResult
	h.root.PostJSON(api+"/connection/test", map[string]any{"target": "main", "host": "10.9.9.9", "port": "6669", "user": "gi", "password": "np"}, &tr)
	if !tr.OK {
		t.Fatalf("%+v", tr)
	}
	if code, _ := do(h.root, "POST", api+"/connection/test", "not json"); code != 400 {
		t.Fatalf("坏请求体 = %d", code)
	}
}

func TestHTTPEditorHelpers(t *testing.T) {
	h := newHTTPEnv(t)
	var rep Report
	h.root.PostJSON(api+"/file/validate", map[string]any{"content": "[inc]\nmax_keys = 999\n"}, &rep)
	if rep.Errors != 1 {
		t.Fatalf("%+v", rep)
	}
	var p ParamsResult
	h.root.PostJSON(api+"/file/params", map[string]any{"content": baseConf}, &p)
	if p.Values["inc.max_update_rows"] != float64(5000) || len(p.SecretSet) != 1 {
		t.Fatalf("%+v", p)
	}
	var out struct{ Content string }
	h.root.PostJSON(api+"/file/patch", map[string]any{"content": baseConf, "changes": []map[string]any{{"section": "inc", "key": "max_update_rows", "value": 7000}}}, &out)
	if !strings.Contains(out.Content, "max_update_rows = 7000") {
		t.Fatal(out.Content)
	}
	if code, b := do(h.root, "POST", api+"/file/patch", map[string]any{"content": baseConf, "changes": []map[string]any{{"section": "inc", "key": "backup_password", "value": "x"}}}); code != 400 {
		t.Fatalf("%d %+v", code, b)
	}
}

func TestHTTPSaveRestartFlow(t *testing.T) {
	h := newHTTPEnv(t)
	h.useDocker()
	h.seed()
	var ov struct {
		File struct {
			Editor string `json:"editor"`
		} `json:"file"`
		LatestVersion int `json:"latest_version"`
	}
	h.root.GetJSON(api, &ov)
	edited := strings.Replace(ov.File.Editor, "5000", "6000", 1)

	// 有任务在执行：409 busy，data 带任务信息
	h.pr.tasks = busyTasks(2)
	req := map[string]any{"content": edited, "base_version": ov.LatestVersion, "restart": true}
	code, b := do(h.root, "POST", api+"/file/save", req)
	var tk Tasks
	_ = json.Unmarshal(b.Data, &tk)
	if code != 409 || b.Code != "busy" || tk.Running != 2 {
		t.Fatalf("%d %+v", code, b)
	}
	// 强制重启
	req["force"] = true
	var res SaveResult
	h.root.PostJSON(api+"/file/save", req, &res)
	if !res.Restarting || res.Version != 2 {
		t.Fatalf("%+v", res)
	}
	h.svc.Wait()
	var st struct {
		Job *Job `json:"job"`
	}
	h.root.GetJSON(api+"/restart/status", &st)
	if st.Job == nil || !st.Job.Done || !st.Job.OK || len(st.Job.Steps) != 3 {
		t.Fatalf("%+v", st.Job)
	}
	// 过期的 base_version
	code, b = do(h.root, "POST", api+"/file/save", map[string]any{"content": edited, "base_version": 1})
	if code != 409 || b.Code != "stale" {
		t.Fatalf("%d %+v", code, b)
	}
	// 校验失败：400 + 报告
	code, b = do(h.root, "POST", api+"/file/save", map[string]any{"content": "[inc\n", "base_version": 2})
	var rep Report
	_ = json.Unmarshal(b.Data, &rep)
	if code != 400 || b.Code != "invalid_config" || rep.Errors != 1 {
		t.Fatalf("%d %+v", code, b)
	}
	// 警告：422 + 报告；确认后通过
	warn := strings.Replace(edited, "[log]", "enable_drop_table = true\n\n[log]", 1)
	code, b = do(h.root, "POST", api+"/file/save", map[string]any{"content": warn, "base_version": 2})
	_ = json.Unmarshal(b.Data, &rep)
	if code != 422 || b.Code != "warnings" || rep.Warnings == 0 {
		t.Fatalf("%d %+v", code, b)
	}
	var res2 SaveResult
	h.root.PostJSON(api+"/file/save", map[string]any{"content": warn, "base_version": 2, "ack_warnings": true, "restart": true, "force": true}, &res2)
	if res2.Version != 3 || len(res2.Warnings) == 0 {
		t.Fatalf("%+v", res2)
	}
	// 上一步的流程已在后台启动，等它结束后再验证历史接口
	h.svc.Wait()
	var vd VersionDetail
	h.root.GetJSON(api+"/versions/3", &vd)
	if vd.Version != 3 || strings.Contains(vd.Content, "s3cret") || vd.Status != StatusApplied {
		t.Fatalf("%+v", vd)
	}
	var df DiffResponse
	h.root.GetJSON(api+"/versions/3/diff?base=1", &df)
	if df.Diff.Added == 0 {
		t.Fatalf("%+v", df.Diff)
	}
	if code, _ := do(h.root, "GET", api+"/versions/abc", nil); code != 400 {
		t.Fatalf("坏版本号 = %d", code)
	}
	if code, b := do(h.root, "GET", api+"/versions/99", nil); code != 404 || b.Code != "not_found" {
		t.Fatalf("%d %+v", code, b)
	}
	// 回滚
	var rb SaveResult
	h.root.PostJSON(api+"/versions/1/rollback", map[string]any{"base_version": 3, "force": true}, &rb)
	h.svc.Wait()
	if !rb.Restarting || rb.Version != 4 || h.repo.version(4).Status != StatusApplied {
		t.Fatalf("%+v", rb)
	}
	// 审计里没有密码
	if strings.Contains(h.repo.auditText(), "s3cret") {
		t.Fatal("审计泄露密码")
	}
}

func TestHTTPDeployFlow(t *testing.T) {
	h := newHTTPEnv(t)
	body := DeployInput{Deploy: DefaultDeploy(), ConfigPath: testPath}
	body.Container, body.UseSudo = "goinception", true
	var v DeployView
	h.root.JSON("PUT", api+"/deploy", body, &v)
	if v.Container != "goinception" || !v.UseSudo || !v.Configured || v.Preview != "sudo -n docker restart goinception" {
		t.Fatalf("%+v", v)
	}
	body.Container = "x;rm -rf /"
	if code, b := do(h.root, "PUT", api+"/deploy", body); code != 400 || b.Code != "bad_request" {
		t.Fatalf("%d %+v", code, b)
	}
	// 预览只生成命令文本，不执行
	var pv struct{ Preview, Problem string }
	body.Container = "a b"
	h.root.PostJSON(api+"/deploy/preview", body, &pv)
	if pv.Preview != "" || pv.Problem == "" {
		t.Fatalf("%+v", pv)
	}
	body.Container, body.UseSudo = "gi-1", false
	h.root.PostJSON(api+"/deploy/preview", body, &pv)
	if pv.Preview != "docker restart gi-1" || pv.Problem != "" {
		t.Fatalf("%+v", pv)
	}
	body.Container = ""
	h.root.PostJSON(api+"/deploy/preview", body, &pv)
	if pv.Preview != "" || !strings.Contains(pv.Problem, "尚未设置") {
		t.Fatalf("%+v", pv)
	}
	var cr CheckResult
	h.root.PostJSON(api+"/deploy/check", nil, &cr)
	if !cr.OK || !strings.Contains(strings.Join(cr.Lines, "\n"), "将执行：sudo -n docker restart goinception") {
		t.Fatalf("%+v", cr)
	}
	if h.rs.count() != 0 {
		t.Fatalf("预览与检查都不能执行命令：%v", h.rs.commands())
	}
	// 旧版页面多提交的 mode / unit / command / ssh 等字段被忽略，不影响保存
	var v2 DeployView
	h.root.JSON("PUT", api+"/deploy", map[string]any{
		"config_path": testPath, "container": "gi-2", "use_sudo": false, "restart_timeout_sec": 60, "health_timeout_sec": 60, "wait_idle_max_sec": 600,
		"auto_rollback": true, "mode": "ssh", "unit": "x;y", "command": "bash -c reboot",
		"ssh": map[string]any{"host": "10.0.0.5", "user": "root"}, "ssh_password": "ssh-pw", "ssh_private_key": "-----BEGIN KEY-----",
	}, &v2)
	if v2.Container != "gi-2" || v2.Preview != "docker restart gi-2" {
		t.Fatalf("%+v", v2)
	}
	// 响应与审计里没有任何凭据（连接密码、配置文件里的密码、旧版页面提交的 SSH 凭据）
	h.seed()
	w := h.root.Do("GET", api, nil)
	for _, bad := range []string{"pw-main", "s3cret", "ssh-pw", "BEGIN KEY", "\"password\":\"", "private_key\":\"", "\"ssh_password\"", "\"ssh_private_key\""} {
		if strings.Contains(w.Body.String(), bad) {
			t.Fatalf("响应里不应出现 %s", bad)
		}
	}
	if a := h.repo.auditText(); strings.Contains(a, "pw-main") || strings.Contains(a, "s3cret") || strings.Contains(a, "ssh-pw") || strings.Contains(a, "BEGIN KEY") {
		t.Fatalf("审计里不应有密码：%s", a)
	}
}

// 已删除的接口：手动方式的“确认已重启”与 SSH 主机指纹，现在都是 404（不是 403/405，也不会有任何副作用）。
func TestHTTPRemovedRoutes(t *testing.T) {
	h := newHTTPEnv(t)
	h.seed()
	n := len(h.repo.audits)
	for _, r := range []struct{ method, path string }{
		{"POST", api + "/versions/1/mark-applied"}, {"POST", api + "/versions/2/mark-applied"}, {"POST", api + "/deploy/ssh-fingerprint"},
		{"GET", api + "/deploy/ssh-fingerprint"}, {"PUT", api + "/versions/1/mark-applied"},
	} {
		if code, _ := do(h.root, r.method, r.path, map[string]any{"host": "10.0.0.5"}); code != http.StatusNotFound {
			t.Errorf("%s %s = %d，期望 404", r.method, r.path, code)
		}
	}
	if len(h.repo.audits) != n || h.rs.count() != 0 {
		t.Fatal("已删除的接口不能有任何副作用")
	}
}

func TestHTTPStatusAndImport(t *testing.T) {
	h := newHTTPEnv(t)
	h.files.set(testPath, baseConf)
	var st Status
	h.root.GetJSON(api+"/status", &st)
	if st.State != "running" || st.Drift.Kind != "unmanaged" {
		t.Fatalf("%+v", st)
	}
	var res SaveResult
	h.root.PostJSON(api+"/file/import", map[string]any{"as_applied": true, "base_version": 0}, &res)
	if res.Version != 1 || res.Status != StatusApplied {
		t.Fatalf("%+v", res)
	}
}

// 未设置容器名：保存并重启 / 仅重启 / 回滚都是 409 deploy_not_set，且不执行任何命令；仅保存可用。
func TestHTTPRestartNeedsContainer(t *testing.T) {
	h := newHTTPEnv(t)
	h.seed()
	ed := strings.Replace(MaskContent(baseConf), "5000", "6000", 1)
	for _, c := range []struct {
		method, path string
		body         map[string]any
	}{
		{"POST", "/file/save", map[string]any{"content": ed, "base_version": 1, "restart": true}},
		{"POST", "/restart", map[string]any{}},
		{"POST", "/versions/1/rollback", map[string]any{"base_version": 1}},
	} {
		code, b := do(h.root, c.method, api+c.path, c.body)
		if code != http.StatusConflict || b.Code != "deploy_not_set" || !strings.Contains(b.Message, "尚未设置 goInception 的 Docker 容器名") {
			t.Errorf("%s %s = %d %+v", c.method, c.path, code, b)
		}
	}
	if h.rs.count() != 0 || len(h.repo.versions) != 1 {
		t.Fatalf("不能执行命令或写版本：%v", h.rs.commands())
	}
	var res SaveResult
	h.root.PostJSON(api+"/file/save", map[string]any{"content": ed, "base_version": 1}, &res)
	if res.Version != 2 || res.Restarting || res.Status != StatusDraft {
		t.Fatalf("仅保存应可用：%+v", res)
	}
}
