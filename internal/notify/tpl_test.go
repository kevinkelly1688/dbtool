// @author: kevinkelly
// @license: Apache Licence
// @file: tpl_test.go
// @time: 2026/10/28

package notify

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// ---- 变量展开 ----

func TestExpandVarsAndIf(t *testing.T) {
	get := func(k string) string {
		return map[string]string{"a": "甲", "b": "", "c": "  ", "x": "{{a}}"}[k]
	}
	cases := []struct{ in, want string }{
		{"[{{a}}]#{{ a }}", "[甲]#甲"},
		{"{{#if a}}有 a{{/if}}|{{#if b}}有 b{{/if}}|{{#if c}}有 c{{/if}}", "有 a||"},
		{"{{#if a}}外{{#if b}}内{{/if}}尾{{/if}}", "外尾"},
		{"{{x}}", "{{a}}"}, // 变量值不会被再次展开
		{"没有变量 }} 也不报错", "没有变量 }} 也不报错"},
		{"{{unknown}}", ""},
	}
	for _, c := range cases {
		got, err := Expand(c.in, get)
		if err != nil || got != c.want {
			t.Errorf("Expand(%q) = %q, %v；想要 %q", c.in, got, err, c.want)
		}
	}
}

func TestExpandSyntaxErrors(t *testing.T) {
	for _, in := range []string{
		"{{a", "{{#if a}}没闭合", "多余{{/if}}", "{{#if}}x{{/if}}", "{{a b}}", "{{A}}", "{{#each a}}{{/each}}", "{{}}",
		"{{#if a}}{{#if a}}{{#if a}}{{#if a}}{{#if a}}x{{/if}}{{/if}}{{/if}}{{/if}}{{/if}}",
	} {
		if _, err := Refs(in); err == nil {
			t.Errorf("Refs(%q) 应该报错", in)
		}
	}
	refs, err := Refs("{{a}} {{#if b}}{{a}}{{c}}{{/if}}")
	if err != nil || strings.Join(refs, ",") != "a,b,c" {
		t.Errorf("Refs = %v, %v", refs, err)
	}
}

// ---- 隐藏字符串常量 ----

func TestMaskStringLiterals(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"简单", `UPDATE t SET k='abc123' WHERE id=1`, `UPDATE t SET k='***' WHERE id=1`},
		{"多个", `WHERE a='x' AND b = 'y'`, `WHERE a='***' AND b = '***'`},
		{"双写引号", `SET s='it''s a secret'`, `SET s='***'`},
		{"反斜杠转义", `SET s='a\'b'`, `SET s='***'`},
		{"双引号", `WHERE n = "bob" AND m='x'`, `WHERE n = "***" AND m='***'`},
		{"N 前缀", `WHERE n = N'张三'`, `WHERE n = N'***'`},
		{"空串保持", `SET a='' , b='z'`, `SET a='' , b='***'`},
		{"换行内常量", "SET s='line1\nline2'", `SET s='***'`},
		{"行注释里的引号", "-- password 'hunter2'\nSELECT 1", "-- password '***'\nSELECT 1"},
		{"块注释里的引号", "/* key='k1' */ SELECT 1", "/* key='***' */ SELECT 1"},
		{"没闭合也遮到结尾", `SELECT 'abc`, `SELECT '***'`},
		{"PG 美元引用", `SELECT $$top secret$$, $t$x$t$`, `SELECT $$***$$, $t$***$t$`},
		{"标识符里的 $ 不当引用", `SELECT a$b$c FROM t`, `SELECT a$b$c FROM t`},
		{"没有常量原样返回", `SELECT * FROM t WHERE id > 5`, `SELECT * FROM t WHERE id > 5`},
		{"数字与十六进制不处理", `WHERE a = 0xDEADBEEF AND b = 12345`, `WHERE a = 0xDEADBEEF AND b = 12345`},
		// 反斜杠在 PG / SQL Server 里不是转义：'C:\' 是一个完整常量，后面的 'secret' 也必须被遮。
		{"不识别转义的引擎", `SELECT 'C:\', 'secret'`, `SELECT '***'`},
	}
	for _, c := range cases {
		if got := MaskStringLiterals(c.in); got != c.want {
			t.Errorf("%s：\n  输入 %q\n  得到 %q\n  想要 %q", c.name, c.in, got, c.want)
		}
	}
}

