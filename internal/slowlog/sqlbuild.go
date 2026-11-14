// @author: kevinkelly
// @license: Apache Licence
// @file: sqlbuild.go
// @time: 2026/10/28

package slowlog

import (
	"strings"
)

// 本文件只负责拼 SQL（纯函数，便于单测）：所有用户输入都走 ? 参数；
// 只有来自白名单映射表的列名/别名会出现在 SQL 文本里。

const (
	tblSlowLogs     = "dbt_slow_logs"
	tblReview       = "dbt_mysql_slow_query_review"
	tblReviewHist   = "dbt_mysql_slow_query_review_history"
	tblInstance     = "dbt_sql_instance"
	tblCollectState = "dbt_slowlog_collect_state"
)

// likeEscaper 转义 LIKE 的通配符（MySQL 默认转义符是反斜杠）。
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func likePattern(q string) string { return "%" + likeEscaper.Replace(q) + "%" }

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func idArgs(ids []uint) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func dir(desc bool) string {
	if desc {
		return "DESC"
	}
	return "ASC"
}

// ---- 查慢日志：dbt_slow_logs 聚合 ----

// 排序名 -> 子查询里的别名。
var groupSortCol = map[string]string{
	"exec_date": "exec_date", "count": "cnt", "query_time_avg": "qt_avg", "query_time_max": "qt_max",
	"rows_examined_avg": "re_avg", "rows_sent_avg": "rs_avg", "total_time": "qt_sum",
}

func groupWhere(q GroupQuery) (string, []any) {
	w := []string{"l.start_time >= ?", "l.start_time < ?"}
	args := []any{q.From, q.To}
	if len(q.InstanceIDs) > 0 {
		w = append(w, "l.instance_id IN ("+placeholders(len(q.InstanceIDs))+")")
		args = append(args, idArgs(q.InstanceIDs)...)
	}
	if q.DB != "" {
		w = append(w, "l.db_name = ?")
		args = append(args, q.DB)
	}
	if q.Search != "" {
		// 搜索范围：SQL 文本、库名、用户@主机（只占一个参数；NULL 列被 CONCAT_WS 忽略）。
		w = append(w, "CONCAT_WS(CHAR(10), l.sql_text, l.db_name, l.user_host) LIKE ?")
		args = append(args, likePattern(q.Search))
	}
	return strings.Join(w, " AND "), args
}

const groupKey = "l.instance_id, COALESCE(l.db_name,''), COALESCE(l.sql_md5,''), DATE_FORMAT(l.start_time,'%Y-%m-%d')"

func buildGroups(q GroupQuery) (listSQL string, listArgs []any, countSQL string, countArgs []any) {
	where, args := groupWhere(q)
	col := groupSortCol[q.Sort]
	if col == "" {
		col = "exec_date"
	}
	order := groupOrder(col, q.Desc, "")
	inner := "SELECT l.instance_id AS instance_id, COALESCE(l.db_name,'') AS db_name, COALESCE(l.sql_md5,'') AS sql_md5, " +
		"DATE_FORMAT(l.start_time,'%Y-%m-%d') AS exec_date, COUNT(*) AS cnt, MAX(l.id) AS last_id, " +
		"COALESCE(AVG(l.query_time_sec),0) AS qt_avg, COALESCE(MAX(l.query_time_sec),0) AS qt_max, COALESCE(MIN(l.query_time_sec),0) AS qt_min, COALESCE(SUM(l.query_time_sec),0) AS qt_sum, " +
		"COALESCE(AVG(l.rows_sent),0) AS rs_avg, COALESCE(MAX(l.rows_sent),0) AS rs_max, COALESCE(MIN(l.rows_sent),0) AS rs_min, " +
		"COALESCE(AVG(l.rows_examined),0) AS re_avg, COALESCE(MAX(l.rows_examined),0) AS re_max, COALESCE(MIN(l.rows_examined),0) AS re_min " +
		"FROM " + tblSlowLogs + " l WHERE " + where + " GROUP BY " + groupKey + " ORDER BY " + order + " LIMIT ? OFFSET ?"
	listSQL = "SELECT g.instance_id, COALESCE(i.instance_name,'') AS instance_name, g.db_name, g.sql_md5, g.exec_date, g.cnt, " +
		"COALESCE(s.user_host,'') AS user_host, COALESCE(s.sql_text,'') AS sql_text, " +
		"g.qt_avg, g.qt_max, g.qt_min, g.qt_sum, g.rs_avg, g.rs_max, g.rs_min, g.re_avg, g.re_max, g.re_min " +
		"FROM (" + inner + ") g " +
		"LEFT JOIN " + tblSlowLogs + " s ON s.id = g.last_id " +
		"LEFT JOIN " + tblInstance + " i ON i.id = g.instance_id " +
		"ORDER BY " + groupOrder(col, q.Desc, "g.")
	listArgs = append(append([]any{}, args...), q.Limit, q.Offset)
	countSQL = "SELECT COUNT(*) FROM (SELECT 1 FROM " + tblSlowLogs + " l WHERE " + where + " GROUP BY " + groupKey + ") t"
	countArgs = args
	return
}

