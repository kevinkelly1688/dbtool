// @author: kevinkelly
// @license: Apache Licence
// @file: check.go
// @time: 2026/10/28

package oracle

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"dbtool/internal/sqlx"
)

// 在线查询语句的静态检查（第一层防护，不连服务端）。默认拒绝：
//
//  1. 词法合法，且恰好一条语句（注释、字符串、引号标识符、q'[…]' 都按 Oracle 规则整体识别）；
//  2. 语句类别在白名单内：SELECT / WITH … SELECT / EXPLAIN [PLAN FOR] <查询>；
//  3. 没有写操作关键字、SELECT … INTO、FOR UPDATE、WITH FUNCTION、序列取值、数据库链接（@）、绑定变量、JDBC 转义；
//  4. 所有函数调用都是内置函数白名单里的函数——自定义函数、包里的函数、对象方法一律拒绝
//     （函数按定义者权限执行，调用别人 schema 里的存储函数等于借用对方的权限，可以绕过只读账号和库表授权）；
//  5. 不访问系统 schema、DBA_* / V$* 等内部视图；数据字典里只开放 dictViews 里的几个（元数据，免库表授权）。
//
// 通过检查后抽取语句涉及的 schema.表（供上层做库表级授权）。真正的只读保证来自执行层与目标库账号：
// 事务内 SET TRANSACTION READ ONLY，并且要求监控账号只授予 SELECT（见 run.go 与帮助文档）。

// DBType 是 sql_instance.db_type 里 Oracle 的取值。
const DBType = "oracle"

// UserError 是语句检查 / 探测不通过的业务原因，原文可直接展示给用户（上层映射成 bad_query）。
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func uerr(format string, a ...any) error { return &UserError{Msg: fmt.Sprintf(format, a...)} }

// MaxQueryBytes 是语句文本的长度上限。
const MaxQueryBytes = 128 << 10

// maxTablesPerQuery 限制一条语句涉及的表数量（执行前要逐个核对存在性）。
const maxTablesPerQuery = 200

// 语句类别。
const (
	KindSelect  = "select"  // SELECT / WITH … SELECT
	KindExplain = "explain" // EXPLAIN [PLAN FOR] <查询>
)

// CheckOptions 是检查选项。
type CheckOptions struct {
	// DB 是执行时的当前 schema（ALTER SESSION SET CURRENT_SCHEMA）。语句里没写 schema 的表按它补全；必须是库里的真实名字。
	DB string
}

// Checked 是通过静态检查的语句。
type Checked struct {
	// Kind 是语句类别：select / explain。
	Kind string
	// Stmt 是要发给服务端的文本：去掉了首尾注释与末尾分号，内部原样保留；EXPLAIN 时是内层的查询。
	Stmt string
	// DB 是执行时的当前 schema。
	DB string
	// Tables 是语句涉及、需要库表授权的表 / 视图（Schema=owner，Name=表名；都是库里的真实名字，不含 CTE 别名与免授权的数据字典视图）。
	Tables []sqlx.TableRef
	// DictRefs 是语句里没写 schema 的数据字典视图（免授权）。执行前要核对当前 schema 里没有同名的对象（否则查到的是别人的表）。
	DictRefs []string
	// CallNames 是语句里调用的内置函数和非保留字关键字（大写、去重、排序）。执行前要核对 schema / PUBLIC 里没有同名的自定义函数 / 包 / 同义词
	// （否则它们会盖过内置函数）。
	CallNames []string
	// BareNames 是语句里不带点、不带括号、不是表名 / 别名 / 关键字的名字（列名，或不带括号调用的无参函数；排序）。
	// 执行前要核对 schema / PUBLIC 里没有同名的函数（否则 SELECT 自定义函数 FROM dual 就能不带括号调用它）。
	BareNames []string
}

type tref struct{ owner, name string }

