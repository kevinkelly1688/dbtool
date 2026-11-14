// @author: kevinkelly
// @license: Apache Licence
// @file: mysql_test.go
// @time: 2026/10/28

package mysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	drv "github.com/go-sql-driver/mysql"

	"dbtool/internal/engine"
)

// fakeServer 是一个只理解本包要用的几条语句的假 MySQL：按连接记录会话是否只读，
// 并把收到的每条语句连同当时的只读状态记下来，供断言“用户语句确实在只读会话里执行”。
type fakeServer struct {
	mu      sync.Mutex
	log     []seen
	opened  int
	closed  int
	nextID  int64
	readOn  error // 非空：SET ... READ ONLY 返回该错误（模拟不支持的服务端）
	readOff error // 非空：SET ... READ WRITE 返回该错误

	// information_schema.VIEWS 的查询：返回 viewRows（或 viewErr），并记下带的参数。
	viewRows [][]driver.Value
	viewErr  error
	viewArgs []driver.NamedValue
}

type seen struct {
	conn     int64
	sql      string
	readOnly bool
}

func (s *fakeServer) stmts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.log))
	for i, l := range s.log {
		out[i] = l.sql
	}
	return out
}

func (s *fakeServer) find(prefix string) (seen, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.log {
		if strings.HasPrefix(l.sql, prefix) {
			return l, true
		}
	}
	return seen{}, false
}

var (
	serversMu sync.Mutex
	servers   = map[string]*fakeServer{}
	dsnSeq    atomic.Int64
)

type fakeDriver struct{}

func (fakeDriver) Open(name string) (driver.Conn, error) {
	serversMu.Lock()
	s := servers[name]
	serversMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opened++
	s.nextID++
	return &fakeConn{srv: s, id: s.nextID}, nil
}

func init() { sql.Register("fakemysql", fakeDriver{}) }

