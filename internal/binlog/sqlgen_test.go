// @author: kevinkelly
// @license: Apache Licence
// @file: sqlgen_test.go
// @time: 2026/10/28

package binlog

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/replication"
)

func TestLiterals(t *testing.T) {
	enum := colMeta{Name: "e", DataType: "enum", Enum: []string{"a", "b'c", "d"}}
	set := colMeta{Name: "s", DataType: "set", Set: []string{"x", "y", "z"}}
	cases := []struct {
		name string
		col  colMeta
		v    any
		want string
	}{
		{"null", colMeta{}, nil, "NULL"},
		{"int", colMeta{DataType: "int"}, int32(-5), "-5"},
		{"unsigned tinyint 负数转回", colMeta{DataType: "tinyint", Unsigned: true}, int8(-56), "200"},
		{"unsigned smallint", colMeta{DataType: "smallint", Unsigned: true}, int16(-1), "65535"},
		{"unsigned int", colMeta{DataType: "int", Unsigned: true}, int32(-1), "4294967295"},
		{"unsigned bigint", colMeta{DataType: "bigint", Unsigned: true}, int64(-1), "18446744073709551615"},
		{"unsigned mediumint", colMeta{DataType: "mediumint", Unsigned: true}, int32(-1), "16777215"},
		{"mediumint 有符号", colMeta{DataType: "mediumint"}, int32(-1), "-1"},
		{"已是无符号", colMeta{DataType: "int", Unsigned: true}, uint32(7), "7"},
		{"enum 标签", enum, int64(2), "'b''c'"},
		{"enum 0 是空串", enum, int64(0), "''"},
		{"enum 越界退回数字", enum, int64(9), "9"},
		{"set", set, int64(5), "'x,z'"},
		{"set 空", set, int64(0), "''"},
		{"bit 高位", colMeta{DataType: "bit"}, int64(-1), "18446744073709551615"},
		{"float32", colMeta{DataType: "float"}, float32(1.1), "1.1"},
		{"float64", colMeta{DataType: "double"}, 1.5, "1.5"},
		{"decimal", colMeta{DataType: "decimal"}, "-12.30", "-12.30"},
		{"decimal 非数字退回字符串", colMeta{DataType: "decimal"}, "abc", "'abc'"},
		{"varchar 转义", colMeta{DataType: "varchar"}, "a'b\\c\n\x00\x1a\r", `'a''b\\c\n\0\Z\r'`},
		{"中文", colMeta{DataType: "varchar"}, "你好", "'你好'"},
		{"非 UTF-8 用十六进制", colMeta{DataType: "varchar"}, "\xff\xfe", "0xfffe"},
		{"blob", colMeta{DataType: "blob"}, []byte{1, 2, 255}, "0x0102ff"},
		{"空 blob", colMeta{DataType: "blob"}, []byte{}, "''"},
		{"text 字节", colMeta{DataType: "text"}, []byte("hi'"), "'hi'''"},
		{"text 非 UTF-8", colMeta{DataType: "text"}, []byte{0xc3, 0x28}, "0xc328"},
		{"varbinary 字符串", colMeta{DataType: "varbinary"}, "ab", "0x6162"},
		{"json", colMeta{DataType: "json"}, `{"a": "it's"}`, `'{"a": "it''s"}'`},
		{"空 json", colMeta{DataType: "json"}, []byte{}, "'null'"},
		{"geometry", colMeta{DataType: "geometry"}, []byte{0, 0, 0, 0, 1, 2, 3}, "ST_GeomFromWKB(0x010203,0)"},
		{"datetime", colMeta{DataType: "datetime"}, "2023-01-02 03:04:05", "'2023-01-02 03:04:05'"},
		{"year", colMeta{DataType: "year"}, 2024, "2024"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tm := &tableMeta{Cols: []colMeta{c.col}}
			got, err := tm.lit(0, c.v)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %s want %s", got, c.want)
			}
		})
	}
	tm := &tableMeta{Cols: []colMeta{{}}}
	for _, v := range []any{math.NaN(), math.Inf(1), &replication.JsonDiff{}, struct{}{}} {
		if _, err := tm.lit(0, v); !errors.Is(err, errUnsupportedValue) {
			t.Errorf("%T 应报不支持: %v", v, err)
		}
	}
}

