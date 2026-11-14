// @author: kevinkelly
// @license: Apache Licence
// @file: notify_email_test.go
// @time: 2026/10/28

package server_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"dbtool/internal/model"
	"dbtool/internal/notify"
)

// miniSMTP 是测试用的最小 SMTP 服务器（明文、AUTH PLAIN 一律通过）：只捕获邮件，不验证协议细节
// （协议、加密、认证、证书的细致覆盖在 internal/notify/email_test.go）。
type miniSMTP struct {
	ln    net.Listener
	mu    sync.Mutex
	mails []miniMail
}

type miniMail struct {
	Rcpts []string
	Data  string
}

func startMiniSMTP(t *testing.T) *miniSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &miniSMTP{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *miniSMTP) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *miniSMTP) got() []miniMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]miniMail(nil), s.mails...)
}

func (s *miniSMTP) serve(c net.Conn) {
	defer c.Close()
	rd := bufio.NewReader(c)
	say := func(l string) { io.WriteString(c, l+"\r\n") }
	say("220 mini ESMTP")
	var cur miniMail
	for {
		l, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		up := strings.ToUpper(strings.TrimSpace(l))
		switch {
		case strings.HasPrefix(up, "EHLO"):
			say("250-mini\r\n250-AUTH PLAIN\r\n250 8BITMIME")
		case strings.HasPrefix(up, "AUTH"):
			say("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			cur = miniMail{}
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			i, j := strings.Index(l, "<"), strings.Index(l, ">")
			cur.Rcpts = append(cur.Rcpts, l[i+1:j])
			say("250 ok")
		case up == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				d, err := rd.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(d, "\r\n") == "." {
					break
				}
				b.WriteString(d)
			}
			cur.Data = b.String()
			s.mu.Lock()
			s.mails = append(s.mails, cur)
			s.mu.Unlock()
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func emailBody(port int) map[string]any {
	return map[string]any{
		"group_id": 1, "name": "邮件通知", "type": "email", "secret": "Sm7p-pass", "enabled": true,
		"events": []string{"pending", "passed"},
		"email": map[string]any{
			"host": "127.0.0.1", "port": port, "encryption": "none", "username": "mailer", "allow_plain_auth": true,
			"from_addr": "dbtool@example.com", "from_name": "DBA 工具", "to": []string{"ops@example.com"}, "cc": []string{"boss@example.com"},
			"subject_prefix": "[dbtool]",
		},
		"rules": []map[string]any{{"target": "reviewers", "events": []string{"pending"}}, {"target": "applicant", "events": []string{"passed"}}},
	}
}

func TestEmailChannelValidation(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")
	mod := func(f func(m, em map[string]any)) map[string]any {
		m := emailBody(2525)
		em := m["email"].(map[string]any)
		f(m, em)
		return m
	}
	many := make([]string, 21)
	for i := range many {
		many[i] = "u" + string(rune('a'+i)) + "@example.com"
	}
	bad := map[string]map[string]any{
		"缺少 SMTP 配置":    mod(func(m, em map[string]any) { delete(m, "email") }),
		"主机为空":          mod(func(m, em map[string]any) { em["host"] = "" }),
		"主机带协议":         mod(func(m, em map[string]any) { em["host"] = "smtp://mail.example.com" }),
		"端口越界":          mod(func(m, em map[string]any) { em["port"] = 70000 }),
		"未知加密方式":        mod(func(m, em map[string]any) { em["encryption"] = "tls13" }),
		"不加密却认证且未允许":    mod(func(m, em map[string]any) { em["allow_plain_auth"] = false }),
		"有密码没用户名":       mod(func(m, em map[string]any) { em["username"] = ""; em["allow_plain_auth"] = false }),
		"发件人不合法":        mod(func(m, em map[string]any) { em["from_addr"] = "nobody" }),
		"发件人带换行":        mod(func(m, em map[string]any) { em["from_addr"] = "a@example.com\r\nBcc: x@example.com" }),
		"显示名带换行":        mod(func(m, em map[string]any) { em["from_name"] = "x\r\nBcc: y@example.com" }),
		"收件人带换行":        mod(func(m, em map[string]any) { em["to"] = []string{"a@example.com\nBcc: y@example.com"} }),
		"收件人不是邮箱":       mod(func(m, em map[string]any) { em["to"] = []string{"张三"} }),
		"主题前缀带换行":       mod(func(m, em map[string]any) { em["subject_prefix"] = "x\ny" }),
		"固定收件人超过 20 个":  mod(func(m, em map[string]any) { em["to"] = many[:11]; em["cc"] = many[11:] }),
		"@规则里的原生标识不是邮箱": mod(func(m, em map[string]any) { m["rules"] = []map[string]any{{"target": "raw", "value": "U012AB3CD"}} }),
	}
	for name, body := range bad {
		if w := root.do("POST", "/api/v1/settings/notify/channels", body); w.Code != 400 {
			t.Errorf("%s 应 400: %d %s", name, w.Code, w.Body.String())
		}
	}
	if len(e.repo.NotifyChannels) != 0 {
		t.Fatalf("校验失败不应保存: %d", len(e.repo.NotifyChannels))
	}
	// 邮箱地址作为原生标识可以保存（含 + 号）
	ok := mod(func(m, em map[string]any) {
		m["rules"] = []map[string]any{{"target": "raw", "value": "dba+alert@example.com"}, {"target": "all"}}
	})
	if w := root.do("POST", "/api/v1/settings/notify/channels", ok); w.Code != 200 {
		t.Fatalf("合法邮件渠道: %d %s", w.Code, w.Body.String())
	}
}

func TestEmailChannelCRUDHidesPassword(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")
	w := root.do("POST", "/api/v1/settings/notify/channels", emailBody(2525))
	if w.Code != 200 {
		t.Fatalf("创建: %d %s", w.Code, w.Body.String())
	}
	var created struct{ Data struct{ ID float64 } }
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id := itoa(created.Data.ID)
	got := e.repo.NotifyChannels[0]
	if got.Type != "email" || got.Secret != "Sm7p-pass" || got.Email == nil || got.Email.Host != "127.0.0.1" || got.Email.SkipVerify || got.WebhookURL != "" {
		t.Fatalf("保存结果: %+v %+v", got, got.Email)
	}
	list := root.do("GET", "/api/v1/settings/notify", nil).Body.String()
	if strings.Contains(list, "Sm7p-pass") {
		t.Errorf("列表不应回显密码: %s", list)
	}
	for _, want := range []string{`"has_secret":true`, `"email":{`, `"from_addr":"dbtool@example.com"`, "127.0.0.1:2525", `"email_ready":true`, `"value":"email"`} {
		if !strings.Contains(list, want) {
			t.Errorf("列表缺少 %s: %s", want, list)
		}
	}
	// 修改：密码留空表示不改
	upd := emailBody(2525)
	upd["secret"] = ""
	upd["name"] = "改名"
	if w := root.do("PUT", "/api/v1/settings/notify/channels/"+id, upd); w.Code != 200 {
		t.Fatalf("修改: %d %s", w.Code, w.Body.String())
	}
	if c := e.repo.NotifyChannels[0]; c.Name != "改名" || c.Secret != "Sm7p-pass" {
		t.Errorf("留空不改: %+v", c)
	}
	// 清除密码（同时不再认证）
	upd["clear_secret"] = true
	upd["email"].(map[string]any)["username"] = ""
	if w := root.do("PUT", "/api/v1/settings/notify/channels/"+id, upd); w.Code != 200 || e.repo.NotifyChannels[0].Secret != "" {
		t.Errorf("清除密码: %d %s", w.Code, w.Body.String())
	}
	// 机器人改成邮件：旧的签名密钥不能变成 SMTP 密码
	hook := chanBody("https://hooks.slack.com/services/T1/B2/TOKEN")
	hook["secret"] = "signing-secret"
	w = root.do("POST", "/api/v1/settings/notify/channels", hook)
	var c2 struct{ Data struct{ ID float64 } }
	_ = json.Unmarshal(w.Body.Bytes(), &c2)
	toEmail := emailBody(2525)
	toEmail["secret"] = ""
	toEmail["email"].(map[string]any)["username"] = ""
	toEmail["name"] = "转邮件"
	if w := root.do("PUT", "/api/v1/settings/notify/channels/"+itoa(c2.Data.ID), toEmail); w.Code != 200 {
		t.Fatalf("机器人改邮件: %d %s", w.Code, w.Body.String())
	}
	if c := e.repo.NotifyChannels[1]; c.Type != "email" || c.Secret != "" || c.WebhookURL != "" {
		t.Errorf("签名密钥不应变成 SMTP 密码: %+v", c)
	}
	if w := root.do("DELETE", "/api/v1/settings/notify/channels/"+id, nil); w.Code != 200 {
		t.Errorf("删除: %d", w.Code)
	}
}

func TestEmailTestSend(t *testing.T) {
	e := newApplyEnv(t)
	dev, root := e.login(t, "dev"), e.login(t, "root")
	e.repo.Users[100] = model.User{ID: 100, Username: "root", Display: "管理员", Email: "root@example.com", IsActive: true}
	smtp := startMiniSMTP(t)

	body := emailBody(smtp.port())
	body["test_to"] = "me@example.com"
	if w := dev.do("POST", "/api/v1/settings/notify/test", body); w.Code != 403 {
		t.Errorf("普通用户不能测试发送: %d", w.Code)
	}
	if len(smtp.got()) != 0 {
		t.Fatal("无权限时不应发出邮件")
	}
	// 测试收件人格式校验
	for _, bad := range []string{"not-an-address", "a@example.com,b@example.com", "a@example.com\r\nBcc: x@example.com"} {
		b := emailBody(smtp.port())
		b["test_to"] = bad
		if w := root.do("POST", "/api/v1/settings/notify/test", b); w.Code != 400 {
			t.Errorf("测试收件人 %q 应 400: %d", bad, w.Code)
		}
	}
	// 成功：只发给测试收件人，返回各阶段，不含密码
	w := root.do("POST", "/api/v1/settings/notify/test", body)
	if w.Code != 200 {
		t.Fatalf("测试发送: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			OK     bool `json:"ok"`
			Stages []struct{ Name, Status, Detail string }
			To     string
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Data.OK || len(resp.Data.Stages) != 4 || resp.Data.Stages[0].Name != "connect" || resp.Data.Stages[3].Name != "send" || resp.Data.To != "me@example.com" {
		t.Fatalf("阶段结果: %s", w.Body.String())
	}
	for _, s := range resp.Data.Stages {
		if s.Status == "fail" {
			t.Errorf("阶段失败: %+v", s)
		}
	}
	if strings.Contains(w.Body.String(), "Sm7p-pass") {
		t.Errorf("响应不应含密码: %s", w.Body.String())
	}
	got := smtp.got()
	if len(got) != 1 || strings.Join(got[0].Rcpts, ",") != "me@example.com" || !strings.Contains(got[0].Data, "Subject:") {
		t.Fatalf("应只发给测试收件人: %+v", got)
	}
	// 连不上：200 + ok=false + 各阶段，不泄露密码
	bad := emailBody(1)
	bad["test_to"] = "me@example.com"
	w = root.do("POST", "/api/v1/settings/notify/test", bad)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":false`) || !strings.Contains(w.Body.String(), `"status":"fail"`) || strings.Contains(w.Body.String(), "Sm7p-pass") {
		t.Errorf("连接失败的结果: %d %s", w.Code, w.Body.String())
	}
	// 不指定测试收件人：按固定收件人与 @ 规则（提交人 = 当前操作者的邮箱）
	b3 := emailBody(smtp.port())
	b3["event"] = "passed"
	if w := root.do("POST", "/api/v1/settings/notify/test", b3); w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("按规则测试发送: %d %s", w.Code, w.Body.String())
	}
	g := smtp.got()
	last := strings.Join(g[len(g)-1].Rcpts, ",")
	if !strings.Contains(last, "ops@example.com") || !strings.Contains(last, "boss@example.com") || !strings.Contains(last, "root@example.com") {
		t.Errorf("应发给固定 To / Cc 与提交人（root）: %s", last)
	}
}

func TestEmailPreview(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")
	e.repo.Users[100] = model.User{ID: 100, Username: "root", Display: "管理员", IsActive: true} // 没有邮箱
	b := emailBody(2525)
	b["event"] = "passed"
	w := root.do("POST", "/api/v1/settings/notify/preview", b)
	if w.Code != 200 {
		t.Fatalf("预览: %d %s", w.Code, w.Body.String())
	}
	s := w.Body.String()
	for _, want := range []string{`"platform":"email"`, "ops@example.com", "boss@example.com", "管理员", "没有邮箱"} {
		if !strings.Contains(s, want) {
			t.Errorf("预览缺少 %q: %s", want, s)
		}
	}
	if strings.Contains(s, "Sm7p-pass") {
		t.Errorf("预览不应含密码")
	}
}

// 端到端：权限申请 -> 按“当前审批人”规则解析到审批人的邮箱 -> 发出邮件。
func TestApprovalEventsSendEmail(t *testing.T) {
	e := newApplyEnv(t)
	e.repo.Users[1] = model.User{ID: 1, Username: "dev", Display: "开发者", Email: "dev@example.com", IsActive: true}
	e.repo.Users[2] = model.User{ID: 2, Username: "leader", Display: "组长", Email: "leader@example.com", IsActive: true}
	dev, leader := e.login(t, "dev"), e.login(t, "leader")
	smtp := startMiniSMTP(t)
	_, err := e.repo.SaveChannel(0, notify.ChannelInput{GroupID: 1, Name: "邮件", Type: "email", Enabled: true, Events: []string{"pending", "passed"},
		Email: &notify.EmailConfig{Host: "127.0.0.1", Port: smtp.port(), Encryption: "none", FromAddr: "dbtool@example.com"},
		Rules: []notify.Rule{{Target: "reviewers", Events: []string{"pending"}}, {Target: "applicant", Events: []string{"passed"}}}})
	if err != nil {
		t.Fatal(err)
	}
	id := applyID(t, dev.do("POST", "/api/v1/query/privilege-applies", createBody()))
	waitFor(t, func() bool { return len(smtp.got()) >= 1 })
	first := smtp.got()[0]
	if strings.Join(first.Rcpts, ",") != "leader@example.com" || !strings.Contains(first.Data, "Subject:") {
		t.Errorf("新申请应发给当前审批人（组长）: %+v", first.Rcpts)
	}
	if w := leader.do("POST", "/api/v1/query/privilege-applies/"+itoa(id)+"/audit", map[string]string{"action": "pass", "remark": "ok"}); w.Code != 200 {
		t.Fatalf("审批: %d %s", w.Code, w.Body.String())
	}
	waitFor(t, func() bool { return len(smtp.got()) >= 2 })
	if second := smtp.got()[1]; strings.Join(second.Rcpts, ",") != "dev@example.com" {
		t.Errorf("通过后应发给提交人: %+v", second.Rcpts)
	}
	// 没订阅的事件（驳回）不发
	id2 := applyID(t, dev.do("POST", "/api/v1/query/privilege-applies", func() map[string]any { b := createBody(); b["table_list"] = []string{"refunds"}; return b }()))
	waitFor(t, func() bool { return len(smtp.got()) >= 3 })
	n := len(smtp.got())
	_ = leader.do("POST", "/api/v1/query/privilege-applies/"+itoa(id2)+"/audit", map[string]string{"action": "reject", "remark": "no"})
	waitMs(300)
	if len(smtp.got()) != n {
		t.Errorf("渠道没有订阅 rejected，不应发送: %d -> %d", n, len(smtp.got()))
	}
}
