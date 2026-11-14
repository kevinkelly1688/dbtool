// @author: kevinkelly
// @license: Apache Licence
// @file: ext.go
// @time: 2026/10/28

package sysadmin

import (
	"errors"
	"strings"
	"time"

	"dbtool/internal/i18n"
)

// 旁路表（deploy/sql/080_sysadmin_ext.sql）：资源组负责人 / 备注、用户部门 / 认证方式 / 强制改密、权限组说明。
// 这些字段所在的表（dbt_resource_group / dbt_sql_users / dbt_auth_group）不加列，所以放在一对一的扩展表里。
// 表不存在时：读取返回空值，页面照常工作；写入非空值时返回 errExtMissing（业务层转成提示），写入空值直接忽略。

const (
	tblResGroupExt  = "dbt_resource_group_ext"
	tblUserProfile  = "dbt_user_profile"
	tblAuthGroupExt = "dbt_auth_group_ext"
	extSQLFile      = "deploy/sql/080_sysadmin_ext.sql"
)

// errExtMissing 表示旁路表还没有创建。
var errExtMissing = errors.New("sysadmin: 扩展表尚未创建") // i18n-ignore 内部哨兵错误，业务层转成 extMissingErr 的提示

// ResGroupExt 是资源组的扩展字段。
type ResGroupExt struct {
	Leader string
	Remark string
}

// UserProfile 是用户的扩展字段。AuthType 空 / local 表示本地认证。
type UserProfile struct {
	Dept               string
	AuthType           string
	MustChangePassword bool
}

func (p UserProfile) zero() bool { return p.Dept == "" && p.AuthType == "" && !p.MustChangePassword }

// OpenWorkflow 是资源组上一个进行中的工单（拒绝停用时列出）。
type OpenWorkflow struct {
	Kind   string `json:"kind"`   // sql / query_priv / archive
	Label  string `json:"label"`  // SQL 上线 / 查询权限 / 数据归档
	ID     uint   `json:"id"`     // 工单 ID（SQL 工单 ID、权限申请 ID）
	Name   string `json:"name"`   // 工单名称 / 标题
	Status string `json:"status"` // 待审批 / 审核通过 / 定时执行 / 排队中 / 执行中
	Href   string `json:"href"`   // 详情页地址，没有则为空
}

// ResGroupRefs 是资源组被各处引用的数量，彻底删除前逐项检查。
type ResGroupRefs struct {
	Users          int64 // 关联的用户
	Instances      int64 // 关联的实例
	SQLWorkflows   int64 // 历史 SQL 上线 / 导出工单
	AuditFlows     int64 // 审批工单（dbt_workflow_audit，含查询权限申请）
	PrivApplies    int64 // 查询权限申请
	PerfIssues     int64 // 性能工单（团队）
	NotifyChannels int64 // 通知渠道
	FlowSettings   int64 // 审批流配置（彻底删除时一并删除，不阻止）
}

// Blocking 返回会阻止彻底删除的引用说明（空表示可以删除）。
func (r ResGroupRefs) Blocking() []string { return r.blocking(i18n.ZH) }

// blocking 按 lang 生成引用说明（服务层用中文；HTTP 层在非中文请求里按请求语言重新生成，见 localizeErr）。
func (r ResGroupRefs) blocking(lang string) []string {
	var out []string
	add := func(n int64, key string) {
		if n > 0 {
			out = append(out, i18n.T(lang, key, "n", n))
		}
	}
	add(r.Users, "{n} 个关联用户")
	add(r.Instances, "{n} 个关联实例")
	add(r.SQLWorkflows, "{n} 个 SQL 上线 / 导出工单")
	add(r.AuditFlows, "{n} 条审批记录")
	add(r.PrivApplies, "{n} 个查询权限申请")
	add(r.PerfIssues, "{n} 个性能工单")
	add(r.NotifyChannels, "{n} 个通知渠道")
	return out
}

// ExtRepo 是旁路表的数据访问。*store 实现它；Service 通过类型断言取得（没有实现的仓库等同于“表不存在”）。
type ExtRepo interface {
	// TableReady 判断旁路表是否存在（带短缓存）。
	TableReady(table string) bool

	ResGroupExts(ids []uint) (map[uint]ResGroupExt, error) // ids 为空表示全部
	UpsertResGroupExt(id uint, e ResGroupExt, by string, at time.Time) error
	UserProfiles(ids []uint) (map[uint]UserProfile, error)
	UpsertUserProfile(id uint, p UserProfile, by string, at time.Time) error
	SetMustChangePassword(id uint, v bool, by string, at time.Time) error
	// UserIDsByDept 返回部门名包含 q 的用户 ID（用户搜索用）。
	UserIDsByDept(q string, limit int) ([]uint, error)
	AuthGroupDescs(ids []uint) (map[uint]string, error)
	UpsertAuthGroupDesc(id uint, desc string, by string, at time.Time) error
	DeleteAuthGroupExt(id uint) error
	// AuthGroupBuiltins 返回内置权限组（dbt_auth_group_ext.builtin=1，deploy/sql/150_builtin_groups.sql）的 ID 集合，ids 为空表示全部。
	// 表或 builtin 列不存在时返回空集合（等同没有内置组，不报错）。
	AuthGroupBuiltins(ids []uint) (map[uint]bool, error)

	// UserAuthKinds 返回用户已绑定的外部登录方式类型（dbt_auth_identity ⨝ dbt_auth_provider.kind，authx 维护，只读）。
	UserAuthKinds(ids []uint) (map[uint][]string, error)
	// UserLastLoginIP 从审计日志里取用户最近一次“登入”记录里的来源 IP（只读 dbt_audit_log）。
	UserLastLoginIP(username string) (string, error)

	// OpenWorkflows 列出资源组上进行中的工单（待审批的审批流 + 已通过但尚未结束的 SQL 工单），最多 limit 条。
	OpenWorkflows(groupID uint, limit int) ([]OpenWorkflow, error)
	ResGroupRefs(groupID uint) (ResGroupRefs, error)
	// EnabledChannelTypes 返回启用中的通知渠道的分布：键是资源组 ID（0 = 全局渠道，对所有资源组生效），值是 渠道类型 → 个数。
	// 渠道关联的资源组取法与通知发送一致：扩展表 group_ids 有就用它，否则只有 group_id 一个，都没有就是全局。表不存在按没有渠道处理。
	EnabledChannelTypes() (map[uint]map[string]int, error)
	// PurgeResourceGroup 物理删除资源组：审批流配置、扩展字段与资源组本身（调用方已确认没有引用）。
	PurgeResourceGroup(groupID uint) error
}

