// @author: kevinkelly
// @license: Apache Licence
// @file: wecom_test.go
// @time: 2026/10/28

package notify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

const wecomTestURL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=k-test"

func TestWeComTypeRegistered(t *testing.T) {
	if TypeLabel(TypeWeCom) != "企业微信" {
		t.Errorf("显示名: %q", TypeLabel(TypeWeCom))
	}
	if strings.Join(Types, ",") != "dingtalk,wecom,feishu,lark,slack,telegram,email" {
		t.Errorf("渠道类型列表: %v", Types)
	}
}

// 文本消息：@ 用 mentioned_list（userid、@all）和 mentioned_mobile_list（手机号）；平台用户没有企业微信 ID，只在正文里写名字；没有加签。
func TestWeComTextMentions(t *testing.T) {
	var r recv
	srv := r.server(`{"errcode":0,"errmsg":"ok"}`, 200)
	defer srv.Close()
	ch := Channel{Name: "w", Type: TypeWeCom, WebhookURL: srv.URL + "/cgi-bin/webhook/send?key=k1", Secret: "不会用到", Enabled: true, Rules: []Rule{
		{Target: TargetReviewers, Events: []string{EventPending}}, // alice、bob：用户资料里没有企业微信 ID
		{Target: TargetRaw, Value: "zhangsan"},
		{Target: TargetRaw, Value: "13800138000"},
		{Target: TargetAll},
	}}
	newDisp(ch).Notify(context.Background(), evPending)
	if len(r.bodies) != 1 {
		t.Fatalf("应发 1 条: %d", len(r.bodies))
	}
	b := r.bodies[0]
	if b["msgtype"] != "text" {
		t.Fatalf("msgtype: %v", b["msgtype"])
	}
	text := b["text"].(map[string]any)
	content := text["content"].(string)
	if ids := strings.Join(toStrings(text["mentioned_list"]), ","); ids != "@all,zhangsan" {
		t.Errorf("mentioned_list = %s", ids)
	}
	if mobiles := strings.Join(toStrings(text["mentioned_mobile_list"]), ","); mobiles != "13800138000" {
		t.Errorf("mentioned_mobile_list = %s", mobiles)
	}
	for _, want := range []string{"@爱丽丝", "@鲍勃", "新的工单申请#9", "库：shop"} {
		if !strings.Contains(content, want) {
			t.Errorf("正文缺少 %q: %s", want, content)
		}
	}
	q := r.reqs[0].URL.Query()
	if q.Get("key") != "k1" || q.Get("sign") != "" || q.Get("timestamp") != "" || b["sign"] != nil {
		t.Errorf("企业微信没有加签，只带 key: %v %v", q, b)
	}
}

func TestWeComNoMentions(t *testing.T) {
	var r recv
	srv := r.server(`{"errcode":0,"errmsg":"ok"}`, 200)
	defer srv.Close()
	newDisp(Channel{Type: TypeWeCom, WebhookURL: srv.URL, Enabled: true}).Notify(context.Background(), evPending)
	text := r.bodies[0]["text"].(map[string]any)
	if _, ok := text["mentioned_list"]; ok {
		t.Errorf("没有 @ 对象时不带 mentioned_list: %v", text)
	}
	if _, ok := text["mentioned_mobile_list"]; ok {
		t.Errorf("没有 @ 对象时不带 mentioned_mobile_list: %v", text)
	}
	if text["content"] != evPending.Title+"\n"+evPending.Content {
		t.Errorf("正文: %v", text["content"])
	}
}

// 返回 errcode ≠ 0 算发送失败（HTTP 仍是 200）；错误里不能带 webhook 的 key。
func TestWeComBusinessError(t *testing.T) {
	var r recv
	bad := r.server(`{"errcode":93000,"errmsg":"invalid webhook url"}`, 200)
	defer bad.Close()
	ch := Channel{Type: TypeWeCom, WebhookURL: bad.URL + "/cgi-bin/webhook/send?key=s3cr3t-key"}
	err := newDisp().SendTest(context.Background(), ch, evPending)
	if err == nil || !strings.Contains(err.Error(), "企业微信返回错误 93000：invalid webhook url") || strings.Contains(err.Error(), "s3cr3t-key") {
		t.Errorf("errcode 93000: %v", err)
	}
	good := r.server(`{"errcode":0,"errmsg":"ok"}`, 200)
	defer good.Close()
	if err := newDisp().SendTest(context.Background(), Channel{Type: TypeWeCom, WebhookURL: good.URL}, evPending); err != nil {
		t.Errorf("errcode 0 应成功: %v", err)
	}
}

