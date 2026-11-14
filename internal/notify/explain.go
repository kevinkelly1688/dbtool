// @author: kevinkelly
// @license: Apache Licence
// @file: explain.go
// @time: 2026/10/28

package notify

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// 本文件：@ 目标的解析（含“为什么没 @”的原因）、测试发送的“预览”（不真发）、模拟事件样例、发送记录接口。

// ---- 发送记录 ----

// SendLog 是一次发送的结果（表 dbt_notify_log，保留 30 天）。
type SendLog struct {
	ID          uint64
	ChannelID   uint
	ChannelName string
	Event       string // 事件名（Specific 优先）
	Kind        string
	OK          bool
	CostMs      int
	Error       string // 错误摘要，不含 webhook 地址
	Mentions    int
	Test        bool
	At          time.Time
}

// LogSink 记录发送结果。实现必须尽力而为：记录失败只记日志，不能影响通知本身。
type LogSink interface {
	RecordSend(rec SendLog)
}

// ---- 工作时间 ----

// InWorkHours 判断 t 是否在工作时间：周一至周五 09:00–19:00（t 所在时区，渠道按服务器本地时间使用）。
func InWorkHours(t time.Time) bool {
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	h := t.Hour()
	return h >= 9 && h < 19
}

func (d *Dispatcher) workTime(ev Event) bool {
	if ev.WorkTime != nil {
		return *ev.WorkTime
	}
	return InWorkHours(d.Now().In(time.Local))
}

// ---- 规则标签与事件标签 ----

// EventLabel 返回事件的显示名（去掉括号里的补充说明）；未知事件原样返回。
func EventLabel(name string) string {
	for _, e := range Events {
		if e.Name == name {
			if i := strings.Index(e.Label, "（"); i > 0 {
				return e.Label[:i]
			}
			return e.Label
		}
	}
	return name
}

// RuleLabel 返回 @ 规则目标的可读描述。
func RuleLabel(r Rule) string {
	switch r.Target {
	case TargetApplicant:
		return "提交人"
	case TargetReviewers:
		return "当前审批级的审批人"
	case TargetDBADuty:
		return "DBA 值班"
	case TargetAuthGroup:
		return fmt.Sprintf("权限组成员（#%s）", r.Value)
	case TargetUser:
		return fmt.Sprintf("指定用户（%s）", r.Value)
	case TargetRaw:
		return fmt.Sprintf("渠道原生 ID（%s）", r.Value)
	case TargetAll:
		return "所有人"
	}
	return r.Target
}

// Skip 是一条没有生效的 @ 规则及原因。
type Skip struct {
	Rule   Rule
	Label  string
	Reason string
}

// ---- 解析 @ 目标 ----

