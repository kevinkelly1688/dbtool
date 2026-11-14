// @author: kevinkelly
// @license: Apache Licence
// @file: where.go
// @time: 2026/10/28

package engine

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 附加条件（Spec.ExtraWhere）的解析器。
//
// 思路：不做“黑名单过滤原文”，而是把原文词法分析后按一个很小的文法重新解析，通过后用记号重新生成 SQL 片段
// （标识符一律加反引号、字符串统一单引号并转义、关键字大写）。原文里的任何东西，只要文法不认识就拒绝，
// 所以即使词法层面有疏漏，也不会把原文直接拼进 SQL。
//
// 文法：
//   expr    := or
//   or      := and { OR and }
//   and     := not { AND not }
//   not     := NOT not | primary
//   primary := "(" expr ")" | predicate
//   predicate := operand ( cmp operand
//                        | [NOT] IN "(" operand {"," operand} ")"
//                        | [NOT] LIKE operand
//                        | [NOT] BETWEEN operand AND operand
//                        | IS [NOT] (NULL | TRUE | FALSE) )
//   operand := 列名 | 字符串 | 数字 | NULL | TRUE | FALSE
//   cmp     := = <> != < <= > >= <=>

type tokKind int

const (
	tkIdent  tokKind = iota // 未加引号的列名
	tkQIdent                // 反引号列名
	tkString
	tkNumber
	tkKeyword // 允许的关键字（大写）
	tkOp
	tkLParen
	tkRParen
	tkComma
)

type token struct {
	kind tokKind
	val  string
}

// 允许出现的关键字。
var extraKeywords = map[string]bool{
	"and": true, "or": true, "not": true, "in": true, "is": true, "null": true,
	"like": true, "between": true, "true": true, "false": true,
}

// 明确禁止的关键字：命中时给出具体提示（即使它们本来也过不了文法）。
var extraForbidden = map[string]bool{
	"select": true, "union": true, "sleep": true, "benchmark": true, "outfile": true, "dumpfile": true, "load_file": true,
	"insert": true, "update": true, "delete": true, "drop": true, "alter": true, "create": true, "truncate": true,
	"replace": true, "grant": true, "revoke": true, "call": true, "exec": true, "execute": true, "handler": true,
	"set": true, "rename": true, "join": true, "from": true, "where": true, "group": true, "having": true,
	"order": true, "limit": true, "into": true, "for": true, "lock": true, "show": true, "use": true,
	"if": true, "case": true, "when": true, "then": true, "else": true, "end": true, "exists": true,
	"all": true, "any": true, "some": true, "interval": true, "xor": true, "regexp": true, "rlike": true,
	"div": true, "mod": true, "collate": true, "binary": true, "cast": true, "convert": true,
	"load": true, "do": true, "prepare": true, "deallocate": true, "kill": true, "shutdown": true,
	"with": true, "values": true, "table": true, "database": true, "schema": true, "procedure": true,
	"function": true, "trigger": true, "view": true, "index": true, "describe": true, "explain": true,
}

func isWordStart(r rune) bool { return unicode.IsLetter(r) || r == '_' || r == '$' }
func isWordPart(r rune) bool  { return isWordStart(r) || unicode.IsDigit(r) }

