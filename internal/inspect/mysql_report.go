// @author: kevinkelly
// @license: Apache Licence
// @file: mysql_report.go
// @time: 2026/10/28

package inspect

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Item 是报告里的一行检查项（两种报告统一的展示结构）。
type Item struct {
	Name      string       `json:"name"`
	Value     string       `json:"value"`
	Threshold string       `json:"threshold"`
	Level     int          `json:"level"`
	DB        string       `json:"db"`
	Detail    bool         `json:"detail"`          // 明细行：只展示，不计入分区的告警/关注数
	Table     *DetailTable `json:"table,omitempty"` // SQL Server 检查项的明细表
	Key       string       `json:"key,omitempty"`
	Advice    string       `json:"advice,omitempty"` // 处理建议（报告详情的“建议”列；关注 / 告警 / 未采集时显示）
}

// DetailTable 是检查项的明细表（JSON：{"columns": [...], "rows": [[...]]}）。
type DetailTable struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// Section 是报告的一个分区。
type Section struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Items     []Item `json:"items"`
	Alert     int    `json:"alert"`
	Attention int    `json:"attention"`
	Normal    int    `json:"normal"`
	Failed    int    `json:"failed"`
	Expanded  bool   `json:"expanded"` // 有告警/关注/未采集的分区默认展开
}

// finalize 统计各分区的告警/关注/未采集数（明细行不计）。
func finalize(secs []Section) []Section {
	for i := range secs {
		s := &secs[i]
		s.Alert, s.Attention, s.Normal, s.Failed = 0, 0, 0, 0
		for _, it := range s.Items {
			if it.Detail {
				continue
			}
			switch it.Level {
			case LevelAlert:
				s.Alert++
			case LevelAttention:
				s.Attention++
			case LevelFailed:
				s.Failed++
			default:
				s.Normal++
			}
		}
		s.Expanded = s.Alert+s.Attention+s.Failed > 0
	}
	return secs
}

// ---- 取值辅助：数据来自 JSON，数字一律是 float64，缺失按零值 ----

type jmap = map[string]any

func sub(m jmap, k string) jmap {
	if v, ok := m[k].(map[string]any); ok {
		return v
	}
	return jmap{}
}

func jrows(m jmap, k string) []jmap {
	var out []jmap
	switch v := m[k].(type) {
	case []any:
		for _, x := range v {
			if mm, ok := x.(map[string]any); ok {
				out = append(out, mm)
			}
		}
	case []map[string]any:
		out = v
	}
	return out
}

func strs(m jmap, k string) []string {
	var out []string
	if v, ok := m[k].([]any); ok {
		for _, x := range v {
			out = append(out, toStr(x))
		}
	}
	return out
}

func f64(m jmap, k string) float64 { return toFloat(m[k]) }
func i64(m jmap, k string) int64   { return toInt(m[k]) }
func sv(m jmap, k string) string   { return toStr(m[k]) }
func has(m jmap, k string) bool {
	v, ok := m[k]
	return ok && v != nil
}
func isTrue(m jmap, k string) bool { b, _ := m[k].(bool); return b }

func numOr(m jmap, k string, def float64) float64 {
	if !has(m, k) {
		return def
	}
	return f64(m, k)
}

// fnum 显示浮点数：整数不带小数点以外的多余位。
func fnum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func it(name string, value any, threshold string, level int) Item {
	return Item{Name: name, Value: fmt.Sprint(value), Threshold: threshold, Level: level}
}

func det(name string, value any, level int, db string) Item {
	return Item{Name: name, Value: fmt.Sprint(value), Level: level, DB: db, Detail: true}
}

func failedItem(name string, data jmap) Item {
	s := sv(data, "error")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return Item{Name: name, Value: fmt.Sprintf("采集失败：%s", s), Level: LevelFailed}
}

func notCollected(name string) Item {
	return Item{Name: name, Value: "未采集（本报告生成时尚未支持该项）", Level: LevelNormal}
}

func sizeText(b int64) string {
	if b >= gb {
		return fmt.Sprintf("%.2f GB", float64(b)/float64(gb))
	}
	return fmt.Sprintf("%.1f MB", float64(b)/1024/1024)
}

func lv3(v float64, attention, alert float64) int {
	switch {
	case v >= alert:
		return LevelAlert
	case v >= attention:
		return LevelAttention
	}
	return LevelNormal
}

