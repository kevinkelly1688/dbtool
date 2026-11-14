// @author: kevinkelly
// @license: Apache Licence
// @file: store_test.go
// @time: 2026/10/28

package sysadmin

import (
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"dbtool/internal/crypto"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// 记录 SQL 的假驱动：查询返回空结果集、写入返回影响 1 行。不连接任何真实数据库，
// 只用来确认 store 里的每个方法能正常生成并执行 SQL（参数个数、IN 展开、事务）并检查关键语句的形态。
type recDriver struct {
	mu   sync.Mutex
	logs []string
}

func (d *recDriver) record(q string, args []driver.NamedValue) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logs = append(d.logs, strings.Join(strings.Fields(q), " "))
	_ = args
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
	s.d.record(s.q, nil)
	return recResult{}, nil
}
func (s *recStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.d.record(s.q, nil)
	return &emptyRows{}, nil
}

type emptyRows struct{}

func (emptyRows) Columns() []string              { return []string{"c"} }
func (emptyRows) Close() error                   { return nil }
func (emptyRows) Next(dest []driver.Value) error { return io.EOF }

func newRecStore(t *testing.T) (*store, *recDriver) {
	t.Helper()
	d := &recDriver{}
	name := "sysadmin-rec-" + t.Name()
	sql.Register(name, d)
	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{
		Logger: logger.Discard, NamingStrategy: schema.NamingStrategy{SingularTable: true}, SkipDefaultTransaction: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := crypto.NewMirage("hfusaf2m4ot#7)fkw#di2bu6(cv0@opwmafx5n#6=3d%x^hpl6", "ECB", "")
	if err != nil {
		t.Fatal(err)
	}
	return newStore(db, m), d
}

func (d *recDriver) has(sub string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func (d *recDriver) reset() { d.mu.Lock(); d.logs = nil; d.mu.Unlock() }

func must(t *testing.T, name string, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", name, err)
	}
}

// TestStoreExecutesAllStatements 逐个调用 store 的方法，保证 SQL 能生成并执行，且关键语句符合预期。
func TestStoreExecutesAllStatements(t *testing.T) {
	s, d := newRecStore(t)
	now := time.Now()
	start := now.Add(-time.Hour)

	_, _, err := s.ListAuditLogs(AuditLogFilter{Action: "登入", User: "a_b%", Search: "x", Start: &start, End: &now, Limit: 10})
	must(t, "ListAuditLogs", err)
	if !d.has("`dbt_audit_log`") || !d.has("action_time >= ?") && !d.has("action_time >=") {
		t.Errorf("审计日志查询语句不对: %v", d.logs)
	}
	_, err = s.AuditActions()
	must(t, "AuditActions", err)
	_, _, err = s.ListQueryLogs(QueryLogFilter{User: "u", Instance: "i", Search: "s", Start: &start, End: &now, Limit: 10})
	must(t, "ListQueryLogs", err)
	_, _, err = s.ListSQLWorkflows(SQLWorkflowFilter{Status: "workflow_finish", GroupID: 1, Search: "x", Start: &start, End: &now, Limit: 10})
	must(t, "ListSQLWorkflows", err)
	if !d.has("LEFT JOIN dbt_sql_instance i ON i.id = w.instance_id") {
		t.Errorf("SQL 工单审计应关联实例名: %v", d.logs)
	}
	must(t, "InsertAuditLog", s.InsertAuditLog(&model.AuditEntry{UserID: 1, UserName: "a", Action: "x", ActionTime: now}))
	if !d.has("INSERT INTO `dbt_audit_log`") {
		t.Errorf("没有写 dbt_audit_log: %v", d.logs)
	}

	// 用户
	_, err = s.GetUser(1)
	must(t, "GetUser", err)
	_, err = s.GetUserByName("a")
	must(t, "GetUserByName", err)
	for _, st := range []string{StatusActive, StatusInactive, StatusLocked, StatusNo2FA, ""} {
		_, _, err = s.ListUsers(UserFilter{Search: "a", AuthGroupID: 1, ResourceGroupID: 2, Status: st, LockCount: 5, LockWindow: time.Minute, Now: now, Limit: 10})
		must(t, "ListUsers "+st, err)
	}
	if !d.has("NOT EXISTS (SELECT 1 FROM `dbt_2fa_config` t WHERE t.user_id = dbt_sql_users.id)") {
		t.Errorf("未绑定 2FA 的筛选语句不对: %v", d.logs)
	}
	must(t, "CreateUser", s.CreateUser(&model.User{Username: "x", Password: "h", DateJoined: now}))
	d.reset()
	must(t, "UpdateUser", s.UpdateUser(3, map[string]any{"failed_login_count": 0, "is_active": false}))
	if !d.has("UPDATE `dbt_sql_users` SET") || !d.has("WHERE id = ?") && !d.has("WHERE id =") {
		t.Errorf("更新用户语句不对: %v", d.logs)
	}
	d.reset()
	n, err := s.CountOtherSuperusers(1)
	must(t, "CountOtherSuperusers", err)
	if n != 0 || !d.has("FOR UPDATE") {
		t.Errorf("统计其他超级用户应加行锁: n=%d %v", n, d.logs)
	}
	_, err = s.UserAuthGroups([]uint{1, 2})
	must(t, "UserAuthGroups", err)
	_, err = s.UserResourceGroups([]uint{1, 2})
	must(t, "UserResourceGroups", err)
	if !d.has("rg.is_deleted = 0") {
		t.Errorf("用户资源组应只看未停用: %v", d.logs)
	}
	m, err := s.UserAuthGroups(nil)
	if err != nil || len(m) != 0 {
		t.Errorf("空 ID 列表不应查询: %v %v", m, err)
	}
	_, err = s.UserTwoFATypes([]uint{1})
	must(t, "UserTwoFATypes", err)
	d.reset()
	must(t, "SetUserAuthGroups", s.SetUserAuthGroups(1, []uint{10, 20}))
	must(t, "SetUserResourceGroups", s.SetUserResourceGroups(1, []uint{1}))
	if !d.has("INSERT IGNORE INTO `dbt_sql_users_groups` (`users_id`, `group_id`) VALUES (?, ?),(?, ?)") {
		t.Errorf("批量插入关联应使用 INSERT IGNORE: %v", d.logs)
	}
	if !d.has("JOIN dbt_resource_group rg ON rg.group_id = ur.resourcegroup_id AND rg.is_deleted = 0") {
		t.Errorf("设置用户资源组只应管理未停用的组: %v", d.logs)
	}
	d.reset()
	_, err = s.DeleteUserTwoFA(1)
	must(t, "DeleteUserTwoFA", err)
	if !d.has("DELETE FROM `dbt_2fa_config` WHERE user_id = ?") {
		t.Errorf("%v", d.logs)
	}

	// 权限组
	_, err = s.ListAuthGroups()
	must(t, "ListAuthGroups", err)
	_, err = s.GetAuthGroup(1)
	must(t, "GetAuthGroup", err)
	_, err = s.AuthGroupByName("x")
	must(t, "AuthGroupByName", err)
	must(t, "CreateAuthGroup", s.CreateAuthGroup(&model.AuthGroup{Name: "x"}))
	must(t, "RenameAuthGroup", s.RenameAuthGroup(1, "y"))
	d.reset()
	must(t, "DeleteAuthGroup", s.DeleteAuthGroup(1))
	if !d.has("DELETE FROM dbt_sql_users_groups WHERE group_id = ?") || !d.has("DELETE FROM dbt_auth_group_permissions WHERE group_id = ?") || !d.has("DELETE FROM dbt_auth_group WHERE id = ?") {
		t.Errorf("删除权限组要先清关联: %v", d.logs)
	}
	_, err = s.AuthGroupMembers(1)
	must(t, "AuthGroupMembers", err)
	_, err = s.PendingAuditsUsingGroup(1)
	must(t, "PendingAuditsUsingGroup", err)

	// 资源组
	_, err = s.ListResourceGroups(ResGroupFilter{Search: "a", IncludeDeleted: true})
	must(t, "ListResourceGroups", err)
	_, err = s.GetResourceGroup(1)
	must(t, "GetResourceGroup", err)
	_, err = s.ResourceGroupByName("x")
	must(t, "ResourceGroupByName", err)
	must(t, "CreateResourceGroup", s.CreateResourceGroup(&model.ResourceGroup{GroupName: "x", CreateTime: now, SysTime: now}))
	must(t, "UpdateResourceGroup", s.UpdateResourceGroup(1, map[string]any{"is_deleted": 1}))
	_, _, err = s.ResourceGroupCounts()
	must(t, "ResourceGroupCounts", err)
	_, err = s.GroupUsers(1)
	must(t, "GroupUsers", err)
	_, err = s.GroupInstances(1)
	must(t, "GroupInstances", err)
	_, err = s.AllUsers()
	must(t, "AllUsers", err)
	_, err = s.AllInstances()
	must(t, "AllInstances", err)
	d.reset()
	must(t, "AddGroupUsers", s.AddGroupUsers(1, []uint{1, 2}))
	must(t, "RemoveGroupUsers", s.RemoveGroupUsers(1, []uint{1}))
	must(t, "AddGroupInstances", s.AddGroupInstances(1, []uint{1}))
	must(t, "RemoveGroupInstances", s.RemoveGroupInstances(1, []uint{1}))
	if !d.has("INSERT IGNORE INTO `dbt_sql_users_resource_group` (`resourcegroup_id`, `users_id`)") || !d.has("INSERT IGNORE INTO `dbt_sql_instance_resource_group` (`resourcegroup_id`, `instance_id`)") {
		t.Errorf("资源组关联表/列名不对: %v", d.logs)
	}
	_, err = s.OpenWorkflowCount(1)
	must(t, "OpenWorkflowCount", err)

	// 审批流
	_, err = s.ListAuditSettings()
	must(t, "ListAuditSettings", err)
	_, err = s.GetAuditSetting(1, 2)
	must(t, "GetAuditSetting", err)
	d.reset()
	must(t, "SaveAuditSetting", s.SaveAuditSetting(1, "组", 2, "10,20"))
	if !d.has("INSERT INTO `dbt_workflow_audit_setting`") {
		t.Errorf("没有现有配置时应插入: %v", d.logs)
	}
	must(t, "DeleteAuditSetting", s.DeleteAuditSetting(1, 2))
	must(t, "RenameAuditSettingGroup", s.RenameAuditSettingGroup(1, "x"))
	_, err = s.ReviewApprovers(1, 1)
	must(t, "ReviewApprovers", err)

	// 其他
	// 没有显式设置时 ConfigValue 返回登记的默认值（5 次），AuthSetting 只返回显式设置的值
	if v, ok := s.ConfigValue(settings.KeySecurityLoginLockAttempts); !ok || v != "5" {
		t.Errorf("空结果应回落到默认值 5: %q %v", v, ok)
	}
	if _, ok := s.AuthSetting(settings.KeySecurityPasswordMinLength); ok {
		t.Error("空结果不应有配置")
	}
	err = s.InTx(func(r Repo) error { return r.UpdateUser(1, map[string]any{"is_active": true}) })
	must(t, "InTx", err)
	if !d.has("BEGIN") || !d.has("COMMIT") {
		t.Errorf("InTx 应开启并提交事务: %v", d.logs)
	}
}

type recResult struct{}

func (recResult) LastInsertId() (int64, error) { return 1, nil }
func (recResult) RowsAffected() (int64, error) { return 1, nil }
