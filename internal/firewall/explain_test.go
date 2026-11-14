// @author: kevinkelly
// @license: Apache Licence
// @file: explain_test.go
// @time: 2026/10/28

package firewall

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dbtool/internal/settings"
)

// ---- 计划 → 扫描行数 ----

var planCols = []string{"id", "select_type", "table", "type", "key", "rows", "filtered", "Extra"}

func planRow(id int, table string, rows, filtered any) []any {
	return []any{id, "SIMPLE", table, "ALL", nil, rows, filtered, nil}
}

func TestScanRowsFromPlan(t *testing.T) {
	cases := []struct {
		name string
		rows [][]any
		want int64
	}{
		{"单表全表扫描：就是 rows，不乘 filtered（filtered 只说明有多少行留下）", [][]any{planRow(1, "orders", int64(5000000), 10.0)}, 5000000},
		{"驱动表 100 行过滤剩 50%，被驱动表每次扫 10 行：100 + 50×10", [][]any{planRow(1, "a", 100, 50.0), planRow(1, "b", 10, 100.0)}, 600},
		{"主查询 + 子查询两个 id 组：各组之和", [][]any{planRow(1, "a", 1000, 100.0), planRow(2, "b", 200, 100.0)}, 1200},
		{"rows 为 NULL 的计划行（Impossible WHERE / UNION RESULT）不计", [][]any{planRow(1, "a", nil, nil), {1, "UNION RESULT", "<union1,2>", "ALL", nil, nil, nil, nil}}, 0},
		{"数值是字符串 / 字节切片也能解析", [][]any{planRow(1, "a", "300", []byte("50.00")), planRow(1, "b", []byte("4"), nil)}, 300 + 150*4},
		{"filtered 缺失按 100% 算", [][]any{planRow(1, "a", 10, nil), planRow(1, "b", 10, nil)}, 10 + 10*10},
	}
	for _, c := range cases {
		got, err := ScanRowsFromPlan(planCols, c.rows)
		if err != nil || got != c.want {
			t.Errorf("%s: 得到 %d %v，应为 %d", c.name, got, err, c.want)
		}
	}
	if _, err := ScanRowsFromPlan([]string{"id", "estRows", "task"}, [][]any{{1, 100, "root"}}); err == nil || !strings.Contains(err.Error(), "MySQL 格式") {
		t.Errorf("不是 MySQL 格式的执行计划应报错: %v", err)
	}
	if _, err := ScanRowsFromPlan(planCols, nil); err == nil {
		t.Error("空计划应报错")
	}
	// 溢出封顶，不 panic
	big := [][]any{planRow(1, "a", int64(1<<60), 100.0), planRow(1, "b", int64(1<<60), 100.0)}
	if n, err := ScanRowsFromPlan(planCols, big); err != nil || n <= 0 || n > 9e15 {
		t.Errorf("溢出应封顶: %d %v", n, err)
	}
}

