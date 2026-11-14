// @author: kevinkelly
// @license: Apache Licence
// @file: cap_test.go
// @time: 2026/10/28

package mssql

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestResultCapCellAndTotal(t *testing.T) {
	// 不设上限：原样通过
	c := &resultCap{}
	row := []any{strings.Repeat("x", 1<<20), int64(1)}
	if !c.fit(row, true) || c.cut != 0 || c.limited {
		t.Fatalf("不限制: %+v", c)
	}

	// 单元格超限：截断并带说明，不切在中文字符中间
	c = &resultCap{cell: 10}
	zh := strings.Repeat("中", 10) // 30 字节
	row = []any{zh, "短", int64(1)}
	if !c.fit(row, true) || c.cut != 1 {
		t.Fatalf("单元格截断: %+v", c)
	}
	s := row[0].(string)
	if !strings.Contains(s, "已截断，共 30 字节") || !utf8.ValidString(s) {
		t.Errorf("截断结果: %q", s)
	}
	if row[1].(string) != "短" {
		t.Errorf("短单元格不动: %v", row[1])
	}

	// 总量超限：第一行总是保留，之后的行被拒并标记 limited
	c = &resultCap{total: 100}
	first := []any{strings.Repeat("a", 500)}
	if !c.fit(first, true) {
		t.Fatal("第一行即使超过总量上限也要保留")
	}
	if c.fit([]any{"b"}, false) || !c.limited {
		t.Errorf("已超总量，后面的行应被拒: %+v", c)
	}

	// 总量内的多行都能放下
	c = &resultCap{total: 100}
	for i := 0; i < 5; i++ {
		if !c.fit([]any{strings.Repeat("a", 10)}, i == 0) {
			t.Fatalf("第 %d 行应放得下: %+v", i, c)
		}
	}
	if c.limited {
		t.Error("没超总量不应标记")
	}
}
