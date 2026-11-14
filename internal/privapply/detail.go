// @author: kevinkelly
// @license: Apache Licence
// @file: detail.go
// @time: 2026/10/28

package privapply

import (
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// Granted 是一张申请授予的一条权限（用于详情页的“已授予权限”链接）。
type Granted struct {
	PrivilegeID uint
	DBName      string
	Table       string
	ValidDate   time.Time
	LimitNum    int
	Revoked     bool   // 已被回收
	State       string // PrivValid / PrivSoon / PrivExpired
}

// 时间线节点状态。
const (
	StepDone     = "done"
	StepCurrent  = "current"
	StepPending  = "pending"
	StepRejected = "rejected"
	StepAborted  = "aborted"
	StepSkipped  = "skipped" // 流程提前终止，没走到
)

// TLStep 是时间线上的一步。
type TLStep struct {
	Title     string
	Node      string // 审批节点的审批组名（Title 是它加上“审批”）；非审批节点为空。界面按 Node 翻译整句，不用拼好的 Title
	State     string
	Actor     string
	Time      time.Time
	Note      string
	Reviewers []string // 审批节点：该级现在能审批的人（页面不列出，鼠标悬停在审批组上才显示）
	// NoReviewer 为真表示“正在审批 / 待审批”的这一级没有任何人能审批（只有超级用户能处理）；已结束的级别不标。
	NoReviewer bool
}

// Tip 是时间线上鼠标悬停在审批组上显示的文字：这一级现在有哪些人能审批；没人时说明原因。非审批节点为空。
func (s TLStep) Tip() string {
	if s.NoReviewer {
		return "本级没有符合条件的审批人，只有超级用户能审批，请联系管理员检查审批流和权限。"
	}
	if len(s.Reviewers) == 0 {
		return ""
	}
	return fmt.Sprintf("可审批人员（%d）：%s", len(s.Reviewers), s.ReviewerList("、"))
}

// ReviewerList 用 sep 连接该级现在能审批的人（模板里按界面语言传分隔符，英文用逗号）。
func (s TLStep) ReviewerList(sep string) string { return strings.Join(s.Reviewers, sep) }

// parseRemark 从审批日志的说明里取出审批人写的备注（日志是“审批备注：xx, 下级审批：yy”这样的拼接串）。
func parseRemark(info string) string {
	for _, p := range []string{"审批备注：", "审批备注: ", "取消原因：", "取消原因: "} { // i18n-ignore 与审批引擎写入的日志原文匹配
		if strings.HasPrefix(info, p) {
			info = info[len(p):]
			break
		}
	}
	if i := strings.LastIndex(info, ", 下级审批："); i >= 0 { // i18n-ignore 与审批引擎写入的日志原文匹配
		info = info[:i]
	} else if strings.HasSuffix(info, ", 无下级审批") { // i18n-ignore 同上
		info = strings.TrimSuffix(info, ", 无下级审批") // i18n-ignore 同上
	}
	return strings.TrimSpace(info)
}

// buildTimeline 由审批流与审批日志生成各级审批时间线。
// 第 i 条“审核通过”日志对应第 i 个节点；驳回日志对应“已通过节点数”之后的那个节点。
func buildTimeline(now time.Time, a *model.QueryPrivilegesApply, au *model.WorkflowAudit, nodes []audit.Node, logs []model.WorkflowLog, reviewers func(audit.Node) []string) []TLStep {
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
			reject = &logs[i]
		case audit.ActionAbort:
			abort = &logs[i]
		}
	}
	steps := make([]TLStep, 0, len(nodes)+3)
	st := TLStep{Title: "提交申请", State: StepDone, Actor: a.UserDisplay, Time: a.CreateTime}
	if submit != nil {
		st.Actor, st.Time = displayOr(submit.OperatorDisplay, submit.Operator), submit.OperationTime
	}
	steps = append(steps, st)

	terminated := au.CurrentStatus == audit.StatusRejected || au.CurrentStatus == audit.StatusAborted
	for i, n := range nodes {
		step := TLStep{Title: n.Name + "审批", Node: n.Name, State: StepPending} // i18n-ignore Title 是 Node + 固定后缀，界面用 Node 配 {name}审批 翻译
		switch {
		case i < len(passes):
			l := passes[i]
			step.State, step.Actor, step.Time, step.Note = StepDone, displayOr(l.OperatorDisplay, l.Operator), l.OperationTime, parseRemark(l.OperationInfo)
		case reject != nil && i == len(passes):
			step.State, step.Actor, step.Time, step.Note = StepRejected, displayOr(reject.OperatorDisplay, reject.Operator), reject.OperationTime, parseRemark(reject.OperationInfo)
		case au.CurrentStatus == audit.StatusPassed:
			step.State = StepDone // 没有逐级日志（例如系统直接通过）
		case terminated:
			step.State = StepSkipped
		case n.Current:
			step.State = StepCurrent
		}
		if reviewers != nil {
			step.Reviewers = reviewers(n) // 每一级都带上：页面只显示审批组，鼠标悬停才列出人员
			step.NoReviewer = step.Reviewers != nil && len(step.Reviewers) == 0 && (step.State == StepCurrent || step.State == StepPending)
		}
		steps = append(steps, step)
	}
	switch au.CurrentStatus {
	case audit.StatusPassed:
		g := TLStep{Title: "授予查询权限", State: StepDone}
		if len(passes) > 0 {
			g.Time = passes[len(passes)-1].OperationTime
		}
		steps = append(steps, g)
		if EffStatus(a.Status, a.ValidDate, now) == StatusExpired {
			steps = append(steps, TLStep{Title: "权限已到期失效", State: StepSkipped, Time: a.ValidDate, Note: fmt.Sprintf("有效期至 %s", a.ValidDate.Format("2006-01-02"))})
		}
	case audit.StatusRejected:
		steps = append(steps, TLStep{Title: "流程终止（已驳回）", State: StepRejected, Note: "可修改后重新申请"})
	case audit.StatusAborted:
		t := TLStep{Title: "申请人撤回", State: StepAborted}
		if abort != nil {
			t.Actor, t.Time, t.Note = displayOr(abort.OperatorDisplay, abort.Operator), abort.OperationTime, parseRemark(abort.OperationInfo)
		}
		steps = append(steps, t)
	default:
		steps = append(steps, TLStep{Title: "授予查询权限", State: StepPending, Note: "末级通过后自动写入，在线查询立即生效"})
	}
	return steps
}

