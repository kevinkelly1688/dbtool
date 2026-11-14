// @author: kevinkelly
// @license: Apache Licence
// @file: collect_test.go
// @time: 2026/10/28

package perf

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"dbtool/internal/model"
)

// fakeCollector 是假的采集来源：记录请求，返回预设的结果。
type fakeCollector struct {
	mu      sync.Mutex
	reqs    []CollectRequest
	digests []Digest
	err     error
	commits int
	note    string
}

func (c *fakeCollector) Collect(_ context.Context, req CollectRequest) (*CollectResult, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	return &CollectResult{Digests: append([]Digest(nil), c.digests...), Commit: func() error { c.commits++; return nil }, Note: c.note}, nil
}

func (f *fx) useCollector(c Collector) { f.svc.SetCollector(CollPlatformSlow, c) }

func TestCollectOneWindowsAndStatus(t *testing.T) {
	f := newFx(t)
	f.now = time.Date(2026, 10, 6, 10, 0, 30, 0, time.Local)
	col := &fakeCollector{digests: []Digest{f.digest(sqlOrderByUser, 1000, 4000)}}
	f.useCollector(col)
	msg, err := f.svc.CollectOne(context.Background(), 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := col.reqs[0]
	if !r.Start.Equal(time.Date(2026, 10, 6, 9, 0, 0, 0, time.Local)) || !r.End.Equal(time.Date(2026, 10, 6, 10, 0, 0, 0, time.Local)) || r.Backfill {
		t.Fatalf("首次窗口: %v ~ %v", r.Start, r.End)
	}
	cfg := f.cfg()
	if cfg.LastCollectTime == nil || !cfg.LastCollectTime.Equal(r.End) || col.commits != 1 {
		t.Fatalf("应推进采集进度并提交游标: %+v commits=%d", cfg, col.commits)
	}
	if !strings.Contains(msg, "OK 窗口10-06 09:00~10:00 指纹1 新建1 更新0") || cfg.LastStatus != msg {
		t.Fatalf("状态: %s", msg)
	}
	if len(f.repo.issues) != 1 {
		t.Fatal("应建单")
	}

	// 接续上次的窗口
	f.now = time.Date(2026, 10, 6, 10, 30, 10, 0, time.Local)
	msg, _ = f.svc.CollectOne(context.Background(), 1, nil, nil)
	r = col.reqs[1]
	if !r.Start.Equal(time.Date(2026, 10, 6, 10, 0, 0, 0, time.Local)) || !r.End.Equal(time.Date(2026, 10, 6, 10, 30, 0, 0, time.Local)) {
		t.Fatalf("接续窗口: %v ~ %v", r.Start, r.End)
	}
	if !strings.Contains(msg, "新建0 更新1") {
		t.Fatalf("第二次应更新已有工单: %s", msg)
	}

	// 没有新窗口：不采，状态保持
	before := f.cfg().LastStatus
	msg, _ = f.svc.CollectOne(context.Background(), 1, nil, nil)
	if len(col.reqs) != 2 || msg != before || strings.HasPrefix(f.cfg().LastStatus, runningPrefix) {
		t.Fatalf("没有新窗口应保持原状态并释放抢占: %q", f.cfg().LastStatus)
	}
}

func TestCollectOneCatchUpCap(t *testing.T) {
	f := newFx(t)
	col := &fakeCollector{}
	f.useCollector(col)
	last := f.now.Add(-100 * time.Hour)
	f.repo.collect[0].LastCollectTime = &last
	f.svc.CollectOne(context.Background(), 1, nil, nil)
	r := col.reqs[0]
	if !r.Start.Equal(last) || !r.End.Equal(last.Add(24*time.Hour)) {
		t.Fatalf("长时间停机后最多补 24 个窗口: %v ~ %v", r.Start, r.End)
	}
}

func TestCollectOneFailureKeepsProgress(t *testing.T) {
	f := newFx(t)
	last := f.now.Add(-2 * time.Hour)
	f.repo.collect[0].LastCollectTime = &last
	col := &fakeCollector{err: errors.New("连接超时 " + strings.Repeat("x", 600))}
	f.useCollector(col)
	msg, err := f.svc.CollectOne(context.Background(), 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.cfg()
	if !strings.Contains(msg, "ERROR 连接超时") || len([]rune(msg)) > 450 || cfg.LastStatus != msg {
		t.Fatalf("失败状态: %q", msg)
	}
	if !cfg.LastCollectTime.Equal(last) || col.commits != 0 {
		t.Fatal("失败不能推进进度")
	}
	// 未知采集方式
	f.repo.collect[0].Collector = "nope"
	msg, _ = f.svc.CollectOne(context.Background(), 1, nil, nil)
	if !strings.Contains(msg, "ERROR 不支持的采集方式: nope") {
		t.Fatalf("%s", msg)
	}
	// 实例被删
	f.repo.collect[0].Collector = CollPlatformSlow
	delete(f.repo.insts, 1)
	msg, _ = f.svc.CollectOne(context.Background(), 1, nil, nil)
	if !strings.Contains(msg, "ERROR 实例不存在") {
		t.Fatalf("%s", msg)
	}
	if _, err = f.svc.CollectOne(context.Background(), 99, nil, nil); httpOf(err) != 404 {
		t.Fatalf("配置不存在: %v", err)
	}
}

func TestCollectOneBackfillDoesNotMoveProgress(t *testing.T) {
	f := newFx(t)
	col := &fakeCollector{digests: []Digest{f.digest(sqlOrderByUser, 1000, 4000)}}
	f.useCollector(col)
	s, e := f.now.Add(-48*time.Hour), f.now.Add(-24*time.Hour)
	if _, err := f.svc.CollectOne(context.Background(), 1, &s, &e); err != nil {
		t.Fatal(err)
	}
	r := col.reqs[0]
	if !r.Backfill || !r.Start.Equal(s) || !r.End.Equal(e) || f.cfg().LastCollectTime != nil {
		t.Fatalf("补采不动进度: %+v %v", r, f.cfg().LastCollectTime)
	}
}

func TestCollectOneLock(t *testing.T) {
	f := newFx(t)
	col := &fakeCollector{}
	f.useCollector(col)
	// 别的进程正在采（新鲜的抢占）
	f.repo.collect[0].LastStatus = runningPrefix + f.now.Add(-5*time.Minute).Format("2006-01-02T15:04:05")
	msg, _ := f.svc.CollectOne(context.Background(), 1, nil, nil)
	if msg != "正在采集，跳过" || len(col.reqs) != 0 {
		t.Fatalf("应跳过: %s", msg)
	}
	// 过期的抢占（30 分钟前）可以接管
	f.repo.collect[0].LastStatus = runningPrefix + f.now.Add(-45*time.Minute).Format("2006-01-02T15:04:05")
	f.svc.CollectOne(context.Background(), 1, nil, nil)
	if len(col.reqs) != 1 {
		t.Fatal("过期抢占应可接管")
	}
	// 本进程内同一配置不并发
	if !f.svc.acquire(1) || f.svc.acquire(1) {
		t.Fatal("进程内互斥")
	}
	f.svc.release(1)
}

func TestCollectDue(t *testing.T) {
	f := newFx(t)
	col := &fakeCollector{}
	f.useCollector(col)
	recent := f.now.Add(-10 * time.Minute)
	f.repo.collect = append(f.repo.collect,
		CollectConfig{ID: 2, InstanceID: 2, Product: ProdSelectDB, Collector: CollPlatformSlow, Enabled: true, LastCollectTime: &recent},
		CollectConfig{ID: 3, InstanceID: 3, Product: ProdOther, Collector: CollPlatformSlow, Enabled: false})
	f.repo.insts[3].DBType = "mysql"
	res, err := f.svc.CollectDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[1] == "" || len(col.reqs) != 1 {
		t.Fatalf("只采到期且启用的: %v", res)
	}
}

func TestPreviewCollect(t *testing.T) {
	f := newFx(t)
	f.newIssue() // 已有工单 #1
	col := &fakeCollector{digests: []Digest{
		f.digest(sqlOrderByUser, 100, 400),                  // 已有工单，达标
		f.digest("select * from t_new where a = 1", 10, 50), // 新问题，达标
		f.digest("select * from t_fast where a = 1", 10, 1), // 不够慢
		f.digest("show tables", 10, 100),                    // 被排除
	}}
	f.useCollector(col)
	issues := len(f.repo.issues)
	in := CollectConfigInput{InstanceID: 1, Product: ProdMySQL, Collector: CollPlatformSlow, MinAvgMs: 1000, MinTotalSec: 10, ExcludeRegex: defaultExcludeRegex}
	res, err := f.svc.PreviewCollect(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 4 || res.Matched != 2 || len(res.Rows) != 4 || !col.reqs[0].Backfill || len(f.repo.issues) != issues {
		t.Fatalf("试采集只预览: %+v issues=%d", res, len(f.repo.issues))
	}
	if res.Rows[0].IssueID != 1 || !res.Rows[0].WouldCreate || res.Rows[2].IssueID != 0 || !res.Rows[2].WouldCreate || res.Rows[1].WouldCreate {
		t.Fatalf("行: %+v", res.Rows)
	}
	for _, r := range []PreviewRow{res.Rows[1], res.Rows[3]} {
		if r.WouldCreate {
			t.Fatalf("不应建单: %+v", r)
		}
	}
	if f.cfg().LastStatus != "" || f.cfg().LastCollectTime != nil || col.commits != 0 {
		t.Fatal("试采集不改采集状态与游标")
	}
	// 失败时给出可读的错误
	col.err = errors.New("access denied")
	_, err = f.svc.PreviewCollect(context.Background(), in)
	wantErr(t, err, 502, "试采集失败：access denied")
	// 参数不合法
	in.MinAvgMs = -1
	_, err = f.svc.PreviewCollect(context.Background(), in)
	wantErr(t, err, 400, "平均耗时阈值")
}

// ---- 具体的采集来源 ----

func TestPlatformSlowLogsCursor(t *testing.T) {
	f := newFx(t)
	t0 := f.now.Add(-30 * time.Minute)
	row := func(id uint, db, sql string, sec float64, inst uint) {
		ts := t0.Add(time.Duration(id) * time.Second)
		f.repo.slowRows = append(f.repo.slowRows, SlowLogRow{ID: id, DBName: db, SQLText: sql, QueryTimeSec: sec, RowsExamined: 100, RowsSent: 1, StartTime: &ts})
		f.repo.slowInst[id] = inst
	}
	row(1, "order", "select * from t where id = 1", 2, 1)
	row(2, "order", "select * from t where id = 2", 5, 1)
	row(3, "order", "select * from other where id = 2", 1, 1)
	row(4, "order", "select * from t where id = 3", 1, 2) // 别的实例
	c := &platformSlowLogs{repo: f.repo}
	req := CollectRequest{Config: f.cfg(), Instance: f.inst(), Start: f.now.Add(-time.Hour), End: f.now}
	res, err := c.Collect(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Digests) != 2 || res.Commit == nil {
		t.Fatalf("首次按窗口读取: %+v", res)
	}
	var d Digest
	for _, x := range res.Digests {
		if strings.Contains(x.SampleSQL, "from t ") {
			d = x
		}
	}
	if d.ExecCount != 2 || d.TotalSec != 7 || d.MaxMs != 5000 || !strings.Contains(d.SampleSQL, "id = 2") || d.RowsExamined != 200 || !d.SampleReal {
		t.Fatalf("聚合: %+v", d)
	}
	if cur, _ := f.repo.GetCursor(1, slowCursorSchema, slowCursorDigest); cur != nil {
		t.Fatal("入库成功前不能提交游标")
	}
	if err := res.Commit(); err != nil {
		t.Fatal(err)
	}
	if cur, _ := f.repo.GetCursor(1, slowCursorSchema, slowCursorDigest); cur == nil || cur.CountStar != 3 {
		t.Fatalf("游标应为最大 id 3: %+v", cur)
	}
	// 之后按 id 增量
	row(5, "order", "select * from t where id = 9", 3, 1)
	res, _ = c.Collect(context.Background(), req)
	if len(res.Digests) != 1 || res.Digests[0].ExecCount != 1 {
		t.Fatalf("增量: %+v", res.Digests)
	}
	res.Commit()
	if cur, _ := f.repo.GetCursor(1, slowCursorSchema, slowCursorDigest); cur.CountStar != 5 {
		t.Fatalf("%+v", cur)
	}
	// 没有新明细：不动游标
	res, _ = c.Collect(context.Background(), req)
	if len(res.Digests) != 0 || res.Commit != nil {
		t.Fatalf("没有新数据: %+v", res)
	}
	// 补采：不读游标、不提交
	req.Backfill = true
	res, _ = c.Collect(context.Background(), req)
	if len(res.Digests) != 2 || res.Commit != nil {
		t.Fatalf("补采: %+v", res)
	}
}

func TestPlatformSlowLogsFirstRunEmptyWindow(t *testing.T) {
	f := newFx(t)
	old := f.now.Add(-5 * time.Hour)
	f.repo.slowRows = []SlowLogRow{{ID: 7, DBName: "order", SQLText: "select 1", QueryTimeSec: 1, StartTime: &old}}
	f.repo.slowInst[7] = 1
	c := &platformSlowLogs{repo: f.repo}
	res, _ := c.Collect(context.Background(), CollectRequest{Config: f.cfg(), Instance: f.inst(), Start: f.now.Add(-time.Hour), End: f.now})
	if len(res.Digests) != 0 || res.Commit == nil {
		t.Fatalf("%+v", res)
	}
	res.Commit()
	if cur, _ := f.repo.GetCursor(1, slowCursorSchema, slowCursorDigest); cur == nil || cur.CountStar != 7 {
		t.Fatalf("游标应定位到窗口结束前的最大 id: %+v", cur)
	}
}

// fakeRunner 是假的目标库查询：按 SQL 里的关键字返回预设结果。
type fakeRunner struct {
	handlers []func(sql string, args []any) ([]map[string]any, error, bool)
	calls    []fakeCall
}

type fakeCall struct {
	sql  string
	args []any
}

func (r *fakeRunner) Query(_ context.Context, _ *model.Instance, _ string, sql string, args ...any) ([]map[string]any, error) {
	r.calls = append(r.calls, fakeCall{sql, args})
	for _, h := range r.handlers {
		if rows, err, ok := h(sql, args); ok {
			return rows, err
		}
	}
	return nil, errors.New("没有预设的查询: " + sql)
}

func (r *fakeRunner) on(substr string, rows func(args []any) ([]map[string]any, error)) {
	r.handlers = append(r.handlers, func(sql string, args []any) ([]map[string]any, error, bool) {
		if !strings.Contains(sql, substr) {
			return nil, nil, false
		}
		out, err := rows(args)
		return out, err, true
	})
}

func (r *fakeRunner) serverNow(t time.Time) {
	r.on("SELECT NOW()", func([]any) ([]map[string]any, error) { return []map[string]any{{"now_ts": t}}, nil })
}

func TestPerfSchemaDeltaAndCursors(t *testing.T) {
	f := newFx(t)
	now := f.now
	state := struct {
		cnt   int64
		timer uint64
	}{100, 100 * 1e12}
	run := &fakeRunner{}
	run.serverNow(now)
	run.on("events_statements_summary_by_digest", func([]any) ([]map[string]any, error) {
		return []map[string]any{{"schema_name": "order", "digest": "d1", "digest_text": "SELECT * FROM `t` WHERE `a` = ?", "count_star": state.cnt,
			"sum_timer_wait": state.timer, "max_timer_wait": int64(3 * 1e12), "sum_rows_examined": state.cnt * 10, "sum_rows_sent": state.cnt,
			"first_seen": now, "last_seen": now, "query_sample_text": "select * from t where a = 1"}}, nil
	})
	c := &perfSchema{run: run, repo: f.repo}
	req := CollectRequest{Config: f.cfg(), Instance: f.inst(), Start: now.Add(-time.Hour), End: now, Now: now}
	res, err := c.Collect(context.Background(), req)
	if err != nil || len(res.Digests) != 0 || !strings.Contains(res.Note, "首次采集") {
		t.Fatalf("首次只建游标: %+v %v", res, err)
	}
	if len(f.repo.cursors) != 0 {
		t.Fatal("入库成功前不写游标")
	}
	res.Commit()
	if len(f.repo.cursors) != 1 || f.repo.cursors[0].CountStar != 100 {
		t.Fatalf("游标: %+v", f.repo.cursors)
	}
	// 第二次：增量 50 次、100 秒
	state.cnt, state.timer = 150, 200*1e12
	res, _ = c.Collect(context.Background(), req)
	if len(res.Digests) != 1 {
		t.Fatalf("%+v", res)
	}
	d := res.Digests[0]
	if d.ExecCount != 50 || d.TotalSec != 100 || d.MaxMs != 3000 || !d.SampleReal || d.DBName != "order" || d.RowsExamined != 500 || d.RowsSent != 50 {
		t.Fatalf("增量: %+v", d)
	}
	// 非首次按 LAST_SEEN 过滤（参数是服务器时区的窗口起点）
	last := run.calls[len(run.calls)-1]
	if !strings.Contains(last.sql, "AND LAST_SEEN >= ?") || !last.args[0].(time.Time).Equal(req.Start) {
		t.Fatalf("过滤条件: %+v", last)
	}
	res.Commit()
	if f.repo.cursors[0].CountStar != 150 {
		t.Fatal("游标应更新")
	}
	// 被 truncate / 重启：累计值变小，取当前值
	state.cnt, state.timer = 20, 40*1e12
	res, _ = c.Collect(context.Background(), req)
	if len(res.Digests) != 1 || res.Digests[0].ExecCount != 20 || res.Digests[0].TotalSec != 40 {
		t.Fatalf("重启后: %+v", res.Digests)
	}
	// 没有增量
	res.Commit()
	res, _ = c.Collect(context.Background(), req)
	if len(res.Digests) != 0 {
		t.Fatalf("没有增量不产出: %+v", res.Digests)
	}
	// 试采集（补采）不写游标
	state.cnt, state.timer = 30, 60*1e12
	req.Backfill = true
	res, _ = c.Collect(context.Background(), req)
	if res.Commit != nil || len(res.Digests) != 1 {
		t.Fatalf("试采集: %+v", res)
	}
}

func TestPerfSchemaOldVersionAndTruncatedSample(t *testing.T) {
	f := newFx(t)
	now := f.now
	run := &fakeRunner{}
	run.serverNow(now)
	run.on("events_statements_summary_by_digest", func([]any) ([]map[string]any, error) {
		return []map[string]any{{"schema_name": "order", "digest": "d1", "digest_text": "SELECT * FROM t WHERE a = ?", "count_star": int64(10), "sum_timer_wait": uint64(10 * 1e12),
			"max_timer_wait": int64(1e12), "sum_rows_examined": int64(1), "sum_rows_sent": int64(1), "first_seen": now, "last_seen": now}}, nil
	})
	// 5.7 没有 QUERY_SAMPLE_TEXT：带该列的查询报错，回退到不带的
	run.handlers = append([]func(string, []any) ([]map[string]any, error, bool){func(sql string, args []any) ([]map[string]any, error, bool) {
		if strings.Contains(sql, "QUERY_SAMPLE_TEXT") {
			return nil, errors.New("Unknown column 'QUERY_SAMPLE_TEXT' in 'field list'"), true
		}
		return nil, nil, false
	}}, run.handlers...)
	c := &perfSchema{run: run, repo: f.repo}
	f.repo.cursors = []DigestCursor{{ID: 1, InstanceID: 1, SchemaName: "order", Digest: "d1", CountStar: 5, SumTimerWait: 5 * 1e12}}
	res, err := c.Collect(context.Background(), CollectRequest{Config: f.cfg(), Instance: f.inst(), Start: now.Add(-time.Hour), End: now, Now: now})
	if err != nil || len(res.Digests) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	d := res.Digests[0]
	if d.SampleReal || d.SampleSQL != "SELECT * FROM t WHERE a = ?" || d.ExecCount != 5 {
		t.Fatalf("只有参数化文本时 sample_is_real 应为 false: %+v", d)
	}
}

func TestSlowLogTableTimezone(t *testing.T) {
	f := newFx(t)
	local := f.now
	server := local.Add(-8 * time.Hour) // AWS RDS 默认 UTC
	run := &fakeRunner{}
	run.serverNow(server)
	run.on("mysql.slow_log", func([]any) ([]map[string]any, error) {
		return []map[string]any{
			{"start_time": server, "db": "order", "qt": "2.500000", "rows_examined": int64(100), "rows_sent": int64(1), "sql_text": "select * from t where id=1"},
			{"start_time": server.Add(-time.Minute), "db": "order", "qt": 1.5, "rows_examined": int64(100), "rows_sent": int64(1), "sql_text": "select * from t where id=2"},
		}, nil
	})
	c := &slowLogTable{run: run}
	req := CollectRequest{Config: f.cfg(), Instance: f.inst(), Start: local.Add(-time.Hour), End: local, Now: local}
	res, err := c.Collect(context.Background(), req)
	if err != nil || len(res.Digests) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	args := run.calls[len(run.calls)-1].args
	if !args[0].(time.Time).Equal(req.Start.Add(-8*time.Hour)) || !args[1].(time.Time).Equal(server) {
		t.Fatalf("窗口应换算为服务器时区: %v", args)
	}
	d := res.Digests[0]
	if d.ExecCount != 2 || d.TotalSec != 4 || !d.LastSeen.Equal(local) || !d.FirstSeen.Equal(local.Add(-time.Minute)) {
		t.Fatalf("聚合与换回本地时区: %+v last=%v", d, d.LastSeen)
	}
	// 取不到服务器时间：按同时区处理
	run2 := &fakeRunner{}
	run2.on("SELECT NOW()", func([]any) ([]map[string]any, error) { return nil, errors.New("denied") })
	run2.on("mysql.slow_log", func([]any) ([]map[string]any, error) { return nil, nil })
	if _, err = (&slowLogTable{run: run2}).Collect(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := run2.calls[len(run2.calls)-1].args[0].(time.Time); !got.Equal(req.Start) {
		t.Fatalf("应按同时区: %v", got)
	}
}

func TestDorisPolarDBXCollectors(t *testing.T) {
	f := newFx(t)
	now := f.now
	run := &fakeRunner{}
	run.serverNow(now)
	run.on("__internal_schema.audit_log", func([]any) ([]map[string]any, error) {
		return []map[string]any{{"time": now, "db": "default_cluster:dw", "query_time": int64(4000), "scan_rows": int64(1e6), "return_rows": int64(10), "stmt": "select a from dw.t where dt = '2026-10-01'"}}, nil
	})
	req := CollectRequest{Config: &CollectConfig{InstanceID: 2, MinAvgMs: 3000}, Instance: &model.Instance{ID: 2, InstanceName: "doris"}, Start: now.Add(-time.Hour), End: now, Now: now}
	res, err := (&dorisAudit{run: run}).Collect(context.Background(), req)
	if err != nil || len(res.Digests) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if d := res.Digests[0]; d.DBName != "dw" || d.TotalSec != 4 || d.RowsExamined != 1e6 {
		t.Fatalf("doris: %+v", d)
	}
	if last := run.calls[len(run.calls)-1]; last.args[2] != 3000 {
		t.Fatalf("Doris 只取超过阈值的: %v", last.args)
	}

	run = &fakeRunner{}
	run.serverNow(now)
	run.on("STATEMENTS_SUMMARY_HISTORY", func([]any) ([]map[string]any, error) {
		return []map[string]any{
			{"db_name": "shop", "sql_template": "SELECT * FROM t WHERE a = ?", "sql_sample": "", "cnt": int64(50), "total_ms": float64(100000), "max_ms": float64(9000),
				"rows_examined": float64(5e6), "rows_sent": float64(50), "first_seen": now.Add(-time.Hour), "last_seen": now},
		}, nil
	})
	res, err = (&polardbxSummary{run: run}).Collect(context.Background(), req)
	if err != nil || len(res.Digests) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if d := res.Digests[0]; d.SampleReal || d.TotalSec != 100 || d.MaxMs != 9000 || d.ExecCount != 50 {
		t.Fatalf("polardbx: %+v", d)
	}

	run = &fakeRunner{}
	run.serverNow(now)
	run.on("SHOW FULL SLOW", func([]any) ([]map[string]any, error) {
		return []map[string]any{{"db": "", "schema": "shop", "sql": "select * from t where a = 1", "execute_time": float64(2000), "affect_row": float64(3), "start_time": now}}, nil
	})
	res, err = (&polardbxShowSlow{run: run}).Collect(context.Background(), req)
	if err != nil || len(res.Digests) != 1 || res.Digests[0].DBName != "shop" || res.Digests[0].TotalSec != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if q := run.calls[len(run.calls)-1].sql; !strings.Contains(q, "START_TIME >= '"+now.Add(-time.Hour).Format(tsLayout)+"'") || !strings.HasSuffix(q, "LIMIT 50000") {
		t.Fatalf("SHOW FULL SLOW: %s", q)
	}
}

func TestCustomSQLCollector(t *testing.T) {
	f := newFx(t)
	now := f.now
	run := &fakeRunner{}
	run.serverNow(now)
	var rows []map[string]any
	run.on("from my_slow", func([]any) ([]map[string]any, error) { return rows, nil })
	cfg := &CollectConfig{InstanceID: 2, CustomSQL: "select db_name, sql_text, query_ms, rows_examined, rows_sent, ts from my_slow where ts >= %(start)s and ts < %(end)s and sql_text like '%%x%%'"}
	req := CollectRequest{Config: cfg, Instance: f.inst(), Start: now.Add(-time.Hour), End: now, Now: now}
	// 明细模式
	rows = []map[string]any{
		{"db_name": "dw", "sql_text": "select * from t where a = 1", "query_ms": float64(2000), "rows_examined": int64(10), "rows_sent": int64(1), "ts": now},
		{"db_name": "dw", "sql_text": "select * from t where a = 2", "query_ms": float64(4000), "rows_examined": int64(10), "rows_sent": int64(1), "ts": now},
	}
	res, err := (&customSQL{run: run}).Collect(context.Background(), req)
	if err != nil || len(res.Digests) != 1 || res.Digests[0].ExecCount != 2 || res.Digests[0].TotalSec != 6 {
		t.Fatalf("明细模式: %+v %v", res, err)
	}
	last := run.calls[len(run.calls)-1]
	if !strings.Contains(last.sql, "ts >= ? and ts < ?") || !strings.Contains(last.sql, "like '%x%'") || len(last.args) != 2 {
		t.Fatalf("占位符应改为 ?，%%%% 还原: %s %v", last.sql, last.args)
	}
	// 聚合模式
	rows = []map[string]any{{"db_name": "dw", "sql_text": "select 1 from t", "exec_count": int64(7), "total_ms": float64(14000), "max_ms": float64(3000), "rows_examined": float64(1), "rows_sent": float64(1), "last_seen": now}}
	res, err = (&customSQL{run: run}).Collect(context.Background(), req)
	if err != nil || len(res.Digests) != 1 || res.Digests[0].ExecCount != 7 || res.Digests[0].TotalSec != 14 || res.Digests[0].MaxMs != 3000 {
		t.Fatalf("聚合模式: %+v %v", res, err)
	}
	rows = nil
	if res, _ = (&customSQL{run: run}).Collect(context.Background(), req); len(res.Digests) != 0 {
		t.Fatal("空结果")
	}
	// 不合法的 SQL 在执行前就被拒绝
	req.Config = &CollectConfig{CustomSQL: "drop table t"}
	if _, err = (&customSQL{run: run}).Collect(context.Background(), req); err == nil {
		t.Fatal("应拒绝")
	}
}

func TestValidateCustomSQL(t *testing.T) {
	ok := []string{"select 1", "SELECT * FROM x WHERE a > %(start)s;", "with a as (select 1) select * from a", "show full slow where start_time > '2026-01-01'"}
	for _, s := range ok {
		if err := ValidateCustomSQL(s); err != nil {
			t.Errorf("%q 应通过: %v", s, err)
		}
	}
	bad := map[string]string{
		"":                                   "不能为空",
		"select 1; select 2":                 "只能是一条",
		"update t set a=1":                   "只能是 SELECT",
		"delete from t":                      "只能是 SELECT",
		"select * from t into outfile '/x'":  "OUTFILE",
		"select * from t into dumpfile '/x'": "DUMPFILE",
		"select sleep(10)":                   "sleep",
		"select load_file('/etc/passwd')":    "load_file",
		strings.Repeat("a", 20001):           "过长",
	}
	for s, want := range bad {
		err := ValidateCustomSQL(s)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q 应拒绝（%s）: %v", clipRunes(s, 30), want, err)
		}
	}
	// 注释里的关键字、字符串里的分号不影响判断
	if err := ValidateCustomSQL("select ';' as x -- ; update\nfrom t"); err != nil {
		t.Errorf("字符串里的分号: %v", err)
	}
}

func TestBindNamed(t *testing.T) {
	s, e := time.Now(), time.Now().Add(time.Hour)
	q, args := bindNamed("a >= %(start)s and b < %(end)s and c like '100%%' and d >= %(start)s", s, e)
	if q != "a >= ? and b < ? and c like '100%' and d >= ?" || len(args) != 3 || args[1] != e {
		t.Fatalf("%q %v", q, args)
	}
}

func TestServerOffsetRounding(t *testing.T) {
	now := time.Date(2026, 10, 6, 10, 0, 0, 0, time.Local)
	for _, c := range []struct {
		server time.Time
		want   time.Duration
	}{
		{now.Add(-8*time.Hour - 20*time.Second), 8 * time.Hour},
		{now, 0},
		{now.Add(5 * time.Hour), -5 * time.Hour},
		{now.Add(-30*time.Minute - 40*time.Second), 30 * time.Minute},
	} {
		run := &fakeRunner{}
		run.serverNow(c.server)
		if got := serverOffset(context.Background(), run, &model.Instance{}, now); got != c.want {
			t.Errorf("服务器 %v → %v，期望 %v", c.server, got, c.want)
		}
	}
	if toTime("2026-10-06 10:00:00.123456") == nil || toTime([]byte("2026-10-06 10:00:00")) == nil || toTime("bad") != nil || toTime(nil) != nil {
		t.Fatal("toTime")
	}
	if u64(nil) != 0 || u64("123.000") != 123 || u64([]byte("77")) != 77 || u64(int64(-1)) != 0 || u64(uint64(5)) != 5 {
		t.Fatal("u64")
	}
}

func TestDefaultCollectorsCoverAll(t *testing.T) {
	m := DefaultCollectors(newMemRepo())
	for _, c := range Collectors {
		if m[c.Value] == nil {
			t.Errorf("采集方式 %s 没有实现", c.Value)
		}
	}
	if len(m) != len(Collectors) {
		t.Errorf("多余的实现")
	}
}
