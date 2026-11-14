// @author: kevinkelly
// @license: Apache Licence
// @file: runner.go
// @time: 2026/10/28

package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/notify"
)

const (
	leaseTTL        = 90 * time.Second
	flushEvery      = 2 * time.Second
	heartbeatEvery  = 10 * time.Second
	preCountTimeout = 30 * time.Second
)

func lockName(taskID uint) string { return fmt.Sprintf("dbt_archive_task_%d", taskID) }

// liveRun 是本进程里正在执行的运行。
type liveRun struct {
	runID    uint
	taskID   uint
	instID   uint
	trigger  string
	started  time.Time
	ctl      *engine.Control
	cancel   context.CancelFunc
	sink     *runSink
	mu       sync.Mutex
	totalEst int64
	params   engine.Params
	pkCols   []string
	maxRows  int64
	ended    bool
}

// state 是运行的当前状态：running | paused | waiting_load。
func (l *liveRun) state() string {
	if l.ctl != nil && l.ctl.Paused() {
		return "paused"
	}
	snap := l.sink.snapshot()
	if snap.HasProg && snap.Prog.Throttled {
		return "waiting_load"
	}
	return "running"
}

func (s *Service) liveOfTask(taskID uint) *liveRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.live {
		if l.taskID == taskID && !l.ended {
			return l
		}
	}
	return nil
}

// taskRunning 判断任务当前是否有运行在进行：本进程内存里有，或库里有该任务 running / paused 且租约未过期的运行记录
// （别的进程在跑；多实例部署时编辑 / 暂停 / 停用 / 删除不能只看本进程）。租约已过期的记录视为进程失联，不拦（等回收）。
// 读库失败时按“没有”处理（后续写库的操作自己也会失败），不因为库抖动把任务锁死。
func (s *Service) taskRunning(taskID uint) bool {
	if s.liveOfTask(taskID) != nil {
		return true
	}
	rs, err := s.repo.RunningRuns(0, s.now())
	if err != nil {
		slog.Warn("检查任务是否有运行中的记录失败", "task", taskID, "err", err)
		return false
	}
	for i := range rs {
		if rs[i].TaskID == taskID {
			return true
		}
	}
	return false
}

func (s *Service) liveOfRun(runID uint) *liveRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live[runID]
}

// skipError 表示前置检查没有通过，本次不启动（调度时记一条“跳过”运行）。
type skipError struct{ Reason string }

func (e *skipError) Error() string { return e.Reason }

type startOpts struct {
	Trigger      string
	By           Caller
	IgnoreWindow bool
	FromRun      *Run // 恢复：被恢复的运行
	Confirm      bool
}

func (s *Service) baseCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.base == nil {
		return context.Background()
	}
	return s.base
}

