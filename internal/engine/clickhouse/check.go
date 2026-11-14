// @author: kevinkelly
// @license: Apache Licence
// @file: check.go
// @time: 2026/10/28

package clickhouse

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"dbtool/internal/sqlx"
)

// 在线查询语句的静态检查（对应 Redis 的 ParseUserCommand / MSSQL 的 checkMSSQL）。不连服务端。
//
// 放行：SELECT（含 WITH、UNION / INTERSECT / EXCEPT、子查询、JOIN）、SHOW（库表结构类）、DESCRIBE / DESC、EXISTS、
// EXPLAIN（AST / SYNTAX / QUERY TREE / PLAN / PIPELINE / ESTIMATE，内层必须是通过检查的 SELECT）。
// 其余一律拒绝。

// UserError 是语句检查 / 探测不通过的业务原因，原文可直接展示给用户（上层映射成 bad_query）。
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func uerr(format string, a ...any) error { return &UserError{Msg: fmt.Sprintf(format, a...)} }

// MaxQueryBytes 是语句文本的长度上限。
const MaxQueryBytes = 128 << 10

// maxTablesPerQuery 限制一条语句涉及的表数量（执行前要逐个确认引擎）。
const maxTablesPerQuery = 200

// CheckOptions 是检查选项。
type CheckOptions struct {
	// DB 是执行时的当前库（HTTP 的 database 参数）。语句里没写库名的表按它补全；为空按 "default"。
	DB string
}

// Checked 是通过静态检查的语句。
type Checked struct {
	// Stmt 是要发给服务端的文本：去掉了首尾注释、末尾分号与 FORMAT 子句，内部原样保留。
	Stmt string
	// Kind 是语句类别：select / show / describe / explain / exists。
	Kind string
	// DB 是执行时的当前库（可能为空，表示用连接配置里的库）。
	DB string
	// Tables 是语句涉及的表 / 视图 / 字典（Schema=库名，已按当前库补全；不含 CTE 别名）。
	// 包括 dictGet / joinGet 的字符串参数指向的对象，以及 `x IN 表名` 形式的引用。
	Tables []sqlx.TableRef
	// Databases 是涉及的库（去重、排序），含只引用库的语句（SHOW TABLES FROM db）。
	Databases []string
	// Notices 是静态改写给用户的提示。
	Notices []string
}

type tref struct{ db, name string }

// CheckUserQuery 检查并整理一条用户输入的语句：单语句、白名单类别、没有 SETTINGS / FORMAT / INTO、
// 没有危险函数和表函数、system 库只开放摘要类表。不通过返回 *UserError。
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
	for _, t := range body {
		if t.isPunct(";") {
			return nil, uerr("检测到多条语句；在线查询一次只能执行一条，请选中其中一条再运行")
		}
	}
	if len(body) == 0 {
		return nil, uerr("SQL 为空")
	}
	ck := &Checked{DB: o.DB}
	// 末尾的 FORMAT xxx：输出格式固定，忽略用户的选择。
	if n := len(body); n >= 2 && body[n-2].is("format") && body[n-1].kind == tkWord && !(n >= 3 && body[n-3].isPunct(".")) {
		body = body[:n-2]
		ck.Notices = append(ck.Notices, "已忽略 FORMAT 子句：在线查询的输出格式固定")
		if len(body) == 0 {
			return nil, uerr("SQL 为空")
		}
	}
	ck.Stmt = text[body[0].start:body[len(body)-1].end]

	first := body[0]
	if first.kind != tkWord {
		if first.isPunct("(") {
			return nil, uerr("暂不支持以括号开头的语句；请去掉最外层括号，以 SELECT / WITH 开头")
		}
		return nil, uerr("语句必须以 SELECT / WITH / SHOW / DESCRIBE / EXPLAIN / EXISTS 开头")
	}
	kw := first.lower()
	switch kw {
	case "select", "with", "show", "describe", "desc", "exists", "explain":
	default:
		if why, ok := denyFirstWord[kw]; ok {
			return nil, uerr("不允许 %s：%s。在线查询只读；数据或结构变更请通过 SQL 工单提交", strings.ToUpper(kw), why)
		}
		return nil, uerr("语句类型 %s 不在在线查询的白名单内，只支持 SELECT / WITH / SHOW / DESCRIBE / EXPLAIN / EXISTS", strings.ToUpper(first.text))
	}
	if err := globalScan(body); err != nil {
		return nil, err
	}
	var refs []tref
	var dbOnly []string
	switch kw {
	case "select", "with":
		ck.Kind = "select"
		refs, err = scanSelect(body)
	case "show":
		ck.Kind = "show"
		refs, dbOnly, err = scanShow(body)
	case "describe", "desc":
		ck.Kind = "describe"
		refs, err = scanDescribe(body)
	case "exists":
		ck.Kind = "exists"
		refs, dbOnly, err = scanExists(body)
	case "explain":
		ck.Kind = "explain"
		refs, err = scanExplain(body)
	}
	if err != nil {
		return nil, err
	}
	cur := o.DB
	if cur == "" {
		cur = "default"
	}
	seen := map[sqlx.TableRef]bool{}
	dbs := map[string]bool{}
	for _, r := range refs {
		db := r.db
		if db == "" {
			db = cur
		}
		if err := checkSystemRef(db, r.name); err != nil {
			return nil, err
		}
		ref := sqlx.TableRef{Schema: db, Name: r.name}
		dbs[db] = true
		if !seen[ref] {
			seen[ref] = true
			ck.Tables = append(ck.Tables, ref)
		}
	}
	for _, d := range dbOnly {
		dbs[d] = true
	}
	if len(ck.Tables) > maxTablesPerQuery {
		return nil, uerr("语句涉及 %d 张表，超过上限 %d", len(ck.Tables), maxTablesPerQuery)
	}
	for d := range dbs {
		ck.Databases = append(ck.Databases, d)
	}
	sort.Strings(ck.Databases)
	return ck, nil
}

