// @author: kevinkelly
// @license: Apache Licence
// @file: repo.go
// @time: 2026/10/28

package archive

import (
	"context"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// TaskFilter 是任务列表的筛选条件。Limit 为 0 表示不分页。
type TaskFilter struct {
	AllGroups        bool   // true：不按资源组限制
	ResourceGroupIDs []uint // AllGroups=false 时只看这些资源组
	Statuses         []string
	Mode             string
	InstanceID       uint
	Q                string // 任务名 / 库表 / 实例名
	IDs              []uint // 非空：只看这些任务
	Offset, Limit    int
}

// RunFilter 是运行记录的筛选条件。
type RunFilter struct {
	AllGroups        bool
	ResourceGroupIDs []uint
	TaskID           uint
	Trigger          string
	Result           string
	Q                string
	Offset, Limit    int
}

// OpenWorkflow 是源表上未结束的 SQL 上线工单（只读查 dbt_sql_workflow）。
type OpenWorkflow struct {
	ID     uint
	Name   string
	Status string
}

// Repo 是服务层需要的元数据库访问：dbt_archive_* 表、审批流存储（audit.Repo）和少量只读的基础表。
// store.go 是 GORM 实现，测试里用内存假实现。InTx 的回调拿到的 Repo 在同一事务内。
type Repo interface {
	audit.Repo
	InTx(fn func(Repo) error) error

	// Ready 表示 dbt_archive_* 表已经创建（180_archive.sql 已执行）。
	Ready() bool

	// ---- 资源组、工单、审批（基础表，只读或经 audit 引擎写）----
	UserResourceGroups(userID uint, all bool) ([]model.ResourceGroup, error)
	ResourceGroupByID(id uint) (*model.ResourceGroup, error)
	InstanceResourceGroupIDs(instanceID uint) ([]uint, error)
	AuditByWorkflow(workflowType int, workflowID uint, forUpdate bool) (*model.WorkflowAudit, error)
	AuditLogs(auditID uint) ([]model.WorkflowLog, error)
	AuditDetails(auditID uint) ([]model.WorkflowAuditDetail, error)
	// OpenWorkflows 返回该实例该库上、SQL 里涉及这张表且未结束（审核通过 / 定时执行 / 排队中 / 执行中）的 SQL 上线工单。
	OpenWorkflows(instanceID uint, db, table string) ([]OpenWorkflow, error)
	// WriteAudit 写一条系统审计（dbt_audit_log）。
	WriteAudit(e *model.AuditEntry) error

	// ---- 任务 ----
	InsertTask(t *Task) error
	// SaveTask 在 t.Version 与库里一致时保存并把 Version+1，不一致返回 ErrVersion。
	SaveTask(t *Task) error
	GetTask(id uint, forUpdate bool) (*Task, error) // 不存在返回 nil, nil
	ListTasks(f TaskFilter) ([]Task, int64, error)
	// ActiveTaskOnTable 返回该表上进行中的任务（待审批 / 已启用 / 已暂停），exceptID 非 0 时排除它。
	ActiveTaskOnTable(instanceID uint, db, table string, exceptID uint) (*Task, error)
	TaskNameTaken(name string, exceptID uint) (bool, error)
	DeleteTask(id uint) error
	// UpdateCheckpoint 只更新断点（不动版本号），运行中节流保存。
	UpdateCheckpoint(id uint, checkpoint string) error
	// ClaimNextRun 在 next_run_at 仍等于 old 时把它改成 new（调度认领：多进程里只有一个能成功）。
	ClaimNextRun(id uint, old, new *time.Time) (bool, error)

	// ---- 运行 ----
	InsertRun(r *Run) error
	SaveRun(r *Run) error
	GetRun(id uint) (*Run, error)
	ListRuns(f RunFilter) ([]Run, int64, error)
	RecentRuns(taskID uint, n int) ([]Run, error)
	// RunningRuns 返回 result=running/paused 的运行（instanceID 非 0 时只看该实例；leaseAfter 非零时只算租约未过期的）。
	RunningRuns(instanceID uint, leaseAfter time.Time) ([]Run, error)
	RunsRestoredBy(runID uint) ([]uint, error)
	// ExpireLeases 把租约早于 before 的 running/paused 运行标为 interrupted，返回被标记的运行。
	ExpireLeases(before, now time.Time) ([]Run, error)

	// ---- 事件 / 吞吐采样 ----
	InsertEvents(evs []Event) error
	ListEvents(runID uint, afterSeq uint, limit int) ([]Event, error)
	TrimEvents(runID uint, keep int) error
	InsertBatchStats(runID uint, rows []BatchStat) error
	ListBatchStats(runID uint) ([]BatchStat, error)

	// ---- 备份存储 ----
	ListStores() ([]StoreRow, error)
	GetStore(id uint) (*StoreRow, error) // AK / SK 为明文；不存在返回 nil, nil
	InsertStore(s *StoreRow) error       // 传入明文 AK / SK，实现负责加密
	SaveStore(s *StoreRow, keepSK bool) error
	DeleteStore(id uint) error
	StoreRefCount(id uint) (int, error)

	// ---- 策略 / 试运行结果 ----
	GetPolicy() (*PolicyRow, error) // 没有记录返回 nil, nil
	SavePolicy(p *PolicyRow) error
	InsertCheck(c *CheckRow) error
	GetCheckByToken(token string) (*CheckRow, error)
	PurgeChecks(before time.Time) error

	// TryLock 取跨进程的互斥锁（GET_LOCK），拿不到返回 ok=false；release 必须调用。
	TryLock(ctx context.Context, name string) (release func(), ok bool, err error)
}
