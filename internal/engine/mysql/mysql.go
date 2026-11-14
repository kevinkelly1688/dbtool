// @author: kevinkelly
// @license: Apache Licence
// @file: mysql.go
// @time: 2026/10/28

// Package mysql 是 MySQL 引擎的 Go 实现。
//
// Query 是只读通道：语句在服务端的只读会话里执行（见 enterReadOnly），不依赖上层的语句检查是否完整；
// 变更语句走 ExecEach。
package mysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	drv "github.com/go-sql-driver/mysql"

	"dbtool/internal/engine"
)

func init() {
	engine.Register("mysql", func(c engine.Conn) (engine.Engine, error) { return New(c) })
	// PolarDB / PolarDB-X / SelectDB(Doris) / ADB 均走 MySQL 协议，先复用基础实现。
	for _, t := range []string{"polardb", "polardb-x", "polardbx", "selectdb", "doris", "adb"} {
		t := t
		engine.Register(t, func(c engine.Conn) (engine.Engine, error) {
			e, err := New(c)
			if err != nil {
				return nil, err
			}
			e.name = t
			return e, nil
		})
	}
}

// 系统库默认不展示。
var systemDBs = map[string]bool{
	"information_schema": true, "performance_schema": true, "mysql": true, "test": true, "sys": true,
}

type Engine struct {
	name string
	addr string // host:port，只用于日志
	db   *sql.DB
}

func New(c engine.Conn) (*Engine, error) {
	cfg := drv.NewConfig()
	cfg.User = c.User
	cfg.Passwd = c.Password
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%d", c.Host, c.Port)
	cfg.DBName = c.DBName
	cfg.ParseTime = true
	cfg.Timeout = 10 * time.Second
	charset := c.Charset
	if charset == "" {
		charset = "utf8mb4"
	}
	cfg.Params = map[string]string{"charset": charset}
	if c.SSL {
		if c.VerifySSL {
			cfg.TLSConfig = "true"
		} else {
			cfg.TLSConfig = "skip-verify"
		}
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &Engine{name: "mysql", addr: cfg.Addr, db: db}, nil
}

// 只读会话。用会话级的 SET 而不是 START TRANSACTION READ ONLY：后者遇到隐式提交的语句就结束了，
// 前者对之后的每个事务（含 autocommit 下的单条语句）都生效。Connector/J 的 setReadOnly(true) 也是这条语句。
const (
	sqlReadOnlyOn  = "SET SESSION TRANSACTION READ ONLY"
	sqlReadOnlyOff = "SET SESSION TRANSACTION READ WRITE"
)

// errCantExecuteInReadOnlyTx 是 ER_CANT_EXECUTE_IN_READ_ONLY_TRANSACTION（SQLSTATE 25006）。
const errCantExecuteInReadOnlyTx = 1792

// IsReadOnlyViolation 判断 err 是否服务端因只读会话拒绝了会修改数据的语句。
func IsReadOnlyViolation(err error) bool {
	var me *drv.MySQLError
	return errors.As(err, &me) && me.Number == errCantExecuteInReadOnlyTx
}

// enterReadOnly 把 conn 所在会话切到只读。返回 true 表示已切换，调用方必须在放回连接池前 leaveReadOnly。
//
// 服务端明确拒绝这条语句（MySQL 5.6.5 之前、TiDB 默认、Doris / ADB 等只兼容协议的库）时不报错，
// 退回只靠语句检查与账号权限，并按地址只记一次警告；网络类错误照常返回，不在没有兜底时悄悄继续。
func (e *Engine) enterReadOnly(ctx context.Context, conn *sql.Conn) (bool, error) {
	_, err := conn.ExecContext(ctx, sqlReadOnlyOn)
	if err == nil {
		return true, nil
	}
	var me *drv.MySQLError
	if !errors.As(err, &me) {
		return false, err
	}
	if _, warned := readOnlyUnsupported.LoadOrStore(e.addr, struct{}{}); !warned {
		slog.Warn("目标库不接受只读会话（"+sqlReadOnlyOn+"），在线查询只能依赖语句检查与账号权限；请确认查询账号只有 SELECT 权限",
			"addr", e.addr, "engine", e.name, "err", err)
	}
	return false, nil
}

// readOnlyUnsupported 记录已经警告过的实例地址。
var readOnlyUnsupported sync.Map

// leaveReadOnly 把会话切回读写。复位失败时这条连接可能还停在只读（或已断开），不能放回连接池，
// 否则同一个池里之后的 ExecEach 会莫名其妙地失败。
func leaveReadOnly(conn *sql.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := conn.ExecContext(ctx, sqlReadOnlyOff); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}

// discard 让这条连接不再回到连接池（带有会话变量的连接不能被别的请求复用）。
func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

func (e *Engine) Name() string { return e.name }

func (e *Engine) Close() error { return e.db.Close() }

func (e *Engine) TestConnection(ctx context.Context) error {
	return e.db.PingContext(ctx)
}

func (e *Engine) ServerVersion(ctx context.Context) (string, error) {
	var v string
	err := e.db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v)
	return v, err
}

