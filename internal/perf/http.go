// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package perf

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/notify"
	"dbtool/internal/query"
	"dbtool/internal/server"
)

// 权限（按钮，登记表 internal/btnperm）：perf.view 查看与处理自己团队的工单；perf.create / assign / verify / approve / adjust / reopen / close_dup
// 是 DBA 侧操作；perfcfg.* 管采集 / 团队 / SLA 配置页面。持有任一管理类按钮（managerButtons）的人也能打开工单页面，并看到全部工单。

type handlers struct {
	s    *server.Server
	svc  *Service
	repo Repo
}

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, mirage, ok := s.DB()
		if !ok {
			return
		}
		repo := newStore(db, mirage)
		var n notify.Notifier = notify.Noop{}
		if src, ok := s.Repo().(notify.Source); ok {
			if dir, ok := s.Repo().(notify.Directory); ok {
				n = notify.NewDispatcher(src, dir)
			}
		}
		svc := NewService(repo, Options{Notifier: n, Explainer: NewExplainer(repo), BaseURL: s.Config().PublicURL})
		registerWith(s, rt, svc, repo)
		server.AddWorker(func(ctx context.Context) { svc.RunWorker(ctx, time.Minute) })
	})
	menuShow := func(codes ...string) func(s *server.Server, a audit.Actor) bool {
		return func(s *server.Server, a audit.Actor) bool {
			for _, c := range codes {
				if s.ActorHasButton(a, c) {
					return true
				}
			}
			return false
		}
	}
	menuOpen := menuShow(append([]string{btnView}, managerButtons...)...)
	server.AddMenu(server.MenuItem{Group: "perf", Key: "perf-list", Href: "/perf", Label: "问题工单", Order: 10, Show: menuOpen})
	server.AddMenu(server.MenuItem{Group: "perf", Key: "perf-board", Href: "/perf/board", Label: "团队榜单", Order: 20, Show: menuOpen})
	// 配置页面：按“查看配置”按钮（perfcfg.view）显示
	menuCfg := menuShow(btnCfgView)
	server.AddMenu(server.MenuItem{Group: "perf", Key: "perf-collect", Href: "/perf/config/collect", Label: "采集配置", Order: 30, Show: menuCfg})
	server.AddMenu(server.MenuItem{Group: "perf", Key: "perf-team", Href: "/perf/config/team", Label: "团队配置", Order: 40, Show: menuCfg})
	server.AddMenu(server.MenuItem{Group: "perf", Key: "perf-sla", Href: "/perf/config/sla", Label: "SLA 策略", Order: 50, Show: menuCfg})
}

// registerWith 登记页面与接口（测试里传入假仓库构造的服务）。
func registerWith(s *server.Server, rt server.Routes, svc *Service, repo Repo) {
	h := &handlers{s: s, svc: svc, repo: repo}

	view := h.openGuard(true)
	cfg := s.RequireButtonPage(btnCfgView)
	rt.Pages.GET("/perf", view, h.pageList)
	rt.Pages.GET("/perf/board", view, h.pageBoard)
	rt.Pages.GET("/perf/config/collect", cfg, h.pageCollect)
	rt.Pages.GET("/perf/config/team", cfg, h.pageTeam)
	rt.Pages.GET("/perf/config/sla", cfg, h.pageSLA)
	rt.Pages.GET("/perf/:id", view, h.pageDetail)

	av := h.openGuard(false)
	create := s.RequireButtonAPI(btnCreate)
	// 配置接口：按钮级权限（查看 / 采集 / 团队 / SLA）
	cv, cc, ct, cs := s.RequireButtonAPI(btnCfgView), s.RequireButtonAPI("perfcfg.collect"), s.RequireButtonAPI("perfcfg.team"), s.RequireButtonAPI("perfcfg.sla")
	rt.API.GET("/perf/my-todo", av, h.apiMyTodo)
	rt.API.GET("/perf/options", av, h.apiOptions)
	rt.API.GET("/perf/issues", av, h.apiList)
	rt.API.POST("/perf/issues", create, h.apiCreate)
	rt.API.POST("/perf/issues/from-slowlog", create, h.apiCreateFromSlowLog)
	rt.API.GET("/perf/issues/:id", av, h.apiDetail)
	rt.API.POST("/perf/issues/:id/actions", av, h.apiAct)
	rt.API.GET("/perf/issues/:id/trend", av, h.apiTrend)
	rt.API.POST("/perf/issues/:id/diagnose", s.RequireButtonAPI("perf.reopen"), h.apiDiagnose)
	rt.API.GET("/perf/board", av, h.apiBoard)

	rt.API.GET("/perf/config/collect", cv, h.apiCollectList)
	rt.API.POST("/perf/config/collect", cc, h.apiCollectSave)
	rt.API.PUT("/perf/config/collect/:id", cc, h.apiCollectSave)
	rt.API.DELETE("/perf/config/collect/:id", cc, h.apiCollectDelete)
	rt.API.POST("/perf/config/collect/preview", cc, h.apiCollectPreview)
	rt.API.POST("/perf/config/collect/init", cc, h.apiCollectInit)
	rt.API.GET("/perf/config/team", cv, h.apiTeamList)
	rt.API.POST("/perf/config/teams", ct, h.apiTeamSave)
	rt.API.PUT("/perf/config/teams/:id", ct, h.apiTeamSave)
	rt.API.DELETE("/perf/config/teams/:id", ct, h.apiTeamDelete)
	rt.API.POST("/perf/config/routes", ct, h.apiRouteSave)
	rt.API.PUT("/perf/config/routes/:id", ct, h.apiRouteSave)
	rt.API.DELETE("/perf/config/routes/:id", ct, h.apiRouteDelete)
	rt.API.GET("/perf/config/sla", cv, h.apiSLAGet)
	rt.API.PUT("/perf/config/sla", cs, h.apiSLASave)
	rt.API.POST("/perf/config/route-test", cv, h.apiRouteTest)
	rt.API.PUT("/perf/config/collect-window", cc, h.apiWindowSave)
}

