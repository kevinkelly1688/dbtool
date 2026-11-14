// @author: kevinkelly
// @license: Apache Licence
// @file: audit_test.go
// @time: 2026/10/28

package audit

import (
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/settings"
)

type fakeRepo struct {
	settings map[[2]int]*model.WorkflowAuditSetting // [groupID, type]
	groups   map[uint]*model.AuthGroup
	members  map[uint]map[uint]bool // userID -> authGroupID
	rgUsers  map[[2]uint]bool       // [resourceGroupID, userID]：属于该资源组的用户
	perms    map[uint]map[string]bool
	cfg      map[string]string
	audits   []*model.WorkflowAudit
	details  []*model.WorkflowAuditDetail
	logs     []*model.WorkflowLog
}

func newRepo() *fakeRepo {
	return &fakeRepo{
		settings: map[[2]int]*model.WorkflowAuditSetting{},
		groups:   map[uint]*model.AuthGroup{10: {ID: 10, Name: "组长"}, 20: {ID: 20, Name: "DBA"}},
		members:  map[uint]map[uint]bool{},
		rgUsers:  map[[2]uint]bool{},
		perms:    map[uint]map[string]bool{},
		cfg:      map[string]string{},
	}
}

func (r *fakeRepo) AuditSetting(g uint, t int) (*model.WorkflowAuditSetting, error) {
	return r.settings[[2]int{int(g), t}], nil
}
func (r *fakeRepo) AuthGroup(id uint) (*model.AuthGroup, error) { return r.groups[id], nil }
func (r *fakeRepo) UserInAuthGroup(u, g uint) (bool, error)     { return r.members[u][g], nil }
func (r *fakeRepo) AuthGroupUsers(names []string, rg uint) ([]model.User, error) {
	var out []model.User
	for uid, gs := range r.members {
		for gid := range gs {
			g := r.groups[gid]
			for _, n := range names {
				if g != nil && g.Name == n && r.rgUsers[[2]uint{rg, uid}] {
					out = append(out, model.User{ID: uid, IsActive: true})
				}
			}
		}
	}
	return out, nil
}
func (r *fakeRepo) InsertAudit(a *model.WorkflowAudit) error {
	a.AuditID = uint(len(r.audits) + 1)
	r.audits = append(r.audits, a)
	return nil
}
func (r *fakeRepo) SaveAudit(*model.WorkflowAudit) error { return nil }
func (r *fakeRepo) InsertDetail(d *model.WorkflowAuditDetail) error {
	r.details = append(r.details, d)
	return nil
}
func (r *fakeRepo) InsertLog(l *model.WorkflowLog) error { r.logs = append(r.logs, l); return nil }
func (r *fakeRepo) HasButton(u uint, c string) (bool, error) {
	return r.perms[u][c], nil
}
func (r *fakeRepo) ConfigValue(k string) (string, bool) { v, ok := r.cfg[k]; return v, ok }

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)

const rg = 1 // 资源组 ID

func spec() Spec {
	return Spec{WorkflowType: TypeQueryPrivilege, WorkflowID: 7, GroupID: rg, GroupName: "研发组", Title: "申请", CreateUser: "dev", CreateUserDisplay: "开发者"}
}

// 两级审批流：组长(10) -> DBA(20)；用户 2 是组长、3 是 DBA，都在资源组 1 里并有 qpriv.review。
func twoLevel() *fakeRepo {
	r := newRepo()
	r.settings[[2]int{rg, TypeQueryPrivilege}] = &model.WorkflowAuditSetting{GroupID: rg, AuditAuthGroups: "10,20"}
	for uid, g := range map[uint]uint{2: 10, 3: 20} {
		r.members[uid] = map[uint]bool{g: true}
		r.rgUsers[[2]uint{rg, uid}] = true
		r.perms[uid] = map[string]bool{"qpriv.review": true}
	}
	return r
}

var (
	leader    = Actor{ID: 2, Username: "leader", Display: "组长"}
	dba       = Actor{ID: 3, Username: "dba", Display: "DBA"}
	applicant = Actor{ID: 1, Username: "dev", Display: "开发者"}
)

