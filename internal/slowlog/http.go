// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package slowlog

import (
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/i18n"
	"dbtool/internal/localize"
	"dbtool/internal/server"
)

// registered 记录路由是否已登记（仓库不支持时不登记，菜单也不应显示）。
var registered atomic.Bool

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, mirage, ok := s.DB()
		if !ok {
			return
		}
		st := newStore(db, mirage)
		coll := NewCollector(ConfigFromEnv(os.Getenv), st, NewDBSource(st.EngineConn), nil)
		// Redis 慢查询：接在同一个采集调度上（同一开关、同一退出信号），不影响上面的 MySQL 采集
		rs := &RedisSlow{Repo: st, Open: OpenRedisTarget, Conn: st.EngineConn, Collector: NewRedisCollector(st, OpenRedisTarget, st.EngineConn, nil)}
		rs.Collector.MinGap = redisGapFromEnv(os.Getenv)
		coll.AddHook(rs.Collector.RunOnce)
		register(s, rt, NewService(st, Options{Collector: coll, Redis: rs}))
		server.AddWorker(coll.Run)
	})
	server.AddMenu(server.MenuItem{Group: "opt", Key: "slowlogs", Href: "/slowlogs", Label: "慢日志检索", Order: 20, Show: showMenuFor("slowlogs.view")})
	server.AddMenu(server.MenuItem{Group: "opt", Key: "slowquery", Href: "/slowquery", Label: "慢 SQL 洞察", Order: 30, Show: showMenuFor("slowquery.view")})
}

// showMenuFor：拥有这个页面自己的“查看”按钮（超级用户恒真），且路由已登记。
func showMenuFor(button string) func(s *server.Server, a audit.Actor) bool {
	return func(s *server.Server, a audit.Actor) bool {
		return registered.Load() && s.ActorHasButton(a, button)
	}
}

type handlers struct {
	s   *server.Server
	svc *Service
}

// register 登记页面与接口（测试里直接传入假服务）。
func register(s *server.Server, rt server.Routes, svc *Service) {
	registered.Store(true)
	h := &handlers{s: s, svc: svc}
	rt.Pages.GET("/slowlogs", s.RequireButtonPage("slowlogs.view"), h.pageLogs)
	rt.Pages.GET("/slowquery", s.RequireButtonPage("slowquery.view"), h.pageQuery)

	// 两个页面共用同一组数据接口：任一页面的“查看”按钮即可
	g := rt.API.Group("/slowlog", s.RequireButtonAPI(viewButtons...))
	g.GET("/instances", h.apiInstances)
	g.GET("/slowlogs", h.apiGroups)
	g.GET("/slowlogs/detail", h.apiDetail)
	g.GET("/slowlogs/stats", h.apiGroupStats)
	g.GET("/collector", h.apiCollector)
	g.GET("/databases", h.apiDatabases)
	g.GET("/review", h.apiReview)
	g.GET("/review/history", h.apiHistory)
	g.GET("/review/trend", h.apiTrend)
	if svc.redisOn() {
		h.registerRedis(s, rt, g)
	}
	// 获取优化建议只认 slowquery.advise 按钮，实例范围在服务里校验。
	rt.API.POST("/slowlog/advise", s.RequireButtonAPI(btnAdvise), h.apiAdvise)
}

// ---- 页面 ----

type pageInfo struct {
	CanAdvise bool
	// CanRedisReset：有 redis.slowlog_reset 按钮（页面才显示“重置慢日志”）
	CanRedisReset bool
	// CanOpenAdvisor：能进“SQL 优化工具”页（optimize.explain 按钮），慢查日志页据此显示“在优化工具中打开”
	CanOpenAdvisor bool
}

// ResetNameHTML 是“重置 Redis 慢日志”弹窗里实例名的占位元素（脚本按 id 填入实例名）。
// 模板用 th 把它嵌进整句译文，不必把句子拆成碎片。
func (pageInfo) ResetNameHTML() template.HTML { return `<b id="rq-mname"></b>` }

func (h *handlers) canAdvise(c *gin.Context) bool { return h.s.HasButton(c, btnAdvise) }

func (h *handlers) pageLogs(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "slowlog/slowlogs.html", server.PageData{Title: "慢日志检索", Nav: "slowlogs", Data: logsPageInfo{CanOptimize: h.canOptimize(c)}})
}

func (h *handlers) pageQuery(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "slowlog/slowquery.html", server.PageData{Title: "慢 SQL 洞察", Nav: "slowquery",
		Data: pageInfo{CanAdvise: h.canAdvise(c), CanOpenAdvisor: h.s.HasButton(c, btnOpenAdvisor),
			CanRedisReset: h.svc.redisOn() && h.s.HasButton(c, redisResetButton)}})
}

// ---- 接口 ----

// lang 是本次请求的界面语言（中间件写进 request.Context）。
func lang(c *gin.Context) string { return i18n.FromContext(c.Request.Context()) }

