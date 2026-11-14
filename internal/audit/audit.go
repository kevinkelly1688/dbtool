// @author: kevinkelly
// @license: Apache Licence
// @file: audit.go
// @time: 2026/10/28

// Package audit 是通用的工单审批引擎，
// 当前只接入“查询权限申请”这一种工单（SQL 上线、归档后续接入时复用）。
//
// 审批流：每个资源组、每种工单类型在 dbt_workflow_audit_setting 里配置一串审批权限组（逗号分隔的 dbt_auth_group ID），
// 工单按顺序逐级审批；任何一级驳回即终止，最后一级通过则整体通过。
package audit

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// 工单类型、状态、操作（数值直接存在数据库里）。
const (
	TypeQueryPrivilege = 1 // 查询权限申请
	TypeSQLReview      = 2 // SQL 上线申请
	TypeArchive        = 3 // 数据归档申请
)

const (
	StatusWaiting  = 0
	StatusPassed   = 1
	StatusRejected = 2
	StatusAborted  = 3
)

const (
	ActionSubmit = 0
	ActionPass   = 1
	ActionReject = 2
	ActionAbort  = 3
)

// ActionLabel 是操作的显示名（写入 dbt_workflow_log.operation_type_desc）。
var ActionLabel = map[int]string{ActionSubmit: "提交", ActionPass: "审核通过", ActionReject: "审核不通过", ActionAbort: "审核取消"}

var typeLabel = map[int]string{TypeQueryPrivilege: "查询权限申请", TypeSQLReview: "SQL上线申请", TypeArchive: "数据归档申请"}

// reviewButton 返回审批这类工单所需的按钮权限（登记表 internal/btnperm），按工单类型与动作区分：
// 查询权限申请通过 / 驳回都要 qpriv.review；SQL 上线（含导出）工单通过要 sqlworkflow.approve、驳回要 sqlworkflow.reject；
// 数据归档申请没有单独的审批按钮（needed 为 false）：审批人 = 该级权限组中、属于该资源组的用户，权限组与资源组仍然逐项校验。
// known 为 false 表示不认识的工单类型。
func reviewButton(workflowType, action int) (code string, needed, known bool) {
	switch workflowType {
	case TypeQueryPrivilege:
		return "qpriv.review", true, true
	case TypeSQLReview:
		if action == ActionReject {
			return "sqlworkflow.reject", true, true
		}
		return "sqlworkflow.approve", true, true
	case TypeArchive:
		return "", false, true
	}
	return "", false, false
}

// 错误类别，供上层映射成 HTTP 状态码。
const (
	KindForbidden = "forbidden" // 无权限做这个操作
	KindConflict  = "conflict"  // 工单当前状态不允许该操作
	KindConfig    = "config"    // 审批流未配置或配置有误
)

// Error 是审批过程中的业务错误，Message 可直接展示给用户。
type Error struct {
	Kind    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func errf(format string, a ...any) *Error {
	return &Error{Kind: KindForbidden, Message: fmt.Sprintf(format, a...)}
}

func errKind(kind, format string, a ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, a...)}
}

// Repo 是审批引擎需要的数据访问。所有写操作应在同一个事务里完成（由调用方通过事务版 Repo 保证）。
type Repo interface {
	AuditSetting(groupID uint, workflowType int) (*model.WorkflowAuditSetting, error) // 不存在返回 nil, nil
	AuthGroup(id uint) (*model.AuthGroup, error)                                      // 不存在返回 nil, nil
	UserInAuthGroup(userID, authGroupID uint) (bool, error)
	// AuthGroupUsers 返回：属于指定权限组（按组名）且属于指定资源组的启用用户。
	AuthGroupUsers(authGroupNames []string, resourceGroupID uint) ([]model.User, error)
	InsertAudit(a *model.WorkflowAudit) error
	SaveAudit(a *model.WorkflowAudit) error
	InsertDetail(d *model.WorkflowAuditDetail) error
	InsertLog(l *model.WorkflowLog) error
	HasButton(userID uint, code string) (bool, error)
	ConfigValue(item string) (string, bool)
}

// Actor 是执行操作的用户。
type Actor struct {
	ID          uint
	Username    string
	Display     string
	IsSuperuser bool
}

// Spec 描述要创建审批流的工单。
type Spec struct {
	WorkflowType      int
	WorkflowID        uint
	GroupID           uint
	GroupName         string
	Title             string
	CreateUser        string
	CreateUserDisplay string
	// 以下仅 SQL 上线工单有意义：
	AutoReject bool // 系统直接驳回（自动审核发现错误）
	AutoPass   bool // 系统直接通过（命中自动审核规则）
}

