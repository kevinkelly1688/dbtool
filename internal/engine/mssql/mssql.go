// @author: kevinkelly
// @license: Apache Licence
// @file: mssql.go
// @time: 2026/10/28

// Package mssql 是 SQL Server 引擎的 Go 实现。
//
// 目前覆盖 SQL 工单（含批量上线）需要的能力：连接、库/表/列清单、服务端语法检查（SET PARSEONLY ON）、
// 按“执行单元”逐个执行（每个单元一个事务，失败回滚并停止）。在线查询尚未实现，Query 返回明确的错误。
package mssql

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/golang-sql/sqlexp"
	mssqldb "github.com/microsoft/go-mssqldb"

	"dbtool/internal/engine"
)

func init() {
	engine.Register("mssql", func(c engine.Conn) (engine.Engine, error) { return New(c) })
}

type Engine struct {
	db           *sql.DB
	printAsError bool // 收到 PRINT 是否按失败处理（见 SetPrintAsError）；默认 false
}

// dsn 构造 go-mssqldb 的连接串。证书一律信任，连接超时 10 秒。
func dsn(c engine.Conn) string {
	q := url.Values{}
	if c.DBName != "" {
		q.Set("database", c.DBName)
	}
	q.Set("TrustServerCertificate", "true")
	q.Set("connection timeout", "10")
	q.Set("app name", "dbtool")
	u := url.URL{Scheme: "sqlserver", User: url.UserPassword(c.User, c.Password), Host: fmt.Sprintf("%s:%d", c.Host, c.Port), RawQuery: q.Encode()}
	return u.String()
}

func New(c engine.Conn) (*Engine, error) {
	if c.Port == 0 {
		c.Port = 1433
	}
	connector, err := mssqldb.NewConnector(dsn(c))
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &Engine{db: db}, nil
}

func (e *Engine) Name() string { return "mssql" }
func (e *Engine) Close() error { return e.db.Close() }

func (e *Engine) TestConnection(ctx context.Context) error { return e.db.PingContext(ctx) }

func (e *Engine) ServerVersion(ctx context.Context) (string, error) {
	var v string
	if err := e.db.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(64))").Scan(&v); err != nil {
		return "", err
	}
	return v, nil
}

// QuoteIdent 用方括号引用标识符。
func QuoteIdent(name string) string { return "[" + strings.ReplaceAll(name, "]", "]]") + "]" }

// AllDatabases 返回当前账号能访问的用户库（排除 master/tempdb/model/msdb 与不在线的库）。
func (e *Engine) AllDatabases(ctx context.Context) ([]string, error) {
	return e.strings(ctx, "SELECT name FROM sys.databases WHERE database_id > 4 AND state = 0 AND HAS_DBACCESS(name) = 1 ORDER BY name")
}

func (e *Engine) AllTables(ctx context.Context, db string) ([]string, error) {
	q := "SELECT s.name + '.' + t.name FROM " + QuoteIdent(db) + ".sys.tables t JOIN " + QuoteIdent(db) + ".sys.schemas s ON s.schema_id = t.schema_id ORDER BY s.name, t.name"
	return e.strings(ctx, q)
}

func (e *Engine) AllColumns(ctx context.Context, db, table string) ([]string, error) {
	schema, name := "dbo", table
	if i := strings.LastIndex(table, "."); i >= 0 {
		schema, name = table[:i], table[i+1:]
	}
	q := "SELECT c.name FROM " + QuoteIdent(db) + ".sys.columns c JOIN " + QuoteIdent(db) + ".sys.tables t ON t.object_id = c.object_id JOIN " +
		QuoteIdent(db) + ".sys.schemas s ON s.schema_id = t.schema_id WHERE s.name = @p1 AND t.name = @p2 ORDER BY c.column_id"
	rows, err := e.db.QueryContext(ctx, q, schema, name)
	if err != nil {
		return nil, err
	}
	return scanStrings(rows)
}

