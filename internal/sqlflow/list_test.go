// @author: kevinkelly
// @license: Apache Licence
// @file: list_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine"
	"dbtool/internal/goinception"
	"dbtool/internal/model"
	"dbtool/internal/review"
	"dbtool/internal/settings"
	"dbtool/internal/sqlflow"
)

// ---- 列表：页签、统计、搜索、排序 ----

func TestListTabsAndStats(t *testing.T) {
	e := setup(t)
	id1 := e.submit(t, dev, "update t set a=1") // 待审核，轮到组长
	id2 := e.submit(t, dev, "update t set a=2")
	e.approve(t, id2) // 审核通过，可执行

	items, total, err := e.svc.List(leader, sqlflow.ListQuery{Tab: sqlflow.TabApprove})
	if err != nil || total != 1 || len(items) != 1 || items[0].ID != id1 {
		t.Fatalf("组长的待我审批: %+v %d %v", items, total, err)
	}
	if !items[0].WaitingForYour || items[0].Level != "第 1 / 2 级 · 组长" {
		t.Errorf("待审批行: %+v", items[0])
	}
	if _, total, _ = e.svc.List(root, sqlflow.ListQuery{Tab: sqlflow.TabApprove}); total != 1 {
		t.Errorf("超级用户能审批所有待审核工单: %d", total)
	}
	if items, total, _ = e.svc.List(dba, sqlflow.ListQuery{Tab: sqlflow.TabApprove}); total != 0 || len(items) != 0 {
		t.Errorf("DBA 还没轮到: %+v", items)
	}
	if _, total, _ = e.svc.List(dev, sqlflow.ListQuery{Tab: sqlflow.TabApprove}); total != 0 {
		t.Errorf("没有审批权限的人没有待审批: %d", total)
	}
	if items, total, _ = e.svc.List(dba, sqlflow.ListQuery{Tab: sqlflow.TabExec}); total != 1 || items[0].ID != id2 || !items[0].CanBeExecuted {
		t.Errorf("DBA 待执行: %+v", items)
	}
	if _, total, _ = e.svc.List(dev, sqlflow.ListQuery{Tab: sqlflow.TabExec}); total != 1 {
		t.Errorf("提交人有 sql_execute，自己的审核通过工单待他执行: %d", total)
	}
	if _, total, _ = e.svc.List(leader, sqlflow.ListQuery{Tab: sqlflow.TabExec}); total != 0 {
		t.Errorf("组长没有执行权限: %d", total)
	}
	if _, total, _ = e.svc.List(dev, sqlflow.ListQuery{Tab: sqlflow.TabMine}); total != 2 {
		t.Errorf("我提交的: %d", total)
	}
	if _, total, _ = e.svc.List(leader, sqlflow.ListQuery{Tab: sqlflow.TabMine}); total != 0 {
		t.Errorf("组长没提交过: %d", total)
	}

	st, err := e.svc.Stats(leader, "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 2 || st.ByStatus[model.WfManReviewing] != 1 || st.ByStatus[model.WfReviewPass] != 1 || st.Mine != 0 || st.Approve != 1 || st.Exec != 0 {
		t.Errorf("统计: %+v", st)
	}
	if st, _ = e.svc.Stats(dev, "export"); st.Total != 0 {
		t.Errorf("没有导出工单: %+v", st)
	}
	td, err := e.svc.TodoCounts(leader)
	if err != nil || td.Approve != 1 || td.Exec != 0 {
		t.Errorf("待办数: %+v %v", td, err)
	}
	// 审批走完：组长没有待审批了
	if _, err := e.svc.Pass(leader, id1, ""); err != nil {
		t.Fatal(err)
	}
	if td, _ = e.svc.TodoCounts(leader); td.Approve != 0 {
		t.Errorf("组长已审完: %+v", td)
	}
	if td, _ = e.svc.TodoCounts(dba); td.Approve != 1 || td.Exec != 1 {
		t.Errorf("DBA: 待审 id1，待执行 id2: %+v", td)
	}
}