// Create 为工单创建审批流并写入“提交”日志。返回创建的审批记录。
// 资源组没有配置该类型的审批流时报错（必须先配置审批流才能提交工单）。
func Create(repo Repo, now time.Time, s Spec) (*model.WorkflowAudit, error) {
	setting, err := repo.AuditSetting(s.GroupID, s.WorkflowType)
	if err != nil {
		return nil, err
	}
	if setting == nil {
		return nil, errKind(KindConfig, "审批类型 %s 未配置审流", typeLabel[s.WorkflowType])
	}
	groups := splitIDs(setting.AuditAuthGroups)
	// 系统自动驳回 / 自动通过：只对 SQL 上线工单生效，不依赖审批流内容。
	if s.WorkflowType == TypeSQLReview && (s.AutoReject || s.AutoPass) {
		return createAuto(repo, now, s)
	}
	if len(groups) == 0 {
		// “空审批流”只对 SQL 上线工单有自动通过的含义；查询权限申请必须有至少一级审批，
		// 否则申请会永远卡在待审核且没有任何人能审批。
		if s.WorkflowType == TypeSQLReview {
			s.AutoPass = true
			return createAuto(repo, now, s)
		}
		return nil, errKind(KindConfig, "资源组「%s」的%s审批流没有配置审批权限组，请联系管理员", s.GroupName, typeLabel[s.WorkflowType])
	}
	a := &model.WorkflowAudit{
		GroupID: uint(s.GroupID), GroupName: s.GroupName, WorkflowID: s.WorkflowID, WorkflowType: s.WorkflowType,
		WorkflowTitle: s.Title, AuditAuthGroups: strings.Join(groups, ","),
		CurrentAudit: groups[0], NextAudit: "-1", CurrentStatus: StatusWaiting,
		CreateUser: s.CreateUser, CreateUserDisplay: s.CreateUserDisplay,
	}
	if len(groups) > 1 {
		a.NextAudit = groups[1]
	}
	if err := repo.InsertAudit(a); err != nil {
		return nil, err
	}
	flow, err := FlowText(repo, a)
	if err != nil {
		return nil, err
	}
	err = repo.InsertLog(&model.WorkflowLog{
		AuditID: a.AuditID, OperationType: ActionSubmit, OperationTypeDesc: ActionLabel[ActionSubmit],
		OperationInfo: "等待审批，审批流程：" + flow, Operator: s.CreateUser, OperatorDisplay: s.CreateUserDisplay, OperationTime: now,
	})
	return a, err
}

// createAuto 创建不需要人工审批的审批记录：自动驳回或自动通过。
func createAuto(repo Repo, now time.Time, s Spec) (*model.WorkflowAudit, error) {
	a := &model.WorkflowAudit{
		GroupID: s.GroupID, GroupName: s.GroupName, WorkflowID: s.WorkflowID, WorkflowType: s.WorkflowType,
		WorkflowTitle: s.Title, AuditAuthGroups: "", CurrentAudit: "-1", NextAudit: "-1",
		CreateUser: s.CreateUser, CreateUserDisplay: s.CreateUserDisplay,
	}
	info := "无需审批，系统直接审核通过"
	a.CurrentStatus = StatusPassed
	if s.AutoReject {
		a.CurrentStatus = StatusRejected
		info = "系统直接审核不通过"
	}
	if err := repo.InsertAudit(a); err != nil {
		return nil, err
	}
	err := repo.InsertLog(&model.WorkflowLog{
		AuditID: a.AuditID, OperationType: ActionSubmit, OperationTypeDesc: ActionLabel[ActionSubmit],
		OperationInfo: info, Operator: s.CreateUser, OperatorDisplay: s.CreateUserDisplay, OperationTime: now,
	})
	return a, err
}

func splitIDs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// FlowText 返回可读的审批流，如 “组长->DBA”（权限组被删除时显示其 ID）。
func FlowText(repo Repo, a *model.WorkflowAudit) (string, error) {
	var names []string
	for _, id := range splitIDs(a.AuditAuthGroups) {
		names = append(names, groupName(repo, id))
	}
	if len(names) == 0 {
		return "无需审批", nil
	}
	return strings.Join(names, "->"), nil
}

func groupName(repo Repo, idStr string) string {
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		return idStr
	}
	g, err := repo.AuthGroup(uint(id))
	if err != nil || g == nil {
		return idStr
	}
	return g.Name
}

// CanOperate 检查用户能否对审批中的工单执行某个操作，不能则返回 *Error。
//
// 审批人必须同时属于“当前节点的权限组”和“工单所属资源组”。
// 其他资源组里同名权限组的成员不能借此越权审批。
func CanOperate(repo Repo, a *model.WorkflowAudit, action int, actor Actor) error {
	return canOperate(repo, a, action, actor, true)
}

