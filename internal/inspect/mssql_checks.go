// @author: kevinkelly
// @license: Apache Licence
// @file: mssql_checks.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SQL Server 巡检的检查项目录（只实现最常用的一部分检查项）。每项只读 DMV / 目录视图。

const (
	msItemTimeout   = 60 * time.Second // 单条 DMV 查询超时
	msTotalBudget   = 600 * time.Second
	msMaxDatabases  = 200
	msSQLTextLimit  = 500
	scopeInstance   = "instance"
	scopeDatabase   = "database"
	msBenignWaitSQL = "'BROKER_EVENTHANDLER','BROKER_RECEIVE_WAITFOR','BROKER_TASK_STOP','BROKER_TO_FLUSH','BROKER_TRANSMITTER','CHECKPOINT_QUEUE','CHKPT','CLR_AUTO_EVENT','CLR_MANUAL_EVENT','CLR_SEMAPHORE'," +
		"'DBMIRROR_DBM_EVENT','DBMIRROR_EVENTS_QUEUE','DBMIRROR_WORKER_QUEUE','DBMIRRORING_CMD','DIRTY_PAGE_POLL','DISPATCHER_QUEUE_SEMAPHORE','EXECSYNC','FSAGENT','FT_IFTS_SCHEDULER_IDLE_WAIT','FT_IFTSHC_MUTEX'," +
		"'HADR_CLUSAPI_CALL','HADR_FILESTREAM_IOMGR_IOCOMPLETION','HADR_LOGCAPTURE_WAIT','HADR_NOTIFICATION_DEQUEUE','HADR_TIMER_TASK','HADR_WORK_QUEUE','KSOURCE_WAKEUP','LAZYWRITER_SLEEP','LOGMGR_QUEUE'," +
		"'MEMORY_ALLOCATION_EXT','ONDEMAND_TASK_QUEUE','PARALLEL_REDO_DRAIN_WORKER','PARALLEL_REDO_LOG_CACHE','PARALLEL_REDO_TRAN_LIST','PARALLEL_REDO_WORKER_SYNC','PARALLEL_REDO_WORKER_WAIT_WORK'," +
		"'PREEMPTIVE_OS_FLUSHFILEBUFFERS','PREEMPTIVE_XE_GETTARGETSTATE','PVS_PREALLOCATE','PWAIT_ALL_COMPONENTS_INITIALIZED','PWAIT_DIRECTLOGCONSUMER_GETNEXT','PWAIT_EXTENSIBILITY_CLEANUP_TASK'," +
		"'QDS_PERSIST_TASK_MAIN_LOOP_SLEEP','QDS_ASYNC_QUEUE','QDS_CLEANUP_STALE_QUERIES_TASK_MAIN_LOOP_SLEEP','QDS_SHUTDOWN_QUEUE','REDO_THREAD_PENDING_WORK','REQUEST_FOR_DEADLOCK_SEARCH','RESOURCE_QUEUE'," +
		"'SERVER_IDLE_CHECK','SLEEP_BPOOL_FLUSH','SLEEP_DBSTARTUP','SLEEP_DCOMSTARTUP','SLEEP_MASTERDBREADY','SLEEP_MASTERMDREADY','SLEEP_MASTERUPGRADED','SLEEP_MSDBSTARTUP','SLEEP_SYSTEMTASK','SLEEP_TASK'," +
		"'SLEEP_TEMPDBSTARTUP','SNI_HTTP_ACCEPT','SOS_WORK_DISPATCHER','SP_SERVER_DIAGNOSTICS_SLEEP','SQLTRACE_BUFFER_FLUSH','SQLTRACE_INCREMENTAL_FLUSH_SLEEP','SQLTRACE_WAIT_ENTRIES','VDI_CLIENT_OTHER'," +
		"'WAIT_FOR_RESULTS','WAITFOR','WAITFOR_TASKSHUTDOWN','WAIT_XTP_RECOVERY','WAIT_XTP_HOST_WAIT','WAIT_XTP_OFFLINE_CKPT_NEW_LOG','WAIT_XTP_CKPT_CLOSE','XE_DISPATCHER_JOIN','XE_DISPATCHER_WAIT','XE_TIMER_EVENT'"
	// 统一取语句文本：按 offset 截取，避免整批文本
	msStmtSQL = "SUBSTRING(st.text, (qs.statement_start_offset/2)+1, ((CASE qs.statement_end_offset WHEN -1 THEN DATALENGTH(st.text) ELSE qs.statement_end_offset END - qs.statement_start_offset)/2)+1)"
)

// msSections 是分区的代码与名称（顺序即展示顺序）。
var msSections = [][2]string{
	{"overview", "实例概况"}, {"cpu", "CPU"}, {"memory", "内存"}, {"io", "IO"}, {"blocking", "阻塞与死锁"},
	{"index", "索引"}, {"topsql", "Top SQL"}, {"database", "数据库与文件"}, {"error", "错误日志与作业"},
}

// msSectionPriority 是“需处理的问题”的挑选顺序：先影响线上、再影响数据安全。
var msSectionPriority = []string{"blocking", "io", "database", "memory", "cpu", "index", "error", "overview", "topsql"}

type msResult struct {
	Level     int
	Value     string
	Threshold string
	Detail    *DetailTable
	Raw       any // 累计型计数器的原始值，供下一份报告做差
}

type msCheck struct {
	Key        string
	Section    string
	Name       string
	Scope      string
	MinVersion int    // 主版本号：11=2012 12=2014 13=2016 14=2017 15=2019 16=2022
	Advice     string // 一句处理建议：报告里该项为关注 / 告警 / 未采集时显示在“建议”列
	Run        func(c *msContext) (msResult, error)
}

// msContext 是一次巡检的执行上下文。
type msContext struct {
	ctx       context.Context
	t         Target
	db        string // 每库检查项当前所在的库
	now       func() time.Time
	prevRaw   map[string]string
	version   int
	versionS  string
	start     time.Time
	cpuCount  int64
	memoryMB  int64
	server    map[string]any
	databases []string
}

var errPermission = fmt.Errorf("权限不足")

func (c *msContext) query(q string, args ...any) ([]Row, error) {
	r, err := c.t.Query(c.ctx, c.db, q, args...)
	if err != nil {
		return nil, err
	}
	return r.Maps(), nil
}

func (c *msContext) scalar(q string, args ...any) (any, error) {
	r, err := c.t.Query(c.ctx, c.db, q, args...)
	if err != nil {
		return nil, err
	}
	if len(r.Rows) == 0 || len(r.Rows[0]) == 0 {
		return nil, nil
	}
	return r.Rows[0][0], nil
}

// prev 取上一份报告同一检查项的原始计数（实例重启过则没有）。
func (c *msContext) prev(key string, out any) bool {
	raw, ok := c.prevRaw[key+"|"+c.db]
	return ok && json.Unmarshal([]byte(raw), out) == nil
}

