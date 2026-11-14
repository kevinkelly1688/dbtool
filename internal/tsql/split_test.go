// @author: kevinkelly
// @license: Apache Licence
// @file: split_test.go
// @time: 2026/10/28

package tsql

import (
	"reflect"
	"strings"
	"testing"
)

func texts(us []Unit) []string {
	var out []string
	for _, u := range us {
		out = append(out, u.SQL)
	}
	return out
}

func TestUnitsBasic(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{"两条语句按分号拆", "update a set x=1; delete from b where id=2;", []string{"update a set x=1;", "delete from b where id=2;"}},
		{"单条保留原文与注释", "-- 说明\nupdate a set x=1\n", []string{"-- 说明\nupdate a set x=1"}},
		{"GO 切批", "create table a(id int)\nGO\ninsert into a values(1)\ngo\n", []string{"create table a(id int)", "insert into a values(1)"}},
		{"字符串里的分号与 GO 不切", "insert into a values('x;y');\nupdate a set s='\nGO\n';", []string{"insert into a values('x;y');", "update a set s='\nGO\n';"}},
		{"注释里的 GO 不切", "select 1 /*\nGO\n*/ ;", []string{"select 1 /*\nGO\n*/ ;"}},
		{"GOTO/GOLD 不是 GO", "update a set gold=1; update a set goto_x=2", []string{"update a set gold=1;", "update a set goto_x=2"}},
		{"GO 后同行注释", "select 1\nGO -- 批结束\nselect 2", []string{"select 1", "select 2"}},
		{"空批与空语句被丢弃", "GO\n;\nselect 1;;\nGO\n-- 只有注释\nGO", []string{"select 1;"}},
		{"变量声明：整批", "declare @i int; set @i=1; update a set x=@i", []string{"declare @i int; set @i=1; update a set x=@i"}},
		{"游标/TRY/事务/GOTO：整批", "begin try select 1; select 2 end try begin catch select 3 end catch", []string{"begin try select 1; select 2 end try begin catch select 3 end catch"}},
		{"BEGIN TRAN：整批", "begin tran; update a set x=1; update b set y=2; commit", []string{"begin tran; update a set x=1; update b set y=2; commit"}},
		{"块内的分号不拆", "if 1=1 begin select 1; select 2; end", []string{"if 1=1 begin select 1; select 2; end"}},
		{"块之后仍可拆", "if 1=1 begin select 1; end; update a set x=1", []string{"if 1=1 begin select 1; end;", "update a set x=1"}},
		{"CASE…END 不影响拆分", "select case when 1=1 then 1 else 2 end; update a set x=1", []string{"select case when 1=1 then 1 else 2 end;", "update a set x=1"}},
		{"IF…;ELSE 被切断：整批", "if 1=1 select 1; else select 2", []string{"if 1=1 select 1; else select 2"}},
		{"方括号标识符里的分号", "update [a;b] set x=1; select 1", []string{"update [a;b] set x=1;", "select 1"}},
	}
	for _, c := range cases {
		if got := texts(Units(c.sql)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
	}
}

func TestUnitsBatchAndLine(t *testing.T) {
	sql := "create table a(id int)\nGO\n\n-- c\nupdate a set x=1;\nupdate a set x=2;\nGO\nselect 3"
	us := Units(sql)
	if len(us) != 4 {
		t.Fatalf("单元数: %+v", us)
	}
	want := []struct{ batch, line int }{{1, 1}, {2, 4}, {2, 6}, {3, 8}}
	for i, w := range want {
		if us[i].Batch != w.batch || us[i].Line != w.line {
			t.Errorf("单元 %d: batch=%d line=%d, 期望 %v (%q)", i, us[i].Batch, us[i].Line, w, us[i].SQL)
		}
	}
}

func TestTokenize(t *testing.T) {
	toks := Significant(Tokenize("select N'it''s' as [a]]b], \"c\" from #tmp where @x = @@ROWCOUNT -- end\n/* a /* nested */ still */ 1.5e3"))
	var got []string
	for _, k := range toks {
		got = append(got, k.Text)
	}
	want := []string{"select", "N'it''s'", "as", "[a]]b]", ",", `"c"`, "from", "#tmp", "where", "@x", "=", "@@ROWCOUNT", "1.5e3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got  %q\n want %q", got, want)
	}
	if Unquote("[a]]b]") != "a]b" || Unquote(`"x""y"`) != `x"y` || Unquote("plain") != "plain" {
		t.Error("Unquote")
	}
	// 未闭合的字符串/注释不会死循环
	for _, s := range []string{"select 'abc", "select /* abc", "select [abc", `select "abc`} {
		if len(Tokenize(s)) == 0 {
			t.Errorf("%q", s)
		}
	}
	if strings.TrimSpace(StripComments("select 1 -- x\n/* y */ from t")) != "select 1  \n   from t" && !strings.Contains(StripComments("a/*b*/c"), "a c") {
		t.Error("StripComments")
	}
}
