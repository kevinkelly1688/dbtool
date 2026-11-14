// @author: kevinkelly
// @license: Apache Licence
// @file: names_test.go
// @time: 2026/10/28

package engine

import (
	"strings"
	"testing"
	"time"
)

func time0(h, m int) time.Duration { return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute }

func TestExpandTableName(t *testing.T) {
	at := time.Date(2026, 3, 7, 1, 2, 3, 0, time.UTC)
	cases := []struct{ tpl, tbl, want string }{
		{"{table}_{yyyy}", "orders", "orders_2026"},
		{"{table}_{yyyymm}", "orders", "orders_202603"},
		{"{table}_{yyyy}{mm}{dd}", "orders", "orders_20260307"},
		{"{table}_{yyyymmdd}", "orders", "orders_20260307"},
		{"arch_{table}", "t", "arch_t"},
		{"", "orders", "orders_archive"},
		{"  ", "orders", "orders_archive"},
		{"{table}_{unknown}", "orders", "orders__unknown_"},
		{"{table}-x", "orders", "orders_x"},
		{"{table}.x", "orders", "orders_x"},
		{"{table}_{yyyy}", "订单", "订单_2026"},
	}
	for _, c := range cases {
		if got := ExpandTableName(c.tpl, c.tbl, at); got != c.want {
			t.Errorf("ExpandTableName(%q,%q)=%q want %q", c.tpl, c.tbl, got, c.want)
		}
	}
	long := ExpandTableName("{table}_{yyyy}", strings.Repeat("a", 80), at)
	if len([]rune(long)) != 64 {
		t.Errorf("应截到 64 位：%d", len([]rune(long)))
	}
}

func TestValidTableTemplate(t *testing.T) {
	good := []string{"{table}_{yyyy}", "{table}_{yyyymm}", "bak_{table}", "{table}_{yyyy}_{mm}_{dd}", "{table}_{yyyymmdd}"}
	bad := []string{"", "  ", "{table}-{yyyy}", "{table}.x", "{table}_{foo}", "{table", "table}", "{{table}}", "{table} x", "a;b", strings.Repeat("a", 70)}
	for _, g := range good {
		if err := ValidTableTemplate(g); err != nil {
			t.Errorf("%q 应合法：%v", g, err)
		}
	}
	for _, b := range bad {
		if err := ValidTableTemplate(b); err == nil {
			t.Errorf("%q 应非法", b)
		}
	}
}

func TestExpandPath(t *testing.T) {
	at := time.Date(2026, 3, 7, 1, 2, 3, 0, time.UTC)
	spec := Spec{TaskID: 5, RunID: 42, DB: "shop", Table: "orders"}
	cases := []struct{ tpl, inst, want string }{
		{"{inst}/{db}/{table}/{yyyy}/{mm}/{dd}/{run_id}", "prod-db01", "prod-db01/shop/orders/2026/03/07/42"},
		{"{db}/{table}/{yyyymmdd}", "x", "shop/orders/20260307"},
		{"", "prod", "prod/shop/orders/2026/03/07/42"},
		{"a//b/./c", "x", "a/b/c"},
		{"{inst}/x", "a/b", "a_b/x"},
		{"{inst}/x", "../../etc", "______etc/x"},
		{"{inst}/x", "实例 1", "实例_1/x"},
		{"../etc/{table}", "x", "_/etc/orders"},
		{"/abs/{table}", "x", "abs/orders"},
		{"a\\b/{table}", "x", "a/b/orders"},
		{"x/{task_id}", "x", "x/5"},
		{"x/{bogus}", "x", "x/_bogus_"},
		{"p;q $(rm)/{table}", "x", "p_q___rm_/orders"},
	}
	for _, c := range cases {
		got := ExpandPath(c.tpl, spec, c.inst, at)
		if got != c.want {
			t.Errorf("ExpandPath(%q,%q)=%q want %q", c.tpl, c.inst, got, c.want)
		}
		if strings.Contains(got, "..") || strings.HasPrefix(got, "/") || strings.Contains(got, "\\") {
			t.Errorf("路径不安全：%q", got)
		}
	}
	// 库表名里的恶意值
	evil := Spec{DB: "..", Table: "a/../b", RunID: 1}
	got := ExpandPath("{db}/{table}", evil, "i", at)
	if strings.Contains(got, "..") || strings.Contains(got, "//") {
		t.Errorf("恶意库表名展开后不安全：%q", got)
	}
}

func TestValidPathTemplate(t *testing.T) {
	for _, g := range []string{"{inst}/{db}/{table}/{yyyy}/{mm}/{dd}/{run_id}", "a/b", "x-y_z.v/{table}", "{yyyymm}/{task_id}"} {
		if err := ValidPathTemplate(g); err != nil {
			t.Errorf("%q 应合法：%v", g, err)
		}
	}
	for _, b := range []string{"", "/a", "a/../b", "..", "a b", "a\\b", "{foo}", "a/{table", strings.Repeat("a", 129), "a;b", "a:b"} {
		if err := ValidPathTemplate(b); err == nil {
			t.Errorf("%q 应非法", b)
		}
	}
}
