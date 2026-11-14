// @author: kevinkelly
// @license: Apache Licence
// @file: values.go
// @time: 2026/10/28

package engine

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type colKind int

const (
	kindText colKind = iota
	kindNumber
	kindBinary
)

// kindOf 按列类型（information_schema 的 COLUMN_TYPE，如 "bigint(20) unsigned"）归类。
func kindOf(colType string) colKind {
	t := strings.ToLower(strings.TrimSpace(colType))
	if i := strings.IndexAny(t, "( "); i >= 0 {
		t = t[:i]
	}
	switch t {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "decimal", "numeric", "float", "double", "real", "year":
		return kindNumber
	case "binary", "varbinary", "blob", "tinyblob", "mediumblob", "longblob", "bit",
		"geometry", "point", "linestring", "polygon", "multipoint", "multilinestring", "multipolygon", "geometrycollection":
		return kindBinary
	}
	return kindText
}

func isUnsignedType(colType string) bool {
	return strings.Contains(strings.ToLower(colType), "unsigned")
}

// valueText 把驱动返回的值转成文本。null=true 表示 SQL NULL。二进制列返回 0x 十六进制。
func valueText(v any, kind colKind) (s string, null bool) {
	switch x := v.(type) {
	case nil:
		return "", true
	case []byte:
		if kind == kindBinary {
			return "0x" + hex.EncodeToString(x), false
		}
		return string(x), false
	case string:
		if kind == kindBinary {
			return "0x" + hex.EncodeToString([]byte(x)), false
		}
		return x, false
	case int64:
		return strconv.FormatInt(x, 10), false
	case int:
		return strconv.Itoa(x), false
	case uint64:
		return strconv.FormatUint(x, 10), false
	case int32:
		return strconv.FormatInt(int64(x), 10), false
	case uint32:
		return strconv.FormatUint(uint64(x), 10), false
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32), false
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), false
	case bool:
		if x {
			return "1", false
		}
		return "0", false
	case time.Time:
		return x.Format("2006-01-02 15:04:05.999999"), false
	}
	return fmt.Sprint(v), false
}

// valKey 是比较两个值是否“相同”用的规范形式（备份端已有行与源行对比）。数值类型统一成十进制文本。
func valKey(v any) string {
	switch x := v.(type) {
	case nil:
		return "\x00NULL"
	case []byte:
		return "s:" + string(x)
	case string:
		return "s:" + x
	case int64, int, uint64, int32, uint32:
		s, _ := valueText(x, kindText)
		return "s:" + s
	case float32:
		return "s:" + strconv.FormatFloat(float64(x), 'g', -1, 32)
	case float64:
		return "s:" + strconv.FormatFloat(x, 'g', -1, 64)
	case time.Time:
		return "s:" + x.Format("2006-01-02 15:04:05.999999")
	}
	return "s:" + fmt.Sprint(v)
}

// rowKey 是整行的比较键。
func rowKey(r Row) string {
	var b strings.Builder
	for _, v := range r {
		k := valKey(v)
		b.WriteString(strconv.Itoa(len(k)))
		b.WriteByte(':')
		b.WriteString(k)
	}
	return b.String()
}

// pkKeyOf 把主键值（字符串形式）拼成一个 map 键。
func pkKeyOf(pk []string) string {
	var b strings.Builder
	for _, v := range pk {
		b.WriteString(strconv.Itoa(len(v)))
		b.WriteByte(':')
		b.WriteString(v)
	}
	return b.String()
}
