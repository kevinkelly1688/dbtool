// @author: kevinkelly
// @license: Apache Licence
// @file: notify.go
// @time: 2026/10/28

package sqlflow

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/review"
	"dbtool/internal/settings"
)

// kindLabel 通知里的工单类型名：数据导出与 SQL 上线分开显示。
// 数据导出选了“导出原值”的，类型名里带上，审批人在通知标题里就能看到。
func (s *Service) kindLabel(w *model.SqlWorkflow) string {
	if w.IsOfflineExport != 0 {
		if e, err := s.repo.GetExportExt(w.ID); err == nil && e != nil && e.Unmasked {
			return "数据导出申请（导出原值，不脱敏）" // i18n-ignore 通知正文（语言属于接收方）
		}
		return "数据导出申请" // i18n-ignore 通知正文（语言属于接收方）
	}
	return "SQL上线申请" // i18n-ignore 通知正文（语言属于接收方）
}

var blankLines = regexp.MustCompile(`[\r\n\f]{2,}`)

// phaseOn 按 notify.phases 判断是否通知该阶段：为空表示所有阶段都通知；否则只通知列出的阶段（Pass / Execute / Cancel）。
func (s *Service) phaseOn(phase string) bool {
	v := cfgStr(s.repo, settings.KeyNotifyPhases)
	if v == "" {
		return true
	}
	return contains(splitComma(v), phase)
}

func (s *Service) workflowURL(id uint) string {
	return fmt.Sprintf("%s/sqlworkflow/%d", strings.TrimRight(s.BaseURL, "/"), id)
}

// preview 是默认文案里的“工单详情预览”。系统配置 notify.sql_preview 控制它的显示方式（影响没有套消息模板的默认文案）：
// head（默认）前 500 个字；mask 先把字符串常量换成 '***' 再取前 500 个字；none 不显示 SQL。
func (s *Service) preview(id uint) string {
	c, err := s.repo.GetContent(id)
	if err != nil || c == nil {
		return ""
	}
	// 配置了脱敏的字段，其值在通知里也只显示脱敏后的结果（通知发到群里，不分收件人）
	sqlText := c.SQLContent
	if w, werr := s.repo.GetWorkflow(id, false); werr == nil && w != nil {
		if sqlText = s.maskedSQLForNotify(w, c.SQLContent); sqlText == "" && c.SQLContent != "" {
			return ""
		}
	}
	switch strings.ToLower(strings.TrimSpace(cfgStr(s.repo, settings.KeyNotifySQLPreview))) {
	case "mask":
		t, _ := notify.SQLDisplay(sqlText, notify.ModeMask, 500)
		return blankLines.ReplaceAllString(t, "\n")
	case "none":
		t, _ := notify.SQLDisplay(sqlText, notify.ModeNone, 500)
		return t
	}
	r := []rune(strings.ReplaceAll(sqlText, "\r", ""))
	if len(r) > 500 {
		r = r[:500]
	}
	return blankLines.ReplaceAllString(string(r), "\n")
}

func (s *Service) readableFlow(au *model.WorkflowAudit) string {
	var parts []string
	for _, n := range audit.ReviewNodes(s.repo, au) {
		switch {
		case n.Current:
			parts = append(parts, n.Name+"(current)")
		case n.Passed:
			parts = append(parts, n.Name+"(passed)")
		default:
			parts = append(parts, n.Name)
		}
	}
	if len(parts) == 0 {
		return "系统自动通过" // i18n-ignore 通知正文（语言属于接收方）
	}
	return strings.Join(parts, " -> ")
}

// instName 通知里展示的实例名；批量工单只显示首个实例名和总数，避免几十个实例名把消息撑爆。
func instName(w *model.SqlWorkflow) string {
	name := ""
	if w.Instance != nil {
		name = w.Instance.InstanceName
	}
	if ids := parseBatchIDs(w.BatchInstanceIDs); len(ids) > 1 {
		return fmt.Sprintf("%s...等 %d 个实例（批量上线）", name, len(ids)) // i18n-ignore 通知正文（语言属于接收方）
	} else if len(ids) == 1 {
		return name + "（批量上线）" // i18n-ignore 通知正文（语言属于接收方）
	}
	return name
}

