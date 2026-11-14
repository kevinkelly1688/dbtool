// @author: kevinkelly
// @license: Apache Licence
// @file: email_test.go
// @time: 2026/10/28

package notify

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"log/slog"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- 进程内假 SMTP 服务器：监听 127.0.0.1:0，EHLO / STARTTLS / AUTH PLAIN & LOGIN / MAIL / RCPT / DATA / QUIT，捕获邮件 ----

type capturedMail struct {
	From   string
	Rcpts  []string
	Data   string
	TLS    bool
	Authed string // 认证用的用户名（没认证为空）
}

type fakeSMTP struct {
	ln   net.Listener
	cert tls.Certificate
	pool *x509.CertPool

	ssl        bool     // 连接即 TLS
	starttls   bool     // 通告并支持 STARTTLS
	auth       []string // 通告的认证机制（PLAIN / LOGIN）；空 = 不通告 AUTH
	authOnTLS  bool     // 只在 TLS 之后通告 AUTH（多数真实服务器的行为）
	user, pass string
	rejectRcpt map[string]bool
	hangGreet  bool   // 接受连接但不发问候
	hangData   bool   // DATA 之后不回应
	badAuthMsg string // 认证失败时的响应文本（可含密码，测试抹除）

	mu    sync.Mutex
	mails []capturedMail
	conns int32
	auths []string // 收到的 AUTH 命令原文（PLAIN 的凭据 / LOGIN 的各行）
}

func newCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake-smtp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func startSMTP(t *testing.T, mod func(*fakeSMTP)) *fakeSMTP {
	t.Helper()
	s := &fakeSMTP{user: "mailer", pass: "S3cr3t!pw", rejectRcpt: map[string]bool{}}
	s.cert, s.pool = newCert(t)
	if mod != nil {
		mod(s)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&s.conns, 1)
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakeSMTP) port() int      { return s.ln.Addr().(*net.TCPAddr).Port }
func (s *fakeSMTP) connCount() int { return int(atomic.LoadInt32(&s.conns)) }
func (s *fakeSMTP) got() []capturedMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]capturedMail(nil), s.mails...)
}

func (s *fakeSMTP) tlsConfig() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{s.cert}}
}

func (s *fakeSMTP) serve(raw net.Conn) {
	defer raw.Close()
	var conn net.Conn = raw
	isTLS := false
	if s.ssl {
		tc := tls.Server(raw, s.tlsConfig())
		if err := tc.Handshake(); err != nil {
			return
		}
		conn, isTLS = tc, true
	}
	if s.hangGreet {
		time.Sleep(3 * time.Second)
		return
	}
	rd := bufio.NewReader(conn)
	say := func(f string) { io.WriteString(conn, f+"\r\n") }
	line := func() (string, bool) {
		l, err := rd.ReadString('\n')
		return strings.TrimRight(l, "\r\n"), err == nil
	}
	say("220 fake.test ESMTP ready")
	var cur capturedMail
	authed := ""
	for {
		l, ok := line()
		if !ok {
			return
		}
		up := strings.ToUpper(l)
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			exts := []string{"fake.test", "8BITMIME"}
			if s.starttls && !isTLS {
				exts = append(exts, "STARTTLS")
			}
			if len(s.auth) > 0 && (isTLS || !s.authOnTLS) {
				exts = append(exts, "AUTH "+strings.Join(s.auth, " "))
			}
			for i, e := range exts {
				sep := "-"
				if i == len(exts)-1 {
					sep = " "
				}
				say("250" + sep + e)
			}
		case up == "STARTTLS":
			if !s.starttls || isTLS {
				say("503 no")
				continue
			}
			say("220 go ahead")
			tc := tls.Server(conn, s.tlsConfig())
			if err := tc.Handshake(); err != nil {
				return
			}
			conn, isTLS = tc, true
			rd = bufio.NewReader(conn)
		case strings.HasPrefix(up, "AUTH PLAIN"):
			arg := strings.TrimSpace(l[len("AUTH PLAIN"):])
			if arg == "" {
				say("334 ")
				arg, _ = line()
			}
			s.recordAuth("PLAIN " + arg)
			b, _ := base64.StdEncoding.DecodeString(arg)
			parts := strings.Split(string(b), "\x00")
			if len(parts) == 3 && parts[1] == s.user && parts[2] == s.pass {
				authed = parts[1]
				say("235 2.7.0 ok")
			} else {
				s.authFail(say)
			}
		case strings.HasPrefix(up, "AUTH LOGIN"):
			say("334 VXNlcm5hbWU6")
			u, _ := line()
			say("334 UGFzc3dvcmQ6")
			p, _ := line()
			s.recordAuth("LOGIN " + u + " " + p)
			ub, _ := base64.StdEncoding.DecodeString(u)
			pb, _ := base64.StdEncoding.DecodeString(p)
			if string(ub) == s.user && string(pb) == s.pass {
				authed = s.user
				say("235 2.7.0 ok")
			} else {
				s.authFail(say)
			}
		case strings.HasPrefix(up, "MAIL FROM:"):
			if len(s.auth) > 0 && s.user != "" && authed == "" && s.requireAuth() {
				say("530 5.7.0 Authentication required")
				continue
			}
			cur = capturedMail{From: between(l), TLS: isTLS, Authed: authed}
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			a := between(l)
			if s.rejectRcpt[a] {
				say("550 5.1.1 no such user")
				continue
			}
			cur.Rcpts = append(cur.Rcpts, a)
			say("250 ok")
		case up == "DATA":
			say("354 end with <CRLF>.<CRLF>")
			if s.hangData {
				time.Sleep(3 * time.Second)
				return
			}
			var b strings.Builder
			for {
				d, ok := line()
				if !ok {
					return
				}
				if d == "." {
					break
				}
				d = strings.TrimPrefix(d, ".") // 透明传输：行首多出的点
				b.WriteString(d + "\r\n")
			}
			cur.Data = b.String()
			s.mu.Lock()
			s.mails = append(s.mails, cur)
			s.mu.Unlock()
			say("250 2.0.0 queued")
		case up == "RSET", up == "NOOP":
			say("250 ok")
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("502 unknown")
		}
	}
}

