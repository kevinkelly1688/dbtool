// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package perf

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	drv "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"dbtool/internal/crypto"
	"dbtool/internal/engine"
	"dbtool/internal/instext"
	"dbtool/internal/model"
	"dbtool/internal/settings"
	"dbtool/internal/sshtunnel"
)

// store 是 Repo 的 GORM 实现，读写元数据库。基础表只读写既有列，不改结构。
type store struct {
	db        *gorm.DB
	m         *crypto.Mirage
	jobRunsOK atomic.Bool // dbt_perf_job_run 表存在后就不再查
	ext       extState    // 旁路表是否存在的缓存（store_ext.go）
}

func newStore(db *gorm.DB, m *crypto.Mirage) *store { return &store{db: db, m: m} }

var _ Repo = (*store)(nil)

// one 取一行，不存在返回 nil, nil。用 Limit(1).Find 而不是 First：GORM 对 First 查不到会打印“record not found”日志，采集建单的热路径上会刷屏。
func one[T any](db *gorm.DB) (*T, error) {
	var l []T
	if err := db.Limit(1).Find(&l).Error; err != nil {
		return nil, err
	}
	if len(l) == 0 {
		return nil, nil
	}
	return &l[0], nil
}

func isDup(err error) bool {
	var me *drv.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

func inList(l []string) string {
	q := make([]string, len(l))
	for i, s := range l {
		q[i] = "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return strings.Join(q, ",")
}

// SQL 片段里用到的状态集合（常量，不含用户输入）。
var (
	sqlOpen    = inList(openStatuses)
	sqlRunning = inList(slaRunning)
)

// ---- 工单 ----

const issueRowSelect = `dbt_perf_issue.*, i.instance_name AS instance_name, g.group_name AS team_name,
 COALESCE(NULLIF(a.display,''), a.username, '') AS assignee_name, COALESCE(NULLIF(d.display,''), d.username, '') AS dba_name`

func (s *store) issueRows() *gorm.DB {
	return s.db.Table("dbt_perf_issue").Select(issueRowSelect).
		Joins("LEFT JOIN dbt_sql_instance i ON i.id = dbt_perf_issue.instance_id").
		Joins("LEFT JOIN dbt_resource_group g ON g.group_id = dbt_perf_issue.team_id").
		Joins("LEFT JOIN dbt_sql_users a ON a.id = dbt_perf_issue.assignee_id").
		Joins("LEFT JOIN dbt_sql_users d ON d.id = dbt_perf_issue.dba_id")
}

func (s *store) GetIssue(id uint) (*Issue, error) {
	return one[Issue](s.db.Where("id = ?", id))
}

func (s *store) GetIssueRow(id uint) (*IssueRow, error) {
	var rows []IssueRow
	if err := s.issueRows().Where("dbt_perf_issue.id = ?", id).Limit(1).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func (s *store) FindIssue(instanceID uint, db, checksum string) (*Issue, error) {
	return one[Issue](s.db.Where("instance_id = ? AND db_name = ? AND checksum = ?", instanceID, db, checksum))
}

func (s *store) InsertIssue(i *Issue) error {
	if err := s.db.Create(i).Error; err != nil {
		if isDup(err) {
			return ErrDuplicate
		}
		return err
	}
	return nil
}

// 状态流转/配置类写入的列；影响指标（采集刷新）与占比不在其中，避免用旧值覆盖采集刚写入的新值。
var issueUpdateCols = []string{"category", "owner_side", "diagnosis", "suggestion", "suggested_ddl", "explain_result", "severity", "severity_locked",
	"status", "close_reason", "team_id", "assignee_id", "dba_id", "plan_finish_date", "fix_desc", "fixed_at", "accepted_at", "closed_at", "reopen_count",
	"baseline_avg_ms", "baseline_daily_sec", "pending_extend_date", "pending_reason", "extend_times", "accept_deadline", "resolve_deadline",
	"escalation_level", "last_remind_at", "remind_count", "sla_breached", "pre_warned", "sample_sql", "update_time"}

var issueMetricCols = []string{"exec_count_7d", "total_time_7d", "avg_time_ms", "max_time_ms", "rows_examined_avg", "rows_sent_avg", "impact_score",
	"per_day_count", "last_seen", "sample_sql"}

func (s *store) UpdateIssue(i *Issue, prev time.Time) error {
	now := time.Now()
	if !now.After(prev) {
		now = prev.Add(time.Microsecond)
	}
	upd := *i
	upd.UpdateTime = now
	res := s.db.Model(&Issue{}).Where("id = ? AND update_time = ?", i.ID, prev).Select(issueUpdateCols).Updates(&upd)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrConflict
	}
	i.UpdateTime = now
	return nil
}

func (s *store) UpdateIssueMetrics(i *Issue) error {
	return s.db.Model(&Issue{}).Where("id = ?", i.ID).Select(issueMetricCols).Updates(i).Error
}

var sortCols = map[string]string{"id": "dbt_perf_issue.id", "severity": "dbt_perf_issue.severity", "impact_score": "dbt_perf_issue.impact_score",
	"avg_time_ms": "dbt_perf_issue.avg_time_ms", "per_day_count": "dbt_perf_issue.per_day_count", "resolve_deadline": "dbt_perf_issue.resolve_deadline",
	"create_time": "dbt_perf_issue.create_time", "last_seen": "dbt_perf_issue.last_seen", "load_share": "dbt_perf_issue.load_share"}

func likeEscape(s string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
}

func (s *store) filtered(q IssueQuery) *gorm.DB {
	db := s.db.Table("dbt_perf_issue")
	switch q.View {
	case ViewTodo:
		db = db.Where("((dbt_perf_issue.assignee_id = ? AND dbt_perf_issue.status IN ("+sqlRunning+")) OR (dbt_perf_issue.dba_id = ? AND dbt_perf_issue.status IN ('fixed','waive_pending')))", q.UserID, q.UserID)
	case ViewTodoOverdue:
		db = db.Where("dbt_perf_issue.assignee_id = ? AND dbt_perf_issue.status IN ("+sqlRunning+") AND dbt_perf_issue.resolve_deadline < ?", q.UserID, q.Now)
	case ViewOpen:
		db = db.Where("dbt_perf_issue.status IN (" + sqlOpen + ")")
	case ViewOverdue:
		db = db.Where("dbt_perf_issue.status IN ("+sqlRunning+") AND dbt_perf_issue.resolve_deadline < ?", q.Now)
	case ViewFixed:
		db = db.Where("dbt_perf_issue.status = 'fixed'")
	case ViewClosed:
		db = db.Where("dbt_perf_issue.status = 'closed'")
	case ViewNew:
		db = db.Where("dbt_perf_issue.status = 'new'")
	case ViewAssigned:
		db = db.Where("dbt_perf_issue.status = 'assigned'")
	case ViewAccepted:
		db = db.Where("dbt_perf_issue.status = 'accepted'")
	case ViewApprove:
		db = db.Where(sqlApprove)
	}
	if q.Status != "" {
		db = db.Where("dbt_perf_issue.status = ?", q.Status)
	}
	if q.Source != "" {
		db = db.Where("dbt_perf_issue.source = ?", q.Source)
	}
	if q.Restrict {
		if len(q.TeamIDs) > 0 {
			db = db.Where("(dbt_perf_issue.team_id IN ? OR dbt_perf_issue.assignee_id = ?)", q.TeamIDs, q.UserID)
		} else {
			db = db.Where("dbt_perf_issue.assignee_id = ?", q.UserID)
		}
	}
	if q.Severity != "" {
		db = db.Where("dbt_perf_issue.severity = ?", q.Severity)
	}
	if q.OwnerSide != "" {
		db = db.Where("dbt_perf_issue.owner_side = ?", q.OwnerSide)
	}
	if q.Category != "" {
		db = db.Where("dbt_perf_issue.category = ?", q.Category)
	}
	if q.TeamID != 0 {
		db = db.Where("dbt_perf_issue.team_id = ?", q.TeamID)
	}
	if q.InstanceID != 0 {
		db = db.Where("dbt_perf_issue.instance_id = ?", q.InstanceID)
	}
	if q.Search != "" {
		like := "%" + likeEscape(q.Search) + "%"
		cond, args := "(dbt_perf_issue.fingerprint LIKE ? ESCAPE '!' OR dbt_perf_issue.db_name LIKE ? ESCAPE '!' OR dbt_perf_issue.tables LIKE ? ESCAPE '!'", []any{like, like, like}
		var id uint
		if _, err := fmt.Sscanf(strings.TrimPrefix(q.Search, "#"), "%d", &id); err == nil && id > 0 {
			cond += " OR dbt_perf_issue.id = ?"
			args = append(args, id)
		}
		db = db.Where(cond+")", args...)
	}
	return db
}

func (s *store) ListIssues(q IssueQuery) ([]IssueRow, int, error) {
	var total int64
	if err := s.filtered(q).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	col, ok := sortCols[q.Sort]
	if !ok {
		col = "dbt_perf_issue.impact_score"
	}
	dir := "ASC"
	if q.Desc {
		dir = "DESC"
	}
	var rows []IssueRow
	db := s.filtered(q).Select(issueRowSelect).
		Joins("LEFT JOIN dbt_sql_instance i ON i.id = dbt_perf_issue.instance_id").
		Joins("LEFT JOIN dbt_resource_group g ON g.group_id = dbt_perf_issue.team_id").
		Joins("LEFT JOIN dbt_sql_users a ON a.id = dbt_perf_issue.assignee_id").
		Joins("LEFT JOIN dbt_sql_users d ON d.id = dbt_perf_issue.dba_id").
		Order(col + " " + dir).Order("dbt_perf_issue.id DESC")
	if q.Limit > 0 {
		db = db.Limit(q.Limit).Offset(q.Offset)
	}
	if err := db.Scan(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, int(total), nil
}

func (s *store) IssuesByStatus(statuses []string) ([]Issue, error) {
	var out []Issue
	err := s.db.Where("status IN ?", statuses).Order("id").Find(&out).Error
	return out, err
}

func (s *store) AbsentIssues(statuses []string, source string, before time.Time) ([]Issue, error) {
	var out []Issue
	err := s.db.Where("status IN ? AND source = ? AND last_seen < ?", statuses, source, before).Order("id").Find(&out).Error
	return out, err
}

// RefreshLoadShare 占比 = 该问题近 7 天耗时 / 该实例所有被跟踪问题近 7 天耗时。
func (s *store) RefreshLoadShare(instanceID uint) error {
	return s.db.Exec(`UPDATE dbt_perf_issue p
JOIN (SELECT SUM(total_time_7d) AS s FROM dbt_perf_issue WHERE instance_id = ? AND total_time_7d > 0) t ON t.s > 0
SET p.load_share = ROUND(p.total_time_7d * 100.0 / t.s, 2)
WHERE p.instance_id = ? AND p.total_time_7d > 0`, instanceID, instanceID).Error
}

// ---- 日志与日指标 ----

func (s *store) AddLog(l *IssueLog) error { return s.db.Create(l).Error }

func (s *store) ListLogs(issueID uint, limit int) ([]IssueLog, error) {
	var out []IssueLog
	err := s.db.Where("issue_id = ?", issueID).Order("id DESC").Limit(limit).Find(&out).Error
	return out, err
}

func (s *store) AddStat(issueID uint, day time.Time, d StatDelta) error {
	return s.db.Exec(`INSERT INTO dbt_perf_issue_stat (issue_id, stat_date, exec_count, total_time, max_time_ms, rows_examined, rows_sent)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE exec_count = exec_count + VALUES(exec_count), total_time = total_time + VALUES(total_time),
 max_time_ms = GREATEST(max_time_ms, VALUES(max_time_ms)), rows_examined = rows_examined + VALUES(rows_examined), rows_sent = rows_sent + VALUES(rows_sent)`,
		issueID, day.Format(dateLayout), d.ExecCount, d.TotalTime, d.MaxTimeMs, d.RowsExamined, d.RowsSent).Error
}

func (s *store) ListStats(issueID uint, since time.Time) ([]IssueStat, error) {
	var out []IssueStat
	err := s.db.Where("issue_id = ? AND stat_date >= ?", issueID, since.Format(dateLayout)).Order("stat_date").Find(&out).Error
	return out, err
}

func (s *store) SumStats(issueID uint, since, until time.Time) (StatSum, error) {
	var r struct {
		Cnt   int64
		Total float64
		Mx    float64
		Rexam float64
		Rsent float64
	}
	q := s.db.Table("dbt_perf_issue_stat").
		Select("COALESCE(SUM(exec_count),0) AS cnt, COALESCE(SUM(total_time),0) AS total, COALESCE(MAX(max_time_ms),0) AS mx, COALESCE(SUM(rows_examined),0) AS rexam, COALESCE(SUM(rows_sent),0) AS rsent").
		Where("issue_id = ? AND stat_date >= ?", issueID, since.Format(dateLayout))
	if !until.IsZero() {
		q = q.Where("stat_date < ?", until.Format(dateLayout))
	}
	if err := q.Scan(&r).Error; err != nil {
		return StatSum{}, err
	}
	return StatSum{ExecCount: r.Cnt, TotalTime: r.Total, MaxTimeMs: r.Mx, RowsExamined: r.Rexam, RowsSent: r.Rsent}, nil
}

// ---- 排行榜聚合 ----

func (s *store) TeamAggregates(q BoardQuery) ([]TeamAgg, error) {
	closedIn := "p.closed_at >= ? AND p.closed_at < ? AND p.close_reason = 'verified'"
	sel := `p.team_id AS team_id, g.group_name AS team_name,
 COALESCE(SUM(p.status IN (` + sqlOpen + `)),0) AS open_cnt,
 COALESCE(SUM(p.status IN ('new','assigned')),0) AS not_accepted,
 COALESCE(SUM(p.status IN (` + sqlRunning + `) AND p.resolve_deadline < ?),0) AS overdue_cnt,
 COALESCE(SUM(p.status IN (` + sqlOpen + `) AND p.severity IN ('P0','P1')),0) AS p0p1_open,
 COALESCE(SUM(p.create_time >= ? AND p.create_time < ?),0) AS new_cnt,
 COALESCE(SUM(` + closedIn + `),0) AS closed_cnt,
 COALESCE(SUM(` + closedIn + ` AND p.sla_breached = 0),0) AS closed_on_time,
 COALESCE(SUM(CASE WHEN p.status IN (` + sqlOpen + `) THEN p.impact_score END),0) AS waste,
 COALESCE(AVG(CASE WHEN ` + closedIn + ` THEN TIMESTAMPDIFF(SECOND, p.create_time, COALESCE(p.fixed_at, p.closed_at)) / 86400 END),0) AS fix_days_avg`
	args := []any{q.Now, q.Start, q.End, q.Start, q.End, q.Start, q.End, q.Start, q.End}
	db := s.db.Table("dbt_perf_issue p").Select(sel, args...).Joins("JOIN dbt_resource_group g ON g.group_id = p.team_id").Where("p.team_id IS NOT NULL")
	if q.OwnerSide != "" {
		db = db.Where("p.owner_side = ?", q.OwnerSide)
	}
	var rows []struct {
		TeamID       uint
		TeamName     string
		OpenCnt      int
		NotAccepted  int
		OverdueCnt   int
		P0p1Open     int
		NewCnt       int
		ClosedCnt    int
		ClosedOnTime int
		Waste        float64
		FixDaysAvg   float64
	}
	if err := db.Group("p.team_id, g.group_name").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]TeamAgg, 0, len(rows))
	for _, r := range rows {
		out = append(out, TeamAgg{TeamID: r.TeamID, TeamName: r.TeamName, Open: r.OpenCnt, NotAccepted: r.NotAccepted, Overdue: r.OverdueCnt, P0P1Open: r.P0p1Open,
			New: r.NewCnt, Closed: r.ClosedCnt, ClosedOnTime: r.ClosedOnTime, Waste: r.Waste, FixDaysAvg: r.FixDaysAvg, FixDaysN: r.ClosedCnt})
	}
	return out, nil
}

func (s *store) OverallAggregate(q BoardQuery) (OverallAgg, error) {
	closedIn := "closed_at >= ? AND closed_at < ? AND close_reason = 'verified'"
	sel := `COALESCE(SUM(status IN (` + sqlOpen + `)),0) AS open_cnt,
 COALESCE(SUM(status IN (` + sqlRunning + `) AND resolve_deadline < ?),0) AS overdue_cnt,
 COALESCE(SUM(create_time >= ? AND create_time < ?),0) AS new_cnt,
 COALESCE(SUM(` + closedIn + `),0) AS closed_cnt,
 COALESCE(SUM(status IN (` + sqlOpen + `) AND owner_side = 'dba'),0) AS dba_open,
 COALESCE(SUM(CASE WHEN status IN (` + sqlOpen + `) THEN impact_score END),0) AS waste,
 COALESCE(SUM(CASE WHEN ` + closedIn + ` THEN baseline_daily_sec END),0) AS saved`
	var r struct {
		OpenCnt    int
		OverdueCnt int
		NewCnt     int
		ClosedCnt  int
		DbaOpen    int
		Waste      float64
		Saved      float64
	}
	if err := s.db.Table("dbt_perf_issue").Select(sel, q.Now, q.Start, q.End, q.Start, q.End, q.Start, q.End).Scan(&r).Error; err != nil {
		return OverallAgg{}, err
	}
	return OverallAgg{Open: r.OpenCnt, Overdue: r.OverdueCnt, New: r.NewCnt, Closed: r.ClosedCnt, DBAOpen: r.DbaOpen, Waste: r.Waste, Saved: r.Saved}, nil
}

func (s *store) WeeklyTrend(since time.Time) ([]WeekCount, error) {
	const weekOf = "DATE_FORMAT(DATE_SUB(DATE(%s), INTERVAL WEEKDAY(%s) DAY), '%%Y-%%m-%%d')"
	type wk struct {
		Week string
		N    int
	}
	var created, closed []wk
	if err := s.db.Raw("SELECT "+fmt.Sprintf(weekOf, "create_time", "create_time")+" AS week, COUNT(*) AS n FROM dbt_perf_issue WHERE create_time >= ? GROUP BY week", since).Scan(&created).Error; err != nil {
		return nil, err
	}
	if err := s.db.Raw("SELECT "+fmt.Sprintf(weekOf, "closed_at", "closed_at")+" AS week, COUNT(*) AS n FROM dbt_perf_issue WHERE closed_at >= ? AND close_reason = 'verified' GROUP BY week", since).Scan(&closed).Error; err != nil {
		return nil, err
	}
	m := map[string]*WeekCount{}
	get := func(w string) *WeekCount {
		if m[w] == nil {
			m[w] = &WeekCount{Week: w}
		}
		return m[w]
	}
	for _, c := range created {
		get(c.Week).Created = c.N
	}
	for _, c := range closed {
		get(c.Week).Closed = c.N
	}
	out := make([]WeekCount, 0, len(m))
	for _, w := range m {
		out = append(out, *w)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Week < out[b].Week })
	return out, nil
}

