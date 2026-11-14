// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package authx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"dbtool/internal/crypto"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// store 是 Repo 的 GORM 实现。新表见 deploy/sql/018_auth.sql；设置读写走 internal/settings；用户（dbt_sql_users）、权限组（dbt_auth_group）、dbt_2fa_config、dbt_audit_log 只读写既有列。
type store struct {
	db *gorm.DB
	m  *crypto.Mirage

	pmu sync.Mutex // 旁路表 dbt_user_profile 是否存在的缓存
	pok bool
	pat time.Time
}

func newStore(db *gorm.DB, m *crypto.Mirage) *store { return &store{db: db, m: m} }

var _ Repo = (*store)(nil)

// ---- 表映射 ----

type providerRow struct {
	ID         uint       `gorm:"primaryKey"`
	Kind       string     `gorm:"column:kind"`
	Name       string     `gorm:"column:name"`
	Enabled    bool       `gorm:"column:enabled"`
	SortOrder  int        `gorm:"column:sort_order"`
	BtnLabel   string     `gorm:"column:btn_label"`
	FirstLogin string     `gorm:"column:first_login"`
	AuthGroups string     `gorm:"column:auth_groups"`
	ResGroups  string     `gorm:"column:resource_groups"`
	Config     string     `gorm:"column:config"`
	TestOK     *bool      `gorm:"column:last_test_ok"`
	TestMsg    string     `gorm:"column:last_test_msg"`
	TestTime   *time.Time `gorm:"column:last_test_time"`
	TestedHash string     `gorm:"column:tested_hash"`
	LastLogin  *time.Time `gorm:"column:last_login_time"`
	CreatedBy  string     `gorm:"column:created_by"`
	CreateTime time.Time  `gorm:"column:create_time;autoCreateTime"`
	SysTime    time.Time  `gorm:"column:sys_time;autoUpdateTime"`
}

func (providerRow) TableName() string { return "dbt_auth_provider" }

type identityRow struct {
	ID         uint      `gorm:"primaryKey"`
	ProviderID uint      `gorm:"column:provider_id"`
	Subject    string    `gorm:"column:subject"`
	UserID     uint      `gorm:"column:user_id"`
	CreateTime time.Time `gorm:"column:create_time;autoCreateTime"`
}

func (identityRow) TableName() string { return "dbt_auth_identity" }

// twoFARow 对应 dbt_2fa_config 表；username / auth_type / phone / secret_key 都是 mirage 加密字段。
type twoFARow struct {
	ID        uint    `gorm:"primaryKey"`
	Username  string  `gorm:"column:username"`
	AuthType  string  `gorm:"column:auth_type"`
	Phone     *string `gorm:"column:phone"`
	SecretKey *string `gorm:"column:secret_key"`
	UserID    uint    `gorm:"column:user_id"`
}

func (twoFARow) TableName() string { return "dbt_2fa_config" }

// ---- 登录方式 ----

func (s *store) encConfig(k Kind, c Config) (string, error) {
	for _, f := range secretFields(k) {
		p := c.secretPtr(f)
		if *p != "" {
			*p = s.m.Encrypt(*p)
		}
	}
	raw, err := json.Marshal(c)
	return string(raw), err
}

func (s *store) toProvider(r providerRow) Provider {
	p := Provider{ID: r.ID, Kind: Kind(r.Kind), Name: r.Name, Enabled: r.Enabled, Sort: r.SortOrder, BtnLabel: r.BtnLabel, FirstLogin: r.FirstLogin,
		AuthGroups: splitUints(r.AuthGroups), ResGroups: splitUints(r.ResGroups), TestOK: r.TestOK, TestMsg: r.TestMsg, TestTime: r.TestTime,
		TestedHash: r.TestedHash, LastLogin: r.LastLogin, CreatedBy: r.CreatedBy, CreateTime: r.CreateTime}
	_ = json.Unmarshal([]byte(r.Config), &p.Config)
	for _, f := range secretFields(p.Kind) {
		ptr := p.Config.secretPtr(f)
		if *ptr != "" {
			*ptr = s.m.Decrypt(*ptr)
		}
	}
	return p
}

