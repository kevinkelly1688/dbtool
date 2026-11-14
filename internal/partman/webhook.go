// @author: kevinkelly
// @license: Apache Licence
// @file: webhook.go
// @time: 2026/10/28

package partman

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Webhook -> dbt_partman_webhook（不改结构）。secret = 加签密钥，keyword = 自定义关键词。
type Webhook struct {
	ID         uint      `gorm:"column:id;primaryKey"`
	URL        string    `gorm:"column:webhook_url"`
	Type       string    `gorm:"column:type"`
	IsEnabled  bool      `gorm:"column:isenabled"`
	MentionOn  string    `gorm:"column:mention_on"`
	Mention    string    `gorm:"column:mention"` // JSON 数组或逗号分隔
	Keyword    string    `gorm:"column:keyword"`
	Secret     string    `gorm:"column:secret"`
	CreateTime time.Time `gorm:"column:create_time"`
}

func (Webhook) TableName() string { return "dbt_partman_webhook" }

// WebhookView 是接口返回的通道：地址脱敏，密钥只给“已设置”标志。
type WebhookView struct {
	ID         uint     `json:"id"`
	Type       string   `json:"type"`
	WebhookURL string   `json:"webhook_url"`
	IsEnabled  bool     `json:"isenabled"`
	MentionOn  string   `json:"mention_on"`
	Mention    []string `json:"mention"`
	Keyword    string   `json:"keyword"`
	SecretSet  bool     `json:"secret_set"`
	CreateTime string   `json:"create_time"`
}

func webhookView(w Webhook) WebhookView {
	m := parseMentionList(w.Mention)
	if m == nil {
		m = []string{}
	}
	return WebhookView{ID: w.ID, Type: normChannelType(w.Type), WebhookURL: maskWebhook(w.URL), IsEnabled: w.IsEnabled, MentionOn: normMentionOn(w.MentionOn),
		Mention: m, Keyword: w.Keyword, SecretSet: w.Secret != "", CreateTime: fmtTime(w.CreateTime)}
}

// WebhookInput 是新增 / 修改的请求体。修改时 webhook_url、secret 为空表示不改；clear_secret 清空密钥。
type WebhookInput struct {
	Type        string   `json:"type"`
	WebhookURL  string   `json:"webhook_url"`
	IsEnabled   bool     `json:"isenabled"`
	MentionOn   string   `json:"mention_on"`
	Mention     []string `json:"mention"`
	Keyword     string   `json:"keyword"`
	Secret      string   `json:"secret"`
	ClearSecret bool     `json:"clear_secret"`
}

const (
	maxWebhookURL = 500
	maxWebhookCol = 500
)

// WebhookList 是 GET /webhooks 的响应。
type WebhookList struct {
	Items           []WebhookView `json:"items"`
	NotifyOn        string        `json:"notify_on"`
	NotifyOnOptions []string      `json:"notify_on_options"`
}

func (s *Service) needWebhook(c Caller) error {
	return c.need(permChange, "管理分区维护的推送通道")
}

// Webhooks 列出全部通道（含停用的）。
func (s *Service) Webhooks(c Caller) (*WebhookList, error) {
	if err := s.needWebhook(c); err != nil {
		return nil, err
	}
	rows, err := s.repo.ListWebhooks(false)
	if err != nil {
		return nil, err
	}
	out := &WebhookList{Items: make([]WebhookView, 0, len(rows)), NotifyOn: s.notifyOn(), NotifyOnOptions: notifyOnValues}
	for _, r := range rows {
		out.Items = append(out.Items, webhookView(r))
	}
	return out, nil
}

// normalize 校验并规整输入，返回要存的 Mention 串。
func (in *WebhookInput) normalize() (mention string, err error) {
	in.Type = strings.ToLower(strings.TrimSpace(in.Type))
	if !contains(channelTypes, in.Type) {
		return "", badReq("推送平台只能是 slack、wecom、feishu、dingtalk")
	}
	in.MentionOn = strings.ToLower(strings.TrimSpace(in.MentionOn))
	if in.MentionOn == "" {
		in.MentionOn = notifyOnFailure
	}
	if !contains(notifyOnValues, in.MentionOn) {
		return "", badReq("@ 时机只能是 always、on_failure、never")
	}
	in.WebhookURL = strings.TrimSpace(in.WebhookURL)
	if len([]rune(in.WebhookURL)) > maxWebhookURL {
		return "", badReq("webhook 地址最多 %d 个字符", maxWebhookURL)
	}
	in.Keyword = strings.TrimSpace(in.Keyword)
	if len([]rune(in.Keyword)) > maxWebhookCol {
		return "", badReq("自定义关键词最多 %d 个字符", maxWebhookCol)
	}
	if len([]rune(in.Secret)) > maxWebhookCol {
		return "", badReq("加签密钥最多 %d 个字符", maxWebhookCol)
	}
	var ms []string
	for _, m := range in.Mention {
		if m = strings.TrimSpace(m); m != "" {
			ms = append(ms, m)
		}
	}
	if ms == nil {
		ms = []string{}
	}
	b, _ := json.Marshal(ms)
	if len([]rune(string(b))) > maxWebhookCol {
		return "", badReq("@ 对象过多（序列化后最多 %d 个字符）", maxWebhookCol)
	}
	return string(b), nil
}

