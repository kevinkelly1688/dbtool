// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

package perf

import (
	"time"
)

// ---- 枚举（取值直接存在数据库里） ----

// 级别。
const (
	SevP0 = "P0"
	SevP1 = "P1"
	SevP2 = "P2"
	SevP3 = "P3"
)

// Severities 按严重程度从高到低。
var Severities = []string{SevP0, SevP1, SevP2, SevP3}

var severityLabel = map[string]string{SevP0: "P0-紧急", SevP1: "P1-高", SevP2: "P2-中", SevP3: "P3-低"}

// severityRank 越小越严重。
var severityRank = map[string]int{SevP0: 0, SevP1: 1, SevP2: 2, SevP3: 3}

// 工单状态。
const (
	StNew          = "new"           // 待分派
	StAssigned     = "assigned"      // 待受理
	StAccepted     = "accepted"      // 处理中
	StFixed        = "fixed"         // 待验证
	StWaivePending = "waive_pending" // 豁免审批中
	StClosed       = "closed"        // 已关闭
)

var statusLabel = map[string]string{StNew: "待分派", StAssigned: "待受理", StAccepted: "处理中", StFixed: "待验证", StWaivePending: "豁免审批中", StClosed: "已关闭"}

// openStatuses 未关闭的状态。
var openStatuses = []string{StNew, StAssigned, StAccepted, StFixed, StWaivePending}

// slaRunning 计入 SLA 计时的状态（待验证/豁免审批中时研发侧已给出答复，暂停计时）。
var slaRunning = []string{StNew, StAssigned, StAccepted}

func isOpen(st string) bool       { return contains(openStatuses, st) }
func isSLARunning(st string) bool { return contains(slaRunning, st) }

// 关闭原因。
const (
	CloseVerified    = "verified"    // 优化完成(已验证)
	CloseWaived      = "waived"      // 豁免(接受现状)
	CloseDisappeared = "disappeared" // 长期未复现自动关闭
	CloseDuplicate   = "duplicate"   // 重复/误报
)

var closeLabel = map[string]string{CloseVerified: "优化完成(已验证)", CloseWaived: "豁免(接受现状)", CloseDisappeared: "长期未复现自动关闭", CloseDuplicate: "重复/误报"}

// 处理方。
const (
	SideDBA = "dba"
	SideDev = "dev"
)

var sideLabel = map[string]string{SideDBA: "DBA处理", SideDev: "研发处理"}

// 问题类型。
const (
	CatMissingIndex  = "missing_index"
	CatIndexInvalid  = "index_invalid"
	CatDeepPaging    = "deep_paging"
	CatFullScanBiz   = "full_scan_biz"
	CatLargeResult   = "large_result"
	CatSortGroup     = "sort_group"
	CatSubqueryJoin  = "subquery_join"
	CatHighFrequency = "high_frequency"
	CatOLAPHeavy     = "olap_heavy"
	CatOther         = "other"
)

// Categories 是问题类型及显示名，按展示顺序。
var Categories = []Option{
	{CatMissingIndex, "缺少索引"}, {CatIndexInvalid, "索引失效(函数/隐式转换/前模糊)"}, {CatDeepPaging, "深分页"},
	{CatFullScanBiz, "无条件/大范围扫描"}, {CatLargeResult, "返回结果集过大"}, {CatSortGroup, "排序/分组/临时表"},
	{CatSubqueryJoin, "子查询/关联写法"}, {CatHighFrequency, "调用频率过高"}, {CatOLAPHeavy, "分析型大查询"}, {CatOther, "其他"},
}

// 产品类型。
const (
	ProdMySQL    = "mysql"
	ProdAliyun   = "aliyun_rds"
	ProdPolarDB  = "polardb"
	ProdAWS      = "aws_rds"
	ProdPolarDBX = "polardbx"
	ProdSelectDB = "selectdb"
	ProdADB      = "adb"
	ProdOther    = "other"
)

// Products 是产品类型及显示名。
var Products = []Option{
	{ProdMySQL, "自建MySQL"}, {ProdAliyun, "阿里云RDS MySQL"}, {ProdPolarDB, "PolarDB MySQL"}, {ProdAWS, "AWS RDS/Aurora MySQL"},
	{ProdPolarDBX, "PolarDB-X"}, {ProdSelectDB, "SelectDB/Doris"}, {ProdADB, "AnalyticDB MySQL"}, {ProdOther, "其他"},
}

