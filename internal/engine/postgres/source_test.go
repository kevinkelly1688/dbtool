// @author: kevinkelly
// @license: Apache Licence
// @file: source_test.go
// @time: 2026/10/28

package postgres

import (
	"context"
	"strings"
	"testing"
)

// 结果列的来源：语句描述里带表 OID + 列号，引擎反查成 架构.表.列；表达式列没有来源。
func TestRunUserColumnSources(t *testing.T) {
	handler := func(sql string, params []string) *fakeResult {
		switch {
		case strings.Contains(sql, "pg_catalog.pg_attribute"):
			if len(params) != 1 || params[0] != "{16384,16500}" {
				t.Errorf("反查的 OID 应是去重后的 {16384,16500}：%v", params)
			}
			return &fakeResult{Cols: textCols("rel", "att", "nsp", "rel", "col", "kind"), Rows: [][]*string{
				cell("16384", "3", "public", "account", "phone", "r"),
				cell("16384", "1", "public", "account", "id", "r"),
				cell("16500", "2", "reporting", "v_account", "email", "v"),
			}}
		case strings.Contains(sql, "from account"):
			return &fakeResult{
				Cols:    []fakeCol{{"id", 20}, {"p", 25}, {"mail", 25}, {"upper", 25}},
				Rows:    [][]*string{cell("1", "13812345678", "a@x.com", "ABC")},
				Origins: []Origin{{16384, 1}, {16384, 3}, {16500, 2}, {0, 0}},
			}
		}
		return nil
	}
	fs := newFakeServer(t, handler)
	e := newEngine(t, fs, "u", "", "d")
	sh, err := e.RunUser(context.Background(), "d", mustCheck(t, "select id, phone as p, email as mail, upper(name) from account"), QueryLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !sh.SourcesKnown || len(sh.Columns) != 4 {
		t.Fatalf("SourcesKnown=%v cols=%d", sh.SourcesKnown, len(sh.Columns))
	}
	want := []*ColumnSource{
		{Schema: "public", Table: "account", Column: "id", RelKind: "r"},
		{Schema: "public", Table: "account", Column: "phone", RelKind: "r"},
		{Schema: "reporting", Table: "v_account", Column: "email", RelKind: "v"},
		nil,
	}
	for i, w := range want {
		got := sh.Columns[i].Source
		if (w == nil) != (got == nil) || (w != nil && *got != *w) {
			t.Errorf("第 %d 列来源 = %+v，期望 %+v", i, got, w)
		}
	}
	if sh.Columns[2].Source != nil && !sh.Columns[2].Source.IsView() {
		t.Error("v 应判为视图")
	}
	if got := want[1].FullTable(); got != "public.account" {
		t.Errorf("FullTable=%q", got)
	}
}

// 反查失败（比如没有权限读 pg_class）不影响查询结果，只是来源未知。
func TestRunUserSourcesLookupFails(t *testing.T) {
	handler := func(sql string, params []string) *fakeResult {
		switch {
		case strings.Contains(sql, "pg_catalog.pg_attribute"):
			return pgErr("42501", "permission denied")
		case strings.Contains(sql, "from account"):
			return &fakeResult{Cols: []fakeCol{{"phone", 25}}, Rows: [][]*string{cell("1")}, Origins: []Origin{{16384, 3}}}
		}
		return nil
	}
	fs := newFakeServer(t, handler)
	e := newEngine(t, fs, "u", "", "d")
	sh, err := e.RunUser(context.Background(), "d", mustCheck(t, "select phone from account"), QueryLimits{})
	if err != nil || len(sh.Rows) != 1 {
		t.Fatalf("反查失败不应让查询失败：%v", err)
	}
	if sh.SourcesKnown || sh.Columns[0].Source != nil {
		t.Errorf("反查失败时来源应未知：known=%v src=%+v", sh.SourcesKnown, sh.Columns[0].Source)
	}
}
