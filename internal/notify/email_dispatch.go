// @author: kevinkelly
// @license: Apache Licence
// @file: email_dispatch.go
// @time: 2026/10/28

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 本文件：邮件渠道的收件人解析与发送（接在 Dispatcher 的 deliver / Preview 上）。

// GroupDirectory 是 Directory 的可选能力：返回资源组内的全部启用用户。邮件渠道的“所有人”规则解析为该资源组的全部用户；
// 目录不提供该能力、或事件没有所属资源组时，“所有人”规则不生效（并在发送记录 / 预览里说明原因）。
type GroupDirectory interface {
	ResourceGroupMembers(resourceGroupID uint) ([]Person, error)
}

// Recipients 是一封邮件解析出的收件人。
type Recipients struct {
	To, Cc  []string
	Skipped []string // 没有生效的原因（无邮箱的用户、邮箱不合法、超出上限……），每项一句话
	NoEmail []string // 没有邮箱的用户（显示名），预览里单独提示
}

// Total 是收件人总数。
func (r Recipients) Total() int { return len(r.To) + len(r.Cc) }

func (r Recipients) all() []string { return append(append([]string(nil), r.To...), r.Cc...) }

// emailRecipients 解析收件人：渠道上固定的 To / Cc，加上 @ 规则解析出的用户邮箱（没有邮箱的跳过并记原因），去重，最多 MaxRecipients 个。
func (d *Dispatcher) emailRecipients(ch Channel, ev Event, ments []Mention) Recipients {
	var r Recipients
	seen := map[string]bool{}
	dropped := 0
	add := func(dst *[]string, a string) bool {
		k := strings.ToLower(a)
		if seen[k] {
			return false
		}
		if r.Total() >= MaxRecipients {
			dropped++
			return false
		}
		seen[k] = true
		*dst = append(*dst, a)
		return true
	}
	if cfg := ch.Email; cfg != nil {
		for _, a := range cfg.To {
			if v, err := ParseAddr(a); err == nil {
				add(&r.To, v)
			}
		}
		for _, a := range cfg.Cc {
			if v, err := ParseAddr(a); err == nil {
				add(&r.Cc, v)
			}
		}
	}
	addPerson := func(p Person) {
		name := p.name()
		if strings.TrimSpace(p.Email) == "" {
			r.NoEmail = append(r.NoEmail, name)
			return
		}
		a, err := ParseAddr(p.Email)
		if err != nil {
			r.Skipped = append(r.Skipped, fmt.Sprintf("用户 %s 的邮箱格式不合法", name))
			return
		}
		add(&r.To, a)
	}
	for _, m := range ments {
		switch {
		case m.Raw == "all":
			gd, ok := d.Dir.(GroupDirectory)
			switch {
			case ev.GroupID == 0:
				r.Skipped = append(r.Skipped, "“所有人”规则需要事件所属的资源组，该事件没有")
			case !ok:
				r.Skipped = append(r.Skipped, "“所有人”规则暂不可用：用户目录不支持按资源组列出成员")
			default:
				ps, err := gd.ResourceGroupMembers(ev.GroupID)
				if err != nil {
					r.Skipped = append(r.Skipped, fmt.Sprintf("“所有人”规则解析失败：%s", err.Error()))
					continue
				}
				for _, p := range ps {
					addPerson(p)
				}
			}
		case m.Raw != "":
			if a, err := ParseAddr(m.Raw); err == nil {
				add(&r.To, a)
			} else {
				r.Skipped = append(r.Skipped, fmt.Sprintf("原生标识 %s 不是邮箱地址", cutRunesN(stripCtl(m.Raw), 40)))
			}
		case m.Person != nil:
			addPerson(*m.Person)
		}
	}
	if len(r.NoEmail) > 0 {
		r.NoEmail = uniqStrings(r.NoEmail)
		r.Skipped = append(r.Skipped, fmt.Sprintf("%d 个用户没有邮箱，已跳过：%s", len(r.NoEmail), joinCut(r.NoEmail, 8)))
	}
	if dropped > 0 {
		r.Skipped = append(r.Skipped, fmt.Sprintf("收件人超过 %d 个上限，已丢弃 %d 个", MaxRecipients, dropped))
	}
	return r
}

func uniqStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func joinCut(list []string, n int) string {
	if len(list) <= n {
		return strings.Join(list, "、")
	}
	return fmt.Sprintf("%s 等 %d 个", strings.Join(list[:n], "、"), len(list))
}

// emailKey 是限流的键：已保存的渠道按 ID；未保存（测试发送）按 SMTP 主机与发件人。
func emailKey(ch Channel) string {
	if ch.ID != 0 {
		return fmt.Sprintf("ch:%d", ch.ID)
	}
	if ch.Email != nil {
		return "tmp:" + ch.Email.Host + "|" + ch.Email.FromAddr
	}
	return "tmp:"
}

// emailMessage 按渠道与事件生成邮件。
func emailMessage(ch Channel, ev Event, now time.Time, rc Recipients) ([]byte, error) {
	text, htmlBody := EmailBodies(ev, ch.Name)
	cfg := ch.Email
	subject := EmailSubject(cfg.SubjectPrefix, ev.Title)
	if ev.rich != nil { // 命中了消息模板：正文与主题都按模板
		text, htmlBody = emailRich(ev.rich, ch.Name)
		subject = EmailSubject(cfg.SubjectPrefix, emailRichSubject(ev.rich))
	}
	return BuildEmail(EmailMessage{From: cfg.FromAddr, FromName: cfg.FromName, To: rc.To, Cc: rc.Cc,
		Subject: subject, Text: text, HTML: htmlBody, Date: now})
}

// sendEmail 向邮件渠道发一封邮件。testTo 非空时只发给这一个地址（管理页的测试发送），忽略固定收件人与 @ 规则。
// 返回收件人数、需要记入发送记录的说明（成功时的“跳过了谁”）、各阶段结果。失败不重试超过 1 次（测试发送不重试）。
func (d *Dispatcher) sendEmail(ctx context.Context, ch Channel, ev Event, ments []Mention, testTo string) (int, string, []EmailStage, error) {
	if ch.Email == nil {
		return 0, "", nil, errors.New("邮件渠道缺少 SMTP 配置")
	}
	var rc Recipients
	if testTo != "" {
		a, err := ParseAddr(testTo)
		if err != nil {
			return 0, "", nil, err
		}
		rc.To = []string{a}
	} else {
		rc = d.emailRecipients(ch, ev, ments)
	}
	note := strings.Join(rc.Skipped, "；")
	if rc.Total() == 0 {
		if note != "" {
			return 0, "", nil, fmt.Errorf("没有可用的收件人（%s）", note)
		}
		return 0, "", nil, errors.New("没有可用的收件人")
	}
	if !emailLimiter.allow(emailKey(ch), d.Now(), EmailRatePerMinute) {
		return rc.Total(), "", nil, fmt.Errorf("超出每分钟 %d 封的发送限流，本封已丢弃", EmailRatePerMinute)
	}
	msg, err := emailMessage(ch, ev, d.Now(), rc)
	if err != nil {
		return rc.Total(), "", nil, err
	}
	params := SMTPParams{Cfg: *ch.Email, Pass: ch.Secret, Rcpts: rc.all(), Msg: msg}
	ctx, cancel := context.WithTimeout(ctx, emailTotalTimeout)
	defer cancel()
	res, err := smtpSend(ctx, params)
	if err != nil && !ev.Test && testTo == "" && retryable(err) && ctx.Err() == nil {
		select {
		case <-time.After(emailRetryDelay):
			res, err = smtpSend(ctx, params)
		case <-ctx.Done():
		}
	}
	if err != nil {
		return rc.Total(), "", res.Stages, errors.New(scrub(err.Error(), ch.Secret))
	}
	if len(res.Rejected) > 0 {
		r := fmt.Sprintf("服务器拒绝了 %d 个收件人：%s", len(res.Rejected), joinCut(res.Rejected, 5))
		if note != "" {
			note += "；"
		}
		note += r
	}
	return rc.Total(), note, res.Stages, nil
}

