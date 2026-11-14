// @author: kevinkelly
// @license: Apache Licence
// @file: tomlsrc.go
// @time: 2026/10/28

package giconfig

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// 本文件对 config.toml 做“保留注释与排版”的行级编辑：表单改动要同步回源文件时，只改动对应的那一行，
// 其余内容（注释、空行、顺序）原样保留。只理解 `key = value` 与 `[section]` 两种结构，
// 内联表、数组表、点号键等少见写法只会被当作“原样保留的文本”，不会参与表单同步。

// MaskPlaceholder 是密码类参数在页面上的占位值：源文件展示和版本对比里都用它代替真实密码，
// 保存时未改动的占位符会还原成磁盘上的原值。
const MaskPlaceholder = "********"

// entry 是源文件里的一条 key = value。行号从 0 开始。
type entry struct {
	Section string // "" 表示顶层；"inc"、"log.file" 等
	Key     string
	Line    int // 起始行
	EndLine int // 结束行（多行值时大于 Line）
	ValCol  int // 值在起始行里的起始字节位置
	EndCol  int // 值在结束行里的结束字节位置
	Raw     string
	HeadIdx int // 条目所在段在 heads 里的下标，-1 表示顶层
}

type sectionHead struct {
	Name string
	Line int
}

var keyRe = regexp.MustCompile(`^\s*((?:[A-Za-z0-9_\-]+|"[^"]*"|'[^']*')(?:\s*\.\s*(?:[A-Za-z0-9_\-]+|"[^"]*"|'[^']*'))*)\s*=\s*`)

// normalizeNewlines 统一换行为 \n，并保证以换行结尾。
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}

func splitLines(src string) []string {
	src = strings.TrimSuffix(src, "\n")
	if src == "" {
		return []string{}
	}
	return strings.Split(src, "\n")
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// scanString 从 lines[li][col]（引号处）开始扫描一个字符串，返回结束位置（闭合引号之后）。
// 支持基本串、字面量串和各自的三引号多行形式。未闭合时返回文本末尾。
func scanString(lines []string, li, col int) (int, int) {
	s := lines[li]
	q := s[col]
	triple := strings.HasPrefix(s[col:], strings.Repeat(string(q), 3))
	if !triple {
		i := col + 1
		for i < len(s) {
			if q == '"' && s[i] == '\\' {
				i += 2
				continue
			}
			if s[i] == q {
				return li, i + 1
			}
			i++
		}
		return li, len(s)
	}
	i := col + 3
	for l := li; l < len(lines); l++ {
		s = lines[l]
		if l != li {
			i = 0
		}
		for i < len(s) {
			if q == '"' && s[i] == '\\' {
				i += 2
				continue
			}
			if strings.HasPrefix(s[i:], strings.Repeat(string(q), 3)) {
				end := i + 3
				for end < len(s) && s[end] == byte(q) { // 允许 """" 这种以引号结尾的写法
					end++
				}
				return l, end
			}
			i++
		}
	}
	return len(lines) - 1, len(lines[len(lines)-1])
}

// scanNested 扫描 [] 或 {} 包起来的值（可能跨多行）。
func scanNested(lines []string, li, col int) (int, int) {
	depth := 0
	for l, c := li, col; l < len(lines); l, c = l+1, 0 {
		s := lines[l]
		for c < len(s) {
			switch s[c] {
			case '"', '\'':
				el, ec := scanString(lines, l, c)
				if el != l {
					l, s = el, lines[el]
				}
				c = ec
				continue
			case '#':
				c = len(s)
				continue
			case '[', '{':
				depth++
			case ']', '}':
				depth--
				if depth == 0 {
					return l, c + 1
				}
			}
			c++
		}
	}
	return len(lines) - 1, len(lines[len(lines)-1])
}

// scanValue 返回从 (li,col) 开始的值的结束位置（不含行尾注释）。
func scanValue(lines []string, li, col int) (int, int) {
	s := lines[li]
	for col < len(s) && (s[col] == ' ' || s[col] == '\t') {
		col++
	}
	if col >= len(s) {
		return li, col
	}
	switch s[col] {
	case '"', '\'':
		return scanString(lines, li, col)
	case '[', '{':
		return scanNested(lines, li, col)
	}
	end := col
	for end < len(s) && s[end] != '#' {
		end++
	}
	for end > col && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return li, end
}

func sectionName(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "[") || strings.HasPrefix(t, "[[") {
		return "", false
	}
	end := strings.Index(t, "]")
	if end < 0 {
		return "", false
	}
	parts := strings.Split(t[1:end], ".")
	for i, p := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(p), `"'`)
	}
	return strings.Join(parts, "."), true
}

