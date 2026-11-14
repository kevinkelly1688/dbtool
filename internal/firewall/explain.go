// @author: kevinkelly
// @license: Apache Licence
// @file: explain.go
// @time: 2026/10/28

package firewall

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/sqlx"
)

// EXPLAIN 事实（设计 §5.2 / §6.1，P5）：scan_rows 条件需要“这条语句预估会扫描多少行”。
//
//   - 只在需要时取：条件按成本排序（词法 → 库表 → EXPLAIN），“全部满足”的规则里前面的条件已经不满足 / 未知、
//     “任一满足”的规则里前面的条件已经命中，就不再为 EXPLAIN 付代价；同一条语句的多条规则共用一次 EXPLAIN；
//   - 限时（firewall.explain_timeout_ms，缺省 3 秒）、同一语句短期缓存（成功 10 秒、失败 15 秒）、同一实例最多 4 路并发；
//   - 实例连续出现超时 / 连不上时熔断 30 秒：期间不再尝试，条件按“未评估”处理，不会拖慢每一次查询；
//   - 取不到（超时、熔断、语句类型不支持、实例不是 MySQL 兼容的执行计划格式）一律“未评估”，不命中，不会误拦；
//   - 实际执行 EXPLAIN 的是调用方（在线查询 / 工单 / 导出各自有连接与权限上下文），这里只负责估算与保护。

// 默认值与限制。
const (
	DefaultExplainTimeout = 3 * time.Second
	minExplainTimeout     = 200 * time.Millisecond
	maxExplainTimeout     = 30 * time.Second

	explainOKTTL       = 10 * time.Second
	explainFailTTL     = 15 * time.Second
	explainMemoMax     = 512
	explainPerInstance = 4
	breakerFailures    = 3
	breakerOpenFor     = 30 * time.Second
)

// ErrExplainUnavailable 表示这次 EXPLAIN 因为连不上实例等原因没有执行（不是语句本身的问题）。
// PlanFunc 的实现用 fmt.Errorf("%w: …", ErrExplainUnavailable) 包装连接类错误，熔断只统计它与超时。
var ErrExplainUnavailable = errors.New("EXPLAIN 暂时不可用")

// errBreakerOpen 熔断期间不再尝试 EXPLAIN。
var errBreakerOpen = errors.New("该实例的 EXPLAIN 连续失败，已暂停尝试 30 秒")

// SupportsExplain 报告该实例类型的 EXPLAIN 输出是否是 MySQL 格式（有 id / rows / filtered 列）。
func SupportsExplain(dbType string) bool {
	switch dbType {
	case "mysql", "polardb", "adb":
		return true
	}
	return false
}

// WrapPlanError 把执行 EXPLAIN 时遇到的“连不上 / 连接中断 / 网络超时”类错误包装成 ErrExplainUnavailable
// （熔断只统计这类实例不可用的失败）；语句自己的错误（表不存在、语法错误……）原样返回。
func WrapPlanError(err error) error {
	if err == nil || errors.Is(err, ErrExplainUnavailable) {
		return err
	}
	var ne net.Error
	msg := strings.ToLower(err.Error())
	switch {
	case errors.As(err, &ne), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, driver.ErrBadConn):
		return fmt.Errorf("%w: %v", ErrExplainUnavailable, err)
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "bad connection"), strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "connection reset"), strings.Contains(msg, "no such host"), strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "dial tcp"), strings.Contains(msg, "too many connections"):
		return fmt.Errorf("%w: %v", ErrExplainUnavailable, err)
	}
	return err
}

// PlanFunc 在目标实例上执行 EXPLAIN 语句，返回结果列与行。timeout 是本次的时限（调用方应同时设成服务端的最长执行时间）。
// 连接 / 打开引擎失败请返回包装了 ErrExplainUnavailable 的错误。
type PlanFunc func(ctx context.Context, explainSQL string, timeout time.Duration) (cols []string, rows [][]any, err error)

// ExplainSQL 返回对一条语句做 EXPLAIN 用的语句。只支持 SELECT / WITH … SELECT / INSERT / REPLACE / UPDATE / DELETE；
// 其他语句（DDL、SHOW、EXPLAIN 本身……）没有扫描行数，返回错误（条件按“未评估”处理）。
func ExplainSQL(stmt string) (string, error) {
	st := strings.TrimSpace(stmt)
	if parts := sqlx.Split(st); len(parts) > 0 {
		st = parts[0]
	}
	switch kw := sqlx.FirstKeyword(st); kw {
	case "select", "insert", "replace", "update", "delete":
		return "EXPLAIN " + st, nil
	case "with":
		if sqlx.WithMainKeyword(st) != "" {
			return "EXPLAIN " + st, nil
		}
	case "":
		return "", errors.New("语句为空")
	default:
		return "", fmt.Errorf("%s 语句没有可估算的扫描行数", strings.ToUpper(kw))
	}
	return "", errors.New("无法识别 WITH 语句的主语句")
}

