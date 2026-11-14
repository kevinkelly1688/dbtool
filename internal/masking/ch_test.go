// @author: kevinkelly
// @license: Apache Licence
// @file: ch_test.go
// @time: 2026/10/28

package masking

import "testing"

// ClickHouse 的 WITH <表达式> AS <别名> SELECT …：别名取表达式的值，污染要传给它。
func TestConservativeHitsClickHouseWithAlias(t *testing.T) {
	keep := &Rule{Kind: KindKeep, Prefix: 3, Suffix: 4}
	cfg := []Column{{Schema: "*", Table: "*", Column: "phone", Rule: keep}}
	cases := []struct {
		name string
		sql  string
		cols []string
		want map[int]*Rule
	}{
		{"字段起别名", "WITH phone AS p SELECT p, id FROM app.users", []string{"p", "id"}, map[int]*Rule{0: keep}},
		{"函数表达式起别名，多个别名", "WITH toString(phone) AS p, 1 AS one SELECT p, one FROM t", []string{"p", "one"}, map[int]*Rule{0: keep}},
		{"标量子查询起别名", "WITH (SELECT phone FROM t LIMIT 1) AS x SELECT x", []string{"x"}, map[int]*Rule{0: keep}},
		{"子查询里的 WITH", "SELECT * FROM (WITH phone AS p SELECT p FROM u)", []string{"p"}, map[int]*Rule{0: keep}},
		{"UNION ALL 后面的 WITH", "SELECT 1 AS a UNION ALL WITH phone AS a SELECT a", []string{"a"}, map[int]*Rule{0: keep}},
		{"标准的 CTE 仍然认得", "WITH t AS (SELECT phone AS p FROM u) SELECT p FROM t", []string{"p"}, map[int]*Rule{0: keep}},
		{"别名取的不是敏感字段不误伤", "WITH id + 1 AS n SELECT n FROM t", []string{"n"}, map[int]*Rule{}},
		{"WITH TOTALS / WITH TIES / WITH FILL 不是别名", "SELECT a, count() FROM t GROUP BY a WITH TOTALS ORDER BY a WITH FILL LIMIT 5 WITH TIES", []string{"a", "count()"}, map[int]*Rule{}},
	}
	for _, c := range cases {
		got := map[int]*Rule{}
		for _, h := range ConservativeHits(c.sql, c.cols, cfg, DialectClickHouse) {
			got[h.Index] = h.Rule
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: 命中 %v，期望 %v", c.name, got, c.want)
			continue
		}
		for i, r := range c.want {
			if got[i] != r {
				t.Errorf("%s: 第 %d 列规则 %v，期望 %v", c.name, i, got[i], r)
			}
		}
	}
	// 视图定义里用这种写法：展开后外层语句引用别名也脱敏
	hits := ConservativeHitsViews("select p, id from v", []string{"p", "id"}, cfg, DialectClickHouse,
		[]ViewBody{{Body: "WITH phone AS p SELECT p, id FROM app.users"}})
	if len(hits) != 1 || hits[0].Index != 0 || hits[0].Rule != keep {
		t.Errorf("视图定义里的 WITH 别名: %+v", hits)
	}
}
