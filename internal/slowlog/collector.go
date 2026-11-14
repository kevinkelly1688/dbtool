// @author: kevinkelly
// @license: Apache Licence
// @file: collector.go
// @time: 2026/10/28

package slowlog

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dbtool/internal/model"
)

// CollectorConfig 是采集器参数。环境变量：SLOW_COLLECT_INTERVAL / SLOW_COLLECT_WORKERS。
type CollectorConfig struct {
	Enabled      bool          // 是否运行采集循环：默认开启，环境变量 SLOWLOG_COLLECT_ENABLE=0/false/no/off 才整体关闭；采哪些实例由实例上的“慢日志采集”开关（is_enable）决定
	Interval     time.Duration // 两轮之间的间隔
	Workers      int           // 并行采集的实例数
	BatchLimit   int           // 单实例单批最多拉取行数
	MaxBatches   int           // 单实例每轮最多拉取批数（积压时追赶用）
	InstTimeout  time.Duration // 单实例单轮的总超时
	BackfillDays int           // 实例第一次采集时回溯的天数；0 表示从日志表最开头
	MaxBackoff   time.Duration // 连续失败后的最长退避
}

// DefaultCollectorConfig 返回默认参数（Enabled 由 ConfigFromEnv 决定，这里不开）。
func DefaultCollectorConfig() CollectorConfig {
	return CollectorConfig{Interval: 10 * time.Second, Workers: 2, BatchLimit: 1000, MaxBatches: 10,
		InstTimeout: 60 * time.Second, BackfillDays: 7, MaxBackoff: 10 * time.Minute}
}

// ConfigFromEnv 从环境变量读取参数；非法值回退到默认值并夹在合理范围内。
func ConfigFromEnv(getenv func(string) string) CollectorConfig {
	c := DefaultCollectorConfig()
	// 默认开启：用户操作的是实例上的“慢日志采集”开关，没有开关的实例一条都不会读；环境变量只留一个整体关闭的出口。
	c.Enabled = true
	switch strings.ToLower(strings.TrimSpace(getenv("SLOWLOG_COLLECT_ENABLE"))) {
	case "0", "false", "no", "off":
		c.Enabled = false
	}
	if n, ok := envInt(getenv, "SLOW_COLLECT_INTERVAL"); ok {
		c.Interval = time.Duration(clampInt(n, 5, 3600)) * time.Second
	}
	if n, ok := envInt(getenv, "SLOW_COLLECT_WORKERS"); ok {
		c.Workers = clampInt(n, 1, 8)
	}
	if n, ok := envInt(getenv, "SLOWLOG_COLLECT_BACKFILL_DAYS"); ok {
		c.BackfillDays = clampInt(n, 0, 365)
	}
	return c
}

func envInt(getenv func(string) string, k string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(getenv(k)))
	return n, err == nil
}

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// Collector 定时从实例拉取慢日志写入 dbt_slow_logs。
//
//   - 采集开关是 dbt_sql_instance.is_enable；单实例失败只记录状态，不影响其他实例；连续失败会指数退避；
//   - 游标 = 该实例 dbt_slow_logs 里最大的 updated_at_micro；写入前再按
//     (游标, 指纹, 用户@主机) 与库里已有记录去重，避免重复写入。
type Collector struct {
	cfg  CollectorConfig
	repo CollectRepo
	src  Source
	now  func() time.Time

	mu       sync.Mutex
	states   map[uint]CollectState
	failures map[uint]backoff
	warned   sync.Once
	seeded   sync.Once
	hooks    []func(context.Context) // 每轮 RunOnce 之后调用（Redis 慢日志采集接在这里）
}

type backoff struct {
	n    int
	next time.Time
}

// NewCollector 创建采集器。now 为空取系统时间。
func NewCollector(cfg CollectorConfig, repo CollectRepo, src Source, now func() time.Time) *Collector {
	d := DefaultCollectorConfig()
	if cfg.Interval <= 0 {
		cfg.Interval = d.Interval
	}
	if cfg.Workers <= 0 {
		cfg.Workers = d.Workers
	}
	if cfg.BatchLimit <= 0 {
		cfg.BatchLimit = d.BatchLimit
	}
	if cfg.MaxBatches <= 0 {
		cfg.MaxBatches = d.MaxBatches
	}
	if cfg.InstTimeout <= 0 {
		cfg.InstTimeout = d.InstTimeout
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = d.MaxBackoff
	}
	if now == nil {
		now = time.Now
	}
	return &Collector{cfg: cfg, repo: repo, src: src, now: now, states: map[uint]CollectState{}, failures: map[uint]backoff{}}
}

