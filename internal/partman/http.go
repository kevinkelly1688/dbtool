// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package partman

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/i18n"
	"dbtool/internal/query"
	"dbtool/internal/server"
)

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, mirage, ok := s.DB()
		if !ok {
			return
		}
		svc := NewService(newStore(db, mirage), s.Repo(), query.NewPrivAuthorizer(s.Repo(), nil), DefaultDialer)
		register(s, rt, svc)
		server.AddWorker(svc.Worker)
	})
	server.AddMenu(server.MenuItem{Group: "query", Key: "partman", Href: "/partman", Label: "分区维护", Order: 40,
		Show: func(s *server.Server, a audit.Actor) bool {
			if a.IsSuperuser {
				return true
			}
			return s.ActorHasButton(a, permMenu)
		}})
}

type handlers struct {
	s   *server.Server
	svc *Service
}

// register 登记路由（测试里直接传入假依赖的 Service）。
func register(s *server.Server, rt server.Routes, svc *Service) {
	h := &handlers{s: s, svc: svc}
	rt.Pages.GET("/partman", s.RequireButtonPage(permMenu), h.page)
	rt.Pages.GET("/partman/logs", s.RequireButtonPage(permMenu), h.logsPage)
	g := rt.API.Group("/partman", s.RequireButtonAPI(permMenu))
	g.GET("/overview", h.overview)
	g.GET("/summary", h.summary)
	g.GET("/logs", h.globalLogs)
	g.GET("/logs/:log_id", h.logDetail)
	g.GET("/logs/:log_id/output", h.logOutput)
	g.GET("/webhooks", s.RequireButtonAPI(permChange), h.webhooks)
	g.POST("/webhooks", s.RequireButtonAPI(permChange), h.webhookCreate)
	g.POST("/webhooks/test", s.RequireButtonAPI(permChange), h.webhookTest)
	g.PUT("/webhooks/:id", s.RequireButtonAPI(permChange), h.webhookUpdate)
	g.DELETE("/webhooks/:id", s.RequireButtonAPI(permChange), h.webhookDelete)
	g.POST("/webhooks/:id/enable", s.RequireButtonAPI(permChange), h.webhookEnable)
	g.GET("/instances/:id/databases", h.databases)
	g.GET("/instances/:id/tables", h.tables)
	g.GET("/instances/:id/columns", h.columns)
	g.GET("/configs", h.list)
	g.GET("/configs/:id/status", h.status)
	g.POST("/configs/:id/enable", s.RequireButtonAPI(permChange), h.setEnabled)
	g.PUT("/configs/:id/alert", s.RequireButtonAPI(permChange), h.setAlert)
	g.POST("/configs/:id/alert/silence", s.RequireButtonAPI(permChange), h.silenceAlert)
	g.GET("/alerts", h.alertLogs)
	g.POST("/configs", s.RequireButtonAPI(permAdd), h.create)
	g.PUT("/configs/:id", s.RequireButtonAPI(permChange), h.update)
	g.DELETE("/configs/:id", s.RequireButtonAPI(permDelete), h.remove)
	g.POST("/preview", h.preview)
	g.POST("/configs/:id/run", s.RequireButtonAPI(btnRun), h.run) // 只认 partman.run：与编辑（partman.edit）分开
	g.GET("/configs/:id/logs", h.logs)
}

// reqLang 返回本次请求的界面语言（中间件写进 request.Context；没有时是系统默认语言）。
func reqLang(c *gin.Context) string { return i18n.FromContext(c.Request.Context()) }

// apiOK 返回 {"data": v}；非中文请求时先把 v 里系统生成的文案翻译成请求语言（见 localize.go）。
func apiOK(c *gin.Context, v any) { server.APIOK(c, localize(reqLang(c), v)) }

func (h *handlers) caller(c *gin.Context) Caller {
	return Caller{Principal: server.PrincipalOf(c), Lang: reqLang(c), Can: func(code string) bool { return h.s.HasButton(c, code) }}
}

