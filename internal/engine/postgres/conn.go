// @author: kevinkelly
// @license: Apache Licence
// @file: conn.go
// @time: 2026/10/28

package postgres

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// maxMessageBytes 是服务端单条消息（一行结果）的大小上限。超过则中止并关闭连接，避免 repeat('x', 1e9) 这类语句撑爆内存。
const maxMessageBytes = 32 << 20

// ---- 建连 ----

// runtimeParams 是连接的启动参数（在启动包里发给服务端，优先级高于角色 / 库级的默认值，用户的查询又无法 SET 它们）。
func runtimeParams() map[string]string {
	return map[string]string{
		"application_name":                    "dbtool",
		"client_encoding":                     "UTF8", // 固定成 UTF8：词法检查与服务端切词一致，不存在“多字节字符里藏着引号”的编码问题
		"standard_conforming_strings":         "on",   // 普通 '…' 里反斜杠不是转义，与 lex.go 一致
		"default_transaction_read_only":       "on",
		"statement_timeout":                   strconv.Itoa(int(metaTimeout / time.Millisecond)),
		"lock_timeout":                        "5000",
		"idle_in_transaction_session_timeout": "60000",
		"DateStyle":                           "ISO, MDY",
		"IntervalStyle":                       "postgres",
		"extra_float_digits":                  "3",
		"bytea_output":                        "hex",
	}
}

// setBuilder 给 pgconn.Config.BuildContextWatcherHandler 赋值。该字段的返回类型在 pgx 的 internal 包里，外部无法写出类型名，
// 所以借助泛型从字段本身推断类型。
func setBuilder[H any](dst *func(*pgconn.PgConn) H, mk func(*pgconn.PgConn) any) {
	*dst = func(pc *pgconn.PgConn) H { return mk(pc).(H) }
}

// dial 建立一条到 db 的新连接。凭据只放在 Config 字段里，不拼进连接串，任何错误信息都不含密码。
func (e *Engine) dial(ctx context.Context, db string) (*pgconn.PgConn, error) {
	c := e.conn
	// 用固定的基础串，只是为了得到合法的 Config（pgconn 要求必须由 ParseConfig 创建）；随后覆盖所有字段，环境变量（PGOPTIONS、PGTZ …）带来的运行参数一并丢弃。
	cfg, err := pgconn.ParseConfig("sslmode=disable")
	if err != nil {
		return nil, errors.New("PostgreSQL 连接配置不合法")
	}
	cfg.Host = c.Host
	cfg.Port = uint16(c.Port)
	cfg.User = c.User
	cfg.Password = c.Password
	cfg.Database = db
	cfg.ConnectTimeout = connectTimeout
	cfg.RuntimeParams = runtimeParams()
	cfg.ValidateConnect = nil
	cfg.Fallbacks = nil
	cfg.TLSConfig = nil
	if !strings.HasPrefix(c.Host, "/") {
		tlsCfg := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: !c.VerifySSL} //nolint:gosec // 与其它引擎一致：由实例上的“校验证书”开关决定
		if c.SSL {
			cfg.TLSConfig = tlsCfg
		} else {
			// 未勾选 SSL：优先尝试 TLS（不校验证书），服务端不支持再回落明文，等同 sslmode=prefer
			tlsCfg.InsecureSkipVerify = true
			cfg.TLSConfig = tlsCfg
			cfg.Fallbacks = []*pgconn.FallbackConfig{{Host: c.Host, Port: cfg.Port, TLSConfig: nil}}
		}
	}
	cfg.BuildFrontend = func(r io.Reader, w io.Writer) *pgproto3.Frontend {
		f := pgproto3.NewFrontend(r, w)
		f.SetMaxBodyLen(maxMessageBytes)
		return f
	}
	// 取消 ctx 时向服务端发送 CancelRequest（否则服务端会把已被放弃的查询跑完），并设一个兜底的连接读写截止时间
	setBuilder(&cfg.BuildContextWatcherHandler, func(pc *pgconn.PgConn) any {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: pc, CancelRequestDelay: 0, DeadlineDelay: 3 * time.Second}
	})

	pc, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败：%w", mapError(err, c.Password))
	}
	if v := pc.ParameterStatus("standard_conforming_strings"); v != "on" {
		closeConn(pc)
		return nil, errors.New("服务端没有接受 standard_conforming_strings=on（经过了不支持启动参数的连接代理？），为保证查询检查的正确性已拒绝连接")
	}
	if v := pc.ParameterStatus("client_encoding"); !strings.EqualFold(v, "UTF8") {
		closeConn(pc)
		return nil, errors.New("服务端没有接受 client_encoding=UTF8，已拒绝连接")
	}
	return pc, nil
}

// ---- 连接池 ----

