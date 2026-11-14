// @author: kevinkelly
// @license: Apache Licence
// @file: flowview.go
// @time: 2026/10/28

package sqlflow

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/i18n"
	"dbtool/internal/model"
)

// 审批流的展示：提交页的审批流预览、详情页的审批时间线与当前级可审批人。
// 可审批人的判定直接用审批引擎的 audit.CanReview（与真正审批时完全一致：权限码、权限组、资源组、禁止自审、超级用户）。

// ApproverView 是某一级的一个可审批人。
type ApproverView struct {
	Username string `json:"username"`
	Display  string `json:"display"`
	Super    bool   `json:"super,omitempty"`
}

// FlowLevelView 是审批流里的一级。
type FlowLevelView struct {
	Level     int            `json:"level"`
	GroupID   uint           `json:"group_id"`
	GroupName string         `json:"group_name"`
	Approvers []ApproverView `json:"approvers"`
	Count     int            `json:"approver_count"`    // 可审批人数（提交页预览只显示审批组，鼠标悬停时才显示具体人员）
	Warning   string         `json:"warning,omitempty"` // 非空表示这一级无人可审（或权限组不存在）
}

// FlowPreview 是提交页展示的审批流：提交到某个资源组后会走哪几级、每级有谁能审。
type FlowPreview struct {
	GroupID    uint            `json:"group_id"`
	GroupName  string          `json:"group_name"`
	Configured bool            `json:"configured"` // 资源组配置了 SQL 上线审批流；没配置则提交会被拒绝
	Auto       bool            `json:"auto"`       // 配置为空流程：提交后系统直接通过
	Levels     []FlowLevelView `json:"levels"`
	Warnings   []string        `json:"warnings"`
}

const maxApproversShown = 50

// candidates 返回某一级权限组里能审批这张（假想的或真实的）工单的人。
func (s *Service) candidates(ac *auditCache, au *model.WorkflowAudit, authGroupID uint) ([]ApproverView, error) {
	g, err := ac.AuthGroup(authGroupID)
	if err != nil || g == nil {
		return nil, err
	}
	users, err := ac.AuthGroupUsers([]string{g.Name}, au.GroupID)
	if err != nil {
		return nil, err
	}
	seen := map[uint]bool{}
	var out []ApproverView
	for _, u := range users {
		if seen[u.ID] {
			continue
		}
		seen[u.ID] = true
		cur := *au
		cur.CurrentAudit, cur.CurrentStatus = strconv.Itoa(int(authGroupID)), audit.StatusWaiting
		if !audit.CanReview(ac, &cur, audit.Actor{ID: u.ID, Username: u.Username, Display: u.Display, IsSuperuser: u.IsSuperuser}) {
			continue
		}
		d := u.Display
		if d == "" {
			d = u.Username
		}
		out = append(out, ApproverView{Username: u.Username, Display: d, Super: u.IsSuperuser})
		if len(out) >= maxApproversShown {
			break
		}
	}
	return out, nil
}

// FlowPreview 按提交人所在的资源组读取 SQL 上线审批流，并算出每一级的可审批人，提示“某级无人可审”。
// 只给自己所在资源组的人看（和提交时的校验一致）。
func (s *Service) FlowPreview(p audit.Actor, groupID uint) (*FlowPreview, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	if !pm.groups[groupID] {
		return nil, forbid("你不在该资源组内")
	}
	g, err := s.repo.ResourceGroupByID(groupID)
	if err != nil {
		return nil, err
	}
	if g == nil || g.IsDeleted != 0 {
		return nil, bad("不存在该资源组")
	}
	out := &FlowPreview{GroupID: g.GroupID, GroupName: g.GroupName, Levels: []FlowLevelView{}, Warnings: []string{}}
	st, err := s.repo.AuditSetting(groupID, audit.TypeSQLReview)
	if err != nil {
		return nil, err
	}
	if st == nil {
		out.Warnings = append(out.Warnings, "该资源组还没有配置 SQL 上线审批流，提交会被拒绝，请联系管理员。")
		return out, nil
	}
	out.Configured = true
	ids := splitAuditIDs(st.AuditAuthGroups)
	if len(ids) == 0 {
		out.Auto = true
		return out, nil
	}
	ac := newAuditCache(s.repo)
	fake := &model.WorkflowAudit{GroupID: groupID, GroupName: g.GroupName, WorkflowType: audit.TypeSQLReview, CreateUser: p.Username, CurrentStatus: audit.StatusWaiting}
	for i, raw := range ids {
		lv := FlowLevelView{Level: i + 1, GroupName: raw, Approvers: []ApproverView{}}
		gid, perr := strconv.ParseUint(raw, 10, 32)
		if perr != nil {
			lv.Warning = fmt.Sprintf("审批流配置里含有无法识别的内容：%s", raw)
		} else {
			lv.GroupID = uint(gid)
			ag, err := ac.AuthGroup(uint(gid))
			switch {
			case err != nil:
				return nil, err
			case ag == nil:
				lv.Warning = "审批权限组不存在（可能已被删除），这一级无人可审，工单会卡住"
			default:
				lv.GroupName = ag.Name
				ap, err := s.candidates(ac, fake, uint(gid))
				if err != nil {
					return nil, err
				}
				if len(ap) > 0 {
					lv.Approvers, lv.Count = ap, len(ap)
				} else {
					lv.Warning = fmt.Sprintf("没有符合条件的审批人（资源组内属于「%s」且有审批权限（sqlworkflow.approve）的启用用户；提交人本人是否可自审取决于系统设置）。仅超级用户可以审批", ag.Name)
				}
			}
		}
		if lv.Warning != "" {
			out.Warnings = append(out.Warnings, fmt.Sprintf("第 %d 级「%s」：%s", lv.Level, lv.GroupName, lv.Warning))
		}
		out.Levels = append(out.Levels, lv)
	}
	return out, nil
}

