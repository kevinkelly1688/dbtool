// @author: kevinkelly
// @license: Apache Licence
// @file: dialer.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	drv "github.com/go-sql-driver/mysql"

	"dbtool/internal/engine"
)

// sqlDialer 是 Dialer 的真实实现：MySQL 协议系用 database/sql + go-sql-driver/mysql，
// 其他类型的连通性测试走 internal/engine 里已注册的引擎。
type sqlDialer struct{}

// NewDialer 返回连接真实目标库的 Dialer。
func NewDialer() Dialer { return sqlDialer{} }

const (
	connectTimeout = 10 * time.Second
	readTimeout    = 2 * time.Minute
	writeTimeout   = 30 * time.Second
	maxRows        = 20000 // 单次查询最多读取的行数，防止异常大的结果撑爆内存
)

// mysqlConfig 构造驱动配置（不含隧道拨号）。
// interpolateParams：参数由驱动在客户端转义后拼进语句，这样 KILL / GRANT / CREATE USER / SET GLOBAL 等
// 不支持服务端预编译的管理语句也能用占位符；驱动会跟随服务器的 NO_BACKSLASH_ESCAPES 状态选择转义方式。
// 字符集固定 utf8mb4：interpolateParams 不能与 gbk 等不安全的排序规则一起用，管理语句也不需要别的字符集。
func mysqlConfig(c ConnInfo) *drv.Config {
	cfg := drv.NewConfig()
	cfg.User, cfg.Passwd = c.User, c.Password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = connectTimeout, readTimeout, writeTimeout
	cfg.InterpolateParams = true
	// 字符集必须用 Charset 选项（连接建立后发 SET NAMES）；写进 cfg.Params 会被驱动当成会话变量发 `SET charset=…`，
	// MySQL 8.0 报 1193 Unknown system variable 'charset'（只有经 FormatDSN/ParseDSN 绕一圈 charset 才会被识别）。
	_ = cfg.Apply(drv.Charset("utf8mb4", ""))
	if c.SSL {
		if c.VerifySSL {
			cfg.TLSConfig = "true"
		} else {
			cfg.TLSConfig = "skip-verify"
		}
	}
	return cfg
}

func (sqlDialer) Dial(ctx context.Context, c ConnInfo) (Target, error) {
	if !mysqlLike[c.DBType] {
		return nil, fmt.Errorf("暂不支持 %s 类型", dbTypeLabel(c.DBType))
	}
	cfg := mysqlConfig(c)
	var closeSSH func()
	if c.Tunnel != nil {
		cli, err := dialSSH(ctx, c.Tunnel)
		if err != nil {
			return nil, fmt.Errorf("SSH 隧道连接失败：%w", err)
		}
		cfg.DialFunc = sshDialFunc(cli)
		closeSSH = func() { _ = cli.Close() }
	}
	fail := func(err error) (Target, error) {
		if closeSSH != nil {
			closeSSH()
		}
		return nil, err
	}
	connector, err := drv.NewConnector(cfg)
	if err != nil {
		return fail(err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)
	// 管理操作固定在一条连接上，CONNECTION_ID() 前后一致。
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return fail(err)
	}
	return &mysqlTarget{db: db, conn: conn, closeSSH: closeSSH}, nil
}

// Probe 测试非 MySQL 协议类型的连通性（SQL Server 等，走 internal/engine 里已注册的引擎）。
func (sqlDialer) Probe(ctx context.Context, c ConnInfo) (string, error) {
	if c.Tunnel != nil {
		return "", fmt.Errorf("暂不支持经 SSH 隧道测试 %s 类型实例", dbTypeLabel(c.DBType))
	}
	eng, err := engine.New(c.DBType, engine.Conn{Host: c.Host, Port: c.Port, User: c.User, Password: c.Password, DBName: c.DBName,
		Charset: c.Charset, SSL: c.SSL, VerifySSL: c.VerifySSL, ServiceName: c.ServiceName, SID: c.SID})
	if err != nil {
		return "", err
	}
	defer eng.Close()
	if err := eng.TestConnection(ctx); err != nil {
		return "", err
	}
	v, _ := eng.ServerVersion(ctx)
	return v, nil
}

type mysqlTarget struct {
	db       *sql.DB
	conn     *sql.Conn
	closeSSH func()
}

func (t *mysqlTarget) Close() error {
	err := t.conn.Close()
	if e := t.db.Close(); err == nil {
		err = e
	}
	if t.closeSSH != nil {
		t.closeSSH()
	}
	return err
}

func (t *mysqlTarget) Exec(ctx context.Context, q string, args ...any) (int64, error) {
	res, err := t.conn.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (t *mysqlTarget) Query(ctx context.Context, q string, args ...any) (*Result, error) {
	rows, err := t.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	res := &Result{Columns: make([]string, len(cols))}
	for i, c := range cols {
		res.Columns[i] = strings.ToLower(c)
	}
	for rows.Next() {
		if len(res.Rows) >= maxRows {
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
			vals[i] = normalizeValue(v)
		}
		res.Rows = append(res.Rows, vals)
	}
	return res, rows.Err()
}

// normalizeValue 把驱动返回的值规整为 nil / string / int64 / uint64 / float64。
func normalizeValue(v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case time.Time:
		return x.Format("2006-01-02 15:04:05")
	}
	return v
}
