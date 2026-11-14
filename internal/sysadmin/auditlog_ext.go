// @author: kevinkelly
// @license: Apache Licence
// @file: auditlog_ext.go
// @time: 2026/10/28

package sysadmin

import (
	"sort"
	"strings"
	"sync"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/btnperm"
	"dbtool/internal/model"
)

// 系统审计的扩展能力：审计行的结果 / 来源 IP / 类别 / 权限码 / 高危（旁路表 dbt_audit_ext，见 internal/audit/ext.go），
// 概览统计，查询审计里被拒绝的语句（旁路表 dbt_query_reject，由“在线查询”模块写入）。
//
// 这些能力都走“可选接口”：仓库不实现（测试里的假仓库）或旁路表还没创建时，系统审计退回到原来的行为，页面照常可用。

// AuditExtFilter 是通用审计在原有过滤条件之外的扩展条件。
type AuditExtFilter struct {
	Category string // audit.Category* 之一
	Result   string // 成功 / 失败 / 拒绝
	Risky    bool   // 仅高危
}

// AuditRowExt 是带扩展信息的审计行（扩展信息缺失的历史行各字段为空）。
type AuditRowExt struct {
	model.AuditEntry
	Result   string
	IP       string
	Category string
	Module   string
	Code     string
	Risky    bool
}

// DayCount 是某天的审计条数。
type DayCount struct {
	Date  string `json:"date"` // YYYY-MM-DD
	Count int64  `json:"count"`
}

// UserCount 是某用户的审计条数。
type UserCount struct {
	UserName    string `json:"user_name"`
	UserDisplay string `json:"user_display"`
	Count       int64  `json:"count" gorm:"column:cnt"`
}

// AuditOverviewRaw 是仓库返回的概览原始数据。Risky7 / Denied7 在扩展表不可用时为 -1。
type AuditOverviewRaw struct {
	Days        []DayCount
	Events7     int64
	LoginFailed int64
	Lockouts    int64
	Risky7      int64
	Denied7     int64
	TopUsers    []UserCount
}

// RejectRow 是被拒绝的查询语句（dbt_query_reject）。
type RejectRow struct {
	ID           uint
	UserName     string
	InstanceID   uint
	InstanceName string
	DBName       string
	SQLText      string
	Reason       string
	CreatedAt    time.Time
}

// QueryStatsRaw 是查询审计的汇总（成功记录来自 dbt_query_log，拒绝记录来自 dbt_query_reject）。
type QueryStatsRaw struct {
	Total     int64   // 成功记录数
	Rejected  int64   // 被拒绝记录数
	AvgCost   float64 // 成功记录平均耗时（秒）
	MaxCost   float64 // 最慢一条（秒）
	MaxUser   string  // 最慢一条的用户
	UserCount int64   // 涉及用户数（成功记录）
}

// auditExtRepo 由实现了审计扩展的仓库提供（store）。
type auditExtRepo interface {
	AuditExtReady() bool
	ListAuditLogsExt(f AuditLogFilter, x AuditExtFilter) ([]AuditRowExt, int64, error)
	GetAuditRowExt(id uint) (*AuditRowExt, error) // 不存在返回 nil, nil
	RecentAuditsByUser(username string, excludeID uint, limit int) ([]AuditRowExt, error)
	AuditOverviewData(now time.Time) (*AuditOverviewRaw, error)
}

// queryAuditRepo 由实现了“被拒绝的查询”读取的仓库提供。
type queryAuditRepo interface {
	QueryRejectReady() bool
	// ListQueryRejects 返回符合条件的前 limit 条（新的在前）与总数；User / Instance 精确，Search 匹配 SQL / 原因 / 库。
	ListQueryRejects(f QueryLogFilter, limit int) ([]RejectRow, int64, error)
	QueryAuditStats(f QueryLogFilter) (*QueryStatsRaw, error)
	GetQueryLog(id uint) (*model.QueryLog, error)
	GetQueryReject(id uint) (*RejectRow, error)
}

// sqlStatsRepo 提供 SQL 上线审计的状态分布。
type sqlStatsRepo interface {
	SQLWorkflowStatusCounts(f SQLWorkflowFilter) (map[string]int64, error)
}

func (s *Service) extRepo() (auditExtRepo, bool) {
	r, ok := s.repo.(auditExtRepo)
	if !ok || !r.AuditExtReady() {
		return nil, false
	}
	return r, true
}

