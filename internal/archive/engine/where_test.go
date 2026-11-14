// @author: kevinkelly
// @license: Apache Licence
// @file: where_test.go
// @time: 2026/10/28

package engine

import (
	"strings"
	"testing"
)

func TestNormalizeExtraAccepts(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"status = 3", "`status` = 3"},
		{"status IN (3,4)", "`status` IN (3, 4)"},
		{"status in (3 , 4) and amount > 0", "`status` IN (3, 4) AND `amount` > 0"},
		{"status NOT IN (1,2)", "`status` NOT IN (1, 2)"},
		{"name LIKE 'a%'", "`name` LIKE 'a%'"},
		{"name NOT LIKE '%x_'", "`name` NOT LIKE '%x_'"},
		{"deleted_at IS NULL", "`deleted_at` IS NULL"},
		{"deleted_at is not null", "`deleted_at` IS NOT NULL"},
		{"a = 1 OR b = 2", "`a` = 1 OR `b` = 2"},
		{"(a = 1 OR b = 2) AND c <> 3", "(`a` = 1 OR `b` = 2) AND `c` <> 3"},
		{"a != 3", "`a` <> 3"},
		{"a <=> NULL", "`a` <=> NULL"},
		{"a >= -5", "`a` >= -5"},
		{"a >= +5.5e3", "`a` >= 5.5e3"},
		{"amount BETWEEN 1 AND 10", "`amount` BETWEEN 1 AND 10"},
		{"NOT (a = 1)", "NOT (`a` = 1)"},
		{"`order` = 1", "`order` = 1"},
		{"name = 'it''s'", "`name` = 'it''s'"},
		{`name = "x y"`, "`name` = 'x y'"},
		{"name = 'a;b'", "`name` = 'a;b'"},
		{"name = 'a--b'", "`name` = 'a--b'"},
		{"name = '含中文'", "`name` = '含中文'"},
		{"状态 = 1", "`状态` = 1"},
		{"a = b", "`a` = `b`"},
		{"flag IS TRUE", "`flag` IS TRUE"},
		{"a = 1\n AND\tb = 2", "`a` = 1 AND `b` = 2"},
	}
	for _, c := range cases {
		got, _, err := NormalizeExtra(c.in)
		if err != nil {
			t.Errorf("%q 应通过，得到错误：%v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q 规范化\n got  %s\n want %s", c.in, got, c.want)
		}
	}
}

func TestNormalizeExtraRejects(t *testing.T) {
	cases := []struct{ in, errHas string }{
		{"a = 1; DROP TABLE x", "分号"},
		{"a = 1 -- x", "注释"},
		{"a = 1 /* x */", "注释"},
		{"a = 1 # x", "注释"},
		{"a IN (SELECT id FROM t)", "SELECT"},
		{"a = 1 UNION SELECT 1", "UNION"},
		{"a = SLEEP(5)", "SLEEP"},
		{"a = BENCHMARK(1000000, MD5(1))", "BENCHMARK"},
		{"a = 1 INTO OUTFILE '/tmp/x'", "INTO"},
		{"a = LOAD_FILE('/etc/passwd')", "LOAD_FILE"},
		{"a = 1 INTO DUMPFILE '/tmp/x'", "INTO"},
		{"a = 1; DELETE FROM t", "分号"},
		{"DELETE FROM t", "DELETE"},
		{"a = 1 OR 1 = 1 ORDER BY 1", "ORDER"},
		{"a = 1 LIMIT 1", "LIMIT"},
		{"a = 1 FOR UPDATE", "FOR"},
		{"a = 1 xor b = 2", "XOR"},
		{"a REGEXP 'x'", "REGEXP"},
		{"if(a, 1, 2) = 1", "IF"},
		{"a = (SELECT 1)", "SELECT"},
		{"NOW() > a", "函数调用"},
		{"lower(name) = 'x'", "函数调用"},
		{"`lower`(name) = 'x'", "函数调用"},
		{"a = 'unterminated", "引号"},
		{"a = 1)", "括号"},
		{"(a = 1", "括号"},
		{"a", "条件表达式"},
		{"= 1", "条件不完整"},
		{"a =", "条件不完整"},
		{"a = 1 b = 2", "多余"},
		{"a = 1 AND", "条件不完整"},
		{"a IN ()", "条件不完整"},
		{"a IN 1,2", "括号列表"},
		{"a IN (1,", "条件不完整"},
		{"a IN (1 2)", "逗号"},
		{`a = 'x\'`, "反斜杠"},
		{"a = 0x41", "十六进制"},
		{"a = 1abc", "数字"},
		{"a = x'41'", "多余"},
		{"t.a = 1", "限定"},
		{"db.t.a = 1", "限定"},
		{"a = 1 + 2", "不允许的字符"},
		{"a = 1 * 2", "不允许的字符"},
		{"a & 1 = 1", "不允许的字符"},
		{"a = @v", "不允许的字符"},
		{"a = ?", "不允许的字符"},
		{"a || b", "不允许的字符"},
		{"a BETWEEN 1 OR 2", "AND"},
		{"a IS 5", "NULL"},
		{"a NOT = 5", "IN / LIKE"},
		{"`a``b` = 1", "反引号"},
		{"`a b` = 1", "含不允许的字符"},
		{"a = '\x00'", "非法字符"},
		{"a = '\x01'", "控制字符"},
		{"CASE WHEN a THEN 1 END", "CASE"},
		{"a = 1 COLLATE utf8_bin", "COLLATE"},
		{"EXISTS (select 1)", "EXISTS"},
		{"a = ANY (1)", "ANY"},
	}
	for _, c := range cases {
		_, _, err := NormalizeExtra(c.in)
		if err == nil {
			t.Errorf("%q 应被拒绝", c.in)
			continue
		}
		if !strings.Contains(err.Error(), c.errHas) {
			t.Errorf("%q 的错误应包含 %q，得到：%v", c.in, c.errHas, err)
		}
	}
}