func buildDetail(q DetailQuery) (string, []any) {
	w := []string{"instance_id = ?", "start_time >= ?", "start_time < ?"}
	args := []any{q.InstanceID, q.From, q.To}
	if q.MD5 != "" {
		w = append(w, "sql_md5 = ?")
		args = append(args, q.MD5)
	} else {
		w = append(w, "(sql_md5 IS NULL OR sql_md5 = '')")
	}
	if q.DB != "" {
		w = append(w, "db_name = ?")
		args = append(args, q.DB)
	} else {
		w = append(w, "(db_name IS NULL OR db_name = '')")
	}
	args = append(args, q.Limit)
	return "SELECT id, DATE_FORMAT(start_time,'%Y-%m-%d %H:%i:%s') AS start_time, COALESCE(user_host,'') AS user_host, COALESCE(db_name,'') AS db_name, " +
		"COALESCE(query_time_sec,0) AS query_time_sec, COALESCE(lock_time_sec,0) AS lock_time_sec, COALESCE(rows_sent,0) AS rows_sent, " +
		"COALESCE(rows_examined,0) AS rows_examined, COALESCE(sql_text,'') AS sql_text FROM " + tblSlowLogs +
		" WHERE " + strings.Join(w, " AND ") + " ORDER BY start_time DESC, id DESC LIMIT ?", args
}

// ---- 慢查日志：统计（两种来源共用别名） ----

var reviewSortCol = map[string]string{
	"create_time": "create_time", "db_name": "db_name", "query_time_avg": "query_time_avg", "exec_count": "exec_count",
	"exec_time_sum": "exec_time_sum", "parse_total_rows": "parse_total_rows", "return_total_rows": "return_total_rows",
	"parse_row_avg": "parse_row_avg", "return_row_avg": "return_row_avg", "query_time_max": "query_time_max", "first_seen": "first_seen",
}

func reviewOrder(q ReviewQuery, tie string) string {
	col := reviewSortCol[q.Sort]
	if col == "" {
		col = "exec_count"
	}
	return col + " " + dir(q.Desc) + ", " + tie
}

func collectedWhere(q ReviewQuery, withMD5 bool) (string, []any) {
	w := []string{"l.instance_id = ?", "l.start_time >= ?", "l.start_time < ?"}
	args := []any{q.InstanceID, q.From, q.To}
	if len(q.Multi) > 0 { // 全部实例汇总
		ids := make([]uint, len(q.Multi))
		for i, t := range q.Multi {
			ids[i] = t.InstanceID
		}
		w[0] = "l.instance_id IN (" + placeholders(len(ids)) + ")"
		args = append(append([]any{}, idArgs(ids)...), q.From, q.To)
	}
	if withMD5 {
		w = append(w, "l.sql_md5 IS NOT NULL", "l.sql_md5 <> ''")
	} else if q.MinSeconds > 0 { // 明细：单次耗时下限（统计是按平均耗时，放在 HAVING 里）
		w = append(w, "l.query_time_sec >= ?")
		args = append(args, q.MinSeconds)
	}
	if q.SQLID != "" {
		w = append(w, "l.sql_md5 = ?")
		args = append(args, q.SQLID)
	}
	if q.DB != "" {
		w = append(w, "l.db_name = ?")
		args = append(args, q.DB)
	}
	if q.Search != "" {
		w = append(w, "l.sql_text LIKE ?")
		args = append(args, likePattern(q.Search))
	}
	return strings.Join(w, " AND "), args
}

