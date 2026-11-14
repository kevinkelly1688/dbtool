// @author: kevinkelly
// @license: Apache Licence
// @file: store_test.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"dbtool/internal/crypto"
	"dbtool/internal/model"
)

// 没有真实元数据库可用，这里用一个记录语句的假 database/sql 驱动，检查 GORM 实现发出的 SQL、参数、事务边界，
// 以及密文字段是否真的加了密。语句在 MySQL 上能否执行要靠真库验证。

type recStmt struct {
	q    string
	args []driver.Value
}

type recDB struct {
	mu   sync.Mutex
	log  []recStmt
	rows func(q string, args []driver.Value) (cols []string, rows [][]driver.Value, err error)
	// lastID 是 INSERT 返回的自增 ID
	lastID int64
}

func (d *recDB) record(q string, args []driver.Value) {
	d.mu.Lock()
	d.log = append(d.log, recStmt{q, args})
	d.mu.Unlock()
}

// stmts 返回记录下来的语句（含 BEGIN / COMMIT / ROLLBACK）。
func (d *recDB) stmts() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.log))
	for i, s := range d.log {
		out[i] = s.q
	}
	return out
}

func (d *recDB) find(sub string) []recStmt {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []recStmt
	for _, s := range d.log {
		if strings.Contains(s.q, sub) {
			out = append(out, s)
		}
	}
	return out
}

type recConnector struct{ d *recDB }

func (c recConnector) Connect(context.Context) (driver.Conn, error) { return &recConn{c.d}, nil }
func (c recConnector) Driver() driver.Driver                        { return nil }

type recConn struct{ d *recDB }

func (c *recConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (c *recConn) Close() error                        { return nil }
func (c *recConn) Begin() (driver.Tx, error)           { c.d.record("BEGIN", nil); return &recTx{c.d}, nil }
func (c *recConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.d.record("BEGIN", nil)
	return &recTx{c.d}, nil
}

func values(nv []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(nv))
	for i, v := range nv {
		out[i] = v.Value
	}
	return out
}

func (c *recConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	vals := values(args)
	c.d.record(q, vals)
	if c.d.rows == nil {
		return &recRows{}, nil
	}
	cols, rows, err := c.d.rows(q, vals)
	if err != nil {
		return nil, err
	}
	return &recRows{cols: cols, rows: rows}, nil
}

func (c *recConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	vals := values(args)
	c.d.record(q, vals)
	if c.d.rows != nil {
		if _, _, err := c.d.rows(q, vals); err != nil {
			return nil, err
		}
	}
	return recResult{c.d.lastID}, nil
}

type recTx struct{ d *recDB }

func (t *recTx) Commit() error   { t.d.record("COMMIT", nil); return nil }
func (t *recTx) Rollback() error { t.d.record("ROLLBACK", nil); return nil }

type recResult struct{ id int64 }

func (r recResult) LastInsertId() (int64, error) { return r.id, nil }
func (r recResult) RowsAffected() (int64, error) { return 1, nil }

type recRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *recRows) Columns() []string { return r.cols }
func (r *recRows) Close() error      { return nil }
func (r *recRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

func newRecStore(t *testing.T) (*store, *recDB, *crypto.Mirage) {
	t.Helper()
	d := &recDB{lastID: 7}
	sqlDB := sql.OpenDB(recConnector{d})
	t.Cleanup(func() { sqlDB.Close() })
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}),
		&gorm.Config{Logger: logger.Discard, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatal(err)
	}
	m, err := crypto.NewMirage(strings.Repeat("k", 32), "ECB", "")
	if err != nil {
		t.Fatal(err)
	}
	return newStore(db, m), d, m
}

