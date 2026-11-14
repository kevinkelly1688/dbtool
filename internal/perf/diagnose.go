// @author: kevinkelly
// @license: Apache Licence
// @file: diagnose.go
// @time: 2026/10/28

package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dbtool/internal/model"
)

// 自动诊断：文本规则 + 指标规则 + EXPLAIN（MySQL 系）。
// 目标不是替代 DBA，而是把“缺索引（DBA 自己能搞定）”和“必须改代码（研发处理）”分开，
// 并给研发一段看得懂、可执行的建议。

// Explainer 对目标库执行 EXPLAIN（真实实现见 collectors.go，单测用假实现）。
type Explainer interface {
	// Explain 返回执行计划（列名小写）与 SHOW WARNINGS 的文本。
	Explain(ctx context.Context, inst *model.Instance, db, sql string, timeoutMs int) (*ExplainResult, error)
}

// ExplainResult 是一次 EXPLAIN 的结果。
type ExplainResult struct {
	Plan     []map[string]any
	Warnings []string
}

// 属于 MySQL 系（能做 EXPLAIN）与分析型的产品。
var (
	mysqlLike = map[string]bool{ProdMySQL: true, ProdAliyun: true, ProdPolarDB: true, ProdAWS: true}
	olapLike  = map[string]bool{ProdSelectDB: true, ProdADB: true}
)

// categoryPriority 类别优先级（靠前的优先作为主类别）。
var categoryPriority = []string{CatIndexInvalid, CatDeepPaging, CatMissingIndex, CatSubqueryJoin, CatSortGroup, CatLargeResult, CatFullScanBiz, CatOLAPHeavy, CatHighFrequency, CatOther}

var (
	reFuncOnCol  = regexp.MustCompile("(?i)\\b(where|and|or|on)\\s+\\(?\\s*(date|date_format|substr|substring|left|right|lower|upper|ifnull|coalesce|cast|convert|concat|year|month|day|from_unixtime|unix_timestamp|trim|md5)\\s*\\(\\s*`?[a-z_][\\w.`]*")
	reLeadLike   = regexp.MustCompile(`(?i)\blike\s+(['"])%`)
	reLimitOff   = regexp.MustCompile(`(?i)\blimit\s+(\d+)\s*,\s*\d+|\blimit\s+\d+\s+offset\s+(\d+)`)
	reOrderRand  = regexp.MustCompile(`(?i)order\s+by\s+rand\s*\(`)
	reInValues   = regexp.MustCompile(`(?i)\bin\s*\(([^()]*)\)`)
	reOrCond     = regexp.MustCompile(`(?is)\bwhere\b.*\bor\b`)
	reNotCond    = regexp.MustCompile(`(?i)(!=|<>|\bnot\s+in\b|\bnot\s+like\b)`)
	reSelectStar = regexp.MustCompile(`(?i)^\s*select\s+\*`)
	reHasWhere   = regexp.MustCompile(`(?i)\bwhere\b`)
	reHasLimit   = regexp.MustCompile(`(?i)\blimit\b`)
	reHasFrom    = regexp.MustCompile(`(?i)\bfrom\b`)
	reFromDual   = regexp.MustCompile(`(?i)\bfrom\s+dual\b`)
	reIsSelect   = regexp.MustCompile(`(?i)^\s*select\b`)
	reColCond    = regexp.MustCompile("(?i)`?(?:(\\w+)`?\\.)?`?(\\w+)`?\\s*(=|<=>|>=|<=|>|<|\\bin\\b|\\bbetween\\b|\\blike\\s+['\"][^%])")
	reWhereBody  = regexp.MustCompile(`(?is)\bwhere\b(.*?)(\bgroup\s+by\b|\border\s+by\b|\blimit\b|\bhaving\b|\bfor\s+update\b|$)`)
	reInSubquery = regexp.MustCompile(`(?i)\b(not\s+)?(in|exists)\s*\(\s*select\b`)
)

var sqlKeywords = map[string]bool{"and": true, "or": true, "not": true, "is": true, "null": true, "select": true, "where": true, "from": true, "in": true, "like": true, "between": true}

// diag 是一次诊断的累积结果。
type diag struct {
	categories  []string
	findings    []string
	suggestions []string
	ddl         string
	plan        []map[string]any
}

func (d *diag) hit(category, finding, suggestion string) {
	if !contains(d.categories, category) {
		d.categories = append(d.categories, category)
	}
	d.findings = append(d.findings, finding)
	if suggestion != "" && !contains(d.suggestions, suggestion) {
		d.suggestions = append(d.suggestions, suggestion)
	}
}

func (d *diag) suggest(s string) { d.suggestions = append(d.suggestions, s) }

