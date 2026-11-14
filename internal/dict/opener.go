// @author: kevinkelly
// @license: Apache Licence
// @file: opener.go
// @time: 2026/10/28

package dict

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"time"

	drv "github.com/go-sql-driver/mysql"
	mssqldb "github.com/microsoft/go-mssqldb"

	"dbtool/internal/engine"
)

// Opener 按实例类型打开元数据连接。db 为空表示不指定库（列库时）；SQL Server 需要连到具体的库才能读它的系统目录。
type Opener func(ctx context.Context, dbType string, conn engine.Conn, db string) (Querier, error)

// maxRows 是单次查询读取的行数上限，防止异常大的元数据把内存吃光。
const maxRows = 300000

// DefaultOpener 用 database/sql 连接目标库：建连超时 10 秒，MaxOpenConns=2，用完由调用方 Close。
func DefaultOpener(_ context.Context, dbType string, c engine.Conn, db string) (Querier, error) {
	switch {
	case engineDialectType(dbType): // PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦：直接用引擎的元数据 API，库名在每次调用时给出
		return openEngine(dbType, c)
	case dbType == "mssql":
		return openMSSQL(c, db)
	case mysqlFamily[dbType]:
		return openMySQL(c)
	}
	return nil, newErr(501, "unsupported_engine", "该类型（%s）的实例暂不支持数据字典，目前支持 MySQL 系、SQL Server、PostgreSQL、MongoDB、ClickHouse、Oracle 与达梦", dbType)
}

func openMySQL(c engine.Conn) (Querier, error) {
	cfg := drv.NewConfig()
	cfg.User, cfg.Passwd = c.User, c.Password
	cfg.Net, cfg.Addr = "tcp", fmt.Sprintf("%s:%d", c.Host, c.Port)
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = 5 * time.Minute
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
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &sqlQuerier{db: db}, nil
}

func openMSSQL(c engine.Conn, dbName string) (Querier, error) {
	port := c.Port
	if port == 0 {
		port = 1433
	}
	q := url.Values{}
	if dbName != "" {
		q.Set("database", dbName)
	}
	q.Set("TrustServerCertificate", "true") // 证书一律信任
	q.Set("connection timeout", "10")
	q.Set("app name", "dbtool")
	u := url.URL{Scheme: "sqlserver", User: url.UserPassword(c.User, c.Password), Host: fmt.Sprintf("%s:%d", c.Host, port), RawQuery: q.Encode()}
	connector, err := mssqldb.NewConnector(u.String())
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &sqlQuerier{db: db}, nil
}

type sqlQuerier struct{ db *sql.DB }

func (q *sqlQuerier) Close() error { return q.db.Close() }

func (q *sqlQuerier) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	rs := &Rows{Columns: cols}
	for rows.Next() {
		if len(rs.Data) >= maxRows {
			return nil, newErr(413, "too_large", "元数据结果超过 %d 行，请缩小范围", maxRows)
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		rs.Data = append(rs.Data, vals)
	}
	return rs, rows.Err()
}
