// @author: kevinkelly
// @license: Apache Licence
// @file: push_test.go
// @time: 2026/10/28

package partman

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// 期望值（签名）由原程序 notify.go 的 feishuSign / 钉钉加签对同一输入算出（scratchpad 里复制函数运行）：
//
//	feishuSign("1700000000", "my-secret")                = I3DorsRQAITp7wWwxm5u7O9Ca8T+zLwkNSN7C16z0yQ=
//	dingtalk HMAC(key=secret, msg="1700000000123\nmy-secret") = 6tiTFauVPaDscGZ7Wv3gGFbdUrQxPtEsVrUwN9EdCZY=
const (
	goldFeishuSign   = "I3DorsRQAITp7wWwxm5u7O9Ca8T+zLwkNSN7C16z0yQ="
	goldDingtalkSign = "6tiTFauVPaDscGZ7Wv3gGFbdUrQxPtEsVrUwN9EdCZY="
)

var signNow = time.Unix(1700000000, 123*int64(time.Millisecond))

func TestSignaturesMatchOriginalProgram(t *testing.T) {
	if got := feishuSign("1700000000", "my-secret"); got != goldFeishuSign {
		t.Errorf("飞书加签 %s，原程序 %s", got, goldFeishuSign)
	}
	if got := dingtalkSign(signNow.UnixMilli(), "my-secret"); got != goldDingtalkSign {
		t.Errorf("钉钉加签 %s，原程序 %s", got, goldDingtalkSign)
	}
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("报文不是 JSON: %s", b)
	}
	return m
}

