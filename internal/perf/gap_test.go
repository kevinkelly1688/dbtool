// @author: kevinkelly
// @license: Apache Licence
// @file: gap_test.go
// @time: 2026/10/28

package perf

import (
	"context"
	"strings"
	"testing"
)

// ---- S6-B1：解决截止日已过时受理 ----

func TestAcceptAfterDeadlinePassed(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	i := f.issue(id)
	past := f.now.AddDate(0, 0, -3)
	i.ResolveDeadline = &past
	if err := f.repo.UpdateIssue(i, i.UpdateTime); err != nil {
		t.Fatal(err)
	}
	// 详情给前端的上限：截止日已过时最晚取今天
	d, err := f.svc.GetIssue(f.dev(), id)
	if err != nil {
		t.Fatal(err)
	}
	if d.MaxPlanDate != f.now.Format(dateLayout) {
		t.Fatalf("逾期工单的承诺日期上限应为今天: %q", d.MaxPlanDate)
	}
	tomorrow := f.now.AddDate(0, 0, 1).Format(dateLayout)
	wantErr(t, mustSecond(f.act(f.dev(), id, ActionInput{Action: ActAccept, PlanFinishDate: tomorrow})), 400, "不能晚于今天")
	f.mustAct(f.dev(), id, ActionInput{Action: ActAccept, PlanFinishDate: f.now.Format(dateLayout)})
	got := f.issue(id)
	if got.Status != StAccepted || got.PlanFinishDate == nil || got.PlanFinishDate.Format(dateLayout) != f.now.Format(dateLayout) {
		t.Fatalf("受理: %+v", got)
	}
	logs, _ := f.repo.ListLogs(id, 5)
	if !strings.Contains(logs[0].Content, "已超时受理") {
		t.Fatalf("逾期受理应在日志里标注: %q", logs[0].Content)
	}
}

func TestAcceptBeforeDeadlineKeepsOldRule(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	f.accept(id)
	logs, _ := f.repo.ListLogs(id, 5)
	if strings.Contains(logs[0].Content, "已超时受理") {
		t.Fatalf("未逾期不应标注: %q", logs[0].Content)
	}
}

// ---- S6-C11：采集阈值必须大于 0 ----

func TestCollectThresholdsMustBePositive(t *testing.T) {
	f := newFx(t)
	v := f.dba()
	base := CollectConfigInput{InstanceID: 2, Product: ProdSelectDB, Collector: CollDorisAudit, MinAvgMs: 3000, MinTotalSec: 30, Enabled: true}
	for name, mod := range map[string]func(*CollectConfigInput){
		"avg=0":   func(c *CollectConfigInput) { c.MinAvgMs = 0 },
		"total=0": func(c *CollectConfigInput) { c.MinTotalSec = 0 },
	} {
		in := base
		mod(&in)
		if _, err := f.svc.SaveCollectConfig(v, in); err == nil || httpOf(err) != 400 || !strings.Contains(err.Error(), "大于 0") {
			t.Errorf("%s 应 400 且提示大于 0: %v", name, err)
		}
	}
	if _, err := f.svc.SaveCollectConfig(v, base); err != nil {
		t.Fatalf("正常值: %v", err)
	}
}

// ---- S6-A1：不能审批自己提交的延期 / 豁免申请 ----

func TestCannotApproveOwnExtend(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	f.mustAct(f.dba(), id, ActionInput{Action: ActSeverity, Severity: SevP1})
	f.mustAct(f.dba(), id, ActionInput{Action: ActExtend, ExtendDate: "2026-10-20", Remark: "依赖的库表迁移要到下周"}) // DBA 自己申请
	wantErr(t, mustSecond(f.act(f.dba(), id, ActionInput{Action: ActExtendApprove})), 403, "不能审批自己")
	d, _ := f.svc.GetIssue(f.dba(), id)
	if hasAction(d.Actions, ActExtendApprove) || !hasAction(d.Actions, ActExtendReject) {
		t.Fatalf("申请人看不到“同意延期”，仍可驳回（撤回）自己的申请: %+v", d.Actions)
	}
	// 另一个管理员（超级用户）可以审批
	d, _ = f.svc.GetIssue(f.root(), id)
	if !hasAction(d.Actions, ActExtendApprove) {
		t.Fatalf("别人应能审批: %+v", d.Actions)
	}
	f.mustAct(f.root(), id, ActionInput{Action: ActExtendApprove})
	if f.issue(id).ExtendTimes != 1 {
		t.Fatal("延期应已生效")
	}
}

func TestCannotApproveOwnWaive(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	f.mustAct(f.dba(), id, ActionInput{Action: ActWaive, Remark: "月度报表低频查询，业务可接受（每月 1 日跑一次）"})
	wantErr(t, mustSecond(f.act(f.dba(), id, ActionInput{Action: ActWaiveApprove})), 403, "不能审批自己")
	if f.issue(id).Status != StWaivePending {
		t.Fatal("状态不应变化")
	}
	f.mustAct(f.root(), id, ActionInput{Action: ActWaiveApprove, Remark: "同意"})
	if f.issue(id).Status != StClosed {
		t.Fatal("别人同意后应关闭")
	}
}

