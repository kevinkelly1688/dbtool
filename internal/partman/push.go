// @author: kevinkelly
// @license: Apache Licence
// @file: push.go
// @time: 2026/10/28

package partman

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/settings"
)

// webhook 推送：移植自外部程序 notify.go（slack / wecom / feishu / dingtalk 四类通道，加签、自定义关键词、@人、
// 各自的返回码判定、地址脱敏）。读 dbt_partman_webhook 里 isenabled=1 的行；列语义以原程序为准：
// secret = 加签密钥（飞书 / 钉钉），keyword = 自定义关键词（会拼进消息）。
//
// 推送失败绝不影响维护结果：错误只写进本次维护的运行输出与服务日志（地址里的 token 脱敏）。

// 通道类型。
const (
	chSlack    = "slack"
	chWecom    = "wecom"
	chFeishu   = "feishu"
	chDingtalk = "dingtalk"
)

var channelTypes = []string{chSlack, chWecom, chFeishu, chDingtalk}

// 全局推送时机（partition.notify_on）。
const (
	cfgNotifyOn = settings.KeyPartitionNotifyOn

	notifyAlways    = "always"
	notifyOnFailure = "on_failure"
	notifyNever     = "never"
)

var notifyOnValues = []string{notifyAlways, notifyOnFailure, notifyNever}

// notifyOn 读取全局推送时机；缺省或取值非法按 always（与原程序一致）。
func (s *Service) notifyOn() string {
	v, ok := s.meta.ConfigValue(cfgNotifyOn)
	if !ok {
		return notifyAlways
	}
	v = strings.ToLower(strings.TrimSpace(v))
	if contains(notifyOnValues, v) {
		return v
	}
	if v != "" {
		slog.Warn("partition.notify_on 取值非法，按 always 处理", "value", v)
	}
	return notifyAlways
}

// pushChannel 是一个可发送的通道（来自 dbt_partman_webhook 的一行）。
type pushChannel struct {
	ID         uint
	Type       string
	WebhookURL string
	Secret     string
	Keyword    string
	MentionOn  string
	Mention    []string
}

func normChannelType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	switch t {
	case "wechat", "wxwork", "qywx":
		return chWecom
	case "lark":
		return chFeishu
	}
	return t
}

func normMentionOn(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return notifyOnFailure
	}
	return v
}

// parseMentionList 支持 JSON 数组 ["a","b"] 或逗号分隔 a,b。
func parseMentionList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "[") {
		var arr []string
		if json.Unmarshal([]byte(s), &arr) == nil {
			return arr
		}
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func channelOf(w Webhook) pushChannel {
	return pushChannel{ID: w.ID, Type: normChannelType(w.Type), WebhookURL: strings.TrimSpace(w.URL), Secret: w.Secret, Keyword: w.Keyword,
		MentionOn: normMentionOn(w.MentionOn), Mention: parseMentionList(w.Mention)}
}

// ---- 地址：允许的主机（与 internal/server 的渠道地址校验一致：平台域名 + https，本机回环仅为联调；企业微信是本通道新增的平台） ----

var platformHosts = map[string][]string{
	chSlack:    {"hooks.slack.com"},
	chWecom:    {"qyapi.weixin.qq.com"},
	chFeishu:   {"open.feishu.cn", "open.larksuite.com"},
	chDingtalk: {"oapi.dingtalk.com"},
}

// checkWebhookURL 校验地址：http(s) 完整地址；主机必须是该平台的域名（https），或本机回环（http / https，仅联调）。
func checkWebhookURL(typ, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return badReq("webhook 地址必须是 http:// 或 https:// 开头的完整地址")
	}
	h := strings.ToLower(u.Hostname())
	for _, want := range platformHosts[typ] {
		if h == want {
			if u.Scheme != "https" {
				return newErr(400, "webhook_host_not_allowed", "%s 的 webhook 地址必须使用 https", want)
			}
			return nil
		}
	}
	if h == "localhost" || h == "127.0.0.1" || h == "::1" {
		return nil
	}
	return newErr(400, "webhook_host_not_allowed", "webhook 地址的主机 %s 不在 %s 通道允许的范围内（允许：%s）", u.Hostname(), typ, strings.Join(platformHosts[typ], "、"))
}