// BuildMySQLSections 把 MySQL 巡检结果（result 的 JSON）转成分区展示。
// PolarDB / PolarDB-X / ADB / SelectDB 的专用分区不展示，这里只展示通用部分。
func BuildMySQLSections(result jmap) []Section {
	var secs []Section
	add := func(code, name string, items ...Item) {
		secs = append(secs, Section{Code: code, Name: name, Items: items})
	}
	base := func(key string) (jmap, *Item) {
		m := sub(result, key)
		if has(m, "error") {
			f := failedItem(key, m)
			return m, &f
		}
		return m, nil
	}
	failedSec := func(code, name string, f *Item) { f.Name = name; add(code, name, *f) }

	if conn, f := base("connection"); f != nil {
		failedSec("connection", "连接", f)
	} else {
		rate := f64(conn, "connection_usage_rate")
		rateLevel := LevelNormal
		switch {
		case rate > 95:
			rateLevel = LevelAlert
		case rate > 80:
			rateLevel = LevelAttention
		}
		add("connection", "连接",
			it("连接使用率", fmt.Sprintf("%.2f%%", rate), ">80% 关注，>95% 告警", rateLevel),
			it("当前连接数", fmt.Sprintf("%d / %d", i64(conn, "threads_connected"), i64(conn, "max_connections")), "", LevelNormal),
			it("活跃线程数", i64(conn, "threads_running"), "", LevelNormal),
			it("中止连接 / 中止客户端", fmt.Sprintf("%d / %d", i64(conn, "aborted_connections"), i64(conn, "aborted_clients")), "", LevelNormal),
			it("累计连接总数", i64(conn, "total_connections"), "", LevelNormal))
	}

	if bp, f := base("buffer_pool"); f != nil {
		failedSec("memory", "内存 / 缓冲池", f)
	} else {
		mem := sub(result, "memory")
		hit := f64(bp, "buffer_pool_hit_rate")
		hitLevel := LevelNormal
		switch {
		case hit < 90:
			hitLevel = LevelAlert
		case hit < 99:
			hitLevel = LevelAttention
		}
		mb := func(k string) string {
			if v := f64(mem, k); v != 0 {
				return fmt.Sprintf("%.2f MB", v/1024/1024)
			}
			return "-"
		}
		add("memory", "内存 / 缓冲池",
			it("缓冲池命中率", fnum(hit)+"%", "<99% 关注，<90% 告警", hitLevel),
			it("缓冲池大小", fmt.Sprintf("%.2f GB", f64(bp, "buffer_pool_size")/float64(gb)), "", LevelNormal),
			it("缓冲池使用率", fnum(f64(bp, "buffer_pool_usage_rate"))+"%", "", LevelNormal),
			it("脏页数", i64(bp, "buffer_pool_pages_dirty"), "", LevelNormal),
			it("sort_buffer_size", mb("sort_buffer_size"), "", LevelNormal),
			it("tmp_table_size", mb("tmp_table_size"), "", LevelNormal))
	}

	if q, f := base("query"); f != nil {
		failedSec("query", "查询", f)
	} else {
		slow := i64(q, "slow_queries")
		level := LevelNormal
		switch {
		case slow > 100:
			level = LevelAlert
		case slow > 10:
			level = LevelAttention
		}
		logLevel, logText := LevelAttention, "未启用"
		if isTrue(q, "slow_query_log_enabled") {
			logLevel, logText = LevelNormal, "已启用"
		}
		add("query", "查询",
			it("慢查询数", slow, ">10 关注，>100 告警", level),
			it("慢查询日志", logText, "", logLevel),
			it("long_query_time", fnum(f64(q, "long_query_time")), "", LevelNormal),
			it("Questions（累计）", i64(q, "questions"), "", LevelNormal),
			it("SELECT 次数", i64(q, "select_count"), "", LevelNormal),
			it("INSERT 次数", i64(q, "insert_count"), "", LevelNormal),
			it("UPDATE 次数", i64(q, "update_count"), "", LevelNormal),
			it("DELETE 次数", i64(q, "delete_count"), "", LevelNormal))
	}

	if trx, f := base("transaction"); f != nil {
		failedSec("transaction", "事务", f)
	} else {
		waits := i64(trx, "innodb_row_lock_waits")
		level := LevelNormal
		switch {
		case waits > 1000:
			level = LevelAlert
		case waits > 100:
			level = LevelAttention
		}
		hist := i64(trx, "innodb_trx_rseg_history_len")
		hl := LevelNormal
		if hist > 10000 {
			hl = LevelAttention
		}
		add("transaction", "事务",
			it("行锁等待次数", waits, ">100 关注，>1000 告警", level),
			it("平均行锁等待时间", fmt.Sprintf("%.2f ms", f64(trx, "innodb_row_lock_time_avg")/1000), "", LevelNormal),
			it("Undo日志版本链长度", hist, ">10000 关注", hl))
	}

	if io, f := base("io"); f != nil {
		failedSec("io", "I/O", f)
	} else {
		lw := i64(io, "innodb_log_waits")
		level := LevelNormal
		if lw > 0 {
			level = LevelAttention
		}
		add("io", "I/O",
			it("数据读取次数", i64(io, "innodb_data_reads"), "", LevelNormal),
			it("数据写入次数", i64(io, "innodb_data_writes"), "", LevelNormal),
			it("日志等待次数", lw, ">0 关注", level),
			it("日志写入次数", i64(io, "innodb_log_writes"), "", LevelNormal))
	}

	secs = append(secs, lockRiskSection(sub(result, "lock_risk")))
	secs = append(secs, autoIncSection(sub(result, "auto_increment")))
	secs = append(secs, noPKSection(sub(result, "no_primary_key")))
	secs = append(secs, deadlockSection(sub(result, "deadlock")))
	secs = append(secs, slowTopSection(sub(result, "slow_top")))
	secs = append(secs, bigTableSection(sub(result, "big_table")))
	secs = append(secs, indexSection(sub(result, "index")))
	secs = append(secs, charsetSection(sub(result, "charset")))
	secs = append(secs, parameterSection(sub(result, "parameter")))
	secs = append(secs, accountSection(sub(result, "account")))
	if _, ok := result["replication"]; ok {
		secs = append(secs, replicationSection(sub(result, "replication")))
	}

	if cp, f := base("capacity"); f != nil {
		failedSec("capacity", "容量", f)
	} else {
		add("capacity", "容量",
			it("数据库数量", i64(cp, "database_count"), "", LevelNormal),
			it("表数量", i64(cp, "table_count"), "", LevelNormal),
			it("数据总大小", fnum(f64(cp, "total_data_size_mb"))+" MB", "", LevelNormal))
	}
	return applyMySQLAdvice(finalize(secs))
}

