// @author: kevinkelly
// @license: Apache Licence
// @file: recover.go
// @time: 2026/10/28

package sqlflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/review"
)

// 工单执行恢复。
//
// 背景：工单执行是后台协程，进程被杀 / 停服等待超时后，工单会永远停在“排队中 / 执行中”（见 run.go 的 begin / finish）。
// 做法：
//  1. 工单执行期间持有“每工单一把”的跨进程锁（MySQL GET_LOCK，锁属于一条连接，进程死了连接断开锁自动释放），
//     所以“没人在执行”可以靠“能拿到这把锁”可靠地判断，多个 dbtool 实例同时运行也安全。
//  2. 启动后以及之后每隔 recoverInterval，扫描停在排队中 / 执行中、且能拿到锁的工单，置为“执行有异常”并写日志、发通知。
//     不会自动重试：SQL 可能已经部分执行，必须由人到目标库核对。
//  3. 页面上对仍显示排队中 / 执行中的工单，有权限的人可以手工“标记为异常”（同样先拿锁，别的进程还在执行就拒绝）。
//
// 仓库不实现 Locker 时（仅测试用的最简仓库）无法可靠判断，周期恢复不做任何事。

// Locker 是仓库可选实现的跨进程互斥锁（GET_LOCK）：拿不到返回 ok=false；拿到后必须调用 release。
type Locker interface {
	TryLock(ctx context.Context, name string) (release func(), ok bool, err error)
}

const (
	// recoverGrace：排队中 / 执行中的工单，最近一条日志距今不足这个时间不处理。
	// 工单从“排队中”到执行协程拿到锁之间有一个极短的空窗，这里留足余量，避免误伤刚排队的工单。
	recoverGrace = 2 * time.Minute
	// recoverInterval：周期扫描的间隔（启动时先扫一次）。
	recoverInterval = 5 * time.Minute
	// recoverBatch：一次最多处理的工单数。
	recoverBatch = 200
)

// RecoverMessage 是写进工单日志的固定说明。
const RecoverMessage = "进程重启/失联，执行状态未知，请人工核对目标库"

var systemActor = audit.Actor{Username: "system", Display: "系统"}

// runLockName 是每张工单的执行锁名（MySQL 锁名最长 64 个字符）。
func runLockName(id uint) string { return fmt.Sprintf("dbt_sqlwf_run_%d", id) }

