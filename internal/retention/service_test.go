// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package retention

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/settings"
)

func enable(f *fakeRepo) { f.set(settings.KeyRetentionEnabled, "true") }

func TestDisabledByDefault(t *testing.T) {
	f := newFake()
	f.table("dbt_query_log", 10, 400, 900)
	f.set(settings.RetentionDaysKey("query_log"), "90") // 配了天数但没开总开关
	s, _, _ := newTestService(t, f)
	rr := s.RunOnce(context.Background(), "schedule")
	if rr.Ran || !strings.Contains(rr.Reason, "未启用") {
		t.Fatalf("总开关缺省必须关闭: %+v", rr)
	}
	if len(f.calls) != 0 || len(f.audits) != 0 || f.locks != 0 {
		t.Fatalf("关闭时不能删除、不能写审计、不能取锁: %+v %d %d", f.calls, len(f.audits), f.locks)
	}
	if got := len(*f.tables["dbt_query_log"]); got != 3 {
		t.Fatalf("行数应不变: %d", got)
	}
	// 显式写 false 也一样
	f.set(settings.KeyRetentionEnabled, "false")
	if rr := s.RunOnce(context.Background(), "manual"); rr.Ran {
		t.Fatal("false 不应执行")
	}
}

func TestEnabledWithoutDaysDoesNothing(t *testing.T) {
	f := newFake()
	enable(f)
	for _, tb := range []string{"dbt_query_log", "dbt_audit_log", "dbt_slow_logs", "dbt_query_event", "dbt_query_reject", "dbt_archive_run"} {
		f.table(tb, 1000, 2000)
	}
	s, _, _ := newTestService(t, f)
	rr := s.RunOnce(context.Background(), "schedule")
	if !rr.Ran || rr.Deleted != 0 || len(f.calls) != 0 {
		t.Fatalf("没配天数 = 不清理: %+v calls=%v", rr, f.calls)
	}
	if len(f.audits) != 0 {
		t.Fatalf("什么都没删不写审计: %+v", f.audits)
	}
}

func TestEachTableTargetByDays(t *testing.T) {
	cases := []struct {
		key, table string
		days       string
		ages       []int
		wantLeft   int
	}{
		{TargetQueryEvent, "dbt_query_event", "90", []int{10, 89, 91, 400}, 2},
		{TargetQueryReject, "dbt_query_reject", "30", []int{5, 29, 31, 400}, 2},
		{TargetQueryLog, "dbt_query_log", "180", []int{1, 179, 181, 900}, 2},
		{TargetSlowLogs, "dbt_slow_logs", "30", []int{1, 20, 40, 60}, 2},
		{TargetAuditLog, "dbt_audit_log", "365", []int{1, 364, 366, 1000}, 2},
		{TargetArchiveRun, "dbt_archive_run", "180", []int{1, 100, 200, 300}, 2},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			f := newFake()
			enable(f)
			f.table(c.table, c.ages...)
			// 其它目标的表也在，但没配天数，不能被动
			for _, other := range cases {
				if other.table != c.table {
					f.table(other.table, 5000, 6000)
				}
			}
			f.set(settings.RetentionDaysKey(c.key), c.days)
			s, _, _ := newTestService(t, f)
			rr := s.RunOnce(context.Background(), "schedule")
			if !rr.Ran || rr.Deleted != int64(len(c.ages)-c.wantLeft) {
				t.Fatalf("删除数: %+v", rr)
			}
			if got := len(*f.tables[c.table]); got != c.wantLeft {
				t.Fatalf("剩余 %d, want %d", got, c.wantLeft)
			}
			for _, other := range cases {
				if other.table != c.table && len(*f.tables[other.table]) != 2 {
					t.Fatalf("没配置的目标 %s 被动了", other.table)
				}
			}
			if len(f.audits) != 1 || f.audits[0].Action != "数据清理" || !strings.Contains(f.audits[0].ExtraInfo, c.key) {
				t.Fatalf("每轮应写一条审计汇总: %+v", f.audits)
			}
			if f.audits[0].UserName != "system" {
				t.Fatalf("审计操作人: %+v", f.audits[0])
			}
			if len(f.rounds) != 1 || f.rounds[0].Deleted != rr.Deleted || f.rounds[0].Status != "ok" {
				t.Fatalf("汇总记录: %+v", f.rounds)
			}
		})
	}
}

