// @author: kevinkelly
// @license: Apache Licence
// @file: collector_test.go
// @time: 2026/10/28

package slowlog

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"dbtool/internal/model"
)

// 可调的假时钟。
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func entry(micro int64, sql string) Entry {
	return Entry{Micro: micro, StartTime: "2026-10-06 10:00:00.000000", UserHost: "app[app] @  [10.0.0.9]", QueryTimeSec: 1.5, LockTimeSec: 0.1,
		RowsSent: 1, RowsExamined: 1000, DBName: "shop", SQLText: sql}
}

func newCollector(r *fakeRepo, src Source, cfg CollectorConfig) (*Collector, *clock) {
	ck := &clock{t: time.Date(2026, 10, 6, 12, 0, 0, 0, time.Local)}
	cfg.Enabled = true
	return NewCollector(cfg, r, src, ck.now), ck
}

func TestCollectorWritesRowsAndState(t *testing.T) {
	r, src := newFakeRepo(), newFakeSource()
	r.instances = []model.Instance{{ID: 1, InstanceName: "a", DBType: "mysql", IsEnable: true}, {ID: 2, InstanceName: "off", DBType: "mysql"}}
	base := time.Date(2026, 10, 6, 11, 0, 0, 0, time.Local).UnixMicro()
	src.rows[1] = []Entry{entry(base+1, "select * from t where id = 1"), entry(base+2, "SELECT * FROM t WHERE id = 2")}
	c, ck := newCollector(r, src, CollectorConfig{})

	c.RunOnce(context.Background())
	if len(r.slowLogs[1]) != 2 || len(r.slowLogs[2]) != 0 {
		t.Fatalf("应只采集开启开关的实例: %v", r.slowLogs)
	}
	got := r.slowLogs[1]
	if got[0].SQLMD5 == "" || got[0].SQLMD5 != got[1].SQLMD5 || got[0].SQLMD5 != SQLMD5("select * from t where id = 9") {
		t.Errorf("只差字面量的 SQL 应有相同指纹: %q %q", got[0].SQLMD5, got[1].SQLMD5)
	}
	st := r.states[1]
	if st.Status != "ok" || st.LastRows != 2 || st.TotalRows != 2 || st.LastOKAt.IsZero() || st.InstanceName != "a" {
		t.Errorf("状态: %+v", st)
	}
	if c.States()[1].LastRows != 2 {
		t.Error("进程内状态")
	}

	// 第二轮没有新数据：游标 = 已有最大游标，不会重复写入
	ck.add(10 * time.Second)
	c.RunOnce(context.Background())
	if len(r.slowLogs[1]) != 2 {
		t.Errorf("没有新数据不应写入: %d", len(r.slowLogs[1]))
	}
	calls := src.calls[1]
	if len(calls) != 2 || calls[1] != base+2 {
		t.Errorf("第二轮应从上一轮的最大游标开始拉取: %v", calls)
	}
	if st = r.states[1]; st.LastRows != 0 || st.TotalRows != 2 || st.Status != "ok" {
		t.Errorf("第二轮状态: %+v", st)
	}
	// 新数据到达
	src.rows[1] = append(src.rows[1], entry(base+3, "select 1"))
	c.RunOnce(context.Background())
	if len(r.slowLogs[1]) != 3 || r.states[1].TotalRows != 3 {
		t.Errorf("增量: %d %+v", len(r.slowLogs[1]), r.states[1])
	}
}

func TestCollectorDedupesAgainstExistingRows(t *testing.T) {
	r, src := newFakeRepo(), newFakeSource()
	r.instances = []model.Instance{{ID: 1, InstanceName: "a", DBType: "mysql", IsEnable: true}}
	base := time.Date(2026, 10, 6, 11, 0, 0, 0, time.Local).UnixMicro()
	// 另一个采集任务已经写入了同一条（游标、指纹、用户@主机相同），但游标水位线落后（比如它正在写）
	e := entry(base+5, "select * from t where id = 1")
	pre := e
	pre.SQLMD5 = SQLMD5(e.SQLText)
	r.slowLogs[1] = []Entry{pre}
	r.watermark[1] = base // 水位线比已有记录旧，模拟读到了重叠区间
	src.rows[1] = []Entry{entry(base+3, "select 2"), e, entry(base+6, "select 3")}
	c, _ := newCollector(r, src, CollectorConfig{})
	c.RunOnce(context.Background())
	// 注意 Watermark 取“记录里的最大游标”(base+5)，所以 base+3 与 base+5 不会被再次拉取；这里验证 ExistingKeys 兜底
	if n := len(r.slowLogs[1]); n != 2 { // 已有 1 + 新增 base+6
		t.Fatalf("应只新增一条: %d", n)
	}

	// 直接验证去重：同游标同指纹同用户的条目被丢弃，不同用户的保留
	r2 := newFakeRepo()
	r2.slowLogs[1] = []Entry{pre}
	c2 := NewCollector(CollectorConfig{Enabled: true}, r2, newFakeSource(), nil)
	other := e
	other.UserHost = "other[o] @ [10.0.0.8]"
	n, err := c2.save(1, []Entry{e, other})
	if err != nil || n != 1 || len(r2.slowLogs[1]) != 2 || r2.slowLogs[1][1].UserHost != other.UserHost {
		t.Fatalf("去重: %d %v %+v", n, err, r2.slowLogs[1])
	}
}

