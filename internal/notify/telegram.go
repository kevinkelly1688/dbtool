// @author: kevinkelly
// @license: Apache Licence
// @file: telegram.go
// @time: 2026/10/28

package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// 本文件：Telegram 机器人渠道（type = telegram）。
//
// Webhook 地址填 Bot API 的 sendMessage 地址：https://api.telegram.org/bot<Bot Token>/sendMessage?chat_id=<Chat ID>
// （Bot Token 向 @BotFather 申请；Chat ID 是群 / 频道的数字 ID，如 -1001234567890，公开频道也可以写 @频道名；
// 群里开了话题时再加 &message_thread_id=<话题 ID>）。地址里的 Bot Token 就是密钥，和其它机器人的 Webhook 一样加密存放、不回显，
// 没有单独的签名密钥。发送时 chat_id 等参数放进 JSON 请求体，请求地址只用 /bot<Bot Token>/sendMessage，地址里的其它参数忽略。
//
// 消息用 HTML 格式（parse_mode = HTML，正文里只有 < > & 要转义）；一条消息最长 4096 个字符（按解析后的可见文字算，UTF-16 单位）。
// 成功返回 {"ok":true,…}；失败是 4xx 加 {"ok":false,"error_code":…,"description":"…"}（如 401 Unauthorized：Bot Token 不对，
// 400 chat not found：Chat ID 不对或机器人不在群里，429：发得太快，同一个群每分钟最多 20 条）。
//
// @：Telegram 用户名（@dba_wang）直接写在正文里；数字用户 ID 写成 <a href="tg://user?id=…">。Telegram 没有“@所有人”，
// 所以不支持“所有人”规则（保存时拒绝）。用户资料里没有 Telegram 用户名 / ID，按平台用户 @ 时只在正文里写“@显示名”，不会真正提醒。

const (
	tgTextMax = 4096 // 一条消息的可见文字上限（UTF-16 单位）

	// limTelegram 是整个请求体的大小兜底（字节）。Telegram 按解析后的可见文字限制，由 telegramRich 自己判断，这里只防止异常放大。
	limTelegram = 64 << 10
)

var (
	tgPathRe   = regexp.MustCompile(`^/bot[0-9]{3,20}:[A-Za-z0-9_-]{20,64}/sendMessage$`)
	tgUserRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{4,31}$`)
	tgUserIDRe = regexp.MustCompile(`^[0-9]{1,16}$`)
	tgChatRe   = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z][A-Za-z0-9_]{4,31})$`)
	tgThreadRe = regexp.MustCompile(`^[0-9]{1,10}$`)
)

// TelegramUserNote 是预览里对“平台用户不能真正 @”的说明（Telegram 渠道专用）。
const TelegramUserNote = "Telegram 渠道不能按平台用户 @（用户资料里没有 Telegram 用户名 / ID），退化为文本“@显示名”，不会真正提醒；要提醒到人，请改用“渠道原生标识”（Telegram 用户名或数字用户 ID）"

// TelegramAddrHint 是 Webhook 地址的正确写法（保存时地址不对的提示、页面的说明里用）。
const TelegramAddrHint = "https://api.telegram.org/bot<Bot Token>/sendMessage?chat_id=<Chat ID>"

// NormalizeTelegramTarget 把写在“渠道原生标识”里的 @用户名 规整成不带 @ 的用户名；其它写法原样返回（去掉首尾空白）。
func NormalizeTelegramTarget(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "@") }

// TelegramRawOK 判断“渠道原生标识”是不是 Telegram 用户名（5–32 位字母、数字、下划线，字母开头）或数字用户 ID。
func TelegramRawOK(v string) bool { return tgUserRe.MatchString(v) || tgUserIDRe.MatchString(v) }

// tgTarget 是从 Webhook 地址里解析出的发送目标。
type tgTarget struct {
	api    string // 请求地址：scheme://host/bot<Bot Token>/sendMessage（不带查询参数）
	chatID any    // 数字（int64）或 @频道名
	thread int64  // 话题 ID；0 = 没有
}

