// @author: kevinkelly
// @license: Apache Licence
// @file: views_test.go
// @time: 2026/10/28

package dm

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	dmdrv "gitee.com/chunanyong/dm"

	"dbtool/internal/engine"
)

func TestViewDefs(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "FROM all_views", cols: []string{"OWNER", "VIEW_NAME", "TEXT"}, rows: [][]driver.Value{
		{"HR", "EMP_V", fakeClob{"select phone p, id from employees"}}, // TEXT 是大字段：驱动返回 *DmClob
		{"HR", "BIG_V", strings.Repeat("x", engine.MaxViewDefBytes+1)},
		{"SALES", "ORD_V", nil},
	}})
	s.add(fakeRule{match: "FROM all_tab_columns", cols: []string{"OWNER", "TABLE_NAME", "COLUMN_NAME"}, rows: [][]driver.Value{
		{"HR", "EMP_V", "CONTACT"}, {"HR", "EMP_V", "ID"}, {"SALES", "ORD_V", "N"},
	}})
	e := newFakeEngine(t, s)
	defs, err := e.ViewDefs(context.Background(), "HR", []engine.ViewRef{
		{Name: "EMP_V"}, {Schema: "HR", Name: "EMP_V"}, {Schema: "HR", Name: "BIG_V"}, {Schema: "SALES", Name: "ORD_V"},
		{Schema: "SYS", Name: "ALL_VIEWS"}, {Schema: "SYSAUDITOR", Name: "V"}, {Name: "X'Y"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 3 {
		t.Fatalf("defs = %+v", defs)
	}
	by := map[string]engine.ViewDef{}
	for _, d := range defs {
		by[d.Schema+"."+d.Name] = d
	}
	if d := by["HR.EMP_V"]; d.Definition != "select phone p, id from employees" || strings.Join(d.ColumnNames, ",") != "CONTACT,ID" {
		t.Errorf("EMP_V: %+v", d)
	}
	if d := by["HR.BIG_V"]; d.Definition != "" {
		t.Errorf("过大的定义当作读不到: %d 字节", len(d.Definition))
	}
	if d := by["SALES.ORD_V"]; d.Definition != "" || strings.Join(d.ColumnNames, ",") != "N" {
		t.Errorf("定义为 NULL: %+v", d)
	}
	q := ""
	for _, l := range s.log() {
		if strings.Contains(l, "FROM all_views") {
			q = l
		}
	}
	for _, sub := range []string{"(owner = 'HR' AND view_name = 'EMP_V')", "(owner = 'HR' AND view_name = 'X''Y')"} {
		if !strings.Contains(q, sub) {
			t.Errorf("目录查询缺少 %q：\n%s", sub, q)
		}
	}
	if strings.Contains(q, "'SYS'") || strings.Contains(q, "SYSAUDITOR") || strings.Count(q, "view_name = 'EMP_V'") != 1 {
		t.Errorf("系统 schema 不查、同一个视图不重复：\n%s", q)
	}
}

func TestViewDefsUnsupportedAndEmpty(t *testing.T) {
	e := newFakeEngine(t, &fakeScript{})
	if defs, err := e.ViewDefs(context.Background(), "HR", nil); err != nil || defs != nil {
		t.Errorf("没有要查的名字: %v %v", defs, err)
	}
	s := &fakeScript{}
	s.add(fakeRule{match: "FROM all_views", err: &dmdrv.DmError{ErrCode: -2106, ErrText: "无效的表或视图名[ALL_VIEWS]"}})
	e2 := newFakeEngine(t, s)
	if _, err := e2.ViewDefs(context.Background(), "HR", []engine.ViewRef{{Name: "V"}}); !errors.Is(err, engine.ErrViewDefsUnsupported) {
		t.Errorf("-2106 应是目录不可用: %v", err)
	}
	s3 := &fakeScript{}
	s3.add(fakeRule{match: "FROM all_views", err: &dmdrv.DmError{ErrCode: -5513, ErrText: "没有权限"}})
	e3 := newFakeEngine(t, s3)
	if _, err := e3.ViewDefs(context.Background(), "HR", []engine.ViewRef{{Name: "V"}}); err == nil || errors.Is(err, engine.ErrViewDefsUnsupported) {
		t.Errorf("其它错误原样返回: %v", err)
	}
}
