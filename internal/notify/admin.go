// @author: kevinkelly
// @license: Apache Licence
// @file: admin.go
// @time: 2026/10/28

package notify

import "time"

// ChannelView 是管理页面展示用的渠道（不含明文地址和密钥）。
type ChannelView struct {
	ID          uint     `json:"id"`
	GroupID     uint     `json:"group_id"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	WebhookHint string   `json:"webhook_hint"` // 只显示主机名，地址里的 token 不回显
	HasSecret   bool     `json:"has_secret"`
	Events      []string `json:"events"`
	Rules       []Rule   `json:"rules"`
	Enabled     bool     `json:"enabled"`
	CreatedBy   string   `json:"created_by"`

	// 以下来自旁路表 dbt_notify_channel_ext / dbt_notify_log；表没建时为空值。
	Email     *EmailConfig `json:"email,omitempty"` // 邮件渠道的 SMTP 配置（不含密码；密码是否已设置看 has_secret）
	GroupIDs  []uint       `json:"group_ids"`       // 关联的资源组；空 = 全局
	DutyUsers []string     `json:"duty_users"`      // DBA 值班人员（用户名）
	LastSend  *SendView    `json:"last_send"`       // 最近一次发送
}

// SendView 是发送记录在页面上的样子。
type SendView struct {
	ID          uint64 `json:"id,omitempty"`
	ChannelID   uint   `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Event       string `json:"event"`
	OK          bool   `json:"ok"`
	CostMs      int    `json:"cost_ms"`
	Error       string `json:"error"`
	Mentions    int    `json:"mentions"`
	Test        bool   `json:"test"`
	Time        string `json:"time"`
}

// ChannelInput 是创建/修改渠道的输入。修改时 URL 为空表示保持原地址，Secret 为空表示保持原密钥。
type ChannelInput struct {
	GroupID     uint
	Name        string
	Type        string
	URL         string
	Secret      string
	ClearSecret bool
	Events      []string
	Rules       []Rule
	Enabled     bool
	Operator    string
	// 以下写旁路表 dbt_notify_channel_ext（表不存在时忽略）。GroupIDs 非空时 GroupID 取其第一个，保持旧代码按单个资源组读取也能工作。
	GroupIDs  []uint
	DutyUsers []string
	// 邮件渠道的 SMTP 配置（写 dbt_notify_email；表不存在时 SaveChannel 返回错误）。密码走 Secret / ClearSecret，与签名密钥同一套。nil 表示不改。
	Email *EmailConfig
}

// IDName 是 (ID, 名称) 对，用于下拉选项。
type IDName struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

// Admin 是渠道管理需要的数据访问（store.Store 与 memrepo.Repo 实现）。
type Admin interface {
	NotifyTableReady() bool
	ListChannelViews() ([]ChannelView, error)
	GetChannel(id uint) (*Channel, error) // 解密后的渠道，用于测试发送；不存在返回 nil, nil
	SaveChannel(id uint, in ChannelInput) (uint, error)
	DeleteChannel(id uint) error
	ListAuthGroups() ([]IDName, error)
	ListResourceGroups() ([]IDName, error)
}

// LogFilter 是发送记录的查询条件。
type LogFilter struct {
	ChannelID  uint
	OnlyFailed bool
	Event      string
	Since      *time.Time
	Offset     int
	Limit      int
}

// SendStats 是一段时间内的发送汇总。
type SendStats struct {
	Total  int64
	Failed int64
}

// LogStore 是发送记录的读取能力（store.Store 实现；可选：Admin 的实现不一定提供，此时页面不显示发送记录）。
// 写入走 LogSink（Dispatcher 发送时调用）。
type LogStore interface {
	LogTableReady() bool
	LastSends() (map[uint]SendLog, error)
	ListSendLogs(f LogFilter) ([]SendLog, int64, error)
	SendStats(since time.Time) (SendStats, error)
}

// View 把发送记录转成页面展示的样子。
func (l SendLog) View() SendView {
	return SendView{ID: l.ID, ChannelID: l.ChannelID, ChannelName: l.ChannelName, Event: l.Event, OK: l.OK, CostMs: l.CostMs,
		Error: l.Error, Mentions: l.Mentions, Test: l.Test, Time: l.At.Format("2006-01-02 15:04:05")}
}

// EnabledSetter 是可选能力：只改渠道的启用状态，不动其它配置（尤其是订阅模型标记），行内启停用它。
type EnabledSetter interface {
	SetChannelEnabled(id uint, enabled bool) error
}

// EmailAdmin 是可选能力：邮件渠道的配置表（dbt_notify_email，deploy/sql/210_notify_email.sql）是否可用。不提供该能力的仓库不支持邮件渠道。
type EmailAdmin interface {
	NotifyEmailReady() bool
}

// TemplateAdmin 是可选能力：消息模板（deploy/sql/260_notify_template.sql）的管理。不提供该能力的仓库没有消息模板页。
// 同一个仓库同时实现 TemplateSource（ActiveTemplates），发送时按它取模板。
type TemplateAdmin interface {
	TemplateTableReady() bool
	ListTemplates() ([]Template, error)
	GetTemplate(eventKey, scope string) (*Template, error) // 不存在返回 nil, nil
	// SaveTemplate 新建或覆盖一条模板（唯一键：消息键 + 范围），并记一条历史。
	SaveTemplate(t Template, operator, note string) error
	// DeleteTemplate 删除一条模板（恢复到上一层 / 系统默认），并记一条历史。
	DeleteTemplate(eventKey, scope, operator, note string) error
	TemplateHistory(eventKey, scope string, limit int) ([]TemplateHist, error)
}

// ExtAdmin 是可选能力：渠道扩展信息（多资源组、值班人员、细分事件模型）是否可用。
type ExtAdmin interface {
	NotifyExtReady() bool
}
