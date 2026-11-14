// @author: kevinkelly
// @license: Apache Licence
// @file: advisor.go
// @time: 2026/10/28

package slowlog

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"dbtool/internal/sqlx"
)

// RuleAdvisor 是默认的 Advisor：只做词法级的静态规则分析，不连接目标库。
// 它能发现常见的写法问题（SELECT *、缺 WHERE、前导通配符、列上套函数、深分页等），
// 并在调用方提供了扫描/返回行数时提示“疑似缺索引”。结果只是线索，需要结合 EXPLAIN 与现有索引确认。
type RuleAdvisor struct{}

const ruleLabel = "规则分析（静态，不连接目标库）"

var levelRank = map[string]int{"crit": 0, "warn": 1, "info": 2}

func (RuleAdvisor) Advise(_ context.Context, req AdviseRequest) (*Advice, error) {
	stmt := firstStatement(req.SQL)
	if stmt == "" {
		return nil, badReq("SQL 为空")
	}
	var items []AdviceItem
	add := func(level, code, title, detail string) {
		items = append(items, AdviceItem{Level: level, Code: code, Title: title, Detail: detail})
	}

	sig := sqlx.Significant(sqlx.Tokenize(stmt))
	kw := sqlx.FirstKeyword(stmt)
	isDML := kw == "select" || kw == "update" || kw == "delete" || kw == "with"
	if !isDML && kw != "insert" && kw != "replace" {
		add("info", "not_dml", "不是常见的 DML 语句", "规则分析只针对 SELECT/UPDATE/DELETE/INSERT；其他语句请直接用 EXPLAIN 或 DBA 工具分析。")
	}

	whereAt := topLevelIndex(sig, "where")
	hasLimit := hasWord(sig, "limit")

	if sqlx.HasSelectStar(stmt) {
		add("warn", "select_star", "避免使用 SELECT *",
			"只取需要的列：可以减少网络与内存开销，也有机会走覆盖索引避免回表；表结构变更时 SELECT * 的结果也会变化。")
	}
	if whereAt < 0 {
		switch kw {
		case "update", "delete":
			add("crit", "no_where_write", "UPDATE/DELETE 没有 WHERE 条件",
				"会对全表逐行修改/删除，持有大量行锁并产生大事务与大量 binlog；请确认是否为预期，必要时分批执行。")
		case "select", "with":
			if hasWord(sig, "from") && !hasLimit {
				add("warn", "no_where", "没有 WHERE 条件且没有 LIMIT",
					"会全表扫描并返回全部数据；请增加过滤条件或分页。")
			}
		}
	}

	if leadingWildcardLike(sig) {
		add("warn", "leading_wildcard", "LIKE 以通配符开头",
			"形如 LIKE '%abc' 的条件无法使用 B+ 树索引，会扫描整个索引或全表；可考虑前缀匹配、全文索引或搜索引擎。")
	}
	for _, fn := range funcOnColumn(sig, whereAt) {
		add("warn", "func_on_column", fmt.Sprintf("WHERE 里对列使用了函数：%s()", fn),
			"在索引列上套函数或做运算会让索引失效，可以把函数挪到常量一侧（如把 DATE(col)=x 改写成范围条件 col>=x AND col<x+1）。")
	}
	if whereAt >= 0 && hasWord(sig[whereAt:], "or") {
		add("info", "or_condition", "WHERE 里有 OR 条件",
			"OR 的各分支不都有合适索引时会退化为全表扫描；可以改写为 UNION ALL 或 IN，或为各分支建立索引（让优化器走 index merge）。")
	}
	if negativeCondition(sig) {
		add("info", "negative_condition", "使用了否定条件（NOT IN / != / <> / NOT LIKE）",
			"否定条件通常不能有效利用索引，命中的行占比大时会全表扫描；可改写为 NOT EXISTS 或反向条件。")
	}
	if orderByRand(sig) {
		add("crit", "order_rand", "ORDER BY RAND()",
			"会对全部命中行生成随机数并排序（临时表+filesort），数据量大时极慢；可先取随机主键范围再查，或在应用层抽样。")
	} else if hasSeq(sig, "order", "by") && !hasLimit && (kw == "select" || kw == "with") {
		add("info", "order_no_limit", "有 ORDER BY 但没有 LIMIT",
			"需要对整个结果集排序；若业务只要前 N 条请加 LIMIT，并让排序列与过滤列共同组成索引以避免 filesort。")
	}
	if off, ok := limitOffset(sig); ok && off >= 10000 {
		add("warn", "deep_pagination", fmt.Sprintf("深分页：偏移量 %d", off),
			"MySQL 要先扫描并丢弃前 N 行；可改用“记住上一页最后的主键/排序值”的 keyset 方式（WHERE id > ? ORDER BY id LIMIT n），或先用覆盖索引取主键再回表（延迟关联）。")
	}
	if n := maxInListSize(sig); n > 200 {
		add("warn", "big_in_list", fmt.Sprintf("IN 列表过长（%d 项）", n),
			"超长 IN 列表会增加解析与优化开销，且可能放弃索引；可改为临时表/关联查询，或分批执行。")
	}
	if hasSeq(sig, "in", "(", "select") {
		add("info", "in_subquery", "IN 子查询",
			"在较老的 MySQL 版本上 IN 子查询可能被改写成相关子查询而变慢；可改写为 JOIN 或 EXISTS，并用 EXPLAIN 确认。")
	}
	if hasUnionDistinct(sig) {
		add("info", "union_distinct", "UNION 默认去重",
			"UNION 会建临时表去重；如果各分支结果本来不会重复，请使用 UNION ALL。")
	}

	// 统计信息：扫描行数远大于返回行数，是缺索引或索引失效的典型迹象。
	if req.RowsExamined >= 10000 {
		sent := req.RowsSent
		if sent < 1 {
			sent = 1
		}
		if ratio := req.RowsExamined / sent; ratio >= 100 {
			detail := fmt.Sprintf("这条慢日志扫描了 %d 行却只返回 %d 行（约 %d:1），疑似缺少合适的索引或索引没有被使用。", req.RowsExamined, req.RowsSent, ratio)
			if hint := indexHint(stmt, sig, whereAt); hint != "" {
				detail += hint
			}
			add("crit", "missing_index", "缺少索引的迹象", detail)
		}
	}
	if req.QueryTime >= 10 {
		add("info", "long_query", fmt.Sprintf("单次耗时 %.2f 秒", req.QueryTime),
			"耗时很长时除了索引，也检查是否存在锁等待（慢日志里的 Lock_time）、大结果集传输或统计信息过期（ANALYZE TABLE）。")
	}

	if req.QueryTime > 0 && req.LockTime >= 0.05 && req.LockTime/req.QueryTime >= 0.5 {
		add("warn", "lock_wait", fmt.Sprintf("耗时主要花在锁等待上：锁等待 %.2f 秒，占单次耗时的 %d%%", req.LockTime, int(req.LockTime/req.QueryTime*100+0.5)),
			"这类语句慢的不是查找而是在排队等锁（扫描行数不多时尤其明显）。排查同一行 / 同一范围上的长事务与热点行更新，缩短事务、避免事务内等待外部调用，多行更新按固定顺序加锁。")
	}
	if conds := joinConditions(sig); len(conds) > 0 {
		add("info", "join_index", "确认连接列上都有索引",
			fmt.Sprintf("连接条件：%s。被驱动表的连接列没有索引时，外层每一行都要扫描整张表（EXPLAIN 里会出现 Using join buffer）；两边的列类型、字符集、排序规则不一致也会让索引失效。请到“优化工具”看执行计划确认。", strings.Join(conds, "；")))
	}
	if len(items) == 0 {
		add("info", "no_findings", "没有发现明显的写法问题",
			"规则分析只能发现常见的静态问题；请结合 EXPLAIN、表的现有索引与数据分布进一步确认。")
	}
	sort.SliceStable(items, func(i, j int) bool { return levelRank[items[i].Level] < levelRank[items[j].Level] })
	adv := &Advice{Source: "rule", Label: ruleLabel, Items: items, Score: adviceScore(items)}
	if table, cols := indexCandidate(stmt, sig, whereAt); table != "" && len(cols) > 0 && (kw == "select" || kw == "update" || kw == "delete" || kw == "with") {
		adv.DDL = []string{candidateDDL(req.DB, table, cols)}
	}
	return adv, nil
}