// precheck 是调度 / 立即执行共用的前置检查（设计 §3.5）。返回的 skipError 表示跳过。
func (s *Service) precheck(t *Task, pol Policy, o startOpts) error {
	if t.Status != StEnabled {
		return &skipError{fmt.Sprintf("任务当前状态为「%s」，只有已启用的任务可以运行", statusLabel(t.Status))}
	}
	c := t.cfg()
	if t.RetentionDays < pol.MinRetentionDays {
		return &skipError{fmt.Sprintf("保留天数 %d 天低于安全策略的最小保留天数 %d 天，不能运行；请编辑任务后重新审批", t.RetentionDays, pol.MinRetentionDays)}
	}
	if t.Mode == string(engine.ModePurge) && !pol.AllowPurgeNoBackup {
		return &skipError{"安全策略当前不允许“不备份清理”，不能运行；请联系管理员"}
	}
	if engine.IsProtected(pol.ProtectedPatterns, t.DBName, t.TblName) {
		return &skipError{fmt.Sprintf("%s.%s 现在属于受保护库表（安全策略），不能运行", t.DBName, t.TblName)}
	}
	if c.DestFile != nil {
		st, err := s.repo.GetStore(c.DestFile.StoreID)
		switch {
		case err != nil:
			return err
		case st == nil:
			return &skipError{"备份存储已被删除，先备份后删除的前提不成立，不能运行"}
		case !st.Enabled:
			return &skipError{fmt.Sprintf("备份存储「%s」已停用，先备份后删除的前提不成立，不能运行", st.Name)}
		case st.Status == "err":
			return &skipError{fmt.Sprintf("备份存储「%s」当前不可用（最近一次测试失败：%s），不能运行", st.Name, st.LastTestMsg)}
		}
	}
	if !o.IgnoreWindow && !inWindow(s.now(), c.Params.Window) {
		return &skipError{fmt.Sprintf("当前时间 %s 不在执行窗口 %s–%s 内；需要立即运行请勾选“忽略执行窗口”", s.now().Format("15:04"), c.Params.Window.Start, c.Params.Window.End)}
	}
	inst, err := s.instanceByID(t.InstanceID)
	if err != nil {
		return &skipError{"来源实例不存在或已停用"}
	}
	if err := eligibleInstance(inst); err != nil {
		return &skipError{err.Error()}
	}
	if wfs, err := s.repo.OpenWorkflows(t.InstanceID, t.DBName, t.TblName); err != nil {
		slog.Warn("检查未结束的 SQL 上线工单失败，按保守策略跳过", "task", t.ID, "err", err)
		return &skipError{"无法确认源表是否有未结束的 SQL 上线工单（读取工单失败），本次跳过"}
	} else if len(wfs) > 0 {
		return &skipError{fmt.Sprintf("源表有未结束的 SQL 上线工单 #%d「%s」（%s），本次跳过；工单结束后再试", wfs[0].ID, wfs[0].Name, wfStatusLabel(wfs[0].Status))}
	}
	return nil
}

func wfStatusLabel(st string) string {
	switch st {
	case "workflow_review_pass":
		return "审核通过"
	case "workflow_timingtask":
		return "定时执行"
	case "workflow_queuing":
		return "排队中"
	case "workflow_executing":
		return "执行中"
	}
	return st
}

// startRun 启动一次运行（调度 / 立即执行 / 恢复）。成功返回已插入的运行记录，运行在后台继续。
func (s *Service) startRun(t *Task, o startOpts) (*Run, error) {
	pol := s.loadPolicy()
	if o.Trigger != TrigRestore {
		if err := s.precheck(t, pol, o); err != nil {
			return nil, err
		}
	}
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.liveOfTask(t.ID) != nil {
		return nil, &skipError{"该任务已有一次运行在进行中（上次运行还没有结束）"}
	}
	now := s.now()
	running, err := s.repo.RunningRuns(0, now)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, r := range running {
		if r.TaskID == t.ID {
			return nil, &skipError{"该任务已有一次运行在进行中（上次运行还没有结束）"}
		}
		if r.InstanceID == t.InstanceID {
			n++
		}
	}
	if n >= pol.MaxConcurrentPerInstance {
		return nil, &skipError{fmt.Sprintf("同一实例同时最多 %d 个归档运行（安全策略），当前已有 %d 个在跑，请稍后再试", pol.MaxConcurrentPerInstance, n)}
	}
	release, ok, err := s.repo.TryLock(context.Background(), lockName(t.ID))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &skipError{"该任务正在其他进程里运行"}
	}
	instName := ""
	if in, err := s.instanceByID(t.InstanceID); err == nil {
		instName = in.InstanceName
	}
	lease := now.Add(leaseTTL)
	run := &Run{TaskID: t.ID, TaskName: t.Name, InstanceID: t.InstanceID, InstanceName: instName, DBName: t.DBName, TblName: t.TblName,
		ResourceGroupID: t.ResourceGroupID, Trigger: o.Trigger, Result: ResRunning, ByUser: o.By.Username, StartedAt: now,
		LeaseOwner: s.owner, LeaseUntil: &lease, HeartbeatAt: &now, CreatedAt: now}
	if o.FromRun != nil {
		run.RestoreOf = o.FromRun.ID
		run.PKCols, run.MinPK, run.MaxPK, run.DestTable, run.DestInstanceID = o.FromRun.PKCols, o.FromRun.MinPK, o.FromRun.MaxPK, o.FromRun.DestTable, o.FromRun.DestInstanceID
	}
	if err := s.repo.InsertRun(run); err != nil {
		release()
		return nil, err
	}
	lr := &liveRun{runID: run.ID, taskID: t.ID, instID: t.InstanceID, trigger: o.Trigger, started: now, ctl: engine.NewControl(), sink: newRunSink(s.now), params: t.cfg().Params}
	s.mu.Lock()
	s.live[run.ID] = lr
	s.mu.Unlock()
	spec := specOf(t, run.ID)
	task := *t
	s.spawn(func() { s.runBody(lr, run, &task, spec, pol, release, o) })
	return run, nil
}

