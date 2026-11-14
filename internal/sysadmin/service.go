// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package sysadmin

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/query"
)

// Repo 是本包需要的全部数据访问。InTx 内部的写操作要么一起提交，要么一起回滚。
type Repo interface {
	InTx(fn func(Repo) error) error
	ConfigValue(item string) (string, bool)
	// AuthSetting 读登录与安全设置（系统设置 dbt_setting 里 security.*，显式设置过才有值）；表不存在或没有该项时返回 ok=false。
	AuthSetting(key string) (string, bool)

	// 审计日志与审计页
	InsertAuditLog(e *model.AuditEntry) error
	ListAuditLogs(f AuditLogFilter) ([]model.AuditEntry, int64, error)
	AuditActions() ([]string, error)
	ListQueryLogs(f QueryLogFilter) ([]model.QueryLog, int64, error)
	ListSQLWorkflows(f SQLWorkflowFilter) ([]SQLWorkflowRow, int64, error)

	// 用户
	GetUser(id uint) (*model.User, error) // 不存在返回 nil, nil
	GetUserByName(name string) (*model.User, error)
	ListUsers(f UserFilter) ([]model.User, int64, error)
	CreateUser(u *model.User) error
	UpdateUser(id uint, fields map[string]any) error
	// CountOtherSuperusers 返回除 excludeID 外启用的超级用户数。事务内调用时对这些行加锁，避免两个管理员同时降级对方导致没有超级用户。
	CountOtherSuperusers(excludeID uint) (int64, error)
	UserAuthGroups(userIDs []uint) (map[uint][]IDName, error)
	// UserResourceGroups 只返回未停用的资源组；SetUserResourceGroups 同样只管理未停用的资源组（已停用组里的关联保持不变）。
	UserResourceGroups(userIDs []uint) (map[uint][]IDName, error)
	UserTwoFATypes(userIDs []uint) (map[uint][]string, error)
	SetUserAuthGroups(userID uint, ids []uint) error
	SetUserResourceGroups(userID uint, ids []uint) error
	DeleteUserTwoFA(userID uint) (int64, error)

	// 权限组（dbt_auth_group）
	ListAuthGroups() ([]AuthGroupRow, error)
	GetAuthGroup(id uint) (*model.AuthGroup, error)
	AuthGroupByName(name string) (*model.AuthGroup, error) // 按数据库排序规则比较（通常不区分大小写）
	CreateAuthGroup(g *model.AuthGroup) error
	RenameAuthGroup(id uint, name string) error
	// DeleteAuthGroup 删除权限组及其成员关系（早期版本留在 dbt_auth_group_permissions 里的行一并清掉）。
	DeleteAuthGroup(id uint) error
	AuthGroupMembers(id uint) ([]UserRef, error)
	PendingAuditsUsingGroup(authGroupID uint) (int64, error)

	// 资源组
	ListResourceGroups(f ResGroupFilter) ([]model.ResourceGroup, error)
	GetResourceGroup(id uint) (*model.ResourceGroup, error)
	ResourceGroupByName(name string) (*model.ResourceGroup, error) // 含已停用的
	CreateResourceGroup(g *model.ResourceGroup) error
	UpdateResourceGroup(id uint, fields map[string]any) error
	ResourceGroupCounts() (users, instances map[uint]int, err error)
	GroupUsers(groupID uint) ([]UserRef, error)
	GroupInstances(groupID uint) ([]InstanceRef, error)
	AllUsers() ([]UserRef, error)
	AllInstances() ([]InstanceRef, error)
	AddGroupUsers(groupID uint, ids []uint) error
	RemoveGroupUsers(groupID uint, ids []uint) error
	AddGroupInstances(groupID uint, ids []uint) error
	RemoveGroupInstances(groupID uint, ids []uint) error
	// OpenWorkflowCount 返回该资源组里进行中的工单数：待审批的审批流 + 已通过但尚未结束的 SQL 工单。
	OpenWorkflowCount(groupID uint) (int64, error)

	// 审批流配置（dbt_workflow_audit_setting）
	ListAuditSettings() ([]model.WorkflowAuditSetting, error)
	GetAuditSetting(groupID uint, workflowType int) (*model.WorkflowAuditSetting, error)
	SaveAuditSetting(groupID uint, groupName string, workflowType int, authGroups string) error
	DeleteAuditSetting(groupID uint, workflowType int) error
	RenameAuditSettingGroup(groupID uint, name string) error
	ReviewApprovers(authGroupID, resourceGroupID uint) ([]Approver, error)
}

