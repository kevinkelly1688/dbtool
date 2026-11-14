// @author: kevinkelly
// @license: Apache Licence
// @file: http_test.go
// @time: 2026/10/28

package perf

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// newHTTP 搭一个带性能工单路由的测试服务：servertest 的 root(1) 是超级用户，bob(2) 对应夹具里的“张三”（订单研发，普通研发）。
func newHTTP(t *testing.T) (*servertest.Env, *fx) {
	t.Helper()
	f := newFx(t)
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){
		func(s *server.Server, rt server.Routes) { registerWith(s, rt, f.svc, f.repo) },
	}})
	return env, f
}

func grant(env *servertest.Env, uid uint, codes ...string) {
	if env.Repo.Perms[uid] == nil {
		env.Repo.Perms[uid] = map[string]bool{}
	}
	for _, c := range codes {
		env.Repo.Perms[uid][c] = true
	}
}

// grantDev 授予普通研发的按钮：查看、受理 / 申请类处理、评论（对应原来的“查看与处理自己的工单”）。
func grantDev(env *servertest.Env, uid uint) { grant(env, uid, btnView, "perf.handle", "perf.comment") }

// grantDBA 授予 DBA 侧的全部按钮（查看 + 管理类），相当于原来的“性能工单管理”角色。
func grantDBA(env *servertest.Env, uid uint) {
	grant(env, uid, append([]string{btnView}, managerButtons...)...)
}

type listResp struct {
	Data    []IssueDTO `json:"data"`
	Total   int        `json:"total"`
	Summary struct {
		Todo    int `json:"todo"`
		Overdue int `json:"overdue"`
	} `json:"summary"`
}

func getList(t *testing.T, c *servertest.Client, query string) listResp {
	t.Helper()
	w := c.Do("GET", "/api/v1/perf/issues"+query, nil)
	if w.Code != 200 {
		t.Fatalf("GET issues%s = %d %s", query, w.Code, w.Body.String())
	}
	var r listResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHTTPAuth(t *testing.T) {
	env, f := newHTTP(t)
	id := f.newIssue()
	_ = id
	// 未登录：页面跳转登录，接口 401
	if w := env.Anonymous("GET", "/perf"); w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Location"), "/login") {
		t.Fatalf("未登录页面: %d %s", w.Code, w.Header().Get("Location"))
	}
	for _, p := range []string{"/api/v1/perf/issues", "/api/v1/perf/my-todo", "/api/v1/perf/board", "/api/v1/perf/config/sla"} {
		if w := env.Anonymous("GET", p); w.Code != 401 {
			t.Fatalf("%s 未登录应 401: %d", p, w.Code)
		}
	}
	// 登录但没有任何权限：403
	bob := env.Login("bob")
	for _, p := range []string{"/perf", "/perf/board", "/perf/1", "/perf/config/collect", "/perf/config/team", "/perf/config/sla"} {
		if code, _ := bob.Page(p); code != 403 {
			t.Fatalf("%s 无权限应 403: %d", p, code)
		}
	}
	for _, p := range []string{"/api/v1/perf/issues", "/api/v1/perf/issues/1", "/api/v1/perf/my-todo", "/api/v1/perf/options", "/api/v1/perf/board", "/api/v1/perf/config/collect"} {
		if w := bob.Do("GET", p, nil); w.Code != 403 {
			t.Fatalf("%s 无权限应 403: %d", p, w.Code)
		}
	}
	if w := bob.Do("POST", "/api/v1/perf/issues/1/actions", map[string]any{"action": "comment", "remark": "x"}); w.Code != 403 {
		t.Fatalf("无权限不能操作: %d", w.Code)
	}
}

