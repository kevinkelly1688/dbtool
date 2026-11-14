// @author: kevinkelly
// @license: Apache Licence
// @file: indexadv.go
// @time: 2026/10/28

package sqlopt

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"dbtool/internal/sqlx"
)

// 本文件是“需要目标实例”的那部分内置规则：读取表的列与索引信息（information_schema），
// 做隐式类型转换检查（S001）和单表查询的索引建议（S002）。

type colMeta struct{ Name, DataType, ColumnType string }

type idxMeta struct {
	Name   string
	Unique bool
	Cols   []string
	Cards  []int64 // 与 Cols 对应：前 i+1 列组合的基数（统计信息估算值；未知为 0）
}

type tableMeta struct {
	Schema, Name string
	Cols         []colMeta
	Indexes      []idxMeta
	Rows         int64 // information_schema.tables.table_rows（InnoDB 下是估算值）
}

func (m *tableMeta) col(name string) *colMeta {
	for i := range m.Cols {
		if strings.EqualFold(m.Cols[i].Name, name) {
			return &m.Cols[i]
		}
	}
	return nil
}

// loadTableMeta 读取一张表的列、索引、行数。表不存在返回 (nil, nil)。
func loadTableMeta(ctx context.Context, sess Session, schema, table string) (*tableMeta, error) {
	cols, err := sess.Query(ctx, "SELECT column_name, data_type, column_type FROM information_schema.columns WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position", schema, table)
	if err != nil {
		return nil, err
	}
	if len(cols.Rows) == 0 {
		return nil, nil
	}
	m := &tableMeta{Schema: schema, Name: table}
	for _, r := range cols.Rows {
		m.Cols = append(m.Cols, colMeta{Name: cellString(r[0]), DataType: strings.ToLower(cellString(r[1])), ColumnType: cellString(r[2])})
	}
	idx, err := sess.Query(ctx, "SELECT index_name, non_unique, seq_in_index, column_name, cardinality FROM information_schema.statistics WHERE table_schema = ? AND table_name = ? ORDER BY index_name, seq_in_index", schema, table)
	if err != nil {
		return nil, err
	}
	byName := map[string]*idxMeta{}
	var order []string
	for _, r := range idx.Rows {
		name := cellString(r[0])
		im := byName[name]
		if im == nil {
			im = &idxMeta{Name: name, Unique: cellString(r[1]) == "0"}
			byName[name] = im
			order = append(order, name)
		}
		if r[3] != nil { // 函数索引的表达式部分没有列名
			im.Cols = append(im.Cols, cellString(r[3]))
			var card int64
			if len(r) > 4 && r[4] != nil {
				card, _ = strconv.ParseInt(cellString(r[4]), 10, 64)
			}
			im.Cards = append(im.Cards, card)
		}
	}
	for _, n := range order {
		m.Indexes = append(m.Indexes, *byName[n])
	}
	if t, err := sess.Query(ctx, "SELECT IFNULL(table_rows, 0) FROM information_schema.tables WHERE table_schema = ? AND table_name = ?", schema, table); err == nil && len(t.Rows) > 0 {
		m.Rows, _ = strconv.ParseInt(cellString(t.Rows[0][0]), 10, 64)
	}
	return m, nil
}

// metaSet 是语句涉及的各表元数据，用于按别名/列名解析列。
type metaSet struct {
	refs  []tableRef
	metas []*tableMeta // 与 refs 一一对应，表不存在为 nil
}

// resolve 解析列引用 → 所属表。限定名按别名或表名匹配；无限定名只在“恰好一张表有该列”时才认。
func (ms *metaSet) resolve(qualifier, name string) (*tableMeta, *colMeta) {
	var hit *tableMeta
	var col *colMeta
	for i, r := range ms.refs {
		m := ms.metas[i]
		if m == nil {
			continue
		}
		if qualifier != "" && !strings.EqualFold(qualifier, r.Alias) && !strings.EqualFold(qualifier, r.Name) {
			continue
		}
		if c := m.col(name); c != nil {
			if hit != nil && hit != m {
				return nil, nil // 不唯一
			}
			hit, col = m, c
		}
	}
	return hit, col
}

