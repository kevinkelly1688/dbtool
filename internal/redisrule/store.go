// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package redisrule

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// Table 是规则表名（deploy/sql/170_redis_rules.sql）。
const Table = "dbt_redis_rule"

// Repo 是规则的存储。
type Repo interface {
	// Ready 表建好了吗；没建时 List 返回空、写操作应提示先执行脚本。
	Ready() bool
	List() ([]Rule, error)
	Get(id uint) (*Rule, error) // 不存在返回 nil, nil
	Create(r *Rule) error       // 回填 ID；名称重复返回 ErrNameTaken
	Update(r *Rule) error
	Delete(id uint) error
}

// 存储层错误。
var (
	ErrNotFound     = errors.New("规则不存在")
	ErrNameTaken    = errors.New("已有同名规则")
	ErrTableMissing = errors.New("规则表 dbt_redis_rule 不存在，请先执行 deploy/sql/170_redis_rules.sql")
)

// Source 是 SQL 上线读取规则的入口（sqlflow 依赖它，不依赖具体存储）。
type Source interface {
	// Rules 返回全部规则（含停用的，由 Evaluate 忽略）；ready=false 表示规则表还没建，按“没有任何规则”处理。
	Rules(ctx context.Context) (rules []Rule, ready bool, err error)
}

// SourceOf 把 Repo 包成 Source。
func SourceOf(r Repo) Source { return repoSource{r} }

type repoSource struct{ r Repo }

func (s repoSource) Rules(context.Context) ([]Rule, bool, error) {
	if !s.r.Ready() {
		return nil, false, nil
	}
	list, err := s.r.List()
	return list, true, err
}

// ---- GORM 存储 ----