func (s *store) CategoryWaste() ([]CategoryAgg, error) {
	var out []CategoryAgg
	err := s.db.Raw("SELECT category, COUNT(*) AS count, COALESCE(SUM(impact_score),0) AS waste FROM dbt_perf_issue WHERE status IN (" + sqlOpen + ") GROUP BY category ORDER BY waste DESC").Scan(&out).Error
	return out, err
}

// ---- 团队 ----

func (s *store) members(table string, teamIDs []uint) (map[uint][]uint, error) {
	out := map[uint][]uint{}
	if len(teamIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		TeamID uint
		UserID uint
	}
	if err := s.db.Raw("SELECT perfteam_id AS team_id, users_id AS user_id FROM "+table+" WHERE perfteam_id IN ? ORDER BY id", teamIDs).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.TeamID] = append(out[r.TeamID], r.UserID)
	}
	return out, nil
}

func (s *store) fillTeams(ts []Team) ([]TeamConfig, error) {
	ids := make([]uint, len(ts))
	for i, t := range ts {
		ids[i] = t.ID
	}
	leaders, err := s.members("dbt_perf_team_leaders", ids)
	if err != nil {
		return nil, err
	}
	dirs, err := s.members("dbt_perf_team_directors", ids)
	if err != nil {
		return nil, err
	}
	out := make([]TeamConfig, len(ts))
	for i, t := range ts {
		out[i] = TeamConfig{Team: t, LeaderIDs: leaders[t.ID], DirectorIDs: dirs[t.ID]}
	}
	return out, nil
}

