// @author: kevinkelly
// @license: Apache Licence
// @file: sqlstmt.go
// @time: 2026/10/28

package masking

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 工单里的 SQL 文本脱敏（UPDATE / INSERT / REPLACE / DELETE 及其 WHERE）。
//
// 与查询结果脱敏不同，这里要遮盖的是语句里“绑定到已配置字段的字面量”：
//
//	UPDATE abc SET password='345345345' WHERE userid=9182   →   UPDATE abc SET password=*** WHERE userid=9182
//
// 做法是不依赖语法树的词法分析，宁多勿漏：
//  1. 找出语句涉及的表（UPDATE / FROM / JOIN / INTO 后面的名字与别名）；
//  2. 凡是 “字段 比较运算符 值”（=、<>、<、>、LIKE、IN (...)、BETWEEN ... AND ...），字段命中配置（库.表.字段，或同名字段）就遮盖值里的全部字面量；
//  3. INSERT / REPLACE 按字段列表与 VALUES 的位置一一对应，只遮盖命中字段那一列；没有字段列表时这张表的 VALUES 全部遮盖；
//  4. 语句里出现了命中的字段名却一条都没认出来（用了 CASE、子查询等），就把这条语句里所有字符串和数字字面量都遮盖。
//
// 被遮盖的原值同时返回，调用方用它把错误信息等派生文本里的同一个值也清掉。

// StmtMasker 对一段 SQL 文本做脱敏。零值或没有配置时 Mask 原样返回。
type StmtMasker struct {
	d     Dialect
	defDB string
	cols  map[string][]Column // 小写字段名 -> 配置
	rules map[int]Rule
	// anyWildcard 表示存在“任意库表同名字段”的配置；无字段列表的 INSERT 因此要整体遮盖。
	anyWildcard bool
}

// NewStmtMasker 创建。defaultDB 是工单选择的库（语句里没有限定库名时用它）；rules 为空时用内置默认规则。
func NewStmtMasker(d Dialect, defaultDB string, cols []Column, rules map[int]Rule) *StmtMasker {
	m := &StmtMasker{d: d, defDB: defaultDB, cols: map[string][]Column{}, rules: rules}
	if m.rules == nil {
		m.rules = DefaultRules()
	}
	for _, c := range cols {
		k := strings.ToLower(c.Column)
		if k == "" {
			continue
		}
		m.cols[k] = append(m.cols[k], c)
		if isWildcard(c) {
			m.anyWildcard = true
		}
	}
	return m
}

// Enabled 报告是否有配置可用。
func (m *StmtMasker) Enabled() bool { return m != nil && len(m.cols) > 0 }

func isWildcard(c Column) bool { return c.Schema == "*" && c.Table == "*" }

// ---- 词法 ----

type sTok struct {
	kind       tokKind
	text       string // 标识符 / 字符串已去引号
	lower      string
	start, end int // 在原文里的字节区间
	depth      int // 括号层级：括号本身取外一层，内容取内一层
}