// skipRun 为调度到点但被拦下的情况记一条“跳过”运行。
func (s *Service) skipRun(t *Task, reason string) {
	now := s.now()
	instName := ""
	if in, err := s.instanceByID(t.InstanceID); err == nil {
		instName = in.InstanceName
	}
	r := &Run{TaskID: t.ID, TaskName: t.Name, InstanceID: t.InstanceID, InstanceName: instName, DBName: t.DBName, TblName: t.TblName, ResourceGroupID: t.ResourceGroupID,
		Trigger: TrigSchedule, Result: ResSkipped, ByUser: "system", StartedAt: now, FinishedAt: &now, Note: truncRunes(reason, 480), CreatedAt: now}
	if err := s.repo.InsertRun(r); err != nil {
		slog.Error("写入跳过运行记录失败", "task", t.ID, "err", err)
	}
}

func (s *Service) unregister(runID uint) {
	s.mu.Lock()
	if l := s.live[runID]; l != nil {
		l.ended = true
	}
	s.mu.Unlock()
	// 留在 map 里一小会儿让页面最后一次轮询读到终态进度，再清掉
	time.AfterFunc(30*time.Second, func() {
		s.mu.Lock()
		delete(s.live, runID)
		s.mu.Unlock()
	})
}

// runBody 在后台 goroutine 里执行一次运行直到结束，并负责落库 / 通知。
func (s *Service) runBody(lr *liveRun, run *Run, t *Task, spec engine.Spec, pol Policy, release func(), o startOpts) {
	defer release()
	defer s.unregister(run.ID)
	ctx, cancel := context.WithCancel(s.baseCtx())
	lr.cancel = cancel
	defer cancel()
	stopFlush := s.startFlusher(ctx, lr, run, t)
	var res *engine.RunResult
	var runErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				runErr = fmt.Errorf("运行内部错误：%v", r)
				slog.Error("数据归档运行 panic", "run", run.ID, "panic", r)
			}
		}()
		res, runErr = s.execute(ctx, lr, run, t, spec, pol, o)
	}()
	stopFlush()
	s.finishRun(lr, run, t, spec, res, runErr)
}

// execute 打开连接并调用引擎。
func (s *Service) execute(ctx context.Context, lr *liveRun, run *Run, t *Task, spec engine.Spec, pol Policy, o startOpts) (*engine.RunResult, error) {
	if o.Trigger == TrigRestore {
		return s.executeRestore(ctx, lr, run, t, spec, o)
	}
	env, err := s.open(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("连接源 / 目标失败：%s", sanitizeErr(err))
	}
	defer env.Close()
	if env.Src != nil {
		dctx, dcancel := context.WithTimeout(ctx, preCountTimeout)
		if info, derr := env.Src.Describe(dctx, spec.DB, spec.Table); derr == nil {
			lr.mu.Lock()
			lr.pkCols = info.PrimaryKey
			lr.mu.Unlock()
		}
		dcancel()
	}
	// 预估总量（供进度条 / 预计剩余）：命中行数与有效上限取小；取不到就不显示百分比
	if where, werr := engine.BuildWhere(spec); werr == nil && env.Src != nil {
		eff := engine.EffectiveMaxRows(spec.Params, pol.engine())
		cctx, cancel := context.WithTimeout(ctx, preCountTimeout)
		n, capped, cerr := env.Src.CountWhere(cctx, spec.DB, spec.Table, where, eff)
		cancel()
		if cerr == nil && (spec.Mode != engine.ModeCopy || len(spec.Checkpoint) == 0) {
			if capped || n > eff {
				n = eff
			}
			lr.mu.Lock()
			lr.totalEst, lr.maxRows = n, eff
			lr.mu.Unlock()
		}
	}
	return s.eng.Run(ctx, spec, pol.engine(), env, lr.ctl, lr.sink)
}

