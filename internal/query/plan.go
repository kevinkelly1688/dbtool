// @author: kevinkelly
// @license: Apache Licence
// @file: plan.go
// @time: 2026/10/28

package query

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dbtool/internal/sqlx"
)

// PlanTip 是执行计划的一条风险提示。Level：bad（严重）/ warn（建议处理）/ info。
type PlanTip struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

var (
	fnOnColRe  = regexp.MustCompile("(?i)\\b([a-z_]+)\\s*\\(\\s*(?:[\\w$]+\\s*\\.\\s*)?`?([\\w$]+)`?\\s*\\)\\s*(?:=|>=|<=|<>|!=|>|<|\\bbetween\\b|\\bin\\b|\\blike\\b)")
	deepLimitA = regexp.MustCompile(`(?i)\blimit\s+(\d+)\s*,\s*\d+`)
	deepLimitB = regexp.MustCompile(`(?i)\boffset\s+(\d+)`)
	notFnNames = map[string]bool{"count": true, "sum": true, "avg": true, "max": true, "min": true, "in": true, "and": true, "or": true,
		"not": true, "exists": true, "values": true, "any": true, "all": true, "some": true, "where": true, "on": true}
)

// colIndex 按列名（不区分大小写）找列下标。
func colIndex(cols []string, name string) int {
	for i, c := range cols {
		if strings.EqualFold(c, name) {
			return i
		}
	}
	return -1
}

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case uint64:
		return int64(x), true
	case float64:
		return int64(x), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n, err == nil
	case []byte:
		n, err := strconv.ParseInt(strings.TrimSpace(string(x)), 10, 64)
		return n, err == nil
	}
	return 0, false
}

func cellStr(row []any, i int) string {
	if i < 0 || i >= len(row) || row[i] == nil {
		return ""
	}
	return fmt.Sprint(row[i])
}

// analyzePlan 根据 EXPLAIN 的结果行与原语句给出风险提示：全表扫描、全索引扫描、filesort、临时表、连接缓冲、
// WHERE 里对列用函数、前导 % 的 LIKE、深分页。结果行不是传统表格式（如 FORMAT=TREE）时只做语句层面的检查。
func analyzePlan(stmt string, cols []string, rows [][]any) []PlanTip {
	tips := []PlanTip{}
	ti, ri, ei, tbi := colIndex(cols, "type"), colIndex(cols, "rows"), colIndex(cols, "Extra"), colIndex(cols, "table")
	seenExtra := map[string]bool{}
	for _, r := range rows {
		table := cellStr(r, tbi)
		if table == "" {
			table = "（派生表）"
		}
		n, hasRows := int64(0), false
		if ri >= 0 && ri < len(r) {
			n, hasRows = toInt64(r[ri])
		}
		switch typ := strings.ToUpper(cellStr(r, ti)); typ {
		case "ALL":
			if hasRows && n >= 10000 {
				lv := "warn"
				if n >= 100000 {
					lv = "bad"
				}
				tips = append(tips, PlanTip{lv, fmt.Sprintf("对 %s 做了全表扫描，预计扫描约 %d 行；请检查 WHERE 条件是否命中索引。", table, n)})
			}
		case "INDEX":
			if hasRows && n >= 100000 {
				tips = append(tips, PlanTip{"warn", fmt.Sprintf("对 %s 做了全索引扫描，预计扫描约 %d 行。", table, n)})
			}
		}
		extra := cellStr(r, ei)
		for _, c := range []struct{ key, text string }{
			{"Using filesort", "存在额外排序（Using filesort）：ORDER BY 无法利用索引顺序，结果集大时较慢。"},
			{"Using temporary", "使用了临时表（Using temporary）：常见于 GROUP BY / DISTINCT / UNION 无可用索引。"},
			{"Using join buffer", "连接没有可用索引（Using join buffer）：被驱动表建议给连接列加索引。"},
		} {
			if strings.Contains(extra, c.key) && !seenExtra[c.key] {
				seenExtra[c.key] = true
				tips = append(tips, PlanTip{"warn", c.text})
			}
		}
	}
	tips = append(tips, stmtTips(stmt)...)
	return tips
}

// stmtTips 是只看语句文本的提示。
func stmtTips(stmt string) []PlanTip {
	var tips []PlanTip
	where := whereClause(stmt)
	if where != "" {
		seen := map[string]bool{}
		for _, m := range fnOnColRe.FindAllStringSubmatch(where, -1) {
			fn, col := strings.ToLower(m[1]), strings.ToLower(m[2])
			if notFnNames[fn] || seen[fn+col] {
				continue
			}
			seen[fn+col] = true
			tips = append(tips, PlanTip{"warn", fmt.Sprintf("WHERE 中对列 %s 使用了函数 %s()，其上的索引无法使用；建议改写为对列本身的范围条件。", col, fn)})
		}
	}
	sig := sqlx.Significant(sqlx.Tokenize(stmt))
	for i := 0; i+1 < len(sig); i++ {
		if sig[i].Kind == sqlx.Word && sig[i].Lower() == "like" && sig[i+1].Kind == sqlx.String {
			if lit := sig[i+1].Text; len(lit) > 2 && (lit[1] == '%' || lit[1] == '_') {
				tips = append(tips, PlanTip{"warn", "LIKE 以通配符开头（前导 % 或 _），无法使用索引。"})
				break
			}
		}
	}
	for _, re := range []*regexp.Regexp{deepLimitA, deepLimitB} {
		if m := re.FindStringSubmatch(stmt); m != nil {
			if off, _ := strconv.Atoi(m[1]); off >= 10000 {
				tips = append(tips, PlanTip{"warn", fmt.Sprintf("深分页：偏移量 %d 很大，会先读出再丢弃前面的行；建议改用“上一页最后一条主键 > ?”的游标方式或延迟关联。", off)})
				break
			}
		}
	}
	return tips
}

// whereClause 取第一个 WHERE 到 GROUP BY / ORDER BY / LIMIT / HAVING 之间的文本（词法级，字符串里的关键字不算）。
func whereClause(stmt string) string {
	toks := sqlx.Tokenize(stmt)
	start, end := -1, len(stmt)
	depth := 0
	for _, t := range toks {
		switch {
		case t.Kind == sqlx.Punct && t.Text == "(":
			depth++
		case t.Kind == sqlx.Punct && t.Text == ")":
			depth--
		case t.Kind == sqlx.Word && start < 0 && t.Lower() == "where":
			start = t.End
		case t.Kind == sqlx.Word && start >= 0 && depth <= 0:
			switch t.Lower() {
			case "group", "order", "limit", "having", "union", "window":
				if t.Start > start && end == len(stmt) {
					end = t.Start
				}
			}
		}
	}
	if start < 0 || start > end {
		return ""
	}
	return stmt[start:end]
}