func scanSQL(sql string, d Dialect) []sTok {
	n := len(sql)
	var out []sTok
	depth := 0
	add := func(k tokKind, text string, s, e, dp int) {
		out = append(out, sTok{kind: k, text: text, lower: strings.ToLower(text), start: s, end: e, depth: dp})
	}
	// quoted 读一个用 q 括起来的片段（q 连写两次是转义；backslash 为真时反斜杠转义下一个字符），返回内容与结束位置（引号之后）。
	quoted := func(i int, q byte, backslash bool) (string, int) {
		j := i + 1
		var b []byte
		for j < n {
			if backslash && sql[j] == '\\' && j+1 < n {
				b = append(b, sql[j+1])
				j += 2
				continue
			}
			if sql[j] == q {
				if j+1 < n && sql[j+1] == q {
					b = append(b, q)
					j += 2
					continue
				}
				return string(b), j + 1
			}
			b = append(b, sql[j])
			j++
		}
		return string(b), n
	}
	for i := 0; i < n; {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case c == '-' && i+1 < n && sql[i+1] == '-':
			for i < n && sql[i] != '\n' {
				i++
			}
		case c == '#' && d != DialectMSSQL:
			for i < n && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && sql[i+1] == '*':
			j := strings.Index(sql[i+2:], "*/")
			if j < 0 {
				i = n
			} else {
				i += 2 + j + 2
			}
		case c == '\'' || (c == '"' && d != DialectMSSQL):
			s, e := quoted(i, c, d != DialectMSSQL)
			add(tString, s, i, e, depth)
			i = e
		case c == '`' || (c == '"' && d == DialectMSSQL):
			s, e := quoted(i, c, false)
			add(tQuoted, s, i, e, depth)
			i = e
		case c == '[' && d == DialectMSSQL:
			j := i + 1
			var b []byte
			for j < n {
				if sql[j] == ']' {
					if j+1 < n && sql[j+1] == ']' {
						b = append(b, ']')
						j += 2
						continue
					}
					break
				}
				b = append(b, sql[j])
				j++
			}
			add(tQuoted, string(b), i, min(j+1, n), depth)
			i = min(j+1, n)
		default:
			r, size := utf8.DecodeRuneInString(sql[i:])
			switch {
			case isIdentStart(r, d):
				j := i + size
				for j < n {
					r2, s2 := utf8.DecodeRuneInString(sql[j:])
					if !isIdentPart(r2) {
						break
					}
					j += s2
				}
				word := sql[i:j]
				lw := strings.ToLower(word)
				// 字符串前缀：N'..'、x'..'、b'..'、_utf8'..'：连同引号一起算一个字符串字面量
				if j < n && sql[j] == '\'' && (lw == "n" || lw == "x" || lw == "b" || strings.HasPrefix(lw, "_")) {
					s, e := quoted(j, '\'', d != DialectMSSQL && lw != "x" && lw != "b")
					add(tString, s, i, e, depth)
					i = e
					break
				}
				add(tIdent, word, i, j, depth)
				i = j
			case unicode.IsDigit(r):
				j := i + 1
				if c == '0' && j < n && (sql[j] == 'x' || sql[j] == 'X') {
					j++
					for j < n && isHex(sql[j]) {
						j++
					}
				} else {
					for j < n && (sql[j] >= '0' && sql[j] <= '9' || sql[j] == '.' || sql[j] == 'e' || sql[j] == 'E') {
						j++
					}
				}
				add(tNumber, sql[i:j], i, j, depth)
				i = j
			default:
				// 多字符运算符
				for _, op := range []string{"<=>", "<=", ">=", "<>", "!=", ":="} {
					if strings.HasPrefix(sql[i:], op) {
						add(tPunct, op, i, i+len(op), depth)
						i += len(op)
						goto next
					}
				}
				switch c {
				case '(':
					add(tPunct, "(", i, i+1, depth)
					depth++
				case ')':
					if depth > 0 {
						depth--
					}
					add(tPunct, ")", i, i+1, depth)
				default:
					add(tPunct, string(r), i, i+size, depth)
				}
				i += size
			}
		}
	next:
	}
	return out
}

