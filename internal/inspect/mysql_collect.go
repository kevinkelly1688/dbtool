// @author: kevinkelly
// @license: Apache Licence
// @file: mysql_collect.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 以下是各检查项的阈值与展示条数。
const (
	longTrxAttentionSeconds   = 60
	longTrxAlertSeconds       = 300
	autoIncAttentionPct       = 70
	autoIncAlertPct           = 85
	autoIncShowPct            = 50
	noPKAlertRows             = 1000000
	deadlockDays              = 7
	deadlockAlertCount        = 10
	detailLimit               = 20
	gb                        = int64(1024 * 1024 * 1024)
	bigTableAttentionBytes    = 50 * gb
	bigTableAlertBytes        = 200 * gb
	bigTableRows              = 50000000
	fragmentMinFreeBytes      = 1 * gb
	fragmentRatioPct          = 30
	slowTopDays               = 7
	slowTopLimit              = 10
	slowTopAttentionAvgSecond = 1
	slowTopAlertAvgSecond     = 10
)

var (
	builtinAccounts       = map[string]bool{"mysql.sys": true, "mysql.session": true, "mysql.infoschema": true, "mysql.pxc.internal.session": true, "mysql.pxc.sst.role": true}
	cloudInternalAccounts = map[string]bool{"aurora": true, "polar_root": true, "replicator": true}
	localHosts            = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	systemSchemas         = []string{"information_schema", "mysql", "performance_schema", "sys", "__recycle_bin__", "__orca_in_polardb_m_", "__internal_schema"}
	systemSchemasSQL      = func() string {
		q := make([]string, len(systemSchemas))
		for i, s := range systemSchemas {
			q[i] = "'" + s + "'"
		}
		return strings.Join(q, ",")
	}()
	// 各整数类型的最大值：{有符号, 无符号}
	intMax = map[string][2]uint64{
		"tinyint": {127, 255}, "smallint": {32767, 65535}, "mediumint": {8388607, 16777215},
		"int": {2147483647, 4294967295}, "integer": {2147483647, 4294967295},
		"bigint": {9223372036854775807, 18446744073709551615},
	}
	// MySQL 8.0 默认缓存 information_schema 统计（最长 1 天），自增值等会不准，需要 SET SESSION information_schema_stats_expiry = 0。
	// 语句里仍带 SET_VAR 提示，但真库实测（8.0.46）该提示对 information_schema 统计不生效；真正起作用的是 sqlTarget.Query
	// 看到语句里有 information_schema_stats_expiry 时，先在这条连接上执行同样的会话变量设置（只影响该连接，巡检仍不改任何数据）。
	statsHint = "/*+ SET_VAR(information_schema_stats_expiry=0) MAX_EXECUTION_TIME(120000) */"
)

// mysqlStep 是一个采集步骤；与报告里的“分区”一一对应，供进度显示（N / 总数 项）。
type mysqlStep struct {
	Key    string
	Run    func(c *mysqlCollector) map[string]any
	Advice string // 一句处理建议：报告里该分区有关注 / 告警 / 未采集的项时显示在“建议”列
}

// mysqlSteps 是 MySQL 系的巡检步骤，按声明顺序依次采集。
var mysqlSteps = []mysqlStep{
	{"server", (*mysqlCollector).serverInfo, "确认版本仍在官方支持期内，已停止维护的版本请规划升级。"},
	{"connection", (*mysqlCollector).connection, "连接使用率高时先排查连接泄漏和空闲连接，再评估调大 max_connections 或引入连接池。"},
	{"buffer_pool", (*mysqlCollector).bufferPool, "命中率偏低说明热数据放不进缓冲池，评估调大 innodb_buffer_pool_size，并排查冲刷缓冲池的全表扫描语句。"},
	{"memory", (*mysqlCollector).memory, "sort_buffer_size、tmp_table_size 等按连接分配的内存不宜过大，避免高并发下把内存耗尽。"},
	{"io", (*mysqlCollector).io, "日志等待次数大于 0 说明 redo 日志缓冲或刷盘跟不上，评估调大 innodb_log_buffer_size 或提升磁盘性能。"},
	{"query", (*mysqlCollector).queryMetrics, "慢查询偏多时到「慢查日志」查看 Top 指纹并获取优化建议，同时确认慢日志已开启。"},
	{"transaction", (*mysqlCollector).transaction, "行锁等待多或 Undo 版本链长，说明有热点行或长事务，请缩短事务并统一加锁顺序。"},
	{"capacity", (*mysqlCollector).capacity, "关注数据总量的增长趋势，提前规划磁盘与归档，避免空间告急。"},
	{"lock_risk", (*mysqlCollector).lockRisk, "长事务和 MDL 等待会阻塞 DDL 与其他事务，请到「会话管理」定位并让应用及时提交，必要时终止阻塞源。"},
	{"auto_increment", (*mysqlCollector).autoIncrement, "自增 ID 使用率高的表要尽快改为 bigint unsigned 或分表，避免主键耗尽后写入失败。"},
	{"no_primary_key", (*mysqlCollector).noPrimaryKey, "无主键表会拖慢从库回放并影响在线改表，请补充自增主键，大表优先处理。"},
	{"deadlock", (*mysqlCollector).deadlock, "出现死锁时统一各业务的加锁顺序、缩短事务，并开启 innodb_print_all_deadlocks 便于排查。"},
	{"slow_top", (*mysqlCollector).slowTop, "优先优化总耗时最高的 Top 指纹（加索引或改写），可到「优化工具」获取建议。"},
	{"big_table", (*mysqlCollector).bigTable, "超大表和高碎片表请评估归档 / 分区，或在低峰期通过在线改表回收空间。"},
	{"index", (*mysqlCollector).indexes, "冗余索引占空间并拖慢写入，评估后删除被其他索引覆盖的索引。"},
	{"charset", (*mysqlCollector).charset, "字符集 / 排序规则混用会导致索引失效和乱码，建议统一为 utf8mb4。"},
	{"parameter", (*mysqlCollector).parameter, "关键参数偏离推荐值时，请在「参数配置」核对，并在维护窗口内调整。"},
	{"account", (*mysqlCollector).account, "清理无人使用、权限过大或允许任意主机连接的账号，高危账号请收敛权限。"},
	{"replication", (*mysqlCollector).replication, "复制延迟或线程异常时先看从库 IO / SQL 线程状态与网络，再排查大事务和从库负载。"},
}

// SlowTopSource 取平台采集的慢日志统计（metadata 库的 dbt_slow_logs 表）；没有数据返回 ok=false。
type SlowTopSource func(instanceID uint, since time.Time, limit int) (map[string]any, bool)