func (s *fakeSMTP) requireAuth() bool { return s.user != "" && len(s.auth) > 0 }

func (s *fakeSMTP) recordAuth(a string) { s.mu.Lock(); s.auths = append(s.auths, a); s.mu.Unlock() }

func (s *fakeSMTP) authFail(say func(string)) {
	if s.badAuthMsg != "" {
		say("535 5.7.8 " + s.badAuthMsg)
		return
	}
	say("535 5.7.8 Authentication credentials invalid")
}

func between(l string) string {
	i, j := strings.Index(l, "<"), strings.LastIndex(l, ">")
	if i < 0 || j < i {
		return ""
	}
	return l[i+1 : j]
}

// ---- 辅助 ----

type mailSink struct {
	mu   sync.Mutex
	logs []SendLog
}

func (m *mailSink) RecordSend(l SendLog) { m.mu.Lock(); m.logs = append(m.logs, l); m.mu.Unlock() }
func (m *mailSink) all() []SendLog {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]SendLog(nil), m.logs...)
}

type emailDir struct {
	people map[string]Person
	groups map[uint][]string // 资源组 -> 用户名
}

func (d emailDir) UsersByName(names []string) ([]Person, error) {
	var out []Person
	for _, n := range names {
		if p, ok := d.people[n]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func (d emailDir) AuthGroupMembers(g, rg uint) ([]Person, error) {
	if rg != 1 {
		return nil, nil
	}
	switch g {
	case 20:
		return d.UsersByName([]string{"alice", "bob", "dave"})
	case 10:
		return d.UsersByName([]string{"carol"})
	}
	return nil, nil
}

func (d emailDir) ResourceGroupMembers(rg uint) ([]Person, error) { return d.UsersByName(d.groups[rg]) }

var mailPeople = map[string]Person{
	"alice": {Username: "alice", Display: "爱丽丝", Email: "alice@example.com"},
	"bob":   {Username: "bob", Display: "鲍勃"}, // 没有邮箱
	"carol": {Username: "carol", Display: "卡罗", Email: "Carol@Example.com"},
	"dave":  {Username: "dave", Display: "戴夫", Email: "ALICE@example.com"}, // 与 alice 同一个邮箱（大小写不同）
	"erin":  {Username: "erin", Display: "艾琳", Email: "not an email"},
}

func testDir() emailDir {
	return emailDir{people: mailPeople, groups: map[uint][]string{1: {"alice", "bob", "carol", "erin"}}}
}

type mailEnv struct {
	srv  *fakeSMTP
	sink *mailSink
	disp *Dispatcher
	now  time.Time
}

// newMailEnv 起假 SMTP、让客户端信任它的证书、把等待时间改小，并清空限流。
func newMailEnv(t *testing.T, mod func(*fakeSMTP)) *mailEnv {
	t.Helper()
	emailLimiter.reset()
	oldCAs, oldC, oldT, oldR, oldRate := tlsRootCAs, emailConnectTimeout, emailTotalTimeout, emailRetryDelay, EmailRatePerMinute
	t.Cleanup(func() {
		tlsRootCAs, emailConnectTimeout, emailTotalTimeout, emailRetryDelay, EmailRatePerMinute = oldCAs, oldC, oldT, oldR, oldRate
		SetPublicURL("", false)
		emailLimiter.reset()
	})
	srv := startSMTP(t, mod)
	tlsRootCAs = srv.pool
	emailRetryDelay = 10 * time.Millisecond
	e := &mailEnv{srv: srv, sink: &mailSink{}, now: time.Unix(1700000000, 0)}
	e.disp = NewDispatcher(fakeSrc{}, testDir())
	e.disp.Sink = e.sink
	e.disp.Now = func() time.Time { return e.now }
	return e
}

func (e *mailEnv) channel(enc string, mod func(*EmailConfig)) Channel {
	cfg := &EmailConfig{Host: "127.0.0.1", Port: e.srv.port(), Encryption: enc, FromAddr: "dbtool@example.com", FromName: "DBA 工具",
		To: []string{"ops@example.com"}, SubjectPrefix: "[dbtool]"}
	if mod != nil {
		mod(cfg)
	}
	return Channel{ID: 7, Name: "邮件渠道", Type: TypeEmail, Enabled: true, Email: cfg, Secret: "S3cr3t!pw"}
}

var evMail = Event{Kind: KindSQLWorkflow, Name: EventPending, Title: "[SQL上线申请]新的工单申请#9", Content: "发起时间：2026-10-07 10:08:21\n发起人：张三\n库：shop", GroupID: 1, Applicant: "alice"}

func parseMail(t *testing.T, data string) (*mail.Message, string, string) {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(data))
	if err != nil {
		t.Fatalf("解析邮件: %v\n%s", err, data)
	}
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("Content-Type: %q %v", m.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	var text, htmlBody string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(quotedprintable.NewReader(p))
		switch {
		case strings.HasPrefix(p.Header.Get("Content-Type"), "text/plain"):
			text = string(b)
		case strings.HasPrefix(p.Header.Get("Content-Type"), "text/html"):
			htmlBody = string(b)
		}
	}
	return m, text, htmlBody
}

func decodeSubject(t *testing.T, m *mail.Message) string {
	t.Helper()
	s, err := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ---- 三种加密方式 ----

func TestEmailEncryptionModes(t *testing.T) {
	cases := []struct {
		name string
		enc  string
		mod  func(*fakeSMTP)
		tls  bool
	}{
		{"不加密", EncNone, nil, false},
		{"STARTTLS", EncStartTLS, func(s *fakeSMTP) { s.starttls = true }, true},
		{"SSL", EncSSL, func(s *fakeSMTP) { s.ssl = true }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newMailEnv(t, c.mod)
			ch := e.channel(c.enc, nil)
			if err := e.disp.SendTest(context.Background(), ch, evMail); err != nil {
				t.Fatalf("发送失败: %v", err)
			}
			got := e.srv.got()
			if len(got) != 1 {
				t.Fatalf("应收到 1 封: %d", len(got))
			}
			g := got[0]
			if g.TLS != c.tls {
				t.Errorf("TLS=%v，期望 %v", g.TLS, c.tls)
			}
			if g.From != "dbtool@example.com" || strings.Join(g.Rcpts, ",") != "ops@example.com" {
				t.Errorf("信封: %+v", g)
			}
			m, text, htmlBody := parseMail(t, g.Data)
			if s := decodeSubject(t, m); s != "[dbtool] [SQL上线申请]新的工单申请#9" {
				t.Errorf("主题: %q", s)
			}
			if !strings.Contains(m.Header.Get("Subject"), "=?utf-8?") {
				t.Errorf("非 ASCII 主题应为 RFC 2047 编码: %q", m.Header.Get("Subject"))
			}
			fa, _ := mail.ParseAddress(m.Header.Get("From"))
			if fa == nil || fa.Address != "dbtool@example.com" || fa.Name != "DBA 工具" {
				t.Errorf("From: %q", m.Header.Get("From"))
			}
			if m.Header.Get("Message-ID") == "" || m.Header.Get("Date") == "" || m.Header.Get("MIME-Version") != "1.0" {
				t.Errorf("缺少必要的头: %v", m.Header)
			}
			if !strings.Contains(text, "发起人：张三") || !strings.Contains(htmlBody, "<td") || !strings.Contains(htmlBody, "张三") {
				t.Errorf("正文: text=%q html=%q", text, htmlBody)
			}
			logs := e.sink.all()
			if len(logs) != 1 || !logs[0].OK || logs[0].Mentions != 1 {
				t.Errorf("发送记录: %+v", logs)
			}
		})
	}
}

func TestEmailStagesReported(t *testing.T) {
	e := newMailEnv(t, func(s *fakeSMTP) { s.starttls = true; s.auth = []string{"PLAIN"}; s.authOnTLS = true })
	ch := e.channel(EncStartTLS, func(c *EmailConfig) { c.Username = "mailer" })
	r := e.disp.SendEmailTest(context.Background(), ch, evMail, "me@example.com")
	if !r.OK || len(r.Stages) != 4 {
		t.Fatalf("测试发送: %+v", r)
	}
	for i, want := range []string{StageConnect, StageStartTLS, StageAuth, StageSend} {
		if r.Stages[i].Name != want || r.Stages[i].Status != StatusOK {
			t.Errorf("阶段 %d: %+v", i, r.Stages[i])
		}
	}
	got := e.srv.got()
	if len(got) != 1 || strings.Join(got[0].Rcpts, ",") != "me@example.com" || got[0].Authed != "mailer" {
		t.Errorf("测试发送只发给指定地址（忽略固定收件人与规则）: %+v", got)
	}
	if logs := e.sink.all(); len(logs) != 1 || !logs[0].Test {
		t.Errorf("发送记录应标为测试: %+v", logs)
	}
	// 失败：认证阶段失败，后面的阶段“未执行”
	ch.Secret = "wrong-password"
	r = e.disp.SendEmailTest(context.Background(), ch, evMail, "me@example.com")
	if r.OK || r.Stages[2].Status != StatusFail || r.Stages[3].Status != StatusSkip || r.Stages[0].Status != StatusOK {
		t.Errorf("认证失败的阶段: %+v", r)
	}
	if strings.Contains(r.Error, "wrong-password") {
		t.Errorf("错误信息含密码: %s", r.Error)
	}
}

// ---- 认证 ----

func TestEmailAuth(t *testing.T) {
	t.Run("PLAIN 成功", func(t *testing.T) {
		e := newMailEnv(t, func(s *fakeSMTP) { s.starttls = true; s.auth = []string{"PLAIN", "LOGIN"}; s.authOnTLS = true })
		if err := e.disp.SendTest(context.Background(), e.channel(EncStartTLS, func(c *EmailConfig) { c.Username = "mailer" }), evMail); err != nil {
			t.Fatal(err)
		}
		if a := e.srv.got(); len(a) != 1 || a[0].Authed != "mailer" {
			t.Errorf("应已认证: %+v", a)
		}
		if len(e.srv.auths) != 1 || !strings.HasPrefix(e.srv.auths[0], "PLAIN ") {
			t.Errorf("两种都支持时优先 PLAIN: %v", e.srv.auths)
		}
	})
	t.Run("只支持 LOGIN", func(t *testing.T) {
		e := newMailEnv(t, func(s *fakeSMTP) { s.ssl = true; s.auth = []string{"LOGIN"} })
		if err := e.disp.SendTest(context.Background(), e.channel(EncSSL, func(c *EmailConfig) { c.Username = "mailer" }), evMail); err != nil {
			t.Fatal(err)
		}
		if a := e.srv.got(); len(a) != 1 || a[0].Authed != "mailer" {
			t.Errorf("应已认证: %+v", a)
		}
	})
	t.Run("认证失败不重试且不泄露密码", func(t *testing.T) {
		e := newMailEnv(t, func(s *fakeSMTP) {
			s.ssl = true
			s.auth = []string{"PLAIN"}
			s.badAuthMsg = "bad password Wr0ng!pw for mailer" // 服务器把客户端提交的密码回显了
		})
		ch := e.channel(EncSSL, func(c *EmailConfig) { c.Username = "mailer" })
		ch.Secret = "Wr0ng!pw"
		err := e.disp.SendTest(context.Background(), ch, evMail)
		if err == nil || !strings.Contains(err.Error(), "认证失败") {
			t.Fatalf("应认证失败: %v", err)
		}
		if strings.Contains(err.Error(), "Wr0ng!pw") {
			t.Errorf("错误信息含密码: %v", err)
		}
		if n := e.srv.connCount(); n != 1 {
			t.Errorf("认证失败属于不可重试错误，应只连 1 次: %d", n)
		}
		for _, l := range e.sink.all() {
			if strings.Contains(l.Error, "Wr0ng!pw") {
				t.Errorf("发送记录含密码: %+v", l)
			}
		}
	})
	t.Run("不加密 + 认证默认拒绝，不会连接", func(t *testing.T) {
		e := newMailEnv(t, func(s *fakeSMTP) { s.auth = []string{"PLAIN"} })
		ch := e.channel(EncNone, func(c *EmailConfig) { c.Username = "mailer" })
		err := e.disp.SendTest(context.Background(), ch, evMail)
		if err == nil || !strings.Contains(err.Error(), "允许明文认证") {
			t.Fatalf("应拒绝: %v", err)
		}
		if e.srv.connCount() != 0 {
			t.Error("拒绝发生在连接之前，不应连接服务器")
		}
		if err := ch.Email.Normalize("S3cr3t!pw"); err == nil || !strings.Contains(err.Error(), "明文") {
			t.Errorf("保存时的校验也应拒绝: %v", err)
		}
	})
	t.Run("显式允许明文认证", func(t *testing.T) {
		e := newMailEnv(t, func(s *fakeSMTP) { s.auth = []string{"PLAIN"} })
		ch := e.channel(EncNone, func(c *EmailConfig) { c.Username = "mailer"; c.AllowPlainAuth = true })
		if err := e.disp.SendTest(context.Background(), ch, evMail); err != nil {
			t.Fatal(err)
		}
		if a := e.srv.got(); len(a) != 1 || a[0].Authed != "mailer" || a[0].TLS {
			t.Errorf("明文连接上认证: %+v", a)
		}
	})
}

// ---- 证书校验 ----

func TestEmailCertificateVerification(t *testing.T) {
	for _, enc := range []string{EncStartTLS, EncSSL} {
		t.Run(enc, func(t *testing.T) {
			e := newMailEnv(t, func(s *fakeSMTP) { s.starttls = enc == EncStartTLS; s.ssl = enc == EncSSL })
			tlsRootCAs = nil // 不信任测试的自签名证书
			ch := e.channel(enc, nil)
			err := e.disp.SendTest(context.Background(), ch, evMail)
			if err == nil || !strings.Contains(err.Error(), "证书校验失败") {
				t.Fatalf("自签名证书应校验失败: %v", err)
			}
			if n := e.srv.connCount(); n != 1 {
				t.Errorf("证书错误不重试: 连接 %d 次", n)
			}
			if len(e.srv.got()) != 0 {
				t.Error("校验失败时不应发出邮件")
			}
			// 显式关闭校验后可以发送
			ch.Email.SkipVerify = true
			if err := e.disp.SendTest(context.Background(), ch, evMail); err != nil {
				t.Fatalf("关闭校验后应成功: %v", err)
			}
			if len(e.srv.got()) != 1 {
				t.Error("应收到 1 封")
			}
		})
	}
}

func TestEmailStartTLSNotAdvertisedAborts(t *testing.T) {
	e := newMailEnv(t, nil) // 服务器不支持 STARTTLS
	err := e.disp.SendTest(context.Background(), e.channel(EncStartTLS, nil), evMail)
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("不应降级为明文: %v", err)
	}
	if len(e.srv.got()) != 0 {
		t.Error("不应发出邮件")
	}
}

