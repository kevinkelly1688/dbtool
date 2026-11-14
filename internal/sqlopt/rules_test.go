// @author: kevinkelly
// @license: Apache Licence
// @file: rules_test.go
// @time: 2026/10/28

package sqlopt

import (
	"strings"
	"testing"
)

func ids(fs []Finding) string {
	var s []string
	for _, f := range fs {
		s = append(s, f.ID)
	}
	return strings.Join(s, ",")
}

func hasID(fs []Finding, id string) bool {
	for _, f := range fs {
		if f.ID == id {
			return true
		}
	}
	return false
}

func TestStaticRules(t *testing.T) {
	cases := []struct {
		name, sql string
		want      []string // 必须命中
		not       []string // 必须不命中
	}{
		{"select_star", "select * from orders where id = 1", []string{"R001"}, []string{"R015", "R003"}},
		{"count_star_ok", "select count(*) from orders", nil, []string{"R001", "R015"}},
		{"update_no_where", "update orders set status = 1", []string{"R002"}, nil},
		{"delete_no_where", "delete from orders", []string{"R002"}, nil},
		{"update_with_where", "update orders set status = 1 where id = 3", nil, []string{"R002"}},
		{"func_on_col", "select id from o where date(created_at) = '2026-10-05'", []string{"R003"}, nil},
		{"func_on_const", "select id from o where created_at >= date_sub(now(), interval 1 day)", nil, []string{"R003"}},
		{"func_on_col_and", "select id from o where status = 1 and lower(name) = 'a'", []string{"R003"}, nil},
		{"leading_wildcard", "select id from u where name like '%abc'", []string{"R004"}, nil},
		{"prefix_like_ok", "select id from u where name like 'abc%'", nil, []string{"R004"}},
		{"concat_wildcard", "select id from u where name like concat('%', ?)", []string{"R004"}, nil},
		{"deep_paging", "select id from o order by id limit 20000, 20", []string{"R005"}, nil},
		{"deep_paging_offset", "select id from o order by id limit 20 offset 500000", []string{"R005"}, nil},
		{"small_paging", "select id from o order by id limit 100, 20", nil, []string{"R005"}},
		{"rand", "select id from o order by rand() limit 3", []string{"R006"}, nil},
		{"or", "select id from o where a = 1 or b = 2", []string{"R007"}, nil},
		{"or_in_subquery_only", "select id from o where a = 1 and b in (select x from t where y = 1 or y = 2)", nil, []string{"R007"}},
		{"neq", "select id from o where status != 1", []string{"R008"}, nil},
		{"ltgt", "select id from o where status <> 1", []string{"R008"}, nil},
		{"not_in", "select id from o where status not in (1,2)", []string{"R008"}, nil},
		{"in_subquery", "select id from o where uid in (select id from u where x = 1)", []string{"R009"}, nil},
		{"insert_no_cols", "insert into t values (1,2)", []string{"R011"}, nil},
		{"insert_cols", "insert into t (a,b) values (1,2)", nil, []string{"R011"}},
		{"insert_select_no_cols", "insert into t select * from s where a = 1", []string{"R011"}, nil},
		{"join_no_on", "select a.id from a join b where a.x = 1", []string{"R012"}, nil},
		{"join_on", "select a.id from a join b on a.id = b.id where a.x = 1", nil, []string{"R012"}},
		{"join_using_left", "select a.id from a left join b using (id) join c on c.id = a.id where a.x = 1", nil, []string{"R012"}},
		{"comma_join", "select a.id from a, b", []string{"R012"}, nil},
		{"comma_join_where", "select a.id from a, b where a.id = b.id", nil, []string{"R012"}},
		{"cross_join_ok", "select a.id from a cross join b where a.x=1", nil, []string{"R012"}},
		{"union", "select a from t1 union select a from t2", []string{"R013"}, nil},
		{"union_all", "select a from t1 union all select a from t2", nil, []string{"R013"}},
		{"null_eq", "select id from o where x = null", []string{"R014"}, nil},
		{"null_neq", "select id from o where x != null", []string{"R014"}, nil},
		{"null_safe_eq", "select id from o where x <=> null", nil, []string{"R014"}},
		{"set_null_ok", "update o set x = null where id = 1", nil, []string{"R014"}},
		{"no_where_no_limit", "select id, name from big", []string{"R015"}, nil},
		{"with_limit", "select id, name from big limit 10", nil, []string{"R015"}},
		{"agg_ok", "select max(id) from big", nil, []string{"R015"}},
		{"limit_no_order", "select id from o where a = 1 limit 10", []string{"R016"}, nil},
		{"limit1_ok", "select id from o where a = 1 limit 1", nil, []string{"R016"}},
		{"clean", "select id, name from o where id = 5", nil, []string{"R001", "R002", "R003", "R015"}},
		{"ddl_quiet", "alter table t add index i (a)", nil, []string{"R001", "R002", "R015"}},
		{"garbage_no_panic", "select from where limit", nil, nil},
		{"truncated_limit", "select a from t where a = 1 limit", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := StaticFindings(c.sql)
			for _, w := range c.want {
				if !hasID(got, w) {
					t.Errorf("应命中 %s，实际 %s", w, ids(got))
				}
			}
			for _, n := range c.not {
				if hasID(got, n) {
					t.Errorf("不应命中 %s，实际 %s", n, ids(got))
				}
			}
		})
	}
}

