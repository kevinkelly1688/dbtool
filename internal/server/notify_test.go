// @author: kevinkelly
// @license: Apache Licence
// @file: notify_test.go
// @time: 2026/10/28

package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/notify"
)

type hookRecv struct {
	mu     sync.Mutex
	bodies []map[string]any
	paths  []string
}

func (h *hookRecv) server(resp string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		h.mu.Lock()
		h.bodies, h.paths = append(h.bodies, m), append(h.paths, r.URL.Path)
		h.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
}

func chanBody(url string) map[string]any {
	return map[string]any{
		"group_id": 1, "name": "DBA 群", "type": "slack", "url": url, "secret": "", "enabled": true,
		"events": []string{"pending", "passed"},
		"rules": []map[string]any{
			{"target": "reviewers", "events": []string{"pending"}},
			{"target": "applicant", "events": []string{"passed"}},
			{"target": "raw", "value": "S0ARY4FQATB"},
		},
	}
}

func TestNotifyAdminPermissionsAndValidation(t *testing.T) {
	e := newApplyEnv(t)
	dev, root := e.login(t, "dev"), e.login(t, "root")

	if w := dev.do("GET", "/api/v1/settings/notify", nil); w.Code != 403 {
		t.Errorf("普通用户不能访问渠道管理 API: %d", w.Code)
	}
	req := httptest.NewRequest("GET", "/settings/notify", nil)
	req.AddCookie(dev.cookie)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("普通用户不能打开渠道管理页: %d", rec.Code)
	}
	req = httptest.NewRequest("GET", "/settings/notify", nil)
	req.AddCookie(root.cookie)
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "通知渠道（机器人 / 邮件）") || !strings.Contains(rec.Body.String(), "notify.js") {
		t.Errorf("超级用户渠道管理页: %d", rec.Code)
	}

	w := root.do("GET", "/api/v1/settings/notify", nil)
	for _, want := range []string{`"ready":true`, `"dingtalk"`, `"lark"`, `"feishu"`, `"slack"`, "研发组", "组长", `"pending"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("概览缺少 %s: %s", want, w.Body.String())
		}
	}

	good := chanBody("https://hooks.slack.com/services/T1/B2/TOKEN")
	mod := func(f func(m map[string]any)) map[string]any {
		m := map[string]any{}
		b, _ := json.Marshal(good)
		_ = json.Unmarshal(b, &m)
		f(m)
		return m
	}
	bad := map[string]map[string]any{
		"名称为空":      mod(func(m map[string]any) { m["name"] = "  " }),
		"名称过长":      mod(func(m map[string]any) { m["name"] = strings.Repeat("长", 51) }),
		"未知类型":      mod(func(m map[string]any) { m["type"] = "wechat" }),
		"地址缺失":      mod(func(m map[string]any) { m["url"] = "" }),
		"地址不是 http": mod(func(m map[string]any) { m["url"] = "ftp://x/y" }),
		"资源组不存在":    mod(func(m map[string]any) { m["group_id"] = 99 }),
		"未知事件":      mod(func(m map[string]any) { m["events"] = []string{"nope"} }),
		"规则里未知事件":   mod(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "all", "events": []string{"nope"}}} }),
		"权限组不存在":    mod(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "auth_group", "value": "999"}} }),
		"用户名为空":     mod(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "user", "value": ""}} }),
		"原生标识非法":    mod(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "raw", "value": "a b<c>"}} }),
		"未知目标":      mod(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "boss"}} }),
	}
	for name, body := range bad {
		if w := root.do("POST", "/api/v1/settings/notify/channels", body); w.Code != 400 {
			t.Errorf("%s 应 400: %d %s", name, w.Code, w.Body.String())
		}
	}
	if len(e.repo.NotifyChannels) != 0 {
		t.Errorf("校验失败不应保存: %d", len(e.repo.NotifyChannels))
	}
}

func TestNotifyAdminCRUDHidesSecrets(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")
	body := chanBody("https://hooks.slack.com/services/T1/B2/TOPSECRETTOKEN")
	body["secret"] = "signing-secret"
	w := root.do("POST", "/api/v1/settings/notify/channels", body)
	if w.Code != 200 {
		t.Fatalf("创建: %d %s", w.Code, w.Body.String())
	}
	var created struct{ Data struct{ ID float64 } }
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id := itoa(created.Data.ID)

	list := root.do("GET", "/api/v1/settings/notify", nil).Body.String()
	if strings.Contains(list, "TOPSECRETTOKEN") || strings.Contains(list, "signing-secret") {
		t.Errorf("列表不应回显地址里的 token 和密钥: %s", list)
	}
	if !strings.Contains(list, "hooks.slack.com") || !strings.Contains(list, `"has_secret":true`) {
		t.Errorf("列表应显示主机名与是否加签: %s", list)
	}
	// 修改：URL、密钥留空表示保持原值
	upd := chanBody("")
	upd["name"] = "改名后"
	upd["enabled"] = false
	if w := root.do("PUT", "/api/v1/settings/notify/channels/"+id, upd); w.Code != 200 {
		t.Fatalf("修改: %d %s", w.Code, w.Body.String())
	}
	got := e.repo.NotifyChannels[0]
	if got.Name != "改名后" || got.Enabled || got.WebhookURL != "https://hooks.slack.com/services/T1/B2/TOPSECRETTOKEN" || got.Secret != "signing-secret" {
		t.Errorf("修改后: %+v", got)
	}
	// 清除密钥
	upd["clear_secret"] = true
	_ = root.do("PUT", "/api/v1/settings/notify/channels/"+id, upd)
	if e.repo.NotifyChannels[0].Secret != "" {
		t.Error("clear_secret 应清除密钥")
	}
	if w := root.do("PUT", "/api/v1/settings/notify/channels/999", upd); w.Code != 404 {
		t.Errorf("修改不存在的渠道: %d", w.Code)
	}
	if w := root.do("DELETE", "/api/v1/settings/notify/channels/"+id, nil); w.Code != 200 || len(e.repo.NotifyChannels) != 0 {
		t.Errorf("删除: %d", w.Code)
	}
}

func TestNotifyTestSend(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")
	e.repo.Users[100] = model.User{ID: 100, Username: "root", Display: "管理员", SlackID: "U0ROOT123"}

	var h hookRecv
	srv := h.server("ok", 200)
	defer srv.Close()
	body := chanBody(srv.URL + "/hook/TOKEN123")
	body["rules"] = []map[string]any{
		{"target": "applicant", "events": []string{"passed"}}, // 测试时忽略事件过滤
		{"target": "raw", "value": "S0ARY4FQATB"},
		{"target": "reviewers"}, // 测试里没有真实工单，解析不出，不会出错
	}
	w := root.do("POST", "/api/v1/settings/notify/test", body)
	if w.Code != 200 || len(h.bodies) != 1 {
		t.Fatalf("测试发送: %d %s", w.Code, w.Body.String())
	}
	text := h.bodies[0]["text"].(string)
	for _, want := range []string{"<@U0ROOT123>", "<!subteam^S0ARY4FQATB>", "通知渠道测试", "测试消息"} {
		if !strings.Contains(text, want) {
			t.Errorf("测试消息缺少 %q: %s", want, text)
		}
	}
	// 对端报错：返回 502，信息里没有 token
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404); _, _ = w.Write([]byte("no_service")) }))
	defer bad.Close()
	w = root.do("POST", "/api/v1/settings/notify/test", chanBody(bad.URL+"/hook/TOKEN123"))
	if w.Code != 502 || !strings.Contains(w.Body.String(), "send_failed") || strings.Contains(w.Body.String(), "TOKEN123") {
		t.Errorf("失败应 502 且不泄露 token: %d %s", w.Code, w.Body.String())
	}
	// 已保存的渠道：地址留空沿用
	id, _ := e.repo.SaveChannel(0, notify.ChannelInput{GroupID: 1, Name: "x", Type: "slack", URL: srv.URL + "/saved", Enabled: true})
	b2 := chanBody("")
	b2["id"] = id
	h.bodies = nil
	if w := root.do("POST", "/api/v1/settings/notify/test", b2); w.Code != 200 || len(h.bodies) != 1 {
		t.Errorf("沿用已保存地址测试: %d %s", w.Code, w.Body.String())
	}
}

// 端到端：权限申请的各个事件按渠道的 @ 规则发到机器人。
func TestApprovalEventsMentionPeople(t *testing.T) {
	e := newApplyEnv(t)
	e.repo.Users[1] = model.User{ID: 1, Username: "dev", Display: "开发者", SlackID: "U0DEV0001"}
	e.repo.Users[2] = model.User{ID: 2, Username: "leader", Display: "组长", SlackID: "U0LEAD001"}
	dev, leader := e.login(t, "dev"), e.login(t, "leader")

	var h hookRecv
	srv := h.server("ok", 200)
	defer srv.Close()
	_, _ = e.repo.SaveChannel(0, notify.ChannelInput{GroupID: 1, Name: "群", Type: "slack", URL: srv.URL, Enabled: true,
		Events: []string{"pending", "passed"},
		Rules:  []notify.Rule{{Target: "reviewers", Events: []string{"pending"}}, {Target: "applicant", Events: []string{"passed"}}, {Target: "raw", Value: "S0TEAM001"}}})

	id := applyID(t, dev.do("POST", "/api/v1/query/privilege-applies", createBody()))
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.bodies) >= 1 })
	first := h.bodies[0]["text"].(string)
	if !strings.Contains(first, "<@U0LEAD001>") || strings.Contains(first, "<@U0DEV0001>") || !strings.Contains(first, "<!subteam^S0TEAM001>") || !strings.Contains(first, "新的工单申请") {
		t.Errorf("新申请应 @当前审批人（组长），并 @ 固定的用户组: %s", first)
	}
	if w := leader.do("POST", "/api/v1/query/privilege-applies/"+itoa(id)+"/audit", map[string]string{"action": "pass", "remark": "ok"}); w.Code != 200 {
		t.Fatalf("审批: %d %s", w.Code, w.Body.String())
	}
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.bodies) >= 2 })
	second := h.bodies[1]["text"].(string)
	if !strings.Contains(second, "<@U0DEV0001>") || !strings.Contains(second, "审核通过") {
		t.Errorf("通过后应 @提交人: %s", second)
	}
	// 没订阅的事件（驳回）不发
	id2 := applyID(t, dev.do("POST", "/api/v1/query/privilege-applies", func() map[string]any { b := createBody(); b["table_list"] = []string{"refunds"}; return b }()))
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.bodies) >= 3 })
	n := len(h.bodies)
	_ = leader.do("POST", "/api/v1/query/privilege-applies/"+itoa(id2)+"/audit", map[string]string{"action": "reject", "remark": "no"})
	waitMs(300)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.bodies) != n {
		t.Errorf("渠道没有订阅 rejected，不应发送: %d -> %d", n, len(h.bodies))
	}
}

func waitMs(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

// waitFor 等待异步通知到达（通知在 goroutine 里发送）。
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		waitMs(20)
	}
	t.Fatal("等待超时：通知没有到达")
}
