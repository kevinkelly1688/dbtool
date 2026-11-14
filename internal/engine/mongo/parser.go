// @author: kevinkelly
// @license: Apache Licence
// @file: parser.go
// @time: 2026/10/28

package mongo

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// 小型 mongo shell 语句解析器（词法 + 递归下降），不使用 eval，也不执行任何 JavaScript。
//
// 语句形态：db.<集合>.<方法>(<参数>)[.<方法>(<参数>)]...[;]
// 参数是 JSON 的超集：键可以不加引号、字符串可以用单引号、允许尾逗号与注释、正则字面量 /x/i，
// 以及 shell 常用的构造函数（ObjectId / ISODate / NumberLong ...）。解析结果统一转换成 MongoDB Extended JSON（relaxed），
// 再由 bson.UnmarshalExtJSON 解码，所以用户直接写 {"$oid": "..."} 这类 Extended JSON 也是同一条路径。

const (
	maxStatementBytes = 64 << 10 // 语句文本上限
	maxNesting        = 64       // 对象 / 数组最大嵌套深度
)

// ---- 词法 ----

type tokKind int

const (
	tkEOF tokKind = iota
	tkIdent
	tkString
	tkNumber
	tkRegex
	tkPunct
)

type token struct {
	kind  tokKind
	text  string // 标识符名 / 解码后的字符串 / 数字原文 / 标点 / 正则模式
	flags string // 正则选项
	off   int    // 起始字节偏移（用于报错定位）
}

// pos 的行列从 1 开始，列按字符（rune）计。
func posOf(src string, off int) (line, col int) {
	if off > len(src) {
		off = len(src)
	}
	line, col = 1, 1
	for i, r := range src {
		if i >= off {
			break
		}
		if r == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return
}

func (p *parser) fail(off int, format string, a ...any) error {
	l, c := posOf(p.src, off)
	return uerr("第 %d 行第 %d 列：%s", l, c, sprintf(format, a...))
}

func lexFail(src string, off int, format string, a ...any) error {
	l, c := posOf(src, off)
	return uerr("第 %d 行第 %d 列：%s", l, c, sprintf(format, a...))
}

func isIdentStart(r rune) bool { return r == '_' || r == '$' || unicode.IsLetter(r) }
func isIdentPart(r rune) bool  { return isIdentStart(r) || unicode.IsDigit(r) }

func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(src) {
		r, size := utf8.DecodeRuneInString(src[i:])
		if r == utf8.RuneError && size <= 1 {
			return nil, lexFail(src, i, "语句含有非法的 UTF-8 字节")
		}
		switch {
		case unicode.IsSpace(r) || r == 0xFEFF:
			i += size
		case r == '/':
			if i+1 < len(src) && src[i+1] == '/' { // 行注释
				for i < len(src) && src[i] != '\n' {
					i++
				}
				continue
			}
			if i+1 < len(src) && src[i+1] == '*' { // 块注释
				end := strings.Index(src[i+2:], "*/")
				if end < 0 {
					return nil, lexFail(src, i, "块注释 /* 没有结束")
				}
				i += 2 + end + 2
				continue
			}
			t, n, err := lexRegex(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, t)
			i += n
		case isIdentStart(r):
			j := i + size
			for j < len(src) {
				r2, s2 := utf8.DecodeRuneInString(src[j:])
				if !isIdentPart(r2) {
					break
				}
				j += s2
			}
			toks = append(toks, token{kind: tkIdent, text: src[i:j], off: i})
			i = j
		case r >= '0' && r <= '9' || (r == '.' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9'):
			j := lexNumberEnd(src, i)
			toks = append(toks, token{kind: tkNumber, text: src[i:j], off: i})
			i = j
		case r == '"' || r == '\'':
			s, n, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tkString, text: s, off: i})
			i += n
		case strings.ContainsRune("{}[](),:.;-", r):
			toks = append(toks, token{kind: tkPunct, text: string(r), off: i})
			i += size
		default:
			return nil, lexFail(src, i, "无法识别的字符 %q", r)
		}
	}
	toks = append(toks, token{kind: tkEOF, off: len(src)})
	return toks, nil
}

