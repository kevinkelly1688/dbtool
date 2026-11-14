// @author: kevinkelly
// @license: Apache Licence
// @file: sqlgen.go
// @time: 2026/10/28

package binlog

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-mysql-org/go-mysql/replication"
)

// 把行镜像还原成 SQL，或生成回滚 SQL。本文件只有纯函数，不依赖连接，便于单测。

// colMeta 是一列的元信息（来自 binlog 的表映射事件，或目标库的 information_schema.COLUMNS）。
type colMeta struct {
	Name       string
	DataType   string // 小写，如 int、varchar、datetime、json
	ColumnType string // 完整类型，如 int(10) unsigned、enum('a','b')
	Unsigned   bool
	Generated  bool // 生成列：不能出现在 INSERT / SET 里
	Virtual    bool // 虚拟生成列：binlog 里不一定有这一列
	PK         bool
	Enum       []string
	Set        []string
}

// tableMeta 是一张表在某个表映射事件下的列信息，与行镜像的列一一对应。
type tableMeta struct {
	Schema, Table string
	Cols          []colMeta
	PK            []int  // 主键列在 Cols 里的下标
	Problem       string // 非空表示拿不到可靠的列信息（表已删除、列数对不上等），此时列名用 @N 占位，不可回滚
}

func (t *tableMeta) colName(i int) string {
	if i < len(t.Cols) && t.Cols[i].Name != "" {
		return t.Cols[i].Name
	}
	return "@" + strconv.Itoa(i+1)
}

// ident 是第 i 列在 SQL 里的写法；列名未知（Problem）时用 mysqlbinlog 风格的 @N，不加引号。
func (t *tableMeta) ident(i int) string {
	if t.Problem != "" {
		return t.colName(i)
	}
	return quoteIdent(t.colName(i))
}

func quoteIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

var strEscaper = strings.NewReplacer(`\`, `\\`, `'`, `''`, "\x00", `\0`, "\n", `\n`, "\r", `\r`, "\x1a", `\Z`)

func quoteString(s string) string { return "'" + strEscaper.Replace(s) + "'" }

func hexLiteral(b []byte) string {
	if len(b) == 0 {
		return "''"
	}
	return "0x" + hex.EncodeToString(b)
}

func (t *tableMeta) qualified(noPrefix bool) string {
	if noPrefix || t.Schema == "" {
		return quoteIdent(t.Table)
	}
	return quoteIdent(t.Schema) + "." + quoteIdent(t.Table)
}

var plainNumber = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

var (
	blobTypes = map[string]bool{"tinyblob": true, "blob": true, "mediumblob": true, "longblob": true, "binary": true, "varbinary": true}
	textTypes = map[string]bool{"tinytext": true, "text": true, "mediumtext": true, "longtext": true}
)

// errUnsupportedValue 表示这一行里有无法还原成 SQL 的值（如 JSON 部分更新）。
var errUnsupportedValue = errors.New("unsupported value")

// lit 把第 i 列的值转成 SQL 字面量。
func (t *tableMeta) lit(i int, v any) (string, error) {
	var c colMeta
	if i < len(t.Cols) {
		c = t.Cols[i]
	}
	switch x := v.(type) {
	case nil:
		return "NULL", nil
	case int8:
		return intLit(int64(x), c, 8), nil
	case int16:
		return intLit(int64(x), c, 16), nil
	case int32:
		return intLit(int64(x), c, 32), nil
	case int64:
		return intLit(x, c, 64), nil
	case int:
		return intLit(int64(x), c, 64), nil
	case uint8:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case float32:
		return floatLit(float64(x), 32)
	case float64:
		return floatLit(x, 64)
	case string:
		return t.stringLit(c, x), nil
	case []byte:
		return t.bytesLit(c, x), nil
	case *replication.JsonDiff:
		return "", fmt.Errorf("%w：JSON 部分更新（binlog_row_value_options=PARTIAL_JSON）", errUnsupportedValue)
	default:
		return "", fmt.Errorf("%w：第 %d 列的值类型 %T", errUnsupportedValue, i+1, v)
	}
}

// intLit 处理整数：枚举 / 集合按标签还原；无符号列里被按有符号解码的值转回无符号。
func intLit(v int64, c colMeta, bits int) string {
	switch c.DataType {
	case "enum":
		if v == 0 {
			return "''"
		}
		if v > 0 && int(v) <= len(c.Enum) {
			return quoteString(c.Enum[v-1])
		}
		return strconv.FormatInt(v, 10)
	case "set":
		if len(c.Set) > 0 && v >= 0 {
			var parts []string
			for b := 0; b < len(c.Set) && b < 64; b++ {
				if v&(1<<uint(b)) != 0 {
					parts = append(parts, c.Set[b])
				}
			}
			return quoteString(strings.Join(parts, ","))
		}
	}
	if (c.Unsigned || c.DataType == "bit") && v < 0 {
		if c.DataType == "mediumint" {
			return strconv.FormatUint(uint64(uint32(v))&0xFFFFFF, 10)
		}
		switch bits {
		case 8:
			return strconv.FormatUint(uint64(uint8(v)), 10)
		case 16:
			return strconv.FormatUint(uint64(uint16(v)), 10)
		case 32:
			return strconv.FormatUint(uint64(uint32(v)), 10)
		default:
			return strconv.FormatUint(uint64(v), 10)
		}
	}
	if c.Unsigned && c.DataType == "mediumint" && bits == 32 {
		return strconv.FormatUint(uint64(uint32(v))&0xFFFFFF, 10)
	}
	return strconv.FormatInt(v, 10)
}

func floatLit(f float64, bits int) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("%w：浮点数 %v", errUnsupportedValue, f)
	}
	return strconv.FormatFloat(f, 'g', -1, bits), nil
}

