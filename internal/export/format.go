// @author: kevinkelly
// @license: Apache Licence
// @file: format.go
// @time: 2026/10/28

// Package export 是“数据导出工单”的文件生成：把查询结果写成 CSV / JSON / XML / XLSX / SQL 文件，批量导出时按实例命名并打成 zip。
package export

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// 支持的导出格式。
const (
	CSV  = "csv"
	JSON = "json"
	XML  = "xml"
	XLSX = "xlsx"
	SQL  = "sql"
)

// Formats 是支持的格式（展示顺序）。
var Formats = []string{CSV, XLSX, JSON, XML, SQL}

// FormatLabel 格式的显示名。
func FormatLabel(f string) string {
	switch f {
	case CSV:
		return "CSV"
	case XLSX:
		return "Excel"
	case JSON:
		return "JSON"
	case XML:
		return "XML"
	case SQL:
		return "SQL（INSERT 语句）"
	}
	return f
}

// ValidFormat 是否支持的格式。
func ValidFormat(f string) bool {
	for _, x := range Formats {
		if x == f {
			return true
		}
	}
	return false
}

// Result 是一份要导出的查询结果。
type Result struct {
	Columns []string
	Rows    [][]any
	// Dialect 影响 SQL 格式的转义：mysql 额外转义反斜杠。
	Dialect string
	// Masked 是导出前已按数据脱敏规则处理过的列名（只作说明，写文件时不用）。
	Masked []string
	// Raw 表示提交人（超级用户）选择了“导出原值”：用户配置的脱敏没有执行（SQL 防火墙强制脱敏的列仍在 Masked 里）。只作说明，写文件时不用。
	Raw bool
}

// text 把一个单元格的值转成文本（时间按 2006-01-02 15:04:05，字节当字符串）。
func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case time.Time:
		return x.Format("2006-01-02 15:04:05")
	case bool:
		if x {
			return "1"
		}
		return "0"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	}
	return fmt.Sprint(v)
}

// Write 按格式把结果写到 w。
func Write(w io.Writer, format string, r Result) error {
	switch format {
	case CSV:
		return writeCSV(w, r)
	case JSON:
		return writeJSON(w, r)
	case XML:
		return writeXML(w, r)
	case XLSX:
		return writeXLSX(w, r)
	case SQL:
		return writeSQL(w, r)
	}
	return fmt.Errorf("不支持的导出格式：%s", format)
}

