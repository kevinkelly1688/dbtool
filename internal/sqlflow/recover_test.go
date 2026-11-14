// @author: kevinkelly
// @license: Apache Licence
// @file: recover_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/review"
	"dbtool/internal/sqlflow"
)

func lockName(id uint) string { return fmt.Sprintf("dbt_sqlwf_run_%d", id) }

// stuck 造一张停在排队中的工单：执行请求被接受，但执行协程没有跑（模拟进程被杀）。
func (e *env) stuck(t *testing.T) uint {
	t.Helper()
	id := e.submit(t, dev, "update orders set a=1 where id=1")
	e.approve(t, id)
	e.svc.SetSpawn(func(func()) {}) // 吞掉后台执行：进程“在排队后被杀”
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	e.svc.SetSpawn(func(f func()) { f() })
	if e.status(id) != model.WfQueuing {
		t.Fatalf("应停在排队中: %s", e.status(id))
	}
	return id
}

func (e *env) later(d time.Duration) {
	at := e.now.Add(d)
	e.svc.SetClock(func() time.Time { return at })
}

func TestRecoverStuckMarksExceptionWithoutRetry(t *testing.T) {
	e := setup(t)
	id := e.stuck(t)
	e.cap.reset()
	e.later(10 * time.Minute)
	if n := e.svc.RecoverStuck(); n != 1 {
		t.Fatalf("恢复数量: %d", n)
	}
	w := e.repo.Workflows[id]
	if w.Status != model.WfException || w.FinishTime == nil {
		t.Fatalf("工单: %+v", w)
	}
	if len(e.eng.ran) != 0 {
		t.Errorf("恢复绝不能重试执行: %v", e.eng.ran)
	}
	last := e.repo.Logs[len(e.repo.Logs)-1]
	if !strings.Contains(last.OperationInfo, "进程重启/失联，执行状态未知，请人工核对目标库") || last.OperatorDisplay != "系统" {
		t.Errorf("日志: %+v", last)
	}
	rows, _ := review.ParseRows(e.repo.Contents[id].ExecuteResult)
	if len(rows) != 1 || !strings.Contains(rows[0].ErrorMessage, "人工核对") {
		t.Errorf("执行结果应说明原因: %+v", rows)
	}
	if names := e.cap.names(); len(names) != 1 || names[0] != notify.EventException {
		t.Errorf("通知: %v", names)
	}
	if len(e.repo.LifecycleAudits) == 0 || !strings.Contains(e.repo.LifecycleAudits[len(e.repo.LifecycleAudits)-1].Action, "执行状态恢复") {
		t.Errorf("审计: %+v", e.repo.LifecycleAudits)
	}
	// 再扫一次不会重复处理
	if n := e.svc.RecoverStuck(); n != 0 {
		t.Errorf("重复恢复: %d", n)
	}
}

func TestRecoverStuckExecuting(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update orders set a=1")
	e.approve(t, id)
	w := e.repo.Workflows[id]
	w.Status = model.WfExecuting
	e.repo.Workflows[id] = w
	e.later(time.Hour)
	if n := e.svc.RecoverStuck(); n != 1 || e.status(id) != model.WfException {
		t.Fatalf("n=%d status=%s", n, e.status(id))
	}
}

func TestRecoverSkipsWhenLockHeld(t *testing.T) {
	e := setup(t)
	id := e.stuck(t)
	e.later(time.Hour)
	release := e.repo.Hold(lockName(id)) // 别的进程仍持有执行锁
	if n := e.svc.RecoverStuck(); n != 0 || e.status(id) != model.WfQueuing {
		t.Fatalf("有人在执行时不能动: n=%d status=%s", n, e.status(id))
	}
	release()
	if n := e.svc.RecoverStuck(); n != 1 || e.status(id) != model.WfException {
		t.Fatalf("锁释放后应恢复: n=%d status=%s", n, e.status(id))
	}
	if e.repo.Locked(lockName(id)) {
		t.Error("恢复结束后必须释放锁")
	}
}