func (s *store) ListTeams() ([]TeamConfig, error) {
	var ts []Team
	if err := s.db.Order("id").Find(&ts).Error; err != nil {
		return nil, err
	}
	return s.fillTeams(ts)
}

func (s *store) oneTeam(where string, arg any) (*TeamConfig, error) {
	var ts []Team
	if err := s.db.Where(where, arg).Limit(1).Find(&ts).Error; err != nil || len(ts) == 0 {
		return nil, err
	}
	out, err := s.fillTeams(ts)
	if err != nil {
		return nil, err
	}
	return &out[0], nil
}

func (s *store) GetTeamByGroup(groupID uint) (*TeamConfig, error) {
	return s.oneTeam("resource_group_id = ?", groupID)
}
func (s *store) GetTeam(id uint) (*TeamConfig, error) { return s.oneTeam("id = ?", id) }

func (s *store) SaveTeam(t *TeamConfig) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if t.ID == 0 {
			if err := tx.Create(&t.Team).Error; err != nil {
				return err
			}
		} else if err := tx.Model(&Team{}).Where("id = ?", t.ID).Select("default_assignee_id", "enabled", "remark").Updates(&t.Team).Error; err != nil {
			return err
		}
		for table, ids := range map[string][]uint{"dbt_perf_team_leaders": t.LeaderIDs, "dbt_perf_team_directors": t.DirectorIDs} {
			if err := tx.Exec("DELETE FROM "+table+" WHERE perfteam_id = ?", t.ID).Error; err != nil {
				return err
			}
			for _, u := range ids {
				if err := tx.Exec("INSERT INTO "+table+" (perfteam_id, users_id) VALUES (?, ?)", t.ID, u).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *store) DeleteTeam(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		for _, table := range []string{"dbt_perf_team_leaders", "dbt_perf_team_directors"} {
			if err := tx.Exec("DELETE FROM "+table+" WHERE perfteam_id = ?", id).Error; err != nil {
				return err
			}
		}
		return tx.Delete(&Team{}, id).Error
	})
}