func TestCollectorFailureIsolationAndBackoff(t *testing.T) {
	r, src := newFakeRepo(), newFakeSource()
	r.instances = []model.Instance{
		{ID: 1, InstanceName: "bad", DBType: "mysql", IsEnable: true},
		{ID: 2, InstanceName: "good", DBType: "mysql", IsEnable: true},
		{ID: 3, InstanceName: "panics", DBType: "doris", IsEnable: true},
	}
	src.errs[1] = errBoom
	src.panics[3] = true
	base := time.Date(2026, 10, 6, 11, 0, 0, 0, time.Local).UnixMicro()
	src.rows[2] = []Entry{entry(base+1, "select 1")}
	c, ck := newCollector(r, src, CollectorConfig{Interval: 10 * time.Second, MaxBackoff: time.Minute, Workers: 1})

	c.RunOnce(context.Background())
	if len(r.slowLogs[2]) != 1 {
		t.Fatal("一个实例失败不能影响其他实例")
	}
	if st := r.states[1]; st.Status != "error" || !strings.Contains(st.Error, "connection refused") || st.LastRows != 0 {
		t.Errorf("失败状态: %+v", st)
	}
	if st := r.states[3]; st.Status != "error" || !strings.Contains(st.Error, "采集异常") {
		t.Errorf("panic 应转成错误状态: %+v", st)
	}
	if r.states[2].Status != "ok" {
		t.Errorf("good: %+v", r.states[2])
	}

	// 退避：10 秒后（第一次失败退避 = Interval）才会再试；第二次失败退避翻倍
	n := len(src.calls[1])
	ck.add(5 * time.Second)
	c.RunOnce(context.Background())
	if len(src.calls[1]) != n {
		t.Error("退避期内不应再拉取失败的实例")
	}
	ck.add(6 * time.Second)
	c.RunOnce(context.Background())
	if len(src.calls[1]) != n+1 {
		t.Errorf("退避期过后应重试: %d", len(src.calls[1]))
	}
	ck.add(15 * time.Second) // 第二次失败退避 20 秒，15 秒内不重试
	c.RunOnce(context.Background())
	if len(src.calls[1]) != n+1 {
		t.Error("第二次失败应退避更久")
	}
	// 恢复：成功一次后清除退避，状态回到 ok
	delete(src.errs, 1)
	src.rows[1] = []Entry{entry(base+9, "select 9")}
	ck.add(10 * time.Second)
	c.RunOnce(context.Background())
	if st := r.states[1]; st.Status != "ok" || st.LastRows != 1 || st.Error != "" {
		t.Errorf("恢复后: %+v", st)
	}
	ck.add(time.Second)
	m := len(src.calls[1])
	c.RunOnce(context.Background())
	if len(src.calls[1]) != m+1 {
		t.Error("成功后不应再退避")
	}
}

func TestCollectorTunnelInstanceCollectedAndSaveErrors(t *testing.T) {
	r, src := newFakeRepo(), newFakeSource()
	tid := uint(5)
	r.instances = []model.Instance{{ID: 1, InstanceName: "tun", DBType: "mysql", IsEnable: true, TunnelID: &tid}, {ID: 2, InstanceName: "ok", DBType: "mysql", IsEnable: true}}
	r.saveErr = errBoom // 状态表不可用不能影响采集
	c, _ := newCollector(r, src, CollectorConfig{})
	c.RunOnce(context.Background())
	if len(src.calls[1]) == 0 {
		t.Error("带 SSH 隧道的实例也要采集（连接经 store.EngineConn 自动走隧道）")
	}
	if st := c.States()[1]; st.Status != "ok" {
		t.Errorf("隧道实例不应再被标为 skipped: %+v", st)
	}
	if c.States()[2].Status != "ok" {
		t.Error("其他实例照常采集")
	}
	// 写入失败：状态 error，不推进
	r.insertErr = errBoom
	src.rows[2] = []Entry{entry(time.Now().UnixMicro(), "select 1")}
	c.RunOnce(context.Background())
	if st := c.States()[2]; st.Status != "error" || !strings.Contains(st.Error, "写入 dbt_slow_logs") {
		t.Errorf("%+v", st)
	}
}