func TestRecoverGraceForFreshQueued(t *testing.T) {
	e := setup(t)
	id := e.stuck(t)
	e.later(30 * time.Second) // 刚排队：到执行协程拿锁之间有空窗，不能误伤
	if n := e.svc.RecoverStuck(); n != 0 || e.status(id) != model.WfQueuing {
		t.Fatalf("宽限期内不处理: n=%d status=%s", n, e.status(id))
	}
	e.later(3 * time.Minute)
	if n := e.svc.RecoverStuck(); n != 1 {
		t.Fatalf("宽限期后应处理: %d", n)
	}
}

func TestRecoverIgnoresOtherStatuses(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update orders set a=1")
	e.approve(t, id)
	e.later(time.Hour)
	if n := e.svc.RecoverStuck(); n != 0 || e.status(id) != model.WfReviewPass {
		t.Fatalf("审核通过的工单不能动: n=%d status=%s", n, e.status(id))
	}
}

// noLockRepo 把仓库包起来，只暴露 sqlflow.Repo 接口（没有 TryLock）。
type noLockRepo struct{ sqlflow.Repo }

func TestRecoverNeedsLocker(t *testing.T) {
	e := setup(t)
	id := e.stuck(t)
	e.later(time.Hour)
	svc := sqlflow.NewService(noLockRepo{e.repo}, sqlflow.Options{})
	svc.SetClock(func() time.Time { return e.now.Add(time.Hour) })
	if n := svc.RecoverStuck(); n != 0 || e.status(id) != model.WfQueuing {
		t.Fatalf("无法可靠判断“没人执行”时不处理: n=%d status=%s", n, e.status(id))
	}
}

// 执行期间必须持有该工单的锁，结束后释放；这是“没人在执行”判断可靠的前提。
func TestRunHoldsLockWhileExecuting(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update orders set a=1")
	e.approve(t, id)
	var during bool
	e.eng.onExec = func() { during = e.repo.Locked(lockName(id)) }
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if !during {
		t.Error("执行期间应持有工单锁")
	}
	if e.repo.Locked(lockName(id)) {
		t.Error("执行结束后必须释放锁")
	}
	if e.status(id) != model.WfFinish {
		t.Errorf("状态: %s", e.status(id))
	}
}

// 别的进程已经在执行（锁被占）时，本进程的执行协程什么也不做，不并发执行同一张工单。
func TestRunSkipsWhenAnotherProcessHoldsLock(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update orders set a=1")
	e.approve(t, id)
	release := e.repo.Hold(lockName(id))
	defer release()
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if len(e.eng.ran) != 0 || e.status(id) != model.WfQueuing {
		t.Errorf("锁被占用时不应执行: ran=%v status=%s", e.eng.ran, e.status(id))
	}
}

// 取锁出错（库抖动）不能让已批准的工单执行不了。
func TestRunProceedsWhenLockErrors(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update orders set a=1")
	e.approve(t, id)
	e.repo.LockErr = fmt.Errorf("connection reset")
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if e.status(id) != model.WfFinish {
		t.Errorf("状态: %s", e.status(id))
	}
}

func TestStartSchedulerRecoversAtStartup(t *testing.T) {
	e := setup(t)
	id := e.stuck(t)
	e.later(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); e.svc.StartScheduler(ctx, time.Hour) }()
	deadline := time.Now().Add(3 * time.Second)
	for e.status(id) != model.WfException && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	if e.status(id) != model.WfException {
		t.Fatalf("启动后应立即扫描并恢复: %s", e.status(id))
	}
}

// ---- 手工标记为异常 ----

