// @author: kevinkelly
// @license: Apache Licence
// @file: mysqlstore.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"dbtool/internal/audit"
	"dbtool/internal/settings"
)

// mysqlStore 是 Store 的真实实现：整个运行期固定使用一个连接（会话级 SET 在后续语句里有效）。
type mysqlStore struct {
	db   *sql.DB
	conn *sql.Conn
	cols map[string]bool // dbt_sql_users 的列（懒加载）
}

var reIdent = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// mysqlConfig 返回连接 p 描述的库用的驱动配置。字符集必须用驱动选项设置：放进 cfg.Params 会被当成会话变量发
// SET charset=…，MySQL 8.0 / TiDB 报 1193（Unknown system variable）；选项会发 SET NAMES utf8mb4。
func mysqlConfig(p ConnParams) *mysql.Config {
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd = p.User, p.Pass
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(p.Host, p.Port)
	cfg.DBName = p.DB
	_ = cfg.Apply(mysql.Charset("utf8mb4", ""))
	cfg.ParseTime = true
	cfg.Loc = time.Local // 与 dbtool 服务一致：库里的时间是无时区的本地时间
	cfg.InterpolateParams = true
	cfg.Timeout = 10 * time.Second
	cfg.AllowNativePasswords = true
	if p.TLS != "" {
		cfg.TLSConfig = p.TLS
	}
	return cfg
}

// OpenMySQL 连接 p 描述的库。库不存在时返回 ErrDatabaseMissing（不自动建库）。
func OpenMySQL(ctx context.Context, p ConnParams) (Store, error) {
	cfg := mysqlConfig(p)
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, errors.New("连接参数无效")
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1049 {
			return nil, ErrDatabaseMissing
		}
		return nil, fmt.Errorf("连接数据库失败：%s", RedactMsg(err.Error()))
	}
	return &mysqlStore{db: db, conn: conn}, nil
}

func (s *mysqlStore) Close() error {
	_ = s.conn.Close()
	return s.db.Close()
}

type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

func (s *mysqlStore) ServerInfo(ctx context.Context) (ServerInfo, error) {
	var si ServerInfo
	err := s.conn.QueryRowContext(ctx, "SELECT VERSION(), IFNULL(DATABASE(), '')").Scan(&si.Version, &si.Database)
	return si, err
}

func hasTable(ctx context.Context, q querier, name string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", name).Scan(&n)
	return n > 0, err
}

func (s *mysqlStore) HasTable(ctx context.Context, name string) (bool, error) {
	return hasTable(ctx, s.conn, name)
}

func (s *mysqlStore) CountRows(ctx context.Context, table string) (int64, error) {
	if !reIdent.MatchString(table) {
		return 0, fmt.Errorf("非法表名 %q", table)
	}
	var n int64
	err := s.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&n)
	return n, err
}

func (s *mysqlStore) CountBaseTables(ctx context.Context) (int, error) {
	var n int
	err := s.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'").Scan(&n)
	return n, err
}

func (s *mysqlStore) DatabaseCharset(ctx context.Context) (DBCharset, error) {
	var c DBCharset
	err := s.conn.QueryRowContext(ctx, "SELECT DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = DATABASE()").Scan(&c.Charset, &c.Collation)
	if errors.Is(err, sql.ErrNoRows) {
		return c, errors.New("information_schema.SCHEMATA 里没有当前库（账号可能对该库没有任何权限）")
	}
	return c, err
}

func (s *mysqlStore) ExecStmt(ctx context.Context, stmt string) error {
	_, err := s.conn.ExecContext(ctx, stmt)
	if err == nil {
		s.cols = nil // DDL 可能改了 dbt_sql_users
	}
	return err
}

