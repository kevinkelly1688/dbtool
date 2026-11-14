// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package maskadmin

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
		register(s, rt, NewService(s.Repo(), newStore(db), nil))
	})
	server.AddMenu(server.MenuItem{Group: "secure", Key: "sys-masking", Href: "/settings/masking", Label: "数据脱敏", Order: 20,
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
	rt.Pages.GET("/settings/masking", s.SuperuserPage(), h.page)
	g := rt.API.Group("/settings/masking", s.SuperuserAPI())
	g.GET("/instances", h.instances)
	g.GET("/databases", h.databases)
	g.GET("/catalog", h.catalog)
	g.GET("/config", h.config)
	g.POST("/save", h.save)
	g.POST("/enable", h.enable)
}

func fail(c *gin.Context, err error) {
	var e *Error
	if errors.As(err, &e) {
		server.APIFail(c, e.Status, e.Code, e.Message)
		return
	}
	slog.Error("数据脱敏配置接口出错", "path", c.Request.URL.Path, "err", err)
	server.APIFail(c, http.StatusInternalServerError, "internal", "服务内部错误，请稍后重试或联系管理员")
}

func idQuery(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Query("instance_id"), 10, 32)
	if err != nil || n == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请选择实例")
		return 0, false
	}
	return uint(n), true
}

func (h *handlers) page(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "maskadmin/index.html", server.PageData{Title: "数据脱敏", Nav: "sys-masking"})
}

func (h *handlers) instances(c *gin.Context) {
	list, err := h.svc.Instances()
	if err != nil {
		fail(c, err)
		return
	}
	localizeInstances(langOf(c), list)
	server.APIOK(c, list)
}

func (h *handlers) databases(c *gin.Context) {
	id, ok := idQuery(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	dbs, err := h.svc.Databases(ctx, id)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, dbs)
}

func (h *handlers) catalog(c *gin.Context) {
	id, ok := idQuery(c)
	if !ok {
		return
	}
	cat, err := h.svc.Catalog(c.Request.Context(), id, c.Query("db"))
	if err != nil {
		fail(c, err)
		return
	}
	localizeCatalog(langOf(c), cat)
	server.APIOK(c, cat)
}

func (h *handlers) config(c *gin.Context) {
	id, ok := idQuery(c)
	if !ok {
		return
	}
	v, err := h.svc.Config(id)
	if err != nil {
		fail(c, err)
		return
	}
	localizeConfig(langOf(c), v)
	server.APIOK(c, v)
}

// enable 打开系统脱敏总开关（只开不关，见 Service.EnableMasking）。
func (h *handlers) enable(c *gin.Context) {
	res, err := h.svc.EnableMasking(server.ActorOf(c))
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) save(c *gin.Context) {
	var req SaveRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<20)
	if err := c.ShouldBindJSON(&req); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误或内容过大")
		return
	}
	res, err := h.svc.Save(server.ActorOf(c), req)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, res)
}
