// @author: kevinkelly
// @license: Apache Licence
// @file: runs.go
// @time: 2026/10/28

package archive

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/i18n"
)

// RunView 是运行记录的对外形态。
type RunView struct {
	ID            uint     `json:"id"`
	TaskID        uint     `json:"task_id"`
	TaskName      string   `json:"task_name"`
	InstanceID    uint     `json:"instance_id"`
	InstanceName  string   `json:"instance_name"`
	Table         string   `json:"table"`
	Trigger       string   `json:"trigger"`
	Result        string   `json:"result"`
	By            string   `json:"by"`
	StartedAt     string   `json:"started_at"`
	FinishedAt    string   `json:"finished_at"`
	DurationMs    int64    `json:"duration_ms"`
	Scanned       int64    `json:"scanned"`
	Backed        int64    `json:"backed"`
	Deleted       int64    `json:"deleted"`
	Batches       int      `json:"batches"`
	PeakThreads   int      `json:"peak_threads"`
	PeakLag       float64  `json:"peak_lag_seconds"`
	ThrottleCount int      `json:"throttle_count"`
	PKCols        []string `json:"pk_cols"`
	MinPK         []string `json:"min_pk"`
	MaxPK         []string `json:"max_pk"`
	DestTable     string   `json:"dest_table"`
	RestoreOf     uint     `json:"restore_of"`
	RestoredBy    []uint   `json:"restored_by"`
	Err           string   `json:"err"`
	Note          string   `json:"note"`
	AutoPause     bool     `json:"auto_pause"`
	CanRestore    bool     `json:"can_restore"`
}

func strSlice(s string) []string {
	v := unmarshalOr[[]string](s, nil)
	if v == nil {
		return []string{}
	}
	return v
}

func (s *Service) runView(c Caller, r *Run, _ *viewer) RunView {
	v := RunView{ID: r.ID, TaskID: r.TaskID, TaskName: r.TaskName, InstanceID: r.InstanceID, InstanceName: r.InstanceName,
		Table: r.DBName + "." + r.TblName, Trigger: r.Trigger, Result: r.Result, By: r.ByUser, StartedAt: fmtT(r.StartedAt), FinishedAt: fmtTP(r.FinishedAt),
		DurationMs: r.DurationMs, Scanned: r.Scanned, Backed: r.Backed, Deleted: r.Deleted, Batches: r.Batches, PeakThreads: r.PeakThreads,
		PeakLag: r.PeakLag, ThrottleCount: r.ThrottleCount, PKCols: strSlice(r.PKCols), MinPK: strSlice(r.MinPK), MaxPK: strSlice(r.MaxPK),
		DestTable: r.DestTable, RestoreOf: r.RestoreOf, RestoredBy: []uint{}, Err: r.Err, Note: r.Note, AutoPause: r.AutoPause}
	if r.Result == ResRunning || r.Result == ResPaused {
		v.DurationMs = s.now().Sub(r.StartedAt).Milliseconds()
		if l := s.liveOfRun(r.ID); l != nil {
			snap := l.sink.snapshot()
			if snap.HasProg {
				v.Scanned, v.Backed, v.Deleted, v.Batches = snap.Prog.Scanned, snap.Prog.Backed, snap.Prog.Deleted, snap.Prog.Batches
			}
			v.PeakThreads, v.PeakLag, v.ThrottleCount = snap.PeakThr, snap.PeakLag, snap.ThrCount
		}
	}
	v.CanRestore = r.DestTable != "" && r.Backed > 0 && r.Finished() && (r.Trigger == TrigSchedule || r.Trigger == TrigManual) && r.TaskID != 0 && c.has("archive.restore")
	return v
}

// RunDetail 是 GET /runs/:id 的结果。
type RunDetail struct {
	RunView
	Verify    []verifyRec      `json:"verify"`
	Backups   []backupRec      `json:"backups"`
	Throttles []throttleRec    `json:"throttles"`
	Series    []seriesPoint    `json:"series"`
	Log       []string         `json:"log"`
	Manifest  *engine.Manifest `json:"manifest"`
	Progress  *ProgressView    `json:"progress,omitempty"`
}

