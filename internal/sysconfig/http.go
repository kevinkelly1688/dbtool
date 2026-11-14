// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

// Package sysconfig 是“平台设置 › 系统配置”页面：本项目自己的系统设置中心（仅超级用户）。
//
// 设置的登记（类型、默认值、校验、分组）、存储与修改规则都在 internal/settings；本包只负责页面与接口：
//
//	GET  /settings/config                      页面
//	GET  /api/v1/settings/config               全部分组与设置项（敏感项只告诉“已设置”，不回显内容）
//	PUT  /api/v1/settings/config               批量修改 {"changes": {"键": "值" | null}}，全部校验通过才写，一个事务
//	PUT  /api/v1/settings/config/:key          修改一项 {"value": "..."}（空串 / null = 恢复默认）
//	GET  /api/v1/settings/config/history       变更记录 ?key=&limit=&offset=
//
// 设计见 docs/settings-design.md。
package sysconfig

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/config"
	"dbtool/internal/i18n"
	"dbtool/internal/server"
	"dbtool/internal/settings"
)

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, m, ok := s.DB() // 仓库不是 store.Store（测试 / 演示）时没有元数据库，路由即 404
		if !ok {
			return
		}
		svc := settings.Open(db, m)
		register(s, rt, svc, func() Security {
			c := s.Config()
			return Security{PublicSecretKey: c.SecretKey == config.DefaultSecretKey(), SeparateMirageKey: c.MirageSecretKey != "" && c.MirageSecretKey != c.SecretKey}
		})
	})
	server.AddMenu(server.MenuItem{Group: "plat", Key: "sys-config", Href: "/settings/config", Label: "系统配置", Order: 40,
		Show: func(s *server.Server, a audit.Actor) bool {
			_, _, ok := s.DB()
			return ok && server.SuperOnly(s, a)
		}})
}

// Mount 在没有元数据库的环境（演示 / 测试）里登记设置中心的路由：svc 通常是 settings.NewMemory 返回的内存服务。
func Mount(s *server.Server, rt server.Routes, svc *settings.Service, sec Security) {
	register(s, rt, svc, func() Security { return sec })
}

// Security 是和密钥有关的两个事实（不含密钥值）。
type Security struct {
	PublicSecretKey   bool `json:"public_secret_key"`   // SECRET_KEY 是公开的缺省值：任何人都能伪造会话
	SeparateMirageKey bool `json:"separate_mirage_key"` // 加密字段密钥与 SECRET_KEY 不是同一把
}

// GroupView 是左侧导航的一项。
type GroupView struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Desc     string `json:"desc,omitempty"`
	Total    int    `json:"total"`
	Modified int    `json:"modified"` // 与默认值不同（或敏感项已设置）的项数
	Issues   int    `json:"issues"`   // 密文解不开的项数
}

// View 是列表接口的返回值。
type View struct {
	Ready         bool            `json:"ready"` // dbt_setting 表是否存在
	Groups        []GroupView     `json:"groups"`
	Items         []settings.Item `json:"items"`
	Security      Security        `json:"security"`
	Undecryptable []string        `json:"undecryptable"` // 密文解不开的设置键
}

type handlers struct {
	s   *server.Server
	svc *settings.Service
	sec func() Security
}

// register 登记路由（测试里直接传入假依赖）。只有超级用户能访问。
func register(s *server.Server, rt server.Routes, svc *settings.Service, sec func() Security) {
	h := &handlers{s: s, svc: svc, sec: sec}
	rt.Pages.GET("/settings/config", s.SuperuserPage(), h.page)
	rt.API.GET("/settings/config", s.SuperuserAPI(), h.list)
	rt.API.PUT("/settings/config", s.SuperuserAPI(), h.saveMany)
	rt.API.GET("/settings/config/history", s.SuperuserAPI(), h.history)
	rt.API.PUT("/settings/config/:key", s.SuperuserAPI(), h.saveOne)
}

func (h *handlers) page(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "sysconfig/index.html", server.PageData{Title: "系统配置", Nav: "sys-config"})
}

func (h *handlers) view() View {
	v := View{Groups: []GroupView{}, Items: []settings.Item{}, Undecryptable: []string{}}
	if h.sec != nil {
		v.Security = h.sec()
	}
	if !h.svc.Ready() {
		return v
	}
	v.Ready = true
	for _, it := range h.svc.Items() {
		if !settings.OffPage(it.Group) {
			v.Items = append(v.Items, it)
		}
	}
	if bad := h.svc.UndecryptableKeys(); bad != nil {
		v.Undecryptable = bad
	}
	idx := map[string]int{}
	for _, g := range settings.Groups {
		if settings.OffPage(g.Key) {
			continue
		}
		idx[g.Key] = len(v.Groups)
		v.Groups = append(v.Groups, GroupView{Key: g.Key, Label: g.Label, Desc: g.Desc})
	}
	for _, it := range v.Items {
		i, ok := idx[it.Group]
		if !ok {
			continue
		}
		v.Groups[i].Total++
		if it.Modified {
			v.Groups[i].Modified++
		}
		if it.Undecryptable {
			v.Groups[i].Issues++
		}
	}
	return v
}

