// @author: kevinkelly
// @license: Apache Licence
// @file: http_test.go
// @time: 2026/10/28

package sqlopt

import (
	"bytes"
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
	"dbtool/internal/settings"
)

type httpEnv struct {
	*servertest.Env
	db     *fakeDB
	runner *fakeRunner
	audit  *fakeAudit
}

func newHTTPEnv(t *testing.T) *httpEnv {
	t.Helper()
	h := &httpEnv{db: newFakeDB(), runner: &fakeRunner{}, audit: &fakeAudit{}}
	hook := func(s *server.Server, rt server.Routes) {
		register(s, rt, NewService(s.Repo(), h.db, h.runner, h.audit))
	}
	h.Env = servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){hook}})
	h.Repo.Instances = []model.Instance{
		{ID: 1, InstanceName: "prod-mysql", DBType: "mysql", Host: "10.0.0.1", Port: 3306},
		{ID: 3, InstanceName: "report-doris", DBType: "doris", Host: "10.0.0.3", Port: 9030},
	}
	h.Repo.Config = map[string]string{}
	return h
}

// grant 给普通用户 bob（ID 2）授权。
func (h *httpEnv) grant(codes ...string) {
	if h.Repo.Perms[2] == nil {
		h.Repo.Perms[2] = map[string]bool{}
	}
	for _, c := range codes {
		h.Repo.Perms[2][c] = true
	}
}

const base = "/api/v1/sqlopt"

type apiResp struct {
	Data    json.RawMessage `json:"data"`
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

func TestPagesAndAuth(t *testing.T) {
	h := newHTTPEnv(t)
	for _, p := range []string{"/sqlanalyze", "/sqladvisor"} {
		if w := h.Anonymous("GET", p); w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
			t.Fatalf("%s 未登录应跳转登录：%d %s", p, w.Code, w.Header().Get("Location"))
		}
	}
	for _, p := range []string{"/options", "/jobs/x", "/instances/1/databases"} {
		if w := h.Anonymous("GET", base+p); w.Code != 401 {
			t.Fatalf("%s 未登录应 401：%d", p, w.Code)
		}
	}
	for _, p := range []string{"/parse", "/analyze", "/explain", "/advise", "/format", "/optimize", "/jobs/x/cancel"} {
		if w := h.Anonymous("POST", base+p); w.Code != 401 {
			t.Fatalf("%s 未登录应 401：%d", p, w.Code)
		}
	}

	bob := h.Login("bob")
	for _, p := range []string{"/sqlanalyze", "/sqladvisor"} {
		if code, body := bob.Page(p); code != 403 {
			t.Fatalf("%s 无权限应 403：%d %s", p, code, body)
		}
	}
	for _, p := range []string{"/options", "/jobs/x", "/jobs/x/items/0", "/instances/1/databases"} {
		if code, r := call(t, bob, "GET", base+p, nil); code != 403 || r.Code != "forbidden" {
			t.Fatalf("%s 无权限应 403：%d %+v", p, code, r)
		}
	}
	for _, p := range []string{"/parse", "/analyze", "/explain", "/advise", "/format", "/optimize", "/jobs/x/cancel"} {
		if code, _ := call(t, bob, "POST", base+p, map[string]any{}); code != 403 {
			t.Fatalf("%s 无权限应 403：%d", p, code)
		}
	}

	h.grant(PermMenuAnalyze)
	code, body := bob.Page("/sqlanalyze")
	if code != 200 || !strings.Contains(body, "SQL 体检") || !strings.Contains(body, `id="sa-file"`) || !strings.Contains(body, "/static/js/sqlopt/analyze.js") || !strings.Contains(body, `data-can-analyze="false"`) {
		t.Fatalf("分析页：%d", code)
	}
	if code, _ = bob.Page("/sqladvisor"); code != 403 {
		t.Fatal("只有 sqlanalyze.view 不能进优化工具页")
	}
	h.grant(PermMenuAdvisor, PermAnalyze)
	code, body = bob.Page("/sqladvisor")
	if code != 200 || !strings.Contains(body, "智能调优") || !strings.Contains(body, `id="ad-editor"`) || !strings.Contains(body, "/static/vendor/ace/ace.js") || !strings.Contains(body, "/static/js/sqlopt/advisor.js") {
		t.Fatalf("优化工具页：%d", code)
	}
	if _, body = bob.Page("/sqlanalyze"); !strings.Contains(body, `data-can-analyze="true"`) {
		t.Fatal("有 sqlanalyze.run 时页面应开放分析按钮")
	}
	root := h.Login("root")
	if code, _ = root.Page("/sqladvisor"); code != 200 {
		t.Fatal("超级用户可访问")
	}
}