func TestStaticRulesBigIn(t *testing.T) {
	var b strings.Builder
	b.WriteString("select id from o where id in (")
	for i := 0; i < 300; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("1")
	}
	b.WriteString(")")
	if !hasID(StaticFindings(b.String()), "R010") {
		t.Fatal("300 项的 IN 应命中 R010")
	}
	if hasID(StaticFindings("select id from o where id in (1,2,3)"), "R010") {
		t.Fatal("3 项的 IN 不应命中")
	}
}

func TestScoreAndLevel(t *testing.T) {
	fs := []Finding{{Level: LevelHigh}, {Level: LevelMedium}, {Level: LevelLow}}
	if got := scoreOf(fs); got != 67 {
		t.Fatalf("score=%d", got)
	}
	if highestLevel(fs) != LevelHigh || highestLevel(nil) != "" {
		t.Fatal("highestLevel")
	}
	many := make([]Finding, 10)
	for i := range many {
		many[i].Level = LevelHigh
	}
	if scoreOf(many) != 0 {
		t.Fatal("评分不应为负")
	}
}

func TestFingerprint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT * FROM t WHERE id = 5 AND name = 'bob'", "select * from t where id = ? and name = ?"},
		{"select * from t where id in (1, 2, 3)  -- c", "select * from t where id in (?+)"},
		{"insert into t (a,b) values (1,'x'),(2,'y'),(3,'z');", "insert into t (a,b) values (?+)"},
		{"select a from t limit 20, 10", "select a from t limit ?"},
		{"select a from t limit 10 offset 20", "select a from t limit ?"},
		{"SELECT /* hint */ `A` FROM `T` WHERE x=0x1F", "select `A` from `T` where x=?"},
		{"select 1.5e10, 'it''s'", "select ?, ?"},
		{"select a from t where b = \"str\"", "select a from t where b = ?"},
	}
	for _, c := range cases {
		if got := Fingerprint(c.in); got != c.want {
			t.Errorf("Fingerprint(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	a := FingerprintID(Fingerprint("select * from t where id = 1"))
	b := FingerprintID(Fingerprint("SELECT * FROM t WHERE id = 99"))
	if a != b || len(a) != 16 {
		t.Fatalf("同一类语句应得到同一个指纹 ID: %s %s", a, b)
	}
}

func TestStatementTypeAndTables(t *testing.T) {
	cases := []struct {
		sql, typ string
		tables   string
	}{
		{"select * from a join b on a.id=b.id", TypeSelect, "a,b"},
		{"update shop.orders o set o.x=1 where id=2", TypeUpdate, "shop.orders"},
		{"delete from t where id=1", TypeDelete, "t"},
		{"insert into t (a) select a from s", TypeInsert, "t,s"},
		{"insert ignore into t values (1)", TypeInsert, "t"},
		{"replace into t values (1)", TypeReplace, "t"},
		{"with c as (select 1) select * from c", TypeSelect, "c"},
		{"with c as (select 1) delete from t where id in (select * from c)", TypeDelete, "t,c"},
		{"alter table t add index i(a)", TypeDDL, ""},
		{"set autocommit=1", TypeOther, ""},
		{"select a from t for update", TypeSelect, "t"},
		{"insert into t (a) values (1) on duplicate key update a = 2", TypeInsert, "t"},
		{"(select a from x) union (select a from y)", TypeSelect, "x,y"},
	}
	for _, c := range cases {
		p := parseStmt(c.sql)
		if p.kind != c.typ {
			t.Errorf("%q kind=%s want %s", c.sql, p.kind, c.typ)
		}
		if got := strings.Join(p.tableNames(), ","); got != c.tables {
			t.Errorf("%q tables=%q want %q", c.sql, got, c.tables)
		}
	}
}

func TestTableAliases(t *testing.T) {
	refs := parseStmt("select 1 from orders o join users as u on u.id = o.uid, items i where 1").tableRefs()
	got := map[string]string{}
	for _, r := range refs {
		got[r.Name] = r.Alias
	}
	if got["orders"] != "o" || got["users"] != "u" || got["items"] != "i" {
		t.Fatalf("别名解析：%v", got)
	}
}
