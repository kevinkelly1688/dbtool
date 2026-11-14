// @author: kevinkelly
// @license: Apache Licence
// @file: model.go
// @time: 2026/10/28

package archive

import (
	"encoding/json"
	"fmt"
	"time"

	"dbtool/internal/archive/engine"
)

const timeLayout = "2006-01-02 15:04:05"

// 任务状态。
const (
	StDraft    = "draft"
	StPending  = "pending"
	StRejected = "rejected"
	StEnabled  = "enabled"
	StPaused   = "paused"
	StDisabled = "disabled"
)

// 暂停类型。
const (
	PauseManual    = "manual"
	PauseAuto      = "auto"
	PauseStructure = "structure"
)

// 触发方式。
const (
	TrigSchedule = "schedule"
	TrigManual   = "manual"
	TrigRestore  = "restore"
	TrigDryrun   = "dryrun"
)

// 运行结果。引擎的 Result* 常量原样使用，另加服务层的几种。
const (
	ResRunning     = "running"
	ResPaused      = "paused"
	ResInterrupted = "interrupted" // 进程崩溃 / 租约过期
	ResSkipped     = "skipped"     // 到点但被互斥 / 工单 / 并发等拦下
)

// 备份存储类型。
const (
	StoreLocal = "local"
	StoreNFS   = "nfs"
	StoreS3    = "s3"
	StoreOSS   = "oss"
)

const (
	checkValidDays = 7
	bigPercent     = 30.0
	maxHistory     = 100
	eventKeep      = 500 // 每个运行保留的事件条数
	ringCap        = 500 // 进程内事件环形缓冲
)

// ---- 错误 ----

// Error 是对外可见的业务错误，Message 可直接展示给用户。
type Error struct {
	Status  int
	Code    string
	Message string
	Data    any
}

func (e *Error) Error() string { return e.Message }

