// @author: kevinkelly
// @license: Apache Licence
// @file: engine.go
// @time: 2026/10/28

// Package postgres 是 PostgreSQL 引擎的 Go 实现（db_type "pgsql"）：连接、库 / 表 / 列元数据、数据字典所需的表 / 列 / 索引 / 近似 DDL，
// 以及在线查询用的“只读语句静态检查 + 只读事务内受限执行 + 结果整形”。
//
// 第一期范围：在线查询 + 库表元数据。不含 SQL 上线（写入）、会话 / 账号 / 参数管理、巡检。
// 驱动用 github.com/jackc/pgx/v5 的 pgconn 层（文本格式结果，扩展查询协议，由服务端保证一次只执行一条语句）。
//
// 多层只读防护：
//  1. 语句静态检查 CheckUserQuery：单语句、类型白名单、危险函数 / 敏感系统视图黑名单；
//  2. 连接层：启动参数 default_transaction_read_only=on，并且每次查询都在 BEGIN READ ONLY 事务里执行、结束时 ROLLBACK；
//  3. 语句超时：statement_timeout（服务端）+ 比它略长的客户端超时 + 取消时发送 CancelRequest；
//  4. 结果行数 / 字节上限：用服务端游标分批取，超限截断并标记；
//  5. 单元格截断：超长文本 / bytea 截断；
//  6. 凭据不进日志与错误信息。
package postgres

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"dbtool/internal/engine"
)

// DBType 是 sql_instance.db_type 里 PostgreSQL 的取值。
const DBType = "pgsql"

func init() {
	engine.Register(DBType, func(c engine.Conn) (engine.Engine, error) { return New(c) })
}

// 连接池参数。
const (
	maxDBPools     = 8                // 同时缓存连接的库数量上限
	maxConnsPerDB  = 4                // 每个库同时打开的连接数上限
	idleConnTTL    = 5 * time.Minute  // 空闲连接保留时长
	connectTimeout = 10 * time.Second // 建连超时
	metaTimeout    = 30 * time.Second // 元数据查询超时
)

// Engine 实现 engine.Engine。连接绑定在 database 上，所以按库缓存连接池（有数量上限、空闲回收）；创建时不连接。
type Engine struct {
	conn engine.Conn

	mu       sync.Mutex
	pools    map[string]*pool
	closed   bool
	stop     chan struct{}
	janitors sync.Once
}

// New 创建引擎（不立即连接）。
func New(c engine.Conn) (*Engine, error) {
	if strings.TrimSpace(c.Host) == "" {
		return nil, errors.New("PostgreSQL 实例地址为空")
	}
	if c.Port == 0 {
		c.Port = 5432
	}
	if c.Port < 0 || c.Port > 65535 {
		return nil, errors.New("PostgreSQL 端口不合法")
	}
	return &Engine{conn: c, pools: map[string]*pool{}, stop: make(chan struct{})}, nil
}

func (e *Engine) Name() string { return DBType }

// defaultDB 是不指定库时连接的库（实例配置的默认库，缺省 postgres）。
func (e *Engine) defaultDB() string {
	if db := strings.TrimSpace(e.conn.DBName); db != "" {
		return db
	}
	return "postgres"
}

func (e *Engine) dbOrDefault(db string) string {
	if strings.TrimSpace(db) == "" {
		return e.defaultDB()
	}
	return db
}

// Close 关闭所有空闲连接；仍在使用中的连接在归还时关闭。可重复调用。
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	close(e.stop)
	pools := e.pools
	e.pools = map[string]*pool{}
	e.mu.Unlock()
	for _, p := range pools {
		p.closeIdle()
	}
	return nil
}

// TestConnection 用一条新连接执行 SELECT 1（不走连接池，保证测的是当前配置）。
func (e *Engine) TestConnection(ctx context.Context) error {
	pc, err := e.dial(ctx, e.defaultDB())
	if err != nil {
		return err
	}
	defer closeConn(pc)
	res := pc.ExecParams(ctx, "SELECT 1", nil, nil, nil, nil).Read()
	if res.Err != nil {
		return mapError(res.Err, e.conn.Password)
	}
	return nil
}