func lockRiskSection(data jmap) Section {
	s := Section{Code: "lock_risk", Name: "长事务 / MDL 锁等待"}
	if len(data) == 0 {
		s.Items = append(s.Items, notCollected("长事务"))
		return s
	}
	lt := sub(data, "long_trx")
	if has(lt, "error") {
		s.Items = append(s.Items, failedItem("长事务", lt))
	} else {
		maxSec := i64(lt, "max_seconds")
		level := LevelNormal
		switch {
		case maxSec >= longTrxAlertSeconds:
			level = LevelAlert
		case i64(lt, "count") > 0:
			level = LevelAttention
		}
		s.Items = append(s.Items, it(fmt.Sprintf("长事务数（>%d秒）", longTrxAttentionSeconds), i64(lt, "count"),
			fmt.Sprintf(">%d秒 关注，>%d秒 告警", longTrxAttentionSeconds, longTrxAlertSeconds), level))
		if i64(lt, "count") > 0 {
			s.Items = append(s.Items, it("最长事务时长", fmt.Sprintf("%d 秒", maxSec), "", level))
		}
		for _, t := range jrows(lt, "items") {
			tl := LevelAttention
			if i64(t, "seconds") >= longTrxAlertSeconds {
				tl = LevelAlert
			}
			q := sv(t, "sql")
			if q == "" {
				q = "（空闲事务，未在执行SQL）"
			}
			s.Items = append(s.Items, det(fmt.Sprintf("线程 %d · %s@%s", i64(t, "thread_id"), sv(t, "user"), sv(t, "host")),
				fmt.Sprintf("%d秒 · %s · 锁定%d行/修改%d行 · %s", i64(t, "seconds"), sv(t, "state"), i64(t, "rows_locked"), i64(t, "rows_modified"), q), tl, sv(t, "db")))
		}
	}
	mdl := sub(data, "mdl")
	if has(mdl, "error") {
		s.Items = append(s.Items, failedItem("MDL 锁等待", mdl))
	} else {
		level := LevelNormal
		if i64(mdl, "count") > 0 {
			level = LevelAlert
		}
		s.Items = append(s.Items, it("等待 MDL 元数据锁的会话数", i64(mdl, "count"), ">0 告警", level))
		for _, w := range jrows(mdl, "items") {
			blocker := "阻塞者未知"
			if has(w, "blocking_pid") {
				blocker = fmt.Sprintf("阻塞者线程 %d", i64(w, "blocking_pid"))
			}
			s.Items = append(s.Items, det(fmt.Sprintf("线程 %d · %s@%s", i64(w, "thread_id"), sv(w, "user"), sv(w, "host")),
				fmt.Sprintf("已等待%d秒 · %s · %s", i64(w, "seconds"), blocker, sv(w, "sql")), LevelAlert, sv(w, "db")))
		}
	}
	return s
}

func autoIncSection(data jmap) Section {
	s := Section{Code: "auto_increment", Name: "自增ID使用率"}
	switch {
	case len(data) == 0:
		s.Items = append(s.Items, notCollected("自增ID使用率"))
	case has(data, "error"):
		s.Items = append(s.Items, failedItem("自增ID使用率", data))
	default:
		maxPct := f64(data, "max_usage_pct")
		s.Items = append(s.Items,
			it("最高自增ID使用率", fnum(maxPct)+"%", fmt.Sprintf(">=%d%% 关注，>=%d%% 告警", autoIncAttentionPct, autoIncAlertPct), lv3(maxPct, autoIncAttentionPct, autoIncAlertPct)),
			it("自增列数量", i64(data, "column_count"), "", LevelNormal))
		for _, c := range jrows(data, "items") {
			pct := f64(c, "usage_pct")
			s.Items = append(s.Items, det(sv(c, "table")+"."+sv(c, "column"),
				fmt.Sprintf("%s%%（%s / %s，%s）", fnum(pct), sv(c, "auto_increment"), sv(c, "max_value"), sv(c, "column_type")), lv3(pct, autoIncAttentionPct, autoIncAlertPct), ""))
		}
	}
	return s
}

func noPKSection(data jmap) Section {
	s := Section{Code: "no_primary_key", Name: "无主键表"}
	switch {
	case len(data) == 0:
		s.Items = append(s.Items, notCollected("无主键表"))
	case has(data, "error"):
		s.Items = append(s.Items, failedItem("无主键表", data))
	default:
		count := i64(data, "count")
		level := LevelNormal
		switch {
		case i64(data, "large_count") > 0:
			level = LevelAlert
		case count > 0:
			level = LevelAttention
		}
		s.Items = append(s.Items, it("无主键表数量", count, fmt.Sprintf(">0 关注，含 %d 万行以上的表告警", noPKAlertRows/10000), level))
		list := jrows(data, "items")
		for _, t := range list {
			tl := LevelAttention
			if i64(t, "rows") >= noPKAlertRows {
				tl = LevelAlert
			}
			s.Items = append(s.Items, det(sv(t, "table"), fmt.Sprintf("%s · 约 %d 行", sv(t, "engine"), i64(t, "rows")), tl, ""))
		}
		if count > int64(len(list)) {
			s.Items = append(s.Items, det("…", fmt.Sprintf("其余 %d 张按行数排序未列出", count-int64(len(list))), LevelNormal, ""))
		}
	}
	return s
}