// fakeDB 返回连着一个新假服务端的 *sql.DB。
func fakeDB(t *testing.T) (*sql.DB, *fakeServer) {
	t.Helper()
	srv := &fakeServer{}
	name := fmt.Sprintf("srv%d", dsnSeq.Add(1))
	serversMu.Lock()
	servers[name] = srv
	serversMu.Unlock()
	db, err := sql.Open("fakemysql", name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, srv
}

func newFake(t *testing.T, name string) (*Engine, *fakeServer) {
	db, srv := fakeDB(t)
	return &Engine{name: name, addr: "fake:" + t.Name(), db: db}, srv
}

type fakeConn struct {
	srv      *fakeServer
	id       int64
	readOnly bool
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (c *fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }
func (c *fakeConn) Close() error {
	c.srv.mu.Lock()
	c.srv.closed++
	c.srv.mu.Unlock()
	return nil
}

// handle 返回要给客户端的错误（nil 表示成功）。
func (c *fakeConn) handle(q string) error {
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	c.srv.log = append(c.srv.log, seen{conn: c.id, sql: q, readOnly: c.readOnly})
	switch {
	case q == sqlReadOnlyOn:
		if c.srv.readOn != nil {
			return c.srv.readOn
		}
		c.readOnly = true
	case q == sqlReadOnlyOff:
		if c.srv.readOff != nil {
			return c.srv.readOff
		}
		c.readOnly = false
	case c.readOnly && (strings.HasPrefix(q, "INSERT") || strings.HasPrefix(q, "UPDATE") || strings.HasPrefix(q, "DELETE")):
		return &drv.MySQLError{Number: 1792, SQLState: [5]byte{'2', '5', '0', '0', '6'},
			Message: "Cannot execute statement in a READ ONLY transaction."}
	}
	return nil
}

func (c *fakeConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	if err := c.handle(q); err != nil {
		return nil, err
	}
	return driver.RowsAffected(0), nil
}

func (c *fakeConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.handle(q); err != nil {
		return nil, err
	}
	if strings.Contains(q, "information_schema.VIEWS") {
		c.srv.mu.Lock()
		defer c.srv.mu.Unlock()
		c.srv.viewArgs = append([]driver.NamedValue(nil), args...)
		if c.srv.viewErr != nil {
			return nil, c.srv.viewErr
		}
		return &fakeRows{cols: []string{"TABLE_SCHEMA", "TABLE_NAME", "VIEW_DEFINITION"}, rows: c.srv.viewRows}, nil
	}
	if strings.HasPrefix(q, "SELECT CONNECTION_ID()") {
		return &fakeRows{cols: []string{"id"}, rows: [][]driver.Value{{c.id}}}, nil
	}
	return &fakeRows{cols: []string{"id"}, rows: [][]driver.Value{{int64(1)}, {int64(2)}}}, nil
}

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

// ---- 测试 ----

// 用户语句必须在只读会话里执行：只读是第一条（先于 USE 和其它设置），复位是最后一条。
func TestQueryRunsInReadOnlySession(t *testing.T) {
	e, srv := newFake(t, "mysql")
	rs, err := e.Query(context.Background(), "SELECT id FROM t", engine.QueryOptions{DBName: "shop", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Rows) != 2 {
		t.Fatalf("rows=%v", rs.Rows)
	}
	got := srv.stmts()
	if len(got) < 3 || got[0] != sqlReadOnlyOn || got[len(got)-1] != sqlReadOnlyOff {
		t.Fatalf("应以只读开头、读写复位结尾: %q", got)
	}
	user, ok := srv.find("SELECT id FROM t")
	if !ok || !user.readOnly {
		t.Fatalf("用户语句执行时会话不是只读: %+v", user)
	}
	if use, ok := srv.find("USE "); !ok || !use.readOnly {
		t.Errorf("USE 也应在只读之后: %+v", use)
	}
	// 复位成功：连接回到连接池复用，没有被丢弃。
	srv.mu.Lock()
	closed := srv.closed
	srv.mu.Unlock()
	if st := e.db.Stats(); st.OpenConnections != 1 || closed != 0 {
		t.Errorf("复位成功时连接应留在池里: open=%d closed=%d", st.OpenConnections, closed)
	}
}

// 写语句被服务端拒绝：错误可用 errors.Is 识别，驱动的原始错误仍可取出，且会话照常复位。
func TestQueryReadOnlyViolation(t *testing.T) {
	e, srv := newFake(t, "mysql")
	_, err := e.Query(context.Background(), "INSERT INTO t VALUES (1)", engine.QueryOptions{})
	if !errors.Is(err, engine.ErrReadOnlyViolation) {
		t.Fatalf("应识别为只读拒绝: %v", err)
	}
	if !IsReadOnlyViolation(err) {
		t.Errorf("原始的 MySQLError 应仍可取出: %v", err)
	}
	got := srv.stmts()
	if got[len(got)-1] != sqlReadOnlyOff {
		t.Errorf("被拒绝之后也要复位: %q", got)
	}
	// 其它引擎错误不应被当成只读拒绝。
	if IsReadOnlyViolation(&drv.MySQLError{Number: 1146, Message: "Table doesn't exist"}) || IsReadOnlyViolation(errors.New("boom")) {
		t.Error("只有 1792 才是只读拒绝")
	}
}

// 服务端拒绝这条 SET（5.6.5 之前、TiDB、Doris 等）：查询照常执行，只是没有会话兜底，也就没有复位。
func TestQueryDegradesWhenServerRefusesReadOnly(t *testing.T) {
	for _, name := range []string{"mysql", "doris"} {
		t.Run(name, func(t *testing.T) {
			e, srv := newFake(t, name)
			srv.readOn = &drv.MySQLError{Number: 1064, Message: "You have an error in your SQL syntax"}
			rs, err := e.Query(context.Background(), "SELECT id FROM t", engine.QueryOptions{})
			if err != nil || len(rs.Rows) != 2 {
				t.Fatalf("应降级后照常查询: rows=%v err=%v", rs, err)
			}
			if user, ok := srv.find("SELECT id FROM t"); !ok || user.readOnly {
				t.Errorf("降级时用户语句不在只读会话里: %+v", user)
			}
			for _, s := range srv.stmts() {
				if s == sqlReadOnlyOff {
					t.Error("没有进入只读就不该复位")
				}
			}
		})
	}
}

// 网络类错误（不是服务端明确拒绝）不能悄悄降级：没有兜底就不执行用户语句。
func TestQueryFailsWhenReadOnlyCannotBeSet(t *testing.T) {
	e, srv := newFake(t, "mysql")
	srv.readOn = errors.New("connection reset by peer")
	if _, err := e.Query(context.Background(), "SELECT id FROM t", engine.QueryOptions{}); err == nil {
		t.Fatal("设置只读失败应返回错误")
	}
	if _, ok := srv.find("SELECT id FROM t"); ok {
		t.Error("没有只读兜底时不应执行用户语句")
	}
}

// 复位失败的连接不能放回池里，否则同一个池里的 ExecEach 会在只读会话里莫名失败。
func TestQueryDiscardsConnectionWhenResetFails(t *testing.T) {
	e, srv := newFake(t, "mysql")
	srv.readOff = errors.New("broken pipe")
	if _, err := e.Query(context.Background(), "SELECT id FROM t", engine.QueryOptions{}); err != nil {
		t.Fatal(err)
	}
	if st := e.db.Stats(); st.OpenConnections != 0 {
		t.Errorf("复位失败的连接应被丢弃: open=%d", st.OpenConnections)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.closed != 1 {
		t.Errorf("应关闭这条连接: closed=%d", srv.closed)
	}
}

// 写入通道不受影响：ExecEach 在没有只读会话的连接上执行。
func TestExecEachIsNotReadOnly(t *testing.T) {
	e, srv := newFake(t, "mysql")
	if _, err := e.Query(context.Background(), "SELECT 1", engine.QueryOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := e.ExecEach(context.Background(), "shop", []string{"INSERT INTO t VALUES (1)"})
	if err != nil || len(res) != 1 || res[0].Err != "" {
		t.Fatalf("ExecEach 不应受只读影响: %+v err=%v", res, err)
	}
	if w, ok := srv.find("INSERT INTO t"); !ok || w.readOnly {
		t.Errorf("变更语句不应在只读会话里: %+v", w)
	}
}

// 前置语句（用户变量赋值等）：与查询在同一条连接、同一个只读会话里按顺序执行；用过的连接不放回池。
func TestPreludeRunsBeforeQueryInSameReadOnlySession(t *testing.T) {
	e, srv := newFake(t, "mysql")
	pre := []string{"SET @a := 1", "SET block_encryption_mode = 'aes-256-cbc'"}
	rs, err := e.Query(context.Background(), "SELECT @a", engine.QueryOptions{DBName: "shop", Prelude: pre})
	if err != nil || len(rs.Rows) != 2 {
		t.Fatalf("rs=%v err=%v", rs, err)
	}
	srv.mu.Lock()
	log := append([]seen(nil), srv.log...)
	closed := srv.closed
	srv.mu.Unlock()
	idx := func(sql string) int {
		for i, l := range log {
			if l.sql == sql {
				return i
			}
		}
		t.Fatalf("没有执行 %q: %+v", sql, log)
		return -1
	}
	ro, use, a, b, q := idx(sqlReadOnlyOn), idx("USE `shop`"), idx(pre[0]), idx(pre[1]), idx("SELECT @a")
	if !(ro < use && use < a && a < b && b < q) {
		t.Errorf("顺序应为 只读 → USE → 前置语句 → 查询: %d %d %d %d %d", ro, use, a, b, q)
	}
	for _, i := range []int{a, b, q} {
		if !log[i].readOnly || log[i].conn != log[q].conn {
			t.Errorf("前置语句必须与查询在同一连接、只读会话里: %+v", log[i])
		}
	}
	if st := e.db.Stats(); st.OpenConnections != 0 || closed != 1 {
		t.Errorf("带过会话变量的连接应被丢弃: open=%d closed=%d", st.OpenConnections, closed)
	}
}

// 前置语句失败：不执行查询；会修改数据的前置语句同样被只读会话挡下（引擎不依赖上层的检查）。
func TestPreludeFailureStopsQuery(t *testing.T) {
	e, srv := newFake(t, "mysql")
	_, err := e.Query(context.Background(), "SELECT 1", engine.QueryOptions{Prelude: []string{"INSERT INTO t VALUES (1)"}})
	if !errors.Is(err, engine.ErrReadOnlyViolation) {
		t.Fatalf("前置语句里的写操作应被只读会话拒绝: %v", err)
	}
	if _, ok := srv.find("SELECT 1"); ok {
		t.Error("前置语句失败后不应执行查询")
	}
}

// PolarDB-X 走 MySQL 协议，复用基础实现（在线查询、工单、导出都要能按类型取到引擎）。
func TestPolarDBXRegistered(t *testing.T) {
	have := map[string]bool{}
	for _, n := range engine.Supported() {
		have[n] = true
	}
	for _, tp := range []string{"polardb", "polardb-x", "polardbx"} {
		if !have[tp] {
			t.Errorf("引擎 %s 未注册", tp)
		}
	}
}
