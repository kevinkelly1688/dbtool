// @author: kevinkelly
// @license: Apache Licence
// @file: notify_ext_test.go
// @time: 2026/10/28

package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"dbtool/internal/model"
	"dbtool/internal/notify"
)

func jsonField(t *testing.T, body string, path ...string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("响应不是 JSON: %s", body)
	}
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("路径 %v 不存在: %s", path, body)
		}
		v = m[p]
	}
	return v
}

// Slack 渠道的“渠道原生标识”可以直接粘贴 Slack 里的 @ 用户组 / 用户写法，保存时规整成 ID；其它平台不做这个转换。
func TestNotifySlackRawTargetAcceptsPastedMarkup(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")
	body := chanBody("https://hooks.slack.com/services/T1/B2/TOKEN")
	body["rules"] = []map[string]any{
		{"target": "raw", "value": "!subteam^S0ARY4FQATB"},
		{"target": "raw", "value": "<!subteam^S0ARY4FQATB|@dba>"},
		{"target": "raw", "value": "<@U012AB3CD>"},
		{"target": "raw", "value": "@here"},
	}
	w := root.do("POST", "/api/v1/settings/notify/channels", body)
	if w.Code != 200 {
		t.Fatalf("Slack 用户组写法应能保存: %d %s", w.Code, w.Body.String())
	}
	var got []string
	for _, r := range e.repo.NotifyChannels[0].Rules {
		got = append(got, r.Value)
	}
	if strings.Join(got, ",") != "S0ARY4FQATB,S0ARY4FQATB,U012AB3CD,here" {
		t.Errorf("应规整成 ID 保存: %v", got)
	}

	// 写法不对的仍然拒绝；其它平台不接受这种写法
	body["rules"] = []map[string]any{{"target": "raw", "value": "!subteam^"}}
	if w := root.do("POST", "/api/v1/settings/notify/channels", body); w.Code != 400 {
		t.Errorf("只有前缀没有 ID 应 400: %d %s", w.Code, w.Body.String())
	}
	body["name"] = "钉钉群"
	body["type"] = "dingtalk"
	body["url"] = "https://oapi.dingtalk.com/robot/send?access_token=x"
	body["rules"] = []map[string]any{{"target": "raw", "value": "!subteam^S0ARY4FQATB"}}
	if w := root.do("POST", "/api/v1/settings/notify/channels", body); w.Code != 400 {
		t.Errorf("钉钉不接受 Slack 的写法: %d %s", w.Code, w.Body.String())
	}
}

