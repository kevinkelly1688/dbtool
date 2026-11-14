// @author: kevinkelly
// @license: Apache Licence
// @file: ident.go
// @time: 2026/10/28

package engine

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// validIdent 判断库名 / 表名 / 列名是否是引擎愿意拼进 SQL 的标识符：1–64 个字符，只含字母（含中文等）、数字、下划线、$、连字符。
// 无论是否通过校验，拼 SQL 时一律加反引号（quoteIdent）；这里是第二道防线。
func validIdent(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$' || r == '-') {
			return false
		}
	}
	return true
}

// quoteIdent 给标识符加反引号（内部反引号加倍）。
func quoteIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// qualified 返回 `db`.`table`。
func qualified(db, table string) string { return quoteIdent(db) + "." + quoteIdent(table) }

func quoteIdents(cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = quoteIdent(c)
	}
	return strings.Join(parts, ", ")
}

// wildMatch 大小写不敏感的通配匹配：* 任意长度，? 单个字符。
func wildMatch(pattern, s string) bool {
	p, t := []rune(strings.ToLower(pattern)), []rune(strings.ToLower(s))
	pi, ti, star, mark := 0, 0, -1, 0
	for ti < len(t) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == t[ti]):
			pi++
			ti++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ti
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ti = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// builtinProtected 是无论策略怎么配都不允许归档的系统库。
var builtinProtected = []string{"mysql", "information_schema", "performance_schema", "sys"}

// IsProtected 判断库表是否命中受保护模式（策略里的模式 + 内置系统库）。模式是 "db" 或 "db.table"，支持 * ?，不区分大小写；
// "db" 只匹配库；table 为空时只比较库。
func IsProtected(patterns []string, db, table string) bool {
	all := append(append([]string{}, builtinProtected...), patterns...)
	for _, p := range all {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if i := strings.Index(p, "."); i >= 0 {
			if table != "" && wildMatch(p[:i], db) && wildMatch(p[i+1:], table) {
				return true
			}
			continue
		}
		if wildMatch(p, db) {
			return true
		}
	}
	return false
}