// ---- 路由、SLA、采集配置 ----

func (s *store) ListRoutes() ([]Route, error) {
	var out []Route
	err := s.db.Order("priority, id").Find(&out).Error
	return out, err
}

func (s *store) GetRoute(id uint) (*Route, error) {
	return one[Route](s.db.Where("id = ?", id))
}

func (s *store) SaveRoute(r *Route) error {
	if r.ID == 0 {
		return s.db.Create(r).Error
	}
	return s.db.Model(&Route{}).Where("id = ?", r.ID).Select("instance_id", "db_regex", "team_id", "assignee_id", "priority").Updates(r).Error
}

func (s *store) DeleteRoute(id uint) error { return s.db.Delete(&Route{}, id).Error }

func (s *store) ListSLA() ([]SLAPolicy, error) {
	var out []SLAPolicy
	err := s.db.Order("id").Find(&out).Error
	return out, err
}

func (s *store) SaveSLA(ps []SLAPolicy) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		for _, p := range ps {
			res := tx.Model(&SLAPolicy{}).Where("severity = ?", p.Severity).
				Select("accept_hours", "resolve_hours", "l3_after_hours", "remind_interval_hours", "max_extend_times").Updates(&p)
			if res.Error != nil {
				return res.Error
			}
			var n int64
			if err := tx.Model(&SLAPolicy{}).Where("severity = ?", p.Severity).Count(&n).Error; err != nil {
				return err
			}
			if n == 0 {
				p.ID = 0
				if err := tx.Create(&p).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *store) ListCollectConfigs() ([]CollectConfig, error) {
	var out []CollectConfig
	err := s.db.Order("id").Find(&out).Error
	return out, err
}

func (s *store) oneCollect(where string, arg any) (*CollectConfig, error) {
	return one[CollectConfig](s.db.Where(where, arg))
}

func (s *store) GetCollectConfig(id uint) (*CollectConfig, error) { return s.oneCollect("id = ?", id) }
func (s *store) GetCollectConfigByInstance(instanceID uint) (*CollectConfig, error) {
	return s.oneCollect("instance_id = ?", instanceID)
}

func (s *store) SaveCollectConfig(c *CollectConfig) error {
	if c.ID == 0 {
		if err := s.db.Create(c).Error; err != nil {
			if isDup(err) {
				return ErrDuplicate
			}
			return err
		}
		return nil
	}
	// 编辑时不写采集进度（last_collect_time / last_status 由采集任务维护）
	return s.db.Model(&CollectConfig{}).Where("id = ?", c.ID).
		Select("product", "collector", "custom_sql", "min_avg_ms", "min_total_sec", "exclude_regex", "exclude_dbs", "team_id", "enabled").Updates(c).Error
}

func (s *store) DeleteCollectConfig(id uint) error { return s.db.Delete(&CollectConfig{}, id).Error }

func (s *store) LockCollect(id uint, token, staleBefore string) (bool, error) {
	res := s.db.Exec("UPDATE dbt_perf_collect_config SET last_status = ? WHERE id = ? AND (last_status NOT LIKE 'RUNNING@%' OR last_status < ?)", token, id, staleBefore)
	return res.RowsAffected == 1, res.Error
}

func (s *store) FinishCollect(id uint, end *time.Time, status string) error {
	vals := map[string]any{"last_status": status}
	if end != nil {
		vals["last_collect_time"] = *end
	}
	return s.db.Model(&CollectConfig{}).Where("id = ?", id).Updates(vals).Error
}

// ---- 游标 ----

func (s *store) GetCursor(instanceID uint, schema, digest string) (*DigestCursor, error) {
	return one[DigestCursor](s.db.Where("instance_id = ? AND schema_name = ? AND digest = ?", instanceID, schema, digest))
}

func (s *store) SetCursor(c *DigestCursor) error {
	cur, err := s.GetCursor(c.InstanceID, c.SchemaName, c.Digest)
	if err != nil {
		return err
	}
	now := time.Now()
	if cur == nil {
		c.UpdateTime = now
		return s.db.Create(c).Error
	}
	return s.db.Model(&DigestCursor{}).Where("id = ?", cur.ID).Updates(map[string]any{"count_star": c.CountStar, "sum_timer_wait": c.SumTimerWait,
		"sum_rows_examined": c.SumRowsExamined, "sum_rows_sent": c.SumRowsSent, "update_time": now}).Error
}

func (s *store) ListCursors(instanceID uint) ([]DigestCursor, error) {
	var out []DigestCursor
	err := s.db.Where("instance_id = ? AND schema_name <> ?", instanceID, slowCursorSchema).Find(&out).Error
	return out, err
}

func (s *store) SaveCursors(creates, updates []DigestCursor) error {
	now := time.Now()
	return s.db.Transaction(func(tx *gorm.DB) error {
		for i := range creates {
			creates[i].UpdateTime = now
		}
		for i := range updates {
			updates[i].UpdateTime = now
		}
		if len(creates) > 0 {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(creates, 500).Error; err != nil {
				return err
			}
		}
		if len(updates) > 0 {
			err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}},
				DoUpdates: clause.AssignmentColumns([]string{"count_star", "sum_timer_wait", "sum_rows_examined", "sum_rows_sent", "update_time"})}).
				CreateInBatches(updates, 500).Error
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- 后台任务运行记录 ----

func (s *store) JobRunsReady() bool {
	if s.jobRunsOK.Load() {
		return true
	}
	if s.db.Migrator().HasTable("dbt_perf_job_run") {
		s.jobRunsOK.Store(true)
		return true
	}
	return false
}

func (s *store) ClaimJobRun(job, key string, now time.Time) (bool, error) {
	res := s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&JobRun{Job: job, RunKey: key, StartedAt: now})
	return res.RowsAffected == 1, res.Error
}

