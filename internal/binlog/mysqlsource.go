// @author: kevinkelly
// @license: Apache Licence
// @file: mysqlsource.go
// @time: 2026/10/28

package binlog

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	drv "github.com/go-sql-driver/mysql"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"

	"dbtool/internal/engine"
)

// mysqlSource 用 database/sql 读元信息（只读查询），用 go-mysql 的 BinlogSyncer 拉取 binlog。
// 拉取时以复制从库的身份注册（COM_REGISTER_SLAVE + COM_BINLOG_DUMP），需要账号有 REPLICATION SLAVE 权限；
// 不会修改目标库的任何数据，也不会改变主从复制状态。
type mysqlSource struct {
	idle time.Duration // 拉取 binlog 时多久没有新事件就认为读完了（NON_BLOCK 模式下服务端到末尾只发 EOF）
}

const (
	defaultIdle     = 8 * time.Second
	connectTimeout  = 10 * time.Second
	queryTimeoutSec = 30
)

type mysqlSession struct {
	c      engine.Conn
	db     *sql.DB
	idle   time.Duration
	flavor string
}

func (m mysqlSource) Connect(ctx context.Context, c engine.Conn) (Session, error) {
	cfg := drv.NewConfig()
	cfg.User, cfg.Passwd = c.User, c.Password
	cfg.Net, cfg.Addr = "tcp", fmt.Sprintf("%s:%d", c.Host, c.Port)
	cfg.Timeout = connectTimeout
	cfg.ReadTimeout = queryTimeoutSec * time.Second
	cfg.WriteTimeout = queryTimeoutSec * time.Second
	cfg.Params = map[string]string{"charset": "utf8mb4"}
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
	pool.SetMaxOpenConns(2)
	pool.SetConnMaxLifetime(5 * time.Minute)
	if err := pool.PingContext(ctx); err != nil {
		_ = pool.Close()
		return nil, err
	}
	idle := m.idle
	if idle <= 0 {
		idle = defaultIdle
	}
	return &mysqlSession{c: c, db: pool, idle: idle, flavor: mysql.MySQLFlavor}, nil
}

func (s *mysqlSession) Close() error { return s.db.Close() }

func (s *mysqlSession) Info(ctx context.Context) (*ServerInfo, error) {
	rows, err := s.db.QueryContext(ctx, "SHOW GLOBAL VARIABLES WHERE Variable_name IN ('version','log_bin','binlog_format','binlog_row_image','binlog_transaction_compression')")
	if err != nil {
		return nil, err
	}
	vars := map[string]string{}
	for rows.Next() {
		var k, v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			_ = rows.Close()
			return nil, err
		}
		vars[strings.ToLower(k.String)] = v.String
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	info := &ServerInfo{Version: vars["version"], Flavor: mysql.MySQLFlavor,
		LogBin:      isOn(vars["log_bin"]),
		Format:      strings.ToUpper(vars["binlog_format"]),
		RowImage:    strings.ToUpper(vars["binlog_row_image"]),
		Compression: isOn(vars["binlog_transaction_compression"]),
		Files:       []LogFile{}}
	if strings.Contains(strings.ToLower(info.Version), "mariadb") {
		info.Flavor = mysql.MariaDBFlavor
	}
	s.flavor = info.Flavor
	var off sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT TIMESTAMPDIFF(SECOND, UTC_TIMESTAMP(), NOW())").Scan(&off); err == nil && off.Valid {
		info.TZOffset = int(off.Int64)
	}
	if info.LogBin {
		files, err := s.logs(ctx)
		if err != nil {
			return nil, err
		}
		info.Files = files
	}
	return info, nil
}

func isOn(v string) bool {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "ON", "1", "TRUE", "YES":
		return true
	}
	return false
}

// logs 执行 SHOW BINARY LOGS。列名因版本而异（Log_name、File_size、Encrypted），按位置取前两列。
func (s *mysqlSession) logs(ctx context.Context) ([]LogFile, error) {
	rows, err := s.db.QueryContext(ctx, "SHOW BINARY LOGS")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []LogFile
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		if len(vals) < 2 {
			continue
		}
		size, _ := strconv.ParseInt(vals[1].String, 10, 64)
		out = append(out, LogFile{Name: vals[0].String, Size: size})
	}
	return out, rows.Err()
}

func (s *mysqlSession) Databases(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SHOW DATABASES")
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
		out = append(out, n)
	}
	return out, rows.Err()
}

