// @author: kevinkelly
// @license: Apache Licence
// @file: scan.go
// @time: 2026/10/28

package binlog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"

	"dbtool/internal/i18n"
)

// scanSpec 是一次扫描的全部参数（已经校验、规范化）。
type scanSpec struct {
	Mode string

	StartFile string
	StartPos  uint32 // ≥ 4；回滚分段时，一个事务都没读完就到上限，续接位置只能退回到这里
	EndFile   string // 空表示读到快照末尾
	EndPos    uint32
	StartTS   int64 // unix 秒，0 表示不限
	StopTS    int64
	LastFile  string // 开始时目标实例的最后一个 binlog 及其大小：读到这里就停（之后写入的不管）
	LastSize  int64

	Visible func(schema string) bool // 用户可见的库（实例的显示 / 隐藏规则、系统库）
	Schemas map[string]bool          // 用户指定的库（小写），空表示不限
	Tables  []tableFilter
	Types   map[string]bool // INSERT / UPDATE / DELETE / DDL

	MaxRows  int
	MaxBytes int
	MaxScan  int

	SpillDir string // 结果临时文件的目录；空 = 不落盘，全放内存
	SpillAt  int    // 结果超过多少字节转存磁盘；0 = 默认（spillThreshold）

	genOpt
	ExtraInfo bool
	Loc       *time.Location
	Lang      string // 发起人的界面语言：只影响结果行里的说明性注释（-- [无法回滚的…]），空 = 中文
}

type tableFilter struct{ Schema, Table string } // 小写；Schema 为空表示任意库

// progress 是扫描进度，扫描协程写、查询接口读。
type progress struct {
	mu      sync.Mutex
	scanned int64
	matched int
	bytes   int
	file    string
	pos     uint32
}

func (p *progress) update(scanned int64, matched, bytes int, file string, pos uint32) {
	p.mu.Lock()
	p.scanned, p.matched, p.bytes, p.file, p.pos = scanned, matched, bytes, file, pos
	p.mu.Unlock()
}

func (p *progress) get() (scanned int64, matched, bytes int, file string, pos uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.scanned, p.matched, p.bytes, p.file, p.pos
}

// Resume 是回滚分段的续接位置：下一段从这里开始读，一定在事务边界上。
type Resume struct {
	File string
	Pos  uint32
}

// scanResult 是扫描结果。
type scanResult struct {
	mode      string     // 解析模式（sql / rollback）：决定结果按什么顺序读取
	Stmts     *stmtStore // 语句按读到的顺序（正序）存放；回滚结果按相反顺序读取（见 stmtStore.Page / EachReverse）
	Truncated string     // 非空表示没有读完：rows / bytes / scan / timeout / stall
	// Next 只在回滚模式下有值：到上限（或目标库卡住）时在事务边界截断，这一段的回滚 SQL 是完整可用的，后面的变更从 Next 继续读。
	Next *Resume
	// unitCut 表示是在一个事务（或单条语句）中间触到上限，Next 是这个事务的起点；此时如果 Trx 和语句数都是 0，
	// 说明第一个事务就放不下，从 Next 继续只会原样再来一遍，不能算分段。
	unitCut    bool
	Warnings   []string
	ByType     map[string]int
	DDL        int // 范围内的 DDL / 非行格式语句数量（不论是否被过滤掉）
	Trx        int
	Bytes      int
	Scanned    int64
	LastFile   string
	LastPos    uint32
	rollbackOK bool
}

type metaEntry struct {
	cols []colMeta
	err  error
}

type scanner struct {
	spec scanSpec
	meta MetaSource
	prog *progress
	res  *scanResult

	file    string
	posFile string // res.LastPos 属于哪个文件
	trxSeq  int
	curTrx  int
	scanned int64

	// 当前事务的起点：到上限时要把这个没读完的事务整个丢掉，续接位置就是它的起点（回滚分段）。
	trxFile    string
	trxPos     uint32
	trxCount   int            // 事务开始时结果里已有的语句数
	trxBytes   int            // 事务开始时的结果体积
	trxByType  map[string]int // 事务里新增的各类语句数
	trxDDL     int            // 事务里新增的 DDL / 非行格式语句计数
	storeError error          // 结果存储出错（写临时文件失败等）

	infos  map[string]*metaEntry
	tables map[string]*tableMeta
	warned map[string]bool
}

