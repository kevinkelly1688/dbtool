// @author: kevinkelly
// @license: Apache Licence
// @file: sink.go
// @time: 2026/10/28

package archive

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/i18n"
)

// runSink 实现 engine.Sink：把事件 / 进度收进内存（环形缓冲、吞吐采样、限流记录），由 runner 的后台 goroutine 节流落库。
// 引擎调用 Event / Progress 时只做内存操作，不会被数据库拖慢。
type runSink struct {
	mu    sync.Mutex
	now   func() time.Time
	start time.Time

	seq     uint
	ring    []sinkEvent
	pending []sinkEvent // 尚未落库的事件

	prog      engine.Progress
	progAt    time.Time
	hasProg   bool
	peakThr   int
	peakLag   float64
	throttled bool
	thrCount  int
	thrs      []throttleRec
	openThr   int // thrs 中未结束记录的下标，-1 无

	verify []verifyRec

	// 吞吐曲线采样
	points    []BatchStat
	flushedAt int // points 中已落库的个数
	interval  time.Duration
	lastPoint time.Time
	lastCount int64 // 上次采样时的 Scanned，用来算速率
	lastTime  time.Time
}

type sinkEvent struct {
	Seq uint
	engine.Event
}

type throttleRec struct {
	At        string `json:"at"`
	Kind      string `json:"kind"` // threads | lag | disk
	Value     string `json:"value"`
	Threshold string `json:"threshold"`
	Waited    int    `json:"waited_seconds"`
	End       string `json:"end"` // wait | resume | abort
	BatchTo   int    `json:"batch_to"`
	startedAt time.Time
}

type verifyRec struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func newRunSink(now func() time.Time) *runSink {
	return &runSink{now: now, start: now(), openThr: -1, interval: time.Second}
}

var _ engine.Sink = (*runSink)(nil)

func (k *runSink) Event(ev engine.Event) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if ev.At.IsZero() {
		ev.At = k.now()
	}
	k.seq++
	se := sinkEvent{Seq: k.seq, Event: ev}
	k.ring = append(k.ring, se)
	if len(k.ring) > ringCap {
		k.ring = append(k.ring[:0], k.ring[len(k.ring)-ringCap:]...)
	}
	k.pending = append(k.pending, se)
	if len(k.pending) > 4000 { // 数据库长时间写不进去时不无限增长
		k.pending = append(k.pending[:0], k.pending[len(k.pending)-2000:]...)
	}
	switch ev.Kind {
	case "verify":
		k.verify = append(k.verify, verifyRec{Key: field(ev, "key", "verify"), Title: ev.Msg, OK: field(ev, "ok", "true") != "false", Detail: field(ev, "detail", "")})
	case "throttle":
		if k.openThr >= 0 {
			r := &k.thrs[k.openThr]
			if v := field(ev, "value", ""); v != "" {
				r.Value = v
			}
			if v := field(ev, "threshold", ""); v != "" {
				r.Threshold = v
			}
			if v := field(ev, "reason", ""); v != "" {
				r.Kind = throttleKind(v)
				// 引擎把读数写在原因文字里（如 “Threads_running=58 > 32”）：取前两个数字当读数 / 阈值
				if nums := numRe.FindAllString(v, -1); len(nums) >= 2 {
					if r.Value == "" {
						r.Value = nums[len(nums)-2]
					}
					if r.Threshold == "" {
						r.Threshold = nums[len(nums)-1]
					}
				}
			}
		}
	}
}

var numRe = regexp.MustCompile(`\d+(?:\.\d+)?`)

func field(ev engine.Event, key, def string) string {
	if ev.Fields != nil {
		if v, ok := ev.Fields[key]; ok && v != "" {
			return v
		}
	}
	return def
}

func throttleKind(reason string) string {
	r := strings.ToLower(reason)
	switch {
	case strings.Contains(r, "thread"):
		return "threads"
	case strings.Contains(r, "lag") || strings.Contains(r, "延迟") || strings.Contains(r, "replica"): // i18n-ignore：与引擎输出的原因文字做匹配
		return "lag"
	case strings.Contains(r, "disk") || strings.Contains(r, "磁盘"): // i18n-ignore：与引擎输出的原因文字做匹配
		return "disk"
	}
	return "threads"
}

