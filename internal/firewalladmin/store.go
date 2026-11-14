// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package firewalladmin

import (
	"errors"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// ErrDuplicateName 表示规则名称已被占用（dbt_firewall_rule.uk_name）。
var ErrDuplicateName = errors.New("规则名称已存在")

// DayHits 是某条规则某一天的命中次数（含观察）。
type DayHits struct {
	RuleID uint
	Day    string // 2006-01-02
	N      int64
}

// HitCount 是命中记录按 (结果, 若生效的动作) 汇总的次数。
type HitCount struct {
	Effective   string
	WouldAction string
	N           int64
}

// HitSummary 是一段时间内命中记录的汇总。
type HitSummary struct {
	Counts []HitCount
	Users  int64
	Rules  int64
}

// HitFilter 是命中记录的存储层筛选。
type HitFilter struct {
	RuleID uint
	Q      string
	User   string
	Result string
	Scene  string
	Since  time.Time
	Offset int
	Limit  int
}

// Store 是防火墙管理的存储。写操作都在一个事务里同时写一条系统审计。
type Store interface {
	// Missing 返回还没建的表名（空 = 三张表都在）。
	Missing() []string
	ListRules() ([]model.FirewallRule, error)
	GetRule(id uint) (*model.FirewallRule, error)
	// CreateRule 新增一条规则（回填 ID）；名称重复返回 ErrDuplicateName。
	CreateRule(m *model.FirewallRule, audit *model.AuditEntry) error
	// UpdateRule 整体更新一条规则（不改 ID 与创建时间）；名称重复返回 ErrDuplicateName。
	UpdateRule(m *model.FirewallRule, audit *model.AuditEntry) error
	// PatchRule 只改指定列（启停 / 切换模式）。
	PatchRule(id uint, fields map[string]any, audit *model.AuditEntry) error
	// DeleteRule 删除规则及其例外，返回一并删除的例外数。命中记录保留。
	DeleteRule(id uint, audit *model.AuditEntry) (int64, error)

	DayHits(since time.Time) ([]DayHits, error)
	HitSummary(since time.Time) (*HitSummary, error)
	ListHits(f HitFilter) ([]model.FirewallHit, int64, error)
	GetHit(id uint) (*model.FirewallHit, error)

	// ListExceptions 返回 since 之后仍有效或刚过期的例外（新的在前）。
	ListExceptions(since time.Time) ([]model.FirewallException, error)
	GetException(id uint) (*model.FirewallException, error)
	CreateException(m *model.FirewallException, audit *model.AuditEntry) error
	DeleteException(id uint, audit *model.AuditEntry) error

	// SetConfig 写一项系统设置（dbt_setting）并在同一事务里写审计。
	SetConfig(item, value, description string, audit *model.AuditEntry) error

	Groups() ([]model.AuthGroup, error)
	// UserGroups 返回一批用户名各自所在的权限组 ID。
	UserGroups(usernames []string) (map[string][]uint, error)
	// QueryLogs 返回 since 之后最近的在线查询日志（新的在前）。
	QueryLogs(since time.Time, limit int) ([]model.QueryLog, error)
	// Workflows 返回 since 之后最近提交的工单（含数据导出；不含线下手工执行的），带 SQL 与提交时的检查结果（新的在前）。
	Workflows(since time.Time, limit int) ([]WorkflowSQL, error)
}

// WorkflowSQL 是回放用的一张工单。
type WorkflowSQL struct {
	ID         uint
	Engineer   string
	InstanceID uint
	DBName     string
	Export     bool
	SQL        string
	Review     string // 提交时的检查结果（JSON 数组），用来取每条语句的预估影响行数
	At         time.Time
}

type gormStore struct{ db *gorm.DB }

func newStore(db *gorm.DB) *gormStore { return &gormStore{db: db} }

func mysqlErrNo(err error) uint16 {
	var me *gomysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

func noTable(err error) bool { return mysqlErrNo(err) == 1146 }

func likeEscape(s string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
}

func (s *gormStore) Missing() []string {
	var out []string
	check := func(name string, m any) {
		var n int64
		if err := s.db.Model(m).Limit(1).Count(&n).Error; err != nil && noTable(err) {
			out = append(out, name)
		}
	}
	check("dbt_firewall_rule", &model.FirewallRule{})
	check("dbt_firewall_exception", &model.FirewallException{})
	check("dbt_firewall_hit", &model.FirewallHit{})
	return out
}

func (s *gormStore) ListRules() ([]model.FirewallRule, error) {
	var out []model.FirewallRule
	err := s.db.Order("priority, id").Find(&out).Error
	if noTable(err) {
		return nil, nil
	}
	return out, err
}

func (s *gormStore) GetRule(id uint) (*model.FirewallRule, error) {
	var m model.FirewallRule
	err := s.db.Where("id = ?", id).Take(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *gormStore) CreateRule(m *model.FirewallRule, entry *model.AuditEntry) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(m).Error; err != nil {
			if mysqlErrNo(err) == 1062 {
				return ErrDuplicateName
			}
			return err
		}
		if entry != nil {
			return tx.Create(entry).Error
		}
		return nil
	})
}