func (e *Engine) AllDatabases(ctx context.Context) ([]string, error) {
	rows, err := e.db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		if !systemDBs[strings.ToLower(n)] {
			out = append(out, n)
		}
	}
	return out, rows.Err()
}

func (e *Engine) AllTables(ctx context.Context, db string) ([]string, error) {
	return e.singleColumn(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema = ? ORDER BY table_name", db)
}

func (e *Engine) AllColumns(ctx context.Context, db, table string) ([]string, error) {
	return e.singleColumn(ctx, "SELECT column_name FROM information_schema.columns WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position", db, table)
}

func (e *Engine) singleColumn(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := e.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Query 在指定库上执行查询。超出 Limit 的行被丢弃；调用方负责 SQL 校验与权限检查。
func (e *Engine) Query(ctx context.Context, q string, opt engine.QueryOptions) (*engine.ResultSet, error) {
	if opt.MaxExecMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(opt.MaxExecMs)*time.Millisecond)
		defer cancel()
	}
	// 用单独连接，保证 USE 与查询在同一会话。
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// 先于其它一切切到只读会话：上层的语句检查是词法级的，这里让数据库自己兜底（带写副作用的存储函数等）。
	// defer 后进先出：结果集（下面的 rows.Close）先关闭，再复位，最后才把连接还给连接池。
	guarded, err := e.enterReadOnly(ctx, conn)
	if err != nil {
		return nil, err
	}
	if guarded {
		defer leaveReadOnly(conn)
	}

	if opt.DBName != "" {
		if _, err := conn.ExecContext(ctx, "USE `"+strings.ReplaceAll(opt.DBName, "`", "``")+"`"); err != nil {
			return nil, err
		}
	}

	// 服务端超时（MySQL 5.7.8+，仅对 SELECT 生效）作为第二道保险；不支持的版本忽略错误。
	if opt.MaxExecMs > 0 {
		_, _ = conn.ExecContext(ctx, "SET SESSION max_execution_time = "+strconv.FormatInt(opt.MaxExecMs, 10))
	}

	// 超时或用户取消时，客户端断开连接并不会让服务端停止执行；记下本会话的 connection id，
	// 在 ctx 结束时用另一条连接发送 KILL QUERY。done 在本函数返回前先于 ctx 的 cancel 关闭，
	// 正常结束的查询不会触发 KILL。
	var connID int64
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&connID); err == nil && connID > 0 {
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				e.killQuery(connID)
			case <-done:
			}
		}()
	}

	// 前置语句（用户变量赋值，以及脚本中间的查询）：与查询在同一条连接、同一个只读会话里按顺序执行；查询语句的结果被驱动丢弃。
	// 放在 KILL 守护之后，超时 / 取消同样能终止它们（query.max_execution_seconds 只管 SELECT，不管 SET @v := (SELECT …)）。
	// 变量只存在于这条连接上，查询结束后连接被丢弃，不会带给下一个人。
	if len(opt.Prelude) > 0 {
		defer discard(conn)
		for i, pre := range opt.Prelude {
			if _, err := conn.ExecContext(ctx, pre); err != nil {
				if IsReadOnlyViolation(err) {
					return nil, wrapReadOnlyViolation(err)
				}
				return nil, fmt.Errorf("脚本第 %d 条语句执行失败：%w", i+1, err)
			}
		}
	}

	start := time.Now()
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return nil, wrapReadOnlyViolation(err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	ctypes, _ := rows.ColumnTypes()
	dateOnly := make([]bool, len(cols))
	for i := range ctypes {
		dateOnly[i] = strings.EqualFold(ctypes[i].DatabaseTypeName(), "DATE")
	}
	rs := &engine.ResultSet{ColumnList: cols, Rows: [][]any{}}
	for rows.Next() {
		if opt.Limit > 0 && len(rs.Rows) >= opt.Limit {
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			vals[i] = normalize(v, dateOnly[i])
		}
		rs.Rows = append(rs.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapReadOnlyViolation(err)
	}
	rs.CostTimeMs = time.Since(start).Milliseconds()
	return rs, nil
}

// wrapReadOnlyViolation 把只读会话的拒绝包装成 engine.ErrReadOnlyViolation（原错误仍可 errors.As 取出）。
func wrapReadOnlyViolation(err error) error {
	if IsReadOnlyViolation(err) {
		return fmt.Errorf("%w: %w", engine.ErrReadOnlyViolation, err)
	}
	return err
}

// jsSafeInt 是 JavaScript 能精确表示的最大整数；超出的整数转成字符串，避免前端丢精度。
const jsSafeInt = 1<<53 - 1

// normalize 把驱动返回的值转成适合 JSON 展示的形式。
func normalize(v any, dateOnly bool) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case time.Time:
		if dateOnly {
			return x.Format("2006-01-02")
		}
		return x.Format("2006-01-02 15:04:05")
	case int64:
		if x > jsSafeInt || x < -jsSafeInt {
			return strconv.FormatInt(x, 10)
		}
	case uint64:
		if x > jsSafeInt {
			return strconv.FormatUint(x, 10)
		}
	}
	return v
}

