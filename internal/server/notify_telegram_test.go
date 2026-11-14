// @author: kevinkelly
// @license: Apache Licence
// @file: notify_telegram_test.go
// @time: 2026/10/28

package server_test

import (
	"strings"
	"testing"

	"dbtool/internal/model"
	"dbtool/internal/notify"
)

const (
	tgToken = "123456789:AAE-abcdefghijklmnopqrstuvwxyz0123"
	tgHook  = "https://api.telegram.org/bot" + tgToken + "/sendMessage?chat_id=-1001234567890"
)

// telegramChan 是一份合法的 Telegram 渠道配置（在 chanBody 的基础上改）。
func telegramChan(f func(m map[string]any)) map[string]any {
	m := chanBody(tgHook)
	m["type"], m["name"] = "telegram", "Telegram 运维群"
	m["rules"] = []map[string]any{{"target": "raw", "value": "@dba_wang"}, {"target": "raw", "value": "123456789"}, {"target": "applicant"}}
	if f != nil {
		f(m)
	}
	return m
}

// Telegram 渠道：地址主机必须是 api.telegram.org（https），路径是 /bot<Bot Token>/sendMessage 并带 chat_id；
// 原生标识是用户名（可带 @）或数字用户 ID；没有“所有人”；没有加签，不保存签名密钥。
func TestNotifyTelegramValidationAndSave(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")

	list := root.do("GET", "/api/v1/settings/notify", nil).Body.String()
	for _, want := range []string{`"telegram"`, "Telegram", "api.telegram.org"} {
		if !strings.Contains(list, want) {
			t.Errorf("概览缺少 %s: %s", want, list)
		}
	}

	for name, body := range map[string]map[string]any{
		"Telegram 渠道填了 Slack 域名": telegramChan(func(m map[string]any) { m["url"] = "https://hooks.slack.com/services/x" }),
		"Slack 渠道填了 Telegram 地址": chanBody(tgHook),
		"Telegram 地址用了 http": telegramChan(func(m map[string]any) {
			m["url"] = "http://api.telegram.org/bot" + tgToken + "/sendMessage?chat_id=-100"
		}),
		"域名只是后缀相似": telegramChan(func(m map[string]any) {
			m["url"] = "https://api.telegram.org.evil.example/bot" + tgToken + "/sendMessage?chat_id=-100"
		}),
		"路径不是 sendMessage": telegramChan(func(m map[string]any) { m["url"] = "https://api.telegram.org/bot" + tgToken + "/getMe?chat_id=-100" }),
		"Bot Token 格式不对":   telegramChan(func(m map[string]any) { m["url"] = "https://api.telegram.org/botABC/sendMessage?chat_id=-100" }),
		"缺少 chat_id":       telegramChan(func(m map[string]any) { m["url"] = "https://api.telegram.org/bot" + tgToken + "/sendMessage" }),
		"chat_id 不是数字也不是 @频道名": telegramChan(func(m map[string]any) {
			m["url"] = "https://api.telegram.org/bot" + tgToken + "/sendMessage?chat_id=abc"
		}),
		"话题 ID 不是数字":         telegramChan(func(m map[string]any) { m["url"] = tgHook + "&message_thread_id=x" }),
		"原生标识有空格":            telegramChan(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "raw", "value": "a b"}} }),
		"原生标识太短（用户名至少 5 位）":  telegramChan(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "raw", "value": "abcd"}} }),
		"原生标识以数字开头又不是纯数字":    telegramChan(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "raw", "value": "1abcde"}} }),
		"原生标识是飞书 open_id 写法": telegramChan(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "raw", "value": "ou-abc"}} }),
		"原生标识是 Slack 用户组写法（尖括号）": telegramChan(func(m map[string]any) {
			m["rules"] = []map[string]any{{"target": "raw", "value": "<!subteam^S0ARY4FQATB>"}}
		}),
		"Telegram 没有“所有人”": telegramChan(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "all"}} }),
	} {
		if w := root.do("POST", "/api/v1/settings/notify/channels", body); w.Code != 400 {
			t.Errorf("%s 应 400: %d %s", name, w.Code, w.Body.String())
		}
	}
	if len(e.repo.NotifyChannels) != 0 {
		t.Fatalf("校验失败不应保存: %d", len(e.repo.NotifyChannels))
	}

	// 提示要说清楚该怎么填
	for name, c := range map[string]struct {
		body map[string]any
		want []string
	}{
		"域名写错":      {telegramChan(func(m map[string]any) { m["url"] = "https://oapi.dingtalk.com/robot/send?access_token=x" }), []string{"Telegram", "https://api.telegram.org/"}},
		"路径写错":      {telegramChan(func(m map[string]any) { m["url"] = "https://api.telegram.org/bot" + tgToken + "/getMe?chat_id=-1" }), []string{"/sendMessage?chat_id="}},
		"缺 chat_id": {telegramChan(func(m map[string]any) { m["url"] = "https://api.telegram.org/bot" + tgToken + "/sendMessage" }), []string{"缺少 chat_id", "-1001234567890"}},
		"所有人":       {telegramChan(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "all"}} }), []string{"没有“@所有人”", "渠道原生标识"}},
		"原生标识":      {telegramChan(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "raw", "value": "abcd"}} }), []string{"Telegram 用户名", "数字用户 ID"}},
	} {
		w := root.do("POST", "/api/v1/settings/notify/channels", c.body)
		for _, want := range c.want {
			if w.Code != 400 || !strings.Contains(w.Body.String(), want) {
				t.Errorf("%s：提示应含 %q: %d %s", name, want, w.Code, w.Body.String())
			}
		}
		if strings.Contains(w.Body.String(), tgToken) {
			t.Errorf("%s：错误里不应带 Bot Token: %s", name, w.Body.String())
		}
	}

	// 合法配置：没有加签，填了签名密钥也不保存；@用户名 规整成用户名；带话题 ID 的地址原样保存
	w := root.do("POST", "/api/v1/settings/notify/channels", telegramChan(func(m map[string]any) {
		m["secret"], m["url"] = "不会保存的密钥", tgHook+"&message_thread_id=7"
	}))
	if w.Code != 200 {
		t.Fatalf("合法配置: %d %s", w.Code, w.Body.String())
	}
	got := e.repo.NotifyChannels[0]
	if got.Type != notify.TypeTelegram || got.Secret != "" || got.WebhookURL != tgHook+"&message_thread_id=7" || len(got.Rules) != 3 || got.Rules[0].Value != "dba_wang" || got.Rules[1].Value != "123456789" || got.Rules[2].Target != "applicant" {
		t.Errorf("保存结果: %+v", got)
	}
	list = root.do("GET", "/api/v1/settings/notify", nil).Body.String()
	if strings.Contains(list, tgToken) || strings.Contains(list, `"has_secret":true`) || !strings.Contains(list, "api.telegram.org") {
		t.Errorf("列表只显示主机名，不回显 Bot Token，也没有加签标记: %s", list)
	}

	// 公开频道可以写 @频道名
	if w := root.do("POST", "/api/v1/settings/notify/channels", telegramChan(func(m map[string]any) {
		m["name"], m["url"] = "公开频道", "https://api.telegram.org/bot"+tgToken+"/sendMessage?chat_id=@dba_channel"
	})); w.Code != 200 {
		t.Errorf("@频道名: %d %s", w.Code, w.Body.String())
	}
}

