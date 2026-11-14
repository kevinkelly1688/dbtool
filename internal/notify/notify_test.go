// @author: kevinkelly
// @license: Apache Licence
// @file: notify_test.go
// @time: 2026/10/28

package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeDir struct{}

var people = map[string]Person{
	"alice": {Username: "alice", Display: "爱丽丝", DingID: "ding_alice", FeishuID: "ou_alice", SlackID: "U0ALICE1"},
	"bob":   {Username: "bob", Display: "鲍勃"}, // 没有任何渠道 ID
	"carol": {Username: "carol", Display: "卡罗", DingID: "ding_carol", FeishuID: "ou_carol", SlackID: "U0CAROL1"},
}

func (fakeDir) UsersByName(names []string) ([]Person, error) {
	var out []Person
	for _, n := range names {
		if p, ok := people[n]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// 权限组 20（资源组 1 内）的成员：alice、bob；权限组 10：carol。
func (fakeDir) AuthGroupMembers(g, rg uint) ([]Person, error) {
	if rg != 1 {
		return nil, nil
	}
	switch g {
	case 20:
		return []Person{people["alice"], people["bob"]}, nil
	case 10:
		return []Person{people["carol"]}, nil
	}
	return nil, nil
}

type fakeSrc struct{ chans []Channel }

func (s fakeSrc) Channels(uint) ([]Channel, error) { return s.chans, nil }

type recv struct {
	mu     sync.Mutex
	reqs   []*http.Request
	bodies []map[string]any
}

func (r *recv) server(resp string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		b, _ := io.ReadAll(q.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		r.mu.Lock()
		r.reqs, r.bodies = append(r.reqs, q), append(r.bodies, m)
		r.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
}

func newDisp(chans ...Channel) *Dispatcher {
	d := NewDispatcher(fakeSrc{chans}, fakeDir{})
	d.Now = func() time.Time { return time.Unix(1700000000, 0) }
	return d
}

var evPending = Event{Kind: KindSQLWorkflow, Name: EventPending, Title: "[SQL上线]新的工单申请#9", Content: "发起人：张三\n库：shop",
	GroupID: 1, Applicant: "carol", ReviewerAuthGroupID: 20}

func TestDingTalkMentionsAndSignature(t *testing.T) {
	var r recv
	srv := r.server(`{"errcode":0}`, 200)
	defer srv.Close()
	ch := Channel{Name: "d", Type: TypeDingTalk, WebhookURL: srv.URL + "/robot/send?access_token=tok", Secret: "SECabc", Enabled: true, Rules: []Rule{
		{Target: TargetReviewers, Events: []string{EventPending}},
		{Target: TargetUser, Value: "carol"},
		{Target: TargetRaw, Value: "13800138000"},
		{Target: TargetRaw, Value: "ding_manager"},
		{Target: TargetAll, Events: []string{EventException}}, // 不在 pending 事件上生效
	}}
	newDisp(ch).Notify(context.Background(), evPending)
	if len(r.bodies) != 1 {
		t.Fatalf("应发 1 条: %d", len(r.bodies))
	}
	b := r.bodies[0]
	content := b["text"].(map[string]any)["content"].(string)
	at := b["at"].(map[string]any)
	ids := toStrings(at["atUserIds"])
	mobiles := toStrings(at["atMobiles"])
	// 审批人 alice(有钉钉 ID)、bob(无 ID，仅显示名)；carol 指定用户；原生手机号与 userId
	if strings.Join(ids, ",") != "ding_alice,ding_carol,ding_manager" || strings.Join(mobiles, ",") != "13800138000" || at["isAtAll"] != false {
		t.Errorf("at: %+v", at)
	}
	for _, want := range []string{"@ding_alice", "@鲍勃", "@ding_carol", "@13800138000", "@ding_manager", "新的工单申请#9", "库：shop"} {
		if !strings.Contains(content, want) {
			t.Errorf("正文缺少 %q: %s", want, content)
		}
	}
	// 加签：URL 带 timestamp(毫秒) 与 sign = base64(HMAC-SHA256(secret, ts+"\n"+secret))，且保留原有参数
	q := r.reqs[0].URL.Query()
	ts := q.Get("timestamp")
	mac := hmac.New(sha256.New, []byte("SECabc"))
	mac.Write([]byte(ts + "\n" + "SECabc"))
	if ts != strconv.FormatInt(1700000000000, 10) || q.Get("sign") != base64.StdEncoding.EncodeToString(mac.Sum(nil)) || q.Get("access_token") != "tok" {
		t.Errorf("签名/参数: %v", q)
	}
}

func TestDingTalkAtAll(t *testing.T) {
	var r recv
	srv := r.server(`{"errcode":0}`, 200)
	defer srv.Close()
	ch := Channel{Type: TypeDingTalk, WebhookURL: srv.URL, Enabled: true, Rules: []Rule{{Target: TargetAll}}}
	newDisp(ch).Notify(context.Background(), evPending)
	b := r.bodies[0]
	if b["at"].(map[string]any)["isAtAll"] != true || !strings.Contains(b["text"].(map[string]any)["content"].(string), "@所有人") {
		t.Errorf("@所有人: %+v", b)
	}
	if len(r.reqs[0].URL.Query()["sign"]) != 0 {
		t.Error("没有配置密钥时不应加签")
	}
}

func TestFeishuAndLark(t *testing.T) {
	for _, typ := range []string{TypeFeishu, TypeLark} {
		var r recv
		srv := r.server(`{"code":0,"msg":"success"}`, 200)
		ch := Channel{Type: typ, WebhookURL: srv.URL + "/open-apis/bot/v2/hook/x", Secret: "fsSecret", Enabled: true, Rules: []Rule{
			{Target: TargetApplicant}, {Target: TargetAuthGroup, Value: "20"}, {Target: TargetRaw, Value: "ou_boss"}, {Target: TargetAll},
		}}
		newDisp(ch).Notify(context.Background(), evPending)
		srv.Close()
		b := r.bodies[0]
		text := b["content"].(map[string]any)["text"].(string)
		for _, want := range []string{`<at user_id="ou_carol">卡罗</at>`, `<at user_id="ou_alice">爱丽丝</at>`, "@鲍勃", `<at user_id="ou_boss"></at>`, `<at user_id="all">所有人</at>`, "新的工单申请#9"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s 正文缺少 %q: %s", typ, want, text)
			}
		}
		if b["msg_type"] != "text" {
			t.Errorf("%s msg_type: %v", typ, b["msg_type"])
		}
		// 签名：HMAC-SHA256，key = timestamp + "\n" + secret，消息体为空
		mac := hmac.New(sha256.New, []byte("1700000000\nfsSecret"))
		want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if b["timestamp"] != "1700000000" || b["sign"] != want {
			t.Errorf("%s 签名: %v %v", typ, b["timestamp"], b["sign"])
		}
	}
}

func TestSlackMentions(t *testing.T) {
	var r recv
	srv := r.server("ok", 200)
	defer srv.Close()
	ch := Channel{Type: TypeSlack, WebhookURL: srv.URL, Enabled: true, Rules: []Rule{
		{Target: TargetUser, Value: "alice"}, {Target: TargetUser, Value: "bob"}, {Target: TargetRaw, Value: "S0ARY4FQATB"},
		{Target: TargetRaw, Value: "U012AB3CD"}, {Target: TargetRaw, Value: "here"}, {Target: TargetRaw, Value: "bad id!"},
	}}
	ev := evPending
	ev.Content = "SQL: select a<b & c from t"
	newDisp(ch).Notify(context.Background(), ev)
	text := r.bodies[0]["text"].(string)
	for _, want := range []string{"<@U0ALICE1>", "@鲍勃", "<!subteam^S0ARY4FQATB>", "<@U012AB3CD>", "<!here>", "a&lt;b &amp; c"} {
		if !strings.Contains(text, want) {
			t.Errorf("缺少 %q: %s", want, text)
		}
	}
	if strings.Contains(text, "bad id") {
		t.Error("非法的原生标识应被忽略")
	}
	// 内容里的特殊字符被转义，不会被 Slack 当成链接/提及
	if strings.Contains(text, "a<b") {
		t.Errorf("内容未转义: %s", text)
	}
}

func TestEventFilteringAndDisabled(t *testing.T) {
	var r recv
	srv := r.server("ok", 200)
	defer srv.Close()
	only := Channel{Name: "only-finished", Type: TypeSlack, WebhookURL: srv.URL, Enabled: true, Events: []string{EventFinished}}
	off := Channel{Name: "off", Type: TypeSlack, WebhookURL: srv.URL, Enabled: false}
	all := Channel{Name: "all", Type: TypeSlack, WebhookURL: srv.URL, Enabled: true}
	d := newDisp(only, off, all)
	d.Notify(context.Background(), evPending)
	if len(r.bodies) != 1 {
		t.Errorf("pending：只有订阅全部的渠道会发，停用的和只订阅 finished 的不发: %d", len(r.bodies))
	}
	ev := evPending
	ev.Name = EventFinished
	d.Notify(context.Background(), ev)
	if len(r.bodies) != 3 {
		t.Errorf("finished：只订阅 finished 的和订阅全部的都发: %d", len(r.bodies))
	}
}

func TestPerEventRulesAndDedupe(t *testing.T) {
	var r recv
	srv := r.server("ok", 200)
	defer srv.Close()
	ch := Channel{Type: TypeSlack, WebhookURL: srv.URL, Enabled: true, Rules: []Rule{
		{Target: TargetApplicant, Events: []string{EventPassed}},
		{Target: TargetReviewers, Events: []string{EventPending}},
		{Target: TargetUser, Value: "alice"}, // 与审批人重复：只 @ 一次
	}}
	d := newDisp(ch)
	d.Notify(context.Background(), evPending)
	text := r.bodies[0]["text"].(string)
	if strings.Count(text, "<@U0ALICE1>") != 1 || strings.Contains(text, "U0CAROL1") {
		t.Errorf("pending 应 @审批人、不 @提交人，且去重: %s", text)
	}
	ev := evPending
	ev.Name = EventPassed
	d.Notify(context.Background(), ev)
	text = r.bodies[1]["text"].(string)
	if !strings.Contains(text, "<@U0CAROL1>") || strings.Contains(text, "鲍勃") {
		t.Errorf("passed 应 @提交人: %s", text)
	}
	// 其他资源组里的同名权限组不会被 @（成员按工单所属资源组解析）
	ev.GroupID, ev.Name = 2, EventPending
	d.Notify(context.Background(), ev)
	if strings.Contains(r.bodies[2]["text"].(string), "@鲍勃") { // 审批人 bob 只属于资源组 1
		t.Errorf("跨资源组不应按审批人 @: %v", r.bodies[2])
	}
}

func TestMentionCap(t *testing.T) {
	var r recv
	srv := r.server("ok", 200)
	defer srv.Close()
	var rules []Rule
	for i := 0; i < 80; i++ {
		rules = append(rules, Rule{Target: TargetRaw, Value: "U0" + strconv.Itoa(1000+i)})
	}
	newDisp(Channel{Type: TypeSlack, WebhookURL: srv.URL, Enabled: true, Rules: rules}).Notify(context.Background(), evPending)
	if n := strings.Count(r.bodies[0]["text"].(string), "<@U0"); n != maxMentions {
		t.Errorf("单条消息最多 @ %d 人，实际 %d", maxMentions, n)
	}
}

func TestFailuresReportedWithoutLeakingURL(t *testing.T) {
	ding := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"errcode":310000,"errmsg":"keywords not in content"}`))
	}))
	defer ding.Close()
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":19021,"msg":"sign match fail"}`))
	}))
	defer feishu.Close()
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404); _, _ = w.Write([]byte("no_service")) }))
	defer slack.Close()
	d := newDisp()
	for _, c := range []struct {
		ch   Channel
		want string
	}{
		{Channel{Type: TypeDingTalk, WebhookURL: ding.URL + "/s3cr3t-token"}, "310000"},
		{Channel{Type: TypeLark, WebhookURL: feishu.URL}, "19021"},
		{Channel{Type: TypeSlack, WebhookURL: slack.URL + "/s3cr3t-token"}, "no_service"},
		{Channel{Type: TypeSlack, WebhookURL: "ftp://x/s3cr3t-token"}, "http(s)"},
		{Channel{Type: "bogus", WebhookURL: "http://x"}, "不支持"},
		{Channel{Type: TypeSlack, WebhookURL: "http://127.0.0.1:1/s3cr3t-token"}, "请求失败"},
	} {
		err := d.SendTest(context.Background(), c.ch, evPending)
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "s3cr3t-token") {
			t.Errorf("%s: %v", c.ch.Type, err)
		}
	}
	// Notify 吞掉错误，不 panic
	newDisp(Channel{Type: TypeSlack, WebhookURL: slack.URL, Enabled: true}).Notify(context.Background(), evPending)
}