func (s *Service) executeRestore(ctx context.Context, lr *liveRun, run *Run, t *Task, spec engine.Spec, o startOpts) (*engine.RunResult, error) {
	src := o.FromRun
	var minPK, maxPK []string
	_ = json.Unmarshal([]byte(src.MinPK), &minPK)
	_ = json.Unmarshal([]byte(src.MaxPK), &maxPK)
	db, tbl, ok := strings.Cut(src.DestTable, ".")
	if !ok {
		return nil, errors.New("被恢复的运行没有库表备份位置")
	}
	spec.Mode, spec.File = engine.ModeCopy, nil
	spec.Dest = &engine.TableDest{InstanceID: src.DestInstanceID, DB: db, Table: tbl, Template: tbl}
	lr.mu.Lock()
	lr.pkCols = unmarshalOr[[]string](src.PKCols, nil)
	lr.mu.Unlock()
	spec.Checkpoint = nil
	env, err := s.open(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("连接源 / 备份库失败：%s", sanitizeErr(err))
	}
	defer env.Close()
	rng := engine.RestoreRange{DestInstanceID: src.DestInstanceID, DestDB: db, DestTable: tbl, MinPK: minPK, MaxPK: maxPK}
	return s.eng.Restore(ctx, spec, rng, env, lr.ctl, lr.sink)
}

// startFlusher 启动后台落库：事件、吞吐采样、进度 / 租约心跳、断点。返回的函数停止它并做最后一次落库。
func (s *Service) startFlusher(ctx context.Context, lr *liveRun, run *Run, t *Task) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(flushEvery)
		defer tick.Stop()
		var lastBeat time.Time
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				s.flushEvents(lr, run.ID)
				if s.now().Sub(lastBeat) >= heartbeatEvery {
					lastBeat = s.now()
					s.heartbeat(lr, run, t)
				}
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
		s.flushEvents(lr, run.ID)
	}
}

func (s *Service) flushEvents(lr *liveRun, runID uint) {
	if evs := lr.sink.takePending(); len(evs) > 0 {
		rows := make([]Event, len(evs))
		for i, e := range evs {
			rows[i] = eventRow(runID, e)
		}
		if err := s.repo.InsertEvents(rows); err != nil {
			slog.Warn("写入归档运行事件失败", "run", runID, "err", err)
			lr.sink.requeue(evs)
		}
	}
	if pts := lr.sink.takePoints(); len(pts) > 0 {
		if err := s.repo.InsertBatchStats(runID, pts); err != nil {
			slog.Warn("写入归档吞吐采样失败", "run", runID, "err", err)
		}
	}
}

func (s *Service) heartbeat(lr *liveRun, run *Run, t *Task) {
	snap := lr.sink.snapshot()
	now := s.now()
	lease := now.Add(leaseTTL)
	cur, err := s.repo.GetRun(run.ID)
	if err != nil || cur == nil || cur.Finished() {
		return
	}
	cur.LeaseUntil, cur.HeartbeatAt = &lease, &now
	if snap.HasProg {
		cur.Scanned, cur.Backed, cur.Deleted, cur.Batches = snap.Prog.Scanned, snap.Prog.Backed, snap.Prog.Deleted, snap.Prog.Batches
		cur.PeakThreads, cur.PeakLag, cur.ThrottleCount = snap.PeakThr, snap.PeakLag, snap.ThrCount
		cur.Progress = mustJSON(s.progressOf(lr, cur))
		if len(snap.Prog.LastPK) > 0 && run.Trigger != TrigRestore {
			if err := s.repo.UpdateCheckpoint(t.ID, mustJSON(snap.Prog.LastPK)); err != nil {
				slog.Warn("保存断点失败", "task", t.ID, "err", err)
			}
		}
	}
	if lr.ctl.Paused() {
		cur.Result = ResPaused
	} else {
		cur.Result = ResRunning
	}
	if err := s.repo.SaveRun(cur); err != nil {
		slog.Warn("保存运行心跳失败", "run", run.ID, "err", err)
	}
}