// scan 读取事件流，按 spec 过滤并生成语句。语句按读到的顺序存放；回滚模式下“最后发生的先回滚”只是读取结果时的顺序（stmtStore.Page / EachReverse）。
// 回滚需要完整的结果：任何一行无法生成回滚语句都会返回错误；结果被截断由调用方判断（res.Truncated）。
//
// 回滚模式下到上限（条数 / 体积 / 扫描事件数 / 超时 / 目标库卡住）不报错，而是在事务边界截断并给出续接位置 res.Next，
// 这一段的回滚 SQL 本身是完整的。返回的结果带临时文件：调用方不再需要它时要 res.Stmts.Close()。
func scan(ctx context.Context, spec scanSpec, st EventStream, meta MetaSource, prog *progress) (*scanResult, error) {
	s := &scanner{spec: spec, meta: meta, prog: prog, file: spec.StartFile,
		res:   &scanResult{mode: spec.Mode, Stmts: newStmtStore(spec.SpillDir, spec.SpillAt), ByType: map[string]int{}, LastFile: spec.StartFile},
		infos: map[string]*metaEntry{}, tables: map[string]*tableMeta{}, warned: map[string]bool{}, trxByType: map[string]int{}}
loop:
	for {
		if err := ctx.Err(); err != nil {
			return s.ctxEnd(err)
		}
		if s.scanned >= int64(spec.MaxScan) {
			s.limit("scan")
			break
		}
		ev, err := st.Next(ctx)
		if err != nil {
			switch {
			case errors.Is(err, io.EOF): // 目标库明确告诉我们没有更多事件（或测试里的流结束）
			case errors.Is(err, errStalled):
				// 一段时间没有新事件：要么真的读到了末尾（服务端没发 EOF 包时靠位置判断），要么目标库卡住了——后者不能当成读完
				if !s.reachedEnd() {
					s.limit("stall")
				}
			case ctx.Err() != nil:
				return s.ctxEnd(ctx.Err())
			default:
				return s.done(friendlyStreamErr(err))
			}
			break loop
		}
		s.scanned++
		stop, err := s.handle(ctx, ev)
		if err != nil {
			return s.done(err)
		}
		if s.scanned%256 == 0 {
			s.report()
		}
		if stop {
			break
		}
	}
	s.report()
	return s.done(nil)
}

// ctxEnd 处理 context 结束：回滚模式超时算“到上限”，在事务边界截断成一段；其它情况（用户取消、还原模式超时）原样返回错误。
func (s *scanner) ctxEnd(err error) (*scanResult, error) {
	if s.spec.Mode == ModeRollback && errors.Is(err, context.DeadlineExceeded) {
		s.limit("timeout")
		return s.done(nil)
	}
	return s.done(err)
}

// done 收尾：封存结果；扫描过程中结果存储出过错的话，以存储的错误为准。
func (s *scanner) done(err error) (*scanResult, error) {
	res := s.finish()
	if serr := res.Stmts.Seal(); serr != nil && s.storeError == nil {
		s.storeError = serr
	}
	if s.storeError != nil {
		return res, storeErr(s.storeError)
	}
	return res, err
}

func storeErr(err error) error {
	var be *Error
	if errors.As(err, &be) {
		return err
	}
	return newErr(http.StatusInternalServerError, "internal", fmt.Sprintf("保存解析结果失败：%v", err))
}

// limit 记录触发的上限。回滚模式下在事务边界截断：丢掉没读完的事务，续接位置是它的起点；
// 不在事务里就从最后读到的位置接着读（一个事件都没读到则退回起点）。
func (s *scanner) limit(reason string) {
	s.res.Truncated = reason
	if s.spec.Mode != ModeRollback {
		return
	}
	if s.curTrx != 0 {
		s.dropOpenTrx()
		s.res.Next = &Resume{File: s.trxFile, Pos: s.trxPos}
		s.res.unitCut = true
		return
	}
	switch {
	case s.posFile == s.file && s.res.LastPos > 0:
		s.res.Next = &Resume{File: s.file, Pos: s.res.LastPos}
	case s.file == s.spec.StartFile:
		s.res.Next = &Resume{File: s.file, Pos: max(s.spec.StartPos, 4)}
	default:
		s.res.Next = &Resume{File: s.file, Pos: 4} // 刚切到新文件，还没读到事件
	}
}

