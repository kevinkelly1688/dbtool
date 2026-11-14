// @author: kevinkelly
// @license: Apache Licence
// @file: lex.go
// @time: 2026/10/28

package postgres

import (
	"fmt"
	"strings"
)

// PostgreSQL 词法分析器。
//
// 在线查询的安全检查建立在“按 PostgreSQL 自己的词法规则切词”之上：字符串、E'' 转义串、美元引号串、
// 带引号的标识符、嵌套块注释、行注释都被整体识别成一个 token，检查代码只看 token，不对原文做正则匹配，
// 所以藏在字符串 / 注释 / 引号标识符里的关键字不会误伤，也不能借它们藏住真正的语句。
//
// 凡是无法确定与服务端切词结果一致的写法（U& 转义、$1 参数、数字后紧跟字母、未闭合的引号 / 注释 …）一律报错拒绝。
// 连接时用启动参数把 standard_conforming_strings 固定为 on、client_encoding 固定为 UTF8，
// 所以这里的规则（普通 '…' 里反斜杠不是转义）与服务端一致；见 conn.go 对这两个参数的二次校验。

type tokKind int

const (
	tWord   tokKind = iota // 未加引号的标识符 / 关键字（low 是按 ASCII 折叠成小写后的形式）
	tQuoted                // "ident"，text 是去掉引号并还原 "" 之后的内容
	tString                // 字符串字面量（含 E'' 与美元引号串），text 是原文片段
	tNumber                // 数字
	tOp                    // 运算符串
	tPunct                 // ( ) [ ] , ; . :
)

type token struct {
	kind       tokKind
	text       string
	low        string // tWord：ASCII 小写；tQuoted：内容的 ASCII 小写（只用于黑名单比对）；其它为 text
	start, end int    // 在原文中的字节区间 [start, end)
}

func (t token) isWord(w string) bool  { return t.kind == tWord && t.low == w }
func (t token) isPunct(p string) bool { return t.kind == tPunct && t.text == p }
func (t token) isIdentLike() bool     { return t.kind == tWord || t.kind == tQuoted }
func (t token) isOp(op string) bool   { return t.kind == tOp && t.text == op }
func (t token) isOneOf(ws ...string) bool {
	if t.kind != tWord {
		return false
	}
	for _, w := range ws {
		if t.low == w {
			return true
		}
	}
	return false
}

