// @author: kevinkelly
// @license: Apache Licence
// @file: contract.go
// @time: 2026/10/28

// Package engine 是“数据归档”的执行引擎：条件与 SQL 生成、校验、试运行、分批归档（读 → 写备份 → 校验 → 删源）、
// 负载自检与限流、文件备份存储、恢复。
//
// 本文件是引擎与上层服务（internal/archive）之间的契约：类型和函数签名由此固定，引擎实现者只能在不改变签名的前提下
// 补充实现（可以新增导出，不得改动或删除这里已有的导出；确需调整必须在最终报告里说明）。
// 引擎不读写元数据库、不知道用户和权限，所有输入都通过 Spec / Policy / Env 传入。
package engine

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sync"
	"time"

	eng "dbtool/internal/engine"
)

// ErrNotImplemented 是契约桩函数的返回值，实现完成后不应再出现（保留导出以免破坏上层引用）。
var ErrNotImplemented = errors.New("archive/engine: not implemented")

// ---- 任务规格 ----

type Mode string

const (
	ModeMove  Mode = "move"  // 归档并删除：备份校验通过后删源
	ModeCopy  Mode = "copy"  // 仅备份：不删源
	ModePurge Mode = "purge" // 仅清理：不备份直接删（受 Policy.AllowPurgeNoBackup 约束，且 Dest 全空）
)

// Window 是执行窗口，"HH:MM"（本地时区），可跨零点；Start == End 视为不限制。
type Window struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// Params 是稳定性参数。零值表示“用 Policy.Defaults”。
type Params struct {
	BatchSize              int    `json:"batch_size"`               // 每批行数，100–10000，默认 1000
	SleepMs                int    `json:"sleep_ms"`                 // 批间休眠，0–5000，默认 200（move/purge 不得低于 50）
	MaxRows                int64  `json:"max_rows"`                 // 单次运行最多处理行数，默认 5,000,000
	MaxThreadsRunning      int    `json:"max_threads_running"`      // 源库 Threads_running 上限，默认 32
	MaxLagSeconds          int    `json:"max_lag_seconds"`          // 从库延迟上限（秒），默认 5；有从库时不允许为 0
	MinDiskFreePct         int    `json:"min_disk_free_pct"`        // 磁盘剩余下限 %，默认 15（取不到则该项“未检测”，不当作通过）
	TargetBatchMs          int    `json:"target_batch_ms"`          // 批耗时目标，超过则批量减半、休眠加倍，默认 500
	MaxWaitSeconds         int    `json:"max_wait_seconds"`         // 负载超限时最长等待，默认 600；超过以 load_abort 结束
	MaxConsecutiveFailures int    `json:"max_consecutive_failures"` // 连续失败批数上限，达到则 Result=failed 且 AutoPause=true，默认 3
	LockWaitSeconds        int    `json:"lock_wait_seconds"`        // 会话 lock_wait_timeout / innodb_lock_wait_timeout，默认 3
	Window                 Window `json:"window"`
}

// TableDest 是库表备份去向。Table 已是展开后的最终表名（模板展开见 ExpandTableName）。
type TableDest struct {
	InstanceID uint   `json:"instance_id"` // 可与源相同
	DB         string `json:"db"`
	Table      string `json:"table"`
	Template   string `json:"template"` // 任务里保存的模板，如 {table}_{yyyy}；Table 为空时由引擎按运行时间展开
}

// FileDest 是文件备份去向。
type FileDest struct {
	StoreID      uint   `json:"store_id"`
	Format       string `json:"format"` // csv | sql
	Gzip         bool   `json:"gzip"`
	Encrypt      bool   `json:"encrypt"`       // AES-256-GCM，密钥来自 Env.FileKey
	PathTemplate string `json:"path_template"` // 如 {inst}/{db}/{table}/{yyyy}/{mm}/{dd}/{run_id}，扩展名由引擎追加
	PartMB       int    `json:"part_mb"`       // 单个文件上限（MB），默认 256，超过则滚动新文件
}