// checkSystemRef：system 库只放行摘要类表；information_schema 整库放行。
func checkSystemRef(db, name string) error {
	switch {
	case isSystemDB(db):
		if name == "" || allowedSystemTables[strings.ToLower(name)] {
			return nil
		}
		return uerr("不允许访问 system.%s：system 库在线查询只开放库表结构类表（%s）；账号权限、他人 SQL、集群与凭据信息请通过 DBA 页面查看",
			name, systemAllowList())
	case isInfoSchemaDB(db):
		return nil
	}
	return nil
}

func systemAllowList() string {
	names := make([]string, 0, len(allowedSystemTables))
	for n := range allowedSystemTables {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, " / ")
}

// globalScan 做与语句类别无关的词法检查：SETTINGS / INTO / 剩余的 FORMAT、危险函数与表函数。
func globalScan(toks []token) error {
	for i, t := range toks {
		if !t.isIdent() {
			continue
		}
		prevDot := i > 0 && toks[i-1].isPunct(".")
		nextParen := i+1 < len(toks) && toks[i+1].isPunct("(")
		name := strings.ToLower(t.name())
		if t.kind == tkWord && !prevDot {
			switch name {
			case "settings":
				return uerr("不允许 SETTINGS 子句：服务端限额与只读模式由平台统一设置，不能在语句里覆盖（列名 / 表名叫 settings 请用反引号括起来）")
			case "into":
				return uerr("不允许 INTO 子句（INSERT INTO / INTO OUTFILE）：在线查询只读")
			case "outfile", "infile":
				return uerr("不允许 %s：在线查询不能读写文件", strings.ToUpper(name))
			case "format":
				if !nextParen {
					return uerr("不允许自行指定 FORMAT：在线查询的输出格式固定（只允许写在语句末尾，会被忽略；列名叫 format 请用反引号括起来）")
				}
			}
		}
		if why, ok := denyScalarFuncs[name]; ok && !prevDot {
			return uerr("不允许使用函数 %s：%s", name, why)
		}
		if nextParen {
			if why, ok := denyTableFuncs[name]; ok && !scalarOK[name] {
				return uerr("不允许使用 %s(...)：%s", name, why)
			}
		}
	}
	return nil
}

// ---- SELECT ----

type frameKind uint8

const (
	fSelect frameKind = iota // (SELECT ...) 或语句本身：认 FROM / JOIN
	fJoin                    // FROM ( a JOIN b ... ) 括号嵌套连接
	fExpr                    // 函数调用 / 表达式括号：不认 FROM（EXTRACT(x FROM y)、TRIM(BOTH ' ' FROM s)）
)

type frame struct {
	kind     frameKind
	inFrom   bool
	expect   bool // 下一个标识符应该是表名
	inWith   bool // 处在 WITH 子句里（WITH 之后、SELECT 之前）
	pending  string
	defName  string // 本括号是 CTE defName 的定义体
	ctes     map[string]bool
	prevJoin bool
}

