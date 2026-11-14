// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

package sqlflow

import (
	"context"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine"
	"dbtool/internal/goinception"
	"dbtool/internal/model"
	"dbtool/internal/review"
)

// 按钮权限码（登记表 internal/btnperm）。
const (
	permSubmit        = "sqlworkflow.submit"        // 提交工单
	permBatchSubmit   = "sqlworkflow.submit_batch"  // 提交批量上线工单
	permExportSubmit  = "export.submit"             // 提交数据导出工单
	permExportBatch   = "export.submit_batch"       // 提交批量数据导出工单
	permDownload      = "export.download"           // 下载导出文件
	permMenuExport    = "export.view"               // 打开数据导出工单列表
	permMenuExportB   = "export.view_batch"         // 打开批量导出工单列表
	permViewGroup     = "sqlworkflow.view_group"    // 查看资源组内的全部工单
	permApprove       = "sqlworkflow.approve"       // 审批通过
	permReject        = "sqlworkflow.reject"        // 驳回 / 终止他人的工单
	permExecute       = "sqlworkflow.execute_own"   // 执行自己提交的工单
	permExecuteForRG  = "sqlworkflow.execute_group" // 执行所在资源组内全部工单
	permMenu          = "sqlworkflow.view_own"      // 打开工单列表
	permAuditUser     = "audit.view"                // 审计员：可看全部工单
	permQueryAllInsts = "query.admin_run"           // 查看 / 查询全部实例
)

// 执行方式。
const (
	ExecGoInception = "goinception"
	ExecNative      = "native"
)

// 提交者选择的“执行方式”（仅人工工单与自动工单的区别）。
const (
	ModeAuto   = "auto"   // 交给系统执行（立即进入队列）
	ModeManual = "manual" // 线下已手工执行，仅确认结束
)

// 工单状态显示名。
var statusLabels = map[string]string{
	model.WfFinish:          "已正常结束",
	model.WfAbort:           "人工终止流程",
	model.WfManReviewing:    "等待审批",
	model.WfReviewPass:      "审核通过",
	model.WfTimingTask:      "定时执行",
	model.WfQueuing:         "排队中",
	model.WfExecuting:       "执行中",
	model.WfAutoReviewWrong: "自动审批不通过",
	model.WfException:       "执行有异常",
}

// StatusLabel 返回工单状态的显示名。
func StatusLabel(s string) string {
	if l, ok := statusLabels[s]; ok {
		return l
	}
	return s
}

// StatusOptions 是列表筛选用的状态选项，按流程顺序。
func StatusOptions() []Option {
	order := []string{model.WfManReviewing, model.WfReviewPass, model.WfTimingTask, model.WfQueuing, model.WfExecuting,
		model.WfFinish, model.WfException, model.WfAbort, model.WfAutoReviewWrong}
	out := make([]Option, 0, len(order))
	for _, k := range order {
		out = append(out, Option{Value: k, Label: statusLabels[k]})
	}
	return out
}

type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Inception 是 goInception 客户端的抽象（真实实现见 internal/goinception，测试里用假实现）。
type Inception interface {
	Check(ctx context.Context, t goinception.Target, db, stmt string, o goinception.CheckOpts) ([]review.Row, error)
	Execute(ctx context.Context, t goinception.Target, db, stmt string, o goinception.ExecOpts) ([]review.Row, error)
	Osc(ctx context.Context, command, sqlsha1 string) ([]map[string]any, error)
}

// BackupReader 从 goInception 备份库读取回滚语句。
type BackupReader interface {
	Rollbacks(ctx context.Context, rows []review.Row) ([]goinception.Rollback, error)
}

// Opener 按类型创建引擎，默认 engine.New。
type Opener func(dbType string, c engine.Conn) (engine.Engine, error)

// WorkflowFilter 是工单列表的筛选条件。
type WorkflowFilter struct {
	Scope       Scope
	GroupIDs    []uint // Scope == ScopeGroups 时有效
	Username    string // Scope == ScopeOwn 时有效
	Status      string
	InstanceID  uint
	GroupID     uint
	SyntaxTypes []int
	Batch       string // "1" 只看批量工单，"0" 只看单实例工单
	Kind        string // "export" 只看数据导出工单，"change" 只看上线（变更）工单
	Search      string
	StartDate   *time.Time // 创建时间 >=
	EndDate     *time.Time // 创建时间 <
	Offset      int
	Limit       int

	Engineer string   // 只看这个提交人（登录名）的工单，与 Scope 叠加（“我提交的”页签）
	IDs      []uint   // 非空时只看这些工单（“待我审批 / 待我执行”页签先算出 ID 再取列表）
	Statuses []string // 状态多选，与 Status 叠加
	OrderBy  string   // 排序键：id / create_time / status / name / instance / db / engineer / group；空表示按 id 倒序
	Desc     bool     // OrderBy 非空时的方向
}

// Scope 是列表的可见范围。
type Scope int

const (
	ScopeOwn    Scope = iota // 只看自己提交的
	ScopeGroups              // 所在资源组内的
	ScopeAll                 // 全部
)

