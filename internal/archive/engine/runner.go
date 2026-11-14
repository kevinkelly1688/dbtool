// @author: kevinkelly
// @license: Apache Licence
// @file: runner.go
// @time: 2026/10/28

package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

type nopSink struct{}

func (nopSink) Event(Event)       {}
func (nopSink) Progress(Progress) {}

// runner 是一次运行（归档或恢复）的共享状态与辅助方法。Sink 回调都在不持锁的情况下调用。
type runner struct {
	spec Spec
	pol  Policy
	env  *Env
	ctl  *Control
	sink Sink
	p    Params // 合并默认值后的参数

	ctx  context.Context // 调用方的 ctx：数据库操作用
	wctx context.Context // ctx + Stop：休眠 / 等待用，Stop 不会打断正在执行的语句
	now  func() time.Time
	// sleepFn 睡眠 d，ctx 取消时提前返回错误。
	sleepFn func(ctx context.Context, d time.Duration) error

	start      time.Time
	prog       Progress
	lastEmit   time.Time
	emittedAny bool
	waitedTot  time.Duration // 负载等待累计（不计入吞吐的分母）
	undetected map[string]bool

	diskAt     time.Time
	diskReason string
	maxThreads int

	res RunResult
}

func newRunner(ctx context.Context, spec Spec, pol Policy, env *Env, ctl *Control, sink Sink) (*runner, context.CancelFunc) {
	if sink == nil {
		sink = nopSink{}
	}
	r := &runner{spec: spec, pol: pol, env: env, ctl: ctl, sink: sink, ctx: ctx, undetected: map[string]bool{}}
	r.p = EffectiveParams(spec.Params, pol.Defaults)
	r.now = env.Now
	if r.now == nil {
		r.now = time.Now
	}
	r.sleepFn = env.Sleep
	if r.sleepFn == nil {
		r.sleepFn = sleepCtx
	}
	wctx, cancel := ctl.stopCtx(ctx)
	r.wctx = wctx
	r.start = r.now()
	return r, cancel
}

func (r *runner) emit(kind, msg string, kv ...string) {
	ev := Event{At: r.now(), Kind: kind, Msg: msg}
	if len(kv) >= 2 {
		ev.Fields = map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			ev.Fields[kv[i]] = kv[i+1]
		}
	}
	r.sink.Event(ev)
}

// wsleep 休眠，可被 Stop / ctx 取消打断（返回错误）。
func (r *runner) wsleep(d time.Duration) error { return r.sleepFn(r.wctx, d) }

// stopped 判断运行应当终止：ctx 取消或 Stop。
func (r *runner) stopped() bool { return r.ctx.Err() != nil || r.ctl.Stopped() }

// waitWhilePaused 阻塞到不再暂停；返回 true 表示等待期间被终止。
func (r *runner) waitWhilePaused() bool {
	announced := false
	for {
		paused, stopped, ch := r.ctl.snapshot()
		if stopped || r.ctx.Err() != nil {
			return true
		}
		if !paused {
			if announced {
				r.emit("resume", "运行已恢复")
				r.emitProgress(true)
			}
			return false
		}
		if !announced {
			announced = true
			r.emit("log", "运行已暂停（保留断点，可恢复）")
		}
		select {
		case <-ch:
		case <-r.ctx.Done():
			return true
		}
	}
}

// retry 对死锁 / 锁等待超时做指数退避重试（最多 retryAttempts 次）。其他错误和上下文错误原样返回。
func (r *runner) retry(what string, op func() error) error {
	for attempt := 0; ; attempt++ {
		err := op()
		if err == nil {
			return nil
		}
		var re *RetryableError
		if !errors.As(err, &re) || attempt >= retryAttempts || r.ctx.Err() != nil {
			return err
		}
		d := retryBase << attempt
		r.emit("warn", fmt.Sprintf("%s遇到死锁 / 锁等待超时，%d ms 后重试（第 %d/%d 次）", what, d.Milliseconds(), attempt+1, retryAttempts),
			"op", what, "attempt", strconv.Itoa(attempt+1))
		if e := r.wsleep(d); e != nil {
			return err
		}
	}
}

// markUndetected 登记“未检测”的负载项：每个运行里每项只记一条事件，且不会被当作通过。
func (r *runner) markUndetected(item string, err error) {
	if r.undetected[item] {
		return
	}
	r.undetected[item] = true
	msg := fmt.Sprintf("未检测：%s", item)
	if err != nil {
		msg = fmt.Sprintf("未检测：%s（%s）", item, err.Error())
	}
	r.emit("warn", msg, "item", item, "status", "undetected")
}

// loadOver 做一次负载自检，返回超限原因（空表示都没超限）。取不到的项标“未检测”并跳过。
func (r *runner) loadOver() string {
	var reasons []string
	pr := r.env.Probe
	if pr == nil {
		r.markUndetected("load_probe", errors.New("没有负载探针"))
		return ""
	}
	if n, err := pr.ThreadsRunning(r.ctx); err != nil {
		r.markUndetected("threads_running", err)
	} else {
		r.prog.Threads = n
		if n > r.maxThreads {
			r.maxThreads = n
		}
		if n > r.p.MaxThreadsRunning {
			reasons = append(reasons, fmt.Sprintf("Threads_running=%d 超过阈值 %d", n, r.p.MaxThreadsRunning))
		}
	}
	if r.p.MaxLagSeconds > 0 {
		if sec, has, err := pr.ReplicaLag(r.ctx); err != nil {
			r.markUndetected("replica_lag", err)
		} else if has {
			r.prog.LagSeconds = sec
			if sec > r.p.MaxLagSeconds {
				reasons = append(reasons, fmt.Sprintf("复制延迟 %d 秒超过阈值 %d 秒", sec, r.p.MaxLagSeconds))
			}
		}
	}
	if d := r.diskCheck(); d != "" {
		reasons = append(reasons, d)
	}
	if len(reasons) == 0 {
		return ""
	}
	return joinReasons(reasons)
}

