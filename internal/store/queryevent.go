// @author: kevinkelly
// @license: Apache Licence
// @file: queryevent.go
// @time: 2026/10/28

package store

import (
	"sort"
	"strconv"
	"sync"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/query"
)

// 在线查询的未成功记录（deploy/sql/050_query_reject.sql）。表不存在时 EventsReady 为 false，调用方降级。

type queryEventRow struct {
	ID           uint      `gorm:"column:id;primaryKey"`
	UserName     string    `gorm:"column:user_name"`
	UserDisplay  string    `gorm:"column:user_display"`
	InstanceID   uint      `gorm:"column:instance_id"`
	InstanceName string    `gorm:"column:instance_name"`
	DBName       string    `gorm:"column:db_name"`
	SQLText      string    `gorm:"column:sql_text"`
	Status       string    `gorm:"column:status"`
	Code         string    `gorm:"column:code"`
	Reason       string    `gorm:"column:reason"`
	CostMs       int64     `gorm:"column:cost_ms"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

func (queryEventRow) TableName() string { return "dbt_query_event" }

type queryRejectRow struct {
	ID         uint      `gorm:"column:id;primaryKey"`
	UserName   string    `gorm:"column:user_name"`
	InstanceID uint      `gorm:"column:instance_id"`
	DBName     string    `gorm:"column:db_name"`
	SQLText    string    `gorm:"column:sql_text"`
	Reason     string    `gorm:"column:reason"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

func (queryRejectRow) TableName() string { return "dbt_query_reject" }

var evReady struct {
	mu    sync.Mutex
	ok    bool
	until time.Time // 上次检查为“不存在”时，到这个时间之前不再重复检查
}

// EventsReady 报告 dbt_query_event 是否存在。存在后不再检查；不存在时每 60 秒重查一次（执行 DDL 后无需重启即可生效）。
func (s *Store) EventsReady() bool {
	evReady.mu.Lock()
	defer evReady.mu.Unlock()
	if evReady.ok {
		return true
	}
	if time.Now().Before(evReady.until) {
		return false
	}
	evReady.ok = s.DB.Migrator().HasTable("dbt_query_event")
	if !evReady.ok {
		evReady.until = time.Now().Add(60 * time.Second)
	}
	return evReady.ok
}

// InsertQueryEvent 写入一条未成功记录；被拦截（blocked）的同时写 dbt_query_reject（系统审计读取）。
func (s *Store) InsertQueryEvent(e *query.QueryEvent) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	row := queryEventRow{UserName: e.UserName, UserDisplay: e.UserDisplay, InstanceID: e.InstanceID, InstanceName: e.InstanceName, DBName: e.DB,
		SQLText: e.SQL, Status: e.Status, Code: e.Code, Reason: e.Reason, CostMs: e.CostMs, CreatedAt: e.CreatedAt}
	if err := s.DB.Create(&row).Error; err != nil {
		return err
	}
	e.ID = row.ID
	if e.Status == query.StatusBlocked {
		rej := queryRejectRow{UserName: e.UserName, InstanceID: e.InstanceID, DBName: e.DB, SQLText: e.SQL, Reason: e.Reason, CreatedAt: e.CreatedAt}
		return s.DB.Create(&rej).Error
	}
	return nil
}

// ExpiredQueryPrivileges 返回用户在实例上已过期（但未删除）的查询权限，到期日新的在前。
func (s *Store) ExpiredQueryPrivileges(username string, instanceID uint) ([]model.QueryPrivilege, error) {
	var out []model.QueryPrivilege
	err := s.DB.Where("user_name = ? AND instance_id = ? AND is_deleted = 0 AND valid_date < ?", username, instanceID, time.Now().Format("2006-01-02")).
		Order("valid_date DESC").Limit(200).Find(&out).Error
	return out, err
}

