// @author: kevinkelly
// @license: Apache Licence
// @file: model.go
// @time: 2026/10/28

// Package model 映射元数据库的数据表（GORM 模型）。
//
// 约束：Go 侧只做读写，不做 AutoMigrate。
package model

import "time"

// ResourceGroup -> dbt_resource_group
type ResourceGroup struct {
	GroupID       uint      `gorm:"column:group_id;primaryKey" json:"group_id"`
	GroupName     string    `gorm:"column:group_name" json:"group_name"`
	GroupParentID int64     `gorm:"column:group_parent_id" json:"group_parent_id"`
	GroupSort     int       `gorm:"column:group_sort" json:"group_sort"`
	GroupLevel    int       `gorm:"column:group_level" json:"group_level"`
	DingWebhook   string    `gorm:"column:ding_webhook" json:"-"`
	FeishuWebhook string    `gorm:"column:feishu_webhook" json:"-"`
	SlackWebhook  string    `gorm:"column:slack_webhook" json:"-"`
	IsDeleted     int       `gorm:"column:is_deleted" json:"is_deleted"`
	CreateTime    time.Time `gorm:"column:create_time" json:"create_time"`
	SysTime       time.Time `gorm:"column:sys_time" json:"sys_time"`
}

func (ResourceGroup) TableName() string { return "dbt_resource_group" }

// User -> dbt_sql_users
type User struct {
	ID                uint       `gorm:"primaryKey" json:"id"`
	Password          string     `json:"-"`
	LastLogin         *time.Time `json:"last_login"`
	IsSuperuser       bool       `json:"is_superuser"`
	Username          string     `json:"username"`
	FirstName         string     `json:"-"`
	LastName          string     `json:"-"`
	Email             string     `json:"email"`
	IsStaff           bool       `json:"is_staff"`
	IsActive          bool       `json:"is_active"`
	DateJoined        time.Time  `json:"date_joined"`
	Display           string     `json:"display"`
	DingUserID        string     `gorm:"column:ding_user_id" json:"ding_user_id"`
	SlackID           string     `gorm:"column:slack_id" json:"slack_id"`
	FeishuOpenID      string     `gorm:"column:feishu_open_id" json:"feishu_open_id"`
	FailedLoginCount  int        `gorm:"column:failed_login_count" json:"failed_login_count"`
	LastLoginFailedAt *time.Time `gorm:"column:last_login_failed_at" json:"last_login_failed_at"`

	ResourceGroups []ResourceGroup `gorm:"many2many:dbt_sql_users_resource_group;joinForeignKey:users_id;joinReferences:resourcegroup_id" json:"resource_groups,omitempty"`
}

func (User) TableName() string { return "dbt_sql_users" }

// InstanceTag -> dbt_sql_instance_tag
type InstanceTag struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	TagCode    string    `gorm:"column:tag_code" json:"tag_code"`
	TagName    string    `gorm:"column:tag_name" json:"tag_name"`
	Active     bool      `json:"active"`
	CreateTime time.Time `gorm:"column:create_time" json:"create_time"`
}

func (InstanceTag) TableName() string { return "dbt_sql_instance_tag" }

// Tunnel -> dbt_ssh_tunnel（user/password/pkey/pkey_password 为 mirage 加密字段）
type Tunnel struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	TunnelName   string    `gorm:"column:tunnel_name" json:"tunnel_name"`
	Host         string    `json:"host"`
	Port         int       `json:"port"`
	User         *string   `json:"-"`
	Password     *string   `json:"-"`
	Pkey         *string   `json:"-"`
	PkeyPath     *string   `gorm:"column:pkey_path" json:"-"`
	PkeyPassword *string   `gorm:"column:pkey_password" json:"-"`
	CreateTime   time.Time `gorm:"column:create_time" json:"create_time"`
	UpdateTime   time.Time `gorm:"column:update_time" json:"update_time"`
}

func (Tunnel) TableName() string { return "dbt_ssh_tunnel" }

// Instance -> dbt_sql_instance（user/password 为 mirage 加密字段，读出后需 Decrypt）
type Instance struct {
	ID                uint      `gorm:"primaryKey" json:"id"`
	InstanceName      string    `gorm:"column:instance_name" json:"instance_name"`
	Type              string    `json:"type"` // master / slave
	DBType            string    `gorm:"column:db_type" json:"db_type"`
	Mode              string    `json:"mode"`
	Host              string    `json:"host"`
	Port              int       `json:"port"`
	User              string    `json:"-"`
	Password          string    `json:"-"`
	IsSSL             bool      `gorm:"column:is_ssl" json:"is_ssl"`
	IsEnable          bool      `gorm:"column:is_enable" json:"is_enable"`
	VerifySSL         bool      `gorm:"column:verify_ssl" json:"verify_ssl"`
	DBName            string    `gorm:"column:db_name" json:"db_name"`
	ShowDBNameRegex   string    `gorm:"column:show_db_name_regex" json:"show_db_name_regex"`
	DeniedDBNameRegex string    `gorm:"column:denied_db_name_regex" json:"denied_db_name_regex"`
	Charset           string    `json:"charset"`
	ServiceName       *string   `gorm:"column:service_name" json:"service_name"`
	SID               *string   `gorm:"column:sid" json:"sid"`
	TunnelID          *uint     `gorm:"column:tunnel_id" json:"tunnel_id"`
	CreateTime        time.Time `gorm:"column:create_time" json:"create_time"`
	UpdateTime        time.Time `gorm:"column:update_time" json:"update_time"`

	Tunnel         *Tunnel         `gorm:"foreignKey:TunnelID" json:"-"`
	ResourceGroups []ResourceGroup `gorm:"many2many:dbt_sql_instance_resource_group;joinForeignKey:instance_id;joinReferences:resourcegroup_id" json:"resource_groups,omitempty"`
	Tags           []InstanceTag   `gorm:"many2many:dbt_sql_instance_instance_tag;joinForeignKey:instance_id;joinReferences:instancetag_id" json:"tags,omitempty"`
}