// ---- 详情页的审批时间线 ----

// 时间线步骤的状态。
const (
	StepDone     = "done"     // 已完成
	StepCurrent  = "current"  // 正在审批
	StepPending  = "pending"  // 还没轮到
	StepRejected = "rejected" // 在这一级被驳回
	StepAborted  = "aborted"  // 提交人撤回
	StepSkipped  = "skipped"  // 流程已结束，没走到这一级
	StepAuto     = "auto"     // 系统自动处理
)

// FlowStep 是审批时间线的一步：提交、每一级审批、系统自动审核。
type FlowStep struct {
	Title     string         `json:"title"`
	State     string         `json:"state"`
	Who       string         `json:"who,omitempty"`
	Time      *time.Time     `json:"time,omitempty"`
	Remark    string         `json:"remark,omitempty"`
	Approvers []ApproverView `json:"approvers,omitempty"` // 这一级现在谁能审批（页面不列出，鼠标悬停在审批组上才显示）
	// NoApprover 为真表示“正在审批 / 还没轮到”的这一级没有任何人能审批（只有超级用户能处理）；已结束的级别不标。
	NoApprover bool `json:"no_approver,omitempty"`
}

// Tip 是时间线上鼠标悬停在审批组上显示的文字：这一级现在有哪些人能审批；没人时说明原因。无审批组（提交 / 自动审核）为空。
func (s FlowStep) Tip() string { return s.TipFor(i18n.ZH) }

// TipFor 同 Tip，按界面语言输出（详情页模板里 {{.TipFor lang}}）。
func (s FlowStep) TipFor(lang string) string {
	if s.NoApprover {
		return i18n.T(lang, "本级没有符合条件的审批人，只有超级用户能审批，请联系管理员检查审批流和权限。")
	}
	if len(s.Approvers) == 0 {
		return ""
	}
	names := make([]string, 0, len(s.Approvers))
	for _, a := range s.Approvers {
		n := a.Display
		if a.Super {
			n += i18n.T(lang, "（超级用户）")
		}
		names = append(names, n)
	}
	tip := i18n.T(lang, "可审批人员（{n}）：{names}", "n", len(names), "names", strings.Join(names, i18n.T(lang, "、")))
	if len(names) >= maxApproversShown {
		tip += i18n.T(lang, "……（最多列出 {n} 人）", "n", maxApproversShown)
	}
	return tip
}

// parseRemark 取出审批日志里的备注：“审批备注：xxx, 下级审批：yyy” / “审批备注: xxx, 无下级审批” / “取消原因：xxx” / “审批备注：xxx”。
func parseRemark(info string) string {
	s := strings.TrimSpace(info)
	for _, pre := range []string{"审批备注：", "审批备注:", "取消原因：", "取消原因:"} { // i18n-ignore 与审批引擎写进日志的原文做匹配
		if strings.HasPrefix(s, pre) {
			s = strings.TrimSpace(strings.TrimPrefix(s, pre))
			break
		}
	}
	if i := strings.LastIndex(s, ", 下级审批："); i >= 0 { // i18n-ignore 与审批引擎写进日志的原文做匹配
		s = s[:i]
	} else if strings.HasSuffix(s, ", 无下级审批") { // i18n-ignore 与审批引擎写进日志的原文做匹配
		s = strings.TrimSuffix(s, ", 无下级审批") // i18n-ignore 与审批引擎写进日志的原文做匹配
	}
	return strings.TrimSpace(s)
}

