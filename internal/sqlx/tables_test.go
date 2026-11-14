// @author: kevinkelly
// @license: Apache Licence
// @file: tables_test.go
// @time: 2026/10/28

package sqlx

import (
	"reflect"
	"testing"
)

func TestExtractTables(t *testing.T) {
	r := func(s, n string) TableRef { return TableRef{Schema: s, Name: n} }
	cases := []struct {
		sql  string
		want []TableRef
	}{
		{"select a from t", []TableRef{r("", "t")}},
		{"select a from db1.t as x where x.a=1", []TableRef{r("db1", "t")}},
		{"select a from `my db`.`my-t`", []TableRef{r("my db", "my-t")}},
		{"select a from t1, t2 where t1.a=t2.a", []TableRef{r("", "t1"), r("", "t2")}},
		{"select a from t1 a join db2.t2 b on a.id=b.id left join t3 on t3.x=a.x", []TableRef{r("", "t1"), r("db2", "t2"), r("", "t3")}},
		{"select a from (select b from inner_t) x", []TableRef{r("", "inner_t")}},
		{"select a from t where a in (select a from u where b in (select b from v))", []TableRef{r("", "t"), r("", "u"), r("", "v")}},
		{"select a from (t1 join t2 on t1.a=t2.a)", []TableRef{r("", "t1"), r("", "t2")}},
		{"select a from t1 join (t2 join t3 on 1=1) on 1=1", []TableRef{r("", "t1"), r("", "t2"), r("", "t3")}},
		{"select a from t1 use index (idx_a, idx_b), t2", []TableRef{r("", "t1"), r("", "t2")}},
		{"select 1 from dual", nil},
		{"select a, b, c from t order by a, b limit 1", []TableRef{r("", "t")}},
		{"select a from t union select a from u", []TableRef{r("", "t"), r("", "u")}},
		{"select a from t1, lateral (select b from t2) x", []TableRef{r("", "t1"), r("", "t2")}},
		{"with c as (select a from base) select a from c", []TableRef{r("", "base"), r("", "c")}},
		{"select 'from secret' as x", nil},
		{"select a from t -- from hidden", []TableRef{r("", "t")}},
	}
	for _, c := range cases {
		if got := ExtractTables(c.sql); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ExtractTables(%q)=%v, want %v", c.sql, got, c.want)
		}
	}
}

func TestDangerousAndShow(t *testing.T) {
	if got := DangerousFunctions("select load_file('/etc/passwd'), SLEEP(5), a from t"); !reflect.DeepEqual(got, []string{"load_file", "sleep"}) {
		t.Errorf("DangerousFunctions=%v", got)
	}
	if len(DangerousFunctions("select 'sleep(1)' as sleep, a from t")) != 0 {
		t.Error("字符串/别名不应命中")
	}
	for s, want := range map[string]bool{
		"show databases": true, "show tables": true, "show full tables from db": true, "show create table t": true,
		"show columns from t": true, "show index from t": true, "show table status": true, "show full columns from t": true,
		"show processlist": false, "show full processlist": false, "show grants": false, "show master status": false,
		"show engine innodb status": false, "show create user root": false, "show variables": false, "select 1": false,
	} {
		if got := IsSafeShow(s); got != want {
			t.Errorf("IsSafeShow(%q)=%v, want %v", s, got, want)
		}
	}
}
