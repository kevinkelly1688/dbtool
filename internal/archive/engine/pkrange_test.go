// @author: kevinkelly
// @license: Apache Licence
// @file: pkrange_test.go
// @time: 2026/10/28

package engine

import (
	"math"
	"strconv"
	"testing"
)

func TestReadUnorderedDecision(t *testing.T) {
	col := func(name, typ string) ColumnInfo { return ColumnInfo{Name: name, Type: typ} }
	cases := []struct {
		name string
		mode Mode
		pk   []string
		cols []ColumnInfo
		want bool
	}{
		{"整数主键 move", ModeMove, []string{"id"}, []ColumnInfo{col("id", "bigint(20)")}, true},
		{"无符号 bigint purge", ModePurge, []string{"id"}, []ColumnInfo{col("id", "bigint(20) unsigned")}, true},
		{"int 显示宽度", ModeMove, []string{"id"}, []ColumnInfo{col("id", "int(11)")}, true},
		{"decimal 主键", ModeMove, []string{"id"}, []ColumnInfo{col("id", "decimal(20,0)")}, true},
		{"datetime 复合主键", ModeMove, []string{"d", "id"}, []ColumnInfo{col("d", "datetime(3)"), col("id", "bigint")}, true},
		{"列名不区分大小写", ModeMove, []string{"ID"}, []ColumnInfo{col("id", "bigint")}, true},
		{"字符串主键仍排序读取", ModeMove, []string{"code"}, []ColumnInfo{col("code", "varchar(32)")}, false},
		{"复合主键里有字符串", ModeMove, []string{"tenant", "id"}, []ColumnInfo{col("tenant", "varchar(8)"), col("id", "bigint")}, false},
		{"浮点主键", ModeMove, []string{"f"}, []ColumnInfo{col("f", "double")}, false},
		{"time 主键（可能为负、超过 24 小时）", ModeMove, []string{"t"}, []ColumnInfo{col("t", "time")}, false},
		{"二进制主键", ModeMove, []string{"b"}, []ColumnInfo{col("b", "binary(16)")}, false},
		{"主键列不在列清单里", ModeMove, []string{"x"}, []ColumnInfo{col("id", "bigint")}, false},
		{"没有主键", ModeMove, nil, []ColumnInfo{col("id", "bigint")}, false},
		{"仅备份不删数据，始终翻页", ModeCopy, []string{"id"}, []ColumnInfo{col("id", "bigint")}, false},
	}
	for _, c := range cases {
		if got := readUnordered(c.mode, TableInfo{PrimaryKey: c.pk, Columns: c.cols}); got != c.want {
			t.Errorf("%s：readUnordered = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestPKRangeTracksMinMaxRegardlessOfOrder(t *testing.T) {
	info := TableInfo{PrimaryKey: []string{"id"}, Columns: []ColumnInfo{{Name: "id", Type: "bigint(20) unsigned"}}}
	cmps, ok := pkComparators(info)
	if !ok {
		t.Fatal("bigint 应能本地比较")
	}
	r := newPKRange(cmps)
	// 不按顺序给：字符串比较会把 "9" 排在 "10" 后面，必须按数值比；还有超过 int64 的无符号值
	r.add([][]string{{"9"}, {"100"}, {"10"}})
	r.add([][]string{{"2"}, {"18446744073709551615"}, {"1000000000000"}})
	if r.min[0] != "2" || r.max[0] != "18446744073709551615" {
		t.Errorf("范围 %v ~ %v", r.min, r.max)
	}
	// 复合主键：逐列比较，每列用自己的类型
	info2 := TableInfo{PrimaryKey: []string{"d", "id"}, Columns: []ColumnInfo{{Name: "d", Type: "datetime(6)"}, {Name: "id", Type: "int"}}}
	cmps2, _ := pkComparators(info2)
	r2 := newPKRange(cmps2)
	r2.add([][]string{
		{"2026-01-02 00:00:00", "5"}, {"2026-01-01 10:00:00.5", "100"}, {"2026-01-01 10:00:00", "9"},
		{"2026-01-01 10:00:00.25", "3"}, {"2026-01-01 10:00:00.5", "20"},
	})
	if got := r2.min[0] + "|" + r2.min[1]; got != "2026-01-01 10:00:00|9" {
		t.Errorf("复合主键最小值：%s", got)
	}
	if got := r2.max[0] + "|" + r2.max[1]; got != "2026-01-02 00:00:00|5" {
		t.Errorf("复合主键最大值：%s", got)
	}
	// 个数对不上的主键忽略，不会 panic
	r3 := newPKRange(cmps2)
	r3.add([][]string{{"x"}})
	if r3.min != nil || r3.max != nil {
		t.Errorf("列数不对应被忽略：%v %v", r3.min, r3.max)
	}
}

func TestColumnComparators(t *testing.T) {
	if cmpInteger("-5", "3") >= 0 || cmpInteger("10", "9") <= 0 || cmpInteger("7", "7") != 0 {
		t.Error("cmpInteger")
	}
	if cmpInteger("18446744073709551615", strconv.FormatInt(math.MaxInt64, 10)) <= 0 {
		t.Error("cmpInteger 超过 int64 的无符号值")
	}
	if cmpDecimal("10.5", "9.75") <= 0 || cmpDecimal("-0.5", "0") >= 0 || cmpDecimal("1.50", "1.5") != 0 {
		t.Error("cmpDecimal")
	}
	// 解析不了时退回字符串比较，不 panic
	if cmpInteger("abc", "abd") >= 0 || cmpDecimal("x", "y") >= 0 {
		t.Error("解析失败应退回字符串比较")
	}
	if colComparator("date")("2026-01-01", "2026-01-02") >= 0 {
		t.Error("date")
	}
}
