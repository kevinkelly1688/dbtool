// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package btnperm

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"dbtool/internal/model"
)

// 本文件是 Source / Repo 的 GORM 实现。表：dbt_group_perm / dbt_user_perm / dbt_perm_setting（deploy/sql/019_btnperm.sql，
// 还没创建时一律按“没有数据”处理），以及只读的基础表 dbt_sql_users / dbt_sql_users_groups / dbt_auth_group；
// 一次性迁移（migrate.go）还会读早期的 dbt_auth_permission / dbt_content_type / dbt_sql_users_user_permissions / dbt_auth_group_permissions。
// 凡是拼进 SQL 的标识符都是本文件里的常量，用户输入一律走参数。

// GroupInfo 是权限组概要。
type GroupInfo struct {
	ID      uint   `json:"id"`
	Name    string `json:"name"`
	Members int    `json:"members"`
}

// UserInfo 是用户概要。
type UserInfo struct {
	ID        uint   `json:"id"`
	Username  string `json:"username"`
	Display   string `json:"display"`
	Active    bool   `json:"active"`
	Superuser bool   `json:"superuser"`
}

// Repo 是管理服务（Service）需要的数据访问。
type Repo interface {
	Source
	InTx(fn func(Repo) error) error

	GetGroup(id uint) (*GroupInfo, error) // 不存在返回 nil, nil
	ListGroups() ([]GroupInfo, error)
	GroupButtons(groupID uint) ([]string, error)
	AllGroupButtons() (map[uint][]string, error)
	ChangeGroupButtons(groupID uint, add, remove []string) error
	GroupDjangoCodes(groupID uint) ([]string, error) // 权限组在 sql 应用下的原权限码（只给一次性迁移用）

	GetUser(id uint) (*UserInfo, error) // 不存在返回 nil, nil
	UserOverrides(uid uint) ([]Override, error)
	UpsertOverrides(uid uint, list []Override) error
	DeleteOverrides(uid uint, codes []string) error
	// AllUserGroups 返回每个用户所在的权限组名（含停用的用户）；迁移用。
	AllUserGroups() (map[uint][]string, error)
	// AllUserDirectCodes 返回每个用户“单独授予”的原权限码（不含权限组带来的）；迁移用。
	AllUserDirectCodes() (map[uint][]string, error)

	SaveSetting(key, value, by string, at time.Time) error
	InsertAudit(e *model.AuditEntry) error
}

// GormRepo 是 Repo 的 GORM 实现。
type GormRepo struct{ db *gorm.DB }

// NewGormRepo 创建 GORM 实现；db 必须是普通连接（不是事务句柄）。
func NewGormRepo(db *gorm.DB) *GormRepo { return &GormRepo{db: db} }

var _ Repo = (*GormRepo)(nil)

func (s *GormRepo) InTx(fn func(Repo) error) error {
	return s.db.Transaction(func(tx *gorm.DB) error { return fn(&GormRepo{db: tx}) })
}

// quiet 返回不输出 SQL 日志的会话：按钮表还没创建时每次查询都会失败，不应刷满日志。
func (s *GormRepo) quiet() *gorm.DB {
	return s.db.Session(&gorm.Session{Logger: s.db.Logger.LogMode(logger.Silent)})
}

// missingTable 判断是不是“表不存在”（MySQL 1146）。
func missingTable(err error) bool {
	var me *mysqldrv.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1146
	}
	return err != nil && strings.Contains(err.Error(), "doesn't exist")
}

// tolerant 把“表不存在”当作成功（没有数据）。
func tolerant(err error) error {
	if missingTable(err) {
		return nil
	}
	return err
}

// ---- Source ----

func (s *GormRepo) Setting() (Setting, error) {
	var rows []struct{ Skey, Svalue string }
	if err := tolerant(s.quiet().Raw("SELECT skey, svalue FROM dbt_perm_setting").Scan(&rows).Error); err != nil {
		return DefaultSetting(), err
	}
	set := DefaultSetting()
	for _, r := range rows {
		switch r.Skey {
		case "mode":
			set.Mode = r.Svalue
		case "deny_display":
			set.DenyDisplay = r.Svalue
		}
	}
	return set.Normalize(), nil
}