func buildCollectedReview(q ReviewQuery) (listSQL string, listArgs []any, countSQL string, countArgs []any) {
	where, args := collectedWhere(q, true)
	group, having := "l.sql_md5", ""
	if len(q.Multi) > 0 {
		group = "l.instance_id, l.sql_md5"
	}
	if q.MinSeconds > 0 { // 统计页：平均耗时不低于下限
		having = " HAVING AVG(l.query_time_sec) >= ?"
	}
	listSQL = "SELECT l.sql_md5 AS sql_id, MAX(l.instance_id) AS instance_id, MAX(l.id) AS last_id, DATE_FORMAT(MAX(l.start_time),'%Y-%m-%d %H:%i:%s') AS create_time, " +
		"DATE_FORMAT(MIN(l.start_time),'%Y-%m-%d %H:%i:%s') AS first_seen, COALESCE(MAX(l.query_time_sec),0) AS query_time_max, " +
		"COALESCE(AVG(l.lock_time_sec),0) AS lock_time_avg, " +
		"COALESCE(MAX(l.db_name),'') AS db_name, COUNT(*) AS exec_count, COALESCE(SUM(l.query_time_sec),0) AS exec_time_sum, " +
		"COALESCE(AVG(l.query_time_sec),0) AS query_time_avg, COALESCE(SUM(l.rows_examined),0) AS parse_total_rows, " +
		"COALESCE(SUM(l.rows_sent),0) AS return_total_rows, COALESCE(AVG(l.rows_examined),0) AS parse_row_avg, " +
		"COALESCE(AVG(l.rows_sent),0) AS return_row_avg FROM " + tblSlowLogs + " l WHERE " + where +
		" GROUP BY " + group + having + " ORDER BY " + reviewOrder(q, "sql_id ASC") + " LIMIT ? OFFSET ?"
	listArgs = append(append([]any{}, args...), q.Limit, q.Offset)
	if q.MinSeconds > 0 {
		listArgs = append(append([]any{}, args...), q.MinSeconds, q.Limit, q.Offset)
	}
	countSQL = "SELECT COUNT(DISTINCT l.sql_md5) FROM " + tblSlowLogs + " l WHERE " + where
	countArgs = args
	if len(q.Multi) > 0 || q.MinSeconds > 0 { // 分组后再数：要么按（实例, 指纹）分组，要么带 HAVING
		countSQL = "SELECT COUNT(*) FROM (SELECT 1 FROM " + tblSlowLogs + " l WHERE " + where + " GROUP BY " + group + having + ") t"
		if q.MinSeconds > 0 {
			countArgs = append(append([]any{}, args...), q.MinSeconds)
		}
	}
	return
}

// 明细排序名 -> dbt_slow_logs 列。
var collectedHistSortCol = map[string]string{
	"exec_start": "l.start_time", "db_name": "l.db_name", "host_address": "l.user_host", "exec_count": "l.start_time",
	"query_time_pct95": "l.query_time_sec", "query_time_sum": "l.query_time_sec", "lock_time_sum": "l.lock_time_sec",
	"parse_rows": "l.rows_examined", "return_rows": "l.rows_sent",
}

