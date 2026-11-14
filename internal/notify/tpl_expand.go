// @author: kevinkelly
// @license: Apache Licence
// @file: tpl_expand.go
// @time: 2026/10/28

package notify

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// 本文件：模板里的 {{变量}} 与 {{#if 变量}}…{{/if}}。
//
// 只有这两种写法；变量名只能是 tpl_catalog.go 里登记的白名单。变量的值只当文字插入，不会再被当作模板解析，
// 所以 SQL 里恰好有 “{{x}}” 也不会被展开。

type tokKind int

const (
	tkText tokKind = iota
	tkVar
	tkIf
	tkEnd
)

type token struct {
	k tokKind
	s string // 文字，或变量名
}

const maxIfDepth = 4

func validKey(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c == '_' || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func lex(s string) ([]token, error) {
	var out []token
	depth := 0
	for len(s) > 0 {
		i := strings.Index(s, "{{")
		if i < 0 {
			out = append(out, token{tkText, s})
			break
		}
		if i > 0 {
			out = append(out, token{tkText, s[:i]})
		}
		s = s[i+2:]
		j := strings.Index(s, "}}")
		if j < 0 {
			return nil, fmt.Errorf("有一个“{{”没有对应的“}}”")
		}
		inner := strings.TrimSpace(s[:j])
		s = s[j+2:]
		switch {
		case inner == "/if":
			if depth == 0 {
				return nil, fmt.Errorf("多了一个“{{/if}}”，前面没有对应的“{{#if …}}”")
			}
			depth--
			out = append(out, token{tkEnd, ""})
		case strings.HasPrefix(inner, "#if") && (len(inner) == 3 || inner[3] == ' ' || inner[3] == '\t'):
			k := strings.TrimSpace(inner[3:])
			if !validKey(k) {
				return nil, fmt.Errorf("“{{%s}}”写法不对，应为 {{#if 变量名}}", inner)
			}
			depth++
			if depth > maxIfDepth {
				return nil, fmt.Errorf("{{#if}} 嵌套超过 %d 层", maxIfDepth)
			}
			out = append(out, token{tkIf, k})
		case validKey(inner):
			out = append(out, token{tkVar, inner})
		default:
			return nil, fmt.Errorf("不认识的写法“{{%s}}”（只支持 {{变量名}} 和 {{#if 变量名}}…{{/if}}）", cutRunesN(inner, 20))
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("有 %d 个“{{#if …}}”没有对应的“{{/if}}”", depth)
	}
	return out, nil
}

// Refs 返回 s 里引用到的变量名（去重，按出现顺序）；写法不对返回错误。
func Refs(s string) ([]string, error) {
	toks, err := lex(s)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, t := range toks {
		if (t.k == tkVar || t.k == tkIf) && !seen[t.s] {
			seen[t.s] = true
			out = append(out, t.s)
		}
	}
	return out, nil
}

// Expand 展开 s。get 返回变量的值（没有就返回空串）；{{#if k}} 在 get(k) 去掉空白后非空时保留内部内容。
func Expand(s string, get func(string) string) (string, error) {
	toks, err := lex(s)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var stack []bool // 每层 #if 是否成立
	on := func() bool {
		for _, v := range stack {
			if !v {
				return false
			}
		}
		return true
	}
	for _, t := range toks {
		switch t.k {
		case tkText:
			if on() {
				b.WriteString(t.s)
			}
		case tkVar:
			if on() {
				b.WriteString(get(t.s))
			}
		case tkIf:
			stack = append(stack, strings.TrimSpace(get(t.s)) != "")
		case tkEnd:
			stack = stack[:len(stack)-1]
		}
	}
	return b.String(), nil
}

// oneLine 把换行折成空格并去掉其它控制字符（标题、字段名、链接文字等单行位置）。
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// multiLine 去掉除换行外的控制字符，统一换行符，并把连续空行折成一个。
func multiLine(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return blankRuns.ReplaceAllString(strings.TrimSpace(s), "\n\n")
}

// cutRunesEllipsis 按字数截断，超出时末尾补“…”。
func cutRunesEllipsis(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