// ---- 收件人 ----

func TestEmailRecipientsResolveDedupAndSkip(t *testing.T) {
	e := newMailEnv(t, nil)
	ch := e.channel(EncNone, func(c *EmailConfig) {
		c.To = []string{"ops@example.com"}
		c.Cc = []string{"boss@example.com", "OPS@example.com"}
	})
	ch.Rules = []Rule{
		{Target: TargetApplicant},                   // alice
		{Target: TargetReviewers},                   // 权限组 20：alice(重复)、bob(无邮箱)、dave(与 alice 同邮箱)
		{Target: TargetAuthGroup, Value: "10"},      // carol
		{Target: TargetUser, Value: "erin"},         // 邮箱格式不合法
		{Target: TargetRaw, Value: "x@example.com"}, // 原生标识为邮箱地址
		{Target: TargetAll},                         // 资源组 1 的全部用户
	}
	ev := evMail
	ev.ReviewerAuthGroupID = 20
	if err := e.disp.SendTest(context.Background(), ch, ev); err != nil {
		t.Fatal(err)
	}
	got := e.srv.got()
	if len(got) != 1 {
		t.Fatalf("应只发 1 封（多收件人一封邮件）: %d", len(got))
	}
	have := map[string]int{}
	for _, r := range got[0].Rcpts {
		have[strings.ToLower(r)]++
	}
	for _, want := range []string{"ops@example.com", "boss@example.com", "alice@example.com", "carol@example.com", "x@example.com"} {
		if have[want] != 1 {
			t.Errorf("收件人 %s 应出现 1 次: %v", want, got[0].Rcpts)
		}
	}
	if len(got[0].Rcpts) != 5 {
		t.Errorf("去重后应 5 个: %v", got[0].Rcpts)
	}
	m, _, _ := parseMail(t, got[0].Data)
	if !strings.Contains(m.Header.Get("To"), "<ops@example.com>") || !strings.Contains(m.Header.Get("Cc"), "boss@example.com") || strings.Contains(m.Header.Get("Cc"), "OPS@") {
		t.Errorf("To / Cc 头: %q / %q", m.Header.Get("To"), m.Header.Get("Cc"))
	}
	logs := e.sink.all()
	if len(logs) != 1 || !logs[0].OK || logs[0].Mentions != 5 {
		t.Fatalf("发送记录: %+v", logs)
	}
	if !strings.Contains(logs[0].Error, "没有邮箱") || !strings.Contains(logs[0].Error, "鲍勃") {
		t.Errorf("发送记录里应写明跳过了没有邮箱的用户: %q", logs[0].Error)
	}
	rc := e.disp.emailRecipients(ch, ev, mustMents(t, e.disp, ch, ev))
	if !contains(rc.NoEmail, "鲍勃") || len(rc.Skipped) < 2 {
		t.Errorf("解析结果: %+v", rc)
	}
	foundBad := false
	for _, s := range rc.Skipped {
		foundBad = foundBad || strings.Contains(s, "邮箱格式不合法")
	}
	if !foundBad {
		t.Errorf("应记录邮箱格式不合法的用户: %v", rc.Skipped)
	}
}

