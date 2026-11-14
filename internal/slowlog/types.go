// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

package slowlog

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/query"
)

// 按钮权限码（登记表 internal/btnperm）。
const (
	permQueryAll     = "query.admin_run"  // 查看 / 查询全部实例；否则按资源组关联
	btnAdvise        = "slowquery.advise" // 获取优化建议
	btnOpenAdvisor   = "optimize.explain" // 打开“SQL 优化工具”页
	actionAdviseName = "慢日志获取优化建议"        // dbt_audit_log.action
)

// viewButtons 是两个慢日志页面（慢日志检索 / 慢 SQL 洞察）共用数据接口的“查看”按钮：任一即可。
var viewButtons = []string{"slowlogs.view", "slowquery.view"}

// 数据源。
const (
	SourceDigest    = "digest"    // pt-query-digest 推送的 dbt_mysql_slow_query_review(_history)
	SourceCollected = "collected" // 平台采集的 dbt_slow_logs
	SourceRedis     = "redis"     // Redis 慢查询（dbt_redis_slowlog，SLOWLOG GET 采集）
)

// 查询约束：慢日志表可能很大，所有查询都必须带时间范围、分页和上限。
const (
	MaxSpanDays   = 93    // 时间范围最长天数
	DefaultLimit  = 30    // 默认每页条数
	MaxLimit      = 200   // 每页条数上限
	MaxOffset     = 20000 // 翻页偏移上限（防深分页拖垮数据库）
	MaxSearchLen  = 200   // 搜索关键字长度上限
	MaxAdviseSQL  = 100000
	DetailRows    = 100 // 聚合行“详细”最多返回的条数
	maxTrendRows  = 100000
	maxTrendPoint = 400
	maxTextRunes  = 20000 // 返回给前端的 SQL 文本最长字符数
)

// Principal 是当前登录用户（与 query 包一致，避免模块间多余转换）。
type Principal = query.Principal

