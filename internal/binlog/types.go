// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

package binlog

import (
	"fmt"
	"net/http"
	"time"

	"dbtool/internal/i18n"
)

// 按钮权限码（登记表 internal/btnperm/registry.go）。
const (
	BtnView     = "binlog.view"     // 打开页面、列出 binlog 文件
	BtnParse    = "binlog.parse"    // 解析出 SQL
	BtnRollback = "binlog.rollback" // 生成回滚 SQL（高危）
	BtnDownload = "binlog.download" // 下载结果文件（高危）
)

// 审计动作名（audit.Classify 按前缀归类，见 internal/audit/ext.go）。
const (
	actParse    = "Binlog解析-解析" // i18n-ignore 审计动作名，存库的枚举
	actRollback = "Binlog解析-回滚" // i18n-ignore 审计动作名，存库的枚举
	actDownload = "Binlog解析-下载" // i18n-ignore 审计动作名，存库的枚举
	actCancel   = "Binlog解析-取消" // i18n-ignore 审计动作名，存库的枚举
)

// 工作模式。
const (
	ModeSQL      = "sql"      // 还原成原始 SQL
	ModeRollback = "rollback" // 生成回滚 SQL
)

// 语句类型（既是过滤条件，也是结果行的类型）。
const (
	KindInsert    = "INSERT"
	KindUpdate    = "UPDATE"
	KindDelete    = "DELETE"
	KindDDL       = "DDL"       // 结构变更等非行事件；不可回滚
	KindStatement = "STATEMENT" // binlog_format=STATEMENT/MIXED 里的 DML 原文；不可回滚
)

// 上限。页面可以在这些范围内调整；超过硬上限一律拒绝。
// “最多条数 / 结果体积”的硬上限来自系统设置（binlog.max_rows / binlog.max_result_mb，见 Service.HardLimits）；
// DefaultHardRows / DefaultHardMB 是读不到设置时的取值，与 internal/settings 里登记的默认值一致（有测试核对）。
const (
	DefaultMaxRows    = 500
	DefaultHardRows   = 500_000
	DefaultMaxMB      = 2
	DefaultHardMB     = 1024
	MaxScanEvents     = 5_000_000 // 单次任务最多扫描的事件数（含不匹配过滤条件的）
	DefaultTimeoutSec = 120
	MaxTimeoutSec     = 600
	RowsPageMax       = 200 // 结果分页：每页最多多少行

	maxActivePerUser = 2
	maxActiveAll     = 6
	maxStoredJobs    = 40
	minStoreBytes    = 96 << 20 // 所有任务结果合计的内存预算的下限；预算随体积硬上限放大，见 storeBudget
	jobTTL           = 30 * time.Minute
	maxSegment       = 9999 // 回滚分段的段号上限（段号只是说明，页面每续一段加 1）
	maxFilterItems   = 50
	maxFilterLen     = 128
	maxWarnings      = 30
	startTimeout     = 25 * time.Second // 同步阶段（连接、读 binlog 列表与变量）的时限
	stmtOverhead     = 160              // 每行结果除 SQL 文本外的估算开销，计入体积上限
)

