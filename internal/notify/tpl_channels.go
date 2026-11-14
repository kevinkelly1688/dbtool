// @author: kevinkelly
// @license: Apache Licence
// @file: tpl_channels.go
// @time: 2026/10/28

package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 本文件：把 Message 渲染成各渠道的原生载荷。
//   Slack：Block Kit（header / section / actions / divider），顶层 text 是通知栏摘要；
//   钉钉：markdown 消息（钉钉 markdown 只支持子集，SQL 用代码围栏，客户端不渲染时就是原样文字）；
//   企业微信：markdown 消息或文本消息（要 @ 手机号 / 所有人时），见 wecom.go；
//   Telegram：HTML 消息（标题加粗、字段逐行、SQL 放进 <pre>），见 telegram.go；
//   飞书 / Lark：interactive 卡片（标题栏按状态着色）；
//   邮件：HTML + 纯文本，见 emailRich。
// 所有载荷都有大小上限；超限时先逐步缩短最长的代码块 / 文本，仍超限就返回错误，由 build 改用系统默认文案。

// 各渠道请求体的大小上限（字节），留出签名、@ 等余量。
const (
	limSlack  = 38000
	limDing   = 19000
	limFeishu = 28000
)

var errTooBig = errors.New("消息超过渠道大小上限")

// buildRich 按渠道类型渲染富文本消息。
func buildRich(ch Channel, m *Message, ments []Mention, now time.Time) (*built, error) {
	m = m.clone()
	for i := 0; i < 8; i++ {
		var b *built
		var limit int
		var err error
		switch ch.Type {
		case TypeSlack:
			b, err = slackRich(ch, m, ments)
			limit = limSlack
		case TypeDingTalk:
			b, err = dingRich(ch, m, ments, now)
			limit = limDing
		case TypeWeCom:
			b, err = wecomRich(ch, m, ments)
			limit = limWeCom
		case TypeTelegram:
			b, err = telegramRich(ch, m, ments)
			limit = limTelegram
		case TypeFeishu, TypeLark:
			b, err = feishuRich(ch, m, ments, now)
			limit = limFeishu
		default:
			return nil, fmt.Errorf("不支持的渠道类型 %s", strconv.Quote(ch.Type))
		}
		if err != nil {
			return nil, err
		}
		if len(b.Body) <= limit {
			return b, nil
		}
		if !m.shrink() {
			break
		}
	}
	return nil, errTooBig
}

// ---- Slack ----

func slackURL(u string) string {
	return strings.NewReplacer("<", "%3C", ">", "%3E", "|", "%7C", " ", "%20").Replace(u)
}

func slackLabel(s string) string {
	return strings.NewReplacer("|", "¦").Replace(slackEscape(s))
}

// slackCode 把文字放进代码围栏；文字里的 ``` 打断，避免提前结束围栏。
func slackCode(s string) string {
	return "```\n" + strings.ReplaceAll(slackEscape(s), "```", "`​``") + "\n```"
}

const (
	slackPairSep = "　　" // 两个“占半行”字段并排时的间隔：全角空格不会被 Slack 合并掉
	slackPairMax = 60   // 并排的两行文字（含 *名称:* 标记）合起来最多这么多字符，再长就各占一行
)

