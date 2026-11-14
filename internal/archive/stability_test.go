// @author: kevinkelly
// @license: Apache Licence
// @file: stability_test.go
// @time: 2026/10/28

package archive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/audit"
	"dbtool/internal/notify"
)

// 归档稳定性与运维修复（A4）的回归测试。

// S8-C11：通知里的任务链接必须是前端实际路由（main.js 的 parseHash：#task/ID 打开任务抽屉）。
func TestTaskLinkMatchesFrontendRoute(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("链接")
	want := fmt.Sprintf("https://dba.example.com/archive#task/%d", task.ID)
	if got := e.svc.taskLink(task); got != want {
		t.Fatalf("任务链接 = %q，应为 %q", got, want)
	}
	// 审批类通知里带的也是这个链接
	found := false
	for _, ev := range e.notif.evs {
		if strings.Contains(ev.Content, want) {
			found = true
		}
	}
	if !found {
		t.Errorf("审批通知正文里应包含 %s", want)
	}
}

// S8-B4：Worker 启动后立即回收租约已过期的运行（不等首轮 Tick 的 15 秒），并通知任务提交人。
func TestWorkerExpiresStaleRunsImmediatelyAndNotifiesCreator(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("启动回收")
	past := e.h.Clock.Now().Add(-5 * time.Minute)
	e.repo.runs[801] = &Run{ID: 801, TaskID: task.ID, TaskName: task.Name, ResourceGroupID: 1, InstanceID: 1, Trigger: TrigSchedule, Result: ResRunning, StartedAt: past, LeaseUntil: &past}
	before := len(e.notif.evs)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.svc.Worker(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(3 * time.Second) // firstDelay 是 15 秒；3 秒内能回收说明不是靠首轮 Tick
	for {
		r, _ := e.repo.GetRun(801)
		if r != nil && r.Result == ResInterrupted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Worker 启动后应立即回收过期租约的运行，不要等首轮 Tick")
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.notif.mu.Lock()
	defer e.notif.mu.Unlock()
	var got *notify.Event
	for i := before; i < len(e.notif.evs); i++ {
		if strings.Contains(e.notif.evs[i].Content, "#801") {
			got = &e.notif.evs[i]
		}
	}
	if got == nil {
		t.Fatalf("回收时应通知任务提交人，实际事件: %+v", e.notif.evs[before:])
	}
	if got.Applicant != task.Creator || got.Name != notify.EventException || got.Kind != "archive" {
		t.Errorf("通知应 @提交人并归入“执行异常”: %+v", got)
	}
	if !strings.Contains(got.Title, "中断") {
		t.Errorf("标题应说明运行被中断: %q", got.Title)
	}
}

// 未就绪（表没建）时 Worker 不碰库。
func TestWorkerStartupSkipsWhenNotReady(t *testing.T) {
	e := newEnv(t)
	e.repo.notReady = true
	if n := e.svc.ExpireStale(); n != 0 {
		t.Errorf("表没建时不回收: %d", n)
	}
}

// S8-B5：任务级互斥不能只看本进程内存；别的进程在跑（库里有租约未过期的运行记录）时，编辑 / 暂停 / 停用 / 删除都要 409。
func TestTaskMutationsBlockedByRunRowFromOtherProcess(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("跨进程互斥")
	now := e.h.Clock.Now()
	lease := now.Add(time.Minute)
	e.repo.runs[901] = &Run{ID: 901, TaskID: task.ID, TaskName: task.Name, InstanceID: 1, ResourceGroupID: 1, Result: ResRunning, StartedAt: now, LeaseUntil: &lease, LeaseOwner: "other-host:42"}
	if e.svc.liveOfTask(task.ID) != nil {
		t.Fatal("前提：本进程里没有这个运行")
	}

	cur, _ := e.repo.GetTask(task.ID, false)
	_, err := e.svc.UpdateTask(e.dba, task.ID, inputOf(cur))
	mustErrCode(t, err, 409, "running")
	_, err = e.svc.PauseTask(e.dba, task.ID, "")
	mustErrCode(t, err, 409, "running")
	_, err = e.svc.DisableTask(e.dba, task.ID, true)
	mustErrCode(t, err, 409, "running")
	e.repo.tasks[task.ID].Status = StDisabled
	mustErrCode(t, e.svc.DeleteTask(e.dba, task.ID, task.Name), 409, "running")
	if e.repo.tasks[task.ID] == nil || e.repo.tasks[task.ID].Status != StDisabled {
		t.Fatal("被拒绝的操作不应改动任务")
	}

	// 暂停状态的运行（paused）同样算“运行中”
	e.repo.runs[901].Result = ResPaused
	e.repo.tasks[task.ID].Status = StEnabled
	_, err = e.svc.PauseTask(e.dba, task.ID, "")
	mustErrCode(t, err, 409, "running")

	// 租约过期（进程已失联）的记录不拦：否则任务会被永久锁死
	past := now.Add(-time.Minute)
	e.repo.runs[901].LeaseUntil = &past
	if _, err := e.svc.PauseTask(e.dba, task.ID, ""); err != nil {
		t.Fatalf("租约已过期的运行不应阻止暂停: %v", err)
	}
	// 已结束的运行不拦
	e.repo.runs[901].Result = "success"
	if _, err := e.svc.ResumeTask(e.dba, task.ID); err != nil {
		t.Fatal(err)
	}
	future := now.Add(time.Hour)
	e.repo.runs[901].LeaseUntil = &future
	if _, err := e.svc.PauseTask(e.dba, task.ID, ""); err != nil {
		t.Fatalf("已结束的运行不应阻止暂停: %v", err)
	}
}

// 别的任务的运行不影响本任务。
func TestTaskMutationNotBlockedByOtherTasksRun(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("互斥-本任务")
	now := e.h.Clock.Now()
	lease := now.Add(time.Minute)
	e.repo.runs[902] = &Run{ID: 902, TaskID: task.ID + 999, InstanceID: 1, Result: ResRunning, StartedAt: now, LeaseUntil: &lease}
	if _, err := e.svc.PauseTask(e.dba, task.ID, ""); err != nil {
		t.Fatalf("别的任务的运行不应拦住本任务: %v", err)
	}
}

// S8-B3（运行时部分）：强杀后暂存目录里遗留的 *.tmp 是还没上传的备份。启动时扫描、在日志和运行记录里提示，绝不自动上传或删除。
func TestSpoolScanFlagsLeftoverTmpFilesWithoutTouchingThem(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("遗留暂存")
	dir := t.TempDir()
	t.Setenv("ARCHIVE_SPOOL_DIR", dir)
	now := e.h.Clock.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	e.repo.runs[801] = &Run{ID: 801, TaskID: task.ID, TaskName: task.Name, Result: ResInterrupted, StartedAt: past, FinishedAt: &past}
	e.repo.runs[802] = &Run{ID: 802, TaskID: task.ID, TaskName: task.Name, Result: ResRunning, StartedAt: now, LeaseUntil: &future}
	e.repo.runs[803] = &Run{ID: 803, TaskID: task.ID, TaskName: task.Name, Result: "success", StartedAt: past, FinishedAt: &past, Note: "原有说明"}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	f1 := write(fmt.Sprintf("t%d-r801-1700000000000000000-part-0001.tmp", task.ID), "aaaa")
	f1b := write(fmt.Sprintf("t%d-r801-1700000000000000000-part-0002.tmp", task.ID), "bb")
	f2 := write(fmt.Sprintf("t%d-r802-1700000000000000001-part-0001.tmp", task.ID), "cc")
	f3 := write(fmt.Sprintf("t%d-r803-1700000000000000002-part-0001.tmp", task.ID), "dddd")
	f4 := write("t1-r999-1700000000000000003-part-0001.tmp", "e") // 运行记录已不存在
	other := write("README.txt", "not a spool file")

	left := e.svc.scanSpoolAtStart()
	if len(left) != 5 {
		t.Fatalf("应发现 5 个遗留 .tmp（不含 README）：%+v", left)
	}
	r801, _ := e.repo.GetRun(801)
	if !strings.Contains(r801.Note, "暂存") || !strings.Contains(r801.Note, "2 个") || !strings.Contains(r801.Note, dir) {
		t.Errorf("中断的运行应提示遗留暂存文件：%q", r801.Note)
	}
	r803, _ := e.repo.GetRun(803)
	if !strings.HasPrefix(r803.Note, "原有说明") || !strings.Contains(r803.Note, "暂存") {
		t.Errorf("已结束运行的原有说明要保留并追加：%q", r803.Note)
	}
	r802, _ := e.repo.GetRun(802)
	if r802.Note != "" {
		t.Errorf("租约有效的运行（可能是别的进程正在写）不应被标注：%q", r802.Note)
	}
	for _, p := range []string{f1, f1b, f2, f3, f4, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("不得删除或移动暂存文件 %s：%v", p, err)
		}
	}
	if !contains(e.repo.auditActions(), auditPrefix+"暂存文件遗留") {
		t.Errorf("应写审计：%v", e.repo.auditActions())
	}
	// 重复扫描不重复追加
	e.svc.scanSpoolAtStart()
	r801, _ = e.repo.GetRun(801)
	if n := strings.Count(r801.Note, "遗留"); n != 1 {
		t.Errorf("提示只应出现一次：%q", r801.Note)
	}
}

func TestSpoolScanEmptyOrMissingDir(t *testing.T) {
	e := newEnv(t)
	t.Setenv("ARCHIVE_SPOOL_DIR", filepath.Join(t.TempDir(), "not-created"))
	if left := e.svc.scanSpoolAtStart(); len(left) != 0 {
		t.Fatalf("目录不存在不是错误：%+v", left)
	}
}

// S8-C4：GET /meta 公布给前端的取值范围必须与服务端的实际校验一致（前端只做即时提示，服务端为准）。
func TestPublishedLimitsMatchServerValidation(t *testing.T) {
	e := newEnv(t)
	pol := defaultPolicy()
	pol.AllowPurgeNoBackup = true
	now := e.h.Clock.Now()
	has := func(in TaskInput, field string) bool {
		for _, i := range in.issues(pol, now) {
			if i.Field == field && i.Level == "error" {
				return true
			}
		}
		return false
	}

	// 单文件上限
	if limits.PartMB != [2]int{engine.PartMBMin, engine.PartMBMax} {
		t.Errorf("part_mb 范围 %v 应等于引擎校验范围 [%d %d]", limits.PartMB, engine.PartMBMin, engine.PartMBMax)
	}
	file := func(mb int) TaskInput {
		in := e.baseInput("文件")
		in.DestTable = nil
		in.DestFile = &engine.FileDest{StoreID: 1, Format: "csv", PathTemplate: "{db}/{table}/{run_id}", PartMB: mb}
		return in
	}
	for _, mb := range []int{limits.PartMB[0], limits.PartMB[1]} {
		if has(file(mb), "dest_file.part_mb") {
			t.Errorf("part_mb=%d 在公布范围内，服务端不应拒绝", mb)
		}
	}
	if !has(file(limits.PartMB[1]+1), "dest_file.part_mb") {
		t.Errorf("part_mb 超出公布上限应被拒绝")
	}

	// 不备份理由
	purge := func(n int) TaskInput {
		in := e.baseInput("清理")
		in.Mode, in.DestTable = "purge", nil
		in.PurgeReason = strings.Repeat("理", n)
		return in
	}
	if limits.PurgeReason != [2]int{purgeReasonMin, purgeReasonMax} {
		t.Errorf("purge_reason 范围 %v", limits.PurgeReason)
	}
	for _, n := range []int{limits.PurgeReason[0], limits.PurgeReason[1]} {
		if has(purge(n), "purge_reason") {
			t.Errorf("理由 %d 字在公布范围内", n)
		}
	}
	for _, n := range []int{limits.PurgeReason[0] - 1, limits.PurgeReason[1] + 1} {
		if !has(purge(n), "purge_reason") {
			t.Errorf("理由 %d 字应被拒绝", n)
		}
	}

	// 任务级单次运行最多处理行数
	rows := func(n int64) TaskInput {
		in := e.baseInput("行数")
		in.Params.MaxRows = n
		return in
	}
	pol.MaxRowsPerRun = 0 // 不限，只看任务级下限
	if limits.RunMaxRows[0] != engine.RunMaxRowsMin {
		t.Errorf("run_max_rows 下限 %d 应等于引擎的 %d", limits.RunMaxRows[0], engine.RunMaxRowsMin)
	}
	if has(rows(int64(limits.RunMaxRows[0])), "params.max_rows") || !has(rows(int64(limits.RunMaxRows[0]-1)), "params.max_rows") {
		t.Errorf("任务级 max_rows 下限边界与公布值不一致")
	}

	// 安全策略的单次运行最大行数
	chk := func(n int64) bool {
		p := defaultPolicy()
		p.MaxRowsPerRun = n
		for _, x := range p.validate() {
			if x.Field == "max_rows_per_run" {
				return true
			}
		}
		return false
	}
	if chk(int64(limits.PolicyMaxRows[0])) || chk(int64(limits.PolicyMaxRows[1])) || !chk(int64(limits.PolicyMaxRows[0])-1) || !chk(int64(limits.PolicyMaxRows[1])+1) {
		t.Errorf("policy_max_rows %v 与 Policy.validate 不一致", limits.PolicyMaxRows)
	}

	// 备份存储的保留天数
	st := func(days int) bool {
		in := StoreInput{Name: "本地", Type: StoreLocal, Path: "/data/arch", RetentionDays: days}
		for _, x := range in.validate(true) {
			if x.Field == "retention_days" {
				return true
			}
		}
		return false
	}
	if limits.StoreRetentionMax != 36500 || st(limits.StoreRetentionMax) || !st(limits.StoreRetentionMax+1) || !st(-1) || st(0) {
		t.Errorf("store_retention_max=%d 与 StoreInput.validate 不一致", limits.StoreRetentionMax)
	}
}

// S8-C2：批间休眠填 0 在服务端会被当成“用策略默认值”（不是“不休眠”）。试运行要明确告诉用户，不能静默改值。
func TestZeroSleepIsReportedNotSilentlyReplaced(t *testing.T) {
	e := newEnv(t)
	in := e.baseInput("休眠0")
	in.Mode = "copy"
	in.Params.SleepMs = 0
	resp, err := e.svc.Check(context.Background(), e.rd, in)
	if err != nil {
		t.Fatal(err)
	}
	var item *engine.CheckItem
	for i := range resp.Report.Items {
		if resp.Report.Items[i].Key == "sleep" {
			item = &resp.Report.Items[i]
		}
	}
	if item == nil || item.Level != "warn" || !strings.Contains(item.Detail, "默认") || !strings.Contains(item.Detail, "200") {
		t.Fatalf("休眠 0 应在试运行里给出黄色说明（当作默认 200 ms）：%+v", item)
	}
	// 明确给了值就没有这条
	in.Params.SleepMs = 1
	resp, err = e.svc.Check(context.Background(), e.rd, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range resp.Report.Items {
		if it.Key == "sleep" {
			t.Fatalf("显式休眠值不应有该提示：%+v", it)
		}
	}
}

// S8-C1/C2：向导前端不再有“复制延迟检查”开关（服务端语义是不能关闭；开关关掉时发的 0 会被当作默认值），
// 批间休眠下限为 1，范围取自服务端公布的 limits。
func TestWizardFrontendMatchesServerSemantics(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "web", "static", "js", "archive", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	model, wiz := read("wizard_model.js"), read("wizard.js")
	for _, bad := range []string{"lag_on"} {
		if strings.Contains(model, bad) || strings.Contains(wiz, bad) {
			t.Errorf("向导前端不应再有 %q（复制延迟检查不能关闭）", bad)
		}
	}
	if !strings.Contains(model, "sleepMin") {
		t.Error("批间休眠下限应由 sleepMin 统一计算（至少 1，删除类模式取 50）")
	}
	for _, key := range []string{"purge_reason", "run_max_rows", "part_mb"} {
		if !strings.Contains(model, key) {
			t.Errorf("wizard_model.js 应使用服务端公布的 limits.%s", key)
		}
	}
	if !strings.Contains(read("stores.js"), "store_retention_max") || !strings.Contains(read("policy.js"), "policy_max_rows") {
		t.Error("存储保留天数 / 策略最大行数应使用服务端公布的 limits")
	}
}

// S9-A1：首页待办用的“等我审批”列表与角标同一套规则。
func TestPendingReviewListForHome(t *testing.T) {
	e := newEnv(t)
	in := e.checked(e.rd, e.baseInput("首页待办"))
	r, err := e.svc.CreateTask(e.rd, in)
	if err != nil {
		t.Fatal(err)
	}
	actor := func(c Caller) audit.Actor {
		return audit.Actor{ID: c.ID, Username: c.Username, Display: c.Display, IsSuperuser: c.IsSuperuser}
	}
	items, total, err := e.svc.PendingReviewList(actor(e.tl), 8)
	if err != nil || total != 1 || len(items) != 1 || items[0].ID != r.Task.ID || items[0].Name != "首页待办" || !strings.Contains(items[0].Sub, "shop.orders") {
		t.Fatalf("一级审批人应看到这张申请: %+v total=%d %v", items, total, err)
	}
	if n, _ := e.svc.PendingReview(actor(e.tl)); n != 1 {
		t.Errorf("角标数应与列表总数一致: %d", n)
	}
	// 提交人自己、没有审批权的人都不算
	for _, c := range []Caller{e.rd, e.viewer} {
		if _, total, _ := e.svc.PendingReviewList(actor(c), 8); total != 0 {
			t.Errorf("%s 不应看到待审批: %d", c.Username, total)
		}
	}
	// limit 只截断展示行数，不影响总数
	if items, total, _ := e.svc.PendingReviewList(actor(e.tl), 0); len(items) != 0 || total != 1 {
		t.Errorf("limit=0: %d %d", len(items), total)
	}
}