func TestExplainSQL(t *testing.T) {
	ok := map[string]string{
		"select * from t where id = 1;":        "EXPLAIN select * from t where id = 1",
		"  UPDATE t SET a = 1 WHERE b = 2":     "EXPLAIN UPDATE t SET a = 1 WHERE b = 2",
		"delete from t":                        "EXPLAIN delete from t",
		"with x as (select 1) select * from x": "EXPLAIN with x as (select 1) select * from x",
		"insert into t select * from u":        "EXPLAIN insert into t select * from u",
		"select 1; select 2":                   "EXPLAIN select 1",
		"/* c */ replace into t values (1)":    "EXPLAIN replace into t values (1)",
	}
	for in, want := range ok {
		got, err := ExplainSQL(in)
		if err != nil || got != want {
			t.Errorf("%q: 得到 %q %v，应为 %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "alter table t add c int", "show tables", "explain select 1", "truncate t", "drop table t"} {
		if _, err := ExplainSQL(in); err == nil {
			t.Errorf("%q 不应生成 EXPLAIN", in)
		}
	}
}

// ---- EstimateScan：限时 / 缓存 / 熔断 / 统计 ----

func okPlan(n *atomic.Int32, rows int64) PlanFunc {
	return func(context.Context, string, time.Duration) ([]string, [][]any, error) {
		n.Add(1)
		return planCols, [][]any{planRow(1, "t", rows, 100.0)}, nil
	}
}

func TestEstimateScanCachesAndCountsStats(t *testing.T) {
	e, now := newEngine(&fakeStore{}, nil)
	var calls atomic.Int32
	plan := okPlan(&calls, 4200)
	for i := 0; i < 3; i++ {
		n, err := e.EstimateScan(context.Background(), 1, "shop", "select * from t where a = 1", plan)
		if err != nil || n != 4200 {
			t.Fatalf("第 %d 次: %d %v", i, n, err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("同一语句短期内只应下发一次 EXPLAIN: %d", calls.Load())
	}
	// 另一个实例 / 另一个库不共用缓存
	e.EstimateScan(context.Background(), 2, "shop", "select * from t where a = 1", plan)
	e.EstimateScan(context.Background(), 1, "other", "select * from t where a = 1", plan)
	if calls.Load() != 3 {
		t.Errorf("实例 / 库不同应各自 EXPLAIN: %d", calls.Load())
	}
	// 缓存过期后重新取
	*now = now.Add(explainOKTTL + time.Second)
	e.EstimateScan(context.Background(), 1, "shop", "select * from t where a = 1", plan)
	if calls.Load() != 4 {
		t.Errorf("缓存过期后应重新 EXPLAIN: %d", calls.Load())
	}
	st := e.ExplainStats()
	if st.Calls != 4 || st.CacheHits != 2 || st.Failures != 0 {
		t.Errorf("统计: %+v", st)
	}
	if _, err := e.EstimateScan(context.Background(), 1, "shop", "alter table t add c int", plan); err == nil {
		t.Error("不支持的语句类型应直接报错，不下发 EXPLAIN")
	}
	if calls.Load() != 4 {
		t.Error("不支持的语句不应下发 EXPLAIN")
	}
}

func TestEstimateScanTimeoutAndNegativeCache(t *testing.T) {
	st := &fakeStore{}
	e, _ := newEngine(st, fakeCfg{settings.KeyFirewallExplainTimeoutMs: "200"})
	var calls atomic.Int32
	slow := func(ctx context.Context, _ string, timeout time.Duration) ([]string, [][]any, error) {
		calls.Add(1)
		if timeout != 200*time.Millisecond {
			t.Errorf("应把配置的时限带给调用方: %v", timeout)
		}
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	start := time.Now()
	_, err := e.EstimateScan(context.Background(), 1, "shop", "select * from big", slow)
	if err == nil || !strings.Contains(err.Error(), "200 毫秒") {
		t.Fatalf("超时应报错并写明时限: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("没有按时限返回: %v", d)
	}
	// 失败也缓存一小段时间：同一语句不会每次都再等一个超时
	if _, err := e.EstimateScan(context.Background(), 1, "shop", "select * from big", slow); err == nil || calls.Load() != 1 {
		t.Errorf("失败应被短期缓存: calls=%d err=%v", calls.Load(), err)
	}
	if s := e.ExplainStats(); s.Timeouts != 1 || s.Failures != 1 || s.CacheHits != 1 {
		t.Errorf("统计: %+v", s)
	}
}

func TestEstimateScanBreakerOpensAndRecovers(t *testing.T) {
	e, now := newEngine(&fakeStore{}, nil)
	var calls atomic.Int32
	down := func(context.Context, string, time.Duration) ([]string, [][]any, error) {
		calls.Add(1)
		return nil, nil, errors.New("dial tcp: connection refused")
	}
	wrapped := func(c context.Context, q string, t time.Duration) ([]string, [][]any, error) {
		cols, rows, err := down(c, q, t)
		return cols, rows, errors.Join(ErrExplainUnavailable, err)
	}
	for i := 0; i < breakerFailures; i++ { // 不同语句，避开缓存
		e.EstimateScan(context.Background(), 9, "shop", "select "+strings.Repeat("1", i+1)+" from t", wrapped)
	}
	if calls.Load() != breakerFailures {
		t.Fatalf("前 %d 次应真正下发: %d", breakerFailures, calls.Load())
	}
	_, err := e.EstimateScan(context.Background(), 9, "shop", "select 99 from t", wrapped)
	if err == nil || !strings.Contains(err.Error(), "暂停") || calls.Load() != breakerFailures {
		t.Errorf("熔断期间不应再下发: %v calls=%d", err, calls.Load())
	}
	if s := e.ExplainStats(); len(s.Open) != 1 || s.Open[0] != 9 || s.Skipped != 1 {
		t.Errorf("统计应列出熔断中的实例: %+v", s)
	}
	// 别的实例不受影响
	var ok atomic.Int32
	if _, err := e.EstimateScan(context.Background(), 10, "shop", "select 1 from t", okPlan(&ok, 10)); err != nil {
		t.Errorf("别的实例不受熔断影响: %v", err)
	}
	// 熔断期过后恢复尝试；成功后计数清零
	*now = now.Add(breakerOpenFor + time.Second)
	if _, err := e.EstimateScan(context.Background(), 9, "shop", "select 100 from t", okPlan(&ok, 5)); err != nil {
		t.Errorf("熔断期过后应恢复: %v", err)
	}
	if s := e.ExplainStats(); len(s.Open) != 0 {
		t.Errorf("恢复后不应还在熔断: %+v", s)
	}
}

func TestEstimateScanStatementErrorsDoNotOpenBreaker(t *testing.T) {
	e, _ := newEngine(&fakeStore{}, nil)
	bad := func(context.Context, string, time.Duration) ([]string, [][]any, error) {
		return nil, nil, errors.New("Error 1146: Table 'shop.nope' doesn't exist")
	}
	for i := 0; i < 2*breakerFailures; i++ {
		e.EstimateScan(context.Background(), 1, "shop", "select "+strings.Repeat("1", i+1)+" from nope", bad)
	}
	if s := e.ExplainStats(); len(s.Open) != 0 || s.Failures != int64(2*breakerFailures) {
		t.Errorf("语句自己的错误（表不存在）与实例健康无关，不应熔断: %+v", s)
	}
}

func TestEstimateScanCancelledCallerIsNotCached(t *testing.T) {
	e, _ := newEngine(&fakeStore{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	plan := func(c context.Context, _ string, _ time.Duration) ([]string, [][]any, error) {
		calls.Add(1)
		cancel()
		return nil, nil, c.Err()
	}
	e.EstimateScan(ctx, 1, "shop", "select * from t", plan)
	var ok atomic.Int32
	n, err := e.EstimateScan(context.Background(), 1, "shop", "select * from t", okPlan(&ok, 77))
	if err != nil || n != 77 {
		t.Errorf("调用方取消不应缓存失败: %d %v", n, err)
	}
}

// ---- 条件的取事实顺序与短路 ----

func scanCond(n int64) Condition { return Condition{T: CondScanRows, V: n} }

func countingReq(sql string, rows int64, calls *atomic.Int32) Request {
	r := baseReq(sql)
	r.ScanRows = func() (int64, error) { calls.Add(1); return rows, nil }
	return r
}

func TestScanRowsConditionIsOnlyFetchedWhenNeeded(t *testing.T) {
	var calls atomic.Int32
	all := rule("全表扫描且没条件", 1, ActDeny, scanCond(1000), featC(FeatNoWhere)) // 故意把贵的条件写在前面
	all.Match = "all"
	// 便宜的条件不满足：不取 EXPLAIN
	d := eval(t, []Rule{all}, countingReq("select * from orders where id = 1", 5000000, &calls))
	if d.Deny || calls.Load() != 0 {
		t.Errorf("没有 no_where 时不应为 EXPLAIN 付代价: deny=%v calls=%d", d.Deny, calls.Load())
	}
	d = eval(t, []Rule{all}, countingReq("select * from orders", 5000000, &calls))
	if !d.Deny || calls.Load() != 1 {
		t.Errorf("便宜的条件满足后才取 EXPLAIN: deny=%v calls=%d", d.Deny, calls.Load())
	}
	if len(d.Hits) != 1 || len(d.Hits[0].Matched) != 2 || !strings.Contains(d.Hits[0].Matched[1], "预估扫描 5000000 行") {
		t.Errorf("命中说明应带预估扫描行数: %+v", d.Hits)
	}

	// 任一满足：前面已命中，不再取 EXPLAIN
	calls.Store(0)
	any := rule("任一", 2, ActWarn, scanCond(1000), kwC("sleep"))
	any.Match = "any"
	d = eval(t, []Rule{any}, countingReq("select sleep(1)", 5000000, &calls))
	if len(d.Hits) != 1 || calls.Load() != 0 {
		t.Errorf("任一满足里便宜的已命中，不应再取 EXPLAIN: hits=%d calls=%d", len(d.Hits), calls.Load())
	}
	// 多条规则共用一次 EXPLAIN（同一条语句的事实只取一次）
	calls.Store(0)
	a := rule("a", 3, ActWarn, scanCond(100))
	b := rule("b", 4, ActWarn, scanCond(200))
	d = eval(t, []Rule{a, b}, countingReq("select * from t", 5000, &calls))
	if len(d.Hits) != 2 || calls.Load() != 1 {
		t.Errorf("同一条语句的 EXPLAIN 只取一次: hits=%d calls=%d", len(d.Hits), calls.Load())
	}
}

func TestScanRowsUnknownWhenExplainFails(t *testing.T) {
	r := rule("大扫描", 1, ActDeny, scanCond(1000))
	req := baseReq("select * from t")
	req.ScanRows = func() (int64, error) { return 0, errors.New("EXPLAIN 超过 3000 毫秒没有返回") }
	d := eval(t, []Rule{r}, req)
	if d.Deny || len(d.Hits) != 0 {
		t.Fatalf("取不到事实不能误拦: %+v", d)
	}
	if len(d.Skipped) != 1 || !d.Skipped[0].Unknown || !strings.Contains(d.Skipped[0].Reason, "未评估") || !strings.Contains(d.Skipped[0].Reason, "3000 毫秒") {
		t.Errorf("应说明未评估的原因: %+v", d.Skipped)
	}
}

func TestAffectRowsFallsBackToScanForDML(t *testing.T) {
	r := rule("影响行数过多", 1, ActDeny, Condition{T: CondAffectRows, V: int64(1000)})
	var calls atomic.Int32
	// 检查结果里有影响行数：直接用，不取 EXPLAIN
	req := countingReq("update orders set a = 1", 9999999, &calls)
	req.AffectRows = 50
	if d := eval(t, []Rule{r}, req); d.Deny || calls.Load() != 0 {
		t.Errorf("已有影响行数就不用 EXPLAIN: deny=%v calls=%d", d.Deny, calls.Load())
	}
	// 没有：UPDATE / DELETE 退回 EXPLAIN 扫描行数（上界）
	req = countingReq("update orders set a = 1", 50000, &calls)
	d := eval(t, []Rule{r}, req)
	if !d.Deny || calls.Load() != 1 || !strings.Contains(d.Hits[0].Matched[0], "EXPLAIN 扫描行数") {
		t.Errorf("UPDATE 应退回 EXPLAIN 上界: %+v calls=%d", d, calls.Load())
	}
	// SELECT / INSERT 没有“影响行数”的 EXPLAIN 退路：未评估
	req = countingReq("select * from orders", 50000, &calls)
	if d := eval(t, []Rule{r}, req); d.Deny || len(d.Skipped) != 1 || !d.Skipped[0].Unknown {
		t.Errorf("SELECT 的影响行数应为未评估: %+v", d)
	}
}

// ---- 规则级统计 ----

func TestEngineRuleStatsCountEvalsAndUnknown(t *testing.T) {
	r1 := rule("词法", 1, ActWarn, kwC("sleep"))
	r1.ID = 1
	r2 := rule("扫描", 2, ActWarn, scanCond(10))
	r2.ID = 2
	st := &fakeStore{rules: []Rule{r1, r2}}
	e, _ := newEngine(st, nil)
	ok := baseReq("select * from t")
	ok.ScanRows = func() (int64, error) { return 100, nil }
	bad := baseReq("select * from t2")
	bad.ScanRows = func() (int64, error) { return 0, errors.New("超时") }
	e.Evaluate(context.Background(), ok)
	e.Evaluate(context.Background(), bad)
	rs := e.RuleStats()
	if rs[1].Evals != 2 || rs[1].Unknown != 0 || rs[2].Evals != 2 || rs[2].Unknown != 1 {
		t.Errorf("规则统计: %+v", rs)
	}
}

func TestScanTimeoutSettingIsClamped(t *testing.T) {
	cases := map[string]time.Duration{"": DefaultExplainTimeout, "5000": 5 * time.Second, "10": minExplainTimeout, "999999": maxExplainTimeout, "abc": DefaultExplainTimeout}
	for in, want := range cases {
		cfg := fakeCfg{}
		if in != "" {
			cfg[settings.KeyFirewallExplainTimeoutMs] = in
		}
		e, _ := newEngine(&fakeStore{}, cfg)
		if got := e.explainTimeout(); got != want {
			t.Errorf("%q: 得到 %v，应为 %v", in, got, want)
		}
	}
}