func (s *Service) rejectRepo() (queryAuditRepo, bool) {
	r, ok := s.repo.(queryAuditRepo)
	if !ok || !r.QueryRejectReady() {
		return nil, false
	}
	return r, true
}

// 结果取值（与 audit 包一致，页面筛选下拉也用这三个值）。
var auditResults = []string{audit.ResultOK, audit.ResultFailed, audit.ResultDenied}

func validResult(v string) bool {
	for _, r := range auditResults {
		if r == v {
			return true
		}
	}
	return false
}

// fillExt 把扩展信息填进列表项。
func fillExt(it *AuditLogItem, r AuditRowExt) {
	it.Result, it.IP, it.Category, it.Module, it.Code, it.Risky = r.Result, r.IP, r.Category, r.Module, r.Code, r.Risky
	it.CategoryLabel = audit.CategoryLabel(r.Category)
	if r.Code != "" {
		if b, ok := btnperm.Lookup(r.Code); ok {
			it.CodeLabel = b.Label
		}
	}
}

// ---- 概览 ----

// AuditKPI 是概览里的数字卡片。RiskyCount / DeniedCount 为 nil 表示扩展表不可用。
type AuditOverview struct {
	ExtReady    bool        `json:"ext_ready"`
	Days        []DayCount  `json:"days"`
	Events7     int64       `json:"events_7d"`
	LoginFailed int64       `json:"login_failed_7d"`
	Lockouts    int64       `json:"lockouts_7d"`
	RiskyCount  *int64      `json:"risky_7d"`
	DeniedCount *int64      `json:"denied_7d"`
	TopUsers    []UserCount `json:"top_users"`
}

const overviewTTL = time.Minute

// ovCache 保存各 Service 的概览缓存（不改 Service 结构体，避免和别的文件的改动冲突）。
var ovCache sync.Map // *Service -> *ovEntry

type ovEntry struct {
	mu sync.Mutex
	at time.Time
	v  *AuditOverview
}