func TestPayloadShapes(t *testing.T) {
	// slack
	_, p, err := buildPayload(pushChannel{Type: chSlack, WebhookURL: "https://hooks.slack.com/services/T/B/x", MentionOn: "always", Mention: []string{"<!subteam^S1>", "<@U1>"}},
		"HDR", "BODY\n", false, signNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := decode(t, p)["text"]; got != "<!subteam^S1> <@U1>  HDR\n```\nBODY\n```" {
		t.Errorf("slack: %q", got)
	}
	// wecom：手机号与 userid 分开；关键词前缀
	_, p, _ = buildPayload(pushChannel{Type: chWecom, WebhookURL: "u", Keyword: "分区", MentionOn: "always", Mention: []string{"13800000000", "zhangsan", "@all"}}, "HDR", "BODY", true, signNow)
	m := decode(t, p)
	text := m["text"].(map[string]any)
	if m["msgtype"] != "text" || text["content"] != "分区 HDR\nBODY" ||
		strings.Join(toStrings(text["mentioned_list"]), ",") != "zhangsan,@all" || strings.Join(toStrings(text["mentioned_mobile_list"]), ",") != "13800000000" {
		t.Errorf("wecom: %s", p)
	}
	// feishu：@ 与加签
	_, p, _ = buildPayload(pushChannel{Type: chFeishu, WebhookURL: "u", Secret: "my-secret", Keyword: "KW", MentionOn: "on_failure", Mention: []string{"ou_abc", "all"}}, "HDR", "BODY", true, signNow)
	m = decode(t, p)
	content := m["content"].(map[string]any)["text"].(string)
	if m["msg_type"] != "text" || m["timestamp"] != "1700000000" || m["sign"] != goldFeishuSign ||
		content != "KW "+`<at user_id="ou_abc"></at> <at user_id="all">所有人</at> `+"HDR\nBODY" {
		t.Errorf("feishu: %s", p)
	}
	// feishu 没有密钥：不带签名
	_, p, _ = buildPayload(pushChannel{Type: chFeishu, WebhookURL: "u"}, "H", "B", false, signNow)
	if m = decode(t, p); m["sign"] != nil || m["timestamp"] != nil {
		t.Errorf("无密钥不应加签: %s", p)
	}
	// dingtalk：URL 追加 timestamp/sign；@ 手机号文本里也带上
	u, p, _ := buildPayload(pushChannel{Type: chDingtalk, WebhookURL: "https://oapi.dingtalk.com/robot/send?access_token=T", Secret: "my-secret", Keyword: "KW", MentionOn: "always", Mention: []string{"13800000000", "uid1", "all"}}, "HDR", "BODY", false, signNow)
	if u != "https://oapi.dingtalk.com/robot/send?access_token=T&timestamp=1700000000123&sign="+url.QueryEscape(goldDingtalkSign) {
		t.Errorf("dingtalk url: %s", u)
	}
	m = decode(t, p)
	at := m["at"].(map[string]any)
	if m["msgtype"] != "text" || m["text"].(map[string]any)["content"] != "KW @13800000000 HDR\nBODY" || at["isAtAll"] != true ||
		strings.Join(toStrings(at["atMobiles"]), ",") != "13800000000" || strings.Join(toStrings(at["atUserIds"]), ",") != "uid1" {
		t.Errorf("dingtalk: %s", p)
	}
	// mention_on=never / on_failure+成功：不 @
	_, p, _ = buildPayload(pushChannel{Type: chDingtalk, WebhookURL: "u", MentionOn: "on_failure", Mention: []string{"all", "13800000000"}}, "H", "B", false, signNow)
	at = decode(t, p)["at"].(map[string]any)
	if at["isAtAll"] != false || at["atMobiles"] != nil {
		t.Errorf("成功时 on_failure 不应 @: %s", p)
	}
	if _, _, err := buildPayload(pushChannel{Type: "mail"}, "H", "B", false, signNow); err == nil {
		t.Error("未知通道类型应报错")
	}
}

func toStrings(v any) []string {
	var out []string
	if a, ok := v.([]any); ok {
		for _, x := range a {
			out = append(out, x.(string))
		}
	}
	return out
}

func TestCheckRespPerPlatform(t *testing.T) {
	cases := []struct {
		typ    string
		status int
		body   string
		want   bool
	}{
		{chSlack, 200, "ok", true}, {chSlack, 200, "invalid_payload", false}, {chSlack, 500, "ok", false},
		{chWecom, 200, `{"errcode":0,"errmsg":"ok"}`, true}, {chWecom, 200, `{"errcode":93000}`, false}, {chWecom, 200, `not json`, false},
		{chDingtalk, 200, `{"errcode":0}`, true}, {chDingtalk, 200, `{"errcode":310000,"errmsg":"keywords not in content"}`, false},
		{chFeishu, 200, `{"code":0,"msg":"success"}`, true}, {chFeishu, 200, `{"StatusCode":0}`, true},
		{chFeishu, 200, `{"code":19024}`, false}, {chFeishu, 200, `{}`, false}, {chFeishu, 400, `{"code":0}`, false},
	}
	for _, c := range cases {
		if got := checkResp(c.typ, c.status, c.body); got != c.want {
			t.Errorf("%s %d %s: %v", c.typ, c.status, c.body, got)
		}
	}
}

// ---- 假 webhook 服务端 ----

type hit struct {
	Path, Query string
	Body        map[string]any
	Raw         string
}

type hookServer struct {
	*httptest.Server
	mu     sync.Mutex
	hits   []hit
	status int
	reply  string
}

func newHookServer(t *testing.T, status int, reply string) *hookServer {
	h := &hookServer{status: status, reply: reply}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		h.mu.Lock()
		h.hits = append(h.hits, hit{Path: r.URL.Path, Query: r.URL.RawQuery, Body: m, Raw: string(b)})
		st, rp := h.status, h.reply
		h.mu.Unlock()
		w.WriteHeader(st)
		_, _ = w.Write([]byte(rp))
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *hookServer) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.hits)
}

func (h *hookServer) last() hit {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits[len(h.hits)-1]
}

const secretToken = "TOKEN-abcdef-0123456789"