func TestStoreListInstancesBuildsFilters(t *testing.T) {
	st, d, _ := newRecStore(t)
	d.rows = func(q string, _ []driver.Value) ([]string, [][]driver.Value, error) {
		if strings.HasPrefix(q, "SELECT count(*)") {
			return []string{"count(*)"}, [][]driver.Value{{int64(42)}}, nil
		}
		return nil, nil, nil
	}
	yes := true
	_, total, err := st.ListInstances(InstanceFilter{Role: "master", DBType: "mysql", Tag: "can_write", Enabled: &yes, Q: "a_b%", Offset: 20, Limit: 10},
		map[uint]bool{3: true})
	if err != nil || total != 42 {
		t.Fatalf("%d %v", total, err)
	}
	sel := d.find("FROM `dbt_sql_instance`")
	if len(sel) < 2 {
		t.Fatalf("应有计数与分页两条查询: %v", d.stmts())
	}
	last := sel[len(sel)-1]
	for _, want := range []string{"dbt_sql_instance.id IN (?)", "dbt_sql_instance.`type` = ?", "dbt_sql_instance.db_type = ?", "dbt_sql_instance_instance_tag", "t.active = 1",
		"dbt_sql_instance.is_enable = ?", "LIKE ? ESCAPE '!'", "ORDER BY dbt_sql_instance.id", "LIMIT ? OFFSET ?"} {
		if !strings.Contains(last.q, want) {
			t.Errorf("缺少 %q:\n%s", want, last.q)
		}
	}
	joined := ""
	for _, a := range last.args {
		joined += "|" + anyString(a)
	}
	for _, want := range []string{"|3|", "|master|", "|mysql|", "|can_write|", "|%a!_b!%%|"} {
		if !strings.Contains(joined+"|", want) {
			t.Errorf("参数里缺少 %s: %s", want, joined)
		}
	}
	// 没有可见实例时不查库
	n := len(d.stmts())
	list, total, err := st.ListInstances(InstanceFilter{Limit: 10}, map[uint]bool{})
	if err != nil || list != nil || total != 0 || len(d.stmts()) != n {
		t.Fatalf("空范围应直接返回: %v %d %v", list, total, err)
	}
}

func anyString(v driver.Value) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case int64:
		return itoa(uint(x))
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return "?"
}

func TestStoreSaveInstanceCreateEncryptsAndReplacesRelations(t *testing.T) {
	st, d, m := newRecStore(t)
	pw := "S3cret!pw"
	id, err := st.SaveInstance(0, InstanceWrite{Name: "prod-mysql-03", Role: "master", DBType: "mysql", Host: "10.0.0.3", Port: 3306, User: "dba_ro",
		Password: &pw, VerifySSL: true, GroupIDs: []uint{1, 2}, TagIDs: []uint{2}})
	if err != nil || id != 7 {
		t.Fatalf("%d %v", id, err)
	}
	got := d.stmts()
	if got[0] != "BEGIN" || got[len(got)-1] != "COMMIT" {
		t.Fatalf("应在一个事务里完成: %v", got)
	}
	ins := d.find("INSERT INTO `dbt_sql_instance` ")
	if len(ins) != 1 {
		t.Fatalf("%v", got)
	}
	var seen []string
	for _, a := range ins[0].args {
		seen = append(seen, anyString(a))
	}
	all := strings.Join(seen, "|")
	if strings.Contains(all, "S3cret!pw") || strings.Contains(all, "|dba_ro|") {
		t.Fatalf("用户名与密码必须加密后入库: %s", all)
	}
	if !strings.Contains(all, m.Encrypt("S3cret!pw")) || !strings.Contains(all, m.Encrypt("dba_ro")) {
		t.Fatalf("应写入 mirage 密文: %s", all)
	}
	if len(d.find("DELETE FROM dbt_sql_instance_resource_group WHERE instance_id = ?")) != 1 || len(d.find("INSERT INTO dbt_sql_instance_resource_group (instance_id, resourcegroup_id)")) != 2 ||
		len(d.find("DELETE FROM dbt_sql_instance_instance_tag WHERE instance_id = ?")) != 1 || len(d.find("INSERT INTO dbt_sql_instance_instance_tag (instance_id, instancetag_id)")) != 1 {
		t.Fatalf("资源组与标签关联应整体替换: %v", got)
	}
	if rel := d.find("INSERT INTO dbt_sql_instance_resource_group"); anyString(rel[0].args[0]) != "7" {
		t.Fatalf("关联应使用新建实例的 ID: %v", rel[0].args)
	}
}

func TestStoreSaveInstanceUpdate(t *testing.T) {
	st, d, m := newRecStore(t)
	tid := uint(1)
	if _, err := st.SaveInstance(5, InstanceWrite{Name: "x", Role: "slave", DBType: "mysql", Host: "h", Port: 3306, User: "u", TunnelID: &tid}); err != nil {
		t.Fatal(err)
	}
	upd := d.find("UPDATE `dbt_sql_instance` SET")
	if len(upd) != 1 || strings.Contains(upd[0].q, "`password`") {
		t.Fatalf("不带密码时不能更新 password 列: %v", upd)
	}
	for _, col := range []string{"`instance_name`", "`user`", "`tunnel_id`", "`update_time`", "`service_name`", "`sid`", "`verify_ssl`"} {
		if !strings.Contains(upd[0].q, col) {
			t.Errorf("缺少列 %s: %s", col, upd[0].q)
		}
	}
	if !strings.Contains(upd[0].q, "WHERE id = ?") {
		t.Fatalf("%s", upd[0].q)
	}
	pw := "n3w"
	d.log = nil
	if _, err := st.SaveInstance(5, InstanceWrite{Name: "x", Role: "slave", DBType: "mysql", Host: "h", Port: 3306, User: "u", Password: &pw}); err != nil {
		t.Fatal(err)
	}
	upd = d.find("UPDATE `dbt_sql_instance` SET")
	if len(upd) != 1 || !strings.Contains(upd[0].q, "`password`") {
		t.Fatalf("带密码时更新 password: %v", upd)
	}
	found := false
	for _, a := range upd[0].args {
		found = found || anyString(a) == m.Encrypt("n3w")
	}
	if !found {
		t.Fatal("密码应加密")
	}
}

