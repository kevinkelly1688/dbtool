// @author: kevinkelly
// @license: Apache Licence
// @file: query_rules_test.go
// @time: 2026/10/28

package sqlx

import "testing"

func TestWithMainKeyword(t *testing.T) {
	cases := []struct{ in, want string }{
		{"with a as (select 1) select * from a", "select"},
		{"WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<5) SELECT x FROM n", "select"},
		{"with a as (select 1), b as (select 2) select * from a join b", "select"},
		{"with a as (select 1) delete from t where id in (select * from a)", "delete"},
		{"with a as (select 1) update t set c=1", "update"},
		{"with a as (select 1) insert into t select * from a", "insert"},
		{"/*x*/ with `update` as (select 1) select 1", "select"},
		{"select 1", ""},
		{"with a as (select 1)", ""},
	}
	for _, c := range cases {
		if got := WithMainKeyword(c.in); got != c.want {
			t.Errorf("WithMainKeyword(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestHasWriteVerb(t *testing.T) {
	yes := []string{
		"with a as (select 1) delete from t",
		"select 1; update t set a=1",
		"select * from (select 1) x union insert into t values (1)",
		"replace into t values (1)",
	}
	no := []string{
		"with a as (select replace(name,'a','b') from t) select * from a",
		"select insert('abc',1,1,'x')",
		"select * from t where note = 'update me' and `delete` = 1",
		"select * from t for update",
		"insert into t values (1) on duplicate key update a=1", // 本身含 insert，这里不会走到“no”
	}
	for _, s := range yes {
		if !HasWriteVerb(s) {
			t.Errorf("HasWriteVerb(%q) 应为 true", s)
		}
	}
	for _, s := range no[:4] {
		if HasWriteVerb(s) {
			t.Errorf("HasWriteVerb(%q) 应为 false", s)
		}
	}
	if !HasWriteVerb(no[4]) {
		t.Errorf("insert … on duplicate key update 应为 true")
	}
}

func TestExplainAnalyze(t *testing.T) {
	yes := []string{
		"explain analyze select 1",
		"EXPLAIN ANALYZE FORMAT=TREE SELECT * FROM t",
		"explain format=tree analyze select 1",
		"desc analyze select 1",
		"describe analyze select * from t",
		"/* c */ explain /* x */ analyze select 1",
	}
	no := []string{
		"explain select 1",
		"explain format=json select analyze from t",
		"explain select * from analyze",
		"select 'explain analyze'",
		"desc orders",
		"explain delete from t where id=1",
	}
	for _, s := range yes {
		if !ExplainAnalyze(s) {
			t.Errorf("ExplainAnalyze(%q) 应为 true", s)
		}
	}
	for _, s := range no {
		if ExplainAnalyze(s) {
			t.Errorf("ExplainAnalyze(%q) 应为 false", s)
		}
	}
}

func TestQueryKind(t *testing.T) {
	cases := []struct{ in, want string }{
		{"select 1", "select"},
		{"WITH a AS (SELECT 1) SELECT * FROM a", "select"},
		{"show tables", "show"},
		{"explain select 1", "explain"},
		{"desc orders", "show"},
		{"DESCRIBE `order items`", "show"},
		{"desc shop.orders id", "show"},
		{"desc select 1", "explain"},
		{"describe analyze select 1", "explain"},
		{"desc format=json select 1", "explain"},
		{"desc", "show"},
		{"update t set a=1", "update"},
	}
	for _, c := range cases {
		if got := QueryKind(c.in); got != c.want {
			t.Errorf("QueryKind(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestTouchesMySQLSchema(t *testing.T) {
	yes := []struct{ sql, db string }{
		{"select * from mysql.user", "shop"},
		{"select * from `mysql`.`db`", "shop"},
		{"select * from mysql . proc", "shop"},
		{"select host from mysql.user_x where 1", ""},
		{"show tables from mysql", "shop"},
		{"show tables in `mysql`", "shop"},
		{"desc mysql.user", "shop"},
		{"select * from user", "mysql"},
		{"select * from t1 join t2 on t1.a=t2.a", "MySQL"},
		{"show tables", "mysql"},
		{"desc user", "mysql"},
	}
	no := []struct{ sql, db string }{
		{"select * from information_schema.tables", "shop"},
		{"select * from performance_schema.threads", "shop"},
		{"select * from user", "shop"},
		{"select 'mysql.user'", "shop"},
		{"select 1", "mysql"},
		{"show databases", "mysql"},
		{"select * from shop.mysql_log", "shop"},
		{"select mysql_col from t", "shop"},
	}
	for _, c := range yes {
		if !TouchesMySQLSchema(c.sql, c.db) {
			t.Errorf("TouchesMySQLSchema(%q,%q) 应为 true", c.sql, c.db)
		}
	}
	for _, c := range no {
		if TouchesMySQLSchema(c.sql, c.db) {
			t.Errorf("TouchesMySQLSchema(%q,%q) 应为 false", c.sql, c.db)
		}
	}
}