// CheckUserQuery 检查并整理一条用户输入的语句。不通过返回 *UserError。
func CheckUserQuery(text string, opts ...CheckOptions) (*Checked, error) {
	var o CheckOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if strings.TrimSpace(text) == "" {
		return nil, uerr("SQL 为空")
	}
	if len(text) > MaxQueryBytes {
		return nil, uerr("SQL 过长（上限 %d KB）", MaxQueryBytes>>10)
	}
	if !utf8.ValidString(text) {
		return nil, uerr("SQL 含有非法的 UTF-8 字节")
	}
	if err := validSchemaName(o.DB); err != nil {
		return nil, err
	}
	toks, err := lex(text)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, uerr("SQL 里只有注释，没有可执行的语句")
	}
	end := len(toks)
	if toks[end-1].isPunct(";") {
		end--
	}
	body := toks[:end]
	if len(body) == 0 {
		return nil, uerr("SQL 为空")
	}
	// 先看开头的词：begin / declare / with function 这类语句里本来就有分号，给出的提示比“多条语句”更到位
	if w := body[0]; w.kind == tkWord {
		kw := w.lower()
		if why, ok := denyFirstWord[kw]; ok {
			return nil, uerr("不允许 %s：%s。在线查询只读；数据或结构变更请通过 SQL 工单提交", strings.ToUpper(kw), why)
		}
		if nx := peekTok(body, 1); kw == "with" && (nx.is("function") || nx.is("procedure")) {
			return nil, uerr("不允许在语句里定义 PL/SQL 函数 / 过程（WITH FUNCTION）")
		}
	}
	for _, t := range body {
		if t.isPunct(";") {
			return nil, uerr("检测到多条语句；在线查询一次只能执行一条，请选中其中一条再运行")
		}
	}
	if body[len(body)-1].isPunct("/") {
		return nil, uerr("语句末尾不要加 /（那是 SQL*Plus 的执行符，不属于 SQL）；去掉后再运行")
	}

	ck := &Checked{DB: o.DB}
	first := body[0]
	switch {
	case first.kind == tkWord:
		kw := first.lower()
		switch kw {
		case "select", "with":
			ck.Kind = KindSelect
		case "explain":
			i, err := explainInner(body)
			if err != nil {
				return nil, err
			}
			ck.Kind = KindExplain
			body = body[i:]
		default:
			if why, ok := denyFirstWord[kw]; ok {
				return nil, uerr("不允许 %s：%s。在线查询只读；数据或结构变更请通过 SQL 工单提交", strings.ToUpper(kw), why)
			}
			return nil, uerr("语句类型 %s 不在在线查询的白名单内，只支持 SELECT / WITH / EXPLAIN PLAN FOR", strings.ToUpper(first.text))
		}
	case first.isPunct("("):
		if nx := peekTok(body, 1); !(nx.is("select") || nx.is("with")) {
			return nil, uerr("语句必须以 SELECT / WITH 开头（括号里也必须是查询）")
		}
		ck.Kind = KindSelect
	default:
		return nil, uerr("语句必须以 SELECT / WITH / EXPLAIN PLAN FOR 开头")
	}
	ck.Stmt = text[body[0].start:body[len(body)-1].end]

	heads := cteHeads(body)
	calls, err := globalScan(body, heads)
	if err != nil {
		return nil, err
	}
	ck.CallNames = calls
	sr, err := scanSelect(body)
	if err != nil {
		return nil, err
	}
	if err := checkQualifiers(body, sr, ck.DB); err != nil {
		return nil, err
	}
	if ck.BareNames, err = bareNames(body, sr, heads); err != nil {
		return nil, err
	}
	if err := classifyRefs(ck, sr.refs); err != nil {
		return nil, err
	}
	if len(ck.Tables) > maxTablesPerQuery {
		return nil, uerr("语句涉及 %d 张表，超过上限 %d", len(ck.Tables), maxTablesPerQuery)
	}
	return ck, nil
}

// validSchemaName 校验当前 schema 名：非空、不含双引号与控制字符（它会被放进 ALTER SESSION SET CURRENT_SCHEMA = "…"）。
func validSchemaName(db string) error {
	if strings.TrimSpace(db) == "" {
		return uerr("请选择数据库（schema）")
	}
	if len(db) > 128 || strings.ContainsAny(db, "\"\x00") {
		return uerr("schema 名称不合法")
	}
	for _, r := range db {
		if r < 0x20 || r == 0x7f {
			return uerr("schema 名称不合法")
		}
	}
	return nil
}