// fail 把业务错误映射成 HTTP 响应；未知错误只记日志，对外统一成 500。
func fail(c *gin.Context, err error) {
	var pe *Error
	if errors.As(err, &pe) {
		if pe.Data != nil {
			c.AbortWithStatusJSON(pe.Status, gin.H{"code": pe.Code, "message": pe.Message, "data": localize(reqLang(c), pe.Data)})
			return
		}
		server.APIFail(c, pe.Status, pe.Code, pe.Message)
		return
	}
	slog.Error("分区维护请求失败", "path", c.Request.URL.Path, "err", err)
	server.APIFail(c, http.StatusInternalServerError, "internal", "服务内部错误，请稍后重试或联系管理员")
}

func idParam(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "编号不合法")
		return 0, false
	}
	return uint(n), true
}

func (h *handlers) page(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "partman/index.html", server.PageData{Title: "分区维护", Nav: "partman"})
}

func (h *handlers) overview(c *gin.Context) {
	ov, err := h.svc.Overview(h.caller(c))
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, ov)
}

func (h *handlers) list(c *gin.Context) {
	q := ListQuery{ColType: c.Query("col_type"), Period: c.Query("period"), Search: c.Query("search")}
	if n, err := strconv.ParseUint(c.Query("instance_id"), 10, 32); err == nil {
		q.InstanceID = uint(n)
	}
	switch c.Query("isenable") {
	case "1":
		t := true
		q.IsEnable = &t
	case "0":
		f := false
		q.IsEnable = &f
	}
	q.Offset, _ = strconv.Atoi(c.Query("offset"))
	q.Limit, _ = strconv.Atoi(c.Query("limit"))
	rows, total, err := h.svc.List(h.caller(c), q)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, localize(reqLang(c), gin.H{"data": rows, "total": total}))
}

func (h *handlers) create(c *gin.Context) {
	var in ConfigInput
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	v, err := h.svc.Create(h.caller(c), in)
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

func (h *handlers) update(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in ConfigInput
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	v, err := h.svc.Update(h.caller(c), id, in)
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

func (h *handlers) remove(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.Delete(h.caller(c), id); err != nil {
		fail(c, err)
		return
	}
	apiOK(c, gin.H{"ok": true})
}

// preview 的请求体：id 非 0 预览已保存的配置，否则按 draft（编辑弹窗里未保存的内容）。
type previewReq struct {
	ID    uint         `json:"id"`
	Draft *ConfigInput `json:"draft"`
	Init  *InitRequest `json:"init"` // 未分区的 MySQL 表：初始化的起点选择
}

// setAlert 设置这张表自己的可用分区告警阈值：threshold 为 null = 跟随全局，0 = 不告警。
func (h *handlers) setAlert(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in struct {
		Threshold *int `json:"threshold"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	v, err := h.svc.SetAlertThreshold(h.caller(c), id, in.Threshold)
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

// silenceAlert 静默这张表的告警 hours 小时；0 = 取消静默。
func (h *handlers) silenceAlert(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in struct {
		Hours int `json:"hours"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	v, err := h.svc.SilenceAlert(h.caller(c), id, in.Hours)
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

// alertLogs 列出可用分区告警的推送记录（新的在前）。
func (h *handlers) alertLogs(c *gin.Context) {
	offset, _ := strconv.Atoi(c.Query("offset"))
	limit, _ := strconv.Atoi(c.Query("limit"))
	rows, total, err := h.svc.AlertLogs(h.caller(c), offset, limit)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, localize(reqLang(c), gin.H{"data": rows, "total": total}))
}

func (h *handlers) preview(c *gin.Context) {
	var req previewReq
	if err := c.ShouldBindJSON(&req); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	v, err := h.svc.Preview(c.Request.Context(), h.caller(c), req.ID, req.Draft, req.Init)
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

func (h *handlers) run(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var opt RunOptions
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&opt); err != nil {
			server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
			return
		}
	}
	res, err := h.svc.Run(c.Request.Context(), h.caller(c), id, opt)
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, res)
}

func (h *handlers) logs(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	rows, err := h.svc.Logs(h.caller(c), id, limit)
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, rows)
}