// 从钉钉 / 飞书 / Slack 改成 Telegram：要重填地址；旧的签名密钥一并清掉（Telegram 用不到）。
func TestNotifyTelegramDropsOldSecretWhenTypeChanges(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")
	ding := chanBody("https://oapi.dingtalk.com/robot/send?access_token=x")
	ding["type"], ding["secret"], ding["rules"] = "dingtalk", "SECdingding", []map[string]any{}
	if w := root.do("POST", "/api/v1/settings/notify/channels", ding); w.Code != 200 {
		t.Fatalf("钉钉渠道: %d %s", w.Code, w.Body.String())
	}
	if e.repo.NotifyChannels[0].Secret != "SECdingding" {
		t.Fatalf("钉钉的签名密钥应保存: %+v", e.repo.NotifyChannels[0])
	}
	to := telegramChan(func(m map[string]any) { m["url"] = ""; m["name"] = "DBA 群" })
	if w := root.do("PUT", "/api/v1/settings/notify/channels/1", to); w.Code != 400 || !strings.Contains(w.Body.String(), "重新填写") {
		t.Errorf("更换类型要求重填地址: %d %s", w.Code, w.Body.String())
	}
	to["url"] = tgHook
	if w := root.do("PUT", "/api/v1/settings/notify/channels/1", to); w.Code != 200 {
		t.Fatalf("改成 Telegram: %d %s", w.Code, w.Body.String())
	}
	got := e.repo.NotifyChannels[0]
	if got.Type != "telegram" || got.Secret != "" || got.WebhookURL != tgHook {
		t.Errorf("旧的签名密钥应被清掉: %+v", got)
	}
}