func (e *Engine) strings(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := e.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanStrings(rows)
}

func scanStrings(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Query 执行一条只读查询，返回第一个结果集（在线查询与数据导出共用）。
// 调用方必须已经校验过语句是只读的（query.checkMSSQL / export.Prepare）。会话级 READ UNCOMMITTED（等价于给所有表加 NOLOCK，不被业务事务阻塞、不对业务表加共享锁）；
// MaxExecMs 到时 ctx 取消，驱动会中止语句。超过 Limit 的行被丢弃（Limit<=0 不限）；
// MaxCellBytes / MaxBytes 大于 0 时按字节裁剪（在线查询用，导出不设）。
func (e *Engine) Query(ctx context.Context, q string, opt engine.QueryOptions) (*engine.ResultSet, error) {
	if opt.MaxExecMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(opt.MaxExecMs)*time.Millisecond)
		defer cancel()
	}
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if opt.DBName != "" {
		if _, err := conn.ExecContext(ctx, "USE "+QuoteIdent(opt.DBName)); err != nil {
			return nil, fmt.Errorf("切换到库 %s 失败: %s", opt.DBName, errText(err))
		}
	}
	if _, err := conn.ExecContext(ctx, "SET TRANSACTION ISOLATION LEVEL READ UNCOMMITTED"); err != nil {
		return nil, err
	}
	start := time.Now()
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return nil, errors.New(errText(err))
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil || len(cols) == 0 {
		return nil, errors.New("该批处理没有返回结果集（需要一条 SELECT 输出结果）")
	}
	ctypes, _ := rows.ColumnTypes()
	names := make([]string, len(ctypes))
	for i := range ctypes {
		names[i] = strings.ToUpper(ctypes[i].DatabaseTypeName())
	}
	rs := &engine.ResultSet{ColumnList: cols, Rows: [][]any{}}
	cp := &resultCap{cell: opt.MaxCellBytes, total: opt.MaxBytes}
	for rows.Next() {
		if opt.Limit > 0 && len(rs.Rows) >= opt.Limit {
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
			vals[i] = NormalizeValue(v, names[i])
		}
		if !cp.fit(vals, len(rs.Rows) == 0) {
			break
		}
		rs.Rows = append(rs.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New(errText(err))
	}
	rs.CellsCut, rs.ByteLimited = cp.cut, cp.limited
	rs.CostTimeMs = time.Since(start).Milliseconds()
	return rs, nil
}

// resultCap 按字节裁剪在线查询的结果：单元格超过 cell 就截断（不切在字符中间，末尾加说明），
// 累计超过 total 就不再收后面的行。第一行总是保留，保证用户至少看到结果。cell / total 为 0 表示不限制。
type resultCap struct {
	cell, total int
	used        int
	cut         int
	limited     bool
}

