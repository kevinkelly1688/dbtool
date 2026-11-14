// @author: kevinkelly
// @license: Apache Licence
// @file: viewexp.go
// @time: 2026/10/28

package masking

import (
	"errors"
	"sort"
	"strings"
)

// 视图展开：视图就是一条存起来的子查询，配置写在基表字段上时，经过视图改名（phone AS mobile）、套表达式
// （left(phone, 3) AS head）、套另一个视图之后，列名和语句文本里都看不出它来自敏感字段。
//
// 做法：查询时从目标库读出语句用到的视图的定义（连同定义里又用到的视图，递归），把它们当作“带别名的子查询”
// 交给保守匹配（ConservativeHitsViews）：视图定义里的别名、表达式引用、自定义函数调用像子查询 / CTE 一样把污染
// 传给视图的输出列，外层语句再引用这些列就脱敏。视图定义只用于分析，不会被执行，也不会改动要执行的语句。
// 取视图定义由查询层按引擎做（见 internal/query/viewexp.go），这里只管文本：找出语句引用了哪些对象（TableNames）、
// 校验定义能不能分析（CheckViewBody）、把定义喂给匹配器（ViewBody）。

// ViewBody 是一个视图的查询体：视图定义里 CREATE VIEW … AS 之后的 SELECT。
type ViewBody struct {
	Body string
	// Columns 是视图定义里显式写出的列名清单（SQL Server 的 CREATE VIEW v (a, b) AS …）；没有写为空。
	// 写了的话视图的输出列名按位置取自这份清单，而不是查询体里的别名。
	Columns []string
	// Positional 是目录里读到的视图实际列名（Oracle / 达梦：定义文本里没有列清单）。只在最外层 SELECT 列表没有 * 、项数与它一致时
	// 按位置对上：视图体里的别名可能被 CREATE VIEW v (列, …) 改了名。有 * 时位置不可知，仍按列名匹配（视图列名与基表列名相同）。
	Positional []string
}

// items 返回查询体里各 SELECT 列表的项（污染传播用），以及视图的输出列名是否不可知。
func (v ViewBody) items(d Dialect) (items []selItem, wild bool) {
	toks := tokenize(v.Body, d)
	lists := selectLists(toks, d)
	if len(lists) == 0 {
		return nil, false
	}
	if d == DialectClickHouse {
		items = withAliasItems(toks, d)
	}
	minDepth := lists[0].depth
	for _, sl := range lists {
		if sl.depth < minDepth {
			minDepth = sl.depth
		}
	}
	for _, sl := range lists {
		wild = wild || sl.udfStar // 查询体里 select * from 自定义表值函数(…)：输出的列名取决于函数，认不出来
		its := sl.items
		if sl.depth == minDepth {
			switch {
			case len(v.Columns) > 0:
				its = renameByColumnList(sl, v.Columns)
			case len(v.Positional) > 0 && !sl.star && len(sl.items) == len(v.Positional):
				its = renameByColumnList(sl, v.Positional)
			}
		}
		items = append(items, its...)
	}
	return items, wild
}

// renameByColumnList 把视图最外层 SELECT 列表的项按位置改成显式列清单里的名字。
// 列表里有 * 时位置不可知：清单里的每个名字都按“不知道带进了哪些字段”处理（取配置里最严的一条）。
func renameByColumnList(sl selList, cols []string) []selItem {
	out := make([]selItem, 0, len(cols))
	if sl.star {
		for _, c := range cols {
			out = append(out, selItem{out: strings.ToLower(strings.TrimSpace(c)), star: true})
		}
		return out
	}
	for i, it := range sl.items {
		it.out = ""
		if i < len(cols) {
			it.out = strings.ToLower(strings.TrimSpace(cols[i]))
		}
		out = append(out, it)
	}
	return out
}

// CheckViewBody 粗查视图查询体能不能拿来分析：字符串 / 引号标识符 / 块注释都有结束符，括号配平，至少有一个 SELECT。
// 通不过说明这段文本按本方言切分会走样（比如把后面的内容吃进了一个没结束的字符串），不能用来分析。
func CheckViewBody(body string, d Dialect) error {
	toks, clean := tokenizeEx(body, d)
	if !clean {
		return errors.New("字符串、引号标识符或注释没有结束") // i18n-ignore 只进日志
	}
	opens, closes, hasSelect := 0, 0, false
	for _, t := range toks {
		switch {
		case t.kind == tPunct && (t.text == "(" || (t.text == "[" && d != DialectMSSQL)):
			opens++
		case t.kind == tPunct && (t.text == ")" || (t.text == "]" && d != DialectMSSQL)):
			closes++
		case t.kind == tIdent && t.lower == "select":
			hasSelect = true
		}
	}
	if opens != closes {
		return errors.New("括号没有配平") // i18n-ignore 只进日志
	}
	if !hasSelect {
		return errors.New("里面没有 SELECT") // i18n-ignore 只进日志
	}
	return nil
}