func TestCreate(t *testing.T) {
	r := twoLevel()
	a, err := Create(r, now, spec())
	if err != nil {
		t.Fatal(err)
	}
	if a.CurrentAudit != "10" || a.NextAudit != "20" || a.CurrentStatus != StatusWaiting || a.AuditAuthGroups != "10,20" {
		t.Errorf("两级审批流初始状态: %+v", a)
	}
	if len(r.logs) != 1 || !strings.Contains(r.logs[0].OperationInfo, "组长->DBA") || r.logs[0].OperationType != ActionSubmit {
		t.Errorf("提交日志: %+v", r.logs)
	}
	// 单级审批流：next 为 -1
	r.settings[[2]int{rg, TypeQueryPrivilege}].AuditAuthGroups = "20"
	if a, _ := Create(r, now, spec()); a.CurrentAudit != "20" || a.NextAudit != "-1" {
		t.Errorf("单级: %+v", a)
	}
	// 未配置审批流
	delete(r.settings, [2]int{rg, TypeQueryPrivilege})
	if _, err := Create(r, now, spec()); err == nil || !strings.Contains(err.Error(), "未配置审流") {
		t.Errorf("未配置审批流应报错: %v", err)
	}
	// 配置了但是空：不能让申请卡死
	r.settings[[2]int{rg, TypeQueryPrivilege}] = &model.WorkflowAuditSetting{AuditAuthGroups: " , "}
	if _, err := Create(r, now, spec()); err == nil {
		t.Error("空审批流应报错")
	}
}

func TestOperateMultiLevelPass(t *testing.T) {
	r := twoLevel()
	a, _ := Create(r, now, spec())

	// 第一级：组长通过 -> 轮到 DBA
	if _, err := Operate(r, now, a, ActionPass, leader, "ok"); err != nil {
		t.Fatal(err)
	}
	if a.CurrentAudit != "20" || a.NextAudit != "-1" || a.CurrentStatus != StatusWaiting {
		t.Fatalf("一级通过后: %+v", a)
	}
	// 组长不能审批第二级
	if _, err := Operate(r, now, a, ActionPass, leader, ""); err == nil || !strings.Contains(err.Error(), "不在当前节点") {
		t.Errorf("越级审批应被拒: %v", err)
	}
	// 第二级：DBA 通过 -> 整体通过
	d, err := Operate(r, now, a, ActionPass, dba, "同意")
	if err != nil {
		t.Fatal(err)
	}
	if a.CurrentStatus != StatusPassed || a.CurrentAudit != "-1" || d.AuditStatus != StatusPassed || d.AuditUser != "dba" {
		t.Errorf("终审通过: %+v %+v", a, d)
	}
	if len(r.details) != 2 || len(r.logs) != 3 || !strings.Contains(r.logs[1].OperationInfo, "下级审批：DBA") || !strings.Contains(r.logs[2].OperationInfo, "无下级审批") {
		t.Errorf("明细/日志: %d %d %+v", len(r.details), len(r.logs), r.logs)
	}
	// 终态不能再操作
	if _, err := Operate(r, now, a, ActionReject, dba, "x"); err == nil {
		t.Error("已通过的工单不应再被操作")
	}
}

func TestReject(t *testing.T) {
	r := twoLevel()
	a, _ := Create(r, now, spec())
	d, err := Operate(r, now, a, ActionReject, leader, "理由不充分")
	if err != nil || a.CurrentStatus != StatusRejected || a.CurrentAudit != "-1" || a.NextAudit != "-1" || d.Remark != "理由不充分" {
		t.Fatalf("驳回: %v %+v", err, a)
	}
}

func TestAbortOnlyByCreator(t *testing.T) {
	r := twoLevel()
	a, _ := Create(r, now, spec())
	if _, err := Operate(r, now, a, ActionAbort, leader, "x"); err == nil || !strings.Contains(err.Error(), "只有工单提交者") {
		t.Errorf("非提交者不能撤回: %v", err)
	}
	if _, err := Operate(r, now, a, ActionAbort, Actor{ID: 9, Username: "root", IsSuperuser: true}, "x"); err == nil {
		t.Error("超级用户也不能替别人撤回")
	}
	if _, err := Operate(r, now, a, ActionAbort, applicant, "不需要了"); err != nil || a.CurrentStatus != StatusAborted {
		t.Errorf("提交者撤回: %v %+v", err, a)
	}
}