func lexNumberEnd(src string, i int) int {
	j := i
	if src[j] == '0' && j+1 < len(src) && (src[j+1] == 'x' || src[j+1] == 'X') {
		j += 2
		for j < len(src) && isHex(src[j]) {
			j++
		}
		return j
	}
	for j < len(src) && src[j] >= '0' && src[j] <= '9' {
		j++
	}
	if j < len(src) && src[j] == '.' {
		j++
		for j < len(src) && src[j] >= '0' && src[j] <= '9' {
			j++
		}
	}
	if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
		k := j + 1
		if k < len(src) && (src[k] == '+' || src[k] == '-') {
			k++
		}
		if k < len(src) && src[k] >= '0' && src[k] <= '9' {
			for k < len(src) && src[k] >= '0' && src[k] <= '9' {
				k++
			}
			j = k
		}
	}
	return j
}

func isHex(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

func lexString(src string, i int) (string, int, error) {
	quote := src[i]
	var sb strings.Builder
	j := i + 1
	for j < len(src) {
		c := src[j]
		switch {
		case c == quote:
			return sb.String(), j + 1 - i, nil
		case c == '\n' || c == '\r':
			return "", 0, lexFail(src, i, "字符串没有结束（字符串内不能直接换行，请用 \\n）")
		case c == '\\':
			if j+1 >= len(src) {
				return "", 0, lexFail(src, j, "字符串末尾的转义不完整")
			}
			e := src[j+1]
			j += 2
			switch e {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'v':
				sb.WriteByte('\v')
			case '0':
				sb.WriteByte(0)
			case '\\', '\'', '"', '/':
				sb.WriteByte(e)
			case 'x':
				if j+2 > len(src) || !isHex(src[j]) || !isHex(src[j+1]) {
					return "", 0, lexFail(src, j-2, "\\x 转义需要两位十六进制数字")
				}
				v, _ := strconv.ParseUint(src[j:j+2], 16, 8)
				sb.WriteRune(rune(v))
				j += 2
			case 'u':
				r, n, err := lexUnicodeEscape(src, j)
				if err != nil {
					return "", 0, err
				}
				j += n
				sb.WriteRune(r)
			default:
				return "", 0, lexFail(src, j-2, "不支持的转义 \\%s", string(rune(e)))
			}
		default:
			sb.WriteByte(c)
			j++
		}
	}
	return "", 0, lexFail(src, i, "字符串没有结束")
}

// lexUnicodeEscape 解析 \u 后的 4 位十六进制（j 指向第一位），遇到高代理项时尝试接上后面的 \uXXXX 低代理项。
func lexUnicodeEscape(src string, j int) (rune, int, error) {
	read4 := func(k int) (rune, bool) {
		if k+4 > len(src) {
			return 0, false
		}
		for _, b := range []byte(src[k : k+4]) {
			if !isHex(b) {
				return 0, false
			}
		}
		v, _ := strconv.ParseUint(src[k:k+4], 16, 32)
		return rune(v), true
	}
	r, ok := read4(j)
	if !ok {
		return 0, 0, lexFail(src, j-2, "\\u 转义需要四位十六进制数字")
	}
	if r >= 0xD800 && r < 0xDC00 && j+10 <= len(src) && src[j+4] == '\\' && src[j+5] == 'u' {
		if lo, ok := read4(j + 6); ok && lo >= 0xDC00 && lo < 0xE000 {
			return (r-0xD800)<<10 + (lo - 0xDC00) + 0x10000, 10, nil
		}
	}
	if r >= 0xD800 && r < 0xE000 {
		return utf8.RuneError, 4, nil
	}
	return r, 4, nil
}

func lexRegex(src string, i int) (token, int, error) {
	j := i + 1
	inClass := false
	for j < len(src) {
		c := src[j]
		switch {
		case c == '\n' || c == '\r':
			return token{}, 0, lexFail(src, i, "正则表达式没有结束")
		case c == '\\':
			j += 2
			continue
		case c == '[':
			inClass = true
		case c == ']':
			inClass = false
		case c == '/' && !inClass:
			pat := src[i+1 : j]
			k := j + 1
			for k < len(src) && src[k] >= 'a' && src[k] <= 'z' {
				k++
			}
			if pat == "" {
				return token{}, 0, lexFail(src, i, "正则表达式不能为空")
			}
			return token{kind: tkRegex, text: pat, flags: src[j+1 : k], off: i}, k - i, nil
		}
		j++
	}
	return token{}, 0, lexFail(src, i, "正则表达式没有结束")
}

// ---- 值节点 ----

type nodeKind int

const (
	nObject nodeKind = iota
	nArray
	nString
	nNumber
	nBool
	nNull
	nOther // 构造函数 / 正则等已经转成 Extended JSON 的标量
)

// node 是一个已转换成 Extended JSON 文本的值。
type node struct {
	js     string
	kind   nodeKind
	str    string  // nString
	hasNum bool    // 数值（数字字面量或 NumberInt / NumberLong 等构造函数）
	isInt  bool    // 整数
	i64    int64   // isInt 时的值
	f64    float64 // 数值
	off    int
}

type call struct {
	name string
	args []node
	off  int
}

// stmt 是解析后的 db.<coll>.<m1>().<m2>()... 语句。
type stmt struct {
	coll    string // 集合名；db 级方法（getCollectionNames / stats）为空
	hasColl bool
	calls   []call // 第一个是 db 或集合上的方法，其后是游标链
}

type parser struct {
	src   string
	toks  []token
	i     int
	depth int
}

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tkEOF {
		p.i++
	}
	return t
}
func (p *parser) isPunct(s string) bool { t := p.peek(); return t.kind == tkPunct && t.text == s }
func (p *parser) accept(s string) bool {
	if p.isPunct(s) {
		p.i++
		return true
	}
	return false
}

