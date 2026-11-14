// @author: kevinkelly
// @license: Apache Licence
// @file: dispatch.go
// @time: 2026/10/28

package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Dispatcher 按事件把消息发到资源组配置的各个渠道。
type Dispatcher struct {
	Source Source
	Dir    Directory
	Client *http.Client
	Now    func() time.Time
	Sink   LogSink // 发送记录（可为空）；Source 同时实现了 LogSink 时 NewDispatcher 自动取用

	// Templates 提供消息模板（可为空）；Source 同时实现了 TemplateSource 时自动取用。没有模板时发系统默认文案。
	Templates TemplateSource
	tpls      tplCache
}

func NewDispatcher(src Source, dir Directory) *Dispatcher {
	d := &Dispatcher{Source: src, Dir: dir, Client: &http.Client{Timeout: 5 * time.Second}, Now: time.Now}
	if s, ok := src.(LogSink); ok {
		d.Sink = s
	}
	if t, ok := src.(TemplateSource); ok {
		d.Templates = t
	}
	return d
}

const maxMentions = 50 // 单条消息最多 @ 的人数，防止配置失误造成刷屏

// Mention 是解析后的一个 @ 对象：平台用户，或渠道原生标识。
type Mention struct {
	Person *Person
	Raw    string
}

// Notify 实现 Notifier：向所有订阅了该事件的渠道发送。失败只记日志。
func (d *Dispatcher) Notify(ctx context.Context, ev Event) {
	chans, err := d.Source.Channels(ev.GroupID)
	if err != nil {
		slog.Warn("读取通知渠道失败", "err", err)
		return
	}
	for _, ch := range chans {
		if !ch.Enabled || (ev.SkipGlobal && ch.IsGlobal()) || !ch.wants(ev) {
			continue
		}
		ments, _ := d.resolveExplain(ch, ev)
		if err := d.deliver(ctx, ch, ev, ments); err != nil {
			slog.Warn("通知发送失败", "channel", ch.Name, "type", ch.Type, "group_id", ch.GroupID, "title", ev.Title, "err", err)
		}
	}
}

// SendTest 向一个渠道发送一条消息，返回错误信息供界面展示；不看渠道是否启用、是否订阅。ev 用来解析 @ 规则
// （调用方通常把当前用户设为 Applicant，这样“@提交人”的规则会 @ 到操作者本人）。
// 管理页的测试发送把 ev.Test 置为 true，发送记录里会标明是测试；定时巡检等按渠道 ID 直接发送的调用方不置。
func (d *Dispatcher) SendTest(ctx context.Context, ch Channel, ev Event) error {
	ments, _ := d.resolveExplain(ch, ev)
	return d.deliver(ctx, ch, ev, ments)
}

// deliver 发送一条消息并记录发送结果（渠道已保存、Sink 可用时）。记录失败不影响发送结果。
func (d *Dispatcher) deliver(ctx context.Context, ch Channel, ev Event, ments []Mention) error {
	_, err := d.deliverTo(ctx, ch, ev, ments, "")
	return err
}

// deliverTo 是 deliver 的完整形式：邮件渠道可以指定只发给 testTo（测试发送），并返回 SMTP 各阶段结果。
func (d *Dispatcher) deliverTo(ctx context.Context, ch Channel, ev Event, ments []Mention, testTo string) ([]EmailStage, error) {
	t0 := time.Now()
	var (
		err    error
		note   string
		stages []EmailStage
		n      = len(ments)
	)
	if ev.rich == nil {
		ev.rich = d.render(ch, ev) // 命中了消息模板就按模板渲染；没有则保持 nil，走原来的 Title + Content
	}
	if ch.Type == TypeEmail {
		n, note, stages, err = d.sendEmail(ctx, ch, ev, ments, testTo)
		if ch.Secret != "" && err != nil { // 防御：错误信息里不能出现 SMTP 密码
			err = errors.New(strings.ReplaceAll(err.Error(), ch.Secret, "***"))
		}
	} else {
		err = d.send(ctx, ch, ev, ments)
	}
	if d.Sink != nil && ch.ID != 0 {
		rec := SendLog{ChannelID: ch.ID, ChannelName: ch.Name, Event: ev.Name, Kind: ev.Kind, OK: err == nil,
			CostMs: int(time.Since(t0).Milliseconds()), Mentions: n, Test: ev.Test, At: d.Now()}
		if ev.Specific != "" {
			rec.Event = ev.Specific
		}
		if err != nil {
			rec.Error = trimMsg([]byte(err.Error()))
		} else if note != "" {
			rec.Error = trimMsg([]byte(note)) // 成功但有提示（如跳过了没有邮箱的用户）：写在“说明”里
		}
		d.Sink.RecordSend(rec)
	}
	return stages, err
}

// built 是构造好的一次 HTTP 请求。
type built struct {
	URL   string
	Body  []byte
	Check func(body []byte) error // 校验响应内容（HTTP 200 之外的业务错误码），可为空
}