func parseTelegramURL(raw string) (tgTarget, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return tgTarget{}, errors.New("webhook 地址不合法")
	}
	if !tgPathRe.MatchString(u.Path) {
		return tgTarget{}, fmt.Errorf("Telegram 机器人的 Webhook 地址应是 %s 的形式（Bot Token 向 @BotFather 申请）", TelegramAddrHint)
	}
	q := u.Query()
	chat := strings.TrimSpace(q.Get("chat_id"))
	if chat == "" {
		return tgTarget{}, errors.New("Webhook 地址里缺少 chat_id（群 / 频道的 Chat ID，如 chat_id=-1001234567890；公开频道可写 chat_id=@频道名）")
	}
	if !tgChatRe.MatchString(chat) {
		return tgTarget{}, errors.New("chat_id 不合法：应是数字 ID（如 -1001234567890）或 @频道名")
	}
	t := tgTarget{api: u.Scheme + "://" + u.Host + u.EscapedPath(), chatID: chat}
	if n, err := strconv.ParseInt(chat, 10, 64); err == nil {
		t.chatID = n
	}
	if th := strings.TrimSpace(q.Get("message_thread_id")); th != "" {
		n, err := strconv.ParseInt(th, 10, 64)
		if err != nil || !tgThreadRe.MatchString(th) {
			return tgTarget{}, errors.New("message_thread_id 必须是数字（群里话题的 ID）")
		}
		t.thread = n
	}
	return t, nil
}

// CheckTelegramURL 校验 Telegram 渠道的 Webhook 地址（路径是 /bot<Bot Token>/sendMessage，带合法的 chat_id）；主机另有校验。
func CheckTelegramURL(raw string) error {
	_, err := parseTelegramURL(raw)
	return err
}

