// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package giconfig

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/server"
)

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, mirage, ok := s.DB() // 仓库不是 store.Store（测试/演示）时没有元数据库，路由即 404
		if !ok {
			return
		}
		svc := NewService(newStore(db, mirage), Options{})
		registerWith(svc)(s, rt)
		// 退出时等后台的重启流程收尾，最多等 2 分钟，避免半途而废
		server.AddWorker(func(ctx context.Context) {
			<-ctx.Done()
			done := make(chan struct{})
			go func() { svc.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Minute):
				slog.Warn("goInception 重启流程在退出时仍未结束")
			}
		})
	})
	server.AddMenu(server.MenuItem{Group: "plat", Key: "sys-goinception", Href: "/settings/goinception", Label: "审核引擎", Order: 30,
		Show: func(s *server.Server, a audit.Actor) bool {
			_, _, ok := s.DB()
			return ok && server.SuperOnly(s, a)
		}})
}

type handlers struct {
	s   *server.Server
	svc *Service
}

// registerWith 用给定的服务登记路由（init 里装配真实依赖，测试里传假依赖）。
func registerWith(svc *Service) func(s *server.Server, rt server.Routes) {
	return func(s *server.Server, rt server.Routes) {
		h := &handlers{s: s, svc: svc}
		rt.Pages.GET("/settings/goinception", s.SuperuserPage(), h.page)
		g := rt.API.Group("/settings/goinception", s.SuperuserAPI())
		g.GET("", h.overview)
		g.GET("/status", h.status)
		g.PUT("/connection", h.saveConn)
		g.POST("/connection/test", h.testConn)
		g.POST("/file/validate", h.validate)
		g.POST("/file/params", h.params)
		g.POST("/file/patch", h.patch)
		g.POST("/file/diff", h.previewDiff)
		g.POST("/file/save", h.save)
		g.POST("/file/import", h.importDisk)
		g.GET("/versions/:v", h.version)
		g.GET("/versions/:v/diff", h.versionDiff)
		g.POST("/versions/:v/rollback", h.rollback)
		g.POST("/restart", h.restart)
		g.GET("/restart/status", h.restartStatus)
		g.PUT("/deploy", h.saveDeploy)
		g.POST("/deploy/preview", h.previewDeploy)
		g.POST("/deploy/check", h.checkDeploy)
	}
}

func (h *handlers) page(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "giconfig/index.html", server.PageData{Title: "审核引擎", Nav: "sys-goinception"})
}

// fail 把业务错误转成统一的 JSON 响应；带 Data 的错误（校验报告、任务列表）把它放进 data 字段。
func fail(c *gin.Context, err error) {
	var e *Error
	if errors.As(err, &e) {
		body := gin.H{"code": e.Code, "message": e.Message}
		if e.Data != nil {
			body["data"] = localizeData(langOf(c), e.Data)
		}
		c.AbortWithStatusJSON(e.Status, body)
		return
	}
	slog.Error("goInception 配置接口出错", "err", err)
	server.APIFail(c, http.StatusInternalServerError, "internal", "服务内部错误")
}

// bind 解析 JSON 请求体，限制 2 MB。
func bind(c *gin.Context, v any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
	if err := c.ShouldBindJSON(v); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误或内容过大")
		return false
	}
	return true
}

func versionParam(c *gin.Context) (int, bool) {
	n, err := strconv.Atoi(c.Param("v"))
	if err != nil || n <= 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "版本号不合法")
		return 0, false
	}
	return n, true
}

func (h *handlers) overview(c *gin.Context) {
	o, err := h.svc.Overview()
	if err != nil {
		fail(c, err)
		return
	}
	localizeOverview(langOf(c), o)
	server.APIOK(c, o)
}

