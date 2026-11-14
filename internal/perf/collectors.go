// @author: kevinkelly
// @license: Apache Licence
// @file: collectors.go
// @time: 2026/10/28

package perf

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	drv "github.com/go-sql-driver/mysql"

	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/sqlx"
)

// ---- 采集来源（接口 + 实现） ----
//
// 一个 Collector 把某个实例在 [Start, End) 内的慢查询聚合成 Digest 列表。接口注入的原因：真实实现要读元库的慢日志表
// 或连接目标实例，单测里用假实现。采集器不写工单：入库由 Service.Ingest 完成，成功后再调用 Commit 推进游标。

// CollectRequest 是一次采集的输入。
type CollectRequest struct {
	Config   *CollectConfig
	Instance *model.Instance
	Start    time.Time
	End      time.Time
	Backfill bool      // 补采/试采：使用指定窗口，不依赖也不移动游标
	Now      time.Time // 当前时间（用来换算数据库服务器时区），为零时取系统时间
	RowLimit int       // 单次最多拉取的明细条数，防止把慢日志表拖垮
}

// CollectResult 是一次采集的输出。
type CollectResult struct {
	Digests []Digest
	// Commit 在入库成功后调用，用来提交增量游标；为空表示没有游标。
	Commit func() error
	// Note 是给人看的补充说明（如“首次采集只建立游标”）。
	Note string
}

// Collector 是性能数据来源。
type Collector interface {
	Collect(ctx context.Context, req CollectRequest) (*CollectResult, error)
}

const defaultRowLimit = 50000

// SlowLogRow 是元库 dbt_slow_logs 表的一行（平台原有的慢日志采集任务写入）。
type SlowLogRow struct {
	ID           uint
	DBName       string
	SQLText      string
	QueryTimeSec float64
	RowsExamined float64
	RowsSent     float64
	StartTime    *time.Time
}

// SlowSample 是慢日志里某条 SQL 最慢的一次。
type SlowSample struct {
	DBName  string
	SQLText string
}

// SlowStore 是元库里慢日志数据的读取（只读，不改表结构）。
type SlowStore interface {
	// SlowLogsAfter 读 dbt_slow_logs 里 id 大于 afterID 的明细（按 id 升序）。
	SlowLogsAfter(instanceID, afterID uint, limit int) ([]SlowLogRow, error)
	// SlowLogsCreatedBetween 读入库时间(created_at)在 [start, end) 内的明细（按 id 升序）。
	SlowLogsCreatedBetween(instanceID uint, start, end time.Time, limit int) ([]SlowLogRow, error)
	// MaxSlowLogID 返回入库时间早于 before 的最大 id，没有返回 0。
	MaxSlowLogID(instanceID uint, before time.Time) (uint, error)
	// PtDigests 读 pt-query-digest 汇总表（dbt_mysql_slow_query_review_history）里 ts_min 在 [start, end) 的聚合，hostPort 为 host:port。
	PtDigests(hostPort string, start, end time.Time) ([]Digest, error)
	// SlowLogByMD5 按 sql_md5 找最慢的一条样例，以及 since 之后按天的聚合；没有明细返回 nil。
	SlowLogByMD5(instanceID uint, md5, db string, since time.Time) (*SlowSample, []Digest, error)
}

// QueryRunner 在目标实例上执行只读查询，返回的列名统一小写。
type QueryRunner interface {
	Query(ctx context.Context, inst *model.Instance, db, sql string, args ...any) ([]map[string]any, error)
}

// DefaultCollectors 返回 8 种采集方式的真实实现：平台慢日志与 pt-query-digest 读元库，其余通过 database/sql 连目标实例。
func DefaultCollectors(repo Repo) map[string]Collector {
	r := &mysqlRunner{repo: repo}
	return map[string]Collector{
		CollPlatformSlow: &platformSlowLogs{repo: repo},
		CollPtDigest:     &ptDigest{repo: repo},
		CollPerfSchema:   &perfSchema{run: r, repo: repo},
		CollSlowLogTable: &slowLogTable{run: r},
		CollDorisAudit:   &dorisAudit{run: r},
		CollPolarDBXSum:  &polardbxSummary{run: r},
		CollPolarDBXSlow: &polardbxShowSlow{run: r},
		CollCustomSQL:    &customSQL{run: r},
	}
}

