// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

// Package envadmin 是「环境管理」页面与接口（系统管理，仅超级用户）：维护实例可选的环境字典（生产 / 测试 / 预发 …）。
// 字典的存取与缓存在 internal/instext（envdict.go），本包只做 HTTP、审计与装配。
package envadmin

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"dbtool/internal/audit"
	"dbtool/internal/instext"
	"dbtool/internal/model"
	"dbtool/internal/server"
)

// 按钮码（均为仅超级用户、不可授予权限组）。
const (
	BtnView = "env.view"
	BtnEdit = "env.edit"
)

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, _, ok := s.DB() // 仓库不是 store.Store（测试 / 演示）时没有元数据库，路由即 404
		if !ok {
			return
		}
		instext.BindEnvDB(db) // 实例管理、防火墙、查询、工单读环境字典都走它
		Register(s, rt, db)
	})
	server.AddMenu(server.MenuItem{Group: "plat", Key: "sys-env", Href: "/settings/environments", Label: "环境设置", Order: 10,
		Show: func(s *server.Server, a audit.Actor) bool {
			_, _, ok := s.DB()
			return ok && server.SuperOnly(s, a)
		}})
}

type auditSink struct{ db *gorm.DB }

func (a auditSink) WriteLifecycleAudit(e *model.AuditEntry) error { return a.db.Create(e).Error }

type handlers struct {
	s  *server.Server
	db *gorm.DB
}

// Register 登记页面与接口。
func Register(s *server.Server, rt server.Routes, db *gorm.DB) {
	h := &handlers{s: s, db: db}
	rt.Pages.GET("/settings/environments", s.SuperuserPage(), s.RequireButtonPage(BtnView), h.page)
	g := rt.API.Group("/settings/environments", s.SuperuserAPI())
	view := s.RequireButtonAPI(BtnView)
	edit := s.RequireButtonAPI(BtnEdit)
	g.GET("", view, h.list)
	g.POST("", edit, h.create)
	g.PUT("/:code", edit, h.update)
	g.POST("/:code/delete", edit, h.remove)
}

func (h *handlers) page(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "env/index.html", server.PageData{Title: "环境设置", Nav: "sys-env"})
}

type item struct {
	instext.EnvDef
	Instances int    `json:"instances"` // 使用它的实例数
	Rules     int    `json:"rules"`     // 把它作为适用范围的防火墙规则数
	UpdatedBy string `json:"updated_by"`
	UpdatedAt string `json:"updated_at"`
}

func (h *handlers) rulesUsing() map[string]int {
	out := map[string]int{}
	var rows []struct {
		Env string
		N   int
	}
	if err := h.db.Session(&gorm.Session{NewDB: true}).Table("dbt_firewall_rule").Select("env, COUNT(*) AS n").Group("env").Scan(&rows).Error; err != nil {
		return out // 防火墙表没建
	}
	for _, r := range rows {
		out[r.Env] = r.N
	}
	return out
}

func (h *handlers) items() ([]item, error) {
	defs, err := instext.ListEnvRows(h.db)
	if err != nil {
		return nil, err
	}
	use, err := instext.EnvUsage(h.db)
	if err != nil {
		return nil, err
	}
	rules := h.rulesUsing()
	meta := map[string]instext.EnvRow{}
	if instext.EnvReady(h.db) {
		var rows []instext.EnvRow
		h.db.Session(&gorm.Session{NewDB: true}).Find(&rows)
		for _, r := range rows {
			meta[r.Code] = r
		}
	}
	out := make([]item, 0, len(defs))
	for _, d := range defs {
		it := item{EnvDef: d, Instances: use[d.Code], Rules: rules[d.Code]}
		if m, ok := meta[d.Code]; ok {
			it.UpdatedBy = m.UpdatedBy
			if !m.UpdatedAt.IsZero() {
				it.UpdatedAt = m.UpdatedAt.Format("2006-01-02 15:04:05")
			}
		}
		out = append(out, it)
	}
	return out, nil
}

func (h *handlers) list(c *gin.Context) {
	items, err := h.items()
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"table_ready": instext.EnvReady(h.db), "items": items, "max": instext.EnvMax, "code_max": instext.EnvCodeMax, "name_max": instext.EnvNameMax})
}

type req struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	IsProd  bool   `json:"is_prod"`
	Sort    int    `json:"sort"`
	Confirm bool   `json:"confirm"`
}

func bind(c *gin.Context, v any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	if err := c.ShouldBindJSON(v); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return false
	}
	return true
}

func (h *handlers) audit(c *gin.Context, action, extra string) {
	audit.Lifecycle(auditSink{h.db}, server.ActorOf(c), action, extra, time.Now())
}