type span struct{ start, end int }

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c >= 0x80
}
func isIdentCont(c byte) bool { return isIdentStart(c) || isDigit(c) || c == '$' }
func isOpChar(c byte) bool {
	switch c {
	case '+', '-', '*', '/', '<', '>', '=', '~', '!', '@', '#', '%', '^', '&', '|', '`', '?':
		return true
	}
	return false
}
func isHexDigit(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// asciiLower 只折叠 ASCII 字母，与 PostgreSQL 对多字节编码的标识符折叠规则一致。
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// lexError 带行列号的词法错误。
func lexError(src string, pos int, format string, a ...any) error {
	line, col := 1, 1
	for i := 0; i < pos && i < len(src); i++ {
		if src[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return uerr("SQL 解析失败（第 %d 行第 %d 列）：%s", line, col, fmt.Sprintf(format, a...))
}

// lex 把 src 切成 token（注释与空白不进入结果，注释的区间单独返回，用于生成去注释的语句文本）。
func lex(src string) (toks []token, comments []span, err error) {
	n := len(src)
	i := 0
	for i < n {
		c := src[i]
		switch {
		case c == 0:
			return nil, nil, lexError(src, i, "SQL 中含有 NUL 字符")
		case isSpaceByte(c):
			i++
		case c == '-' && i+1 < n && src[i+1] == '-':
			j := i + 2
			for j < n && src[j] != '\n' && src[j] != '\r' { // PostgreSQL 的行注释在 \n 或 \r 处结束
				j++
			}
			comments = append(comments, span{i, j})
			i = j
		case c == '/' && i+1 < n && src[i+1] == '*':
			depth, j := 1, i+2
			for j < n && depth > 0 {
				switch {
				case src[j] == '/' && j+1 < n && src[j+1] == '*':
					depth++
					j += 2
				case src[j] == '*' && j+1 < n && src[j+1] == '/':
					depth--
					j += 2
				default:
					j++
				}
			}
			if depth > 0 {
				return nil, nil, lexError(src, i, "块注释没有结束（缺少 */）")
			}
			comments = append(comments, span{i, j})
			i = j
		case c == '\'':
			j, e := scanQuoted(src, i, '\'', false)
			if e != nil {
				return nil, nil, e
			}
			toks = append(toks, token{kind: tString, text: src[i:j], low: src[i:j], start: i, end: j})
			i = j
		case c == '"':
			j, e := scanQuoted(src, i, '"', false)
			if e != nil {
				return nil, nil, e
			}
			content := strings.ReplaceAll(src[i+1:j-1], `""`, `"`)
			if content == "" {
				return nil, nil, lexError(src, i, "不允许零长度的带引号标识符 \"\"")
			}
			if strings.IndexByte(content, 0) >= 0 {
				return nil, nil, lexError(src, i, "标识符含有 NUL 字符")
			}
			toks = append(toks, token{kind: tQuoted, text: content, low: asciiLower(content), start: i, end: j})
			i = j
		case isIdentStart(c):
			j := i + 1
			for j < n && isIdentCont(src[j]) {
				j++
			}
			word := src[i:j]
			// E'…' 转义串：整个 token 恰好是 e / E 且紧跟单引号
			if (word == "e" || word == "E") && j < n && src[j] == '\'' {
				k, e := scanQuoted(src, j, '\'', true)
				if e != nil {
					return nil, nil, e
				}
				toks = append(toks, token{kind: tString, text: src[i:k], low: src[i:k], start: i, end: k})
				i = k
				break
			}
			// U&'…' / U&"…"：Unicode 转义可以拼出任意标识符或字符，无法与服务端逐字对齐，直接拒绝
			if (word == "u" || word == "U") && j+1 < n && src[j] == '&' && (src[j+1] == '\'' || src[j+1] == '"') {
				return nil, nil, lexError(src, i, "不支持 U& 形式的 Unicode 转义字符串 / 标识符")
			}
			toks = append(toks, token{kind: tWord, text: word, low: asciiLower(word), start: i, end: j})
			i = j
		case c == '$':
			if i+1 < n && isDigit(src[i+1]) {
				return nil, nil, lexError(src, i, "在线查询不支持参数占位符 $n，请直接写入字面量")
			}
			// 美元引号串 $tag$ … $tag$
			j := i + 1
			if j < n && isIdentStart(src[j]) {
				for j < n && isIdentCont(src[j]) && src[j] != '$' {
					j++
				}
			}
			if j >= n || src[j] != '$' {
				return nil, nil, lexError(src, i, "无法识别的 $ 符号")
			}
			tag := src[i : j+1]
			end := strings.Index(src[j+1:], tag)
			if end < 0 {
				return nil, nil, lexError(src, i, "美元引号字符串没有结束（缺少 %s）", tag)
			}
			k := j + 1 + end + len(tag)
			toks = append(toks, token{kind: tString, text: src[i:k], low: src[i:k], start: i, end: k})
			i = k
		case isDigit(c) || (c == '.' && i+1 < n && isDigit(src[i+1])):
			j, e := scanNumber(src, i)
			if e != nil {
				return nil, nil, e
			}
			toks = append(toks, token{kind: tNumber, text: src[i:j], low: src[i:j], start: i, end: j})
			i = j
		case isOpChar(c):
			j := i
			for j < n && isOpChar(src[j]) {
				if src[j] == '-' && j+1 < n && src[j+1] == '-' {
					break
				}
				if src[j] == '/' && j+1 < n && src[j+1] == '*' {
					break
				}
				j++
			}
			if j == i { // 不可能：-- 与 /* 在上面的分支已处理
				return nil, nil, lexError(src, i, "无法识别的字符")
			}
			toks = append(toks, token{kind: tOp, text: src[i:j], low: src[i:j], start: i, end: j})
			i = j
		case c == '(' || c == ')' || c == '[' || c == ']' || c == ',' || c == ';' || c == '.' || c == ':':
			toks = append(toks, token{kind: tPunct, text: string(c), low: string(c), start: i, end: i + 1})
			i++
		default:
			return nil, nil, lexError(src, i, "不允许的字符 %q", string(rune(c)))
		}
	}
	return toks, comments, nil
}

// scanQuoted 从 src[i]（引号）起扫描到配对的结束引号，返回结束引号之后的位置。
// 引号连写（” / ""）表示引号本身；escape=true（E” 串）时反斜杠转义下一个字节。
func scanQuoted(src string, i int, quote byte, escape bool) (int, error) {
	n := len(src)
	j := i + 1
	for j < n {
		switch {
		case escape && src[j] == '\\':
			j += 2
		case src[j] == quote:
			if j+1 < n && src[j+1] == quote {
				j += 2
				continue
			}
			return j + 1, nil
		default:
			if src[j] == 0 {
				return 0, lexError(src, j, "字符串含有 NUL 字符")
			}
			j++
		}
	}
	what := "字符串"
	if quote == '"' {
		what = "带引号的标识符"
	}
	return 0, lexError(src, i, "%s没有结束（缺少 %s）", what, string(quote))
}

// scanNumber 扫描数字：十进制（含小数、指数、下划线分隔）与 0x / 0o / 0b 整数；数字后紧跟标识符字符视为非法
// （PostgreSQL 16+ 同样报错，更早版本会拆成“数字 + 标识符”，行为不一致，宁可拒绝）。
func scanNumber(src string, i int) (int, error) {
	n := len(src)
	j := i
	if src[j] == '0' && j+2 < n && (src[j+1] == 'x' || src[j+1] == 'X' || src[j+1] == 'o' || src[j+1] == 'O' || src[j+1] == 'b' || src[j+1] == 'B') &&
		(isHexDigit(src[j+2]) || src[j+2] == '_') {
		j += 2
		for j < n && (isHexDigit(src[j]) || src[j] == '_') {
			j++
		}
	} else {
		for j < n && (isDigit(src[j]) || (src[j] == '_' && j+1 < n && isDigit(src[j+1]))) {
			j++
		}
		if j < n && src[j] == '.' && !(j+1 < n && src[j+1] == '.') {
			j++
			for j < n && (isDigit(src[j]) || (src[j] == '_' && j+1 < n && isDigit(src[j+1]))) {
				j++
			}
		}
		if j < n && (src[j] == 'e' || src[j] == 'E') {
			k := j + 1
			if k < n && (src[k] == '+' || src[k] == '-') {
				k++
			}
			if k < n && isDigit(src[k]) {
				for k < n && isDigit(src[k]) {
					k++
				}
				j = k
			}
		}
	}
	if j < n && isIdentStart(src[j]) {
		return 0, lexError(src, i, "数字后面紧跟了字母或下划线")
	}
	return j, nil
}

// stmtRange 是一条语句在 token 序列中的区间 [from, to)（不含结尾分号）。
type stmtRange struct{ from, to int }

// splitStatements 按顶层分号切分；空语句（连续分号）被丢弃。
func splitStatements(toks []token) []stmtRange {
	var out []stmtRange
	from := 0
	for i, t := range toks {
		if t.isPunct(";") {
			if i > from {
				out = append(out, stmtRange{from, i})
			}
			from = i + 1
		}
	}
	if from < len(toks) {
		out = append(out, stmtRange{from, len(toks)})
	}
	return out
}

// stripComments 返回 src[start:end] 中把注释区间换成单个空格后的文本（注释在 PostgreSQL 里等价于空白）。
func stripComments(src string, start, end int, comments []span) string {
	var sb strings.Builder
	pos := start
	for _, c := range comments {
		if c.end <= start || c.start >= end {
			continue
		}
		if c.start > pos {
			sb.WriteString(src[pos:c.start])
		}
		sb.WriteByte(' ')
		if c.end > pos {
			pos = c.end
		}
	}
	if pos < end {
		sb.WriteString(src[pos:end])
	}
	return strings.TrimSpace(sb.String())
}
