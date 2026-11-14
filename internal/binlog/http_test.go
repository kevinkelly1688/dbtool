// @author: kevinkelly
// @license: Apache Licence
// @file: http_test.go
// @time: 2026/10/28

package binlog

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbtool/internal/btnperm/btnpermtest"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
	"dbtool/internal/settings"
)

type httpEnv struct {
	*servertest.Env
	chk *btnpermtest.Checker
	svc *Service
	au  *fakeAudit
	src *fakeSource
}

const (
	bobID  = 2
	rootID = 1
)

func newHTTP(t *testing.T) *httpEnv {
	t.Helper()
	h := &httpEnv{chk: btnpermtest.New()}
	h.src = newFakeSource(scenario(), t)
	h.Env = servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){h.chk.Hook,
		func(s *server.Server, rt server.Routes) {
			repo := newFakeRepo()
			h.au = &fakeAudit{}
			h.svc = NewService(repo, h.src, h.au)
			register(s, rt, h.svc)
		}}})
	return h
}

func (h *httpEnv) waitJob(t *testing.T, c *servertest.Client, id string) JobView {
	t.Helper()
	for i := 0; i < 400; i++ {
		var v JobView
		c.GetJSON("/api/v1/binlog/jobs/"+id, &v)
		if v.Status != StatusQueued && v.Status != StatusRunning {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("任务没有结束")
	return JobView{}
}

func TestHTTPAnonymous(t *testing.T) {
	h := newHTTP(t)
	for _, p := range []string{"/api/v1/binlog/options", "/api/v1/binlog/jobs", "/api/v1/binlog/jobs/x/download"} {
		if w := h.Anonymous("GET", p); w.Code != http.StatusUnauthorized && w.Code != http.StatusFound {
			t.Errorf("%s 未登录: %d", p, w.Code)
		}
	}
	if w := h.Anonymous("GET", "/binlog"); w.Code != http.StatusFound {
		t.Errorf("页面未登录应跳转登录: %d", w.Code)
	}
}

// 页面里的“最多条数（≤ N）”“结果体积上限 MB（≤ N）”和 /options 的上限，都是系统设置里当前生效的值。
func TestHTTPLimitsFollowSettings(t *testing.T) {
	h := newHTTP(t)
	h.chk.Grant(bobID, BtnView)
	bob := h.Login("bob")
	type opts struct {
		Limits map[string]int `json:"limits"`
	}
	check := func(wantRows, wantMB, wantDefRows, wantDefMB int) {
		t.Helper()
		code, body := bob.Page("/binlog")
		if code != 200 || !strings.Contains(body, `<span id="bl-hard-rows">`+strconv.Itoa(wantRows)+`</span>`) ||
			!strings.Contains(body, `<span id="bl-hard-mb">`+strconv.Itoa(wantMB)+`</span>`) {
			t.Errorf("页面应显示上限 %d / %d: %d", wantRows, wantMB, code)
		}
		var o opts
		bob.GetJSON("/api/v1/binlog/options", &o)
		if o.Limits["hard_rows"] != wantRows || o.Limits["hard_mb"] != wantMB || o.Limits["default_rows"] != wantDefRows || o.Limits["default_mb"] != wantDefMB {
			t.Errorf("options 的 limits = %+v，期望上限 %d / %d、默认 %d / %d", o.Limits, wantRows, wantMB, wantDefRows, wantDefMB)
		}
	}
	check(DefaultHardRows, DefaultHardMB, DefaultMaxRows, DefaultMaxMB) // 没设置：默认 50 万条 / 1024 MB
	repo := h.svc.repo.(*fakeRepo)
	repo.cfg = map[string]string{settings.KeyBinlogMaxRows: "1000", settings.KeyBinlogMaxResultMB: "4"}
	check(1000, 4, DefaultMaxRows, DefaultMaxMB)
	repo.cfg = map[string]string{settings.KeyBinlogMaxRows: "100", settings.KeyBinlogMaxResultMB: "1"} // 比页面默认值还小
	check(100, 1, 100, 1)
}

func TestHTTPNoButtons(t *testing.T) {
	h := newHTTP(t)
	bob := h.Login("bob")
	if code, _ := bob.Page("/binlog"); code != 403 {
		t.Errorf("没有 binlog.view 打不开页面: %d", code)
	}
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/v1/binlog/options"}, {"GET", "/api/v1/binlog/instances/1/info"}, {"GET", "/api/v1/binlog/instances/1/databases"},
		{"POST", "/api/v1/binlog/parse"}, {"GET", "/api/v1/binlog/jobs"}, {"GET", "/api/v1/binlog/jobs/x"},
		{"GET", "/api/v1/binlog/jobs/x/rows"}, {"POST", "/api/v1/binlog/jobs/x/cancel"}, {"GET", "/api/v1/binlog/jobs/x/download"},
	} {
		if code, _ := bob.JSON(c.m, c.p, map[string]any{"instance_id": 1, "start_file": "bin.000001"}, nil); code != 403 {
			t.Errorf("%s %s 没有任何按钮应是 403: %d", c.m, c.p, code)
		}
	}
	if len(h.au.actions()) != 0 {
		t.Errorf("被拒绝的请求不写审计: %v", h.au.actions())
	}
}

func TestHTTPViewOnly(t *testing.T) {
	h := newHTTP(t)
	h.chk.Grant(bobID, BtnView)
	bob := h.Login("bob")
	code, body := bob.Page("/binlog")
	if code != 200 || !strings.Contains(body, `id="bl"`) || !strings.Contains(body, "/static/js/binlog/binlog.js") {
		t.Errorf("页面: %d", code)
	}
	var opt struct {
		Instances []InstanceView  `json:"instances"`
		Perms     map[string]bool `json:"perms"`
		Limits    map[string]int  `json:"limits"`
	}
	bob.GetJSON("/api/v1/binlog/options", &opt)
	if len(opt.Instances) != 1 || opt.Instances[0].ID != 1 || opt.Perms["parse"] || opt.Perms["rollback"] || opt.Perms["download"] {
		t.Errorf("options: %+v", opt)
	}
	if opt.Limits["hard_rows"] != DefaultHardRows || opt.Limits["hard_mb"] != DefaultHardMB || opt.Limits["default_rows"] != DefaultMaxRows || opt.Limits["keep_minutes"] != 30 {
		t.Errorf("limits: %+v", opt.Limits)
	}
	var info ServerInfo
	bob.GetJSON("/api/v1/binlog/instances/1/info", &info)
	if len(info.Files) != 2 || !info.CanRollback {
		t.Errorf("info: %+v", info)
	}
	var dbs []string
	bob.GetJSON("/api/v1/binlog/instances/1/databases", &dbs)
	if strings.Join(dbs, ",") != "hidden,report,shop" {
		t.Errorf("databases: %v", dbs)
	}
	if code, _ := bob.JSON("GET", "/api/v1/binlog/instances/2/info", nil, nil); code != 403 {
		t.Errorf("不在资源组的实例: %d", code)
	}
	if code, _ := bob.JSON("GET", "/api/v1/binlog/instances/abc/info", nil, nil); code != 400 {
		t.Errorf("实例参数错误: %d", code)
	}
	if code, _ := bob.JSON("POST", "/api/v1/binlog/parse", Req{InstanceID: 1, StartFile: "bin.000001"}, nil); code != 403 {
		t.Errorf("只有查看权限不能解析: %d", code)
	}
	if code, _ := bob.JSON("GET", "/api/v1/binlog/jobs", nil, nil); code != 403 {
		t.Errorf("只有查看权限看不到任务列表: %d", code)
	}
}

func TestHTTPParseRollbackDownloadMatrix(t *testing.T) {
	h := newHTTP(t)
	h.chk.Grant(bobID, BtnView, BtnParse)
	bob := h.Login("bob")
	root := h.Login("root")

	// 解析：可以
	var v JobView
	bob.PostJSON("/api/v1/binlog/parse", Req{InstanceID: 1, StartFile: "bin.000001"}, &v)
	if v.ID == "" || v.Mode != ModeSQL || !v.Mine {
		t.Fatalf("%+v", v)
	}
	done := h.waitJob(t, bob, v.ID)
	if done.Status != StatusDone || done.Matched != 3 {
		t.Fatalf("%+v", done)
	}
	if done.CanDownload {
		t.Error("没有下载按钮时 can_download 应为 false")
	}
	var page RowsPage
	bob.GetJSON("/api/v1/binlog/jobs/"+v.ID+"/rows?offset=1&limit=1", &page)
	if page.Total != 3 || len(page.Rows) != 1 || page.Rows[0].Type != KindUpdate {
		t.Errorf("rows: %+v", page)
	}
	var list []JobView
	bob.GetJSON("/api/v1/binlog/jobs", &list)
	if len(list) != 1 || list[0].ID != v.ID {
		t.Errorf("jobs: %+v", list)
	}
	// 回滚：没有 binlog.rollback → 403；非法模式 400
	if code, msg := bob.JSON("POST", "/api/v1/binlog/parse", Req{InstanceID: 1, Mode: ModeRollback, StartFile: "bin.000001"}, nil); code != 403 || !strings.Contains(msg, "回滚") {
		t.Errorf("无回滚权限: %d %s", code, msg)
	}
	if code, _ := bob.JSON("POST", "/api/v1/binlog/parse", Req{InstanceID: 1, Mode: "x", StartFile: "bin.000001"}, nil); code != 400 {
		t.Errorf("非法模式: %d", code)
	}
	// 下载：没有 binlog.download → 403
	if code, _ := bob.JSON("GET", "/api/v1/binlog/jobs/"+v.ID+"/download", nil, nil); code != 403 {
		t.Errorf("无下载权限: %d", code)
	}
	if got := h.au.actions(); len(got) != 1 {
		t.Errorf("只有成功的解析写审计: %v", got)
	}

	// 授予回滚后可以回滚
	h.chk.Grant(bobID, BtnRollback)
	var rv JobView
	bob.PostJSON("/api/v1/binlog/parse", Req{InstanceID: 1, Mode: ModeRollback, StartFile: "bin.000001"}, &rv)
	if d := h.waitJob(t, bob, rv.ID); d.Status != StatusDone || d.Mode != ModeRollback {
		t.Fatalf("%+v", d)
	}
	// 回滚任务下载：需要 rollback + download
	if code, _ := bob.JSON("GET", "/api/v1/binlog/jobs/"+rv.ID+"/download", nil, nil); code != 403 {
		t.Errorf("回滚任务没有下载权限: %d", code)
	}
	h.chk.Grant(bobID, BtnDownload)
	w := bob.Do("GET", "/api/v1/binlog/jobs/"+rv.ID+"/download", nil)
	if w.Code != 200 {
		t.Fatalf("下载: %d %s", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="binlog_1_rollback_`) {
		t.Errorf("Content-Disposition: %s", cd)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("响应头: %v", w.Header())
	}
	if !strings.Contains(w.Body.String(), "DELETE FROM `shop`.`orders` WHERE `id` = 1;") {
		t.Errorf("内容: %s", w.Body.String())
	}
	var dv JobView
	bob.GetJSON("/api/v1/binlog/jobs/"+rv.ID, &dv)
	if !dv.CanDownload {
		t.Error("有按钮且任务完成时 can_download 应为 true")
	}
	// 收回回滚权限后，回滚任务的内容不能再看 / 下载
	h.chk.Revoke(bobID, BtnRollback)
	for _, p := range []string{"/api/v1/binlog/jobs/" + rv.ID, "/api/v1/binlog/jobs/" + rv.ID + "/rows", "/api/v1/binlog/jobs/" + rv.ID + "/download"} {
		if code, _ := bob.JSON("GET", p, nil, nil); code != 403 {
			t.Errorf("收回回滚权限后 %s: %d", p, code)
		}
	}
	bob.GetJSON("/api/v1/binlog/jobs", &list)
	if len(list) != 1 || list[0].Mode != ModeSQL {
		t.Errorf("列表里不显示回滚任务: %+v", list)
	}
	// 解析任务的下载不要求回滚权限
	if code, _ := bob.JSON("GET", "/api/v1/binlog/jobs/"+v.ID+"/download", nil, nil); code != 200 {
		t.Errorf("解析任务下载: %d", code)
	}
	acts := strings.Join(h.au.actions(), ",")
	if acts != "Binlog解析-解析,Binlog解析-回滚,Binlog解析-下载,Binlog解析-下载" {
		t.Errorf("审计动作: %s", acts)
	}

	// 超级用户：看得到别人的任务，可以下载
	var rl []JobView
	root.GetJSON("/api/v1/binlog/jobs", &rl)
	if len(rl) != 2 {
		t.Errorf("超级用户看全部: %d", len(rl))
	}
	if code, _ := root.JSON("GET", "/api/v1/binlog/jobs/"+v.ID+"/download", nil, nil); code != 200 {
		t.Errorf("超级用户下载: %d", code)
	}
}

func TestHTTPJobIsolationAndCancel(t *testing.T) {
	h := newHTTP(t)
	h.src.block = make(chan struct{})
	h.chk.Grant(bobID, BtnView, BtnParse)
	h.chk.Grant(3, BtnView, BtnParse)
	bob := h.Login("bob")
	root := h.Login("root")
	var v JobView
	bob.PostJSON("/api/v1/binlog/parse", Req{InstanceID: 1, StartFile: "bin.000001"}, &v)
	waitRunning(t, h.src, 1)

	// 另一个普通用户 (carol, id 3) 在 servertest 里不存在，用 root 验证“不是所有者”的行为已在 service 层覆盖；
	// 这里验证所有者与超级用户都能取消，重复取消 409
	if code, _ := bob.JSON("POST", "/api/v1/binlog/jobs/"+v.ID+"/cancel", nil, nil); code != 200 {
		t.Errorf("所有者取消: %d", code)
	}
	if code, _ := root.JSON("POST", "/api/v1/binlog/jobs/"+v.ID+"/cancel", nil, nil); code != 409 {
		t.Errorf("重复取消: %d", code)
	}
	if d := h.waitJob(t, bob, v.ID); d.Status != StatusCanceled {
		t.Errorf("%+v", d)
	}
	if code, _ := bob.JSON("GET", "/api/v1/binlog/jobs/not-exist", nil, nil); code != 404 {
		t.Errorf("不存在的任务: %d", code)
	}
	// 行与状态在取消后仍可查询（空结果）
	var page RowsPage
	bob.GetJSON("/api/v1/binlog/jobs/"+v.ID+"/rows", &page)
	if page.Total != 0 {
		t.Errorf("%+v", page)
	}
	// 非超级用户的 user 2 看不到 root 的任务
	var rv JobView
	root.PostJSON("/api/v1/binlog/parse", Req{InstanceID: 1, StartFile: "bin.000001"}, &rv)
	waitRunning(t, h.src, 2)
	if code, _ := bob.JSON("GET", "/api/v1/binlog/jobs/"+rv.ID, nil, nil); code != 404 {
		t.Errorf("别人的任务应是 404: %d", code)
	}
	close(h.src.block)
	h.waitJob(t, root, rv.ID)
}

func TestHTTPBadBodyAndErrors(t *testing.T) {
	h := newHTTP(t)
	h.chk.Grant(bobID, BtnView, BtnParse)
	bob := h.Login("bob")
	if w := bob.Do("POST", "/api/v1/binlog/parse", "{not json"); w.Code != 400 {
		t.Errorf("坏 JSON: %d", w.Code)
	}
	big := `{"instance_id":1,"start_file":"` + strings.Repeat("a", 70<<10) + `"}`
	if w := bob.Do("POST", "/api/v1/binlog/parse", big); w.Code != 400 {
		t.Errorf("过大的请求体: %d", w.Code)
	}
	if code, _ := bob.JSON("POST", "/api/v1/binlog/parse", Req{InstanceID: 1, StartFile: "nope"}, nil); code != 400 {
		t.Errorf("文件不存在: %d", code)
	}
	h.src.connectErr = http.ErrAbortHandler
	if code, _ := bob.JSON("POST", "/api/v1/binlog/parse", Req{InstanceID: 1, StartFile: "bin.000001"}, nil); code != 502 {
		t.Errorf("连不上实例: %d", code)
	}
}
