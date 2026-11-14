// @author: kevinkelly
// @license: Apache Licence
// @file: actions.go
// @time: 2026/10/28

package sqlflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// 工单日志的操作类型（dbt_workflow_log.operation_type）。
const (
	opSchedule = 4 // 定时执行
	opExecute  = 5 // 执行工单
	opFinish   = 6 // 执行结束 / 手工工单
)

func (s *Service) addLog(tx Repo, auditID uint, op int, desc, info string, actor *audit.Actor) error {
	l := &model.WorkflowLog{AuditID: auditID, OperationType: op, OperationTypeDesc: desc, OperationInfo: truncate(info, 1000), OperationTime: s.now()}
	if actor != nil {
		l.Operator, l.OperatorDisplay = actor.Username, actor.Display
	} else {
		l.OperatorDisplay = "系统"
	}
	return tx.InsertLog(l)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// lockedWorkflow 在事务里按“先工单、后审批”的顺序加锁，避免与并发操作死锁。
func lockedWorkflow(tx Repo, id uint) (*model.SqlWorkflow, *model.WorkflowAudit, error) {
	w, err := tx.GetWorkflow(id, true)
	if err != nil {
		return nil, nil, err
	}
	if w == nil {
		return nil, nil, notFound("工单不存在")
	}
	au, err := tx.AuditByWorkflow(audit.TypeSQLReview, id, true)
	if err != nil {
		return nil, nil, err
	}
	if au == nil {
		return nil, nil, newErr(500, "no_audit", "工单缺少审批记录，请联系管理员")
	}
	return w, au, nil
}

// visible 看不到的工单一律按不存在处理。
func (s *Service) visible(p audit.Actor, pm perms, w *model.SqlWorkflow) error {
	if !pm.canView(p, w) {
		return notFound("工单不存在")
	}
	return nil
}

// ---- 审批 ----

// Pass 审批通过。审批流还有下一级时仍是“待审核”，走完才是“审核通过”（可以执行）。
func (s *Service) Pass(p audit.Actor, id uint, remark string) (*model.SqlWorkflow, error) {
	remark = strings.TrimSpace(remark)
	if len([]rune(remark)) > 500 {
		return nil, bad("审批备注过长")
	}
	var w *model.SqlWorkflow
	var au *model.WorkflowAudit
	var planned *time.Time
	err := s.repo.InTx(func(tx Repo) error {
		var err error
		if w, au, err = lockedWorkflow(tx, id); err != nil {
			return err
		}
		if w.Status != model.WfManReviewing {
			return conflict(fmt.Sprintf("工单当前状态为「%s」，不能审批", StatusLabel(w.Status)))
		}
		if _, err = audit.Operate(tx, s.now(), au, audit.ActionPass, p, remark); err != nil {
			return err
		}
		if au.CurrentStatus == audit.StatusPassed {
			w.Status = model.WfReviewPass
			if planned, err = s.applyPlan(tx, w, au); err != nil {
				return err
			}
			return tx.SaveWorkflow(w)
		}
		return nil
	})
	if err != nil {
		return nil, translate(err)
	}
	if s.phaseOn("Pass") {
		s.notifyAudit(w, au, remark)
	}
	if planned != nil {
		s.notifyScheduled(w, au, *planned)
	}
	info := wfAuditInfo(w)
	if w.Status == model.WfReviewPass || w.Status == model.WfTimingTask {
		info += "，审批已全部通过" // i18n-ignore 写入审计库的存档文本
	} else {
		info += "，进入下一级审批" // i18n-ignore 写入审计库的存档文本
	}
	s.sysAudit(p, "审批通过", withRemark(info, remark)) // i18n-ignore 审计动作名，由 audit.Classify 按原文归类
	return w, nil
}

// applyPlan 审批全部通过时，把提交时设定的定时计划（dbt_sql_schedule 里这张工单的记录）转为“定时执行”。
// 计划时间已过则删除计划、保持“审核通过”并记一条日志，由人决定何时执行。要求 w 已加锁，w.Status 已置为审核通过。
func (s *Service) applyPlan(tx Repo, w *model.SqlWorkflow, au *model.WorkflowAudit) (*time.Time, error) {
	if !tx.ScheduleTableReady() {
		return nil, nil
	}
	sc, err := tx.GetSchedule(w.ID)
	if err != nil || sc == nil {
		return nil, err
	}
	if !sc.RunAt.After(s.now()) {
		if err := tx.DeleteSchedule(w.ID); err != nil {
			return nil, err
		}
		return nil, s.addLog(tx, au.AuditID, opSchedule, "定时执行", fmt.Sprintf("提交时设定的计划时间 %s 已过，未自动转入定时，请手动执行或重新设置定时", sc.RunAt.Format("2006-01-02 15:04")), nil)
	}
	w.Status = model.WfTimingTask
	at := sc.RunAt
	return &at, s.addLog(tx, au.AuditID, opSchedule, "定时执行", fmt.Sprintf("按提交时设定的计划转入定时执行，时间：%s", sc.RunAt.Format("2006-01-02 15:04")), nil)
}

// Cancel 终止工单。提交人是“撤回”；其他有审批权限的人是“驳回”。
// 审核中：当前审批人或提交人；审核通过/定时执行：有执行权限的人或提交人（队列中、执行中的工单不能终止）。
func (s *Service) Cancel(p audit.Actor, id uint, remark string) (*model.SqlWorkflow, error) {
	remark = strings.TrimSpace(remark)
	if remark == "" {
		return nil, bad("终止原因不能为空")
	}
	if len([]rune(remark)) > 500 {
		return nil, bad("终止原因过长")
	}
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	var w *model.SqlWorkflow
	var au *model.WorkflowAudit
	var wasTiming bool
	var cancelAction int
	err = s.repo.InTx(func(tx Repo) error {
		var err error
		if w, au, err = lockedWorkflow(tx, id); err != nil {
			return err
		}
		if err := s.visible(p, pm, w); err != nil {
			return err
		}
		if !s.canCancel(p, pm, w, au) {
			if w.Status == model.WfManReviewing || w.Status == model.WfReviewPass || w.Status == model.WfTimingTask {
				return forbid("你无权操作当前工单！")
			}
			return conflict(fmt.Sprintf("工单当前状态为「%s」，不能终止", StatusLabel(w.Status)))
		}
		action := audit.ActionReject
		if p.Username == w.Engineer {
			action = audit.ActionAbort
		} else if !pm.reject {
			return forbid("你无权操作当前工单！")
		}
		if _, err = audit.Operate(tx, s.now(), au, action, p, remark); err != nil {
			return err
		}
		cancelAction = action
		wasTiming = w.Status == model.WfTimingTask
		w.Status = model.WfAbort
		if err := tx.SaveWorkflow(w); err != nil {
			return err
		}
		// 定时执行的计划，以及提交时设定、尚未生效的计划，都随工单终止一起清掉
		if wasTiming || tx.ScheduleTableReady() {
			return tx.DeleteSchedule(w.ID)
		}
		return nil
	})
	if err != nil {
		return nil, translate(err)
	}
	if s.phaseOn("Cancel") {
		s.notifyAudit(w, au, remark)
	}
	act := "驳回" // i18n-ignore 审计动作名；提交人自己终止是“撤销”，审批 / 执行人终止他人的工单是“驳回”
	if cancelAction == audit.ActionAbort {
		act = "撤销" // i18n-ignore 审计动作名
	}
	s.sysAudit(p, act, withRemark(wfAuditInfo(w), remark))
	return w, nil
}

// AlterRunDate 审批人修改可执行时间窗（只在审核中、由当前审批人操作）。
func (s *Service) AlterRunDate(p audit.Actor, id uint, start, end *time.Time) error {
	if start != nil && end != nil && !end.After(*start) {
		return bad("可执行时间的结束时间必须晚于开始时间")
	}
	var info string
	err := translate(s.repo.InTx(func(tx Repo) error {
		w, au, err := lockedWorkflow(tx, id)
		if err != nil {
			return err
		}
		if w.Status != model.WfManReviewing {
			return conflict(fmt.Sprintf("工单当前状态为「%s」，不能修改可执行时间", StatusLabel(w.Status)))
		}
		if !audit.IsCurrentApprover(tx, au, p) { // 按钮 sqlworkflow.alter_window 由接口校验；这里只要求人在当前审批节点上
			return forbid("你无权操作当前工单！")
		}
		w.RunDateStart, w.RunDateEnd = start, end
		info = wfAuditInfo(w)
		return tx.SaveWorkflow(w)
	}))
	if err == nil {
		s.sysAudit(p, "修改可执行时间", info+"，时间窗："+runWindowText(start, end)) // i18n-ignore 审计动作名与存档文本
	}
	return err
}

func runWindowText(start, end *time.Time) string {
	f := func(t *time.Time) string {
		if t == nil {
			return "不限"
		}
		return t.Format("2006-01-02 15:04")
	}
	return f(start) + " ~ " + f(end)
}

// ---- 执行 ----

// ExecuteReq 是执行请求。
type ExecuteReq struct {
	Mode     string // ModeAuto 交给系统执行 / ModeManual 线下已手工执行，仅确认
	Executor string // 执行方式；为空取推荐默认值
}

// resolveExecutor 校验并确定执行方式。
func (s *Service) resolveExecutor(tx Repo, w *model.SqlWorkflow, choice string) (string, error) {
	opts, def := s.executorOptionsFor(w, s.createTableSQL(tx, w))
	if len(opts) == 0 {
		return "", newErr(500, "no_executor", "未配置 goInception，且已开启强制使用 goInception，无法执行，请联系管理员。")
	}
	choice = strings.TrimSpace(choice)
	if choice == "" {
		return def, nil
	}
	for _, o := range opts {
		if o.Value == choice {
			return choice, nil
		}
	}
	switch {
	case choice == ExecGoInception && s.InceptionConfigured():
		return "", bad("这张工单只能使用原生执行，不能选择 goInception")
	case choice == ExecGoInception:
		return "", bad("未配置 goInception，不能选择 goInception 执行")
	case choice == ExecNative && w.IsManual == 0 && s.requireInception:
		return "", bad("已开启强制使用 goInception，不能选择原生执行")
	}
	return "", bad(fmt.Sprintf("不支持的执行方式：%s", choice))
}

// checkRunnable 执行前的公共检查：状态、权限、时间窗、同库 DDL 并发。要求 w 已加锁。
func (s *Service) checkRunnable(p audit.Actor, pm perms, tx Repo, w *model.SqlWorkflow, at time.Time) error {
	if err := s.visible(p, pm, w); err != nil {
		return err
	}
	if w.Status != model.WfReviewPass && w.Status != model.WfTimingTask {
		return conflict(fmt.Sprintf("工单当前状态为「%s」，只有审核通过或定时执行的工单才能执行", StatusLabel(w.Status)))
	}
	if !pm.canExecute(p, w) {
		return forbid("你无权操作当前工单！")
	}
	if !onWindow(w, at) {
		return conflict("不在可执行时间范围内，如果需要修改执行时间请重新提交工单!")
	}
	return nil
}

// ddlConflict 同一个实例、同一个库上有其他 DDL 正在执行时返回提示：并发 DDL 会互相等元数据锁，需要串行。
func (s *Service) ddlConflict(tx Repo, w *model.SqlWorkflow) (string, error) {
	if w.DBName == "" {
		return "", nil
	}
	others, err := tx.ExecutingDDL(w.DBName, w.ID)
	if err != nil {
		return "", err
	}
	mine := workflowInstanceIDs(w)
	for _, o := range others {
		o := o
		if overlaps(mine, workflowInstanceIDs(&o)) {
			return fmt.Sprintf("相同实例库名下有其他工单正在执行，请稍等。（库：%s，冲突工单：#%d %s）", w.DBName, o.ID, o.WorkflowName), nil
		}
	}
	return "", nil
}

// Execute 立即执行：交给系统执行（进入队列，后台运行）或确认线下已手工执行。
func (s *Service) Execute(ctx context.Context, p audit.Actor, id uint, req ExecuteReq) (*model.SqlWorkflow, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	if !pm.execute && !pm.executeRG {
		return nil, forbid("你没有执行工单的权限")
	}
	var w *model.SqlWorkflow
	var au *model.WorkflowAudit
	executor := ""
	err = s.repo.InTx(func(tx Repo) error {
		var err error
		if w, au, err = lockedWorkflow(tx, id); err != nil {
			return err
		}
		if err := s.checkRunnable(p, pm, tx, w, s.now()); err != nil {
			return err
		}
		if w.IsOfflineExport != 0 && req.Mode == ModeManual {
			return bad("数据导出工单没有“手工执行”，请选择立即执行或定时执行")
		}
		switch req.Mode {
		case ModeAuto:
			if executor, err = s.resolveExecutor(tx, w, req.Executor); err != nil {
				return err
			}
			msg, err := s.ddlConflict(tx, w)
			if err != nil {
				return err
			}
			if msg != "" {
				return conflict(msg)
			}
			w.Status = model.WfQueuing
			if err := tx.SaveWorkflow(w); err != nil {
				return err
			}
			if err := tx.DeleteSchedule(w.ID); err != nil {
				return err
			}
			return s.addLog(tx, au.AuditID, opExecute, "执行工单", fmt.Sprintf("工单执行排队中（执行方式：%s）", executorLabel(executor)), &p)
		case ModeManual:
			now := s.now()
			w.Status, w.FinishTime = model.WfFinish, &now
			if err := tx.SaveWorkflow(w); err != nil {
				return err
			}
			if err := tx.DeleteSchedule(w.ID); err != nil {
				return err
			}
			return s.addLog(tx, au.AuditID, opFinish, "手工工单", "确认手工执行结束", &p)
		}
		return bad("不支持的执行模式")
	})
	if err != nil {
		return nil, translate(err)
	}
	if req.Mode == ModeManual {
		if s.phaseOn("Execute") {
			s.notifyExecute(w, au)
		}
		s.sysAudit(p, "手工执行确认", wfAuditInfo(w)) // i18n-ignore 审计动作名
		return w, nil
	}
	s.sysAudit(p, "立即执行", wfAuditInfo(w)+"，执行方式："+executorLabel(executor)) // i18n-ignore 审计动作名与存档文本
	actor := p
	s.spawn(func() { s.run(id, &actor, executor) })
	return w, nil
}

func executorLabel(e string) string {
	if e == ExecGoInception {
		return "goInception"
	}
	return "原生执行"
}

// Schedule 设置定时执行。
func (s *Service) Schedule(p audit.Actor, id uint, runAt time.Time, executor string) (*model.SqlWorkflow, error) {
	if !s.repo.ScheduleTableReady() {
		return nil, newErr(500, "schedule_table", "定时执行需要先创建 dbt_sql_schedule 表（见 deploy/sql/002_sql_schedule.sql），请联系管理员。")
	}
	if !runAt.After(s.now()) {
		return nil, bad("时间不能小于当前时间")
	}
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	if !pm.execute && !pm.executeRG {
		return nil, forbid("你没有执行工单的权限")
	}
	var w *model.SqlWorkflow
	var au *model.WorkflowAudit
	err = s.repo.InTx(func(tx Repo) error {
		var err error
		if w, au, err = lockedWorkflow(tx, id); err != nil {
			return err
		}
		if err := s.checkRunnable(p, pm, tx, w, runAt); err != nil {
			return err
		}
		if executor, err = s.resolveExecutor(tx, w, executor); err != nil {
			return err
		}
		w.Status = model.WfTimingTask
		if err := tx.SaveWorkflow(w); err != nil {
			return err
		}
		if err := tx.UpsertSchedule(&model.SqlSchedule{WorkflowID: w.ID, RunAt: runAt, CreatedBy: p.Username, Executor: executor}); err != nil {
			return err
		}
		return s.addLog(tx, au.AuditID, opSchedule, "定时执行",
			fmt.Sprintf("定时执行时间：%s（执行方式：%s）", runAt.Format("2006-01-02 15:04"), executorLabel(executor)), &p)
	})
	if err != nil {
		return nil, translate(err)
	}
	s.notifyScheduled(w, au, runAt)
	s.sysAudit(p, "定时执行", wfAuditInfo(w)+"，定时时间："+runAt.Format("2006-01-02 15:04")) // i18n-ignore 审计动作名与存档文本
	return w, nil
}
