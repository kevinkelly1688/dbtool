// @author: kevinkelly
// @license: Apache Licence
// @file: value.go
// @time: 2026/10/28

package postgres

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 结果值整形：把文本格式的结果转成 JSON 安全的 Go 值。
//
//	NULL → nil；bool → bool；int2/int4/oid/xid/cid → int64（number）；int8 超出 ±2^53-1 → 字符串（保精度）
//	float → float64（NaN / ±Infinity → 字符串）；numeric / money → 字符串
//	date / time / timestamp → ISO-8601 文本；timestamptz / timetz → ISO-8601 并保留时区偏移（+08 → +08:00）；infinity → 字符串
//	json / jsonb → 紧凑 JSON 字符串；数组 → 紧凑 JSON 数组字符串（元素按元素类型整形）
//	bytea → \xHH… 十六进制串（超过 64KB 截断并注明）；其它类型（uuid、inet、interval、range、枚举、复合、扩展类型 …）→ 文本原样

// MaxCellBytes 是单个单元格的显示上限（字节），超出截断并注明总长。bytea 以解码后的字节数计。
const MaxCellBytes = 64 << 10

// PostgreSQL 内置类型 OID。
const (
	oidBool        = 16
	oidBytea       = 17
	oidInt8        = 20
	oidInt2        = 21
	oidInt4        = 23
	oidOid         = 26
	oidXid         = 28
	oidCid         = 29
	oidJSON        = 114
	oidFloat4      = 700
	oidFloat8      = 701
	oidMoney       = 790
	oidDate        = 1082
	oidTime        = 1083
	oidTimestamp   = 1114
	oidTimestamptz = 1184
	oidTimetz      = 1266
	oidNumeric     = 1700
	oidJSONB       = 3802
)

var typeNames = map[uint32]string{
	16: "bool", 17: "bytea", 18: "char", 19: "name", 20: "int8", 21: "int2", 22: "int2vector", 23: "int4", 24: "regproc", 25: "text", 26: "oid",
	27: "tid", 28: "xid", 29: "cid", 30: "oidvector", 114: "json", 142: "xml", 600: "point", 601: "lseg", 602: "path", 603: "box", 604: "polygon",
	628: "line", 650: "cidr", 700: "float4", 701: "float8", 705: "unknown", 718: "circle", 774: "macaddr8", 790: "money", 829: "macaddr", 869: "inet",
	1042: "bpchar", 1043: "varchar", 1082: "date", 1083: "time", 1114: "timestamp", 1184: "timestamptz", 1186: "interval", 1266: "timetz",
	1560: "bit", 1562: "varbit", 1700: "numeric", 1790: "refcursor", 2202: "regprocedure", 2203: "regoper", 2204: "regoperator", 2205: "regclass",
	2206: "regtype", 2249: "record", 2950: "uuid", 2970: "txid_snapshot", 3220: "pg_lsn", 3614: "tsvector", 3615: "tsquery", 3734: "regconfig",
	3769: "regdictionary", 3802: "jsonb", 3904: "int4range", 3906: "numrange", 3908: "tsrange", 3910: "tstzrange", 3912: "daterange", 3926: "int8range",
	4072: "jsonpath", 4451: "int4multirange", 4532: "nummultirange", 4533: "tsmultirange", 4534: "tstzmultirange", 4535: "datemultirange",
	4536: "int8multirange", 5069: "xid8",
}

// 数组类型 OID → 元素类型 OID。
var arrayElem = map[uint32]uint32{
	199: 114, 143: 142, 1000: 16, 1001: 17, 1002: 18, 1003: 19, 1005: 21, 1006: 22, 1007: 23, 1008: 24, 1009: 25, 1010: 27, 1011: 28, 1012: 29, 1013: 30,
	1014: 1042, 1015: 1043, 1016: 20, 1017: 600, 1018: 601, 1019: 602, 1020: 603, 1021: 700, 1022: 701, 1027: 604, 1028: 26, 1040: 829, 1041: 869,
	651: 650, 719: 718, 775: 774, 791: 790, 1115: 1114, 1182: 1082, 1183: 1083, 1185: 1184, 1187: 1186, 1231: 1700, 1270: 1266, 1561: 1560,
	1563: 1562, 2951: 2950, 3807: 3802, 3643: 3614, 3645: 3615, 3905: 3904, 3907: 3906, 3909: 3908, 3911: 3910, 3913: 3912, 3927: 3926,
	2201: 1790, 2207: 2202, 2208: 2203, 2209: 2204, 2210: 2205, 2211: 2206, 3221: 3220, 4073: 4072, 271: 5069,
}