func (h *handlers) status(c *gin.Context) {
	st, err := h.svc.Status(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	localizeStatus(langOf(c), st)
	server.APIOK(c, st)
}

func (h *handlers) saveConn(c *gin.Context) {
	var in ConnInput
	if !bind(c, &in) {
		return
	}
	if err := h.svc.SaveConn(server.ActorOf(c), in); err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) testConn(c *gin.Context) {
	var in ConnInput
	if !bind(c, &in) {
		return
	}
	res, err := h.svc.TestConn(c.Request.Context(), in)
	if err != nil {
		fail(c, err)
		return
	}
	res.Lines = tpLines(langOf(c), res.Lines)
	server.APIOK(c, res)
}

type contentReq struct {
	Content string            `json:"content"`
	Secrets map[string]string `json:"secrets"`
	Changes []Change          `json:"changes"`
}

func (h *handlers) validate(c *gin.Context) {
	var in contentReq
	if !bind(c, &in) {
		return
	}
	rep := h.svc.ValidateEdit(in.Content)
	localizeReport(langOf(c), &rep)
	server.APIOK(c, rep)
}

func (h *handlers) params(c *gin.Context) {
	var in contentReq
	if !bind(c, &in) {
		return
	}
	res := h.svc.ParseParams(in.Content)
	localizeReport(langOf(c), &res.Report)
	server.APIOK(c, res)
}

func (h *handlers) patch(c *gin.Context) {
	var in contentReq
	if !bind(c, &in) {
		return
	}
	out, err := h.svc.PatchContent(in.Content, in.Changes)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"content": out})
}

func (h *handlers) previewDiff(c *gin.Context) {
	var in contentReq
	if !bind(c, &in) {
		return
	}
	res, err := h.svc.PreviewDiff(in.Content, in.Secrets)
	if err != nil {
		fail(c, err)
		return
	}
	localizeDiffResponse(langOf(c), res)
	server.APIOK(c, res)
}

func (h *handlers) save(c *gin.Context) {
	var in SaveRequest
	if !bind(c, &in) {
		return
	}
	res, err := h.svc.Save(c.Request.Context(), server.ActorOf(c), in)
	if err != nil {
		fail(c, err)
		return
	}
	localizeSave(langOf(c), res)
	server.APIOK(c, res)
}

func (h *handlers) importDisk(c *gin.Context) {
	var in ImportRequest
	if !bind(c, &in) {
		return
	}
	res, err := h.svc.ImportDisk(server.ActorOf(c), in)
	if err != nil {
		fail(c, err)
		return
	}
	localizeSave(langOf(c), res)
	server.APIOK(c, res)
}

func (h *handlers) version(c *gin.Context) {
	n, ok := versionParam(c)
	if !ok {
		return
	}
	v, err := h.svc.GetVersion(n)
	if err != nil {
		fail(c, err)
		return
	}
	localizeVersion(langOf(c), v)
	server.APIOK(c, v)
}

func (h *handlers) versionDiff(c *gin.Context) {
	n, ok := versionParam(c)
	if !ok {
		return
	}
	base, _ := strconv.Atoi(c.Query("base"))
	res, err := h.svc.DiffVersions(n, base)
	if err != nil {
		fail(c, err)
		return
	}
	localizeDiffResponse(langOf(c), res)
	server.APIOK(c, res)
}

func (h *handlers) rollback(c *gin.Context) {
	n, ok := versionParam(c)
	if !ok {
		return
	}
	var in RollbackRequest
	if !bind(c, &in) {
		return
	}
	res, err := h.svc.Rollback(c.Request.Context(), server.ActorOf(c), n, in)
	if err != nil {
		fail(c, err)
		return
	}
	localizeSave(langOf(c), res)
	server.APIOK(c, res)
}

func (h *handlers) restart(c *gin.Context) {
	var in RestartRequest
	if !bind(c, &in) {
		return
	}
	res, err := h.svc.Restart(c.Request.Context(), server.ActorOf(c), in)
	if err != nil {
		fail(c, err)
		return
	}
	localizeSave(langOf(c), res)
	server.APIOK(c, res)
}

func (h *handlers) restartStatus(c *gin.Context) {
	job := h.svc.JobView()
	localizeJob(langOf(c), job)
	server.APIOK(c, gin.H{"job": job})
}

func (h *handlers) saveDeploy(c *gin.Context) {
	var in DeployInput
	if !bind(c, &in) {
		return
	}
	v, err := h.svc.SaveDeploy(server.ActorOf(c), in)
	if err != nil {
		fail(c, err)
		return
	}
	v.PreviewError = tp(langOf(c), v.PreviewError)
	server.APIOK(c, v)
}

func (h *handlers) checkDeploy(c *gin.Context) {
	res, err := h.svc.CheckDeploy(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	res.Lines = tpLines(langOf(c), res.Lines)
	server.APIOK(c, res)
}

func (h *handlers) previewDeploy(c *gin.Context) {
	var in DeployInput
	if !bind(c, &in) {
		return
	}
	preview, problem := h.svc.PreviewDeploy(in.Deploy)
	server.APIOK(c, gin.H{"preview": preview, "problem": tp(langOf(c), problem)})
}