func TestListSearchSortAndIDs(t *testing.T) {
	e := setup(t)
	mk := func(name string) uint {
		r := req("update t set a=1")
		r.Name = name
		res, err := e.svc.Submit(context.Background(), dev, r)
		if err != nil {
			t.Fatal(err)
		}
		return res.WorkflowID
	}
	a, b, c := mk("B 表加索引"), mk("A 表改字段"), mk("C 表清理")
	ids := func(q sqlflow.ListQuery) []uint {
		items, _, err := e.svc.List(root, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []uint
		for _, it := range items {
			out = append(out, it.ID)
		}
		return out
	}
	eq := func(got []uint, want ...uint) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	if got := ids(sqlflow.ListQuery{}); !eq(got, c, b, a) {
		t.Errorf("默认按工单号倒序: %v", got)
	}
	// 名称：A 表(b) < B 表(a) < C 表(c)
	if got := ids(sqlflow.ListQuery{OrderBy: "name", Desc: false}); !eq(got, b, a, c) {
		t.Errorf("名称升序: %v", got)
	}
	if got := ids(sqlflow.ListQuery{OrderBy: "name", Desc: true}); !eq(got, c, a, b) {
		t.Errorf("名称倒序: %v", got)
	}
	if got := ids(sqlflow.ListQuery{OrderBy: "id", Desc: false}); !eq(got, a, b, c) {
		t.Errorf("编号升序: %v", got)
	}
	if got := ids(sqlflow.ListQuery{OrderBy: "id); drop table x;--", Desc: false}); !eq(got, c, b, a) {
		t.Errorf("非法排序键回到默认: %v", got)
	}
	if sqlflow.NormalizeOrder("create_time") != "create_time" || sqlflow.NormalizeOrder("x") != "" {
		t.Error("排序键白名单")
	}
	// 搜索：工单号、#工单号、名称、提交人登录名、库、实例名
	if got := ids(sqlflow.ListQuery{Search: "#" + strconv.Itoa(int(b))}); !eq(got, b) {
		t.Errorf("按 #工单号: %v", got)
	}
	if got := ids(sqlflow.ListQuery{Search: strconv.Itoa(int(c))}); !eq(got, c) {
		t.Errorf("按工单号: %v", got)
	}
	if got := ids(sqlflow.ListQuery{Search: "清理"}); !eq(got, c) {
		t.Errorf("按名称: %v", got)
	}
	for _, kw := range []string{"dev", "shop", "prod-mysql"} {
		if got := ids(sqlflow.ListQuery{Search: kw}); len(got) != 3 {
			t.Errorf("按 %q 应命中全部: %v", kw, got)
		}
	}
	if got := ids(sqlflow.ListQuery{Search: "不存在的关键字"}); len(got) != 0 {
		t.Errorf("无匹配: %v", got)
	}
	if got := ids(sqlflow.ListQuery{Status: model.WfManReviewing, InstanceID: 1, GroupID: 1}); len(got) != 3 {
		t.Errorf("实例 / 资源组 / 状态筛选: %v", got)
	}
	if got := ids(sqlflow.ListQuery{InstanceID: 2}); len(got) != 0 {
		t.Errorf("别的实例: %v", got)
	}
}

func TestListDecoratesOSCAndSchedule(t *testing.T) {
	e := setup(t)
	e.useInception()
	e.inc.checkOut = inceptionRows("alter table t add c int") // goInception 给出 sqlsha1：需要在线改表
	id := e.submit(t, dev, "alter table t add c int")
	items, _, _ := e.svc.List(dev, sqlflow.ListQuery{})
	if !items[0].OSC {
		t.Errorf("OSC 标签: %+v", items[0])
	}
	e.approve(t, id)
	at := e.now.Add(2 * time.Hour)
	if _, err := e.svc.Schedule(dba, id, at, ""); err != nil {
		t.Fatal(err)
	}
	items, _, _ = e.svc.List(dev, sqlflow.ListQuery{})
	if items[0].ScheduleAt == nil || !items[0].ScheduleAt.Equal(at) || items[0].Level != "2 级审批完成" {
		t.Errorf("定时 / 审批级: %+v", items[0])
	}
}

// ---- 审批流预览 ----

func TestFlowPreview(t *testing.T) {
	e := setup(t)
	f, err := e.svc.FlowPreview(dev, 1)
	if err != nil || !f.Configured || f.Auto || len(f.Levels) != 2 {
		t.Fatalf("预览: %+v %v", f, err)
	}
	if f.Levels[0].GroupName != "组长" || len(f.Levels[0].Approvers) != 1 || f.Levels[0].Approvers[0].Username != "leader" {
		t.Errorf("第 1 级只应有资源组内的组长（outsider 在别的资源组）: %+v", f.Levels[0])
	}
	if f.Levels[1].GroupName != "DBA" || len(f.Levels[1].Approvers) != 1 || f.Levels[1].Approvers[0].Username != "dba" || len(f.Warnings) != 0 {
		t.Errorf("第 2 级: %+v %v", f.Levels[1], f.Warnings)
	}

	// 禁止自审：组长自己提交时，第 1 级没有别的人可审
	e.repo.Cfg[settings.KeyApprovalForbidSelf] = "true"
	f, err = e.svc.FlowPreview(leader, 1)
	if err != nil || f.Levels[0].Warning == "" || len(f.Levels[0].Approvers) != 0 || len(f.Warnings) != 1 {
		t.Errorf("自审被禁止: %+v %v", f, err)
	}
	delete(e.repo.Cfg, settings.KeyApprovalForbidSelf)

	// 权限组被删：本级无人可审
	delete(e.repo.Groups, 20)
	f, _ = e.svc.FlowPreview(dev, 1)
	if f.Levels[1].Warning == "" || !strings.Contains(f.Levels[1].Warning, "不存在") {
		t.Errorf("权限组不存在: %+v", f.Levels[1])
	}
	e.repo.Groups[20] = &model.AuthGroup{ID: 20, Name: "DBA"}

	// 不在资源组内
	if _, err := e.svc.FlowPreview(outsider, 1); code(err) != "forbidden" {
		t.Errorf("不在组内: %v", err)
	}
	// 没配置审批流 / 空审批流
	e.repo.RGs = append(e.repo.RGs, model.ResourceGroup{GroupID: 3, GroupName: "新组"}, model.ResourceGroup{GroupID: 4, GroupName: "空流程组"})
	e.repo.UserRGs[1] = []uint{1, 3, 4}
	e.repo.Settings[[2]int{4, 2}] = &model.WorkflowAuditSetting{GroupID: 4, WorkflowType: 2, AuditAuthGroups: ""}
	if f, _ = e.svc.FlowPreview(dev, 3); f.Configured || len(f.Warnings) != 1 {
		t.Errorf("未配置: %+v", f)
	}
	if f, _ = e.svc.FlowPreview(dev, 4); !f.Configured || !f.Auto || len(f.Levels) != 0 {
		t.Errorf("空流程: %+v", f)
	}
}

// ---- 详情：审批时间线 ----

func TestDetailStepsAndApprovers(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update t set a=1")
	d, err := e.svc.Get(dev, id)
	if err != nil || len(d.Steps) != 3 {
		t.Fatalf("时间线: %+v %v", d.Steps, err)
	}
	if d.Steps[0].Title != "提交" || d.Steps[0].State != sqlflow.StepDone || d.Steps[0].Who != "开发者" {
		t.Errorf("提交: %+v", d.Steps[0])
	}
	if d.Steps[1].State != sqlflow.StepCurrent || d.Steps[1].Title != "第 1 级 · 组长" || len(d.Steps[1].Approvers) != 1 || d.Steps[1].Approvers[0].Username != "leader" {
		t.Errorf("当前级: %+v", d.Steps[1])
	}
	if d.Steps[2].State != sqlflow.StepPending || len(d.CurrentApprovers) != 1 {
		t.Errorf("下一级 / 当前审批人: %+v %+v", d.Steps[2], d.CurrentApprovers)
	}
	// 还没轮到的级别也带着“能审批的人”（页面只在鼠标悬停在审批组上时显示）；提交 / 当前级的提示文字
	if len(d.Steps[2].Approvers) != 1 || d.Steps[2].Approvers[0].Username != "dba" || d.Steps[2].NoApprover || !strings.HasPrefix(d.Steps[2].Tip(), "可审批人员（1）：") {
		t.Errorf("下一级也应带可审批人员: %+v tip=%q", d.Steps[2], d.Steps[2].Tip())
	}
	if d.Steps[0].Tip() != "" || d.Steps[1].Tip() == "" || d.Steps[1].NoApprover {
		t.Errorf("提交没有提示、当前级有提示: %q %q", d.Steps[0].Tip(), d.Steps[1].Tip())
	}
	// 下一级的权限组被删：这一级标“无人可审”，提示改为原因；已经结束的级别不标
	delete(e.repo.Groups, 20)
	d, _ = e.svc.Get(dev, id)
	if !d.Steps[2].NoApprover || !strings.Contains(d.Steps[2].Tip(), "没有符合条件的审批人") || d.Steps[1].NoApprover {
		t.Errorf("无人可审: %+v tip=%q", d.Steps[2], d.Steps[2].Tip())
	}
	e.repo.Groups[20] = &model.AuthGroup{ID: 20, Name: "DBA"}

	e.svc.Pass(leader, id, "方案没问题, 同意")
	d, _ = e.svc.Get(dev, id)
	if d.Steps[1].State != sqlflow.StepDone || d.Steps[1].Who != "组长" || d.Steps[1].Remark != "方案没问题, 同意" || d.Steps[1].Time == nil {
		t.Errorf("第 1 级已通过: %+v", d.Steps[1])
	}
	if d.Steps[2].State != sqlflow.StepCurrent || d.CurrentApprovers[0].Username != "dba" {
		t.Errorf("轮到 DBA: %+v %+v", d.Steps[2], d.CurrentApprovers)
	}

	// 第 2 级驳回
	e.svc.Cancel(dba, id, "索引重复")
	d, _ = e.svc.Get(dev, id)
	if d.Steps[2].State != sqlflow.StepRejected || d.Steps[2].Who != "DBA" || d.Steps[2].Remark != "索引重复" || len(d.CurrentApprovers) != 0 {
		t.Errorf("驳回: %+v", d.Steps[2])
	}

	// 提交人撤回：停在第 1 级
	id2 := e.submit(t, dev, "update t set a=2")
	e.svc.Cancel(dev, id2, "写错了")
	d, _ = e.svc.Get(dev, id2)
	if d.Steps[1].State != sqlflow.StepAborted || d.Steps[1].Remark != "写错了" || d.Steps[2].State != sqlflow.StepSkipped {
		t.Errorf("撤回: %+v", d.Steps)
	}

	// 审批走完后又被驳回：补一个终止步骤
	id3 := e.submit(t, dev, "update t set a=3")
	e.approve(t, id3)
	e.svc.Cancel(dba, id3, "窗口不对")
	d, _ = e.svc.Get(dev, id3)
	last := d.Steps[len(d.Steps)-1]
	if len(d.Steps) != 4 || last.Title != "审批通过后被驳回" || last.Remark != "窗口不对" {
		t.Errorf("通过后驳回: %+v", d.Steps)
	}
	if d.Steps[2].State != sqlflow.StepDone || d.Steps[2].Remark != "ok" {
		t.Errorf("通过的级: %+v", d.Steps[2])
	}
}

func TestDetailStepsAutoApproved(t *testing.T) {
	e := setup(t)
	e.repo.Settings[[2]int{1, 2}].AuditAuthGroups = ""
	id := e.submit(t, dev, "update t set a=1")
	d, err := e.svc.Get(dev, id)
	if err != nil || len(d.Steps) != 2 || d.Steps[1].State != sqlflow.StepAuto || !strings.Contains(d.Steps[1].Remark, "无需审批") {
		t.Errorf("自动通过: %+v %v", d.Steps, err)
	}
}

// ---- 复制为新工单 ----

func TestCopySource(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update t set a=1")
	if _, err := e.svc.CopySource(dev, id); code(err) != "conflict" {
		t.Errorf("审批中的工单不能复制: %v", err)
	}
	if _, err := e.svc.Cancel(dev, id, "撤回重提"); err != nil {
		t.Fatal(err)
	}
	c, err := e.svc.CopySource(dev, id)
	if err != nil || c.Name != "给 orders 加索引" || c.InstanceID != 1 || c.DBName != "shop" || c.GroupName != "研发组" || c.SQL != "update t set a=1" || c.IsExport {
		t.Errorf("带出内容: %+v %v", c, err)
	}
	if _, err := e.svc.CopySource(stranger, id); code(err) != "not_found" {
		t.Errorf("看不到的工单: %v", err)
	}
	if _, err := e.svc.CopySource(dev, 999); code(err) != "not_found" {
		t.Errorf("不存在: %v", err)
	}
	d, _ := e.svc.Get(dev, id)
	if !d.CanCopy {
		t.Error("终态工单提交人可以复制")
	}
	d2, _ := e.svc.Get(dev, e.submit(t, dev, "update t set a=2"))
	if d2.CanCopy {
		t.Error("审批中的不显示复制")
	}
}

// ---- 取消定时 ----

func TestCancelSchedule(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update t set a=1")
	if _, err := e.svc.CancelSchedule(dev, id); code(err) != "conflict" {
		t.Errorf("没在定时: %v", err)
	}
	e.approve(t, id)
	if _, err := e.svc.Schedule(dba, id, e.now.Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	d, _ := e.svc.Get(dev, id)
	if !d.CanCancelSchedule || d.Schedule == nil {
		t.Errorf("详情: %+v", d.CanCancelSchedule)
	}
	if _, err := e.svc.CancelSchedule(stranger, id); code(err) != "not_found" {
		t.Errorf("看不到的工单: %v", err)
	}
	if _, err := e.svc.CancelSchedule(leader, id); code(err) != "forbidden" {
		t.Errorf("既不是提交人也没有执行权限: %v", err)
	}
	if _, err := e.svc.CancelSchedule(dev, id); err != nil {
		t.Fatal(err)
	}
	if e.status(id) != model.WfReviewPass || len(e.repo.Schedules) != 0 {
		t.Errorf("取消后: %s %v", e.status(id), e.repo.Schedules)
	}
	last := e.repo.Logs[len(e.repo.Logs)-1]
	if last.OperationTypeDesc != "取消定时" || last.Operator != "dev" {
		t.Errorf("日志: %+v", last)
	}
	// 取消后可以重新设置定时或立即执行
	if _, err := e.svc.Schedule(dev, id, e.now.Add(2*time.Hour), ""); err != nil {
		t.Errorf("重新定时: %v", err)
	}
}

// ---- 提交时设定定时 ----

func planReq(at time.Time) sqlflow.SubmitReq {
	r := req("update t set a=1")
	r.PlanAt = &at
	return r
}

func TestSubmitWithPlan(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	at := e.now.Add(3 * time.Hour)
	res, err := e.svc.Submit(ctx, dev, planReq(at))
	if err != nil || !strings.Contains(res.Message, "自动定时执行") {
		t.Fatalf("提交: %+v %v", res, err)
	}
	id := res.WorkflowID
	sc := e.repo.Schedules[id]
	if sc.WorkflowID != id || !sc.RunAt.Equal(at) || e.status(id) != model.WfManReviewing {
		t.Fatalf("计划: %+v 状态 %s", sc, e.status(id))
	}
	d, _ := e.svc.Get(dev, id)
	if d.Schedule == nil || !d.Schedule.RunAt.Equal(at) {
		t.Errorf("待审批的工单详情显示计划: %+v", d.Schedule)
	}
	// 调度器对待审批的工单不动它
	e.svc.SetClock(func() time.Time { return e.now })
	if n := e.svc.RunDue(); n != 0 || len(e.repo.Schedules) != 1 {
		t.Errorf("没到点: %d %v", n, e.repo.Schedules)
	}
	e.svc.Pass(leader, id, "")
	if e.status(id) != model.WfManReviewing {
		t.Fatalf("还有一级: %s", e.status(id))
	}
	e.cap.reset()
	e.svc.Pass(dba, id, "")
	if e.status(id) != model.WfTimingTask || len(e.repo.Schedules) != 1 {
		t.Fatalf("最后一级通过后转入定时: %s %v", e.status(id), e.repo.Schedules)
	}
	names := strings.Join(e.cap.names(), ",")
	if !strings.Contains(names, "scheduled") && !strings.Contains(names, "Scheduled") && len(e.cap.msgs) < 2 {
		t.Errorf("应通知审批通过与已设置定时: %v", names)
	}
	logs := e.repo.Logs[len(e.repo.Logs)-1]
	if logs.OperationTypeDesc != "定时执行" || !strings.Contains(logs.OperationInfo, "提交时设定") {
		t.Errorf("日志: %+v", logs)
	}
	// 到点执行
	e.svc.SetClock(func() time.Time { return at.Add(time.Minute) })
	if n := e.svc.RunDue(); n != 1 || e.status(id) != model.WfFinish {
		t.Errorf("到点执行: n=%d status=%s", n, e.status(id))
	}
}

func TestSubmitWithPlanValidation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	past := e.now.Add(-time.Hour)
	if _, err := e.svc.Submit(ctx, dev, planReq(past)); code(err) != "bad_request" {
		t.Errorf("过去的时间: %v", err)
	}
	r := planReq(e.now.Add(5 * time.Hour))
	st, en := e.now.Add(time.Hour), e.now.Add(2*time.Hour)
	r.RunStart, r.RunEnd = &st, &en
	if _, err := e.svc.Submit(ctx, dev, r); code(err) != "bad_request" {
		t.Errorf("超出可执行时间范围: %v", err)
	}
	e.repo.NoSchedTb = true
	if _, err := e.svc.Submit(ctx, dev, planReq(e.now.Add(time.Hour))); code(err) != "schedule_table" {
		t.Errorf("没有计划表: %v", err)
	}
	e.repo.NoSchedTb = false
	if len(e.repo.Workflows) != 0 {
		t.Errorf("校验失败不应留下工单: %d", len(e.repo.Workflows))
	}
}

func TestPlanExpiredBeforeApprovalAndCancel(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	at := e.now.Add(time.Hour)
	res, _ := e.svc.Submit(ctx, dev, planReq(at))
	id := res.WorkflowID
	// 审批时计划时间已过：保持审核通过，计划作废
	e.svc.SetClock(func() time.Time { return at.Add(time.Minute) })
	e.approve(t, id)
	if e.status(id) != model.WfReviewPass || len(e.repo.Schedules) != 0 {
		t.Errorf("计划已过: %s %v", e.status(id), e.repo.Schedules)
	}
	if l := e.repo.Logs[len(e.repo.Logs)-1]; !strings.Contains(l.OperationInfo, "已过") {
		t.Errorf("日志: %+v", l)
	}

	// 调度器遇到到点仍在审批的工单：计划作废并记日志
	e.svc.SetClock(func() time.Time { return e.now })
	res, _ = e.svc.Submit(ctx, dev, planReq(e.now.Add(time.Hour)))
	id2 := res.WorkflowID
	e.svc.SetClock(func() time.Time { return e.now.Add(2 * time.Hour) })
	if n := e.svc.RunDue(); n != 0 || len(e.repo.Schedules) != 0 || e.status(id2) != model.WfManReviewing {
		t.Errorf("审批中到点: n=%d %v %s", n, e.repo.Schedules, e.status(id2))
	}
	if l := e.repo.Logs[len(e.repo.Logs)-1]; !strings.Contains(l.OperationInfo, "计划已作废") {
		t.Errorf("日志: %+v", l)
	}

	// 终止工单时一并清掉计划
	e.svc.SetClock(func() time.Time { return e.now })
	res, _ = e.svc.Submit(ctx, dev, planReq(e.now.Add(time.Hour)))
	if len(e.repo.Schedules) != 1 {
		t.Fatal("应有计划")
	}
	e.svc.Cancel(dev, res.WorkflowID, "不要了")
	if len(e.repo.Schedules) != 0 {
		t.Errorf("终止后计划应删除: %v", e.repo.Schedules)
	}
}

func TestSubmitWithPlanAutoPass(t *testing.T) {
	e := setup(t)
	e.repo.Settings[[2]int{1, 2}].AuditAuthGroups = "" // 空审批流：直接通过
	at := e.now.Add(time.Hour)
	res, err := e.svc.Submit(context.Background(), dev, planReq(at))
	if err != nil || e.status(res.WorkflowID) != model.WfTimingTask || !strings.Contains(res.Message, "定时执行") {
		t.Fatalf("无需审批 + 计划: %+v %v 状态 %s", res, err, e.status(res.WorkflowID))
	}
	if sc := e.repo.Schedules[res.WorkflowID]; !sc.RunAt.Equal(at) {
		t.Errorf("计划: %+v", sc)
	}
}

// ---- 回滚语句的访问审计 ----

func TestRollbackAudited(t *testing.T) {
	e := setup(t)
	e.useInception()
	e.inc.checkOut = inceptionRows("update t set a=1")
	id := e.submit(t, dev, "update t set a=1")
	e.approve(t, id)
	e.inc.execOut = []review.Row{{ID: 1, Stage: "EXECUTED", StageStatus: "Execute Successfully", ErrorMessage: "None", SQL: "update t set a=1", AffectedRows: 5, BackupDBName: "bk", SQLSha1: "s"}}
	e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto})
	e.svc.SetInception(nil, func(goinception.BackupConfig) sqlflow.BackupReader {
		return fakeBackup{items: []goinception.Rollback{{SQL: "update t set a=1", Rollback: "UPDATE t SET a=0;"}}}
	})
	ctx := context.Background()
	if _, items, err := e.svc.RollbackAudited(ctx, dev, id, sqlflow.RollbackView); err != nil || len(items) != 1 {
		t.Fatalf("查看: %v", err)
	}
	if _, _, err := e.svc.RollbackAudited(ctx, dev, id, sqlflow.RollbackDownload); err != nil {
		t.Fatal(err)
	}
	if len(e.repo.AuditEntries) != 2 || e.repo.AuditEntries[0].Action != "查看回滚语句" || e.repo.AuditEntries[1].Action != "下载回滚语句" ||
		e.repo.AuditEntries[0].UserName != "dev" || !strings.Contains(e.repo.AuditEntries[0].ExtraInfo, "工单id：") || !strings.Contains(e.repo.AuditEntries[0].ExtraInfo, "语句数：1") {
		t.Errorf("审计: %+v", e.repo.AuditEntries)
	}
	// 失败（没配备份库）也记；看不到的工单不记
	delete(e.repo.Cfg, settings.KeyGoInceptionBackupHost)
	if _, _, err := e.svc.RollbackAudited(ctx, dev, id, sqlflow.RollbackView); code(err) != "no_backup_db" {
		t.Fatalf("没配备份库: %v", err)
	}
	if n := len(e.repo.AuditEntries); n != 3 || !strings.Contains(e.repo.AuditEntries[2].ExtraInfo, "error:") {
		t.Errorf("失败也要记: %+v", e.repo.AuditEntries)
	}
	if _, _, err := e.svc.RollbackAudited(ctx, stranger, id, sqlflow.RollbackView); code(err) != "not_found" || len(e.repo.AuditEntries) != 3 {
		t.Errorf("看不到的工单不写审计: %v %d", err, len(e.repo.AuditEntries))
	}
}

