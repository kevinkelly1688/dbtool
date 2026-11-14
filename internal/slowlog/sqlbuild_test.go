// @author: kevinkelly
// @license: Apache Licence
// @file: sqlbuild_test.go
// @time: 2026/10/28

package slowlog

import (
	"strings"
	"testing"
)

const evil = `x'; DROP TABLE slow_logs; -- % _ \`

// 用户输入只能出现在参数里，不能拼进 SQL 文本。
func noEvil(t *testing.T, name, sql string) {
	t.Helper()
	for _, bad := range []string{"DROP TABLE", "x';", "--"} {
		if strings.Contains(sql, bad) {
			t.Errorf("%s: SQL 文本里出现了用户输入 %q:\n%s", name, bad, sql)
		}
	}
}

func countQ(s string) int { return strings.Count(s, "?") }

func TestBuildGroups(t *testing.T) {
	q := GroupQuery{InstanceIDs: []uint{3, 5}, Search: evil, From: "2026-10-01 00:00:00", To: "2026-10-07 00:00:00", Sort: "count", Desc: true, Limit: 30, Offset: 60}
	list, la, count, ca := buildGroups(q)
	noEvil(t, "list", list)
	noEvil(t, "count", count)
	if countQ(list) != len(la) || countQ(count) != len(ca) {
		t.Fatalf("占位符与参数个数不一致: %d/%d %d/%d", countQ(list), len(la), countQ(count), len(ca))
	}
	// 参数顺序：时间范围、实例、搜索，然后是 LIMIT/OFFSET
	if la[0] != q.From || la[1] != q.To || la[2] != uint(3) || la[3] != uint(5) || la[4] != `%x'; DROP TABLE slow\_logs; -- \% \_ \\%` || la[5] != 30 || la[6] != 60 {
		t.Errorf("参数: %#v", la)
	}
	for _, want := range []string{"l.start_time >= ?", "l.start_time < ?", "l.instance_id IN (?,?)", "CONCAT_WS(CHAR(10), l.sql_text, l.db_name, l.user_host) LIKE ?", "GROUP BY", "LIMIT ? OFFSET ?", "ORDER BY cnt DESC"} {
		if !strings.Contains(list, want) {
			t.Errorf("缺少 %q:\n%s", want, list)
		}
	}
	if !strings.Contains(list, "ORDER BY g.cnt DESC, g.exec_date DESC") {
		t.Errorf("外层也要排序: %s", list)
	}
	if strings.Contains(count, "LIMIT") || !strings.HasPrefix(count, "SELECT COUNT(*) FROM (SELECT 1 FROM dbt_slow_logs") {
		t.Errorf("计数: %s", count)
	}
	// 没有实例过滤与搜索时不带这些条件；未知排序回退到默认
	q = GroupQuery{From: "a", To: "b", Sort: "hack; drop", Limit: 10}
	list, la, _, _ = buildGroups(q)
	if strings.Contains(list, "instance_id IN") || strings.Contains(list, "LIKE") || strings.Contains(list, "hack") || !strings.Contains(list, "ORDER BY exec_date ASC") {
		t.Errorf("默认: %s", list)
	}
	if len(la) != 4 {
		t.Errorf("参数个数: %v", la)
	}
}

func TestBuildDetail(t *testing.T) {
	sqlText, args := buildDetail(DetailQuery{InstanceID: 1, DB: evil, MD5: "abc", From: "f", To: "t", Limit: 100})
	noEvil(t, "detail", sqlText)
	if countQ(sqlText) != len(args) || args[len(args)-1] != 100 || !strings.Contains(sqlText, "sql_md5 = ?") || !strings.Contains(sqlText, "db_name = ?") {
		t.Errorf("%s %v", sqlText, args)
	}
	sqlText, args = buildDetail(DetailQuery{InstanceID: 1, From: "f", To: "t", Limit: 100})
	if !strings.Contains(sqlText, "sql_md5 IS NULL") || !strings.Contains(sqlText, "db_name IS NULL") || countQ(sqlText) != len(args) {
		t.Errorf("空指纹/空库对应 NULL 分组: %s", sqlText)
	}
}

