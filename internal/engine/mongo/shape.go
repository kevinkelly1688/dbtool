// @author: kevinkelly
// @license: Apache Licence
// @file: shape.go
// @time: 2026/10/28

package mongo

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// 结果整形：把 BSON 值转成 JSON 安全的单元格值。
//
//	ObjectId   → ObjectId("hex")            Int64 / Decimal128 → 字符串（保精度）
//	日期       → ISO-8601 UTC（毫秒）       Int32 / 有限 Double → JSON 数字；NaN / Infinity → 字符串
//	BinData    → BinData(子类型, \xHH...)，超过 64KB 截断；子类型 4 且 16 字节 → UUID("...")
//	嵌套文档 / 数组 → 紧凑的 Extended JSON（relaxed）字符串
//	null / undefined → nil；字符串 / 文本超过单元格上限 → 截断并标注
type shaper struct {
	maxCell int
	cut     int // 被截断的单元格数
	binary  int // 二进制单元格数（用于提示）
}

func newShaper(maxCell int) *shaper {
	if maxCell <= 0 {
		maxCell = DefaultMaxCellBytes
	}
	return &shaper{maxCell: maxCell}
}

func (s *shaper) cutString(v string) string {
	if len(v) <= s.maxCell {
		return v
	}
	s.cut++
	end := s.maxCell
	for end > 0 && !utf8.RuneStart(v[end]) {
		end--
	}
	return v[:end] + fmt.Sprintf("…[已截断，共 %d 字节]", len(v)) // i18n-ignore: 拼进结果单元格的值，属于数据输出
}

// cell 把一个顶层字段值转成单元格值。
func (s *shaper) cell(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return s.cutString(x)
	case bool:
		return x
	case int32:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return int64(x)
	case float64:
		switch {
		case math.IsNaN(x):
			return "NaN"
		case math.IsInf(x, 1):
			return "Infinity"
		case math.IsInf(x, -1):
			return "-Infinity"
		}
		return x
	case bson.ObjectID:
		return `ObjectId("` + x.Hex() + `")`
	case bson.DateTime:
		return fmtDate(x)
	case bson.Decimal128:
		return x.String()
	case bson.Binary:
		return s.binaryString(x)
	case bson.Timestamp:
		return fmt.Sprintf("Timestamp(%d, %d)", x.T, x.I)
	case bson.Regex:
		return s.cutString("/" + x.Pattern + "/" + x.Options)
	case bson.MinKey:
		return "MinKey"
	case bson.MaxKey:
		return "MaxKey"
	case bson.Undefined, bson.Null:
		return nil
	case bson.JavaScript:
		return s.cutString(string(x))
	case bson.Symbol:
		return s.cutString(string(x))
	case bson.DBPointer:
		return fmt.Sprintf("DBPointer(%q, ObjectId(%q))", x.DB, x.Pointer.Hex())
	case bson.CodeWithScope:
		return s.cutString(string(x.Code))
	case bson.D, bson.A, bson.M, map[string]any, []any:
		return s.compactJSON(x)
	}
	return s.cutString(fmt.Sprintf("%v", v))
}

func fmtDate(d bson.DateTime) string {
	return d.Time().UTC().Format("2006-01-02T15:04:05.000Z")
}

func (s *shaper) binaryString(b bson.Binary) string {
	s.binary++
	if b.Subtype == 4 && len(b.Data) == 16 {
		h := fmt.Sprintf("%x", b.Data)
		return `UUID("` + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:] + `")`
	}
	data := b.Data
	suffix := ""
	if len(data) > 64<<10 {
		suffix = fmt.Sprintf("…[已截断，共 %d 字节]", len(data)) // i18n-ignore: 拼进结果单元格的值，属于数据输出
		data = data[:64<<10]
	}
	return fmt.Sprintf(`BinData(%d, \x%x%s)`, b.Subtype, data, suffix)
}

// compactJSON 把嵌套文档 / 数组转成紧凑的 relaxed Extended JSON 文本。
func (s *shaper) compactJSON(v any) string {
	if m, ok := v.(map[string]any); ok { // 稳定键序
		v = sortedD(m)
	} else if m, ok := v.(bson.M); ok {
		v = sortedD(m)
	}
	b, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: v}}, false, false)
	if err != nil {
		return s.cutString(fmt.Sprintf("%v", v))
	}
	txt := string(b)
	txt = strings.TrimPrefix(txt, `{"v":`)
	txt = strings.TrimSuffix(txt, `}`)
	return s.cutString(txt)
}

func sortedD(m map[string]any) bson.D {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	d := make(bson.D, 0, len(keys))
	for _, k := range keys {
		d = append(d, bson.E{Key: k, Value: m[k]})
	}
	return d
}

// docRows 把一批文档整形成二维表：列 = 文档顶层字段的并集（按首次出现的顺序），最多 MaxColumns 列，
// 超出的字段折叠进最后一个 _extra 列（紧凑 JSON）。
func (s *shaper) docRows(docs []bson.D) ([]string, [][]any) {
	var cols []string
	idx := map[string]int{}
	extra := false
	for _, d := range docs {
		for _, e := range d {
			if _, ok := idx[e.Key]; ok {
				continue
			}
			if len(cols) >= MaxColumns {
				extra = true
				continue
			}
			idx[e.Key] = len(cols)
			cols = append(cols, e.Key)
		}
	}
	extraName := "_extra"
	for {
		if _, taken := idx[extraName]; !taken {
			break
		}
		extraName += "_"
	}
	n := len(cols)
	if extra {
		cols = append(cols, extraName)
	}
	rows := make([][]any, 0, len(docs))
	for _, d := range docs {
		row := make([]any, len(cols))
		var rest bson.D
		for _, e := range d {
			if i, ok := idx[e.Key]; ok {
				row[i] = s.cell(e.Value)
			} else {
				rest = append(rest, e)
			}
		}
		if extra && len(rest) > 0 {
			row[n] = s.compactJSON(rest)
		}
		rows = append(rows, row)
	}
	return cols, rows
}
