// @author: kevinkelly
// @license: Apache Licence
// @file: server.go
// @time: 2026/10/28

// Package pgtest 是进程内的假 PostgreSQL 服务端（pgproto3），供需要真实 postgres 引擎、又不能连真实库的测试使用（internal/query、internal/dict 等）。
// 内容取自 internal/engine/postgres/fake_test.go（只导出，未改行为）：支持启动、扩展 / 简单查询协议、READ ONLY 事务语义、DECLARE CURSOR / FETCH、
// CancelRequest、statement_timeout 模拟；查询结果由测试给出的 Handler 提供。
package pgtest

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"dbtool/internal/engine"
)

// 进程内假 PostgreSQL 服务端（pgproto3）：支持启动（SSLRequest 拒绝、cleartext 密码认证）、扩展查询协议、简单查询协议、
// 事务状态与 READ ONLY 语义、DECLARE CURSOR / FETCH、CancelRequest、statement_timeout 模拟。查询结果由测试里的 handler 提供。

type Col struct {
	Name string
	OID  uint32
}

type Result struct {
	Cols  []Col
	Rows  [][]*string
	Tag   string
	Err   *pgproto3.ErrorResponse
	Delay time.Duration // 响应前先“执行”这么久（可被 CancelRequest / statement_timeout 打断）
	// Origins 与 Cols 一一对应：该列的来源表 OID 与列号（0 表示表达式列）。只出现在语句本身的结果描述里；
	// FETCH 游标的结果描述与真实服务端一样没有这两项。
	Origins []Origin
}

// Origin 是结果列的来源表 OID 与列号。
type Origin struct {
	Table uint32
	Att   uint16
}

func Str(s string) *string { return &s }

func Cell(vals ...string) []*string {
	out := make([]*string, len(vals))
	for i := range vals {
		if vals[i] != "\x00NULL" {
			out[i] = Str(vals[i])
		}
	}
	return out
}

const NullCell = "\x00NULL"

func Err(code, msg string) *Result {
	return &Result{Err: &pgproto3.ErrorResponse{Severity: "ERROR", Code: code, Message: msg}}
}

// handler 返回 nil 表示“未知语句”，服务端回一个空结果。
type Handler func(sql string, params []string) *Result

type Server struct {
	t        testing.TB
	ln       net.Listener
	password string // 非空则要求 cleartext 密码
	scs      string // standard_conforming_strings 的回报值，缺省 on
	encoding string // client_encoding 的回报值，缺省 UTF8
	handler  Handler

	mu       sync.Mutex
	startups []map[string]string
	stmts    []string // 收到的所有语句（扩展协议的 Parse + 简单查询里按分号拆开的每一条）
	ssl      int
	cancels  atomic.Int32
	nextPID  uint32
	live     map[uint32]*fakeConn
	conns    atomic.Int32 // 累计连接数
	open     atomic.Int32 // 当前打开的连接数
	wg       sync.WaitGroup
}

func New(t testing.TB, h Handler) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{t: t, ln: ln, handler: h, scs: "on", encoding: "UTF8", live: map[uint32]*fakeConn{}}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

func (s *Server) Close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for _, c := range s.live {
		_ = c.nc.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) Addr() (string, int) {
	a := s.ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

func (s *Server) Conn(user, password, db string) engine.Conn {
	h, p := s.Addr()
	return engine.Conn{Host: h, Port: p, User: user, Password: password, DBName: db}
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(nc)
		}()
	}
}

func (s *Server) Statements() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.stmts...)
}

func (s *Server) StartupParams() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]string(nil), s.startups...)
}

// has 判断是否收到过包含 sub 的语句。
func (s *Server) Has(sub string) bool {
	for _, st := range s.Statements() {
		if strings.Contains(st, sub) {
			return true
		}
	}
	return false
}