// CSV：所有字段加引号，第一行是列名，NULL 写成 null。
func writeCSV(w io.Writer, r Result) error {
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	var b strings.Builder
	line := func(cells []string) {
		for i, c := range cells {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(quote(c))
		}
		b.WriteString("\r\n")
	}
	if len(r.Columns) > 0 {
		line(r.Columns)
	}
	cells := make([]string, 0, len(r.Columns))
	for _, row := range r.Rows {
		cells = cells[:0]
		for _, v := range row {
			if v == nil {
				cells = append(cells, "null")
			} else {
				cells = append(cells, text(v))
			}
		}
		line(cells)
		if b.Len() > 1<<20 {
			if _, err := io.WriteString(w, b.String()); err != nil {
				return err
			}
			b.Reset()
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// JSON：对象数组，键顺序同列顺序，缩进 2 空格，不转义中文。
func writeJSON(w io.Writer, r Result) error {
	var b bytes.Buffer
	enc := func(v any) []byte {
		var o bytes.Buffer
		e := json.NewEncoder(&o)
		e.SetEscapeHTML(false)
		_ = e.Encode(jsonValue(v))
		return bytes.TrimRight(o.Bytes(), "\n")
	}
	if len(r.Rows) == 0 {
		_, err := io.WriteString(w, "[]")
		return err
	}
	b.WriteString("[\n")
	for i, row := range r.Rows {
		b.WriteString("  {\n")
		for j, col := range r.Columns {
			var v any
			if j < len(row) {
				v = row[j]
			}
			fmt.Fprintf(&b, "    %s: %s", enc(col), enc(v))
			if j < len(r.Columns)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString("  }")
		if i < len(r.Rows)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("]")
	_, err := w.Write(b.Bytes())
	return err
}

func jsonValue(v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case time.Time:
		return x.Format("2006-01-02 15:04:05")
	}
	return v
}

// XML：<tabledata><fields>…</fields><data><row id="1"><column-1>…</column-1></row></data></tabledata>，NULL 写成 (null)。
func writeXML(w io.Writer, r Result) error {
	var b strings.Builder
	b.WriteString("<?xml version='1.0' encoding='utf-8'?>\n<tabledata><fields>")
	for _, c := range r.Columns {
		b.WriteString("<field>")
		b.WriteString(esc(c))
		b.WriteString("</field>")
	}
	b.WriteString("</fields><data>")
	for i, row := range r.Rows {
		fmt.Fprintf(&b, `<row id="%d">`, i+1)
		for j, v := range row {
			s := "(null)"
			if v != nil {
				s = text(v)
			}
			fmt.Fprintf(&b, "<column-%d>%s</column-%d>", j+1, esc(s), j+1)
		}
		b.WriteString("</row>")
		if b.Len() > 1<<20 {
			if _, err := io.WriteString(w, b.String()); err != nil {
				return err
			}
			b.Reset()
		}
	}
	b.WriteString("</data></tabledata>")
	_, err := io.WriteString(w, b.String())
	return err
}

// esc 转义 XML 文本，并去掉 XML 1.0 不允许的控制字符（否则文件无法被解析）。
func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(strings.Map(func(r rune) rune {
		if r == 0x9 || r == 0xA || r == 0xD || r >= 0x20 && r != 0xFFFE && r != 0xFFFF {
			return r
		}
		return -1
	}, s)))
	return b.String()
}

// MaxXLSXRows Excel 单个工作表的行数上限（含表头）。
const MaxXLSXRows = 1048576

// XLSX：一个工作表，第一行是列名，所有单元格按文本写入（NULL 为空串）。
func writeXLSX(w io.Writer, r Result) error {
	if len(r.Rows)+1 > MaxXLSXRows {
		return fmt.Errorf("Excel 最大支持行数为 %d，已超出", MaxXLSXRows)
	}
	f := excelize.NewFile()
	defer f.Close()
	sw, err := f.NewStreamWriter("Sheet1")
	if err != nil {
		return err
	}
	cell := func(row int) string { c, _ := excelize.CoordinatesToCellName(1, row); return c }
	head := make([]any, len(r.Columns))
	for i, c := range r.Columns {
		head[i] = c
	}
	if err := sw.SetRow(cell(1), head); err != nil {
		return err
	}
	for i, row := range r.Rows {
		vals := make([]any, len(row))
		for j, v := range row {
			vals[j] = text(v)
		}
		if err := sw.SetRow(cell(i+2), vals); err != nil {
			return err
		}
	}
	if err := sw.Flush(); err != nil {
		return err
	}
	return f.Write(w)
}

// SQL：每行一条 INSERT。表名无法从任意查询里可靠得到，写成 your_table_name 由使用者替换。
// 字符串与日期加引号，NULL 与空串写成 NULL，数字原样。
func writeSQL(w io.Writer, r Result) error {
	var b strings.Builder
	cols := strings.Join(r.Columns, ", ")
	mysql := r.Dialect == "mysql"
	for _, row := range r.Rows {
		vals := make([]string, len(row))
		for i, v := range row {
			vals[i] = sqlLiteral(v, mysql)
		}
		fmt.Fprintf(&b, "INSERT INTO your_table_name (%s) VALUES (%s);\n", cols, strings.Join(vals, ", "))
		if b.Len() > 1<<20 {
			if _, err := io.WriteString(w, b.String()); err != nil {
				return err
			}
			b.Reset()
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func sqlLiteral(v any, mysql bool) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return text(x)
	case bool:
		return text(x)
	}
	s := text(v)
	if s == "" {
		return "NULL"
	}
	if mysql {
		s = strings.ReplaceAll(s, `\`, `\\`)
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
