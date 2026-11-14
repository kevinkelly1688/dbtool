// @author: kevinkelly
// @license: Apache Licence
// @file: goinception_test.go
// @time: 2026/10/28

package goinception

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"dbtool/internal/sqlx"
)

// 样例按 TiDB parser 的 AST JSON 结构手写（goInception 打印的语法树同源），
// 用于验证查找算法；上线前应再用生产环境 goInception 的真实输出核对一次。
func ts(schema, name string) string {
	return `{"Source":{"Schema":{"O":"` + schema + `","L":"` + schema + `"},"Name":{"O":"` + name + `","L":"` + name + `"}},"AsName":{"O":"","L":""}}`
}

func parse(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTableRefs(t *testing.T) {
	r := func(s, n string) sqlx.TableRef { return sqlx.TableRef{Schema: s, Name: n} }
	cases := []struct {
		name, tree string
		want       []sqlx.TableRef
	}{
		{"单表", `{"Fields":{},"From":{"TableRefs":{"Left":` + ts("", "orders") + `,"Right":null,"Tp":0}}}`, []sqlx.TableRef{r("shop", "orders")}},
		{"显式库名", `{"From":{"TableRefs":{"Left":` + ts("report", "daily") + `,"Right":null}}}`, []sqlx.TableRef{r("report", "daily")}},
		{"三表连接", `{"From":{"TableRefs":{"Left":{"Left":` + ts("", "orders") + `,"Right":` + ts("db2", "users") + `,"Tp":1},"Right":` + ts("", "refunds") + `,"Tp":1}}}`,
			[]sqlx.TableRef{r("db2", "users"), r("shop", "orders"), r("shop", "refunds")}},
		{"派生表只取内部真表", `{"From":{"TableRefs":{"Left":{"Source":{"From":{"TableRefs":{"Left":` + ts("", "inner_t") + `,"Right":null}}},"AsName":{"O":"x"}},"Right":null}}}`,
			[]sqlx.TableRef{r("shop", "inner_t")}},
		{"WHERE 子查询里的表", `{"From":{"TableRefs":{"Left":` + ts("", "t") + `,"Right":null}},"Where":{"Sel":{"From":{"TableRefs":{"Left":` + ts("", "u") + `,"Right":null}}}}}`,
			[]sqlx.TableRef{r("shop", "t"), r("shop", "u")}},
		{"重复引用去重", `{"From":{"TableRefs":{"Left":` + ts("", "t") + `,"Right":` + ts("", "t") + `}}}`, []sqlx.TableRef{r("shop", "t")}},
		{"无表语句", `{"Fields":{"Fields":[{"Expr":{"Value":1}}]}}`, nil},
	}
	for _, c := range cases {
		got := TableRefs(parse(t, c.tree), "shop")
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBuildPrintScript(t *testing.T) {
	tg := Target{Host: "10.0.0.5", Port: 3306, User: "ro", Password: "p@ss;w0rd"}
	s, err := BuildPrintScript(tg, "sh`op", "select a from t;")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--user='ro'", "--password='p@ss;w0rd'", "--host='10.0.0.5'", "--port=3306", "--enable-query-print", "inception_magic_start;", "use `sh``op`;", "select a from t;\ninception_magic_commit;"} {
		if !strings.Contains(s, want) {
			t.Errorf("脚本缺少 %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{"p'x", `p\x`, "p*/x", "p\nx"} {
		tg.Password = bad
		if _, err := BuildPrintScript(tg, "d", "select 1"); err == nil {
			t.Errorf("密码 %q 应被拒绝", bad)
		}
	}
}

func TestInterpretRow(t *testing.T) {
	if tree, err := interpretRow(map[string]string{"errlevel": "0", "errmsg": "", "query_tree": `{"a":1}`}); err != nil || tree != `{"a":1}` {
		t.Errorf("正常行: %q %v", tree, err)
	}
	if _, err := interpretRow(map[string]string{"errmsg": "syntax error"}); err == nil {
		t.Error("errmsg 应报错")
	}
	if _, err := interpretRow(map[string]string{"errlevel": "2"}); err == nil {
		t.Error("errlevel 非 0 应报错")
	}
	if _, err := interpretRow(map[string]string{"error_level": "2", "error_message": "x"}); err == nil {
		t.Error("审核格式的错误行应报错")
	}
	if tree, err := interpretRow(map[string]string{"id": "1", "statement": "use db"}); err != nil || tree != "" {
		t.Errorf("无语法树的行应忽略: %q %v", tree, err)
	}
}

func TestBuildMaskingScript(t *testing.T) {
	tg := Target{Host: "10.0.0.5", Port: 3306, User: "ro", Password: "pw123"}
	s, err := BuildMaskingScript(tg, "shop", "select phone from users;")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/*--user=ro;--password=pw123;--host=10.0.0.5;--port=3306;--masking=1;*/", "inception_magic_start;", "use `shop`;", "select phone from users;\ninception_magic_commit;"} {
		if !strings.Contains(s, want) {
			t.Errorf("脚本缺少 %q:\n%s", want, s)
		}
	}
	// 头部取值不加引号，所以空白和分号也不允许
	for _, bad := range []string{"p w", "p;w", "p'w", "p*/w"} {
		tg.Password = bad
		if _, err := BuildMaskingScript(tg, "d", "select 1"); err == nil {
			t.Errorf("密码 %q 应被拒绝", bad)
		}
	}
}

func TestParseSelectList(t *testing.T) {
	raw := `[{"index":0,"field":"id","type":"int(11)","table":"customers","schema":"shop","alias":"id"},
	         {"index":1,"field":"phone","type":"varchar(80)","table":"customers","schema":"shop","alias":"p"},
	         {"index":2,"field":"","type":"","table":"","schema":"","alias":"n"}]`
	got, err := ParseSelectList(raw)
	if err != nil || len(got) != 3 || got[1].Field != "phone" || got[1].Alias != "p" || got[1].Schema != "shop" || got[2].Index != 2 {
		t.Fatalf("解析结果异常: %+v err=%v", got, err)
	}
	for _, bad := range []string{`not json`, `{"a":1}`, `[{"field":"x"}]`, `[{"index":"0"}]`} {
		if _, err := ParseSelectList(bad); err == nil {
			t.Errorf("%q 应报错", bad)
		}
	}
}