// fromTerminators 结束同一层级里的 FROM 列表。
var fromTerminators = map[string]bool{
	"where": true, "prewhere": true, "group": true, "having": true, "order": true, "limit": true, "union": true,
	"intersect": true, "except": true, "window": true, "qualify": true, "offset": true, "fetch": true, "into": true,
	"settings": true, "format": true, "select": true, "array": true,
}

// scanSelect 提取 SELECT 语句涉及的表，同时检查表位置的表函数。
func scanSelect(toks []token) ([]tref, error) {
	stack := []*frame{{kind: fSelect, ctes: map[string]bool{}}}
	top := func() *frame { return stack[len(stack)-1] }
	isCTE := func(name string) bool {
		for _, f := range stack {
			if f.ctes[name] {
				return true
			}
		}
		return false
	}
	var refs []tref
	n := len(toks)
	peek := func(i int) token {
		if i < n {
			return toks[i]
		}
		return token{kind: tkPunct}
	}
	for i := 0; i < n; i++ {
		t := toks[i]
		f := top()
		switch {
		case t.isPunct("("):
			nf := &frame{ctes: map[string]bool{}}
			nx := peek(i + 1)
			switch {
			case nx.is("select") || nx.is("with"):
				nf.kind = fSelect
				f.expect = false
			case f.expect && f.inFrom:
				nf.kind, nf.inFrom, nf.expect = fJoin, true, true
				f.expect = false
			default:
				nf.kind = fExpr
			}
			if f.pending != "" {
				nf.defName, f.pending = f.pending, ""
			}
			stack = append(stack, nf)
		case t.isPunct(")"):
			if len(stack) == 1 {
				return nil, uerr("括号不匹配：多了一个右括号")
			}
			child := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if child.defName != "" {
				top().ctes[child.defName] = true
			}
		case t.isPunct(","):
			if f.inFrom && f.kind != fExpr {
				f.expect = true
			}
		case t.isPunct("{"):
			if f.expect {
				return nil, uerr("不支持参数化的表名 {name:Type}")
			}
		case t.isIdent():
			prevDot := i > 0 && toks[i-1].isPunct(".")
			nextParen := peek(i + 1).isPunct("(")
			lname := strings.ToLower(t.name())

			// 函数调用：dictGet / joinGet 的第一个参数是对象名，要计入涉及的表。
			if nextParen && !prevDot {
				if isDictFunc(lname) || isJoinGetFunc(lname) {
					r, err := stringObjectArg(toks, i+2, lname)
					if err != nil {
						return nil, err
					}
					refs = append(refs, r)
				}
			}

			if f.expect && !(t.kind == tkWord && lname == "select") {
				// 表位置：表名、db.表名 或表函数
				if nextParen && !peek(i+1).isPunct(".") {
					if err := checkTableFunc(toks, i, lname); err != nil {
						return nil, err
					}
					f.expect = false
					continue
				}
				db, name := "", t.name()
				if peek(i+1).isPunct(".") && peek(i+2).isIdent() {
					db, name = t.name(), peek(i+2).name()
					i += 2
					if peek(i + 1).isPunct(".") {
						return nil, uerr("不支持的表名写法（出现多于两段的名字）")
					}
					if peek(i + 1).isPunct("(") {
						return nil, uerr("不允许 %s.%s(...) 形式的调用", db, name)
					}
				}
				f.expect = false
				if db == "" && isCTE(name) {
					continue
				}
				refs = append(refs, tref{db: db, name: name})
				continue
			}
			if t.kind != tkWord || prevDot {
				continue
			}
			switch lname {
			case "with":
				if nx := peek(i + 1); !(nx.is("totals") || nx.is("fill") || nx.is("ties") || nx.is("rollup") || nx.is("cube")) {
					f.inWith = true
				}
			case "select":
				f.inWith, f.inFrom, f.expect = false, false, false
			case "from":
				if f.kind != fExpr {
					f.inFrom, f.expect = true, true
				}
			case "join":
				switch {
				case i > 0 && toks[i-1].is("array"):
					f.inFrom, f.expect = false, false // ARRAY JOIN 后面是表达式，不是表
				case f.kind != fExpr:
					f.inFrom, f.expect = true, true
				}
			case "union", "intersect", "except":
				// 集合运算符：WITH 定义的 CTE 只对其所在的那条 SELECT 可见（后面的 SELECT 里同名的是真实表，按真实表报），
				// 宁可多报也不漏报。* EXCEPT (col) 是列变换器，不是集合运算。
				if nx := peek(i + 1); lname != "except" || nx.is("select") || nx.is("with") || nx.is("all") || nx.is("distinct") {
					f.ctes = map[string]bool{}
				}
				if f.inFrom {
					f.inFrom, f.expect = false, false
				}
			case "as":
				if f.inWith && i > 0 && toks[i-1].isIdent() && peek(i+1).isPunct("(") {
					f.pending = toks[i-1].name()
				}
			case "in":
				nx := peek(i + 1)
				if !nx.isIdent() {
					break
				}
				if nx.kind == tkWord {
					if l := nx.lower(); l == "null" || l == "true" || l == "false" || l == "select" || l == "with" {
						break
					}
				}
				if peek(i + 2).isPunct("(") {
					break // IN fn(...)：函数；表函数已由 globalScan 按黑名单拒绝
				}
				db, name := "", nx.name()
				step := 1
				if peek(i+2).isPunct(".") && peek(i+3).isIdent() {
					db, name = nx.name(), peek(i+3).name()
					step = 3
					if peek(i + 4).isPunct("(") {
						return nil, uerr("不允许 IN %s.%s(...) 形式的调用", db, name)
					}
				}
				i += step
				if db == "" && isCTE(name) {
					break
				}
				refs = append(refs, tref{db: db, name: name})
			default:
				if f.inFrom && fromTerminators[lname] {
					f.inFrom, f.expect = false, false
				}
			}
		}
	}
	if len(stack) != 1 {
		return nil, uerr("括号不匹配：缺少右括号")
	}
	return refs, nil
}