var charTypes = map[string]bool{"char": true, "varchar": true, "tinytext": true, "text": true, "mediumtext": true, "longtext": true}
var unindexableTypes = map[string]bool{"tinytext": true, "text": true, "mediumtext": true, "longtext": true, "tinyblob": true,
	"blob": true, "mediumblob": true, "longblob": true, "json": true, "geometry": true, "point": true, "linestring": true, "polygon": true}

// opAt 读取 i 位置开始的连续比较运算符（= < > ! 的组合），返回运算符文本与占用的词数。
func opAt(p *pstmt, i int) (string, int) {
	var b strings.Builder
	n := 0
	for k := i; k < len(p.sig) && p.sig[k].Kind == sqlx.Punct && strings.Contains("=<>!", p.sig[k].Text) && p.sig[k].Text != ""; k++ {
		if n > 0 && p.sig[k].Start != p.sig[k-1].End {
			break
		}
		b.WriteString(p.sig[k].Text)
		n++
	}
	return b.String(), n
}

// implicitConversion 检查“字符串列与数字常量比较”（S001）。
func implicitConversion(p *pstmt, ms *metaSet) []Finding {
	mask := p.condMask()
	var out []Finding
	seen := map[string]bool{}
	report := func(m *tableMeta, c *colMeta, lit string) {
		key := m.Name + "." + c.Name
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Finding{ID: "S001", Level: LevelMedium,
			Title:      fmt.Sprintf("字符串列 %s.%s（%s）与数字 %s 比较，发生隐式类型转换", m.Name, c.Name, c.ColumnType, lit),
			Detail:     "MySQL 会把该列每一行的值转成数字再比较，列上的索引失效，而且 '1abc' 之类的值会被当成 1 造成误匹配。",
			Suggestion: fmt.Sprintf("给常量加引号，写成 %s = '%s'，或把列类型改成与业务一致的数字类型。", c.Name, lit)})
	}
	for i := range p.sig {
		if !mask[i] {
			continue
		}
		name, n, ok := p.columnAt(i)
		if !ok {
			continue
		}
		m, c := ms.resolve(p.qualifierAt(i), name)
		if c == nil || !charTypes[c.DataType] {
			continue
		}
		j := i + n
		switch op, k := opAt(p, j); {
		case k > 0 && (op == "=" || op == "<=>" || op == "<>" || op == "!=" || op == "<" || op == ">" || op == "<=" || op == ">="):
			if j+k < len(p.sig) && p.sig[j+k].Kind == sqlx.Number {
				report(m, c, p.sig[j+k].Text)
			}
		case p.w(j) == "in" && p.punct(j+1, "(") && j+2 < len(p.sig) && p.sig[j+2].Kind == sqlx.Number:
			report(m, c, p.sig[j+2].Text)
		}
	}
	return out
}

// conjunct 是 WHERE 里按顶层 AND 切出的一个条件，[lo,hi) 是词的区间。
type conjunct struct{ lo, hi int }

// splitConjuncts 切分 WHERE 条件；含顶层 OR 返回 hasOr=true（此时不给索引建议）。
func splitConjuncts(p *pstmt, w, end int) (list []conjunct, hasOr bool) {
	d := p.depth[w]
	lo := w + 1
	between := false
	for i := w + 1; i < end; i++ {
		if p.depth[i] != d {
			continue
		}
		switch p.w(i) {
		case "between":
			between = true
		case "or":
			hasOr = true
		case "and":
			if between {
				between = false
				continue
			}
			list = append(list, conjunct{lo, i})
			lo = i + 1
		}
	}
	if lo < end {
		list = append(list, conjunct{lo, end})
	}
	return list, hasOr
}

