// @author: kevinkelly
// @license: Apache Licence
// @file: facts.go
// @time: 2026/10/28

package firewall

import (
	"errors"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"time"

	"dbtool/internal/sqlx"
)

// ---- 指纹与脱字面量 ----

var inListRe = regexp.MustCompile(`\bin \( \?( , \?)+ \)`)

// Fingerprint 返回语句指纹：归一化文本与它的 FNV-1a 32 位十六进制（8 位）。
// 归一化：去注释；标识符转小写（反引号去掉）；字符串与数字字面量换成 ?；IN 列表折叠为 in ( ? )；空白折叠；去掉末尾分号。
// 指纹只用于等值匹配与聚合（黑名单、例外、命中合并），不是安全边界。
func Fingerprint(sql string) (text, id string) {
	var parts []string
	for _, t := range sqlx.Tokenize(sql) {
		switch t.Kind {
		case sqlx.Space, sqlx.Comment:
		case sqlx.Word:
			parts = append(parts, strings.ToLower(t.Text))
		case sqlx.Quoted:
			parts = append(parts, strings.ToLower(strings.Trim(t.Text, "`\"")))
		case sqlx.String, sqlx.Number:
			parts = append(parts, "?")
		default:
			parts = append(parts, t.Text)
		}
	}
	for len(parts) > 0 && parts[len(parts)-1] == ";" {
		parts = parts[:len(parts)-1]
	}
	text = inListRe.ReplaceAllString(strings.Join(parts, " "), "in ( ? )")
	h := fnv.New32a()
	_, _ = h.Write([]byte(text))
	return text, fmt.Sprintf("%08x", h.Sum32())
}