// maskWebhook 脱敏地址里的密钥段（原程序的规则），其余情形只保留协议与主机。
func maskWebhook(u string) string {
	for _, sep := range []string{"key=", "access_token=", "/hook/", "/services/"} {
		if i := strings.Index(u, sep); i >= 0 {
			return u[:i+len(sep)] + "****"
		}
	}
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		if p.Path != "" && p.Path != "/" || p.RawQuery != "" {
			return p.Scheme + "://" + p.Host + "/****"
		}
		return p.Scheme + "://" + p.Host
	}
	return "****"
}

// scrub 把错误文本里可能出现的完整地址 / 密钥去掉。
func scrub(msg string, ch pushChannel) string {
	if ch.WebhookURL != "" {
		msg = strings.ReplaceAll(msg, ch.WebhookURL, maskWebhook(ch.WebhookURL))
		if p, err := url.Parse(ch.WebhookURL); err == nil {
			for _, vs := range p.Query() {
				for _, v := range vs {
					if len(v) >= 6 {
						msg = strings.ReplaceAll(msg, v, "****")
					}
				}
			}
			if i := strings.LastIndex(p.Path, "/"); i >= 0 && len(p.Path)-i > 6 {
				msg = strings.ReplaceAll(msg, p.Path[i+1:], "****")
			}
		}
	}
	if len(ch.Secret) >= 4 {
		msg = strings.ReplaceAll(msg, ch.Secret, "****")
	}
	return msg
}

// ---- 报文 ----

func mentionApplies(on string, failed bool) bool {
	switch on {
	case "always":
		return true
	case "on_failure":
		return failed
	}
	return false
}