func deadlockSection(data jmap) Section {
	s := Section{Code: "deadlock", Name: fmt.Sprintf("死锁（最近%d天）", deadlockDays)}
	name := fmt.Sprintf("最近%d天死锁次数", deadlockDays)
	if len(data) == 0 {
		s.Items = append(s.Items, notCollected(name))
		return s
	}
	if has(data, "error") {
		s.Items = append(s.Items, failedItem(name, data))
		return s
	}
	count := i64(data, "count")
	threshold := fmt.Sprintf(">0 关注，>=%d 告警", deadlockAlertCount)
	level := LevelNormal
	switch {
	case count >= deadlockAlertCount:
		level = LevelAlert
	case count > 0:
		level = LevelAttention
	}
	switch sv(data, "source") {
	case "error_log":
		s.Items = append(s.Items, it(name, count, threshold, level))
		if ev := strs(data, "events"); len(ev) > 0 {
			s.Items = append(s.Items, det("死锁发生时间", strings.Join(ev, "、"), level, ""))
		}
	case "latest_only":
		value := "最近一次死锁在 7 天之前，7 天内无记录"
		if count > 0 {
			value = "最近一次死锁发生在 7 天内（只能看到最近一次，实际可能更多）"
		}
		s.Items = append(s.Items, it(name, value, threshold, level))
	default:
		value := "无法统计（错误日志无记录，且读不到 INNODB STATUS）"
		if has(data, "counter_since_startup") && i64(data, "counter_since_startup") == 0 {
			value = "未发现死锁记录"
		} else if !has(data, "counter_since_startup") && !has(data, "print_all_deadlocks") {
			value = "不适用（非 InnoDB 引擎，或账号缺少 PROCESS 权限）"
		}
		s.Items = append(s.Items, it(name, value, threshold, LevelNormal))
	}
	if latest := sub(data, "latest"); len(latest) > 0 {
		tables := strings.Join(strs(latest, "tables"), "、")
		if tables == "" {
			tables = "未解析出表名"
		}
		s.Items = append(s.Items, det("最近一次死锁", fmt.Sprintf("%s · 涉及表 %s", sv(latest, "time"), tables), LevelNormal, ""))
	}
	if has(data, "counter_since_startup") {
		s.Items = append(s.Items, it("实例启动以来死锁累计", i64(data, "counter_since_startup"), "", LevelNormal))
	}
	if has(data, "print_all_deadlocks") {
		on, l := "OFF", LevelAttention
		if isTrue(data, "print_all_deadlocks") {
			on, l = "ON", LevelNormal
		}
		s.Items = append(s.Items, it("innodb_print_all_deadlocks", on, "建议 ON，才能统计 7 天内的全部死锁", l))
	}
	return s
}

func slowTopSection(data jmap) Section {
	s := Section{Code: "slow_top", Name: fmt.Sprintf("慢查询 Top%d", slowTopLimit)}
	name := s.Name
	switch {
	case len(data) == 0:
		s.Items = append(s.Items, notCollected(name))
	case has(data, "error"):
		s.Items = append(s.Items, failedItem(name, data))
	default:
		top := jrows(data, "items")
		veryslow := 0
		for _, q := range top {
			if f64(q, "avg_seconds") >= slowTopAlertAvgSecond {
				veryslow++
			}
		}
		var source string
		switch sv(data, "source") {
		case "slow_log":
			source = fmt.Sprintf("平台慢日志 · 近%d天共 %d 条慢SQL，%d 类 · 按总耗时排序", i64(data, "days"), i64(data, "slow_count"), i64(data, "fingerprint_count"))
		case "none":
			source = fmt.Sprintf("无数据：近%d天没有平台采集的慢日志（实例管理里开启「慢日志采集开关」），且实例不支持 performance_schema", slowTopDays)
		default:
			source = fmt.Sprintf("performance_schema 累计统计（实例未开启慢日志采集或近%d天无记录）· 最大耗时 >= long_query_time(%s秒) · 按总耗时排序", slowTopDays, fnum(f64(data, "long_query_time")))
		}
		level := LevelNormal
		if veryslow > 0 {
			level = LevelAttention
		}
		s.Items = append(s.Items, it("数据来源", source, "", LevelNormal),
			it(fmt.Sprintf("平均耗时>=%d秒的SQL", slowTopAlertAvgSecond), veryslow,
				fmt.Sprintf(">0 关注；明细行平均>=%d秒 关注，>=%d秒 告警", slowTopAttentionAvgSecond, slowTopAlertAvgSecond), level))
		if len(top) == 0 {
			s.Items = append(s.Items, it("慢SQL", "无", "", LevelNormal))
		}
		for i, q := range top {
			avg := f64(q, "avg_seconds")
			last := sv(q, "last_seen")
			if last == "" {
				last = "-"
			}
			db := sv(q, "db")
			shown := db
			if shown == "" {
				shown = "-"
			}
			s.Items = append(s.Items, det(fmt.Sprintf("Top%d · %s", i+1, shown),
				fmt.Sprintf("%d次 · 总%s秒 · 平均%s秒 · 最大%s秒 · 平均扫描%d行 · 最近%s · %s", i64(q, "count"), fnum(f64(q, "total_seconds")), fnum(avg), fnum(f64(q, "max_seconds")), i64(q, "avg_rows_examined"), last, sv(q, "sql")),
				lv3(avg, slowTopAttentionAvgSecond, slowTopAlertAvgSecond), db))
		}
	}
	return s
}

