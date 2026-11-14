// @author: kevinkelly
// @license: Apache Licence
// @file: scan.go
// @time: 2026/10/28

package dm

import (
	"sort"
	"strings"

	"dbtool/internal/sqlx"
)

// ---- FROM 子句里的表、别名与限定名 ----
//
// Oracle 允许不带括号调用无参函数：SELECT 包名.函数名 FROM dual、SELECT 自定义函数 FROM dual。语法上它们和“表别名.列名”“列名”一模一样，
// 只看“后面跟括号”拦不住。所以这里再补两道：
//   1. 带点的名字（a.b、a.b.c）第一段必须是语句里的表名 / 别名 / WITH 名（schema.表.列 要 schema.表 在语句里出现过）——
//      schema.函数、包.函数 这类不带括号的调用因此被拒绝；
//   2. 不带点、不带括号的名字（可能是列，也可能是无参函数）收集到 Checked.BareNames，执行前核对库里没有同名的函数。

type frameKind uint8

const (
	fSelect frameKind = iota // (SELECT ...) 或语句本身：认 FROM / JOIN
	fJoin                    // FROM ( a JOIN b ... ) 括号嵌套连接
	fExpr                    // 函数调用 / 表达式括号：不认 FROM（EXTRACT(x FROM y)、TRIM(BOTH ' ' FROM s)）
)

type frame struct {
	parent    *frame
	kind      frameKind
	inFrom    bool
	expect    bool // 下一个标识符应该是表名
	aliasNext bool // 刚读完表名 / 子查询，下一个标识符可能是它的别名
	inWith    bool // 处在 WITH 子句里（WITH 之后、SELECT 之前）
	pending   string
	ctes      map[string]bool
	aliases   map[string]bool // 本层 FROM 里的表名与别名（大写 / 引号原样）
}

func newFrame(parent *frame, kind frameKind) *frame {
	return &frame{parent: parent, kind: kind, ctes: map[string]bool{}, aliases: map[string]bool{}}
}

// visible 判断名字是不是当前层或外层可见的表名 / 别名 / WITH 名。
func (f *frame) visible(name string) bool {
	for fr := f; fr != nil; fr = fr.parent {
		if fr.aliases[name] || fr.ctes[name] {
			return true
		}
	}
	return false
}

// scanResult 是 scanSelect 的结果。
type scanResult struct {
	refs     []tref
	skip     map[int]bool // 表名 / 别名 token：它们不是表达式里的名字
	tokFrame []*frame     // 每个 token 所在的层
}

// fromTerminators 结束同一层级里的 FROM 列表（只放保留字：非保留字可能是表别名，误当结束会漏掉后面的表）。
var fromTerminators = setOf("where", "group", "having", "order", "union", "intersect", "minus", "except", "connect", "start", "select")

// notAlias 是表名后面可以出现、但不是别名的词。（LIMIT 只放这里、不放进 fromTerminators：万一某个版本里它能当别名，后面逗号接的表也不会被漏掉。）
var notAlias = setOf("join", "inner", "left", "right", "full", "cross", "natural", "outer", "on", "using", "where", "group", "order", "having", "union",
	"intersect", "minus", "except", "connect", "start", "partition", "subpartition", "sample", "pivot", "unpivot", "fetch", "offset", "limit", "lateral", "apply",
	"model", "for", "with", "select", "of", "versions", "returning")

// cteNameBefore 返回 toks[i]（AS）前面的 CTE 名字：WITH x AS (…) 或 WITH x(列, …) AS (…)。
func cteNameBefore(toks []token, i int) string {
	if i == 0 {
		return ""
	}
	p := toks[i-1]
	if p.isIdent() {
		return p.name()
	}
	if p.isPunct(")") {
		depth := 0
		for j := i - 1; j >= 0; j-- {
			switch {
			case toks[j].isPunct(")"):
				depth++
			case toks[j].isPunct("("):
				depth--
				if depth == 0 {
					if j > 0 && toks[j-1].isIdent() {
						return toks[j-1].name()
					}
					return ""
				}
			}
		}
	}
	return ""
}