// slackRich 把消息排成 Slack Block Kit。Slack 里每个块之间都有固定的上下留白，块拆得越碎行间距越大，
// 所以把提醒 @、标题、字段、文本、代码块都排进同一个 section 的 mrkdwn 文字里（一段放不下约 2800 字才另起一段），
// 只有按钮和分隔线才单独成块。标题用加粗的一行，不用 header 块（header 块自带很大的上下留白）。
//
// 版式：
//   - 第一行是“@某人 加粗标题”（@ 与标题在同一行，如 “@dba_user 📋 有新工单需要处理”）；
//   - 字段表里勾选“占半行”的字段，连续的每两个并排放在同一行（“*名称:* 值　　*名称:* 值”，中间隔两个全角空格），
//     没勾选的整行显示“*名称:* 值”。并排不用 Slack 的 fields 栅格：fields 要单独成块，上下各多一道很大的留白；
//     两个字段的文字合起来太长、或值里有换行时不并排，各占一行。
func slackRich(ch Channel, m *Message, ments []Mention) (*built, error) {
	var blocks []map[string]any
	var buf []string
	size := 0
	section := func(text string) map[string]any {
		return map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": cutRunes(text, 3000)}}
	}
	flush := func() {
		if len(buf) > 0 {
			blocks = append(blocks, section(strings.Join(buf, "\n")))
			buf, size = nil, 0
		}
	}
	put := func(text string) {
		n := utf8.RuneCountInString(text) + 1
		if size > 0 && size+n > 2800 {
			flush()
		}
		buf = append(buf, text)
		size += n
	}
	mt := strings.TrimSpace(slackMentions(ments))
	head := "*" + slackEscape(cutRunes(m.Head(), 150)) + "*"
	if mt != "" {
		put(mt + " " + head) // @ 与标题同一行
	} else {
		put(head)
	}
	// 连续的“占半行”字段先攒着，遇到整行字段 / 别的块时排成一个两列的 fields 段（每段最多 10 个）
	rowLine := func(r Row) string { // 整行：“*名称:* 值”；多行的值：名称单独一行，值在下面
		if strings.Contains(r.V, "\n") {
			return cutRunes("*"+slackEscape(r.L)+":*\n"+slackEscape(r.V), 1500)
		}
		return cutRunes("*"+slackEscape(r.L)+":* "+slackEscape(r.V), 1500)
	}
	var half []Row
	flushHalf := func() {
		for i := 0; i < len(half); i += 2 {
			a := rowLine(half[i])
			if i+1 >= len(half) { // 落单的一个：没有搭档，按整行显示
				put(a)
				break
			}
			b := rowLine(half[i+1])
			if strings.Contains(half[i].V, "\n") || strings.Contains(half[i+1].V, "\n") || utf8.RuneCountInString(a)+utf8.RuneCountInString(b) > slackPairMax {
				put(a) // 太长或多行：并排会折行错位，各占一行
				put(b)
				continue
			}
			put(a + slackPairSep + b)
		}
		half = nil
	}
	for _, b := range m.Blocks {
		if b.T != BlockFields {
			flushHalf()
		}
		switch b.T {
		case BlockFields:
			for _, r := range b.Rows {
				if r.Short {
					half = append(half, r)
					continue
				}
				flushHalf()
				put(rowLine(r))
			}
			flushHalf()
		case BlockText:
			put(slackEscape(b.V))
		case BlockCode:
			head := ""
			if b.L != "" {
				head = "*" + slackEscape(b.L) + ":*\n"
			}
			code := b.V
			if utf8.RuneCountInString(code) > 2600 {
				code = string([]rune(code)[:2600]) + "\n…"
			}
			put(head + slackCode(code))
		case BlockLink:
			if b.As == "button" {
				flush()
				blocks = append(blocks, map[string]any{"type": "actions", "elements": []any{
					map[string]any{"type": "button", "text": map[string]any{"type": "plain_text", "text": cutRunes(b.L, 75)}, "url": slackURL(b.V)}}})
			} else {
				put("<" + slackURL(b.V) + "|" + slackLabel(b.L) + ">")
			}
		case BlockDivider:
			flush()
			blocks = append(blocks, map[string]any{"type": "divider"})
		}
	}
	flushHalf()
	flush()
	if len(blocks) > 50 {
		blocks = append(blocks[:49], section("…（内容过多，已省略后面的部分）")) // i18n-ignore 通知正文 / 模板内容
	}
	fallback := strings.TrimSpace(mt + " " + slackEscape(m.Head()))
	body, err := json.Marshal(map[string]any{"text": fallback, "blocks": blocks})
	if err != nil {
		return nil, err
	}
	return &built{URL: ch.WebhookURL, Body: body}, nil
}