func peekTok(toks []token, i int) token {
	if i >= 0 && i < len(toks) {
		return toks[i]
	}
	return token{kind: tkPunct}
}

// explainInner 解析 EXPLAIN [PLAN] [FOR] <查询>，返回内层查询在 toks 里的起点。
func explainInner(toks []token) (int, error) {
	i := 1
	if peekTok(toks, i).is("plan") {
		i++
	}
	if peekTok(toks, i).is("set") {
		return 0, uerr("EXPLAIN PLAN 不能自己指定 SET STATEMENT_ID：平台会自动处理")
	}
	if peekTok(toks, i).is("for") {
		i++
	}
	nx := peekTok(toks, i)
	switch {
	case nx.is("select") || nx.is("with"):
	case nx.isPunct("(") && (peekTok(toks, i+1).is("select") || peekTok(toks, i+1).is("with")):
	default:
		return 0, uerr("EXPLAIN PLAN FOR 后面必须是 SELECT / WITH 查询语句")
	}
	return i, nil
}

// ---- 全局词法检查 ----

var denyFuncWhy = map[string]string{
	"table":       "TABLE(...) 可以把存储函数的结果当作表来读",
	"cursor":      "CURSOR(...) 表达式属于 PL/SQL 的游标语法",
	"multiset":    "MULTISET 子查询会生成对象集合",
	"xmltable":    "XQuery 里的 ora:view 可以读任意表",
	"xmlquery":    "XQuery 里的 ora:view 可以读任意表",
	"xmlexists":   "XQuery 里的 ora:view 可以读任意表",
	"xmlsequence": "它可以接收游标表达式",
	"deref":       "它可以按对象引用读其它表",
	"ref":         "它返回对象引用",
	"treat":       "它是对象类型的转换",
}

