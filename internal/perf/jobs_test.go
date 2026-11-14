// @author: kevinkelly
// @license: Apache Licence
// @file: jobs_test.go
// @time: 2026/10/28

package perf

import (
	"context"
	"testing"
	"time"

	"dbtool/internal/settings"
)

func newWorker(f *fx) *worker {
	return &worker{s: f.svc, memKeys: map[string]bool{}, warned: map[string]bool{}}
}

func TestWorkerDefaultsToNoJobs(t *testing.T) {
	f := newFx(t)
	col := &fakeCollector{digests: []Digest{f.digest(sqlOrderByUser, 1000, 4000)}}
	f.useCollector(col)
	f.repo.jobsOK = true
	w := newWorker(f)
	w.tick(context.Background())
	if len(col.reqs) != 0 || len(f.repo.issues) != 0 || len(f.repo.jobRuns) != 0 {
		t.Fatal("没有开启 perf_go_jobs 时，本进程的任务一项都不应执行（避免与 Django 定时任务重复采集）")
	}
	st := f.svc.JobsStatus()
	if len(st.Enabled) != 0 || !st.JobRun || st.Item != settings.KeyPerfJobs {
		t.Fatalf("%+v", st)
	}
}

func TestWorkerCollectAndSLA(t *testing.T) {
	f := newFx(t)
	col := &fakeCollector{digests: []Digest{f.digest(sqlOrderByUser, 1000, 4000)}}
	f.useCollector(col)
	f.repo.cfg[settings.KeyPerfJobs] = "collect,sla"
	w := newWorker(f)
	w.tick(context.Background())
	if len(col.reqs) != 1 || len(f.repo.issues) != 1 {
		t.Fatalf("应采集建单: %d %d", len(col.reqs), len(f.repo.issues))
	}
	// 同一个窗口没过完不会再采
	w.tick(context.Background())
	if len(col.reqs) != 1 {
		t.Fatal("没到下一个窗口不应再采")
	}
	// SLA 每 30 分钟一次
	f.n.reset()
	f.now = f.now.Add(5 * time.Hour)
	w.tick(context.Background())
	if f.n.count() == 0 {
		t.Fatal("应催办")
	}
	n := f.n.count()
	f.now = f.now.Add(10 * time.Minute)
	w.tick(context.Background())
	if f.n.count() != n {
		t.Fatal("30 分钟内不应再巡检")
	}
}

func TestWorkerVerifyOncePerDay(t *testing.T) {
	f := newFx(t)
	f.repo.cfg[settings.KeyPerfJobs] = "verify"
	f.repo.jobsOK = true
	id := f.fixedIssue()
	f.now = time.Date(2026, 10, 10, 9, 0, 0, 0, time.Local) // 还没到 10:05
	w := newWorker(f)
	w.tick(context.Background())
	if f.issue(id).Status != StFixed {
		t.Fatal("10:05 之前不跑")
	}
	f.now = time.Date(2026, 10, 10, 10, 6, 0, 0, time.Local)
	w.tick(context.Background())
	if f.issue(id).Status != StClosed {
		t.Fatalf("应验证: %+v", f.issue(id))
	}
	if !f.repo.jobRuns["verify|verify:2026-10-10"] {
		t.Fatalf("应记录运行: %v", f.repo.jobRuns)
	}
	// 另一个进程（或重启后）同一天不再跑
	id2 := f.newIssue()
	f.accept(id2)
	f.mustAct(f.dev(), id2, ActionInput{Action: ActFix, Remark: "ok"})
	f.now = f.now.AddDate(0, 0, 0).Add(5 * time.Hour)
	f.repo.issues[id2].FixedAt = at(f.now.AddDate(0, 0, -5))
	newWorker(f).tick(context.Background())
	if f.issue(id2).Status != StFixed {
		t.Fatal("同一天只执行一次")
	}
	// 第二天再跑
	f.repo.stats = map[string]*IssueStat{} // 修复后没有再出现
	f.now = f.now.AddDate(0, 0, 1)
	newWorker(f).tick(context.Background())
	if f.issue(id2).Status != StClosed {
		t.Fatal("第二天应再执行")
	}
}

func TestWorkerVerifyWithoutJobRunTableUsesMemory(t *testing.T) {
	f := newFx(t)
	f.repo.cfg[settings.KeyPerfJobs] = "verify"
	f.repo.jobsOK = false
	f.now = time.Date(2026, 10, 10, 11, 0, 0, 0, time.Local)
	w := newWorker(f)
	id := f.newIssue()
	f.repo.issues[id].LastSeen = at(f.now.AddDate(0, 0, -30))
	w.tick(context.Background())
	if f.issue(id).Status != StClosed {
		t.Fatal("没有记录表时本进程内按天去重仍应执行")
	}
	if !w.memKeys["verify:2026-10-10"] {
		t.Fatal("memKeys")
	}
}

func TestWorkerWeeklyReport(t *testing.T) {
	f := newFx(t)
	f.repo.cfg[settings.KeyPerfJobs] = "report"
	f.repo.jobsOK = false
	f.now = time.Date(2026, 10, 12, 9, 40, 0, 0, time.Local) // 周一 09:40
	w := newWorker(f)
	w.tick(context.Background())
	if f.n.count() != 0 || !w.warned[JobReport] {
		t.Fatal("没有运行记录表时周报不发，并提示")
	}
	f.repo.jobsOK = true
	w.tick(context.Background())
	if f.n.count() != 1 { // 没有任何团队数据：只发全局一条
		t.Fatalf("周报: %d", f.n.count())
	}
	w.tick(context.Background())
	newWorker(f).tick(context.Background())
	if f.n.count() != 1 || !f.repo.jobRuns["report|weekly:2026-W42"] {
		t.Fatalf("同一周只发一次: %d %v", f.n.count(), f.repo.jobRuns)
	}
	// 非周一 / 周一 09:35 之前不发
	f.n.reset()
	f.now = time.Date(2026, 10, 13, 10, 0, 0, 0, time.Local)
	newWorker(f).tick(context.Background())
	f.now = time.Date(2026, 10, 19, 9, 30, 0, 0, time.Local)
	newWorker(f).tick(context.Background())
	if f.n.count() != 0 {
		t.Fatal("时间未到不发")
	}
}

func TestRunWorkerStopsOnCancel(t *testing.T) {
	f := newFx(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.svc.RunWorker(ctx, 5*time.Millisecond); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后应返回")
	}
}