// ---- 批量实例表补充信息 ----

func TestBatchInstanceViewsEnriched(t *testing.T) {
	be := setupBatch(t)
	id := submitBatch(t, be, 11, 12)
	be.hosts["ms12"].failOn = "Orders"
	execBatch(t, be, id)
	d, err := be.svc.Get(dev, id)
	if err != nil || len(d.BatchInstances) != 2 {
		t.Fatalf("批量实例: %+v %v", d, err)
	}
	var ok, bad *sqlflow.BatchInstanceView
	for i := range d.BatchInstances {
		if d.BatchInstances[i].ID == 11 {
			ok = &d.BatchInstances[i]
		} else {
			bad = &d.BatchInstances[i]
		}
	}
	if ok == nil || bad == nil || !strings.Contains(ok.Address, "ms11") || ok.DBType != "mssql" || ok.Note == "" || !bad.Failed || bad.Note == "" || bad.Note == ok.Note {
		t.Errorf("地址 / 类型 / 说明: ok=%+v bad=%+v", ok, bad)
	}
	if !strings.Contains(bad.Note, "Invalid object name") {
		t.Errorf("失败说明应是第一条错误信息: %q", bad.Note)
	}
}

// ---- 数据导出：独立列表、过滤、文件卡片、进度 ----

func TestExportListViewAndFileCard(t *testing.T) {
	xe := setupExport(t)
	ctx := context.Background()
	single, err := xe.svc.Submit(ctx, dev, xe.req("select id,name from orders", "csv", 1))
	if err != nil {
		t.Fatal(err)
	}
	batch, err := xe.svc.Submit(ctx, dev, xe.req("select id,name from orders", "csv", 11, 12))
	if err != nil {
		t.Fatal(err)
	}
	// 一张普通上线工单：不应出现在导出列表里
	xe.env.submit(t, dev, "update t set a=1")

	// 两种查看权限都没有：拒绝
	if _, _, err := xe.svc.ListExports(dev, sqlflow.ListQuery{}); code(err) != "forbidden" {
		t.Errorf("没有 export.view: %v", err)
	}
	if _, err := xe.svc.ExportStats(dev); code(err) != "forbidden" {
		t.Errorf("统计同样拒绝: %v", err)
	}
	xe.repo.Perms[1]["export.view"] = true
	items, total, err := xe.svc.ListExports(dev, sqlflow.ListQuery{})
	if err != nil || total != 1 || items[0].ID != single.WorkflowID || !items[0].IsExport {
		t.Fatalf("只有 export.view：只看单实例导出: %+v %v", items, err)
	}
	// 页面传 batch=1 也不能越过权限
	if _, total, _ = xe.svc.ListExports(dev, sqlflow.ListQuery{Batch: "1"}); total != 1 {
		t.Errorf("越权的 batch 参数: %d", total)
	}
	st, _ := xe.svc.ExportStats(dev)
	if st.Total != 1 || st.ByStatus[model.WfManReviewing] != 1 {
		t.Errorf("统计: %+v", st)
	}
	delete(xe.repo.Perms[1], "export.view")
	xe.repo.Perms[1]["export.view_batch"] = true
	if items, total, _ = xe.svc.ListExports(dev, sqlflow.ListQuery{}); total != 1 || items[0].ID != batch.WorkflowID || items[0].BatchCount != 2 {
		t.Errorf("只有 export.view_batch: %+v", items)
	}
	xe.repo.Perms[1]["export.view"] = true
	if _, total, _ = xe.svc.ListExports(dev, sqlflow.ListQuery{}); total != 2 {
		t.Errorf("两者都有: %d", total)
	}
	if items, _, _ = xe.svc.ListExports(dev, sqlflow.ListQuery{Batch: "0"}); len(items) != 1 || items[0].ID != single.WorkflowID {
		t.Errorf("普通导出页签: %+v", items)
	}
	// 待审批预计行数（检查阶段统计）
	items, _, _ = xe.svc.ListExports(dev, sqlflow.ListQuery{Batch: "0"})
	if items[0].ExportRows == nil || *items[0].ExportRows != 2 || !items[0].ExportRowsEstimate {
		t.Errorf("预计行数: %+v", items[0])
	}

	// 导出完成：行数、文件卡片
	xe.approve(t, single.WorkflowID)
	if _, err := xe.svc.Execute(ctx, dev, single.WorkflowID, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	items, _, _ = xe.svc.ListExports(dev, sqlflow.ListQuery{Batch: "0"})
	if items[0].Status != model.WfFinish || items[0].ExportRows == nil || *items[0].ExportRows != 2 || items[0].ExportRowsEstimate {
		t.Errorf("完成后的行数: %+v", items[0])
	}
	df, err := xe.svc.Download(dev, single.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	df.Reader.Close()
	d, err := xe.svc.Get(dev, single.WorkflowID)
	if err != nil || d.ExportFile == nil {
		t.Fatalf("详情: %v", err)
	}
	f := d.ExportFile
	if f.Size <= 0 || f.SizeText == "" || f.Cleaned || f.Unknown || f.Downloads != 1 || f.GeneratedAt == nil {
		t.Errorf("文件卡片: %+v", f)
	}
	if d.ExportRows == nil || *d.ExportRows != 2 || d.ExportRowsEst {
		t.Errorf("详情导出行数: %v", d.ExportRows)
	}
	// 文件被清理
	if err := xe.store.Delete(f.Name); err != nil {
		t.Fatal(err)
	}
	d, _ = xe.svc.Get(dev, single.WorkflowID)
	if !d.ExportFile.Cleaned || d.ExportFile.SizeText != "" {
		t.Errorf("已清理: %+v", d.ExportFile)
	}
	// 下载失败不计入下载次数
	xe.svc.Download(dev, single.WorkflowID)
	if d, _ = xe.svc.Get(dev, single.WorkflowID); d.ExportFile.Downloads != 1 {
		t.Errorf("失败的下载不计次: %d", d.ExportFile.Downloads)
	}
}

func TestHumanSize(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KB", 1 << 20: "1.0 MB", 3 << 30: "3.00 GB"} {
		if got := sqlflow.HumanSize(in); got != want {
			t.Errorf("%d: %q want %q", in, got, want)
		}
	}
}

func TestProgress(t *testing.T) {
	xe := setupExport(t)
	ctx := context.Background()
	res, _ := xe.svc.Submit(ctx, dev, xe.req("select id,name from orders", "csv", 11, 12, 13))
	id := res.WorkflowID
	p, err := xe.svc.Progress(dev, id)
	if err != nil || p.Active || p.Total != 3 || p.Percent != -1 {
		t.Errorf("待审批: %+v %v", p, err)
	}
	if _, err := xe.svc.Progress(stranger, id); code(err) != "not_found" {
		t.Errorf("看不到: %v", err)
	}
	xe.approve(t, id)
	if _, err := xe.svc.Execute(ctx, dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	p, _ = xe.svc.Progress(dev, id)
	if p.Active || p.Percent != 100 || p.Done != 3 || p.File == "" || p.TimeoutSec != 60 {
		t.Errorf("完成后: %+v", p)
	}
}

// 进度只在内存里：批量工单执行过程中按实例计数（用阻塞的假引擎观察中间状态）。
func TestProgressWhileRunning(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update t set a=1")
	e.approve(t, id)
	// 手工把工单置为执行中，并写一条“执行工单”日志，模拟别的进程在执行：只能从日志推算开始时间
	w := e.repo.Workflows[id]
	w.Status = model.WfExecuting
	e.repo.Workflows[id] = w
	au, _ := e.repo.AuditByWorkflow(audit.TypeSQLReview, id, false)
	start := e.now.Add(-90 * time.Second)
	e.repo.InsertLog(&model.WorkflowLog{AuditID: au.AuditID, OperationType: 5, OperationTypeDesc: "执行工单", OperationTime: start})
	p, err := e.svc.Progress(dev, id)
	if err != nil || !p.Active || p.StartedAt == nil || p.ElapsedSec != 90 || p.Percent != -1 {
		t.Errorf("执行中: %+v %v", p, err)
	}
}

// gatedEngine 让指定主机上的导出查询卡住，直到放行，用来观察批量导出进行中的进度。
type gatedEngine struct {
	*qEngine
	gate map[string]chan struct{}
}

func (g *gatedEngine) Query(ctx context.Context, sqlText string, o engine.QueryOptions) (*engine.ResultSet, error) {
	if !strings.HasPrefix(sqlText, "SELECT COUNT(*) FROM (") {
		if ch := g.gate[g.host]; ch != nil {
			<-ch
		}
	}
	return g.qEngine.Query(ctx, sqlText, o)
}

func TestProgressBatchExportInFlight(t *testing.T) {
	xe := setupExport(t)
	ctx := context.Background()
	res, err := xe.svc.Submit(ctx, dev, xe.req("select id,name from orders", "csv", 11, 12, 13))
	if err != nil {
		t.Fatal(err)
	}
	id := res.WorkflowID
	xe.approve(t, id)
	gate := map[string]chan struct{}{"ms12": make(chan struct{}), "ms13": make(chan struct{})}
	s := sqlflow.NewService(xe.repo, sqlflow.Options{Notifier: xe.cap, ExportStorage: xe.store, Open: func(_ string, c engine.Conn) (engine.Engine, error) {
		return &gatedEngine{qEngine: &qEngine{host: c.Host, st: xe.hosts[c.Host]}, gate: gate}, nil
	}})
	s.SetAsync(func(f func()) { f() })
	s.SetClock(func() time.Time { return xe.now })
	if _, err := s.Execute(ctx, dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	// ms11 先完成，另外两台卡住：进度应停在 1/3。
	// 内存仓库不是并发安全的：先等后台协程做完开始执行时的写入（之后到放行前它只读），再开始读进度。
	time.Sleep(100 * time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	var p *sqlflow.Progress
	for time.Now().Before(deadline) {
		p, err = s.Progress(dev, id)
		if err != nil {
			t.Fatal(err)
		}
		if p.Done == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !p.Active || p.Total != 3 || p.Done != 1 || p.Percent != 33 || p.StartedAt == nil || p.TimeoutSec != 60 {
		t.Errorf("进行中: %+v", p)
	}
	close(gate["ms12"])
	close(gate["ms13"])
	s.Wait()
	p, _ = s.Progress(dev, id)
	if p.Active || p.Percent != 100 || p.Done != 3 || p.File == "" {
		t.Errorf("完成后: %+v", p)
	}
}