func (c *msContext) loadServerInfo() error {
	base := "SELECT CONVERT(varchar(30), SERVERPROPERTY('ProductVersion')) AS version, CONVERT(varchar(30), SERVERPROPERTY('ProductLevel')) AS level, " +
		"CONVERT(varchar(80), SERVERPROPERTY('Edition')) AS edition, CONVERT(varchar(30), SERVERPROPERTY('ProductUpdateLevel')) AS cu, " +
		"CONVERT(varchar(128), SERVERPROPERTY('ServerName')) AS server_name, si.sqlserver_start_time, si.cpu_count, %s AS memory_mb FROM sys.dm_os_sys_info si"
	rows, err := c.query(fmt.Sprintf(base, "si.physical_memory_kb/1024"))
	if err != nil {
		rows, err = c.query(fmt.Sprintf(base, "si.physical_memory_in_bytes/1048576")) // 2008 R2 及更早
	}
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("读不到实例信息（sys.dm_os_sys_info 无数据）")
	}
	r := rows[0]
	c.versionS = r.Str("version")
	c.version, _ = strconv.Atoi(strings.SplitN(c.versionS, ".", 2)[0])
	c.start = toTime(r["sqlserver_start_time"])
	c.cpuCount, c.memoryMB = r.Int("cpu_count"), r.Int("memory_mb")
	startS := ""
	if !c.start.IsZero() {
		startS = c.start.Format("2006-01-02 15:04:05")
	}
	c.server = map[string]any{
		"version": c.versionS, "level": r.Str("level"), "cu": r.Str("cu"), "edition": r.Str("edition"), "server_name": r.Str("server_name"),
		"start_time": startS, "cpu_count": c.cpuCount, "memory_mb": c.memoryMB,
	}
	cfg, err := c.query("SELECT name, CONVERT(int, value_in_use) AS v FROM sys.configurations WHERE name IN ('max degree of parallelism','cost threshold for parallelism','max server memory (MB)','optimize for ad hoc workloads')")
	if err == nil {
		m := map[string]int64{}
		for _, row := range cfg {
			m[row.Str("name")] = row.Int("v")
		}
		for k, name := range map[string]string{"maxdop": "max degree of parallelism", "cost_threshold": "cost threshold for parallelism", "max_memory_mb": "max server memory (MB)", "adhoc": "optimize for ad hoc workloads"} {
			if v, ok := m[name]; ok {
				c.server[k] = v
			}
		}
	}
	return nil
}

func (c *msContext) loadDatabases(only []string) error {
	rows, err := c.query("SELECT name FROM sys.databases WHERE database_id > 4 AND state_desc = 'ONLINE' AND name NOT IN ('distribution') ORDER BY name")
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, d := range only {
		want[strings.ToLower(d)] = true
	}
	c.databases = nil
	for _, r := range rows {
		if n := r.Str("name"); len(want) == 0 || want[strings.ToLower(n)] {
			c.databases = append(c.databases, n)
		}
	}
	c.server["db_count"] = len(c.databases)
	names := c.databases
	if len(names) > 20 {
		names = names[:20]
	}
	c.server["db_names"] = names
	return nil
}

// ---- 小工具 ----

func fmtNum(v any) string {
	n := toInt(v)
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	out := strings.Join(parts, ",")
	if neg {
		out = "-" + out
	}
	return out
}

func fmtDuration(seconds int64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	case seconds < 86400:
		return fmt.Sprintf("%dh%02dm", seconds/3600, seconds%3600/60)
	}
	return fmt.Sprintf("%d 天 %d 小时", seconds/86400, seconds%86400/3600)
}

func fmtDT(v any) string {
	if t, ok := v.(time.Time); ok {
		return t.Format("2006-01-02 15:04:05")
	}
	return toStr(v)
}

var reWS = regexp.MustCompile(`\s+`)

func truncSQL(v any) string {
	s := strings.TrimSpace(reWS.ReplaceAllString(toStr(v), " "))
	if r := []rune(s); len(r) > msSQLTextLimit {
		s = string(r[:msSQLTextLimit]) + "…"
	}
	return s
}

func tbl(cols []string, rows [][]any) *DetailTable { return &DetailTable{Columns: cols, Rows: rows} }

func r1(f float64) float64 { return math.Round(f*10) / 10 }

func msQuoteIdent(name string) string { return "[" + strings.ReplaceAll(name, "]", "]]") + "]" }

var topSQLCols = []string{"#", "SQL", "库", "执行次数", "总CPU ms", "平均耗时 ms", "逻辑读", "最近执行"}

func topSQLQuery(orderBy, where string) string {
	return "SELECT TOP 10 qs.execution_count, qs.total_worker_time/1000 AS total_cpu_ms, qs.total_elapsed_time/1000 AS total_elapsed_ms, qs.total_logical_reads, qs.last_execution_time, " +
		msStmtSQL + " AS stmt, DB_NAME(st.dbid) AS db_name FROM sys.dm_exec_query_stats qs CROSS APPLY sys.dm_exec_sql_text(qs.sql_handle) st " + where + " ORDER BY " + orderBy + " DESC"
}

func topSQLRows(rows []Row) [][]any {
	out := make([][]any, 0, len(rows))
	for i, r := range rows {
		n := max(r.Int("execution_count"), 1)
		out = append(out, []any{i + 1, truncSQL(r["stmt"]), r.Str("db_name"), fmtNum(r["execution_count"]), fmtNum(r["total_cpu_ms"]), r1(float64(r.Int("total_elapsed_ms")) / float64(n)), fmtNum(r["total_logical_reads"]), fmtDT(r["last_execution_time"])})
	}
	return out
}

// ---- 检查项目录 ----

