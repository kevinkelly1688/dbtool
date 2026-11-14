// @author: kevinkelly
// @license: Apache Licence
// @file: health.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/model"
)

// 实例健康探测：实例列表的“状态 / QPS / 连接 / 版本”列。
// 页面逐行异步调用；这里保证三件事：结果有短缓存（页面反复刷新不会反复连库）、同时探测的实例数有上限
// （一页 200 个实例也不会把平台和目标库一起压垮）、慢实例拖不住别人（单次探测有独立的短超时，超时就显示“未知”）。

const (
	healthTTL         = 30 * time.Second // 缓存有效期
	healthProbeTime   = 8 * time.Second  // 单个实例一次探测的总时限（含连接）
	healthMaxParallel = 6                // 同时探测的实例数上限
	healthQueueWait   = 10 * time.Second // 排队等探测名额的最长时间
	qpsMaxGap         = 10 * time.Minute // 两次采样相隔太久就不做差，改用启动以来的平均值
	connWarnPct       = 80               // 连接使用率达到该值判为“告警”
)

// 健康状态。
const (
	HealthOK       = "ok"
	HealthWarn     = "warn"
	HealthUnknown  = "unknown" // 探测失败（连不上、超时、不支持探测该类型）
	HealthDisabled = "disabled"
)

// Health 是一个实例的健康探测结果。
type Health struct {
	ID             uint    `json:"id"`
	Status         string  `json:"status"`
	Message        string  `json:"message,omitempty"` // 未知 / 告警的原因
	Version        string  `json:"version"`
	Connections    int64   `json:"connections"`
	MaxConnections int64   `json:"max_connections"`
	ThreadsRunning int64   `json:"threads_running"`
	QPS            float64 `json:"qps"`
	QPSKind        string  `json:"qps_kind"` // delta 两次采样之差 / avg 启动以来平均 / 空 没有
	LatencyMs      int64   `json:"latency_ms"`
	CheckedAt      string  `json:"checked_at"`
	Cached         bool    `json:"cached"`
}

type qpsSample struct {
	questions int64
	uptime    int64
	at        time.Time
}

type healthEntry struct {
	h      Health
	at     time.Time
	sample *qpsSample
}

type healthCache struct {
	mu       sync.Mutex
	m        map[uint]*healthEntry
	locks    map[uint]*sync.Mutex // 每个实例同时只探测一次，其余请求等它的结果
	sem      chan struct{}
	probeFor time.Duration
	queueFor time.Duration
	ttl      time.Duration
}

func newHealthCache() *healthCache {
	return &healthCache{m: map[uint]*healthEntry{}, locks: map[uint]*sync.Mutex{}, sem: make(chan struct{}, healthMaxParallel),
		probeFor: healthProbeTime, queueFor: healthQueueWait, ttl: healthTTL}
}

func (h *healthCache) lockOf(id uint) *sync.Mutex {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		h.locks[id] = l
	}
	return l
}

func (h *healthCache) get(id uint, now time.Time) (*healthEntry, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.m[id]
	if e == nil || now.Sub(e.at) >= h.ttl {
		return e, false
	}
	return e, true
}

func (h *healthCache) last(id uint) *healthEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.m[id]
}

func (h *healthCache) put(id uint, e *healthEntry) {
	h.mu.Lock()
	h.m[id] = e
	h.mu.Unlock()
}

