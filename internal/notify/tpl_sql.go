// @author: kevinkelly
// @license: Apache Licence
// @file: tpl_sql.go
// @time: 2026/10/28

package notify

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 本文件：SQL 在消息里的显示方式 —— 截断、隐藏字符串常量（'…' → '***'）。
//
// 隐藏字符串常量是“尽力而为的脱敏”，目的是别让密钥 / 密码 / 手机号这类写在 SQL 常量里的值被推到群里：
//   - 单引号、双引号（MySQL 里是字符串，PG / SQL Server 里是标识符，也一并隐藏，宁多勿漏）、PG 的 $$…$$ 里的内容都会被替换；
//   - 注释里出现的引号同样处理（注释里常有人贴样例值）；
//   - 反斜杠转义在不同引擎里含义不同，所以按“识别 / 不识别反斜杠”各扫一遍，取两者遮盖范围的并集 —— 只可能多遮，不会漏；
//   - 先隐藏、再按字数截断，不会把一个常量切成两半后漏出前半截。
// 数字、十六进制（0x…）、标识符不处理：它们不是字符串常量。

var blankRuns = regexp.MustCompile(`\n[ \t]*(?:\n[ \t]*)+`)

func isIdentByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// litMask 返回 s 每个字节是否落在字符串常量（含两侧引号）里。backslash 为 true 时 \ 转义下一个字符。
func litMask(s string, backslash bool) []bool {
	n := len(s)
	m := make([]bool, n)
	mark := func(a, b int) {
		for k := a; k < b && k < n; k++ {
			m[k] = true
		}
	}
	// 从 i（引号 q 的位置）扫到常量结束，不超过 stop；返回结束位置（不含）。没闭合就一直遮到 stop。
	quoted := func(i int, q byte, stop int) int {
		j := i + 1
		for j < stop {
			c := s[j]
			if backslash && c == '\\' {
				j += 2
				continue
			}
			if c == q {
				if j+1 < stop && s[j+1] == q {
					j += 2
					continue
				}
				mark(i, j+1)
				return j + 1
			}
			j++
		}
		mark(i, stop)
		return stop
	}
	// 注释里的引号常量同样遮（在注释结束处截止）。
	inComment := func(from, end int) {
		for k := from; k < end; {
			if s[k] == '\'' || s[k] == '"' {
				k = quoted(k, s[k], end)
			} else {
				k++
			}
		}
	}
	for i := 0; i < n; {
		c := s[i]
		switch {
		case c == '-' && i+1 < n && s[i+1] == '-', c == '#':
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				end = n
			} else {
				end += i
			}
			inComment(i+1, end)
			i = end
		case c == '/' && i+1 < n && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				end = n
			} else {
				end += i + 2
			}
			inComment(i+2, end)
			i = end + 2
		case c == '\'' || c == '"':
			i = quoted(i, c, n)
		case c == '$' && (i == 0 || !isIdentByte(s[i-1])):
			// PG 的 $tag$ … $tag$
			j := i + 1
			for j < n && (s[j] == '_' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || j > i+1 && s[j] >= '0' && s[j] <= '9') {
				j++
			}
			if j < n && s[j] == '$' {
				tag := s[i : j+1]
				if k := strings.Index(s[j+1:], tag); k >= 0 {
					mark(i, j+1+k+len(tag))
					i = j + 1 + k + len(tag)
				} else {
					mark(i, n)
					i = n
				}
				continue
			}
			i++
		default:
			i++
		}
	}
	return m
}

// MaskStringLiterals 把 SQL 里的字符串常量替换成 '***'（双引号的换成 "***"，$$…$$ 的换成 $$***$$）；空常量 ” 保持原样。
func MaskStringLiterals(sql string) string {
	if !strings.ContainsAny(sql, `'"$`) && !strings.Contains(sql, "--") && !strings.Contains(sql, "/*") && !strings.Contains(sql, "#") {
		return sql
	}
	a, b := litMask(sql, true), litMask(sql, false)
	var out strings.Builder
	for i := 0; i < len(sql); {
		if !(a[i] || b[i]) {
			out.WriteByte(sql[i])
			i++
			continue
		}
		j := i
		for j < len(sql) && (a[j] || b[j]) {
			j++
		}
		out.WriteString(maskedRegion(sql[i:j]))
		i = j
	}
	return out.String()
}

// maskedRegion 把一段被遮盖的区间写成占位形式；区间可能是一个常量，也可能是几个相邻 / 重叠的常量并成的一段。
func maskedRegion(r string) string {
	switch {
	case r == `''` || r == `""`:
		return r
	case r[0] == '\'':
		return `'***'`
	case r[0] == '"':
		return `"***"`
	case r[0] == '$':
		if j := strings.IndexByte(r[1:], '$'); j >= 0 {
			tag := r[:j+2]
			return tag + "***" + tag
		}
	}
	return `'***'`
}

// 默认的 SQL 显示上限。
const (
	fullCodeCap  = 3000 // “完整显示”在消息里的字数上限（再受各渠道总大小限制）
	inlineSQLCap = 200  // {{sql}} 出现在代码块以外的位置时的字数上限（同时隐藏常量）
)

// SQLDisplay 按显示方式整理一段 SQL 文字。n 是 head / mask 的字数。返回显示文本与是否被截断。
func SQLDisplay(sql, mode string, n int) (string, bool) {
	sql = multiLine(sql)
	total := utf8.RuneCountInString(sql)
	switch mode {
	case ModeNone:
		return "（SQL 已隐藏，请登录平台查看）", false // i18n-ignore 通知正文 / 模板内容
	case ModeMask:
		sql = MaskStringLiterals(sql)
	case ModeFull:
		n = fullCodeCap
	default: // head
	}
	if n < MinCodeN {
		n = DefaultCodeN
	}
	if utf8.RuneCountInString(sql) <= n {
		return sql, false
	}
	r := []rune(sql)
	return strings.TrimRight(string(r[:n]), " \t\n") + fmt.Sprintf("\n…（已截断，共 %d 字，完整内容见工单）", total), true // i18n-ignore 通知正文 / 模板内容
}

// inlineSQL 是 {{sql}} 出现在标题 / 字段 / 文本里时的取值：隐藏常量并截短，防止一不小心把整段 SQL 推出去。
func inlineSQL(sql string) string {
	s := MaskStringLiterals(multiLine(sql))
	s = oneLine(s)
	return cutRunesEllipsis(s, inlineSQLCap)
}