func (d *diag) category() string {
	for _, c := range categoryPriority {
		if contains(d.categories, c) {
			return c
		}
	}
	return CatOther
}

// ownerSide 只有“纯缺索引”交给 DBA，其余都需要研发改代码/改逻辑。
func (d *diag) ownerSide() string {
	if d.category() == CatMissingIndex {
		return SideDBA
	}
	return SideDev
}

// DiagResult 是诊断写回工单的字段。
type DiagResult struct {
	Category      string `json:"category"`
	OwnerSide     string `json:"owner_side"`
	Diagnosis     string `json:"diagnosis"`
	Suggestion    string `json:"suggestion"`
	SuggestedDDL  string `json:"suggested_ddl"`
	ExplainResult string `json:"-"`
}

func (d *diag) result() DiagResult {
	var fs, ss []string
	for _, f := range d.findings {
		fs = append(fs, "- "+f)
	}
	for i, s := range d.suggestions {
		ss = append(ss, fmt.Sprintf("%d. %s", i+1, s))
	}
	r := DiagResult{Category: d.category(), OwnerSide: d.ownerSide(), Diagnosis: strings.Join(fs, "\n"), Suggestion: strings.Join(ss, "\n"),
		SuggestedDDL: strings.TrimSpace(d.ddl)}
	if r.Diagnosis == "" {
		r.Diagnosis = "- " + "未命中已知规则，需DBA人工分析" // 前缀与正文分开写：详情页去掉“- ”前缀后按正文翻译
	}
	if len(d.plan) > 0 {
		if b, err := json.Marshal(d.plan); err == nil {
			r.ExplainResult = string(b)
		}
	}
	return r
}

func textRules(sql string, d *diag) {
	if reLeadLike.MatchString(sql) {
		d.hit(CatIndexInvalid, "LIKE 以 % 开头，索引无法使用", "前模糊改为后模糊(like 'abc%')；确需全文检索请改用 ES/全文索引，或冗余反转字段")
	}
	if m := reFuncOnCol.FindStringSubmatch(sql); m != nil {
		d.hit(CatIndexInvalid, fmt.Sprintf("WHERE/ON 条件对列使用了函数 %s()，索引失效", strings.ToUpper(m[2])),
			"把函数移到常量一侧，例如 date(create_time)='2024-01-01' 改为 create_time >= '2024-01-01' and create_time < '2024-01-02'")
	}
	for _, m := range reLimitOff.FindAllStringSubmatch(sql, -1) {
		off, _ := strconv.Atoi(m[1])
		if off == 0 {
			off, _ = strconv.Atoi(m[2])
		}
		if off >= 10000 {
			d.hit(CatDeepPaging, fmt.Sprintf("深分页 offset=%d，需要扫描并丢弃大量行", off),
				"改为游标分页(where id > 上一页最大id order by id limit N)，或先查主键再回表(延迟关联)")
			break
		}
	}
	if reOrderRand.MatchString(sql) {
		d.hit(CatSortGroup, "ORDER BY RAND() 会全表排序", "程序侧随机：先取 id 范围/总数，随机生成 id 再查询")
	}
	for _, m := range reInValues.FindAllStringSubmatch(sql, -1) {
		if n := strings.Count(m[1], ",") + 1; n >= 500 {
			d.hit(CatOther, fmt.Sprintf("IN 列表过长(%d个值)", n), "IN 列表控制在 500 以内，分批查询")
			break
		}
	}
	if reIsSelect.MatchString(sql) && !reHasWhere.MatchString(sql) && !reHasLimit.MatchString(sql) {
		if reHasFrom.MatchString(sql) && !reFromDual.MatchString(sql) {
			d.hit(CatFullScanBiz, "SELECT 没有 WHERE 也没有 LIMIT，全表读取", "确认业务是否真的需要全量数据；需要的话走离线/分析库，在线库分批拉取")
		}
	}
	if reSelectStar.MatchString(sql) {
		d.suggest("避免 SELECT *，只取需要的列，才有机会用到覆盖索引")
	}
	if reNotCond.MatchString(sql) {
		d.suggest("!= / <> / NOT IN 通常无法走索引范围扫描，尽量改为正向条件")
	}
	if reOrCond.MatchString(sql) && len(d.categories) == 0 {
		d.suggest("WHERE 中的 OR 连接不同列时可能无法使用索引，可改写为 UNION ALL")
	}
}

// Metrics 是诊断用到的指标。
type Metrics struct {
	AvgMs           float64
	RowsSentAvg     float64
	RowsExaminedAvg float64
	PerDay          float64
}