// Spec 是一次运行（含试运行、恢复）所需的任务规格，由上层从任务记录转换而来。
type Spec struct {
	TaskID uint   `json:"task_id"`
	RunID  uint   `json:"run_id"`
	Name   string `json:"name"`

	InstanceID uint   `json:"instance_id"`
	DB         string `json:"db"`
	Table      string `json:"table"`

	TimeColumn    string `json:"time_column"`
	TimeUnit      string `json:"time_unit,omitempty"` // 空 = date / datetime / timestamp 列；epoch_s / epoch_ms = int / bigint 里的 10 位秒 / 13 位毫秒时间戳
	RetentionDays int    `json:"retention_days"`
	ExtraWhere    string `json:"extra_where"`

	Mode Mode       `json:"mode"`
	Dest *TableDest `json:"dest_table"` // 可同时有库表与文件；Mode=purge 时两者必须都为空
	File *FileDest  `json:"dest_file"`

	Params     Params   `json:"params"`
	Checkpoint []string `json:"checkpoint"` // 上次运行断点：最后处理的主键值（按主键列顺序，字符串形式）；空表示从头开始

	Fingerprint string `json:"fingerprint"` // 审批 / 试运行时的源表结构指纹（列名+类型+主键）；运行时不一致则 Result=structure_changed
}

// Policy 是全局安全策略（由上层从安全策略读取后传入）。
type Policy struct {
	MinRetentionDays   int      `json:"min_retention_days"`    // 默认 30
	AllowPurgeNoBackup bool     `json:"allow_purge_no_backup"` // 默认 false
	ProtectedPatterns  []string `json:"protected_patterns"`    // "db" 或 "db.table"，支持 * ? 通配；默认含 mysql / information_schema / performance_schema / sys
	MaxRowsPerRun      int64    `json:"max_rows_per_run"`      // 单次运行的硬上限，实际取 min(Spec.Params.MaxRows, 此值)
	Defaults           Params   `json:"defaults"`              // Spec.Params 中为零值的字段取这里
}

// ---- 校验与 SQL 预览（纯函数，不连库）----

type Issue struct {
	Field string `json:"field"`
	Level string `json:"level"` // error | warn
	Msg   string `json:"msg"`
}

// Validate 校验规格本身（范围、附加条件的禁止项、保护库表、模式与备份去向的组合、窗口≥30 分钟等），不连库。
// 有 error 级问题则不可保存 / 提交。
//
// 实现在 validate.go。
func Validate(spec Spec, pol Policy) []Issue { return validateSpec(spec, pol, true) }

// BuildWhere 生成最终 WHERE（不含 WHERE 关键字），形如 `created_at` < NOW() - INTERVAL 180 DAY AND (extra)；
// 整型时间戳列（TimeUnit = epoch_s / epoch_ms）形如 `ts` > 0 AND `ts` < UNIX_TIMESTAMP() - 15552000 AND (extra)（毫秒再乘 1000）。
// 实现在 where.go。
func BuildWhere(spec Spec) (string, error) { return buildWhere(spec) }

// ExpandTableName 展开库表备份的表名模板（{table} {yyyy} {mm} {dd} {yyyymm}）。实现在 names.go。
func ExpandTableName(tpl, table string, at time.Time) string { return expandTableName(tpl, table, at) }

// ExpandPath 展开文件路径模板（{inst} {db} {table} {yyyy} {mm} {dd} {run_id}）。实现在 names.go。
func ExpandPath(tpl string, spec Spec, instName string, at time.Time) string {
	return expandPath(tpl, spec, instName, at)
}

// SQLPreview 是“将执行的 SQL”（用示例参数填好，只用于展示，引擎实际执行用参数化语句）。
type SQLPreview struct {
	Select string `json:"select"`
	Insert string `json:"insert"` // 无库表备份时为空
	Delete string `json:"delete"` // copy 模式为空
}

// ---- 表信息 ----

type ColumnInfo struct {
	Name, Type string
	Nullable   bool
}