// globalScan 做与语句结构无关的词法检查：写操作词、危险符号、函数白名单。返回语句里用到的内置函数名（大写、去重、排序）。
func globalScan(toks []token, heads map[int]bool) ([]string, error) {
	calls := map[string]bool{}
	for i, t := range toks {
		switch t.kind {
		case tkBind:
			return nil, uerr("不支持绑定变量 %s：请把值直接写进语句", t.text)
		case tkPunct:
			switch t.text {
			case "@":
				return nil, uerr("不允许使用数据库链接（@链接名）：在线查询只能访问本实例")
			case "{", "}", "[", "]", "\\":
				return nil, uerr("语句里有不支持的符号 %s", t.text)
			}
			continue
		}
		if !t.isIdent() {
			continue
		}
		next := peekTok(toks, i+1)
		if t.kind == tkWord {
			l := t.lower()
			if why, ok := denyWords[l]; ok {
				if l == "update" && i > 0 && toks[i-1].is("for") {
					return nil, uerr("不允许 FOR UPDATE：在线查询只读，不能加行锁")
				}
				return nil, uerr("不允许使用 %s：%s。在线查询只读（列名 / 别名叫 %s 请用双引号括起来）", strings.ToUpper(l), why, strings.ToUpper(l))
			}
			if l == "with" && (next.is("function") || next.is("procedure")) {
				return nil, uerr("不允许在语句里定义 PL/SQL 函数 / 过程（WITH FUNCTION）")
			}
		}
		if !next.isPunct("(") {
			continue
		}
		// 标识符后面跟左括号：函数调用，或者白名单里的“关键字 + (”。
		if heads[i] {
			continue // WITH 名字(列, …) AS (…) 的列名列表
		}
		if peekTok(toks, i+2).isPunct("+") && peekTok(toks, i+3).isPunct(")") {
			continue // 老式外连接标记 col (+)
		}
		prevDot := i > 0 && toks[i-1].isPunct(".")
		if t.kind == tkQuoted {
			return nil, uerr("不允许调用加双引号命名的函数 %s（自定义函数）：在线查询只放行内置函数", t.text)
		}
		if prevDot {
			return nil, uerr("不允许调用 %s(...)：schema / 包 / 对象里的函数或方法按定义者权限执行，在线查询只放行内置函数", dottedName(toks, i))
		}
		l := t.lower()
		switch {
		case parenKeywords[l]:
			if collisionKeywords[l] {
				calls[strings.ToUpper(l)] = true
			}
		case typeParenWords[l] && typeContext(toks, i):
		case funcAllowed(l):
			calls[strings.ToUpper(l)] = true
		default:
			if why := denyFuncWhy[l]; why != "" {
				return nil, uerr("不允许使用 %s(...)：%s", strings.ToUpper(l), why)
			}
			return nil, uerr("不允许调用函数 %s：在线查询只放行内置函数白名单（自定义函数和包里的函数按定义者权限执行，会绕过只读账号与库表授权）；"+
				"确实需要这个内置函数，请联系管理员", strings.ToUpper(l))
		}
	}
	out := make([]string, 0, len(calls))
	for n := range calls {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// dottedName 返回 toks[i] 前面连着的 a.b.c 形式的完整名字（用于报错）。
func dottedName(toks []token, i int) string {
	j := i
	for j >= 2 && toks[j-1].isPunct(".") && toks[j-2].isIdent() {
		j -= 2
	}
	var parts []string
	for k := j; k <= i; k++ {
		if toks[k].isIdent() {
			parts = append(parts, toks[k].text)
		}
	}
	return strings.Join(parts, ".")
}

// typeContext 判断 toks[i]（NUMBER / TIMESTAMP / DAY …）是不是处在类型位置：CAST(… AS NUMBER(10,2))、INTERVAL DAY(3) TO SECOND(6)、
// '1' DAY(3)、RETURNING NUMBER(10)。其它位置的“类型名 + (”当函数调用处理。
func typeContext(toks []token, i int) bool {
	if i == 0 {
		return false
	}
	p := toks[i-1]
	return p.is("as") || p.is("interval") || p.is("to") || p.is("returning") || p.kind == tkString
}

func matchParen(toks []token, i int) int {
	depth := 0
	for j := i; j < len(toks); j++ {
		switch {
		case toks[j].isPunct("("):
			depth++
		case toks[j].isPunct(")"):
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// cteHeads 找出 WITH 子句里带列名列表的 CTE 名字（WITH r(n, m) AS (…)）：它们后面的左括号不是函数调用。
// WITH 只在语句开头或子查询开头（前面是左括号）才是 CTE 头，TIMESTAMP WITH TIME ZONE 之类不会误判（后面不是 名字 AS (）。
func cteHeads(toks []token) map[int]bool {
	heads := map[int]bool{}
	for i, t := range toks {
		if !t.is("with") || (i > 0 && !toks[i-1].isPunct("(")) {
			continue
		}
		j := i + 1
	defs:
		for j < len(toks) && toks[j].isIdent() {
			nameIdx := j
			j++
			if j < len(toks) && toks[j].isPunct("(") {
				k := matchParen(toks, j)
				if k < 0 {
					break
				}
				for x := j + 1; x < k; x++ {
					if !(toks[x].isIdent() || toks[x].isPunct(",")) {
						break defs
					}
				}
				heads[nameIdx] = true
				j = k + 1
			}
			if j >= len(toks) || !toks[j].is("as") {
				break
			}
			j++
			if j >= len(toks) || !toks[j].isPunct("(") {
				break
			}
			k := matchParen(toks, j)
			if k < 0 {
				break
			}
			j = k + 1
			// SEARCH … / CYCLE … 子句：跳到下一个逗号（下一个 CTE）或主查询。
			for j < len(toks) && (toks[j].is("search") || toks[j].is("cycle")) {
				depth := 0
				for j < len(toks) {
					if toks[j].isPunct("(") {
						depth++
					} else if toks[j].isPunct(")") {
						depth--
					}
					if depth <= 0 && (toks[j].isPunct(",") || toks[j].is("select")) {
						break
					}
					j++
				}
			}
			if j < len(toks) && toks[j].isPunct(",") {
				j++
				continue
			}
			break
		}
	}
	return heads
}
