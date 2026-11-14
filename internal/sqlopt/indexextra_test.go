// @author: kevinkelly
// @license: Apache Licence
// @file: indexextra_test.go
// @time: 2026/10/28

package sqlopt

import (
	"strings"
	"testing"
)

func metaWith(rows int64, ixs ...idxMeta) *tableMeta {
	m := ordersMeta()
	m.Rows = rows
	m.Indexes = ixs
	return m
}

func TestRedundantIndexes(t *testing.T) {
	m := metaWith(100000,
		idxMeta{Name: "PRIMARY", Unique: true, Cols: []string{"id"}},
		idxMeta{Name: "idx_a", Cols: []string{"user_id"}},                 // 是 idx_ab 的前缀
		idxMeta{Name: "idx_ab", Cols: []string{"user_id", "status"}},      // 最长，保留
		idxMeta{Name: "idx_dup1", Cols: []string{"phone"}},                // 与 idx_dup2 完全相同，靠前的保留
		idxMeta{Name: "idx_dup2", Cols: []string{"phone"}},                // 重复
		idxMeta{Name: "uk_id", Unique: true, Cols: []string{"id"}},        // 与主键重复
		idxMeta{Name: "uk_user", Unique: true, Cols: []string{"user_id"}}, // 唯一索引是约束，不因前缀判冗余
		idxMeta{Name: "idx_created", Cols: []string{"created_at"}},
	)
	got := map[string]string{}
	for i, ix := range m.Indexes {
		if why := redundantReason(m, i); why != "" {
			got[ix.Name] = why
		}
	}
	want := []string{"idx_a", "idx_dup2", "uk_id"}
	if len(got) != len(want) {
		t.Fatalf("冗余索引: %v", got)
	}
	for _, n := range want {
		if got[n] == "" {
			t.Fatalf("%s 应判冗余: %v", n, got)
		}
	}
	if !strings.Contains(got["idx_a"], "idx_ab") || !strings.Contains(got["idx_dup2"], "idx_dup1") || !strings.Contains(got["uk_id"], "PRIMARY") {
		t.Fatalf("原因: %v", got)
	}
	fs := redundantIndexFindings(m, 10)
	if len(fs) != 3 || fs[0].ID != "S003" || fs[0].Level != LevelLow || !strings.Contains(fs[0].Suggestion, "DROP INDEX") {
		t.Fatalf("冗余提示: %+v", fs)
	}
	if got := redundantIndexFindings(m, 2); len(got) != 2 {
		t.Fatalf("应受 budget 限制: %d", len(got))
	}
}

func TestExistingIndexesListing(t *testing.T) {
	m := metaWith(5000, idxMeta{Name: "PRIMARY", Unique: true, Cols: []string{"id"}, Cards: []int64{5000}},
		idxMeta{Name: "idx_u", Cols: []string{"user_id"}, Cards: []int64{300}},
		idxMeta{Name: "idx_ut", Cols: []string{"user_id", "status"}, Cards: []int64{300, 900}})
	_, ms := metaFor("select * from orders where user_id = 1", m)
	list := existingIndexes(ms)
	if len(list) != 3 || list[0].Name != "PRIMARY" || list[0].Cardinality != 5000 || !list[0].Unique {
		t.Fatalf("清单: %+v", list)
	}
	if list[1].Redundant == "" || list[2].Cardinality != 900 || list[2].Redundant != "" {
		t.Fatalf("冗余/基数: %+v", list)
	}
	// 同一张表被自连接两次引用，只列一次
	_, ms = metaFor("select * from orders a join orders b on a.id = b.id", m)
	if n := len(existingIndexes(ms)); n != 3 {
		t.Fatalf("自连接重复: %d", n)
	}
}

