// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/i18n"
	"dbtool/internal/localize"
	"dbtool/internal/notify"
	"dbtool/internal/server"
)

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, mirage, ok := s.DB() // 仓库不是 store.Store（测试 / 演示）时没有元数据库，路由即 404
		if !ok {
			return
		}
		st := newStore(db, mirage)
		st.connFn = s.Repo().EngineConn
		svc := NewService(st, Options{Notifier: newDispatchNotifier(s), BaseURL: s.Config().PublicURL,
			OpenRedis: OpenRedis, RedisBigKeys: redisBigKeysFromEnv()})
		registerWith(svc, defaultHooks())(s, rt)
		server.AddWorker(svc.Worker)
	})
	server.AddMenu(server.MenuItem{Group: "inst", Key: "inst-inspect", Href: "/inspection", Label: "性能巡检", Order: 60,
		Show: func(s *server.Server, a audit.Actor) bool {
			if a.IsSuperuser {
				return true
			}
			return s.ActorHasButton(a, btnView)
		}})
}

// hooks 是 HTTP 层依赖的、可在测试里替换的部分。
type hooks struct {
	scope func(s *server.Server, c *gin.Context) (Scope, error) // 用户可见的实例范围
}

func defaultHooks() hooks {
	return hooks{
		// 实例可见范围：超级用户 / 持有“查看 / 查询全部实例”（query.admin_run）的看全部，其余按资源组关联的实例
		scope: func(s *server.Server, c *gin.Context) (Scope, error) {
			if s.HasButton(c, "query.admin_run") {
				return Scope{All: true}, nil
			}
			ids, err := s.Repo().UserInstanceIDs(server.PrincipalOf(c).ID)
			return Scope{IDs: ids}, err
		},
	}
}

type handlers struct {
	s   *server.Server
	svc *Service
	h   hooks
}

// registerWith 返回登记函数；测试里用它传入假服务。
func registerWith(svc *Service, h hooks) func(s *server.Server, rt server.Routes) {
	return func(s *server.Server, rt server.Routes) {
		hd := &handlers{s: s, svc: svc, h: h}
		menu := s.RequireButtonPage(btnView)
		rt.Pages.GET("/inspection", menu, hd.listPage)
		rt.Pages.GET("/inspection/:id", menu, hd.detailPage)

		apiMenu := s.RequireButtonAPI(btnView)
		btn := s.RequireButtonAPI // 新建 / 编辑、启停删除、立即运行是三个按钮
		g := rt.API.Group("/inspection")
		g.GET("/options", apiMenu, hd.apiOptions)
		g.GET("/reports", apiMenu, hd.apiReports)
		g.GET("/reports/:id", apiMenu, hd.apiReport)
		g.POST("/reports/run", apiMenu, s.RequireButtonAPI("inspect.run"), hd.apiRun)
		g.POST("/reports/cancel", apiMenu, hd.apiCancel)
		g.POST("/reports/delete", apiMenu, s.RequireButtonAPI("inspect.delete"), hd.apiDelete)
		g.GET("/plans", apiMenu, hd.apiPlans)
		g.POST("/plans", apiMenu, btn(btnPlanEdit), hd.apiSavePlan)
		g.POST("/plans/preview", apiMenu, btn(btnPlanEdit), hd.apiPreview)
		g.PUT("/plans/:id", apiMenu, btn(btnPlanEdit), hd.apiSavePlan)
		g.DELETE("/plans/:id", apiMenu, btn(btnPlanToggle), hd.apiDeletePlan)
		g.POST("/plans/:id/enable", apiMenu, btn(btnPlanToggle), hd.apiEnablePlan)
		g.POST("/plans/:id/run", apiMenu, btn(btnPlanRun), hd.apiRunPlan)
		g.GET("/plans/:id/runs", apiMenu, hd.apiPlanRuns)
	}
}

func fail(c *gin.Context, err error) {
	var e *Error
	if errors.As(err, &e) {
		server.APIFail(c, e.Status, e.Code, e.Message)
		return
	}
	slog.Error("性能巡检接口出错", "path", c.Request.URL.Path, "err", err)
	server.APIFail(c, http.StatusInternalServerError, "internal", "服务内部错误，请稍后重试")
}