func lexExtra(s string) ([]token, error) {
	rs := []rune(s)
	var toks []token
	n := len(rs)
	prevIsOperand := func() bool {
		if len(toks) == 0 {
			return false
		}
		t := toks[len(toks)-1]
		switch t.kind {
		case tkIdent, tkQIdent, tkString, tkNumber, tkRParen:
			return true
		case tkKeyword:
			return t.val == "NULL" || t.val == "TRUE" || t.val == "FALSE"
		}
		return false
	}
	for i := 0; i < n; {
		r := rs[i]
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			i++
		case r == ';':
			return nil, fmt.Errorf("不允许分号（只能是一个条件表达式，不能写多条语句）")
		case r == '#':
			return nil, fmt.Errorf("不允许注释符（-- /* */ #）")
		case r == '\\':
			return nil, fmt.Errorf("不允许反斜杠")
		case r == '/' && i+1 < n && rs[i+1] == '*', r == '*' && i+1 < n && rs[i+1] == '/':
			return nil, fmt.Errorf("不允许注释符（-- /* */ #）")
		case r == '-' && i+1 < n && rs[i+1] == '-':
			return nil, fmt.Errorf("不允许注释符（-- /* */ #）")
		case r == '\'' || r == '"':
			q := r
			var b strings.Builder
			j := i + 1
			closed := false
			for j < n {
				c := rs[j]
				if c == q {
					if j+1 < n && rs[j+1] == q { // 加倍的引号 = 一个引号
						b.WriteRune(q)
						j += 2
						continue
					}
					closed = true
					j++
					break
				}
				if c == '\\' {
					return nil, fmt.Errorf("不允许反斜杠")
				}
				if c < 0x20 && c != '\t' {
					return nil, fmt.Errorf("字符串里不允许控制字符")
				}
				b.WriteRune(c)
				j++
			}
			if !closed {
				return nil, fmt.Errorf("引号不成对")
			}
			toks = append(toks, token{tkString, b.String()})
			i = j
		case r == '`':
			j := i + 1
			for j < n && rs[j] != '`' {
				j++
			}
			if j >= n {
				return nil, fmt.Errorf("反引号不成对")
			}
			name := string(rs[i+1 : j])
			if !validIdent(name) {
				return nil, fmt.Errorf("列名 %s 含不允许的字符", quote(name))
			}
			if j+1 < n && rs[j+1] == '`' {
				return nil, fmt.Errorf("列名里不允许反引号")
			}
			toks = append(toks, token{tkQIdent, name})
			i = j + 1
		case unicode.IsDigit(r) || (r == '.' && i+1 < n && unicode.IsDigit(rs[i+1])) ||
			((r == '-' || r == '+') && i+1 < n && (unicode.IsDigit(rs[i+1]) || rs[i+1] == '.') && !prevIsOperand()):
			j := i
			if rs[j] == '-' || rs[j] == '+' {
				j++
			}
			for j < n && unicode.IsDigit(rs[j]) {
				j++
			}
			if j < n && rs[j] == '.' {
				j++
				for j < n && unicode.IsDigit(rs[j]) {
					j++
				}
			}
			if j < n && (rs[j] == 'e' || rs[j] == 'E') {
				k := j + 1
				if k < n && (rs[k] == '+' || rs[k] == '-') {
					k++
				}
				if k < n && unicode.IsDigit(rs[k]) {
					for k < n && unicode.IsDigit(rs[k]) {
						k++
					}
					j = k
				}
			}
			if j < n && isWordPart(rs[j]) {
				return nil, fmt.Errorf("数字后面不能紧跟字母（不支持十六进制 / 二进制字面量）：%s", quote(string(rs[i:j+1])))
			}
			num := string(rs[i:j])
			if num == "-" || num == "+" || num == "." {
				return nil, fmt.Errorf("数字格式不对")
			}
			toks = append(toks, token{tkNumber, strings.TrimPrefix(num, "+")})
			i = j
		case isWordStart(r):
			j := i
			for j < n && isWordPart(rs[j]) {
				j++
			}
			word := string(rs[i:j])
			if j < n && rs[j] == '.' {
				return nil, fmt.Errorf("不允许带库名 / 表名限定的列名（%s.…）", word)
			}
			lw := strings.ToLower(word)
			switch {
			case extraKeywords[lw]:
				toks = append(toks, token{tkKeyword, strings.ToUpper(word)})
			case extraForbidden[lw]:
				return nil, fmt.Errorf("不允许出现关键字 %s（禁止子查询、联表、函数副作用、DML / DDL）", strings.ToUpper(word))
			default:
				if !validIdent(word) {
					return nil, fmt.Errorf("列名 %s 含不允许的字符", quote(word))
				}
				toks = append(toks, token{tkIdent, word})
			}
			i = j
		case r == '(':
			toks = append(toks, token{tkLParen, "("})
			i++
		case r == ')':
			toks = append(toks, token{tkRParen, ")"})
			i++
		case r == ',':
			toks = append(toks, token{tkComma, ","})
			i++
		case r == '=':
			toks = append(toks, token{tkOp, "="})
			i++
		case r == '<':
			switch {
			case i+2 < n && rs[i+1] == '=' && rs[i+2] == '>':
				toks = append(toks, token{tkOp, "<=>"})
				i += 3
			case i+1 < n && rs[i+1] == '=':
				toks = append(toks, token{tkOp, "<="})
				i += 2
			case i+1 < n && rs[i+1] == '>':
				toks = append(toks, token{tkOp, "<>"})
				i += 2
			default:
				toks = append(toks, token{tkOp, "<"})
				i++
			}
		case r == '>':
			if i+1 < n && rs[i+1] == '=' {
				toks = append(toks, token{tkOp, ">="})
				i += 2
			} else {
				toks = append(toks, token{tkOp, ">"})
				i++
			}
		case r == '!':
			if i+1 < n && rs[i+1] == '=' {
				toks = append(toks, token{tkOp, "<>"})
				i += 2
			} else {
				return nil, fmt.Errorf("不允许的字符 !")
			}
		case r < 0x20:
			return nil, fmt.Errorf("不允许控制字符")
		default:
			return nil, fmt.Errorf("不允许的字符 %s（只允许比较运算、IN、LIKE、BETWEEN、IS NULL 与 AND / OR）", quote(string(r)))
		}
	}
	return toks, nil
}

