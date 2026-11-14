// @author: kevinkelly
// @license: Apache Licence
// @file: mysqlstore_test.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// 一个最小的 database/sql 假驱动：记录收到的语句与参数，按规则返回预置结果。
// 它不理解 SQL，只用来核对 mysqlStore 发出了什么语句、用了哪些参数、事务是否提交 / 回滚。

type fakeResult struct {
	cols   []string
	rows   [][]driver.Value
	err    error
	lastID int64
}

type fakeQuery struct {
	sql  string
	args []driver.Value
}

type fakeRule func(q string, args []driver.Value) *fakeResult

type fakeSQL struct {
	mu       sync.Mutex
	log      []fakeQuery
	rules    []fakeRule
	begins   int
	commits  int
	rollback int
}

func (f *fakeSQL) Connect(context.Context) (driver.Conn, error) { return &fakeConn{f: f}, nil }
func (f *fakeSQL) Driver() driver.Driver                        { return nil }

func (f *fakeSQL) result(q string, args []driver.Value) *fakeResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, fakeQuery{q, args})
	for _, r := range f.rules {
		if res := r(q, args); res != nil {
			return res
		}
	}
	return &fakeResult{}
}

// queries 返回包含 sub 的已记录语句。
func (f *fakeSQL) queries(sub string) []fakeQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeQuery
	for _, q := range f.log {
		if strings.Contains(q.sql, sub) {
			out = append(out, q)
		}
	}
	return out
}

type fakeConn struct{ f *fakeSQL }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.f.mu.Lock()
	c.f.begins++
	c.f.mu.Unlock()
	return &fakeTx{f: c.f}, nil
}

type fakeTx struct{ f *fakeSQL }

func (t *fakeTx) Commit() error   { t.f.mu.Lock(); t.f.commits++; t.f.mu.Unlock(); return nil }
func (t *fakeTx) Rollback() error { t.f.mu.Lock(); t.f.rollback++; t.f.mu.Unlock(); return nil }

func named(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out
}

func (c *fakeConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	r := c.f.result(q, named(args))
	if r.err != nil {
		return nil, r.err
	}
	return &fakeRows{cols: r.cols, rows: r.rows}, nil
}

func (c *fakeConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	r := c.f.result(q, named(args))
	if r.err != nil {
		return nil, r.err
	}
	return fakeExecResult{id: r.lastID}, nil
}

type fakeExecResult struct{ id int64 }

func (r fakeExecResult) LastInsertId() (int64, error) { return r.id, nil }
func (r fakeExecResult) RowsAffected() (int64, error) { return 1, nil }

type fakeRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

func newFakeStore(t *testing.T, rules ...fakeRule) (*mysqlStore, *fakeSQL) {
	t.Helper()
	f := &fakeSQL{rules: rules}
	db := sql.OpenDB(f)
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); db.Close() })
	return &mysqlStore{db: db, conn: conn}, f
}

// tablesRule：information_schema.tables 里存在 names 中的表。
func tablesRule(names ...string) fakeRule {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(q string, args []driver.Value) *fakeResult {
		if strings.Contains(q, "information_schema.tables") && strings.Contains(q, "table_name = ?") {
			n := int64(0)
			if s, _ := args[0].(string); set[s] {
				n = 1
			}
			return &fakeResult{cols: []string{"c"}, rows: [][]driver.Value{{n}}}
		}
		return nil
	}
}

func columnsRule(cols ...string) fakeRule {
	return func(q string, args []driver.Value) *fakeResult {
		if strings.Contains(q, "information_schema.columns") {
			r := &fakeResult{cols: []string{"column_name"}}
			for _, c := range cols {
				r.rows = append(r.rows, []driver.Value{c})
			}
			return r
		}
		return nil
	}
}

