// @author: kevinkelly
// @license: Apache Licence
// @file: plan_test.go
// @time: 2026/10/28

package sqlopt

import (
	"strings"
	"testing"
)

func TestPrepareExplain(t *testing.T) {
	ok := []struct{ in, want, kind string }{
		{"select * from t where id = 1;", "select * from t where id = 1", TypeSelect},
		{"  -- c\nSELECT 1  ", "SELECT 1", TypeSelect},
		{"explain select * from t", "select * from t", TypeSelect},
		{"EXPLAIN FORMAT=JSON select 1", "select 1", TypeSelect},
		{"explain extended select 1", "select 1", TypeSelect},
		{"update t set a=1 where id=2", "update t set a=1 where id=2", TypeUpdate},
		{"delete from t where id=2", "delete from t where id=2", TypeDelete},
		{"insert into t (a) select a from s", "insert into t (a) select a from s", TypeInsert},
		{"replace into t select * from s", "replace into t select * from s", TypeReplace},
		{"with c as (select 1) select * from c", "with c as (select 1) select * from c", TypeSelect},
		{"(select 1) union (select 2)", "(select 1) union (select 2)", TypeSelect},
	}
	for _, c := range ok {
		got, kind, err := prepareExplain(c.in)
		if err != nil || got != c.want || kind != c.kind {
			t.Errorf("prepareExplain(%q) = %q, %q, %v；期望 %q, %q", c.in, got, kind, err, c.want, c.kind)
		}
	}
	bads := []string{
		"", "   ", "-- 只有注释",
		"select 1; select 2",
		"select 1; drop table t",
		"drop table t",
		"alter table t add index i(a)",
		"insert into t values (1)",
		"insert into t (a) values (1)",
		"truncate table t",
		"call p()",
		"set @a = 1",
		"show databases",
		"use mysql",
		"explain analyze select * from t",
		"explain",
		"desc t",
		"select sleep(100)",
		"select * from t where id = benchmark(10000000, md5('x'))",
		"select load_file('/etc/passwd')",
		"select * from t into outfile '/tmp/x'",
		"select * from t into dumpfile '/tmp/x'",
		"table t",
		"values row(1)",
		"/etc/passwd",
		"grant all on *.* to x",
	}
	for _, in := range bads {
		if got, _, err := prepareExplain(in); err == nil {
			t.Errorf("prepareExplain(%q) 应拒绝，得到 %q", in, got)
		} else if codeOf(t, err) != "bad_request" {
			t.Errorf("错误码 %v", err)
		}
	}
	if _, _, err := prepareExplain("select '" + strings.Repeat("x", MaxSQLBytes) + "'"); err == nil {
		t.Error("过长的 SQL 应拒绝")
	}
}

func TestPrepareSelectAndExternal(t *testing.T) {
	if s, err := prepareSelect("select a from t;"); err != nil || s != "select a from t" {
		t.Fatalf("%q %v", s, err)
	}
	for _, in := range []string{"update t set a=1", "delete from t", "insert into t select 1", "show tables", "select sleep(1)"} {
		if _, err := prepareSelect(in); err == nil {
			t.Errorf("SQLTuning 只允许 SELECT：%q", in)
		}
	}
	for _, in := range []string{"select 1", "update t set a = 1", "alter table t add index i(a)", "with a as (select 1) select * from a"} {
		if _, err := prepareExternal(in); err != nil {
			t.Errorf("prepareExternal(%q): %v", in, err)
		}
	}
	// 外部工具的 -query 参数可能把路径当文件读：非 SQL 关键字开头的一律拒绝
	for _, in := range []string{"/etc/passwd", "C:\\Windows\\win.ini", "../../secret.sql", "-config /tmp/x", "soar.yaml", "show tables", "select 1; select 2"} {
		if _, err := prepareExternal(in); err == nil {
			t.Errorf("prepareExternal(%q) 应拒绝", in)
		}
	}
}

func plan(rows ...[]any) *Table {
	return &Table{Columns: []string{"id", "select_type", "table", "type", "possible_keys", "key", "key_len", "ref", "rows", "filtered", "Extra"}, Rows: rows}
}

