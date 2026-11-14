// @author: kevinkelly
// @license: Apache Licence
// @file: workflow.go
// @time: 2026/10/28

package model

import "time"

// AuthGroup -> dbt_auth_group（权限组；审批流里的“审批权限组”）
type AuthGroup struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gorm:"column:name"`
}

func (AuthGroup) TableName() string { return "dbt_auth_group" }

// QueryPrivilegesApply -> dbt_query_privileges_apply（查询权限申请）
// PrivType: 1 库权限（DBList 为逗号分隔的库名），2 表权限（DBList 为单个库名，TableList 为逗号分隔的表名）。
// Status: 0 待审核 1 审核通过 2 审核不通过 3 审核取消。
type QueryPrivilegesApply struct {
	ApplyID         uint      `gorm:"column:apply_id;primaryKey"`
	GroupID         uint      `gorm:"column:group_id"`
	GroupName       string    `gorm:"column:group_name"`
	Title           string    `gorm:"column:title"`
	UserName        string    `gorm:"column:user_name"`
	UserDisplay     string    `gorm:"column:user_display"`
	InstanceID      uint      `gorm:"column:instance_id"`
	DBList          string    `gorm:"column:db_list"`
	TableList       string    `gorm:"column:table_list"`
	ValidDate       time.Time `gorm:"column:valid_date"`
	LimitNum        int       `gorm:"column:limit_num"`
	PrivType        int       `gorm:"column:priv_type"`
	Status          int       `gorm:"column:status"`
	AuditAuthGroups string    `gorm:"column:audit_auth_groups"`
	CreateTime      time.Time `gorm:"column:create_time;autoCreateTime"`
	SysTime         time.Time `gorm:"column:sys_time;autoUpdateTime"`

	Instance *Instance `gorm:"foreignKey:InstanceID" json:"-"`
}

func (QueryPrivilegesApply) TableName() string { return "dbt_query_privileges_apply" }

// WorkflowAudit -> dbt_workflow_audit（工作流审批主表；(workflow_id, workflow_type) 唯一）
type WorkflowAudit struct {
	AuditID           uint      `gorm:"column:audit_id;primaryKey"`
	GroupID           uint      `gorm:"column:group_id"`
	GroupName         string    `gorm:"column:group_name"`
	WorkflowID        uint      `gorm:"column:workflow_id"`
	WorkflowType      int       `gorm:"column:workflow_type"`
	WorkflowTitle     string    `gorm:"column:workflow_title"`
	WorkflowRemark    string    `gorm:"column:workflow_remark"`
	AuditAuthGroups   string    `gorm:"column:audit_auth_groups"` // 逗号分隔的权限组 ID；空串表示无需审批
	CurrentAudit      string    `gorm:"column:current_audit"`     // 当前审批权限组 ID，"-1" 表示没有
	NextAudit         string    `gorm:"column:next_audit"`
	CurrentStatus     int       `gorm:"column:current_status"`
	CreateUser        string    `gorm:"column:create_user"`
	CreateUserDisplay string    `gorm:"column:create_user_display"`
	CreateTime        time.Time `gorm:"column:create_time;autoCreateTime"`
	SysTime           time.Time `gorm:"column:sys_time;autoUpdateTime"`

	// IsExport 不是 dbt_workflow_audit 的列：由 store.AuditByWorkflow 在读取 SQL 上线类审批记录时用子查询带出
	// （对应 dbt_sql_workflow.is_offline_export），审批引擎据此对数据导出工单应用更严格的禁止自审规则。只读，不会写回。
	IsExport bool `gorm:"column:is_export_flag;->" json:"-"`
}

func (WorkflowAudit) TableName() string { return "dbt_workflow_audit" }

// WorkflowAuditDetail -> dbt_workflow_audit_detail（每一次审批动作）
type WorkflowAuditDetail struct {
	AuditDetailID uint      `gorm:"column:audit_detail_id;primaryKey"`
	AuditID       uint      `gorm:"column:audit_id"`
	AuditUser     string    `gorm:"column:audit_user"`
	AuditTime     time.Time `gorm:"column:audit_time"`
	AuditStatus   int       `gorm:"column:audit_status"`
	Remark        string    `gorm:"column:remark"`
	SysTime       time.Time `gorm:"column:sys_time;autoUpdateTime"`
}

func (WorkflowAuditDetail) TableName() string { return "dbt_workflow_audit_detail" }

// WorkflowAuditSetting -> dbt_workflow_audit_setting（每个资源组、每种工单类型的审批流）
type WorkflowAuditSetting struct {
	AuditSettingID  uint      `gorm:"column:audit_setting_id;primaryKey"`
	GroupID         uint      `gorm:"column:group_id"`
	GroupName       string    `gorm:"column:group_name"`
	WorkflowType    int       `gorm:"column:workflow_type"`
	AuditAuthGroups string    `gorm:"column:audit_auth_groups"`
	CreateTime      time.Time `gorm:"column:create_time;autoCreateTime"`
	SysTime         time.Time `gorm:"column:sys_time;autoUpdateTime"`
}

func (WorkflowAuditSetting) TableName() string { return "dbt_workflow_audit_setting" }

// WorkflowLog -> dbt_workflow_log（工单日志）
type WorkflowLog struct {
	ID                uint      `gorm:"primaryKey"`
	AuditID           uint      `gorm:"column:audit_id"`
	OperationType     int       `gorm:"column:operation_type"`
	OperationTypeDesc string    `gorm:"column:operation_type_desc"`
	OperationInfo     string    `gorm:"column:operation_info"`
	Operator          string    `gorm:"column:operator"`
	OperatorDisplay   string    `gorm:"column:operator_display"`
	OperationTime     time.Time `gorm:"column:operation_time;autoCreateTime"`
}

func (WorkflowLog) TableName() string { return "dbt_workflow_log" }