// ---- 状态、汇总、启停、全局日志、库表级联 ----

func (h *handlers) summary(c *gin.Context) {
	v, err := h.svc.Summary(h.caller(c))
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

// status 读目标表的真实分区推导状态。读不到目标表不是 HTTP 错误（state=unknown），所以前端可以放心逐行异步调用。
func (h *handlers) status(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	v, err := h.svc.Status(c.Request.Context(), h.caller(c), id, c.Query("refresh") == "1")
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

type enableReq struct {
	Enable *bool `json:"enable"`
}

func (h *handlers) setEnabled(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var req enableReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Enable == nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误：需要 enable 字段")
		return
	}
	v, err := h.svc.SetEnabled(h.caller(c), id, *req.Enable)
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

func (h *handlers) globalLogs(c *gin.Context) {
	q := GlobalLogQuery{Source: c.Query("source"), System: c.Query("scope") == "system"}
	if n, err := strconv.ParseUint(c.Query("config_id"), 10, 32); err == nil {
		q.ConfigID = uint(n)
	}
	q.Offset, _ = strconv.Atoi(c.Query("offset"))
	q.Limit, _ = strconv.Atoi(c.Query("limit"))
	rows, total, opts, err := h.svc.GlobalLogs(h.caller(c), q)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, localize(reqLang(c), gin.H{"data": rows, "total": total, "configs": opts}))
}

func (h *handlers) logsPage(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "partman/logs.html", server.PageData{Title: "分区维护日志", Nav: "partman"})
}

func (h *handlers) databases(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	v, err := h.svc.Databases(c.Request.Context(), h.caller(c), id)
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

func (h *handlers) tables(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	v, err := h.svc.Tables(c.Request.Context(), h.caller(c), id, c.Query("db"))
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

func (h *handlers) columns(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	v, err := h.svc.Columns(c.Request.Context(), h.caller(c), id, c.Query("db"), c.Query("table"))
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

// ---- 日志明细与运行输出 ----

func logIDParam(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("log_id"), 10, 32)
	if err != nil || n == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "日志编号不合法")
		return 0, false
	}
	return uint(n), true
}

func (h *handlers) logDetail(c *gin.Context) {
	id, ok := logIDParam(c)
	if !ok {
		return
	}
	v, err := h.svc.LogDetail(h.caller(c), id)
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

// logOutput 以 text/plain 附件导出一次运行的输出（同查看日志的权限）。
func (h *handlers) logOutput(c *gin.Context) {
	id, ok := logIDParam(c)
	if !ok {
		return
	}
	name, text, err := h.svc.LogOutput(h.caller(c), id)
	if err != nil {
		fail(c, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(text))
}

// ---- 推送通道 ----

func (h *handlers) webhooks(c *gin.Context) {
	v, err := h.svc.Webhooks(h.caller(c))
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

func (h *handlers) webhookCreate(c *gin.Context) {
	var in WebhookInput
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	v, err := h.svc.CreateWebhook(h.caller(c), in)
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

func (h *handlers) webhookUpdate(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in WebhookInput
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	v, err := h.svc.UpdateWebhook(h.caller(c), id, in)
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

func (h *handlers) webhookDelete(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteWebhook(h.caller(c), id); err != nil {
		fail(c, err)
		return
	}
	apiOK(c, gin.H{"ok": true})
}

func (h *handlers) webhookEnable(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var req enableReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Enable == nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误：需要 enable 字段")
		return
	}
	v, err := h.svc.SetWebhookEnabled(h.caller(c), id, *req.Enable)
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}

type webhookTestReq struct {
	ID uint `json:"id"`
}

func (h *handlers) webhookTest(c *gin.Context) {
	var req webhookTestReq
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
			return
		}
	}
	v, err := h.svc.TestWebhooks(c.Request.Context(), h.caller(c), req.ID)
	if err != nil {
		fail(c, err)
		return
	}
	apiOK(c, v)
}
