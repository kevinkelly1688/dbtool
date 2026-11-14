// @author: kevinkelly
// @license: Apache Licence
// @file: notify_handlers.go
// @time: 2026/10/28

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/query"
)

// ---- 通知渠道管理（仅超级用户） ----

func (s *Server) requireSuperuserPage() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !principalOf(c).IsSuperuser {
			s.renderForbidden(c, "通知渠道的配置仅超级用户可以操作。")
			c.Abort()
			return
		}
		c.Next()
	}
}

func (s *Server) requireSuperuserAPI() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !principalOf(c).IsSuperuser {
			apiFail(c, http.StatusForbidden, "forbidden", "通知渠道的配置仅超级用户可以操作")
			return
		}
		c.Next()
	}
}

func (s *Server) notifyAdminEnabled() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.admin == nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

func (s *Server) notifySettingsPage(c *gin.Context) {
	p := principalOf(c)
	s.render(c, http.StatusOK, "notify_settings.html", pageData{Title: "通知渠道", Nav: "notify", Username: p.Username, Display: p.Display,
		CSRF: sessionOf(c).CSRF, Nav2: s.nav(actorOf(c))})
}

type optionItem struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// notifyEmailReady 邮件渠道的配置表（deploy/sql/210_notify_email.sql）是否可用。仓库不提供该能力时不支持邮件渠道。
func (s *Server) notifyEmailReady() bool {
	if x, ok := s.admin.(notify.EmailAdmin); ok {
		return x.NotifyEmailReady()
	}
	return false
}

// notifyExtReady 渠道扩展表（多资源组、值班人员、细分事件模型）是否可用。仓库不提供该能力时按“可用”处理（由仓库自行取舍）。
func (s *Server) notifyExtReady() bool {
	if x, ok := s.admin.(notify.ExtAdmin); ok {
		return x.NotifyExtReady()
	}
	return true
}

func (s *Server) notifyLogs() (notify.LogStore, bool) {
	l, ok := s.admin.(notify.LogStore)
	if !ok || !l.LogTableReady() {
		return nil, false
	}
	return l, true
}