// orderColumns 取顶层 ORDER BY 的列（全部是简单列、方向一致才返回）。
func orderColumns(p *pstmt) []string {
	o := p.top("order")
	if o < 0 || p.w(o+1) != "by" {
		return nil
	}
	end := p.endOfClause(o+2, 0, map[string]bool{"limit": true, "for": true, "lock": true, "union": true, "into": true})
	var cols []string
	dir := ""
	i := o + 2
	for i < end {
		name, n, ok := p.columnAt(i)
		if !ok {
			return nil
		}
		cols = append(cols, name)
		i += n
		d := "asc"
		if p.w(i) == "asc" || p.w(i) == "desc" {
			d = p.w(i)
			i++
		}
		if dir != "" && dir != d {
			return nil // 升降序混用，8.0 以前的索引无法满足
		}
		dir = d
		if i < end {
			if !p.punct(i, ",") {
				return nil
			}
			i++
		}
	}
	return cols
}

func lowerSet(cols []string) map[string]bool {
	m := map[string]bool{}
	for _, c := range cols {
		m[strings.ToLower(c)] = true
	}
	return m
}

// indexServes 判断已有索引能否满足“等值列 + 范围列/排序列”的访问。
func indexServes(ix idxMeta, eq []string, rng string, order []string) bool {
	cols := make([]string, len(ix.Cols))
	for i, c := range ix.Cols {
		cols[i] = strings.ToLower(c)
	}
	eqSet := lowerSet(eq)
	k := len(eqSet)
	if ix.Unique && len(cols) > 0 && len(cols) <= k {
		all := true
		for _, c := range cols {
			all = all && eqSet[c]
		}
		if all {
			return true // 唯一索引的全部列都是等值条件：最多一行
		}
	}
	if k > 0 {
		if len(cols) < k {
			return false
		}
		for _, c := range cols[:k] {
			if !eqSet[c] {
				return false
			}
		}
		return true
	}
	if rng != "" {
		return len(cols) > 0 && cols[0] == strings.ToLower(rng)
	}
	if len(order) > 0 && len(cols) >= len(order) {
		for i, c := range order {
			if cols[i] != strings.ToLower(c) {
				return false
			}
		}
		return true
	}
	return false
}