type TableInfo struct {
	DB, Name    string // 新增：库名、表名（Describe 回填；EnsureTable 用）
	CreateSQL   string // 新增：SHOW CREATE TABLE 原文（源表 Describe 回填；EnsureTable 在“其他实例”上建表时据此改写）
	IsView      bool   // 新增：对象存在但是视图
	Exists      bool
	Engine      string
	Columns     []ColumnInfo
	PrimaryKey  []string // 主键列（无主键时取第一个非空唯一键；都没有则为空）
	Indexes     map[string][]string
	Rows        int64 // information_schema 估算
	DataMB      int64
	Partitioned bool
	HasTriggers bool
	FKParents   []string // 被子表外键引用的表（"db.table"），非空视为红色
	Fingerprint string   // sha256(列名+类型+主键)
}

// ---- 连接与环境 ----

// SourceDB 是源库访问（主库）。真实实现是 NewMySQLSource；引擎自带的单元测试用内存假实现。
type SourceDB interface {
	Describe(ctx context.Context, db, table string) (TableInfo, error)
	Explain(ctx context.Context, query string, args ...any) (ExplainResult, error)
	CountWhere(ctx context.Context, db, table, where string, limit int64) (n int64, capped bool, err error)
	Sample(ctx context.Context, db, table, where string, cols []string, limit int) ([]map[string]string, error)
	ReadBatch(ctx context.Context, req ReadReq) (Batch, error)
	DeleteBatch(ctx context.Context, req DeleteReq) (affected int64, err error)
	// Fingerprint 只取结构指纹（比 Describe 轻），每批前核对用。
	Fingerprint(ctx context.Context, db, table string) (string, error)
	// InsertIgnore 恢复用：INSERT IGNORE 写回源表，返回实际插入行数。
	InsertIgnore(ctx context.Context, req InsertReq) (affected int64, err error)
	Close() error
}

// InsertReq 是 SourceDB.InsertIgnore 的参数。
type InsertReq struct {
	DB, Table  string
	Cols       []string
	Rows       []Row
	LockWaitSe int
}

// RangeReq 是 DestDB.ReadRange 的参数：读取备份表中 Min <= 主键 <= Max（均含）且主键 > After（After 非空时）的行，主键升序。
type RangeReq struct {
	DB, Table  string
	PKCols     []string
	Min, Max   []string
	After      []string
	Limit      int
	LockWaitSe int
}

// RetryableError 标记可重试的错误（死锁、锁等待超时）。真实实现把 MySQL 1205/1213 包成它；测试替身可直接返回。
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return "retryable: " + e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// ErrMismatch 表示备份端已有同主键但内容不一致的行，或写入后回读不一致；引擎据此以 verify_failed 结束，且不会删除源行。
var ErrMismatch = errors.New("archive/engine: backup row mismatch")

type ExplainResult struct {
	UsedIndex string
	ScanRows  int64
	Type      string // ALL / range / ref ...
	Extra     string
}

type ReadReq struct {
	DB, Table  string
	Where      string
	PKCols     []string
	After      []string // 断点：主键 > After；空表示从头（Unordered 时忽略）
	Unordered  bool     // 不排序、不翻页：只取“当前符合条件的前 Limit 行”（会删数据的模式：删掉之后下一批自然取到剩下的，不需要断点）
	Limit      int
	LockWaitSe int
}

type Row = []any

type Batch struct {
	Cols []string
	Rows []Row
	PKs  [][]string // 每行的主键值（字符串形式，与 PKCols 同序）
}

type DeleteReq struct {
	DB, Table  string
	Where      string // 重复一遍归档条件
	PKCols     []string
	PKs        [][]string
	LockWaitSe int
}

// DestDB 是库表备份去向的访问。
type DestDB interface {
	// EnsureTable 目标表不存在时按源表结构创建（CREATE TABLE ... LIKE 或等价），已存在则校验列兼容。
	EnsureTable(ctx context.Context, db, table string, src TableInfo) error
	// Insert 写入一批；主键已存在且内容一致视为已存在（计入 existed）；内容不一致返回错误。
	Insert(ctx context.Context, db, table string, cols []string, rows []Row, pkCols []string) (inserted, existed int64, err error)
	// VerifyPKs 返回这些主键中目标端存在的数量。
	VerifyPKs(ctx context.Context, db, table string, pkCols []string, pks [][]string) (found int64, err error)
	DiskFreePct(ctx context.Context) (pct float64, ok bool, err error)
	// Describe 只读取目标表信息（试运行检查用，不建表）；表不存在时 Exists=false。
	Describe(ctx context.Context, db, table string) (TableInfo, error)
	// DBExists 目标库是否存在（试运行检查用）。
	DBExists(ctx context.Context, db string) (bool, error)
	// ReadRange 恢复用：按主键范围读取备份表。
	ReadRange(ctx context.Context, req RangeReq) (Batch, error)
	Close() error
}