func (s *store) FinishJobRun(job, key string, now time.Time, result string) error {
	return s.db.Model(&JobRun{}).Where("job = ? AND run_key = ?", job, key).Updates(map[string]any{"finished_at": now, "result": result}).Error
}

// ---- 目录 ----

func briefOf(u model.User) UserBrief {
	return UserBrief{ID: u.ID, Username: u.Username, Display: u.Display, Active: u.IsActive, DingID: u.DingUserID, FeishuID: u.FeishuOpenID, SlackID: u.SlackID}
}

const userCols = "id, username, display, is_active, ding_user_id, feishu_open_id, slack_id"

func (s *store) GetUsers(ids []uint) (map[uint]UserBrief, error) {
	out := map[uint]UserBrief{}
	if len(ids) == 0 {
		return out, nil
	}
	var us []model.User
	if err := s.db.Select(userCols).Where("id IN ?", ids).Find(&us).Error; err != nil {
		return nil, err
	}
	for _, u := range us {
		out[u.ID] = briefOf(u)
	}
	return out, nil
}

func (s *store) ListActiveUsers() ([]UserBrief, error) {
	var us []model.User
	if err := s.db.Select(userCols).Where("is_active = 1").Order("username").Find(&us).Error; err != nil {
		return nil, err
	}
	out := make([]UserBrief, 0, len(us))
	for _, u := range us {
		out = append(out, briefOf(u))
	}
	return out, nil
}

