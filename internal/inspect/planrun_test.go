// @author: kevinkelly
// @license: Apache Licence
// @file: planrun_test.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/settings"
)

func (e *env) savePlan(t *testing.T, p Plan) *Plan {
	t.Helper()
	saved, err := e.svc.SavePlan(admin, all, 0, p)
	if err != nil {
		t.Fatalf("保存计划失败：%v", err)
	}
	return saved
}

// due 让计划“到点”：把下次运行时间设为 at。
func (e *env) due(id uint, at string) {
	e.repo.mu.Lock()
	defer e.repo.mu.Unlock()
	t := ts(at)
	e.repo.plans[id].NextRunAt = &t
}

func (e *env) planNext(id uint) string {
	e.repo.mu.Lock()
	defer e.repo.mu.Unlock()
	if n := e.repo.plans[id].NextRunAt; n != nil {
		return n.Format("2006-01-02 15:04")
	}
	return ""
}

func (e *env) runOf(t *testing.T, id uint) *Run {
	t.Helper()
	r, _ := e.repo.GetRun(id)
	if r == nil {
		t.Fatalf("没有运行记录 %d", id)
	}
	return r
}

func TestSavePlan(t *testing.T) {
	e := newEnv()
	p := e.savePlan(t, basePlan())
	if p.ID == 0 || p.NextRunAt == nil || p.NextRunAt.Format("2006-01-02 15:04") != "2026-10-07 02:00" || p.CreatedBy != "root" {
		t.Fatalf("保存结果：%+v", p)
	}
	if acts := e.repo.auditActions(); len(acts) != 1 || acts[0] != "性能巡检·新建计划" {
		t.Errorf("审计：%v", acts)
	}
	// 同名
	_, err := e.svc.SavePlan(admin, all, 0, basePlan())
	asErr(t, err, 409)
	// 修改自己不算同名；停用后没有下次运行时间
	edit := basePlan()
	edit.Enabled = false
	edit.RetentionDays = 30
	saved, err := e.svc.SavePlan(admin, all, p.ID, edit)
	if err != nil || saved.NextRunAt != nil || saved.CreatedBy != "root" {
		t.Fatalf("%+v %v", saved, err)
	}
	if got, _ := e.repo.GetPlan(p.ID); got.RetentionDays != 30 || got.Enabled {
		t.Errorf("没存上：%+v", got)
	}
	if acts := e.repo.auditActions(); acts[len(acts)-1] != "性能巡检·修改计划" {
		t.Errorf("审计：%v", acts)
	}
	// 不存在的计划
	_, err = e.svc.SavePlan(admin, all, 999, basePlan())
	asErr(t, err, 404)
	// 校验失败
	bad := basePlan()
	bad.Name = "x2"
	bad.Concurrency = 0
	_, err = e.svc.SavePlan(admin, all, 0, bad)
	asErr(t, err, 400)
}

func TestSavePlanInstanceChecks(t *testing.T) {
	e := newEnv()
	p := basePlan()
	p.InstanceIDs = []uint{99}
	_, err := e.svc.SavePlan(admin, all, 0, p)
	asErr(t, err, 400)
	p.InstanceIDs = []uint{4} // redis
	_, err = e.svc.SavePlan(admin, all, 0, p)
	if e := asErr(t, err, 400); !strings.Contains(e.Message, "暂不支持巡检") {
		t.Errorf("%v", e)
	}
	// 范围外的实例不能选
	p.InstanceIDs = []uint{1, 2}
	_, err = e.svc.SavePlan(admin, Scope{IDs: map[uint]bool{1: true}}, 0, p)
	if e := asErr(t, err, 403); !strings.Contains(e.Message, "prod-mysql-02") {
		t.Errorf("%v", e)
	}
	// 新建时 2 在范围外；管理员建好之后，范围只含 1 的用户编辑，2 不会被移除
	saved := e.savePlan(t, p)
	edit := basePlan()
	edit.InstanceIDs = []uint{1, 3}
	_, err = e.svc.SavePlan(admin, Scope{IDs: map[uint]bool{1: true, 3: true}}, saved.ID, edit)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.repo.GetPlan(saved.ID)
	if len(got.InstanceIDs) != 3 || got.InstanceIDs[1] != 2 {
		t.Errorf("看不到的实例应保留：%v", got.InstanceIDs)
	}
	// 已有的、范围外的实例，编辑提交里带着也不报错
	edit.InstanceIDs = []uint{1, 2}
	if _, err = e.svc.SavePlan(admin, Scope{IDs: map[uint]bool{1: true}}, saved.ID, edit); err != nil {
		t.Errorf("已在计划里的范围外实例不应报错：%v", err)
	}
}

func TestSavePlanNotifyChecks(t *testing.T) {
	e := newEnv()
	p := basePlan()
	p.NotifyMode, p.NotifyChannels = NotifyAlert, []uint{99}
	_, err := e.svc.SavePlan(admin, all, 0, p)
	if e := asErr(t, err, 400); !strings.Contains(e.Message, "通知渠道 #99") {
		t.Errorf("%v", e)
	}
	p.NotifyChannels = []uint{7, 8}
	saved := e.savePlan(t, p)
	if len(saved.NotifyChannels) != 2 {
		t.Errorf("%+v", saved)
	}
	e.svc.notifier = nil
	p.Name = "无通知器"
	_, err = e.svc.SavePlan(admin, all, 0, p)
	if e := asErr(t, err, 400); !strings.Contains(e.Message, "通知发送器未启用") {
		t.Errorf("%v", e)
	}
}

