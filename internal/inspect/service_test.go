// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dbtool/internal/model"
)

var (
	admin  = Actor{ID: 1, Username: "root", Display: "管理员", IsSuperuser: true}
	all    = Scope{All: true}
	nobody = Scope{IDs: map[uint]bool{}}
)

// run 同步创建并执行一份报告。
func (e *env) run(t *testing.T, kind string, instID uint, opt RunOptions) (ReportRef, Outcome) {
	t.Helper()
	id, busy, err := e.repo.CreateReport(kind, instID, TriggerManual, "root", 0, e.clk.Now(), time.Hour)
	if err != nil || busy {
		t.Fatalf("建报告失败：busy=%v err=%v", busy, err)
	}
	in, _ := e.repo.GetInstance(instID)
	ref := ReportRef{kind, id}
	return ref, e.svc.execute(*in, ref, opt)
}

func asErr(t *testing.T, err error, status int) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Status != status {
		t.Fatalf("期望 %d 错误，得到 %v", status, err)
	}
	return e
}

func TestRunNowEndToEnd(t *testing.T) {
	e := newEnv()
	res, err := e.svc.RunNow(admin, all, []uint{1, 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 2 || res.Created[0].Ref != "mysql-1" || res.Created[1].Ref != "mssql-1" {
		t.Fatalf("创建结果不对：%+v", res)
	}
	e.waitIdle()

	my := e.repo.report(KindMySQL, 1)
	if my.Status != StatusFinish || my.HealthScore == nil || my.Result == "" || my.Operator != "root" || my.Trigger != TriggerManual {
		t.Fatalf("MySQL 报告不对：%+v", my.ReportSummary)
	}
	if my.Total != len(mysqlSteps) || my.Done != len(mysqlSteps) || my.Alert == 0 || my.FinishTime == nil {
		t.Errorf("MySQL 进度或计数不对：%+v", my.ReportSummary)
	}
	ms := e.repo.report(KindMSSQL, 1)
	if ms.Status != StatusFinish || len(ms.Items) == 0 || ms.Summary == "" || ms.ServerStart == nil || ms.HealthScore != nil {
		t.Fatalf("SQL Server 报告不对：%+v", ms.ReportSummary)
	}
	if ms.Total != ms.Done || ms.Done != len(ms.Items) || ms.Alert+ms.Attention+ms.Normal+ms.Failed != len(ms.Items) {
		t.Errorf("SQL Server 计数不对：%+v items=%d", ms.ReportSummary, len(ms.Items))
	}
	// 每库检查项对 2 个用户库各跑一次
	perDB := 0
	for _, c := range msChecks {
		if c.Scope == scopeDatabase {
			perDB++
		}
	}
	if want := len(msChecks) - perDB + perDB*2; len(ms.Items) != want {
		t.Errorf("检查项数 %d，期望 %d", len(ms.Items), want)
	}
	if acts := e.repo.auditActions(); len(acts) != 1 || acts[0] != "性能巡检·立即巡检" {
		t.Errorf("审计：%v", acts)
	}
	if e.repo.audits[0].UserName != "root" || !strings.Contains(e.repo.audits[0].ExtraInfo, "prod-mysql-01") || strings.Contains(e.repo.audits[0].ExtraInfo, "password") {
		t.Errorf("审计内容：%+v", e.repo.audits[0])
	}
}

func TestRunNowFilters(t *testing.T) {
	e := newEnv()
	// 范围只含实例 1：2、99 不可见按不存在处理；4 不在范围内也一样（不泄露存在性）
	res, err := e.svc.RunNow(admin, Scope{IDs: map[uint]bool{1: true}}, []uint{1, 2, 4, 99})
	if err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	if len(res.Created) != 1 || len(res.NotFound) != 3 || len(res.Unsupported) != 0 {
		t.Errorf("范围过滤不对：%+v", res)
	}
	// 范围内但类型不支持
	res, _ = e.svc.RunNow(admin, all, []uint{4})
	if len(res.Unsupported) != 1 || !strings.Contains(res.Unsupported[0], "redis-01") || len(res.Created) != 0 {
		t.Errorf("不支持的类型要明确说明：%+v", res)
	}
	// 已有进行中的巡检：跳过
	e.repo.CreateReport(KindMySQL, 2, TriggerManual, "x", 0, e.clk.Now(), time.Hour)
	res, _ = e.svc.RunNow(admin, all, []uint{2})
	if len(res.Busy) != 1 || res.Busy[0] != "prod-mysql-02" || len(res.Created) != 0 {
		t.Errorf("进行中的应跳过：%+v", res)
	}
	// 进行中的报告超过阈值视为进程中断，可以再建
	e.clk.Add(2 * time.Hour)
	res, _ = e.svc.RunNow(admin, all, []uint{2})
	e.waitIdle()
	if len(res.Created) != 1 {
		t.Errorf("超时的 running 应被收尾：%+v", res)
	}
	if st := e.repo.report(KindMySQL, 2).Status; st != StatusFailed {
		t.Errorf("中断的报告应记为失败：%s", st)
	}
}

func TestRunNowValidation(t *testing.T) {
	e := newEnv()
	_, err := e.svc.RunNow(admin, all, nil)
	asErr(t, err, 400)
	many := make([]uint, 101)
	for i := range many {
		many[i] = uint(i + 1)
	}
	_, err = e.svc.RunNow(admin, all, many)
	asErr(t, err, 400)
	if len(e.repo.reports) != 0 {
		t.Error("校验失败不应建报告")
	}
	// 不可见范围为空：什么都不能巡检
	res, _ := e.svc.RunNow(admin, nobody, []uint{1})
	if len(res.Created) != 0 || len(res.NotFound) != 1 {
		t.Errorf("%+v", res)
	}
}

func TestOpenFailureMarksReportFailed(t *testing.T) {
	e := newEnv()
	e.openErr[1] = errors.New("无法连接到 10.0.0.1:3306：i/o timeout")
	ref, out := e.run(t, KindMySQL, 1, RunOptions{})
	if out.Status != StatusFailed || !strings.Contains(out.Err, "i/o timeout") {
		t.Errorf("结果：%+v", out)
	}
	r := e.repo.report(ref.Kind, ref.ID)
	if r.Status != StatusFailed || !strings.Contains(r.Error, "10.0.0.1:3306") || r.FinishTime == nil {
		t.Errorf("报告：%+v", r.ReportSummary)
	}
}

func TestMySQLUnreachableAfterConnect(t *testing.T) {
	e := newEnv()
	e.ft = func(model.Instance) *fakeTarget { return &fakeTarget{} } // 所有查询都没有预设 → 都失败
	ref, out := e.run(t, KindMySQL, 1, RunOptions{})
	if out.Status != StatusFailed || !strings.Contains(out.Err, "无法采集实例信息") {
		t.Errorf("基本信息都采不到应整份失败：%+v", out)
	}
	if r := e.repo.report(ref.Kind, ref.ID); r.Status != StatusFailed || r.Result != "" {
		t.Errorf("%+v", r.ReportSummary)
	}
}

func TestMySQLPartialFailureStillFinishes(t *testing.T) {
	e := newEnv()
	e.ft = func(model.Instance) *fakeTarget {
		ft := mysqlHappyTarget()
		ft.rules = append([]rule{{sub: "FROM mysql.user", err: errors.New("SELECT command denied to user for table 'user'")}}, ft.rules...)
		return ft
	}
	ref, out := e.run(t, KindMySQL, 1, RunOptions{})
	if out.Status != StatusFinish || out.Failed != 1 {
		t.Fatalf("单项失败只标未采集：%+v", out)
	}
	r := e.repo.report(ref.Kind, ref.ID)
	if r.Failed != 1 || !strings.Contains(r.Result, "denied") {
		t.Errorf("%+v", r.ReportSummary)
	}
	if out.Score == nil || *out.Score != int(*r.HealthScore) {
		t.Errorf("评分：%v vs %v", out.Score, r.HealthScore)
	}
}

func TestMSSQLItemFailureIsolated(t *testing.T) {
	e := newEnv()
	e.ft = func(model.Instance) *fakeTarget {
		ft := mssqlHappyTarget()
		ft.rules = append([]rule{
			{sub: "Number of Deadlocks/sec", err: errors.New("mssql: The user does not have permission to perform this action. (297)")},
			{sub: "dm_io_pending_io_requests", err: errors.New("connection reset by peer")},
		}, ft.rules...)
		return ft
	}
	ref, out := e.run(t, KindMSSQL, 3, RunOptions{})
	if out.Status != StatusFinish || out.Failed < 2 {
		t.Fatalf("%+v", out)
	}
	r := e.repo.report(ref.Kind, ref.ID)
	byKey := map[string]MSSQLItem{}
	for _, it := range r.Items {
		byKey[it.Key] = it
	}
	if it := byKey["blocking.deadlocks"]; it.Level != LevelFailed || !strings.HasPrefix(it.Value, "无权限：") {
		t.Errorf("权限不足：%+v", it)
	}
	if it := byKey["io.pending"]; it.Level != LevelFailed || !strings.HasPrefix(it.Value, "采集失败：") {
		t.Errorf("采集失败：%+v", it)
	}
	if it := byKey["memory.ple"]; it.Level != LevelAlert {
		t.Errorf("其他项不受影响：%+v", it)
	}
	if !strings.Contains(r.Summary, `"top"`) || !strings.Contains(r.Summary, "PLE") {
		t.Errorf("摘要里应有 Top 问题：%s", r.Summary)
	}
}

func TestMSSQLBudget(t *testing.T) {
	e := newEnv()
	e.ft = func(model.Instance) *fakeTarget {
		ft := mssqlHappyTarget()
		ft.hook = func(n int) { e.clk.Add(time.Minute) } // 每条查询“耗时”1 分钟
		return ft
	}
	ref, _ := e.run(t, KindMSSQL, 3, RunOptions{Timeout: 5 * time.Minute})
	r := e.repo.report(ref.Kind, ref.ID)
	over := 0
	for _, it := range r.Items {
		if strings.Contains(it.Value, "时间预算未执行") {
			over++
			if it.Level != LevelFailed {
				t.Errorf("超预算应标未采集：%+v", it)
			}
		}
	}
	if over == 0 || over == len(r.Items) {
		t.Errorf("前面的项应执行、后面的超预算：%d / %d", over, len(r.Items))
	}
}

func TestCancelRunningMySQL(t *testing.T) {
	e := newEnv()
	block := make(chan struct{})
	started := make(chan struct{})
	e.ft = func(model.Instance) *fakeTarget {
		ft := mysqlHappyTarget()
		ft.block, ft.started = block, started
		return ft
	}
	res, err := e.svc.RunNow(admin, all, []uint{1})
	if err != nil || len(res.Created) != 1 {
		t.Fatal(res, err)
	}
	<-started
	cr, err := e.svc.Cancel(admin, all, []ReportRef{{KindMySQL, 1}})
	if err != nil || cr.Cancelled != 1 {
		t.Fatalf("%+v %v", cr, err)
	}
	e.waitIdle() // 取消后正在跑的任务应及时停下，而不是等到 block 解除
	r := e.repo.report(KindMySQL, 1)
	if r.Status != StatusCancelled || !strings.Contains(r.Error, "root 于 2026-10-06 08:00:00 取消") || r.Result != "" {
		t.Errorf("取消后的报告：%+v err=%q", r.ReportSummary, r.Error)
	}
	// 已结束的再取消：冲突
	_, err = e.svc.Cancel(admin, all, []ReportRef{{KindMySQL, 1}})
	asErr(t, err, 409)
	// 范围外的看不到：按不存在 / 已结束处理
	_, err = e.svc.Cancel(admin, nobody, []ReportRef{{KindMySQL, 1}})
	asErr(t, err, 409)
	if acts := e.repo.auditActions(); acts[len(acts)-1] != "性能巡检·取消巡检" {
		t.Errorf("审计：%v", acts)
	}
}

func TestCancelKeepsPartialMSSQLItems(t *testing.T) {
	e := newEnv()
	var cancelled atomic.Bool
	e.ft = func(model.Instance) *fakeTarget {
		ft := mssqlHappyTarget()
		ft.hook = func(n int) {
			if n == 12 && cancelled.CompareAndSwap(false, true) {
				if _, err := e.svc.Cancel(admin, all, []ReportRef{{KindMSSQL, 1}}); err != nil {
					t.Error(err)
				}
			}
		}
		return ft
	}
	e.svc.RunNow(admin, all, []uint{3})
	e.waitIdle()
	r := e.repo.report(KindMSSQL, 1)
	if r.Status != StatusCancelled || len(r.Items) == 0 || len(r.Items) >= r.Total || r.Total == 0 {
		t.Errorf("取消后应保留已采集的部分：status=%s items=%d total=%d", r.Status, len(r.Items), r.Total)
	}
}

func TestDeleteReports(t *testing.T) {
	e := newEnv()
	e.run(t, KindMySQL, 1, RunOptions{})
	e.run(t, KindMSSQL, 3, RunOptions{})
	e.repo.CreateReport(KindMySQL, 2, TriggerManual, "x", 0, e.clk.Now(), time.Hour) // mysql-2 进行中
	dr, err := e.svc.Delete(admin, all, []ReportRef{{KindMySQL, 1}, {KindMSSQL, 1}, {KindMySQL, 2}, {KindMySQL, 77}})
	if err != nil || dr.Deleted != 2 || dr.Running != 1 {
		t.Fatalf("%+v %v", dr, err)
	}
	if e.repo.report(KindMySQL, 1) != nil || e.repo.report(KindMSSQL, 1) != nil || e.repo.report(KindMySQL, 2) == nil {
		t.Error("删除对象不对")
	}
	_, err = e.svc.Delete(admin, all, []ReportRef{{KindMySQL, 2}})
	asErr(t, err, 409)
	_, err = e.svc.Delete(admin, all, []ReportRef{{KindMySQL, 77}})
	asErr(t, err, 404)
	// 同号不同类：mysql-1 与 mssql-1 互不影响
	e.run(t, KindMySQL, 1, RunOptions{})
	e.run(t, KindMSSQL, 3, RunOptions{})
	e.svc.Delete(admin, all, []ReportRef{{KindMSSQL, 2}})
	if e.repo.report(KindMySQL, 2) == nil {
		t.Error("删除 mssql-2 不应影响 mysql-2")
	}
	if _, err := ParseRefs([]string{"bad"}); err == nil {
		t.Error("非法标识应报错")
	}
	if _, err := ParseRefs(nil); err == nil {
		t.Error("空列表应报错")
	}
	// 范围外：不能删
	e.run(t, KindMySQL, 1, RunOptions{})
	_, err = e.svc.Delete(admin, nobody, []ReportRef{{KindMySQL, 3}})
	asErr(t, err, 404)
}

func seedReport(r *memRepo, kind string, instID uint, name, dbType, status, trigger string, alert, attention int, start string) uint {
	id, _, _ := r.CreateReport(kind, instID, trigger, "root", 0, ts(start), time.Hour)
	x := r.reports[ReportRef{kind, id}]
	x.InstanceName, x.DBType, x.Status, x.Alert, x.Attention = name, dbType, status, alert, attention
	return id
}

func TestListReportsFilters(t *testing.T) {
	e := newEnv()
	r := e.repo
	seedReport(r, KindMySQL, 1, "prod-mysql-01", "mysql", StatusFinish, TriggerSchedule, 1, 3, "2026-10-05 08:00:00")
	seedReport(r, KindMSSQL, 3, "mssql-01", "mssql", StatusFinish, TriggerSchedule, 2, 5, "2026-10-06 03:00:00")
	seedReport(r, KindMySQL, 2, "prod-mysql-02", "mysql", StatusFailed, TriggerManual, 0, 0, "2026-10-04 08:00:00")
	seedReport(r, KindMSSQL, 3, "mssql-01", "mssql", StatusCancelled, TriggerManual, 0, 1, "2026-10-03 08:00:00")
	seedReport(r, KindMySQL, 1, "prod-mysql-01", "mysql", StatusFinish, TriggerManual, 0, 0, "2026-10-02 08:00:00")

	list, total, err := e.svc.ListReports(all, ReportQuery{})
	if err != nil || total != 5 || len(list) != 5 {
		t.Fatalf("%d %v", total, err)
	}
	if list[0].Kind != KindMSSQL || list[0].StartTime != ts("2026-10-06 03:00:00") { // 两类合并，按开始时间倒序
		t.Errorf("排序不对：%+v", list[0])
	}
	for level, want := range map[string]int{"alert": 2, "attention": 1, "clean": 1, "failed": 1, "cancelled": 1} {
		if _, n, _ := e.svc.ListReports(all, ReportQuery{Level: level}); int(n) != want {
			t.Errorf("level=%s：%d，期望 %d", level, n, want)
		}
	}
	if _, n, _ := e.svc.ListReports(all, ReportQuery{Trigger: TriggerSchedule}); n != 2 {
		t.Errorf("trigger=schedule：%d", n)
	}
	if _, n, _ := e.svc.ListReports(all, ReportQuery{InstanceID: 3}); n != 2 {
		t.Errorf("instance=3：%d", n)
	}
	if _, n, _ := e.svc.ListReports(all, ReportQuery{Search: "mysql-02"}); n != 1 {
		t.Errorf("search：%d", n)
	}
	from, to := ts("2026-10-03 00:00:00"), ts("2026-10-05 00:00:00")
	if _, n, _ := e.svc.ListReports(all, ReportQuery{Start: &from, End: &to}); n != 2 {
		t.Errorf("日期范围：%d", n)
	}
	page, total, _ := e.svc.ListReports(all, ReportQuery{Offset: 4, Limit: 2})
	if total != 5 || len(page) != 1 {
		t.Errorf("分页：%d/%d", len(page), total)
	}
	// 可见范围：只能看自己资源组内的实例
	_, n, _ := e.svc.ListReports(Scope{IDs: map[uint]bool{3: true}}, ReportQuery{})
	if n != 2 {
		t.Errorf("范围过滤：%d", n)
	}
	if l, n, _ := e.svc.ListReports(nobody, ReportQuery{}); n != 0 || len(l) != 0 {
		t.Errorf("范围为空：%d", n)
	}
	// 非法筛选
	_, _, err = e.svc.ListReports(all, ReportQuery{Level: "bogus"})
	asErr(t, err, 400)
	_, _, err = e.svc.ListReports(all, ReportQuery{Trigger: "bogus"})
	asErr(t, err, 400)
	if r2, _, _ := e.svc.ListReports(all, ReportQuery{Limit: 1000}); len(r2) != 5 {
		t.Error("过大的 limit 应回到默认值")
	}
}

// Latest：每个实例只取最近一次结束（成功或失败）的报告，进行中、已取消的不算；级别等条件在“最近一次”之上再筛。
func TestListReportsLatest(t *testing.T) {
	e := newEnv()
	r := e.repo
	seedReport(r, KindMySQL, 1, "prod-mysql-01", "mysql", StatusFinish, TriggerSchedule, 2, 1, "2026-10-02 08:00:00") // 实例 1 以前有告警……
	seedReport(r, KindMySQL, 1, "prod-mysql-01", "mysql", StatusFinish, TriggerSchedule, 0, 0, "2026-10-05 08:00:00") // ……最近一次已经正常
	seedReport(r, KindMySQL, 2, "prod-mysql-02", "mysql", StatusFailed, TriggerManual, 0, 0, "2026-10-04 08:00:00")
	seedReport(r, KindMSSQL, 3, "mssql-01", "mssql", StatusFinish, TriggerSchedule, 2, 5, "2026-10-06 03:00:00")
	seedReport(r, KindMSSQL, 3, "mssql-01", "mssql", StatusCancelled, TriggerManual, 0, 1, "2026-10-07 08:00:00") // 比有告警那份新，但已取消，不算
	seedReport(r, KindMSSQL, 3, "mssql-01", "mssql", StatusRunning, TriggerManual, 0, 0, "2026-10-08 08:00:00")   // 进行中，不算

	if _, n, _ := e.svc.ListReports(all, ReportQuery{Level: "alert"}); n != 2 {
		t.Fatalf("不取最近一次时 alert 应有 2 份（含实例 1 以前的）：%d", n)
	}
	list, total, err := e.svc.ListReports(all, ReportQuery{Latest: true, Limit: 1000})
	if err != nil || total != 3 || len(list) != 3 {
		t.Fatalf("每个实例一份：%d/%d %v", len(list), total, err)
	}
	byInst := map[uint]ReportSummary{}
	for _, s := range list {
		byInst[s.InstanceID] = s
	}
	if byInst[1].Alert != 0 || byInst[1].Status != StatusFinish || byInst[2].Status != StatusFailed || byInst[3].Alert != 2 || byInst[3].Status != StatusFinish {
		t.Errorf("取到的不是各实例最近一次结束的报告：%+v", byInst)
	}
	for level, want := range map[string]int{"alert": 1, "failed": 1, "clean": 1, "attention": 0, "cancelled": 0} {
		if _, n, _ := e.svc.ListReports(all, ReportQuery{Latest: true, Level: level}); int(n) != want {
			t.Errorf("latest + level=%s：%d，期望 %d", level, n, want)
		}
	}
	if _, n, _ := e.svc.ListReports(Scope{IDs: map[uint]bool{3: true}}, ReportQuery{Latest: true}); n != 1 {
		t.Errorf("latest + 范围过滤：%d", n)
	}
	// 首页一次要带回上千个实例的最近报告，所以 Latest 时上限放宽；不带 Latest 仍是 100
	if maxReportLimit(true) < 2000 || maxReportLimit(false) != 100 {
		t.Errorf("limit 上限：latest=%d 其它=%d", maxReportLimit(true), maxReportLimit(false))
	}
}

func TestGetReportViews(t *testing.T) {
	e := newEnv()
	my, _ := e.run(t, KindMySQL, 1, RunOptions{})
	ms, _ := e.run(t, KindMSSQL, 3, RunOptions{})

	v, err := e.svc.GetReport(all, my)
	if err != nil {
		t.Fatal(err)
	}
	if v.Report.HealthScore == nil || len(v.Sections) < 15 || v.Engine != "MySQL" {
		t.Fatalf("MySQL 视图：%+v", v.Report)
	}
	facts := map[string]string{}
	for _, f := range v.Facts {
		facts[f.Label] = f.Value
	}
	if facts["版本"] != "MySQL 8.0.36" || facts["运行时长"] != "0 天 1 小时" || !strings.Contains(facts["数据库 / 表"], "5 个库") {
		t.Errorf("摘要：%v", facts)
	}
	blocks := map[string][]string{}
	for _, b := range v.Blocks {
		blocks[b.Title] = b.Items
	}
	if len(blocks["需处理的问题"]) == 0 || len(blocks["需要关注"]) == 0 || len(blocks["优化建议"]) == 0 {
		t.Errorf("提示块：%+v", v.Blocks)
	}

	v, err = e.svc.GetReport(all, ms)
	if err != nil {
		t.Fatal(err)
	}
	if v.Report.HealthScore != nil || v.Engine != "SQL Server" || len(v.Sections) != len(msSections) {
		t.Fatalf("SQL Server 视图：%d 个分区 %+v", len(v.Sections), v.Report)
	}
	if v.Sections[0].Code != "overview" || v.Sections[0].Items[0].Table == nil || len(v.Sections[0].Items[0].Table.Columns) != 2 {
		t.Errorf("分区顺序或明细表：%+v", v.Sections[0].Items[0])
	}
	var top *Block
	for i := range v.Blocks {
		if strings.HasPrefix(v.Blocks[i].Title, "需处理的 Top") {
			top = &v.Blocks[i]
		}
	}
	if top == nil || !strings.Contains(strings.Join(top.Items, "\n"), "PLE") {
		t.Errorf("需处理的 Top：%+v", v.Blocks)
	}
	hasMem := false
	for _, s := range v.Sections {
		if s.Code == "memory" && s.Alert > 0 && s.Expanded {
			hasMem = true
		}
	}
	if !hasMem {
		t.Error("有告警的分区应默认展开")
	}

	// 范围外 / 不存在：404，不泄露存在性
	_, err = e.svc.GetReport(nobody, my)
	asErr(t, err, 404)
	_, err = e.svc.GetReport(all, ReportRef{KindMySQL, 999})
	asErr(t, err, 404)
}

func TestGetReportFailedAndLegacy(t *testing.T) {
	e := newEnv()
	e.openErr[2] = errors.New("无法连接")
	ref, _ := e.run(t, KindMySQL, 2, RunOptions{})
	v, err := e.svc.GetReport(all, ref)
	if err != nil || len(v.Blocks) != 1 || v.Blocks[0].Kind != "err" || !strings.Contains(v.Blocks[0].Items[0], "无法连接") {
		t.Fatalf("失败报告应显示原因：%+v %v", v, err)
	}
	// PolarDB 专用巡检生成的报告
	id := seedReport(e.repo, KindMySQL, 1, "polar", "polardb", StatusFinish, TriggerManual, 0, 0, "2026-10-01 00:00:00")
	e.repo.reports[ReportRef{KindMySQL, id}].Result = `{"engine":"polardb","polardb_version":"1.1","server":{"version":"8.0.13"},"inspection_report":{"health_score":77}}`
	v, err = e.svc.GetReport(all, ReportRef{KindMySQL, id})
	if err != nil || !strings.Contains(v.Note, "PolarDB 专用巡检") || !strings.Contains(v.Facts[0].Value, "PolarDB 1.1") {
		t.Errorf("专用引擎的报告应说明只展示通用分区：%+v %v", v.Note, err)
	}
	if v.Report.HealthScore == nil || *v.Report.HealthScore != 77 {
		t.Errorf("评分应取报告里的值：%v", v.Report.HealthScore)
	}
}

func TestShutdownWaitsThenAborts(t *testing.T) {
	e := newEnv()
	block := make(chan struct{})
	started := make(chan struct{})
	e.ft = func(model.Instance) *fakeTarget {
		ft := mysqlHappyTarget()
		ft.block, ft.started = block, started
		return ft
	}
	e.svc.RunNow(admin, all, []uint{1})
	<-started
	done := make(chan struct{})
	go func() { e.svc.Shutdown(100 * time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("超过等待时间后应中止在跑的巡检并返回")
	}
	r := e.repo.report(KindMySQL, 1)
	if r.Status != StatusFailed || !strings.Contains(r.Error, "服务退出") {
		t.Errorf("被中止的报告：%s %q", r.Status, r.Error)
	}
	// 没有在跑的任务时立即返回
	start := time.Now()
	newEnv().svc.Shutdown(10 * time.Second)
	if time.Since(start) > time.Second {
		t.Error("空闲时 Shutdown 不应等待")
	}
}

func TestManualConcurrencyLimited(t *testing.T) {
	e := newEnv()
	e.svc.sem = make(chan struct{}, 1)
	var running, peak atomic.Int32
	release := make(chan struct{})
	e.ft = func(model.Instance) *fakeTarget {
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
				<-release
				running.Add(-1)
			}
		}
		return ft
	}
	e.svc.RunNow(admin, all, []uint{1, 2})
	time.Sleep(100 * time.Millisecond)
	if n := running.Load(); n != 1 {
		t.Errorf("并发限制为 1 时同时只应有 1 个在跑，实际 %d", n)
	}
	// 排队中的报告已经是“进行中”
	if st := e.repo.report(KindMySQL, 2).Status; st != StatusRunning {
		t.Errorf("排队中的报告应显示进行中：%s", st)
	}
	close(release)
	e.waitIdle()
	if peak.Load() != 1 {
		t.Errorf("峰值并发 %d", peak.Load())
	}
	for _, id := range []uint{1, 2} {
		if st := e.repo.report(KindMySQL, id).Status; st != StatusFinish {
			t.Errorf("报告 %d：%s", id, st)
		}
	}
}

func TestInstancesOptions(t *testing.T) {
	e := newEnv()
	e.repo.insts[0].Tags = []model.InstanceTag{{TagCode: "mysql_inspection", TagName: "参与巡检", Active: true}, {TagCode: "off", Active: false}}
	e.repo.insts[0].ResourceGroups = []model.ResourceGroup{{GroupName: "研发组"}}
	list, tags, err := e.svc.Instances(all)
	if err != nil || len(list) != 3 { // redis 不支持
		t.Fatalf("%v %v", list, err)
	}
	if list[0].Name != "mssql-01" || list[0].Kind != "mssql" || list[1].Label != "MySQL" {
		t.Errorf("排序或类型：%+v", list)
	}
	if len(tags) != 1 || tags[0].Code != "mysql_inspection" {
		t.Errorf("只列启用的标签：%v", tags)
	}
	if got := list[1].Tags; len(got) != 1 || got[0] != "mysql_inspection" || list[1].Groups[0] != "研发组" {
		t.Errorf("%+v", list[1])
	}
	list, _, _ = e.svc.Instances(Scope{IDs: map[uint]bool{3: true}})
	if len(list) != 1 || list[0].ID != 3 {
		t.Errorf("范围过滤：%+v", list)
	}
}

func TestRefParsing(t *testing.T) {
	r, err := ParseRef("mssql-411")
	if err != nil || r.Kind != KindMSSQL || r.ID != 411 || r.String() != "mssql-411" {
		t.Errorf("%+v %v", r, err)
	}
	for _, bad := range []string{"", "410", "oracle-1", "mysql-", "mysql-0", "mysql-abc", "mysql-1-2", "mysql--1"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("应拒绝 %q", bad)
		}
	}
}

func TestKindOf(t *testing.T) {
	for db, want := range map[string]string{"mysql": KindMySQL, "polardb": KindMySQL, "polardb-x": KindMySQL, "doris": KindMySQL, "selectdb": KindMySQL, "adb": KindMySQL, "MSSQL": KindMSSQL, "mssql": KindMSSQL} {
		if got, ok := KindOf(db); !ok || got != want {
			t.Errorf("%s → %s %v", db, got, ok)
		}
	}
	for _, db := range []string{"redis", "mongo", "oracle", "pgsql", "clickhouse", ""} {
		if _, ok := KindOf(db); ok {
			t.Errorf("%s 暂不支持巡检", db)
		}
	}
}

var _ = context.Background
