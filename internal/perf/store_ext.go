// @author: kevinkelly
// @license: Apache Licence
// @file: store_ext.go
// @time: 2026/10/28

package perf

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	drv "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"dbtool/internal/settings"
)

// store 对 ExtRepo 的实现：旁路表（060_perf_ext.sql）、dbt_setting 写入、列表计数。

var _ ExtRepo = (*store)(nil)

// 审批中：豁免审批中，或有待审批的延期申请（延期申请不改状态，只写 pending_extend_date）。
var sqlApprove = "(dbt_perf_issue.status = 'waive_pending' OR (dbt_perf_issue.pending_extend_date IS NOT NULL AND dbt_perf_issue.status IN (" + sqlOpen + ")))"

// extState 缓存旁路表是否存在：存在后不再查；不存在时每 30 秒重查一次（执行了建表脚本后不用重启就能生效）。
type extState struct {
	mu    sync.Mutex
	have  map[string]bool
	asked map[string]time.Time
}

const extRecheck = 30 * time.Second

func (s *store) ExtReady(table string) bool {
	st := &s.ext
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.have[table] {
		return true
	}
	if t, ok := st.asked[table]; ok && time.Since(t) < extRecheck {
		return false
	}
	ok := s.db.Migrator().HasTable(table)
	if st.have == nil {
		st.have, st.asked = map[string]bool{}, map[string]time.Time{}
	}
	st.asked[table] = time.Now()
	st.have[table] = ok
	return ok
}

func (s *store) GetIssueExt(issueID uint) (IssueExt, error) {
	var rows []struct {
		WorkflowID uint `gorm:"column:workflow_id"`
	}
	err := s.db.Raw("SELECT COALESCE(workflow_id,0) AS workflow_id FROM "+tblIssueExt+" WHERE issue_id = ? LIMIT 1", issueID).Scan(&rows).Error
	if err != nil {
		if isNoTable(err) {
			return IssueExt{}, nil
		}
		return IssueExt{}, err
	}
	if len(rows) == 0 {
		return IssueExt{}, nil
	}
	return IssueExt{WorkflowID: rows[0].WorkflowID}, nil
}

func (s *store) SaveIssueExt(issueID uint, e IssueExt) error {
	var err error
	if e.WorkflowID == 0 {
		err = s.db.Exec("DELETE FROM "+tblIssueExt+" WHERE issue_id = ?", issueID).Error
	} else {
		err = s.db.Exec("INSERT INTO "+tblIssueExt+" (issue_id, workflow_id, updated_at) VALUES (?,?,?) ON DUPLICATE KEY UPDATE workflow_id = VALUES(workflow_id), updated_at = VALUES(updated_at)",
			issueID, e.WorkflowID, time.Now()).Error
	}
	if isNoTable(err) {
		return ErrExtMissing
	}
	return err
}

func (s *store) WorkflowExists(id uint) (bool, error) {
	var n int64
	err := s.db.Raw("SELECT COUNT(*) FROM dbt_sql_workflow WHERE id = ?", id).Scan(&n).Error
	return n > 0, err
}

func (s *store) ListRouteExt() (map[uint]string, error) {
	var rows []struct {
		RouteID  uint   `gorm:"column:route_id"`
		TblRegex string `gorm:"column:tbl_regex"`
	}
	err := s.db.Raw("SELECT route_id, tbl_regex FROM " + tblRouteExt + " LIMIT 5000").Scan(&rows).Error
	if err != nil {
		if isNoTable(err) {
			return map[uint]string{}, nil
		}
		return nil, err
	}
	out := make(map[uint]string, len(rows))
	for _, r := range rows {
		out[r.RouteID] = r.TblRegex
	}
	return out, nil
}

func (s *store) SaveRouteExt(routeID uint, tblRegex string) error {
	var err error
	if strings.TrimSpace(tblRegex) == "" {
		err = s.db.Exec("DELETE FROM "+tblRouteExt+" WHERE route_id = ?", routeID).Error
	} else {
		err = s.db.Exec("INSERT INTO "+tblRouteExt+" (route_id, tbl_regex, updated_at) VALUES (?,?,?) ON DUPLICATE KEY UPDATE tbl_regex = VALUES(tbl_regex), updated_at = VALUES(updated_at)",
			routeID, tblRegex, time.Now()).Error
	}
	if isNoTable(err) {
		return ErrExtMissing
	}
	return err
}

