// @author: kevinkelly
// @license: Apache Licence
// @file: rules.go
// @time: 2026/10/28

package sqlopt

import (
	"fmt"
	"strconv"
	"strings"

	"dbtool/internal/sqlx"
)

// Finding 是一条优化建议。
type Finding struct {
	ID         string `json:"id"`
	Level      string `json:"level"` // high / medium / low / info
	Title      string `json:"title"`
	Detail     string `json:"detail,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

// 建议级别。
const (
	LevelHigh   = "high"
	LevelMedium = "medium"
	LevelLow    = "low"
	LevelInfo   = "info"
)

var levelRank = map[string]int{LevelHigh: 3, LevelMedium: 2, LevelLow: 1, LevelInfo: 0}

// highestLevel 返回一组建议里最高的级别，没有返回空串。
func highestLevel(fs []Finding) string {
	best, rank := "", -1
	for _, f := range fs {
		if r := levelRank[f.Level]; r > rank {
			best, rank = f.Level, r
		}
	}
	return best
}

// scoreOf 综合评分：100 分起，高 -20、中 -10、低 -3，最低 0。
func scoreOf(fs []Finding) int {
	s := 100
	for _, f := range fs {
		switch f.Level {
		case LevelHigh:
			s -= 20
		case LevelMedium:
			s -= 10
		case LevelLow:
			s -= 3
		}
	}
	if s < 0 {
		s = 0
	}
	return s
}

// RuleInfo 描述一条内置规则（给页面做说明）。
type RuleInfo struct {
	ID, Level, Title string
}

// StaticRules 是不需要连接数据库的内置规则清单；顺序即输出顺序。
var StaticRules = []RuleInfo{
	{"R001", LevelLow, "SELECT * 读取所有列"},
	{"R002", LevelHigh, "UPDATE/DELETE 没有 WHERE 条件"},
	{"R003", LevelMedium, "WHERE/ON 条件里对列使用函数"},
	{"R004", LevelMedium, "LIKE 以通配符开头"},
	{"R005", LevelMedium, "深分页（LIMIT 偏移量过大）"},
	{"R006", LevelHigh, "ORDER BY RAND()"},
	{"R007", LevelLow, "WHERE 中使用 OR"},
	{"R008", LevelLow, "否定条件（NOT IN / != / <> / NOT LIKE）"},
	{"R009", LevelLow, "IN 子查询"},
	{"R010", LevelMedium, "IN 列表过长"},
	{"R011", LevelLow, "INSERT 没有列清单"},
	{"R012", LevelHigh, "JOIN 缺少连接条件（笛卡尔积）"},
	{"R013", LevelLow, "UNION 未指定 ALL"},
	{"R014", LevelMedium, "与 NULL 用 = / != 比较"},
	{"R015", LevelMedium, "SELECT 没有 WHERE 也没有 LIMIT"},
	{"R016", LevelLow, "LIMIT 没有配合 ORDER BY"},
	{"S001", LevelMedium, "字符串列与数字比较，发生隐式类型转换（需要实例）"},
	{"S002", LevelMedium, "查询条件没有可用的索引（需要实例；给出 ADD INDEX 建议）"},
	{"P001", LevelHigh, "执行计划：全表扫描（需要实例）"},
	{"P002", LevelMedium, "执行计划：全索引扫描"},
	{"P003", LevelMedium, "执行计划：Using filesort"},
	{"P004", LevelMedium, "执行计划：Using temporary"},
	{"P005", LevelMedium, "执行计划：有候选索引但未使用"},
	{"P006", LevelHigh, "执行计划：Using join buffer（被驱动表无索引）"},
	{"P007", LevelHigh, "执行计划：相关子查询（DEPENDENT SUBQUERY）"},
	{"P008", LevelHigh, "执行计划：Range checked for each record"},
	{"P009", LevelLow, "执行计划：大量扫描后过滤掉绝大部分行"},
	{"P010", LevelInfo, "执行计划：优化器判定 WHERE 条件不可能成立"},
}

// StaticFindings 对一条语句运行不依赖数据库的规则。
func StaticFindings(sql string) []Finding {
	p := parseStmt(sql)
	var out []Finding
	for _, r := range staticRules {
		out = append(out, r(p)...)
	}
	return out
}

var staticRules = []func(*pstmt) []Finding{
	ruleSelectStar, ruleNoWhereDML, ruleFuncOnColumn, ruleLeadingWildcard, ruleDeepPaging, ruleOrderByRand,
	ruleOrCond, ruleNegative, ruleInSubquery, ruleBigIn, ruleInsertNoColumns, ruleCartesian, ruleUnionDistinct,
	ruleNullCompare, ruleSelectNoWhere, ruleLimitNoOrder,
}

func ruleSelectStar(p *pstmt) []Finding {
	if p.kind != TypeSelect && p.kind != TypeInsert && p.kind != TypeReplace || !sqlx.HasSelectStar(p.sql) {
		return nil
	}
	return []Finding{{ID: "R001", Level: LevelLow, Title: "不建议使用 SELECT *",
		Detail:     "读取所有列会增加网络与内存开销，也让覆盖索引无法生效，表结构变化时还可能让程序出错。",
		Suggestion: "只列出需要的列。"}}
}

func ruleNoWhereDML(p *pstmt) []Finding {
	if (p.kind == TypeUpdate || p.kind == TypeDelete) && p.top("where") < 0 {
		return []Finding{{ID: "R002", Level: LevelHigh, Title: "UPDATE/DELETE 没有 WHERE 条件",
			Detail:     "语句会修改或删除整张表的数据，执行时间长且会长时间持锁。",
			Suggestion: "确认确实要处理全表；否则补上 WHERE 条件，大批量变更请分批执行（LIMIT + 循环）。"}}
	}
	return nil
}

// 条件子句里包在列外面会让索引失效的常见函数。
var indexKillerFuncs = map[string]bool{
	"date": true, "year": true, "month": true, "day": true, "dayofmonth": true, "hour": true, "minute": true, "second": true,
	"date_format": true, "date_add": true, "date_sub": true, "adddate": true, "subdate": true, "datediff": true,
	"timestampdiff": true, "to_days": true, "from_days": true, "unix_timestamp": true, "from_unixtime": true, "time": true,
	"str_to_date": true, "lower": true, "lcase": true, "upper": true, "ucase": true, "substring": true, "substr": true,
	"left": true, "right": true, "concat": true, "concat_ws": true, "trim": true, "ltrim": true, "rtrim": true,
	"replace": true, "length": true, "char_length": true, "abs": true, "round": true, "floor": true, "ceil": true,
	"ceiling": true, "cast": true, "convert": true, "ifnull": true, "coalesce": true, "md5": true, "sha1": true,
	"sha2": true, "reverse": true, "if": true, "mod": true, "json_extract": true, "hex": true, "unhex": true,
}

var condStop = map[string]bool{"group": true, "order": true, "limit": true, "union": true, "select": true, "from": true,
	"join": true, "set": true, "window": true, "for": true, "lock": true, "into": true, "values": true}

// condMask 标记哪些词位于 WHERE / ON / HAVING 条件里（任意括号深度）。
func (p *pstmt) condMask() []bool {
	m := make([]bool, len(p.sig))
	in := false
	for i := range p.sig {
		switch w := p.w(i); {
		case w == "where" || w == "on" || w == "having":
			in = true
		case condStop[w]:
			in = false
		default:
			m[i] = in
		}
	}
	return m
}

func isCompareAt(p *pstmt, i int) bool {
	if i >= len(p.sig) {
		return false
	}
	t := p.sig[i]
	if t.Kind == sqlx.Punct {
		return t.Text == "=" || t.Text == "<" || t.Text == ">" || t.Text == "!"
	}
	switch p.w(i) {
	case "in", "like", "between", "is", "not", "regexp", "rlike":
		return true
	}
	return false
}

func ruleFuncOnColumn(p *pstmt) []Finding {
	mask := p.condMask()
	var out []Finding
	seen := map[string]bool{}
	for i := range p.sig {
		if !mask[i] || !indexKillerFuncs[p.w(i)] || !p.punct(i+1, "(") {
			continue
		}
		switch prev := p.w(i - 1); {
		case prev == "where" || prev == "and" || prev == "or" || prev == "on" || prev == "not" || prev == "having" || p.punct(i-1, "("):
		default:
			continue
		}
		j := p.matchParen(i + 1)
		if j < 0 || !isCompareAt(p, j+1) {
			continue
		}
		hasCol := false
		for k := i + 2; k < j; k++ {
			if _, _, ok := p.columnAt(k); ok {
				hasCol = true
				break
			}
		}
		if !hasCol {
			continue
		}
		expr := truncRunes(p.text(i, j), 60)
		if seen[expr] {
			continue
		}
		seen[expr] = true
		out = append(out, Finding{ID: "R003", Level: LevelMedium, Title: fmt.Sprintf("条件里对列使用函数：%s", expr),
			Detail:     "对列做函数运算后，该列上的索引无法用于范围/等值查找，会退化成全表扫描。",
			Suggestion: "把函数挪到常量一侧，例如 date(c) = '2026-10-05' 改成 c >= '2026-10-05' AND c < '2026-10-06'。"})
	}
	return out
}

func ruleLeadingWildcard(p *pstmt) []Finding {
	for i := range p.sig {
		if p.w(i) != "like" {
			continue
		}
		j := i + 1
		if p.w(j) == "concat" && p.punct(j+1, "(") {
			j += 2
		}
		if j < len(p.sig) && p.sig[j].Kind == sqlx.String && len(p.sig[j].Text) > 1 {
			if c := p.sig[j].Text[1]; c == '%' || c == '_' {
				return []Finding{{ID: "R004", Level: LevelMedium, Title: fmt.Sprintf("LIKE 以通配符开头：%s", truncRunes(p.sig[j].Text, 40)),
					Detail:     "以 % 或 _ 开头的模糊匹配无法使用 B+ 树索引，需要扫描全表。",
					Suggestion: "尽量改成前缀匹配（'abc%'）；确需全文检索时考虑全文索引或搜索引擎。"}}
			}
		}
	}
	return nil
}

func ruleDeepPaging(p *pstmt) []Finding {
	i := p.top("limit")
	if i < 0 {
		return nil
	}
	var offset int64
	n := func(k int) (int64, bool) {
		if k < len(p.sig) && p.sig[k].Kind == sqlx.Number {
			v, err := strconv.ParseInt(p.sig[k].Text, 10, 64)
			return v, err == nil
		}
		return 0, false
	}
	if a, ok := n(i + 1); ok && p.punct(i+2, ",") {
		offset = a
	} else if _, ok := n(i + 1); ok && p.w(i+2) == "offset" {
		offset, _ = n(i + 3)
	}
	if offset < 10000 {
		return nil
	}
	lv := LevelMedium
	if offset >= 100000 {
		lv = LevelHigh
	}
	return []Finding{{ID: "R005", Level: lv, Title: fmt.Sprintf("深分页：LIMIT 偏移量 %d", offset),
		Detail:     "MySQL 要先读出并丢弃偏移量之前的所有行，页越靠后越慢。",
		Suggestion: "改用“上一页最后一条的主键/排序值”做游标（WHERE id > ? ORDER BY id LIMIT n），或先查主键再延迟关联回表。"}}
}

func ruleOrderByRand(p *pstmt) []Finding {
	o := p.top("order")
	if o < 0 {
		return nil
	}
	end := p.endOfClause(o, 0, map[string]bool{"limit": true, "for": true, "lock": true, "union": true})
	for i := o; i < end; i++ {
		if p.w(i) == "rand" && p.punct(i+1, "(") {
			return []Finding{{ID: "R006", Level: LevelHigh, Title: "ORDER BY RAND()",
				Detail:     "会给每一行生成随机数并对全部结果排序，数据量大时极慢。",
				Suggestion: "在应用里生成随机主键范围，或用 WHERE id >= (随机值) LIMIT n 的方式取样。"}}
		}
	}
	return nil
}

func ruleOrCond(p *pstmt) []Finding {
	w := p.top("where")
	if w < 0 {
		return nil
	}
	end := p.endOfClause(w+1, 0, whereStop)
	if p.find(w+1, end, 0, "or") >= 0 {
		return []Finding{{ID: "R007", Level: LevelLow, Title: "WHERE 中使用 OR",
			Detail:     "OR 连接不同列的条件时，优化器可能放弃索引或只能做索引合并。",
			Suggestion: "同一列的 OR 改成 IN；不同列的可以拆成 UNION ALL 分别走索引（注意去重语义）。"}}
	}
	return nil
}

func ruleNegative(p *pstmt) []Finding {
	mask := p.condMask()
	for i := range p.sig {
		if !mask[i] {
			continue
		}
		neg := ""
		switch {
		case p.punct(i, "!") && p.punct(i+1, "=") && p.sig[i].End == p.sig[i+1].Start:
			neg = "!="
		case p.punct(i, "<") && p.punct(i+1, ">") && p.sig[i].End == p.sig[i+1].Start:
			neg = "<>"
		case p.w(i) == "not" && (p.w(i+1) == "in" || p.w(i+1) == "like"):
			neg = "NOT " + strings.ToUpper(p.w(i+1))
		}
		if neg != "" {
			return []Finding{{ID: "R008", Level: LevelLow, Title: fmt.Sprintf("否定条件：%s", neg),
				Detail:     "否定条件通常无法使用索引的有序性，需要扫描大部分数据。",
				Suggestion: "尽量改写成正向条件（例如枚举值用 IN），或确认该条件前面已有高选择性的等值条件。"}}
		}
	}
	return nil
}

func ruleInSubquery(p *pstmt) []Finding {
	for i := range p.sig {
		if p.w(i) == "in" && p.punct(i+1, "(") && p.w(i+2) == "select" {
			return []Finding{{ID: "R009", Level: LevelLow, Title: "IN 子查询",
				Detail:     "MySQL 5.5 及更早版本会把它当作相关子查询逐行执行；5.6+ 虽有半连接优化，但在复杂语句里仍可能退化。",
				Suggestion: "改写成 JOIN 或 EXISTS，并确认子查询的连接列上有索引。"}}
		}
	}
	return nil
}

func ruleBigIn(p *pstmt) []Finding {
	for i := range p.sig {
		if p.w(i) != "in" || !p.punct(i+1, "(") || p.w(i+2) == "select" {
			continue
		}
		j := p.matchParen(i + 1)
		if j < 0 {
			continue
		}
		cnt := 0
		for k := i + 2; k < j; k++ {
			if p.depth[k] == p.depth[i]+1 && p.punct(k, ",") {
				cnt++
			}
		}
		if cnt+1 >= 200 {
			return []Finding{{ID: "R010", Level: LevelMedium, Title: fmt.Sprintf("IN 列表过长（%d 项）", cnt+1),
				Detail:     "过长的 IN 列表会增加解析与优化开销，超过 eq_range_index_dive_limit 后还会改用不准的统计信息估算。",
				Suggestion: "拆成多批查询，或把这批值放进临时表后 JOIN。"}}
		}
	}
	return nil
}

func ruleInsertNoColumns(p *pstmt) []Finding {
	if p.kind != TypeInsert && p.kind != TypeReplace {
		return nil
	}
	i := p.top("into")
	if i < 0 {
		return nil
	}
	i++
	if !p.isIdent(i) {
		return nil
	}
	i++
	if p.punct(i, ".") {
		i += 2
	}
	switch p.w(i) {
	case "values", "value", "select":
		return []Finding{{ID: "R011", Level: LevelLow, Title: "INSERT 没有指定列清单",
			Detail:     "表结构增减列后，语句会报错或把数据写错位置。",
			Suggestion: "写明列名：INSERT INTO t (a, b, c) VALUES (...)。"}}
	}
	return nil
}

var joinEnd = map[string]bool{"join": true, "where": true, "group": true, "order": true, "limit": true, "union": true,
	"having": true, "set": true, "for": true, "straight_join": true, "lock": true}

func ruleCartesian(p *pstmt) []Finding {
	if p.kind != TypeSelect && p.kind != TypeUpdate && p.kind != TypeDelete {
		return nil
	}
	for i := range p.sig {
		if p.w(i) != "join" {
			continue
		}
		switch p.w(i - 1) {
		case "cross", "natural":
			continue
		}
		// 向后找同层的 ON / USING，遇到下一个 JOIN / WHERE 等或括号闭合还没找到就是缺条件
		d := p.depth[i]
		found := false
		for k := i + 1; k < len(p.sig); k++ {
			if p.depth[k] < d {
				break
			}
			if p.depth[k] != d {
				continue
			}
			w := p.w(k)
			if w == "on" || w == "using" {
				found = true
				break
			}
			if joinEnd[w] || (w == "left" || w == "right" || w == "inner" || w == "cross" || w == "natural") && p.w(k+1) != "" {
				break
			}
		}
		if !found {
			return []Finding{{ID: "R012", Level: LevelHigh, Title: "JOIN 缺少 ON / USING 连接条件",
				Detail:     "没有连接条件的 JOIN 产生笛卡尔积，结果行数是两表行数的乘积。",
				Suggestion: "补上 ON 连接条件，并确认连接列上有索引。"}}
		}
	}
	// 逗号连接多表且没有 WHERE
	if p.kind == TypeSelect {
		if f := p.top("from"); f >= 0 && p.top("where") < 0 {
			end := p.endOfClause(f+1, 0, whereStop)
			for k := f + 1; k < end; k++ {
				if p.depth[k] == 0 && p.punct(k, ",") {
					return []Finding{{ID: "R012", Level: LevelHigh, Title: "多表逗号连接且没有 WHERE 条件",
						Detail: "没有连接条件会产生笛卡尔积。", Suggestion: "改用 JOIN ... ON 并写明连接条件。"}}
				}
			}
		}
	}
	return nil
}

func ruleUnionDistinct(p *pstmt) []Finding {
	for i := range p.sig {
		if p.w(i) == "union" && p.w(i+1) != "all" && p.w(i+1) != "distinct" {
			return []Finding{{ID: "R013", Level: LevelLow, Title: "UNION 未指定 ALL",
				Detail:     "UNION 默认要对结果去重，需要建临时表并排序/哈希。",
				Suggestion: "确认各部分结果不会重复或允许重复时，改用 UNION ALL。"}}
		}
	}
	return nil
}

func ruleNullCompare(p *pstmt) []Finding {
	mask := p.condMask()
	for i := range p.sig {
		if !mask[i] || !p.punct(i, "=") || p.w(i+1) != "null" {
			continue
		}
		if prev := i - 1; prev >= 0 && p.sig[prev].Kind == sqlx.Punct && (p.sig[prev].Text == "<" || p.sig[prev].Text == ">") && p.sig[prev].End == p.sig[i].Start {
			continue // <=> null 是合法的空安全比较
		}
		return []Finding{{ID: "R014", Level: LevelMedium, Title: "与 NULL 使用 = / != 比较",
			Detail:     "任何值与 NULL 用 =、!= 比较的结果都是 NULL（视为假），条件永远不成立。",
			Suggestion: "使用 IS NULL / IS NOT NULL，或空安全比较 <=>。"}}
	}
	for i := range p.sig {
		if mask[i] && p.punct(i, "!") && p.punct(i+1, "=") && p.w(i+2) == "null" {
			return []Finding{{ID: "R014", Level: LevelMedium, Title: "与 NULL 使用 = / != 比较",
				Detail:     "任何值与 NULL 用 =、!= 比较的结果都是 NULL（视为假），条件永远不成立。",
				Suggestion: "使用 IS NULL / IS NOT NULL，或空安全比较 <=>。"}}
		}
	}
	return nil
}

var aggFuncs = map[string]bool{"count": true, "sum": true, "avg": true, "min": true, "max": true, "group_concat": true}

func ruleSelectNoWhere(p *pstmt) []Finding {
	if p.kind != TypeSelect {
		return nil
	}
	f := p.top("from")
	if f < 0 || p.top("where") >= 0 || p.top("limit") >= 0 || p.top("group") >= 0 || p.top("having") >= 0 {
		return nil
	}
	if p.isIdent(f+1) && strings.EqualFold(ident(p.sig[f+1]), "dual") {
		return nil
	}
	for i := 0; i < f; i++ {
		if aggFuncs[p.w(i)] && p.punct(i+1, "(") {
			return nil
		}
	}
	return []Finding{{ID: "R015", Level: LevelMedium, Title: "SELECT 没有 WHERE 条件也没有 LIMIT",
		Detail:     "会读取并返回整张表（或整个连接结果）的全部数据，表大时耗时长、占用大量网络和内存。",
		Suggestion: "补上过滤条件，或加 LIMIT 限制返回行数。"}}
}

func ruleLimitNoOrder(p *pstmt) []Finding {
	if p.kind != TypeSelect {
		return nil
	}
	l := p.top("limit")
	if l < 0 || p.top("from") < 0 || p.top("order") >= 0 || p.top("group") >= 0 {
		return nil
	}
	if l+1 >= len(p.sig) || p.sig[l+1].Kind == sqlx.Number && p.sig[l+1].Text == "1" && !p.punct(l+2, ",") {
		return nil // LIMIT 1 常用于“是否存在”
	}
	return []Finding{{ID: "R016", Level: LevelLow, Title: "LIMIT 没有配合 ORDER BY",
		Detail:     "没有排序时返回哪些行是不确定的，分页还可能出现重复或遗漏。",
		Suggestion: "加上唯一且稳定的排序列（如主键）。"}}
}