func TestPlanTablesMissing(t *testing.T) {
	e := newEnv()
	e.repo.noTables = true
	_, err := e.svc.SavePlan(admin, all, 0, basePlan())
	if e := asErr(t, err, 409); e.Code != "table_missing" || !strings.Contains(e.Message, "013_inspect.sql") {
		t.Errorf("%v", e)
	}
	if _, err := e.svc.ListPlans(all); err == nil {
		t.Error("没建表应提示")
	}
	if n := e.svc.RunDue(); n != 0 {
		t.Error("没建表时调度器什么都不做")
	}
	if e.svc.PlansReady() {
		t.Error("PlansReady")
	}
	if got := e.svc.Cleanup(); got != 0 {
		t.Error("清理不应报错")
	}
}

func TestSetEnabledAndDelete(t *testing.T) {
	e := newEnv()
	p := e.savePlan(t, basePlan())
	if err := e.svc.SetPlanEnabled(admin, p.ID, false); err != nil || e.planNext(p.ID) != "" {
		t.Fatalf("停用：%v next=%q", err, e.planNext(p.ID))
	}
	e.clk.Add(24 * time.Hour)
	if err := e.svc.SetPlanEnabled(admin, p.ID, true); err != nil || e.planNext(p.ID) != "2026-10-08 02:00" {
		t.Fatalf("启用应重新计算：%v next=%q", err, e.planNext(p.ID))
	}
	asErr(t, e.svc.SetPlanEnabled(admin, 99, true), 404)
	if err := e.svc.DeletePlan(admin, p.ID); err != nil {
		t.Fatal(err)
	}
	asErr(t, e.svc.DeletePlan(admin, p.ID), 404)
	want := []string{"性能巡检·新建计划", "性能巡检·停用计划", "性能巡检·启用计划", "性能巡检·删除计划"}
	if got := e.repo.auditActions(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("审计：%v", got)
	}
}

func TestPreviewSchedule(t *testing.T) {
	e := newEnv()
	times, desc, err := e.svc.PreviewSchedule(Plan{Kind: KindCron, Cron: "0 */6 * * *"}, 3)
	if err != nil || len(times) != 3 || desc != "cron 0 */6 * * *" || times[0].Format("15:04") != "12:00" {
		t.Errorf("%v %q %v", times, desc, err)
	}
	if _, _, err := e.svc.PreviewSchedule(Plan{Kind: KindCron, Cron: "* * * * *"}, 3); err == nil {
		t.Error("预览也要校验")
	}
	if _, _, err := e.svc.PreviewSchedule(Plan{Kind: KindWeekly, Time: "02:00"}, 3); err == nil {
		t.Error("每周没选星期")
	}
}

func TestResolveInstances(t *testing.T) {
	e := newEnv()
	e.repo.tags["mysql_inspection"] = []uint{1, 2, 4, 99}
	p := basePlan()
	p.InstanceIDs, p.TagCodes = []uint{3, 1}, []string{"mysql_inspection"}
	list, err := e.svc.resolveInstances(&p)
	if err != nil || len(list) != 3 || list[0].InstanceName != "mssql-01" || list[1].InstanceName != "prod-mysql-01" || list[2].InstanceName != "prod-mysql-02" {
		t.Errorf("去重、去掉不支持和不存在的、按名称排序：%+v %v", list, err)
	}
}