func TestBuildCollectedReviewAndHistory(t *testing.T) {
	q := ReviewQuery{InstanceID: 2, DB: evil, SQLID: "abc", Search: evil, From: "f", To: "t", Sort: "exec_time_sum", Desc: false, Limit: 20, Offset: 40}
	list, la, count, ca := buildCollectedReview(q)
	noEvil(t, "review", list)
	noEvil(t, "review-count", count)
	if countQ(list) != len(la) || countQ(count) != len(ca) || la[len(la)-2] != 20 || la[len(la)-1] != 40 {
		t.Errorf("参数: %d/%d %v", countQ(list), len(la), la)
	}
	if !strings.Contains(list, "ORDER BY exec_time_sum ASC, sql_id ASC") || !strings.Contains(list, "l.instance_id = ?") || !strings.Contains(list, "l.sql_md5 IS NOT NULL") {
		t.Errorf("%s", list)
	}
	list, _, _, _ = buildCollectedReview(ReviewQuery{Sort: "evil", From: "f", To: "t", Desc: true})
	if !strings.Contains(list, "ORDER BY exec_count DESC") || strings.Contains(list, "evil") {
		t.Errorf("未知排序应回退: %s", list)
	}

	q.Sort = "query_time_sum"
	list, la, count, ca = buildCollectedHistory(q)
	noEvil(t, "history", list)
	if countQ(list) != len(la) || countQ(count) != len(ca) || !strings.Contains(list, "ORDER BY l.query_time_sec ASC, l.id DESC LIMIT ? OFFSET ?") {
		t.Errorf("%s %v", list, la)
	}
	list, _, _, _ = buildCollectedHistory(ReviewQuery{Sort: "bad", From: "f", To: "t", Desc: true})
	if !strings.Contains(list, "ORDER BY l.rows_examined DESC") {
		t.Errorf("明细默认排序: %s", list)
	}
}

func TestBuildDigest(t *testing.T) {
	q := ReviewQuery{HostKey: "10.0.0.1:3306", DB: evil, SQLID: "abc", Search: evil, From: "f", To: "t", Sort: "parse_row_avg", Desc: true, Limit: 30, Offset: 0}
	list, la, count, ca := buildDigestReview(q)
	noEvil(t, "digest", list)
	noEvil(t, "digest-count", count)
	if countQ(list) != len(la) || countQ(count) != len(ca) {
		t.Errorf("占位符: %d/%d %d/%d", countQ(list), len(la), countQ(count), len(ca))
	}
	for _, want := range []string{"h.hostname_max = ?", "h.ts_min >= ?", "h.ts_min < ?", "h.checksum = ?", "h.db_max = ?",
		"dbt_mysql_slow_query_review WHERE fingerprint LIKE ?", "GROUP BY h.checksum", "ORDER BY parse_row_avg DESC, sql_id ASC LIMIT ? OFFSET ?", "NULLIF(SUM(h.ts_cnt),0)"} {
		if !strings.Contains(list, want) {
			t.Errorf("缺少 %q:\n%s", want, list)
		}
	}
	if la[0] != "10.0.0.1:3306" {
		t.Errorf("第一个参数应是 host:port: %v", la[0])
	}
	list, la, count, ca = buildDigestHistory(q)
	noEvil(t, "digest-history", list)
	if countQ(list) != len(la) || countQ(count) != len(ca) || !strings.Contains(list, "h.sample LIKE ?") || !strings.Contains(list, "ORDER BY h.`Rows_examined_sum` DESC, h.id DESC LIMIT ? OFFSET ?") {
		t.Errorf("%s", list)
	}
	list, la = buildDigestTrend(TrendQuery{HostKey: "h", SQLID: "abc", From: "f", To: "t", Bucket: "hour"})
	if countQ(list) != len(la) || !strings.Contains(list, "%Y-%m-%d %H:00") || !strings.Contains(list, "GROUP BY t ORDER BY t LIMIT ?") || !strings.Contains(list, "hostname_max = ?") {
		t.Errorf("趋势: %s", list)
	}
	list, _ = buildDigestTrend(TrendQuery{HostKey: "h", SQLID: "abc", From: "f", To: "t", Bucket: "day"})
	if strings.Contains(list, "%H") {
		t.Errorf("按天: %s", list)
	}
}

