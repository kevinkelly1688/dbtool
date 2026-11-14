// @author: kevinkelly
// @license: Apache Licence
// @file: fingerprint.go
// @time: 2026/10/28

package slowlog

import (
	"crypto/md5"
	"encoding/hex"
	"regexp"
	"strings"

	"dbtool/internal/sqlx"
)

// SQL 指纹：
// 去注释、转小写、字面量替换成 ?、折叠 IN/VALUES 列表、去掉操作符周围空格。
// 只差字面量或格式的 SQL 得到同一指纹。
//
// 这里用 sqlx 的词法去注释并整体转小写。
var (
	reSQString = regexp.MustCompile(`'(?:[^'\\]|\\.)*'`)
	reDQString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	reNumber   = regexp.MustCompile(`\b\d+\.?\d*\b`)
	reInList   = regexp.MustCompile(`(?i)\bin\s*\(\s*\?(?:\s*,\s*\?)*\s*\)`)
	reValues   = regexp.MustCompile(`(?is)values\s*\(.*?\)(\s*,\s*\(.*?\))*`)
	rePunct    = regexp.MustCompile(`\s*([=<>!,()])\s*`)
	reSpace    = regexp.MustCompile(`\s+`)
)

// Fingerprint 返回 SQL 的归一化指纹；空输入返回空串。
func Fingerprint(sql string) string {
	s := strings.ToLower(strings.TrimSpace(sqlx.StripComments(sql)))
	if s == "" {
		return ""
	}
	s = reSQString.ReplaceAllString(s, "?")
	s = reDQString.ReplaceAllString(s, "?")
	s = reNumber.ReplaceAllString(s, "?")
	s = reInList.ReplaceAllString(s, "in (?)")
	s = reValues.ReplaceAllString(s, "values (?)")
	s = rePunct.ReplaceAllString(s, "$1")
	s = reSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// SQLMD5 是指纹的 MD5（写入 slow_logs.sql_md5）；指纹为空返回空串。
func SQLMD5(sql string) string {
	fp := Fingerprint(sql)
	if fp == "" {
		return ""
	}
	sum := md5.Sum([]byte(fp))
	return hex.EncodeToString(sum[:])
}

var reMainTable = regexp.MustCompile("(?i)\\b(?:from|into|update|join)\\s+`?([a-zA-Z0-9_$.]+)`?")

// ExtractTable 从 SQL 里粗提取主表名，提不到返回空串。
func ExtractTable(sql string) string {
	m := reMainTable.FindStringSubmatch(sql)
	if m == nil {
		return ""
	}
	parts := strings.Split(m[1], ".")
	return parts[len(parts)-1]
}

// clipText 把过长的文本截断到 maxTextRunes 个字符，并返回是否截断。
func clipText(s string) (string, bool) {
	if len(s) <= maxTextRunes {
		return s, false
	}
	r := []rune(s)
	if len(r) <= maxTextRunes {
		return s, false
	}
	return string(r[:maxTextRunes]), true
}