// ScanRowsFromPlan 由 EXPLAIN 结果估算语句预估检查的行数。
//
// 每个 id 组（主查询 / 子查询 / 派生表各一组）按计划行顺序（即连接顺序）做嵌套循环估算：
// 第 i 张表检查的行数 = 前面各表的 rows × filtered% 之积 × 本表 rows；各组之和为总数。
// 这不是“返回多少行”（那是 query.EstimateRows 的口径），而是“要翻多少行”：单表 WHERE 命中索引时就是 rows 列，
// 全表扫描就是表的估算行数，连接则按驱动表过滤后的行数放大。rows 为 NULL 的计划行（Impossible WHERE、UNION RESULT）不计。
func ScanRowsFromPlan(cols []string, rows [][]any) (int64, error) {
	idx := func(name string) int {
		for i, c := range cols {
			if strings.EqualFold(strings.TrimSpace(c), name) {
				return i
			}
		}
		return -1
	}
	ii, ri, fi := idx("id"), idx("rows"), idx("filtered")
	if ri < 0 {
		return 0, errors.New("执行计划里没有 rows 列（实例的 EXPLAIN 输出不是 MySQL 格式）")
	}
	if len(rows) == 0 {
		return 0, errors.New("执行计划为空")
	}
	type group struct {
		order   int
		running float64
		total   float64
	}
	groups := map[string]*group{}
	var keys []string
	for n, r := range rows {
		if ri >= len(r) {
			continue
		}
		key := "0"
		if ii >= 0 && ii < len(r) && r[ii] != nil {
			key = fmt.Sprint(r[ii])
		}
		g := groups[key]
		if g == nil {
			g = &group{order: n, running: 1}
			groups[key] = g
			keys = append(keys, key)
		}
		cnt, ok := planNumber(r[ri])
		if !ok {
			continue // rows 为 NULL：不扫描
		}
		pct := 100.0
		if fi >= 0 && fi < len(r) && r[fi] != nil {
			if v, ok := planNumber(r[fi]); ok {
				pct = v
			}
		}
		g.total += g.running * cnt
		g.running *= cnt * pct / 100
		if g.running > 1e15 {
			g.running = 1e15
		}
	}
	sort.Slice(keys, func(i, j int) bool { return groups[keys[i]].order < groups[keys[j]].order })
	total := 0.0
	for _, k := range keys {
		total += groups[k].total
	}
	switch {
	case math.IsNaN(total) || total < 0:
		total = 0
	case total > 9e15:
		total = 9e15
	}
	return int64(total), nil
}

func planNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	case []byte:
		f, err := strconv.ParseFloat(strings.TrimSpace(string(x)), 64)
		return f, err == nil
	}
	return 0, false
}

// ---- 限时 / 缓存 / 熔断 / 统计 ----

type explainEntry struct {
	n     int64
	err   error
	until time.Time
}

type breaker struct {
	fails     int
	openUntil time.Time
}

// ExplainStats 是本进程里 EXPLAIN 事实的使用情况（概览页展示；多实例部署各进程各自统计）。
type ExplainStats struct {
	Calls     int64  `json:"calls"`      // 真正下发给实例的 EXPLAIN 次数
	Failures  int64  `json:"failures"`   // 其中失败的（含超时）
	Timeouts  int64  `json:"timeouts"`   // 其中超时的
	CacheHits int64  `json:"cache_hits"` // 命中短期缓存（含缓存的失败）
	Skipped   int64  `json:"skipped"`    // 熔断期间跳过的次数
	TotalMs   int64  `json:"total_ms"`   // 下发的 EXPLAIN 总耗时
	MaxMs     int64  `json:"max_ms"`
	Open      []uint `json:"open"` // 当前处于熔断期的实例 ID
}

type explainState struct {
	mu      sync.Mutex
	memo    map[string]explainEntry
	brk     map[uint]*breaker
	sems    map[uint]chan struct{}
	stats   ExplainStats
	nowFunc func() time.Time
}

func newExplainState() *explainState {
	return &explainState{memo: map[string]explainEntry{}, brk: map[uint]*breaker{}, sems: map[uint]chan struct{}{}}
}

func (e *Engine) explainTimeout() time.Duration {
	t := e.cachedSettings().ExplainTimeout
	switch {
	case t <= 0:
		return DefaultExplainTimeout
	case t < minExplainTimeout:
		return minExplainTimeout
	case t > maxExplainTimeout:
		return maxExplainTimeout
	}
	return t
}