// killQuery 在另一条连接上终止指定会话正在执行的语句（KILL QUERY 只停语句，不断开会话）。
func (e *Engine) killQuery(connID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := e.db.ExecContext(ctx, "KILL QUERY "+strconv.FormatInt(connID, 10)); err != nil {
		slog.Warn("KILL QUERY 失败", "conn_id", connID, "err", err)
	}
}

// ReadOnly 判断实例是否只读（@@global.read_only / super_read_only）。
func (e *Engine) ReadOnly(ctx context.Context) (bool, error) {
	var ro, sro int
	if err := e.db.QueryRowContext(ctx, "SELECT @@global.read_only, IFNULL(@@global.super_read_only, 0)").Scan(&ro, &sro); err != nil {
		// 老版本没有 super_read_only
		if err2 := e.db.QueryRowContext(ctx, "SELECT @@global.read_only").Scan(&ro); err2 != nil {
			return false, err
		}
	}
	return ro != 0 || sro != 0, nil
}

// ExecEach 在同一个会话里逐条执行语句（先 USE 库），失败即停。
// 语句因 context 结束（人工取消执行 / 超时）时，驱动只会关掉本地连接，所以这里补一条 KILL QUERY（见 killQuery）。
func (e *Engine) ExecEach(ctx context.Context, db string, stmts []string) ([]engine.ExecStmtResult, error) {
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if db != "" {
		if _, err := conn.ExecContext(ctx, "USE `"+strings.ReplaceAll(db, "`", "``")+"`"); err != nil {
			return nil, fmt.Errorf("切换到库 %s 失败: %w", db, err)
		}
	}
	// 取消（人工取消执行 / 超时）时驱动只会关掉本地连接，目标库上这条语句可能还在跑：
	// 记下这条连接的线程 ID，在语句因 context 结束时补一条 KILL QUERY，让目标库真的停下来并回滚这一条。
	var connID int64
	_ = conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&connID)
	out := make([]engine.ExecStmtResult, len(stmts))
	failed := false
	for i, s := range stmts {
		out[i].SQL = s
		if failed {
			out[i].Skipped = true
			continue
		}
		start := time.Now()
		res, err := conn.ExecContext(ctx, s)
		out[i].ElapsedMs = float64(time.Since(start).Microseconds()) / 1000
		if err != nil {
			out[i].Err = err.Error()
			failed = true
			if ctx.Err() != nil && connID > 0 {
				e.killQuery(connID)
			}
			continue
		}
		if n, err := res.RowsAffected(); err == nil {
			out[i].Affected = n
		}
	}
	return out, nil
}
