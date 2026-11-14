// @author: kevinkelly
// @license: Apache Licence
// @file: fake_test.go
// @time: 2026/10/28

package oracle

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

	"dbtool/internal/engine"
)

// 假驱动：不连任何数据库，按脚本（语句里含的子串 → 结果 / 错误）应答，并按顺序记下收到的每条语句，
// 用来核对引擎发出的语句序列（切 schema、核对对象、只读事务、用户语句、回滚）与结果整形。

type fakeRule struct {
	match string // 语句里含有的子串（区分大小写）
	cols  []string
	types []string
	rows  [][]driver.Value
	err   error
	block bool // 一直阻塞到 ctx 结束（模拟卡住的查询）
}

type fakeScript struct {
	mu      sync.Mutex
	stmts   []string
	rules   []fakeRule
	connErr error
	opened  int
}

func (s *fakeScript) add(r fakeRule) *fakeScript { s.rules = append(s.rules, r); return s }

func (s *fakeScript) record(q string) {
	s.mu.Lock()
	s.stmts = append(s.stmts, q)
	s.mu.Unlock()
}

func (s *fakeScript) find(q string) *fakeRule {
	for i := range s.rules {
		if strings.Contains(q, s.rules[i].match) {
			return &s.rules[i]
		}
	}
	return nil
}

// log 返回已记录的语句（拷贝）。
func (s *fakeScript) log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.stmts...)
}

// has 判断是否执行过含该子串的语句。
func (s *fakeScript) has(sub string) bool {
	for _, q := range s.log() {
		if strings.Contains(q, sub) {
			return true
		}
	}
	return false
}

// index 返回第一条含该子串的语句的序号，没有为 -1。
func (s *fakeScript) index(sub string) int {
	for i, q := range s.log() {
		if strings.Contains(q, sub) {
			return i
		}
	}
	return -1
}

type fakeDriver struct{}

var (
	fakeScripts sync.Map // DSN → *fakeScript
	fakeOnce    sync.Once
)

func (fakeDriver) Open(name string) (driver.Conn, error) {
	v, ok := fakeScripts.Load(name)
	if !ok {
		return nil, errors.New("没有这个假数据库脚本：" + name)
	}
	s := v.(*fakeScript)
	if s.connErr != nil {
		return nil, s.connErr
	}
	s.mu.Lock()
	s.opened++
	s.mu.Unlock()
	return &fakeConn{s: s}, nil
}

type fakeConn struct{ s *fakeScript }

func (c *fakeConn) Prepare(q string) (driver.Stmt, error) {
	return nil, errors.New("假驱动不支持 Prepare")
}
func (c *fakeConn) Close() error { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *fakeConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.s.record("BEGIN")
	return fakeTx{c}, nil
}

func (c *fakeConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.s.record(q)
	if r := c.s.find(q); r != nil && r.err != nil {
		return nil, r.err
	}
	return driver.RowsAffected(0), nil
}

func (c *fakeConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.s.record(q)
	r := c.s.find(q)
	if r == nil {
		return nil, fmt.Errorf("假驱动：没有匹配的脚本：%s", q)
	}
	if r.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if r.err != nil {
		return nil, r.err
	}
	return &fakeRows{cols: r.cols, types: r.types, rows: r.rows}, nil
}

type fakeTx struct{ c *fakeConn }

func (t fakeTx) Commit() error   { t.c.s.record("COMMIT"); return nil }
func (t fakeTx) Rollback() error { t.c.s.record("ROLLBACK"); return nil }

type fakeRows struct {
	cols  []string
	types []string
	rows  [][]driver.Value
	i     int
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
func (r *fakeRows) ColumnTypeDatabaseTypeName(i int) string {
	if i < len(r.types) {
		return r.types[i]
	}
	return ""
}

// newFakeEngine 造一个连假驱动的引擎。
func newFakeEngine(t *testing.T, s *fakeScript) *Engine {
	t.Helper()
	fakeOnce.Do(func() { sql.Register("oracle-fake", fakeDriver{}) })
	key := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	fakeScripts.Store(key, s)
	oldOpen, oldName := openDB, driverName
	openDB = func(string) (*sql.DB, error) { return sql.Open("oracle-fake", key) }
	driverName = "oracle-fake"
	t.Cleanup(func() {
		openDB, driverName = oldOpen, oldName
		fakeScripts.Delete(key)
	})
	e, err := New(engine.Conn{Host: "db.example.com", Port: 1521, User: "dbt_ro", Password: "s3cr3t-pw", ServiceName: "ORCLPDB1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// verifyRule 是通过“核对对象”查询的脚本：每个给出的 (owner, name) 都当作存在的表。
func verifyRule(objs ...[2]string) fakeRule {
	r := fakeRule{match: "FROM all_objects", cols: []string{"K", "OWNER", "OBJECT_NAME"}, types: []string{"CHAR", "VARCHAR2", "VARCHAR2"}}
	for _, o := range objs {
		r.rows = append(r.rows, []driver.Value{"T", o[0], o[1]})
	}
	return r
}
