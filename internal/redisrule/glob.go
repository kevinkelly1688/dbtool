// @author: kevinkelly
// @license: Apache Licence
// @file: glob.go
// @time: 2026/10/28

package redisrule

import "unicode/utf8"

// Redis 风格的 glob（同 stringmatchlen）：
//
//	*       任意长度（含空）；
//	?       任意一个字节；
//	[abc]   集合；[^abc] 取反；[a-z] 区间；
//	\x      转义，x 按字面量。
//
// 匹配按字节进行（键是二进制安全的）。键里的 * ? 等字符在被匹配的一侧只是普通字符，不会被展开。

type gtok struct {
	kind  byte // 'l' 字面量 '?' 任意字节 '*' 星号 '[' 集合
	b     byte
	neg   bool
	items []gitem
}

type gitem struct{ lo, hi byte }

func compileGlob(p string) ([]gtok, error) {
	var out []gtok
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if n := len(out); n > 0 && out[n-1].kind == '*' {
				continue
			}
			out = append(out, gtok{kind: '*'})
		case '?':
			out = append(out, gtok{kind: '?'})
		case '\\':
			if i+1 < len(p) {
				i++
			}
			out = append(out, gtok{kind: 'l', b: p[i]})
		case '[':
			t := gtok{kind: '['}
			i++
			if i < len(p) && p[i] == '^' {
				t.neg = true
				i++
			}
			closed := false
			for ; i < len(p); i++ {
				if p[i] == ']' {
					closed = true
					break
				}
				lo := p[i]
				if lo == '\\' && i+1 < len(p) {
					i++
					lo = p[i]
				}
				hi := lo
				if i+2 < len(p) && p[i+1] == '-' && p[i+2] != ']' {
					hi = p[i+2]
					i += 2
					if hi == '\\' && i+1 < len(p) {
						i++
						hi = p[i]
					}
				}
				if lo > hi {
					lo, hi = hi, lo
				}
				t.items = append(t.items, gitem{lo, hi})
			}
			if !closed {
				return nil, invalid("键匹配模式 \"%s\" 里的 [ 没有对应的 ]", p)
			}
			out = append(out, t)
		default:
			out = append(out, gtok{kind: 'l', b: c})
		}
	}
	return out, nil
}

func (t *gtok) match(c byte) bool {
	switch t.kind {
	case 'l':
		return t.b == c
	case '?':
		return true
	case '[':
		in := false
		for _, it := range t.items {
			if c >= it.lo && c <= it.hi {
				in = true
				break
			}
		}
		return in != t.neg
	}
	return false
}

// GlobMatch 判断 s 是否匹配 Redis 风格的 pattern。pattern 不合法（如 [ 未闭合）返回 false。
func GlobMatch(pattern, s string) bool {
	toks, err := compileGlob(pattern)
	if err != nil {
		return false
	}
	return matchToks(toks, s)
}

// matchToks 双指针回溯：只在最近一个 * 处回退，复杂度 O(len(toks) * len(s))，不会指数爆炸。
func matchToks(toks []gtok, s string) bool {
	ti, si := 0, 0
	starT, starS := -1, 0
	for si < len(s) {
		switch {
		case ti < len(toks) && toks[ti].kind == '*':
			starT, starS = ti, si
			ti++
		case ti < len(toks) && toks[ti].match(s[si]):
			ti++
			si++
		case starT >= 0:
			starS++
			si = starS
			ti = starT + 1
		default:
			return false
		}
	}
	for ti < len(toks) && toks[ti].kind == '*' {
		ti++
	}
	return ti == len(toks)
}

// ValidatePattern 校验键匹配模式：非空、不过长、方括号闭合、不是纯通配符（至少要有一个固定字符，避免一条规则放行全部键）、是合法 UTF-8。
func ValidatePattern(p string) error {
	if p == "" {
		return invalid("键匹配模式不能为空")
	}
	if len(p) > MaxPatternLen {
		return invalid("键匹配模式过长（最多 %d 字节）", MaxPatternLen)
	}
	if !utf8.ValidString(p) {
		return invalid("键匹配模式必须是合法的 UTF-8 文本")
	}
	toks, err := compileGlob(p)
	if err != nil {
		return err
	}
	for _, t := range toks {
		if t.kind == 'l' {
			return nil
		}
	}
	return invalid("键匹配模式 \"%s\" 只含通配符：请至少写一个固定字符（如前缀 cache:*），避免一条规则放行全部键", p)
}