type idleConn struct {
	pc *pgconn.PgConn
	at time.Time
}

type pool struct {
	db   string
	sem  chan struct{} // 容量 = 每库最大连接数；持有一个槽位 = 占用一条连接
	mu   sync.Mutex
	idle []idleConn
	last time.Time
}

func (p *pool) popIdle() *pgconn.PgConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.idle) > 0 {
		ic := p.idle[len(p.idle)-1]
		p.idle = p.idle[:len(p.idle)-1]
		if time.Since(ic.at) > idleConnTTL || ic.pc.IsClosed() {
			go closeConn(ic.pc)
			continue
		}
		return ic.pc
	}
	return nil
}

func (p *pool) pushIdle(pc *pgconn.PgConn) {
	p.mu.Lock()
	p.idle = append(p.idle, idleConn{pc: pc, at: time.Now()})
	p.last = time.Now()
	p.mu.Unlock()
}

func (p *pool) closeIdle() {
	p.mu.Lock()
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, ic := range idle {
		closeConn(ic.pc)
	}
}

func (p *pool) reap(now time.Time) {
	p.mu.Lock()
	keep := p.idle[:0]
	var drop []*pgconn.PgConn
	for _, ic := range p.idle {
		if now.Sub(ic.at) > idleConnTTL || ic.pc.IsClosed() {
			drop = append(drop, ic.pc)
		} else {
			keep = append(keep, ic)
		}
	}
	p.idle = keep
	p.mu.Unlock()
	for _, pc := range drop {
		closeConn(pc)
	}
}

// poolFor 取（必要时创建）db 的连接池；超过缓存的库数量上限时淘汰最久没用且没有在用连接的池。
func (e *Engine) poolFor(db string) (*pool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errors.New("PostgreSQL 引擎已关闭")
	}
	if p, ok := e.pools[db]; ok {
		p.last = time.Now()
		return p, nil
	}
	if len(e.pools) >= maxDBPools {
		var victim *pool
		for _, p := range e.pools {
			if len(p.sem) == 0 && (victim == nil || p.last.Before(victim.last)) {
				victim = p
			}
		}
		if victim == nil {
			return nil, uerr("同时在访问的数据库太多（最多 %d 个），请稍后重试", maxDBPools)
		}
		delete(e.pools, victim.db)
		go victim.closeIdle()
	}
	p := &pool{db: db, sem: make(chan struct{}, maxConnsPerDB), last: time.Now()}
	e.pools[db] = p
	e.janitors.Do(func() { go e.janitor() })
	return p, nil
}

// janitor 定期回收各库的过期空闲连接，Close 时退出。
func (e *Engine) janitor() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case now := <-t.C:
			e.mu.Lock()
			ps := make([]*pool, 0, len(e.pools))
			for _, p := range e.pools {
				ps = append(ps, p)
			}
			e.mu.Unlock()
			for _, p := range ps {
				p.reap(now)
			}
		}
	}
}

// ---- 会话 ----

// session 是借出的一条连接。
type session struct {
	e      *Engine
	p      *pool
	pc     *pgconn.PgConn
	reused bool // 来自空闲池（可能已被服务端断开）
	broken bool // 出错后状态不确定，归还时直接关闭
}

// open 借一条到 db 的连接（等待槽位时受 ctx 控制）。
func (e *Engine) open(ctx context.Context, db string) (*session, error) {
	p, err := e.poolFor(db)
	if err != nil {
		return nil, err
	}
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, mapError(ctx.Err(), e.conn.Password)
	}
	if pc := p.popIdle(); pc != nil {
		return &session{e: e, p: p, pc: pc, reused: true}, nil
	}
	pc, err := e.dial(ctx, db)
	if err != nil {
		<-p.sem
		return nil, err
	}
	return &session{e: e, p: p, pc: pc}, nil
}

// release 归还连接。
func (s *session) release() {
	defer func() { <-s.p.sem }()
	s.e.mu.Lock()
	closed := s.e.closed || s.e.pools[s.p.db] != s.p // 引擎已关闭，或这个池已被淘汰
	s.e.mu.Unlock()
	if closed || s.broken || s.pc.IsClosed() || s.pc.TxStatus() != 'I' {
		closeConn(s.pc)
		return
	}
	s.p.pushIdle(s.pc)
}

// fail 在出现非服务端 SQL 错误（网络、超时、取消）时标记连接作废。
func (s *session) fail(err error) {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		s.broken = true
	}
}