// ---- 钉钉 ----

// dingEsc 转义 markdown 里会改变排版的字符：反斜杠、星号、反引号，以及行首的 # > - 数字.（标题 / 引用 / 列表）。
func dingEsc(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `*`, `\*`, "`", "\\`").Replace(s)
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		t := strings.TrimLeft(ln, " \t")
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, ">") || strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "+ ") || startsOrdered(t) {
			lines[i] = `\` + t
		}
	}
	return strings.Join(lines, "  \n")
}

func startsOrdered(s string) bool {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i > 0 && i < len(s) && s[i] == '.'
}

func mdURL(u string) string {
	return strings.NewReplacer(" ", "%20", ")", "%29", "(", "%28").Replace(u)
}

func dingRich(ch Channel, m *Message, ments []Mention, now time.Time) (*built, error) {
	at := dingMentions(ments)
	var md []string
	if p := strings.TrimSpace(at.prefix); p != "" {
		md = append(md, p, "")
	}
	md = append(md, "### "+dingEsc(m.Head()), "")
	for _, b := range m.Blocks {
		switch b.T {
		case BlockFields:
			for _, r := range b.Rows {
				md = append(md, "- **"+dingEsc(r.L)+"**："+dingEsc(r.V))
			}
			md = append(md, "")
		case BlockText:
			md = append(md, dingEsc(b.V), "")
		case BlockCode:
			if b.L != "" {
				md = append(md, "**"+dingEsc(b.L)+"**：")
			}
			md = append(md, "```", strings.ReplaceAll(b.V, "```", "'''"), "```", "")
		case BlockLink:
			md = append(md, "["+strings.NewReplacer("[", "(", "]", ")").Replace(b.L)+"]("+mdURL(b.V)+")", "")
		case BlockDivider:
			md = append(md, "---", "")
		}
	}
	title := cutRunes(oneLine(m.Head()), 100)
	body, err := json.Marshal(map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]string{"title": title, "text": strings.TrimRight(strings.Join(md, "\n"), "\n")},
		"at":       at.field(),
	})
	if err != nil {
		return nil, err
	}
	u, err := dingURL(ch, now)
	if err != nil {
		return nil, err
	}
	return &built{URL: u, Body: body, Check: dingCheck}, nil
}

// ---- 飞书 / Lark ----

var feishuTone = map[string]string{ToneInfo: "blue", ToneOK: "green", ToneWarn: "orange", ToneCrit: "red"}

// larkEsc 防止消息内容里的 “<at id=all>” 之类标签被当成真正的 @：把 < 换成全角。
func larkEsc(s string) string { return strings.ReplaceAll(s, "<", "＜") }

// feishuCardMentions 是卡片里 @ 的写法（<at id=…></at>）。
func feishuCardMentions(ments []Mention) string {
	var b strings.Builder
	for _, m := range ments {
		switch {
		case m.Raw == "all":
			b.WriteString("<at id=all></at> ")
		case m.Raw != "":
			if safeIDRe.MatchString(m.Raw) {
				b.WriteString("<at id=" + m.Raw + "></at> ")
			}
		case m.Person.FeishuID != "" && safeIDRe.MatchString(m.Person.FeishuID):
			b.WriteString("<at id=" + m.Person.FeishuID + "></at> ")
		default:
			b.WriteString("@" + larkEsc(m.Person.name()) + " ") // 没有 open_id：只显示名字，不会真正提醒
		}
	}
	return strings.TrimSpace(b.String())
}