// parseEntries 扫描源文件，返回所有 key = value 条目与段标题。
func parseEntries(lines []string) ([]entry, []sectionHead) {
	var entries []entry
	var heads []sectionHead
	cur := -1
	section := ""
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(t, "[[") { // 数组表：其下的键不参与表单同步
			heads = append(heads, sectionHead{Name: "[[" + strings.Trim(t, "[] ") + "]]", Line: i})
			cur, section = len(heads)-1, heads[len(heads)-1].Name
			continue
		}
		if name, ok := sectionName(lines[i]); ok {
			heads = append(heads, sectionHead{Name: name, Line: i})
			cur, section = len(heads)-1, name
			continue
		}
		m := keyRe.FindStringSubmatchIndex(lines[i])
		if m == nil {
			continue
		}
		key := lines[i][m[2]:m[3]]
		key = strings.NewReplacer(" ", "", "\t", "", `"`, "", `'`, "").Replace(key)
		valCol := m[1]
		el, ec := scanValue(lines, i, valCol)
		raw := lines[i][valCol:]
		if el == i {
			raw = lines[i][valCol:ec]
		} else {
			parts := []string{lines[i][valCol:]}
			parts = append(parts, lines[i+1:el]...)
			parts = append(parts, lines[el][:ec])
			raw = strings.Join(parts, "\n")
		}
		entries = append(entries, entry{Section: section, Key: key, Line: i, EndLine: el, ValCol: valCol, EndCol: ec, Raw: strings.TrimSpace(raw), HeadIdx: cur})
		i = el
	}
	return entries, heads
}

func paramID(section, key string) string {
	if section == "" {
		return key
	}
	return section + "." + key
}

// IsSensitiveKey 判断参数名是不是密码类（页面不回显、版本对比里脱敏）。
func IsSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	if i := strings.LastIndex(k, "."); i >= 0 {
		k = k[i+1:]
	}
	for _, w := range []string{"password", "passwd", "secret", "token"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

func isMaskRaw(raw string) bool {
	return raw == `"`+MaskPlaceholder+`"` || raw == `'`+MaskPlaceholder+`'`
}

// replaceRaw 把条目的值整体替换成 newRaw（多行值会合并成一行），行尾注释保留。
func replaceRaw(lines []string, e entry, newRaw string) []string {
	tail := lines[e.EndLine][e.EndCol:]
	first := lines[e.Line][:e.ValCol] + newRaw + tail
	out := make([]string, 0, len(lines))
	out = append(out, lines[:e.Line]...)
	out = append(out, first)
	out = append(out, lines[e.EndLine+1:]...)
	return out
}

// MaskContent 把密码类参数的非空字符串值换成占位符。
func MaskContent(src string) string {
	lines := splitLines(normalizeNewlines(src))
	entries, _ := parseEntries(lines)
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if IsSensitiveKey(e.Key) && isStringRaw(e.Raw) && !isEmptyStringRaw(e.Raw) {
			lines = replaceRaw(lines, e, `"`+MaskPlaceholder+`"`)
		}
	}
	return joinLines(lines)
}

func isStringRaw(raw string) bool {
	return strings.HasPrefix(raw, `"`) || strings.HasPrefix(raw, `'`)
}

func isEmptyStringRaw(raw string) bool {
	return raw == `""` || raw == `''`
}

// Unmask 把 src 里的密码占位符还原成 baselines（按顺序，先出现的优先）里同名参数的原值。
// 找不到原值时报错，避免把“********”当成真实密码写进配置文件。
func Unmask(src string, baselines ...string) (string, error) {
	lines := splitLines(normalizeNewlines(src))
	entries, _ := parseEntries(lines)
	var base [][]entry
	for _, b := range baselines {
		es, _ := parseEntries(splitLines(normalizeNewlines(b)))
		base = append(base, es)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if !IsSensitiveKey(e.Key) || !isMaskRaw(e.Raw) {
			continue
		}
		found := ""
		for _, es := range base {
			for _, b := range es {
				if b.Section == e.Section && b.Key == e.Key && b.Raw != "" && !isMaskRaw(b.Raw) && !isEmptyStringRaw(b.Raw) {
					found = b.Raw
				}
			}
			if found != "" {
				break
			}
		}
		if found == "" {
			return "", fmt.Errorf("%s 的值是占位符 %s，但没有找到可以沿用的原值，请填写真实密码", paramID(e.Section, e.Key), MaskPlaceholder)
		}
		lines = replaceRaw(lines, e, found)
	}
	return joinLines(lines), nil
}