// tpText 翻译服务端拼出的提示 / 标签（先按原文精确查目录，再按带 %s %d 的格式串匹配）；中文原样返回。
func tpText(c *gin.Context, s string) string { return i18n.TP(lang(c), s) }

// tpTexts 就地翻译一组提示。
func tpTexts(c *gin.Context, ss []string) {
	for i := range ss {
		ss[i] = tpText(c, ss[i])
	}
}

func writeErr(c *gin.Context, err error) {
	var e *Error
	if errors.As(err, &e) {
		server.APIFail(c, e.HTTP, e.Code, e.Message)
		return
	}
	slog.Error("慢日志请求失败", "path", c.Request.URL.Path, "err", err)
	server.APIFail(c, http.StatusInternalServerError, "internal", "服务异常，请稍后再试")
}

// intQuery 读取非负整数查询参数；缺省返回 0，非法返回 false（已写出 400）。
func intQuery(c *gin.Context, name string) (int, bool) {
	v := c.Query(name)
	if v == "" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", fmt.Sprintf("参数 %s 不合法", name))
		return 0, false
	}
	return n, true
}

func (h *handlers) apiInstances(c *gin.Context) {
	list, err := h.svc.Instances(server.PrincipalOf(c), c.DefaultQuery("page", "query"))
	if err != nil {
		writeErr(c, err)
		return
	}
	for i := range list {
		list[i].Hint = tpText(c, list[i].Hint)
	}
	server.APIOK(c, list)
}

func (h *handlers) apiGroups(c *gin.Context) {
	inst, ok1 := intQuery(c, "instance_id")
	limit, ok2 := intQuery(c, "limit")
	offset, ok3 := intQuery(c, "offset")
	if !ok1 || !ok2 || !ok3 {
		return
	}
	page, err := h.svc.Groups(server.PrincipalOf(c), GroupParams{InstanceID: uint(inst), DB: c.Query("db_name"), Search: c.Query("q"), Start: c.Query("start"), End: c.Query("end"),
		Sort: c.Query("sort"), Order: c.Query("order"), Limit: limit, Offset: offset})
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, page)
}

func (h *handlers) apiDetail(c *gin.Context) {
	inst, ok := intQuery(c, "instance_id")
	if !ok {
		return
	}
	rows, err := h.svc.Detail(server.PrincipalOf(c), DetailParams{InstanceID: uint(inst), DB: c.Query("db_name"), MD5: c.Query("sql_md5"), Day: c.Query("exec_date")})
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"total": len(rows), "rows": rows})
}

func (h *handlers) apiCollector(c *gin.Context) {
	ov, err := h.svc.CollectorOverview(server.PrincipalOf(c))
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, localize.Value(lang(c), ov, "instance_name"))
}

// reviewParams 读取统计/明细/趋势/库列表共用的查询参数。
func reviewParams(c *gin.Context) (ReviewParams, bool) {
	inst, ok1 := intQuery(c, "instance_id")
	limit, ok2 := intQuery(c, "limit")
	offset, ok3 := intQuery(c, "offset")
	if !ok1 || !ok2 || !ok3 {
		return ReviewParams{}, false
	}
	return ReviewParams{InstanceID: uint(inst), DB: c.Query("db_name"), Source: c.Query("source"), SQLID: c.Query("sql_id"), Search: c.Query("q"),
		Start: c.Query("start"), End: c.Query("end"), Sort: c.Query("sort"), Order: c.Query("order"), Bucket: c.Query("bucket"),
		Limit: limit, Offset: offset, MinSeconds: c.Query("min_seconds"), All: c.Query("all") == "1"}, true
}

func (h *handlers) apiDatabases(c *gin.Context) {
	in, ok := reviewParams(c)
	if !ok {
		return
	}
	list, err := h.svc.Databases(server.PrincipalOf(c), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, list)
}

func (h *handlers) apiReview(c *gin.Context) {
	in, ok := reviewParams(c)
	if !ok {
		return
	}
	page, err := h.svc.Review(c.Request.Context(), server.PrincipalOf(c), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	page.Label = tpText(c, page.Label)
	tpTexts(c, page.Notes)
	server.APIOK(c, page)
}

func (h *handlers) apiHistory(c *gin.Context) {
	in, ok := reviewParams(c)
	if !ok {
		return
	}
	page, err := h.svc.History(c.Request.Context(), server.PrincipalOf(c), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	page.Label = tpText(c, page.Label)
	server.APIOK(c, page)
}

func (h *handlers) apiTrend(c *gin.Context) {
	in, ok := reviewParams(c)
	if !ok {
		return
	}
	tr, err := h.svc.Trend(c.Request.Context(), server.PrincipalOf(c), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	tr.Label = tpText(c, tr.Label)
	server.APIOK(c, tr)
}

func (h *handlers) apiAdvise(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2*MaxAdviseSQL)
	var in AdviseParams
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	adv, err := h.svc.Advise(c.Request.Context(), server.PrincipalOf(c), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, localize.Value(lang(c), adv, "index_ddl"))
}