func buildCollectedHistory(q ReviewQuery) (listSQL string, listArgs []any, countSQL string, countArgs []any) {
	where, args := collectedWhere(q, false)
	col := collectedHistSortCol[q.Sort]
	if col == "" {
		col = "l.rows_examined"
	}
	listSQL = "SELECT l.id, COALESCE(l.sql_md5,'') AS sql_id, DATE_FORMAT(l.start_time,'%Y-%m-%d %H:%i:%s') AS exec_start, " +
		"COALESCE(l.db_name,'') AS db_name, COALESCE(l.user_host,'') AS host_address, COALESCE(l.sql_text,'') AS sql_text, " +
		"1 AS exec_count, COALESCE(l.query_time_sec,0) AS query_time_pct95, COALESCE(l.query_time_sec,0) AS query_time_sum, " +
		"COALESCE(l.lock_time_sec,0) AS lock_time_sum, COALESCE(l.rows_examined,0) AS parse_rows, COALESCE(l.rows_sent,0) AS return_rows " +
		"FROM " + tblSlowLogs + " l WHERE " + where + " ORDER BY " + col + " " + dir(q.Desc) + ", l.id DESC LIMIT ? OFFSET ?"
	listArgs = append(append([]any{}, args...), q.Limit, q.Offset)
	countSQL = "SELECT COUNT(*) FROM " + tblSlowLogs + " l WHERE " + where
	countArgs = args
	return
}

func bucketExpr(col, bucket string) string {
	if bucket == "hour" {
		return "DATE_FORMAT(" + col + ",'%Y-%m-%d %H:00')"
	}
	return "DATE_FORMAT(" + col + ",'%Y-%m-%d')"
}

// buildCollectedTrend 返回按桶聚合的次数/均值/最大值，以及用于估算 95% 耗时的明细查询（最多 maxTrendRows+1 行）。
func buildCollectedTrend(q TrendQuery) (aggSQL string, aggArgs []any, rowsSQL string, rowsArgs []any) {
	b := bucketExpr("start_time", q.Bucket)
	base := "instance_id = ? AND sql_md5 = ? AND start_time >= ? AND start_time < ?"
	args := []any{q.InstanceID, q.SQLID, q.From, q.To}
	aggSQL = "SELECT " + b + " AS t, COUNT(*) AS cnt, COALESCE(AVG(query_time_sec),0) AS avg_t, COALESCE(MAX(query_time_sec),0) AS max_t FROM " +
		tblSlowLogs + " WHERE " + base + " GROUP BY t ORDER BY t LIMIT ?"
	aggArgs = append(append([]any{}, args...), maxTrendPoint)
	rowsSQL = "SELECT " + b + " AS t, COALESCE(query_time_sec,0) AS qt FROM " + tblSlowLogs + " WHERE " + base + " ORDER BY start_time LIMIT ?"
	rowsArgs = append(append([]any{}, args...), maxTrendRows+1)
	return
}

func buildCollectedDatabases(instanceID uint, from, to string) (string, []any) {
	return "SELECT DISTINCT db_name FROM " + tblSlowLogs + " WHERE instance_id = ? AND start_time >= ? AND start_time < ? " +
		"AND db_name IS NOT NULL AND db_name <> '' ORDER BY db_name LIMIT 500", []any{instanceID, from, to}
}

// ---- pt-query-digest 表 ----

func digestWhere(q ReviewQuery) (string, []any) {
	w := []string{"h.hostname_max = ?", "h.ts_min >= ?", "h.ts_min < ?"}
	args := []any{q.HostKey, q.From, q.To}
	if len(q.Multi) > 0 { // 全部实例汇总
		args = args[:0]
		for _, t := range q.Multi {
			args = append(args, t.HostKey)
		}
		w[0] = "h.hostname_max IN (" + placeholders(len(q.Multi)) + ")"
		args = append(args, q.From, q.To)
	}
	if q.SQLID != "" {
		w = append(w, "h.checksum = ?")
		args = append(args, q.SQLID)
	}
	if q.DB != "" {
		w = append(w, "h.db_max = ?")
		args = append(args, q.DB)
	}
	return strings.Join(w, " AND "), args
}

