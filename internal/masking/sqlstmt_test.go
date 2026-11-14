// @author: kevinkelly
// @license: Apache Licence
// @file: sqlstmt_test.go
// @time: 2026/10/28

package masking

import (
	"strings"
	"testing"
)

func cfgCols() []Column {
	return []Column{
		{Schema: "shop", Table: "users", Column: "password", Rule: &Rule{Kind: KindWhole}},
		{Schema: "shop", Table: "users", Column: "phone", Rule: &Rule{Kind: KindKeep, Prefix: 3, Suffix: 4}},
		{Schema: "*", Table: "*", Column: "id_card", Rule: &Rule{Kind: KindKeep, Prefix: 3, Suffix: 4}},
	}
}

func maskIt(t *testing.T, d Dialect, db, sql string) (string, []string) {
	t.Helper()
	return NewStmtMasker(d, db, cfgCols(), nil).Mask(sql)
}

func TestMaskStatementsUpdateWhere(t *testing.T) {
	cases := []struct{ in, want string }{
		// 用户给的例子：只遮盖配置字段的值，userid 不动
		{"update users set password='345345345' and userid=9182", "update users set password=*** and userid=9182"},
		{"UPDATE users SET password = 'abc123', nick = 'bob' WHERE id = 7", "UPDATE users SET password = ***, nick = 'bob' WHERE id = 7"},
		{"update shop.users set phone='13812345678' where id=1", "update shop.users set phone=138***5678 where id=1"},
		{"delete from users where password = 'x1y2z3' and id = 5", "delete from users where password = *** and id = 5"},
		{"update users set nick='a' where password in ('p1111','p2222') and id=1", "update users set nick='a' where password in (***,***) and id=1"},
		{"update users set nick='a' where password between 'aaaa' and 'zzzz'", "update users set nick='a' where password between *** and ***"},
		{"update users set nick='a' where password like 'ab%cd'", "update users set nick='a' where password like ***"},
		{"update users u set u.password=MD5('secret1') where u.id=1", "update users u set u.password=MD5(***) where u.id=1"},
		{"update users set nick='a' where 'p9999' = password", "update users set nick='a' where *** = password"},
		// 数字值
		{"update users set phone=13812345678 where id=1", "update users set phone=138***5678 where id=1"},
		// 同名字段（任意库表）
		{"update other.t set id_card='110101199003071234' where id=1", "update other.t set id_card=110***1234 where id=1"},
		// 没有配置的字段 / 表：不动
		{"update orders set password='keepme' where id=1", "update orders set password='keepme' where id=1"},
		{"update users set nick='a' where id=1", "update users set nick='a' where id=1"},
		// 注释与字符串里的字样不算
		{"update users set nick='password=''x''' where id=1 -- password='y'", "update users set nick='password=''x''' where id=1 -- password='y'"},
	}
	for _, c := range cases {
		got, _ := maskIt(t, DialectMySQL, "shop", c.in)
		if got != c.want {
			t.Errorf("\n输入：%s\n期望：%s\n实际：%s", c.in, c.want, got)
		}
	}
}

func TestMaskStatementsInsertReplace(t *testing.T) {
	cases := []struct{ in, want string }{
		{"insert into users (id, password, nick) values (1, 'pw123456', 'bob'), (2, 'pw654321', 'amy')",
			"insert into users (id, password, nick) values (1, ***, 'bob'), (2, ***, 'amy')"},
		{"REPLACE INTO shop.users(phone,nick) VALUES ('13812345678','x')", "REPLACE INTO shop.users(phone,nick) VALUES (138***5678,'x')"},
		// 没有字段列表：这张表的 VALUES 全部遮盖
		{"insert into users values (1, 'pw123456', 'bob')", "insert into users values (***, ***, ***)"},
		// 别的表不动
		{"insert into orders (id, password) values (1, 'visible')", "insert into orders (id, password) values (1, 'visible')"},
		{"insert into users set password='abcd1234', nick='n'", "insert into users set password=***, nick='n'"},
		{"insert into users (id,password) values (1,'old') on duplicate key update password='newpass1'",
			"insert into users (id,password) values (1,***) on duplicate key update password=***"},
	}
	for _, c := range cases {
		got, _ := maskIt(t, DialectMySQL, "shop", c.in)
		if got != c.want {
			t.Errorf("\n输入：%s\n期望：%s\n实际：%s", c.in, c.want, got)
		}
	}
}

