// @author: kevinkelly
// @license: Apache Licence
// @file: runs_test.go
// @time: 2026/10/28

package archive

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"dbtool/internal/archive/engine"
)

func (e *env) lastRun() Run {
	rs := e.repo.allRuns()
	if len(rs) == 0 {
		e.t.Fatal("没有运行记录")
	}
	return rs[0]
}

func (e *env) runNow(id uint) uint {
	e.t.Helper()
	r, err := e.svc.RunNow(e.dba, id, true, true)
	if err != nil {
		e.t.Fatalf("立即执行: %v", err)
	}
	return r.RunID
}

func TestRunNowSuccess(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("成功运行")
	_, err := e.svc.RunNow(e.dba, task.ID, true, false)
	mustErrCode(t, err, 422, "confirm_required")
	stale, _ := e.repo.GetTask(task.ID, false)
	stale.Checkpoint = `["0"]` // 运行中节流写入的旧断点：成功跑完后必须被清掉
	if err := e.repo.SaveTask(stale); err != nil {
		t.Fatal(err)
	}
	runID := e.runNow(task.ID)
	e.wait()

	d, err := e.svc.GetRun(e.dba, runID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Result != engine.ResultSuccess || d.Scanned != 300 || d.Backed != 300 || d.Deleted != 300 || d.Trigger != TrigManual || d.By != "dba_li" || d.Batches != 3 {
		t.Fatalf("运行结果: %+v", d.RunView)
	}
	if d.DestTable != "shop_arch.orders_bak" || len(d.Backups) != 1 || d.Backups[0].Kind != "table" || d.Backups[0].Location != "shop_arch.orders_bak" || d.Backups[0].Rows != 300 {
		t.Errorf("备份位置: %q %+v", d.DestTable, d.Backups)
	}
	if len(d.Verify) == 0 || !d.CanRestore || d.FinishedAt == "" || len(d.PKCols) != 1 || len(d.MinPK) != 1 || len(d.MaxPK) != 1 {
		t.Errorf("校验 / 主键范围 / 可恢复: %+v", d)
	}
	for _, v := range d.Verify {
		if !v.OK {
			t.Errorf("校验项失败: %+v", v)
		}
	}
	if e.h.Src.Len() != 0 || e.h.Dst == nil {
		t.Errorf("源表应被清空: %d", e.h.Src.Len())
	}
	if len(d.Log) == 0 {
		t.Error("应有日志")
	}
	if evs, last, err := e.svc.RunEvents(e.dba, runID, 0, 0); err != nil || len(evs) == 0 || last == 0 {
		t.Errorf("事件应已落库: %v %d", err, len(evs))
	} else if more, _, _ := e.svc.RunEvents(e.dba, runID, last, 0); len(more) != 0 {
		t.Errorf("增量拉取应为空: %d", len(more))
	}

	cur, _ := e.repo.GetTask(task.ID, false)
	if cur.CumRows != 300 || cur.CumRuns != 1 || cur.LastRunID != runID || cur.NextRunAt == nil || cur.Status != StEnabled {
		t.Errorf("任务累计 / 断点: %+v", cur)
	}
	if cur.Checkpoint != "" {
		t.Errorf("归档并删除成功跑完后断点应清空（下次从头）: %q", cur.Checkpoint)
	}
	tv, _ := e.svc.GetTask(e.dba, task.ID)
	if tv.Running != nil || tv.LastRun == nil || tv.LastRun.Result != "success" || tv.CumRows != 300 || len(tv.RecentRuns) != 1 {
		t.Errorf("任务详情: running=%v last=%+v cum=%d runs=%d", tv.Running, tv.LastRun, tv.CumRows, len(tv.RecentRuns))
	}
	got := e.notif.names()
	if !contains(got, "executing") || !contains(got, "finished") {
		t.Errorf("通知: %v", got)
	}
	acts := e.repo.auditActions()
	if !contains(acts, "数据归档-立即执行") || !contains(acts, "数据归档-运行结束") {
		t.Errorf("审计: %v", acts)
	}
	// 没有数据了：再跑一次也是成功（0 行）
	runID = e.runNow(task.ID)
	e.wait()
	if r, _ := e.svc.GetRun(e.dba, runID); r.Result != engine.ResultSuccess || r.Scanned != 0 {
		t.Errorf("空跑: %+v", r.RunView)
	}
}

func TestRunListFilters(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("列表")
	e.runNow(task.ID)
	e.wait()
	rows, total, err := e.svc.ListRuns(e.rd, RunQuery{})
	if err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("列表: %v %d", err, total)
	}
	if rows, _, _ = e.svc.ListRuns(e.rd, RunQuery{Trigger: TrigDryrun}); len(rows) != 1 || rows[0].Trigger != TrigDryrun {
		t.Errorf("按触发方式: %+v", rows)
	}
	if rows, _, _ = e.svc.ListRuns(e.rd, RunQuery{Result: "success", TaskID: task.ID}); len(rows) != 1 {
		t.Errorf("按结果 / 任务: %d", len(rows))
	}
	other := caller(40, "other", false, "archive.view")
	e.repo.userRGs[40] = []uint{2}
	if _, total, _ = e.svc.ListRuns(other, RunQuery{}); total != 0 {
		t.Errorf("别的资源组看不到: %d", total)
	}
	_, err = e.svc.GetRun(other, rows[0].ID)
	mustErrCode(t, err, 404, "not_found")
	_, err = e.svc.RunManifest(e.dba, rows[0].ID)
	mustErrCode(t, err, 404, "not_found")
}