func mustMents(t *testing.T, d *Dispatcher, ch Channel, ev Event) []Mention {
	t.Helper()
	m, _ := d.resolveExplain(ch, ev)
	return m
}

func TestEmailNoRecipientsFails(t *testing.T) {
	e := newMailEnv(t, nil)
	ch := e.channel(EncNone, func(c *EmailConfig) { c.To = nil })
	ch.Rules = []Rule{{Target: TargetUser, Value: "bob"}} // 只有一个没邮箱的用户
	err := e.disp.SendTest(context.Background(), ch, evMail)
	if err == nil || !strings.Contains(err.Error(), "没有可用的收件人") || !strings.Contains(err.Error(), "鲍勃") {
		t.Fatalf("应因没有收件人失败并说明原因: %v", err)
	}
	if e.srv.connCount() != 0 {
		t.Error("没有收件人时不应连接服务器")
	}
	if logs := e.sink.all(); len(logs) != 1 || logs[0].OK {
		t.Errorf("应记一条失败: %+v", logs)
	}
}

func TestEmailRecipientCap(t *testing.T) {
	e := newMailEnv(t, nil)
	people := map[string]Person{}
	var names []string
	for i := 0; i < 70; i++ {
		n := "u" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		people[n] = Person{Username: n, Email: n + "@example.com"}
		names = append(names, n)
	}
	e.disp.Dir = emailDir{people: people, groups: map[uint][]string{1: names}}
	ch := e.channel(EncNone, func(c *EmailConfig) { c.To = []string{"ops@example.com"} })
	ch.Rules = []Rule{{Target: TargetAll}}
	if err := e.disp.SendTest(context.Background(), ch, evMail); err != nil {
		t.Fatal(err)
	}
	got := e.srv.got()
	if len(got) != 1 || len(got[0].Rcpts) != MaxRecipients {
		t.Fatalf("一封邮件最多 %d 个收件人: %+v", MaxRecipients, got)
	}
	if logs := e.sink.all(); !strings.Contains(logs[0].Error, "已丢弃") {
		t.Errorf("应记录丢弃: %q", logs[0].Error)
	}
}

