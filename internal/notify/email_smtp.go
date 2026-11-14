// @author: kevinkelly
// @license: Apache Licence
// @file: email_smtp.go
// @time: 2026/10/28

package notify

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 本文件：只用标准库（net/smtp、crypto/tls）的 SMTP 发送、分阶段结果、限流。

// 可调参数（测试里会改小）。
var (
	emailConnectTimeout = 10 * time.Second // 建立连接（含 TCP、TLS 握手、服务器问候与 EHLO）
	emailTotalTimeout   = 30 * time.Second // 一次发送（含重试）的总上限
	emailRetryDelay     = 2 * time.Second  // 失败后重试前的等待
	// EmailRatePerMinute 是每个渠道每分钟最多发送的邮件数，超出的丢弃并记日志；<= 0 表示不限制。
	EmailRatePerMinute = 60

	// tlsRootCAs 非空时作为校验服务器证书的根证书池（仅测试使用；生产为 nil = 系统根证书）。
	tlsRootCAs *x509.CertPool
)

// 阶段名。
const (
	StageConnect  = "connect"
	StageStartTLS = "starttls"
	StageAuth     = "auth"
	StageSend     = "send"
)

// 阶段结果。
const (
	StatusOK   = "ok"
	StatusFail = "fail"
	StatusSkip = "skip" // 不适用或前一阶段失败而未执行
)

// EmailStage 是 SMTP 一个阶段的结果。Detail 不含密码。
type EmailStage struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func newStages() []EmailStage {
	return []EmailStage{
		{Name: StageConnect, Label: "连接", Status: StatusSkip, Detail: "未执行"},
		{Name: StageStartTLS, Label: "STARTTLS", Status: StatusSkip, Detail: "未执行"},
		{Name: StageAuth, Label: "认证", Status: StatusSkip, Detail: "未执行"},
		{Name: StageSend, Label: "发送", Status: StatusSkip, Detail: "未执行"},
	}
}

// SMTPParams 是一次 SMTP 发送的参数。
type SMTPParams struct {
	Cfg   EmailConfig
	Pass  string
	Rcpts []string // 信封收件人（To + Cc + 动态解析的）
	Msg   []byte
}

// SMTPResult 是一次 SMTP 发送的结果。
type SMTPResult struct {
	Stages   []EmailStage
	Rejected []string // 被服务器拒绝的收件人
}

// permanentError 是重试也不会成功的错误（配置问题、认证失败、证书校验失败……）。
type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

// wrapErr 带着抹掉密码后的文本，同时保留原始错误链（用于判断是否可重试）。
type wrapErr struct {
	msg string
	err error
}

func (e wrapErr) Error() string { return e.msg }
func (e wrapErr) Unwrap() error { return e.err }

// scrub 把错误文本里可能出现的密码抹掉。
func scrub(s, pass string) string {
	if pass != "" {
		s = strings.ReplaceAll(s, pass, "***")
	}
	return s
}

func isCertErr(err error) bool {
	var ua x509.UnknownAuthorityError
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	var cv *tls.CertificateVerificationError
	return errors.As(err, &ua) || errors.As(err, &he) || errors.As(err, &ci) || errors.As(err, &cv)
}

// retryable 判断错误是否值得重试一次：网络层的临时故障与 SMTP 4xx；证书、认证、5xx 与配置错误不重试。
func retryable(err error) bool {
	var pe permanentError
	if errors.As(err, &pe) || isCertErr(err) {
		return false
	}
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code >= 400 && te.Code < 500
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// ---- 认证：自己实现 PLAIN / LOGIN，不受 net/smtp.PlainAuth “非 TLS 拒绝”的限制 ----
// 是否允许在明文连接上认证由配置（AllowPlainAuth）决定，smtpSend 在连接前就检查。

type plainAuth struct{ user, pass string }

func (a plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}

func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("服务器在 PLAIN 认证中返回了意外的挑战")
	}
	return nil, nil
}

type loginAuth struct{ user, pass string }

func (a loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }

func (a loginAuth) Next(from []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch s := strings.ToLower(strings.TrimSpace(string(from))); {
	case strings.HasPrefix(s, "username"):
		return []byte(a.user), nil
	case strings.HasPrefix(s, "password"):
		return []byte(a.pass), nil
	}
	return nil, errors.New("服务器在 LOGIN 认证中返回了无法识别的挑战")
}