func TestRestoreRun(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("可恢复")
	runID := e.runNow(task.ID)
	e.wait()
	if e.h.Src.Len() != 0 {
		t.Fatal("前置：源表应已清空")
	}
	_, err := e.svc.Restore(e.rd, runID, task.Name)
	mustErrCode(t, err, 403, "forbidden")
	_, err = e.svc.Restore(e.dba, runID, "不是任务名")
	mustErrCode(t, err, 422, "confirm_required")
	res, err := e.svc.Restore(e.dba, runID, task.Name)
	if err != nil {
		t.Fatal(err)
	}
	e.wait()
	rr, _ := e.svc.GetRun(e.dba, res.RunID)
	if rr.Trigger != TrigRestore || rr.RestoreOf != runID || rr.Result != engine.ResultSuccess || rr.Backed != 300 {
		t.Fatalf("恢复运行: %+v", rr.RunView)
	}
	if e.h.Src.Len() != 300 {
		t.Errorf("源表应补回 300 行: %d", e.h.Src.Len())
	}
	if orig, _ := e.svc.GetRun(e.dba, runID); len(orig.RestoredBy) != 1 || orig.RestoredBy[0] != res.RunID {
		t.Errorf("原运行应记录被恢复过: %+v", orig.RestoredBy)
	}
	// 恢复运行本身不能再恢复，也不计入任务累计
	_, err = e.svc.Restore(e.dba, res.RunID, task.Name)
	mustErrCode(t, err, 409, "conflict")
	if cur, _ := e.repo.GetTask(task.ID, false); cur.CumRuns != 1 || cur.CumRows != 300 {
		t.Errorf("恢复不计入累计: %+v", cur)
	}
	// 再恢复一次：只补缺、不覆盖 → 补回 0 行
	res2, err := e.svc.Restore(e.dba, runID, task.Name)
	if err != nil {
		t.Fatal(err)
	}
	e.wait()
	if rr, _ = e.svc.GetRun(e.dba, res2.RunID); rr.Backed != 0 || rr.Result != engine.ResultSuccess {
		t.Errorf("重复恢复应补回 0 行: %+v", rr.RunView)
	}
	if !contains(e.repo.auditActions(), "数据归档-恢复数据") {
		t.Error("恢复应写审计")
	}
}