func (k *runSink) Progress(p engine.Progress) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	k.prog, k.progAt, k.hasProg = p, now, true
	if p.Threads > k.peakThr {
		k.peakThr = p.Threads
	}
	if l := float64(p.LagSeconds); l > k.peakLag {
		k.peakLag = l
	}
	switch {
	case p.Throttled && !k.throttled:
		k.thrCount++
		k.thrs = append(k.thrs, throttleRec{At: now.Format(timeLayout), Kind: throttleKind(p.ThrottleReason), Value: valueOf(p), Waited: 0, End: "wait", startedAt: now})
		k.openThr = len(k.thrs) - 1
	case !p.Throttled && k.throttled && k.openThr >= 0:
		r := &k.thrs[k.openThr]
		r.End, r.Waited, r.BatchTo = "resume", int(now.Sub(r.startedAt).Seconds()), p.BatchSize
		k.openThr = -1
	case p.Throttled && k.openThr >= 0:
		k.thrs[k.openThr].Waited = int(now.Sub(k.thrs[k.openThr].startedAt).Seconds())
	}
	k.throttled = p.Throttled
	// 吞吐采样：每 interval 一个点；点数超过 200 时间隔翻倍并丢掉一半旧点（曲线始终覆盖整个运行）
	if k.lastPoint.IsZero() || now.Sub(k.lastPoint) >= k.interval {
		rate := p.RowsPerSec
		if rate == 0 && !k.lastTime.IsZero() && now.After(k.lastTime) && p.Scanned >= k.lastCount {
			rate = float64(p.Scanned-k.lastCount) / now.Sub(k.lastTime).Seconds()
		}
		k.points = append(k.points, BatchStat{TMs: now.Sub(k.start).Milliseconds(), RowsPerSec: rate, BatchSize: p.BatchSize, Throttled: p.Throttled})
		k.lastPoint, k.lastCount, k.lastTime = now, p.Scanned, now
		if len(k.points) > 200 {
			kept := k.points[:0]
			for i, pt := range k.points {
				if i%2 == 0 {
					kept = append(kept, pt)
				}
			}
			k.points = kept
			k.interval *= 2
			if k.flushedAt > len(k.points) {
				k.flushedAt = len(k.points)
			}
		}
	}
}

func valueOf(p engine.Progress) string {
	switch throttleKind(p.ThrottleReason) {
	case "lag":
		return fmt.Sprint(p.LagSeconds)
	case "threads":
		return fmt.Sprint(p.Threads)
	}
	return ""
}

// closeThrottle 运行结束时收尾：还在等待负载的记录，按结果标成 abort（load_abort）或 resume。
func (k *runSink) closeThrottle(result string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if result == engine.ResultLoadAbort && len(k.thrs) > 0 { // 引擎结束前的最后一次进度可能已把它标成 resume：以运行结果为准
		k.thrs[len(k.thrs)-1].End = "abort"
		k.openThr = -1
		return
	}
	if k.openThr >= 0 {
		r := &k.thrs[k.openThr]
		r.Waited = int(k.now().Sub(r.startedAt).Seconds())
		if result == engine.ResultLoadAbort {
			r.End = "abort"
		} else {
			r.End = "resume"
		}
		k.openThr = -1
	}
}

// takePending 取走未落库的事件。
func (k *runSink) takePending() []sinkEvent {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := k.pending
	k.pending = nil
	return out
}

// requeue 落库失败时把事件放回去（放在队首，保持顺序）。
func (k *runSink) requeue(evs []sinkEvent) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pending = append(append([]sinkEvent(nil), evs...), k.pending...)
}

// takePoints 取走尚未落库的采样点。
func (k *runSink) takePoints() []BatchStat {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.flushedAt >= len(k.points) {
		return nil
	}
	out := append([]BatchStat(nil), k.points[k.flushedAt:]...)
	k.flushedAt = len(k.points)
	return out
}

// snapshot 是进度的只读快照。
type sinkSnapshot struct {
	Prog     engine.Progress
	ProgAt   time.Time
	HasProg  bool
	PeakThr  int
	PeakLag  float64
	ThrCount int
	Thr      []throttleRec
	Verify   []verifyRec
	Points   []BatchStat
}

func (k *runSink) snapshot() sinkSnapshot {
	k.mu.Lock()
	defer k.mu.Unlock()
	return sinkSnapshot{Prog: k.prog, ProgAt: k.progAt, HasProg: k.hasProg, PeakThr: k.peakThr, PeakLag: k.peakLag, ThrCount: k.thrCount,
		Thr: append([]throttleRec(nil), k.thrs...), Verify: append([]verifyRec(nil), k.verify...), Points: append([]BatchStat(nil), k.points...)}
}

// eventsAfter 返回环形缓冲里 seq > after 的事件（最多 limit 条）。
func (k *runSink) eventsAfter(after uint, limit int) ([]sinkEvent, uint) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []sinkEvent
	for _, e := range k.ring {
		if e.Seq > after {
			out = append(out, e)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, k.seq
}

// logLines 返回最近 n 条事件的“时间 消息”文本；消息按 lang 翻译（引擎产出的中文，目录里有译文才换）。
func (k *runSink) logLines(n int, lang string) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	from := 0
	if len(k.ring) > n {
		from = len(k.ring) - n
	}
	out := make([]string, 0, len(k.ring)-from)
	for _, e := range k.ring[from:] {
		out = append(out, e.At.Format("15:04:05")+" "+i18n.TP(lang, e.Msg))
	}
	return out
}

func eventRow(runID uint, e sinkEvent) Event {
	fields := ""
	if len(e.Fields) > 0 {
		if b, err := json.Marshal(e.Fields); err == nil {
			fields = string(b)
		}
	}
	return Event{RunID: runID, Seq: e.Seq, At: e.At, Kind: e.Kind, Msg: truncRunes(e.Msg, 900), Fields: fields}
}