func (s *store) UsersByNames(names []string) ([]UserBrief, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var us []model.User
	if err := s.db.Select(userCols).Where("username IN ? AND is_active = 1", names).Find(&us).Error; err != nil {
		return nil, err
	}
	out := make([]UserBrief, 0, len(us))
	for _, u := range us {
		out = append(out, briefOf(u))
	}
	return out, nil
}

func (s *store) UserGroupIDs(userID uint) ([]uint, error) {
	var ids []uint
	err := s.db.Raw(`SELECT ur.resourcegroup_id FROM dbt_sql_users_resource_group ur
JOIN dbt_resource_group rg ON rg.group_id = ur.resourcegroup_id AND rg.is_deleted = 0
WHERE ur.users_id = ? ORDER BY ur.resourcegroup_id`, userID).Scan(&ids).Error
	return ids, err
}

func (s *store) ListGroups() ([]GroupBrief, error) {
	var gs []model.ResourceGroup
	if err := s.db.Where("is_deleted = 0").Order("group_id").Find(&gs).Error; err != nil {
		return nil, err
	}
	hooks := map[uint][]string{}
	if s.db.Migrator().HasTable("dbt_notify_channel") {
		var rows []struct {
			GroupID uint
			Type    string
		}
		if err := s.db.Raw("SELECT group_id, channel_type AS type FROM dbt_notify_channel WHERE enabled = 1 AND group_id > 0").Scan(&rows).Error; err == nil {
			for _, r := range rows {
				hooks[r.GroupID] = append(hooks[r.GroupID], r.Type)
			}
		}
	}
	label := map[string]string{"dingtalk": "钉钉", "wecom": "企业微信", "feishu": "飞书", "lark": "Lark", "slack": "Slack", "telegram": "Telegram"}
	out := make([]GroupBrief, 0, len(gs))
	for _, g := range gs {
		set := map[string]bool{}
		if g.DingWebhook != "" {
			set["钉钉"] = true
		}
		if g.FeishuWebhook != "" {
			set["飞书"] = true
		}
		if g.SlackWebhook != "" {
			set["Slack"] = true
		}
		for _, t := range hooks[g.GroupID] {
			set[orDefault(label[t], t)] = true
		}
		var names []string
		for n := range set {
			names = append(names, n)
		}
		sort.Strings(names)
		out = append(out, GroupBrief{ID: g.GroupID, Name: g.GroupName, Webhooks: names})
	}
	return out, nil
}