// scanSelect 提取查询涉及的表（FROM / JOIN / 逗号列表 / 括号连接 / 子查询 / CTE），返回 owner 与名字（已折成库里的真实大小写），
// 同时记录每一层 FROM 里的表名与别名，供限定名检查用。
func scanSelect(toks []token) (*scanResult, error) {
	root := newFrame(nil, fSelect)
	cur := root
	sr := &scanResult{skip: map[int]bool{}, tokFrame: make([]*frame, len(toks))}
	n := len(toks)
	for i := 0; i < n; i++ {
		t := toks[i]
		f := cur
		sr.tokFrame[i] = f
		// 别名只可能紧跟在表名 / 子查询后面：遇到别的符号就不再等别名。
		if f.aliasNext && !(t.isIdent() || t.isPunct(")")) {
			f.aliasNext = false
		}
		switch {
		case t.isPunct("("):
			var nf *frame
			switch nx := peekTok(toks, i+1); {
			case nx.is("select") || nx.is("with"):
				nf = newFrame(f, fSelect)
				f.expect = false
			case f.expect && f.inFrom:
				nf = newFrame(f, fJoin)
				nf.inFrom, nf.expect = true, true
				f.expect = false
			default:
				nf = newFrame(f, fExpr)
			}
			f.aliasNext = false
			if f.pending != "" {
				f.ctes[f.pending] = true // 递归 CTE 在自己的定义体里也能引用自己
				f.pending = ""
			}
			cur = nf
		case t.isPunct(")"):
			if cur.parent == nil {
				return nil, uerr("括号不匹配：多了一个右括号")
			}
			child := cur
			cur = cur.parent
			if child.kind == fJoin { // FROM (a x JOIN b y …) 里的别名在外层也能用
				for a := range child.aliases {
					cur.aliases[a] = true
				}
			}
			if cur.inFrom {
				cur.aliasNext = true // FROM (子查询) 别名 / 表 PARTITION (…) 别名
			}
		case t.isPunct(","):
			if f.inFrom && f.kind != fExpr {
				f.expect = true
			}
		case t.isIdent():
			prevDot := i > 0 && toks[i-1].isPunct(".")
			lname := ""
			if t.kind == tkWord {
				lname = t.lower()
			}
			if f.aliasNext && !f.expect {
				switch {
				case lname == "as":
					continue // 表 AS 别名
				case notAlias[lname]:
					f.aliasNext = false
				case prevDot || peekTok(toks, i+1).isPunct(".") || peekTok(toks, i+1).isPunct("("):
					f.aliasNext = false
				default:
					f.aliases[t.name()] = true
					sr.skip[i] = true
					f.aliasNext = false
					continue
				}
			}
			if f.expect && !(lname == "select" || lname == "with") {
				// 表位置
				if !prevDot {
					switch lname {
					case "lateral":
						continue // LATERAL (SELECT …)：保持 expect，让后面的括号按子查询处理
					case "only":
						return nil, uerr("不支持 FROM ONLY (...) 写法")
					}
				}
				if peekTok(toks, i+1).isPunct("(") {
					f.expect = false // 表位置的函数（JSON_TABLE 等）：函数白名单已在 globalScan 检查过
					sr.skip[i] = true
					f.aliasNext = true
					continue
				}
				owner, name := "", t.name()
				sr.skip[i] = true
				if peekTok(toks, i+1).isPunct(".") && peekTok(toks, i+2).isIdent() {
					owner, name = t.name(), toks[i+2].name()
					sr.skip[i+2] = true
					i += 2
					sr.tokFrame[i-1], sr.tokFrame[i] = f, f
					if peekTok(toks, i+1).isPunct(".") {
						return nil, uerr("不支持的表名写法（出现多于两段的名字）")
					}
				}
				f.expect = false
				f.aliasNext = true
				if owner == "" && f.visibleCTE(name) {
					continue
				}
				f.aliases[name] = true
				sr.refs = append(sr.refs, tref{owner: owner, name: name})
				continue
			}
			if t.kind != tkWord || prevDot {
				continue
			}
			switch lname {
			case "with":
				if i == 0 || toks[i-1].isPunct("(") {
					if nx := peekTok(toks, i+1); !(nx.is("function") || nx.is("procedure")) {
						f.inWith = true
					}
				}
			case "select":
				f.inWith, f.inFrom, f.expect, f.aliasNext = false, false, false, false
			case "from":
				if f.kind != fExpr {
					f.inFrom, f.expect = true, true
				}
			case "join":
				if f.kind != fExpr {
					f.inFrom, f.expect = true, true
				}
			case "apply":
				if f.kind != fExpr && i > 0 && (toks[i-1].is("cross") || toks[i-1].is("outer")) {
					f.inFrom, f.expect = true, true
				}
			case "union", "intersect", "minus", "except":
				if f.inFrom {
					f.inFrom, f.expect = false, false
				}
			case "as":
				if f.inWith && peekTok(toks, i+1).isPunct("(") {
					if name := cteNameBefore(toks, i); name != "" {
						f.pending = name
					}
				}
			default:
				if f.inFrom && fromTerminators[lname] {
					f.inFrom, f.expect = false, false
				}
			}
		}
	}
	if cur.parent != nil {
		return nil, uerr("括号不匹配：缺少右括号")
	}
	return sr, nil
}