// 采集方式。
const (
	CollPlatformSlow = "platform_slowlogs"
	CollPtDigest     = "pt_digest"
	CollPerfSchema   = "perf_schema"
	CollSlowLogTable = "slow_log_table"
	CollDorisAudit   = "doris_audit"
	CollPolarDBXSum  = "polardbx_summary"
	CollPolarDBXSlow = "polardbx_show_slow"
	CollCustomSQL    = "custom_sql"
)

// Collectors 是采集方式及显示名。
var Collectors = []Option{
	{CollPlatformSlow, "平台慢日志"}, {CollPtDigest, "pt-query-digest"},
	{CollPerfSchema, "performance_schema digest"}, {CollSlowLogTable, "mysql.slow_log 表"},
	{CollDorisAudit, "Doris/SelectDB audit_log"}, {CollPolarDBXSum, "PolarDB-X statements_summary_history"},
	{CollPolarDBXSlow, "PolarDB-X SHOW FULL SLOW"}, {CollCustomSQL, "自定义采集SQL"},
}

// dbTypeDefaults 是实例类型 -> (产品类型, 默认采集方式)。
var dbTypeDefaults = map[string][2]string{
	"mysql":     {ProdMySQL, CollPlatformSlow},
	"polardb":   {ProdPolarDB, CollPlatformSlow},
	"polardb-x": {ProdPolarDBX, CollPlatformSlow},
	"selectdb":  {ProdSelectDB, CollPlatformSlow},
	"doris":     {ProdSelectDB, CollPlatformSlow},
	"adb":       {ProdADB, CollCustomSQL},
}

// SupportedDBTypes 是性能工单支持的实例类型。
var SupportedDBTypes = []string{"mysql", "polardb", "polardb-x", "selectdb", "doris", "adb"}

// Option 是下拉选项。
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

func labelOf(list []Option, v string) string {
	for _, o := range list {
		if o.Value == v {
			return o.Label
		}
	}
	return v
}

// 工单日志的动作名。
const (
	ActCreate        = "create"
	ActAssign        = "assign"
	ActAccept        = "accept"
	ActFix           = "fix"
	ActComment       = "comment"
	ActExtend        = "extend"
	ActExtendApprove = "extend_approve"
	ActExtendReject  = "extend_reject"
	ActWaive         = "waive"
	ActWaiveApprove  = "waive_approve"
	ActWaiveReject   = "waive_reject"
	ActVerifyPass    = "verify_pass"
	ActVerifyFail    = "verify_fail"
	ActClose         = "close"
	ActCloseDup      = "close_dup"
	ActReopen        = "reopen"
	ActSeverity      = "severity"
	ActSide          = "side"
	ActEscalate      = "escalate"
	ActDiagnose      = "diagnose"
)

// actionName 是日志里动作的显示名。
var actionName = map[string]string{
	ActCreate: "建单", ActAssign: "指派", ActAccept: "受理", ActFix: "标记修复", ActComment: "评论", ActExtend: "申请延期",
	ActExtendApprove: "同意延期", ActExtendReject: "驳回延期", ActWaive: "申请豁免", ActWaiveApprove: "同意豁免", ActWaiveReject: "驳回豁免",
	ActVerifyFail: "验证不通过", ActClose: "关闭", ActReopen: "重开", ActSeverity: "级别变更", ActSide: "切换处理方", ActEscalate: "SLA升级", ActDiagnose: "重新诊断",
	ActVerifyPass: "验证通过", ActCloseDup: "误报关闭", // 工单日志里这两个仍记为 close；这里供系统审计区分动作
}

// 按钮权限码（登记表 internal/btnperm）。
const (
	btnView      = "perf.view"       // 查看工单与排行榜：打开问题工单 / 团队榜单页面
	btnCreate    = "perf.create"     // 手工建单
	btnCfgView   = "perfcfg.view"    // 打开采集 / 团队 / SLA 配置页面
	permQueryAll = "query.admin_run" // 可选择全部实例建单
)

// managerButtons 是 DBA 侧的管理类按钮（建单 / 指派 / 验证 / 审批 / 调整 / 重开 / 误报关闭，以及配置类）：
// 持有其中任一个的人是“性能工单管理者”（Viewer.Manage）——能看到全部工单、能打开工单页面；具体能做哪件事仍由各自的按钮决定。
var managerButtons = []string{"perf.create", "perf.assign", "perf.verify", "perf.approve", "perf.adjust", "perf.reopen", "perf.close_dup",
	"perfcfg.view", "perfcfg.collect", "perfcfg.team", "perfcfg.sla"}

// ---- 数据模型：基础表（不改结构） ----