func TestHTTPViewButtonIsEnoughButConfigAndWritesNeedTheirButtons(t *testing.T) {
	env, f := newHTTP(t)
	f.newIssue()
	grant(env, 2, btnView)
	bob := env.Login("bob")
	for _, p := range []string{"/perf", "/perf/board", "/perf/1"} {
		if code, _ := bob.Page(p); code != 200 {
			t.Fatalf("%s 有 perf.view 应 200: %d", p, code)
		}
	}
	for _, p := range []string{"/perf/config/collect", "/perf/config/team", "/perf/config/sla"} {
		if code, _ := bob.Page(p); code != 403 {
			t.Fatalf("%s 配置页需要 perfcfg.view: %d", p, code)
		}
	}
	for _, p := range []string{"/api/v1/perf/config/collect", "/api/v1/perf/config/team", "/api/v1/perf/config/sla"} {
		if w := bob.Do("GET", p, nil); w.Code != 403 {
			t.Fatalf("%s 配置接口需要 perfcfg.view: %d", p, w.Code)
		}
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/v1/perf/issues"}, {"POST", "/api/v1/perf/issues/from-slowlog"}, {"POST", "/api/v1/perf/issues/1/diagnose"},
		{"POST", "/api/v1/perf/config/collect"}, {"PUT", "/api/v1/perf/config/collect/1"}, {"DELETE", "/api/v1/perf/config/collect/1"},
		{"POST", "/api/v1/perf/config/collect/preview"}, {"POST", "/api/v1/perf/config/collect/init"}, {"POST", "/api/v1/perf/config/teams"},
		{"DELETE", "/api/v1/perf/config/teams/1"}, {"POST", "/api/v1/perf/config/routes"}, {"DELETE", "/api/v1/perf/config/routes/1"}, {"PUT", "/api/v1/perf/config/sla"},
	} {
		if w := bob.Do(c.method, c.path, map[string]any{}); w.Code != 403 {
			t.Fatalf("%s %s 需要对应的管理按钮: %d", c.method, c.path, w.Code)
		}
	}
}

// 管理类按钮（DBA 侧）本身就能打开工单页面、看到全部工单；配置页面仍只认 perfcfg.view。
func TestHTTPManagerButtonOpensPagesAndSeesAll(t *testing.T) {
	env, f := newHTTP(t)
	f.newIssue()
	grant(env, 2, "perf.assign") // 只有一个管理类按钮，没有 perf.view
	bob := env.Login("bob")
	for _, p := range []string{"/perf", "/perf/board", "/perf/1"} {
		if code, _ := bob.Page(p); code != 200 {
			t.Fatalf("%s 持有管理类按钮应能打开: %d", p, code)
		}
	}
	if code, _ := bob.Page("/perf/config/sla"); code != 403 {
		t.Fatal("没有 perfcfg.view 不能打开配置页")
	}
	var d IssueDetail
	bob.GetJSON("/api/v1/perf/issues/1", &d)
	if !d.IsDBA {
		t.Fatalf("持有管理类按钮应是 DBA（看到全部工单）: %+v", d)
	}
	grant(env, 2, "perfcfg.view")
	if code, _ := bob.Page("/perf/config/sla"); code != 200 {
		t.Fatal("perfcfg.view 应能打开配置页")
	}
}

func TestHTTPPagesRender(t *testing.T) {
	env, f := newHTTP(t)
	f.newIssue()
	root := env.Login("root")
	pages := map[string][]string{
		"/perf":                {"问题工单", "我的待办", "/static/js/perf/list.js", "/static/css/perf.css", "手工建单", "/perf/board"},
		"/perf/board":          {"团队榜单", "团队健康度", "/static/js/perf/board.js", "近 12 周"},
		"/perf/1":              {"性能工单 #1", `data-id="1"`, "/static/js/perf/detail.js", "处理记录", "m-perf-act"},
		"/perf/config/collect": {"采集配置", "试采集", "/static/js/perf/collect.js", "m-collect"},
		"/perf/config/team":    {"团队配置", "路由规则", "/static/js/perf/team.js", "m-team", "m-route"},
		"/perf/config/sla":     {"SLA 策略", "级别评估规则", "/static/js/perf/sla.js"},
	}
	for path, wants := range pages {
		code, body := root.Page(path)
		if code != 200 {
			t.Fatalf("%s = %d", path, code)
		}
		for _, w := range wants {
			if !strings.Contains(body, w) {
				t.Errorf("%s 缺少 %q", path, w)
			}
		}
		if !strings.Contains(body, "/static/js/perf/todo.js") {
			t.Errorf("%s 应引入待办角标脚本", path)
		}
		if strings.Contains(body, "onclick=") || strings.Contains(body, "<script>") {
			t.Errorf("%s 不能有内联脚本（CSP）", path)
		}
	}
	// 页面不存在的编号
	if code, _ := root.Page("/perf/abc"); code != 404 {
		t.Fatalf("非法编号: %d", code)
	}
	// 普通研发看不到“手工建单”和配置入口
	grantDev(env, 2)
	bob := env.Login("bob")
	_, body := bob.Page("/perf")
	if strings.Contains(body, "手工建单") || strings.Contains(body, "采集配置") {
		t.Error("普通研发不应看到 DBA 入口")
	}
	if !strings.Contains(body, "问题工单") || !strings.Contains(body, "团队榜单") {
		t.Error("应有侧栏入口")
	}
}