func needConfirm(c *gin.Context, ok bool) bool {
	if !ok {
		server.APIFail(c, http.StatusBadRequest, "need_confirm", "需要二次确认")
	}
	return ok
}

func (h *handlers) create(c *gin.Context) {
	var in req
	if !bind(c, &in) || !needConfirm(c, in.Confirm) {
		return
	}
	code, name, err := instext.ValidateEnvInput(in.Code, in.Name, true)
	if err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if in.Sort < 0 || in.Sort > 9999 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "排序必须在 0–9999 之间")
		return
	}
	if in.Sort == 0 {
		in.Sort = 100
	}
	now := time.Now()
	actor := server.ActorOf(c)
	row := instext.EnvRow{Code: code, Name: name, IsProd: in.IsProd, SortOrder: in.Sort, CreatedBy: actor.Username, UpdatedBy: actor.Username, CreatedAt: now, UpdatedAt: now}
	if err := instext.CreateEnv(h.db, row); err != nil {
		fail(c, err)
		return
	}
	h.audit(c, "环境-新增", fmt.Sprintf("环境 %s「%s」，%s，排序 %d", code, name, prodText(in.IsProd), in.Sort)) // i18n-ignore 审计文本，存库后不翻译
	server.APIOK(c, gin.H{"code": code})
}

func (h *handlers) update(c *gin.Context) {
	code := strings.TrimSpace(c.Param("code"))
	var in req
	if !bind(c, &in) || !needConfirm(c, in.Confirm) {
		return
	}
	_, name, err := instext.ValidateEnvInput(code, in.Name, false)
	if err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if in.Sort < 0 || in.Sort > 9999 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "排序必须在 0–9999 之间")
		return
	}
	old, cur, err := instext.UpdateEnv(h.db, code, name, in.IsProd, in.Sort, server.ActorOf(c).Username, time.Now())
	if err != nil {
		fail(c, err)
		return
	}
	var diff []string
	if old.Name != cur.Name {
		diff = append(diff, fmt.Sprintf("名称「%s」->「%s」", old.Name, cur.Name)) // i18n-ignore 审计文本，存库后不翻译
	}
	if old.Prod != cur.Prod {
		diff = append(diff, fmt.Sprintf("%s -> %s", prodText(old.Prod), prodText(cur.Prod)))
	}
	if old.Sort != cur.Sort {
		diff = append(diff, fmt.Sprintf("排序 %d -> %d", old.Sort, cur.Sort)) // i18n-ignore 审计文本，存库后不翻译
	}
	if len(diff) > 0 {
		h.audit(c, "环境-修改", fmt.Sprintf("环境 %s：%s", code, strings.Join(diff, "；"))) // i18n-ignore 审计文本，存库后不翻译
	}
	server.APIOK(c, gin.H{"code": code})
}

func (h *handlers) remove(c *gin.Context) {
	code := strings.TrimSpace(c.Param("code"))
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if !bind(c, &in) || !needConfirm(c, in.Confirm) {
		return
	}
	if n := h.rulesUsing()[code]; n > 0 {
		server.APIFail(c, http.StatusConflict, "in_use", fmt.Sprintf("还有 %d 条防火墙规则把它作为适用范围，请先修改这些规则", n))
		return
	}
	def, err := instext.DeleteEnv(h.db, code)
	if err != nil {
		fail(c, err)
		return
	}
	h.audit(c, "环境-删除", fmt.Sprintf("环境 %s「%s」", def.Code, def.Name)) // i18n-ignore 审计文本，存库后不翻译
	server.APIOK(c, gin.H{"ok": true})
}

func prodText(p bool) string {
	if p {
		return "按生产对待" // i18n-ignore 审计文本，存库后不翻译
	}
	return "非生产" // i18n-ignore 审计文本，存库后不翻译
}

func fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, instext.ErrEnvTableMissing):
		server.APIFail(c, http.StatusServiceUnavailable, "table_missing", err.Error())
	case errors.Is(err, instext.ErrEnvNotFound):
		server.APIFail(c, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, instext.ErrEnvCodeTaken), errors.Is(err, instext.ErrEnvNameTaken):
		server.APIFail(c, http.StatusConflict, "duplicate", err.Error())
	case errors.Is(err, instext.ErrEnvInUse):
		server.APIFail(c, http.StatusConflict, "in_use", "仍有实例在使用该环境，请先把这些实例改成别的环境或“未设置”")
	case errors.Is(err, instext.ErrEnvBuiltin), errors.Is(err, instext.ErrEnvLimit):
		server.APIFail(c, http.StatusBadRequest, "bad_request", err.Error())
	default:
		slog.Error("环境管理接口出错", "path", c.Request.URL.Path, "err", err)
		server.APIFail(c, http.StatusInternalServerError, "internal", "服务内部错误")
	}
}