// addHook 往仓库里放一个指向假服务端的通道（用回环地址，通过地址校验）。
func (e *env) addHook(typ string, srv *hookServer, mutate func(w *Webhook)) *Webhook {
	w := Webhook{Type: typ, URL: srv.URL + "/robot/send?access_token=" + secretToken, IsEnabled: true, MentionOn: "on_failure", CreateTime: fixedNow}
	if mutate != nil {
		mutate(&w)
	}
	_ = e.repo.InsertWebhook(&w)
	cp := w
	return &cp
}

func replyFor(typ string) string {
	switch typ {
	case chSlack:
		return "ok"
	case chFeishu:
		return `{"code":0}`
	}
	return `{"errcode":0}`
}

// 一次维护要建 3 个分区（无删除）。
func (e *env) oneRunnable() *Config {
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-06", "2026-10-07", "2026-10-08")
	in := goodInput()
	in.PreCreate, in.Retain, in.Env = 4, 0, "Prod"
	return e.addConfig(in)
}

// notify_on 矩阵：always / on_failure / never × 成功 / 失败 / 没有变更。
func TestNotifyOnMatrix(t *testing.T) {
	type c struct {
		on      string
		fail    bool
		noop    bool
		pushes  bool
		display string
	}
	for _, tc := range []c{
		{"", false, false, true, "缺省按 always"},
		{"always", false, false, true, "always 成功"},
		{"always", true, false, true, "always 失败"},
		{"always", false, true, false, "always 没有变更不推"},
		{"on_failure", false, false, false, "on_failure 成功不推"},
		{"on_failure", true, false, true, "on_failure 失败推"},
		{"never", false, false, false, "never 成功"},
		{"never", true, false, false, "never 失败"},
		{"bogus", false, false, true, "非法值按 always"},
	} {
		e := newEnv()
		if tc.on != "" {
			e.src.cfg[cfgNotifyOn] = tc.on
		}
		srv := newHookServer(t, 200, replyFor(chDingtalk))
		e.addHook(chDingtalk, srv, nil)
		cfg := e.oneRunnable()
		if tc.noop {
			e.tgt.tables["log.access_log"] = dailyTable("2026-10-06", "2026-10-07", "2026-10-08", "2026-10-09", "2026-10-10", "2026-10-11")
		}
		if tc.fail {
			e.tgt.failOn = "p20261009"
		}
		res, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{})
		if err != nil {
			t.Fatalf("%s: %v", tc.display, err)
		}
		if res.Failed != tc.fail {
			t.Fatalf("%s: failed=%v", tc.display, res.Failed)
		}
		if got := srv.count() > 0; got != tc.pushes {
			t.Errorf("%s: 推送=%v，期望 %v", tc.display, got, tc.pushes)
		}
	}
}

// mention_on：通道级 always / on_failure / never 决定何时 @。
func TestMentionOnMatrix(t *testing.T) {
	for _, tc := range []struct {
		on        string
		fail      bool
		mentioned bool
	}{
		{"always", false, true}, {"always", true, true},
		{"on_failure", false, false}, {"on_failure", true, true},
		{"never", false, false}, {"never", true, false},
		{"", false, false}, {"", true, true}, // 空值按 on_failure（与原程序一致）
	} {
		e := newEnv()
		srv := newHookServer(t, 200, replyFor(chWecom))
		e.addHook(chWecom, srv, func(w *Webhook) { w.MentionOn, w.Mention = tc.on, `["zhangsan","13800000000"]` })
		cfg := e.oneRunnable()
		if tc.fail {
			e.tgt.failOn = "p20261009"
		}
		if _, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{}); err != nil {
			t.Fatal(err)
		}
		text := srv.last().Body["text"].(map[string]any)
		_, has := text["mentioned_list"]
		if has != tc.mentioned {
			t.Errorf("mention_on=%q fail=%v: 实际 @=%v 期望 %v (%s)", tc.on, tc.fail, has, tc.mentioned, srv.last().Raw)
		}
	}
}