func (p *parser) describe(t token) string {
	switch t.kind {
	case tkEOF:
		return "语句结束"
	case tkString:
		return "字符串"
	case tkNumber:
		return fmt.Sprintf("数字 %s", t.text)
	case tkRegex:
		return "正则表达式"
	}
	return "“" + t.text + "”"
}

func (p *parser) expect(s string) error {
	t := p.peek()
	if t.kind == tkPunct && t.text == s {
		p.i++
		return nil
	}
	return p.fail(t.off, "此处应为“%s”，实际是%s", s, p.describe(t))
}

// parseStatement 解析一条 shell 语句。
func parseStatement(src string) (*stmt, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{src: src, toks: toks}
	st, err := p.statement()
	if err != nil {
		return nil, err
	}
	for p.accept(";") {
	}
	if t := p.peek(); t.kind != tkEOF {
		if t.kind == tkIdent && t.text == "db" {
			return nil, p.fail(t.off, "检测到多条语句，在线查询一次只能执行一条；请只保留一条（或选中其中一条再运行）")
		}
		return nil, p.fail(t.off, "语句在这里应当结束，多余内容：%s（一次只能执行一条语句）", p.describe(t))
	}
	return st, nil
}

func (p *parser) statement() (*stmt, error) {
	t := p.peek()
	if t.kind != tkIdent || t.text != "db" {
		hint := ""
		switch {
		case t.kind == tkEOF:
			return nil, uerr("语句为空")
		case t.kind == tkIdent && t.text == "use":
			hint = "；切换库请用页面顶部的库下拉"
		case t.kind == tkIdent && t.text == "show":
			hint = "；查看集合请执行 db.getCollectionNames()，库列表在左侧库表树里"
		case t.kind == tkIdent && (t.text == "rs" || t.text == "sh"):
			hint = "；副本集 / 分片管理命令不支持"
		}
		return nil, p.fail(t.off, "语句必须以 db. 开头，例如 db.users.find({})%s", hint)
	}
	p.i++
	st := &stmt{}
	var parts []string
	for {
		switch {
		case p.isPunct("["):
			if st.hasColl || len(parts) > 0 {
				return nil, p.fail(p.peek().off, "不支持这种集合写法，请用 db.<集合>.<方法>() 或 db.getCollection(\"名称\")")
			}
			p.i++
			s := p.next()
			if s.kind != tkString {
				return nil, p.fail(s.off, "db[...] 里应为带引号的集合名")
			}
			if err := p.expect("]"); err != nil {
				return nil, err
			}
			st.coll, st.hasColl = s.text, true
			if !p.isPunct(".") {
				return nil, p.fail(p.peek().off, "集合后面需要跟方法，例如 .find({})")
			}
			continue
		case p.isPunct("."):
			p.i++
		default:
			t := p.peek()
			return nil, p.fail(t.off, "此处应为“.”，实际是%s", p.describe(t))
		}
		nt := p.next()
		if nt.kind != tkIdent {
			return nil, p.fail(nt.off, "“.”后面应为集合名或方法名，实际是%s", p.describe(nt))
		}
		if !p.isPunct("(") {
			// 集合名的一部分（含点号的集合名 a.b）
			if st.hasColl {
				st.coll += "." + nt.text
			} else {
				parts = append(parts, nt.text)
			}
			continue
		}
		args, err := p.args()
		if err != nil {
			return nil, err
		}
		c := call{name: nt.text, args: args, off: nt.off}
		if err := methodDenied(c); err != nil {
			return nil, err
		}
		if len(parts) == 0 && !st.hasColl {
			if c.name == "getCollection" {
				if len(args) != 1 || args[0].kind != nString {
					return nil, p.fail(nt.off, "getCollection 需要一个字符串参数（集合名）")
				}
				st.coll, st.hasColl = args[0].str, true
				if !p.isPunct(".") {
					return nil, p.fail(p.peek().off, "集合后面需要跟方法，例如 .find({})")
				}
				continue
			}
			st.calls = append(st.calls, c) // db 级方法
		} else {
			if !st.hasColl {
				st.coll, st.hasColl = strings.Join(parts, "."), true
			} else if len(parts) > 0 {
				st.coll += "." + strings.Join(parts, ".")
			}
			st.calls = append(st.calls, c)
		}
		break
	}
	for p.isPunct(".") { // 游标链
		p.i++
		nt := p.next()
		if nt.kind != tkIdent {
			return nil, p.fail(nt.off, "“.”后面应为方法名，实际是%s", p.describe(nt))
		}
		if !p.isPunct("(") {
			return nil, p.fail(p.peek().off, "%s 后面缺少括号，应为 .%s(...)", nt.text, nt.text)
		}
		args, err := p.args()
		if err != nil {
			return nil, err
		}
		st.calls = append(st.calls, call{name: nt.text, args: args, off: nt.off})
	}
	return st, nil
}