func TestParseQuotedList(t *testing.T) {
	got := parseQuotedList("enum('a','b''c','d,e','')")
	want := []string{"a", "b'c", "d,e", ""}
	if len(got) != len(want) {
		t.Fatalf("%q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %q vs %q", i, got[i], want[i])
		}
	}
	if parseQuotedList("int") != nil {
		t.Error("非 enum 类型应返回空")
	}
}

func TestWhereSkipsFloatAndGeometry(t *testing.T) {
	tm := &tableMeta{Schema: "d", Table: "t", Cols: []colMeta{
		{Name: "a", DataType: "int"}, {Name: "f", DataType: "float"}, {Name: "g", DataType: "geometry"}, {Name: "j", DataType: "json"}, {Name: "gen", DataType: "int", Generated: true}}}
	cond, byPK, err := tm.where(genOpt{}, []any{1, float32(1.1), []byte{0, 0, 0, 0, 1}, `{"k":1}`, 9}, nil)
	if err != nil || byPK {
		t.Fatal(err, byPK)
	}
	if cond != "`a` = 1 AND `j` = CAST('{\"k\":1}' AS JSON)" {
		t.Errorf("条件不对: %s", cond)
	}
	// 只有浮点列时，只好用浮点列
	tm2 := &tableMeta{Table: "t", Cols: []colMeta{{Name: "f", DataType: "double"}}}
	cond, _, err = tm2.where(genOpt{}, []any{2.5}, nil)
	if err != nil || cond != "`f` = 2.5" {
		t.Errorf("%s %v", cond, err)
	}
}

func TestGeneratedColumnsExcludedFromWrites(t *testing.T) {
	tm := &tableMeta{Schema: "d", Table: "t", PK: []int{0}, Cols: []colMeta{
		{Name: "id", DataType: "int", PK: true}, {Name: "a", DataType: "int"}, {Name: "g", DataType: "int", Generated: true}}}
	ins, err := tm.insertSQL(genOpt{}, []any{1, 2, 3}, nil)
	if err != nil || ins != "INSERT INTO `d`.`t` (`id`,`a`) VALUES (1,2);" {
		t.Errorf("%s %v", ins, err)
	}
	upd, err := tm.updateSQL(genOpt{}, []any{1, 5, 6}, nil, []any{1, 2, 3}, nil)
	if err != nil || upd != "UPDATE `d`.`t` SET `a` = 5 WHERE `id` = 1;" {
		t.Errorf("%s %v", upd, err)
	}
}

func TestUpdateWithoutChangeStillValid(t *testing.T) {
	tm := &tableMeta{Schema: "d", Table: "t", PK: []int{0}, Cols: []colMeta{{Name: "id", DataType: "int", PK: true}, {Name: "a", DataType: "int"}}}
	upd, err := tm.updateSQL(genOpt{}, []any{1, 2}, nil, []any{1, 2}, nil)
	if err != nil || !strings.HasPrefix(upd, "UPDATE `d`.`t` SET `id` = 1 WHERE `id` = 1") {
		t.Errorf("%s %v", upd, err)
	}
}

func TestIdentQuoting(t *testing.T) {
	if quoteIdent("a`b") != "`a``b`" {
		t.Error(quoteIdent("a`b"))
	}
	tm := &tableMeta{Schema: "my`db", Table: "t"}
	if tm.qualified(false) != "`my``db`.`t`" || tm.qualified(true) != "`t`" {
		t.Error(tm.qualified(false))
	}
}
