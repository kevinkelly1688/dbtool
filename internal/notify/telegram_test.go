// @author: kevinkelly
// @license: Apache Licence
// @file: telegram_test.go
// @time: 2026/10/28

package notify

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	tgTestToken = "123456789:AAE-abcdefghijklmnopqrstuvwxyz0123"
	tgTestPath  = "/bot" + tgTestToken + "/sendMessage"
	tgTestURL   = "https://api.telegram.org" + tgTestPath + "?chat_id=-1001234567890"
)

type tgBody struct {
	ChatID  any    `json:"chat_id"`
	Thread  int64  `json:"message_thread_id"`
	Text    string `json:"text"`
	Parse   string `json:"parse_mode"`
	Preview struct {
		Disabled bool `json:"is_disabled"`
	} `json:"link_preview_options"`
}

func decodeTG(t *testing.T, b []byte) tgBody {
	t.Helper()
	var p tgBody
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

var tgTagRe = regexp.MustCompile(`<[^>]*>`)

// tgVisible 是 Telegram 解析 HTML 后用户看到的文字长度（UTF-16 单位）：去掉标签、还原转义。
func tgVisible(html string) int {
	s := tgTagRe.ReplaceAllString(html, "")
	return utf16Len(strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&amp;", "&").Replace(s))
}

func TestTelegramTypeRegistered(t *testing.T) {
	if TypeLabel(TypeTelegram) != "Telegram" {
		t.Errorf("显示名: %q", TypeLabel(TypeTelegram))
	}
	if strings.Join(Types, ",") != "dingtalk,wecom,feishu,lark,slack,telegram,email" {
		t.Errorf("渠道类型列表: %v", Types)
	}
}

func TestTelegramTargetHelpers(t *testing.T) {
	for in, want := range map[string]string{" @dba_wang ": "dba_wang", "dba_wang": "dba_wang", "123456": "123456", "@": ""} {
		if got := NormalizeTelegramTarget(in); got != want {
			t.Errorf("NormalizeTelegramTarget(%q) = %q，想要 %q", in, got, want)
		}
	}
	for v, want := range map[string]bool{
		"dba_wang": true, "Dba_Wang9": true, "abcde": true, "123456789": true, "7": true,
		"abcd": false, "1abcde": false, "a-b-cdef": false, "@dba_wang": false, "": false, "a b cde": false,
		strings.Repeat("a", 33): false, strings.Repeat("a", 32): true, "12345678901234567": false,
	} {
		if got := TelegramRawOK(v); got != want {
			t.Errorf("TelegramRawOK(%q) = %v，想要 %v", v, got, want)
		}
	}
}

func TestTelegramParseURL(t *testing.T) {
	const api = "https://api.telegram.org" + tgTestPath
	ok := []struct {
		url    string
		chat   any
		thread int64
	}{
		{api + "?chat_id=-1001234567890", int64(-1001234567890), 0},
		{api + "?chat_id=123456", int64(123456), 0},
		{api + "?chat_id=@dba_channel", "@dba_channel", 0},
		{api + "?chat_id=-1001234567890&message_thread_id=42&parse_mode=Markdown&text=x", int64(-1001234567890), 42}, // 地址里的其它参数忽略
	}
	for _, c := range ok {
		tg, err := parseTelegramURL(c.url)
		if err != nil || tg.chatID != c.chat || tg.thread != c.thread || tg.api != api {
			t.Errorf("%s → %+v, %v", c.url, tg, err)
		}
		if err := CheckTelegramURL(c.url); err != nil {
			t.Errorf("CheckTelegramURL(%s): %v", c.url, err)
		}
	}
	bad := map[string]string{
		api:                  "缺少 chat_id",
		api + "?chat_id=":    "缺少 chat_id",
		api + "?chat_id=abc": "chat_id 不合法",
		api + "?chat_id=@ab": "chat_id 不合法",
		api + "?chat_id=-100&message_thread_id=x":                            "message_thread_id",
		api + "?chat_id=-100&message_thread_id=-3":                           "message_thread_id",
		"https://api.telegram.org/bot" + tgTestToken + "/getMe?chat_id=-100": "应是",
		"https://api.telegram.org/bot123:short/sendMessage?chat_id=-100":     "应是",
		"https://api.telegram.org/sendMessage?chat_id=-100":                  "应是",
		"ftp://api.telegram.org" + tgTestPath + "?chat_id=-100":              "不合法",
		"not a url": "不合法",
		"":          "不合法",
	}
	for u, want := range bad {
		err := CheckTelegramURL(u)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q 应报错并含 %q: %v", u, want, err)
		}
		if err != nil && strings.Contains(err.Error(), tgTestToken) {
			t.Errorf("错误里不应带 Bot Token: %v", err)
		}
	}
	if err := CheckTelegramURL("https://api.telegram.org/bot1/x?chat_id=1"); err == nil || !strings.Contains(err.Error(), TelegramAddrHint) {
		t.Errorf("路径不对时应写明正确的写法: %v", err)
	}
}

// 文本消息：chat_id / 话题放进 JSON 请求体，请求地址里不带查询参数；@ 写在第一行：用户名、数字用户 ID（链接）、平台用户（只有名字）；“所有人”忽略；没有加签。
func TestTelegramTextMentions(t *testing.T) {
	var r recv
	srv := r.server(`{"ok":true,"result":{"message_id":5}}`, 200)
	defer srv.Close()
	ch := Channel{Name: "t", Type: TypeTelegram, WebhookURL: srv.URL + tgTestPath + "?chat_id=-1001234567890&message_thread_id=7", Secret: "不会用到", Enabled: true, Rules: []Rule{
		{Target: TargetReviewers, Events: []string{EventPending}}, // alice、bob：用户资料里没有 Telegram 标识
		{Target: TargetRaw, Value: "dba_wang"},
		{Target: TargetRaw, Value: "@zhang_san"}, // 直接写带 @ 的也行
		{Target: TargetRaw, Value: "1234567"},    // 数字用户 ID：要写成链接才能提醒到
		{Target: TargetAll},                      // Telegram 没有“@所有人”：忽略
	}}
	newDisp(ch).Notify(context.Background(), evPending)
	if len(r.bodies) != 1 {
		t.Fatalf("应发 1 条: %d", len(r.bodies))
	}
	b := r.bodies[0]
	if b["chat_id"] != float64(-1001234567890) || b["message_thread_id"] != float64(7) || b["parse_mode"] != "HTML" {
		t.Errorf("请求体: %v", b)
	}
	if lp, _ := b["link_preview_options"].(map[string]any); lp["is_disabled"] != true {
		t.Errorf("应关掉链接预览: %v", b["link_preview_options"])
	}
	want := "@爱丽丝 @鲍勃 @dba_wang @zhang_san <a href=\"tg://user?id=1234567\">@1234567</a>\n" +
		"[SQL上线]新的工单申请#9\n发起人：张三\n库：shop"
	if b["text"] != want {
		t.Errorf("text = %q\n想要 %q", b["text"], want)
	}
	if u := r.reqs[0].URL; u.Path != tgTestPath || u.RawQuery != "" {
		t.Errorf("请求地址只留 /bot<Token>/sendMessage，参数都在请求体里: %s", u)
	}
	if b["sign"] != nil || b["timestamp"] != nil {
		t.Errorf("Telegram 没有加签: %v", b)
	}
}

func TestTelegramNoMentionsEscapes(t *testing.T) {
	var r recv
	srv := r.server(`{"ok":true}`, 200)
	defer srv.Close()
	ev := Event{Kind: KindSQLWorkflow, Name: EventPending, Title: "a<b>&c", Content: "x < y && z > 1\n<script>alert(1)</script>"}
	newDisp(Channel{Type: TypeTelegram, WebhookURL: srv.URL + tgTestPath + "?chat_id=@dba_channel", Enabled: true}).Notify(context.Background(), ev)
	if len(r.bodies) != 1 {
		t.Fatalf("应发 1 条: %d", len(r.bodies))
	}
	b := r.bodies[0]
	if want := "a&lt;b&gt;&amp;c\nx &lt; y &amp;&amp; z &gt; 1\n&lt;script&gt;alert(1)&lt;/script&gt;"; b["text"] != want {
		t.Errorf("只转义 & < >，没有 @ 时不带 @ 行: %q", b["text"])
	}
	if b["chat_id"] != "@dba_channel" {
		t.Errorf("公开频道的 chat_id 是字符串: %v", b["chat_id"])
	}
	if _, ok := b["message_thread_id"]; ok {
		t.Errorf("没有话题时不带 message_thread_id: %v", b)
	}
}

// 返回 4xx（Telegram 的错误都带 HTTP 状态码）或 200 里 ok = false 都算发送失败；错误里没有 Bot Token（地址里的主机之外都不写）。
func TestTelegramErrors(t *testing.T) {
	var r recv
	for _, c := range []struct {
		status int
		resp   string
		want   string
	}{
		{401, `{"ok":false,"error_code":401,"description":"Unauthorized"}`, "HTTP 401"},
		{400, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`, "chat not found"},
		{429, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 17","parameters":{"retry_after":17}}`, "retry after 17"},
		{200, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`, "Telegram 返回错误 403：Forbidden: bot was blocked by the user"},
	} {
		srv := r.server(c.resp, c.status)
		err := newDisp().SendTest(context.Background(), Channel{Type: TypeTelegram, WebhookURL: srv.URL + tgTestPath + "?chat_id=-100123"}, evPending)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), tgTestToken) {
			t.Errorf("%d %s → %v", c.status, c.resp, err)
		}
	}
	good := r.server(`{"ok":true,"result":{"message_id":1}}`, 200)
	defer good.Close()
	if err := newDisp().SendTest(context.Background(), Channel{Type: TypeTelegram, WebhookURL: good.URL + tgTestPath + "?chat_id=-100123"}, evPending); err != nil {
		t.Errorf("ok = true 应成功: %v", err)
	}
}

func TestCutUTF16(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"abc", 10, "abc"},
		{"abcdef", 6, "abcdef"},
		{"abcdefg", 6, "abcde…"}, // 留出“…”的 1 个单位
		{"😀😀😀", 6, "😀😀😀"},        // emoji 占 2 个单位：刚好 6，不截
		{"😀😀😀😀", 6, "😀😀…"},       // 4 + 1 = 5 ≤ 6，不切在 emoji 中间
		{"字字字", 1, "…"},          // max 比省略号还小：只剩省略号（调用方不会这样用）
	}
	for _, c := range cases {
		got := cutUTF16(c.in, c.max)
		if got != c.want || !utf8.ValidString(got) || utf16Len(got) > c.max && c.max > 1 {
			t.Errorf("cutUTF16(%q, %d) = %q，想要 %q", c.in, c.max, got, c.want)
		}
	}
}

// 没有消息模板的文本超过 4096 个字符（按 UTF-16 单位）：截掉多出的部分，@ 那一行不受影响。
func TestTelegramTextCutAt4096(t *testing.T) {
	for name, content := range map[string]string{"汉字": strings.Repeat("字", 5000), "emoji": strings.Repeat("😀", 3000)} {
		ev := evPending
		ev.Content = content
		b, err := build(Channel{Type: TypeTelegram, WebhookURL: tgTestURL}, ev, []Mention{{Raw: "dba_wang"}}, newDisp().Now())
		if err != nil {
			t.Fatal(name, err)
		}
		p := decodeTG(t, b.Body)
		if n := tgVisible(p.Text); n > tgTextMax || n < tgTextMax-2 || !strings.HasSuffix(p.Text, "…") || !utf8.ValidString(p.Text) || !strings.HasPrefix(p.Text, "@dba_wang\n[SQL上线]新的工单申请#9") {
			t.Errorf("%s: 应截到 %d 个字符以内: %d %q", name, tgTextMax, n, p.Text[len(p.Text)-12:])
		}
	}
}

func TestTelegramMentionsLine(t *testing.T) {
	l, ok := tgMentions([]Mention{{Raw: "all"}, {Raw: "@all"}, {Raw: "a b"}, {Raw: "abc"}})
	if ok {
		t.Errorf("所有人 / 不合法的标识 / 过短的用户名都不生成 @: %+v", l)
	}
	l, ok = tgMentions([]Mention{{Person: &Person{Username: "x", Display: "<小&明>"}}, {Raw: "@Dba_Wang"}, {Raw: "42"}})
	if !ok || l.h != `@&lt;小&amp;明&gt; @Dba_Wang <a href="tg://user?id=42">@42</a>` {
		t.Errorf("平台用户名字要转义: %q", l.h)
	}
	if n := tgVisible(l.h); n != l.v {
		t.Errorf("可见长度 %d，算出来 %d", l.v, n)
	}
}

// 模板消息：标题加粗，字段是“名称：值”，SQL 放进 <pre>，链接用 <a>；@ 在第一行；内容里的 < > & 全部转义，标签成对。
func TestTelegramRichMessage(t *testing.T) {
	m := richMsg()
	b, err := build(Channel{Type: TypeTelegram, WebhookURL: tgTestURL}, finishedEvent().WithRich(m),
		[]Mention{{Raw: "dba_wang"}, {Raw: "1234567"}, {Person: &Person{Username: "bob", Display: "鲍勃"}}, {Raw: "all"}}, newDisp().Now())
	if err != nil {
		t.Fatal(err)
	}
	p := decodeTG(t, b.Body)
	want := "@dba_wang <a href=\"tg://user?id=1234567\">@1234567</a> @鲍勃\n" +
		"<b>✅ 工单 #3058</b>\n" +
		"\n" +
		"<b>工单名</b>：更新账户\n" +
		"<b>耗时</b>：2.3 秒\n" +
		"\n" +
		"<b>SQL</b>\n" +
		"<pre>UPDATE tab SET PrivateKey='***' WHERE Code='***'</pre>\n" +
		"\n" +
		`<a href="https://dba.example.com/sqlworkflow/3058">详情</a>`
	if p.Text != want || p.Parse != "HTML" || p.ChatID != float64(-1001234567890) || !p.Preview.Disabled {
		t.Errorf("请求体: %+v\n想要 text = %q", p, want)
	}
	if strings.Contains(p.Text, "S3cr3tK3y") {
		t.Errorf("SQL 里的常量要隐藏: %s", p.Text)
	}

	// 内容里的标签 / 引号不能破坏 HTML：真正的标签只有我们自己生成的
	m.Blocks = append(m.Blocks,
		MBlock{T: BlockText, V: `<b>注入</b> & <a href="x">假链接</a>`}, MBlock{T: BlockDivider},
		MBlock{T: BlockLink, L: "带引号的地址", V: `https://dba.example.com/x?a=1&b="2"`, As: "text"},
		MBlock{T: BlockLink, L: "不是 http 的地址", V: "javascript:alert(1)", As: "text"})
	b, err = build(Channel{Type: TypeTelegram, WebhookURL: tgTestURL}, finishedEvent().WithRich(m), nil, newDisp().Now())
	if err != nil {
		t.Fatal(err)
	}
	txt := decodeTG(t, b.Body).Text
	for _, w := range []string{
		"&lt;b&gt;注入&lt;/b&gt; &amp; &lt;a href=\"x\"&gt;假链接&lt;/a&gt;", "──────────",
		`<a href="https://dba.example.com/x?a=1&amp;b=&quot;2&quot;">带引号的地址</a>`, "不是 http 的地址：javascript:alert(1)",
	} {
		if !strings.Contains(txt, w) {
			t.Errorf("缺少 %q:\n%s", w, txt)
		}
	}
	if strings.Count(txt, "<b>") != strings.Count(txt, "</b>") || strings.Count(txt, "<a ") != strings.Count(txt, "</a>") || strings.Count(txt, "<pre>") != 1 ||
		strings.Contains(txt, `<a href="javascript`) || strings.Contains(txt, "<b>注入") || strings.Contains(txt, `<a href="x">`) {
		t.Errorf("内容里的标签必须被转义，生成的标签要成对:\n%s", txt)
	}
}

// 超过 4096 个字符：先把最长的代码块缩短一半再试；一堆短块撑爆、缩不动时返回 errTooBig，build 回退为原来的文本（同样按上限截断）。
func TestTelegramRichOversize(t *testing.T) {
	ch := Channel{Type: TypeTelegram, WebhookURL: tgTestURL}
	big := strings.Repeat("SELECT 1 FROM t;\n", 3000)
	m := &Message{Key: "sql.finished", Title: "T", Tone: ToneInfo, Blocks: []MBlock{{T: BlockCode, L: "SQL", V: big}}}
	b, err := buildRich(ch, m, []Mention{{Raw: "dba_wang"}}, newDisp().Now())
	if err != nil {
		t.Fatal(err)
	}
	p := decodeTG(t, b.Body)
	if n := tgVisible(p.Text); n > tgTextMax || !strings.Contains(p.Text, "已截断") || !strings.HasPrefix(p.Text, "@dba_wang\n<b>T</b>") {
		t.Errorf("应缩短后发出: %d", n)
	}
	if m.Blocks[0].V != big {
		t.Errorf("缩短不应改动原消息")
	}
	// 一堆 150 字的块撑爆：缩不动
	var many []MBlock
	for i := 0; i < 40; i++ {
		many = append(many, MBlock{T: BlockText, V: strings.Repeat("字", 150)})
	}
	m2 := &Message{Key: "sql.finished", Title: "T", Tone: ToneInfo, Blocks: many}
	if _, err := buildRich(ch, m2, nil, newDisp().Now()); err != errTooBig {
		t.Fatalf("err = %v", err)
	}
	ev := Event{Title: "原标题", Content: strings.Repeat("原正文", 2000)}.WithRich(m2)
	b, err = build(ch, ev, nil, newDisp().Now())
	if err != nil {
		t.Fatal(err)
	}
	if p := decodeTG(t, b.Body); !strings.HasPrefix(p.Text, "原标题") || tgVisible(p.Text) > tgTextMax {
		t.Errorf("应回退到截断后的文本消息: %d", tgVisible(p.Text))
	}
}

// 预览：平台用户不能真正 @（没有 Telegram 标识）并给出专门的说明；原生标识算有效；预览不带 chat_id（用占位地址）；没有签名。
func TestTelegramPreview(t *testing.T) {
	ch := Channel{Type: TypeTelegram, Secret: "x", Enabled: true, GroupID: 1, Rules: []Rule{
		{Target: TargetApplicant}, {Target: TargetRaw, Value: "dba_wang"},
	}}
	pv, err := newDisp(ch).Preview(ch, evPending)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Platform != TypeTelegram || len(pv.Mentions) != 2 {
		t.Fatalf("预览: %+v", pv)
	}
	if u := pv.Mentions[0]; u.Kind != "user" || u.Resolved || u.Note != TelegramUserNote {
		t.Errorf("平台用户: %+v", u)
	}
	if !pv.Mentions[1].Resolved {
		t.Errorf("原生标识: %+v", pv.Mentions[1])
	}
	var body map[string]any
	if err := json.Unmarshal(pv.Payload, &body); err != nil {
		t.Fatal(err)
	}
	text, _ := body["text"].(string)
	if body["parse_mode"] != "HTML" || !strings.HasPrefix(text, "@卡罗 @dba_wang\n") || strings.Contains(string(pv.Payload), "example.invalid") {
		t.Errorf("请求体: %s", pv.Payload)
	}
	if _, ok := body["chat_id"]; ok {
		t.Errorf("预览用占位地址，不带 chat_id: %s", pv.Payload)
	}
}

// 带 @ 的消息，Notify 一条链路走通：模板 → HTML 消息 → HTTP。
func TestTelegramRichThroughDispatcher(t *testing.T) {
	var r recv
	srv := r.server(`{"ok":true,"result":{"message_id":9}}`, 200)
	defer srv.Close()
	ch := Channel{ID: 7, Name: "t", Type: TypeTelegram, WebhookURL: srv.URL + tgTestPath + "?chat_id=-1001234567890", Enabled: true, Rules: []Rule{{Target: TargetRaw, Value: "dba_wang"}}}
	src := &tplSrc{fakeSrc: fakeSrc{[]Channel{ch}}, list: []Template{validTpl()}}
	NewDispatcher(src, fakeDir{}).Notify(context.Background(), finishedEvent())
	if len(r.bodies) != 1 {
		t.Fatalf("应发 1 条: %+v", r.bodies)
	}
	text, _ := r.bodies[0]["text"].(string)
	if !strings.HasPrefix(text, "@dba_wang\n<b>✅ 工单 #3058</b>\n") || !strings.Contains(text, `<a href="https://dba.example.com/sqlworkflow/3058">详情</a>`) || r.bodies[0]["parse_mode"] != "HTML" {
		t.Errorf("text: %s", text)
	}
}
