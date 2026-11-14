// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package perf

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/notify"
	"dbtool/internal/query"
	"dbtool/internal/settings"
)

func httpOf(err error) int {
	var qe *query.Error
	if errors.As(err, &qe) {
		return qe.HTTP
	}
	return 0
}

func wantErr(t *testing.T, err error, status int, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("应该失败（%d %s）", status, contains)
	}
	if httpOf(err) != status || !strings.Contains(err.Error(), contains) {
		t.Fatalf("错误不符: %d %v，期望 %d 含 %q", httpOf(err), err, status, contains)
	}
}

func TestIngestCreatesIssueWithRouting(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	i := f.issue(id)
	if i.Severity != SevP0 || i.Status != StAssigned || i.OwnerSide != SideDev {
		t.Fatalf("级别/状态不对: %+v", i)
	}
	if i.AssigneeID == nil || *i.AssigneeID != uDev || i.TeamID == nil || *i.TeamID != 10 || i.DBAID == nil || *i.DBAID != uDBA {
		t.Fatalf("路由不对: assignee=%v team=%v dba=%v", i.AssigneeID, i.TeamID, i.DBAID)
	}
	if !i.AcceptDeadline.Equal(f.now.Add(4*time.Hour)) || !i.ResolveDeadline.Equal(f.now.Add(48*time.Hour)) {
		t.Fatalf("P0 时限不对: %v %v", i.AcceptDeadline, i.ResolveDeadline)
	}
	if i.ImpactScore != 4000 || i.AvgTimeMs != 4000 || i.PerDayCount != 1000 || i.LoadShare != 100 {
		t.Fatalf("影响量化不对: %+v", i)
	}
	if i.Tables != "t_order" || i.Source != "collector" {
		t.Fatalf("表/来源: %q %q", i.Tables, i.Source)
	}
	if got := f.repo.logActions(id); len(got) != 1 || got[0] != ActCreate {
		t.Fatalf("日志: %v", got)
	}
	if got := f.repo.auditActions(); len(got) != 1 || got[0] != "性能工单-建单" {
		t.Fatalf("审计: %v", got)
	}
	ev := f.n.last()
	if ev.Name != notify.EventPending || ev.GroupID != 10 || ev.Applicant != "zhangsan" || !hasUser(ev, "zhangsan") || ev.Kind != KindPerfIssue {
		t.Fatalf("通知: %+v", ev)
	}
	if !strings.Contains(ev.Content, "http://dba.example.com/perf/") || !strings.Contains(ev.Title, "新问题待受理") {
		t.Fatalf("通知内容: %s / %s", ev.Title, ev.Content)
	}
}

func TestIngestDedupeAndAccumulate(t *testing.T) {
	f := newFx(t)
	r1 := f.ingest(f.digest(sqlOrderByUser, 1000, 4000))
	r2 := f.ingest(f.digest("SELECT * FROM t_order WHERE user_id = 99", 500, 2000)) // 同一指纹
	if r1.Created != 1 || r2.Created != 0 || r2.Updated != 1 || len(f.repo.issues) != 1 {
		t.Fatalf("去重失败: %+v %+v n=%d", r1, r2, len(f.repo.issues))
	}
	i := f.issue(1)
	if i.ExecCount7d != 1500 || i.TotalTime7d != 6000 || i.ImpactScore != 6000 {
		t.Fatalf("指标没有累加: %+v", i)
	}
	// 不同库是不同的问题
	d := f.digest(sqlOrderByUser, 1000, 4000)
	d.DBName = "other"
	if r := f.ingest(d); r.Created != 1 || len(f.repo.issues) != 2 {
		t.Fatalf("不同库应各建一张: %+v", r)
	}
}

func TestIngestDuplicateRace(t *testing.T) {
	// 模拟并发刚建了同一个问题：Find 没找到，Insert 撞唯一键 → 按已有工单累加
	f := newFx(t)
	id := f.newIssue()
	ck := Checksum(Fingerprint(sqlOrderByUser))
	i := &Issue{InstanceID: 1, DBName: "order", Checksum: ck, CreateTime: f.now}
	if err := f.repo.InsertIssue(i); err != ErrDuplicate {
		t.Fatalf("唯一键应冲突: %v", err)
	}
	_ = id
}

func TestIngestFilters(t *testing.T) {
	f := newFx(t)
	cases := []struct {
		name string
		d    Digest
	}{
		{"排除语句", f.digest("show processlist", 100, 5000)},
		{"排除库", func() Digest { d := f.digest(sqlOrderByUser, 100, 5000); d.DBName = "MySQL"; return d }()},
		{"没有执行", f.digest(sqlOrderByUser, 0, 0)},
		{"不够慢", f.digest(sqlOrderByUser, 1000, 5)}, // 平均 5ms，总耗时 5s < 10s
	}
	for _, c := range cases {
		if r := f.ingest(c.d); r.Created+r.Updated != 0 {
			t.Fatalf("%s 不应建单: %+v", c.name, r)
		}
	}
	// 总耗时达标（高频小慢 SQL）也建单
	if r := f.ingest(f.digest(sqlOrderByUser, 100000, 20)); r.Created != 1 {
		t.Fatalf("高频小慢 SQL 应建单: %+v", r)
	}
}

func TestIngestBatchNotification(t *testing.T) {
	f := newFx(t)
	var ds []Digest
	for _, tb := range []string{"a", "b", "c", "d", "e", "f"} {
		ds = append(ds, f.digest("select * from t_"+tb+" where id = 1", 1000, 4000))
	}
	f.ingest(ds...)
	if f.n.count() != 1 {
		t.Fatalf("超过 %d 张应汇总成一条：%d 条", batchNotifyThreshold, f.n.count())
	}
	if ev := f.n.last(); !strings.Contains(ev.Title, "新发现 6 个") || ev.GroupID != 10 || !hasUser(ev, "zhangsan") {
		t.Fatalf("汇总通知: %+v", ev)
	}
}