// 无论怎么拼，被遮的 SQL 里都不应再出现常量原文。
func TestMaskNeverLeaks(t *testing.T) {
	secrets := []string{"S3cr3tK3y", "p@ssw0rd!", "13800138000"}
	for _, sql := range []string{
		`UPDATE a SET k='S3cr3tK3y', p="p@ssw0rd!" WHERE phone='13800138000'`,
		"INSERT INTO a VALUES ('S3cr3tK3y'),\n('p@ssw0rd!'),\n('13800138000')",
		`SELECT 'a\', 'S3cr3tK3y', 'p@ssw0rd!', '13800138000'`,
		`SELECT 'a''b', 'S3cr3tK3y' -- 'p@ssw0rd!'` + "\n" + `, '13800138000'`,
		`/* it's */ SELECT 'S3cr3tK3y', 'p@ssw0rd!', '13800138000'`,
		`SELECT ARRAY['S3cr3tK3y','p@ssw0rd!'], x['13800138000']`,
		`SELECT $q$S3cr3tK3y$q$, 'p@ssw0rd!', "13800138000"`,
	} {
		out := MaskStringLiterals(sql)
		for _, s := range secrets {
			if strings.Contains(sql, s) && strings.Contains(out, s) {
				t.Errorf("常量 %q 没被遮盖：\n  输入 %q\n  输出 %q", s, sql, out)
			}
		}
	}
}

func TestSQLDisplayModes(t *testing.T) {
	sql := "UPDATE t SET k='" + strings.Repeat("x", 300) + "' WHERE id=1;\nSELECT 1"
	if got, cut := SQLDisplay(sql, ModeNone, 500); cut || !strings.Contains(got, "已隐藏") || strings.Contains(got, "UPDATE") {
		t.Errorf("none: %q", got)
	}
	got, cut := SQLDisplay(sql, ModeHead, 50)
	if !cut || !strings.HasPrefix(got, "UPDATE t SET k='xxx") || !strings.Contains(got, "已截断") {
		t.Errorf("head 应保留常量并截断: %q", got)
	}
	got, cut = SQLDisplay(sql, ModeMask, 500)
	if cut || !strings.Contains(got, "k='***' WHERE") || strings.Contains(got, "xxx") {
		t.Errorf("mask: %q", got)
	}
	// 先遮再截：被截点落在常量里也不会漏出前半截
	got, _ = SQLDisplay("SELECT 'abcdefghij"+strings.Repeat("k", 100)+"', 2", ModeMask, 50)
	if strings.Contains(got, "abcdefghij") {
		t.Errorf("截断点在常量内，常量原文泄露: %q", got)
	}
	if got, _ = SQLDisplay(strings.Repeat("a", 4000), ModeFull, 0); utf8.RuneCountInString(got) > fullCodeCap+60 {
		t.Errorf("full 也有上限: %d", utf8.RuneCountInString(got))
	}
}

// ---- 校验 ----

func validTpl() Template {
	return Template{EventKey: "sql.finished", Scope: ScopeGlobal, Name: "t", Enabled: true, Title: "✅ 工单 #{{ticket_id}}", Tone: ToneOK,
		Blocks: []Block{
			{T: BlockFields, Rows: []Row{{L: "工单名", V: "{{title}}"}, {L: "耗时", V: "{{elapsed}}", Short: true}}},
			{T: BlockCode, L: "SQL", V: "{{sql}}", Mode: ModeMask, N: 500},
			{T: BlockLink, L: "详情", V: "{{url}}", As: "button"},
		}}
}