// typeName 返回内置类型名；未知类型返回 "" （由调用方查 pg_type 补）。
func typeName(oid uint32) string {
	if n, ok := typeNames[oid]; ok {
		return n
	}
	if e, ok := arrayElem[oid]; ok {
		if n, ok := typeNames[e]; ok {
			return n + "[]"
		}
	}
	return ""
}

const maxSafeInt = 1<<53 - 1

var (
	reTimestamp   = regexp.MustCompile(`^(\d{4,}-\d\d-\d\d) (\d\d:\d\d:\d\d(?:\.\d+)?)$`)
	reTimestamptz = regexp.MustCompile(`^(\d{4,}-\d\d-\d\d) (\d\d:\d\d:\d\d(?:\.\d+)?)([+-]\d\d(?::\d\d(?::\d\d)?)?)$`)
	reTimetz      = regexp.MustCompile(`^(\d\d:\d\d:\d\d(?:\.\d+)?)([+-]\d\d(?::\d\d(?::\d\d)?)?)$`)
)

// isoOffset 把 PostgreSQL 输出的偏移（+08、+05:30、+05:30:15）补成至少 ±HH:MM。
func isoOffset(off string) string {
	if len(off) == 3 {
		return off + ":00"
	}
	return off
}

// shapeStats 记录整形过程中的特殊情况，用于给用户提示。
type shapeStats struct {
	Binary int // bytea 单元格个数
	Cut    int // 因过长被截断的单元格个数
}

// toValidUTF8 保证文本可以安全放进 JSON。
func toValidUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// cutText 把过长文本截断到 MaxCellBytes（落在 UTF-8 字符边界上）。
func (st *shapeStats) cutText(s string) string {
	s = toValidUTF8(s)
	if len(s) <= MaxCellBytes {
		return s
	}
	st.Cut++
	cut := MaxCellBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("…（已截断，共 %d 字节）", len(s)) // i18n-ignore: 拼进结果单元格的值，属于数据输出
}

// convert 把一个文本格式的列值转成 JSON 安全的值。
func (st *shapeStats) convert(oid uint32, raw []byte) any {
	if raw == nil {
		return nil
	}
	s := string(raw)
	if oid == oidBytea {
		return st.bytea(s)
	}
	if elem, ok := arrayElem[oid]; ok && elem != oidBytea {
		if v, err := parseArrayJSON(s, func(e string) any { return scalar(elem, e) }); err == nil {
			return st.cutText(v)
		}
		return st.cutText(s)
	}
	if elem, ok := arrayElem[oid]; ok && elem == oidBytea {
		if v, err := parseArrayJSON(s, func(e string) any { return e }); err == nil {
			return st.cutText(v)
		}
		return st.cutText(s)
	}
	switch v := scalar(oid, s).(type) {
	case string:
		return st.cutText(v)
	default:
		return v
	}
}

// bytea：默认输出 \xHH… ；超过 64KB 的字节数截断。
func (st *shapeStats) bytea(s string) any {
	st.Binary++
	if !strings.HasPrefix(s, `\x`) {
		return st.cutText(s) // bytea_output=escape，原样（连接时已固定为 hex，这里只是兜底）
	}
	hexLen := len(s) - 2
	if hexLen/2 <= MaxCellBytes {
		return s
	}
	st.Cut++
	return s[:2+MaxCellBytes*2] + fmt.Sprintf("…（已截断，共 %d 字节）", hexLen/2) // i18n-ignore: 拼进结果单元格的值，属于数据输出
}