func (p *parser) args() ([]node, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}
	var out []node
	for !p.isPunct(")") {
		n, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, n)
		if p.accept(",") {
			continue
		}
		if !p.isPunct(")") {
			t := p.peek()
			return nil, p.fail(t.off, "参数之间应为“,”，实际是%s", p.describe(t))
		}
	}
	p.i++
	return out, nil
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (p *parser) value() (node, error) {
	t := p.peek()
	switch t.kind {
	case tkPunct:
		switch t.text {
		case "{":
			return p.object()
		case "[":
			return p.array()
		case "-":
			p.i++
			nt := p.peek()
			switch {
			case nt.kind == tkNumber:
				p.i++
				n, err := p.number(nt, true)
				return n, err
			case nt.kind == tkIdent && nt.text == "Infinity":
				p.i++
				return node{js: `{"$numberDouble":"-Infinity"}`, kind: nNumber, hasNum: true, f64: math.Inf(-1), off: t.off}, nil
			}
			return node{}, p.fail(nt.off, "“-”后面应为数字")
		}
	case tkString:
		p.i++
		return node{js: jsonString(t.text), kind: nString, str: t.text, off: t.off}, nil
	case tkNumber:
		p.i++
		return p.number(t, false)
	case tkRegex:
		p.i++
		return regexNode(p, t.text, t.flags, t.off)
	case tkIdent:
		return p.identValue()
	}
	return node{}, p.fail(t.off, "此处应为一个值，实际是%s", p.describe(t))
}

func (p *parser) number(t token, neg bool) (node, error) {
	txt := t.text
	if strings.HasPrefix(txt, "0x") || strings.HasPrefix(txt, "0X") {
		v, err := strconv.ParseInt(txt[2:], 16, 64)
		if err != nil {
			return node{}, p.fail(t.off, "十六进制数 %s 不合法或超出范围", txt)
		}
		if neg {
			v = -v
		}
		return intNode(v, t.off), nil
	}
	if !strings.ContainsAny(txt, ".eE") {
		if v, err := strconv.ParseInt(txt, 10, 64); err == nil {
			if neg {
				v = -v
			}
			return intNode(v, t.off), nil
		}
	}
	f, err := strconv.ParseFloat(txt, 64)
	if err != nil || math.IsInf(f, 0) {
		return node{}, p.fail(t.off, "数字 %s 不合法或超出范围", txt)
	}
	if neg {
		f = -f
	}
	return doubleNode(f, t.off), nil
}

func intNode(v int64, off int) node {
	return node{js: strconv.FormatInt(v, 10), kind: nNumber, hasNum: true, isInt: true, i64: v, f64: float64(v), off: off}
}