// workflowVars 是消息模板用的变量（见 notify.Vars）。sql 是工单 SQL 的全文（按 notify.MaxSQLVar 截断），
// 模板里怎么显示（完整 / 只显示前 N 字 / 隐藏字符串常量 / 不显示）由模板的 SQL 代码块决定。
func (s *Service) workflowVars(w *model.SqlWorkflow, au *model.WorkflowAudit) map[string]string {
	pick := func(vs ...string) string {
		for _, v := range vs {
			if v != "" {
				return v
			}
		}
		return ""
	}
	v := map[string]string{
		"ticket_id":      strconv.FormatUint(uint64(au.AuditID), 10),
		"kind":           s.kindLabel(w),
		"title":          pick(au.WorkflowTitle, w.WorkflowName),
		"created_at":     w.CreateTime.Format("2006-01-02 15:04:05"),
		"url":            s.workflowURL(w.ID),
		"applicant":      pick(au.CreateUserDisplay, w.EngineerDisplay, w.Engineer),
		"applicant_user": w.Engineer,
		"group":          pick(au.GroupName, w.GroupName),
		"instance":       instName(w),
		"db":             w.DBName,
		"syntax":         SyntaxLabel(w.SyntaxType),
		"flow":           s.readableFlow(au),
	}
	if c, err := s.repo.GetContent(w.ID); err == nil && c != nil {
		v["sql"] = truncate(s.maskedSQLForNotify(w, c.SQLContent), notify.MaxSQLVar)
	}
	return v
}

// execVars 在 workflowVars 之上补执行结果：结束时间、耗时、影响行数、第一条错误信息。
func (s *Service) execVars(w *model.SqlWorkflow, au *model.WorkflowAudit) map[string]string {
	v := s.workflowVars(w, au)
	if w.FinishTime != nil {
		v["finish_at"] = w.FinishTime.Format("2006-01-02 15:04:05")
	}
	c, err := s.repo.GetContent(w.ID)
	if err != nil || c == nil {
		return v
	}
	rows, err := review.ParseRows(c.ExecuteResult)
	if err != nil || len(rows) == 0 {
		return v
	}
	if k, ok := s.notifyMask(w); ok {
		rows = k.Rows(rows) // 执行记录与错误信息里的敏感值同样脱敏
	} else {
		return v // 脱敏配置读不出来：不带执行记录
	}
	if rep, ok := buildExecReport(rows); ok { // 总数 / 成功 / 失败 / 执行记录（非批量按语句，批量按实例）
		reportVars(v, rep)
	}
	var affected int64
	var secs float64
	var firstErr string
	for _, r := range rows {
		if r.IsSummary {
			continue
		}
		if r.AffectedRows > 0 { // 小于 0（SQL Server 的 DDL 返回 -1）按 0 算
			affected += int64(r.AffectedRows)
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(r.ExecuteTime), 64); err == nil {
			secs += f
		}
		if firstErr == "" && (r.ErrLevel >= 2 || (r.ErrLevel > 0 && r.StageStatus != "")) && strings.TrimSpace(r.ErrorMessage) != "" {
			firstErr = strings.TrimSpace(r.ErrorMessage)
		}
	}
	v["affected_rows"] = strconv.FormatInt(affected, 10)
	if secs > 0 {
		v["elapsed"] = strconv.FormatFloat(secs, 'f', 3, 64) + " 秒" // i18n-ignore 通知正文（语言属于接收方）
	}
	if firstErr != "" {
		v["error"] = truncate(firstErr, 1000)
	}
	return v
}

