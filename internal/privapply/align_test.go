// @author: kevinkelly
// @license: Apache Licence
// @file: align_test.go
// @time: 2026/10/28

package privapply_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/privapply"
	"dbtool/internal/privapply/memrepo"
)

// S3-B3：申请的实例必须属于所选资源组（审批流跟着资源组走）。
func TestCreateInstanceMustBelongToChosenGroup(t *testing.T) {
	s, r, _ := setup()
	// dev 同时属于资源组 1、2；实例 1 只属于资源组 1
	r.UserRGs[1] = []uint{1, 2}
	r.Settings[[2]int{2, 1}] = &model.WorkflowAuditSetting{GroupID: 2, AuditAuthGroups: "10"}
	req := tableReq()
	req.GroupName = "其他组"
	_, err := s.Create(context.Background(), dev, req)
	if code(err) != "bad_request" || !strings.Contains(err.Error(), "所选资源组未关联实例") {
		t.Fatalf("实例不属于所选资源组应拒绝: %v", err)
	}
	if len(r.Applies) != 0 || len(r.Audits) != 0 {
		t.Fatal("被拒绝时不应产生申请与审批记录")
	}
	// 选对资源组可以
	req.GroupName = "研发组"
	if _, err := s.Create(context.Background(), dev, req); err != nil {
		t.Fatalf("所选资源组关联该实例应通过: %v", err)
	}
}

// S3-B2：待审批申请按库表重叠判断，不只看内容完全相同。
func TestCreateDuplicateByOverlap(t *testing.T) {
	s, _, _ := setup()
	ctx := context.Background()
	first := tableReq() // shop.orders,customers
	mustCreate(t, s, first)

	cases := []struct {
		name string
		mod  func(*privapply.CreateReq)
		dup  bool
	}{
		{"同库部分表重叠", func(q *privapply.CreateReq) { q.TableList = []string{"orders", "items"} }, true},
		{"表名大小写不同", func(q *privapply.CreateReq) { q.TableList = []string{"ORDERS"} }, true},
		{"同库不同表", func(q *privapply.CreateReq) { q.TableList = []string{"items"} }, false},
		{"不同库同表名", func(q *privapply.CreateReq) { q.DBName = "other" }, false},
	}
	for _, c := range cases {
		req := tableReq()
		c.mod(&req)
		_, err := s.Create(ctx, dev, req)
		if c.dup && (code(err) != "bad_request" || !strings.Contains(err.Error(), "待审批申请")) {
			t.Errorf("%s：应判为重复: %v", c.name, err)
		}
		if !c.dup && err != nil {
			t.Errorf("%s：不应判为重复: %v", c.name, err)
		}
	}

	// 库权限：库列表有交集即重复；待审批的库权限覆盖该库的表权限申请
	db := dbReq()
	db.DBList = []string{"report"}
	mustCreate(t, s, db)
	again := dbReq()
	again.DBList = []string{"report", "extra"}
	if _, err := s.Create(ctx, dev, again); code(err) != "bad_request" || !strings.Contains(err.Error(), "待审批申请") {
		t.Errorf("库列表有交集: %v", err)
	}
	tbl := tableReq()
	tbl.DBName, tbl.TableList = "report", []string{"t1"}
	if _, err := s.Create(ctx, dev, tbl); code(err) != "bad_request" || !strings.Contains(err.Error(), "待审批申请") {
		t.Errorf("待审批的库权限已覆盖该库的表: %v", err)
	}
}

// extSetup3 同 extSetup（打开旁路表、用户启用），另返回通知捕获器。
func extSetup3() (*privapply.Service, *memrepo.Repo, *capture) {
	s, r, c := setup()
	r.ExtOn = true
	for id, u := range r.Users {
		u.IsActive = true
		u.Display = map[uint]string{1: "开发者", 2: "组长", 3: "DBA", 9: "外人"}[id]
		r.Users[id] = u
	}
	return s, r, c
}