func TestMarkExceptionValidation(t *testing.T) {
	e := setup(t)
	id := e.stuck(t)
	if _, err := e.svc.MarkException(dev, id, " "); code(err) != "bad_request" {
		t.Errorf("原因必填: %v", err)
	}
	if _, err := e.svc.MarkException(dev, id, strings.Repeat("长", 501)); code(err) != "bad_request" {
		t.Errorf("原因过长: %v", err)
	}
	if _, err := e.svc.MarkException(stranger, id, "x"); code(err) != "not_found" {
		t.Errorf("看不到的工单按不存在处理: %v", err)
	}
	if _, err := e.svc.MarkException(outsider, id, "x"); code(err) != "not_found" {
		t.Errorf("别的资源组: %v", err)
	}
	if e.status(id) != model.WfQueuing {
		t.Errorf("失败的请求不应改状态: %s", e.status(id))
	}
}

func TestMarkExceptionAllowedUsers(t *testing.T) {
	for _, tc := range []struct {
		name string
		who  audit.Actor
		ok   bool
	}{{"提交人", dev, true}, {"组内执行人", dba, true}, {"超级用户", root, true}, {"组内审批人", leader, true}, {"路人", stranger, false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			id := e.stuck(t)
			e.cap.reset()
			w, err := e.svc.MarkException(tc.who, id, "进程被杀")
			if !tc.ok {
				if err == nil || e.status(id) != model.WfQueuing {
					t.Fatalf("不应成功: %v %s", err, e.status(id))
				}
				return
			}
			if err != nil || w.Status != model.WfException || e.status(id) != model.WfException {
				t.Fatalf("err=%v status=%s", err, e.status(id))
			}
			last := e.repo.Logs[len(e.repo.Logs)-1]
			if !strings.Contains(last.OperationInfo, "进程被杀") || last.Operator != tc.who.Username {
				t.Errorf("日志: %+v", last)
			}
			if len(e.eng.ran) != 0 {
				t.Errorf("不能重试执行: %v", e.eng.ran)
			}
			if names := e.cap.names(); len(names) != 1 || names[0] != notify.EventException {
				t.Errorf("通知: %v", names)
			}
			if a := e.repo.LifecycleAudits; len(a) == 0 || !strings.Contains(a[len(a)-1].Action, "标记执行异常") {
				t.Errorf("审计: %+v", a)
			}
		})
	}
}

func TestMarkExceptionRefusedWhileRunning(t *testing.T) {
	e := setup(t)
	id := e.stuck(t)
	release := e.repo.Hold(lockName(id))
	if _, err := e.svc.MarkException(dev, id, "x"); code(err) != "conflict" || e.status(id) != model.WfQueuing {
		t.Fatalf("有进程持有执行锁时必须拒绝: %v %s", err, e.status(id))
	}
	release()
	if _, err := e.svc.MarkException(dev, id, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.MarkException(dev, id, "x"); code(err) != "conflict" {
		t.Errorf("已不在执行中: %v", err)
	}
}

func TestMarkExceptionWrongStatus(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update orders set a=1")
	if _, err := e.svc.MarkException(dev, id, "x"); code(err) != "conflict" {
		t.Errorf("等待审批的工单不能标记: %v", err)
	}
}

func TestDetailMarkExceptionFlag(t *testing.T) {
	e := setup(t)
	id := e.stuck(t)
	d, err := e.svc.Get(dev, id)
	if err != nil || !d.CanMarkException || d.StuckHint != "" {
		t.Fatalf("刚排队：可标记但还不提示: %+v %v", d.CanMarkException, err)
	}
	e.later(10 * time.Minute)
	if d, _ = e.svc.Get(dev, id); !d.CanMarkException || d.StuckHint == "" {
		t.Errorf("长时间无进展应提示: %q", d.StuckHint)
	}
	if d, _ = e.svc.Get(leader, id); !d.CanMarkException {
		t.Error("组内审批人可标记")
	}
	e.svc.MarkException(dev, id, "x")
	if d, _ = e.svc.Get(dev, id); d.CanMarkException {
		t.Error("已标记后不再显示")
	}
}