func (s *store) InstanceGroupIDs(instanceID uint) ([]uint, error) {
	var ids []uint
	err := s.db.Raw(`SELECT ir.resourcegroup_id FROM dbt_sql_instance_resource_group ir
JOIN dbt_resource_group rg ON rg.group_id = ir.resourcegroup_id AND rg.is_deleted = 0
WHERE ir.instance_id = ? ORDER BY ir.resourcegroup_id`, instanceID).Scan(&ids).Error
	return ids, err
}

func (s *store) UserInstanceIDs(userID uint) (map[uint]bool, error) {
	var ids []uint
	err := s.db.Raw(`SELECT DISTINCT ir.instance_id FROM dbt_sql_instance_resource_group ir
JOIN dbt_resource_group rg ON rg.group_id = ir.resourcegroup_id AND rg.is_deleted = 0
JOIN dbt_sql_users_resource_group ur ON ur.resourcegroup_id = rg.group_id
WHERE ur.users_id = ?`, userID).Scan(&ids).Error
	out := make(map[uint]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, err
}

func (s *store) ListInstances(dbTypes []string) ([]InstanceBrief, error) {
	var out []InstanceBrief
	// 实例管理里被停用的实例（旁路表 dbt_instance_ext）不再出现；表不存在时不过滤。
	err := s.db.Raw("SELECT id, instance_name AS name, db_type FROM dbt_sql_instance WHERE db_type IN ?"+instext.DisabledCond(s.db, "dbt_sql_instance.id")+" ORDER BY instance_name", dbTypes).Scan(&out).Error
	return out, err
}

func (s *store) GetInstance(id uint) (*model.Instance, error) {
	return one[model.Instance](s.db.Where("id = ?", id))
}

// EngineConn 解密凭据，构造连接信息（同 store.Store.EngineConn；配置了 SSH 隧道的实例经本地转发器连接）。
func (s *store) EngineConn(inst *model.Instance) engine.Conn {
	c := engine.Conn{Host: inst.Host, Port: inst.Port, User: s.m.Decrypt(inst.User), Password: s.m.Decrypt(inst.Password), DBName: inst.DBName,
		Charset: inst.Charset, SSL: inst.IsSSL, VerifySSL: inst.VerifySSL, ServiceName: inst.ServiceNameValue(), SID: inst.SIDValue()}
	if sshtunnel.Enabled(inst) {
		c = sshtunnel.NewGormResolver(s.db, s.m.Decrypt).Apply(inst, c)
	}
	return c
}

func (s *store) DatabaseOwner(instanceID uint, db string) (string, error) {
	var owner []string
	err := s.db.Raw("SELECT owner FROM dbt_instance_database WHERE instance_id = ? AND db_name = ? AND owner <> '' LIMIT 1", instanceID, db).Scan(&owner).Error
	if err != nil || len(owner) == 0 {
		return "", err
	}
	return owner[0], nil
}

func (s *store) DBALoads(usernames []string) ([]UserLoad, error) {
	var rows []struct {
		ID       uint
		Username string
		Display  string
		N        int
	}
	err := s.db.Raw(`SELECT u.id, u.username, u.display, COUNT(p.id) AS n FROM dbt_sql_users u
LEFT JOIN dbt_perf_issue p ON p.dba_id = u.id AND p.status IN (`+sqlOpen+`)
WHERE u.username IN ? AND u.is_active = 1 GROUP BY u.id, u.username, u.display`, usernames).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]UserLoad, 0, len(rows))
	for _, r := range rows {
		out = append(out, UserLoad{User: UserBrief{ID: r.ID, Username: r.Username, Display: r.Display, Active: true}, Open: r.N})
	}
	return out, nil
}

func (s *store) ConfigValue(item string) (string, bool) {
	return settings.Open(s.db, s.m).Value(item)
}

func (s *store) InsertAudit(e *model.AuditEntry) error { return s.db.Create(e).Error }

// ---- 元库里的慢日志（只读） ----

type slowRow struct {
	ID           uint
	DBName       string
	SQLText      string
	QueryTimeSec float64
	RowsExamined float64
	RowsSent     float64
	StartTime    sql.NullTime
}

const slowCols = "id, COALESCE(db_name,'') AS db_name, COALESCE(sql_text,'') AS sql_text, COALESCE(query_time_sec,0) AS query_time_sec, COALESCE(rows_examined,0) AS rows_examined, COALESCE(rows_sent,0) AS rows_sent, start_time"

func toSlowRows(rs []slowRow) []SlowLogRow {
	out := make([]SlowLogRow, 0, len(rs))
	for _, r := range rs {
		row := SlowLogRow{ID: r.ID, DBName: r.DBName, SQLText: r.SQLText, QueryTimeSec: r.QueryTimeSec, RowsExamined: r.RowsExamined, RowsSent: r.RowsSent}
		if r.StartTime.Valid {
			t := r.StartTime.Time
			row.StartTime = &t
		}
		out = append(out, row)
	}
	return out
}

