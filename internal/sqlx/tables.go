// @author: kevinkelly
// @license: Apache Licence
// @file: tables.go
// @time: 2026/10/28

package sqlx

import "strings"

// TableRef 是语句里引用的表。Schema 为空表示未显式写库名（调用方用当前库补全）。
type TableRef struct {
	Schema string
	Name   string
}

func unquote(t Token) string {
	if t.Kind == Quoted {
		return strings.Trim(t.Text, "`\"")
	}
	return t.Text
}

// 这些关键字会结束同一括号层级里的 FROM 列表。
var fromTerminators = map[string]bool{
	"where": true, "group": true, "having": true, "order": true, "limit": true, "union": true,
	"intersect": true, "except": true, "window": true, "for": true, "lock": true, "into": true,
	"procedure": true, "set": true, "offset": true, "select": true,
}

// ExtractTables 用词法扫描提取 SELECT 语句引用的表（含子查询、JOIN、逗号连接、括号嵌套连接）。
//
// 这是词法级的保守实现，用于没有 goInception 的环境的库表权限校验：宁可多报（把 CTE 名、
// 表函数名当成表）也不漏报；遇到拿不准的写法时多报会导致“无权限”，不会放行。
// 生产环境的权限校验优先使用 goInception 的语法树。
func ExtractTables(stmt string) []TableRef {
	sig := Significant(Tokenize(stmt))
	var out []TableRef
	seen := map[TableRef]bool{}
	add := func(r TableRef) {
		if r.Name == "" || strings.EqualFold(r.Name, "dual") || seen[r] {
			return
		}
		seen[r] = true
		out = append(out, r)
	}

	// 每个括号层级一个状态：是否处在 FROM 列表中，以及下一个标识符是否应是表名。
	type level struct{ inFrom, expect bool }
	stack := []level{{}}
	top := func() *level { return &stack[len(stack)-1] }

	isIdent := func(t Token) bool { return t.Kind == Word || t.Kind == Quoted }
	for i := 0; i < len(sig); i++ {
		t := sig[i]
		lw := t.Lower()
		switch {
		case t.Kind == Punct && t.Text == "(":
			// 在“期待表名”的位置遇到括号：子查询 (select/with/values) 或嵌套连接 (a join b)
			derived := false
			if top().expect && i+1 < len(sig) {
				n := sig[i+1].Lower()
				derived = n == "select" || n == "with" || n == "values" || n == "table"
			}
			nested := top().expect && !derived
			top().expect = false
			stack = append(stack, level{inFrom: nested, expect: nested})
		case t.Kind == Punct && t.Text == ")":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case t.Kind == Punct && t.Text == ",":
			if top().inFrom {
				top().expect = true
			}
		case t.Kind == Word && (lw == "from" || lw == "join"):
			top().inFrom, top().expect = true, true
		case t.Kind == Word && top().inFrom && fromTerminators[lw]:
			top().inFrom, top().expect = false, false
		case top().expect && t.Kind == Word && lw == "lateral":
			// LATERAL (subquery)：保持“期待表名”，括号处理见上
		case top().expect && isIdent(t):
			ref := TableRef{Name: unquote(t)}
			if i+2 < len(sig) && sig[i+1].Kind == Punct && sig[i+1].Text == "." && isIdent(sig[i+2]) {
				ref = TableRef{Schema: unquote(t), Name: unquote(sig[i+2])}
				i += 2
			}
			add(ref)
			top().expect = false
		}
	}
	return out
}

// DangerousFunctions 返回语句里出现的、普通查询用户不应使用的函数名（小写）。
// 这些函数可读服务器文件、制造长时间占用或锁，且与是否有表权限无关。
func DangerousFunctions(stmt string) []string {
	danger := map[string]bool{
		"load_file": true, "benchmark": true, "sleep": true, "get_lock": true, "release_lock": true,
		"release_all_locks": true, "master_pos_wait": true, "source_pos_wait": true, "sys_exec": true, "sys_eval": true,
	}
	sig := Significant(Tokenize(stmt))
	var out []string
	for i := 0; i+1 < len(sig); i++ {
		if sig[i].Kind == Word && danger[sig[i].Lower()] && sig[i+1].Kind == Punct && sig[i+1].Text == "(" {
			out = append(out, sig[i].Lower())
		}
	}
	return out
}

// 普通用户允许执行的 SHOW 子句（只涉及库表结构，不泄露会话、权限、复制状态等运维信息）。
var safeShow = map[string]bool{
	"databases": true, "schemas": true, "tables": true, "columns": true, "fields": true,
	"index": true, "indexes": true, "keys": true, "full": true, "create": true,
}

// IsSafeShow 判断 SHOW 语句是否属于普通用户可执行的结构查询。
func IsSafeShow(stmt string) bool {
	sig := Significant(Tokenize(stmt))
	if len(sig) < 2 || sig[0].Lower() != "show" {
		return false
	}
	w := sig[1].Lower()
	if w == "table" && len(sig) > 2 && sig[2].Lower() == "status" {
		return true
	}
	if w == "create" {
		return len(sig) > 2 && (sig[2].Lower() == "table" || sig[2].Lower() == "view")
	}
	if w == "full" {
		return len(sig) > 2 && safeShow[sig[2].Lower()] && sig[2].Lower() != "full" && sig[2].Lower() != "create"
	}
	return safeShow[w]
}
