// @author: kevinkelly
// @license: Apache Licence
// @file: fingerprint.go
// @time: 2026/10/28

package sqlopt

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"dbtool/internal/sqlx"
)

var (
	reInList  = regexp.MustCompile(`\(\s*\?(?:\s*,\s*\?)+\s*\)`)
	reTuples  = regexp.MustCompile(`(\(\?\+\))(?:\s*,\s*\(\?\+\))+`)
	reLimitFp = regexp.MustCompile(`\blimit \?(?:\s*,\s*\?| offset \?)`)
	reHexTail = regexp.MustCompile(`^[xX][0-9a-fA-F]+$`)
	reExpTail = regexp.MustCompile(`^[eE][0-9]+$`)
)

// Fingerprint 把 SQL 归一化成“指纹”：去注释、字面量换成 ?、关键字转小写、空白压成一个空格，
// IN 列表与多行 VALUES 折叠成 (?+)，LIMIT 的偏移量与行数合并成 limit ?。同一类语句得到同一个指纹，用来分组统计。
// 规则参考 pt-fingerprint，但只做词法级处理。
func Fingerprint(sql string) string {
	toks := sqlx.Tokenize(sql)
	var b strings.Builder
	space := func() {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), " ") {
			b.WriteByte(' ')
		}
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch t.Kind {
		case sqlx.Comment:
			space()
		case sqlx.Space:
			space()
		case sqlx.String:
			b.WriteByte('?')
		case sqlx.Number:
			// 0x1F、1e10 被词法切成数字 + 单词，合并成一个 ?
			if i+1 < len(toks) && toks[i+1].Kind == sqlx.Word && toks[i+1].Start == t.End {
				if (t.Text == "0" && reHexTail.MatchString(toks[i+1].Text)) || reExpTail.MatchString(toks[i+1].Text) {
					i++
				}
			}
			b.WriteByte('?')
		case sqlx.Quoted:
			if strings.HasPrefix(t.Text, `"`) { // MySQL 默认模式下双引号是字符串
				b.WriteByte('?')
			} else {
				b.WriteString(t.Text)
			}
		case sqlx.Word:
			b.WriteString(strings.ToLower(t.Text))
		default:
			b.WriteString(t.Text)
		}
	}
	fp := strings.TrimSpace(b.String())
	fp = strings.TrimRight(fp, "; ")
	fp = reInList.ReplaceAllString(fp, "(?+)")
	fp = reTuples.ReplaceAllString(fp, "$1")
	fp = reLimitFp.ReplaceAllString(fp, "limit ?")
	return fp
}

// FingerprintID 是指纹的短哈希（16 位十六进制），用作分组键。
func FingerprintID(fp string) string {
	h := sha256.Sum256([]byte(fp))
	return hex.EncodeToString(h[:8])
}

// 语句类型。
const (
	TypeSelect  = "select"
	TypeInsert  = "insert"
	TypeReplace = "replace"
	TypeUpdate  = "update"
	TypeDelete  = "delete"
	TypeDDL     = "ddl"
	TypeOther   = "other"
)

// StatementType 判断语句类型。WITH 开头的按 CTE 之后的主语句判断。
func StatementType(sql string) string { return parseStmt(sql).kind }