func bigTableSection(data jmap) Section {
	s := Section{Code: "big_table", Name: "大表 / 碎片"}
	switch {
	case len(data) == 0:
		s.Items = append(s.Items, notCollected(s.Name))
	case has(data, "error"):
		s.Items = append(s.Items, failedItem(s.Name, data))
	default:
		bigLevel := LevelNormal
		switch {
		case i64(data, "big_alert_count") > 0:
			bigLevel = LevelAlert
		case i64(data, "big_count") > 0:
			bigLevel = LevelAttention
		}
		s.Items = append(s.Items,
			it("表数量 / 总大小", fmt.Sprintf("%d 张 / %s", i64(data, "table_count"), sizeText(i64(data, "total_size_bytes"))), "", LevelNormal),
			it("大表数量", i64(data, "big_count"), fmt.Sprintf("单表 >%dGB 或 >%d万行 关注，>%dGB 告警", bigTableAttentionBytes/gb, bigTableRows/10000, bigTableAlertBytes/gb), bigLevel))
		big := jrows(data, "big_items")
		for _, t := range big {
			l := LevelAttention
			if i64(t, "size_bytes") >= bigTableAlertBytes {
				l = LevelAlert
			}
			s.Items = append(s.Items, det(sv(t, "table"), fmt.Sprintf("%s（数据 %s + 索引 %s） · 约 %d 行 · %s", sizeText(i64(t, "size_bytes")), sizeText(i64(t, "data_bytes")), sizeText(i64(t, "index_bytes")), i64(t, "rows"), sv(t, "engine")), l, ""))
		}
		if len(big) == 0 {
			for _, t := range jrows(data, "largest") {
				s.Items = append(s.Items, det(fmt.Sprintf("最大表 · %s", sv(t, "table")), fmt.Sprintf("%s · 约 %d 行", sizeText(i64(t, "size_bytes")), i64(t, "rows")), LevelNormal, ""))
			}
		}
		fl := LevelNormal
		if i64(data, "fragment_count") > 0 {
			fl = LevelAttention
		}
		s.Items = append(s.Items, it("高碎片表数量", i64(data, "fragment_count"), fmt.Sprintf("碎片 >=%dGB 且碎片率 >=%d%% 关注", fragmentMinFreeBytes/gb, fragmentRatioPct), fl))
		for _, t := range jrows(data, "fragment_items") {
			s.Items = append(s.Items, det(sv(t, "table"), fmt.Sprintf("碎片 %s · 碎片率 %s%% · 表大小 %s", sizeText(i64(t, "free_bytes")), fnum(f64(t, "fragment_pct")), sizeText(i64(t, "size_bytes"))), LevelAttention, ""))
		}
	}
	return s
}

func indexSection(data jmap) Section {
	s := Section{Code: "index", Name: "索引（冗余 / 未使用）"}
	if len(data) == 0 {
		s.Items = append(s.Items, notCollected(s.Name))
		return s
	}
	red := sub(data, "redundant")
	if has(red, "error") {
		s.Items = append(s.Items, failedItem("冗余索引", red))
	} else {
		l := LevelNormal
		if i64(red, "count") > 0 {
			l = LevelAttention
		}
		s.Items = append(s.Items, it("冗余索引数量", i64(red, "count"), ">0 关注", l))
		for _, r := range jrows(red, "items") {
			s.Items = append(s.Items, det(fmt.Sprintf("%s · %s(%s)", sv(r, "table"), sv(r, "index"), sv(r, "columns")),
				fmt.Sprintf("被 %s(%s) 覆盖 · %s", sv(r, "dominant_index"), sv(r, "dominant_columns"), sv(r, "sql")), LevelAttention, ""))
		}
	}
	un := sub(data, "unused")
	switch {
	case isTrue(un, "disabled"):
		s.Items = append(s.Items, it("未使用索引", "不可用：实例未开启 performance_schema，无法统计索引使用情况", "", LevelNormal))
	case has(un, "error"):
		s.Items = append(s.Items, failedItem("未使用索引", un))
	default:
		days := f64(un, "uptime_seconds") / 86400
		s.Items = append(s.Items, it("未使用索引数量", i64(un, "count"),
			fmt.Sprintf("仅供参考：来源 performance_schema，统计本节点启动以来（%.1f 天）的访问；读写分离时只读节点上的查询不计入，删除前需在所有节点确认", days), LevelNormal))
		for _, n := range strs(un, "items") {
			s.Items = append(s.Items, det(n, "自节点启动以来未被使用", LevelNormal, ""))
		}
	}
	return s
}

func sortedDist(m jmap) string {
	type kvp struct {
		k string
		v int64
	}
	var l []kvp
	for k, v := range m {
		l = append(l, kvp{k, toInt(v)})
	}
	sort.Slice(l, func(i, j int) bool {
		if l[i].v != l[j].v {
			return l[i].v > l[j].v
		}
		return l[i].k < l[j].k
	})
	parts := make([]string, len(l))
	for i, x := range l {
		parts[i] = fmt.Sprintf("%s: %d 张", x.k, x.v)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "，")
}

