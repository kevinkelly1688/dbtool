// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package settings

import (
	"errors"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"dbtool/internal/model"
)

// Row 是 dbt_setting 的一行（值是存储形态：敏感项为密文）。
type Row struct {
	Key       string
	Value     string
	Encrypted bool
	Version   uint
	UpdatedBy string
	UpdatedAt time.Time
}

// Write 是对 dbt_setting 的一次写：Delete 删除该行（恢复默认）；SkipExisting 表示目标键已有行时不覆盖（导入用）。
type Write struct {
	Key          string
	Value        string
	Encrypted    bool
	Delete       bool
	By           string
	SkipExisting bool
}

// History 是 dbt_setting_history 的一行。敏感项的 Old / New 为空。
type History struct {
	ID        uint      `json:"id"`
	Key       string    `json:"key"`
	Old       string    `json:"old"`
	New       string    `json:"new"`
	Secret    bool      `json:"secret"`
	Action    string    `json:"action"` // set / reset / import
	By        string    `json:"by"`
	ChangedAt time.Time `json:"changed_at"`
}

// Store 是设置的存储。GORM 实现读写 dbt_setting / dbt_setting_history；内存实现给测试用。
type Store interface {
	// Load 返回全部行。ready=false 表示 dbt_setting 表不存在（没有执行 deploy/sql/260_setting.sql）。
	Load() (rows map[string]Row, ready bool, err error)
	// Apply 在一个事务里写入变更与变更记录；audit 非空时同一事务里写一条系统审计。
	Apply(writes []Write, hist []History, audit *model.AuditEntry) error
	// Recent 返回变更记录（新的在前）；key 为空 = 全部。
	Recent(key string, limit, offset int) ([]History, error)
}

// ---- GORM 实现 ----

type settingRow struct {
	Key        string    `gorm:"column:skey;primaryKey"`
	Value      string    `gorm:"column:svalue"`
	Encrypted  bool      `gorm:"column:encrypted"`
	Version    uint      `gorm:"column:version"`
	UpdatedBy  string    `gorm:"column:updated_by"`
	UpdateTime time.Time `gorm:"column:update_time"`
}

func (settingRow) TableName() string { return "dbt_setting" }

type historyRow struct {
	ID         uint      `gorm:"column:id;primaryKey;autoIncrement"`
	Key        string    `gorm:"column:skey"`
	OldValue   string    `gorm:"column:old_value"`
	NewValue   string    `gorm:"column:new_value"`
	IsSecret   bool      `gorm:"column:is_secret"`
	Action     string    `gorm:"column:action"`
	ChangedBy  string    `gorm:"column:changed_by"`
	ChangeTime time.Time `gorm:"column:change_time"`
}

func (historyRow) TableName() string { return "dbt_setting_history" }

// NewGormStore 返回元数据库上的存储。
func NewGormStore(db *gorm.DB) Store { return gormStore{db: db} }

type gormStore struct{ db *gorm.DB }

// isMissingTable 判断错误是不是“表不存在”（MySQL 1146）。
func isMissingTable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "1146") || strings.Contains(strings.ToLower(s), "doesn't exist")
}

func (g gormStore) Load() (map[string]Row, bool, error) {
	var rs []settingRow
	if err := g.db.Find(&rs).Error; err != nil {
		if isMissingTable(err) {
			return nil, false, nil
		}
		return nil, true, err
	}
	out := make(map[string]Row, len(rs))
	for _, r := range rs {
		out[r.Key] = Row{Key: r.Key, Value: r.Value, Encrypted: r.Encrypted, Version: r.Version, UpdatedBy: r.UpdatedBy, UpdatedAt: r.UpdateTime}
	}
	return out, true, nil
}

