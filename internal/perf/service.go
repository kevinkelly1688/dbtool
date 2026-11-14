// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package perf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/query"
)

// 仓库返回的哨兵错误。
var (
	ErrDuplicate = errors.New("perf: 唯一键冲突")  // 同一 (实例, 库, 指纹) 已有工单
	ErrConflict  = errors.New("perf: 并发修改冲突") // 乐观锁失败：工单在读出之后被别人改过
)

func newErr(status int, code, msg string) error { return query.NewError(status, code, msg) }
func bad(msg string) error                      { return newErr(400, "bad_request", msg) }
func forbid(msg string) error                   { return newErr(403, "forbidden", msg) }
func notFound(msg string) error                 { return newErr(404, "not_found", msg) }
func conflict(msg string) error                 { return newErr(409, "conflict", msg) }

// IssueQuery 是工单列表的查询条件。View 决定“我的待办 / 全部未关闭 / 已超时 / 待验证 / 已关闭 / 全部”。
type IssueQuery struct {
	View       string
	Severity   string
	OwnerSide  string
	Category   string
	TeamID     uint
	InstanceID uint
	Search     string
	Sort       string
	Desc       bool
	Offset     int
	Limit      int
	Status     string // 精确状态筛选（与 View 叠加）
	Source     string // 来源筛选：collector / manual / slowlog

	// 可见范围：Restrict 为 true 时只返回 TeamIDs 里的团队的工单，或负责人是 UserID 的工单。
	Restrict bool
	TeamIDs  []uint
	// UserID 在 View=todo 时也是“我”。
	UserID uint
	Now    time.Time
}

// 列表视图。
const (
	ViewTodo    = "todo"
	ViewOpen    = "open"
	ViewOverdue = "overdue"
	ViewFixed   = "fixed"
	ViewClosed  = "closed"
	ViewAll     = "all"

	// ViewTodoOverdue 是“我的待办”里已超时的部分（SLA 计时中且已过解决截止），只给顶栏角标用。
	ViewTodoOverdue = "todo_overdue"

	// 按状态拆分的页签：待分派 / 待受理 / 处理中 / 审批中（豁免审批中，或有待审批的延期申请）。
	ViewNew      = "new"
	ViewAssigned = "assigned"
	ViewAccepted = "accepted"
	ViewApprove  = "approve"
)

// 允许排序的字段。
var sortable = map[string]bool{"id": true, "severity": true, "impact_score": true, "avg_time_ms": true, "per_day_count": true,
	"resolve_deadline": true, "create_time": true, "last_seen": true, "load_share": true}

// StatDelta 是要累加到某天的指标。
type StatDelta struct {
	ExecCount    int64
	TotalTime    float64
	MaxTimeMs    float64
	RowsExamined float64
	RowsSent     float64
}

// StatSum 是一段时间内指标的合计。
type StatSum struct {
	ExecCount    int64
	TotalTime    float64
	MaxTimeMs    float64
	RowsExamined float64
	RowsSent     float64
}

// UserLoad 是 DBA 手上未关闭的工单数（DBA 轮值用）。
type UserLoad struct {
	User UserBrief
	Open int
}