func charsetSection(data jmap) Section {
	s := Section{Code: "charset", Name: "字符集"}
	switch {
	case len(data) == 0:
		s.Items = append(s.Items, notCollected(s.Name))
	case has(data, "error"):
		s.Items = append(s.Items, failedItem(s.Name, data))
	default:
		cs, coll := sub(data, "table_charsets"), sub(data, "table_collations")
		more := func(m jmap) int {
			if len(m) > 1 {
				return LevelAttention
			}
			return LevelNormal
		}
		l := LevelAttention
		if sv(data, "character_set_server") == "utf8mb4" {
			l = LevelNormal
		}
		mm := LevelNormal
		if i64(data, "column_mismatch_count") > 0 {
			mm = LevelAttention
		}
		s.Items = append(s.Items,
			it("实例默认", sv(data, "character_set_server")+" / "+sv(data, "collation_server"), "建议 utf8mb4", l),
			it("表字符集分布", sortedDist(cs), "超过 1 种 关注", more(cs)),
			it("表排序规则分布", sortedDist(coll), "超过 1 种 关注", more(coll)),
			it("与所在表字符集不同的列", i64(data, "column_mismatch_count"), ">0 关注", mm),
			it("utf8(mb3) 表 / 列", fmt.Sprintf("%d 张表 / %d 个列", i64(data, "utf8mb3_table_count"), i64(data, "utf8mb3_column_count")), "建议统一为 utf8mb4", LevelNormal))
		for _, t := range jrows(data, "other_collation_tables") {
			s.Items = append(s.Items, det(sv(t, "table"), fmt.Sprintf("排序规则 %s（主流为 %s）", sv(t, "collation"), sv(data, "main_collation")), LevelAttention, ""))
		}
		for _, c := range jrows(data, "column_mismatch") {
			s.Items = append(s.Items, det(sv(c, "column"), fmt.Sprintf("列 %s/%s，表为 %s", sv(c, "charset"), sv(c, "collation"), sv(c, "table_charset")), LevelAttention, ""))
		}
	}
	return s
}

func parameterSection(data jmap) Section {
	s := Section{Code: "parameter", Name: "参数基线"}
	switch {
	case len(data) == 0:
		s.Items = append(s.Items, notCollected(s.Name))
	case has(data, "error"):
		s.Items = append(s.Items, failedItem(s.Name, data))
	default:
		for _, p := range jrows(data, "items") {
			value := sv(p, "actual") + " · " + sv(p, "status")
			if r := sv(p, "reason"); r != "" && sv(p, "status") == "不符合" { // i18n-ignore 与采集结果里存的状态值比较
				value += " · " + r
			}
			th := ""
			if e := sv(p, "expected"); e != "-" {
				th = fmt.Sprintf("建议 %s", e)
			}
			s.Items = append(s.Items, it(sv(p, "name"), value, th, int(i64(p, "level"))))
		}
	}
	return s
}

func accountSection(data jmap) Section {
	s := Section{Code: "account", Name: "高危账号"}
	switch {
	case len(data) == 0:
		s.Items = append(s.Items, notCollected(s.Name))
	case has(data, "error"):
		s.Items = append(s.Items, failedItem(s.Name, data))
	default:
		risk, alert := i64(data, "risk_count"), i64(data, "alert_count")
		th := ""
		if sk := i64(data, "skipped_internal"); sk > 0 {
			th = fmt.Sprintf("已跳过云厂商内置账号 %d 个", sk)
		}
		l := LevelNormal
		switch {
		case alert > 0:
			l = LevelAlert
		case risk > 0:
			l = LevelAttention
		}
		s.Items = append(s.Items, it("账号总数", i64(data, "account_count"), th, LevelNormal),
			it("风险项数量", risk, "远程空密码/匿名账号 告警；本机空密码、任意主机高权限、FILE 权限 关注", l))
		for _, r := range jrows(data, "items") {
			s.Items = append(s.Items, det(sv(r, "account"), sv(r, "risk"), int(i64(r, "level")), ""))
		}
	}
	return s
}

