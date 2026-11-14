// @author: kevinkelly
// @license: Apache Licence
// @file: shape.go
// @time: 2026/10/28

package clickhouse

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "time/tzdata" // 带时区的 DateTime 列要换算偏移量；不依赖运行环境里的 zoneinfo

	"dbtool/internal/engine"
)

// Shaped 是整形后的查询结果：值都是 JSON 安全的（见 shapeCell）。
type Shaped struct {
	ColumnList  []string `json:"column_list"`
	ColumnTypes []string `json:"column_types"`
	Rows        [][]any  `json:"rows"`
	Truncated   bool     `json:"truncated"`
	// TruncatedBy 说明为什么被截断：rows（超过行数上限）、bytes（超过字节上限）、response（服务端响应超过读取上限）。
	TruncatedBy string   `json:"truncated_by,omitempty"`
	ElapsedMs   int64    `json:"elapsed_ms"`
	RowsRead    int64    `json:"rows_read"`  // 服务端扫描的行数（statistics.rows_read；取不到为 0）
	BytesRead   int64    `json:"bytes_read"` // 服务端扫描的字节数
	Warnings    []string `json:"warnings,omitempty"`
	Notices     []string `json:"notices,omitempty"`
}

// ToResultSet 转成 engine.ResultSet。
func (s *Shaped) ToResultSet() *engine.ResultSet {
	return &engine.ResultSet{Rows: s.Rows, ColumnList: s.ColumnList, CostTimeMs: s.ElapsedMs}
}

type colMeta struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type stats struct {
	Elapsed   float64 `json:"elapsed"`
	RowsRead  int64   `json:"rows_read"`
	BytesRead int64   `json:"bytes_read"`
}

// parsed 是解析 JSONCompact 后、尚未按列类型整形的结果。
type parsed struct {
	cols      []colMeta
	rows      [][]json.RawMessage
	truncated bool
	by        string
	stats     stats
}

// parseJSONCompact 流式解析 {"meta":[..],"data":[[..],..],"rows":N,"statistics":{..}}。
// 最多收 maxRows 行、约 maxBytes 字节；再多一行即标记截断并立即停止读取。
// 响应被 capReader 截断时保留已解析的整行，并标记 response 截断。
func parseJSONCompact(r *capReader, maxRows int, maxBytes int64) (*parsed, error) {
	p := &parsed{}
	dec := json.NewDecoder(r)
	fail := func(err error) (*parsed, error) {
		if p.cols != nil && r.hit {
			p.truncated, p.by = true, "response"
			return p, nil
		}
		if m := reExcInBody.FindSubmatch(r.tail); m != nil {
			c := &Client{}
			return nil, c.serverError(200, nil, r.tail[bytes.Index(r.tail, m[0]):])
		}
		if r.hit {
			return nil, errors.New("ClickHouse 响应超过读取上限，已丢弃；请缩小结果范围")
		}
		return nil, fmt.Errorf("无法解析 ClickHouse 响应：%v", err)
	}
	if t, err := dec.Token(); err != nil {
		return fail(err)
	} else if d, ok := t.(json.Delim); !ok || d != '{' {
		return fail(errors.New("响应不是 JSON 对象"))
	}
	var total int64
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return fail(err)
		}
		key, _ := kt.(string)
		switch key {
		case "meta":
			if err := dec.Decode(&p.cols); err != nil {
				return fail(err)
			}
			if p.cols == nil {
				p.cols = []colMeta{}
			}
		case "data":
			if t, err := dec.Token(); err != nil {
				return fail(err)
			} else if d, ok := t.(json.Delim); !ok || d != '[' {
				return fail(errors.New("data 不是数组"))
			}
			for dec.More() {
				var row []json.RawMessage
				if err := dec.Decode(&row); err != nil {
					return fail(err)
				}
				if len(p.rows) >= maxRows {
					p.truncated, p.by = true, "rows"
					return p, nil
				}
				var sz int64
				for _, c := range row {
					sz += int64(len(c))
				}
				if len(p.rows) > 0 && total+sz > maxBytes {
					p.truncated, p.by = true, "bytes"
					return p, nil
				}
				total += sz
				p.rows = append(p.rows, row)
			}
			if _, err := dec.Token(); err != nil {
				return fail(err)
			}
		case "statistics":
			if err := dec.Decode(&p.stats); err != nil {
				return fail(err)
			}
		case "exception":
			var msg string
			_ = dec.Decode(&msg)
			c := &Client{}
			return nil, c.serverError(200, nil, []byte(msg))
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return fail(err)
			}
		}
	}
	if p.cols == nil {
		return fail(errors.New("响应里没有 meta"))
	}
	return p, nil
}

