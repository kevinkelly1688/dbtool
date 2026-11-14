// @author: kevinkelly
// @license: Apache Licence
// @file: lex.go
// @time: 2026/10/28

// Package sqlx 提供不依赖具体数据库的轻量 SQL 词法工具：切分语句、去注释、
// 判断 select 列表里的 `*`、改写末尾 LIMIT。
//
// 它只做词法级处理（正确处理引号、反引号、
// 注释、括号嵌套），不构建语法树；更精确的表级解析由 goInception/解析器在后续阶段提供。
package sqlx

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Kind int

const (
	Space Kind = iota
	Comment
	Word   // 标识符或关键字
	Quoted // `ident` 或 "ident"（视作标识符）
	String // 'str'
	Number
	Punct
)

type Token struct {
	Kind       Kind
	Text       string
	Start, End int // 在原文中的字节区间 [Start, End)
}

// Lower 返回 Word 的小写形式，其他类型返回原文。
func (t Token) Lower() string {
	if t.Kind == Word {
		return strings.ToLower(t.Text)
	}
	return t.Text
}

func isWordStart(r rune) bool {
	return r == '_' || r == '$' || r == '@' || unicode.IsLetter(r)
}

func isWordPart(r rune) bool {
	return isWordStart(r) || unicode.IsDigit(r)
}