// AddHook 登记一个每轮采集之后调用的函数（与 MySQL 采集共用开关、间隔与退出信号）。只应在启动时调用。
func (c *Collector) AddHook(f func(context.Context)) { c.hooks = append(c.hooks, f) }

func (c *Collector) Enabled() bool { return c.cfg.Enabled }

// States 返回本进程内记录的各实例最近一次采集状态（副本）。
func (c *Collector) States() map[uint]CollectState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[uint]CollectState, len(c.states))
	for k, v := range c.states {
		out[k] = v
	}
	return out
}

// Run 是后台任务：每隔 Interval 采集一轮，ctx 取消时返回。未启用时立即返回。
// 一轮没结束不会开始下一轮（不叠加）。
func (c *Collector) Run(ctx context.Context) {
	if !c.cfg.Enabled {
		slog.Info("慢日志采集器已被环境变量 SLOWLOG_COLLECT_ENABLE 关闭（去掉该变量或设为 1 并重启即可恢复；采哪些实例由实例上的“慢日志采集”开关决定）")
		return
	}
	slog.Info("慢日志采集器已启动", "interval", c.cfg.Interval, "workers", c.cfg.Workers)
	for {
		c.RunOnce(ctx)
		for _, h := range c.hooks {
			h(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.cfg.Interval):
		}
	}
}

// RunOnce 采集一轮：所有开启采集开关的实例，最多 Workers 个并行。
func (c *Collector) RunOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	insts, err := c.repo.CollectInstances()
	if err != nil {
		slog.Error("读取待采集实例失败", "err", err)
		return
	}
	c.seeded.Do(c.seedStates)
	sem := make(chan struct{}, c.cfg.Workers)
	var wg sync.WaitGroup
	for i := range insts {
		inst := insts[i]
		if !c.due(inst.ID) {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			c.collectOne(ctx, &inst)
		}()
	}
	wg.Wait()
}

// seedStates 用库里保存的状态补齐进程内状态（重启后累计写入数从库里的值继续累加）。
func (c *Collector) seedStates() {
	saved, ok, err := c.repo.LoadStates()
	if err != nil || !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, st := range saved {
		if _, exists := c.states[id]; !exists {
			c.states[id] = st
		}
	}
}

// due 判断实例是否过了退避期。
func (c *Collector) due(id uint) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.failures[id]
	return !ok || !c.now().Before(b.next)
}

func (c *Collector) collectOne(ctx context.Context, inst *model.Instance) {
	start := c.now()
	c.mu.Lock()
	prev := c.states[inst.ID]
	c.mu.Unlock()
	st := CollectState{InstanceID: inst.ID, InstanceName: inst.InstanceName, LastRunAt: start, LastOKAt: prev.LastOKAt, TotalRows: prev.TotalRows}

	n, err := c.pull(ctx, inst)
	st.DurationMs = c.now().Sub(start).Milliseconds()
	switch {
	case err != nil:
		st.Status, st.Error = "error", errText(err)
		c.fail(inst.ID)
		slog.Warn("慢日志采集失败", "instance", inst.InstanceName, "err", st.Error)
	default:
		st.Status, st.LastRows, st.LastOKAt = "ok", n, c.now()
		st.TotalRows += int64(n)
		c.mu.Lock()
		delete(c.failures, inst.ID)
		c.mu.Unlock()
		if n > 0 {
			slog.Info("慢日志采集", "instance", inst.InstanceName, "rows", n)
		}
	}
	c.mu.Lock()
	c.states[inst.ID] = st
	c.mu.Unlock()
	if err := c.repo.SaveState(st); err != nil {
		c.warned.Do(func() {
			slog.Warn("保存采集状态失败（是否已执行 deploy/sql/015_slowlog.sql？），页面只显示本进程内的状态", "err", err)
		})
	}
}