func (s *store) GetSetting(key string) (string, error) {
	var rows []struct {
		V string `gorm:"column:svalue"`
	}
	err := s.db.Raw("SELECT svalue FROM "+tblSetting+" WHERE skey = ? LIMIT 1", key).Scan(&rows).Error
	if err != nil {
		if isNoTable(err) {
			return "", nil
		}
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rows[0].V, nil
}

func (s *store) SetSetting(key, value, by string) error {
	err := s.db.Exec("INSERT INTO "+tblSetting+" (skey, svalue, updated_by, updated_at) VALUES (?,?,?,?) ON DUPLICATE KEY UPDATE svalue = VALUES(svalue), updated_by = VALUES(updated_by), updated_at = VALUES(updated_at)",
		key, value, truncate(by, 64), time.Now()).Error
	if isNoTable(err) {
		return ErrExtMissing
	}
	return err
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// SetConfigValue 写一项系统设置（dbt_setting）：校验后写入，空串 = 恢复默认。desc 只为兼容接口，不再使用。
func (s *store) SetConfigValue(item, value, desc string) error {
	svc := settings.Open(s.db, s.m)
	ws, hs, err := svc.Prepare("", map[string]string{item: value})
	if err != nil {
		return err
	}
	if len(ws) == 0 {
		return nil
	}
	err = s.db.Transaction(func(tx *gorm.DB) error { return settings.WriteTx(tx, ws, hs) })
	svc.Invalidate()
	return err
}

func isNoTable(err error) bool {
	var me *drv.MySQLError
	return errors.As(err, &me) && me.Number == 1146
}

// IssueCounts 在 q 的可见范围内（只用 Restrict / TeamIDs / UserID / Now）一次算出页签与 KPI 的全部计数。
func (s *store) IssueCounts(q IssueQuery) (IssueCounts, error) {
	base := IssueQuery{Restrict: q.Restrict, TeamIDs: q.TeamIDs, UserID: q.UserID, Now: q.Now}
	week := q.Now.AddDate(0, 0, -7)
	sel := "COUNT(*) AS all_cnt," +
		"COALESCE(SUM((dbt_perf_issue.assignee_id = ? AND dbt_perf_issue.status IN (" + sqlRunning + ")) OR (dbt_perf_issue.dba_id = ? AND dbt_perf_issue.status IN ('fixed','waive_pending'))),0) AS todo," +
		"COALESCE(SUM(dbt_perf_issue.assignee_id = ? AND dbt_perf_issue.status IN (" + sqlRunning + ") AND dbt_perf_issue.resolve_deadline < ?),0) AS todo_overdue," +
		"COALESCE(SUM(dbt_perf_issue.status IN (" + sqlOpen + ")),0) AS open_cnt," +
		"COALESCE(SUM(dbt_perf_issue.status IN (" + sqlRunning + ") AND dbt_perf_issue.resolve_deadline < ?),0) AS overdue," +
		"COALESCE(SUM(dbt_perf_issue.status = 'new'),0) AS new_cnt," +
		"COALESCE(SUM(dbt_perf_issue.status = 'assigned'),0) AS assigned_cnt," +
		"COALESCE(SUM(dbt_perf_issue.status = 'accepted'),0) AS accepted_cnt," +
		"COALESCE(SUM(dbt_perf_issue.status = 'fixed'),0) AS fixed_cnt," +
		"COALESCE(SUM(" + sqlApprove + "),0) AS approve_cnt," +
		"COALESCE(SUM(dbt_perf_issue.status = 'closed'),0) AS closed_cnt," +
		"COALESCE(SUM(dbt_perf_issue.create_time >= ?),0) AS created7," +
		"COALESCE(SUM(dbt_perf_issue.status = 'closed' AND dbt_perf_issue.closed_at >= ?),0) AS closed7"
	var r struct {
		AllCnt, Todo, TodoOverdue, OpenCnt, Overdue, NewCnt, AssignedCnt, AcceptedCnt, FixedCnt, ApproveCnt, ClosedCnt, Created7, Closed7 int
	}
	args := []any{q.UserID, q.UserID, q.UserID, q.Now, q.Now, week, week}
	if err := s.filtered(base).Select(sel, args...).Scan(&r).Error; err != nil {
		return IssueCounts{}, fmt.Errorf("统计工单数量失败: %w", err)
	}
	return IssueCounts{Todo: r.Todo, TodoOverdue: r.TodoOverdue, Open: r.OpenCnt, Overdue: r.Overdue, New: r.NewCnt, Assigned: r.AssignedCnt, Accepted: r.AcceptedCnt,
		Fixed: r.FixedCnt, Approve: r.ApproveCnt, Closed: r.ClosedCnt, All: r.AllCnt, Created7d: r.Created7, Closed7d: r.Closed7}, nil
}