// tomlQuote 生成 TOML 基本字符串。
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// formatValue 把前端传来的值按参数类型格式化成 TOML 字面量。
func formatValue(section, key string, v any) (string, error) {
	kind := kString
	if p := lookupParam(section, key); p != nil {
		kind = p.Kind
	} else {
		switch v.(type) {
		case bool:
			kind = kBool
		case float64, int, int64:
			kind = kInt
		}
	}
	id := paramID(section, key)
	switch kind {
	case kBool:
		b, ok := v.(bool)
		if !ok {
			return "", fmt.Errorf("%s 需要布尔值（true / false）", id)
		}
		return strconv.FormatBool(b), nil
	case kInt:
		switch n := v.(type) {
		case float64:
			if n != float64(int64(n)) {
				return "", fmt.Errorf("%s 需要整数", id)
			}
			return strconv.FormatInt(int64(n), 10), nil
		case int:
			return strconv.Itoa(n), nil
		case int64:
			return strconv.FormatInt(n, 10), nil
		case string:
			n2, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
			if err != nil {
				return "", fmt.Errorf("%s 需要整数", id)
			}
			return strconv.FormatInt(n2, 10), nil
		}
		return "", fmt.Errorf("%s 需要整数", id)
	case kFloat:
		var f float64
		switch n := v.(type) {
		case float64:
			f = n
		case int:
			f = float64(n)
		case int64:
			f = float64(n)
		case string:
			var err error
			if f, err = strconv.ParseFloat(strings.TrimSpace(n), 64); err != nil {
				return "", fmt.Errorf("%s 需要数字", id)
			}
		default:
			return "", fmt.Errorf("%s 需要数字", id)
		}
		s := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(s, ".") {
			s += ".0"
		}
		return s, nil
	case kList:
		return "", fmt.Errorf("%s 是数组，请在源文件里直接编辑", id)
	default:
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("%s 需要字符串", id)
		}
		return tomlQuote(s), nil
	}
}

// SetKey 在 src 里设置 [section] key 的值：已存在则只替换值，不存在则追加到该段末尾（段不存在则新建）。
func SetKey(src, section, key string, value any) (string, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9_\-]+$`).MatchString(key) {
		return "", fmt.Errorf("参数名 \"%s\" 不合法", key)
	}
	if section != "" && !regexp.MustCompile(`^[A-Za-z0-9_\-]+(\.[A-Za-z0-9_\-]+)*$`).MatchString(section) {
		return "", fmt.Errorf("配置段名 \"%s\" 不合法", section)
	}
	raw, err := formatValue(section, key, value)
	if err != nil {
		return "", err
	}
	lines := splitLines(normalizeNewlines(src))
	entries, heads := parseEntries(lines)
	for _, e := range entries {
		if e.Section == section && e.Key == key {
			return joinLines(replaceRaw(lines, e, raw)), nil
		}
	}
	newLine := key + " = " + raw
	insertAt := -1
	if section == "" {
		if len(heads) > 0 {
			insertAt = heads[0].Line
			// 顶层键放在第一个段标题之前，且尽量贴着最后一个顶层条目
			last := -1
			for _, e := range entries {
				if e.Section == "" && e.EndLine > last {
					last = e.EndLine
				}
			}
			if last >= 0 {
				insertAt = last + 1
			}
			out := append([]string{}, lines[:insertAt]...)
			out = append(out, newLine)
			if last < 0 {
				out = append(out, "")
			}
			out = append(out, lines[insertAt:]...)
			return joinLines(out), nil
		}
		return joinLines(append(lines, newLine)), nil
	}
	for hi, h := range heads {
		if h.Name != section {
			continue
		}
		insertAt = h.Line + 1
		for _, e := range entries {
			if e.HeadIdx == hi && e.EndLine+1 > insertAt {
				insertAt = e.EndLine + 1
			}
		}
		out := append([]string{}, lines[:insertAt]...)
		out = append(out, newLine)
		out = append(out, lines[insertAt:]...)
		return joinLines(out), nil
	}
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
		lines = append(lines, "")
	}
	lines = append(lines, "["+section+"]", newLine)
	return joinLines(lines), nil
}

// lineIndex 返回每个参数在源文件里的起始行号（从 1 开始）。
func lineIndex(src string) map[string]int {
	entries, _ := parseEntries(splitLines(normalizeNewlines(src)))
	m := map[string]int{}
	for _, e := range entries {
		id := paramID(e.Section, e.Key)
		if _, dup := m[id]; !dup {
			m[id] = e.Line + 1
		}
	}
	return m
}
