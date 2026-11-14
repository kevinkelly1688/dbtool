// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package binlog

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"dbtool/internal/audit"
	"dbtool/internal/i18n"
	"dbtool/internal/instext"
	"dbtool/internal/model"
	"dbtool/internal/server"
)

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, _, ok := s.DB()
		if !ok { // 仓库不是 store.Store（测试 / 演示）：没有元数据库，路由即 404
			return
		}
		svc := NewService(s.Repo(), nil, newStore(db))
		svc.SetDisabled(func() (map[uint]bool, error) { return instext.DisabledSet(db) })
		register(s, rt, svc)
	})
	server.AddMenu(server.MenuItem{Group: "inst", Key: "inst-binlog", Href: "/binlog", Label: "日志解析", Order: 55,
		Show: func(s *server.Server, a audit.Actor) bool {
			if _, _, ok := s.DB(); !ok {
				return false
			}
			return s.ActorHasButton(a, BtnView)
		}})
}

// store 只负责写操作审计（dbt_audit_log）。本模块没有自己的表：任务保存在内存里，结束 30 分钟后清理。
type store struct{ db *gorm.DB }

func newStore(db *gorm.DB) *store { return &store{db: db} }

func (st *store) Record(e *model.AuditEntry) error { return st.db.Create(e).Error }

type handlers struct {
	s   *server.Server
	svc *Service
}

// register 登记页面与接口（测试里直接传入假依赖的 Service）。
func register(s *server.Server, rt server.Routes, svc *Service) {
	h := &handlers{s: s, svc: svc}
	server.AddWorker(func(ctx context.Context) {
		svc.SetRoot(ctx)
		svc.CleanSpill() // 清掉上次进程被杀时遗留的结果临时文件
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

	rt.Pages.GET("/binlog", s.RequireButtonPage(BtnView), h.page)

	view := s.RequireButtonAPI(BtnView)
	anyParse := s.RequireButtonAPI(BtnParse, BtnRollback)
	g := rt.API.Group("/binlog")
	g.GET("/options", view, h.apiOptions)
	g.GET("/instances/:id/info", view, h.apiInfo)
	g.GET("/instances/:id/databases", view, h.apiDatabases)
	g.POST("/parse", anyParse, h.apiParse)
	g.GET("/jobs", anyParse, h.apiJobs)
	g.GET("/jobs/:id", anyParse, h.apiJob)
	g.GET("/jobs/:id/rows", anyParse, h.apiRows)
	g.POST("/jobs/:id/cancel", anyParse, h.apiCancel)
	g.GET("/jobs/:id/download", s.RequireButtonAPI(BtnDownload), h.apiDownload)
}

// pageData 是页面模板的数据：三个“上限”数值包在带 id 的 <span> 里（脚本会按接口返回值刷新它们），
// 句子整句交给 {{th}} 翻译，数值以 template.HTML 原样代入。
type pageData struct {
	HardRows, HardMB, MaxTimeout template.HTML
}

func (h *handlers) page(c *gin.Context) {
	hardRows, hardMB := h.svc.HardLimits()
	// #nosec G203 -- 只含整数（系统设置里的上限已按整数解析并夹在登记范围内）
	d := pageData{
		HardRows:   template.HTML(fmt.Sprintf(`<span id="bl-hard-rows">%d</span>`, hardRows)),
		HardMB:     template.HTML(fmt.Sprintf(`<span id="bl-hard-mb">%d</span>`, hardMB)),
		MaxTimeout: template.HTML(fmt.Sprintf(`<span id="bl-max-timeout">%d</span>`, MaxTimeoutSec)),
	}
	h.s.Render(c, http.StatusOK, "binlog/index.html", server.PageData{Title: "日志解析", Nav: "inst-binlog", Data: d})
}

func writeErr(c *gin.Context, err error) {
	var e *Error
	if errors.As(err, &e) {
		server.APIFail(c, e.HTTP, e.Code, e.Message)
		return
	}
	slog.Error("Binlog 解析模块请求失败", "path", c.Request.URL.Path, "err", err)
	server.APIFail(c, http.StatusInternalServerError, "internal", "服务异常，请稍后再试")
}

func idParam(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "实例参数错误")
		return 0, false
	}
	return uint(id), true
}

func (h *handlers) perms(c *gin.Context) gin.H {
	return gin.H{"parse": h.s.HasButton(c, BtnParse), "rollback": h.s.HasButton(c, BtnRollback), "download": h.s.HasButton(c, BtnDownload)}
}

func (h *handlers) apiOptions(c *gin.Context) {
	list, err := h.svc.Instances(server.ActorOf(c))
	if err != nil {
		writeErr(c, err)
		return
	}
	hardRows, hardMB := h.svc.HardLimits()
	server.APIOK(c, gin.H{
		"instances": list,
		"perms":     h.perms(c),
		"limits": gin.H{"default_rows": min(DefaultMaxRows, hardRows), "hard_rows": hardRows, "default_mb": min(DefaultMaxMB, hardMB), "hard_mb": hardMB,
			"default_timeout": DefaultTimeoutSec, "max_timeout": MaxTimeoutSec, "page_rows": RowsPageMax, "max_scan": MaxScanEvents,
			"keep_minutes": int(jobTTL / time.Minute)},
	})
}