// Issue -> dbt_perf_issue。时间字段在库里是本地时间。
type Issue struct {
	ID         uint   `gorm:"column:id;primaryKey" json:"id"`
	InstanceID uint   `gorm:"column:instance_id" json:"instance_id"`
	DBName     string `gorm:"column:db_name" json:"db_name"`
	Checksum   string `gorm:"column:checksum" json:"checksum"`
	// 去重键：(instance_id, db_name, checksum) 在库里有唯一索引。
	Fingerprint string `gorm:"column:fingerprint" json:"fingerprint"`
	SampleSQL   string `gorm:"column:sample_sql" json:"sample_sql"`
	Tables      string `gorm:"column:tables" json:"tables"`
	Source      string `gorm:"column:source" json:"source"` // collector / manual / slowlog

	FirstSeen       *time.Time `gorm:"column:first_seen" json:"first_seen"`
	LastSeen        *time.Time `gorm:"column:last_seen" json:"last_seen"`
	ExecCount7d     int64      `gorm:"column:exec_count_7d" json:"exec_count_7d"`
	TotalTime7d     float64    `gorm:"column:total_time_7d" json:"total_time_7d"`
	AvgTimeMs       float64    `gorm:"column:avg_time_ms" json:"avg_time_ms"`
	MaxTimeMs       float64    `gorm:"column:max_time_ms" json:"max_time_ms"`
	RowsExaminedAvg float64    `gorm:"column:rows_examined_avg" json:"rows_examined_avg"`
	RowsSentAvg     float64    `gorm:"column:rows_sent_avg" json:"rows_sent_avg"`
	LoadShare       float64    `gorm:"column:load_share" json:"load_share"`
	ImpactScore     float64    `gorm:"column:impact_score" json:"impact_score"` // 日均浪费DB时间(s)
	PerDayCount     float64    `gorm:"column:per_day_count" json:"per_day_count"`

	Category      string `gorm:"column:category" json:"category"`
	OwnerSide     string `gorm:"column:owner_side" json:"owner_side"`
	Diagnosis     string `gorm:"column:diagnosis" json:"diagnosis"`
	Suggestion    string `gorm:"column:suggestion" json:"suggestion"`
	SuggestedDDL  string `gorm:"column:suggested_ddl" json:"suggested_ddl"`
	ExplainResult string `gorm:"column:explain_result" json:"-"`

	Severity       string `gorm:"column:severity" json:"severity"`
	SeverityLocked bool   `gorm:"column:severity_locked" json:"severity_locked"`
	Status         string `gorm:"column:status" json:"status"`
	CloseReason    string `gorm:"column:close_reason" json:"close_reason"`
	TeamID         *uint  `gorm:"column:team_id" json:"team_id"`
	AssigneeID     *uint  `gorm:"column:assignee_id" json:"assignee_id"`
	DBAID          *uint  `gorm:"column:dba_id" json:"dba_id"`

	PlanFinishDate   *time.Time `gorm:"column:plan_finish_date" json:"plan_finish_date"` // DATE
	FixDesc          string     `gorm:"column:fix_desc" json:"fix_desc"`
	FixedAt          *time.Time `gorm:"column:fixed_at" json:"fixed_at"`
	AcceptedAt       *time.Time `gorm:"column:accepted_at" json:"accepted_at"`
	ClosedAt         *time.Time `gorm:"column:closed_at" json:"closed_at"`
	ReopenCount      int        `gorm:"column:reopen_count" json:"reopen_count"`
	BaselineAvgMs    float64    `gorm:"column:baseline_avg_ms" json:"baseline_avg_ms"`
	BaselineDailySec float64    `gorm:"column:baseline_daily_sec" json:"baseline_daily_sec"`

	PendingExtendDate *time.Time `gorm:"column:pending_extend_date" json:"pending_extend_date"` // DATE
	PendingReason     string     `gorm:"column:pending_reason" json:"pending_reason"`
	ExtendTimes       int        `gorm:"column:extend_times" json:"extend_times"`

	AcceptDeadline  *time.Time `gorm:"column:accept_deadline" json:"accept_deadline"`
	ResolveDeadline *time.Time `gorm:"column:resolve_deadline" json:"resolve_deadline"`
	EscalationLevel int        `gorm:"column:escalation_level" json:"escalation_level"` // 0正常 1催办 2TL 3总监
	LastRemindAt    *time.Time `gorm:"column:last_remind_at" json:"last_remind_at"`
	RemindCount     int        `gorm:"column:remind_count" json:"remind_count"`
	SLABreached     bool       `gorm:"column:sla_breached" json:"sla_breached"`
	PreWarned       bool       `gorm:"column:pre_warned" json:"pre_warned"`

	CreateTime time.Time `gorm:"column:create_time" json:"create_time"`
	// UpdateTime 同时是乐观锁版本：条件更新时要求库里的值等于读出来的值。
	UpdateTime time.Time `gorm:"column:update_time" json:"update_time"`
}