// ---- 结束 ----

// sanitizeErr 去掉错误信息里可能出现的 user:password@ 连接串片段。
func sanitizeErr(err error) string {
	msg := err.Error()
	for {
		i := strings.Index(msg, "@tcp(")
		if i < 0 {
			break
		}
		j := strings.LastIndex(msg[:i], " ")
		if j < 0 {
			j = -1
		}
		msg = msg[:j+1] + "***" + msg[i:]
	}
	return truncRunes(msg, 500)
}

func manifestPath(m *engine.Manifest) string {
	if m == nil || len(m.Files) == 0 {
		return ""
	}
	return path.Dir(m.Files[0].Key)
}

type backupRec struct {
	Kind           string                `json:"kind"` // table | file
	InstanceID     uint                  `json:"instance_id,omitempty"`
	InstanceName   string                `json:"instance_name,omitempty"`
	Location       string                `json:"location,omitempty"`
	Rows           int64                 `json:"rows"`
	StoreID        uint                  `json:"store_id,omitempty"`
	StoreName      string                `json:"store_name,omitempty"`
	Path           string                `json:"path,omitempty"`
	Bytes          int64                 `json:"bytes,omitempty"`
	Encrypted      bool                  `json:"encrypted,omitempty"`
	Enc            string                `json:"enc,omitempty"`
	ManifestSHA256 string                `json:"manifest_sha256,omitempty"`
	ManifestOK     bool                  `json:"manifest_ok,omitempty"`
	Files          []engine.ManifestFile `json:"files,omitempty"`
}

func (s *Service) backupsOf(run *Run, spec engine.Spec, res *engine.RunResult) []backupRec {
	var out []backupRec
	if res == nil || run.Trigger == TrigRestore {
		return out
	}
	if res.DestTable != "" && res.Backed > 0 {
		name := run.InstanceName
		id := run.InstanceID
		if spec.Dest != nil {
			id = spec.Dest.InstanceID
			if in, err := s.instanceByID(id); err == nil {
				name = in.InstanceName
			}
		}
		out = append(out, backupRec{Kind: "table", InstanceID: id, InstanceName: name, Location: qualifyDest(res.DestTable, spec), Rows: res.Backed})
	}
	if m := res.Manifest; m != nil && len(m.Files) > 0 && spec.File != nil {
		var bytes int64
		for _, f := range m.Files {
			bytes += f.Size
		}
		sum := sha256.Sum256([]byte(mustJSON(m)))
		sname := ""
		if st, err := s.repo.GetStore(spec.File.StoreID); err == nil && st != nil {
			sname = st.Name
		}
		out = append(out, backupRec{Kind: "file", StoreID: spec.File.StoreID, StoreName: sname, Path: manifestPath(m), Rows: m.Rows, Bytes: bytes,
			Encrypted: m.Enc != "", Enc: m.Enc, ManifestSHA256: hex.EncodeToString(sum[:]), ManifestOK: res.Err == "" || res.Result == engine.ResultSuccess || res.Result == engine.ResultWindowEnd || res.Result == engine.ResultLimitReached || res.Result == engine.ResultStopped,
			Files: m.Files})
	}
	return out
}

