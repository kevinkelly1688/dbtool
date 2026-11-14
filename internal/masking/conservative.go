// @author: kevinkelly
// @license: Apache Licence
// @file: conservative.go
// @time: 2026/10/28

package masking

import (
	"sort"
	"strings"
	"unicode"
)

// 保守匹配：引擎拿不到“结果列来自哪个字段”时（ClickHouse、SelectDB / Doris，以及 goInception 不可用 / SQL Server 无法静态推断结果集时的降级），
// 只根据语句文本判断哪些结果列要脱敏。原则是宁多勿漏：
//
//  1. 结果列名（或别名）与某个已配置的字段名相同，就脱敏；
//  2. 该列的表达式里引用了已配置的字段名（concat(phone, ''), substr(phone, 1, 3) AS p），就脱敏；
//  3. 上面两类列的别名视为“已被污染”的名字，外层语句 / 子查询 / CTE 再引用它也脱敏（WITH t AS (SELECT phone AS p ...) SELECT p ...）；
//  4. 该列的表达式里调用了非内置函数（存储函数 / 自定义函数），或取自 FROM 里的自定义表值函数：值可能来自函数体里读到的任何字段，
//     语句文本看不出来，整列按配置里最严的一条脱敏，别名同样视为“已被污染”（见 udf.go）；
//  5. 语句用到视图时，视图定义里的别名、表达式、嵌套引用、函数调用同样把污染传给视图的输出列（ConservativeHitsViews，见 viewexp.go）。
//
// 配置不区分库表：只要同名字段在任何库表里被配置过，本实例上所有同名列都会被脱敏，多个配置规则不同时取最严的一条。
// 代价是偶尔误伤，但不会因为改了别名或套了函数就漏掉。

// Dialect 决定语句里的注释、引号风格。
type Dialect int

const (
	DialectMySQL Dialect = iota // MySQL 系、SelectDB、Doris、ADB
	DialectMSSQL                // SQL Server：[标识符]、alias = expr
	DialectPostgres
	DialectClickHouse
	DialectOracle // Oracle：双引号标识符、反斜杠不是转义、没加引号的名字折成大写
	DialectDM     // 达梦：词法规则与 Oracle 相同（两种引擎各自独立，这里分开定义是为了以后可以各走各的）
)

// isOracleFamily 判断是不是 Oracle 系方言（Oracle、达梦）：词法与 PostgreSQL 相近，但没加引号的名字折成大写。
func isOracleFamily(d Dialect) bool { return d == DialectOracle || d == DialectDM }

type tokKind uint8

const (
	tIdent tokKind = iota
	tQuoted
	tString
	tNumber
	tPunct
)

type token struct {
	kind  tokKind
	text  string // 标识符已去引号
	lower string
	depth int // 所在括号层级；括号本身的层级与其内容外一层相同
}

func isIdentStart(r rune, d Dialect) bool {
	return r == '_' || r == '@' || r == '$' || unicode.IsLetter(r) || (r == '#' && d == DialectMSSQL)
}