var msChecks = []msCheck{
	{Key: "overview.version", Section: "overview", Name: "版本与补丁", Scope: scopeInstance, Advice: "确认已安装受支持的累积更新（CU），版本过旧请规划升级。", Run: chkVersion},
	{Key: "overview.uptime", Section: "overview", Name: "启动以来运行时长", Scope: scopeInstance, Advice: "运行时长过短时累计型统计的覆盖窗口很短，结论仅供参考，运行几天后再巡检一次。", Run: chkUptime},
	{Key: "overview.config", Section: "overview", Name: "并行度与内存上限", Scope: scopeInstance, Advice: "按核数和内存核对 MAXDOP、cost threshold for parallelism 与 max server memory，避免内存被无限占用。", Run: chkConfig},
	{Key: "overview.waits", Section: "overview", Name: "Top 10 等待类型", Scope: scopeInstance, Advice: "结合 Top 等待类型定位瓶颈（CPU、IO、锁、内存授予），再针对性优化。", Run: chkWaits},
	{Key: "cpu.signal_wait", Section: "cpu", Name: "信号等待占比", Scope: scopeInstance, Advice: "信号等待占比高说明 CPU 在排队，先查 Top CPU SQL，必要时扩容或调整并行度。", Run: chkSignalWait},
	{Key: "cpu.runnable", Section: "cpu", Name: "可运行队列（采样时刻）", Scope: scopeInstance, Advice: "可运行队列持续大于 0 表示 CPU 吃紧，请优化高 CPU 的语句或扩容。", Run: chkRunnable},
	{Key: "cpu.top_sql", Section: "cpu", Name: "Top 10 CPU SQL", Scope: scopeInstance, Advice: "优先优化 CPU 消耗最高的语句（补索引、改写、处理参数嗅探）。", Run: chkCPUTopSQL},
	{Key: "memory.ple", Section: "memory", Name: "PLE（页生命周期）", Scope: scopeInstance, Advice: "PLE 偏低说明内存压力大，检查 max server memory、缺失索引导致的大扫描和内存授予。", Run: chkPLE},
	{Key: "memory.grants", Section: "memory", Name: "内存授予等待", Scope: scopeInstance, Advice: "内存授予等待多时检查大排序 / 哈希语句和统计信息，必要时调整 Resource Governor。", Run: chkGrants},
	{Key: "memory.buffer_by_db", Section: "memory", Name: "缓冲池按库占用 Top 10", Scope: scopeInstance, Advice: "缓冲池被个别库占满时，评估是否有大扫描，或需要把该库拆到独立实例。", Run: chkBufferByDB},
	{Key: "io.file_latency", Section: "io", Name: "文件读写延迟", Scope: scopeInstance, Advice: "文件读写延迟偏高时检查存储性能，并把数据文件、日志文件、tempdb 分到不同磁盘。", Run: chkFileLatency},
	{Key: "io.pending", Section: "io", Name: "Pending IO（采样时刻）", Scope: scopeInstance, Advice: "Pending IO 持续存在说明磁盘跟不上，请排查存储并优化逻辑读最高的语句。", Run: chkPendingIO},
	{Key: "io.top_reads_sql", Section: "io", Name: "Top 10 逻辑读 SQL", Scope: scopeInstance, Advice: "逻辑读最多的语句通常缺索引，优先优化它们以降低 IO。", Run: chkReadsTopSQL},
	{Key: "io.tempdb", Section: "io", Name: "tempdb 文件数与大小", Scope: scopeInstance, Advice: "tempdb 数据文件数建议与 CPU 核数（最多 8 个）对齐且大小一致，避免分配页争用。", Run: chkTempdb},
	{Key: "blocking.chain", Section: "blocking", Name: "当前阻塞链（采样时刻）", Scope: scopeInstance, Advice: "存在阻塞时先定位根因会话和语句，缩短事务，必要时终止阻塞源会话。", Run: chkBlocking},
	{Key: "blocking.long_tran", Section: "blocking", Name: "长事务（采样时刻）", Scope: scopeInstance, Advice: "长事务会一直持锁并阻止日志截断，请让应用尽快提交或回滚。", Run: chkLongTran},
	{Key: "blocking.deadlocks", Section: "blocking", Name: "死锁次数", Scope: scopeInstance, Advice: "出现死锁时统一各业务的访问顺序、缩短事务，并检查缺失索引引起的大范围加锁。", Run: chkDeadlocks},
	{Key: "index.missing", Section: "index", Name: "缺失索引 Top 20", Scope: scopeDatabase, Advice: "缺失索引提示仅供参考，评估收益和写入代价后再创建，避免与现有索引重复。", Run: chkMissingIndex},
	{Key: "index.unused", Section: "index", Name: "无用索引", Scope: scopeDatabase, Advice: "长期无读的索引占空间并拖慢写入，确认后再删除，删除前先保留创建脚本。", Run: chkUnusedIndex},
	{Key: "topsql.by_exec", Section: "topsql", Name: "Top 10 执行次数", Scope: scopeInstance, Advice: "执行次数最多的语句即使单次很快也会累积成负载，检查是否可以缓存或合并请求。", Run: chkTopSQLExec},
	{Key: "topsql.by_avg", Section: "topsql", Name: "Top 10 平均耗时（执行 ≥10 次）", Scope: scopeInstance, Advice: "平均耗时高的语句优先看执行计划，补索引或改写。", Run: chkTopSQLAvg},
	{Key: "database.files", Section: "database", Name: "文件大小与自动增长", Scope: scopeDatabase, Advice: "自动增长建议设为固定大小而不是百分比，并监控磁盘剩余空间。", Run: chkDBFiles},
	{Key: "database.log", Section: "database", Name: "日志重用与 VLF", Scope: scopeDatabase, Advice: "日志重用等待或 VLF 过多时检查备份与长事务，并按需重建日志文件。", Run: chkDBLog},
	{Key: "database.backup", Section: "database", Name: "最近备份", Scope: scopeDatabase, Advice: "超过恢复目标的未备份库请立即补做全备 / 日志备份，并确认备份作业在正常运行。", Run: chkDBBackup},
	{Key: "database.options", Section: "database", Name: "危险选项", Scope: scopeDatabase, Advice: "自动关闭、自动收缩、页校验关闭等危险选项请改回推荐值。", Run: chkDBOptions},
	{Key: "error.jobs", Section: "error", Name: "Agent 作业失败（7 天）", Scope: scopeInstance, Advice: "查看失败作业的错误信息，修复后重跑，避免备份、索引维护等例行任务长期失败。", Run: chkJobs},
	{Key: "error.dumps", Section: "error", Name: "内存转储文件", Scope: scopeInstance, Advice: "出现内存转储文件说明实例发生过严重错误，请保存文件并联系厂商分析，同时清理过旧的转储。", Run: chkDumps},
}

func chkVersion(c *msContext) (msResult, error) {
	s := c.server
	cu := toStr(s["cu"])
	value := fmt.Sprintf("SQL Server %s %s", toStr(s["version"]), toStr(s["level"]))
	if cu != "" {
		value += " " + cu
	}
	value += " · " + toStr(s["edition"])
	level := strings.TrimSpace(toStr(s["level"]) + " " + cu)
	return msResult{Level: LevelNormal, Value: value, Threshold: "—", Detail: tbl([]string{"项目", "值"}, [][]any{
		{"版本", s["version"]}, {"补丁级别", level}, {"版本类型", s["edition"]}, {"服务器名", s["server_name"]}, {"CPU 核数", s["cpu_count"]}, {"物理内存 MB", fmtNum(s["memory_mb"])},
	})}, nil
}

func chkUptime(c *msContext) (msResult, error) {
	var secs int64
	if !c.start.IsZero() {
		secs = int64(c.now().Sub(c.start).Seconds())
	}
	value := fmt.Sprintf("%s（启动于 %s）", fmtDuration(secs), fmtDT(c.start))
	if secs < 86400 {
		return msResult{Level: LevelAttention, Value: fmt.Sprintf("%s，累计型统计的覆盖窗口很短", value), Threshold: "<24h 提示"}, nil
	}
	return msResult{Level: LevelNormal, Value: value, Threshold: "<24h 提示"}, nil
}

func chkConfig(c *msContext) (msResult, error) {
	s := c.server
	var problems []string
	if v, ok := s["maxdop"]; ok && toInt(v) == 0 {
		problems = append(problems, "MAXDOP=0（未限制）")
	}
	if v, ok := s["cost_threshold"]; ok && toInt(v) == 5 {
		problems = append(problems, "cost threshold for parallelism=5（默认值偏低）")
	}
	if v, ok := s["max_memory_mb"]; !ok || toInt(v) == 2147483647 {
		problems = append(problems, "max server memory 未限制")
	}
	value := fmt.Sprintf("MAXDOP %v，cost threshold %v，max server memory %s MB，optimize for ad hoc %v", s["maxdop"], s["cost_threshold"], fmtNum(s["max_memory_mb"]), s["adhoc"])
	if len(problems) > 0 {
		return msResult{Level: LevelAttention, Value: value + "；" + strings.Join(problems, "、"), Threshold: "MAXDOP≠0，阈值≠5，内存已限制"}, nil
	}
	return msResult{Level: LevelNormal, Value: value, Threshold: "MAXDOP≠0，阈值≠5，内存已限制"}, nil
}