func splitUints(s string) []uint {
	var out []uint
	for _, p := range splitList(s) {
		if n, err := strconv.ParseUint(p, 10, 32); err == nil {
			out = append(out, uint(n))
		}
	}
	return out
}

func (s *store) ListProviders() ([]Provider, error) {
	var rows []providerRow
	if err := s.db.Order("sort_order, id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Provider, len(rows))
	for i, r := range rows {
		out[i] = s.toProvider(r)
	}
	return out, nil
}

func (s *store) GetProvider(id uint) (*Provider, error) {
	var r providerRow
	if err := s.db.First(&r, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	p := s.toProvider(r)
	return &p, nil
}

func (s *store) SaveProvider(p *Provider) error {
	cfg, err := s.encConfig(p.Kind, p.Config)
	if err != nil {
		return err
	}
	fields := map[string]any{
		"name": p.Name, "enabled": p.Enabled, "sort_order": p.Sort, "btn_label": p.BtnLabel, "first_login": p.FirstLogin,
		"auth_groups": joinUints(p.AuthGroups), "resource_groups": joinUints(p.ResGroups), "config": cfg,
		"last_test_ok": p.TestOK, "last_test_msg": p.TestMsg, "last_test_time": p.TestTime, "tested_hash": p.TestedHash,
	}
	if p.ID == 0 {
		now := time.Now()
		r := providerRow{Kind: string(p.Kind), Name: p.Name, Enabled: p.Enabled, SortOrder: p.Sort, BtnLabel: p.BtnLabel, FirstLogin: p.FirstLogin,
			AuthGroups: joinUints(p.AuthGroups), ResGroups: joinUints(p.ResGroups), Config: cfg, TestOK: p.TestOK, TestMsg: p.TestMsg,
			TestTime: p.TestTime, TestedHash: p.TestedHash, CreatedBy: p.CreatedBy, CreateTime: now, SysTime: now}
		if err := s.db.Create(&r).Error; err != nil {
			return err
		}
		p.ID = r.ID
		return nil
	}
	fields["sys_time"] = time.Now()
	return s.db.Model(&providerRow{}).Where("id = ?", p.ID).Updates(fields).Error
}

func (s *store) DeleteProvider(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("provider_id = ?", id).Delete(&identityRow{}).Error; err != nil {
			return err
		}
		return tx.Delete(&providerRow{}, id).Error
	})
}

func (s *store) SetProviderTest(id uint, ok bool, msg, hash string, at time.Time) error {
	return s.db.Model(&providerRow{}).Where("id = ?", id).
		Updates(map[string]any{"last_test_ok": ok, "last_test_msg": msg, "tested_hash": hash, "last_test_time": at, "sys_time": time.Now()}).Error
}

func (s *store) SetProviderEnabled(id uint, enabled bool) error {
	return s.db.Model(&providerRow{}).Where("id = ?", id).Updates(map[string]any{"enabled": enabled, "sys_time": time.Now()}).Error
}

func (s *store) SetProviderOrder(ids []uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		for i, id := range ids {
			if err := tx.Model(&providerRow{}).Where("id = ?", id).Update("sort_order", (i+1)*10).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *store) TouchProviderLogin(id uint, at time.Time) error {
	return s.db.Model(&providerRow{}).Where("id = ?", id).Update("last_login_time", at).Error
}

// ---- 外部身份绑定 ----

// subj 把过长的外部 ID 压成固定长度（列宽 190，索引限制）。
func subj(subject string) string {
	if len(subject) <= 190 {
		return subject
	}
	h := sha256.Sum256([]byte(subject))
	return "sha256:" + hex.EncodeToString(h[:])
}

func (s *store) IdentityUser(providerID uint, subject string) (uint, error) {
	var r identityRow
	err := s.db.Where("provider_id = ? AND subject = ?", providerID, subj(subject)).First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	return r.UserID, err
}

func (s *store) BindIdentity(providerID uint, subject string, uid uint) error {
	r := identityRow{ProviderID: providerID, Subject: subj(subject), UserID: uid, CreateTime: time.Now()}
	return s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "provider_id"}, {Name: "subject"}}, DoUpdates: clause.AssignmentColumns([]string{"user_id"})}).Create(&r).Error
}