// redisBigKeysFromEnv 读取 REDIS_INSPECT_BIGKEY_SAMPLE：定时巡检和立即巡检默认的 Redis 大 key 抽样数。默认 0（关闭）。
func redisBigKeysFromEnv() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("REDIS_INSPECT_BIGKEY_SAMPLE")))
	if err != nil || n < 0 {
		return 0
	}
	return min(n, redisBigKeyMax)
}

// lang 是本次请求的界面语言（中间件写进 request.Context）。
func lang(c *gin.Context) string { return i18n.FromContext(c.Request.Context()) }

// userKeys 是响应里原样展示的用户数据 / 标识字段：不参与翻译。
var userKeys = []string{"instance_name", "plan_name", "operator", "db"}

func (h *handlers) actor(c *gin.Context) Actor {
	p := server.PrincipalOf(c)
	return Actor{ID: p.ID, Username: p.Username, Display: p.Display, IsSuperuser: p.IsSuperuser}
}

func (h *handlers) scope(c *gin.Context) (Scope, bool) {
	sc, err := h.h.scope(h.s, c)
	if err != nil {
		fail(c, err)
		return Scope{}, false
	}
	return sc, true
}

// canCancel：超级用户或持有按钮 inspect.cancel。不再认 is_staff：自助注册的用户都带 is_staff，
// 取消超级用户也不会清掉它，认它等于绕过按钮授权和单独禁用。
func (h *handlers) canCancel(c *gin.Context) bool {
	return h.s.HasButton(c, "inspect.cancel")
}

// ---- 页面 ----

func (h *handlers) listPage(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "inspect/list.html", server.PageData{Title: "性能巡检", Nav: "inst-inspect"})
}

func (h *handlers) detailPage(c *gin.Context) {
	ref, err := ParseRef(c.Param("id"))
	if err != nil {
		c.String(http.StatusNotFound, i18n.T(lang(c), "巡检报告不存在"))
		return
	}
	h.s.Render(c, http.StatusOK, "inspect/detail.html", server.PageData{Title: "性能巡检报告", Nav: "inst-inspect", Data: gin.H{"Ref": ref.String()}})
}

// ---- 选项 ----

func (h *handlers) apiOptions(c *gin.Context) {
	sc, ok := h.scope(c)
	if !ok {
		return
	}
	insts, tags, err := h.svc.Instances(sc)
	if err != nil {
		fail(c, err)
		return
	}
	out := gin.H{
		"instances": insts, "tags": tags, "channels": h.svc.Channels(), "plans_ready": h.svc.PlansReady(),
		"can_run": h.s.HasButton(c, "inspect.run"), "can_delete": h.s.HasButton(c, "inspect.delete"), "can_cancel": h.canCancel(c),
		// 定时计划：新建 / 编辑、暂停 / 启用 / 删除、立即运行各是一个按钮；can_plan 是“能不能新建 / 编辑”（沿用旧字段名）
		"can_plan": h.s.HasButton(c, btnPlanEdit), "can_plan_toggle": h.s.HasButton(c, btnPlanToggle), "can_plan_run": h.s.HasButton(c, btnPlanRun),
	}
	server.APIOK(c, out)
}

// ---- 报告 ----

type reportRow struct {
	ReportSummary
	Ref       string `json:"ref"`
	TypeLabel string `json:"type_label"`
	Elapsed   int    `json:"elapsed"` // 秒
}

func (h *handlers) toRow(r ReportSummary) reportRow {
	end := h.svc.now()
	if r.FinishTime != nil {
		end = *r.FinishTime
	}
	el := int(end.Sub(r.StartTime).Seconds())
	if el < 0 {
		el = 0
	}
	return reportRow{ReportSummary: r, Ref: ReportRef{r.Kind, r.ID}.String(), TypeLabel: KindLabel(r.DBType), Elapsed: el}
}

func parseDate(s string) (*time.Time, error) {
	if s = strings.TrimSpace(s); s == "" {
		return nil, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return nil, badInput("日期格式应为 YYYY-MM-DD")
	}
	return &t, nil
}