func TestPermissionRules(t *testing.T) {
	r := twoLevel()
	a, _ := Create(r, now, spec())
	deny := func(name string, actor Actor, want string) {
		t.Helper()
		err := CanOperate(r, a, ActionPass, actor)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: 期望含 %q，实际 %v", name, want, err)
		}
	}
	// 没有 qpriv.review 权限
	r.perms[2]["qpriv.review"] = false
	deny("无审批权限", leader, "无相关审批权限")
	r.perms[2]["qpriv.review"] = true

	// 有权限但不在当前节点权限组
	deny("不在节点组", dba, "不在当前节点")

	// 同名权限组但属于另一个资源组：必须被拒绝（否则可越权审批）
	r.members[5] = map[uint]bool{10: true}
	r.perms[5] = map[string]bool{"qpriv.review": true}
	r.rgUsers[[2]uint{2, 5}] = true // 只属于资源组 2
	deny("跨资源组", Actor{ID: 5, Username: "other"}, "不在流程相关资源组")

	// 当前节点权限组被删除
	r.groups[10] = nil
	deny("权限组不存在", leader, "当前审批权限组不存在")
	r.groups[10] = &model.AuthGroup{ID: 10, Name: "组长"}

	// 超级用户可以审批任何工单
	if err := CanOperate(r, a, ActionPass, Actor{ID: 99, Username: "root", IsSuperuser: true}); err != nil {
		t.Errorf("超级用户: %v", err)
	}
	// 禁止自审：申请人本人是审批人。未设置 ban_self_audit 时默认禁止；显式 false 才允许。
	// 超级用户的组合见 TestSelfAuditMatrix。
	r.members[1] = map[uint]bool{10: true}
	r.rgUsers[[2]uint{rg, 1}] = true
	r.perms[1] = map[string]bool{"qpriv.review": true}
	deny("默认禁止自审", applicant, "禁止本人审核")
	r.cfg[settings.KeyApprovalForbidSelf] = "false"
	if err := CanOperate(r, a, ActionPass, applicant); err != nil {
		t.Errorf("显式 ban_self_audit=false 时本人可审批: %v", err)
	}
	r.cfg[settings.KeyApprovalForbidSelf] = "true"
	deny("禁止自审", applicant, "禁止本人审核")
	// CanReview 与 CanOperate 一致
	if !CanReview(r, a, leader) || CanReview(r, a, dba) {
		t.Error("CanReview 判断错误")
	}
}

func TestReviewNodes(t *testing.T) {
	r := twoLevel()
	a, _ := Create(r, now, spec())
	n := ReviewNodes(r, a)
	if len(n) != 2 || !n[0].Current || n[0].Passed || n[1].Current || n[1].Passed {
		t.Errorf("初始: %+v", n)
	}
	_, _ = Operate(r, now, a, ActionPass, leader, "")
	n = ReviewNodes(r, a)
	if !n[0].Passed || n[0].Current || !n[1].Current {
		t.Errorf("一级通过后: %+v", n)
	}
	// 整体终态时不设置节点状态
	_, _ = Operate(r, now, a, ActionReject, dba, "no")
	for _, x := range ReviewNodes(r, a) {
		if x.Current || x.Passed {
			t.Errorf("终态节点不应有状态: %+v", x)
		}
	}
}

func TestFlowTextMissingGroup(t *testing.T) {
	r := twoLevel()
	a := &model.WorkflowAudit{AuditAuthGroups: "10,99"}
	if got, _ := FlowText(r, a); got != "组长->99" {
		t.Errorf("权限组被删除时应显示 ID: %q", got)
	}
}

func TestBadFlowConfig(t *testing.T) {
	r := twoLevel()
	a, _ := Create(r, now, spec())
	a.NextAudit = "77" // next 不在审批流内（脏数据）
	_, err := Operate(r, now, a, ActionPass, leader, "")
	var ae *Error
	if !errors.As(err, &ae) || !strings.Contains(ae.Message, "审批流配置错误") {
		t.Errorf("脏数据应报配置错误: %v", err)
	}
}