// WriteTx 在调用方的事务里写设置与变更记录（模块在自己的事务里同时写自己的审计时用）。写完后要调用 Service.Invalidate。
func WriteTx(tx *gorm.DB, writes []Write, hist []History) error {
	now := time.Now()
	for _, w := range writes {
		switch {
		case w.Delete:
			if err := tx.Where("skey = ?", w.Key).Delete(&settingRow{}).Error; err != nil {
				return err
			}
		case w.SkipExisting:
			if err := tx.Exec("INSERT IGNORE INTO dbt_setting (skey, svalue, encrypted, version, updated_by, update_time) VALUES (?, ?, ?, 1, ?, ?)",
				w.Key, w.Value, w.Encrypted, w.By, now).Error; err != nil {
				return err
			}
		default:
			if err := tx.Exec("INSERT INTO dbt_setting (skey, svalue, encrypted, version, updated_by, update_time) VALUES (?, ?, ?, 1, ?, ?) "+
				"ON DUPLICATE KEY UPDATE svalue = VALUES(svalue), encrypted = VALUES(encrypted), version = version + 1, "+
				"updated_by = VALUES(updated_by), update_time = VALUES(update_time)",
				w.Key, w.Value, w.Encrypted, w.By, now).Error; err != nil {
				return err
			}
		}
	}
	for _, h := range hist {
		hr := historyRow{Key: h.Key, OldValue: h.Old, NewValue: h.New, IsSecret: h.Secret, Action: h.Action, ChangedBy: h.By, ChangeTime: now}
		if err := tx.Create(&hr).Error; err != nil {
			return err
		}
	}
	return nil
}

func (g gormStore) Apply(writes []Write, hist []History, audit *model.AuditEntry) error {
	return g.db.Transaction(func(tx *gorm.DB) error {
		if err := WriteTx(tx, writes, hist); err != nil {
			return err
		}
		if audit != nil {
			return tx.Create(audit).Error
		}
		return nil
	})
}

func (g gormStore) Recent(key string, limit, offset int) ([]History, error) {
	q := g.db.Model(&historyRow{})
	if key != "" {
		q = q.Where("skey = ?", key)
	}
	var rs []historyRow
	if err := q.Order("id DESC").Offset(offset).Limit(limit).Find(&rs).Error; err != nil {
		if isMissingTable(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]History, 0, len(rs))
	for _, r := range rs {
		out = append(out, History{ID: r.ID, Key: r.Key, Old: r.OldValue, New: r.NewValue, Secret: r.IsSecret, Action: r.Action, By: r.ChangedBy, ChangedAt: r.ChangeTime})
	}
	return out, nil
}

// ---- 内存实现（测试 / 演示）----

// MemStore 是内存存储，满足 Store。
type MemStore struct {
	mu     sync.Mutex
	rows   map[string]Row
	hist   []History
	audits []model.AuditEntry
	// NotReady 为 true 时 Load 返回 ready=false（模拟表不存在）。
	NotReady bool
	// FailApply 非空时 Apply 返回这个错误（模拟写库失败）。
	FailApply error
}

// NewMemStore 返回内存存储。
func NewMemStore() *MemStore { return &MemStore{rows: map[string]Row{}} }

func (m *MemStore) Load() (map[string]Row, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.NotReady {
		return nil, false, nil
	}
	out := make(map[string]Row, len(m.rows))
	for k, v := range m.rows {
		out[k] = v
	}
	return out, true, nil
}

func (m *MemStore) Apply(writes []Write, hist []History, audit *model.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailApply != nil {
		return m.FailApply
	}
	if m.NotReady {
		return errors.New("dbt_setting 不存在")
	}
	now := time.Now()
	for _, w := range writes {
		cur, exists := m.rows[w.Key]
		switch {
		case w.Delete:
			delete(m.rows, w.Key)
		case w.SkipExisting && exists:
		default:
			v := cur.Version + 1
			m.rows[w.Key] = Row{Key: w.Key, Value: w.Value, Encrypted: w.Encrypted, Version: v, UpdatedBy: w.By, UpdatedAt: now}
		}
	}
	for _, h := range hist {
		h.ID = uint(len(m.hist) + 1)
		h.ChangedAt = now
		m.hist = append(m.hist, h)
	}
	if audit != nil {
		m.audits = append(m.audits, *audit)
	}
	return nil
}

func (m *MemStore) Recent(key string, limit, offset int) ([]History, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []History
	for i := len(m.hist) - 1; i >= 0; i-- {
		if key == "" || m.hist[i].Key == key {
			out = append(out, m.hist[i])
		}
	}
	if offset > len(out) {
		offset = len(out)
	}
	out = out[offset:]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Audits 返回已写入的系统审计（测试用）。
func (m *MemStore) Audits() []model.AuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]model.AuditEntry(nil), m.audits...)
}

// Put 直接放一行（测试用，不写历史）。
func (m *MemStore) Put(key, value string, encrypted bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[key] = Row{Key: key, Value: value, Encrypted: encrypted, Version: 1, UpdatedAt: time.Now()}
}