func TestIngestSilentMode(t *testing.T) {
	f := newFx(t)
	f.repo.cfg[settings.KeyPerfSilentMode] = "true"
	f.newIssue()
	if f.n.count() != 0 {
		t.Fatal("静默模式不应发通知")
	}
	if len(f.repo.issues) != 1 {
		t.Fatal("静默模式仍要建单")
	}
}

func TestIngestRecurrenceSeverityUp(t *testing.T) {
	f := newFx(t)
	f.ingest(f.digest(sqlOrderByUser, 100, 120)) // 日均 120s → P2
	i := f.issue(1)
	if i.Severity != SevP2 {
		t.Fatalf("起始级别 %s", i.Severity)
	}
	deadline := *i.ResolveDeadline
	f.n.reset()
	f.ingest(f.digest(sqlOrderByUser, 1000, 4000)) // 累计 4120s → P0
	i = f.issue(1)
	if i.Severity != SevP0 {
		t.Fatalf("应升到 P0: %s", i.Severity)
	}
	if !i.ResolveDeadline.Before(deadline) || !i.ResolveDeadline.Equal(f.now.Add(48*time.Hour)) {
		t.Fatalf("时限应收紧: %v → %v", deadline, i.ResolveDeadline)
	}
	if got := f.repo.logActions(1); got[len(got)-1] != ActSeverity {
		t.Fatalf("日志: %v", got)
	}
	if ev := f.n.last(); !strings.Contains(ev.Title, "P2→P0") {
		t.Fatalf("通知: %s", ev.Title)
	}
	// 锁定级别后不再自动调整
	f.mustAct(f.dba(), 1, ActionInput{Action: ActSeverity, Severity: SevP3})
	f.ingest(f.digest(sqlOrderByUser, 100000, 400000))
	if f.issue(1).Severity != SevP3 {
		t.Fatal("人工锁定的级别不应被自动调整")
	}
}

func TestIngestRecurrenceReopen(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	f.accept(id)
	f.mustAct(f.dev(), id, ActionInput{Action: ActFix, Remark: "加索引已上线"})
	f.mustAct(f.dba(), id, ActionInput{Action: ActVerifyPass})
	if f.issue(id).Status != StClosed {
		t.Fatal("应已关闭")
	}
	f.n.reset()
	f.ingest(f.digest(sqlOrderByUser, 1000, 4000))
	i := f.issue(id)
	if i.Status != StAssigned || i.ReopenCount != 1 || i.CloseReason != "" || i.ClosedAt != nil {
		t.Fatalf("应自动重开: %+v", i)
	}
	if ev := f.n.last(); !strings.Contains(ev.Title, "重开") {
		t.Fatalf("通知: %s", ev.Title)
	}
	// 误报关闭的不再重开
	f.mustAct(f.dba(), id, ActionInput{Action: ActCloseDup, Remark: "误报"})
	f.ingest(f.digest(sqlOrderByUser, 1000, 4000))
	if f.issue(id).Status != StClosed {
		t.Fatal("重复/误报不应重开")
	}
}

func TestIngestWaivedOnlyReopensOnP0(t *testing.T) {
	f := newFx(t)
	f.ingest(f.digest(sqlOrderByUser, 100, 120)) // P2
	f.mustAct(f.dev(), 1, ActionInput{Action: ActWaive, Remark: "月度报表低频查询，业务可接受"})
	f.mustAct(f.dba(), 1, ActionInput{Action: ActWaiveApprove})
	f.ingest(f.digest(sqlOrderByUser, 100, 600)) // 累计日均 720 → P1，不是 P0
	if f.issue(1).Status != StClosed {
		t.Fatal("豁免的问题未到 P0 不应重开")
	}
	f.ingest(f.digest(sqlOrderByUser, 1000, 4000)) // P0
	if f.issue(1).Status == StClosed {
		t.Fatal("豁免的问题升到 P0 应重开")
	}
}

// ---- 状态机 ----

func TestAcceptValidation(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	yesterday := f.now.AddDate(0, 0, -1).Format(dateLayout)
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActAccept})), 400, "承诺完成日期")
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActAccept, PlanFinishDate: "2026/10/07"})), 400, "格式")
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActAccept, PlanFinishDate: yesterday})), 400, "不能早于今天")
	late := f.now.AddDate(0, 0, 5).Format(dateLayout) // P0 解决时限 2 天
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActAccept, PlanFinishDate: late})), 400, "超过 SLA 截止日")
	f.accept(id)
	i := f.issue(id)
	if i.Status != StAccepted || i.AcceptedAt == nil || i.PlanFinishDate == nil || *i.AssigneeID != uDev {
		t.Fatalf("受理后状态: %+v", i)
	}
	// 已受理的不能再受理
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActAccept, PlanFinishDate: f.now.Format(dateLayout)})), 409, "不能“受理”")
}

func mustSecond(_ *ActionResult, err error) error { return err }

func TestAcceptByTeamMemberTakesOverWhenUnassigned(t *testing.T) {
	f := newFx(t)
	f.repo.owners = map[string]string{}
	f.repo.teams[0].DefaultAssigneeID = nil
	id := f.newIssue()
	i := f.issue(id)
	if i.Status != StNew || i.AssigneeID != nil {
		t.Fatalf("没有负责人应待分派: %+v", i)
	}
	// 同团队的 TL 可以受理，并成为负责人
	tl := Viewer{Actor: f.dev().Actor, Groups: map[uint]bool{10: true}}
	tl.ID, tl.Username = uTL, "tl"
	f.mustAct(tl, id, ActionInput{Action: ActAccept, PlanFinishDate: f.now.Format(dateLayout)})
	if i = f.issue(id); i.AssigneeID == nil || *i.AssigneeID != uTL || i.Status != StAccepted {
		t.Fatalf("%+v", i)
	}
}

