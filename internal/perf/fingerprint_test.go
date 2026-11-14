// @author: kevinkelly
// @license: Apache Licence
// @file: fingerprint_test.go
// @time: 2026/10/28

package perf

import (
	"strings"
	"testing"
)

func TestFingerprint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT * FROM t WHERE id = 1", "select * from t where id=?"},
		{"select a from t where name='bob' and age>18;", "select a from t where name=? and age>?"},
		{"SELECT * FROM t WHERE id IN (1, 2, 3)", "select * from t where id in(?+)"},
		{"select * from t where id in ( 'a' , 'b' )", "select * from t where id in(?+)"},
		{"insert into t(a,b) values (1,'x'),(2,'y')", "insert into t(a,b) values(?+)"},
		{"REPLACE INTO t (a) VALUES (1)", "replace into t (a) values(?+)"},
		{"select * from t where a = 1 /* c */ -- x\n and b = 2", "select * from t where a=? and b=?"},
		{"select * from t where a = 1 # tail", "select * from t where a=?"},
		{"SELECT * FROM t LIMIT 20000, 20", "select * from t limit ?"},
		{"select * from t limit 10", "select * from t limit ?"},
		{"select * from t1 where c2=3", "select * from t1 where c2=?"},
		{"select '#abc' , \"a--b\" from t", "select ?,? from t"},
		{"select * from t where a=0xFF and b = 'it''s' and c = 'a\\'b'", "select * from t where a=? and b=? and c=?"},
		{"select * from t where a = -5 and b > +3.5e2", "select * from t where a=? and b>?"},
		{"select * from t where price > 12.50", "select * from t where price>?"},
		{"select `a1`, b.2 from `t2`", "select `a1`,b.2 from `t2`"},
		{"select /*+ MAX_EXECUTION_TIME(1000) */ * from t where a = 1", "select /*+ max_execution_time(?) */ * from t where a=?"},
		{"  SELECT   *\n\tFROM  t  WHERE a  >=  1  AND b <=> 2  ", "select * from t where a>=? and b<=>?"},
		{"select ( a + 1 ) from t", "select (a + ?) from t"},
		{"", ""},
	}
	for _, c := range cases {
		if got := Fingerprint(c.in); got != c.want {
			t.Errorf("Fingerprint(%q)\n got  %q\n want %q", c.in, got, c.want)
		}
	}
	// 只差字面值与格式的 SQL 指纹相同
	a := Fingerprint("SELECT * FROM t WHERE id = 1 AND name = 'x'")
	b := Fingerprint("select *   from t where id=99 and name='yy'")
	if a != b || Checksum(a) != Checksum(b) || len(Checksum(a)) != 32 {
		t.Fatalf("指纹应一致: %q %q", a, b)
	}
	if Checksum("a") == Checksum("b") {
		t.Fatal("不同指纹校验和应不同")
	}
	// 已知答案：指纹字符串的 MD5 十六进制摘要
	if got := Checksum("select * from t where id=?"); got != "74f34faaab403184c4c6dc6695c38a4c" {
		t.Fatalf("checksum 格式: %s", got)
	}
}

// 已知答案：数字紧挨汉字/全角数字/科学计数法的回溯边界。
func TestFingerprintPythonParity(t *testing.T) {
	cases := []struct{ in, want string }{
		{"select id from t where name = '中文' and id = 1数字 and c > '2026-02-02'", "select id from t where name=? and id=1数字 and c>?"},
		{"select * from t where a=1数字 and b=2", "select * from t where a=1数字 and b=?"},
		{"select 1数 from t where x = 1.5数 and y = 2e3数 and z = 3e5", "select 1数 from t where x=?.5数 and y=2e3数 and z=?"},
		{"select * from t where id in (1,2,3) and 名字in (4,5)", "select * from t where id in(?+) and 名字in (?,?)"},
		{"select col1, col2 from t1 where c2 = 12ab and d = 3.14.15 and e = -7", "select col1,col2 from t1 where c2=12ab and d=?.15 and e=?"},
		{"select * from t where a = １２３ and b = 5", "select * from t where a=? and b=?"},
		{"select * from t where a=1e 5 and b = 1e+5x and c = 1E+5", "select * from t where a=1e ? and b=1e+5x and c=?"},
		{"select 价格-1 from t where v = 价格+2 and w=a1 and n= 7中", "select 价格-? from t where v=价格+? and w=a1 and n=7中"},
		{"select * from t limit 10, 20", "select * from t limit ?"},
		{"insert into t values (1,'a'),(2,'b')", "insert into t values(?+)"},
		{"select a.b1, `x2`, t.3 from t where q = 1.e5", "select a.b1,`x2`,t.3 from t where q=?.e5"},
		{"select * from t where id=1中 or id=2 or 数字3=4", "select * from t where id=1中 or id=? or 数字3=?"},
	}
	for _, c := range cases {
		if got := Fingerprint(c.in); got != c.want {
			t.Errorf("Fingerprint(%q)\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}

func TestExtractTables(t *testing.T) {
	got := ExtractTables("SELECT * FROM `shop`.`Orders` o JOIN order_item i ON o.id=i.oid, other WHERE 1 UPDATE t3 SET a=1 INSERT INTO t4 VALUES(1) FROM (select 1) x", 10)
	if strings.Join(got, ",") != "shop.orders,order_item,t3,t4" {
		t.Fatalf("%v", got)
	}
	if got = ExtractTables("select * from a join b join c join d", 2); len(got) != 2 {
		t.Fatalf("limit: %v", got)
	}
	if got = ExtractTables("select 1 from dual", 5); len(got) != 0 {
		t.Fatalf("dual: %v", got)
	}
}