// ---- 权限 ----

// isManager 判断当前用户是否持有任一管理类按钮（Viewer.Manage）。
func (h *handlers) isManager(c *gin.Context) bool {
	for _, code := range managerButtons {
		if h.s.HasButton(c, code) {
			return true
		}
	}
	return false
}

// openGuard 是问题工单 / 榜单页面与接口的守卫：拥有 perf.view 或任一管理类按钮；page 为 true 时无权限渲染 403 页，否则返回 JSON 403。
func (h *handlers) openGuard(page bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.s.HasButton(c, btnView) || h.isManager(c) {
			c.Next()
			return
		}
		if page {
			h.s.Forbidden(c, "没有权限访问该页面。")
			c.Abort()
			return
		}
		server.APIFail(c, http.StatusForbidden, "forbidden", "没有权限执行该操作")
		c.Abort()
	}
}

// actionButtons 是工单操作对应的按钮（登记表 perf.*）。没有列出的操作由服务层自己拒绝。
var actionButtons = map[string]string{
	ActAccept: "perf.handle", ActFix: "perf.handle", ActExtend: "perf.handle", ActWaive: "perf.handle",
	ActComment: "perf.comment", ActAssign: "perf.assign", ActVerifyPass: "perf.verify", ActVerifyFail: "perf.verify",
	ActExtendApprove: "perf.approve", ActExtendReject: "perf.approve", ActWaiveApprove: "perf.approve", ActWaiveReject: "perf.approve",
	ActSeverity: "perf.adjust", ActSide: "perf.adjust", ActCloseDup: "perf.close_dup", ActReopen: "perf.reopen",
	ActDiagnose: "perf.reopen", // 重新诊断走独立接口，同样要 perf.reopen
}

func (h *handlers) viewer(c *gin.Context) (Viewer, error) {
	a := server.ActorOf(c)
	v := Viewer{Actor: a, Manage: h.isManager(c), Groups: map[uint]bool{}}
	// 超级用户也读自己所在的资源组：列表的“只看我的团队”范围要用（超级用户的可见范围本身不受它限制）
	ids, err := h.repo.UserGroupIDs(a.ID)
	if err != nil {
		if !a.IsSuperuser {
			return v, err
		}
		ids = nil
	}
	for _, id := range ids {
		v.Groups[id] = true
	}
	return v, nil
}

// ---- 响应 ----