func feishuRich(ch Channel, m *Message, ments []Mention, now time.Time) (*built, error) {
	md := func(s string) map[string]any { return map[string]any{"tag": "lark_md", "content": s} }
	div := func(s string) map[string]any { return map[string]any{"tag": "div", "text": md(s)} }
	var el []any
	if mt := feishuCardMentions(ments); mt != "" {
		el = append(el, div(mt))
	}
	for _, b := range m.Blocks {
		switch b.T {
		case BlockFields:
			fs := make([]any, 0, len(b.Rows))
			for _, r := range b.Rows {
				fs = append(fs, map[string]any{"is_short": r.Short, "text": md("**" + larkEsc(r.L) + "**\n" + larkEsc(r.V))})
			}
			el = append(el, map[string]any{"tag": "div", "fields": fs})
		case BlockText:
			el = append(el, div(larkEsc(b.V)))
		case BlockCode:
			head := ""
			if b.L != "" {
				head = "**" + larkEsc(b.L) + "**\n"
			}
			el = append(el, div(head+"```\n"+strings.ReplaceAll(larkEsc(b.V), "```", "'''")+"\n```"))
		case BlockLink:
			if b.As == "button" {
				el = append(el, map[string]any{"tag": "action", "actions": []any{
					map[string]any{"tag": "button", "text": map[string]any{"tag": "plain_text", "content": cutRunes(b.L, 60)}, "url": b.V, "type": "default"}}})
			} else {
				el = append(el, div("["+strings.NewReplacer("[", "(", "]", ")").Replace(larkEsc(b.L))+"]("+mdURL(b.V)+")"))
			}
		case BlockDivider:
			el = append(el, map[string]any{"tag": "hr"})
		}
	}
	if len(el) == 0 {
		el = append(el, div(larkEsc(m.Title)))
	}
	color := feishuTone[m.Tone]
	if color == "" {
		color = "blue"
	}
	payload := map[string]any{"msg_type": "interactive", "card": map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   map[string]any{"template": color, "title": map[string]any{"tag": "plain_text", "content": cutRunes(m.Head(), 100)}},
		"elements": el,
	}}
	feishuSigned(ch, payload, now)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &built{URL: ch.WebhookURL, Body: body, Check: feishuCheck(ch)}, nil
}

// ---- 邮件 ----

var emailTone = map[string]string{ToneInfo: "#2f6fed", ToneOK: "#1f9d55", ToneWarn: "#d97706", ToneCrit: "#d64545"}

