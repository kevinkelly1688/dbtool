// @author: kevinkelly
// @license: Apache Licence
// @file: wecom.go
// @time: 2026/10/28

package notify

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 本文件：企业微信群机器人渠道（type = wecom）。
//
// 机器人地址是 https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…（key 就在地址里，没有加签）。
//   - 文本消息：content 最长 2048 字节；@ 用 mentioned_list（成员 userid，"@all" 是所有人）和 mentioned_mobile_list（手机号）；
//   - markdown 消息：content 最长 4096 字节；只能用 <@userid> @ 成员，@ 不了手机号和所有人（所以要 @ 手机号 / 所有人的消息改发文本，见 wecomRich）。
//
// 成功返回 {"errcode":0,"errmsg":"ok"}。每个机器人每分钟最多 20 条，超了企业微信返回错误（常见错误码 45009；只记在发送记录里，通知本来就是尽力而为）。
// 用户资料里没有“企业微信 ID”这一项，所以按平台用户（提交人 / 审批人 / 权限组 / 指定用户 / DBA 值班）@ 时只在正文里写“@显示名”，
// 不会真正提醒；要提醒到人，用“渠道原生标识”（成员 userid 或手机号）或“所有人”。

const (
	wecomTextMax     = 2048 // 文本消息 content 的字节上限
	wecomMarkdownMax = 4096 // markdown 消息 content 的字节上限

	// limWeCom 是整个请求体的大小兜底（字节）。企业微信按 content 的字节数限制，由 wecomRich 自己判断，这里只防止异常放大。
	limWeCom = 64 << 10
)

// wecomIDRe 是企业微信成员 userid 的写法：字母、数字、_ - . @，最长 64 位。
var wecomIDRe = regexp.MustCompile(`^[A-Za-z0-9_\-.@]{1,64}$`)

// WeComUserNote 是预览里对“平台用户没法真正 @”的说明（企业微信渠道专用）。
const WeComUserNote = "企业微信渠道不能按平台用户 @（用户资料里没有企业微信 ID），退化为文本“@显示名”，不会真正提醒；要提醒到人，请改用“渠道原生标识”（成员 userid 或手机号）或“所有人”"

// wecomAt 是企业微信消息里 @ 的部分。
type wecomAt struct {
	userIDs []string // 成员 userid（文本消息的 mentioned_list；markdown 里写成 <@userid>）
	mobiles []string // 手机号（mentioned_mobile_list，只有文本消息支持）
	all     bool     // @所有人（只有文本消息支持）
	names   string   // 没有企业微信 ID 的平台用户：只在正文里写“@显示名”，不会真正提醒
}

func wecomMentions(ments []Mention) wecomAt {
	var a wecomAt
	var names strings.Builder
	for _, m := range ments {
		switch {
		case m.Raw == "all" || m.Raw == "@all":
			a.all = true
		case m.Raw != "":
			if mobileRe.MatchString(m.Raw) {
				a.mobiles = append(a.mobiles, m.Raw)
			} else if wecomIDRe.MatchString(m.Raw) {
				a.userIDs = append(a.userIDs, m.Raw)
			}
		default:
			names.WriteString("@" + m.Person.name() + " ")
		}
	}
	a.names = names.String()
	return a
}

// textField 是文本消息里的 text 字段：正文加上 @ 列表。
func (a wecomAt) textField(content string) map[string]any {
	f := map[string]any{"content": content}
	ids := a.userIDs
	if a.all {
		ids = append([]string{"@all"}, ids...)
	}
	if len(ids) > 0 {
		f["mentioned_list"] = ids
	}
	if len(a.mobiles) > 0 {
		f["mentioned_mobile_list"] = a.mobiles
	}
	return f
}

func wecomCheck(b []byte) error {
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if json.Unmarshal(b, &r) == nil && r.ErrCode != 0 {
		return fmt.Errorf("企业微信返回错误 %d：%s", r.ErrCode, r.ErrMsg)
	}
	return nil
}

// cutBytes 把 s 截到不超过 max 个字节（不切在字符中间），截过的在末尾加“…”。
func cutBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "…"
	n := max - len(ellipsis)
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + ellipsis
}

// buildWeCom 构造文本消息（没有消息模板时的 Title + Content）：超过 2048 字节的部分截掉，@ 列表不受影响。
func buildWeCom(ch Channel, text string, ments []Mention) (*built, error) {
	at := wecomMentions(ments)
	body, _ := json.Marshal(map[string]any{"msgtype": "text", "text": at.textField(cutBytes(at.names+text, wecomTextMax))})
	return &built{URL: ch.WebhookURL, Body: body, Check: wecomCheck}, nil
}