func TestAuditLogDeletesChildExtAndArchiveRunChildren(t *testing.T) {
	f := newFake()
	enable(f)
	f.table("dbt_audit_log", 500, 1)
	f.table("dbt_archive_run", 500, 1)
	f.set(settings.RetentionDaysKey("audit_log"), "100")
	f.set(settings.RetentionDaysKey("archive_run"), "100")
	s, _, _ := newTestService(t, f)
	s.RunOnce(context.Background(), "schedule")
	if got := f.childOf["dbt_audit_ext"]; len(got) != 1 || got[0] != 1 {
		t.Fatalf("dbt_audit_ext 应随 dbt_audit_log 一起删: %v", f.childOf)
	}
	if len(f.childOf["dbt_archive_event"]) != 1 || len(f.childOf["dbt_archive_batch_stat"]) != 1 {
		t.Fatalf("归档运行的 event / batch_stat 应一起删: %v", f.childOf)
	}
}

func TestMissingChildTableIsNotDeleted(t *testing.T) {
	f := newFake()
	enable(f)
	f.table("dbt_audit_log", 500)
	f.set("_no_dbt_audit_ext", "1") // 扩展表还没建
	f.set(settings.RetentionDaysKey("audit_log"), "100")
	s, _, _ := newTestService(t, f)
	rr := s.RunOnce(context.Background(), "schedule")
	if rr.Deleted != 1 || len(f.childOf["dbt_audit_ext"]) != 0 {
		t.Fatalf("扩展表不存在时只删主表: %+v %v", rr, f.childOf)
	}
}

func TestDaysFloorAndInvalidValues(t *testing.T) {
	f := newFake()
	enable(f)
	f.table("dbt_audit_log", 400)
	f.table("dbt_query_log", 400)
	f.table("dbt_slow_logs", 400)
	f.set(settings.RetentionDaysKey("audit_log"), "10") // 低于审计下限 30
	f.set(settings.RetentionDaysKey("query_log"), "3")  // 低于下限 7
	f.set(settings.RetentionDaysKey("slow_logs"), "abc")
	s, _, _ := newTestService(t, f)
	rr := s.RunOnce(context.Background(), "schedule")
	if rr.Deleted != 0 || len(f.calls) != 0 {
		t.Fatalf("低于下限 / 非法值的目标必须跳过: %+v", rr)
	}
	if len(rr.Problems) != 3 {
		t.Fatalf("应有 3 条配置问题: %v", rr.Problems)
	}
	for _, want := range []string{settings.RetentionDaysKey("audit_log"), settings.RetentionDaysKey("query_log"), settings.RetentionDaysKey("slow_logs")} {
		found := false
		for _, p := range rr.Problems {
			found = found || strings.Contains(p, want)
		}
		if !found {
			t.Fatalf("缺少 %s 的问题说明: %v", want, rr.Problems)
		}
	}
	// 负数、0
	f.set(settings.RetentionDaysKey("audit_log"), "-5")
	f.set(settings.RetentionDaysKey("query_log"), "0")
	set := loadSettings(f)
	if set.Days[TargetAuditLog] != 0 || set.Days[TargetQueryLog] != 0 {
		t.Fatalf("负数 / 0 = 不清理: %v", set.Days)
	}
}

