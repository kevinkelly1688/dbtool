// @author: kevinkelly
// @license: Apache Licence
// @file: value_test.go
// @time: 2026/10/28

package postgres

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestConvertScalars(t *testing.T) {
	cases := []struct {
		name string
		oid  uint32
		raw  string
		want any
	}{
		{"bool t", 16, "t", true},
		{"bool f", 16, "f", false},
		{"int2", 21, "-32768", int64(-32768)},
		{"int4", 23, "2147483647", int64(2147483647)},
		{"int8 小", 20, "123456789012", int64(123456789012)},
		{"int8 安全边界", 20, "9007199254740991", int64(9007199254740991)},
		{"int8 超出安全范围用字符串", 20, "9007199254740992", "9007199254740992"},
		{"int8 最大", 20, "9223372036854775807", "9223372036854775807"},
		{"int8 最小", 20, "-9223372036854775808", "-9223372036854775808"},
		{"int8 负安全边界", 20, "-9007199254740991", int64(-9007199254740991)},
		{"oid", 26, "4294967295", int64(4294967295)},
		{"xid", 28, "12345", int64(12345)},
		{"xid8 用字符串", 5069, "18446744073709551615", "18446744073709551615"},
		{"float8", 701, "3.14", 3.14},
		{"float8 指数", 701, "1e+100", 1e100},
		{"float4 最短表示", 700, "0.1", 0.1},
		{"float NaN", 701, "NaN", "NaN"},
		{"float Infinity", 701, "Infinity", "Infinity"},
		{"float -Infinity", 701, "-Infinity", "-Infinity"},
		{"numeric 保精度", 1700, "12345678901234567890.123456789", "12345678901234567890.123456789"},
		{"numeric NaN", 1700, "NaN", "NaN"},
		{"money", 790, "$1,234.50", "$1,234.50"},
		{"text", 25, "héllo 世界", "héllo 世界"},
		{"varchar", 1043, "abc", "abc"},
		{"uuid", 2950, "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"},
		{"inet", 869, "192.168.0.1/24", "192.168.0.1/24"},
		{"interval", 1186, "1 day 02:03:04", "1 day 02:03:04"},
		{"date", 1082, "2024-02-29", "2024-02-29"},
		{"date infinity", 1082, "infinity", "infinity"},
		{"date BC", 1082, "0044-03-15 BC", "0044-03-15 BC"},
		{"time", 1083, "12:34:56.789", "12:34:56.789"},
		{"timestamp", 1114, "2024-02-29 12:34:56.123456", "2024-02-29T12:34:56.123456"},
		{"timestamp 无小数", 1114, "2024-02-29 12:34:56", "2024-02-29T12:34:56"},
		{"timestamp infinity", 1114, "-infinity", "-infinity"},
		{"timestamp BC 原样", 1114, "0044-03-15 12:00:00 BC", "0044-03-15 12:00:00 BC"},
		{"timestamptz +08", 1184, "2024-02-29 12:34:56.123+08", "2024-02-29T12:34:56.123+08:00"},
		{"timestamptz -05", 1184, "2024-02-29 12:34:56-05", "2024-02-29T12:34:56-05:00"},
		{"timestamptz +05:30", 1184, "2024-02-29 12:34:56+05:30", "2024-02-29T12:34:56+05:30"},
		{"timestamptz +00", 1184, "2024-02-29 12:34:56+00", "2024-02-29T12:34:56+00:00"},
		{"timestamptz 历史秒级偏移", 1184, "1900-01-01 00:00:00+08:05:43", "1900-01-01T00:00:00+08:05:43"},
		{"timestamptz infinity", 1184, "infinity", "infinity"},
		{"timetz", 1266, "12:34:56+08", "12:34:56+08:00"},
		{"timetz 半时区", 1266, "12:34:56.5-03:30", "12:34:56.5-03:30"},
		{"jsonb 紧凑", 3802, `{"a": 1, "b": [1, 2, {"c": null}]}`, `{"a":1,"b":[1,2,{"c":null}]}`},
		{"json 紧凑", 114, "{ \"a\" :  1 }", `{"a":1}`},
		{"json 标量", 114, "  42 ", "42"},
		{"json 非法原样", 114, "{oops", "{oops"},
		{"range", 3904, "[1,5)", "[1,5)"},
		{"tsvector", 3614, "'a':1 'b':2", "'a':1 'b':2"},
		{"未知类型原样", 99999, "(1,abc)", "(1,abc)"},
		{"空串", 25, "", ""},
	}
	for _, c := range cases {
		var st shapeStats
		got := st.convert(c.oid, []byte(c.raw))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s：convert(%d, %q) = %#v，want %#v", c.name, c.oid, c.raw, got, c.want)
		}
	}
	var st shapeStats
	if st.convert(25, nil) != nil || st.convert(1700, nil) != nil || st.convert(17, nil) != nil || st.convert(1007, nil) != nil {
		t.Error("NULL 应为 nil")
	}
}