func TestEmailRejectedRecipientsReported(t *testing.T) {
	e := newMailEnv(t, func(s *fakeSMTP) { s.rejectRcpt["boss@example.com"] = true })
	ch := e.channel(EncNone, func(c *EmailConfig) { c.Cc = []string{"boss@example.com"} })
	if err := e.disp.SendTest(context.Background(), ch, evMail); err != nil {
		t.Fatalf("部分收件人被拒绝仍应发送成功: %v", err)
	}
	if logs := e.sink.all(); !strings.Contains(logs[0].Error, "boss@example.com") {
		t.Errorf("应记录被拒绝的收件人: %q", logs[0].Error)
	}
	ch.Email.To, ch.Email.Cc = nil, []string{"boss@example.com"}
	ch.Rules = nil
	if err := e.disp.SendTest(context.Background(), ch, evMail); err == nil {
		t.Error("全部被拒绝应失败")
	}
}

// ---- 头注入 ----

func TestEmailHeaderInjection(t *testing.T) {
	evil := Event{Name: EventPending, Title: "正常标题\r\nBcc: evil@example.com\r\nX-Injected: 1", Content: "发起人：张三", GroupID: 1}
	e := newMailEnv(t, nil)
	ch := e.channel(EncNone, func(c *EmailConfig) {
		c.FromName = "管理员\r\nBcc: evil2@example.com"
		c.SubjectPrefix = "[前缀]\nBcc: evil3@example.com"
	})
	if err := e.disp.SendTest(context.Background(), ch, evil); err != nil {
		t.Fatal(err)
	}
	got := e.srv.got()
	if len(got) != 1 {
		t.Fatal("应发出")
	}
	m, _, _ := parseMail(t, got[0].Data)
	for _, h := range []string{"Bcc", "X-Injected"} {
		if m.Header.Get(h) != "" {
			t.Errorf("头注入成功: %s=%q", h, m.Header.Get(h))
		}
	}
	for _, a := range got[0].Rcpts {
		if strings.Contains(a, "evil") {
			t.Errorf("收件人被注入: %v", got[0].Rcpts)
		}
	}
	// 头部区域（空行之前）没有任何一行以 Bcc 开头
	head := got[0].Data[:strings.Index(got[0].Data, "\r\n\r\n")]
	for _, ln := range strings.Split(head, "\r\n") {
		if strings.HasPrefix(strings.ToLower(ln), "bcc:") || strings.HasPrefix(ln, "X-Injected") {
			t.Errorf("头部出现注入行: %q", ln)
		}
	}
	if s := decodeSubject(t, m); strings.ContainsAny(s, "\r\n") || !strings.Contains(s, "正常标题") {
		t.Errorf("主题: %q", s)
	}

	// 地址里的 CR/LF 一律拒绝
	for _, bad := range []string{"a@example.com\r\nBcc: x@example.com", "a@example.com\nBcc:x@y.com", "a@exa\rmple.com", "<a@example.com>", "A <a@example.com>", "a@example.com,b@example.com", "a b@example.com", "中文@example.com"} {
		if _, err := ParseAddr(bad); err == nil {
			t.Errorf("应拒绝地址 %q", bad)
		}
	}
	cfg := EmailConfig{Host: "smtp.example.com", Port: 25, Encryption: EncNone, FromAddr: "a@example.com", To: []string{"b@example.com\r\nBcc: c@example.com"}}
	if err := cfg.Normalize(""); err == nil {
		t.Error("保存配置时应拒绝带换行的收件人")
	}
	for _, f := range []func(*EmailConfig){
		func(c *EmailConfig) { c.FromName = "x\r\ny" },
		func(c *EmailConfig) { c.SubjectPrefix = "x\ny" },
		func(c *EmailConfig) { c.Username = "u\r\nv"; c.Encryption = EncSSL },
		func(c *EmailConfig) { c.Host = "smtp.example.com\r\nRCPT TO:<x@y.z>" },
		func(c *EmailConfig) { c.FromAddr = "a@example.com\r\nBcc: z@example.com" },
	} {
		c := EmailConfig{Host: "smtp.example.com", Port: 25, Encryption: EncNone, FromAddr: "a@example.com"}
		f(&c)
		if err := c.Normalize(""); err == nil {
			t.Errorf("应拒绝配置: %+v", c)
		}
	}
	if _, err := BuildEmail(EmailMessage{From: "a@example.com\r\nBcc: z@example.com", To: []string{"b@example.com"}}); err == nil {
		t.Error("BuildEmail 应拒绝带换行的发件人")
	}
	if _, err := BuildEmail(EmailMessage{From: "a@example.com", To: []string{"b@example.com\r\nBcc: z@example.com"}}); err == nil {
		t.Error("BuildEmail 应拒绝带换行的收件人")
	}
}