func (s *store) DeleteIdentity(providerID uint, subject string) error {
	return s.db.Where("provider_id = ? AND subject = ?", providerID, subj(subject)).Delete(&identityRow{}).Error
}

func (s *store) CountIdentities(providerID uint) (int, error) {
	var n int64
	err := s.db.Model(&identityRow{}).Where("provider_id = ?", providerID).Count(&n).Error
	return int(n), err
}

// ---- 用户 ----

func (s *store) firstUser(q *gorm.DB) (*model.User, error) {
	var u model.User
	if err := q.First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &u, nil
}

func (s *store) UserByID(id uint) (*model.User, error) { return s.firstUser(s.db.Where("id = ?", id)) }
func (s *store) UserByName(name string) (*model.User, error) {
	return s.firstUser(s.db.Where("username = ?", name))
}

func (s *store) UsersByEmail(email string) ([]model.User, error) {
	var out []model.User
	err := s.db.Where("email <> '' AND LOWER(email) = LOWER(?)", email).Limit(5).Find(&out).Error
	return out, err
}

var thirdIDCols = map[string]string{"ding": "ding_user_id", "feishu": "feishu_open_id", "slack": "slack_id"}

func (s *store) UserByThirdID(kind, id string) (*model.User, error) {
	col, ok := thirdIDCols[kind]
	if !ok || id == "" {
		return nil, nil
	}
	return s.firstUser(s.db.Where(col+" = ?", id)) // 列名来自白名单
}

func (s *store) CreateUser(u *model.User) error { return s.db.Omit("ResourceGroups").Create(u).Error }

func (s *store) RecordLoginFailure(uid uint, at time.Time) (int, error) {
	if err := s.db.Exec("UPDATE dbt_sql_users SET failed_login_count = LEAST(failed_login_count + 1, 127), last_login_failed_at = ? WHERE id = ?", at, uid).Error; err != nil {
		return 0, err
	}
	var n int
	err := s.db.Raw("SELECT failed_login_count FROM dbt_sql_users WHERE id = ?", uid).Scan(&n).Error
	return n, err
}

func (s *store) ResetLoginFailures(uid uint) error {
	return s.db.Exec("UPDATE dbt_sql_users SET failed_login_count = 0 WHERE id = ?", uid).Error
}

func (s *store) MarkLogin(uid uint, at time.Time) error {
	return s.db.Exec("UPDATE dbt_sql_users SET last_login = ?, failed_login_count = 0 WHERE id = ?", at, uid).Error
}

func (s *store) UpdateProfile(uid uint, display, email string) error {
	return s.db.Exec("UPDATE dbt_sql_users SET display = ?, email = ? WHERE id = ?", display, email, uid).Error
}

func (s *store) SetThirdID(uid uint, kind, id string) error {
	col, ok := thirdIDCols[kind]
	if !ok {
		return errors.New("不支持的第三方 ID 类型")
	}
	return s.db.Exec("UPDATE dbt_sql_users SET "+col+" = ? WHERE id = ?", id, uid).Error // 列名来自白名单
}

func (s *store) ListUsers(limit int) ([]IDName, error) {
	var out []IDName
	err := s.db.Table("dbt_sql_users").Select("id, username AS name").Where("is_active = 1").Order("username").Limit(limit).Scan(&out).Error
	return out, err
}

// ---- 组 ----

func (s *store) ListAuthGroups() ([]IDName, error) {
	var out []IDName
	err := s.db.Table("dbt_auth_group").Select("id, name").Order("name").Scan(&out).Error
	return out, err
}

func (s *store) ListResourceGroups() ([]IDName, error) {
	var out []IDName
	err := s.db.Table("dbt_resource_group").Select("group_id AS id, group_name AS name").Where("is_deleted = 0").Order("group_name").Scan(&out).Error
	return out, err
}