// nullExt 是没有旁路表时的空实现。
type nullExt struct{}

var _ ExtRepo = nullExt{}

func (nullExt) TableReady(string) bool                                       { return false }
func (nullExt) ResGroupExts([]uint) (map[uint]ResGroupExt, error)            { return map[uint]ResGroupExt{}, nil }
func (nullExt) UpsertResGroupExt(uint, ResGroupExt, string, time.Time) error { return errExtMissing }
func (nullExt) UserProfiles([]uint) (map[uint]UserProfile, error)            { return map[uint]UserProfile{}, nil }
func (nullExt) UpsertUserProfile(uint, UserProfile, string, time.Time) error { return errExtMissing }
func (nullExt) SetMustChangePassword(uint, bool, string, time.Time) error    { return errExtMissing }
func (nullExt) UserIDsByDept(string, int) ([]uint, error)                    { return nil, nil }
func (nullExt) AuthGroupDescs([]uint) (map[uint]string, error)               { return map[uint]string{}, nil }
func (nullExt) UpsertAuthGroupDesc(uint, string, string, time.Time) error    { return errExtMissing }
func (nullExt) DeleteAuthGroupExt(uint) error                                { return nil }
func (nullExt) AuthGroupBuiltins([]uint) (map[uint]bool, error)              { return map[uint]bool{}, nil }
func (nullExt) UserAuthKinds([]uint) (map[uint][]string, error)              { return map[uint][]string{}, nil }
func (nullExt) UserLastLoginIP(string) (string, error)                       { return "", nil }
func (nullExt) OpenWorkflows(uint, int) ([]OpenWorkflow, error)              { return nil, nil }
func (nullExt) ResGroupRefs(uint) (ResGroupRefs, error)                      { return ResGroupRefs{}, nil }
func (nullExt) EnabledChannelTypes() (map[uint]map[string]int, error) {
	return map[uint]map[string]int{}, nil
}
func (nullExt) PurgeResourceGroup(uint) error { return errExtMissing }

// extOf 取仓库（可能是事务内的仓库）的旁路表接口；仓库没有实现时返回空实现。
func extOf(r Repo) ExtRepo {
	if e, ok := r.(ExtRepo); ok {
		return e
	}
	return nullExt{}
}

func (s *Service) ext() ExtRepo { return extOf(s.repo) }

// extMissingErr 把 errExtMissing 转成给管理员看的提示；其他错误原样返回。
func extMissingErr(err error, what string) error {
	if errors.Is(err, errExtMissing) {
		return conflict(errf("%s需要先创建扩展表：在元数据库执行 %s 后重试", what, extSQLFile))
	}
	return err
}

// ---- 认证方式 ----

const (
	authLocal = "local"
	authLDAP  = "ldap"
)

var authKindLabels = map[string]string{
	"local": "本地", "ldap": "LDAP", "oidc": "OIDC", "dingtalk": "钉钉", "cas": "CAS", "feishu": "飞书", "slack": "Slack", "wecom": "企业微信",
}

// AuthLabel 返回认证方式的中文名。
func AuthLabel(kind string) string {
	if l, ok := authKindLabels[kind]; ok {
		return l
	}
	return kind
}

// authMethods 由用户扩展里的认证方式与外部身份绑定的类型合成认证方式列表（去重、保持顺序）；都没有则是本地。
func authMethods(profileType string, kinds []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(k string) {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, k)
	}
	add(profileType)
	for _, k := range kinds {
		add(k)
	}
	if len(out) == 0 {
		out = []string{authLocal}
	}
	return out
}

// passwordManagedExternally 判断密码是否由外部（LDAP）管理：认证方式里有 LDAP 而没有本地。
func passwordManagedExternally(methods []string) bool {
	ldap, local := false, false
	for _, m := range methods {
		switch m {
		case authLDAP:
			ldap = true
		case authLocal:
			local = true
		}
	}
	return ldap && !local
}