func TestNormalizeExtraLength(t *testing.T) {
	ok := "a IN (" + strings.TrimSuffix(strings.Repeat("1,", 100), ",") + ")" // 约 210 字
	if _, _, err := NormalizeExtra(ok); err != nil {
		t.Fatalf("短条件应通过：%v", err)
	}
	long := "name = '" + strings.Repeat("x", 495) + "'" // 504 字
	if _, _, err := NormalizeExtra(long); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("超过 500 字应被拒绝：%v", err)
	}
	exact := "name = '" + strings.Repeat("x", 490) + "'" // 500 字
	if _, _, err := NormalizeExtra(exact); err != nil {
		t.Errorf("正好 500 字应通过：%v", err)
	}
	// 按字符数而非字节数
	cn := "name = '" + strings.Repeat("字", 490) + "'"
	if _, _, err := NormalizeExtra(cn); err != nil {
		t.Errorf("500 个字符（含中文）应通过：%v", err)
	}
	if _, _, err := NormalizeExtra("a IN (" + strings.Repeat("1,", 250) + "1)"); err == nil {
		t.Errorf("IN 列表过长应被拒绝")
	}
}

func TestNormalizeExtraColumns(t *testing.T) {
	_, cols, err := NormalizeExtra("status IN (3,4) AND `Amount` > 0 AND status <> 9 AND (b = a)")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cols, ",") != "status,Amount,b,a" {
		t.Errorf("列名去重并保持出现顺序，得到 %v", cols)
	}
}

// 无论输入什么，规范化的结果只会由反引号标识符、字面量、运算符和关键字组成：对一批“攻击样例”确认没有原样泄漏。
func TestNormalizeExtraNeverLeaksRaw(t *testing.T) {
	inputs := []string{
		"a = 1 OR 1 = 1",
		"name = 'x'' OR ''1''=''1'",
		"a = 1 AND b LIKE '%;%'",
	}
	for _, in := range inputs {
		out, _, err := NormalizeExtra(in)
		if err != nil {
			continue
		}
		if strings.ContainsAny(out, ";\\") && !strings.Contains(out, "'") {
			t.Errorf("%q → %q", in, out)
		}
	}
}

func TestBuildWhere(t *testing.T) {
	s := Spec{TimeColumn: "created_at", RetentionDays: 180}
	got, err := BuildWhere(s)
	if err != nil || got != "`created_at` < NOW() - INTERVAL 180 DAY" {
		t.Fatalf("got %q, %v", got, err)
	}
	s.ExtraWhere = "status IN (3,4)"
	got, err = BuildWhere(s)
	if err != nil || got != "`created_at` < NOW() - INTERVAL 180 DAY AND (`status` IN (3, 4))" {
		t.Fatalf("got %q, %v", got, err)
	}
	s.ExtraWhere = "a = 1 OR b = 2"
	got, _ = BuildWhere(s)
	if got != "`created_at` < NOW() - INTERVAL 180 DAY AND (`a` = 1 OR `b` = 2)" {
		t.Errorf("OR 必须被括号包住：%s", got)
	}
	for _, bad := range []Spec{
		{TimeColumn: "", RetentionDays: 30},
		{TimeColumn: "a`b", RetentionDays: 30},
		{TimeColumn: "a;b", RetentionDays: 30},
		{TimeColumn: "ts", RetentionDays: 0},
		{TimeColumn: "ts", RetentionDays: -3},
		{TimeColumn: "ts", RetentionDays: 30, ExtraWhere: "a = 1; drop table x"},
	} {
		if _, err := BuildWhere(bad); err == nil {
			t.Errorf("%+v 应报错", bad)
		}
	}
}