const createAppliedSQL = "CREATE TABLE IF NOT EXISTS " + TblApplied + " (\n" +
	"  file         VARCHAR(190) NOT NULL COMMENT '脚本文件（相对安装根目录，如 bootstrap/001_base_schema.sql）',\n" +
	"  sha256       CHAR(64)     NOT NULL COMMENT '最近一次成功执行时的文件内容 SHA-256',\n" +
	"  applied_at   DATETIME     NOT NULL COMMENT '最近一次成功执行的时间',\n" +
	"  tool_version VARCHAR(32)  NOT NULL DEFAULT '' COMMENT 'dbt-admin 版本',\n" +
	"  PRIMARY KEY (file)\n" +
	") ENGINE=InnoDB COMMENT='dbt-admin bootstrap 已应用的脚本'" // 不指定字符集 / 校验规则：跟随库默认（docs/charset-policy.md）

func (s *mysqlStore) EnsureAppliedTable(ctx context.Context) error {
	_, err := s.conn.ExecContext(ctx, createAppliedSQL)
	return err
}

func (s *mysqlStore) AppliedSHAs(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	ok, err := s.HasTable(ctx, TblApplied)
	if err != nil || !ok {
		return out, err
	}
	rows, err := s.conn.QueryContext(ctx, "SELECT file, sha256 FROM "+TblApplied)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f, h string
		if err := rows.Scan(&f, &h); err != nil {
			return nil, err
		}
		out[f] = h
	}
	return out, rows.Err()
}

func (s *mysqlStore) RecordApplied(ctx context.Context, file, sha, toolVersion string, at time.Time) error {
	_, err := s.conn.ExecContext(ctx, "INSERT INTO "+TblApplied+" (file, sha256, applied_at, tool_version) VALUES (?, ?, ?, ?) "+
		"ON DUPLICATE KEY UPDATE sha256 = VALUES(sha256), applied_at = VALUES(applied_at), tool_version = VALUES(tool_version)",
		file, sha, at, toolVersion)
	return err
}

// settingRaw 读 dbt_setting 里显式设置的一项（原始值，敏感项是密文）。表不存在或没有这一项返回 ok=false。
func (s *mysqlStore) settingRaw(ctx context.Context, key string) (string, bool, error) {
	ok, err := s.HasTable(ctx, tblSetting)
	if err != nil || !ok {
		return "", false, err
	}
	var v sql.NullString
	err = s.conn.QueryRowContext(ctx, "SELECT svalue FROM "+tblSetting+" WHERE skey = ? LIMIT 1", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v.String, v.Valid, err
}

func (s *mysqlStore) AuthSetting(ctx context.Context, key string) (string, bool, error) {
	return s.settingRaw(ctx, key)
}

func (s *mysqlStore) ConfigValue(ctx context.Context, item string) (string, bool, error) {
	return s.settingRaw(ctx, item)
}

// SettingRows 读 dbt_setting 的全部行（见 Store.SettingRows）。
func (s *mysqlStore) SettingRows(ctx context.Context) (map[string]settings.Row, bool, error) {
	ok, err := s.HasTable(ctx, tblSetting)
	if err != nil || !ok {
		return nil, false, err
	}
	rs, err := s.conn.QueryContext(ctx, "SELECT skey, svalue, encrypted, version, updated_by, update_time FROM "+tblSetting)
	if err != nil {
		return nil, true, err
	}
	defer rs.Close()
	out := map[string]settings.Row{}
	for rs.Next() {
		var r settings.Row
		var enc sql.NullBool
		var ver sql.NullInt64
		var by sql.NullString
		var at sql.NullTime
		if err := rs.Scan(&r.Key, &r.Value, &enc, &ver, &by, &at); err != nil {
			return nil, true, err
		}
		r.Encrypted, r.Version, r.UpdatedBy, r.UpdatedAt = enc.Bool, uint(ver.Int64), by.String, at.Time
		out[r.Key] = r
	}
	return out, true, rs.Err()
}

const userCols = "id, username, display, email, is_superuser, is_staff, is_active, last_login, failed_login_count, last_login_failed_at"

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	var u User
	var display, email sql.NullString
	var failed sql.NullInt64
	if err := sc.Scan(&u.ID, &u.Username, &display, &email, &u.IsSuperuser, &u.IsStaff, &u.IsActive, &u.LastLogin, &failed, &u.LastFailedAt); err != nil {
		return nil, err
	}
	u.Display, u.Email, u.FailedLoginCount = display.String, email.String, int(failed.Int64)
	return &u, nil
}