func TestRunDueRunsPlanOnce(t *testing.T) {
	e := newEnv()
	p := basePlan()
	p.InstanceIDs = []uint{1, 3}
	p.StaggerSeconds = 0
	saved := e.savePlan(t, p)
	e.due(saved.ID, "2026-10-06 08:00:00")
	e.clk.Add(30 * time.Second)

	if n := e.svc.RunDue(); n != 1 {
		t.Fatalf("应启动 1 个计划：%d", n)
	}
	e.waitIdle()
	got, _ := e.repo.GetPlan(saved.ID)
	if got.NextRunAt == nil || got.NextRunAt.Format("2006-01-02 15:04") != "2026-10-07 02:00" || got.LastRunID == 0 || got.LastRunAt == nil {
		t.Errorf("下次运行应顺延到明天 02:00，并记录上次运行：%+v", got)
	}
	run := e.runOf(t, got.LastRunID)
	if run.Status != RunFinish || run.Total != 2 || run.OK != 2 || run.Failed != 0 || run.Trigger != TriggerSchedule || run.Operator != "系统" || run.FinishedAt == nil || run.PlanName != p.Name {
		t.Fatalf("运行记录：%+v", run)
	}
	if run.Alert == 0 {
		t.Errorf("告警合计应汇总各实例：%+v", run)
	}
	items, _ := e.repo.RunItems(run.ID)
	if len(items) != 2 || items[0].InstanceName != "mssql-01" || items[0].Status != ItemOK || items[0].ReportID == 0 || items[0].Kind != KindMSSQL {
		t.Errorf("运行明细：%+v", items)
	}
	for _, kr := range []ReportRef{{KindMySQL, 1}, {KindMSSQL, 1}} {
		r := e.repo.report(kr.Kind, kr.ID)
		if r.Status != StatusFinish || r.Trigger != TriggerSchedule || r.Operator != "系统" {
			t.Errorf("定时产生的报告：%+v", r.ReportSummary)
		}
		if r.PlanName != p.Name || r.PlanID != saved.ID {
			t.Errorf("报告应关联到计划：%+v", r.ReportSummary)
		}
	}
	// 同一时刻再检查一遍：不会重复触发
	if n := e.svc.RunDue(); n != 0 {
		t.Errorf("没到点不应再启动：%d", n)
	}
	acts := e.repo.auditActions()
	if acts[len(acts)-1] != "性能巡检·定时运行计划" || e.repo.audits[len(e.repo.audits)-1].UserName != "系统" {
		t.Errorf("审计：%v", acts)
	}
	// 按计划筛选报告
	list, total, _ := e.svc.ListReports(all, ReportQuery{PlanID: saved.ID})
	if total != 2 || list[0].PlanName != p.Name {
		t.Errorf("按计划筛选：%d %+v", total, list)
	}
	if _, n, _ := e.svc.ListReports(all, ReportQuery{RunID: run.ID}); n != 2 {
		t.Errorf("按运行筛选：%d", n)
	}
	if _, n, _ := e.svc.ListReports(all, ReportQuery{RunID: 999}); n != 0 {
		t.Errorf("按运行筛选：%d", n)
	}
}

// 多个 dbtool 实例同时运行时，同一个计划同一时刻只会被一个实例领取。
func TestRunDueNoDoubleTriggerAcrossProcesses(t *testing.T) {
	e := newEnv()
	var svcs []*Service
	svcs = append(svcs, e.svc)
	for i := 0; i < 3; i++ {
		svcs = append(svcs, NewService(e.repo, Options{Now: e.clk.Now, Open: func(ctx context.Context, dbType string, c engine.Conn) (Target, error) {
			return mysqlHappyTarget(), nil
		},
			Sleep: func(context.Context, time.Duration) {}}))
	}
	p := basePlan()
	p.InstanceIDs = []uint{1, 2}
	saved := e.savePlan(t, p)
	e.due(saved.ID, "2026-10-06 08:00:00")
	e.clk.Add(time.Second)

	var wg sync.WaitGroup
	var started atomic.Int32
	for _, s := range svcs {
		wg.Add(1)
		go func(s *Service) { defer wg.Done(); started.Add(int32(s.RunDue())) }(s)
	}
	wg.Wait()
	for _, s := range svcs {
		s.wg.Wait()
	}
	if started.Load() != 1 {
		t.Fatalf("4 个进程同时调度，应只有 1 个领到：%d", started.Load())
	}
	if len(e.repo.runs) != 1 || len(e.repo.reports) != 2 {
		t.Errorf("runs=%d reports=%d", len(e.repo.runs), len(e.repo.reports))
	}
}

func TestRunDueSkipsMissedOccurrence(t *testing.T) {
	e := newEnv()
	saved := e.savePlan(t, basePlan())
	e.due(saved.ID, "2026-10-06 02:00:00") // 现在 08:00：服务停了 6 小时
	if n := e.svc.RunDue(); n != 0 {
		t.Fatalf("错过的不补跑：%d", n)
	}
	e.waitIdle()
	if len(e.repo.reports) != 0 {
		t.Error("不应产生报告")
	}
	runs, _ := e.svc.ListRuns(saved.ID, 10)
	if len(runs) != 1 || runs[0].Status != RunSkipped || !strings.Contains(runs[0].Note, "错过计划时间 2026-10-06 02:00") {
		t.Errorf("应记录一次跳过的运行：%+v", runs)
	}
	if e.planNext(saved.ID) != "2026-10-07 02:00" {
		t.Errorf("下次运行应是明天：%s", e.planNext(saved.ID))
	}
	// 晚了 10 分钟（宽限内）照常运行
	e.due(saved.ID, "2026-10-06 07:50:00")
	if n := e.svc.RunDue(); n != 1 {
		t.Errorf("宽限内应运行：%d", n)
	}
	e.waitIdle()
}