func (s *store) SlowLogsAfter(instanceID, afterID uint, limit int) ([]SlowLogRow, error) {
	var rs []slowRow
	err := s.db.Raw("SELECT "+slowCols+" FROM dbt_slow_logs WHERE instance_id = ? AND id > ? ORDER BY id LIMIT ?", instanceID, afterID, limit).Scan(&rs).Error
	return toSlowRows(rs), err
}

func (s *store) SlowLogsCreatedBetween(instanceID uint, start, end time.Time, limit int) ([]SlowLogRow, error) {
	var rs []slowRow
	err := s.db.Raw("SELECT "+slowCols+" FROM dbt_slow_logs WHERE instance_id = ? AND created_at >= ? AND created_at < ? ORDER BY id LIMIT ?", instanceID, start, end, limit).Scan(&rs).Error
	return toSlowRows(rs), err
}

func (s *store) MaxSlowLogID(instanceID uint, before time.Time) (uint, error) {
	var id uint
	err := s.db.Raw("SELECT COALESCE(MAX(id),0) FROM dbt_slow_logs WHERE instance_id = ? AND created_at < ?", instanceID, before).Scan(&id).Error
	return id, err
}

type ptRow struct {
	DBMax   string
	Sample  string
	Cnt     float64
	Total   float64
	Qmax    float64
	Rexam   float64
	Rsent   float64
	FirstTs sql.NullTime
	LastTs  sql.NullTime
}

func (s *store) PtDigests(hostPort string, start, end time.Time) ([]Digest, error) {
	if !s.db.Migrator().HasTable("dbt_mysql_slow_query_review_history") {
		return nil, errors.New("pt-query-digest 汇总表 dbt_mysql_slow_query_review_history 不存在，请先部署 pt-query-digest 推送脚本")
	}
	var rs []ptRow
	err := s.db.Raw("SELECT COALESCE(db_max,'') AS db_max, MAX(sample) AS sample, COALESCE(SUM(ts_cnt),0) AS cnt, COALESCE(SUM(Query_time_sum),0) AS total,"+
		" COALESCE(MAX(Query_time_max),0) AS qmax, COALESCE(SUM(Rows_examined_sum),0) AS rexam, COALESCE(SUM(Rows_sent_sum),0) AS rsent,"+
		" MIN(ts_min) AS first_ts, MAX(ts_max) AS last_ts FROM dbt_mysql_slow_query_review_history"+
		" WHERE hostname_max = ? AND ts_min >= ? AND ts_min < ? GROUP BY checksum, db_max", hostPort, start, end).Scan(&rs).Error
	if err != nil {
		return nil, err
	}
	out := make([]Digest, 0, len(rs))
	for _, r := range rs {
		d := Digest{DBName: r.DBMax, SampleSQL: r.Sample, ExecCount: int64(r.Cnt), TotalSec: r.Total, MaxMs: r.Qmax * 1000, RowsExamined: r.Rexam, RowsSent: r.Rsent, SampleReal: true}
		if r.FirstTs.Valid {
			t := r.FirstTs.Time
			d.FirstSeen = &t
		}
		if r.LastTs.Valid {
			t := r.LastTs.Time
			d.LastSeen = &t
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *store) SlowLogByMD5(instanceID uint, md5, db string, since time.Time) (*SlowSample, []Digest, error) {
	var samples []struct {
		DBName  string
		SQLText string
	}
	q := s.db.Table("dbt_slow_logs").Select("COALESCE(db_name,'') AS db_name, sql_text").Where("instance_id = ? AND sql_md5 = ? AND sql_text IS NOT NULL", instanceID, md5)
	if db != "" {
		q = q.Where("db_name = ?", db)
	}
	if err := q.Order("query_time_sec DESC, id DESC").Limit(1).Scan(&samples).Error; err != nil || len(samples) == 0 {
		return nil, nil, err
	}
	sample := &SlowSample{DBName: samples[0].DBName, SQLText: samples[0].SQLText}
	var days []struct {
		Cnt     float64
		Total   float64
		Qmax    float64
		Rexam   float64
		Rsent   float64
		FirstTs sql.NullTime
		LastTs  sql.NullTime
	}
	err := s.db.Raw(`SELECT COUNT(*) AS cnt, COALESCE(SUM(query_time_sec),0) AS total, COALESCE(MAX(query_time_sec),0) AS qmax,
 COALESCE(SUM(rows_examined),0) AS rexam, COALESCE(SUM(rows_sent),0) AS rsent, MIN(start_time) AS first_ts, MAX(start_time) AS last_ts
FROM dbt_slow_logs WHERE instance_id = ? AND sql_md5 = ? AND COALESCE(db_name,'') = ? AND start_time >= ? GROUP BY DATE(start_time)`,
		instanceID, md5, sample.DBName, since).Scan(&days).Error
	if err != nil {
		return nil, nil, err
	}
	out := make([]Digest, 0, len(days))
	for _, d := range days {
		dg := Digest{DBName: sample.DBName, SampleSQL: sample.SQLText, ExecCount: int64(d.Cnt), TotalSec: d.Total, MaxMs: d.Qmax * 1000,
			RowsExamined: d.Rexam, RowsSent: d.Rsent, SampleReal: true}
		if d.FirstTs.Valid {
			t := d.FirstTs.Time
			dg.FirstSeen = &t
		}
		if d.LastTs.Valid {
			t := d.LastTs.Time
			dg.LastSeen = &t
		}
		out = append(out, dg)
	}
	return sample, out, nil
}
