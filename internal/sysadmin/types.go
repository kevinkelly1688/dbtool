// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

package sysadmin

import "time"

// 工单类型（数值存在 dbt_workflow_audit_setting.workflow_type 里）。
const (
	WorkflowQuery   = 1 // 查询权限申请
	WorkflowSQL     = 2 // SQL上线申请
	WorkflowArchive = 3 // 数据归档申请
)

// WorkflowTypeInfo 描述一种工单类型及其审批人需要持有的审批按钮（同 internal/audit 的 reviewButton）；
// ReviewButton 为空表示这类工单没有单独的审批按钮（数据归档申请）：该级权限组 + 资源组内的成员都能审。
type WorkflowTypeInfo struct {
	Type         int    `json:"type"`
	Label        string `json:"label"`
	ReviewButton string `json:"review_button"`
}

// WorkflowTypes 是审批流支持的全部工单类型，顺序即页面展示顺序。
var WorkflowTypes = []WorkflowTypeInfo{
	{WorkflowSQL, "SQL上线申请", "sqlworkflow.approve"},
	{WorkflowQuery, "查询权限申请", "qpriv.review"},
	{WorkflowArchive, "数据归档申请", ""},
}

func typeInfo(t int) (WorkflowTypeInfo, bool) {
	for _, w := range WorkflowTypes {
		if w.Type == t {
			return w, true
		}
	}
	return WorkflowTypeInfo{}, false
}

// IDName 是一个 ID + 名称。
type IDName struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

// UserRef 是用户的简要信息（关联对象选择、成员列表用）。
type UserRef struct {
	ID       uint   `json:"id"`
	Username string `json:"username"`
	Display  string `json:"display"`
	IsActive bool   `json:"is_active"`
}

// InstanceRef 是实例的简要信息。
type InstanceRef struct {
	ID     uint   `json:"id"`
	Name   string `json:"name"`
	DBType string `json:"db_type"`
	Type   string `json:"type"`
}

// Approver 是审批流某一级里的一个用户（资源组内、属于该权限组、启用）。
type Approver struct {
	ID       uint
	Username string
	Display  string

	// 以下三项只用于配置页的预览 / 告警（按按钮权限计算“可审”、识别账号已锁定），不参与实际审批。
	Super        bool       // 超级用户
	FailedCount  int        // 登录失败次数
	LastFailedAt *time.Time // 最近一次登录失败时间
}

// AuthGroupRow 是权限组列表的一行（含统计）。
type AuthGroupRow struct {
	ID      uint
	Name    string
	Members int
}

// ---- 查询过滤条件 ----

type AuditLogFilter struct {
	Action string
	User   string // 用户名模糊匹配
	Search string // 动作 / 额外信息 / 用户名模糊匹配
	Start  *time.Time
	End    *time.Time // 不含
	Offset int
	Limit  int
}

type QueryLogFilter struct {
	User     string // 用户名精确
	Instance string // 实例名精确
	Search   string // SQL / 姓名 / 别名模糊匹配
	Start    *time.Time
	End      *time.Time // 不含
	Offset   int
	Limit    int
}

type SQLWorkflowFilter struct {
	Status  string
	GroupID uint
	Search  string // 工单名 / 提交人模糊匹配
	Start   *time.Time
	End     *time.Time // 不含
	Offset  int
	Limit   int
}

// SQLWorkflowRow 是 SQL 上线审计列表的一行（已关联实例名）。
type SQLWorkflowRow struct {
	ID              uint
	Name            string
	Engineer        string
	EngineerDisplay string
	Status          string
	GroupName       string
	InstanceName    string
	DBName          string
	SyntaxType      int
	IsOfflineExport int
	CreateTime      time.Time
	FinishTime      *time.Time
}

// 用户状态筛选。
const (
	StatusActive   = "active"
	StatusLocked   = "locked"
	StatusInactive = "inactive"
	StatusNo2FA    = "no2fa"
	StatusSuper    = "superuser" // 超级用户（启用与否都算）
)

// 用户列表排序（白名单）。空值 = 用户名升序（沿用旧行为）。
const (
	SortUsername  = "username"
	SortLastLogin = "last_login" // 最近登录在前，从未登录的排最后
	SortStatus    = "status"     // 已停用 / 锁定的在前，便于处理
)

type UserFilter struct {
	Search          string // 用户名 / 姓名 / 邮箱模糊匹配
	DeptUserIDs     []uint // 部门名命中的用户 ID（与 Search 是“或”的关系；部门在旁路表里）
	AuthGroupID     uint
	ResourceGroupID uint
	Status          string
	Sort            string
	LockCount       int           // 锁定判定：失败次数达到该值
	LockWindow      time.Duration // 且最近一次失败在这段时间内
	Now             time.Time
	Offset          int
	Limit           int
}

type ResGroupFilter struct {
	Search         string
	IncludeDeleted bool
}