func buildDigestReview(q ReviewQuery) (listSQL string, listArgs []any, countSQL string, countArgs []any) {
	where, args := digestWhere(q)
	if q.Search != "" {
		where += " AND h.checksum IN (SELECT checksum FROM " + tblReview + " WHERE fingerprint LIKE ?)"
		args = append(args, likePattern(q.Search))
	}
	group, having := "h.checksum", ""
	if len(q.Multi) > 0 {
		group = "h.hostname_max, h.checksum"
	}
	if q.MinSeconds > 0 { // 统计页：平均耗时不低于下限
		having = " HAVING COALESCE(SUM(h.`Query_time_sum`)/NULLIF(SUM(h.ts_cnt),0),0) >= ?"
	}
	listSQL = "SELECT h.checksum AS sql_id, MAX(h.hostname_max) AS host_key, DATE_FORMAT(MAX(h.ts_max),'%Y-%m-%d %H:%i:%s') AS create_time, " +
		"DATE_FORMAT(MIN(h.ts_min),'%Y-%m-%d %H:%i:%s') AS first_seen, COALESCE(MAX(h.`Query_time_max`),0) AS query_time_max, " +
		"COALESCE(SUM(h.`Lock_time_sum`)/NULLIF(SUM(h.ts_cnt),0),0) AS lock_time_avg, COALESCE(MAX(h.db_max),'') AS db_name, " +
		"COALESCE(SUM(h.ts_cnt),0) AS exec_count, COALESCE(SUM(h.`Query_time_sum`),0) AS exec_time_sum, " +
		"COALESCE(SUM(h.`Query_time_sum`)/NULLIF(SUM(h.ts_cnt),0),0) AS query_time_avg, " +
		"COALESCE(SUM(h.`Rows_examined_sum`),0) AS parse_total_rows, COALESCE(SUM(h.`Rows_sent_sum`),0) AS return_total_rows, " +
		"COALESCE(SUM(h.`Rows_examined_sum`)/NULLIF(SUM(h.ts_cnt),0),0) AS parse_row_avg, " +
		"COALESCE(SUM(h.`Rows_sent_sum`)/NULLIF(SUM(h.ts_cnt),0),0) AS return_row_avg " +
		"FROM " + tblReviewHist + " h WHERE " + where + " GROUP BY " + group + having + " ORDER BY " + reviewOrder(q, "sql_id ASC") + " LIMIT ? OFFSET ?"
	listArgs = append(append([]any{}, args...), q.Limit, q.Offset)
	if q.MinSeconds > 0 {
		listArgs = append(append([]any{}, args...), q.MinSeconds, q.Limit, q.Offset)
	}
	countSQL = "SELECT COUNT(DISTINCT h.checksum) FROM " + tblReviewHist + " h WHERE " + where
	countArgs = args
	if len(q.Multi) > 0 || q.MinSeconds > 0 {
		countSQL = "SELECT COUNT(*) FROM (SELECT 1 FROM " + tblReviewHist + " h WHERE " + where + " GROUP BY " + group + having + ") t"
		if q.MinSeconds > 0 {
			countArgs = append(append([]any{}, args...), q.MinSeconds)
		}
	}
	return
}

var digestHistSortCol = map[string]string{
	"exec_start": "h.ts_min", "db_name": "h.db_max", "host_address": "h.user_max", "exec_count": "h.ts_cnt",
	"query_time_pct95": "h.`Query_time_pct_95`", "query_time_sum": "h.`Query_time_sum`", "lock_time_sum": "h.`Lock_time_sum`",
	"parse_rows": "h.`Rows_examined_sum`", "return_rows": "h.`Rows_sent_sum`",
}