func TestValidate(t *testing.T) {
	def := MessageByKey("sql.finished")
	tp := validTpl()
	if is := tp.Validate(def); len(is) != 0 {
		t.Fatalf("合法模板不应有问题: %+v", is)
	}
	for name, mut := range map[string]func(*Template){
		"未知变量":    func(x *Template) { x.Title = "{{nope}}" },
		"标题空":     func(x *Template) { x.Title = " " },
		"状态色不合法":  func(x *Template) { x.Tone = "purple" },
		"字段行名称空":  func(x *Template) { x.Blocks[0].Rows[0].L = "" },
		"代码块字数越界": func(x *Template) { x.Blocks[1].N = 10 },
		"显示方式不合法": func(x *Template) { x.Blocks[1].Mode = "raw" },
		"链接地址非法":  func(x *Template) { x.Blocks[2].V = "javascript:alert(1)" },
		"块类型不认识":  func(x *Template) { x.Blocks = append(x.Blocks, Block{T: "image"}) },
		"if 没闭合":  func(x *Template) { x.Blocks[0].Rows[0].V = "{{#if title}}x" },
		"块太多": func(x *Template) {
			for i := 0; i < MaxBlocks; i++ {
				x.Blocks = append(x.Blocks, Block{T: BlockDivider})
			}
		},
	} {
		x := validTpl()
		mut(&x)
		if is := x.Validate(def); !HasCrit(is) {
			t.Errorf("%s：应该不能保存: %+v", name, is)
		}
	}
	// 用了这条消息里没有的变量：只提示
	x := validTpl()
	x.Title = "{{ticket_id}} {{error}} {{remark}}"
	is := x.Validate(MessageByKey("sql.finished"))
	if HasCrit(is) || len(is) != 2 {
		t.Errorf("error 在 sql.finished 里有，remark 没有；应只有 1 条提示: %+v", is)
	}
	x = Template{Title: "{{remark}}", Tone: ToneInfo, Blocks: []Block{{T: BlockDivider}}}
	if is = x.Validate(MessageByKey("sql.submitted")); HasCrit(is) || len(is) != 1 || is[0].Level != "warn" {
		t.Errorf("提示: %+v", is)
	}
}

// ---- 渲染 ----

func finishedEvent() Event {
	return Event{Kind: KindSQLWorkflow, Name: EventFinished, Title: "[SQL上线申请]工单已正常结束#3058", Content: "发起时间：…", GroupID: 1, Applicant: "alice",
		Vars: map[string]string{
			"ticket_id": "3058", "kind": "SQL上线申请", "title": "更新账户", "instance": "inst-1", "db": "shopdb", "url": "https://dba.example.com/sqlworkflow/3058",
			"applicant": "爱丽丝", "elapsed": "2.3 秒", "affected_rows": "2", "error": "", "remark": "不在这条消息里",
			"sql": "UPDATE tab SET PrivateKey='S3cr3tK3y' WHERE Code='X'",
		}}
}

func TestRenderTemplate(t *testing.T) {
	def := MessageByKey("sql.finished")
	tp := validTpl()
	tp.Blocks[0].Rows = append(tp.Blocks[0].Rows,
		Row{L: "错误", V: "{{error}}"},   // 值为空：这一行不发
		Row{L: "备注", V: "{{remark}}"},  // 这条消息没有该变量：同样不发
		Row{L: "SQL 片段", V: "{{sql}}"}) // 代码块以外用 {{sql}}：自动遮常量
	m, err := tp.Render(def, finishedEvent())
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "✅ 工单 #3058" || m.Tone != ToneOK || m.Key != "sql.finished" {
		t.Errorf("标题: %+v", m)
	}
	f := m.Blocks[0]
	var labels []string
	for _, r := range f.Rows {
		labels = append(labels, r.L)
		if strings.Contains(r.V, "S3cr3tK3y") {
			t.Errorf("{{sql}} 在字段里泄露了常量: %q", r.V)
		}
	}
	if strings.Join(labels, ",") != "工单名,耗时,SQL 片段" {
		t.Errorf("空值行应被去掉: %v", labels)
	}
	code := m.Blocks[1]
	if code.T != BlockCode || !strings.Contains(code.V, "PrivateKey='***'") || strings.Contains(code.V, "S3cr3tK3y") {
		t.Errorf("代码块应隐藏常量: %+v", code)
	}
	if lk := m.Blocks[2]; lk.T != BlockLink || lk.V != "https://dba.example.com/sqlworkflow/3058" || lk.As != "button" {
		t.Errorf("链接: %+v", lk)
	}

	// 链接地址取不到（不是 http）时整块不发；完全取不到变量的块不发
	ev := finishedEvent()
	ev.Vars["url"] = ""
	ev.Vars["sql"] = ""
	m, _ = tp.Render(def, ev)
	for _, b := range m.Blocks {
		if b.T == BlockLink || b.T == BlockCode {
			t.Errorf("取不到值的块不应发出: %+v", b)
		}
	}

	// 标题展开后为空时退回事件原标题
	tp.Title = "{{remark}}"
	if m, _ = tp.Render(def, finishedEvent()); m.Title != "[SQL上线申请]工单已正常结束#3058" {
		t.Errorf("标题回退: %q", m.Title)
	}

	// 通用变量：msg_title 只在声明了的消息里可用
	tp2 := Template{Scope: ScopeGlobal, Title: "{{msg_title}}|{{msg_content}}|{{event_label}}", Tone: ToneInfo}
	m, _ = tp2.Render(MessageByKey("sql.finished"), finishedEvent())
	if m.Title != "[SQL上线申请]工单已正常结束#3058||执行完成" {
		t.Errorf("sql 家族不提供 msg_content: %q", m.Title)
	}
	m, _ = tp2.Render(MessageByKey("archive.archive_done"), Event{Kind: "archive", Title: "T", Content: "C"})
	if m.Title != "T|C|归档完成" {
		t.Errorf("归档家族: %q", m.Title)
	}
}

