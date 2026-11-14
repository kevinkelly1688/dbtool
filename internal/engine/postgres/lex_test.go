// @author: kevinkelly
// @license: Apache Licence
// @file: lex_test.go
// @time: 2026/10/28

package postgres

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func lexKinds(t *testing.T, src string) []string {
	t.Helper()
	toks, _, err := lex(src)
	if err != nil {
		t.Fatalf("lex(%q)：%v", src, err)
	}
	var out []string
	for _, tk := range toks {
		var k string
		switch tk.kind {
		case tWord:
			k = "W"
		case tQuoted:
			k = "Q"
		case tString:
			k = "S"
		case tNumber:
			k = "N"
		case tOp:
			k = "O"
		case tPunct:
			k = "P"
		}
		out = append(out, k+":"+tk.text)
	}
	return out
}

func TestLexTokens(t *testing.T) {
	cases := []struct {
		src  string
		want []string
	}{
		{"select 1", []string{"W:select", "N:1"}},
		{"select 'a''b'", []string{"W:select", "S:'a''b'"}},
		{`select 'a\'`, []string{"W:select", `S:'a\'`}}, // 普通字符串里反斜杠不是转义
		{`select E'a\'b'`, []string{"W:select", `S:E'a\'b'`}},
		{`select e'\\'`, []string{"W:select", `S:e'\\'`}},
		{`select xe'a'`, []string{"W:select", "W:xe", "S:'a'"}}, // xe 是标识符，不是 E 串前缀
		{"select $$a'b$$", []string{"W:select", "S:$$a'b$$"}},
		{"select $x$ $$ $x$", []string{"W:select", "S:$x$ $$ $x$"}},
		{"select $x$a$y$b$x$", []string{"W:select", "S:$x$a$y$b$x$"}},
		{"select a$b$c", []string{"W:select", "W:a$b$c"}}, // $ 在标识符中间不是美元引号
		{`select "a""b"`, []string{"W:select", `Q:a"b`}},
		{`select "a;b"`, []string{"W:select", "Q:a;b"}},
		{"select 1 -- c\n, 2", []string{"W:select", "N:1", "P:,", "N:2"}},
		{"select 1 --c\r, 2", []string{"W:select", "N:1", "P:,", "N:2"}}, // 行注释在 \r 结束
		{"select /* a /* b */ c */ 1", []string{"W:select", "N:1"}},
		{"select 1+-2", []string{"W:select", "N:1", "O:+-", "N:2"}},
		{"select 1+--2\n3", []string{"W:select", "N:1", "O:+", "N:3"}},
		{"select 1*/*c*/2", []string{"W:select", "N:1", "O:*", "N:2"}},
		{"select a::int", []string{"W:select", "W:a", "P::", "P::", "W:int"}},
		{"select a->>'b'", []string{"W:select", "W:a", "O:->>", "S:'b'"}},
		{"select 1.5e-3, .5, 1., 0x1F, 1_000", []string{"W:select", "N:1.5e-3", "P:,", "N:.5", "P:,", "N:1.", "P:,", "N:0x1F", "P:,", "N:1_000"}},
		{"select t.a, t.*", []string{"W:select", "W:t", "P:.", "W:a", "P:,", "W:t", "P:.", "O:*"}},
		{"select a[1]", []string{"W:select", "W:a", "P:[", "N:1", "P:]"}},
		{"select 'x';select 'y'", []string{"W:select", "S:'x'", "P:;", "W:select", "S:'y'"}},
		{"select É", []string{"W:select", "W:É"}},
		{"select 1 /* ' */ ", []string{"W:select", "N:1"}},
		{"select '/* not comment */'", []string{"W:select", "S:'/* not comment */'"}},
		{"select 'a'\n'b'", []string{"W:select", "S:'a'", "S:'b'"}},
	}
	for _, c := range cases {
		if got := lexKinds(t, c.src); !reflect.DeepEqual(got, c.want) {
			t.Errorf("lex(%q)\n got  %q\n want %q", c.src, got, c.want)
		}
	}
}

func TestLexErrorsHavePosition(t *testing.T) {
	_, _, err := lex("select 1\n  from 'oops")
	if err == nil || !strings.Contains(err.Error(), "第 2 行第 8 列") {
		t.Fatalf("错误应带行列号：%v", err)
	}
}

func TestStatementSplit(t *testing.T) {
	toks, _, _ := lex("select 1;; select 2 ; ;select 3")
	st := splitStatements(toks)
	if len(st) != 3 {
		t.Fatalf("%v", st)
	}
	toks, _, _ = lex(";;;")
	if len(splitStatements(toks)) != 0 {
		t.Fatal("全是空语句")
	}
}

func TestStripComments(t *testing.T) {
	src := "select 1 /* a */ , 2 -- b\n, 3 /* c"
	// 未闭合的块注释会被词法器拒绝；这里只测合法输入
	src = "select 1 /* a */ , 2 -- b\n, 3"
	toks, comments, err := lex(src)
	if err != nil {
		t.Fatal(err)
	}
	got := stripComments(src, toks[0].start, toks[len(toks)-1].end, comments)
	if got != "select 1   , 2  \n, 3" {
		t.Fatalf("%q", got)
	}
}