// NameRef 是语句里表位置上的一个对象名。1 段名只有 Name；2 段名是 Schema.Name（MySQL 里 Schema 就是库名）；
// 3 段名是 Catalog.Schema.Name（SQL Server 的 库.架构.对象）。引号去掉；PostgreSQL 里没加引号的名字已按服务端规则折成小写，Oracle / 达梦折成大写。
type NameRef struct{ Catalog, Schema, Name string }

// TableNames 返回语句里出现在表位置的对象名：FROM / JOIN / APPLY 之后的名字，以及它们后面逗号连接的名字，
// 按在语句里出现的先后、去重。函数调用（FROM unnest(…)）、#临时表、@表变量、派生表不算。子查询、CTE 里的同样会收进来。
// 其中的 CTE 名等不是库里对象的名字会一并返回，由调用方去库里查，查不到的忽略。
func TableNames(sql string, d Dialect) []NameRef {
	toks := tokenize(sql, d)
	type found struct {
		ref NameRef
		pos int
	}
	var got []found
	seen := map[NameRef]bool{}
	add := func(r NameRef, pos int) {
		if r.Name != "" && !seen[r] {
			seen[r] = true
			got = append(got, found{r, pos})
		}
	}
	for i, t := range toks {
		if t.kind != tIdent {
			continue
		}
		switch t.lower {
		case "from":
			if i > 0 && toks[i-1].kind == tIdent && toks[i-1].lower == "distinct" {
				continue // a IS DISTINCT FROM b
			}
			sourceList(toks, i+1, d, add)
		case "join", "apply", "straight_join":
			sourceList(toks, i+1, d, add)
		}
	}
	sort.SliceStable(got, func(i, j int) bool { return got[i].pos < got[j].pos })
	out := make([]NameRef, len(got))
	for i, f := range got {
		out[i] = f.ref
	}
	return out
}

// 同一层上结束 FROM 子句的词。
var fromEnd = map[string]bool{
	"where": true, "group": true, "having": true, "order": true, "limit": true, "union": true, "intersect": true,
	"except": true, "minus": true, "window": true, "qualify": true, "fetch": true, "offset": true, "for": true,
	"option": true, "settings": true, "format": true, "prewhere": true, "into": true, "returning": true,
}

func isPunct(t token, s string) bool { return t.kind == tPunct && t.text == s }

// sourceList 从 toks[p] 起解析一个表位置：一个名字（或派生表、括起来的连接组），以及它之后同一层上逗号连接的下一个、再下一个……
func sourceList(toks []token, p int, d Dialect, add func(NameRef, int)) {
	for p < len(toks) {
		for p < len(toks) && toks[p].kind == tIdent && (toks[p].lower == "lateral" || toks[p].lower == "only") {
			p++
		}
		if p >= len(toks) {
			return
		}
		dp := toks[p].depth
		if isPunct(toks[p], "(") {
			if derivedAt(toks, p) {
				p++ // 派生表没有名字；下面从它的内容之后、按括号外这一层找逗号
			} else {
				// 括起来的连接组 (a join b …)：进去取第一个名字，逗号也按里面这一层找
				for p < len(toks) && isPunct(toks[p], "(") {
					p++
				}
				if p >= len(toks) {
					return
				}
				dp = toks[p].depth
				var ok bool
				if p, ok = oneName(toks, p, d, add); !ok {
					return
				}
			}
		} else {
			var ok bool
			if p, ok = oneName(toks, p, d, add); !ok {
				return
			}
		}
		// 同一层上找下一个逗号（逗号连接的下一个表）；碰到结束 FROM 子句的词、分号或上一层的括号就结束。
		found := false
		for ; p < len(toks); p++ {
			tk := toks[p]
			if tk.depth < dp {
				return
			}
			if tk.depth != dp {
				continue
			}
			if isPunct(tk, ",") {
				p++
				found = true
				break
			}
			if isPunct(tk, ";") || (tk.kind == tIdent && fromEnd[tk.lower]) {
				return
			}
		}
		if !found {
			return
		}
	}
}