// Service 是系统管理的业务逻辑。
type Service struct {
	repo Repo
	now  func() time.Time
	// Iterations 是新密码的 pbkdf2 迭代次数，默认 390000。
	Iterations int
	// btn 是按钮权限引擎（审批流预览按按钮权限计算“可审”）；nil 时用 btnperm.Default()，再没有（测试 / 演示）需要审批按钮的类型一律按“不可审”处理。
	btn buttonEngine

	actMu   sync.Mutex
	actAt   time.Time
	actions []string
}

func NewService(repo Repo) *Service {
	return &Service{repo: repo, now: time.Now, Iterations: 390000}
}

func bad(msg string) error      { return query.NewError(400, "bad_request", msg) }
func forbid(msg string) error   { return query.NewError(403, "forbidden", msg) }
func notFound(msg string) error { return query.NewError(404, "not_found", msg) }
func conflict(msg string) error { return query.NewError(409, "conflict", msg) }

// Admin 用数据库里的最新状态确认操作者仍是启用的超级用户，并返回带最新姓名的 Actor。
// 会话 / JWT 里的“超级用户”标记在有效期内不会随数据库变化，管理类操作不能只信它。
func (s *Service) Admin(id uint) (audit.Actor, error) {
	u, err := s.repo.GetUser(id)
	if err != nil {
		return audit.Actor{}, err
	}
	if u == nil || !u.IsActive || !u.IsSuperuser {
		return audit.Actor{}, forbid("仅超级用户可以操作")
	}
	return audit.Actor{ID: u.ID, Username: u.Username, Display: u.Display, IsSuperuser: true}, nil
}

// Operator 返回启用状态的操作者（不要求超级用户，如审计页上拥有 audit.view 的用户），用来补齐姓名写审计。
func (s *Service) Operator(id uint) (audit.Actor, error) {
	u, err := s.repo.GetUser(id)
	if err != nil {
		return audit.Actor{}, err
	}
	if u == nil || !u.IsActive {
		return audit.Actor{}, forbid("账号不可用")
	}
	return audit.Actor{ID: u.ID, Username: u.Username, Display: u.Display, IsSuperuser: u.IsSuperuser}, nil
}

// 写入 dbt_audit_log 的字段长度（表里 user_name 30、user_display 50、action 255）。
func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// writeAudit 在 r（通常是事务内的 Repo）里写一条操作审计。extra 里不得出现密码、webhook 等敏感值。
func (s *Service) writeAudit(r Repo, a audit.Actor, action, extra string) error {
	return r.InsertAuditLog(&model.AuditEntry{
		UserID: a.ID, UserName: trunc(a.Username, 30), UserDisplay: trunc(a.Display, 50),
		Action: trunc(action, 255), ExtraInfo: trunc(extra, 4000), ActionTime: s.now(),
	})
}

// tx 在事务里执行 fn，并在同一事务里写操作审计（审计写失败则整体回滚）。
func (s *Service) tx(a audit.Actor, action string, fn func(r Repo) (extra string, err error)) error {
	return s.repo.InTx(func(r Repo) error {
		extra, err := fn(r)
		if err != nil {
			return err
		}
		return s.writeAudit(r, a, action, extra)
	})
}

// Page 把页码参数规整成 offset/limit。
func Page(page, size int) (offset, limit, p int) {
	if size <= 0 {
		size = 20
	}
	if size > 200 {
		size = 200
	}
	if page < 1 {
		page = 1
	}
	return (page - 1) * size, size, page
}

const timeLayout = "2006-01-02 15:04:05"

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(timeLayout)
}

func fmtTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return fmtTime(*t)
}

// dedupIDs 去掉重复 ID 并保持顺序，拒绝 0。
func dedupIDs(ids []uint) ([]uint, error) {
	seen := map[uint]bool{}
	out := make([]uint, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			return nil, bad("ID 不能为 0")
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

func joinNames(names []string) string {
	if len(names) == 0 {
		return "（无）"
	}
	return strings.Join(names, "、")
}

// diffSets 返回 after 相对 before 新增与移除的元素（保持各自顺序）。
func diffSets(before, after []string) (added, removed []string) {
	b, a := map[string]bool{}, map[string]bool{}
	for _, x := range before {
		b[x] = true
	}
	for _, x := range after {
		a[x] = true
	}
	for _, x := range after {
		if !b[x] {
			added = append(added, x)
		}
	}
	for _, x := range before {
		if !a[x] {
			removed = append(removed, x)
		}
	}
	return
}

func describeDiff(label string, before, after []string) string {
	added, removed := diffSets(before, after)
	if len(added) == 0 && len(removed) == 0 {
		return ""
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "新增"+label+" "+joinNames(added)) // i18n-ignore 存库的审计说明
	}
	if len(removed) > 0 {
		parts = append(parts, "移除"+label+" "+joinNames(removed)) // i18n-ignore 存库的审计说明
	}
	return strings.Join(parts, "；")
}

func errf(format string, a ...any) string { return fmt.Sprintf(format, a...) }