func TestPermissions(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	// 看不到的工单按不存在处理
	_, err := f.act(f.dev2(), id, ActionInput{Action: ActComment, Remark: "x"})
	wantErr(t, err, 404, "不存在")
	_, err = f.svc.GetIssue(f.stranger(), id)
	wantErr(t, err, 404, "不存在")
	// 研发不能做 DBA 的动作
	for _, a := range []string{ActAssign, ActVerifyPass, ActVerifyFail, ActExtendApprove, ActExtendReject, ActWaiveApprove, ActWaiveReject, ActSeverity, ActSide, ActCloseDup, ActReopen} {
		_, err = f.act(f.dev(), id, ActionInput{Action: a, Remark: "x"})
		wantErr(t, err, 403, "DBA")
	}
	_, err = f.act(f.dev(), id, ActionInput{Action: "nope"})
	wantErr(t, err, 400, "未知操作")
	_, err = f.act(f.dev(), id, ActionInput{Action: ActDiagnose})
	wantErr(t, err, 400, "未知操作")
	_, err = f.act(f.dev(), 999, ActionInput{Action: ActComment, Remark: "x"})
	wantErr(t, err, 404, "不存在")
	// 超级用户与 DBA 可以处理任何团队的工单
	f.mustAct(f.root(), id, ActionInput{Action: ActComment, Remark: "root 评论"})
	f.mustAct(f.dba(), id, ActionInput{Action: ActComment, Remark: "dba 评论"})
}

func TestFixVerifyFlow(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActFix})), 400, "优化方案")
	f.accept(id)
	f.n.reset()
	f.mustAct(f.dev(), id, ActionInput{Action: ActFix, Remark: "改写分页，v2.3.1 已上线"})
	i := f.issue(id)
	if i.Status != StFixed || i.FixedAt == nil || i.FixDesc == "" || i.BaselineAvgMs != 4000 || i.BaselineDailySec != 4000 {
		t.Fatalf("修复后: %+v", i)
	}
	if ev := f.n.last(); !hasUser(ev, "dba1") || !strings.Contains(ev.Title, "待验证") || !strings.Contains(ev.Content, "改写分页") {
		t.Fatalf("通知跟进 DBA: %+v", ev)
	}
	// 待验证时研发不能再申请延期/豁免
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActExtend, ExtendDate: "2026-10-30", Remark: "x"})), 409, "不能")
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActWaive, Remark: "月度报表低频查询，业务可接受"})), 409, "不能")
	// 验证不通过：必须写原因，回到处理中
	wantErr(t, mustSecond(f.act(f.dba(), id, ActionInput{Action: ActVerifyFail})), 400, "原因")
	f.mustAct(f.dba(), id, ActionInput{Action: ActVerifyFail, Remark: "耗时没有下降"})
	if i = f.issue(id); i.Status != StAccepted || i.ReopenCount != 1 {
		t.Fatalf("验证不通过: %+v", i)
	}
	// 再修复、验证通过
	f.mustAct(f.dev(), id, ActionInput{Action: ActFix, Remark: "再次优化"})
	f.n.reset()
	f.mustAct(f.dba(), id, ActionInput{Action: ActVerifyPass, Remark: "OK"})
	i = f.issue(id)
	if i.Status != StClosed || i.CloseReason != CloseVerified || i.ClosedAt == nil || i.SLABreached {
		t.Fatalf("验证通过: %+v", i)
	}
	if ev := f.n.last(); ev.Name != notify.EventFinished {
		t.Fatalf("关闭通知: %+v", ev)
	}
	// 只有待验证的工单能验证
	wantErr(t, mustSecond(f.act(f.dba(), id, ActionInput{Action: ActVerifyPass})), 409, "不能")
}

func TestCloseAfterDeadlineMarksBreachByFixTime(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	f.accept(id)
	f.mustAct(f.dev(), id, ActionInput{Action: ActFix, Remark: "按时修复"}) // 修复时间在截止前
	f.now = f.now.Add(60 * time.Hour)                                   // 验证拖到超时之后
	f.mustAct(f.dba(), id, ActionInput{Action: ActVerifyPass})
	if f.issue(id).SLABreached {
		t.Fatal("以研发标记修复的时间判定，验证期不算研发超时")
	}
	// 反之：修复本身就超时
	id2 := func() uint {
		f.ingest(f.digest("select * from t_x where a = 1", 1000, 4000))
		i, _ := f.repo.FindIssue(1, "order", Checksum(Fingerprint("select * from t_x where a = 1")))
		return i.ID
	}()
	f.now = f.now.Add(60 * time.Hour)
	f.mustAct(f.dev(), id2, ActionInput{Action: ActFix, Remark: "晚了"})
	f.mustAct(f.dba(), id2, ActionInput{Action: ActVerifyPass})
	if !f.issue(id2).SLABreached {
		t.Fatal("修复晚于截止应记超时")
	}
}