func TestPlanFindings(t *testing.T) {
	cases := []struct {
		name string
		row  []any
		want []string
		lv   string // 第一条建议的级别
	}{
		{"全表大表", []any{"1", "SIMPLE", "orders", "ALL", "idx_user", nil, nil, nil, "8214990", "10.00", "Using where; Using filesort"}, []string{"P001", "P005", "P003"}, LevelHigh},
		{"全表中表", []any{"1", "SIMPLE", "t", "ALL", nil, nil, nil, nil, "5000", "100.00", ""}, []string{"P001"}, LevelMedium},
		{"全表小表", []any{"1", "SIMPLE", "t", "ALL", nil, nil, nil, nil, "20", "100.00", ""}, []string{"P001"}, LevelLow},
		{"全索引扫描", []any{"1", "SIMPLE", "t", "index", nil, "idx_a", "4", nil, "500000", "100.00", "Using index"}, []string{"P002"}, LevelMedium},
		{"临时表", []any{"1", "SIMPLE", "t", "ref", "idx", "idx", "4", "const", "10", "100.00", "Using temporary"}, []string{"P004"}, LevelMedium},
		{"join buffer", []any{"1", "SIMPLE", "b", "ALL", nil, nil, nil, nil, "200000", "10.00", "Using where; Using join buffer (hash join)"}, []string{"P001", "P006"}, LevelHigh},
		{"相关子查询", []any{"2", "DEPENDENT SUBQUERY", "u", "ref", "idx", "idx", "4", "func", "1", "100.00", ""}, []string{"P007"}, LevelHigh},
		{"range checked", []any{"1", "SIMPLE", "t", "ALL", nil, nil, nil, nil, "10", "100.00", "Range checked for each record (index map: 0x1)"}, []string{"P008"}, LevelLow},
		{"过滤率低", []any{"1", "SIMPLE", "t", "ref", "idx", "idx", "4", "const", "200000", "5.00", "Using where"}, []string{"P009"}, LevelLow},
		{"不可能", []any{nil, "SIMPLE", nil, nil, nil, nil, nil, nil, nil, nil, "Impossible WHERE"}, []string{"P010"}, LevelInfo},
		{"良好", []any{"1", "SIMPLE", "t", "const", "PRIMARY", "PRIMARY", "4", "const", "1", "100.00", ""}, nil, ""},
		{"覆盖索引", []any{"1", "SIMPLE", "t", "ref", "idx", "idx", "4", "const", "100", "100.00", "Using index"}, nil, ""},
	}
	for _, c := range cases {
		got := planFindings(plan(c.row))
		for _, w := range c.want {
			if !hasID(got, w) {
				t.Errorf("%s：应命中 %s，实际 %s", c.name, w, ids(got))
			}
		}
		if len(c.want) == 0 && len(got) != 0 {
			t.Errorf("%s：不应有建议，实际 %s", c.name, ids(got))
		}
		if c.lv != "" && len(got) > 0 && got[0].Level != c.lv {
			t.Errorf("%s：第一条级别 %s，期望 %s", c.name, got[0].Level, c.lv)
		}
	}
	// 同一张表同一规则只报一次
	got := planFindings(plan(
		[]any{"1", "SIMPLE", "t", "ALL", nil, nil, nil, nil, "5000", "100", ""},
		[]any{"1", "UNION", "t", "ALL", nil, nil, nil, nil, "5000", "100", ""}))
	if len(got) != 1 {
		t.Errorf("重复建议应合并：%s", ids(got))
	}
	// 非 MySQL 传统格式（如 Doris 的文本计划）不解读
	if planFindings(&Table{Columns: []string{"Explain String"}, Rows: [][]any{{"PLAN FRAGMENT 0"}}}) != nil || planFindings(nil) != nil {
		t.Error("没有 type 列的计划不应解读")
	}
}

func TestFmtRows(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 8214990: "8,214,990"} {
		if got := fmtRows(in); got != want {
			t.Errorf("fmtRows(%d)=%s", in, got)
		}
	}
}
