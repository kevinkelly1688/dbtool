// @author: kevinkelly
// @license: Apache Licence
// @file: email.go
// @time: 2026/10/28

package notify

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 邮件渠道（type = email）。
//
// SMTP 的非敏感配置（主机、端口、加密方式、用户名、发件人、固定收件人、主题前缀）存在 dbt_notify_email.config（JSON，deploy/sql/210_notify_email.sql）；
// SMTP 密码与机器人的“签名密钥”共用 dbt_notify_channel.secret（同一套加密、不回显、留空不改）；webhook_url 对邮件渠道为空。
// 动态收件人复用 @ 规则：提交人、当前审批人、权限组成员、指定用户、DBA 值班、所有人，解析成用户资料里的邮箱。

// TypeEmail 是邮件渠道类型。
const TypeEmail = "email"

// 加密方式。
const (
	EncNone     = "none"     // 明文连接
	EncStartTLS = "starttls" // 先明文连接，再升级为 TLS（587）
	EncSSL      = "ssl"      // 连接即 TLS（465）
)

// 邮件渠道的数量限制。
const (
	MaxFixedRecipients = 20 // 固定收件人（To + Cc）最多个数
	MaxRecipients      = 50 // 一封邮件的收件人（含动态解析的）最多个数
	maxSubjectRunes    = 200
	maxFromNameRunes   = 64
	maxPrefixRunes     = 50
)

// EmailConfig 是邮件渠道的 SMTP 配置。密码不在其中（见 Channel.Secret）。
type EmailConfig struct {
	Host           string   `json:"host"`
	Port           int      `json:"port"`
	Encryption     string   `json:"encryption"`       // none / starttls / ssl
	SkipVerify     bool     `json:"skip_verify"`      // 不校验服务器证书（默认校验；有被中间人攻击的风险）
	Username       string   `json:"username"`         // 为空表示不认证
	AllowPlainAuth bool     `json:"allow_plain_auth"` // 允许在 encryption=none 时认证（密码明文传输）
	FromAddr       string   `json:"from_addr"`
	FromName       string   `json:"from_name"`
	To             []string `json:"to"`
	Cc             []string `json:"cc"`
	SubjectPrefix  string   `json:"subject_prefix"`
}

// DefaultPort 返回加密方式对应的常用端口。
func DefaultPort(enc string) int {
	switch enc {
	case EncSSL:
		return 465
	case EncStartTLS:
		return 587
	}
	return 25
}

// EncLabel 返回加密方式的显示名。
func EncLabel(enc string) string {
	switch enc {
	case EncNone:
		return "不加密"
	case EncStartTLS:
		return "STARTTLS"
	case EncSSL:
		return "SSL/TLS"
	}
	return enc
}

// HostHint 是列表里展示的“机器人”一栏：SMTP 主机、端口与加密方式（不含用户名与密码）。
func (c *EmailConfig) HostHint() string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d · %s", c.Host, c.Port, EncLabel(c.Encryption))
}

// hasCtl 判断是否含 CR / LF / NUL 等控制字符（头注入的载体）。
func hasCtl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// stripCtl 剔除控制字符（用于头部字段：主题、显示名）。
func stripCtl(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			if r == '\t' {
				return ' '
			}
			return -1
		}
		return r
	}, s)
}

// ParseAddr 校验并规整一个邮箱地址：只接受裸地址（不带显示名、不带尖括号），ASCII，不含控制字符。
func ParseAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("邮箱地址不能为空")
	}
	if hasCtl(s) || len(s) > 254 {
		return "", fmt.Errorf("邮箱地址 %s 不合法", strconv.Quote(cutRunesN(stripCtl(s), 40)))
	}
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 || s[i] == ' ' || s[i] == '<' || s[i] == '>' || s[i] == ',' || s[i] == ';' || s[i] == '"' {
			return "", fmt.Errorf("邮箱地址 %s 不合法（只支持不带显示名的 ASCII 地址）", strconv.Quote(cutRunesN(s, 40)))
		}
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || strings.Count(s, "@") != 1 || strings.HasPrefix(s, "@") || strings.HasSuffix(s, "@") {
		return "", fmt.Errorf("邮箱地址 %s 格式不正确", strconv.Quote(cutRunesN(s, 40)))
	}
	return s, nil
}

func cutRunesN(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// SplitAddrs 把逗号 / 分号 / 空白分隔的地址串拆开。
func SplitAddrs(s string) []string {
	f := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '，' || r == '；' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	})
	return f
}

