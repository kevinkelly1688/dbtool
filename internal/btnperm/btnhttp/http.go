// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

// Package btnhttp 是按钮级权限（internal/btnperm）的 HTTP 接口：权限树、权限组按钮授权、单用户单独授权 / 禁用、无权限按钮的呈现方式。
// 单独成包是因为 internal/server 要引用 btnperm，而 btnperm 不能反过来引用 server。
// 管理类接口仅超级用户（每次请求回库再确认一次）；/api/v1/button-perms/me 任何登录用户可用，前端据此显示 / 置灰按钮。
package btnhttp

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/btnperm"
	"dbtool/internal/i18n"
	"dbtool/internal/server"
)

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, _, ok := s.DB()
		if !ok {
			return
		}
		repo := btnperm.NewGormRepo(db)
		res := btnperm.Default()
		if res == nil {
			res = btnperm.New(repo)
			btnperm.SetDefault(res)
		}
		Register(btnperm.NewService(repo, res), res)(s, rt)
	})
}

type handlers struct {
	s   *server.Server
	svc *btnperm.Service
	res btnperm.Checker
}

// Register 返回登记函数（init 里用真实仓库装配；测试里传入假仓库构造的服务，也可与其他模块的登记函数一起使用以检查路由不冲突）。
func Register(svc *btnperm.Service, res btnperm.Checker) func(s *server.Server, rt server.Routes) {
	return func(s *server.Server, rt server.Routes) {
		h := &handlers{s: s, svc: svc, res: res}
		rt.API.GET("/button-perms/me", h.me)

		g := rt.API.Group("/settings/button-perms", s.SuperuserAPI(), h.freshAdmin())
		g.GET("/registry", h.registry)
		g.GET("/groups/:id", h.groupGet)
		g.PUT("/groups/:id", h.groupSave)
		g.POST("/groups/:id/diff", h.groupDiff)
		g.GET("/users/:id", h.userGet)
		g.PUT("/users/:id/overrides", h.overridesSet)
		g.DELETE("/users/:id/overrides", h.overridesRemove)
		g.GET("/setting", h.settingGet)
		g.PUT("/setting", h.settingSet)
	}
}

// freshAdmin 用数据库里的最新状态确认操作者仍是启用的超级用户（会话 / JWT 里的标记在有效期内不会自动失效）。
func (h *handlers) freshAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		ok, err := h.svc.IsAdmin(server.PrincipalOf(c).ID)
		if err != nil {
			writeErr(c, err)
			return
		}
		if !ok {
			server.APIFail(c, http.StatusForbidden, "forbidden", "仅超级用户可以操作")
			return
		}
		c.Next()
	}
}

func actorOf(c *gin.Context) btnperm.Actor {
	p := server.PrincipalOf(c)
	return btnperm.Actor{ID: p.ID, Username: p.Username, Display: p.Display}
}

func writeErr(c *gin.Context, err error) {
	var e *btnperm.Error
	if errors.As(err, &e) {
		c.AbortWithStatusJSON(e.HTTP, e)
		return
	}
	slog.Error("按钮权限请求失败", "path", c.Request.URL.Path, "err", err)
	server.APIFail(c, http.StatusInternalServerError, "internal", "服务异常，请稍后再试")
}

// localizeWarnings 把保存结果里服务层拼好的告警按请求语言翻译（它们在成功响应里，不经过错误响应的边缘翻译）。
func localizeWarnings(c *gin.Context, v *btnperm.SaveResult) {
	if v == nil {
		return
	}
	lang := i18n.FromContext(c.Request.Context())
	for i, w := range v.Warnings {
		v.Warnings[i] = i18n.TP(lang, w)
	}
}

func bad(c *gin.Context, msg string) { server.APIFail(c, http.StatusBadRequest, "bad_request", msg) }

func idParam(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		bad(c, "参数非法")
		return 0, false
	}
	return uint(n), true
}

func bind(c *gin.Context, v any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	if err := c.ShouldBindJSON(v); err != nil {
		bad(c, "请求格式错误")
		return false
	}
	return true
}

func respond(c *gin.Context, data any, err error) {
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, data)
}