// dropOpenTrx 丢掉当前还没读完的事务已经加进结果里的语句，并把计数还原。
func (s *scanner) dropOpenTrx() {
	if err := s.res.Stmts.Truncate(s.trxCount); err != nil && s.storeError == nil {
		s.storeError = err
	}
	s.res.Bytes = s.trxBytes
	for k, n := range s.trxByType {
		if s.res.ByType[k] -= n; s.res.ByType[k] <= 0 {
			delete(s.res.ByType, k)
		}
	}
	s.res.DDL -= s.trxDDL
	s.trxSeq-- // 这个事务不计入本段
	s.curTrx = 0
	clear(s.trxByType)
	s.trxDDL = 0
}

// beginTrx / endTrx 跟踪事务的起点（BEGIN 查询事件的起始位置）。
func (s *scanner) beginTrx(h *replication.EventHeader) {
	s.trxFile, s.trxPos = s.file, h.LogPos-h.EventSize
	s.trxCount, s.trxBytes = s.res.Stmts.Len(), s.res.Bytes
	clear(s.trxByType)
	s.trxDDL = 0
}

func (s *scanner) endTrx() { s.curTrx = 0 }

// reachedEnd 在流停了一段时间之后判断：是不是已经读到了该读的末尾（结束位置，或开始时最后一个文件的大小）。
// 位置对不上说明目标库中途卡住了，不能把没读完的结果当成完整的。
func (s *scanner) reachedEnd() bool {
	sp := s.spec
	file, pos := sp.EndFile, int64(sp.EndPos)
	if file == "" {
		file, pos = sp.LastFile, sp.LastSize
	}
	if file == "" {
		return true // 没有可对照的结束位置（测试 / 旧调用）：沿用“空闲即读完”
	}
	if s.posFile != s.file {
		return fileCmp(s.file, file) > 0 // 当前文件里还一个事件都没读到
	}
	switch c := fileCmp(s.file, file); {
	case c > 0:
		return true
	case c < 0:
		return false
	}
	return pos <= 0 || int64(s.res.LastPos) >= pos
}

func (s *scanner) report() {
	if s.prog != nil {
		s.prog.update(s.scanned, s.res.Stmts.Len(), s.res.Bytes, s.res.LastFile, s.res.LastPos)
	}
}

func (s *scanner) finish() *scanResult {
	s.res.Scanned = s.scanned
	s.res.Trx = s.trxSeq
	if s.res.DDL > 0 {
		s.warn("ddl", fmt.Sprintf("范围内有 %d 条 DDL / 非行格式语句，它们无法回滚，且之前的行变更可能依赖当时的表结构；请人工确认", s.res.DDL))
	}
	return s.res
}

func (s *scanner) warn(key, msg string) {
	if s.warned[key] || len(s.res.Warnings) >= maxWarnings {
		return
	}
	s.warned[key] = true
	s.res.Warnings = append(s.res.Warnings, msg)
}

// friendlyStreamErr 把读取 binlog 流时的失败翻译成可读的业务错误；已经是业务错误的原样返回。secrets 里的字符串（实例密码）会从信息里去掉。
func friendlyStreamErr(err error, secrets ...string) error {
	var be *Error
	if errors.As(err, &be) {
		return err
	}
	m := err.Error()
	for _, sec := range secrets {
		if len(sec) >= 4 {
			m = strings.ReplaceAll(m, sec, "******")
		}
	}
	var me *mysql.MyError
	code := uint16(0)
	if errors.As(err, &me) {
		code = me.Code
	}
	switch {
	case strings.Contains(m, "no corresponding table map") || strings.Contains(m, "table map event"):
		return newErr(http.StatusBadRequest, "bad_position", "起始位置不在事务边界上（找不到对应的表映射事件）。请从事务开头的位置开始，或把起始位置留空从文件开头读取")
	case code == 1227 || strings.Contains(m, "REPLICATION SLAVE") || strings.Contains(strings.ToLower(m), "access denied"):
		return newErr(http.StatusForbidden, "no_privilege", fmt.Sprintf("实例账号没有权限拉取 binlog，需要 REPLICATION SLAVE（和 REPLICATION CLIENT）权限：%s", m))
	case code == 1236 || strings.Contains(m, "Could not find first log file"):
		return newErr(http.StatusBadRequest, "bad_position", fmt.Sprintf("起始文件或位置不存在（可能已被清理，或位置不在事件边界上）：%s", m))
	}
	return newErr(http.StatusBadGateway, "stream_failed", fmt.Sprintf("读取 binlog 失败：%s", m))
}

