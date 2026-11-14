// @author: kevinkelly
// @license: Apache Licence
// @file: split_test.go
// @time: 2026/10/28

package dbtadmin

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func texts(ss []Stmt) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Text
	}
	return out
}

func TestSplitSQL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"基本", "CREATE TABLE a (id INT);\nINSERT INTO a VALUES (1);\n", []string{"CREATE TABLE a (id INT)", "INSERT INTO a VALUES (1)"}},
		{"最后一条没有分号", "SELECT 1;\nSELECT 2", []string{"SELECT 1", "SELECT 2"}},
		{"单引号里的分号", "INSERT INTO t VALUES ('a;b');SELECT 1;", []string{"INSERT INTO t VALUES ('a;b')", "SELECT 1"}},
		{"双引号里的分号", `INSERT INTO t VALUES ("a;b");`, []string{`INSERT INTO t VALUES ("a;b")`}},
		{"反斜杠转义的引号", `INSERT INTO t VALUES ('it\'s; ok');SELECT 2;`, []string{`INSERT INTO t VALUES ('it\'s; ok')`, "SELECT 2"}},
		{"重复引号转义", "INSERT INTO t VALUES ('it''s; ok');SELECT 2;", []string{"INSERT INTO t VALUES ('it''s; ok')", "SELECT 2"}},
		{"反引号里的分号与引号", "CREATE TABLE `a;b'c` (id INT);SELECT 1;", []string{"CREATE TABLE `a;b'c` (id INT)", "SELECT 1"}},
		{"反引号重复转义", "CREATE TABLE `a``;b` (id INT);", []string{"CREATE TABLE `a``;b` (id INT)"}},
		{"-- 注释里的分号", "-- 说明; 还有分号\nSELECT 1; -- 尾注释;\nSELECT 2;", []string{"SELECT 1", "SELECT 2"}},
		{"# 注释里的分号", "# a;b\nSELECT 1;", []string{"SELECT 1"}},
		{"块注释里的分号", "/* a;\nb; */ SELECT 1;", []string{"SELECT 1"}},
		{"--x 不是注释", "SELECT 1--1;", []string{"SELECT 1--1"}},
		{"-- 后面是文件结尾", "SELECT 1; --", []string{"SELECT 1"}},
		{"可执行注释保留", "/*!40101 SET NAMES utf8mb4 */;SELECT 1;", []string{"/*!40101 SET NAMES utf8mb4 */", "SELECT 1"}},
		{"可执行注释里的分号", "/*!50003 SELECT 1; */;", []string{"/*!50003 SELECT 1; */"}},
		{"优化器提示", "SELECT /*+ MAX_EXECUTION_TIME(1000) */ 1;", []string{"SELECT /*+ MAX_EXECUTION_TIME(1000) */ 1"}},
		{"空语句与纯注释被丢弃", ";;\n-- only comment\n;/* x */;SELECT 1;;", []string{"SELECT 1"}},
		{"空文件", "", nil},
		{"只有空白", " \n\t\r\n", nil},
		{"CRLF", "SELECT 1;\r\nSELECT 2;\r\n", []string{"SELECT 1", "SELECT 2"}},
		{"BOM", string(rune(0xFEFF)) + "SELECT 1;", []string{"SELECT 1"}},
		{"字符串里含注释符", "INSERT INTO t VALUES ('-- not comment', '/* nor this */', '# no');", []string{"INSERT INTO t VALUES ('-- not comment', '/* nor this */', '# no')"}},
		{"多行字符串", "INSERT INTO t VALUES ('l1;\nl2;');SELECT 1;", []string{"INSERT INTO t VALUES ('l1;\nl2;')", "SELECT 1"}},
		{"中文", "INSERT INTO t VALUES ('权限;组');", []string{"INSERT INTO t VALUES ('权限;组')"}},
		{"以 delimiter 开头的标识符不是 DELIMITER 命令", "SELECT delimiter_col FROM t;", []string{"SELECT delimiter_col FROM t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SplitSQL(tc.in, SplitOptions{})
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if !reflect.DeepEqual(texts(got), tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
				t.Errorf("得到 %q\n期望 %q", texts(got), tc.want)
			}
		})
	}
}

