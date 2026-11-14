// @author: kevinkelly
// @license: Apache Licence
// @file: engine.go
// @time: 2026/10/28

// Package oracle 是 Oracle 数据库引擎的 Go 实现（db_type "oracle"）：连接、schema / 表 / 列元数据、数据字典所需的表 / 列 / 索引 / DDL，
// 以及在线查询用的“只读语句静态检查 + 只读事务内受限执行 + 结果整形”。
//
// 范围：在线查询 + 库表元数据。不含 SQL 上线（写入）、会话 / 账号 / 参数管理、巡检。
// 驱动用 github.com/sijms/go-ora/v2（纯 Go，不需要 Oracle 客户端，可以在 CGO_ENABLED=0 的静态程序里用）。
// “库”对应 Oracle 的 schema（用户）：库表授权按 schema + 表名。
//
// 多层只读防护：
//  1. 语句静态检查 CheckUserQuery：单语句、类型白名单、内置函数白名单（自定义函数 / 包调用一律拒绝）、没有写操作词 / 数据库链接 / 序列取值，
//     不访问系统 schema 与 DBA_* / V$* 视图；
//  2. 执行前核对：语句里的表确实在 ALL_OBJECTS 里（同义词 / 当前 schema 的同名对象不会被偷换），内置函数名没有被同名的自定义对象盖过；
//  3. 事务内 SET TRANSACTION READ ONLY，结束时 ROLLBACK；
//  4. 超时：连接 / 执行的 context 期限（驱动在取消时向服务端发中断）；
//  5. 结果行数 / 字节上限、单元格截断；
//  6. 口令不进日志与错误信息。
//
// 注意：Oracle 没有像 PostgreSQL 那样的会话级只读开关。目标库账号必须只授予 SELECT（以及 CREATE SESSION）——这是最后一道、也是最可靠的一道防线，
// 见帮助文档“连接 Oracle / 达梦的账号要求”。
package oracle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	go_ora "github.com/sijms/go-ora/v2"

	"dbtool/internal/engine"
)

func init() {
	engine.Register(DBType, func(c engine.Conn) (engine.Engine, error) { return New(c) })
}

// 参数。
const (
	connectTimeout = 10 * time.Second // 建连超时
	metaTimeout    = 30 * time.Second // 元数据查询超时
	maxOpenConns   = 4
	prefetchRows   = 200
)

// driverName 是 database/sql 里 go-ora 注册的驱动名；测试里换成假驱动。
var driverName = "oracle"

// Engine 实现 engine.Engine。创建时不连接；第一次用到时才建连接池。
type Engine struct {
	conn engine.Conn

	mu     sync.Mutex
	db     *sql.DB
	closed bool
}

// New 创建引擎（不立即连接）。
func New(c engine.Conn) (*Engine, error) {
	if strings.TrimSpace(c.Host) == "" {
		return nil, errors.New("Oracle 实例地址为空")
	}
	if c.Port == 0 {
		c.Port = 1521
	}
	if c.Port < 0 || c.Port > 65535 {
		return nil, errors.New("Oracle 端口不合法")
	}
	c.ServiceName, c.SID = strings.TrimSpace(c.ServiceName), strings.TrimSpace(c.SID)
	if c.ServiceName == "" && c.SID == "" {
		return nil, errors.New("Oracle 实例需要填写 service_name 或 SID")
	}
	if strings.TrimSpace(c.User) == "" {
		return nil, errors.New("Oracle 实例的用户名为空")
	}
	return &Engine{conn: c}, nil
}

func (e *Engine) Name() string { return DBType }

// dsn 构造 go-ora 的连接串（口令等特殊字符由 BuildUrl 转义）。服务名优先，没有才用 SID。
func (e *Engine) dsn() string {
	opts := map[string]string{
		"CONNECT TIMEOUT": strconv.Itoa(int(connectTimeout / time.Second)),
		"LOB FETCH":       "PRE", // LOB 随行取回（CLOB 是 string，BLOB 是 []byte），不用流
		"PREFETCH_ROWS":   strconv.Itoa(prefetchRows),
	}
	if e.conn.ServiceName == "" {
		opts["SID"] = e.conn.SID
	}
	if e.conn.SSL {
		opts["SSL"] = "true"
		if e.conn.VerifySSL {
			opts["SSL VERIFY"] = "true"
		} else {
			opts["SSL VERIFY"] = "false"
		}
	}
	return go_ora.BuildUrl(e.conn.Host, e.conn.Port, e.conn.ServiceName, e.conn.User, e.conn.Password, opts)
}

// openDB 打开连接池（测试可替换）。
var openDB = func(dsn string) (*sql.DB, error) { return sql.Open(driverName, dsn) }

func (e *Engine) pool() (*sql.DB, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errors.New("Oracle 引擎已关闭")
	}
	if e.db != nil {
		return e.db, nil
	}
	db, err := openDB(e.dsn())
	if err != nil {
		return nil, mapError(err, e.conn.Password)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(time.Minute)
	e.db = db
	return db, nil
}

// Close 关闭连接池。可重复调用。
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	if e.db != nil {
		err := e.db.Close()
		e.db = nil
		return err
	}
	return nil
}

// getConn 从池里取一条连接（建连失败按连接错误处理）。
func (e *Engine) getConn(ctx context.Context) (*sql.Conn, error) {
	db, err := e.pool()
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, connectTimeout+5*time.Second)
	defer cancel()
	c, err := db.Conn(cctx)
	if err != nil {
		return nil, connectError(err, e.conn.Host, e.conn.Port, e.conn.Password)
	}
	return c, nil
}

