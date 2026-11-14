// @author: kevinkelly
// @license: Apache Licence
// @file: notify.go
// @time: 2026/10/28

package perf

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"dbtool/internal/notify"
	"dbtool/internal/settings"
)

// 状态变化通知：复用 internal/notify 的渠道（dbt_notify_channel 与资源组上的旧 webhook）、签名与 @ 规则。
//
// 与工单事件的对应：notify 包的事件名是固定的一组（渠道订阅与 @ 规则按它们配置），性能工单的事件映射到其中：
//
//	待处理  ← 新建、指派、重开、级别升级、标记修复待验证、验证不通过、申请延期/豁免、评论、即将超时
//	审批通过 ← 同意延期      被驳回 ← 驳回延期/豁免      执行完成 ← 工单关闭      执行异常 ← SLA 催办/升级
//
// Applicant（“@提交人”规则）取工单负责人；Kind 为 KindPerfIssue。需要额外 @ 的人（团队 TL、总监、跟进 DBA）放在 ExtraUsers，
// 无论渠道的 @ 规则怎么配都会 @。个人私信（钉钉工作通知、邮件等）新系统没有对应通道，不发。

// KindPerfIssue 是通知里的工单类别。
const KindPerfIssue = "perf_issue"

// 通知事件（本包内部）。
const (
	evCreated       = "created"
	evAssigned      = "assigned"
	evReopened      = "reopened"
	evSeverityUp    = "severity_up"
	evFixed         = "fixed"
	evVerifyFail    = "verify_fail"
	evClosed        = "closed"
	evExtendReq     = "extend_request"
	evWaiveReq      = "waive_request"
	evExtendApprove = "extend_approve"
	evExtendReject  = "extend_reject"
	evWaiveReject   = "waive_reject"
	evCommented     = "commented"
)

func (s *Service) issueURL(id uint) string {
	return fmt.Sprintf("%s/perf/%d", strings.TrimRight(s.BaseURL, "/"), id)
}

// send 在后台发送；静默模式（perf.silent_mode，上线初期校准阈值用）与关闭群通知（perf.group_notify=false）时不发。
func (s *Service) send(build func() *notify.Event) {
	if s.cfgBool(settings.KeyPerfSilentMode, false) || !s.cfgBool(settings.KeyPerfGroupNotify, true) {
		return
	}
	n := s.notifier
	s.async(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("构造性能工单通知失败", "panic", r)
			}
		}()
		ev := build()
		if ev == nil {
			return
		}
		ctx, cancel := ctxBG(15 * time.Second)
		defer cancel()
		n.Notify(ctx, *ev)
	})
}

