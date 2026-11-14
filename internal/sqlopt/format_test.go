// @author: kevinkelly
// @license: Apache Licence
// @file: format_test.go
// @time: 2026/10/28

package sqlopt

import (
	"strings"
	"testing"
)

func TestPrettySQL(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			"select a,b from t where x=1 and y in (select z from u where k=2) order by a limit 10",
			"SELECT a,\n  b\nFROM t\nWHERE x=1\n  AND y IN (\n    SELECT z\n    FROM u\n    WHERE k=2\n  )\nORDER BY a\nLIMIT 10",
		},
		{
			"select count(*) from t where d between 1 and 5 and e = 'a  b'",
			"SELECT count(*)\nFROM t\nWHERE d BETWEEN 1 AND 5\n  AND e = 'a  b'",
		},
		{
			"select a from t1 left join t2 on t1.id=t2.id inner join t3 on t3.id = t1.id",
			"SELECT a\nFROM t1\nLEFT JOIN t2 ON t1.id=t2.id\nINNER JOIN t3 ON t3.id = t1.id",
		},
		{
			"update t set a=1,b=2 where id=3",
			"UPDATE t\nSET a=1,\n  b=2\nWHERE id=3",
		},
		{
			"select 1; select 2",
			"SELECT 1;\n\nSELECT 2",
		},
		{
			"select a -- note\nfrom t",
			"SELECT a -- note\nFROM t",
		},
		{
			"insert into t (a,b) values (1,2) on duplicate key update a=3",
			"INSERT INTO t (a, b)\nVALUES (1, 2)\nON DUPLICATE KEY UPDATE a=3",
		},
	}
	for _, c := range cases {
		got := PrettySQL(c.in)
		if got != c.want {
			t.Errorf("PrettySQL(%q)\n got:\n%s\nwant:\n%s", c.in, got, c.want)
		}
	}
}

func TestFormatKeepsStringsAndIsStable(t *testing.T) {
	in := "select `a b`, \"x;y\", 'it''s; ok' from t where c = ';' /* ; */"
	out := PrettySQL(in)
	for _, keep := range []string{"`a b`", "\"x;y\"", "'it''s; ok'", "';'", "/* ; */"} {
		if !strings.Contains(out, keep) {
			t.Errorf("丢失 %s：\n%s", keep, out)
		}
	}
	if again := PrettySQL(out); again != out {
		t.Errorf("格式化应当幂等：\n%s\n----\n%s", out, again)
	}
	if PrettySQL("") != "" || PrettySQL("   ") != "" {
		t.Error("空输入应得到空串")
	}
}
