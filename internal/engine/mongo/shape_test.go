// @author: kevinkelly
// @license: Apache Licence
// @file: shape_test.go
// @time: 2026/10/28

package mongo

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func nanValue() float64 { return math.NaN() }

func TestShapeCell(t *testing.T) {
	oid, _ := bson.ObjectIDFromHex("507f1f77bcf86cd799439011")
	dec, _ := bson.ParseDecimal128("1.50")
	s := newShaper(0)
	cases := []struct {
		in   any
		want any
	}{
		{nil, nil},
		{"x", "x"},
		{true, true},
		{int32(7), int32(7)},
		{int64(-9223372036854775808), "-9223372036854775808"},
		{1.5, 1.5},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
		{oid, `ObjectId("507f1f77bcf86cd799439011")`},
		{bson.NewDateTimeFromTime(time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)), "2024-05-06T07:08:09.000Z"},
		{bson.NewDateTimeFromTime(time.Date(2024, 5, 6, 7, 8, 9, 0, time.FixedZone("x", 8*3600))), "2024-05-05T23:08:09.000Z"},
		{dec, "1.50"},
		{bson.Timestamp{T: 1, I: 2}, "Timestamp(1, 2)"},
		{bson.Regex{Pattern: "a.b", Options: "i"}, "/a.b/i"},
		{bson.MinKey{}, "MinKey"},
		{bson.MaxKey{}, "MaxKey"},
		{bson.Undefined{}, nil},
		{bson.Symbol("sym"), "sym"},
		{bson.Binary{Subtype: 4, Data: []byte{0x12, 0x3e, 0x45, 0x67, 0xe8, 0x9b, 0x12, 0xd3, 0xa4, 0x56, 0x42, 0x66, 0x14, 0x17, 0x40, 0x00}}, `UUID("123e4567-e89b-12d3-a456-426614174000")`},
		{bson.Binary{Subtype: 128, Data: []byte{0xde, 0xad}}, `BinData(128, \xdead)`},
		{bson.D{{Key: "a", Value: int64(5)}, {Key: "b", Value: bson.A{"x", nil}}}, `{"a":5,"b":["x",null]}`},
		{bson.A{int32(1), 2.5}, `[1,2.5]`},
		{bson.M{"b": 1, "a": 2}, `{"a":2,"b":1}`},
		{bson.D{}, `{}`},
	}
	for _, c := range cases {
		if got := s.cell(c.in); got != c.want {
			t.Errorf("%#v: got %#v want %#v", c.in, got, c.want)
		}
	}
}

func TestShapeBinaryTruncation(t *testing.T) {
	s := newShaper(0)
	data := make([]byte, 100<<10)
	got := s.cell(bson.Binary{Subtype: 0, Data: data}).(string)
	if !strings.Contains(got, "已截断，共 102400 字节") || len(got) > 2*(64<<10)+100 {
		t.Errorf("二进制超过 64KB 应截断：len=%d", len(got))
	}
}

func TestShapeStringTruncationRuneBoundary(t *testing.T) {
	s := newShaper(10)
	got := s.cell(strings.Repeat("中", 20)).(string)
	if !strings.HasPrefix(got, "中中中") || strings.Contains(got, "�") || s.cut != 1 {
		t.Errorf("按字符边界截断：%q cut=%d", got, s.cut)
	}
	if s.cell("short") != "short" || s.cut != 1 {
		t.Error("短字符串不应截断")
	}
	// 嵌套 JSON 超长同样截断
	s = newShaper(20)
	d := bson.D{{Key: "k", Value: strings.Repeat("v", 100)}}
	if g := s.cell(d).(string); !strings.Contains(g, "已截断") {
		t.Errorf("嵌套 JSON 应截断：%q", g)
	}
}

func TestDocRowsUnionAndOrder(t *testing.T) {
	s := newShaper(0)
	cols, rows := s.docRows([]bson.D{
		{{Key: "_id", Value: int32(1)}, {Key: "a", Value: "x"}},
		{{Key: "_id", Value: int32(2)}, {Key: "b", Value: int32(2)}, {Key: "a", Value: "y"}},
		{},
	})
	if strings.Join(cols, ",") != "_id,a,b" {
		t.Fatalf("cols=%v", cols)
	}
	if rows[0][2] != nil || rows[1][2] != int32(2) || rows[2][0] != nil || len(rows[2]) != 3 {
		t.Errorf("rows=%v", rows)
	}
}

func TestDocRowsExtraFold(t *testing.T) {
	s := newShaper(0)
	var d bson.D
	for i := 0; i < MaxColumns+5; i++ {
		d = append(d, bson.E{Key: fmt.Sprintf("f%03d", i), Value: int32(i)})
	}
	d2 := bson.D{{Key: "f000", Value: int32(0)}, {Key: "only2", Value: "z"}}
	cols, rows := s.docRows([]bson.D{d, d2})
	if len(cols) != MaxColumns+1 || cols[MaxColumns] != "_extra" {
		t.Fatalf("列数=%d 最后一列=%q", len(cols), cols[len(cols)-1])
	}
	extra, _ := rows[0][MaxColumns].(string)
	if !strings.Contains(extra, `"f200":200`) || !strings.Contains(extra, `"f204":204`) || strings.Contains(extra, `"f199"`) {
		t.Errorf("_extra=%q", extra)
	}
	// only2 出现得晚（前 200 列已满），落入第二行的 _extra
	if e2, _ := rows[1][MaxColumns].(string); e2 != `{"only2":"z"}` {
		t.Errorf("第二行 _extra=%v", rows[1][MaxColumns])
	}
	if len(rows[0]) != MaxColumns+1 || len(rows[1]) != MaxColumns+1 {
		t.Errorf("行宽不一致")
	}
	// 没有超出时不产生 _extra
	cols, _ = s.docRows([]bson.D{d2})
	if len(cols) != 2 {
		t.Errorf("cols=%v", cols)
	}
	// 与真实字段重名时 _extra 改名
	var d3 bson.D
	d3 = append(d3, bson.E{Key: "_extra", Value: 1})
	for i := 0; i < MaxColumns+1; i++ {
		d3 = append(d3, bson.E{Key: fmt.Sprintf("g%03d", i), Value: 1})
	}
	cols, _ = s.docRows([]bson.D{d3})
	if cols[len(cols)-1] != "_extra_" {
		t.Errorf("重名处理：%q", cols[len(cols)-1])
	}
}