// fileSeq 取 binlog 文件名末尾的序号（mysql-bin.000012 → 12），取不到返回 -1。
func fileSeq(name string) int {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return -1
	}
	n, err := strconv.Atoi(name[i+1:])
	if err != nil {
		return -1
	}
	return n
}

func fileCmp(a, b string) int {
	x, y := fileSeq(a), fileSeq(b)
	if x >= 0 && y >= 0 {
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	}
	return strings.Compare(a, b)
}

// beyond 判断事件是否已经超出要读的范围。
func (s *scanner) beyond(endPos uint32, ts uint32) bool {
	sp := s.spec
	if sp.EndFile != "" {
		if c := fileCmp(s.file, sp.EndFile); c > 0 || (c == 0 && sp.EndPos > 0 && endPos > sp.EndPos) {
			return true
		}
	} else if sp.LastFile != "" {
		if c := fileCmp(s.file, sp.LastFile); c > 0 || (c == 0 && sp.LastSize > 0 && int64(endPos) > sp.LastSize) {
			return true
		}
	}
	return sp.StopTS > 0 && ts > 0 && int64(ts) > sp.StopTS
}

func (s *scanner) handle(ctx context.Context, ev *replication.BinlogEvent) (bool, error) {
	h := ev.Header
	switch e := ev.Event.(type) {
	case *replication.RotateEvent:
		name := string(e.NextLogName)
		if name != "" {
			s.file = name
			s.res.LastFile = name
		}
		sp := s.spec
		if sp.EndFile != "" && fileCmp(s.file, sp.EndFile) > 0 {
			return true, nil
		}
		if sp.EndFile == "" && sp.LastFile != "" && fileCmp(s.file, sp.LastFile) > 0 {
			return true, nil
		}
		return false, nil
	case *replication.FormatDescriptionEvent, *replication.HeartbeatEvent:
		return false, nil
	}
	if h.LogPos == 0 {
		return false, nil // 伪事件
	}
	if s.beyond(h.LogPos, h.Timestamp) {
		return true, nil
	}
	s.res.LastPos, s.posFile = h.LogPos, s.file
	switch e := ev.Event.(type) {
	case *replication.QueryEvent:
		return s.query(h, e)
	case *replication.XIDEvent:
		s.endTrx()
	case *replication.RowsEvent:
		return s.rows(ctx, h, e)
	case *replication.TransactionPayloadEvent:
		for _, in := range e.Events {
			if in == nil {
				continue
			}
			stop, err := s.handle(ctx, in)
			if stop || err != nil {
				return stop, err
			}
		}
	}
	return false, nil
}

func (s *scanner) stamp(ts uint32) string {
	loc := s.spec.Loc
	if loc == nil {
		loc = time.UTC
	}
	return time.Unix(int64(ts), 0).In(loc).Format("2006-01-02 15:04:05")
}

func firstWord(q string) string {
	q = strings.TrimLeft(q, " \t\r\n(")
	i := 0
	for i < len(q) && (q[i] >= 'A' && q[i] <= 'Z' || q[i] >= 'a' && q[i] <= 'z') {
		i++
	}
	return strings.ToUpper(q[:i])
}