func TestRunSkipRules(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("跳过规则")
	try := func(ign bool) error { _, err := e.svc.RunNow(e.dba, task.ID, ign, true); return err }

	// 未结束的 SQL 上线工单
	e.repo.openWF = []OpenWorkflow{{ID: 77, Name: "加字段", Status: "workflow_executing"}}
	pe := mustErrCode(t, try(true), 409, "skipped")
	if !strings.Contains(pe.Message, "#77") || !strings.Contains(pe.Message, "执行中") {
		t.Errorf("工单提示: %s", pe.Message)
	}
	e.repo.openWF = nil

	// 窗口外
	e.h.Clock.Advance(10 * time.Hour) // 12:00
	pe = mustErrCode(t, try(false), 409, "skipped")
	if !strings.Contains(pe.Message, "执行窗口") {
		t.Errorf("窗口提示: %s", pe.Message)
	}
	e.h.Clock.Advance(-10 * time.Hour)

	// 存储不可用（文件备份）
	e.repo.stores[1].Status = "err"
	e.repo.stores[1].LastTestMsg = "i/o timeout"
	cur, _ := e.repo.GetTask(task.ID, false)
	c := cur.cfg()
	c.DestFile = &engine.FileDest{StoreID: 1, Format: "csv", PathTemplate: "{db}/{table}"}
	cur.setCfg(c)
	e.repo.tasks[cur.ID].DestFile = cur.DestFile
	pe = mustErrCode(t, try(true), 409, "skipped")
	if !strings.Contains(pe.Message, "不可用") {
		t.Errorf("存储提示: %s", pe.Message)
	}
	e.repo.stores[1].Status = "ok"
	e.repo.stores[1].Enabled = false
	pe = mustErrCode(t, try(true), 409, "skipped")
	if !strings.Contains(pe.Message, "已停用") {
		t.Errorf("存储停用提示: %s", pe.Message)
	}
	e.repo.stores[1].Enabled = true
	e.repo.tasks[cur.ID].DestFile = ""

	// 实例并发上限（策略默认 2）：另外两个任务的运行占着
	lease := e.h.Clock.Now().Add(time.Minute)
	for i := uint(1); i <= 2; i++ {
		e.repo.runs[900+i] = &Run{ID: 900 + i, TaskID: 500 + i, InstanceID: 1, Result: ResRunning, LeaseUntil: &lease}
	}
	pe = mustErrCode(t, try(true), 409, "skipped")
	if !strings.Contains(pe.Message, "同一实例同时最多 2 个") {
		t.Errorf("并发提示: %s", pe.Message)
	}
	// 租约过期的不算
	past := e.h.Clock.Now().Add(-time.Minute)
	e.repo.runs[901].LeaseUntil, e.repo.runs[902].LeaseUntil = &past, &past
	delete(e.repo.runs, 902)
	id := e.runNow(task.ID)
	e.wait()
	if r, _ := e.svc.GetRun(e.dba, id); r.Result != engine.ResultSuccess {
		t.Errorf("过期租约不应占并发: %+v", r.RunView)
	}

	// 状态不对
	if _, err := e.svc.PauseTask(e.dba, task.ID, ""); err != nil {
		t.Fatal(err)
	}
	pe = mustErrCode(t, try(true), 409, "skipped")
	if !strings.Contains(pe.Message, "只有已启用") {
		t.Errorf("状态提示: %s", pe.Message)
	}
}