// S3-B4 / A1 / A2：修改、回收权限的审计带目标用户与新旧值；通知受影响用户；回写来源申请单日志。
func TestPrivilegeChangeAuditNotifyAndApplyLog(t *testing.T) {
	s, r, c := extSetup3()
	a := mustCreate(t, s, withReason(tableReq()))
	approveAll(t, s, a.ApplyID)
	if len(r.Privs) != 2 {
		t.Fatalf("应授予 2 条权限: %d", len(r.Privs))
	}
	logsBefore := len(r.Logs)
	c.msgs = nil
	r.AuditEntries = nil

	id := r.Privs[0].PrivilegeID
	oldDate := r.Privs[0].ValidDate.Format("2006-01-02")
	newDate := time.Now().AddDate(0, 0, 10).Format("2006-01-02")
	if err := s.ModifyPrivilege(leader, id, newDate, 77); err != nil {
		t.Fatal(err)
	}
	if len(r.AuditEntries) != 1 {
		t.Fatalf("修改应写 1 条审计: %d", len(r.AuditEntries))
	}
	e := r.AuditEntries[0]
	for _, want := range []string{"用户:dev", "prod-mysql-01", "shop.orders", oldDate + "→" + newDate, "500→77"} {
		if !strings.Contains(e.ExtraInfo, want) {
			t.Errorf("修改审计缺少 %q: %s", want, e.ExtraInfo)
		}
	}
	if e.Action != "修改查询权限" || e.UserName != "leader" {
		t.Errorf("修改审计: %+v", e)
	}
	if len(c.msgs) != 1 || !contains(c.msgs[0].ExtraUsers, "dev") || !strings.Contains(c.msgs[0].Content, "77") {
		t.Errorf("应通知受影响用户 dev: %+v", c.msgs)
	}
	if len(r.Logs) != logsBefore+1 || !strings.Contains(r.Logs[len(r.Logs)-1].OperationInfo, "shop.orders") ||
		r.Logs[len(r.Logs)-1].AuditID != r.Audits[0].AuditID || r.Logs[len(r.Logs)-1].Operator != "leader" {
		t.Errorf("应回写来源申请单日志: %+v", r.Logs[len(r.Logs)-1])
	}

	// 回收
	c.msgs, r.AuditEntries = nil, nil
	if err := s.DeletePrivilege(leader, id); err != nil {
		t.Fatal(err)
	}
	if len(r.AuditEntries) != 1 || r.AuditEntries[0].Action != "回收查询权限" || !strings.Contains(r.AuditEntries[0].ExtraInfo, "用户:dev") || !strings.Contains(r.AuditEntries[0].ExtraInfo, "shop.orders") {
		t.Errorf("回收审计: %+v", r.AuditEntries)
	}
	if len(c.msgs) != 1 || !contains(c.msgs[0].ExtraUsers, "dev") || !strings.Contains(c.msgs[0].Title, "回收") {
		t.Errorf("回收应通知 dev: %+v", c.msgs)
	}
	// 权限变更与申请审批共用 passed / aborted 事件名，靠 TplKey 区分，套“权限被修改 / 回收”的模板
	if ev := c.msgs[0]; notify.MessageKey(ev) != "priv.changed" || ev.Vars["applicant_user"] != "dev" || ev.Vars["change"] == "" || ev.Vars["operator"] == "" {
		t.Errorf("权限变更的模板键与变量: %q %+v", notify.MessageKey(ev), ev.Vars)
	}
	if len(r.Logs) != logsBefore+2 || !strings.Contains(r.Logs[len(r.Logs)-1].OperationInfo, "回收") {
		t.Errorf("回收应回写申请单日志: %d", len(r.Logs))
	}
}

// S3-A1 / A2：直接授予通知被授予人；直接授予的权限没有来源申请单，不回写申请单日志。
func TestDirectGrantNotifiesTarget(t *testing.T) {
	s, r, c := extSetup3()
	logs := len(r.Logs)
	req := privapply.GrantReq{UserName: "dev", InstanceID: 1, PrivType: 1, DBList: []string{"shop"}, ValidDate: tomorrow(), LimitNum: 100, Reason: "排查线上问题需要"}
	if _, err := s.GrantPrivilege(context.Background(), leader, req); err != nil {
		t.Fatal(err)
	}
	if len(c.msgs) != 1 || !contains(c.msgs[0].ExtraUsers, "dev") || !strings.Contains(c.msgs[0].Title, "授予") || !strings.Contains(c.msgs[0].Content, "shop") {
		t.Fatalf("应通知被授予人: %+v", c.msgs)
	}
	if len(r.Logs) != logs {
		t.Error("直接授予没有来源申请单，不应写申请单日志")
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

var _ = audit.ActionPass
