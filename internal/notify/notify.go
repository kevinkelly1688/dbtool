// @author: kevinkelly
// @license: Apache Licence
// @file: notify.go
// @time: 2026/10/28

// Package notify 发送工单通知到机器人 webhook（钉钉、企业微信、Lark、飞书、Slack、Telegram）和邮件（SMTP，见 email.go）。
//
// 每个资源组可以配置多个渠道（dbt_notify_channel），每个渠道有自己的签名密钥、订阅的事件和 @ 规则：
// 可以 @ 工单提交人、当前节点的审批人、某个权限组的成员、指定的平台用户，或渠道原生的标识
// （Slack 用户/用户组 ID、钉钉手机号、企业微信成员 userid / 手机号、Telegram 用户名 / 用户 ID、飞书 open_id，以及“所有人”）。
//
// 企业微信渠道（type = wecom，见 wecom.go）和 Telegram 渠道（type = telegram，见 telegram.go）没有加签；
// 用户资料里没有这两个平台的 ID，按平台用户 @ 时只显示名字。Telegram 没有“@所有人”。
//
// 邮件渠道（type = email）把 @ 规则解析成用户资料里的邮箱，发给这些人（再加上渠道上固定的收件人）。
//
// 通知是尽力而为的：发送失败只记日志，不影响业务操作。
package notify

import (
	"context"
	"strconv"
	"strings"
)

// 渠道类型。
const (
	TypeDingTalk = "dingtalk"
	TypeWeCom    = "wecom"  // 企业微信群机器人（域名 qyapi.weixin.qq.com）
	TypeLark     = "lark"   // Lark（飞书国际版，域名 larksuite.com）
	TypeFeishu   = "feishu" // 飞书（域名 feishu.cn）
	TypeSlack    = "slack"
	TypeTelegram = "telegram" // Telegram 机器人（域名 api.telegram.org）
)

// Types 是支持的渠道类型。
var Types = []string{TypeDingTalk, TypeWeCom, TypeFeishu, TypeLark, TypeSlack, TypeTelegram, TypeEmail}

// TypeLabel 返回渠道类型的显示名。
func TypeLabel(t string) string {
	switch t {
	case TypeDingTalk:
		return "钉钉"
	case TypeWeCom:
		return "企业微信"
	case TypeFeishu:
		return "飞书"
	case TypeLark:
		return "Lark"
	case TypeSlack:
		return "Slack"
	case TypeTelegram:
		return "Telegram"
	case TypeEmail:
		return "邮件"
	}
	return t
}

// 事件名。不同类型的工单（查询权限申请、SQL 上线）共用同一套事件。
const (
	EventPending   = "pending"   // 有工单待处理（新提交，或流转到下一级审批）
	EventPassed    = "passed"    // 审批通过
	EventRejected  = "rejected"  // 被驳回
	EventAborted   = "aborted"   // 提交人终止
	EventScheduled = "scheduled" // 已设置定时执行
	EventExecuting = "executing" // 开始执行
	EventFinished  = "finished"  // 执行完成
	EventException = "exception" // 执行异常

	// 以下三个是更细的事件，见 Event.Specific 与 Channel.wants。
	EventSubmitted    = "submitted"     // 新工单提交（同时也属于“待处理”）
	EventPerfOverdue  = "perf_overdue"  // 性能工单 SLA 超时升级（独立事件；旧渠道仍按“执行异常”收到）
	EventInspectAlert = "inspect_alert" // 巡检发现问题（独立事件；旧渠道仍按“执行异常”收到）
)

// Events 是全部事件名及其显示名，按展示顺序。
var Events = []struct{ Name, Label string }{
	{EventPending, "待处理（新工单、流转到下一级）"}, {EventSubmitted, "新工单提交"}, {EventPassed, "审批通过"}, {EventRejected, "被驳回"}, {EventAborted, "提交人终止"},
	{EventScheduled, "已设置定时执行"}, {EventExecuting, "开始执行"}, {EventFinished, "执行完成"}, {EventException, "执行异常"},
	{EventPerfOverdue, "性能工单超时（SLA 超期升级）"}, {EventInspectAlert, "巡检告警"},
}