// lockRun 取工单的执行锁。locked=false 表示别的进程（或别的协程）持有；err 为取锁本身出错。
// 仓库不支持锁时返回空操作的 release 与 supported=false。
func (s *Service) lockRun(id uint) (release func(), locked, supported bool, err error) {
	lk, ok := s.repo.(Locker)
	if !ok || lk == nil {
		return func() {}, true, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rel, got, err := lk.TryLock(ctx, runLockName(id))
	if err != nil {
		return func() {}, false, true, err
	}
	if !got {
		return func() {}, false, true, nil
	}
	return rel, true, true, nil
}

// SetRecoverInterval 调整周期扫描间隔，仅测试使用。
func (s *Service) SetRecoverInterval(d time.Duration) { s.recoverEvery = d }

func isInFlight(status string) bool {
	return status == model.WfQueuing || status == model.WfExecuting
}

var (
	errNotStuck = errors.New("工单已不在排队/执行中")
	errRunning  = errors.New("工单仍有进程在执行")
)

// abandon 把停在排队中 / 执行中的工单置为“执行有异常”。要求先拿到执行锁；拿不到（还有进程在执行）返回 errRunning。
// pre 在工单加锁后调用，用来做权限等检查；actor 为 nil 表示系统恢复。
func (s *Service) abandon(id uint, actor *audit.Actor, desc, info string, pre func(*model.SqlWorkflow) error) (*model.SqlWorkflow, *model.WorkflowAudit, error) {
	if _, local := s.prog.Load(id); local {
		return nil, nil, errRunning
	}
	release, locked, _, err := s.lockRun(id)
	if err != nil {
		return nil, nil, err
	}
	if !locked {
		return nil, nil, errRunning
	}
	defer release()
	var w *model.SqlWorkflow
	var au *model.WorkflowAudit
	err = s.repo.InTx(func(tx Repo) error {
		var err error
		if w, au, err = lockedWorkflow(tx, id); err != nil {
			return err
		}
		if pre != nil {
			if err := pre(w); err != nil {
				return err
			}
		}
		if !isInFlight(w.Status) {
			return errNotStuck
		}
		now := s.now()
		w.Status, w.FinishTime = model.WfException, &now
		if err := tx.SaveWorkflow(w); err != nil {
			return err
		}
		// 没有执行结果时补一行，让详情页的“执行结果”里能看到原因；已有（批量工单的逐实例状态等）则保留。
		c, err := tx.GetContent(id)
		if err != nil {
			return err
		}
		if c == nil {
			c = &model.SqlWorkflowContent{WorkflowID: id}
		}
		if rows, _ := review.ParseRows(c.ExecuteResult); len(rows) == 0 {
			c.ExecuteResult = failedSet(c.SQLContent, RecoverMessage).JSON()
			if err := tx.SaveContent(c); err != nil {
				return err
			}
		}
		return s.addLog(tx, au.AuditID, opFinish, desc, info, actor)
	})
	if err != nil {
		return nil, nil, err
	}
	return w, au, nil
}

// lastActivity 取工单最近一条日志的时间（取不到返回零值）。
func (s *Service) lastActivity(id uint) time.Time {
	au, err := s.repo.AuditByWorkflow(audit.TypeSQLReview, id, false)
	if err != nil || au == nil {
		return time.Time{}
	}
	logs, err := s.repo.AuditLogs(au.AuditID)
	if err != nil {
		return time.Time{}
	}
	var t time.Time
	for i := range logs {
		if logs[i].OperationTime.After(t) {
			t = logs[i].OperationTime
		}
	}
	return t
}

// RecoverStuck 扫描一次：把“排队中 / 执行中”且没有任何进程在执行的工单置为“执行有异常”，返回处理的数量。
func (s *Service) RecoverStuck() int {
	if _, ok := s.repo.(Locker); !ok {
		return 0 // 无法可靠判断“没人在执行”，宁可不动
	}
	rows, _, err := s.repo.ListWorkflows(WorkflowFilter{Scope: ScopeAll, Statuses: []string{model.WfQueuing, model.WfExecuting}, Limit: recoverBatch, OrderBy: "id"})
	if err != nil {
		slog.Error("扫描排队/执行中的工单失败", "err", err)
		return 0
	}
	n := 0
	for _, r := range rows {
		id := r.ID
		if last := s.lastActivity(id); !last.IsZero() && s.now().Sub(last) < recoverGrace {
			continue
		}
		w, au, err := s.abandon(id, nil, "执行异常（自动恢复）", RecoverMessage, nil)
		switch {
		case err == nil:
		case errors.Is(err, errRunning), errors.Is(err, errNotStuck):
			continue
		default:
			slog.Error("恢复停在执行中的工单失败", "workflow", id, "err", err)
			continue
		}
		n++
		slog.Warn("工单停在执行中且无进程执行，已置为“执行有异常”（未重试，请人工核对目标库）", "workflow", id, "status_before", r.Status)
		if s.phaseOn("Execute") {
			s.notifyExecute(w, au)
		}
		s.sysAudit(systemActor, "执行状态恢复", wfAuditInfo(w)+"，原状态："+StatusLabel(r.Status)+"，"+RecoverMessage) // i18n-ignore 审计动作名与存档文本
	}
	return n
}

// canMarkStuck 谁可以手工把卡住的工单标记为异常：超级用户、提交人、有该资源组执行权限的人、该组的审批人。
func (pm perms) canMarkStuck(p audit.Actor, w *model.SqlWorkflow) bool {
	switch {
	case p.IsSuperuser, w.Engineer == p.Username:
		return true
	case pm.groups[w.GroupID] && pm.groupView():
		return true
	}
	return false
}

// MarkException 手工把仍显示“排队中 / 执行中”的工单标记为“执行有异常”（不重试）。
// 只有在没有任何进程持有该工单的执行锁时才允许，避免把正在执行的工单误标。
func (s *Service) MarkException(p audit.Actor, id uint, remark string) (*model.SqlWorkflow, error) {
	remark = strings.TrimSpace(remark)
	if remark == "" {
		return nil, bad("请填写标记原因")
	}
	if len([]rune(remark)) > 500 {
		return nil, bad("原因过长")
	}
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	w, au, err := s.abandon(id, &p, "标记执行异常", fmt.Sprintf("人工标记为执行有异常（不会重试，请核对目标库）。原因：%s", remark), func(w *model.SqlWorkflow) error {
		if err := s.visible(p, pm, w); err != nil {
			return err
		}
		if !isInFlight(w.Status) {
			return conflict(fmt.Sprintf("工单当前状态为「%s」，只有排队中或执行中的工单才能标记为异常", StatusLabel(w.Status)))
		}
		if !pm.canMarkStuck(p, w) {
			return forbid("你无权操作当前工单！")
		}
		return nil
	})
	switch {
	case errors.Is(err, errRunning):
		return nil, conflict("该工单仍有进程在执行（持有执行锁），不能标记；若确认是进程已失联，请稍后重试或等待系统自动恢复")
	case errors.Is(err, errNotStuck):
		return nil, conflict("工单已不在排队/执行中")
	case err != nil:
		return nil, translate(err)
	}
	if s.phaseOn("Execute") {
		s.notifyExecute(w, au)
	}
	s.sysAudit(p, "标记执行异常", withRemark(wfAuditInfo(w), remark)) // i18n-ignore 审计动作名
	return w, nil
}

// canMarkException 详情页是否显示“标记为异常”：排队中 / 执行中，有权限，且本进程里没有在跑。
func (s *Service) canMarkException(p audit.Actor, pm perms, w *model.SqlWorkflow) bool {
	if !isInFlight(w.Status) || !pm.canMarkStuck(p, w) {
		return false
	}
	_, local := s.prog.Load(w.ID)
	return !local
}

// stuckHint 排队中 / 执行中的工单，最近一条日志已超过 recoverGrace 时给出提示（可能是进程失联）。
func (s *Service) stuckHint(w *model.SqlWorkflow) string {
	if !isInFlight(w.Status) {
		return ""
	}
	last := s.lastActivity(w.ID)
	if last.IsZero() || s.now().Sub(last) < recoverGrace {
		return ""
	}
	return fmt.Sprintf("该工单已长时间停留在「%s」。若确认执行进程已失联（服务被重启 / 被强杀），系统会在几分钟内自动置为「执行有异常」；也可以由有权限的人手工标记。无论哪种，都不会自动重试，请先到目标库核对实际执行情况。", StatusLabel(w.Status))
}