func uintQuery(c *gin.Context, key string) uint {
	n, _ := strconv.ParseUint(c.Query(key), 10, 32)
	return uint(n)
}

func intQuery(c *gin.Context, key string, def int) int {
	if v := c.Query(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func (h *handlers) apiReports(c *gin.Context) {
	sc, ok := h.scope(c)
	if !ok {
		return
	}
	start, err := parseDate(c.Query("start_date"))
	if err != nil {
		fail(c, err)
		return
	}
	end, err := parseDate(c.Query("end_date"))
	if err != nil {
		fail(c, err)
		return
	}
	if end != nil {
		e := end.AddDate(0, 0, 1)
		end = &e
	}
	q := ReportQuery{InstanceID: uintQuery(c, "instance_id"), Level: c.Query("level"), Trigger: c.Query("trigger"), Search: c.Query("search"), Start: start, End: end,
		Running: c.Query("running") == "1", Latest: c.Query("latest") == "1", PlanID: uintQuery(c, "plan_id"), RunID: uintQuery(c, "run_id"), Offset: intQuery(c, "offset", 0), Limit: intQuery(c, "limit", 20)}
	list, total, err := h.svc.ListReports(sc, q)
	if err != nil {
		fail(c, err)
		return
	}
	rows := make([]reportRow, len(list))
	for i, r := range list {
		rows[i] = h.toRow(r)
	}
	c.JSON(http.StatusOK, gin.H{"data": localize.Value(lang(c), rows, userKeys...), "total": total})
}

func (h *handlers) apiReport(c *gin.Context) {
	sc, ok := h.scope(c)
	if !ok {
		return
	}
	ref, err := ParseRef(c.Param("id"))
	if err != nil {
		fail(c, errNotFound("巡检报告"))
		return
	}
	v, err := h.svc.GetReport(sc, ref)
	if err != nil {
		fail(c, err)
		return
	}
	// 检查项名称 / 建议 / 提示按请求语言翻译；明细表（table）留给前端按列名与结论取值，不在这里翻译。
	server.APIOK(c, localize.Value(lang(c), gin.H{"report": h.toRow(v.Report), "engine": v.Engine, "facts": v.Facts, "blocks": v.Blocks, "sections": v.Sections, "note": v.Note},
		append([]string{"table"}, userKeys...)...))
}

func (h *handlers) apiRun(c *gin.Context) {
	var req struct {
		InstanceIDs  []uint `json:"instance_ids"`
		RedisBigKeys bool   `json:"redis_bigkey"` // Redis 实例做大 key 抽样（默认关闭）
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, badInput("请求格式错误"))
		return
	}
	sc, ok := h.scope(c)
	if !ok {
		return
	}
	res, err := h.svc.RunNowWith(h.actor(c), sc, req.InstanceIDs, RunNowOptions{RedisBigKeys: req.RedisBigKeys})
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, res)
}

type refsReq struct {
	IDs []string `json:"ids"`
}

func (h *handlers) apiCancel(c *gin.Context) {
	if !h.canCancel(c) {
		server.APIFail(c, http.StatusForbidden, "forbidden", "仅超级用户或持有 inspect.cancel 按钮的用户可以取消巡检")
		return
	}
	var req refsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, badInput("请求格式错误"))
		return
	}
	refs, err := ParseRefs(req.IDs)
	if err != nil {
		fail(c, err)
		return
	}
	sc, ok := h.scope(c)
	if !ok {
		return
	}
	res, err := h.svc.Cancel(h.actor(c), sc, refs)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) apiDelete(c *gin.Context) {
	var req refsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, badInput("请求格式错误"))
		return
	}
	refs, err := ParseRefs(req.IDs)
	if err != nil {
		fail(c, err)
		return
	}
	sc, ok := h.scope(c)
	if !ok {
		return
	}
	res, err := h.svc.Delete(h.actor(c), sc, refs)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, res)
}

// ---- 计划 ----