func (t *tableMeta) stringLit(c colMeta, s string) string {
	switch {
	case c.DataType == "decimal" && plainNumber.MatchString(s):
		return s
	case blobTypes[c.DataType]:
		if s == "" {
			return "''"
		}
		return hexLiteral([]byte(s))
	case c.DataType == "set" && len(c.Set) == 0, c.DataType == "enum" && len(c.Enum) == 0:
		return quoteString(s)
	case !utf8.ValidString(s):
		return hexLiteral([]byte(s))
	}
	return quoteString(s)
}

func (t *tableMeta) bytesLit(c colMeta, b []byte) string {
	switch {
	case c.DataType == "geometry":
		if len(b) > 4 {
			return fmt.Sprintf("ST_GeomFromWKB(%s,%d)", hexLiteral(b[4:]), binary.LittleEndian.Uint32(b[:4]))
		}
		return hexLiteral(b)
	case c.DataType == "json":
		if len(b) == 0 {
			return "'null'"
		}
		return quoteString(string(b))
	case blobTypes[c.DataType]:
		return hexLiteral(b)
	case utf8.Valid(b):
		return quoteString(string(b))
	}
	return hexLiteral(b)
}

func isSkipped(skipped []int, i int) bool {
	for _, s := range skipped {
		if s == i {
			return true
		}
	}
	return false
}

// valueLits 渲染一行里除跳过列外的所有列。
func (t *tableMeta) valueLits(row []any, skipped []int) ([]string, error) {
	out := make([]string, len(row))
	for i, v := range row {
		if isSkipped(skipped, i) {
			continue
		}
		s, err := t.lit(i, v)
		if err != nil {
			return nil, fmt.Errorf("列 %s：%w", t.colName(i), err)
		}
		out[i] = s
	}
	return out, nil
}

// insertSQL 由一行的完整（或部分）镜像生成 INSERT。
func (t *tableMeta) insertSQL(opt genOpt, row []any, skipped []int) (string, error) {
	lits, err := t.valueLits(row, skipped)
	if err != nil {
		return "", err
	}
	var cols, vals []string
	for i := range row {
		if isSkipped(skipped, i) || t.generated(i) {
			continue
		}
		cols = append(cols, t.ident(i))
		vals = append(vals, lits[i])
	}
	if len(cols) == 0 {
		return "", fmt.Errorf("%w：没有可写入的列", errUnsupportedValue)
	}
	if t.Problem != "" {
		return "INSERT INTO " + t.qualified(opt.NoDBPrefix) + " VALUES (" + strings.Join(vals, ",") + ");", nil
	}
	return "INSERT INTO " + t.qualified(opt.NoDBPrefix) + " (" + strings.Join(cols, ",") + ") VALUES (" + strings.Join(vals, ",") + ");", nil
}