func chkWaits(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT TOP 150 wait_type, wait_time_ms, signal_wait_time_ms, waiting_tasks_count FROM sys.dm_os_wait_stats WHERE wait_type NOT IN (" + msBenignWaitSQL + ") AND wait_type NOT LIKE 'SLEEP_%' AND wait_time_ms > 0 ORDER BY wait_time_ms DESC")
	if err != nil {
		return msResult{}, err
	}
	raw := map[string][3]int64{}
	for _, r := range rows {
		raw[r.Str("wait_type")] = [3]int64{r.Int("wait_time_ms"), r.Int("signal_wait_time_ms"), r.Int("waiting_tasks_count")}
	}
	var prev map[string][3]int64
	window := "自启动以来"
	stats := raw
	if c.prev("overview.waits", &prev) && prev != nil {
		window = "与上一份报告的差值"
		stats = map[string][3]int64{}
		for wt, cur := range raw {
			p := prev[wt]
			if d := [3]int64{cur[0] - p[0], cur[1] - p[1], cur[2] - p[2]}; d[0] > 0 {
				stats[wt] = d
			}
		}
	}
	var total int64
	type wv struct {
		name string
		v    [3]int64
	}
	var top []wv
	for k, v := range stats {
		total += v[0]
		top = append(top, wv{k, v})
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].v[0] != top[j].v[0] {
			return top[i].v[0] > top[j].v[0]
		}
		return top[i].name < top[j].name
	})
	if len(top) > 10 {
		top = top[:10]
	}
	if total == 0 {
		total = 1
	}
	var detail [][]any
	var parts []string
	for i, w := range top {
		pct := r1(100 * float64(w.v[0]) / float64(total))
		detail = append(detail, []any{w.name, fmtNum(w.v[0]), pct, fmtNum(w.v[1]), fmtNum(w.v[2]), math.Round(float64(w.v[0])/float64(max(w.v[2], 1))*100) / 100})
		if i < 5 {
			parts = append(parts, fmt.Sprintf("%s %v%%", w.name, pct))
		}
	}
	value := fmt.Sprintf("[%s] 无显著等待", window)
	if len(parts) > 0 {
		value = fmt.Sprintf("[%s] %s", window, strings.Join(parts, " · "))
	}
	level := LevelNormal
	if len(top) > 0 && 100*float64(top[0].v[0])/float64(total) > 40 {
		level = LevelAttention
	}
	return msResult{Level: level, Value: value, Threshold: "单类型占比 >40% 关注", Detail: tbl([]string{"等待类型", "等待时长 ms", "占比 %", "信号等待 ms", "等待次数", "平均 ms"}, detail), Raw: raw}, nil
}

func chkSignalWait(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT SUM(signal_wait_time_ms) AS signal_ms, SUM(wait_time_ms) AS wait_ms FROM sys.dm_os_wait_stats WHERE wait_type NOT IN (" + msBenignWaitSQL + ")")
	if err != nil {
		return msResult{}, err
	}
	row := Row{}
	if len(rows) > 0 {
		row = rows[0]
	}
	raw := map[string]int64{"signal_ms": row.Int("signal_ms"), "wait_ms": row.Int("wait_ms")}
	sig, tot, window := raw["signal_ms"], raw["wait_ms"], "自启动以来"
	var prev map[string]int64
	if c.prev("cpu.signal_wait", &prev) && prev != nil {
		sig, tot, window = sig-prev["signal_ms"], tot-prev["wait_ms"], "与上一份报告的差值"
	}
	pct := 0.0
	if tot > 0 {
		pct = r1(100 * float64(sig) / float64(tot))
	}
	level := LevelNormal
	if pct > 25 {
		level = LevelAlert
	}
	return msResult{Level: level, Value: fmt.Sprintf("[%s] 信号等待 %v%%（%s / %s ms）", window, pct, fmtNum(sig), fmtNum(tot)), Threshold: ">25% 告警（CPU 排队）", Raw: raw}, nil
}

func chkRunnable(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT AVG(CONVERT(float, runnable_tasks_count)) AS avg_r, MAX(runnable_tasks_count) AS max_r, COUNT(*) AS n FROM sys.dm_os_schedulers WHERE status = 'VISIBLE ONLINE'")
	if err != nil {
		return msResult{}, err
	}
	r := Row{}
	if len(rows) > 0 {
		r = rows[0]
	}
	avg := math.Round(r.Float("avg_r")*100) / 100
	level := LevelNormal
	if avg > 2 {
		level = LevelAlert
	}
	return msResult{Level: level, Value: fmt.Sprintf("%d 个调度器，平均可运行任务 %v，最大 %d", r.Int("n"), avg, r.Int("max_r")), Threshold: "平均 >2 告警"}, nil
}

func topSQLResult(c *msContext, order, where, summary string, pick func(Row) string) (msResult, error) {
	rows, err := c.query(topSQLQuery(order, where))
	if err != nil {
		return msResult{}, err
	}
	first := "0"
	if len(rows) > 0 {
		first = pick(rows[0])
	}
	return msResult{Level: LevelNormal, Value: fmt.Sprintf(summary, len(rows), first), Threshold: "展示", Detail: tbl(topSQLCols, topSQLRows(rows))}, nil
}

func chkCPUTopSQL(c *msContext) (msResult, error) {
	return topSQLResult(c, "qs.total_worker_time", "", "共 %d 条，按累计 CPU 排序；首位累计 CPU %s ms", func(r Row) string { return fmtNum(r["total_cpu_ms"]) })
}

func chkReadsTopSQL(c *msContext) (msResult, error) {
	return topSQLResult(c, "qs.total_logical_reads", "", "共 %d 条，按累计逻辑读排序；首位 %s 页", func(r Row) string { return fmtNum(r["total_logical_reads"]) })
}

func chkTopSQLExec(c *msContext) (msResult, error) {
	return topSQLResult(c, "qs.execution_count", "", "共 %d 条，按执行次数排序；首位执行 %s 次", func(r Row) string { return fmtNum(r["execution_count"]) })
}

func chkTopSQLAvg(c *msContext) (msResult, error) {
	return topSQLResult(c, "qs.total_elapsed_time / qs.execution_count", "WHERE qs.execution_count >= 10", "共 %d 条；首位平均耗时 %s ms", func(r Row) string {
		return fmtNum(r.Int("total_elapsed_ms") / max(r.Int("execution_count"), 1))
	})
}

