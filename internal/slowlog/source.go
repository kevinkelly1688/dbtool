// @author: kevinkelly
// @license: Apache Licence
// @file: source.go
// @time: 2026/10/28

package slowlog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	drv "github.com/go-sql-driver/mysql"

	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// 目标库上的采集 SQL。
//
// query_time / lock_time 保留小数秒；显式 LIMIT。
const (
	// mysql.slow_log（需要 log_output 包含 TABLE）。MySQL / PolarDB / PolarDB-X 通用。
	mysqlSlowLogSQL = "SELECT CAST(UNIX_TIMESTAMP(start_time)*1000000 + MICROSECOND(start_time) AS SIGNED) AS micro, " +
		"DATE_FORMAT(start_time, '%Y-%m-%d %H:%i:%s.%f'), user_host, " +
		"TIME_TO_SEC(query_time) + MICROSECOND(query_time)/1000000, TIME_TO_SEC(lock_time) + MICROSECOND(lock_time)/1000000, " +
		"rows_sent, rows_examined, db, CONVERT(sql_text USING utf8mb4) " +
		"FROM mysql.slow_log " +
		"WHERE CAST(UNIX_TIMESTAMP(start_time)*1000000 + MICROSECOND(start_time) AS SIGNED) > ? " +
		"ORDER BY start_time ASC LIMIT ?"

	// Doris / SelectDB 的审计日志（query_time 单位毫秒，超过 1 秒的算慢查询）。
	dorisAuditSQL = "SELECT UNIX_TIMESTAMP(`time`) * 1000000 AS micro, CAST(`time` AS STRING), CONCAT(`user`, '@', client_ip), " +
		"query_time / 1000, 0, return_rows, scan_rows, db, CAST(stmt AS STRING) " +
		"FROM __internal_schema.audit_log " +
		"WHERE query_time > 1000 AND db != '__internal_schema' AND UNIX_TIMESTAMP(`time`) * 1000000 > ? " +
		"ORDER BY `time` ASC LIMIT ?"
)

// NewDBSource 返回连接目标库的真实 Source。conn 解密凭据（store.EngineConn）。
// 每次 Fetch 新开一个连接，查完就关（采集间隔很长，不值得常驻连接）。
func NewDBSource(conn ConnFunc) Source { return &dbSource{conn: conn} }

type dbSource struct{ conn ConnFunc }

func (s *dbSource) Fetch(ctx context.Context, inst *model.Instance, after int64, limit int) ([]Entry, error) {
	var q string
	switch inst.DBType {
	case "mysql", "polardb", "polardb-x":
		q = mysqlSlowLogSQL
	case "selectdb", "doris":
		q = dorisAuditSQL
	default:
		return nil, fmt.Errorf("暂不支持采集 %s 类型实例的慢日志", inst.DBType)
	}
	db, err := openTarget(s.conn(inst))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, q, after, limit)
	if err != nil {
		return nil, fmt.Errorf("读取慢日志: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var (
			micro, qt, lt           sql.NullFloat64
			sent, exam              sql.NullInt64
			start, host, dbn, query sql.NullString
		)
		if err := rows.Scan(&micro, &start, &host, &qt, &lt, &sent, &exam, &dbn, &query); err != nil {
			return nil, fmt.Errorf("解析慢日志行: %w", err)
		}
		out = append(out, Entry{Micro: int64(micro.Float64), StartTime: start.String, UserHost: host.String,
			QueryTimeSec: qt.Float64, LockTimeSec: lt.Float64, RowsSent: sent.Int64, RowsExamined: exam.Int64,
			DBName: dbn.String, SQLText: query.String})
	}
	return out, rows.Err()
}

// openTarget 打开目标库连接：带连接/读写超时，最多 1 个连接。
func openTarget(c engine.Conn) (*sql.DB, error) {
	cfg := drv.NewConfig()
	cfg.User, cfg.Passwd = c.User, c.Password
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%d", c.Host, c.Port)
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = 30 * time.Second
	cfg.WriteTimeout = 10 * time.Second
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
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(time.Minute)
	return db, nil
}