// Redact 把语句里的字符串与数字字面量换成 ?、去掉注释，保留其余原样（命中记录默认只存这种形态：
// 语句里可能带手机号、身份证等明文条件值）。
func Redact(sql string) string {
	var b strings.Builder
	for _, t := range sqlx.Tokenize(sql) {
		switch t.Kind {
		case sqlx.Comment:
			b.WriteByte(' ')
		case sqlx.String:
			b.WriteString("'?'")
		case sqlx.Number:
			b.WriteByte('?')
		default:
			b.WriteString(t.Text)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// ---- 词法事实 ----

// facts 是一次求值里语句的事实。词法事实在构造时一次算好（微秒级）；对象（语法树 / 词法表解析）与扫描行数按需取，各取一次。
type facts struct {
	req *Request
	now time.Time

	multi bool
	kind  string

	hasFrom     bool // 最外层有 FROM
	hasWhere    bool // 最外层任意位置有 WHERE
	hasLimit    bool // 最外层有 LIMIT / TOP / FETCH
	star        bool // 最外层选择列表含 *
	cartesian   bool
	leadingWild bool
	bigIn       bool

	all    []sqlx.Token // 整个输入（所有语句）的有效词，供关键字 / 标识符判断
	words  []string     // 与 all 对齐：Word 为小写文本，其他为 ""
	idents map[string]bool
	clean  string // 去注释后的全文，供正则

	fpText, fpID string

	objects     []Object
	objectsDone bool
	objectsErr  error
	scan        int64
	scanDone    bool
	scanErr     error
}

func newFacts(req *Request, now time.Time) *facts {
	f := &facts{req: req, now: now}
	sql := req.SQL
	if len(sql) > maxFactsBytes {
		sql = sql[:maxFactsBytes]
	}
	f.clean = sqlx.StripComments(sql)
	f.fpText, f.fpID = Fingerprint(sql)

	all := sqlx.Significant(sqlx.Tokenize(sql))
	f.all = all
	f.words = make([]string, len(all))
	f.idents = map[string]bool{}
	for i, t := range all {
		switch t.Kind {
		case sqlx.Word:
			f.words[i] = strings.ToLower(t.Text)
			f.idents[f.words[i]] = true
		case sqlx.Quoted:
			f.idents[strings.ToLower(strings.Trim(t.Text, "`\""))] = true
		}
	}

	stmts := sqlx.Split(sql)
	f.multi = len(stmts) > 1
	if len(stmts) == 0 {
		return f
	}
	first := stmts[0]
	f.kind = stmtKind(first)
	sig := sqlx.Significant(sqlx.Tokenize(first))
	f.scanStructure(sig)
	return f
}

// stmtKind 把语句归为 SELECT / INSERT / UPDATE / DELETE / CREATE / ALTER / DROP / TRUNCATE；其余返回首个关键字的大写。
func stmtKind(stmt string) string {
	kw := sqlx.FirstKeyword(stmt)
	if kw == "" { // 以括号开头：(SELECT …) UNION (SELECT …)
		for _, t := range sqlx.Significant(sqlx.Tokenize(stmt)) {
			if t.Kind == sqlx.Word {
				kw = t.Lower()
				break
			}
		}
	}
	if kw == "with" {
		if m := sqlx.WithMainKeyword(stmt); m != "" {
			kw = m
		}
	}
	switch kw {
	case "select":
		return "SELECT"
	case "insert", "replace":
		return "INSERT"
	case "update":
		return "UPDATE"
	case "delete":
		return "DELETE"
	case "create":
		return "CREATE"
	case "alter", "rename":
		return "ALTER"
	case "drop":
		return "DROP"
	case "truncate":
		return "TRUNCATE"
	}
	return strings.ToUpper(kw)
}

// fromEnd 结束 FROM 列表的关键字（最外层）。
var fromEnd = map[string]bool{
	"where": true, "group": true, "having": true, "order": true, "limit": true, "union": true, "intersect": true,
	"except": true, "window": true, "for": true, "lock": true, "into": true, "procedure": true,
}

// scanStructure 在第一条语句的词序列上，按括号深度判定结构特征（子查询里的 WHERE / LIMIT / * 不算最外层）。
func (f *facts) scanStructure(sig []sqlx.Token) {
	depth, fromAt, joins, ons := 0, -1, 0, 0
	commaInFrom, commaInUpdate := false, false
	inFrom, inUpdate := false, false
	for i, t := range sig {
		if t.Kind == sqlx.Punct {
			switch t.Text {
			case "(":
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			case ",":
				if depth == 0 {
					if inFrom {
						commaInFrom = true
					}
					if inUpdate {
						commaInUpdate = true
					}
				}
			case "*":
				// 最外层选择列表（第一个最外层 FROM 之前）里的通配列：SELECT * / SELECT t.* / , *
				if depth == 0 && fromAt < 0 && i > 0 {
					p := sig[i-1]
					switch {
					case p.Kind == sqlx.Punct && (p.Text == "," || p.Text == "."):
						f.star = true
					case p.Kind == sqlx.Word:
						switch p.Lower() {
						case "select", "distinct", "distinctrow", "all", "straight_join", "sql_calc_found_rows", "sql_no_cache", "sql_cache":
							f.star = true
						}
					}
				}
			}
			continue
		}
		if t.Kind != sqlx.Word {
			continue
		}
		lw := t.Lower()
		switch lw {
		case "join":
			joins++
		case "using":
			ons++
		case "on":
			if !(i+1 < len(sig) && sig[i+1].Kind == sqlx.Word && sig[i+1].Lower() == "duplicate") {
				ons++
			}
		case "like":
			if i+1 < len(sig) && sig[i+1].Kind == sqlx.String && len(sig[i+1].Text) > 1 && sig[i+1].Text[1] == '%' {
				f.leadingWild = true
			}
		case "in":
			if i+1 < len(sig) && sig[i+1].Kind == sqlx.Punct && sig[i+1].Text == "(" && inListItems(sig, i+2) > BigInItems {
				f.bigIn = true
			}
		}
		if depth != 0 {
			continue
		}
		switch lw {
		case "from":
			f.hasFrom = true
			if fromAt < 0 {
				fromAt = i
			}
			inFrom = true
		case "update":
			if i == 0 {
				inUpdate = true
			}
		case "set":
			inUpdate = false
		case "where":
			f.hasWhere = true
			inFrom = false
		case "limit", "fetch":
			f.hasLimit = true
			inFrom = false
		case "top":
			if i > 0 && sig[i-1].Kind == sqlx.Word && (sig[i-1].Lower() == "select" || sig[i-1].Lower() == "distinct") {
				f.hasLimit = true
			}
		default:
			if fromEnd[lw] {
				inFrom = false
			}
		}
	}
	switch f.kind {
	case "SELECT", "UPDATE", "DELETE":
		f.cartesian = joins > ons || (!f.hasWhere && ((f.kind != "UPDATE" && commaInFrom) || (f.kind == "UPDATE" && commaInUpdate)))
	}
}

// inListItems 统计从 from 开始（紧跟在左括号之后）到匹配右括号的 IN 列表项数；子查询（以 SELECT / WITH 开头）返回 0。
func inListItems(sig []sqlx.Token, from int) int {
	if from < len(sig) && sig[from].Kind == sqlx.Word {
		switch sig[from].Lower() {
		case "select", "with", "values":
			return 0
		}
	}
	depth, items := 1, 1
	for i := from; i < len(sig); i++ {
		t := sig[i]
		if t.Kind != sqlx.Punct {
			continue
		}
		switch t.Text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return items
			}
		case ",":
			if depth == 1 {
				items++
			}
		}
	}
	return items
}

// hasPhrase 判断整个输入里是否出现由若干个词（空白分隔、不区分大小写）连续组成的短语。
func (f *facts) hasPhrase(phrase string) bool {
	ws := strings.Fields(strings.ToLower(phrase))
	if len(ws) == 0 {
		return false
	}
	for i := 0; i+len(ws) <= len(f.words); i++ {
		ok := true
		for j, w := range ws {
			if f.words[i+j] != w {
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

// getObjects 取语句涉及的对象（只取一次）。库名为空的按请求的当前库补全。
func (f *facts) getObjects() ([]Object, error) {
	if f.objectsDone {
		return f.objects, f.objectsErr
	}
	f.objectsDone = true
	if f.req.Tables == nil {
		f.objectsErr = errNoTables
		return nil, f.objectsErr
	}
	objs, err := f.req.Tables()
	if err != nil {
		f.objectsErr = err
		return nil, err
	}
	out := make([]Object, len(objs)) // 复制：不改调用方返回的切片（它可能被缓存复用）
	for i, o := range objs {
		if o.Schema == "" {
			o.Schema = f.req.DB
		}
		out[i] = o
	}
	f.objects = out
	return out, nil
}

func (f *facts) getScanRows() (int64, error) {
	if f.scanDone {
		return f.scan, f.scanErr
	}
	f.scanDone = true
	if f.req.ScanRows == nil {
		f.scanErr = errNoExplain
		return 0, f.scanErr
	}
	f.scan, f.scanErr = f.req.ScanRows()
	return f.scan, f.scanErr
}

// getAffectRows 取预估影响行数：调用方给了（工单检查结果里 goInception 的值）就用它；
// 否则对 UPDATE / DELETE 退回 EXPLAIN 的扫描行数（受影响行数的上界）；都没有就是未知。
// 第二个返回值是说明依据，拼进命中说明里。
func (f *facts) getAffectRows() (int64, string, error) {
	if f.req.AffectRows >= 0 {
		return f.req.AffectRows, "", nil
	}
	if (f.kind == "UPDATE" || f.kind == "DELETE") && f.req.ScanRows != nil {
		n, err := f.getScanRows()
		if err != nil {
			return 0, "", fmt.Errorf("没有预估影响行数，EXPLAIN 也取不到（%s）", err.Error())
		}
		return n, "（EXPLAIN 扫描行数，受影响行数的上界）", nil
	}
	return 0, "", errors.New("没有预估影响行数")
}

func (f *facts) summary() FactsSummary {
	s := FactsSummary{Kind: f.kind, Fingerprint: f.fpID, Multi: f.multi}
	if f.objectsDone && f.objectsErr == nil {
		for _, o := range f.objects {
			s.Objects = append(s.Objects, strings.Trim(o.Schema+"."+o.Table, "."))
		}
	}
	return s
}