func isIdentPart(r rune) bool {
	return r == '_' || r == '$' || r == '@' || r == '#' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// tokenize 做最小限度的词法切分：注释、字符串、引号标识符、括号层级。不会 panic，遇到不完整的引号就吃到末尾。
func tokenize(sql string, d Dialect) []token {
	out, _ := tokenizeEx(sql, d)
	return out
}

// tokenizeEx 同 tokenize，另外报告文本是否“干净”：字符串、引号标识符、块注释都有结束符。
// 不干净时后面的内容已经被吃进了那个字符串 / 注释里，拼在它后面的文本也就看不到了。
func tokenizeEx(sql string, d Dialect) (out []token, clean bool) {
	clean = true
	rs := []rune(sql)
	n := len(rs)
	depth := 0
	add := func(k tokKind, text string, dp int) {
		out = append(out, token{kind: k, text: text, lower: strings.ToLower(text), depth: dp})
	}
	for i := 0; i < n; {
		c := rs[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '-' && i+1 < n && rs[i+1] == '-':
			for i < n && rs[i] != '\n' {
				i++
			}
		case c == '#' && (d == DialectMySQL || d == DialectClickHouse):
			for i < n && rs[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && rs[i+1] == '*':
			i += 2
			clean = clean && strings.Contains(string(rs[i:]), "*/")
			for i+1 < n && !(rs[i] == '*' && rs[i+1] == '/') {
				i++
			}
			i += 2
		case c == '\'':
			j := i + 1
			var b strings.Builder
			for j < n {
				if rs[j] == '\\' && d != DialectMSSQL && d != DialectPostgres && !isOracleFamily(d) && j+1 < n {
					b.WriteRune(rs[j+1])
					j += 2
					continue
				}
				if rs[j] == '\'' {
					if j+1 < n && rs[j+1] == '\'' {
						b.WriteRune('\'')
						j += 2
						continue
					}
					break
				}
				b.WriteRune(rs[j])
				j++
			}
			clean = clean && j < n
			add(tString, b.String(), depth)
			i = j + 1
		case c == '`' || c == '"':
			j := i + 1
			var b strings.Builder
			for j < n {
				if rs[j] == c {
					if j+1 < n && rs[j+1] == c {
						b.WriteRune(c)
						j += 2
						continue
					}
					break
				}
				b.WriteRune(rs[j])
				j++
			}
			clean = clean && j < n
			add(tQuoted, b.String(), depth)
			i = j + 1
		case c == '[' && d == DialectMSSQL:
			j := i + 1
			var b strings.Builder
			for j < n {
				if rs[j] == ']' {
					if j+1 < n && rs[j+1] == ']' {
						b.WriteRune(']')
						j += 2
						continue
					}
					break
				}
				b.WriteRune(rs[j])
				j++
			}
			clean = clean && j < n
			add(tQuoted, b.String(), depth)
			i = j + 1
		case isIdentStart(c, d):
			j := i + 1
			for j < n && isIdentPart(rs[j]) {
				j++
			}
			add(tIdent, string(rs[i:j]), depth)
			i = j
		case unicode.IsDigit(c):
			j := i + 1
			for j < n && (unicode.IsDigit(rs[j]) || rs[j] == '.' || rs[j] == 'e' || rs[j] == 'E') {
				j++
			}
			add(tNumber, string(rs[i:j]), depth)
			i = j
		default:
			s := string(c)
			switch {
			case c == '(' || (c == '[' && d != DialectMSSQL):
				add(tPunct, s, depth)
				depth++
			case c == ')' || (c == ']' && d != DialectMSSQL):
				if depth > 0 {
					depth--
				}
				add(tPunct, s, depth)
			default:
				add(tPunct, s, depth)
			}
			i++
		}
	}
	return out, clean
}

// 结束 SELECT 列表的子句关键字。
var listEnd = map[string]bool{
	"from": true, "into": true, "where": true, "group": true, "having": true, "order": true, "limit": true,
	"union": true, "intersect": true, "except": true, "minus": true, "window": true, "fetch": true, "offset": true,
	"for": true, "format": true, "settings": true, "qualify": true, "prewhere": true, "option": true,
}

// 不能当别名用的结尾关键字。
var notAlias = map[string]bool{
	"end": true, "null": true, "true": true, "false": true, "asc": true, "desc": true, "current_timestamp": true,
	"current_date": true, "current_time": true, "current_user": true, "and": true, "or": true, "not": true, "is": true,
	"in": true, "like": true, "between": true, "then": true, "else": true, "when": true, "case": true, "distinct": true,
	"collate": true, "over": true, "interval": true,
}

type selItem struct {
	refs []string // 表达式里引用的标识符（小写，已去掉函数名与限定符）
	out  string   // 该项的输出名：别名，或单个（限定）列名；表达式且无别名时为空
	star bool
	// udf：表达式里调用了非内置函数（存储函数 / 自定义函数），或取自 FROM 里的自定义表值函数。值可能来自函数体里读到的任何字段，
	// 语句文本看不出来，按配置里最严的一条脱敏（见 udf.go）。
	udf      bool
	chains   [][]string // 表达式里的字段引用（小写，a、u.phone、db.u.phone），判断是否取自表值函数用
	starQual string     // `别名.*` 的别名；裸 `*` 为空
}

type selList struct {
	depth   int
	items   []selItem
	star    bool
	udfStar bool // 有一个 `*` 取自自定义表值函数
}

func isNameTok(t token) bool { return t.kind == tIdent || t.kind == tQuoted }

// refsOf 取表达式里引用的标识符（小写，已去掉函数名与限定符）。
func refsOf(expr []token) []string {
	var refs []string
	for i, t := range expr {
		if !isNameTok(t) {
			continue
		}
		if i+1 < len(expr) && expr[i+1].kind == tPunct && (expr[i+1].text == "(" || expr[i+1].text == ".") {
			continue // 函数名 / 限定符
		}
		refs = append(refs, t.lower)
	}
	return refs
}

// stmtStart 是 T-SQL 里（括号外）能开始新语句的关键字：没写分号的批处理靠它们判断上一条语句到哪里结束。
var stmtStart = map[string]bool{"declare": true, "set": true, "select": true, "if": true, "print": true, "throw": true,
	"begin": true, "return": true, "insert": true, "update": true, "delete": true}

// varAssignments 找出 T-SQL 的变量赋值：`DECLARE @v 类型 = 表达式`（可一次声明多个）和 `SET @v = 表达式`（含 += 等）。
// 变量会把敏感字段的值带进后面的结果列（`DECLARE @k = (SELECT PrivateKey …); SELECT @k AS x`），
// 所以要像别名一样把污染传给变量名。`SELECT @v = 表达式` 本身就是 select 列表里的 `别名 = 表达式`，不用在这里处理。
func varAssignments(toks []token) []selItem {
	var out []selItem
	stop := func(j, dp int) bool { // toks[j] 是否结束 dp 层上的当前语句
		tk := toks[j]
		if tk.depth < dp {
			return true
		}
		if tk.depth > dp {
			return false
		}
		if tk.kind == tPunct && tk.text == ";" {
			return true
		}
		return tk.kind == tIdent && stmtStart[tk.lower]
	}
	isVar := func(t token) bool { return t.kind == tIdent && strings.HasPrefix(t.text, "@") }
	punct := func(t token, dp int, texts ...string) bool {
		if t.kind != tPunct || t.depth != dp {
			return false
		}
		for _, s := range texts {
			if t.text == s {
				return true
			}
		}
		return false
	}
	// 从 from 开始取一个表达式（到同层的逗号或语句结束为止），返回结束位置
	exprEnd := func(from, dp int, comma bool) int {
		e := from
		for e < len(toks) && !stop(e, dp) && !(comma && punct(toks[e], dp, ",")) {
			e++
		}
		return e
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind != tIdent {
			continue
		}
		dp := t.depth
		switch t.lower {
		case "declare":
			j := i + 1
			for j < len(toks) && !stop(j, dp) {
				if !isVar(toks[j]) || toks[j].depth != dp {
					j++
					continue
				}
				name := toks[j].lower
				k := j + 1 // 数据类型一直到 = 或 ,
				for k < len(toks) && !stop(k, dp) && !punct(toks[k], dp, "=", ",") {
					k++
				}
				if k < len(toks) && !stop(k, dp) && punct(toks[k], dp, "=") {
					e := exprEnd(k+1, dp, true)
					out = append(out, selItem{out: name, refs: refsOf(toks[k+1 : e]), udf: hasUserFunc(toks[k+1:e], DialectMSSQL)})
					j = e
				} else {
					j = k
				}
			}
		case "set":
			if i+1 >= len(toks) || !isVar(toks[i+1]) || toks[i+1].depth != dp {
				continue
			}
			k := i + 2 // @v 之后是 = 或 += -= 等
			for k < len(toks) && k <= i+3 && !punct(toks[k], dp, "=") {
				k++
			}
			if k < len(toks) && punct(toks[k], dp, "=") {
				e := exprEnd(k+1, dp, false)
				out = append(out, selItem{out: toks[i+1].lower, refs: refsOf(toks[k+1 : e]), udf: hasUserFunc(toks[k+1:e], DialectMSSQL)})
			}
		}
	}
	return out
}

// tableDefKeyword 是 DECLARE @t TABLE (…) 里不是列定义的元素开头词（表级约束、索引）。
var tableDefKeyword = map[string]bool{"constraint": true, "primary": true, "unique": true, "check": true, "index": true, "foreign": true}

// tableVarInserts 找出 T-SQL 往表变量写数据的语句 `INSERT [INTO] @t [(列, …)] SELECT … / VALUES (…)`。
// 表变量和变量一样会把敏感字段的值带进后面的结果列（`DECLARE @t TABLE (x varchar(20)); INSERT INTO @t SELECT phone FROM u; SELECT x FROM @t`），
// 列名对不上敏感字段名，只看结果列名就漏了。这里不按位置对应：写入的值里引用了敏感字段，这个表变量的所有列名
// （DECLARE … TABLE 里声明的、INSERT 括号里写的）都算被污染。写入的来源里有 * 时不知道带进了哪些字段，star 置位，由调用方按配置里最严的一条处理。
// 宁多勿漏：同一个表变量里不敏感的列偶尔会被一起脱敏。
func tableVarInserts(toks []token) []selItem {
	isVar := func(t token) bool { return t.kind == tIdent && strings.HasPrefix(t.text, "@") }
	isPunct := func(t token, s string) bool { return t.kind == tPunct && t.text == s }

	// 表变量 → DECLARE 里声明的列名
	declared := map[string][]string{}
	for i := 0; i+3 < len(toks); i++ {
		if toks[i].kind != tIdent || toks[i].lower != "declare" || !isVar(toks[i+1]) {
			continue
		}
		j := i + 2
		if toks[j].kind == tIdent && toks[j].lower == "as" {
			j++
		}
		if j+1 >= len(toks) || toks[j].kind != tIdent || toks[j].lower != "table" || !isPunct(toks[j+1], "(") {
			continue
		}
		dp := toks[j+1].depth // 括号本身与外层同一层，括号里面是 dp+1
		name := toks[i+1].lower
		atStart := true
		for k := j + 2; k < len(toks); k++ {
			tk := toks[k]
			if tk.depth == dp && isPunct(tk, ")") {
				break
			}
			if tk.depth != dp+1 {
				continue
			}
			if isPunct(tk, ",") {
				atStart = true
				continue
			}
			if atStart && isNameTok(tk) && !tableDefKeyword[tk.lower] {
				declared[name] = append(declared[name], tk.lower)
			}
			atStart = false
		}
	}

	var out []selItem
	for i, t := range toks {
		if t.kind != tIdent || t.lower != "insert" {
			continue
		}
		dp := t.depth
		j := i + 1
		if j < len(toks) && toks[j].kind == tIdent && toks[j].lower == "into" {
			j++
		}
		if j >= len(toks) || !isVar(toks[j]) || toks[j].depth != dp {
			continue
		}
		names := append([]string(nil), declared[toks[j].lower]...)
		j++
		if j < len(toks) && isPunct(toks[j], "(") { // 列清单
			for j++; j < len(toks) && !(toks[j].depth == dp && isPunct(toks[j], ")")); j++ {
				if isNameTok(toks[j]) {
					names = append(names, toks[j].lower)
				}
			}
			j = min(j+1, len(toks))
		}
		// 写入的值：到语句结束为止。INSERT 自己的 SELECT 和 UNION 分支算在里面，没有分号直接跟着的下一条 SELECT 不算
		sawSelect := false
		end := j
		for ; end < len(toks); end++ {
			tk := toks[end]
			if tk.depth < dp || (tk.depth == dp && isPunct(tk, ";")) {
				break
			}
			if tk.depth != dp || tk.kind != tIdent {
				continue
			}
			if stmtStart[tk.lower] && tk.lower != "select" {
				break
			}
			if tk.lower == "select" {
				if prev := toks[end-1]; sawSelect && !(prev.kind == tIdent && (prev.lower == "union" || prev.lower == "all" || prev.lower == "intersect" || prev.lower == "except")) {
					break
				}
				sawSelect = true
			}
		}
		var refs []string
		star, udf := false, false
		lists := selectLists(toks[j:end], DialectMSSQL)
		for _, sl := range lists {
			star = star || sl.star
			for _, it := range sl.items {
				refs = append(refs, it.refs...)
				udf = udf || it.udf
			}
		}
		if len(lists) == 0 { // VALUES (…)
			refs = refsOf(toks[j:end])
			udf = hasUserFunc(toks[j:end], DialectMSSQL)
		}
		for _, n := range names {
			out = append(out, selItem{out: n, refs: refs, star: star, udf: udf})
		}
	}
	return out
}

// userVarAssigns 找出 MySQL 的用户变量赋值，污染要像别名一样传给变量名：
//   - 任何位置的 `@v := 表达式`（含 SELECT 列表里的 `SELECT @v := phone, @v`）；
//   - SET 语句里的 `@v = 表达式`；一条 SET 可以用逗号给多个变量赋值（SET @a = 1, @b = (SELECT phone …)）。
//
// 表达式到同层的逗号 / 分号 / 子句关键字（from、where …）或上层括号结束；多取一点只会多脱敏。
// 在线查询允许“前置 SET + 查询”之后，变量是把敏感字段的值带进结果列的另一条路，必须和别名一样处理。
func userVarAssigns(toks []token) []selItem {
	isUserVar := func(t token) bool {
		return t.kind == tIdent && len(t.text) > 1 && t.text[0] == '@' && t.text[1] != '@'
	}
	punct := func(j int, s string) bool { return j < len(toks) && toks[j].kind == tPunct && toks[j].text == s }
	first := make([]int, len(toks)) // 每个 token 所在语句的第一个 token
	cur := 0
	for i, t := range toks {
		first[i] = cur
		if t.kind == tPunct && t.text == ";" {
			cur = i + 1
		}
	}
	end := func(from, dp int) int {
		e := from
		for e < len(toks) {
			tk := toks[e]
			if tk.depth < dp {
				break
			}
			if tk.depth == dp {
				if tk.kind == tPunct && (tk.text == "," || tk.text == ";") {
					break
				}
				if tk.kind == tIdent && listEnd[tk.lower] && !notListEnd(toks, e) {
					break
				}
			}
			e++
		}
		return e
	}
	var out []selItem
	for i, t := range toks {
		if !isUserVar(t) {
			continue
		}
		switch {
		case punct(i+1, ":") && punct(i+2, "="):
			e := end(i+3, t.depth)
			out = append(out, selItem{out: t.lower, refs: refsOf(toks[i+3 : e]), udf: hasUserFunc(toks[i+3:e], DialectMySQL)})
		case punct(i+1, "="):
			f := first[i]
			if f < len(toks) && toks[f].kind == tIdent && toks[f].lower == "set" && toks[f].depth == t.depth {
				e := end(i+2, t.depth)
				out = append(out, selItem{out: t.lower, refs: refsOf(toks[i+2 : e]), udf: hasUserFunc(toks[i+2:e], DialectMySQL)})
			}
		}
	}
	return out
}

func analyzeItem(toks []token, d Dialect) selItem {
	var it selItem
	if len(toks) == 0 {
		return it
	}
	last := toks[len(toks)-1]
	if last.kind == tPunct && last.text == "*" && (len(toks) == 1 || (toks[len(toks)-2].kind == tPunct && toks[len(toks)-2].text == ".")) {
		it.star = true
		if n := len(toks); n >= 3 && isNameTok(toks[n-3]) {
			it.starQual = toks[n-3].lower
		}
		return it
	}
	expr := toks
	switch {
	case d == DialectMSSQL && len(toks) > 2 && isNameTok(toks[0]) && toks[1].kind == tPunct && toks[1].text == "=":
		it.out = toks[0].lower
		expr = toks[2:]
	case d == DialectMSSQL && len(toks) > 3 && isNameTok(toks[0]) && toks[1].kind == tPunct && len(toks[1].text) == 1 && strings.Contains("+-*/%&|^", toks[1].text) &&
		toks[2].kind == tPunct && toks[2].text == "=":
		// `@s += 表达式`（-= *= /= %= &= |= ^= 同理）：和 `@s = 表达式` 一样是给变量赋值（字符串拼接聚合的常见写法），污染要传给变量
		it.out = toks[0].lower
		expr = toks[3:]
	case len(toks) >= 3 && toks[len(toks)-2].kind == tIdent && toks[len(toks)-2].lower == "as" && (isNameTok(last) || last.kind == tString):
		it.out = last.lower
		expr = toks[:len(toks)-2]
	case len(toks) >= 2 && (isNameTok(last) || last.kind == tString) && !(last.kind == tIdent && notAlias[last.lower]):
		prev := toks[len(toks)-2]
		switch {
		case prev.kind == tPunct && prev.text == ")", prev.kind == tNumber, prev.kind == tString, prev.kind == tQuoted:
			it.out = last.lower
			expr = toks[:len(toks)-1]
		case prev.kind == tIdent && !notAlias[prev.lower] || prev.kind == tIdent && prev.lower == "end":
			it.out = last.lower
			expr = toks[:len(toks)-1]
		}
	}
	it.refs = refsOf(expr)
	it.chains = nameChains(expr)
	it.udf = hasUserFunc(expr, d)
	if it.out == "" && len(expr) > 0 {
		// 单个（限定）列：输出名就是最后一段
		simple := true
		for i, t := range expr {
			if i%2 == 0 && !isNameTok(t) || i%2 == 1 && !(t.kind == tPunct && t.text == ".") {
				simple = false
				break
			}
		}
		if simple && len(expr)%2 == 1 {
			it.out = expr[len(expr)-1].lower
		}
	}
	return it
}

// notListEnd 排除两种“长得像子句关键字”的写法，否则列表会在这里被截断、后面的列（和别名）全部漏掉：
//   - `… WITHIN GROUP (ORDER BY …)`：T-SQL / PostgreSQL / Oracle 的有序集合聚合（STRING_AGG、LISTAGG、PERCENTILE_CONT），GROUP 不是 GROUP BY；
//   - `FORMAT(…)`：MySQL / SQL Server 的函数；ClickHouse 的 `FORMAT JSON` 后面不是括号，仍然结束列表。
func notListEnd(toks []token, j int) bool {
	switch toks[j].lower {
	case "group":
		return j > 0 && toks[j-1].kind == tIdent && toks[j-1].lower == "within"
	case "format":
		return j+1 < len(toks) && toks[j+1].kind == tPunct && toks[j+1].text == "("
	}
	return false
}

// selectLists 找出语句里所有 SELECT 的列表（含子查询、CTE、UNION 的各个分支）。
func selectLists(toks []token, d Dialect) []selList {
	var lists []selList
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind != tIdent || t.lower != "select" {
			continue
		}
		dp := t.depth
		j := i + 1
		// 修饰词：DISTINCT / ALL / TOP n / TOP (n) [PERCENT] [WITH TIES] / SQL_CALC_FOUND_ROWS
		for j < len(toks) && toks[j].kind == tIdent {
			switch toks[j].lower {
			case "distinct", "all", "sql_calc_found_rows", "sql_no_cache", "straight_join", "high_priority":
				j++
				continue
			case "top":
				j++
				if j < len(toks) && toks[j].kind == tPunct && toks[j].text == "(" {
					for j < len(toks) && !(toks[j].kind == tPunct && toks[j].text == ")" && toks[j].depth == dp) {
						j++
					}
					j++
				} else if j < len(toks) {
					j++
				}
				for j < len(toks) && toks[j].kind == tIdent && (toks[j].lower == "percent" || toks[j].lower == "with" || toks[j].lower == "ties") {
					j++
				}
				continue
			}
			break
		}
		var cur []token
		sl := selList{depth: dp}
		flush := func() {
			if len(cur) > 0 {
				it := analyzeItem(cur, d)
				sl.items = append(sl.items, it)
				if it.star {
					sl.star = true
				}
				cur = nil
			}
		}
		for ; j < len(toks); j++ {
			tk := toks[j]
			if tk.depth < dp {
				break
			}
			if tk.depth == dp {
				if tk.kind == tIdent && listEnd[tk.lower] && !notListEnd(toks, j) {
					break
				}
				if tk.kind == tPunct && tk.text == ";" {
					break
				}
				if tk.kind == tPunct && tk.text == "," {
					flush()
					continue
				}
			}
			cur = append(cur, tk)
		}
		flush()
		// FROM 里的自定义表值函数：取自它的结果列（和 *）同样按最严规则脱敏
		if tv := tvfSources(toks, j, dp, d); len(tv) > 0 {
			for k := range sl.items {
				if it := &sl.items[k]; it.touchesTVF(tv) {
					it.udf = true
					if it.star {
						sl.udfStar = true
					}
				}
			}
		}
		lists = append(lists, sl)
	}
	return lists
}

func (c Column) strictness() int {
	if c.Rule != nil {
		return c.Rule.Strictness()
	}
	return 300
}

// ConservativeHits 返回要脱敏的结果列（见文件头的匹配规则）。configured 为空或 columns 为空时返回 nil。
func ConservativeHits(sql string, columns []string, configured []Column, d Dialect) []Hit {
	return ConservativeHitsViews(sql, columns, configured, d, nil)
}

// ConservativeHitsViews 同 ConservativeHits；views 是语句里用到的视图的查询体（见 viewexp.go）：
// 视图就是一条存起来的子查询，它的别名、表达式、嵌套引用和自定义函数调用同样会把污染传给视图的输出列，
// 外层语句再引用这些列就跟着脱敏。查询体和语句各自词法切分，一个视图定义有问题不会影响语句本身的分析。
func ConservativeHitsViews(sql string, columns []string, configured []Column, d Dialect, views []ViewBody) []Hit {
	if len(columns) == 0 || len(configured) == 0 {
		return nil
	}
	// 字段名 → 最严的一条配置
	taint := map[string]*Column{}
	for i := range configured {
		c := &configured[i]
		name := strings.ToLower(c.Column)
		if name == "" {
			continue
		}
		// 路径型字段（Mongo）不会出现在 SQL 引擎里；带点号时取最后一段
		if k := strings.LastIndex(name, "."); k >= 0 {
			name = name[k+1:]
		}
		if cur, ok := taint[name]; !ok || c.strictness() > cur.strictness() {
			taint[name] = c
		}
	}
	toks := tokenize(sql, d)
	lists := selectLists(toks, d)

	strictest := func(refs []string) *Column {
		var best *Column
		for _, r := range refs {
			if c, ok := taint[r]; ok && (best == nil || c.strictness() > best.strictness()) {
				best = c
			}
		}
		return best
	}
	// 全部配置里最严的一条：非内置函数（udf.go）、`*` 拿不准带进了哪些字段时用它
	var all *Column
	for i := range configured {
		if c := &configured[i]; strings.TrimSpace(c.Column) != "" && (all == nil || c.strictness() > all.strictness()) {
			all = c
		}
	}
	stricter := func(a, b *Column) *Column {
		if a == nil || (b != nil && b.strictness() > a.strictness()) {
			return b
		}
		return a
	}
	srcOf := func(it selItem) *Column {
		src := strictest(it.refs)
		if it.udf {
			src = stricter(src, all)
		}
		return src
	}
	// 污染传播：引用了敏感字段的项，它的输出名也算敏感名字；T-SQL 的变量赋值同理（变量名当别名）
	var propagate []selItem
	viewWild := false // 某个视图的输出列名不可知（select * from 自定义表值函数）：整个结果都按最严规则脱敏
	for _, vb := range views {
		items, wild := vb.items(d)
		propagate = append(propagate, items...)
		viewWild = viewWild || wild
	}
	for _, sl := range lists {
		propagate = append(propagate, sl.items...)
	}
	switch d {
	case DialectMSSQL:
		propagate = append(propagate, varAssignments(toks)...)
		propagate = append(propagate, tableVarInserts(toks)...)
	case DialectMySQL:
		propagate = append(propagate, userVarAssigns(toks)...)
	case DialectClickHouse:
		propagate = append(propagate, withAliasItems(toks, d)...)
	}
	for round := 0; round < 8; round++ {
		changed := false
		for _, it := range propagate {
			if it.out == "" {
				continue
			}
			src := srcOf(it)
			if it.star { // 表变量的写入来源里有 *：不知道带进了哪些字段，按所有配置里最严的一条
				src = stricter(src, all)
			}
			if src != nil {
				if cur, ok := taint[it.out]; !ok || src.strictness() > cur.strictness() {
					taint[it.out] = src
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}

	hit := map[int]*Column{}
	for j, name := range columns {
		if c, ok := taint[strings.ToLower(strings.TrimSpace(name))]; ok {
			hit[j] = c
		}
	}
	mark := func(j int, src *Column) {
		if src == nil || j < 0 || j >= len(columns) {
			return
		}
		if cur, ok := hit[j]; !ok || src.strictness() > cur.strictness() {
			hit[j] = src
		}
	}
	if viewWild {
		for j := range columns {
			mark(j, all)
		}
	}
	// 最外层 SELECT 的位置对应：按位置判断表达式列。
	// 没有 * 时项数要与结果列数一致；有 * 时 * 展开的列数不定，但 * 之前的项从头数、最后一个 * 之后的项从尾数，位置是确定的，
	// * 到最后一个 * 之间（含）的列算一个区域，区域里任何一项命中就整个区域脱敏。
	if len(lists) > 0 {
		minDepth := lists[0].depth
		for _, sl := range lists {
			if sl.depth < minDepth {
				minDepth = sl.depth
			}
		}
		for _, sl := range lists {
			if sl.depth > minDepth && sl.udfStar {
				// 内层的 `select * from 自定义表值函数(…)`：外层拿到的列名未知，整个结果都按最严规则脱敏
				for j := range columns {
					mark(j, all)
				}
			}
			if sl.depth != minDepth {
				continue
			}
			first, last := -1, -1
			for i, it := range sl.items {
				if it.star {
					if first < 0 {
						first = i
					}
					last = i
				}
			}
			if first < 0 {
				if len(sl.items) != len(columns) {
					continue
				}
				for j, it := range sl.items {
					mark(j, srcOf(it))
				}
				continue
			}
			head, tail := sl.items[:first], sl.items[last+1:]
			if len(head)+len(tail) > len(columns) {
				continue
			}
			for j, it := range head {
				mark(j, srcOf(it))
			}
			for k, it := range tail {
				mark(len(columns)-len(tail)+k, srcOf(it))
			}
			var region *Column
			for _, it := range sl.items[first : last+1] {
				region = stricter(region, srcOf(it))
			}
			for j := len(head); j < len(columns)-len(tail); j++ {
				mark(j, region)
			}
		}
	}
	idx := make([]int, 0, len(hit))
	for j := range hit {
		idx = append(idx, j)
	}
	sort.Ints(idx)
	out := make([]Hit, 0, len(idx))
	for _, j := range idx {
		c := hit[j]
		out = append(out, Hit{Index: j, Column: strings.ToLower(c.Column), RuleType: c.RuleType, Rule: c.Rule})
	}
	return out
}
