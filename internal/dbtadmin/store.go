// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"errors"
	"time"

	"dbtool/internal/settings"
)

// 工具用到的表名（常量集中在这里，真实实现与测试用同一份）。本系统所有表统一带 dbt_ 前缀：
const (
	tblUsers       = "dbt_sql_users"
	tblGroups      = "dbt_auth_group"
	tblUserGroups  = "dbt_sql_users_groups"
	tblAuditLog    = "dbt_audit_log"
	tblAuditExt    = "dbt_audit_ext"
	tblUserProfile = "dbt_user_profile"
	tblSetting     = "dbt_setting"
	tblInstance    = "dbt_sql_instance"
	tblPermission  = "dbt_auth_permission"
	// TblApplied 记录已应用的脚本文件。
	TblApplied = "dbt_bootstrap_applied"
)

// ErrTableMissing 表示操作需要的表在目标库里不存在。
var ErrTableMissing = errors.New("表不存在")

// ErrDatabaseMissing 表示连接串里的库不存在（MySQL 1049）。
var ErrDatabaseMissing = errors.New("目标库不存在")

// ServerInfo 是连接上服务器后读到的信息。
type ServerInfo struct {
	Version  string
	Database string // DATABASE()
}

// User 是 dbt_sql_users 里工具关心的列（不含密码哈希）。
type User struct {
	ID               uint
	Username         string
	Display          string
	Email            string
	IsSuperuser      bool
	IsStaff          bool
	IsActive         bool
	LastLogin        *time.Time
	FailedLoginCount int
	LastFailedAt     *time.Time
}

// NewUser 是新建用户的输入；PasswordHash 是已编码的哈希。
type NewUser struct {
	Username     string
	Display      string
	Email        string
	PasswordHash string
	IsSuperuser  bool
	IsStaff      bool
	IsActive     bool
	DateJoined   time.Time
}

// AuditEntry 是写入 dbt_audit_log 的一行。
type AuditEntry struct {
	UserName    string
	UserDisplay string
	Action      string
	ExtraInfo   string
	At          time.Time
}

// RekeyColumn 登记一张表里的 mirage 加密列（dbt-admin reencrypt 只会读写这里登记的列）。
type RekeyColumn struct {
	Table  string
	RefCol string // 便于人认的名字所在的列（实例名 / 隧道名 / 配置项名）；空 = 用 id
	Cols   []string
	IDCol  string // 主键列；空 = id
	Where  string // 只处理满足条件的行（写死的常量，不含用户输入）；空 = 全部
}

// RekeyColumns 是 reencrypt 处理的表。dbt_setting 的主键是 skey，且只有 encrypted=1 的行是密文。
var RekeyColumns = []RekeyColumn{
	{Table: tblInstance, RefCol: "instance_name", Cols: []string{"user", "password"}},
	{Table: "dbt_ssh_tunnel", RefCol: "tunnel_name", Cols: []string{"user", "password", "pkey", "pkey_password"}},
	{Table: "dbt_instance_account", Cols: []string{"user", "password"}},
	{Table: tblSetting, IDCol: "skey", Where: "encrypted = 1", Cols: []string{"svalue"}},
	{Table: "dbt_2fa_config", Cols: []string{"username", "auth_type", "phone", "secret_key"}},
}

// Cell 是一个加密列里的一个值。Value 是密文或明文，只能分类 / 换密钥，不得输出。
type Cell struct {
	Table, Column string
	ID            string // 主键 id（文本）
	Ref           string // 便于人认的名字：实例名 / 隧道名 / 配置项名；没有的是 id=N
	Value         string
	MaxLen        int // 列最多能放多少个字符；0 = 不限
}

// AuthInfo 是判断“密码是否由外部系统管理”所需的信息。
type AuthInfo struct {
	ProfileType string   // dbt_user_profile.auth_type
	Kinds       []string // 绑定的外部身份类型（dbt_auth_identity ⨝ dbt_auth_provider）
	MustChange  bool     // dbt_user_profile.must_change_password
	HasProfile  bool
}