func doubleNode(f float64, off int) node {
	var js string
	switch {
	case math.IsNaN(f):
		js = `{"$numberDouble":"NaN"}`
	case math.IsInf(f, 1):
		js = `{"$numberDouble":"Infinity"}`
	case math.IsInf(f, -1):
		js = `{"$numberDouble":"-Infinity"}`
	default:
		js = `{"$numberDouble":"` + strconv.FormatFloat(f, 'g', -1, 64) + `"}`
	}
	return node{js: js, kind: nNumber, hasNum: true, f64: f, off: off}
}

func (p *parser) enter(off int) error {
	p.depth++
	if p.depth > maxNesting {
		return p.fail(off, "嵌套层数超过 %d 层", maxNesting)
	}
	return nil
}

func (p *parser) object() (node, error) {
	open := p.next()
	if err := p.enter(open.off); err != nil {
		return node{}, err
	}
	defer func() { p.depth-- }()
	var sb strings.Builder
	sb.WriteByte('{')
	first := true
	for !p.isPunct("}") {
		kt := p.next()
		var key string
		switch kt.kind {
		case tkIdent, tkString:
			key = kt.text
		case tkNumber:
			key = kt.text
		default:
			return node{}, p.fail(kt.off, "文档的键应为标识符或字符串，实际是%s", p.describe(kt))
		}
		if err := p.expect(":"); err != nil {
			return node{}, err
		}
		v, err := p.value()
		if err != nil {
			return node{}, err
		}
		if !first {
			sb.WriteByte(',')
		}
		first = false
		sb.WriteString(jsonString(key))
		sb.WriteByte(':')
		sb.WriteString(v.js)
		if p.accept(",") {
			continue
		}
		if !p.isPunct("}") {
			t := p.peek()
			return node{}, p.fail(t.off, "文档的键值对之间应为“,”或结束的“}”，实际是%s", p.describe(t))
		}
	}
	p.i++
	sb.WriteByte('}')
	return node{js: sb.String(), kind: nObject, off: open.off}, nil
}

func (p *parser) array() (node, error) {
	open := p.next()
	if err := p.enter(open.off); err != nil {
		return node{}, err
	}
	defer func() { p.depth-- }()
	var sb strings.Builder
	sb.WriteByte('[')
	first := true
	for !p.isPunct("]") {
		v, err := p.value()
		if err != nil {
			return node{}, err
		}
		if !first {
			sb.WriteByte(',')
		}
		first = false
		sb.WriteString(v.js)
		if p.accept(",") {
			continue
		}
		if !p.isPunct("]") {
			t := p.peek()
			return node{}, p.fail(t.off, "数组元素之间应为“,”或结束的“]”，实际是%s", p.describe(t))
		}
	}
	p.i++
	sb.WriteByte(']')
	return node{js: sb.String(), kind: nArray, off: open.off}, nil
}

func (p *parser) identValue() (node, error) {
	t := p.next()
	switch t.text {
	case "true":
		return node{js: "true", kind: nBool, off: t.off}, nil
	case "false":
		return node{js: "false", kind: nBool, off: t.off}, nil
	case "null", "undefined":
		return node{js: "null", kind: nNull, off: t.off}, nil
	case "NaN":
		return doubleNode(math.NaN(), t.off), nil
	case "Infinity":
		return doubleNode(math.Inf(1), t.off), nil
	case "new":
		nt := p.next()
		if nt.kind != tkIdent || !p.isPunct("(") {
			return node{}, p.fail(nt.off, "new 后面应为构造函数调用，例如 new Date(...)")
		}
		return p.constructor(nt)
	}
	if t.text == "function" || t.text == "this" {
		return node{}, p.fail(t.off, "不支持 JavaScript 函数 / 表达式（%s）", t.text)
	}
	if p.isPunct("(") {
		return p.constructor(t)
	}
	return node{}, p.fail(t.off, "无法识别的标识符 %s（字符串请加引号）", t.text)
}

func (p *parser) constructor(name token) (node, error) {
	if err := p.enter(name.off); err != nil {
		return node{}, err
	}
	defer func() { p.depth-- }()
	args, err := p.args()
	if err != nil {
		return node{}, err
	}
	n, err := buildConstructor(p, name, args)
	if err != nil {
		return node{}, err
	}
	n.off = name.off
	return n, nil
}

