// @author: kevinkelly
// @license: Apache Licence
// @file: lex.go
// @time: 2026/10/28

package dm

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// 词法：达梦的 SQL 文本切成 token。注释（含 /*+ 提示 */）直接丢掉，字符串与加引号的标识符整体识别，
// 所以后面的静态检查不会被字符串或注释里的内容骗到。

type tokKind uint8

const (
	tkWord   tokKind = iota // 未加引号的标识符 / 关键字
	tkQuoted                // "标识符"
	tkString                // '字符串'、n'字符串'
	tkNumber
	tkPunct
	tkBind // :name / :1
)

type token struct {
	kind       tokKind
	text       string // 原文
	val        string // 加引号的标识符 / 字符串的内容（已还原转义）
	start, end int    // 在原文里的字节位置
}

func (t token) isPunct(s string) bool { return t.kind == tkPunct && t.text == s }
func (t token) is(kw string) bool     { return t.kind == tkWord && strings.EqualFold(t.text, kw) }
func (t token) isIdent() bool         { return t.kind == tkWord || t.kind == tkQuoted }
func (t token) lower() string         { return strings.ToLower(t.text) }

// name 返回标识符在库里的真实名字：未加引号的折成大写，加引号的原样。
func (t token) name() string {
	if t.kind == tkQuoted {
		return t.val
	}
	return strings.ToUpper(t.text)
}

func isWordStart(r rune) bool { return r == '_' || unicode.IsLetter(r) }
func isWordPart(r rune) bool {
	return r == '_' || r == '$' || r == '#' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// twoCharPuncts 是两个字符的运算符。
var twoCharPuncts = map[string]bool{"||": true, "<=": true, ">=": true, "<>": true, "!=": true, "^=": true, ":=": true, "=>": true, "..": true, "**": true}

const singlePuncts = "()[]{},.;+-*/=<>%@&|!^~?#:\\"

func lex(src string) ([]token, error) {
	var toks []token
	n := len(src)
	i := 0
	for i < n {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
			continue
		case c == 0:
			return nil, uerr("SQL 含有空字符")
		case c == '-' && i+1 < n && src[i+1] == '-':
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				i = n
			} else {
				i += j + 1
			}
			continue
		case c == '/' && i+1 < n && src[i+1] == '*':
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				return nil, uerr("注释没有结束（缺少 */）")
			}
			i += 2 + j + 2
			continue
		case c == '\'':
			val, end, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tkString, text: src[i:end], val: val, start: i, end: end})
			i = end
			continue
		case c == '"':
			j := i + 1
			var sb strings.Builder
			closed := false
			for j < n {
				if src[j] == '"' {
					if j+1 < n && src[j+1] == '"' {
						sb.WriteByte('"')
						j += 2
						continue
					}
					closed = true
					j++
					break
				}
				sb.WriteByte(src[j])
				j++
			}
			if !closed {
				return nil, uerr("加双引号的标识符没有结束")
			}
			if sb.Len() == 0 {
				return nil, uerr("不允许空的标识符 \"\"")
			}
			toks = append(toks, token{kind: tkQuoted, text: src[i:j], val: sb.String(), start: i, end: j})
			i = j
			continue
		case c >= '0' && c <= '9', c == '.' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9':
			j := lexNumber(src, i)
			toks = append(toks, token{kind: tkNumber, text: src[i:j], start: i, end: j})
			i = j
			continue
		case c == ':':
			if i+1 < n {
				r, _ := utf8.DecodeRuneInString(src[i+1:])
				if isWordPart(r) {
					j := i + 1
					for j < n {
						r, sz := utf8.DecodeRuneInString(src[j:])
						if !isWordPart(r) {
							break
						}
						j += sz
					}
					toks = append(toks, token{kind: tkBind, text: src[i:j], start: i, end: j})
					i = j
					continue
				}
			}
		}
		r, sz := utf8.DecodeRuneInString(src[i:])
		if r == utf8.RuneError && sz == 1 {
			return nil, uerr("SQL 含有非法的 UTF-8 字节")
		}
		if isWordStart(r) {
			j := i + sz
			for j < n {
				r, sz := utf8.DecodeRuneInString(src[j:])
				if !isWordPart(r) {
					break
				}
				j += sz
			}
			word := src[i:j]
			// 紧跟单引号的词：n'…' 是国家字符集字符串（规则与普通字符串相同）；q'[…]' / e'…' / b'…' 这类前缀字符串的引号、转义规则
			// 与普通字符串不同，不同版本是否支持没有把握——词法和服务端对“字符串到哪里结束”的理解不一致就能藏进第二条语句，所以直接拒绝。
			if j < n && src[j] == '\'' {
				switch strings.ToLower(word) {
				case "q", "nq", "e", "b":
					return nil, uerr("不支持 %s'…' 这种带前缀的字符串写法，请改用普通字符串（字符串里的单引号写两个）", word)
				case "n":
					val, end, err := lexString(src, j)
					if err != nil {
						return nil, err
					}
					toks = append(toks, token{kind: tkString, text: src[i:end], val: val, start: i, end: end})
					i = end
					continue
				}
			}
			toks = append(toks, token{kind: tkWord, text: word, start: i, end: j})
			i = j
			continue
		}
		if i+1 < n && twoCharPuncts[src[i:i+2]] {
			toks = append(toks, token{kind: tkPunct, text: src[i : i+2], start: i, end: i + 2})
			i += 2
			continue
		}
		if strings.IndexByte(singlePuncts, c) >= 0 {
			toks = append(toks, token{kind: tkPunct, text: src[i : i+1], start: i, end: i + 1})
			i++
			continue
		}
		return nil, uerr("SQL 里有无法识别的字符 %q", string(r))
	}
	return toks, nil
}

// lexString 读 src[i]=='\” 开始的字符串，” 表示一个单引号。返回内容与结束位置。
func lexString(src string, i int) (string, int, error) {
	var sb strings.Builder
	j := i + 1
	for j < len(src) {
		if src[j] == '\'' {
			if j+1 < len(src) && src[j+1] == '\'' {
				sb.WriteByte('\'')
				j += 2
				continue
			}
			return sb.String(), j + 1, nil
		}
		sb.WriteByte(src[j])
		j++
	}
	return "", 0, uerr("字符串没有结束")
}

func lexNumber(src string, i int) int {
	n := len(src)
	j := i
	digits := func() {
		for j < n && src[j] >= '0' && src[j] <= '9' {
			j++
		}
	}
	digits()
	if j < n && src[j] == '.' && !(j+1 < n && src[j+1] == '.') {
		j++
		digits()
	}
	if j < n && (src[j] == 'e' || src[j] == 'E') {
		k := j + 1
		if k < n && (src[k] == '+' || src[k] == '-') {
			k++
		}
		if k < n && src[k] >= '0' && src[k] <= '9' {
			j = k
			digits()
		}
	}
	if j < n && (src[j] == 'f' || src[j] == 'F' || src[j] == 'd' || src[j] == 'D') {
		if j+1 >= n || !isWordPartByte(src[j+1]) {
			j++
		}
	}
	return j
}

func isWordPartByte(b byte) bool {
	return b == '_' || b == '$' || b == '#' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b >= 0x80
}
