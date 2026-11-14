// @author: kevinkelly
// @license: Apache Licence
// @file: views_test.go
// @time: 2026/10/28

package mssql

import (
	"reflect"
	"strings"
	"testing"

	"dbtool/internal/engine"
)

func TestViewDefsQuery(t *testing.T) {
	q, args := viewDefsQuery("ERP", []engine.ViewRef{{Schema: "crm", Name: "v_buyer"}, {Name: "v_inner"}, {Schema: "dbo", Name: "v]x"}})
	for _, sub := range []string{
		"FROM [ERP].sys.views v JOIN [ERP].sys.schemas s", "LEFT JOIN [ERP].sys.sql_modules m",
		"s.name NOT IN ('sys', 'INFORMATION_SCHEMA')",
		"(s.name = @p1 AND v.name = @p2) OR v.name = @p3 OR (s.name = @p4 AND v.name = @p5)",
	} {
		if !strings.Contains(q, sub) {
			t.Errorf("SQL 缺少 %q:\n%s", sub, q)
		}
	}
	if want := []any{"crm", "v_buyer", "v_inner", "dbo", "v]x"}; !reflect.DeepEqual(args, want) {
		t.Errorf("args=%v", args)
	}
	if strings.Contains(q, "v_buyer") || strings.Contains(q, "v]x") {
		t.Errorf("名字不应拼进 SQL: %s", q)
	}
	// 库名里的 ] 要转义；没有库名时用连接的默认库
	if q, _ := viewDefsQuery("a]b", []engine.ViewRef{{Name: "v"}}); !strings.Contains(q, "[a]]b].sys.views") {
		t.Errorf("库名没转义: %s", q)
	}
	if q, _ := viewDefsQuery("", []engine.ViewRef{{Name: "v"}}); !strings.Contains(q, "FROM sys.views v JOIN sys.schemas s") {
		t.Errorf("无库名: %s", q)
	}
}