// ---- HTML 转义与链接 ----

func TestEmailHTMLEscapeAndLinks(t *testing.T) {
	SetPublicURL("https://dba.example.com", true)
	defer SetPublicURL("", false)
	ev := Event{Name: EventRejected, Title: `<script>alert(1)</script>工单#1`, GroupID: 1,
		Content: "发起人：<img src=x onerror=alert(1)>\n工单地址：https://dba.example.com/sqlworkflow/9\n备注：见 javascript:alert(1) 与 https://evil.example.net/x?a=1&b=2\n驳回原因：\"quoted\" & 'single'\n工单详情预览：select 1;\n-- <b>注释</b>"}
	text, h := EmailBodies(ev, `渠道"<b>`)
	for _, bad := range []string{"<script", "<img", "<b>注释", `onerror=alert(1)>`} {
		if strings.Contains(h, bad) {
			t.Errorf("HTML 里出现未转义的 %q:\n%s", bad, h)
		}
	}
	for _, want := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "&lt;img src=x onerror=alert(1)&gt;", "&lt;b&gt;注释&lt;/b&gt;", "&#34;quoted&#34; &amp; &#39;single&#39;", "渠道&#34;&lt;b&gt;"} {
		if !strings.Contains(h, want) {
			t.Errorf("HTML 缺少转义结果 %q:\n%s", want, h)
		}
	}
	if !strings.Contains(h, `<a href="https://dba.example.com/sqlworkflow/9">https://dba.example.com/sqlworkflow/9</a>`) {
		t.Errorf("本系统链接应可点击:\n%s", h)
	}
	if strings.Contains(h, `href="https://evil.example.net`) || strings.Contains(h, "href=\"javascript") {
		t.Errorf("外部链接不应生成 <a>:\n%s", h)
	}
	if !strings.Contains(h, "https://evil.example.net/x?a=1&amp;b=2") {
		t.Errorf("外部链接按文字显示（& 转义）:\n%s", h)
	}
	if !strings.Contains(text, "<script>alert(1)</script>工单#1") {
		t.Errorf("纯文本正文原样保留标题:\n%s", text)
	}

	// 没有显式配置 PUBLIC_URL：邮件里不放链接，只剩编号
	SetPublicURL("http://127.0.0.1:8000", false)
	text, h = EmailBodies(ev, "x")
	for _, s := range []string{text, h} {
		if strings.Contains(s, "dba.example.com") || strings.Contains(s, "127.0.0.1") || strings.Contains(s, "工单地址") {
			t.Errorf("未配置 PUBLIC_URL 时不应有本系统链接:\n%s", s)
		}
	}
	if !strings.Contains(text, "工单#1") {
		t.Errorf("编号仍应保留:\n%s", text)
	}
}

