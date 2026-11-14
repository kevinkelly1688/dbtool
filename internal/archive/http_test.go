// @author: kevinkelly
// @license: Apache Licence
// @file: http_test.go
// @time: 2026/10/28

package archive

import (
	"encoding/json"
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
	e := newEnv(t)
	web := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){
		func(s *server.Server, rt server.Routes) { register(s, rt, e.svc) },
	}})
	// servertest 里 root 是超级用户（id 1）、bob 是普通用户（id 2）：都放进订单组
	for id, name := range map[uint]string{1: "root", 2: "bob"} {
		e.repo.users[id] = e.repo.users[9]
		u := e.repo.users[id]
		u.ID, u.Username = id, name
		e.repo.users[id] = u
		e.repo.userRGs[id] = []uint{1}
	}
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
	Outcome string          `json:"outcome"`
	KPI     map[string]any  `json:"kpi"`
}

func call(t *testing.T, c *servertest.Client, method, path string, body any) (int, apiResp) {
	t.Helper()
	w := c.Do(method, path, body)
	var r apiResp
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	return w.Code, r
}

const api = "/api/v1/archive"

// archiveWriteButtons 是归档的申请（提交 / 预演）与管理（暂停、执行、终止、还原、删除）按钮。
var archiveWriteButtons = []string{"archive.apply", "archive.dryrun", "archive.pause", "archive.run", "archive.stop", "archive.restore", "archive.delete"}

func TestHTTPPage(t *testing.T) {
	h := newHTTPEnv(t)
	if w := h.web.Anonymous("GET", "/archive"); w.Code != 302 {
		t.Errorf("未登录页面应跳转登录: %d", w.Code)
	}
	bob := h.web.Login("bob")
	if code, _ := bob.Page("/archive"); code != 403 {
		t.Errorf("没有 archive.view 访问页面应 403: %d", code)
	}
	h.grant(2, "archive.view")
	code, body := bob.Page("/archive")
	if code != 200 {
		t.Fatalf("页面: %d", code)
	}
	for _, want := range []string{`id="ar-tabs"`, `id="ar-view-tasks"`, `id="ar-view-runs"`, `id="ar-view-stores"`, `id="ar-view-policy"`, `id="ar-view-wizard"`,
		`id="ar-drawer-mask"`, `id="ar-modal-mask"`, `id="ar-msg"`, `id="ar-notready"`, "/static/js/archive/main.js", "/static/css/archive.css", "数据归档"} {
		if !strings.Contains(body, want) {
			t.Errorf("页面缺少 %s", want)
		}
	}
	for _, tag := range regexp.MustCompile(`(?i)<script[^>]*>`).FindAllString(body, -1) {
		if !strings.Contains(tag, "src=") {
			t.Errorf("内联脚本: %s", tag)
		}
	}
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(body) || strings.Contains(strings.ToLower(body), "onclick=") {
		t.Error("页面不能有内联事件处理属性")
	}
	// 菜单里能看到入口
	if !strings.Contains(body, `href="/archive"`) {
		t.Error("侧栏应有数据归档入口")
	}
}