func displayOr(display, name string) string {
	if display != "" {
		return display
	}
	return name
}

// reapplyQuery 生成“重新申请”预填参数。
func reapplyQuery(a *model.QueryPrivilegesApply, reason string) string {
	v := url.Values{}
	v.Set("instance", strconv.Itoa(int(a.InstanceID)))
	v.Set("type", strconv.Itoa(a.PrivType))
	v.Set("db", a.DBList)
	if a.PrivType == 2 {
		v.Set("table", a.TableList)
	}
	if a.GroupName != "" {
		v.Set("group", a.GroupName)
	}
	v.Set("limit", strconv.Itoa(a.LimitNum))
	if reason != "" {
		v.Set("reason", reason)
	}
	return "new=1&" + v.Encode()
}

// fillDetailExt 补全详情页的扩展信息。任何一项取不到都只跳过（旁路表没创建、部门表不存在等），不影响详情本身。
func (s *Service) fillDetailExt(d *Detail, au *model.WorkflowAudit) {
	now := s.now()
	e := ext(s.repo)
	if e != nil {
		d.Dept = e.UserDept(d.Apply.UserName)
		if e.ApplyExtReady() {
			if m, err := e.ApplyExts([]uint{d.Apply.ApplyID}); err == nil {
				d.Reason = m[d.Apply.ApplyID].Reason
			} else {
				slog.Warn("读取申请理由失败", "apply_id", d.Apply.ApplyID, "err", err)
			}
		}
		if e.PrivExtReady() && d.Apply.Status == audit.StatusPassed {
			list, err := e.PrivExtsByApply(d.Apply.ApplyID)
			if err != nil {
				slog.Warn("读取授予的权限失败", "apply_id", d.Apply.ApplyID, "err", err)
			}
			for _, x := range list {
				p, err := s.repo.GetPrivilege(x.PrivilegeID)
				if err != nil || p == nil {
					continue
				}
				d.Granted = append(d.Granted, Granted{PrivilegeID: p.PrivilegeID, DBName: p.DBName, Table: p.TbName,
					ValidDate: p.ValidDate, LimitNum: p.LimitNum, Revoked: p.IsDeleted != 0, State: PrivState(p.ValidDate, now)})
			}
		}
	}
	reviewers := func(n audit.Node) []string {
		users, err := s.repo.AuthGroupUsers([]string{n.Name}, d.Apply.GroupID)
		if err != nil {
			return nil
		}
		out := make([]string, 0, len(users))
		for _, u := range users {
			out = append(out, displayOr(u.Display, u.Username))
		}
		return out
	}
	d.Timeline = buildTimeline(now, &d.Apply, au, d.Nodes, d.Logs, reviewers)
	switch d.EffStatus {
	case audit.StatusRejected, audit.StatusAborted, StatusExpired:
		d.Reapply = reapplyQuery(&d.Apply, d.Reason)
	}
}