// ---- 列类型 ----

type colType struct {
	raw      string
	base     string // 去掉 Nullable / LowCardinality 与参数后的类型名，如 DateTime64、Decimal、Array
	nullable bool
	tz       string // DateTime('tz') / DateTime64(3, 'tz')
}

func parseColType(s string) colType {
	t := strings.TrimSpace(s)
	ct := colType{raw: s}
	for {
		switch {
		case strings.HasPrefix(t, "LowCardinality(") && strings.HasSuffix(t, ")"):
			t = t[len("LowCardinality(") : len(t)-1]
			continue
		case strings.HasPrefix(t, "Nullable(") && strings.HasSuffix(t, ")"):
			ct.nullable = true
			t = t[len("Nullable(") : len(t)-1]
			continue
		}
		break
	}
	args := ""
	ct.base = t
	if i := strings.IndexByte(t, '('); i > 0 && strings.HasSuffix(t, ")") {
		ct.base, args = t[:i], t[i+1:len(t)-1]
	}
	if ct.base == "DateTime" || ct.base == "DateTime64" {
		if a := strings.IndexByte(args, '\''); a >= 0 {
			if b := strings.IndexByte(args[a+1:], '\''); b >= 0 {
				ct.tz = args[a+1 : a+1+b]
			}
		}
	}
	return ct
}

// isNumericSmall：JSON 数字可以直接当 int64 表示的整数类型（64 位及以上的整数 ClickHouse 以字符串输出，保精度）。
func (c colType) isSmallInt() bool {
	switch c.base {
	case "Int8", "Int16", "Int32", "UInt8", "UInt16", "UInt32":
		return true
	}
	return false
}

func (c colType) isFloat() bool {
	return c.base == "Float32" || c.base == "Float64" || c.base == "BFloat16"
}

// ---- 单元格整形 ----

// shaper 在整形过程中累计警告。
type shaper struct {
	cellMax  int
	binCols  map[int]bool
	cutCols  map[int]bool
	badUTF8  map[int]bool
	tzCache  map[string]*time.Location
	tzFailed map[string]bool
}

func newShaper(cellMax int) *shaper {
	return &shaper{cellMax: cellMax, binCols: map[int]bool{}, cutCols: map[int]bool{}, badUTF8: map[int]bool{}, tzCache: map[string]*time.Location{}, tzFailed: map[string]bool{}}
}

// shapeCell 把一个 JSON 单元格转成 JSON 安全的值：
//
//	NULL→nil；Int8..Int32/UInt8..UInt32→int64；Float→float64；Bool→bool；
//	Int64/UInt64/Int128/Int256/Decimal 等大整数与定点数→字符串（保精度）；
//	Date/Date32→"YYYY-MM-DD"；DateTime/DateTime64→ISO-8601（列声明了时区的带偏移量）；
//	Array/Tuple/Map/JSON/Nested→紧凑 JSON 字符串；
//	含控制字符的二进制内容→\xHH…（并给出警告）；单元格超过上限→截断并带说明。
func (s *shaper) shapeCell(col int, raw json.RawMessage, ct colType) any {
	if len(raw) == 0 {
		return nil
	}
	switch raw[0] {
	case 'n':
		return nil
	case 't':
		return true
	case 'f':
		return false
	case '"':
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return s.limitString(col, string(raw))
		}
		switch ct.base {
		case "DateTime", "DateTime64":
			return s.isoTime(str, ct)
		}
		return s.cleanString(col, str)
	case '[', '{':
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			return s.limitString(col, string(raw))
		}
		return s.limitString(col, buf.String())
	}
	// 数字
	txt := string(raw)
	switch {
	case ct.isSmallInt():
		if n, err := strconv.ParseInt(txt, 10, 64); err == nil {
			return n
		}
	case ct.isFloat():
		if f, err := strconv.ParseFloat(txt, 64); err == nil {
			return f
		}
	}
	return txt
}