func (Instance) TableName() string { return "dbt_sql_instance" }

// ServiceNameValue / SIDValue 取 Oracle 连接用的服务名 / SID（没填为空串）。
func (i Instance) ServiceNameValue() string {
	if i.ServiceName == nil {
		return ""
	}
	return *i.ServiceName
}

func (i Instance) SIDValue() string {
	if i.SID == nil {
		return ""
	}
	return *i.SID
}

// QueryLog -> dbt_query_log（在线查询审计日志）
type QueryLog struct {
	ID           uint   `gorm:"primaryKey" json:"id"`
	InstanceName string `gorm:"column:instance_name" json:"instance_name"`
	DBName       string `gorm:"column:db_name" json:"db_name"`
	SQLLog       string `gorm:"column:sqllog" json:"sqllog"`
	EffectRow    int64  `gorm:"column:effect_row" json:"effect_row"`
	CostTime     string `gorm:"column:cost_time" json:"cost_time"`
	Username     string `gorm:"column:username" json:"username"`
	UserDisplay  string `gorm:"column:user_display" json:"user_display"`
	PrivCheck    bool   `gorm:"column:priv_check" json:"priv_check"`
	HitRule      bool   `gorm:"column:hit_rule" json:"hit_rule"`
	Masking      bool   `gorm:"column:masking" json:"masking"`
	Favorite     bool   `gorm:"column:favorite" json:"favorite"`
	Alias        string `gorm:"column:alias" json:"alias"`
	// create_time / sys_time 在库里没有默认值，
	// 写入方必须自己设置，否则会插入 0000-00-00 并被严格模式拒绝。
	CreateTime time.Time `gorm:"column:create_time;autoCreateTime" json:"create_time"`
	SysTime    time.Time `gorm:"column:sys_time;autoUpdateTime" json:"sys_time"`
}

func (QueryLog) TableName() string { return "dbt_query_log" }

// QueryLogUser 是按用户汇总的查询次数（执行历史里管理员的用户过滤下拉用）。
type QueryLogUser struct {
	Username    string
	UserDisplay string
	Count       int64
}

// QueryPrivilege -> dbt_query_privileges（用户的库/表查询权限）。PrivType: 1 库权限，2 表权限；
// 库权限的 DBName 可为 "*" 表示该实例全部库。
type QueryPrivilege struct {
	PrivilegeID uint      `gorm:"column:privilege_id;primaryKey" json:"privilege_id"`
	UserName    string    `gorm:"column:user_name" json:"user_name"`
	UserDisplay string    `gorm:"column:user_display" json:"user_display"`
	InstanceID  uint      `gorm:"column:instance_id" json:"instance_id"`
	DBName      string    `gorm:"column:db_name" json:"db_name"`
	TbName      string    `gorm:"column:table_name" json:"table_name"`
	ValidDate   time.Time `gorm:"column:valid_date" json:"valid_date"`
	LimitNum    int       `gorm:"column:limit_num" json:"limit_num"`
	PrivType    int       `gorm:"column:priv_type" json:"priv_type"`
	IsDeleted   int       `gorm:"column:is_deleted" json:"is_deleted"`
	// create_time / sys_time 在库里没有默认值：写入方必须自己设置。
	CreateTime time.Time `gorm:"column:create_time;autoCreateTime" json:"create_time"`
	SysTime    time.Time `gorm:"column:sys_time;autoUpdateTime" json:"sys_time"`
}

func (QueryPrivilege) TableName() string { return "dbt_query_privileges" }

// DataMaskingColumn 是查询侧使用的一条脱敏字段配置（由 dbt_masking_policy 的行转成；schema 与 table 都为 "*" 表示任意库表的同名字段）。
type DataMaskingColumn struct {
	ColumnID      uint      `gorm:"column:column_id;primaryKey"`
	RuleType      int       `gorm:"column:rule_type"`
	Active        bool      `gorm:"column:active"`
	InstanceID    uint      `gorm:"column:instance_id"`
	TableSchema   string    `gorm:"column:table_schema"`
	TableName_    string    `gorm:"column:table_name"`
	ColumnName    string    `gorm:"column:column_name"`
	ColumnComment string    `gorm:"column:column_comment"`
	CreateTime    time.Time `gorm:"column:create_time;autoCreateTime"`
	SysTime       time.Time `gorm:"column:sys_time;autoUpdateTime"`

	// 来自 dbt_masking_policy（Kind 非空）由仓库层并入。
	PolicyID   uint   `gorm:"-"`
	Kind       string `gorm:"-"`
	KeepPrefix int    `gorm:"-"`
	KeepSuffix int    `gorm:"-"`
	HideGroup  int    `gorm:"-"`
	RuleRegex  string `gorm:"-"`
}