// firstStatement 返回第一条语句（去注释、去分号）。
func firstStatement(sql string) string {
	parts := sqlx.Split(sql)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func hasWord(sig []sqlx.Token, w string) bool {
	for _, t := range sig {
		if t.Kind == sqlx.Word && t.Lower() == w {
			return true
		}
	}
	return false
}

// hasSeq 判断词序列（Word 按小写比较，Punct 按原文比较）是否出现。
func hasSeq(sig []sqlx.Token, words ...string) bool {
	for i := 0; i+len(words) <= len(sig); i++ {
		ok := true
		for j, w := range words {
			t := sig[i+j]
			if t.Kind == sqlx.Word {
				ok = t.Lower() == w
			} else if t.Kind == sqlx.Punct {
				ok = t.Text == w
			} else {
				ok = false
			}
			if !ok {
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// topLevelIndex 返回第一个括号深度为 0 的关键字的下标，没有返回 -1。
func topLevelIndex(sig []sqlx.Token, w string) int {
	depth := 0
	for i, t := range sig {
		switch {
		case t.Kind == sqlx.Punct && t.Text == "(":
			depth++
		case t.Kind == sqlx.Punct && t.Text == ")":
			depth--
		case depth == 0 && t.Kind == sqlx.Word && t.Lower() == w:
			return i
		}
	}
	return -1
}

func leadingWildcardLike(sig []sqlx.Token) bool {
	for i := 0; i+1 < len(sig); i++ {
		if sig[i].Kind == sqlx.Word && sig[i].Lower() == "like" && sig[i+1].Kind == sqlx.String {
			if lit := strings.TrimPrefix(sig[i+1].Text, "'"); strings.HasPrefix(lit, "%") || strings.HasPrefix(lit, "_") {
				return true
			}
		}
	}
	return false
}

// 常见的会让索引失效的函数。
var indexKillFuncs = map[string]bool{
	"date": true, "date_format": true, "year": true, "month": true, "day": true, "hour": true, "lower": true, "upper": true,
	"substring": true, "substr": true, "left": true, "right": true, "trim": true, "ifnull": true, "coalesce": true, "cast": true,
	"convert": true, "unix_timestamp": true, "from_unixtime": true, "datediff": true, "abs": true, "md5": true, "concat": true,
	"length": true, "char_length": true, "round": true, "floor": true, "ceil": true, "ceiling": true, "to_days": true,
}

// funcOnColumn 找出 WHERE 之后形如 fn(列…) 的调用（括号内第一个词是列名而不是常量/函数）。
func funcOnColumn(sig []sqlx.Token, whereAt int) []string {
	if whereAt < 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for i := whereAt; i+2 < len(sig); i++ {
		t := sig[i]
		if t.Kind != sqlx.Word || !indexKillFuncs[t.Lower()] || sig[i+1].Kind != sqlx.Punct || sig[i+1].Text != "(" {
			continue
		}
		a := sig[i+2]
		if a.Kind != sqlx.Word && a.Kind != sqlx.Quoted {
			continue
		}
		if i+3 < len(sig) && sig[i+3].Kind == sqlx.Punct && sig[i+3].Text == "(" {
			continue // 内层又是函数调用，如 date(now())
		}
		if a.Kind == sqlx.Word && sqlKeywords[a.Lower()] {
			continue
		}
		if name := t.Lower(); !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func negativeCondition(sig []sqlx.Token) bool {
	for i, t := range sig {
		switch {
		case t.Kind == sqlx.Punct && t.Text == "!" && i+1 < len(sig) && sig[i+1].Text == "=":
			return true
		case t.Kind == sqlx.Punct && t.Text == "<" && i+1 < len(sig) && sig[i+1].Text == ">":
			return true
		case t.Kind == sqlx.Word && t.Lower() == "not" && i+1 < len(sig) && sig[i+1].Kind == sqlx.Word:
			switch sig[i+1].Lower() {
			case "in", "like", "exists", "between":
				return true
			}
		}
	}
	return false
}

func orderByRand(sig []sqlx.Token) bool {
	for i := 0; i+2 < len(sig); i++ {
		if sig[i].Kind == sqlx.Word && sig[i].Lower() == "order" && sig[i+1].Kind == sqlx.Word && sig[i+1].Lower() == "by" {
			for j := i + 2; j+1 < len(sig) && j < i+8; j++ {
				if sig[j].Kind == sqlx.Word && sig[j].Lower() == "rand" && sig[j+1].Text == "(" {
					return true
				}
			}
		}
	}
	return false
}

// limitOffset 解析 LIMIT a,b / LIMIT b OFFSET a 里的偏移量；参数是 ? 占位符时返回 false。
func limitOffset(sig []sqlx.Token) (int64, bool) {
	for i := 0; i+1 < len(sig); i++ {
		if sig[i].Kind != sqlx.Word || sig[i].Lower() != "limit" || sig[i+1].Kind != sqlx.Number {
			continue
		}
		first, err := strconv.ParseInt(strings.TrimSuffix(sig[i+1].Text, "."), 10, 64)
		if err != nil {
			continue
		}
		if i+3 < len(sig) && sig[i+2].Kind == sqlx.Punct && sig[i+2].Text == "," && sig[i+3].Kind == sqlx.Number {
			return first, true
		}
		if i+3 < len(sig) && sig[i+2].Kind == sqlx.Word && sig[i+2].Lower() == "offset" && sig[i+3].Kind == sqlx.Number {
			if off, err := strconv.ParseInt(strings.TrimSuffix(sig[i+3].Text, "."), 10, 64); err == nil {
				return off, true
			}
		}
	}
	return 0, false
}

// maxInListSize 返回最长的 IN (…) 列表项数。
func maxInListSize(sig []sqlx.Token) int {
	best := 0
	for i := 0; i+1 < len(sig); i++ {
		if sig[i].Kind != sqlx.Word || sig[i].Lower() != "in" || sig[i+1].Text != "(" || sig[i+1].Kind != sqlx.Punct {
			continue
		}
		depth, n := 0, 1
		for j := i + 1; j < len(sig); j++ {
			t := sig[j]
			if t.Kind == sqlx.Punct {
				switch t.Text {
				case "(":
					depth++
				case ")":
					depth--
				case ",":
					if depth == 1 {
						n++
					}
				}
			}
			if depth == 0 {
				break
			}
		}
		if n > best {
			best = n
		}
	}
	return best
}

func hasUnionDistinct(sig []sqlx.Token) bool {
	for i, t := range sig {
		if t.Kind == sqlx.Word && t.Lower() == "union" {
			if i+1 >= len(sig) || sig[i+1].Kind != sqlx.Word || sig[i+1].Lower() != "all" {
				return true
			}
		}
	}
	return false
}

var sqlKeywords = map[string]bool{
	"and": true, "or": true, "not": true, "is": true, "null": true, "like": true, "in": true, "between": true, "exists": true,
	"true": true, "false": true, "where": true, "select": true, "case": true, "when": true, "then": true, "else": true,
	"end": true, "interval": true, "from": true, "on": true, "by": true, "as": true, "distinct": true, "binary": true,
}

// indexHint 从主表与 WHERE 条件里推测一个候选复合索引：等值列在前、范围列在后（文字说明）。
func indexHint(stmt string, sig []sqlx.Token, whereAt int) string {
	table, cols := indexCandidate(stmt, sig, whereAt)
	if table == "" || len(cols) == 0 {
		return ""
	}
	return fmt.Sprintf("可以先评估在表 %s 上建立复合索引 (%s)（等值条件列在前、范围条件列在后），并确认列的区分度与现有索引；该建议仅由 WHERE 条件推测，请以 EXPLAIN 为准。",
		table, strings.Join(cols, ", "))
}

// indexCandidate 从主表与 WHERE 条件里推测候选复合索引的表和列：等值列在前、范围列在后，最多 4 列。
// 只在语句只涉及一张表时给出（多表时无法确定列属于哪张表）。
func indexCandidate(stmt string, sig []sqlx.Token, whereAt int) (string, []string) {
	if whereAt < 0 {
		return "", nil
	}
	tables := sqlx.ExtractTables(stmt)
	if len(tables) != 1 {
		return "", nil
	}
	var eq, rng []string
	seen := map[string]bool{}
	depth := 0
	for i := whereAt + 1; i < len(sig); i++ {
		t := sig[i]
		if t.Kind == sqlx.Punct {
			switch t.Text {
			case "(":
				depth++
			case ")":
				depth--
			}
		}
		if depth != 0 {
			continue
		}
		if t.Kind == sqlx.Word {
			switch t.Lower() {
			case "group", "order", "limit", "having", "union", "for", "lock":
				i = len(sig) // 结束 WHERE 区域
				continue
			}
		}
		if (t.Kind != sqlx.Word && t.Kind != sqlx.Quoted) || (t.Kind == sqlx.Word && sqlKeywords[t.Lower()]) {
			continue
		}
		j := i + 1
		col := strings.Trim(t.Text, "`\"")
		if j+1 < len(sig) && sig[j].Kind == sqlx.Punct && sig[j].Text == "." && (sig[j+1].Kind == sqlx.Word || sig[j+1].Kind == sqlx.Quoted) {
			col = strings.Trim(sig[j+1].Text, "`\"")
			j += 2
		}
		if j >= len(sig) || sig[j].Text == "(" || seen[strings.ToLower(col)] {
			continue
		}
		op := sig[j]
		isEq, isRange := false, false
		switch {
		case op.Kind == sqlx.Punct && op.Text == "=":
			isEq = true
		case op.Kind == sqlx.Word && op.Lower() == "in":
			isEq = !(j+2 < len(sig) && sig[j+1].Text == "(" && sig[j+2].Lower() == "select")
		case op.Kind == sqlx.Punct && (op.Text == "<" || op.Text == ">"):
			isRange = !(j+1 < len(sig) && sig[j+1].Text == ">") // 排除 <>
		case op.Kind == sqlx.Word && op.Lower() == "between":
			isRange = true
		}
		switch {
		case isEq:
			seen[strings.ToLower(col)] = true
			eq = append(eq, col)
		case isRange:
			seen[strings.ToLower(col)] = true
			rng = append(rng, col)
		}
	}
	cols := append(eq, rng...)
	if len(cols) == 0 {
		return "", nil
	}
	if len(cols) > 4 {
		cols = cols[:4]
	}
	return tables[0].Name, cols
}

func quoteID(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// candidateDDL 把候选索引写成 ALTER 语句；db 为空时不带库名。
func candidateDDL(db, table string, cols []string) string {
	qs := make([]string, len(cols))
	for i, c := range cols {
		qs[i] = quoteID(c)
	}
	name := "idx_" + strings.ToLower(strings.Join(cols, "_"))
	if len(name) > 64 {
		name = name[:64]
	}
	tbl := quoteID(table)
	if db != "" {
		tbl = quoteID(db) + "." + tbl
	}
	return fmt.Sprintf("ALTER TABLE %s ADD INDEX %s (%s);", tbl, quoteID(name), strings.Join(qs, ", "))
}

// adviceScore 综合评分：100 分起，严重 -25、注意 -10、提示 -3，最低 0；“没有发现问题 / 不是 DML”这类说明性条目不扣分。
func adviceScore(items []AdviceItem) int {
	s := 100
	for _, it := range items {
		if it.Code == "no_findings" || it.Code == "not_dml" {
			continue
		}
		switch it.Level {
		case "crit":
			s -= 25
		case "warn":
			s -= 10
		case "info":
			s -= 3
		}
	}
	if s < 0 {
		s = 0
	}
	return s
}

// joinConditions 找出 JOIN ... ON 里“列 = 列”形式的连接条件（最多 6 个），写成 "a.x = b.y"。
func joinConditions(sig []sqlx.Token) []string {
	var out []string
	seen := map[string]bool{}
	stop := map[string]bool{"where": true, "join": true, "left": true, "right": true, "inner": true, "cross": true, "natural": true,
		"straight_join": true, "group": true, "order": true, "limit": true, "having": true, "union": true, "for": true, "lock": true}
	for i := 0; i < len(sig) && len(out) < 6; i++ {
		if sig[i].Kind != sqlx.Word || sig[i].Lower() != "on" || (i+1 < len(sig) && sig[i+1].Lower() == "duplicate") {
			continue
		}
		for j := i + 1; j < len(sig) && len(out) < 6; j++ {
			if sig[j].Kind == sqlx.Word && stop[sig[j].Lower()] {
				break
			}
			l, n := colRef(sig, j)
			if n == 0 || j+n >= len(sig) || sig[j+n].Kind != sqlx.Punct || sig[j+n].Text != "=" {
				continue
			}
			r, m := colRef(sig, j+n+1)
			if m == 0 {
				continue
			}
			if c := l + " = " + r; !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
			j += n + m
		}
	}
	return out
}

// colRef 从 i 处读一个列引用（可带限定名 t.col），返回文本与占用的词数；不是列引用（常量、函数调用、关键字）返回 0。
func colRef(sig []sqlx.Token, i int) (string, int) {
	if i >= len(sig) {
		return "", 0
	}
	ident := func(t sqlx.Token) bool {
		return (t.Kind == sqlx.Word && !sqlKeywords[t.Lower()]) || t.Kind == sqlx.Quoted
	}
	if !ident(sig[i]) {
		return "", 0
	}
	name, n := strings.Trim(sig[i].Text, "`\""), 1
	if i+2 < len(sig) && sig[i+1].Kind == sqlx.Punct && sig[i+1].Text == "." && ident(sig[i+2]) {
		name += "." + strings.Trim(sig[i+2].Text, "`\"")
		n = 3
	}
	if i+n < len(sig) && sig[i+n].Kind == sqlx.Punct && sig[i+n].Text == "(" { // 函数调用
		return "", 0
	}
	return name, n
}