type extraParser struct {
	toks  []token
	pos   int
	buf   []byte
	cols  []string
	seen  map[string]bool
	depth int
}

func (p *extraParser) peek() (token, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return token{}, false
}

func (p *extraParser) emit(s string) {
	if n := len(p.buf); n > 0 && p.buf[n-1] != '(' && s != ")" {
		p.buf = append(p.buf, ' ')
	}
	p.buf = append(p.buf, s...)
}

func (p *extraParser) isKw(kw string) bool {
	t, ok := p.peek()
	return ok && t.kind == tkKeyword && t.val == kw
}

func (p *extraParser) expr() error {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > 20 {
		return fmt.Errorf("括号嵌套太深")
	}
	if err := p.and(); err != nil {
		return err
	}
	for p.isKw("OR") {
		p.pos++
		p.emit("OR")
		if err := p.and(); err != nil {
			return err
		}
	}
	return nil
}

func (p *extraParser) and() error {
	if err := p.not(); err != nil {
		return err
	}
	for p.isKw("AND") {
		p.pos++
		p.emit("AND")
		if err := p.not(); err != nil {
			return err
		}
	}
	return nil
}

func (p *extraParser) not() error {
	if p.isKw("NOT") {
		p.pos++
		p.emit("NOT")
		return p.not()
	}
	return p.primary()
}

func (p *extraParser) primary() error {
	if t, ok := p.peek(); ok && t.kind == tkLParen {
		p.pos++
		p.emit("(")
		if err := p.expr(); err != nil {
			return err
		}
		if t, ok := p.peek(); !ok || t.kind != tkRParen {
			return fmt.Errorf("括号不成对")
		}
		p.pos++
		p.emit(")")
		return nil
	}
	return p.predicate()
}

func (p *extraParser) operand() error {
	t, ok := p.peek()
	if !ok {
		return fmt.Errorf("条件不完整：缺少值或列名")
	}
	switch t.kind {
	case tkIdent, tkQIdent:
		p.pos++
		// 函数调用：标识符后面直接跟左括号
		if n, ok := p.peek(); ok && n.kind == tkLParen {
			return fmt.Errorf("不允许函数调用 %s()（只允许比较运算、IN、LIKE、BETWEEN、IS NULL 与 AND / OR）", t.val)
		}
		p.emit(quoteIdent(t.val))
		lc := strings.ToLower(t.val)
		if !p.seen[lc] {
			p.seen[lc] = true
			p.cols = append(p.cols, t.val)
		}
	case tkString:
		p.pos++
		p.emit("'" + strings.ReplaceAll(t.val, "'", "''") + "'")
	case tkNumber:
		p.pos++
		p.emit(t.val)
	case tkKeyword:
		if t.val == "NULL" || t.val == "TRUE" || t.val == "FALSE" {
			p.pos++
			p.emit(t.val)
			return nil
		}
		return fmt.Errorf("此处不应出现 %s", t.val)
	default:
		return fmt.Errorf("条件不完整：此处需要值或列名")
	}
	return nil
}