func newErr(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

func badReq(format string, a ...any) *Error    { return newErr(400, "bad_request", format, a...) }
func forbidden(format string, a ...any) *Error { return newErr(403, "forbidden", format, a...) }
func notFound(format string, a ...any) *Error  { return newErr(404, "not_found", format, a...) }
func conflict(format string, a ...any) *Error  { return newErr(409, "conflict", format, a...) }
func withData(e *Error, d any) *Error          { e.Data = d; return e }
func notReady() *Error                         { return newErr(503, "not_ready", "请先执行 deploy/sql/180_archive.sql") }
func confirmNeeded(format string, a ...any) *Error {
	return newErr(422, "confirm_required", format, a...)
}

// ---- 表模型 ----

// Task -> dbt_archive_task。JSON 列（DestTable / DestFile / Params / Schedule / Checkpoint / History）用 cfg() / setCfg() 读写。
type Task struct {
	ID              uint       `gorm:"column:id;primaryKey"`
	Name            string     `gorm:"column:name"`
	ResourceGroupID uint       `gorm:"column:resource_group_id"`
	InstanceID      uint       `gorm:"column:instance_id"`
	DBName          string     `gorm:"column:db_name"`
	TblName         string     `gorm:"column:tbl_name"`
	TimeColumn      string     `gorm:"column:time_column"`
	TimeUnit        string     `gorm:"column:time_unit"` // 空 = datetime 列；epoch_s / epoch_ms = int / bigint 的 10 位秒 / 13 位毫秒时间戳
	RetentionDays   int        `gorm:"column:retention_days"`
	ExtraWhere      string     `gorm:"column:extra_where"`
	Mode            string     `gorm:"column:mode"`
	PurgeReason     string     `gorm:"column:purge_reason"`
	DestTable       string     `gorm:"column:dest_table"`
	DestFile        string     `gorm:"column:dest_file"`
	Params          string     `gorm:"column:params"`
	Schedule        string     `gorm:"column:schedule"`
	Status          string     `gorm:"column:status"`
	PauseKind       string     `gorm:"column:pause_kind"`
	PauseReason     string     `gorm:"column:pause_reason"`
	PauseAt         *time.Time `gorm:"column:pause_at"`
	AuditID         uint       `gorm:"column:audit_id"`
	RejectReason    string     `gorm:"column:reject_reason"`
	Checkpoint      string     `gorm:"column:checkpoint"`
	Fingerprint     string     `gorm:"column:fingerprint"`
	SpecHash        string     `gorm:"column:spec_hash"`
	CumRows         int64      `gorm:"column:cum_rows"`
	CumRuns         int        `gorm:"column:cum_runs"`
	LastRunID       uint       `gorm:"column:last_run_id"`
	NextRunAt       *time.Time `gorm:"column:next_run_at"`
	History         string     `gorm:"column:history"`
	Version         uint       `gorm:"column:version"`
	Creator         string     `gorm:"column:creator"`
	CreatorID       uint       `gorm:"column:creator_id"`
	CreatorDisplay  string     `gorm:"column:creator_display"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

func (Task) TableName() string { return "dbt_archive_task" }

// Schedule 是调度配置。
type Schedule struct {
	Kind     string `json:"kind"` // day | week | month | cron
	Time     string `json:"time"` // HH:MM（kind != cron）
	Weekdays []int  `json:"weekdays"`
	MonthDay int    `json:"month_day"`
	Cron     string `json:"cron"`
}

// taskCfg 是任务里 JSON 列的解码结果。
type taskCfg struct {
	DestTable *engine.TableDest
	DestFile  *engine.FileDest
	Params    engine.Params
	Schedule  Schedule
}

func unmarshalOr[T any](s string, def T) T {
	if s == "" {
		return def
	}
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return def
	}
	return v
}

func (t *Task) cfg() taskCfg {
	return taskCfg{
		DestTable: unmarshalOr[*engine.TableDest](t.DestTable, nil),
		DestFile:  unmarshalOr[*engine.FileDest](t.DestFile, nil),
		Params:    unmarshalOr(t.Params, engine.Params{}),
		Schedule:  unmarshalOr(t.Schedule, Schedule{}),
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func (t *Task) setCfg(c taskCfg) {
	t.DestTable, t.DestFile = "", ""
	if c.DestTable != nil {
		t.DestTable = mustJSON(c.DestTable)
	}
	if c.DestFile != nil {
		t.DestFile = mustJSON(c.DestFile)
	}
	t.Params = mustJSON(c.Params)
	t.Schedule = mustJSON(c.Schedule)
}

func (t *Task) checkpoint() []string { return unmarshalOr[[]string](t.Checkpoint, nil) }

// HistoryItem 是任务的一条变更历史。
type HistoryItem struct {
	At   string `json:"at"`
	User string `json:"user"`
	Text string `json:"text"`
}

func (t *Task) history() []HistoryItem { return unmarshalOr[[]HistoryItem](t.History, nil) }

func (t *Task) addHistory(at time.Time, user, text string) {
	h := append(t.history(), HistoryItem{At: at.Format(timeLayout), User: user, Text: text})
	if len(h) > maxHistory {
		h = h[len(h)-maxHistory:]
	}
	t.History = mustJSON(h)
}

// Run -> dbt_archive_run。
type Run struct {
	ID              uint       `gorm:"column:id;primaryKey"`
	TaskID          uint       `gorm:"column:task_id"`
	TaskName        string     `gorm:"column:task_name"`
	InstanceID      uint       `gorm:"column:instance_id"`
	InstanceName    string     `gorm:"column:instance_name"`
	DBName          string     `gorm:"column:db_name"`
	TblName         string     `gorm:"column:tbl_name"`
	ResourceGroupID uint       `gorm:"column:resource_group_id"`
	Trigger         string     `gorm:"column:trigger_kind"`
	Result          string     `gorm:"column:result"`
	ByUser          string     `gorm:"column:by_user"`
	StartedAt       time.Time  `gorm:"column:started_at"`
	FinishedAt      *time.Time `gorm:"column:finished_at"`
	DurationMs      int64      `gorm:"column:duration_ms"`
	Scanned         int64      `gorm:"column:scanned"`
	Backed          int64      `gorm:"column:backed"`
	Deleted         int64      `gorm:"column:deleted"`
	Batches         int        `gorm:"column:batches"`
	PeakThreads     int        `gorm:"column:peak_threads"`
	PeakLag         float64    `gorm:"column:peak_lag"`
	ThrottleCount   int        `gorm:"column:throttle_count"`
	PKCols          string     `gorm:"column:pk_cols"`
	MinPK           string     `gorm:"column:min_pk"`
	MaxPK           string     `gorm:"column:max_pk"`
	DestTable       string     `gorm:"column:dest_table"`
	DestInstanceID  uint       `gorm:"column:dest_instance_id"`
	RestoreOf       uint       `gorm:"column:restore_of"`
	CheckID         uint       `gorm:"column:check_id"`
	Err             string     `gorm:"column:err"`
	Note            string     `gorm:"column:note"`
	AutoPause       bool       `gorm:"column:auto_pause"`
	LeaseOwner      string     `gorm:"column:lease_owner"`
	LeaseUntil      *time.Time `gorm:"column:lease_until"`
	HeartbeatAt     *time.Time `gorm:"column:heartbeat_at"`
	Progress        string     `gorm:"column:progress"`
	Spec            string     `gorm:"column:spec"`
	Manifest        string     `gorm:"column:manifest"`
	Verify          string     `gorm:"column:verify"`
	Throttles       string     `gorm:"column:throttles"`
	Backups         string     `gorm:"column:backups"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
}

func (Run) TableName() string { return "dbt_archive_run" }

// Finished 表示运行已经结束。
func (r *Run) Finished() bool { return r.Result != ResRunning && r.Result != ResPaused }

// Event -> dbt_archive_event。
type Event struct {
	ID     uint      `gorm:"column:id;primaryKey"`
	RunID  uint      `gorm:"column:run_id"`
	Seq    uint      `gorm:"column:seq"`
	At     time.Time `gorm:"column:at"`
	Kind   string    `gorm:"column:kind"`
	Msg    string    `gorm:"column:msg"`
	Fields string    `gorm:"column:fields"`
}

func (Event) TableName() string { return "dbt_archive_event" }

// BatchStat -> dbt_archive_batch_stat。
type BatchStat struct {
	ID         uint    `gorm:"column:id;primaryKey"`
	RunID      uint    `gorm:"column:run_id"`
	TMs        int64   `gorm:"column:t_ms"`
	RowsPerSec float64 `gorm:"column:rows_per_sec"`
	BatchSize  int     `gorm:"column:batch_size"`
	Throttled  bool    `gorm:"column:throttled"`
}

func (BatchStat) TableName() string { return "dbt_archive_batch_stat" }

// StoreRow -> dbt_archive_store。AK / SK 存的是 mirage 密文，由 Repo 实现负责加解密（上层看到的永远是明文 / 空）。
type StoreRow struct {
	ID            uint       `gorm:"column:id;primaryKey"`
	Name          string     `gorm:"column:name"`
	Type          string     `gorm:"column:type"`
	Path          string     `gorm:"column:path"`
	Endpoint      string     `gorm:"column:endpoint"`
	Region        string     `gorm:"column:region"`
	Bucket        string     `gorm:"column:bucket"`
	Prefix        string     `gorm:"column:prefix"`
	AK            string     `gorm:"column:ak"`
	SK            string     `gorm:"column:sk"`
	PathStyle     bool       `gorm:"column:path_style"`
	RetentionDays int        `gorm:"column:retention_days"`
	Enabled       bool       `gorm:"column:enabled"`
	Status        string     `gorm:"column:status"`
	LastTestAt    *time.Time `gorm:"column:last_test_at"`
	LastTestOK    bool       `gorm:"column:last_test_ok"`
	LastTestMs    int        `gorm:"column:last_test_ms"`
	LastTestMsg   string     `gorm:"column:last_test_msg"`
	FreeBytes     *int64     `gorm:"column:free_bytes"`
	UpdatedBy     string     `gorm:"column:updated_by"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at"`

	SKSet bool `gorm:"-"` // 库里是否保存了 SK（列表读取时 SK 本身不解密、不外传）
}

func (StoreRow) TableName() string { return "dbt_archive_store" }

// PolicyRow -> dbt_archive_policy（单行，id=1）。
type PolicyRow struct {
	ID        uint      `gorm:"column:id;primaryKey"`
	Content   string    `gorm:"column:content"`
	UpdatedBy string    `gorm:"column:updated_by"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (PolicyRow) TableName() string { return "dbt_archive_policy" }

// CheckRow -> dbt_archive_check。
type CheckRow struct {
	ID          uint      `gorm:"column:id;primaryKey"`
	Token       string    `gorm:"column:token"`
	TaskID      uint      `gorm:"column:task_id"`
	SpecHash    string    `gorm:"column:spec_hash"`
	CreatedBy   string    `gorm:"column:created_by"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	ExpiresAt   time.Time `gorm:"column:expires_at"`
	Blocked     bool      `gorm:"column:blocked"`
	WarnCount   int       `gorm:"column:warn_count"`
	Big         bool      `gorm:"column:big"`
	Fingerprint string    `gorm:"column:fingerprint"`
	Report      string    `gorm:"column:report"`
}

func (CheckRow) TableName() string { return "dbt_archive_check" }
