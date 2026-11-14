// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package giconfig

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"dbtool/internal/crypto"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// store 是 Repo 的 GORM 实现。版本内容写库前用 mirage 加密（与系统设置同一把密钥）。
type store struct {
	db    *gorm.DB
	m     *crypto.Mirage
	ready atomic.Bool
}

func newStore(db *gorm.DB, m *crypto.Mirage) *store { return &store{db: db, m: m} }

var _ Repo = (*store)(nil)

// dbt_gi_config_version
type versionRow struct {
	ID            uint       `gorm:"primaryKey;column:id"`
	Version       int        `gorm:"column:version"`
	Content       string     `gorm:"column:content"`
	ContentSHA256 string     `gorm:"column:content_sha256"`
	ChangeSummary string     `gorm:"column:change_summary"`
	Operator      string     `gorm:"column:operator"`
	Status        string     `gorm:"column:status"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	AppliedAt     *time.Time `gorm:"column:applied_at"`
	Error         string     `gorm:"column:error"`
}

func (versionRow) TableName() string { return "dbt_gi_config_version" }

// dbt_gi_setting：部署设置、最近一次重启结果、操作锁。
type settingRow struct {
	Item      string    `gorm:"primaryKey;column:item"`
	Value     string    `gorm:"column:value"`
	UpdatedBy string    `gorm:"column:updated_by"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (settingRow) TableName() string { return "dbt_gi_setting" }

func (s *store) TableReady() bool {
	if s.ready.Load() {
		return true
	}
	if s.db.Migrator().HasTable(&versionRow{}) && s.db.Migrator().HasTable(&settingRow{}) {
		s.ready.Store(true)
		return true
	}
	return false
}

// ---- 系统设置（goInception 连接信息在 dbt_setting 里，键见 settings.KeyGoInception*）----

// ConfigValue 返回显式设置的值（敏感项已解密）。不用 settings 的默认值：页面据此区分“没配置”和“配置了”。
func (s *store) ConfigValue(item string) (string, bool) {
	return settings.Open(s.db, s.m).Lookup(item)
}

// SetConfigValues 在一个事务里写入多个设置项（校验后写入，空串 = 清除），变更记录里敏感项不带值。
func (s *store) SetConfigValues(items map[string]string) error {
	svc := settings.Open(s.db, s.m)
	ws, hs, err := svc.Prepare("", items)
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

// ---- 版本 ----

func (s *store) toVersion(r versionRow, withContent bool) *Version {
	v := &Version{ID: r.ID, Version: r.Version, ContentSHA256: r.ContentSHA256, ChangeSummary: r.ChangeSummary, Operator: r.Operator,
		Status: r.Status, CreatedAt: r.CreatedAt, AppliedAt: r.AppliedAt, Error: r.Error}
	if withContent {
		v.Content = s.m.Decrypt(r.Content)
	}
	return v
}

func (s *store) one(q *gorm.DB) (*Version, error) {
	var r versionRow
	err := q.Order("version DESC").Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.toVersion(r, true), nil
}

func (s *store) LatestVersion() (*Version, error) { return s.one(s.db) }

func (s *store) LatestByStatus(status string) (*Version, error) {
	return s.one(s.db.Where("status = ?", status))
}

func (s *store) GetVersion(num int) (*Version, error) { return s.one(s.db.Where("version = ?", num)) }

func (s *store) ListVersions(limit int) ([]Version, error) {
	var rows []versionRow
	err := s.db.Select("id, version, content_sha256, change_summary, operator, status, created_at, applied_at, error").
		Order("version DESC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]Version, 0, len(rows))
	for _, r := range rows {
		out = append(out, *s.toVersion(r, false))
	}
	return out, nil
}

func (s *store) InsertVersion(v *Version) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var max int
		if err := tx.Model(&versionRow{}).Select("COALESCE(MAX(version), 0)").Scan(&max).Error; err != nil {
			return err
		}
		row := versionRow{Version: max + 1, Content: s.m.Encrypt(v.Content), ContentSHA256: v.ContentSHA256, ChangeSummary: v.ChangeSummary,
			Operator: v.Operator, Status: v.Status, CreatedAt: v.CreatedAt, AppliedAt: v.AppliedAt, Error: v.Error}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		v.ID, v.Version = row.ID, row.Version
		return nil
	})
}

func (s *store) UpdateVersionResult(id uint, from []string, status string, appliedAt *time.Time, errMsg string) (bool, error) {
	upd := map[string]any{"status": status, "error": errMsg}
	if appliedAt != nil {
		upd["applied_at"] = *appliedAt
	}
	res := s.db.Model(&versionRow{}).Where("id = ? AND status IN ?", id, from).Updates(upd)
	return res.RowsAffected > 0, res.Error
}