func (s *Server) apiNotifyOverview(c *gin.Context) {
	ready := s.admin.NotifyTableReady()
	out := gin.H{"ready": ready, "channels": []notify.ChannelView{}}
	if ready {
		list, err := s.admin.ListChannelViews()
		if err != nil {
			writeErr(c, err)
			return
		}
		out["channels"] = list
	}
	groups, err := s.admin.ListResourceGroups()
	if err != nil {
		writeErr(c, err)
		return
	}
	auth, err := s.admin.ListAuthGroups()
	if err != nil {
		writeErr(c, err)
		return
	}
	types := make([]optionItem, 0, len(notify.Types))
	for _, t := range notify.Types {
		types = append(types, optionItem{t, notify.TypeLabel(t)})
	}
	events := make([]optionItem, 0, len(notify.Events))
	for _, e := range notify.Events {
		events = append(events, optionItem{e.Name, e.Label})
	}
	out["groups"], out["auth_groups"], out["types"], out["events"] = groups, auth, types, events
	out["ext_ready"] = s.notifyExtReady()
	out["email_ready"] = s.notifyEmailReady()
	out["hosts"] = platformHosts
	logs, logReady := s.notifyLogs()
	out["log_ready"] = logReady
	if logReady {
		if st, err := logs.SendStats(time.Now().AddDate(0, 0, -7)); err == nil {
			out["stats"] = gin.H{"total": st.Total, "failed": st.Failed}
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

type channelReq struct {
	ID          uint          `json:"id"` // 仅测试发送 / 预览使用：基于已保存的渠道（URL/密钥留空时沿用）
	GroupID     uint          `json:"group_id"`
	GroupIDs    []uint        `json:"group_ids"` // 关联的多个资源组；为空且 group_id 为 0 表示全局
	Name        string        `json:"name"`
	Type        string        `json:"type"`
	URL         string        `json:"url"`
	Secret      string        `json:"secret"`
	ClearSecret bool          `json:"clear_secret"`
	Events      []string      `json:"events"`
	Rules       []notify.Rule `json:"rules"`
	DutyUsers   []string      `json:"duty_users"`
	Enabled     bool          `json:"enabled"`
	// 邮件渠道：SMTP 配置（密码走 secret / clear_secret，与签名密钥同一套：留空不改、永不回显）；test_to 仅测试发送使用，单个测试收件人。
	Email  *notify.EmailConfig `json:"email"`
	TestTo string              `json:"test_to"`

	// 仅测试发送 / 预览：模拟某个事件（为空时测试发送沿用旧行为：忽略事件过滤，预览按“待处理”）。
	Event           string `json:"event"`
	WorkTime        *bool  `json:"work_time"`         // 模拟“现在是否工作时间”；为空取当前时间
	ReviewerGroupID uint   `json:"reviewer_group_id"` // 模拟当前审批级所在的权限组，让“审批人”规则有对象可解析
}

var (
	rawIDRe      = regexp.MustCompile(`^[A-Za-z0-9_\-.:@]{1,128}$`)
	dingMobileRe = regexp.MustCompile(`^\d{11}$`)
	dingUserRe   = regexp.MustCompile(`^[A-Za-z0-9_\-.]{1,64}$`)
	feishuOpenRe = regexp.MustCompile(`^ou_[A-Za-z0-9]+$`)
	slackIDRe    = regexp.MustCompile(`^([UWS][A-Z0-9]{2,31}|here|channel|everyone)$`)
	wecomUserRe  = regexp.MustCompile(`^[A-Za-z0-9_\-.@]{1,64}$`) // 企业微信成员 userid
)

// platformHosts 是各平台机器人 webhook 的主机名；保存地址时必须匹配（本机回环地址除外，便于联调）。
var platformHosts = map[string]string{
	notify.TypeDingTalk: "oapi.dingtalk.com",
	notify.TypeWeCom:    "qyapi.weixin.qq.com",
	notify.TypeFeishu:   "open.feishu.cn",
	notify.TypeLark:     "open.larksuite.com",
	notify.TypeSlack:    "hooks.slack.com",
	notify.TypeTelegram: "api.telegram.org",
}

// noSignType 是没有签名校验的机器人类型：页面不显示“签名密钥”，服务端也不保存（企业微信、Telegram 的密钥都在 Webhook 地址里）。
func noSignType(typ string) bool { return typ == notify.TypeWeCom || typ == notify.TypeTelegram }

// secretKindChanged 渠道在“邮件”与“机器人”之间切换：secret 字段的含义（SMTP 密码 / 签名密钥）变了，旧值不能沿用。
func secretKindChanged(from, to string) bool {
	return from != "" && from != to && (from == notify.TypeEmail || to == notify.TypeEmail)
}

func badReq(msg string) error { return query.NewError(400, "bad_request", msg) }

// webhookHostOK 校验机器人地址的主机：必须是平台域名（https），或本机回环地址（http / https 均可，仅用于联调）。
func webhookHostOK(typ string, u *url.URL) bool {
	h := strings.ToLower(u.Hostname())
	if want, ok := platformHosts[typ]; ok && h == want {
		return u.Scheme == "https"
	}
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// rawIDFormatOK 校验“渠道原生标识”是否符合该平台的格式。
func rawIDFormatOK(typ, v string) bool {
	switch typ {
	case notify.TypeDingTalk:
		return dingMobileRe.MatchString(v) || dingUserRe.MatchString(v)
	case notify.TypeWeCom:
		return dingMobileRe.MatchString(v) || wecomUserRe.MatchString(v)
	case notify.TypeTelegram:
		return notify.TelegramRawOK(v)
	case notify.TypeFeishu, notify.TypeLark:
		return feishuOpenRe.MatchString(v)
	case notify.TypeSlack:
		return slackIDRe.MatchString(v)
	}
	return false
}

func rawIDHint(typ string) string {
	switch typ {
	case notify.TypeDingTalk:
		return "手机号（11 位）或 userId"
	case notify.TypeWeCom:
		return "成员账号 userid，或 11 位手机号"
	case notify.TypeTelegram:
		return "Telegram 用户名（5–32 位字母、数字、下划线，字母开头，可带 @）或数字用户 ID"
	case notify.TypeFeishu, notify.TypeLark:
		return "open_id（ou_ 开头）"
	case notify.TypeSlack:
		return "用户 U…/W…、用户组 S…（可直接粘贴 <!subteam^S…>），或 here / channel / everyone"
	}
	return ""
}

// validateChannel 校验并规整渠道配置。requireURL 为 true（创建）时地址必填；checkName 为 true（保存）时校验渠道名唯一（selfID 是正在修改的渠道）。
func (s *Server) validateChannel(in *channelReq, requireURL bool, selfID uint, checkName bool) error {
	in.Name = strings.TrimSpace(in.Name)
	if n := len([]rune(in.Name)); n == 0 || n > 50 {
		return badReq("渠道名称需要 1 到 50 个字符")
	}
	known := false
	for _, t := range notify.Types {
		known = known || t == in.Type
	}
	if !known {
		return badReq("不支持的渠道类型")
	}
	if checkName {
		views, err := s.admin.ListChannelViews()
		if err != nil {
			return err
		}
		for _, v := range views {
			if v.ID != selfID && strings.EqualFold(strings.TrimSpace(v.Name), in.Name) {
				return badReq(fmt.Sprintf("渠道名称「%s」已存在，请换一个", in.Name))
			}
		}
	}
	if err := s.validateGroups(in); err != nil {
		return err
	}
	in.URL = strings.TrimSpace(in.URL)
	if in.Type == notify.TypeEmail {
		in.URL = "" // 邮件渠道没有 webhook 地址
		if !s.notifyEmailReady() {
			return query.NewError(http.StatusConflict, "table_missing", "邮件渠道需要先在数据库里执行 deploy/sql/210_notify_email.sql")
		}
		if in.Email == nil {
			return badReq("请填写 SMTP 配置")
		}
		if err := in.Email.Normalize(in.Secret); err != nil {
			return badReq(err.Error())
		}
	} else {
		in.Email = nil
	}
	if in.URL == "" && requireURL && in.Type != notify.TypeEmail {
		return badReq("请填写机器人的 webhook 地址")
	}
	if in.URL != "" {
		u, err := url.Parse(in.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return badReq("webhook 地址必须是 http:// 或 https:// 开头的完整地址")
		}
		if len(in.URL) > 1000 {
			return badReq("webhook 地址过长（最多 1000 个字符）")
		}
		if !webhookHostOK(in.Type, u) {
			return badReq(fmt.Sprintf("%s 机器人的 webhook 地址应以 https://%s/ 开头", notify.TypeLabel(in.Type), platformHosts[in.Type]))
		}
		if in.Type == notify.TypeTelegram { // 路径要是 /bot<Bot Token>/sendMessage，并带上 chat_id
			if err := notify.CheckTelegramURL(in.URL); err != nil {
				return badReq(err.Error())
			}
		}
	}
	if len(in.Secret) > 200 {
		return badReq("签名密钥 / SMTP 密码过长（最多 200 个字符）")
	}
	if noSignType(in.Type) {
		in.Secret = "" // 企业微信 / Telegram 机器人没有加签：不保存签名密钥（从别的类型改过来时旧密钥在 apiSaveChannel 里清掉）
	}
	validEvent := map[string]bool{}
	for _, e := range notify.Events {
		validEvent[e.Name] = true
	}
	var events []string
	seen := map[string]bool{}
	for _, e := range in.Events {
		if !validEvent[e] {
			return badReq(fmt.Sprintf("未知的事件：%s", e))
		}
		if !seen[e] {
			seen[e] = true
			events = append(events, e)
		}
	}
	in.Events = events
	if len(in.Rules) > 30 {
		return badReq("@ 规则最多 30 条")
	}
	auth, err := s.admin.ListAuthGroups()
	if err != nil {
		return err
	}
	authOK := map[string]bool{}
	for _, g := range auth {
		authOK[strconv.Itoa(int(g.ID))] = true
	}
	for i := range in.Rules {
		r := &in.Rules[i]
		r.Value = strings.TrimSpace(r.Value)
		for _, e := range r.Events {
			if !validEvent[e] {
				return badReq(fmt.Sprintf("@ 规则里有未知的事件：%s", e))
			}
		}
		if r.Target == notify.TargetAll && in.Type == notify.TypeTelegram {
			return badReq("Telegram 没有“@所有人”的功能，请去掉“所有人”规则；要提醒到人，用“渠道原生标识”填 Telegram 用户名或用户 ID")
		}
		switch r.Target {
		case notify.TargetApplicant, notify.TargetReviewers, notify.TargetAll, notify.TargetDBADuty:
			r.Value = ""
		case notify.TargetAuthGroup:
			if !authOK[r.Value] {
				return badReq("@ 规则里选择的权限组不存在")
			}
		case notify.TargetUser:
			if r.Value == "" || len(r.Value) > 150 {
				return badReq("@ 规则里的用户名不能为空")
			}
		case notify.TargetRaw:
			if in.Type == notify.TypeEmail {
				a, err := notify.ParseAddr(r.Value)
				if err != nil {
					return badReq(fmt.Sprintf("@ 规则里的收件地址不合法：%s", err.Error()))
				}
				r.Value = a
				break
			}
			if in.Type == notify.TypeSlack {
				// Slack 里复制出来的 <!subteam^S0ARY4FQATB>、!subteam^…、<@U…>、@here 等写法，规整成 ID 再保存
				r.Value = notify.NormalizeSlackTarget(r.Value)
			}
			if in.Type == notify.TypeTelegram {
				r.Value = notify.NormalizeTelegramTarget(r.Value) // @dba_wang → dba_wang
			}
			if !rawIDRe.MatchString(r.Value) {
				return badReq(fmt.Sprintf("渠道原生标识 %s 不合法（只允许字母、数字和 _-.:@，最长 128 位）", strconv.Quote(r.Value)))
			}
			if !rawIDFormatOK(in.Type, r.Value) {
				return badReq(fmt.Sprintf("渠道原生标识 %s 不符合%s的格式：%s", strconv.Quote(r.Value), notify.TypeLabel(in.Type), rawIDHint(in.Type)))
			}
		default:
			return badReq(fmt.Sprintf("未知的 @ 目标类型：%s", r.Target))
		}
	}
	return s.validateDuty(in)
}

// validateGroups 规整关联的资源组：group_ids 优先（去重，group_id 取第一个）；都为空表示全局。
func (s *Server) validateGroups(in *channelReq) error {
	ids := in.GroupIDs
	if len(ids) == 0 && in.GroupID != 0 {
		ids = []uint{in.GroupID}
	}
	if len(ids) == 0 {
		in.GroupID, in.GroupIDs = 0, nil
		return nil
	}
	if len(ids) > 50 {
		return badReq("一个渠道最多关联 50 个资源组")
	}
	groups, err := s.admin.ListResourceGroups()
	if err != nil {
		return err
	}
	exist := map[uint]bool{}
	for _, g := range groups {
		exist[g.ID] = true
	}
	var out []uint
	seen := map[uint]bool{}
	for _, id := range ids {
		if id == 0 || !exist[id] {
			return badReq("所选资源组不存在")
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) > 1 && !s.notifyExtReady() {
		return query.NewError(http.StatusConflict, "table_missing", "一个渠道关联多个资源组需要先在数据库里执行 deploy/sql/091_notify_ext.sql")
	}
	in.GroupIDs, in.GroupID = out, out[0]
	return nil
}

// validateDuty 校验 DBA 值班人员：用户名去重、必须是已有用户。
func (s *Server) validateDuty(in *channelReq) error {
	var out []string
	seen := map[string]bool{}
	for _, n := range in.DutyUsers {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		if len(n) > 150 {
			return badReq("值班人员用户名过长")
		}
		u, err := s.repo.GetUserByName(n)
		if err != nil {
			return err
		}
		if u == nil {
			return badReq(fmt.Sprintf("值班人员「%s」不存在", n))
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) > 20 {
		return badReq("值班人员最多 20 人")
	}
	if len(out) > 0 && !s.notifyExtReady() {
		return query.NewError(http.StatusConflict, "table_missing", "配置 DBA 值班人员需要先在数据库里执行 deploy/sql/091_notify_ext.sql")
	}
	in.DutyUsers = out
	return nil
}

func inputOf(in channelReq, operator string) notify.ChannelInput {
	return notify.ChannelInput{GroupID: in.GroupID, GroupIDs: in.GroupIDs, DutyUsers: in.DutyUsers, Name: in.Name, Type: in.Type, URL: in.URL, Secret: in.Secret,
		ClearSecret: in.ClearSecret, Events: in.Events, Rules: in.Rules, Enabled: in.Enabled, Operator: operator, Email: in.Email}
}

func (s *Server) notifyReady(c *gin.Context) bool {
	if !s.admin.NotifyTableReady() {
		apiFail(c, http.StatusConflict, "table_missing", "通知渠道表还没有创建，请先在数据库里执行 deploy/sql/001_notify_channel.sql")
		return false
	}
	return true
}

// auditNotify 写一条渠道管理的操作审计。extra 里不放 webhook 地址和密钥。写失败只记日志。
func (s *Server) auditNotify(c *gin.Context, action, extra string) {
	db, _, ok := s.DB()
	if !ok {
		return
	}
	p := principalOf(c)
	e := model.AuditEntry{UserID: p.ID, UserName: cutRunes(p.Username, 30), UserDisplay: cutRunes(p.Display, 50), Action: action, ExtraInfo: cutRunes(extra, 1000)}
	if err := db.Create(&e).Error; err != nil {
		slog.Error("写通知渠道审计失败", "action", action, "err", err)
	}
}

// groupsText 把资源组 ID 列表翻译成名称（审计用）；空表示全局。
func (s *Server) groupsText(ids []uint) string {
	if len(ids) == 0 {
		return "全局（所有资源组）" // i18n-ignore 审计文本（存库）
	}
	names := map[uint]string{}
	if gs, err := s.admin.ListResourceGroups(); err == nil {
		for _, g := range gs {
			names[g.ID] = g.Name
		}
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		if n, ok := names[id]; ok {
			parts = append(parts, n)
		} else {
			parts = append(parts, "#"+strconv.Itoa(int(id)))
		}
	}
	return strings.Join(parts, "、")
}

func (s *Server) apiSaveChannel(c *gin.Context) {
	if !s.notifyReady(c) {
		return
	}
	var req channelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	var id uint
	var cur *notify.Channel
	if v := c.Param("id"); v != "" {
		n, ok := uintParam(c, "id")
		if !ok {
			return
		}
		id = n
		var err error
		if cur, err = s.admin.GetChannel(id); err != nil {
			writeErr(c, err)
			return
		} else if cur == nil {
			apiFail(c, http.StatusNotFound, "not_found", "渠道不存在")
			return
		}
	}
	if err := s.validateChannel(&req, id == 0, id, true); err != nil {
		writeErr(c, err)
		return
	}
	if cur != nil && cur.Type != req.Type && req.URL == "" && req.Type != notify.TypeEmail {
		writeErr(c, badReq("更换渠道类型后需要重新填写 Webhook 地址"))
		return
	}
	if cur != nil && secretKindChanged(cur.Type, req.Type) && req.Secret == "" {
		req.ClearSecret = true // 机器人签名密钥与 SMTP 密码不是一回事：换类型后旧值不能沿用
	}
	if cur != nil && noSignType(req.Type) && cur.Secret != "" {
		req.ClearSecret = true // 企业微信 / Telegram 没有加签：从钉钉 / 飞书 / Slack 改过来时，旧的签名密钥不再保留
	}
	saved, err := s.admin.SaveChannel(id, inputOf(req, principalOf(c).Username))
	if err != nil {
		writeErr(c, err)
		return
	}
	evText := "全部事件" // i18n-ignore 审计文本（存库）
	if len(req.Events) > 0 {
		evText = strconv.Itoa(len(req.Events)) + " 项事件" // i18n-ignore 审计文本（存库）
	}
	secretState := map[bool]string{true: "已清除", false: map[bool]string{true: "已更新", false: "未改"}[req.Secret != ""]}[req.ClearSecret]                                // i18n-ignore 审计文本（存库）
	extra := fmt.Sprintf("渠道「%s」#%d（%s，%s，订阅 %s，%d 条 @ 规则，值班 %d 人，%s）；Webhook %s；签名密钥 %s", req.Name, saved, notify.TypeLabel(req.Type), s.groupsText(req.GroupIDs), // i18n-ignore 审计文本（存库）
		evText, len(req.Rules), len(req.DutyUsers), map[bool]string{true: "启用", false: "停用"}[req.Enabled], // i18n-ignore 审计文本（存库）
		map[bool]string{true: "已更新", false: "未改"}[req.URL != ""], secretState) // i18n-ignore 审计文本（存库）
	if req.Type == notify.TypeEmail { // 审计里不放密码，也不放收件人明细
		e := req.Email
		extra = fmt.Sprintf("渠道「%s」#%d（邮件，%s，订阅 %s，%d 条 @ 规则，值班 %d 人，%s）；SMTP %s:%d %s%s%s；认证用户 %s；发件人 %s；固定收件人 %d 个；密码 %s", // i18n-ignore 审计文本（存库）
			req.Name, saved, s.groupsText(req.GroupIDs), evText, len(req.Rules), len(req.DutyUsers), map[bool]string{true: "启用", false: "停用"}[req.Enabled], // i18n-ignore 审计文本（存库）
			e.Host, e.Port, notify.EncLabel(e.Encryption), map[bool]string{true: "，不校验证书", false: ""}[e.SkipVerify], map[bool]string{true: "，允许明文认证", false: ""}[e.AllowPlainAuth], // i18n-ignore 审计文本（存库）
			map[bool]string{true: e.Username, false: "（无）"}[e.Username != ""], e.FromAddr, len(e.To)+len(e.Cc), secretState) // i18n-ignore 审计文本（存库）
	}
	action := "通知渠道-修改" // i18n-ignore 审计文本（存库）
	if id == 0 {
		action = "通知渠道-新增" // i18n-ignore 审计文本（存库）
	}
	s.auditNotify(c, action, extra)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": saved}})
}

func (s *Server) apiDeleteChannel(c *gin.Context) {
	if !s.notifyReady(c) {
		return
	}
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	cur, err := s.admin.GetChannel(id)
	if err != nil {
		writeErr(c, err)
		return
	}
	if err := s.admin.DeleteChannel(id); err != nil {
		writeErr(c, err)
		return
	}
	if cur != nil {
		s.auditNotify(c, "通知渠道-删除", fmt.Sprintf("渠道「%s」#%d（%s）", cur.Name, id, notify.TypeLabel(cur.Type))) // i18n-ignore 审计文本（存库）
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// buildChannel 用表单里的配置（URL、密钥留空则沿用已保存的）组装一个渠道，测试发送与预览共用。
func (s *Server) buildChannel(req *channelReq, needURL bool) (notify.Channel, error) {
	ch := notify.Channel{}
	if req.ID != 0 {
		cur, err := s.admin.GetChannel(req.ID)
		if err != nil {
			return ch, err
		}
		if cur == nil {
			return ch, query.NewError(404, "not_found", "渠道不存在")
		}
		ch = *cur
	}
	if err := s.validateChannel(req, needURL && req.ID == 0, req.ID, false); err != nil {
		return ch, err
	}
	if secretKindChanged(ch.Type, req.Type) {
		ch.Secret = "" // 换了类型：旧的签名密钥 / SMTP 密码不能沿用
	}
	ch.Name, ch.Type, ch.GroupID, ch.GroupIDs, ch.DutyUsers = req.Name, req.Type, req.GroupID, req.GroupIDs, req.DutyUsers
	ch.Email = nil
	if req.Type == notify.TypeEmail && req.Email != nil {
		e := *req.Email
		ch.Email, ch.WebhookURL = &e, ""
	}
	if req.URL != "" {
		ch.WebhookURL = req.URL
	}
	if req.ClearSecret {
		ch.Secret = ""
	} else if req.Secret != "" {
		ch.Secret = req.Secret
	}
	if noSignType(ch.Type) {
		ch.Secret = "" // 企业微信 / Telegram 没有加签（预览里不要显示“请求体（未含签名）”）
	}
	ch.Rules = append([]notify.Rule(nil), req.Rules...)
	return ch, nil
}

// sampleGroup 返回模拟事件所属的资源组：渠道关联的第一个；全局渠道取第一个资源组（让“权限组成员”之类的规则有对象可解析）。
func (s *Server) sampleGroup(req *channelReq) uint {
	if req.GroupID != 0 {
		return req.GroupID
	}
	if gs, err := s.admin.ListResourceGroups(); err == nil && len(gs) > 0 {
		return gs[0].ID
	}
	return 0
}

// apiTestChannel 用表单里的配置发一条消息（真发）。
//   - 不带 event：沿用原行为——忽略规则里的事件过滤，让所有 @ 对象都出现，其中“提交人”解析为当前操作者；
//   - 带 event：模拟该事件（样例工单内容），规则里的事件过滤与“仅工作时间 @”按真实逻辑生效（work_time 可模拟工作 / 非工作时间）。
func (s *Server) apiTestChannel(c *gin.Context) {
	if s.disp == nil {
		apiFail(c, http.StatusServiceUnavailable, "unavailable", "通知发送器未启用")
		return
	}
	var req channelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	ch, err := s.buildChannel(&req, true)
	if err != nil {
		writeErr(c, err)
		return
	}
	p := principalOf(c)
	var ev notify.Event
	if req.Event == "" {
		for i := range ch.Rules {
			ch.Rules[i].Events = nil // 测试时忽略事件过滤，让所有 @ 对象都出现
		}
		ev = notify.Event{Name: notify.EventPending, GroupID: req.GroupID, Applicant: p.Username, Test: true,
			Title:   "[dbtool] 通知渠道测试",                                                                                                                                                             // i18n-ignore 测试消息正文
			Content: fmt.Sprintf("这是一条测试消息，由 %s 在 %s 触发。\n收到这条消息说明机器人地址%s配置正确。", p.Username, time.Now().Format("2006-01-02 15:04:05"), map[bool]string{true: "与签名密钥", false: ""}[ch.Secret != ""])} // i18n-ignore 测试消息正文
		if ch.Type == notify.TypeEmail {
			ev.Content = fmt.Sprintf("这是一封测试邮件，由 %s 在 %s 触发。\n收到这封邮件说明 SMTP 服务器、发件人和收件地址配置正确。", p.Username, time.Now().Format("2006-01-02 15:04:05")) // i18n-ignore 测试消息正文
		}
		ev.WorkTime = req.WorkTime
	} else {
		if ev, err = notify.SampleEvent(req.Event, p.Username, s.sampleGroup(&req), req.ReviewerGroupID); err != nil {
			writeErr(c, badReq(fmt.Sprintf("未知的事件：%s", req.Event)))
			return
		}
		ev.WorkTime = req.WorkTime
		ev.Title = "[dbtool 测试] " + ev.Title // i18n-ignore 测试消息正文
	}
	if ch.Type == notify.TypeEmail {
		s.testEmail(c, &req, ch, ev)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := s.disp.SendTest(ctx, ch, ev); err != nil {
		s.auditNotify(c, "通知渠道-测试发送", fmt.Sprintf("渠道「%s」（%s）；结果：失败", ch.Name, notify.TypeLabel(ch.Type))) // i18n-ignore 审计文本（存库）
		apiFail(c, http.StatusBadGateway, "send_failed", fmt.Sprintf("发送失败：%s", err.Error()))
		return
	}
	s.auditNotify(c, "通知渠道-测试发送", fmt.Sprintf("渠道「%s」（%s）；结果：成功；%s", ch.Name, notify.TypeLabel(ch.Type), map[bool]string{true: "模拟事件 " + req.Event, false: "默认测试消息"}[req.Event != ""])) // i18n-ignore 审计文本（存库）
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// testEmail 邮件渠道的测试发送：真的连接 SMTP 并发出一封邮件，返回各阶段（连接 / STARTTLS / 认证 / 发送）的结果，不含密码。
// test_to 给了就只发给这一个地址（校验格式，单个）；不给则按渠道的固定收件人与 @ 规则解析（与其它渠道的测试发送一致）。
// 不论成败都返回 200 与 {ok, error, stages}，页面据此逐阶段展示；参数错误才返回 4xx。整次最长 30 秒。
func (s *Server) testEmail(c *gin.Context, req *channelReq, ch notify.Channel, ev notify.Event) {
	to := strings.TrimSpace(req.TestTo)
	if to != "" {
		a, err := notify.ParseAddr(to)
		if err != nil {
			writeErr(c, badReq(fmt.Sprintf("测试收件人：%s", err.Error())))
			return
		}
		to = a
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 35*time.Second)
	defer cancel()
	res := s.disp.SendEmailTest(ctx, ch, ev, to)
	target := "渠道固定收件人与 @ 规则" // i18n-ignore 审计文本（存库）
	if to != "" {
		target = to
	}
	s.auditNotify(c, "通知渠道-测试发送", fmt.Sprintf("渠道「%s」（邮件）；SMTP %s；测试收件人 %s；结果：%s", ch.Name, ch.Email.HostHint(), target, // i18n-ignore 审计文本（存库）
		map[bool]string{true: "成功", false: "失败"}[res.OK])) // i18n-ignore 审计文本（存库）
	c.JSON(http.StatusOK, gin.H{"data": res})
}
