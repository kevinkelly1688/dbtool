// @author: kevinkelly
// @license: Apache Licence
// @file: store_test.go
// @time: 2026/10/28

package btnperm

import (
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"dbtool/internal/model"
)

// 记录 SQL 的假驱动：查询返回空结果集（或指定的错误）、写入返回影响 1 行。不连接任何真实数据库，
// 只用来确认 GORM 实现里的每个方法能正常生成并执行 SQL（参数个数、IN 展开、事务），并检查关键语句的形态。
// 这不等于在真库验证过。
type recDriver struct {
	mu      sync.Mutex
	logs    []string
	args    [][]driver.Value
	failFor []string // 查询语句含其中任一子串时返回 MySQL 1146（表不存在）
}

func (d *recDriver) record(q string, args []driver.Value) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logs = append(d.logs, strings.Join(strings.Fields(q), " "))
	d.args = append(d.args, args)
}

func (d *recDriver) Open(string) (driver.Conn, error) { return &recConn{d}, nil }

type recConn struct{ d *recDriver }

func (c *recConn) Prepare(q string) (driver.Stmt, error) { return &recStmt{c.d, q}, nil }
func (c *recConn) Close() error                          { return nil }
func (c *recConn) Begin() (driver.Tx, error)             { c.d.record("BEGIN", nil); return recTx{c.d}, nil }

type recTx struct{ d *recDriver }

func (t recTx) Commit() error   { t.d.record("COMMIT", nil); return nil }
func (t recTx) Rollback() error { t.d.record("ROLLBACK", nil); return nil }

type recStmt struct {
	d *recDriver
	q string
}

func (s *recStmt) Close() error  { return nil }
func (s *recStmt) NumInput() int { return -1 }
func (s *recStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.d.record(s.q, args)
	return recResult{}, nil
}
func (s *recStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.d.record(s.q, args)
	for _, sub := range s.d.failFor {
		if strings.Contains(s.q, sub) {
			return nil, &mysqldrv.MySQLError{Number: 1146, Message: "Table 'x' does not exist"}
		}
	}
	return emptyRows{}, nil
}

type recResult struct{}

func (recResult) LastInsertId() (int64, error) { return 1, nil }
func (recResult) RowsAffected() (int64, error) { return 1, nil }

type emptyRows struct{}

func (emptyRows) Columns() []string              { return []string{"c"} }
func (emptyRows) Close() error                   { return nil }
func (emptyRows) Next(dest []driver.Value) error { return io.EOF }