// ---- 聚合 ----

// aggregator 把逐条慢日志按 (库, 指纹) 聚合成 Digest；样例保留最慢的一条，便于 EXPLAIN 复现。
type aggregator struct{ data map[string]*Digest }

func newAggregator() *aggregator { return &aggregator{data: map[string]*Digest{}} }

func (a *aggregator) add(db, sqlText string, querySec, rowsExamined, rowsSent float64, ts *time.Time) {
	if strings.TrimSpace(sqlText) == "" {
		return
	}
	fp := Fingerprint(sqlText)
	if fp == "" {
		return
	}
	d := &Digest{DBName: db, SampleSQL: sqlText}
	d.Normalize()
	key := d.DBName + "\x00" + fp
	cur := a.data[key]
	if cur == nil {
		d.Fingerprint, d.Checksum, d.SampleReal = fp, Checksum(fp), true
		d.FirstSeen, d.LastSeen = ts, ts
		cur = d
		a.data[key] = cur
	}
	ms := querySec * 1000
	if ms >= cur.MaxMs {
		cur.SampleSQL, cur.MaxMs = sqlText, ms
	}
	cur.ExecCount++
	cur.TotalSec += querySec
	cur.RowsExamined += rowsExamined
	cur.RowsSent += rowsSent
	if ts != nil {
		if cur.FirstSeen == nil || ts.Before(*cur.FirstSeen) {
			cur.FirstSeen = ts
		}
		if cur.LastSeen == nil || ts.After(*cur.LastSeen) {
			cur.LastSeen = ts
		}
	}
}

func (a *aggregator) result() []Digest {
	out := make([]Digest, 0, len(a.data))
	for _, d := range a.data {
		out = append(out, *d)
	}
	return out
}

// ---- 平台慢日志（dbt_slow_logs 表） ----

const (
	slowCursorSchema = "__slow_logs__" // dbt_perf_digest_cursor 里记 dbt_slow_logs 自增 id 游标的固定键
	slowCursorDigest = "id"
)

type platformSlowLogs struct {
	repo interface {
		SlowStore
		GetCursor(instanceID uint, schema, digest string) (*DigestCursor, error)
		SetCursor(c *DigestCursor) error
	}
}

// Collect 按 dbt_slow_logs 的自增 id 往后读（表上只有 instance_id 索引，按 created_at 过滤会扫全实例数据）：
// 首次运行取窗口内入库的明细并记下最大 id；之后只读 id 大于游标的明细，与原采集任务的入库滞后无关，不重不漏；
// 补采/试采按 created_at 取指定窗口，不用也不动游标。游标在入库成功后才提交。
func (c *platformSlowLogs) Collect(_ context.Context, req CollectRequest) (*CollectResult, error) {
	limit := req.RowLimit
	if limit <= 0 {
		limit = defaultRowLimit
	}
	instID := req.Config.InstanceID
	var cur *DigestCursor
	var err error
	if !req.Backfill {
		if cur, err = c.repo.GetCursor(instID, slowCursorSchema, slowCursorDigest); err != nil {
			return nil, err
		}
	}
	var rows []SlowLogRow
	if cur != nil {
		rows, err = c.repo.SlowLogsAfter(instID, uint(cur.CountStar), limit)
	} else {
		rows, err = c.repo.SlowLogsCreatedBetween(instID, req.Start, req.End, limit)
	}
	if err != nil {
		return nil, err
	}
	agg := newAggregator()
	for _, r := range rows {
		agg.add(r.DBName, r.SQLText, r.QueryTimeSec, r.RowsExamined, r.RowsSent, r.StartTime)
	}
	res := &CollectResult{Digests: agg.result()}
	if len(rows) >= limit {
		res.Note = fmt.Sprintf("dbt_slow_logs 本次读取达到上限 %d 行，剩余明细下次继续", limit)
	}
	if req.Backfill {
		return res, nil
	}
	var newCursor uint
	switch {
	case len(rows) > 0:
		newCursor = rows[len(rows)-1].ID
	case cur == nil:
		// 首次运行且窗口内没有明细：游标定位到窗口结束前入库的最大 id（不能取当前最大 id，否则窗口结束后刚入库的明细会被跳过）
		if newCursor, err = c.repo.MaxSlowLogID(instID, req.End); err != nil {
			return nil, err
		}
	default:
		return res, nil
	}
	res.Commit = func() error {
		return c.repo.SetCursor(&DigestCursor{InstanceID: instID, SchemaName: slowCursorSchema, Digest: slowCursorDigest, CountStar: int64(newCursor)})
	}
	return res, nil
}