func TestMessageKey(t *testing.T) {
	cases := []struct {
		ev   Event
		want string
	}{
		{Event{Kind: KindSQLWorkflow, Name: EventPending, Specific: EventSubmitted}, "sql.submitted"},
		{Event{Kind: KindSQLWorkflow, Name: EventPending}, "sql.pending"},
		{Event{Kind: KindSQLWorkflow, Name: EventException}, "sql.exception"},
		{Event{Kind: KindQueryPrivilege, Name: EventPassed}, "priv.passed"},
		{Event{Kind: KindQueryPrivilege, Name: EventPassed, TplKey: "priv.changed"}, "priv.changed"},
		{Event{Kind: KindQueryPrivilege, Name: EventAborted, TplKey: "priv.nope"}, ""},
		{Event{Kind: "archive", Name: EventFinished, Specific: "archive_done"}, "archive.archive_done"},
		{Event{Kind: "archive", Name: EventPending, Specific: EventSubmitted}, "archive.submitted"},
		{Event{Kind: "perf_issue", Name: EventException, Specific: EventPerfOverdue}, "perf.perf_overdue"},
		{Event{Kind: "perf_issue", Name: EventPending}, "perf.pending"},
		{Event{Kind: "inspect", Name: EventException, Specific: EventInspectAlert}, "inspect.inspect_alert"},
		{Event{Name: EventFinished}, ""}, // 没有 Kind：不套模板
		{Event{Kind: "unknown", Name: EventFinished}, ""},
	}
	for _, c := range cases {
		if got := MessageKey(c.ev); got != c.want {
			t.Errorf("MessageKey(%+v) = %q，想要 %q", c.ev, got, c.want)
		}
	}
}

func TestCatalogConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range Messages {
		if seen[m.Key] {
			t.Errorf("消息键重复: %s", m.Key)
		}
		seen[m.Key] = true
		if !strings.HasPrefix(m.Key, m.Family+".") || !validTone(m.Tone) {
			t.Errorf("消息 %s 定义不对: %+v", m.Key, m)
		}
		for _, v := range m.Vars {
			if _, ok := varByKey[v]; !ok {
				t.Errorf("消息 %s 引用了未登记的变量 %s", m.Key, v)
			}
		}
	}
	for _, v := range Vars {
		if !validKey(v.Key) {
			t.Errorf("变量键不合法: %s", v.Key)
		}
	}
}

// ---- 各渠道载荷 ----

func richMsg() *Message {
	m, err := validTpl().Render(MessageByKey("sql.finished"), finishedEvent())
	if err != nil {
		panic(err)
	}
	return m
}