func newRecRepo(t *testing.T) (*GormRepo, *recDriver) {
	t.Helper()
	d := &recDriver{}
	name := "btnperm-rec-" + t.Name()
	sql.Register(name, d)
	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{
		Logger: logger.Discard, NamingStrategy: schema.NamingStrategy{SingularTable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewGormRepo(db), d
}

func (d *recDriver) has(sub string) bool { return d.find(sub) >= 0 }

func (d *recDriver) find(sub string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, l := range d.logs {
		if strings.Contains(l, sub) {
			return i
		}
	}
	return -1
}

func (d *recDriver) argsOf(sub string) []driver.Value {
	i := d.find(sub)
	if i < 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.args[i]
}

func (d *recDriver) reset() { d.mu.Lock(); d.logs, d.args = nil, nil; d.mu.Unlock() }

func noErr(t *testing.T, name string, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", name, err)
	}
}

func TestGormRepoExecutesAllStatements(t *testing.T) {
	s, d := newRecRepo(t)
	now := time.Now()
	exp := now.Add(time.Hour)

	_, err := s.Setting()
	noErr(t, "Setting", err)
	if !d.has("FROM dbt_perm_setting") {
		t.Error("应读 dbt_perm_setting")
	}
	_, err = s.Profile(7)
	noErr(t, "Profile", err) // 用户不存在：只查 dbt_sql_users 就返回
	if !d.has("FROM dbt_sql_users WHERE id = ?") {
		t.Error("应读 dbt_sql_users")
	}

	_, err = s.GetGroup(1)
	noErr(t, "GetGroup", err)
	_, err = s.ListGroups()
	noErr(t, "ListGroups", err)
	_, err = s.GroupButtons(1)
	noErr(t, "GroupButtons", err)
	_, err = s.AllGroupButtons()
	noErr(t, "AllGroupButtons", err)
	_, err = s.GroupDjangoCodes(1)
	noErr(t, "GroupDjangoCodes", err)
	_, err = s.GetUser(1)
	noErr(t, "GetUser", err)
	_, err = s.UserOverrides(1)
	noErr(t, "UserOverrides", err)
	_, err = s.AllUserGroups()
	noErr(t, "AllUserGroups", err)
	if !d.has("FROM dbt_sql_users_groups ug JOIN dbt_auth_group g") {
		t.Error("AllUserGroups 应读 dbt_sql_users_groups")
	}
	_, err = s.AllUserDirectCodes()
	noErr(t, "AllUserDirectCodes", err)
	if !d.has("ct.app_label IN ('sql', 'sql_perf')") {
		t.Error("AllUserDirectCodes 应同时取 sql 与 sql_perf")
	}
	_, err = s.TablesReady()
	noErr(t, "TablesReady", err)

	d.reset()
	noErr(t, "ChangeGroupButtons", s.ChangeGroupButtons(3, []string{"a.b", "c.d"}, []string{"x.y", "z.w"}))
	if !d.has("DELETE FROM dbt_group_perm WHERE group_id = ? AND perm_code IN (?,?)") {
		t.Errorf("DELETE 应展开 IN: %v", d.logs)
	}
	if !d.has("INSERT IGNORE INTO dbt_group_perm (group_id, perm_code) VALUES (?, ?), (?, ?)") {
		t.Errorf("批量 INSERT IGNORE: %v", d.logs)
	}
	if a := d.argsOf("INSERT IGNORE"); len(a) != 4 {
		t.Errorf("INSERT 参数 = %v", a)
	}
	d.reset()
	noErr(t, "ChangeGroupButtons(空)", s.ChangeGroupButtons(3, nil, nil))
	if len(d.logs) != 0 {
		t.Error("没有变化不应执行 SQL")
	}

	noErr(t, "UpsertOverrides", s.UpsertOverrides(1, []Override{{Code: "a.b", Effect: EffectDeny, ExpireAt: &exp, Reason: "r", CreatedBy: "root", CreatedAt: now}, {Code: "c.d", Effect: EffectAllow, CreatedAt: now}}))
	if !d.has("INSERT INTO dbt_user_perm") || !d.has("ON DUPLICATE KEY UPDATE effect = VALUES(effect), expire_at = VALUES(expire_at)") {
		t.Error("单独授权应是 upsert（唯一键 user_id + perm_code）")
	}
	noErr(t, "DeleteOverrides", s.DeleteOverrides(1, []string{"a.b"}))
	if !d.has("DELETE FROM dbt_user_perm WHERE user_id = ? AND perm_code IN (?)") {
		t.Error("撤销应只删该用户的指定码")
	}
	noErr(t, "SaveSetting", s.SaveSetting("mode", ModeStrict, "root", now))
	if !d.has("INSERT INTO dbt_perm_setting") || !d.has("ON DUPLICATE KEY UPDATE svalue = VALUES(svalue)") {
		t.Error("设置应是 upsert")
	}
	noErr(t, "InsertAudit", s.InsertAudit(&model.AuditEntry{UserName: "root", Action: "修改按钮权限"}))
	if !d.has("`dbt_audit_log`") {
		t.Error("应写 dbt_audit_log")
	}

	// 事务
	d.reset()
	noErr(t, "InTx", s.InTx(func(tx Repo) error { return tx.ChangeGroupButtons(1, []string{"a.b"}, nil) }))
	if d.find("BEGIN") < 0 || d.find("INSERT IGNORE") < d.find("BEGIN") || d.find("COMMIT") < d.find("INSERT IGNORE") {
		t.Errorf("InTx 应 BEGIN → 写 → COMMIT: %v", d.logs)
	}
	d.reset()
	_ = s.InTx(func(tx Repo) error { _ = tx.ChangeGroupButtons(1, []string{"a.b"}, nil); return io.ErrUnexpectedEOF })
	if !d.has("ROLLBACK") || d.has("COMMIT") {
		t.Errorf("出错应 ROLLBACK: %v", d.logs)
	}
}

// 表还没创建（没执行 019_btnperm.sql）：读取按“没有数据”处理，行为与升级前一致。
func TestGormRepoMissingTablesTolerated(t *testing.T) {
	s, d := newRecRepo(t)
	// 基础表也带 dbt_ 前缀，所以只让 btnperm 自己的三张表“不存在”
	d.failFor = []string{"dbt_group_perm", "dbt_user_perm", "dbt_perm_setting"}
	set, err := s.Setting()
	if err != nil || set != DefaultSetting() {
		t.Fatalf("设置表不存在应是默认设置: %+v %v", set, err)
	}
	if o, err := s.UserOverrides(1); err != nil || len(o) != 0 {
		t.Fatalf("%v %v", o, err)
	}
	if c, err := s.GroupButtons(1); err != nil || len(c) != 0 {
		t.Fatalf("%v %v", c, err)
	}
	if m, err := s.AllGroupButtons(); err != nil || len(m) != 0 {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := s.AllUserGroups(); err != nil {
		t.Fatal(err)
	}
	if !missingTable(&mysqldrv.MySQLError{Number: 1146}) || missingTable(&mysqldrv.MySQLError{Number: 1062}) || missingTable(nil) {
		t.Error("missingTable 判断")
	}
}

// 解析器 + GORM 实现（假驱动）端到端：用户不存在 / 没有数据时不放行任何东西。
func TestResolverOverGormRepoEmpty(t *testing.T) {
	s, _ := newRecRepo(t)
	r := New(s)
	ok, err := r.HasButton(1, "sqlworkflow.approve")
	if err != nil || ok {
		t.Fatalf("%v %v", ok, err)
	}
	if got, err := r.AllowedButtons(1); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}