// ---- pt-query-digest（dbt_mysql_slow_query_review_history 汇总表） ----

type ptDigest struct{ repo SlowStore }

func (c *ptDigest) Collect(_ context.Context, req CollectRequest) (*CollectResult, error) {
	if req.Instance == nil {
		return nil, errors.New("实例不存在")
	}
	ds, err := c.repo.PtDigests(fmt.Sprintf("%s:%d", req.Instance.Host, req.Instance.Port), req.Start, req.End)
	if err != nil {
		return nil, err
	}
	for i := range ds {
		ds[i].SampleReal = true
	}
	return &CollectResult{Digests: ds}, nil
}

// ---- 目标实例上的采集 ----

// serverOffset 数据库服务器时区与本地时区之差（本地 − 服务器）。AWS RDS 等默认 UTC，直接用本地时间过滤会错开 8 小时。
func serverOffset(ctx context.Context, run QueryRunner, inst *model.Instance, now time.Time) time.Duration {
	rows, err := run.Query(ctx, inst, "", "SELECT NOW() AS now_ts")
	if err != nil || len(rows) == 0 {
		slog.Warn("获取服务器时间失败，按同时区处理", "instance", inst.InstanceName, "err", err)
		return 0
	}
	t := toTime(rows[0]["now_ts"])
	if t == nil {
		return 0
	}
	minutes := int(float64(now.Sub(*t).Minutes())/15 + sign(now.Sub(*t))*0.5) // 按 15 分钟取整，去掉网络/执行耗时误差
	return time.Duration(minutes) * 15 * time.Minute
}

func sign(d time.Duration) float64 {
	if d < 0 {
		return -1
	}
	return 1
}

// toTime 把驱动返回的时间值转成 time.Time（parseTime 开启时是 time.Time，否则是字符串）。
func toTime(v any) *time.Time {
	switch x := v.(type) {
	case time.Time:
		return &x
	case []byte:
		return toTime(string(x))
	case string:
		// Go 解析时允许秒后面带小数，layout 里不必写
		if t, err := time.ParseInLocation(tsLayout, x, time.Local); err == nil {
			return &t
		}
	}
	return nil
}

func (r *targetCtx) toLocal(v any) *time.Time {
	t := toTime(v)
	if t == nil {
		return nil
	}
	l := t.Add(r.offset)
	return &l
}

func (r *targetCtx) toServer(t time.Time) time.Time { return t.Add(-r.offset) }

// targetCtx 是一次对目标实例采集的上下文：服务器时区偏移。
type targetCtx struct{ offset time.Duration }

func newTarget(ctx context.Context, run QueryRunner, req CollectRequest) *targetCtx {
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	return &targetCtx{offset: serverOffset(ctx, run, req.Instance, now)}
}

func u64(v any) uint64 {
	switch x := v.(type) {
	case nil:
		return 0
	case uint64:
		return x
	case int64:
		if x < 0 {
			return 0
		}
		return uint64(x)
	case []byte:
		n, _ := strconv.ParseUint(strings.SplitN(string(x), ".", 2)[0], 10, 64)
		return n
	case string:
		n, _ := strconv.ParseUint(strings.SplitN(x, ".", 2)[0], 10, 64)
		return n
	}
	return uint64(num(v))
}

// ---- performance_schema digest ----

type perfSchema struct {
	run  QueryRunner
	repo interface {
		ListCursors(instanceID uint) ([]DigestCursor, error)
		SaveCursors(creates, updates []DigestCursor) error
	}
}

const perfSchemaSQL = `SELECT SCHEMA_NAME, DIGEST, DIGEST_TEXT, COUNT_STAR, SUM_TIMER_WAIT, MAX_TIMER_WAIT,
       SUM_ROWS_EXAMINED, SUM_ROWS_SENT, FIRST_SEEN, LAST_SEEN%s
FROM performance_schema.events_statements_summary_by_digest
WHERE DIGEST IS NOT NULL %s`