type fakeConn struct {
	s        *Server
	nc       net.Conn
	be       *pgproto3.Backend
	pid      uint32
	secret   uint32
	user, db string
	inTx     bool
	readOnly bool
	failed   bool // 事务内出错
	timeout  time.Duration
	cursors  map[string]*fakeCursor
	cancelCh chan struct{}
}

type fakeCursor struct {
	res *Result
	pos int
}

func (c *fakeConn) txStatus() byte {
	switch {
	case c.failed:
		return 'E'
	case c.inTx:
		return 'T'
	}
	return 'I'
}

func (s *Server) handleConn(nc net.Conn) {
	defer nc.Close()
	be := pgproto3.NewBackend(nc, nc)
	c := &fakeConn{s: s, nc: nc, be: be, cursors: map[string]*fakeCursor{}, cancelCh: make(chan struct{}, 1)}
	for {
		msg, err := be.ReceiveStartupMessage()
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
			s.mu.Lock()
			s.ssl++
			s.mu.Unlock()
			if _, err := nc.Write([]byte("N")); err != nil {
				return
			}
			continue
		case *pgproto3.CancelRequest:
			s.mu.Lock()
			target := s.live[m.ProcessID]
			s.mu.Unlock()
			if target != nil && target.secret == m.SecretKey {
				s.cancels.Add(1)
				select {
				case target.cancelCh <- struct{}{}:
				default:
				}
			}
			return
		case *pgproto3.StartupMessage:
			params := map[string]string{}
			for k, v := range m.Parameters {
				params[k] = v
			}
			c.user, c.db = params["user"], params["database"]
			s.conns.Add(1)
			s.open.Add(1)
			defer s.open.Add(-1)
			s.mu.Lock()
			s.startups = append(s.startups, params)
			s.nextPID++
			c.pid, c.secret = 1000+s.nextPID, 777+s.nextPID
			s.live[c.pid] = c
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				delete(s.live, c.pid)
				s.mu.Unlock()
			}()
			if !c.authenticate(params) {
				return
			}
			c.run()
			return
		}
	}
}

func (c *fakeConn) authenticate(params map[string]string) bool {
	be := c.be
	if c.s.password != "" {
		_ = be.SetAuthType(pgproto3.AuthTypeCleartextPassword)
		be.Send(&pgproto3.AuthenticationCleartextPassword{})
		if be.Flush() != nil {
			return false
		}
		msg, err := be.Receive()
		if err != nil {
			return false
		}
		pw, ok := msg.(*pgproto3.PasswordMessage)
		if !ok || pw.Password != c.s.password {
			be.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: "28P01", Message: fmt.Sprintf("password authentication failed for user %q", c.user)})
			_ = be.Flush()
			return false
		}
	}
	be.Send(&pgproto3.AuthenticationOk{})
	for k, v := range map[string]string{"server_version": "16.2", "server_encoding": "UTF8", "client_encoding": c.s.encoding, "DateStyle": "ISO, MDY",
		"integer_datetimes": "on", "standard_conforming_strings": c.s.scs, "TimeZone": "Asia/Shanghai"} {
		be.Send(&pgproto3.ParameterStatus{Name: k, Value: v})
	}
	be.Send(&pgproto3.BackendKeyData{ProcessID: c.pid, SecretKey: c.secret})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	return be.Flush() == nil
}

func (c *fakeConn) record(sql string) {
	c.s.mu.Lock()
	c.s.stmts = append(c.s.stmts, sql)
	c.s.mu.Unlock()
}

var (
	reDeclare = regexp.MustCompile(`(?is)^DECLARE\s+(\w+)\s+NO\s+SCROLL\s+CURSOR\s+FOR\s+(.*)$`)
	reFetch   = regexp.MustCompile(`(?is)^FETCH\s+FORWARD\s+(\d+)\s+FROM\s+(\w+)$`)
	reTimeout = regexp.MustCompile(`(?is)^SET\s+LOCAL\s+statement_timeout\s*=\s*(\d+)$`)
	reWrite   = regexp.MustCompile(`(?is)^(INSERT|UPDATE|DELETE|CREATE|DROP|ALTER|TRUNCATE)\b`)
)