// webhook 主机必须匹配平台域名；原生 ID 按平台格式；渠道名唯一；更换类型必须重填地址。
func TestNotifyValidationHostRawIDNameAndType(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")
	base := func(f func(m map[string]any)) map[string]any {
		m := chanBody("https://hooks.slack.com/services/T1/B2/TOKEN")
		f(m)
		return m
	}
	for name, body := range map[string]map[string]any{
		"Slack 渠道填了钉钉域名": base(func(m map[string]any) { m["url"] = "https://oapi.dingtalk.com/robot/send?access_token=x" }),
		"域名只是后缀相似":       base(func(m map[string]any) { m["url"] = "https://hooks.slack.com.evil.example/services/x" }),
		"平台域名用了 http":    base(func(m map[string]any) { m["url"] = "http://hooks.slack.com/services/x" }),
		"飞书原生 ID 不是 ou_": base(func(m map[string]any) {
			m["type"] = "feishu"
			m["url"] = "https://open.feishu.cn/open-apis/bot/v2/hook/x"
			m["rules"] = []map[string]any{{"target": "raw", "value": "abc123"}}
		}),
		"Slack 原生 ID 太短": base(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "raw", "value": "u1"}} }),
		"值班人员不存在":        base(func(m map[string]any) { m["duty_users"] = []string{"ghost"} }),
		"资源组不存在（多选）":     base(func(m map[string]any) { m["group_ids"] = []uint{1, 99} }),
	} {
		if w := root.do("POST", "/api/v1/settings/notify/channels", body); w.Code != 400 {
			t.Errorf("%s 应 400: %d %s", name, w.Code, w.Body.String())
		}
	}
	ok := base(func(m map[string]any) {
		m["type"] = "dingtalk"
		m["url"] = "https://oapi.dingtalk.com/robot/send?access_token=x"
		m["rules"] = []map[string]any{{"target": "raw", "value": "13800000000"}, {"target": "dba_duty", "work_hours": true}}
		m["duty_users"] = []string{"leader"}
		m["events"] = []string{"submitted", "perf_overdue", "inspect_alert"}
	})
	w := root.do("POST", "/api/v1/settings/notify/channels", ok)
	if w.Code != 200 {
		t.Fatalf("合法配置: %d %s", w.Code, w.Body.String())
	}
	got := e.repo.NotifyChannels[0]
	if len(got.Rules) != 2 || !got.Rules[1].WorkHours || got.Rules[1].Target != "dba_duty" || got.Rules[1].Value != "" || len(got.Events) != 3 {
		t.Errorf("规则与事件应原样保存: %+v", got)
	}
	// 渠道名唯一（不区分大小写与首尾空白）
	dup := base(func(m map[string]any) {
		m["name"] = " DBA 群 "
		m["type"] = "dingtalk"
		m["url"] = "https://oapi.dingtalk.com/robot/send?access_token=y"
	})
	if w := root.do("POST", "/api/v1/settings/notify/channels", dup); w.Code != 400 || !strings.Contains(w.Body.String(), "已存在") {
		t.Errorf("同名渠道应 400: %d %s", w.Code, w.Body.String())
	}
	// 改自己不算重名；更换类型必须重填地址
	id := itoa(jsonField(t, w.Body.String(), "data", "id").(float64))
	_ = id
	self := base(func(m map[string]any) { m["type"] = "dingtalk"; m["url"] = "" })
	if w := root.do("PUT", "/api/v1/settings/notify/channels/1", self); w.Code != 200 {
		t.Errorf("同类型地址留空、名称不变应通过: %d %s", w.Code, w.Body.String())
	}
	toLark := base(func(m map[string]any) { m["type"] = "lark"; m["url"] = ""; m["rules"] = []map[string]any{} })
	if w := root.do("PUT", "/api/v1/settings/notify/channels/1", toLark); w.Code != 400 || !strings.Contains(w.Body.String(), "重新填写") {
		t.Errorf("更换类型要求重填地址: %d %s", w.Code, w.Body.String())
	}
	// 事件下拉：11 项
	list := root.do("GET", "/api/v1/settings/notify", nil).Body.String()
	for _, want := range []string{`"submitted"`, `"perf_overdue"`, `"inspect_alert"`, `"ext_ready":true`, `"log_ready":false`, "oapi.dingtalk.com"} {
		if !strings.Contains(list, want) {
			t.Errorf("概览缺少 %s: %s", want, list)
		}
	}
}

// 一个渠道关联多个资源组：group_id 保持第一个。
func TestNotifyMultiGroupKeepsFirstGroupID(t *testing.T) {
	e := newApplyEnv(t)
	e.repo.RGs = append(e.repo.RGs, model.ResourceGroup{GroupID: 2, GroupName: "数据组"})
	root := e.login(t, "root")
	body := chanBody("https://hooks.slack.com/services/T1/B2/TOKEN")
	body["group_id"] = 0
	body["group_ids"] = []uint{2, 1, 2}
	if w := root.do("POST", "/api/v1/settings/notify/channels", body); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if g := e.repo.NotifyChannels[0].GroupID; g != 2 {
		t.Errorf("group_id 应保持为第一个资源组: %d", g)
	}
	// 全局渠道
	body["name"], body["group_ids"] = "全局", []uint{}
	if w := root.do("POST", "/api/v1/settings/notify/channels", body); w.Code != 200 || e.repo.NotifyChannels[1].GroupID != 0 {
		t.Errorf("全局渠道: %d", w.Code)
	}
}