// Repo 是性能工单依赖的数据访问（store.go 是 GORM 实现，测试里用内存假实现）。
type Repo interface {
	// ---- 工单 ----
	GetIssue(id uint) (*Issue, error)                               // 不存在返回 nil, nil
	GetIssueRow(id uint) (*IssueRow, error)                         // 带关联名称
	FindIssue(instanceID uint, db, checksum string) (*Issue, error) // 不存在返回 nil, nil
	InsertIssue(i *Issue) error                                     // 唯一键冲突返回 ErrDuplicate
	// UpdateIssue 保存工单的全部可变字段：仅当库里的 update_time 仍等于 prev 时成功，否则返回 ErrConflict。成功后 i.UpdateTime 为新值。
	UpdateIssue(i *Issue, prev time.Time) error
	// UpdateIssueMetrics 只写影响指标、最近出现时间和样例（采集刷新用，列与状态流转互不重叠，不检查也不改版本）。
	UpdateIssueMetrics(i *Issue) error
	ListIssues(q IssueQuery) ([]IssueRow, int, error)
	IssuesByStatus(statuses []string) ([]Issue, error)
	// AbsentIssues 返回状态在 statuses 内、来源为 source 且最近出现早于 before 的工单。
	AbsentIssues(statuses []string, source string, before time.Time) ([]Issue, error)
	RefreshLoadShare(instanceID uint) error

	// ---- 日志与日指标 ----
	AddLog(l *IssueLog) error
	ListLogs(issueID uint, limit int) ([]IssueLog, error) // 新的在前
	AddStat(issueID uint, day time.Time, d StatDelta) error
	ListStats(issueID uint, since time.Time) ([]IssueStat, error)   // 按日期升序
	SumStats(issueID uint, since, until time.Time) (StatSum, error) // [since, until)；until 为零值表示不限

	// ---- 排行榜（SQL 聚合） ----
	TeamAggregates(q BoardQuery) ([]TeamAgg, error)
	OverallAggregate(q BoardQuery) (OverallAgg, error)
	WeeklyTrend(since time.Time) ([]WeekCount, error)
	CategoryWaste() ([]CategoryAgg, error)

	// ---- 配置：团队、路由、SLA、采集 ----
	ListTeams() ([]TeamConfig, error)
	GetTeamByGroup(groupID uint) (*TeamConfig, error)
	GetTeam(id uint) (*TeamConfig, error)
	SaveTeam(t *TeamConfig) error
	DeleteTeam(id uint) error
	ListRoutes() ([]Route, error) // 按优先级、id
	GetRoute(id uint) (*Route, error)
	SaveRoute(r *Route) error
	DeleteRoute(id uint) error
	ListSLA() ([]SLAPolicy, error)
	SaveSLA(ps []SLAPolicy) error // 按级别 upsert，同一事务
	ListCollectConfigs() ([]CollectConfig, error)
	GetCollectConfig(id uint) (*CollectConfig, error)
	GetCollectConfigByInstance(instanceID uint) (*CollectConfig, error)
	SaveCollectConfig(c *CollectConfig) error
	DeleteCollectConfig(id uint) error
	// LockCollect 抢占一次采集：把 last_status 置为 token（RUNNING@时间），仅当它不是未过期的 RUNNING 时成功。
	LockCollect(id uint, token string, staleBefore string) (bool, error)
	// FinishCollect 写采集结果；end 非空时同时推进 last_collect_time。
	FinishCollect(id uint, end *time.Time, status string) error

	// ---- 游标（performance_schema 累计值、dbt_slow_logs 自增 id） ----
	GetCursor(instanceID uint, schema, digest string) (*DigestCursor, error)
	SetCursor(c *DigestCursor) error
	ListCursors(instanceID uint) ([]DigestCursor, error)
	SaveCursors(creates, updates []DigestCursor) error

	// ---- 后台任务运行记录（新表 dbt_perf_job_run） ----
	JobRunsReady() bool
	ClaimJobRun(job, key string, now time.Time) (bool, error) // 已有同 key 记录返回 false
	FinishJobRun(job, key string, now time.Time, result string) error

	// ---- 目录：用户、资源组、实例 ----
	GetUsers(ids []uint) (map[uint]UserBrief, error)
	ListActiveUsers() ([]UserBrief, error)
	UsersByNames(names []string) ([]UserBrief, error) // 仅启用用户
	UserGroupIDs(userID uint) ([]uint, error)
	ListGroups() ([]GroupBrief, error) // 未删除的资源组
	InstanceGroupIDs(instanceID uint) ([]uint, error)
	UserInstanceIDs(userID uint) (map[uint]bool, error)
	ListInstances(dbTypes []string) ([]InstanceBrief, error)
	GetInstance(id uint) (*model.Instance, error)
	EngineConn(inst *model.Instance) engine.Conn
	DatabaseOwner(instanceID uint, db string) (string, error) // dbt_instance_database.owner
	DBALoads(usernames []string) ([]UserLoad, error)
	ConfigValue(item string) (string, bool) // dbt_setting
	InsertAudit(e *model.AuditEntry) error

	SlowStore
}