func TestSlackPayload(t *testing.T) {
	ev := finishedEvent().WithRich(richMsg())
	ch := Channel{Type: TypeSlack, WebhookURL: "https://hooks.slack.test/x"}
	b, err := build(ch, ev, []Mention{{Raw: "U0ABC123"}, {Person: &Person{Username: "bob", Display: "鲍勃"}}}, newDisp().Now())
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Text   string           `json:"text"`
		Blocks []map[string]any `json:"blocks"`
	}
	if err := json.Unmarshal(b.Body, &p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Text, "<@U0ABC123>") || !strings.Contains(p.Text, "工单 #3058") {
		t.Errorf("fallback text: %q", p.Text)
	}
	types := []string{}
	for _, bl := range p.Blocks {
		types = append(types, bl["type"].(string))
	}
	// 提醒 @ 与标题在第一行；标题、整行字段、SQL 代码块排在同一个 section 里（Slack 里块与块之间的留白很大），只有按钮单独成块
	// （这条消息只有一个“占半行”字段，没有搭档，按整行显示）
	if strings.Join(types, ",") != "section,actions" {
		t.Errorf("块结构: %v", types)
	}
	txt, _ := p.Blocks[0]["text"].(map[string]any)
	body, _ := txt["text"].(string)
	lines := strings.Split(body, "\n")
	if len(lines) < 5 || !strings.HasPrefix(lines[0], "<@U0ABC123>") || !strings.HasSuffix(lines[0], " *✅ 工单 #3058*") ||
		lines[1] != "*工单名:* 更新账户" || lines[2] != "*耗时:* 2.3 秒" || lines[3] != "*SQL:*" || !strings.HasPrefix(lines[4], "```") {
		t.Errorf("同一段里依次是：“@ 加粗标题”同一行、“*名称:* 值”各一行、代码块（没有 header 块）: %q", lines)
	}
	for _, bl := range p.Blocks {
		if bl["type"] == "header" {
			t.Errorf("不应再用 header 块（自带很大的上下留白）")
		}
	}
	s := string(b.Body)
	if strings.Contains(s, "S3cr3tK3y") || !strings.Contains(s, "PrivateKey='***'") {
		t.Errorf("SQL 应被隐藏: %s", s)
	}
	if !strings.Contains(s, `"url":"https://dba.example.com/sqlworkflow/3058"`) {
		t.Errorf("按钮地址: %s", s)
	}
}

func TestDingTalkPayload(t *testing.T) {
	m := richMsg()
	m.Blocks = append(m.Blocks, MBlock{T: BlockText, V: "# 不是标题 *星号*\n- 不是列表"})
	ch := Channel{Type: TypeDingTalk, WebhookURL: "https://oapi.dingtalk.test/robot/send?access_token=t", Secret: "SEC"}
	b, err := build(ch, finishedEvent().WithRich(m), []Mention{{Person: &Person{Username: "alice", Display: "爱丽丝", DingID: "ding_alice"}}}, newDisp().Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.URL, "sign=") {
		t.Errorf("应加签: %s", b.URL)
	}
	var p struct {
		MsgType  string `json:"msgtype"`
		Markdown struct{ Title, Text string }
		At       struct {
			AtUserIds []string `json:"atUserIds"`
		} `json:"at"`
	}
	if err := json.Unmarshal(b.Body, &p); err != nil {
		t.Fatal(err)
	}
	txt := p.Markdown.Text
	if p.MsgType != "markdown" || len(p.At.AtUserIds) != 1 || !strings.HasPrefix(txt, "@ding_alice") {
		t.Errorf("markdown/at: %+v", p)
	}
	for _, want := range []string{"### ✅ 工单 #3058", "- **工单名**：更新账户", "```\nUPDATE tab SET PrivateKey='***'", "[详情](https://dba.example.com/sqlworkflow/3058)", `\# 不是标题 \*星号\*`, `\- 不是列表`} {
		if !strings.Contains(txt, want) {
			t.Errorf("markdown 缺少 %q:\n%s", want, txt)
		}
	}
}