// ---- 限流 ----

func TestEmailRateLimit(t *testing.T) {
	e := newMailEnv(t, nil)
	EmailRatePerMinute = 3
	ch := e.channel(EncNone, nil)
	var fails int
	for i := 0; i < 5; i++ {
		if err := e.disp.SendTest(context.Background(), ch, evMail); err != nil {
			fails++
			if !strings.Contains(err.Error(), "限流") {
				t.Errorf("错误原因: %v", err)
			}
		}
	}
	if fails != 2 || len(e.srv.got()) != 3 {
		t.Fatalf("每分钟 3 封：应发出 3 封、丢弃 2 封: fails=%d got=%d", fails, len(e.srv.got()))
	}
	logs := e.sink.all()
	if len(logs) != 5 || logs[4].OK || !strings.Contains(logs[4].Error, "限流") {
		t.Errorf("被丢弃的也要记日志: %+v", logs)
	}
	// 另一个渠道不受影响
	other := ch
	other.ID = 8
	if err := e.disp.SendTest(context.Background(), other, evMail); err != nil {
		t.Errorf("限流按渠道计: %v", err)
	}
	// 一分钟之后恢复
	e.now = e.now.Add(61 * time.Second)
	if err := e.disp.SendTest(context.Background(), ch, evMail); err != nil {
		t.Errorf("一分钟后应恢复: %v", err)
	}
}

// ---- 超时与重试 ----

func TestEmailConnectTimeoutRetriesOnce(t *testing.T) {
	e := newMailEnv(t, func(s *fakeSMTP) { s.hangGreet = true })
	emailConnectTimeout = 150 * time.Millisecond
	start := time.Now()
	e.disp.Source = fakeSrc{[]Channel{e.channel(EncNone, nil)}}
	e.disp.Notify(context.Background(), evMail) // 经 Notify 走完整投递流程（非测试发送，允许重试一次）
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("耗时过长: %v", d)
	}
	if n := e.srv.connCount(); n != 2 {
		t.Errorf("失败后最多重试 1 次（共 2 次连接）: %d", n)
	}
	logs := e.sink.all()
	if len(logs) != 1 || logs[0].OK {
		t.Errorf("一次投递只记一条日志且为失败: %+v", logs)
	}
}

func TestEmailTotalTimeout(t *testing.T) {
	e := newMailEnv(t, func(s *fakeSMTP) { s.hangData = true })
	emailTotalTimeout = 300 * time.Millisecond
	start := time.Now()
	err := e.disp.SendTest(context.Background(), e.channel(EncNone, nil), evMail)
	if err == nil {
		t.Fatal("应超时")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("整次发送应在总超时内结束: %v", d)
	}
	if n := e.srv.connCount(); n != 1 {
		t.Errorf("测试发送不重试: %d", n)
	}
}

func TestEmailTransientFailureRetriedOnce(t *testing.T) {
	e := newMailEnv(t, nil)
	ch := e.channel(EncNone, nil)
	ch.Email.Port = 1 // 连接被拒绝
	e.disp.Source = fakeSrc{[]Channel{ch}}
	e.disp.Notify(context.Background(), evMail)
	logs := e.sink.all()
	if len(logs) != 1 || logs[0].OK || !strings.Contains(logs[0].Error, "连接") {
		t.Errorf("连接被拒绝应失败并记录: %+v", logs)
	}
}

// ---- 日志与错误里不含密码 ----

func TestEmailPasswordNeverLeaks(t *testing.T) {
	e := newMailEnv(t, func(s *fakeSMTP) {
		s.ssl = true
		s.auth = []string{"PLAIN"}
		s.badAuthMsg = "Wr0ng!pw rejected"
	})
	ch := e.channel(EncSSL, func(c *EmailConfig) { c.Username = "mailer" })
	ch.Secret = "Wr0ng!pw"
	r := e.disp.SendEmailTest(context.Background(), ch, evMail, "me@example.com")
	blob := r.Error
	for _, s := range r.Stages {
		blob += s.Detail + s.Label
	}
	for _, l := range e.sink.all() {
		blob += l.Error
	}
	if r.OK || strings.Contains(blob, "Wr0ng!pw") {
		t.Errorf("结果 / 日志不应含密码: ok=%v %s", r.OK, blob)
	}
	// 预览不连接 SMTP，也不含密码
	pv, err := e.disp.Preview(ch, evMail)
	if err != nil {
		t.Fatal(err)
	}
	if b := string(pv.Payload); strings.Contains(b, "Wr0ng!pw") || pv.Platform != TypeEmail {
		t.Errorf("预览: %s", b)
	}
	if e.srv.connCount() != 1 {
		t.Errorf("预览不应连接服务器: %d", e.srv.connCount())
	}
}

// ---- 订阅过滤、全局与资源组渠道 ----

