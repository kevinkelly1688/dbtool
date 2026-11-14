// @author: kevinkelly
// @license: Apache Licence
// @file: e2e_test.go
// @time: 2026/10/28

package binlog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/go-mysql-org/go-mysql/server"

	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// 进程内的假 MySQL 服务：讲真实的 MySQL 协议（握手、COM_QUERY、COM_REGISTER_SLAVE、COM_BINLOG_DUMP），
// 用来端到端测试 mysqlSource（database/sql 读元信息 + BinlogSyncer 拉 binlog）。不连任何真实数据库。

type fakeMySQL struct {
	t        *testing.T
	ln       net.Listener
	srv      *server.Server
	b        *binBuilder
	mu       sync.Mutex
	vars     map[string]string
	files    []LogFile
	dbs      []string
	cols     map[string][][]string // 表名 → 行（COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, COLUMN_KEY, EXTRA）
	queries  []string
	dumps    []mysql.Position
	streams  []*replication.BinlogStreamer
	failDump string // 非空时 COM_BINLOG_DUMP 返回这个错误
	registry int
}

const (
	e2eUser = "binlog_ro"
	e2ePass = "s3cret-pass-1"
)

func startFakeMySQL(t *testing.T, b *binBuilder) *fakeMySQL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeMySQL{t: t, ln: ln, b: b,
		srv:   server.NewServer("8.0.36", mysql.DEFAULT_COLLATION_ID, mysql.AUTH_NATIVE_PASSWORD, nil, nil),
		vars:  map[string]string{"version": "8.0.36", "log_bin": "ON", "binlog_format": "ROW", "binlog_row_image": "FULL", "binlog_transaction_compression": "OFF"},
		files: []LogFile{{Name: "bin.000001", Size: int64(b.pos)}, {Name: "bin.000002", Size: 200}},
		dbs:   []string{"shop", "mysql", "report"},
		cols: map[string][][]string{"orders": {
			{"id", "int", "int", "PRI", ""}, {"name", "varchar", "varchar(50)", "", ""},
			{"qty", "tinyint", "tinyint unsigned", "", ""}, {"note", "varchar", "varchar(50)", "", ""}}},
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				auth := server.NewInMemoryAuthenticationHandler(mysql.AUTH_NATIVE_PASSWORD)
				if err := auth.AddUser(e2eUser, e2ePass, mysql.AUTH_NATIVE_PASSWORD); err != nil {
					return
				}
				conn, err := f.srv.NewCustomizedConn(c, auth, &fakeHandler{f: f})
				if err != nil {
					return
				}
				for {
					if err := conn.HandleCommand(); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		f.mu.Lock()
		for _, s := range f.streams {
			s.AddErrorToStreamer(errors.New("test over")) // 让阻塞在空流上的处理协程退出
		}
		f.mu.Unlock()
	})
	return f
}

func (f *fakeMySQL) conn() engine.Conn {
	addr := f.ln.Addr().(*net.TCPAddr)
	return engine.Conn{Host: "127.0.0.1", Port: addr.Port, User: e2eUser, Password: e2ePass}
}

func (f *fakeMySQL) seen(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.queries {
		if strings.Contains(q, substr) {
			return true
		}
	}
	return false
}

type fakeHandler struct{ f *fakeMySQL }

