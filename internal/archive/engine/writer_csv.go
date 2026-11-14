// @author: kevinkelly
// @license: Apache Licence
// @file: writer_csv.go
// @time: 2026/10/28

package engine

import (
	"bytes"
	"strings"
)

// batchFormatter 把一批行编码成备份文件里的一段字节。header 只在每个文件的开头调用一次。
type batchFormatter interface {
	ext() string
	header(table string, cols []string) []byte
	rows(table string, cols []string, kinds []colKind, rows []Row) []byte
}

// csvFormatter：RFC 4180。第一行是列名；NULL 写成不带引号的 \N，空字符串写成 ""；
// 二进制列（BLOB / BINARY / BIT 等）写成 0x 十六进制。换行统一 \n。
type csvFormatter struct{}

func (csvFormatter) ext() string { return "csv" }

func csvField(s string, null bool) string {
	if null {
		return `\N`
	}
	if s == "" || s == `\N` || strings.ContainsAny(s, ",\"\n\r") || s[0] == ' ' || s[len(s)-1] == ' ' {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func (csvFormatter) header(_ string, cols []string) []byte {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = csvField(c, false)
	}
	return []byte(strings.Join(parts, ",") + "\n")
}

func (csvFormatter) rows(_ string, cols []string, kinds []colKind, rows []Row) []byte {
	var b bytes.Buffer
	for _, r := range rows {
		for i := range cols {
			if i > 0 {
				b.WriteByte(',')
			}
			var v any
			if i < len(r) {
				v = r[i]
			}
			k := kindText
			if i < len(kinds) {
				k = kinds[i]
			}
			s, null := valueText(v, k)
			b.WriteString(csvField(s, null))
		}
		b.WriteByte('\n')
	}
	return b.Bytes()
}