func TestHTTPAuthAndPermissions(t *testing.T) {
	h := newHTTPEnv(t)
	for _, p := range []string{api + "/meta", api + "/tasks", api + "/runs", api + "/progress", api + "/stores", api + "/policy"} {
		if w := h.web.Anonymous("GET", p); w.Code != 401 {
			t.Errorf("未登录 %s 应 401: %d", p, w.Code)
		}
	}
	if w := h.web.Anonymous("POST", api+"/tasks"); w.Code != 401 {
		t.Errorf("未登录 POST 应 401: %d", w.Code)
	}
	bob := h.web.Login("bob")
	all := []struct{ m, p string }{
		{"GET", "/meta"}, {"GET", "/sources/instances"}, {"GET", "/sources/databases?instance_id=1"}, {"GET", "/sources/time-probe?instance_id=1&db=shop&table=orders&column=created_ms"}, {"GET", "/flow?resource_group_id=1"},
		{"GET", "/tasks"}, {"GET", "/tasks/1"}, {"POST", "/tasks"}, {"PUT", "/tasks/1"}, {"POST", "/tasks/1/submit"}, {"POST", "/tasks/1/approve"},
		{"POST", "/tasks/1/reject"}, {"POST", "/tasks/1/pause"}, {"POST", "/tasks/1/resume"}, {"POST", "/tasks/1/run"}, {"POST", "/tasks/1/disable"},
		{"DELETE", "/tasks/1"}, {"POST", "/preview"}, {"POST", "/check"}, {"GET", "/runs"}, {"GET", "/runs/1"}, {"GET", "/runs/1/events"},
		{"GET", "/runs/1/manifest"}, {"POST", "/runs/1/pause"}, {"POST", "/runs/1/resume"}, {"POST", "/runs/1/stop"}, {"POST", "/runs/1/restore"},
		{"GET", "/progress"}, {"GET", "/stores"}, {"POST", "/stores"}, {"POST", "/stores/test"}, {"PUT", "/stores/1"}, {"POST", "/stores/1/test"},
		{"POST", "/stores/1/enable"}, {"DELETE", "/stores/1"}, {"GET", "/policy"}, {"POST", "/policy/diff"}, {"PUT", "/policy"},
	}
	for _, c := range all {
		if code, _ := call(t, bob, c.m, api+c.p, map[string]any{}); code != 403 {
			t.Errorf("无任何权限 %s %s 应 403: %d", c.m, c.p, code)
		}
	}
	// 只有查看权限：写接口都 403
	h.grant(2, "archive.view", "archive.cfg_view")
	writes := []struct{ m, p string }{
		{"POST", "/tasks"}, {"PUT", "/tasks/1"}, {"POST", "/tasks/1/submit"}, {"POST", "/tasks/1/pause"}, {"POST", "/tasks/1/run"}, {"POST", "/tasks/1/disable"},
		{"DELETE", "/tasks/1"}, {"POST", "/check"}, {"POST", "/runs/1/pause"}, {"POST", "/runs/1/stop"}, {"POST", "/runs/1/restore"},
		{"POST", "/stores"}, {"POST", "/stores/test"}, {"PUT", "/stores/1"}, {"DELETE", "/stores/1"}, {"PUT", "/policy"},
	}
	for _, c := range writes {
		if code, _ := call(t, bob, c.m, api+c.p, map[string]any{}); code != 403 {
			t.Errorf("只读用户 %s %s 应 403: %d", c.m, c.p, code)
		}
	}
	// archive.cfg_view：查看存储与策略
	if code, r := call(t, bob, "GET", api+"/stores", nil); code != 200 {
		t.Errorf("cfg_view 应能看存储: %d %s", code, r.Message)
	}
	// 锁定码：即使给了普通用户也不行（Locked 按钮不能授予）
	h.grant(2, archiveWriteButtons...)
	if code, _ := call(t, bob, "PUT", api+"/policy", map[string]any{"confirm": true}); code != 403 {
		t.Errorf("锁定码 policy_edit: %d", code)
	}
	if code, _ := call(t, bob, "POST", api+"/stores", map[string]any{"name": "x", "type": "local", "path": "/x"}); code != 403 {
		t.Errorf("锁定码 store_edit: %d", code)
	}
}

