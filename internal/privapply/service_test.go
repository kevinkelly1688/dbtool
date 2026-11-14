// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package privapply_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/privapply"
	"dbtool/internal/privapply/memrepo"
	"dbtool/internal/query"
	"dbtool/internal/settings"
)

// ---- 测试场景 ----
//
// 资源组 1「研发组」：用户 1 dev（申请人）、2 leader（组长）、3 dba。
// 审批流：组长(10) -> DBA(20)。实例 1 带 can_read 标签，属于资源组 1；实例 2 没有标签；实例 3 带标签但不属于任何资源组。
// 用户 9 outsider：在资源组 2，也有 qpriv.review 并属于“组长”权限组（用来测试跨资源组越权）。

type capture struct{ msgs []notify.Event }

func (c *capture) Notify(_ context.Context, ev notify.Event) { c.msgs = append(c.msgs, ev) }

var (
	dev      = audit.Actor{ID: 1, Username: "dev", Display: "开发者"}
	leader   = audit.Actor{ID: 2, Username: "leader", Display: "组长"}
	dba      = audit.Actor{ID: 3, Username: "dba", Display: "DBA"}
	outsider = audit.Actor{ID: 9, Username: "outsider", Display: "外人"}
	root     = audit.Actor{ID: 100, Username: "root", Display: "管理员", IsSuperuser: true}
)

func setup() (*privapply.Service, *memrepo.Repo, *capture) {
	tag := []model.InstanceTag{{TagCode: "can_read", Active: true}}
	r := memrepo.New()
	r.Users = map[uint]model.User{1: {ID: 1, Username: "dev"}, 2: {ID: 2, Username: "leader"}, 3: {ID: 3, Username: "dba"}, 9: {ID: 9, Username: "outsider"}}
	r.Groups = map[uint]*model.AuthGroup{10: {ID: 10, Name: "组长"}, 20: {ID: 20, Name: "DBA"}}
	r.Members = map[uint]map[uint]bool{2: {10: true}, 3: {20: true}, 9: {10: true}}
	r.RGs = []model.ResourceGroup{{GroupID: 1, GroupName: "研发组", SlackWebhook: "https://hooks.example/x"}, {GroupID: 2, GroupName: "其他组"}}
	r.UserRGs = map[uint][]uint{1: {1}, 2: {1}, 3: {1}, 9: {2}}
	r.Instances = []model.Instance{
		{ID: 1, InstanceName: "prod-mysql-01", DBType: "mysql", Tags: tag},
		{ID: 2, InstanceName: "no-tag", DBType: "mysql"},
		{ID: 3, InstanceName: "orphan", DBType: "mysql", Tags: tag},
	}
	r.InstRGs = map[uint][]uint{1: {1}, 2: {1}}
	r.Perms = map[uint]map[string]bool{
		1: {"qpriv.view": true, "qpriv.list": true},
		2: {"qpriv.review": true, "qpriv.manage": true, "qpriv.list": true},
		3: {"qpriv.review": true},
		9: {"qpriv.review": true},
	}
	r.Settings = map[[2]int]*model.WorkflowAuditSetting{{1, 1}: {GroupID: 1, AuditAuthGroups: "10,20"}}
	c := &capture{}
	s := privapply.NewService(r, c)
	s.SetAsync(func(f func()) { f() })
	s.BaseURL = "http://dba.example.com"
	return s, r, c
}

func tomorrow() string { return time.Now().AddDate(0, 0, 1).Format("2006-01-02") }

func tableReq() privapply.CreateReq {
	return privapply.CreateReq{InstanceID: 1, GroupName: "研发组", PrivType: 2, DBName: "shop", TableList: []string{"orders", "customers"}, ValidDate: tomorrow(), LimitNum: 500}
}

func dbReq() privapply.CreateReq {
	return privapply.CreateReq{InstanceID: 1, GroupName: "研发组", PrivType: 1, DBList: []string{"shop", "report"}, ValidDate: tomorrow(), LimitNum: 200}
}

func code(err error) string {
	var qe *query.Error
	if errors.As(err, &qe) {
		return qe.Code
	}
	return "?"
}