func TestRunDueSkipsWhenPreviousRunStillGoing(t *testing.T) {
	e := newEnv()
	block := make(chan struct{})
	started := make(chan struct{})
	e.ft = func(model.Instance) *fakeTarget {
		ft := mysqlHappyTarget()
		ft.block, ft.started = block, started
		return ft
	}
	p := basePlan()
	p.InstanceIDs = []uint{1}
	saved := e.savePlan(t, p)
	e.due(saved.ID, "2026-10-06 08:00:00")
	if e.svc.RunDue() != 1 {
		t.Fatal("第一次应启动")
	}
	<-started
	// 上一次还没跑完，下一个计划时间又到了
	e.clk.Add(time.Minute)
	e.due(saved.ID, "2026-10-06 08:01:00")
	if n := e.svc.RunDue(); n != 0 {
		t.Errorf("同一计划同一时刻只跑一份：%d", n)
	}
	runs, _ := e.svc.ListRuns(saved.ID, 10)
	if len(runs) != 2 || runs[0].Status != RunSkipped || !strings.Contains(runs[0].Note, "该计划正在运行") {
		t.Errorf("应记录跳过：%+v", runs)
	}
	// 手动“立即运行”同样被拒绝
	_, err := e.svc.RunPlanNow(admin, saved.ID)
	asErr(t, err, 409)
	close(block)
	e.waitIdle()
	if run := e.runOf(t, 1); run.Status != RunFinish {
		t.Errorf("第一次应正常结束：%+v", run)
	}
}

func TestStaleRunsAreFailed(t *testing.T) {
	e := newEnv()
	saved := e.savePlan(t, basePlan())
	id, _ := e.repo.CreateRun(&Run{PlanID: saved.ID, PlanName: "x", Status: RunRunning, StartedAt: e.clk.Now(), Heartbeat: e.clk.Now()})
	e.clk.Add(10 * time.Minute) // 心跳停了 10 分钟：进程已经没了
	e.svc.RunDue()
	if r := e.runOf(t, id); r.Status != RunFailed || !strings.Contains(r.Note, "中断") || r.FinishedAt == nil {
		t.Errorf("%+v", r)
	}
}

func TestPlanStaggerAndConcurrency(t *testing.T) {
	e := newEnv()
	e.repo.insts = append(e.repo.insts, inst(5, "prod-mysql-03", "mysql"), inst(6, "prod-mysql-04", "mysql"))
	var running, peak atomic.Int32
	e.ft = func(in model.Instance) *fakeTarget {
		ft := mysqlHappyTarget()
		var once atomic.Bool
		ft.hook = func(n int) {
			if once.CompareAndSwap(false, true) {
				cur := running.Add(1)
				for {
					p := peak.Load()
					if cur <= p || peak.CompareAndSwap(p, cur) {
						break
					}
				}
				time.Sleep(30 * time.Millisecond)
				running.Add(-1)
			}
		}
		return ft
	}
	p := basePlan()
	p.InstanceIDs = []uint{1, 2, 5, 6}
	p.Concurrency, p.StaggerSeconds = 2, 45
	saved := e.savePlan(t, p)
	e.due(saved.ID, "2026-10-06 08:00:00")
	e.svc.RunDue()
	e.waitIdle()
	if peak.Load() > 2 || peak.Load() < 1 {
		t.Errorf("并发上限 2，峰值 %d", peak.Load())
	}
	stag := 0
	for _, d := range e.sleepList() {
		if d == 45*time.Second {
			stag++
		}
	}
	if stag != 3 { // 4 个实例，每启动一个（除第一个）前等 45 秒
		t.Errorf("错峰等待 %d 次：%v", stag, e.sleepList())
	}
	if run := e.runOf(t, 1); run.OK != 4 || run.Status != RunFinish {
		t.Errorf("%+v", run)
	}
}

func TestPlanSkipBusy(t *testing.T) {
	e := newEnv()
	e.repo.CreateReport(KindMySQL, 1, TriggerManual, "bob", 0, e.clk.Now(), time.Hour) // 实例 1 上有人手动巡检中
	p := basePlan()
	p.InstanceIDs = []uint{1, 2}
	saved := e.savePlan(t, p)
	e.due(saved.ID, "2026-10-06 08:00:00")
	e.svc.RunDue()
	e.waitIdle()
	run := e.runOf(t, 1)
	if run.OK != 1 || run.Skipped != 1 || run.Failed != 0 || run.Status != RunFinish || !strings.Contains(run.Note, "prod-mysql-01 已有进行中的巡检") {
		t.Errorf("%+v", run)
	}
	items, _ := e.repo.RunItems(run.ID)
	if items[0].Status != ItemSkipped || items[0].ReportID != 0 {
		t.Errorf("%+v", items)
	}
}

func TestPlanWaitsForBusyWhenNotSkipping(t *testing.T) {
	e := newEnv()
	busyID, _, _ := e.repo.CreateReport(KindMySQL, 1, TriggerManual, "bob", 0, e.clk.Now(), time.Hour)
	var polls atomic.Int32
	e.svc.sleep = func(ctx context.Context, d time.Duration) {
		e.clk.Add(d)
		if polls.Add(1) == 3 { // 等了三轮之后，别人的巡检结束了
			e.repo.FinishReport(KindMySQL, busyID, Finish{Status: StatusFinish, At: e.clk.Now()})
		}
	}
	p := basePlan()
	p.InstanceIDs, p.SkipBusy, p.StaggerSeconds = []uint{1}, false, 0
	saved := e.savePlan(t, p)
	e.due(saved.ID, "2026-10-06 08:00:00")
	e.svc.RunDue()
	e.waitIdle()
	run := e.runOf(t, 1)
	if run.OK != 1 || run.Skipped != 0 || polls.Load() < 3 {
		t.Errorf("应等对方结束后再跑：%+v polls=%d", run, polls.Load())
	}
	// 一直不结束：等到超时后放弃
	e2 := newEnv()
	e2.repo.CreateReport(KindMySQL, 1, TriggerManual, "bob", 0, e2.clk.Now(), 100*time.Hour)
	p2 := basePlan()
	p2.InstanceIDs, p2.SkipBusy, p2.TimeoutMinutes, p2.StaggerSeconds = []uint{1}, false, 1, 0
	s2 := e2.savePlan(t, p2)
	e2.due(s2.ID, "2026-10-06 08:00:00")
	e2.svc.RunDue()
	e2.waitIdle()
	if run := e2.runOf(t, 1); run.Skipped != 1 || !strings.Contains(run.Note, "等待已有巡检结束超时") {
		t.Errorf("%+v", run)
	}
}

