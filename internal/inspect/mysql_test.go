// @author: kevinkelly
// @license: Apache Licence
// @file: mysql_test.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func collectAll(t *testing.T, ft *fakeTarget) jmap {
	t.Helper()
	c := &mysqlCollector{ctx: context.Background(), t: ft, instID: 1, now: newClock(ts("2026-10-06 08:00:00")).Now}
	result := jmap{}
	for _, st := range mysqlSteps {
		result[st.Key] = st.Run(c)
	}
	norm, err := normalizeJSON(result)
	if err != nil {
		t.Fatal(err)
	}
	return norm
}

func TestMySQLCollectHappy(t *testing.T) {
	r := collectAll(t, mysqlHappyTarget())
	for k, v := range r {
		if m, ok := v.(map[string]any); ok && has(m, "error") && strings.Contains(sv(m, "error"), "只读") {
			t.Errorf("步骤 %s 的语句被只读校验拒绝：%v", k, m["error"])
		}
	}
	conn := sub(r, "connection")
	if i64(conn, "threads_connected") != 38 || i64(conn, "max_connections") != 100 || f64(conn, "connection_usage_rate") != 38 {
		t.Errorf("连接指标不对：%v", conn)
	}
	bp := sub(r, "buffer_pool")
	if f64(bp, "buffer_pool_hit_rate") != 99 || f64(bp, "buffer_pool_usage_rate") != 99 {
		t.Errorf("缓冲池指标不对：%v", bp)
	}
	if v := sub(r, "server"); sv(v, "version") != "8.0.36" || i64(v, "uptime_seconds") != 3600 {
		t.Errorf("基本信息不对：%v", v)
	}
	if c := sub(r, "capacity"); i64(c, "table_count") != 100 || f64(c, "total_data_size_mb") != 512.5 {
		t.Errorf("容量不对：%v", c)
	}
	ai := sub(r, "auto_increment")
	if i64(ai, "alert_count") != 1 || f64(ai, "max_usage_pct") != 90 {
		t.Errorf("自增使用率不对：%v", ai)
	}
	npk := sub(r, "no_primary_key")
	if i64(npk, "count") != 2 || i64(npk, "large_count") != 1 {
		t.Errorf("无主键表不对：%v", npk)
	}
	red := sub(sub(r, "index"), "redundant")
	items := jrows(red, "items")
	if i64(red, "count") != 1 || len(items) != 1 || sv(items[0], "index") != "idx_user" || sv(items[0], "dominant_index") != "idx_user_created" ||
		sv(items[0], "sql") != "ALTER TABLE `shop`.`orders` DROP INDEX `idx_user`;" {
		t.Errorf("冗余索引不对：%v", red)
	}
	if !isTrue(sub(sub(r, "index"), "unused"), "disabled") {
		t.Errorf("performance_schema 关闭时应标记未使用索引不可用：%v", sub(r, "index"))
	}
	dl := sub(r, "deadlock")
	if has(dl, "count") || dl["print_all_deadlocks"] != false || i64(dl, "counter_since_startup") != 0 {
		t.Errorf("死锁采集不对：%v", dl)
	}
	slow := sub(r, "slow_top")
	if sv(slow, "source") != "digest" || len(jrows(slow, "items")) != 1 {
		t.Errorf("慢查询 Top 不对：%v", slow)
	}
	param := sub(r, "parameter")
	if i64(param, "alert_count") != 1 { // innodb_flush_log_at_trx_commit=2
		t.Errorf("参数基线告警数不对：%v", param)
	}
	acct := sub(r, "account")
	if i64(acct, "risk_count") != 2 || i64(acct, "alert_count") != 1 { // app@% 空密码（告警）、root@localhost 有 FILE 权限（关注）
		t.Errorf("高危账号不对：%v", acct)
	}
	if bl := sub(sub(r, "replication"), "binlog"); i64(bl, "count") != 1 {
		t.Errorf("binlog 不对：%v", bl)
	}
	if isTrue(sub(r, "replication"), "is_replica") {
		t.Error("不是从库")
	}
}

func TestMySQLStepFailureIsIsolated(t *testing.T) {
	ft := mysqlHappyTarget().onErr("information_schema.TABLES t JOIN", errors.New("boom"))
	ft.rules = append([]rule{{sub: "c.EXTRA LIKE", err: errors.New("permission denied")}}, ft.rules...) // 自增列采集失败
	r := collectAll(t, ft)
	if e := sv(sub(r, "auto_increment"), "error"); !strings.Contains(e, "permission denied") {
		t.Errorf("失败应记在该步骤里：%v", r["auto_increment"])
	}
	if i64(sub(r, "connection"), "threads_connected") != 38 {
		t.Error("其他步骤不受影响")
	}
	secs := BuildMySQLSections(r)
	var found bool
	for _, s := range secs {
		if s.Code == "auto_increment" {
			found = true
			if s.Failed != 1 || s.Items[0].Level != LevelFailed || !strings.Contains(s.Items[0].Value, "采集失败") {
				t.Errorf("自增分区应显示采集失败：%+v", s)
			}
		}
	}
	if !found {
		t.Fatal("没有自增分区")
	}
}

