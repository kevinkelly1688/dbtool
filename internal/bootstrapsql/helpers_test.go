// @author: kevinkelly
// @license: Apache Licence
// @file: helpers_test.go
// @time: 2026/10/28

package bootstrapsql

import (
	"strings"
	"testing"

	"dbtool/internal/dbtadmin"
)

// 测试辅助代码本身的自检：切分器与 INSERT 解析器若有错，上面所有结论都不可信。

func TestSplitSQLHandlesQuotesAndComments(t *testing.T) {
	src := "-- 头注释; 带分号\n" +
		"SET NAMES utf8mb4;\n" +
		"/* 块注释; */ INSERT INTO t (a, b) VALUES (1, 'x;y'), (2, 'it''s'), (3, 'back\\'slash;'); # 行注释;\n" +
		"CREATE TABLE `a;b` (id int) -- 行尾注释; 里面有分号\n" +
		"ENGINE=InnoDB;\n" +
		"SELECT 1"
	ss, err := splitSQL(src)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"SET NAMES utf8mb4",
		"INSERT INTO t (a, b) VALUES (1, 'x;y'), (2, 'it''s'), (3, 'back\\'slash;')",
		"CREATE TABLE `a;b` (id int) -- 行尾注释; 里面有分号\nENGINE=InnoDB",
		"SELECT 1",
	}
	if len(ss) != len(want) {
		t.Fatalf("切出 %d 条，期望 %d：%#v", len(ss), len(want), ss)
	}
	for i := range want {
		if ss[i].Text != want[i] {
			t.Errorf("第 %d 条：\n got  %q\n want %q", i, ss[i].Text, want[i])
		}
	}
	if ss[0].Line != 2 {
		t.Errorf("第一条语句的行号 %d，期望 2", ss[0].Line)
	}
	if _, err := splitSQL("SELECT 'abc"); err == nil {
		t.Error("引号没闭合应报错")
	}
	if _, err := splitSQL("SELECT 1 /* x"); err == nil {
		t.Error("块注释没闭合应报错")
	}
}

func TestParseInsert(t *testing.T) {
	ins, err := parseInsert(stmt{Line: 7, Text: "INSERT INTO `t` (`id`, `n`, `d`) VALUES\n  (1, 'a''b', NOW(6)),\n  (2, 'c\\\\d,(e)', NULL)\nON DUPLICATE KEY UPDATE `id` = `id`"})
	if err != nil {
		t.Fatal(err)
	}
	if ins.Table != "t" || ins.Ignore || len(ins.Rows) != 2 || ins.OnDup != "`id` = `id`" {
		t.Fatalf("解析结果不对：%+v", ins)
	}
	if v := ins.col(ins.Rows[0], "n"); !v.IsStr || v.S != "a'b" {
		t.Errorf("转义的引号：%+v", v)
	}
	if v := ins.col(ins.Rows[0], "d"); v.IsStr || v.S != "NOW(6)" {
		t.Errorf("函数调用：%+v", v)
	}
	if v := ins.col(ins.Rows[1], "n"); v.S != "c\\d,(e)" {
		t.Errorf("反斜杠与括号：%+v", v)
	}
	if v := ins.col(ins.Rows[1], "d"); v.IsStr || v.S != "NULL" {
		t.Errorf("NULL：%+v", v)
	}
	if _, err := parseInsert(stmt{Text: "INSERT INTO t (a, b) VALUES (1)"}); err == nil {
		t.Error("列数与值个数不一致应报错")
	}
	ig, err := parseInsert(stmt{Text: "INSERT IGNORE INTO t (a) VALUES (1), (2)"})
	if err != nil || !ig.Ignore || len(ig.Rows) != 2 {
		t.Errorf("INSERT IGNORE：%+v %v", ig, err)
	}
}

// 与 dbt-admin 实际使用的切分器（internal/dbtadmin.SplitSQL）逐条对照：两个文件在两种切分器下得到完全相同的语句序列，
// 说明 dbt-admin 执行到的就是这里解析、校验过的那些语句。
func TestSplitterAgreesWithDbtAdmin(t *testing.T) {
	for _, f := range []string{schemaFile, dataFile} {
		mine := loadStmts(t, f)
		theirs, err := dbtadmin.SplitSQL(readFile(t, f), dbtadmin.SplitOptions{})
		if err != nil {
			t.Fatalf("%s：dbt-admin 的切分器报错：%v", f, err)
		}
		if len(mine) != len(theirs) {
			t.Fatalf("%s：本测试切出 %d 条，dbt-admin 切出 %d 条", f, len(mine), len(theirs))
		}
		for i := range mine {
			if mine[i].Text != theirs[i].Text {
				t.Errorf("%s 第 %d 条语句不一致：\n 测试 %s\n 工具 %s", f, i+1, preview(mine[i].Text), preview(theirs[i].Text))
			}
			if strings.TrimSpace(theirs[i].Text) == "" {
				t.Errorf("%s 第 %d 条为空语句", f, i+1)
			}
		}
	}
}