// wecomTone 是状态色对应的 markdown 内置颜色：企业微信只有 info（绿）/ comment（灰）/ warning（橙红）三种。
var wecomTone = map[string]string{ToneOK: "info", ToneWarn: "warning", ToneCrit: "warning"}

// wecomEsc 把“<”换成全角：消息内容里的 <@userid>、<font …> 之类标签不能被当成真正的 @ 或排版。
func wecomEsc(s string) string { return strings.ReplaceAll(s, "<", "＜") }

// wecomQuote 把多行文字排成 markdown 引用（每行行首加“> ”）；引用里的行首不会被当成标题 / 列表。
func wecomQuote(s string) string {
	lines := strings.Split(strings.ReplaceAll(wecomEsc(s), "\r\n", "\n"), "\n")
	for i, ln := range lines {
		lines[i] = "> " + ln
	}
	return strings.Join(lines, "\n")
}

// wecomMarkdown 把消息排成企业微信 markdown（只用它支持的子集：标题、加粗、链接、引用、字体颜色）。
func wecomMarkdown(m *Message, at wecomAt) string {
	var md []string
	var mt []string
	for _, id := range at.userIDs {
		mt = append(mt, "<@"+id+">")
	}
	if n := strings.TrimSpace(at.names); n != "" {
		mt = append(mt, n)
	}
	if len(mt) > 0 {
		md = append(md, strings.Join(mt, " "))
	}
	head := wecomEsc(m.Head())
	if c := wecomTone[m.Tone]; c != "" {
		head = `<font color="` + c + `">` + head + `</font>`
	}
	md = append(md, "### "+head)
	for _, b := range m.Blocks {
		switch b.T {
		case BlockFields:
			for _, r := range b.Rows {
				md = append(md, "> **"+wecomEsc(r.L)+"**："+strings.TrimPrefix(wecomQuote(r.V), "> "))
			}
			md = append(md, "")
		case BlockText:
			md = append(md, wecomEsc(b.V), "")
		case BlockCode:
			if b.L != "" {
				md = append(md, "**"+wecomEsc(b.L)+"**：")
			}
			md = append(md, wecomQuote(b.V), "")
		case BlockLink:
			u := strings.ReplaceAll(mdURL(b.V), "<", "%3C") // 地址里的 < 也不能留着（<@userid> 会被当成 @）
			md = append(md, "["+strings.NewReplacer("[", "(", "]", ")").Replace(wecomEsc(b.L))+"]("+u+")", "")
		case BlockDivider:
			md = append(md, `<font color="comment">──────────</font>`, "")
		}
	}
	return strings.TrimRight(strings.Join(md, "\n"), "\n")
}

// wecomRich 把消息渲染成企业微信消息：
//   - 只 @ 成员 userid（或不 @）：发 markdown，标题按状态着色，字段 / 文本 / SQL / 链接用 markdown 的子集排版，@ 写成 <@userid>；
//   - 要 @ 手机号或所有人：markdown 做不到，这条消息改发文本（消息的纯文字版），@ 才能真正提醒到。
//
// content 超过上限（markdown 4096 / 文本 2048 字节）时，先把最长的代码块 / 文本缩短一半再试；缩不动就返回 errTooBig，
// 由 build 改发系统默认文案（那条同样按上限截断）。
func wecomRich(ch Channel, m *Message, ments []Mention) (*built, error) {
	at := wecomMentions(ments)
	asText := at.all || len(at.mobiles) > 0
	m = m.clone()
	for i := 0; i < 8; i++ {
		var body []byte
		var size, limit int
		if asText {
			content := at.names + m.Text()
			size, limit = len(content), wecomTextMax
			body, _ = json.Marshal(map[string]any{"msgtype": "text", "text": at.textField(content)})
		} else {
			content := wecomMarkdown(m, at)
			size, limit = len(content), wecomMarkdownMax
			body, _ = json.Marshal(map[string]any{"msgtype": "markdown", "markdown": map[string]string{"content": content}})
		}
		if size <= limit {
			return &built{URL: ch.WebhookURL, Body: body, Check: wecomCheck}, nil
		}
		if !m.shrink() {
			break
		}
	}
	return nil, errTooBig
}
