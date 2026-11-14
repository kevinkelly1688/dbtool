// @author: kevinkelly
// @license: Apache Licence
// @file: repo.go
// @time: 2026/10/28

package authx

import (
	"time"

	"dbtool/internal/model"
)

// IDName 是 (ID, 名称) 对，用于下拉选项。
type IDName struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

// BoundIdentity 是用户已绑定的一个外部身份（不含外部唯一 ID）。
type BoundIdentity struct {
	ID         uint
	ProviderID uint
	CreateTime time.Time
}

// ProfileRepo 是仓库可选实现的“用户资料扩展”（旁路表 dbt_user_profile，由系统管理维护）：
// 表不存在时各方法返回零值，调用方不能因此失败。
type ProfileRepo interface {
	// UserDept 返回部门（没有为空）。
	UserDept(uid uint) string
	// MustChangePassword 返回是否被要求下次登录修改密码。
	MustChangePassword(uid uint) bool
	// ClearMustChange 清除“必须修改密码”标记。
	ClearMustChange(uid uint) error
}

// TwoFAConfig 是用户已绑定的一种两步验证方式（对应 dbt_2fa_config 表一行，值已解密）。
type TwoFAConfig struct {
	Type   string // totp / sms
	Phone  string
	Secret string
}

// Repo 是本包需要的数据访问；store.go 是 GORM 实现，测试里用内存假实现。
type Repo interface {
	// ---- 用户 ----
	UserByID(id uint) (*model.User, error) // 不存在返回 nil, nil
	UserByName(name string) (*model.User, error)
	UsersByEmail(email string) ([]model.User, error)
	UserByThirdID(kind, id string) (*model.User, error) // kind: ding / feishu / slack
	CreateUser(u *model.User) error
	// RecordLoginFailure 把失败计数加一（上限 127）并记录时间，返回新计数。
	RecordLoginFailure(uid uint, at time.Time) (int, error)
	// ResetLoginFailures 清零失败计数。
	ResetLoginFailures(uid uint) error
	// MarkLogin 记录登录成功：last_login = at，失败计数清零。
	MarkLogin(uid uint, at time.Time) error
	UpdateProfile(uid uint, display, email string) error
	// SetPassword 写入新的密码哈希（pbkdf2_sha256 格式）。
	SetPassword(uid uint, hash string) error
	// UsersByPhone 返回已绑定该手机号（短信两步验证）的启用用户，最多几个（匹配依据“手机号”用）。
	UsersByPhone(phone string) ([]model.User, error)
	SetThirdID(uid uint, kind, id string) error
	// ListUsers 返回启用的用户（API 白名单选择用），最多 limit 个。
	ListUsers(limit int) ([]IDName, error)

	// ---- 组 ----
	ListAuthGroups() ([]IDName, error)
	ListResourceGroups() ([]IDName, error)
	AuthGroupIDsByName(names []string) ([]uint, error)
	ResourceGroupIDsByName(names []string) ([]uint, error)
	AddUserAuthGroups(uid uint, ids []uint) error     // 幂等
	AddUserResourceGroups(uid uint, ids []uint) error // 幂等

	// ---- 登录方式 ----
	ListProviders() ([]Provider, error)     // 按 Sort, ID 升序
	GetProvider(id uint) (*Provider, error) // 不存在返回 nil, nil
	// SaveProvider 新增（ID=0，回填 ID）或整体更新；Config 里的密钥由实现负责加密。
	SaveProvider(p *Provider) error
	DeleteProvider(id uint) error
	// SetProviderTest 记录测试结果。
	SetProviderTest(id uint, ok bool, msg, hash string, at time.Time) error
	// SetProviderEnabled 启用 / 停用。
	SetProviderEnabled(id uint, enabled bool) error
	SetProviderOrder(ids []uint) error
	TouchProviderLogin(id uint, at time.Time) error
	// 外部身份绑定：(登录方式, 外部唯一 ID) → 平台用户。
	IdentityUser(providerID uint, subject string) (uint, error) // 没有返回 0, nil
	BindIdentity(providerID uint, subject string, uid uint) error
	DeleteIdentity(providerID uint, subject string) error
	CountIdentities(providerID uint) (int, error)
	// IdentitiesByUser 返回某用户已绑定的外部身份（个人“账号绑定”页用）。
	IdentitiesByUser(uid uint) ([]BoundIdentity, error)
	// DeleteIdentityByID 解除绑定：只删属于该用户的那一条，返回是否删到。
	DeleteIdentityByID(id, uid uint) (bool, error)
	// CountTwoFA 返回启用用户总数与其中已绑定两步验证的用户数（安全设置页概览用）。
	CountTwoFA() (bound, total int, err error)

	// ---- 设置 ----
	ConfigValues(items []string) (map[string]string, error) // dbt_setting（值已解密；不存在的不返回）
	SetConfigValues(kv map[string]string) error             // dbt_setting（写入时加密）
	AuthSettings() (map[string]string, error)               // 登录与安全里密码策略 / 会话 / 水印样式等设置中显式设置过的项（系统设置 dbt_setting）
	SetAuthSettings(kv map[string]string, by string) error  // 写系统设置（dbt_setting）

	// ---- 两步验证（dbt_2fa_config）----
	TwoFAConfigs(uid uint) ([]TwoFAConfig, error)
	SaveTwoFA(uid uint, username string, c TwoFAConfig) error // 同类型旧配置被替换
	DeleteTwoFA(uid uint, typ string) error

	// ---- 审计 ----
	InsertAudit(e *model.AuditEntry) error
}