// scalar 转换单个值（不含数组 / bytea 的截断逻辑，数组元素也走这里）。
func scalar(oid uint32, s string) any {
	switch oid {
	case oidBool:
		switch s {
		case "t":
			return true
		case "f":
			return false
		}
		return s
	case oidInt2, oidInt4, oidOid, oidXid, oidCid:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
		return s
	case oidInt8:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n <= maxSafeInt && n >= -maxSafeInt {
			return n
		}
		return s
	case oidFloat4, oidFloat8:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return s // NaN / Infinity / -Infinity：JSON 没有对应表示
		}
		return f
	case oidNumeric, oidMoney:
		return s
	case oidTimestamp:
		if m := reTimestamp.FindStringSubmatch(s); m != nil {
			return m[1] + "T" + m[2]
		}
		return s
	case oidTimestamptz:
		if m := reTimestamptz.FindStringSubmatch(s); m != nil {
			return m[1] + "T" + m[2] + isoOffset(m[3])
		}
		return s
	case oidTimetz:
		if m := reTimetz.FindStringSubmatch(s); m != nil {
			return m[1] + isoOffset(m[2])
		}
		return s
	case oidJSON, oidJSONB:
		var buf bytes.Buffer
		if err := json.Compact(&buf, []byte(s)); err == nil {
			return buf.String()
		}
		return s
	}
	return s
}

// ---- 数组 ----

// parseArrayJSON 解析 PostgreSQL 数组文本（如 {1,2,NULL}、{{"a b","c"}}、[0:1]={x,y}），元素用 elem 转换，返回紧凑 JSON 文本。
func parseArrayJSON(s string, elem func(string) any) (string, error) {
	if strings.HasPrefix(s, "[") { // 带下标范围的前缀：[1:3]={…}
		if i := strings.Index(s, "="); i > 0 {
			s = s[i+1:]
		}
	}
	p := &arrParser{s: s, elem: elem}
	v, err := p.parse()
	if err != nil {
		return "", err
	}
	p.skipSpace()
	if p.i != len(p.s) {
		return "", fmt.Errorf("数组文本末尾有多余内容")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

type arrParser struct {
	s     string
	i     int
	elem  func(string) any
	depth int
}

func (p *arrParser) skipSpace() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n' || p.s[p.i] == '\r') {
		p.i++
	}
}

func (p *arrParser) parse() (any, error) {
	p.skipSpace()
	if p.i >= len(p.s) || p.s[p.i] != '{' {
		return nil, fmt.Errorf("不是数组文本")
	}
	p.depth++
	if p.depth > 32 {
		return nil, fmt.Errorf("数组嵌套过深")
	}
	defer func() { p.depth-- }()
	p.i++
	out := []any{}
	p.skipSpace()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return out, nil
	}
	for {
		p.skipSpace()
		if p.i >= len(p.s) {
			return nil, fmt.Errorf("数组文本没有结束")
		}
		switch p.s[p.i] {
		case '{':
			sub, err := p.parse()
			if err != nil {
				return nil, err
			}
			out = append(out, sub)
		case '"':
			p.i++
			var sb strings.Builder
			for {
				if p.i >= len(p.s) {
					return nil, fmt.Errorf("数组元素引号没有结束")
				}
				c := p.s[p.i]
				if c == '\\' && p.i+1 < len(p.s) {
					sb.WriteByte(p.s[p.i+1])
					p.i += 2
					continue
				}
				if c == '"' {
					p.i++
					break
				}
				sb.WriteByte(c)
				p.i++
			}
			out = append(out, p.elem(sb.String()))
		default:
			start := p.i
			for p.i < len(p.s) && p.s[p.i] != ',' && p.s[p.i] != '}' {
				if p.s[p.i] == '\\' && p.i+1 < len(p.s) {
					p.i++
				}
				p.i++
			}
			raw := strings.TrimSpace(p.s[start:p.i])
			if strings.EqualFold(raw, "NULL") {
				out = append(out, nil)
			} else {
				out = append(out, p.elem(unescapeArrayElem(raw)))
			}
		}
		p.skipSpace()
		if p.i >= len(p.s) {
			return nil, fmt.Errorf("数组文本没有结束")
		}
		if p.s[p.i] == ',' {
			p.i++
			continue
		}
		if p.s[p.i] == '}' {
			p.i++
			return out, nil
		}
		return nil, fmt.Errorf("数组文本格式不对")
	}
}

func unescapeArrayElem(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}