// 消息正文沿用原程序：状态行、"sql_text" 行，失败的带 "exec_result"，env / 库表在标题里。
func TestPushBodyContent(t *testing.T) {
	e := newEnv()
	srv := newHookServer(t, 200, replyFor(chSlack))
	e.addHook(chSlack, srv, nil)
	cfg := e.oneRunnable()
	e.tgt.failOn = "p20261009"
	if _, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	text := srv.last().Body["text"].(string)
	for _, want := range []string{"Prod / log.access_log 分区维护", "状态：❌ 失败  （执行 成功1 / 失败1）", `"sql_text":"ALTER TABLE`, `"exec_result":"failed:`, "p20261009"} {
		if !strings.Contains(text, want) {
			t.Errorf("正文缺少 %q:\n%s", want, text)
		}
	}
	if srv.last().Path != "/robot/send" || srv.last().Query != "access_token="+secretToken {
		t.Errorf("请求地址: %+v", srv.last())
	}
}

// 推送失败绝不影响维护结果；日志、审计、运行输出里没有完整地址 / token / 密钥。
func TestPushFailureDoesNotAffectMaintenanceAndIsRedacted(t *testing.T) {
	cases := map[string]func(t *testing.T, e *env){
		"HTTP 500": func(t *testing.T, e *env) {
			srv := newHookServer(t, 500, "boom "+secretToken)
			e.addHook(chSlack, srv, func(w *Webhook) { w.Secret = "SIGN-SECRET-VALUE" })
		},
		"平台返回业务失败": func(t *testing.T, e *env) {
			srv := newHookServer(t, 200, `{"errcode":310000,"errmsg":"echo `+secretToken+`"}`)
			e.addHook(chDingtalk, srv, func(w *Webhook) { w.Secret = "SIGN-SECRET-VALUE" })
		},
		"连接被拒绝": func(t *testing.T, e *env) {
			srv := newHookServer(t, 200, "ok")
			e.addHook(chSlack, srv, nil)
			srv.Close()
		},
	}
	for name, setup := range cases {
		e := newEnv()
		setup(t, e)
		cfg := e.oneRunnable()
		res, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{})
		if err != nil || res.Failed || len(res.Statements) != 3 {
			t.Fatalf("%s: 推送失败不应影响维护结果: %+v %v", name, res, err)
		}
		if got := e.tgt.names("log.access_log"); !strings.Contains(got, "p20261009") {
			t.Fatalf("%s: 分区没建出来: %s", name, got)
		}
		if len(e.repo.logs) != 1 || len(e.repo.audits) != 1 {
			t.Fatalf("%s: 日志 %d 审计 %d", name, len(e.repo.logs), len(e.repo.audits))
		}
		var everything strings.Builder
		everything.WriteString(e.repo.logs[0].ExecResult)
		for _, a := range e.repo.audits {
			everything.WriteString(a.ExtraInfo)
		}
		for _, l := range res.Output.Lines {
			everything.WriteString(l.M)
		}
		for _, s := range []string{secretToken, "SIGN-SECRET-VALUE", "access_token=" + secretToken} {
			if strings.Contains(everything.String(), s) {
				t.Errorf("%s: 日志 / 审计 / 运行输出里出现了 %q", name, s)
			}
		}
		if !strings.Contains(everything.String(), "推送") || !strings.Contains(everything.String(), "失败") {
			t.Errorf("%s: 运行输出里应记录推送失败: %s", name, everything.String())
		}
		if !strings.Contains(everything.String(), "access_token=****") && name != "连接被拒绝" {
			t.Errorf("%s: 应出现脱敏后的地址", name)
		}
	}
}

