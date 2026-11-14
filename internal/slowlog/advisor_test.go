// @author: kevinkelly
// @license: Apache Licence
// @file: advisor_test.go
// @time: 2026/10/28

package slowlog

import (
	"context"
	"strings"
	"testing"
)

func codes(a *Advice) string {
	var c []string
	for _, it := range a.Items {
		c = append(c, it.Code)
	}
	return strings.Join(c, ",")
}

func advise(t *testing.T, req AdviseRequest) *Advice {
	t.Helper()
	a, err := RuleAdvisor{}.Advise(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRuleAdvisorFindings(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string // 必须出现的规则码
		not  []string // 不能出现的规则码
	}{
		{"select star", "select * from orders where id = 1", []string{"select_star"}, []string{"no_where"}},
		{"count(*) 不算", "select count(*) from orders where status = 1", nil, []string{"select_star"}},
		{"无 where", "select id from orders", []string{"no_where"}, nil},
		{"无 where 但有 limit", "select id from orders limit 10", nil, []string{"no_where"}},
		{"delete 无 where", "delete from orders", []string{"no_where_write"}, nil},
		{"update 无 where", "update orders set a = 1", []string{"no_where_write"}, nil},
		{"update 有 where", "update orders set a = 1 where id = 2", nil, []string{"no_where_write"}},
		{"前导通配符", "select id from t where name like '%abc'", []string{"leading_wildcard"}, nil},
		{"前缀匹配没问题", "select id from t where name like 'abc%'", nil, []string{"leading_wildcard"}},
		{"列上套函数", "select id from t where date(created_at) = '2026-01-01'", []string{"func_on_column"}, nil},
		{"常量上的函数没问题", "select id from t where created_at > date(now())", nil, []string{"func_on_column"}},
		{"or", "select id from t where a = 1 or b = 2", []string{"or_condition"}, nil},
		{"否定", "select id from t where a != 1", []string{"negative_condition"}, nil},
		{"not in", "select id from t where a not in (1,2)", []string{"negative_condition"}, nil},
		{"order by rand", "select id from t where a = 1 order by rand() limit 1", []string{"order_rand"}, []string{"order_no_limit"}},
		{"排序无 limit", "select id from t where a = 1 order by b", []string{"order_no_limit"}, nil},
		{"深分页 逗号", "select id from t where a = 1 order by id limit 500000, 20", []string{"deep_pagination"}, nil},
		{"深分页 offset", "select id from t where a = 1 order by id limit 20 offset 100000", []string{"deep_pagination"}, nil},
		{"浅分页", "select id from t where a = 1 order by id limit 100, 20", nil, []string{"deep_pagination"}},
		{"占位符分页不判断", "select id from t where a=? order by id limit ?,?", nil, []string{"deep_pagination"}},
		{"in 子查询", "select id from t where a in (select a from u)", []string{"in_subquery"}, nil},
		{"union", "select a from t where b=1 union select a from u where c=2", []string{"union_distinct"}, nil},
		{"union all 没问题", "select a from t where b=1 union all select a from u where c=2", nil, []string{"union_distinct"}},
		{"非 DML", "show create table t", []string{"not_dml"}, nil},
		{"干净的语句", "select id, name from t where id = 1", []string{"no_findings"}, []string{"select_star", "no_where"}},
	}
	for _, c := range cases {
		a := advise(t, AdviseRequest{SQL: c.sql})
		got := codes(a)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: 应包含 %s，实际 %s", c.name, w, got)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("%s: 不应包含 %s，实际 %s", c.name, n, got)
			}
		}
		if a.Source != "rule" || a.Label == "" {
			t.Errorf("%s: 来源: %+v", c.name, a)
		}
	}
}