// Service 是性能工单的业务入口。
type Service struct {
	repo       Repo
	now        func() time.Time
	notifier   notify.Notifier
	async      func(func()) // 发通知用；测试里替换成同步执行
	explainer  Explainer
	collectors map[string]Collector
	BaseURL    string // 对外访问地址，用于通知里的工单链接

	mu         sync.Mutex
	collecting map[uint]bool // 本进程内正在采集的配置，防止试采集与定时采集同时跑
}

// Options 是构造服务的可选项。
type Options struct {
	Notifier   notify.Notifier
	Explainer  Explainer
	Collectors map[string]Collector // 为空时用 DefaultCollectors(repo)
	BaseURL    string
}

func NewService(repo Repo, o Options) *Service {
	if o.Notifier == nil {
		o.Notifier = notify.Noop{}
	}
	s := &Service{repo: repo, now: time.Now, notifier: o.Notifier, explainer: o.Explainer, collectors: o.Collectors, BaseURL: o.BaseURL,
		collecting: map[uint]bool{}}
	if s.collectors == nil {
		s.collectors = DefaultCollectors(repo)
	}
	s.async = func(f func()) { go f() }
	return s
}

// SetClock / SetAsync 仅测试使用。
func (s *Service) SetClock(f func() time.Time) { s.now = f }
func (s *Service) SetAsync(f func(func()))     { s.async = f }

// SetCollector 替换某种采集方式的实现（测试里注入假采集器）。
func (s *Service) SetCollector(name string, c Collector) { s.collectors[name] = c }

// ---- 用户与权限 ----

// Viewer 是发起操作的用户及其权限快照。
type Viewer struct {
	audit.Actor
	Manage bool          // 持有任一管理类按钮（managerButtons）：DBA
	Groups map[uint]bool // 所在资源组
}

// IsDBA 超级用户或持有任一管理类按钮。
func (v Viewer) IsDBA() bool { return v.IsSuperuser || v.Manage }

// CanHandle 研发侧操作权限：负责人 / 同团队成员 / DBA。同时也是可见范围。
func (v Viewer) CanHandle(i *Issue) bool {
	if v.IsDBA() {
		return true
	}
	if i.AssigneeID != nil && *i.AssigneeID == v.ID {
		return true
	}
	return i.TeamID != nil && v.Groups[*i.TeamID]
}

// system 是后台任务的操作人。
var systemActor = audit.Actor{Username: "system", Display: "系统"}

// 系统配置（dbt_setting）项，都可在“系统管理 → 配置项”里覆盖。
func (s *Service) cfgStr(item, def string) string {
	if v, ok := s.repo.ConfigValue(item); ok {
		return strings.TrimSpace(v)
	}
	return def
}

func (s *Service) cfgInt(item string, def int) int {
	if v, ok := s.repo.ConfigValue(item); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func (s *Service) cfgFloat(item string, def float64) float64 {
	if v, ok := s.repo.ConfigValue(item); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}

func (s *Service) cfgBool(item string, def bool) bool {
	if v, ok := s.repo.ConfigValue(item); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off", "":
			return false
		}
	}
	return def
}

// ---- 时间格式 ----

const (
	tsLayout   = "2006-01-02 15:04:05"
	dateLayout = "2006-01-02"
)

func fmtTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Format(tsLayout)
}

func fmtDate(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Format(dateLayout)
}

// dateOf 取某天 0 点（本地时间）。
func dateOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func parseDate(s string, loc *time.Location) (time.Time, error) {
	return time.ParseInLocation(dateLayout, strings.TrimSpace(s), loc)
}

// ---- 对外结构 ----

