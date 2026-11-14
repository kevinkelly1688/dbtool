// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package duty

import (
	"errors"
	"sort"
	"sync"
	"time"

	"gorm.io/gorm"

	"dbtool/internal/model"
)

var (
	// ErrTableMissing 表示值班表还没建（没执行 deploy/sql/300_duty.sql）。
	ErrTableMissing = errors.New("duty: tables missing")
	// ErrNotFound 表示班次 / 调班不存在。
	ErrNotFound = errors.New("duty: not found")
)

// Store 是班次与调班的存取。
type Store interface {
	// Ready 报告两张表是否都已创建。
	Ready() bool
	ListShifts() ([]Shift, error)
	GetShift(id uint) (*Shift, error)
	// SaveShift 保存班次：ID 为 0 时新增（回填 ID），否则整行覆盖。
	SaveShift(sh *Shift) error
	DeleteShift(id uint) error
	// ListOverrides 返回结束时间不早于 since 的调班（含已撤销的），按开始时间排序。
	ListOverrides(since time.Time) ([]Override, error)
	GetOverride(id uint) (*Override, error)
	CreateOverride(o *Override) error
	CancelOverride(id uint, by string, at time.Time) error
}

// Directory 是用户目录：值班人从这里选，也从这里确认账号是否还可用。
type Directory interface {
	// List 返回启用的账号，按登录名排序（管理页的下拉用）。
	List() ([]Person, error)
	// Lookup 按登录名取账号（含已停用的）；不存在的不在结果里。
	Lookup(usernames []string) (map[string]Person, error)
}

// ---------------------------------------------------------------- 内存实现（测试 / 演示）

// MemoryStore 是内存里的 Store。
type MemoryStore struct {
	mu        sync.Mutex
	NotReady  bool // 测试用：模拟表没建
	shifts    []Shift
	overrides []Override
	nextShift uint
	nextOv    uint
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{nextShift: 1, nextOv: 1} }

func (m *MemoryStore) Ready() bool { return !m.NotReady }

func (m *MemoryStore) ListShifts() ([]Shift, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.NotReady {
		return nil, ErrTableMissing
	}
	return append([]Shift(nil), m.shifts...), nil
}