// Repo 是 SQL 工单服务需要的数据访问。InTx 内部的所有写操作在同一个事务里提交或回滚。
type Repo interface {
	audit.Repo
	InTx(fn func(Repo) error) error

	ListInstances() ([]model.Instance, error)
	GetInstance(id uint) (*model.Instance, error) // 带标签
	EngineConn(inst *model.Instance) engine.Conn
	UserInstanceIDs(userID uint) (map[uint]bool, error)
	UserResourceGroups(userID uint, all bool) ([]model.ResourceGroup, error)
	ResourceGroupByName(name string) (*model.ResourceGroup, error)
	ResourceGroupByID(id uint) (*model.ResourceGroup, error)
	// GroupInstances 返回属于某个资源组的实例（带标签）。
	GroupInstances(groupID uint) ([]model.Instance, error)
	// LockUser 在事务内对提交人加行锁，使同一个人的并发提交串行化。
	LockUser(username string) error

	// InsertWorkflow 写入工单和内容（content.WorkflowID 由实现回填）。
	InsertWorkflow(w *model.SqlWorkflow, c *model.SqlWorkflowContent) error
	// GetWorkflow 取工单（带 Instance 及其标签）；不存在返回 nil, nil。forUpdate 对工单行加锁。
	GetWorkflow(id uint, forUpdate bool) (*model.SqlWorkflow, error)
	SaveWorkflow(w *model.SqlWorkflow) error
	GetContent(workflowID uint) (*model.SqlWorkflowContent, error) // 不存在返回 nil, nil
	SaveContent(c *model.SqlWorkflowContent) error
	ListWorkflows(f WorkflowFilter) ([]WorkflowRow, int64, error)
	// ExecutingDDL 返回在指定库上正在执行的 DDL 工单（排除 excludeID）。
	ExecutingDDL(dbName string, excludeID uint) ([]model.SqlWorkflow, error)

	AuditByWorkflow(workflowType int, workflowID uint, forUpdate bool) (*model.WorkflowAudit, error)
	AuditLogs(auditID uint) ([]model.WorkflowLog, error)

	// InsertAuditEntry 写操作审计日志（如离线下载）。
	InsertAuditEntry(e *model.AuditEntry) error

	// 定时执行计划（dbt_sql_schedule）。
	UpsertSchedule(s *model.SqlSchedule) error
	GetSchedule(workflowID uint) (*model.SqlSchedule, error)
	DeleteSchedule(workflowID uint) error
	// ClaimSchedule 删除计划并报告是否真的删到了一行；多个进程同时调度时只有一个能认领成功。
	ClaimSchedule(workflowID uint) (bool, error)
	DueSchedules(now time.Time, limit int) ([]model.SqlSchedule, error)
	ScheduleTableReady() bool

	// 数据导出的扩展（dbt_sql_export_ext，deploy/sql/290_export_unmasked.sql）：超级用户选择“导出原值（不脱敏）”的记录。
	ExportExtReady() bool
	UpsertExportExt(e *model.SqlExportExt) error
	GetExportExt(workflowID uint) (*model.SqlExportExt, error) // 表没建或不存在返回 nil, nil
	// UserIsSuperuser 查登录名对应的启用用户当前是不是超级用户（导出原值在执行时要复核提交人仍是超级用户）；用户不存在或已停用返回 false。
	UserIsSuperuser(username string) (bool, error)
}

// ExportUnmaskedLoader 是仓库可选实现：一次查出一批工单里选择了“导出原值”的（工单号 -> true），列表加标签用。表没建时返回空集合。
type ExportUnmaskedLoader interface {
	ExportUnmaskedIDs(ids []uint) (map[uint]bool, error)
}

// ExportMergeGate 是仓库可选实现：dbt_sql_export_ext 是否已有 merge_files 列（批量导出“合并成一个文件”要用）。
// 没有实现、或返回 false 时，提交页不出现这个选项，服务端也拒绝带这个选项的提交。
type ExportMergeGate interface {
	ExportMergeReady() bool
}

// StatusCounter 是仓库可选实现的优化：一次 GROUP BY 取各状态的工单数。没有实现时服务逐状态查询。
type StatusCounter interface {
	StatusCounts(f WorkflowFilter) (map[string]int64, error)
}

// DownloadCounter 是仓库可选实现：导出工单文件被成功下载的次数（来自 dbt_audit_log 的“离线下载”记录）。
type DownloadCounter interface {
	DownloadCount(workflowID uint) (int, error)
}

// InstanceEnvLoader 是仓库可选实现：一次查出一批实例的环境（instance_id -> prod / test，未设置的不在结果里）。
// 旁路表 dbt_instance_ext 不存在时实现方返回空集合。没有实现时详情里的环境为空。
type InstanceEnvLoader interface {
	InstanceEnvs(ids []uint) (map[uint]string, error)
}

// WorkflowRow 是列表里的一行（带实例名）。
type WorkflowRow struct {
	model.SqlWorkflow
	InstanceName string
}

func cfgStr(r Repo, item string) string {
	v, _ := r.ConfigValue(item)
	return strings.TrimSpace(v)
}

func cfgBool(r Repo, item string) bool {
	v, ok := r.ConfigValue(item)
	return ok && strings.EqualFold(strings.TrimSpace(v), "true")
}