func (p *extraParser) predicate() error {
	if err := p.operand(); err != nil {
		return err
	}
	t, ok := p.peek()
	if !ok {
		return fmt.Errorf("不是有效的条件表达式（缺少比较运算 / IN / LIKE / IS NULL）")
	}
	if t.kind == tkOp {
		p.pos++
		p.emit(t.val)
		return p.operand()
	}
	if t.kind != tkKeyword {
		return fmt.Errorf("不是有效的条件表达式（缺少比较运算 / IN / LIKE / IS NULL）")
	}
	neg := false
	if t.val == "NOT" {
		neg = true
		p.pos++
		p.emit("NOT")
		t, ok = p.peek()
		if !ok || t.kind != tkKeyword {
			return fmt.Errorf("NOT 后面应是 IN / LIKE / BETWEEN")
		}
	}
	switch t.val {
	case "IN":
		p.pos++
		p.emit("IN")
		if n, ok := p.peek(); !ok || n.kind != tkLParen {
			return fmt.Errorf("IN 后面应是括号列表")
		}
		p.pos++
		p.emit("(")
		cnt := 0
		for {
			if err := p.operand(); err != nil {
				return err
			}
			cnt++
			if cnt > 200 {
				return fmt.Errorf("IN 列表最多 200 项")
			}
			n, ok := p.peek()
			if !ok {
				return fmt.Errorf("括号不成对")
			}
			p.pos++
			if n.kind == tkComma {
				p.buf = append(p.buf, ',')
				continue
			}
			if n.kind == tkRParen {
				p.emit(")")
				return nil
			}
			return fmt.Errorf("IN 列表里只能是值，用逗号分隔")
		}
	case "LIKE":
		p.pos++
		p.emit("LIKE")
		return p.operand()
	case "BETWEEN":
		p.pos++
		p.emit("BETWEEN")
		if err := p.operand(); err != nil {
			return err
		}
		if !p.isKw("AND") {
			return fmt.Errorf("BETWEEN 需要 AND")
		}
		p.pos++
		p.emit("AND")
		return p.operand()
	case "IS":
		if neg {
			return fmt.Errorf("不是有效的条件表达式")
		}
		p.pos++
		p.emit("IS")
		if p.isKw("NOT") {
			p.pos++
			p.emit("NOT")
		}
		n, ok := p.peek()
		if !ok || n.kind != tkKeyword || (n.val != "NULL" && n.val != "TRUE" && n.val != "FALSE") {
			return fmt.Errorf("IS 后面应是 NULL / TRUE / FALSE（或 NOT NULL）")
		}
		p.pos++
		p.emit(n.val)
		return nil
	}
	return fmt.Errorf("不是有效的条件表达式（缺少比较运算 / IN / LIKE / IS NULL）")
}

// NormalizeExtra 校验并规范化附加条件，返回可安全拼进 WHERE 的片段和其中引用的列名（去重，保持出现顺序）。
// 空串返回 ("", nil, nil)。长度按字符数，上限 500。
func NormalizeExtra(s string) (string, []string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil, nil
	}
	if utf8.RuneCountInString(s) > maxExtraLen {
		return "", nil, fmt.Errorf("附加条件最长 %d 字（当前 %d 字）", maxExtraLen, utf8.RuneCountInString(s))
	}
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		return "", nil, fmt.Errorf("附加条件含非法字符")
	}
	toks, err := lexExtra(s)
	if err != nil {
		return "", nil, err
	}
	if len(toks) == 0 {
		return "", nil, nil
	}
	p := &extraParser{toks: toks, seen: map[string]bool{}}
	if err := p.expr(); err != nil {
		return "", nil, err
	}
	if p.pos < len(p.toks) {
		if p.toks[p.pos].kind == tkRParen {
			return "", nil, fmt.Errorf("括号不成对")
		}
		return "", nil, fmt.Errorf("条件表达式在 %s 附近有多余内容", quote(p.toks[p.pos].val))
	}
	return string(p.buf), p.cols, nil
}

func buildWhere(spec Spec) (string, error) {
	if !validIdent(spec.TimeColumn) {
		return "", fmt.Errorf("时间列名无效：%s", quote(spec.TimeColumn))
	}
	if spec.RetentionDays < 1 {
		return "", fmt.Errorf("保留天数必须是正整数")
	}
	unit, ok := NormalizeTimeUnit(spec.TimeUnit)
	if !ok {
		return "", fmt.Errorf("时间单位无效：%s", quote(spec.TimeUnit))
	}
	col := quoteIdent(spec.TimeColumn)
	var where string
	if IsEpochUnit(unit) {
		// col > 0：0 / NULL 表示“未设置”，不当作很早的数据归档；常量在右边，列上不套函数，索引可用
		where = fmt.Sprintf("%s > 0 AND %s < %s", col, col, epochCutoff(unit, spec.RetentionDays))
	} else {
		where = fmt.Sprintf("%s < NOW() - INTERVAL %d DAY", col, spec.RetentionDays)
	}
	extra, _, err := NormalizeExtra(spec.ExtraWhere)
	if err != nil {
		return "", fmt.Errorf("附加条件：%w", err)
	}
	if extra != "" {
		where += " AND (" + extra + ")"
	}
	return where, nil
}