// exec 执行一条语句，返回结果（Err 非空表示出错）。
func (c *fakeConn) exec(sql string, params []string) *Result {
	sql = strings.TrimSpace(sql)
	up := strings.ToUpper(sql)
	c.record(sql)
	if c.failed && !strings.HasPrefix(up, "ROLLBACK") {
		return Err("25P02", "current transaction is aborted, commands ignored until end of transaction block")
	}
	switch {
	case up == "BEGIN READ ONLY":
		c.inTx, c.readOnly = true, true
		return &Result{Tag: "BEGIN"}
	case up == "BEGIN":
		c.inTx = true
		return &Result{Tag: "BEGIN"}
	case up == "ROLLBACK":
		c.inTx, c.readOnly, c.failed = false, false, false
		c.cursors = map[string]*fakeCursor{}
		c.timeout = 0
		return &Result{Tag: "ROLLBACK"}
	case up == "COMMIT":
		c.inTx, c.readOnly, c.failed = false, false, false
		return &Result{Tag: "COMMIT"}
	}
	if m := reTimeout.FindStringSubmatch(sql); m != nil {
		ms, _ := strconv.Atoi(m[1])
		c.timeout = time.Duration(ms) * time.Millisecond
		return &Result{Tag: "SET"}
	}
	if strings.HasPrefix(up, "SET LOCAL ") {
		return &Result{Tag: "SET"}
	}
	if m := reDeclare.FindStringSubmatch(sql); m != nil {
		if !c.inTx {
			return Err("25P01", "DECLARE CURSOR can only be used in transaction blocks")
		}
		res := c.eval(m[2], params)
		if res.Err != nil {
			return res
		}
		c.cursors[m[1]] = &fakeCursor{res: res}
		return &Result{Tag: "DECLARE CURSOR"}
	}
	if m := reFetch.FindStringSubmatch(sql); m != nil {
		cur := c.cursors[m[2]]
		if cur == nil {
			return Err("34000", fmt.Sprintf("cursor %q does not exist", m[2]))
		}
		n, _ := strconv.Atoi(m[1])
		end := min(cur.pos+n, len(cur.res.Rows))
		out := &Result{Cols: cur.res.Cols, Rows: cur.res.Rows[cur.pos:end]}
		out.Tag = "FETCH " + strconv.Itoa(len(out.Rows))
		cur.pos = end
		return out
	}
	return c.eval(sql, params)
}

// eval 执行普通语句：只读事务里拒绝写操作，其余交给 handler；模拟 statement_timeout 与取消。
func (c *fakeConn) eval(sql string, params []string) *Result {
	if c.readOnly && reWrite.MatchString(strings.TrimSpace(sql)) {
		return Err("25006", "cannot execute "+strings.ToUpper(strings.Fields(sql)[0])+" in a read-only transaction")
	}
	var res *Result
	if c.s.handler != nil {
		res = c.s.handler(sql, params)
	}
	if res == nil {
		return &Result{Tag: "SELECT 0"}
	}
	if res.Delay > 0 {
		wait := res.Delay
		timedOut := false
		if c.timeout > 0 && c.timeout < wait {
			wait, timedOut = c.timeout, true
		}
		select {
		case <-time.After(wait):
			if timedOut {
				return Err("57014", "canceling statement due to statement timeout")
			}
		case <-c.cancelCh:
			return Err("57014", "canceling statement due to user request")
		}
	}
	return res
}

func (c *fakeConn) sendResult(res *Result, withDesc bool) {
	be := c.be
	if res.Err != nil {
		be.Send(res.Err)
		return
	}
	if withDesc && len(res.Cols) > 0 {
		be.Send(rowDesc(res))
	}
	for _, r := range res.Rows {
		vals := make([][]byte, len(r))
		for i, v := range r {
			if v != nil {
				vals[i] = []byte(*v)
			}
		}
		be.Send(&pgproto3.DataRow{Values: vals})
	}
	tag := res.Tag
	if tag == "" {
		tag = "SELECT " + strconv.Itoa(len(res.Rows))
	}
	be.Send(&pgproto3.CommandComplete{CommandTag: []byte(tag)})
}

