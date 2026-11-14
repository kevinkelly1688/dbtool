// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package firewalladmin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/server"
)

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, _, ok := s.DB() // 仓库不是 store.Store（测试 / 演示）时没有元数据库，路由即 404
		if !ok {
			return
		}
		var inv Invalidator
		if e := s.Firewall(); e != nil {
			inv = e
		}
		register(s, rt, NewService(s.Repo(), newStore(db), inv))
	})
	server.AddMenu(server.MenuItem{Group: "secure", Key: "sys-firewall", Href: "/settings/firewall", Label: "SQL 防火墙", Order: 30,
		Show: func(s *server.Server, a audit.Actor) bool {
			_, _, ok := s.DB()
			return ok && server.SuperOnly(s, a)
		}})
}

type handlers struct {
	s   *server.Server
	svc *Service
}

// register 登记路由（测试里直接传入假依赖的 Service）。
func register(s *server.Server, rt server.Routes, svc *Service) {
	h := &handlers{s: s, svc: svc}
	rt.Pages.GET("/settings/firewall", s.SuperuserPage(), h.page)
	g := rt.API.Group("/settings/firewall", s.SuperuserAPI())
	g.GET("/overview", h.overview)
	g.GET("/meta", h.meta)
	g.GET("/rules", h.rules)
	g.POST("/rules", h.createRule)
	g.GET("/rules/:id", h.rule)
	g.PUT("/rules/:id", h.updateRule)
	g.DELETE("/rules/:id", h.deleteRule)
	g.POST("/rules/:id/toggle", h.toggleRule)
	g.POST("/rules/:id/mode", h.ruleMode)
	g.POST("/rules/:id/replay", h.replayRule)
	g.POST("/replay", h.replay)
	g.POST("/evaluate", h.evaluate)
	g.GET("/hits", h.hits)
	g.GET("/hits/:id", h.hit)
	g.GET("/exceptions", h.exceptions)
	g.POST("/exceptions", h.createException)
	g.DELETE("/exceptions/:id", h.deleteException)
	g.GET("/templates", h.templates)
	g.POST("/templates/:key/apply", h.applyTemplate)
	g.PUT("/mode", h.setMode)
	g.PUT("/switch", h.setSwitch)
}

func fail(c *gin.Context, err error) {
	var e *Error
	if errors.As(err, &e) {
		server.APIFail(c, e.Status, e.Code, e.Message)
		return
	}
	slog.Error("SQL 防火墙管理接口出错", "path", c.Request.URL.Path, "err", err)
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

// bind 解析 JSON 请求体（上限 1MB）。
func bind(c *gin.Context, v any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	if err := c.ShouldBindJSON(v); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误或内容过大")
		return false
	}
	return true
}

func intQuery(c *gin.Context, key string) int {
	n, _ := strconv.Atoi(c.Query(key))
	return n
}

func (h *handlers) page(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "firewall/index.html", server.PageData{Title: "SQL 防火墙", Nav: "sys-firewall"})
}

func (h *handlers) overview(c *gin.Context) {
	v, err := h.svc.Overview()
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, v)
}

func (h *handlers) meta(c *gin.Context) {
	v, err := h.svc.Meta()
	if err != nil {
		fail(c, err)
		return
	}
	localizeMeta(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) rules(c *gin.Context) {
	v, err := h.svc.Rules()
	if err != nil {
		fail(c, err)
		return
	}
	localizeRuleList(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) rule(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	v, err := h.svc.Rule(id)
	if err != nil {
		fail(c, err)
		return
	}
	localizeRule(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) createRule(c *gin.Context) {
	var in RuleInput
	if !bind(c, &in) {
		return
	}
	v, err := h.svc.CreateRule(server.ActorOf(c), in)
	if err != nil {
		fail(c, err)
		return
	}
	localizeRule(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) updateRule(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in RuleInput
	if !bind(c, &in) {
		return
	}
	v, err := h.svc.UpdateRule(server.ActorOf(c), id, in)
	if err != nil {
		fail(c, err)
		return
	}
	localizeRule(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) deleteRule(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	n, err := h.svc.DeleteRule(server.ActorOf(c), id)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"removed_exceptions": n})
}

func (h *handlers) toggleRule(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b toggleBody
	if !bind(c, &b) {
		return
	}
	v, err := h.svc.ToggleRule(server.ActorOf(c), id, b.Enabled)
	if err != nil {
		fail(c, err)
		return
	}
	localizeRule(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) ruleMode(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b modeBody
	if !bind(c, &b) {
		return
	}
	v, err := h.svc.SetRuleMode(server.ActorOf(c), id, b.Mode)
	if err != nil {
		fail(c, err)
		return
	}
	localizeMode(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) replay(c *gin.Context) {
	var rq ReplayRequest
	if !bind(c, &rq) {
		return
	}
	h.doReplay(c, rq)
}

func (h *handlers) replayRule(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var body struct {
		Days int `json:"days"`
	}
	if c.Request.ContentLength != 0 && !bind(c, &body) {
		return
	}
	h.doReplay(c, ReplayRequest{RuleID: id, Days: body.Days})
}

func (h *handlers) doReplay(c *gin.Context, rq ReplayRequest) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), replayBudget+10*time.Second)
	defer cancel()
	v, err := h.svc.Replay(ctx, rq)
	if err != nil {
		fail(c, err)
		return
	}
	localizeReplay(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) evaluate(c *gin.Context) {
	var in EvaluateRequest
	if !bind(c, &in) {
		return
	}
	v, err := h.svc.Evaluate(in)
	if err != nil {
		fail(c, err)
		return
	}
	localizeEval(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) hits(c *gin.Context) {
	rid, _ := strconv.ParseUint(c.Query("rule_id"), 10, 32)
	v, err := h.svc.Hits(HitQuery{
		RuleID: uint(rid), Q: c.Query("q"), User: c.Query("user"), Result: c.Query("result"), Scene: c.Query("scene"),
		Days: intQuery(c, "days"), Page: intQuery(c, "page"), Size: intQuery(c, "size"),
	})
	if err != nil {
		fail(c, err)
		return
	}
	localizeHits(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) hit(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	v, err := h.svc.Hit(id)
	if err != nil {
		fail(c, err)
		return
	}
	localizeHit(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) exceptions(c *gin.Context) {
	v, err := h.svc.Exceptions()
	if err != nil {
		fail(c, err)
		return
	}
	localizeExceptions(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) createException(c *gin.Context) {
	var in ExceptionInput
	if !bind(c, &in) {
		return
	}
	v, err := h.svc.CreateException(server.ActorOf(c), in)
	if err != nil {
		fail(c, err)
		return
	}
	localizeException(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) deleteException(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteException(server.ActorOf(c), id); err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{})
}

func (h *handlers) templates(c *gin.Context) {
	v, err := h.svc.Templates()
	if err != nil {
		fail(c, err)
		return
	}
	localizeTemplates(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) applyTemplate(c *gin.Context) {
	v, err := h.svc.ApplyTemplate(server.ActorOf(c), c.Param("key"))
	if err != nil {
		fail(c, err)
		return
	}
	localizeRule(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) setMode(c *gin.Context) {
	var b modeBody
	if !bind(c, &b) {
		return
	}
	v, err := h.svc.SetMode(server.ActorOf(c), b.Mode)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, v)
}

func (h *handlers) setSwitch(c *gin.Context) {
	var b switchBody
	if !bind(c, &b) {
		return
	}
	v, err := h.svc.SetSwitch(server.ActorOf(c), b.Enabled, b.Reason)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, v)
}
