// @author: kevinkelly
// @license: Apache Licence
// @file: auditlog.go
// @time: 2026/10/28

package sysadmin

import (
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

const (
	maxExportRows   = 50000
	exportPageSize  = 1000
	actionsCacheTTL = time.Minute
	sqlPreviewRunes = 1000 // 查询审计列表里每条 SQL 最多返回的字符数
)

// AuditQuery 是通用审计的查询条件。日期是 YYYY-MM-DD，起止都含当天。
type AuditQuery struct {
	Action    string
	User      string
	Search    string
	StartDate string
	EndDate   string
	Page      int
	PageSize  int
	// 以下三项依赖审计扩展表（dbt_audit_ext）；表不可用时指定它们会得到 409。
	Category string // audit.Category* 之一
	Result   string // 成功 / 失败 / 拒绝
	Risky    bool   // 仅高危
}

// AuditLogItem 是通用审计的一行。扩展字段在历史行（写入时扩展表还不存在）或扩展表不可用时为空。
type AuditLogItem struct {
	ID            uint   `json:"id"`
	UserID        uint   `json:"user_id"`
	UserName      string `json:"user_name"`
	UserDisplay   string `json:"user_display"`
	Action        string `json:"action"`
	ExtraInfo     string `json:"extra_info"`
	ActionTime    string `json:"action_time"`
	Result        string `json:"result"`         // 成功 / 失败 / 拒绝
	IP            string `json:"ip"`             // 来源 IP
	Category      string `json:"category"`       // audit.Category*
	CategoryLabel string `json:"category_label"` // 类别显示名
	Module        string `json:"module"`
	Code          string `json:"code"`       // 权限码
	CodeLabel     string `json:"code_label"` // 权限码对应的按钮名
	Risky         bool   `json:"risky"`      // 高危
}

// Paged 是分页结果。
type Paged[T any] struct {
	Rows     []T   `json:"rows"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Pages    int   `json:"pages"`
}

func paged[T any](rows []T, total int64, page, size int) *Paged[T] {
	if rows == nil {
		rows = []T{}
	}
	pages := int((total + int64(size) - 1) / int64(size))
	if pages < 1 {
		pages = 1
	}
	return &Paged[T]{Rows: rows, Total: total, Page: page, PageSize: size, Pages: pages}
}

// parseDateRange 解析 YYYY-MM-DD 起止日期（含当天），返回 [start, end)。
func parseDateRange(startDate, endDate string) (start, end *time.Time, err error) {
	parse := func(s string) (*time.Time, error) {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, nil
		}
		t, err := time.ParseInLocation("2006-01-02", s, time.Local)
		if err != nil {
			return nil, bad("日期格式应为 YYYY-MM-DD")
		}
		return &t, nil
	}
	if start, err = parse(startDate); err != nil {
		return nil, nil, err
	}
	if end, err = parse(endDate); err != nil {
		return nil, nil, err
	}
	if end != nil {
		e := end.AddDate(0, 0, 1)
		end = &e
	}
	if start != nil && end != nil && !start.Before(*end) {
		return nil, nil, bad("结束日期不能早于开始日期")
	}
	return start, end, nil
}

func toAuditItem(e model.AuditEntry) AuditLogItem {
	return AuditLogItem{ID: e.ID, UserID: e.UserID, UserName: e.UserName, UserDisplay: e.UserDisplay, Action: e.Action,
		ExtraInfo: e.ExtraInfo, ActionTime: fmtTime(e.ActionTime)}
}

func (s *Service) auditFilter(q AuditQuery) (AuditLogFilter, error) {
	start, end, err := parseDateRange(q.StartDate, q.EndDate)
	if err != nil {
		return AuditLogFilter{}, err
	}
	return AuditLogFilter{Action: strings.TrimSpace(q.Action), User: strings.TrimSpace(q.User), Search: strings.TrimSpace(q.Search), Start: start, End: end}, nil
}

func (s *Service) auditExtFilter(q AuditQuery) (AuditExtFilter, error) {
	x := AuditExtFilter{Category: strings.TrimSpace(q.Category), Result: strings.TrimSpace(q.Result), Risky: q.Risky}
	if x.Category != "" && !audit.ValidCategory(x.Category) {
		return x, bad("未知的动作类别")
	}
	if x.Result != "" && !validResult(x.Result) {
		return x, bad("结果只能是 成功 / 失败 / 拒绝")
	}
	return x, nil
}

// fetchAudit 读一页通用审计：扩展表可用时带上扩展信息，否则退回基础字段（此时不能按类别 / 结果 / 高危筛选）。
func (s *Service) fetchAudit(f AuditLogFilter, x AuditExtFilter) ([]AuditLogItem, int64, error) {
	if r, ok := s.extRepo(); ok {
		rows, total, err := r.ListAuditLogsExt(f, x)
		if err != nil {
			return nil, 0, err
		}
		items := make([]AuditLogItem, len(rows))
		for i, e := range rows {
			items[i] = rowItem(e)
		}
		return items, total, nil
	}
	if x != (AuditExtFilter{}) {
		return nil, 0, conflict("审计扩展表还没有创建，暂不能按类别 / 结果 / 高危筛选（请先执行 deploy/sql/090_audit_ext.sql）")
	}
	rows, total, err := s.repo.ListAuditLogs(f)
	if err != nil {
		return nil, 0, err
	}
	items := make([]AuditLogItem, len(rows))
	for i, e := range rows {
		items[i] = toAuditItem(e)
	}
	return items, total, nil
}

// ListAuditLogs 分页返回通用审计（新的在前）。
func (s *Service) ListAuditLogs(q AuditQuery) (*Paged[AuditLogItem], error) {
	f, err := s.auditFilter(q)
	if err != nil {
		return nil, err
	}
	x, err := s.auditExtFilter(q)
	if err != nil {
		return nil, err
	}
	off, lim, page := Page(q.Page, q.PageSize)
	f.Offset, f.Limit = off, lim
	items, total, err := s.fetchAudit(f, x)
	if err != nil {
		return nil, err
	}
	return paged(items, total, page, lim), nil
}

// ExportAuditLogs 导出符合条件的通用审计（最多 50000 行），并把这次导出写入审计。
func (s *Service) ExportAuditLogs(a audit.Actor, q AuditQuery) (rows []AuditLogItem, truncated bool, err error) {
	f, err := s.auditFilter(q)
	if err != nil {
		return nil, false, err
	}
	x, err := s.auditExtFilter(q)
	if err != nil {
		return nil, false, err
	}
	for off := 0; off < maxExportRows; off += exportPageSize {
		f.Offset, f.Limit = off, exportPageSize
		page, total, err := s.fetchAudit(f, x)
		if err != nil {
			return nil, false, err
		}
		rows = append(rows, page...)
		if int64(off+len(page)) >= total || len(page) == 0 {
			break
		}
		if off+exportPageSize >= maxExportRows {
			truncated = true
		}
	}
	extra := errf("导出 %d 行；条件：动作=%s 用户=%s 关键词=%s 日期=%s~%s", len(rows), q.Action, q.User, q.Search, q.StartDate, q.EndDate) // i18n-ignore 存库的审计说明
	if x.Category != "" || x.Result != "" || x.Risky {
		extra += errf(" 类别=%s 结果=%s 仅高危=%v", x.Category, x.Result, x.Risky) // i18n-ignore 存库的审计说明
	}
	if truncated {
		extra += "（已截断到 50000 行）" // i18n-ignore 存库的审计说明
	}
	if err := s.writeAudit(s.repo, a, "导出审计日志", extra); err != nil {
		return nil, false, err
	}
	return rows, truncated, nil
}

// AuditActions 返回 dbt_audit_log 里出现过的动作（筛选下拉用；每分钟刷新一次，避免每次打开页面都全表 DISTINCT）。
func (s *Service) AuditActions() ([]string, error) {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	if s.actions != nil && s.now().Sub(s.actAt) < actionsCacheTTL {
		return s.actions, nil
	}
	list, err := s.repo.AuditActions()
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []string{}
	}
	s.actions, s.actAt = list, s.now()
	return list, nil
}

// ---- 查询审计 ----

// QueryAuditQuery 是查询审计（dbt_query_log）的查询条件。
type QueryAuditQuery struct {
	User         string
	Instance     string
	Search       string
	StartDate    string
	EndDate      string
	OnlyRejected bool // 仅看被拒绝的语句（dbt_query_reject）
	Page         int
	PageSize     int
}

// QueryLogItem 是查询审计的一行。SQL 过长时截断（SQLTruncated=true），完整 SQL 见 QueryAuditDetail。
// Kind=rejected 的行来自在线查询的“被拒绝语句”记录，没有行数 / 耗时，Reason 是拒绝原因。
type QueryLogItem struct {
	ID           uint   `json:"id"`
	Kind         string `json:"kind"`   // ok / rejected
	Status       string `json:"status"` // ok / masked（有敏感列被脱敏）/ rejected
	Reason       string `json:"reason"`
	InstanceName string `json:"instance_name"`
	DBName       string `json:"db_name"`
	SQL          string `json:"sql"`
	SQLTruncated bool   `json:"sql_truncated"`
	EffectRow    int64  `json:"effect_row"`
	CostTime     string `json:"cost_time"`
	Username     string `json:"username"`
	UserDisplay  string `json:"user_display"`
	Masking      bool   `json:"masking"`
	CreateTime   string `json:"create_time"`
}

// ListQueryLogs 分页返回在线查询审计（新的在前）。被拒绝的语句表（dbt_query_reject）存在时与成功记录合并，
// 不存在时只显示成功记录（reject_ready=false）。
func (s *Service) ListQueryLogs(q QueryAuditQuery) (*QueryAuditPage, error) {
	start, end, err := parseDateRange(q.StartDate, q.EndDate)
	if err != nil {
		return nil, err
	}
	off, lim, page := Page(q.Page, q.PageSize)
	f := QueryLogFilter{User: strings.TrimSpace(q.User), Instance: strings.TrimSpace(q.Instance),
		Search: strings.TrimSpace(q.Search), Start: start, End: end, Offset: off, Limit: lim}
	if _, ok := s.rejectRepo(); ok || q.OnlyRejected {
		return s.listQueryAudit(q, f, off, lim, page)
	}
	rows, total, err := s.repo.ListQueryLogs(f)
	if err != nil {
		return nil, err
	}
	items := make([]QueryLogItem, len(rows))
	for i, l := range rows {
		items[i] = logItem(l)
	}
	return &QueryAuditPage{Paged: paged(items, total, page, lim)}, nil
}

// ---- SQL 上线审计 ----

// SQL 工单状态的显示名。
var sqlStatusLabels = []struct{ Value, Label string }{
	{model.WfManReviewing, "等待审批"}, {model.WfReviewPass, "审核通过"}, {model.WfTimingTask, "定时执行"}, {model.WfQueuing, "排队中"},
	{model.WfExecuting, "执行中"}, {model.WfFinish, "已正常结束"}, {model.WfException, "执行有异常"}, {model.WfAbort, "人工终止流程"},
	{model.WfAutoReviewWrong, "自动审批不通过"},
}

func sqlStatusLabel(v string) string {
	for _, s := range sqlStatusLabels {
		if s.Value == v {
			return s.Label
		}
	}
	return v
}

func syntaxLabel(t int) string {
	switch t {
	case 1:
		return "DDL"
	case 2:
		return "DML"
	case 3:
		return "离线导出"
	}
	return "其他"
}

// SQLWorkflowQuery 是 SQL 上线审计的查询条件。
type SQLWorkflowQuery struct {
	Status    string
	GroupID   uint
	Search    string
	StartDate string
	EndDate   string
	Page      int
	PageSize  int
}

// SQLWorkflowItem 是 SQL 上线审计的一行（详情走 /sqlworkflow/:id）。
type SQLWorkflowItem struct {
	ID              uint   `json:"id"`
	Name            string `json:"name"`
	Engineer        string `json:"engineer"`
	EngineerDisplay string `json:"engineer_display"`
	Status          string `json:"status"`
	StatusLabel     string `json:"status_label"`
	GroupName       string `json:"group_name"`
	InstanceName    string `json:"instance_name"`
	DBName          string `json:"db_name"`
	SyntaxType      string `json:"syntax_type"`
	CreateTime      string `json:"create_time"`
	FinishTime      string `json:"finish_time"`
}

// ListSQLWorkflows 分页返回全部 SQL 上线工单（审计视角，新的在前）。
func (s *Service) ListSQLWorkflows(q SQLWorkflowQuery) (*Paged[SQLWorkflowItem], error) {
	if q.Status != "" && sqlStatusLabel(q.Status) == q.Status {
		return nil, bad("未知的工单状态")
	}
	start, end, err := parseDateRange(q.StartDate, q.EndDate)
	if err != nil {
		return nil, err
	}
	off, lim, page := Page(q.Page, q.PageSize)
	rows, total, err := s.repo.ListSQLWorkflows(SQLWorkflowFilter{Status: q.Status, GroupID: q.GroupID, Search: strings.TrimSpace(q.Search),
		Start: start, End: end, Offset: off, Limit: lim})
	if err != nil {
		return nil, err
	}
	items := make([]SQLWorkflowItem, len(rows))
	for i, w := range rows {
		items[i] = SQLWorkflowItem{ID: w.ID, Name: w.Name, Engineer: w.Engineer, EngineerDisplay: w.EngineerDisplay, Status: w.Status,
			StatusLabel: sqlStatusLabel(w.Status), GroupName: w.GroupName, InstanceName: w.InstanceName, DBName: w.DBName,
			SyntaxType: syntaxLabel(w.SyntaxType), CreateTime: fmtTime(w.CreateTime), FinishTime: fmtTimePtr(w.FinishTime)}
	}
	return paged(items, total, page, lim), nil
}

// AuditOptions 是系统审计页的筛选选项。
type AuditOptions struct {
	Actions     []string  `json:"actions"`
	SQLStatuses []IDLabel `json:"sql_statuses"`
	ResGroups   []IDName  `json:"resource_groups"`
	Categories  []IDLabel `json:"categories"`   // 动作类别（审计扩展表不可用时页面禁用对应筛选）
	Results     []string  `json:"results"`      // 结果取值
	ExtReady    bool      `json:"ext_ready"`    // dbt_audit_ext 可用：结果 / 来源 IP / 类别 / 权限码 / 高危
	RejectReady bool      `json:"reject_ready"` // dbt_query_reject 可用：查询审计里有被拒绝的语句
}

// IDLabel 是 值 + 显示名。
type IDLabel struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// AuditOptions 返回系统审计页的筛选选项。
func (s *Service) AuditOptions() (*AuditOptions, error) {
	actions, err := s.AuditActions()
	if err != nil {
		return nil, err
	}
	groups, err := s.repo.ListResourceGroups(ResGroupFilter{IncludeDeleted: true})
	if err != nil {
		return nil, err
	}
	out := &AuditOptions{Actions: actions, SQLStatuses: []IDLabel{}, ResGroups: []IDName{}, Categories: []IDLabel{}, Results: append([]string(nil), auditResults...)}
	for _, st := range sqlStatusLabels {
		out.SQLStatuses = append(out.SQLStatuses, IDLabel{st.Value, st.Label})
	}
	for _, g := range groups {
		out.ResGroups = append(out.ResGroups, IDName{g.GroupID, g.GroupName})
	}
	for _, c := range audit.Categories {
		out.Categories = append(out.Categories, IDLabel{c.Key, c.Label})
	}
	_, out.ExtReady = s.extRepo()
	_, out.RejectReady = s.rejectRepo()
	return out, nil
}
