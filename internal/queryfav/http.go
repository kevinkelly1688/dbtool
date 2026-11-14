// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package queryfav

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"dbtool/internal/server"
)

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, _, ok := s.DB() // 仓库不是 store.Store（测试 / 演示）时没有元数据库，路由即 404
		if !ok {
			return
		}
		register(s, rt, NewService(newStore(db), nil))
	})
}

type handlers struct {
	s   *server.Server
	svc *Service
}

// register 登记路由（测试里直接传入内存仓库的 Service）。
// 只要能用在线查询（query.view，超级用户恒真）就能收藏；每个人只能操作自己的记录——owner 一律取登录用户，请求里没有“用户”参数。
func register(s *server.Server, rt server.Routes, svc *Service) {
	h := &handlers{s: s, svc: svc}
	g := rt.API.Group("/query/favorites", s.RequireButtonAPI("query.view"))
	g.GET("", h.list)
	g.POST("", h.add)
	g.PUT("/:id", h.update)
	g.POST("/:id/use", h.use)
	g.DELETE("/:id", h.remove)
}

func fail(c *gin.Context, err error) {
	var e *Error
	if errors.As(err, &e) {
		server.APIFail(c, e.Status, e.Code, e.Message)
		return
	}
	slog.Error("收藏接口出错", "path", c.Request.URL.Path, "err", err)
	server.APIFail(c, http.StatusInternalServerError, "internal", "服务异常，请稍后再试")
}

func owner(c *gin.Context) string { return server.PrincipalOf(c).Username }

func idParam(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数非法")
		return 0, false
	}
	return uint(n), true
}

func (h *handlers) list(c *gin.Context) {
	off, _ := strconv.Atoi(c.Query("offset"))
	lim, _ := strconv.Atoi(c.Query("limit"))
	pg, err := h.svc.List(owner(c), ListQuery{Q: c.Query("q"), Sort: c.Query("sort"), Asc: c.Query("order") == "asc", Offset: off, Limit: lim})
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, pg)
}

func (h *handlers) add(c *gin.Context) {
	var in SaveReq
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求内容不是合法的 JSON")
		return
	}
	it, existed, err := h.svc.Add(owner(c), in)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"item": it, "existed": existed})
}

func (h *handlers) update(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in SaveReq
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求内容不是合法的 JSON")
		return
	}
	it, err := h.svc.Update(owner(c), id, in)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, it)
}

func (h *handlers) use(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.Use(owner(c), id); err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) remove(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.Delete(owner(c), id); err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}