func isMobile(s string) bool {
	if len(s) != 11 || s[0] != '1' {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func feishuSign(ts, secret string) string {
	mac := hmac.New(sha256.New, []byte(ts+"\n"+secret))
	_, _ = mac.Write([]byte{})
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func dingtalkSign(ms int64, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d\n%s", ms, secret)))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// buildPayload 按通道类型生成 (目标 URL, 报文)。now 用于加签的时间戳。
func buildPayload(ch pushChannel, header, body string, failed bool, now time.Time) (string, []byte, error) {
	doMention := mentionApplies(ch.MentionOn, failed)
	switch ch.Type {
	case chSlack:
		text := header + "\n```\n" + body + "```"
		if doMention && len(ch.Mention) > 0 {
			text = strings.Join(ch.Mention, " ") + "  " + text
		}
		p, _ := json.Marshal(map[string]string{"text": text})
		return ch.WebhookURL, p, nil

	case chWecom: // 企业微信群机器人
		content := header + "\n" + body
		if ch.Keyword != "" {
			content = ch.Keyword + " " + content
		}
		text := map[string]any{"content": content}
		if doMention {
			var ids, mobiles []string
			for _, m := range ch.Mention {
				if isMobile(m) {
					mobiles = append(mobiles, m)
				} else {
					ids = append(ids, m) // userid 或 @all
				}
			}
			if len(ids) > 0 {
				text["mentioned_list"] = ids
			}
			if len(mobiles) > 0 {
				text["mentioned_mobile_list"] = mobiles
			}
		}
		p, _ := json.Marshal(map[string]any{"msgtype": "text", "text": text})
		return ch.WebhookURL, p, nil

	case chFeishu: // 飞书自定义机器人
		prefix := ""
		if doMention {
			var ats []string
			for _, m := range ch.Mention {
				if m == "all" || m == "@all" {
					ats = append(ats, `<at user_id="all">所有人</at>`) // i18n-ignore：飞书 @所有人 的消息体
				} else {
					ats = append(ats, fmt.Sprintf(`<at user_id=%q></at>`, m))
				}
			}
			if len(ats) > 0 {
				prefix = strings.Join(ats, " ") + " "
			}
		}
		text := prefix + header + "\n" + body
		if ch.Keyword != "" {
			text = ch.Keyword + " " + text
		}
		msg := map[string]any{"msg_type": "text", "content": map[string]string{"text": text}}
		if ch.Secret != "" { // 开启了签名校验
			ts := strconv.FormatInt(now.Unix(), 10)
			msg["timestamp"] = ts
			msg["sign"] = feishuSign(ts, ch.Secret)
		}
		p, _ := json.Marshal(msg)
		return ch.WebhookURL, p, nil

	case chDingtalk: // 钉钉自定义机器人
		u := ch.WebhookURL
		if ch.Secret != "" { // 加签
			ms := now.UnixMilli()
			sep := "&"
			if !strings.Contains(u, "?") {
				sep = "?"
			}
			u += fmt.Sprintf("%stimestamp=%d&sign=%s", sep, ms, url.QueryEscape(dingtalkSign(ms, ch.Secret)))
		}
		prefix := ""
		at := map[string]any{"isAtAll": false}
		if doMention {
			var mobiles, ids []string
			for _, m := range ch.Mention {
				switch {
				case m == "all" || m == "@all":
					at["isAtAll"] = true
				case isMobile(m):
					mobiles = append(mobiles, m)
					prefix += "@" + m + " " // 文本里也带上 @手机号才会高亮
				default:
					ids = append(ids, m)
				}
			}
			if len(mobiles) > 0 {
				at["atMobiles"] = mobiles
			}
			if len(ids) > 0 {
				at["atUserIds"] = ids
			}
		}
		content := prefix + header + "\n" + body
		if ch.Keyword != "" {
			content = ch.Keyword + " " + content
		}
		p, _ := json.Marshal(map[string]any{"msgtype": "text", "text": map[string]string{"content": content}, "at": at})
		return u, p, nil
	}
	return "", nil, fmt.Errorf("未知通道类型: %s", ch.Type)
}

func jsonIntFieldOpt(body, key string) (bool, int) {
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) != nil {
		return false, -1
	}
	if raw, ok := m[key]; ok {
		if f, ok := raw.(float64); ok {
			return true, int(f)
		}
	}
	return false, -1
}

// checkResp 按平台判断推送是否成功。
func checkResp(typ string, status int, body string) bool {
	if status != http.StatusOK {
		return false
	}
	switch typ {
	case chSlack:
		return strings.TrimSpace(body) == "ok"
	case chWecom, chDingtalk:
		_, v := jsonIntFieldOpt(body, "errcode")
		return v == 0
	case chFeishu:
		// 新版 {"code":0}，旧版 {"StatusCode":0}
		if has, v := jsonIntFieldOpt(body, "code"); has {
			return v == 0
		}
		if has, v := jsonIntFieldOpt(body, "StatusCode"); has {
			return v == 0
		}
		return false
	}
	return false
}

// ---- 发送 ----

// PushResult 是一个通道的发送结果。
type PushResult struct {
	ID         uint   `json:"id"`
	Type       string `json:"type"`
	WebhookURL string `json:"webhook_url"` // 脱敏
	OK         bool   `json:"ok"`
	Status     int    `json:"status"`
	Message    string `json:"message"`
}

const pushTimeout = 10 * time.Second

func (s *Service) pushHTTP() *http.Client {
	if s.pushClient != nil {
		return s.pushClient
	}
	return &http.Client{Timeout: pushTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// sendOne 向一个通道发送。任何错误都收敛成 PushResult，不 panic、不返回 error。
func (s *Service) sendOne(ctx context.Context, ch pushChannel, header, body string, failed bool) (res PushResult) {
	res = PushResult{ID: ch.ID, Type: ch.Type, WebhookURL: maskWebhook(ch.WebhookURL)}
	defer func() {
		if r := recover(); r != nil {
			res.OK, res.Message = false, "发送时发生内部错误"
			slog.Error("分区维护推送 panic", "channel", ch.Type, "recover", r)
		}
	}()
	if !contains(channelTypes, ch.Type) {
		res.Message = fmt.Sprintf("未知通道类型：%s", ch.Type)
		return
	}
	if err := checkWebhookURL(ch.Type, ch.WebhookURL); err != nil {
		res.Message = fmt.Sprintf("地址不合法，未发送：%s", err.Error())
		return
	}
	target, payload, err := buildPayload(ch, header, body, failed, s.now())
	if err != nil {
		res.Message = err.Error()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		res.Message = "构造请求失败"
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.pushHTTP().Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		res.Message = fmt.Sprintf("请求失败：%s", clipMsg(scrub(err.Error(), ch), 200))
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	text := strings.TrimSpace(string(b))
	res.Status = resp.StatusCode
	res.OK = checkResp(ch.Type, resp.StatusCode, text)
	if res.OK {
		res.Message = "发送成功"
	} else {
		res.Message = fmt.Sprintf("平台返回失败（HTTP %d）：%s", resp.StatusCode, clipMsg(scrub(text, ch), 200))
	}
	return
}

// sendAll 并发向这些通道发送，结果与 chs 同序。
func (s *Service) sendAll(ctx context.Context, chs []pushChannel, header, body string, failed bool) []PushResult {
	out := make([]PushResult, len(chs))
	var wg sync.WaitGroup
	for i := range chs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = s.sendOne(ctx, chs[i], header, body, failed)
		}(i)
	}
	wg.Wait()
	return out
}

// pushRecord 是一条语句的结果（推送正文用）。
type pushRecord struct {
	SQL    string
	Result string // success / failed:<原因>
}

// maintEvent 是一次维护的结果，供推送使用。
type maintEvent struct {
	Env, DB, Table string
	Records        []pushRecord
	Skipped        int
	Err            error // 计划 / 连接失败
}

// buildBody 生成消息的头部和正文（状态 + sql_text 行，无代码块围栏），措辞沿用原程序。
func buildBody(ev maintEvent, now time.Time) (header, body string, okN, failN int, failed bool) {
	for _, r := range ev.Records {
		if r.Result == "success" {
			okN++
		} else {
			failN++
		}
	}
	failed = ev.Err != nil || failN > 0
	icon := "✅ 成功" // i18n-ignore：推送消息正文，语言属于接收方
	if failed {
		icon = "❌ 失败" // i18n-ignore：推送消息正文，语言属于接收方
	}
	header = fmt.Sprintf("%s / %s.%s 分区维护  %s", ev.Env, ev.DB, ev.Table, now.Format("2006-01-02 15:04:05")) // i18n-ignore：推送消息正文，语言属于接收方
	var b strings.Builder
	b.WriteString(fmt.Sprintf("状态：%s  （执行 成功%d / 失败%d）\n", icon, okN, failN)) // i18n-ignore：推送消息正文，语言属于接收方
	if ev.Err != nil {
		b.WriteString(fmt.Sprintf("\"error\":%q\n", ev.Err.Error()))
	}
	for _, r := range ev.Records {
		b.WriteString(fmt.Sprintf("\"sql_text\":%q\n", clipMsg(r.SQL, 500)))
		if r.Result != "success" {
			b.WriteString(fmt.Sprintf("\"exec_result\":%q\n", clipMsg(r.Result, 500)))
		}
	}
	if ev.Skipped > 0 {
		b.WriteString(fmt.Sprintf("（另有 %d 条语句因前面失败未执行）\n", ev.Skipped)) // i18n-ignore：推送消息正文，语言属于接收方
	}
	return header, b.String(), okN, failN, failed
}

// pushMaintenance 在一次维护之后推送并返回要并入运行输出的行。没有变更且没有失败不推；失败只记输出与服务日志。
func (s *Service) pushMaintenance(ctx context.Context, ev maintEvent) []traceLine {
	tr := newTracer(s.now)
	if len(ev.Records) == 0 && ev.Err == nil {
		return nil
	}
	header, body, _, _, failed := buildBody(ev, s.now())
	switch on := s.notifyOn(); {
	case on == notifyNever:
		tr.add("INFO", "推送时机 %s：不推送", on)
		return tr.lines
	case on == notifyOnFailure && !failed:
		tr.add("INFO", "推送时机 %s：本次没有失败，不推送", on)
		return tr.lines
	}
	hooks, err := s.repo.ListWebhooks(true)
	if err != nil {
		slog.Warn("读取分区维护推送配置失败（已忽略）", "err", err)
		tr.add("WARN", "读取推送配置失败，未推送：%v", err)
		return tr.lines
	}
	if len(hooks) == 0 {
		tr.add("INFO", "没有启用的推送通道")
		return tr.lines
	}
	chs := make([]pushChannel, 0, len(hooks))
	for _, h := range hooks {
		chs = append(chs, channelOf(h))
	}
	for i, r := range s.sendAll(ctx, chs, header, body, failed) {
		ch := chs[i]
		if r.OK {
			tr.add("INFO", "推送 %s %s 成功（HTTP %d）", ch.Type, r.WebhookURL, r.Status)
		} else {
			tr.add("WARN", "推送 %s %s 失败：%s", ch.Type, r.WebhookURL, r.Message)
			slog.Warn("分区维护推送失败", "channel", ch.Type, "webhook", r.WebhookURL, "status", r.Status, "msg", r.Message)
		}
	}
	return tr.lines
}
