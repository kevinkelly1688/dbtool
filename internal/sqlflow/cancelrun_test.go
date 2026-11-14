// @author: kevinkelly
// @license: Apache Licence
// @file: cancelrun_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"strings"
	"testing"

	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/review"
	"dbtool/internal/sqlflow"
)

// 执行中的工单可以被 DBA 取消：正在执行的语句被中断、其后的不执行，工单变为“执行有异常”，日志写明谁、为什么。
func TestCancelRunStopsExecution(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update a set x=1; update b set x=1; update c set x=1")
	e.approve(t, id)
	e.cap.reset()

	var inHook bool
	e.eng.onExec = func() {
		inHook = true
		if got := e.status(id); got != model.WfExecuting {
			t.Errorf("执行期间状态应为执行中: %s", got)
		}
		if _, err := e.svc.CancelRun(dev, id, "我想停"); code(err) != "forbidden" {
			t.Errorf("提交人（没有资源组执行权限）不能取消: %v", err)
		}
		if _, err := e.svc.CancelRun(outsider, id, "x"); code(err) != "not_found" {
			t.Errorf("看不到这张工单的人按不存在处理: %v", err)
		}
		if _, err := e.svc.CancelRun(dba, id, "  "); code(err) != "bad_request" {
			t.Errorf("取消原因必填: %v", err)
		}
		if _, err := e.svc.CancelRun(dba, id, "执行太久，影响线上业务"); err != nil {
			t.Errorf("DBA 应能取消: %v", err)
		}
		if _, err := e.svc.CancelRun(dba, id, "再点一次"); code(err) != "conflict" {
			t.Errorf("重复取消应提示已经发出: %v", err)
		}
	}
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if !inHook {
		t.Fatal("没有进入执行")
	}

	if w := e.repo.Workflows[id]; w.Status != model.WfException || w.FinishTime == nil {
		t.Fatalf("取消后应为执行有异常: %+v", w)
	}
	if len(e.eng.ran) != 0 {
		t.Errorf("取消后不应有语句真正执行: %v", e.eng.ran)
	}
	rows, _ := review.ParseRows(e.repo.Contents[id].ExecuteResult)
	if len(rows) != 3 || !strings.Contains(rows[0].ErrorMessage, "已被 DBA 取消执行") || !strings.Contains(rows[0].ErrorMessage, "原因：执行太久，影响线上业务") || rows[1].Stage != "SKIPPED" && rows[1].StageStatus != "Audit completed" && rows[1].StageStatus == "" {
		t.Errorf("执行结果行: %+v", rows)
	}
	var logs []string
	for _, l := range e.repo.Logs {
		logs = append(logs, l.OperationTypeDesc+":"+l.OperationInfo)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "取消执行:人工取消执行。原因：执行太久，影响线上业务") || !strings.Contains(joined, "执行结束:执行结果：执行有异常") {
		t.Errorf("日志里应有取消和结束: %s", joined)
	}
	if names := strings.Join(e.cap.names(), ","); names != "executing,exception" {
		t.Errorf("通知: %s", names)
	}
	last := e.cap.msgs[len(e.cap.msgs)-1]
	if last.Name != notify.EventException || !strings.Contains(last.Vars["error"], "已被") || !strings.Contains(last.Vars["error"], "取消执行") {
		t.Errorf("异常通知里应写明被取消: %+v", last.Vars)
	}
}

// 取消请求只对“执行中”的工单有效；不在执行中直接拒绝。
func TestCancelRunRequiresExecuting(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update a set x=1")
	if _, err := e.svc.CancelRun(dba, id, "x"); code(err) != "conflict" {
		t.Errorf("待审批的工单不能取消执行: %v", err)
	}
	e.approve(t, id)
	if _, err := e.svc.CancelRun(dba, id, "x"); code(err) != "conflict" {
		t.Errorf("审核通过但没执行的工单不能取消执行: %v", err)
	}
	if _, err := e.svc.CancelRun(dba, 999, "x"); code(err) != "not_found" {
		t.Errorf("不存在的工单: %v", err)
	}
}

// 取消请求到得太晚（语句已经全部执行成功）：工单仍是正常结束，日志里说明没有生效。
func TestCancelRunTooLateKeepsSuccess(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update a set x=1; update b set x=1")
	e.approve(t, id)
	e.eng.ignoreCtx = true
	e.eng.onExec = func() {
		if _, err := e.svc.CancelRun(dba, id, "试试"); err != nil {
			t.Errorf("取消请求: %v", err)
		}
	}
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if w := e.repo.Workflows[id]; w.Status != model.WfFinish {
		t.Fatalf("语句全部执行成功，不能因为取消请求变成异常: %s", w.Status)
	}
	var seen bool
	for _, l := range e.repo.Logs {
		if strings.Contains(l.OperationInfo, "没有生效") {
			seen = true
		}
	}
	if !seen {
		t.Error("日志里应说明取消请求没有生效")
	}
}
