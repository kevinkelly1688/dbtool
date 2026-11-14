// @author: kevinkelly
// @license: Apache Licence
// @file: notify_ext.go
// @time: 2026/10/28

package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/i18n"
	"dbtool/internal/notify"
)

// 通知渠道管理的扩展接口：消息预览（不真发）、发送记录、行内启停。与 notify_handlers.go 同属“仅超级用户”。
func init() {
	addRoutes(func(s *Server, rt Routes) {
		g := rt.API.Group("/settings/notify", s.notifyAdminEnabled(), s.requireSuperuserAPI())
		g.POST("/preview", s.apiPreviewChannel)
		g.GET("/logs", s.apiNotifyLogs)
		g.POST("/channels/:id/toggle", s.apiToggleChannel)
	})
}

// apiPreviewChannel 按表单里的配置与模拟事件，返回将要发送的消息体与 @ 解析结果；不发送任何请求。
// 规则里的事件过滤、“仅工作时间 @”按真实逻辑生效，没有 @ 的规则会附上原因。
func (s *Server) apiPreviewChannel(c *gin.Context) {
	if s.disp == nil {
		apiFail(c, http.StatusServiceUnavailable, "unavailable", "通知发送器未启用")
		return
	}
	var req channelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	if req.Event == "" {
		req.Event = notify.EventPending
	}
	ch, err := s.buildChannel(&req, false)
	if err != nil {
		writeErr(c, err)
		return
	}
	ch.EventsV2 = true
	ev, err := notify.SampleEvent(req.Event, principalOf(c).Username, s.sampleGroup(&req), req.ReviewerGroupID)
	if err != nil {
		writeErr(c, badReq(fmt.Sprintf("未知的事件：%s", req.Event)))
		return
	}
	ev.WorkTime = req.WorkTime
	pv, err := s.disp.Preview(ch, ev)
	if err != nil {
		writeErr(c, err)
		return
	}
	localizePreviewSkips(c, pv)
	c.JSON(http.StatusOK, gin.H{"data": pv})
}

// skipOnlyHead / skipOnlyTail 是“该规则只在「A、B」时 @”这类原因的头尾（见 notify.resolveExplain）。
const (
	skipOnlyHead = "该规则只在「"       // i18n-ignore 与下面的整句配套，清单逐项翻译
	skipOnlyTail = "」时 @，当前事件不 @" // i18n-ignore 同上
)

// localizePreviewSkips 把预览里“没生效的规则”的原因按请求语言翻译：整句与 %s 格式串由前端 / 边缘按目录匹配，
// 这里补的是带“事件名清单”的那一类（清单整串匹配不到，先逐项翻译再整句翻译）。中文界面原样不动。
func localizePreviewSkips(c *gin.Context, pv *notify.Preview) {
	if pv == nil || langOf(c) == i18n.ZH {
		return
	}
	for i, s := range pv.Skipped {
		inner, ok := strings.CutPrefix(s.Reason, skipOnlyHead)
		if !ok {
			continue
		}
		if inner, ok = strings.CutSuffix(inner, skipOnlyTail); !ok {
			continue
		}
		names := strings.Split(inner, "、")
		for j := range names {
			names[j] = trMsg(c, names[j])
		}
		pv.Skipped[i].Reason = tr(c, "该规则只在「{names}」时 @，当前事件不 @", "names", strings.Join(names, ", "))
	}
}

// apiNotifyLogs 分页返回发送记录（含测试发送），可按渠道、仅失败、事件过滤；默认只看最近 30 天（表里本来就只留 30 天）。
func (s *Server) apiNotifyLogs(c *gin.Context) {
	logs, ok := s.notifyLogs()
	if !ok {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ready": false, "rows": []notify.SendView{}, "total": 0, "page": 1, "pages": 1}})
		return
	}
	qint := func(name string, def int) int {
		if n, err := strconv.Atoi(c.Query(name)); err == nil {
			return n
		}
		return def
	}
	page, size := qint("page", 1), qint("page_size", 20)
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}
	f := notify.LogFilter{OnlyFailed: c.Query("failed") == "1", Event: c.Query("event"), Offset: (page - 1) * size, Limit: size}
	if id := qint("channel_id", 0); id > 0 {
		f.ChannelID = uint(id)
	}
	if f.Event != "" && !notify.ValidEvent(f.Event) {
		writeErr(c, badReq(fmt.Sprintf("未知的事件：%s", f.Event)))
		return
	}
	if d := qint("days", 0); d > 0 && d <= 30 {
		since := time.Now().AddDate(0, 0, -d)
		f.Since = &since
	}
	rows, total, err := logs.ListSendLogs(f)
	if err != nil {
		writeErr(c, err)
		return
	}
	views := make([]notify.SendView, len(rows))
	for i, r := range rows {
		views[i] = r.View()
	}
	pages := int((total + int64(size) - 1) / int64(size))
	if pages < 1 {
		pages = 1
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ready": true, "rows": views, "total": total, "page": page, "page_size": size, "pages": pages}})
}

// apiToggleChannel 启用 / 停用一个渠道（只改启用状态，不动其它配置）。
func (s *Server) apiToggleChannel(c *gin.Context) {
	if !s.notifyReady(c) {
		return
	}
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	cur, err := s.admin.GetChannel(id)
	if err != nil {
		writeErr(c, err)
		return
	}
	if cur == nil {
		apiFail(c, http.StatusNotFound, "not_found", "渠道不存在")
		return
	}
	if set, ok := s.admin.(notify.EnabledSetter); ok {
		err = set.SetChannelEnabled(id, req.Enabled)
	} else {
		// 仓库没有单独改启用状态的能力：用原配置整体保存一次（地址、密钥留空表示不修改）
		_, err = s.admin.SaveChannel(id, notify.ChannelInput{GroupID: cur.GroupID, GroupIDs: cur.GroupIDs, DutyUsers: cur.DutyUsers, Name: cur.Name, Type: cur.Type,
			Events: cur.Events, Rules: cur.Rules, Enabled: req.Enabled, Operator: principalOf(c).Username, Email: cur.Email})
	}
	if err != nil {
		writeErr(c, err)
		return
	}
	s.auditNotify(c, "通知渠道-修改", "渠道「"+cur.Name+"」#"+strconv.Itoa(int(id))+"："+map[bool]string{true: "启用", false: "停用"}[req.Enabled]) // i18n-ignore 审计文本（存库）
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "enabled": req.Enabled}})
}
