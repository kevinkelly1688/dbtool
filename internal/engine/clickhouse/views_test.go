// @author: kevinkelly
// @license: Apache Licence
// @file: views_test.go
// @time: 2026/10/28

package clickhouse

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"dbtool/internal/engine"
)

var viewCols = [][2]string{{"database", "String"}, {"name", "String"}, {"engine", "String"}, {"as_select", "String"}}

func TestViewDefs(t *testing.T) {
	f := newFake(t)
	f.onJC("FROM system.tables WHERE engine IN", viewCols, [][]any{
		{"app", "v_users", "View", "SELECT phone AS mobile, id FROM app.users"},
		{"app", "mv_daily", "MaterializedView", "SELECT toDate(ts) AS d, count() AS c FROM app.events GROUP BY d"},
		{"other", "v_locked", "View", ""}, // 没有权限看定义
	})
	e := f.engine()
	defs, err := e.ViewDefs(context.Background(), "app", []engine.ViewRef{{Name: "v_users"}, {Schema: "other", Name: "it's"}, {Name: "mv_daily"}, {Schema: "other", Name: "v_locked"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []engine.ViewDef{
		{Schema: "app", Name: "v_users", Definition: "SELECT phone AS mobile, id FROM app.users"},
		{Schema: "app", Name: "mv_daily", Definition: "SELECT toDate(ts) AS d, count() AS c FROM app.events GROUP BY d"},
		{Schema: "other", Name: "v_locked"},
	}
	if !reflect.DeepEqual(defs, want) {
		t.Errorf("defs=%+v", defs)
	}
	var body string
	var q map[string][]string
	for _, r := range f.requests() {
		if strings.Contains(r.Body, "as_select") {
			body, q = r.Body, r.Query
		}
	}
	for _, sub := range []string{
		"FROM system.tables WHERE engine IN ('View', 'MaterializedView', 'LiveView', 'WindowView')",
		"database NOT IN ('system', 'INFORMATION_SCHEMA', 'information_schema')",
		"(database = 'app' AND name = 'v_users')",
		`(database = 'other' AND name = 'it\'s')`, // 名字里的单引号要转义
		"(database = 'app' AND name = 'mv_daily')",
	} {
		if !strings.Contains(body, sub) {
			t.Errorf("SQL 缺少 %q:\n%s", sub, body)
		}
	}
	if len(q["readonly"]) == 0 {
		t.Errorf("元数据查询也要带 readonly: %v", q)
	}
}

func TestViewDefsDefaultDatabaseAndNothingToAsk(t *testing.T) {
	f := newFake(t)
	f.onJC("as_select", viewCols, [][]any{{"app", "v", "View", "SELECT 1"}})
	e := f.engine() // 实例默认库 app
	if defs, err := e.ViewDefs(context.Background(), "", []engine.ViewRef{{Name: "v"}}); err != nil || len(defs) != 1 {
		t.Fatalf("没有当前库时用实例的默认库: %v %v", defs, err)
	}
	if !f.lastBodyHas("(database = 'app' AND name = 'v')") {
		t.Error("应按默认库 app 查")
	}
	n := len(f.requests())
	if defs, err := e.ViewDefs(context.Background(), "app", []engine.ViewRef{{}, {Name: ""}}); err != nil || defs != nil || len(f.requests()) != n {
		t.Errorf("没有可查的名字不该发请求: %v %v", defs, err)
	}
	if q := viewDefsQuery("", []engine.ViewRef{{Name: "v"}}); q != "" {
		t.Errorf("没有库名也没有默认库: %q", q)
	}
}

func TestViewDefsOversizedDefinitionIsUnreadable(t *testing.T) {
	f := newFake(t)
	f.onJC("as_select", viewCols, [][]any{{"app", "v_big", "View", "SELECT " + strings.Repeat("x", engine.MaxViewDefBytes+10)}})
	defs, err := f.engine().ViewDefs(context.Background(), "app", []engine.ViewRef{{Name: "v_big"}})
	if err != nil || len(defs) != 1 || defs[0].Definition != "" {
		t.Errorf("过大的定义当作读不到: err=%v defs=%d", err, len(defs))
	}
}

func TestViewDefsErrors(t *testing.T) {
	refs := []engine.ViewRef{{Name: "v"}}
	cases := []struct {
		name        string
		setup       func(f *fakeCH)
		unsupported bool
	}{
		{"老版本没有 as_select 列（47）", func(f *fakeCH) {
			f.on(contains("as_select"), chError(47, "Missing columns: 'as_select' while processing query"))
		}, true},
		{"表里没有这一列（16）", func(f *fakeCH) {
			f.on(contains("as_select"), chError(16, "There is no column with name 'as_select' in table"))
		}, true},
		{"返回的列不对", func(f *fakeCH) {
			f.onJC("as_select", [][2]string{{"x", "UInt8"}}, [][]any{{1}})
		}, true},
		{"没有权限（497）不能当作没有视图", func(f *fakeCH) {
			f.on(contains("as_select"), chError(497, "reader: Not enough privileges. To execute this query it's necessary to have grant SELECT ON system.tables"))
		}, false},
		{"超时（159）", func(f *fakeCH) {
			f.on(contains("as_select"), chError(159, "Timeout exceeded"))
		}, false},
	}
	for _, c := range cases {
		f := newFake(t)
		c.setup(f)
		_, err := f.engine().ViewDefs(context.Background(), "app", refs)
		if err == nil {
			t.Errorf("%s: 应返回错误", c.name)
			continue
		}
		if got := errors.Is(err, engine.ErrViewDefsUnsupported); got != c.unsupported {
			t.Errorf("%s: unsupported=%v，期望 %v（%v）", c.name, got, c.unsupported, err)
		}
	}
}
