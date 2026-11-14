// @author: kevinkelly
// @license: Apache Licence
// @file: plan.go
// @time: 2026/10/28

package sqlopt

import (
	"fmt"
	"strconv"
	"strings"

	"dbtool/internal/sqlx"
)

// normalizeSingle 去注释、要求恰好一条非空语句，返回该语句（不含分号）。
func normalizeSingle(sqlText string) (string, error) {
	if strings.TrimSpace(sqlText) == "" {
		return "", bad("请输入 SQL")
	}
	stmts := sqlx.Split(sqlText)
	switch {
	case len(stmts) == 0:
		return "", bad("没有有效的 SQL 语句")
	case len(stmts) > 1:
		return "", bad("一次只能分析一条 SQL，请只保留一条语句")
	}
	if len(stmts[0]) > MaxSQLBytes {
		return "", bad(fmt.Sprintf("SQL 过长（超过 %dKB）", MaxSQLBytes/1024))
	}
	return stmts[0], nil
}

// firstWordIdx 跳过开头的括号，返回第一个关键字的位置。
func firstWordIdx(p *pstmt) int {
	i := 0
	for i < len(p.sig) && p.punct(i, "(") {
		i++
	}
	return i
}

// prepareExplain 校验并规整要做 EXPLAIN 的语句，返回可以直接接在 "EXPLAIN " 后面的语句与类型。
//
// 只允许单条 SELECT / UPDATE / DELETE / INSERT…SELECT（及 REPLACE…SELECT、WITH 开头的它们）。
// 兼容“用户自己写了 explain”的情况（去掉前缀），但拒绝 EXPLAIN ANALYZE
// ——它会真实执行语句。含 sleep/benchmark/load_file 等危险函数或 INTO OUTFILE 的语句也拒绝。
func prepareExplain(sqlText string) (stmt, kind string, err error) {
	one, err := normalizeSingle(sqlText)
	if err != nil {
		return "", "", err
	}
	p := parseStmt(one)
	if kw := p.w(firstWordIdx(p)); kw == "explain" || kw == "desc" || kw == "describe" {
		i := 1
		for i < len(p.sig) {
			switch {
			case p.w(i) == "analyze":
				return "", "", bad("不支持 EXPLAIN ANALYZE（它会真实执行语句），请直接写要分析的语句")
			case p.w(i) == "extended" || p.w(i) == "partitions":
				i++
				continue
			case p.w(i) == "format" && p.punct(i+1, "="):
				i += 3
				continue
			}
			break
		}
		if i >= len(p.sig) {
			return "", "", bad("没有有效的 SQL 语句")
		}
		one = one[p.sig[i].Start:]
		p = parseStmt(one)
	}
	switch p.kind {
	case TypeSelect, TypeUpdate, TypeDelete:
	case TypeInsert, TypeReplace:
		if p.find(0, len(p.sig), 0, "select") < 0 {
			return "", "", bad("INSERT / REPLACE 仅支持 INSERT … SELECT 形式的语句查看执行计划")
		}
	default:
		return "", "", bad("仅支持对 SELECT / UPDATE / DELETE / INSERT … SELECT 语句查看执行计划")
	}
	if kw := p.w(firstWordIdx(p)); kw == "table" || kw == "values" {
		return "", "", bad("仅支持对 SELECT / UPDATE / DELETE / INSERT … SELECT 语句查看执行计划")
	}
	if err := rejectDangerous(one); err != nil {
		return "", "", err
	}
	return one, p.kind, nil
}

// prepareSelect 要求是一条只读查询（SELECT 或 WITH … SELECT），用于 SQLTuning。
func prepareSelect(sqlText string) (string, error) {
	one, kind, err := prepareExplain(sqlText)
	if err != nil {
		return "", bad("只支持查询 SQL（SELECT）")
	}
	if kind != TypeSelect {
		return "", bad("只支持查询 SQL（SELECT）")
	}
	return one, nil
}

// externalKeywords 是交给外部工具（SQLAdvisor / SOAR）的语句允许的开头关键字。
// 不是 SQL 关键字开头的内容一律拒绝：SOAR 的 -query 参数会把“看起来像文件路径”的内容当文件读。
var externalKeywords = map[string]bool{
	"select": true, "with": true, "insert": true, "replace": true, "update": true, "delete": true,
	"alter": true, "create": true, "drop": true, "truncate": true, "rename": true,
}

func prepareExternal(sqlText string) (string, error) {
	one, err := normalizeSingle(sqlText)
	if err != nil {
		return "", err
	}
	p := parseStmt(one)
	if !externalKeywords[p.w(firstWordIdx(p))] {
		return "", bad("SQL 必须以 SELECT / INSERT / UPDATE / DELETE / ALTER 等语句关键字开头")
	}
	if strings.ContainsRune(one, 0) {
		return "", bad("SQL 含有非法字符")
	}
	return one, nil
}

func rejectDangerous(stmt string) error {
	if fns := sqlx.DangerousFunctions(stmt); len(fns) > 0 {
		return bad(fmt.Sprintf("语句里使用了不允许的函数：%s", strings.Join(fns, "、")))
	}
	if sqlx.ContainsSequence(stmt, "into", "outfile") || sqlx.ContainsSequence(stmt, "into", "dumpfile") {
		return bad("不允许 INTO OUTFILE / DUMPFILE")
	}
	return nil
}

// ---- 执行计划分析 ----

func planInt(t *Table, row []any, col string) int64 {
	s := strings.TrimSpace(t.str(row, col))
	if s == "" {
		return 0
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(f)
	}
	return 0
}

func planFloat(t *Table, row []any, col string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(t.str(row, col)), 64)
	return f
}