func (s *scanner) query(h *replication.EventHeader, e *replication.QueryEvent) (bool, error) {
	q := strings.TrimSpace(string(e.Query))
	switch strings.ToUpper(q) {
	case "BEGIN":
		s.trxSeq++
		s.curTrx = s.trxSeq
		s.beginTrx(h)
		return false, nil
	case "COMMIT", "ROLLBACK", "":
		s.endTrx()
		return false, nil
	}
	kind := KindDDL
	w := firstWord(q)
	switch w {
	case "INSERT", "REPLACE", "UPDATE", "DELETE":
		kind = KindStatement
	case "CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME", "GRANT", "REVOKE", "ANALYZE", "OPTIMIZE", "REPAIR":
	default: // SET、FLUSH、SAVEPOINT、XA 等：不是数据或结构变更
		return false, nil
	}
	schema := string(e.Schema)
	if schema != "" && s.spec.Visible != nil && !s.spec.Visible(schema) {
		return false, nil
	}
	if len(s.spec.Schemas) > 0 && schema != "" && !s.spec.Schemas[strings.ToLower(schema)] {
		return false, nil
	}
	if h.Timestamp > 0 && s.spec.StartTS > 0 && int64(h.Timestamp) < s.spec.StartTS {
		return false, nil
	}
	if len(s.spec.Tables) > 0 && !mentionsTable(q, s.spec.Tables) {
		return false, nil
	}
	s.res.DDL++ // 不论类型是否被过滤掉都要统计：范围内有结构变更时要提醒
	if s.curTrx != 0 {
		s.trxDDL++
	}
	// 类型过滤：DDL 要选了 ddl；binlog_format=STATEMENT 下的 DML 按其关键字对应的类型
	want := KindDDL
	if kind == KindStatement {
		want = w
		if want == "REPLACE" {
			want = KindInsert
		}
	}
	if !s.spec.Types[want] {
		return false, nil
	}
	sql := q
	if !strings.HasSuffix(sql, ";") {
		sql += ";"
	}
	if s.spec.Mode == ModeRollback {
		tag := "-- [无法回滚的DDL]"
		if kind == KindStatement {
			tag = "-- [无法回滚的语句]"
		}
		sql = i18n.T(s.spec.Lang, tag) + " " + oneLine(sql)
	}
	return s.add(Stmt{Trx: s.curTrx, Time: s.stamp(h.Timestamp), File: s.file, Pos: h.LogPos - h.EventSize, EndPos: h.LogPos,
		Schema: schema, Type: kind, SQL: sql, NoRollback: true})
}

func oneLine(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "…"
	}
	return s
}

func mentionsTable(q string, tf []tableFilter) bool {
	lq := strings.ToLower(q)
	for _, f := range tf {
		if strings.Contains(lq, f.Table) {
			return true
		}
	}
	return false
}

func (s *scanner) passTable(schema, table string) bool {
	if len(s.spec.Tables) == 0 {
		return true
	}
	ls, lt := strings.ToLower(schema), strings.ToLower(table)
	for _, f := range s.spec.Tables {
		if f.Table == lt && (f.Schema == "" || f.Schema == ls) {
			return true
		}
	}
	return false
}

// add 加入一条结果；到达上限时标记截断并通知调用方停止。
// 回滚模式下到上限不是失败：丢掉这条语句所在的、还没读完的事务，结果在前一个事务边界结束，续接位置记在 res.Next。
func (s *scanner) add(st Stmt) (bool, error) {
	reason := ""
	switch {
	case s.res.Stmts.Len() >= s.spec.MaxRows:
		reason = "rows"
	case s.res.Bytes+st.size() > s.spec.MaxBytes:
		reason = "bytes"
	}
	if reason != "" {
		if s.spec.Mode == ModeRollback && s.curTrx == 0 {
			if st.Table != "" { // 不在事务里的行事件：它前面的表映射事件没法单独当起点，不能分段
				return true, newErr(http.StatusBadRequest, "limit_exceeded",
					fmt.Sprintf("%s位于 %s:%d 的行变更不在事务里，无法在它前面分段。请调大“最多条数 / 结果体积上限”，或缩小范围。", truncText(reason, Limits{MaxRows: s.spec.MaxRows, MaxBytes: s.spec.MaxBytes}, nil), st.File, st.Pos))
			}
			// 不在事务里的单个语句（DDL 等）放不下：就从它前面接着读；它在 query() 里已经计过一次 DDL 了
			s.res.DDL--
			s.res.Truncated = reason
			s.res.Next = &Resume{File: st.File, Pos: st.Pos}
			s.res.unitCut = true
			return true, nil
		}
		s.limit(reason)
		return true, nil
	}
	if err := s.res.Stmts.Append(st); err != nil {
		s.storeError = err
		return true, err
	}
	s.res.Bytes += st.size()
	s.res.ByType[st.Type]++
	if s.curTrx != 0 {
		s.trxByType[st.Type]++
	}
	return false, nil
}