func (s *store) AuthGroupIDsByName(names []string) ([]uint, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var ids []uint
	err := s.db.Table("dbt_auth_group").Where("name IN ?", names).Pluck("id", &ids).Error
	return ids, err
}

func (s *store) ResourceGroupIDsByName(names []string) ([]uint, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var ids []uint
	err := s.db.Table("dbt_resource_group").Where("group_name IN ? AND is_deleted = 0", names).Pluck("group_id", &ids).Error
	return ids, err
}

func (s *store) AddUserAuthGroups(uid uint, ids []uint) error {
	for _, id := range ids {
		if err := s.db.Exec("INSERT IGNORE INTO dbt_sql_users_groups (users_id, group_id) VALUES (?, ?)", uid, id).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *store) AddUserResourceGroups(uid uint, ids []uint) error {
	for _, id := range ids {
		if err := s.db.Exec("INSERT IGNORE INTO dbt_sql_users_resource_group (users_id, resourcegroup_id) VALUES (?, ?)", uid, id).Error; err != nil {
			return err
		}
	}
	return nil
}

// ---- 设置 ----

func (s *store) svc() *settings.Service { return settings.Open(s.db, s.m) }

// ConfigValues 返回显式设置过的项（值已解密；没设置或空白的不返回）。
func (s *store) ConfigValues(items []string) (map[string]string, error) {
	svc := s.svc()
	out := map[string]string{}
	for _, it := range items {
		if v, ok := svc.Lookup(it); ok {
			out[it] = v
		}
	}
	return out, nil
}

// write 校验并写入系统设置（空串 = 恢复默认）；值不合法时返回错误、什么都不写。
func (s *store) write(kv map[string]string, by string) error {
	svc := s.svc()
	ws, hs, err := svc.Prepare(truncRunes(by, 30), kv)
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

func (s *store) SetConfigValues(kv map[string]string) error { return s.write(kv, "") }

// AuthSettings 返回登录与安全里新增的那组设置（含水印样式）中显式设置过的项。
func (s *store) AuthSettings() (map[string]string, error) { return s.ConfigValues(authSettingKeys) }

func (s *store) SetAuthSettings(kv map[string]string, by string) error { return s.write(kv, by) }

// ---- 两步验证 ----

func (s *store) TwoFAConfigs(uid uint) ([]TwoFAConfig, error) {
	var rows []twoFARow
	if err := s.db.Where("user_id = ?", uid).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]TwoFAConfig, 0, len(rows))
	for _, r := range rows {
		c := TwoFAConfig{Type: strings.TrimSpace(s.m.Decrypt(r.AuthType))}
		if r.Phone != nil {
			c.Phone = s.m.Decrypt(*r.Phone)
		}
		if r.SecretKey != nil {
			c.Secret = s.m.Decrypt(*r.SecretKey)
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *store) SaveTwoFA(uid uint, username string, c TwoFAConfig) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		// 同类型的旧配置先删（unique_together(user, auth_type) 的语义）；auth_type 是确定性加密，兼容历史明文
		if err := tx.Where("user_id = ? AND auth_type IN ?", uid, []string{s.m.Encrypt(c.Type), c.Type}).Delete(&twoFARow{}).Error; err != nil {
			return err
		}
		r := twoFARow{Username: s.m.Encrypt(username), AuthType: s.m.Encrypt(c.Type), UserID: uid}
		phone := s.m.Encrypt(c.Phone)
		r.Phone = &phone
		if c.Secret != "" {
			sk := s.m.Encrypt(c.Secret)
			r.SecretKey = &sk
		}
		return tx.Create(&r).Error
	})
}

func (s *store) DeleteTwoFA(uid uint, typ string) error {
	return s.db.Where("user_id = ? AND auth_type IN ?", uid, []string{s.m.Encrypt(typ), typ}).Delete(&twoFARow{}).Error
}

// ---- 审计 ----