func displayOf(l model.WorkflowLog) string {
	if l.OperatorDisplay != "" {
		return l.OperatorDisplay
	}
	return l.Operator
}

// buildSteps 用审批流定义 + 审批日志还原每一级的审批人、时间、意见；每一级都补上现在能审批的人（页面悬停才显示）。
func (s *Service) buildSteps(ac *auditCache, w *model.SqlWorkflow, au *model.WorkflowAudit, logs []model.WorkflowLog) []FlowStep {
	var submit *model.WorkflowLog
	var passes []model.WorkflowLog
	var reject, abort *model.WorkflowLog
	for i := range logs {
		l := logs[i]
		switch l.OperationType {
		case audit.ActionSubmit:
			if submit == nil {
				submit = &logs[i]
			}
		case audit.ActionPass:
			passes = append(passes, l)
		case audit.ActionReject:
			if reject == nil {
				reject = &logs[i]
			}
		case audit.ActionAbort:
			if abort == nil {
				abort = &logs[i]
			}
		}
	}
	steps := []FlowStep{}
	st := FlowStep{Title: "提交", State: StepDone, Who: w.EngineerDisplay}
	st.Time = &w.CreateTime
	if submit != nil {
		t := submit.OperationTime
		st.Time = &t
		if d := displayOf(*submit); d != "" {
			st.Who = d
		}
	}
	steps = append(steps, st)

	ids := splitAuditIDs(au.AuditAuthGroups)
	if len(ids) == 0 {
		a := FlowStep{Title: "系统自动审核", State: StepAuto}
		if submit != nil {
			a.Remark = submit.OperationInfo
			t := submit.OperationTime
			a.Time = &t
		}
		return append(steps, a)
	}
	done := len(passes)
	for i, raw := range ids {
		step := FlowStep{Title: fmt.Sprintf("第 %d 级 · %s", i+1, raw), State: StepPending}
		gid, perr := strconv.ParseUint(raw, 10, 32)
		var cands []ApproverView
		candOK := false
		if perr == nil {
			if g, _ := ac.AuthGroup(uint(gid)); g != nil {
				step.Title = fmt.Sprintf("第 %d 级 · %s", i+1, g.Name)
			}
			// 每一级都算一份“能审批的人”：页面上只显示审批组，鼠标悬停在审批组上才列出人员
			var cerr error
			cands, cerr = s.candidates(ac, au, uint(gid))
			candOK = cerr == nil
		}
		switch {
		case i < done:
			l := passes[i]
			t := l.OperationTime
			step.State, step.Who, step.Time, step.Remark = StepDone, displayOf(l), &t, parseRemark(l.OperationInfo)
		case i == done && au.CurrentStatus == audit.StatusWaiting:
			step.State = StepCurrent
		case i == done && au.CurrentStatus == audit.StatusRejected && reject != nil:
			t := reject.OperationTime
			step.State, step.Who, step.Time, step.Remark = StepRejected, displayOf(*reject), &t, parseRemark(reject.OperationInfo)
		case i == done && au.CurrentStatus == audit.StatusAborted && abort != nil:
			t := abort.OperationTime
			step.State, step.Who, step.Time, step.Remark = StepAborted, displayOf(*abort), &t, parseRemark(abort.OperationInfo)
		case au.CurrentStatus != audit.StatusWaiting:
			step.State = StepSkipped
		}
		step.Approvers = cands
		step.NoApprover = candOK && len(cands) == 0 && (step.State == StepCurrent || step.State == StepPending)
		steps = append(steps, step)
	}
	// 审批流走完（审核通过）之后才被驳回 / 终止的工单：补一步，免得终止的人和原因丢失
	if done >= len(ids) {
		for _, x := range []struct {
			l     *model.WorkflowLog
			title string
			state string
		}{{reject, "审批通过后被驳回", StepRejected}, {abort, "审批通过后被撤回", StepAborted}} {
			if x.l != nil {
				t := x.l.OperationTime
				steps = append(steps, FlowStep{Title: x.title, State: x.state, Who: displayOf(*x.l), Time: &t, Remark: parseRemark(x.l.OperationInfo)})
			}
		}
	}
	return steps
}

