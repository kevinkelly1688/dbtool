// @author: kevinkelly
// @license: Apache Licence
// @file: format.go
// @time: 2026/10/28

package sqlopt

import (
	"strings"

	"dbtool/internal/sqlx"
)

// PrettySQL 美化 SQL（对应页面的“美化”按钮）：主要子句各占一行，SELECT / SET / GROUP BY / ORDER BY 列表逐项换行，
// 条件里的 AND / OR 换行缩进，子查询缩进，关键字转大写。字符串、标识符、注释原样保留。
// 只做词法级处理，不改变语义；写法特别的语句可能排版不够漂亮。
func PrettySQL(sql string) string {
	f := &formatter{toks: sqlx.Tokenize(sql), atStart: true}
	f.stack = []fmtLevel{{sub: true}}
	f.run()
	return strings.TrimSpace(f.b.String())
}

type fmtLevel struct {
	sub, inList, inCond, between bool
	base                         int // 本层子句关键字的缩进级别
}

type formatter struct {
	toks    []sqlx.Token
	b       strings.Builder
	stack   []fmtLevel
	line    int  // 当前行的缩进级别
	atStart bool // 当前在行首
	space   bool // 下一个词前需要一个空格
	tight   bool // 下一个词前不要空格（点号之后、左括号之后）
	brk     bool // 上一个词是行注释：下一个词必须换行
}

var clauseMulti = [][]string{
	{"on", "duplicate", "key", "update"}, {"group", "by"}, {"order", "by"}, {"union", "all"}, {"union", "distinct"},
	{"insert", "into"}, {"delete", "from"}, {"left", "outer", "join"}, {"right", "outer", "join"}, {"left", "join"},
	{"right", "join"}, {"inner", "join"}, {"cross", "join"}, {"natural", "join"}, {"for", "update"}, {"lock", "in", "share", "mode"},
}

var clauseSingle = map[string]bool{"select": true, "from": true, "where": true, "having": true, "limit": true, "union": true,
	"set": true, "values": true, "value": true, "update": true, "join": true, "insert": true, "delete": true, "replace": true,
	"with": true, "straight_join": true, "window": true}

var upperWords = map[string]bool{"and": true, "or": true, "not": true, "in": true, "is": true, "null": true, "like": true,
	"between": true, "exists": true, "as": true, "on": true, "using": true, "distinct": true, "all": true, "asc": true,
	"desc": true, "case": true, "when": true, "then": true, "else": true, "end": true, "by": true, "into": true,
	"offset": true, "true": true, "false": true, "interval": true, "outer": true, "inner": true, "left": true,
	"right": true, "cross": true, "natural": true, "duplicate": true, "key": true, "div": true, "mod": true, "xor": true,
	"regexp": true, "rlike": true, "escape": true, "collate": true, "over": true, "partition": true, "ignore": true}

// sigWords 返回从第 i 个词开始的最多 n 个有效单词（跳过空白与注释）的小写形式与各自的下标。
func (f *formatter) sigWords(i, n int) (words []string, idx []int) {
	for ; i < len(f.toks) && len(words) < n; i++ {
		t := f.toks[i]
		if t.Kind == sqlx.Space || t.Kind == sqlx.Comment {
			continue
		}
		if t.Kind != sqlx.Word {
			break
		}
		words = append(words, t.Lower())
		idx = append(idx, i)
	}
	return
}

// matchClause 判断 i 处是否是子句关键字，返回大写文本与最后一个词的下标。
func (f *formatter) matchClause(i int) (string, int, bool) {
	words, idx := f.sigWords(i, 4)
	for _, m := range clauseMulti {
		if len(words) >= len(m) {
			ok := true
			for k := range m {
				ok = ok && words[k] == m[k]
			}
			if ok {
				return strings.ToUpper(strings.Join(m, " ")), idx[len(m)-1], true
			}
		}
	}
	if len(words) > 0 && clauseSingle[words[0]] {
		return strings.ToUpper(words[0]), idx[0], true
	}
	return "", 0, false
}