// visibleCTE 判断名字是不是当前层或外层的 WITH 名。
func (f *frame) visibleCTE(name string) bool {
	for fr := f; fr != nil; fr = fr.parent {
		if fr.ctes[name] {
			return true
		}
	}
	return false
}

// checkQualifiers 检查所有带点的名字（表位置除外）：第一段必须是当前层或外层可见的表名 / 别名 / WITH 名；
// schema.表.列 这种三段写法，schema.表 要在语句的 FROM 里出现过。cur 是当前 schema（没写 schema 的表按它补全）。
func checkQualifiers(toks []token, sr *scanResult, cur string) error {
	known := map[[2]string]bool{}
	for _, r := range sr.refs {
		owner := r.owner
		if owner == "" {
			owner = cur
		}
		known[[2]string{owner, r.name}] = true
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if !t.isIdent() || sr.skip[i] || (i > 0 && toks[i-1].isPunct(".")) || !peekTok(toks, i+1).isPunct(".") {
			continue
		}
		parts := []token{t}
		j := i
		for peekTok(toks, j+1).isPunct(".") && peekTok(toks, j+2).isIdent() {
			parts = append(parts, toks[j+2])
			j += 2
		}
		first := parts[0].name()
		if sr.tokFrame[i] != nil && sr.tokFrame[i].visible(first) {
			i = j
			continue
		}
		if len(parts) >= 2 && known[[2]string{first, parts[1].name()}] {
			i = j
			continue
		}
		texts := make([]string, len(parts))
		for k, p := range parts {
			texts[k] = p.text
		}
		return uerr("%s 的第一段不是语句里的表、别名或 WITH 名称：不带括号的 schema.函数 / 包.函数 调用在线查询不开放（列要写成 表别名.列名）", strings.Join(texts, "."))
	}
	return nil
}