// 预览：返回各平台消息体与 @ 解析，不真发；原因里能看到没 @ 的规则。
func TestNotifyPreviewDoesNotSend(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")
	e.repo.Users[100] = model.User{ID: 100, Username: "root", Display: "管理员", SlackID: "U0ROOT123"}
	var h hookRecv
	srv := h.server("ok", 200)
	defer srv.Close()
	body := chanBody(srv.URL + "/hook/TOKEN123")
	body["events"] = []string{}
	body["event"] = "submitted"
	body["work_time"] = false
	body["reviewer_group_id"] = 10
	body["rules"] = []map[string]any{
		{"target": "applicant"},
		{"target": "reviewers", "work_hours": true},
		{"target": "raw", "value": "S0ARY4FQATB", "events": []string{"passed"}},
	}
	w := root.do("POST", "/api/v1/settings/notify/preview", body)
	if w.Code != 200 {
		t.Fatalf("预览: %d %s", w.Code, w.Body.String())
	}
	if len(h.bodies) != 0 {
		t.Fatal("预览不应发送请求")
	}
	s := w.Body.String()
	for _, want := range []string{"U0ROOT123", "新的工单申请", `"event":"submitted"`, "非工作时间", "只在「审批通过」时 @", `"platform":"slack"`} {
		if !strings.Contains(s, want) {
			t.Errorf("预览缺少 %q: %s", want, s)
		}
	}
	if strings.Contains(s, "TOKEN123") || strings.Contains(s, srv.URL) {
		t.Errorf("预览不应含真实地址: %s", s)
	}
	body["event"] = "nope"
	if w := root.do("POST", "/api/v1/settings/notify/preview", body); w.Code != 400 {
		t.Errorf("未知事件应 400: %d", w.Code)
	}
	if w := e.login(t, "dev").do("POST", "/api/v1/settings/notify/preview", body); w.Code != 403 {
		t.Errorf("普通用户不能预览: %d", w.Code)
	}
}

// 测试发送带 event：按真实逻辑模拟（规则里的事件过滤生效、非工作时间不 @）；不带 event 沿用旧行为。
func TestNotifyTestSendSimulatesEvent(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")
	e.repo.Users[100] = model.User{ID: 100, Username: "root", Display: "管理员", SlackID: "U0ROOT123"}
	var h hookRecv
	srv := h.server("ok", 200)
	defer srv.Close()
	body := chanBody(srv.URL + "/hook/TOKEN123")
	body["rules"] = []map[string]any{
		{"target": "applicant", "events": []string{"passed"}},
		{"target": "raw", "value": "S0ARY4FQATB", "work_hours": true},
	}
	body["event"] = "perf_overdue"
	body["work_time"] = false
	if w := root.do("POST", "/api/v1/settings/notify/test", body); w.Code != 200 || len(h.bodies) != 1 {
		t.Fatalf("模拟事件测试发送: %d %s", w.Code, w.Body.String())
	}
	text := h.bodies[0]["text"].(string)
	if strings.Contains(text, "<@U0ROOT123>") || strings.Contains(text, "S0ARY4FQATB") || !strings.Contains(text, "性能工单") || !strings.Contains(text, "[dbtool 测试]") {
		t.Errorf("perf_overdue 不属于规则的 passed 过滤、非工作时间不 @ 用户组: %s", text)
	}
	body["work_time"] = true
	body["event"] = "passed"
	h.bodies = nil
	_ = root.do("POST", "/api/v1/settings/notify/test", body)
	text = h.bodies[0]["text"].(string)
	if !strings.Contains(text, "<@U0ROOT123>") || !strings.Contains(text, "<!subteam^S0ARY4FQATB>") {
		t.Errorf("passed + 工作时间：两条规则都应 @: %s", text)
	}
	body["event"] = "nope"
	if w := root.do("POST", "/api/v1/settings/notify/test", body); w.Code != 400 {
		t.Errorf("未知事件: %d", w.Code)
	}
}

