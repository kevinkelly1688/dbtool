// @author: kevinkelly
// @license: Apache Licence
// @file: actions.go
// @time: 2026/10/28

package perf

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dbtool/internal/settings"
)

// 工单状态机。所有流转都在服务端校验“谁能做、当前状态能不能做”，并用 update_time 做乐观锁，
// 两个人同时点按钮只有一个成功，另一个得到 409 提示刷新。

// ActionInput 是一次状态流转的请求。
type ActionInput struct {
	Action         string `json:"action"`
	Remark         string `json:"remark"`
	PlanFinishDate string `json:"plan_finish_date"` // accept：承诺完成日 yyyy-mm-dd
	ExtendDate     string `json:"extend_date"`      // extend：申请延期至 yyyy-mm-dd
	AssigneeID     uint   `json:"assignee_id"`      // assign：新负责人
	TeamID         uint   `json:"team_id"`          // assign：新团队（0 表示不变）
	Severity       string `json:"severity"`         // severity：新级别
	OwnerSide      string `json:"owner_side"`       // side：新处理方
	WorkflowID     uint   `json:"workflow_id"`      // fix：关联的 SQL 上线工单号（可空）
}

type actionDef struct {
	name  string
	label string
	dba   bool     // 需要 DBA（持有管理类按钮）
	from  []string // 允许的当前状态；为空表示不限
}

// actionDefs 按按钮展示顺序排列。
var actionDefs = []actionDef{
	{ActAccept, "受理", false, []string{StNew, StAssigned}},
	{ActFix, "已优化上线，申请验证", false, []string{StAssigned, StAccepted}},
	{ActExtend, "申请延期", false, slaRunning},
	{ActWaive, "申请豁免", false, slaRunning},
	{ActComment, "评论", false, nil},
	{ActAssign, "指派", true, openStatuses},
	{ActVerifyPass, "验证通过", true, []string{StFixed}},
	{ActVerifyFail, "验证不通过", true, []string{StFixed}},
	{ActExtendApprove, "同意延期", true, openStatuses},
	{ActExtendReject, "驳回延期", true, openStatuses},
	{ActWaiveApprove, "同意豁免", true, []string{StWaivePending}},
	{ActWaiveReject, "驳回豁免", true, []string{StWaivePending}},
	{ActSeverity, "调整级别", true, openStatuses},
	{ActSide, "切换处理方", true, openStatuses},
	{ActCloseDup, "误报关闭", true, openStatuses},
	{ActReopen, "重开", true, []string{StClosed}},
	{ActDiagnose, "重新诊断", true, nil},
}

func findAction(name string) *actionDef {
	for i := range actionDefs {
		if actionDefs[i].name == name {
			return &actionDefs[i]
		}
	}
	return nil
}

// availableActions 返回当前用户在该工单当前状态下能做的操作（详情页据此显示按钮）。
func (s *Service) availableActions(v Viewer, i *Issue) []ActionDTO {
	if !v.CanHandle(i) {
		return []ActionDTO{}
	}
	out := []ActionDTO{}
	for _, d := range actionDefs {
		if d.dba && !v.IsDBA() {
			continue
		}
		if len(d.from) > 0 && !contains(d.from, i.Status) {
			continue
		}
		switch d.name {
		case ActExtend:
			if i.PendingExtendDate != nil || i.ExtendTimes >= s.policy(i.Severity).MaxExtendTimes {
				continue
			}
		case ActWaive:
			if i.PendingExtendDate != nil {
				continue
			}
		case ActExtendApprove, ActExtendReject:
			if i.PendingExtendDate == nil {
				continue
			}
			if d.name == ActExtendApprove && s.requesterOf(i) == v.Username {
				continue // 自己提交的申请不能自己批（仍可驳回 = 撤回）
			}
		case ActWaiveApprove:
			if s.requesterOf(i) == v.Username {
				continue
			}
		}
		out = append(out, ActionDTO{Name: d.name, Label: d.label, DBA: d.dba})
	}
	return out
}