func (h *handlers) apiInfo(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	info, err := h.svc.Info(c.Request.Context(), server.ActorOf(c), id)
	if err != nil {
		writeErr(c, err)
		return
	}
	info.RollbackReason = i18n.TP(i18n.FromContext(c.Request.Context()), info.RollbackReason) // 不能回滚的原因（服务端生成的中文提示）
	server.APIOK(c, info)
}

func (h *handlers) apiDatabases(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	dbs, err := h.svc.Databases(c.Request.Context(), server.ActorOf(c), id)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, dbs)
}

func (h *handlers) apiParse(c *gin.Context) {
	var req Req
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	if err := c.ShouldBindJSON(&req); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	switch req.Mode {
	case "", ModeSQL:
		if !h.s.HasButton(c, BtnParse) {
			server.APIFail(c, http.StatusForbidden, "forbidden", "没有权限解析 binlog")
			return
		}
	case ModeRollback:
		if !h.s.HasButton(c, BtnRollback) {
			server.APIFail(c, http.StatusForbidden, "forbidden", "没有权限生成回滚 SQL")
			return
		}
	default:
		server.APIFail(c, http.StatusBadRequest, "bad_request", "模式只能是 sql 或 rollback")
		return
	}
	v, err := h.svc.Start(c.Request.Context(), server.ActorOf(c), req)
	if err != nil {
		writeErr(c, err)
		return
	}
	h.finishView(c, v)
	server.APIOK(c, v)
}

// finishView 把“能否下载”与当前用户的下载按钮权限相与，并把服务端生成的提示文字翻译成请求语言。
func (h *handlers) finishView(c *gin.Context, v *JobView) {
	v.CanDownload = v.CanDownload && h.s.HasButton(c, BtnDownload) && (v.Mode != ModeRollback || h.s.HasButton(c, BtnRollback))
	v.localize(i18n.FromContext(c.Request.Context()))
}

// modeAllowed 查看回滚任务的内容需要回滚权限（权限可能在任务创建之后被收回）。
func (h *handlers) modeAllowed(c *gin.Context, mode string) bool {
	if mode == ModeRollback {
		return h.s.HasButton(c, BtnRollback)
	}
	return h.s.HasButton(c, BtnParse)
}

func (h *handlers) apiJobs(c *gin.Context) {
	list := h.svc.Jobs(server.ActorOf(c))
	out := make([]JobView, 0, len(list))
	for i := range list {
		if !h.modeAllowed(c, list[i].Mode) {
			continue
		}
		h.finishView(c, &list[i])
		out = append(out, list[i])
	}
	server.APIOK(c, out)
}

func (h *handlers) apiJob(c *gin.Context) {
	v, err := h.svc.Job(server.ActorOf(c), c.Param("id"))
	if err != nil {
		writeErr(c, err)
		return
	}
	if !h.modeAllowed(c, v.Mode) {
		server.APIFail(c, http.StatusForbidden, "forbidden", "没有权限查看该任务")
		return
	}
	h.finishView(c, v)
	server.APIOK(c, v)
}

func (h *handlers) apiRows(c *gin.Context) {
	actor := server.ActorOf(c)
	v, err := h.svc.Job(actor, c.Param("id"))
	if err != nil {
		writeErr(c, err)
		return
	}
	if !h.modeAllowed(c, v.Mode) {
		server.APIFail(c, http.StatusForbidden, "forbidden", "没有权限查看该任务")
		return
	}
	offset, _ := strconv.Atoi(c.Query("offset"))
	limit, _ := strconv.Atoi(c.Query("limit"))
	page, err := h.svc.Rows(actor, c.Param("id"), offset, limit)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, page)
}

func (h *handlers) apiCancel(c *gin.Context) {
	if err := h.svc.Cancel(server.ActorOf(c), c.Param("id")); err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiDownload(c *gin.Context) {
	actor := server.ActorOf(c)
	v, err := h.svc.Job(actor, c.Param("id"))
	if err != nil {
		writeErr(c, err)
		return
	}
	if v.Mode == ModeRollback && !h.s.HasButton(c, BtnRollback) {
		server.APIFail(c, http.StatusForbidden, "forbidden", "没有权限下载回滚 SQL")
		return
	}
	d, err := h.svc.OpenDownload(i18n.FromContext(c.Request.Context()), actor, c.Param("id"))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+d.Name+`"`)
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Status(http.StatusOK)
	// 结果可能有几百 MB 到几 GB：边读边写，不在内存里拼整个文件；响应已经开始，中途出错只能记日志（审计里会标“未传完”）
	if _, err := d.WriteTo(c.Writer); err != nil {
		slog.Warn("Binlog 结果文件下载中断", "job", c.Param("id"), "err", err)
	}
}