func chkPLE(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT RTRIM(counter_name) AS name, cntr_value FROM sys.dm_os_performance_counters WHERE object_name LIKE '%Buffer Manager%' AND RTRIM(counter_name) IN ('Page life expectancy','Database pages')")
	if err != nil {
		return msResult{}, err
	}
	vals := map[string]int64{}
	for _, r := range rows {
		vals[r.Str("name")] = r.Int("cntr_value")
	}
	ple := vals["Page life expectancy"]
	bufferGB := float64(vals["Database pages"]) * 8 / 1024 / 1024
	threshold := int64(300 * math.Max(bufferGB, 4) / 4)
	level := LevelNormal
	if ple < threshold {
		level = LevelAlert
	}
	return msResult{Level: level, Value: fmt.Sprintf("PLE %s s，缓冲池数据页 %.1f GB", fmtNum(ple), bufferGB), Threshold: fmt.Sprintf("< %s s 告警（300s × 每4GB）", fmtNum(threshold))}, nil
}

func chkGrants(c *msContext) (msResult, error) {
	p, err := c.scalar("SELECT COUNT(*) FROM sys.dm_exec_query_memory_grants WHERE grant_time IS NULL")
	if err != nil {
		return msResult{}, err
	}
	g, err := c.scalar("SELECT COUNT(*) FROM sys.dm_exec_query_memory_grants WHERE grant_time IS NOT NULL")
	if err != nil {
		return msResult{}, err
	}
	level := LevelNormal
	if toInt(p) > 0 {
		level = LevelAlert
	}
	return msResult{Level: level, Value: fmt.Sprintf("等待授予 %d 个，已授予 %d 个", toInt(p), toInt(g)), Threshold: "等待授予 >0 告警"}, nil
}

func chkBufferByDB(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT TOP 10 ISNULL(DB_NAME(database_id), 'ResourceDb') AS db_name, COUNT(*)/128 AS mb FROM sys.dm_os_buffer_descriptors GROUP BY database_id ORDER BY mb DESC")
	if err != nil {
		return msResult{}, err
	}
	var detail [][]any
	var parts []string
	for i, r := range rows {
		detail = append(detail, []any{r.Str("db_name"), fmtNum(r["mb"])})
		if i < 5 {
			parts = append(parts, fmt.Sprintf("%s %s MB", r.Str("db_name"), fmtNum(r["mb"])))
		}
	}
	return msResult{Level: LevelNormal, Value: strings.Join(parts, " · "), Threshold: "展示", Detail: tbl([]string{"库", "占用 MB"}, detail)}, nil
}

func chkFileLatency(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT vfs.database_id, vfs.file_id, DB_NAME(vfs.database_id) AS db_name, mf.name AS file_name, mf.type_desc, vfs.num_of_reads, vfs.io_stall_read_ms, vfs.num_of_writes, vfs.io_stall_write_ms, " +
		"vfs.num_of_bytes_read, vfs.num_of_bytes_written FROM sys.dm_io_virtual_file_stats(NULL, NULL) vfs JOIN sys.master_files mf ON mf.database_id = vfs.database_id AND mf.file_id = vfs.file_id")
	if err != nil {
		return msResult{}, err
	}
	raw := map[string][6]int64{}
	for _, r := range rows {
		raw[fmt.Sprintf("%d:%d", r.Int("database_id"), r.Int("file_id"))] = [6]int64{r.Int("num_of_reads"), r.Int("io_stall_read_ms"), r.Int("num_of_writes"), r.Int("io_stall_write_ms"), r.Int("num_of_bytes_read"), r.Int("num_of_bytes_written")}
	}
	var prev map[string][6]int64
	havePrev := c.prev("io.file_latency", &prev) && prev != nil
	window := "自启动以来"
	if havePrev {
		window = "与上一份报告的差值"
	}
	type line struct {
		cells []any
		flag  string
		isLog bool
		rlat  float64
		wlat  float64
	}
	var lines []line
	level := LevelNormal
	var alerts []string
	for _, r := range rows {
		key := fmt.Sprintf("%d:%d", r.Int("database_id"), r.Int("file_id"))
		cur := raw[key]
		var base [6]int64
		if havePrev {
			base = prev[key]
		}
		var d [6]int64
		for i := range d {
			d[i] = cur[i] - base[i]
		}
		if d[0] < 0 || d[2] < 0 { // 文件被重建等异常情况，退回累计值
			d = cur
		}
		reads, writes := d[0], d[2]
		var rLat, wLat float64
		if reads > 0 {
			rLat = r1(float64(d[1]) / float64(reads))
		}
		if writes > 0 {
			wLat = r1(float64(d[3]) / float64(writes))
		}
		isLog := r.Str("type_desc") == "LOG"
		flag := ""
		switch {
		case isLog && writes > 100 && wLat > 5:
			flag, level = "告警", LevelAlert
			alerts = append(alerts, fmt.Sprintf("%s.%s 日志写 %v ms", r.Str("db_name"), r.Str("file_name"), wLat))
		case !isLog && ((reads > 100 && rLat > 20) || (writes > 100 && wLat > 10)):
			flag = "关注"
			level = max(level, LevelAttention)
		}
		kind := "数据"
		if isLog {
			kind = "日志"
		}
		lines = append(lines, line{[]any{r.Str("db_name") + " · " + r.Str("file_name"), kind, rLat, wLat, fmtNum(reads), fmtNum(writes), fmtNum(d[4] / 1048576), fmtNum(d[5] / 1048576), flag}, flag, isLog, rLat, wLat})
	}
	sort.SliceStable(lines, func(i, j int) bool {
		a, b := lines[i], lines[j]
		if (a.flag == "") != (b.flag == "") {
			return a.flag != ""
		}
		if a.isLog {
			return a.wlat > b.wlat
		}
		return a.rlat > b.rlat
	})
	if len(lines) > 60 {
		lines = lines[:60]
	}
	var detail [][]any
	for _, l := range lines {
		detail = append(detail, l.cells)
	}
	problem := "延迟正常"
	switch {
	case len(alerts) > 0:
		a := alerts
		if len(a) > 3 {
			a = a[:3]
		}
		problem = fmt.Sprintf("%d 个日志文件写延迟超标：%s", len(alerts), strings.Join(a, "；"))
	case level == LevelAttention:
		problem = "有数据文件延迟偏高"
	}
	return msResult{Level: level, Value: fmt.Sprintf("[%s] %d 个文件，%s", window, len(rows), problem), Threshold: "数据读 >20ms / 写 >10ms 关注；日志写 >5ms 告警",
		Detail: tbl([]string{"库 · 文件", "类型", "读延迟 ms", "写延迟 ms", "读次数", "写次数", "读 MB", "写 MB", "结论"}, detail), Raw: raw}, nil
}

func chkPendingIO(c *msContext) (msResult, error) {
	n, err := c.scalar("SELECT COUNT(*) FROM sys.dm_io_pending_io_requests")
	if err != nil {
		return msResult{}, err
	}
	level := LevelNormal
	if toInt(n) > 0 {
		level = LevelAttention
	}
	return msResult{Level: level, Value: fmt.Sprintf("待完成 IO 请求 %d 个", toInt(n)), Threshold: ">0 关注"}, nil
}