// AuditOverview 返回近 7 天 KPI、近 14 天按天条数与最活跃用户（缓存 1 分钟，避免每次打开页面都扫表）。
func (s *Service) AuditOverview() (*AuditOverview, error) {
	v, _ := ovCache.LoadOrStore(s, &ovEntry{})
	e := v.(*ovEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.v != nil && s.now().Sub(e.at) < overviewTTL {
		return e.v, nil
	}
	out := &AuditOverview{Days: []DayCount{}, TopUsers: []UserCount{}}
	r, ok := s.repo.(auditExtRepo)
	if !ok {
		return out, nil // 假仓库 / 不支持：返回空概览，页面隐藏该区域
	}
	raw, err := r.AuditOverviewData(s.now())
	if err != nil {
		return nil, err
	}
	out.ExtReady = r.AuditExtReady()
	out.Events7, out.LoginFailed, out.Lockouts = raw.Events7, raw.LoginFailed, raw.Lockouts
	if raw.Days != nil {
		out.Days = raw.Days
	}
	if raw.TopUsers != nil {
		out.TopUsers = raw.TopUsers
	}
	if raw.Risky7 >= 0 {
		v := raw.Risky7
		out.RiskyCount = &v
	}
	if raw.Denied7 >= 0 {
		v := raw.Denied7
		out.DeniedCount = &v
	}
	e.v, e.at = out, s.now()
	return out, nil
}

// ---- 详情 ----

// AuditDetail 是审计行详情：本行 + 同一用户的其他近期操作。
type AuditDetail struct {
	Item   AuditLogItem   `json:"item"`
	Recent []AuditLogItem `json:"recent"`
	Hint   string         `json:"hint,omitempty"` // 给登录失败类审计的说明
}

// AuditDetail 返回一条审计的详情。扩展表不可用时只返回本行基本信息与同一用户的其他近期操作（基础字段）。
func (s *Service) AuditDetail(id uint) (*AuditDetail, error) {
	r, ok := s.repo.(auditExtRepo)
	if !ok {
		return nil, notFound("当前仓库不支持审计详情")
	}
	row, err := r.GetAuditRowExt(id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, notFound("审计记录不存在")
	}
	d := &AuditDetail{Item: rowItem(*row), Recent: []AuditLogItem{}}
	if row.UserName != "" {
		recent, err := r.RecentAuditsByUser(row.UserName, row.ID, 6)
		if err != nil {
			return nil, err
		}
		for _, x := range recent {
			d.Recent = append(d.Recent, rowItem(x))
		}
	}
	if row.Action == "登入失败" || row.Action == "账号锁定" { // i18n-ignore 与存库的动作名比较
		d.Hint = "登录失败连续达到阈值会锁定账号（阈值与锁定时长见“登录与安全设置”）；被锁定账号由超级用户在“用户管理”解锁。登录提示语不区分“用户不存在 / 密码错 / 被锁定”，防止枚举账号。"
	}
	return d, nil
}

func rowItem(r AuditRowExt) AuditLogItem {
	it := toAuditItem(r.AuditEntry)
	fillExt(&it, r)
	return it
}

// ---- SQL 上线状态分布 ----

// StatusCount 是某个工单状态的数量。
type StatusCount struct {
	Status string `json:"status"`
	Label  string `json:"label"`
	Count  int64  `json:"count"`
}

// SQLWorkflowStats 返回 SQL 上线审计（同列表的筛选条件）里各状态的工单数，按常见流转顺序排列。
func (s *Service) SQLWorkflowStats(q SQLWorkflowQuery) ([]StatusCount, error) {
	if q.Status != "" && sqlStatusLabel(q.Status) == q.Status {
		return nil, bad("未知的工单状态")
	}
	start, end, err := parseDateRange(q.StartDate, q.EndDate)
	if err != nil {
		return nil, err
	}
	r, ok := s.repo.(sqlStatsRepo)
	if !ok {
		return []StatusCount{}, nil
	}
	m, err := r.SQLWorkflowStatusCounts(SQLWorkflowFilter{Status: q.Status, GroupID: q.GroupID, Search: strings.TrimSpace(q.Search), Start: start, End: end})
	if err != nil {
		return nil, err
	}
	out := []StatusCount{}
	seen := map[string]bool{}
	for _, st := range sqlStatusLabels {
		if n := m[st.Value]; n > 0 {
			out = append(out, StatusCount{st.Value, st.Label, n})
		}
		seen[st.Value] = true
	}
	var rest []string
	for k := range m {
		if !seen[k] && m[k] > 0 {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		out = append(out, StatusCount{k, k, m[k]})
	}
	return out, nil
}

// ---- 查询审计：成功记录 + 被拒绝的语句 ----

// QueryAuditStats 是查询审计页顶部的三个 KPI（加上总数）。
type QueryAuditStats struct {
	RejectReady bool    `json:"reject_ready"`
	Total       int64   `json:"total"`    // 成功 + 被拒绝
	Rejected    int64   `json:"rejected"` // 被拒绝
	Users       int64   `json:"users"`
	AvgCost     float64 `json:"avg_cost"` // 秒，不含被拒绝
	MaxCost     float64 `json:"max_cost"`
	MaxUser     string  `json:"max_user"`
}

// QueryAuditStats 返回查询审计的汇总（同列表的筛选条件，忽略“仅被拒绝”与分页）。
func (s *Service) QueryAuditStats(q QueryAuditQuery) (*QueryAuditStats, error) {
	start, end, err := parseDateRange(q.StartDate, q.EndDate)
	if err != nil {
		return nil, err
	}
	f := QueryLogFilter{User: strings.TrimSpace(q.User), Instance: strings.TrimSpace(q.Instance), Search: strings.TrimSpace(q.Search), Start: start, End: end}
	out := &QueryAuditStats{}
	r, ok := s.repo.(queryAuditRepo)
	if !ok {
		return out, nil
	}
	out.RejectReady = r.QueryRejectReady()
	raw, err := r.QueryAuditStats(f)
	if err != nil {
		return nil, err
	}
	out.Total, out.Rejected, out.Users = raw.Total+raw.Rejected, raw.Rejected, raw.UserCount
	out.AvgCost, out.MaxCost, out.MaxUser = raw.AvgCost, raw.MaxCost, raw.MaxUser
	return out, nil
}

// QueryLogDetail 是一条查询审计的完整内容（SQL 不截断，上限 10 万字符）。
type QueryLogDetail struct {
	QueryLogItem
	SQLFull string `json:"sql_full"`
}

const detailSQLRunes = 100000

// QueryAuditDetail 返回一条查询审计的完整 SQL：kind 为 ok（dbt_query_log）或 rejected（dbt_query_reject）。
func (s *Service) QueryAuditDetail(kind string, id uint) (*QueryLogDetail, error) {
	r, ok := s.repo.(queryAuditRepo)
	if !ok {
		return nil, notFound("当前仓库不支持查询审计详情")
	}
	var it QueryLogItem
	var full string
	switch kind {
	case "ok":
		l, err := r.GetQueryLog(id)
		if err != nil {
			return nil, err
		}
		if l == nil {
			return nil, notFound("查询记录不存在")
		}
		it, full = logItem(*l), l.SQLLog
	case "rejected":
		if !r.QueryRejectReady() {
			return nil, notFound("查询记录不存在")
		}
		x, err := r.GetQueryReject(id)
		if err != nil {
			return nil, err
		}
		if x == nil {
			return nil, notFound("查询记录不存在")
		}
		it, full = s.rejectItem(*x, ""), x.SQLText
	default:
		return nil, bad("kind 只能是 ok 或 rejected")
	}
	if rr := []rune(full); len(rr) > detailSQLRunes {
		full = string(rr[:detailSQLRunes])
	}
	it.SQL, it.SQLTruncated = full, false
	return &QueryLogDetail{QueryLogItem: it, SQLFull: full}, nil
}

func clipSQL(s string) (string, bool) {
	if r := []rune(s); len(r) > sqlPreviewRunes {
		return string(r[:sqlPreviewRunes]), true
	}
	return s, false
}

func logItem(l model.QueryLog) QueryLogItem {
	sqlText, cut := clipSQL(l.SQLLog)
	status := "ok"
	if l.Masking {
		status = "masked"
	}
	return QueryLogItem{ID: l.ID, Kind: "ok", Status: status, InstanceName: l.InstanceName, DBName: l.DBName, SQL: sqlText, SQLTruncated: cut,
		EffectRow: l.EffectRow, CostTime: l.CostTime, Username: l.Username, UserDisplay: l.UserDisplay, Masking: l.Masking,
		CreateTime: fmtTime(l.CreateTime)}
}

func (s *Service) rejectItem(x RejectRow, display string) QueryLogItem {
	sqlText, cut := clipSQL(x.SQLText)
	if display == "" && x.UserName != "" {
		if u, err := s.repo.GetUserByName(x.UserName); err == nil && u != nil {
			display = u.Display
		}
	}
	return QueryLogItem{ID: x.ID, Kind: "rejected", Status: "rejected", Reason: x.Reason, InstanceName: x.InstanceName, DBName: x.DBName,
		SQL: sqlText, SQLTruncated: cut, CostTime: "", Username: x.UserName, UserDisplay: display, CreateTime: fmtTime(x.CreatedAt)}
}

// maxMergeRows 限制翻页深度：成功记录与拒绝记录要合并排序，每次各取前 offset+limit 条。
const maxMergeRows = 10000

// listQueryAudit 合并 dbt_query_log 与 dbt_query_reject（后者不存在时只有前者），按时间倒序分页。
func (s *Service) listQueryAudit(q QueryAuditQuery, f QueryLogFilter, off, lim, page int) (*QueryAuditPage, error) {
	rr, rejOK := s.rejectRepo()
	out := &QueryAuditPage{RejectReady: rejOK}
	if !rejOK && q.OnlyRejected {
		out.Paged = paged([]QueryLogItem{}, 0, page, lim)
		return out, nil
	}
	if off+lim > maxMergeRows {
		return nil, bad("翻页过深，请缩小时间范围或加上筛选条件")
	}
	var items []QueryLogItem
	var total int64
	if !q.OnlyRejected {
		f.Offset, f.Limit = 0, off+lim
		rows, n, err := s.repo.ListQueryLogs(f)
		if err != nil {
			return nil, err
		}
		total += n
		for _, l := range rows {
			items = append(items, logItem(l))
		}
	}
	if rejOK {
		rows, n, err := rr.ListQueryRejects(f, off+lim)
		if err != nil {
			return nil, err
		}
		total += n
		disp := map[string]string{}
		for _, x := range rows {
			d, seen := disp[x.UserName]
			if !seen {
				if u, err := s.repo.GetUserByName(x.UserName); err == nil && u != nil {
					d = u.Display
				}
				disp[x.UserName] = d
			}
			items = append(items, s.rejectItem(x, d))
		}
		sort.SliceStable(items, func(i, j int) bool { return items[i].CreateTime > items[j].CreateTime })
	}
	if off > len(items) {
		off = len(items)
	}
	end := off + lim
	if end > len(items) {
		end = len(items)
	}
	out.Paged = paged(append([]QueryLogItem{}, items[off:end]...), total, page, lim)
	return out, nil
}

// QueryAuditPage 是查询审计列表：分页结果 + 拒绝记录表是否可用。
type QueryAuditPage struct {
	*Paged[QueryLogItem]
	RejectReady bool `json:"reject_ready"`
}