func TestBatchesAreCappedAndSleep(t *testing.T) {
	f := newFake()
	enable(f)
	ages := make([]int, 12000)
	for i := range ages {
		ages[i] = 500
	}
	f.table("dbt_query_log", ages...)
	f.set(settings.RetentionDaysKey("query_log"), "100")
	f.set(settings.KeyRetentionBatchSize, "99999") // 超过硬上限 5000 → 非法，用缺省 1000
	s, sleeps, _ := newTestService(t, f)
	rr := s.RunOnce(context.Background(), "schedule")
	if rr.Deleted != 12000 || len(*f.tables["dbt_query_log"]) != 0 {
		t.Fatalf("应清空: %+v", rr)
	}
	for _, c := range f.calls {
		if c.Limit > 1000 {
			t.Fatalf("批大小非法时应回到缺省 1000: %+v", c)
		}
	}
	if len(f.calls) != 13 { // 12 个满批 + 1 个空批确认完成
		t.Fatalf("批数: %d", len(f.calls))
	}
	if len(*sleeps) != 12 || (*sleeps)[0] != 200*time.Millisecond {
		t.Fatalf("批间应休眠 200ms: %v", *sleeps)
	}
	if len(rr.Problems) == 0 {
		t.Fatal("非法批大小应报告配置问题")
	}

	// 合法的最大值 5000
	f2 := newFake()
	enable(f2)
	f2.table("dbt_query_log", ages...)
	f2.set(settings.RetentionDaysKey("query_log"), "100")
	f2.set(settings.KeyRetentionBatchSize, "5000")
	f2.set(settings.KeyRetentionBatchSleepMs, "0")
	s2, sleeps2, _ := newTestService(t, f2)
	s2.RunOnce(context.Background(), "schedule")
	for _, c := range f2.calls {
		if c.Limit != 5000 {
			t.Fatalf("批大小应为 5000: %+v", c)
		}
	}
	if len(f2.calls) != 3 { // 5000 + 5000 + 2000
		t.Fatalf("批数: %d", len(f2.calls))
	}
	for _, d := range *sleeps2 {
		if d != 0 {
			t.Fatalf("休眠 0 时不应休眠: %v", *sleeps2)
		}
	}
}

func TestMaxRowsPerRoundIsPartial(t *testing.T) {
	f := newFake()
	enable(f)
	ages := make([]int, 3000)
	for i := range ages {
		ages[i] = 500
	}
	f.table("dbt_query_log", ages...)
	f.set(settings.RetentionDaysKey("query_log"), "100")
	f.set(settings.KeyRetentionMaxRows, "1500")
	f.set(settings.KeyRetentionBatchSize, "1000")
	s, _, _ := newTestService(t, f)
	rr := s.RunOnce(context.Background(), "schedule")
	if rr.Deleted != 1500 || len(*f.tables["dbt_query_log"]) != 1500 {
		t.Fatalf("每轮上限 1500: %+v", rr)
	}
	if len(rr.Targets) != 1 || rr.Targets[0].Status != StatusPartial || !strings.Contains(rr.Targets[0].Note, "下一轮继续") {
		t.Fatalf("应为 partial: %+v", rr.Targets)
	}
	if f.calls[len(f.calls)-1].Limit != 500 {
		t.Fatalf("最后一批只能取剩余额度: %+v", f.calls)
	}
}

func TestPinnedRowsAreKept(t *testing.T) {
	f := newFake()
	enable(f)
	rows := f.table("dbt_query_log", 500, 500, 500)
	(*rows)[1].Keep = true // 收藏的查询
	f.set(settings.RetentionDaysKey("query_log"), "100")
	s, _, _ := newTestService(t, f)
	rr := s.RunOnce(context.Background(), "schedule")
	if rr.Deleted != 2 || len(*f.tables["dbt_query_log"]) != 1 || (*f.tables["dbt_query_log"])[0].ID != 2 {
		t.Fatalf("被固定条件排除的行不能删: %+v", rr)
	}
}