func TestHTTPEndToEnd(t *testing.T) {
	h := newHTTPEnv(t)
	h.grant(2, append([]string{"archive.view", "archive.cfg_view"}, archiveWriteButtons...)...)
	bob, root := h.web.Login("bob"), h.web.Login("root")
	// 审批流：bob 提交，root（超级用户）审批——但一级 / 二级权限组里没有人也行，超管可审
	if code, r := call(t, bob, "GET", api+"/meta", nil); code != 200 || !strings.Contains(string(r.Data), `"ready":true`) || !strings.Contains(string(r.Data), `"batch_size":[100,10000]`) {
		t.Fatalf("meta: %d %s", code, r.Data)
	}
	if code, r := call(t, bob, "GET", api+"/sources/instances", nil); code != 200 || !strings.Contains(string(r.Data), "mysql-prod-m1") || strings.Contains(string(r.Data), "mssql") {
		t.Errorf("实例: %d %s", code, r.Data)
	}
	// 整型时间戳列的探测：只读，返回统计结果
	if code, r := call(t, bob, "GET", api+"/sources/time-probe?instance_id=1&db=shop&table=orders&column=created_ms", nil); code != 200 ||
		!strings.Contains(string(r.Data), `"guess_unit":"epoch_ms"`) || !strings.Contains(string(r.Data), `"has_range":true`) {
		t.Errorf("时间列探测: %d %s", code, r.Data)
	}
	if code, r := call(t, bob, "GET", api+"/sources/time-probe?instance_id=1&db=shop&table=orders&column=note", nil); code != 400 {
		t.Errorf("非整型列应 400: %d %s", code, r.Data)
	}
	if code, r := call(t, bob, "GET", api+"/sources/columns?instance_id=1&db=shop&table=orders", nil); code != 200 ||
		!strings.Contains(string(r.Data), `"is_int":true`) {
		t.Errorf("列信息应标出整型候选列: %d %s", code, r.Data)
	}
	in := h.baseInput("HTTP 任务")
	// 草稿
	code, r := call(t, bob, "POST", api+"/tasks", func() TaskInput { d := in; d.Submit = false; return d }())
	if code != 200 || r.Outcome != "draft" {
		t.Fatalf("草稿: %d %+v", code, r)
	}
	var draft TaskView
	_ = json.Unmarshal(r.Data, &draft)
	if draft.Status != StDraft || draft.Allow.Edit != true {
		t.Errorf("草稿视图: %+v", draft)
	}
	// 非法输入 → 400 validation + issues
	bad := in
	bad.Name = "非法"
	bad.RetentionDays = 1
	code, r = call(t, bob, "POST", api+"/tasks", bad)
	if code != 400 || r.Code != "validation" {
		t.Errorf("校验: %d %+v", code, r)
	}
	// 预览
	if code, r = call(t, bob, "POST", api+"/preview", in); code != 200 || !strings.Contains(string(r.Data), "next_runs") {
		t.Errorf("预览: %d %s", code, r.Data)
	}
	// 试运行 → 提交
	in.TaskID = draft.ID
	code, r = call(t, bob, "POST", api+"/check", in)
	if code != 200 {
		t.Fatalf("试运行: %d %+v", code, r)
	}
	var chk CheckResponse
	_ = json.Unmarshal(r.Data, &chk)
	if chk.CheckToken == "" || chk.Report == nil || chk.Blocked {
		t.Fatalf("试运行结果: %+v", chk)
	}
	sub := in
	sub.Submit, sub.CheckToken, sub.AckWarn, sub.AckBig = true, chk.CheckToken, true, true
	code, r = call(t, bob, "PUT", api+"/tasks/"+itoa(int(draft.ID)), sub)
	if code != 200 || r.Outcome != "submitted" {
		t.Fatalf("提交: %d %+v", code, r)
	}
	var tv TaskView
	_ = json.Unmarshal(r.Data, &tv)
	if tv.Status != StPending || tv.Approval == nil {
		t.Fatalf("待审批: %+v", tv)
	}
	// 自批被拒（bob 不是审批人也是自己提的）
	if code, r = call(t, bob, "POST", api+"/tasks/"+itoa(int(tv.ID))+"/approve", map[string]any{}); code != 409 && code != 403 {
		t.Errorf("自批: %d %+v", code, r)
	}
	// 驳回必须有原因
	if code, r = call(t, root, "POST", api+"/tasks/"+itoa(int(tv.ID))+"/reject", map[string]any{"reason": ""}); code != 400 {
		t.Errorf("无原因驳回: %d", code)
	}
	for i := 0; i < 2; i++ {
		if code, r = call(t, root, "POST", api+"/tasks/"+itoa(int(tv.ID))+"/approve", map[string]any{"remark": "ok"}); code != 200 {
			t.Fatalf("审批 %d: %d %+v", i, code, r)
		}
	}
	_ = json.Unmarshal(r.Data, &tv)
	if tv.Status != StEnabled {
		t.Fatalf("应启用: %+v", tv)
	}
	// 列表 / 详情
	code, r = call(t, root, "GET", api+"/tasks?status=enabled", nil)
	if code != 200 || r.Total != 1 || r.KPI["enabled"] != float64(1) {
		t.Errorf("列表: %d %+v", code, r)
	}
	if code, r = call(t, root, "GET", api+"/tasks/"+itoa(int(tv.ID)), nil); code != 200 || !strings.Contains(string(r.Data), `"approval_detail"`) || !strings.Contains(string(r.Data), `"sql"`) {
		t.Errorf("详情: %d", code)
	}
	// 立即执行：需要 archive_mgt + confirm
	if code, _ = call(t, bob, "POST", api+"/tasks/"+itoa(int(tv.ID))+"/run", map[string]any{"ignore_window": true}); code != 422 {
		t.Errorf("缺 confirm 应 422: %d", code)
	}
	code, r = call(t, bob, "POST", api+"/tasks/"+itoa(int(tv.ID))+"/run", map[string]any{"ignore_window": true, "confirm": true})
	if code != 200 {
		t.Fatalf("立即执行: %d %+v", code, r)
	}
	h.wait()
	code, r = call(t, bob, "GET", api+"/runs?trigger=manual&task_id="+itoa(int(tv.ID)), nil)
	if code != 200 || r.Total != 1 {
		t.Fatalf("运行列表: %d %+v", code, r)
	}
	var runs []RunView
	_ = json.Unmarshal(r.Data, &runs)
	if runs[0].Result != "success" || runs[0].Deleted != 300 {
		t.Errorf("运行: %+v", runs[0])
	}
	if code, r = call(t, bob, "GET", api+"/runs/"+itoa(int(runs[0].ID)), nil); code != 200 || !strings.Contains(string(r.Data), `"backups"`) {
		t.Errorf("运行详情: %d", code)
	}
	if code, r = call(t, bob, "GET", api+"/runs/"+itoa(int(runs[0].ID))+"/events", nil); code != 200 || !strings.Contains(string(r.Data), "运行开始") {
		t.Errorf("事件: %d %s", code, r.Data)
	}
	if code, r = call(t, bob, "GET", api+"/progress", nil); code != 200 || string(r.Data) == "null" {
		t.Errorf("进度应是数组: %d %s", code, r.Data)
	}
	// 恢复：输入任务名
	if code, _ = call(t, bob, "POST", api+"/runs/"+itoa(int(runs[0].ID))+"/restore", map[string]any{"confirm_name": "错的"}); code != 422 {
		t.Errorf("恢复确认: %d", code)
	}
	if code, r = call(t, bob, "POST", api+"/runs/"+itoa(int(runs[0].ID))+"/restore", map[string]any{"confirm_name": "HTTP 任务"}); code != 200 {
		t.Fatalf("恢复: %d %+v", code, r)
	}
	h.wait()
	// 404 / 400
	if code, _ = call(t, bob, "GET", api+"/tasks/99999", nil); code != 404 {
		t.Errorf("不存在的任务: %d", code)
	}
	if code, _ = call(t, bob, "GET", api+"/runs/99999", nil); code != 404 {
		t.Errorf("不存在的运行: %d", code)
	}
	if code, _ = call(t, bob, "GET", api+"/tasks/abc", nil); code != 400 {
		t.Errorf("非法 id: %d", code)
	}
	if code, _ = call(t, bob, "GET", api+"/tasks/0", nil); code != 400 {
		t.Errorf("id=0: %d", code)
	}
	w := bob.Do("POST", api+"/preview", nil)
	if w.Code != 400 {
		t.Errorf("空请求体: %d", w.Code)
	}
	// 审计已写入
	acts := h.repo.auditActions()
	for _, want := range []string{"数据归档-保存草稿", "数据归档-试运行", "数据归档-提交申请", "数据归档-审批通过", "数据归档-立即执行", "数据归档-恢复数据"} {
		if !contains(acts, want) {
			t.Errorf("缺审计 %s: %v", want, acts)
		}
	}
	// 清单下载：没有文件备份 → 404
	if code, _ = call(t, bob, "GET", api+"/runs/"+itoa(int(runs[0].ID))+"/manifest", nil); code != 404 {
		t.Errorf("无清单: %d", code)
	}
}

