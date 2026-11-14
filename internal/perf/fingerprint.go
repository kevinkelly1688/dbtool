// @author: kevinkelly
// @license: Apache Licence
// @file: fingerprint.go
// @time: 2026/10/28

package perf

import (
	"crypto/md5"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SQL 指纹：去注释、参数化、合并 IN 列表、统一大小写与空白。
// 指纹的 MD5 是工单的去重键（实例 + 库 + 指纹）；
// Go 的正则没有后行断言，数字的“前面不能是字母/数字/点/反引号”改为手工判断。
var (
	reCommentBlock = regexp.MustCompile(`(?s)/\*.*?\*/`)
	reCommentLine  = regexp.MustCompile(`(--\s|#)[^\n]*`)
	reString       = regexp.MustCompile(`(?s)'(?:[^'\\]|\\.|'')*'|"(?:[^"\\]|\\.|"")*"`)
	reHex          = regexp.MustCompile(`(?i)0x[0-9a-f]+`) // 前后的 \b 由 replaceBounded 按 Unicode 判断
	reInList       = regexp.MustCompile(`(?i)in\s*\(\s*\?(?:\s*,\s*\?)*\s*\)`)
	reValues       = regexp.MustCompile(`(?is)values\s*\(.*`)
	reSpace        = regexp.MustCompile(`\s+`)
	reOpSpace      = regexp.MustCompile(`\s*(<=>|>=|<=|!=|<>|=|<|>|,)\s*`)
	reParenSpace   = regexp.MustCompile(`\(\s+|\s+\)`)
	reLimit        = regexp.MustCompile(`(?i)limit \?\s*,\s*\?`)
	reTables       = regexp.MustCompile("(?i)\\b(?:from|join|update|into)\\s+((?:`?[\\w$]+`?\\.)?`?[\\w$]+`?)")
)

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// replaceBounded 按 Unicode 单词边界替换：匹配前面必须不是单词字符（含汉字、全角字母数字），
// trailing 为真时后面也必须不是单词字符。Go 的 \b 只认 ASCII，汉字紧挨关键字/数字时判断不准。
func replaceBounded(s string, re *regexp.Regexp, repl func(string) string, trailing bool) string {
	var b strings.Builder
	pos, search := 0, 0
	for search <= len(s) {
		loc := re.FindStringIndex(s[search:])
		if loc == nil {
			break
		}
		start, end := search+loc[0], search+loc[1]
		okStart := true
		if start > 0 {
			r, _ := utf8.DecodeLastRuneInString(s[:start])
			okStart = !isWordRune(r)
		}
		if !okStart || (trailing && !unicodeBoundary(s, end)) {
			_, sz := utf8.DecodeRuneInString(s[start:])
			search = start + sz
			continue
		}
		b.WriteString(s[pos:start])
		b.WriteString(repl(s[start:end]))
		pos, search = end, end
		if end == start {
			search++
		}
	}
	b.WriteString(s[pos:])
	return b.String()
}

// digitsEnd 返回 s 里从 from 起连续的数字（Unicode 十进制数字）的结束位置。
func digitsEnd(s string, from int) int {
	q := from
	for q < len(s) {
		r, sz := utf8.DecodeRuneInString(s[q:])
		if !unicode.IsDigit(r) {
			break
		}
		q += sz
	}
	return q
}

// afterEach 返回 [from, to) 这段数字里“取 1..n 个数字”之后的位置，从长到短（正则回溯的顺序）。
func afterEach(s string, from, to int) []int {
	var pos []int
	for q := from; q < to; {
		_, sz := utf8.DecodeRuneInString(s[q:])
		q += sz
		pos = append(pos, q)
	}
	for i, j := 0, len(pos)-1; i < j; i, j = i+1, j-1 {
		pos[i], pos[j] = pos[j], pos[i]
	}
	return pos
}

// unicodeBoundary 是 Unicode 模式下 \b 的判断：e 之前的字符是单词字符（数字或 e），所以只要后一个字符不是单词字符（含 Unicode 字母/汉字）就是边界。
func unicodeBoundary(s string, e int) bool {
	if e >= len(s) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s[e:])
	return !isWordRune(r)
}