func metricRules(m Metrics, product string, minAvgMs int, d *diag) {
	if m.RowsSentAvg >= 10000 {
		d.hit(CatLargeResult, fmt.Sprintf("平均每次返回 %d 行", int64(m.RowsSentAvg)), "在线接口避免一次返回大量数据：分页/流式导出/走离线库")
	}
	if olapLike[product] && m.RowsExaminedAvg >= 1e8 {
		d.hit(CatOLAPHeavy, fmt.Sprintf("平均扫描 %s 行", commaInt(int64(m.RowsExaminedAvg))), "检查分区裁剪和分桶键是否命中；高频聚合考虑物化视图/汇总表；避免 SELECT *")
	}
	if m.AvgMs < float64(minAvgMs) && m.PerDay >= 100000 {
		d.hit(CatHighFrequency, fmt.Sprintf("单次不算慢(平均%.0fms)但日均执行 %s 次，总消耗高", m.AvgMs, commaInt(int64(m.PerDay))), "加缓存、合并批量调用、排查循环内查库(N+1)")
	}
	if m.RowsExaminedAvg >= 100000 && m.RowsExaminedAvg >= 100*maxf(m.RowsSentAvg, 1) {
		d.findings = append(d.findings, fmt.Sprintf("扫描/返回比过高：平均扫描 %s 行，仅返回 %s 行", commaInt(int64(m.RowsExaminedAvg)), commaInt(int64(m.RowsSentAvg))))
	}
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// commaInt 给整数加千分位。
func commaInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// whereColumns 从 WHERE 中提取某张表的条件列：等值在前、范围在后（范围列之后的列用不上索引，只取第一个范围列）。
func whereColumns(sql string, aliases map[string]bool) []string {
	m := reWhereBody.FindStringSubmatch(sql)
	if m == nil {
		return nil
	}
	var eq, rng []string
	seen := func(c string) bool { return contains(eq, c) || contains(rng, c) }
	for _, cm := range reColCond.FindAllStringSubmatch(m[1], -1) {
		alias, col, op := cm[1], cm[2], strings.ToLower(strings.TrimSpace(cm[3]))
		if sqlKeywords[strings.ToLower(col)] {
			continue
		}
		if _, err := strconv.Atoi(col); err == nil {
			continue
		}
		if alias != "" && !aliases[strings.ToLower(alias)] {
			continue
		}
		if seen(col) {
			continue
		}
		if op == "=" || op == "<=>" || op == "in" {
			eq = append(eq, col)
		} else {
			rng = append(rng, col)
		}
	}
	if len(rng) > 1 {
		rng = rng[:1]
	}
	return append(eq, rng...)
}

func quoteIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	}
	return fmt.Sprint(v)
}

func num(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	case float64:
		return x
	case float32:
		return float64(x)
	case []byte:
		f, _ := strconv.ParseFloat(string(x), 64)
		return f
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	f, _ := strconv.ParseFloat(fmt.Sprint(v), 64)
	return f
}