// fit 就地裁剪一行里超长的单元格，返回这一行能否放进总量上限（false 表示应当停止读取）。
func (c *resultCap) fit(vals []any, first bool) bool {
	size := 0
	for i, v := range vals {
		if s, ok := v.(string); ok && c.cell > 0 && len(s) > c.cell {
			vals[i] = truncateUTF8(s, c.cell) + fmt.Sprintf("…（已截断，共 %d 字节）", len(s)) // i18n-ignore: 拼进结果单元格的值，属于数据输出
			c.cut++
		}
		switch x := vals[i].(type) {
		case string:
			size += len(x)
		case []byte:
			size += len(x)
		default:
			size += 8
		}
	}
	if c.total > 0 && !first && c.used+size > c.total {
		c.limited = true
		return false
	}
	c.used += size
	return true
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// NormalizeValue 把驱动返回的值转成适合写文件/JSON 的形式。typeName 是 DatabaseTypeName 的大写形式。
func NormalizeValue(v any, typeName string) any {
	switch x := v.(type) {
	case []byte:
		switch typeName {
		case "UNIQUEIDENTIFIER":
			var u mssqldb.UniqueIdentifier
			if err := u.Scan(x); err == nil {
				return u.String()
			}
			return hex.EncodeToString(x)
		case "VARBINARY", "BINARY", "IMAGE", "TIMESTAMP", "ROWVERSION":
			return "0x" + strings.ToUpper(hex.EncodeToString(x))
		}
		return string(x) // DECIMAL / MONEY / XML / 字符类型
	case time.Time:
		switch typeName {
		case "DATE":
			return x.Format("2006-01-02")
		case "TIME":
			return x.Format("15:04:05.9999999")
		case "DATETIMEOFFSET":
			return x.Format("2006-01-02 15:04:05.9999999 -07:00")
		}
		return x.Format("2006-01-02 15:04:05.999999")
	}
	return v
}

// ReadOnly SQL Server 没有实例级 read_only 开关；库是否只读在 ExecEach 里按目标库检查。
func (e *Engine) ReadOnly(context.Context) (bool, error) { return false, nil }

// SyntaxCheck 用 SET PARSEONLY ON 让服务端只解析不执行。
// 只能发现语法错误，不会校验对象是否存在（SQL Server 的延迟名称解析）。
func (e *Engine) SyntaxCheck(ctx context.Context, db string, units []string) ([]string, error) {
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if db != "" {
		if _, err := conn.ExecContext(ctx, "USE "+QuoteIdent(db)); err != nil {
			return nil, fmt.Errorf("切换到库 %s 失败: %w", db, err)
		}
	}
	// 无论如何都要把 PARSEONLY 关掉，避免这条连接回到连接池后污染别的请求
	defer func() { _, _ = conn.ExecContext(context.Background(), "SET PARSEONLY OFF") }()
	out := make([]string, len(units))
	for i, u := range units {
		if _, err := conn.ExecContext(ctx, "SET PARSEONLY ON"); err != nil {
			return nil, err
		}
		_, err := conn.ExecContext(ctx, u)
		if _, e2 := conn.ExecContext(ctx, "SET PARSEONLY OFF"); e2 != nil {
			return nil, e2
		}
		if err != nil {
			out[i] = errText(err)
		}
	}
	return out, nil
}

// errText 取 SQL Server 错误的原始消息（去掉驱动加的 "mssql: " 前缀和错误号）。
func errText(err error) string {
	var me mssqldb.Error
	if errors.As(err, &me) {
		if len(me.All) > 0 {
			parts := make([]string, 0, len(me.All))
			for _, e := range me.All {
				parts = append(parts, e.Message)
			}
			return strings.Join(parts, "\n")
		}
		return me.Message
	}
	return err.Error()
}

// ---- 执行 ----

// Message 是服务端回传的一条消息。Number 为 0 表示 PRINT；RAISERROR 严重度≤10 时是 50000 或指定的消息号。
type Message struct {
	Number int32
	Text   string
}

// UnitOutcome 是一个执行单元的结果。
type UnitOutcome struct {
	Affected   int64 // DML 影响行数；没有时为 -1
	ResultSets int   // 返回的结果集个数
	FetchRows  int64 // 结果集里的总行数（只计数，不保留数据）
	Messages   []Message
}

// UnitRunner 执行一个单元。真实实现在 sessionRunner；测试里用假实现验证 ExecEach 的编排。
type UnitRunner interface {
	// Run 在一个事务里执行 unit；出错时已回滚，并把已收到的消息一并返回。
	Run(ctx context.Context, unit string) (UnitOutcome, error)
}

// ExecEach 先切到目标库并确认可写，再逐个单元执行，遇到第一个失败即停，其后的单元标记 Skipped。
// 每个单元成功后立即提交。PRINT（消息号 0）默认只是信息，显示在该语句的“信息”里；
// “收到 PRINT 就按失败处理并回滚该单元”为可选（SetPrintAsError，系统配置 review.mssql_print_as_error）。
// SetPrintAsError 设置收到 PRINT 时的处理：true = 按失败处理并回滚该单元、其后的单元不执行，
// 常见于脚本里用 PRINT 提示“不要执行”；false（默认）= PRINT 只是信息，显示在该语句的“信息”里，语句照常提交。
func (e *Engine) SetPrintAsError(on bool) { e.printAsError = on }

func (e *Engine) ExecEach(ctx context.Context, db string, units []string) ([]engine.ExecStmtResult, error) {
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if db != "" {
		if _, err := conn.ExecContext(ctx, "USE "+QuoteIdent(db)); err != nil {
			return nil, fmt.Errorf("切换到库 %s 失败: %s", db, errText(err))
		}
	}
	var upd string
	if err := conn.QueryRowContext(ctx, "SELECT CAST(DATABASEPROPERTYEX(DB_NAME(), 'Updateability') AS varchar(20))").Scan(&upd); err == nil && strings.EqualFold(upd, "READ_ONLY") {
		return nil, fmt.Errorf("库 %s 是只读的，禁止执行变更语句", db)
	}
	sr := &sessionRunner{conn: conn, printFails: e.printAsError}
	res := ExecUnitsWith(ctx, sr, units, e.printAsError)
	sr.finish(ctx, res)
	return res, nil
}

// ExecUnits 是与连接无关的编排逻辑，便于测试。PRINT 只是信息（见 ExecUnitsWith）。
func ExecUnits(ctx context.Context, r UnitRunner, units []string) []engine.ExecStmtResult {
	return ExecUnitsWith(ctx, r, units, false)
}

// ExecUnitsWith 同 ExecUnits，printFails 为 true 时收到 PRINT 按失败处理。
func ExecUnitsWith(ctx context.Context, r UnitRunner, units []string, printFails bool) []engine.ExecStmtResult {
	out := make([]engine.ExecStmtResult, len(units))
	failed := false
	for i, u := range units {
		out[i].SQL = u
		if failed {
			out[i].Skipped = true
			continue
		}
		start := time.Now()
		oc, err := r.Run(ctx, u)
		out[i].ElapsedMs = float64(time.Since(start).Microseconds()) / 1000
		var notes []string
		if oc.ResultSets > 0 {
			// 核对查询只报返回行数，不回传数据；单元里没有 DML 计数时把返回行数填进影响行数
			notes = append(notes, fmt.Sprintf("查询返回 %d 行", oc.FetchRows))
			if oc.Affected < 0 {
				oc.Affected = oc.FetchRows
			}
		}
		msgs := joinMessages(oc.Messages)
		switch {
		case err != nil:
			text := errText(err)
			if msgs != "" {
				text = msgs + "\n" + text
			}
			out[i].Err = text
			failed = true
			continue
		case printFails && hasPrint(oc.Messages):
			// 开了 review.mssql_print_as_error：收到 PRINT 即视为失败（PRINT 常用来提示“不要执行”），本单元已回滚
			out[i].Err = msgs
			failed = true
			continue
		}
		if msgs != "" {
			notes = append(notes, msgs)
		}
		if oc.Affected > 0 {
			out[i].Affected = oc.Affected
		}
		out[i].Note = strings.Join(notes, "\n")
	}
	return out
}

func hasPrint(ms []Message) bool {
	for _, m := range ms {
		if m.Number == 0 {
			return true
		}
	}
	return false
}

func joinMessages(ms []Message) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		if t := strings.TrimSpace(m.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// sessionRunner 在同一条连接上执行单元。
//   - 普通单元：每个单元开一个事务，成功提交、失败回滚（语句级原子性）。
//   - 自管事务的单元（脚本里检查 @@TRANCOUNT，或自己写了 BEGIN TRAN / COMMIT / ROLLBACK / SAVE TRAN）：不再包外层事务，
//     原样在会话里执行（与 Navicat / SSMS 一致）。外面再包一层 BEGIN TRAN 会让脚本里的 “IF @@TRANCOUNT > 0 就拒绝执行”
//     这类检查永远命中，脚本自己的 COMMIT / ROLLBACK 也会和外层事务互相冲突。脚本留下的未提交事务由本类型收尾：
//     出错、被 PRINT 判失败或执行结束时仍未提交，一律回滚。
type sessionRunner struct {
	conn       *sql.Conn
	printFails bool // 收到 PRINT 时回滚本单元（与 ExecUnitsWith 的 printFails 一致）
	open       bool // 脚本自管的事务还没结束（@@TRANCOUNT > 0）：后续单元继续在它里面执行，不再包外层事务
}

// 注释、字符串常量与方括号标识符里出现的关键字不算：先抹掉再判断。
var (
	reTxnStrip = regexp.MustCompile(`(?s)--[^\n]*|/\*.*?\*/|N?'(?:[^']|'')*'|\[[^\]]*\]`)
	reTxnWords = regexp.MustCompile(`(?i)@@trancount\b|\b(?:begin|commit|rollback|save)\s+(?:distributed\s+)?tran(?:saction)?\b|\b(?:commit|rollback)\s*(?:;|$|\n)|\bsave\s+tran`)
)

// usesTxnControl 判断一个执行单元是否自己管理事务。
func usesTxnControl(unit string) bool {
	return reTxnWords.MatchString(reTxnStrip.ReplaceAllString(unit, " "))
}

// tranCount 读会话当前的 @@TRANCOUNT；读失败按 0 处理。
func (s *sessionRunner) tranCount(ctx context.Context) int {
	var n int
	if err := s.conn.QueryRowContext(ctx, "SELECT @@TRANCOUNT").Scan(&n); err != nil {
		return 0
	}
	return n
}

// rollbackOpen 回滚脚本留下的未提交事务（没有就什么也不做）。
func (s *sessionRunner) rollbackOpen(ctx context.Context) {
	if s.tranCount(ctx) > 0 {
		_, _ = s.conn.ExecContext(ctx, "ROLLBACK TRANSACTION")
	}
	s.open = false
}

// finish 在全部单元执行完后收尾：脚本留下的未提交事务回滚，并把最后一个已执行的单元记为失败，否则会把“其实没提交”的变更报成成功。
func (s *sessionRunner) finish(ctx context.Context, res []engine.ExecStmtResult) {
	if !s.open && s.tranCount(ctx) == 0 {
		return
	}
	s.rollbackOpen(ctx)
	for i := len(res) - 1; i >= 0; i-- {
		if res[i].Skipped {
			continue
		}
		if res[i].Err == "" {
			res[i].Err = "脚本结束时仍有未提交的事务（缺少 COMMIT），已回滚，本次变更没有生效。"
		}
		return
	}
}

func (s *sessionRunner) Run(ctx context.Context, unit string) (UnitOutcome, error) {
	oc := UnitOutcome{Affected: -1}
	// 自管事务：不包外层事务，原样执行
	var tx *sql.Tx
	var q interface {
		QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	} = s.conn
	managed := s.open || usesTxnControl(unit)
	if !managed {
		var err error
		if tx, err = s.conn.BeginTx(ctx, nil); err != nil {
			return oc, err
		}
		q = tx
	}
	abort := func() {
		if tx != nil {
			_ = tx.Rollback()
		} else {
			s.rollbackOpen(ctx)
		}
	}
	retmsg := &sqlexp.ReturnMessage{}
	rows, err := q.QueryContext(ctx, unit, retmsg)
	if err != nil {
		abort()
		return oc, err
	}
	var errs []error
	first := true
	for active := true; active; {
		switch m := retmsg.Message(ctx).(type) {
		case sqlexp.MsgNotice:
			oc.Messages = append(oc.Messages, notice(m.Message))
		case sqlexp.MsgNext:
			oc.ResultSets++
			for rows.Next() {
				oc.FetchRows++
			}
		case sqlexp.MsgNextResultSet:
			active = rows.NextResultSet()
		case sqlexp.MsgRowsAffected:
			if first {
				oc.Affected, first = m.Count, false
			}
		case sqlexp.MsgError:
			errs = append(errs, m.Error)
		}
	}
	if err := rows.Err(); err != nil {
		errs = append(errs, err)
	}
	_ = rows.Close()
	if len(errs) > 0 {
		abort()
		return oc, errs[0]
	}
	// 开了 review.mssql_print_as_error 时 PRINT 按失败处理：这里先回滚，由 ExecUnitsWith 记失败
	if s.printFails && hasPrint(oc.Messages) {
		abort()
		return oc, nil
	}
	if tx == nil {
		s.open = s.tranCount(ctx) > 0 // 脚本里 BEGIN TRAN 了还没 COMMIT：后续单元继续在它里面执行
		return oc, nil
	}
	if err := tx.Commit(); err != nil {
		return oc, err
	}
	return oc, nil
}

func notice(s fmt.Stringer) Message {
	var me mssqldb.Error
	switch v := s.(type) {
	case mssqldb.Error:
		me = v
		return Message{Number: me.Number, Text: me.Message}
	case *mssqldb.Error:
		return Message{Number: v.Number, Text: v.Message}
	}
	return Message{Number: -1, Text: s.String()}
}

var (
	_ engine.Engine        = (*Engine)(nil)
	_ engine.Writer        = (*Engine)(nil)
	_ engine.SyntaxChecker = (*Engine)(nil)
)

// ColumnSource 是服务端对一条查询的结果列所做的来源分析（sys.dm_exec_describe_first_result_set）。
// DB / Schema / Table / Column 全空表示表达式列、常量等没有来源的列。
type ColumnSource struct {
	Ordinal int // 结果列序号，从 1 开始
	Name    string
	DB      string
	Schema  string
	Table   string
	Column  string
}

// DescribeSources 不执行语句，让服务端分析第一个结果集每一列来自哪个库 / 架构 / 表 / 列（数据脱敏据此精确定位）。
// 返回两份：base 把视图展开到底层基表（browse 模式 1），views 保留视图自己的列（模式 2）。
// 配置可能写在基表上，也可能写在视图上，两份都要对照。
// 临时表、动态 SQL 等服务端无法静态推断结果集的语句会返回错误，调用方应降级为保守匹配。
func (e *Engine) DescribeSources(ctx context.Context, db, q string) (base, views []ColumnSource, err error) {
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	if db != "" {
		if _, err := conn.ExecContext(ctx, "USE "+QuoteIdent(db)); err != nil {
			return nil, nil, fmt.Errorf("切换到库 %s 失败: %s", db, errText(err))
		}
	}
	for i, mode := range []int{1, 2} {
		got, err := describeMode(ctx, conn, q, mode)
		if err != nil {
			return nil, nil, err
		}
		if i == 0 {
			base = got
		} else {
			views = got
		}
	}
	return base, views, nil
}

func describeMode(ctx context.Context, conn *sql.Conn, q string, mode int) ([]ColumnSource, error) {
	rows, err := conn.QueryContext(ctx,
		"SELECT column_ordinal, name, source_database, source_schema, source_table, source_column "+
			"FROM sys.dm_exec_describe_first_result_set(@p1, NULL, "+fmt.Sprint(mode)+") WHERE is_hidden = 0 ORDER BY column_ordinal", q)
	if err != nil {
		return nil, errors.New(errText(err))
	}
	defer rows.Close()
	var out []ColumnSource
	for rows.Next() {
		var (
			c                       ColumnSource
			name, db, sch, tbl, col sql.NullString
		)
		if err := rows.Scan(&c.Ordinal, &name, &db, &sch, &tbl, &col); err != nil {
			return nil, err
		}
		c.Name, c.DB, c.Schema, c.Table, c.Column = name.String, db.String, sch.String, tbl.String, col.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New(errText(err))
	}
	return out, nil
}