// ---- SQL 上线工单的扩展行为 ----

func sqlSpec() Spec {
	return Spec{WorkflowType: TypeSQLReview, WorkflowID: 7, GroupID: 1, GroupName: "研发组", Title: "上线", CreateUser: "dev", CreateUserDisplay: "开发者"}
}

func TestSQLWorkflowAutoPassRejectAndEmptyFlow(t *testing.T) {
	r := newRepo()
	r.settings[[2]int{1, TypeSQLReview}] = &model.WorkflowAuditSetting{GroupID: 1, WorkflowType: TypeSQLReview, AuditAuthGroups: "10,20"}

	s := sqlSpec()
	s.AutoReject = true
	a, err := Create(r, now, s)
	if err != nil || a.CurrentStatus != StatusRejected || a.AuditAuthGroups != "" || a.CurrentAudit != "-1" ||
		r.logs[0].OperationInfo != "系统直接审核不通过" {
		t.Errorf("自动驳回: %+v %v", a, err)
	}
	s = sqlSpec()
	s.AutoPass = true
	if a, err = Create(r, now, s); err != nil || a.CurrentStatus != StatusPassed || a.AuditAuthGroups != "" || !strings.Contains(r.logs[1].OperationInfo, "系统直接审核通过") {
		t.Errorf("自动通过: %+v %v", a, err)
	}
	// 自动驳回优先于自动通过
	s.AutoReject = true
	if a, _ = Create(r, now, s); a.CurrentStatus != StatusRejected {
		t.Errorf("同时命中时应驳回: %+v", a)
	}
	// 空审批流：SQL 工单自动通过；查询权限申请仍必须配置
	r.settings[[2]int{1, TypeSQLReview}].AuditAuthGroups = ""
	if a, err = Create(r, now, sqlSpec()); err != nil || a.CurrentStatus != StatusPassed {
		t.Errorf("空审批流: %+v %v", a, err)
	}
	r.settings[[2]int{1, TypeQueryPrivilege}] = &model.WorkflowAuditSetting{GroupID: 1, WorkflowType: TypeQueryPrivilege}
	q := sqlSpec()
	q.WorkflowType = TypeQueryPrivilege
	if _, err = Create(r, now, q); err == nil {
		t.Error("查询权限申请的空审批流应报错")
	}
	// 自动标志对其他类型无效
	q.AutoPass = true
	if _, err = Create(r, now, q); err == nil {
		t.Error("自动通过标志不能让查询权限申请绕过审批")
	}
}

func TestReviewPermissionPerType(t *testing.T) {
	r := newRepo()
	r.members[2] = map[uint]bool{10: true}
	r.rgUsers[[2]uint{1, 2}] = true
	r.perms[2] = map[string]bool{"qpriv.review": true} // 只有查询权限审批权限
	a := &model.WorkflowAudit{AuditID: 1, GroupID: 1, WorkflowType: TypeSQLReview, AuditAuthGroups: "10", CurrentAudit: "10", NextAudit: "-1", CurrentStatus: StatusWaiting, CreateUser: "dev"}
	actor := Actor{ID: 2, Username: "leader"}
	if err := CanOperate(r, a, ActionPass, actor); err == nil {
		t.Error("只有 qpriv.review 不能审批 SQL 上线工单")
	}
	r.perms[2] = map[string]bool{"sqlworkflow.approve": true, "sqlworkflow.reject": true}
	if err := CanOperate(r, a, ActionPass, actor); err != nil {
		t.Errorf("sqlworkflow.approve 可以审批 SQL 工单: %v", err)
	}
	a.WorkflowType = TypeQueryPrivilege
	if err := CanOperate(r, a, ActionPass, actor); err == nil {
		t.Error("sqlworkflow.approve / reject 不能审批查询权限申请")
	}
}