func TestConvertArrays(t *testing.T) {
	cases := []struct {
		name string
		oid  uint32
		raw  string
		want string
	}{
		{"int4[]", 1007, "{1,2,3}", "[1,2,3]"},
		{"int4[] 含 NULL", 1007, "{1,NULL,3}", "[1,null,3]"},
		{"空数组", 1007, "{}", "[]"},
		{"text[]", 1009, `{a,"b c","d,e","f\"g","h\\i",NULL,"NULL"}`, `["a","b c","d,e","f\"g","h\\i",null,"NULL"]`},
		{"text[] 大小写 null", 1009, "{null,Null}", "[null,null]"},
		{"二维", 1007, "{{1,2},{3,4}}", "[[1,2],[3,4]]"},
		{"三维空", 1007, "{{{}}}", "[[[]]]"},
		{"下标范围前缀", 1007, "[0:2]={7,8,9}", "[7,8,9]"},
		{"int8[] 大数转字符串", 1016, "{1,9007199254740993}", `[1,"9007199254740993"]`},
		{"bool[]", 1000, "{t,f,NULL}", "[true,false,null]"},
		{"float8[]", 1022, "{1.5,NaN,Infinity}", `[1.5,"NaN","Infinity"]`},
		{"numeric[] 字符串", 1231, "{1.10,2.20}", `["1.10","2.20"]`},
		{"timestamptz[]", 1185, `{"2024-01-02 03:04:05+08","2024-01-02 03:04:05.5-05"}`, `["2024-01-02T03:04:05+08:00","2024-01-02T03:04:05.5-05:00"]`},
		{"uuid[]", 2951, "{a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11}", `["a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"]`},
		{"jsonb[]", 3807, `{"{\"a\": 1}","[1, 2]"}`, `["{\"a\":1}","[1,2]"]`},
		{"bytea[] 原样", 1001, `{"\\x4142",NULL}`, `["\\x4142",null]`},
		{"含 HTML 字符不转义", 1009, `{"<a&b>"}`, `["<a&b>"]`},
		{"含空白", 1007, "{ 1 , 2 }", "[1,2]"},
		{"unicode", 1009, `{"世界",é}`, `["世界","é"]`},
	}
	for _, c := range cases {
		var st shapeStats
		got := st.convert(c.oid, []byte(c.raw))
		s, ok := got.(string)
		if !ok || s != c.want {
			t.Errorf("%s：convert(%d, %q) = %#v，want %q", c.name, c.oid, c.raw, got, c.want)
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Errorf("%s：结果不是合法 JSON：%v", c.name, err)
		}
	}
	// 解析失败的数组原样返回
	for _, raw := range []string{"{1,2", "{\"a", "not an array", "{1,2}}", "{{1,2}"} {
		var st shapeStats
		if got := st.convert(1007, []byte(raw)); got != raw {
			t.Errorf("坏数组 %q 应原样：%#v", raw, got)
		}
	}
	// 过深的嵌套
	deep := strings.Repeat("{", 40) + strings.Repeat("}", 40)
	var st shapeStats
	if got := st.convert(1007, []byte(deep)); got != deep {
		t.Errorf("过深嵌套应原样：%#v", got)
	}
}

func TestConvertLongText(t *testing.T) {
	var st shapeStats
	s := strings.Repeat("世", MaxCellBytes) // 3 字节一个字符
	got := st.convert(25, []byte(s)).(string)
	if !utf8.ValidString(got) {
		t.Fatal("截断后必须是合法 UTF-8")
	}
	if len(got) > MaxCellBytes+100 || !strings.Contains(got, "已截断，共 196608 字节") {
		t.Fatalf("len=%d", len(got))
	}
	if st.Cut != 1 {
		t.Errorf("Cut=%d", st.Cut)
	}
	// 长 json 也受 64KB 限制
	long := `{"k":"` + strings.Repeat("v", 70<<10) + `"}`
	if g := st.convert(3802, []byte(long)).(string); !strings.Contains(g, "已截断") {
		t.Error("超长 jsonb 应被截断")
	}
	// 长数组
	arr := "{" + strings.TrimSuffix(strings.Repeat("12345,", 20000), ",") + "}"
	if g := st.convert(1007, []byte(arr)).(string); !strings.Contains(g, "已截断") {
		t.Error("超长数组应被截断")
	}
	// 非法 UTF-8 被替换
	if g := st.convert(25, []byte("a\xffb")).(string); !utf8.ValidString(g) {
		t.Error("非法 UTF-8 应被替换")
	}
}

func TestByteaConvert(t *testing.T) {
	var st shapeStats
	if got := st.convert(17, []byte(`\xdeadbeef`)); got != `\xdeadbeef` {
		t.Errorf("%v", got)
	}
	exact := `\x` + strings.Repeat("ab", MaxCellBytes)
	if got := st.convert(17, []byte(exact)); got != exact {
		t.Error("恰好 64KB 不应截断")
	}
	over := `\x` + strings.Repeat("ab", MaxCellBytes+1)
	got := st.convert(17, []byte(over)).(string)
	if !strings.Contains(got, "共 65537 字节") || len(got) != 2+2*MaxCellBytes+len("…（已截断，共 65537 字节）") {
		t.Errorf("len=%d", len(got))
	}
	if got := st.convert(17, []byte("plain")); got != "plain" { // bytea_output=escape 的兜底
		t.Errorf("%v", got)
	}
	if st.Binary != 4 {
		t.Errorf("Binary=%d", st.Binary)
	}
	// bytea 数组每个元素保持文本
	if got := st.convert(1001, []byte(`{"\\xab"}`)); got != `["\\xab"]` {
		t.Errorf("%v", got)
	}
}

func TestTypeName(t *testing.T) {
	for oid, want := range map[uint32]string{23: "int4", 1007: "int4[]", 1009: "text[]", 1184: "timestamptz", 3802: "jsonb", 99999: "", 1185: "timestamptz[]"} {
		if got := typeName(oid); got != want {
			t.Errorf("typeName(%d) = %q，want %q", oid, got, want)
		}
	}
}