type mysqlCollector struct {
	ctx    context.Context
	t      Target
	instID uint
	slow   SlowTopSource
	now    func() time.Time
	doris  *bool
}

func errText(err error) string {
	s := err.Error()
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

func errMap(err error) map[string]any { return map[string]any{"error": errText(err)} }

func (c *mysqlCollector) q(query string) (*Result, error) { return c.t.Query(c.ctx, "", query) }

// kv 把 SHOW STATUS / SHOW VARIABLES 的结果（前两列）转成 map。
func kv(r *Result) map[string]string {
	out := map[string]string{}
	if r == nil {
		return out
	}
	for _, row := range r.Rows {
		if len(row) >= 2 {
			out[toStr(row[0])] = toStr(row[1])
		}
	}
	return out
}

func atoi(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

func round2(f float64) float64 { return float64(int64(f*100+0.5*sign(f))) / 100 }
func round3(f float64) float64 { return float64(int64(f*1000+0.5*sign(f))) / 1000 }
func sign(f float64) float64 {
	if f < 0 {
		return -1
	}
	return 1
}

// isDoris 判断是否 SelectDB/Doris（MySQL 协议兼容，但没有 InnoDB / performance_schema，账号体系也不同）。
func (c *mysqlCollector) isDoris() bool {
	if c.doris == nil {
		v := false
		if r, err := c.q("SELECT @@version_comment"); err == nil && len(r.Rows) > 0 {
			s := strings.ToLower(toStr(r.Rows[0][0]))
			v = strings.Contains(s, "selectdb") || strings.Contains(s, "doris")
		}
		c.doris = &v
	}
	return *c.doris
}

// ---- 一、基本信息、连接、内存、I/O、查询、事务、容量 ----

func (c *mysqlCollector) serverInfo() map[string]any {
	r, err := c.q("SELECT VERSION()")
	if err != nil {
		return errMap(err)
	}
	version := ""
	if len(r.Rows) > 0 {
		version = toStr(r.Rows[0][0])
	}
	// SelectDB/Doris 的 VERSION() 固定返回兼容版本号，真实版本在 version_comment 里
	if cr, err := c.q("SELECT @@version_comment"); err == nil && len(cr.Rows) > 0 {
		s := toStr(cr.Rows[0][0])
		if l := strings.ToLower(s); strings.Contains(l, "selectdb") || strings.Contains(l, "doris") {
			version = s
		}
	}
	var uptime int64
	if ur, err := c.q("SHOW STATUS WHERE variable_name = 'Uptime'"); err == nil { // 兼容库不支持时记 0
		uptime = atoi(kv(ur)["Uptime"])
	}
	return map[string]any{"version": version, "uptime_seconds": uptime}
}

func (c *mysqlCollector) connection() map[string]any {
	sr, err := c.q("SHOW STATUS WHERE variable_name IN ('Threads_connected','Threads_running','Threads_cached','Aborted_connections','Aborted_clients','Connections')")
	if err != nil {
		return errMap(err)
	}
	st := kv(sr)
	vr, err := c.q("SHOW VARIABLES WHERE variable_name = 'max_connections'")
	if err != nil {
		return errMap(err)
	}
	maxConn := atoi(kv(vr)["max_connections"])
	connected := atoi(st["Threads_connected"])
	usage := 0.0
	if maxConn > 0 {
		usage = float64(connected) / float64(maxConn) * 100
	}
	return map[string]any{
		"threads_connected": connected, "threads_running": atoi(st["Threads_running"]), "threads_cached": atoi(st["Threads_cached"]),
		"max_connections": maxConn, "connection_usage_rate": usage,
		"aborted_connections": atoi(st["Aborted_connections"]), "aborted_clients": atoi(st["Aborted_clients"]),
		"total_connections": atoi(st["Connections"]),
	}
}

func (c *mysqlCollector) bufferPool() map[string]any {
	vr, err := c.q("SHOW VARIABLES WHERE variable_name = 'innodb_buffer_pool_size'")
	if err != nil {
		return errMap(err)
	}
	size := atoi(kv(vr)["innodb_buffer_pool_size"])
	sr, err := c.q("SHOW STATUS WHERE variable_name LIKE 'Innodb_buffer_pool%'")
	if err != nil {
		return errMap(err)
	}
	st := kv(sr)
	reads, requests := atoi(st["Innodb_buffer_pool_reads"]), int64(1) // 没有该状态（非 InnoDB 的兼容库）时取 1，命中率记 100%
	if s, ok := st["Innodb_buffer_pool_read_requests"]; ok {
		requests = atoi(s)
	}
	hit := 0.0
	if requests > 0 {
		hit = float64(requests-reads) / float64(requests) * 100
	}
	total, free := atoi(st["Innodb_buffer_pool_pages_total"]), atoi(st["Innodb_buffer_pool_pages_free"])
	usage := 0.0
	if total > 0 {
		usage = float64(total-free) / float64(total) * 100
	}
	return map[string]any{
		"buffer_pool_size": size, "buffer_pool_pages_total": total, "buffer_pool_pages_free": free,
		"buffer_pool_pages_dirty": atoi(st["Innodb_buffer_pool_pages_dirty"]), "buffer_pool_read_requests": requests,
		"buffer_pool_reads": reads, "buffer_pool_hit_rate": round2(hit), "buffer_pool_usage_rate": round2(usage),
	}
}

func (c *mysqlCollector) memory() map[string]any {
	r, err := c.q("SHOW VARIABLES WHERE variable_name IN ('innodb_buffer_pool_size','innodb_log_buffer_size','sort_buffer_size','read_rnd_buffer_size','tmp_table_size','max_heap_table_size')")
	if err != nil {
		return errMap(err)
	}
	out := map[string]any{}
	for k, v := range kv(r) {
		out[k] = atoi(v)
	}
	return out
}

func (c *mysqlCollector) io() map[string]any {
	r, err := c.q("SHOW STATUS WHERE variable_name LIKE 'Innodb_data_%' OR variable_name LIKE 'Innodb_log_%'")
	if err != nil {
		return errMap(err)
	}
	st := kv(r)
	return map[string]any{
		"innodb_data_reads": atoi(st["Innodb_data_reads"]), "innodb_data_writes": atoi(st["Innodb_data_writes"]),
		"innodb_data_read": atoi(st["Innodb_data_read"]), "innodb_data_written": atoi(st["Innodb_data_written"]),
		"innodb_log_waits": atoi(st["Innodb_log_waits"]), "innodb_log_writes": atoi(st["Innodb_log_writes"]),
	}
}

func (c *mysqlCollector) queryMetrics() map[string]any {
	vr, err := c.q("SHOW VARIABLES WHERE variable_name IN ('slow_query_log','long_query_time')")
	if err != nil {
		return errMap(err)
	}
	cfg := kv(vr)
	sr, err := c.q("SHOW STATUS WHERE variable_name IN ('Slow_queries','Questions','Queries','Com_select','Com_insert','Com_update','Com_delete')")
	if err != nil {
		return errMap(err)
	}
	st := kv(sr)
	lqt := 2.0
	if v, err := strconv.ParseFloat(cfg["long_query_time"], 64); err == nil {
		lqt = v
	}
	return map[string]any{
		"slow_query_log_enabled": strings.EqualFold(cfg["slow_query_log"], "ON") || cfg["slow_query_log"] == "1", "long_query_time": lqt,
		"slow_queries": atoi(st["Slow_queries"]), "questions": atoi(st["Questions"]), "queries": atoi(st["Queries"]),
		"select_count": atoi(st["Com_select"]), "insert_count": atoi(st["Com_insert"]),
		"update_count": atoi(st["Com_update"]), "delete_count": atoi(st["Com_delete"]),
	}
}

func (c *mysqlCollector) transaction() map[string]any {
	r, err := c.q("SHOW STATUS WHERE variable_name LIKE 'Innodb_%lock%' OR variable_name = 'Innodb_trx_rseg_history_len'")
	if err != nil {
		return errMap(err)
	}
	st := kv(r)
	return map[string]any{
		"innodb_row_lock_waits": atoi(st["Innodb_row_lock_waits"]), "innodb_row_lock_time": atoi(st["Innodb_row_lock_time"]),
		"innodb_row_lock_time_avg": atoi(st["Innodb_row_lock_time_avg"]), "innodb_trx_rseg_history_len": atoi(st["Innodb_trx_rseg_history_len"]),
	}
}

func (c *mysqlCollector) capacity() map[string]any {
	const sys = "('information_schema','mysql','performance_schema','sys')"
	dbs, err := c.q("SELECT COUNT(*) FROM INFORMATION_SCHEMA.SCHEMATA")
	if err != nil {
		return errMap(err)
	}
	tables, err := c.q("SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA NOT IN " + sys)
	if err != nil {
		return errMap(err)
	}
	size, err := c.q("SELECT ROUND(SUM(DATA_LENGTH + INDEX_LENGTH) / 1024 / 1024, 2) FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA NOT IN " + sys)
	if err != nil {
		return errMap(err)
	}
	out := map[string]any{"database_count": int64(0), "table_count": int64(0), "total_data_size_mb": 0.0}
	if len(dbs.Rows) > 0 {
		out["database_count"] = toInt(dbs.Rows[0][0])
	}
	if len(tables.Rows) > 0 {
		out["table_count"] = toInt(tables.Rows[0][0])
	}
	if len(size.Rows) > 0 {
		out["total_data_size_mb"] = toFloat(size.Rows[0][0])
	}
	return out
}

// ---- 二、风险项：长事务 / MDL、自增 ID、无主键表、死锁 ----

func (c *mysqlCollector) lockRisk() map[string]any {
	out := map[string]any{}
	cnt, err := c.q(fmt.Sprintf("SELECT COUNT(*), IFNULL(MAX(TIMESTAMPDIFF(SECOND, trx_started, NOW())), 0) FROM information_schema.innodb_trx WHERE trx_started < NOW() - INTERVAL %d SECOND", longTrxAttentionSeconds))
	if err == nil {
		var list *Result
		list, err = c.q(fmt.Sprintf("SELECT t.trx_mysql_thread_id, TIMESTAMPDIFF(SECOND, t.trx_started, NOW()), t.trx_state, t.trx_rows_locked, t.trx_rows_modified, p.USER, p.HOST, p.DB, LEFT(IFNULL(t.trx_query, p.INFO), 200) "+
			"FROM information_schema.innodb_trx t LEFT JOIN information_schema.processlist p ON p.ID = t.trx_mysql_thread_id "+
			"WHERE t.trx_started < NOW() - INTERVAL %d SECOND ORDER BY t.trx_started LIMIT %d", longTrxAttentionSeconds, detailLimit))
		if err == nil {
			items := []any{}
			for _, r := range list.Rows {
				items = append(items, map[string]any{
					"thread_id": toInt(r[0]), "seconds": toInt(r[1]), "state": toStr(r[2]), "rows_locked": toInt(r[3]), "rows_modified": toInt(r[4]),
					"user": toStr(r[5]), "host": toStr(r[6]), "db": toStr(r[7]), "sql": toStr(r[8]),
				})
			}
			var count, maxSec int64
			if len(cnt.Rows) > 0 {
				count, maxSec = toInt(cnt.Rows[0][0]), toInt(cnt.Rows[0][1])
			}
			out["long_trx"] = map[string]any{"count": count, "max_seconds": maxSec, "items": items}
		}
	}
	if err != nil {
		out["long_trx"] = errMap(err)
	}

	mdl, err := c.q("SELECT ID, USER, HOST, DB, TIME, STATE, LEFT(INFO, 200) FROM information_schema.processlist WHERE STATE LIKE 'Waiting for%metadata lock%' ORDER BY TIME DESC LIMIT " + strconv.Itoa(detailLimit))
	if err != nil {
		out["mdl"] = errMap(err)
		return out
	}
	waits := []any{}
	index := map[int64]map[string]any{}
	for _, r := range mdl.Rows {
		w := map[string]any{"thread_id": toInt(r[0]), "user": toStr(r[1]), "host": toStr(r[2]), "db": toStr(r[3]), "seconds": toInt(r[4]), "state": toStr(r[5]), "sql": toStr(r[6]), "blocking_pid": nil}
		waits = append(waits, w)
		index[toInt(r[0])] = w
	}
	if len(waits) > 0 { // 阻塞者：sys.schema_table_lock_waits 依赖 performance_schema 的 MDL instrument，拿不到就只列等待者
		if br, err := c.q("SELECT waiting_pid, blocking_pid FROM sys.schema_table_lock_waits"); err == nil {
			for _, r := range br.Rows {
				if w, ok := index[toInt(r[0])]; ok && r[1] != nil {
					w["blocking_pid"] = toInt(r[1])
				}
			}
		}
	}
	out["mdl"] = map[string]any{"count": len(waits), "items": waits}
	return out
}

func (c *mysqlCollector) autoIncrement() map[string]any {
	r, err := c.q("SELECT " + statsHint + " t.TABLE_SCHEMA, t.TABLE_NAME, c.COLUMN_NAME, c.DATA_TYPE, c.COLUMN_TYPE, t.AUTO_INCREMENT " +
		"FROM information_schema.TABLES t JOIN information_schema.COLUMNS c ON c.TABLE_SCHEMA = t.TABLE_SCHEMA AND c.TABLE_NAME = t.TABLE_NAME AND c.EXTRA LIKE '%auto_increment%' " +
		"WHERE t.AUTO_INCREMENT IS NOT NULL AND t.TABLE_SCHEMA NOT IN (" + systemSchemasSQL + ")")
	if err != nil {
		return errMap(err)
	}
	type col struct {
		table, column, ctype string
		auto                 uint64
		max                  uint64
		pct                  float64
	}
	var cols []col
	for _, row := range r.Rows {
		limits, ok := intMax[strings.ToLower(toStr(row[3]))]
		if !ok || row[5] == nil {
			continue
		}
		max := limits[0]
		if strings.Contains(strings.ToLower(toStr(row[4])), "unsigned") {
			max = limits[1]
		}
		auto, _ := strconv.ParseUint(toStr(row[5]), 10, 64)
		if auto == 0 {
			continue
		}
		cols = append(cols, col{toStr(row[0]) + "." + toStr(row[1]), toStr(row[2]), toStr(row[4]), auto, max, round2(float64(auto-1) / float64(max) * 100)})
	}
	sort.SliceStable(cols, func(i, j int) bool { return cols[i].pct > cols[j].pct })
	out := map[string]any{"column_count": len(cols), "max_usage_pct": 0.0, "alert_count": 0, "attention_count": 0}
	if len(cols) > 0 {
		out["max_usage_pct"] = cols[0].pct
	}
	alert, attention := 0, 0
	items := []any{}
	for _, x := range cols {
		switch {
		case x.pct >= autoIncAlertPct:
			alert++
		case x.pct >= autoIncAttentionPct:
			attention++
		}
		if x.pct >= autoIncShowPct && len(items) < detailLimit {
			items = append(items, map[string]any{"table": x.table, "column": x.column, "column_type": x.ctype, "auto_increment": x.auto, "max_value": x.max, "usage_pct": x.pct})
		}
	}
	out["alert_count"], out["attention_count"], out["items"] = alert, attention, items
	return out
}

func (c *mysqlCollector) noPrimaryKey() map[string]any {
	// 只看 InnoDB 表：主键缺失的影响（复制回放、聚簇索引）只针对 InnoDB；SelectDB/Doris 等表用 UNIQUE/DUPLICATE KEY 模型，本就没有主键约束
	r, err := c.q("SELECT t.TABLE_SCHEMA, t.TABLE_NAME, t.ENGINE, IFNULL(t.TABLE_ROWS, 0) FROM information_schema.TABLES t " +
		"LEFT JOIN information_schema.TABLE_CONSTRAINTS c ON c.TABLE_SCHEMA = t.TABLE_SCHEMA AND c.TABLE_NAME = t.TABLE_NAME AND c.CONSTRAINT_TYPE = 'PRIMARY KEY' " +
		"WHERE t.TABLE_TYPE = 'BASE TABLE' AND t.ENGINE = 'InnoDB' AND t.TABLE_SCHEMA NOT IN (" + systemSchemasSQL + ") AND c.CONSTRAINT_NAME IS NULL ORDER BY t.TABLE_ROWS DESC")
	if err != nil {
		return errMap(err)
	}
	items := []any{}
	large := 0
	for i, row := range r.Rows {
		rows := toInt(row[3])
		if rows >= noPKAlertRows {
			large++
		}
		if i < detailLimit {
			items = append(items, map[string]any{"table": toStr(row[0]) + "." + toStr(row[1]), "engine": toStr(row[2]), "rows": rows})
		}
	}
	return map[string]any{"count": len(r.Rows), "large_count": large, "items": items}
}

var deadlockRe = regexp.MustCompile(`(?s)LATEST DETECTED DEADLOCK\s*\n-+\n(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})(.*?)\n-{5,}\n[A-Z]`)
var deadlockTableRe = regexp.MustCompile("of table (`[^`]+`\\.`[^`]+`)")

func (c *mysqlCollector) deadlock() map[string]any {
	nr, err := c.q("SELECT NOW()")
	if err != nil {
		return errMap(err)
	}
	serverNow := c.now()
	if len(nr.Rows) > 0 {
		if t := toTime(nr.Rows[0][0]); !t.IsZero() {
			serverNow = t
		}
	}
	out := map[string]any{"days": deadlockDays, "count": nil, "source": "", "events": []any{}, "print_all_deadlocks": nil, "counter_since_startup": nil, "latest": nil}
	if r, err := c.q("SHOW VARIABLES LIKE 'innodb_print_all_deadlocks'"); err == nil && len(r.Rows) > 0 { // 没有该变量（非 InnoDB 的兼容库）记为 null，不提示开启
		out["print_all_deadlocks"] = strings.EqualFold(toStr(r.Rows[0][1]), "ON")
	}
	if r, err := c.q("SELECT COUNT FROM information_schema.INNODB_METRICS WHERE NAME = 'lock_deadlocks'"); err == nil && len(r.Rows) > 0 {
		out["counter_since_startup"] = toInt(r.Rows[0][0])
	}
	latestWithin := false
	if r, err := c.q("SHOW ENGINE INNODB STATUS"); err == nil && len(r.Rows) > 0 && len(r.Rows[0]) > 2 {
		if m := deadlockRe.FindStringSubmatch(toStr(r.Rows[0][2])); m != nil {
			lt, _ := time.ParseInLocation("2006-01-02 15:04:05", m[1], time.Local)
			set := map[string]bool{}
			for _, t := range deadlockTableRe.FindAllStringSubmatch(m[2], -1) {
				set[t[1]] = true
			}
			tables := make([]string, 0, len(set))
			for t := range set {
				tables = append(tables, t)
			}
			sort.Strings(tables)
			latestWithin = serverNow.Sub(lt) <= deadlockDays*24*time.Hour
			out["latest"] = map[string]any{"time": m[1], "tables": tables, "within_days": latestWithin}
		}
	}
	// 7 天内全部死锁：错误日志里每次死锁有一条 "Transactions deadlock detected"（8.0.22+）
	if r, err := c.q(fmt.Sprintf("SELECT LOGGED FROM performance_schema.error_log WHERE LOGGED >= NOW() - INTERVAL %d DAY AND DATA LIKE '%%deadlock detected%%' ORDER BY LOGGED DESC", deadlockDays)); err == nil {
		if len(r.Rows) > 0 || out["print_all_deadlocks"] == true {
			events := []any{}
			for i, row := range r.Rows {
				if i < detailLimit {
					s := toStr(row[0])
					if len(s) > 19 {
						s = s[:19]
					}
					events = append(events, s)
				}
			}
			out["count"], out["source"], out["events"] = len(r.Rows), "error_log", events
		}
	}
	if out["count"] == nil && out["latest"] != nil { // 只能确认最近一次死锁是否在 7 天内，实际次数可能更多
		out["count"], out["source"] = 0, "latest_only"
		if latestWithin {
			out["count"] = 1
		}
	}
	return out
}

// ---- 三、慢查询 Top ----

var (
	reQuoted = regexp.MustCompile(`'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"`)
	reNumber = regexp.MustCompile(`\b\d+\.?\d*\b`)
	reInList = regexp.MustCompile(`(?i)\bin\s*\(\s*\?(?:\s*,\s*\?)*\s*\)`)
	reSpace  = regexp.MustCompile(`\s+`)
)

// fingerprint 把字面值替换成 ?，让只差字面值的 SQL 得到同一指纹（简化版，只用于展示）。
func fingerprint(s string) string {
	s = reQuoted.ReplaceAllString(s, "?")
	s = reNumber.ReplaceAllString(s, "?")
	s = reInList.ReplaceAllString(s, "in (?)")
	return strings.TrimSpace(reSpace.ReplaceAllString(s, " "))
}

func (c *mysqlCollector) slowTop() map[string]any {
	if c.slow != nil { // 优先用平台采集的慢日志
		if m, ok := c.slow(c.instID, c.now().Add(-slowTopDays*24*time.Hour), slowTopLimit); ok {
			return m
		}
	}
	lqt := 1.0
	if r, err := c.q("SHOW VARIABLES LIKE 'long_query_time'"); err == nil && len(r.Rows) > 0 {
		if v, err := strconv.ParseFloat(toStr(r.Rows[0][1]), 64); err == nil {
			lqt = v
		}
	}
	r, err := c.q(fmt.Sprintf("SELECT SCHEMA_NAME, LEFT(DIGEST_TEXT, 300), COUNT_STAR, SUM_TIMER_WAIT / 1e12, AVG_TIMER_WAIT / 1e12, MAX_TIMER_WAIT / 1e12, SUM_ROWS_EXAMINED / GREATEST(COUNT_STAR, 1), LAST_SEEN "+
		"FROM performance_schema.events_statements_summary_by_digest WHERE DIGEST_TEXT IS NOT NULL AND IFNULL(SCHEMA_NAME, '') NOT IN (%s) AND MAX_TIMER_WAIT >= %v * 1e12 ORDER BY SUM_TIMER_WAIT DESC LIMIT %d", systemSchemasSQL, lqt, slowTopLimit))
	if err != nil { // SelectDB/ADB 等没有 performance_schema，只能靠平台慢日志采集
		return map[string]any{"source": "none", "reason": errText(err), "items": []any{}}
	}
	items := []any{}
	for _, row := range r.Rows {
		last := toStr(row[7])
		if len(last) > 19 {
			last = last[:19]
		}
		items = append(items, map[string]any{
			"db": toStr(row[0]), "sql": toStr(row[1]), "count": toInt(row[2]), "total_seconds": round2(toFloat(row[3])),
			"avg_seconds": round3(toFloat(row[4])), "max_seconds": round3(toFloat(row[5])), "avg_rows_examined": toInt(row[6]), "last_seen": last,
		})
	}
	return map[string]any{"source": "digest", "long_query_time": lqt, "items": items}
}

// ---- 四、结构与配置：大表/碎片、索引、字符集、参数基线、高危账号 ----

func (c *mysqlCollector) bigTable() map[string]any {
	r, err := c.q("SELECT /*+ MAX_EXECUTION_TIME(120000) */ TABLE_SCHEMA, TABLE_NAME, ENGINE, IFNULL(TABLE_ROWS, 0), IFNULL(DATA_LENGTH, 0), IFNULL(INDEX_LENGTH, 0), IFNULL(DATA_FREE, 0) " +
		"FROM information_schema.TABLES WHERE TABLE_TYPE = 'BASE TABLE' AND TABLE_SCHEMA NOT IN (" + systemSchemasSQL + ")")
	if err != nil {
		return errMap(err)
	}
	type tb struct {
		name, engine                  string
		rows, data, index, free, size int64
		pct                           float64
	}
	var tables []tb
	var total int64
	for _, row := range r.Rows {
		t := tb{name: toStr(row[0]) + "." + toStr(row[1]), engine: toStr(row[2]), rows: toInt(row[3]), data: toInt(row[4]), index: toInt(row[5]), free: toInt(row[6])}
		t.size = t.data + t.index
		if all := t.size + t.free; all > 0 {
			t.pct = round2(float64(t.free) / float64(all) * 100)
		}
		total += t.size
		tables = append(tables, t)
	}
	toMap := func(t tb) map[string]any {
		return map[string]any{"table": t.name, "engine": t.engine, "rows": t.rows, "data_bytes": t.data, "index_bytes": t.index, "size_bytes": t.size, "free_bytes": t.free, "fragment_pct": t.pct}
	}
	var big, frag []tb
	for _, t := range tables {
		if t.size >= bigTableAttentionBytes || t.rows >= bigTableRows {
			big = append(big, t)
		}
		if t.free >= fragmentMinFreeBytes && t.pct >= fragmentRatioPct {
			frag = append(frag, t)
		}
	}
	sort.SliceStable(big, func(i, j int) bool { return big[i].size > big[j].size })
	sort.SliceStable(frag, func(i, j int) bool { return frag[i].free > frag[j].free })
	sort.SliceStable(tables, func(i, j int) bool { return tables[i].size > tables[j].size })
	limit := func(l []tb, n int) []any {
		out := []any{}
		for i, t := range l {
			if i >= n {
				break
			}
			out = append(out, toMap(t))
		}
		return out
	}
	alertCount := 0
	for _, t := range big {
		if t.size >= bigTableAlertBytes {
			alertCount++
		}
	}
	var fragFree int64
	for _, t := range frag {
		fragFree += t.free
	}
	return map[string]any{
		"table_count": len(tables), "total_size_bytes": total, "big_count": len(big), "big_alert_count": alertCount,
		"big_items": limit(big, detailLimit), "largest": limit(tables, 5), "fragment_count": len(frag), "fragment_free_bytes": fragFree, "fragment_items": limit(frag, detailLimit),
	}
}

func (c *mysqlCollector) indexes() map[string]any {
	out := map[string]any{}
	// 冗余索引：直接按 STATISTICS 计算，不依赖 sys schema
	r, err := c.q("SELECT /*+ MAX_EXECUTION_TIME(120000) */ s.TABLE_SCHEMA, s.TABLE_NAME, s.INDEX_NAME, s.NON_UNIQUE, s.SEQ_IN_INDEX, s.COLUMN_NAME, IFNULL(s.SUB_PART, 0), s.INDEX_TYPE " +
		"FROM information_schema.STATISTICS s JOIN information_schema.TABLES t ON t.TABLE_SCHEMA = s.TABLE_SCHEMA AND t.TABLE_NAME = s.TABLE_NAME AND t.ENGINE = 'InnoDB' " +
		"WHERE s.TABLE_SCHEMA NOT IN (" + systemSchemasSQL + ") ORDER BY s.TABLE_SCHEMA, s.TABLE_NAME, s.INDEX_NAME, s.SEQ_IN_INDEX")
	if err != nil {
		out["redundant"] = errMap(err)
	} else {
		out["redundant"] = redundantIndexes(r)
	}
	// 未使用索引：本节点自启动以来从未被读过、且所在表有访问的非唯一索引
	if r, err := c.q("SHOW VARIABLES LIKE 'performance_schema'"); err == nil {
		if len(r.Rows) == 0 || !(strings.EqualFold(toStr(r.Rows[0][1]), "ON") || toStr(r.Rows[0][1]) == "1") {
			out["unused"] = map[string]any{"disabled": true}
			return out
		}
	}
	var uptime int64
	if r, err := c.q("SHOW GLOBAL STATUS LIKE 'Uptime'"); err == nil && len(r.Rows) > 0 {
		uptime = toInt(r.Rows[0][1])
	}
	ur, err := c.q("SELECT /*+ MAX_EXECUTION_TIME(120000) */ u.OBJECT_SCHEMA, u.OBJECT_NAME, u.INDEX_NAME, u.COUNT_STAR FROM performance_schema.table_io_waits_summary_by_index_usage u WHERE u.OBJECT_SCHEMA NOT IN (" + systemSchemasSQL + ")")
	if err != nil {
		out["unused"] = errMap(err)
		return out
	}
	nr, err := c.q("SELECT DISTINCT TABLE_SCHEMA, TABLE_NAME, INDEX_NAME FROM information_schema.STATISTICS WHERE NON_UNIQUE = 1 AND TABLE_SCHEMA NOT IN (" + systemSchemasSQL + ")")
	if err != nil {
		out["unused"] = errMap(err)
		return out
	}
	active := map[string]bool{}
	for _, row := range ur.Rows {
		if toInt(row[3]) > 0 {
			active[toStr(row[0])+"."+toStr(row[1])] = true
		}
	}
	nonUnique := map[string]bool{}
	for _, row := range nr.Rows {
		nonUnique[toStr(row[0])+"."+toStr(row[1])+"."+toStr(row[2])] = true
	}
	var unused []string
	for _, row := range ur.Rows {
		tbl, idx := toStr(row[0])+"."+toStr(row[1]), toStr(row[2])
		if idx != "" && idx != "PRIMARY" && row[3] != nil && toInt(row[3]) == 0 && active[tbl] && nonUnique[tbl+"."+idx] {
			unused = append(unused, tbl+"."+idx)
		}
	}
	sort.Strings(unused)
	items := []any{}
	for i, s := range unused {
		if i < detailLimit {
			items = append(items, s)
		}
	}
	out["unused"] = map[string]any{"count": len(unused), "uptime_seconds": uptime, "items": items}
	return out
}

type idxDef struct {
	unique bool
	typ    string
	cols   []string
}

// redundantIndexes 找出列是另一个索引的前缀（或与之完全相同）的 BTREE 索引。
func redundantIndexes(r *Result) map[string]any {
	type tkey struct{ schema, table string }
	tables := map[tkey]map[string]*idxDef{}
	var order []tkey
	for _, row := range r.Rows {
		k := tkey{toStr(row[0]), toStr(row[1])}
		m := tables[k]
		if m == nil {
			m = map[string]*idxDef{}
			tables[k] = m
			order = append(order, k)
		}
		name := toStr(row[2])
		d := m[name]
		if d == nil {
			d = &idxDef{unique: toInt(row[3]) == 0, typ: toStr(row[7])}
			m[name] = d
		}
		col := toStr(row[5])
		if sub := toInt(row[6]); sub > 0 {
			col = fmt.Sprintf("%s(%d)", col, sub)
		}
		d.cols = append(d.cols, col)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].schema != order[j].schema {
			return order[i].schema < order[j].schema
		}
		return order[i].table < order[j].table
	})
	items := []any{}
	count := 0
	for _, k := range order {
		m := tables[k]
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			idx := m[name]
			if name == "PRIMARY" || idx.typ != "BTREE" {
				continue
			}
			for _, other := range names {
				o := m[other]
				if other == name || o.typ != "BTREE" || !isPrefix(idx.cols, o.cols) {
					continue
				}
				if len(idx.cols) == len(o.cols) {
					// 完全相同：保留主键/唯一索引，两个同类时保留名字靠前的那个
					if idx.unique && !o.unique {
						continue
					}
					if idx.unique == o.unique && other != "PRIMARY" && name < other {
						continue
					}
				} else if idx.unique {
					continue // 唯一索引承担约束，即便是别的索引前缀也不能删
				}
				count++
				if len(items) < detailLimit {
					items = append(items, map[string]any{
						"table": k.schema + "." + k.table, "index": name, "columns": strings.Join(idx.cols, ","),
						"dominant_index": other, "dominant_columns": strings.Join(o.cols, ","),
						"sql": fmt.Sprintf("ALTER TABLE `%s`.`%s` DROP INDEX `%s`;", k.schema, k.table, name),
					})
				}
				break
			}
		}
	}
	return map[string]any{"count": count, "items": items}
}