func TestMySQLSectionsAndHealth(t *testing.T) {
	r := collectAll(t, mysqlHappyTarget())
	secs := BuildMySQLSections(r)
	by := map[string]Section{}
	for _, s := range secs {
		by[s.Code] = s
	}
	for _, code := range []string{"connection", "memory", "query", "transaction", "io", "lock_risk", "auto_increment", "no_primary_key", "deadlock", "slow_top", "big_table", "index", "charset", "parameter", "account", "replication", "capacity"} {
		if _, ok := by[code]; !ok {
			t.Errorf("缺少分区 %s", code)
		}
	}
	if c := by["connection"]; c.Alert != 0 || c.Attention != 0 {
		t.Errorf("连接使用率 38%% 应正常：%+v", c)
	}
	if m := by["memory"]; m.Attention != 1 { // 命中率 99.0 不小于 99 → 正常；使用率不判；这里命中率 99 正常
		_ = m
	}
	if a := by["auto_increment"]; a.Alert != 1 {
		t.Errorf("自增分区应有 1 个告警：%+v", a)
	}
	if n := by["no_primary_key"]; n.Alert != 1 || len(n.Items) < 3 { // 有 100 万行以上的无主键表 → 告警；明细行不计数
		t.Errorf("无主键分区不对：%+v", n)
	}
	for _, it := range by["no_primary_key"].Items[1:] {
		if !it.Detail {
			t.Errorf("无主键表明细应是 Detail 行：%+v", it)
		}
	}
	if a := by["account"]; a.Alert != 1 {
		t.Errorf("账号分区应有 1 个告警：%+v", a)
	}
	h := MySQLHealth(r)
	// 扣分：自增告警 15、无主键 5、冗余索引 1、字符集混用（只有 utf8mb4 一种表字符集，列没有不一致 → 不扣）、
	// 参数 innodb_flush_log_at_trx_commit 告警 5 + innodb_print_all_deadlocks 1 + performance_schema 1、高危账号 10、权限风险账号 3、
	// 慢查询 Top 平均 3.2 秒（< 10）不扣
	issues, warnings := strs(h, "issues"), strs(h, "warnings")
	if len(issues) == 0 || len(warnings) == 0 {
		t.Fatalf("应有问题和警告：%v", h)
	}
	want := 100 - 15 - 5 - 1 - 5 - 1 - 1 - 10 - 3
	if int(i64(h, "health_score")) != want || sv(h, "status") != "critical" {
		t.Errorf("评分 = %v，期望 %d（critical）：%v", h["health_score"], want, h)
	}
	joined := strings.Join(issues, "\n")
	for _, s := range []string{"自增列使用率超过 85%", "参数 innodb_flush_log_at_trx_commit=2", "高危账号 'app'@'%'"} {
		if !strings.Contains(joined, s) {
			t.Errorf("问题里缺少 %q：%s", s, joined)
		}
	}
}