type row struct {
	ID           uint      `gorm:"column:id;primaryKey"`
	Name         string    `gorm:"column:name"`
	Enabled      bool      `gorm:"column:enabled"`
	Commands     string    `gorm:"column:commands"`
	KeyPatterns  string    `gorm:"column:key_patterns"`
	InstanceIDs  string    `gorm:"column:instance_ids"`
	DBs          string    `gorm:"column:dbs"`
	MaxKeys      int       `gorm:"column:max_keys"`
	RecordBefore bool      `gorm:"column:record_before"`
	Description  string    `gorm:"column:description"`
	Remark       string    `gorm:"column:remark"`
	CreatedBy    string    `gorm:"column:created_by"`
	UpdatedBy    string    `gorm:"column:updated_by"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

func (row) TableName() string { return Table }

// GormRepo 是基于元数据库的存储。
type GormRepo struct {
	db *gorm.DB

	mu       sync.Mutex
	ready    bool
	checkedA time.Time
}

// NewGormRepo 创建存储。表是否存在在每次需要时检查（已存在则缓存，不存在则最多每 5 秒查一次）。
func NewGormRepo(db *gorm.DB) *GormRepo { return &GormRepo{db: db} }

func (g *GormRepo) Ready() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ready {
		return true
	}
	if time.Since(g.checkedA) < 5*time.Second {
		return false
	}
	g.checkedA = time.Now()
	g.ready = g.db.Migrator().HasTable(Table)
	return g.ready
}

func (g *GormRepo) List() ([]Rule, error) {
	if !g.Ready() {
		return nil, nil
	}
	var rows []row
	if err := g.db.Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Rule, len(rows))
	for i, r := range rows {
		out[i] = fromRow(r)
	}
	return out, nil
}

func (g *GormRepo) Get(id uint) (*Rule, error) {
	if !g.Ready() {
		return nil, nil
	}
	var r row
	err := g.db.First(&r, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := fromRow(r)
	return &out, nil
}

func (g *GormRepo) Create(r *Rule) error {
	if !g.Ready() {
		return ErrTableMissing
	}
	var n int64
	if err := g.db.Model(&row{}).Where("name = ?", r.Name).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return ErrNameTaken
	}
	x := toRow(*r)
	if err := g.db.Create(&x).Error; err != nil {
		return err
	}
	r.ID = x.ID
	return nil
}

func (g *GormRepo) Update(r *Rule) error {
	if !g.Ready() {
		return ErrTableMissing
	}
	var n int64
	if err := g.db.Model(&row{}).Where("name = ? AND id <> ?", r.Name, r.ID).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return ErrNameTaken
	}
	x := toRow(*r)
	res := g.db.Model(&row{}).Where("id = ?", r.ID).
		Select("name", "enabled", "commands", "key_patterns", "instance_ids", "dbs", "max_keys", "record_before", "description", "remark", "updated_by", "updated_at").
		Updates(&x)
	if res.Error != nil {
		return res.Error
	}
	return nil
}

func (g *GormRepo) Delete(id uint) error {
	if !g.Ready() {
		return ErrTableMissing
	}
	return g.db.Delete(&row{}, id).Error
}

func fromRow(r row) Rule {
	out := Rule{ID: r.ID, Name: r.Name, Enabled: r.Enabled, MaxKeys: r.MaxKeys, RecordBefore: r.RecordBefore, Description: r.Description,
		Remark: r.Remark, CreatedBy: r.CreatedBy, UpdatedBy: r.UpdatedBy, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	if out.MaxKeys <= 0 {
		out.MaxKeys = DefaultMaxKey
	}
	for _, c := range strings.Split(r.Commands, ",") {
		if c = strings.ToUpper(strings.TrimSpace(c)); c != "" {
			out.Commands = append(out.Commands, c)
		}
	}
	_ = json.Unmarshal([]byte(r.KeyPatterns), &out.KeyPatterns)
	for _, s := range strings.Split(r.InstanceIDs, ",") {
		if n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32); err == nil && n > 0 {
			out.InstanceIDs = append(out.InstanceIDs, uint(n))
		}
	}
	for _, s := range strings.Split(r.DBs, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 0 {
			out.DBs = append(out.DBs, n)
		}
	}
	return out
}

func toRow(r Rule) row {
	pats, _ := json.Marshal(r.KeyPatterns)
	ids := make([]string, len(r.InstanceIDs))
	for i, id := range r.InstanceIDs {
		ids[i] = strconv.FormatUint(uint64(id), 10)
	}
	dbs := make([]string, len(r.DBs))
	for i, d := range r.DBs {
		dbs[i] = strconv.Itoa(d)
	}
	return row{ID: r.ID, Name: r.Name, Enabled: r.Enabled, Commands: strings.Join(r.Commands, ","), KeyPatterns: string(pats),
		InstanceIDs: strings.Join(ids, ","), DBs: strings.Join(dbs, ","), MaxKeys: r.MaxKeys, RecordBefore: r.RecordBefore,
		Description: r.Description, Remark: r.Remark, CreatedBy: r.CreatedBy, UpdatedBy: r.UpdatedBy, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

// ---- 内存存储（测试、演示）----

// MemRepo 是内存存储。
type MemRepo struct {
	mu       sync.Mutex
	next     uint
	rules    map[uint]Rule
	NotReady bool  // 模拟规则表不存在
	FailList error // 模拟读取失败
}

func NewMemRepo(rules ...Rule) *MemRepo {
	m := &MemRepo{next: 1, rules: map[uint]Rule{}}
	for _, r := range rules {
		_ = m.Create(&r)
	}
	return m
}

func (m *MemRepo) Ready() bool { return !m.NotReady }

func (m *MemRepo) List() ([]Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailList != nil {
		return nil, m.FailList
	}
	out := make([]Rule, 0, len(m.rules))
	for _, r := range m.rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemRepo) Get(id uint) (*Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rules[id]; ok {
		return &r, nil
	}
	return nil, nil
}

func (m *MemRepo) Create(r *Rule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.NotReady {
		return ErrTableMissing
	}
	for _, x := range m.rules {
		if x.Name == r.Name {
			return ErrNameTaken
		}
	}
	r.ID = m.next
	m.next++
	if r.MaxKeys <= 0 {
		r.MaxKeys = DefaultMaxKey
	}
	m.rules[r.ID] = *r
	return nil
}

func (m *MemRepo) Update(r *Rule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.rules {
		if x.Name == r.Name && x.ID != r.ID {
			return ErrNameTaken
		}
	}
	old, ok := m.rules[r.ID]
	if !ok {
		return ErrNotFound
	}
	r.CreatedBy, r.CreatedAt = old.CreatedBy, old.CreatedAt
	m.rules[r.ID] = *r
	return nil
}

func (m *MemRepo) Delete(id uint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rules, id)
	return nil
}