// EstimateScan 估算一条语句在实例上预估检查的行数（EXPLAIN）。plan 由调用方提供（它持有到目标实例的连接）。
// 错误一律意味着“取不到”，调用方（条件求值）把它当“未评估”。并发安全。
func (e *Engine) EstimateScan(ctx context.Context, instanceID uint, db, stmt string, plan PlanFunc) (int64, error) {
	q, err := ExplainSQL(stmt)
	if err != nil {
		return 0, err
	}
	st := e.ex
	key := strconv.FormatUint(uint64(instanceID), 10) + "\x00" + db + "\x00" + q
	now := e.now()

	st.mu.Lock()
	if m, ok := st.memo[key]; ok && now.Before(m.until) {
		st.stats.CacheHits++
		st.mu.Unlock()
		return m.n, m.err
	}
	if b := st.brk[instanceID]; b != nil && now.Before(b.openUntil) {
		st.stats.Skipped++
		st.mu.Unlock()
		return 0, errBreakerOpen
	}
	sem := st.sems[instanceID]
	if sem == nil {
		sem = make(chan struct{}, explainPerInstance)
		st.sems[instanceID] = sem
	}
	st.mu.Unlock()

	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	timeout := e.explainTimeout()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	cols, rows, perr := plan(cctx, q, timeout)
	dur := time.Since(start)
	var n int64
	if perr == nil {
		n, perr = ScanRowsFromPlan(cols, rows)
	}
	timedOut := perr != nil && (errors.Is(perr, context.DeadlineExceeded) || cctx.Err() == context.DeadlineExceeded)
	if timedOut {
		perr = fmt.Errorf("EXPLAIN 超过 %d 毫秒没有返回", timeout.Milliseconds())
	} else if perr != nil && !errors.Is(perr, ErrExplainUnavailable) {
		perr = fmt.Errorf("EXPLAIN 失败：%s", strings.TrimSpace(perr.Error()))
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	st.stats.Calls++
	st.stats.TotalMs += dur.Milliseconds()
	if ms := dur.Milliseconds(); ms > st.stats.MaxMs {
		st.stats.MaxMs = ms
	}
	if perr != nil {
		st.stats.Failures++
		if timedOut {
			st.stats.Timeouts++
		}
	}
	if ctx.Err() != nil {
		return n, perr // 调用方自己取消了：不缓存，也不算实例的问题
	}
	// 熔断只统计“实例不可用”类失败（超时 / 连不上）；语句自己的错误（表不存在等）与实例健康无关
	b := st.brk[instanceID]
	if b == nil {
		b = &breaker{}
		st.brk[instanceID] = b
	}
	switch {
	case perr == nil:
		b.fails = 0
	case timedOut || errors.Is(perr, ErrExplainUnavailable):
		b.fails++
		if b.fails >= breakerFailures {
			b.openUntil = e.now().Add(breakerOpenFor)
			b.fails = 0
		}
	}
	ttl := explainOKTTL
	if perr != nil {
		ttl = explainFailTTL
	}
	if len(st.memo) >= explainMemoMax {
		for k, m := range st.memo {
			if !e.now().Before(m.until) {
				delete(st.memo, k)
			}
		}
		if len(st.memo) >= explainMemoMax {
			st.memo = map[string]explainEntry{}
		}
	}
	st.memo[key] = explainEntry{n: n, err: perr, until: e.now().Add(ttl)}
	return n, perr
}

// ExplainStats 返回 EXPLAIN 的使用统计快照。
func (e *Engine) ExplainStats() ExplainStats {
	st := e.ex
	st.mu.Lock()
	defer st.mu.Unlock()
	out := st.stats
	now := e.now()
	for id, b := range st.brk {
		if now.Before(b.openUntil) {
			out.Open = append(out.Open, id)
		}
	}
	sort.Slice(out.Open, func(i, j int) bool { return out.Open[i] < out.Open[j] })
	return out
}

// ---- 规则级统计（慢规则 / 未评估占比，设计 §11） ----

// RuleStat 是一条规则在本进程里被求值的统计（不含被范围 / 例外跳过的次数）。
type RuleStat struct {
	Evals   int64 `json:"evals"`
	Unknown int64 `json:"unknown"` // 其中有条件未能评估的次数
	TotalNs int64 `json:"total_ns"`
	MaxNs   int64 `json:"max_ns"`
}

type ruleStats struct {
	mu sync.Mutex
	m  map[uint]*RuleStat
}

func (r *ruleStats) observe(id uint, d time.Duration, hadUnknown bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.m[id]
	if s == nil {
		if len(r.m) >= MaxRules*2 { // 规则被删改后遗留的 ID：防止无限增长
			r.m = map[uint]*RuleStat{}
		}
		s = &RuleStat{}
		r.m[id] = s
	}
	s.Evals++
	if hadUnknown {
		s.Unknown++
	}
	s.TotalNs += d.Nanoseconds()
	if n := d.Nanoseconds(); n > s.MaxNs {
		s.MaxNs = n
	}
}

// RuleStats 返回各规则的求值统计快照（键为规则 ID）。
func (e *Engine) RuleStats() map[uint]RuleStat {
	e.rs.mu.Lock()
	defer e.rs.mu.Unlock()
	out := make(map[uint]RuleStat, len(e.rs.m))
	for id, s := range e.rs.m {
		out[id] = *s
	}
	return out
}

// ruleObserver 是 evaluate 的可选观察者（只有线上的 Engine 提供；离线的 Simulator 不统计）。
type ruleObserver interface {
	observe(id uint, d time.Duration, hadUnknown bool)
}
