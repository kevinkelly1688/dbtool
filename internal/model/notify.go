// @author: kevinkelly
// @license: Apache Licence
// @file: notify.go
// @time: 2026/10/28

package model

import "time"

// NotifyChannel -> dbt_notify_channel（dbtool 新增的表，见 deploy/sql/001_notify_channel.sql）。
// WebhookURL 与 Secret 存的是加密后的密文，由 store 层加解密。
type NotifyChannel struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	GroupID     uint      `gorm:"column:group_id" json:"group_id"`
	Name        string    `gorm:"column:name" json:"name"`
	ChannelType string    `gorm:"column:channel_type" json:"channel_type"`
	WebhookURL  string    `gorm:"column:webhook_url" json:"-"`
	Secret      string    `gorm:"column:secret" json:"-"`
	Events      string    `gorm:"column:events" json:"events"`
	Mentions    string    `gorm:"column:mentions" json:"mentions"`
	Enabled     bool      `gorm:"column:enabled" json:"enabled"`
	CreatedBy   string    `gorm:"column:created_by" json:"created_by"`
	CreateTime  time.Time `gorm:"column:create_time;autoCreateTime" json:"create_time"`
	SysTime     time.Time `gorm:"column:sys_time;autoUpdateTime" json:"sys_time"`
}

func (NotifyChannel) TableName() string { return "dbt_notify_channel" }

// NotifyChannelExt -> dbt_notify_channel_ext（渠道的扩展信息，与 dbt_notify_channel 一对一；见 deploy/sql/091_notify_ext.sql）。
type NotifyChannelExt struct {
	ChannelID  uint      `gorm:"column:channel_id;primaryKey" json:"channel_id"`
	GroupIDs   string    `gorm:"column:group_ids" json:"group_ids"`   // 关联的资源组 ID，逗号分隔；空表示只看 dbt_notify_channel.group_id
	DutyUsers  string    `gorm:"column:duty_users" json:"duty_users"` // DBA 值班人员用户名，逗号分隔
	EventsV2   bool      `gorm:"column:events_v2" json:"events_v2"`   // 订阅按细分事件模型保存
	UpdateTime time.Time `gorm:"column:update_time" json:"update_time"`
}

func (NotifyChannelExt) TableName() string { return "dbt_notify_channel_ext" }

// NotifyLog -> dbt_notify_log（通知发送记录，保留 30 天；见 deploy/sql/092_notify_log.sql）。
type NotifyLog struct {
	ID          uint64    `gorm:"primaryKey" json:"id"`
	ChannelID   uint      `gorm:"column:channel_id" json:"channel_id"`
	ChannelName string    `gorm:"column:channel_name" json:"channel_name"`
	Event       string    `gorm:"column:event" json:"event"`
	Kind        string    `gorm:"column:kind" json:"kind"`
	OK          bool      `gorm:"column:ok" json:"ok"`
	CostMs      int       `gorm:"column:cost_ms" json:"cost_ms"`
	Error       string    `gorm:"column:error" json:"error"`
	Mentions    int       `gorm:"column:mentions" json:"mentions"`
	IsTest      bool      `gorm:"column:is_test" json:"is_test"`
	CreatedAt   time.Time `gorm:"column:created_at" json:"created_at"`
}

func (NotifyLog) TableName() string { return "dbt_notify_log" }

// NotifyEmail -> dbt_notify_email（邮件渠道的 SMTP 配置 JSON，与 dbt_notify_channel 一对一；见 deploy/sql/210_notify_email.sql）。
// 密码不在这里：与签名密钥共用 dbt_notify_channel.secret（加密）。
type NotifyEmail struct {
	ChannelID  uint      `gorm:"column:channel_id;primaryKey" json:"channel_id"`
	Config     string    `gorm:"column:config" json:"config"`
	UpdateTime time.Time `gorm:"column:update_time" json:"update_time"`
}

func (NotifyEmail) TableName() string { return "dbt_notify_email" }

// NotifyTemplate -> dbt_notify_template（通知消息模板；见 deploy/sql/260_notify_template.sql）。
type NotifyTemplate struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	EventKey  string    `gorm:"column:event_key" json:"event_key"`
	ScopeKey  string    `gorm:"column:scope_key" json:"scope_key"`
	Name      string    `gorm:"column:name" json:"name"`
	Enabled   bool      `gorm:"column:enabled" json:"enabled"`
	Title     string    `gorm:"column:title" json:"title"`
	Tone      string    `gorm:"column:tone" json:"tone"`
	Icon      string    `gorm:"column:icon" json:"icon"`
	Blocks    string    `gorm:"column:blocks" json:"blocks"`
	UpdatedBy string    `gorm:"column:updated_by" json:"updated_by"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (NotifyTemplate) TableName() string { return "dbt_notify_template" }

// NotifyTemplateHist -> dbt_notify_template_hist（消息模板的修改历史）。
type NotifyTemplateHist struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	EventKey  string    `gorm:"column:event_key" json:"event_key"`
	ScopeKey  string    `gorm:"column:scope_key" json:"scope_key"`
	Action    string    `gorm:"column:action" json:"action"`
	Snapshot  string    `gorm:"column:snapshot" json:"snapshot"`
	Note      string    `gorm:"column:note" json:"note"`
	Operator  string    `gorm:"column:operator" json:"operator"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
}

func (NotifyTemplateHist) TableName() string { return "dbt_notify_template_hist" }