func chkTempdb(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT COUNT(*) AS files, MIN(size)/128 AS min_mb, MAX(size)/128 AS max_mb FROM sys.master_files WHERE database_id = 2 AND type = 0")
	if err != nil {
		return msResult{}, err
	}
	r := Row{}
	if len(rows) > 0 {
		r = rows[0]
	}
	expected := min(max(c.cpuCount, 1), 8)
	files := r.Int("files")
	level := LevelNormal
	if files != expected || r.Int("min_mb") != r.Int("max_mb") {
		level = LevelAttention
	}
	return msResult{Level: level, Value: fmt.Sprintf("%d 个数据文件，大小 %s ~ %s MB；CPU %d 核", files, fmtNum(r["min_mb"]), fmtNum(r["max_mb"]), c.cpuCount), Threshold: fmt.Sprintf("文件数 = min(核心数, 8) = %d 且大小一致", expected)}, nil
}

func chkBlocking(c *msContext) (msResult, error) {
	waiting, err := c.query("SELECT r.session_id, r.blocking_session_id, r.wait_type, r.wait_time, r.wait_resource, DB_NAME(r.database_id) AS db_name, s.login_name, s.host_name, s.program_name, " +
		"SUBSTRING(t.text, (r.statement_start_offset/2)+1, ((CASE r.statement_end_offset WHEN -1 THEN DATALENGTH(t.text) ELSE r.statement_end_offset END - r.statement_start_offset)/2)+1) AS stmt " +
		"FROM sys.dm_exec_requests r JOIN sys.dm_exec_sessions s ON s.session_id = r.session_id OUTER APPLY sys.dm_exec_sql_text(r.sql_handle) t WHERE r.blocking_session_id <> 0 ORDER BY r.wait_time DESC")
	if err != nil {
		return msResult{}, err
	}
	if len(waiting) == 0 {
		return msResult{Level: LevelNormal, Value: "无阻塞", Threshold: "等待 >30s 告警"}, nil
	}
	heads, err := c.query("SELECT s.session_id, s.login_name, s.host_name, s.program_name, s.status, DATEDIFF(SECOND, s.last_request_end_time, GETDATE()) AS idle_seconds, t.text AS last_sql " +
		"FROM sys.dm_exec_sessions s LEFT JOIN sys.dm_exec_connections c ON c.session_id = s.session_id OUTER APPLY sys.dm_exec_sql_text(c.most_recent_sql_handle) t " +
		"WHERE s.session_id IN (SELECT blocking_session_id FROM sys.dm_exec_requests WHERE blocking_session_id <> 0) AND s.session_id NOT IN (SELECT session_id FROM sys.dm_exec_requests WHERE blocking_session_id <> 0)")
	if err != nil {
		return msResult{}, err
	}
	var detail [][]any
	var ids []string
	for _, h := range heads {
		ids = append(ids, h.Str("session_id"))
		detail = append(detail, []any{"头阻塞者", h["session_id"], "", "", fmt.Sprintf("%s @ %s (%s)", h.Str("login_name"), h.Str("host_name"), h.Str("program_name")), fmt.Sprintf("%s，空闲 %ds", h.Str("status"), h.Int("idle_seconds")), truncSQL(h["last_sql"])})
	}
	for i, w := range waiting {
		if i >= 30 {
			break
		}
		detail = append(detail, []any{"被阻塞", w["session_id"], w["blocking_session_id"], fmt.Sprintf("%s %s ms", w.Str("wait_type"), fmtNum(w["wait_time"])), fmt.Sprintf("%s @ %s (%s)", w.Str("login_name"), w.Str("host_name"), w.Str("program_name")), w.Str("db_name") + " " + w.Str("wait_resource"), truncSQL(w["stmt"])})
	}
	longest := waiting[0].Int("wait_time")
	level := LevelAttention
	if longest > 30000 {
		level = LevelAlert
	}
	return msResult{Level: level, Value: fmt.Sprintf("%d 个会话被阻塞，头阻塞者 SPID %s，最长等待 %s", len(waiting), strings.Join(ids, "/"), fmtDuration(longest/1000)), Threshold: "等待 >30s 告警",
		Detail: tbl([]string{"角色", "SPID", "被谁阻塞", "等待", "登录/主机/程序", "资源", "SQL"}, detail)}, nil
}

func chkLongTran(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT TOP 20 st.session_id, at.name AS tran_name, at.transaction_begin_time, DATEDIFF(SECOND, at.transaction_begin_time, GETDATE()) AS seconds, s.login_name, s.host_name, s.program_name, s.status, t.text AS last_sql " +
		"FROM sys.dm_tran_active_transactions at JOIN sys.dm_tran_session_transactions st ON st.transaction_id = at.transaction_id JOIN sys.dm_exec_sessions s ON s.session_id = st.session_id " +
		"LEFT JOIN sys.dm_exec_connections c ON c.session_id = s.session_id OUTER APPLY sys.dm_exec_sql_text(c.most_recent_sql_handle) t WHERE st.is_user_transaction = 1 ORDER BY at.transaction_begin_time")
	if err != nil {
		return msResult{}, err
	}
	if len(rows) == 0 {
		return msResult{Level: LevelNormal, Value: "无用户事务", Threshold: ">5 分钟告警"}, nil
	}
	var detail [][]any
	for _, r := range rows {
		detail = append(detail, []any{r["session_id"], r.Str("tran_name"), fmtDT(r["transaction_begin_time"]), fmtDuration(r.Int("seconds")), fmt.Sprintf("%s @ %s (%s)", r.Str("login_name"), r.Str("host_name"), r.Str("program_name")), r.Str("status"), truncSQL(r["last_sql"])})
	}
	longest := rows[0].Int("seconds")
	level := LevelNormal
	if longest > 300 {
		level = LevelAlert
	}
	return msResult{Level: level, Value: fmt.Sprintf("%d 个用户事务，最长 SPID %s 已开启 %s", len(rows), rows[0].Str("session_id"), fmtDuration(longest)), Threshold: ">5 分钟告警",
		Detail: tbl([]string{"SPID", "事务名", "开始时间", "已开启", "登录/主机/程序", "会话状态", "最近 SQL"}, detail)}, nil
}

func chkDeadlocks(c *msContext) (msResult, error) {
	v, err := c.scalar("SELECT cntr_value FROM sys.dm_os_performance_counters WHERE RTRIM(counter_name) = 'Number of Deadlocks/sec' AND RTRIM(instance_name) = '_Total'")
	if err != nil {
		return msResult{}, err
	}
	total := toInt(v)
	n, window := total, "自启动以来"
	var prev map[string]int64
	if c.prev("blocking.deadlocks", &prev) && prev != nil {
		n, window = total-prev["total"], "与上一份报告的差值"
	}
	level := LevelNormal
	if n > 0 {
		level = LevelAttention
	}
	return msResult{Level: level, Value: fmt.Sprintf("[%s] 死锁 %s 次", window, fmtNum(n)), Threshold: ">0 关注", Raw: map[string]int64{"total": total}}, nil
}