// chooseAuth 按服务器通告的机制选择 PLAIN 或 LOGIN。
func chooseAuth(params, user, pass string) (smtp.Auth, error) {
	mechs := strings.Fields(strings.ToUpper(params))
	has := func(m string) bool {
		for _, x := range mechs {
			if x == m {
				return true
			}
		}
		return false
	}
	switch {
	case has("PLAIN"):
		return plainAuth{user, pass}, nil
	case has("LOGIN"):
		return loginAuth{user, pass}, nil
	}
	return nil, fmt.Errorf("服务器不支持 PLAIN / LOGIN 认证（它通告的机制：%s）", strings.Join(mechs, " "))
}

// smtpSend 发送一封邮件，返回各阶段结果。ctx 取消 / 超时会中断连接；整次发送最长 emailTotalTimeout。
func smtpSend(ctx context.Context, p SMTPParams) (SMTPResult, error) {
	res := SMTPResult{Stages: newStages()}
	cfg := p.Cfg
	set := func(i int, status, detail string) {
		res.Stages[i].Status, res.Stages[i].Detail = status, scrub(detail, p.Pass)
	}
	fail := func(i int, err error) (SMTPResult, error) {
		msg := scrub(err.Error(), p.Pass)
		set(i, StatusFail, msg)
		return res, wrapErr{msg, err}
	}
	const (
		iConn, iTLS, iAuth, iSend = 0, 1, 2, 3
	)

	// 配置检查先于任何网络动作：明文连接上默认不允许认证
	if cfg.Username != "" && cfg.Encryption == EncNone && !cfg.AllowPlainAuth {
		err := permanentError{errors.New("加密方式为“不加密”，且未勾选“允许明文认证”：为避免密码明文传输，已拒绝连接")}
		return fail(iConn, err)
	}
	if len(p.Rcpts) == 0 {
		return fail(iSend, permanentError{errors.New("没有收件人")})
	}

	ctx, cancel := context.WithTimeout(ctx, emailTotalTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	tlsCfg := &tls.Config{ServerName: cfg.Host, InsecureSkipVerify: cfg.SkipVerify, MinVersion: tls.VersionTLS12, RootCAs: tlsRootCAs} //nolint:gosec // 是否校验由渠道配置决定，默认校验
	d := net.Dialer{Timeout: emailConnectTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fail(iConn, fmt.Errorf("连接 %s 失败：%w", addr, err))
	}
	defer conn.Close()
	// ctx 取消时立即断开
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()

	// 连接阶段（TCP 已通；SSL 的 TLS 握手、服务器问候、EHLO）整体受连接超时限制
	connDeadline := time.Now().Add(emailConnectTimeout)
	if connDeadline.After(deadline) {
		connDeadline = deadline
	}
	_ = conn.SetDeadline(connDeadline)
	var nc net.Conn = conn
	proto := "明文"
	if cfg.Encryption == EncSSL {
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return fail(iConn, tlsErr(addr, err))
		}
		nc, proto = tc, "SSL/TLS "+tlsVersion(tc.ConnectionState().Version)
	}
	c, err := smtp.NewClient(nc, cfg.Host)
	if err != nil {
		return fail(iConn, fmt.Errorf("读取服务器问候失败：%w", err))
	}
	defer c.Close()
	if err := c.Hello(helloName(cfg.FromAddr)); err != nil {
		return fail(iConn, fmt.Errorf("EHLO 失败：%w", err))
	}
	set(iConn, StatusOK, fmt.Sprintf("已连接 %s（%s）", addr, proto))
	_ = conn.SetDeadline(deadline)

	// STARTTLS：服务器不支持就中止，绝不降级为明文
	switch cfg.Encryption {
	case EncStartTLS:
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fail(iTLS, permanentError{errors.New("服务器没有通告 STARTTLS（为避免降级为明文，已中止）")})
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fail(iTLS, tlsErr(addr, err))
		}
		st, _ := c.TLSConnectionState()
		set(iTLS, StatusOK, fmt.Sprintf("已升级为 TLS %s", tlsVersion(st.Version)))
	case EncSSL:
		set(iTLS, StatusSkip, "不适用：已在连接时完成 SSL/TLS 握手")
	default:
		set(iTLS, StatusSkip, "不适用：未启用加密")
	}

	// 认证
	if cfg.Username == "" {
		set(iAuth, StatusSkip, "未配置用户名，不认证")
	} else {
		ok, params := c.Extension("AUTH")
		if !ok {
			return fail(iAuth, permanentError{errors.New("服务器没有通告 AUTH，无法使用用户名密码认证")})
		}
		auth, err := chooseAuth(params, cfg.Username, p.Pass)
		if err != nil {
			return fail(iAuth, permanentError{err})
		}
		if err := c.Auth(auth); err != nil {
			var te *textproto.Error
			if errors.As(err, &te) && te.Code >= 500 {
				err = permanentError{fmt.Errorf("认证失败：%w", err)}
			} else {
				err = fmt.Errorf("认证失败：%w", err)
			}
			return fail(iAuth, err)
		}
		detail := "认证成功"
		if cfg.Encryption == EncNone {
			detail = "认证成功（明文连接，用户名和密码未加密传输）"
		}
		set(iAuth, StatusOK, detail)
	}

	// 发送
	if err := c.Mail(cfg.FromAddr); err != nil {
		return fail(iSend, fmt.Errorf("MAIL FROM 被拒绝：%w", err))
	}
	accepted := 0
	for _, r := range p.Rcpts {
		if err := c.Rcpt(r); err != nil {
			var te *textproto.Error
			if !errors.As(err, &te) {
				return fail(iSend, err)
			}
			res.Rejected = append(res.Rejected, r)
			continue
		}
		accepted++
	}
	if accepted == 0 {
		return fail(iSend, permanentError{errors.New("所有收件人都被服务器拒绝")})
	}
	w, err := c.Data()
	if err != nil {
		return fail(iSend, fmt.Errorf("DATA 被拒绝：%w", err))
	}
	if _, err := w.Write(p.Msg); err != nil {
		return fail(iSend, fmt.Errorf("写入邮件内容失败：%w", err))
	}
	if err := w.Close(); err != nil {
		return fail(iSend, fmt.Errorf("服务器拒收邮件：%w", err))
	}
	_ = c.Quit()
	detail := fmt.Sprintf("已提交给服务器，%d 个收件人", accepted)
	if len(res.Rejected) > 0 {
		detail = fmt.Sprintf("已提交给服务器，%d 个收件人；%d 个被拒绝：%s", accepted, len(res.Rejected), strings.Join(res.Rejected, "、"))
	}
	set(iSend, StatusOK, detail)
	return res, nil
}

