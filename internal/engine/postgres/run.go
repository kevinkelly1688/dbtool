// @author: kevinkelly
// @license: Apache Licence
// @file: run.go
// @time: 2026/10/28

package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// 在线查询的执行层：只读事务 + 服务端游标分批取 + 行数 / 字节 / 超时限制 + 结果整形。

// 查询限制的默认值与上限。
const (
	DefaultMaxRows  = 1000
	DefaultMaxBytes = 4 << 20
	DefaultTimeout  = 30 * time.Second
	HardMaxRows     = 1_000_000
	HardMaxBytes    = 256 << 20
	HardTimeout     = 10 * time.Minute

	fetchBatch  = 500             // 每次从游标取多少行
	clientGrace = 3 * time.Second // 客户端超时比服务端 statement_timeout 多等多久（让服务端先报超时，错误更准确）
	cursorName  = "db_tool_cur"
)

// QueryLimits 是一次查询的限制；零值字段取默认值，超过硬上限的值会被收紧。并发限制由上层控制。
type QueryLimits struct {
	MaxRows  int           // 最多返回多少行（超出截断并标记）
	MaxBytes int           // 结果所有单元格合计的字节上限（超出截断并标记）
	Timeout  time.Duration // 语句执行时限（服务端 statement_timeout）
}

func (l QueryLimits) normalize() QueryLimits {
	if l.MaxRows <= 0 {
		l.MaxRows = DefaultMaxRows
	}
	if l.MaxRows > HardMaxRows {
		l.MaxRows = HardMaxRows
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultMaxBytes
	}
	if l.MaxBytes > HardMaxBytes {
		l.MaxBytes = HardMaxBytes
	}
	if l.Timeout <= 0 {
		l.Timeout = DefaultTimeout
	}
	if l.Timeout > HardTimeout {
		l.Timeout = HardTimeout
	}
	return l
}

// 截断原因（Shaped.TruncatedBy）。
const (
	TruncRows  = "行数上限"
	TruncBytes = "结果大小上限"
)

// Column 是结果列。
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"` // 类型名（int4、text、timestamptz、numeric、jsonb、int4[] …）
	OID  uint32 `json:"oid"`
	// Source 是该列直接来自哪张表的哪一列；表达式列、子查询输出列等没有来源时为空。数据脱敏据此精确定位。
	Source *ColumnSource `json:"-"`
}

// Shaped 是整形后的查询结果。
type Shaped struct {
	Columns     []Column
	Rows        [][]any
	Truncated   bool          // 因行数 / 字节上限被截断
	TruncatedBy string        // TruncRows / TruncBytes
	Elapsed     time.Duration // 从开启事务到取完结果
	Warnings    []string      // 给用户看的提示（自动补 LIMIT、截断、bytea 转义 …）
	BinaryCells int           // bytea 单元格个数
	CutCells    int           // 被截断的超长单元格个数
	ExecutedSQL string        // 实际发给服务端的语句（可能补了 LIMIT）
	// SourcesKnown 为 true 表示各列的 Source 已成功解析（没有 Source 的是表达式列）；false 表示没能取到来源信息，所有列都当作来源未知。
	SourcesKnown bool
}

