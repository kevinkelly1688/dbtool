// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package sysadmin

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"dbtool/internal/crypto"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// store 是 Repo 的 GORM 实现。
// 凡是拼进 SQL 的标识符都是本文件里的常量，用户输入一律走参数。
type store struct {
	db *gorm.DB
	m  *crypto.Mirage
}

func newStore(db *gorm.DB, m *crypto.Mirage) *store { return &store{db: db, m: m} }

var _ Repo = (*store)(nil)

func (s *store) InTx(fn func(Repo) error) error {
	return s.db.Transaction(func(tx *gorm.DB) error { return fn(&store{db: tx, m: s.m}) })
}

func nilIfMissing(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	return err
}

// likePattern 生成 LIKE 参数，转义 \ % _，让用户输入的通配符按字面匹配。
func likePattern(s string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s) + "%"
}

// ConfigValue 读一项系统设置的生效值（显式值，否则登记的默认值；敏感项已解密）。key 是 settings.KeyXxx。
func (s *store) ConfigValue(item string) (string, bool) {
	return settings.Open(s.db, s.m).Value(item)
}

// ---- 审计日志与审计页 ----

func (s *store) InsertAuditLog(e *model.AuditEntry) error { return s.db.Create(e).Error }

func (s *store) ListAuditLogs(f AuditLogFilter) ([]model.AuditEntry, int64, error) {
	q := s.db.Model(&model.AuditEntry{})
	if f.Action != "" {
		q = q.Where("action = ?", f.Action)
	}
	if f.User != "" {
		q = q.Where("user_name LIKE ?", likePattern(f.User))
	}
	if f.Search != "" {
		p := likePattern(f.Search)
		q = q.Where("(user_name LIKE ? OR user_display LIKE ? OR action LIKE ? OR extra_info LIKE ?)", p, p, p, p)
	}
	if f.Start != nil {
		q = q.Where("action_time >= ?", *f.Start)
	}
	if f.End != nil {
		q = q.Where("action_time < ?", *f.End)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.AuditEntry
	err := q.Order("action_time DESC, id DESC").Offset(f.Offset).Limit(f.Limit).Find(&rows).Error
	return rows, total, err
}

func (s *store) AuditActions() ([]string, error) {
	var out []string
	err := s.db.Raw("SELECT DISTINCT action FROM dbt_audit_log ORDER BY action LIMIT 500").Scan(&out).Error
	return out, err
}

func (s *store) ListQueryLogs(f QueryLogFilter) ([]model.QueryLog, int64, error) {
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
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.QueryLog
	err := q.Order("id DESC").Offset(f.Offset).Limit(f.Limit).Find(&rows).Error
	return rows, total, err
}

type sqlWorkflowScan struct {
	ID              uint
	Name            string
	Engineer        string
	EngineerDisplay string
	Status          string
	GroupName       string
	InstanceName    string
	DBName          string
	SyntaxType      int
	IsOfflineExport int
	CreateTime      time.Time
	FinishTime      *time.Time
}

func (s *store) ListSQLWorkflows(f SQLWorkflowFilter) ([]SQLWorkflowRow, int64, error) {
	where := func(q *gorm.DB) *gorm.DB {
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
		return q
	}
	var total int64
	if err := where(s.db.Table("dbt_sql_workflow w")).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var scan []sqlWorkflowScan
	err := where(s.db.Table("dbt_sql_workflow w").
		Joins("LEFT JOIN dbt_sql_instance i ON i.id = w.instance_id").
		Select("w.id, w.workflow_name AS name, w.engineer, w.engineer_display, w.status, w.group_name, i.instance_name AS instance_name, " +
			"w.db_name, w.syntax_type, w.is_offline_export, w.create_time, w.finish_time")).
		Order("w.create_time DESC, w.id DESC").Offset(f.Offset).Limit(f.Limit).Scan(&scan).Error
	if err != nil {
		return nil, 0, err
	}
	out := make([]SQLWorkflowRow, len(scan))
	for i, r := range scan {
		out[i] = SQLWorkflowRow{ID: r.ID, Name: r.Name, Engineer: r.Engineer, EngineerDisplay: r.EngineerDisplay, Status: r.Status,
			GroupName: r.GroupName, InstanceName: r.InstanceName, DBName: r.DBName, SyntaxType: r.SyntaxType, IsOfflineExport: r.IsOfflineExport,
			CreateTime: r.CreateTime, FinishTime: r.FinishTime}
	}
	return out, total, nil
}

// AuthSetting 读登录与安全里的一项设置（显式设置过才返回；没有配置按“没有”处理，不影响本模块使用默认值）。key 是 settings.KeyXxx。
func (s *store) AuthSetting(key string) (string, bool) {
	return settings.Open(s.db, s.m).Lookup(key)
}

// quiet 返回不输出 SQL 日志的会话：写入密码哈希、Webhook 等敏感值的语句即使出错或变慢，gorm 默认也会把带参数的 SQL 打进日志，
// 这些语句统一走 quiet，保证敏感值不会进日志。失败仍会通过返回的 error 交给调用方处理。
func (s *store) quiet() *gorm.DB {
	return s.db.Session(&gorm.Session{Logger: s.db.Logger.LogMode(logger.Silent)})
}
