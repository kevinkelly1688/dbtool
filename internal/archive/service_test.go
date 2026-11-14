// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package archive

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// ---- 权限 ----

func TestPermissionsEveryEntry(t *testing.T) {
	e := newEnv(t)
	none := caller(20, "nobody", false)
	task := e.enabled("t1")
	runID := uint(0)
	if _, err := e.svc.RunNow(e.dba, task.ID, true, true); err != nil {
		t.Fatal(err)
	}
	e.wait()
	for _, r := range e.repo.allRuns() {
		runID = r.ID
	}
	ctx := context.Background()
	calls := map[string]func() error{
		"meta-list": func() error { _, err := e.svc.ListTasks(none, TaskListQuery{}); return err },
		"get":       func() error { _, err := e.svc.GetTask(none, task.ID); return err },
		"create":    func() error { _, err := e.svc.CreateTask(e.viewer, e.baseInput("x")); return err },
		"update":    func() error { _, err := e.svc.UpdateTask(e.viewer, task.ID, e.baseInput("x")); return err },
		"check":     func() error { _, err := e.svc.Check(ctx, e.viewer, e.baseInput("x")); return err },
		"pause":     func() error { _, err := e.svc.PauseTask(e.rd, task.ID, ""); return err },
		"resume":    func() error { _, err := e.svc.ResumeTask(e.rd, task.ID); return err },
		"run":       func() error { _, err := e.svc.RunNow(e.rd, task.ID, true, true); return err },
		"disable":   func() error { _, err := e.svc.DisableTask(e.rd, task.ID, true); return err },
		"delete":    func() error { return e.svc.DeleteTask(e.rd, task.ID, task.Name) },
		"stop":      func() error { _, err := e.svc.StopRun(e.rd, runID, true); return err },
		"restore":   func() error { _, err := e.svc.Restore(e.rd, runID, task.Name); return err },
		"runs":      func() error { _, _, err := e.svc.ListRuns(none, RunQuery{}); return err },
		"progress":  func() error { _, err := e.svc.Progress(none, 0); return err },
		"stores":    func() error { _, err := e.svc.ListStores(e.rd); return err },
		"policy":    func() error { _, err := e.svc.GetPolicy(e.rd); return err },
		"preview":   func() error { _, err := e.svc.Preview(none, e.baseInput("x")); return err },
		"sources":   func() error { _, err := e.svc.SourceInstances(none); return err },
		"time-probe": func() error {
			_, err := e.svc.SourceTimeProbe(ctx, none, 1, "shop", "orders", "created_ms")
			return err
		},
		"store-edit": func() error {
			_, err := e.svc.CreateStore(e.dba, StoreInput{Name: "xx", Type: "local", Path: "/tmp"})
			return err
		},
		"policy-edit": func() error { _, err := e.svc.SavePolicy(e.dba, defaultPolicy(), true); return err },
	}
	for name, f := range calls {
		err := f()
		if err == nil {
			t.Errorf("%s: 无权限应被拒绝", name)
			continue
		}
		mustErrCode(t, err, 403, "forbidden")
	}
}

func TestLockedCodesNeedSuperuser(t *testing.T) {
	e := newEnv(t)
	// 即使把锁定码授予了普通用户，服务端也必须拒绝
	sneaky := caller(30, "sneaky", false, "archive.view", "archive.store_edit", "archive.policy_edit", "archive.cfg_view")
	_, err := e.svc.CreateStore(sneaky, StoreInput{Name: "s", Type: "local", Path: "/data/x"})
	mustErrCode(t, err, 403, "super_only")
	_, err = e.svc.TestStore(context.Background(), sneaky, 1)
	mustErrCode(t, err, 403, "super_only")
	_, err = e.svc.TestStoreDraft(context.Background(), sneaky, StoreInput{Name: "s", Type: "local", Path: "/data/x"})
	mustErrCode(t, err, 403, "super_only")
	_, err = e.svc.EnableStore(sneaky, 1, false)
	mustErrCode(t, err, 403, "super_only")
	mustErrCode(t, e.svc.DeleteStore(sneaky, 1), 403, "super_only")
	_, err = e.svc.UpdateStore(sneaky, 1, StoreInput{Name: "s", Type: "local", Path: "/data/x"})
	mustErrCode(t, err, 403, "super_only")
	_, err = e.svc.SavePolicy(sneaky, defaultPolicy(), true)
	mustErrCode(t, err, 403, "super_only")
	if p, _ := e.repo.GetPolicy(); p != nil {
		t.Error("不应写入策略")
	}
}

// ---- 缺表降级 ----

func TestNotReadyDegrades(t *testing.T) {
	e := newEnv(t)
	e.repo.notReady = true
	if m := e.svc.Meta(e.rd); m.Ready || m.Limits != nil || m.Me.Username != "rd_zhao" {
		t.Errorf("meta: %+v", m)
	}
	_, err := e.svc.ListTasks(e.rd, TaskListQuery{})
	mustErrCode(t, err, 503, "not_ready")
	_, err = e.svc.CreateTask(e.rd, e.baseInput("x"))
	mustErrCode(t, err, 503, "not_ready")
	_, _, err = e.svc.ListRuns(e.rd, RunQuery{})
	mustErrCode(t, err, 503, "not_ready")
	_, err = e.svc.ListStores(e.dba)
	mustErrCode(t, err, 503, "not_ready")
	_, err = e.svc.GetPolicy(e.dba)
	mustErrCode(t, err, 503, "not_ready")
	if n, err := e.svc.PendingReview(audit.Actor{ID: 6, Username: "tl_wang"}); err != nil || n != 0 {
		t.Errorf("角标: %d %v", n, err)
	}
	// 调度器：什么也不做
	e.svc.Tick(context.Background())
	if len(e.repo.runs) != 0 {
		t.Error("表未建时不应登记调度")
	}
	if p := e.svc.loadPolicy(); !p.IsDefault || p.MinRetentionDays != 30 {
		t.Errorf("默认策略: %+v", p)
	}
}

// ---- 创建 / 校验 / 互斥 ----