// auditEvent 审批阶段的通知内容。
func (s *Service) auditEvent(w *model.SqlWorkflow, au *model.WorkflowAudit, remark string) notify.Event {
	created := w.CreateTime.Format("2006-01-02 15:04:05")
	flow := s.readableFlow(au)
	url := s.workflowURL(w.ID)
	prev := s.preview(w.ID)
	ev := notify.Event{Kind: notify.KindSQLWorkflow, GroupID: w.GroupID, Applicant: w.Engineer, Vars: s.workflowVars(w, au)}
	switch au.CurrentStatus {
	case audit.StatusWaiting:
		current := ""
		for _, n := range audit.ReviewNodes(s.repo, au) {
			if n.Current {
				current, ev.ReviewerAuthGroupID = n.Name, n.GroupID
			}
		}
		ev.Name = notify.EventPending
		ev.Vars["current_step"] = current
		if notify.IsFirstAuditNode(au.AuditAuthGroups, au.CurrentAudit) {
			ev.Specific = notify.EventSubmitted // 新工单提交（同时仍属于“待处理”）
		}
		ev.Title = fmt.Sprintf("[%s]新的工单申请#%d", s.kindLabel(w), au.AuditID)                                               // i18n-ignore 通知正文（语言属于接收方）
		ev.Content = fmt.Sprintf("发起时间：%s\n发起人：%s\n组：%s\n目标实例：%s\n数据库：%s\n审批流程：%s\n当前审批：%s\n工单名称：%s\n工单地址：%s\n工单详情预览：%s", // i18n-ignore 通知正文（语言属于接收方）
			created, au.CreateUserDisplay, au.GroupName, instName(w), w.DBName, flow, current, au.WorkflowTitle, url, prev)
	case audit.StatusPassed:
		ev.Name = notify.EventPassed
		ev.Title = fmt.Sprintf("[%s]工单审核通过#%d", s.kindLabel(w), au.AuditID)                                        // i18n-ignore 通知正文（语言属于接收方）
		ev.Content = fmt.Sprintf("发起时间：%s\n发起人：%s\n组：%s\n目标实例：%s\n数据库：%s\n审批流程：%s\n工单名称：%s\n工单地址：%s\n工单详情预览：%s\n", // i18n-ignore 通知正文（语言属于接收方）
			created, au.CreateUserDisplay, au.GroupName, instName(w), w.DBName, flow, au.WorkflowTitle, url, prev)
	case audit.StatusRejected:
		ev.Name = notify.EventRejected
		ev.Vars["remark"] = strings.TrimSpace(blankLines.ReplaceAllString(remark, "\n"))
		ev.Title = fmt.Sprintf("[%s]工单被驳回#%d", s.kindLabel(w), au.AuditID)                                         // i18n-ignore 通知正文（语言属于接收方）
		ev.Content = fmt.Sprintf("发起时间：%s\n目标实例：%s\n数据库：%s\n工单名称：%s\n工单地址：%s\n驳回原因：%s\n提醒：此工单被审核不通过，请按照驳回原因进行修改！", // i18n-ignore 通知正文（语言属于接收方）
			created, instName(w), w.DBName, au.WorkflowTitle, url, blankLines.ReplaceAllString(remark, "\n"))
	default: // 已撤回
		ev.Name = notify.EventAborted
		ev.Vars["remark"] = strings.TrimSpace(remark)
		ev.Title = fmt.Sprintf("[%s]提交人主动终止工单#%d", s.kindLabel(w), au.AuditID)                        // i18n-ignore 通知正文（语言属于接收方）
		ev.Content = fmt.Sprintf("发起时间：%s\n发起人：%s\n组：%s\n目标实例：%s\n数据库：%s\n工单名称：%s\n工单地址：%s\n终止原因：%s", // i18n-ignore 通知正文（语言属于接收方）
			created, au.CreateUserDisplay, au.GroupName, instName(w), w.DBName, au.WorkflowTitle, url, remark)
	}
	return ev
}

// send 在后台构造并发送通知：构造内容要读几次库（审批流、SQL 预览），不应该拖慢业务请求或工单执行的启动。
func (s *Service) send(build func() notify.Event) {
	n := s.notifier
	s.async(func() {
		ev := build()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		n.Notify(ctx, ev)
	})
}

// notifyAudit 发审批阶段的通知；失败只记日志，由通知器处理。
func (s *Service) notifyAudit(w *model.SqlWorkflow, au *model.WorkflowAudit, remark string) {
	s.send(func() notify.Event { return s.auditEvent(w, au, remark) })
}

func (s *Service) execContent(w *model.SqlWorkflow, au *model.WorkflowAudit) string {
	return fmt.Sprintf("发起时间：%s\n发起人：%s\n组：%s\n目标实例：%s\n数据库：%s\n审批流程：%s\n工单名称：%s\n工单地址：%s\n工单详情预览：%s", // i18n-ignore 通知正文（语言属于接收方）
		w.CreateTime.Format("2006-01-02 15:04:05"), w.EngineerDisplay, w.GroupName, instName(w), w.DBName,
		s.readableFlow(au), w.WorkflowName, s.workflowURL(w.ID), s.preview(w.ID))
}

// notifyExecute 执行结束（含手工确认结束）的通知，对应 render_execute；DDL 执行完成时按 notify.ddl_extra_groups 额外 @ 相关人员。
func (s *Service) notifyExecute(w *model.SqlWorkflow, au *model.WorkflowAudit) {
	s.send(func() notify.Event { return s.executeEvent(w, au) })
}