// Tokenize 把 SQL 切成词。对不完整的引号/注释做宽容处理（吞到结尾）。
func Tokenize(sql string) []Token {
	var toks []Token
	n := len(sql)
	i := 0
	emit := func(k Kind, s, e int) {
		toks = append(toks, Token{Kind: k, Text: sql[s:e], Start: s, End: e})
	}
	for i < n {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			s := i
			for i < n && strings.IndexByte(" \t\n\r\f\v", sql[i]) >= 0 {
				i++
			}
			emit(Space, s, i)
		case c == '#':
			s := i
			for i < n && sql[i] != '\n' {
				i++
			}
			emit(Comment, s, i)
		case c == '-' && i+1 < n && sql[i+1] == '-' && (i+2 >= n || strings.IndexByte(" \t\n\r", sql[i+2]) >= 0):
			s := i
			for i < n && sql[i] != '\n' {
				i++
			}
			emit(Comment, s, i)
		case c == '/' && i+1 < n && sql[i+1] == '*':
			s := i
			i += 2
			for i < n && !(sql[i] == '*' && i+1 < n && sql[i+1] == '/') {
				i++
			}
			if i < n {
				i += 2
			}
			emit(Comment, s, i)
		case c == '\'' || c == '"' || c == '`':
			s := i
			i++
			for i < n {
				if sql[i] == '\\' && c != '`' && i+1 < n {
					i += 2
					continue
				}
				if sql[i] == c {
					if i+1 < n && sql[i+1] == c { // 双写转义
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			if c == '\'' {
				emit(String, s, i)
			} else {
				emit(Quoted, s, i)
			}
		case c >= '0' && c <= '9':
			s := i
			for i < n && (sql[i] >= '0' && sql[i] <= '9' || sql[i] == '.') {
				i++
			}
			emit(Number, s, i)
		default:
			r, size := utf8.DecodeRuneInString(sql[i:])
			if isWordStart(r) {
				s := i
				i += size
				for i < n {
					r, size = utf8.DecodeRuneInString(sql[i:])
					if !isWordPart(r) {
						break
					}
					i += size
				}
				emit(Word, s, i)
			} else {
				i += size
				emit(Punct, i-size, i)
			}
		}
	}
	return toks
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

// StripComments 去掉注释（以一个空格替代，避免相邻词粘连）。
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

// Split 按顶层分号切分语句，忽略引号/注释里的分号；空语句被丢弃，结果去掉首尾空白，不含分号。
func Split(sql string) []string {
	var out []string
	start := 0
	flush := func(end int) {
		if s := strings.TrimSpace(StripComments(sql[start:end])); s != "" {
			out = append(out, s)
		}
	}
	for _, t := range Tokenize(sql) {
		if t.Kind == Punct && t.Text == ";" {
			flush(t.Start)
			start = t.End
		}
	}
	flush(len(sql))
	return out
}

// FirstKeyword 返回语句的第一个关键字（小写）。
func FirstKeyword(stmt string) string {
	for _, t := range Significant(Tokenize(stmt)) {
		if t.Kind == Word {
			return t.Lower()
		}
		return ""
	}
	return ""
}

// HasSelectStar 判断是否存在 `SELECT *`、`SELECT t.*`、`, *` 形式的通配列（含子查询）。
// `COUNT(*)`、`a * b` 等不算。
func HasSelectStar(stmt string) bool {
	sig := Significant(Tokenize(stmt))
	for i, t := range sig {
		if t.Kind != Punct || t.Text != "*" || i == 0 {
			continue
		}
		p := sig[i-1]
		switch {
		case p.Kind == Punct && (p.Text == "," || p.Text == "."):
			return true
		case p.Kind == Word:
			switch p.Lower() {
			case "select", "distinct", "distinctrow", "all", "straight_join", "sql_calc_found_rows", "sql_no_cache", "sql_cache":
				return true
			}
		}
	}
	return false
}

// ContainsSequence 判断是否出现某个关键字序列（忽略大小写、注释与空白），如 "for","update"。
func ContainsSequence(stmt string, words ...string) bool {
	sig := Significant(Tokenize(stmt))
	for i := 0; i+len(words) <= len(sig); i++ {
		ok := true
		for j, w := range words {
			if sig[i+j].Kind != Word || sig[i+j].Lower() != w {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// TouchesMySQLUser 判断是否引用了 mysql.user 表（`mysql`.`user`、mysql . user 等）。
func TouchesMySQLUser(stmt string, currentDB string) bool {
	sig := Significant(Tokenize(stmt))
	name := func(t Token) string {
		if t.Kind == Quoted {
			return strings.ToLower(strings.Trim(t.Text, "`\""))
		}
		if t.Kind == Word {
			return t.Lower()
		}
		return ""
	}
	for i := 0; i+2 < len(sig); i++ {
		if name(sig[i]) == "mysql" && sig[i+1].Text == "." && name(sig[i+2]) == "user" {
			return true
		}
	}
	if strings.EqualFold(currentDB, "mysql") {
		for i, t := range sig {
			if i > 0 && name(t) == "user" && sig[i-1].Kind == Word {
				switch sig[i-1].Lower() {
				case "from", "join":
					return true
				}
			}
		}
	}
	return false
}

// RewriteLimit 给 SELECT 语句施加最大返回行数：
// 已有末尾 `LIMIT n` / `LIMIT m,n` / `LIMIT n OFFSET m` 时取 min(n, max)；没有则追加 `LIMIT max`。
// max<=0 表示不限制，原样返回（去掉结尾分号）。仅识别顶层、位于语句末尾的 LIMIT。
func RewriteLimit(stmt string, max int) string {
	// 先去注释：否则行尾 `-- ...` 注释会把追加的 LIMIT 吞掉。
	stmt = strings.TrimSpace(StripComments(stmt))
	stmt = strings.TrimRight(stmt, "; \t\r\n")
	if max <= 0 {
		return stmt
	}
	sig := Significant(Tokenize(stmt))
	// 计算每个 token 的括号深度（在该 token 之前）
	depth := make([]int, len(sig))
	d := 0
	for i, t := range sig {
		if t.Kind == Punct && t.Text == ")" {
			d--
		}
		depth[i] = d
		if t.Kind == Punct && t.Text == "(" {
			d++
		}
	}
	// 从尾部找 limit 子句
	for i := len(sig) - 1; i >= 0; i-- {
		if depth[i] != 0 {
			continue
		}
		if sig[i].Kind == Word && sig[i].Lower() == "limit" {
			tail := sig[i+1:]
			prefix := strings.TrimSpace(stmt[:sig[i].Start])
			switch {
			case len(tail) == 1 && tail[0].Kind == Number:
				n := clamp(tail[0].Text, max)
				return prefix + " LIMIT " + strconv.Itoa(n)
			case len(tail) == 3 && tail[0].Kind == Number && tail[1].Text == "," && tail[2].Kind == Number:
				n := clamp(tail[2].Text, max)
				return prefix + " LIMIT " + tail[0].Text + "," + strconv.Itoa(n)
			case len(tail) == 3 && tail[0].Kind == Number && tail[1].Kind == Word && tail[1].Lower() == "offset" && tail[2].Kind == Number:
				n := clamp(tail[0].Text, max)
				return prefix + " LIMIT " + strconv.Itoa(n) + " OFFSET " + tail[2].Text
			}
			break // limit 后面不是数字形式（如表达式），按没有处理
		}
	}
	return stmt + " LIMIT " + strconv.Itoa(max)
}

func clamp(s string, max int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n > max {
		return max
	}
	return n
}