func (Issue) TableName() string { return "dbt_perf_issue" }

// IssueLog -> dbt_perf_issue_log
type IssueLog struct {
	ID              uint      `gorm:"column:id;primaryKey" json:"id"`
	IssueID         uint      `gorm:"column:issue_id" json:"issue_id"`
	Action          string    `gorm:"column:action" json:"action"`
	Content         string    `gorm:"column:content" json:"content"`
	Operator        string    `gorm:"column:operator" json:"operator"`
	OperatorDisplay string    `gorm:"column:operator_display" json:"operator_display"`
	CreateTime      time.Time `gorm:"column:create_time" json:"create_time"`
}

func (IssueLog) TableName() string { return "dbt_perf_issue_log" }

// IssueStat -> dbt_perf_issue_stat（按天聚合的指标；(issue_id, stat_date) 唯一）
type IssueStat struct {
	ID           uint      `gorm:"column:id;primaryKey"`
	IssueID      uint      `gorm:"column:issue_id"`
	StatDate     time.Time `gorm:"column:stat_date"` // DATE
	ExecCount    int64     `gorm:"column:exec_count"`
	TotalTime    float64   `gorm:"column:total_time"`
	MaxTimeMs    float64   `gorm:"column:max_time_ms"`
	RowsExamined float64   `gorm:"column:rows_examined"`
	RowsSent     float64   `gorm:"column:rows_sent"`
}

func (IssueStat) TableName() string { return "dbt_perf_issue_stat" }

// Team -> dbt_perf_team（TL、总监在多对多表 dbt_perf_team_leaders / dbt_perf_team_directors 里）
type Team struct {
	ID                uint   `gorm:"column:id;primaryKey" json:"id"`
	ResourceGroupID   uint   `gorm:"column:resource_group_id" json:"resource_group_id"`
	DefaultAssigneeID *uint  `gorm:"column:default_assignee_id" json:"default_assignee_id"`
	Enabled           bool   `gorm:"column:enabled" json:"enabled"` // 参与排行与周报
	Remark            string `gorm:"column:remark" json:"remark"`
}

func (Team) TableName() string { return "dbt_perf_team" }

// TeamConfig 是团队配置及其成员。
type TeamConfig struct {
	Team
	LeaderIDs   []uint `json:"leader_ids"`
	DirectorIDs []uint `json:"director_ids"`
}

// Route -> dbt_perf_route：库 -> 团队/负责人 路由规则（优先级高于 dbt_instance_database.owner）
type Route struct {
	ID         uint   `gorm:"column:id;primaryKey" json:"id"`
	InstanceID *uint  `gorm:"column:instance_id" json:"instance_id"`
	DBRegex    string `gorm:"column:db_regex" json:"db_regex"`
	TeamID     uint   `gorm:"column:team_id" json:"team_id"`
	AssigneeID *uint  `gorm:"column:assignee_id" json:"assignee_id"`
	Priority   int    `gorm:"column:priority" json:"priority"`
}

func (Route) TableName() string { return "dbt_perf_route" }

// SLAPolicy -> dbt_perf_sla_policy（每个级别一行）
type SLAPolicy struct {
	ID                  uint   `gorm:"column:id;primaryKey" json:"id"`
	Severity            string `gorm:"column:severity" json:"severity"`
	AcceptHours         int    `gorm:"column:accept_hours" json:"accept_hours"`
	ResolveHours        int    `gorm:"column:resolve_hours" json:"resolve_hours"`
	L3AfterHours        int    `gorm:"column:l3_after_hours" json:"l3_after_hours"`
	RemindIntervalHours int    `gorm:"column:remind_interval_hours" json:"remind_interval_hours"`
	MaxExtendTimes      int    `gorm:"column:max_extend_times" json:"max_extend_times"`
}

func (SLAPolicy) TableName() string { return "dbt_perf_sla_policy" }

// defaultSLA 是没有配置时的默认策略。
var defaultSLA = []SLAPolicy{
	{Severity: SevP0, AcceptHours: 4, ResolveHours: 48, L3AfterHours: 24, RemindIntervalHours: 4, MaxExtendTimes: 0},
	{Severity: SevP1, AcceptHours: 24, ResolveHours: 7 * 24, L3AfterHours: 72, RemindIntervalHours: 24, MaxExtendTimes: 1},
	{Severity: SevP2, AcceptHours: 72, ResolveHours: 14 * 24, L3AfterHours: 7 * 24, RemindIntervalHours: 48, MaxExtendTimes: 1},
	{Severity: SevP3, AcceptHours: 72, ResolveHours: 30 * 24, L3AfterHours: 14 * 24, RemindIntervalHours: 72, MaxExtendTimes: 2},
}