// ---- 复制为新工单 ----

// CopySource 是“复制为新工单”带出的内容：名称、实例、库、SQL 等，提交页据此预填。
type CopySource struct {
	Name             string `json:"name"`
	DemandURL        string `json:"demand_url"`
	GroupName        string `json:"group_name"`
	InstanceID       uint   `json:"instance_id"`
	BatchInstanceIDs []uint `json:"instance_ids,omitempty"`
	DBName           string `json:"db_name"`
	SQL              string `json:"sql"`
	IsBackup         bool   `json:"is_backup"`
	IsExport         bool   `json:"is_export"`
	ExportFormat     string `json:"export_format,omitempty"`
}

// isTerminal 是否已经结束（正常结束、异常结束、被终止、自动驳回），只有这些工单可以复制为新工单。
func isTerminal(status string) bool {
	switch status {
	case model.WfFinish, model.WfException, model.WfAbort, model.WfAutoReviewWrong:
		return true
	}
	return false
}

// CopySource 返回终态工单可复制的内容。看不到的工单按不存在处理；还没结束的工单不能复制（避免同一份变更被重复提交）。
func (s *Service) CopySource(p audit.Actor, id uint) (*CopySource, error) {
	w, err := s.load(id, false)
	if err != nil {
		return nil, err
	}
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	if !pm.canView(p, w) {
		return nil, notFound("工单不存在")
	}
	if !isTerminal(w.Status) {
		return nil, conflict(fmt.Sprintf("工单当前状态为「%s」，只有已结束或已终止的工单才能复制为新工单", StatusLabel(w.Status)))
	}
	c, err := s.repo.GetContent(id)
	if err != nil {
		return nil, err
	}
	out := &CopySource{Name: w.WorkflowName, DemandURL: w.DemandURL, GroupName: w.GroupName, InstanceID: w.InstanceID, DBName: w.DBName,
		IsBackup: w.IsBackup, BatchInstanceIDs: parseBatchIDs(w.BatchInstanceIDs)}
	if c != nil {
		out.SQL = c.SQLContent
		mk, err := s.maskerFor(&p, w)
		if err != nil {
			return nil, err
		}
		if masked := mk.SQL(c.SQLContent); mk.Changed() && masked != c.SQLContent {
			return nil, conflict("该工单的 SQL 里含有已配置脱敏的字段值，只有超级用户可以复制；请重新编写后提交") // 复制出来的是脱敏后的文本，不能提交
		}
	}
	if w.IsOfflineExport != 0 {
		out.IsExport, out.ExportFormat = true, exportFormat(w)
	}
	return out, nil
}

// ---- 取消定时 ----

// CancelSchedule 取消定时执行：工单从“定时执行”回到“审核通过”，可以重新设置定时或立即执行（和终止工单不同，工单本身保留）。
// 权限与终止一致：提交人，或有执行权限的人。
func (s *Service) CancelSchedule(p audit.Actor, id uint) (*model.SqlWorkflow, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	var w *model.SqlWorkflow
	var au *model.WorkflowAudit
	err = s.repo.InTx(func(tx Repo) error {
		var err error
		if w, au, err = lockedWorkflow(tx, id); err != nil {
			return err
		}
		if err := s.visible(p, pm, w); err != nil {
			return err
		}
		if w.Status != model.WfTimingTask {
			return conflict(fmt.Sprintf("工单当前状态为「%s」，没有可取消的定时", StatusLabel(w.Status)))
		}
		if w.Engineer != p.Username && !pm.canExecute(p, w) {
			return forbid("你无权操作当前工单！")
		}
		w.Status = model.WfReviewPass
		if err := tx.SaveWorkflow(w); err != nil {
			return err
		}
		if err := tx.DeleteSchedule(w.ID); err != nil {
			return err
		}
		return s.addLog(tx, au.AuditID, opSchedule, "取消定时", "已取消定时执行，工单回到「审核通过」，可重新设置定时或立即执行", &p)
	})
	if err != nil {
		return nil, translate(err)
	}
	s.sysAudit(p, "取消定时", wfAuditInfo(w)) // i18n-ignore 审计动作名
	return w, nil
}

// canCancelSchedule 详情页是否显示“取消定时”。
func (s *Service) canCancelSchedule(p audit.Actor, pm perms, w *model.SqlWorkflow) bool {
	return w.Status == model.WfTimingTask && (w.Engineer == p.Username || pm.canExecute(p, w))
}
