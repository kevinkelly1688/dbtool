// @author: kevinkelly
// @license: Apache Licence
// @file: parse.go
// @time: 2026/10/28

package sqlopt

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"dbtool/internal/sqlx"
)

// SQLRow 是从上传内容里拆出的一条 SQL。
type SQLRow struct {
	ID    string    `json:"sql_id"`
	SQL   string    `json:"sql"`
	Stats *LogStats `json:"stats,omitempty"` // 仅慢日志有
	Err   string    `json:"error,omitempty"` // 非空表示这条不会被分析（如过长）
}

// LogStats 是慢日志里一条记录的统计头。
type LogStats struct {
	Time         string  `json:"time,omitempty"`
	User         string  `json:"user,omitempty"`
	Host         string  `json:"host,omitempty"`
	DB           string  `json:"db,omitempty"`
	QueryTime    float64 `json:"query_time"`
	LockTime     float64 `json:"lock_time"`
	RowsSent     int64   `json:"rows_sent"`
	RowsExamined int64   `json:"rows_examined"`

	user, stats bool // 解析过程中的标记：已读到 User@Host / Query_time 行
}

// 上传内容的格式。
const (
	FormatSQL    = "sql"
	FormatMapper = "mapper"
	FormatSlow   = "slowlog"
)

func itoa(n int) string { return strconv.Itoa(n) }

const utf8BOM = "\xef\xbb\xbf"

// checkText 校验上传文本：大小、必须是有效 UTF-8 文本（没有 NUL 字节）。内容是不可信数据，只在内存里处理，不落盘。
func checkText(text string) (string, error) {
	if len(text) > MaxTextBytes {
		return "", bad("文件超过 5MB，请拆分后再上传")
	}
	if !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
		return "", bad("文件不是有效的 UTF-8 文本，请确认上传的是 .sql / Mapper XML / 慢日志文本")
	}
	return strings.TrimPrefix(text, utf8BOM), nil
}

// ParseText 把上传内容拆成 SQL 列表：MyBatis3 Mapper XML、MySQL 慢日志文本，或普通 SQL（按分号切分，去注释）。
// 返回识别到的格式。
func ParseText(text string) ([]SQLRow, string, error) {
	text, err := checkText(text)
	if err != nil {
		return nil, "", err
	}
	trim := strings.TrimSpace(text)
	if trim == "" {
		return []SQLRow{}, FormatSQL, nil
	}
	var rows []SQLRow
	format := FormatSQL
	switch {
	case looksLikeXML(trim):
		r, err := parseMapper(text)
		var be *Error
		switch {
		case err == nil:
			rows, format = r, FormatMapper
		case errors.As(err, &be):
			return nil, "", be
		default:
			// XML 解析失败时按普通 SQL 文本处理
			rows = splitPlain(text)
		}
	case looksLikeSlowLog(text):
		rows, format = parseSlowLog(text), FormatSlow
	default:
		rows = splitPlain(text)
	}
	if len(rows) > MaxStatements {
		return nil, "", bad(fmt.Sprintf("SQL 超过 %d 条，请拆分后再分析", MaxStatements))
	}
	for i := range rows {
		if len(rows[i].SQL) > MaxSQLBytes && rows[i].Err == "" {
			rows[i].Err = fmt.Sprintf("单条 SQL 超过 %dKB，已跳过", MaxSQLBytes/1024)
		}
	}
	return rows, format, nil
}

func looksLikeXML(trim string) bool {
	return strings.HasPrefix(trim, "<?xml") || strings.HasPrefix(trim, "<!DOCTYPE") || strings.HasPrefix(trim, "<mapper") || strings.HasPrefix(trim, "<!--")
}

func splitPlain(text string) []SQLRow {
	stmts := sqlx.Split(text)
	rows := make([]SQLRow, 0, len(stmts))
	for i, s := range stmts {
		rows = append(rows, SQLRow{ID: itoa(i + 1), SQL: s})
	}
	return rows
}

// ---- 慢日志 ----

var (
	reSlowMark  = regexp.MustCompile(`(?m)^# (?:Query_time|User@Host):`)
	reSlowStats = regexp.MustCompile(`Query_time:\s*([\d.]+)\s+Lock_time:\s*([\d.]+)\s+Rows_sent:\s*(\d+)\s+Rows_examined:\s*(\d+)`)
	reSlowUser  = regexp.MustCompile(`^# User@Host:\s*([^\[@\s]*)\s*\[[^\]]*\]\s*@\s*([^\[\s]*)\s*\[([^\]]*)\]`)
	reSetTS     = regexp.MustCompile(`(?i)^set\s+timestamp\s*=\s*\d+\s*;?\s*$`)
	reUseDB     = regexp.MustCompile("(?i)^use\\s+`?([^`;\\s]+)`?\\s*;?\\s*$")
)

func looksLikeSlowLog(text string) bool { return reSlowMark.MatchString(text) }

// parseSlowLog 解析 MySQL 慢日志文本（# Time / # User@Host / # Query_time 头 + 语句）。
// 去掉 SET timestamp、use 语句（库名记入统计），没有 SQL 的记录（如 administrator command）丢弃。
func parseSlowLog(text string) []SQLRow {
	var rows []SQLRow
	var cur *LogStats
	var sqlLines []string
	flush := func() {
		if cur != nil && len(sqlLines) > 0 {
			for _, st := range sqlx.Split(strings.Join(sqlLines, "\n")) {
				stats := *cur
				rows = append(rows, SQLRow{ID: itoa(len(rows) + 1), SQL: st, Stats: &stats})
			}
		}
		cur, sqlLines = nil, nil
	}
	lastDB := "" // 慢日志只在库切换时才写 use，沿用上一条的库
	fresh := func() { flush(); cur = &LogStats{DB: lastDB} }
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "# Time:"):
			fresh()
			cur.Time = strings.TrimSpace(strings.TrimPrefix(line, "# Time:"))
		case strings.HasPrefix(line, "# User@Host:"):
			if cur == nil || cur.user || len(sqlLines) > 0 {
				fresh()
			}
			cur.user = true
			if m := reSlowUser.FindStringSubmatch(line); m != nil {
				cur.User = m[1]
				cur.Host = m[2]
				if cur.Host == "" {
					cur.Host = m[3]
				}
			}
		case strings.HasPrefix(line, "# Query_time:"):
			if cur == nil || cur.stats || len(sqlLines) > 0 {
				fresh()
			}
			cur.stats = true
			if m := reSlowStats.FindStringSubmatch(line); m != nil {
				cur.QueryTime, _ = strconv.ParseFloat(m[1], 64)
				cur.LockTime, _ = strconv.ParseFloat(m[2], 64)
				cur.RowsSent, _ = strconv.ParseInt(m[3], 10, 64)
				cur.RowsExamined, _ = strconv.ParseInt(m[4], 10, 64)
			}
		case strings.HasPrefix(line, "#"):
			// 其他注释头（# Schema、# QC_Hit …）忽略
		case cur == nil:
			// 文件头（mysqld 版本、列名行）或没有头的游离内容
		default:
			t := strings.TrimSpace(line)
			if t == "" {
				continue
			}
			if len(sqlLines) == 0 {
				if reSetTS.MatchString(t) {
					continue
				}
				if m := reUseDB.FindStringSubmatch(t); m != nil {
					cur.DB, lastDB = m[1], m[1]
					continue
				}
			}
			sqlLines = append(sqlLines, line)
		}
	}
	flush()
	return rows
}