func TestCollectorBatchesAndBackfill(t *testing.T) {
	r, src := newFakeRepo(), newFakeSource()
	r.instances = []model.Instance{{ID: 1, InstanceName: "a", DBType: "mysql", IsEnable: true}}
	c, ck := newCollector(r, src, CollectorConfig{BatchLimit: 2, MaxBatches: 5, BackfillDays: 7})
	// 第一次采集：游标从 7 天前开始，而不是 0
	c.RunOnce(context.Background())
	want := ck.now().AddDate(0, 0, -7).UnixMicro()
	if len(src.calls[1]) != 1 || src.calls[1][0] != want {
		t.Fatalf("回溯起点: %v want %d", src.calls[1], want)
	}
	// 积压 5 条、每批 2 条：一轮内连续拉取直到取完；批尾同游标的记录留给下一批，不丢不重
	base := ck.now().UnixMicro() - 1000
	src.rows[1] = []Entry{entry(base+1, "q1"), entry(base+2, "q2"), entry(base+2, "q2b"), entry(base+3, "q3"), entry(base+4, "q4")}
	c.RunOnce(context.Background())
	if len(r.slowLogs[1]) != 5 {
		t.Fatalf("应全部采集（含同游标的两条）: %d %+v", len(r.slowLogs[1]), r.slowLogs[1])
	}
	seen := map[string]int{}
	for _, e := range r.slowLogs[1] {
		seen[e.SQLText]++
	}
	for k, v := range seen {
		if v != 1 {
			t.Errorf("%s 写入了 %d 次", k, v)
		}
	}
	// BackfillDays=0：从日志表最开头
	r2, src2 := newFakeRepo(), newFakeSource()
	r2.instances = r.instances
	c2, _ := newCollector(r2, src2, CollectorConfig{BackfillDays: 0})
	c2.cfg.BackfillDays = 0
	c2.RunOnce(context.Background())
	if src2.calls[1][0] != 0 {
		t.Errorf("BackfillDays=0 应从 0 开始: %v", src2.calls[1])
	}
}

func TestTrimTail(t *testing.T) {
	mk := func(ms ...int64) []Entry {
		var out []Entry
		for _, m := range ms {
			out = append(out, Entry{Micro: m})
		}
		return out
	}
	if got := trimTail(mk(1, 2, 3, 3)); len(got) != 2 {
		t.Errorf("去掉尾部同游标: %d", len(got))
	}
	if got := trimTail(mk(1, 2, 3)); len(got) != 2 {
		t.Errorf("%d", len(got))
	}
	if got := trimTail(mk(7, 7, 7)); len(got) != 3 {
		t.Errorf("全部同游标时原样保留: %d", len(got))
	}
}

func TestCollectorWorkerLimit(t *testing.T) {
	r, src := newFakeRepo(), newFakeSource()
	for i := uint(1); i <= 6; i++ {
		r.instances = append(r.instances, model.Instance{ID: i, InstanceName: "i", DBType: "mysql", IsEnable: true})
	}
	src.block = make(chan struct{})
	c, _ := newCollector(r, src, CollectorConfig{Workers: 2})
	done := make(chan struct{})
	go func() { c.RunOnce(context.Background()); close(done) }()
	time.Sleep(50 * time.Millisecond)
	src.mu.Lock()
	running := src.running
	src.mu.Unlock()
	if running != 2 {
		t.Errorf("并发应被限制为 2，实际 %d", running)
	}
	close(src.block)
	<-done
	if src.maxRun > 2 {
		t.Errorf("最大并发 %d", src.maxRun)
	}
	if len(src.calls) != 6 {
		t.Errorf("6 个实例都应采集: %d", len(src.calls))
	}
}