func joinReasons(rs []string) string {
	out := rs[0]
	for _, x := range rs[1:] {
		out += "；" + x
	}
	return out
}

// diskCheck 检查源库 / 目标库磁盘剩余和文件存储可用空间。磁盘变化慢，结果缓存 diskCacheTTL。
func (r *runner) diskCheck() string {
	now := r.now()
	if !r.diskAt.IsZero() && now.Sub(r.diskAt) < diskCacheTTL {
		return r.diskReason
	}
	r.diskAt = now
	var reasons []string
	chk := func(item, label string, f func() (float64, bool, error)) {
		pct, ok, err := f()
		switch {
		case err != nil:
			r.markUndetected(item, err)
		case !ok:
			r.markUndetected(item, nil)
		case pct < float64(r.p.MinDiskFreePct):
			reasons = append(reasons, fmt.Sprintf("%s磁盘剩余 %.1f%% 低于阈值 %d%%", label, pct, r.p.MinDiskFreePct))
		}
	}
	if r.env.Probe != nil {
		chk("src_disk", "源库", func() (float64, bool, error) { return r.env.Probe.DiskFreePct(r.ctx) })
	}
	if r.env.Dst != nil && r.spec.Dest != nil {
		chk("dest_disk", "目标库", func() (float64, bool, error) { return r.env.Dst.DiskFreePct(r.ctx) })
	}
	if r.env.Files != nil && r.spec.File != nil {
		free, ok, err := r.env.Files.Free(r.ctx)
		need := int64(effPartMB(r.spec.File)) << 21 // 两个文件的空间
		switch {
		case err != nil:
			r.markUndetected("file_free", err)
		case !ok:
			r.markUndetected("file_free", nil)
		case free < need:
			reasons = append(reasons, fmt.Sprintf("文件存储可用空间 %d MB 不足（至少需要 %d MB）", free>>20, need>>20))
		}
	}
	r.diskReason = ""
	if len(reasons) > 0 {
		r.diskReason = joinReasons(reasons)
	}
	return r.diskReason
}

type loadOutcome int

const (
	loadOK loadOutcome = iota
	loadAbort
	loadStopped
)

// waitLoad 在负载超限时暂停，每 30 秒复查；超过 MaxWaitSeconds 返回 loadAbort。
func (r *runner) waitLoad() loadOutcome {
	reason := r.loadOver()
	if reason == "" {
		if r.prog.Throttled {
			r.prog.Throttled, r.prog.ThrottleReason = false, ""
		}
		return loadOK
	}
	r.res.ThrottleCount++
	r.prog.Throttled, r.prog.ThrottleReason = true, reason
	r.emit("throttle", fmt.Sprintf("负载超限，暂停等待：%s", reason), "reason", reason, "kind", "load")
	r.emitProgress(true)
	maxWait := time.Duration(r.p.MaxWaitSeconds) * time.Second
	var waited time.Duration
	for {
		if waited >= maxWait {
			r.waitedTot += waited
			r.prog.Throttled = true
			return loadAbort
		}
		d := loadRecheck
		if rem := maxWait - waited; rem < d {
			d = rem
		}
		if err := r.wsleep(d); err != nil {
			r.waitedTot += waited
			return loadStopped
		}
		waited += d
		if reason = r.loadOver(); reason == "" {
			r.waitedTot += waited
			r.prog.Throttled, r.prog.ThrottleReason = false, ""
			r.emit("resume", fmt.Sprintf("负载恢复正常，已等待 %d 秒，继续运行", int(waited.Seconds())), "waited_sec", strconv.Itoa(int(waited.Seconds())))
			r.emitProgress(true)
			return loadOK
		}
		r.prog.ThrottleReason = reason
	}
}

// emitProgress 把进度交给 Sink；除非 force，最多每 progressEvery 一次。
func (r *runner) emitProgress(force bool) {
	now := r.now()
	if !force && r.emittedAny && now.Sub(r.lastEmit) < progressEvery {
		return
	}
	r.emittedAny = true
	r.lastEmit = now
	p := r.prog
	p.Scanned, p.Backed, p.Deleted, p.Batches = r.res.Scanned, r.res.Backed, r.res.Deleted, r.res.Batches
	p.LastPK = append([]string(nil), r.res.Checkpoint...)
	active := now.Sub(r.start) - r.waitedTot
	if active > 0 {
		p.RowsPerSec = float64(r.res.Scanned) / active.Seconds()
	}
	r.sink.Progress(p)
}

func copyStrings(s []string) []string { return append([]string(nil), s...) }

func (r *runner) fail(result, msg string, autoPause bool) {
	r.res.Result = result
	r.res.Err = msg
	r.res.AutoPause = autoPause
}

// batchFailed 登记一次批失败；达到连续失败上限返回 true（并设置了 failed + AutoPause）。否则退避后返回 false。
func (r *runner) batchFailed(fails *int, err error) bool {
	*fails++
	r.emit("warn", fmt.Sprintf("批失败（连续第 %d/%d 次）：%s", *fails, r.p.MaxConsecutiveFailures, err.Error()),
		"fails", strconv.Itoa(*fails), "err", err.Error())
	if *fails >= r.p.MaxConsecutiveFailures {
		r.fail(ResultFailed, fmt.Sprintf("连续失败 %d 批，最后一次：%s", *fails, err.Error()), true)
		return true
	}
	back := failBackoff << min(*fails-1, 4)
	_ = r.wsleep(back)
	if r.stopped() {
		r.fail(ResultStopped, "", false)
		return true
	}
	return false
}