func (s *GormRepo) Profile(uid uint) (*Profile, error) {
	var u []struct {
		IsActive    bool
		IsSuperuser bool
	}
	if err := s.db.Raw("SELECT is_active, is_superuser FROM dbt_sql_users WHERE id = ?", uid).Scan(&u).Error; err != nil {
		return nil, err
	}
	p := &Profile{GroupSource: map[string][]string{}}
	if len(u) == 0 || !u[0].IsActive {
		return p, nil
	}
	p.Active, p.Super = true, u[0].IsSuperuser
	var gp []struct{ Code, Name string }
	if err := tolerant(s.quiet().Raw(`SELECT gp.perm_code AS code, g.name AS name FROM dbt_group_perm gp
JOIN dbt_sql_users_groups ug ON ug.group_id = gp.group_id AND ug.users_id = ?
JOIN dbt_auth_group g ON g.id = gp.group_id ORDER BY g.name`, uid).Scan(&gp).Error); err != nil {
		return nil, err
	}
	for _, r := range gp {
		p.GroupSource[r.Code] = append(p.GroupSource[r.Code], r.Name)
	}
	ov, err := s.UserOverrides(uid)
	if err != nil {
		return nil, err
	}
	p.Overrides = ov
	return p, nil
}

// ---- 权限组 ----