func TestWindow(t *testing.T) {
	f := newFake()
	enable(f)
	f.table("dbt_query_log", 500)
	f.set(settings.RetentionDaysKey("query_log"), "100")
	f.set(settings.KeyRetentionWindow, "01:00-05:00")

	// 03:00 在窗口内
	s, _, _ := newTestService(t, f)
	if rr := s.RunOnce(context.Background(), "schedule"); !rr.Ran || rr.Deleted != 1 {
		t.Fatalf("窗口内应执行: %+v", rr)
	}

	// 12:00 不在窗口内：定时不执行；手动不受窗口限制
	f2 := newFake()
	enable(f2)
	f2.table("dbt_query_log", 500)
	f2.set(settings.RetentionDaysKey("query_log"), "100")
	f2.set(settings.KeyRetentionWindow, "01:00-05:00")
	s2, _, clock := newTestService(t, f2)
	*clock = time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local)
	rr := s2.RunOnce(context.Background(), "schedule")
	if rr.Ran || !strings.Contains(rr.Reason, "窗口") || len(f2.calls) != 0 {
		t.Fatalf("窗口外不应执行: %+v", rr)
	}
	if rr := s2.RunOnce(context.Background(), "manual"); !rr.Ran || rr.Deleted != 1 {
		t.Fatalf("手动触发不受窗口约束: %+v", rr)
	}

	// 跨零点窗口 22:00-02:00
	f3 := newFake()
	enable(f3)
	f3.table("dbt_query_log", 500)
	f3.set(settings.RetentionDaysKey("query_log"), "100")
	f3.set(settings.KeyRetentionWindow, "22:00-02:00")
	s3, _, clock3 := newTestService(t, f3)
	*clock3 = time.Date(2026, 10, 9, 23, 30, 0, 0, time.Local)
	if rr := s3.RunOnce(context.Background(), "schedule"); !rr.Ran {
		t.Fatalf("跨零点窗口内应执行: %+v", rr)
	}

	// 窗口写错：宁可不执行
	f4 := newFake()
	enable(f4)
	f4.table("dbt_query_log", 500)
	f4.set(settings.RetentionDaysKey("query_log"), "100")
	f4.set(settings.KeyRetentionWindow, "3am-5am")
	s4, _, _ := newTestService(t, f4)
	rr4 := s4.RunOnce(context.Background(), "schedule")
	if rr4.Ran || len(f4.calls) != 0 || len(rr4.Problems) == 0 {
		t.Fatalf("非法窗口不应执行并提示: %+v", rr4)
	}
}

func TestWindowEndsMidRun(t *testing.T) {
	f := newFake()
	enable(f)
	ages := make([]int, 5000)
	for i := range ages {
		ages[i] = 500
	}
	f.table("dbt_query_log", ages...)
	f.table("dbt_slow_logs", 500)
	f.set(settings.RetentionDaysKey("query_log"), "100")
	f.set(settings.RetentionDaysKey("slow_logs"), "100")
	f.set(settings.KeyRetentionWindow, "01:00-04:00")
	f.set(settings.KeyRetentionBatchSize, "1000")
	s, _, clock := newTestService(t, f)
	// 第二批之后窗口结束
	n := 0
	s.sleep = func(context.Context, time.Duration) {
		n++
		if n == 2 {
			*clock = time.Date(2026, 10, 9, 4, 1, 0, 0, time.Local)
		}
	}
	rr := s.RunOnce(context.Background(), "schedule")
	if rr.Deleted != 2000 {
		t.Fatalf("窗口结束后应停止（共 2 批）: %+v", rr)
	}
	if rr.Stopped != "时间窗口结束" {
		t.Fatalf("Stopped: %q", rr.Stopped)
	}
	if len(*f.tables["dbt_slow_logs"]) != 1 {
		t.Fatal("窗口结束后后面的目标也不再执行")
	}
	if rr.Targets[0].Status != StatusPartial {
		t.Fatalf("%+v", rr.Targets)
	}
}

func TestMaxDurationStopsRound(t *testing.T) {
	f := newFake()
	enable(f)
	ages := make([]int, 4000)
	for i := range ages {
		ages[i] = 500
	}
	f.table("dbt_query_log", ages...)
	f.set(settings.RetentionDaysKey("query_log"), "100")
	f.set(settings.KeyRetentionMaxMinutes, "1")
	s, _, clock := newTestService(t, f)
	s.sleep = func(context.Context, time.Duration) { *clock = clock.Add(40 * time.Second) }
	rr := s.RunOnce(context.Background(), "manual")
	if rr.Deleted >= 4000 || !strings.Contains(rr.Targets[0].Note, "最长执行时间") {
		t.Fatalf("应因超时停止: %+v", rr)
	}
}