// normAddrList 校验一组地址，去重（不区分大小写），返回规整后的列表。
func normAddrList(label string, in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		a, err := ParseAddr(raw)
		if err != nil {
			return nil, fmt.Errorf("%s：%v", label, err)
		}
		if k := strings.ToLower(a); !seen[k] {
			seen[k] = true
			out = append(out, a)
		}
	}
	return out, nil
}

var hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.\-]{0,251}[A-Za-z0-9])?$|^\[[0-9A-Fa-f:.]+\]$`)

// Normalize 校验并规整配置，返回面向用户的中文错误。password 为本次提交的密码（可空，仅用于校验字符，不会写进错误）。
func (c *EmailConfig) Normalize(password string) error {
	c.Host = strings.TrimSpace(c.Host)
	if c.Host == "" {
		return fmt.Errorf("请填写 SMTP 主机")
	}
	if len(c.Host) > 253 || !hostRe.MatchString(c.Host) {
		return fmt.Errorf("SMTP 主机不合法（只允许主机名或 IP，不要带 smtp:// 和端口）")
	}
	c.Host = strings.Trim(c.Host, "[]")
	c.Encryption = strings.ToLower(strings.TrimSpace(c.Encryption))
	switch c.Encryption {
	case EncNone, EncStartTLS, EncSSL:
	case "":
		c.Encryption = EncStartTLS
	default:
		return fmt.Errorf("不支持的加密方式 %s（可选 none / starttls / ssl）", strconv.Quote(cutRunesN(stripCtl(c.Encryption), 20)))
	}
	if c.Port == 0 {
		c.Port = DefaultPort(c.Encryption)
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("端口需要在 1 到 65535 之间")
	}
	c.Username = strings.TrimSpace(c.Username)
	if hasCtl(c.Username) || len(c.Username) > 200 {
		return fmt.Errorf("SMTP 用户名不合法（最多 200 个字符，不能含换行）")
	}
	if hasCtl(password) || len(password) > 200 {
		return fmt.Errorf("SMTP 密码不合法（最多 200 个字符，不能含换行）")
	}
	if c.Username == "" && password != "" {
		return fmt.Errorf("填写了密码但没有填写用户名")
	}
	if c.Username != "" && c.Encryption == EncNone && !c.AllowPlainAuth {
		return fmt.Errorf("加密方式为“不加密”时默认不允许认证（用户名和密码会以明文在网络上传输）。请改用 STARTTLS 或 SSL，或勾选“允许明文认证”并自行承担风险")
	}
	if c.Encryption != EncNone {
		c.AllowPlainAuth = false // 只在“不加密”时才有意义，避免残留
	}
	from, err := ParseAddr(c.FromAddr)
	if err != nil {
		return fmt.Errorf("发件人地址：%v", err)
	}
	c.FromAddr = from
	c.FromName = strings.TrimSpace(c.FromName)
	if hasCtl(c.FromName) || len([]rune(c.FromName)) > maxFromNameRunes {
		return fmt.Errorf("发件人显示名不合法（最多 %d 个字符，不能含换行）", maxFromNameRunes)
	}
	c.SubjectPrefix = strings.TrimSpace(c.SubjectPrefix)
	if hasCtl(c.SubjectPrefix) || len([]rune(c.SubjectPrefix)) > maxPrefixRunes {
		return fmt.Errorf("主题前缀不合法（最多 %d 个字符，不能含换行）", maxPrefixRunes)
	}
	if c.To, err = normAddrList("收件人（To）", c.To); err != nil {
		return err
	}
	if c.Cc, err = normAddrList("抄送（Cc）", c.Cc); err != nil {
		return err
	}
	// Cc 里与 To 重复的去掉
	to := map[string]bool{}
	for _, a := range c.To {
		to[strings.ToLower(a)] = true
	}
	var cc []string
	for _, a := range c.Cc {
		if !to[strings.ToLower(a)] {
			cc = append(cc, a)
		}
	}
	c.Cc = cc
	if len(c.To)+len(c.Cc) > MaxFixedRecipients {
		return fmt.Errorf("固定收件人（To 与 Cc 合计）最多 %d 个", MaxFixedRecipients)
	}
	if c.To == nil {
		c.To = []string{}
	}
	if c.Cc == nil {
		c.Cc = []string{}
	}
	return nil
}

// ---- 邮件内容 ----

// EmailMessage 是要发送的一封邮件。
type EmailMessage struct {
	From, FromName string
	To, Cc         []string
	Subject        string
	Text, HTML     string
	Date           time.Time
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// foldAddrs 把地址列表写成可折行的头部值（单行不超过 998 字符，这里 72 折一次）。
func foldAddrs(addrs []string) string {
	var b strings.Builder
	line := 0
	for i, a := range addrs {
		piece := "<" + a + ">"
		if i > 0 {
			b.WriteString(",")
			line++
			if line+len(piece)+1 > 72 {
				b.WriteString("\r\n ")
				line = 1
			} else {
				b.WriteString(" ")
				line++
			}
		}
		b.WriteString(piece)
		line += len(piece)
	}
	return b.String()
}

// BuildEmail 生成 RFC 5322 格式的 multipart/alternative 邮件（text/plain + text/html，均为 quoted-printable）。
// 头部字段一律剔除换行等控制字符，地址必须通过 ParseAddr；主题用 RFC 2047 编码。
func BuildEmail(m EmailMessage) ([]byte, error) {
	from, err := ParseAddr(m.From)
	if err != nil {
		return nil, fmt.Errorf("发件人地址：%v", err)
	}
	if len(m.To)+len(m.Cc) == 0 {
		return nil, fmt.Errorf("没有收件人")
	}
	for _, list := range [][]string{m.To, m.Cc} {
		for _, a := range list {
			if _, err := ParseAddr(a); err != nil {
				return nil, err
			}
		}
	}
	subject := cutRunes(strings.TrimSpace(stripCtl(m.Subject)), maxSubjectRunes)
	name := cutRunes(strings.TrimSpace(stripCtl(m.FromName)), maxFromNameRunes)
	fromHdr := "<" + from + ">"
	if name != "" {
		fromHdr = (&mail.Address{Name: name, Address: from}).String()
	}
	date := m.Date
	if date.IsZero() {
		date = time.Now()
	}
	domain := from[strings.LastIndex(from, "@")+1:]

	var buf bytes.Buffer
	hdr := func(k, v string) { buf.WriteString(k + ": " + v + "\r\n") }
	boundary := "=_dbt_" + randHex(16)
	hdr("Date", date.Format(time.RFC1123Z))
	hdr("From", fromHdr)
	if len(m.To) > 0 {
		hdr("To", foldAddrs(m.To))
	}
	if len(m.Cc) > 0 {
		hdr("Cc", foldAddrs(m.Cc))
	}
	hdr("Subject", mime.QEncoding.Encode("utf-8", subject))
	hdr("Message-ID", "<"+randHex(12)+"@"+domain+">")
	hdr("MIME-Version", "1.0")
	hdr("Auto-Submitted", "auto-generated")
	hdr("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	buf.WriteString("\r\n")

	var body bytes.Buffer
	pw := multipart.NewWriter(&body)
	if err := pw.SetBoundary(boundary); err != nil {
		return nil, err
	}
	for _, part := range []struct{ ctype, text string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", part.ctype+"; charset=utf-8")
		h.Set("Content-Transfer-Encoding", "quoted-printable")
		w, err := pw.CreatePart(h)
		if err != nil {
			return nil, err
		}
		qw := quotedprintable.NewWriter(w)
		if _, err := qw.Write([]byte(part.text)); err != nil {
			return nil, err
		}
		if err := qw.Close(); err != nil {
			return nil, err
		}
	}
	if err := pw.Close(); err != nil {
		return nil, err
	}
	buf.Write(body.Bytes())
	return buf.Bytes(), nil
}

func cutRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---- 正文 ----

var (
	urlRe     = regexp.MustCompile(`https?://[^\s<>"'，。；、）)]+`)
	linkLine  = regexp.MustCompile(`^\s*[^：:\n]{1,20}[：:]\s*https?://\S+\s*$`)
	linkMu    sync.RWMutex
	linkBaseV string
)