func (s *mysqlStore) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.conn.QueryContext(ctx, "SELECT "+userCols+" FROM "+tblUsers+" ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *mysqlStore) InTx(ctx context.Context, fn func(Tx) error) error {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(&mysqlTx{ctx: ctx, tx: tx, s: s}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// userColumns 返回 dbt_sql_users 的列集合（表结构新旧不一：slack_id 等是后加的列）。
func (s *mysqlStore) userColumns(ctx context.Context, q querier) (map[string]bool, error) {
	if s.cols != nil {
		return s.cols, nil
	}
	rows, err := q.QueryContext(ctx, "SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?", tblUsers)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols[strings.ToLower(c)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.cols = cols
	return cols, nil
}

type mysqlTx struct {
	ctx context.Context
	tx  *sql.Tx
	s   *mysqlStore
}

func (t *mysqlTx) GetUser(username string) (*User, error) {
	u, err := scanUser(t.tx.QueryRowContext(t.ctx, "SELECT "+userCols+" FROM "+tblUsers+" WHERE username = ? LIMIT 1 FOR UPDATE", username))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

func (t *mysqlTx) CountOtherActiveSuperusers(excludeID uint) (int, error) {
	var n int
	err := t.tx.QueryRowContext(t.ctx, "SELECT COUNT(*) FROM "+tblUsers+" WHERE is_superuser = 1 AND is_active = 1 AND id <> ? FOR UPDATE", excludeID).Scan(&n)
	return n, err
}

func (t *mysqlTx) GroupIDs(names []string) (map[string]uint, error) {
	out := map[string]uint{}
	if len(names) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}
	rows, err := t.tx.QueryContext(t.ctx, "SELECT id, name FROM "+tblGroups+" WHERE name IN ("+ph+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uint
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[name] = id
	}
	return out, rows.Err()
}

func (t *mysqlTx) InsertUser(u NewUser) (uint, error) {
	cols, err := t.s.userColumns(t.ctx, t.tx)
	if err != nil {
		return 0, err
	}
	names := []string{"password", "is_superuser", "username", "first_name", "last_name", "email", "is_staff", "is_active", "date_joined", "display"}
	args := []any{u.PasswordHash, u.IsSuperuser, u.Username, "", "", u.Email, u.IsStaff, u.IsActive, u.DateJoined, u.Display}
	// 这些列是 NOT NULL 且没有库级默认值：存在就必须显式给值
	for _, c := range []struct {
		name string
		val  any
	}{{"ding_user_id", ""}, {"slack_id", ""}, {"feishu_open_id", ""}, {"failed_login_count", 0}} {
		if cols[c.name] {
			names = append(names, c.name)
			args = append(args, c.val)
		}
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
	res, err := t.tx.ExecContext(t.ctx, "INSERT INTO "+tblUsers+" ("+strings.Join(names, ", ")+") VALUES ("+ph+")", args...)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	return uint(id), err
}

func (t *mysqlTx) AddUserGroups(userID uint, groupIDs []uint) error {
	if len(groupIDs) == 0 {
		return nil
	}
	ph := make([]string, len(groupIDs))
	args := make([]any, 0, len(groupIDs)*2)
	for i, g := range groupIDs {
		ph[i] = "(?, ?)"
		args = append(args, userID, g)
	}
	_, err := t.tx.ExecContext(t.ctx, "INSERT IGNORE INTO "+tblUserGroups+" (users_id, group_id) VALUES "+strings.Join(ph, ","), args...)
	return err
}

func (t *mysqlTx) SetPassword(userID uint, hash string) error {
	_, err := t.tx.ExecContext(t.ctx, "UPDATE "+tblUsers+" SET password = ?, failed_login_count = 0, last_login_failed_at = NULL WHERE id = ?", hash, userID)
	return err
}

func (t *mysqlTx) ClearFailedLogins(userID uint) error {
	_, err := t.tx.ExecContext(t.ctx, "UPDATE "+tblUsers+" SET failed_login_count = 0, last_login_failed_at = NULL WHERE id = ?", userID)
	return err
}

func (t *mysqlTx) SetActive(userID uint, active bool) error {
	_, err := t.tx.ExecContext(t.ctx, "UPDATE "+tblUsers+" SET is_active = ? WHERE id = ?", active, userID)
	return err
}

func (t *mysqlTx) AuthInfo(userID uint) (AuthInfo, error) {
	var ai AuthInfo
	ok, err := hasTable(t.ctx, t.tx, tblUserProfile)
	if err != nil {
		return ai, err
	}
	if ok {
		var typ sql.NullString
		var must sql.NullBool
		err := t.tx.QueryRowContext(t.ctx, "SELECT auth_type, must_change_password FROM "+tblUserProfile+" WHERE user_id = ? LIMIT 1", userID).Scan(&typ, &must)
		switch {
		case err == nil:
			ai.HasProfile, ai.ProfileType, ai.MustChange = true, typ.String, must.Bool
		case errors.Is(err, sql.ErrNoRows):
		default:
			return ai, err
		}
	}
	idOK, err := hasTable(t.ctx, t.tx, "dbt_auth_identity")
	if err != nil {
		return ai, err
	}
	prOK, err := hasTable(t.ctx, t.tx, "dbt_auth_provider")
	if err != nil {
		return ai, err
	}
	if idOK && prOK {
		rows, err := t.tx.QueryContext(t.ctx, "SELECT p.kind FROM dbt_auth_identity i JOIN dbt_auth_provider p ON p.id = i.provider_id WHERE i.user_id = ? ORDER BY p.kind", userID)
		if err != nil {
			return ai, err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				return ai, err
			}
			ai.Kinds = append(ai.Kinds, k)
		}
		if err := rows.Err(); err != nil {
			return ai, err
		}
	}
	return ai, nil
}

func (t *mysqlTx) SetMustChange(userID uint, v bool, by string, at time.Time) error {
	ok, err := hasTable(t.ctx, t.tx, tblUserProfile)
	if err != nil {
		return err
	}
	if !ok {
		return ErrTableMissing
	}
	_, err = t.tx.ExecContext(t.ctx, "INSERT INTO "+tblUserProfile+" (user_id, must_change_password, updated_by, updated_at) VALUES (?, ?, ?, ?) "+
		"ON DUPLICATE KEY UPDATE must_change_password = VALUES(must_change_password), updated_by = VALUES(updated_by), updated_at = VALUES(updated_at)",
		userID, v, truncRunes(by, 30), at)
	return err
}

func (t *mysqlTx) InsertAudit(e AuditEntry) error {
	ok, err := hasTable(t.ctx, t.tx, tblAuditLog)
	if err != nil {
		return err
	}
	if !ok {
		return ErrTableMissing
	}
	res, err := t.tx.ExecContext(t.ctx, "INSERT INTO "+tblAuditLog+" (user_id, user_name, user_display, action, extra_info, action_time) VALUES (?, ?, ?, ?, ?, ?)",
		0, truncRunes(e.UserName, 30), truncRunes(e.UserDisplay, 50), truncRunes(e.Action, 255), truncRunes(e.ExtraInfo, 4000), e.At)
	if err != nil {
		return err
	}
	// 系统审计页按类别 / 结果筛选读的是旁路表 dbt_audit_ext（服务进程里由 GORM 回调写入）：这里同样补一行，表不存在或写入失败都不影响审计本身。
	id, err := res.LastInsertId()
	if err != nil || id == 0 {
		return nil
	}
	if ok, err := hasTable(t.ctx, t.tx, tblAuditExt); err != nil || !ok {
		return nil
	}
	x := audit.Classify(e.Action, e.ExtraInfo)
	_, _ = t.tx.ExecContext(t.ctx, "INSERT INTO "+tblAuditExt+" (audit_id, result, ip, category, module, code, risky) VALUES (?, ?, ?, ?, ?, ?, ?) "+
		"ON DUPLICATE KEY UPDATE result = VALUES(result), category = VALUES(category), module = VALUES(module), code = VALUES(code), risky = VALUES(risky)",
		id, x.Result, "", x.Category, truncRunes(x.Module, 32), truncRunes(x.Code, 64), x.Risky)
	return nil
}

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// SecretRows 读 dbt_setting（encrypted=1）与 dbt_sql_instance 里需要密钥的列（见 Store.SecretRows）。
func (s *mysqlStore) SecretRows(ctx context.Context) ([]SecretRow, error) {
	var out []SecretRow
	if ok, err := s.HasTable(ctx, tblSetting); err != nil {
		return nil, err
	} else if ok {
		rs, err := s.conn.QueryContext(ctx, "SELECT skey, svalue FROM "+tblSetting+" WHERE encrypted = 1 ORDER BY skey")
		if err != nil {
			return nil, err
		}
		for rs.Next() {
			var key, v sql.NullString
			if err := rs.Scan(&key, &v); err != nil {
				rs.Close()
				return nil, err
			}
			out = append(out, SecretRow{Column: ColSettingValue, Ref: key.String, Value: v.String})
		}
		if err := rs.Err(); err != nil {
			rs.Close()
			return nil, err
		}
		rs.Close()
	}
	if ok, err := s.HasTable(ctx, tblInstance); err != nil {
		return nil, err
	} else if ok {
		rs, err := s.conn.QueryContext(ctx, "SELECT instance_name, `user`, `password` FROM "+tblInstance+" ORDER BY instance_name")
		if err != nil {
			return nil, err
		}
		defer rs.Close()
		for rs.Next() {
			var name, u, p sql.NullString
			if err := rs.Scan(&name, &u, &p); err != nil {
				return nil, err
			}
			out = append(out, SecretRow{Column: ColInstanceUser, Ref: name.String, Value: u.String},
				SecretRow{Column: ColInstancePassword, Ref: name.String, Value: p.String})
		}
		if err := rs.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// EncryptedCells 读 RekeyColumns 登记的各表里的加密列（见 Store.EncryptedCells）。列宽取自 information_schema，用来在换密钥前判断放不放得下。
func (s *mysqlStore) EncryptedCells(ctx context.Context) ([]Cell, error) {
	var out []Cell
	for _, spec := range RekeyColumns {
		ok, err := s.HasTable(ctx, spec.Table)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		widths := map[string]int{}
		rs, err := s.conn.QueryContext(ctx, "SELECT column_name, character_maximum_length FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?", spec.Table)
		if err != nil {
			return nil, err
		}
		for rs.Next() {
			var name string
			var n sql.NullInt64
			if err := rs.Scan(&name, &n); err != nil {
				rs.Close()
				return nil, err
			}
			w := 0
			if n.Valid && n.Int64 > 0 && n.Int64 < 1<<24 {
				w = int(n.Int64)
			}
			widths[strings.ToLower(name)] = w
		}
		if err := rs.Err(); err != nil {
			rs.Close()
			return nil, err
		}
		rs.Close()
		var cols []string // 库里真实存在的加密列（不同版本的表结构列不完全一样）
		for _, c := range spec.Cols {
			if _, has := widths[strings.ToLower(c)]; has {
				cols = append(cols, c)
			}
		}
		if len(cols) == 0 {
			continue
		}
		idCol := spec.IDCol
		if idCol == "" {
			idCol = "id"
		}
		idExpr := idCol // 主键列名是 RekeyColumns 里写死的常量；非缺省的加反引号
		if idCol != "id" {
			idExpr = "`" + idCol + "`"
		}
		sel := []string{idExpr}
		refCol := ""
		if _, has := widths[strings.ToLower(spec.RefCol)]; spec.RefCol != "" && has {
			refCol = spec.RefCol
			sel = append(sel, "`"+refCol+"`")
		}
		for _, c := range cols {
			sel = append(sel, "`"+c+"`")
		}
		query := "SELECT " + strings.Join(sel, ", ") + " FROM `" + spec.Table + "`"
		if spec.Where != "" {
			query += " WHERE " + spec.Where
		}
		rows, err := s.conn.QueryContext(ctx, query+" ORDER BY "+idExpr)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id sql.NullString
			var ref sql.NullString
			vals := make([]sql.NullString, len(cols))
			dest := []any{&id}
			if refCol != "" {
				dest = append(dest, &ref)
			}
			for i := range vals {
				dest = append(dest, &vals[i])
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return nil, err
			}
			name := id.String
			if idCol == "id" {
				name = "id=" + id.String
			}
			if ref.Valid && ref.String != "" {
				name = ref.String
			}
			for i, c := range cols {
				if vals[i].Valid && vals[i].String != "" {
					out = append(out, Cell{Table: spec.Table, Column: c, ID: id.String, Ref: name, Value: vals[i].String, MaxLen: widths[strings.ToLower(c)]})
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// ReplaceCell 见 Tx.ReplaceCell。表名 / 列名只接受 RekeyColumns 里登记的（拼进语句前的白名单校验）。
func (t *mysqlTx) ReplaceCell(table, column, id, oldValue, newValue string) (bool, error) {
	if !rekeyAllowed(table, column) {
		return false, fmt.Errorf("不允许改 %s.%s", table, column)
	}
	idCol := "id"
	for _, sp := range RekeyColumns {
		if sp.Table == table && sp.IDCol != "" {
			idCol = sp.IDCol
		}
	}
	res, err := t.tx.ExecContext(t.ctx, "UPDATE `"+table+"` SET `"+column+"` = ? WHERE `"+idCol+"` = ? AND BINARY `"+column+"` = BINARY ?", newValue, id, oldValue)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func rekeyAllowed(table, column string) bool {
	for _, s := range RekeyColumns {
		if s.Table != table {
			continue
		}
		for _, c := range s.Cols {
			if c == column {
				return true
			}
		}
	}
	return false
}

// ApplySettings 写 dbt_setting 与 dbt_setting_history（语句与 settings.WriteTx 一致）。
func (t *mysqlTx) ApplySettings(writes []settings.Write, hist []settings.History) error {
	now := time.Now()
	for _, w := range writes {
		var err error
		switch {
		case w.Delete:
			_, err = t.tx.ExecContext(t.ctx, "DELETE FROM "+tblSetting+" WHERE skey = ?", w.Key)
		case w.SkipExisting:
			_, err = t.tx.ExecContext(t.ctx, "INSERT IGNORE INTO "+tblSetting+" (skey, svalue, encrypted, version, updated_by, update_time) VALUES (?, ?, ?, 1, ?, ?)",
				w.Key, w.Value, w.Encrypted, w.By, now)
		default:
			_, err = t.tx.ExecContext(t.ctx, "INSERT INTO "+tblSetting+" (skey, svalue, encrypted, version, updated_by, update_time) VALUES (?, ?, ?, 1, ?, ?) "+
				"ON DUPLICATE KEY UPDATE svalue = VALUES(svalue), encrypted = VALUES(encrypted), version = version + 1, updated_by = VALUES(updated_by), update_time = VALUES(update_time)",
				w.Key, w.Value, w.Encrypted, w.By, now)
		}
		if err != nil {
			return err
		}
	}
	for _, h := range hist {
		if _, err := t.tx.ExecContext(t.ctx, "INSERT INTO dbt_setting_history (skey, old_value, new_value, is_secret, action, changed_by, change_time) VALUES (?, ?, ?, ?, ?, ?, ?)",
			h.Key, h.Old, h.New, h.Secret, h.Action, h.By, now); err != nil {
			return err
		}
	}
	return nil
}