// exec 用扩展查询协议执行一条语句（params 为文本参数）。扩展协议下服务端保证一次只执行一条语句。
func (s *session) exec(ctx context.Context, sql string, params ...string) (*pgconn.Result, error) {
	var pv [][]byte
	if len(params) > 0 {
		pv = make([][]byte, len(params))
		for i, p := range params {
			pv[i] = []byte(p)
		}
	}
	res := s.pc.ExecParams(ctx, sql, pv, nil, nil, nil).Read()
	if res.Err != nil {
		s.fail(res.Err)
		if ctx.Err() != nil {
			s.broken = true
		}
		return nil, s.e.wrapErr(ctx, res.Err)
	}
	return res, nil
}

// stream 用扩展查询协议执行一条语句，逐行回调 fn（返回 false 表示不再需要后续行：继续读完但丢弃，不占内存）。
// 回调拿到的 row 只在回调期间有效。返回列描述的拷贝。
func (s *session) stream(ctx context.Context, sql string, fn func(fds []pgconn.FieldDescription, row [][]byte) bool) ([]pgconn.FieldDescription, error) {
	rr := s.pc.ExecParams(ctx, sql, nil, nil, nil, nil)
	var fds []pgconn.FieldDescription
	keep := true
	for rr.NextRow() {
		if fds == nil {
			fds = append([]pgconn.FieldDescription(nil), rr.FieldDescriptions()...)
		}
		if keep {
			keep = fn(fds, rr.Values())
		}
	}
	if fds == nil {
		fds = append([]pgconn.FieldDescription(nil), rr.FieldDescriptions()...)
	}
	if _, err := rr.Close(); err != nil {
		s.fail(err)
		if ctx.Err() != nil {
			s.broken = true
		}
		return nil, s.e.wrapErr(ctx, err)
	}
	return fds, nil
}

// wrapErr 把 ctx 的取消 / 超时状态并入错误映射。
func (e *Engine) wrapErr(ctx context.Context, err error) error {
	if ce := ctx.Err(); ce != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "57014" || !errors.As(err, &pe) {
			return mapError(ce, e.conn.Password)
		}
	}
	return mapError(err, e.conn.Password)
}

// rows 执行并把结果转成字符串矩阵（NULL 为 nil）。
func (s *session) rows(ctx context.Context, sql string, params ...string) ([][]*string, error) {
	res, err := s.exec(ctx, sql, params...)
	if err != nil {
		return nil, err
	}
	out := make([][]*string, len(res.Rows))
	for i, r := range res.Rows {
		row := make([]*string, len(r))
		for j, v := range r {
			if v != nil {
				sv := string(v)
				row[j] = &sv
			}
		}
		out[i] = row
	}
	return out, nil
}

// simple 用简单查询协议执行一组由代码生成的常量语句（BEGIN / SET LOCAL / ROLLBACK），一次往返。绝不用于用户输入。
func (s *session) simple(ctx context.Context, stmts ...string) error {
	_, err := s.pc.Exec(ctx, strings.Join(stmts, "; ")).ReadAll()
	if err != nil {
		s.fail(err)
		if ctx.Err() != nil {
			s.broken = true
		}
		return s.e.wrapErr(ctx, err)
	}
	return nil
}

// begin 开启只读事务并设置本事务的语句超时（与可选的额外 SET LOCAL）。
func (s *session) begin(ctx context.Context, timeout time.Duration, extra []string) error {
	stmts := []string{"BEGIN READ ONLY", "SET LOCAL statement_timeout = " + strconv.FormatInt(timeout.Milliseconds(), 10)}
	stmts = append(stmts, extra...)
	return s.simple(ctx, stmts...)
}

// rollback 结束事务。连接状态不确定时放弃该连接。
func (s *session) rollback() {
	if s.broken || s.pc.IsClosed() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.simple(ctx, "ROLLBACK"); err != nil {
		s.broken = true
	}
}

// isStale 判断开启事务失败是否因为复用的空闲连接已被服务端断开。
func isStale(err error) bool {
	if err == nil {
		return false
	}
	var ue *UserError
	if errors.As(err, &ue) || IsTimeout(err) || errors.Is(err, context.Canceled) {
		return false
	}
	return true
}

// readTx 借一条连接，在只读事务里执行 fn，最后 ROLLBACK 并归还。
// 复用的空闲连接如果在开启事务时就失败（多半是被服务端断开了），换新连接重试一次。
func (e *Engine) readTx(ctx context.Context, db string, timeout time.Duration, extra []string, fn func(*session) error) error {
	for attempt := 0; ; attempt++ {
		s, err := e.open(ctx, db)
		if err != nil {
			return err
		}
		if err := s.begin(ctx, timeout, extra); err != nil {
			stale := s.reused && attempt == 0 && isStale(err)
			s.broken = true
			s.release()
			if stale {
				continue
			}
			return err
		}
		ferr := fn(s)
		s.rollback()
		s.release()
		return ferr
	}
}