func TestStoreInstanceRefsAndDelete(t *testing.T) {
	st, d, _ := newRecStore(t)
	d.rows = func(q string, args []driver.Value) ([]string, [][]driver.Value, error) {
		switch {
		case strings.Contains(q, "FROM dbt_sql_workflow WHERE instance_id"):
			return []string{"c"}, [][]driver.Value{{int64(3)}}, nil
		case strings.Contains(q, "FROM dbt_slow_logs"):
			return nil, nil, errors.New("Error 1146 (42S02): Table 'archery.slow_logs' doesn't exist")
		case strings.Contains(q, "FROM dbt_instance_account"):
			return []string{"c"}, [][]driver.Value{{int64(10001)}}, nil
		}
		return []string{"c"}, [][]driver.Value{{int64(0)}}, nil
	}
	refs, err := st.InstanceRefs(9)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Ref{}
	for _, r := range refs {
		by[r.Label] = r
	}
	if len(refs) != 2 || by["SQL 工单"].Count != 3 || !by["SQL 工单"].Blocking || by["账号登记"].Blocking || !by["账号登记"].Capped || by["账号登记"].Count != refCap {
		t.Fatalf("%+v", refs)
	}
	if q := d.find("FIND_IN_SET"); len(q) != 1 || anyString(q[0].args[0]) != "9" {
		t.Fatalf("%+v", q)
	}
	// 其他错误不能吞掉
	d.rows = func(q string, _ []driver.Value) ([]string, [][]driver.Value, error) {
		return nil, nil, errors.New("connection reset")
	}
	if _, err := st.InstanceRefs(9); err == nil {
		t.Fatal("非“表不存在”的错误必须返回")
	}

	d.rows, d.log = nil, nil
	if err := st.DeleteInstance(9); err != nil {
		t.Fatal(err)
	}
	got := d.stmts()
	if got[0] != "BEGIN" || got[len(got)-1] != "COMMIT" {
		t.Fatalf("%v", got)
	}
	last := got[len(got)-2]
	if !strings.HasPrefix(last, "DELETE FROM `dbt_sql_instance` WHERE") {
		t.Fatalf("最后删实例本身: %v", got)
	}
	for _, tbl := range []string{"dbt_instance_account", "dbt_instance_database", "dbt_param_history", "dbt_sql_instance_resource_group", "dbt_sql_instance_instance_tag"} {
		if len(d.find("DELETE FROM "+tbl+" WHERE instance_id = ?")) != 1 {
			t.Errorf("应先清理 %s: %v", tbl, got)
		}
	}
}