func TestCreateDraftAndValidation(t *testing.T) {
	e := newEnv(t)
	r, err := e.svc.CreateTask(e.rd, func() TaskInput { in := e.baseInput("草稿任务"); in.Submit = false; return in }())
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != "draft" || r.Task.Status != StDraft || r.Task.Params.BatchSize != 100 || !r.Task.Allow.Edit || r.Task.Allow.Approve {
		t.Errorf("草稿: %+v", r)
	}
	if r.Task.Where == "" || r.Task.SQL == nil || !strings.Contains(r.Task.SQL.Select, "shop") {
		t.Errorf("应带 WHERE 与 SQL: %+v", r.Task)
	}
	// 草稿不要求试运行，也不创建审批
	if len(e.repo.wfAudits) != 0 {
		t.Error("草稿不应创建审批")
	}

	bad := map[string]func(*TaskInput){
		"名称过短":      func(in *TaskInput) { in.Name = "a" },
		"名称重复":      func(in *TaskInput) { in.Name = "草稿任务" },
		"没选资源组":     func(in *TaskInput) { in.ResourceGroupID = 0 },
		"资源组与实例不匹配": func(in *TaskInput) { in.ResourceGroupID = 2 },
		"从库":        func(in *TaskInput) { in.InstanceID = 2 },
		"mssql":     func(in *TaskInput) { in.InstanceID = 3 },
		"保留天数过小":    func(in *TaskInput) { in.RetentionDays = 7 },
		"受保护库":      func(in *TaskInput) { in.DB = "mysql" },
		"附加条件注入":    func(in *TaskInput) { in.ExtraWhere = "1=1; DROP TABLE x" },
		"无备份去向":     func(in *TaskInput) { in.DestTable, in.DestFile = nil, nil },
		"仅清理被策略禁止": func(in *TaskInput) {
			in.Mode, in.PurgeReason, in.DestTable = "purge", "测试环境临时数据清理", nil
		},
		"存储不存在": func(in *TaskInput) {
			in.DestFile = &engine.FileDest{StoreID: 99, Format: "csv", PathTemplate: "{db}/{table}"}
		},
		"cron 太密": func(in *TaskInput) { in.Schedule = Schedule{Kind: "cron", Cron: "*/10 * * * *"} },
		"窗口太短":    func(in *TaskInput) { in.Params.Window = engine.Window{Start: "02:00", End: "02:10"} },
	}
	for name, mod := range bad {
		in := e.baseInput("另一个任务")
		mod(&in)
		_, err := e.svc.CreateTask(e.rd, in)
		var pe *Error
		if err == nil || !asErr(err, &pe) || pe.Code != "validation" {
			t.Errorf("%s: 应校验失败: %v", name, err)
		}
	}
	// 调用者看不见的实例
	in := e.baseInput("viewer 的任务")
	v := caller(8, "viewer", false, "archive.view", "archive.apply")
	e.repo.userRGs[8] = []uint{1}
	in.InstanceID = 4
	_, err = e.svc.CreateTask(v, in)
	if pe := mustErrCode2(t, err, 400, "validation"); !strings.Contains(pe.Message, "未关联") {
		t.Errorf("无权访问实例: %s", pe.Message)
	}
	// 不在资源组
	out := caller(21, "outsider", false, "archive.view", "archive.apply")
	_, err = e.svc.CreateTask(out, e.baseInput("外人"))
	if pe := mustErrCode2(t, err, 400, "validation"); !strings.Contains(pe.Message, "不属于该资源组") {
		t.Errorf("非资源组成员: %s", pe.Message)
	}
}

func asErr(err error, target **Error) bool {
	pe, ok := err.(*Error)
	if ok {
		*target = pe
	}
	return ok
}

func mustErrCode2(t *testing.T, err error, status int, code string) *Error {
	t.Helper()
	return mustErrCode(t, err, status, code)
}

func TestSubmitNeedsCheckToken(t *testing.T) {
	e := newEnv(t)
	in := e.baseInput("需要试运行")
	in.Submit = true
	_, err := e.svc.CreateTask(e.rd, in)
	mustErrCode(t, err, 422, "check_required")

	in.CheckToken = "no-such-token"
	_, err = e.svc.CreateTask(e.rd, in)
	mustErrCode(t, err, 422, "check_required")

	ok := e.checked(e.rd, e.baseInput("需要试运行"))

	// 试运行之后改了关键字段
	changed := ok
	changed.RetentionDays = 120
	_, err = e.svc.CreateTask(e.rd, changed)
	mustErrCode(t, err, 422, "check_mismatch")
	changed = ok
	changed.ExtraWhere = "status = 3"
	_, err = e.svc.CreateTask(e.rd, changed)
	mustErrCode(t, err, 422, "check_mismatch")

	// 只改调度 / 参数不影响 token
	sched := ok
	sched.Schedule = Schedule{Kind: "day", Time: "03:00"}
	sched.Params.BatchSize = 50 + 50
	if _, err := e.svc.CreateTask(e.rd, sched); err != nil {
		t.Fatalf("调度 / 参数变化不应使 token 失效: %v", err)
	}
}