func tlsVersion(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "1.3"
	case tls.VersionTLS12:
		return "1.2"
	}
	return fmt.Sprintf("0x%x", v)
}

// tlsErr 把 TLS 错误翻译成给运维看的提示；证书错误是不可重试的。
func tlsErr(addr string, err error) error {
	if isCertErr(err) {
		return permanentError{fmt.Errorf("证书校验失败（%s）：%w。如果是自签名证书，请让服务器使用受信任的证书；也可以在渠道里关闭“校验证书”，但这会使连接可被中间人窃听", addr, err)}
	}
	return fmt.Errorf("TLS 握手失败（%s）：%w", addr, err)
}

// helloName 取发件人地址的域名作为 EHLO 名字（比 localhost 更容易通过服务器的 HELO 检查）。
func helloName(from string) string {
	if i := strings.LastIndex(from, "@"); i >= 0 && i+1 < len(from) {
		return from[i+1:]
	}
	return "localhost"
}

// ---- 限流：每个渠道每分钟最多 EmailRatePerMinute 封（进程内；多实例各算各的） ----

type rateLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

var emailLimiter = &rateLimiter{hits: map[string][]time.Time{}}

func (r *rateLimiter) allow(key string, now time.Time, limit int) bool {
	if limit <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cut := now.Add(-time.Minute)
	list := r.hits[key][:0]
	for _, t := range r.hits[key] {
		if t.After(cut) {
			list = append(list, t)
		}
	}
	if len(list) >= limit {
		r.hits[key] = list
		return false
	}
	r.hits[key] = append(list, now)
	return true
}

func (r *rateLimiter) reset() {
	r.mu.Lock()
	r.hits = map[string][]time.Time{}
	r.mu.Unlock()
}