// Columns 读表的列信息。查询里的库名、表名来自 binlog 的表映射事件，按字符串字面量转义后拼入。
func (s *mysqlSession) Columns(ctx context.Context, schema, table string) ([]colMeta, error) {
	q := "SELECT COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, COLUMN_KEY, EXTRA FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = " +
		quoteString(schema) + " AND TABLE_NAME = " + quoteString(table) + " ORDER BY ORDINAL_POSITION"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []colMeta
	for rows.Next() {
		var name, dt, ct, key, extra sql.NullString
		if err := rows.Scan(&name, &dt, &ct, &key, &extra); err != nil {
			return nil, err
		}
		c := colMeta{Name: name.String, DataType: strings.ToLower(dt.String), ColumnType: ct.String,
			PK: key.String == "PRI"}
		lct := strings.ToLower(ct.String)
		c.Unsigned = strings.Contains(lct, "unsigned")
		ex := strings.ToUpper(extra.String)
		c.Generated = strings.Contains(ex, "GENERATED") || strings.Contains(ex, "PERSISTENT")
		c.Virtual = strings.Contains(ex, "VIRTUAL")
		switch c.DataType {
		case "enum":
			c.Enum = parseQuotedList(ct.String)
		case "set":
			c.Set = parseQuotedList(ct.String)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// parseQuotedList 解析 enum('a','b”c') / set(...) 里的取值列表。
func parseQuotedList(ct string) []string {
	i := strings.IndexByte(ct, '(')
	j := strings.LastIndexByte(ct, ')')
	if i < 0 || j <= i {
		return nil
	}
	body := ct[i+1 : j]
	var out []string
	var cur strings.Builder
	inQ := false
	for k := 0; k < len(body); k++ {
		ch := body[k]
		switch {
		case !inQ && ch == '\'':
			inQ = true
			cur.Reset()
		case inQ && ch == '\'':
			if k+1 < len(body) && body[k+1] == '\'' {
				cur.WriteByte('\'')
				k++
			} else {
				inQ = false
				out = append(out, cur.String())
			}
		case inQ && ch == '\\' && k+1 < len(body):
			k++
			cur.WriteByte(body[k])
		case inQ:
			cur.WriteByte(ch)
		}
	}
	return out
}

func randServerID() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return 1<<31 | binary.BigEndian.Uint32(b[:])&0x3fffffff // 2^31 以上，避开真实从库常用的小编号
}

// errStalled 表示一段时间没有收到新事件，而且服务端没有发过“已经没有更多事件”的 EOF 包：目标库可能卡住了，不能当成读完。
var errStalled = errors.New("目标库长时间没有返回 binlog 事件")

// eofLogText 是 go-mysql 在非阻塞模式下收到 EOF 包（服务端：没有更多事件了）时写的那条日志。库只写日志、不通知调用方，
// 所以用日志处理器把它接住，当作“真读完了”的明确信号；库升级后这条日志的文字变了也没关系——没接到信号时会退回按结束位置判断（scanner.reachedEnd）。
const eofLogText = "receive EOF packet"

// eofGrace 是收到 EOF 包之后再等多久：缓冲里没有事件了就是真读完，不必再等满 idle。
const eofGrace = 200 * time.Millisecond

type eofHandler struct{ flag *atomic.Bool }

func (h *eofHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *eofHandler) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, eofLogText) {
		h.flag.Store(true)
	}
	return nil
}
func (h *eofHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *eofHandler) WithGroup(string) slog.Handler      { return h }

type syncStream struct {
	syncer   *replication.BinlogSyncer
	streamer *replication.BinlogStreamer
	idle     time.Duration
	secrets  []string
	eof      *atomic.Bool // 服务端已经发过“没有更多事件”的 EOF 包
}

func (s *mysqlSession) Stream(ctx context.Context, file string, pos uint32, loc *time.Location) (EventStream, error) {
	if pos < 4 {
		pos = 4
	}
	eof := new(atomic.Bool)
	cfg := replication.BinlogSyncerConfig{
		ServerID: randServerID(), Flavor: s.flavor, Host: s.c.Host, Port: uint16(s.c.Port), User: s.c.User, Password: s.c.Password,
		Charset: "utf8mb4", TimestampStringLocation: loc, VerifyChecksum: true, RenderJSONAsMySQLText: true,
		DumpCommandFlag: replication.BINLOG_DUMP_NON_BLOCK, DisableRetrySync: true, EventCacheCount: 2048,
		ReadTimeout: 3 * time.Minute, Logger: slog.New(&eofHandler{flag: eof}),
	}
	if s.c.SSL {
		cfg.TLSConfig = &tls.Config{ServerName: s.c.Host, InsecureSkipVerify: !s.c.VerifySSL, MinVersion: tls.VersionTLS12} //nolint:gosec // 与实例“验证证书”开关一致
	}
	syncer := replication.NewBinlogSyncer(cfg)
	type res struct {
		st  *replication.BinlogStreamer
		err error
	}
	ch := make(chan res, 1)
	go func() {
		st, err := syncer.StartSync(mysql.Position{Name: file, Pos: pos})
		ch <- res{st, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			syncer.Close()
			return nil, friendlyStreamErr(r.err, s.c.Password)
		}
		return &syncStream{syncer: syncer, streamer: r.st, idle: s.idle, secrets: []string{s.c.Password}, eof: eof}, nil
	case <-ctx.Done():
		go func() { <-ch; syncer.Close() }()
		return nil, ctx.Err()
	}
}

// Next 取下一个事件。非阻塞模式下读到末尾时服务端只发一个 EOF 包、不关连接，库不会把它交给我们，所以结束靠“一段时间没有新事件”判断：
// 服务端发过 EOF 包的话缓冲取空就是真读完（io.EOF）；没发过就是目标库卡住或网络停顿（errStalled），由调用方按结束位置判断。
func (s *syncStream) Next(ctx context.Context) (*replication.BinlogEvent, error) {
	idle := s.idle
	if s.eof.Load() {
		idle = min(idle, eofGrace)
	}
	c, cancel := context.WithTimeout(ctx, idle)
	defer cancel()
	ev, err := s.streamer.GetEvent(c)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			if s.eof.Load() {
				return nil, io.EOF
			}
			return nil, errStalled
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, friendlyStreamErr(err, s.secrets...)
	}
	return ev, nil
}

func (s *syncStream) Close() error {
	s.syncer.Close()
	return nil
}