func (s *Service) executeEvent(w *model.SqlWorkflow, au *model.WorkflowAudit) notify.Event {
	ev := notify.Event{Kind: notify.KindSQLWorkflow, GroupID: w.GroupID, Applicant: w.Engineer, Name: notify.EventFinished, Vars: s.execVars(w, au)}
	if w.Status == model.WfException {
		ev.Name = notify.EventException
	}
	ev.Title = fmt.Sprintf("[%s]工单%s#%d", s.kindLabel(w), StatusLabel(w.Status), au.AuditID) // i18n-ignore 通知正文（语言属于接收方）
	ev.Content = s.execContent(w, au)
	if sum := ev.Vars["exec_summary"]; sum != "" { // 执行报表：汇总 + 执行记录
		ev.Content += "\n执行汇总：" + sum // i18n-ignore 通知正文（语言属于接收方）
		if lg := ev.Vars["exec_log"]; lg != "" {
			ev.Content += "\n执行记录：\n" + lg // i18n-ignore 通知正文（语言属于接收方）
		}
	}
	if groups := splitComma(cfgStr(s.repo, settings.KeyNotifyDDLExtraGroups)); len(groups) > 0 && w.Status == model.WfFinish && w.SyntaxType == SyntaxDDL {
		if us, err := s.repo.AuthGroupUsers(groups, w.GroupID); err == nil {
			for _, u := range us {
				ev.ExtraUsers = append(ev.ExtraUsers, u.Username)
			}
		}
		ev.Content += fmt.Sprintf("\n\n有新的DDL语句执行完成：变更组 %s，变更实例 %s，变更数据库 %s", w.GroupName, instName(w), w.DBName) // i18n-ignore 通知正文（语言属于接收方）
	}
	return ev
}

func (s *Service) notifyScheduled(w *model.SqlWorkflow, au *model.WorkflowAudit, runAt time.Time) {
	s.send(func() notify.Event {
		vars := s.workflowVars(w, au)
		vars["run_at"] = runAt.Format("2006-01-02 15:04")
		return notify.Event{Kind: notify.KindSQLWorkflow, GroupID: w.GroupID, Applicant: w.Engineer, Name: notify.EventScheduled, Vars: vars,
			Title:   fmt.Sprintf("[%s]工单已设置定时执行#%d", s.kindLabel(w), au.AuditID),           // i18n-ignore 通知正文（语言属于接收方）
			Content: s.execContent(w, au) + "\n计划执行时间：" + runAt.Format("2006-01-02 15:04")} // i18n-ignore 通知正文（语言属于接收方）
	})
}

// notifyRetrying 重试失败实例开始时发“开始执行”通知，并写明重试的是哪个实例（否则群里看不出这次执行是重试）。
func (s *Service) notifyRetrying(w *model.SqlWorkflow, au *model.WorkflowAudit, instName string) {
	s.send(func() notify.Event {
		vars := s.workflowVars(w, au)
		vars["executor"] = "重试实例 " + instName // i18n-ignore 通知正文（语言属于接收方）
		return notify.Event{Kind: notify.KindSQLWorkflow, GroupID: w.GroupID, Applicant: w.Engineer, Name: notify.EventExecuting, Vars: vars,
			Title:   fmt.Sprintf("[%s]工单开始重试实例#%d", s.kindLabel(w), au.AuditID), // i18n-ignore 通知正文（语言属于接收方）
			Content: s.execContent(w, au) + "\n重试实例：" + instName}                // i18n-ignore 通知正文（语言属于接收方）
	})
}

// notifyRetried 重试结束的通知：沿用“执行完成 / 执行异常”事件（订阅它们的渠道照常收到），标题和正文里写明重试的实例与结果。
func (s *Service) notifyRetried(w *model.SqlWorkflow, au *model.WorkflowAudit, instName string, ok bool) {
	s.send(func() notify.Event {
		ev := s.executeEvent(w, au)
		res := "仍然失败" // i18n-ignore 通知正文（语言属于接收方）
		if ok {
			res = "成功" // i18n-ignore 通知正文（语言属于接收方）
		}
		ev.Title = fmt.Sprintf("[%s]工单重试实例%s#%d（%s）", s.kindLabel(w), res, au.AuditID, instName) // i18n-ignore 通知正文（语言属于接收方）
		ev.Content = "重试实例：" + instName + "（" + res + "）\n" + ev.Content                         // i18n-ignore 通知正文（语言属于接收方）
		if ev.Vars != nil {
			ev.Vars["retry_instance"] = instName
		}
		return ev
	})
}

func (s *Service) notifyExecuting(w *model.SqlWorkflow, au *model.WorkflowAudit, executor string) {
	s.send(func() notify.Event {
		vars := s.workflowVars(w, au)
		vars["executor"] = executorLabel(executor)
		return notify.Event{Kind: notify.KindSQLWorkflow, GroupID: w.GroupID, Applicant: w.Engineer, Name: notify.EventExecuting, Vars: vars,
			Title:   fmt.Sprintf("[%s]工单开始执行#%d", s.kindLabel(w), au.AuditID),   // i18n-ignore 通知正文（语言属于接收方）
			Content: s.execContent(w, au) + "\n执行方式：" + executorLabel(executor)} // i18n-ignore 通知正文（语言属于接收方）
	})
}