// Collect events_statements_summary_by_digest 是自实例启动（或 truncate）以来的累计值，这里用 dbt_perf_digest_cursor
// 记录上次的累计值，两次采集相减得到窗口增量。首次采集只建立游标，不产出数据（试采集不写游标，所以预览为空）。
func (c *perfSchema) Collect(ctx context.Context, req CollectRequest) (*CollectResult, error) {
	inst := req.Instance
	cursors, err := c.repo.ListCursors(req.Config.InstanceID)
	if err != nil {
		return nil, err
	}
	type key struct{ schema, digest string }
	have := map[key]*DigestCursor{}
	for i := range cursors {
		have[key{cursors[i].SchemaName, cursors[i].Digest}] = &cursors[i]
	}
	firstRun := len(have) == 0
	tg := newTarget(ctx, c.run, req)
	where, args := "", []any(nil)
	if !firstRun {
		where, args = "AND LAST_SEEN >= ?", []any{tg.toServer(req.Start)}
	}
	// MySQL 8.0 才有 QUERY_SAMPLE_TEXT（真实样例 SQL，可以 EXPLAIN）
	rows, err := c.run.Query(ctx, inst, "", fmt.Sprintf(perfSchemaSQL, ", QUERY_SAMPLE_TEXT", where), args...)
	if err != nil && strings.Contains(strings.ToUpper(err.Error()), "QUERY_SAMPLE_TEXT") {
		rows, err = c.run.Query(ctx, inst, "", fmt.Sprintf(perfSchemaSQL, "", where), args...)
	}
	if err != nil {
		return nil, err
	}
	var digests []Digest
	var creates, updates []DigestCursor
	for _, r := range rows {
		schema, digest := str(r["schema_name"]), str(r["digest"])
		cnt, timer := int64(num(r["count_star"])), u64(r["sum_timer_wait"])
		rexam, rsent := int64(num(r["sum_rows_examined"])), int64(num(r["sum_rows_sent"]))
		cur := have[key{schema, digest}]
		var dCnt, dRexam, dRsent int64
		var dTimer uint64
		if cur == nil {
			creates = append(creates, DigestCursor{InstanceID: req.Config.InstanceID, SchemaName: schema, Digest: digest, CountStar: cnt,
				SumTimerWait: timer, SumRowsExamined: rexam, SumRowsSent: rsent})
			if firstRun {
				continue
			}
			dCnt, dTimer, dRexam, dRsent = cnt, timer, rexam, rsent
		} else {
			if cnt < cur.CountStar || timer < cur.SumTimerWait { // 被 truncate / 实例重启 / 淘汰后重建
				dCnt, dTimer, dRexam, dRsent = cnt, timer, rexam, rsent
			} else {
				dCnt, dTimer, dRexam, dRsent = cnt-cur.CountStar, timer-cur.SumTimerWait, rexam-cur.SumRowsExamined, rsent-cur.SumRowsSent
			}
			upd := *cur
			upd.CountStar, upd.SumTimerWait, upd.SumRowsExamined, upd.SumRowsSent = cnt, timer, rexam, rsent
			updates = append(updates, upd)
		}
		if dCnt <= 0 {
			continue
		}
		sample := str(r["query_sample_text"])
		real := sample != "" && !strings.HasSuffix(strings.TrimRight(sample, " \t\r\n"), "...")
		text := sample
		if !real {
			text = str(r["digest_text"])
		}
		digests = append(digests, Digest{DBName: schema, SampleSQL: text, SampleReal: real, ExecCount: dCnt, TotalSec: float64(dTimer) / 1e12,
			MaxMs: num(r["max_timer_wait"]) / 1e9, RowsExamined: float64(dRexam), RowsSent: float64(dRsent),
			FirstSeen: tg.toLocal(r["first_seen"]), LastSeen: tg.toLocal(r["last_seen"])})
	}
	res := &CollectResult{Digests: digests}
	if firstRun {
		res.Note = fmt.Sprintf("performance_schema 首次采集，已建立 %d 个游标，从下一次开始产出数据", len(creates))
	}
	if !req.Backfill {
		res.Commit = func() error { return c.repo.SaveCursors(creates, updates) }
	}
	return res, nil
}

// ---- mysql.slow_log 表 ----

type slowLogTable struct{ run QueryRunner }