func replicationSection(data jmap) Section {
	s := Section{Code: "replication", Name: "复制与 binlog"}
	if has(data, "error") {
		s.Items = append(s.Items, failedItem(s.Name, data))
		return s
	}
	rep := sub(data, "replica")
	switch {
	case has(rep, "error"):
		s.Items = append(s.Items, failedItem("复制状态", rep))
	case !isTrue(data, "is_replica"):
		s.Items = append(s.Items, it("复制状态", "未配置复制（本实例不是从库）", "", LevelNormal))
	default:
		io, sql := sv(rep, "io_running"), sv(rep, "sql_running")
		running := strings.EqualFold(io, "Yes") && strings.EqualFold(sql, "Yes")
		l := LevelNormal
		if !running {
			l = LevelAlert
		}
		s.Items = append(s.Items, it("复制线程", fmt.Sprintf("IO %s / SQL %s · 主库 %s", io, sql, sv(rep, "source_host")), "IO 与 SQL 线程都应为 Yes，否则告警", l))
		lag := i64(rep, "seconds_behind")
		switch {
		case lag < 0:
			s.Items = append(s.Items, it("复制延迟", "未知（复制线程未运行）", fmt.Sprintf(">%d秒 关注，>%d秒 告警", replLagAttentionSeconds, replLagAlertSeconds), LevelAlert))
		default:
			ll := LevelNormal
			switch {
			case lag > replLagAlertSeconds:
				ll = LevelAlert
			case lag > replLagAttentionSeconds:
				ll = LevelAttention
			}
			s.Items = append(s.Items, it("复制延迟", fmt.Sprintf("%d 秒", lag), fmt.Sprintf(">%d秒 关注，>%d秒 告警", replLagAttentionSeconds, replLagAlertSeconds), ll))
		}
		for _, k := range [][2]string{{"last_io_error", "最近 IO 错误"}, {"last_sql_error", "最近 SQL 错误"}} {
			if e := sv(rep, k[0]); e != "" {
				s.Items = append(s.Items, det(k[1], e, LevelAlert, ""))
			}
		}
	}
	bl := sub(data, "binlog")
	switch {
	case isTrue(bl, "disabled"):
		s.Items = append(s.Items, it("binlog", "未开启或账号无权限查看（需要 REPLICATION CLIENT）", "", LevelNormal))
	default:
		l := LevelNormal
		if i64(bl, "total_bytes") >= binlogAttentionBytes {
			l = LevelAttention
		}
		s.Items = append(s.Items, it("binlog 文件", fmt.Sprintf("%d 个，共 %s", i64(bl, "count"), sizeText(i64(bl, "total_bytes"))), fmt.Sprintf(">%dGB 关注（检查 binlog 过期时间）", binlogAttentionBytes/gb), l))
	}
	return s
}

// ---- 健康评分 ----

