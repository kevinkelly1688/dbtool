// @author: kevinkelly
// @license: Apache Licence
// @file: views_test.go
// @time: 2026/10/28

package postgres

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"dbtool/internal/engine"
)

func TestTextArray(t *testing.T) {
	if got := textArray([]string{"a", `b"c`, `d\e`, "", "中文"}); got != `{"a","b\"c","d\\e","","中文"}` {
		t.Errorf("textArray=%s", got)
	}
	if got := textArray(nil); got != "{}" {
		t.Errorf("空数组=%s", got)
	}
}

func TestViewDefs(t *testing.T) {
	var gotParams [][]string
	h := func(sql string, params []string) *fakeResult {
		if strings.Contains(sql, "pg_get_viewdef") {
			gotParams = append(gotParams, params)
			return &fakeResult{Cols: textCols("nspname", "relname", "def"), Rows: [][]*string{
				cell("reporting", "v_account", " SELECT account.email AS contact\n   FROM account;"),
				cell("public", "v_locked", nullCell),
			}}
		}
		return nil
	}
	fs := newFakeServer(t, h)
	e := newEngine(t, fs, "u", "", "appdb")
	defs, err := e.ViewDefs(context.Background(), "appdb", []engine.ViewRef{{Schema: "reporting", Name: "v_account"}, {Name: "v_locked"}, {Name: ""}})
	if err != nil {
		t.Fatal(err)
	}
	want := []engine.ViewDef{
		{Schema: "reporting", Name: "v_account", Definition: " SELECT account.email AS contact\n   FROM account;"},
		{Schema: "public", Name: "v_locked"}, // 定义为 NULL：读不到
	}
	if !reflect.DeepEqual(defs, want) {
		t.Errorf("defs=%+v", defs)
	}
	// 名字走绑定参数（数组字面量）；空名字不带
	if wantP := [][]string{{`{"v_account","v_locked"}`}}; !reflect.DeepEqual(gotParams, wantP) {
		t.Errorf("params=%q", gotParams)
	}
	var q string
	for _, s := range fs.statements() {
		if strings.Contains(s, "pg_get_viewdef") {
			q = s
		}
	}
	for _, sub := range []string{"relkind IN ('v', 'm')", "information_schema", "!~ '^pg_'", "c.relname = ANY($1::text[])"} {
		if !strings.Contains(q, sub) {
			t.Errorf("SQL 缺少 %q: %s", sub, q)
		}
	}
	if strings.Contains(q, "v_account") {
		t.Errorf("名字不应拼进 SQL: %s", q)
	}
	if !fs.has("BEGIN READ ONLY") {
		t.Error("应在只读事务里查询")
	}
	// 没有可查的名字：不连库
	if defs, err := e.ViewDefs(context.Background(), "appdb", []engine.ViewRef{{}}); err != nil || defs != nil {
		t.Errorf("空名单: %v %v", defs, err)
	}
}