// IssueDTO 是工单列表/详情用的 JSON 结构。
type IssueDTO struct {
	ID              uint     `json:"id"`
	Severity        string   `json:"severity"`
	SeverityLabel   string   `json:"severity_label"`
	SeverityLocked  bool     `json:"severity_locked"`
	Status          string   `json:"status"`
	StatusLabel     string   `json:"status_label"`
	Category        string   `json:"category"`
	CategoryLabel   string   `json:"category_label"`
	OwnerSide       string   `json:"owner_side"`
	OwnerSideLabel  string   `json:"owner_side_label"`
	InstanceID      uint     `json:"instance_id"`
	Instance        string   `json:"instance"`
	DBName          string   `json:"db_name"`
	Tables          string   `json:"tables"`
	Fingerprint     string   `json:"fingerprint"`
	TeamID          uint     `json:"team_id"`
	Team            string   `json:"team"`
	AssigneeID      uint     `json:"assignee_id"`
	Assignee        string   `json:"assignee"`
	DBAID           uint     `json:"dba_id"`
	DBA             string   `json:"dba"`
	ImpactScore     float64  `json:"impact_score"`
	LoadShare       float64  `json:"load_share"`
	AvgTimeMs       float64  `json:"avg_time_ms"`
	MaxTimeMs       float64  `json:"max_time_ms"`
	PerDayCount     float64  `json:"per_day_count"`
	RowsExamined    float64  `json:"rows_examined_avg"`
	RowsSent        float64  `json:"rows_sent_avg"`
	AcceptDeadline  string   `json:"accept_deadline"`
	ResolveDeadline string   `json:"resolve_deadline"`
	LeftHours       *float64 `json:"left_hours"` // 距解决截止的小时数；仅 SLA 计时中的工单有值，负数表示已超时
	Overdue         bool     `json:"overdue"`
	EscalationLevel int      `json:"escalation_level"`
	RemindCount     int      `json:"remind_count"`
	SLABreached     bool     `json:"sla_breached"`
	FirstSeen       string   `json:"first_seen"`
	LastSeen        string   `json:"last_seen"`
	CreateTime      string   `json:"create_time"`
	ClosedAt        string   `json:"closed_at"`
	CloseReason     string   `json:"close_reason"`
	CloseLabel      string   `json:"close_reason_label"`
	Source          string   `json:"source"`
}

func (s *Service) toDTO(r *IssueRow, now time.Time, fpLimit int) IssueDTO {
	fp := r.Fingerprint
	if fpLimit > 0 {
		fp = clipRunes(fp, fpLimit)
	}
	d := IssueDTO{
		ID: r.ID, Severity: r.Severity, SeverityLabel: severityLabel[r.Severity], SeverityLocked: r.SeverityLocked,
		Status: r.Status, StatusLabel: statusLabel[r.Status], Category: r.Category, CategoryLabel: labelOf(Categories, r.Category),
		OwnerSide: r.OwnerSide, OwnerSideLabel: sideLabel[r.OwnerSide], InstanceID: r.InstanceID, Instance: r.InstanceName,
		DBName: r.DBName, Tables: r.Tables, Fingerprint: fp, TeamID: derefUint(r.TeamID), Team: r.TeamName,
		AssigneeID: derefUint(r.AssigneeID), Assignee: r.AssigneeName, DBAID: derefUint(r.DBAID), DBA: r.DBAName,
		ImpactScore: r.ImpactScore, LoadShare: r.LoadShare, AvgTimeMs: r.AvgTimeMs, MaxTimeMs: r.MaxTimeMs, PerDayCount: r.PerDayCount,
		RowsExamined: r.RowsExaminedAvg, RowsSent: r.RowsSentAvg, AcceptDeadline: fmtTime(r.AcceptDeadline), ResolveDeadline: fmtTime(r.ResolveDeadline),
		EscalationLevel: r.EscalationLevel, RemindCount: r.RemindCount, SLABreached: r.SLABreached,
		FirstSeen: fmtTime(r.FirstSeen), LastSeen: fmtTime(r.LastSeen), CreateTime: fmtTime(&r.CreateTime), ClosedAt: fmtTime(r.ClosedAt),
		CloseReason: r.CloseReason, Source: r.Source,
	}
	if r.CloseReason != "" {
		d.CloseLabel = closeLabel[r.CloseReason]
	}
	if r.ResolveDeadline != nil && isSLARunning(r.Status) {
		h := r.ResolveDeadline.Sub(now).Hours()
		h = float64(int(h*10)) / 10
		d.LeftHours = &h
		d.Overdue = r.ResolveDeadline.Before(now)
	}
	return d
}

