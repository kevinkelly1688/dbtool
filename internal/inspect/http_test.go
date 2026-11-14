// @author: kevinkelly
// @license: Apache Licence
// @file: http_test.go
// @time: 2026/10/28

package inspect

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// 普通用户 bob 只能看到实例 1（相当于只属于一个资源组）。
func testHooks() hooks {
	return hooks{
		scope: func(s *server.Server, c *gin.Context) (Scope, error) {
			if server.PrincipalOf(c).IsSuperuser {
				return Scope{All: true}, nil
			}
			return Scope{IDs: map[uint]bool{1: true}}, nil
		},
	}
}

func newHTTP(t *testing.T) (*env, *servertest.Env) {
	t.Helper()
	e := newEnv()
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){registerWith(e.svc, testHooks())}})
	env.Repo.Perms[2] = map[string]bool{}
	return e, env
}

func grant(env *servertest.Env, uid uint, codes ...string) {
	if env.Repo.Perms[uid] == nil {
		env.Repo.Perms[uid] = map[string]bool{}
	}
	for _, c := range codes {
		env.Repo.Perms[uid][c] = true
	}
}

type listResp struct {
	Data  []reportRow `json:"data"`
	Total int         `json:"total"`
}

func getList(t *testing.T, c *servertest.Client, path string) listResp {
	t.Helper()
	w := c.Do("GET", path, nil)
	if w.Code != 200 {
		t.Fatalf("GET %s = %d %s", path, w.Code, w.Body.String())
	}
	var out listResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func status(c *servertest.Client, method, path string, body any) (int, string) {
	return c.JSON(method, path, body, nil)
}

func TestHTTPAuth(t *testing.T) {
	_, env := newHTTP(t)
	for _, p := range []string{"/api/v1/inspection/options", "/api/v1/inspection/reports", "/api/v1/inspection/plans", "/api/v1/inspection/reports/mysql-1"} {
		if w := env.Anonymous("GET", p); w.Code != 401 {
			t.Errorf("未登录 GET %s = %d，期望 401", p, w.Code)
		}
	}
	for _, p := range []string{"/api/v1/inspection/reports/run", "/api/v1/inspection/plans", "/api/v1/inspection/reports/cancel"} {
		if w := env.Anonymous("POST", p); w.Code != 401 {
			t.Errorf("未登录 POST %s = %d，期望 401", p, w.Code)
		}
	}
	for _, p := range []string{"/inspection", "/inspection/mysql-1"} {
		w := env.Anonymous("GET", p)
		if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
			t.Errorf("未登录访问页面 %s 应跳转登录：%d %s", p, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestHTTPPermissions(t *testing.T) {
	e, env := newHTTP(t)
	bob := env.Login("bob")
	// 没有 menu_mssqlinspection：页面与接口都是 403
	for _, p := range []string{"/inspection", "/inspection/mysql-1"} {
		if code, body := bob.Page(p); code != 403 {
			t.Errorf("无权限页面 %s = %d %s", p, code, truncate(body, 80))
		}
	}
	for _, p := range []string{"/api/v1/inspection/options", "/api/v1/inspection/reports", "/api/v1/inspection/plans"} {
		if code, _ := status(bob, "GET", p, nil); code != 403 {
			t.Errorf("无权限 GET %s = %d", p, code)
		}
	}
	// 顶栏菜单：没有权限看不到入口
	if _, html := bob.Page("/query"); strings.Contains(html, `href="/inspection"`) {
		t.Error("无权限用户不应看到“性能巡检”入口")
	}
	grant(env, 2, "inspect.view")
	if _, html := bob.Page("/query"); !strings.Contains(html, `href="/inspection"`) || !strings.Contains(html, "性能巡检") {
		t.Error("有权限的用户应看到入口")
	}
	root := env.Login("root")
	if _, html := root.Page("/query"); !strings.Contains(html, `href="/inspection"`) {
		t.Error("超级用户应看到入口")
	}

	// 有菜单权限：能看，不能操作
	var opts struct {
		Instances []InstanceOption `json:"instances"`
		CanRun    bool             `json:"can_run"`
		CanCancel bool             `json:"can_cancel"`
		CanPlan   bool             `json:"can_plan"`
	}
	bob.GetJSON("/api/v1/inspection/options", &opts)
	if opts.CanRun || opts.CanCancel || opts.CanPlan || len(opts.Instances) != 1 || opts.Instances[0].ID != 1 {
		t.Errorf("bob 的选项：%+v", opts)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/v1/inspection/reports/run", map[string]any{"instance_ids": []int{1}}},
		{"POST", "/api/v1/inspection/reports/delete", map[string]any{"ids": []string{"mysql-1"}}},
		{"POST", "/api/v1/inspection/reports/cancel", map[string]any{"ids": []string{"mysql-1"}}},
		{"POST", "/api/v1/inspection/plans", map[string]any{}},
		{"POST", "/api/v1/inspection/plans/preview", map[string]any{}},
		{"PUT", "/api/v1/inspection/plans/1", map[string]any{}},
		{"DELETE", "/api/v1/inspection/plans/1", nil},
		{"POST", "/api/v1/inspection/plans/1/enable", map[string]any{"enabled": true}},
		{"POST", "/api/v1/inspection/plans/1/run", nil},
	} {
		if code, _ := status(bob, c.method, c.path, c.body); code != 403 {
			t.Errorf("bob %s %s = %d，期望 403", c.method, c.path, code)
		}
	}
	if len(e.repo.reports) != 0 || len(e.repo.plans) != 0 || len(e.repo.audits) != 0 {
		t.Error("被拒绝的操作不应有任何副作用")
	}
	// is_staff 不是授权依据：自助注册的用户都带 is_staff，取消超级用户也不会清掉它；既不能取消，也不能触发 / 删除
	env.Repo.Users["carol"] = &model.User{ID: 3, Username: "carol", Password: env.Repo.Users["bob"].Password, IsActive: true, IsStaff: true}
	grant(env, 3, "inspect.view")
	carol := env.Login("carol")
	carol.GetJSON("/api/v1/inspection/options", &opts)
	if opts.CanCancel || opts.CanRun {
		t.Errorf("staff 的选项：%+v", opts)
	}
	if code, _ := status(carol, "POST", "/api/v1/inspection/reports/run", map[string]any{"instance_ids": []int{1}}); code != 403 {
		t.Errorf("staff 不能触发巡检：%d", code)
	}
	if code, msg := status(carol, "POST", "/api/v1/inspection/reports/cancel", map[string]any{"ids": []string{"mysql-9"}}); code != 403 {
		t.Errorf("staff 没有 inspect.cancel 不能取消：%d %s", code, msg)
	}
	// 计划管理权限码
	grant(env, 2, "inspect.plan_edit")
	bob.GetJSON("/api/v1/inspection/options", &opts)
	if !opts.CanPlan || opts.CanRun {
		t.Errorf("授予 %s 后：%+v", "inspect.plan_edit", opts)
	}
}

func TestHTTPCSRF(t *testing.T) {
	e, env := newHTTP(t)
	root := env.Login("root")
	root.CSRF = "不对的令牌"
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/v1/inspection/reports/run", map[string]any{"instance_ids": []int{1}}},
		{"POST", "/api/v1/inspection/plans", map[string]any{"name": "x"}},
		{"DELETE", "/api/v1/inspection/plans/1", nil},
	} {
		w := root.Do(c.method, c.path, c.body)
		if w.Code != 403 || !strings.Contains(w.Body.String(), `"csrf"`) {
			t.Errorf("%s %s 令牌不对应拒绝：%d %s", c.method, c.path, w.Code, w.Body.String())
		}
	}
	if len(e.repo.reports) != 0 || len(e.repo.plans) != 0 {
		t.Error("CSRF 校验失败不应有副作用")
	}
}

func TestHTTPPages(t *testing.T) {
	_, env := newHTTP(t)
	root := env.Login("root")
	code, html := root.Page("/inspection")
	if code != 200 || !strings.Contains(html, "性能巡检") || !strings.Contains(html, "/static/js/inspect/list.js") || !strings.Contains(html, `id="f-instance"`) ||
		!strings.Contains(html, "定时巡检") || !strings.Contains(html, "新建定时巡检计划") || !strings.Contains(html, "/static/css/inspect.css") {
		t.Errorf("列表页：%d %s", code, truncate(html, 200))
	}
	if strings.Contains(html, "onclick=") || strings.Contains(html, "onsubmit=") || strings.Contains(html, "<script>") {
		t.Error("CSP 禁止内联脚本和内联事件")
	}
	code, html = root.Page("/inspection/mssql-12")
	if code != 200 || !strings.Contains(html, `data-ref="mssql-12"`) || !strings.Contains(html, "/static/js/inspect/detail.js") {
		t.Errorf("详情页：%d %s", code, truncate(html, 200))
	}
	if code, _ := root.Page("/inspection/12"); code != 404 {
		t.Errorf("不合法的报告标识应 404：%d", code)
	}
	if code, _ := root.Page("/inspection/mysql-x"); code != 404 {
		t.Errorf("%d", code)
	}
	// 静态资源随二进制发布
	for _, p := range []string{"/static/js/inspect/list.js", "/static/js/inspect/detail.js", "/static/js/inspect/util.js", "/static/css/inspect.css"} {
		if w := env.Anonymous("GET", p); w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("静态资源 %s = %d", p, w.Code)
		}
	}
}

func TestHTTPRunListDetailDelete(t *testing.T) {
	e, env := newHTTP(t)
	root := env.Login("root")
	var run RunNowResult
	root.PostJSON("/api/v1/inspection/reports/run", map[string]any{"instance_ids": []int{1, 3, 4}}, &run)
	if len(run.Created) != 2 || len(run.Unsupported) != 1 {
		t.Fatalf("%+v", run)
	}
	e.waitIdle()

	l := getList(t, root, "/api/v1/inspection/reports")
	if l.Total != 2 || len(l.Data) != 2 {
		t.Fatalf("%+v", l)
	}
	byRef := map[string]reportRow{}
	for _, r := range l.Data {
		byRef[r.Ref] = r
	}
	my := byRef["mysql-1"]
	if my.TypeLabel != "MySQL" || my.Status != StatusFinish || my.HealthScore == nil || my.Operator != "root" || my.InstanceName != "prod-mysql-01" {
		t.Errorf("MySQL 行：%+v", my)
	}
	if ms := byRef["mssql-1"]; ms.TypeLabel != "SQL Server" || ms.HealthScore != nil || ms.Normal == 0 {
		t.Errorf("SQL Server 行：%+v", ms)
	}
	if got := getList(t, root, "/api/v1/inspection/reports?level=alert&trigger=manual&instance_id=3&limit=5&offset=0"); got.Total != 1 || got.Data[0].Ref != "mssql-1" {
		t.Errorf("筛选：%+v", got)
	}
	if got := getList(t, root, "/api/v1/inspection/reports?start_date=2026-10-06&end_date=2026-10-06"); got.Total != 2 {
		t.Errorf("日期筛选（含结束当天）：%d", got.Total)
	}
	if got := getList(t, root, "/api/v1/inspection/reports?end_date=2026-10-05"); got.Total != 0 {
		t.Errorf("日期筛选：%d", got.Total)
	}
	if got := getList(t, root, "/api/v1/inspection/reports?search=mssql"); got.Total != 1 {
		t.Errorf("实例名搜索：%d", got.Total)
	}
	if code, msg := status(root, "GET", "/api/v1/inspection/reports?level=weird", nil); code != 400 || !strings.Contains(msg, "级别") {
		t.Errorf("非法筛选：%d %s", code, msg)
	}
	if code, msg := status(root, "GET", "/api/v1/inspection/reports?start_date=2026/10/06", nil); code != 400 || !strings.Contains(msg, "YYYY-MM-DD") {
		t.Errorf("非法日期：%d %s", code, msg)
	}

	var detail struct {
		Report   reportRow `json:"report"`
		Engine   string    `json:"engine"`
		Facts    []Fact    `json:"facts"`
		Blocks   []Block   `json:"blocks"`
		Sections []Section `json:"sections"`
	}
	root.GetJSON("/api/v1/inspection/reports/mysql-1", &detail)
	if detail.Engine != "MySQL" || len(detail.Sections) < 15 || len(detail.Facts) == 0 || detail.Report.Ref != "mysql-1" {
		t.Errorf("MySQL 详情：%+v", detail.Report)
	}
	root.GetJSON("/api/v1/inspection/reports/mssql-1", &detail)
	if detail.Engine != "SQL Server" || len(detail.Sections) != len(msSections) {
		t.Errorf("SQL Server 详情：%d 个分区", len(detail.Sections))
	}
	if code, _ := status(root, "GET", "/api/v1/inspection/reports/mysql-99", nil); code != 404 {
		t.Errorf("不存在：%d", code)
	}
	if code, _ := status(root, "GET", "/api/v1/inspection/reports/nope", nil); code != 404 {
		t.Errorf("不合法的标识：%d", code)
	}

	// bob 只看得到实例 1 的报告
	grant(env, 2, "inspect.view")
	bob := env.Login("bob")
	if got := getList(t, bob, "/api/v1/inspection/reports"); got.Total != 1 || got.Data[0].Ref != "mysql-1" {
		t.Errorf("bob 的列表：%+v", got)
	}
	if code, _ := status(bob, "GET", "/api/v1/inspection/reports/mssql-1", nil); code != 404 {
		t.Errorf("范围外的报告按不存在处理：%d", code)
	}
	if code, _ := status(bob, "GET", "/api/v1/inspection/reports/mysql-1", nil); code != 200 {
		t.Errorf("范围内的可以看：%d", code)
	}

	// 取消已结束的报告：409；删除：200
	if code, _ := status(root, "POST", "/api/v1/inspection/reports/cancel", map[string]any{"ids": []string{"mysql-1"}}); code != 409 {
		t.Errorf("取消已结束：%d", code)
	}
	if code, _ := status(root, "POST", "/api/v1/inspection/reports/delete", map[string]any{"ids": []string{"mysql-1", "bad"}}); code != 400 {
		t.Errorf("非法标识：%d", code)
	}
	var del DeleteResult
	root.PostJSON("/api/v1/inspection/reports/delete", map[string]any{"ids": []string{"mysql-1"}}, &del)
	if del.Deleted != 1 || e.repo.report(KindMySQL, 1) != nil {
		t.Errorf("%+v", del)
	}
	if code, _ := status(root, "POST", "/api/v1/inspection/reports/delete", map[string]any{"ids": []string{}}); code != 400 {
		t.Errorf("空列表：%d", code)
	}
	if code, _ := status(root, "POST", "/api/v1/inspection/reports/run", "不是 JSON"); code != 400 {
		t.Errorf("坏请求：%d", code)
	}
	if code, _ := status(root, "POST", "/api/v1/inspection/reports/run", map[string]any{"instance_ids": []int{}}); code != 400 {
		t.Errorf("没选实例：%d", code)
	}
}

func TestHTTPCancel(t *testing.T) {
	e, env := newHTTP(t)
	block := make(chan struct{})
	started := make(chan struct{})
	e.ft = func(model.Instance) *fakeTarget {
		ft := mysqlHappyTarget()
		ft.block, ft.started = block, started
		return ft
	}
	root := env.Login("root")
	root.PostJSON("/api/v1/inspection/reports/run", map[string]any{"instance_ids": []int{1}}, nil)
	<-started
	l := getList(t, root, "/api/v1/inspection/reports?running=1")
	if l.Total != 1 || l.Data[0].Status != StatusRunning {
		t.Fatalf("进行中的列表：%+v", l)
	}
	// 进行中的不能删
	if code, _ := status(root, "POST", "/api/v1/inspection/reports/delete", map[string]any{"ids": []string{"mysql-1"}}); code != 409 {
		t.Errorf("进行中的不能删：%d", code)
	}
	var cr CancelResult
	root.PostJSON("/api/v1/inspection/reports/cancel", map[string]any{"ids": []string{"mysql-1"}}, &cr)
	if cr.Cancelled != 1 {
		t.Errorf("%+v", cr)
	}
	e.waitIdle()
	if r := e.repo.report(KindMySQL, 1); r.Status != StatusCancelled {
		t.Errorf("%s", r.Status)
	}
	acts := e.repo.auditActions()
	if acts[len(acts)-1] != "性能巡检·取消巡检" {
		t.Errorf("审计：%v", acts)
	}
}

func TestHTTPPlans(t *testing.T) {
	e, env := newHTTP(t)
	root := env.Login("root")
	body := map[string]any{
		"name": "生产 MySQL 夜间巡检", "kind": "weekly", "time": "03:00", "weekdays": []int{1, 3}, "instance_ids": []int{1, 2}, "tag_codes": []string{"mysql_inspection"},
		"concurrency": 3, "stagger_seconds": 30, "skip_busy": true, "retry_once": true, "timeout_minutes": 15,
		"notify_mode": "alert", "notify_channels": []int{7}, "retention_days": 90, "enabled": true,
	}
	var created struct {
		ID        uint   `json:"id"`
		NextRunAt string `json:"next_run_at"`
	}
	root.PostJSON("/api/v1/inspection/plans", body, &created)
	if created.ID == 0 || !strings.HasPrefix(created.NextRunAt, "2026-10-07T03:00") { // 2026-10-06 是周二，下一个是周三
		t.Fatalf("%+v", created)
	}
	// 同名 → 409；缺字段 → 400
	if code, _ := status(root, "POST", "/api/v1/inspection/plans", body); code != 409 {
		t.Errorf("同名：%d", code)
	}
	bad := map[string]any{"name": "x", "kind": "cron", "cron": "* * * * *", "instance_ids": []int{1}, "concurrency": 1, "timeout_minutes": 1, "retention_days": 1}
	if code, msg := status(root, "POST", "/api/v1/inspection/plans", bad); code != 400 || !strings.Contains(msg, "5 分钟") {
		t.Errorf("过密的 cron：%d %s", code, msg)
	}
	if code, _ := status(root, "POST", "/api/v1/inspection/plans", "不是 JSON"); code != 400 {
		t.Errorf("坏请求：%d", code)
	}

	var plans []PlanView
	root.GetJSON("/api/v1/inspection/plans", &plans)
	if len(plans) != 1 || plans[0].Describe != "每周一、三 03:00" || plans[0].InstanceTotal != 2 || plans[0].ChannelNames[0] != "研发群" {
		t.Fatalf("%+v", plans)
	}

	// 修改
	body["name"] = "生产 MySQL 夜间巡检（改）"
	body["kind"] = "daily"
	body["notify_mode"] = "none"
	body["notify_channels"] = []int{}
	url := "/api/v1/inspection/plans/" + itoa(created.ID)
	root.JSON("PUT", url, body, &created)
	if p, _ := e.repo.GetPlan(created.ID); p.Name != "生产 MySQL 夜间巡检（改）" || p.Kind != KindDaily || p.NotifyMode != NotifyNone || p.CreatedBy != "root" {
		t.Errorf("修改没生效：%+v", p)
	}
	if code, _ := status(root, "PUT", "/api/v1/inspection/plans/999", body); code != 404 {
		t.Errorf("修改不存在的计划：%d", code)
	}
	if code, _ := status(root, "PUT", "/api/v1/inspection/plans/abc", body); code != 404 {
		t.Errorf("非法的计划 ID：%d", code)
	}

	// 启停
	root.PostJSON(url+"/enable", map[string]any{"enabled": false}, nil)
	if p, _ := e.repo.GetPlan(created.ID); p.Enabled || p.NextRunAt != nil {
		t.Errorf("停用：%+v", p)
	}
	root.PostJSON(url+"/enable", map[string]any{"enabled": true}, nil)
	if p, _ := e.repo.GetPlan(created.ID); !p.Enabled || p.NextRunAt == nil {
		t.Errorf("启用：%+v", p)
	}

	// 预览下次运行时间
	var pv struct {
		Describe string   `json:"describe"`
		NextRuns []string `json:"next_runs"`
	}
	root.PostJSON("/api/v1/inspection/plans/preview", map[string]any{"kind": "cron", "cron": "0 */6 * * *"}, &pv)
	if pv.Describe != "cron 0 */6 * * *" || len(pv.NextRuns) != 5 || !strings.HasPrefix(pv.NextRuns[0], "2026-10-06T12:00") {
		t.Errorf("预览：%+v", pv)
	}
	if code, msg := status(root, "POST", "/api/v1/inspection/plans/preview", map[string]any{"kind": "cron", "cron": "bad"}); code != 400 || !strings.Contains(msg, "5 段") {
		t.Errorf("预览非法表达式：%d %s", code, msg)
	}

	// 立即运行 → 运行记录 → 报告关联到计划
	var rr struct {
		RunID uint `json:"run_id"`
	}
	root.PostJSON(url+"/run", nil, &rr)
	e.waitIdle()
	var runs []Run
	root.GetJSON(url+"/runs", &runs)
	if len(runs) != 1 || runs[0].ID != rr.RunID || runs[0].OK != 2 || runs[0].Trigger != TriggerManual {
		t.Fatalf("运行记录：%+v", runs)
	}
	if got := getList(t, root, "/api/v1/inspection/reports?plan_id="+itoa(created.ID)+"&run_id="+itoa(rr.RunID)); got.Total != 2 || got.Data[0].PlanName != "生产 MySQL 夜间巡检（改）" {
		t.Errorf("按计划 / 运行筛选：%+v", got)
	}
	if code, _ := status(root, "GET", "/api/v1/inspection/plans/999/runs", nil); code != 404 {
		t.Errorf("不存在的计划：%d", code)
	}

	// 删除
	root.JSON("DELETE", url, nil, nil)
	if p, _ := e.repo.GetPlan(created.ID); p != nil {
		t.Error("没删掉")
	}
	if code, _ := status(root, "DELETE", url, nil); code != 404 {
		t.Errorf("重复删除：%d", code)
	}
	want := []string{"新建计划", "修改计划", "停用计划", "启用计划", "立即运行计划", "删除计划"}
	var got []string
	for _, a := range e.repo.auditActions() {
		if strings.Contains(a, "计划") && a != "性能巡检·定时运行计划" {
			got = append(got, strings.TrimPrefix(a, "性能巡检·"))
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("审计：%v，期望 %v", got, want)
	}
	for _, a := range e.repo.audits {
		if strings.Contains(a.ExtraInfo, "webhook") || strings.Contains(a.ExtraInfo, "secret") {
			t.Errorf("审计里不能有密钥类信息：%s", a.ExtraInfo)
		}
	}
}

func TestHTTPPlansTablesMissing(t *testing.T) {
	e, env := newHTTP(t)
	e.repo.noTables = true
	root := env.Login("root")
	if code, msg := status(root, "GET", "/api/v1/inspection/plans", nil); code != 409 || !strings.Contains(msg, "013_inspect.sql") {
		t.Errorf("没建表要明确提示：%d %s", code, msg)
	}
	var opts struct {
		PlansReady bool `json:"plans_ready"`
	}
	root.GetJSON("/api/v1/inspection/options", &opts)
	if opts.PlansReady {
		t.Error("plans_ready 应为 false")
	}
	// 报告功能不依赖新表
	root.PostJSON("/api/v1/inspection/reports/run", map[string]any{"instance_ids": []int{1}}, nil)
	e.waitIdle()
	if l := getList(t, root, "/api/v1/inspection/reports"); l.Total != 1 {
		t.Errorf("%+v", l)
	}
}

func itoa(n uint) string { return strconv.FormatUint(uint64(n), 10) }