func (s *store) InsertAudit(e *model.AuditEntry) error { return s.db.Create(e).Error }

// ---- 密码、手机号、账号绑定、概览 ----

func (s *store) SetPassword(uid uint, hash string) error {
	return s.db.Exec("UPDATE dbt_sql_users SET password = ? WHERE id = ?", hash, uid).Error
}

// UsersByPhone 按短信两步验证里绑定的号码找用户。dbt_2fa_config 的 phone / auth_type 是 mirage 确定性加密字段，
// 所以可以直接拿加密后的值去比；同时兼容历史明文。
func (s *store) UsersByPhone(phone string) ([]model.User, error) {
	var ids []uint
	err := s.db.Table("dbt_2fa_config").Where("phone IN ? AND auth_type IN ?", []string{s.m.Encrypt(phone), phone}, []string{s.m.Encrypt("sms"), "sms"}).
		Limit(5).Pluck("user_id", &ids).Error
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	var out []model.User
	err = s.db.Where("id IN ? AND is_active = 1", ids).Limit(5).Find(&out).Error
	return out, err
}

func (s *store) IdentitiesByUser(uid uint) ([]BoundIdentity, error) {
	var rows []identityRow
	if err := s.db.Where("user_id = ?", uid).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]BoundIdentity, len(rows))
	for i, r := range rows {
		out[i] = BoundIdentity{ID: r.ID, ProviderID: r.ProviderID, CreateTime: r.CreateTime}
	}
	return out, nil
}

func (s *store) DeleteIdentityByID(id, uid uint) (bool, error) {
	res := s.db.Where("id = ? AND user_id = ?", id, uid).Delete(&identityRow{})
	return res.RowsAffected > 0, res.Error
}

// CountTwoFA 返回启用用户数与其中已绑定两步验证（任一方式）的用户数。
func (s *store) CountTwoFA() (bound, total int, err error) {
	var t, b int64
	if err = s.db.Table("dbt_sql_users").Where("is_active = 1").Count(&t).Error; err != nil {
		return 0, 0, err
	}
	if err = s.db.Table("dbt_2fa_config").Select("COUNT(DISTINCT user_id)").Scan(&b).Error; err != nil {
		return 0, int(t), err
	}
	return int(b), int(t), nil
}

// ---- 用户资料扩展（dbt_user_profile，系统管理维护；表不存在时全部返回零值）----

const profileTable = "dbt_user_profile"

var _ ProfileRepo = (*store)(nil)

// profileReady 判断旁路表是否存在：存在后一直为真，不存在时最多每分钟重查一次（表通常在运行中被运维建出来）。
func (s *store) profileReady() bool {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	if s.pok || time.Since(s.pat) < time.Minute {
		return s.pok
	}
	s.pat = time.Now()
	s.pok = s.db.Migrator().HasTable(profileTable)
	return s.pok
}

func (s *store) UserDept(uid uint) string {
	if !s.profileReady() {
		return ""
	}
	var d string
	if err := s.db.Table(profileTable).Select("dept").Where("user_id = ?", uid).Limit(1).Scan(&d).Error; err != nil {
		return ""
	}
	return d
}

func (s *store) MustChangePassword(uid uint) bool {
	if !s.profileReady() {
		return false
	}
	var n int
	if err := s.db.Table(profileTable).Select("must_change_password").Where("user_id = ?", uid).Limit(1).Scan(&n).Error; err != nil {
		return false
	}
	return n != 0
}

func (s *store) ClearMustChange(uid uint) error {
	if !s.profileReady() {
		return nil
	}
	return s.db.Exec("UPDATE `"+profileTable+"` SET must_change_password = 0, updated_at = ? WHERE user_id = ?", time.Now(), uid).Error
}

// ready 判断 018_auth.sql 创建的登录方式 / 外部身份表，以及系统设置表（260_setting.sql）是否都在。
func (s *store) ready() bool {
	m := s.db.Migrator()
	return m.HasTable(&providerRow{}) && m.HasTable(&identityRow{}) && s.svc().Ready()
}