func TestCancelStops(t *testing.T) {
	f := newFake()
	enable(f)
	f.table("dbt_query_log", 500, 500)
	f.set(settings.RetentionDaysKey("query_log"), "100")
	s, _, _ := newTestService(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rr := s.RunOnce(ctx, "manual")
	if rr.Deleted != 0 {
		t.Fatalf("已取消不应删除: %+v", rr)
	}
}

func TestLockPreventsConcurrentRun(t *testing.T) {
	f := newFake()
	enable(f)
	f.table("dbt_query_log", 500)
	f.set(settings.RetentionDaysKey("query_log"), "100")
	f.lockBusy = true
	s, _, _ := newTestService(t, f)
	rr := s.RunOnce(context.Background(), "schedule")
	if rr.Ran || !strings.Contains(rr.Reason, "别的进程") || len(f.calls) != 0 {
		t.Fatalf("拿不到 GET_LOCK 不能执行: %+v", rr)
	}
	f.lockBusy = false
	f.lockErr = errors.New("db down")
	rr = s.RunOnce(context.Background(), "schedule")
	if rr.Ran || len(f.calls) != 0 {
		t.Fatalf("取锁出错不能执行: %+v", rr)
	}
	f.lockErr = nil
	if rr := s.RunOnce(context.Background(), "schedule"); !rr.Ran || rr.Deleted != 1 || f.locks != 1 {
		t.Fatalf("锁可用时执行并持锁: %+v locks=%d", rr, f.locks)
	}
}

func TestSameProcessSingleRound(t *testing.T) {
	f := newFake()
	enable(f)
	f.table("dbt_query_log", 500)
	f.set(settings.RetentionDaysKey("query_log"), "100")
	s, _, _ := newTestService(t, f)
	s.mu.Lock()
	rr := s.RunOnce(context.Background(), "schedule")
	s.mu.Unlock()
	if rr.Ran || !strings.Contains(rr.Reason, "本进程") {
		t.Fatalf("本进程已有一轮在执行时不能再开: %+v", rr)
	}
}

func TestErrorStopsTargetAndContinuesNext(t *testing.T) {
	f := newFake()
	enable(f)
	ages := make([]int, 2500)
	for i := range ages {
		ages[i] = 500
	}
	f.table("dbt_query_event", ages...)
	f.table("dbt_query_log", 500, 500)
	f.set(settings.RetentionDaysKey("query_event"), "100")
	f.set(settings.RetentionDaysKey("query_log"), "100")
	f.set(settings.KeyRetentionBatchSize, "1000")
	f.failOn["dbt_query_event"] = errors.New("deadlock found")
	s, _, _ := newTestService(t, f)
	rr := s.RunOnce(context.Background(), "schedule")
	if !rr.Failed() {
		t.Fatalf("应有失败项: %+v", rr)
	}
	if len(*f.tables["dbt_query_log"]) != 0 {
		t.Fatal("某目标出错后应继续下一个目标")
	}
	if len(f.calls) != 1 || f.calls[0].Table != "dbt_query_log" {
		t.Fatalf("出错目标只尝试一次就停止: %+v", f.calls)
	}
	if len(f.audits) != 1 || !strings.Contains(f.audits[0].ExtraInfo, "失败项：query_event") || !strings.Contains(f.audits[0].ExtraInfo, "deadlock") {
		t.Fatalf("审计应包含失败项: %+v", f.audits)
	}
	if f.rounds[0].Status != "partial" {
		t.Fatalf("汇总状态: %+v", f.rounds)
	}
}

func TestSelectedButNothingDeletedStops(t *testing.T) {
	f := newFake()
	enable(f)
	f.table("dbt_query_log", 500)
	f.set(settings.RetentionDaysKey("query_log"), "100")
	s, _, _ := newTestService(t, f)
	wrapped := &zeroDelRepo{fakeRepo: f}
	s.repo = wrapped
	rr := s.RunOnce(context.Background(), "schedule")
	if !rr.Failed() || wrapped.n != 1 {
		t.Fatalf("选中却没删掉应报错并停止，不能死循环: %+v n=%d", rr, wrapped.n)
	}
}

type zeroDelRepo struct {
	*fakeRepo
	n int
}

func (z *zeroDelRepo) DeleteBatch(context.Context, Target, time.Time, int) (int, int64, error) {
	z.n++
	return 5, 0, nil
}

func TestMissingTableOrColumnSkipped(t *testing.T) {
	f := newFake()
	enable(f)
	// dbt_slow_logs 表不存在；dbt_query_log 缺 favorite 列；dbt_archive_run 缺依赖表 dbt_archive_task
	f.table("dbt_query_log", 500)
	f.missing["dbt_query_log"] = []string{"favorite"}
	f.table("dbt_archive_run", 500)
	f.set("_no_dbt_archive_task", "1")
	f.set(settings.RetentionDaysKey("slow_logs"), "30")
	f.set(settings.RetentionDaysKey("query_log"), "30")
	f.set(settings.RetentionDaysKey("archive_run"), "30")
	s, _, _ := newTestService(t, f)
	rr := s.RunOnce(context.Background(), "schedule")
	if rr.Deleted != 0 || len(f.calls) != 0 {
		t.Fatalf("缺表缺列不能执行: %+v", rr)
	}
	notes := map[string]string{}
	for _, tr := range rr.Targets {
		if tr.Status != StatusSkipped {
			t.Fatalf("应为 skipped: %+v", tr)
		}
		notes[tr.Key] = tr.Note
	}
	if !strings.Contains(notes[TargetSlowLogs], "不存在") || !strings.Contains(notes[TargetQueryLog], "favorite") || !strings.Contains(notes[TargetArchiveRun], "dbt_archive_task") {
		t.Fatalf("跳过原因: %v", notes)
	}
	if len(f.audits) != 0 {
		t.Fatalf("只有跳过不写审计: %+v", f.audits)
	}
}

func TestPreviewDoesNotDelete(t *testing.T) {
	f := newFake()
	// 总开关没开，也能预览
	f.table("dbt_query_log", 10, 500, 600)
	f.table("dbt_audit_log", 1, 2000)
	f.set(settings.RetentionDaysKey("query_log"), "100")
	f.set(settings.RetentionDaysKey("audit_log"), "365")
	f.set(settings.KeyRetentionWindow, "01:00-05:00")
	s, _, clock := newTestService(t, f)
	*clock = time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local)
	p := s.Preview(context.Background())
	if p.Enabled || p.WindowOpen || p.Window != "01:00-05:00" {
		t.Fatalf("预览应如实反映开关与窗口: %+v", p)
	}
	by := map[string]PreviewItem{}
	for _, it := range p.Items {
		by[it.Key] = it
	}
	if by[TargetQueryLog].Candidates != 2 || by[TargetAuditLog].Candidates != 1 || !by[TargetQueryLog].Configured {
		t.Fatalf("预览数量: %+v", p.Items)
	}
	if by[TargetSlowLogs].Configured || by[TargetSlowLogs].Status != "off" || by[TargetSlowLogs].Risk == "" || by[TargetSlowLogs].Suggest == "" {
		t.Fatalf("未配置的目标: %+v", by[TargetSlowLogs])
	}
	if len(f.calls) != 0 || len(*f.tables["dbt_query_log"]) != 3 || len(*f.tables["dbt_audit_log"]) != 2 {
		t.Fatal("预览不能删除任何东西")
	}
	if len(f.audits) != 0 || f.locks != 0 {
		t.Fatal("预览不写审计(由接口写)、不取锁")
	}
}