// Error 是业务错误，Message 可直接展示给用户。
type Error struct {
	HTTP    int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func newErr(status int, code, msg string) *Error {
	return &Error{HTTP: status, Code: code, Message: msg}
}
func bad(msg string) *Error { return newErr(http.StatusBadRequest, "bad_request", msg) }

// LogFile 是 SHOW BINARY LOGS 的一行。
type LogFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// ServerInfo 是解析前需要了解的目标实例信息。
type ServerInfo struct {
	Version     string    `json:"version"`
	Flavor      string    `json:"flavor"` // mysql / mariadb
	LogBin      bool      `json:"log_bin"`
	Format      string    `json:"binlog_format"`
	RowImage    string    `json:"binlog_row_image"`
	Compression bool      `json:"binlog_transaction_compression,omitempty"`
	TZOffset    int       `json:"tz_offset_seconds"` // 服务器当前时区相对 UTC 的偏移（页面里输入的时间按服务器时区解释）
	Files       []LogFile `json:"files"`

	CanRollback    bool   `json:"can_rollback"`
	RollbackReason string `json:"rollback_reason,omitempty"`
}

// rollbackProblem 判断实例当前配置能否生成回滚 SQL，不能时返回原因。
func (i *ServerInfo) rollbackProblem() string {
	switch {
	case !i.LogBin:
		return "实例没有开启 binlog（log_bin=OFF）"
	case i.Format == "":
		return "读取不到 binlog_format"
	case i.Format != "ROW":
		return fmt.Sprintf("binlog_format=%s，回滚需要 ROW 格式（STATEMENT / MIXED 里的语句无法还原行数据）", i.Format)
	case i.RowImage == "":
		return "读取不到 binlog_row_image"
	case i.RowImage != "FULL":
		return fmt.Sprintf("binlog_row_image=%s，回滚需要 FULL（MINIMAL / NOBLOB 的行镜像不完整）", i.RowImage)
	}
	return ""
}

// Req 是一次解析的参数。
type Req struct {
	InstanceID uint   `json:"instance_id"`
	Mode       string `json:"mode"` // sql / rollback

	StartFile string `json:"start_file"`
	StartPos  uint32 `json:"start_pos"` // 0 表示文件开头
	EndFile   string `json:"end_file"`  // 空表示读到当前最后一个文件的末尾
	EndPos    uint32 `json:"end_pos"`   // 0 表示该文件末尾
	StartTime string `json:"start_time"`
	StopTime  string `json:"stop_time"`

	Databases []string `json:"databases"`
	Tables    []string `json:"tables"`
	SQLTypes  []string `json:"sql_types"` // insert / update / delete / ddl；空表示 insert+update+delete

	MaxRows    int `json:"max_rows"`
	MaxMB      int `json:"max_mb"`
	TimeoutSec int `json:"timeout_sec"`

	NoDBPrefix  bool `json:"no_db_prefix"`
	ExtraInfo   bool `json:"extra_info"`
	FullColumns bool `json:"full_columns"`

	// Segment 是回滚分段的段号（1 起）：从上一段的续接位置继续生成时由页面带上，只用于结果文件里的说明与页面提示，不影响解析。
	Segment int `json:"segment,omitempty"`
}

// Stmt 是结果里的一条语句。
type Stmt struct {
	Seq        int    `json:"seq"`
	Trx        int    `json:"trx"` // 所在事务序号，0 表示不在事务里
	Time       string `json:"time"`
	File       string `json:"file"`
	Pos        uint32 `json:"pos"`
	EndPos     uint32 `json:"end_pos"`
	Schema     string `json:"schema"`
	Table      string `json:"table"`
	Type       string `json:"type"` // 原始操作类型
	SQL        string `json:"sql"`  // 模式 sql 时是还原出的语句，模式 rollback 时是回滚语句
	NoRollback bool   `json:"no_rollback,omitempty"`
}

func (s Stmt) size() int { return len(s.SQL) + len(s.Schema) + len(s.Table) + stmtOverhead }

// 任务状态。
const (
	StatusQueued   = "queued"
	StatusRunning  = "running"
	StatusDone     = "done"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
)

// JobView 是任务的状态（不含结果行，结果行分页取）。
type JobView struct {
	ID         string `json:"id"`
	Mode       string `json:"mode"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	Instance   string `json:"instance"`
	Owner      string `json:"owner"`
	Mine       bool   `json:"mine"`
	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	RemainSec  int    `json:"remain_seconds"` // 距离结果被清理还有多少秒；进行中的任务为 -1（结束后才开始计时）
	KeepMin    int    `json:"keep_minutes"`

	Range string `json:"range"` // 范围的文字描述
	// 进度
	Scanned     int64          `json:"scanned"`
	Matched     int            `json:"matched"`
	File        string         `json:"file"`
	Pos         uint32         `json:"pos"`
	Bytes       int            `json:"bytes"`
	Limits      Limits         `json:"limits"`
	Truncate    string         `json:"truncated,omitempty"` // 非空表示结果被截断及原因
	Warnings    []string       `json:"warnings"`
	ByType      map[string]int `json:"by_type"`
	Trx         int            `json:"trx"`
	CanDownload bool           `json:"can_download"`
	// Segment 只在回滚任务里出现：这是第几段，后面还有没有，续接位置在哪。
	Segment *SegmentView `json:"segment,omitempty"`

	req Req // 任务的请求参数：Range 按请求语言重新生成时用（不输出）
}

// SegmentView 描述回滚分段：到上限时在事务边界截断，这一段的回滚 SQL 是完整的，后面的变更从 NextFile:NextPos 继续生成下一段。
// 完整回滚要按段号从大到小执行：先执行最后一段，最后执行第 1 段；每一段的文件内部再按文件顺序执行。
type SegmentView struct {
	Index    int    `json:"index"`
	More     bool   `json:"more"`
	NextFile string `json:"next_file,omitempty"`
	NextPos  uint32 `json:"next_pos,omitempty"`
	Reason   string `json:"reason,omitempty"` // rows / bytes / scan / timeout / stall
}

// localize 把任务视图里服务端生成的中文说明翻译成 lang：范围描述按请求参数重新生成，
// 错误 / 警告 / 截断说明（任务结束时按中文原文存下的）按目录匹配（含带 %s %d 的格式串）。中文原样不动。
func (v *JobView) localize(lang string) {
	if lang == "" || lang == i18n.ZH {
		return
	}
	v.Range = describeIn(lang, v.req)
	v.Error = i18n.TP(lang, v.Error)
	v.Truncate = i18n.TP(lang, v.Truncate)
	for i, w := range v.Warnings {
		v.Warnings[i] = i18n.TP(lang, w)
	}
}

// Limits 是本次任务生效的上限。
type Limits struct {
	MaxRows    int `json:"max_rows"`
	MaxBytes   int `json:"max_bytes"`
	MaxScan    int `json:"max_scan"`
	TimeoutSec int `json:"timeout_sec"`
}

// RowsPage 是一页结果。
type RowsPage struct {
	Total  int    `json:"total"`
	Offset int    `json:"offset"`
	Rows   []Stmt `json:"rows"`
}