func (h *handlers) list(c *gin.Context) { server.APIOK(c, h.view()) }

func actorOf(c *gin.Context) settings.Actor {
	a := server.ActorOf(c)
	return settings.Actor{ID: a.ID, Username: a.Username, Display: a.Display}
}

// fail 把设置服务的业务错误转成接口错误；逐项的校验错误放在 data.fields 里。
func fail(c *gin.Context, err error, what string, key string) {
	var se *settings.Error
	if errors.As(err, &se) {
		// message 由 HTTP 边缘统一翻译；逐项的校验错误在 data.fields 里（边缘不看），这里按请求语言翻译。
		// 只有一项出错时 message 是“键：说明”，说明部分也要翻译。
		lang := i18n.FromContext(c.Request.Context())
		msg := se.Message
		fields := se.Fields
		if lang != i18n.ZH && len(fields) > 0 {
			fields = make(map[string]string, len(se.Fields))
			for k, v := range se.Fields {
				fields[k] = i18n.TP(lang, v)
				if len(se.Fields) == 1 && se.Message == k+"："+v {
					msg = k + "：" + fields[k]
				}
			}
		}
		body := gin.H{"code": se.Code, "message": msg}
		if len(fields) > 0 {
			body["data"] = gin.H{"fields": fields}
		}
		c.AbortWithStatusJSON(se.Status, body)
		return
	}
	slog.Error(what, "key", key, "err", err)
	server.APIFail(c, http.StatusInternalServerError, "internal", "保存失败，请查看服务日志")
}

// saveMany 批量修改：{"changes": {"键": "值"}}；值为 null 或空串 = 恢复默认（敏感项 = 清除）。
func (h *handlers) saveMany(c *gin.Context) {
	var req struct {
		Changes map[string]*string `json:"changes"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 512<<10)
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Changes) == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误：需要 {\"changes\": {\"设置键\": \"值\"}}")
		return
	}
	if len(req.Changes) > 200 {
		server.APIFail(c, http.StatusBadRequest, "too_many", "一次最多修改 200 项")
		return
	}
	kv := make(map[string]string, len(req.Changes))
	for k, v := range req.Changes {
		if v == nil {
			kv[strings.TrimSpace(k)] = ""
		} else {
			kv[strings.TrimSpace(k)] = *v
		}
	}
	changes, err := h.svc.SetMany(actorOf(c), kv)
	if err != nil {
		fail(c, err, "修改系统设置失败", "") // i18n-ignore 仅写日志
		return
	}
	if changes == nil {
		changes = []settings.Change{}
	}
	server.APIOK(c, gin.H{"changes": changes, "changed": len(changes) > 0})
}

// saveOne 修改一项：{"value": "..."}。
func (h *handlers) saveOne(c *gin.Context) {
	var req struct {
		Value *string `json:"value"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	if err := c.ShouldBindJSON(&req); err != nil || req.Value == nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误：需要 {\"value\": \"…\"}")
		return
	}
	key := strings.TrimSpace(c.Param("key"))
	changes, err := h.svc.SetMany(actorOf(c), map[string]string{key: *req.Value})
	if err != nil {
		fail(c, err, "修改系统设置失败", key) // i18n-ignore 仅写日志
		return
	}
	if changes == nil {
		changes = []settings.Change{}
	}
	server.APIOK(c, gin.H{"changes": changes, "changed": len(changes) > 0})
}

// history 变更记录：新的在前。key 为空 = 全部。
func (h *handlers) history(c *gin.Context) {
	key := strings.TrimSpace(c.Query("key"))
	if key != "" && settings.Find(key) == nil {
		server.APIFail(c, http.StatusNotFound, "not_found", "没有这个设置项")
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := h.svc.Recent(key, limit, offset)
	if err != nil {
		slog.Error("读取系统设置变更记录失败", "err", err)
		server.APIFail(c, http.StatusInternalServerError, "internal", "读取变更记录失败，请查看服务日志")
		return
	}
	if rows == nil {
		rows = []settings.History{}
	}
	labels := map[string]string{}
	for _, d := range settings.Defs() {
		labels[d.Key] = d.Label
	}
	type row struct {
		settings.History
		Label string `json:"label"`
	}
	out := make([]row, 0, len(rows))
	for _, r := range rows {
		out = append(out, row{History: r, Label: labels[r.Key]})
	}
	server.APIOK(c, gin.H{"items": out, "limit": limit, "offset": offset})
}