func planOf(typ, key, rows, extra string) *Table {
	return &Table{Columns: []string{"id", "select_type", "table", "type", "possible_keys", "key", "rows", "filtered", "Extra"},
		Rows: [][]any{{"1", "SIMPLE", "orders", typ, nil, nilIfEmpty(key), rows, "10.00", extra}}}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestPlanCompareRowsAndFilesort(t *testing.T) {
	// status 上只有单列索引 idx_status(基数 5)：两个等值列 (status, phone) 现有索引盖不住 → 建议 (status, phone)
	m := metaWith(1000000,
		idxMeta{Name: "PRIMARY", Unique: true, Cols: []string{"id"}, Cards: []int64{1000000}},
		idxMeta{Name: "idx_status", Cols: []string{"status"}, Cards: []int64{5}},
		idxMeta{Name: "idx_phone", Cols: []string{"phone"}, Cards: []int64{200000}})
	p, ms := metaFor("select * from orders where status = 'a' and phone = '1' order by created_at limit 10", m)
	s := adviseIndex(p, ms)
	if s == nil || strings.Join(s.Cand, ",") != "status,phone,created_at" {
		t.Fatalf("候选: %+v", s)
	}
	pc := buildPlanCompare(s, planOf("ref", "idx_status", "200000", "Using where; Using filesort"), ms.refs[0])
	if pc == nil || !pc.Estimated || pc.Table != "orders" || pc.NewIndex != s.Name {
		t.Fatalf("对比: %+v", pc)
	}
	if pc.Before.Type != "ref" || pc.Before.Key != "idx_status" || pc.Before.Rows != 200000 || !pc.Before.Filesort {
		t.Fatalf("前: %+v", pc.Before)
	}
	// 后：ref + 新索引 + 无 filesort；行数取 idx_phone 基数 200000 → 1000000/200000 = 5
	if pc.After.Type != "ref" || pc.After.Key != s.Name || pc.After.Filesort || !pc.After.RowsKnown || pc.After.Rows != 5 {
		t.Fatalf("后: %+v", pc.After)
	}
	if !strings.Contains(pc.Basis, "filesort") || !strings.Contains(pc.Basis, "idx_phone") {
		t.Fatalf("依据: %s", pc.Basis)
	}

	// 范围条件：类型 range，行数推不出来 → RowsKnown=false，并提示建好后再 EXPLAIN
	p, ms = metaFor("select * from orders where status = 'a' and created_at >= '2026-01-01'", metaWith(1000000, idxMeta{Name: "PRIMARY", Unique: true, Cols: []string{"id"}, Cards: []int64{1000000}}))
	s = adviseIndex(p, ms)
	pc = buildPlanCompare(s, planOf("ALL", "", "950000", "Using where"), ms.refs[0])
	if pc == nil || pc.After.Type != "range" || pc.After.RowsKnown || !strings.Contains(pc.Basis, "再次 EXPLAIN") || pc.Before.Key != "" {
		t.Fatalf("范围: %+v", pc)
	}

	// 只有排序：index 类型
	p, ms = metaFor("select id from orders order by created_at limit 10", m)
	s = adviseIndex(p, ms)
	pc = buildPlanCompare(s, planOf("index", "PRIMARY", "10", "Using filesort"), ms.refs[0])
	if pc == nil || pc.After.Type != "index" || pc.After.Filesort {
		t.Fatalf("仅排序: %+v", pc)
	}

	// 拿不到执行计划或没有建议：不给对比
	if buildPlanCompare(nil, planOf("ALL", "", "1", ""), ms.refs[0]) != nil {
		t.Fatal("没有建议不应有对比")
	}
	if buildPlanCompare(s, &Table{}, ms.refs[0]) != nil {
		t.Fatal("空执行计划不应有对比")
	}
}

func TestEqCardinality(t *testing.T) {
	m := metaWith(0,
		idxMeta{Name: "a", Cols: []string{"x", "y"}, Cards: []int64{10, 400}},
		idxMeta{Name: "b", Cols: []string{"y"}, Cards: []int64{50}},
		idxMeta{Name: "c", Cols: []string{"z"}, Cards: []int64{9999}}, // 不在等值列里
		idxMeta{Name: "d", Cols: []string{"x"}})                       // 没有基数信息（Cards 与 Cols 不一致）
	card, name, l := eqCardinality(m, []string{"X", "y"})
	if card != 400 || name != "a" || l != 2 {
		t.Fatalf("%d %s %d", card, name, l)
	}
	if card, _, _ = eqCardinality(m, []string{"y"}); card != 50 {
		t.Fatalf("单列: %d", card)
	}
	if card, _, _ = eqCardinality(m, []string{"nope"}); card != 0 {
		t.Fatalf("无: %d", card)
	}
}

// 端到端：fakeDB 的统计信息只有 4 列（没有 cardinality），也要能正常出报告，并带上现有索引清单、冗余提示和执行计划对比。
func TestAdviseReportIncludesIndexExtras(t *testing.T) {
	e := newEnv(t)
	e.db.tables["orders"] = fakeTable{
		cols:    [][3]string{{"id", "bigint", "bigint(20)"}, {"user_id", "bigint", "bigint(20)"}, {"status", "varchar", "varchar(16)"}, {"created_at", "datetime", "datetime"}},
		indexes: [][4]string{{"PRIMARY", "0", "1", "id"}, {"idx_u", "1", "1", "user_id"}, {"idx_us", "1", "1", "user_id"}, {"idx_us", "1", "2", "status"}},
		rows:    "8000000"}
	rep, err := e.svc.Advise(ctx(), alice, AdviseReq{InstanceID: 1, DB: "shop", SQL: "select * from orders where status = 'paid' and created_at >= '2026-01-01'"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ExistingIndexes) != 3 || rep.ExistingIndexes[1].Name != "idx_u" || rep.ExistingIndexes[1].Redundant == "" {
		t.Fatalf("现有索引: %+v", rep.ExistingIndexes)
	}
	var s003, s002 bool
	for _, f := range rep.Findings {
		s003 = s003 || f.ID == "S003" && strings.Contains(f.Title, "idx_u")
		s002 = s002 || f.ID == "S002"
	}
	if !s003 || !s002 {
		t.Fatalf("应有 S002 与 S003: %+v", rep.Findings)
	}
	if len(rep.PlanCompare) != 1 || !rep.PlanCompare[0].Estimated || rep.PlanCompare[0].Before.Type != "ALL" || rep.PlanCompare[0].After.Type != "range" {
		t.Fatalf("执行计划对比: %+v", rep.PlanCompare)
	}
}