// canOperate 是 CanOperate 的实现；needButton 为 false 时不校验审批按钮（只校验状态、禁止自审、权限组 / 资源组成员关系），见 IsCurrentApprover。
func canOperate(repo Repo, a *model.WorkflowAudit, action int, actor Actor, needButton bool) error {
	if action != ActionPass && action != ActionReject && action != ActionAbort {
		return errf("不允许的操作")
	}
	// 待审核：可通过 / 驳回 / 撤回；审批通过但还没执行完的 SQL 工单（尚未执行，业务层负责判断）：可驳回 / 撤回。
	switch {
	case a.CurrentStatus == StatusWaiting:
	case a.CurrentStatus == StatusPassed && a.WorkflowType == TypeSQLReview && action != ActionPass:
	default:
		return errKind(KindConflict, "不允许的操作，工单当前状态为 %s，不允许做任何操作", statusLabel(a.CurrentStatus))
	}
	if action == ActionAbort {
		if actor.Username != a.CreateUser {
			return errf("只有工单提交者可以撤回工单")
		}
		return nil
	}
	// 已通过的 SQL 工单被驳回：此时审批流已走完，没有“当前节点”可比对，
	// 由业务层按“有执行权限”授权，这里只保证状态与动作合法。
	if a.CurrentStatus == StatusPassed {
		return nil
	}
	// 审批通过 / 驳回：先判断禁止自审（见 selfAuditDenied），再判断超级用户放行。
	if err := selfAuditDenied(repo, a, actor); err != nil {
		return err
	}
	if actor.IsSuperuser {
		return nil // 超级用户可以审批所有工单
	}
	code, needed, known := reviewButton(a.WorkflowType, action)
	if !known {
		return errf("不支持的工单类型")
	}
	if needed && needButton {
		has, err := repo.HasButton(actor.ID, code)
		if err != nil {
			return err
		}
		if !has {
			return errf("用户无相关审批权限，请合理配置权限")
		}
	}
	gid, err := strconv.ParseUint(a.CurrentAudit, 10, 32)
	if err != nil {
		return errf("当前审批权限组不存在，请联系管理员检查并清洗错误数据")
	}
	g, err := repo.AuthGroup(uint(gid))
	if err != nil {
		return err
	}
	if g == nil {
		return errf("当前审批权限组不存在，请联系管理员检查并清洗错误数据")
	}
	inGroup, err := repo.UserInAuthGroup(actor.ID, g.ID)
	if err != nil {
		return err
	}
	if !inGroup {
		return errf("用户不在当前节点的审核组内，无权限审核")
	}
	users, err := repo.AuthGroupUsers([]string{g.Name}, a.GroupID)
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.ID == actor.ID {
			return nil
		}
	}
	return errf("用户不在流程相关资源组内，无权限审核")
}

// BanSelfAudit 返回“禁止审批自己提交的工单”是否生效：approval.forbid_self_approve 未设置（或为空）时默认生效，
// 只有显式设为 false（也接受 0 / no / off / n，不区分大小写）才允许自审。
func BanSelfAudit(repo Repo) bool {
	v, ok := repo.ConfigValue(settings.KeyApprovalForbidSelf)
	if !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "no", "off", "n", "f":
		return false
	}
	return true
}

// selfAuditStrict 判断这类工单是否“连超级用户也不能审批自己的单”：查询权限申请、数据导出工单、数据归档。
// SQL 上线工单（非导出）保持超级用户可自审。
func selfAuditStrict(a *model.WorkflowAudit) bool {
	switch a.WorkflowType {
	case TypeQueryPrivilege, TypeArchive:
		return true
	case TypeSQLReview:
		return a.IsExport
	}
	return false
}

// selfAuditDenied 实现禁止自审：审批人就是提交人时，approval.forbid_self_approve 生效（默认）则普通用户一律拒绝，
// 超级用户仅在“严格类”工单（selfAuditStrict）上拒绝；approval.forbid_self_approve 显式为 false 时不限制。
func selfAuditDenied(repo Repo, a *model.WorkflowAudit, actor Actor) error {
	if actor.Username == "" || actor.Username != a.CreateUser {
		return nil
	}
	if !BanSelfAudit(repo) {
		return nil
	}
	if actor.IsSuperuser && !selfAuditStrict(a) {
		return nil
	}
	return errf("当前配置禁止本人审核自己的工单")
}

func statusLabel(s int) string {
	switch s {
	case StatusWaiting:
		return "待审核"
	case StatusPassed:
		return "审核通过"
	case StatusRejected:
		return "审核不通过"
	case StatusAborted:
		return "审核取消"
	}
	return strconv.Itoa(s)
}