// ---- 列表 ----

// ListFilter 是接口层传来的列表筛选。
type ListFilter struct {
	View       string
	Severity   string
	OwnerSide  string
	Category   string
	TeamID     uint
	InstanceID uint
	Search     string
	Sort       string
	Desc       bool
	Offset     int
	Limit      int
	Status     string
	Source     string
	Scope      string // "" 按角色（DBA 看全部，其他人看自己团队的）；"mine"：DBA 也只看自己团队与自己负责的
}

// ListResult 是列表结果：行、总数，以及各页签的角标数。
type ListResult struct {
	Rows    []IssueDTO   `json:"rows"`
	Total   int          `json:"total"`
	Todo    int          `json:"todo"`             // 我的待办
	Overdue int          `json:"overdue"`          // 已超时（可见范围内）
	Counts  *IssueCounts `json:"counts,omitempty"` // 各页签 / KPI 的计数（仓库支持时）
}

func (s *Service) scope(v Viewer, q *IssueQuery) { s.scopeFor(v, q, "") }

// scopeFor 同 scope，另可让 DBA 只看“我的团队与我负责的”（scope=mine）。
func (s *Service) scopeFor(v Viewer, q *IssueQuery, scope string) {
	q.UserID = v.ID
	q.Now = s.now()
	if v.IsDBA() && scope != "mine" {
		return
	}
	q.Restrict = true
	for g := range v.Groups {
		q.TeamIDs = append(q.TeamIDs, g)
	}
}

func (s *Service) ListIssues(v Viewer, f ListFilter) (*ListResult, error) {
	if f.View == "" {
		f.View = ViewOpen
	}
	switch f.View {
	case ViewTodo, ViewOpen, ViewOverdue, ViewFixed, ViewClosed, ViewAll, ViewNew, ViewAssigned, ViewAccepted, ViewApprove:
	default:
		return nil, bad("不支持的列表视图")
	}
	if _, ok := severityRank[f.Severity]; f.Severity != "" && !ok {
		return nil, bad("级别不合法")
	}
	if f.Status != "" && statusLabel[f.Status] == "" {
		return nil, bad("状态不合法")
	}
	if f.Source != "" && !contains(issueSources, f.Source) {
		return nil, bad("来源不合法")
	}
	if f.Scope != "" && f.Scope != "all" && f.Scope != "mine" {
		return nil, bad("可见范围不合法")
	}
	if f.OwnerSide != "" && sideLabel[f.OwnerSide] == "" {
		return nil, bad("处理方不合法")
	}
	if f.Limit <= 0 {
		f.Limit = 20
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	sortKey := f.Sort
	if !sortable[sortKey] {
		sortKey, f.Desc = "impact_score", true
	}
	q := IssueQuery{View: f.View, Severity: f.Severity, OwnerSide: f.OwnerSide, Category: f.Category, TeamID: f.TeamID, InstanceID: f.InstanceID,
		Search: strings.TrimSpace(f.Search), Sort: sortKey, Desc: f.Desc, Offset: f.Offset, Limit: f.Limit, Status: f.Status, Source: f.Source}
	s.scopeFor(v, &q, f.Scope)
	rows, total, err := s.repo.ListIssues(q)
	if err != nil {
		return nil, err
	}
	out := &ListResult{Rows: make([]IssueDTO, 0, len(rows)), Total: total}
	for i := range rows {
		out.Rows = append(out.Rows, s.toDTO(&rows[i], q.Now, 300))
	}
	if e := s.ext(); e != nil { // 一条 SQL 算出全部页签与 KPI 的计数
		cq := IssueQuery{}
		s.scopeFor(v, &cq, f.Scope)
		if c, cerr := e.IssueCounts(cq); cerr == nil {
			out.Counts, out.Todo, out.Overdue = &c, c.Todo, c.Overdue
			return out, nil
		}
	}
	if out.Todo, out.Overdue, err = s.counts(v); err != nil {
		return nil, err
	}
	return out, nil
}

// 工单来源（dbt_perf_issue.source）。
var issueSources = []string{"collector", "manual", "slowlog"}

// IssueSources 是来源及显示名（筛选下拉用）。
var IssueSources = []Option{{"collector", "自动采集"}, {"manual", "手工建单"}, {"slowlog", "慢日志页面建单"}}

// counts 返回页签角标：我的待办数、可见范围内的已超时数。
func (s *Service) counts(v Viewer) (todo, overdue int, err error) {
	for _, view := range []string{ViewTodo, ViewOverdue} {
		q := IssueQuery{View: view, Limit: 1, Sort: "id"}
		s.scope(v, &q)
		_, n, e := s.repo.ListIssues(q)
		if e != nil {
			return 0, 0, e
		}
		if view == ViewTodo {
			todo = n
		} else {
			overdue = n
		}
	}
	return todo, overdue, nil
}

// MyTodo 是顶栏菜单角标：我负责的待办数，以及其中已超时的数。
type MyTodo struct {
	Todo    int `json:"todo"`
	Overdue int `json:"overdue"`
}

// MyTodo 统计我的待办：我是负责人且 SLA 计时中，或我是跟进 DBA 且待验证/豁免审批中；
// 其中超时的是 SLA 计时中且已过解决截止的。
func (s *Service) MyTodo(v Viewer) (MyTodo, error) {
	now := s.now()
	_, todo, err := s.repo.ListIssues(IssueQuery{View: ViewTodo, UserID: v.ID, Now: now, Limit: 1, Sort: "id"})
	if err != nil {
		return MyTodo{}, err
	}
	_, over, err := s.repo.ListIssues(IssueQuery{View: ViewTodoOverdue, UserID: v.ID, Now: now, Limit: 1, Sort: "id"})
	if err != nil {
		return MyTodo{}, err
	}
	return MyTodo{Todo: todo, Overdue: over}, nil
}

// ---- 详情 ----

// LogDTO 是处理记录。
type LogDTO struct {
	ID         uint   `json:"id"`
	Action     string `json:"action"`
	ActionName string `json:"action_name"`
	Content    string `json:"content"`
	Operator   string `json:"operator"`
	Display    string `json:"operator_display"`
	Time       string `json:"time"`
}

// ActionDTO 是详情页上可点的按钮。
type ActionDTO struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	DBA   bool   `json:"dba"`
}

