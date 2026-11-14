// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package archive

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/i18n"
	"dbtool/internal/notify"
	"dbtool/internal/query"
	"dbtool/internal/server"
)

// menuKey / 页面常量。
const (
	menuKey  = "archive"
	pagePath = "/archive"
)

var (
	svcMu    = make(chan struct{}, 1)
	activeBy = map[*server.Server]*Service{}
)

func setService(s *server.Server, svc *Service) {
	svcMu <- struct{}{}
	activeBy[s] = svc
	<-svcMu
}

func serviceOf(s *server.Server) *Service {
	svcMu <- struct{}{}
	defer func() { <-svcMu }()
	return activeBy[s]
}

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, mirage, ok := s.DB()
		if !ok {
			return
		}
		repo := newGormRepo(db, mirage)
		var n notify.Notifier = notify.Noop{}
		if src, ok := s.Repo().(notify.Source); ok {
			if dir, ok := s.Repo().(notify.Directory); ok {
				n = notify.NewDispatcher(src, dir)
			}
		}
		secret := s.Config().MirageSecretKey
		svc := NewService(repo, s.Repo(), query.NewPrivAuthorizer(s.Repo(), nil), Options{
			Notifier: n, BaseURL: s.Config().PublicURL,
			Disk: func(ctx context.Context, id uint) (float64, bool, error) {
				return repo.inspectDiskFreePct(ctx, id, time.Now())
			},
			FileKey: func() ([]byte, error) {
				if secret == "" {
					return nil, errors.New("未配置 MIRAGE_SECRET_KEY / SECRET_KEY，无法加密备份文件")
				}
				sum := sha256.Sum256([]byte("dba-tool/archive/file-key/v1:" + secret))
				return sum[:], nil
			},
		})
		register(s, rt, svc)
		// 表没建时 Worker 什么也不做（每轮检查，执行 SQL 脚本后自动开始），所以总是登记。
		server.AddWorker(svc.Worker)
	})
	server.AddMenu(server.MenuItem{Group: "query", Key: menuKey, Href: pagePath, Label: "数据归档", Order: 30,
		Show: func(s *server.Server, a audit.Actor) bool { return s.ActorHasButton(a, "archive.view") }})
	// 首页“我的待办 / 待我审批”：数据归档待审批（没有“查看数据归档”按钮权限的人不显示）
	server.AddHomeApprove(func(s *server.Server, a audit.Actor, limit int) ([]server.HomeTodoRow, int, error) {
		svc := serviceOf(s)
		if svc == nil || !s.ActorHasButton(a, "archive.view") {
			return nil, 0, nil
		}
		items, total, err := svc.PendingReviewList(a, limit)
		if err != nil {
			return nil, 0, err
		}
		rows := make([]server.HomeTodoRow, 0, len(items))
		for _, it := range items {
			rows = append(rows, server.HomeTodoRow{Kind: "数据归档", Title: it.Name, Sub: it.Sub, Href: fmt.Sprintf("%s#task/%d", pagePath, it.ID)})
		}
		return rows, total, nil
	})
	server.AddNavBadge(menuKey, "等待我审批的数据归档申请 %d 张", func(s *server.Server, a audit.Actor) (int, bool, error) {
		svc := serviceOf(s)
		if svc == nil {
			return 0, false, nil
		}
		n, err := svc.PendingReview(a)
		return n, false, err
	})
}

type handlers struct {
	s   *server.Server
	svc *Service
}

