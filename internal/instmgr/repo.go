// @author: kevinkelly
// @license: Apache Licence
// @file: repo.go
// @time: 2026/10/28

package instmgr

import (
	"time"

	"dbtool/internal/model"
)

// Repo 是本包需要的元数据库访问（store.go 用 GORM 实现，测试用内存假实现）。
// 所有“不存在”都返回 (nil, nil)，不用 gorm.ErrRecordNotFound。
type Repo interface {
	// ---- 实例 ----
	// ListInstances 按条件分页；scope 非 nil 时只返回其中的实例 ID（资源组可见范围）。返回的实例带 Tags、ResourceGroups。
	ListInstances(f InstanceFilter, scope map[uint]bool) ([]model.Instance, int64, error)
	GetInstance(id uint) (*model.Instance, error) // 带 Tunnel、Tags、ResourceGroups
	// BriefInstances 返回下拉框用的实例（只有基本字段，按名称排序）；scope 非 nil 时只返回其中的实例。
	BriefInstances(scope map[uint]bool) ([]model.Instance, error)
	InstanceOptions() (*InstanceOptions, error)
	// TunnelInfo 取解密后的隧道连接信息；不存在返回 (nil, nil)。
	TunnelInfo(id uint) (*TunnelInfo, error)
	InstanceNameTaken(name string, exceptID uint) (bool, error)
	// SaveInstance 新建（id=0）或更新实例及其资源组、标签；in.Password 为 nil 表示不修改密码。
	SaveInstance(id uint, in InstanceWrite) (uint, error)
	SetInstanceEnable(id uint, enable bool) error
	// InstanceRefs 返回引用该实例的记录统计（Count>0 的才返回）。
	InstanceRefs(id uint) ([]Ref, error)
	// DeleteInstance 删除实例及其配置类数据（账号/库登记、参数历史、资源组与标签关联）。调用前必须已确认没有阻塞引用。
	DeleteInstance(id uint) error
	// ConnInfo 解密凭据（含隧道），构造连接信息。
	ConnInfo(inst *model.Instance) (ConnInfo, error)
	// Decrypt 解密 mirage 字段（显示用户名用）。
	Decrypt(s string) string
	UserInstanceIDs(userID uint) (map[uint]bool, error)

	// ---- 数据库登记（dbt_instance_database）----
	SavedDatabases(instanceID uint) ([]DatabaseRecord, error)
	SaveDatabaseRecord(instanceID uint, rec DatabaseRecord) error
	ActiveUser(username string) (*model.User, error)
	ActiveUsers() ([]model.User, error)

	// ---- 账号登记（dbt_instance_account）----
	SavedAccounts(instanceID uint) ([]AccountRecord, error)
	// SaveAccountRecord 新增或更新登记；rec.Password 为 nil 表示不修改已登记的密码。
	SaveAccountRecord(instanceID uint, rec AccountRecord) error
	DeleteAccountRecord(instanceID uint, user, host string) error

	// ---- 参数 ----
	ListTemplates(dbType string) ([]ParamTemplate, error)
	GetTemplate(dbType, name string) (*ParamTemplate, error)
	GetTemplateByID(id uint) (*ParamTemplate, error)
	SaveTemplate(t *ParamTemplate) error // ID=0 新建，否则更新
	InsertParamHistory(h *ParamHistory) error
	ListParamHistory(instanceID uint, q string, offset, limit int) ([]ParamHistoryRow, int64, error)

	// ---- 审计 ----
	InsertAudit(e *model.AuditEntry) error
}

// InstanceFilter 是实例列表的筛选条件。
type InstanceFilter struct {
	Role    string // master / slave
	DBType  string
	Tag     string // 标签代码（只匹配启用的标签）
	Enabled *bool  // 慢日志采集开关
	State   string // disabled 只看已停用 / enabled 排除已停用（旁路表 dbt_instance_ext；表不存在时 disabled 无结果、enabled 不过滤）
	Env     string // prod / test（旁路表）
	Q       string // 实例名称 / HOST 关键字
	Offset  int
	Limit   int
}