// EmailTestResult 是管理页“测试发送”的结果。
type EmailTestResult struct {
	OK     bool         `json:"ok"`
	Error  string       `json:"error,omitempty"`
	Stages []EmailStage `json:"stages"`
	To     string       `json:"to"`
}

// SendEmailTest 向 to（单个地址）发一封测试邮件，返回 SMTP 各阶段结果（不含密码）。不看渠道是否启用、是否订阅，不重试。
func (d *Dispatcher) SendEmailTest(ctx context.Context, ch Channel, ev Event, to string) EmailTestResult {
	ev.Test = true
	var ments []Mention
	if to == "" { // 不指定测试收件人：按渠道的固定收件人与 @ 规则解析（与其它渠道的测试发送一致）
		ments, _ = d.resolveExplain(ch, ev)
	}
	stages, err := d.deliverTo(ctx, ch, ev, ments, to)
	out := EmailTestResult{OK: err == nil, Stages: stages, To: to}
	if out.Stages == nil {
		out.Stages = newStages()
	}
	if err != nil {
		out.Error = err.Error()
	}
	return out
}

// previewEmail 生成邮件的预览：主题、收件人解析、纯文本正文；不连接 SMTP。
func (d *Dispatcher) previewEmail(ch Channel, ev Event, ments []Mention, skips []Skip) (*Preview, error) {
	if ch.Email == nil {
		return nil, errors.New("邮件渠道缺少 SMTP 配置")
	}
	rc := d.emailRecipients(ch, ev, ments)
	text, _ := EmailBodies(ev, ch.Name)
	title := ev.Title
	if ev.rich != nil {
		text, _ = emailRich(ev.rich, ch.Name)
		title = emailRichSubject(ev.rich)
	}
	payload, _ := json.Marshal(map[string]any{
		"from": ch.Email.FromAddr, "from_name": ch.Email.FromName, "to": nzs(rc.To), "cc": nzs(rc.Cc),
		"subject": cutRunes(EmailSubject(ch.Email.SubjectPrefix, title), maxSubjectRunes), "text": text,
	})
	p := &Preview{Event: ev.Name, Title: ev.Title, Content: ev.Content, WorkTime: d.workTime(ev), Mentions: []MentionView{}, Skipped: []SkipView{},
		Payload: payload, Platform: TypeEmail}
	if ev.rich != nil {
		p.Template, p.Message = ev.rich.Scope, ev.rich
	}
	if ev.Specific != "" {
		p.Event = ev.Specific
	}
	for _, a := range rc.To {
		p.Mentions = append(p.Mentions, MentionView{Label: a, Kind: "user", Resolved: true})
	}
	for _, a := range rc.Cc {
		p.Mentions = append(p.Mentions, MentionView{Label: fmt.Sprintf("%s（抄送）", a), Kind: "user", Resolved: true})
	}
	for _, n := range rc.NoEmail {
		p.Mentions = append(p.Mentions, MentionView{Label: n, Kind: "user", Resolved: false,
			Note: "该用户在用户资料里没有邮箱，收不到邮件（在“用户”页补充邮箱）"})
	}
	for _, s := range skips {
		// 邮件渠道不需要“@ 标识”类的提示，其余规则原因照常展示
		p.Skipped = append(p.Skipped, SkipView{Rule: s.Label, Reason: s.Reason})
	}
	for _, s := range rc.Skipped {
		if !strings.Contains(s, "没有邮箱") { // 无邮箱的用户已在上面逐个列出 // i18n-ignore 与 emailRecipients 里拼出的提示文字匹配（存库 / 接口的原文），不翻译
			p.Skipped = append(p.Skipped, SkipView{Rule: "收件人", Reason: s})
		}
	}
	if rc.Total() == 0 {
		p.Skipped = append(p.Skipped, SkipView{Rule: "收件人", Reason: "没有可用的收件人：真实发送会失败（请配置固定收件人，或让 @ 规则解析到有邮箱的用户）"})
	}
	return p, nil
}

func nzs(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