func (s *scanner) rows(ctx context.Context, h *replication.EventHeader, e *replication.RowsEvent) (bool, error) {
	if e.Table == nil {
		return false, nil
	}
	var kind string
	switch e.Type() {
	case replication.EnumRowsEventTypeInsert:
		kind = KindInsert
	case replication.EnumRowsEventTypeUpdate:
		kind = KindUpdate
	case replication.EnumRowsEventTypeDelete:
		kind = KindDelete
	default:
		if h.EventType == replication.PARTIAL_UPDATE_ROWS_EVENT {
			return false, s.unsupported("含 JSON 部分更新（binlog_row_value_options=PARTIAL_JSON）的 UPDATE 无法还原")
		}
		return false, nil
	}
	schema, table := string(e.Table.Schema), string(e.Table.Table)
	if !s.spec.Types[kind] || (s.spec.Visible != nil && !s.spec.Visible(schema)) ||
		(len(s.spec.Schemas) > 0 && !s.spec.Schemas[strings.ToLower(schema)]) || !s.passTable(schema, table) {
		return false, nil
	}
	if s.spec.StartTS > 0 && int64(h.Timestamp) < s.spec.StartTS {
		return false, nil
	}
	tm, err := s.tableMetaFor(ctx, e.Table)
	if err != nil {
		return false, err
	}
	rollback := s.spec.Mode == ModeRollback
	if rollback && tm.Problem != "" {
		return false, newErr(400, "rollback_unsupported", fmt.Sprintf("表 %s.%s：%s，无法生成回滚 SQL", schema, table, tm.Problem))
	}
	if tm.Problem != "" {
		s.warn("meta:"+schema+"."+table, fmt.Sprintf("表 %s.%s：%s；该表的 SQL 用 @N 作为列名，仅供查看，不能直接执行", schema, table, tm.Problem))
	}
	if len(e.SkippedColumns) < len(e.Rows) {
		return false, newErr(502, "bad_event", "行事件格式异常（缺少列位图信息）")
	}
	step := 1
	if kind == KindUpdate {
		step = 2
	}
	pos := h.LogPos - h.EventSize
	for i := 0; i+step <= len(e.Rows); i += step {
		var sql string
		var err error
		switch kind {
		case KindInsert:
			if rollback {
				if !imageComplete(e.SkippedColumns[i]) {
					return false, s.partialImage(h, schema, table)
				}
				sql, err = tm.deleteSQL(s.spec.genOpt, e.Rows[i], e.SkippedColumns[i])
			} else {
				sql, err = tm.insertSQL(s.spec.genOpt, e.Rows[i], e.SkippedColumns[i])
			}
		case KindDelete:
			if rollback {
				if !imageComplete(e.SkippedColumns[i]) {
					return false, s.partialImage(h, schema, table)
				}
				sql, err = tm.insertSQL(s.spec.genOpt, e.Rows[i], e.SkippedColumns[i])
			} else {
				sql, err = tm.deleteSQL(s.spec.genOpt, e.Rows[i], e.SkippedColumns[i])
			}
		case KindUpdate:
			before, bs, after, as := e.Rows[i], e.SkippedColumns[i], e.Rows[i+1], e.SkippedColumns[i+1]
			if rollback {
				if !imageComplete(bs) || !imageComplete(as) {
					return false, s.partialImage(h, schema, table)
				}
				sql, err = tm.updateSQL(s.spec.genOpt, before, bs, after, as)
			} else {
				sql, err = tm.updateSQL(s.spec.genOpt, after, as, before, bs)
			}
		}
		if err != nil {
			if rollback {
				return false, newErr(400, "rollback_unsupported", fmt.Sprintf("%s.%s 位于 %s:%d 的行无法生成回滚 SQL：%v", schema, table, s.file, pos, err))
			}
			s.warn("row:"+schema+"."+table, fmt.Sprintf("表 %s.%s 有行无法还原成 SQL，已跳过（%v）", schema, table, err))
			continue
		}
		if s.spec.ExtraInfo {
			sql += fmt.Sprintf(" -- %s:%d %s", s.file, pos, s.stamp(h.Timestamp))
		}
		if stop, err := s.add(Stmt{Trx: s.curTrx, Time: s.stamp(h.Timestamp), File: s.file, Pos: pos, EndPos: h.LogPos,
			Schema: schema, Table: table, Type: kind, SQL: sql}); stop || err != nil {
			return stop, err
		}
	}
	return false, nil
}

