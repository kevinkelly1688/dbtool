// @author: kevinkelly
// @license: Apache Licence
// @file: store_ext_test.go
// @time: 2026/10/28

package sysadmin

import (
	"database/sql"
	"database/sql/driver"
	"strings"
	"sync"
	"testing"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"dbtool/internal/crypto"
)

// missingDriver 对引用了 missing 里任一表名的查询 / 语句返回 MySQL 1146（表不存在），其余返回空结果，并记录语句。
type missingDriver struct {
	mu      sync.Mutex
	missing []string
	logs    []string
}

func (d *missingDriver) Open(string) (driver.Conn, error) { return &missingConn{d}, nil }

func (d *missingDriver) check(q string) error {
	d.mu.Lock()
	d.logs = append(d.logs, strings.Join(strings.Fields(q), " "))
	d.mu.Unlock()
	for _, m := range d.missing {
		if strings.Contains(q, m) {
			return &mysqldrv.MySQLError{Number: 1146, Message: "Table '" + m + "' doesn't exist"}
		}
	}
	return nil
}

func (d *missingDriver) count(sub string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, l := range d.logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

type missingConn struct{ d *missingDriver }

func (c *missingConn) Prepare(q string) (driver.Stmt, error) { return &missingStmt{c.d, q}, nil }
func (c *missingConn) Close() error                          { return nil }
func (c *missingConn) Begin() (driver.Tx, error)             { return recTx{&recDriver{}}, nil }

type missingStmt struct {
	d *missingDriver
	q string
}

func (s *missingStmt) Close() error  { return nil }
func (s *missingStmt) NumInput() int { return -1 }
func (s *missingStmt) Exec([]driver.Value) (driver.Result, error) {
	if err := s.d.check(s.q); err != nil {
		return nil, err
	}
	return recResult{}, nil
}
func (s *missingStmt) Query([]driver.Value) (driver.Rows, error) {
	if err := s.d.check(s.q); err != nil {
		return nil, err
	}
	return &emptyRows{}, nil
}

func newMissingStore(t *testing.T, missing ...string) (*store, *missingDriver) {
	t.Helper()
	d := &missingDriver{missing: missing}
	name := "sysadmin-missing-" + t.Name()
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
	m, err := crypto.NewMirage("hfusaf2m4ot#7)fkw#di2bu6(cv0@opwmafx5n#6=3d%x^hpl6", "ECB", "")
	if err != nil {
		t.Fatal(err)
	}
	return newStore(db, m), d
}

// 旁路表都存在：每个新方法的 SQL 都能生成并执行，关键语句符合预期。
func TestStoreExtStatements(t *testing.T) {
	resetReadyCache()
	s, d := newRecStore(t)
	now := time.Now()

	_, err := s.ResGroupExts(nil)
	must(t, "ResGroupExts", err)
	_, err = s.ResGroupExts([]uint{1, 2})
	must(t, "ResGroupExts ids", err)
	d.reset()
	must(t, "UpsertResGroupExt", s.UpsertResGroupExt(1, ResGroupExt{Leader: "a", Remark: "r"}, "root", now))
	if !d.has("INSERT INTO `dbt_resource_group_ext`") || !d.has("ON DUPLICATE KEY UPDATE leader = VALUES(leader)") {
		t.Errorf("资源组扩展应 upsert: %v", d.logs)
	}
	_, err = s.UserProfiles([]uint{1})
	must(t, "UserProfiles", err)
	if m, err := s.UserProfiles(nil); err != nil || len(m) != 0 {
		t.Errorf("空 ID 不应查询: %v %v", m, err)
	}
	d.reset()
	must(t, "UpsertUserProfile", s.UpsertUserProfile(1, UserProfile{Dept: "d", AuthType: "ldap", MustChangePassword: true}, "root", now))
	must(t, "SetMustChangePassword", s.SetMustChangePassword(1, true, "root", now))
	if !d.has("INSERT INTO `dbt_user_profile` (user_id, dept, auth_type, must_change_password") || !d.has("(user_id, must_change_password, updated_by, updated_at)") {
		t.Errorf("用户扩展应 upsert: %v", d.logs)
	}
	d.reset()
	_, err = s.UserIDsByDept("数据", 0)
	must(t, "UserIDsByDept", err)
	if !d.has("WHERE dept LIKE ?") {
		t.Errorf("%v", d.logs)
	}
	if ids, err := s.UserIDsByDept("", 10); err != nil || ids != nil {
		t.Errorf("空关键字不应查询: %v %v", ids, err)
	}
	_, err = s.AuthGroupDescs(nil)
	must(t, "AuthGroupDescs", err)
	must(t, "UpsertAuthGroupDesc", s.UpsertAuthGroupDesc(1, "x", "root", now))
	must(t, "DeleteAuthGroupExt", s.DeleteAuthGroupExt(1))
	d.reset()
	_, err = s.UserAuthKinds([]uint{1, 2})
	must(t, "UserAuthKinds", err)
	if !d.has("FROM dbt_auth_identity i JOIN dbt_auth_provider p ON p.id = i.provider_id") {
		t.Errorf("应只读 authx 的身份绑定: %v", d.logs)
	}
	d.reset()
	ip, err := s.UserLastLoginIP("alice")
	must(t, "UserLastLoginIP", err)
	if ip != "" || !d.has("FROM dbt_audit_log WHERE user_name = ? AND action = ?") {
		t.Errorf("%q %v", ip, d.logs)
	}
	d.reset()
	_, err = s.OpenWorkflows(1, 20)
	must(t, "OpenWorkflows", err)
	if !d.has("FROM dbt_workflow_audit WHERE group_id = ? AND current_status = 0") || !d.has("FROM dbt_sql_workflow WHERE group_id = ? AND status IN") {
		t.Errorf("进行中的工单应同时查审批流与 SQL 工单: %v", d.logs)
	}
	d.reset()
	_, err = s.ResGroupRefs(1)
	must(t, "ResGroupRefs", err)
	for _, tbl := range []string{"dbt_sql_users_resource_group", "dbt_sql_instance_resource_group", "dbt_sql_workflow", "dbt_workflow_audit", "dbt_query_privileges_apply", "dbt_perf_issue", "dbt_notify_channel", "dbt_workflow_audit_setting"} {
		if !d.has("FROM " + tbl + " WHERE") {
			t.Errorf("引用检查缺少 %s: %v", tbl, d.logs)
		}
	}
	d.reset()
	must(t, "PurgeResourceGroup", s.PurgeResourceGroup(1))
	if !d.has("DELETE FROM dbt_workflow_audit_setting WHERE group_id = ?") || !d.has("DELETE FROM `dbt_resource_group_ext` WHERE group_id = ?") || !d.has("DELETE FROM dbt_resource_group WHERE group_id = ? AND is_deleted = 1") {
		t.Errorf("彻底删除应只删已停用的组并清配置: %v", d.logs)
	}

	// 用户列表：部门命中、超级用户筛选、排序白名单
	d.reset()
	_, _, err = s.ListUsers(UserFilter{Search: "x", DeptUserIDs: []uint{7, 8}, Status: StatusSuper, Sort: SortLastLogin, Limit: 10, Now: now, LockWindow: time.Minute})
	must(t, "ListUsers dept", err)
	if !d.has("dbt_sql_users.id IN") || !d.has("dbt_sql_users.is_superuser = 1") || !d.has("ORDER BY (dbt_sql_users.last_login IS NULL), dbt_sql_users.last_login DESC") {
		t.Errorf("%v", d.logs)
	}
	d.reset()
	_, err = s.ReviewApprovers(1, 1)
	must(t, "ReviewApprovers", err)
	if !d.has("u.is_superuser AS is_super") || !d.has("u.failed_login_count AS failed_count") {
		t.Errorf("审批人查询应带出超级用户与登录失败信息: %v", d.logs)
	}

	// 强制改密标记的进程内读取
	setMustChangeStore(s)
	defer setMustChangeStore(nil)
	d.reset()
	if MustChangePassword("alice") {
		t.Error("没有记录应为 false")
	}
	if !d.has("p.must_change_password FROM `dbt_user_profile` p JOIN dbt_sql_users u ON u.id = p.user_id WHERE u.username = ?") {
		t.Errorf("%v", d.logs)
	}
	d.reset()
	_ = MustChangePassword("alice") // 5 秒内走缓存
	if len(d.logs) != 0 {
		t.Errorf("短时间内应走缓存: %v", d.logs)
	}
	d.reset()
	must(t, "ClearMustChangePassword", ClearMustChangePassword("alice"))
	if !d.has("UPDATE `dbt_user_profile` p JOIN dbt_sql_users u ON u.id = p.user_id SET p.must_change_password = 0") {
		t.Errorf("%v", d.logs)
	}
	if MustChangePassword("") || ClearMustChangePassword("") != nil {
		t.Error("空用户名不应出错")
	}
}

// 旁路表不存在：读取返回空、写入返回 errExtMissing，不报错、不影响页面；探测结果有缓存。
func TestStoreExtTablesMissing(t *testing.T) {
	resetReadyCache()
	defer resetReadyCache()
	s, d := newMissingStore(t, "dbt_resource_group_ext", "dbt_user_profile", "dbt_auth_group_ext", "dbt_auth_identity", "dbt_perf_issue")
	now := time.Now()
	if s.TableReady(tblUserProfile) {
		t.Fatal("表不存在应为未就绪")
	}
	if s.TableReady(tblUserProfile); d.count("FROM `dbt_user_profile` LIMIT 1") != 1 {
		t.Fatalf("探测结果应缓存: %v", d.logs)
	}
	if m, err := s.ResGroupExts(nil); err != nil || len(m) != 0 {
		t.Fatalf("%v %v", m, err)
	}
	if m, err := s.UserProfiles([]uint{1}); err != nil || len(m) != 0 {
		t.Fatalf("%v %v", m, err)
	}
	if m, err := s.AuthGroupDescs(nil); err != nil || len(m) != 0 {
		t.Fatalf("%v %v", m, err)
	}
	if k, err := s.UserAuthKinds([]uint{1}); err != nil || len(k) != 0 {
		t.Fatalf("%v %v", k, err)
	}
	if ids, err := s.UserIDsByDept("x", 5); err != nil || len(ids) != 0 {
		t.Fatalf("%v %v", ids, err)
	}
	for name, err := range map[string]error{
		"resgroup": s.UpsertResGroupExt(1, ResGroupExt{Leader: "a"}, "r", now),
		"profile":  s.UpsertUserProfile(1, UserProfile{Dept: "d"}, "r", now),
		"must":     s.SetMustChangePassword(1, true, "r", now),
		"desc":     s.UpsertAuthGroupDesc(1, "x", "r", now),
	} {
		if err != errExtMissing {
			t.Errorf("%s 缺表应返回 errExtMissing: %v", name, err)
		}
	}
	if err := s.DeleteAuthGroupExt(1); err != nil {
		t.Errorf("缺表时删除应忽略: %v", err)
	}
	// 强制改密标记：缺表一律 false，不影响登录
	setMustChangeStore(s)
	defer setMustChangeStore(nil)
	if MustChangePassword("alice") || ClearMustChangePassword("alice") != nil {
		t.Error("缺表时必须放行")
	}
	// 引用检查：个别表（性能工单）不存在按 0 处理，其余照查
	refs, err := s.ResGroupRefs(1)
	if err != nil || refs.PerfIssues != 0 {
		t.Fatalf("%v %+v", err, refs)
	}
	// 彻底删除在缺少扩展表时仍能删除资源组本身
	if err := s.PurgeResourceGroup(1); err != nil {
		t.Fatalf("%v", err)
	}
	if d.count("DELETE FROM dbt_resource_group WHERE group_id = ? AND is_deleted = 1") == 0 {
		t.Errorf("%v", d.logs)
	}
}

func TestIsNoSuchTable(t *testing.T) {
	if !isNoSuchTable(&mysqldrv.MySQLError{Number: 1146}) || isNoSuchTable(&mysqldrv.MySQLError{Number: 1062}) || isNoSuchTable(nil) || isNoSuchTable(errBoom) {
		t.Fatal("isNoSuchTable")
	}
}
