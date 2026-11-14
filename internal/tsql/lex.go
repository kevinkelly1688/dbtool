// @author: kevinkelly
// @license: Apache Licence
// @file: lex.go
// @time: 2026/10/28

// Package tsql 是 SQL Server（T-SQL）脚本的静态分析：切分执行单元、语法解析、上线规则检查。
//
// 语法解析用 Bytebase 开源的 T-SQL ANTLR 语法（github.com/bytebase/parser/tsql，BSD-3），规则命名与思路参考
// Bytebase 的 SQL 审核（statement.where.require.update-delete、table.require-pk 等），详见 rules.go。
package tsql

import "strings"

// Kind 是词法单元类型。
type Kind int

const (
	Space   Kind = iota // 空白（含换行）
	Comment             // -- 行注释 或 /* */ 块注释（T-SQL 的块注释可嵌套）
	String              // '…'（''转义），含 N'…' 前缀
	Ident               // [标识符] 或 "标识符"
	Word                // 关键字/名称
	Number
	Variable // @x、@@x
	Punct    // 其他符号
)

type Token struct {
	Kind  Kind
	Text  string
	Start int // 在原文里的字节偏移
}

// Upper 返回 Word 的大写形式（其他类型返回原文）。
func (t Token) Upper() string {
	if t.Kind == Word {
		return strings.ToUpper(t.Text)
	}
	return t.Text
}

func isWordStart(c byte) bool {
	return c == '_' || c == '#' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}
func isWordPart(c byte) bool {
	return isWordStart(c) || c == '$' || (c >= '0' && c <= '9') || c == '@'
}

// Tokenize 把脚本切成词法单元；任何输入都能切分（未闭合的字符串/注释吞到结尾）。
func Tokenize(sql string) []Token {
	var out []Token
	n := len(sql)
	i := 0
	emit := func(k Kind, start, end int) { out = append(out, Token{Kind: k, Text: sql[start:end], Start: start}) }
	for i < n {
		c := sql[i]
		start := i
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f':
			for i < n && (sql[i] == ' ' || sql[i] == '\t' || sql[i] == '\r' || sql[i] == '\n' || sql[i] == '\f') {
				i++
			}
			emit(Space, start, i)
		case c == '-' && i+1 < n && sql[i+1] == '-':
			for i < n && sql[i] != '\n' {
				i++
			}
			emit(Comment, start, i)
		case c == '/' && i+1 < n && sql[i+1] == '*':
			depth := 0
			for i < n {
				if i+1 < n && sql[i] == '/' && sql[i+1] == '*' {
					depth++
					i += 2
					continue
				}
				if i+1 < n && sql[i] == '*' && sql[i+1] == '/' {
					depth--
					i += 2
					if depth == 0 {
						break
					}
					continue
				}
				i++
			}
			emit(Comment, start, i)
		case c == '\'' || ((c == 'N' || c == 'n') && i+1 < n && sql[i+1] == '\''):
			if c != '\'' {
				i++
			}
			i++ // 开头的引号
			for i < n {
				if sql[i] == '\'' {
					if i+1 < n && sql[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			emit(String, start, i)
		case c == '[':
			i++
			for i < n {
				if sql[i] == ']' {
					if i+1 < n && sql[i+1] == ']' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			emit(Ident, start, i)
		case c == '"':
			i++
			for i < n {
				if sql[i] == '"' {
					if i+1 < n && sql[i+1] == '"' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			emit(Ident, start, i)
		case c == '@':
			i++
			for i < n && isWordPart(sql[i]) {
				i++
			}
			emit(Variable, start, i)
		case c >= '0' && c <= '9':
			for i < n && (sql[i] >= '0' && sql[i] <= '9' || sql[i] == '.' || sql[i] == 'e' || sql[i] == 'E' || sql[i] == 'x' || sql[i] == 'X' || (sql[i] >= 'a' && sql[i] <= 'f') || (sql[i] >= 'A' && sql[i] <= 'F')) {
				i++
			}
			emit(Number, start, i)
		case isWordStart(c):
			for i < n && isWordPart(sql[i]) && sql[i] != '@' {
				i++
			}
			emit(Word, start, i)
		default:
			i++
			emit(Punct, start, i)
		}
	}
	return out
}

// Significant 去掉空白与注释。
func Significant(toks []Token) []Token {
	out := make([]Token, 0, len(toks))
	for _, t := range toks {
		if t.Kind != Space && t.Kind != Comment {
			out = append(out, t)
		}
	}
	return out
}

// StripComments 去掉注释（以一个空格替代）。
func StripComments(sql string) string {
	var b strings.Builder
	for _, t := range Tokenize(sql) {
		if t.Kind == Comment {
			b.WriteByte(' ')
			continue
		}
		b.WriteString(t.Text)
	}
	return b.String()
}

// Unquote 去掉标识符外面的 [] 或 ""，并还原转义。
func Unquote(s string) string {
	if len(s) >= 2 {
		switch {
		case s[0] == '[' && s[len(s)-1] == ']':
			return strings.ReplaceAll(s[1:len(s)-1], "]]", "]")
		case s[0] == '"' && s[len(s)-1] == '"':
			return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
		}
	}
	return s
}
