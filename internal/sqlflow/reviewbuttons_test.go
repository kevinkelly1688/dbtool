// @author: kevinkelly
// @license: Apache Licence
// @file: reviewbuttons_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"testing"
	"time"

	"dbtool/internal/model"
)

// “审批通过”和“驳回”是两个按钮：只有 sqlworkflow.reject 的审批人能驳回但不能通过；只有 sqlworkflow.approve 的能通过但不能驳回别人的单。
func TestRejectAndApproveButtonsAreIndependent(t *testing.T) {
	e := setup(t)

	// 只有驳回：能驳回，不能通过
	e.repo.Perms[2] = map[string]bool{"sqlworkflow.reject": true}
	id := e.submit(t, dev, "update t set a=1")
	if _, err := e.svc.Pass(leader, id, "同意"); code(err) != "forbidden" {
		t.Errorf("只有 sqlworkflow.reject 不能审批通过: %v", err)
	}
	if _, err := e.svc.Cancel(leader, id, "方案不对"); err != nil {
		t.Fatalf("只有 sqlworkflow.reject 应能驳回: %v", err)
	}
	if got := e.status(id); got != model.WfAbort {
		t.Errorf("驳回后状态 = %s", got)
	}

	// 只有通过：能通过，不能驳回别人的单
	e.repo.Perms[2] = map[string]bool{"sqlworkflow.approve": true}
	id2 := e.submit(t, dev, "update t set a=2")
	if _, err := e.svc.Cancel(leader, id2, "不行"); code(err) != "forbidden" {
		t.Errorf("只有 sqlworkflow.approve 不能驳回别人的单: %v", err)
	}
	if got := e.status(id2); got != model.WfManReviewing {
		t.Errorf("被拒绝的驳回不应改状态: %s", got)
	}
	if _, err := e.svc.Pass(leader, id2, "同意"); err != nil {
		t.Errorf("sqlworkflow.approve 应能通过: %v", err)
	}

	// 提交人始终能撤回自己的单（不需要审批按钮）
	id3 := e.submit(t, dev, "update t set a=3")
	if _, err := e.svc.Cancel(dev, id3, "写错了"); err != nil {
		t.Errorf("提交人撤回: %v", err)
	}
}

// “修改可执行时间”只要求人在当前审批节点上（按钮 sqlworkflow.alter_window 由接口校验），不再同时要求审批按钮。
func TestAlterWindowDoesNotNeedApproveButton(t *testing.T) {
	e := setup(t)
	e.repo.Perms[2] = map[string]bool{"sqlworkflow.view_group": true, "sqlworkflow.alter_window": true} // leader：能看组内工单 + 修改可执行时间，没有审批 / 驳回
	id := e.submit(t, dev, "update t set a=1")
	s, en := e.now.Add(time.Hour), e.now.Add(2*time.Hour)

	d, err := e.svc.Get(leader, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.CanReview {
		t.Error("没有审批按钮，不能审批")
	}
	if !d.CanAlterDate {
		t.Error("在当前节点上、能修改可执行时间")
	}
	if err := e.svc.AlterRunDate(leader, id, &s, &en); err != nil {
		t.Fatalf("只有 alter_window 也应能改时间窗: %v", err)
	}
	// 不在当前节点上的人（第二级的 DBA 还没轮到）不行
	if err := e.svc.AlterRunDate(dba, id, &s, &en); code(err) != "forbidden" {
		t.Errorf("还没轮到的审批人不能改: %v", err)
	}
	// 组外的人不行
	if err := e.svc.AlterRunDate(stranger, id, &s, &en); code(err) != "forbidden" {
		t.Errorf("无关的人不能改: %v", err)
	}
}