// LoadProbe 是源库负载探针。
type LoadProbe interface {
	ThreadsRunning(ctx context.Context) (int, error)
	// ReplicaLag 返回最大复制延迟（秒）；hasReplica=false 表示没有从库（该项通过）。
	ReplicaLag(ctx context.Context) (sec int, hasReplica bool, err error)
	DiskFreePct(ctx context.Context) (pct float64, ok bool, err error)
}

// Env 是一次运行用到的外部资源，用完调用 Close。
type Env struct {
	Src      SourceDB
	Dst      DestDB    // 无库表备份时为 nil
	Files    FileStore // 无文件备份时为 nil
	Probe    LoadProbe
	FileKey  []byte // 32 字节，Encrypt=true 时必须有
	Now      func() time.Time
	Sleep    func(ctx context.Context, d time.Duration) error // 测试里可替换；nil 用真实睡眠
	Closers  []io.Closer
	TempDir  string // 新增：文件备份的本地临时目录；空则用系统临时目录下的 dba-archive（生产环境应指向持久目录，崩溃后里面的 .tmp 文件是尚未上传的备份）
	InstName string // 新增：源实例名，用于展开文件路径模板的 {inst}；空则用 inst<实例ID>
}

// Close 关闭 Src / Dst 与 Closers（实现在 opener.go）。
func (e *Env) Close() error { return closeEnv(e) }

// ConnResolver 由上层提供：按实例 ID 取解密后的连接信息和实例名；实例不存在、已停用或不是主库时返回错误。
type ConnResolver func(ctx context.Context, instanceID uint) (conn eng.Conn, instName string, err error)

// StoreResolver 由上层提供：按备份存储 ID 取解密后的配置。
type StoreResolver func(ctx context.Context, storeID uint) (StoreConfig, error)

// Opener 按 Spec 打开 Env（真实环境；测试里直接构造 Env）。
type Opener struct {
	Conn    ConnResolver
	Store   StoreResolver
	FileKey func() ([]byte, error)

	// 以下为新增的可选项（nil 表示不提供，对应检查项在运行记录里标“未检测”）：

	// Replicas 返回源实例的从库连接信息，用于复制延迟检测；nil 时只能判断“是否有从库”（SHOW REPLICAS），测不了延迟。
	Replicas func(ctx context.Context, instanceID uint) ([]eng.Conn, error)
	// SlaveByHost 在 Replicas 为 nil 时启用“自动发现”：Opener 查源库 processlist 里的 Binlog Dump 线程得到从库主机，
	// 再用它按主机取平台里登记的从库实例连接（0 个或出错 = 认不出，探针报“复制延迟未检测”而不是当作没有从库）。
	SlaveByHost func(ctx context.Context, host string) ([]eng.Conn, error)
	// Disk 返回实例所在磁盘的剩余百分比（上层从巡检指标取）；ok=false 表示取不到。
	Disk func(ctx context.Context, instanceID uint) (pct float64, ok bool, err error)
	// TempDir 文件备份的本地临时目录。
	TempDir string
	// Lenient 返回该实例是否是“MySQL 协议兼容实现”（PolarDB-X 等）：会话里的锁等待参数设不上、读不到 sql_log_bin 时
	// 降级继续（不中止运行），负载探针本来就会标“未检测”。nil 表示都按 MySQL 严格处理。
	Lenient func(ctx context.Context, instanceID uint) bool
}

// Open 会话级设置 lock_wait_timeout / innodb_lock_wait_timeout / time_zone / sql_mode，并拒绝 sql_log_bin=0。实现在 opener.go。
func (o Opener) Open(ctx context.Context, spec Spec) (*Env, error) { return o.open(ctx, spec) }

// ---- 备份存储 ----