// register 登记路由（测试里传入假依赖的 Service）。
func register(s *server.Server, rt server.Routes, svc *Service) {
	setService(s, svc)
	h := &handlers{s: s, svc: svc}
	rt.Pages.GET(pagePath, s.RequireButtonPage("archive.view"), h.page)

	view := s.RequireButtonAPI("archive.view")
	g := rt.API.Group("/archive")
	g.GET("/meta", view, h.meta)
	g.GET("/sources/instances", view, h.srcInstances)
	g.GET("/sources/databases", view, h.srcDatabases)
	g.GET("/sources/tables", view, h.srcTables)
	g.GET("/sources/columns", view, h.srcColumns)
	g.GET("/sources/time-probe", view, h.srcTimeProbe)
	g.GET("/flow", view, h.flow)

	g.GET("/tasks", view, h.listTasks)
	g.GET("/tasks/:id", view, h.getTask)
	g.POST("/tasks", s.RequireButtonAPI("archive.apply"), h.createTask)
	g.PUT("/tasks/:id", s.RequireButtonAPI("archive.apply"), h.updateTask)
	g.POST("/tasks/:id/submit", s.RequireButtonAPI("archive.apply"), h.submitTask)
	g.POST("/tasks/:id/approve", view, h.approve)
	g.POST("/tasks/:id/reject", view, h.reject)
	g.POST("/tasks/:id/pause", s.RequireButtonAPI("archive.pause"), h.pauseTask)
	g.POST("/tasks/:id/resume", s.RequireButtonAPI("archive.pause"), h.resumeTask)
	g.POST("/tasks/:id/run", s.RequireButtonAPI("archive.run"), h.runTask)
	g.POST("/tasks/:id/disable", s.RequireButtonAPI("archive.delete"), h.disableTask)
	g.DELETE("/tasks/:id", s.RequireButtonAPI("archive.delete"), h.deleteTask)
	g.POST("/preview", view, h.preview)
	g.POST("/check", s.RequireButtonAPI("archive.dryrun"), h.check)

	g.GET("/runs", view, h.listRuns)
	g.GET("/runs/:id", view, h.getRun)
	g.GET("/runs/:id/events", view, h.runEvents)
	g.GET("/runs/:id/manifest", view, h.runManifest)
	g.POST("/runs/:id/pause", s.RequireButtonAPI("archive.pause"), h.pauseRun)
	g.POST("/runs/:id/resume", s.RequireButtonAPI("archive.pause"), h.resumeRun)
	g.POST("/runs/:id/stop", s.RequireButtonAPI("archive.stop"), h.stopRun)
	g.POST("/runs/:id/restore", s.RequireButtonAPI("archive.restore"), h.restoreRun)
	g.GET("/progress", view, h.progress)

	cfgView := s.RequireButtonAPI("archive.cfg_view")
	storeEdit := s.RequireButtonAPI("archive.store_edit")
	g.GET("/stores", s.RequireButtonAPI("archive.cfg_view", "archive.apply", "archive.dryrun"), h.listStores)
	g.POST("/stores", storeEdit, h.createStore)
	g.POST("/stores/test", storeEdit, h.testStoreDraft)
	g.PUT("/stores/:id", storeEdit, h.updateStore)
	g.POST("/stores/:id/test", storeEdit, h.testStore)
	g.POST("/stores/:id/enable", storeEdit, h.enableStore)
	g.DELETE("/stores/:id", storeEdit, h.deleteStore)
	g.GET("/policy", cfgView, h.getPolicy)
	g.POST("/policy/diff", cfgView, h.diffPolicy)
	g.PUT("/policy", s.RequireButtonAPI("archive.policy_edit"), h.savePolicy)
}

// reqLang 返回本次请求的界面语言（中间件写进 request.Context；没有时是系统默认语言）。
func reqLang(c *gin.Context) string { return i18n.FromContext(c.Request.Context()) }

func (h *handlers) caller(c *gin.Context) Caller {
	return Caller{Principal: server.PrincipalOf(c), Lang: reqLang(c),
		Can: func(code string) bool { return h.s.HasButton(c, code) }}
}

