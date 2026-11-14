// @author: kevinkelly
// @license: Apache Licence
// @file: indexextra.go
// @time: 2026/10/28

package sqlopt

import (
	"fmt"
	"strconv"
	"strings"
)

// 本文件是索引建议的补充信息：现有索引清单、冗余（前缀）索引提示，以及“建索引前后”的执行计划对比。
//
// 执行计划对比里“前”是目标库上真实的 EXPLAIN 结果；“后”不可能真的去建索引来验证，所以是按建议索引的列和表的统计信息推导的
// 估算值（访问类型、使用的索引、是否还需要排序、大致扫描行数），页面上明确标注为估算，行数基于已有索引的基数，推不出来就不给。

// IndexInfo 是一个现有索引。
type IndexInfo struct {
	Table       string   `json:"table"`
	Name        string   `json:"name"`
	Unique      bool     `json:"unique"`
	Columns     []string `json:"columns"`
	Cardinality int64    `json:"cardinality,omitempty"` // 索引全部列的基数（统计信息估算值），未知不返回
	Redundant   string   `json:"redundant,omitempty"`   // 非空表示这是冗余索引，内容是原因
}

// PlanBrief 是执行计划里和这张表有关的几个关键信息。
type PlanBrief struct {
	Type      string `json:"type"`
	Key       string `json:"key"`
	Rows      int64  `json:"rows"`
	RowsKnown bool   `json:"rows_known"`
	Filesort  bool   `json:"filesort"`
	Temporary bool   `json:"temporary"`
}

// PlanCompare 是针对一条索引建议的“建索引前后”执行计划对比。
type PlanCompare struct {
	Table    string    `json:"table"`
	NewIndex string    `json:"new_index"`
	Columns  []string  `json:"columns"`
	Before   PlanBrief `json:"before"`
	After    PlanBrief `json:"after"`
	// Estimated 恒为 true：After 是推导出来的，不是真实执行计划。
	Estimated bool   `json:"estimated"`
	Basis     string `json:"basis"`
}

func lowerCols(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = strings.ToLower(c)
	}
	return out
}

func isPrefixOf(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// coveredByNew 返回候选索引 cand 覆盖（现有索引的列是 cand 的真前缀）的非唯一、非主键索引名；没有返回空串。
func coveredByNew(m *tableMeta, cand []string) string {
	lc := lowerCols(cand)
	for _, ix := range m.Indexes {
		if ix.Unique || strings.EqualFold(ix.Name, "PRIMARY") || len(ix.Cols) == 0 || len(ix.Cols) >= len(cand) {
			continue
		}
		if isPrefixOf(lowerCols(ix.Cols), lc) {
			return ix.Name
		}
	}
	return ""
}

// redundantReason 判断索引 idx 是否冗余，返回原因；不冗余返回空串。
//   - 非唯一索引的列是另一个索引列的真前缀：前缀索引能被更长的索引代替；
//   - 两个索引列完全相同：重复索引（保留名字靠前的一个；与唯一索引重复则非唯一的那个冗余）。
//
// 主键本身承担约束，不判冗余；唯一索引只在与更靠前的同列唯一索引完全重复时才判。
func redundantReason(m *tableMeta, idx int) string {
	a := m.Indexes[idx]
	if len(a.Cols) == 0 || strings.EqualFold(a.Name, "PRIMARY") {
		return ""
	}
	ac := lowerCols(a.Cols)
	for j, b := range m.Indexes {
		if j == idx || len(b.Cols) == 0 {
			continue
		}
		bc := lowerCols(b.Cols)
		switch {
		case len(bc) == len(ac) && isPrefixOf(ac, bc): // 列完全相同
			if a.Unique && !b.Unique {
				continue
			}
			if a.Unique == b.Unique && !strings.EqualFold(b.Name, "PRIMARY") && j > idx {
				continue // 靠前的保留，后面的判重复
			}
			return fmt.Sprintf("与索引 %s 的列完全相同", b.Name)
		case !a.Unique && len(bc) > len(ac) && isPrefixOf(ac, bc):
			return fmt.Sprintf("是索引 %s(%s) 的前缀", b.Name, strings.Join(b.Cols, ", "))
		}
	}
	return ""
}

const maxRedundantFindings = 3

// redundantIndexFindings 对一张表给出冗余索引提示（S003，低级别，最多 budget 条）。
func redundantIndexFindings(m *tableMeta, budget int) []Finding {
	var out []Finding
	for i, ix := range m.Indexes {
		if len(out) >= budget {
			break
		}
		why := redundantReason(m, i)
		if why == "" {
			continue
		}
		tbl := quoteIdent(m.Name)
		if m.Schema != "" {
			tbl = quoteIdent(m.Schema) + "." + tbl
		}
		out = append(out, Finding{ID: "S003", Level: LevelLow,
			Title:      fmt.Sprintf("表 %s 的索引 %s(%s) 冗余：%s", m.Name, ix.Name, strings.Join(ix.Cols, ", "), why),
			Detail:     "冗余索引不会让查询更快，却会增加写入开销和空间占用，优化器选择索引时也多一个干扰项。",
			Suggestion: fmt.Sprintf("确认没有 FORCE INDEX / 外键依赖 %s 后，可在低峰删除：ALTER TABLE %s DROP INDEX %s;", ix.Name, tbl, quoteIdent(ix.Name))})
	}
	return out
}

// existingIndexes 列出语句涉及的各张表的现有索引（含冗余标记和基数）。
func existingIndexes(ms *metaSet) []IndexInfo {
	var out []IndexInfo
	seen := map[*tableMeta]bool{}
	for _, m := range ms.metas {
		if m == nil || seen[m] {
			continue
		}
		seen[m] = true
		for i, ix := range m.Indexes {
			if len(ix.Cols) == 0 {
				continue
			}
			info := IndexInfo{Table: m.Name, Name: ix.Name, Unique: ix.Unique, Columns: append([]string{}, ix.Cols...), Redundant: redundantReason(m, i)}
			if n := len(ix.Cards); n > 0 && n == len(ix.Cols) {
				info.Cardinality = ix.Cards[n-1]
			}
			out = append(out, info)
		}
	}
	return out
}

// briefOf 从 EXPLAIN 结果里取出某张表那一行的关键信息；找不到对应行时取第一行。
func briefOf(plan *Table, names ...string) (PlanBrief, bool) {
	if plan == nil || len(plan.Rows) == 0 {
		return PlanBrief{}, false
	}
	pick := plan.Rows[0]
	if ti := plan.colIndex("table"); ti >= 0 {
	outer:
		for _, r := range plan.Rows {
			if ti >= len(r) || r[ti] == nil {
				continue
			}
			for _, n := range names {
				if n != "" && strings.EqualFold(cellString(r[ti]), n) {
					pick = r
					break outer
				}
			}
		}
	}
	get := func(col string) string {
		i := plan.colIndex(col)
		if i < 0 || i >= len(pick) || pick[i] == nil {
			return ""
		}
		return cellString(pick[i])
	}
	b := PlanBrief{Type: get("type"), Key: get("key")}
	if v := get("rows"); v != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			b.Rows, b.RowsKnown = n, true
		}
	}
	extra := strings.ToLower(get("extra"))
	b.Filesort = strings.Contains(extra, "using filesort")
	b.Temporary = strings.Contains(extra, "using temporary")
	return b, true
}