// verifyOf 汇总校验结果：引擎上报的 verify 事件 + 按结果推导的几条固定项。
func verifyOf(spec engine.Spec, res *engine.RunResult, reported []verifyRec) []verifyRec {
	out := append([]verifyRec(nil), reported...)
	if res == nil {
		return out
	}
	have := map[string]bool{}
	for _, v := range out {
		have[v.Key] = true
	}
	add := func(key, title string, ok bool, detail string) {
		if !have[key] {
			out = append(out, verifyRec{Key: key, Title: title, OK: ok, Detail: detail})
		}
	}
	bad := res.Result == engine.ResultVerifyFailed
	switch {
	case spec.Mode == engine.ModePurge:
		add("no_backup", "无备份（仅清理）", true, "本任务不备份，只校验删除行数")
	default:
		if spec.Dest != nil {
			add("dest_count", "目标端 COUNT 与读取行数一致", !bad, verifyDetail(bad, res, fmt.Sprintf("%d 批全部一致", res.Batches)))
			add("dest_pk", "目标端主键集合与读到的一致", !bad, verifyDetail(bad, res, fmt.Sprintf("%d 逐批比对通过", res.Backed)))
		}
		if spec.File != nil {
			add("file_rows", "写入行数 = 读到行数", !bad, verifyDetail(bad, res, fmt.Sprintf("%d 行", res.Backed)))
			add("manifest", "清单回读校验（sha256）", res.Manifest != nil && !bad, manifestDetail(res))
		}
	}
	if spec.Mode != engine.ModeCopy {
		add("delete_count", "删除行数 = 读到行数", !bad && (res.Deleted == res.Backed || spec.Mode == engine.ModePurge), fmt.Sprintf("读取 %d 行，删除 %d 行", res.Scanned, res.Deleted))
	}
	return out
}

// verifyDetail 返回校验项的详情：失败取引擎错误原文（截断），通过用 ok（调用处按各自的格式串生成）。
func verifyDetail(bad bool, res *engine.RunResult, ok string) string {
	if bad {
		return truncRunes(res.Err, 400)
	}
	return ok
}

func manifestDetail(res *engine.RunResult) string {
	if res.Manifest == nil {
		return "运行未生成清单"
	}
	return fmt.Sprintf("%d 个文件，回读校验 sha256 一致", len(res.Manifest.Files))
}

func resultNotice(result string) (event, title string) {
	switch result {
	case engine.ResultSuccess:
		return evDone, "归档完成" // i18n-ignore：通知标题，语言属于接收方
	case engine.ResultWindowEnd:
		return evInterrupt, "执行窗口结束，本次中断" // i18n-ignore：通知标题，语言属于接收方
	case engine.ResultLoadAbort:
		return evInterrupt, "负载超限等待超时，本次中断" // i18n-ignore：通知标题，语言属于接收方
	case engine.ResultLimitReached:
		return evInterrupt, "达到单次行数上限，本次结束" // i18n-ignore：通知标题，语言属于接收方
	case engine.ResultFailed:
		return evFailed, "归档失败" // i18n-ignore：通知标题，语言属于接收方
	case engine.ResultVerifyFailed:
		return evVerify, "备份校验失败（该批未删除源数据）" // i18n-ignore：通知标题，语言属于接收方
	case engine.ResultStructure:
		return evStructure, "源表结构变更，任务已暂停" // i18n-ignore：通知标题，语言属于接收方
	}
	return "", ""
}