func TestBuildCollectedTrendAndDatabases(t *testing.T) {
	agg, aa, rows, ra := buildCollectedTrend(TrendQuery{InstanceID: 1, SQLID: evil, From: "f", To: "t", Bucket: "day"})
	noEvil(t, "trend-agg", agg)
	noEvil(t, "trend-rows", rows)
	if countQ(agg) != len(aa) || countQ(rows) != len(ra) || ra[len(ra)-1] != maxTrendRows+1 || aa[len(aa)-1] != maxTrendPoint {
		t.Errorf("%v %v", aa, ra)
	}
	if !strings.Contains(rows, "LIMIT ?") || !strings.Contains(agg, "start_time >= ?") {
		t.Error("趋势必须带时间范围与上限")
	}
	q, a := buildCollectedDatabases(1, "f", "t")
	if countQ(q) != len(a) || !strings.Contains(q, "LIMIT 500") {
		t.Errorf("%s", q)
	}
	q, a = buildDigestDatabases("h", "f", "t")
	if countQ(q) != len(a) || !strings.Contains(q, "LIMIT 500") {
		t.Errorf("%s", q)
	}
}

func TestLikePatternAndPlaceholders(t *testing.T) {
	if got := likePattern(`50%_off\`); got != `%50\%\_off\\%` {
		t.Errorf("%q", got)
	}
	if placeholders(0) != "" || placeholders(1) != "?" || placeholders(3) != "?,?,?" {
		t.Error("placeholders")
	}
}

func TestPercentile95(t *testing.T) {
	if percentile95(nil) != 0 {
		t.Error("空")
	}
	if got := percentile95([]float64{5}); got != 5 {
		t.Error(got)
	}
	vals := make([]float64, 100)
	for i := range vals {
		vals[i] = float64(100 - i) // 乱序：100..1
	}
	if got := percentile95(vals); got != 96 { // 排序后下标 int(100*0.95)=95 -> 第 96 个值
		t.Errorf("取法与预期一致: %v", got)
	}
}

func TestBuildInsertSlowLogs(t *testing.T) {
	rows := []Entry{
		{Micro: 1, StartTime: "2026-10-06 10:00:00.000000", UserHost: "a@b", QueryTimeSec: 1.5, DBName: "shop", SQLText: evil, SQLMD5: "m"},
		{Micro: 2, StartTime: "2026-10-06 10:00:01.000000", SQLText: "select 1"}, // 用户、库、指纹为空 -> NULL
	}
	q, args := buildInsertSlowLogs(7, rows, "2026-10-06 10:05:00")
	noEvil(t, "insert", q)
	if countQ(q) != len(args) || len(args) != 24 || strings.Count(q, "(?,?,?,?,?,?,?,?,?,?,?,?)") != 2 {
		t.Fatalf("占位符与参数: %d/%d\n%s", countQ(q), len(args), q)
	}
	if args[0] != uint(7) || args[9] != evil || args[3] != "a@b" || args[10] != "m" || args[23] != "2026-10-06 10:05:00" {
		t.Errorf("第一行参数: %#v", args[:12])
	}
	if args[15] != nil || args[20] != nil || args[22] != nil {
		t.Errorf("空值应写成 NULL: %#v", args[12:])
	}
}

func TestCollectSQLShape(t *testing.T) {
	for name, q := range map[string]string{"mysql": mysqlSlowLogSQL, "doris": dorisAuditSQL} {
		if strings.Count(q, "?") != 2 || !strings.Contains(q, "LIMIT ?") || !strings.Contains(q, "ASC") {
			t.Errorf("%s: 采集 SQL 应是“游标 > ? ORDER BY … LIMIT ?”: %s", name, q)
		}
	}
	if !strings.Contains(mysqlSlowLogSQL, "mysql.slow_log") || !strings.Contains(dorisAuditSQL, "__internal_schema.audit_log") {
		t.Error("数据源表名")
	}
}