func (t *tableMeta) generated(i int) bool { return i < len(t.Cols) && t.Cols[i].Generated }

// where 生成 WHERE 条件（不含 WHERE 关键字）。优先用主键；没有主键或主键列不在镜像里时用全部列，此时 byPK 为 false。
func (t *tableMeta) where(opt genOpt, row []any, skipped []int) (cond string, byPK bool, err error) {
	lits, err := t.valueLits(row, skipped)
	if err != nil {
		return "", false, err
	}
	usePK := len(t.PK) > 0 && !opt.FullColumns && t.Problem == ""
	if usePK {
		for _, i := range t.PK {
			if isSkipped(skipped, i) {
				usePK = false
			}
		}
	}
	idx := t.PK
	if !usePK {
		idx = nil
		var float []int
		for i := range row {
			if isSkipped(skipped, i) || t.generated(i) || (i < len(t.Cols) && t.Cols[i].DataType == "geometry") {
				continue
			}
			// 浮点数的十进制文本与存储值常对不上，等值比较不可靠：有别的列可用时不参与条件
			if i < len(t.Cols) && (t.Cols[i].DataType == "float" || t.Cols[i].DataType == "double") {
				float = append(float, i)
				continue
			}
			idx = append(idx, i)
		}
		if len(idx) == 0 {
			idx = float
		}
	}
	if len(idx) == 0 {
		return "", false, fmt.Errorf("%w：没有可用于定位行的列", errUnsupportedValue)
	}
	parts := make([]string, 0, len(idx))
	for _, i := range idx {
		name := t.ident(i)
		switch {
		case lits[i] == "NULL":
			parts = append(parts, name+" IS NULL")
		case i < len(t.Cols) && t.Cols[i].DataType == "json":
			parts = append(parts, name+" = CAST("+lits[i]+" AS JSON)")
		default:
			parts = append(parts, name+" = "+lits[i])
		}
	}
	return strings.Join(parts, " AND "), usePK, nil
}

// deleteSQL 生成 DELETE，按给定镜像定位行。
func (t *tableMeta) deleteSQL(opt genOpt, row []any, skipped []int) (string, error) {
	cond, byPK, err := t.where(opt, row, skipped)
	if err != nil {
		return "", err
	}
	q := "DELETE FROM " + t.qualified(opt.NoDBPrefix) + " WHERE " + cond
	if !byPK {
		q += " LIMIT 1"
	}
	return q + ";", nil
}

// updateSQL 生成 UPDATE：把表里“当前是 cur 镜像”的行改成 target 镜像的值。
// 正向还原时 cur=更新前、target=更新后；回滚时反过来。SET 只带发生变化的列（FullColumns 时带全部列）。
func (t *tableMeta) updateSQL(opt genOpt, target []any, targetSkipped []int, cur []any, curSkipped []int) (string, error) {
	tl, err := t.valueLits(target, targetSkipped)
	if err != nil {
		return "", err
	}
	cl, err := t.valueLits(cur, curSkipped)
	if err != nil {
		return "", err
	}
	var sets []string
	first := -1
	for i := range target {
		if isSkipped(targetSkipped, i) || t.generated(i) {
			continue
		}
		if first < 0 {
			first = i
		}
		if !opt.FullColumns && i < len(cur) && !isSkipped(curSkipped, i) && tl[i] == cl[i] {
			continue
		}
		sets = append(sets, t.ident(i)+" = "+tl[i])
	}
	if len(sets) == 0 {
		if first < 0 {
			return "", fmt.Errorf("%w：没有可更新的列", errUnsupportedValue)
		}
		sets = append(sets, t.ident(first)+" = "+tl[first]) // 前后镜像完全一致：写成一个无变化的赋值，保持语句有效
	}
	cond, byPK, err := t.where(opt, cur, curSkipped)
	if err != nil {
		return "", err
	}
	q := "UPDATE " + t.qualified(opt.NoDBPrefix) + " SET " + strings.Join(sets, ", ") + " WHERE " + cond
	if !byPK {
		q += " LIMIT 1"
	}
	return q + ";", nil
}

// genOpt 是生成 SQL 的选项。
type genOpt struct {
	NoDBPrefix  bool
	FullColumns bool
}

// imageComplete 表示这一行镜像包含全部列（回滚的前提）。
func imageComplete(skipped []int) bool { return len(skipped) == 0 }