// SQL 上线工单的“通过”和“驳回”是两个按钮：只有其中一个的人，只能做那一个动作。
func TestSQLApproveAndRejectAreSeparateButtons(t *testing.T) {
	r := newRepo()
	r.members[2] = map[uint]bool{10: true}
	r.rgUsers[[2]uint{1, 2}] = true
	a := &model.WorkflowAudit{AuditID: 1, GroupID: 1, WorkflowType: TypeSQLReview, AuditAuthGroups: "10", CurrentAudit: "10", NextAudit: "-1", CurrentStatus: StatusWaiting, CreateUser: "dev"}
	actor := Actor{ID: 2, Username: "leader"}

	r.perms[2] = map[string]bool{"sqlworkflow.approve": true}
	if err := CanOperate(r, a, ActionPass, actor); err != nil {
		t.Errorf("只有 approve：应能通过: %v", err)
	}
	if err := CanOperate(r, a, ActionReject, actor); err == nil || !strings.Contains(err.Error(), "无相关审批权限") {
		t.Errorf("只有 approve：不应能驳回: %v", err)
	}
	r.perms[2] = map[string]bool{"sqlworkflow.reject": true}
	if err := CanOperate(r, a, ActionReject, actor); err != nil {
		t.Errorf("只有 reject：应能驳回: %v", err)
	}
	if err := CanOperate(r, a, ActionPass, actor); err == nil || !strings.Contains(err.Error(), "无相关审批权限") {
		t.Errorf("只有 reject：不应能通过: %v", err)
	}
	if CanReview(r, a, actor) {
		t.Error("CanReview 看的是“通过”：只有 reject 时不应为真")
	}
	// 导出工单也是 SQL 工单：同样看 approve / reject
	a.IsExport = true
	r.perms[2] = map[string]bool{"sqlworkflow.approve": true}
	if err := CanOperate(r, a, ActionPass, actor); err != nil {
		t.Errorf("导出工单：approve 应能通过: %v", err)
	}
}

// IsCurrentApprover 只看“人在当前节点上”：不要审批按钮；其余规则（成员关系、禁止自审、状态）照常。
func TestIsCurrentApproverIgnoresButtons(t *testing.T) {
	r := twoLevel()
	r.perms[2]["qpriv.review"] = false // 组长没有任何审批按钮
	a, _ := Create(r, now, spec())
	if CanReview(r, a, leader) {
		t.Fatal("没有审批按钮不能审批")
	}
	if !IsCurrentApprover(r, a, leader) {
		t.Error("组长在当前节点上，应是当前审批人（不看按钮）")
	}
	if IsCurrentApprover(r, a, dba) {
		t.Error("DBA 还没轮到（不在当前节点的权限组里）")
	}
	if IsCurrentApprover(r, a, Actor{ID: 99, Username: "x"}) {
		t.Error("不相关的人不是审批人")
	}
	if !IsCurrentApprover(r, a, Actor{ID: 99, Username: "root", IsSuperuser: true}) {
		t.Error("超级用户总是")
	}
	// 禁止自审：申请人本人在当前节点里也不算
	r.members[1] = map[uint]bool{10: true}
	r.rgUsers[[2]uint{rg, 1}] = true
	if IsCurrentApprover(r, a, applicant) {
		t.Error("禁止自审时，申请人本人不是当前审批人")
	}
	// 审批结束后没有“当前审批人”
	a.CurrentStatus = StatusPassed
	if IsCurrentApprover(r, a, leader) {
		t.Error("已通过的工单没有当前审批人")
	}
}

// 数据归档申请没有单独的审批按钮：属于该级权限组、该资源组的用户就能审批；权限组与资源组仍要逐项校验。
func TestArchiveReviewNeedsNoButton(t *testing.T) {
	r := newRepo()
	r.members[2] = map[uint]bool{10: true}
	r.rgUsers[[2]uint{1, 2}] = true
	a := &model.WorkflowAudit{AuditID: 1, GroupID: 1, WorkflowType: TypeArchive, AuditAuthGroups: "10", CurrentAudit: "10", NextAudit: "-1", CurrentStatus: StatusWaiting, CreateUser: "dev"}
	actor := Actor{ID: 2, Username: "leader"}
	if err := CanOperate(r, a, ActionPass, actor); err != nil {
		t.Errorf("归档审批不需要按钮权限: %v", err)
	}
	// 不在当前节点权限组 / 不在资源组：仍然被拒绝
	if err := CanOperate(r, a, ActionPass, Actor{ID: 9, Username: "x"}); err == nil {
		t.Error("不在审核组的用户不能审批归档申请")
	}
	r.members[5] = map[uint]bool{10: true}
	r.rgUsers[[2]uint{2, 5}] = true // 同名权限组，但只属于另一个资源组
	if err := CanOperate(r, a, ActionPass, Actor{ID: 5, Username: "other"}); err == nil {
		t.Error("其他资源组里的同名权限组成员不能审批归档申请")
	}
}