func TestPushHostValidationAtSendAndNoRedirect(t *testing.T) {
	e := newEnv()
	// 库里被直接写入了不在白名单里的地址：发送时再校验，不发请求
	_ = e.repo.InsertWebhook(&Webhook{Type: chDingtalk, URL: "https://evil.example.com/robot/send?access_token=" + secretToken, IsEnabled: true})
	_ = e.repo.InsertWebhook(&Webhook{Type: chSlack, URL: "ftp://hooks.slack.com/services/T/B/x", IsEnabled: true})
	_ = e.repo.InsertWebhook(&Webhook{Type: chSlack, URL: "http://hooks.slack.com/services/T/B/x", IsEnabled: true}) // 平台域名但不是 https
	cfg := e.oneRunnable()
	res, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{})
	if err != nil || res.Failed {
		t.Fatalf("%+v %v", res, err)
	}
	joined := ""
	for _, l := range res.Output.Lines {
		joined += l.M + "\n"
	}
	if strings.Count(joined, "地址不合法，未发送") != 3 || strings.Contains(joined, secretToken) {
		t.Fatalf("%s", joined)
	}
	// 重定向不跟随：第二个服务端不应被访问
	target := newHookServer(t, 200, "ok")
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusFound)
	}))
	defer redir.Close()
	e2 := newEnv()
	_ = e2.repo.InsertWebhook(&Webhook{Type: chSlack, URL: redir.URL + "/services/T/B/x", IsEnabled: true})
	cfg2 := e2.oneRunnable()
	if _, err := e2.svc.Run(ctx, root, cfg2.ID, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if target.count() != 0 {
		t.Fatal("不应跟随重定向")
	}
}

func TestCheckWebhookURL(t *testing.T) {
	ok := []struct{ typ, u string }{
		{chSlack, "https://hooks.slack.com/services/T/B/x"},
		{chWecom, "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abc"},
		{chFeishu, "https://open.feishu.cn/open-apis/bot/v2/hook/abc"},
		{chFeishu, "https://open.larksuite.com/open-apis/bot/v2/hook/abc"},
		{chDingtalk, "https://oapi.dingtalk.com/robot/send?access_token=abc"},
		{chDingtalk, "http://127.0.0.1:8080/hook"}, {chSlack, "http://localhost/x"}, {chSlack, "https://[::1]:9/x"},
	}
	for _, c := range ok {
		if err := checkWebhookURL(c.typ, c.u); err != nil {
			t.Errorf("%s %s: %v", c.typ, c.u, err)
		}
	}
	bad := []struct{ typ, u string }{
		{chSlack, "https://evil.com/x"}, {chDingtalk, "https://hooks.slack.com/services/x"}, {chSlack, "http://hooks.slack.com/x"},
		{chSlack, "file:///etc/passwd"}, {chSlack, "gopher://hooks.slack.com/x"}, {chSlack, "https://hooks.slack.com.evil.com/x"},
		{chSlack, "https://169.254.169.254/latest/meta-data"}, {chSlack, "https://10.0.0.1/x"}, {chSlack, "not a url"}, {chSlack, "https:///x"},
		{chSlack, "https://hooks.slack.com@evil.com/x"},
	}
	for _, c := range bad {
		if err := checkWebhookURL(c.typ, c.u); err == nil {
			t.Errorf("%s %s 应拒绝", c.typ, c.u)
		}
	}
}

func TestMaskWebhook(t *testing.T) {
	for in, want := range map[string]string{
		"https://oapi.dingtalk.com/robot/send?access_token=abc123&x=1": "https://oapi.dingtalk.com/robot/send?access_token=****",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abc":     "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=****",
		"https://open.feishu.cn/open-apis/bot/v2/hook/abc":             "https://open.feishu.cn/open-apis/bot/v2/hook/****",
		"https://hooks.slack.com/services/T000/B000/XXXX":              "https://hooks.slack.com/services/****",
		"http://127.0.0.1:9/custom/path/with-token-1234":               "http://127.0.0.1:9/****",
		"http://127.0.0.1:9": "http://127.0.0.1:9",
	} {
		if got := maskWebhook(in); got != want {
			t.Errorf("%s → %s，期望 %s", in, got, want)
		}
	}
}