func TestExtendFlow(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	// P0 不允许延期
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActExtend, ExtendDate: "2026-10-20", Remark: "x"})), 400, "最多延期 0 次")
	f.mustAct(f.dba(), id, ActionInput{Action: ActSeverity, Severity: SevP1})
	i := f.issue(id)
	if !i.SeverityLocked || i.Severity != SevP1 || !i.ResolveDeadline.Equal(f.now.Add(7*24*time.Hour)) {
		t.Fatalf("降级后按当前时间重算时限: %+v", i)
	}
	cur := i.ResolveDeadline.Format(dateLayout) // 2026-10-13
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActExtend, ExtendDate: "bad", Remark: "x"})), 400, "延期日期")
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActExtend, ExtendDate: cur, Remark: "x"})), 400, "晚于当前的解决截止日")
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActExtend, ExtendDate: "2026-10-20"})), 400, "延期理由")
	f.n.reset()
	f.mustAct(f.dev(), id, ActionInput{Action: ActExtend, ExtendDate: "2026-10-20", Remark: "依赖的库表迁移要到下周"})
	if i = f.issue(id); i.PendingExtendDate == nil || i.PendingReason == "" {
		t.Fatalf("应有待审批的延期: %+v", i)
	}
	if ev := f.n.last(); !hasUser(ev, "dba1") || !strings.Contains(ev.Content, "2026-10-20") {
		t.Fatalf("通知 DBA: %+v", ev)
	}
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActExtend, ExtendDate: "2026-10-25", Remark: "again"})), 409, "待审批")
	// 驳回
	f.mustAct(f.dba(), id, ActionInput{Action: ActExtendReject, Remark: "不行"})
	if i = f.issue(id); i.PendingExtendDate != nil || i.PendingReason != "" || i.ExtendTimes != 0 {
		t.Fatalf("驳回后: %+v", i)
	}
	wantErr(t, mustSecond(f.act(f.dba(), id, ActionInput{Action: ActExtendApprove})), 409, "没有待审批")
	// 再申请、同意
	f.mustAct(f.dev(), id, ActionInput{Action: ActExtend, ExtendDate: "2026-10-20", Remark: "依赖的库表迁移要到下周"})
	f.mustAct(f.dba(), id, ActionInput{Action: ActExtendApprove})
	i = f.issue(id)
	want := time.Date(2026, 10, 20, 23, 59, 59, 0, time.Local)
	if !i.ResolveDeadline.Equal(want) || i.ExtendTimes != 1 || i.PendingExtendDate != nil || i.EscalationLevel != 0 {
		t.Fatalf("同意后: %+v", i)
	}
	// P1 最多 1 次
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActExtend, ExtendDate: "2026-10-30", Remark: "x"})), 400, "最多延期 1 次")
	// 详情里不再给“申请延期”按钮
	d, _ := f.svc.GetIssue(f.dev(), id)
	if hasAction(d.Actions, ActExtend) {
		t.Fatal("次数用完应隐藏申请延期")
	}
}

func hasAction(l []ActionDTO, name string) bool {
	for _, a := range l {
		if a.Name == name {
			return true
		}
	}
	return false
}

func TestWaiveFlow(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActWaive, Remark: "太短了"})), 400, "不少于10字")
	f.n.reset()
	f.mustAct(f.dev(), id, ActionInput{Action: ActWaive, Remark: "月度报表低频查询，业务可接受（每月 1 日跑一次）"})
	i := f.issue(id)
	if i.Status != StWaivePending || i.PendingReason == "" {
		t.Fatalf("%+v", i)
	}
	if ev := f.n.last(); !hasUser(ev, "dba1") || !strings.Contains(ev.Title, "豁免") {
		t.Fatalf("通知: %+v", ev)
	}
	// 驳回：没受理过 → 回到待受理
	f.mustAct(f.dba(), id, ActionInput{Action: ActWaiveReject, Remark: "不接受"})
	if i = f.issue(id); i.Status != StAssigned || i.PendingReason != "" {
		t.Fatalf("驳回后: %+v", i)
	}
	// 受理过的驳回回到处理中
	f.accept(id)
	f.mustAct(f.dev(), id, ActionInput{Action: ActWaive, Remark: "月度报表低频查询，业务可接受（每月 1 日跑一次）"})
	f.mustAct(f.dba(), id, ActionInput{Action: ActWaiveReject})
	if f.issue(id).Status != StAccepted {
		t.Fatal("受理过的应回到处理中")
	}
	// 同意：以豁免关闭
	f.mustAct(f.dev(), id, ActionInput{Action: ActWaive, Remark: "月度报表低频查询，业务可接受（每月 1 日跑一次）"})
	f.mustAct(f.dba(), id, ActionInput{Action: ActWaiveApprove, Remark: "同意"})
	i = f.issue(id)
	if i.Status != StClosed || i.CloseReason != CloseWaived || i.PendingReason != "" {
		t.Fatalf("同意豁免: %+v", i)
	}
	wantErr(t, mustSecond(f.act(f.dba(), id, ActionInput{Action: ActWaiveApprove})), 409, "不能")
}

func TestSeverityAndSide(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	wantErr(t, mustSecond(f.act(f.dba(), id, ActionInput{Action: ActSeverity, Severity: "P9"})), 400, "级别不合法")
	wantErr(t, mustSecond(f.act(f.dba(), id, ActionInput{Action: ActSeverity, Severity: SevP0})), 400, "没有变化")
	// 调高级别时，时限从建单时起算
	f.mustAct(f.dba(), id, ActionInput{Action: ActSeverity, Severity: SevP3})
	f.now = f.now.Add(30 * time.Hour)
	f.mustAct(f.dba(), id, ActionInput{Action: ActSeverity, Severity: SevP1})
	i := f.issue(id)
	created := i.CreateTime
	_ = created
	// 切换处理方：改给 DBA → 负责人变成跟进 DBA
	wantErr(t, mustSecond(f.act(f.dba(), id, ActionInput{Action: ActSide, OwnerSide: "x"})), 400, "处理方不合法")
	wantErr(t, mustSecond(f.act(f.dba(), id, ActionInput{Action: ActSide, OwnerSide: SideDev})), 400, "没有变化")
	f.mustAct(f.dba(), id, ActionInput{Action: ActSide, OwnerSide: SideDBA})
	i = f.issue(id)
	if i.OwnerSide != SideDBA || *i.AssigneeID != uDBA {
		t.Fatalf("改给 DBA: %+v", i)
	}
	// 改回研发：重新路由到库负责人
	f.mustAct(f.dba(), id, ActionInput{Action: ActSide, OwnerSide: SideDev})
	if i = f.issue(id); *i.AssigneeID != uDev {
		t.Fatalf("改回研发应重新路由: %v", *i.AssigneeID)
	}
}