// brief 是通知正文：问题是什么、影响多大、谁负责、什么时候到期。
func (s *Service) brief(r *IssueRow, withDeadline bool, now time.Time) string {
	assignee := r.AssigneeName
	if assignee == "" {
		assignee = "待分派"
	}
	team := r.TeamName
	if team == "" {
		team = "-"
	}
	lines := []string{
		fmt.Sprintf("实例：%s  库：%s  表：%s", r.InstanceName, orDash(r.DBName), orDash(r.Tables)),
		fmt.Sprintf("类型：%s（%s）", labelOf(Categories, r.Category), sideLabel[r.OwnerSide]),
		fmt.Sprintf("影响：日均浪费DB时间 %.0fs，占该实例慢SQL %.1f%%，日均执行 %s 次，平均 %sms", r.ImpactScore, r.LoadShare, commaInt(int64(r.PerDayCount)), commaInt(int64(r.AvgTimeMs))),
		fmt.Sprintf("负责人：%s  团队：%s", assignee, team),
	}
	if withDeadline && r.ResolveDeadline != nil {
		left := r.ResolveDeadline.Sub(now)
		state := "剩余 " + humanDelta(left)
		if left <= 0 {
			state = "已超时 " + humanDelta(-left)
		}
		lines = append(lines, fmt.Sprintf("解决截止：%s（%s）", r.ResolveDeadline.Format("2006-01-02 15:04"), state))
	}
	lines = append(lines, "SQL："+clipRunes(r.Fingerprint, 200), "详情："+s.issueURL(r.ID))
	return strings.Join(lines, "\n")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// usernames 取若干用户 ID 对应的用户名（去重，跳过停用的）。
func (s *Service) usernames(ids ...uint) []string {
	ids = uniqueUints(ids)
	if len(ids) == 0 {
		return nil
	}
	us, err := s.repo.GetUsers(ids)
	if err != nil {
		return nil
	}
	var out []string
	for _, id := range ids {
		if u, ok := us[id]; ok && u.Active {
			out = append(out, u.Username)
		}
	}
	return out
}

func (s *Service) teamMembers(teamID *uint, leaders, directors bool) []uint {
	if teamID == nil {
		return nil
	}
	t, err := s.repo.GetTeamByGroup(*teamID)
	if err != nil || t == nil {
		return nil
	}
	var ids []uint
	if leaders {
		ids = append(ids, t.LeaderIDs...)
	}
	if directors {
		ids = append(ids, t.DirectorIDs...)
	}
	return ids
}

func derefOr(p *uint) uint { return derefUint(p) }

// notifyIssue 发送工单状态变化的通知。exclude 是不需要被 @ 的人（如评论的发表者）。
func (s *Service) notifyIssue(id uint, ev event, exclude uint) {
	s.send(func() *notify.Event {
		r, err := s.repo.GetIssueRow(id)
		if err != nil || r == nil {
			return nil
		}
		now := s.now()
		head := fmt.Sprintf("【SQL性能工单 #%d %s】", r.ID, r.Severity)
		out := &notify.Event{Kind: KindPerfIssue, Name: notify.EventPending, GroupID: derefOr(r.TeamID), Content: s.brief(r, true, now)}
		var at []uint
		a, dba := derefOr(r.AssigneeID), derefOr(r.DBAID)
		switch ev.name {
		case evCreated:
			out.Title, at = head+"新问题待受理", []uint{a}
		case evAssigned:
			out.Title, at = head+"已指派给 "+orDefault(r.AssigneeName, "待分派"), []uint{a}
		case evReopened:
			out.Title, at = head+"问题复现，工单重开", []uint{a, dba}
			out.Content = "原因：" + ev.reason + "\n" + out.Content
		case evSeverityUp:
			out.Title, at = fmt.Sprintf("%s影响扩大 %s→%s，时限已收紧", head, ev.oldSev, r.Severity), []uint{a}
		case evFixed:
			out.Title, at = fmt.Sprintf("%s%s 已标记修复，待验证", head, ev.actor), []uint{dba}
			out.Content = "方案：" + r.FixDesc + "\n" + out.Content
		case evVerifyFail:
			out.Title, at = head+"验证未通过，请继续处理", []uint{a}
			out.Content = "原因：" + ev.reason + "\n" + out.Content
		case evClosed:
			out.Name = notify.EventFinished
			out.Title, at = head+"已关闭："+closeLabel[r.CloseReason], []uint{a}
			out.Content = ev.reason + "\n" + s.brief(r, false, now)
		case evExtendReq, evWaiveReq:
			name := "延期"
			if ev.name == evWaiveReq {
				name = "豁免"
			}
			out.Title, at = fmt.Sprintf("%s%s 申请%s，请DBA审批", head, ev.actor, name), []uint{dba}
			extra := ""
			if ev.name == evExtendReq {
				extra = "申请延期至：" + fmtDate(r.PendingExtendDate) + "\n"
			}
			out.Content = extra + "理由：" + r.PendingReason + "\n" + out.Content
		case evExtendApprove:
			out.Name = notify.EventPassed
			out.Title, at = head+"延期申请已通过", []uint{a}
			out.Content = ev.reason + "\n" + out.Content
		case evExtendReject, evWaiveReject:
			out.Name = notify.EventRejected
			out.Title, at = head+"申请被驳回", []uint{a}
			out.Content = ev.reason + "\n" + out.Content
		case evCommented:
			out.Title, at = fmt.Sprintf("%s%s 发表了评论", head, ev.actor), []uint{a, dba}
			out.Content = ev.remark + "\n" + s.issueURL(id)
		default:
			return nil
		}
		out.Applicant = firstOr(s.usernames(a))
		out.ExtraUsers = s.usernames(withoutUint(at, exclude)...)
		return out
	})
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func firstOr(l []string) string {
	if len(l) > 0 {
		return l[0]
	}
	return ""
}

func withoutUint(l []uint, x uint) []uint {
	if x == 0 {
		return l
	}
	var out []uint
	for _, v := range l {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

// notifyEscalation SLA 提醒/催办/升级。level：0 即将超时；1 催办负责人；2 升级团队 TL 与跟进 DBA；3 再升级总监。全局渠道默认每一级都会收到，可在 SLA 页升级矩阵的“全局渠道”列按级关闭（Event.SkipGlobal）。
func (s *Service) notifyEscalation(id uint, level int, title string, now time.Time) {
	s.send(func() *notify.Event {
		r, err := s.repo.GetIssueRow(id)
		if err != nil || r == nil {
			return nil
		}
		// 通知对象按 SLA 页配置的升级矩阵（没有配置时是：L1 负责人；L2 + 团队 TL + 跟进 DBA；L3 + 总监）；即将超时提醒固定只通知负责人
		tg := s.escalationMatrix().targetsFor(level)
		var at []uint
		if tg.Owner {
			at = append(at, derefOr(r.AssigneeID))
		}
		if tg.TL {
			at = append(at, s.teamMembers(r.TeamID, true, false)...)
		}
		if tg.DBA {
			at = append(at, derefOr(r.DBAID))
		}
		if tg.Director {
			at = append(at, s.teamMembers(r.TeamID, false, true)...)
		}
		name, specific := notify.EventException, notify.EventPerfOverdue // 已超时的升级：独立事件“性能工单超时”，旧渠道仍按“执行异常”收到
		if level == 0 {
			name, specific = notify.EventPending, ""
		}
		return &notify.Event{Kind: KindPerfIssue, Name: name, Specific: specific, SkipGlobal: tg.NoGlobal, GroupID: derefOr(r.TeamID), Applicant: firstOr(s.usernames(derefOr(r.AssigneeID))),
			Title: fmt.Sprintf("【SQL性能工单 #%d %s】%s", r.ID, r.Severity, title), Content: s.brief(r, true, now), ExtraUsers: s.usernames(at...)}
	})
}

// notifyBatchCreated 一次新建太多（如首次接入）时逐条推送会刷屏，改为按团队汇总：团队群一条消息 @ 所有负责人。
func (s *Service) notifyBatchCreated(ids []uint) {
	if s.cfgBool(settings.KeyPerfSilentMode, false) || !s.cfgBool(settings.KeyPerfGroupNotify, true) {
		return
	}
	n := s.notifier
	s.async(func() {
		byTeam := map[uint][]*IssueRow{}
		for _, id := range ids {
			if r, err := s.repo.GetIssueRow(id); err == nil && r != nil {
				byTeam[derefOr(r.TeamID)] = append(byTeam[derefOr(r.TeamID)], r)
			}
		}
		teams := make([]uint, 0, len(byTeam))
		for t := range byTeam {
			teams = append(teams, t)
		}
		sort.Slice(teams, func(a, b int) bool { return teams[a] < teams[b] })
		for _, t := range teams {
			items := byTeam[t]
			sort.SliceStable(items, func(a, b int) bool {
				if items[a].Severity != items[b].Severity {
					return items[a].Severity < items[b].Severity
				}
				return items[a].ImpactScore > items[b].ImpactScore
			})
			var lines []string
			var assignees []uint
			for idx, r := range items {
				if idx < 30 {
					lines = append(lines, fmt.Sprintf("#%d %s %s %s 日均浪费%.0fs %s", r.ID, r.Severity, labelOf(Categories, r.Category), orDefault(r.AssigneeName, "待分派"), r.ImpactScore, s.issueURL(r.ID)))
				}
				assignees = append(assignees, derefOr(r.AssigneeID))
			}
			if len(items) > 30 {
				lines = append(lines, fmt.Sprintf("... 共 %d 个，详见 %s/perf", len(items), strings.TrimRight(s.BaseURL, "/")))
			}
			ev := notify.Event{Kind: KindPerfIssue, Name: notify.EventPending, GroupID: t, Title: fmt.Sprintf("【SQL性能工单】新发现 %d 个慢SQL待受理", len(items)),
				Content: strings.Join(lines, "\n"), ExtraUsers: s.usernames(assignees...)}
			ctx, cancel := ctxBG(15 * time.Second)
			n.Notify(ctx, ev)
			cancel()
		}
	})
}
