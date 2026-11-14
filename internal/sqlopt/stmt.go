// @author: kevinkelly
// @license: Apache Licence
// @file: stmt.go
// @time: 2026/10/28

package sqlopt

import (
	"strings"

	"dbtool/internal/sqlx"
)

// pstmt 是一条语句的词法视图：去掉空白与注释的词序列 + 每个词之前的括号深度。
// 规则在它上面做判断；它不是语法树，写法特别的语句可能判断不准，规则的结论都是“建议”。
type pstmt struct {
	sql   string
	sig   []sqlx.Token
	depth []int // depth[i] 是第 i 个词之前的括号深度（"(" 自身的深度是括号外的深度）
	kind  string
}

func parseStmt(sql string) *pstmt {
	p := &pstmt{sql: sql, sig: sqlx.Significant(sqlx.Tokenize(sql))}
	p.depth = make([]int, len(p.sig))
	d := 0
	for i, t := range p.sig {
		if t.Kind == sqlx.Punct && t.Text == ")" && d > 0 {
			d--
		}
		p.depth[i] = d
		if t.Kind == sqlx.Punct && t.Text == "(" {
			d++
		}
	}
	p.kind = p.detectKind()
	return p
}

// w 返回第 i 个词的小写形式（仅 Word），越界或非单词返回空串。
func (p *pstmt) w(i int) string {
	if i < 0 || i >= len(p.sig) || p.sig[i].Kind != sqlx.Word {
		return ""
	}
	return p.sig[i].Lower()
}

func (p *pstmt) punct(i int, s string) bool {
	return i >= 0 && i < len(p.sig) && p.sig[i].Kind == sqlx.Punct && p.sig[i].Text == s
}

func (p *pstmt) detectKind() string {
	i := 0
	for i < len(p.sig) && p.punct(i, "(") {
		i++
	}
	switch p.w(i) {
	case "select", "table", "values":
		return TypeSelect
	case "insert":
		return TypeInsert
	case "replace":
		return TypeReplace
	case "update":
		return TypeUpdate
	case "delete":
		return TypeDelete
	case "create", "alter", "drop", "truncate", "rename":
		return TypeDDL
	case "with":
		// CTE 定义都在括号里，括号外第一个 select/insert/update/delete 才是主语句
		for j := i + 1; j < len(p.sig); j++ {
			if p.depth[j] != p.depth[i] {
				continue
			}
			switch p.w(j) {
			case "select":
				return TypeSelect
			case "insert":
				return TypeInsert
			case "update":
				return TypeUpdate
			case "delete":
				return TypeDelete
			}
		}
		return TypeSelect
	}
	return TypeOther
}

// find 返回括号深度为 depth、位于 [lo,hi) 的某个关键字的位置，找不到返回 -1。
func (p *pstmt) find(lo, hi, depth int, words ...string) int {
	if hi > len(p.sig) {
		hi = len(p.sig)
	}
	for i := lo; i < hi; i++ {
		if p.depth[i] != depth || p.sig[i].Kind != sqlx.Word {
			continue
		}
		lw := p.sig[i].Lower()
		for _, w := range words {
			if lw == w {
				return i
			}
		}
	}
	return -1
}

// top 在括号外找关键字。
func (p *pstmt) top(words ...string) int { return p.find(0, len(p.sig), 0, words...) }

// endOfClause 返回从 start 开始的子句在深度 depth 上的结束位置（遇到 stop 里的关键字、或括号闭合、或语句结束）。
func (p *pstmt) endOfClause(start, depth int, stop map[string]bool) int {
	for i := start; i < len(p.sig); i++ {
		if p.depth[i] < depth {
			return i
		}
		if p.depth[i] == depth && p.sig[i].Kind == sqlx.Word && stop[p.sig[i].Lower()] {
			return i
		}
	}
	return len(p.sig)
}

var whereStop = map[string]bool{"group": true, "having": true, "order": true, "limit": true, "union": true, "intersect": true,
	"except": true, "for": true, "lock": true, "into": true, "window": true, "procedure": true}

// matchParen 返回与 open 位置的 "(" 配对的 ")" 位置，找不到返回 -1。
func (p *pstmt) matchParen(open int) int {
	d := p.depth[open]
	for i := open + 1; i < len(p.sig); i++ {
		if p.depth[i] == d && p.punct(i, ")") {
			return i
		}
	}
	return -1
}

// text 返回 [lo,hi] 闭区间词对应的原文。
func (p *pstmt) text(lo, hi int) string {
	if lo < 0 || hi >= len(p.sig) || lo > hi {
		return ""
	}
	return p.sql[p.sig[lo].Start:p.sig[hi].End]
}

func ident(t sqlx.Token) string {
	if t.Kind == sqlx.Quoted {
		return strings.Trim(t.Text, "`\"")
	}
	return t.Text
}

func (p *pstmt) isIdent(i int) bool {
	return i >= 0 && i < len(p.sig) && (p.sig[i].Kind == sqlx.Word || p.sig[i].Kind == sqlx.Quoted)
}