// PruneVersions 只保留最新的 keep 个版本，以及最近一个已生效版本和最近一个待生效版本。
func (s *store) PruneVersions(keep int) error {
	var old []versionRow
	if err := s.db.Select("id, version, status").Order("version DESC").Offset(keep).Limit(1000).Find(&old).Error; err != nil {
		return err
	}
	if len(old) == 0 {
		return nil
	}
	protect := map[uint]bool{}
	for _, st := range []string{StatusApplied, StatusDraft} {
		if v, err := s.one(s.db.Where("status = ?", st)); err == nil && v != nil {
			protect[v.ID] = true
		}
	}
	var ids []uint
	for _, r := range old {
		if !protect[r.ID] {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return s.db.Where("id IN ?", ids).Delete(&versionRow{}).Error
}

// ---- 设置与状态 ----

func (s *store) getSetting(item string) (*settingRow, error) {
	var r settingRow
	err := s.db.Where("item = ?", item).Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &r, err
}

func (s *store) putSetting(item, value, by string) error {
	row := settingRow{Item: item, Value: value, UpdatedBy: by, UpdatedAt: time.Now()}
	return s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "item"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "updated_by", "updated_at"})}).Create(&row).Error
}

// LoadDeploy 读取部署设置。旧版本写入的 JSON（带 mode / unit / command / ssh 与 SSH 密文）也能读，已删除的字段忽略，见 parseDeploy。
func (s *store) LoadDeploy() (Deploy, bool, error) {
	r, err := s.getSetting("deploy")
	if err != nil || r == nil {
		return DefaultDeploy(), false, err
	}
	d, err := parseDeploy(r.Value)
	return d, err == nil, err
}

// SaveDeploy 保存部署设置：整份 JSON 重写，旧字段与 SSH 密文随之消失。
func (s *store) SaveDeploy(d Deploy, operator string) error {
	raw, err := encodeDeploy(d)
	if err != nil {
		return err
	}
	return s.putSetting("deploy", raw, operator)
}

func (s *store) LoadState(key string) (string, error) {
	r, err := s.getSetting("state_" + key)
	if err != nil || r == nil {
		return "", err
	}
	return r.Value, nil
}

func (s *store) SaveState(key, value string) error { return s.putSetting("state_"+key, value, "") }

// ---- 操作锁 ----

type lockDoc struct {
	Token     string `json:"token"`
	Owner     string `json:"owner"`
	ExpiresAt int64  `json:"expires_at"`
}

func (s *store) AcquireLock(owner string, ttl time.Duration) (string, bool, string, error) {
	var tb [16]byte
	if _, err := rand.Read(tb[:]); err != nil {
		return "", false, "", err
	}
	token := hex.EncodeToString(tb[:])
	now := time.Now()
	b, _ := json.Marshal(lockDoc{Token: token, Owner: owner, ExpiresAt: now.Add(ttl).Unix()})
	val := string(b)
	// 先尝试直接插入（主键冲突说明已有锁）
	res := s.db.Exec("INSERT IGNORE INTO dbt_gi_setting (item, `value`, updated_by, updated_at) VALUES ('lock', ?, ?, ?)", val, owner, now)
	if res.Error != nil {
		return "", false, "", res.Error
	}
	if res.RowsAffected == 1 {
		return token, true, "", nil
	}
	cur, err := s.getSetting("lock")
	if err != nil {
		return "", false, "", err
	}
	if cur == nil { // 刚好被释放：再试一次
		res = s.db.Exec("INSERT IGNORE INTO dbt_gi_setting (item, `value`, updated_by, updated_at) VALUES ('lock', ?, ?, ?)", val, owner, now)
		return token, res.Error == nil && res.RowsAffected == 1, "", res.Error
	}
	var ld lockDoc
	_ = json.Unmarshal([]byte(cur.Value), &ld)
	if ld.ExpiresAt > now.Unix() {
		return "", false, ld.Owner, nil
	}
	// 锁已过期（持有者崩溃）：用旧值做比较并交换，保证只有一个人接手
	res = s.db.Exec("UPDATE dbt_gi_setting SET `value` = ?, updated_by = ?, updated_at = ? WHERE item = 'lock' AND `value` = ?", val, owner, now, cur.Value)
	if res.Error != nil {
		return "", false, "", res.Error
	}
	return token, res.RowsAffected == 1, ld.Owner, nil
}

func (s *store) ReleaseLock(token string) error {
	return s.db.Exec("DELETE FROM dbt_gi_setting WHERE item = 'lock' AND `value` LIKE ?", `%"token":"`+token+`"%`).Error
}

func (s *store) InsertAudit(e *model.AuditEntry) error { return s.db.Create(e).Error }