func TestPlanRetryOnce(t *testing.T) {
	for _, retry := range []bool{false, true} {
		e := newEnv()
		e.openErr[1] = errors.New("连接超时")
		e.svc.sleep = func(ctx context.Context, d time.Duration) {
			e.clk.Add(d)
			if d == time.Minute { // 重试等待之后，网络恢复
				e.mu.Lock()
				delete(e.openErr, 1)
				e.mu.Unlock()
			}
		}
		p := basePlan()
		p.InstanceIDs, p.RetryOnce, p.StaggerSeconds = []uint{1, 2}, retry, 0
		saved := e.savePlan(t, p)
		e.due(saved.ID, "2026-10-06 08:00:00")
		e.svc.RunDue()
		e.waitIdle()
		run := e.runOf(t, 1)
		failedReports := 0
		for _, r := range e.repo.reports {
			if r.Status == StatusFailed {
				failedReports++
			}
		}
		if retry {
			if run.OK != 2 || run.Failed != 0 || failedReports != 1 || len(e.repo.reports) != 3 {
				t.Errorf("重试：%+v 失败报告=%d 报告=%d", run, failedReports, len(e.repo.reports))
			}
		} else {
			if run.OK != 1 || run.Failed != 1 || !strings.Contains(run.Note, "失败：prod-mysql-01 连接超时") || run.Status != RunFinish {
				t.Errorf("不重试：%+v", run)
			}
		}
	}
	// 重试后仍失败
	e := newEnv()
	e.openErr[1] = errors.New("连接超时")
	p := basePlan()
	p.InstanceIDs, p.RetryOnce, p.StaggerSeconds = []uint{1}, true, 0
	saved := e.savePlan(t, p)
	e.due(saved.ID, "2026-10-06 08:00:00")
	e.svc.RunDue()
	e.waitIdle()
	run := e.runOf(t, 1)
	if run.Failed != 1 || run.Status != RunFailed || !strings.Contains(run.Note, "重试后仍失败") {
		t.Errorf("全部失败应整体标失败：%+v", run)
	}
}

func TestNotifyRunModes(t *testing.T) {
	mk := func(mode string) (Plan, *Run, []planResult) {
		p := basePlan()
		p.NotifyMode, p.NotifyChannels, p.CreatedBy = mode, []uint{7, 8}, "alice"
		run := &Run{Total: 3, OK: 2, Failed: 1, Alert: 4, Attention: 9}
		res := []planResult{
			{item: RunItem{InstanceName: "prod-mysql-01", Kind: KindMySQL, ReportID: 11, Status: ItemOK, Alert: 3, Attention: 4}},
			{item: RunItem{InstanceName: "mssql-01", Kind: KindMSSQL, ReportID: 5, Status: ItemOK, Alert: 1, Attention: 5}},
			{item: RunItem{InstanceName: "prod-mysql-07", Status: ItemFailed, Message: "连接超时"}},
		}
		return p, run, res
	}
	e := newEnv()
	p, run, res := mk(NotifyAlert)
	if msg := e.svc.notifyRun(context.Background(), p, run, res); msg != "" {
		t.Errorf("发送成功不应有说明：%q", msg)
	}
	if len(e.nt.sent) != 1 {
		t.Fatalf("应发 1 条：%d", len(e.nt.sent))
	}
	ev := e.nt.sent[0]
	if ev.Name != notify.EventException || ev.Applicant != "alice" || !strings.Contains(ev.Title, "生产夜间巡检") || !strings.Contains(ev.Title, "发现问题") {
		t.Errorf("事件：%+v", ev)
	}
	for _, s := range []string{"成功 2 / 失败 1 / 跳过 0", "告警合计 4 · 关注合计 9", "1. prod-mysql-01 告警 3 关注 4 https://dba.example.com/inspection/mysql-11",
		"2. mssql-01 告警 1 关注 5 https://dba.example.com/inspection/mssql-5", "prod-mysql-07：连接超时", "https://dba.example.com/inspection"} {
		if !strings.Contains(ev.Content, s) {
			t.Errorf("通知内容缺少 %q：\n%s", s, ev.Content)
		}
	}
	if ids := e.nt.sentIDs[0]; len(ids) != 2 || ids[0] != 7 {
		t.Errorf("渠道：%v", ids)
	}

	// 仅告警 / 失败时通知：一切正常就不发
	e = newEnv()
	p, run, res = mk(NotifyAlert)
	run.Alert, run.Failed = 0, 0
	e.svc.notifyRun(context.Background(), p, run, res[:2])
	if len(e.nt.sent) != 0 {
		t.Error("没有告警也没有失败时 alert 模式不应通知")
	}
	// 每次跑完都通知；没问题时事件是“执行完成”
	p.NotifyMode = NotifyAlways
	e.svc.notifyRun(context.Background(), p, run, res[:2])
	if len(e.nt.sent) != 1 || e.nt.sent[0].Name != notify.EventFinished || !strings.Contains(e.nt.sent[0].Title, "已完成") {
		t.Errorf("always：%+v", e.nt.sent)
	}
	// 不通知
	p.NotifyMode = NotifyNone
	e.svc.notifyRun(context.Background(), p, run, res)
	if len(e.nt.sent) != 1 {
		t.Error("none 不应通知")
	}
	// 发送失败只记进运行说明，不影响运行结果
	e = newEnv()
	e.nt.err = errors.New("webhook 返回 500")
	p, run, res = mk(NotifyAlways)
	if msg := e.svc.notifyRun(context.Background(), p, run, res); !strings.Contains(msg, "通知发送失败：webhook 返回 500") {
		t.Errorf("%q", msg)
	}
	// 渠道已删除或停用：一条都没发出去
	e = newEnv()
	e.nt.err = nil
	e.svc.notifier = zeroSender{}
	if msg := e.svc.notifyRun(context.Background(), p, run, res); !strings.Contains(msg, "没有发出") {
		t.Errorf("%q", msg)
	}
	// 没有 BaseURL 时不拼链接
	e = newEnv()
	e.svc.baseURL = ""
	e.svc.notifyRun(context.Background(), p, run, res)
	if strings.Contains(e.nt.sent[0].Content, "http") {
		t.Errorf("没有对外地址时不应出现链接：%s", e.nt.sent[0].Content)
	}
}