func (s *GormRepo) GetGroup(id uint) (*GroupInfo, error) {
	var rows []GroupInfo
	err := s.db.Raw(`SELECT g.id AS id, g.name AS name,
  (SELECT COUNT(*) FROM dbt_sql_users_groups ug WHERE ug.group_id = g.id) AS members
FROM dbt_auth_group g WHERE g.id = ?`, id).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

func (s *GormRepo) ListGroups() ([]GroupInfo, error) {
	var rows []GroupInfo
	err := s.db.Raw(`SELECT g.id AS id, g.name AS name,
  (SELECT COUNT(*) FROM dbt_sql_users_groups ug WHERE ug.group_id = g.id) AS members
FROM dbt_auth_group g ORDER BY g.name`).Scan(&rows).Error
	return rows, err
}

func (s *GormRepo) GroupButtons(groupID uint) ([]string, error) {
	var out []string
	err := tolerant(s.quiet().Raw("SELECT perm_code FROM dbt_group_perm WHERE group_id = ? ORDER BY perm_code", groupID).Scan(&out).Error)
	return out, err
}

func (s *GormRepo) AllGroupButtons() (map[uint][]string, error) {
	var rows []struct {
		GroupID  uint
		PermCode string
	}
	if err := tolerant(s.quiet().Raw("SELECT group_id, perm_code FROM dbt_group_perm").Scan(&rows).Error); err != nil {
		return nil, err
	}
	out := map[uint][]string{}
	for _, r := range rows {
		out[r.GroupID] = append(out[r.GroupID], r.PermCode)
	}
	return out, nil
}

func (s *GormRepo) ChangeGroupButtons(groupID uint, add, remove []string) error {
	if len(remove) > 0 {
		if err := s.db.Exec("DELETE FROM dbt_group_perm WHERE group_id = ? AND perm_code IN ?", groupID, remove).Error; err != nil {
			return err
		}
	}
	if len(add) == 0 {
		return nil
	}
	ph := make([]string, len(add))
	args := make([]any, 0, len(add)*2)
	for i, c := range add {
		ph[i] = "(?, ?)"
		args = append(args, groupID, c)
	}
	return s.db.Exec("INSERT IGNORE INTO dbt_group_perm (group_id, perm_code) VALUES "+strings.Join(ph, ", "), args...).Error
}

func (s *GormRepo) GroupDjangoCodes(groupID uint) ([]string, error) {
	var out []string
	err := s.db.Raw(`SELECT DISTINCT p.codename FROM dbt_auth_group_permissions gp
JOIN dbt_auth_permission p ON p.id = gp.permission_id
JOIN dbt_content_type ct ON ct.id = p.content_type_id
WHERE gp.group_id = ? AND ct.app_label IN ('sql', 'sql_perf') ORDER BY p.codename`, groupID).Scan(&out).Error
	return out, err
}

// ---- 用户 ----

func (s *GormRepo) GetUser(id uint) (*UserInfo, error) {
	var rows []UserInfo
	err := s.db.Raw(`SELECT id, username, display, is_active AS active, is_superuser AS superuser FROM dbt_sql_users WHERE id = ?`, id).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

func (s *GormRepo) UserOverrides(uid uint) ([]Override, error) {
	var rows []struct {
		PermCode  string
		Effect    string
		ExpireAt  *time.Time
		Reason    string
		CreatedBy string
		CreatedAt time.Time
	}
	err := tolerant(s.quiet().Raw(`SELECT perm_code, effect, expire_at, reason, created_by, created_at FROM dbt_user_perm WHERE user_id = ? ORDER BY perm_code`, uid).Scan(&rows).Error)
	if err != nil {
		return nil, err
	}
	out := make([]Override, 0, len(rows))
	for _, r := range rows {
		out = append(out, Override{Code: r.PermCode, Effect: r.Effect, ExpireAt: r.ExpireAt, Reason: r.Reason, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

func (s *GormRepo) UpsertOverrides(uid uint, list []Override) error {
	for _, o := range list {
		err := s.db.Exec(`INSERT INTO dbt_user_perm (user_id, perm_code, effect, expire_at, reason, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE effect = VALUES(effect), expire_at = VALUES(expire_at), reason = VALUES(reason), created_by = VALUES(created_by), created_at = VALUES(created_at)`,
			uid, o.Code, o.Effect, o.ExpireAt, o.Reason, o.CreatedBy, o.CreatedAt).Error
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *GormRepo) DeleteOverrides(uid uint, codes []string) error {
	if len(codes) == 0 {
		return nil
	}
	return s.db.Exec("DELETE FROM dbt_user_perm WHERE user_id = ? AND perm_code IN ?", uid, codes).Error
}

// AllUserDirectCodes 一次取出每个用户“单独授予”的原权限码（迁移用）。
func (s *GormRepo) AllUserDirectCodes() (map[uint][]string, error) {
	var rows []struct {
		UserID   uint
		Codename string
		AppLabel string
	}
	err := tolerant(s.quiet().Raw(`
SELECT up.users_id AS user_id, p.codename AS codename, ct.app_label AS app_label FROM dbt_sql_users_user_permissions up
JOIN dbt_auth_permission p ON p.id = up.permission_id JOIN dbt_content_type ct ON ct.id = p.content_type_id
WHERE ct.app_label IN ('sql', 'sql_perf') ORDER BY up.users_id, p.codename`).Scan(&rows).Error)
	if err != nil {
		return nil, err
	}
	out := map[uint][]string{}
	for _, r := range rows {
		if r.AppLabel == "sql" || LegacyApp(r.Codename) == r.AppLabel {
			out[r.UserID] = append(out[r.UserID], r.Codename)
		}
	}
	return out, nil
}

// AllUserGroups 一次取出每个用户所在的权限组名（迁移用，含停用的用户）。
func (s *GormRepo) AllUserGroups() (map[uint][]string, error) {
	var rows []struct {
		UserID uint
		Name   string
	}
	if err := s.db.Raw(`SELECT ug.users_id AS user_id, g.name AS name FROM dbt_sql_users_groups ug JOIN dbt_auth_group g ON g.id = ug.group_id ORDER BY g.name`).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := map[uint][]string{}
	for _, r := range rows {
		out[r.UserID] = append(out[r.UserID], r.Name)
	}
	return out, nil
}

func (s *GormRepo) SaveSetting(key, value, by string, at time.Time) error {
	return s.db.Exec(`INSERT INTO dbt_perm_setting (skey, svalue, updated_by, updated_at) VALUES (?, ?, ?, ?)
ON DUPLICATE KEY UPDATE svalue = VALUES(svalue), updated_by = VALUES(updated_by), updated_at = VALUES(updated_at)`, key, value, by, at).Error
}

func (s *GormRepo) InsertAudit(e *model.AuditEntry) error { return s.db.Create(e).Error }

// TablesReady 判断三张按钮权限表是否已创建（页面上提示“请先执行 019_btnperm.sql”）。
func (s *GormRepo) TablesReady() (bool, error) {
	for _, t := range []string{"dbt_group_perm", "dbt_user_perm", "dbt_perm_setting"} {
		var n int64
		if err := s.db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", t).Scan(&n).Error; err != nil {
			return false, err
		}
		if n == 0 {
			return false, nil
		}
	}
	return true, nil
}

// sortedCopy 返回排序后的副本。
func sortedCopy(in []string) []string {
	out := append([]string{}, in...) // 非 nil：JSON 里是 [] 而不是 null
	sort.Strings(out)
	return out
}

func errf(format string, a ...any) error { return fmt.Errorf(format, a...) }