func TestStoreAccountRecordsEncryptUserAndPassword(t *testing.T) {
	st, d, m := newRecStore(t)
	pw := "Passw0rd!x"
	if err := st.SaveAccountRecord(3, AccountRecord{User: "app_rw", Host: "10.%", Remark: "r", Password: &pw}); err != nil {
		t.Fatal(err)
	}
	ins := d.find("INSERT INTO `dbt_instance_account`")
	if len(ins) != 1 {
		t.Fatalf("%v", d.stmts())
	}
	var seen []string
	for _, a := range ins[0].args {
		seen = append(seen, anyString(a))
	}
	all := strings.Join(seen, "|")
	if strings.Contains(all, "Passw0rd") || strings.Contains(all, "|app_rw|") || !strings.Contains(all, m.Encrypt("app_rw")) || !strings.Contains(all, m.Encrypt("Passw0rd!x")) {
		t.Fatalf("账号与密码必须是密文: %s", all)
	}
	if sel := d.find("FROM `dbt_instance_account`"); len(sel) != 1 || !strings.Contains(sel[0].q, "db_name = ''") {
		t.Fatalf("只看 MySQL 系的登记（db_name 为空）: %+v", sel)
	}

	// 已有登记：按解密后的账号匹配，更新而不是新建
	d.log = nil
	d.rows = func(q string, _ []driver.Value) ([]string, [][]driver.Value, error) {
		if strings.Contains(q, "FROM `dbt_instance_account`") {
			return []string{"id", "instance_id", "user", "host", "db_name", "password", "remark"},
				[][]driver.Value{{int64(11), int64(3), m.Encrypt("app_rw"), "10.%", "", m.Encrypt("old"), "旧备注"}}, nil
		}
		return nil, nil, nil
	}
	if err := st.SaveAccountRecord(3, AccountRecord{User: "app_rw", Host: "10.%", Password: &pw, KeepRemark: true}); err != nil {
		t.Fatal(err)
	}
	upd := d.find("UPDATE `dbt_instance_account` SET")
	if len(upd) != 1 || strings.Contains(upd[0].q, "`remark`") || !strings.Contains(upd[0].q, "`password`") || len(d.find("INSERT INTO `dbt_instance_account`")) != 0 {
		t.Fatalf("改密流程不动备注: %v", d.stmts())
	}
	list, err := st.SavedAccounts(3)
	if err != nil || len(list) != 1 || list[0].User != "app_rw" || list[0].Remark != "旧备注" || list[0].Password != nil {
		t.Fatalf("读出来的账号是明文、密码不返回: %+v %v", list, err)
	}
	d.log = nil
	if err := st.DeleteAccountRecord(3, "app_rw", "10.%"); err != nil || len(d.find("DELETE FROM `dbt_instance_account`")) != 1 {
		t.Fatalf("%v %v", err, d.stmts())
	}
	d.log = nil
	if err := st.DeleteAccountRecord(3, "nobody", "10.%"); err != nil || len(d.find("DELETE FROM")) != 0 {
		t.Fatalf("没有这条登记时什么都不删: %v %v", err, d.stmts())
	}
}

func TestStoreOtherQueries(t *testing.T) {
	st, d, _ := newRecStore(t)
	d.rows = func(q string, _ []driver.Value) ([]string, [][]driver.Value, error) {
		if strings.HasPrefix(q, "SELECT count(*)") {
			return []string{"c"}, [][]driver.Value{{int64(1)}}, nil
		}
		return nil, nil, nil
	}
	if u, err := st.ActiveUser("dev"); err != nil || u != nil {
		t.Fatalf("%v %v", u, err)
	}
	if q := d.find("FROM `dbt_sql_users`"); len(q) != 1 || !strings.Contains(q[0].q, "is_active = 1") {
		t.Fatalf("%+v", q)
	}
	if t0, err := st.GetTemplate("mysql", "Max_Connections"); err != nil || t0 != nil {
		t.Fatalf("%v %v", t0, err)
	}
	if q := d.find("FROM `dbt_param_template`"); len(q) != 1 || !strings.Contains(q[0].q, "LOWER(variable_name) = ?") || anyString(q[0].args[1]) != "max_connections" {
		t.Fatalf("参数名不区分大小写匹配: %+v", q)
	}
	if _, _, err := st.ListParamHistory(2, "max_", 20, 10); err != nil {
		t.Fatal(err)
	}
	h := d.find("dbt_param_history ph")
	if len(h) < 2 {
		t.Fatalf("%v", d.stmts())
	}
	sel := h[len(h)-1]
	for _, want := range []string{"JOIN dbt_sql_instance i ON i.id = ph.instance_id", "ph.instance_id = ?", "LIKE ? ESCAPE '!'", "ORDER BY ph.id DESC", "i.instance_name AS instance_name"} {
		if !strings.Contains(sel.q, want) {
			t.Errorf("缺少 %q: %s", want, sel.q)
		}
	}
	if err := st.InsertParamHistory(&ParamHistory{InstanceID: 2, VariableName: "x", CreateTime: time.Now()}); err != nil || len(d.find("INSERT INTO `dbt_param_history`")) != 1 {
		t.Fatalf("%v %v", err, d.stmts())
	}
	if err := st.InsertAudit(&model.AuditEntry{UserID: 1, UserName: "root", Action: "a", ExtraInfo: "b"}); err != nil || len(d.find("INSERT INTO `dbt_audit_log`")) != 1 {
		t.Fatalf("%v %v", err, d.stmts())
	}
	tpl := &ParamTemplate{DBType: "mysql", VariableName: "x", Editable: false, CreateTime: time.Now(), SysTime: time.Now()}
	if err := st.SaveTemplate(tpl); err != nil || len(d.find("INSERT INTO `dbt_param_template`")) != 1 {
		t.Fatalf("%v %v", err, d.stmts())
	}
	tpl.ID = 4
	if err := st.SaveTemplate(tpl); err != nil {
		t.Fatal(err)
	}
	if u := d.find("UPDATE `dbt_param_template` SET"); len(u) != 1 || !strings.Contains(u[0].q, "`editable`") {
		t.Fatalf("editable=false 也要写入（map 更新不会丢零值）: %v", u)
	}
}