// StoreConfig 是解密后的备份存储配置。Type：local（本地目录）| nfs（挂载目录，用法同 local）| s3（S3 兼容含 MinIO）| oss（阿里云 OSS 的 S3 兼容端点）。
type StoreConfig struct {
	ID            uint
	Name          string
	Type          string
	Path          string // local / nfs：根目录
	Endpoint      string // s3 / oss：如 https://oss-cn-hangzhou.aliyuncs.com 或 http://minio:9000
	Region        string
	Bucket        string
	Prefix        string
	AK, SK        string
	PathStyle     bool // s3：路径风格（MinIO 通常需要）
	RetentionDays int
}

type FileInfo struct {
	Key    string
	Size   int64
	SHA256 string // 可空：Stat 不保证
}

// FileStore 是备份文件存储。Key 是相对 Prefix / 根目录的路径。
type FileStore interface {
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Stat(ctx context.Context, key string) (FileInfo, error)
	Delete(ctx context.Context, key string) error
	// Test 写入、读取、删除一个探测对象，返回耗时与错误（给“测试连接”用）。
	Test(ctx context.Context) (time.Duration, error)
	// Free 返回可用空间字节数；ok=false 表示无法得知（对象存储）。
	Free(ctx context.Context) (bytes int64, ok bool, err error)
}

// NewFileStore 按类型创建。不依赖第三方库（S3 / OSS 用标准库实现 SigV4）。实现在 filestore.go。
func NewFileStore(cfg StoreConfig) (FileStore, error) { return newFileStore(cfg) }

// ErrNotExist 是 FileStore.Stat / Get 对象不存在时返回（或包装）的错误，errors.Is(err, ErrNotExist) 判断。
var ErrNotExist = fs.ErrNotExist

// ---- 试运行 ----

type CheckItem struct {
	Key    string `json:"key"` // pk / index / fk / trigger / partition / retention / dest_table / dest_store / protected / large_batch ...
	Title  string `json:"title"`
	Level  string `json:"level"` // ok | warn | error
	Detail string `json:"detail"`
}

type CheckReport struct {
	Items       []CheckItem         `json:"items"`
	EstRows     int64               `json:"est_rows"` // 命中行数（COUNT 有上限时为下界，Capped=true）
	Capped      bool                `json:"capped"`
	TableRows   int64               `json:"table_rows"`
	Percent     float64             `json:"percent"`
	EstBatches  int                 `json:"est_batches"`
	EstSeconds  int                 `json:"est_seconds"`
	EstBinlogMB int                 `json:"est_binlog_mb"`
	SampleCols  []string            `json:"sample_cols"`
	Sample      []map[string]string `json:"sample"`
	Fingerprint string              `json:"fingerprint"`
	SQL         SQLPreview          `json:"sql"`
	PKCols      []string            `json:"pk_cols"`
}

// HasError 判断是否有红色项。
func (r *CheckReport) HasError() bool {
	for _, it := range r.Items {
		if it.Level == "error" {
			return true
		}
	}
	return false
}

// ---- 运行 ----

type Event struct {
	At     time.Time         `json:"at"`
	Kind   string            `json:"kind"` // log | batch | throttle | resume | warn | checkpoint | verify | file | structure
	Msg    string            `json:"msg"`
	Fields map[string]string `json:"fields,omitempty"`
}

type Progress struct {
	Scanned, Backed, Deleted int64
	Batches                  int
	LastPK                   []string
	BatchSize                int
	Threads                  int
	LagSeconds               int
	Throttled                bool
	ThrottleReason           string
	RowsPerSec               float64
}

// Sink 接收运行中的事件与进度（上层据此写运行记录、推送给页面）。实现必须并发安全且不能阻塞引擎。
type Sink interface {
	Event(Event)
	Progress(Progress)
}

// Control 供上层在运行中暂停 / 恢复 / 终止。当前批做完后生效。
// 实现在 control.go（NewControl / Pause / Resume / Stop / Paused / Stopped）。
type Control struct {
	mu      sync.Mutex
	paused  bool
	stopped bool
	wake    chan struct{} // 状态变化时关闭并重建
	done    chan struct{} // Stop 后关闭
}