// StatusLabel 返回状态的显示名。
func StatusLabel(s int) string { return statusLabel(s) }

// Operate 执行审批动作（通过 / 驳回 / 撤回）：检查权限、更新审批主表、写明细与日志。
// 返回写入的审批明细。业务表（如权限申请）的状态更新与授权由调用方在同一事务里完成。
func Operate(repo Repo, now time.Time, a *model.WorkflowAudit, action int, actor Actor, remark string) (*model.WorkflowAuditDetail, error) {
	if err := CanOperate(repo, a, action, actor); err != nil {
		return nil, err
	}
	var info string
	var detailStatus int
	switch action {
	case ActionPass:
		detailStatus = StatusPassed
		if a.NextAudit == "-1" {
			a.CurrentAudit, a.CurrentStatus = "-1", StatusPassed
			info = fmt.Sprintf("审批备注: %s, 无下级审批", remark)
		} else {
			groups := splitIDs(a.AuditAuthGroups)
			pos := -1
			for i, g := range groups {
				if g == a.NextAudit {
					pos = i
				}
			}
			if pos < 0 {
				return nil, errKind(KindConfig, "审批流配置错误：审批节点 %s 不在审批流内（审核ID %d）", a.NextAudit, a.AuditID)
			}
			a.CurrentStatus, a.CurrentAudit = StatusWaiting, a.NextAudit
			if pos+1 >= len(groups) {
				a.NextAudit = "-1"
			} else {
				a.NextAudit = groups[pos+1]
			}
			info = fmt.Sprintf("审批备注：%s, 下级审批：%s", remark, groupName(repo, a.CurrentAudit))
		}
	case ActionReject:
		detailStatus = StatusRejected
		a.CurrentAudit, a.NextAudit, a.CurrentStatus = "-1", "-1", StatusRejected
		info = "审批备注：" + remark
	case ActionAbort:
		detailStatus = StatusAborted
		a.NextAudit, a.CurrentStatus = "-1", StatusAborted
		info = "取消原因：" + remark
	}
	if err := repo.SaveAudit(a); err != nil {
		return nil, err
	}
	d := &model.WorkflowAuditDetail{AuditID: a.AuditID, AuditUser: actor.Username, AuditTime: now, AuditStatus: detailStatus, Remark: remark}
	if err := repo.InsertDetail(d); err != nil {
		return nil, err
	}
	err := repo.InsertLog(&model.WorkflowLog{
		AuditID: a.AuditID, OperationType: action, OperationTypeDesc: ActionLabel[action],
		OperationInfo: truncate(info, 1000), Operator: actor.Username, OperatorDisplay: actor.Display, OperationTime: now,
	})
	return d, err
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Node 是审批流里的一个节点，用于展示。
type Node struct {
	GroupID uint
	Name    string
	Current bool // 当前正在审批
	Passed  bool // 已通过（仅在总体待审核时有意义）
}

// ReviewNodes 返回审批流各节点的状态：
// 总体待审核时，当前节点之前为已通过、当前节点为当前、之后为未通过；总体为其他状态时节点不设置状态。
func ReviewNodes(repo Repo, a *model.WorkflowAudit) []Node {
	var nodes []Node
	met := false
	for _, s := range splitIDs(a.AuditAuthGroups) {
		id, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			continue
		}
		n := Node{GroupID: uint(id), Name: groupName(repo, s)}
		if a.CurrentStatus == StatusWaiting {
			switch {
			case s == a.CurrentAudit:
				n.Current, met = true, true
			case !met:
				n.Passed = true
			}
		}
		nodes = append(nodes, n)
	}
	return nodes
}

// CanReview 判断用户当前能否审批这张工单（用于页面决定是否显示审批按钮），规则同 CanOperate 的通过/驳回分支。
func CanReview(repo Repo, a *model.WorkflowAudit, actor Actor) bool {
	if a.CurrentStatus != StatusWaiting {
		return false
	}
	return CanOperate(repo, a, ActionPass, actor) == nil
}

// IsCurrentApprover 判断用户是不是这张工单当前审批节点上的审批人：节点的权限组 + 工单所属资源组的成员，并满足禁止自审 / 超级用户的规则；
// 不看审批按钮。“修改可执行时间”这类只需要“人在当前节点上”的动作用它，各自的按钮（sqlworkflow.alter_window）由调用方校验。
func IsCurrentApprover(repo Repo, a *model.WorkflowAudit, actor Actor) bool {
	if a.CurrentStatus != StatusWaiting {
		return false
	}
	return canOperate(repo, a, ActionPass, actor, false) == nil
}
