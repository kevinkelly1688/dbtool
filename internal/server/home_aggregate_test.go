// @author: kevinkelly
// @license: Apache Licence
// @file: home_aggregate_test.go
// @time: 2026/10/28

package server_test

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/server"
)

// 首页聚合：待我审批（SQL 上线 + 查询权限）、我提交的、KPI、近 7 日图、平台状态；审批人和提交人看到的不同。
func TestHomeAggregates(t *testing.T) {
	e := newSQLEnv(t)
	m := e.repo.Repo
	m.Privs = nil // 这个用例要现场申请 shop.orders 的查询权限：去掉 newSQLEnv 为导出预置的权限
	// 让 dev 能申请查询权限、leader 能审批（对应 newApplyEnv 里的权限与审批流）
	m.Perms[1]["qpriv.list"], m.Perms[1]["qpriv.view"], m.Perms[1]["query.run"] = true, true, true
	m.Perms[2]["qpriv.review"], m.Perms[2]["qpriv.view"], m.Perms[2]["qpriv.manage"] = true, true, true
	m.Settings[[2]int{1, 1}] = &model.WorkflowAuditSetting{GroupID: 1, WorkflowType: 1, AuditAuthGroups: "10"}
	dev, leader, viewer := e.login(t, "dev"), e.login(t, "leader"), e.login(t, "viewer")

	w := dev.do("POST", "/api/v1/sqlworkflow", submitBody("update orders set a=1 where id=1"))
	wfID := itoa(dataOf(t, w)["workflow_id"].(float64))
	if w.Code != 200 {
		t.Fatalf("提交工单: %d %s", w.Code, w.Body.String())
	}
	w = dev.do("POST", "/api/v1/query/privilege-applies", createBody())
	applyNo := itoa(applyID(t, w))

	kpi := func(body, id string) (value, hint string) {
		m := regexp.MustCompile(`id="` + id + `"[^>]*>.*?data-role="v">([^<]*)</span><span class="hk-d" data-role="d">([^<]*)</span>`).FindStringSubmatch(body)
		if m == nil {
			return "", ""
		}
		return m[1], m[2]
	}

	// 审批人：2 项待我审批（1 张 SQL 工单 + 1 张权限申请），行可点进详情
	body := leader.get("/").Body.String()
	if v, _ := kpi(body, "k-approve"); v != "2" {
		t.Errorf("审批人的“待我审批”应为 2: %q", v)
	}
	for _, want := range []string{`href="/sqlworkflow/` + wfID + `"`, `href="/query/applies/` + applyNo + `"`, "给 orders 加索引", `data-tab="approve"`, `data-tab="mine"`,
		`id="cnt-approve">2<`, `id="todo-approve"`, "近 7 日 SQL 工单", `<svg class="home-chart"`, "平台状态", "goInception"} {
		if !strings.Contains(body, want) {
			t.Errorf("审批人首页缺少 %q", want)
		}
	}
	// 近 7 日图：今天（最后一根柱）有 1 张工单
	if !regexp.MustCompile(`class="bar today"[^>]*>.*?</rect>\s*<text class="bar-n"[^>]*>1</text>`).MatchString(body) {
		t.Error("近 7 日图今天应有 1 张")
	}

	// 提交人：自己提交的不算“等我审批”，我提交的页签里有那张权限申请
	body = dev.get("/").Body.String()
	if v, hint := kpi(body, "k-approve"); v != "0" || !strings.Contains(hint, "没有待审批") {
		t.Errorf("提交人的“待我审批”应为 0: %q %q", v, hint)
	}
	if strings.Contains(body, `href="/sqlworkflow/`+wfID+`"><span class="tag brand">SQL 上线</span><span class="todo-main"><b>给 orders 加索引`) && !strings.Contains(body, `id="todo-mine"`) {
		t.Error("结构不对")
	}
	if !strings.Contains(body, `id="cnt-mine"`) || !strings.Contains(body, "审批中") {
		t.Error("提交人应在“我提交的”里看到审批中的权限申请")
	}

	// 只有菜单权限、没有审批权的人：待办卡片仍可见，但没有待审批
	body = viewer.get("/").Body.String()
	if v, _ := kpi(body, "k-approve"); v != "0" {
		t.Errorf("无审批权限: %q", v)
	}

	// 页面脚本是外部文件，没有内联脚本 / 事件
	if strings.Contains(body, "onclick=") || regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]`).MatchString(body) {
		t.Error("首页不能有内联脚本")
	}
	if !strings.Contains(body, `src="/static/js/home.js"`) {
		t.Error("应引入 home.js")
	}
}

// 没有 SQL 工单 / 权限申请服务的最小环境：首页照常打开，没有对应的块。
func TestHomeMinimalEnvHidesBlocks(t *testing.T) {
	e := newApplyEnv(t)
	body := e.login(t, "dev").get("/").Body.String()
	if strings.Contains(body, "近 7 日 SQL 工单") || strings.Contains(body, "<svg class=\"home-chart\"") {
		t.Error("没有 SQL 工单服务时不应出现 SQL 相关块")
	}
	if !strings.Contains(body, "快捷入口") {
		t.Error("快捷入口应保留")
	}
}

type fakeHomeDB struct {
	wfs      []server.HomeWorkflowRow
	audits   []model.AuditEntry
	lastUser *string
}

func (f fakeHomeDB) MyWorkflows(username string, limit int) ([]server.HomeWorkflowRow, int64, error) {
	if username != "dev" {
		return nil, 0, nil
	}
	return f.wfs, int64(len(f.wfs)), nil
}

func (f fakeHomeDB) RecentAudit(username string, limit int) ([]model.AuditEntry, error) {
	*f.lastUser = username
	var out []model.AuditEntry
	for _, a := range f.audits {
		if username == "" || a.UserName == username {
			out = append(out, a)
		}
	}
	return out, nil
}

// 我提交的 SQL 工单 + 最近动态：普通用户只看自己的，有“查看审计日志”按钮权限（这里用超级用户）看全平台。
func TestHomeMineAndActivity(t *testing.T) {
	last := new(string)
	restore := server.SetHomeDB(fakeHomeDB{
		wfs: []server.HomeWorkflowRow{{ID: 7, Name: "给 orders 加索引", InstanceName: "prod-mysql-01", DBName: "shop", Status: model.WfException},
			{ID: 6, Name: "清理历史数据", InstanceName: "prod-mysql-01", DBName: "shop", Status: model.WfManReviewing}},
		audits: []model.AuditEntry{{UserName: "dev", UserDisplay: "开发者", Action: "登入", ExtraInfo: "10.0.0.1（本地）"},
			{UserName: "leader", UserDisplay: "组长", Action: "审批通过", ExtraInfo: "<script>alert(1)</script>"}},
		lastUser: last,
	})
	defer restore()
	e := newSQLEnv(t)
	dev := e.login(t, "dev")
	body := dev.get("/").Body.String()
	for _, want := range []string{`href="/sqlworkflow/7"`, `href="/sqlworkflow/6"`, "清理历史数据", "执行有异常", "需处理", `id="cnt-mine">2<`, "我的操作", "开发者"} {
		if !strings.Contains(body, want) {
			t.Errorf("提交人首页缺少 %q", want)
		}
	}
	if *last != "dev" || strings.Contains(body, "审批通过") {
		t.Errorf("普通用户只看自己的动态: query=%q", *last)
	}
	if v := regexp.MustCompile(`id="k-mine"[^>]*>.*?data-role="v">([^<]*)<`).FindStringSubmatch(body); v == nil || !strings.HasPrefix(v[1], "2 / ") {
		t.Errorf("“我的进行中工单”应为 2 / 可见范围内全部: %v", v)
	}
	// 超级用户 → 全平台；ExtraInfo 里的 HTML 被转义
	e.repo.users["root"] = &model.User{ID: 100, Username: "root", Display: "管理员", Password: e.repo.users["dev"].Password, IsActive: true, IsSuperuser: true}
	root := e.login(t, "root")
	body = root.get("/").Body.String()
	if *last != "" || !strings.Contains(body, "全平台") || !strings.Contains(body, "审批通过") || !strings.Contains(body, `href="/audit"`) {
		t.Errorf("超级用户看全平台动态: query=%q", *last)
	}
	if strings.Contains(body, "<script>alert(1)") || !strings.Contains(body, "&lt;script&gt;alert(1)") {
		t.Error("动态内容必须转义")
	}
}

// S9-A1：其他模块（数据归档）可以把“等我审批”的单据汇入首页的待办；钩子出错 / panic 不影响首页其余部分。
func TestHomeApproveIncludesModuleHooks(t *testing.T) {
	var gotUser string
	restore := server.ReplaceHomeApproveHooks(
		func(s *server.Server, a audit.Actor, limit int) ([]server.HomeTodoRow, int, error) {
			gotUser = a.Username
			if a.Username != "leader" {
				return nil, 0, nil
			}
			return []server.HomeTodoRow{{Kind: "数据归档", Title: "清理 orders 历史数据", Sub: "prod-mysql-01 / shop.orders · 张三 · 10-08 09:00", Href: "/archive#task/12"}}, 3, nil
		},
		func(s *server.Server, a audit.Actor, limit int) ([]server.HomeTodoRow, int, error) {
			return nil, 0, errors.New("boom")
		},
		func(s *server.Server, a audit.Actor, limit int) ([]server.HomeTodoRow, int, error) { panic("bad hook") },
	)
	defer restore()
	e := newSQLEnv(t)
	m := e.repo.Repo
	m.Perms[2]["qpriv.review"], m.Perms[2]["qpriv.view"], m.Perms[2]["qpriv.manage"] = true, true, true
	leader, viewer := e.login(t, "leader"), e.login(t, "viewer")

	body := leader.get("/").Body.String()
	if gotUser != "leader" {
		t.Fatalf("钩子应收到当前用户: %q", gotUser)
	}
	for _, want := range []string{`href="/archive#task/12"`, "数据归档", "清理 orders 历史数据", `id="cnt-approve">3<`} {
		if !strings.Contains(body, want) {
			t.Errorf("首页待我审批缺少 %q", want)
		}
	}
	// 钩子报告共 3 项但只给了 1 行：其余的显示在“还有 N 项”里
	if !strings.Contains(body, "还有 2 项") {
		t.Errorf("总数大于已列出行数时应提示还有几项")
	}
	if v := regexp.MustCompile(`id="k-approve"[^>]*>.*?data-role="v">([^<]*)<`).FindStringSubmatch(body); v == nil || v[1] != "3" {
		t.Errorf("KPI“待我审批”应包含归档待办: %v", v)
	}
	// 别的用户没有归档待办
	body = viewer.get("/").Body.String()
	if strings.Contains(body, "/archive#task/12") {
		t.Error("没有归档审批权的人不应看到归档待办")
	}
}

// S9-B1：“我的待办”标题旁的 todo-total 由页面脚本赋值（待我审批 + 性能工单待办）。
func TestHomeScriptFillsTodoTotal(t *testing.T) {
	b, err := os.ReadFile("../../web/static/js/home.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{"todo-total", "cnt-approve", "cnt-perf"} {
		if !strings.Contains(js, want) {
			t.Errorf("home.js 应读写 %q 以汇总待办总数", want)
		}
	}
	if !strings.Contains(js, "updateTodoTotal") {
		t.Error("home.js 应有 updateTodoTotal，在页面加载与性能工单加载后更新总数")
	}
}

// 只有“数据归档”权限（archive.view）的用户也有“我的待办”卡片：之前 HasTodo 只看 SQL 上线 / 权限申请页面权限，归档待审批无处显示。
func TestHomeTodoCardForArchiveOnlyUser(t *testing.T) {
	restore := server.ReplaceHomeApproveHooks(
		func(s *server.Server, a audit.Actor, limit int) ([]server.HomeTodoRow, int, error) {
			if !s.ActorHasButton(a, "archive.view") {
				return nil, 0, nil
			}
			return []server.HomeTodoRow{{Kind: "数据归档", Title: "清理 orders 历史数据", Sub: "prod-mysql-01 / shop.orders", Href: "/archive#task/12"}}, 1, nil
		},
	)
	defer restore()
	e := newSQLEnv(t)
	e.repo.Repo.Perms[3]["archive.view"] = true // other：除查询外只有 archive.view
	body := e.login(t, "other").get("/").Body.String()
	for _, want := range []string{`id="todo-approve"`, `id="cnt-approve">1<`, `href="/archive#task/12"`, "清理 orders 历史数据"} {
		if !strings.Contains(body, want) {
			t.Errorf("只有归档权限的用户应看到待办卡片，缺少 %q", want)
		}
	}
	// 没有 archive.view 的同类用户：仍然没有待办卡片
	e.repo.Repo.Perms[3]["archive.view"] = false
	body = e.login(t, "other").get("/").Body.String()
	if strings.Contains(body, `id="todo-approve"`) || strings.Contains(body, "/archive#task/12") {
		t.Error("没有任何待办来源权限的用户不应出现待办卡片")
	}
}