// isoTime 把 "2024-01-02 03:04:05[.fff]" 转成 ISO-8601。列声明了时区且能解析时附带偏移量，否则不带（服务端时区未知）。
func (s *shaper) isoTime(v string, ct colType) any {
	if len(v) < 19 || v[10] != ' ' {
		return v
	}
	iso := v[:10] + "T" + v[11:]
	if ct.tz == "" {
		return iso
	}
	loc := s.tzCache[ct.tz]
	if loc == nil && !s.tzFailed[ct.tz] {
		l, err := time.LoadLocation(ct.tz)
		if err != nil {
			s.tzFailed[ct.tz] = true
		} else {
			loc = l
			s.tzCache[ct.tz] = l
		}
	}
	if loc == nil {
		return iso
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", iso, loc)
	if err != nil {
		return iso
	}
	return t.Format("2006-01-02T15:04:05.999999999Z07:00")
}

// cleanString 处理字符串：控制字符（含 NUL）视为二进制，转 \xHH；超长截断。
func (s *shaper) cleanString(col int, v string) string {
	if hasBinary(v) {
		s.binCols[col] = true
		return s.limitHex(col, v)
	}
	if strings.ContainsRune(v, utf8.RuneError) {
		s.badUTF8[col] = true
	}
	return s.limitString(col, v)
}

func hasBinary(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' || c == 0x7f {
			return true
		}
	}
	return false
}

func (s *shaper) limitString(col int, v string) string {
	if len(v) <= s.cellMax {
		return v
	}
	s.cutCols[col] = true
	cut := s.cellMax
	for cut > 0 && !utf8.RuneStart(v[cut]) {
		cut--
	}
	return v[:cut] + fmt.Sprintf("…（已截断，原长 %d 字节）", len(v)) // i18n-ignore: 拼进结果单元格的值，属于数据输出
}

func (s *shaper) limitHex(col int, v string) string {
	b := []byte(v)
	cut := false
	if len(b) > s.cellMax {
		b, cut = b[:s.cellMax], true
		s.cutCols[col] = true
	}
	const hexd = "0123456789abcdef"
	var sb strings.Builder
	sb.Grow(len(b) * 4)
	for _, c := range b {
		sb.WriteString(`\x`)
		sb.WriteByte(hexd[c>>4])
		sb.WriteByte(hexd[c&15])
	}
	if cut {
		sb.WriteString(fmt.Sprintf("…（已截断，原长 %d 字节）", len(v))) // i18n-ignore: 拼进结果单元格的值，属于数据输出
	}
	return sb.String()
}

// shape 把解析结果整形成 Shaped。
func shape(p *parsed, cellMax int) *Shaped {
	sh := newShaper(cellMax)
	out := &Shaped{ColumnList: make([]string, len(p.cols)), ColumnTypes: make([]string, len(p.cols)), Rows: make([][]any, 0, len(p.rows)),
		Truncated: p.truncated, TruncatedBy: p.by, RowsRead: p.stats.RowsRead, BytesRead: p.stats.BytesRead}
	types := make([]colType, len(p.cols))
	for i, c := range p.cols {
		out.ColumnList[i], out.ColumnTypes[i] = c.Name, c.Type
		types[i] = parseColType(c.Type)
	}
	for _, row := range p.rows {
		vals := make([]any, len(p.cols))
		for i := range vals {
			if i < len(row) {
				vals[i] = sh.shapeCell(i, row[i], types[i])
			}
		}
		out.Rows = append(out.Rows, vals)
	}
	name := func(i int) string { return fmt.Sprintf("第 %d 列「%s」", i+1, p.cols[i].Name) }
	for i := range p.cols {
		if sh.binCols[i] {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s含二进制内容，已以 \\xHH 形式显示", name(i)))
		}
		if sh.badUTF8[i] {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s含无法按 UTF-8 解码的字节，已被替换为 U+FFFD", name(i)))
		}
		if sh.cutCols[i] {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s有单元格超过 %d KB，已截断", name(i), sh.cellMax>>10))
		}
	}
	return out
}