// 定时维护：执行了语句 / 计划失败都会推送；跳过的不推。
func TestAutoPushesOnExecutionAndPlanFailure(t *testing.T) {
	e := newEnv()
	e.enableAuto(false)
	srv := newHookServer(t, 200, replyFor(chDingtalk))
	e.addHook(chDingtalk, srv, nil)
	cfg := e.oneRunnable()
	sum := e.svc.RunAuto(ctx)
	if sum.Executed != 1 || srv.count() != 1 {
		t.Fatalf("%+v pushes=%d", sum, srv.count())
	}
	if text := srv.last().Body["text"].(map[string]any)["content"].(string); !strings.Contains(text, "分区维护") || !strings.Contains(text, "状态：✅ 成功") {
		t.Fatal(text)
	}
	// 再跑：没有变更（且在保护窗口内）：不推
	e.svc.RunAuto(ctx)
	if srv.count() != 1 {
		t.Fatalf("没有变更不应推送: %d", srv.count())
	}
	// 计划失败（表不存在）：推送，正文带 error 与 (reconcile)
	e.repo.logs = nil
	delete(e.tgt.tables, "log.access_log")
	sum = e.svc.RunAuto(ctx)
	if sum.Failed != 1 || srv.count() != 2 {
		t.Fatalf("%+v pushes=%d", sum, srv.count())
	}
	text := srv.last().Body["text"].(map[string]any)["content"].(string)
	if !strings.Contains(text, "状态：❌ 失败") || !strings.Contains(text, `"error":`) || !strings.Contains(text, "(reconcile)") {
		t.Fatal(text)
	}
	_ = cfg
}

// ---- 通道管理 ----

