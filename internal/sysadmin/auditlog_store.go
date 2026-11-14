// @author: kevinkelly
// @license: Apache Licence
// @file: auditlog_store.go
// @time: 2026/10/28

package sysadmin

import (
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// 系统审计扩展能力的 GORM 实现（auditExtRepo / queryAuditRepo / sqlStatsRepo）。
// 旁路表不存在时 Ready 返回 false，服务层随之退回基础行为；表的存在与否缓存 30 秒。

var (
	_ auditExtRepo   = (*store)(nil)
	_ queryAuditRepo = (*store)(nil)
	_ sqlStatsRepo   = (*store)(nil)
)

const audReadyTTL = 30 * time.Second

type audReadyCache struct {
	mu sync.Mutex
	at time.Time
	ok bool
}

var audTableReady sync.Map // "<table>" -> *audReadyCache（按表名缓存；全进程只有一个元数据库）

func (s *store) audHasTable(name string) bool {
	v, _ := audTableReady.LoadOrStore(name, &audReadyCache{})
	c := v.(*audReadyCache)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < audReadyTTL {
		return c.ok
	}
	db := s.db.Session(&gorm.Session{Logger: s.db.Logger.LogMode(logger.Silent)})
	c.ok, c.at = db.Migrator().HasTable(name), time.Now()
	return c.ok
}

func (s *store) AuditExtReady() bool    { return s.audHasTable(audit.ExtTable) }
func (s *store) QueryRejectReady() bool { return s.audHasTable(queryRejectTable) }

// queryRejectTable 是“在线查询”模块写入的被拒绝语句表（deploy/sql/050_query_reject.sql），本模块只读。
const queryRejectTable = "dbt_query_reject"

// ---- 通用审计（带扩展信息） ----

type auditScan struct {
	ID          uint      `gorm:"column:id"`
	UserID      uint      `gorm:"column:user_id"`
	UserName    string    `gorm:"column:user_name"`
	UserDisplay string    `gorm:"column:user_display"`
	Action      string    `gorm:"column:action"`
	ExtraInfo   string    `gorm:"column:extra_info"`
	ActionTime  time.Time `gorm:"column:action_time"`
	Result      *string   `gorm:"column:x_result"`
	IP          *string   `gorm:"column:x_ip"`
	Category    *string   `gorm:"column:x_category"`
	Module      *string   `gorm:"column:x_module"`
	Code        *string   `gorm:"column:x_code"`
	Risky       *bool     `gorm:"column:x_risky"`
}

const auditExtSelect = "a.id, a.user_id, a.user_name, a.user_display, a.action, a.extra_info, a.action_time, " +
	"x.result AS x_result, x.ip AS x_ip, x.category AS x_category, x.module AS x_module, x.code AS x_code, x.risky AS x_risky"

func audStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (a auditScan) row() AuditRowExt {
	return AuditRowExt{
		AuditEntry: model.AuditEntry{ID: a.ID, UserID: a.UserID, UserName: a.UserName, UserDisplay: a.UserDisplay, Action: a.Action, ExtraInfo: a.ExtraInfo, ActionTime: a.ActionTime},
		Result:     audStr(a.Result), IP: audStr(a.IP), Category: audStr(a.Category), Module: audStr(a.Module), Code: audStr(a.Code), Risky: a.Risky != nil && *a.Risky,
	}
}

func (s *store) auditExtBase() *gorm.DB {
	return s.db.Table("dbt_audit_log a").Joins("LEFT JOIN " + audit.ExtTable + " x ON x.audit_id = a.id")
}

func (s *store) ListAuditLogsExt(f AuditLogFilter, x AuditExtFilter) ([]AuditRowExt, int64, error) {
	q := s.auditExtBase()
	if f.Action != "" {
		q = q.Where("a.action = ?", f.Action)
	}
	if f.User != "" {
		q = q.Where("a.user_name LIKE ?", likePattern(f.User))
	}
	if f.Search != "" {
		p := likePattern(f.Search)
		q = q.Where("(a.user_name LIKE ? OR a.user_display LIKE ? OR a.action LIKE ? OR a.extra_info LIKE ? OR x.ip LIKE ? OR x.code LIKE ?)", p, p, p, p, p, p)
	}
	if f.Start != nil {
		q = q.Where("a.action_time >= ?", *f.Start)
	}
	if f.End != nil {
		q = q.Where("a.action_time < ?", *f.End)
	}
	if x.Category != "" {
		q = q.Where("x.category = ?", x.Category)
	}
	if x.Result != "" {
		q = q.Where("x.result = ?", x.Result)
	}
	if x.Risky {
		q = q.Where("x.risky = 1")
	}
	q = q.Session(&gorm.Session{}) // 之后 Count 与查询各自基于这套条件，互不污染
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var scan []auditScan
	err := q.Select(auditExtSelect).Order("a.action_time DESC, a.id DESC").Offset(f.Offset).Limit(f.Limit).Scan(&scan).Error
	if err != nil {
		return nil, 0, err
	}
	out := make([]AuditRowExt, len(scan))
	for i, r := range scan {
		out[i] = r.row()
	}
	return out, total, nil
}

func (s *store) GetAuditRowExt(id uint) (*AuditRowExt, error) {
	var scan []auditScan
	if err := s.auditExtBase().Select(auditExtSelect).Where("a.id = ?", id).Limit(1).Scan(&scan).Error; err != nil {
		return nil, err
	}
	if len(scan) == 0 {
		return nil, nil
	}
	r := scan[0].row()
	return &r, nil
}

func (s *store) RecentAuditsByUser(username string, excludeID uint, limit int) ([]AuditRowExt, error) {
	var scan []auditScan
	err := s.auditExtBase().Select(auditExtSelect).Where("a.user_name = ? AND a.id <> ?", username, excludeID).
		Order("a.action_time DESC, a.id DESC").Limit(limit).Scan(&scan).Error
	if err != nil {
		return nil, err
	}
	out := make([]AuditRowExt, len(scan))
	for i, r := range scan {
		out[i] = r.row()
	}
	return out, nil
}

// AuditOverviewData 汇总近 7 天 KPI、近 14 天按天条数、近 7 天最活跃用户。只扫最近 14 天的数据（action_time 条件）。
func (s *store) AuditOverviewData(now time.Time) (*AuditOverviewRaw, error) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	since14, since7 := today.AddDate(0, 0, -13), today.AddDate(0, 0, -6)
	out := &AuditOverviewRaw{Risky7: -1, Denied7: -1}

	var perDay []struct {
		D string
		N int64
	}
	if err := s.db.Raw("SELECT DATE_FORMAT(action_time, '%Y-%m-%d') AS d, COUNT(*) AS n FROM dbt_audit_log WHERE action_time >= ? GROUP BY d", since14).Scan(&perDay).Error; err != nil {
		return nil, err
	}
	m := map[string]int64{}
	for _, r := range perDay {
		m[r.D] = r.N
	}
	for i := 0; i < 14; i++ {
		d := since14.AddDate(0, 0, i).Format("2006-01-02")
		out.Days = append(out.Days, DayCount{Date: d, Count: m[d]})
		if !since14.AddDate(0, 0, i).Before(since7) {
			out.Events7 += m[d]
		}
	}
	for _, c := range []struct {
		action string
		dst    *int64
	}{{"登入失败", &out.LoginFailed}, {"账号锁定", &out.Lockouts}} { // i18n-ignore SQL 里按存库的动作名统计
		if err := s.db.Raw("SELECT COUNT(*) FROM dbt_audit_log WHERE action_time >= ? AND action = ?", since7, c.action).Scan(c.dst).Error; err != nil {
			return nil, err
		}
	}
	if s.AuditExtReady() {
		var n int64
		if err := s.db.Raw("SELECT COUNT(*) FROM dbt_audit_log a JOIN "+audit.ExtTable+" x ON x.audit_id = a.id WHERE a.action_time >= ? AND x.risky = 1", since7).Scan(&n).Error; err != nil {
			return nil, err
		}
		out.Risky7 = n
		if err := s.db.Raw("SELECT COUNT(*) FROM dbt_audit_log a JOIN "+audit.ExtTable+" x ON x.audit_id = a.id WHERE a.action_time >= ? AND x.result = ?", since7, audit.ResultDenied).Scan(&n).Error; err != nil {
			return nil, err
		}
		out.Denied7 = n
	}
	if err := s.db.Raw("SELECT user_name, MAX(user_display) AS user_display, COUNT(*) AS cnt FROM dbt_audit_log WHERE action_time >= ? AND user_name <> '' "+
		"GROUP BY user_name ORDER BY cnt DESC, user_name LIMIT 6", since7).Scan(&out.TopUsers).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// ---- SQL 上线状态分布 ----

func (s *store) SQLWorkflowStatusCounts(f SQLWorkflowFilter) (map[string]int64, error) {
	q := s.db.Table("dbt_sql_workflow w")
	if f.Status != "" {
		q = q.Where("w.status = ?", f.Status)
	}
	if f.GroupID != 0 {
		q = q.Where("w.group_id = ?", f.GroupID)
	}
	if f.Search != "" {
		p := likePattern(f.Search)
		q = q.Where("(w.workflow_name LIKE ? OR w.engineer_display LIKE ? OR w.engineer LIKE ?)", p, p, p)
	}
	if f.Start != nil {
		q = q.Where("w.create_time >= ?", *f.Start)
	}
	if f.End != nil {
		q = q.Where("w.create_time < ?", *f.End)
	}
	var rows []struct {
		Status string
		N      int64
	}
	if err := q.Select("w.status AS status, COUNT(*) AS n").Group("w.status").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, r := range rows {
		out[r.Status] = r.N
	}
	return out, nil
}

// ---- 查询审计：被拒绝的语句 + 汇总 ----

type rejectScan struct {
	ID           uint      `gorm:"column:id"`
	UserName     string    `gorm:"column:user_name"`
	InstanceID   uint      `gorm:"column:instance_id"`
	InstanceName *string   `gorm:"column:instance_name"`
	DBName       string    `gorm:"column:db_name"`
	SQLText      *string   `gorm:"column:sql_text"`
	Reason       string    `gorm:"column:reason"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

func (r rejectScan) row() RejectRow {
	return RejectRow{ID: r.ID, UserName: r.UserName, InstanceID: r.InstanceID, InstanceName: audStr(r.InstanceName), DBName: r.DBName,
		SQLText: audStr(r.SQLText), Reason: r.Reason, CreatedAt: r.CreatedAt}
}

const rejectSelect = "r.id, r.user_name, r.instance_id, i.instance_name AS instance_name, r.db_name, r.sql_text, r.reason, r.created_at"

func (s *store) rejectBase() *gorm.DB {
	return s.db.Table(queryRejectTable + " r").Joins("LEFT JOIN dbt_sql_instance i ON i.id = r.instance_id")
}

func (s *store) rejectWhere(q *gorm.DB, f QueryLogFilter) *gorm.DB {
	if f.User != "" {
		q = q.Where("r.user_name = ?", f.User)
	}
	if f.Instance != "" {
		q = q.Where("i.instance_name = ?", f.Instance)
	}
	if f.Search != "" {
		p := likePattern(f.Search)
		q = q.Where("(r.sql_text LIKE ? OR r.reason LIKE ? OR r.db_name LIKE ?)", p, p, p)
	}
	if f.Start != nil {
		q = q.Where("r.created_at >= ?", *f.Start)
	}
	if f.End != nil {
		q = q.Where("r.created_at < ?", *f.End)
	}
	return q
}

func (s *store) ListQueryRejects(f QueryLogFilter, limit int) ([]RejectRow, int64, error) {
	q := s.rejectWhere(s.rejectBase(), f)
	q = q.Session(&gorm.Session{})
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var scan []rejectScan
	if err := q.Select(rejectSelect).Order("r.created_at DESC, r.id DESC").Limit(limit).Scan(&scan).Error; err != nil {
		return nil, 0, err
	}
	out := make([]RejectRow, len(scan))
	for i, r := range scan {
		out[i] = r.row()
	}
	return out, total, nil
}

func (s *store) GetQueryReject(id uint) (*RejectRow, error) {
	var scan []rejectScan
	if err := s.rejectBase().Select(rejectSelect).Where("r.id = ?", id).Limit(1).Scan(&scan).Error; err != nil {
		return nil, err
	}
	if len(scan) == 0 {
		return nil, nil
	}
	r := scan[0].row()
	return &r, nil
}

func (s *store) GetQueryLog(id uint) (*model.QueryLog, error) {
	var rows []model.QueryLog
	if err := s.db.Where("id = ?", id).Limit(1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// QueryAuditStats 汇总成功记录（dbt_query_log）与被拒绝记录（dbt_query_reject，存在时）。cost_time 是文本，按数值计算。
func (s *store) QueryAuditStats(f QueryLogFilter) (*QueryStatsRaw, error) {
	q := s.db.Model(&model.QueryLog{})
	if f.User != "" {
		q = q.Where("username = ?", f.User)
	}
	if f.Instance != "" {
		q = q.Where("instance_name = ?", f.Instance)
	}
	if f.Search != "" {
		p := likePattern(f.Search)
		q = q.Where("(sqllog LIKE ? OR user_display LIKE ? OR alias LIKE ?)", p, p, p)
	}
	if f.Start != nil {
		q = q.Where("create_time >= ?", *f.Start)
	}
	if f.End != nil {
		q = q.Where("create_time < ?", *f.End)
	}
	q = q.Session(&gorm.Session{})
	var agg struct {
		N     int64
		Users int64
		Avg   *float64
		Max   *float64
	}
	if err := q.Select("COUNT(*) AS n, COUNT(DISTINCT username) AS users, AVG(CAST(cost_time AS DECIMAL(14,4))) AS avg, MAX(CAST(cost_time AS DECIMAL(14,4))) AS max").Scan(&agg).Error; err != nil {
		return nil, err
	}
	out := &QueryStatsRaw{Total: agg.N, UserCount: agg.Users}
	if agg.Avg != nil {
		out.AvgCost = *agg.Avg
	}
	if agg.Max != nil {
		out.MaxCost = *agg.Max
		var u []string
		if err := q.Where("CAST(cost_time AS DECIMAL(14,4)) = ?", *agg.Max).Order("id DESC").Limit(1).Pluck("username", &u).Error; err == nil && len(u) > 0 {
			out.MaxUser = u[0]
		}
	}
	if s.QueryRejectReady() {
		if err := s.rejectWhere(s.rejectBase(), f).Count(&out.Rejected).Error; err != nil {
			return nil, err
		}
	}
	return out, nil
}