// derivedAt 判断 toks[p]（一个 "("）开头的是不是派生表：(select …) / (with …) / (values …)。
func derivedAt(toks []token, p int) bool {
	if p+1 >= len(toks) || toks[p+1].kind != tIdent {
		return false
	}
	switch toks[p+1].lower {
	case "select", "with", "values":
		return true
	}
	return false
}

// oneName 解析 toks[p] 起的一个名字并登记，返回它之后的位置。ok 为 false：这里不是名字，调用方不再往后找。
func oneName(toks []token, p int, d Dialect, add func(NameRef, int)) (next int, ok bool) {
	ref, next, ok := nameChain(toks, p, d)
	if ok {
		add(ref, p) // ref.Name 为空（函数调用、临时表等）时 add 忽略
	}
	return next, ok
}

// nameChain 解析 toks[p] 起的一串 名字(.名字)*（点可以连着写：a..b 表示省略中间一段）。
// ok 为 false 表示 toks[p] 不是名字。是名字但不是库里的对象（函数调用、#临时表、@表变量、超过 3 段）时 ok 为 true、ref 为空，
// 调用方照常往后找逗号。
func nameChain(toks []token, p int, d Dialect) (ref NameRef, next int, ok bool) {
	var parts []string
	i := p
	for i < len(toks) && isNameTok(toks[i]) {
		t := toks[i]
		text := t.text
		if d == DialectPostgres && t.kind == tIdent {
			text = t.lower // PostgreSQL 没加引号的名字折成小写，加了引号的保持原样
		}
		if isOracleFamily(d) && t.kind == tIdent {
			text = strings.ToUpper(t.text) // Oracle / 达梦没加引号的名字折成大写，加了引号的保持原样
		}
		parts = append(parts, text)
		i++
		if i < len(toks) && isPunct(toks[i], ".") {
			i++
			for i < len(toks) && isPunct(toks[i], ".") {
				parts = append(parts, "")
				i++
			}
			continue
		}
		break
	}
	if len(parts) == 0 {
		return NameRef{}, p, false
	}
	call := i < len(toks) && isPunct(toks[i], "(")
	last := parts[len(parts)-1]
	if call || last == "" || strings.HasPrefix(last, "#") || strings.HasPrefix(last, "@") {
		return NameRef{}, i, true
	}
	switch len(parts) {
	case 1:
		ref = NameRef{Name: parts[0]}
	case 2:
		ref = NameRef{Schema: parts[0], Name: parts[1]}
	case 3:
		ref = NameRef{Catalog: parts[0], Schema: parts[1], Name: parts[2]}
	}
	return ref, i, true
}

// withAliasItems 找出 ClickHouse 的 `WITH <表达式> AS <别名>[, …] SELECT …`：别名取的是表达式的值，要像 select 列表里的别名一样传递污染
// （WITH phone AS p SELECT p …）。标准的 CTE `WITH name AS (SELECT …)` 最后一个词是 ")"，analyzeItem 取不到别名，不受影响。
// 只看语句 / 子查询开头的 WITH（前面是开头、"("、";" 或 UNION / ALL 之类），GROUP BY … WITH TOTALS、LIMIT … WITH TIES 里的 WITH 不算。
func withAliasItems(toks []token, d Dialect) []selItem {
	var out []selItem
	for i, t := range toks {
		if t.kind != tIdent || t.lower != "with" {
			continue
		}
		if i > 0 {
			prev := toks[i-1]
			switch {
			case isPunct(prev, "(") || isPunct(prev, ";"):
			case prev.kind == tIdent && (prev.lower == "all" || prev.lower == "distinct" || prev.lower == "union" || prev.lower == "intersect" || prev.lower == "except"):
			default:
				continue
			}
		}
		dp := t.depth
		var cur []token
		flush := func() {
			if len(cur) > 0 {
				out = append(out, analyzeItem(cur, d))
				cur = nil
			}
		}
		for j := i + 1; j < len(toks); j++ {
			tk := toks[j]
			if tk.depth < dp {
				break
			}
			if tk.depth == dp {
				if tk.kind == tIdent && tk.lower == "select" || isPunct(tk, ";") {
					break
				}
				if isPunct(tk, ",") {
					flush()
					continue
				}
			}
			cur = append(cur, tk)
		}
		flush()
	}
	return out
}