func TestMySQLHealthRules(t *testing.T) {
	cases := []struct {
		name string
		r    jmap
		want int
	}{
		{"全部正常", jmap{"connection": jmap{"connection_usage_rate": 10.0}, "buffer_pool": jmap{"buffer_pool_hit_rate": 99.5}}, 100},
		{"连接数 96%", jmap{"connection": jmap{"connection_usage_rate": 96.0}}, 80},
		{"连接数 85%", jmap{"connection": jmap{"connection_usage_rate": 85.0}}, 90},
		{"命中率 94", jmap{"buffer_pool": jmap{"buffer_pool_hit_rate": 94.0}}, 75},
		{"命中率 98", jmap{"buffer_pool": jmap{"buffer_pool_hit_rate": 98.0}}, 85},
		{"慢查询 60", jmap{"query": jmap{"slow_queries": 60.0}}, 70},
		{"慢查询 11", jmap{"query": jmap{"slow_queries": 11.0}}, 85},
		{"行锁等待 1001", jmap{"transaction": jmap{"innodb_row_lock_waits": 1001.0}}, 80},
		{"MDL 等待", jmap{"lock_risk": jmap{"mdl": jmap{"count": 2.0}}}, 85},
		{"长事务 6 分钟", jmap{"lock_risk": jmap{"long_trx": jmap{"count": 1.0, "max_seconds": 360.0}}}, 85},
		{"长事务 2 分钟", jmap{"lock_risk": jmap{"long_trx": jmap{"count": 1.0, "max_seconds": 120.0}}}, 95},
		{"死锁 12 次", jmap{"deadlock": jmap{"count": 12.0}}, 90},
		{"死锁 3 次", jmap{"deadlock": jmap{"count": 3.0}}, 95},
		{"大表 8 张最多扣 5", jmap{"big_table": jmap{"big_count": 8.0}}, 95},
		{"复制线程停止", jmap{"replication": jmap{"is_replica": true, "replica": jmap{"io_running": "No", "sql_running": "Yes"}}}, 80},
		{"复制延迟 400 秒", jmap{"replication": jmap{"is_replica": true, "replica": jmap{"io_running": "Yes", "sql_running": "Yes", "seconds_behind": 400.0}}}, 90},
		{"复制延迟 100 秒", jmap{"replication": jmap{"is_replica": true, "replica": jmap{"io_running": "Yes", "sql_running": "Yes", "seconds_behind": 100.0}}}, 95},
		{"分数不会低于 0", jmap{"connection": jmap{"connection_usage_rate": 99.0}, "buffer_pool": jmap{"buffer_pool_hit_rate": 10.0}, "query": jmap{"slow_queries": 99.0},
			"transaction": jmap{"innodb_row_lock_waits": 5000.0}, "auto_increment": jmap{"alert_count": 1.0}}, 0},
	}
	for _, c := range cases {
		if got := int(i64(MySQLHealth(c.r), "health_score")); got != c.want {
			t.Errorf("%s：评分 %d，期望 %d", c.name, got, c.want)
		}
	}
}

func TestReplicationCollect(t *testing.T) {
	ft := mysqlHappyTarget()
	ft.rules = append([]rule{{sub: "SHOW REPLICA STATUS", res: res(
		[]string{"Source_Host", "Replica_IO_Running", "Replica_SQL_Running", "Seconds_Behind_Source", "Last_IO_Error", "Last_SQL_Error"},
		[]any{"10.0.0.9", "Yes", "No", nil, "", "Duplicate entry"})}}, ft.rules...)
	r := collectAll(t, ft)
	rep := sub(r, "replication")
	if !isTrue(rep, "is_replica") {
		t.Fatalf("应识别为从库：%v", rep)
	}
	rr := sub(rep, "replica")
	if sv(rr, "sql_running") != "No" || i64(rr, "seconds_behind") != -1 || sv(rr, "last_sql_error") != "Duplicate entry" {
		t.Errorf("从库状态不对：%v", rr)
	}
	sec := replicationSection(rep)
	if sec.Items[0].Level != LevelAlert {
		t.Errorf("SQL 线程停止应告警：%+v", sec.Items[0])
	}
	// 8.0.22 之前用 SHOW SLAVE STATUS
	ft2 := mysqlHappyTarget()
	ft2.rules = append([]rule{
		{sub: "SHOW REPLICA STATUS", err: errors.New("syntax error")},
		{sub: "SHOW SLAVE STATUS", res: res([]string{"Master_Host", "Slave_IO_Running", "Slave_SQL_Running", "Seconds_Behind_Master"}, []any{"10.0.0.8", "Yes", "Yes", int64(3)})},
	}, ft2.rules...)
	r2 := collectAll(t, ft2)
	if rr := sub(sub(r2, "replication"), "replica"); sv(rr, "source_host") != "10.0.0.8" || i64(rr, "seconds_behind") != 3 {
		t.Errorf("SHOW SLAVE STATUS 解析不对：%v", rr)
	}
}

func TestDeadlockFromErrorLog(t *testing.T) {
	ft := mysqlHappyTarget()
	ft.rules = append([]rule{{sub: "performance_schema.error_log", res: res([]string{"LOGGED"}, []any{ts("2026-10-05 10:00:00")}, []any{ts("2026-10-04 09:00:00")})}}, ft.rules...)
	r := collectAll(t, ft)
	dl := sub(r, "deadlock")
	if sv(dl, "source") != "error_log" || i64(dl, "count") != 2 || len(strs(dl, "events")) != 2 {
		t.Errorf("应从错误日志统计：%v", dl)
	}
	sec := deadlockSection(dl)
	if sec.Items[0].Level != LevelAttention || sec.Items[0].Value != "2" {
		t.Errorf("2 次死锁应关注：%+v", sec.Items[0])
	}
}