func TestMaskStatementsMultiAndUse(t *testing.T) {
	sql := "use shop;\nupdate users set password='aaaa1111' where id=1;\nupdate users set nick='x' where id=2;\n"
	got, secrets := maskIt(t, DialectMySQL, "", sql)
	want := "use shop;\nupdate users set password=*** where id=1;\nupdate users set nick='x' where id=2;\n"
	if got != want {
		t.Errorf("\n期望：%q\n实际：%q", want, got)
	}
	if len(secrets) != 1 || secrets[0] != "aaaa1111" {
		t.Errorf("被遮盖的原值: %v", secrets)
	}
	// 错误信息里回显的值也要清掉
	if s := Scrub("Duplicate entry 'aaaa1111' for key 'uk'", secrets); strings.Contains(s, "aaaa1111") || !strings.Contains(s, "***") {
		t.Errorf("Scrub: %s", s)
	}
	// 默认库不是配置里的库：不命中
	got, _ = maskIt(t, DialectMySQL, "other", "update users set password='keep' where id=1")
	if got != "update users set password='keep' where id=1" {
		t.Errorf("别的库的同名表不应被遮盖: %s", got)
	}
}

func TestMaskStatementsFailClosed(t *testing.T) {
	// 认不出“字段 = 值”（CASE）：这条语句里的字面量全部遮盖
	in := "update users set password = case when id = 1 then 'p1111' else 'p2222' end where id in (1,2)"
	got, _ := maskIt(t, DialectMySQL, "shop", in)
	if strings.Contains(got, "p1111") || strings.Contains(got, "p2222") {
		t.Errorf("CASE 里的值必须被遮盖: %s", got)
	}
	// IS NULL 不算泄露，也不触发整句遮盖
	in = "delete from users where password is null and id = 5"
	if got, _ := maskIt(t, DialectMySQL, "shop", in); got != in {
		t.Errorf("IS NULL 不应遮盖其它值: %s", got)
	}
}

func TestMaskStatementsMSSQL(t *testing.T) {
	cols := []Column{{Schema: "app", Table: "dbo.users", Column: "password", Rule: &Rule{Kind: KindWhole}}}
	m := NewStmtMasker(DialectMSSQL, "app", cols, nil)
	cases := []struct{ in, want string }{
		{"UPDATE [dbo].[users] SET [password] = N'pw中文12' WHERE id = 1", "UPDATE [dbo].[users] SET [password] = *** WHERE id = 1"},
		{"update users set password='abcd' where id=1\nGO\nupdate users set nick='x' where password='zzzz'", "update users set password=*** where id=1\nGO\nupdate users set nick='x' where password=***"},
		{"INSERT INTO dbo.users (id, [password]) VALUES (1, 'secret')", "INSERT INTO dbo.users (id, [password]) VALUES (1, ***)"},
		{"update dbo.orders set password='visible' where id=1", "update dbo.orders set password='visible' where id=1"},
	}
	for _, c := range cases {
		if got, _ := m.Mask(c.in); got != c.want {
			t.Errorf("\n输入：%s\n期望：%s\n实际：%s", c.in, c.want, got)
		}
	}
}

func TestMaskStatementsDisabled(t *testing.T) {
	m := NewStmtMasker(DialectMySQL, "shop", nil, nil)
	in := "update users set password='x'"
	if got, s := m.Mask(in); got != in || len(s) != 0 || m.Enabled() {
		t.Errorf("没有配置时原样返回: %q %v", got, s)
	}
	var nilM *StmtMasker
	if got, _ := nilM.Mask(in); got != in {
		t.Errorf("nil 安全")
	}
}