func TestCheckTokenExpiryAckAndBlocked(t *testing.T) {
	e := newEnv(t)
	// 过期
	in := e.checked(e.rd, e.baseInput("过期"))
	e.h.Clock.Advance(7*24*time.Hour + time.Minute)
	_, err := e.svc.CreateTask(e.rd, in)
	mustErrCode(t, err, 422, "check_expired")

	// 警告 / 大批量需要确认
	in2 := e.baseInput("需确认")
	resp, err := e.svc.Check(context.Background(), e.rd, in2)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.NeedAckBig || resp.NeedAckWarn == 0 || resp.Blocked {
		t.Fatalf("应需要确认: %+v", resp)
	}
	in2.Submit, in2.CheckToken = true, resp.CheckToken
	_, err = e.svc.CreateTask(e.rd, in2)
	pe := mustErrCode(t, err, 422, "ack_required")
	if d, _ := pe.Data.(map[string]any); d == nil || d["need_big"] != true {
		t.Errorf("ack_required 数据: %+v", pe.Data)
	}
	in2.AckWarn = true
	_, err = e.svc.CreateTask(e.rd, in2)
	mustErrCode(t, err, 422, "ack_required")
	in2.AckBig = true
	if _, err := e.svc.CreateTask(e.rd, in2); err != nil {
		t.Fatalf("全部确认后应可提交: %v", err)
	}

	// 有红色项（源表没有主键）不能提交
	e2 := newEnv(t)
	e2.h.Src.Info.PrimaryKey = nil
	e2.h.Src.Info.Indexes = map[string][]string{}
	bad := e2.baseInput("无主键")
	resp, err = e2.svc.Check(context.Background(), e2.rd, bad)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Blocked {
		t.Fatalf("无主键应被阻断: %+v", resp.Report.Items)
	}
	bad.Submit, bad.CheckToken, bad.AckWarn, bad.AckBig = true, resp.CheckToken, true, true
	_, err = e2.svc.CreateTask(e2.rd, bad)
	mustErrCode(t, err, 422, "check_failed")
	// 试运行记录
	var dry int
	for _, r := range e2.repo.allRuns() {
		if r.Trigger == TrigDryrun && r.Result == engine.ResultFailed && r.CheckID != 0 {
			dry++
		}
	}
	if dry != 1 {
		t.Errorf("试运行应记一条 dryrun 运行: %d", dry)
	}
}