type zeroSender struct{}

func (zeroSender) Channels() ([]ChannelOption, error) { return nil, nil }
func (zeroSender) Send(context.Context, []uint, notify.Event) (int, error) {
	return 0, nil
}

func TestPlanRunSendsNotification(t *testing.T) {
	e := newEnv()
	p := basePlan()
	p.InstanceIDs, p.NotifyMode, p.NotifyChannels, p.StaggerSeconds = []uint{1}, NotifyAlert, []uint{7}, 0
	saved := e.savePlan(t, p)
	e.due(saved.ID, "2026-10-06 08:00:00")
	e.svc.RunDue()
	e.waitIdle()
	if len(e.nt.sent) != 1 || !strings.Contains(e.nt.sent[0].Content, "/inspection/mysql-1") {
		t.Errorf("跑完应通知：%+v", e.nt.sent)
	}
	if e.nt.sent[0].Applicant != "root" {
		t.Errorf("@提交人应指计划创建人：%q", e.nt.sent[0].Applicant)
	}
}

func TestRunPlanNow(t *testing.T) {
	e := newEnv()
	p := basePlan()
	p.InstanceIDs, p.Enabled, p.StaggerSeconds = []uint{1, 3}, false, 0 // 停用的计划也能手动运行
	saved := e.savePlan(t, p)
	bob := Actor{ID: 2, Username: "bob", Display: "鲍勃"}
	runID, err := e.svc.RunPlanNow(bob, saved.ID)
	if err != nil || runID == 0 {
		t.Fatal(runID, err)
	}
	e.waitIdle()
	run := e.runOf(t, runID)
	if run.Trigger != TriggerManual || run.Operator != "bob" || run.OK != 2 {
		t.Errorf("%+v", run)
	}
	if r := e.repo.report(KindMySQL, 1); r.Trigger != TriggerManual || r.Operator != "bob" {
		t.Errorf("手动运行计划产生的报告记手动：%+v", r.ReportSummary)
	}
	if e.planNext(saved.ID) != "" {
		t.Error("手动运行不改下次运行时间（停用的仍是空）")
	}
	acts := e.repo.auditActions()
	if acts[len(acts)-1] != "性能巡检·立即运行计划" || e.repo.audits[len(e.repo.audits)-1].UserName != "bob" {
		t.Errorf("审计：%v", acts)
	}
	asErr(t, func() error { _, err := e.svc.RunPlanNow(bob, 99); return err }(), 404)
	// 计划里的实例都没了
	e.repo.insts = e.repo.insts[3:]
	_, err = e.svc.RunPlanNow(bob, saved.ID)
	if e := asErr(t, err, 400); !strings.Contains(e.Message, "没有可巡检的实例") {
		t.Errorf("%v", e)
	}
}