func telegramCheck(b []byte) error {
	var r struct {
		OK          *bool  `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if json.Unmarshal(b, &r) == nil && r.OK != nil && !*r.OK {
		return fmt.Errorf("Telegram 返回错误 %d：%s", r.ErrorCode, r.Description)
	}
	return nil
}

// ---- 文字处理 ----

var tgEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func tgEsc(s string) string { return tgEscaper.Replace(s) }

// tgAttr 转义放进 href="…" 里的地址。
func tgAttr(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// utf16Len 是 s 的 UTF-16 长度（Telegram 按它算消息长度，emoji 占 2）。
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// cutUTF16 把 s 截到不超过 max 个 UTF-16 单位（不切在字符中间），截过的末尾加“…”。
func cutUTF16(s string, max int) string {
	if utf16Len(s) <= max {
		return s
	}
	n := 0
	var b strings.Builder
	for _, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if n+w > max-1 {
			break
		}
		b.WriteRune(r)
		n += w
	}
	return b.String() + "…"
}

// tgLine 是消息里的一行：h 是 HTML，v 是它解析后的可见长度。
type tgLine struct {
	h string
	v int
}

func tgPlain(s string) tgLine { return tgLine{tgEsc(s), utf16Len(s)} }

// tgJoin 把各行用换行连起来，返回 HTML 和解析后的可见长度（换行也算）。
func tgJoin(lines []tgLine) (string, int) {
	var b strings.Builder
	n := 0
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
			n++
		}
		b.WriteString(l.h)
		n += l.v
	}
	return b.String(), n
}

// tgMentions 返回 @ 的那一行：用户名写成 @name，数字用户 ID 写成 tg:// 链接，没有 Telegram 标识的平台用户只写“@显示名”。
// Telegram 没有“@所有人”，这种对象忽略。
func tgMentions(ments []Mention) (tgLine, bool) {
	var parts []string
	vis := 0
	for _, m := range ments {
		switch {
		case m.Raw == "all" || m.Raw == "@all":
			continue
		case m.Raw != "":
			v := NormalizeTelegramTarget(m.Raw)
			switch {
			case tgUserIDRe.MatchString(v):
				parts = append(parts, `<a href="tg://user?id=`+v+`">@`+v+`</a>`)
				vis += 1 + len(v)
			case tgUserRe.MatchString(v):
				parts = append(parts, "@"+v)
				vis += 1 + len(v)
			}
		default:
			name := "@" + m.Person.name()
			parts = append(parts, tgEsc(name))
			vis += utf16Len(name)
		}
	}
	if len(parts) == 0 {
		return tgLine{}, false
	}
	return tgLine{strings.Join(parts, " "), vis + len(parts) - 1}, true
}

// tgRequest 组装 sendMessage 请求：地址里的 chat_id / message_thread_id 放进请求体；地址解析不了（比如预览用的占位地址）时原样用它，不带 chat_id。
func tgRequest(ch Channel, html string) *built {
	body := map[string]any{"text": html, "parse_mode": "HTML", "link_preview_options": map[string]any{"is_disabled": true}}
	reqURL := ch.WebhookURL
	if t, err := parseTelegramURL(ch.WebhookURL); err == nil {
		reqURL = t.api
		body["chat_id"] = t.chatID
		if t.thread != 0 {
			body["message_thread_id"] = t.thread
		}
	}
	b, _ := json.Marshal(body)
	return &built{URL: reqURL, Body: b, Check: telegramCheck}
}

// buildTelegram 构造文本消息（没有消息模板时的 Title + Content）：超过 4096 个字符的部分截掉，@ 那一行不受影响。
func buildTelegram(ch Channel, text string, ments []Mention) (*built, error) {
	var lines []tgLine
	budget := tgTextMax
	if ml, ok := tgMentions(ments); ok {
		lines = append(lines, ml)
		budget -= ml.v + 1
	}
	lines = append(lines, tgPlain(cutUTF16(text, budget)))
	html, _ := tgJoin(lines)
	return tgRequest(ch, html), nil
}

// telegramRich 把消息渲染成 Telegram 的 HTML 消息：标题加粗，字段是“名称：值”一行一个，SQL 放进 <pre>，链接用 <a>。
// 超过 4096 个字符时先把最长的代码块 / 文本缩短一半再试；缩不动就返回 errTooBig，由 build 改发系统默认文案（那条同样按上限截断）。
func telegramRich(ch Channel, m *Message, ments []Mention) (*built, error) {
	m = m.clone()
	for i := 0; i < 8; i++ {
		html, n := tgMessage(m, ments)
		if n <= tgTextMax {
			return tgRequest(ch, html), nil
		}
		if !m.shrink() {
			break
		}
	}
	return nil, errTooBig
}

func tgMessage(m *Message, ments []Mention) (string, int) {
	var lines []tgLine
	if ml, ok := tgMentions(ments); ok {
		lines = append(lines, ml)
	}
	head := m.Head()
	lines = append(lines, tgLine{"<b>" + tgEsc(head) + "</b>", utf16Len(head)})
	for _, b := range m.Blocks {
		lines = append(lines, tgLine{}) // 块之间空一行
		switch b.T {
		case BlockFields:
			for _, r := range b.Rows {
				lines = append(lines, tgLine{"<b>" + tgEsc(r.L) + "</b>：" + tgEsc(r.V), utf16Len(r.L) + 1 + utf16Len(r.V)})
			}
		case BlockText:
			lines = append(lines, tgPlain(b.V))
		case BlockCode:
			if b.L != "" {
				lines = append(lines, tgLine{"<b>" + tgEsc(b.L) + "</b>", utf16Len(b.L)})
			}
			lines = append(lines, tgLine{"<pre>" + tgEsc(b.V) + "</pre>", utf16Len(b.V)})
		case BlockLink:
			if strings.HasPrefix(b.V, "http://") || strings.HasPrefix(b.V, "https://") {
				lines = append(lines, tgLine{`<a href="` + tgAttr(b.V) + `">` + tgEsc(b.L) + `</a>`, utf16Len(b.L)})
			} else {
				lines = append(lines, tgPlain(b.L+"："+b.V))
			}
		case BlockDivider:
			lines = append(lines, tgPlain("──────────"))
		}
	}
	return tgJoin(lines)
}