func TestRuleAdvisorBigInListAndSeverityOrder(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("select id from t where id in (")
	for i := 0; i < 300; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("1")
	}
	sb.WriteString(")")
	if got := codes(advise(t, AdviseRequest{SQL: sb.String()})); !strings.Contains(got, "big_in_list") {
		t.Errorf("超长 IN: %s", got)
	}
	if got := codes(advise(t, AdviseRequest{SQL: "select id from t where id in (1,2,3)"})); strings.Contains(got, "big_in_list") {
		t.Errorf("短 IN: %s", got)
	}
	// 严重程度排序：crit 在 warn 前，warn 在 info 前
	a := advise(t, AdviseRequest{SQL: "select * from t order by rand()"})
	last := -1
	for _, it := range a.Items {
		if r := levelRank[it.Level]; r < last {
			t.Errorf("没有按严重程度排序: %s", codes(a))
		} else {
			last = r
		}
	}
	if a.Items[0].Level != "crit" {
		t.Errorf("order by rand 应排第一: %s", codes(a))
	}
}

func TestRuleAdvisorMissingIndexHint(t *testing.T) {
	req := AdviseRequest{SQL: "select id, amount from orders where status = 'paid' and user_id = 7 and created_at > '2026-01-01' order by id limit 20",
		RowsExamined: 9821004, RowsSent: 20, QueryTime: 4.2}
	a := advise(t, req)
	var item *AdviceItem
	for i := range a.Items {
		if a.Items[i].Code == "missing_index" {
			item = &a.Items[i]
		}
	}
	if item == nil || item.Level != "crit" {
		t.Fatalf("扫描/返回比很大应提示缺索引: %s", codes(a))
	}
	// 等值列在前、范围列在后
	if !strings.Contains(item.Detail, "表 orders") || !strings.Contains(item.Detail, "(status, user_id, created_at)") {
		t.Errorf("候选索引: %s", item.Detail)
	}
	// 多表语句不给列推测（无法确定列属于哪张表），但仍提示
	a = advise(t, AdviseRequest{SQL: "select o.id from orders o join users u on u.id = o.uid where o.status = 1", RowsExamined: 500000, RowsSent: 10})
	for _, it := range a.Items {
		if it.Code == "missing_index" && strings.Contains(it.Detail, "复合索引") {
			t.Errorf("多表不应推测索引: %s", it.Detail)
		}
	}
	if !strings.Contains(codes(a), "missing_index") {
		t.Errorf("多表也应提示缺索引迹象: %s", codes(a))
	}
	// 扫描行数不大或比例正常：不提示
	for _, r := range []AdviseRequest{
		{SQL: "select id from t where a = 1", RowsExamined: 500, RowsSent: 1},
		{SQL: "select id from t where a = 1", RowsExamined: 100000, RowsSent: 90000},
	} {
		if got := codes(advise(t, r)); strings.Contains(got, "missing_index") {
			t.Errorf("%+v 不应提示缺索引: %s", r, got)
		}
	}
	if got := codes(advise(t, AdviseRequest{SQL: "select id from t where a = 1", QueryTime: 12})); !strings.Contains(got, "long_query") {
		t.Errorf("耗时很长: %s", got)
	}
}

func TestRuleAdvisorInput(t *testing.T) {
	if _, err := (RuleAdvisor{}).Advise(context.Background(), AdviseRequest{SQL: "  ;  -- 只有注释\n"}); err == nil {
		t.Error("空语句应报错")
	}
	// 多条语句只分析第一条；注释不影响分析
	a := advise(t, AdviseRequest{SQL: "/* hint */ select * from t where id = 1; delete from t"})
	if got := codes(a); !strings.Contains(got, "select_star") || strings.Contains(got, "no_where_write") {
		t.Errorf("只分析第一条: %s", got)
	}
	// 字符串里的关键字不能误判
	a = advise(t, AdviseRequest{SQL: "select id from t where note = 'select * from x where a or b' and id = 1"})
	if got := codes(a); strings.Contains(got, "select_star") || strings.Contains(got, "or_condition") {
		t.Errorf("字符串内容误判: %s", got)
	}
}