// SetPublicURL 设置邮件里链接的基准地址（PUBLIC_URL）。configured 为 false（没有显式配置，系统只是回落到了本机地址）时，
// 邮件里不放任何链接，只留工单编号。进程启动时调用一次。
func SetPublicURL(u string, configured bool) {
	linkMu.Lock()
	defer linkMu.Unlock()
	if configured {
		linkBaseV = strings.TrimRight(strings.TrimSpace(u), "/")
	} else {
		linkBaseV = ""
	}
}

func linkBase() string {
	linkMu.RLock()
	defer linkMu.RUnlock()
	return linkBaseV
}

// scrubLinks 在没有配置对外地址时去掉正文里的链接（本机地址对收件人没有意义），只保留工单编号等其它信息。
func scrubLinks(content, base string) string {
	if base != "" {
		return content
	}
	var out []string
	for _, ln := range strings.Split(content, "\n") {
		if linkLine.MatchString(ln) {
			continue
		}
		out = append(out, urlRe.ReplaceAllString(ln, ""))
	}
	return strings.Join(out, "\n")
}

// htmlLinkify 转义文本，并把以 base 开头的链接变成可点击的 <a>（其它地址只当文字显示，防止 javascript: 之类）。
func htmlLinkify(s, base string) string {
	var b strings.Builder
	last := 0
	for _, loc := range urlRe.FindAllStringIndex(s, -1) {
		b.WriteString(html.EscapeString(s[last:loc[0]]))
		u := s[loc[0]:loc[1]]
		if base != "" && strings.HasPrefix(u, base+"/") {
			e := html.EscapeString(u)
			b.WriteString(`<a href="` + e + `">` + e + `</a>`)
		} else {
			b.WriteString(html.EscapeString(u))
		}
		last = loc[1]
	}
	b.WriteString(html.EscapeString(s[last:]))
	return b.String()
}