// 不认识的工单类型一律拒绝（不会因为“不需要按钮”而被放行）。
func TestUnknownWorkflowTypeRejected(t *testing.T) {
	r := newRepo()
	r.members[2] = map[uint]bool{10: true}
	r.rgUsers[[2]uint{1, 2}] = true
	a := &model.WorkflowAudit{AuditID: 1, GroupID: 1, WorkflowType: 99, AuditAuthGroups: "10", CurrentAudit: "10", NextAudit: "-1", CurrentStatus: StatusWaiting, CreateUser: "dev"}
	if err := CanOperate(r, a, ActionPass, Actor{ID: 2, Username: "leader"}); err == nil || !strings.Contains(err.Error(), "不支持的工单类型") {
		t.Errorf("未知工单类型应被拒绝: %v", err)
	}
}

func TestPassedSQLWorkflowCanBeRejectedOrAborted(t *testing.T) {
	r := newRepo()
	mk := func(typ int) *model.WorkflowAudit {
		return &model.WorkflowAudit{AuditID: 1, GroupID: 1, WorkflowType: typ, AuditAuthGroups: "10", CurrentAudit: "-1", NextAudit: "-1", CurrentStatus: StatusPassed, CreateUser: "dev"}
	}
	someone := Actor{ID: 3, Username: "dba"}
	// 业务层授权后，已通过的 SQL 工单可以被驳回
	a := mk(TypeSQLReview)
	if _, err := Operate(r, now, a, ActionReject, someone, "窗口取消"); err != nil || a.CurrentStatus != StatusRejected {
		t.Errorf("通过后驳回: %+v %v", a, err)
	}
	// 提交人可以撤回；别人不能以“撤回”名义撤
	a = mk(TypeSQLReview)
	if err := CanOperate(r, a, ActionAbort, someone); err == nil {
		t.Error("非提交人不能撤回")
	}
	if _, err := Operate(r, now, a, ActionAbort, Actor{ID: 1, Username: "dev"}, "不做了"); err != nil || a.CurrentStatus != StatusAborted {
		t.Errorf("提交人撤回: %+v %v", a, err)
	}
	// 通过动作对已通过的工单不可用；查询权限申请通过后不允许任何操作
	if err := CanOperate(r, mk(TypeSQLReview), ActionPass, Actor{ID: 100, IsSuperuser: true}); err == nil {
		t.Error("已通过不能再通过")
	}
	if err := CanOperate(r, mk(TypeQueryPrivilege), ActionReject, Actor{ID: 100, IsSuperuser: true}); err == nil {
		t.Error("查询权限申请通过后不能驳回")
	}
}

