// @author: kevinkelly
// @license: Apache Licence
// @file: writer_sql.go
// @time: 2026/10/28

package engine

import (
	"bytes"
	"regexp"
	"strings"
)

// sqlFormatter：每批若干条多行 INSERT（每条最多 sqlRowsPerStmt 行），可直接 mysql < file.sql 导入。
// 表名用源表名（不带库名），导入时由使用者选择目标库。字符串用反斜杠转义，二进制列写 0x 十六进制，NULL 写 NULL。
type sqlFormatter struct{}

const sqlRowsPerStmt = 200

var numericLit = regexp.MustCompile(`^[-+]?(\d+(\.\d*)?|\.\d+)([eE][-+]?\d+)?$`)

func (sqlFormatter) ext() string { return "sql" }

func (sqlFormatter) header(table string, _ []string) []byte {
	return []byte("-- dbtool archive backup\n-- table: " + strings.ReplaceAll(table, "\n", " ") + "\n/*!40101 SET NAMES utf8mb4 */;\n")
}

func (sqlFormatter) rows(table string, cols []string, kinds []colKind, rows []Row) []byte {
	var b bytes.Buffer
	head := "INSERT INTO " + quoteIdent(table) + " (" + quoteIdents(cols) + ") VALUES\n"
	for start := 0; start < len(rows); start += sqlRowsPerStmt {
		end := start + sqlRowsPerStmt
		if end > len(rows) {
			end = len(rows)
		}
		b.WriteString(head)
		for ri := start; ri < end; ri++ {
			r := rows[ri]
			b.WriteByte('(')
			for i := range cols {
				if i > 0 {
					b.WriteString(", ")
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
				switch {
				case null:
					b.WriteString("NULL")
				case k == kindBinary:
					if s == "0x" {
						b.WriteString("''")
					} else {
						b.WriteString(s)
					}
				case k == kindNumber && numericLit.MatchString(s):
					b.WriteString(s)
				default:
					b.WriteString(sqlLiteralString(s))
				}
			}
			b.WriteByte(')')
			if ri < end-1 {
				b.WriteString(",\n")
			} else {
				b.WriteString(";\n")
			}
		}
	}
	return b.Bytes()
}

func formatterFor(format string) batchFormatter {
	if format == "sql" {
		return sqlFormatter{}
	}
	return csvFormatter{}
}