func chkMissingIndex(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT TOP 20 mid.statement AS table_name, mid.equality_columns, mid.inequality_columns, mid.included_columns, migs.user_seeks, migs.avg_user_impact, migs.avg_total_user_cost, migs.last_user_seek, " +
		"CONVERT(decimal(18,2), migs.user_seeks * migs.avg_total_user_cost * migs.avg_user_impact / 100.0) AS score FROM sys.dm_db_missing_index_groups mig " +
		"JOIN sys.dm_db_missing_index_group_stats migs ON migs.group_handle = mig.index_group_handle JOIN sys.dm_db_missing_index_details mid ON mid.index_handle = mig.index_handle " +
		"WHERE mid.database_id = DB_ID() ORDER BY score DESC")
	if err != nil {
		return msResult{}, err
	}
	if len(rows) == 0 {
		return msResult{Level: LevelNormal, Value: "无缺失索引建议", Threshold: "impact >80 且 seeks >1000 关注"}, nil
	}
	var detail [][]any
	strong := 0
	for _, r := range rows {
		detail = append(detail, []any{r.Str("table_name"), r.Str("equality_columns"), r.Str("inequality_columns"), r.Str("included_columns"), fmtNum(r["user_seeks"]), r1(r.Float("avg_user_impact")), r.Float("score"), fmtDT(r["last_user_seek"])})
		if r.Float("avg_user_impact") > 80 && r.Int("user_seeks") > 1000 {
			strong++
		}
	}
	level := LevelNormal
	if strong > 0 {
		level = LevelAttention
	}
	return msResult{Level: level, Value: fmt.Sprintf("%d 条建议，%d 条 impact >80 且 seeks >1000（仅供参考，勿直接照建）", len(rows), strong), Threshold: "impact >80 且 seeks >1000 关注",
		Detail: tbl([]string{"表", "等值列", "范围列", "包含列", "seeks", "impact %", "得分", "最近使用"}, detail)}, nil
}

func chkUnusedIndex(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT TOP 20 OBJECT_SCHEMA_NAME(i.object_id) + '.' + OBJECT_NAME(i.object_id) AS table_name, i.name AS index_name, ISNULL(u.user_updates, 0) AS updates, ps.row_count FROM sys.indexes i " +
		"JOIN sys.objects o ON o.object_id = i.object_id AND o.type = 'U' AND o.is_ms_shipped = 0 " +
		"LEFT JOIN sys.dm_db_index_usage_stats u ON u.database_id = DB_ID() AND u.object_id = i.object_id AND u.index_id = i.index_id " +
		"OUTER APPLY (SELECT SUM(row_count) AS row_count FROM sys.dm_db_partition_stats p WHERE p.object_id = i.object_id AND p.index_id = i.index_id) ps " +
		"WHERE i.type_desc = 'NONCLUSTERED' AND i.is_primary_key = 0 AND i.is_unique_constraint = 0 AND i.is_disabled = 0 " +
		"AND ISNULL(u.user_seeks, 0) + ISNULL(u.user_scans, 0) + ISNULL(u.user_lookups, 0) = 0 AND ISNULL(u.user_updates, 0) > 10000 ORDER BY updates DESC")
	if err != nil {
		return msResult{}, err
	}
	if len(rows) == 0 {
		return msResult{Level: LevelNormal, Value: "无写多读少的无用索引", Threshold: "无读取且写入 >1 万次 关注"}, nil
	}
	var detail [][]any
	for _, r := range rows {
		detail = append(detail, []any{r.Str("table_name"), r.Str("index_name"), fmtNum(r["updates"]), fmtNum(r["row_count"])})
	}
	return msResult{Level: LevelAttention, Value: fmt.Sprintf("%d 个索引自启动以来没有读取、写入却超过 1 万次（实例重启后统计会清零，删除前请确认）", len(rows)), Threshold: "无读取且写入 >1 万次 关注",
		Detail: tbl([]string{"表", "索引", "写入次数", "行数"}, detail)}, nil
}

func chkDBFiles(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT f.name, f.type_desc, f.physical_name, CONVERT(decimal(18,1), f.size/128.0) AS size_mb, CONVERT(decimal(18,1), FILEPROPERTY(f.name, 'SpaceUsed')/128.0) AS used_mb, f.growth, f.is_percent_growth, f.max_size, " +
		"CONVERT(decimal(18,1), vs.total_bytes/1048576.0) AS vol_total_mb, CONVERT(decimal(18,1), vs.available_bytes/1048576.0) AS vol_free_mb, vs.volume_mount_point FROM sys.database_files f CROSS APPLY sys.dm_os_volume_stats(DB_ID(), f.file_id) vs")
	if err != nil {
		return msResult{}, err
	}
	level := LevelNormal
	var notes []string
	var detail [][]any
	var size float64
	for _, r := range rows {
		pct := toInt(r["is_percent_growth"]) != 0
		growth := fmt.Sprintf("%d MB", r.Int("growth")/128)
		if pct {
			growth = fmt.Sprintf("%d%%", r.Int("growth"))
		}
		freePct := 0.0
		if t := r.Float("vol_total_mb"); t > 0 {
			freePct = r1(100 * r.Float("vol_free_mb") / t)
		}
		flag := ""
		switch {
		case freePct < 10:
			flag, level = "告警", LevelAlert
			notes = append(notes, fmt.Sprintf("%s 剩余 %v%%", r.Str("volume_mount_point"), freePct))
		case pct || r.Int("growth")/128 < 64:
			flag = "关注"
			level = max(level, LevelAttention)
		}
		maxSize := fmt.Sprintf("%d MB", r.Int("max_size")/128)
		if m := r.Int("max_size"); m == -1 || m == 268435456 {
			maxSize = "不限"
		}
		kind := "数据"
		if r.Str("type_desc") == "LOG" {
			kind = "日志"
		}
		size += r.Float("size_mb")
		detail = append(detail, []any{r.Str("name"), kind, r.Float("size_mb"), r.Float("used_mb"), growth, maxSize, fmt.Sprintf("%s 剩余 %s MB (%v%%)", r.Str("volume_mount_point"), fmtNum(int64(r.Float("vol_free_mb"))), freePct), flag})
	}
	value := fmt.Sprintf("%d 个文件共 %s MB", len(rows), fmtNum(int64(size)))
	if len(notes) > 0 {
		value += "；" + strings.Join(notes, "；")
	}
	if level == LevelAttention {
		value += "；存在百分比增长或增量 <64MB"
	}
	return msResult{Level: level, Value: value, Threshold: "百分比增长或增量 <64MB 关注；卷剩余 <10% 告警", Detail: tbl([]string{"文件", "类型", "大小 MB", "已用 MB", "自动增长", "上限", "所在卷", "结论"}, detail)}, nil
}

func chkDBLog(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT log_reuse_wait_desc, recovery_model_desc FROM sys.databases WHERE database_id = DB_ID()")
	if err != nil {
		return msResult{}, err
	}
	if len(rows) == 0 {
		return msResult{}, fmt.Errorf("读不到库 %s 的信息", c.db)
	}
	r := rows[0]
	level := LevelNormal
	if r.Str("log_reuse_wait_desc") != "NOTHING" {
		level = LevelAttention
	}
	value := fmt.Sprintf("恢复模式 %s，日志重用等待 %s", r.Str("recovery_model_desc"), r.Str("log_reuse_wait_desc"))
	if c.version >= 13 {
		n, err := c.scalar("SELECT COUNT(*) FROM sys.dm_db_log_info(DB_ID())")
		if err != nil {
			return msResult{}, err
		}
		if toInt(n) > 1000 {
			level = LevelAlert
		}
		value += fmt.Sprintf("，VLF %s 个", fmtNum(n))
	} else {
		value += "，VLF 需 2016 SP2+"
	}
	return msResult{Level: level, Value: value, Threshold: "重用等待≠NOTHING 关注；VLF >1000 告警"}, nil
}