func writeErr(c *gin.Context, err error) {
	var qe *query.Error
	var ex *ExistsError
	switch {
	case errors.As(err, &qe):
		c.AbortWithStatusJSON(qe.HTTP, qe)
	case errors.As(err, &ex):
		e := query.NewError(http.StatusConflict, "exists", ex.Error())
		e.Data = map[string]any{"id": ex.ID}
		c.AbortWithStatusJSON(e.HTTP, e)
	case errors.Is(err, ErrDuplicate):
		server.APIFail(c, http.StatusConflict, "conflict", "记录已存在")
	case errors.Is(err, ErrConflict):
		server.APIFail(c, http.StatusConflict, "conflict", "数据刚刚被他人更新，请刷新后重试")
	default:
		slog.Error("性能工单请求失败", "path", c.Request.URL.Path, "err", err)
		server.APIFail(c, http.StatusInternalServerError, "internal", "服务异常，请稍后再试")
	}
}

func uintParam(c *gin.Context, name string) (uint, bool) {
	n, err := strconv.ParseUint(c.Param(name), 10, 32)
	if err != nil || n == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数非法")
		return 0, false
	}
	return uint(n), true
}

func intQuery(c *gin.Context, name string, def int) int {
	if v := c.Query(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// bindJSON 解析请求体（限制 2MB）。
func bindJSON(c *gin.Context, out any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
	if err := c.ShouldBindJSON(out); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return false
	}
	return true
}

// ---- 页面 ----

type sideLink struct {
	Href, Label string
	On          bool
}

type pageInfo struct {
	ID        uint
	IsDBA     bool
	CanAdmin  bool       // perfcfg.view：能看配置类入口
	CanCreate bool       // perf.create：能手工建单
	Side      []sideLink // 二级侧栏（与侧栏“性能治理”分组一致，配置类入口只给有配置权限的人）
}

func (h *handlers) render(c *gin.Context, page, title, nav string, d any) {
	h.s.Render(c, http.StatusOK, "perf/"+page, server.PageData{Title: title, Nav: nav, Data: d})
}

func (h *handlers) pageInfo(c *gin.Context, active string) pageInfo {
	dba := h.isManager(c)
	p := pageInfo{IsDBA: dba, CanAdmin: h.s.HasButton(c, btnCfgView), CanCreate: h.s.HasButton(c, btnCreate)}
	for _, it := range []struct {
		key, href, label string
		admin            bool
	}{{"perf-list", "/perf", "问题工单", false}, {"perf-board", "/perf/board", "团队榜单", false}, {"perf-collect", "/perf/config/collect", "采集配置", true},
		{"perf-team", "/perf/config/team", "团队配置", true}, {"perf-sla", "/perf/config/sla", "SLA 策略", true}} {
		if !it.admin || p.CanAdmin {
			p.Side = append(p.Side, sideLink{Href: it.href, Label: it.label, On: it.key == active})
		}
	}
	return p
}

func (h *handlers) pageList(c *gin.Context) {
	h.render(c, "list.html", "问题工单", "perf-list", h.pageInfo(c, "perf-list"))
}
func (h *handlers) pageBoard(c *gin.Context) {
	h.render(c, "board.html", "团队榜单", "perf-board", h.pageInfo(c, "perf-board"))
}
func (h *handlers) pageCollect(c *gin.Context) {
	h.render(c, "collect.html", "采集配置", "perf-collect", h.pageInfo(c, "perf-collect"))
}
func (h *handlers) pageTeam(c *gin.Context) {
	h.render(c, "team.html", "团队配置", "perf-team", h.pageInfo(c, "perf-team"))
}
func (h *handlers) pageSLA(c *gin.Context) {
	h.render(c, "sla.html", "SLA 策略", "perf-sla", h.pageInfo(c, "perf-sla"))
}

func (h *handlers) pageDetail(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		h.s.Render(c, http.StatusNotFound, "error.html", server.PageData{Title: "工单不存在", Error: "工单编号不正确。"})
		return
	}
	d := h.pageInfo(c, "perf-list")
	d.ID = uint(id)
	h.render(c, "detail.html", fmt.Sprintf("性能工单 #%d", id), "perf-list", d) // 标题在 server.render 里按目录翻译（含 %d 格式串）
}

// ---- 工单 ----

// apiMyTodo 顶栏“性能工单”菜单上的待办角标：我的待办数与其中已超时的数。很轻（两个计数），页面加载时由 JS 调用。
func (h *handlers) apiMyTodo(c *gin.Context) {
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	t, err := h.svc.MyTodo(v)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, t)
}

