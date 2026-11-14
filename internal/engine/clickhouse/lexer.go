// @author: kevinkelly
// @license: Apache Licence
// @file: lexer.go
// @time: 2026/10/28

package clickhouse

import (
	"fmt"
	"strings"
)

// ClickHouse SQL 的词法分析。
//
// 静态检查完全建立在词法层上：字符串、带引号的标识符、注释、heredoc 都要按服务端的规则切分，
// 否则用户可以把关键字藏进注释 / 字符串，或者反过来让我们误以为一段危险文本在字符串里。
// 凡是我们与服务端可能理解不一致的写法（# 注释、标识符里的转义序列、非 ASCII 的裸字符、控制字符），
// 一律直接拒绝，而不是猜测。

type tokKind uint8

const (
	tkWord   tokKind = iota // 裸标识符 / 关键字
	tkQuoted                // `ident` 或 "ident"
	tkString                // 'str' 或 $$heredoc$$
	tkNumber
	tkPunct
)

type token struct {
	kind  tokKind
	text  string // 原文
	val   string // tkQuoted / tkString：去掉引号并处理 '' 与 \\ 之后的值
	esc   bool   // 带引号的值里含有除 \\ 和引号自身之外的反斜杠转义（值我们没有还原，不能拿来当名字用）
	start int
	end   int
}

func (t token) lower() string { return strings.ToLower(t.text) }

// is 判断是否为（未加引号的）关键字 / 裸标识符 kw，大小写不敏感。
func (t token) is(kw string) bool { return t.kind == tkWord && strings.EqualFold(t.text, kw) }

func (t token) isPunct(p string) bool { return t.kind == tkPunct && t.text == p }

// isIdent 判断是否可以当名字用：裸标识符或带引号的标识符。
func (t token) isIdent() bool { return t.kind == tkWord || t.kind == tkQuoted }