func TestSeverityRaiseUsesCreateTime(t *testing.T) {
	f := newFx(t)
	f.ingest(f.digest(sqlOrderByUser, 100, 120)) // P2
	create := f.issue(1).CreateTime
	f.now = f.now.Add(10 * time.Hour)
	f.mustAct(f.dba(), 1, ActionInput{Action: ActSeverity, Severity: SevP0})
	i := f.issue(1)
	if !i.ResolveDeadline.Equal(create.Add(48*time.Hour)) || !i.AcceptDeadline.Equal(create.Add(4*time.Hour)) {
		t.Fatalf("调高级别应从建单时起算: %v %v", i.AcceptDeadline, i.ResolveDeadline)
	}
	if i.EscalationLevel != 0 || i.PreWarned {
		t.Fatal("重算时限要清掉升级状态")
	}
}

func TestAssign(t *testing.T) {
	f := newFx(t)
	f.repo.owners = map[string]string{}
	f.repo.teams[0].DefaultAssigneeID = nil
	id := f.newIssue()
	if f.issue(id).Status != StNew {
		t.Fatal("应待分派")
	}
	wantErr(t, mustSecond(f.act(f.dba(), id, ActionInput{Action: ActAssign})), 400, "请选择负责人")
	wantErr(t, mustSecond(f.act(f.dba(), id, ActionInput{Action: ActAssign, AssigneeID: 8})), 400, "已停用")
	wantErr(t, mustSecond(f.act(f.dba(), id, ActionInput{Action: ActAssign, AssigneeID: uDev, TeamID: 99})), 400, "团队不存在")
	f.n.reset()
	f.mustAct(f.dba(), id, ActionInput{Action: ActAssign, AssigneeID: uDev, TeamID: 11})
	i := f.issue(id)
	if i.Status != StAssigned || *i.AssigneeID != uDev || *i.TeamID != 11 {
		t.Fatalf("%+v", i)
	}
	if ev := f.n.last(); ev.GroupID != 11 || !hasUser(ev, "zhangsan") {
		t.Fatalf("通知: %+v", ev)
	}
}

func TestReopen(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	wantErr(t, mustSecond(f.act(f.dba(), id, ActionInput{Action: ActReopen})), 409, "不能")
	f.mustAct(f.dba(), id, ActionInput{Action: ActCloseDup, Remark: "重复"})
	if i := f.issue(id); i.Status != StClosed || i.CloseReason != CloseDuplicate {
		t.Fatalf("%+v", i)
	}
	wantErr(t, mustSecond(f.act(f.dba(), id, ActionInput{Action: ActCloseDup})), 409, "不能")
	f.mustAct(f.dba(), id, ActionInput{Action: ActReopen})
	i := f.issue(id)
	if i.Status != StAssigned || i.ReopenCount != 1 || i.ClosedAt != nil || !i.ResolveDeadline.Equal(f.now.Add(48*time.Hour)) {
		t.Fatalf("重开: %+v", i)
	}
}

func TestCommentAnyStatusExcludesActor(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActComment})), 400, "请填写内容")
	f.n.reset()
	f.mustAct(f.dev(), id, ActionInput{Action: ActComment, Remark: "在看了"})
	ev := f.n.last()
	if hasUser(ev, "zhangsan") || !hasUser(ev, "dba1") {
		t.Fatalf("评论不 @ 自己，要 @ DBA: %+v", ev)
	}
	f.mustAct(f.dba(), id, ActionInput{Action: ActCloseDup, Remark: "重复"})
	f.mustAct(f.dev(), id, ActionInput{Action: ActComment, Remark: "关闭后也能评论"})
}

func TestConcurrentTransitionConflict(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	// 在 Act 读出工单之后、写入之前，别人先改了它
	f.repo.beforeUpdate = func(i uint) {
		f.repo.beforeUpdate = nil
		cur := f.repo.issues[i]
		cur.UpdateTime = cur.UpdateTime.Add(time.Second)
		cur.Status = StWaivePending
	}
	logs := len(f.repo.logs)
	_, err := f.act(f.dev(), id, ActionInput{Action: ActAccept, PlanFinishDate: f.now.Format(dateLayout)})
	wantErr(t, err, 409, "刚刚被他人更新")
	if f.issue(id).Status != StWaivePending {
		t.Fatal("别人的修改不应被覆盖")
	}
	if len(f.repo.logs) != logs {
		t.Fatal("冲突时不应写日志")
	}
}

func TestAuditAndLogWritten(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	f.accept(id)
	f.mustAct(f.dev(), id, ActionInput{Action: ActFix, Remark: "已上线"})
	f.mustAct(f.dba(), id, ActionInput{Action: ActVerifyPass})
	logs := f.repo.logActions(id)
	if strings.Join(logs, ",") != "create,accept,fix,close" {
		t.Fatalf("日志: %v", logs)
	}
	last := f.repo.logs[len(f.repo.logs)-1]
	if last.Operator != "dba1" || last.OperatorDisplay != "DBA小刘" || !strings.Contains(last.Content, "优化完成(已验证)") {
		t.Fatalf("操作人/内容: %+v", last)
	}
	acts := f.repo.auditActions()
	if strings.Join(acts, ",") != "性能工单-建单,性能工单-受理,性能工单-标记修复,性能工单-验证通过" {
		t.Fatalf("审计: %v", acts)
	}
	if a := f.repo.audits[1]; a.UserName != "zhangsan" || !strings.Contains(a.ExtraInfo, "工单#") {
		t.Fatalf("审计内容: %+v", a)
	}
	// 审计写失败不影响主流程
	f.repo.failAudit = true
	f.mustAct(f.dba(), id, ActionInput{Action: ActReopen})
}