func TestTableBusy(t *testing.T) {
	e := newEnv(t)
	e.enabled("第一个")
	in := e.checked(e.rd, e.baseInput("第二个"))
	_, err := e.svc.CreateTask(e.rd, in)
	pe := mustErrCode(t, err, 409, "table_busy")
	if d, _ := pe.Data.(map[string]any); d == nil || d["name"] != "第一个" {
		t.Errorf("table_busy 数据: %+v", pe.Data)
	}
	// 草稿不占坑
	d := e.baseInput("草稿")
	if _, err := e.svc.CreateTask(e.rd, d); err != nil {
		t.Errorf("草稿不应受互斥限制: %v", err)
	}
	// 停用后可以再建
	first := e.repo.tasks[101]
	if first == nil {
		for _, tk := range e.repo.tasks {
			if tk.Name == "第一个" {
				first = tk
			}
		}
	}
	if _, err := e.svc.DisableTask(e.dba, first.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateTask(e.rd, in); err != nil {
		t.Errorf("停用后应能新建: %v", err)
	}
}

// ---- 审批 ----

func TestApprovalFlow(t *testing.T) {
	e := newEnv(t)
	in := e.checked(e.rd, e.baseInput("审批流"))
	r, err := e.svc.CreateTask(e.rd, in)
	if err != nil {
		t.Fatal(err)
	}
	id := r.Task.ID
	if r.Outcome != "submitted" || r.Task.Status != StPending || r.Task.Approval == nil || r.Task.Approval.LevelCount != 2 || !strings.Contains(r.Task.Approval.PendingText, "团队负责人") {
		t.Fatalf("提交后: %+v %+v", r.Task.Status, r.Task.Approval)
	}
	if !contains(e.notif.names(), "pending") {
		t.Errorf("应通知待审批: %v", e.notif.names())
	}
	// 不能审批自己提的（包括超级用户自己提的）
	_, err = e.svc.Approve(e.rd, id, "")
	mustErrCode(t, err, 409, "self_approve")
	// 一级审批人不是 dba：二级人不能越级
	_, err = e.svc.Approve(e.dba, id, "")
	mustErrCode(t, err, 403, "forbidden")
	// 无关用户
	_, err = e.svc.Approve(e.viewer, id, "")
	mustErrCode(t, err, 403, "forbidden")
	// allow 标志
	if tv, _ := e.svc.GetTask(e.tl, id); !tv.Allow.Approve || !tv.Allow.Reject {
		t.Errorf("tl 应可审批: %+v", tv.Allow)
	}
	if tv, _ := e.svc.GetTask(e.dba, id); tv.Allow.Approve {
		t.Error("dba 在第一级不应可审批")
	}
	if n, _ := e.svc.PendingReview(e.tl.actor()); n != 1 {
		t.Errorf("tl 的角标应为 1: %d", n)
	}
	if n, _ := e.svc.PendingReview(e.dba.actor()); n != 0 {
		t.Errorf("dba 的角标应为 0: %d", n)
	}
	lst, _ := e.svc.ListTasks(e.tl, TaskListQuery{Mine: true})
	if lst.Total != 1 || lst.KPI.PendingMine != 1 {
		t.Errorf("待我审批: %+v", lst.KPI)
	}
	if lst, _ = e.svc.ListTasks(e.dba, TaskListQuery{Mine: true}); lst.Total != 0 {
		t.Errorf("dba 的待我审批应为空: %d", lst.Total)
	}

	v, err := e.svc.Approve(e.tl, id, "同意")
	if err != nil || v.Status != StPending || v.Approval.CurrentLevel != 1 {
		t.Fatalf("一级通过: %v %+v", err, v)
	}
	if n, _ := e.svc.PendingReview(e.dba.actor()); n != 1 {
		t.Errorf("进入二级后 dba 角标为 1: %d", n)
	}
	v, err = e.svc.Approve(e.dba, id, "试运行 OK")
	if err != nil || v.Status != StEnabled || v.NextRunAt == "" {
		t.Fatalf("二级通过应启用: %v %+v", err, v)
	}
	if got := e.notif.names(); !contains(got, "passed") {
		t.Errorf("应通知通过: %v", got)
	}
	// 已启用后再审批 → 冲突
	_, err = e.svc.Approve(e.dba, id, "")
	mustErrCode(t, err, 409, "conflict")

	d, err := e.svc.GetTask(e.rd, id)
	if err != nil {
		t.Fatal(err)
	}
	ap := d.ApprovalDetail
	if ap == nil || ap.Status != "passed" || len(ap.Steps) != 2 || ap.Steps[0].State != "approved" || ap.Steps[0].By != "tl_wang" || ap.Steps[1].By != "dba_li" || ap.Applicant != "rd_zhao" {
		t.Errorf("审批时间线: %+v", ap)
	}
	if len(d.History) < 3 {
		t.Errorf("变更历史: %+v", d.History)
	}
	acts := e.repo.auditActions()
	for _, want := range []string{"数据归档-试运行", "数据归档-提交申请", "数据归档-审批通过"} {
		if !contains(acts, want) {
			t.Errorf("缺审计 %s: %v", want, acts)
		}
	}
}

func TestSuperuserCannotApproveOwn(t *testing.T) {
	e := newEnv(t)
	e.super.Can = nil
	in := e.checked(e.super, e.baseInput("超管自提"))
	r, err := e.svc.CreateTask(e.super, in)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Approve(e.super, r.Task.ID, "")
	mustErrCode(t, err, 409, "self_approve")
	// 超管可以审批别人提的
	in = e.checked(e.rd, func() TaskInput { i := e.baseInput("别人的"); i.Table = "orders"; return i }())
	if _, err := e.svc.DisableTask(e.super, r.Task.ID, true); err != nil {
		t.Fatal(err)
	}
	r2, err := e.svc.CreateTask(e.rd, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Approve(e.super, r2.Task.ID, ""); err != nil {
		t.Errorf("超管可审批他人申请: %v", err)
	}
}

func TestRejectAndResubmit(t *testing.T) {
	e := newEnv(t)
	in := e.checked(e.rd, e.baseInput("被驳回"))
	r, err := e.svc.CreateTask(e.rd, in)
	if err != nil {
		t.Fatal(err)
	}
	id := r.Task.ID
	_, err = e.svc.Reject(e.tl, id, " ")
	mustErrCode(t, err, 400, "bad_request")
	v, err := e.svc.Reject(e.tl, id, "时间列没有索引")
	if err != nil || v.Status != StRejected || v.RejectReason != "时间列没有索引" {
		t.Fatalf("驳回: %v %+v", err, v)
	}
	if !contains(e.notif.names(), "rejected") {
		t.Errorf("应通知驳回: %v", e.notif.names())
	}
	d, _ := e.svc.GetTask(e.rd, id)
	if d.ApprovalDetail.Reject == nil || d.ApprovalDetail.Reject.By != "tl_wang" || d.ApprovalDetail.Steps[0].State != "rejected" || d.ApprovalDetail.Steps[1].State != "skipped" {
		t.Errorf("驳回时间线: %+v", d.ApprovalDetail)
	}
	if !d.Allow.Edit || d.Allow.Run {
		t.Errorf("已驳回的 allow: %+v", d.Allow)
	}
	// 重新提交：同一条审批记录重置，只展示最近一轮
	before := len(e.repo.wfAudits)
	e.h.Clock.Advance(time.Minute)
	cur, _ := e.repo.GetTask(id, false)
	in2 := inputOf(cur)
	in2.RetentionDays = 100
	in2 = e.checked(e.rd, in2)
	in2.Version = d.Version
	r2, err := e.svc.UpdateTask(e.rd, id, in2)
	if err != nil || r2.Outcome != "submitted" || r2.Task.Status != StPending {
		t.Fatalf("重新提交: %v %+v", err, r2)
	}
	if len(e.repo.wfAudits) != before {
		t.Errorf("应复用同一条审批记录: %d -> %d", before, len(e.repo.wfAudits))
	}
	d, _ = e.svc.GetTask(e.rd, id)
	if d.ApprovalDetail.Status != "waiting" || d.ApprovalDetail.Steps[0].State != "pending" || d.ApprovalDetail.Reject != nil {
		t.Errorf("第二轮时间线: %+v", d.ApprovalDetail)
	}
	if d.RejectReason != "" {
		t.Error("重新提交应清空驳回原因")
	}
	// 走完审批
	e.svc.Approve(e.tl, id, "")
	if v, err := e.svc.Approve(e.dba, id, ""); err != nil || v.Status != StEnabled {
		t.Fatalf("第二轮通过: %v", err)
	}
}

func TestNoApprovalFlowConfigured(t *testing.T) {
	e := newEnv(t)
	delete(e.repo.settings, [2]int{1, audit.TypeArchive})
	in := e.baseInput("无审批流")
	// 试运行里就有红色的 flow 项
	resp, err := e.svc.Check(context.Background(), e.rd, in)
	if err != nil || !resp.Blocked {
		t.Fatalf("无审批流应被试运行阻断: %v %+v", err, resp)
	}
	// 直接伪造一个通过的 token（绕过 Check 里的阻断）仍然被 no_flow 拦住
	e.repo.settings[[2]int{1, audit.TypeArchive}] = newSetting()
	ok := e.checked(e.rd, in)
	delete(e.repo.settings, [2]int{1, audit.TypeArchive})
	_, err = e.svc.CreateTask(e.rd, ok)
	pe := mustErrCode(t, err, 422, "no_flow")
	if !strings.Contains(pe.Message, "订单组") {
		t.Errorf("提示应带资源组名: %s", pe.Message)
	}
	// 事务回滚：没有残留任务
	if len(e.repo.tasks) != 0 {
		t.Errorf("提交失败不应留下任务: %d", len(e.repo.tasks))
	}
	// 空审批流同样不行
	e.repo.settings[[2]int{1, audit.TypeArchive}] = newSetting()
	e.repo.settings[[2]int{1, audit.TypeArchive}].AuditAuthGroups = ""
	_, err = e.svc.CreateTask(e.rd, ok)
	mustErrCode(t, err, 422, "no_flow")
}

// ---- 修改：放宽 / 收紧 ----

func TestUpdateConservativeVsLoosen(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("可调参数")
	get := func() TaskInput {
		cur, _ := e.repo.GetTask(task.ID, false)
		in := inputOf(cur)
		in.Version = cur.Version
		return in
	}
	// 收紧：批量减小、休眠增大、窗口缩小、总量降低 → 直接保存
	in := get()
	in.Params.BatchSize, in.Params.SleepMs, in.Params.MaxRows = 50+50, 300, 1000000
	in.Params.Window = engine.Window{Start: "02:00", End: "04:00"}
	in.Schedule = Schedule{Kind: "day", Time: "03:00"}
	r, err := e.svc.UpdateTask(e.dba, task.ID, in)
	if err != nil || r.Outcome != "saved" || r.Task.Status != StEnabled || r.Task.ScheduleText != "每天 03:00" {
		t.Fatalf("收紧应直接保存: %v %+v", err, r)
	}
	if len(r.Changes) == 0 {
		t.Error("应列出变更")
	}
	if cur, _ := e.repo.GetTask(task.ID, false); cur.NextRunAt == nil || cur.NextRunAt.Hour() != 3 {
		t.Errorf("调度改了，下次运行时间应重算: %v", cur.NextRunAt)
	}
	// 放宽：批量增大 → 需要审批，不带 token 且 submit=false → 冲突
	in = get()
	in.Params.BatchSize = 500
	_, err = e.svc.UpdateTask(e.dba, task.ID, in)
	mustErrCode(t, err, 409, "conflict")
	// 带试运行 token 提交 → 重新进入待审批
	in = get()
	in.Params.BatchSize = 500
	in = e.checked(e.dba, in)
	in.Version = get().Version
	r, err = e.svc.UpdateTask(e.dba, task.ID, in)
	if err != nil || r.Outcome != "submitted" || r.Task.Status != StPending {
		t.Fatalf("放宽应重新审批: %v %+v", err, r)
	}
	// 待审批的任务不能立即执行
	_, err = e.svc.RunNow(e.dba, task.ID, true, true)
	mustErrCode(t, err, 409, "skipped")
}

func TestNeedsApprovalRules(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("规则")
	cur, _ := e.repo.GetTask(task.ID, false)
	old := inputOf(cur)
	same := old
	if need, _ := needsApproval(cur, old, same); need {
		t.Error("无变化不需要审批")
	}
	for name, mod := range map[string]func(*TaskInput){
		"保留天数": func(i *TaskInput) { i.RetentionDays = 91 },
		"附加条件": func(i *TaskInput) { i.ExtraWhere = "status=1" },
		"模式":   func(i *TaskInput) { i.Mode = "copy" },
		"备份去向": func(i *TaskInput) { i.DestTable.Template = "other_{yyyy}" },
		"总量上限": func(i *TaskInput) { i.Params.MaxRows = old.Params.MaxRows + 1 },
		"批量增大": func(i *TaskInput) { i.Params.BatchSize = old.Params.BatchSize + 1 },
		"休眠减小": func(i *TaskInput) { i.Params.SleepMs = old.Params.SleepMs - 1 },
		"窗口扩大": func(i *TaskInput) { i.Params.Window.End = "06:00" },
		"阈值放宽": func(i *TaskInput) { i.Params.MaxThreadsRunning = old.Params.MaxThreadsRunning + 1 },
		"资源组":  func(i *TaskInput) { i.ResourceGroupID = 2 },
	} {
		n := old
		if old.DestTable != nil {
			d := *old.DestTable
			n.DestTable = &d
		}
		mod(&n)
		if need, _ := needsApproval(cur, old, n); !need {
			t.Errorf("%s 应需要审批", name)
		}
	}
	for name, mod := range map[string]func(*TaskInput){
		"总量降低": func(i *TaskInput) { i.Params.MaxRows = old.Params.MaxRows - 1 },
		"批量减小": func(i *TaskInput) { i.Params.BatchSize = old.Params.BatchSize - 1 },
		"休眠增大": func(i *TaskInput) { i.Params.SleepMs = old.Params.SleepMs + 100 },
		"窗口缩小": func(i *TaskInput) { i.Params.Window.End = "04:00" },
		"调度":   func(i *TaskInput) { i.Schedule = Schedule{Kind: "day", Time: "04:00"} },
		"改名":   func(i *TaskInput) { i.Name = "改名后" },
		"阈值收紧": func(i *TaskInput) { i.Params.MaxThreadsRunning = old.Params.MaxThreadsRunning - 1 },
	} {
		n := old
		mod(&n)
		if need, _ := needsApproval(cur, old, n); need {
			t.Errorf("%s 更保守，不应需要审批", name)
		}
	}
	for _, st := range []string{StRejected, StDisabled, StDraft, StPending} {
		c2 := *cur
		c2.Status = st
		if need, _ := needsApproval(&c2, old, old); !need {
			t.Errorf("状态 %s 需要提交审批", st)
		}
	}
	c3 := *cur
	c3.Status, c3.PauseKind = StPaused, PauseStructure
	if need, _ := needsApproval(&c3, old, old); !need {
		t.Error("结构变更暂停需要审批")
	}
	c3.PauseKind = PauseManual
	if need, _ := needsApproval(&c3, old, old); need {
		t.Error("人工暂停的任务保守修改不需审批")
	}
}

func TestVersionConflictAndRunningEdit(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("并发")
	cur, _ := e.repo.GetTask(task.ID, false)
	in := inputOf(cur)
	in.Version = cur.Version + 5
	_, err := e.svc.UpdateTask(e.dba, task.ID, in)
	mustErrCode(t, err, 409, "version_conflict")
}

// ---- 暂停 / 恢复 / 停用 / 删除 ----

func TestPauseResumeDisableDelete(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("生命周期")
	v, err := e.svc.PauseTask(e.dba, task.ID, "")
	if err != nil || v.Status != StPaused || v.Pause == nil || v.Pause.Kind != PauseManual || v.NextRunAt != "" {
		t.Fatalf("暂停: %v %+v", err, v)
	}
	_, err = e.svc.PauseTask(e.dba, task.ID, "")
	mustErrCode(t, err, 409, "conflict")
	if v, err = e.svc.ResumeTask(e.dba, task.ID); err != nil || v.Status != StEnabled || v.NextRunAt == "" || v.Pause != nil {
		t.Fatalf("恢复: %v %+v", err, v)
	}
	// 停用需要 confirm
	_, err = e.svc.DisableTask(e.dba, task.ID, false)
	mustErrCode(t, err, 422, "confirm_required")
	if v, err = e.svc.DisableTask(e.dba, task.ID, true); err != nil || v.Status != StDisabled {
		t.Fatalf("停用: %v %+v", err, v)
	}
	// 删除需要输入任务名
	mustErrCode(t, e.svc.DeleteTask(e.dba, task.ID, "不对"), 422, "confirm_required")
	if err := e.svc.DeleteTask(e.dba, task.ID, "生命周期"); err != nil {
		t.Fatal(err)
	}
	if e.repo.tasks[task.ID] != nil {
		t.Error("应已删除")
	}
	_, err = e.svc.GetTask(e.dba, task.ID)
	mustErrCode(t, err, 404, "not_found")
	// 已启用的任务不能直接删
	t2 := e.enabled("不能直接删")
	mustErrCode(t, e.svc.DeleteTask(e.dba, t2.ID, "不能直接删"), 409, "conflict")
}

func TestDisablePendingAbortsApproval(t *testing.T) {
	e := newEnv(t)
	in := e.checked(e.rd, e.baseInput("待审批被停用"))
	r, _ := e.svc.CreateTask(e.rd, in)
	if _, err := e.svc.DisableTask(e.dba, r.Task.ID, true); err != nil {
		t.Fatal(err)
	}
	if a := e.repo.wfAudits[0]; a.CurrentStatus != audit.StatusAborted {
		t.Errorf("审批应作废: %d", a.CurrentStatus)
	}
	_, err := e.svc.Approve(e.tl, r.Task.ID, "")
	mustErrCode(t, err, 409, "conflict")
}

func TestVisibilityByResourceGroup(t *testing.T) {
	e := newEnv(t)
	e.enabled("订单组任务")
	other := caller(40, "other", false, "archive.view")
	e.repo.userRGs[40] = []uint{2}
	e.repo.rgs[2] = e.repo.rgs[2]
	lst, err := e.svc.ListTasks(other, TaskListQuery{})
	if err != nil || lst.Total != 0 {
		t.Errorf("别的资源组看不到: %v %+v", err, lst)
	}
	_, err = e.svc.GetTask(other, 101)
	mustErrCode(t, err, 404, "not_found")
	all := caller(41, "admin2", false, "archive.view", "query.admin_run")
	if lst, _ = e.svc.ListTasks(all, TaskListQuery{}); lst.Total != 1 {
		t.Errorf("query.admin_run 看全部: %d", lst.Total)
	}
	if lst, _ = e.svc.ListTasks(e.super, TaskListQuery{}); lst.Total != 1 || lst.KPI.Enabled != 1 || len(lst.Instances) != 1 {
		t.Errorf("超管: %+v", lst)
	}
	if lst, _ = e.svc.ListTasks(e.rd, TaskListQuery{Status: "paused"}); lst.Total != 0 || lst.KPI.Total != 1 {
		t.Errorf("状态筛选不应影响 KPI: %+v", lst)
	}
	if lst, _ = e.svc.ListTasks(e.rd, TaskListQuery{Q: "shop.ord"}); lst.Total != 1 {
		t.Errorf("按库表搜索: %d", lst.Total)
	}
}

// ---- 策略 ----

func TestPolicyDiffAndSave(t *testing.T) {
	e := newEnv(t)
	p, err := e.svc.GetPolicy(e.dba)
	if err != nil || !p.IsDefault || p.MinRetentionDays != 30 || p.AllowPurgeNoBackup || p.MaxRowsPerRun != 5000000 || p.MaxConcurrentPerInstance != 2 || len(p.ProtectedPatterns) != 4 {
		t.Fatalf("默认策略: %v %+v", err, p)
	}
	n := *p
	n.MinRetentionDays, n.AllowPurgeNoBackup = 7, true
	n.ProtectedPatterns = []string{"audit_db.*", "tmp?"}
	d, err := e.svc.DiffPolicy(e.dba, n)
	if err != nil || len(d.Errors) != 0 || len(d.Changes) < 3 || len(d.Risks) < 2 {
		t.Fatalf("diff: %v %+v", err, d)
	}
	// 系统库恒受保护：即使新值里没有它们
	if len(n.validate()) != 0 || !contains(n.ProtectedPatterns, "mysql") {
		t.Errorf("内置保护库应被补回: %v", n.ProtectedPatterns)
	}
	// 非法值
	bad := *p
	bad.MinRetentionDays, bad.Defaults.BatchSize, bad.ProtectedPatterns = 0, 5, []string{"a b"}
	d, _ = e.svc.DiffPolicy(e.dba, bad)
	if len(d.Errors) != 3 {
		t.Errorf("应有 3 个错误: %+v", d.Errors)
	}
	_, err = e.svc.SavePolicy(e.super, bad, true)
	mustErrCode(t, err, 400, "validation")
	// 需要二次确认
	_, err = e.svc.SavePolicy(e.super, n, false)
	mustErrCode(t, err, 422, "confirm_required")
	saved, err := e.svc.SavePolicy(e.super, n, true)
	if err != nil || saved.IsDefault || saved.MinRetentionDays != 7 || saved.UpdatedBy != "root" {
		t.Fatalf("保存: %v %+v", err, saved)
	}
	var found bool
	for _, a := range e.repo.audits {
		if a.Action == "数据归档-策略修改" && strings.Contains(a.ExtraInfo, "最小保留天数：30 → 7") {
			found = true
		}
	}
	if !found {
		t.Errorf("策略修改应写审计（含差异）: %v", e.repo.auditActions())
	}
	// 新策略立即影响校验：允许 7 天保留
	in := e.baseInput("七天")
	in.RetentionDays = 7
	if _, err := e.svc.Check(context.Background(), e.rd, in); err != nil {
		t.Errorf("策略放宽后 7 天应能试运行: %v", err)
	}
}

func TestPolicyMinRetentionBlocksRun(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("策略收紧")
	p := e.svc.loadPolicy()
	p.MinRetentionDays = 365
	if _, err := e.svc.SavePolicy(e.super, p, true); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.RunNow(e.dba, task.ID, true, true)
	pe := mustErrCode(t, err, 409, "skipped")
	if !strings.Contains(pe.Message, "最小保留天数") {
		t.Errorf("提示: %s", pe.Message)
	}
}

// ---- 备份存储 ----

func TestStoreLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for name, in := range map[string]StoreInput{
		"相对路径":          {Name: "本地盘2", Type: "local", Path: "data/archive"},
		"路径穿越":          {Name: "本地盘2", Type: "local", Path: "/data/../etc"},
		"s3 缺 endpoint": {Name: "oss1", Type: "oss", Bucket: "b", AK: "ak", SK: "sk"},
		"s3 缺 SK":       {Name: "oss1", Type: "s3", Endpoint: "http://minio:9000", Bucket: "b", AK: "ak"},
		"类型":            {Name: "xx", Type: "ftp"},
		"重名":            {Name: "本地归档盘", Type: "local", Path: "/data/x"},
		"名称过短":          {Name: "a", Type: "local", Path: "/data/x"},
		"前缀穿越":          {Name: "p1", Type: "local", Path: "/data/x", Prefix: "../x"},
	} {
		_, err := e.svc.CreateStore(e.super, in)
		var pe *Error
		if err == nil || !asErr(err, &pe) || pe.Code != "validation" {
			t.Errorf("%s: 应校验失败: %v", name, err)
		}
	}
	v, err := e.svc.CreateStore(e.super, StoreInput{Name: "MinIO", Type: "s3", Endpoint: "http://minio:9000/", Bucket: "dba", AK: "minioadmin01", SK: "secret-sk", PathStyle: true, Prefix: "/prod/"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Endpoint != "http://minio:9000" || v.Prefix != "prod" || !v.SKSet || v.AK != "minioadmin01" || v.Status != "unknown" {
		t.Errorf("创建: %+v", v)
	}
	// SK 永不回显
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "secret-sk") {
		t.Errorf("响应不应含 SK: %s", raw)
	}
	// 没有 store_edit 的人看到脱敏的 AK
	list, err := e.svc.ListStores(e.dba)
	if err != nil || len(list) != 2 {
		t.Fatalf("列表: %v %d", err, len(list))
	}
	for _, s := range list {
		if s.Name == "MinIO" && (s.AK != "mini****n01" || !s.SKSet) {
			t.Errorf("AK 应脱敏: %+v", s)
		}
	}
	// 编辑：SK 留空 = 不改
	v2, err := e.svc.UpdateStore(e.super, v.ID, StoreInput{Name: "MinIO", Type: "s3", Endpoint: "http://minio:9000", Bucket: "dba2", AK: "minioadmin01", RetentionDays: 30})
	if err != nil || v2.Bucket != "dba2" || !v2.SKSet || v2.RetentionDays != 30 {
		t.Fatalf("编辑: %v %+v", err, v2)
	}
	if row, _ := e.repo.GetStore(v.ID); row.SK != "secret-sk" {
		t.Errorf("SK 留空不应修改: %q", row.SK)
	}
	if _, err = e.svc.UpdateStore(e.super, v.ID, StoreInput{Name: "MinIO", Type: "s3", Endpoint: "http://minio:9000", Bucket: "dba2", AK: "minioadmin01", SK: "new-sk"}); err != nil {
		t.Fatal(err)
	}
	if row, _ := e.repo.GetStore(v.ID); row.SK != "new-sk" {
		t.Errorf("SK 应更新: %q", row.SK)
	}
	// 测试连接
	res, err := e.svc.TestStore(ctx, e.super, v.ID)
	if err != nil || !res.OK || res.Kind != "ok" || res.Store == nil || res.Store.Status != "ok" || res.Store.LastTest == nil || res.Ms != 12 {
		t.Fatalf("测试: %v %+v", err, res)
	}
	e.testStoreErr = errString("dial tcp 10.255.8.5:9000: i/o timeout (ak=minioadmin01 sk=new-sk)")
	res, _ = e.svc.TestStore(ctx, e.super, v.ID)
	if res.OK || res.Kind != "timeout" || strings.Contains(res.Msg, "new-sk") || strings.Contains(res.Msg, "minioadmin01") || res.Store.Status != "err" {
		t.Errorf("失败应脱敏凭据并标 err: %+v", res)
	}
	e.testStoreErr = nil
	// 未保存的表单：id + 空 SK 沿用库里的
	draft, err := e.svc.TestStoreDraft(ctx, e.super, StoreInput{ID: v.ID, Name: "MinIO", Type: "s3", Endpoint: "http://minio:9000", Bucket: "dba2", AK: "minioadmin01"})
	if err != nil || !draft.OK || draft.Store != nil {
		t.Errorf("草稿测试: %v %+v", err, draft)
	}
	// 停用 / 启用
	if v3, err := e.svc.EnableStore(e.super, v.ID, false); err != nil || v3.Status != "off" || v3.Enabled {
		t.Errorf("停用: %v %+v", err, v3)
	}
	// 被任务引用：不能删、不能改类型
	in := e.baseInput("文件备份")
	in.DestTable, in.DestFile = nil, &engine.FileDest{StoreID: 1, Format: "csv", Gzip: true, PathTemplate: "{db}/{table}/{yyyy}{mm}{dd}"}
	if _, err := e.svc.CreateTask(e.rd, in); err != nil {
		t.Fatal(err)
	}
	mustErrCode(t, e.svc.DeleteStore(e.super, 1), 409, "conflict")
	_, err = e.svc.UpdateStore(e.super, 1, StoreInput{Name: "本地归档盘", Type: "nfs", Path: e.tmpDir})
	mustErrCode(t, err, 409, "conflict")
	if err := e.svc.DeleteStore(e.super, v.ID); err != nil {
		t.Errorf("未被引用的可删: %v", err)
	}
	// 停用的存储不能被新任务选
	e.repo.stores[1].Enabled = false
	in.Name = "再来一个"
	_, err = e.svc.CreateTask(e.rd, in)
	if pe := mustErrCode(t, err, 400, "validation"); !strings.Contains(pe.Message, "已停用") {
		t.Errorf("停用存储: %s", pe.Message)
	}
	// picker 只有精简字段
	pk, err := e.svc.StorePickers(e.rd)
	if err != nil || len(pk) != 1 || pk[0].Name != "本地归档盘" {
		t.Errorf("picker: %v %+v", err, pk)
	}
	if _, err := e.svc.StorePickers(e.viewer); err == nil {
		t.Error("只读用户不能列存储")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// ---- 预览 ----

func TestPreview(t *testing.T) {
	e := newEnv(t)
	in := e.baseInput("预览")
	in.DestFile = &engine.FileDest{StoreID: 1, Format: "csv", Gzip: true, PathTemplate: "{inst}/{db}/{table}/{yyyy}/{mm}/{dd}"}
	r, err := e.svc.Preview(e.viewer, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Issues) != 0 && hasErrorIssue(r.Issues) {
		// viewer 不在 4 号实例的白名单里，但用的是 1 号实例：应没有错误
		t.Errorf("issues: %+v", r.Issues)
	}
	if !strings.Contains(r.Where, "INTERVAL 90 DAY") || !strings.Contains(r.Where, "`status` IN (3, 4)") || !strings.Contains(r.SQL.Select, "不排序") || strings.Contains(r.SQL.Select, "ORDER BY") ||
		!strings.Contains(r.SQL.Delete, "DELETE FROM") || r.DestTableName != "orders_bak" || !strings.Contains(r.DestFilePath, "shop/orders/") ||
		len(r.NextRuns) != 5 || r.ScheduleText != "每天 02:00" || !r.NeedsApproval {
		t.Errorf("预览: %+v", r)
	}
	// 带 task_id：更保守的修改不需要审批
	task := e.enabled("预览对照")
	cur, _ := e.repo.GetTask(task.ID, false)
	same := inputOf(cur)
	same.TaskID = task.ID
	r, err = e.svc.Preview(e.dba, same)
	if err != nil || r.NeedsApproval || len(r.Changes) != 0 {
		t.Errorf("无变化: %v %+v", err, r)
	}
	same.Params.BatchSize++
	if r, _ = e.svc.Preview(e.dba, same); !r.NeedsApproval || len(r.Changes) == 0 {
		t.Errorf("放宽应需要审批: %+v", r)
	}
	// 非法输入返回 issues 而不是错误
	in.RetentionDays = 1
	in.ExtraWhere = "x = (SELECT 1)"
	r, err = e.svc.Preview(e.dba, in)
	if err != nil || !hasErrorIssue(r.Issues) {
		t.Errorf("非法输入应返回 issues: %v %+v", err, r.Issues)
	}
}

// ---- 来源选择 ----

// 旧标签 read_view 不再影响归档资格：带它的 MySQL 主库与普通主库一致。
func TestEligibleInstanceIgnoresLegacyReadViewTag(t *testing.T) {
	in := &model.Instance{InstanceName: "legacy-rv", Type: "master", DBType: "mysql", IsEnable: true,
		Tags: []model.InstanceTag{{TagCode: "read_view", Active: true}}}
	if err := eligibleInstance(in); err != nil {
		t.Errorf("带旧标签 read_view 的主库应可归档: %v", err)
	}
}

func TestSources(t *testing.T) {
	e := newEnv(t)
	insts, err := e.svc.SourceInstances(e.rd)
	if err != nil || len(insts) != 2 || insts[0].Name != "mysql-prod-m1" {
		t.Fatalf("可选实例（只有 MySQL 主库）: %v %+v", err, insts)
	}
	if rg := insts[0].ResourceGroups; len(rg) != 1 || rg[0].Name != "订单组" || !rg[0].HasFlow {
		t.Errorf("资源组与审批流: %+v", rg)
	}
	// 4 号实例关联资源组 1、2；rd 只在 1
	if rg := insts[1].ResourceGroups; len(rg) != 1 || rg[0].ID != 1 {
		t.Errorf("只返回调用者所属的资源组: %+v", rg)
	}
	e.repo.settings = map[[2]int]*model.WorkflowAuditSetting{}
	if insts, _ = e.svc.SourceInstances(e.rd); insts[0].ResourceGroups[0].HasFlow {
		t.Error("没配置审批流 has_flow=false")
	}
	if insts, _ = e.svc.SourceInstances(e.viewer); len(insts) != 1 {
		t.Errorf("viewer 看不到 4 号实例: %d", len(insts))
	}
	ctx := context.Background()
	dbs, err := e.svc.SourceDatabases(ctx, e.rd, 1)
	if err != nil || len(dbs) != 3 {
		t.Fatalf("库（名字不合法的被过滤）: %v %+v", err, dbs)
	}
	for _, d := range dbs {
		if d.Name == "mysql" && !d.Protected || d.Name == "shop" && d.Protected {
			t.Errorf("受保护标记: %+v", d)
		}
	}
	if _, err := e.svc.SourceDatabases(ctx, e.rd, 2); err == nil {
		t.Error("从库不能选")
	}
	if _, err := e.svc.SourceDatabases(ctx, e.viewer, 4); err == nil {
		t.Error("无权访问的实例")
	}
	if _, err := e.svc.SourceTables(ctx, e.rd, 1, "bad db"); err == nil {
		t.Error("非法库名")
	}
	tbs, err := e.svc.SourceTables(ctx, e.rd, 1, "shop")
	if err != nil || len(tbs) != 1 || tbs[0].Name != "orders" {
		t.Errorf("表: %v %+v", err, tbs)
	}
	cols, err := e.svc.SourceColumns(ctx, e.rd, 1, "shop", "orders")
	if err != nil || !cols.HasPK || cols.ActiveTask != nil || cols.Protected {
		t.Errorf("列: %v %+v", err, cols)
	}
	fl, err := e.svc.Flow(e.rd, 1)
	if err != nil || fl.Exists {
		t.Errorf("flow（已清空设置）: %v %+v", err, fl)
	}
	e.repo.settings[[2]int{1, audit.TypeArchive}] = newSetting()
	if fl, _ = e.svc.Flow(e.rd, 1); !fl.Exists || len(fl.Levels) != 2 || fl.Levels[0].GroupName != "团队负责人" || fl.Levels[0].Users[0] != "tl_wang" || fl.Levels[1].Users[0] != "dba_li" {
		t.Errorf("flow: %+v", fl)
	}
	e.enabled("占着表")
	if cols, _ = e.svc.SourceColumns(ctx, e.rd, 1, "shop", "orders"); cols.ActiveTask == nil || cols.ActiveTask.Name != "占着表" {
		t.Errorf("应提示同表已有任务: %+v", cols)
	}
	_, err = e.svc.SourceColumns(ctx, e.rd, 1, "shop", "nope")
	mustErrCode(t, err, 404, "not_found")
}