// adviseIndex 对单表语句给出索引建议（S002）：候选索引、依据的条件列与对应的 ALTER 语句。没有建议返回 nil。
func adviseIndex(p *pstmt, ms *metaSet) *indexSugg {
	if len(ms.refs) != 1 || ms.metas[0] == nil {
		return nil
	}
	switch p.kind {
	case TypeSelect, TypeUpdate, TypeDelete:
	default:
		return nil
	}
	m := ms.metas[0]
	w := p.top("where")
	var eq []string
	rng := ""
	if w >= 0 {
		end := p.endOfClause(w+1, 0, whereStop)
		list, hasOr := splitConjuncts(p, w, end)
		if hasOr {
			return nil
		}
		seen := map[string]bool{}
		for _, cj := range list {
			name, n, ok := p.columnAt(cj.lo)
			if !ok || m.col(name) == nil {
				continue
			}
			if q := p.qualifierAt(cj.lo); q != "" && !strings.EqualFold(q, ms.refs[0].Alias) && !strings.EqualFold(q, ms.refs[0].Name) {
				continue
			}
			j := cj.lo + n
			isEq, isRange := false, false
			switch op, k := opAt(p, j); {
			case k > 0 && (op == "=" || op == "<=>"):
				// 右侧是另一列（a = b）不算常量等值
				if _, _, rhsCol := p.columnAt(j + k); !rhsCol {
					isEq = true
				}
			case k > 0 && (op == ">" || op == "<" || op == ">=" || op == "<="):
				isRange = true
			case p.w(j) == "in" && p.punct(j+1, "("):
				isEq = true
			case p.w(j) == "is" && p.w(j+1) == "null":
				isEq = true
			case p.w(j) == "between":
				isRange = true
			case p.w(j) == "like" && j+1 < cj.hi && p.sig[j+1].Kind == sqlx.String && len(p.sig[j+1].Text) > 1 && p.sig[j+1].Text[1] != '%' && p.sig[j+1].Text[1] != '_':
				isRange = true
			}
			lname := strings.ToLower(name)
			switch {
			case isEq && !seen[lname]:
				seen[lname] = true
				eq = append(eq, m.col(name).Name)
			case isRange && rng == "":
				rng = m.col(name).Name
			}
		}
	}
	// 范围列已经占了“最后一列”，排序再追加无意义；没有范围列时让排序列接在等值列后面
	var order []string
	if rng == "" && p.kind == TypeSelect && (len(eq) > 0 || p.top("limit") >= 0) {
		for _, c := range orderColumns(p) {
			if mc := m.col(c); mc != nil {
				order = append(order, mc.Name)
			} else {
				order = nil
				break
			}
		}
	}
	if len(eq) == 0 && rng == "" && len(order) == 0 {
		return nil
	}
	for _, ix := range m.Indexes {
		if indexServes(ix, eq, rng, order) {
			return nil
		}
	}
	// 候选索引列：等值列 → 范围列（或排序列），去重、限 5 列、遇到不能建索引的类型就截断
	var cand []string
	used := map[string]bool{}
	push := func(c string) bool {
		if used[strings.ToLower(c)] || len(cand) >= 5 {
			return true
		}
		if mc := m.col(c); mc == nil || unindexableTypes[mc.DataType] {
			return false
		}
		used[strings.ToLower(c)] = true
		cand = append(cand, c)
		return true
	}
	ok := true
	for _, c := range eq {
		if ok = push(c); !ok {
			break
		}
	}
	if ok && rng != "" {
		push(rng)
	} else if ok {
		for _, c := range order {
			if !push(c) {
				break
			}
		}
	}
	if len(cand) == 0 {
		return nil
	}
	name := "idx_" + strings.ToLower(strings.Join(cand, "_"))
	if len(name) > 64 {
		name = name[:64]
	}
	qs := make([]string, len(cand))
	for i, c := range cand {
		qs[i] = quoteIdent(c)
	}
	tbl := quoteIdent(m.Name)
	if m.Schema != "" {
		tbl = quoteIdent(m.Schema) + "." + tbl
	}
	ddl := fmt.Sprintf("ALTER TABLE %s ADD INDEX %s (%s);", tbl, quoteIdent(name), strings.Join(qs, ", "))
	lv := LevelMedium
	switch {
	case m.Rows >= 100000:
		lv = LevelHigh
	case m.Rows < 1000:
		lv = LevelLow
	}
	detail := fmt.Sprintf("表 %s 现有索引都不能同时覆盖查询条件的列，查询会扫描大量行（表约 %s 行）。等值条件的列放在前面，范围条件或排序的列放在最后。", m.Name, fmtRows(m.Rows))
	if m.Rows < 1000 {
		detail += "表数据量很小，索引收益有限，请结合实际增长趋势判断。"
	}
	prefixOf := coveredByNew(m, cand)
	if prefixOf != "" {
		detail += fmt.Sprintf("新索引以现有索引 %s 为前缀，上线后 %s 可以评估删除（先确认没有 FORCE INDEX / 外键依赖它）。", prefixOf, prefixOf)
	}
	return &indexSugg{Meta: m, Eq: eq, Rng: rng, Order: order, Cand: cand, Name: name, DDL: ddl, PrefixOf: prefixOf,
		Finding: Finding{ID: "S002", Level: lv, Title: fmt.Sprintf("建议为表 %s 增加索引 (%s)", m.Name, strings.Join(cand, ", ")),
			Detail: detail, Suggestion: ddl}}
}

// indexSugg 是一次索引建议的完整信息：除了页面展示的 Finding 与 DDL，还带着“依据哪些条件列”，用来推导建索引前后的执行计划变化。
type indexSugg struct {
	Meta     *tableMeta
	Eq       []string // 等值条件列
	Rng      string   // 范围条件列
	Order    []string // 追加的排序列
	Cand     []string // 候选索引的列
	Name     string
	DDL      string
	PrefixOf string // 候选索引覆盖（以它为前缀）的现有索引，没有为空
	Finding  Finding
}

// indexAdvice 对单表语句给出索引建议（S002）与对应的 ALTER 语句。
func indexAdvice(p *pstmt, ms *metaSet) ([]Finding, []string) {
	s := adviseIndex(p, ms)
	if s == nil {
		return nil, nil
	}
	return []Finding{s.Finding}, []string{s.DDL}
}
