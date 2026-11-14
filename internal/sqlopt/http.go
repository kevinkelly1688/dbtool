// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package sqlopt

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/i18n"
	"dbtool/internal/localize"
	"dbtool/internal/server"
)

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, _, ok := s.DB()
		if !ok {
			return
		}
		registerSettingsGuards(db)
		register(s, rt, NewService(s.Repo(), nil, nil, newStore(db)))
	})
	// 菜单：变更发布 › SQL 体检、性能优化 › 智能调优；路由没有登记时（仓库不是 store.Store）不显示入口。
	show := func(code string) func(s *server.Server, a audit.Actor) bool {
		return func(s *server.Server, a audit.Actor) bool {
			if _, _, ok := s.DB(); !ok {
				return false
			}
			return s.ActorHasButton(a, code)
		}
	}
	server.AddMenu(server.MenuItem{Group: "check", Key: "sqlanalyze", Href: "/sqlanalyze", Label: "SQL 体检", Order: 20, Show: show(PermMenuAnalyze)})
	server.AddMenu(server.MenuItem{Group: "opt", Key: "sqladvisor", Href: "/sqladvisor", Label: "智能调优", Order: 10, Show: show(PermMenuAdvisor)})
}

type handlers struct {
	s   *server.Server
	svc *Service
}

// register 登记页面与接口（测试里直接传入假依赖的 Service）。
func register(s *server.Server, rt server.Routes, svc *Service) {
	h := &handlers{s: s, svc: svc}
	server.AddWorker(func(ctx context.Context) {
		svc.SetRoot(ctx)
		tk := time.NewTicker(5 * time.Minute)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				svc.gc()
			}
		}
	})

	rt.Pages.GET("/sqlanalyze", s.RequireButtonPage(PermMenuAnalyze), h.analyzePage)
	rt.Pages.GET("/sqladvisor", s.RequireButtonPage(PermMenuAdvisor), h.advisorPage)

	anyMenu := s.RequireButtonAPI(PermMenuAnalyze, PermMenuAdvisor)
	anyJob := s.RequireButtonAPI(PermMenuAnalyze, PermMenuAdvisor, PermAnalyze, PermSOAR, PermSQLTuning)
	g := rt.API.Group("/sqlopt")
	g.GET("/options", anyMenu, h.apiOptions)
	g.GET("/instances/:id/databases", anyMenu, h.apiDatabases)
	g.POST("/parse", s.RequireButtonAPI(PermAnalyze), h.apiParse)
	g.POST("/analyze", s.RequireButtonAPI(PermAnalyze), h.apiAnalyze)
	g.GET("/jobs", anyJob, h.apiJobs)
	g.GET("/jobs/:id", anyJob, h.apiJob)
	g.GET("/jobs/:id/items/:index", anyJob, h.apiJobItem)
	g.POST("/jobs/:id/cancel", anyJob, h.apiJobCancel)
	g.POST("/explain", s.RequireButtonAPI(PermMenuAdvisor), h.apiExplain)
	g.POST("/advise", s.RequireButtonAPI(PermMenuAdvisor), h.apiAdvise)
	g.POST("/format", s.RequireButtonAPI(PermMenuAdvisor), h.apiFormat)
	g.POST("/optimize", s.RequireButtonAPI(PermSOAR, PermSQLTuning), h.apiOptimize)
}

// ---- 页面 ----

func (h *handlers) analyzePage(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "sqlopt/analyze.html", server.PageData{Title: "SQL 体检", Nav: "sqlanalyze",
		Data: gin.H{"CanAnalyze": h.s.HasButton(c, PermAnalyze), "MaxMB": MaxTextBytes >> 20}})
}

func (h *handlers) advisorPage(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "sqlopt/advisor.html", server.PageData{Title: "智能调优", Nav: "sqladvisor"})
}

// ---- 接口 ----

// rawKeys 是响应里原样展示的数据字段（SQL、指纹、工具输出、结果集、文件名、实例 / 库名……）：不参与翻译，
// 其余字段（建议标题、说明、状态提示等服务端生成的文案）按请求语言翻译。
var rawKeys = []string{"sql", "full_sql", "fingerprint", "output", "json", "structure", "rows", "columns", "index_ddl",
	"file", "instance", "db", "owner", "name", "tables", "table"}

// respond 输出成功响应，并把其中服务端生成的中文文案翻译成请求语言（中文请求原样输出）。
func respond(c *gin.Context, v any) {
	server.APIOK(c, localize.Value(i18n.FromContext(c.Request.Context()), v, rawKeys...))
}

func writeErr(c *gin.Context, err error) {
	var e *Error
	if errors.As(err, &e) {
		server.APIFail(c, e.HTTP, e.Code, e.Message)
		return
	}
	slog.Error("SQL 优化模块请求失败", "path", c.Request.URL.Path, "err", err)
	server.APIFail(c, http.StatusInternalServerError, "internal", "服务异常，请稍后再试")
}

// bind 读取 JSON 请求体，限制大小（上传的文本是不可信数据）。
func bind(c *gin.Context, v any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxRequestBytes)
	if err := c.ShouldBindJSON(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			server.APIFail(c, http.StatusRequestEntityTooLarge, "too_large", "请求内容过大（上传文本不能超过 5MB）")
			return false
		}
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return false
	}
	return true
}

type toolStatus struct {
	Ready   bool   `json:"ready"`
	Message string `json:"message,omitempty"`
}

