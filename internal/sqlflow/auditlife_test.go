// @author: kevinkelly
// @license: Apache Licence
// @file: auditlife_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/sqlflow"
)

// ---- 审批类动作写系统审计 ----

func (e *env) lifeActions() []string {
	var out []string
	for _, a := range e.repo.LifecycleAudits {
		out = append(out, a.Action)
	}
	return out
}

func TestLifecycleAuditFlow(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update t set a=1")
	if got := e.lifeActions(); len(got) != 1 || got[0] != "SQL上线-提交" {
		t.Fatalf("提交: %v", got)
	}
	first := e.repo.LifecycleAudits[0]
	if first.UserName != "dev" || first.UserDisplay != "开发者" || !strings.Contains(first.ExtraInfo, "工单id：") || !strings.Contains(first.ExtraInfo, "类型：SQL上线") {
		t.Errorf("提交审计: %+v", first)
	}

	// 两级审批：第一级通过进入下一级，第二级通过后全部通过
	if _, err := e.svc.Pass(leader, id, "同意"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Pass(dba, id, "ok"); err != nil {
		t.Fatal(err)
	}
	acts := e.repo.LifecycleAudits
	if len(acts) != 3 || acts[1].Action != "SQL上线-审批通过" || !strings.Contains(acts[1].ExtraInfo, "进入下一级审批") || !strings.Contains(acts[1].ExtraInfo, "备注：同意") ||
		!strings.Contains(acts[2].ExtraInfo, "审批已全部通过") || acts[2].UserName != "dba" {
		t.Errorf("审批通过: %+v", acts)
	}

	// 失败的审批不记（状态不对）
	if _, err := e.svc.Pass(dba, id, "again"); err == nil {
		t.Fatal("已通过的工单不能再审批")
	}
	if len(e.repo.LifecycleAudits) != 3 {
		t.Errorf("失败的操作不应写审计: %v", e.lifeActions())
	}

	// 立即执行
	if _, err := e.svc.Execute(ctxBG(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto, Executor: sqlflow.ExecNative}); err != nil {
		t.Fatal(err)
	}
	if got := e.lifeActions(); got[len(got)-1] != "SQL上线-立即执行" || !strings.Contains(e.repo.LifecycleAudits[len(got)-1].ExtraInfo, "原生执行") {
		t.Errorf("执行: %v", got)
	}
	// 原有的下载 / 回滚语句审计表不受影响
	if len(e.repo.AuditEntries) != 0 {
		t.Errorf("审批审计不应混进 AuditEntries: %+v", e.repo.AuditEntries)
	}
}

func TestLifecycleAuditRejectAbortScheduleAlter(t *testing.T) {
	e := setup(t)
	// 驳回（审批人）与撤销（提交人）分开记
	id := e.submit(t, dev, "update t set a=1")
	if _, err := e.svc.Cancel(leader, id, "方案不对"); err != nil {
		t.Fatal(err)
	}
	id2 := e.submit(t, dev, "update t set a=2")
	if _, err := e.svc.Cancel(dev, id2, "写错了"); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range e.repo.LifecycleAudits {
		if a.Action != "SQL上线-提交" {
			got = append(got, a.Action+"|"+a.UserName)
		}
	}
	if strings.Join(got, ",") != "SQL上线-驳回|leader,SQL上线-撤销|dev" {
		t.Errorf("驳回 / 撤销: %v", got)
	}
	if a := e.repo.LifecycleAudits[1]; !strings.Contains(a.ExtraInfo, "备注：方案不对") {
		t.Errorf("驳回原因: %+v", a)
	}

	// 修改可执行时间
	id3 := e.submit(t, dev, "update t set a=3")
	s, en := e.now.Add(time.Hour), e.now.Add(2*time.Hour)
	if err := e.svc.AlterRunDate(leader, id3, &s, &en); err != nil {
		t.Fatal(err)
	}
	last := e.repo.LifecycleAudits[len(e.repo.LifecycleAudits)-1]
	if last.Action != "SQL上线-修改可执行时间" || !strings.Contains(last.ExtraInfo, "时间窗：") {
		t.Errorf("修改可执行时间: %+v", last)
	}

	// 定时 / 取消定时
	id4 := e.submit(t, dev, "update t set a=4")
	e.approve(t, id4)
	if _, err := e.svc.Schedule(dev, id4, e.now.Add(time.Hour), sqlflow.ExecNative); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CancelSchedule(dev, id4); err != nil {
		t.Fatal(err)
	}
	n := len(e.repo.LifecycleAudits)
	if a, b := e.repo.LifecycleAudits[n-2], e.repo.LifecycleAudits[n-1]; a.Action != "SQL上线-定时执行" || !strings.Contains(a.ExtraInfo, "定时时间：") || b.Action != "SQL上线-取消定时" {
		t.Errorf("定时: %+v %+v", a, b)
	}

	// 手工执行确认
	id5 := e.submit(t, dev, "update t set a=5")
	e.approve(t, id5)
	if _, err := e.svc.Execute(ctxBG(), dev, id5, sqlflow.ExecuteReq{Mode: sqlflow.ModeManual}); err != nil {
		t.Fatal(err)
	}
	if a := e.repo.LifecycleAudits[len(e.repo.LifecycleAudits)-1]; a.Action != "SQL上线-手工执行确认" {
		t.Errorf("手工执行确认: %+v", a)
	}
}

// 审计写失败（或仓库不支持）绝不影响业务；工单日志（dbt_workflow_log）照常写。
func TestLifecycleAuditFailureDoesNotBreakBusiness(t *testing.T) {
	e := setup(t)
	e.repo.FailLifecycle = errors.New("dbt_audit_log 写入失败")
	id := e.submit(t, dev, "update t set a=1")
	e.approve(t, id)
	if e.status(id) != model.WfReviewPass {
		t.Fatalf("审计失败不应影响业务: %s", e.status(id))
	}
	if len(e.repo.LifecycleAudits) != 0 {
		t.Errorf("注入故障后不应有记录: %v", e.lifeActions())
	}
	if len(e.repo.Logs) == 0 {
		t.Error("工单日志（dbt_workflow_log）行为不变")
	}
}

// 审计动作名必须能被 audit.Classify 归到 SQL 上线类别和对应按钮权限码。
func TestLifecycleActionsClassified(t *testing.T) {
	cases := map[string]string{
		"提交": "sqlworkflow.submit", "审批通过": "sqlworkflow.approve", "驳回": "sqlworkflow.reject", "撤销": "sqlworkflow.cancel_own",
		"立即执行": "sqlworkflow.execute_group", "手工执行确认": "sqlworkflow.manual_finish", "定时执行": "sqlworkflow.schedule", "取消定时": "sqlworkflow.schedule",
		"修改可执行时间": "sqlworkflow.alter_window", "在线改表终止": "sqlworkflow.osc", "批量实例重试": "sqlworkflow.batch_op", "批量实例关闭": "sqlworkflow.batch_op",
	}
	for act, code := range cases {
		x := audit.Classify(audit.ActionPrefixSQLFlow+act, "")
		if x.Code != code || x.Category != audit.CatSQLFlow || x.Result != audit.ResultOK {
			t.Errorf("%s: %+v 期望码 %s", act, x, code)
		}
	}
}

func ctxBG() context.Context { return context.Background() }

func TestLifecycleAuditOscAndBatch(t *testing.T) {
	e := setup(t)
	e.useInception()
	rows := inceptionRows("alter table t add c int")
	rows[0].SQLSha1 = "abc123"
	e.inc.checkOut = rows
	id := e.submit(t, dev, "alter table t add c int")
	e.approve(t, id)
	w := e.repo.Workflows[id]
	w.Status = model.WfExecuting
	e.repo.Workflows[id] = w
	e.inc.oscOut = []map[string]any{{"PERCENT": 37}}
	if _, err := e.svc.OscControl(ctxBG(), dev, id, "get", "abc123"); err != nil {
		t.Fatal(err)
	}
	before := len(e.repo.LifecycleAudits)
	if _, err := e.svc.OscControl(ctxBG(), dev, id, "pause", "abc123"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.OscControl(ctxBG(), dev, id, "kill", "abc123"); err != nil {
		t.Fatal(err)
	}
	got := e.repo.LifecycleAudits[before:]
	if len(got) != 2 || got[0].Action != "SQL上线-在线改表暂停" || got[1].Action != "SQL上线-在线改表终止" {
		t.Errorf("查询进度不记，暂停 / 终止要记: %+v", got)
	}
	if x := audit.Classify(got[1].Action, got[1].ExtraInfo); x.Code != "sqlworkflow.osc" || !x.Risky {
		t.Errorf("在线改表终止应归 osc 且高危: %+v", x)
	}
}