// Error 是对外可见的业务错误，Message 可直接展示给用户。
type Error struct {
	HTTP    int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func newErr(status int, code, format string, a ...any) *Error {
	return &Error{HTTP: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

func badReq(format string, a ...any) *Error {
	return newErr(http.StatusBadRequest, "bad_request", format, a...)
}

// ---- 实例 ----

// InstanceInfo 是页面实例下拉里的一项（不含主机、账号等敏感信息）。
type InstanceInfo struct {
	ID      uint   `json:"id"`
	Name    string `json:"name"`
	DBType  string `json:"db_type"`
	Source  string `json:"source"`          // 慢查日志页默认数据源；空表示该类型暂不支持
	Hint    string `json:"hint,omitempty"`  // 不支持或需注意时的说明
	Collect bool   `json:"collect_enabled"` // dbt_sql_instance.is_enable：采集开关
}

// ---- “查慢日志”：dbt_slow_logs 聚合 ----

// GroupParams 是聚合列表的请求参数（原始字符串，由服务校验）。
type GroupParams struct {
	InstanceID uint   // 0 表示全部可见实例
	DB         string // 空表示全部库
	Search     string // 在 SQL 文本、库名、用户@主机 里模糊匹配
	Start, End string // YYYY-MM-DD，End 含当天
	Sort       string
	Order      string // asc / desc
	Limit      int
	Offset     int
}

// GroupQuery 是校验后交给仓库的聚合查询。
type GroupQuery struct {
	InstanceIDs []uint // 为空表示不限实例（仅管理员且没有选择实例时）
	DB          string // 空表示不按库过滤
	Search      string
	From, To    string // [From, To)，YYYY-MM-DD HH:MM:SS
	Sort        string
	Desc        bool
	Limit       int
	Offset      int
}

// SlowLogGroup 是慢日志列表的一行：某实例 × 某库 × 某指纹 × 某天。
type SlowLogGroup struct {
	InstanceID      uint    `json:"instance_id"`
	InstanceName    string  `json:"instance_name"`
	DBName          string  `json:"db_name"`
	SQLMD5          string  `json:"sql_md5"`
	ExecDate        string  `json:"exec_date"`
	Count           int64   `json:"count"`
	UserHost        string  `json:"user_host"` // 当天最近一条的用户@主机（代表值）
	SQLText         string  `json:"sql_text"`  // 当天最近一条的 SQL（代表值）
	TableName       string  `json:"table_name"`
	QueryTimeAvg    float64 `json:"query_time_avg"`
	QueryTimeMax    float64 `json:"query_time_max"`
	QueryTimeMin    float64 `json:"query_time_min"`
	QueryTimeSum    float64 `json:"query_time_sum"` // 当天该指纹的累计耗时（秒）
	Fingerprint     string  `json:"fingerprint"`    // SQL 指纹（字面量为 ?），服务层由代表 SQL 计算
	RowsSentAvg     int64   `json:"rows_sent_avg"`
	RowsSentMax     int64   `json:"rows_sent_max"`
	RowsSentMin     int64   `json:"rows_sent_min"`
	RowsExaminedAvg int64   `json:"rows_examined_avg"`
	RowsExaminedMax int64   `json:"rows_examined_max"`
	RowsExaminedMin int64   `json:"rows_examined_min"`
	TextTruncated   bool    `json:"text_truncated,omitempty"`
}

// GroupPage 是聚合列表的一页。
type GroupPage struct {
	Total int64          `json:"total"`
	Rows  []SlowLogGroup `json:"rows"`
	From  string         `json:"from"` // 实际使用的日期范围
	To    string         `json:"to"`
}

// DetailParams 是聚合行“详细”的请求参数。
type DetailParams struct {
	InstanceID uint
	DB         string
	MD5        string
	Day        string // YYYY-MM-DD
}

// DetailQuery 是校验后的明细查询。
type DetailQuery struct {
	InstanceID uint
	DB         string
	MD5        string
	From, To   string
	Limit      int
}

// SlowLogRow 是 dbt_slow_logs 的一条记录。
type SlowLogRow struct {
	ID            uint    `json:"id"`
	StartTime     string  `json:"start_time"`
	UserHost      string  `json:"user_host"`
	DBName        string  `json:"db_name"`
	QueryTimeSec  float64 `json:"query_time_sec"`
	LockTimeSec   float64 `json:"lock_time_sec"`
	RowsSent      int64   `json:"rows_sent"`
	RowsExamined  int64   `json:"rows_examined"`
	SQLText       string  `json:"sql_text"`
	TextTruncated bool    `json:"text_truncated,omitempty"`
}

// ---- “慢查日志”：统计 / 明细 / 趋势 ----

// ReviewParams 是统计、明细、趋势接口共用的请求参数。
type ReviewParams struct {
	InstanceID uint
	DB         string
	Source     string // 空或 auto：按实例类型自动选；digest / collected：mysql 实例手动指定
	SQLID      string // 明细按指纹过滤；趋势必填
	Search     string
	Start, End string
	Sort       string
	Order      string
	Bucket     string // 趋势：day / hour
	Limit      int
	Offset     int
	MinSeconds string // 耗时下限（秒，可带小数）：统计页按平均耗时、明细页按单次耗时过滤；空表示不限
	All        bool   // 统计页的“全部实例”汇总（此时 InstanceID 为 0）
}

// ReviewQuery 是校验后的统计/明细查询。
type ReviewQuery struct {
	InstanceID uint
	HostKey    string // host:port，pt-query-digest 表里的 hostname_max
	DB         string
	SQLID      string
	Search     string
	From, To   string
	Sort       string
	Desc       bool
	Limit      int
	Offset     int
	MinSeconds float64        // 耗时下限（秒）；0 表示不限
	Multi      []ReviewTarget // 非空表示“全部实例”汇总：按这些实例查（InstanceID / HostKey 不再使用）
}

// ReviewTarget 是“全部实例”汇总里的一个实例。
type ReviewTarget struct {
	InstanceID uint
	HostKey    string
}

// TrendQuery 是校验后的趋势查询。
type TrendQuery struct {
	InstanceID uint
	HostKey    string
	SQLID      string
	From, To   string
	Bucket     string
}

// ReviewRow 是统计的一行（按 SQL 指纹聚合）。
type ReviewRow struct {
	SQLID          string  `json:"sql_id"`
	SQLText        string  `json:"sql_text"` // SQL 指纹（字面量为 ?）
	CreateTime     string  `json:"create_time"`
	DBName         string  `json:"db_name"`
	ExecCount      int64   `json:"exec_count"`
	ExecTimeSum    float64 `json:"exec_time_sum"`
	QueryTimeAvg   float64 `json:"query_time_avg"`
	ParseTotalRows int64   `json:"parse_total_rows"`
	ReturnTotal    int64   `json:"return_total_rows"`
	ParseRowAvg    int64   `json:"parse_row_avg"`
	ReturnRowAvg   int64   `json:"return_row_avg"`
	Truncated      bool    `json:"text_truncated,omitempty"`

	// 以下是统计页后加的列（旧的调用方不受影响）
	InstanceID   uint    `json:"instance_id,omitempty"`
	InstanceName string  `json:"instance_name,omitempty"`
	QueryTimeMax float64 `json:"query_time_max"`
	LockTimeAvg  float64 `json:"lock_time_avg"`
	FirstSeen    string  `json:"first_seen"`
	LastSeen     string  `json:"last_seen"`
	SampleSQL    string  `json:"sample_sql,omitempty"` // 平台采集来源：最近一条的真实 SQL（带字面量），供“优化建议”使用
	Spark        []int64 `json:"spark,omitempty"`      // 范围内每天的慢查次数（从开始日期起逐日）
	HostKey      string  `json:"-"`
}

// ReviewPage 是统计的一页。
type ReviewPage struct {
	Total  int64          `json:"total"`
	Rows   []ReviewRow    `json:"rows"`
	Source string         `json:"source"`
	Label  string         `json:"source_label"`
	From   string         `json:"from"`
	To     string         `json:"to"`
	All    bool           `json:"all,omitempty"` // 全部实例汇总
	Notes  []string       `json:"notes,omitempty"`
	Sum    *ReviewSummary `json:"summary,omitempty"`
}

// ReviewSummary 是统计页顶部 KPI：同一组过滤条件下（不分页）的汇总。
type ReviewSummary struct {
	Fingerprints int64   `json:"fingerprints"`
	ExecCount    int64   `json:"exec_count"`
	ExecTimeSum  float64 `json:"exec_time_sum"`
	QueryTimeMax float64 `json:"query_time_max"`
}

// HistoryRow 是明细的一行。pt-query-digest 来源每 5 分钟一行（含次数），平台采集来源每条慢日志一行。
type HistoryRow struct {
	ID           uint    `json:"id"`
	SQLID        string  `json:"sql_id"`
	ExecStart    string  `json:"exec_start"`
	DBName       string  `json:"db_name"`
	HostAddress  string  `json:"host_address"`
	SQLText      string  `json:"sql_text"`
	ExecCount    int64   `json:"exec_count"`
	QueryPct95   float64 `json:"query_time_pct95"`
	QueryTimeSum float64 `json:"query_time_sum"`
	LockTimeSum  float64 `json:"lock_time_sum"`
	ParseRows    int64   `json:"parse_rows"`
	ReturnRows   int64   `json:"return_rows"`
	Truncated    bool    `json:"text_truncated,omitempty"`
}

// HistoryPage 是明细的一页。
type HistoryPage struct {
	Total  int64        `json:"total"`
	Rows   []HistoryRow `json:"rows"`
	Source string       `json:"source"`
	Label  string       `json:"source_label"`
	From   string       `json:"from"`
	To     string       `json:"to"`
}

// TrendPoint 是趋势的一个时间桶。
type TrendPoint struct {
	T     string  `json:"t"` // 日 YYYY-MM-DD 或小时 YYYY-MM-DD HH:00
	Count int64   `json:"count"`
	Pct95 float64 `json:"pct95"`
	Avg   float64 `json:"avg"`
	Max   float64 `json:"max"`
}

// Trend 是某条 SQL 指纹的历史趋势。
type Trend struct {
	Bucket string       `json:"bucket"`
	Points []TrendPoint `json:"points"`
	Source string       `json:"source"`
	Label  string       `json:"source_label"`
	// Approx 为 true 表示 95% 耗时只基于部分明细估算（平台采集来源的行数超过上限）。
	Approx bool   `json:"approx,omitempty"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// ---- 采集状态 ----

// CollectState 是某个实例最近一次采集的结果。
type CollectState struct {
	InstanceID   uint      `json:"instance_id"`
	InstanceName string    `json:"instance_name,omitempty"`
	LastRunAt    time.Time `json:"last_run_at"`
	LastOKAt     time.Time `json:"last_ok_at"`
	Status       string    `json:"status"` // ok / error / skipped
	Error        string    `json:"error,omitempty"`
	LastRows     int       `json:"last_rows"`
	TotalRows    int64     `json:"total_rows"`
	DurationMs   int64     `json:"duration_ms"`
}

// CollectorOverview 是采集器页面卡片的数据。
type CollectorOverview struct {
	Enabled     bool           `json:"enabled"`      // 本进程是否启用了采集器
	IntervalSec int            `json:"interval_sec"` // 采集间隔
	Workers     int            `json:"workers"`
	StateTable  bool           `json:"state_table"` // dbt_slowlog_collect_state 是否已创建
	Items       []CollectState `json:"items"`
}

// ---- 优化建议 ----

// AdviseParams 是“获取优化建议”的请求。
type AdviseParams struct {
	InstanceID   uint    `json:"instance_id"`
	DB           string  `json:"db_name"`
	SQL          string  `json:"sql"`
	RowsExamined int64   `json:"rows_examined"` // 可选：来自选中的慢日志行
	RowsSent     int64   `json:"rows_sent"`
	QueryTime    float64 `json:"query_time"`
	LockTime     float64 `json:"lock_time"` // 可选：平均锁等待（秒），用来判断耗时是不是主要花在等锁上
}

// AdviceItem 是一条建议。
type AdviceItem struct {
	Level  string `json:"level"` // crit / warn / info
	Code   string `json:"code"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// Advice 是建议结果。
type Advice struct {
	Source string       `json:"source"`
	Label  string       `json:"source_label"`
	Items  []AdviceItem `json:"items"`
	Score  int          `json:"score"`               // 综合评分（100 分起，按建议扣分；越高越好）
	DDL    []string     `json:"index_ddl,omitempty"` // 由 WHERE 条件推测的候选索引 DDL（仅供参考）
}

// AdviseRequest 是交给 Advisor 的输入（已校验，且已确认实例在用户可见范围内）。
type AdviseRequest struct {
	InstanceID   uint
	InstanceName string
	DBType       string
	DB           string
	SQL          string
	RowsExamined int64
	RowsSent     int64
	QueryTime    float64
	LockTime     float64
}

// Advisor 生成优化建议。默认实现是基于规则的静态分析（RuleAdvisor）；
// 之后 sqlopt 模块可以实现这个接口接入 SQLAdvisor/SOAR/EXPLAIN（会访问目标库的实现需自行做超时与只读保护）。
type Advisor interface {
	Advise(ctx context.Context, req AdviseRequest) (*Advice, error)
}

// ---- 仓库 ----

// Entry 是从目标库拉取的一条慢日志。时间保留为字符串（目标库本地时间），避免时区换算。
type Entry struct {
	Micro        int64 // 游标：UNIX_TIMESTAMP(start_time)*1000000 + MICROSECOND(start_time)
	StartTime    string
	UserHost     string
	QueryTimeSec float64
	LockTimeSec  float64
	RowsSent     int64
	RowsExamined int64
	DBName       string
	SQLText      string
	SQLMD5       string // 采集器填写
}

// Repo 是服务与采集器依赖的元数据访问（GORM 实现见 store.go，测试用内存假实现）。
type Repo interface {
	// 实例与权限
	ListInstances() ([]model.Instance, error)
	GetInstance(id uint) (*model.Instance, error)
	HasButton(userID uint, code string) (bool, error)
	UserInstanceIDs(userID uint) (map[uint]bool, error)
	InsertAudit(e *model.AuditEntry) error

	// dbt_slow_logs（平台采集）
	SlowLogGroups(q GroupQuery) ([]SlowLogGroup, int64, error)
	SlowLogDetail(q DetailQuery) ([]SlowLogRow, error)
	CollectedReview(q ReviewQuery) ([]ReviewRow, int64, error)
	CollectedHistory(q ReviewQuery) ([]HistoryRow, int64, error)
	CollectedTrend(q TrendQuery) (*Trend, error)
	CollectedDatabases(instanceID uint, from, to string) ([]string, error)

	// pt-query-digest 表
	DigestAvailable() (bool, error)
	// DigestHasHost 判断历史表里有没有该实例（host:port）的数据。表存在不代表有数据：基础表结构安装时就会建出这两张表，
	// 没部署 pt-query-digest 推送脚本的实例它们是空的，自动选数据源必须看这个，不能只看表在不在。
	DigestHasHost(hostKey string) (bool, error)
	DigestReview(q ReviewQuery) ([]ReviewRow, int64, error)
	DigestHistory(q ReviewQuery) ([]HistoryRow, int64, error)
	DigestTrend(q TrendQuery) (*Trend, error)
	DigestDatabases(hostKey, from, to string) ([]string, error)

	CollectRepo
}

// CollectRepo 是采集器需要的数据访问。
type CollectRepo interface {
	// CollectInstances 返回开启了采集开关（is_enable=1）且类型受支持的实例（含解密所需的字段）。
	CollectInstances() ([]model.Instance, error)
	// Watermark 返回该实例已采集到的最大游标（dbt_slow_logs.updated_at_micro），没有则 0。
	Watermark(instanceID uint) (int64, error)
	// ExistingKeys 返回游标区间内已存在记录的去重键（见 entryKey）。
	ExistingKeys(instanceID uint, minMicro, maxMicro int64) (map[string]bool, error)
	InsertSlowLogs(instanceID uint, rows []Entry) (int, error)
	// SaveState / LoadStates 读写最近一次采集状态；状态表没创建时 LoadStates 返回 ok=false。
	SaveState(st CollectState) error
	LoadStates() (states map[uint]CollectState, ok bool, err error)
}

// Source 从目标实例拉取慢日志。
type Source interface {
	// Fetch 返回游标大于 afterMicro 的记录，按游标升序，最多 limit 条。
	Fetch(ctx context.Context, inst *model.Instance, afterMicro int64, limit int) ([]Entry, error)
}

// ConnFunc 解密凭据、得到连接信息（对应 store.EngineConn）。
type ConnFunc func(inst *model.Instance) engine.Conn