var ruleCols = []string{"name", "description", "priority", "enabled", "mode", "observe_since", "scenes", "env", "instance_ids",
	"group_mode", "group_ids", "match_mode", "conditions", "action", "action_arg", "message", "notify", "updated_by", "updated_at"}

func (s *gormStore) UpdateRule(m *model.FirewallRule, entry *model.AuditEntry) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.FirewallRule{}).Where("id = ?", m.ID).Select(ruleCols).Updates(m)
		if res.Error != nil {
			if mysqlErrNo(res.Error) == 1062 {
				return ErrDuplicateName
			}
			return res.Error
		}
		if entry != nil {
			return tx.Create(entry).Error
		}
		return nil
	})
}

func (s *gormStore) PatchRule(id uint, fields map[string]any, entry *model.AuditEntry) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.FirewallRule{}).Where("id = ?", id).Updates(fields).Error; err != nil {
			return err
		}
		if entry != nil {
			return tx.Create(entry).Error
		}
		return nil
	})
}

func (s *gormStore) DeleteRule(id uint, entry *model.AuditEntry) (int64, error) {
	var removed int64
	err := s.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Where("rule_id = ?", id).Delete(&model.FirewallException{})
		if res.Error != nil {
			return res.Error
		}
		removed = res.RowsAffected
		if err := tx.Where("id = ?", id).Delete(&model.FirewallRule{}).Error; err != nil {
			return err
		}
		if entry != nil {
			return tx.Create(entry).Error
		}
		return nil
	})
	return removed, err
}

func (s *gormStore) DayHits(since time.Time) ([]DayHits, error) {
	var out []DayHits
	err := s.db.Model(&model.FirewallHit{}).
		Select("rule_id AS rule_id, DATE_FORMAT(created_at, '%Y-%m-%d') AS day, SUM(repeat_cnt) AS n").
		Where("created_at >= ?", since).Group("rule_id, DATE_FORMAT(created_at, '%Y-%m-%d')").Scan(&out).Error
	if noTable(err) {
		return nil, nil
	}
	return out, err
}

func (s *gormStore) HitSummary(since time.Time) (*HitSummary, error) {
	sum := &HitSummary{}
	err := s.db.Model(&model.FirewallHit{}).Select("effective AS effective, would_action AS would_action, SUM(repeat_cnt) AS n").
		Where("created_at >= ?", since).Group("effective, would_action").Scan(&sum.Counts).Error
	if noTable(err) {
		return sum, nil
	}
	if err != nil {
		return nil, err
	}
	var row struct{ Users, Rules int64 }
	if err := s.db.Model(&model.FirewallHit{}).Select("COUNT(DISTINCT user_name) AS users, COUNT(DISTINCT rule_id) AS `rules`").
		Where("created_at >= ?", since).Scan(&row).Error; err != nil {
		return nil, err
	}
	sum.Users, sum.Rules = row.Users, row.Rules
	return sum, nil
}