func isHex(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

func (t sTok) isPunct(s string) bool { return t.kind == tPunct && t.text == s }
func (t sTok) isWord(s string) bool  { return t.kind == tIdent && t.lower == s }
func (t sTok) isName() bool          { return t.kind == tIdent || t.kind == tQuoted }
func (t sTok) isLiteral() bool       { return t.kind == tString || t.kind == tNumber }

// ---- 表引用 ----

type tref struct {
	db, table, alias string // table：MySQL 系是表名，SQL Server 是 架构.表（没写架构时只有表名）
}

// 不能当表别名的词（出现在表名后面的子句关键字）。
var notTableAlias = map[string]bool{
	"set": true, "where": true, "on": true, "using": true, "left": true, "right": true, "inner": true, "outer": true, "cross": true,
	"natural": true, "full": true, "join": true, "group": true, "order": true, "having": true, "limit": true, "union": true,
	"values": true, "value": true, "select": true, "partition": true, "use": true, "force": true, "ignore": true, "straight_join": true,
	"window": true, "for": true, "lock": true, "into": true, "output": true, "with": true, "returning": true, "option": true,
	"from": true, "when": true, "then": true, "else": true, "end": true, "and": true, "or": true, "default": true,
}

var tableModifiers = map[string]bool{"only": true, "low_priority": true, "ignore": true, "quick": true, "delayed": true, "high_priority": true, "top": true}

func lastSeg(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

// tableRefs 找出语句涉及的表。
func (m *StmtMasker) tableRefs(toks []sTok, defDB string) []tref {
	var out []tref
	n := len(toks)
	for i := 0; i < n; i++ {
		t := toks[i]
		if t.kind != tIdent {
			continue
		}
		list := false
		switch t.lower {
		case "from", "update":
			list = true
		case "join", "into":
		default:
			continue
		}
		j := i + 1
		for j < n {
			for j < n && toks[j].kind == tIdent && tableModifiers[toks[j].lower] {
				j++
			}
			if j >= n || !toks[j].isName() {
				break // 子查询 / 派生表等：不解析
			}
			parts := []string{toks[j].text}
			j++
			for j+1 < n && toks[j].isPunct(".") && toks[j+1].isName() {
				parts = append(parts, toks[j+1].text)
				j += 2
			}
			ref := m.makeRef(parts, defDB)
			if j < n && toks[j].isWord("as") {
				j++
			}
			if j < n && toks[j].isName() && !(toks[j].kind == tIdent && notTableAlias[toks[j].lower]) {
				ref.alias = toks[j].text
				j++
			}
			out = append(out, ref)
			if list && j < n && toks[j].isPunct(",") {
				j++
				continue
			}
			break
		}
	}
	return out
}

func (m *StmtMasker) makeRef(parts []string, defDB string) tref {
	switch m.d {
	case DialectMSSQL: // [库.]架构.表
		switch len(parts) {
		case 1:
			return tref{db: defDB, table: parts[0]}
		case 2:
			return tref{db: defDB, table: parts[0] + "." + parts[1]}
		default:
			k := len(parts)
			return tref{db: parts[k-3], table: parts[k-2] + "." + parts[k-1]}
		}
	}
	if len(parts) == 1 {
		return tref{db: defDB, table: parts[0]}
	}
	k := len(parts)
	return tref{db: parts[k-2], table: parts[k-1]}
}

func dbMatch(cfg, db string) bool {
	return cfg == "" || cfg == "*" || db == "" || strings.EqualFold(cfg, db)
}

// tabMatch 配置里的表名与语句里的表名比较：一边没写架构时只比最后一段。
func tabMatch(cfg, ref string) bool {
	if cfg == "" || cfg == "*" || strings.EqualFold(cfg, ref) {
		return true
	}
	if !strings.Contains(cfg, ".") || !strings.Contains(ref, ".") {
		return strings.EqualFold(lastSeg(cfg), lastSeg(ref))
	}
	return false
}

// ruleOf 取配置实际使用的规则；规则找不到时按整体隐藏（显示用途，宁多勿漏）。
func (m *StmtMasker) ruleOf(c Column) Rule {
	if r, ok := c.ResolveColumn(m.rules); ok {
		return r
	}
	return Rule{Kind: KindWhole}
}

// resolve 判断 qualifier.name 是否命中配置，命中返回应使用的（最严的）规则。
func (m *StmtMasker) resolve(qualifier, name string, refs []tref) (Rule, bool) {
	cands := m.cols[strings.ToLower(name)]
	if len(cands) == 0 {
		return Rule{}, false
	}
	sel := refs
	if qualifier != "" {
		var q []tref
		for _, r := range refs {
			if strings.EqualFold(r.alias, qualifier) || strings.EqualFold(r.table, qualifier) || strings.EqualFold(lastSeg(r.table), qualifier) {
				q = append(q, r)
			}
		}
		if len(q) > 0 {
			sel = q
		}
	}
	var best Rule
	found := false
	for _, c := range cands {
		hit := isWildcard(c) || len(sel) == 0 // 认不出表时按字段名处理（宁多勿漏）
		for _, r := range sel {
			if hit {
				break
			}
			hit = dbMatch(c.Schema, r.db) && tabMatch(c.Table, r.table)
		}
		if !hit {
			continue
		}
		r := m.ruleOf(c)
		if !found || r.Strictness() > best.Strictness() {
			best, found = r, true
		}
	}
	return best, found
}

// tableConfigured 判断这张表有没有任何字段配置（无字段列表的 INSERT 用）。
func (m *StmtMasker) tableConfigured(ref tref) bool {
	if m.anyWildcard {
		return true
	}
	for _, list := range m.cols {
		for _, c := range list {
			if dbMatch(c.Schema, ref.db) && tabMatch(c.Table, ref.table) {
				return true
			}
		}
	}
	return false
}

// ---- 值区间 ----

// 值表达式的结束词（同一括号层级上遇到就停）。
var regionStop = map[string]bool{
	"and": true, "or": true, "xor": true, "where": true, "order": true, "group": true, "having": true, "limit": true, "union": true,
	"intersect": true, "except": true, "on": true, "from": true, "set": true, "join": true, "left": true, "right": true, "inner": true,
	"outer": true, "cross": true, "natural": true, "full": true, "into": true, "values": true, "value": true, "using": true,
	"returning": true, "output": true, "option": true, "for": true, "offset": true, "fetch": true, "window": true, "select": true,
	"when": true, "then": true, "else": true, "end": true,
}

// regionEnd 返回从 r 开始的值表达式在 toks 里的结束位置（不含）。
func regionEnd(toks []sTok, r int) int {
	if r >= len(toks) {
		return r
	}
	d0 := toks[r].depth
	j := r
	for j < len(toks) {
		t := toks[j]
		if t.depth < d0 {
			break
		}
		if t.depth == d0 {
			if t.kind == tPunct && (t.text == "," || t.text == ";") {
				break
			}
			if t.kind == tIdent && regionStop[t.lower] {
				break
			}
		}
		j++
	}
	return j
}

var cmpOps = map[string]bool{"=": true, "<=>": true, "!=": true, "<>": true, "<": true, ">": true, "<=": true, ">=": true, ":=": true}

type edit struct {
	start, end int
	repl       string
	secret     string
}

type stmtRun struct {
	m      *StmtMasker
	sql    string
	toks   []sTok
	refs   []tref
	defDB  string
	edits  map[int]edit // 起点 -> 替换
	masked int          // 命中并遮盖的字面量个数
	seen   int          // 认出的“字段 IS [NOT] NULL”之类不需要遮盖的引用个数
}

func (r *stmtRun) maskTok(i int, rule Rule) {
	t := r.toks[i]
	if !t.isLiteral() {
		return
	}
	if _, ok := r.edits[t.start]; ok {
		return
	}
	text := t.text
	repl := Mask
	if v, err := Apply(text, rule); err == nil {
		if s, ok := v.(string); ok {
			repl = s
		}
	}
	r.edits[t.start] = edit{start: t.start, end: t.end, repl: repl, secret: text}
	r.masked++
}

func (r *stmtRun) maskRange(from, to int, rule Rule) {
	for i := from; i < to && i < len(r.toks); i++ {
		r.maskTok(i, rule)
	}
}

// refAt 在 i 处解析 [限定符.]字段，返回字段名、限定符和下一个位置；i 处不是字段引用返回 ok=false。
func (r *stmtRun) refAt(i int) (name, qualifier string, next int, ok bool) {
	n := len(r.toks)
	if i >= n || !r.toks[i].isName() {
		return "", "", i, false
	}
	if i > 0 && r.toks[i-1].isPunct(".") {
		return "", "", i, false // 限定符的一部分，由起点处理
	}
	parts := []string{r.toks[i].text}
	j := i + 1
	for j+1 < n && r.toks[j].isPunct(".") && r.toks[j+1].isName() {
		parts = append(parts, r.toks[j+1].text)
		j += 2
	}
	if j < n && r.toks[j].isPunct("(") { // 函数调用
		return "", "", i, false
	}
	name = parts[len(parts)-1]
	if len(parts) > 1 {
		qualifier = parts[len(parts)-2]
	}
	return name, qualifier, j, true
}

func (r *stmtRun) generic() {
	n := len(r.toks)
	for i := 0; i < n; i++ {
		t := r.toks[i]
		// 反过来写：'值' = 字段
		if t.isLiteral() && i+2 < n && r.toks[i+1].kind == tPunct && cmpOps[r.toks[i+1].text] {
			if name, q, _, ok := r.refAt(i + 2); ok {
				if rule, hit := r.m.resolve(q, name, r.refs); hit {
					r.maskTok(i, rule)
				}
			}
			continue
		}
		name, q, e, ok := r.refAt(i)
		if !ok {
			continue
		}
		rule, hit := r.m.resolve(q, name, r.refs)
		if !hit {
			continue
		}
		if e >= n {
			continue
		}
		op := r.toks[e]
		switch {
		case op.kind == tPunct && cmpOps[op.text]:
			r.maskRange(e+1, regionEnd(r.toks, e+1), rule)
		case op.isWord("like") || op.isWord("regexp") || op.isWord("rlike"):
			r.maskRange(e+1, regionEnd(r.toks, e+1), rule)
		case op.isWord("not") && e+1 < n && (r.toks[e+1].isWord("like") || r.toks[e+1].isWord("regexp") || r.toks[e+1].isWord("rlike") || r.toks[e+1].isWord("in") || r.toks[e+1].isWord("between")):
			k := e + 1
			switch r.toks[k].lower {
			case "in":
				r.maskIn(k+1, rule)
			case "between":
				r.maskBetween(k+1, rule)
			default:
				r.maskRange(k+1, regionEnd(r.toks, k+1), rule)
			}
		case op.isWord("in"):
			r.maskIn(e+1, rule)
		case op.isWord("between"):
			r.maskBetween(e+1, rule)
		case op.isWord("is"):
			r.seen++ // IS [NOT] NULL：没有值可遮盖
		}
	}
}

// maskIn 遮盖 IN ( ... ) 列表里的字面量（子查询不处理，里面有自己的条件）。
func (r *stmtRun) maskIn(i int, rule Rule) {
	n := len(r.toks)
	if i >= n || !r.toks[i].isPunct("(") {
		return
	}
	if i+1 < n && r.toks[i+1].isWord("select") {
		return
	}
	d := r.toks[i].depth + 1
	for j := i + 1; j < n; j++ {
		t := r.toks[j]
		if t.depth < d {
			return
		}
		r.maskTok(j, rule)
	}
}

func (r *stmtRun) maskBetween(i int, rule Rule) {
	e := regionEnd(r.toks, i)
	r.maskRange(i, e, rule)
	if e < len(r.toks) && r.toks[e].isWord("and") {
		r.maskRange(e+1, regionEnd(r.toks, e+1), rule)
	}
}

// insert 处理 INSERT / REPLACE ... [(列, ...)] VALUES (...), (...)。
func (r *stmtRun) insert() {
	n := len(r.toks)
	i := 0
	if n == 0 || !(r.toks[0].isWord("insert") || r.toks[0].isWord("replace")) {
		return
	}
	for i < n && !r.toks[i].isWord("into") && !r.toks[i].isWord("values") && !r.toks[i].isWord("value") {
		i++
	}
	if i < n && r.toks[i].isWord("into") {
		i++
	}
	// 表名
	var parts []string
	for i < n && r.toks[i].isName() {
		parts = append(parts, r.toks[i].text)
		i++
		if i+1 < n && r.toks[i].isPunct(".") && r.toks[i+1].isName() {
			i++
			continue
		}
		break
	}
	var ref tref
	if len(parts) > 0 {
		ref = r.m.makeRef(parts, r.defDB)
	}
	// 字段列表
	var cols []string
	hasList := false
	if i < n && r.toks[i].isPunct("(") && !(i+1 < n && r.toks[i+1].isWord("select")) {
		hasList = true
		d := r.toks[i].depth + 1
		j := i + 1
		for j < n && r.toks[j].depth >= d {
			if r.toks[j].depth == d && r.toks[j].isName() {
				cols = append(cols, r.toks[j].text)
			}
			j++
		}
		i = j + 1 // 跳过 ')'
	}
	for i < n && !r.toks[i].isWord("values") && !r.toks[i].isWord("value") {
		if r.toks[i].isWord("select") || r.toks[i].isWord("set") {
			return
		}
		i++
	}
	if i >= n {
		return
	}
	i++ // VALUES
	refs := []tref{ref}
	for i < n {
		if !r.toks[i].isPunct("(") {
			i++
			continue
		}
		d := r.toks[i].depth + 1
		start := i + 1
		j := start
		pos := 0
		segStart := start
		flush := func(end int) {
			if hasList {
				if pos < len(cols) {
					if rule, hit := r.m.resolve("", cols[pos], refs); hit {
						r.maskRange(segStart, end, rule)
					}
				}
			} else if r.m.tableConfigured(ref) {
				r.maskRange(segStart, end, Rule{Kind: KindWhole})
			}
		}
		for j < n && r.toks[j].depth >= d {
			if r.toks[j].depth == d && r.toks[j].isPunct(",") {
				flush(j)
				pos++
				segStart = j + 1
			}
			j++
		}
		flush(j)
		i = j + 1
	}
}

// mentions 判断语句里是否出现了命中配置的字段名（不论出现在哪里）。
func (r *stmtRun) mentions() bool {
	for _, t := range r.toks {
		if !t.isName() {
			continue
		}
		if _, ok := r.m.resolve("", t.text, r.refs); ok {
			return true
		}
	}
	return false
}

// ---- 入口 ----

// Mask 返回遮盖后的 SQL，以及被遮盖的原值（去重）。没有命中时原样返回、第二个返回值为空。
func (m *StmtMasker) Mask(sql string) (string, []string) {
	if !m.Enabled() || strings.TrimSpace(sql) == "" {
		return sql, nil
	}
	toks := scanSQL(sql, m.d)
	var edits []edit
	defDB := m.defDB
	for _, st := range splitStatements(sql, toks, m.d) {
		if len(st) == 0 {
			continue
		}
		if st[0].isWord("use") && len(st) > 1 && st[1].isName() {
			defDB = st[1].text
			continue
		}
		run := &stmtRun{m: m, sql: sql, toks: st, defDB: defDB, edits: map[int]edit{}}
		run.refs = m.tableRefs(st, defDB)
		run.insert()
		run.generic()
		if run.masked == 0 && run.seen == 0 && run.mentions() {
			// 出现了敏感字段名却没认出任何“字段 = 值”：这条语句里所有字符串和数字字面量都遮盖
			for i := range st {
				run.maskTok(i, Rule{Kind: KindWhole})
			}
		}
		for _, e := range run.edits {
			edits = append(edits, e)
		}
	}
	if len(edits) == 0 {
		return sql, nil
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var b strings.Builder
	last := 0
	var secrets []string
	seen := map[string]bool{}
	for _, e := range edits {
		if e.start < last {
			continue
		}
		b.WriteString(sql[last:e.start])
		b.WriteString(e.repl)
		last = e.end
		if len([]rune(e.secret)) >= 3 && !seen[e.secret] {
			seen[e.secret] = true
			secrets = append(secrets, e.secret)
		}
	}
	b.WriteString(sql[last:])
	return b.String(), secrets
}

// Scrub 把文本里出现的、已被遮盖的原值换成 ***（错误信息常回显值，如 Duplicate entry '…'）。
func Scrub(text string, secrets []string) string {
	for _, s := range secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, Mask)
		}
	}
	return text
}

// splitStatements 按顶层分号（SQL Server 还有单独一行的 GO）把词法单元切成语句。
func splitStatements(sql string, toks []sTok, d Dialect) [][]sTok {
	var out [][]sTok
	start := 0
	for i, t := range toks {
		cut := false
		if t.depth == 0 && t.isPunct(";") {
			cut = true
		} else if d == DialectMSSQL && t.kind == tIdent && t.lower == "go" && onOwnLine(sql, t.start, t.end) {
			cut = true
		}
		if cut {
			out = append(out, toks[start:i])
			start = i + 1
		}
	}
	if start < len(toks) {
		out = append(out, toks[start:])
	}
	return out
}

func onOwnLine(sql string, s, e int) bool {
	for i := s - 1; i >= 0 && sql[i] != '\n'; i-- {
		if sql[i] != ' ' && sql[i] != '\t' && sql[i] != '\r' {
			return false
		}
	}
	for i := e; i < len(sql) && sql[i] != '\n'; i++ {
		if sql[i] != ' ' && sql[i] != '\t' && sql[i] != '\r' {
			return false
		}
	}
	return true
}