func TestListPlansViews(t *testing.T) {
	e := newEnv()
	e.repo.tags["mssql_inspection"] = []uint{3, 1}
	p := basePlan()
	p.InstanceIDs, p.TagCodes, p.NotifyMode, p.NotifyChannels = []uint{1, 2}, []string{"mssql_inspection"}, NotifyAlert, []uint{7, 8}
	saved := e.savePlan(t, p)
	e.nt.channels = e.nt.channels[:1] // 渠道 8 之后被删除了
	e.repo.CreateRun(&Run{PlanID: saved.ID, Status: RunFinish, OK: 2, StartedAt: e.clk.Now()})
	e.repo.MarkPlanRun(saved.ID, 1, e.clk.Now())
	views, err := e.svc.ListPlans(Scope{IDs: map[uint]bool{1: true}})
	if err != nil || len(views) != 1 {
		t.Fatal(views, err)
	}
	v := views[0]
	if v.InstanceTotal != 3 || v.TagMatched != 2 || v.Hidden != 1 || len(v.InstanceNames) != 1 || v.InstanceNames[0] != "prod-mysql-01" || len(v.Plan.InstanceIDs) != 1 {
		t.Errorf("实例统计（只显示可见的）：%+v", v)
	}
	if v.Describe != "每天 02:00" || v.LastRun == nil || v.LastRun.OK != 2 || v.Running {
		t.Errorf("%+v", v)
	}
	if len(v.ChannelNames) != 2 || v.ChannelNames[0] != "研发群" || !strings.Contains(v.ChannelNames[1], "已删除") {
		t.Errorf("渠道名：%v", v.ChannelNames)
	}
	if v.Weekdays == nil || v.Databases == nil || v.TagCodes == nil || v.NotifyChannels == nil {
		t.Error("列表字段不应为 null（前端直接 .map / .join）")
	}
	// 有运行中的记录
	e.repo.CreateRun(&Run{PlanID: saved.ID, Status: RunRunning, StartedAt: e.clk.Now(), Heartbeat: e.clk.Now()})
	views, _ = e.svc.ListPlans(all)
	if !views[0].Running {
		t.Error("应显示运行中")
	}
	_, err = e.svc.ListRuns(999, 10)
	asErr(t, err, 404)
}

func TestCleanupByRetention(t *testing.T) {
	e := newEnv()
	p := basePlan()
	p.InstanceIDs, p.RetentionDays = []uint{1}, 7
	saved := e.savePlan(t, p)
	old := seedReport(e.repo, KindMySQL, 1, "prod-mysql-01", "mysql", StatusFinish, TriggerSchedule, 0, 0, "2026-09-20 02:00:00")     // 计划产生、已过期
	fresh := seedReport(e.repo, KindMySQL, 1, "prod-mysql-01", "mysql", StatusFinish, TriggerSchedule, 0, 0, "2026-10-05 02:00:00")   // 计划产生、未过期
	manualOld := seedReport(e.repo, KindMySQL, 2, "prod-mysql-02", "mysql", StatusFinish, TriggerManual, 0, 0, "2026-01-01 02:00:00") // 不属于任何计划
	runID, _ := e.repo.CreateRun(&Run{PlanID: saved.ID, Status: RunFinish, StartedAt: ts("2026-09-20 02:00:00")})
	e.repo.AddRunItem(&RunItem{RunID: runID, Kind: KindMySQL, ReportID: old})
	e.repo.AddRunItem(&RunItem{RunID: runID, Kind: KindMySQL, ReportID: fresh})
	if n := e.svc.Cleanup(); n != 1 {
		t.Fatalf("应只删计划里过期的 1 份：%d", n)
	}
	if e.repo.report(KindMySQL, old) != nil || e.repo.report(KindMySQL, fresh) == nil || e.repo.report(KindMySQL, manualOld) == nil {
		t.Error("清理对象不对")
	}
	// 运行记录至少保留 30 天，即使报告保留期更短
	if _, err := e.repo.GetRun(runID); err != nil || e.repo.runs[runID] == nil {
		t.Error("运行记录不应在 30 天内被清理")
	}
	e.clk.Add(40 * 24 * time.Hour)
	e.svc.Cleanup()
	if e.repo.runs[runID] != nil {
		t.Error("超过 30 天且超过保留期的运行记录应被清理")
	}
}

func TestCleanupLegacyRetention(t *testing.T) {
	e := newEnv()
	e.savePlan(t, basePlan())
	a := seedReport(e.repo, KindMySQL, 1, "n", "mysql", StatusFinish, TriggerManual, 0, 0, "2026-06-01 02:00:00")
	b := seedReport(e.repo, KindMSSQL, 3, "m", "mssql", StatusFinish, TriggerSchedule, 0, 0, "2026-06-01 02:00:00")
	running := seedReport(e.repo, KindMySQL, 2, "n2", "mysql", StatusRunning, TriggerManual, 0, 0, "2026-06-01 02:00:00")
	if n := e.svc.Cleanup(); n != 0 {
		t.Fatalf("没配置保留天数时不删不属于计划的报告：%d", n)
	}
	e.repo.config[settings.KeyInspectMySQLRetentionDays] = "30"
	if n := e.svc.Cleanup(); n != 1 {
		t.Fatalf("只清理 MySQL 一类：%d", n)
	}
	if e.repo.report(KindMySQL, a) != nil || e.repo.report(KindMSSQL, b) == nil || e.repo.report(KindMySQL, running) == nil {
		t.Error("清理对象不对（进行中的不删）")
	}
	e.repo.config[settings.KeyInspectMSSQLRetentionDays] = "abc"
	e.repo.config[settings.KeyInspectMySQLRetentionDays] = "0"
	if n := e.svc.Cleanup(); n != 0 {
		t.Errorf("非法或 0 表示不清理：%d", n)
	}
}