// emailRich 把消息渲染成邮件的纯文本与 HTML 正文。没有配置对外地址（PUBLIC_URL）时不放任何链接，与原来的邮件一致。
func emailRich(m *Message, chName string) (text, htmlBody string) {
	base := linkBase()
	foot := "事件：" + oneLine(m.eventLabel()) + " · 渠道：" + chName // i18n-ignore 通知正文 / 模板内容
	if base != "" {
		foot += " · 系统地址：" + base // i18n-ignore 通知正文 / 模板内容
	}
	mm := m.clone()
	if base == "" {
		mm = mm.withoutLinks()
	}
	var t strings.Builder
	t.WriteString(mm.Text())
	t.WriteString("\n-- \n" + foot + "\n本邮件由 dbtool 自动发送，请勿直接回复。\n") // i18n-ignore 通知正文 / 模板内容
	text = t.String()

	color := emailTone[mm.Tone]
	if color == "" {
		color = emailTone[ToneInfo]
	}
	var h strings.Builder
	h.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8"></head><body style="margin:0;padding:16px;background:#f5f6f8;font-family:-apple-system,'Segoe UI','PingFang SC','Microsoft YaHei',sans-serif;color:#1f2933">`)
	h.WriteString(`<div style="max-width:720px;margin:0 auto;background:#ffffff;border:1px solid #dfe3e8;border-top:4px solid ` + color + `;border-radius:6px;padding:20px">`)
	h.WriteString(`<h2 style="margin:0 0 14px;font-size:17px;line-height:1.4">` + html.EscapeString(mm.Head()) + `</h2>`)
	cell := func(s string) string {
		return htmlLinkify(s, base)
	}
	for _, b := range mm.Blocks {
		switch b.T {
		case BlockFields:
			h.WriteString(`<table role="presentation" style="border-collapse:collapse;font-size:14px;line-height:1.6;width:100%">`)
			for _, r := range b.Rows {
				h.WriteString(`<tr><td style="padding:4px 12px 4px 0;color:#52606d;white-space:nowrap;vertical-align:top">` + html.EscapeString(r.L) +
					`</td><td style="padding:4px 0;white-space:pre-wrap;word-break:break-word">` + cell(r.V) + `</td></tr>`)
			}
			h.WriteString(`</table>`)
		case BlockText:
			h.WriteString(`<p style="margin:10px 0;font-size:14px;line-height:1.6;white-space:pre-wrap;word-break:break-word">` + cell(b.V) + `</p>`)
		case BlockCode:
			if b.L != "" {
				h.WriteString(`<div style="margin:12px 0 4px;color:#7b8794;font-size:12px">` + html.EscapeString(b.L) + `</div>`)
			}
			h.WriteString(`<pre style="margin:0 0 8px;padding:10px 12px;background:#f5f7fa;border:1px solid #e4e7eb;border-radius:4px;font-size:13px;line-height:1.5;white-space:pre-wrap;word-break:break-word;font-family:Consolas,'Courier New',monospace">` + html.EscapeString(b.V) + `</pre>`)
		case BlockLink:
			e := html.EscapeString(b.V)
			label := html.EscapeString(b.L)
			clickable := base != "" && strings.HasPrefix(b.V, base+"/")
			switch {
			case clickable && b.As == "button":
				h.WriteString(`<p style="margin:14px 0"><a href="` + e + `" style="display:inline-block;padding:8px 16px;background:` + color + `;color:#ffffff;text-decoration:none;border-radius:4px;font-size:14px">` + label + `</a></p>`)
			case clickable:
				h.WriteString(`<p style="margin:8px 0;font-size:14px">` + label + `：<a href="` + e + `">` + e + `</a></p>`)
			default:
				h.WriteString(`<p style="margin:8px 0;font-size:14px">` + label + `：` + e + `</p>`)
			}
		case BlockDivider:
			h.WriteString(`<hr style="border:0;border-top:1px solid #e4e7eb;margin:14px 0">`)
		}
	}
	h.WriteString(`<p style="margin:18px 0 0;padding-top:10px;border-top:1px solid #e4e7eb;color:#7b8794;font-size:12px">` + html.EscapeString(foot) + `<br>本邮件由 dbtool 自动发送，请勿直接回复。</p>`) // i18n-ignore 通知正文 / 模板内容
	h.WriteString(`</div></body></html>`)
	return text, h.String()
}

// withoutLinks 去掉没有对外地址时不该出现的链接：链接块整块去掉；字段 / 文本里的 URL 抹掉，抹空的行去掉。
func (m *Message) withoutLinks() *Message {
	c := &Message{Key: m.Key, Scope: m.Scope, Title: urlRe.ReplaceAllString(m.Title, ""), Icon: m.Icon, Tone: m.Tone}
	for _, b := range m.Blocks {
		switch b.T {
		case BlockLink:
			continue
		case BlockFields:
			var rows []Row
			for _, r := range b.Rows {
				if v := strings.TrimSpace(urlRe.ReplaceAllString(r.V, "")); v != "" {
					rows = append(rows, Row{L: r.L, V: v, Short: r.Short})
				}
			}
			if len(rows) == 0 {
				continue
			}
			b.Rows = rows
		case BlockText:
			b.V = strings.TrimSpace(urlRe.ReplaceAllString(b.V, ""))
			if b.V == "" {
				continue
			}
		}
		c.Blocks = append(c.Blocks, b)
	}
	return c
}

// eventLabel 是页脚里的事件名：目录里这条消息的显示名，找不到用键。
func (m *Message) eventLabel() string {
	if d := MessageByKey(m.Key); d != nil {
		return d.Name
	}
	return m.Key
}

// emailRichSubject 取邮件主题的标题部分（不含图标，邮件主题里放 emoji 在部分客户端会乱码）。
func emailRichSubject(m *Message) string { return m.Title }