// requesterOf 返回当前待审批的延期 / 豁免申请的申请人（登录名）：取工单日志里最近一条对应的申请记录。
// 没有待审批的申请或查不到记录返回空串（此时不限制，避免因日志缺失把审批卡死）。
func (s *Service) requesterOf(i *Issue) string {
	act := ""
	switch {
	case i.PendingExtendDate != nil:
		act = ActExtend
	case i.Status == StWaivePending:
		act = ActWaive
	default:
		return ""
	}
	logs, err := s.repo.ListLogs(i.ID, 200) // 新的在前
	if err != nil {
		return ""
	}
	for _, l := range logs {
		if l.Action == act {
			return l.Operator
		}
	}
	return ""
}

const (
	maxRemarkLen  = 2000 // 备注/说明
	maxPendingLen = 1000 // 申请理由（dbt_perf_issue.pending_reason 是 varchar(1000)）
)

// event 是一次状态变化要发的通知。
type event struct {
	name   string
	actor  string
	reason string
	remark string
	oldSev string
}

// Act 执行一次状态流转。成功返回更新后的工单状态。
func (s *Service) Act(v Viewer, id uint, in ActionInput) (*ActionResult, error) {
	def := findAction(in.Action)
	if def == nil || in.Action == ActDiagnose {
		return nil, bad("未知操作")
	}
	i, err := s.repo.GetIssue(id)
	if err != nil {
		return nil, err
	}
	if i == nil || !v.CanHandle(i) {
		return nil, notFound("工单不存在或无权查看")
	}
	if def.dba && !v.IsDBA() {
		return nil, forbid("需要 DBA(性能工单管理) 权限")
	}
	if len(def.from) > 0 && !contains(def.from, i.Status) {
		return nil, conflict(fmt.Sprintf("当前状态（%s）不能“%s”，请刷新页面", statusLabel[i.Status], def.label))
	}
	remark := strings.TrimSpace(in.Remark)
	if len([]rune(remark)) > maxRemarkLen {
		return nil, bad(fmt.Sprintf("备注过长（最多 %d 个字符）", maxRemarkLen))
	}
	now := s.now()
	w := whoOf(v.Actor)
	prev := i.UpdateTime
	var (
		logText string
		ev      *event
	)
	switch in.Action {
	case ActComment:
		if remark == "" {
			return nil, bad("请填写内容")
		}
		s.record(i, ActComment, remark, w)
		s.notifyIssue(i.ID, event{name: evCommented, actor: w.display, remark: remark}, v.ID)
		return &ActionResult{ID: i.ID, Status: i.Status}, nil

	case ActAccept:
		if strings.TrimSpace(in.PlanFinishDate) == "" {
			return nil, bad("请填写承诺完成日期")
		}
		plan, err := parseDate(in.PlanFinishDate, now.Location())
		if err != nil {
			return nil, bad("承诺完成日期格式不对，应为 yyyy-mm-dd")
		}
		if plan.Before(dateOf(now)) {
			return nil, bad("承诺日期不能早于今天")
		}
		lateAccept := i.ResolveDeadline != nil && now.After(*i.ResolveDeadline) // 受理时 SLA 解决截止已过
		if i.ResolveDeadline != nil && plan.After(maxPlanDate(*i.ResolveDeadline, now)) {
			if dateOf(*i.ResolveDeadline).Before(dateOf(now)) {
				return nil, bad(fmt.Sprintf("SLA 解决截止日 %s 已过，承诺日期不能晚于今天（%s）；需要更晚请先受理后走【申请延期】", i.ResolveDeadline.Format(dateLayout), dateOf(now).Format(dateLayout)))
			}
			return nil, bad(fmt.Sprintf("承诺日期超过 SLA 截止日 %s，请先受理后走【申请延期】", i.ResolveDeadline.Format(dateLayout)))
		}
		if i.AssigneeID == nil {
			i.AssigneeID = ptr(v.ID)
		}
		i.Status, i.AcceptedAt, i.PlanFinishDate = StAccepted, &now, &plan
		logText = fmt.Sprintf("受理，承诺 %s 前完成。%s", plan.Format(dateLayout), remark)
		if lateAccept {
			logText = fmt.Sprintf("受理（已超时受理：SLA 解决截止 %s 已过），承诺 %s 前完成。%s", i.ResolveDeadline.Format(dateLayout), plan.Format(dateLayout), remark)
		}

	case ActFix:
		if remark == "" {
			return nil, bad("请填写优化方案/上线说明（如：改写分页，版本 v2.3.1 已上线）")
		}
		i.Status, i.FixedAt, i.FixDesc = StFixed, &now, remark
		if i.AcceptedAt == nil {
			i.AcceptedAt = &now
		}
		i.BaselineAvgMs, i.BaselineDailySec = i.AvgTimeMs, i.ImpactScore
		logText = remark
		if err := s.checkWorkflow(in.WorkflowID); err != nil {
			return nil, err
		}
		if in.WorkflowID != 0 {
			logText = fmt.Sprintf("%s（关联 SQL 上线工单 #%d）", remark, in.WorkflowID)
		}
		ev = &event{name: evFixed, actor: w.display}

	case ActExtend:
		p := s.policy(i.Severity)
		if i.PendingExtendDate != nil {
			return nil, conflict("已有待审批的延期申请")
		}
		if i.ExtendTimes >= p.MaxExtendTimes {
			return nil, bad(fmt.Sprintf("%s 最多延期 %d 次，请联系DBA", i.Severity, p.MaxExtendTimes))
		}
		nd, err := parseDate(in.ExtendDate, now.Location())
		if err != nil {
			return nil, bad("请填写延期日期（yyyy-mm-dd）")
		}
		if nd.Before(dateOf(now)) || (i.ResolveDeadline != nil && !nd.After(dateOf(*i.ResolveDeadline))) {
			return nil, bad("延期日期必须晚于当前的解决截止日")
		}
		if nd.After(dateOf(now).AddDate(0, 0, 365)) {
			return nil, bad("延期日期过远（最多一年）")
		}
		if remark == "" {
			return nil, bad("请填写延期理由")
		}
		if len([]rune(remark)) > maxPendingLen {
			return nil, bad(fmt.Sprintf("申请理由过长（最多 %d 个字符）", maxPendingLen))
		}
		i.PendingExtendDate, i.PendingReason = &nd, remark
		logText = fmt.Sprintf("申请延期至 %s：%s", nd.Format(dateLayout), remark)
		ev = &event{name: evExtendReq, actor: w.display}

	case ActWaive:
		if i.PendingExtendDate != nil {
			return nil, conflict("已有待审批的延期申请，请等待审批后再申请豁免")
		}
		if len([]rune(remark)) < 10 {
			return nil, bad("请详细说明豁免理由（不少于10字，如：报表类低频查询，业务可接受）")
		}
		if len([]rune(remark)) > maxPendingLen {
			return nil, bad(fmt.Sprintf("申请理由过长（最多 %d 个字符）", maxPendingLen))
		}
		i.PendingReason, i.Status = remark, StWaivePending
		logText = remark
		ev = &event{name: evWaiveReq, actor: w.display}

	case ActAssign:
		if in.AssigneeID == 0 {
			return nil, bad("请选择负责人")
		}
		users, err := s.repo.GetUsers([]uint{in.AssigneeID})
		if err != nil {
			return nil, err
		}
		u, ok := users[in.AssigneeID]
		if !ok || !u.Active {
			return nil, bad("所选负责人不存在或已停用")
		}
		i.AssigneeID = ptr(u.ID)
		teamName := "-"
		if in.TeamID != 0 {
			g, err := s.group(in.TeamID)
			if err != nil {
				return nil, err
			}
			i.TeamID = ptr(g.ID)
		}
		if i.TeamID != nil {
			if g, _ := s.group(*i.TeamID); g != nil {
				teamName = g.Name
			}
		}
		if i.Status == StNew {
			i.Status = StAssigned
		}
		logText = fmt.Sprintf("指派给 %s（%s）。%s", u.Name(), teamName, remark)
		ev = &event{name: evAssigned, actor: w.display}

	case ActVerifyPass:
		closeIssue(i, CloseVerified, now)
		logText = fmt.Sprintf("[%s] DBA 验证通过。%s", closeLabel[CloseVerified], remark)
		ev = &event{name: evClosed, reason: fmt.Sprintf("DBA 验证通过。%s", remark)}

	case ActVerifyFail:
		if remark == "" {
			return nil, bad("请填写验证不通过的原因")
		}
		i.Status = StAccepted
		i.ReopenCount++
		logText = remark
		ev = &event{name: evVerifyFail, reason: remark}

	case ActExtendApprove, ActExtendReject:
		if i.PendingExtendDate == nil {
			return nil, conflict("没有待审批的延期申请")
		}
		if in.Action == ActExtendApprove && s.requesterOf(i) == v.Username {
			return nil, forbid("不能审批自己提交的延期申请，请其他 DBA 处理")
		}
		if in.Action == ActExtendApprove {
			old := i.ResolveDeadline
			nd := time.Date(i.PendingExtendDate.Year(), i.PendingExtendDate.Month(), i.PendingExtendDate.Day(), 23, 59, 59, 0, now.Location())
			i.ResolveDeadline = &nd
			i.ExtendTimes++
			i.EscalationLevel, i.PreWarned = 0, false
			oldS := "-"
			if old != nil {
				oldS = old.Format(dateLayout)
			}
			logText = fmt.Sprintf("同意延期：%s -> %s。%s", oldS, nd.Format(dateLayout), remark)
			ev = &event{name: evExtendApprove, reason: logText}
		} else {
			logText = fmt.Sprintf("驳回延期申请。%s", remark)
			ev = &event{name: evExtendReject, reason: logText}
		}
		i.PendingExtendDate, i.PendingReason = nil, ""

	case ActWaiveApprove:
		if s.requesterOf(i) == v.Username {
			return nil, forbid("不能审批自己提交的豁免申请，请其他 DBA 处理")
		}
		reason := i.PendingReason
		closeIssue(i, CloseWaived, now)
		i.PendingReason = ""
		logText = fmt.Sprintf("[%s] 同意豁免：%s。%s", closeLabel[CloseWaived], reason, remark)
		ev = &event{name: evClosed, reason: fmt.Sprintf("同意豁免：%s。%s", reason, remark)}

	case ActWaiveReject:
		switch {
		case i.AcceptedAt != nil:
			i.Status = StAccepted
		case i.AssigneeID != nil:
			i.Status = StAssigned
		default:
			i.Status = StNew
		}
		i.PendingReason = ""
		logText = fmt.Sprintf("驳回豁免。%s", remark)
		ev = &event{name: evWaiveReject, reason: remark}

	case ActSeverity:
		if _, ok := severityRank[in.Severity]; !ok {
			return nil, bad("级别不合法")
		}
		if in.Severity == i.Severity {
			return nil, bad("级别没有变化")
		}
		old := i.Severity
		i.Severity, i.SeverityLocked = in.Severity, true
		if isSLARunning(i.Status) {
			base := now
			if severityRank[in.Severity] < severityRank[old] { // 调高级别：时限从建单时起算
				base = i.CreateTime
			}
			setDeadlines(i, s.policy(in.Severity), base, false)
		}
		logText = fmt.Sprintf("人工调整级别 %s -> %s（锁定，不再自动调整）。%s", old, in.Severity, remark)

	case ActSide:
		if sideLabel[in.OwnerSide] == "" {
			return nil, bad("处理方不合法")
		}
		if in.OwnerSide == i.OwnerSide {
			return nil, bad("处理方没有变化")
		}
		i.OwnerSide = in.OwnerSide
		if in.OwnerSide == SideDBA {
			if i.DBAID != nil {
				i.AssigneeID = i.DBAID
			} else {
				i.AssigneeID = ptr(v.ID)
			}
			if i.Status == StNew {
				i.Status = StAssigned
			}
		} else if r, err := s.reroute(i); err == nil && r.assigneeID != 0 {
			i.AssigneeID = ptr(r.assigneeID)
			if i.TeamID == nil && r.teamID != 0 {
				i.TeamID = ptr(r.teamID)
			}
			if i.Status == StNew {
				i.Status = StAssigned
			}
		}
		logText = fmt.Sprintf("处理方改为 %s。%s", sideLabel[i.OwnerSide], remark)
		ev = &event{name: evAssigned, actor: w.display}

	case ActCloseDup:
		if remark == "" {
			return nil, bad("请填写原因")
		}
		closeIssue(i, CloseDuplicate, now)
		logText = "[" + closeLabel[CloseDuplicate] + "] " + remark
		ev = &event{name: evClosed, reason: remark}

	case ActReopen:
		if remark == "" {
			remark = "DBA 手动重开"
		}
		s.reopenIssue(i, now)
		logText = remark
		ev = &event{name: evReopened, reason: remark}

	default:
		return nil, bad("未知操作")
	}

	if err := s.repo.UpdateIssue(i, prev); err != nil {
		if err == ErrConflict {
			return nil, conflict("工单刚刚被他人更新，请刷新页面后重试")
		}
		return nil, err
	}
	if in.Action == ActFix {
		s.saveWorkflowLink(i.ID, in.WorkflowID)
	}
	logAct := in.Action
	switch in.Action {
	case ActVerifyPass, ActWaiveApprove, ActCloseDup:
		logAct = ActClose // 工单时间线仍是“关闭”（内容里有原因标签）；系统审计用各自的动作名，便于区分
	}
	s.recordAs(i, logAct, in.Action, logText, w)
	if ev != nil {
		ev.actor = w.display
		s.notifyIssue(i.ID, *ev, 0)
	}
	return &ActionResult{ID: i.ID, Status: i.Status}, nil
}