func TestCutBytes(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"abc", 10, "abc"},
		{"abcdef", 6, "abcdef"},
		{"abcdefg", 6, "abc…"}, // 留出“…”的 3 个字节
		{"字字字字", 12, "字字字字"},   // 刚好 12 字节：不截
		{"字字字字字", 12, "字字字…"},  // 9 + 3 = 12 字节，不切在字符中间
		{"a字字字字", 11, "a字字…"},  // 1 + 3 + 3 + 3(…) = 10 ≤ 11
		{"字字字字字字", 2, "…"},     // max 比省略号还小：只剩省略号（调用方不会这样用）
	}
	for _, c := range cases {
		got := cutBytes(c.in, c.max)
		if got != c.want || !utf8.ValidString(got) {
			t.Errorf("cutBytes(%q, %d) = %q，想要 %q", c.in, c.max, got, c.want)
		}
	}
}

// 没有消息模板的文本超过 2048 字节：截掉多出的部分（不切坏字符），@ 列表不受影响。
func TestWeComTextCutAt2048Bytes(t *testing.T) {
	ev := evPending
	ev.Content = strings.Repeat("字", 1500) // 4500 字节
	b, err := build(Channel{Type: TypeWeCom, WebhookURL: wecomTestURL}, ev, []Mention{{Raw: "zhangsan"}}, newDisp().Now())
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Text struct {
			Content string   `json:"content"`
			List    []string `json:"mentioned_list"`
		} `json:"text"`
	}
	if err := json.Unmarshal(b.Body, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Text.Content) > wecomTextMax || !strings.HasSuffix(p.Text.Content, "…") || !utf8.ValidString(p.Text.Content) || !strings.HasPrefix(p.Text.Content, "[SQL上线]新的工单申请#9") {
		t.Errorf("content 应截到 %d 字节以内: %d %q", wecomTextMax, len(p.Text.Content), p.Text.Content[len(p.Text.Content)-12:])
	}
	if strings.Join(p.Text.List, ",") != "zhangsan" {
		t.Errorf("mentioned_list: %v", p.Text.List)
	}
}

type wecomMD struct {
	MsgType  string `json:"msgtype"`
	Markdown struct {
		Content string `json:"content"`
	} `json:"markdown"`
	Text struct {
		Content string   `json:"content"`
		List    []string `json:"mentioned_list"`
		Mobiles []string `json:"mentioned_mobile_list"`
	} `json:"text"`
}