const slowLogTableSQL = `SELECT start_time, db,
       HOUR(query_time)*3600 + MINUTE(query_time)*60 + SECOND(query_time) + MICROSECOND(query_time)/1000000 AS qt,
       rows_examined, rows_sent, CONVERT(sql_text USING utf8mb4) AS sql_text
FROM mysql.slow_log
WHERE start_time >= ? AND start_time < ?
LIMIT %d`

// Collect mysql.slow_log（CSV 引擎，无索引）。AWS RDS 会自动轮转，Aurora 需自行 CALL mysql.rds_rotate_slow_log。
func (c *slowLogTable) Collect(ctx context.Context, req CollectRequest) (*CollectResult, error) {
	limit := req.RowLimit
	if limit <= 0 {
		limit = defaultRowLimit
	}
	tg := newTarget(ctx, c.run, req)
	rows, err := c.run.Query(ctx, req.Instance, "", fmt.Sprintf(slowLogTableSQL, limit), tg.toServer(req.Start), tg.toServer(req.End))
	if err != nil {
		return nil, err
	}
	agg := newAggregator()
	for _, r := range rows {
		agg.add(str(r["db"]), str(r["sql_text"]), num(r["qt"]), num(r["rows_examined"]), num(r["rows_sent"]), tg.toLocal(r["start_time"]))
	}
	res := &CollectResult{Digests: agg.result()}
	if len(rows) >= limit {
		res.Note = fmt.Sprintf("slow_log 窗口内超过 %d 行，已截断，建议调大 long_query_time 或缩短采集窗口", limit)
	}
	return res, nil
}

// ---- Doris / SelectDB audit_log ----

type dorisAudit struct{ run QueryRunner }

const dorisAuditSQL = "SELECT `time`, db, query_time, scan_rows, return_rows, stmt\n" +
	"FROM __internal_schema.audit_log\n" +
	"WHERE `time` >= ? AND `time` < ?\n" +
	"  AND is_query = 1 AND state <> 'ERR' AND query_time >= ?\n" +
	"LIMIT %d"

func (c *dorisAudit) Collect(ctx context.Context, req CollectRequest) (*CollectResult, error) {
	limit := req.RowLimit
	if limit <= 0 {
		limit = defaultRowLimit
	}
	tg := newTarget(ctx, c.run, req)
	rows, err := c.run.Query(ctx, req.Instance, "", fmt.Sprintf(dorisAuditSQL, limit), tg.toServer(req.Start), tg.toServer(req.End), req.Config.MinAvgMs)
	if err != nil {
		return nil, err
	}
	agg := newAggregator()
	for _, r := range rows {
		agg.add(str(r["db"]), str(r["stmt"]), num(r["query_time"])/1000, num(r["scan_rows"]), num(r["return_rows"]), tg.toLocal(r["time"]))
	}
	return &CollectResult{Digests: agg.result()}, nil
}

// ---- PolarDB-X ----

type polardbxSummary struct{ run QueryRunner }

const polardbxSummarySQL = "SELECT `SCHEMA` AS db_name, TEMPLATE_ID, MAX(SQL_TEMPLATE) AS sql_template, MAX(SQL_SAMPLE) AS sql_sample,\n" +
	"       SUM(`COUNT`) AS cnt, SUM(SUM_RESPONSE_TIME_MS) AS total_ms, MAX(MAX_RESPONSE_TIME_MS) AS max_ms,\n" +
	"       SUM(SUM_PHYSICAL_FETCH_ROWS) AS rows_examined, SUM(SUM_AFFECTED_ROWS) AS rows_sent,\n" +
	"       MIN(FIRST_SEEN) AS first_seen, MAX(LAST_SEEN) AS last_seen\n" +
	"FROM information_schema.STATEMENTS_SUMMARY_HISTORY\n" +
	"WHERE BEGIN_TIME >= ? AND BEGIN_TIME < ?\n" +
	"GROUP BY `SCHEMA`, TEMPLATE_ID"