func TestFeishuPayload(t *testing.T) {
	m := richMsg()
	m.Tone = ToneCrit
	m.Blocks = append(m.Blocks, MBlock{T: BlockText, V: "<at id=all></at> 注入"})
	ch := Channel{Type: TypeFeishu, WebhookURL: "https://open.feishu.test/hook/x", Secret: "SEC"}
	b, err := build(ch, finishedEvent().WithRich(m), []Mention{{Raw: "all"}}, newDisp().Now())
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		MsgType string `json:"msg_type"`
		Sign    string `json:"sign"`
		Card    struct {
			Header struct {
				Template string
				Title    struct{ Content string }
			}
			Elements []map[string]any
		}
	}
	if err := json.Unmarshal(b.Body, &p); err != nil {
		t.Fatal(err)
	}
	if p.MsgType != "interactive" || p.Sign == "" || p.Card.Header.Template != "red" || p.Card.Header.Title.Content != "✅ 工单 #3058" {
		t.Errorf("卡片: %+v", p)
	}
	s := string(b.Body)
	// JSON 里 < 会被写成 <；这里数“at id=all”出现次数：我们自己生成的 1 个 + 被打断（改成全角＜）的 1 个。
	if strings.Count(s, "at id=all") != 2 || strings.Count(s, "＜at id=all") != 1 {
		t.Errorf("内容里的 <at> 标签必须被打断，只保留我们自己生成的 @所有人: %s", s)
	}
	if p.Card.Elements[len(p.Card.Elements)-1]["tag"] != "div" {
		t.Errorf("elements: %v", p.Card.Elements)
	}
}

func TestOversizeShrinksThenFallsBack(t *testing.T) {
	big := strings.Repeat("SELECT 1 FROM t;\n", 3000)
	m := &Message{Key: "sql.finished", Title: "T", Tone: ToneInfo, Blocks: []MBlock{{T: BlockCode, L: "SQL", V: big}}}
	ch := Channel{Type: TypeDingTalk, WebhookURL: "https://oapi.dingtalk.test/robot/send"}
	b, err := buildRich(ch, m, nil, newDisp().Now())
	if err != nil || len(b.Body) > limDing {
		t.Fatalf("应缩短后发出: err=%v len=%d", err, len(b.Body))
	}
	if !strings.Contains(string(b.Body), "已截断") {
		t.Errorf("缩短后应带说明")
	}
	if m.Blocks[0].V != big {
		t.Errorf("缩短不应改动原消息")
	}
	// 一堆 200 字以内的块也撑爆：返回 errTooBig，build 回退为原来的文本
	var many []MBlock
	for i := 0; i < 400; i++ {
		many = append(many, MBlock{T: BlockText, V: strings.Repeat("字", 150)})
	}
	m2 := &Message{Key: "sql.finished", Title: "T", Tone: ToneInfo, Blocks: many}
	if _, err := buildRich(ch, m2, nil, newDisp().Now()); err != errTooBig {
		t.Fatalf("err = %v", err)
	}
	ev := Event{Title: "原标题", Content: "原正文"}.WithRich(m2)
	b, err = build(ch, ev, nil, newDisp().Now())
	if err != nil || !strings.Contains(string(b.Body), "原标题") || !strings.Contains(string(b.Body), `"msgtype":"text"`) {
		t.Errorf("应回退到文本消息: %v %s", err, b.Body)
	}
}

func TestEmailRich(t *testing.T) {
	SetPublicURL("https://dba.example.com", true)
	defer SetPublicURL("", false)
	m := richMsg()
	m.Title = `<b>工单</b> & "x"`
	text, h := emailRich(m, "审计邮件")
	if strings.Contains(h, "<b>工单</b>") || !strings.Contains(h, "&lt;b&gt;工单&lt;/b&gt;") {
		t.Errorf("标题必须转义: %s", h)
	}
	if !strings.Contains(h, `<a href="https://dba.example.com/sqlworkflow/3058"`) || !strings.Contains(h, "PrivateKey=&#39;***&#39;") || strings.Contains(h, "S3cr3tK3y") {
		t.Errorf("链接 / 常量: %s", h)
	}
	if !strings.Contains(text, "耗时：2.3 秒") || strings.Contains(text, "S3cr3tK3y") {
		t.Errorf("text: %s", text)
	}
	// 没有配置对外地址：链接块与字段里的 URL 一律去掉
	SetPublicURL("", false)
	m.Blocks[0].Rows = append(m.Blocks[0].Rows, Row{L: "地址", V: "https://localhost:8866/sqlworkflow/3058"})
	text, h = emailRich(m, "x")
	if strings.Contains(h, "http") || strings.Contains(text, "http") || strings.Contains(h, "<a ") {
		t.Errorf("没有对外地址时不应出现链接: %s | %s", text, h)
	}
}