func (s *scanner) unsupported(msg string) error {
	if s.spec.Mode == ModeRollback {
		return newErr(400, "rollback_unsupported", msg)
	}
	s.warn("unsupported:"+msg, fmt.Sprintf("%s，已跳过", msg))
	return nil
}

func (s *scanner) partialImage(h *replication.EventHeader, schema, table string) error {
	return newErr(400, "rollback_unsupported", fmt.Sprintf("%s.%s 位于 %s:%d 的行事件只记录了部分列（binlog_row_image 不是 FULL），无法生成回滚 SQL；仍可用“还原 SQL”模式解析",
		schema, table, s.file, h.LogPos-h.EventSize))
}

// tableMetaFor 取表映射事件对应的列信息（带缓存）：优先用 binlog 自带的列名（binlog_row_metadata=FULL），
// 否则读目标库 information_schema 的当前表结构；列数对不上说明表结构在这段 binlog 之后被改过。
func (s *scanner) tableMetaFor(ctx context.Context, tm *replication.TableMapEvent) (*tableMeta, error) {
	schema, table := string(tm.Schema), string(tm.Table)
	ik := strings.ToLower(schema) + "." + strings.ToLower(table)
	key := ik + "|" + strconv.FormatUint(tm.ColumnCount, 10) + "|" + string(tm.ColumnType)
	if m, ok := s.tables[key]; ok {
		return m, nil
	}
	info := s.infos[ik]
	if info == nil {
		cols, err := s.meta.Columns(ctx, schema, table)
		if err != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		info = &metaEntry{cols: cols, err: err}
		if len(s.infos) >= 2000 {
			s.infos = map[string]*metaEntry{}
		}
		s.infos[ik] = info
	}
	m := buildMeta(tm, info)
	if len(s.tables) >= 2000 {
		s.tables = map[string]*tableMeta{}
	}
	s.tables[key] = m
	return m, nil
}

func buildMeta(tm *replication.TableMapEvent, info *metaEntry) *tableMeta {
	n := int(tm.ColumnCount)
	m := &tableMeta{Schema: string(tm.Schema), Table: string(tm.Table), Cols: make([]colMeta, n)}
	names := tm.ColumnNameString()
	var use []colMeta
	if info != nil && info.err == nil {
		switch {
		case len(info.cols) == n:
			use = info.cols
		case len(info.cols) > n:
			var nv []colMeta
			for _, c := range info.cols {
				if !c.Virtual {
					nv = append(nv, c)
				}
			}
			if len(nv) == n {
				use = nv
			}
		}
	}
	switch {
	case len(names) == n && n > 0:
		for i := range m.Cols {
			m.Cols[i].Name = names[i]
			if use != nil && strings.EqualFold(use[i].Name, names[i]) {
				m.Cols[i] = use[i]
				m.Cols[i].Name = names[i]
			}
		}
		for _, p := range tm.PrimaryKey {
			if int(p) < n {
				m.PK = append(m.PK, int(p))
			}
		}
		if len(m.PK) == 0 {
			m.PK = pkOf(m.Cols)
		}
	case use != nil:
		copy(m.Cols, use)
		m.PK = pkOf(m.Cols)
	default:
		switch {
		case info != nil && info.err != nil:
			m.Problem = fmt.Sprintf("读取目标库表结构失败（%s）", info.err.Error())
		case info != nil && len(info.cols) == 0:
			m.Problem = "表在目标库中已不存在，无法得知列名"
		default:
			m.Problem = fmt.Sprintf("binlog 里有 %d 列，目标库当前表有 %d 列（表结构在这段 binlog 之后被修改过）", n, len(info.cols))
		}
	}
	return m
}

func pkOf(cols []colMeta) []int {
	var pk []int
	for i, c := range cols {
		if c.PK {
			pk = append(pk, i)
		}
	}
	return pk
}