func (c *polardbxSummary) Collect(ctx context.Context, req CollectRequest) (*CollectResult, error) {
	tg := newTarget(ctx, c.run, req)
	rows, err := c.run.Query(ctx, req.Instance, "", polardbxSummarySQL, tg.toServer(req.Start), tg.toServer(req.End))
	if err != nil {
		return nil, err
	}
	var out []Digest
	for _, r := range rows {
		sample := str(r["sql_sample"])
		text := sample
		if text == "" {
			text = str(r["sql_template"])
		}
		out = append(out, Digest{DBName: str(r["db_name"]), SampleSQL: text, SampleReal: sample != "", ExecCount: int64(num(r["cnt"])),
			TotalSec: num(r["total_ms"]) / 1000, MaxMs: num(r["max_ms"]), RowsExamined: num(r["rows_examined"]), RowsSent: num(r["rows_sent"]),
			FirstSeen: tg.toLocal(r["first_seen"]), LastSeen: tg.toLocal(r["last_seen"])})
	}
	return &CollectResult{Digests: out}, nil
}

type polardbxShowSlow struct{ run QueryRunner }

func (c *polardbxShowSlow) Collect(ctx context.Context, req CollectRequest) (*CollectResult, error) {
	limit := req.RowLimit
	if limit <= 0 {
		limit = defaultRowLimit
	}
	tg := newTarget(ctx, c.run, req)
	// SHOW 语句不支持参数绑定：时间由程序按固定格式生成，limit 是整数，没有注入风险
	q := fmt.Sprintf("SHOW FULL SLOW WHERE START_TIME >= '%s' AND START_TIME < '%s' LIMIT %d",
		tg.toServer(req.Start).Format(tsLayout), tg.toServer(req.End).Format(tsLayout), limit)
	rows, err := c.run.Query(ctx, req.Instance, "", q)
	if err != nil {
		return nil, err
	}
	agg := newAggregator()
	for _, r := range rows {
		db := str(r["db"])
		if db == "" {
			db = str(r["schema"])
		}
		agg.add(db, str(r["sql"]), num(r["execute_time"])/1000, 0, num(r["affect_row"]), tg.toLocal(r["start_time"]))
	}
	return &CollectResult{Digests: agg.result()}, nil
}

// ---- 自定义采集 SQL ----

type customSQL struct{ run QueryRunner }

// ValidateCustomSQL 校验自定义采集 SQL：只允许单条 SELECT/WITH/SHOW，不允许写文件和危险函数。保存配置时和每次执行前都会校验。
func ValidateCustomSQL(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("自定义采集 SQL 不能为空")
	}
	if len(text) > 20000 {
		return errors.New("自定义采集 SQL 过长（最多 20000 个字符）")
	}
	stmts := sqlx.Split(text)
	if len(stmts) != 1 {
		return errors.New("自定义采集 SQL 只能是一条语句")
	}
	first := sqlx.FirstKeyword(stmts[0])
	if first != "select" && first != "with" && first != "show" {
		return errors.New("自定义采集 SQL 只能是 SELECT / WITH / SHOW 语句")
	}
	sig := sqlx.Significant(sqlx.Tokenize(stmts[0]))
	for i, t := range sig {
		if t.Lower() == "into" && i+1 < len(sig) {
			if n := sig[i+1].Lower(); n == "outfile" || n == "dumpfile" {
				return errors.New("自定义采集 SQL 不允许 INTO OUTFILE / DUMPFILE")
			}
		}
	}
	if fns := sqlx.DangerousFunctions(stmts[0]); len(fns) > 0 {
		return fmt.Errorf("自定义采集 SQL 不允许使用函数 %s", strings.Join(fns, "、"))
	}
	return nil
}

// bindNamed 把 %(start)s / %(end)s 命名占位符换成 ?（按出现顺序给参数），%% 还原成 %。
func bindNamed(text string, start, end time.Time) (string, []any) {
	var b strings.Builder
	var args []any
	for i := 0; i < len(text); i++ {
		switch {
		case strings.HasPrefix(text[i:], "%%"):
			b.WriteByte('%')
			i++
		case strings.HasPrefix(text[i:], "%(start)s"):
			b.WriteByte('?')
			args = append(args, start)
			i += len("%(start)s") - 1
		case strings.HasPrefix(text[i:], "%(end)s"):
			b.WriteByte('?')
			args = append(args, end)
			i += len("%(end)s") - 1
		default:
			b.WriteByte(text[i])
		}
	}
	return b.String(), args
}