func (s *gormStore) ListHits(f HitFilter) ([]model.FirewallHit, int64, error) {
	q := s.db.Model(&model.FirewallHit{}).Where("created_at >= ?", f.Since)
	if f.RuleID != 0 {
		q = q.Where("rule_id = ?", f.RuleID)
	}
	if f.User != "" {
		q = q.Where("user_name = ?", f.User)
	}
	if f.Result != "" {
		q = q.Where("effective = ?", f.Result)
	}
	if f.Scene != "" {
		q = q.Where("scene = ?", f.Scene)
	}
	if f.Q != "" {
		like := "%" + likeEscape(f.Q) + "%"
		q = q.Where("(sql_text LIKE ? ESCAPE '!' OR user_name LIKE ? ESCAPE '!' OR rule_name LIKE ? ESCAPE '!' OR instance_name LIKE ? ESCAPE '!' OR db_name LIKE ? ESCAPE '!')",
			like, like, like, like, like)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		if noTable(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	var out []model.FirewallHit
	err := q.Order("last_at DESC, id DESC").Offset(f.Offset).Limit(f.Limit).Find(&out).Error
	return out, total, err
}

func (s *gormStore) GetHit(id uint) (*model.FirewallHit, error) {
	var m model.FirewallHit
	err := s.db.Where("id = ?", id).Take(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *gormStore) ListExceptions(since time.Time) ([]model.FirewallException, error) {
	var out []model.FirewallException
	err := s.db.Where("expires_at > ?", since).Order("id DESC").Find(&out).Error
	if noTable(err) {
		return nil, nil
	}
	return out, err
}

func (s *gormStore) GetException(id uint) (*model.FirewallException, error) {
	var m model.FirewallException
	err := s.db.Where("id = ?", id).Take(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *gormStore) CreateException(m *model.FirewallException, entry *model.AuditEntry) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(m).Error; err != nil {
			return err
		}
		if entry != nil {
			return tx.Create(entry).Error
		}
		return nil
	})
}

func (s *gormStore) DeleteException(id uint, entry *model.AuditEntry) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id = ?", id).Delete(&model.FirewallException{}).Error; err != nil {
			return err
		}
		if entry != nil {
			return tx.Create(entry).Error
		}
		return nil
	})
}

func (s *gormStore) SetConfig(item, value, description string, entry *model.AuditEntry) error {
	svc := settings.Open(s.db, nil)
	by := ""
	if entry != nil {
		by = entry.UserName
	}
	ws, hs, err := svc.Prepare(by, map[string]string{item: value})
	if err != nil {
		return err
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := settings.WriteTx(tx, ws, hs); err != nil {
			return err
		}
		if entry != nil {
			return tx.Create(entry).Error
		}
		return nil
	})
	svc.Invalidate()
	return err
}

func (s *gormStore) Groups() ([]model.AuthGroup, error) {
	var out []model.AuthGroup
	err := s.db.Order("id").Find(&out).Error
	return out, err
}

func (s *gormStore) UserGroups(usernames []string) (map[string][]uint, error) {
	out := map[string][]uint{}
	const batch = 500
	for i := 0; i < len(usernames); i += batch {
		part := usernames[i:min(i+batch, len(usernames))]
		var rows []struct {
			Username string
			GroupID  uint
		}
		err := s.db.Table("dbt_sql_users AS u").Select("u.username AS username, g.group_id AS group_id").
			Joins("JOIN dbt_sql_users_groups g ON g.users_id = u.id").Where("u.username IN ?", part).Scan(&rows).Error
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.Username] = append(out[r.Username], r.GroupID)
		}
	}
	return out, nil
}

func (s *gormStore) QueryLogs(since time.Time, limit int) ([]model.QueryLog, error) {
	var out []model.QueryLog
	err := s.db.Select("id, instance_name, db_name, sqllog, username, create_time").
		Where("create_time >= ?", since).Order("id DESC").Limit(limit).Find(&out).Error
	return out, err
}

func (s *gormStore) Workflows(since time.Time, limit int) ([]WorkflowSQL, error) {
	var rows []struct {
		ID              uint
		Engineer        string
		InstanceID      uint
		DBName          string
		IsOfflineExport int
		CreateTime      time.Time
		SQLContent      string
		ReviewContent   string
	}
	err := s.db.Table("dbt_sql_workflow AS w").
		Select("w.id AS id, w.engineer AS engineer, w.instance_id AS instance_id, w.db_name AS db_name, w.is_offline_export AS is_offline_export, "+
			"w.create_time AS create_time, c.sql_content AS sql_content, c.review_content AS review_content").
		Joins("JOIN dbt_sql_workflow_content c ON c.workflow_id = w.id").
		Where("w.create_time >= ? AND w.is_manual = 0", since).Order("w.id DESC").Limit(limit).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]WorkflowSQL, 0, len(rows))
	for _, r := range rows {
		out = append(out, WorkflowSQL{ID: r.ID, Engineer: r.Engineer, InstanceID: r.InstanceID, DBName: r.DBName, Export: r.IsOfflineExport != 0,
			SQL: r.SQLContent, Review: r.ReviewContent, At: r.CreateTime})
	}
	return out, nil
}
