// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// 报告所在的表：MySQL 系（dbt_sql_mysql_inspection_report）与 SQL Server（dbt_sql_mssql_inspection_report）。
const (
	KindMySQL = "mysql"
	KindMSSQL = "mssql"
	// KindRedis 是 Redis 报告：存在新表 dbt_inspect_redis_report（deploy/sql/131_redis_inspect.sql），结构与 MySQL 报告一致（评分 + result JSON）。
	KindRedis = "redis"
)

// 报告状态。
const (
	StatusRunning   = "running"
	StatusFinish    = "finish"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// 触发方式。
const (
	TriggerManual   = "manual"
	TriggerSchedule = "schedule"
)

// 检查项结论级别。
const (
	LevelNormal    = 0
	LevelAttention = 1
	LevelAlert     = 2
	LevelFailed    = 3 // 采集失败 / 无权限 / 未执行，不计入告警
)

// 按钮权限码（登记表 internal/btnperm）。
const (
	btnView       = "inspect.view"        // 打开页面、查看报告与定时计划
	btnPlanEdit   = "inspect.plan_edit"   // 新建 / 编辑定时计划
	btnPlanToggle = "inspect.plan_toggle" // 暂停 / 启用 / 删除定时计划
	btnPlanRun    = "inspect.plan_run"    // 立即运行定时计划
)

// mysqlFamily 是走 MySQL 协议、用 MySQL 巡检的实例类型。
var mysqlFamily = map[string]bool{"mysql": true, "polardb": true, "polardb-x": true, "polardbx": true, "selectdb": true, "doris": true, "adb": true}

// KindOf 返回实例类型对应的报告种类；不支持巡检的类型 ok=false。
func KindOf(dbType string) (string, bool) {
	dbType = strings.ToLower(strings.TrimSpace(dbType))
	switch {
	case dbType == "mssql":
		return KindMSSQL, true
	case mysqlFamily[dbType]:
		return KindMySQL, true
	}
	return "", false
}

// KindLabel 是报告种类的显示名。
func KindLabel(dbType string) string {
	switch strings.ToLower(dbType) {
	case "mssql":
		return "SQL Server"
	case "mysql":
		return "MySQL"
	case "polardb":
		return "PolarDB"
	case "polardb-x", "polardbx":
		return "PolarDB-X"
	case "selectdb":
		return "SelectDB"
	case "doris":
		return "Doris"
	case "adb":
		return "ADB"
	case "redis":
		return "Redis"
	}
	return dbType
}

// ReportRef 定位一份报告。两张表各自自增，所以 ID 单独不唯一；对外用 “mysql-410” / “mssql-411”。
type ReportRef struct {
	Kind string
	ID   uint
}

func (r ReportRef) String() string { return r.Kind + "-" + strconv.FormatUint(uint64(r.ID), 10) }

// ParseRef 解析 “mysql-410” 形式的报告标识。
func ParseRef(s string) (ReportRef, error) {
	kind, id, ok := strings.Cut(strings.TrimSpace(s), "-")
	n, err := strconv.ParseUint(id, 10, 32)
	if !ok || (kind != KindMySQL && kind != KindMSSQL && kind != KindRedis) || err != nil || n == 0 {
		return ReportRef{}, fmt.Errorf("报告标识 %q 不合法", s)
	}
	return ReportRef{Kind: kind, ID: uint(n)}, nil
}

// ---- 对目标库的只读访问 ----

// Result 是一次查询的结果集。
type Result struct {
	Cols []string
	Rows [][]any
}

// Row 是以（小写）列名为键的一行。
type Row map[string]any

// Maps 把结果集转成以小写列名为键的行。
func (r *Result) Maps() []Row {
	if r == nil {
		return nil
	}
	out := make([]Row, 0, len(r.Rows))
	for _, vals := range r.Rows {
		row := make(Row, len(r.Cols))
		for i, c := range r.Cols {
			if i < len(vals) {
				row[strings.ToLower(c)] = vals[i]
			}
		}
		out = append(out, row)
	}
	return out
}

// First 返回第一行；没有行返回空 Row（取值都是零值）。
func (r *Result) First() Row {
	if m := r.Maps(); len(m) > 0 {
		return m[0]
	}
	return Row{}
}

func (r Row) Str(k string) string    { return toStr(r[k]) }
func (r Row) Int(k string) int64     { return toInt(r[k]) }
func (r Row) Float(k string) float64 { return toFloat(r[k]) }

// Target 是被巡检的实例：只读查询。实现负责超时与连接数限制（database/sql，MaxOpenConns=2）。
// db 非空时先切换到该库（SQL Server 的每库检查项用）。
type Target interface {
	Query(ctx context.Context, db, query string, args ...any) (*Result, error)
	Close() error
}

// Opener 打开到实例的连接；单测里用假实现，不连任何真实数据库。
type Opener func(ctx context.Context, dbType string, c engine.Conn) (Target, error)

// ---- 仓库 ----

// ReportFilter 是报告列表的筛选条件。
type ReportFilter struct {
	InstanceID  uint
	Level       string // alert / attention / clean / failed / cancelled
	Trigger     string // manual / schedule
	Search      string // 实例名模糊匹配
	Start       *time.Time
	End         *time.Time // 开区间
	Running     bool       // 只看进行中的
	Latest      bool       // 每个实例只取最近一次结束（成功或失败）的报告，不含进行中和已取消；级别等其它条件在这之后再筛
	PlanID      uint       // 只看某个计划产生的报告
	RunID       uint       // 只看某次计划运行产生的报告
	AllInst     bool       // 不限制实例范围
	InstanceIDs []uint     // AllInst 为 false 时可见的实例
	Offset      int
	Limit       int
}

// ReportSummary 是列表里的一行（两种报告合并）。
type ReportSummary struct {
	Kind         string     `gorm:"column:kind" json:"kind"`
	ID           uint       `gorm:"column:id" json:"id"`
	InstanceID   uint       `gorm:"column:instance_id" json:"instance_id"`
	InstanceName string     `gorm:"column:instance_name" json:"instance_name"`
	DBType       string     `gorm:"column:db_type" json:"db_type"`
	Status       string     `gorm:"column:status" json:"status"`
	Trigger      string     `gorm:"column:trigger_type" json:"trigger"`
	Operator     string     `gorm:"column:operator" json:"operator"`
	PlanID       uint       `gorm:"column:plan_id" json:"plan_id"`
	PlanName     string     `gorm:"column:plan_name" json:"plan_name"`
	HealthScore  *int       `gorm:"column:health_score" json:"health_score"`
	Alert        int        `gorm:"column:alert_count" json:"alert"`
	Attention    int        `gorm:"column:attention_count" json:"attention"`
	Normal       int        `gorm:"column:normal_count" json:"normal"`
	Failed       int        `gorm:"column:failed_count" json:"failed"`
	Total        int        `gorm:"column:total_items" json:"total"`
	Done         int        `gorm:"column:done_items" json:"done"`
	StartTime    time.Time  `gorm:"column:start_time" json:"start_time"`
	FinishTime   *time.Time `gorm:"column:finish_time" json:"finish_time"`
	Error        string     `gorm:"column:error_message" json:"error"`
}

// ReportDetail 是一份完整的报告：MySQL 的 Result，或 SQL Server 的 Items 与 Summary（都是 JSON）。
type ReportDetail struct {
	ReportSummary
	Result      string // MySQL：result 列的 JSON
	Summary     string // SQL Server：summary 列的 JSON
	ServerStart *time.Time
	Items       []MSSQLItem
}

// MSSQLItem 对应 dbt_sql_mssql_inspection_item 的一行。
type MSSQLItem struct {
	Section    string
	Key        string
	Name       string
	DB         string
	Level      int
	Value      string
	Threshold  string
	Detail     string // JSON：{"columns":[...],"rows":[[...]]}；空表示没有
	RawCounter string // JSON；空表示没有
	ElapsedMs  int
}

// Finish 是报告收尾时写回的内容。
type Finish struct {
	Status      string
	HealthScore int
	Alert       int
	Attention   int
	Normal      int
	Failed      int
	Total       int
	Done        int
	Result      string // MySQL：result JSON
	Summary     string // SQL Server：summary JSON
	ServerStart *time.Time
	Error       string
	At          time.Time
}

// PrevRaw 是上一份成功的 SQL Server 报告里的累计型计数器原始值（只有实例没重启过才能做差）。
type PrevRaw struct {
	ReportID    uint
	StartTime   time.Time
	ServerStart *time.Time
	Raw         map[string]string // "item_key|db" -> JSON
}

// Repo 是本模块依赖的数据访问；store.go 是 GORM 实现，测试里用内存假实现。
type Repo interface {
	// 实例
	ListInstances() ([]model.Instance, error) // 含标签
	GetInstance(id uint) (*model.Instance, error)
	InstanceIDsByTags(codes []string) ([]uint, error)
	Conn(inst *model.Instance) engine.Conn

	// 报告
	// CreateReport 创建一份 running 的报告；该实例已有进行中的报告时 busy=true。
	// 超过 staleAfter 仍在 running 的报告视为进程中断，先收尾为失败。
	CreateReport(kind string, instanceID uint, trigger, operator string, total int, now time.Time, staleAfter time.Duration) (id uint, busy bool, err error)
	ReportStatus(kind string, id uint) (string, error)
	UpdateProgress(kind string, id uint, done, total int) error
	InsertMSSQLItems(reportID uint, items []MSSQLItem) error
	// FinishReport 只在报告仍是 running 时生效（已被取消的不覆盖），返回是否生效。
	FinishReport(kind string, id uint, f Finish) (bool, error)
	CancelReports(refs []ReportRef, by string, now time.Time) ([]ReportRef, error)
	DeleteReports(refs []ReportRef) (deleted []ReportRef, running []ReportRef, err error)
	ListReports(f ReportFilter) ([]ReportSummary, int64, error)
	GetReport(kind string, id uint) (*ReportDetail, error)
	ReportBrief(kind string, id uint) (*ReportSummary, error) // 只取列表里的摘要字段；不存在返回 nil, nil
	// SlowTop 取平台采集的慢日志统计（dbt_slow_logs 表）；没有数据或表不存在返回 ok=false。
	SlowTop(instanceID uint, since time.Time, limit int) (map[string]any, bool)
	PrevMSSQLRaw(instanceID, excludeID uint) (*PrevRaw, error)
	// 过期清理：计划产生的报告 / 不属于任何计划的报告
	PlanReportRefsBefore(planID uint, cutoff time.Time, limit int) ([]ReportRef, error)
	UnlinkedReportRefsBefore(kind string, cutoff time.Time, limit int) ([]ReportRef, error)

	// 定时计划
	PlanTablesReady() bool
	ListPlans() ([]Plan, error)
	GetPlan(id uint) (*Plan, error)
	PlanByName(name string) (*Plan, error)
	SavePlan(p *Plan) (uint, error) // ID 为 0 表示新建；同时覆盖 InstanceIDs
	DeletePlan(id uint) error
	SetPlanEnabled(id uint, enabled bool, next *time.Time) error
	DuePlans(now time.Time, limit int) ([]Plan, error)
	// ClaimPlan 原子占位：只有 next_run_at 仍等于 expected 的那个进程能领取，并把它改成 next。
	ClaimPlan(id uint, expected time.Time, next *time.Time, now time.Time) (bool, error)
	MarkPlanRun(id uint, runID uint, at time.Time) error
	CreateRun(r *Run) (uint, error)
	UpdateRun(r *Run) error
	TouchRun(id uint, now time.Time) error
	GetRun(id uint) (*Run, error)
	ListRuns(planID uint, limit int) ([]Run, error)
	ActiveRun(planID uint, freshAfter time.Time) (*Run, error)
	// FailStaleRuns 把心跳早于 before 仍是 running 的运行记录收尾为失败，返回数量。
	FailStaleRuns(before time.Time, now time.Time) (int, error)
	AddRunItem(it *RunItem) error
	RunItems(runID uint) ([]RunItem, error)
	DeleteRunsBefore(planID uint, cutoff time.Time) error

	// 系统配置（只读）、审计
	ConfigValue(item string) (string, bool)
	InsertAudit(e *model.AuditEntry) error
}

// ---- 值转换 ----

func toStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case time.Time:
		return x.Format("2006-01-02 15:04:05")
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	}
	return fmt.Sprint(v)
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case nil:
		return 0
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case uint:
		return int64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return int64(x)
	case float32:
		return int64(x)
	case float64:
		return int64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	case []byte:
		return toInt(string(x))
	case string:
		s := strings.TrimSpace(x)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(f)
		}
	}
	return 0
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case []byte:
		return toFloat(string(x))
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	case nil:
		return 0
	}
	return float64(toInt(v))
}

// toTime 解析时间值；无法解析返回零值。
func toTime(v any) time.Time {
	switch x := v.(type) {
	case time.Time:
		return x
	case []byte:
		return toTime(string(x))
	case string:
		for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", time.RFC3339Nano} {
			if t, err := time.ParseInLocation(layout, strings.TrimSpace(x), time.Local); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// clip 按字符数截断（不加省略号），用于写入有长度限制的列：超长的值在严格模式下会让整条 INSERT 失败。
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