// fail 把业务错误映射成 HTTP 响应；未知错误只记日志，对外统一成 500。
func fail(c *gin.Context, err error) {
	var pe *Error
	if errors.As(err, &pe) {
		if pe.Data != nil {
			c.AbortWithStatusJSON(pe.Status, gin.H{"code": pe.Code, "message": pe.Message, "data": localize(reqLang(c), pe.Data)})
			return
		}
		server.APIFail(c, pe.Status, pe.Code, pe.Message)
		return
	}
	var ae *audit.Error
	if errors.As(err, &ae) {
		fail(c, translateAudit(err, ""))
		return
	}
	slog.Error("数据归档请求失败", "path", c.Request.URL.Path, "err", err)
	server.APIFail(c, http.StatusInternalServerError, "internal", "服务内部错误，请稍后重试或联系管理员")
}

func idParam(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "编号不合法")
		return 0, false
	}
	return uint(n), true
}

func uintQuery(c *gin.Context, key string) uint {
	n, _ := strconv.ParseUint(c.Query(key), 10, 32)
	return uint(n)
}

func intQuery(c *gin.Context, key string) int {
	n, _ := strconv.Atoi(c.Query(key))
	return n
}

// bind 解析 JSON 请求体；allowEmpty 为 true 时空体不算错。
func bind(c *gin.Context, v any, allowEmpty bool) bool {
	if c.Request.ContentLength == 0 && allowEmpty {
		return true
	}
	if err := c.ShouldBindJSON(v); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return false
	}
	return true
}

func ok(c *gin.Context, v any, err error) {
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, localize(reqLang(c), v))
}

func (h *handlers) page(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "archive/index.html", server.PageData{Title: "数据归档", Nav: menuKey})
}

func (h *handlers) meta(c *gin.Context) {
	server.APIOK(c, localize(reqLang(c), h.svc.Meta(h.caller(c))))
}

func (h *handlers) srcInstances(c *gin.Context) {
	v, err := h.svc.SourceInstances(h.caller(c))
	ok(c, v, err)
}

func (h *handlers) srcDatabases(c *gin.Context) {
	v, err := h.svc.SourceDatabases(c.Request.Context(), h.caller(c), uintQuery(c, "instance_id"))
	ok(c, v, err)
}

func (h *handlers) srcTables(c *gin.Context) {
	v, err := h.svc.SourceTables(c.Request.Context(), h.caller(c), uintQuery(c, "instance_id"), c.Query("db"))
	ok(c, v, err)
}

func (h *handlers) srcColumns(c *gin.Context) {
	v, err := h.svc.SourceColumns(c.Request.Context(), h.caller(c), uintQuery(c, "instance_id"), c.Query("db"), c.Query("table"))
	ok(c, v, err)
}

func (h *handlers) srcTimeProbe(c *gin.Context) {
	v, err := h.svc.SourceTimeProbe(c.Request.Context(), h.caller(c), uintQuery(c, "instance_id"), c.Query("db"), c.Query("table"), c.Query("column"))
	ok(c, v, err)
}

func (h *handlers) flow(c *gin.Context) {
	v, err := h.svc.Flow(h.caller(c), uintQuery(c, "resource_group_id"))
	ok(c, v, err)
}

func (h *handlers) listTasks(c *gin.Context) {
	q := TaskListQuery{Status: c.Query("status"), Mode: c.Query("mode"), InstanceID: uintQuery(c, "instance_id"), Q: c.Query("q"),
		Mine: c.Query("mine") == "1", Offset: intQuery(c, "offset"), Limit: intQuery(c, "limit")}
	v, err := h.svc.ListTasks(h.caller(c), q)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, localize(reqLang(c), gin.H{"data": v.Data, "total": v.Total, "kpi": v.KPI, "instances": v.Instances}))
}

func (h *handlers) getTask(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	v, err := h.svc.GetTask(h.caller(c), id)
	ok(c, v, err)
}

func (h *handlers) taskResult(c *gin.Context, r *TaskResult, err error) {
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, localize(reqLang(c), gin.H{"data": r.Task, "outcome": r.Outcome, "changes": r.Changes}))
}

