// @author: kevinkelly
// @license: Apache Licence
// @file: notify_wecom_test.go
// @time: 2026/10/28

package server_test

import (
	"strings"
	"testing"

	"dbtool/internal/model"
	"dbtool/internal/notify"
)

const wecomHook = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=KEY-123"

// wecomChan 是一份合法的企业微信渠道配置（在 chanBody 的基础上改）。
func wecomChan(f func(m map[string]any)) map[string]any {
	m := chanBody(wecomHook)
	m["type"], m["name"] = "wecom", "企业微信群"
	m["rules"] = []map[string]any{{"target": "raw", "value": "zhangsan"}, {"target": "raw", "value": "13800138000"}, {"target": "all"}}
	if f != nil {
		f(m)
	}
	return m
}

// 企业微信渠道：地址主机必须是 qyapi.weixin.qq.com（https）；原生标识是成员 userid 或 11 位手机号；没有加签，不保存签名密钥。
func TestNotifyWeComValidationAndSave(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")

	list := root.do("GET", "/api/v1/settings/notify", nil).Body.String()
	for _, want := range []string{`"wecom"`, "企业微信", "qyapi.weixin.qq.com"} {
		if !strings.Contains(list, want) {
			t.Errorf("概览缺少 %s: %s", want, list)
		}
	}

	for name, body := range map[string]map[string]any{
		"企业微信渠道填了 Slack 域名": wecomChan(func(m map[string]any) { m["url"] = "https://hooks.slack.com/services/x" }),
		"Slack 渠道填了企业微信域名":  chanBody(wecomHook),
		"钉钉渠道填了企业微信域名":      chanBody(wecomHook), // type 在下面改
		"企业微信地址用了 http":     wecomChan(func(m map[string]any) { m["url"] = "http://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=x" }),
		"域名只是后缀相似": wecomChan(func(m map[string]any) {
			m["url"] = "https://qyapi.weixin.qq.com.evil.example/cgi-bin/webhook/send?key=x"
		}),
		"原生标识有空格":             wecomChan(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "raw", "value": "a b"}} }),
		"原生标识带冒号（userid 里没有）": wecomChan(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "raw", "value": "ou:abc"}} }),
		"原生标识超过 64 位": wecomChan(func(m map[string]any) {
			m["rules"] = []map[string]any{{"target": "raw", "value": strings.Repeat("a", 65)}}
		}),
		"原生标识是 Slack 用户组写法（有尖括号）": wecomChan(func(m map[string]any) {
			m["rules"] = []map[string]any{{"target": "raw", "value": "<!subteam^S0ARY4FQATB>"}}
		}),
	} {
		if strings.HasPrefix(name, "钉钉渠道") {
			body["type"] = "dingtalk"
		}
		if w := root.do("POST", "/api/v1/settings/notify/channels", body); w.Code != 400 {
			t.Errorf("%s 应 400: %d %s", name, w.Code, w.Body.String())
		}
	}
	if len(e.repo.NotifyChannels) != 0 {
		t.Fatalf("校验失败不应保存: %d", len(e.repo.NotifyChannels))
	}

	// 地址写错域名时的提示要说清楚该填什么
	w := root.do("POST", "/api/v1/settings/notify/channels", wecomChan(func(m map[string]any) { m["url"] = "https://oapi.dingtalk.com/robot/send?access_token=x" }))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "企业微信") || !strings.Contains(w.Body.String(), "https://qyapi.weixin.qq.com/") {
		t.Errorf("提示应写明企业微信机器人的地址前缀: %d %s", w.Code, w.Body.String())
	}
	// 原生标识的提示里说清楚格式
	w = root.do("POST", "/api/v1/settings/notify/channels", wecomChan(func(m map[string]any) { m["rules"] = []map[string]any{{"target": "raw", "value": "ou:abc"}} }))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "成员账号 userid") {
		t.Errorf("提示应写明原生标识的格式: %d %s", w.Code, w.Body.String())
	}

	// 合法配置：企业微信没有加签，填了签名密钥也不保存；规则原样保存
	w = root.do("POST", "/api/v1/settings/notify/channels", wecomChan(func(m map[string]any) { m["secret"] = "不会保存的密钥" }))
	if w.Code != 200 {
		t.Fatalf("合法配置: %d %s", w.Code, w.Body.String())
	}
	got := e.repo.NotifyChannels[0]
	if got.Type != notify.TypeWeCom || got.Secret != "" || got.WebhookURL != wecomHook || len(got.Rules) != 3 || got.Rules[0].Value != "zhangsan" || got.Rules[2].Target != "all" {
		t.Errorf("保存结果: %+v", got)
	}
	list = root.do("GET", "/api/v1/settings/notify", nil).Body.String()
	if strings.Contains(list, "KEY-123") || strings.Contains(list, `"has_secret":true`) || !strings.Contains(list, "qyapi.weixin.qq.com") {
		t.Errorf("列表只显示主机名，不回显 key，也没有加签标记: %s", list)
	}
}