// finishRun 写运行结果、更新任务累计 / 断点 / 状态，并发通知与审计。
func (s *Service) finishRun(lr *liveRun, run *Run, t *Task, spec engine.Spec, res *engine.RunResult, runErr error) {
	now := s.now()
	result := engine.ResultFailed
	errMsg := ""
	if res != nil {
		result, errMsg = res.Result, res.Err
	}
	if runErr != nil {
		if res == nil {
			result = engine.ResultFailed
		}
		if errMsg == "" {
			errMsg = runErr.Error()
		}
		if errors.Is(runErr, context.Canceled) && res == nil {
			result, errMsg = engine.ResultStopped, ""
		}
	}
	lr.sink.closeThrottle(result)
	snap := lr.sink.snapshot()
	cur, err := s.repo.GetRun(run.ID)
	if err != nil || cur == nil {
		slog.Error("读取运行记录失败，无法写入结果", "run", run.ID, "err", err)
		return
	}
	*run = *cur
	run.Result, run.FinishedAt, run.DurationMs = result, &now, now.Sub(run.StartedAt).Milliseconds()
	run.LeaseUntil, run.Err = nil, truncRunes(errMsg, 4000)
	if res != nil {
		run.Scanned, run.Backed, run.Deleted, run.Batches = res.Scanned, res.Backed, res.Deleted, res.Batches
		run.ThrottleCount, run.AutoPause = res.ThrottleCount, res.AutoPause
		if res.Duration > 0 {
			run.DurationMs = res.Duration.Milliseconds()
		}
		if res.PeakThreads > snap.PeakThr {
			snap.PeakThr = res.PeakThreads
		}
		lr.mu.Lock()
		if len(lr.pkCols) > 0 {
			run.PKCols = mustJSON(lr.pkCols)
		}
		lr.mu.Unlock()
		run.MinPK, run.MaxPK = mustJSON(res.MinPK), mustJSON(res.MaxPK)
		if res.DestTable != "" {
			run.DestTable = qualifyDest(res.DestTable, spec)
			if spec.Dest != nil {
				run.DestInstanceID = spec.Dest.InstanceID
			}
		}
		if res.Manifest != nil {
			run.Manifest = mustJSON(res.Manifest)
		}
	} else if snap.HasProg {
		run.Scanned, run.Backed, run.Deleted, run.Batches = snap.Prog.Scanned, snap.Prog.Backed, snap.Prog.Deleted, snap.Prog.Batches
	}
	if snap.ThrCount > run.ThrottleCount {
		run.ThrottleCount = snap.ThrCount
	}
	run.PeakThreads, run.PeakLag = snap.PeakThr, snap.PeakLag
	if run.Trigger == TrigRestore {
		run.Note = fmt.Sprintf("按运行 #%d 的主键范围 INSERT IGNORE 回灌（只补缺、不覆盖）：备份中 %d 行，补回 %d 行", run.RestoreOf, run.Scanned, run.Backed)
	}
	if run.Trigger != TrigRestore {
		run.Backups = mustJSON(s.backupsOf(run, spec, res))
	}
	run.Verify = mustJSON(verifyOf(spec, res, snap.Verify))
	run.Throttles = mustJSON(snap.Thr)
	if snap.HasProg {
		last := lr.progressSnapshot(run, snap)
		last.State = "finished"
		run.Progress = mustJSON(last)
	}
	if err := s.repo.SaveRun(run); err != nil {
		slog.Error("保存运行结果失败", "run", run.ID, "err", err)
	}
	if err := s.repo.TrimEvents(run.ID, eventKeep); err != nil {
		slog.Warn("清理运行事件失败", "run", run.ID, "err", err)
	}
	if run.Trigger != TrigRestore {
		s.updateTaskAfterRun(run, t, res, result, errMsg)
	}
	s.noticeRun(run, t, result, errMsg)
}

// qualifyDest 把引擎返回的备份表名补成 库.表（引擎只返回展开后的表名）。
func qualifyDest(name string, spec engine.Spec) string {
	if strings.Contains(name, ".") || spec.Dest == nil {
		return name
	}
	return spec.Dest.DB + "." + name
}