func (h *handlers) toolStatus() map[string]toolStatus {
	st := func(err error) toolStatus {
		if err == nil {
			return toolStatus{Ready: true}
		}
		return toolStatus{Message: err.Error()}
	}
	out := map[string]toolStatus{}
	cf, err := h.svc.soarConfig()
	if err == nil && cf.TestDSN == "" {
		err = newErr(409, "tool_not_configured", "尚未配置 SOAR 的测试环境（“系统配置”页的“SQL 优化工具”）。SQL 分析页面的离线分析不受影响，但“智能调优”里的 SOAR 需要它。")
	}
	out[ToolSOAR] = st(err)
	out[ToolTuning] = toolStatus{Ready: true}
	return out
}

func (h *handlers) apiOptions(c *gin.Context) {
	list, err := h.svc.Instances(server.ActorOf(c))
	if err != nil {
		writeErr(c, err)
		return
	}
	perm := func(code string) bool { return h.s.HasButton(c, code) }
	respond(c, gin.H{
		"instances": list,
		"tools":     h.toolStatus(),
		"perms": gin.H{"analyze": perm(PermAnalyze), "explain": perm(PermMenuAdvisor),
			ToolSOAR: perm(PermSOAR), ToolTuning: perm(PermSQLTuning)},
		"limits":        gin.H{"max_text_bytes": MaxTextBytes, "max_statements": MaxStatements, "max_sql_bytes": MaxSQLBytes},
		"can_configure": server.ActorOf(c).IsSuperuser, // 超级用户可在系统配置页设置 SOAR
	})
}

func (h *handlers) apiDatabases(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "实例参数错误")
		return
	}
	d, err := h.svc.DatabasesDetail(c.Request.Context(), server.ActorOf(c), uint(id))
	if err != nil {
		writeErr(c, err)
		return
	}
	if c.Query("detail") == "1" { // 页面用：带统计，列表为空时能说明原因
		server.APIOK(c, d)
		return
	}
	server.APIOK(c, d.Databases)
}

type parsedRow struct {
	ID    string    `json:"sql_id"`
	SQL   string    `json:"sql"`
	Chars int       `json:"chars"`
	Stats *LogStats `json:"stats,omitempty"`
	Error string    `json:"error,omitempty"`
}

func (h *handlers) apiParse(c *gin.Context) {
	var req struct {
		Text string `json:"text"`
	}
	if !bind(c, &req) {
		return
	}
	rows, format, err := h.svc.Parse(req.Text)
	if err != nil {
		writeErr(c, err)
		return
	}
	out := make([]parsedRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, parsedRow{ID: r.ID, SQL: truncRunes(r.SQL, itemSQLPreview), Chars: len([]rune(r.SQL)), Stats: r.Stats, Error: r.Err})
	}
	respond(c, gin.H{"rows": out, "total": len(out), "format": format})
}

func (h *handlers) apiAnalyze(c *gin.Context) {
	var req AnalyzeReq
	if !bind(c, &req) {
		return
	}
	v, err := h.svc.StartAnalyze(c.Request.Context(), server.ActorOf(c), req)
	if err != nil {
		writeErr(c, err)
		return
	}
	respond(c, v)
}

// apiJobs 返回“最近报告”：仍在保留期内的分析任务（本人的；超级用户可见所有人的）。
func (h *handlers) apiJobs(c *gin.Context) {
	respond(c, gin.H{"jobs": h.svc.ListJobs(server.ActorOf(c)), "keep_minutes": int(jobTTL / time.Minute)})
}

func (h *handlers) apiJob(c *gin.Context) {
	v, err := h.svc.Job(server.ActorOf(c), c.Param("id"))
	if err != nil {
		writeErr(c, err)
		return
	}
	respond(c, v)
}

func (h *handlers) apiJobItem(c *gin.Context) {
	idx, err := strconv.Atoi(c.Param("index"))
	if err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "条目参数错误")
		return
	}
	v, err := h.svc.JobItem(server.ActorOf(c), c.Param("id"), idx)
	if err != nil {
		writeErr(c, err)
		return
	}
	respond(c, v)
}

func (h *handlers) apiJobCancel(c *gin.Context) {
	if err := h.svc.CancelJob(server.ActorOf(c), c.Param("id")); err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiExplain(c *gin.Context) {
	var req ExplainReq
	if !bind(c, &req) {
		return
	}
	v, err := h.svc.Explain(c.Request.Context(), server.ActorOf(c), req)
	if err != nil {
		writeErr(c, err)
		return
	}
	respond(c, v)
}

func (h *handlers) apiAdvise(c *gin.Context) {
	var req AdviseReq
	if !bind(c, &req) {
		return
	}
	v, err := h.svc.Advise(c.Request.Context(), server.ActorOf(c), req)
	if err != nil {
		writeErr(c, err)
		return
	}
	respond(c, v)
}

func (h *handlers) apiFormat(c *gin.Context) {
	var req struct {
		SQL string `json:"sql"`
	}
	if !bind(c, &req) {
		return
	}
	if len(req.SQL) > MaxSQLBytes*4 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "SQL 过长")
		return
	}
	server.APIOK(c, gin.H{"sql": PrettySQL(req.SQL)})
}

func (h *handlers) apiOptimize(c *gin.Context) {
	var req OptimizeReq
	if !bind(c, &req) {
		return
	}
	perm := ToolPerm(req.Tool)
	if perm == "" {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "不支持的优化方式")
		return
	}
	if !h.s.HasButton(c, perm) {
		server.APIFail(c, http.StatusForbidden, "forbidden", "没有权限使用该优化方式")
		return
	}
	v, err := h.svc.StartOptimize(c.Request.Context(), server.ActorOf(c), req)
	if err != nil {
		writeErr(c, err)
		return
	}
	respond(c, v)
}