func TestHTTPStoresAndPolicy(t *testing.T) {
	h := newHTTPEnv(t)
	root := h.web.Login("root")
	code, r := call(t, root, "POST", api+"/stores", map[string]any{"name": "MinIO", "type": "s3", "endpoint": "http://minio:9000", "bucket": "dba", "ak": "AKIA123456", "sk": "super-secret"})
	if code != 200 || strings.Contains(string(r.Data), "super-secret") {
		t.Fatalf("创建存储: %d %s", code, r.Data)
	}
	var sv StoreView
	_ = json.Unmarshal(r.Data, &sv)
	if code, r = call(t, root, "POST", api+"/stores/"+itoa(int(sv.ID))+"/test", nil); code != 200 || !strings.Contains(string(r.Data), `"ok":true`) {
		t.Errorf("测试连接: %d %s", code, r.Data)
	}
	if code, r = call(t, root, "POST", api+"/stores/test", map[string]any{"name": "xx", "type": "local", "path": "/data/x"}); code != 200 {
		t.Errorf("草稿测试: %d %s", code, r.Data)
	}
	if code, _ = call(t, root, "POST", api+"/stores/"+itoa(int(sv.ID))+"/enable", map[string]any{}); code != 400 {
		t.Errorf("缺 enable 字段: %d", code)
	}
	if code, r = call(t, root, "POST", api+"/stores/"+itoa(int(sv.ID))+"/enable", map[string]any{"enable": false}); code != 200 || !strings.Contains(string(r.Data), `"status":"off"`) {
		t.Errorf("停用: %d %s", code, r.Data)
	}
	if code, r = call(t, root, "GET", api+"/stores?for=picker", nil); code != 200 || strings.Contains(string(r.Data), "minio:9000") {
		t.Errorf("picker 不应带地址: %d %s", code, r.Data)
	}
	if code, _ = call(t, root, "DELETE", api+"/stores/"+itoa(int(sv.ID)), nil); code != 200 {
		t.Errorf("删除存储: %d", code)
	}
	// 策略
	code, r = call(t, root, "GET", api+"/policy", nil)
	var p Policy
	_ = json.Unmarshal(r.Data, &p)
	if code != 200 || p.MinRetentionDays != 30 {
		t.Fatalf("策略: %d %s", code, r.Data)
	}
	p.MinRetentionDays = 14
	if code, r = call(t, root, "POST", api+"/policy/diff", p); code != 200 || !strings.Contains(string(r.Data), "最小保留天数") {
		t.Errorf("diff: %d %s", code, r.Data)
	}
	body := map[string]any{"min_retention_days": 14, "allow_purge_no_backup": false, "protected_patterns": p.ProtectedPatterns, "max_rows_per_run": p.MaxRowsPerRun,
		"max_concurrent_per_instance": 2, "defaults": p.Defaults}
	if code, _ = call(t, root, "PUT", api+"/policy", body); code != 422 {
		t.Errorf("缺确认应 422: %d", code)
	}
	body["confirm"] = true
	if code, r = call(t, root, "PUT", api+"/policy", body); code != 200 || !strings.Contains(string(r.Data), `"min_retention_days":14`) {
		t.Errorf("保存策略: %d %s", code, r.Data)
	}
}

func TestHTTPCSRF(t *testing.T) {
	h := newHTTPEnv(t)
	root := h.web.Login("root")
	root.CSRF = "wrong"
	w := root.Do("POST", api+"/preview", map[string]any{})
	if w.Code != 403 {
		t.Errorf("缺 CSRF 应 403: %d", w.Code)
	}
}