// ---- 当前用户 ----

// me 返回当前用户拥有的按钮与无权限按钮的呈现方式，前端据此显示 / 隐藏 / 置灰按钮（见 web/static/js/btnperm.js）。
func (h *handlers) me(c *gin.Context) {
	p := server.PrincipalOf(c)
	set := btnperm.DefaultSetting()
	if h.res != nil {
		if v, err := h.res.Setting(); err == nil {
			set = v
		}
	}
	var codes []string
	if p.IsSuperuser {
		codes = btnperm.Codes()
	} else if h.res != nil {
		// 一次算出全部按钮；逐个 HasButton 会对每个按钮各查一次库（真库上一次 me 上百次查询、几十秒）。
		m, err := h.res.AllowedButtons(p.ID)
		if err != nil {
			writeErr(c, err)
			return
		}
		for _, code := range btnperm.GrantableCodes() {
			if m[code] {
				codes = append(codes, code)
			}
		}
	} else {
		for _, code := range btnperm.GrantableCodes() {
			if h.s.HasButton(c, code) {
				codes = append(codes, code)
			}
		}
	}
	if codes == nil {
		codes = []string{}
	}
	server.APIOK(c, gin.H{"deny_display": set.DenyDisplay, "superuser": p.IsSuperuser, "allowed": codes})
}

// ---- 管理 ----

func (h *handlers) registry(c *gin.Context) {
	v, err := h.svc.Registry()
	respond(c, v, err)
}

func (h *handlers) groupGet(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	v, err := h.svc.Group(id)
	respond(c, v, err)
}

type groupReq struct {
	Codes  []string `json:"codes"`
	Reason string   `json:"reason"`
}

func (h *handlers) groupSave(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in groupReq
	if !bind(c, &in) {
		return
	}
	v, err := h.svc.SaveGroup(actorOf(c), id, in.Codes, in.Reason)
	localizeWarnings(c, v)
	respond(c, v, err)
}

func (h *handlers) groupDiff(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in groupReq
	if !bind(c, &in) {
		return
	}
	v, err := h.svc.GroupDiff(id, in.Codes)
	localizeWarnings(c, v)
	respond(c, v, err)
}

func (h *handlers) userGet(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	v, err := h.svc.User(id)
	respond(c, v, err)
}

type overrideReq struct {
	Effect   string   `json:"effect"`
	Codes    []string `json:"codes"`
	ExpireAt string   `json:"expire_at"` // 空 = 长期；YYYY-MM-DD（当天结束）或 YYYY-MM-DD HH:MM
	Reason   string   `json:"reason"`
}

// parseExpire 解析到期时间（本地时间）。只给日期时取当天 23:59:59。
func parseExpire(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	for _, l := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return &t, nil
		}
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return nil, &btnperm.Error{HTTP: http.StatusBadRequest, Code: "bad_request", Message: "到期时间格式应为 YYYY-MM-DD 或 YYYY-MM-DD HH:MM"}
	}
	t = t.Add(24*time.Hour - time.Second)
	return &t, nil
}

func (h *handlers) overridesSet(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in overrideReq
	if !bind(c, &in) {
		return
	}
	exp, err := parseExpire(in.ExpireAt)
	if err != nil {
		writeErr(c, err)
		return
	}
	err = h.svc.SetOverrides(actorOf(c), id, btnperm.OverrideReq{Effect: in.Effect, Codes: in.Codes, ExpireAt: exp, Reason: in.Reason})
	respond(c, gin.H{"ok": true}, err)
}

func (h *handlers) overridesRemove(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in groupReq
	if !bind(c, &in) {
		return
	}
	err := h.svc.RemoveOverrides(actorOf(c), id, in.Codes, in.Reason)
	respond(c, gin.H{"ok": true}, err)
}

func (h *handlers) settingGet(c *gin.Context) {
	v, err := h.svc.GetSetting()
	respond(c, v, err)
}

func (h *handlers) settingSet(c *gin.Context) {
	var in btnperm.SettingReq
	if !bind(c, &in) {
		return
	}
	v, err := h.svc.SetSetting(actorOf(c), in)
	respond(c, v, err)
}