func chkDBBackup(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT type, MAX(backup_finish_date) AS last_finish FROM msdb.dbo.backupset WHERE database_name = @p1 GROUP BY type", c.db)
	if err != nil {
		return msResult{}, err
	}
	last := map[string]time.Time{}
	for _, r := range rows {
		last[r.Str("type")] = toTime(r["last_finish"])
	}
	model, err := c.scalar("SELECT recovery_model_desc FROM sys.databases WHERE database_id = DB_ID()")
	if err != nil {
		return msResult{}, err
	}
	now := c.now()
	level := LevelNormal
	var notes []string
	full := last["D"]
	if full.IsZero() || now.Sub(full) >= 7*24*time.Hour {
		level = LevelAlert
		if full.IsZero() {
			notes = append(notes, "完整备份 从未")
		} else {
			notes = append(notes, fmt.Sprintf("完整备份 距今 %d 天", int(now.Sub(full).Hours()/24)))
		}
	}
	if toStr(model) == "FULL" {
		lg := last["L"]
		switch {
		case lg.IsZero():
			level = LevelAlert
			notes = append(notes, "日志备份 从未，日志会持续增长")
		case now.Sub(lg) > 24*time.Hour:
			level = LevelAlert
			notes = append(notes, fmt.Sprintf("日志备份 距今 %.0f 小时，日志会持续增长", now.Sub(lg).Hours()))
		case now.Sub(lg) > time.Hour:
			level = max(level, LevelAttention)
			notes = append(notes, fmt.Sprintf("日志备份距今 %.1f 小时", now.Sub(lg).Hours()))
		}
	}
	show := func(t time.Time) string {
		if t.IsZero() {
			return "无"
		}
		return fmtDT(t)
	}
	value := fmt.Sprintf("恢复模式 %s；完整 %s，差异 %s，日志 %s", toStr(model), show(full), show(last["I"]), show(last["L"]))
	if len(notes) > 0 {
		value += "；" + strings.Join(notes, "；")
	}
	return msResult{Level: level, Value: value, Threshold: "完整 >7 天告警；FULL 模式日志 >1h 关注、>24h 告警"}, nil
}

func chkDBOptions(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT is_auto_shrink_on, page_verify_option_desc, is_auto_create_stats_on, is_auto_update_stats_on, compatibility_level, is_read_only FROM sys.databases WHERE database_id = DB_ID()")
	if err != nil {
		return msResult{}, err
	}
	if len(rows) == 0 {
		return msResult{}, fmt.Errorf("读不到库 %s 的信息", c.db)
	}
	r := rows[0]
	var problems []string
	shrink := toInt(r["is_auto_shrink_on"]) != 0
	stats := toInt(r["is_auto_create_stats_on"]) != 0 && toInt(r["is_auto_update_stats_on"]) != 0
	if shrink {
		problems = append(problems, "AUTO_SHRINK 已开启")
	}
	if pv := r.Str("page_verify_option_desc"); pv != "CHECKSUM" {
		problems = append(problems, "PAGE_VERIFY="+pv)
	}
	if !stats {
		problems = append(problems, "自动创建/更新统计信息已关闭")
	}
	onOff := func(b bool) string {
		if b {
			return "开"
		}
		return "关"
	}
	value := fmt.Sprintf("兼容级别 %d，PAGE_VERIFY %s，AUTO_SHRINK %s，自动统计 %s", r.Int("compatibility_level"), r.Str("page_verify_option_desc"), onOff(shrink), onOff(stats))
	if toInt(r["is_read_only"]) != 0 {
		value += "，只读"
	}
	level := LevelNormal
	if len(problems) > 0 {
		level = LevelAlert
		value += "；" + strings.Join(problems, "、")
	}
	return msResult{Level: level, Value: value, Threshold: "AUTO_SHRINK 开 / PAGE_VERIFY≠CHECKSUM / 自动统计关 → 告警"}, nil
}

func chkJobs(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT TOP 20 j.name AS job_name, h.run_date, h.run_time, h.message FROM msdb.dbo.sysjobhistory h JOIN msdb.dbo.sysjobs j ON j.job_id = h.job_id WHERE h.step_id = 0 AND h.run_status = 0 " +
		"AND h.run_date >= CONVERT(int, CONVERT(varchar(8), DATEADD(DAY, -7, GETDATE()), 112)) ORDER BY h.run_date DESC, h.run_time DESC")
	if err != nil {
		return msResult{}, err
	}
	if len(rows) == 0 {
		return msResult{Level: LevelNormal, Value: "无失败作业", Threshold: "存在关注"}, nil
	}
	var detail [][]any
	for _, r := range rows {
		msg := []rune(r.Str("message"))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		detail = append(detail, []any{r.Str("job_name"), r["run_date"], fmt.Sprintf("%06d", r.Int("run_time")), string(msg)})
	}
	plus := ""
	if len(rows) == 20 {
		plus = "+"
	}
	return msResult{Level: LevelAttention, Value: fmt.Sprintf("%d%s 次作业失败，最近 %s", len(rows), plus, rows[0].Str("job_name")), Threshold: "存在关注", Detail: tbl([]string{"作业", "日期", "时间", "信息"}, detail)}, nil
}

func chkDumps(c *msContext) (msResult, error) {
	rows, err := c.query("SELECT TOP 20 filename, creation_time, size_in_bytes FROM sys.dm_server_memory_dumps ORDER BY creation_time DESC")
	if err != nil {
		return msResult{}, err
	}
	if len(rows) == 0 {
		return msResult{Level: LevelNormal, Value: "无", Threshold: "7 天内告警，更早关注"}, nil
	}
	latest := toTime(rows[0]["creation_time"])
	recent := !latest.IsZero() && c.now().Sub(latest) <= 7*24*time.Hour
	var detail [][]any
	for _, r := range rows {
		detail = append(detail, []any{r.Str("filename"), fmtDT(r["creation_time"]), r1(r.Float("size_in_bytes") / 1048576)})
	}
	level := LevelAttention
	if recent {
		level = LevelAlert
	}
	return msResult{Level: level, Value: fmt.Sprintf("%d 个转储文件，最近 %s", len(rows), fmtDT(latest)), Threshold: "7 天内告警，更早关注", Detail: tbl([]string{"文件", "时间", "大小 MB"}, detail)}, nil
}

// isPermissionError 判断错误是否是目标实例账号权限不足。
func isPermissionError(err error) bool {
	s := err.Error()
	for _, code := range []string{"(229)", "(297)", "(300)", "(262)", "(916)", "(15247)"} {
		if strings.Contains(s, code) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(s), "permission") || strings.Contains(s, "权限") // i18n-ignore 与错误文本做匹配
}