func (d *Dispatcher) resolveExplain(ch Channel, ev Event) ([]Mention, []Skip) {
	var out []Mention
	var skips []Skip
	seen := map[string]bool{}
	addPeople := func(ps []Person) int {
		n := 0
		for i := range ps {
			p := ps[i]
			if key := "u:" + p.Username; !seen[key] && len(out) < maxMentions {
				seen[key] = true
				out = append(out, Mention{Person: &p})
				n++
			}
		}
		return n
	}
	addRaw := func(raw string) {
		raw = strings.TrimSpace(raw)
		if key := "r:" + raw; raw != "" && !seen[key] && len(out) < maxMentions {
			seen[key] = true
			out = append(out, Mention{Raw: raw})
		}
	}
	work := d.workTime(ev)
	for _, r := range ch.Rules {
		skip := func(why string) { skips = append(skips, Skip{Rule: r, Label: RuleLabel(r), Reason: why}) }
		if len(r.Events) > 0 && !eventHit(r.Events, ev, ch.EventsV2) {
			names := make([]string, len(r.Events))
			for i, e := range r.Events {
				names[i] = EventLabel(e)
			}
			skip(fmt.Sprintf("该规则只在「%s」时 @，当前事件不 @", strings.Join(names, "、")))
			continue
		}
		if r.WorkHours && !work {
			skip("设置了“仅工作时间 @”（周一至周五 09:00–19:00），当前是非工作时间，只发消息不 @")
			continue
		}
		var ps []Person
		var err error
		switch r.Target {
		case TargetApplicant:
			if ev.Applicant == "" {
				skip("该事件没有提交人")
				continue
			}
			ps, err = d.Dir.UsersByName([]string{ev.Applicant})
		case TargetReviewers:
			if ev.ReviewerAuthGroupID == 0 {
				skip(fmt.Sprintf("「%s」事件没有当前审批级", EventLabel(ev.Name)))
				continue
			}
			ps, err = d.Dir.AuthGroupMembers(ev.ReviewerAuthGroupID, ev.GroupID)
		case TargetAuthGroup:
			id, perr := strconv.ParseUint(strings.TrimSpace(r.Value), 10, 32)
			if perr != nil {
				skip("权限组 ID 不合法")
				continue
			}
			ps, err = d.Dir.AuthGroupMembers(uint(id), ev.GroupID)
		case TargetUser:
			ps, err = d.Dir.UsersByName([]string{strings.TrimSpace(r.Value)})
		case TargetDBADuty:
			if len(ch.DutyUsers) == 0 {
				skip("该渠道没有配置 DBA 值班人员")
				continue
			}
			ps, err = d.Dir.UsersByName(ch.DutyUsers)
		case TargetRaw:
			addRaw(r.Value)
			continue
		case TargetAll:
			addRaw("all")
			continue
		default:
			skip("未知的 @ 目标类型")
			continue
		}
		if err != nil {
			slog.Warn("解析 @ 对象失败", "target", r.Target, "err", err)
			skip(fmt.Sprintf("解析失败：%s", err.Error()))
			continue
		}
		if addPeople(ps) == 0 {
			if len(ps) == 0 {
				skip("没有匹配到可 @ 的启用用户（该资源组 / 权限组下没有成员，或用户已停用）")
			} else {
				skip("匹配到的用户已经被前面的规则 @ 过")
			}
		}
	}
	if len(ev.ExtraUsers) > 0 {
		if ps, err := d.Dir.UsersByName(ev.ExtraUsers); err != nil {
			slog.Warn("解析额外 @ 对象失败", "err", err)
		} else {
			addPeople(ps)
		}
	}
	return out, skips
}

// ---- 预览（不真发） ----

// MentionView 是预览里的一个 @ 对象。
type MentionView struct {
	Label    string `json:"label"`
	Kind     string `json:"kind"`     // user / raw / all
	Resolved bool   `json:"resolved"` // 真的会提醒到人：用户在该渠道有标识 / 原生标识合法
	Note     string `json:"note,omitempty"`
}

// SkipView 是预览里一条没生效的规则。
type SkipView struct {
	Rule   string `json:"rule"`
	Reason string `json:"reason"`
}

// Preview 是一次预览的结果。
type Preview struct {
	Event    string          `json:"event"`
	Title    string          `json:"title"`
	Content  string          `json:"content"`
	WorkTime bool            `json:"work_time"`
	Mentions []MentionView   `json:"mentions"`
	Skipped  []SkipView      `json:"skipped"`
	Payload  json.RawMessage `json:"payload"`  // 将要 POST 给机器人的请求体（签名字段未包含）
	Signed   bool            `json:"signed"`   // 渠道配置了签名密钥：真实发送时会带上签名
	Platform string          `json:"platform"` // 渠道类型

	// 套用了消息模板时：命中的模板范围（global / c:<渠道 ID>）与渲染后的消息；没套用为空（发系统默认文案）。
	Template string   `json:"template,omitempty"`
	Message  *Message `json:"message,omitempty"`
}