const (
	ResultSuccess      = "success"       // 没有更多符合条件的行
	ResultWindowEnd    = "window_end"    // 窗口结束，优雅中断
	ResultLoadAbort    = "load_abort"    // 负载超限等待超时
	ResultLimitReached = "limit_reached" // 达到单次行数上限
	ResultFailed       = "failed"        // 连续失败
	ResultVerifyFailed = "verify_failed" // 备份校验不一致（该批不删源）
	ResultStopped      = "stopped"       // 人工终止
	ResultStructure    = "structure_changed"
)

type Manifest struct {
	Files []ManifestFile `json:"files"`
	Rows  int64          `json:"rows"`
	MinPK []string       `json:"min_pk"`
	MaxPK []string       `json:"max_pk"`
	Cols  []ColumnInfo   `json:"cols"`
	Enc   string         `json:"enc"` // "" | aes-256-gcm

	// 以下为新增字段（解码备份文件需要）
	Format      string `json:"format,omitempty"`       // csv | sql
	Gzip        bool   `json:"gzip,omitempty"`         // 每批一个 gzip 成员
	Table       string `json:"table,omitempty"`        // 源表名
	ManifestKey string `json:"manifest_key,omitempty"` // 清单自身在存储里的 Key
}

type ManifestFile struct {
	Key    string `json:"key"`
	Rows   int64  `json:"rows"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type RunResult struct {
	Result        string        `json:"result"`
	Scanned       int64         `json:"scanned"`
	Backed        int64         `json:"backed"`
	Deleted       int64         `json:"deleted"`
	Batches       int           `json:"batches"`
	Duration      time.Duration `json:"duration"`
	PeakThreads   int           `json:"peak_threads"`
	ThrottleCount int           `json:"throttle_count"`
	Checkpoint    []string      `json:"checkpoint"`
	MinPK, MaxPK  []string      `json:"-"`          // 本次运行处理的主键范围（恢复用）
	DestTable     string        `json:"dest_table"` // 实际使用的库表备份表名
	Manifest      *Manifest     `json:"manifest,omitempty"`
	Err           string        `json:"err,omitempty"`
	AutoPause     bool          `json:"auto_pause"` // 建议上层自动暂停任务（连续失败 / 校验失败 / 结构变更）
}

// Engine 无状态，可并发使用。
type Engine struct{}

func New() *Engine { return &Engine{} }

// Check 试运行：只读，不写不删（对源库只发 SELECT / EXPLAIN / information_schema 查询；对文件存储只做连通性探测）。实现在 check.go。
func (e *Engine) Check(ctx context.Context, spec Spec, pol Policy, env *Env) (*CheckReport, error) {
	return e.check(ctx, spec, pol, env)
}

// Run 执行一次归档运行，直到完成、被窗口 / 负载 / 上限中断、失败或被终止。ctx 取消视为 stopped。
//
// 返回约定：运行期的一切结果（含失败、校验失败、结构变更）都在 RunResult.Result 里，error 为 nil；
// 只有运行根本没法开始（规格 / 策略 / Env 非法，如 Validate 有 error 级问题）才返回 (nil, error)。实现在 run.go。
func (e *Engine) Run(ctx context.Context, spec Spec, pol Policy, env *Env, ctl *Control, sink Sink) (*RunResult, error) {
	return e.run(ctx, spec, pol, env, ctl, sink)
}

// RestoreRange 是要恢复的运行的主键范围与备份表。
type RestoreRange struct {
	DestInstanceID uint
	DestDB         string
	DestTable      string
	MinPK, MaxPK   []string
}

// Restore 把库表备份中该主键范围的数据 INSERT IGNORE 回源表（只补缺、不覆盖）。Env.Src 用于写源表，Env.Dst 用于读备份。
// 返回的 RunResult：Scanned = 从备份表读到的行数，Backed = 实际补回源表的行数（INSERT IGNORE 的受影响行数），Deleted = 0。实现在 restore.go。
func (e *Engine) Restore(ctx context.Context, spec Spec, rng RestoreRange, env *Env, ctl *Control, sink Sink) (*RunResult, error) {
	return e.restore(ctx, spec, rng, env, ctl, sink)
}