// OptionsDTO 是筛选与弹窗用的选项。
type OptionsDTO struct {
	Severities      []Option        `json:"severities"`
	Statuses        []Option        `json:"statuses"`
	Categories      []Option        `json:"categories"`
	Sources         []Option        `json:"sources"`
	OwnerSides      []Option        `json:"owner_sides"`
	Teams           []GroupBrief    `json:"teams"`
	Instances       []InstanceBrief `json:"instances"` // 仅 DBA：可建单的实例
	Users           []UserBrief     `json:"users"`     // 仅 DBA：可指派的用户
	IsDBA           bool            `json:"is_dba"`
	FilterInstances []InstanceBrief `json:"filter_instances"`
}

func (h *handlers) apiOptions(c *gin.Context) {
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	out := OptionsDTO{Categories: Categories, Sources: IssueSources, IsDBA: v.IsDBA(), Teams: []GroupBrief{}, Instances: []InstanceBrief{}, Users: []UserBrief{}}
	for _, s := range Severities {
		out.Severities = append(out.Severities, Option{s, severityLabel[s]})
	}
	for _, s := range []string{StNew, StAssigned, StAccepted, StFixed, StWaivePending, StClosed} {
		out.Statuses = append(out.Statuses, Option{s, statusLabel[s]})
	}
	out.OwnerSides = []Option{{SideDev, sideLabel[SideDev]}, {SideDBA, sideLabel[SideDBA]}}
	groups, err := h.repo.ListGroups()
	if err != nil {
		writeErr(c, err)
		return
	}
	for _, g := range groups {
		if v.IsDBA() || v.Groups[g.ID] {
			g.Webhooks = nil
			out.Teams = append(out.Teams, g)
		}
	}
	if fi, err := h.filterInstances(v); err != nil {
		writeErr(c, err)
		return
	} else {
		out.FilterInstances = fi
	}
	if v.IsDBA() {
		all, err := h.repo.ListInstances(SupportedDBTypes)
		if err != nil {
			writeErr(c, err)
			return
		}
		if v.IsSuperuser || h.s.HasButton(c, permQueryAll) {
			out.Instances = all
		} else if mine, err := h.repo.UserInstanceIDs(v.ID); err == nil {
			for _, i := range all {
				if mine[i.ID] {
					out.Instances = append(out.Instances, i)
				}
			}
		}
		if out.Users, err = h.repo.ListActiveUsers(); err != nil {
			writeErr(c, err)
			return
		}
	}
	server.APIOK(c, out)
}

// filterInstances 列表“实例”筛选的选项：DBA 看全部；其他人只看自己所在资源组的实例（与工单可见范围一致）。
func (h *handlers) filterInstances(v Viewer) ([]InstanceBrief, error) {
	all, err := h.repo.ListInstances(SupportedDBTypes)
	if err != nil {
		return nil, err
	}
	out := []InstanceBrief{}
	for _, in := range all {
		if !v.IsDBA() {
			gs, err := h.repo.InstanceGroupIDs(in.ID)
			if err != nil {
				return nil, err
			}
			hit := false
			for _, g := range gs {
				hit = hit || v.Groups[g]
			}
			if !hit {
				continue
			}
		}
		out = append(out, in)
		if len(out) >= 500 {
			break
		}
	}
	return out, nil
}

func (h *handlers) apiList(c *gin.Context) {
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	f := ListFilter{View: c.Query("view"), Severity: c.Query("severity"), OwnerSide: c.Query("owner_side"), Category: c.Query("category"),
		TeamID: uint(intQuery(c, "team_id", 0)), InstanceID: uint(intQuery(c, "instance_id", 0)), Search: c.Query("search"),
		Sort: c.Query("sort"), Desc: c.Query("order") != "asc", Offset: intQuery(c, "offset", 0), Limit: intQuery(c, "limit", 20),
		Status: c.Query("status"), Source: c.Query("source"), Scope: c.Query("scope")}
	res, err := h.svc.ListIssues(v, f)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res.Rows, "total": res.Total, "summary": listSummary(res)})
}

func (h *handlers) apiDetail(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	d, err := h.svc.GetIssue(v, id)
	if err != nil {
		writeErr(c, err)
		return
	}
	// 页面上的操作按钮与接口的按钮校验一致
	d.Actions = filterActionsByButton(d.Actions, func(code string) bool { return h.s.HasButton(c, code) })
	server.APIOK(c, d)
}

func (h *handlers) apiAct(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in ActionInput
	if !bindJSON(c, &in) {
		return
	}
	if code, ok := actionButtons[in.Action]; ok && !h.s.ButtonOrFail(c, code) {
		return
	}
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	res, err := h.svc.Act(v, id, in)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) apiTrend(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	t, err := h.svc.IssueTrend(v, id, intQuery(c, "days", 30))
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, t)
}