// InstanceWrite 是写入实例表单的内容（已校验）。
type InstanceWrite struct {
	Name        string
	Role        string
	DBType      string
	Mode        string
	Host        string
	Port        int
	User        string
	Password    *string // nil 表示不修改
	IsSSL       bool
	VerifySSL   bool
	IsEnable    bool
	DBName      string
	ShowRegex   string
	DeniedRegex string
	Charset     string
	ServiceName *string
	SID         *string
	TunnelID    *uint
	GroupIDs    []uint
	TagIDs      []uint
}

// InstanceOptions 是实例表单的可选项。
type InstanceOptions struct {
	Groups  []GroupOption  `json:"groups"`
	Tags    []TagOption    `json:"tags"`
	Tunnels []TunnelOption `json:"tunnels"`
}

type GroupOption struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

type TagOption struct {
	ID     uint   `json:"id"`
	Code   string `json:"code"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

type TunnelOption struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

// Ref 是对实例的一类引用。
type Ref struct {
	Label    string `json:"label"`
	Count    int64  `json:"count"`
	Capped   bool   `json:"capped"`   // 统计到上限就停了，实际更多
	Blocking bool   `json:"blocking"` // 阻止删除
	Hint     string `json:"hint"`     // 阻止删除时给用户的处理建议
}

// DatabaseRecord 是平台里登记的库信息（dbt_instance_database）。
type DatabaseRecord struct {
	DBName       string
	Owner        string
	OwnerDisplay string
	Remark       string
}

// AccountRecord 是平台里登记的账号信息（dbt_instance_account；user 与 password 在库里是 mirage 密文）。
type AccountRecord struct {
	User     string
	Host     string
	Remark   string
	Password *string
	// KeepRemark 为 true 时更新已有登记不改备注（改密流程只更新密码）。
	KeepRemark bool
}

// ParamTemplate -> dbt_param_template（实例参数模板）。
type ParamTemplate struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	DBType       string    `gorm:"column:db_type" json:"db_type"`
	VariableName string    `gorm:"column:variable_name" json:"variable_name"`
	DefaultValue string    `gorm:"column:default_value" json:"default_value"`
	Editable     bool      `gorm:"column:editable" json:"editable"`
	ValidValues  string    `gorm:"column:valid_values" json:"valid_values"`
	Description  string    `gorm:"column:description" json:"description"`
	CreateTime   time.Time `gorm:"column:create_time" json:"create_time"`
	SysTime      time.Time `gorm:"column:sys_time" json:"sys_time"`
}

func (ParamTemplate) TableName() string { return "dbt_param_template" }

// ParamHistory -> dbt_param_history（参数修改历史）。
type ParamHistory struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	InstanceID   uint      `gorm:"column:instance_id" json:"instance_id"`
	VariableName string    `gorm:"column:variable_name" json:"variable_name"`
	OldVar       string    `gorm:"column:old_var" json:"old_var"`
	NewVar       string    `gorm:"column:new_var" json:"new_var"`
	SetSQL       string    `gorm:"column:set_sql" json:"set_sql"`
	UserName     string    `gorm:"column:user_name" json:"user_name"`
	UserDisplay  string    `gorm:"column:user_display" json:"user_display"`
	CreateTime   time.Time `gorm:"column:create_time" json:"create_time"`
}

func (ParamHistory) TableName() string { return "dbt_param_history" }

// ParamHistoryRow 是带实例名的修改历史。
type ParamHistoryRow struct {
	ParamHistory
	InstanceName string `json:"instance_name"`
	// CreateTimeText 是格式化后的修改时间；与内嵌的 create_time 同名，JSON 里取这个（外层字段优先）。
	CreateTimeText string `json:"create_time" gorm:"-"`
}