func (c *Collector) fail(id uint) {
	c.mu.Lock()
	n := c.failures[id].n + 1
	c.mu.Unlock()
	d := c.cfg.Interval
	for i := 1; i < n && d < c.cfg.MaxBackoff; i++ {
		d *= 2
	}
	if d > c.cfg.MaxBackoff {
		d = c.cfg.MaxBackoff
	}
	c.mu.Lock()
	c.failures[id] = backoff{n: n, next: c.now().Add(d)}
	c.mu.Unlock()
}

func (c *Collector) setBackoff(id uint, d time.Duration) {
	c.mu.Lock()
	c.failures[id] = backoff{n: c.failures[id].n, next: c.now().Add(d)}
	c.mu.Unlock()
}

// pull 对单个实例拉取并写入，返回写入的条数。panic 会被转成错误，不会影响其他实例。
func (c *Collector) pull(ctx context.Context, inst *model.Instance) (n int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("采集异常：%v", r)
		}
	}()
	// 配置了 SSH 隧道的实例不再跳过：src 通过 store.EngineConn 取连接信息，已自动改写为本地转发器地址。
	ictx, cancel := context.WithTimeout(ctx, c.cfg.InstTimeout)
	defer cancel()

	wm, err := c.repo.Watermark(inst.ID)
	if err != nil {
		return 0, fmt.Errorf("读取游标: %w", err)
	}
	if wm == 0 && c.cfg.BackfillDays > 0 {
		wm = c.now().AddDate(0, 0, -c.cfg.BackfillDays).UnixMicro()
	}
	total := 0
	for b := 0; b < c.cfg.MaxBatches; b++ {
		entries, err := c.src.Fetch(ictx, inst, wm, c.cfg.BatchLimit)
		if err != nil {
			return total, err
		}
		if len(entries) == 0 {
			break
		}
		full := len(entries) >= c.cfg.BatchLimit
		if full {
			entries = trimTail(entries)
		}
		wrote, err := c.save(inst.ID, entries)
		total += wrote
		if err != nil {
			return total, err
		}
		last := entries[len(entries)-1].Micro
		if last <= wm { // 没有前进，避免死循环
			break
		}
		wm = last
		if !full {
			break
		}
	}
	return total, nil
}

// trimTail 去掉末尾与最后一条同游标的记录（批被 LIMIT 截断时它们可能没取全，留给下一批）。
// 全部同游标时原样保留，避免永远前进不了。
func trimTail(es []Entry) []Entry {
	last := es[len(es)-1].Micro
	i := len(es)
	for i > 0 && es[i-1].Micro == last {
		i--
	}
	if i == 0 {
		return es
	}
	return es[:i]
}

// entryKey 是去重键：游标 + 指纹 + 用户@主机。
func entryKey(micro int64, md5, userHost string) string {
	return strconv.FormatInt(micro, 10) + "|" + md5 + "|" + userHost
}

// maxStoredSQL 是写入 slow_logs.sql_text（TEXT，64KB）的字节上限。
const maxStoredSQL = 60000

func (c *Collector) save(instanceID uint, entries []Entry) (int, error) {
	minM, maxM := entries[0].Micro, entries[0].Micro
	for i := range entries {
		e := &entries[i]
		e.SQLMD5 = SQLMD5(e.SQLText)
		e.SQLText = truncateUTF8(e.SQLText, maxStoredSQL)
		if e.Micro < minM {
			minM = e.Micro
		}
		if e.Micro > maxM {
			maxM = e.Micro
		}
	}
	exist, err := c.repo.ExistingKeys(instanceID, minM, maxM)
	if err != nil {
		return 0, fmt.Errorf("去重检查: %w", err)
	}
	fresh := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if !exist[entryKey(e.Micro, e.SQLMD5, e.UserHost)] {
			fresh = append(fresh, e)
		}
	}
	if len(fresh) == 0 {
		return 0, nil
	}
	n, err := c.repo.InsertSlowLogs(instanceID, fresh)
	if err != nil {
		return n, fmt.Errorf("写入 dbt_slow_logs: %w", err)
	}
	return n, nil
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// errText 把错误整理成可展示的一行（去换行、限长）。连接错误不含密码。
func errText(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if r := []rune(s); len(r) > 480 {
		s = string(r[:480]) + "…"
	}
	return s
}