type seriesPoint struct {
	TMs        int64   `json:"t_ms"`
	RowsPerSec float64 `json:"rows_per_sec"`
	BatchSize  int     `json:"batch_size"`
	Throttled  bool    `json:"throttled"`
}

func seriesOf(pts []BatchStat) []seriesPoint {
	out := make([]seriesPoint, 0, len(pts))
	for _, p := range pts {
		out = append(out, seriesPoint{p.TMs, p.RowsPerSec, p.BatchSize, p.Throttled})
	}
	return out
}

func (s *Service) getRun(c Caller, id uint) (*Run, error) {
	r, err := s.repo.GetRun(id)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, notFound("运行记录不存在")
	}
	ok, err := s.canSeeGroup(c, r.ResourceGroupID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("运行记录不存在")
	}
	return r, nil
}

// RunQuery 是运行记录列表的筛选参数。
type RunQuery struct {
	TaskID  uint
	Trigger string
	Result  string
	Q       string
	Offset  int
	Limit   int
}

func (s *Service) ListRuns(c Caller, q RunQuery) ([]RunView, int64, error) {
	if err := c.need("archive.view", "查看运行记录"); err != nil {
		return nil, 0, err
	}
	if !s.repo.Ready() {
		return nil, 0, notReady()
	}
	all, ids, err := s.scope(c)
	if err != nil {
		return nil, 0, err
	}
	rows, total, err := s.repo.ListRuns(RunFilter{AllGroups: all, ResourceGroupIDs: ids, TaskID: q.TaskID, Trigger: q.Trigger, Result: q.Result, Q: strings.TrimSpace(q.Q),
		Offset: maxInt(q.Offset, 0), Limit: clampLimit(q.Limit, 50, 200)})
	if err != nil {
		return nil, 0, err
	}
	out := make([]RunView, 0, len(rows))
	for i := range rows {
		out = append(out, s.runView(c, &rows[i], nil))
	}
	return out, total, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Service) GetRun(c Caller, id uint) (*RunDetail, error) {
	if err := c.need("archive.view", "查看运行记录"); err != nil {
		return nil, err
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	r, err := s.getRun(c, id)
	if err != nil {
		return nil, err
	}
	d := &RunDetail{RunView: s.runView(c, r, nil), Verify: unmarshalOr(r.Verify, []verifyRec{}), Backups: unmarshalOr(r.Backups, []backupRec{}),
		Throttles: unmarshalOr(r.Throttles, []throttleRec{})}
	if d.RestoredBy, err = s.repo.RunsRestoredBy(r.ID); err != nil {
		return nil, err
	}
	if d.RestoredBy == nil {
		d.RestoredBy = []uint{}
	}
	if r.Manifest != "" {
		var m engine.Manifest
		if json.Unmarshal([]byte(r.Manifest), &m) == nil {
			d.Manifest = &m
		}
	}
	if l := s.liveOfRun(r.ID); l != nil && !l.ended {
		snap := l.sink.snapshot()
		d.Throttles, d.Verify = nonNil(snap.Thr), nonNilV(snap.Verify)
		d.Series = seriesOf(snap.Points)
		d.Log = l.sink.logLines(300, c.Lang)
		p := s.progressOf(l, r)
		d.Progress = &p
		return d, nil
	}
	pts, err := s.repo.ListBatchStats(r.ID)
	if err != nil {
		return nil, err
	}
	d.Series = seriesOf(pts)
	evs, err := s.repo.ListEvents(r.ID, 0, 0)
	if err != nil {
		return nil, err
	}
	if len(evs) > 300 {
		evs = evs[len(evs)-300:]
	}
	d.Log = []string{}
	for _, e := range evs {
		d.Log = append(d.Log, e.At.Format("15:04:05")+" "+i18n.TP(c.Lang, e.Msg))
	}
	if r.Note != "" {
		d.Log = append(d.Log, i18n.TP(c.Lang, r.Note))
	}
	return d, nil
}

func nonNil(x []throttleRec) []throttleRec {
	if x == nil {
		return []throttleRec{}
	}
	return x
}

func nonNilV(x []verifyRec) []verifyRec {
	if x == nil {
		return []verifyRec{}
	}
	return x
}

// EventView 是运行事件的对外形态。
type EventView struct {
	Seq    uint              `json:"seq"`
	At     string            `json:"at"`
	Kind   string            `json:"kind"`
	Msg    string            `json:"msg"`
	Fields map[string]string `json:"fields,omitempty"`
}

// RunEvents 返回 seq > after 的事件（日志面板增量拉取）。
func (s *Service) RunEvents(c Caller, id uint, after uint, limit int) ([]EventView, uint, error) {
	if err := c.need("archive.view", "查看运行记录"); err != nil {
		return nil, 0, err
	}
	if !s.repo.Ready() {
		return nil, 0, notReady()
	}
	if _, err := s.getRun(c, id); err != nil {
		return nil, 0, err
	}
	limit = clampLimit(limit, 200, 500)
	out := []EventView{}
	last := after
	if l := s.liveOfRun(id); l != nil {
		evs, seq := l.sink.eventsAfter(after, limit)
		for _, e := range evs {
			out = append(out, EventView{Seq: e.Seq, At: e.At.Format(timeLayout), Kind: e.Kind, Msg: e.Msg, Fields: e.Fields})
		}
		if len(out) > 0 {
			last = out[len(out)-1].Seq
		} else if seq > last {
			last = seq
		}
		return out, last, nil
	}
	rows, err := s.repo.ListEvents(id, after, limit)
	if err != nil {
		return nil, 0, err
	}
	for _, e := range rows {
		ev := EventView{Seq: e.Seq, At: e.At.Format(timeLayout), Kind: e.Kind, Msg: e.Msg}
		if e.Fields != "" {
			_ = json.Unmarshal([]byte(e.Fields), &ev.Fields)
		}
		out = append(out, ev)
		last = e.Seq
	}
	return out, last, nil
}

// RunManifest 返回运行的清单 JSON。
func (s *Service) RunManifest(c Caller, id uint) ([]byte, error) {
	if err := c.need("archive.view", "查看运行记录"); err != nil {
		return nil, err
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	r, err := s.getRun(c, id)
	if err != nil {
		return nil, err
	}
	if r.Manifest == "" {
		return nil, notFound("这次运行没有文件备份清单")
	}
	var buf json.RawMessage = []byte(r.Manifest)
	return json.MarshalIndent(buf, "", "  ")
}

// ---- 实时进度 ----

// ProgressView 是进行中运行的实时进度（GET /progress）。
type ProgressView struct {
	RunID          uint     `json:"run_id"`
	TaskID         uint     `json:"task_id"`
	Trigger        string   `json:"trigger"`
	StartedAt      string   `json:"started_at"`
	ElapsedMs      int64    `json:"elapsed_ms"`
	State          string   `json:"state"` // running | paused | waiting_load | finished
	Scanned        int64    `json:"scanned"`
	Backed         int64    `json:"backed"`
	Deleted        int64    `json:"deleted"`
	Batches        int      `json:"batches"`
	TotalEst       int64    `json:"total_est"`
	Percent        float64  `json:"percent"`
	RowsPerSec     float64  `json:"rows_per_sec"`
	EtaSeconds     int      `json:"eta_seconds"`
	BatchSize      int      `json:"batch_size"`
	SetBatchSize   int      `json:"set_batch_size"`
	SetSleepMs     int      `json:"set_sleep_ms"`
	Threads        int      `json:"threads"`
	MaxThreads     int      `json:"max_threads"`
	LagSeconds     int      `json:"lag_seconds"`
	MaxLagSeconds  int      `json:"max_lag_seconds"`
	Throttled      bool     `json:"throttled"`
	ThrottleReason string   `json:"throttle_reason"`
	ThrottleWaited int      `json:"throttle_waited_seconds"`
	MaxWaitSeconds int      `json:"max_wait_seconds"`
	ThrottleCount  int      `json:"throttle_count"`
	PeakThreads    int      `json:"peak_threads"`
	LastPK         []string `json:"last_pk"`
	UpdatedAt      string   `json:"updated_at"`
}

func (lr *liveRun) progressSnapshot(run *Run, snap sinkSnapshot) ProgressView {
	lr.mu.Lock()
	total, params := lr.totalEst, lr.params
	lr.mu.Unlock()
	p := snap.Prog
	pv := ProgressView{RunID: run.ID, TaskID: run.TaskID, Trigger: run.Trigger, StartedAt: fmtT(run.StartedAt), State: lr.state(),
		Scanned: p.Scanned, Backed: p.Backed, Deleted: p.Deleted, Batches: p.Batches, TotalEst: total, RowsPerSec: p.RowsPerSec, BatchSize: p.BatchSize,
		SetBatchSize: params.BatchSize, SetSleepMs: params.SleepMs, Threads: p.Threads, MaxThreads: params.MaxThreadsRunning, LagSeconds: p.LagSeconds,
		MaxLagSeconds: params.MaxLagSeconds, Throttled: p.Throttled, ThrottleReason: p.ThrottleReason, MaxWaitSeconds: params.MaxWaitSeconds,
		ThrottleCount: snap.ThrCount, PeakThreads: snap.PeakThr, LastPK: p.LastPK}
	if pv.LastPK == nil {
		pv.LastPK = []string{}
	}
	if snap.HasProg {
		pv.UpdatedAt = fmtT(snap.ProgAt)
		pv.ElapsedMs = snap.ProgAt.Sub(run.StartedAt).Milliseconds()
	}
	if n := len(snap.Thr); n > 0 && snap.Thr[n-1].End == "wait" {
		pv.ThrottleWaited = snap.Thr[n-1].Waited
	}
	done := p.Scanned
	if run.Trigger != TrigRestore && lr.trigger != TrigRestore {
		if p.Deleted > 0 || p.Backed == 0 {
			done = maxInt64(p.Deleted, p.Backed)
		} else {
			done = p.Backed
		}
	}
	if total > 0 {
		pv.Percent = float64(done) / float64(total) * 100
		if pv.Percent > 100 {
			pv.Percent = 100
		}
		if p.RowsPerSec > 0 && total > done {
			pv.EtaSeconds = int(float64(total-done) / p.RowsPerSec)
		}
	}
	return pv
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (s *Service) progressOf(lr *liveRun, run *Run) ProgressView {
	return lr.progressSnapshot(run, lr.sink.snapshot())
}

// Progress 返回调用者可见的进行中运行的实时进度。taskID 非 0 时只看该任务。
func (s *Service) Progress(c Caller, taskID uint) ([]ProgressView, error) {
	if err := c.need("archive.view", "查看归档进度"); err != nil {
		return nil, err
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	out := []ProgressView{}
	seen := map[uint]bool{}
	s.mu.Lock()
	var lives []*liveRun
	for _, l := range s.live {
		if !l.ended && (taskID == 0 || l.taskID == taskID) {
			lives = append(lives, l)
		}
	}
	s.mu.Unlock()
	for _, l := range lives {
		r, err := s.repo.GetRun(l.runID)
		if err != nil || r == nil {
			continue
		}
		if ok, _ := s.canSeeGroup(c, r.ResourceGroupID); !ok {
			continue
		}
		seen[l.runID] = true
		out = append(out, s.progressOf(l, r))
	}
	// 其他进程里的运行：取节流落库的最近一次进度
	running, err := s.repo.RunningRuns(0, s.now())
	if err != nil {
		return out, nil
	}
	for i := range running {
		r := &running[i]
		if seen[r.ID] || (taskID != 0 && r.TaskID != taskID) {
			continue
		}
		if ok, _ := s.canSeeGroup(c, r.ResourceGroupID); !ok {
			continue
		}
		var pv ProgressView
		if r.Progress != "" {
			_ = json.Unmarshal([]byte(r.Progress), &pv)
		}
		pv.RunID, pv.TaskID, pv.Trigger, pv.StartedAt = r.ID, r.TaskID, r.Trigger, fmtT(r.StartedAt)
		pv.ElapsedMs = s.now().Sub(r.StartedAt).Milliseconds()
		pv.State = "running"
		if r.Result == ResPaused {
			pv.State = "paused"
		}
		if pv.Scanned == 0 {
			pv.Scanned, pv.Backed, pv.Deleted, pv.Batches = r.Scanned, r.Backed, r.Deleted, r.Batches
		}
		if pv.LastPK == nil {
			pv.LastPK = []string{}
		}
		out = append(out, pv)
	}
	return out, nil
}

// ---- 运行控制 ----

// RunNowResult 是立即执行的结果。
type RunNowResult struct {
	RunID uint     `json:"run_id"`
	Task  TaskView `json:"task"`
}

// RunNow 立即执行一次（archive.run，高危）：仅已启用的任务，前置检查同调度；窗口外需 ignoreWindow。
func (s *Service) RunNow(c Caller, id uint, ignoreWindow, confirm bool) (*RunNowResult, error) {
	if err := c.need("archive.run", "立即执行归档"); err != nil {
		return nil, err
	}
	if !confirm {
		return nil, confirmNeeded("立即执行需要二次确认（confirm=true）")
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	t, err := s.getTask(c, id)
	if err != nil {
		return nil, err
	}
	run, err := s.startRun(t, startOpts{Trigger: TrigManual, By: c, IgnoreWindow: ignoreWindow})
	if err != nil {
		if se, ok := err.(*skipError); ok {
			s.auditAt(c, "立即执行", fmt.Sprintf("立即执行归档任务「%s」被拒绝：%s", t.Name, se.Reason)) // i18n-ignore：审计 / 通知正文，不翻译
			return nil, newErr(409, "skipped", "%s", se.Reason)
		}
		return nil, err
	}
	s.auditAt(c, "立即执行", fmt.Sprintf("立即执行归档任务「%s」（运行 #%d）%s", t.Name, run.ID, map[bool]string{true: "，已忽略执行窗口", false: ""}[ignoreWindow])) // i18n-ignore：审计 / 通知正文，不翻译
	s.notifyRun(t, run, evStart, "归档开始", "")                                                                                                // i18n-ignore：审计 / 通知正文，不翻译
	t, _ = s.repo.GetTask(id, false)
	return &RunNowResult{RunID: run.ID, Task: s.newViewer(c).view(t, false)}, nil
}

func (s *Service) controlRun(c Caller, id uint, code, what string, fn func(l *liveRun)) (*RunView, error) {
	if err := c.need(code, what); err != nil {
		return nil, err
	}
	r, err := s.getRun(c, id)
	if err != nil {
		return nil, err
	}
	l := s.liveOfRun(id)
	if l == nil || l.ended {
		return nil, conflict("该运行已结束，或不在本进程里运行（多实例部署时请到启动它的进程操作）")
	}
	fn(l)
	v := s.runView(c, r, nil)
	return &v, nil
}

// PauseRun 暂停运行：当前批做完后停，保留断点，可恢复。
func (s *Service) PauseRun(c Caller, id uint) (*RunView, error) {
	v, err := s.controlRun(c, id, "archive.pause", "暂停归档运行", func(l *liveRun) { l.ctl.Pause() })
	if err == nil {
		s.markRunState(id, ResPaused)
		v.Result = ResPaused
		s.auditAt(c, "暂停运行", fmt.Sprintf("暂停归档运行 #%d「%s」", id, v.TaskName)) // i18n-ignore：审计 / 通知正文，不翻译
	}
	return v, err
}

// ResumeRun 继续运行。
func (s *Service) ResumeRun(c Caller, id uint) (*RunView, error) {
	v, err := s.controlRun(c, id, "archive.pause", "继续归档运行", func(l *liveRun) { l.ctl.Resume() })
	if err == nil {
		s.markRunState(id, ResRunning)
		v.Result = ResRunning
		s.auditAt(c, "恢复运行", fmt.Sprintf("继续归档运行 #%d「%s」", id, v.TaskName)) // i18n-ignore：审计 / 通知正文，不翻译
	}
	return v, err
}

// StopRun 终止运行（archive.stop，高危）：当前批做完后结束，结果 stopped，保留断点。
func (s *Service) StopRun(c Caller, id uint, confirm bool) (*RunView, error) {
	if err := c.need("archive.stop", "终止归档运行"); err != nil {
		return nil, err
	}
	if !confirm {
		return nil, confirmNeeded("终止运行需要二次确认（confirm=true）")
	}
	v, err := s.controlRun(c, id, "archive.stop", "终止归档运行", func(l *liveRun) { l.ctl.Stop() })
	if err == nil {
		s.auditAt(c, "终止运行", fmt.Sprintf("终止归档运行 #%d「%s」", id, v.TaskName)) // i18n-ignore：审计 / 通知正文，不翻译
	}
	return v, err
}

func (s *Service) markRunState(id uint, result string) {
	if r, err := s.repo.GetRun(id); err == nil && r != nil && !r.Finished() {
		r.Result = result
		_ = s.repo.SaveRun(r)
	}
}

// RestoreResult 是恢复数据的结果。
type RestoreResult struct {
	RunID uint    `json:"run_id"`
	Run   RunView `json:"run"`
}

// Restore 恢复数据（archive.restore，高危）：按被恢复运行的主键范围把库表备份 INSERT IGNORE 回源表（只补缺、不覆盖）。
func (s *Service) Restore(c Caller, id uint, confirmName string) (*RestoreResult, error) {
	if err := c.need("archive.restore", "恢复归档数据"); err != nil {
		return nil, err
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	src, err := s.getRun(c, id)
	if err != nil {
		return nil, err
	}
	if src.Trigger != TrigSchedule && src.Trigger != TrigManual {
		return nil, conflict("只能恢复定时 / 手动运行（当前运行的触发方式是 %s）", src.Trigger)
	}
	if !src.Finished() {
		return nil, conflict("运行还没有结束，不能恢复")
	}
	if src.DestTable == "" || src.Backed == 0 {
		return nil, conflict("这次运行没有库表备份，无法按库表恢复（文件备份请手工导入）")
	}
	t, err := s.getTask(c, src.TaskID)
	if err != nil {
		return nil, conflict("任务已被删除，无法恢复")
	}
	if strings.TrimSpace(confirmName) != t.Name {
		return nil, confirmNeeded("请输入任务名「%s」确认恢复", t.Name)
	}
	run, err := s.startRun(t, startOpts{Trigger: TrigRestore, By: c, FromRun: src})
	if err != nil {
		if se, ok := err.(*skipError); ok {
			return nil, newErr(409, "running", "%s", se.Reason)
		}
		return nil, err
	}
	s.auditAt(c, "恢复数据", fmt.Sprintf("恢复归档数据：运行 #%d「%s」→ %s.%s（恢复运行 #%d）", src.ID, t.Name, t.DBName, t.TblName, run.ID)) // i18n-ignore：审计 / 通知正文，不翻译
	return &RestoreResult{RunID: run.ID, Run: s.runView(c, run, nil)}, nil
}

// ---- 恢复进程时的收尾 ----

// ExpireStale 把租约过期的 running / paused 运行标为 interrupted（进程崩溃 / 失联）。启动时与调度器每轮都会调用。
func (s *Service) ExpireStale() int {
	if !s.repo.Ready() {
		return 0
	}
	now := s.now()
	runs, err := s.repo.ExpireLeases(now, now)
	if err != nil {
		return 0
	}
	n := 0
	for i := range runs {
		if l := s.liveOfRun(runs[i].ID); l != nil && !l.ended {
			continue // 本进程里还活着，只是心跳慢了：不处理（ExpireLeases 已写库的话，下一次心跳 / 结束会覆盖）
		}
		n++
		s.noticeInterrupted(&runs[i])
	}
	return n
}

func (s *Service) noticeInterrupted(r *Run) {
	if r.TaskID == 0 {
		return
	}
	t, err := s.repo.GetTask(r.TaskID, false)
	if err != nil || t == nil {
		return
	}
	s.auditAt(systemCaller, "运行中断", fmt.Sprintf("运行 #%d「%s」的执行进程失联，已标记为中断", r.ID, r.TaskName)) // i18n-ignore：审计 / 通知正文，不翻译
	// 通知任务提交人（沿用任务所属资源组的渠道，@提交人）：进程被强杀 / 失联时没有人会收到“运行结束”通知
	hint := "执行进程失联（可能被强杀或所在机器宕机），运行已被标记为中断，没有写入最终结果。源表数据以已提交的批为准；" + // i18n-ignore：通知正文，语言属于接收方
		"文件备份的未上传部分可能留在暂存目录（" + spoolDir() + "），需要人工核对。任务仍按调度继续，下次从断点接着跑。" // i18n-ignore：通知正文，语言属于接收方
	s.notifyRun(t, r, evLost, "运行被中断（执行进程失联）", hint) // i18n-ignore：审计 / 通知正文，不翻译
}

var _ = time.Second