func TestEmailSubscriptionAndScope(t *testing.T) {
	e := newMailEnv(t, nil)
	sub := e.channel(EncNone, nil)
	sub.ID, sub.Events, sub.EventsV2 = 1, []string{EventPassed}, true // 只订阅“审批通过”
	all := e.channel(EncNone, func(c *EmailConfig) { c.To = []string{"all@example.com"} })
	all.ID = 2 // 订阅全部
	global := e.channel(EncNone, func(c *EmailConfig) { c.To = []string{"global@example.com"} })
	global.ID = 3 // GroupID=0、没有关联资源组：全局渠道
	scoped := e.channel(EncNone, func(c *EmailConfig) { c.To = []string{"scoped@example.com"} })
	scoped.ID, scoped.GroupID, scoped.GroupIDs = 4, 1, []uint{1}
	off := e.channel(EncNone, func(c *EmailConfig) { c.To = []string{"off@example.com"} })
	off.ID, off.Enabled = 5, false
	e.disp.Source = fakeSrc{[]Channel{sub, all, global, scoped, off}}

	rcpt := func() map[string]bool {
		m := map[string]bool{}
		for _, g := range e.srv.got() {
			for _, r := range g.Rcpts {
				m[r] = true
			}
		}
		return m
	}
	e.disp.Notify(context.Background(), evMail) // pending
	got := rcpt()
	if got["off@example.com"] || len(got) != 3 || !got["all@example.com"] || !got["global@example.com"] || !got["scoped@example.com"] {
		t.Fatalf("pending: 订阅了 passed 的渠道与停用的渠道不应收到；全局与资源组渠道都收到: %v", got)
	}
	// SkipGlobal：全局渠道不发
	before := len(e.srv.got())
	ev := evMail
	ev.SkipGlobal = true
	e.disp.Notify(context.Background(), ev)
	if n := len(e.srv.got()) - before; n != 1 {
		t.Errorf("SkipGlobal: 全局渠道（sub/all/global）不发，只剩关联了资源组的 scoped: %d", n)
	}
	// passed 事件：订阅了 passed 的渠道也收到
	before = len(e.srv.got())
	ev = evMail
	ev.Name = EventPassed
	e.disp.Notify(context.Background(), ev)
	if n := len(e.srv.got()) - before; n != 4 {
		t.Errorf("passed: 应有 4 封（sub/all/global/scoped）: %d", n)
	}
}

// ---- 配置校验 ----

func TestEmailConfigNormalize(t *testing.T) {
	ok := EmailConfig{Host: " smtp.example.com ", Encryption: "STARTTLS", FromAddr: " dba@example.com ", To: []string{"a@example.com", "A@example.com", ""}, Cc: []string{"a@example.com", "b@example.com"}}
	if err := ok.Normalize(""); err != nil {
		t.Fatal(err)
	}
	if ok.Port != 587 || ok.Encryption != EncStartTLS || ok.Host != "smtp.example.com" || len(ok.To) != 1 || len(ok.Cc) != 1 || ok.Cc[0] != "b@example.com" {
		t.Errorf("规整结果: %+v", ok)
	}
	if ok.SkipVerify {
		t.Error("证书校验默认开启（SkipVerify 默认 false）")
	}
	many := make([]string, 21)
	for i := range many {
		many[i] = "u" + string(rune('a'+i)) + "@example.com"
	}
	c := EmailConfig{Host: "h.example.com", FromAddr: "a@example.com", To: many[:11], Cc: many[11:]}
	if err := c.Normalize(""); err == nil {
		t.Error("To + Cc 超过 20 个应拒绝")
	}
	c = EmailConfig{Host: "h.example.com", FromAddr: "a@example.com", To: many[:10], Cc: many[10:20]}
	if err := c.Normalize(""); err != nil {
		t.Errorf("正好 20 个应通过: %v", err)
	}
	bad := []EmailConfig{
		{Host: "", FromAddr: "a@example.com"},
		{Host: "smtp://x", FromAddr: "a@example.com"},
		{Host: "x.example.com:25", FromAddr: "a@example.com"},
		{Host: "x.example.com", FromAddr: "a@example.com", Port: 70000},
		{Host: "x.example.com", FromAddr: "a@example.com", Encryption: "tls13"},
		{Host: "x.example.com", FromAddr: "not-an-address"},
		{Host: "x.example.com", FromAddr: "a@example.com", To: []string{"bad"}},
	}
	for i, b := range bad {
		if err := b.Normalize(""); err == nil {
			t.Errorf("坏配置 %d 应被拒绝: %+v", i, b)
		}
	}
	pw := EmailConfig{Host: "h.example.com", FromAddr: "a@example.com", Encryption: EncSSL}
	if err := pw.Normalize("secret"); err == nil {
		t.Error("有密码没用户名应拒绝")
	}
}

func TestRateLimiterWindow(t *testing.T) {
	r := &rateLimiter{hits: map[string][]time.Time{}}
	now := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		if !r.allow("k", now, 3) {
			t.Fatal("前 3 次应放行")
		}
	}
	if r.allow("k", now.Add(30*time.Second), 3) {
		t.Error("窗口内第 4 次应拒绝")
	}
	if !r.allow("k", now.Add(61*time.Second), 3) {
		t.Error("窗口滑过后应放行")
	}
	if !r.allow("k", now, 0) {
		t.Error("limit<=0 表示不限制")
	}
}

// 通知失败写进进程日志（slog）的内容里也不能有密码。
func TestEmailSlogNoPassword(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	e := newMailEnv(t, func(s *fakeSMTP) {
		s.ssl = true
		s.auth = []string{"PLAIN"}
		s.badAuthMsg = "Wr0ng!pw is not accepted"
	})
	ch := e.channel(EncSSL, func(c *EmailConfig) { c.Username = "mailer" })
	ch.Secret = "Wr0ng!pw"
	e.disp.Source = fakeSrc{[]Channel{ch}}
	e.disp.Notify(context.Background(), evMail)
	out := buf.String()
	if !strings.Contains(out, "通知发送失败") || !strings.Contains(out, "认证失败") {
		t.Fatalf("应记录失败日志: %q", out)
	}
	if strings.Contains(out, "Wr0ng!pw") {
		t.Errorf("进程日志含密码: %s", out)
	}
}