func TestSummaryTextIsInformative(t *testing.T) {
	rr := RoundResult{Trigger: "schedule", Deleted: 12, Stopped: "时间窗口结束", Targets: []TargetResult{
		{Key: "query_log", Days: 90, Cutoff: "2026-07-11 03:00:00", Deleted: 12, Status: StatusPartial, Note: "窗口"},
		{Key: "slow_logs", Status: StatusSkipped, Note: "表不存在"},
		{Key: "audit_log", Status: StatusError, Err: "boom"},
	}}
	got := summaryText(&rr)
	for _, w := range []string{"共删除 12", "query_log：删除 12", "早于 2026-07-11", "slow_logs：跳过", "audit_log：出错", "时间窗口结束"} {
		if !strings.Contains(got, w) {
			t.Fatalf("汇总缺少 %q: %s", w, got)
		}
	}
}

func TestWorkerRunsRoundsUntilCancel(t *testing.T) {
	f := newFake()
	enable(f)
	f.table("dbt_query_log", 500)
	f.set(settings.RetentionDaysKey("query_log"), "100")
	s, _, _ := newTestService(t, f)
	s.firstDelay = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Worker(ctx); close(done) }()
	deadline := time.After(3 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.calls)
		f.mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Worker 没有执行第一轮")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后 Worker 应返回")
	}
}