// IssueDetail 是工单详情。
type IssueDetail struct {
	IssueDTO
	SampleSQL        string           `json:"sample_sql"`
	FingerprintFull  string           `json:"fingerprint_full"`
	Diagnosis        string           `json:"diagnosis"`
	Suggestion       string           `json:"suggestion"`
	SuggestedDDL     string           `json:"suggested_ddl"`
	Explain          []map[string]any `json:"explain"`
	ExplainCols      []string         `json:"explain_cols"`
	PlanFinishDate   string           `json:"plan_finish_date"`
	FixDesc          string           `json:"fix_desc"`
	FixedAt          string           `json:"fixed_at"`
	AcceptedAt       string           `json:"accepted_at"`
	ReopenCount      int              `json:"reopen_count"`
	ExtendTimes      int              `json:"extend_times"`
	MaxExtendTimes   int              `json:"max_extend_times"`
	PendingExtend    string           `json:"pending_extend_date"`
	PendingReason    string           `json:"pending_reason"`
	BaselineAvgMs    float64          `json:"baseline_avg_ms"`
	BaselineDailySec float64          `json:"baseline_daily_sec"`
	ExecCount7d      int64            `json:"exec_count_7d"`
	Logs             []LogDTO         `json:"logs"`
	Actions          []ActionDTO      `json:"actions"`
	MaxPlanDate      string           `json:"max_plan_date"` // 受理时承诺日期的上限（SLA 解决截止日）
	Today            string           `json:"today"`
	IsDBA            bool             `json:"is_dba"`
	WorkflowID       uint             `json:"workflow_id,omitempty"` // 标记修复时关联的 SQL 上线工单号
	Effect           *FixEffect       `json:"effect,omitempty"`      // 标记修复之后的效果（与修复前基线对比）
}