func (h *handlers) createTask(c *gin.Context) {
	var in TaskInput
	if !bind(c, &in, false) {
		return
	}
	r, err := h.svc.CreateTask(h.caller(c), in)
	h.taskResult(c, r, err)
}

func (h *handlers) updateTask(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	var in TaskInput
	if !bind(c, &in, false) {
		return
	}
	r, err := h.svc.UpdateTask(h.caller(c), id, in)
	h.taskResult(c, r, err)
}

type submitReq struct {
	CheckToken string `json:"check_token"`
	AckWarn    bool   `json:"ack_warn"`
	AckBig     bool   `json:"ack_big"`
}

func (h *handlers) submitTask(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	var req submitReq
	if !bind(c, &req, true) {
		return
	}
	r, err := h.svc.SubmitTask(h.caller(c), id, req.CheckToken, req.AckWarn, req.AckBig)
	h.taskResult(c, r, err)
}

type remarkReq struct {
	Remark string `json:"remark"`
	Reason string `json:"reason"`
}

func (h *handlers) approve(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	var req remarkReq
	if !bind(c, &req, true) {
		return
	}
	v, err := h.svc.Approve(h.caller(c), id, req.Remark)
	ok(c, v, err)
}

func (h *handlers) reject(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	var req remarkReq
	if !bind(c, &req, true) {
		return
	}
	v, err := h.svc.Reject(h.caller(c), id, req.Reason)
	ok(c, v, err)
}

func (h *handlers) pauseTask(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	var req remarkReq
	if !bind(c, &req, true) {
		return
	}
	v, err := h.svc.PauseTask(h.caller(c), id, req.Reason)
	ok(c, v, err)
}

func (h *handlers) resumeTask(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	v, err := h.svc.ResumeTask(h.caller(c), id)
	ok(c, v, err)
}

type confirmReq struct {
	Confirm      bool   `json:"confirm"`
	ConfirmName  string `json:"confirm_name"`
	IgnoreWindow bool   `json:"ignore_window"`
}

func (h *handlers) runTask(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	var req confirmReq
	if !bind(c, &req, true) {
		return
	}
	v, err := h.svc.RunNow(h.caller(c), id, req.IgnoreWindow, req.Confirm)
	ok(c, v, err)
}

func (h *handlers) disableTask(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	var req confirmReq
	if !bind(c, &req, true) {
		return
	}
	v, err := h.svc.DisableTask(h.caller(c), id, req.Confirm)
	ok(c, v, err)
}

func (h *handlers) deleteTask(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	name := c.Query("confirm_name")
	if name == "" && c.Request.ContentLength > 0 {
		var req confirmReq
		if !bind(c, &req, true) {
			return
		}
		name = req.ConfirmName
	}
	if err := h.svc.DeleteTask(h.caller(c), id, name); err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) preview(c *gin.Context) {
	var in TaskInput
	if !bind(c, &in, false) {
		return
	}
	v, err := h.svc.Preview(h.caller(c), in)
	ok(c, v, err)
}

func (h *handlers) check(c *gin.Context) {
	var in TaskInput
	if !bind(c, &in, false) {
		return
	}
	v, err := h.svc.Check(c.Request.Context(), h.caller(c), in)
	ok(c, v, err)
}

func (h *handlers) listRuns(c *gin.Context) {
	q := RunQuery{TaskID: uintQuery(c, "task_id"), Trigger: c.Query("trigger"), Result: c.Query("result"), Q: c.Query("q"), Offset: intQuery(c, "offset"), Limit: intQuery(c, "limit")}
	rows, total, err := h.svc.ListRuns(h.caller(c), q)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, localize(reqLang(c), gin.H{"data": rows, "total": total}))
}

func (h *handlers) getRun(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	v, err := h.svc.GetRun(h.caller(c), id)
	ok(c, v, err)
}