// checkTableFunc 校验表位置的表函数：默认拒绝，只放行 numbers(<小的整数字面量>) 与 values(...)。
func checkTableFunc(toks []token, i int, lname string) error {
	if !allowedTableFuncs[lname] {
		why := denyTableFuncs[lname]
		if why != "" {
			return uerr("不允许使用表函数 %s：%s", lname, why)
		}
		return uerr("不允许使用表函数 %s：在线查询的表函数采用默认拒绝，只放行 numbers(N) 与 values(...)", lname)
	}
	if lname != "numbers" {
		return nil
	}
	// numbers(N) / numbers(offset, N) / numbers(offset, N, step)：参数只能是不超过 numbersMax 的十进制整数字面量。
	j := i + 2
	cnt := 0
	for ; j < len(toks); j++ {
		t := toks[j]
		if t.isPunct(")") {
			break
		}
		if t.isPunct(",") {
			continue
		}
		if t.kind != tkNumber {
			return uerr("numbers(...) 的参数只能是整数字面量")
		}
		v, err := strconv.ParseUint(t.text, 10, 64)
		if err != nil || v > numbersMax {
			return uerr("numbers(...) 的参数不能超过 %d", numbersMax)
		}
		cnt++
	}
	if j >= len(toks) || cnt < 1 || cnt > 3 {
		return uerr("numbers(...) 需要 1 到 3 个整数字面量参数")
	}
	return nil
}

// stringObjectArg 读取 dictGet / joinGet 的第一个参数：必须是字符串常量 'db.name' 或 'name'。
func stringObjectArg(toks []token, i int, fn string) (tref, error) {
	if i >= len(toks) || toks[i].kind != tkString {
		return tref{}, uerr("%s 的第一个参数必须是字符串常量（字典 / 表名），否则无法确定涉及的对象", fn)
	}
	if i+1 >= len(toks) || !(toks[i+1].isPunct(",") || toks[i+1].isPunct(")")) {
		return tref{}, uerr("%s 的第一个参数必须是单独的字符串常量", fn)
	}
	t := toks[i]
	if t.esc || t.kind != tkString || strings.ContainsAny(t.val, "\x00\\`\"") {
		return tref{}, uerr("%s 的对象名含有转义或特殊字符，已拒绝", fn)
	}
	v := t.val
	if v == "" {
		return tref{}, uerr("%s 的对象名为空", fn)
	}
	if k := strings.Index(v, "."); k >= 0 {
		if strings.Contains(v[k+1:], ".") || k == 0 || k == len(v)-1 {
			return tref{}, uerr("%s 的对象名 %q 写法不支持", fn, v)
		}
		return tref{db: v[:k], name: v[k+1:]}, nil
	}
	return tref{name: v}, nil
}

// ---- EXPLAIN ----

var explainKinds = map[string]bool{"ast": true, "syntax": true, "plan": true, "pipeline": true, "estimate": true}