// 禁止自审矩阵（D1）：ban_self_audit 未设置 = 禁止；显式 false = 不限制。
// 查询权限申请 / 数据导出工单 / 数据归档：超级用户也不能审批自己的；SQL 上线（非导出）：超级用户可自审。
func TestSelfAuditMatrix(t *testing.T) {
	type wf struct {
		name   string
		typ    int
		export bool
		strict bool // 超级用户也不能自审
	}
	flows := []wf{
		{"查询权限", TypeQueryPrivilege, false, true},
		{"SQL上线", TypeSQLReview, false, false},
		{"数据导出", TypeSQLReview, true, true},
		{"归档", TypeArchive, false, true},
	}
	cfgs := []struct {
		name string
		set  func(map[string]string)
		ban  bool
	}{
		{"未设置", func(m map[string]string) {}, true},
		{"空值", func(m map[string]string) { m[settings.KeyApprovalForbidSelf] = "" }, true},
		{"true", func(m map[string]string) { m[settings.KeyApprovalForbidSelf] = "true" }, true},
		{"False", func(m map[string]string) { m[settings.KeyApprovalForbidSelf] = " False " }, false},
		{"0", func(m map[string]string) { m[settings.KeyApprovalForbidSelf] = "0" }, false},
		{"乱写", func(m map[string]string) { m[settings.KeyApprovalForbidSelf] = "maybe" }, true},
	}
	for _, f := range flows {
		for _, c := range cfgs {
			r := twoLevel()
			c.set(r.cfg)
			r.perms[2] = map[string]bool{"qpriv.review": true, "sqlworkflow.approve": true, "sqlworkflow.reject": true}
			// 提交人本人在当前节点（组长）里、有审核权限；另有一个超级用户提交人
			r.members[1] = map[uint]bool{10: true}
			r.rgUsers[[2]uint{rg, 1}] = true
			r.perms[1] = map[string]bool{"qpriv.review": true, "sqlworkflow.approve": true, "sqlworkflow.reject": true}
			a := &model.WorkflowAudit{
				GroupID: rg, WorkflowType: f.typ, IsExport: f.export, AuditAuthGroups: "10,20",
				CurrentAudit: "10", NextAudit: "20", CurrentStatus: StatusWaiting, CreateUser: "dev",
			}
			for _, act := range []int{ActionPass, ActionReject} {
				// 普通提交人
				err := CanOperate(r, a, act, applicant)
				if c.ban && err == nil {
					t.Errorf("%s/%s/动作%d: 普通用户自审应被拒绝", f.name, c.name, act)
				}
				if !c.ban && err != nil {
					t.Errorf("%s/%s/动作%d: ban=false 时普通用户自审应放行: %v", f.name, c.name, act, err)
				}
				// 超级用户提交人
				su := Actor{ID: 1, Username: "dev", IsSuperuser: true}
				err = CanOperate(r, a, act, su)
				wantDeny := c.ban && f.strict
				if wantDeny && (err == nil || !strings.Contains(err.Error(), "禁止本人审核")) {
					t.Errorf("%s/%s/动作%d: 超级用户自审应被拒绝, got %v", f.name, c.name, act, err)
				}
				if !wantDeny && err != nil {
					t.Errorf("%s/%s/动作%d: 超级用户自审应放行: %v", f.name, c.name, act, err)
				}
				// 别人的单：超级用户、组长照常
				other := Actor{ID: 99, Username: "root", IsSuperuser: true}
				if err := CanOperate(r, a, act, other); err != nil {
					t.Errorf("%s/%s: 超级用户审批他人的单应放行: %v", f.name, c.name, err)
				}
			}
			if !CanReview(r, a, leader) {
				t.Errorf("%s/%s: 别的审批人应可审", f.name, c.name)
			}
			// CanReview 与 CanOperate 一致
			if got := CanReview(r, a, Actor{ID: 1, Username: "dev", IsSuperuser: true}); got != !(c.ban && f.strict) {
				t.Errorf("%s/%s: CanReview 超级用户自审=%v", f.name, c.name, got)
			}
		}
	}
}

// 撤回不受禁止自审影响（只有提交人本人能撤回）。
func TestAbortNotAffectedBySelfAuditBan(t *testing.T) {
	r := twoLevel()
	a, _ := Create(r, now, spec())
	if err := CanOperate(r, a, ActionAbort, applicant); err != nil {
		t.Errorf("提交人撤回: %v", err)
	}
}

func TestBanSelfAuditParsing(t *testing.T) {
	r := newRepo()
	if !BanSelfAudit(r) {
		t.Error("未设置应默认禁止")
	}
	for v, want := range map[string]bool{"true": true, "TRUE": true, "1": true, "": true, "false": false, "FALSE": false, "off": false, "no": false, "0": false} {
		r.cfg[settings.KeyApprovalForbidSelf] = v
		if got := BanSelfAudit(r); got != want {
			t.Errorf("%q: got %v want %v", v, got, want)
		}
	}
}