// TestConnection 取一条连接执行 SELECT 1 FROM DUAL（引擎每次按当前配置新建，所以测的就是当前配置）。
func (e *Engine) TestConnection(ctx context.Context) error {
	conn, err := e.getConn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	qctx, cancel := context.WithTimeout(ctx, metaTimeout)
	defer cancel()
	var one any
	if err := conn.QueryRowContext(qctx, "SELECT 1 FROM DUAL").Scan(&one); err != nil {
		return mapError(err, e.conn.Password)
	}
	return nil
}

// ServerVersion 返回数据库版本（V$VERSION 的第一行；读不到时退到 PRODUCT_COMPONENT_VERSION）。
func (e *Engine) ServerVersion(ctx context.Context) (string, error) {
	for _, q := range []string{
		"SELECT banner FROM v$version WHERE ROWNUM = 1",
		"SELECT version FROM product_component_version WHERE ROWNUM = 1",
	} {
		rows, err := e.queryAll(ctx, q)
		if err == nil && len(rows) > 0 && len(rows[0]) > 0 {
			if s := asString(rows[0][0]); s != "" {
				return s, nil
			}
		}
	}
	return "", errors.New("读不到 Oracle 版本（账号没有 V$VERSION / PRODUCT_COMPONENT_VERSION 的查询权限）")
}

// AllDatabases 返回账号能看到的 schema（用户），排除系统 schema。
func (e *Engine) AllDatabases(ctx context.Context) ([]string, error) {
	names, err := e.strings(ctx, "SELECT username FROM all_users ORDER BY username")
	if err != nil {
		return nil, err
	}
	out := names[:0]
	for _, n := range names {
		if !isSystemSchema(n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// AllTables 返回 schema 里账号看得到的表 / 视图 / 物化视图名（不含回收站对象）。
func (e *Engine) AllTables(ctx context.Context, db string) ([]string, error) {
	if err := validSchemaName(db); err != nil {
		return nil, err
	}
	return e.strings(ctx, "SELECT object_name FROM all_objects WHERE owner = "+lit(db)+
		" AND object_type IN ('TABLE', 'VIEW', 'MATERIALIZED VIEW') AND object_name NOT LIKE 'BIN$%' ORDER BY object_name")
}

// AllColumns 返回表的列名（按列序）。
func (e *Engine) AllColumns(ctx context.Context, db, table string) ([]string, error) {
	if err := validSchemaName(db); err != nil {
		return nil, err
	}
	if err := validObjectName(table); err != nil {
		return nil, err
	}
	return e.strings(ctx, "SELECT column_name FROM all_tab_columns WHERE owner = "+lit(db)+" AND table_name = "+lit(table)+" ORDER BY column_id")
}

// Query 实现 engine.Engine：执行一条只读查询（供数据导出等使用）。语句仍会过一遍 CheckUserQuery，不信任调用方。
// opt.DBName 是当前 schema；opt.Limit<=0 表示不限制，但仍受 HardMaxRows / HardMaxBytes 保护，超出返回错误而不是静默截断。
func (e *Engine) Query(ctx context.Context, sql string, opt engine.QueryOptions) (*engine.ResultSet, error) {
	chk, err := CheckUserQuery(sql, CheckOptions{DB: opt.DBName})
	if err != nil {
		return nil, err
	}
	lim := QueryLimits{MaxRows: opt.Limit, MaxBytes: HardMaxBytes}
	if lim.MaxRows <= 0 {
		lim.MaxRows = HardMaxRows
	}
	if opt.MaxExecMs > 0 {
		lim.Timeout = time.Duration(opt.MaxExecMs) * time.Millisecond
	}
	sh, err := e.RunUser(ctx, chk, lim)
	if err != nil {
		return nil, err
	}
	if sh.Truncated && (opt.Limit <= 0 || sh.TruncatedBy == TruncBytes) {
		return nil, uerr("查询结果超过上限（%s），请缩小范围或加行数限制", sh.TruncatedBy)
	}
	rs := &engine.ResultSet{ColumnList: sh.ColumnList, Rows: sh.Rows, CostTimeMs: sh.Elapsed.Milliseconds()}
	if rs.Rows == nil {
		rs.Rows = [][]any{}
	}
	return rs, nil
}

var _ engine.Engine = (*Engine)(nil)

// ---- 元数据查询的小工具 ----

// queryAll 在池里的一条连接上执行元数据查询，返回全部行（驱动值原样）。
func (e *Engine) queryAll(ctx context.Context, q string) ([][]any, error) {
	conn, err := e.getConn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	qctx, cancel := context.WithTimeout(ctx, metaTimeout)
	defer cancel()
	rows, err := conn.QueryContext(qctx, q)
	if err != nil {
		return nil, mapError(err, e.conn.Password)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, mapError(err, e.conn.Password)
	}
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, mapError(err, e.conn.Password)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, e.conn.Password)
	}
	return out, nil
}

func (e *Engine) strings(ctx context.Context, q string) ([]string, error) {
	rows, err := e.queryAll(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if len(r) > 0 && r[0] != nil {
			out = append(out, asString(r[0]))
		}
	}
	return out, nil
}

// lit 返回 SQL 字符串字面量（单引号翻倍）。名字都来自界面 / 库里的元数据，并且已校验过没有控制字符。
func lit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// validObjectName 校验表 / 对象名：非空、不含控制字符。
func validObjectName(name string) error {
	if strings.TrimSpace(name) == "" || len(name) > 128 {
		return uerr("对象名不合法")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return uerr("对象名不合法")
		}
	}
	return nil
}

func asString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case fmt.Stringer:
		return x.String()
	}
	return fmt.Sprint(v)
}