// matchNumber 在 s[i:] 开头按 [-+]?\d+(?:\.\d+)?(?:e[-+]?\d+)?\b（Unicode、re.I）匹配数字字面量，返回结束位置。
// 不能用 Go 的正则：RE2 的 \b 只认 ASCII，数字紧挨汉字（如 id=1数字）时判断不准，会得到不同的指纹。
func matchNumber(s string, i int) (int, bool) {
	p := i
	if p < len(s) && (s[p] == '-' || s[p] == '+') {
		p++
	}
	d1 := digitsEnd(s, p)
	if d1 == p {
		return 0, false
	}
	for _, e := range afterEach(s, p, d1) {
		fracs := []int{e}
		if e < len(s) && s[e] == '.' {
			if f := digitsEnd(s, e+1); f > e+1 {
				fracs = append(afterEach(s, e+1, f), e)
			}
		}
		for _, e2 := range fracs {
			exps := []int{e2}
			if e2 < len(s) && (s[e2] == 'e' || s[e2] == 'E') {
				q := e2 + 1
				if q < len(s) && (s[q] == '-' || s[q] == '+') {
					q++
				}
				if f := digitsEnd(s, q); f > q {
					exps = append(afterEach(s, q, f), e2)
				}
			}
			for _, e3 := range exps {
				if unicodeBoundary(s, e3) {
					return e3, true
				}
			}
		}
	}
	return 0, false
}

// replaceNumbers 把数字字面量换成 ?：数字前面是字母、数字、下划线、点或反引号时不算（如 t1、col.2、`a1`）。
// 逐位置扫描，等价于正则 (?<![\w.`])[-+]?\d+(?:\.\d+)?(?:e[-+]?\d+)?\b（包括 Unicode 单词边界）。
func replaceNumbers(s string) string {
	var b strings.Builder
	var prev rune
	for i := 0; i < len(s); {
		r, sz := utf8.DecodeRuneInString(s[i:])
		if i == 0 || !(isWordRune(prev) || prev == '.' || prev == '`') {
			if r == '-' || r == '+' || unicode.IsDigit(r) {
				if end, ok := matchNumber(s, i); ok {
					b.WriteByte('?')
					prev, _ = utf8.DecodeLastRuneInString(s[:end])
					i = end
					continue
				}
			}
		}
		b.WriteString(s[i : i+sz])
		prev = r
		i += sz
	}
	return b.String()
}

// Fingerprint 返回 SQL 的指纹。
func Fingerprint(sql string) string {
	if sql == "" {
		return ""
	}
	s := strings.TrimRight(strings.TrimSpace(sql), ";")
	s = reCommentBlock.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "/*+") { // 保留 optimizer hint
			return m
		}
		return " "
	})
	s = reString.ReplaceAllString(s, "?") // 先替换字符串，避免字符串里的 # / -- 被当成注释
	s = reCommentLine.ReplaceAllString(s, " ")
	s = replaceBounded(s, reHex, func(string) string { return "?" }, true)
	s = replaceNumbers(s)
	s = strings.ToLower(strings.TrimSpace(reSpace.ReplaceAllString(s, " ")))
	s = reOpSpace.ReplaceAllString(s, "$1")
	s = reParenSpace.ReplaceAllStringFunc(s, strings.TrimSpace)
	s = replaceBounded(s, reInList, func(string) string { return "in(?+)" }, false)
	if strings.HasPrefix(s, "insert") || strings.HasPrefix(s, "replace") {
		s = replaceBounded(s, reValues, func(string) string { return "values(?+)" }, false)
	}
	s = replaceBounded(s, reLimit, func(string) string { return "limit ?" }, false)
	return s
}

// Checksum 是指纹的 MD5（小写十六进制），库里的 checksum 列。
func Checksum(fp string) string {
	sum := md5.Sum([]byte(fp))
	return hex.EncodeToString(sum[:])
}

// ExtractTables 粗略提取表名（小写、去反引号），用于展示和路由，不追求 100% 准确。
func ExtractTables(sql string, limit int) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range reTables.FindAllStringSubmatch(sql, -1) {
		name := strings.ToLower(strings.ReplaceAll(m[1], "`", ""))
		if name == "select" || name == "dual" || strings.HasPrefix(name, "(") || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if len(out) >= limit {
			break
		}
	}
	return out
}
