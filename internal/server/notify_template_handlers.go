// @author: kevinkelly
// @license: Apache Licence
// @file: notify_template_handlers.go
// @time: 2026/10/28

package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/notify"
)

// 通知消息模板（仅超级用户）：按“消息 × 范围（全局 / 某个渠道）”定制推送的标题、状态色、字段、SQL 显示方式与按钮。
// 原型 prototype/notify-templates.html。
func init() {
	addRoutes(func(s *Server, rt Routes) {
		if s.admin == nil {
			return
		}
		if _, ok := s.admin.(notify.TemplateAdmin); !ok {
			return
		}
		rt.Pages.GET("/settings/notify/templates", s.notifyAdminEnabled(), s.requireSuperuserPage(), s.notifyTemplatesPage)
		g := rt.API.Group("/settings/notify/templates", s.notifyAdminEnabled(), s.requireSuperuserAPI())
		g.GET("/catalog", s.apiTplCatalog)
		g.GET("/item", s.apiTplGet)
		g.PUT("/item", s.apiTplSave)
		g.DELETE("/item", s.apiTplDelete)
		g.GET("/history", s.apiTplHistory)
		g.POST("/preview", s.apiTplPreview)
		g.POST("/test", s.apiTplTest)
	})
}

// invalidateTemplates 让发送器下一条消息重新读取模板。
func (s *Server) invalidateTemplates() {
	if s.disp != nil {
		s.disp.InvalidateTemplates()
	}
}

func (s *Server) tplAdmin() notify.TemplateAdmin {
	a, _ := s.admin.(notify.TemplateAdmin)
	return a
}

func (s *Server) notifyTemplatesPage(c *gin.Context) {
	p := principalOf(c)
	s.render(c, http.StatusOK, "notify_templates.html", pageData{Title: "消息模板", Nav: "notify", Username: p.Username, Display: p.Display,
		CSRF: sessionOf(c).CSRF, Nav2: s.nav(actorOf(c))})
}

// tplReq 是保存 / 预览 / 测试发送的请求体。
type tplReq struct {
	Event   string         `json:"event"`
	Scope   string         `json:"scope"`
	Name    string         `json:"name"`
	Enabled bool           `json:"enabled"`
	Title   string         `json:"title"`
	Tone    string         `json:"tone"`
	Icon    string         `json:"icon"`
	Blocks  []notify.Block `json:"blocks"`
	Note    string         `json:"note"`
	// 预览 / 测试发送：按哪个渠道渲染。channel_id 是已保存的渠道；为 0 时按 channel_type 模拟（没有 @ 规则）。
	ChannelID   uint   `json:"channel_id"`
	ChannelType string `json:"channel_type"`
	TestTo      string `json:"test_to"`
}

func (r tplReq) template() notify.Template {
	t := notify.Template{EventKey: r.Event, Scope: r.Scope, Name: r.Name, Enabled: r.Enabled, Title: r.Title, Tone: r.Tone, Icon: r.Icon, Blocks: r.Blocks}
	if t.Blocks == nil {
		t.Blocks = []notify.Block{}
	}
	t.Normalize()
	return t
}

// checkScope 校验范围：global 或已存在的渠道（c:<id>）。
func (s *Server) checkScope(scope string) error {
	if scope == notify.ScopeGlobal {
		return nil
	}
	id := notify.ChannelOfScope(scope)
	if id == 0 {
		return badReq("范围不合法：只能是 global 或 c:<渠道 ID>")
	}
	ch, err := s.admin.GetChannel(id)
	if err != nil {
		return err
	}
	if ch == nil {
		return badReq("所选渠道不存在")
	}
	return nil
}

func (s *Server) tplDef(key string) (*notify.MessageDef, error) {
	def := notify.MessageByKey(key)
	if def == nil {
		return nil, badReq(fmt.Sprintf("未知的消息：%s", key))
	}
	return def, nil
}

