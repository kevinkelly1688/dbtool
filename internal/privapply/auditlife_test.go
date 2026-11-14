// @author: kevinkelly
// @license: Apache Licence
// @file: auditlife_test.go
// @time: 2026/10/28

package privapply_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dbtool/internal/audit"
)

// ---- 审批类动作写系统审计：提交、审批通过、驳回、撤销 ----

func TestLifecycleAudit(t *testing.T) {
	s, r, _ := setup()
	ctx := context.Background()
	a := mustCreate(t, s, tableReq())
	if len(r.LifecycleAudits) != 1 || r.LifecycleAudits[0].Action != "查询权限申请-提交" || r.LifecycleAudits[0].UserName != "dev" ||
		!strings.Contains(r.LifecycleAudits[0].ExtraInfo, "申请单id：1") || !strings.Contains(r.LifecycleAudits[0].ExtraInfo, "shop.orders,customers") {
		t.Fatalf("提交: %+v", r.LifecycleAudits)
	}
	// 审批失败（无权）不记
	if err := s.Review(ctx, dev, a.ApplyID, audit.ActionPass, "ok"); err == nil {
		t.Fatal("申请人不能审批")
	}
	if len(r.LifecycleAudits) != 1 {
		t.Errorf("失败的操作不应写审计: %d", len(r.LifecycleAudits))
	}
	_ = s.Review(ctx, leader, a.ApplyID, audit.ActionPass, "同意")
	_ = s.Review(ctx, dba, a.ApplyID, audit.ActionPass, "")
	got := r.LifecycleAudits
	if len(got) != 3 || got[1].Action != "查询权限申请-审批通过" || !strings.Contains(got[1].ExtraInfo, "进入下一级审批") || !strings.Contains(got[1].ExtraInfo, "备注：同意") ||
		!strings.Contains(got[2].ExtraInfo, "权限已授予") || got[2].UserName != "dba" {
		t.Errorf("审批通过: %+v", got)
	}

	b := mustCreate(t, s, dbReq())
	_ = s.Review(ctx, leader, b.ApplyID, audit.ActionReject, "范围太大")
	creq := tableReq()
	creq.TableList = []string{"refunds"}
	c := mustCreate(t, s, creq)
	_ = s.Abort(ctx, dev, c.ApplyID, "不需要了")
	var acts []string
	for _, e := range r.LifecycleAudits[3:] {
		acts = append(acts, e.Action+"|"+e.UserName)
	}
	if strings.Join(acts, ",") != "查询权限申请-提交|dev,查询权限申请-驳回|leader,查询权限申请-提交|dev,查询权限申请-撤销|dev" {
		t.Errorf("驳回 / 撤销: %v", acts)
	}
	if e := r.LifecycleAudits[4]; !strings.Contains(e.ExtraInfo, "备注：范围太大") {
		t.Errorf("驳回原因: %+v", e)
	}
	if len(r.AuditEntries) != 0 {
		t.Errorf("审批审计不影响直接授予的审计表: %d", len(r.AuditEntries))
	}
}

func TestLifecycleAuditFailureDoesNotBreakBusiness(t *testing.T) {
	s, r, _ := setup()
	r.FailLifecycle = errors.New("dbt_audit_log 写入失败")
	a := mustCreate(t, s, tableReq())
	if err := s.Review(context.Background(), leader, a.ApplyID, audit.ActionReject, "不行"); err != nil {
		t.Fatalf("审计失败不应影响业务: %v", err)
	}
	if a.Status != audit.StatusRejected || len(r.LifecycleAudits) != 0 {
		t.Errorf("状态=%d 审计=%d", a.Status, len(r.LifecycleAudits))
	}
}

func TestLifecycleActionsClassified(t *testing.T) {
	cases := map[string]string{"提交": "qpriv.apply", "撤销": "qpriv.apply", "审批通过": "qpriv.review", "驳回": "qpriv.review"}
	for act, code := range cases {
		x := audit.Classify(audit.ActionPrefixPriv+act, "")
		if x.Code != code || x.Category != audit.CatQuery || x.Result != audit.ResultOK {
			t.Errorf("%s: %+v 期望码 %s", act, x, code)
		}
	}
	if x := audit.Classify("直接授予查询权限", ""); x.Code != "qpriv.manage" || !x.Risky {
		t.Errorf("直接授予: %+v", x)
	}
}