func personHasID(ch Channel, p *Person) bool {
	switch ch.Type {
	case TypeEmail:
		_, err := ParseAddr(p.Email)
		return err == nil
	case TypeDingTalk:
		return p.DingID != "" && safeIDRe.MatchString(p.DingID)
	case TypeFeishu, TypeLark:
		return p.FeishuID != "" && safeIDRe.MatchString(p.FeishuID)
	case TypeSlack:
		return p.SlackID != "" && slackIDRe.MatchString(p.SlackID)
	}
	return false
}

// Preview 按渠道配置与模拟事件生成将要发送的消息体与 @ 解析结果，不发送任何请求。
// 渠道的地址与密钥不参与（用占位地址），所以预览不会泄露也不会触发签名。
func (d *Dispatcher) Preview(ch Channel, ev Event) (*Preview, error) {
	if ev.rich == nil {
		ev.rich = d.render(ch, ev)
	}
	ments, skips := d.resolveExplain(ch, ev)
	if ch.Type == TypeEmail {
		return d.previewEmail(ch, ev, ments, skips)
	}
	pc := ch
	pc.WebhookURL, pc.Secret = "https://example.invalid/robot/send", ""
	b, err := build(pc, ev, ments, d.Now())
	if err != nil {
		return nil, err
	}
	p := &Preview{Event: ev.Name, Title: ev.Title, Content: ev.Content, WorkTime: d.workTime(ev), Mentions: []MentionView{}, Skipped: []SkipView{},
		Payload: json.RawMessage(b.Body), Signed: ch.Secret != "", Platform: ch.Type}
	if ev.rich != nil {
		p.Template, p.Message = ev.rich.Scope, ev.rich
	}
	if ev.Specific != "" {
		p.Event = ev.Specific
	}
	for _, m := range ments {
		switch {
		case m.Raw == "all":
			p.Mentions = append(p.Mentions, MentionView{Label: "@所有人", Kind: "all", Resolved: true})
		case m.Raw != "":
			p.Mentions = append(p.Mentions, MentionView{Label: "@" + m.Raw, Kind: "raw", Resolved: true})
		default:
			v := MentionView{Label: "@" + m.Person.name(), Kind: "user", Resolved: personHasID(ch, m.Person)}
			if !v.Resolved {
				v.Note = "该用户没有此渠道的标识，退化为文本“@显示名”，不会真正提醒（在用户资料里补充钉钉 ID / 飞书 open_id / Slack ID）"
				switch ch.Type {
				case TypeWeCom:
					v.Note = WeComUserNote // 企业微信没有对应的用户资料项，补不了
				case TypeTelegram:
					v.Note = TelegramUserNote // Telegram 同样没有
				}
			}
			p.Mentions = append(p.Mentions, v)
		}
	}
	for _, s := range skips {
		p.Skipped = append(p.Skipped, SkipView{Rule: s.Label, Reason: s.Reason})
	}
	return p, nil
}

// ---- 模拟事件样例 ----