// 工单类别。
const (
	KindQueryPrivilege = "query_privilege"
	KindSQLWorkflow    = "sql_workflow"
)

// @ 规则的目标类型。
const (
	TargetApplicant = "applicant"  // 工单提交人
	TargetReviewers = "reviewers"  // 当前节点的审批人
	TargetAuthGroup = "auth_group" // 某个权限组（Value 为权限组 ID）在工单所属资源组内的成员
	TargetUser      = "user"       // 指定的平台用户（Value 为用户名）
	TargetRaw       = "raw"        // 渠道原生标识（Value 如 Slack 的 U…/S…、钉钉手机号、企业微信 userid / 手机号、飞书 open_id）
	TargetAll       = "all"        // 所有人
	TargetDBADuty   = "dba_duty"   // DBA 值班：渠道上配置的值班人员列表（Channel.DutyUsers）
)

// Rule 是一条 @ 规则。Events 为空表示对该渠道订阅的所有事件都 @。
type Rule struct {
	Target string   `json:"target"`
	Value  string   `json:"value,omitempty"`
	Events []string `json:"events,omitempty"`
	// WorkHours 为 true 时只在工作时间（周一至周五 09:00-19:00，服务器本地时区）@，其余时间只发消息不 @。
	WorkHours bool `json:"work_hours,omitempty"`
}

// Channel 是一个机器人渠道（已解密）。
type Channel struct {
	ID         uint
	GroupID    uint // 0 表示全局
	Name       string
	Type       string
	WebhookURL string
	Secret     string
	Events     []string // 订阅的事件；空表示全部
	Rules      []Rule
	Enabled    bool
	Legacy     bool // 来自 dbt_resource_group 表里的旧 webhook 列（无 @ 规则、无签名）

	// 以下来自旁路表 dbt_notify_channel_ext（deploy/sql/091_notify_ext.sql）；表不存在时全是零值，行为与升级前完全一致。
	GroupIDs  []uint   // 关联的多个资源组；为空时只看 GroupID（0 = 全局）
	DutyUsers []string // “DBA 值班”目标的值班人员（用户名）
	EventsV2  bool     // 订阅是在“细分事件”模型下保存的（见 wants）；旧渠道为 false

	// 邮件渠道（Type = email）的 SMTP 配置（dbt_notify_email，deploy/sql/210_notify_email.sql）；SMTP 密码在 Secret 里。其它类型为 nil。
	Email *EmailConfig
}

// IsGlobal 表示渠道是全局渠道：没有关联任何资源组（group_id = 0 且没有扩展的多资源组关联），对所有资源组的事件都生效。
// dbt_resource_group 表里的旧 webhook 属于该资源组，不算全局。
func (c Channel) IsGlobal() bool { return !c.Legacy && c.GroupID == 0 && len(c.GroupIDs) == 0 }

// contains 判断 list 是否包含 s。
func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// independent 是“独立事件”：它们原来复用别的事件名发送（性能 SLA 超时、巡检告警都借用了 exception）。
// 旧渠道（EventsV2=false）订阅了借用的事件仍会收到；按细分模型保存过的渠道只按事件本身匹配。
func independent(specific string) bool {
	return specific == EventPerfOverdue || specific == EventInspectAlert
}