// sqlWords 是不可能是函数名的关键字、类型名（不放进 BareNames，免得把核对的名字列表撑大）。
var sqlWords = setOf(
	"select", "from", "where", "and", "or", "not", "in", "is", "null", "like", "between", "exists", "any", "all", "some", "as", "on", "using", "join",
	"inner", "left", "right", "full", "outer", "cross", "natural", "union", "intersect", "minus", "except", "group", "by", "having", "order", "asc", "desc",
	"nulls", "first", "last", "distinct", "unique", "case", "when", "then", "else", "end", "with", "connect", "start", "prior", "fetch", "next", "rows",
	"row", "only", "offset", "over", "partition", "within", "keep", "interval", "to", "for", "at", "time", "zone", "local", "escape", "of", "versions",
	"between", "nocycle", "siblings", "rollup", "cube", "grouping", "sets", "lateral", "apply", "pivot", "unpivot", "sample", "seed", "dense_rank",
	"number", "varchar2", "nvarchar2", "varchar", "char", "nchar", "date", "timestamp", "integer", "int", "smallint", "float", "decimal", "numeric",
	"raw", "blob", "clob", "nclob", "binary_double", "binary_float", "day", "month", "year", "hour", "minute", "second", "scn", "ties", "percent",
	"value", "values", "returning", "passing", "columns", "path", "error", "default", "on", "empty", "content", "document", "wrapper", "format", "json",
	"true", "false", "unknown", "both", "leading", "trailing", "list", "limit", "top",
)

// maxBareNames 是语句里不同的“裸名字”个数上限。
const maxBareNames = 1500

// bareNames 收集语句里“不带点、不带括号、不是表名 / 别名 / 关键字”的名字：它们要么是列，要么是不带括号调用的无参函数
// （包括被同名自定义函数盖住的 SYSDATE、USER 之类），执行前核对库里没有同名的函数。
func bareNames(toks []token, sr *scanResult, heads map[int]bool) ([]string, error) {
	set := map[string]bool{}
	for i, t := range toks {
		if !t.isIdent() || sr.skip[i] || heads[i] {
			continue
		}
		if i > 0 && toks[i-1].isPunct(".") {
			continue
		}
		if nx := peekTok(toks, i+1); nx.isPunct(".") || nx.isPunct("(") {
			continue
		}
		if t.kind == tkWord && sqlWords[t.lower()] {
			continue
		}
		set[t.name()] = true
	}
	if len(set) > maxBareNames {
		return nil, uerr("语句里用到的名字太多（%d 个，上限 %d）：请拆成更小的查询", len(set), maxBareNames)
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// classifyRefs 把扫描到的表引用分成：需要库表授权的表、免授权的数据字典视图，并拒绝系统 schema 与内部视图。
func classifyRefs(ck *Checked, refs []tref) error {
	seen := map[sqlx.TableRef]bool{}
	dict := map[string]bool{}
	for _, r := range refs {
		owner, name := r.owner, r.name
		if err := validObjectName(name); err != nil {
			return err
		}
		if owner != "" {
			if err := validObjectName(owner); err != nil {
				return err
			}
		}
		switch {
		case owner == "":
			if dictViews[name] {
				dict[name] = true
				continue
			}
			if internalName(name) {
				return uerr("不允许访问 %s：数据库内部视图（DBA_* / V$* / GV$* / X$*）在线查询不开放", name)
			}
			owner = ck.DB
		case isSystemSchema(owner):
			if dictViews[name] {
				continue // SYS.ALL_TABLES 之类：数据字典视图，免授权
			}
			return uerr("不允许访问系统 schema 里的对象 %s.%s：在线查询只开放数据字典里的 %s", owner, name, dictList())
		case internalName(name):
			return uerr("不允许访问 %s.%s：数据库内部视图在线查询不开放", owner, name)
		}
		ref := sqlx.TableRef{Schema: owner, Name: name}
		if !seen[ref] {
			seen[ref] = true
			ck.Tables = append(ck.Tables, ref)
		}
	}
	for n := range dict {
		ck.DictRefs = append(ck.DictRefs, n)
	}
	sort.Strings(ck.DictRefs)
	return nil
}

// dictList 是 dictViews 的展示文本（排序）。
func dictList() string {
	names := make([]string, 0, len(dictViews))
	for n := range dictViews {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, " / ")
}