func TestSchedulerTick(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("定时")
	ctx := context.Background()
	now := e.h.Clock.Now()

	// 还没到点：只补算下次运行时间
	e.svc.Tick(ctx)
	e.wait()
	if len(e.repo.runs) != 1 { // 只有试运行
		t.Fatalf("未到点不应运行: %d", len(e.repo.runs))
	}
	// 到点且在窗口内
	past := now.Add(-time.Minute)
	e.repo.tasks[task.ID].NextRunAt = &past
	e.svc.Tick(ctx)
	e.wait()
	r := e.lastRun()
	if r.Trigger != TrigSchedule || r.Result != engine.ResultSuccess || r.ByUser != "system" {
		t.Fatalf("定时运行: %+v", r)
	}
	cur, _ := e.repo.GetTask(task.ID, false)
	if cur.NextRunAt == nil || !cur.NextRunAt.After(now) {
		t.Errorf("下次运行应推进: %v", cur.NextRunAt)
	}
	n := len(e.repo.runs)
	e.svc.Tick(ctx)
	e.wait()
	if len(e.repo.runs) != n {
		t.Error("同一触发不应重复运行")
	}

	// 到点但有未结束的 SQL 工单：记一条跳过
	e.repo.openWF = []OpenWorkflow{{ID: 9, Name: "改表", Status: "workflow_queuing"}}
	e.repo.tasks[task.ID].NextRunAt = &past
	e.svc.Tick(ctx)
	e.wait()
	r = e.lastRun()
	if r.Result != ResSkipped || r.Trigger != TrigSchedule || !strings.Contains(r.Note, "#9") {
		t.Fatalf("应记录跳过: %+v", r)
	}
	e.repo.openWF = nil

	// 到点但已错过窗口：不运行、不记录，推进到下次
	e.h.Clock.Advance(5 * time.Hour) // 07:00，窗口 01:00-05:00
	n = len(e.repo.runs)
	e.repo.tasks[task.ID].NextRunAt = &past
	e.svc.Tick(ctx)
	e.wait()
	if len(e.repo.runs) != n {
		t.Error("错过窗口不应运行或记录")
	}
	if cur, _ = e.repo.GetTask(task.ID, false); !cur.NextRunAt.After(e.h.Clock.Now()) {
		t.Errorf("错过窗口也应推进下次运行: %v", cur.NextRunAt)
	}

	// 暂停的任务不会被调度
	e.h.Clock.Advance(-5 * time.Hour)
	if _, err := e.svc.PauseTask(e.dba, task.ID, ""); err != nil {
		t.Fatal(err)
	}
	n = len(e.repo.runs)
	e.svc.Tick(ctx)
	e.wait()
	if len(e.repo.runs) != n {
		t.Error("已暂停的任务不应运行")
	}
}

