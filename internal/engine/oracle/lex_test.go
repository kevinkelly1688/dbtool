// @author: kevinkelly
// @license: Apache Licence
// @file: lex_test.go
// @time: 2026/10/28

package oracle

import (
	"reflect"
	"strings"
	"testing"
)

// words 返回每个 token 的原文。
func words(toks []token) []string {
	out := make([]string, len(toks))
	for i, t := range toks {
		out[i] = t.text
	}
	return out
}

func TestLexBasics(t *testing.T) {
	cases := []struct {
		sql  string
		want []string
	}{
		{"select 1 from dual", []string{"select", "1", "from", "dual"}},
		{"SELECT a, b FROM hr.employees e", []string{"SELECT", "a", ",", "b", "FROM", "hr", ".", "employees", "e"}},
		{"select 'it''s' from dual", []string{"select", "'it''s'", "from", "dual"}},
		{`select "Col""X" from t`, []string{"select", `"Col""X"`, "from", "t"}},
		{"select q'[a'b]' , q'#x#', nq'{y}', n'z' from dual", []string{"select", "q'[a'b]'", ",", "q'#x#'", ",", "nq'{y}'", ",", "n'z'", "from", "dual"}},
		{"select 1 -- 注释 ; drop\nfrom dual", []string{"select", "1", "from", "dual"}},
		{"select /*+ full(t) */ * from t /* x ; y */", []string{"select", "*", "from", "t"}},
		{"select 1.5e-3, .5, 10f, 3d from dual", []string{"select", "1.5e-3", ",", ".5", ",", "10f", ",", "3d", "from", "dual"}},
		{"select a||b, a<=b, a<>b, a!=b from t", []string{"select", "a", "||", "b", ",", "a", "<=", "b", ",", "a", "<>", "b", ",", "a", "!=", "b", "from", "t"}},
		{"select :1, :name from dual", []string{"select", ":1", ",", ":name", "from", "dual"}},
		{"select a from t where x := y", []string{"select", "a", "from", "t", "where", "x", ":=", "y"}},
		{"select a(+) , b from t", []string{"select", "a", "(", "+", ")", ",", "b", "from", "t"}},
	}
	for _, c := range cases {
		toks, err := lex(c.sql)
		if err != nil {
			t.Errorf("%q: %v", c.sql, err)
			continue
		}
		if got := words(toks); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q:\n  got  %q\n  want %q", c.sql, got, c.want)
		}
	}
}

func TestLexKinds(t *testing.T) {
	toks, err := lex(`select 'it''s', q'[a'b]', "Mixed""Case", abc, 12, :b1 from hr.t`)
	if err != nil {
		t.Fatal(err)
	}
	want := []tokKind{tkWord, tkString, tkPunct, tkString, tkPunct, tkQuoted, tkPunct, tkWord, tkPunct, tkNumber, tkPunct, tkBind, tkWord, tkWord, tkPunct, tkWord}
	if len(toks) != len(want) {
		t.Fatalf("token 数 = %d，期望 %d：%q", len(toks), len(want), words(toks))
	}
	for i, k := range want {
		if toks[i].kind != k {
			t.Errorf("第 %d 个 token %q 的种类 = %d，期望 %d", i, toks[i].text, toks[i].kind, k)
		}
	}
	if toks[1].val != "it's" {
		t.Errorf("'' 转义: %q", toks[1].val)
	}
	if toks[3].val != "a'b" {
		t.Errorf("q'[]' 内容: %q", toks[3].val)
	}
	if toks[5].val != `Mixed"Case` || toks[5].name() != `Mixed"Case` {
		t.Errorf("加引号的标识符原样保留: %q %q", toks[5].val, toks[5].name())
	}
	if toks[7].name() != "ABC" {
		t.Errorf("未加引号的标识符折成大写: %q", toks[7].name())
	}
	if toks[13].name() != "HR" {
		t.Errorf("%+v", toks[13])
	}
}

func TestLexPositions(t *testing.T) {
	src := "select  abc  from t"
	toks, _ := lex(src)
	if src[toks[1].start:toks[1].end] != "abc" {
		t.Errorf("位置不对: %+v", toks[1])
	}
}

func TestLexErrors(t *testing.T) {
	for _, c := range []struct{ sql, want string }{
		{"select 'abc from dual", "字符串没有结束"},
		{`select "abc from dual`, "没有结束"},
		{`select "" from dual`, "空的标识符"},
		{"select /* abc from dual", "注释没有结束"},
		{"select q'[abc' from dual", "字符串没有结束"},
		{"select q' x' from dual", "定界符"},
		{"select a ` b from dual", "无法识别"},
		{"select 1\x00 from dual", "空字符"},
	} {
		_, err := lex(c.sql)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: 期望含 %q，得到 %v", c.sql, c.want, err)
		}
	}
}

func TestLexStringsHideKeywords(t *testing.T) {
	// 字符串、引号标识符、注释里的内容不是关键字 token
	toks, err := lex(`select 'drop table x; --', "update", /* delete ; */ c from t -- insert`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range toks {
		if tk.kind == tkWord {
			switch strings.ToLower(tk.text) {
			case "drop", "update", "delete", "insert":
				t.Errorf("不应出现关键字 token: %+v", tk)
			}
		}
	}
	want := []string{"select", "'drop table x; --'", ",", `"update"`, ",", "c", "from", "t"}
	if got := words(toks); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
}