func rowDesc(res *Result) *pgproto3.RowDescription {
	rd := &pgproto3.RowDescription{}
	for i, col := range res.Cols {
		fd := pgproto3.FieldDescription{Name: []byte(col.Name), DataTypeOID: col.OID, DataTypeSize: -1, TypeModifier: -1}
		if i < len(res.Origins) {
			fd.TableOID, fd.TableAttributeNumber = res.Origins[i].Table, res.Origins[i].Att
		}
		rd.Fields = append(rd.Fields, fd)
	}
	return rd
}

func (c *fakeConn) run() {
	be := c.be
	stmts := map[string]string{}
	type portal struct {
		sql    string
		params []string
		res    *Result
	}
	var cur *portal
	skipToSync := false
	for {
		msg, err := be.Receive()
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case *pgproto3.Terminate:
			return
		case *pgproto3.Sync:
			skipToSync = false
			be.Send(&pgproto3.ReadyForQuery{TxStatus: c.txStatus()})
		case *pgproto3.Flush:
		case *pgproto3.Query:
			for _, st := range strings.Split(m.String, ";") {
				if strings.TrimSpace(st) == "" {
					continue
				}
				res := c.exec(st, nil)
				if res.Err != nil {
					if c.inTx {
						c.failed = true
					}
					be.Send(res.Err)
					break
				}
				c.sendResult(res, true)
			}
			be.Send(&pgproto3.ReadyForQuery{TxStatus: c.txStatus()})
		case *pgproto3.Parse:
			if skipToSync {
				continue
			}
			stmts[m.Name] = m.Query
			be.Send(&pgproto3.ParseComplete{})
		case *pgproto3.Bind:
			if skipToSync {
				continue
			}
			p := &portal{sql: stmts[m.PreparedStatement]}
			for _, v := range m.Parameters {
				p.params = append(p.params, string(v))
			}
			cur = p
			be.Send(&pgproto3.BindComplete{})
		case *pgproto3.Describe:
			if skipToSync {
				continue
			}
			if m.ObjectType == 'S' { // 描述预备语句：不执行，只回参数与结果列（含来源表 OID / 列号）
				var dres *Result
				if c.s.handler != nil {
					dres = c.s.handler(strings.TrimSpace(stmts[m.Name]), nil)
				}
				be.Send(&pgproto3.ParameterDescription{})
				switch {
				case dres != nil && dres.Err != nil:
					be.Send(dres.Err)
					skipToSync = true
				case dres != nil && len(dres.Cols) > 0:
					be.Send(rowDesc(dres))
				default:
					be.Send(&pgproto3.NoData{})
				}
				break
			}
			if cur == nil {
				continue
			}
			cur.res = c.exec(cur.sql, cur.params)
			if cur.res.Err != nil {
				if c.inTx {
					c.failed = true
				}
				be.Send(cur.res.Err)
				skipToSync = true
				continue
			}
			if len(cur.res.Cols) > 0 {
				be.Send(rowDesc(cur.res))
			} else {
				be.Send(&pgproto3.NoData{})
			}
		case *pgproto3.Execute:
			if skipToSync || cur == nil || cur.res == nil {
				continue
			}
			c.sendResult(cur.res, false)
		default:
			// 其它消息（Close 等）忽略
		}
		if err := be.Flush(); err != nil {
			return
		}
	}
}

// ---- 常用 handler 构件 ----

// textCols 生成全是 text 列的列描述。
func TextCols(names ...string) []Col {
	out := make([]Col, len(names))
	for i, n := range names {
		out[i] = Col{n, 25}
	}
	return out
}