// ---- Dispatcher：模板的查找与回退 ----

type tplSrc struct {
	fakeSrc
	mu   sync.Mutex
	list []Template
	n    int
}

func (s *tplSrc) ActiveTemplates() ([]Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return append([]Template(nil), s.list...), nil
}

func TestDispatcherTemplateLookup(t *testing.T) {
	var r recv
	srv := r.server(`ok`, 200)
	defer srv.Close()
	chA := Channel{ID: 11, Name: "A", Type: TypeSlack, WebhookURL: srv.URL + "/a", Enabled: true}
	chB := Channel{ID: 12, Name: "B", Type: TypeSlack, WebhookURL: srv.URL + "/b", Enabled: true}
	glob := Template{EventKey: "sql.finished", Scope: ScopeGlobal, Enabled: true, Title: "全局模板 #{{ticket_id}}", Tone: ToneOK,
		Blocks: []Block{{T: BlockText, V: "{{title}}"}}}
	only := Template{EventKey: "sql.finished", Scope: ScopeChannel(11), Enabled: true, Title: "A 专属 #{{ticket_id}}", Tone: ToneInfo,
		Blocks: []Block{{T: BlockText, V: "{{title}}"}}}
	off := Template{EventKey: "sql.submitted", Scope: ScopeGlobal, Enabled: false, Title: "停用的模板", Tone: ToneInfo,
		Blocks: []Block{{T: BlockText, V: "x"}}}
	src := &tplSrc{fakeSrc: fakeSrc{[]Channel{chA, chB}}, list: []Template{glob, only, off}}
	d := NewDispatcher(src, fakeDir{})

	bodies := func() map[string]string {
		out := map[string]string{}
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, q := range r.reqs {
			b, _ := json.Marshal(r.bodies[i])
			out[q.URL.Path] = string(b)
		}
		return out
	}
	d.Notify(context.Background(), finishedEvent())
	got := bodies()
	if !strings.Contains(got["/a"], "A 专属 #3058") || strings.Contains(got["/a"], "全局模板") {
		t.Errorf("渠道 A 应用专属模板: %s", got["/a"])
	}
	if !strings.Contains(got["/b"], "全局模板 #3058") {
		t.Errorf("渠道 B 应用全局模板: %s", got["/b"])
	}

	// 停用的模板、目录里没有的事件：发原来的文本
	r.reqs, r.bodies = nil, nil
	sub := finishedEvent()
	sub.Name, sub.Specific = EventPending, EventSubmitted
	d.Notify(context.Background(), sub)
	d.Notify(context.Background(), Event{Kind: "weird", Name: EventFinished, Title: "杂项", Content: "内容", GroupID: 1})
	for p, b := range bodies() {
		if strings.Contains(b, "blocks") {
			t.Errorf("%s 应发纯文本: %s", p, b)
		}
	}
	if len(r.bodies) != 4 {
		t.Errorf("应发 4 条: %d", len(r.bodies))
	}

	// 缓存：连发多条只读一次库；Invalidate 后重读
	n := src.n
	for i := 0; i < 5; i++ {
		d.Notify(context.Background(), finishedEvent())
	}
	if src.n != n {
		t.Errorf("TTL 内不应重复读库: %d -> %d", n, src.n)
	}
	d.InvalidateTemplates()
	d.Notify(context.Background(), finishedEvent())
	if src.n != n+1 {
		t.Errorf("失效后应重读: %d -> %d", n, src.n)
	}
}

func TestPreviewWithDraft(t *testing.T) {
	ch := Channel{ID: 5, Name: "s", Type: TypeSlack, WebhookURL: "https://hooks.slack.test/real-token", Enabled: true,
		Rules: []Rule{{Target: TargetApplicant}}}
	d := newDisp(ch)
	ev := finishedEvent()
	ev.Test = true
	pv, err := d.PreviewTemplate(ch, ev, validTpl())
	if err != nil {
		t.Fatal(err)
	}
	if pv.Message == nil || pv.Template != ScopeGlobal || !strings.Contains(string(pv.Payload), `"blocks"`) {
		t.Errorf("预览: %+v", pv)
	}
	if strings.Contains(string(pv.Payload), "real-token") || strings.Contains(string(pv.Payload), "S3cr3tK3y") {
		t.Errorf("预览泄露: %s", pv.Payload)
	}
	// 没有模板的预览：发系统默认文案
	pv, err = d.Preview(ch, ev)
	if err != nil || pv.Message != nil || pv.Template != "" {
		t.Errorf("无模板预览: %+v %v", pv, err)
	}
}