func isPrefix(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (c *mysqlCollector) charset() map[string]any {
	vr, err := c.q("SHOW VARIABLES WHERE Variable_name IN ('character_set_server', 'collation_server')")
	if err != nil {
		return errMap(err)
	}
	vars := kv(vr)
	tr, err := c.q("SELECT /*+ MAX_EXECUTION_TIME(120000) */ TABLE_SCHEMA, TABLE_NAME, TABLE_COLLATION FROM information_schema.TABLES WHERE TABLE_TYPE = 'BASE TABLE' AND TABLE_SCHEMA NOT IN (" + systemSchemasSQL + ")")
	if err != nil {
		return errMap(err)
	}
	type tb struct{ name, collation, charset string }
	var tables []tb
	tableCharset := map[string]string{}
	for _, row := range tr.Rows {
		coll := toStr(row[2])
		t := tb{toStr(row[0]) + "." + toStr(row[1]), coll, strings.SplitN(coll, "_", 2)[0]} // 排序规则名以字符集开头（utf8mb4_0900_ai_ci -> utf8mb4）
		tables = append(tables, t)
		tableCharset[t.name] = t.charset
	}
	cr, err := c.q("SELECT /*+ MAX_EXECUTION_TIME(120000) */ TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, CHARACTER_SET_NAME, COLLATION_NAME FROM information_schema.COLUMNS WHERE CHARACTER_SET_NAME IS NOT NULL AND TABLE_SCHEMA NOT IN (" + systemSchemasSQL + ")")
	if err != nil {
		return errMap(err)
	}
	csDist, collDist := map[string]any{}, map[string]any{}
	collCount := map[string]int{}
	for _, t := range tables {
		if t.charset != "" {
			csDist[t.charset] = toInt(csDist[t.charset]) + 1
		}
		if t.collation != "" {
			collDist[t.collation] = toInt(collDist[t.collation]) + 1
			collCount[t.collation]++
		}
	}
	main := ""
	for k, n := range collCount { // 最多的排序规则（并列时取名字靠前的，保证结果稳定）
		if n > collCount[main] || (n == collCount[main] && (main == "" || k < main)) {
			main = k
		}
	}
	other := []any{}
	for _, t := range tables {
		if t.collation != "" && t.collation != main && len(other) < detailLimit {
			other = append(other, map[string]any{"table": t.name, "collation": t.collation})
		}
	}
	mismatch := []any{}
	mismatchCount, utf8mb3Cols := 0, 0
	for _, row := range cr.Rows {
		tbl := toStr(row[0]) + "." + toStr(row[1])
		cs := toStr(row[3])
		if cs == "utf8" || cs == "utf8mb3" {
			utf8mb3Cols++
		}
		if tc := tableCharset[tbl]; tc != "" && cs != tc {
			mismatchCount++
			if len(mismatch) < detailLimit {
				mismatch = append(mismatch, map[string]any{"column": tbl + "." + toStr(row[2]), "charset": cs, "collation": toStr(row[4]), "table_charset": tc})
			}
		}
	}
	mb3 := []any{}
	mb3Count := 0
	for _, t := range tables {
		if t.charset == "utf8" || t.charset == "utf8mb3" {
			mb3Count++
			if len(mb3) < detailLimit {
				mb3 = append(mb3, t.name)
			}
		}
	}
	return map[string]any{
		"character_set_server": vars["character_set_server"], "collation_server": vars["collation_server"],
		"table_charsets": csDist, "table_collations": collDist, "main_collation": main, "other_collation_tables": other,
		"column_mismatch_count": mismatchCount, "column_mismatch": mismatch, "utf8mb3_table_count": mb3Count,
		"utf8mb3_column_count": utf8mb3Cols, "utf8mb3_tables": mb3,
	}
}

func (c *mysqlCollector) parameter() map[string]any {
	r, err := c.q("SHOW GLOBAL VARIABLES")
	if err != nil {
		return errMap(err)
	}
	v := map[string]string{}
	for k, val := range kv(r) {
		v[strings.ToLower(k)] = val
	}
	on := func(name string) bool {
		s := strings.ToUpper(v[name])
		return s == "ON" || s == "1" || s == "TRUE"
	}
	if c.isDoris() { // SelectDB/Doris 没有 performance_schema、sql_mode 严格模式等 MySQL 语义，按不适用处理
		delete(v, "performance_schema")
		delete(v, "sql_mode")
	}
	logBin := on("log_bin") || on("polar_log_bin") || on("loose_polar_log_bin")
	items := []any{}
	check := func(name, expected string, ok bool, level int, reason string, actual *string) {
		act, has := v[name]
		if actual != nil {
			act, has = *actual, true
		}
		if !has {
			items = append(items, map[string]any{"name": name, "actual": "-", "expected": expected, "level": LevelNormal, "status": "不适用", "reason": "实例未提供该参数"})
			return
		}
		lv, status := LevelNormal, "符合"
		if !ok {
			lv, status = level, "不符合"
		}
		items = append(items, map[string]any{"name": name, "actual": act, "expected": expected, "level": lv, "status": status, "reason": reason})
	}
	check("innodb_flush_log_at_trx_commit", "1", v["innodb_flush_log_at_trx_commit"] == "1", LevelAlert, "非 1 时宕机可能丢失已提交事务", nil)
	if logBin {
		check("sync_binlog", "1", v["sync_binlog"] == "1", LevelAlert, "非 1 时宕机可能丢失 binlog，主从不一致", nil)
		check("binlog_format", "ROW", strings.EqualFold(v["binlog_format"], "ROW"), LevelAttention, "STATEMENT/MIXED 格式有主从不一致风险，也不利于数据订阅", nil)
		check("binlog_row_image", "FULL", strings.EqualFold(v["binlog_row_image"], "FULL"), LevelAttention, "MINIMAL 会影响闪回和部分数据同步工具", nil)
	}
	isolation, hasIso := v["transaction_isolation"]
	if !hasIso {
		isolation, hasIso = v["tx_isolation"]
	}
	if hasIso {
		iso := strings.ToUpper(isolation)
		check("transaction_isolation", "READ-COMMITTED 或 REPEATABLE-READ", iso == "READ-COMMITTED" || iso == "REPEATABLE-READ", LevelAttention, "READ-UNCOMMITTED 有脏读，SERIALIZABLE 锁冲突严重", &isolation)
	} else {
		check("transaction_isolation", "READ-COMMITTED 或 REPEATABLE-READ", false, LevelAttention, "", nil)
	}
	check("slow_query_log", "ON", on("slow_query_log"), LevelAttention, "关闭后无法定位慢 SQL", nil)
	lqt, perr := strconv.ParseFloat(v["long_query_time"], 64)
	if _, ok := v["long_query_time"]; !ok {
		lqt, perr = 10, nil
	}
	check("long_query_time", "<= 2 秒", perr == nil && lqt <= 2, LevelAttention, "阈值过大会漏掉慢 SQL", nil)
	check("innodb_print_all_deadlocks", "ON", on("innodb_print_all_deadlocks"), LevelAttention, "关闭后只能看到最近一次死锁", nil)
	check("performance_schema", "ON", on("performance_schema"), LevelAttention, "关闭后无法做 SQL 统计、锁分析、未使用索引分析", nil)
	check("sql_mode", "包含 STRICT_TRANS_TABLES", strings.Contains(strings.ToUpper(v["sql_mode"]), "STRICT_TRANS_TABLES"), LevelAttention, "非严格模式下超长/非法数据会被静默截断", nil)
	check("character_set_server", "utf8mb4", v["character_set_server"] == "utf8mb4", LevelAttention, "utf8(mb3) 无法存储 emoji 等 4 字节字符", nil)
	lockWaitOK := true
	if s, ok := v["innodb_lock_wait_timeout"]; ok {
		if n, err := strconv.Atoi(s); err == nil {
			lockWaitOK = n <= 120
		}
	} else {
		lockWaitOK = true
	}
	check("innodb_lock_wait_timeout", "<= 120 秒", lockWaitOK, LevelAttention, "过大时锁等待会长时间堆积连接", nil)
	check("local_infile", "OFF", !on("local_infile"), LevelAttention, "开启后客户端可读取服务器文件，有安全风险", nil)
	check("innodb_file_per_table", "ON", on("innodb_file_per_table"), LevelAttention, "关闭后表空间无法单独回收", nil)
	for _, name := range []string{"max_connections", "wait_timeout", "time_zone", "lower_case_table_names", "max_allowed_packet"} { // 仅展示，不判定
		if val, ok := v[name]; ok {
			items = append(items, map[string]any{"name": name, "actual": val, "expected": "-", "level": LevelNormal, "status": "参考", "reason": ""})
		}
	}
	alert, attention := 0, 0
	for _, it := range items {
		switch it.(map[string]any)["level"] {
		case LevelAlert:
			alert++
		case LevelAttention:
			attention++
		}
	}
	return map[string]any{"log_bin": logBin, "items": items, "alert_count": alert, "attention_count": attention}
}

func (c *mysqlCollector) account() map[string]any {
	if c.isDoris() {
		return map[string]any{"error": "SelectDB/Doris 的账号风险检查暂不支持"}
	}
	r, err := c.q("SELECT * FROM mysql.user")
	if err != nil {
		return errMap(err) // 需要 mysql.user 查询权限
	}
	users := r.Maps()
	yes := func(u Row, col string) bool { return strings.EqualFold(u.Str(col), "Y") }
	highPrivs := [][2]string{{"super_priv", "SUPER"}, {"grant_priv", "GRANT"}, {"file_priv", "FILE"}, {"shutdown_priv", "SHUTDOWN"}, {"create_user_priv", "CREATE USER"}, {"process_priv", "PROCESS"}, {"reload_priv", "RELOAD"}}
	var risks []map[string]any
	skipped := 0
	for _, u := range users {
		user, host := u.Str("user"), u.Str("host")
		if builtinAccounts[user] || yes(u, "account_locked") {
			continue
		}
		if cloudInternalAccounts[user] {
			skipped++
			continue
		}
		account := fmt.Sprintf("'%s'@'%s'", user, host)
		password := u.Str("authentication_string")
		if _, ok := u["authentication_string"]; !ok {
			password = u.Str("password")
		}
		plugin := u.Str("plugin")
		var privs []string
		for _, p := range highPrivs {
			if yes(u, p[0]) {
				privs = append(privs, p[1])
			}
		}
		has := func(p string) bool {
			for _, x := range privs {
				if x == p {
					return true
				}
			}
			return false
		}
		if user == "" {
			risks = append(risks, map[string]any{"account": account, "risk": "匿名账号（用户名为空）", "level": LevelAlert})
		}
		if password == "" && plugin != "auth_socket" && plugin != "unix_socket" {
			if localHosts[host] { // 只能在数据库主机本机登录，风险低于远程空密码
				risks = append(risks, map[string]any{"account": account, "risk": "空密码（仅限本机登录）", "level": LevelAttention})
			} else {
				risks = append(risks, map[string]any{"account": account, "risk": "空密码", "level": LevelAlert})
			}
		}
		if host == "%" && (has("SUPER") || has("GRANT") || has("FILE")) {
			risks = append(risks, map[string]any{"account": account, "risk": fmt.Sprintf("任意主机(%%)可登录且有高危权限：%s", strings.Join(privs, ",")), "level": LevelAttention})
		} else if has("FILE") {
			risks = append(risks, map[string]any{"account": account, "risk": "拥有 FILE 权限（可读写服务器文件）", "level": LevelAttention})
		}
	}
	alert := 0
	items := []any{}
	for i, rk := range risks {
		if rk["level"] == LevelAlert {
			alert++
		}
		if i < detailLimit*2 {
			items = append(items, rk)
		}
	}
	return map[string]any{"account_count": len(users), "skipped_internal": skipped, "risk_count": len(risks), "alert_count": alert, "items": items}
}

// ---- 五、复制与 binlog ----

const (
	replLagAttentionSeconds = 60
	replLagAlertSeconds     = 300
	binlogAttentionBytes    = 100 * gb
)

func (c *mysqlCollector) replication() map[string]any {
	out := map[string]any{"is_replica": false}
	r, err := c.q("SHOW REPLICA STATUS")
	if err != nil {
		r, err = c.q("SHOW SLAVE STATUS") // 8.0.22 之前
	}
	if err != nil {
		out["replica"] = errMap(err) // 缺少 REPLICATION CLIENT 权限，或兼容库不支持
	} else if rows := r.Maps(); len(rows) > 0 {
		row := rows[0]
		pick := func(a, b string) string {
			if _, ok := row[a]; ok {
				return row.Str(a)
			}
			return row.Str(b)
		}
		lag := int64(-1) // -1：复制线程没跑，延迟未知
		if v := pick("seconds_behind_source", "seconds_behind_master"); v != "" && !strings.EqualFold(v, "NULL") {
			lag = atoi(v)
		}
		out["is_replica"] = true
		out["replica"] = map[string]any{
			"io_running": pick("replica_io_running", "slave_io_running"), "sql_running": pick("replica_sql_running", "slave_sql_running"),
			"seconds_behind": lag, "last_io_error": pick("last_io_error", "last_io_error"), "last_sql_error": pick("last_sql_error", "last_sql_error"),
			"source_host": pick("source_host", "master_host"),
		}
	}
	// binlog：文件数与总大小（SHOW BINARY LOGS 需要 REPLICATION CLIENT 或 SUPER）
	if br, err := c.q("SHOW BINARY LOGS"); err == nil {
		var total int64
		for _, row := range br.Rows {
			if len(row) > 1 {
				total += toInt(row[1])
			}
		}
		out["binlog"] = map[string]any{"count": len(br.Rows), "total_bytes": total}
	} else {
		out["binlog"] = map[string]any{"disabled": true, "reason": errText(err)} // 没开 binlog 或没有权限
	}
	return out
}
