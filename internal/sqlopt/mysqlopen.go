// @author: kevinkelly
// @license: Apache Licence
// @file: mysqlopen.go
// @time: 2026/10/28

package sqlopt

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"time"
	"unicode/utf8"

	drv "github.com/go-sql-driver/mysql"

	"dbtool/internal/engine"
)

// mysqlOpener 用 database/sql + go-sql-driver 连接 MySQL 协议的目标库（mysql、polardb、selectdb、doris、adb 等）。
// 不开启 multiStatements：驱动层面就拒绝“一次发多条语句”。
type mysqlOpener struct{}

func (mysqlOpener) Open(ctx context.Context, c engine.Conn, db string) (Session, error) {
	cfg := drv.NewConfig()
	cfg.User, cfg.Passwd = c.User, c.Password
	cfg.Net, cfg.Addr = "tcp", fmt.Sprintf("%s:%d", c.Host, c.Port)
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
	pool, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(2) // 一条跑业务语句，一条用来 KILL
	pool.SetConnMaxLifetime(5 * time.Minute)
	conn, err := pool.Conn(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	s := &mysqlSession{pool: pool, conn: conn}
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&s.connID); err != nil {
		s.Close()
		return nil, err
	}
	if db != "" {
		if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(db)); err != nil {
			s.Close()
			return nil, fmt.Errorf("切换到数据库 %s 失败：%w", db, err)
		}
	}
	return s, nil
}

type mysqlSession struct {
	pool   *sql.DB
	conn   *sql.Conn
	connID int64
}

func (s *mysqlSession) Close() error {
	_ = s.conn.Close()
	return s.pool.Close()
}

// guard 在 ctx 结束（超时或取消）时用另一条连接 KILL QUERY：客户端断开不会让服务端停止执行。
// 返回的函数在语句正常结束后调用，取消监控。
func (s *mysqlSession) guard(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			kctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := s.pool.ExecContext(kctx, "KILL QUERY "+strconv.FormatInt(s.connID, 10)); err != nil {
				slog.Warn("KILL QUERY 失败", "conn_id", s.connID, "err", err)
			}
		case <-done:
		}
	}()
	return func() { close(done) }
}

func (s *mysqlSession) Query(ctx context.Context, q string, args ...any) (*Table, error) {
	defer s.guard(ctx)()
	rows, err := s.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	t := &Table{Columns: cols, Rows: [][]any{}}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		if len(t.Rows) >= MaxTableRows {
			t.Truncated = true
			break
		}
		for i, v := range vals {
			vals[i] = normalizeCell(v)
		}
		t.Rows = append(t.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *mysqlSession) Drain(ctx context.Context, q string) (int64, error) {
	defer s.guard(ctx)()
	rows, err := s.conn.QueryContext(ctx, q)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	raw := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	var n int64
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

const jsSafeInt = 1<<53 - 1

// normalizeCell 把驱动返回的值转成适合 JSON 展示的形式：字节转字符串、超长截断、大整数转字符串。
func normalizeCell(v any) any {
	switch x := v.(type) {
	case []byte:
		return clipCell(string(x))
	case string:
		return clipCell(x)
	case int64:
		if x > jsSafeInt || x < -jsSafeInt {
			return strconv.FormatInt(x, 10)
		}
	case uint64:
		if x > jsSafeInt {
			return strconv.FormatUint(x, 10)
		}
	case time.Time:
		return x.Format("2006-01-02 15:04:05")
	}
	return v
}

func clipCell(s string) string {
	if len(s) <= MaxCellBytes {
		return s
	}
	cut := MaxCellBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…（已截断）"
}