func explainRules(res *ExplainResult, db, sql string, d *diag) {
	d.plan = res.Plan
	var conv []string
	for _, w := range res.Warnings {
		if strings.Contains(strings.ToLower(w), "conversion") {
			conv = append(conv, w)
		}
	}
	if len(conv) > 0 {
		d.hit(CatIndexInvalid, fmt.Sprintf("隐式类型/字符集转换导致索引失效：%s", clipRunes(conv[0], 200)),
			"条件值的类型与列类型保持一致（如 varchar 列传字符串 '123' 而不是数字 123），关联字段字符集/排序规则保持一致")
	}
	for _, p := range res.Plan {
		table := str(p["table"])
		if strings.HasPrefix(table, "<") { // <derived2> / <subquery3>
			continue
		}
		rows := num(p["rows"])
		typ := strings.ToUpper(str(p["type"]))
		key := str(p["key"])
		possible := str(p["possible_keys"])
		extra := str(p["extra"])
		selectType := strings.ToUpper(str(p["select_type"]))

		if strings.Contains(selectType, "DEPENDENT") && rows >= 1000 {
			d.hit(CatSubqueryJoin, fmt.Sprintf("表 %s 为相关子查询(DEPENDENT SUBQUERY)，外层每行都要执行一次", table), "相关子查询改写为 JOIN 或先查出结果集再关联")
		}
		switch {
		case typ == "ALL" && rows >= 10000 && key == "":
			if possible != "" {
				if !contains(d.categories, CatIndexInvalid) {
					d.hit(CatIndexInvalid, fmt.Sprintf("表 %s 有候选索引(%s)但未使用，全表扫描约 %s 行", table, possible, commaInt(int64(rows))),
						"检查条件是否区分度过低、是否存在隐式转换，必要时让 DBA 评估 force index / 复合索引")
				}
			} else {
				cols := whereColumns(sql, map[string]bool{strings.ToLower(table): true})
				ddl := ""
				if len(cols) > 0 {
					tbl := quoteIdent(table)
					if db != "" {
						tbl = quoteIdent(db) + "." + tbl
					}
					lower := make([]string, len(cols))
					quoted := make([]string, len(cols))
					for i, c := range cols {
						lower[i] = strings.ToLower(c)
						quoted[i] = quoteIdent(c)
					}
					idx := "idx_" + strings.Join(lower, "_")
					if len(idx) > 54 {
						idx = idx[:54]
					}
					ddl = fmt.Sprintf("ALTER TABLE %s ADD INDEX %s (%s);", tbl, idx, strings.Join(quoted, ", "))
					d.ddl += ddl + "\n"
				}
				sg := "由 DBA 评估字段区分度后加索引"
				if ddl != "" {
					sg = fmt.Sprintf("由 DBA 评估字段区分度后加索引（草案：%s）", ddl)
				}
				d.hit(CatMissingIndex, fmt.Sprintf("表 %s 全表扫描约 %s 行且无可用索引", table, commaInt(int64(rows))), sg)
			}
		case typ == "INDEX" && rows >= 100000:
			d.hit(CatFullScanBiz, fmt.Sprintf("表 %s 全索引扫描约 %s 行", table, commaInt(int64(rows))), "补充过滤条件或建立与查询条件匹配的复合索引")
		}
		if (strings.Contains(extra, "Using temporary") || strings.Contains(extra, "Using filesort")) && rows >= 10000 {
			d.hit(CatSortGroup, fmt.Sprintf("表 %s %s，参与排序/分组约 %s 行", table, strings.TrimSpace(extra), commaInt(int64(rows))),
				"让 ORDER BY / GROUP BY 的列与过滤条件组成复合索引，或减少排序数据量")
		}
	}
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func productRules(sql, product string, d *diag) {
	switch {
	case product == ProdPolarDBX:
		if reInSubquery.MatchString(sql) {
			d.hit(CatSubqueryJoin, "分布式库中使用 IN/EXISTS 子查询，可能产生跨分片的多轮查询", "改写为 JOIN 并确保关联字段为分区键，或拆成两次查询在应用侧拼装")
		}
		d.suggest("确认 WHERE 条件包含分区键(拆分键)，否则会下发到全部分片执行")
	case olapLike[product]:
		d.suggest("确认 WHERE 命中分区列（如 dt）以触发分区裁剪；等值条件尽量命中分桶键")
	}
}

// DiagnoseInput 是一次诊断的输入。
type DiagnoseInput struct {
	Instance    *model.Instance
	Product     string
	MinAvgMs    int
	DB          string
	SQL         string
	Metrics     Metrics
	SampleReal  bool
	AutoExplain bool // 系统配置 perf.auto_explain
	TimeoutMs   int  // 系统配置 perf.explain_timeout_ms
}

// canExplain 只对单条的 select/update/delete/with 做 EXPLAIN（普通 EXPLAIN 不会执行语句）。
func canExplain(sql string) bool {
	head := strings.ToLower(strings.TrimSpace(sql))
	ok := false
	for _, p := range []string{"select", "update", "delete", "with"} {
		if strings.HasPrefix(head, p) {
			ok = true
		}
	}
	return ok && !strings.Contains(strings.TrimRight(head, "; \t\r\n"), ";")
}

// Diagnose 按规则诊断一条 SQL。explainer 为空或无法 EXPLAIN 时只用文本与指标规则。
func Diagnose(ctx context.Context, ex Explainer, in DiagnoseInput) DiagResult {
	d := &diag{}
	textRules(in.SQL, d)
	productRules(in.SQL, in.Product, d)
	metricRules(in.Metrics, in.Product, in.MinAvgMs, d)
	switch {
	case mysqlLike[in.Product] && in.SampleReal && in.AutoExplain && ex != nil && in.Instance != nil:
		if canExplain(in.SQL) {
			timeout := in.TimeoutMs
			if timeout <= 0 {
				timeout = 3000
			}
			res, err := ex.Explain(ctx, in.Instance, in.DB, strings.TrimRight(strings.TrimSpace(in.SQL), "; \t\r\n"), timeout)
			if err != nil {
				d.findings = append(d.findings, fmt.Sprintf("EXPLAIN 诊断异常：%s", clipRunes(err.Error(), 200)))
			} else {
				explainRules(res, in.DB, in.SQL, d)
			}
		}
	case !in.SampleReal:
		d.findings = append(d.findings, "仅有参数化SQL文本，未执行 EXPLAIN")
	}
	return d.result()
}