func TestNotifyToggleAndLogsDegrade(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")
	id, _ := e.repo.SaveChannel(0, notify.ChannelInput{GroupID: 1, Name: "x", Type: "slack", URL: "https://hooks.slack.com/services/a", Enabled: true,
		Events: []string{"pending"}, Rules: []notify.Rule{{Target: "all"}}})
	path := "/api/v1/settings/notify/channels/" + itoa(float64(id)) + "/toggle"
	if w := root.do("POST", path, map[string]any{"enabled": false}); w.Code != 200 || e.repo.NotifyChannels[0].Enabled {
		t.Fatalf("停用: %d %s", w.Code, w.Body.String())
	}
	if len(e.repo.NotifyChannels[0].Rules) != 1 || e.repo.NotifyChannels[0].Name != "x" {
		t.Errorf("启停不应改其它配置: %+v", e.repo.NotifyChannels[0])
	}
	if w := root.do("POST", path, map[string]any{"enabled": true}); w.Code != 200 || !e.repo.NotifyChannels[0].Enabled {
		t.Fatalf("启用: %d", w.Code)
	}
	if w := root.do("POST", "/api/v1/settings/notify/channels/999/toggle", map[string]any{"enabled": true}); w.Code != 404 {
		t.Errorf("不存在的渠道: %d", w.Code)
	}
	if w := e.login(t, "dev").do("POST", path, map[string]any{"enabled": true}); w.Code != http.StatusForbidden {
		t.Errorf("普通用户: %d", w.Code)
	}
	// 发送记录表不可用（假仓库没有 LogStore）：返回 ready=false 的空列表，不报错
	w := root.do("GET", "/api/v1/settings/notify/logs?channel_id=1", nil)
	if w.Code != 200 || jsonField(t, w.Body.String(), "data", "ready") != false {
		t.Errorf("日志降级: %d %s", w.Code, w.Body.String())
	}
}

// 端到端：新工单提交只发给订阅了 submitted 的渠道；订阅 pending 的旧渠道照旧收到；订阅 passed 的不收。
func TestSubmittedEventEndToEnd(t *testing.T) {
	e := newApplyEnv(t)
	e.repo.Users[1] = model.User{ID: 1, Username: "dev", Display: "开发者", SlackID: "U0DEV0001"}
	dev := e.login(t, "dev")
	var onlySubmitted, pending, passed hookRecv
	a, b, c := onlySubmitted.server("ok", 200), pending.server("ok", 200), passed.server("ok", 200)
	defer a.Close()
	defer b.Close()
	defer c.Close()
	for i, x := range []struct {
		url    string
		events []string
	}{{a.URL, []string{"submitted"}}, {b.URL, []string{"pending"}}, {c.URL, []string{"passed"}}} {
		_, _ = e.repo.SaveChannel(0, notify.ChannelInput{GroupID: 1, Name: "群" + string(rune('A'+i)), Type: "slack", URL: x.url, Enabled: true, Events: x.events,
			Rules: []notify.Rule{{Target: "applicant"}}})
	}
	_ = applyID(t, dev.do("POST", "/api/v1/query/privilege-applies", createBody()))
	waitFor(t, func() bool {
		onlySubmitted.mu.Lock()
		defer onlySubmitted.mu.Unlock()
		pending.mu.Lock()
		defer pending.mu.Unlock()
		return len(onlySubmitted.bodies) >= 1 && len(pending.bodies) >= 1
	})
	waitMs(200)
	passed.mu.Lock()
	defer passed.mu.Unlock()
	if len(passed.bodies) != 0 {
		t.Errorf("订阅 passed 的渠道不应收到新工单提交: %d", len(passed.bodies))
	}
}