func (d *Dispatcher) send(ctx context.Context, ch Channel, ev Event, ments []Mention) error {
	b, err := build(ch, ev, ments, d.Now())
	if err != nil {
		return err
	}
	u, err := url.Parse(b.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("webhook 地址不是合法的 http(s) 地址") // 不把地址写进错误：其中常带 token
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.URL, bytes.NewReader(b.Body))
	if err != nil {
		return errors.New("构造请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.Client.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败（%s）", u.Host)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d（%s）：%s", resp.StatusCode, u.Host, trimMsg(body))
	}
	if b.Check != nil {
		if err := b.Check(body); err != nil {
			return err
		}
	}
	return nil
}

func trimMsg(b []byte) string {
	s := strings.TrimSpace(string(b))
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

// build 构造一次 HTTP 请求：事件带了按模板渲染好的消息（ev.rich）就发富文本，否则（或渲染后超过渠道大小上限）发原来的 Title + Content 文本。
func build(ch Channel, ev Event, ments []Mention, now time.Time) (*built, error) {
	if ev.rich != nil {
		b, err := buildRich(ch, ev.rich, ments, now)
		if err == nil {
			return b, nil
		}
		slog.Warn("消息模板渲染失败，改用系统默认文案", "channel", ch.Name, "type", ch.Type, "message", ev.rich.Key, "err", err)
	}
	text := ev.Title + "\n" + ev.Content
	switch ch.Type {
	case TypeDingTalk:
		return buildDingTalk(ch, text, ments, now)
	case TypeWeCom:
		return buildWeCom(ch, text, ments)
	case TypeFeishu, TypeLark:
		return buildFeishu(ch, text, ments, now)
	case TypeSlack:
		return buildSlack(ch, text, ments)
	case TypeTelegram:
		return buildTelegram(ch, text, ments)
	}
	return nil, fmt.Errorf("不支持的渠道类型 %s", strconv.Quote(ch.Type))
}

var (
	mobileRe  = regexp.MustCompile(`^\d{11}$`)
	safeIDRe  = regexp.MustCompile(`^[A-Za-z0-9_\-.:@]{1,128}$`)
	slackIDRe = regexp.MustCompile(`^[A-Z0-9]{3,32}$`)
)

// ---- 钉钉 ----

// dingAt 是钉钉消息里 @ 的部分：正文前缀（钉钉要求正文里出现 @手机号 / @userId 才会高亮并提醒）与 at 字段。
type dingAt struct {
	prefix  string
	userIDs []string
	mobiles []string
	all     bool
}

func (a dingAt) field() map[string]any {
	return map[string]any{"atUserIds": nz(a.userIDs), "atMobiles": nz(a.mobiles), "isAtAll": a.all}
}

func dingMentions(ments []Mention) dingAt {
	var a dingAt
	var prefix strings.Builder
	for _, m := range ments {
		switch {
		case m.Raw == "all":
			a.all = true
			prefix.WriteString("@所有人 ") // i18n-ignore 通知正文 / 模板内容
		case m.Raw != "":
			if mobileRe.MatchString(m.Raw) {
				a.mobiles = append(a.mobiles, m.Raw)
			} else if safeIDRe.MatchString(m.Raw) {
				a.userIDs = append(a.userIDs, m.Raw)
			} else {
				continue
			}
			prefix.WriteString("@" + m.Raw + " ")
		case m.Person.DingID != "" && safeIDRe.MatchString(m.Person.DingID):
			a.userIDs = append(a.userIDs, m.Person.DingID)
			prefix.WriteString("@" + m.Person.DingID + " ")
		default:
			prefix.WriteString("@" + m.Person.name() + " ") // 没有钉钉 ID：只显示名字，不会真正提醒
		}
	}
	a.prefix = prefix.String()
	return a
}

// dingURL 返回钉钉机器人的请求地址；配置了加签密钥时带上 timestamp / sign。
func dingURL(ch Channel, now time.Time) (string, error) {
	u := ch.WebhookURL
	if ch.Secret == "" {
		return u, nil
	}
	pu, err := url.Parse(u)
	if err != nil {
		return "", errors.New("webhook 地址不合法")
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(ch.Secret))
	mac.Write([]byte(ts + "\n" + ch.Secret))
	q := pu.Query()
	q.Set("timestamp", ts)
	q.Set("sign", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	pu.RawQuery = q.Encode()
	return pu.String(), nil
}

func dingCheck(b []byte) error {
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if json.Unmarshal(b, &r) == nil && r.ErrCode != 0 {
		return fmt.Errorf("钉钉返回错误 %d：%s", r.ErrCode, r.ErrMsg)
	}
	return nil
}

func buildDingTalk(ch Channel, text string, ments []Mention, now time.Time) (*built, error) {
	at := dingMentions(ments)
	body, _ := json.Marshal(map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": at.prefix + text},
		"at":      at.field(),
	})
	u, err := dingURL(ch, now)
	if err != nil {
		return nil, err
	}
	return &built{URL: u, Body: body, Check: dingCheck}, nil
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---- 飞书 / Lark ----

// feishuMentions 是飞书文本消息里 @ 的前缀（<at user_id="…">）。
func feishuMentions(ments []Mention) string {
	var prefix strings.Builder
	at := func(id, name string) {
		prefix.WriteString(fmt.Sprintf(`<at user_id="%s">%s</at> `, id, html.EscapeString(name)))
	}
	for _, m := range ments {
		switch {
		case m.Raw == "all":
			at("all", "所有人") // i18n-ignore 通知正文 / 模板内容
		case m.Raw != "":
			if safeIDRe.MatchString(m.Raw) {
				at(m.Raw, "")
			}
		case m.Person.FeishuID != "" && safeIDRe.MatchString(m.Person.FeishuID):
			at(m.Person.FeishuID, m.Person.name())
		default:
			prefix.WriteString("@" + m.Person.name() + " ") // 没有飞书 open_id：只显示名字，不会真正提醒
		}
	}
	return prefix.String()
}

// feishuSigned 在 payload 里补上加签字段（配置了密钥时）。
func feishuSigned(ch Channel, payload map[string]any, now time.Time) {
	if ch.Secret == "" {
		return
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(ts+"\n"+ch.Secret))
	payload["timestamp"] = ts
	payload["sign"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func feishuCheck(ch Channel) func(b []byte) error {
	return func(b []byte) error {
		var r struct {
			Code       *int   `json:"code"`
			StatusCode *int   `json:"StatusCode"`
			Msg        string `json:"msg"`
			StatusMsg  string `json:"StatusMessage"`
		}
		if json.Unmarshal(b, &r) != nil {
			return nil
		}
		if r.Code != nil && *r.Code != 0 {
			return fmt.Errorf("%s返回错误 %d：%s", TypeLabel(ch.Type), *r.Code, r.Msg)
		}
		if r.StatusCode != nil && *r.StatusCode != 0 {
			return fmt.Errorf("%s返回错误 %d：%s", TypeLabel(ch.Type), *r.StatusCode, r.StatusMsg)
		}
		return nil
	}
}

func buildFeishu(ch Channel, text string, ments []Mention, now time.Time) (*built, error) {
	payload := map[string]any{"msg_type": "text", "content": map[string]string{"text": feishuMentions(ments) + text}}
	feishuSigned(ch, payload, now)
	body, _ := json.Marshal(payload)
	return &built{URL: ch.WebhookURL, Body: body, Check: feishuCheck(ch)}, nil
}

// ---- Slack ----

func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// NormalizeSlackTarget 把 Slack 里复制出来的各种 @ 写法规整成系统使用的标识：
//
//	<!subteam^S0ARY4FQATB>、!subteam^S0ARY4FQATB、<!subteam^S0ARY4FQATB|@dba> → S0ARY4FQATB（用户组）
//	<@U012AB3CD>、@U012AB3CD、<@U012AB3CD|name>                              → U012AB3CD（用户）
//	<!here>、!here、@here（channel / everyone 同）                            → here
//
// 不是这些写法的原样返回（去掉首尾空白），由后面的格式校验处理。
func NormalizeSlackTarget(v string) string {
	s := strings.TrimSpace(v)
	if len(s) >= 2 && strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">") {
		s = s[1 : len(s)-1]
	}
	if i := strings.Index(s, "|"); i >= 0 { // <!subteam^ID|@名字>：竖线后面只是显示名
		s = s[:i]
	}
	switch {
	case strings.HasPrefix(s, "!subteam^"):
		s = strings.TrimPrefix(s, "!subteam^")
	case strings.HasPrefix(s, "subteam^"):
		s = strings.TrimPrefix(s, "subteam^")
	case strings.HasPrefix(s, "!"), strings.HasPrefix(s, "@"):
		s = s[1:]
	}
	switch strings.ToLower(s) {
	case "here", "channel", "everyone":
		return strings.ToLower(s)
	}
	return s
}

// slackMentions 是 Slack 消息里 @ 的前缀。
func slackMentions(ments []Mention) string {
	var prefix strings.Builder
	for _, m := range ments {
		switch {
		case m.Raw != "":
			switch raw := NormalizeSlackTarget(m.Raw); {
			case raw == "all" || raw == "channel":
				prefix.WriteString("<!channel> ")
			case raw == "here":
				prefix.WriteString("<!here> ")
			case raw == "everyone":
				prefix.WriteString("<!everyone> ")
			case slackIDRe.MatchString(raw) && strings.HasPrefix(raw, "S"):
				prefix.WriteString("<!subteam^" + raw + "> ") // 用户组
			case slackIDRe.MatchString(raw):
				prefix.WriteString("<@" + raw + "> ") // 用户（U… / W…）
			}
		case m.Person.SlackID != "" && slackIDRe.MatchString(m.Person.SlackID):
			prefix.WriteString("<@" + m.Person.SlackID + "> ")
		default:
			prefix.WriteString("@" + slackEscape(m.Person.name()) + " ")
		}
	}
	return prefix.String()
}

func buildSlack(ch Channel, text string, ments []Mention) (*built, error) {
	body, _ := json.Marshal(map[string]string{"text": slackMentions(ments) + slackEscape(text)})
	return &built{URL: ch.WebhookURL, Body: body}, nil
}