func TestWorkerRunsDuePlansAndWaitsOnShutdown(t *testing.T) {
	e := newEnv()
	e.svc.tick = 5 * time.Millisecond
	block := make(chan struct{})
	started := make(chan struct{})
	e.ft = func(model.Instance) *fakeTarget {
		ft := mysqlHappyTarget()
		ft.block, ft.started = block, started
		return ft
	}
	p := basePlan()
	p.InstanceIDs = []uint{1}
	saved := e.savePlan(t, p)
	e.due(saved.ID, "2026-10-06 08:00:00")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.svc.Worker(ctx); close(done) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("调度器没有启动到点的计划")
	}
	cancel() // 进程退出：停止调度，但要等在跑的巡检结束
	select {
	case <-done:
		t.Fatal("还有巡检在跑，Worker 不应返回")
	case <-time.After(100 * time.Millisecond):
	}
	close(block)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("巡检结束后 Worker 应返回")
	}
	if r := e.repo.report(KindMySQL, 1); r.Status != StatusFinish {
		t.Errorf("退出时在跑的巡检应跑完：%s", r.Status)
	}
}

func TestSchedulerIgnoresDisabledAndBrokenPlans(t *testing.T) {
	e := newEnv()
	off := basePlan()
	off.Enabled = false
	off.Name = "停用的"
	e.savePlan(t, off)
	if n := e.svc.RunDue(); n != 0 {
		t.Errorf("停用的计划不运行：%d", n)
	}
	// 被手工改坏的 cron：停用，不能让调度器反复报错
	bad := e.savePlan(t, func() Plan { p := basePlan(); p.Name = "坏的"; return p }())
	e.repo.mu.Lock()
	e.repo.plans[bad.ID].Kind, e.repo.plans[bad.ID].Cron = KindCron, "oops"
	t0 := ts("2026-10-06 07:00:00")
	e.repo.plans[bad.ID].NextRunAt = &t0
	e.repo.mu.Unlock()
	if n := e.svc.RunDue(); n != 0 {
		t.Errorf("%d", n)
	}
	if got, _ := e.repo.GetPlan(bad.ID); got.Enabled {
		t.Error("cron 无法解析的计划应被停用")
	}
	// 启用却没有下次运行时间：补上，不立即运行
	nilNext := e.savePlan(t, func() Plan { p := basePlan(); p.Name = "没下次"; return p }())
	e.repo.mu.Lock()
	e.repo.plans[nilNext.ID].NextRunAt = nil
	e.repo.mu.Unlock()
	if n := e.svc.RunDue(); n != 0 {
		t.Errorf("%d", n)
	}
	if e.planNext(nilNext.ID) != "2026-10-07 02:00" {
		t.Errorf("应补上下次运行时间：%q", e.planNext(nilNext.ID))
	}
}

func TestPlanAllInstancesGoneRecordsSkippedRun(t *testing.T) {
	e := newEnv()
	p := basePlan()
	p.InstanceIDs = []uint{1}
	saved := e.savePlan(t, p)
	e.repo.insts = nil
	e.due(saved.ID, "2026-10-06 08:00:00")
	if n := e.svc.RunDue(); n != 0 {
		t.Errorf("%d", n)
	}
	runs, _ := e.svc.ListRuns(saved.ID, 10)
	if len(runs) != 1 || runs[0].Status != RunSkipped || !strings.Contains(runs[0].Note, "没有可巡检的实例") {
		t.Errorf("应记录跳过的原因：%+v", runs)
	}
}

func TestNotifyLowScore(t *testing.T) {
	e := newEnv()
	p := basePlan()
	p.NotifyMode, p.NotifyChannels = NotifyAlert, []uint{7}
	score := func(n int) *int { return &n }
	run := &Run{Total: 2, OK: 2}
	good := []planResult{{item: RunItem{InstanceName: "a", Status: ItemOK}, score: score(90)}, {item: RunItem{InstanceName: "b", Status: ItemOK}, score: score(70)}}
	if e.svc.notifyRun(context.Background(), p, run, good); len(e.nt.sent) != 0 {
		t.Fatal("没有告警、没有失败、评分都不低于 70：alert 模式不发")
	}
	low := []planResult{{item: RunItem{InstanceName: "a", Status: ItemOK}, score: score(90)}, {item: RunItem{InstanceName: "b-mysql", Status: ItemOK}, score: score(55)}}
	e.svc.notifyRun(context.Background(), p, run, low)
	if len(e.nt.sent) != 1 || e.nt.sent[0].Name != notify.EventException || !strings.Contains(e.nt.sent[0].Content, "健康评分低于 70 的实例：b-mysql（55 分）") {
		t.Errorf("低分应点名并按异常事件发送：%+v", e.nt.sent)
	}
}