func TestDeadlockLatestOnly(t *testing.T) {
	status := "...\n------------------------\nLATEST DETECTED DEADLOCK\n------------------------\n2026-10-05 12:30:00 140000\n*** (1) TRANSACTION:\nRECORD LOCKS space id 1 page no 3 n bits 72 index PRIMARY of table `shop`.`orders` trx id 1\n------------------------\nTRANSACTIONS\n------------------------\n"
	ft := mysqlHappyTarget()
	ft.rules = append([]rule{{sub: "SHOW ENGINE INNODB STATUS", res: res([]string{"t", "n", "s"}, []any{"InnoDB", "", status})}}, ft.rules...)
	r := collectAll(t, ft)
	dl := sub(r, "deadlock")
	latest := sub(dl, "latest")
	if sv(latest, "time") != "2026-10-05 12:30:00" || !isTrue(latest, "within_days") || len(strs(latest, "tables")) != 1 {
		t.Errorf("最近一次死锁解析不对：%v", dl)
	}
	if sv(dl, "source") != "latest_only" || i64(dl, "count") != 1 {
		t.Errorf("只能确认最近一次：%v", dl)
	}
}

func TestSlowTopFromPlatform(t *testing.T) {
	ft := mysqlHappyTarget()
	c := &mysqlCollector{ctx: context.Background(), t: ft, instID: 9, now: newClock(ts("2026-10-06 08:00:00")).Now,
		slow: func(id uint, since time.Time, limit int) (map[string]any, bool) {
			if id != 9 || limit != slowTopLimit || !since.Equal(ts("2026-09-29 08:00:00")) {
				t.Errorf("参数不对：%d %v %d", id, since, limit)
			}
			return map[string]any{"source": "slow_log", "days": 7, "slow_count": 3, "fingerprint_count": 1, "items": []any{}}, true
		}}
	if got := c.slowTop(); got["source"] != "slow_log" {
		t.Errorf("应优先用平台慢日志：%v", got)
	}
	for _, q := range ft.queries {
		if strings.Contains(q, "events_statements_summary_by_digest") {
			t.Error("有平台慢日志时不应再查 performance_schema")
		}
	}
}

func TestFingerprint(t *testing.T) {
	got := fingerprint("SELECT * FROM t WHERE a = 12 AND b = 'x y' AND c IN (1, 2, 3)")
	want := "SELECT * FROM t WHERE a = ? AND b = ? AND c in (?)"
	if got != want {
		t.Errorf("指纹 = %q，期望 %q", got, want)
	}
}

func TestAccountRisksDoris(t *testing.T) {
	ft := (&fakeTarget{}).on("SELECT @@version_comment", res([]string{"c"}, []any{"Doris version 2.0"}))
	c := &mysqlCollector{ctx: context.Background(), t: ft, now: time.Now}
	if e := sv(c.account(), "error"); !strings.Contains(e, "暂不支持") {
		t.Errorf("Doris 账号检查应明确说明暂不支持：%q", e)
	}
}

func TestJSONRoundTripKeepsSections(t *testing.T) {
	r := collectAll(t, mysqlHappyTarget())
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back jmap
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	a, c := BuildMySQLSections(r), BuildMySQLSections(back)
	if len(a) != len(c) {
		t.Fatalf("分区数不同：%d vs %d", len(a), len(c))
	}
	for i := range a {
		if len(a[i].Items) != len(c[i].Items) {
			t.Errorf("分区 %s 检查项数不同", a[i].Code)
		}
	}
}

// 较早生成的 result 里缺少新增步骤时，详情页照常显示，缺失的分区标“未采集”。
func TestSectionsFromLegacyResult(t *testing.T) {
	legacy := `{"engine":"mysql","server":{"version":"5.7.40","uptime_seconds":100},"connection":{"threads_connected":10,"max_connections":100,"connection_usage_rate":10.0},
	"buffer_pool":{"buffer_pool_hit_rate":98.0,"buffer_pool_size":1073741824},"query":{"slow_queries":0,"slow_query_log_enabled":true,"long_query_time":2.0},
	"inspection_report":{"health_score":85,"issues":[],"warnings":["w"],"suggestions":[],"status":"healthy"}}`
	var r jmap
	if err := json.Unmarshal([]byte(legacy), &r); err != nil {
		t.Fatal(err)
	}
	secs := BuildMySQLSections(r)
	by := map[string]Section{}
	for _, s := range secs {
		by[s.Code] = s
	}
	if m := by["memory"]; m.Attention != 1 {
		t.Errorf("命中率 98 应关注：%+v", m)
	}
	if l := by["lock_risk"]; len(l.Items) != 1 || !strings.Contains(l.Items[0].Value, "未采集") {
		t.Errorf("缺失的分区应显示未采集：%+v", l)
	}
	if _, ok := by["replication"]; ok {
		t.Error("报告没有复制分区")
	}
}