// 勾选“占半行”的字段连续排在一起时，Slack 里每两个并排在同一行（全角空格隔开，仍在同一个 section 里，不另起块，行间距最小）：
// “发起人 … 数据库 …”一行；整行字段仍是“*名称:* 值”。
func TestSlackHalfWidthFieldsShareARow(t *testing.T) {
	m := &Message{Icon: "📋", Title: "有新工单需要处理", Blocks: []MBlock{{T: BlockFields, Rows: []Row{
		{L: "工单名称", V: "平台执行权限"},
		{L: "发起人", V: "marcus", Short: true},
		{L: "数据库", V: "shopbasic", Short: true},
		{L: "目标实例", V: "(生产) 平台Saas"},
		{L: "URL", V: "http://dba.example.com/sqlworkflow/3057"},
	}}}}
	ch := Channel{Type: TypeSlack, WebhookURL: "https://hooks.slack.test/x"}
	b, err := slackRich(ch, m, []Mention{{Raw: "U0ABC123"}})
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Blocks []struct {
			Type string `json:"type"`
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
			Fields []struct {
				Text string `json:"text"`
			} `json:"fields"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(b.Body, &p); err != nil {
		t.Fatal(err)
	}
	// 只有一个 section：不拆出 fields 块（Slack 里块与块之间的留白很大，拆开就是图里那样大的行间距）
	if len(p.Blocks) != 1 || p.Blocks[0].Type != "section" || len(p.Blocks[0].Fields) != 0 {
		t.Fatalf("应是一个 section，没有 fields 栅格：%s", b.Body)
	}
	lines := strings.Split(p.Blocks[0].Text.Text, "\n")
	want := []string{
		"<@U0ABC123> *📋 有新工单需要处理*",
		"*工单名称:* 平台执行权限",
		"*发起人:* marcus" + slackPairSep + "*数据库:* shopbasic",
		"*目标实例:* (生产) 平台Saas",
		"*URL:* http://dba.example.com/sqlworkflow/3057",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("发起人与数据库应在同一行，其余各占一行：\n got %q\nwant %q", lines, want)
	}
}

// 并排放不下的情况：两个字段合起来太长、值里有换行，各占一行；奇数个时最后落单的一个也占一行。
func TestSlackHalfWidthPairFallsBackToOwnLines(t *testing.T) {
	long := strings.Repeat("库名", 25)
	m := &Message{Icon: "📋", Title: "t", Blocks: []MBlock{{T: BlockFields, Rows: []Row{
		{L: "发起人", V: "marcus", Short: true},
		{L: "数据库", V: long, Short: true}, // 合起来超过 slackPairMax
		{L: "环境", V: "生产", Short: true},
		{L: "备注", V: "第一行\n第二行", Short: true}, // 值里有换行
		{L: "级别", V: "P1", Short: true},       // 落单
	}}}}
	b, err := slackRich(Channel{Type: TypeSlack, WebhookURL: "https://hooks.slack.test/x"}, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Blocks []struct {
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(b.Body, &p); err != nil || len(p.Blocks) != 1 {
		t.Fatalf("应是一个 section：%v %s", err, b.Body)
	}
	body := p.Blocks[0].Text.Text
	if strings.Contains(body, slackPairSep) {
		t.Errorf("都放不下并排，不应出现并排间隔: %q", body)
	}
	for _, want := range []string{"*发起人:* marcus\n", "*数据库:* " + long + "\n", "*环境:* 生产\n", "*备注:*\n第一行\n第二行\n", "*级别:* P1"} {
		if !strings.Contains(body, want) {
			t.Errorf("缺少 %q：%q", want, body)
		}
	}
}