func text(names []string, rows [][]any) (*mysql.Result, error) {
	rs, err := mysql.BuildSimpleTextResultset(names, rows)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

var colsQuery = regexp.MustCompile(`TABLE_SCHEMA = '((?:[^']|'')*)' AND TABLE_NAME = '((?:[^']|'')*)'`)

func (h *fakeHandler) HandleQuery(q string) (*mysql.Result, error) {
	f := h.f
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	uq := strings.ToUpper(strings.TrimSpace(q))
	switch {
	case strings.HasPrefix(uq, "SET "):
		return nil, nil
	case strings.HasPrefix(uq, "SHOW GLOBAL VARIABLES LIKE 'BINLOG_CHECKSUM'"):
		return text([]string{"Variable_name", "Value"}, nil)
	case strings.HasPrefix(uq, "SHOW GLOBAL VARIABLES WHERE"):
		var rows [][]any
		for k, v := range f.vars {
			rows = append(rows, []any{k, v})
		}
		return text([]string{"Variable_name", "Value"}, rows)
	case strings.Contains(uq, "TIMESTAMPDIFF"):
		return text([]string{"d"}, [][]any{{"28800"}})
	case uq == "SHOW BINARY LOGS":
		var rows [][]any
		for _, lf := range f.files {
			rows = append(rows, []any{lf.Name, lf.Size, "No"})
		}
		return text([]string{"Log_name", "File_size", "Encrypted"}, rows)
	case uq == "SHOW DATABASES":
		var rows [][]any
		for _, d := range f.dbs {
			rows = append(rows, []any{d})
		}
		return text([]string{"Database"}, rows)
	case strings.Contains(uq, "INFORMATION_SCHEMA.COLUMNS"):
		m := colsQuery.FindStringSubmatch(q)
		if m == nil {
			return nil, fmt.Errorf("unexpected columns query: %s", q)
		}
		var rows [][]any
		if strings.ReplaceAll(m[1], "''", "'") == "shop" {
			for _, c := range f.cols[m[2]] {
				rows = append(rows, []any{c[0], c[1], c[2], c[3], c[4]})
			}
		}
		return text([]string{"COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE", "COLUMN_KEY", "EXTRA"}, rows)
	}
	return nil, fmt.Errorf("fake mysql: unsupported query: %s", q)
}

func (h *fakeHandler) UseDB(string) error { return nil }
func (h *fakeHandler) HandleFieldList(string, string) ([]*mysql.Field, error) {
	return nil, errors.New("not supported")
}
func (h *fakeHandler) HandleStmtPrepare(string) (int, int, any, error) {
	return 0, 0, nil, errors.New("not supported")
}
func (h *fakeHandler) HandleStmtExecute(any, string, []any) (*mysql.Result, error) {
	return nil, errors.New("not supported")
}
func (h *fakeHandler) HandleStmtClose(any) error { return nil }
func (h *fakeHandler) HandleOtherCommand(cmd byte, _ []byte) error {
	return mysql.NewError(mysql.ER_UNKNOWN_ERROR, fmt.Sprintf("command %d not supported", cmd))
}
func (h *fakeHandler) HandleRegisterSlave([]byte) error {
	h.f.mu.Lock()
	h.f.registry++
	h.f.mu.Unlock()
	return nil
}

func (h *fakeHandler) HandleBinlogDump(pos mysql.Position) (*replication.BinlogStreamer, error) {
	f := h.f
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dumps = append(f.dumps, pos)
	if f.failDump != "" {
		return nil, mysql.NewError(1236, f.failDump)
	}
	s := replication.NewBinlogStreamer()
	add := func(raw []byte) { _ = s.AddEventToStreamer(&replication.BinlogEvent{RawData: raw}) }
	add(rotateRaw(pos.Name)) // 真实服务端先发一个 LogPos 为 0 的伪 Rotate，再发 FDE，然后是起始位置之后的事件
	add(f.b.raws[0])
	for i := 1; i < len(f.b.raws); i++ {
		if f.b.starts[i] >= pos.Pos {
			add(f.b.raws[i])
		}
	}
	f.streams = append(f.streams, s)
	return s, nil
}

func (h *fakeHandler) HandleBinlogDumpGTID(*mysql.MysqlGTIDSet) (*replication.BinlogStreamer, error) {
	return nil, errors.New("gtid dump not supported")
}

// ---- 测试 ----

func realSession(t *testing.T, f *fakeMySQL) Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := mysqlSource{idle: 400 * time.Millisecond}.Connect(ctx, f.conn())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func TestRealSessionMetadata(t *testing.T) {
	f := startFakeMySQL(t, scenario())
	sess := realSession(t, f)
	ctx := context.Background()

	info, err := sess.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "8.0.36" || info.Flavor != "mysql" || !info.LogBin || info.Format != "ROW" || info.RowImage != "FULL" || info.TZOffset != 28800 {
		t.Errorf("info: %+v", info)
	}
	if len(info.Files) != 2 || info.Files[0].Name != "bin.000001" || info.Files[1].Size != 200 {
		t.Errorf("files: %+v", info.Files)
	}
	dbs, err := sess.Databases(ctx)
	if err != nil || len(dbs) != 3 {
		t.Errorf("dbs: %v %v", dbs, err)
	}
	cols, err := sess.Columns(ctx, "shop", "orders")
	if err != nil || len(cols) != 4 {
		t.Fatalf("cols: %v %v", cols, err)
	}
	if !cols[0].PK || cols[0].DataType != "int" || !cols[2].Unsigned || cols[2].DataType != "tinyint" {
		t.Errorf("cols: %+v", cols)
	}
	none, err := sess.Columns(ctx, "shop", "gone")
	if err != nil || len(none) != 0 {
		t.Errorf("不存在的表返回空: %v %v", none, err)
	}
	// 库名里的引号被转义，不会破坏查询
	if _, err := sess.Columns(ctx, "sh'op", "or\\ders"); err != nil {
		t.Errorf("转义: %v", err)
	}
	if !f.seen(`TABLE_SCHEMA = 'sh''op' AND TABLE_NAME = 'or\\ders'`) {
		t.Errorf("库名 / 表名应作为转义后的字面量: %v", f.queries)
	}
	// 只读：从没发过写语句
	for _, q := range f.queries {
		u := strings.ToUpper(strings.TrimSpace(q))
		if strings.HasPrefix(u, "INSERT") || strings.HasPrefix(u, "UPDATE") || strings.HasPrefix(u, "DELETE") || strings.HasPrefix(u, "ALTER") || strings.HasPrefix(u, "DROP") || strings.HasPrefix(u, "PURGE") {
			t.Errorf("不应对目标库发写语句: %s", q)
		}
	}
}

func TestRealSessionBadCredentials(t *testing.T) {
	f := startFakeMySQL(t, scenario())
	c := f.conn()
	c.Password = "wrong-password"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := (mysqlSource{}).Connect(ctx, c); err == nil {
		t.Fatal("密码错误应连接失败")
	}
}

func TestRealStreamEndToEnd(t *testing.T) {
	f := startFakeMySQL(t, scenario())
	sess := realSession(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := sess.Stream(ctx, "bin.000001", 4, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	spec := baseSpec(ModeSQL)
	spec.LastFile, spec.LastSize = "bin.000001", int64(f.b.pos)
	res, err := scan(ctx, spec, st, sess, &progress{})
	if err != nil {
		t.Fatal(err)
	}
	wantSQLs(t, res,
		"INSERT INTO `shop`.`orders` (`id`,`name`,`qty`,`note`) VALUES (1,'apple',200,NULL);",
		"UPDATE `shop`.`orders` SET `name` = 'pear', `note` = 'x' WHERE `id` = 1;",
		"DELETE FROM `shop`.`orders` WHERE `id` = 2;")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.dumps) != 1 || f.dumps[0].Name != "bin.000001" || f.dumps[0].Pos != 4 || f.registry != 1 {
		t.Errorf("dump 命令: %+v registry=%d", f.dumps, f.registry)
	}
}

func TestRealStreamIdleTimeoutEndsScan(t *testing.T) {
	// 没有设置快照末尾：流在读完事件后没有新事件，空闲超时按“读完”处理
	f := startFakeMySQL(t, scenario())
	sess := realSession(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := sess.Stream(ctx, "bin.000001", 4, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	start := time.Now()
	res, err := scan(ctx, baseSpec(ModeRollback), st, sess, nil)
	if err != nil || len(stmtsOf(res)) != 3 {
		t.Fatalf("%v %v", err, sqls(res))
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("空闲超时太慢: %v", time.Since(start))
	}
	if !strings.HasPrefix(stmtsOf(res)[0].SQL, "INSERT INTO `shop`.`orders`") {
		t.Errorf("回滚: %v", sqls(res))
	}
}

func TestRealStreamFromMiddleOfFile(t *testing.T) {
	f := startFakeMySQL(t, scenario())
	sess := realSession(t, f)
	// 第二个事务 BEGIN 事件的起点
	var begin2 uint32
	seen := 0
	for i, raw := range f.b.raws {
		if raw[4] == byte(replication.QUERY_EVENT) {
			if seen++; seen == 2 {
				begin2 = f.b.starts[i]
				break
			}
		}
	}
	if begin2 == 0 {
		t.Fatal("没找到第二个 BEGIN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := sess.Stream(ctx, "bin.000001", begin2, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	res, err := scan(ctx, baseSpec(ModeSQL), st, sess, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantSQLs(t, res, "DELETE FROM `shop`.`orders` WHERE `id` = 2;")
}

func TestRealStreamStartInsideTransaction(t *testing.T) {
	f := startFakeMySQL(t, scenario())
	sess := realSession(t, f)
	// 起点落在行事件上（表映射事件已被跳过）：应报“不在事务边界”
	var rowsStart uint32
	for i, raw := range f.b.raws {
		if raw[4] == byte(replication.UPDATE_ROWS_EVENTv2) {
			rowsStart = f.b.starts[i]
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := sess.Stream(ctx, "bin.000001", rowsStart, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = scan(ctx, baseSpec(ModeSQL), st, sess, nil)
	var be *Error
	if !errors.As(err, &be) || be.Code != "bad_position" {
		t.Fatalf("应提示起始位置不在事务边界: %v", err)
	}
}

func TestRealStreamDumpError(t *testing.T) {
	f := startFakeMySQL(t, scenario())
	f.failDump = "Could not find first log file name in binary log index file"
	sess := realSession(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// COM_BINLOG_DUMP 的失败在第一次读事件时才出现（服务端返回错误包）
	firstErr := func(file string) error {
		st, err := sess.Stream(ctx, file, 4, time.UTC)
		if err != nil {
			return err
		}
		defer st.Close()
		_, err = st.Next(ctx)
		return err
	}
	err := firstErr("bin.000009")
	var be *Error
	if !errors.As(err, &be) || be.Code != "bad_position" {
		t.Fatalf("文件不存在应给出可读错误: %v", err)
	}
	f.failDump = "Access denied; you need (at least one of) the REPLICATION SLAVE privilege(s) for this operation"
	err = firstErr("bin.000001")
	if !errors.As(err, &be) || be.Code != "no_privilege" {
		t.Fatalf("缺权限应给出可读错误: %v", err)
	}
	if strings.Contains(err.Error(), e2ePass) {
		t.Error("错误信息不能带密码")
	}
}

// e2eRepo 把假仓库的实例连接指向进程内的假 MySQL。
type e2eRepo struct {
	*fakeRepo
	conn engine.Conn
}

func (r *e2eRepo) EngineConn(*model.Instance) engine.Conn { return r.conn }

func TestServiceEndToEndWithRealSource(t *testing.T) {
	f := startFakeMySQL(t, scenario())
	au := &fakeAudit{}
	svc := NewService(&e2eRepo{fakeRepo: newFakeRepo(), conn: f.conn()}, mysqlSource{idle: 400 * time.Millisecond}, au)

	info, err := svc.Info(context.Background(), root, 1)
	if err != nil || !info.CanRollback || len(info.Files) != 2 {
		t.Fatalf("%+v %v", info, err)
	}
	dbs, err := svc.Databases(context.Background(), root, 1)
	if err != nil || strings.Join(dbs, ",") != "report,shop" {
		t.Fatalf("%v %v", dbs, err)
	}

	r := req(ModeRollback)
	r.Databases = []string{"shop"}
	r.SQLTypes = []string{"update", "delete"}
	v, err := svc.Start(context.Background(), root, r)
	if err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, svc, root, v.ID)
	if done.Status != StatusDone || done.Matched != 2 {
		t.Fatalf("%+v", done)
	}
	page, _ := svc.Rows(root, v.ID, 0, 0)
	if page.Rows[0].SQL != "INSERT INTO `shop`.`orders` (`id`,`name`,`qty`,`note`) VALUES (2,'bob''s \\\\ cat',5,'n');" ||
		page.Rows[1].SQL != "UPDATE `shop`.`orders` SET `name` = 'apple', `note` = NULL WHERE `id` = 1;" {
		t.Errorf("回滚结果: %+v", page.Rows)
	}
	// 快照末尾来自 SHOW BINARY LOGS：第一个文件读完后（Rotate 到 bin.000002）就停，不会继续读
	if done.File != "bin.000001" {
		t.Errorf("应停在快照末尾文件: %+v", done)
	}
	if acts := au.actions(); len(acts) != 1 || acts[0] != "Binlog解析-回滚" {
		t.Errorf("审计: %v", acts)
	}
	for _, q := range f.queries {
		if strings.Contains(strings.ToUpper(q), "PURGE") {
			t.Error("不应发 PURGE")
		}
	}
}

func TestServiceSourceDownMapsError(t *testing.T) {
	f := startFakeMySQL(t, scenario())
	conn := f.conn()
	_ = f.ln.Close() // 实例不可达
	svc := NewService(&e2eRepo{fakeRepo: newFakeRepo(), conn: conn}, mysqlSource{}, nil)
	_, err := svc.Info(context.Background(), root, 1)
	var be *Error
	if !errors.As(err, &be) || be.Code != "connect_failed" {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), e2ePass) {
		t.Error("不能带密码")
	}
}

var _ = io.EOF