// visibleIssue 取工单并校验可见性；不可见按不存在处理（不暴露工单是否存在）。
func (s *Service) visibleIssue(v Viewer, id uint) (*IssueRow, error) {
	r, err := s.repo.GetIssueRow(id)
	if err != nil {
		return nil, err
	}
	if r == nil || !v.CanHandle(&r.Issue) {
		return nil, notFound("工单不存在或无权查看")
	}
	return r, nil
}

func (s *Service) GetIssue(v Viewer, id uint) (*IssueDetail, error) {
	r, err := s.visibleIssue(v, id)
	if err != nil {
		return nil, err
	}
	now := s.now()
	d := &IssueDetail{IssueDTO: s.toDTO(r, now, 0), SampleSQL: r.SampleSQL, FingerprintFull: r.Fingerprint, Diagnosis: r.Diagnosis,
		Suggestion: r.Suggestion, SuggestedDDL: r.SuggestedDDL, PlanFinishDate: fmtDate(r.PlanFinishDate), FixDesc: r.FixDesc,
		FixedAt: fmtTime(r.FixedAt), AcceptedAt: fmtTime(r.AcceptedAt), ReopenCount: r.ReopenCount, ExtendTimes: r.ExtendTimes,
		PendingExtend: fmtDate(r.PendingExtendDate), PendingReason: r.PendingReason, BaselineAvgMs: r.BaselineAvgMs,
		BaselineDailySec: r.BaselineDailySec, ExecCount7d: r.ExecCount7d, Today: now.Format(dateLayout), IsDBA: v.IsDBA()}
	d.Fingerprint = r.Fingerprint
	if r.ResolveDeadline != nil {
		d.MaxPlanDate = maxPlanDate(*r.ResolveDeadline, now).Format(dateLayout)
	}
	d.MaxExtendTimes = s.policy(r.Severity).MaxExtendTimes
	d.Explain, d.ExplainCols = parseExplain(r.ExplainResult)
	logs, err := s.repo.ListLogs(id, 200)
	if err != nil {
		return nil, err
	}
	d.Logs = make([]LogDTO, 0, len(logs))
	for _, l := range logs {
		name := actionName[l.Action]
		if name == "" {
			name = l.Action
		}
		d.Logs = append(d.Logs, LogDTO{ID: l.ID, Action: l.Action, ActionName: name, Content: l.Content, Operator: l.Operator,
			Display: l.OperatorDisplay, Time: fmtTime(&l.CreateTime)})
	}
	d.Actions = s.availableActions(v, &r.Issue)
	d.WorkflowID = s.workflowOf(id)
	d.Effect = s.fixEffect(&r.Issue)
	return d, nil
}

// TrendPoint 是趋势图的一个点。
type TrendPoint struct {
	Date      string  `json:"date"`
	ExecCount int64   `json:"exec_count"`
	TotalTime float64 `json:"total_time"`
	AvgMs     float64 `json:"avg_ms"`
}

// Trend 是工单的趋势：每日总耗时与平均耗时，以及“标记修复”“建单”的日期。
type Trend struct {
	Points  []TrendPoint `json:"points"`
	FixedAt string       `json:"fixed_at"`
	Created string       `json:"created"`
}

func (s *Service) IssueTrend(v Viewer, id uint, days int) (*Trend, error) {
	r, err := s.visibleIssue(v, id)
	if err != nil {
		return nil, err
	}
	if days <= 0 || days > 365 {
		days = 30
	}
	since := dateOf(s.now()).AddDate(0, 0, -days)
	stats, err := s.repo.ListStats(id, since)
	if err != nil {
		return nil, err
	}
	t := &Trend{Points: make([]TrendPoint, 0, len(stats)), FixedAt: fmtDate(r.FixedAt), Created: r.CreateTime.Format(dateLayout)}
	for _, st := range stats {
		p := TrendPoint{Date: st.StatDate.Format(dateLayout), ExecCount: st.ExecCount, TotalTime: round(st.TotalTime, 2)}
		if st.ExecCount > 0 {
			p.AvgMs = round(st.TotalTime*1000/float64(st.ExecCount), 1)
		}
		t.Points = append(t.Points, p)
	}
	return t, nil
}