// scanExplain：EXPLAIN [AST|SYNTAX|QUERY TREE|PLAN|PIPELINE|ESTIMATE] [k = v, ...] <SELECT>。
// 这些子类型都不返回数据行；其中 PLAN / PIPELINE 为了做常量折叠可能会执行标量子查询，
// 所以内层语句必须通过完整的 SELECT 检查，并且和普通查询一样在 readonly + 限额下执行。
// EXPLAIN TABLE OVERRIDE 等其它形式一律拒绝。
func scanExplain(toks []token) ([]tref, error) {
	i := 1
	if i < len(toks) && toks[i].kind == tkWord {
		w := toks[i].lower()
		switch {
		case explainKinds[w]:
			i++
		case w == "query":
			if i+1 < len(toks) && toks[i+1].is("tree") {
				i += 2
			} else {
				return nil, uerr("EXPLAIN QUERY 后面只能是 TREE")
			}
		case w == "select" || w == "with":
		default:
			return nil, uerr("EXPLAIN 只放行 AST / SYNTAX / QUERY TREE / PLAN / PIPELINE / ESTIMATE；%s 不在白名单内", strings.ToUpper(toks[i].text))
		}
	}
	// 选项：key = number，逗号分隔
	for i+2 < len(toks) && toks[i].kind == tkWord && toks[i+1].isPunct("=") && toks[i+2].kind == tkNumber {
		i += 3
		if i < len(toks) && toks[i].isPunct(",") {
			i++
		}
	}
	if i >= len(toks) || !(toks[i].is("select") || toks[i].is("with")) {
		return nil, uerr("EXPLAIN 后面必须是 SELECT / WITH 查询语句")
	}
	return scanSelect(toks[i:])
}

// ---- DESCRIBE / EXISTS / SHOW ----

// qname 读取 [db.]name，返回新的位置。
func qname(toks []token, i int) (db, name string, next int, ok bool) {
	if i >= len(toks) || !toks[i].isIdent() {
		return "", "", i, false
	}
	if i+2 < len(toks) && toks[i+1].isPunct(".") && toks[i+2].isIdent() {
		return toks[i].name(), toks[i+2].name(), i + 3, true
	}
	return "", toks[i].name(), i + 1, true
}

func scanDescribe(toks []token) ([]tref, error) {
	i := 1
	if i < len(toks) && toks[i].is("table") {
		i++
	}
	db, name, next, ok := qname(toks, i)
	if !ok || next != len(toks) {
		return nil, uerr("DESCRIBE 只支持 DESCRIBE [TABLE] [库.]表名；DESCRIBE 子查询 / 表函数不放行")
	}
	return []tref{{db: db, name: name}}, nil
}

func scanExists(toks []token) ([]tref, []string, error) {
	i := 1
	if i < len(toks) && toks[i].is("temporary") {
		i++
	}
	isDB := false
	if i < len(toks) {
		switch {
		case toks[i].is("database"):
			isDB = true
			i++
		case toks[i].is("table") || toks[i].is("view") || toks[i].is("dictionary"):
			i++
		}
	}
	db, name, next, ok := qname(toks, i)
	if !ok || next != len(toks) {
		return nil, nil, uerr("EXISTS 只支持 EXISTS [TABLE|VIEW|DICTIONARY|DATABASE] [库.]名称")
	}
	if isDB {
		if db != "" {
			return nil, nil, uerr("EXISTS DATABASE 后面只能是库名")
		}
		return nil, []string{name}, nil
	}
	return []tref{{db: db, name: name}}, nil, nil
}

// showFiller 是 SHOW 语句里允许出现的辅助词。
var showFiller = map[string]bool{
	"from": true, "in": true, "like": true, "ilike": true, "not": true, "limit": true,
	"full": true, "extended": true, "temporary": true,
}

