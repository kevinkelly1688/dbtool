// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

// Package admin 是「Redis 规则」页面与接口（仅超级用户）。规则模型与匹配在 internal/redisrule，本包只做 HTTP 与装配。
package admin

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"dbtool/internal/audit"
	"dbtool/internal/engine/redis"
	"dbtool/internal/model"
	"dbtool/internal/redisrule"
	"dbtool/internal/server"
)

// 按钮码（均为仅超级用户、不可授予权限组）。
const (
	BtnView = "redisrule.view"
	BtnEdit = "redisrule.edit"
)

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, _, ok := s.DB() // 仓库不是 store.Store（测试 / 演示）时没有元数据库，路由即 404
		if !ok {
			return
		}
		svc := redisrule.NewService(redisrule.NewGormRepo(db), auditSink{db})
		Register(s, rt, svc)
	})
	server.AddMenu(server.MenuItem{Group: "secure", Key: "sys-redis-rules", Href: "/settings/redis-rules", Label: "Redis 规则", Order: 40,
		Show: func(s *server.Server, a audit.Actor) bool {
			_, _, ok := s.DB()
			return ok && server.SuperOnly(s, a)
		}})
}

// auditSink 把审批类审计写进 dbt_audit_log（audit.Lifecycle 的目标）。
type auditSink struct{ db *gorm.DB }

func (a auditSink) WriteLifecycleAudit(e *model.AuditEntry) error { return a.db.Create(e).Error }

type handlers struct {
	s   *server.Server
	svc *redisrule.Service
}

// Register 登记页面与接口（测试里直接传入假存储的 Service）。
func Register(s *server.Server, rt server.Routes, svc *redisrule.Service) {
	h := &handlers{s: s, svc: svc}
	rt.Pages.GET("/settings/redis-rules", s.SuperuserPage(), s.RequireButtonPage(BtnView), h.page)

	// 服务端校验：既要是超级用户，又要有按钮（这两个按钮都是 locked，只有超级用户为真；两层校验互为兜底）
	g := rt.API.Group("/settings/redis-rules", s.SuperuserAPI())
	view := s.RequireButtonAPI(BtnView)
	edit := s.RequireButtonAPI(BtnEdit)
	g.GET("", view, h.list)
	g.POST("/test", view, h.test)
	g.POST("", edit, h.create)
	g.PUT("/:id", edit, h.update)
	g.POST("/:id/enable", edit, h.enable)
	g.POST("/:id/delete", edit, h.remove)
}

func (h *handlers) page(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "redisrule/index.html", server.PageData{Title: "Redis 规则", Nav: "sys-redis-rules"})
}

func fail(c *gin.Context, err error) {
	var ae *redisrule.APIError
	if errors.As(err, &ae) {
		server.APIFail(c, ae.Status, ae.Code, ae.Msg)
		return
	}
	slog.Error("Redis 上线规则接口出错", "path", c.Request.URL.Path, "err", err)
	server.APIFail(c, http.StatusInternalServerError, "internal", "服务内部错误")
}

func idParam(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "规则编号不合法")
		return 0, false
	}
	return uint(n), true
}

func bind(c *gin.Context, v any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256<<10)
	if err := c.ShouldBindJSON(v); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return false
	}
	return true
}

type instanceView struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

type listResp struct {
	Ready          bool                 `json:"table_ready"`
	Rules          []redisrule.Rule     `json:"rules"`
	Instances      []instanceView       `json:"instances"`
	Supported      []string             `json:"supported_commands"`
	HardForbidden  []string             `json:"hard_forbidden"`
	Templates      []redisrule.Template `json:"templates"`
	DefaultMaxKeys int                  `json:"default_max_keys"`
	MaxKeys        int                  `json:"max_keys_limit"`
}

func (h *handlers) list(c *gin.Context) {
	rules, err := h.svc.List()
	if err != nil {
		fail(c, err)
		return
	}
	if rules == nil {
		rules = []redisrule.Rule{}
	}
	out := listResp{Ready: h.svc.Ready(), Rules: rules, Instances: []instanceView{}, Supported: redis.SupportedCmds(), HardForbidden: redis.HardForbiddenList(),
		Templates: redisrule.Templates(), DefaultMaxKeys: redisrule.DefaultMaxKey, MaxKeys: redisrule.MaxKeysLimit}
	localizeTemplates(langOf(c), out.Templates)
	if list, err := h.s.Repo().ListInstances(); err == nil {
		for _, in := range list {
			if in.DBType == "redis" {
				out.Instances = append(out.Instances, instanceView{ID: in.ID, Name: in.InstanceName})
			}
		}
	}
	server.APIOK(c, out)
}

type ruleReq struct {
	redisrule.Rule
	Confirm bool `json:"confirm"`
}

func (h *handlers) create(c *gin.Context) {
	var in ruleReq
	if !bind(c, &in) {
		return
	}
	r, err := h.svc.Create(server.ActorOf(c), in.Rule, in.Confirm)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, r)
}

func (h *handlers) update(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in ruleReq
	if !bind(c, &in) {
		return
	}
	r, err := h.svc.Update(server.ActorOf(c), id, in.Rule, in.Confirm)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, r)
}

func (h *handlers) enable(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
		Confirm bool `json:"confirm"`
	}
	if !bind(c, &in) {
		return
	}
	r, err := h.svc.SetEnabled(server.ActorOf(c), id, in.Enabled, in.Confirm)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, r)
}

func (h *handlers) remove(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if !bind(c, &in) {
		return
	}
	if err := h.svc.Delete(server.ActorOf(c), id, in.Confirm); err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

// test 用当前规则试检查一段命令：不执行、不写库。
func (h *handlers) test(c *gin.Context) {
	var in struct {
		InstanceID uint   `json:"instance_id"`
		DB         int    `json:"db"`
		Text       string `json:"text"`
	}
	if !bind(c, &in) {
		return
	}
	if in.DB < 0 || in.DB > 1023 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "库编号不合法")
		return
	}
	out, err := h.svc.TestLines(in.InstanceID, in.DB, in.Text)
	if err != nil {
		fail(c, err)
		return
	}
	localizeVerdicts(langOf(c), out)
	server.APIOK(c, out)
}
