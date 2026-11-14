// @author: kevinkelly
// @license: Apache Licence
// @file: privapply_ext.go
// @time: 2026/10/28

package store

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"dbtool/internal/model"
	"dbtool/internal/privapply"
)

// 权限申请的旁路扩展（deploy/sql/040_query_apply_ext.sql、041_query_priv_ext.sql）。
// 表没创建时就绪检查返回 false，privapply 服务会降级（不要求申请理由、授予来源不显示）。

var _ privapply.ExtRepo = (*Store)(nil)

const (
	tblApplyExt = "dbt_query_apply_ext"
	tblPrivExt  = "dbt_query_priv_ext"
	dateLayout  = "2006-01-02"
)

// 旁路表是否存在：进程内缓存一小会儿，避免每个请求都查 information_schema；执行 SQL 脚本后最多 30 秒生效。
var extTables struct {
	mu          sync.Mutex
	at          time.Time
	apply, priv bool
}

func (s *Store) extReady() (apply, priv bool) {
	t := &extTables
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.at.IsZero() && time.Since(t.at) < 30*time.Second {
		return t.apply, t.priv
	}
	m := s.DB.Migrator()
	t.apply, t.priv, t.at = m.HasTable(tblApplyExt), m.HasTable(tblPrivExt), time.Now()
	return t.apply, t.priv
}

func (s *Store) ApplyExtReady() bool { a, _ := s.extReady(); return a }
func (s *Store) PrivExtReady() bool  { _, p := s.extReady(); return p }

func (s *Store) SaveApplyExt(e privapply.ApplyExt) error {
	return s.DB.Exec("INSERT INTO "+tblApplyExt+" (apply_id, reason, created_at) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE reason = VALUES(reason)",
		e.ApplyID, e.Reason, e.CreatedAt).Error
}

type applyExtRow struct {
	ApplyID   uint `gorm:"primaryKey"`
	Reason    string
	CreatedAt time.Time
}