// scanShow 只放行库表结构类的 SHOW：DATABASES / TABLES / DICTIONARIES / CREATE TABLE|VIEW|DICTIONARY|DATABASE /
// COLUMNS|FIELDS / INDEX|INDEXES|KEYS。SHOW PROCESSLIST / GRANTS / USERS / CLUSTERS / SETTINGS / CREATE USER 等一律拒绝。
// 参数部分只能是：库表名、LIKE 字符串、LIMIT 数字；WHERE 等含表达式的写法不放行。
func scanShow(toks []token) ([]tref, []string, error) {
	if len(toks) < 2 || toks[1].kind != tkWord {
		return nil, nil, uerr("SHOW 后面缺少子命令")
	}
	i := 1
	for i < len(toks) && (toks[i].is("temporary") || toks[i].is("full") || toks[i].is("extended")) {
		i++
	}
	if i >= len(toks) {
		return nil, nil, uerr("SHOW 后面缺少子命令")
	}
	sub := toks[i].lower()
	rest := toks[i+1:]
	var refs []tref
	var dbs []string
	switch sub {
	case "databases", "schemas":
		// 无对象
	case "tables", "dictionaries":
	case "create":
		if len(rest) == 0 {
			return nil, nil, uerr("SHOW CREATE 后面缺少对象类型")
		}
		kind := rest[0].lower()
		if rest[0].kind != tkWord || (kind != "table" && kind != "view" && kind != "dictionary" && kind != "database" && kind != "temporary") {
			return nil, nil, uerr("SHOW CREATE 只放行 TABLE / VIEW / DICTIONARY / DATABASE（SHOW CREATE USER / ROLE / QUOTA 等含权限信息，不放行）")
		}
		rest = rest[1:]
		if kind == "temporary" {
			if len(rest) == 0 || !(rest[0].is("table")) {
				return nil, nil, uerr("SHOW CREATE TEMPORARY 后面必须是 TABLE")
			}
			rest = rest[1:]
		}
		db, name, next, ok := qname(rest, 0)
		if !ok || next != len(rest) {
			return nil, nil, uerr("SHOW CREATE 的参数只能是 [库.]名称")
		}
		if kind == "database" {
			if db != "" {
				return nil, nil, uerr("SHOW CREATE DATABASE 后面只能是库名")
			}
			return nil, []string{name}, nil
		}
		return []tref{{db: db, name: name}}, nil, nil
	case "columns", "fields", "index", "indexes", "keys":
		// SHOW COLUMNS FROM tbl [FROM db]
	default:
		return nil, nil, uerr("SHOW %s 不在白名单内；在线查询的 SHOW 只支持 DATABASES / TABLES / DICTIONARIES / CREATE TABLE|VIEW|DICTIONARY|DATABASE / COLUMNS / INDEX", strings.ToUpper(toks[i].text))
	}
	// 解析 FROM/IN <对象> 与 LIKE '..'、LIMIT n
	var objs []string // FROM/IN 后面的名字，按出现顺序
	for j := 0; j < len(rest); j++ {
		t := rest[j]
		switch {
		case t.kind == tkWord && showFiller[t.lower()]:
			switch t.lower() {
			case "from", "in":
				db, name, next, ok := qname(rest, j+1)
				if !ok {
					return nil, nil, uerr("SHOW 的 FROM / IN 后面缺少名称")
				}
				if db != "" {
					objs = append(objs, db+"\x00"+name)
				} else {
					objs = append(objs, name)
				}
				j = next - 1
			case "like", "ilike":
				if j+1 >= len(rest) || rest[j+1].kind != tkString {
					return nil, nil, uerr("SHOW 的 LIKE 后面必须是字符串常量")
				}
				j++
			case "limit":
				if j+1 >= len(rest) || rest[j+1].kind != tkNumber {
					return nil, nil, uerr("SHOW 的 LIMIT 后面必须是数字")
				}
				j++
			}
		default:
			return nil, nil, uerr("SHOW 语句里出现了不支持的写法（%s）；只支持 FROM / IN 库表名、LIKE '模式'、LIMIT 数字", t.text)
		}
	}
	switch sub {
	case "tables", "dictionaries":
		for _, o := range objs {
			if strings.Contains(o, "\x00") {
				return nil, nil, uerr("SHOW %s FROM 后面只能是库名", strings.ToUpper(sub))
			}
			dbs = append(dbs, o)
		}
	case "columns", "fields", "index", "indexes", "keys":
		if len(objs) == 0 || len(objs) > 2 {
			return nil, nil, uerr("SHOW %s 需要 FROM 表名 [FROM 库名]", strings.ToUpper(sub))
		}
		db, name := "", objs[0]
		if k := strings.Index(name, "\x00"); k >= 0 {
			db, name = name[:k], name[k+1:]
		}
		if len(objs) == 2 {
			if strings.Contains(objs[1], "\x00") {
				return nil, nil, uerr("SHOW %s 的第二个 FROM 只能是库名", strings.ToUpper(sub))
			}
			if db != "" && db != objs[1] {
				return nil, nil, uerr("SHOW %s 指定了两个不同的库", strings.ToUpper(sub))
			}
			db = objs[1]
		}
		refs = append(refs, tref{db: db, name: name})
	}
	return refs, dbs, nil
}
