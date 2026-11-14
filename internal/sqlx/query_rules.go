// @author: kevinkelly
// @license: Apache Licence
// @file: query_rules.go
// @time: 2026/10/28

package sqlx

import "strings"

// 本文件放“在线查询语句放行规则”用的词法判断：WITH（CTE）、DESC/DESCRIBE、EXPLAIN ANALYZE、mysql 系统库。
// 都只做词法级判断（忽略注释、字符串、反引号里的同名单词），宁可多拦不漏拦。

var stmtStartKeywords = map[string]bool{
	"select": true, "insert": true, "update": true, "delete": true, "replace": true, "table": true, "values": true, "with": true,
}

// WithMainKeyword 返回以 WITH 开头的语句的“主语句”关键字（小写）：跳过各个 CTE 定义（括号内的内容），
// 取括号深度为 0 的第一个语句关键字，如 select / insert / update / delete。无法识别时返回空串。
// 非 WITH 开头的语句也返回空串。
func WithMainKeyword(stmt string) string {
	sig := Significant(Tokenize(stmt))
	if len(sig) == 0 || sig[0].Kind != Word || sig[0].Lower() != "with" {
		return ""
	}
	depth := 0
	for _, t := range sig[1:] {
		switch {
		case t.Kind == Punct && t.Text == "(":
			depth++
		case t.Kind == Punct && t.Text == ")":
			depth--
		case depth == 0 && t.Kind == Word && stmtStartKeywords[t.Lower()] && t.Lower() != "with":
			return t.Lower()
		}
	}
	return ""
}

// HasWriteVerb 判断语句里是否出现写操作动词（INSERT / REPLACE / UPDATE / DELETE），用于拦截夹在 WITH 里的写语句。
// INSERT(...)、REPLACE(...) 作为字符串函数（后面紧跟左括号）不算；FOR UPDATE 另有检查，这里同样不计。
func HasWriteVerb(stmt string) bool {
	sig := Significant(Tokenize(stmt))
	for i, t := range sig {
		if t.Kind != Word {
			continue
		}
		next := ""
		if i+1 < len(sig) {
			next = sig[i+1].Text
		}
		prev := ""
		if i > 0 && sig[i-1].Kind == Word {
			prev = sig[i-1].Lower()
		}
		switch t.Lower() {
		case "insert", "replace":
			if next != "(" {
				return true
			}
		case "delete":
			return true
		case "update":
			if prev != "for" && prev != "key" { // FOR UPDATE / ON DUPLICATE KEY UPDATE
				return true
			}
		}
	}
	return false
}

// ExplainAnalyze 判断是否是 EXPLAIN ANALYZE（含 DESC / DESCRIBE 同义写法）。ANALYZE 会真正执行被解释的语句，在线查询必须拦截。
func ExplainAnalyze(stmt string) bool {
	sig := Significant(Tokenize(stmt))
	if len(sig) < 2 || sig[0].Kind != Word {
		return false
	}
	switch sig[0].Lower() {
	case "explain", "desc", "describe":
	default:
		return false
	}
	for _, t := range sig[1:] {
		if t.Kind != Word {
			continue
		}
		lw := t.Lower()
		if lw == "analyze" {
			return true
		}
		if stmtStartKeywords[lw] || lw == "for" {
			return false // 已经进入被解释的语句本体
		}
	}
	return false
}

// QueryKind 把语句归为在线查询流水线里的三类之一：select、show、explain；其余返回原第一个关键字（小写）。
//   - WITH … → select（调用方另行检查主语句是 SELECT、不含写动词）；
//   - DESC / DESCRIBE 后面跟表名 → show（等价 SHOW COLUMNS）；后面跟 SELECT 等语句、ANALYZE、FORMAT= 等 → explain；
//   - 其余保持第一个关键字。
func QueryKind(stmt string) string {
	kw := FirstKeyword(stmt)
	switch kw {
	case "with":
		return "select"
	case "desc", "describe":
		sig := Significant(Tokenize(stmt))
		if len(sig) < 2 {
			return "show"
		}
		n := sig[1]
		if n.Kind == Quoted {
			return "show"
		}
		if n.Kind == Word {
			switch lw := n.Lower(); {
			case stmtStartKeywords[lw], lw == "analyze", lw == "format", lw == "extended", lw == "partitions", lw == "for":
				return "explain"
			}
			return "show"
		}
		return "explain"
	}
	return kw
}

func identName(t Token) string {
	switch t.Kind {
	case Quoted:
		return strings.ToLower(strings.Trim(t.Text, "`\""))
	case Word:
		return t.Lower()
	}
	return ""
}

// TouchesMySQLSchema 判断语句是否访问 mysql 系统库：出现 mysql.<对象>（含 `mysql`.`user`、mysql . user）、SHOW … FROM/IN mysql，
// 或当前库就是 mysql 时引用了任何表。information_schema / performance_schema 不在此限（保持原有放行）。
func TouchesMySQLSchema(stmt, currentDB string) bool {
	sig := Significant(Tokenize(stmt))
	for i := 0; i+2 < len(sig); i++ {
		if identName(sig[i]) == "mysql" && sig[i+1].Kind == Punct && sig[i+1].Text == "." && (sig[i+2].Kind == Word || sig[i+2].Kind == Quoted) {
			return true
		}
	}
	kw := FirstKeyword(stmt)
	if kw == "show" || kw == "desc" || kw == "describe" || kw == "explain" {
		for i := 1; i+1 < len(sig); i++ {
			if sig[i].Kind == Word && (sig[i].Lower() == "from" || sig[i].Lower() == "in") && identName(sig[i+1]) == "mysql" {
				return true
			}
		}
	}
	if strings.EqualFold(currentDB, "mysql") {
		for _, r := range ExtractTables(stmt) {
			if r.Schema == "" {
				return true
			}
		}
		switch kw {
		case "desc", "describe":
			return true
		case "show":
			if len(sig) > 1 {
				switch sig[1].Lower() {
				case "databases", "schemas":
				default:
					return true
				}
			}
		}
	}
	return false
}