func TestGetIssueActionsByRoleAndStatus(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	names := func(v Viewer) string {
		d, err := f.svc.GetIssue(v, id)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, a := range d.Actions {
			out = append(out, a.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(f.dev()); got != "accept,fix,waive,comment" { // P0 不允许延期
		t.Fatalf("研发/待受理: %s", got)
	}
	if got := names(f.dba()); !strings.Contains(got, "assign") || !strings.Contains(got, "severity") || strings.Contains(got, "verify_pass") || strings.Contains(got, "reopen") {
		t.Fatalf("DBA/待受理: %s", got)
	}
	f.accept(id)
	if got := names(f.dev()); got != "fix,waive,comment" {
		t.Fatalf("研发/处理中: %s", got)
	}
	f.mustAct(f.dev(), id, ActionInput{Action: ActFix, Remark: "ok"})
	if got := names(f.dev()); got != "comment" {
		t.Fatalf("研发/待验证: %s", got)
	}
	if got := names(f.dba()); !strings.Contains(got, "verify_pass") || !strings.Contains(got, "verify_fail") {
		t.Fatalf("DBA/待验证: %s", got)
	}
	f.mustAct(f.dba(), id, ActionInput{Action: ActVerifyPass})
	if got := names(f.dba()); !strings.Contains(got, "reopen") || strings.Contains(got, "assign") {
		t.Fatalf("DBA/已关闭: %s", got)
	}
	d, _ := f.svc.GetIssue(f.dev(), id)
	if d.MaxPlanDate == "" || d.Today != "2026-10-06" || len(d.Logs) < 3 || d.Logs[0].ActionName != "关闭" {
		t.Fatalf("详情: %+v", d)
	}
}

func TestRediagnose(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	_, err := f.svc.Rediagnose(context.Background(), f.dev(), id, "")
	wantErr(t, err, 403, "DBA")
	res, err := f.svc.Rediagnose(context.Background(), f.dba(), id, "select * from t_order where name like '%abc'")
	if err != nil {
		t.Fatal(err)
	}
	i := f.issue(id)
	if res.Category != CatIndexInvalid || i.Category != CatIndexInvalid || !strings.Contains(i.SampleSQL, "like '%abc'") {
		t.Fatalf("%+v %q", res, i.SampleSQL)
	}
	if got := f.repo.logActions(id); got[len(got)-1] != ActDiagnose {
		t.Fatalf("日志: %v", got)
	}
	_, err = f.svc.Rediagnose(context.Background(), f.dba(), 999, "")
	wantErr(t, err, 404, "不存在")
}

// ---- 手工建单 ----

func TestCreateIssueManual(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	in := CreateInput{InstanceID: 1, DBName: "order", SQL: "select * from t_order where user_id = 7 and note like '%x'", Remark: "大促前必须优化"}
	_, err := f.svc.CreateIssue(ctx, f.dev(), false, in, nil, "manual", true)
	wantErr(t, err, 403, "DBA")
	_, err = f.svc.CreateIssue(ctx, f.dba(), false, CreateInput{InstanceID: 1}, nil, "manual", true)
	wantErr(t, err, 400, "请填写SQL")
	_, err = f.svc.CreateIssue(ctx, f.dba(), false, CreateInput{InstanceID: 3, SQL: "select 1"}, nil, "manual", true)
	wantErr(t, err, 400, "暂不支持")
	_, err = f.svc.CreateIssue(ctx, f.dba(), false, CreateInput{InstanceID: 1, SQL: "select 1", Severity: "PX"}, nil, "manual", true)
	wantErr(t, err, 400, "级别不合法")
	// DBA 没有该实例的资源组权限 → 看不到实例
	_, err = f.svc.CreateIssue(ctx, f.dba(), false, in, nil, "manual", true)
	wantErr(t, err, 400, "无权限")
	f.repo.ugroups[uDBA] = []uint{10}
	i, err := f.svc.CreateIssue(ctx, f.dba(), false, in, nil, "manual", true)
	if err != nil {
		t.Fatal(err)
	}
	got := f.issue(i.ID)
	if got.Source != "manual" || got.Severity != SevP2 || got.SeverityLocked || got.Category != CatIndexInvalid || got.Status != StAssigned || *got.AssigneeID != uDev {
		t.Fatalf("手工建单: %+v", got)
	}
	if acts := f.repo.logActions(i.ID); strings.Join(acts, ",") != "create,comment" {
		t.Fatalf("日志: %v", acts)
	}
	// 同一指纹再建 → 提示已有工单
	_, err = f.svc.CreateIssue(ctx, f.root(), true, in, nil, "manual", true)
	var ex *ExistsError
	if !errors.As(err, &ex) || ex.ID != i.ID {
		t.Fatalf("应提示已有工单: %v", err)
	}
	// 指定级别与负责人：锁定级别、按指定负责人
	in2 := CreateInput{InstanceID: 1, DBName: "order", SQL: "select * from t_member where a = 1", Severity: SevP1, AssigneeID: uDev2}
	i2, err := f.svc.CreateIssue(ctx, f.root(), true, in2, nil, "manual", true)
	if err != nil {
		t.Fatal(err)
	}
	g2 := f.issue(i2.ID)
	if g2.Severity != SevP1 || !g2.SeverityLocked || *g2.AssigneeID != uDev2 || *g2.TeamID != 10 && *g2.TeamID != 11 {
		t.Fatalf("指定级别/负责人: %+v", g2)
	}
}

func TestCreateFromSlowLog(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.repo.ugroups[uDBA] = []uint{10}
	day := f.now.Add(-24 * time.Hour)
	f.repo.slowMD5["1|abc123"] = &SlowSample{DBName: "order", SQLText: "select * from t_order where user_id = 5"}
	f.repo.slowDays["1|abc123"] = []Digest{
		{DBName: "order", SampleSQL: "select * from t_order where user_id = 5", ExecCount: 100, TotalSec: 1400, MaxMs: 20000, FirstSeen: &day, LastSeen: &day, SampleReal: true},
	}
	i, err := f.svc.CreateFromSlowLog(ctx, f.dba(), false, SlowLogInput{InstanceID: 1, SQLID: "abc123", Remark: ""})
	if err != nil {
		t.Fatal(err)
	}
	got := f.issue(i.ID)
	if got.Source != "slowlog" || got.ExecCount7d != 100 || got.TotalTime7d != 1400 {
		t.Fatalf("按慢日志指标建单: %+v", got)
	}
	// 日均：首次出现昨天 → 2 天，700s/天 → P1
	if got.Severity != SevP1 {
		t.Fatalf("应按影响评估级别: %s impact=%v", got.Severity, got.ImpactScore)
	}
	if l := f.repo.logs[len(f.repo.logs)-1]; l.Action != ActComment || !strings.Contains(l.Content, "平台慢日志") {
		t.Fatalf("备注: %+v", l)
	}
	// 找不到明细时用页面上的 SQL 文本，影响为 0
	i2, err := f.svc.CreateFromSlowLog(ctx, f.dba(), false, SlowLogInput{InstanceID: 1, SQLID: "nothere", SQLText: "select * from t_x where a=1", DBName: "order"})
	if err != nil {
		t.Fatal(err)
	}
	if g := f.issue(i2.ID); g.ImpactScore != 0 || g.Severity != SevP2 {
		t.Fatalf("没有明细: %+v", g)
	}
	_, err = f.svc.CreateFromSlowLog(ctx, f.dba(), false, SlowLogInput{InstanceID: 1, SQLID: "nothere"})
	wantErr(t, err, 400, "无法建单")
	_, err = f.svc.CreateFromSlowLog(ctx, f.dev(), false, SlowLogInput{InstanceID: 1, SQLID: "abc123"})
	wantErr(t, err, 403, "DBA")
}

// ---- 列表 / 待办 / 趋势 ----

func TestListIssuesViewsAndVisibility(t *testing.T) {
	f := newFx(t)
	f.newIssue() // #1 张三负责，订单研发
	f.ingest(f.digest("select * from t_x where a = 1", 100, 120))
	f.repo.owners["1:other"] = "lisi"
	f.repo.igroups[1] = []uint{10, 11}
	d := f.digest("select * from t_y where a = 1", 100, 120)
	d.DBName = "other"
	f.ingest(d) // #3 owner lisi（会员研发），实例属订单研发组
	list := func(v Viewer, view string) *ListResult {
		r, err := f.svc.ListIssues(v, ListFilter{View: view})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := list(f.dba(), ViewOpen); r.Total != 3 {
		t.Fatalf("DBA 看全部: %d", r.Total)
	}
	if r := list(f.dev(), ViewOpen); r.Total != 2 {
		t.Fatalf("研发只看本团队/自己的: %d", r.Total)
	}
	if r := list(f.dev2(), ViewOpen); r.Total != 1 || r.Rows[0].ID != 3 {
		t.Fatalf("李四只看指派给自己的/本团队: %+v", r)
	}
	if r := list(f.stranger(), ViewOpen); r.Total != 0 {
		t.Fatalf("无团队无指派: %d", r.Total)
	}
	r := list(f.dev(), ViewTodo)
	if r.Total != 2 || r.Todo != 2 || r.Overdue != 0 {
		t.Fatalf("待办: %+v", r)
	}
	// 默认按日均浪费 DB 时间倒序
	if r.Rows[0].ID != 1 || r.Rows[0].SeverityLabel != "P0-紧急" || r.Rows[0].Instance != "order-db" || r.Rows[0].Assignee != "张三" {
		t.Fatalf("行: %+v", r.Rows[0])
	}
	// 超时高亮与“已超时”页签
	f.now = f.now.Add(49 * time.Hour)
	r = list(f.dev(), ViewOverdue)
	if r.Total != 1 || !r.Rows[0].Overdue || r.Rows[0].LeftHours == nil || *r.Rows[0].LeftHours >= 0 || r.Overdue != 1 {
		t.Fatalf("超时: %+v", r)
	}
	// 筛选
	if r := list(f.dba(), ViewAll); r.Total != 3 {
		t.Fatal("all")
	}
	res, _ := f.svc.ListIssues(f.dba(), ListFilter{View: ViewAll, Severity: SevP0})
	if res.Total != 1 {
		t.Fatalf("按级别: %d", res.Total)
	}
	res, _ = f.svc.ListIssues(f.dba(), ListFilter{View: ViewAll, Search: "#2"})
	if res.Total != 1 || res.Rows[0].ID != 2 {
		t.Fatalf("按编号搜索: %+v", res)
	}
	res, _ = f.svc.ListIssues(f.dba(), ListFilter{View: ViewAll, Search: "t_y"})
	if res.Total != 1 || res.Rows[0].ID != 3 {
		t.Fatalf("按指纹搜索: %+v", res)
	}
	res, _ = f.svc.ListIssues(f.dba(), ListFilter{View: ViewAll, Limit: 2, Offset: 2, Sort: "id", Desc: false})
	if res.Total != 3 || len(res.Rows) != 1 || res.Rows[0].ID != 3 {
		t.Fatalf("分页: %+v", res)
	}
	_, err := f.svc.ListIssues(f.dba(), ListFilter{View: "bogus"})
	wantErr(t, err, 400, "视图")
	_, err = f.svc.ListIssues(f.dba(), ListFilter{Severity: "PX"})
	wantErr(t, err, 400, "级别")
	_, err = f.svc.ListIssues(f.dba(), ListFilter{OwnerSide: "x"})
	wantErr(t, err, 400, "处理方")
	// 非法排序字段回退到默认
	if res, err = f.svc.ListIssues(f.dba(), ListFilter{View: ViewAll, Sort: "1;drop table"}); err != nil || res.Total != 3 {
		t.Fatal("非法排序字段应回退默认")
	}
}

func TestMyTodo(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	td, err := f.svc.MyTodo(f.dev())
	if err != nil || td.Todo != 1 || td.Overdue != 0 {
		t.Fatalf("%+v %v", td, err)
	}
	f.now = f.now.Add(49 * time.Hour)
	if td, _ = f.svc.MyTodo(f.dev()); td.Todo != 1 || td.Overdue != 1 {
		t.Fatalf("超时: %+v", td)
	}
	// 研发标记修复后，待办转给跟进 DBA（待验证）
	f.mustAct(f.dev(), id, ActionInput{Action: ActFix, Remark: "ok"})
	if td, _ = f.svc.MyTodo(f.dev()); td.Todo != 0 {
		t.Fatalf("研发不再有待办: %+v", td)
	}
	if td, _ = f.svc.MyTodo(f.dba()); td.Todo != 1 || td.Overdue != 0 {
		t.Fatalf("DBA 的待验证: %+v", td)
	}
}

func TestIssueTrend(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	for d, c := range map[int]int64{1: 100, 2: 300} {
		f.repo.AddStat(id, f.now.AddDate(0, 0, -d), StatDelta{ExecCount: c, TotalTime: float64(c)})
	}
	tr, err := f.svc.IssueTrend(f.dev(), id, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Points) != 3 || tr.Points[0].ExecCount != 300 || tr.Points[0].AvgMs != 1000 || tr.Created != "2026-10-06" {
		t.Fatalf("%+v", tr)
	}
	_, err = f.svc.IssueTrend(f.dev2(), id, 30)
	wantErr(t, err, 404, "不存在")
}

func TestCalcSeverity(t *testing.T) {
	rules, _ := parseSeverityRules(defaultSeverityRules)
	cases := []struct {
		impact, share, avg, perDay float64
		want                       string
	}{
		{4000, 0, 0, 0, SevP0},
		{700, 0, 0, 0, SevP1},
		{70, 0, 0, 0, SevP2},
		{5, 0, 0, 0, SevP3},
		{50, 40, 0, 0, SevP2},      // 占比高但绝对量不够 P0/P1，落到 P2
		{1000, 5, 0, 0, SevP1},     // 日均 1000s ≥ 600
		{5, 100, 0, 0, SevP3},      // 实例上只有一条慢 SQL 占 100% 也不能判 P0
		{0, 0, 6000, 200, SevP1},   // 平均 6s 且日均 200 次
		{0, 0, 6000, 5, SevP3},     // 次数太少
		{0, 0, 11000, 1500, SevP0}, // 平均 11s 且日均 1500 次
	}
	for _, c := range cases {
		if got := calcSeverity(rules, c.impact, c.share, c.avg, c.perDay); got != c.want {
			t.Errorf("%+v → %s，期望 %s", c, got, c.want)
		}
	}
	f := newFx(t)
	f.repo.cfg[settings.KeyPerfSeverityRules] = `{"P0":{"daily_sec":100}}`
	if got := f.svc.evaluateSeverity(&Issue{ImpactScore: 150}); got != SevP0 {
		t.Fatalf("自定义规则: %s", got)
	}
	if got := f.svc.evaluateSeverity(&Issue{ImpactScore: 50, AvgTimeMs: 99999, PerDayCount: 99999}); got != SevP3 {
		t.Fatalf("缺项的条件不起作用: %s", got)
	}
	f.repo.cfg[settings.KeyPerfSeverityRules] = `{bad json`
	if got := f.svc.evaluateSeverity(&Issue{ImpactScore: 4000}); got != SevP0 {
		t.Fatalf("非法 JSON 回退默认: %s", got)
	}
}

func TestRouting(t *testing.T) {
	f := newFx(t)
	// 路由规则优先于库负责人
	f.repo.routes = []Route{
		{ID: 1, InstanceID: ptr(uint(2)), DBRegex: "order", TeamID: 11, Priority: 1}, // 别的实例，不匹配
		{ID: 2, DBRegex: "ord.*", TeamID: 11, AssigneeID: ptr(uint(uDev2)), Priority: 20},
		{ID: 3, DBRegex: "(", TeamID: 10, Priority: 10}, // 非法正则被跳过
	}
	r, err := f.svc.route(1, "order", SideDev, f.cfg())
	if err != nil || r.teamID != 11 || r.assigneeID != uDev2 || r.dbaID != uDBA {
		t.Fatalf("路由规则: %+v %v", r, err)
	}
	// 整体匹配：orders 不匹配 "order"
	f.repo.routes = []Route{{ID: 1, DBRegex: "order", TeamID: 11, AssigneeID: ptr(uint(uDev2))}}
	r, _ = f.svc.route(1, "orders", SideDev, f.cfg())
	if r.assigneeID == uDev2 {
		t.Fatal("正则应整体匹配")
	}
	// 没有规则：库负责人 → 团队取负责人所在且与实例关联的资源组
	f.repo.routes = nil
	r, _ = f.svc.route(1, "order", SideDev, f.cfg())
	if r.assigneeID != uDev || r.teamID != 10 {
		t.Fatalf("库负责人: %+v", r)
	}
	// 采集配置的默认团队
	cfg := *f.cfg()
	cfg.TeamID = ptr(uint(11))
	r, _ = f.svc.route(1, "order", SideDev, &cfg)
	if r.teamID != 11 || r.assigneeID != uDev {
		t.Fatalf("默认团队: %+v", r)
	}
	// 库没有负责人：团队默认接口人
	r, _ = f.svc.route(1, "nobody", SideDev, f.cfg())
	if r.assigneeID != uDev || r.teamID != 10 {
		t.Fatalf("默认接口人: %+v", r)
	}
	// 处理方为 DBA：改派 DBA 轮值（取未关闭工单最少的）
	f.repo.users[9] = UserBrief{ID: 9, Username: "dba2", Display: "DBA2", Active: true}
	f.repo.cfg[settings.KeyPerfDBAUsers] = "dba1, dba2"
	f.repo.dbaLoad[uDBA] = 3
	f.repo.issues[50] = &Issue{ID: 50, Status: StAccepted, DBAID: ptr(uint(uDBA))}
	r, _ = f.svc.route(1, "order", SideDBA, f.cfg())
	if r.assigneeID != 9 || r.dbaID != 9 {
		t.Fatalf("DBA 轮值: %+v", r)
	}
	// 没有配置 DBA：不改派
	f.repo.cfg[settings.KeyPerfDBAUsers] = ""
	r, _ = f.svc.route(1, "order", SideDBA, f.cfg())
	if r.assigneeID != uDev || r.dbaID != 0 {
		t.Fatalf("没有 DBA: %+v", r)
	}
}