func (s *Service) updateTaskAfterRun(run *Run, t *Task, res *engine.RunResult, result, errMsg string) {
	err := s.repo.InTx(func(tx Repo) error {
		cur, err := tx.GetTask(t.ID, true)
		if err != nil || cur == nil {
			return err
		}
		now := s.now()
		cur.LastRunID = run.ID
		if run.Trigger == TrigSchedule || run.Trigger == TrigManual {
			cur.CumRuns++
			if cur.Mode == string(engine.ModeCopy) {
				cur.CumRows += run.Backed
			} else {
				cur.CumRows += run.Deleted
			}
		}
		if res != nil && len(res.Checkpoint) > 0 {
			cur.Checkpoint = mustJSON(res.Checkpoint)
		} else if res != nil && cur.Mode != string(engine.ModeCopy) {
			// 归档并删除 / 仅清理：不排序读取、不用断点（删掉的行不会再被读到），升级前遗留的旧断点一并清掉；
			// 主键含字符串列等仍按主键排序读取的表，跑完（成功）时引擎也会清空断点，下次从头（才能捡到后来才过期的小主键行）。
			cur.Checkpoint = ""
		}
		if cur.Status == StEnabled {
			cur.NextRunAt = ptrTime(nextRun(cur.cfg().Schedule, now))
			pause := res != nil && res.AutoPause
			if result == engine.ResultStructure {
				cur.Status, cur.PauseKind, cur.PauseAt = StPaused, PauseStructure, &now
				cur.PauseReason = withDetail("源表结构指纹与审批时不一致（列名 / 类型 / 主键有变化），已暂停。需要重新试运行并重新提交审批。", errMsg)
				cur.NextRunAt = nil
				cur.addHistory(now, "system", "结构指纹变化，任务已暂停并通知")
			} else if pause {
				cur.Status, cur.PauseKind, cur.PauseAt = StPaused, PauseAuto, &now
				cur.PauseReason = autoPauseReason(result, errMsg)
				cur.NextRunAt = nil
				cur.addHistory(now, "system", fmt.Sprintf("自动暂停：%s", truncRunes(cur.PauseReason, 200)))
			}
		}
		cur.UpdatedAt = now
		return tx.SaveTask(cur)
	})
	if err != nil {
		slog.Error("更新归档任务累计失败", "task", t.ID, "run", run.ID, "err", err)
	}
}

// withDetail 在说明后面补上引擎错误详情（无详情时原样返回）；写成单个格式串，便于按目录翻译。
func withDetail(msg, e string) string {
	if e == "" {
		return msg
	}
	return fmt.Sprintf("%s 详情：%s", msg, truncRunes(e, 400))
}

func autoPauseReason(result, errMsg string) string {
	switch result {
	case engine.ResultVerifyFailed:
		return withDetail("备份校验不一致，已自动暂停（该批未删除任何源数据）。", errMsg)
	case engine.ResultFailed:
		return withDetail("连续多批失败，已自动暂停；失败批已回滚，未删除任何未备份的数据。", errMsg)
	}
	return withDetail("任务已自动暂停。", errMsg)
}

func (s *Service) noticeRun(run *Run, t *Task, result, errMsg string) {
	by := Caller{Principal: systemCaller.Principal}
	by.Username = run.ByUser
	if by.Username == "" {
		by.Username = "system"
	}
	s.auditAt(by, "运行结束", fmt.Sprintf("运行 #%d「%s」(%s)结束：%s，读取 %d 行，备份 %d 行，删除 %d 行", run.ID, run.TaskName, run.Trigger, result, run.Scanned, run.Backed, run.Deleted)) // i18n-ignore：审计 / 通知正文，不翻译
	if run.Trigger == TrigRestore {
		return
	}
	name, title := resultNotice(result)
	if name != "" {
		s.notifyRun(t, run, name, title, errMsg)
	}
	if run.AutoPause && result != engine.ResultStructure {
		s.notifyRun(t, run, evPaused, "任务已自动暂停", errMsg) // i18n-ignore：审计 / 通知正文，不翻译
	}
}

func (s *Service) notifyRun(t *Task, run *Run, specific, title, errMsg string) {
	name := notifyName(specific, run.Result)
	content := taskContent(t, s.instNameOf(t.InstanceID), fmt.Sprintf("运行：#%d（%s）\n读取 %d 行 · 备份 %d 行 · 删除 %d 行 · %d 批", run.ID, run.Trigger, run.Scanned, run.Backed, run.Deleted, run.Batches), // i18n-ignore：通知正文，语言属于接收方
		suffixErr0(errMsg), s.taskLink(t))
	s.notifyAsync(notify.Event{Kind: "archive", Name: name, Specific: specific, Title: "[数据归档]" + t.Name + "：" + title, Content: content, GroupID: t.ResourceGroupID, Applicant: t.Creator}) // i18n-ignore：审计 / 通知正文，不翻译
}

func suffixErr0(e string) string {
	if e == "" {
		return ""
	}
	return "错误：" + truncRunes(e, 300) // i18n-ignore：通知正文，语言属于接收方
}