func (h *handlers) apiCreate(c *gin.Context) {
	var in CreateInput
	if !bindJSON(c, &in) {
		return
	}
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	i, err := h.svc.CreateIssue(c.Request.Context(), v, h.s.HasButton(c, permQueryAll), in, nil, "manual", true)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"id": i.ID, "severity": i.Severity, "status": i.Status})
}

func (h *handlers) apiCreateFromSlowLog(c *gin.Context) {
	var in SlowLogInput
	if !bindJSON(c, &in) {
		return
	}
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	i, err := h.svc.CreateFromSlowLog(c.Request.Context(), v, h.s.HasButton(c, permQueryAll), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"id": i.ID, "severity": i.Severity, "status": i.Status, "impact_score": i.ImpactScore})
}

func (h *handlers) apiDiagnose(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in struct {
		SQL string `json:"sql"`
	}
	if c.Request.ContentLength != 0 && !bindJSON(c, &in) {
		return
	}
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	res, err := h.svc.Rediagnose(ctx, v, id, in.SQL)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) apiBoard(c *gin.Context) {
	side := c.DefaultQuery("owner_side", "dev")
	res, err := h.svc.Board(intQuery(c, "days", 7), strings.TrimSpace(side))
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, res)
}

// ---- 配置 ----

func (h *handlers) apiCollectList(c *gin.Context) {
	o, err := h.svc.CollectOverview()
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, o)
}

func (h *handlers) apiCollectSave(c *gin.Context) {
	var in CollectConfigInput
	if !bindJSON(c, &in) {
		return
	}
	if c.Request.Method == http.MethodPut {
		id, ok := uintParam(c, "id")
		if !ok {
			return
		}
		in.ID = id
	} else {
		in.ID = 0
	}
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	cfg, err := h.svc.SaveCollectConfig(v, in)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"id": cfg.ID})
}

func (h *handlers) apiCollectDelete(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	if err := h.svc.DeleteCollectConfig(v, id); err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiCollectPreview(c *gin.Context) {
	var in CollectConfigInput
	if !bindJSON(c, &in) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	res, err := h.svc.PreviewCollect(ctx, in)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) apiCollectInit(c *gin.Context) {
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	res, err := h.svc.InitCollectConfigs(v)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) apiTeamList(c *gin.Context) {
	o, err := h.svc.TeamOverview()
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, o)
}

func (h *handlers) apiTeamSave(c *gin.Context) {
	var in TeamInput
	if !bindJSON(c, &in) {
		return
	}
	if c.Request.Method == http.MethodPut {
		id, ok := uintParam(c, "id")
		if !ok {
			return
		}
		in.ID = id
	} else {
		in.ID = 0
	}
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	t, err := h.svc.SaveTeam(v, in)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"id": t.ID})
}

func (h *handlers) apiTeamDelete(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	if err := h.svc.DeleteTeam(v, id); err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiRouteSave(c *gin.Context) {
	var in RouteInput
	if !bindJSON(c, &in) {
		return
	}
	if c.Request.Method == http.MethodPut {
		id, ok := uintParam(c, "id")
		if !ok {
			return
		}
		in.ID = id
	} else {
		in.ID = 0
	}
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	r, err := h.svc.SaveRoute(v, in)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"id": r.ID})
}

func (h *handlers) apiRouteDelete(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	if err := h.svc.DeleteRoute(v, id); err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiSLAGet(c *gin.Context) {
	o, err := h.svc.SLAOverview()
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, o)
}

func (h *handlers) apiSLASave(c *gin.Context) {
	var in struct {
		Policies   []SLAPolicy `json:"policies"`
		Escalation EscMatrix   `json:"escalation"` // 可选：升级通知对象矩阵
	}
	if !bindJSON(c, &in) {
		return
	}
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	if in.Escalation != nil { // 先校验矩阵：不合法时时限也不保存
		if err := validateEscMatrix(in.Escalation); err != nil {
			writeErr(c, err)
			return
		}
	}
	if err := h.svc.SaveSLA(v, in.Policies); err != nil {
		writeErr(c, err)
		return
	}
	if in.Escalation != nil {
		if err := h.svc.SaveEscalation(v, in.Escalation); err != nil {
			writeErr(c, err)
			return
		}
	}
	server.APIOK(c, gin.H{"ok": true})
}
