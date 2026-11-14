// @author: kevinkelly
// @license: Apache Licence
// @file: lex_test.go
// @time: 2026/10/28

package sqlx

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"select 1", []string{"select 1"}},
		{"select 1; select 2;", []string{"select 1", "select 2"}},
		{"select ';' as a; select \"x;y\"", []string{"select ';' as a", "select \"x;y\""}},
		{"-- 注释;\nselect 1 /* ; */ ;", []string{"select 1"}},
		{"select `a;b` from t;;", []string{"select `a;b` from t"}},
		{"  \n ; ", nil},
		{"select 'it''s;ok'; select 2", []string{"select 'it''s;ok'", "select 2"}},
		{"select 'a\\';b'; select 2", []string{"select 'a\\';b'", "select 2"}},
	}
	for _, c := range cases {
		got := Split(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Split(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestHasSelectStar(t *testing.T) {
	yes := []string{
		"select * from t", "SELECT\n *\nFROM t", "select t.* from t", "select a, * from t",
		"select distinct * from t", "select a from (select * from t) x", "select /*c*/ * from t",
	}
	no := []string{
		"select count(*) from t", "select a*b from t", "select 2 * 3", "select a, b from t where c = '*'",
		"select 'select *'", "select `*` from t", "select a from t -- select *",
	}
	for _, s := range yes {
		if !HasSelectStar(s) {
			t.Errorf("应判定含通配列: %q", s)
		}
	}
	for _, s := range no {
		if HasSelectStar(s) {
			t.Errorf("不应判定含通配列: %q", s)
		}
	}
}

func TestRewriteLimit(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"select a from t", 100, "select a from t LIMIT 100"},
		{"select a from t;", 100, "select a from t LIMIT 100"},
		{"select a from t limit 10", 100, "select a from t LIMIT 10"},
		{"select a from t limit 500", 100, "select a from t LIMIT 100"},
		{"select a from t limit 20, 500", 100, "select a from t LIMIT 20,100"},
		{"select a from t limit 500 offset 7", 100, "select a from t LIMIT 100 OFFSET 7"},
		{"select a from (select b from u limit 5) x", 100, "select a from (select b from u limit 5) x LIMIT 100"},
		{"select 'limit 5' as s from t", 100, "select 'limit 5' as s from t LIMIT 100"},
		{"select a from t -- limit 3", 100, "select a from t LIMIT 100"},
		{"select a from t", 0, "select a from t"},
	}
	for _, c := range cases {
		if got := RewriteLimit(c.in, c.max); got != c.want {
			t.Errorf("RewriteLimit(%q,%d)=%q, want %q", c.in, c.max, got, c.want)
		}
	}
}

func TestMisc(t *testing.T) {
	if FirstKeyword("  /*x*/ SeLeCt 1") != "select" {
		t.Error("FirstKeyword")
	}
	if !ContainsSequence("select a from t for\n update", "for", "update") {
		t.Error("for update")
	}
	if ContainsSequence("select 'for update'", "for", "update") {
		t.Error("字符串里的 for update 不应命中")
	}
	if !TouchesMySQLUser("select * from `mysql`.`user`", "x") || !TouchesMySQLUser("select * from user", "mysql") {
		t.Error("mysql.user")
	}
	if TouchesMySQLUser("select * from user", "shop") {
		t.Error("shop.user 不应命中")
	}
}