// 不能当作表别名 / 列名的保留字。
var reserved = map[string]bool{
	"select": true, "from": true, "where": true, "and": true, "or": true, "not": true, "in": true, "is": true, "null": true,
	"like": true, "between": true, "exists": true, "true": true, "false": true, "case": true, "when": true, "then": true,
	"else": true, "end": true, "join": true, "inner": true, "left": true, "right": true, "outer": true, "cross": true,
	"natural": true, "straight_join": true, "on": true, "using": true, "group": true, "order": true, "by": true,
	"having": true, "limit": true, "offset": true, "union": true, "all": true, "distinct": true, "as": true, "set": true,
	"values": true, "value": true, "into": true, "for": true, "lock": true, "update": true, "delete": true, "insert": true,
	"interval": true, "asc": true, "desc": true, "regexp": true, "rlike": true, "div": true, "mod": true, "xor": true,
	"year": true, "month": true, "day": true, "hour": true, "minute": true, "second": true, "week": true, "quarter": true,
	"microsecond": true, "signed": true, "unsigned": true, "char": true, "binary": true, "decimal": true, "date": true,
	"datetime": true, "time": true, "json": true, "separator": true, "with": true, "partition": true, "window": true,
	"intersect": true, "except": true, "ignore": true, "low_priority": true, "quick": true, "force": true, "use": true,
	"index": true, "key": true, "escape": true, "sounds": true, "collate": true, "current_date": true, "current_timestamp": true,
}

// isColumnAt 判断第 i 个词是不是列引用（标识符，不是保留字，后面也不是 "("）；返回列名与占用的词数（a.b.c 取最后一段）。
func (p *pstmt) columnAt(i int) (name string, n int, ok bool) {
	if !p.isIdent(i) {
		return "", 0, false
	}
	if p.sig[i].Kind == sqlx.Word && reserved[p.sig[i].Lower()] {
		return "", 0, false
	}
	n = 1
	name = ident(p.sig[i])
	for p.punct(i+n, ".") && p.isIdent(i+n+1) {
		name = ident(p.sig[i+n+1])
		n += 2
	}
	if p.punct(i+n, "(") {
		return "", 0, false // 函数调用
	}
	return name, n, true
}

// qualifierAt 返回第 i 个词开头的列引用的限定名（a.b 里的 a，没有为空）。
func (p *pstmt) qualifierAt(i int) string {
	if p.isIdent(i) && p.punct(i+1, ".") && p.isIdent(i+2) {
		return ident(p.sig[i])
	}
	return ""
}

// tableRef 是语句里引用的一张表及其别名。
type tableRef struct {
	Schema, Name, Alias string
}

// tableRefs 提取语句引用的表（含别名），覆盖 SELECT 的 FROM/JOIN（含子查询）、UPDATE、DELETE、INSERT/REPLACE INTO。
func (p *pstmt) tableRefs() []tableRef {
	var out []tableRef
	seen := map[[3]string]bool{}
	add := func(r tableRef) {
		k := [3]string{strings.ToLower(r.Schema), strings.ToLower(r.Name), strings.ToLower(r.Alias)}
		if r.Name == "" || strings.EqualFold(r.Name, "dual") || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, r)
	}
	// readRef 从 i 读一个 [schema.]name [[as] alias]，返回下一个位置。
	readRef := func(i int) (tableRef, int, bool) {
		if !p.isIdent(i) {
			return tableRef{}, i, false
		}
		if p.sig[i].Kind == sqlx.Word && reserved[p.sig[i].Lower()] {
			return tableRef{}, i, false
		}
		r := tableRef{Name: ident(p.sig[i])}
		i++
		if p.punct(i, ".") && p.isIdent(i+1) {
			r.Schema, r.Name = r.Name, ident(p.sig[i+1])
			i += 2
		}
		if p.w(i) == "as" && p.isIdent(i+1) {
			r.Alias = ident(p.sig[i+1])
			i += 2
		} else if p.isIdent(i) && !(p.sig[i].Kind == sqlx.Word && reserved[p.sig[i].Lower()]) && !p.punct(i+1, "(") {
			r.Alias = ident(p.sig[i])
			i++
		}
		return r, i, true
	}
	type level struct{ inFrom, expect bool }
	stack := []level{{}}
	top := func() *level { return &stack[len(stack)-1] }
	for i := 0; i < len(p.sig); i++ {
		t := p.sig[i]
		lw := p.w(i)
		switch {
		case t.Kind == sqlx.Punct && t.Text == "(":
			derived := false
			if top().expect && i+1 < len(p.sig) {
				n := p.w(i + 1)
				derived = n == "select" || n == "with" || n == "values" || n == "table"
			}
			nested := top().expect && !derived
			top().expect = false
			stack = append(stack, level{inFrom: nested, expect: nested})
		case t.Kind == sqlx.Punct && t.Text == ")":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case t.Kind == sqlx.Punct && t.Text == ",":
			if top().inFrom {
				top().expect = true
			}
		case lw == "from" || lw == "join":
			top().inFrom, top().expect = true, true
		case lw == "update" && p.w(i-1) != "for" && p.w(i-1) != "key", // for update / on duplicate key update 不是表
			lw == "into" && (p.kind == TypeInsert || p.kind == TypeReplace) && len(stack) == 1:
			top().inFrom, top().expect = true, true
		case lw != "" && top().inFrom && (whereStop[lw] || lw == "where" || lw == "set" || lw == "select" || lw == "values" || lw == "value" || lw == "partition"):
			top().inFrom, top().expect = false, false
		case top().expect && (lw == "lateral" || lw == "low_priority" || lw == "ignore" || lw == "quick"):
		case top().expect:
			if r, next, ok := readRef(i); ok {
				add(r)
				top().expect = false
				i = next - 1
			}
		}
	}
	return out
}

// tableNames 供展示：schema.name 形式，去重。
func (p *pstmt) tableNames() []string {
	var out []string
	for _, r := range p.tableRefs() {
		n := r.Name
		if r.Schema != "" {
			n = r.Schema + "." + r.Name
		}
		out = append(out, n)
	}
	return out
}