// SampleEvent 返回用于测试发送 / 预览的模拟事件（样例工单，不对应真实数据）。applicant 是当前操作者，
// 会作为“提交人”；reviewerGroup 非 0 时作为当前审批级的权限组（让“审批人”规则有对象可解析）。
func SampleEvent(name, applicant string, groupID, reviewerGroup uint) (Event, error) {
	wf := func(title, extra string) (string, string) {
		c := "发起时间：2026-10-07 10:08:21\n发起人：（样例）" + applicant + "\n目标实例：mysql-order-prod-m\n数据库：order_db\n工单名称：订单表增加字段 refund_flag\n工单地址：https://dba.example.com/sqlworkflow/1042" // i18n-ignore 通知正文 / 模板内容
		if extra != "" {
			c += "\n" + extra
		}
		return "[SQL上线申请]" + title, c // i18n-ignore 通知正文 / 模板内容
	}
	ev := Event{Kind: KindSQLWorkflow, Name: name, GroupID: groupID, Applicant: applicant, Test: true}
	switch name {
	case EventSubmitted:
		ev.Name, ev.Specific = EventPending, EventSubmitted
		ev.Title, ev.Content = wf("新的工单申请#1042", "当前审批：（样例）组长") // i18n-ignore 通知正文 / 模板内容
		ev.ReviewerAuthGroupID = reviewerGroup
	case EventPending:
		ev.Title, ev.Content = wf("工单流转到下一级#1042", "当前审批：（样例）DBA") // i18n-ignore 通知正文 / 模板内容
		ev.ReviewerAuthGroupID = reviewerGroup
	case EventPassed:
		ev.Title, ev.Content = wf("审批通过#1042", "") // i18n-ignore 通知正文 / 模板内容
	case EventRejected:
		ev.Title, ev.Content = wf("工单被驳回#1042", "驳回原因：缺少回滚方案，请补充后重新提交") // i18n-ignore 通知正文 / 模板内容
	case EventAborted:
		ev.Title, ev.Content = wf("提交人终止工单#1042", "终止原因：需求变更") // i18n-ignore 通知正文 / 模板内容
	case EventScheduled:
		ev.Title, ev.Content = wf("已设置定时执行#1042", "计划执行时间：2026-10-08 02:00") // i18n-ignore 通知正文 / 模板内容
	case EventExecuting:
		ev.Title, ev.Content = wf("开始执行#1042", "执行方式：goInception") // i18n-ignore 通知正文 / 模板内容
	case EventFinished:
		ev.Title, ev.Content = wf("工单执行完成#1042", "执行结果：成功，耗时 3.2 s") // i18n-ignore 通知正文 / 模板内容
	case EventException:
		ev.Title, ev.Content = wf("工单执行有异常#1042", "错误信息：Lock wait timeout exceeded; try restarting transaction") // i18n-ignore 通知正文 / 模板内容
	case EventPerfOverdue:
		ev.Kind, ev.Name, ev.Specific = "perf_issue", EventException, EventPerfOverdue
		ev.Title = "【SQL性能工单 #2031 P1】已超时 18 小时"                                                                             // i18n-ignore 通知正文 / 模板内容
		ev.Content = "实例：mysql-pay-prod-m  库：pay_db\n解决截止：2026-10-06 18:00（已超时 18 小时）\n详情：https://dba.example.com/perf/2031" // i18n-ignore 通知正文 / 模板内容
	case EventInspectAlert:
		ev.Kind, ev.Name, ev.Specific = "inspect", EventException, EventInspectAlert
		ev.Title = "[dbtool] 定时巡检「生产库日巡检」发现问题"                                                      // i18n-ignore 通知正文 / 模板内容
		ev.Content = "实例 12 个：成功 11 / 失败 1\n告警合计 3 · 关注合计 7\n健康评分低于 70 的实例：mysql-user-prod-m（62 分）" // i18n-ignore 通知正文 / 模板内容
	default:
		return Event{}, fmt.Errorf("未知的事件 %s", strconv.Quote(name)) // i18n-ignore 内部错误，调用方已换成面向用户的提示
	}
	return ev, nil
}

// ValidEvent 判断事件名是否是已知事件。
func ValidEvent(name string) bool {
	for _, e := range Events {
		if e.Name == name {
			return true
		}
	}
	return false
}

// IsFirstAuditNode 判断审批流（权限组 ID 逗号分隔，同 dbt_workflow_audit.audit_auth_groups）的当前节点是否第一级：
// 新工单提交时进入第一级，此时除了“待处理”还要发“新工单提交”（Event.Specific = EventSubmitted）。
func IsFirstAuditNode(auditAuthGroups, currentAudit string) bool {
	first := strings.TrimSpace(strings.SplitN(auditAuthGroups, ",", 2)[0])
	return first != "" && first == strings.TrimSpace(currentAudit)
}