// EmailBodies 把事件渲染成纯文本与 HTML 两份正文。HTML 里所有变量都转义。
func EmailBodies(ev Event, chName string) (text, htmlBody string) {
	base := linkBase()
	content := scrubLinks(strings.ReplaceAll(ev.Content, "\r", ""), base)
	evLabel := EventLabel(ev.Name)
	if ev.Specific != "" {
		evLabel = EventLabel(ev.Specific)
	}
	foot := "事件：" + evLabel + " · 渠道：" + chName // i18n-ignore 通知正文 / 模板内容
	if base != "" {
		foot += " · 系统地址：" + base // i18n-ignore 通知正文 / 模板内容
	}

	var t strings.Builder
	t.WriteString(strings.TrimSpace(stripCtl(ev.Title)) + "\n\n" + strings.TrimSpace(content) + "\n\n-- \n" + foot + "\n本邮件由 dbtool 自动发送，请勿直接回复。\n") // i18n-ignore 通知正文 / 模板内容
	text = t.String()

	var h strings.Builder
	h.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8"></head><body style="margin:0;padding:16px;background:#f5f6f8;font-family:-apple-system,'Segoe UI','PingFang SC','Microsoft YaHei',sans-serif;color:#1f2933">`)
	h.WriteString(`<div style="max-width:720px;margin:0 auto;background:#ffffff;border:1px solid #dfe3e8;border-radius:6px;padding:20px">`)
	h.WriteString(`<h2 style="margin:0 0 14px;font-size:17px;line-height:1.4">` + html.EscapeString(strings.TrimSpace(stripCtl(ev.Title))) + `</h2>`)
	var rows strings.Builder
	var para strings.Builder
	flushPara := func() {
		if para.Len() > 0 {
			rows.WriteString(`<tr><td colspan="2" style="padding:4px 0;white-space:pre-wrap;word-break:break-word">` + para.String() + `</td></tr>`)
			para.Reset()
		}
	}
	lastVal := false // 上一行是 “键：值”，后面不含冒号的行并入它的值（多行 SQL 预览）
	var curKey, curVal string
	flushRow := func() {
		if lastVal {
			rows.WriteString(`<tr><td style="padding:4px 12px 4px 0;color:#52606d;white-space:nowrap;vertical-align:top">` + html.EscapeString(curKey) +
				`</td><td style="padding:4px 0;white-space:pre-wrap;word-break:break-word">` + htmlLinkify(curVal, base) + `</td></tr>`)
			lastVal = false
		}
	}
	for _, ln := range strings.Split(strings.TrimSpace(content), "\n") {
		if i := strings.Index(ln, "："); i > 0 && i <= 30 && !strings.HasPrefix(ln, " ") {
			flushRow()
			flushPara()
			curKey, curVal, lastVal = ln[:i], ln[i+len("："):], true
			continue
		}
		if lastVal {
			curVal += "\n" + ln
			continue
		}
		if para.Len() > 0 {
			para.WriteString("\n")
		}
		para.WriteString(htmlLinkify(ln, base))
	}
	flushRow()
	flushPara()
	h.WriteString(`<table role="presentation" style="border-collapse:collapse;font-size:14px;line-height:1.6;width:100%">` + rows.String() + `</table>`)
	h.WriteString(`<p style="margin:18px 0 0;padding-top:10px;border-top:1px solid #e4e7eb;color:#7b8794;font-size:12px">` + html.EscapeString(foot) + `<br>本邮件由 dbtool 自动发送，请勿直接回复。</p>`) // i18n-ignore 通知正文 / 模板内容
	h.WriteString(`</div></body></html>`)
	return text, h.String()
}

// EmailSubject 组合主题：前缀 + 事件标题（去掉控制字符）。
func EmailSubject(prefix, title string) string {
	prefix, title = strings.TrimSpace(stripCtl(prefix)), strings.TrimSpace(stripCtl(title))
	if prefix == "" {
		return title
	}
	return prefix + " " + title
}