func TestSchedulerRunsStillRunningIsSkipped(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("上次未结束")
	gate := make(chan struct{})
	var once sync.Once
	e.h.Src.OnRead = func(call int, _ engine.ReadReq) error {
		once.Do(func() { <-gate })
		return nil
	}
	id := e.runNow(task.ID)
	waitFor(t, func() bool { return e.svc.liveOfTask(task.ID) != nil })
	past := e.h.Clock.Now().Add(-time.Minute)
	e.repo.tasks[task.ID].NextRunAt = &past
	e.svc.Tick(context.Background())
	var skipped bool
	for _, r := range e.repo.allRuns() {
		if r.Result == ResSkipped && strings.Contains(r.Note, "还没有结束") {
			skipped = true
		}
	}
	if !skipped {
		t.Error("上次未结束时本次应记一条跳过")
	}
	// 运行中：任务详情有 running，进度可见，不能编辑 / 停用 / 删除 / 再次执行
	tv, _ := e.svc.GetTask(e.dba, task.ID)
	if tv.Running == nil || tv.Running.RunID != id || !tv.Running.Stop || tv.Allow.Run || tv.Allow.Edit || tv.Allow.Disable {
		t.Errorf("运行中的任务视图: %+v %+v", tv.Running, tv.Allow)
	}
	ps, err := e.svc.Progress(e.dba, 0)
	if err != nil || len(ps) != 1 || ps[0].RunID != id || ps[0].State != "running" || ps[0].SetBatchSize != 100 {
		t.Errorf("进度: %v %+v", err, ps)
	}
	cur, _ := e.repo.GetTask(task.ID, false)
	in := inputOf(cur)
	_, err = e.svc.UpdateTask(e.dba, task.ID, in)
	mustErrCode(t, err, 409, "running")
	_, err = e.svc.DisableTask(e.dba, task.ID, true)
	mustErrCode(t, err, 409, "running")
	_, err = e.svc.PauseTask(e.dba, task.ID, "")
	mustErrCode(t, err, 409, "running")
	_, err = e.svc.RunNow(e.dba, task.ID, true, true)
	mustErrCode(t, err, 409, "skipped")
	close(gate)
	e.wait()
	if r, _ := e.svc.GetRun(e.dba, id); r.Result != engine.ResultSuccess {
		t.Errorf("放行后应成功: %+v", r.RunView)
	}
	if ps, _ = e.svc.Progress(e.dba, 0); len(ps) != 0 && ps[0].State != "finished" {
		t.Errorf("结束后不应再有进行中的进度: %+v", ps)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}

func TestPauseResumeStopRun(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("控制")
	gate := make(chan struct{})
	var once sync.Once
	e.h.Src.OnRead = func(call int, _ engine.ReadReq) error {
		once.Do(func() { <-gate })
		return nil
	}
	id := e.runNow(task.ID)
	waitFor(t, func() bool { return e.svc.liveOfRun(id) != nil })

	_, err := e.svc.PauseRun(e.rd, id)
	mustErrCode(t, err, 403, "forbidden")
	v, err := e.svc.PauseRun(e.dba, id)
	if err != nil || v.Result != ResPaused {
		t.Fatalf("暂停: %v %+v", err, v)
	}
	if tv, _ := e.svc.GetTask(e.dba, task.ID); tv.Running == nil || tv.Running.State != "paused" || !tv.Running.Resume || tv.Running.Pause {
		t.Errorf("暂停后的任务视图: %+v", tv.Running)
	}
	if r, _ := e.repo.GetRun(id); r.Result != ResPaused {
		t.Errorf("运行记录应为 paused: %s", r.Result)
	}
	if v, err = e.svc.ResumeRun(e.dba, id); err != nil || v.Result != ResRunning {
		t.Fatalf("继续: %v %+v", err, v)
	}
	_, err = e.svc.StopRun(e.dba, id, false)
	mustErrCode(t, err, 422, "confirm_required")
	if _, err = e.svc.StopRun(e.dba, id, true); err != nil {
		t.Fatal(err)
	}
	close(gate)
	e.wait()
	r, _ := e.svc.GetRun(e.dba, id)
	if r.Result != engine.ResultStopped {
		t.Fatalf("终止后结果应为 stopped: %+v", r.RunView)
	}
	// 已结束的运行不能再控制
	_, err = e.svc.PauseRun(e.dba, id)
	mustErrCode(t, err, 409, "conflict")
	acts := e.repo.auditActions()
	for _, w := range []string{"数据归档-暂停运行", "数据归档-恢复运行", "数据归档-终止运行"} {
		if !contains(acts, w) {
			t.Errorf("缺审计 %s: %v", w, acts)
		}
	}
	if cur, _ := e.repo.GetTask(task.ID, false); cur.Status != StEnabled {
		t.Errorf("终止运行不改变任务状态: %s", cur.Status)
	}
}

func TestFailedRunAutoPausesTask(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("连续失败")
	e.h.Src.OnRead = func(call int, _ engine.ReadReq) error { return errors.New("ERROR 1205: Lock wait timeout exceeded") }
	id := e.runNow(task.ID)
	e.wait()
	r, _ := e.svc.GetRun(e.dba, id)
	if r.Result != engine.ResultFailed || !r.AutoPause || r.Err == "" || r.Deleted != 0 {
		t.Fatalf("运行: %+v", r.RunView)
	}
	cur, _ := e.repo.GetTask(task.ID, false)
	if cur.Status != StPaused || cur.PauseKind != PauseAuto || !strings.Contains(cur.PauseReason, "自动暂停") || cur.NextRunAt != nil {
		t.Errorf("任务应被自动暂停: %+v", cur)
	}
	if e.h.Src.Len() != 300 {
		t.Error("失败不能删除数据")
	}
	names := e.notif.names()
	if len(names) < 3 || !contains(names, "exception") {
		t.Errorf("应通知失败与自动暂停: %v", names)
	}
	// 人工恢复后继续
	e.h.Src.OnRead = nil
	if v, err := e.svc.ResumeTask(e.dba, task.ID); err != nil || v.Status != StEnabled {
		t.Fatalf("恢复: %v", err)
	}
	id = e.runNow(task.ID)
	e.wait()
	if r, _ = e.svc.GetRun(e.dba, id); r.Result != engine.ResultSuccess {
		t.Errorf("恢复后应成功: %+v", r.RunView)
	}
}

func TestStructureChangePausesTask(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("结构变更")
	e.h.Src.SetFingerprint("changed-fingerprint")
	id := e.runNow(task.ID)
	e.wait()
	r, _ := e.svc.GetRun(e.dba, id)
	if r.Result != engine.ResultStructure {
		t.Fatalf("应为 structure_changed: %+v", r.RunView)
	}
	cur, _ := e.repo.GetTask(task.ID, false)
	if cur.Status != StPaused || cur.PauseKind != PauseStructure {
		t.Fatalf("任务应因结构变更暂停: %+v", cur)
	}
	_, err := e.svc.ResumeTask(e.dba, task.ID)
	mustErrCode(t, err, 409, "conflict")
	if tv, _ := e.svc.GetTask(e.dba, task.ID); tv.Allow.Resume || tv.Pause.Kind != PauseStructure {
		t.Errorf("结构变更暂停不可直接恢复: %+v", tv.Allow)
	}
	if e.h.Src.Len() != 300 {
		t.Error("不能删除数据")
	}
	// 重新试运行并提交审批后才能重新启用
	cur, _ = e.repo.GetTask(task.ID, false)
	e.h.Src.SetFingerprint(engine.Fingerprint(e.h.Src.Info.Columns, e.h.Src.Info.PrimaryKey))
	in := inputOf(cur)
	in = e.checked(e.dba, in)
	in.Version = cur.Version
	in.TaskID = task.ID
	r2, err := e.svc.UpdateTask(e.dba, task.ID, in)
	if err != nil || r2.Outcome != "submitted" || r2.Task.Status != StPending {
		t.Fatalf("重新提交: %v %+v", err, r2)
	}
}

func TestVerifyFailedStopsWithoutDeleting(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("校验失败")
	e.h.Dst.VerifyAdjust = func(call int, found int64) int64 { return found - 1 }
	id := e.runNow(task.ID)
	e.wait()
	r, _ := e.svc.GetRun(e.dba, id)
	if r.Result != engine.ResultVerifyFailed || r.Deleted != 0 {
		t.Fatalf("校验失败不能删源: %+v", r.RunView)
	}
	if e.h.Src.Len() != 300 {
		t.Error("源数据必须保留")
	}
	bad := 0
	for _, v := range r.Verify {
		if !v.OK {
			bad++
		}
	}
	if bad == 0 {
		t.Errorf("校验结果里应有失败项: %+v", r.Verify)
	}
	cur, _ := e.repo.GetTask(task.ID, false)
	if cur.Status != StPaused || cur.PauseKind != PauseAuto {
		t.Errorf("校验失败应自动暂停: %+v", cur)
	}
}

func TestOpenFailureRecordedAsFailedRun(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("连不上")
	e.openErr = errors.New("dial tcp 10.0.0.1:3306: connect: connection refused")
	id := e.runNow(task.ID)
	e.wait()
	r, _ := e.svc.GetRun(e.dba, id)
	if r.Result != engine.ResultFailed || !strings.Contains(r.Err, "连接源") {
		t.Errorf("连接失败应记为失败的运行: %+v", r.RunView)
	}
	if cur, _ := e.repo.GetTask(task.ID, false); cur.Status != StEnabled {
		t.Errorf("连接失败不自动暂停任务: %s", cur.Status)
	}
	if e.svc.liveOfTask(task.ID) != nil {
		t.Error("失败后不应留在运行表里")
	}
}

func TestLeaseRecovery(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("租约")
	past := e.h.Clock.Now().Add(-5 * time.Minute)
	future := e.h.Clock.Now().Add(time.Minute)
	e.repo.runs[801] = &Run{ID: 801, TaskID: task.ID, TaskName: task.Name, Result: ResRunning, StartedAt: past, LeaseUntil: &past}
	e.repo.runs[802] = &Run{ID: 802, TaskID: task.ID, TaskName: task.Name, Result: ResPaused, StartedAt: past, LeaseUntil: &past}
	e.repo.runs[803] = &Run{ID: 803, TaskID: task.ID, TaskName: task.Name, Result: ResRunning, StartedAt: past, LeaseUntil: &future}
	e.repo.runs[804] = &Run{ID: 804, TaskID: task.ID, TaskName: task.Name, Result: engine.ResultSuccess, StartedAt: past, LeaseUntil: &past}
	if n := e.svc.ExpireStale(); n != 2 {
		t.Errorf("应回收 2 条: %d", n)
	}
	for id, want := range map[uint]string{801: ResInterrupted, 802: ResInterrupted, 803: ResRunning, 804: engine.ResultSuccess} {
		if got := e.repo.runs[id].Result; got != want {
			t.Errorf("run %d: %s want %s", id, got, want)
		}
	}
	if e.repo.runs[801].FinishedAt == nil {
		t.Error("中断的运行应有结束时间")
	}
	// Tick 也会回收；中断的运行不占并发
	e.repo.runs[805] = &Run{ID: 805, TaskID: task.ID, InstanceID: 1, Result: ResRunning, StartedAt: past, LeaseUntil: &past}
	e.svc.Tick(context.Background())
	if e.repo.runs[805].Result != ResInterrupted {
		t.Error("Tick 应回收过期租约")
	}
}

func TestFileBackupRunAndManifest(t *testing.T) {
	e := newEnv(t)
	in := e.baseInput("文件备份")
	in.DestTable = nil
	in.DestFile = &engine.FileDest{StoreID: 1, Format: "csv", Gzip: true, Encrypt: true, PathTemplate: "{inst}/{db}/{table}/{yyyy}{mm}{dd}/{run_id}"}
	in = e.checked(e.rd, in)
	r, err := e.svc.CreateTask(e.rd, in)
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Approve(e.tl, r.Task.ID, "")
	e.svc.Approve(e.dba, r.Task.ID, "")
	id := e.runNow(r.Task.ID)
	e.wait()
	d, err := e.svc.GetRun(e.dba, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Result != engine.ResultSuccess {
		t.Fatalf("文件备份运行: %+v", d.RunView)
	}
	var file *backupRec
	for i := range d.Backups {
		if d.Backups[i].Kind == "file" {
			file = &d.Backups[i]
		}
	}
	if file == nil || file.Rows != 300 || !file.Encrypted || file.Enc != "aes-256-gcm" || len(file.Files) == 0 || file.ManifestSHA256 == "" || !file.ManifestOK || file.StoreName != "本地归档盘" {
		t.Fatalf("文件备份位置: %+v", d.Backups)
	}
	if d.Manifest == nil || d.Manifest.Rows != 300 {
		t.Errorf("清单: %+v", d.Manifest)
	}
	b, err := e.svc.RunManifest(e.dba, id)
	if err != nil || !strings.Contains(string(b), `"sha256"`) {
		t.Errorf("清单下载: %v %s", err, b)
	}
	if d.CanRestore {
		t.Error("只有文件备份时不能按库表恢复")
	}
	_, err = e.svc.Restore(e.dba, id, "文件备份")
	mustErrCode(t, err, 409, "conflict")
}

func TestLoadAbortIsNotAFailure(t *testing.T) {
	e := newEnv(t)
	in := e.baseInput("负载中断")
	in.Params.MaxWaitSeconds = 60
	in = e.checked(e.rd, in)
	r, err := e.svc.CreateTask(e.rd, in)
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Approve(e.tl, r.Task.ID, "")
	e.svc.Approve(e.dba, r.Task.ID, "")
	e.h.Probe.Threads = func(int) (int, error) { return 200, nil }
	id := e.runNow(r.Task.ID)
	e.wait()
	d, _ := e.svc.GetRun(e.dba, id)
	if d.Result != engine.ResultLoadAbort || d.Err != "" || d.AutoPause || d.Deleted != 0 {
		t.Fatalf("负载中断: %+v", d.RunView)
	}
	if d.ThrottleCount < 1 || len(d.Throttles) < 1 || d.Throttles[len(d.Throttles)-1].End != "abort" || d.Throttles[0].Kind != "threads" {
		t.Errorf("限流记录: %+v", d.Throttles)
	}
	if cur, _ := e.repo.GetTask(r.Task.ID, false); cur.Status != StEnabled || cur.CumRuns != 1 {
		t.Errorf("负载中断不暂停任务: %+v", cur)
	}
}

func TestSinkThrottleAndSampling(t *testing.T) {
	now := time.Date(2026, 10, 8, 2, 0, 0, 0, time.Local)
	clock := func() time.Time { return now }
	k := newRunSink(clock)
	k.Progress(engine.Progress{Scanned: 100, BatchSize: 1000, Threads: 10})
	now = now.Add(2 * time.Second)
	k.Progress(engine.Progress{Scanned: 100, BatchSize: 1000, Threads: 58, Throttled: true, ThrottleReason: "Threads_running=58 > 32"})
	k.Event(engine.Event{Kind: "throttle", Msg: "负载超限", Fields: map[string]string{"reason": "Threads_running=58 > 32"}})
	now = now.Add(30 * time.Second)
	k.Progress(engine.Progress{Scanned: 100, BatchSize: 1000, Threads: 58, Throttled: true, ThrottleReason: "Threads_running=58 > 32"})
	now = now.Add(30 * time.Second)
	k.Progress(engine.Progress{Scanned: 600, BatchSize: 500, Threads: 12})
	s := k.snapshot()
	if s.ThrCount != 1 || len(s.Thr) != 1 {
		t.Fatalf("限流次数: %+v", s.Thr)
	}
	th := s.Thr[0]
	if th.Kind != "threads" || th.Value != "58" || th.Threshold != "32" || th.End != "resume" || th.Waited != 60 || th.BatchTo != 500 {
		t.Errorf("限流记录: %+v", th)
	}
	if s.PeakThr != 58 || len(s.Points) < 3 {
		t.Errorf("峰值 / 采样: %d %d", s.PeakThr, len(s.Points))
	}
	// 采样点封顶：点数不会超过 200，间隔翻倍
	for i := 0; i < 1000; i++ {
		now = now.Add(5 * time.Second)
		k.Progress(engine.Progress{Scanned: int64(i), BatchSize: 500})
	}
	if n := len(k.snapshot().Points); n > 200 {
		t.Errorf("采样点过多: %d", n)
	}
	// 事件环形缓冲与增量
	for i := 0; i < ringCap+50; i++ {
		k.Event(engine.Event{Kind: "log", Msg: "m"})
	}
	evs, last := k.eventsAfter(0, 0)
	if len(evs) != ringCap || last != uint(ringCap+51) {
		t.Errorf("环形缓冲: %d %d", len(evs), last)
	}
	if more, _ := k.eventsAfter(last, 0); len(more) != 0 {
		t.Error("增量应为空")
	}
	if p := k.takePending(); len(p) == 0 {
		t.Error("应有待落库事件")
	}
	k.requeue([]sinkEvent{{Seq: 1}})
	if p := k.takePending(); len(p) != 1 {
		t.Error("requeue")
	}
}