func (m *MemoryStore) GetShift(id uint) (*Shift, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.NotReady {
		return nil, ErrTableMissing
	}
	for _, s := range m.shifts {
		if s.ID == id {
			c := s
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemoryStore) SaveShift(sh *Shift) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.NotReady {
		return ErrTableMissing
	}
	if sh.ID == 0 {
		sh.ID = m.nextShift
		m.nextShift++
		m.shifts = append(m.shifts, *sh)
		return nil
	}
	for i := range m.shifts {
		if m.shifts[i].ID == sh.ID {
			m.shifts[i] = *sh
			return nil
		}
	}
	return ErrNotFound
}

func (m *MemoryStore) DeleteShift(id uint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.NotReady {
		return ErrTableMissing
	}
	for i := range m.shifts {
		if m.shifts[i].ID == id {
			m.shifts = append(m.shifts[:i], m.shifts[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (m *MemoryStore) ListOverrides(since time.Time) ([]Override, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.NotReady {
		return nil, ErrTableMissing
	}
	var out []Override
	for _, o := range m.overrides {
		if !o.End.Before(since) {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

func (m *MemoryStore) GetOverride(id uint) (*Override, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.NotReady {
		return nil, ErrTableMissing
	}
	for _, o := range m.overrides {
		if o.ID == id {
			c := o
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemoryStore) CreateOverride(o *Override) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.NotReady {
		return ErrTableMissing
	}
	o.ID = m.nextOv
	m.nextOv++
	m.overrides = append(m.overrides, *o)
	return nil
}

func (m *MemoryStore) CancelOverride(id uint, by string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.NotReady {
		return ErrTableMissing
	}
	for i := range m.overrides {
		if m.overrides[i].ID == id {
			m.overrides[i].Canceled, m.overrides[i].CanceledBy, m.overrides[i].CanceledAt = true, by, &at
			return nil
		}
	}
	return ErrNotFound
}

// StaticDirectory 是固定名单的 Directory（测试 / 演示）。
type StaticDirectory []Person

func (d StaticDirectory) List() ([]Person, error) {
	var out []Person
	for _, p := range d {
		if p.Active {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

func (d StaticDirectory) Lookup(names []string) (map[string]Person, error) {
	out := map[string]Person{}
	for _, n := range names {
		for _, p := range d {
			if p.Username == n {
				out[n] = p
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- 元数据库实现

const (
	shiftTable    = "dbt_duty_shift"
	overrideTable = "dbt_duty_override"

	recheckEvery = 10 * time.Second // 表没建时，隔多久再查一次是否已建好
	listUsersMax = 2000
)

type shiftRow struct {
	ID        uint      `gorm:"primaryKey;column:id"`
	Name      string    `gorm:"column:name"`
	Username  string    `gorm:"column:username"`
	StartMin  int       `gorm:"column:start_min"`
	EndMin    int       `gorm:"column:end_min"`
	Weekdays  int       `gorm:"column:weekdays"`
	Contact   string    `gorm:"column:contact"`
	Remark    string    `gorm:"column:remark"`
	Enabled   bool      `gorm:"column:enabled"`
	CreatedBy string    `gorm:"column:created_by"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedBy string    `gorm:"column:updated_by"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (shiftRow) TableName() string { return shiftTable }

func (r shiftRow) shift() Shift {
	return Shift{ID: r.ID, Name: r.Name, Username: r.Username, StartMin: r.StartMin, EndMin: r.EndMin, Weekdays: uint8(r.Weekdays),
		Contact: r.Contact, Remark: r.Remark, Enabled: r.Enabled, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, UpdatedBy: r.UpdatedBy, UpdatedAt: r.UpdatedAt}
}

func rowOfShift(s Shift) shiftRow {
	return shiftRow{ID: s.ID, Name: s.Name, Username: s.Username, StartMin: s.StartMin, EndMin: s.EndMin, Weekdays: int(s.Weekdays),
		Contact: s.Contact, Remark: s.Remark, Enabled: s.Enabled, CreatedBy: s.CreatedBy, CreatedAt: s.CreatedAt, UpdatedBy: s.UpdatedBy, UpdatedAt: s.UpdatedAt}
}

type overrideRow struct {
	ID         uint       `gorm:"primaryKey;column:id"`
	Username   string     `gorm:"column:username"`
	StartAt    time.Time  `gorm:"column:start_at"`
	EndAt      time.Time  `gorm:"column:end_at"`
	Reason     string     `gorm:"column:reason"`
	Contact    string     `gorm:"column:contact"`
	Canceled   bool       `gorm:"column:canceled"`
	CanceledBy string     `gorm:"column:canceled_by"`
	CanceledAt *time.Time `gorm:"column:canceled_at"`
	CreatedBy  string     `gorm:"column:created_by"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
}

func (overrideRow) TableName() string { return overrideTable }

func (r overrideRow) override() Override {
	return Override{ID: r.ID, Username: r.Username, Start: r.StartAt, End: r.EndAt, Reason: r.Reason, Contact: r.Contact,
		Canceled: r.Canceled, CanceledBy: r.CanceledBy, CanceledAt: r.CanceledAt, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt}
}

// GormStore 是元数据库里的 Store。
type GormStore struct {
	db *gorm.DB

	mu      sync.Mutex
	ok      bool
	checked time.Time
}

func NewGormStore(db *gorm.DB) *GormStore { return &GormStore{db: db} }

func (g *GormStore) s() *gorm.DB { return g.db.Session(&gorm.Session{NewDB: true}) }

func (g *GormStore) Ready() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ok {
		return true
	}
	if !g.checked.IsZero() && time.Since(g.checked) < recheckEvery {
		return false
	}
	g.checked = time.Now()
	m := g.s().Migrator()
	g.ok = m.HasTable(shiftTable) && m.HasTable(overrideTable)
	return g.ok
}

func (g *GormStore) ListShifts() ([]Shift, error) {
	if !g.Ready() {
		return nil, ErrTableMissing
	}
	var rows []shiftRow
	if err := g.s().Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Shift, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.shift())
	}
	return out, nil
}

func (g *GormStore) GetShift(id uint) (*Shift, error) {
	if !g.Ready() {
		return nil, ErrTableMissing
	}
	var r shiftRow
	if err := g.s().Where("id = ?", id).Take(&r).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s := r.shift()
	return &s, nil
}

func (g *GormStore) SaveShift(sh *Shift) error {
	if !g.Ready() {
		return ErrTableMissing
	}
	r := rowOfShift(*sh)
	if sh.ID == 0 {
		if err := g.s().Create(&r).Error; err != nil {
			return err
		}
		sh.ID = r.ID
		return nil
	}
	return g.s().Model(&shiftRow{}).Where("id = ?", sh.ID).
		Select("name", "username", "start_min", "end_min", "weekdays", "contact", "remark", "enabled", "updated_by", "updated_at").
		Updates(&r).Error
}

func (g *GormStore) DeleteShift(id uint) error {
	if !g.Ready() {
		return ErrTableMissing
	}
	return g.s().Where("id = ?", id).Delete(&shiftRow{}).Error
}

func (g *GormStore) ListOverrides(since time.Time) ([]Override, error) {
	if !g.Ready() {
		return nil, ErrTableMissing
	}
	var rows []overrideRow
	if err := g.s().Where("end_at >= ?", since).Order("start_at, id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Override, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.override())
	}
	return out, nil
}

func (g *GormStore) GetOverride(id uint) (*Override, error) {
	if !g.Ready() {
		return nil, ErrTableMissing
	}
	var r overrideRow
	if err := g.s().Where("id = ?", id).Take(&r).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	o := r.override()
	return &o, nil
}

func (g *GormStore) CreateOverride(o *Override) error {
	if !g.Ready() {
		return ErrTableMissing
	}
	r := overrideRow{Username: o.Username, StartAt: o.Start, EndAt: o.End, Reason: o.Reason, Contact: o.Contact, CreatedBy: o.CreatedBy, CreatedAt: o.CreatedAt}
	if err := g.s().Create(&r).Error; err != nil {
		return err
	}
	o.ID = r.ID
	return nil
}

func (g *GormStore) CancelOverride(id uint, by string, at time.Time) error {
	if !g.Ready() {
		return ErrTableMissing
	}
	return g.s().Model(&overrideRow{}).Where("id = ? AND canceled = 0", id).
		Updates(map[string]any{"canceled": true, "canceled_by": by, "canceled_at": at}).Error
}

// GormDirectory 是元数据库用户表（dbt_sql_users）上的 Directory。
type GormDirectory struct{ db *gorm.DB }

func NewGormDirectory(db *gorm.DB) *GormDirectory { return &GormDirectory{db: db} }

func personOf(u model.User) Person {
	return Person{Username: u.Username, Display: u.Display, Email: u.Email, Active: u.IsActive}
}

func (d *GormDirectory) List() ([]Person, error) {
	var us []model.User
	err := d.db.Session(&gorm.Session{NewDB: true}).Select("username", "display", "email", "is_active").
		Where("is_active = ?", true).Order("username").Limit(listUsersMax).Find(&us).Error
	if err != nil {
		return nil, err
	}
	out := make([]Person, 0, len(us))
	for _, u := range us {
		out = append(out, personOf(u))
	}
	return out, nil
}

func (d *GormDirectory) Lookup(names []string) (map[string]Person, error) {
	out := map[string]Person{}
	if len(names) == 0 {
		return out, nil
	}
	var us []model.User
	err := d.db.Session(&gorm.Session{NewDB: true}).Select("username", "display", "email", "is_active").
		Where("username IN ?", names).Find(&us).Error
	if err != nil {
		return nil, err
	}
	for _, u := range us {
		out[u.Username] = personOf(u)
	}
	return out, nil
}