func TestSuperuserCannotApproveOwnRequest(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	f.mustAct(f.root(), id, ActionInput{Action: ActWaive, Remark: "月度报表低频查询，业务可接受（每月 1 日跑一次）"})
	wantErr(t, mustSecond(f.act(f.root(), id, ActionInput{Action: ActWaiveApprove})), 403, "不能审批自己")
}

// 申请人取最近一次申请：DBA 的申请被驳回后，研发再申请，DBA 可以批。
func TestRequesterIsLatestRequest(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	f.mustAct(f.dba(), id, ActionInput{Action: ActWaive, Remark: "月度报表低频查询，业务可接受（每月 1 日跑一次）"})
	f.mustAct(f.root(), id, ActionInput{Action: ActWaiveReject, Remark: "不行"})
	f.mustAct(f.dev(), id, ActionInput{Action: ActWaive, Remark: "月度报表低频查询，业务可接受（每月 1 日跑一次）"})
	f.mustAct(f.dba(), id, ActionInput{Action: ActWaiveApprove})
	if f.issue(id).Status != StClosed {
		t.Fatal("应关闭")
	}
}

// ---- S6-C14：审计动作区分 ----

func (f *fx) findByFP(sql string) uint {
	f.t.Helper()
	i, _ := f.repo.FindIssue(1, "order", Checksum(Fingerprint(sql)))
	if i == nil {
		f.t.Fatalf("找不到 %q 的工单", sql)
	}
	return i.ID
}

func TestAuditDistinguishesCloseKinds(t *testing.T) {
	f := newFx(t)
	a := f.newIssue() // 验证通过
	f.accept(a)
	f.mustAct(f.dev(), a, ActionInput{Action: ActFix, Remark: "已上线"})
	f.mustAct(f.dba(), a, ActionInput{Action: ActVerifyPass})
	sqlB := "select * from t_member where a = 1" // 同意豁免
	f.ingest(f.digest(sqlB, 1000, 4000))
	b := f.findByFP(sqlB)
	f.mustAct(f.dev(), b, ActionInput{Action: ActWaive, Remark: "月度报表低频查询，业务可接受（每月 1 日跑一次）"})
	f.mustAct(f.dba(), b, ActionInput{Action: ActWaiveApprove})
	sqlC := "select * from t_goods where a = 1" // 误报关闭
	f.ingest(f.digest(sqlC, 1000, 4000))
	c := f.findByFP(sqlC)
	f.mustAct(f.dba(), c, ActionInput{Action: ActCloseDup, Remark: "重复"})
	acts := strings.Join(f.repo.auditActions(), ",")
	for _, want := range []string{"性能工单-验证通过", "性能工单-同意豁免", "性能工单-误报关闭"} {
		if !strings.Contains(acts, want) {
			t.Errorf("审计缺少 %q: %s", want, acts)
		}
	}
	if strings.Contains(acts, "性能工单-关闭") {
		t.Errorf("人工关闭不应再笼统记为“关闭”: %s", acts)
	}
	// 工单日志里仍是 close（时间线兼容）
	if got := f.repo.logActions(a); got[len(got)-1] != ActClose {
		t.Errorf("日志动作: %v", got)
	}
}

// ---- S6-B2：手工建单 / 重新诊断校验 SQL ----

func TestValidateSampleSQL(t *testing.T) {
	ok := []string{
		"select 1", "SELECT * FROM t WHERE a = ';'", "  update t set a=1 where id=2", "delete from t where id=1",
		"with x as (select 1) select * from x", "/* c */ select * from t -- x", "select 'a;b' as s, \"c;d\" as d",
	}
	for _, s := range ok {
		if err := validateSampleSQL(s); err != nil {
			t.Errorf("%q 应通过: %v", s, err)
		}
	}
	bads := []string{
		"select 1; select 2", "select 1;", "drop table t", "insert into t values (1)", "alter table t add c int", "truncate t",
		"call p()", "set @a=1", "explain select 1", "show tables", "update t set a=1; delete from t", "", "-- only comment",
		"select 1 /* ; */ ; select 2", "grant all on *.* to x",
	}
	for _, s := range bads {
		if err := validateSampleSQL(s); err == nil {
			t.Errorf("%q 应被拒绝", s)
		}
	}
}