func (s *Store) ApplyExts(ids []uint) (map[uint]privapply.ApplyExt, error) {
	out := map[uint]privapply.ApplyExt{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []applyExtRow
	if err := s.DB.Table(tblApplyExt).Where("apply_id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ApplyID] = privapply.ApplyExt{ApplyID: r.ApplyID, Reason: r.Reason, CreatedAt: r.CreatedAt}
	}
	return out, nil
}

type privExtRow struct {
	PrivilegeID uint `gorm:"primaryKey;autoIncrement:false"`
	Source      string
	ApplyID     uint
	GrantedBy   string
	Reason      string
	CreatedAt   time.Time
}

func (r privExtRow) ext() privapply.PrivExt {
	return privapply.PrivExt{PrivilegeID: r.PrivilegeID, Source: r.Source, ApplyID: r.ApplyID, GrantedBy: r.GrantedBy, Reason: r.Reason, CreatedAt: r.CreatedAt}
}

func (s *Store) SavePrivExts(es []privapply.PrivExt) error {
	if len(es) == 0 {
		return nil
	}
	rows := make([]privExtRow, 0, len(es))
	for _, e := range es {
		rows = append(rows, privExtRow{PrivilegeID: e.PrivilegeID, Source: e.Source, ApplyID: e.ApplyID, GrantedBy: e.GrantedBy, Reason: e.Reason, CreatedAt: e.CreatedAt})
	}
	return s.DB.Table(tblPrivExt).Clauses(clause.OnConflict{UpdateAll: true}).Create(&rows).Error
}

func (s *Store) PrivExts(ids []uint) (map[uint]privapply.PrivExt, error) {
	out := map[uint]privapply.PrivExt{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []privExtRow
	if err := s.DB.Table(tblPrivExt).Where("privilege_id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.PrivilegeID] = r.ext()
	}
	return out, nil
}

func (s *Store) PrivExtsByApply(applyID uint) ([]privapply.PrivExt, error) {
	var rows []privExtRow
	if err := s.DB.Table(tblPrivExt).Where("apply_id = ? AND source = ?", applyID, privapply.SourceApply).Order("privilege_id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]privapply.PrivExt, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ext())
	}
	return out, nil
}

// ---- 列表筛选 ----

func dayOf(t time.Time) time.Time {
	if t.IsZero() {
		t = time.Now()
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// applyListFilters 加上申请列表的“只看本人 / 状态 / 搜索”筛选（范围条件由调用方先加）。
func applyListFilters(q *gorm.DB, f privapply.ApplyFilter) *gorm.DB {
	if f.MineOnly != "" {
		q = q.Where("user_name = ?", f.MineOnly)
	}
	day := dayOf(f.Today).Format(dateLayout)
	switch f.Status {
	case "":
	case "expired":
		q = q.Where("status = 1 AND valid_date < ?", day)
	case "1":
		q = q.Where("status = 1 AND valid_date >= ?", day)
	default:
		if n, err := strconv.Atoi(f.Status); err == nil {
			q = q.Where("status = ?", n)
		}
	}
	if f.Search != "" {
		p := likePattern(f.Search)
		conds := []string{"title LIKE ?", "user_display LIKE ?", "user_name LIKE ?", "db_list LIKE ?", "table_list LIKE ?",
			"instance_id IN (SELECT id FROM dbt_sql_instance WHERE instance_name LIKE ?)"}
		args := []any{p, p, p, p, p, p}
		if n, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(f.Search), "#"), 10, 32); err == nil {
			conds = append(conds, "apply_id = ?")
			args = append(args, n)
		}
		q = q.Where(strings.Join(conds, " OR "), args...)
	}
	return q
}

// privListFilters 加上查询权限列表的筛选：状态（按有效期）、用户 / 实例 / 库、搜索。
func privListFilters(q *gorm.DB, f privapply.PrivFilter) *gorm.DB {
	day := dayOf(f.Today)
	d0, d7 := day.Format(dateLayout), day.AddDate(0, 0, 7).Format(dateLayout)
	switch f.State {
	case "all":
	case "soon":
		q = q.Where("qp.valid_date >= ? AND qp.valid_date <= ?", d0, d7)
	case "expired":
		q = q.Where("qp.valid_date < ?", d0)
	default:
		q = q.Where("qp.valid_date >= ?", d0)
	}
	if f.FilterUser != "" {
		q = q.Where("qp.user_name = ?", f.FilterUser)
	}
	if f.FilterInst != 0 {
		q = q.Where("qp.instance_id = ?", f.FilterInst)
	}
	if f.FilterDB != "" {
		q = q.Where("qp.db_name = ?", f.FilterDB)
	}
	if f.Search != "" {
		p := likePattern(f.Search)
		q = q.Where("qp.user_display LIKE ? OR qp.db_name LIKE ? OR qp.table_name LIKE ?", p, p, p)
	}
	if f.UserDisplay != "" && f.UserDisplay != "all" {
		q = q.Where("qp.user_display = ?", f.UserDisplay)
	}
	return q
}

func privScope(q *gorm.DB, f privapply.PrivFilter) *gorm.DB {
	switch f.Scope {
	case privapply.ScopeOwn:
		q = q.Where("qp.user_name = ?", f.UserName)
	case privapply.ScopeGroups:
		q = q.Where("qp.instance_id IN ?", f.InstanceIDs)
	}
	return q
}

type privCounts struct {
	Valid, Soon, Expired, Users, Insts int
}

func (s *Store) PrivStats(f privapply.PrivFilter, today time.Time) (privapply.PrivStats, error) {
	var st privapply.PrivStats
	day := dayOf(today)
	d0, d7 := day.Format(dateLayout), day.AddDate(0, 0, 7).Format(dateLayout)
	base := func() *gorm.DB {
		return privScope(s.DB.Table("dbt_query_privileges AS qp").Where("qp.is_deleted = 0"), f)
	}
	var c privCounts
	err := base().Select(
		"COALESCE(SUM(qp.valid_date >= ?), 0) AS valid, COALESCE(SUM(qp.valid_date >= ? AND qp.valid_date <= ?), 0) AS soon, "+
			"COALESCE(SUM(qp.valid_date < ?), 0) AS expired, COUNT(DISTINCT qp.user_name) AS users, COUNT(DISTINCT qp.instance_id) AS insts",
		d0, d0, d7, d0).Scan(&c).Error
	if err != nil {
		return st, err
	}
	st.Valid, st.Soon, st.Expired, st.Users, st.Insts = c.Valid, c.Soon, c.Expired, c.Users, c.Insts

	var us []struct{ UserName, UserDisplay string }
	if err := base().Select("DISTINCT qp.user_name, qp.user_display").Order("qp.user_name").Limit(500).Scan(&us).Error; err != nil {
		return st, err
	}
	for _, u := range us {
		st.UserOpts = append(st.UserOpts, privapply.UserOpt{UserName: u.UserName, Display: u.UserDisplay})
	}
	var is []struct {
		ID   uint
		Name string
	}
	if err := base().Joins("JOIN dbt_sql_instance i ON i.id = qp.instance_id").Select("DISTINCT qp.instance_id AS id, i.instance_name AS name").
		Order("name").Limit(500).Scan(&is).Error; err != nil {
		return st, err
	}
	for _, i := range is {
		st.InstOpts = append(st.InstOpts, privapply.InstOpt{ID: i.ID, Name: i.Name})
	}
	if f.FilterInst != 0 {
		var dbs []struct{ DBName string }
		if err := base().Where("qp.instance_id = ?", f.FilterInst).Select("DISTINCT qp.db_name AS db_name").Order("db_name").Limit(500).Scan(&dbs).Error; err != nil {
			return st, err
		}
		for _, d := range dbs {
			st.DBOpts = append(st.DBOpts, d.DBName)
		}
	}
	return st, nil
}

// ---- 用户 ----

func (s *Store) UserByName(username string) (*model.User, error) {
	var u model.User
	err := s.DB.Where("username = ?", username).First(&u).Error
	if err != nil {
		return nil, notFoundToNil(err)
	}
	return &u, nil
}

func (s *Store) GrantableUsers(groupIDs []uint, all bool) ([]model.User, error) {
	q := s.DB.Model(&model.User{}).Where("dbt_sql_users.is_active = 1")
	if !all {
		if len(groupIDs) == 0 {
			return nil, nil
		}
		q = q.Where("dbt_sql_users.id IN (SELECT users_id FROM dbt_sql_users_resource_group WHERE resourcegroup_id IN ?)", groupIDs)
	}
	var out []model.User
	err := q.Order("dbt_sql_users.username").Limit(2000).Find(&out).Error
	return out, err
}

// UserDept 取用户部门（系统管理的 dbt_user_profile）；表不存在或没有记录时返回空串。
func (s *Store) UserDept(username string) string {
	var dept string
	err := s.DB.Raw("SELECT p.dept FROM dbt_user_profile p JOIN dbt_sql_users u ON u.id = p.user_id WHERE u.username = ? LIMIT 1", username).Scan(&dept).Error
	if err != nil {
		return ""
	}
	return dept
}

func (s *Store) WriteAuditLog(e *model.AuditEntry) error { return s.DB.Create(e).Error }

// WriteLifecycleAudit 写权限申请的审批类动作（提交 / 审批 / 驳回 / 撤销）的系统审计（audit.LifecycleWriter）。
func (s *Store) WriteLifecycleAudit(e *model.AuditEntry) error { return s.DB.Create(e).Error }