func TestSplitSQLLineNumbers(t *testing.T) {
	src := "-- 头部注释\n\nCREATE TABLE a (\n  id INT\n);\n/* c\n c */\n  INSERT INTO a VALUES ('x\ny');\nSELECT 1"
	got, err := SplitSQL(src, SplitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantLines := []int{3, 8, 10}
	if len(got) != len(wantLines) {
		t.Fatalf("语句数 %d", len(got))
	}
	for i, s := range got {
		if s.Line != wantLines[i] {
			t.Errorf("语句 %d 行号 %d，期望 %d（%q）", i+1, s.Line, wantLines[i], s.Text)
		}
	}
}

func TestSplitSQLDelimiterRejected(t *testing.T) {
	for _, src := range []string{
		"DELIMITER $$\nCREATE PROCEDURE p() BEGIN SELECT 1; END$$\nDELIMITER ;\n",
		"SELECT 1;\ndelimiter //\n",
		"  Delimiter ;;\n",
	} {
		_, err := SplitSQL(src, SplitOptions{})
		var se *SplitError
		if !errors.As(err, &se) || !strings.Contains(se.Msg, "DELIMITER") {
			t.Errorf("%q 应报不支持 DELIMITER，得到 %v", src, err)
		}
	}
	// 第二个例子的行号应指向 DELIMITER 所在行
	_, err := SplitSQL("SELECT 1;\ndelimiter //\n", SplitOptions{})
	var se *SplitError
	if errors.As(err, &se) && se.Line != 2 {
		t.Errorf("行号 = %d，期望 2", se.Line)
	}
}

func TestSplitSQLUnterminated(t *testing.T) {
	for name, src := range map[string]string{
		"单引号": "SELECT 'abc;\nSELECT 2;",
		"双引号": `SELECT "abc;`,
		"反引号": "SELECT `abc;",
		"块注释": "SELECT 1; /* 没有结束\n;SELECT 2;",
	} {
		_, err := SplitSQL(src, SplitOptions{})
		var se *SplitError
		if !errors.As(err, &se) {
			t.Errorf("%s：应报没有闭合，得到 %v", name, err)
			continue
		}
		if !strings.Contains(se.Msg, "没有闭合") {
			t.Errorf("%s：信息 %q", name, se.Msg)
		}
	}
}

func TestSplitSQLTooLong(t *testing.T) {
	long := "INSERT INTO t VALUES ('" + strings.Repeat("x", 5000) + "');"
	if _, err := SplitSQL(long, SplitOptions{MaxStmtBytes: 1000}); err == nil || !strings.Contains(err.Error(), "过长") {
		t.Errorf("超长语句应报错: %v", err)
	}
	if got, err := SplitSQL(long, SplitOptions{}); err != nil || len(got) != 1 {
		t.Errorf("默认上限下不应报错: %v %d", err, len(got))
	}
	// 刚好在上限内
	ok := "SELECT '" + strings.Repeat("y", 20) + "'"
	if _, err := SplitSQL(ok, SplitOptions{MaxStmtBytes: len(ok)}); err != nil {
		t.Errorf("恰好等于上限应通过: %v", err)
	}
	if _, err := SplitSQL(ok, SplitOptions{MaxStmtBytes: len(ok) - 1}); err == nil {
		t.Error("超过上限 1 字节应报错")
	}
	// 默认上限本身：构造 DefaultMaxStmtBytes+1 的语句
	huge := "SELECT '" + strings.Repeat("z", DefaultMaxStmtBytes) + "'"
	if _, err := SplitSQL(huge, SplitOptions{}); err == nil {
		t.Error("超过默认上限应报错")
	}
}

func TestRedact(t *testing.T) {
	s := Redact("INSERT INTO dbt_sql_users (username, password) VALUES ('admin', 'pbkdf2_sha256$390000$salt$HASHVALUE');", 200)
	if strings.Contains(s, "pbkdf2") || strings.Contains(s, "admin'") || strings.Contains(s, "HASHVALUE") {
		t.Errorf("字符串字面量应被隐去: %s", s)
	}
	if !strings.Contains(s, "INSERT INTO dbt_sql_users") {
		t.Errorf("语句结构应保留: %s", s)
	}
	long := Redact("CREATE TABLE t ("+strings.Repeat("c INT, ", 100)+"x INT)", 200)
	if n := len([]rune(long)); n > 201 {
		t.Errorf("应截断到 200 个字符，得到 %d", n)
	}
	// 没闭合的字符串：后半部分都当作字面量
	if s := Redact("SELECT 'abc SECRET", 200); strings.Contains(s, "SECRET") {
		t.Errorf("未闭合字符串也要隐去: %s", s)
	}
	// 转义引号
	if s := Redact(`SELECT 'it\'s SECRET', 1`, 200); strings.Contains(s, "SECRET") {
		t.Errorf("转义引号处理有误: %s", s)
	}
}