func TestCreateAndRediagnoseRejectBadSQL(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.repo.ugroups[uDBA] = []uint{10}
	for _, sql := range []string{"select 1; drop table t_order", "drop table t_order", "insert into t_order values (1)"} {
		_, err := f.svc.CreateIssue(ctx, f.dba(), false, CreateInput{InstanceID: 1, DBName: "order", SQL: sql}, nil, "manual", true)
		wantErr(t, err, 400, "SQL")
	}
	id := f.newIssue()
	for _, sql := range []string{"select 1; drop table t_order", "delete from a; delete from b", "insert into t values (1)"} {
		_, err := f.svc.Rediagnose(ctx, f.dba(), id, sql)
		wantErr(t, err, 400, "SQL")
	}
	if got := f.issue(id).SampleSQL; got != sqlOrderByUser {
		t.Fatalf("被拒绝的 SQL 不能覆盖样例: %q", got)
	}
	if _, err := f.svc.Rediagnose(ctx, f.dba(), id, "select * from t_order where name like '%abc'"); err != nil {
		t.Fatalf("合法 SQL: %v", err)
	}
	// 空 SQL = 用已有样例重跑，不受影响
	if _, err := f.svc.Rediagnose(ctx, f.dba(), id, ""); err != nil {
		t.Fatalf("空 SQL: %v", err)
	}
}

// ---- S6-B3：详情按钮按细粒度按钮码裁剪 ----

func TestFilterActionsByButton(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	f.accept(id)
	f.mustAct(f.dev(), id, ActionInput{Action: ActFix, Remark: "ok"})
	d, _ := f.svc.GetIssue(f.dba(), id)
	if !hasAction(d.Actions, ActVerifyPass) || !hasAction(d.Actions, ActComment) || !hasAction(d.Actions, ActAssign) {
		t.Fatalf("前提: %+v", d.Actions)
	}
	only := func(codes ...string) func(string) bool {
		return func(c string) bool {
			for _, x := range codes {
				if x == c {
					return true
				}
			}
			return false
		}
	}
	got := filterActionsByButton(d.Actions, only("perf.comment"))
	if len(got) != 1 || got[0].Name != ActComment {
		t.Fatalf("只有 perf.comment: %+v", got)
	}
	got = filterActionsByButton(d.Actions, only("perf.verify", "perf.comment"))
	if hasAction(got, ActAssign) || !hasAction(got, ActVerifyPass) || !hasAction(got, ActVerifyFail) {
		t.Fatalf("perf.verify: %+v", got)
	}
	if got = filterActionsByButton(d.Actions, only()); len(got) != 0 {
		t.Fatalf("什么按钮都没有: %+v", got)
	}
	// 没有映射的动作保持原样（由服务层自己拒绝）
	if got = filterActionsByButton([]ActionDTO{{Name: "weird"}}, only()); len(got) != 1 {
		t.Fatalf("未登记的动作不裁剪: %+v", got)
	}
	// 重新诊断对应 perf.reopen
	if got = filterActionsByButton([]ActionDTO{{Name: ActDiagnose}}, only("perf.reopen")); len(got) != 1 {
		t.Fatalf("重新诊断按 perf.reopen: %+v", got)
	}
}

func TestHTTPDetailActionsFollowButtons(t *testing.T) {
	env, f, chk := newBtnEnv(t)
	id := f.newIssue()
	grantDBA(env, 2)
	chk.Grant(2, "perf.view", "perf.comment")
	bob := env.Login("bob")
	var d IssueDetail
	bob.GetJSON("/api/v1/perf/issues/"+itoaU(id), &d)
	if len(d.Actions) != 1 || d.Actions[0].Name != ActComment {
		t.Fatalf("只有评论按钮时详情只应返回评论: %+v", d.Actions)
	}
	chk.Grant(2, "perf.assign")
	bob.GetJSON("/api/v1/perf/issues/"+itoaU(id), &d)
	if !hasAction(d.Actions, ActAssign) {
		t.Fatalf("授予 perf.assign 后应出现指派: %+v", d.Actions)
	}
}

// ---- S6-B5：列表“实例”筛选的选项 ----

func TestOptionsFilterInstances(t *testing.T) {
	env, f := newHTTP(t)
	f.newIssue()
	grantDev(env, 2)
	bob := env.Login("bob")
	var o OptionsDTO
	bob.GetJSON("/api/v1/perf/options", &o)
	if len(o.FilterInstances) != 1 || o.FilterInstances[0].ID != 1 {
		t.Fatalf("研发只能按自己团队的实例筛选: %+v", o.FilterInstances)
	}
	if len(o.Instances) != 0 {
		t.Fatal("建单用的实例列表仍仅限 DBA")
	}
	root := env.Login("root")
	var o2 OptionsDTO
	root.GetJSON("/api/v1/perf/options", &o2)
	if len(o2.FilterInstances) != 2 {
		t.Fatalf("DBA 可按全部实例筛选: %+v", o2.FilterInstances)
	}
	// 按实例筛选生效
	if r := getList(t, root, "?view=all&instance_id=1"); r.Total != 1 {
		t.Fatalf("instance_id=1: %d", r.Total)
	}
	if r := getList(t, root, "?view=all&instance_id=2"); r.Total != 0 {
		t.Fatalf("instance_id=2: %d", r.Total)
	}
}
