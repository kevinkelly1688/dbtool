// @author: kevinkelly
// @license: Apache Licence
// @file: selfaudit_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"strings"
	"testing"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// 自审（D1）：SQL 上线工单超级用户可自审；数据导出工单超级用户也不能审批自己的；
// ban_self_audit 显式 false 时保留旧行为。
func TestSuperuserSelfAuditByWorkflowKind(t *testing.T) {
	xe := setupExport(t)
	ctx := context.Background()
	xe.repo.Perms[100] = map[string]bool{"export.submit": true, "sqlworkflow.submit": true}
	xe.repo.UserRGs[100] = []uint{1}

	// 超级用户提交导出工单
	r := xe.req(exportSQL, "csv", 1)
	res, err := xe.svc.Submit(ctx, root, r)
	if err != nil {
		t.Fatal(err)
	}
	exp := res.WorkflowID
	if _, err := xe.svc.Pass(root, exp, "自批"); code(err) != "forbidden" || !strings.Contains(err.Error(), "禁止本人审核") {
		t.Errorf("导出工单超级用户自审应被拒绝: %v", err)
	}
	if audit.CanReview(xe.repo, mustAudit(t, xe, exp), root) {
		t.Error("CanReview 应与 Pass 一致")
	}
	if _, err := xe.svc.Pass(leader, exp, "别人批"); err != nil {
		t.Errorf("别人审批照常: %v", err)
	}

	// 超级用户提交普通 SQL 上线工单：可以自审
	res, err = xe.svc.Submit(ctx, root, req("update t set a=1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := xe.svc.Pass(root, res.WorkflowID, "自批"); err != nil {
		t.Errorf("SQL 上线超级用户可自审: %v", err)
	}

	// 显式 false：导出工单恢复旧行为
	xe.repo.Cfg[settings.KeyApprovalForbidSelf] = "false"
	res, err = xe.svc.Submit(ctx, root, xe.req(exportSQL, "csv", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := xe.svc.Pass(root, res.WorkflowID, "自批"); err != nil {
		t.Errorf("ban_self_audit=false 时导出工单可自审: %v", err)
	}
}

// 普通用户默认不能审批自己提交的 SQL 上线工单。
func TestRegularUserSelfAuditDeniedByDefault(t *testing.T) {
	e := setup(t)
	e.repo.Perms[2]["sqlworkflow.submit"] = true
	e.repo.UserRGs[2] = []uint{1}
	id := e.submit(t, leader, "update t set a=1") // 未设置 ban_self_audit
	if _, err := e.svc.Pass(leader, id, "自己批"); code(err) != "forbidden" {
		t.Errorf("默认禁止自审: %v", err)
	}
	e.repo.Cfg[settings.KeyApprovalForbidSelf] = "false"
	if _, err := e.svc.Pass(leader, id, "自己批"); err != nil {
		t.Errorf("显式 false 允许: %v", err)
	}
}

func mustAudit(t *testing.T, xe *exportEnv, id uint) *model.WorkflowAudit {
	t.Helper()
	a, err := xe.repo.AuditByWorkflow(audit.TypeSQLReview, id, false)
	if err != nil || a == nil {
		t.Fatalf("审批记录: %v %v", a, err)
	}
	return a
}