type tplSummary struct {
	Event     string `json:"event"`
	Scope     string `json:"scope"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Title     string `json:"title"`
	UpdatedBy string `json:"updated_by"`
	UpdatedAt string `json:"updated_at"`
}

// apiTplCatalog 返回页面需要的全部元数据：消息目录、变量、渠道、已保存模板的摘要。
func (s *Server) apiTplCatalog(c *gin.Context) {
	adm := s.tplAdmin()
	out := gin.H{"ready": adm.TemplateTableReady(), "messages": notify.Messages, "vars": notify.Vars, "groups": notify.GroupsOf()}
	chans := []gin.H{}
	if s.admin.NotifyTableReady() {
		views, err := s.admin.ListChannelViews()
		if err != nil {
			writeErr(c, err)
			return
		}
		for _, v := range views {
			chans = append(chans, gin.H{"id": v.ID, "name": v.Name, "type": v.Type, "label": notify.TypeLabel(v.Type), "enabled": v.Enabled, "group_ids": v.GroupIDs})
		}
	}
	out["channels"] = chans
	sums := []tplSummary{}
	if adm.TemplateTableReady() {
		list, err := adm.ListTemplates()
		if err != nil {
			writeErr(c, err)
			return
		}
		for _, t := range list {
			sums = append(sums, tplSummary{t.EventKey, t.Scope, t.Name, t.Enabled, t.Title, t.UpdatedBy, t.UpdatedAt})
		}
	}
	out["templates"] = sums
	out["limits"] = gin.H{"max_blocks": notify.MaxBlocks, "max_rows": notify.MaxRows, "code_min": notify.MinCodeN, "code_max": notify.MaxCodeN, "code_default": notify.DefaultCodeN}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// apiTplGet 返回某个范围下的模板：已保存的（没有为 null）、它的“上一层”（渠道专属 → 全局或系统默认；全局 → 系统默认）、系统默认。
func (s *Server) apiTplGet(c *gin.Context) {
	def, err := s.tplDef(c.Query("event"))
	if err != nil {
		writeErr(c, err)
		return
	}
	scope := c.DefaultQuery("scope", notify.ScopeGlobal)
	if err := s.checkScope(scope); err != nil {
		writeErr(c, err)
		return
	}
	adm := s.tplAdmin()
	def0 := notify.DefaultTemplate(def)
	out := gin.H{"default": def0, "template": nil, "parent": def0, "parent_scope": "default"}
	if !adm.TemplateTableReady() {
		c.JSON(http.StatusOK, gin.H{"data": out})
		return
	}
	cur, err := adm.GetTemplate(def.Key, scope)
	if err != nil {
		writeErr(c, err)
		return
	}
	if cur != nil {
		out["template"] = cur
	}
	if scope != notify.ScopeGlobal {
		g, err := adm.GetTemplate(def.Key, notify.ScopeGlobal)
		if err != nil {
			writeErr(c, err)
			return
		}
		if g != nil && g.Enabled {
			out["parent"], out["parent_scope"] = g, notify.ScopeGlobal
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func issuesOrEmpty(is []notify.Issue) []notify.Issue {
	if is == nil {
		return []notify.Issue{}
	}
	return is
}

// apiTplSave 校验并保存一条模板。有“不能保存”的问题时返回 400 并带上全部问题。
func (s *Server) apiTplSave(c *gin.Context) {
	var req tplReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	def, err := s.tplDef(req.Event)
	if err != nil {
		writeErr(c, err)
		return
	}
	if err := s.checkScope(req.Scope); err != nil {
		writeErr(c, err)
		return
	}
	adm := s.tplAdmin()
	if !adm.TemplateTableReady() {
		apiFail(c, http.StatusConflict, "not_ready", "还没有创建消息模板表：请先执行 deploy/sql/260_notify_template.sql（升级时 install.sh 会自动执行）")
		return
	}
	t := req.template()
	issues := t.Validate(def)
	if notify.HasCrit(issues) {
		c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_template", "message": "模板有问题，不能保存", "data": gin.H{"issues": issues}})
		return
	}
	op := principalOf(c).Username
	if err := adm.SaveTemplate(t, op, req.Note); err != nil {
		writeErr(c, err)
		return
	}
	s.invalidateTemplates()
	s.auditNotify(c, "通知消息模板-保存", fmt.Sprintf("消息「%s」（%s）范围：%s；名称：%s；%s；说明：%s", def.Name, def.Key, s.scopeText(t.Scope), t.Name, // i18n-ignore 审计文本（存库）
		map[bool]string{true: "启用", false: "停用"}[t.Enabled], strings.TrimSpace(req.Note))) // i18n-ignore 审计文本（存库）
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "issues": issuesOrEmpty(issues)}})
}

// apiTplDelete 删除一条模板：该范围恢复到上一层（渠道专属 → 全局 → 系统默认）。
func (s *Server) apiTplDelete(c *gin.Context) {
	def, err := s.tplDef(c.Query("event"))
	if err != nil {
		writeErr(c, err)
		return
	}
	scope := c.DefaultQuery("scope", notify.ScopeGlobal)
	if scope != notify.ScopeGlobal && notify.ChannelOfScope(scope) == 0 {
		writeErr(c, badReq("范围不合法"))
		return
	}
	adm := s.tplAdmin()
	if !adm.TemplateTableReady() {
		apiFail(c, http.StatusConflict, "not_ready", "还没有创建消息模板表：请先执行 deploy/sql/260_notify_template.sql")
		return
	}
	if err := adm.DeleteTemplate(def.Key, scope, principalOf(c).Username, c.Query("note")); err != nil {
		writeErr(c, err)
		return
	}
	s.invalidateTemplates()
	s.auditNotify(c, "通知消息模板-删除", fmt.Sprintf("消息「%s」（%s）范围：%s；已恢复到上一层 / 系统默认", def.Name, def.Key, s.scopeText(scope))) // i18n-ignore 审计文本（存库）
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

func (s *Server) apiTplHistory(c *gin.Context) {
	def, err := s.tplDef(c.Query("event"))
	if err != nil {
		writeErr(c, err)
		return
	}
	scope := c.DefaultQuery("scope", notify.ScopeGlobal)
	list, err := s.tplAdmin().TemplateHistory(def.Key, scope, 30)
	if err != nil {
		writeErr(c, err)
		return
	}
	if list == nil {
		list = []notify.TemplateHist{}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"history": list}})
}

// scopeText 是范围的可读名称（审计用）。
func (s *Server) scopeText(scope string) string {
	if scope == notify.ScopeGlobal {
		return "全局" // i18n-ignore 审计文本（存库）
	}
	if id := notify.ChannelOfScope(scope); id != 0 {
		if ch, err := s.admin.GetChannel(id); err == nil && ch != nil {
			return "渠道「" + ch.Name + "」" // i18n-ignore 审计文本（存库）
		}
		return fmt.Sprintf("渠道 #%d", id) // i18n-ignore 审计文本（存库）
	}
	return scope
}

// tplChannel 取预览 / 测试发送用的渠道：已保存的渠道（解密后；预览不会用到地址与密钥），或按类型模拟的渠道。
func (s *Server) tplChannel(req *tplReq) (notify.Channel, error) {
	if req.ChannelID != 0 {
		ch, err := s.admin.GetChannel(req.ChannelID)
		if err != nil {
			return notify.Channel{}, err
		}
		if ch == nil {
			return notify.Channel{}, badReq("所选渠道不存在")
		}
		return *ch, nil
	}
	typ := req.ChannelType
	ok := false
	for _, t := range notify.Types {
		ok = ok || t == typ
	}
	if !ok {
		return notify.Channel{}, badReq(fmt.Sprintf("渠道类型不合法：%s", typ))
	}
	ch := notify.Channel{Name: "（示例渠道）", Type: typ, Enabled: true, WebhookURL: "https://example.invalid/robot/send"} // i18n-ignore 示例渠道的样例数据（用于渲染预览），不翻译
	if typ == notify.TypeEmail {
		ch.Email = &notify.EmailConfig{Host: "smtp.example.com", Port: 587, Encryption: notify.EncStartTLS, FromAddr: "dbtool@example.com", FromName: "DB 工具", // i18n-ignore 示例渠道的样例数据（用于渲染预览），不翻译
			To: []string{"ops@example.com"}, Cc: []string{}}
	}
	return ch, nil
}

// tplSample 生成模拟事件：样例变量 + 渠道所属的资源组（让 @ 规则有对象可解析）。
func (s *Server) tplSample(key string, ch notify.Channel, username string) (notify.Event, error) {
	g := ch.GroupID
	if g == 0 && len(ch.GroupIDs) > 0 {
		g = ch.GroupIDs[0]
	}
	if g == 0 {
		g = s.sampleGroup(&channelReq{})
	}
	return notify.SampleMessageEvent(key, username, g, 0)
}

// apiTplPreview 用草稿模板渲染一条模拟事件，返回各渠道的原生载荷与渲染结果；不发送任何请求。
func (s *Server) apiTplPreview(c *gin.Context) {
	if s.disp == nil {
		apiFail(c, http.StatusServiceUnavailable, "unavailable", "通知发送器未启用")
		return
	}
	var req tplReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	def, err := s.tplDef(req.Event)
	if err != nil {
		writeErr(c, err)
		return
	}
	t := req.template()
	issues := t.Validate(def)
	if notify.HasCrit(issues) { // 草稿还有硬伤：只返回问题，不渲染
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"issues": issues, "preview": nil}})
		return
	}
	ch, err := s.tplChannel(&req)
	if err != nil {
		writeErr(c, err)
		return
	}
	ev, err := s.tplSample(def.Key, ch, principalOf(c).Username)
	if err != nil {
		writeErr(c, err)
		return
	}
	pv, err := s.disp.PreviewTemplate(ch, ev, t)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"issues": issuesOrEmpty(issues), "preview": pv}})
}

// apiTplTest 用草稿模板向一个已保存的渠道真发一条模拟消息（不看渠道是否启用 / 订阅）。
func (s *Server) apiTplTest(c *gin.Context) {
	if s.disp == nil {
		apiFail(c, http.StatusServiceUnavailable, "unavailable", "通知发送器未启用")
		return
	}
	var req tplReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	if req.ChannelID == 0 {
		writeErr(c, badReq("请选择要发送到的渠道"))
		return
	}
	def, err := s.tplDef(req.Event)
	if err != nil {
		writeErr(c, err)
		return
	}
	t := req.template()
	issues := t.Validate(def)
	if notify.HasCrit(issues) {
		c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_template", "message": "模板有问题，不能发送", "data": gin.H{"issues": issues}})
		return
	}
	ch, err := s.tplChannel(&req)
	if err != nil {
		writeErr(c, err)
		return
	}
	ev, err := s.tplSample(def.Key, ch, principalOf(c).Username)
	if err != nil {
		writeErr(c, err)
		return
	}
	ev.Title = "[dbtool 测试] " + ev.Title // i18n-ignore 测试消息正文
	m, err := notify.RenderWith(t, ev)
	if err != nil {
		writeErr(c, badReq(fmt.Sprintf("模板渲染失败：%s", err.Error())))
		return
	}
	m.Title = "[测试] " + m.Title // i18n-ignore 测试消息正文
	ev = ev.WithRich(m)

	audit := func(result string) {
		s.auditNotify(c, "通知消息模板-测试发送", fmt.Sprintf("消息「%s」（%s）；渠道「%s」（%s）；结果：%s", def.Name, def.Key, ch.Name, notify.TypeLabel(ch.Type), result)) // i18n-ignore 审计文本（存库）
	}
	if ch.Type == notify.TypeEmail {
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
		audit(map[bool]string{true: "成功", false: "失败"}[res.OK]) // i18n-ignore 审计文本（存库）
		c.JSON(http.StatusOK, gin.H{"data": res})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := s.disp.SendTest(ctx, ch, ev); err != nil {
		audit("失败") // i18n-ignore 审计文本（存库）
		apiFail(c, http.StatusBadGateway, "send_failed", fmt.Sprintf("发送失败：%s", err.Error()))
		return
	}
	audit("成功") // i18n-ignore 审计文本（存库）
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}
