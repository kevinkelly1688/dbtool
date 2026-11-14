// @author: kevinkelly
// @license: Apache Licence
// @file: fingerprint_test.go
// @time: 2026/10/28

package slowlog

import (
	"crypto/md5"
	"encoding/hex"
	"testing"
)

func TestFingerprint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT * FROM Orders WHERE id = 42", "select * from orders where id=?"},
		{"select * from orders where id=7", "select * from orders where id=?"},
		{"select a, b from t where name = 'bob' and age > 18", "select a,b from t where name=? and age>?"},
		{`select * from t where s = "x\"y" and n = 1.5`, "select * from t where s=? and n=?"},
		{"select * from t where id in (1, 2, 3, 4)", "select * from t where id in(?)"},
		{"insert into t (a, b) values (1, 'x'), (2, 'y'), (3, 'z')", "insert into t(a,b)values(?)"},
		{"select /* hint */ 1 -- trailing\n from dual", "select ? from dual"},
		{"  select\n\t*\nfrom   t   where  a  =  'it''s'  ", "select * from t where a=??"}, // 双写引号不特殊处理
		{"select * from t1 where c2 = 3", "select * from t1 where c2=?"},                   // 标识符里的数字不替换
		{"", ""},
		{"   ", ""},
		{"-- 只有注释", ""},
	}
	for _, c := range cases {
		if got := Fingerprint(c.in); got != c.want {
			t.Errorf("Fingerprint(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestSQLMD5(t *testing.T) {
	a, b := SQLMD5("select * from t where id = 1"), SQLMD5("SELECT *  FROM t WHERE id=999")
	if a == "" || a != b || len(a) != 32 {
		t.Errorf("只差字面量与格式的 SQL 应得到同一个 MD5: %q %q", a, b)
	}
	sum := md5.Sum([]byte("select * from t where id=?"))
	if a != hex.EncodeToString(sum[:]) {
		t.Errorf("MD5 应是指纹字符串的 MD5: %s", a)
	}
	if SQLMD5("select * from u where id = 1") == a {
		t.Error("不同表应有不同指纹")
	}
	if SQLMD5("") != "" || SQLMD5("/* x */") != "" {
		t.Error("空指纹应返回空串（写库时为 NULL）")
	}
}

func TestExtractTable(t *testing.T) {
	for in, want := range map[string]string{
		"select * from orders o where a=1":    "orders",
		"select * from shop.orders":           "orders",
		"SELECT 1 FROM `orders`":              "orders",
		"insert into logs (a) values (1)":     "logs",
		"update t_user set a=1":               "t_user",
		"select * from a join b on a.id=b.id": "a",
		"select 1":                            "",
		"":                                    "",
		"delete from `db`.`t` where 1":        "db",
	} {
		if got := ExtractTable(in); got != want {
			t.Errorf("ExtractTable(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestClipText(t *testing.T) {
	if s, cut := clipText("abc"); s != "abc" || cut {
		t.Error("短文本")
	}
	long := make([]rune, maxTextRunes+5)
	for i := range long {
		long[i] = '中'
	}
	if s, cut := clipText(string(long)); !cut || len([]rune(s)) != maxTextRunes {
		t.Error("按字符数截断")
	}
}
