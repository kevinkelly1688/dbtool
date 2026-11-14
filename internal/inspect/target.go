// @author: kevinkelly
// @license: Apache Licence
// @file: target.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	drv "github.com/go-sql-driver/mysql"
	mssqldb "github.com/microsoft/go-mssqldb"

	"dbtool/internal/engine"
)

// statsExpiryVar 是 information_schema 统计缓存时间的会话变量名；语句里带它（见 statsHint）时 sqlTarget 会先在该连接上把它设为 0。
const statsExpiryVar = "information_schema_stats_expiry"

const (
	connectTimeout   = 10 * time.Second
	defaultQueryTime = 60 * time.Second  // 单条查询超时
	mysqlQueryTime   = 150 * time.Second // MySQL 系单条查询超时：比语句里的 MAX_EXECUTION_TIME(120s) 略长，让服务端先终止
	maxResultRows    = 5000              // 单条查询最多读取的行数，防止异常大的结果撑爆内存
)

// ---- 只读校验 ----

var (
	leadingNoise = regexp.MustCompile(`(?s)^(?:\s+|/\*.*?\*/|--[^\n]*(?:\n|$)|#[^\n]*(?:\n|$))+`)
	readOnlyHead = regexp.MustCompile(`(?is)^(?:select|show|with|explain|desc|describe)\b`)
	// 即便语句以 SELECT 开头，也不允许产生副作用或持有锁的写法。
	sideEffects = regexp.MustCompile(`(?is)\binto\b|\bfor\s+update\b|\block\s+in\s+share\s+mode\b|\bget_lock\s*\(|\bsleep\s*\(|\bbenchmark\s*\(`)
	moreStmts   = regexp.MustCompile(`;\s*\S`)
)

// ErrNotReadOnly 表示语句不是只读查询。
var ErrNotReadOnly = errors.New("巡检只允许只读查询（SELECT / SHOW / WITH / EXPLAIN / DESC）")

// CheckReadOnly 校验语句是单条只读查询。巡检的语句都是代码里写死的，这里是兜底：任何变更语句都会被拒绝。
func CheckReadOnly(q string) error {
	s := q
	if loc := leadingNoise.FindStringIndex(s); loc != nil {
		s = s[loc[1]:]
	}
	if !readOnlyHead.MatchString(s) || sideEffects.MatchString(s) || moreStmts.MatchString(s) {
		return ErrNotReadOnly
	}
	return nil
}

// ---- database/sql 实现 ----

type sqlTarget struct {
	db      *sql.DB
	kind    string
	timeout time.Duration
	conn    engine.Conn // 只用于抹掉错误信息里的账号密码
}

// OpenSQL 是 Opener 的真实实现：MySQL 系用 go-sql-driver，SQL Server 用 go-mssqldb。
// 每个实例一个连接池，MaxOpenConns=2，连接超时 10 秒，单条查询默认 60 秒。
func OpenSQL(ctx context.Context, dbType string, c engine.Conn) (Target, error) {
	kind, ok := KindOf(dbType)
	if !ok {
		return nil, fmt.Errorf("暂不支持巡检的数据库类型：%s", dbType)
	}
	var db *sql.DB
	switch kind {
	case KindMSSQL:
		if c.Port == 0 {
			c.Port = 1433
		}
		q := url.Values{}
		if c.DBName != "" {
			q.Set("database", c.DBName)
		}
		q.Set("TrustServerCertificate", "true")
		q.Set("connection timeout", "10")
		q.Set("app name", "dbtool-inspect")
		u := url.URL{Scheme: "sqlserver", User: url.UserPassword(c.User, c.Password), Host: fmt.Sprintf("%s:%d", c.Host, c.Port), RawQuery: q.Encode()}
		connector, err := mssqldb.NewConnector(u.String())
		if err != nil {
			return nil, errors.New("SQL Server 连接参数不合法")
		}
		db = sql.OpenDB(connector)
	default:
		cfg := drv.NewConfig()
		cfg.User, cfg.Passwd, cfg.Net = c.User, c.Password, "tcp"
		cfg.Addr = fmt.Sprintf("%s:%d", c.Host, c.Port)
		cfg.ParseTime = true
		cfg.Timeout = connectTimeout
		cfg.ReadTimeout = mysqlQueryTime + 5*time.Second
		charset := c.Charset
		if charset == "" {
			charset = "utf8mb4"
		}
		cfg.Params = map[string]string{"charset": charset}
		if c.SSL {
			cfg.TLSConfig = "skip-verify"
			if c.VerifySSL {
				cfg.TLSConfig = "true"
			}
		}
		d, err := sql.Open("mysql", cfg.FormatDSN())
		if err != nil {
			return nil, errors.New("MySQL 连接参数不合法")
		}
		db = d
	}
	timeout := defaultQueryTime
	if kind == KindMySQL {
		timeout = mysqlQueryTime
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)
	pctx, cancel := context.WithTimeout(ctx, connectTimeout+2*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("无法连接到 %s:%d：%s", c.Host, c.Port, scrub(err.Error(), c))
	}
	return &sqlTarget{db: db, kind: kind, timeout: timeout, conn: c}, nil
}

// scrub 去掉错误信息里可能出现的账号密码。
func scrub(msg string, c engine.Conn) string {
	for _, s := range []string{c.Password, c.User} {
		if len(s) >= 3 {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	return msg
}

func quoteIdent(kind, name string) string {
	if kind == KindMSSQL {
		return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
	}
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// clean 抹掉目标库返回的错误信息里的账号密码（例如 1142 报错带 'user'@'host'）：错误信息会存进报告并展示给有页面权限的用户。
func (t *sqlTarget) clean(err error) error {
	if err == nil {
		return nil
	}
	if msg := scrub(err.Error(), t.conn); msg != err.Error() {
		return errors.New(msg)
	}
	return err
}

func (t *sqlTarget) Query(ctx context.Context, dbName, q string, args ...any) (res *Result, err error) {
	if err := CheckReadOnly(q); err != nil {
		return nil, err
	}
	defer func() { err = t.clean(err) }()
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	conn, err := t.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if t.kind == KindMySQL && strings.Contains(q, statsExpiryVar) {
		// MySQL 8.0 默认缓存 information_schema 的统计（最长 1 天，自增值等会不准）。语句内的 SET_VAR 优化器提示在 8.0.46 上实测不生效，
		// 所以给执行这条语句的连接设置会话变量（只影响这条连接，不改数据、不是全局设置）。5.7 没有这个变量，报错忽略。
		_, _ = conn.ExecContext(ctx, "SET SESSION "+statsExpiryVar+" = 0")
	}
	if dbName != "" {
		// 只切换当前库（不改任何数据）：库名来自目标库自己的目录视图，仍按标识符转义。
		if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(t.kind, dbName)); err != nil {
			return nil, err
		}
	}
	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	res = &Result{Cols: cols}
	for rows.Next() {
		if len(res.Rows) >= maxResultRows {
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
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		res.Rows = append(res.Rows, vals)
	}
	return res, rows.Err()
}

func (t *sqlTarget) Close() error { return t.db.Close() }