// ActionResult 是流转后的工单状态。
type ActionResult struct {
	ID     uint   `json:"id"`
	Status string `json:"status"`
}

// closeIssue 关闭工单。以研发“标记修复”的时间判定是否按时，验证期不算研发超时。
func closeIssue(i *Issue, reason string, now time.Time) {
	i.Status, i.CloseReason = StClosed, reason
	t := now
	i.ClosedAt = &t
	if i.ResolveDeadline != nil && now.After(*i.ResolveDeadline) && reason != CloseDuplicate {
		done := now
		if i.FixedAt != nil {
			done = *i.FixedAt
		}
		if done.After(*i.ResolveDeadline) {
			i.SLABreached = true
		}
	}
}

// reopenIssue 重开：回到待受理（没有负责人则待分派），重新评估级别并重置时限。
func (s *Service) reopenIssue(i *Issue, now time.Time) {
	if i.AssigneeID != nil {
		i.Status = StAssigned
	} else {
		i.Status = StNew
	}
	i.CloseReason, i.ClosedAt = "", nil
	i.ReopenCount++
	if !i.SeverityLocked {
		i.Severity = s.evaluateSeverity(i)
	}
	setDeadlines(i, s.policy(i.Severity), now, false)
}

// Rediagnose 重新诊断（DBA）：可以粘贴真实 SQL 替换样例后重跑。处理方不自动改，避免已指派的工单被来回踢。
func (s *Service) Rediagnose(ctx context.Context, v Viewer, id uint, sql string) (*DiagResult, error) {
	if !v.IsDBA() {
		return nil, forbid("需要 DBA(性能工单管理) 权限")
	}
	i, err := s.repo.GetIssue(id)
	if err != nil {
		return nil, err
	}
	if i == nil {
		return nil, notFound("工单不存在")
	}
	inst, err := s.repo.GetInstance(i.InstanceID)
	if err != nil {
		return nil, err
	}
	cfg := s.collectConfigFor(i.InstanceID, inst)
	text := i.SampleSQL
	if strings.TrimSpace(sql) != "" {
		if len(sql) > 200000 {
			return nil, bad("SQL 过长")
		}
		if err := validateSampleSQL(sql); err != nil {
			return nil, bad(err.Error())
		}
		text = strings.TrimSpace(sql)
	}
	res := Diagnose(ctx, s.explainer, DiagnoseInput{Instance: inst, Product: cfg.Product, MinAvgMs: cfg.MinAvgMs, DB: i.DBName, SQL: text,
		Metrics: Metrics{i.AvgTimeMs, i.RowsSentAvg, i.RowsExaminedAvg, i.PerDayCount}, SampleReal: true,
		AutoExplain: s.cfgBool(settings.KeyPerfAutoExplain, true), TimeoutMs: s.cfgInt(settings.KeyPerfExplainTimeoutMs, 3000)})
	prev := i.UpdateTime
	i.Category, i.Diagnosis, i.Suggestion, i.SuggestedDDL, i.ExplainResult = res.Category, res.Diagnosis, res.Suggestion, res.SuggestedDDL, res.ExplainResult
	if strings.TrimSpace(sql) != "" {
		i.SampleSQL = text
	}
	if err := s.repo.UpdateIssue(i, prev); err != nil {
		if err == ErrConflict {
			return nil, conflict("工单刚刚被他人更新，请刷新页面后重试")
		}
		return nil, err
	}
	s.record(i, ActDiagnose, fmt.Sprintf("重新诊断：%s", labelOf(Categories, i.Category)), whoOf(v.Actor))
	return &res, nil
}