// CollectConfig -> dbt_perf_collect_config
type CollectConfig struct {
	ID              uint       `gorm:"column:id;primaryKey" json:"id"`
	InstanceID      uint       `gorm:"column:instance_id" json:"instance_id"`
	Product         string     `gorm:"column:product" json:"product"`
	Collector       string     `gorm:"column:collector" json:"collector"`
	CustomSQL       string     `gorm:"column:custom_sql" json:"custom_sql"`
	MinAvgMs        int        `gorm:"column:min_avg_ms" json:"min_avg_ms"`
	MinTotalSec     int        `gorm:"column:min_total_sec" json:"min_total_sec"`
	ExcludeRegex    string     `gorm:"column:exclude_regex" json:"exclude_regex"`
	ExcludeDBs      string     `gorm:"column:exclude_dbs" json:"exclude_dbs"`
	TeamID          *uint      `gorm:"column:team_id" json:"team_id"`
	Enabled         bool       `gorm:"column:enabled" json:"enabled"`
	LastCollectTime *time.Time `gorm:"column:last_collect_time" json:"last_collect_time"`
	LastStatus      string     `gorm:"column:last_status" json:"last_status"`
}

func (CollectConfig) TableName() string { return "dbt_perf_collect_config" }

const (
	defaultExcludeRegex = `(?i)^\s*(show|set|use|commit|rollback|begin|start|xa|flush|analyze|optimize|kill)\b`
	defaultExcludeDBs   = "mysql,information_schema,performance_schema,sys,__internal_schema"
)

// DigestCursor -> dbt_perf_digest_cursor：performance_schema 累计值游标；也用来记 dbt_slow_logs 的自增 id 游标。
type DigestCursor struct {
	ID              uint      `gorm:"column:id;primaryKey"`
	InstanceID      uint      `gorm:"column:instance_id"`
	SchemaName      string    `gorm:"column:schema_name"`
	Digest          string    `gorm:"column:digest"`
	CountStar       int64     `gorm:"column:count_star"`
	SumTimerWait    uint64    `gorm:"column:sum_timer_wait"`
	SumRowsExamined int64     `gorm:"column:sum_rows_examined"`
	SumRowsSent     int64     `gorm:"column:sum_rows_sent"`
	UpdateTime      time.Time `gorm:"column:update_time"`
}

func (DigestCursor) TableName() string { return "dbt_perf_digest_cursor" }

// JobRun -> dbt_perf_job_run（新表）：后台任务的运行记录；(job, run_key) 唯一，用来保证按天/按周的任务只执行一次。
type JobRun struct {
	ID         uint       `gorm:"column:id;primaryKey"`
	Job        string     `gorm:"column:job"`
	RunKey     string     `gorm:"column:run_key"`
	StartedAt  time.Time  `gorm:"column:started_at"`
	FinishedAt *time.Time `gorm:"column:finished_at"`
	Result     string     `gorm:"column:result"`
}

func (JobRun) TableName() string { return "dbt_perf_job_run" }

// ---- 对外（JSON）结构 ----

// UserBrief 是用户的简要信息。
type UserBrief struct {
	ID       uint   `json:"id"`
	Username string `json:"username"`
	Display  string `json:"display"`
	Active   bool   `json:"-"`
	DingID   string `json:"-"`
	FeishuID string `json:"-"`
	SlackID  string `json:"-"`
}

// Name 返回显示名（没有则用户名）。
func (u UserBrief) Name() string {
	if u.Display != "" {
		return u.Display
	}
	return u.Username
}

// GroupBrief 是资源组（团队）的简要信息。
type GroupBrief struct {
	ID       uint     `json:"id"`
	Name     string   `json:"name"`
	Webhooks []string `json:"webhooks,omitempty"` // 资源组上配置了哪些群机器人：钉钉/飞书/Slack
}

// InstanceBrief 是实例的简要信息。
type InstanceBrief struct {
	ID     uint   `json:"id"`
	Name   string `json:"name"`
	DBType string `json:"db_type"`
}

// IssueRow 是工单列表/详情用的行：工单加上关联名称。
type IssueRow struct {
	Issue
	InstanceName string `gorm:"column:instance_name"`
	TeamName     string `gorm:"column:team_name"`
	AssigneeName string `gorm:"column:assignee_name"`
	DBAName      string `gorm:"column:dba_name"`
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }

func derefUint(p *uint) uint {
	if p == nil {
		return 0
	}
	return *p
}