func buildConstructor(p *parser, name token, args []node) (node, error) {
	bad := func(format string, a ...any) (node, error) {
		return node{}, p.fail(name.off, name.text+"："+format, a...)
	}
	argc := func(min, max int) error {
		if len(args) < min || len(args) > max {
			return p.fail(name.off, "%s 的参数个数不对", name.text)
		}
		return nil
	}
	switch name.text {
	case "ObjectId", "ObjectID":
		if err := argc(1, 1); err != nil {
			return node{}, err
		}
		if args[0].kind != nString || len(args[0].str) != 24 || !allHex(args[0].str) {
			return bad("需要一个 24 位十六进制字符串")
		}
		return node{js: `{"$oid":"` + strings.ToLower(args[0].str) + `"}`, kind: nOther}, nil
	case "ISODate", "Date":
		if err := argc(0, 1); err != nil {
			return node{}, err
		}
		var t time.Time
		switch {
		case len(args) == 0:
			t = time.Now().UTC()
		case args[0].kind == nString:
			var ok bool
			if t, ok = parseDate(args[0].str); !ok {
				return bad("无法识别的日期 %q，应为 ISO-8601，例如 \"2024-01-02T03:04:05Z\"", args[0].str)
			}
		case args[0].hasNum && args[0].isInt:
			t = time.UnixMilli(args[0].i64).UTC()
		default:
			return bad("参数应为 ISO-8601 日期字符串或毫秒时间戳")
		}
		return node{js: `{"$date":{"$numberLong":"` + strconv.FormatInt(t.UnixMilli(), 10) + `"}}`, kind: nOther}, nil
	case "NumberLong", "Long":
		if err := argc(1, 1); err != nil {
			return node{}, err
		}
		var v int64
		switch {
		case args[0].kind == nString:
			x, err := strconv.ParseInt(strings.TrimSpace(args[0].str), 10, 64)
			if err != nil {
				return bad("%q 不是合法的 64 位整数", args[0].str)
			}
			v = x
		case args[0].hasNum && args[0].isInt:
			v = args[0].i64
		default:
			return bad("参数应为整数或整数字符串")
		}
		n := intNode(v, 0)
		n.js, n.kind = `{"$numberLong":"`+strconv.FormatInt(v, 10)+`"}`, nOther
		return n, nil
	case "NumberInt", "Int32":
		if err := argc(1, 1); err != nil {
			return node{}, err
		}
		var v int64
		switch {
		case args[0].kind == nString:
			x, err := strconv.ParseInt(strings.TrimSpace(args[0].str), 10, 32)
			if err != nil {
				return bad("%q 不是合法的 32 位整数", args[0].str)
			}
			v = x
		case args[0].hasNum && args[0].isInt && args[0].i64 >= math.MinInt32 && args[0].i64 <= math.MaxInt32:
			v = args[0].i64
		default:
			return bad("参数应为 32 位整数")
		}
		n := intNode(v, 0)
		n.js, n.kind = `{"$numberInt":"`+strconv.FormatInt(v, 10)+`"}`, nOther
		return n, nil
	case "NumberDouble", "Double":
		if err := argc(1, 1); err != nil {
			return node{}, err
		}
		switch {
		case args[0].hasNum:
			return doubleNode(args[0].f64, 0), nil
		case args[0].kind == nString:
			f, err := strconv.ParseFloat(strings.TrimSpace(args[0].str), 64)
			if err != nil {
				return bad("%q 不是合法的浮点数", args[0].str)
			}
			return doubleNode(f, 0), nil
		}
		return bad("参数应为数字")
	case "NumberDecimal", "Decimal128":
		if err := argc(1, 1); err != nil {
			return node{}, err
		}
		var s string
		switch {
		case args[0].kind == nString:
			s = strings.TrimSpace(args[0].str)
		case args[0].hasNum:
			if args[0].isInt {
				s = strconv.FormatInt(args[0].i64, 10)
			} else {
				s = strconv.FormatFloat(args[0].f64, 'g', -1, 64)
			}
		default:
			return bad("参数应为十进制数字符串")
		}
		return node{js: `{"$numberDecimal":` + jsonString(s) + `}`, kind: nOther}, nil
	case "Timestamp":
		if err := argc(2, 2); err != nil {
			return node{}, err
		}
		if !args[0].hasNum || !args[0].isInt || !args[1].hasNum || !args[1].isInt ||
			args[0].i64 < 0 || args[0].i64 > math.MaxUint32 || args[1].i64 < 0 || args[1].i64 > math.MaxUint32 {
			return bad("需要两个 0 到 4294967295 的整数（秒, 序号）")
		}
		return node{js: `{"$timestamp":{"t":` + strconv.FormatInt(args[0].i64, 10) + `,"i":` + strconv.FormatInt(args[1].i64, 10) + `}}`, kind: nOther}, nil
	case "BinData":
		if err := argc(2, 2); err != nil {
			return node{}, err
		}
		if !args[0].hasNum || !args[0].isInt || args[0].i64 < 0 || args[0].i64 > 255 || args[1].kind != nString {
			return bad("需要（子类型 0-255, base64 字符串）")
		}
		if _, err := base64.StdEncoding.DecodeString(args[1].str); err != nil {
			return bad("第二个参数不是合法的 base64")
		}
		return node{js: `{"$binary":{"base64":` + jsonString(args[1].str) + `,"subType":"` + hex.EncodeToString([]byte{byte(args[0].i64)}) + `"}}`, kind: nOther}, nil
	case "HexData":
		if err := argc(2, 2); err != nil {
			return node{}, err
		}
		if !args[0].hasNum || !args[0].isInt || args[0].i64 < 0 || args[0].i64 > 255 || args[1].kind != nString {
			return bad("需要（子类型 0-255, 十六进制字符串）")
		}
		raw, err := hex.DecodeString(args[1].str)
		if err != nil {
			return bad("第二个参数不是合法的十六进制")
		}
		return node{js: `{"$binary":{"base64":"` + base64.StdEncoding.EncodeToString(raw) + `","subType":"` + hex.EncodeToString([]byte{byte(args[0].i64)}) + `"}}`, kind: nOther}, nil
	case "UUID":
		if err := argc(1, 1); err != nil {
			return node{}, err
		}
		h := ""
		if args[0].kind == nString {
			h = strings.ReplaceAll(args[0].str, "-", "")
		}
		raw, err := hex.DecodeString(h)
		if err != nil || len(raw) != 16 {
			return bad("需要 UUID 字符串，例如 UUID(\"123e4567-e89b-12d3-a456-426614174000\")")
		}
		return node{js: `{"$binary":{"base64":"` + base64.StdEncoding.EncodeToString(raw) + `","subType":"04"}}`, kind: nOther}, nil
	case "RegExp":
		if err := argc(1, 2); err != nil {
			return node{}, err
		}
		flags := ""
		if len(args) == 2 {
			if args[1].kind != nString {
				return bad("选项应为字符串")
			}
			flags = args[1].str
		}
		if args[0].kind != nString {
			return bad("模式应为字符串")
		}
		return regexNode(p, args[0].str, flags, name.off)
	case "MinKey", "MaxKey":
		if err := argc(0, 0); err != nil {
			return node{}, err
		}
		return node{js: `{"$` + strings.ToLower(name.text[:1]) + name.text[1:] + `":1}`, kind: nOther}, nil
	}
	return node{}, p.fail(name.off, "不支持的函数 / 构造函数 %s（只支持 ObjectId、ISODate、Date、NumberLong、NumberInt、NumberDouble、NumberDecimal、Timestamp、BinData、HexData、UUID、RegExp、MinKey、MaxKey）", name.text)
}

func allHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isHex(s[i]) {
			return false
		}
	}
	return true
}

func regexNode(p *parser, pattern, flags string, off int) (node, error) {
	var keep []byte
	for i := 0; i < len(flags); i++ {
		switch flags[i] {
		case 'g': // JS 的全局标志在 MongoDB 里没有意义，忽略
		case 'i', 'm', 'x', 's', 'u', 'l':
			keep = append(keep, flags[i])
		default:
			return node{}, p.fail(off, "不支持的正则选项 %q（只支持 i m x s u l）", flags[i])
		}
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i] < keep[j] })
	return node{js: `{"$regularExpression":{"pattern":` + jsonString(pattern) + `,"options":"` + string(keep) + `"}}`, kind: nOther, off: off}, nil
}

var dateLayouts = []string{
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05.999999999-0700",
	"2006-01-02T15:04:05-0700",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04Z07:00",
	"2006-01-02T15:04",
	"2006-01-02",
}

// parseDate 无时区的日期按 UTC 处理（与 mongo shell 的 ISODate 一致）。
func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, l := range dateLayouts {
		if t, err := time.ParseInLocation(l, s, time.UTC); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