func TestMySQLStoreInsertUserAdaptsToColumns(t *testing.T) {
	base := []string{"id", "password", "last_login", "is_superuser", "username", "first_name", "last_name", "email", "is_staff", "is_active", "date_joined", "display"}
	for _, tc := range []struct {
		name      string
		extra     []string
		wantCols  []string
		wantAbsnt []string
	}{
		{"较早的表结构（没有 slack_id / failed_login_count）", []string{"ding_user_id", "feishu_open_id"}, []string{"ding_user_id", "feishu_open_id"}, []string{"slack_id", "failed_login_count"}},
		{"完整列", []string{"ding_user_id", "feishu_open_id", "slack_id", "failed_login_count", "last_login_failed_at"}, []string{"ding_user_id", "feishu_open_id", "slack_id", "failed_login_count"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, f := newFakeStore(t, columnsRule(append(append([]string{}, base...), tc.extra...)...), func(q string, a []driver.Value) *fakeResult {
				if strings.HasPrefix(q, "INSERT INTO dbt_sql_users") {
					return &fakeResult{lastID: 42}
				}
				return nil
			})
			hash := "pbkdf2_sha256$1000$saltsaltsaltsaltsaltsa$AAAA"
			err := st.InTx(context.Background(), func(tx Tx) error {
				id, err := tx.InsertUser(NewUser{Username: "ops", Display: "运维", Email: "o@x.com", PasswordHash: hash, IsSuperuser: true, IsStaff: true, IsActive: true, DateJoined: time.Unix(0, 0)})
				if err != nil {
					return err
				}
				if id != 42 {
					return fmt.Errorf("id=%d", id)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			qs := f.queries("INSERT INTO dbt_sql_users")
			if len(qs) != 1 {
				t.Fatalf("INSERT 语句 %d 条", len(qs))
			}
			q := qs[0]
			for _, c := range append([]string{"password", "is_superuser", "username", "first_name", "last_name", "email", "is_staff", "is_active", "date_joined", "display"}, tc.wantCols...) {
				if !strings.Contains(q.sql, c) {
					t.Errorf("INSERT 应包含列 %s: %s", c, q.sql)
				}
			}
			for _, c := range tc.wantAbsnt {
				if strings.Contains(q.sql, c) {
					t.Errorf("表里没有的列 %s 不应出现: %s", c, q.sql)
				}
			}
			if strings.Contains(q.sql, hash) {
				t.Error("哈希必须走参数，不能拼进 SQL 文本")
			}
			if len(q.args) != strings.Count(q.sql, "?") {
				t.Errorf("参数个数 %d 与占位符 %d 不一致", len(q.args), strings.Count(q.sql, "?"))
			}
			foundHash := false
			for _, a := range q.args {
				if a == hash {
					foundHash = true
				}
			}
			if !foundHash {
				t.Error("哈希应作为参数传入")
			}
		})
	}
}

func TestMySQLStoreAuditWritesLogAndExt(t *testing.T) {
	st, f := newFakeStore(t, tablesRule(tblAuditLog, tblAuditExt), func(q string, a []driver.Value) *fakeResult {
		if strings.HasPrefix(q, "INSERT INTO dbt_audit_log") {
			return &fakeResult{lastID: 7}
		}
		return nil
	})
	at := time.Date(2026, 10, 10, 1, 2, 3, 0, time.UTC)
	err := st.InTx(context.Background(), func(tx Tx) error {
		return tx.InsertAudit(AuditEntry{UserName: "dbt-admin(" + strings.Repeat("x", 40) + ")", UserDisplay: "d", Action: "CLI-创建超级用户", ExtraInfo: "目标用户 ops", At: at})
	})
	if err != nil {
		t.Fatal(err)
	}
	qs := f.queries("INSERT INTO dbt_audit_log")
	if len(qs) != 1 {
		t.Fatalf("dbt_audit_log 插入 %d 条", len(qs))
	}
	args := qs[0].args
	if args[0] != int64(0) && args[0] != 0 {
		t.Errorf("user_id 应为 0: %v", args[0])
	}
	if n := len([]rune(args[1].(string))); n != 30 {
		t.Errorf("user_name 应截断到 30 个字符: %d", n)
	}
	if args[3] != "CLI-创建超级用户" {
		t.Errorf("action: %v", args[3])
	}
	if ext := f.queries("INSERT INTO " + tblAuditExt); len(ext) != 1 || ext[0].args[0] != int64(7) {
		t.Errorf("应补写 dbt_audit_ext（audit_id=7）: %+v", ext)
	}
	if f.commits != 1 || f.rollback != 0 {
		t.Errorf("事务应提交: commits=%d rollback=%d", f.commits, f.rollback)
	}
}

func TestMySQLStoreAuditTableMissing(t *testing.T) {
	st, f := newFakeStore(t, tablesRule())
	err := st.InTx(context.Background(), func(tx Tx) error {
		return tx.InsertAudit(AuditEntry{Action: "x"})
	})
	if !errors.Is(err, ErrTableMissing) {
		t.Fatalf("应返回 ErrTableMissing: %v", err)
	}
	if len(f.queries("INSERT")) != 0 {
		t.Error("表不存在时不应有 INSERT")
	}
	if f.rollback != 1 || f.commits != 0 {
		t.Errorf("fn 返回错误应回滚: commits=%d rollback=%d", f.commits, f.rollback)
	}
}

func TestMySQLStoreRejectsBadTableNames(t *testing.T) {
	st, f := newFakeStore(t)
	for _, n := range []string{"a;DROP TABLE x", "a`b", "a b", ""} {
		if _, err := st.CountRows(context.Background(), n); err == nil {
			t.Errorf("表名 %q 应被拒绝", n)
		}
	}
	if len(f.log) != 0 {
		t.Error("非法表名不应发出任何语句")
	}
}

func TestMySQLStoreSetMustChangeNeedsTable(t *testing.T) {
	st, f := newFakeStore(t, tablesRule())
	err := st.InTx(context.Background(), func(tx Tx) error {
		return tx.SetMustChange(1, true, "dbt-admin(a)", time.Now())
	})
	if !errors.Is(err, ErrTableMissing) {
		t.Errorf("应返回 ErrTableMissing: %v", err)
	}
	if len(f.queries("INSERT")) != 0 {
		t.Error("不应写入")
	}
}

func TestMySQLStoreListUsersHandlesNulls(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	st, _ := newFakeStore(t, func(q string, a []driver.Value) *fakeResult {
		if strings.HasPrefix(q, "SELECT id, username") {
			return &fakeResult{
				cols: []string{"id", "username", "display", "email", "is_superuser", "is_staff", "is_active", "last_login", "failed_login_count", "last_login_failed_at"},
				rows: [][]driver.Value{
					{int64(1), "root", "管理员", "r@x.com", int64(1), int64(1), int64(1), now, int64(0), nil},
					{int64(2), "bob", nil, nil, int64(0), int64(0), int64(0), nil, nil, now},
				},
			}
		}
		return nil
	})
	us, err := st.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(us) != 2 || !us[0].IsSuperuser || us[0].LastLogin == nil || us[1].LastLogin != nil || us[1].LastFailedAt == nil || us[1].IsActive {
		t.Errorf("解析结果不对: %+v", us)
	}
}

func TestMySQLStoreRecordApplied(t *testing.T) {
	st, f := newFakeStore(t)
	at := time.Now()
	if err := st.RecordApplied(context.Background(), "bootstrap/001.sql", strings.Repeat("a", 64), "v1", at); err != nil {
		t.Fatal(err)
	}
	qs := f.queries("INSERT INTO " + TblApplied)
	if len(qs) != 1 || !strings.Contains(qs[0].sql, "ON DUPLICATE KEY UPDATE") || len(qs[0].args) != 4 {
		t.Errorf("语句不对: %+v", qs)
	}
}

func TestMySQLStoreExecStmtPassesNoArgs(t *testing.T) {
	st, f := newFakeStore(t)
	// 含 ? 的语句（比如注释或字符串里）不能被当作带参数的语句处理
	if err := st.ExecStmt(context.Background(), "INSERT INTO t VALUES ('what?')"); err != nil {
		t.Fatal(err)
	}
	if qs := f.queries("what?"); len(qs) != 1 || len(qs[0].args) != 0 {
		t.Errorf("ExecStmt 不应带参数: %+v", qs)
	}
}