// Health 探测实例健康。force 为 true 时忽略缓存。已停用的实例不连接，直接返回 disabled。
// 探测失败不是错误：返回 Status=unknown 并说明原因（页面显示“未知”）。
func (s *Service) Health(ctx context.Context, c Caller, id uint, force bool) (*Health, error) {
	inst, err := s.visibleInstance(c, id)
	if err != nil {
		return nil, err
	}
	if s.extOf([]uint{id})[id].Disabled {
		return &Health{ID: id, Status: HealthDisabled, CheckedAt: s.now().Format("2006-01-02 15:04:05")}, nil
	}
	hc := s.hc
	if !force {
		if e, ok := hc.get(id, s.now()); ok {
			h := e.h
			h.Cached = true
			return &h, nil
		}
	}
	l := hc.lockOf(id)
	l.Lock()
	defer l.Unlock()
	if !force { // 排队期间别人可能已经探测完了
		if e, ok := hc.get(id, s.now()); ok {
			h := e.h
			h.Cached = true
			return &h, nil
		}
	}
	// 占一个探测名额；排队太久就放弃（显示未知，不影响页面）。
	wait := time.NewTimer(hc.queueFor)
	defer wait.Stop()
	select {
	case hc.sem <- struct{}{}:
		defer func() { <-hc.sem }()
	case <-wait.C:
		return &Health{ID: id, Status: HealthUnknown, Message: "探测排队超时（同时探测的实例太多），请稍后刷新", CheckedAt: s.now().Format("2006-01-02 15:04:05")}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var prevSample *qpsSample // 上一次的采样（不看缓存是否过期）
	if prev := hc.last(id); prev != nil {
		prevSample = prev.sample
	}
	h, sample := s.probeHealth(ctx, inst, prevSample)
	hc.put(id, &healthEntry{h: *h, at: s.now(), sample: sample})
	return h, nil
}

func (s *Service) probeHealth(ctx context.Context, inst *model.Instance, prev *qpsSample) (*Health, *qpsSample) {
	start := s.now()
	h := &Health{ID: inst.ID, CheckedAt: start.Format("2006-01-02 15:04:05")}
	pctx, cancel := context.WithTimeout(ctx, s.hc.probeFor)
	defer cancel()
	ci, err := s.connInfo(inst)
	if err != nil {
		h.Status, h.Message = HealthUnknown, "读取连接信息失败"
		return h, nil
	}
	if !mysqlLike[inst.DBType] {
		v, err := s.dialer.Probe(pctx, ci)
		if err != nil {
			h.Status = HealthUnknown
			h.Message = clip(strings.TrimPrefix(err.Error(), "暂不支持"), 160) // i18n-ignore 与拨号层返回的中文文案做匹配
			if strings.Contains(err.Error(), "暂不支持") {                     // i18n-ignore 与拨号层返回的中文文案做匹配
				h.Message = fmt.Sprintf("暂不支持探测 %s 实例的运行状态", dbTypeLabel(inst.DBType))
			}
			return h, nil
		}
		h.Status, h.Version = HealthOK, v
		h.LatencyMs = s.now().Sub(start).Milliseconds()
		return h, nil
	}
	t, err := s.dialer.Dial(pctx, ci)
	if err != nil {
		h.Status, h.Message = HealthUnknown, fmt.Sprintf("无法连接：%s", clip(err.Error(), 160))
		return h, nil
	}
	defer t.Close()
	st, err := t.Query(pctx, "SHOW GLOBAL STATUS WHERE Variable_name IN ('Threads_connected','Threads_running','Questions','Uptime')")
	if err != nil {
		// Doris / SelectDB 等不支持 SHOW GLOBAL STATUS：只要能连上并查到版本，就算正常，只是没有 QPS / 连接数。
		if r, verr := t.Query(pctx, "SELECT VERSION() AS v"); verr == nil {
			if recs := r.Records(); len(recs) > 0 && recs[0].Str("v") != "" {
				h.Version, h.Status, h.Message = recs[0].Str("v"), HealthOK, fmt.Sprintf("读不到运行状态（%s），只确认了连接和版本", clip(err.Error(), 80))
				h.LatencyMs = s.now().Sub(start).Milliseconds()
				return h, nil
			}
		}
		h.Status, h.Message = HealthUnknown, fmt.Sprintf("读取状态失败：%s", clip(err.Error(), 160))
		return h, nil
	}
	vals := map[string]int64{}
	for _, rec := range st.Records() {
		n, _ := strconv.ParseInt(strings.TrimSpace(rec.Str("value")), 10, 64)
		vals[strings.ToLower(rec.Str("variable_name"))] = n
	}
	h.Connections, h.ThreadsRunning = vals["threads_connected"], vals["threads_running"]
	if r, err := t.Query(pctx, "SELECT VERSION() AS v"); err == nil {
		if recs := r.Records(); len(recs) > 0 {
			h.Version = recs[0].Str("v")
		}
	}
	if r, err := t.Query(pctx, "SHOW GLOBAL VARIABLES LIKE 'max_connections'"); err == nil {
		if recs := r.Records(); len(recs) > 0 {
			h.MaxConnections, _ = strconv.ParseInt(strings.TrimSpace(recs[0].Str("value")), 10, 64)
		}
	}
	now := s.now()
	h.LatencyMs = now.Sub(start).Milliseconds()
	cur := &qpsSample{questions: vals["questions"], uptime: vals["uptime"], at: now}
	h.QPS, h.QPSKind = computeQPS(prev, cur)
	h.Status = HealthOK
	if h.MaxConnections > 0 && h.Connections*100/h.MaxConnections >= connWarnPct {
		h.Status = HealthWarn
		h.Message = fmt.Sprintf("连接使用率 %d%%（%d / %d）", h.Connections*100/h.MaxConnections, h.Connections, h.MaxConnections)
	}
	return h, cur
}

// computeQPS：有上一次采样且间隔合理、计数器没有回退（实例没重启）时按差值算；否则用启动以来的平均值。
func computeQPS(prev, cur *qpsSample) (float64, string) {
	if prev != nil && cur.at.Sub(prev.at) <= qpsMaxGap && cur.uptime > prev.uptime && cur.questions >= prev.questions {
		return round1(float64(cur.questions-prev.questions) / float64(cur.uptime-prev.uptime)), "delta"
	}
	if cur.uptime > 0 {
		return round1(float64(cur.questions) / float64(cur.uptime)), "avg"
	}
	return 0, ""
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