func decodeWeCom(t *testing.T, b *built) wecomMD {
	t.Helper()
	var p wecomMD
	if err := json.Unmarshal(b.Body, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// 模板消息：只 @ 成员 userid 时发 markdown；标题按状态着色，字段 / SQL 用引用排版，链接用 markdown 链接；@ 写成 <@userid>。
func TestWeComRichMarkdown(t *testing.T) {
	m := richMsg()
	m.Tone = ToneWarn
	m.Blocks = append(m.Blocks, MBlock{T: BlockText, V: `<@evil> 注入 <font color="warning">假警告</font>`}, MBlock{T: BlockDivider},
		MBlock{T: BlockLink, L: "带标签的地址", V: "https://dba.example.com/x/<@evil2>", As: "text"})
	b, err := build(Channel{Type: TypeWeCom, WebhookURL: wecomTestURL}, finishedEvent().WithRich(m),
		[]Mention{{Raw: "zhangsan"}, {Person: &Person{Username: "bob", Display: "鲍勃"}}}, newDisp().Now())
	if err != nil {
		t.Fatal(err)
	}
	p := decodeWeCom(t, b)
	c := p.Markdown.Content
	if p.MsgType != "markdown" {
		t.Fatalf("msgtype = %q", p.MsgType)
	}
	for _, want := range []string{
		"<@zhangsan> @鲍勃\n", `### <font color="warning">✅ 工单 #3058</font>`, "> **工单名**：更新账户", "> **耗时**：2.3 秒",
		"**SQL**：\n> UPDATE tab SET PrivateKey='***'", "[详情](https://dba.example.com/sqlworkflow/3058)", "＜@evil> 注入", "＜font color=",
		"[带标签的地址](https://dba.example.com/x/%3C@evil2>)",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("markdown 缺少 %q:\n%s", want, c)
		}
	}
	// 内容里的 <@…> / <font …> 必须被打断：真正的 @ 只有我们自己生成的那一个，<font> 只有标题和分隔线上我们自己加的两个
	if strings.Count(c, "<@") != 1 || strings.Count(c, "<font") != 2 || strings.Contains(c, "S3cr3tK3y") {
		t.Errorf("内容里的标签必须被打断，常量要隐藏:\n%s", c)
	}
	if len(c) > wecomMarkdownMax {
		t.Errorf("content %d 字节超过上限", len(c))
	}
}

func TestWeComRichToneColor(t *testing.T) {
	for tone, want := range map[string]string{ToneOK: `<font color="info">`, ToneWarn: `<font color="warning">`, ToneCrit: `<font color="warning">`, ToneInfo: ""} {
		m := richMsg()
		m.Tone = tone
		b, err := wecomRich(Channel{WebhookURL: wecomTestURL}, m, nil)
		if err != nil {
			t.Fatal(err)
		}
		head := strings.SplitN(decodeWeCom(t, b).Markdown.Content, "\n", 2)[0]
		if want == "" && strings.Contains(head, "<font") || want != "" && !strings.Contains(head, want) {
			t.Errorf("%s 的标题: %q", tone, head)
		}
	}
}

// 要 @ 手机号或所有人：markdown 做不到，这条消息改发文本（纯文字版），@ 才能真正提醒到。
func TestWeComRichFallsBackToTextForMobileAndAll(t *testing.T) {
	m := richMsg()
	for name, ments := range map[string][]Mention{
		"手机号":  {{Raw: "13800138000"}, {Raw: "zhangsan"}},
		"所有人":  {{Raw: "all"}},
		"@all": {{Raw: "@all"}},
	} {
		b, err := build(Channel{Type: TypeWeCom, WebhookURL: wecomTestURL}, finishedEvent().WithRich(m), ments, newDisp().Now())
		if err != nil {
			t.Fatal(name, err)
		}
		p := decodeWeCom(t, b)
		if p.MsgType != "text" || !strings.HasPrefix(p.Text.Content, "✅ 工单 #3058\n") || !strings.Contains(p.Text.Content, "工单名：更新账户") || strings.Contains(p.Text.Content, "<@") {
			t.Errorf("%s: 应改发文本: %+v", name, p)
		}
		switch name {
		case "手机号":
			if strings.Join(p.Text.Mobiles, ",") != "13800138000" || strings.Join(p.Text.List, ",") != "zhangsan" {
				t.Errorf("%s: %+v", name, p.Text)
			}
		default:
			if strings.Join(p.Text.List, ",") != "@all" {
				t.Errorf("%s: %+v", name, p.Text)
			}
		}
	}
}

// 超过 4096 字节：先把最长的代码块缩短一半再试；一堆短块撑爆、缩不动时返回 errTooBig，build 回退为原来的文本（同样按 2048 字节截断）。
func TestWeComRichOversize(t *testing.T) {
	ch := Channel{Type: TypeWeCom, WebhookURL: wecomTestURL}
	big := strings.Repeat("SELECT 1 FROM t;\n", 3000)
	m := &Message{Key: "sql.finished", Title: "T", Tone: ToneInfo, Blocks: []MBlock{{T: BlockCode, L: "SQL", V: big}}}
	b, err := buildRich(ch, m, nil, newDisp().Now())
	if err != nil {
		t.Fatal(err)
	}
	c := decodeWeCom(t, b).Markdown.Content
	if len(c) > wecomMarkdownMax || !strings.Contains(c, "已截断") {
		t.Errorf("应缩短后发出: %d 字节", len(c))
	}
	if m.Blocks[0].V != big {
		t.Errorf("缩短不应改动原消息")
	}
	// 文本消息的上限更小（2048 字节）
	bt, err := buildRich(ch, m, []Mention{{Raw: "all"}}, newDisp().Now())
	if err != nil {
		t.Fatal(err)
	}
	if p := decodeWeCom(t, bt); p.MsgType != "text" || len(p.Text.Content) > wecomTextMax {
		t.Errorf("文本消息应缩到 %d 字节以内: %d", wecomTextMax, len(p.Text.Content))
	}
	// 一堆 200 字以内的块撑爆：缩不动
	var many []MBlock
	for i := 0; i < 40; i++ {
		many = append(many, MBlock{T: BlockText, V: strings.Repeat("字", 150)})
	}
	m2 := &Message{Key: "sql.finished", Title: "T", Tone: ToneInfo, Blocks: many}
	if _, err := buildRich(ch, m2, nil, newDisp().Now()); err != errTooBig {
		t.Fatalf("err = %v", err)
	}
	ev := Event{Title: "原标题", Content: strings.Repeat("原正文", 800)}.WithRich(m2)
	b, err = build(ch, ev, nil, newDisp().Now())
	if err != nil {
		t.Fatal(err)
	}
	if p := decodeWeCom(t, b); p.MsgType != "text" || !strings.HasPrefix(p.Text.Content, "原标题") || len(p.Text.Content) > wecomTextMax {
		t.Errorf("应回退到截断后的文本消息: %+v", p)
	}
}

// 预览：平台用户不能真正 @（没有企业微信 ID）并给出专门的说明；原生标识算有效；没有签名。
func TestWeComPreview(t *testing.T) {
	ch := Channel{Type: TypeWeCom, Secret: "x", Enabled: true, GroupID: 1, Rules: []Rule{
		{Target: TargetApplicant}, {Target: TargetRaw, Value: "zhangsan"}, {Target: TargetAll},
	}}
	pv, err := newDisp(ch).Preview(ch, evPending)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Platform != TypeWeCom || len(pv.Mentions) != 3 {
		t.Fatalf("预览: %+v", pv)
	}
	if u := pv.Mentions[0]; u.Kind != "user" || u.Resolved || u.Note != WeComUserNote {
		t.Errorf("平台用户: %+v", u)
	}
	if !pv.Mentions[1].Resolved || !pv.Mentions[2].Resolved {
		t.Errorf("原生标识 / 所有人: %+v", pv.Mentions)
	}
	var body wecomMD
	if err := json.Unmarshal(pv.Payload, &body); err != nil || body.MsgType != "text" || strings.Join(body.Text.List, ",") != "@all,zhangsan" || !strings.Contains(body.Text.Content, "@卡罗") {
		t.Errorf("请求体: %s", pv.Payload)
	}
}

// 带 @ 的消息，Notify 一条链路走通：模板 → markdown → HTTP。
func TestWeComRichThroughDispatcher(t *testing.T) {
	var r recv
	srv := r.server(`{"errcode":0,"errmsg":"ok"}`, 200)
	defer srv.Close()
	ch := Channel{ID: 7, Name: "w", Type: TypeWeCom, WebhookURL: srv.URL + "/cgi-bin/webhook/send?key=k", Enabled: true, Rules: []Rule{{Target: TargetRaw, Value: "zhangsan"}}}
	src := &tplSrc{fakeSrc: fakeSrc{[]Channel{ch}}, list: []Template{validTpl()}}
	NewDispatcher(src, fakeDir{}).Notify(context.Background(), finishedEvent())
	if len(r.bodies) != 1 || r.bodies[0]["msgtype"] != "markdown" {
		t.Fatalf("应发 1 条 markdown: %+v", r.bodies)
	}
	c := r.bodies[0]["markdown"].(map[string]any)["content"].(string)
	if !strings.HasPrefix(c, "<@zhangsan>\n### ") || !strings.Contains(c, "[详情](https://dba.example.com/sqlworkflow/3058)") {
		t.Errorf("markdown: %s", c)
	}
}