func TestHTTPMenu(t *testing.T) {
	env, f := newHTTP(t)
	f.newIssue()
	root := env.Login("root")
	_, body := root.Page("/perf")
	for _, label := range []string{"性能治理", "问题工单", "团队榜单", "采集配置", "团队配置", "SLA 策略"} {
		if !strings.Contains(body, label) {
			t.Errorf("超级用户的菜单缺少 %q", label)
		}
	}
	for _, href := range []string{`href="/perf"`, `href="/perf/board"`, `href="/perf/config/collect"`, `href="/perf/config/team"`, `href="/perf/config/sla"`} {
		if !strings.Contains(body, href) {
			t.Errorf("菜单缺少 %s", href)
		}
	}
	// 仅有 perf.view：能看到列表与排行榜，看不到配置类
	grantDev(env, 2)
	_, body = env.Login("bob").Page("/perf/board")
	topbar := body[:strings.Index(body, "</header>")]
	if !strings.Contains(topbar, `href="/perf/board"`) || strings.Contains(topbar, "/perf/config/") {
		t.Errorf("研发的菜单: %s", topbar)
	}
	// 没有任何权限的人看不到“性能治理”分组
	_, body = env.Login("bob").Page("/query")
	_ = body
	delete(env.Repo.Perms, 2)
	_, body = env.Login("bob").Page("/query")
	if strings.Contains(body[:strings.Index(body, "</header>")], "性能治理") {
		t.Error("没有权限不应出现菜单分组")
	}
}

