// @author: kevinkelly
// @license: Apache Licence
// @file: extract.go
// @time: 2026/10/28

// Package extract 扫描模板 / JS / Go 源码里的中文文案：找出没被翻译函数包起来的中文，以及代码里用到的翻译键。
// 供 internal/i18n 的闸门测试与 cmd/dbt-i18n 命令行工具共用。
//
// 约定：同一行里出现 i18n-ignore 的中文一律放行（用于确实不该翻译的字面量：存库的枚举值、与引擎输出做匹配的文本等）。
package extract

import (
	"go/scanner"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Finding 是一处位置信息。
type Finding struct {
	File string
	Line int
	Text string
}

// HasHan 判断字符串里是否有汉字。
func HasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func lineIgnored(lines []string, n int) bool {
	return n-1 >= 0 && n-1 < len(lines) && strings.Contains(lines[n-1], "i18n-ignore")
}

// ---- 模板 ----

var (
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	tCallArgRe    = regexp.MustCompile("\\b(?:t|th)\\s+(\"(?:[^\"\\\\]|\\\\.)*\"|`[^`]*`)")
)

func blankKeepNewlines(s string) string { return strings.Repeat("\n", strings.Count(s, "\n")) }

// ScanTemplate 返回模板里没被 {{t "…"}} / {{th "…"}} 包起来的中文：标签文本、属性值、以及动作里的字符串字面量。
func ScanTemplate(file, src string) []Finding {
	lines := strings.Split(src, "\n")
	src = htmlCommentRe.ReplaceAllStringFunc(src, blankKeepNewlines)
	var out []Finding
	add := func(line int, text string) {
		if !lineIgnored(lines, line) {
			out = append(out, Finding{file, line, strings.TrimSpace(text)})
		}
	}
	line := 1
	i := 0
	for i < len(src) {
		j := strings.Index(src[i:], "{{")
		if j < 0 {
			j = len(src) - i
		}
		for n, seg := range strings.Split(src[i:i+j], "\n") { // 文本段：逐行检查
			if HasHan(seg) {
				add(line+n, seg)
			}
		}
		line += strings.Count(src[i:i+j], "\n")
		i += j
		if i >= len(src) {
			break
		}
		// 动作：找到配对的 }}（跳过字符串字面量）
		k := i + 2
		for k < len(src) {
			c := src[k]
			if c == '"' || c == '`' || c == '\'' {
				k++
				for k < len(src) && src[k] != c {
					if src[k] == '\\' && c != '`' {
						k++
					}
					k++
				}
				k++
				continue
			}
			if c == '}' && k+1 < len(src) && src[k+1] == '}' {
				break
			}
			k++
		}
		if k >= len(src) {
			k = len(src) - 2
		}
		action := src[i+2 : k]
		trim := strings.TrimSpace(strings.Trim(action, "-"))
		if !strings.HasPrefix(trim, "/*") {
			rest := tCallArgRe.ReplaceAllString(action, "")
			if HasHan(rest) {
				add(line, "{{"+strings.TrimSpace(rest)+"}}")
			}
		}
		line += strings.Count(src[i:k+2], "\n")
		i = k + 2
	}
	return out
}

// ---- JS ----

type jsScanner struct {
	file  string
	s     string
	i     int
	line  int
	prev  byte   // 上一个非空白字符（判断 / 是除号还是正则）
	word  string // 紧邻 prev 的标识符（return / typeof 之后的 / 是正则）
	out   []Finding
	lines []string
}

var jsTNames = []string{"t", "tHTML"}

func isIdent(c byte) bool {
	return c == '_' || c == '$' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

// viaT 判断位置 start（字符串字面量起点）是否紧跟在 t( / tHTML( 之后（允许空白）。
func (sc *jsScanner) viaT(start int) bool {
	j := start - 1
	for j >= 0 && (sc.s[j] == ' ' || sc.s[j] == '\t' || sc.s[j] == '\n' || sc.s[j] == '\r') {
		j--
	}
	if j < 0 || sc.s[j] != '(' {
		return false
	}
	j--
	for j >= 0 && (sc.s[j] == ' ' || sc.s[j] == '\t') {
		j--
	}
	end := j + 1
	for j >= 0 && isIdent(sc.s[j]) {
		j--
	}
	name := sc.s[j+1 : end]
	if j >= 0 && sc.s[j] == '.' { // obj.t( 不算
		return false
	}
	for _, n := range jsTNames {
		if name == n {
			return true
		}
	}
	return false
}

func (sc *jsScanner) flag(line int, text string) {
	if !lineIgnored(sc.lines, line) {
		sc.out = append(sc.out, Finding{sc.file, line, strings.TrimSpace(text)})
	}
}

func (sc *jsScanner) regexAllowed() bool {
	switch sc.prev {
	case 0, '(', ',', '=', ':', '[', '!', '&', '|', '?', '{', '}', ';', '+', '-', '*', '%', '<', '>', '~', '^':
		return true
	}
	switch sc.word {
	case "return", "typeof", "case", "in", "of", "void", "delete", "new", "throw", "else", "do":
		return true
	}
	return false
}

func (sc *jsScanner) str(q byte) {
	start, line := sc.i, sc.line
	sc.i++
	var b strings.Builder
	for sc.i < len(sc.s) && sc.s[sc.i] != q {
		c := sc.s[sc.i]
		if c == '\\' && sc.i+1 < len(sc.s) {
			if sc.s[sc.i+1] == '\n' {
				sc.line++
			}
			b.WriteByte(sc.s[sc.i+1])
			sc.i += 2
			continue
		}
		if c == '\n' { // 未闭合：按行尾收场，避免把后面整个文件吞掉
			break
		}
		b.WriteByte(c)
		sc.i++
	}
	sc.i++
	if text := b.String(); HasHan(text) && !sc.viaT(start) {
		sc.flag(line, string(q)+text+string(q))
	}
	sc.prev, sc.word = q, ""
}

func (sc *jsScanner) tmpl() {
	start, line := sc.i, sc.line
	sc.i++
	var b strings.Builder
	hasExpr := false
	for sc.i < len(sc.s) {
		c := sc.s[sc.i]
		switch {
		case c == '\\' && sc.i+1 < len(sc.s):
			if sc.s[sc.i+1] == '\n' {
				sc.line++
			}
			b.WriteByte(sc.s[sc.i+1])
			sc.i += 2
		case c == '`':
			sc.i++
			text := b.String()
			if HasHan(text) && (hasExpr || !sc.viaT(start)) {
				sc.flag(line, "`"+text+"`")
			}
			sc.prev, sc.word = '`', ""
			return
		case c == '$' && sc.i+1 < len(sc.s) && sc.s[sc.i+1] == '{':
			hasExpr = true
			b.WriteString("${…}")
			sc.i += 2
			sc.code(true)
		default:
			if c == '\n' {
				sc.line++
			}
			b.WriteByte(c)
			sc.i++
		}
	}
}

func (sc *jsScanner) regex() {
	sc.i++
	inClass := false
	for sc.i < len(sc.s) {
		c := sc.s[sc.i]
		switch {
		case c == '\\':
			sc.i++
		case c == '\n':
			return
		case c == '[':
			inClass = true
		case c == ']':
			inClass = false
		case c == '/' && !inClass:
			sc.i++
			for sc.i < len(sc.s) && (sc.s[sc.i] >= 'a' && sc.s[sc.i] <= 'z') { // 标志位
				sc.i++
			}
			sc.prev, sc.word = ')', ""
			return
		}
		sc.i++
	}
}

func (sc *jsScanner) code(stopAtBrace bool) {
	depth := 0
	for sc.i < len(sc.s) {
		c := sc.s[sc.i]
		switch {
		case c == '\n':
			sc.line++
			sc.i++
		case c == ' ' || c == '\t' || c == '\r':
			sc.i++
		case c == '/' && sc.i+1 < len(sc.s) && sc.s[sc.i+1] == '/':
			for sc.i < len(sc.s) && sc.s[sc.i] != '\n' {
				sc.i++
			}
		case c == '/' && sc.i+1 < len(sc.s) && sc.s[sc.i+1] == '*':
			end := strings.Index(sc.s[sc.i+2:], "*/")
			if end < 0 {
				end = len(sc.s) - sc.i - 2
			}
			sc.line += strings.Count(sc.s[sc.i:sc.i+2+end], "\n")
			sc.i += 2 + end + 2
		case c == '\'' || c == '"':
			sc.str(c)
		case c == '`':
			sc.tmpl()
		case c == '/' && sc.regexAllowed():
			sc.regex()
		case c == '{':
			depth++
			sc.i++
			sc.prev, sc.word = '{', ""
		case c == '}':
			if stopAtBrace && depth == 0 {
				sc.i++
				return
			}
			depth--
			sc.i++
			sc.prev, sc.word = '}', ""
		case isIdent(c):
			j := sc.i
			for j < len(sc.s) && isIdent(sc.s[j]) {
				j++
			}
			sc.word, sc.prev = sc.s[sc.i:j], sc.s[j-1]
			sc.i = j
		default:
			sc.prev, sc.word = c, ""
			sc.i++
		}
	}
}

// ScanJS 返回 JS 里没被 t('…') 包起来的、含中文的字符串字面量（注释不算）。
func ScanJS(file, src string) []Finding {
	sc := &jsScanner{file: file, s: src, line: 1, lines: strings.Split(src, "\n")}
	sc.code(false)
	return sc.out
}

// ---- Go ----

// Literal 是 Go 源码里一个含中文的字符串字面量。
type Literal struct {
	Line int
	Text string // 去掉引号、处理转义后的内容
	Log  bool   // 出现在日志调用里（slog / log），不面向用户
}

// GoLiterals 返回 Go 源码里含中文的字符串字面量（注释不算；同行有 i18n-ignore 的跳过）。
func GoLiterals(file, src string) []Literal {
	fset := token.NewFileSet()
	f := fset.AddFile(file, fset.Base(), len(src))
	lines := strings.Split(src, "\n")
	var s scanner.Scanner
	s.Init(f, []byte(src), nil, 0)
	var out []Literal
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok != token.STRING || !HasHan(lit) {
			continue
		}
		ln := fset.Position(pos).Line
		if lineIgnored(lines, ln) {
			continue
		}
		text, err := strconv.Unquote(lit)
		if err != nil {
			continue
		}
		cur := lines[ln-1]
		prev := ""
		if ln >= 2 {
			prev = lines[ln-2]
		}
		isLog := logCallRe.MatchString(cur) || (strings.HasSuffix(strings.TrimSpace(prev), "(") && logCallRe.MatchString(prev))
		out = append(out, Literal{Line: ln, Text: text, Log: isLog})
	}
	return out
}

var logCallRe = regexp.MustCompile(`\b(?:slog|log)\.(?:Debug|Info|Warn|Error|Print|Printf|Println|Fatal|Fatalf|Panic|Panicf)\w*\(`)

// ---- 键（模板 / JS 里显式翻译调用的第一个参数） ----

var (
	tplKeyRe = regexp.MustCompile("\\b(?:t|th)\\s+(\"(?:[^\"\\\\]|\\\\.)*\"|`[^`]*`)")
	jsKeyRe  = regexp.MustCompile(`\b(?:t|tHTML)\(\s*('(?:[^'\\\n]|\\.)*'|"(?:[^"\\\n]|\\.)*"|` + "`[^`$]*`" + `)`)
)

// Unquote 还原模板 / JS 字符串字面量（支持 "…"、'…'、`…`）。
func Unquote(lit string) (string, bool) {
	if len(lit) < 2 {
		return "", false
	}
	switch lit[0] {
	case '`':
		return lit[1 : len(lit)-1], true
	case '"':
		s, err := strconv.Unquote(lit)
		return s, err == nil
	case '\'':
		s, err := strconv.Unquote(`"` + strings.ReplaceAll(strings.ReplaceAll(lit[1:len(lit)-1], `\'`, `'`), `"`, `\"`) + `"`)
		return s, err == nil
	}
	return "", false
}

// needsEntry 判断显式翻译调用的键是否必须在目录里有译文：含汉字，或含全角标点等非 ASCII 字符
// （如 "、|sep"、"{name}（{type}）"，英文里要换成半角标点）。纯 ASCII 的键（占位符 "{n}" 之类）不要求。
func needsEntry(k string) bool {
	for _, r := range k {
		if r > 0x7f {
			return true
		}
	}
	return false
}

func keysFrom(re *regexp.Regexp, src string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		if k, ok := Unquote(m[1]); ok && needsEntry(k) {
			out = append(out, k)
		}
	}
	return out
}

// TemplateKeys 返回模板里 {{t "…"}} / {{th "…"}} 的键（含中文的）。
func TemplateKeys(src string) []string { return keysFrom(tplKeyRe, src) }

// JSKeys 返回 JS 里 t('…') / tHTML('…') 的键（含中文的）。
func JSKeys(src string) []string { return keysFrom(jsKeyRe, src) }

// ---- Go 文案闸门的范围 ----

// GoScopeSkip 是不面向 Web 界面的 Go 路径片段（命令行工具、测试替身、初始化脚本等），不纳入 Go 文案闸门。
var GoScopeSkip = []string{
	"internal/dbtadmin", "internal/migrate", "internal/bootstrapsql", "internal/i18n",
	"internal/config", "internal/crypto", "servertest", "memrepo", "enginetest",
}

// InGoScope 判断相对仓库根的 Go 文件路径是否纳入闸门（非测试文件、不在跳过名单里）。
func InGoScope(rel string) bool {
	if !strings.HasPrefix(rel, "internal/") || !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
		return false
	}
	for _, s := range GoScopeSkip {
		if strings.Contains(rel, s) {
			return false
		}
	}
	return true
}