// 测试发送、预览：走 Telegram 的消息格式（HTML 文本，chat_id 在请求体里）；对端返回错误算失败，错误里没有 Bot Token。
func TestNotifyTelegramTestSendAndPreview(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")
	e.repo.Users[100] = model.User{ID: 100, Username: "root", Display: "管理员"}

	var h hookRecv
	srv := h.server(`{"ok":true,"result":{"message_id":1}}`, 200)
	defer srv.Close()
	body := telegramChan(func(m map[string]any) {
		m["url"] = srv.URL + "/bot" + tgToken + "/sendMessage?chat_id=-1001234567890&message_thread_id=7"
		m["rules"] = []map[string]any{{"target": "applicant"}, {"target": "raw", "value": "dba_wang"}, {"target": "raw", "value": "123456789"}}
	})
	w := root.do("POST", "/api/v1/settings/notify/test", body)
	if w.Code != 200 || len(h.bodies) != 1 {
		t.Fatalf("测试发送: %d %s", w.Code, w.Body.String())
	}
	b := h.bodies[0]
	text, _ := b["text"].(string)
	if h.paths[0] != "/bot"+tgToken+"/sendMessage" || b["chat_id"] != float64(-1001234567890) || b["message_thread_id"] != float64(7) || b["parse_mode"] != "HTML" {
		t.Errorf("请求: %v %v", h.paths, b)
	}
	if !strings.HasPrefix(text, "@管理员 @dba_wang <a href=\"tg://user?id=123456789\">@123456789</a>\n") || !strings.Contains(text, "通知渠道测试") || strings.Contains(text, "签名密钥") {
		t.Errorf("测试消息: %q", text)
	}

	// 预览：不发请求；平台用户给出 Telegram 专用的说明；没有加签；请求体不带 chat_id（用占位地址）
	h.bodies = nil
	body["event"] = "submitted"
	w = root.do("POST", "/api/v1/settings/notify/preview", body)
	if w.Code != 200 || len(h.bodies) != 0 {
		t.Fatalf("预览: %d %s", w.Code, w.Body.String())
	}
	if jsonField(t, w.Body.String(), "data", "platform") != "telegram" || jsonField(t, w.Body.String(), "data", "signed") != false {
		t.Errorf("预览的平台 / 加签: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Telegram 渠道不能按平台用户 @") || !strings.Contains(w.Body.String(), "parse_mode") || strings.Contains(w.Body.String(), tgToken) {
		t.Errorf("预览里应有 Telegram 的说明和请求体，且没有 Bot Token: %s", w.Body.String())
	}

	// 对端返回错误：502，错误里没有 Bot Token
	bad := h.server(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`, 400)
	defer bad.Close()
	body["url"] = bad.URL + "/bot" + tgToken + "/sendMessage?chat_id=-1001234567890"
	w = root.do("POST", "/api/v1/settings/notify/test", body)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "chat not found") || strings.Contains(w.Body.String(), tgToken) {
		t.Errorf("对端错误应 502 且不泄露 Bot Token: %d %s", w.Code, w.Body.String())
	}
}