func buildDigestHistory(q ReviewQuery) (listSQL string, listArgs []any, countSQL string, countArgs []any) {
	where, args := digestWhere(q)
	if q.Search != "" {
		where += " AND h.sample LIKE ?"
		args = append(args, likePattern(q.Search))
	}
	if q.MinSeconds > 0 { // 单次耗时下限（pt-query-digest 每行是 5 分钟的汇总，按该行的平均耗时算）
		where += " AND COALESCE(h.`Query_time_sum`/NULLIF(h.ts_cnt,0),0) >= ?"
		args = append(args, q.MinSeconds)
	}
	col := digestHistSortCol[q.Sort]
	if col == "" {
		col = "h.`Rows_examined_sum`"
	}
	listSQL = "SELECT h.id, h.checksum AS sql_id, DATE_FORMAT(h.ts_min,'%Y-%m-%d %H:%i:%s') AS exec_start, COALESCE(h.db_max,'') AS db_name, " +
		"COALESCE(h.user_max,'') AS user_max, COALESCE(h.client_max,'') AS client_max, h.sample AS sql_text, COALESCE(h.ts_cnt,0) AS exec_count, " +
		"COALESCE(h.`Query_time_pct_95`,0) AS query_time_pct95, COALESCE(h.`Query_time_sum`,0) AS query_time_sum, " +
		"COALESCE(h.`Lock_time_sum`,0) AS lock_time_sum, COALESCE(h.`Rows_examined_sum`,0) AS parse_rows, COALESCE(h.`Rows_sent_sum`,0) AS return_rows " +
		"FROM " + tblReviewHist + " h WHERE " + where + " ORDER BY " + col + " " + dir(q.Desc) + ", h.id DESC LIMIT ? OFFSET ?"
	listArgs = append(append([]any{}, args...), q.Limit, q.Offset)
	countSQL = "SELECT COUNT(*) FROM " + tblReviewHist + " h WHERE " + where
	countArgs = args
	return
}

func buildDigestTrend(q TrendQuery) (string, []any) {
	b := bucketExpr("ts_min", q.Bucket)
	return "SELECT " + b + " AS t, COALESCE(SUM(ts_cnt),0) AS cnt, COALESCE(MAX(`Query_time_pct_95`),0) AS pct95, " +
			"COALESCE(SUM(`Query_time_sum`)/NULLIF(SUM(ts_cnt),0),0) AS avg_t, COALESCE(MAX(`Query_time_max`),0) AS max_t " +
			"FROM " + tblReviewHist + " WHERE hostname_max = ? AND checksum = ? AND ts_min >= ? AND ts_min < ? GROUP BY t ORDER BY t LIMIT ?",
		[]any{q.HostKey, q.SQLID, q.From, q.To, maxTrendPoint}
}

func buildDigestDatabases(hostKey, from, to string) (string, []any) {
	return "SELECT DISTINCT db_max FROM " + tblReviewHist + " WHERE hostname_max = ? AND ts_min >= ? AND ts_min < ? " +
		"AND db_max IS NOT NULL AND db_max <> '' ORDER BY db_max LIMIT 500", []any{hostKey, from, to}
}

// groupOrder 生成聚合列表的排序子句：先按所选列，再用固定的次序保证翻页稳定。prefix 为列前缀（外层查询用 "g."）。
func groupOrder(col string, desc bool, prefix string) string {
	p := func(c string) string { return prefix + c }
	return p(col) + " " + dir(desc) + ", " + p("exec_date") + " DESC, " + p("cnt") + " DESC, " + p("instance_id") + " ASC, " +
		p("db_name") + " ASC, " + p("sql_md5") + " ASC"
}

// buildInsertSlowLogs 拼多行 INSERT：空的用户@主机、库名、指纹写成 NULL。
func buildInsertSlowLogs(instanceID uint, rows []Entry, now string) (string, []any) {
	var sb strings.Builder
	sb.WriteString("INSERT INTO " + tblSlowLogs + " (instance_id, updated_at_micro, start_time, user_host, query_time_sec, lock_time_sec, " +
		"rows_sent, rows_examined, db_name, sql_text, sql_md5, created_at) VALUES ")
	args := make([]any, 0, len(rows)*12)
	for i, e := range rows {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString("(" + placeholders(12) + ")")
		args = append(args, instanceID, e.Micro, e.StartTime, nullable(e.UserHost), e.QueryTimeSec, e.LockTimeSec,
			e.RowsSent, e.RowsExamined, nullable(e.DBName), e.SQLText, nullable(e.SQLMD5), now)
	}
	return sb.String(), args
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