// Collect 约定返回列（大小写不敏感）：
//
//	明细模式（每行一次执行）：db_name, sql_text, query_ms, rows_examined, rows_sent, ts
//	聚合模式（每行一个指纹）：db_name, sql_text, exec_count, total_ms, max_ms, rows_examined, rows_sent, last_seen
func (c *customSQL) Collect(ctx context.Context, req CollectRequest) (*CollectResult, error) {
	text := strings.TrimSpace(req.Config.CustomSQL)
	if err := ValidateCustomSQL(text); err != nil {
		return nil, err
	}
	tg := newTarget(ctx, c.run, req)
	q, args := bindNamed(text, tg.toServer(req.Start), tg.toServer(req.End))
	rows, err := c.run.Query(ctx, req.Instance, "", q, args...)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return &CollectResult{}, nil
	}
	if _, aggMode := rows[0]["exec_count"]; aggMode {
		var out []Digest
		for _, r := range rows {
			out = append(out, Digest{DBName: str(r["db_name"]), SampleSQL: str(r["sql_text"]), SampleReal: true, ExecCount: int64(num(r["exec_count"])),
				TotalSec: num(r["total_ms"]) / 1000, MaxMs: num(r["max_ms"]), RowsExamined: num(r["rows_examined"]), RowsSent: num(r["rows_sent"]),
				LastSeen: tg.toLocal(r["last_seen"])})
		}
		return &CollectResult{Digests: out}, nil
	}
	agg := newAggregator()
	for _, r := range rows {
		agg.add(str(r["db_name"]), str(r["sql_text"]), num(r["query_ms"])/1000, num(r["rows_examined"]), num(r["rows_sent"]), tg.toLocal(r["ts"]))
	}
	return &CollectResult{Digests: agg.result()}, nil
}

// ---- 真实的目标库查询与 EXPLAIN（database/sql + go-sql-driver） ----

type mysqlRunner struct{ repo Repo }

func openTarget(repo Repo, inst *model.Instance, db string) (*sql.DB, error) {
	c := repo.EngineConn(inst)
	return openConn(c, db)
}

func openConn(c engine.Conn, db string) (*sql.DB, error) {
	cfg := drv.NewConfig()
	cfg.User, cfg.Passwd = c.User, c.Password
	cfg.Net, cfg.Addr = "tcp", fmt.Sprintf("%s:%d", c.Host, c.Port)
	cfg.DBName = db
	if cfg.DBName == "" {
		cfg.DBName = c.DBName
	}
	cfg.ParseTime, cfg.Loc = true, time.Local
	cfg.Timeout, cfg.ReadTimeout = 10*time.Second, 90*time.Second
	charset := c.Charset
	if charset == "" {
		charset = "utf8mb4"
	}
	cfg.Params = map[string]string{"charset": charset}
	if c.SSL {
		cfg.TLSConfig = "skip-verify"
		if c.VerifySSL {
			cfg.TLSConfig = "true"
		}
	}
	d, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	d.SetConnMaxLifetime(time.Minute)
	return d, nil
}

func (r *mysqlRunner) Query(ctx context.Context, inst *model.Instance, db, q string, args ...any) ([]map[string]any, error) {
	d, err := openTarget(r.repo, inst, db)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

func scanRows(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = strings.ToLower(c)
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]any, len(cols))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			m[names[i]] = v
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// mysqlExplainer 在目标库上做 EXPLAIN（同一连接上再取 SHOW WARNINGS，里面有隐式转换的提示）。
type mysqlExplainer struct{ repo Repo }

// NewExplainer 返回真实的 EXPLAIN 实现。
func NewExplainer(repo Repo) Explainer { return &mysqlExplainer{repo: repo} }

func (e *mysqlExplainer) Explain(ctx context.Context, inst *model.Instance, db, q string, timeoutMs int) (*ExplainResult, error) {
	if !canExplain(q) {
		return nil, errors.New("只支持对单条 SELECT/UPDATE/DELETE/WITH 语句做 EXPLAIN")
	}
	d, err := openTarget(e.repo, inst, db)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond+5*time.Second)
	defer cancel()
	conn, err := d.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rows, err := conn.QueryContext(ctx, "EXPLAIN "+q)
	if err != nil {
		return nil, err
	}
	plan, err := scanRows(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	res := &ExplainResult{Plan: plan}
	if wr, err := conn.QueryContext(ctx, "SHOW WARNINGS"); err == nil {
		ws, _ := scanRows(wr)
		wr.Close()
		for _, w := range ws {
			res.Warnings = append(res.Warnings, str(w["message"]))
		}
	}
	return res, nil
}