// 没有 CSRF 头的写请求被拒绝（由 server 的 API 中间件统一处理，这里确认本模块的接口都挂在它后面）。
func TestCSRFRequired(t *testing.T) {
	h := newHTTPEnv(t)
	w := h.H
	get := httptest.NewRecorder()
	w.ServeHTTP(get, httptest.NewRequest("GET", "/login", nil))
	var lc *http.Cookie
	for _, c := range get.Result().Cookies() {
		if c.Name == "dba_login_csrf" {
			lc = c
		}
	}
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(get.Body.String())
	form := url.Values{"username": {"root"}, "password": {servertest.Password}, "csrf": {m[1]}, "next": {"/query"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(lc)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	var sess *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "dba_session" {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("登录失败")
	}
	for _, p := range []string{"/parse", "/analyze", "/explain", "/advise", "/format", "/optimize", "/jobs/x/cancel"} {
		req := httptest.NewRequest("POST", base+p, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(sess)
		rec := httptest.NewRecorder()
		w.ServeHTTP(rec, req)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), `"csrf"`) {
			t.Fatalf("%s 缺少 CSRF 头应 403：%d %s", p, rec.Code, rec.Body.String())
		}
	}
}

func TestOptionsAndDatabases(t *testing.T) {
	h := newHTTPEnv(t)
	h.grant(PermMenuAdvisor, PermSOAR)
	bob := h.Login("bob")
	var o struct {
		Instances []instanceView        `json:"instances"`
		Tools     map[string]toolStatus `json:"tools"`
		Perms     map[string]bool       `json:"perms"`
		Limits    map[string]int        `json:"limits"`
		CanConfig bool                  `json:"can_configure"`
	}
	bob.GetJSON(base+"/options", &o)
	if len(o.Instances) != 0 {
		t.Fatalf("bob 没有关联实例：%+v", o.Instances)
	}
	if !o.Perms["explain"] || !o.Perms[ToolSOAR] || o.Perms[ToolTuning] || o.Perms["analyze"] || o.Limits["max_text_bytes"] != MaxTextBytes {
		t.Fatalf("%+v", o)
	}
	if o.Tools[ToolSOAR].Ready || o.Tools[ToolSOAR].Message == "" || !o.Tools[ToolTuning].Ready {
		t.Fatalf("工具状态：%+v", o.Tools)
	}
	if o.CanConfig {
		t.Fatal("非超级用户不应看到“去配置”")
	}
	root := h.Login("root")
	root.GetJSON(base+"/options", &o)
	if !o.CanConfig {
		t.Fatal("超级用户应能去系统配置页配置 SOAR")
	}
	if len(o.Instances) != 2 || o.Instances[0].Name != "prod-mysql" {
		t.Fatalf("%+v", o.Instances)
	}
	var dbs []string
	root.GetJSON(base+"/instances/1/databases", &dbs)
	if strings.Join(dbs, ",") != "report,secret,shop" {
		t.Fatalf("%v", dbs)
	}
	var detail DBList
	root.GetJSON(base+"/instances/1/databases?detail=1", &detail)
	if strings.Join(detail.Databases, ",") != "report,secret,shop" || detail.Total != 5 || detail.HiddenSystem != 2 {
		t.Fatalf("detail=1 应带统计：%+v", detail)
	}
	if code, r := call(t, bob, "GET", base+"/instances/1/databases", nil); code != 403 || r.Code != "no_instance" {
		t.Fatalf("bob 不能看未关联实例的库：%d %+v", code, r)
	}
	if code, _ := call(t, root, "GET", base+"/instances/abc/databases", nil); code != 400 {
		t.Fatalf("参数错误 %d", code)
	}
	if code, r := call(t, root, "GET", base+"/instances/99/databases", nil); code != 404 || r.Code != "not_found" {
		t.Fatalf("%d %+v", code, r)
	}
}

func TestParseEndpoint(t *testing.T) {
	h := newHTTPEnv(t)
	h.grant(PermAnalyze)
	bob := h.Login("bob")
	var out struct {
		Rows   []parsedRow `json:"rows"`
		Total  int         `json:"total"`
		Format string      `json:"format"`
	}
	bob.PostJSON(base+"/parse", map[string]string{"text": "select 1;\nselect '" + strings.Repeat("长", 500) + "'"}, &out)
	if out.Total != 2 || out.Format != FormatSQL || out.Rows[0].SQL != "select 1" || out.Rows[1].Chars != len([]rune("select '"))+500+1 || len([]rune(out.Rows[1].SQL)) > itemSQLPreview+1 {
		t.Fatalf("%+v", out)
	}
	bob.PostJSON(base+"/parse", map[string]string{"text": mapperXML}, &out)
	if out.Format != FormatMapper || out.Total != 6 {
		t.Fatalf("%+v", out)
	}
	if code, r := call(t, bob, "POST", base+"/parse", map[string]string{"text": "a\x00b"}); code != 400 || r.Code != "bad_request" {
		t.Fatalf("%d %+v", code, r)
	}
	if code, _ := call(t, bob, "POST", base+"/parse", `not json`); code != 400 {
		t.Fatalf("坏 JSON 应 400：%d", code)
	}
	// 请求体超过上限：413，不会把整个内容读进内存
	big := bytes.Repeat([]byte("a"), MaxRequestBytes+1024)
	payload := append(append([]byte(`{"text":"`), big...), []byte(`"}`)...)
	if code, r := call(t, bob, "POST", base+"/parse", payload); code != 413 || r.Code != "too_large" {
		t.Fatalf("%d %+v", code, r.Code)
	}
	// 文本超过 5MB（请求体未超限）：400
	mid := strings.Repeat("a", MaxTextBytes+10)
	if code, _ := call(t, bob, "POST", base+"/parse", map[string]string{"text": mid}); code != 400 {
		t.Fatalf("超过 5MB 应 400：%d", code)
	}
}

func pollHTTP(t *testing.T, c *servertest.Client, id string) JobView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var v JobView
		c.GetJSON(base+"/jobs/"+id, &v)
		if v.Status != StatusQueued && v.Status != StatusRunning {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("任务超时")
	return JobView{}
}

func TestAnalyzeOverHTTP(t *testing.T) {
	h := newHTTPEnv(t)
	h.grant(PermMenuAnalyze, PermAnalyze, "query.admin_run")
	h.db.tables["orders"] = ordersFake()
	bob := h.Login("bob")
	var v JobView
	bob.PostJSON(base+"/analyze", AnalyzeReq{Text: "select * from orders where phone = 1;\ndelete from orders", InstanceID: 1, DB: "shop"}, &v)
	if v.ID == "" || v.Total != 2 {
		t.Fatalf("%+v", v)
	}
	done := pollHTTP(t, bob, v.ID)
	if done.Status != StatusDone || done.Done != 2 || done.Summary == nil {
		t.Fatalf("%+v", done)
	}
	var d ItemDetail
	bob.GetJSON(base+"/jobs/"+v.ID+"/items/0", &d)
	if d.Report == nil || !hasID(d.Report.Findings, "S001") || d.Report.Plan == nil {
		t.Fatalf("%+v", d)
	}
	if code, r := call(t, bob, "GET", base+"/jobs/"+v.ID+"/items/9", nil); code != 404 {
		t.Fatalf("%d %+v", code, r)
	}
	if code, _ := call(t, bob, "GET", base+"/jobs/"+v.ID+"/items/x", nil); code != 400 {
		t.Fatalf("%d", code)
	}
	// 超级用户可以看别人的任务
	root := h.Login("root")
	root.GetJSON(base+"/jobs/"+v.ID, &done)
	if code, _ := call(t, bob, "POST", base+"/jobs/"+v.ID+"/cancel", nil); code != 409 {
		t.Fatalf("已结束的任务取消应 409：%d", code)
	}
	if es := h.audit.all(); len(es) != 1 || es[0].Action != "SQL分析" || es[0].UserName != "bob" {
		t.Fatalf("%+v", es)
	}
	// 无 sqlanalyze.run 权限不能分析
	delete(h.Repo.Perms[2], PermAnalyze)
	if code, _ := call(t, bob, "POST", base+"/analyze", AnalyzeReq{Text: "select 1"}); code != 403 {
		t.Fatalf("缺少 sqlanalyze.run 应 403：%d", code)
	}
	// 业务错误的格式
	h.grant(PermAnalyze)
	if code, r := call(t, bob, "POST", base+"/analyze", AnalyzeReq{Text: ""}); code != 400 || r.Code != "bad_request" || r.Message == "" {
		t.Fatalf("%d %+v", code, r)
	}
	if code, r := call(t, bob, "POST", base+"/analyze", AnalyzeReq{Text: "select 1", InstanceID: 3}); code != 400 || r.Code != "unsupported" {
		t.Fatalf("%d %+v", code, r)
	}
}

func TestAdvisorEndpoints(t *testing.T) {
	h := newHTTPEnv(t)
	h.grant(PermMenuAdvisor, "query.admin_run")
	h.db.tables["orders"] = ordersFake()
	bob := h.Login("bob")

	var ex ExplainResult
	bob.PostJSON(base+"/explain", ExplainReq{InstanceID: 1, DB: "shop", SQL: "select * from orders where user_id = 1"}, &ex)
	if ex.Plan == nil || !hasID(ex.Findings, "P001") {
		t.Fatalf("%+v", ex)
	}
	if code, r := call(t, bob, "POST", base+"/explain", ExplainReq{InstanceID: 1, DB: "shop", SQL: "drop table t"}); code != 400 || r.Code != "bad_request" {
		t.Fatalf("%d %+v", code, r)
	}
	if code, r := call(t, bob, "POST", base+"/explain", ExplainReq{InstanceID: 1, DB: "nope", SQL: "select 1"}); code != 403 || r.Code != "no_database" {
		t.Fatalf("%d %+v", code, r)
	}
	var rep Report
	bob.PostJSON(base+"/advise", AdviseReq{InstanceID: 1, DB: "shop", SQL: "select * from orders where phone = 1"}, &rep)
	if !hasID(rep.Findings, "S001") || rep.Score >= 100 {
		t.Fatalf("%+v", rep)
	}
	var f struct {
		SQL string `json:"sql"`
	}
	bob.PostJSON(base+"/format", map[string]string{"sql": "select a,b from t where x=1"}, &f)
	if f.SQL != "SELECT a,\n  b\nFROM t\nWHERE x=1" {
		t.Fatalf("%q", f.SQL)
	}
	if code, _ := call(t, bob, "POST", base+"/format", map[string]string{"sql": strings.Repeat("a", MaxSQLBytes*4+1)}); code != 400 {
		t.Fatalf("%d", code)
	}
	es := h.audit.all()
	if len(es) != 2 || es[0].Action != "SQL优化-执行计划" || es[1].Action != "SQL优化-内置建议" {
		t.Fatalf("%+v", es)
	}
}

func TestOptimizeEndpointPerTool(t *testing.T) {
	h := newHTTPEnv(t)
	h.grant(PermSOAR, "query.admin_run")
	bob := h.Login("bob")
	req := OptimizeReq{Tool: ToolSOAR, InstanceID: 1, DB: "shop", SQL: "select * from orders where id = 1"}
	// 工具没配置：明确提示（409），不是 500
	if code, r := call(t, bob, "POST", base+"/optimize", req); code != 409 || r.Code != "tool_not_configured" || !strings.Contains(r.Message, "SOAR") {
		t.Fatalf("%d %+v", code, r)
	}
	h.Repo.Config[settings.KeyOptimizerSoarPath] = testToolPath
	h.Repo.Config[settings.KeyOptimizerSoarTestDSN] = "tester:tpass1234@10.9.9.9:3306/test"
	h.runner.res = &ExecResult{Stdout: "advice text"}
	var v JobView
	bob.PostJSON(base+"/optimize", req, &v)
	done := pollHTTP(t, bob, v.ID)
	if done.Status != StatusDone || done.Kind != KindOptimize {
		t.Fatalf("%+v", done)
	}
	var d ItemDetail
	bob.GetJSON(base+"/jobs/"+v.ID+"/items/0", &d)
	if d.Result == nil || d.Result.Output != "advice text" {
		t.Fatalf("%+v", d)
	}
	// 其他工具需要各自的权限码
	for _, tool := range []string{ToolTuning} {
		r := req
		r.Tool = tool
		r.Options = []string{OptSQLPlan}
		if code, resp := call(t, bob, "POST", base+"/optimize", r); code != 403 || resp.Code != "forbidden" {
			t.Fatalf("%s 缺权限应 403：%d %+v", tool, code, resp)
		}
	}
	h.grant(PermSQLTuning)
	r := req
	r.Tool, r.Options = ToolTuning, []string{OptSQLPlan}
	bob.PostJSON(base+"/optimize", r, &v)
	pollHTTP(t, bob, v.ID)
	bob.GetJSON(base+"/jobs/"+v.ID+"/items/0", &d)
	if d.Result == nil || d.Result.Tuning == nil || d.Result.Tuning.Plan == nil {
		t.Fatalf("%+v", d)
	}
	for _, gone := range []string{"x", "sqladvisor"} { // 已下线的 SQLAdvisor 与未知工具一样被拒绝
		if code, resp := call(t, bob, "POST", base+"/optimize", OptimizeReq{Tool: gone, InstanceID: 1, SQL: "select 1"}); code != 400 {
			t.Fatalf("%s: %d %+v", gone, code, resp)
		}
	}
	// 只有 optimize_* 权限（没有 menu_*）的用户也能轮询自己的任务
	bob.GetJSON(base+"/jobs/"+v.ID, &done)
	// 取消运行中的任务
	h.runner.block = make(chan struct{})
	bob.PostJSON(base+"/optimize", req, &v)
	var ok struct {
		OK bool `json:"ok"`
	}
	bob.PostJSON(base+"/jobs/"+v.ID+"/cancel", nil, &ok)
	if !ok.OK {
		t.Fatal("取消应成功")
	}
	if c := pollHTTP(t, bob, v.ID); c.Status != StatusCanceled {
		t.Fatalf("%+v", c)
	}
}