// MySQLHealth 按规则扣分，生成评分、问题、警告和建议。
// 复制检查也在这里扣分。
func MySQLHealth(r jmap) jmap {
	score := 100
	status := "healthy"
	issues, warnings, suggestions := []any{}, []any{}, []any{}
	issue := func(msg string, d int, critical bool) {
		issues = append(issues, msg)
		score -= d
		if critical {
			status = "critical"
		}
	}
	warn := func(msg string, d int) { warnings = append(warnings, msg); score -= d }

	rate := f64(sub(r, "connection"), "connection_usage_rate")
	switch {
	case rate > 95:
		issue("连接数已达到上限，立即采取措施", 20, true)
	case rate > 80:
		warn("连接数接近上限，建议增加max_connections或优化连接管理", 10)
	}
	hit := numOr(sub(r, "buffer_pool"), "buffer_pool_hit_rate", 100)
	switch {
	case hit < 95:
		issue(fmt.Sprintf("缓冲池命中率过低(%s%%)，性能受影响", fnum(hit)), 25, true)
	case hit < 99:
		warn(fmt.Sprintf("缓冲池命中率低于99%%（当前%s%%），建议调整innodb_buffer_pool_size", fnum(hit)), 15)
	}
	slow := i64(sub(r, "query"), "slow_queries")
	switch {
	case slow > 50:
		issue(fmt.Sprintf("慢查询数过多（%d），性能严重下降", slow), 30, true)
	case slow > 10:
		warn(fmt.Sprintf("慢查询数较多（%d），建议分析和优化", slow), 15)
	}
	trx := sub(r, "transaction")
	switch lw := i64(trx, "innodb_row_lock_waits"); {
	case lw > 1000:
		issue(fmt.Sprintf("行锁等待严重（%d次），建议检查长事务和死锁", lw), 20, true)
	case lw > 100:
		warn(fmt.Sprintf("行锁等待过多（%d次），建议优化", lw), 10)
	}
	if h := i64(trx, "innodb_trx_rseg_history_len"); h > 10000 {
		warn(fmt.Sprintf("Undo日志版本链过长(%d)，存在长事务", h), 10)
	}
	lock := sub(r, "lock_risk")
	lt := sub(lock, "long_trx")
	switch {
	case i64(lt, "max_seconds") >= longTrxAlertSeconds:
		issue(fmt.Sprintf("存在长事务 %d 个，最长 %d 秒，建议确认后提交或终止", i64(lt, "count"), i64(lt, "max_seconds")), 15, true)
	case i64(lt, "count") > 0:
		warn(fmt.Sprintf("存在超过 %d 秒的事务 %d 个", longTrxAttentionSeconds, i64(lt, "count")), 5)
	}
	if n := i64(sub(lock, "mdl"), "count"); n > 0 {
		issue(fmt.Sprintf("有 %d 个会话在等待 MDL 元数据锁，通常是 DDL 被长事务/长查询阻塞", n), 15, true)
	}
	ai := sub(r, "auto_increment")
	switch {
	case i64(ai, "alert_count") > 0:
		issue(fmt.Sprintf("%d 个自增列使用率超过 %d%%（最高 %s%%），尽快改为 BIGINT", i64(ai, "alert_count"), autoIncAlertPct, fnum(f64(ai, "max_usage_pct"))), 15, true)
	case i64(ai, "attention_count") > 0:
		warn(fmt.Sprintf("%d 个自增列使用率超过 %d%%，建议规划改为 BIGINT", i64(ai, "attention_count"), autoIncAttentionPct), 5)
	}
	if n := i64(sub(r, "no_primary_key"), "count"); n > 0 {
		warn(fmt.Sprintf("存在 %d 张无主键表，ROW 复制下从库回放慢，建议补主键", n), 5)
	}
	dl := sub(r, "deadlock")
	switch n := i64(dl, "count"); {
	case n >= deadlockAlertCount:
		issue(fmt.Sprintf("最近 %d 天发生死锁 %d 次，建议排查加锁顺序", deadlockDays, n), 10, false)
	case n > 0:
		warn(fmt.Sprintf("最近 %d 天发生过死锁（%d 次）", deadlockDays, n), 5)
	}
	big := sub(r, "big_table")
	if n := i64(big, "big_count"); n > 0 {
		warn(fmt.Sprintf("存在 %d 张大表（>50GB 或 >5000 万行），DDL 和备份恢复耗时长，建议归档或拆分", n), int(min(n, 5)))
	}
	if n := i64(big, "fragment_count"); n > 0 {
		suggestions = append(suggestions, fmt.Sprintf("%d 张表碎片率超过 %d%%，可回收约 %.1f GB，低峰期执行 ALTER TABLE ... ENGINE=InnoDB 整理", n, fragmentRatioPct, float64(i64(big, "fragment_free_bytes"))/float64(gb)))
	}
	idx := sub(r, "index")
	if n := i64(sub(idx, "redundant"), "count"); n > 0 {
		warn(fmt.Sprintf("存在 %d 个冗余索引，增加写入开销和存储，建议删除", n), int(min(n, 5)))
	}
	if n := i64(sub(idx, "unused"), "count"); n > 0 {
		suggestions = append(suggestions, fmt.Sprintf("%d 个索引自节点启动以来未被使用（只统计当前连接的节点），读写分离下需在只读节点上确认后再删除", n))
	}
	cs := sub(r, "charset")
	if len(sub(cs, "table_charsets")) > 1 || i64(cs, "column_mismatch_count") > 0 {
		names := make([]string, 0)
		for k := range sub(cs, "table_charsets") {
			names = append(names, k)
		}
		sort.Strings(names)
		warn(fmt.Sprintf("字符集混用：表字符集 %s，%d 个列与所在表字符集不同，关联时会隐式转换导致索引失效", strings.Join(names, "/"), i64(cs, "column_mismatch_count")), 3)
	}
	if n := i64(cs, "utf8mb3_table_count"); n > 0 {
		suggestions = append(suggestions, fmt.Sprintf("%d 张表使用 utf8(mb3)，无法存储 emoji，建议统一为 utf8mb4", n))
	}
	for _, p := range jrows(sub(r, "parameter"), "items") {
		switch int(i64(p, "level")) {
		case LevelAlert:
			issue(fmt.Sprintf("参数 %s=%s，建议 %s：%s", sv(p, "name"), sv(p, "actual"), sv(p, "expected"), sv(p, "reason")), 5, false)
		case LevelAttention:
			warn(fmt.Sprintf("参数 %s=%s，建议 %s", sv(p, "name"), sv(p, "actual"), sv(p, "expected")), 1)
		}
	}
	attentionAccounts := 0
	for _, a := range jrows(sub(r, "account"), "items") {
		switch int(i64(a, "level")) {
		case LevelAlert:
			issue(fmt.Sprintf("高危账号 %s：%s", sv(a, "account"), sv(a, "risk")), 10, true)
		case LevelAttention:
			attentionAccounts++
		}
	}
	if attentionAccounts > 0 {
		warn(fmt.Sprintf("%d 个账号存在权限风险（任意主机高权限 / FILE 权限）", attentionAccounts), 3)
	}
	veryslow := 0
	maxAvg := 0.0
	for _, q := range jrows(sub(r, "slow_top"), "items") {
		if a := f64(q, "avg_seconds"); a >= slowTopAlertAvgSecond {
			veryslow++
			maxAvg = max(maxAvg, a)
		}
	}
	if veryslow > 0 {
		warn(fmt.Sprintf("慢查询 Top%d 中有 %d 类 SQL 平均耗时超过 %d 秒，最慢平均 %s 秒，建议优先优化", slowTopLimit, veryslow, slowTopAlertAvgSecond, fnum(maxAvg)), 5)
	}
	if len(dl) > 0 && !has(dl, "error") && has(dl, "print_all_deadlocks") && !isTrue(dl, "print_all_deadlocks") {
		suggestions = append(suggestions, "建议开启 innodb_print_all_deadlocks，死锁会写入错误日志，巡检才能统计 7 天内的全部死锁")
	}
	// 复制：线程停止是故障，延迟过大是风险
	if repl := sub(r, "replication"); isTrue(repl, "is_replica") {
		rep := sub(repl, "replica")
		if !strings.EqualFold(sv(rep, "io_running"), "Yes") || !strings.EqualFold(sv(rep, "sql_running"), "Yes") {
			issue(fmt.Sprintf("复制线程异常（IO %s / SQL %s），从库已停止同步", sv(rep, "io_running"), sv(rep, "sql_running")), 20, true)
		} else if lag := i64(rep, "seconds_behind"); lag > replLagAlertSeconds {
			issue(fmt.Sprintf("复制延迟 %d 秒，从库读到的数据已明显落后", lag), 10, false)
		} else if lag > replLagAttentionSeconds {
			warn(fmt.Sprintf("复制延迟 %d 秒", lag), 5)
		}
	}
	if score < 0 {
		score = 0
	}
	return jmap{"health_score": score, "issues": issues, "warnings": warnings, "suggestions": suggestions, "status": status}
}