// MaskingPolicy -> dbt_masking_policy（dbtool 新增：字段级脱敏策略，新页面的配置存在这里；旧表继续生效）。
type MaskingPolicy struct {
	ID         uint      `gorm:"primaryKey"`
	InstanceID uint      `gorm:"column:instance_id"`
	ScopeDB    string    `gorm:"column:scope_db"`
	ScopeTable string    `gorm:"column:scope_table"`
	FieldPath  string    `gorm:"column:field_path"`
	RuleKind   string    `gorm:"column:rule_kind"`
	KeepPrefix int       `gorm:"column:keep_prefix"`
	KeepSuffix int       `gorm:"column:keep_suffix"`
	HideGroup  int       `gorm:"column:hide_group"`
	RuleRegex  string    `gorm:"column:rule_regex"`
	Active     bool      `gorm:"column:active"`
	UpdatedBy  string    `gorm:"column:updated_by"`
	CreatedAt  time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt  time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (MaskingPolicy) TableName() string { return "dbt_masking_policy" }

// FirewallRule -> dbt_firewall_rule（dbtool 新增：SQL 防火墙规则）。
// scenes / instance_ids / group_ids 是逗号分隔文本，conditions 是 JSON 文本，由 internal/firewall 解析与校验。
type FirewallRule struct {
	ID           uint       `gorm:"primaryKey"`
	Name         string     `gorm:"column:name"`
	Description  string     `gorm:"column:description"`
	Priority     int        `gorm:"column:priority"`
	Enabled      bool       `gorm:"column:enabled"`
	Mode         string     `gorm:"column:mode"`
	ObserveSince *time.Time `gorm:"column:observe_since"`
	Scenes       string     `gorm:"column:scenes"`
	Env          string     `gorm:"column:env"`
	InstanceIDs  string     `gorm:"column:instance_ids"`
	GroupMode    string     `gorm:"column:group_mode"`
	GroupIDs     string     `gorm:"column:group_ids"`
	MatchMode    string     `gorm:"column:match_mode"`
	Conditions   string     `gorm:"column:conditions"`
	Action       string     `gorm:"column:action"`
	ActionArg    int        `gorm:"column:action_arg"`
	Message      string     `gorm:"column:message"`
	Notify       bool       `gorm:"column:notify"`
	UpdatedBy    string     `gorm:"column:updated_by"`
	CreatedAt    time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt    time.Time  `gorm:"column:updated_at;autoUpdateTime"`
}

func (FirewallRule) TableName() string { return "dbt_firewall_rule" }

// FirewallException -> dbt_firewall_exception（豁免某条规则对某个主体的约束，带原因与到期时间）。
type FirewallException struct {
	ID           uint      `gorm:"primaryKey"`
	RuleID       uint      `gorm:"column:rule_id"`
	SubjectKind  string    `gorm:"column:subject_kind"`
	SubjectValue string    `gorm:"column:subject_value"`
	Reason       string    `gorm:"column:reason"`
	ExpiresAt    time.Time `gorm:"column:expires_at"`
	CreatedBy    string    `gorm:"column:created_by"`
	CreatedAt    time.Time `gorm:"column:created_at;autoCreateTime"`
}

func (FirewallException) TableName() string { return "dbt_firewall_exception" }

// FirewallHit -> dbt_firewall_hit（命中记录；同一规则 / 用户 / 指纹 / 结果在合并窗口内累加 repeat_cnt）。
type FirewallHit struct {
	ID           uint      `gorm:"primaryKey"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	LastAt       time.Time `gorm:"column:last_at"`
	RuleID       uint      `gorm:"column:rule_id"`
	RuleName     string    `gorm:"column:rule_name"`
	Scene        string    `gorm:"column:scene"`
	UserName     string    `gorm:"column:user_name"`
	InstanceID   uint      `gorm:"column:instance_id"`
	InstanceName string    `gorm:"column:instance_name"`
	DBName       string    `gorm:"column:db_name"`
	Effective    string    `gorm:"column:effective"`
	WouldAction  string    `gorm:"column:would_action"`
	Fingerprint  string    `gorm:"column:fingerprint"`
	SQLText      string    `gorm:"column:sql_text"`
	ReasonText   string    `gorm:"column:reason_text"`
	Matched      string    `gorm:"column:matched"`
	RepeatCnt    int       `gorm:"column:repeat_cnt"`
	QueryLogID   uint      `gorm:"column:query_log_id"`
}

func (FirewallHit) TableName() string { return "dbt_firewall_hit" }