func TestHTTPListDetailAndActions(t *testing.T) {
	env, f := newHTTP(t)
	id := f.newIssue()
	grantDev(env, 2)
	bob := env.Login("bob")

	// 列表
	r := getList(t, bob, "?view=todo")
	if r.Total != 1 || len(r.Data) != 1 || r.Data[0].ID != id || r.Summary.Todo != 1 || r.Data[0].Instance != "order-db" {
		t.Fatalf("列表: %+v", r)
	}
	if w := bob.Do("GET", "/api/v1/perf/issues?view=bogus", nil); w.Code != 400 {
		t.Fatalf("非法视图: %d", w.Code)
	}
	if r = getList(t, bob, "?view=all&search=t_order&sort=avg_time_ms&order=asc&limit=5&offset=0"); r.Total != 1 {
		t.Fatalf("搜索: %+v", r)
	}

	// 详情
	var d IssueDetail
	bob.GetJSON("/api/v1/perf/issues/1", &d)
	if d.ID != id || d.SampleSQL != sqlOrderByUser || len(d.Actions) == 0 || d.Actions[0].Name != "accept" || len(d.Logs) != 1 || d.IsDBA {
		t.Fatalf("详情: %+v", d)
	}
	if code, _ := bob.JSON("GET", "/api/v1/perf/issues/999", nil, nil); code != 404 {
		t.Fatalf("不存在: %d", code)
	}
	if code, _ := bob.JSON("GET", "/api/v1/perf/issues/abc", nil, nil); code != 400 {
		t.Fatalf("非法编号: %d", code)
	}

	// 趋势
	var tr Trend
	bob.GetJSON("/api/v1/perf/issues/1/trend?days=30", &tr)
	if len(tr.Points) != 1 {
		t.Fatalf("趋势: %+v", tr)
	}

	// CSRF：令牌不对时拒绝
	good := bob.CSRF
	bob.CSRF = "wrong"
	if code, _ := bob.JSON("POST", "/api/v1/perf/issues/1/actions", map[string]any{"action": "comment", "remark": "x"}, nil); code != 403 {
		t.Fatalf("CSRF 应拒绝: %d", code)
	}
	bob.CSRF = good

	// 受理：校验失败 → 400 带中文信息；成功 → 状态变化
	code, m := bob.JSON("POST", "/api/v1/perf/issues/1/actions", map[string]any{"action": "accept"}, nil)
	if code != 400 || !strings.Contains(m, "承诺完成日期") {
		t.Fatalf("缺少日期: %d %s", code, m)
	}
	var res ActionResult
	bob.PostJSON("/api/v1/perf/issues/1/actions", map[string]any{"action": "accept", "plan_finish_date": f.now.Format(dateLayout)}, &res)
	if res.Status != StAccepted || f.issue(id).Status != StAccepted {
		t.Fatalf("受理: %+v", res)
	}
	// 非法流转：409
	code, m = bob.JSON("POST", "/api/v1/perf/issues/1/actions", map[string]any{"action": "accept", "plan_finish_date": f.now.Format(dateLayout)}, nil)
	if code != 409 || !strings.Contains(m, "不能") {
		t.Fatalf("重复受理: %d %s", code, m)
	}
	// 研发做 DBA 动作：403
	if code, _ = bob.JSON("POST", "/api/v1/perf/issues/1/actions", map[string]any{"action": "verify_pass"}, nil); code != 403 {
		t.Fatalf("越权: %d", code)
	}
	// 坏请求体
	if w := bob.Do("POST", "/api/v1/perf/issues/1/actions", "{bad"); w.Code != 400 {
		t.Fatalf("坏 JSON: %d", w.Code)
	}
	// 看不到别的团队的工单：按不存在处理
	f.repo.issues[id].TeamID, f.repo.issues[id].AssigneeID = ptr(uint(11)), ptr(uint(uDev2))
	if code, _ = bob.JSON("GET", "/api/v1/perf/issues/1", nil, nil); code != 404 {
		t.Fatalf("别的团队: %d", code)
	}
	if r = getList(t, bob, "?view=all"); r.Total != 0 {
		t.Fatalf("列表不应包含别的团队: %+v", r)
	}
}

func TestHTTPMyTodoAndOptions(t *testing.T) {
	env, f := newHTTP(t)
	f.newIssue()
	grantDev(env, 2)
	bob := env.Login("bob")
	var td MyTodo
	bob.GetJSON("/api/v1/perf/my-todo", &td)
	if td.Todo != 1 || td.Overdue != 0 {
		t.Fatalf("%+v", td)
	}
	f.now = f.now.Add(49 * time.Hour)
	bob.GetJSON("/api/v1/perf/my-todo", &td)
	if td.Overdue != 1 {
		t.Fatalf("%+v", td)
	}
	var o OptionsDTO
	bob.GetJSON("/api/v1/perf/options", &o)
	if o.IsDBA || len(o.Users) != 0 || len(o.Instances) != 0 || len(o.Severities) != 4 || len(o.Categories) != 10 || len(o.Teams) != 1 || o.Teams[0].Name != "订单研发" || len(o.Statuses) != 6 {
		t.Fatalf("研发的选项: %+v", o)
	}
	root := env.Login("root")
	root.GetJSON("/api/v1/perf/options", &o)
	if !o.IsDBA || len(o.Users) == 0 || len(o.Instances) != 2 || len(o.Teams) != 2 {
		t.Fatalf("DBA 的选项: %+v", o)
	}
}

