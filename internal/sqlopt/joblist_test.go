// @author: kevinkelly
// @license: Apache Licence
// @file: joblist_test.go
// @time: 2026/10/28

package sqlopt

import (
	"strings"
	"testing"
	"time"
)

func TestListJobsVisibilityAndRetention(t *testing.T) {
	e := newEnv(t)
	va, err := e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select 1", FileName: `C:\fakepath\a\orders.sql`})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, e.svc, alice, va.ID)
	vb, _ := e.svc.StartAnalyze(ctx(), bob, AnalyzeReq{Text: "select 2"})
	waitJob(t, e.svc, bob, vb.ID)

	got := e.svc.ListJobs(alice)
	if len(got) != 1 || got[0].ID != va.ID || !got[0].Mine || got[0].Owner != "alice" {
		t.Fatalf("普通用户只能看到自己的任务：%+v", got)
	}
	if got[0].File != "orders.sql" || got[0].Format != FormatSQL || got[0].Total != 1 || got[0].Done != 1 {
		t.Fatalf("文件名应去掉路径：%+v", got[0])
	}
	if got[0].RemainSec <= 0 || got[0].RemainSec > int(jobTTL/time.Second) || got[0].ExpiresAt == "" || got[0].CanCancel {
		t.Fatalf("结束的任务应带剩余保留时间：%+v", got[0])
	}
	all := e.svc.ListJobs(root)
	if len(all) != 2 {
		t.Fatalf("超级用户可见所有人的任务：%+v", all)
	}
	if all[0].CreatedAt < all[1].CreatedAt {
		t.Fatalf("应按创建时间倒序：%+v", all)
	}
	for _, b := range all {
		if b.ID == vb.ID && b.Mine {
			t.Fatalf("别人的任务 Mine 应为 false：%+v", b)
		}
	}
	// 过期后不再出现（与任务本身的清理策略一致）
	e.svc.now = func() time.Time { return time.Now().Add(jobTTL + time.Minute) }
	if left := e.svc.ListJobs(root); len(left) != 0 {
		t.Fatalf("过期任务应从列表消失：%+v", left)
	}
}

func TestListJobsRunningHasNoCountdownAndSkipsOptimize(t *testing.T) {
	e := newEnv(t)
	e.configureTools()
	e.runner.block = make(chan struct{})
	v, err := e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select 1"})
	if err != nil {
		t.Fatal(err)
	}
	brief := e.svc.ListJobs(alice)
	if len(brief) != 1 || brief[0].RemainSec != -1 || !brief[0].CanCancel || brief[0].ExpiresAt != "" {
		t.Fatalf("进行中的任务不倒计时：%+v", brief)
	}
	// 优化类任务不进入“最近报告”
	j := e.svc.newJob(alice, KindOptimize, "优化")
	j.Status = StatusDone
	j.Finished = time.Now()
	if err := e.svc.jobs.add(j, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, b := range e.svc.ListJobs(alice) {
		if b.ID == j.ID {
			t.Fatalf("优化任务不应出现：%+v", b)
		}
	}
	close(e.runner.block)
	waitJob(t, e.svc, alice, v.ID)
}

func TestSummaryAvgScoreAndItemTables(t *testing.T) {
	e := newEnv(t)
	v, _ := e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select * from orders where phone = 1;\nupdate stock set qty = 1;"})
	done := waitJob(t, e.svc, alice, v.ID)
	s := done.Summary
	if s == nil || s.Scored != 2 || s.AvgScore <= 0 || s.AvgScore > 100 {
		t.Fatalf("应有平均评分：%+v", s)
	}
	if len(done.Items[0].Tables) == 0 || !strings.EqualFold(done.Items[0].Tables[0], "orders") || done.Items[0].FPID == "" || done.Items[0].Score == nil {
		t.Fatalf("条目应带涉及表/指纹ID/评分：%+v", done.Items[0])
	}
}

func TestJobsEndpoint(t *testing.T) {
	h := newHTTPEnv(t)
	h.grant("sqlanalyze.view", "sqlanalyze.run")
	bob := h.Login("bob")
	code, r := call(t, bob, "POST", base+"/analyze", map[string]any{"text": "select 1", "file_name": "x.sql"})
	if code != 200 {
		t.Fatalf("%d %s", code, r.Message)
	}
	code, r = call(t, bob, "GET", base+"/jobs", nil)
	if code != 200 || !strings.Contains(string(r.Data), `"file":"x.sql"`) || !strings.Contains(string(r.Data), `"keep_minutes":30`) {
		t.Fatalf("%d %s", code, r.Data)
	}
	if w := h.Anonymous("GET", base+"/jobs"); w.Code != 401 {
		t.Fatalf("未登录应 401：%d", w.Code)
	}
}