// Store 是元数据库访问的抽象。一个 Store 对应一个固定的数据库会话（SET 语句在整个运行期内有效）。
type Store interface {
	Close() error
	ServerInfo(ctx context.Context) (ServerInfo, error)
	HasTable(ctx context.Context, name string) (bool, error)
	CountRows(ctx context.Context, table string) (int64, error)
	// CountBaseTables 返回当前库里的基础表数量。
	CountBaseTables(ctx context.Context) (int, error)
	// DatabaseCharset 返回当前库的默认字符集 / 校验规则（information_schema.SCHEMATA，只读）。
	DatabaseCharset(ctx context.Context) (DBCharset, error)
	// ExecStmt 在固定会话里执行一条语句（不带参数）。
	ExecStmt(ctx context.Context, stmt string) error

	// EnsureAppliedTable 创建 dbt_bootstrap_applied（IF NOT EXISTS）。
	EnsureAppliedTable(ctx context.Context) error
	// AppliedSHAs 返回 文件 → 上次记录的 sha256；表不存在时返回空 map。
	AppliedSHAs(ctx context.Context) (map[string]string, error)
	RecordApplied(ctx context.Context, file, sha, toolVersion string, at time.Time) error

	// AuthSetting / ConfigValue 读 dbt_setting 里显式设置的一项（原始值：敏感项是 mirage 密文，由调用方处理）。表不存在按“没有”处理。
	AuthSetting(ctx context.Context, key string) (string, bool, error)
	ConfigValue(ctx context.Context, item string) (string, bool, error)
	// SettingRows 返回 dbt_setting 的全部行（原始值）；表不存在 ready=false。
	SettingRows(ctx context.Context) (rows map[string]settings.Row, ready bool, err error)

	// SecretRows 返回自检要看的“可能加密”的值：dbt_setting 里 encrypted=1 的 (设置键, 值)、
	// 与 dbt_sql_instance 的 (实例名, 账号 / 密码)。
	// 只读；表不存在按“没有”处理。值是密文或明文，调用方只能分类统计，不得输出。
	SecretRows(ctx context.Context) ([]SecretRow, error)
	// EncryptedCells 返回 RekeyColumns 登记的各表里全部非空的加密列值（只读）；表 / 列不存在按“没有”处理。
	EncryptedCells(ctx context.Context) ([]Cell, error)

	ListUsers(ctx context.Context) ([]User, error)
	// PermMigrator 返回权限迁移入口（dbt-admin perm migrate 用）。
	PermMigrator(ctx context.Context) (PermMigrator, error)
	// InTx 在一个事务里执行 fn：fn 返回 error 则回滚。
	InTx(ctx context.Context, fn func(Tx) error) error
}

// Tx 是事务内可用的用户 / 审计操作。
type Tx interface {
	// GetUser 按用户名取用户（加行锁）；不存在返回 nil。
	GetUser(username string) (*User, error)
	// CountOtherActiveSuperusers 统计除 excludeID 外启用的超级用户数。
	CountOtherActiveSuperusers(excludeID uint) (int, error)
	// GroupIDs 按名称取权限组 ID；不存在的名称不在结果里。
	GroupIDs(names []string) (map[string]uint, error)
	InsertUser(u NewUser) (uint, error)
	AddUserGroups(userID uint, groupIDs []uint) error
	// SetPassword 设置密码哈希并清零失败计数 / 失败时间。
	SetPassword(userID uint, hash string) error
	// ClearFailedLogins 失败计数清零、失败时间置空。
	ClearFailedLogins(userID uint) error
	SetActive(userID uint, active bool) error
	AuthInfo(userID uint) (AuthInfo, error)
	// SetMustChange 写 dbt_user_profile.must_change_password；表不存在返回 ErrTableMissing。
	SetMustChange(userID uint, v bool, by string, at time.Time) error
	// InsertAudit 写 dbt_audit_log（及 dbt_audit_ext，若存在）；dbt_audit_log 不存在返回 ErrTableMissing。
	InsertAudit(e AuditEntry) error
	// ReplaceCell 把 RekeyColumns 登记的 (table, column, id) 的值从 oldValue 改成 newValue。条件是列值仍等于 oldValue
	// （按字节比较）：被别人改过则不动并返回 false。table / column 不在登记里返回错误。
	ReplaceCell(table, column, id, oldValue, newValue string) (bool, error)
	// ApplySettings 写 dbt_setting 与 dbt_setting_history（语义同 settings.WriteTx）。
	ApplySettings(writes []settings.Write, hist []settings.History) error
}