func TestWebhookCRUDAuditAndSecrets(t *testing.T) {
	e := newEnv()
	in := WebhookInput{Type: chDingtalk, WebhookURL: "https://oapi.dingtalk.com/robot/send?access_token=" + secretToken, IsEnabled: true,
		MentionOn: "always", Mention: []string{" 13800000000 ", "", "all"}, Keyword: "分区", Secret: "SIGN-SECRET-VALUE"}
	v, err := e.svc.CreateWebhook(root, in)
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != chDingtalk || !v.SecretSet || !v.IsEnabled || strings.Join(v.Mention, ",") != "13800000000,all" || v.Keyword != "分区" ||
		v.WebhookURL != "https://oapi.dingtalk.com/robot/send?access_token=****" {
		t.Fatalf("%+v", v)
	}
	if b, _ := json.Marshal(v); strings.Contains(string(b), "SIGN-SECRET-VALUE") || strings.Contains(string(b), secretToken) {
		t.Fatalf("响应里不得有完整地址 / 密钥: %s", b)
	}
	if e.repo.webhooks[0].Mention != `["13800000000","all"]` {
		t.Fatalf("mention 存库为 JSON 数组: %s", e.repo.webhooks[0].Mention)
	}
	// 同一地址不能重复
	if _, err := e.svc.CreateWebhook(root, in); code(err) != "duplicate" {
		t.Fatalf("%v", err)
	}
	// 修改：地址 / 密钥留空 = 不改
	upd := WebhookInput{Type: chDingtalk, IsEnabled: false, MentionOn: "never", Mention: []string{"uid1"}, Keyword: ""}
	v2, err := e.svc.UpdateWebhook(root, v.ID, upd)
	if err != nil || v2.IsEnabled || !v2.SecretSet || v2.MentionOn != "never" || v2.Keyword != "" {
		t.Fatalf("%+v %v", v2, err)
	}
	if got := e.repo.webhooks[0]; got.URL != in.WebhookURL || got.Secret != "SIGN-SECRET-VALUE" {
		t.Fatalf("留空应保持不变: %+v", got)
	}
	// 清空密钥 / 更新密钥
	upd.ClearSecret = true
	if v3, err := e.svc.UpdateWebhook(root, v.ID, upd); err != nil || v3.SecretSet {
		t.Fatalf("%+v %v", v3, err)
	}
	upd.ClearSecret, upd.Secret = false, "NEW-SECRET-VALUE"
	if v3, err := e.svc.UpdateWebhook(root, v.ID, upd); err != nil || !v3.SecretSet {
		t.Fatalf("%+v %v", v3, err)
	}
	// 启停：与当前值相同是空操作（不写审计）
	n := len(e.repo.audits)
	if _, err := e.svc.SetWebhookEnabled(root, v.ID, false); err != nil || len(e.repo.audits) != n {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.SetWebhookEnabled(root, v.ID, true); err != nil || len(e.repo.audits) != n+1 || !e.repo.webhooks[0].IsEnabled {
		t.Fatalf("%v", err)
	}
	// 列表
	l, err := e.svc.Webhooks(root)
	if err != nil || len(l.Items) != 1 || l.NotifyOn != "always" || len(l.NotifyOnOptions) != 3 {
		t.Fatalf("%+v %v", l, err)
	}
	if b, _ := json.Marshal(l); strings.Contains(string(b), "NEW-SECRET-VALUE") || strings.Contains(string(b), secretToken) {
		t.Fatalf("列表里不得有完整地址 / 密钥: %s", b)
	}
	// 删除
	if err := e.svc.DeleteWebhook(root, v.ID); err != nil || len(e.repo.webhooks) != 0 {
		t.Fatal(err)
	}
	if err := e.svc.DeleteWebhook(root, v.ID); code(err) != "not_found" {
		t.Fatalf("%v", err)
	}
	// 所有写操作都有审计，审计里没有完整地址与密钥
	acts := strings.Join(e.repo.auditActions(), ",")
	for _, a := range []string{"分区维护-新增推送通道", "分区维护-编辑推送通道", "分区维护-启用推送通道", "分区维护-删除推送通道"} {
		if !strings.Contains(acts, a) {
			t.Errorf("缺少审计 %s: %s", a, acts)
		}
	}
	for _, a := range e.repo.audits {
		for _, bad := range []string{secretToken, "SIGN-SECRET-VALUE", "NEW-SECRET-VALUE", "access_token=" + secretToken} {
			if strings.Contains(a.ExtraInfo, bad) {
				t.Errorf("审计 %s 里出现 %q: %s", a.Action, bad, a.ExtraInfo)
			}
		}
	}
}

func TestWebhookValidationAndPermission(t *testing.T) {
	e := newEnv()
	ok := WebhookInput{Type: chSlack, WebhookURL: "https://hooks.slack.com/services/T/B/x"}
	bad := map[string]WebhookInput{
		"未知平台":     {Type: "mail", WebhookURL: ok.WebhookURL},
		"地址为空":     {Type: chSlack},
		"主机不在白名单":  {Type: chSlack, WebhookURL: "https://evil.example.com/x"},
		"平台与主机不匹配": {Type: chDingtalk, WebhookURL: ok.WebhookURL},
		"非 https":  {Type: chSlack, WebhookURL: "http://hooks.slack.com/x"},
		"@时机非法":    {Type: chSlack, WebhookURL: ok.WebhookURL, MentionOn: "sometimes"},
		"地址过长":     {Type: chSlack, WebhookURL: "https://hooks.slack.com/services/" + strings.Repeat("a", 500)},
		"关键词过长":    {Type: chSlack, WebhookURL: ok.WebhookURL, Keyword: strings.Repeat("长", 501)},
		"@对象过多": {Type: chSlack, WebhookURL: ok.WebhookURL, Mention: func() []string {
			var m []string
			for i := 0; i < 100; i++ {
				m = append(m, "13800000000")
			}
			return m
		}()},
	}
	for name, in := range bad {
		if _, err := e.svc.CreateWebhook(root, in); err == nil {
			t.Errorf("%s：应拒绝", name)
		}
	}
	if len(e.repo.webhooks) != 0 || len(e.repo.audits) != 0 {
		t.Fatal("校验失败不应写入")
	}
	if _, err := e.svc.CreateWebhook(root, bad["主机不在白名单"]); code(err) != "webhook_host_not_allowed" {
		t.Fatal(err)
	}
	// 默认 mention_on = on_failure
	v, err := e.svc.CreateWebhook(root, ok)
	if err != nil || v.MentionOn != "on_failure" {
		t.Fatalf("%+v %v", v, err)
	}
	// 权限：需要 partman.edit
	ro := user(permMenu, permAdd, permDelete)
	if _, err := e.svc.Webhooks(ro); code(err) != "forbidden" {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.CreateWebhook(ro, ok); code(err) != "forbidden" {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.UpdateWebhook(ro, v.ID, ok); code(err) != "forbidden" {
		t.Fatalf("%v", err)
	}
	if err := e.svc.DeleteWebhook(ro, v.ID); code(err) != "forbidden" {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.SetWebhookEnabled(ro, v.ID, true); code(err) != "forbidden" {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.TestWebhooks(ctx, ro, 0); code(err) != "forbidden" {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.Webhooks(user(permMenu, permChange)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateWebhook(root, 999, ok); code(err) != "not_found" {
		t.Fatal(err)
	}
	if _, err := e.svc.TestWebhooks(ctx, root, 999); code(err) != "not_found" {
		t.Fatal(err)
	}
}

func TestWebhookTestSend(t *testing.T) {
	e := newEnv()
	e.src.cfg[cfgNotifyOn] = "never" // 测试发送不受全局时机影响
	if _, err := e.svc.TestWebhooks(ctx, root, 0); code(err) != "no_channel" {
		t.Fatalf("没有可用通道: %v", err)
	}
	okSrv := newHookServer(t, 200, replyFor(chFeishu))
	badSrv := newHookServer(t, 200, `{"code":19024,"msg":"Key Words Not Found"}`)
	offSrv := newHookServer(t, 200, replyFor(chSlack))
	a := e.addHook(chFeishu, okSrv, func(w *Webhook) { w.Secret = "SIGN-SECRET-VALUE"; w.MentionOn = "always"; w.Mention = `["ou_x"]` })
	b := e.addHook(chFeishu, badSrv, nil)
	c := e.addHook(chSlack, offSrv, func(w *Webhook) { w.IsEnabled = false })

	res, err := e.svc.TestWebhooks(ctx, root, 0)
	if err != nil || len(res.Results) != 2 || res.NotifyOn != "never" {
		t.Fatalf("%+v %v", res, err)
	}
	if !res.Results[0].OK || res.Results[0].ID != a.ID || res.Results[1].OK || res.Results[1].ID != b.ID || !strings.Contains(res.Results[1].Message, "19024") {
		t.Fatalf("%+v", res.Results)
	}
	if offSrv.count() != 0 {
		t.Fatal("全部测试只发已启用的通道")
	}
	// 固定测试文案；按通道 mention_on 决定是否 @；加签
	h := okSrv.last()
	text := h.Body["content"].(map[string]any)["text"].(string)
	if !strings.Contains(text, "分区维护推送测试") || !strings.Contains(text, "测试消息") || !strings.Contains(text, `<at user_id="ou_x"></at>`) || h.Body["sign"] == nil {
		t.Fatalf("%s", h.Raw)
	}
	if strings.Contains(badSrv.last().Raw, "at user_id") {
		t.Fatal("on_failure 的通道测试时不 @")
	}
	// 指定通道：不论是否启用
	res, err = e.svc.TestWebhooks(ctx, root, c.ID)
	if err != nil || len(res.Results) != 1 || !res.Results[0].OK || offSrv.count() != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	// 结果里没有完整地址 / token / 密钥
	if bs, _ := json.Marshal(res); strings.Contains(string(bs), secretToken) {
		t.Fatalf("%s", bs)
	}
	// 测试结果写进系统记录（仅管理员可见）
	var sys *Log
	for i := range e.repo.logs {
		if e.repo.logs[i].CnfID == 0 {
			sys = &e.repo.logs[i]
		}
	}
	if sys == nil || !strings.Contains(sys.ExecResult, "推送测试开始") || !strings.Contains(sys.ExecResult, `"audience":"admin"`) || strings.Contains(sys.ExecResult, secretToken) {
		t.Fatalf("%+v", sys)
	}
	// 有审计，且没有地址与密钥
	if got := e.repo.auditActions(); got[len(got)-1] != "分区维护-测试推送" {
		t.Fatalf("%v", got)
	}
	_ = time.Now
}