func TestHTTPCreateByDBA(t *testing.T) {
	env, f := newHTTP(t)
	grantDBA(env, 2)
	f.repo.ugroups[2] = []uint{10}
	bob := env.Login("bob")
	body := map[string]any{"instance_id": 1, "db_name": "order", "sql": "select * from t_order where a = 1", "remark": "测试"}
	var out struct {
		ID       uint   `json:"id"`
		Severity string `json:"severity"`
	}
	bob.PostJSON("/api/v1/perf/issues", body, &out)
	if out.ID == 0 || out.Severity != SevP2 {
		t.Fatalf("%+v", out)
	}
	// 重复建单 → 409 exists，并带上已有工单号
	w := bob.Do("POST", "/api/v1/perf/issues", body)
	var e struct {
		Code string `json:"code"`
		Data struct {
			ID uint `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != 409 || e.Code != "exists" || e.Data.ID != out.ID {
		t.Fatalf("重复建单: %d %s", w.Code, w.Body.String())
	}
	if w = bob.Do("POST", "/api/v1/perf/issues", map[string]any{"instance_id": 1}); w.Code != 400 {
		t.Fatalf("缺 SQL: %d", w.Code)
	}
	// 慢日志页面生成
	f.repo.slowMD5["1|md5x"] = &SlowSample{DBName: "order", SQLText: "select * from t_slow where a = 1"}
	var sl struct {
		ID uint `json:"id"`
	}
	bob.PostJSON("/api/v1/perf/issues/from-slowlog", map[string]any{"instance_id": 1, "sql_id": "md5x"}, &sl)
	if sl.ID == 0 || f.issue(sl.ID).Source != "slowlog" {
		t.Fatalf("%+v", sl)
	}
	// 重新诊断
	var dr DiagResult
	bob.PostJSON("/api/v1/perf/issues/"+itoaU(out.ID)+"/diagnose", map[string]any{"sql": "select * from t where a like '%x'"}, &dr)
	if dr.Category != CatIndexInvalid {
		t.Fatalf("%+v", dr)
	}
	if w = bob.Do("POST", "/api/v1/perf/issues/"+itoaU(out.ID)+"/diagnose", nil); w.Code != 200 {
		t.Fatalf("不带 body 也可以重新诊断: %d %s", w.Code, w.Body.String())
	}
}

func TestHTTPBoard(t *testing.T) {
	env, f := newHTTP(t)
	f.newIssue()
	grantDev(env, 2)
	bob := env.Login("bob")
	var b BoardResult
	bob.GetJSON("/api/v1/perf/board?days=30&owner_side=dev", &b)
	if b.Days != 30 || len(b.Teams) != 1 || b.Teams[0].Open != 1 || b.Overall.Open != 1 {
		t.Fatalf("%+v", b)
	}
	if code, _ := bob.JSON("GET", "/api/v1/perf/board?owner_side=bogus", nil, nil); code != 400 {
		t.Fatalf("非法处理方: %d", code)
	}
}

func TestHTTPConfigCRUD(t *testing.T) {
	env, f := newHTTP(t)
	root := env.Login("root")

	// 采集配置
	var co CollectOverview
	root.GetJSON("/api/v1/perf/config/collect", &co)
	if len(co.Rows) != 1 || len(co.Instances) != 2 || len(co.Collectors) != 8 || len(co.Products) != 8 {
		t.Fatalf("%+v", co)
	}
	in := map[string]any{"instance_id": 2, "product": ProdSelectDB, "collector": CollDorisAudit, "min_avg_ms": 3000, "min_total_sec": 30, "enabled": true}
	var saved struct {
		ID uint `json:"id"`
	}
	root.PostJSON("/api/v1/perf/config/collect", in, &saved)
	if saved.ID == 0 {
		t.Fatal("应返回 id")
	}
	if code, _ := root.JSON("POST", "/api/v1/perf/config/collect", in, nil); code != 409 {
		t.Fatalf("同一实例重复: %d", code)
	}
	in["min_avg_ms"] = 5000
	root.JSON("PUT", "/api/v1/perf/config/collect/"+itoaU(saved.ID), in, nil)
	if c, _ := f.repo.GetCollectConfig(saved.ID); c.MinAvgMs != 5000 {
		t.Fatalf("修改: %+v", c)
	}
	if code, _ := root.JSON("PUT", "/api/v1/perf/config/collect/abc", in, nil); code != 400 {
		t.Fatalf("非法 id: %d", code)
	}
	var ir InitResult
	root.PostJSON("/api/v1/perf/config/collect/init", nil, &ir)
	if ir.Created != 0 || ir.Skipped != 2 {
		t.Fatalf("%+v", ir)
	}
	// 试采集：没有该采集方式的实现时给出可读错误
	col := &fakeCollector{digests: []Digest{f.digest(sqlOrderByUser, 100, 400)}}
	f.useCollector(col)
	var pv PreviewResult
	root.PostJSON("/api/v1/perf/config/collect/preview", map[string]any{"instance_id": 1, "product": ProdMySQL, "collector": CollPlatformSlow, "min_avg_ms": 1000, "min_total_sec": 10}, &pv)
	if pv.Total != 1 || pv.Matched != 1 || len(f.repo.issues) != 0 {
		t.Fatalf("试采集: %+v", pv)
	}
	if code, _ := root.JSON("DELETE", "/api/v1/perf/config/collect/"+itoaU(saved.ID), nil, nil); code != 200 {
		t.Fatalf("删除: %d", code)
	}
	if code, _ := root.JSON("DELETE", "/api/v1/perf/config/collect/"+itoaU(saved.ID), nil, nil); code != 404 {
		t.Fatalf("重复删除: %d", code)
	}

	// 团队与路由
	var to TeamOverview
	root.GetJSON("/api/v1/perf/config/team", &to)
	if len(to.Teams) != 1 || to.Teams[0].Team != "订单研发" || len(to.Groups) != 2 {
		t.Fatalf("%+v", to)
	}
	root.PostJSON("/api/v1/perf/config/teams", map[string]any{"resource_group_id": 11, "default_assignee_id": uDev2, "leader_ids": []uint{uTL}, "enabled": true}, &saved)
	if code, _ := root.JSON("POST", "/api/v1/perf/config/teams", map[string]any{"resource_group_id": 11}, nil); code != 409 {
		t.Fatalf("团队重复: %d", code)
	}
	root.JSON("PUT", "/api/v1/perf/config/teams/"+itoaU(saved.ID), map[string]any{"resource_group_id": 11, "enabled": false}, nil)
	if tc, _ := f.repo.GetTeam(saved.ID); tc.Enabled {
		t.Fatal("修改团队")
	}
	if code, _ := root.JSON("DELETE", "/api/v1/perf/config/teams/"+itoaU(saved.ID), nil, nil); code != 200 {
		t.Fatalf("删除团队: %d", code)
	}
	root.PostJSON("/api/v1/perf/config/routes", map[string]any{"db_regex": "pay_.*", "team_id": 10, "priority": 10}, &saved)
	if code, m := root.JSON("POST", "/api/v1/perf/config/routes", map[string]any{"db_regex": "(", "team_id": 10}, nil); code != 400 || !strings.Contains(m, "正则不合法") {
		t.Fatalf("非法正则: %d %s", code, m)
	}
	if code, _ := root.JSON("DELETE", "/api/v1/perf/config/routes/"+itoaU(saved.ID), nil, nil); code != 200 {
		t.Fatalf("删除路由: %d", code)
	}

	// SLA
	var so SLAOverview
	root.GetJSON("/api/v1/perf/config/sla", &so)
	if len(so.Policies) != 4 || so.Rules[SevP0].DailySec != 3600 {
		t.Fatalf("%+v", so)
	}
	so.Policies[0].AcceptHours = 3
	if code, _ := root.JSON("PUT", "/api/v1/perf/config/sla", map[string]any{"policies": so.Policies}, nil); code != 200 || f.svc.policy(SevP0).AcceptHours != 3 {
		t.Fatal("保存 SLA")
	}
	so.Policies[0].AcceptHours = 0
	if code, m := root.JSON("PUT", "/api/v1/perf/config/sla", map[string]any{"policies": so.Policies}, nil); code != 400 || !strings.Contains(m, "受理时限") {
		t.Fatalf("非法 SLA: %d %s", code, m)
	}
	// 所有配置变更都写了审计
	if len(f.repo.audits) < 8 {
		t.Fatalf("配置变更应写审计: %v", f.repo.auditActions())
	}
	for _, a := range f.repo.audits {
		if a.UserName != "root" || !strings.HasPrefix(a.Action, "性能工单配置-") {
			t.Fatalf("审计: %+v", a)
		}
	}
}

func itoaU(n uint) string { return strconv.FormatUint(uint64(n), 10) }