func (h *handlers) runEvents(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	evs, last, err := h.svc.RunEvents(h.caller(c), id, uintQuery(c, "after_seq"), intQuery(c, "limit"))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, localize(reqLang(c), gin.H{"data": evs, "last_seq": last}))
}

func (h *handlers) runManifest(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	b, err := h.svc.RunManifest(h.caller(c), id)
	if err != nil {
		fail(c, err)
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="manifest-%d.json"`, id))
	c.Data(http.StatusOK, "application/json; charset=utf-8", b)
}

func (h *handlers) pauseRun(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	v, err := h.svc.PauseRun(h.caller(c), id)
	ok(c, v, err)
}

func (h *handlers) resumeRun(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	v, err := h.svc.ResumeRun(h.caller(c), id)
	ok(c, v, err)
}

func (h *handlers) stopRun(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	var req confirmReq
	if !bind(c, &req, true) {
		return
	}
	v, err := h.svc.StopRun(h.caller(c), id, req.Confirm)
	ok(c, v, err)
}

func (h *handlers) restoreRun(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	var req confirmReq
	if !bind(c, &req, true) {
		return
	}
	v, err := h.svc.Restore(h.caller(c), id, req.ConfirmName)
	ok(c, v, err)
}

func (h *handlers) progress(c *gin.Context) {
	v, err := h.svc.Progress(h.caller(c), uintQuery(c, "task_id"))
	ok(c, v, err)
}

func (h *handlers) listStores(c *gin.Context) {
	cl := h.caller(c)
	if c.Query("for") == "picker" || !cl.has("archive.cfg_view") {
		v, err := h.svc.StorePickers(cl)
		ok(c, v, err)
		return
	}
	v, err := h.svc.ListStores(cl)
	ok(c, v, err)
}

func (h *handlers) createStore(c *gin.Context) {
	var in StoreInput
	if !bind(c, &in, false) {
		return
	}
	v, err := h.svc.CreateStore(h.caller(c), in)
	ok(c, v, err)
}

func (h *handlers) updateStore(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	var in StoreInput
	if !bind(c, &in, false) {
		return
	}
	v, err := h.svc.UpdateStore(h.caller(c), id, in)
	ok(c, v, err)
}

func (h *handlers) testStore(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	v, err := h.svc.TestStore(c.Request.Context(), h.caller(c), id)
	ok(c, v, err)
}

func (h *handlers) testStoreDraft(c *gin.Context) {
	var in StoreInput
	if !bind(c, &in, false) {
		return
	}
	v, err := h.svc.TestStoreDraft(c.Request.Context(), h.caller(c), in)
	ok(c, v, err)
}

type enableReq struct {
	Enable *bool `json:"enable"`
}

func (h *handlers) enableStore(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	var req enableReq
	if !bind(c, &req, false) || req.Enable == nil {
		if req.Enable == nil && !c.IsAborted() {
			server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误：需要 enable 字段")
		}
		return
	}
	v, err := h.svc.EnableStore(h.caller(c), id, *req.Enable)
	ok(c, v, err)
}

func (h *handlers) deleteStore(c *gin.Context) {
	id, good := idParam(c)
	if !good {
		return
	}
	if err := h.svc.DeleteStore(h.caller(c), id); err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) getPolicy(c *gin.Context) {
	v, err := h.svc.GetPolicy(h.caller(c))
	ok(c, v, err)
}

func (h *handlers) diffPolicy(c *gin.Context) {
	var in Policy
	if !bind(c, &in, false) {
		return
	}
	v, err := h.svc.DiffPolicy(h.caller(c), in)
	ok(c, v, err)
}

type policyReq struct {
	Policy
	Confirm bool `json:"confirm"`
}

func (h *handlers) savePolicy(c *gin.Context) {
	var in policyReq
	if !bind(c, &in, false) {
		return
	}
	v, err := h.svc.SavePolicy(h.caller(c), in.Policy, in.Confirm)
	ok(c, v, err)
}

var _ = context.Background