func (h *handlers) apiPlans(c *gin.Context) {
	sc, ok := h.scope(c)
	if !ok {
		return
	}
	list, err := h.svc.ListPlans(sc)
	if err != nil {
		fail(c, err)
		return
	}
	if list == nil {
		list = []PlanView{}
	}
	for i := range list {
		list[i].Describe = list[i].Plan.DescribeLang(lang(c)) // 周期说明由语序决定，不能整句查表
	}
	server.APIOK(c, list)
}

func planID(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		fail(c, errNotFound("定时巡检计划"))
		return 0, false
	}
	return uint(n), true
}

func (h *handlers) apiSavePlan(c *gin.Context) {
	var id uint
	if c.Param("id") != "" {
		var ok bool
		if id, ok = planID(c); !ok {
			return
		}
	}
	var in Plan
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, badInput("请求格式错误"))
		return
	}
	sc, ok := h.scope(c)
	if !ok {
		return
	}
	saved, err := h.svc.SavePlan(h.actor(c), sc, id, in)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"id": saved.ID, "next_run_at": saved.NextRunAt})
}

func (h *handlers) apiPreview(c *gin.Context) {
	var in Plan
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, badInput("请求格式错误"))
		return
	}
	times, desc, err := h.svc.PreviewSchedule(in, 5)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"describe": desc, "next_runs": times})
}

func (h *handlers) apiDeletePlan(c *gin.Context) {
	id, ok := planID(c)
	if !ok {
		return
	}
	if err := h.svc.DeletePlan(h.actor(c), id); err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiEnablePlan(c *gin.Context) {
	id, ok := planID(c)
	if !ok {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, badInput("请求格式错误"))
		return
	}
	if err := h.svc.SetPlanEnabled(h.actor(c), id, req.Enabled); err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiRunPlan(c *gin.Context) {
	id, ok := planID(c)
	if !ok {
		return
	}
	runID, err := h.svc.RunPlanNow(h.actor(c), id)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"run_id": runID})
}

func (h *handlers) apiPlanRuns(c *gin.Context) {
	id, ok := planID(c)
	if !ok {
		return
	}
	runs, err := h.svc.ListRuns(id, intQuery(c, "limit", 30))
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, localize.Value(lang(c), runs, userKeys...))
}

// ---- 通知：复用 internal/notify 的渠道与 @ 规则 ----

type dispatchNotifier struct {
	adm  notify.Admin
	disp *notify.Dispatcher
}

// newDispatchNotifier 在仓库同时具备渠道来源、@ 对象目录与渠道管理能力时返回通知发送器，否则返回 nil（计划不能配置通知）。
func newDispatchNotifier(s *server.Server) Notifier {
	repo := s.Repo()
	src, ok1 := repo.(notify.Source)
	dir, ok2 := repo.(notify.Directory)
	adm, ok3 := repo.(notify.Admin)
	if !ok1 || !ok2 || !ok3 {
		return nil
	}
	return &dispatchNotifier{adm: adm, disp: notify.NewDispatcher(src, dir)}
}

func (n *dispatchNotifier) Channels() ([]ChannelOption, error) {
	if !n.adm.NotifyTableReady() {
		return nil, nil
	}
	views, err := n.adm.ListChannelViews()
	if err != nil {
		return nil, err
	}
	out := make([]ChannelOption, len(views))
	for i, v := range views {
		out[i] = ChannelOption{ID: v.ID, Name: v.Name, Type: v.Type, Label: notify.TypeLabel(v.Type), GroupID: v.GroupID, Enabled: v.Enabled}
	}
	return out, nil
}

func (n *dispatchNotifier) Send(ctx context.Context, ids []uint, ev notify.Event) (int, error) {
	sent := 0
	var last error
	for _, id := range ids {
		ch, err := n.adm.GetChannel(id)
		if err != nil {
			last = err
			continue
		}
		if ch == nil || !ch.Enabled {
			continue
		}
		e := ev
		e.GroupID = ch.GroupID // “某个权限组的成员”按渠道所属资源组解析
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = n.disp.SendTest(sctx, *ch, e) // 向这个渠道发送并按它的 @ 规则解析（规则里的事件过滤按 ev.Name）
		cancel()
		if err != nil {
			last = err
			continue
		}
		sent++
	}
	if sent == 0 {
		return 0, last
	}
	return sent, nil
}