// buildPlanCompare 推导建议索引上线后的执行计划。plan 是目标库上真实的 EXPLAIN。
func buildPlanCompare(s *indexSugg, plan *Table, ref tableRef) *PlanCompare {
	if s == nil {
		return nil
	}
	before, ok := briefOf(plan, ref.Alias, ref.Name)
	if !ok {
		return nil
	}
	m := s.Meta
	after := PlanBrief{Key: s.Name, Temporary: before.Temporary}
	var basis []string
	switch {
	case s.Rng != "":
		after.Type = "range"
	case len(s.Eq) > 0:
		after.Type = "ref"
	default:
		after.Type = "index"
	}
	// 等值列放在索引最前面、排序列接在等值列后面，且没有范围列时，排序可以直接走索引顺序
	after.Filesort = before.Filesort && !(s.Rng == "" && len(s.Order) > 0)
	if before.Filesort && !after.Filesort {
		basis = append(basis, "排序列已纳入索引，可以消除 filesort")
	}
	if len(s.Eq) > 0 && s.Rng == "" && m.Rows > 0 {
		if card, ixName, pl := eqCardinality(m, s.Eq); card > 0 {
			rows := m.Rows / card
			if rows < 1 {
				rows = 1
			}
			if before.RowsKnown && rows > before.Rows {
				rows = before.Rows
			}
			after.Rows, after.RowsKnown = rows, true
			basis = append(basis, fmt.Sprintf("扫描行数上限按表约 %s 行 ÷ 现有索引 %s 前 %d 列的基数 %s 估算，实际可能更少", fmtRows(m.Rows), ixName, pl, fmtRows(card)))
		}
	}
	if !after.RowsKnown {
		basis = append(basis, "现有索引的统计信息推不出扫描行数，建好索引后请再次 EXPLAIN 确认")
	}
	return &PlanCompare{Table: m.Name, NewIndex: s.Name, Columns: append([]string{}, s.Cand...), Before: before, After: after, Estimated: true, Basis: strings.Join(basis, "；")}
}

// eqCardinality 用现有索引的统计基数估算等值列的区分度：对每个现有索引，取其开头恰好落在等值列集合里的最长前缀，
// 该前缀的基数就是“这些等值列至少能把行数切成多少份”的下界；返回其中最大的一个（及所属索引、前缀长度）。
// 等值列一起使用时区分度只会更高，所以据此算出的扫描行数是上限，实际可能更少。
func eqCardinality(m *tableMeta, eq []string) (card int64, index string, prefix int) {
	want := map[string]bool{}
	for _, c := range eq {
		want[strings.ToLower(c)] = true
	}
	for _, ix := range m.Indexes {
		if len(ix.Cards) != len(ix.Cols) {
			continue
		}
		l := 0
		for l < len(ix.Cols) && l < len(eq) && want[strings.ToLower(ix.Cols[l])] {
			l++
		}
		if l > 0 && ix.Cards[l-1] > card {
			card, index, prefix = ix.Cards[l-1], ix.Name, l
		}
	}
	return card, index, prefix
}