func (s *Service) CreateWebhook(c Caller, in WebhookInput) (*WebhookView, error) {
	if err := s.needWebhook(c); err != nil {
		return nil, err
	}
	mention, err := in.normalize()
	if err != nil {
		return nil, err
	}
	if in.WebhookURL == "" {
		return nil, badReq("请填写 webhook 地址")
	}
	if err := checkWebhookURL(in.Type, in.WebhookURL); err != nil {
		return nil, err
	}
	rows, err := s.repo.ListWebhooks(false)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if strings.TrimSpace(r.URL) == in.WebhookURL {
			return nil, newErr(409, "duplicate", "该 webhook 地址已经配置过（%s）", maskWebhook(in.WebhookURL))
		}
	}
	w := Webhook{URL: in.WebhookURL, Type: in.Type, IsEnabled: in.IsEnabled, MentionOn: in.MentionOn, Mention: mention,
		Keyword: in.Keyword, Secret: in.Secret, CreateTime: s.now()}
	if err := s.repo.InsertWebhook(&w); err != nil {
		return nil, err
	}
	s.auditBestEffort(c, "分区维护-新增推送通道", fmt.Sprintf("通道 #%d：%s %s，启用 %v，@时机 %s，@对象 %d 个，关键词 %s，加签密钥 %s", // i18n-ignore：审计日志正文，不翻译
		w.ID, w.Type, maskWebhook(w.URL), w.IsEnabled, w.MentionOn, len(in.Mention), setLabel(w.Keyword != ""), setLabel(w.Secret != "")))
	v := webhookView(w)
	return &v, nil
}

func setLabel(b bool) string {
	if b {
		return "已设置"
	}
	return "未设置"
}

func (s *Service) loadWebhook(id uint) (*Webhook, error) {
	rows, err := s.repo.ListWebhooks(false)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i], nil
		}
	}
	return nil, newErr(404, "not_found", "推送通道不存在")
}

func (s *Service) UpdateWebhook(c Caller, id uint, in WebhookInput) (*WebhookView, error) {
	if err := s.needWebhook(c); err != nil {
		return nil, err
	}
	w, err := s.loadWebhook(id)
	if err != nil {
		return nil, err
	}
	mention, err := in.normalize()
	if err != nil {
		return nil, err
	}
	newURL := in.WebhookURL
	if newURL == "" {
		newURL = strings.TrimSpace(w.URL)
	}
	if err := checkWebhookURL(in.Type, newURL); err != nil {
		return nil, err
	}
	if newURL != strings.TrimSpace(w.URL) {
		rows, err := s.repo.ListWebhooks(false)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if r.ID != w.ID && strings.TrimSpace(r.URL) == newURL {
				return nil, newErr(409, "duplicate", "该 webhook 地址已经配置过（%s）", maskWebhook(newURL))
			}
		}
	}
	secretNote := "未改" // i18n-ignore：审计日志正文
	switch {
	case in.ClearSecret:
		w.Secret, secretNote = "", "已清空" // i18n-ignore：审计日志正文
	case in.Secret != "":
		w.Secret, secretNote = in.Secret, "已更新" // i18n-ignore：审计日志正文
	}
	before := fmt.Sprintf("%s %s 启用 %v @时机 %s", normChannelType(w.Type), maskWebhook(w.URL), w.IsEnabled, normMentionOn(w.MentionOn)) // i18n-ignore：审计日志正文
	w.URL, w.Type, w.IsEnabled, w.MentionOn, w.Mention, w.Keyword = newURL, in.Type, in.IsEnabled, in.MentionOn, mention, in.Keyword
	if err := s.repo.UpdateWebhook(w); err != nil {
		return nil, err
	}
	s.auditBestEffort(c, "分区维护-编辑推送通道", fmt.Sprintf("通道 #%d：%s => %s %s 启用 %v @时机 %s @对象 %d 个 关键词 %s 加签密钥%s", // i18n-ignore：审计日志正文，不翻译
		w.ID, before, w.Type, maskWebhook(w.URL), w.IsEnabled, w.MentionOn, len(in.Mention), setLabel(w.Keyword != ""), secretNote))
	v := webhookView(*w)
	return &v, nil
}