// ServerVersion 返回 server_version（如 16.2 (Debian 16.2-1.pgdg120+2)）。
func (e *Engine) ServerVersion(ctx context.Context) (string, error) {
	var v string
	err := e.readTx(ctx, e.defaultDB(), metaTimeout, nil, func(s *session) error {
		rows, err := s.rows(ctx, "SHOW server_version")
		if err != nil {
			return err
		}
		if len(rows) > 0 && rows[0][0] != nil {
			v = *rows[0][0]
		}
		return nil
	})
	return v, err
}

// AllDatabases 返回当前账号可连接的库，排除 template0 / template1。
func (e *Engine) AllDatabases(ctx context.Context) ([]string, error) {
	return e.strings(ctx, e.defaultDB(), `SELECT d.datname FROM pg_catalog.pg_database d
WHERE d.datallowconn AND d.datname NOT IN ('template0', 'template1') AND pg_catalog.has_database_privilege(d.datname, 'CONNECT')
ORDER BY d.datname`)
}

// AllTables 返回库内账号有查询权限的表 / 视图 / 物化视图 / 外部表 / 分区表，名称是 schema.table；
// 不含系统 schema（pg_catalog、information_schema、pg_toast、pg_temp_*）与分区子表（子表可直接按名字查询）。
func (e *Engine) AllTables(ctx context.Context, db string) ([]string, error) {
	return e.strings(ctx, e.dbOrDefault(db), `SELECT n.nspname || '.' || c.relname
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f') AND NOT c.relispartition
  AND n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
  AND pg_catalog.has_any_column_privilege(c.oid, 'SELECT')
ORDER BY n.nspname, c.relname`)
}

// AllColumns 返回表的列名（按列序）。table 写成 schema.table（只按第一个点切分）；不带点时按 public。
func (e *Engine) AllColumns(ctx context.Context, db, table string) ([]string, error) {
	schema, name := splitTable(table)
	return e.strings(ctx, e.dbOrDefault(db), `SELECT a.attname
FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid = a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`, schema, name)
}

// splitTable 把 "schema.table" 拆开；没有点时 schema 取 public。
func splitTable(table string) (schema, name string) {
	if i := strings.Index(table, "."); i >= 0 {
		return table[:i], table[i+1:]
	}
	return DefaultSchema, table
}

func (e *Engine) strings(ctx context.Context, db, q string, params ...string) ([]string, error) {
	var out []string
	err := e.readTx(ctx, db, metaTimeout, nil, func(s *session) error {
		rows, err := s.rows(ctx, q, params...)
		if err != nil {
			return err
		}
		out = make([]string, 0, len(rows))
		for _, r := range rows {
			if r[0] != nil {
				out = append(out, *r[0])
			}
		}
		return nil
	})
	return out, err
}

// Query 实现 engine.Engine：执行一条只读查询（供数据导出等使用）。语句仍会过一遍 CheckUserQuery，不信任调用方。
// opt.Limit<=0 表示不限制，但仍受 HardMaxRows / HardMaxBytes 保护，超出返回错误而不是静默截断；opt.Limit>0 时超出的行被丢弃。
func (e *Engine) Query(ctx context.Context, sql string, opt engine.QueryOptions) (*engine.ResultSet, error) {
	chk, err := CheckUserQuery(sql, CheckOptions{DBName: opt.DBName})
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
	sh, err := e.RunUser(ctx, opt.DBName, chk, lim)
	if err != nil {
		return nil, err
	}
	if sh.Truncated && (opt.Limit <= 0 || sh.TruncatedBy == TruncBytes) {
		return nil, uerr("查询结果超过上限（%s），请缩小范围或加 LIMIT", sh.TruncatedBy)
	}
	rs := &engine.ResultSet{ColumnList: sh.ColumnNames(), Rows: sh.Rows, CostTimeMs: sh.Elapsed.Milliseconds()}
	if rs.Rows == nil {
		rs.Rows = [][]any{}
	}
	return rs, nil
}

var _ engine.Engine = (*Engine)(nil)

// IsReadOnlyViolation 判断 err 是否服务端因只读事务拒绝了写操作（SQLSTATE 25006）。
func IsReadOnlyViolation(err error) bool {
	var ue *UserError
	return errors.As(err, &ue) && ue.Code == "25006"
}

func closeConn(pc *pgconn.PgConn) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = pc.Close(ctx)
}