func TestCollectorRunLifecycle(t *testing.T) {
	r, src := newFakeRepo(), newFakeSource()
	r.instances = []model.Instance{{ID: 1, InstanceName: "a", DBType: "mysql", IsEnable: true}}
	// 未启用：立即返回，不采集
	off := NewCollector(CollectorConfig{}, r, src, nil)
	done := make(chan struct{})
	go func() { off.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("未启用的采集器应立即返回")
	}
	if len(src.calls) != 0 {
		t.Error("未启用不应采集")
	}
	// 启用：循环采集，取消后返回
	on := NewCollector(CollectorConfig{Enabled: true, Interval: 10 * time.Millisecond}, r, src, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { on.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		src.mu.Lock()
		n := len(src.calls[1])
		src.mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ctx 取消后应返回")
	}
	if len(src.calls[1]) < 3 {
		t.Errorf("应周期性采集: %d", len(src.calls[1]))
	}
}

func TestCollectorSeedsTotalFromStoredState(t *testing.T) {
	r, src := newFakeRepo(), newFakeSource()
	r.instances = []model.Instance{{ID: 1, InstanceName: "a", DBType: "mysql", IsEnable: true}}
	r.states[1] = CollectState{InstanceID: 1, Status: "ok", TotalRows: 100}
	src.rows[1] = []Entry{entry(time.Now().UnixMicro(), "select 1")}
	c, _ := newCollector(r, src, CollectorConfig{})
	c.RunOnce(context.Background())
	if r.states[1].TotalRows != 101 {
		t.Errorf("重启后累计数应从库里的值继续: %+v", r.states[1])
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	d := ConfigFromEnv(env(nil))
	if !d.Enabled || d.Interval != 10*time.Second || d.Workers != 2 || d.BackfillDays != 7 {
		t.Errorf("默认值: %+v", d)
	}
	c := ConfigFromEnv(env(map[string]string{"SLOWLOG_COLLECT_ENABLE": "true", "SLOW_COLLECT_INTERVAL": "1", "SLOW_COLLECT_WORKERS": "99", "SLOWLOG_COLLECT_BACKFILL_DAYS": "9999"}))
	if !c.Enabled || c.Interval != 5*time.Second || c.Workers != 8 || c.BackfillDays != 365 {
		t.Errorf("夹取: %+v", c)
	}
	c = ConfigFromEnv(env(map[string]string{"SLOW_COLLECT_INTERVAL": "abc", "SLOW_COLLECT_WORKERS": "0", "SLOWLOG_COLLECT_ENABLE": "no"}))
	if c.Enabled || c.Interval != 10*time.Second || c.Workers != 1 {
		t.Errorf("非法值: %+v", c)
	}
	// 默认开启：不设、空、1、true、乱写的值都是开；只有 0 / false / no / off（不区分大小写、忽略空白）才关
	for _, v := range []string{"", "1", "true", "on", "yes", "whatever"} {
		if !ConfigFromEnv(env(map[string]string{"SLOWLOG_COLLECT_ENABLE": v})).Enabled {
			t.Errorf("SLOWLOG_COLLECT_ENABLE=%q 应开启", v)
		}
	}
	for _, v := range []string{"0", "false", "FALSE", "no", "off", " Off "} {
		if ConfigFromEnv(env(map[string]string{"SLOWLOG_COLLECT_ENABLE": v})).Enabled {
			t.Errorf("SLOWLOG_COLLECT_ENABLE=%q 应关闭", v)
		}
	}
}

func TestTruncateAndErrText(t *testing.T) {
	s := strings.Repeat("中", 10) // 每个 3 字节
	got := truncateUTF8(s, 10)
	if !utf8.ValidString(got) || len(got) != 9 {
		t.Errorf("不能截断在字符中间: %d %q", len(got), got)
	}
	if truncateUTF8("abc", 10) != "abc" {
		t.Error("短文本原样返回")
	}
	long := errText(errBoom)
	if long != "connection refused" {
		t.Error(long)
	}
	if e := errText(&Error{Message: "a\nb\t c"}); e != "a b c" {
		t.Errorf("折叠空白: %q", e)
	}
	if e := errText(&Error{Message: strings.Repeat("x", 600)}); len([]rune(e)) != 481 {
		t.Errorf("限长: %d", len([]rune(e)))
	}
	// 写入前截断超长 SQL，但指纹基于完整文本
	r := newFakeRepo()
	c := NewCollector(CollectorConfig{Enabled: true}, r, newFakeSource(), nil)
	big := "select " + strings.Repeat("a", 70000)
	if _, err := c.save(1, []Entry{{Micro: 1, SQLText: big}}); err != nil {
		t.Fatal(err)
	}
	if got := r.slowLogs[1][0]; len(got.SQLText) > maxStoredSQL || got.SQLMD5 != SQLMD5(big) {
		t.Errorf("截断与指纹: %d %s", len(got.SQLText), got.SQLMD5)
	}
}
