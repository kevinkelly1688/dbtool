// @author: kevinkelly
// @license: Apache Licence
// @file: pattern.go
// @time: 2026/10/28

package i18n

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 带格式动词的文案（fmt.Errorf("实例 %s 不存在", name) 之类）在运行时已经被代入成了具体文字，没法按原文精确查表。
// 目录里把格式串本身当键（"实例 %s 不存在": "Instance %s does not exist"），TP 把运行时的字符串反过来匹配
// 这些格式串、取出参数后按译文重新格式化。这样 Go 源码不用为了翻译改调用点，只要在 HTTP 边缘调 TP / Localize。

type pattern struct {
	re     *regexp.Regexp
	verbs  []byte // 每个捕获组对应的格式动词
	out    string // 译文格式串
	prefix string // 原文开头到第一个动词之前的字面量，用来快速排除
	lits   int    // 原文里字面量的字符数，越大越具体，优先匹配
}

var fmtVerbRe = regexp.MustCompile(`%(?:%|(?:\[\d+\])?[-+# 0]*\d*(?:\.\d+)?([sdvqfgxtTw]))`)

// compilePattern 把含格式动词（或 %%）的原文编译成匹配器；一个都没有返回 false。
func compilePattern(key, out string) (pattern, bool) {
	locs := fmtVerbRe.FindAllStringSubmatchIndex(key, -1)
	var b strings.Builder
	var verbs []byte
	b.WriteString("^")
	last, lits := 0, 0
	prefix, prefixDone := "", false
	for _, m := range locs {
		lit := key[last:m[0]]
		b.WriteString(regexp.QuoteMeta(lit))
		lits += len([]rune(lit))
		if !prefixDone {
			prefix += lit
		}
		last = m[1]
		if m[2] < 0 { // %%
			b.WriteString("%")
			lits++
			if !prefixDone {
				prefix += "%"
			}
			continue
		}
		prefixDone = true
		v := key[m[2]]
		if v == 'w' { // fmt.Errorf 的 %w 运行时就是被包装错误的文字，按 %s 处理
			v = 's'
		}
		verbs = append(verbs, v)
		switch v {
		case 'd':
			b.WriteString(`(-?\d+)`)
		case 'f', 'g':
			b.WriteString(`(-?\d+(?:\.\d+)?)`)
		case 'q':
			b.WriteString(`("(?:[^"\\]|\\.)*")`)
		default:
			if m[1] == len(key) { // 末尾的动词可以带换行（多行错误详情），其余的不跨行，避免整段文字被错配
				b.WriteString(`((?s:.+))`)
			} else {
				b.WriteString(`([^\n]+?)`)
			}
		}
	}
	tail := key[last:]
	b.WriteString(regexp.QuoteMeta(tail))
	lits += len([]rune(tail))
	if !prefixDone {
		prefix += tail
	}
	b.WriteString("$")
	if len(locs) == 0 { // 只有 %% 的键也要登记：运行时字符串里是单个 %
		return pattern{}, false
	}
	re, err := regexp.Compile(b.String())
	if err != nil {
		return pattern{}, false
	}
	// 译文里的 %w 同样当 %s 用
	out = fmtVerbRe.ReplaceAllStringFunc(out, func(m string) string {
		if strings.HasSuffix(m, "w") {
			return m[:len(m)-1] + "s"
		}
		return m
	})
	return pattern{re: re, verbs: verbs, out: out, prefix: prefix, lits: lits}, true
}

func buildPatterns(m map[string]string) []pattern {
	var ps []pattern
	for k, v := range m {
		if !hasHan(k) || !strings.Contains(k, "%") {
			continue
		}
		if p, ok := compilePattern(k, v); ok {
			ps = append(ps, p)
		}
	}
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].lits != ps[j].lits {
			return ps[i].lits > ps[j].lits
		}
		return ps[i].re.String() < ps[j].re.String()
	})
	return ps
}

// TP 翻译运行时生成的文案：先按原文精确查目录，查不到再按带格式动词的键匹配并重新格式化（参数里的中文递归翻译）。
// lang 为中文、不认识或没有匹配时原样返回。
func TP(lang, s string) string { return tp(lang, s, 0) }

func tp(lang, s string, depth int) string {
	if lang == ZH || s == "" || depth > 2 {
		return s
	}
	c := cat.Load()
	m := c.msgs[lang]
	if m == nil {
		return s
	}
	if v, ok := m[s]; ok {
		return v
	}
	if !hasHan(s) {
		return s
	}
	for _, p := range c.pats[lang] {
		if p.prefix != "" && !strings.HasPrefix(s, p.prefix) {
			continue
		}
		sm := p.re.FindStringSubmatch(s)
		if sm == nil {
			continue
		}
		args := make([]any, 0, len(p.verbs))
		ok := true
		for i, v := range p.verbs {
			raw := sm[i+1]
			switch v {
			case 'd':
				n, err := strconv.ParseInt(raw, 10, 64)
				if err != nil {
					ok = false
				}
				args = append(args, n)
			case 'f', 'g':
				f, err := strconv.ParseFloat(raw, 64)
				if err != nil {
					ok = false
				}
				args = append(args, f)
			case 'q':
				u, err := strconv.Unquote(raw)
				if err != nil {
					ok = false
				}
				args = append(args, tp(lang, u, depth+1))
			default:
				args = append(args, tp(lang, raw, depth+1))
			}
		}
		if ok {
			return fmt.Sprintf(p.out, args...)
		}
	}
	return s
}