func (f *formatter) top() *fmtLevel {
	return &f.stack[len(f.stack)-1]
}

func (f *formatter) newline(indent int) {
	s := strings.TrimRight(f.b.String(), " ")
	f.b.Reset()
	f.b.WriteString(s)
	if f.b.Len() > 0 && !strings.HasSuffix(s, "\n") {
		f.b.WriteByte('\n')
	}
	f.b.WriteString(strings.Repeat("  ", indent))
	f.line = indent
	f.atStart, f.space, f.tight, f.brk = true, false, false, false
}

func (f *formatter) write(text string) {
	if f.brk {
		f.newline(f.top().base)
	}
	if !f.atStart && f.space && !f.tight {
		f.b.WriteByte(' ')
	}
	f.b.WriteString(text)
	f.atStart, f.space, f.tight = false, false, false
}

func (f *formatter) run() {
	for i := 0; i < len(f.toks); i++ {
		t := f.toks[i]
		switch t.Kind {
		case sqlx.Space:
			f.space = true
		case sqlx.Comment:
			f.space = true
			f.write(t.Text)
			if strings.HasPrefix(t.Text, "#") || strings.HasPrefix(t.Text, "--") {
				f.brk = true
			}
			f.space = true
		case sqlx.Punct:
			f.punct(i, t.Text)
		case sqlx.Word:
			i = f.word(i, t)
		default:
			f.write(t.Text)
		}
	}
}

func (f *formatter) punct(i int, p string) {
	switch p {
	case "(":
		words, _ := f.sigWords(i+1, 1)
		sub := len(words) > 0 && (words[0] == "select" || words[0] == "with")
		f.write("(")
		base := f.top().base
		if sub {
			base = f.line + 1
		}
		f.stack = append(f.stack, fmtLevel{sub: sub, base: base})
		if sub {
			f.newline(base)
		} else {
			f.tight = true
		}
	case ")":
		f.space, f.tight = false, false
		if len(f.stack) > 1 {
			lv := *f.top()
			f.stack = f.stack[:len(f.stack)-1]
			if lv.sub {
				f.newline(lv.base - 1)
			}
		}
		f.write(")")
	case ",":
		f.space = false
		f.write(",")
		if lv := f.top(); lv.sub && lv.inList {
			f.newline(lv.base + 1)
		} else {
			f.space = true
		}
	case ";":
		f.space = false
		f.write(";")
		f.b.WriteString("\n\n")
		f.atStart, f.space, f.tight, f.brk = true, false, false, false
		f.stack = f.stack[:1]
		f.stack[0] = fmtLevel{sub: true}
		f.line = 0
	case ".":
		f.tight = true
		f.write(".")
		f.tight = true
	default:
		f.write(p)
	}
}

// word 处理一个单词，返回已消费到的下标。
func (f *formatter) word(i int, t sqlx.Token) int {
	lw := t.Lower()
	lv := f.top()
	if lv.sub {
		if kw, last, ok := f.matchClause(i); ok {
			if !f.atStart && f.b.Len() > 0 {
				f.newline(lv.base)
			}
			f.write(kw)
			f.space = true
			lv.between = false
			switch kw {
			case "SELECT", "SET", "GROUP BY", "ORDER BY":
				lv.inList, lv.inCond = true, false
			case "WHERE", "HAVING":
				lv.inList, lv.inCond = false, true
			case "ON DUPLICATE KEY UPDATE":
				lv.inList, lv.inCond = true, false
			default:
				lv.inList, lv.inCond = false, false
			}
			return last
		}
		switch {
		case lw == "on":
			lv.inList, lv.inCond = false, true
		case lw == "between":
			lv.between = true
		case (lw == "and" || lw == "or") && lv.inCond:
			if lw == "and" && lv.between {
				lv.between = false
			} else {
				f.newline(lv.base + 1)
			}
		}
	}
	if upperWords[lw] {
		f.write(strings.ToUpper(lw))
	} else {
		f.write(t.Text)
	}
	return i
}