// eventHit 判断订阅 / 过滤列表 list 是否命中事件 ev（list 为空由调用方处理）。
//
//   - 命中事件自己的名字（Name）或更细的事件名（Specific）都算；
//   - 独立事件对按细分模型保存的渠道（v2）只认 Specific，避免“订阅了执行异常”的新渠道被性能 / 巡检消息打扰；
//   - 新工单提交（submitted）同时属于“待处理”（pending），所以订阅 pending 的渠道照旧收到新工单。
func eventHit(list []string, ev Event, v2 bool) bool {
	if ev.Specific != "" {
		if contains(list, ev.Specific) {
			return true
		}
		if v2 && independent(ev.Specific) {
			return false
		}
	}
	return contains(list, ev.Name)
}

// wants 判断渠道是否订阅了这个事件。订阅为空表示全部。
func (c Channel) wants(ev Event) bool {
	if len(c.Events) == 0 {
		return true
	}
	return eventHit(c.Events, ev, c.EventsV2)
}

// Event 是一条要通知的业务事件。
type Event struct {
	Kind                string // KindQueryPrivilege / KindSQLWorkflow
	Name                string // Event* 之一
	Specific            string // 可选：更细的事件名（EventSubmitted / EventPerfOverdue / EventInspectAlert）；Name 仍保持原来的取值
	Title               string
	Content             string
	GroupID             uint     // 工单所属资源组
	Applicant           string   // 提交人用户名，用于 @提交人
	ReviewerAuthGroupID uint     // 当前节点的审批权限组，用于 @审批人；0 表示没有
	ExtraUsers          []string // 无论 @ 规则如何都要 @ 的用户名（如 DDL 执行完成要通知的权限组成员）
	WorkTime            *bool    // 非 nil 时强制指定“现在是否工作时间”（预览 / 模拟用）；nil 取当前时间
	Test                bool     // 管理页的测试发送：发送记录里标为测试
	SkipGlobal          bool     // 为 true 时不发到全局渠道（未关联任何资源组的渠道）；默认 false = 全局渠道照常收到（性能工单 SLA 升级矩阵的“全局渠道”列用）

	// 以下供消息模板用（见 tpl_*.go）：没有模板、或产生方没有填时，行为与原来完全一致（发 Title + Content）。
	Vars   map[string]string // 模板变量的值，键见 tpl_catalog.go 的 Vars；SQL 全文放在 "sql"
	TplKey string            // 显式指定消息键（如 "priv.changed"）；空则按 Kind 与 Specific / Name 推算
	rich   *Message          // 已按模板渲染好的消息（Dispatcher 内部使用，同一事件发多个渠道时每个渠道各自渲染）
}

// Person 是可以被 @ 的平台用户及其在各渠道的标识（来自 dbt_sql_users 表）。
type Person struct {
	Username string
	Display  string
	DingID   string
	FeishuID string
	SlackID  string
	Email    string // 邮件渠道的收件地址（dbt_sql_users.email）
}

func (p Person) name() string {
	if p.Display != "" {
		return p.Display
	}
	return p.Username
}

// Directory 解析 @ 目标。
type Directory interface {
	UsersByName(usernames []string) ([]Person, error)
	// AuthGroupMembers 返回属于某个权限组、且属于某个资源组的启用用户。
	AuthGroupMembers(authGroupID, resourceGroupID uint) ([]Person, error)
}

// Source 提供渠道配置。
type Source interface {
	// Channels 返回某个资源组生效的渠道：该组的 + 全局的（已解密），包含旧 webhook 列转换来的渠道。
	Channels(groupID uint) ([]Channel, error)
}

// Notifier 是业务代码使用的发送接口。
type Notifier interface {
	Notify(ctx context.Context, ev Event)
}

// Noop 什么都不发送。
type Noop struct{}

func (Noop) Notify(context.Context, Event) {}

// ParseIDs 把逗号分隔的数字列表解析出来，忽略非法项。
func ParseIDs(s string) []uint {
	var out []uint
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.ParseUint(strings.TrimSpace(p), 10, 32); err == nil {
			out = append(out, uint(n))
		}
	}
	return out
}

// SplitEvents 把逗号分隔的事件列表解析出来。
func SplitEvents(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