func round(v float64, n int) float64 {
	p := 1.0
	for i := 0; i < n; i++ {
		p *= 10
	}
	if v < 0 {
		return float64(int64(v*p-0.5)) / p
	}
	return float64(int64(v*p+0.5)) / p
}

// ---- 日志与审计 ----

// who 是操作人。
type who struct {
	id       uint
	username string
	display  string
}

func whoOf(a audit.Actor) who {
	d := a.Display
	if d == "" {
		d = a.Username
	}
	return who{a.ID, a.Username, d}
}

var systemWho = who{0, "system", "系统"}

func (s *Service) addLog(issueID uint, act, content string, w who) {
	err := s.repo.AddLog(&IssueLog{IssueID: issueID, Action: act, Content: content, Operator: w.username, OperatorDisplay: clipRunes(w.display, 50), CreateTime: s.now()})
	if err != nil {
		slog.Error("写性能工单日志失败", "issue", issueID, "action", act, "err", err)
	}
}

// audit 写操作审计（dbt_audit_log）。内容里不放密码、密钥；SQL 文本只放工单号。
func (s *Service) audit(w who, action, extra string) {
	err := s.repo.InsertAudit(&model.AuditEntry{UserID: w.id, UserName: w.username, UserDisplay: w.display, Action: action, ExtraInfo: extra, ActionTime: s.now()})
	if err != nil {
		slog.Error("写审计日志失败", "action", action, "err", err)
	}
}

// record 同时写工单日志与审计日志。
func (s *Service) record(i *Issue, act, content string, w who) { s.recordAs(i, act, act, content, w) }

// recordAs 同 record，但系统审计里的动作名用 auditAct（验证通过 / 同意豁免 / 误报关闭在工单日志里都是 close，审计要分开）。
func (s *Service) recordAs(i *Issue, logAct, auditAct, content string, w who) {
	s.addLog(i.ID, logAct, content, w)
	name := actionName[auditAct]
	if name == "" {
		name = auditAct
	}
	s.audit(w, "性能工单-"+name, fmt.Sprintf("工单#%d：%s", i.ID, clipRunes(strings.ReplaceAll(content, "\n", " "), 200)))
}

// ---- 小工具 ----

func uniqueUints(in []uint) []uint {
	seen := map[uint]bool{}
	var out []uint
	for _, x := range in {
		if x != 0 && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ctxBG 用于后台发送通知这类脱离请求的调用。
func ctxBG(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// explainColOrder 是执行计划列的展示顺序（存库的 JSON 里列名无序）。
var explainColOrder = []string{"id", "select_type", "table", "partitions", "type", "possible_keys", "key", "key_len", "ref", "rows", "filtered", "extra"}

// parseExplain 解析存在 explain_result 里的执行计划 JSON，返回行与列顺序；解析失败返回空。
func parseExplain(raw string) ([]map[string]any, []string) {
	rows := []map[string]any{}
	cols := []string{}
	if strings.TrimSpace(raw) == "" {
		return rows, cols
	}
	if err := json.Unmarshal([]byte(raw), &rows); err != nil || len(rows) == 0 {
		return []map[string]any{}, cols
	}
	seen := map[string]bool{}
	for _, c := range explainColOrder {
		for _, r := range rows {
			if _, ok := r[c]; ok && !seen[c] {
				seen[c] = true
				cols = append(cols, c)
			}
		}
	}
	var rest []string
	for _, r := range rows {
		for c := range r {
			if !seen[c] {
				seen[c] = true
				rest = append(rest, c)
			}
		}
	}
	sort.Strings(rest)
	return rows, append(cols, rest...)
}

// maxPlanDate 受理时承诺日期的上限：SLA 解决截止日；截止日已经过了就最晚取今天（否则这张单永远没法受理）。
func maxPlanDate(deadline, now time.Time) time.Time {
	d, today := dateOf(deadline), dateOf(now)
	if d.Before(today) {
		return today
	}
	return d
}