// ListHistory 合并 dbt_query_log（成功）与 dbt_query_event（未成功），按时间倒序分页。
// 两张表的字符集 / 排序规则可能不同，所以不用 UNION，而是各取前 offset+limit 条在内存里归并。
func (s *Store) ListHistory(f query.QueryLogFilter, status string) ([]query.HistoryRow, int64, error) {
	var (
		okRows   []model.QueryLog
		evRows   []queryEventRow
		okTotal  int64
		evTotal  int64
		wantOK   = status == "" || status == query.StatusOK
		wantEv   = status != query.StatusOK
		need     = f.Offset + f.Limit
		keywords = f.Keywords
	)
	if wantOK {
		q := s.DB.Model(&model.QueryLog{})
		if f.Username != "" {
			q = q.Where("username = ?", f.Username)
		}
		if f.UserLike != "" {
			p := "%" + likeEscape(f.UserLike) + "%"
			q = q.Where("(username LIKE ? ESCAPE '!' OR user_display LIKE ? ESCAPE '!')", p, p)
		}
		if f.Instance != "" {
			q = q.Where("instance_name = ?", f.Instance)
		}
		if f.Start != nil {
			q = q.Where("create_time >= ?", *f.Start)
		}
		if f.End != nil {
			q = q.Where("create_time < ?", *f.End)
		}
		for _, k := range keywords {
			q = q.Where("sqllog LIKE ? ESCAPE '!'", "%"+likeEscape(k)+"%")
		}
		if err := q.Count(&okTotal).Error; err != nil {
			return nil, 0, err
		}
		if err := q.Order("id DESC").Limit(need).Find(&okRows).Error; err != nil {
			return nil, 0, err
		}
	}
	if wantEv {
		q := s.DB.Model(&queryEventRow{})
		if status != "" {
			q = q.Where("status = ?", status)
		}
		if f.Username != "" {
			q = q.Where("user_name = ?", f.Username)
		}
		if f.UserLike != "" {
			p := "%" + likeEscape(f.UserLike) + "%"
			q = q.Where("(user_name LIKE ? ESCAPE '!' OR user_display LIKE ? ESCAPE '!')", p, p)
		}
		if f.Instance != "" {
			q = q.Where("instance_name = ?", f.Instance)
		}
		if f.Start != nil {
			q = q.Where("created_at >= ?", *f.Start)
		}
		if f.End != nil {
			q = q.Where("created_at < ?", *f.End)
		}
		for _, k := range keywords {
			q = q.Where("sql_text LIKE ? ESCAPE '!'", "%"+likeEscape(k)+"%")
		}
		if err := q.Count(&evTotal).Error; err != nil {
			return nil, 0, err
		}
		if err := q.Order("id DESC").Limit(need).Find(&evRows).Error; err != nil {
			return nil, 0, err
		}
	}
	all := make([]query.HistoryRow, 0, len(okRows)+len(evRows))
	for _, r := range okRows {
		all = append(all, query.HistoryRow{ID: r.ID, Time: r.CreateTime, Username: r.Username, Display: r.UserDisplay, Instance: r.InstanceName,
			DB: r.DBName, SQL: r.SQLLog, Rows: r.EffectRow, CostSec: r.CostTime, Status: query.StatusOK})
	}
	for _, r := range evRows {
		all = append(all, query.HistoryRow{ID: r.ID, Time: r.CreatedAt, Username: r.UserName, Display: r.UserDisplay, Instance: r.InstanceName,
			DB: r.DBName, SQL: r.SQLText, CostSec: formatSec(r.CostMs), Status: r.Status, Code: r.Code, Reason: r.Reason, FromEvent: true})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Time.After(all[j].Time) })
	if f.Offset >= len(all) {
		return []query.HistoryRow{}, okTotal + evTotal, nil
	}
	all = all[f.Offset:]
	if len(all) > f.Limit {
		all = all[:f.Limit]
	}
	return all, okTotal + evTotal, nil
}

func formatSec(ms int64) string { return strconv.FormatFloat(float64(ms)/1000, 'f', 3, 64) }

var (
	_ query.EventRepo       = (*Store)(nil)
	_ query.ExpiredPrivRepo = (*Store)(nil)
)