func TestSendTestResolvesRequester(t *testing.T) {
	var r recv
	srv := r.server("ok", 200)
	defer srv.Close()
	ch := Channel{Type: TypeSlack, WebhookURL: srv.URL, Rules: []Rule{{Target: TargetApplicant}}}
	ev := Event{Name: EventPending, Title: "测试", Content: "x", Applicant: "alice"}
	if err := newDisp().SendTest(context.Background(), ch, ev); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.bodies[0]["text"].(string), "<@U0ALICE1>") {
		t.Errorf("测试消息应 @ 操作者: %v", r.bodies[0])
	}
}

func TestHelpers(t *testing.T) {
	if got := ParseIDs("10, 20,x,,30"); len(got) != 3 || got[2] != 30 {
		t.Errorf("ParseIDs: %v", got)
	}
	if got := SplitEvents(" pending , ,finished"); len(got) != 2 || got[1] != "finished" {
		t.Errorf("SplitEvents: %v", got)
	}
	if TypeLabel(TypeLark) != "Lark" || TypeLabel("x") != "x" {
		t.Error("TypeLabel")
	}
	_ = url.Values{}
}

func toStrings(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			out = append(out, x.(string))
		}
	}
	return out
}

// ExtraUsers 不依赖渠道的 @ 规则，只要渠道订阅了这个事件就会被 @（用于 DDL 执行完成时通知指定权限组）；与规则里的同一个人只 @ 一次。
func TestExtraUsersAlwaysMentioned(t *testing.T) {
	var r recv
	srv := r.server("ok", 200)
	defer srv.Close()
	ch := Channel{Type: TypeSlack, WebhookURL: srv.URL, Enabled: true, Events: []string{EventFinished}, Rules: []Rule{{Target: TargetUser, Value: "alice"}}}
	ev := Event{Kind: KindSQLWorkflow, Name: EventFinished, Title: "t", Content: "c", GroupID: 1, ExtraUsers: []string{"alice", "carol", "nobody"}}
	newDisp(ch).Notify(context.Background(), ev)
	text := r.bodies[0]["text"].(string)
	if strings.Count(text, "<@U0ALICE1>") != 1 || !strings.Contains(text, "<@U0CAROL1>") {
		t.Errorf("额外 @ 对象: %s", text)
	}
	// 渠道没订阅该事件：不发送，也就不会 @
	ev.Name = EventPending
	r.bodies = nil
	newDisp(ch).Notify(context.Background(), ev)
	if len(r.bodies) != 0 {
		t.Errorf("未订阅的事件不应发送: %v", r.bodies)
	}
}