func (s *Service) DeleteWebhook(c Caller, id uint) error {
	if err := s.needWebhook(c); err != nil {
		return err
	}
	w, err := s.loadWebhook(id)
	if err != nil {
		return err
	}
	if err := s.repo.DeleteWebhook(id); err != nil {
		return err
	}
	s.auditBestEffort(c, "分区维护-删除推送通道", fmt.Sprintf("通道 #%d：%s %s", w.ID, normChannelType(w.Type), maskWebhook(w.URL))) // i18n-ignore：审计日志正文，不翻译
	return nil
}

// SetWebhookEnabled 启停通道；与当前值相同是空操作（不写审计）。
func (s *Service) SetWebhookEnabled(c Caller, id uint, enable bool) (*WebhookView, error) {
	if err := s.needWebhook(c); err != nil {
		return nil, err
	}
	w, err := s.loadWebhook(id)
	if err != nil {
		return nil, err
	}
	if w.IsEnabled != enable {
		w.IsEnabled = enable
		if err := s.repo.UpdateWebhook(w); err != nil {
			return nil, err
		}
		verb := "停用" // i18n-ignore：审计动作名，不翻译
		if enable {
			verb = "启用" // i18n-ignore：审计动作名，不翻译
		}
		s.auditBestEffort(c, "分区维护-"+verb+"推送通道", fmt.Sprintf("通道 #%d：%s %s", w.ID, normChannelType(w.Type), maskWebhook(w.URL))) // i18n-ignore：审计日志正文，不翻译
	}
	v := webhookView(*w)
	return &v, nil
}

// TestResult 是测试发送的响应。
type TestResult struct {
	NotifyOn string       `json:"notify_on"`
	Results  []PushResult `json:"results"`
	LogID    uint         `json:"log_id,omitempty"`
}

const testHeader = "分区维护推送测试"                            // i18n-ignore：发给外部平台的消息正文，语言属于接收方
const testBody = "这是一条来自 dbtool 分区维护的测试消息，收到即表示该通道可用。\n" // i18n-ignore：发给外部平台的消息正文，语言属于接收方

// TestWebhooks 测试发送：id=0 发给所有已启用通道；否则只发这一个（不论是否启用）。不依赖维护事件，不受 partition.notify_on 影响。
// 结果同时写进“系统记录”（运行输出里可见，仅有 change_partmanconfig 的人可看）。
func (s *Service) TestWebhooks(ctx context.Context, c Caller, id uint) (*TestResult, error) {
	if err := s.needWebhook(c); err != nil {
		return nil, err
	}
	var rows []Webhook
	if id != 0 {
		w, err := s.loadWebhook(id)
		if err != nil {
			return nil, err
		}
		rows = []Webhook{*w}
	} else {
		var err error
		if rows, err = s.repo.ListWebhooks(true); err != nil {
			return nil, err
		}
	}
	if len(rows) == 0 {
		return nil, newErr(409, "no_channel", "没有已启用的推送通道可供测试")
	}
	chs := make([]pushChannel, 0, len(rows))
	for _, r := range rows {
		chs = append(chs, channelOf(r))
	}
	header := fmt.Sprintf("%s  %s", testHeader, s.now().Format("2006-01-02 15:04:05"))
	results := s.sendAll(ctx, chs, header, testBody, false)

	tr := newTracer(s.now)
	tr.add("INFO", "=== 推送测试开始 | 操作人=%s 通道数=%d ===", c.Username, len(chs))
	bad := 0
	for i, r := range results {
		if r.OK {
			tr.add("INFO", "推送 %s(#%d) %s 成功（HTTP %d）", chs[i].Type, chs[i].ID, r.WebhookURL, r.Status)
		} else {
			bad++
			tr.add("WARN", "推送 %s(#%d) %s 失败：%s", chs[i].Type, chs[i].ID, r.WebhookURL, r.Message)
		}
	}
	tr.add("INFO", "=== 推送测试结束 | 成功 %d / 失败 %d ===", len(results)-bad, bad)
	status := "success"
	if bad > 0 {
		status = "failed"
	}
	out := &TestResult{NotifyOn: s.notifyOn(), Results: results}
	out.LogID = s.writeSystemLog(tr.lines, logItem{Status: status, Operator: c.Username, Source: SourceManual, ExecAt: s.now().Format(timeLayout), Audience: audienceAdmin, Event: "webhook_test"})
	s.auditBestEffort(c, "分区维护-测试推送", fmt.Sprintf("通道 %d 个，成功 %d / 失败 %d", len(results), len(results)-bad, bad)) // i18n-ignore：审计日志正文，不翻译
	return out, nil
}