// 从钉钉 / 飞书 / Slack 改成企业微信：要重填地址；旧的签名密钥一并清掉（企业微信用不到）。
func TestNotifyWeComDropsOldSecretWhenTypeChanges(t *testing.T) {
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
	to := wecomChan(func(m map[string]any) { m["url"] = ""; m["name"] = "DBA 群" })
	if w := root.do("PUT", "/api/v1/settings/notify/channels/1", to); w.Code != 400 || !strings.Contains(w.Body.String(), "重新填写") {
		t.Errorf("更换类型要求重填地址: %d %s", w.Code, w.Body.String())
	}
	to["url"] = wecomHook
	if w := root.do("PUT", "/api/v1/settings/notify/channels/1", to); w.Code != 200 {
		t.Fatalf("改成企业微信: %d %s", w.Code, w.Body.String())
	}
	got := e.repo.NotifyChannels[0]
	if got.Type != "wecom" || got.Secret != "" || got.WebhookURL != wecomHook {
		t.Errorf("旧的签名密钥应被清掉: %+v", got)
	}
}

// 测试发送、预览：走企业微信的消息格式；对端返回 errcode ≠ 0 算失败，错误里没有 key。
func TestNotifyWeComTestSendAndPreview(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")
	e.repo.Users[100] = model.User{ID: 100, Username: "root", Display: "管理员"}

	var h hookRecv
	srv := h.server(`{"errcode":0,"errmsg":"ok"}`, 200)
	defer srv.Close()
	body := wecomChan(func(m map[string]any) {
		m["url"] = srv.URL + "/cgi-bin/webhook/send?key=KEY-123"
		m["rules"] = []map[string]any{{"target": "applicant"}, {"target": "raw", "value": "zhangsan"}, {"target": "raw", "value": "13800138000"}, {"target": "all"}}
	})
	w := root.do("POST", "/api/v1/settings/notify/test", body)
	if w.Code != 200 || len(h.bodies) != 1 {
		t.Fatalf("测试发送: %d %s", w.Code, w.Body.String())
	}
	b := h.bodies[0]
	text, _ := b["text"].(map[string]any)
	content, _ := text["content"].(string)
	if b["msgtype"] != "text" || !strings.Contains(content, "@管理员") || !strings.Contains(content, "通知渠道测试") || strings.Contains(content, "签名密钥") {
		t.Errorf("测试消息: %+v", b)
	}
	ids, _ := text["mentioned_list"].([]any)
	mobiles, _ := text["mentioned_mobile_list"].([]any)
	if len(ids) != 2 || ids[0] != "@all" || ids[1] != "zhangsan" || len(mobiles) != 1 || mobiles[0] != "13800138000" {
		t.Errorf("@ 列表: %v %v", ids, mobiles)
	}

	// 预览：不发请求；平台用户给出企业微信专用的说明；没有加签
	h.bodies = nil
	body["event"] = "submitted"
	w = root.do("POST", "/api/v1/settings/notify/preview", body)
	if w.Code != 200 || len(h.bodies) != 0 {
		t.Fatalf("预览: %d %s", w.Code, w.Body.String())
	}
	if jsonField(t, w.Body.String(), "data", "platform") != "wecom" || jsonField(t, w.Body.String(), "data", "signed") != false {
		t.Errorf("预览的平台 / 加签: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "企业微信渠道不能按平台用户 @") || !strings.Contains(w.Body.String(), `\"msgtype\":\"text\"`) && !strings.Contains(w.Body.String(), `"msgtype":"text"`) {
		t.Errorf("预览里应有企业微信的说明和请求体: %s", w.Body.String())
	}

	// 对端返回业务错误：502，错误里没有 key
	bad := h.server(`{"errcode":93000,"errmsg":"invalid webhook url"}`, 200)
	defer bad.Close()
	body["url"] = bad.URL + "/cgi-bin/webhook/send?key=KEY-123"
	w = root.do("POST", "/api/v1/settings/notify/test", body)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "企业微信返回错误 93000") || strings.Contains(w.Body.String(), "KEY-123") {
		t.Errorf("业务错误应 502 且不泄露 key: %d %s", w.Code, w.Body.String())
	}
}