// name 返回标识符的名字（带引号的取去引号后的值）。
func (t token) name() string {
	if t.kind == tkQuoted {
		return t.val
	}
	return t.text
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isWordStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isWordChar(c byte) bool { return isWordStart(c) || isDigit(c) }

// pos 把字节偏移换算成“第 L 行第 C 列”，用于错误提示。
func pos(src string, off int) string {
	if off > len(src) {
		off = len(src)
	}
	line, col := 1, 1
	for i := 0; i < off; i++ {
		if src[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return fmt.Sprintf("第 %d 行第 %d 列", line, col)
}

// lex 把 SQL 文本切成记号（注释与空白被丢弃）。遇到不能确定含义的写法返回 *UserError。
func lex(src string) ([]token, error) {
	var toks []token
	n := len(src)
	i := 0
	for i < n {
		c := src[i]
		switch {
		case isSpace(c):
			i++
		case c == 0 || c < 0x20 || c == 0x7f:
			return nil, uerr("SQL 在%s含有控制字符，已拒绝", pos(src, i))
		case c >= 0x80:
			return nil, uerr("SQL 在%s含有非 ASCII 字符（引号、注释之外）；标识符里的中文等字符请用反引号括起来", pos(src, i))
		case c == '-' && i+1 < n && src[i+1] == '-':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				return nil, uerr("SQL 在%s有未结束的 /* 注释", pos(src, i))
			}
			i += 2 + j + 2
		case c == '#':
			return nil, uerr("SQL 在%s含有 # 字符；不支持 # 注释，请改用 -- 或 /* */", pos(src, i))
		case c == '\'':
			t, end, err := lexQuoted(src, i, '\'', tkString)
			if err != nil {
				return nil, err
			}
			toks = append(toks, t)
			i = end
		case c == '"' || c == '`':
			t, end, err := lexQuoted(src, i, c, tkQuoted)
			if err != nil {
				return nil, err
			}
			if t.esc {
				return nil, uerr("SQL 在%s的带引号标识符里含有转义序列，已拒绝（标识符里只允许用重复引号表示引号本身）", pos(src, i))
			}
			toks = append(toks, t)
			i = end
		case c == '$':
			if t, end, ok, err := lexHeredoc(src, i); err != nil {
				return nil, err
			} else if ok {
				toks = append(toks, t)
				i = end
				continue
			}
			j := i + 1
			for j < n && (isWordChar(src[j]) || src[j] == '$') {
				j++
			}
			toks = append(toks, token{kind: tkWord, text: src[i:j], start: i, end: j})
			i = j
		case isDigit(c) || (c == '.' && i+1 < n && isDigit(src[i+1])):
			j := lexNumber(src, i)
			toks = append(toks, token{kind: tkNumber, text: src[i:j], start: i, end: j})
			i = j
		case isWordStart(c):
			j := i + 1
			for j < n && (isWordChar(src[j]) || src[j] == '$') {
				j++
			}
			toks = append(toks, token{kind: tkWord, text: src[i:j], start: i, end: j})
			i = j
		default:
			toks = append(toks, token{kind: tkPunct, text: src[i : i+1], start: i, end: i + 1})
			i++
		}
	}
	return toks, nil
}

// lexNumber 返回数字字面量的结束位置。字母数字下划线与小数点都吞进去（服务端对“数字后紧跟字母”同样报错），
// 指数记号 1e+5 / 0x1p-3 的符号位也算在内。
func lexNumber(src string, i int) int {
	n := len(src)
	j := i + 1
	hex := len(src) > i+1 && src[i] == '0' && (src[i+1] == 'x' || src[i+1] == 'X')
	for j < n {
		ch := src[j]
		switch {
		case isWordChar(ch) || ch == '.':
			j++
		case (ch == '+' || ch == '-') && !hex && (src[j-1] == 'e' || src[j-1] == 'E'):
			j++
		case (ch == '+' || ch == '-') && hex && (src[j-1] == 'p' || src[j-1] == 'P'):
			j++
		default:
			return j
		}
	}
	return j
}

// lexQuoted 读取 q 引起来的片段：\x 转义跳过下一个字符，两个连续的 q 表示 q 本身。
func lexQuoted(src string, i int, q byte, kind tokKind) (token, int, error) {
	n := len(src)
	var sb strings.Builder
	esc := false
	j := i + 1
	for j < n {
		ch := src[j]
		switch {
		case ch == '\\':
			if j+1 >= n {
				return token{}, 0, uerr("SQL 在%s有未结束的引号", pos(src, i))
			}
			nx := src[j+1]
			if nx == '\\' || nx == q {
				sb.WriteByte(nx)
			} else {
				esc = true
				sb.WriteByte('\\')
				sb.WriteByte(nx)
			}
			j += 2
		case ch == q:
			if j+1 < n && src[j+1] == q {
				sb.WriteByte(q)
				j += 2
				continue
			}
			return token{kind: kind, text: src[i : j+1], val: sb.String(), esc: esc, start: i, end: j + 1}, j + 1, nil
		default:
			sb.WriteByte(ch)
			j++
		}
	}
	return token{}, 0, uerr("SQL 在%s有未结束的引号", pos(src, i))
}

// lexHeredoc 识别 $$text$$ 与 $tag$text$tag$；不是 heredoc 起始返回 ok=false。
func lexHeredoc(src string, i int) (token, int, bool, error) {
	n := len(src)
	j := i + 1
	if j < n && src[j] == '$' {
		// $$
	} else if j < n && isWordStart(src[j]) {
		for j < n && isWordChar(src[j]) {
			j++
		}
		if j >= n || src[j] != '$' {
			return token{}, 0, false, nil
		}
	} else {
		return token{}, 0, false, nil
	}
	open := src[i : j+1]
	body := j + 1
	k := strings.Index(src[body:], open)
	if k < 0 {
		return token{}, 0, false, uerr("SQL 在%s有未结束的 %s 字符串", pos(src, i), open)
	}
	end := body + k + len(open)
	return token{kind: tkString, text: src[i:end], val: src[body : body+k], start: i, end: end}, end, true, nil
}