// 去掉注释后重新切词，token 序列必须与原文一致（我们发给服务端的是去注释后的文本，所以这个性质是安全检查成立的前提）。
func TestCleanedTextLexesIdentically(t *testing.T) {
	frags := []string{
		"select", "from", "t", "a", ",", "(", ")", ";", "1", "2.5", "'x'", "'it''s'", "'--'", "'/*'", "$$a;b$$", "$q$ ' $q$", "E'\\''", "e'a\\\\'",
		"\"id\"", "\"a\"\"b\"", "\"--\"", "-- c\n", "-- c\r", "/* c */", "/* a /* n */ b */", "::", "->", "+", "-", "*", "/", "=", "[", "]", "\n", " ", "\t",
		"with", "as", "union", "limit", "1e5", "0x1F", "U", "é", ".", "x$y",
	}
	rng := rand.New(rand.NewSource(7))
	checked := 0
	for i := 0; i < 20000; i++ {
		var sb strings.Builder
		for j, n := 0, 1+rng.Intn(14); j < n; j++ {
			sb.WriteString(frags[rng.Intn(len(frags))])
			if rng.Intn(3) > 0 {
				sb.WriteByte(' ')
			}
		}
		src := sb.String()
		toks, comments, err := lex(src)
		if err != nil || len(toks) == 0 {
			continue
		}
		clean := stripComments(src, toks[0].start, toks[len(toks)-1].end, comments)
		toks2, _, err2 := lex(clean)
		if err2 != nil {
			t.Fatalf("去注释后词法失败：%q → %q：%v", src, clean, err2)
		}
		if len(toks) != len(toks2) {
			t.Fatalf("token 数不一致：%q → %q", src, clean)
		}
		for k := range toks {
			if toks[k].kind != toks2[k].kind || toks[k].text != toks2[k].text {
				t.Fatalf("token 不一致：%q → %q（第 %d 个：%v vs %v）", src, clean, k, toks[k], toks2[k])
			}
		}
		checked++
	}
	if checked < 2000 {
		t.Fatalf("有效样本太少：%d", checked)
	}
}

// 检查器对任意输入都不能 panic；放行的语句再检查一次结果必须一致（幂等）。
func TestCheckNeverPanicsAndIsIdempotent(t *testing.T) {
	frags := []string{
		"select", "from", "join", "t", "u.v", ",", "(", ")", "[", "]", ";", "1", "'x'", "$$z$$", "\"q\"", "-- c\n", "/* c */", "with", "recursive", "as", "c",
		"materialized", "not", "union", "all", "lateral", "only", "rows", "table", "values", "extract", "year", "limit", "fetch", "first", "distinct", "is",
		"where", "group", "by", "order", "window", "w", "for", "update", "share", "explain", "analyze", "verbose", "show", "insert", "into", "pg_sleep",
		"pg_authid", "*", "=", "::", ".", "on", "using", "left", "cross", "natural",
	}
	rng := rand.New(rand.NewSource(11))
	allowed := 0
	for i := 0; i < 30000; i++ {
		var sb strings.Builder
		for j, n := 0, 1+rng.Intn(16); j < n; j++ {
			sb.WriteString(frags[rng.Intn(len(frags))])
			sb.WriteByte(' ')
		}
		src := sb.String()
		c, err := CheckUserQuery(src, CheckOptions{})
		if err != nil {
			continue
		}
		allowed++
		c2, err := CheckUserQuery(c.SQL, CheckOptions{})
		if err != nil {
			t.Fatalf("放行的语句去注释后再检查被拒绝：%q → %q：%v", src, c.SQL, err)
		}
		if c2.SQL != c.SQL || c2.Kind != c.Kind || !reflect.DeepEqual(c2.Tables, c.Tables) || c2.hasLimit != c.hasLimit {
			t.Fatalf("再检查结果不一致：%q", src)
		}
		low := strings.ToLower(c.SQL)
		for _, bad := range []string{"insert", "pg_sleep(", "pg_authid"} {
			if strings.Contains(low, bad) && !strings.Contains(low, "'") && !strings.Contains(low, "$$") && !strings.Contains(low, "\"") {
				t.Fatalf("放行的语句含 %q：%q", bad, c.SQL)
			}
		}
	}
	if allowed < 200 {
		t.Fatalf("放行样本太少：%d", allowed)
	}
}

func FuzzCheckUserQuery(f *testing.F) {
	for _, s := range []string{"select 1", "select '", "select $$", "with a as (select 1) select * from a", "explain (analyze) select 1", "select 1; select 2", "/*", "select e'\\'", "select * from t for update"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, err := CheckUserQuery(s, CheckOptions{})
		if err != nil {
			return
		}
		// 放行的语句只有一条，且不含写操作 / 危险函数的 token
		toks, _, lerr := lex(c.SQL)
		if lerr != nil {
			t.Fatalf("放行的语句无法再切词：%q", c.SQL)
		}
		if len(splitStatements(toks)) != 1 {
			t.Fatalf("放行的语句不止一条：%q", c.SQL)
		}
		for i, tk := range toks {
			if tk.kind == tWord && denyWord[tk.low] != "" {
				t.Fatalf("放行的语句含 %s：%q", tk.low, c.SQL)
			}
			if tk.isIdentLike() && i+1 < len(toks) && toks[i+1].isPunct("(") && denyFunction(tk.low) != "" {
				t.Fatalf("放行的语句调用 %s：%q", tk.low, c.SQL)
			}
			if tk.isIdentLike() && denyRel(tk.low) != "" {
				t.Fatalf("放行的语句引用 %s：%q", tk.low, c.SQL)
			}
		}
	})
}