func mustCreate(t *testing.T, s *privapply.Service, req privapply.CreateReq) *model.QueryPrivilegesApply {
	t.Helper()
	a, err := s.Create(context.Background(), dev, req)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCreateSuccess(t *testing.T) {
	s, r, c := setup()
	a := mustCreate(t, s, tableReq())
	if a.Status != audit.StatusWaiting || a.DBList != "shop" || a.TableList != "orders,customers" || a.AuditAuthGroups != "10,20" || a.GroupName != "研发组" {
		t.Errorf("申请: %+v", a)
	}
	if !strings.Contains(a.Title, "prod-mysql-01") || len([]rune(a.Title)) > 50 {
		t.Errorf("默认标题: %q", a.Title)
	}
	if len(r.Audits) != 1 || r.Audits[0].CurrentAudit != "10" || r.Audits[0].WorkflowID != a.ApplyID || r.Audits[0].CreateUser != "dev" || len(r.Logs) != 1 {
		t.Errorf("审批记录: %+v logs=%d", r.Audits, len(r.Logs))
	}
	if len(c.msgs) != 1 || !strings.Contains(c.msgs[0].Title, "新的工单申请#1") ||
		!strings.Contains(c.msgs[0].Content, "http://dba.example.com/query/applies/1") || !strings.Contains(c.msgs[0].Content, "当前审批：组长") ||
		!strings.Contains(c.msgs[0].Content, "表清单：orders,customers") {
		t.Errorf("通知: %+v", c.msgs)
	}
	// 消息模板用的变量与消息键
	if v := c.msgs[0].Vars; v["kind"] != "查询权限申请" || v["table_list"] != "orders,customers" || v["applicant_user"] != "dev" || v["current_step"] != "组长" ||
		v["valid_date"] == "" || v["limit_num"] == "" || !strings.Contains(v["url"], "/query/applies/1") {
		t.Errorf("模板变量: %+v", v)
	}
	if k := notify.MessageKey(c.msgs[0]); k != "priv.submitted" {
		t.Errorf("消息键: %q", k)
	}
	// 库权限申请：库列表去重、去空白
	req := dbReq()
	req.DBList = []string{" shop ", "SHOP", "report", ""}
	if a2 := mustCreate(t, s, req); a2.DBList != "shop,report" || a2.TableList != "" {
		t.Errorf("库权限: %+v", a2)
	}
}

func TestCreateValidation(t *testing.T) {
	s, r, _ := setup()
	cases := []struct {
		name   string
		mod    func(*privapply.CreateReq)
		actor  audit.Actor
		status string
	}{
		{"权限类型错误", func(q *privapply.CreateReq) { q.PrivType = 3 }, dev, "bad_request"},
		{"行数为 0", func(q *privapply.CreateReq) { q.LimitNum = 0 }, dev, "bad_request"},
		{"行数过大", func(q *privapply.CreateReq) { q.LimitNum = 99999999 }, dev, "bad_request"},
		{"日期格式", func(q *privapply.CreateReq) { q.ValidDate = "明天" }, dev, "bad_request"},
		{"有效期早于今天", func(q *privapply.CreateReq) { q.ValidDate = time.Now().AddDate(0, 0, -1).Format("2006-01-02") }, dev, "bad_request"},
		{"没有表", func(q *privapply.CreateReq) { q.TableList = nil }, dev, "bad_request"},
		{"没有库", func(q *privapply.CreateReq) { q.DBName = "" }, dev, "bad_request"},
		{"名称含逗号", func(q *privapply.CreateReq) { q.TableList = []string{"a,b"} }, dev, "bad_request"},
		{"名称过长", func(q *privapply.CreateReq) { q.TableList = []string{strings.Repeat("x", 65)} }, dev, "bad_request"},
		{"实例没有 can_read 标签", func(q *privapply.CreateReq) { q.InstanceID = 2 }, dev, "forbidden"},
		{"实例不属于任何资源组", func(q *privapply.CreateReq) { q.InstanceID = 3 }, dev, "forbidden"},
		{"实例不存在", func(q *privapply.CreateReq) { q.InstanceID = 99 }, dev, "forbidden"},
		{"资源组不是自己的", func(q *privapply.CreateReq) { q.GroupName = "其他组" }, dev, "forbidden"},
		{"资源组不存在", func(q *privapply.CreateReq) { q.GroupName = "不存在" }, dev, "forbidden"},
	}
	for _, c := range cases {
		req := tableReq()
		c.mod(&req)
		if _, err := s.Create(context.Background(), c.actor, req); code(err) != c.status {
			t.Errorf("%s: 期望 %s，实际 %v", c.name, c.status, err)
		}
	}
	if len(r.Applies) != 0 {
		t.Errorf("校验失败不应留下申请: %d", len(r.Applies))
	}
	// 资源组为空：用户只有一个资源组时自动选择
	req := tableReq()
	req.GroupName = ""
	if _, err := s.Create(context.Background(), dev, req); err != nil {
		t.Errorf("只属于一个资源组时应自动选择: %v", err)
	}
}

func TestCreateDuplicates(t *testing.T) {
	s, r, _ := setup()
	future := time.Now().AddDate(0, 1, 0)
	r.Privs = []*model.QueryPrivilege{
		{PrivilegeID: 1, UserName: "dev", InstanceID: 1, PrivType: 1, DBName: "report", ValidDate: future, LimitNum: 10},
		{PrivilegeID: 2, UserName: "dev", InstanceID: 1, PrivType: 2, DBName: "shop", TbName: "orders", ValidDate: future, LimitNum: 10},
		{PrivilegeID: 3, UserName: "dev", InstanceID: 1, PrivType: 2, DBName: "old", TbName: "t", ValidDate: time.Now().AddDate(0, 0, -3), LimitNum: 10}, // 已过期
	}
	// 已有库权限，不能再申请库权限，也不能申请该库的表权限
	req := dbReq()
	req.DBList = []string{"report"}
	if _, err := s.Create(context.Background(), dev, req); code(err) != "bad_request" || !strings.Contains(err.Error(), "不能重复申请") {
		t.Errorf("重复库权限: %v", err)
	}
	req = tableReq()
	req.DBName, req.TableList = "report", []string{"x"}
	if _, err := s.Create(context.Background(), dev, req); !strings.Contains(err.Error(), "库的全部权限") {
		t.Errorf("库权限已覆盖: %v", err)
	}
	// 已有表权限
	if _, err := s.Create(context.Background(), dev, tableReq()); code(err) != "bad_request" || !strings.Contains(err.Error(), "shop.orders") {
		t.Errorf("重复表权限: %v", err)
	}
	// 已过期的权限不算
	req = tableReq()
	req.DBName, req.TableList = "old", []string{"t"}
	if _, err := s.Create(context.Background(), dev, req); err != nil {
		t.Errorf("已过期的权限不应阻止重新申请: %v", err)
	}
	// 相同内容的待审批申请不能重复提交
	if _, err := s.Create(context.Background(), dev, req); code(err) != "bad_request" || !strings.Contains(err.Error(), "待审批申请") {
		t.Errorf("重复待审批: %v", err)
	}
}

func TestCreateWithoutAuditSetting(t *testing.T) {
	s, r, _ := setup()
	delete(r.Settings, [2]int{1, 1})
	_, err := s.Create(context.Background(), dev, tableReq())
	if code(err) != "audit_config" || !strings.Contains(err.Error(), "未配置审流") {
		t.Errorf("未配置审批流: %v", err)
	}
	if len(r.Applies) != 0 || len(r.Audits) != 0 {
		t.Errorf("事务应回滚，不留下申请: applies=%d audits=%d", len(r.Applies), len(r.Audits))
	}
}

func TestFullApprovalGrantsPrivileges(t *testing.T) {
	s, r, c := setup()
	ctx := context.Background()
	a := mustCreate(t, s, tableReq())

	// 申请人本人、其他资源组的“组长”都不能审批；第二级的 DBA 也不能越级
	for name, p := range map[string]audit.Actor{"申请人": dev, "跨资源组": outsider, "越级 DBA": dba} {
		if err := s.Review(ctx, p, a.ApplyID, audit.ActionPass, "ok"); code(err) != "forbidden" {
			t.Errorf("%s不应能审批: %v", name, err)
		}
	}
	if len(r.Privs) != 0 {
		t.Fatal("未通过前不应授权")
	}
	// 一级通过：仍待审核，轮到 DBA
	if err := s.Review(ctx, leader, a.ApplyID, audit.ActionPass, "同意"); err != nil {
		t.Fatal(err)
	}
	if a.Status != audit.StatusWaiting || len(r.Privs) != 0 {
		t.Fatalf("一级通过后: status=%d privs=%d", a.Status, len(r.Privs))
	}
	// 二级通过：整体通过并授权（每张表一条表权限）
	if err := s.Review(ctx, dba, a.ApplyID, audit.ActionPass, "同意"); err != nil {
		t.Fatal(err)
	}
	if a.Status != audit.StatusPassed || len(r.Privs) != 2 {
		t.Fatalf("终审通过: status=%d privs=%d", a.Status, len(r.Privs))
	}
	p0 := r.Privs[0]
	if p0.UserName != "dev" || p0.DBName != "shop" || p0.TbName != "orders" || p0.PrivType != 2 || p0.LimitNum != 500 || p0.InstanceID != 1 || p0.ValidDate.IsZero() {
		t.Errorf("授权记录: %+v", p0)
	}
	// 授予的权限立即能被查询流水线识别
	privs, _ := r.ValidQueryPrivileges("dev", 1)
	if !query.HasTablePriv(privs, "SHOP", "customers") {
		t.Error("授权后应能通过表权限校验")
	}
	// 通知：新申请、一级通过后（给下一级）、终审通过
	if len(c.msgs) != 3 || !strings.Contains(c.msgs[1].Content, "组长(passed) -> DBA(current)") || !strings.Contains(c.msgs[2].Title, "工单审核通过") {
		t.Errorf("通知: %+v", c.msgs)
	}
	// 通过后不能再操作
	if err := s.Review(ctx, dba, a.ApplyID, audit.ActionReject, "晚了"); code(err) != "conflict" {
		t.Errorf("终态再操作: %v", err)
	}
	// 库权限申请：每个库一条库权限
	b := mustCreate(t, s, dbReq())
	_ = s.Review(ctx, leader, b.ApplyID, audit.ActionPass, "")
	_ = s.Review(ctx, dba, b.ApplyID, audit.ActionPass, "")
	var dbPrivs int
	for _, p := range r.Privs {
		if p.PrivType == 1 {
			dbPrivs++
			if p.TbName != "" {
				t.Errorf("库权限的表名应为空: %+v", p)
			}
		}
	}
	if dbPrivs != 2 {
		t.Errorf("库权限条数: %d", dbPrivs)
	}
}

func TestRejectAndAbort(t *testing.T) {
	s, r, c := setup()
	ctx := context.Background()
	a := mustCreate(t, s, tableReq())
	if err := s.Review(ctx, leader, a.ApplyID, audit.ActionReject, "  "); code(err) != "bad_request" {
		t.Errorf("驳回必须填写原因: %v", err)
	}
	if err := s.Review(ctx, leader, a.ApplyID, audit.ActionReject, "范围太大"); err != nil {
		t.Fatal(err)
	}
	if a.Status != audit.StatusRejected || len(r.Privs) != 0 {
		t.Errorf("驳回: %d %d", a.Status, len(r.Privs))
	}
	last := c.msgs[len(c.msgs)-1]
	if !strings.Contains(last.Title, "工单被驳回") || !strings.Contains(last.Content, "驳回原因：范围太大") {
		t.Errorf("驳回通知: %+v", last)
	}
	d, _ := s.Detail(dev, a.ApplyID)
	if !strings.Contains(d.LastRemark, "范围太大") || d.CanReview || d.CanAbort {
		t.Errorf("驳回后的详情: %+v", d)
	}

	// 撤回：只有申请人
	b := mustCreate(t, s, dbReq())
	if err := s.Abort(ctx, leader, b.ApplyID, "x"); err == nil {
		t.Error("非申请人不能撤回")
	}
	if err := s.Abort(ctx, dev, b.ApplyID, "不需要了"); err != nil || b.Status != audit.StatusAborted {
		t.Errorf("撤回: %v %d", err, b.Status)
	}
	if err := s.Review(ctx, leader, b.ApplyID, audit.ActionPass, ""); code(err) != "conflict" {
		t.Errorf("撤回后不能再审批: %v", err)
	}
	if last := c.msgs[len(c.msgs)-1]; !strings.Contains(last.Title, "主动终止") || !strings.Contains(last.Content, "不需要了") {
		t.Errorf("撤回通知: %+v", last)
	}
}

func TestSuperuserAndSelfAuditBan(t *testing.T) {
	s, r, _ := setup()
	ctx := context.Background()
	a := mustCreate(t, s, tableReq())
	// 超级用户可以审批任意一级
	if err := s.Review(ctx, root, a.ApplyID, audit.ActionPass, "ok"); err != nil {
		t.Fatalf("超级用户一级: %v", err)
	}
	if err := s.Review(ctx, root, a.ApplyID, audit.ActionPass, "ok"); err != nil || a.Status != audit.StatusPassed {
		t.Fatalf("超级用户二级: %v", err)
	}
	// 禁止自审：申请人同时是审批人
	r.Members[1] = map[uint]bool{10: true}
	r.Perms[1]["qpriv.review"] = true
	req := tableReq()
	req.DBName = "other"
	b := mustCreate(t, s, req)
	// ban_self_audit 未设置时默认禁止自审（D1）；显式设为 false 才保留旧行为
	if err := s.Review(ctx, dev, b.ApplyID, audit.ActionPass, "自己通过"); code(err) != "forbidden" || !strings.Contains(err.Error(), "禁止本人审核") {
		t.Errorf("未设置 ban_self_audit 时默认禁止自审: %v", err)
	}
	r.Cfg[settings.KeyApprovalForbidSelf] = "false"
	if err := s.Review(ctx, dev, b.ApplyID, audit.ActionPass, "自己通过"); err != nil {
		t.Errorf("显式 ban_self_audit=false 时本人可审批: %v", err)
	}
	r.Cfg[settings.KeyApprovalForbidSelf] = "true"
	req.DBName = "other2"
	c2 := mustCreate(t, s, req)
	if err := s.Review(ctx, dev, c2.ApplyID, audit.ActionPass, "自己通过"); code(err) != "forbidden" || !strings.Contains(err.Error(), "禁止本人审核") {
		t.Errorf("禁止自审: %v", err)
	}
}

// 回归保护：授权写入失败时，审批状态必须一起回滚，不能出现“审批通过但没有权限”。
func TestAtomicityOnGrantFailure(t *testing.T) {
	s, r, _ := setup()
	ctx := context.Background()
	a := mustCreate(t, s, tableReq())
	_ = s.Review(ctx, leader, a.ApplyID, audit.ActionPass, "")
	r.FailInsertPrivs = errors.New("db down")
	err := s.Review(ctx, dba, a.ApplyID, audit.ActionPass, "")
	if err == nil {
		t.Fatal("授权失败应返回错误")
	}
	au := r.Audits[0]
	if au.CurrentStatus != audit.StatusWaiting || au.CurrentAudit != "20" || a.Status != audit.StatusWaiting || len(r.Privs) != 0 {
		t.Errorf("应整体回滚: audit=%+v apply.status=%d", au, a.Status)
	}
	if len(r.Details) != 1 || len(r.Logs) != 2 {
		t.Errorf("回滚后审批明细/日志应只剩第一级: details=%d logs=%d", len(r.Details), len(r.Logs))
	}
	// 故障恢复后可以重试
	r.FailInsertPrivs = nil
	if err := s.Review(ctx, dba, a.ApplyID, audit.ActionPass, ""); err != nil || a.Status != audit.StatusPassed || len(r.Privs) != 2 {
		t.Errorf("重试: %v", err)
	}
}

func TestListAndDetailVisibility(t *testing.T) {
	s, _, _ := setup()
	a := mustCreate(t, s, tableReq())

	count := func(p audit.Actor) int {
		res, err := s.List(p, "", 0, 20)
		if err != nil {
			t.Fatal(err)
		}
		return int(res.Total)
	}
	if count(dev) != 1 || count(root) != 1 || count(leader) != 1 {
		t.Errorf("申请人/管理员/同组审批人应能看到: %d %d %d", count(dev), count(root), count(leader))
	}
	if count(outsider) != 0 {
		t.Error("其他资源组的审批人不应看到")
	}
	if count(audit.Actor{ID: 55, Username: "stranger"}) != 0 {
		t.Error("无关用户不应看到")
	}
	// 详情：无关用户和跨组审批人拿到的都是“不存在”
	for name, p := range map[string]audit.Actor{"无关用户": {ID: 55, Username: "stranger"}, "跨组审批人": outsider} {
		if _, err := s.Detail(p, a.ApplyID); code(err) != "not_found" {
			t.Errorf("%s: %v", name, err)
		}
	}
	d, err := s.Detail(leader, a.ApplyID)
	if err != nil {
		t.Fatal(err)
	}
	if !d.CanReview || d.CanAbort || d.InstanceName != "prod-mysql-01" || d.FlowText != "组长->DBA" || len(d.Nodes) != 2 || !d.Nodes[0].Current {
		t.Errorf("审批人看到的详情: %+v", d)
	}
	if len(d.CurrentReviewers) != 1 || d.CurrentReviewers[0].Username != "leader" || len(d.Logs) != 1 {
		t.Errorf("当前审批人/日志: %+v %+v", d.CurrentReviewers, d.Logs)
	}
	d, _ = s.Detail(dev, a.ApplyID)
	if d.CanReview || !d.CanAbort {
		t.Errorf("申请人: CanReview=%v CanAbort=%v", d.CanReview, d.CanAbort)
	}
	if _, err := s.Detail(dev, 999); code(err) != "not_found" {
		t.Errorf("不存在的工单: %v", err)
	}
}

func TestPrivilegeManagement(t *testing.T) {
	s, r, _ := setup()
	future := time.Now().AddDate(0, 1, 0)
	r.Privs = []*model.QueryPrivilege{
		{PrivilegeID: 1, UserName: "dev", InstanceID: 1, PrivType: 2, DBName: "shop", TbName: "orders", ValidDate: future, LimitNum: 10},
		{PrivilegeID: 2, UserName: "someone", InstanceID: 2, PrivType: 1, DBName: "x", ValidDate: future, LimitNum: 10},
		{PrivilegeID: 3, UserName: "far", InstanceID: 3, PrivType: 1, DBName: "y", ValidDate: future, LimitNum: 10}, // 不在 leader 的资源组关联实例里
	}
	// 页面权限
	if _, err := s.ListPrivileges(dba, privapply.PrivQuery{Limit: 20}); code(err) != "forbidden" {
		t.Errorf("无 qpriv.list: %v", err)
	}
	// 普通用户（有 applypriv）只看自己的，且不能管理
	res, err := s.ListPrivileges(dev, privapply.PrivQuery{Limit: 20})
	if err != nil || res.Total != 1 || res.CanManage {
		t.Errorf("普通用户: %v %+v", err, res)
	}
	// 管理者看自己资源组关联实例上的权限；超级用户看全部
	if res, _ := s.ListPrivileges(leader, privapply.PrivQuery{Limit: 20}); res.Total != 2 || !res.CanManage {
		t.Errorf("管理者: %+v", res)
	}
	if res, _ := s.ListPrivileges(root, privapply.PrivQuery{Limit: 20}); res.Total != 3 {
		t.Errorf("超级用户: %+v", res)
	}

	// 修改与删除
	if err := s.ModifyPrivilege(dev, 1, tomorrow(), 5); code(err) != "forbidden" {
		t.Errorf("无 qpriv.manage 不能修改: %v", err)
	}
	if err := s.ModifyPrivilege(leader, 1, "垃圾", 5); code(err) != "bad_request" {
		t.Errorf("日期格式: %v", err)
	}
	if err := s.ModifyPrivilege(leader, 1, time.Now().AddDate(0, 0, -1).Format("2006-01-02"), 5); code(err) != "bad_request" {
		t.Errorf("过去的日期: %v", err)
	}
	if err := s.ModifyPrivilege(leader, 1, tomorrow(), 0); code(err) != "bad_request" {
		t.Errorf("行数: %v", err)
	}
	if err := s.ModifyPrivilege(leader, 1, tomorrow(), 77); err != nil || r.Privs[0].LimitNum != 77 {
		t.Errorf("修改: %v %+v", err, r.Privs[0])
	}
	// 范围限制：不能动别的资源组实例上的权限
	if err := s.ModifyPrivilege(leader, 3, tomorrow(), 5); code(err) != "not_found" {
		t.Errorf("跨范围修改: %v", err)
	}
	if err := s.DeletePrivilege(leader, 3); code(err) != "not_found" {
		t.Errorf("跨范围删除: %v", err)
	}
	if err := s.DeletePrivilege(leader, 1); err != nil || r.Privs[0].IsDeleted != 1 {
		t.Errorf("删除: %v", err)
	}
	if err := s.DeletePrivilege(leader, 1); code(err) != "not_found" {
		t.Errorf("重复删除: %v", err)
	}
	if err := s.DeletePrivilege(root, 3); err != nil {
		t.Errorf("超级用户删除任意权限: %v", err)
	}
}

func TestApplyInstancesAndGroups(t *testing.T) {
	s, r, _ := setup()
	list, _ := s.ApplyInstances(dev)
	if len(list) != 1 || list[0].InstanceName != "prod-mysql-01" {
		t.Errorf("普通用户可申请实例: %+v", list)
	}
	// 超级用户也受标签限制，但不受资源组限制
	list, _ = s.ApplyInstances(root)
	if len(list) != 2 {
		t.Errorf("超级用户: %+v", list)
	}
	r.Perms[1]["query.admin_run"] = true
	if list, _ = s.ApplyInstances(dev); len(list) != 2 {
		t.Errorf("query.admin_run: %+v", list)
	}
	if g, _ := s.ApplyGroups(dev); len(g) != 1 || g[0].GroupName != "研发组" {
		t.Errorf("资源组: %+v", g)
	}
	if g, _ := s.ApplyGroups(root); len(g) != 2 {
		t.Errorf("超级用户资源组: %+v", g)
	}
	if !s.CanAccessPage(dev) || s.CanAccessPage(dba) || !s.CanAccessPage(root) {
		t.Error("页面权限判断错误")
	}
}

// 事件携带 @ 规则需要的信息：事件名、提交人、当前节点的审批权限组、资源组。
func TestEventsCarryMentionContext(t *testing.T) {
	s, _, c := setup()
	ctx := context.Background()
	a := mustCreate(t, s, tableReq())
	ev := c.msgs[0]
	if ev.Name != notify.EventPending || ev.Kind != notify.KindQueryPrivilege || ev.Applicant != "dev" || ev.GroupID != 1 || ev.ReviewerAuthGroupID != 10 {
		t.Errorf("新申请事件: %+v", ev)
	}
	_ = s.Review(ctx, leader, a.ApplyID, 1, "ok") // 一级通过：仍是“待处理”，审批人换成第二级的权限组
	if ev := c.msgs[1]; ev.Name != notify.EventPending || ev.ReviewerAuthGroupID != 20 {
		t.Errorf("流转到下一级: %+v", ev)
	}
	_ = s.Review(ctx, dba, a.ApplyID, 1, "ok")
	if ev := c.msgs[2]; ev.Name != notify.EventPassed || ev.Applicant != "dev" || ev.ReviewerAuthGroupID != 0 {
		t.Errorf("通过事件: %+v", ev)
	}
	b := mustCreate(t, s, dbReq())
	_ = s.Review(ctx, leader, b.ApplyID, 2, "不行")
	if ev := c.msgs[len(c.msgs)-1]; ev.Name != notify.EventRejected {
		t.Errorf("驳回事件: %+v", ev)
	}
	d := mustCreate(t, s, privapply.CreateReq{InstanceID: 1, GroupName: "研发组", PrivType: 1, DBList: []string{"other"}, ValidDate: tomorrow(), LimitNum: 10})
	_ = s.Abort(ctx, dev, d.ApplyID, "算了")
	if ev := c.msgs[len(c.msgs)-1]; ev.Name != notify.EventAborted {
		t.Errorf("撤回事件: %+v", ev)
	}
}

// 权限申请：超级用户也不能审批自己提交的申请（D1）；ban_self_audit 显式为 false 时保留旧行为。
func TestSuperuserCannotApproveOwnApply(t *testing.T) {
	s, r, _ := setup()
	ctx := context.Background()
	r.Members[100] = map[uint]bool{10: true}
	req := tableReq()
	a, err := s.Create(ctx, root, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Review(ctx, root, a.ApplyID, audit.ActionPass, "自批"); code(err) != "forbidden" || !strings.Contains(err.Error(), "禁止本人审核") {
		t.Errorf("超级用户自审应被拒绝: %v", err)
	}
	if err := s.Review(ctx, leader, a.ApplyID, audit.ActionPass, "别人批"); err != nil {
		t.Errorf("别人审批照常: %v", err)
	}
	r.Cfg[settings.KeyApprovalForbidSelf] = "false"
	if err := s.Review(ctx, root, a.ApplyID, audit.ActionPass, "自批"); err != nil {
		t.Errorf("显式 false 保留旧行为: %v", err)
	}
}
