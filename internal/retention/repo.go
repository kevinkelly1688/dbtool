// @author: kevinkelly
// @license: Apache Licence
// @file: repo.go
// @time: 2026/10/28

package retention

import (
	"context"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/model"
)

// TableState 是目标表在元数据库里的状态。
type TableState struct {
	Exists  bool
	Missing []string // 缺少的列
}

// OK 表、所需列都在。
func (s TableState) OK() bool { return s.Exists && len(s.Missing) == 0 }

// ExportFile 是一张已结束、已过保留期的导出工单及其文件。
type ExportFile struct {
	WorkflowID uint
	FileName   string
	RefTime    time.Time // 结束时间（缺省取创建时间）
}

// ExportPurge 是“导出文件已清理”的标记（写入 dbt_export_purged）。
type ExportPurge struct {
	WorkflowID uint
	FileName   string
	SizeBytes  int64
	Existed    bool // 删除时文件是否还在（false = 早已被人工清理）
	PurgedAt   time.Time
}

// BackupStore 是一个设置了保留天数的备份存储（凭据已解密，只在内存中使用，不写日志）。
type BackupStore struct {
	ID            uint
	Name          string
	RetentionDays int
	Config        engine.StoreConfig
}

// BackupRun 是一条带文件备份清单、待判断是否过期的归档运行。
type BackupRun struct {
	ID       uint
	StoreID  uint // 来自运行记录 backups 里的文件备份项；0 = 无法确定
	RefTime  time.Time
	Keys     []string // manifest 记录的对象 Key（含清单自身）
	Bytes    int64
	TaskName string
}

// RoundRecord 是一轮清理的汇总（写入 dbt_retention_run，页面 / 接口查看用）。
type RoundRecord struct {
	ID         uint      `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Trigger    string    `json:"trigger"`
	Status     string    `json:"status"` // ok / partial / failed / skipped
	Deleted    int64     `json:"deleted"`
	Summary    string    `json:"summary"`
}

// Repo 是清理要用的全部元数据库访问。生产实现见 store.go，测试用内存假实现。
type Repo interface {
	// ConfigValue 读 dbt_setting（值已解密）。
	ConfigValue(item string) (string, bool)
	// TableState 检查表与列是否存在；表 / 列不存在时调用方跳过并记日志。
	TableState(table string, cols ...string) (TableState, error)

	// CountOlder 统计 t 中时间早于 cutoff 且满足固定条件的行数（预览用，不删除）。
	CountOlder(ctx context.Context, t Target, cutoff time.Time) (int64, error)
	// DeleteBatch 取至多 limit 个早于 cutoff 的 id，连同 t.Children 里的子表行一起删除（单事务）。
	// 返回取到的 id 数（selected）与父表实际删除行数；selected < limit 表示这个目标已经清完。
	DeleteBatch(ctx context.Context, t Target, cutoff time.Time, limit int) (selected int, deleted int64, err error)

	// TryLock 取跨进程互斥锁（GET_LOCK），拿不到 ok=false；release 必须调用。
	TryLock(ctx context.Context, name string) (release func(), ok bool, err error)

	// 导出文件
	ExportMarkerReady() (bool, error)
	ExpiredExports(ctx context.Context, cutoff time.Time, afterID uint, limit int) ([]ExportFile, error)
	MarkExportPurged(ctx context.Context, p ExportPurge) error

	// 归档文件备份
	BackupStores(ctx context.Context) ([]BackupStore, error)
	BackupRuns(ctx context.Context, before time.Time, afterID uint, limit int) ([]BackupRun, error)
	MarkBackupPurged(ctx context.Context, runID uint, note string) error

	// 审计与汇总
	InsertAudit(e *model.AuditEntry) error
	SaveRound(r *RoundRecord) error
	ListRounds(limit int) ([]RoundRecord, error)
}
