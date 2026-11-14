// @author: kevinkelly
// @license: Apache Licence
// @file: ext_test.go
// @time: 2026/10/28

package perf

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"dbtool/internal/query"
)

func wantHTTP(t *testing.T, err error, status int, sub string) {
	t.Helper()
	qe, ok := err.(*query.Error)
	if !ok {
		t.Fatalf("期望业务错误 %d %q，实际 %v", status, sub, err)
	}
	if qe.HTTP != status || !strings.Contains(qe.Message, sub) {
		t.Fatalf("期望 %d 含 %q，实际 %d %q", status, sub, qe.HTTP, qe.Message)
	}
}

// ---- 列表：新页签 / 状态 / 来源 / 范围 / 计数 ----

func TestListViewsStatusSourceScopeAndCounts(t *testing.T) {
	f := newFx(t)
	id1 := f.newIssue() // 待指派给张三 → assigned
	if st := f.issue(id1).Status; st != StAssigned {
		t.Fatalf("初始状态 %s", st)
	}
	f.accept(id1) // accepted
	f.ingest(f.digest("select * from t_member where id = 2", 800, 3000))
	var id2 uint
	for id, i := range f.repo.issues {
		if id != id1 {
			id2 = i.ID
		}
	}
	// 第二张：手工来源 + 延期审批中
	f.repo.issues[id2].Source = "manual"
	f.repo.issues[id2].PendingExtendDate = ptr(f.now.AddDate(0, 0, 9))

	list := func(v Viewer, fl ListFilter) *ListResult {
		t.Helper()
		r, err := f.svc.ListIssues(v, fl)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := list(f.dba(), ListFilter{View: ViewAccepted}); r.Total != 1 || r.Rows[0].ID != id1 {
		t.Fatalf("处理中: %+v", r.Total)
	}
	if r := list(f.dba(), ListFilter{View: ViewAssigned}); r.Total != 1 || r.Rows[0].ID != id2 {
		t.Fatalf("待受理: %+v", r.Total)
	}
	if r := list(f.dba(), ListFilter{View: ViewApprove}); r.Total != 1 || r.Rows[0].ID != id2 {
		t.Fatalf("审批中（延期申请）: %+v", r.Total)
	}
	if r := list(f.dba(), ListFilter{View: ViewNew}); r.Total != 0 {
		t.Fatalf("待分派: %d", r.Total)
	}
	if r := list(f.dba(), ListFilter{View: ViewAll, Source: "manual"}); r.Total != 1 || r.Rows[0].ID != id2 {
		t.Fatalf("来源筛选: %d", r.Total)
	}
	if r := list(f.dba(), ListFilter{View: ViewAll, Status: StAccepted}); r.Total != 1 || r.Rows[0].ID != id1 {
		t.Fatalf("状态筛选: %d", r.Total)
	}
	// 范围：DBA（不在任何资源组）选“我的”应该看不到别人团队的工单；默认看全部
	if r := list(f.dba(), ListFilter{View: ViewAll}); r.Total != 2 {
		t.Fatalf("DBA 默认应看全部: %d", r.Total)
	}
	if r := list(f.dba(), ListFilter{View: ViewAll, Scope: "mine"}); r.Total != 0 {
		t.Fatalf("DBA 选 mine 应只看自己团队/负责的: %d", r.Total)
	}
	// 非 DBA 不能靠 scope=all 看到更多
	if r := list(f.stranger(), ListFilter{View: ViewAll, Scope: "all"}); r.Total != 0 {
		t.Fatalf("scope=all 不能越权: %d", r.Total)
	}
	// 计数
	r := list(f.dba(), ListFilter{View: ViewAll})
	c := r.Counts
	if c == nil || c.All != 2 || c.Open != 2 || c.Accepted != 1 || c.Assigned != 1 || c.Approve != 1 || c.Created7d != 2 || c.Closed != 0 {
		t.Fatalf("计数: %+v", c)
	}
	if r.Todo != c.Todo || r.Overdue != c.Overdue {
		t.Fatalf("兼容字段: %d/%d vs %+v", r.Todo, r.Overdue, c)
	}
	// 张三（订单研发）的待办 = 他负责且计时中的 2 张
	if r = list(f.dev(), ListFilter{View: ViewTodo}); r.Total != 2 || r.Counts.Todo != 2 {
		t.Fatalf("张三待办: %d %+v", r.Total, r.Counts)
	}
	// 非法参数
	for _, fl := range []ListFilter{{View: "bogus"}, {Status: "x"}, {Source: "x"}, {Scope: "x"}} {
		if _, err := f.svc.ListIssues(f.dba(), fl); err == nil {
			t.Fatalf("应拒绝 %+v", fl)
		}
	}
}

// 仓库不支持扩展：列表仍可用，只是没有 counts，Todo / Overdue 走原来的计数。
func TestListWithoutExtRepo(t *testing.T) {
	f := newFx(t)
	f.newIssue()
	f.svc.repo = plainRepo{f.repo}
	r, err := f.svc.ListIssues(f.dba(), ListFilter{View: ViewAll})
	if err != nil || r.Total != 1 || r.Counts != nil {
		t.Fatalf("降级: %+v %v", r, err)
	}
	if r.Todo != 0 && r.Todo != 1 {
		t.Fatalf("todo=%d", r.Todo)
	}
}

// ---- 标记修复：关联上线工单 / 修复效果 ----

func TestFixLinksWorkflow(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	f.accept(id)
	f.repo.workflows = map[uint]bool{1038: true}

	_, err := f.act(f.dev(), id, ActionInput{Action: ActFix, Remark: "加索引上线", WorkflowID: 999})
	wantHTTP(t, err, 400, "不存在")
	if f.issue(id).Status != StAccepted {
		t.Fatal("关联失败时不应改状态")
	}
	f.mustAct(f.dev(), id, ActionInput{Action: ActFix, Remark: "加索引上线", WorkflowID: 1038})
	d, err := f.svc.GetIssue(f.dev(), id)
	if err != nil || d.WorkflowID != 1038 {
		t.Fatalf("详情应带关联工单: %+v %v", d, err)
	}
	if logs := f.repo.logs; !strings.Contains(logs[len(logs)-1].Content, "#1038") {
		t.Fatalf("日志应记录关联: %+v", logs[len(logs)-1])
	}

	// 验证不通过后再次修复且不填关联：旧的关联被清掉
	f.mustAct(f.dba(), id, ActionInput{Action: ActVerifyFail, Remark: "还是慢"})
	f.mustAct(f.dev(), id, ActionInput{Action: ActFix, Remark: "换方案"})
	if d, _ = f.svc.GetIssue(f.dev(), id); d.WorkflowID != 0 {
		t.Fatalf("应清除旧关联: %d", d.WorkflowID)
	}

	// 旁路表还没创建：填了关联号 → 409 提示执行 sql；不填则不受影响
	f.mustAct(f.dba(), id, ActionInput{Action: ActVerifyFail, Remark: "再慢"})
	f.repo.extTables = map[string]bool{}
	_, err = f.act(f.dev(), id, ActionInput{Action: ActFix, Remark: "三改", WorkflowID: 1038})
	wantHTTP(t, err, 409, "060_perf_ext.sql")
	f.mustAct(f.dev(), id, ActionInput{Action: ActFix, Remark: "三改"})
}

func TestFixEffectAgainstBaseline(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	f.accept(id)
	i := f.issue(id)
	base, daily := i.AvgTimeMs, i.ImpactScore
	f.mustAct(f.dev(), id, ActionInput{Action: ActFix, Remark: "加索引"})
	d, _ := f.svc.GetIssue(f.dba(), id)
	if d.Effect == nil || d.Effect.Observed || d.Effect.BaselineAvgMs != base || d.Effect.BaselineDailySec != daily {
		t.Fatalf("刚修复还没有观察数据: %+v", d.Effect)
	}
	// 两天后：修复后每天 100 次共 10 秒（平均 100ms）
	for k := 1; k <= 2; k++ {
		day := dateOf(f.now).AddDate(0, 0, k)
		if err := f.repo.AddStat(id, day, StatDelta{ExecCount: 100, TotalTime: 10}); err != nil {
			t.Fatal(err)
		}
	}
	f.now = f.now.AddDate(0, 0, 3)
	d, _ = f.svc.GetIssue(f.dba(), id)
	e := d.Effect
	if e == nil || !e.Observed || e.Days != 2 || e.AfterAvgMs != 100 || e.AfterDailySec != 10 || !e.Pass || e.AvgImprovePct < 90 || e.NeedDays != 3 || e.NeedPct != 50 {
		t.Fatalf("效果: %+v (base %v)", e, base)
	}
	// 没有再出现：Absent
	f.repo.stats = map[string]*IssueStat{}
	d, _ = f.svc.GetIssue(f.dba(), id)
	if d.Effect == nil || !d.Effect.Absent || !d.Effect.Pass {
		t.Fatalf("未再出现: %+v", d.Effect)
	}
	// 没有标记过修复的工单没有效果块
	id2 := f.newIssueAnother()
	if d, _ = f.svc.GetIssue(f.dba(), id2); d.Effect != nil {
		t.Fatalf("不应有效果: %+v", d.Effect)
	}
}

func (f *fx) newIssueAnother() uint {
	f.t.Helper()
	f.ingest(f.digest("select * from t_other where k = 1", 900, 3500))
	i, _ := f.repo.FindIssue(1, "order", Checksum(Fingerprint("select * from t_other where k = 1")))
	return i.ID
}

// ---- 升级通知矩阵 ----

func TestEscalationMatrixDefaultsKeepLegacyTargets(t *testing.T) {
	f := newFx(t)
	m := f.svc.escalationMatrix()
	if !m.equal(defaultEscMatrix()) {
		t.Fatalf("默认矩阵: %+v", m)
	}
	// 原写死行为：即将超时/L1 只通知负责人；L2 加 TL 与 DBA；L3 再加总监
	if g := m.targetsFor(0); g != (EscTargets{Owner: true}) {
		t.Fatal(g)
	}
	if g := m.targetsFor(2); g != (EscTargets{Owner: true, TL: true, DBA: true}) {
		t.Fatal(g)
	}
	if g := m.targetsFor(3); g != (EscTargets{Owner: true, TL: true, DBA: true, Director: true}) {
		t.Fatal(g)
	}
}

func TestEscalationMatrixSaveAndUse(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	t0 := f.now
	custom := defaultEscMatrix()
	custom["L2"] = EscTargets{Owner: true, DBA: true} // L2 不再 @ TL
	custom["L3"] = EscTargets{Owner: true, Director: true}

	if err := f.svc.SaveEscalation(f.dev(), custom); err == nil {
		t.Fatal("非 DBA 不能保存")
	}
	bad := defaultEscMatrix()
	bad["L2"] = EscTargets{TL: true}
	wantHTTP(t, f.svc.SaveEscalation(f.dba(), bad), 400, "必须通知负责人")
	miss := defaultEscMatrix()
	delete(miss, "L3")
	wantHTTP(t, f.svc.SaveEscalation(f.dba(), miss), 400, "缺少 L3")
	if err := f.svc.SaveEscalation(f.dba(), custom); err != nil {
		t.Fatal(err)
	}
	if !f.svc.escalationMatrix().equal(custom) {
		t.Fatal("保存后应读到自定义矩阵")
	}
	if a := f.repo.auditActions(); a[len(a)-1] != "性能工单配置-升级通知对象修改" {
		t.Fatalf("审计: %v", a)
	}

	f.n.reset()
	f.now = t0.Add(49 * time.Hour) // 超过解决时限 → L2
	f.runSLA()
	ev := f.n.last()
	if !hasUser(ev, "zhangsan") || !hasUser(ev, "dba1") || hasUser(ev, "tl") {
		t.Fatalf("L2 应按矩阵只 @ 负责人和 DBA: %v", ev.ExtraUsers)
	}
	f.now = t0.Add(80 * time.Hour) // 48h + 24h = 72h → L3
	f.runSLA()
	ev = f.n.last()
	if !hasUser(ev, "boss") || hasUser(ev, "tl") || hasUser(ev, "dba1") {
		t.Fatalf("L3 应按矩阵只 @ 负责人和总监: %v", ev.ExtraUsers)
	}
	_ = id

	// 内容损坏时回退默认值
	f.repo.settings[settingEscMatrix] = `{"L1":{"owner":false}}`
	if !f.svc.escalationMatrix().equal(defaultEscMatrix()) {
		t.Fatal("损坏的矩阵应回退默认")
	}
	// 表不存在：读默认；改了矩阵时提示执行 sql，没改则静默成功
	f.repo.extTables = map[string]bool{}
	if !f.svc.escalationMatrix().equal(defaultEscMatrix()) {
		t.Fatal("表不存在应读默认")
	}
	wantHTTP(t, f.svc.SaveEscalation(f.dba(), custom), 409, "060_perf_ext.sql")
	if err := f.svc.SaveEscalation(f.dba(), defaultEscMatrix()); err != nil {
		t.Fatalf("矩阵没有变化不应报错: %v", err)
	}
}

// ---- 路由：表名正则与路由测试 ----

func TestRouteTableRegexAndRouteTest(t *testing.T) {
	f := newFx(t)
	f.repo.routes = []Route{
		{ID: 1, DBRegex: "order", TeamID: 11, AssigneeID: ptr(uint(uDev2)), Priority: 1}, // 只管 refunds/payments 表
		{ID: 2, DBRegex: "order", TeamID: 10, AssigneeID: ptr(uint(uDev)), Priority: 2},  // 兜底，不限表
		{ID: 3, InstanceID: ptr(uint(1)), DBRegex: ".*", TeamID: 11, Priority: 3},        // 永远排在后面
	}
	f.repo.extRoute = map[uint]string{1: "(payments|refunds)"}

	r, err := f.svc.route(1, "order", SideDev, f.cfg(), "refunds")
	if err != nil || r.teamID != 11 || r.assigneeID != uDev2 {
		t.Fatalf("表名命中: %+v %v", r, err)
	}
	r, _ = f.svc.route(1, "order", SideDev, f.cfg(), "orders", "refunds")
	if r.assigneeID != uDev2 {
		t.Fatalf("任一张表命中即可: %+v", r)
	}
	r, _ = f.svc.route(1, "order", SideDev, f.cfg(), "orders")
	if r.teamID != 10 || r.assigneeID != uDev {
		t.Fatalf("表名不匹配应落到下一条: %+v", r)
	}
	// 没有表信息：设置了表名正则的规则不命中；没设置的不受影响
	r, _ = f.svc.route(1, "order", SideDev, f.cfg())
	if r.teamID != 10 {
		t.Fatalf("没有表信息: %+v", r)
	}
	// 整体匹配
	r, _ = f.svc.route(1, "order", SideDev, f.cfg(), "payments_old")
	if r.teamID != 10 {
		t.Fatalf("表名正则应整体匹配: %+v", r)
	}

	// 路由测试
	res, err := f.svc.TestRoute(RouteTestInput{InstanceID: 1, DB: "order", SQL: "select * from refunds r join orders o on o.id = r.oid"})
	if err != nil || !res.Matched || res.Rule.ID != 1 || res.Team != "会员研发" || res.Assignee != "李四" || res.RuleTbl != "(payments|refunds)" || res.AssigneeSrc != "route" {
		t.Fatalf("路由测试: %+v %v", res, err)
	}
	res, _ = f.svc.TestRoute(RouteTestInput{InstanceID: 1, DB: "order", Table: "orders"})
	if res.Rule.ID != 2 || res.Assignee != "张三" || !strings.Contains(res.Summary, "命中路由规则 #2") {
		t.Fatalf("路由测试 2: %+v", res)
	}
	f.repo.routes = nil
	res, _ = f.svc.TestRoute(RouteTestInput{InstanceID: 1, DB: "order"})
	if res.Matched || res.TeamSrc != "instance_group" && res.TeamSrc != "collect_config" || res.AssigneeSrc != "db_owner" {
		t.Fatalf("没有规则: %+v", res)
	}
	res, _ = f.svc.TestRoute(RouteTestInput{InstanceID: 1, DB: "nobody", OwnerSide: SideDBA})
	if res.AssigneeSrc != "dba_rotation" || res.Assignee != "DBA小刘" {
		t.Fatalf("DBA 值班: %+v", res)
	}
	for _, in := range []RouteTestInput{{InstanceID: 0}, {InstanceID: 3}, {InstanceID: 1, OwnerSide: "x"}, {InstanceID: 1, DB: strings.Repeat("a", 129)}} {
		if _, err := f.svc.TestRoute(in); err == nil {
			t.Fatalf("应拒绝 %+v", in)
		}
	}
}

func TestSaveRouteWithTableRegex(t *testing.T) {
	f := newFx(t)
	in := RouteInput{DBRegex: "order", TeamID: 10, Priority: 5, TblRegex: "t_(a|b)"}
	r, err := f.svc.SaveRoute(f.dba(), in)
	if err != nil || r.ID == 0 {
		t.Fatal(err)
	}
	if f.repo.extRoute[r.ID] != "t_(a|b)" {
		t.Fatalf("表名正则应落旁路表: %v", f.repo.extRoute)
	}
	o, _ := f.svc.TeamOverview()
	if len(o.Routes) != 1 || o.Routes[0].TblRegex != "t_(a|b)" || !o.TblRegex {
		t.Fatalf("总览: %+v", o.Routes)
	}
	// ".*" 与空等价于不限制：清掉旧值
	in.ID, in.TblRegex = r.ID, ".*"
	if _, err := f.svc.SaveRoute(f.dba(), in); err != nil || len(f.repo.extRoute) != 0 {
		t.Fatalf("清除: %v %v", err, f.repo.extRoute)
	}
	in.TblRegex = "(unclosed"
	_, err = f.svc.SaveRoute(f.dba(), in)
	wantHTTP(t, err, 400, "表名正则不合法")
	in.TblRegex = strings.Repeat("a", 201)
	_, err = f.svc.SaveRoute(f.dba(), in)
	wantHTTP(t, err, 400, "过长")
	// 旁路表不存在：设置表名正则 409，不设置照常保存
	f.repo.extTables = map[string]bool{}
	in.TblRegex = "x"
	_, err = f.svc.SaveRoute(f.dba(), in)
	wantHTTP(t, err, 409, "060_perf_ext.sql")
	in.TblRegex = ""
	if _, err = f.svc.SaveRoute(f.dba(), in); err != nil {
		t.Fatal(err)
	}
	// 删除规则时一并清掉表名正则
	f.repo.extTables = nil
	f.repo.extRoute = map[uint]string{r.ID: "x"}
	if err := f.svc.DeleteRoute(f.dba(), r.ID); err != nil || len(f.repo.extRoute) != 0 {
		t.Fatalf("删除: %v %v", err, f.repo.extRoute)
	}
}

// ---- 采集窗口 ----

func TestCollectWindowSave(t *testing.T) {
	f := newFx(t)
	o, _ := f.svc.CollectOverview()
	if o.Window.Minutes != 60 || o.Window.Custom || !o.Window.Writable || o.Window.Min != 5 || o.Window.Max != 1440 {
		t.Fatalf("默认窗口: %+v", o.Window)
	}
	if _, err := f.svc.SaveCollectWindow(f.dev(), 30); err == nil {
		t.Fatal("非 DBA 不能改")
	}
	for _, bad := range []int{0, 4, 1441, -1} {
		if _, err := f.svc.SaveCollectWindow(f.dba(), bad); err == nil {
			t.Fatalf("窗口 %d 应被拒绝", bad)
		}
	}
	w, err := f.svc.SaveCollectWindow(f.dba(), 15)
	if err != nil || w.Minutes != 15 || !w.Custom {
		t.Fatalf("保存: %+v %v", w, err)
	}
	if f.svc.collectWindow() != 15*time.Minute {
		t.Fatalf("应立即生效: %v", f.svc.collectWindow())
	}
	if a := f.repo.auditActions(); a[len(a)-1] != "性能工单配置-采集窗口修改" {
		t.Fatalf("审计: %v", a)
	}
	// 仓库不支持写：提示去配置项
	f.svc.repo = plainRepo{f.repo}
	if _, err := f.svc.SaveCollectWindow(f.dba(), 20); err == nil || !strings.Contains(err.Error(), "配置项") {
		t.Fatalf("不支持写: %v", err)
	}
	if o, _ = f.svc.CollectOverview(); o.Window.Writable {
		t.Fatal("不支持写时 writable 应为 false")
	}
}

func TestEscMatrixJSONShape(t *testing.T) {
	b, _ := json.Marshal(defaultEscMatrix())
	for _, k := range []string{`"L1"`, `"L2"`, `"L3"`, `"owner":true`, `"tl":`, `"dba":`, `"director":`} {
		if !strings.Contains(string(b), k) {
			t.Fatalf("矩阵 JSON 缺 %s: %s", k, b)
		}
	}
}