func fmtRows(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// planFindings 解读 MySQL 传统格式的 EXPLAIN 结果（列：id select_type table type possible_keys key rows filtered Extra）。
func planFindings(plan *Table) []Finding {
	if plan == nil || plan.colIndex("type") < 0 {
		return nil
	}
	var out []Finding
	add := func(f Finding) {
		for _, e := range out { // 同一规则同一张表只报一次
			if e.ID == f.ID && e.Title == f.Title {
				return
			}
		}
		out = append(out, f)
	}
	for _, row := range plan.Rows {
		tbl := plan.str(row, "table")
		typ := strings.ToLower(plan.str(row, "type"))
		key := plan.str(row, "key")
		poss := plan.str(row, "possible_keys")
		extra := plan.str(row, "Extra")
		rows := planInt(plan, row, "rows")
		sel := strings.ToUpper(plan.str(row, "select_type"))
		lvBig := func(big, small string) string {
			if rows >= 100000 {
				return big
			}
			return small
		}
		switch {
		case typ == "all":
			lv := LevelLow
			switch {
			case rows >= 100000:
				lv = LevelHigh
			case rows >= 1000:
				lv = LevelMedium
			}
			f := Finding{ID: "P001", Level: lv, Title: fmt.Sprintf("表 %s 全表扫描，预估扫描 %s 行", tbl, fmtRows(rows)),
				Detail:     "type=ALL 表示没有使用任何索引，要逐行读取整张表。" + map[bool]string{true: "表很小时影响不大。", false: ""}[lv == LevelLow],
				Suggestion: "在 WHERE / JOIN 条件的列上建立合适的索引；检查条件是否对列使用了函数或发生了隐式类型转换。"}
			add(f)
			if poss != "" && key == "" {
				add(Finding{ID: "P005", Level: LevelMedium, Title: fmt.Sprintf("表 %s 有候选索引（%s）但没有使用", tbl, truncRunes(poss, 60)),
					Detail:     "优化器评估后认为全表扫描更划算：可能是命中数据比例太高、统计信息过期、条件对列做了运算或隐式转换。",
					Suggestion: "ANALYZE TABLE 更新统计信息；检查条件写法；必要时用 FORCE INDEX 对比验证。"})
			}
		case typ == "index":
			add(Finding{ID: "P002", Level: lvBig(LevelMedium, LevelLow), Title: fmt.Sprintf("表 %s 全索引扫描（key=%s），预估 %s 行", tbl, key, fmtRows(rows)),
				Detail:     "type=index 要把整棵索引从头扫到尾，比全表扫描略好，但数据量大时仍然很慢。",
				Suggestion: "让查询条件能用到索引最左前缀，或加上 LIMIT。"})
		}
		if strings.Contains(extra, "Using filesort") {
			add(Finding{ID: "P003", Level: lvBig(LevelMedium, LevelLow), Title: fmt.Sprintf("表 %s 需要额外排序（Using filesort）", tbl),
				Detail:     "排序无法利用索引的有序性，要在内存或磁盘上额外排序。",
				Suggestion: "建立 (过滤列, 排序列) 的联合索引，让索引顺序与 ORDER BY 一致。"})
		}
		if strings.Contains(extra, "Using temporary") {
			add(Finding{ID: "P004", Level: LevelMedium, Title: fmt.Sprintf("表 %s 需要临时表（Using temporary）", tbl),
				Detail:     "GROUP BY / DISTINCT / UNION 等操作用到了临时表，数据量大时可能落盘。",
				Suggestion: "让分组列有索引；减少参与分组的数据量；确认 tmp_table_size 与 max_heap_table_size 足够。"})
		}
		if strings.Contains(extra, "Using join buffer") {
			add(Finding{ID: "P006", Level: lvBig(LevelHigh, LevelMedium), Title: fmt.Sprintf("表 %s 作为被驱动表使用 join buffer", tbl),
				Detail:     "连接列上没有可用索引，每次都要在 join buffer 里做块嵌套循环 / 哈希连接。",
				Suggestion: "在被驱动表的连接列上建立索引，并确保两边连接列的类型和字符集一致。"})
		}
		if strings.Contains(sel, "DEPENDENT") {
			add(Finding{ID: "P007", Level: LevelHigh, Title: fmt.Sprintf("相关子查询（%s）", sel),
				Detail:     "子查询依赖外层查询的列，外层每一行都要执行一次子查询。",
				Suggestion: "改写成 JOIN 或派生表，并给连接列建立索引。"})
		}
		if strings.Contains(extra, "Range checked for each record") {
			add(Finding{ID: "P008", Level: LevelHigh, Title: fmt.Sprintf("表 %s：Range checked for each record", tbl),
				Detail:     "优化器找不到好的索引，只能对外层每一行重新判断可用的索引。",
				Suggestion: "为连接/过滤列建立合适的索引。"})
		}
		if fl := planFloat(plan, row, "filtered"); rows >= 10000 && plan.colIndex("filtered") >= 0 && fl > 0 && fl <= 10 && typ != "all" {
			add(Finding{ID: "P009", Level: LevelLow, Title: fmt.Sprintf("表 %s 扫描 %s 行后只保留约 %.0f%%", tbl, fmtRows(rows), fl),
				Detail:     "索引过滤得不够，大部分读出的行在回表后又被丢弃。",
				Suggestion: "把后续过滤条件的列加入联合索引，让更多过滤在索引层完成。"})
		}
		if strings.Contains(extra, "Impossible WHERE") || strings.Contains(extra, "no matching row") {
			add(Finding{ID: "P010", Level: LevelInfo, Title: fmt.Sprintf("优化器判定 WHERE 条件不可能成立：%s", extra),
				Detail: "语句不会读取任何数据，请确认条件是否写错。"})
		}
	}
	return out
}