// ColumnNames 返回列名。
func (s *Shaped) ColumnNames() []string {
	out := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		out[i] = c.Name
	}
	return out
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// RunUser 在 db 库上执行已通过 CheckUserQuery 的语句并整形结果。
//
// 不信任调用方传来的 Checked：先对 c.SQL 重新做一遍静态检查（任何被篡改 / 手工构造的 Checked 都会被拒绝）。
// select 类语句用服务端游标（DECLARE … CURSOR / FETCH）分批取，顶层没有 LIMIT 时补 LIMIT MaxRows+1；
// EXPLAIN / SHOW 直接执行（结果天然很小，仍按行数上限截断）。整个过程在 BEGIN READ ONLY 事务里，结束时 ROLLBACK。
// 超时返回 ErrTimeout（errors.Is / IsTimeout）；服务端 SQL 错误返回 *UserError（带 SQLSTATE）；ctx 取消返回 context.Canceled。
func (e *Engine) RunUser(ctx context.Context, db string, c *Checked, lim QueryLimits) (*Shaped, error) {
	if c == nil {
		return nil, uerr("缺少已检查的语句")
	}
	db = e.dbOrDefault(db)
	chk, err := CheckUserQuery(c.SQL, CheckOptions{DefaultSchema: c.DefaultSchema, KeepServerSearchPath: !c.PinSearchPath, DBName: db})
	if err != nil {
		return nil, err
	}
	lim = lim.normalize()
	ctx, cancel := context.WithTimeout(ctx, lim.Timeout+clientGrace)
	defer cancel()

	out := &Shaped{Rows: [][]any{}}
	start := time.Now()
	var extra []string
	if chk.PinSearchPath {
		extra = append(extra, "SET LOCAL search_path = "+quoteIdent(chk.DefaultSchema)+", pg_catalog")
	}
	err = e.readTx(ctx, db, lim.Timeout, extra, func(s *session) error {
		if chk.Kind == KindSelect {
			return e.runCursor(ctx, s, chk, lim, out)
		}
		return e.runDirect(ctx, s, chk, lim, out)
	})
	out.Elapsed = time.Since(start)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// shaper 累积结果行并执行字节上限。
type shaper struct {
	lim   QueryLimits
	out   *Shaped
	st    shapeStats
	bytes int
	done  bool // 已经触达上限，之后的行丢弃
}

func (sh *shaper) setColumns(fds []pgconn.FieldDescription) {
	if len(sh.out.Columns) > 0 || len(fds) == 0 {
		return
	}
	cols := make([]Column, len(fds))
	for i, fd := range fds {
		cols[i] = Column{Name: toValidUTF8(fd.Name), OID: fd.DataTypeOID, Type: typeName(fd.DataTypeOID)}
	}
	sh.out.Columns = cols
}

// add 追加一行；返回 false 表示已达上限（这一行没有加入）。
func (sh *shaper) add(raw [][]byte) bool {
	if sh.done {
		return false
	}
	if len(sh.out.Rows) >= sh.lim.MaxRows {
		sh.out.Truncated, sh.out.TruncatedBy, sh.done = true, TruncRows, true
		return false
	}
	row := make([]any, len(raw))
	n := 0
	before := sh.st
	for i, v := range raw {
		var oid uint32
		if i < len(sh.out.Columns) {
			oid = sh.out.Columns[i].OID
		}
		row[i] = sh.st.convert(oid, v)
		switch x := row[i].(type) {
		case string:
			n += len(x)
		case nil:
		default:
			n += 8
		}
	}
	if sh.bytes+n > sh.lim.MaxBytes && len(sh.out.Rows) > 0 {
		sh.st = before
		sh.out.Truncated, sh.out.TruncatedBy, sh.done = true, TruncBytes, true
		return false
	}
	sh.bytes += n
	sh.out.Rows = append(sh.out.Rows, row)
	return true
}

func (sh *shaper) finish(autoLimit bool, limit int) {
	o := sh.out
	o.BinaryCells, o.CutCells = sh.st.Binary, sh.st.Cut
	if autoLimit {
		o.Warnings = append(o.Warnings, fmt.Sprintf("语句没有 LIMIT，已自动限制最多返回 %d 行。", limit))
	}
	if o.Truncated {
		o.Warnings = append(o.Warnings, fmt.Sprintf("结果已按%s截断（最多 %d 行 / %d MB）；请缩小范围或用 LIMIT / OFFSET 分页。", o.TruncatedBy, sh.lim.MaxRows, sh.lim.MaxBytes>>20))
	}
	if sh.st.Binary > 0 {
		o.Warnings = append(o.Warnings, fmt.Sprintf("%d 个 bytea 值按 \\xHH 十六进制显示。", sh.st.Binary))
	}
	if sh.st.Cut > 0 {
		o.Warnings = append(o.Warnings, fmt.Sprintf("%d 个值超过 %d KB，已截断显示。", sh.st.Cut, MaxCellBytes>>10))
	}
}

func (e *Engine) runCursor(ctx context.Context, s *session, chk *Checked, lim QueryLimits, out *Shaped) error {
	execSQL := chk.ExecSQL(lim.MaxRows)
	out.ExecutedSQL = execSQL
	// FETCH 游标的结果描述里没有来源表 OID / 列号，先用 Parse + Describe 取一次。语句本身有错时，这里报的错与 DECLARE 一致。
	origins, err := s.describeOrigins(ctx, execSQL)
	if err != nil {
		return err
	}
	if _, err := s.exec(ctx, "DECLARE "+cursorName+" NO SCROLL CURSOR FOR "+execSQL); err != nil {
		return err
	}
	sh := &shaper{lim: lim, out: out}
	want := lim.MaxRows + 1
	got := 0
	for got < want && !sh.done {
		n := min(fetchBatch, want-got)
		cnt := 0
		fds, err := s.stream(ctx, "FETCH FORWARD "+strconv.Itoa(n)+" FROM "+cursorName, func(fds []pgconn.FieldDescription, row [][]byte) bool {
			sh.setColumns(fds)
			cnt++
			return sh.add(row)
		})
		if err != nil {
			return err
		}
		sh.setColumns(fds)
		got += cnt
		if cnt < n {
			break
		}
	}
	e.resolveTypes(ctx, s, out)
	e.resolveSources(ctx, s, out, origins)
	sh.finish(chk.AutoLimited(lim.MaxRows), lim.MaxRows)
	return nil
}

func (e *Engine) runDirect(ctx context.Context, s *session, chk *Checked, lim QueryLimits, out *Shaped) error {
	out.ExecutedSQL = chk.SQL
	sh := &shaper{lim: lim, out: out}
	fds, err := s.stream(ctx, chk.SQL, func(fds []pgconn.FieldDescription, row [][]byte) bool {
		sh.setColumns(fds)
		return sh.add(row)
	})
	if err != nil {
		return err
	}
	sh.setColumns(fds)
	e.resolveTypes(ctx, s, out)
	sh.finish(false, lim.MaxRows)
	return nil
}

// resolveTypes 给内置类型表里没有的类型（枚举、复合、扩展类型 citext / hstore / geometry …）查 pg_type 补上类型名。
// 失败不影响查询结果（类型名留空）。必须在事务还可用时调用，而且只在确有未知类型时才多发一次查询。
func (e *Engine) resolveTypes(ctx context.Context, s *session, out *Shaped) {
	var unknown []uint32
	seen := map[uint32]bool{}
	for _, c := range out.Columns {
		if c.Type == "" && !seen[c.OID] {
			seen[c.OID] = true
			unknown = append(unknown, c.OID)
		}
	}
	if len(unknown) == 0 {
		return
	}
	parts := make([]string, len(unknown))
	for i, o := range unknown {
		parts[i] = strconv.FormatUint(uint64(o), 10)
	}
	rows, err := s.rows(ctx, "SELECT t.oid::text, pg_catalog.format_type(t.oid, NULL) FROM pg_catalog.pg_type t WHERE t.oid = ANY($1::oid[])", "{"+strings.Join(parts, ",")+"}")
	if err != nil {
		for i := range out.Columns {
			if out.Columns[i].Type == "" {
				out.Columns[i].Type = "oid:" + strconv.FormatUint(uint64(out.Columns[i].OID), 10)
			}
		}
		return
	}
	names := map[string]string{}
	for _, r := range rows {
		if len(r) == 2 && r[0] != nil && r[1] != nil {
			names[*r[0]] = *r[1]
		}
	}
	for i := range out.Columns {
		if out.Columns[i].Type == "" {
			key := strconv.FormatUint(uint64(out.Columns[i].OID), 10)
			if n := names[key]; n != "" {
				out.Columns[i].Type = n
			} else {
				out.Columns[i].Type = "oid:" + key
			}
		}
	}
}
