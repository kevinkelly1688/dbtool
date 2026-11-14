// @author: kevinkelly
// @license: Apache Licence
// @file: sqlworkflow.go
// @time: 2026/10/28

package model

import "time"

// SQL 工单状态（直接存在数据库里）。
const (
	WfFinish          = "workflow_finish"          // 执行结束
	WfAbort           = "workflow_abort"           // 人工终止流程
	WfManReviewing    = "workflow_manreviewing"    // 待审核
	WfReviewPass      = "workflow_review_pass"     // 审核通过
	WfTimingTask      = "workflow_timingtask"      // 定时执行
	WfQueuing         = "workflow_queuing"         // 排队中
	WfExecuting       = "workflow_executing"       // 执行中
	WfAutoReviewWrong = "workflow_autoreviewwrong" // 自动审核不通过
	WfException       = "workflow_exception"       // 执行有异常
)

// SqlWorkflow -> dbt_sql_workflow（SQL 上线工单）。
type SqlWorkflow struct {
	ID               uint       `gorm:"primaryKey" json:"id"`
	WorkflowName     string     `gorm:"column:workflow_name" json:"workflow_name"`
	DemandURL        string     `gorm:"column:demand_url" json:"demand_url"`
	GroupID          uint       `gorm:"column:group_id" json:"group_id"`
	GroupName        string     `gorm:"column:group_name" json:"group_name"`
	InstanceID       uint       `gorm:"column:instance_id" json:"instance_id"`
	DBName           string     `gorm:"column:db_name" json:"db_name"`
	SyntaxType       int        `gorm:"column:syntax_type" json:"syntax_type"` // 0 其他 1 DDL 2 DML 3 离线导出
	IsBackup         bool       `gorm:"column:is_backup" json:"is_backup"`
	Engineer         string     `gorm:"column:engineer" json:"engineer"`
	EngineerDisplay  string     `gorm:"column:engineer_display" json:"engineer_display"`
	Status           string     `gorm:"column:status" json:"status"`
	AuditAuthGroups  string     `gorm:"column:audit_auth_groups" json:"-"`
	RunDateStart     *time.Time `gorm:"column:run_date_start" json:"run_date_start"`
	RunDateEnd       *time.Time `gorm:"column:run_date_end" json:"run_date_end"`
	CreateTime       time.Time  `gorm:"column:create_time;autoCreateTime" json:"create_time"`
	FinishTime       *time.Time `gorm:"column:finish_time" json:"finish_time"`
	IsManual         int        `gorm:"column:is_manual" json:"is_manual"`
	IsOfflineExport  int        `gorm:"column:is_offline_export" json:"is_offline_export"`
	ExportFormat     *string    `gorm:"column:export_format" json:"-"`
	FileName         *string    `gorm:"column:file_name" json:"-"`
	BatchInstanceIDs *string    `gorm:"column:batch_instance_ids" json:"-"`

	Instance *Instance `gorm:"foreignKey:InstanceID" json:"-"`
}

func (SqlWorkflow) TableName() string { return "dbt_sql_workflow" }

// IsBatch 是否多实例批量工单（本系统暂不支持）。
func (w *SqlWorkflow) IsBatch() bool { return w.BatchInstanceIDs != nil && *w.BatchInstanceIDs != "" }

// SqlWorkflowContent -> dbt_sql_workflow_content（工单的 SQL、检查结果、执行结果）。
// ReviewContent / ExecuteResult 是 JSON 数组。
type SqlWorkflowContent struct {
	ID            uint   `gorm:"primaryKey"`
	WorkflowID    uint   `gorm:"column:workflow_id"`
	SQLContent    string `gorm:"column:sql_content"`
	ReviewContent string `gorm:"column:review_content"`
	ExecuteResult string `gorm:"column:execute_result"`
}

func (SqlWorkflowContent) TableName() string { return "dbt_sql_workflow_content" }

// SqlSchedule -> dbt_sql_schedule（dbtool 新增：定时执行计划，见 deploy/sql/002_sql_schedule.sql）。
type SqlSchedule struct {
	WorkflowID uint      `gorm:"column:workflow_id;primaryKey"`
	RunAt      time.Time `gorm:"column:run_at"`
	CreatedBy  string    `gorm:"column:created_by"`
	Executor   string    `gorm:"column:executor"` // goinception / native：设置定时时由执行人选定
	CreateTime time.Time `gorm:"column:create_time;autoCreateTime"`
}

func (SqlSchedule) TableName() string { return "dbt_sql_schedule" }

// SqlExportExt -> dbt_sql_export_ext（dbtool 新增：数据导出工单的扩展，见 deploy/sql/290_export_unmasked.sql）。
// 一行 = 这张导出工单提交时选了扩展选项：Unmasked 是提交人（超级用户）选择了“导出原值（不脱敏）”并写明了理由；
// MergeFiles 是批量导出选择了“合并成一个文件”（各实例导出的数据依次写进同一个文件，不再每个实例一个文件）。
// 没有这一行的导出工单按默认处理：按系统的脱敏设置导出，批量导出每个实例一个文件。
type SqlExportExt struct {
	WorkflowID uint      `gorm:"column:workflow_id;primaryKey"`
	Unmasked   bool      `gorm:"column:unmasked"`
	Reason     string    `gorm:"column:reason"`
	SetBy      string    `gorm:"column:set_by"`
	MergeFiles bool      `gorm:"column:merge_files"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

func (SqlExportExt) TableName() string { return "dbt_sql_export_ext" }

// AuditEntry -> dbt_audit_log（操作审计日志：登录、离线下载等）。
type AuditEntry struct {
	ID          uint      `gorm:"primaryKey"`
	UserID      uint      `gorm:"column:user_id"`
	UserName    string    `gorm:"column:user_name"`
	UserDisplay string    `gorm:"column:user_display"`
	Action      string    `gorm:"column:action"`
	ExtraInfo   string    `gorm:"column:extra_info"`
	ActionTime  time.Time `gorm:"column:action_time;autoCreateTime"`
}

func (AuditEntry) TableName() string { return "dbt_audit_log" }
