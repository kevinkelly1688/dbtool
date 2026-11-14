// @author: kevinkelly
// @license: Apache Licence
// @file: sql_extra.go
// @time: 2026/10/28

package server

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"dbtool/internal/i18n"
	"dbtool/internal/sqlflow"
)

// SQL 上线 / 数据导出的增量接口：数据导出独立列表页、提交页的审批流预览、复制为新工单、取消定时、执行进度。
// 路由放在这里登记（而不是改 server.go），与其他模块的做法一致。

func init() {
	addRoutes(func(s *Server, rt Routes) {
		if s.sql == nil {
			return
		}
		rt.Pages.GET("/sqlworkflow/export", s.sqlEnabled(), s.sqlExportListPage)
		sa := rt.API.Group("/sqlworkflow", s.sqlEnabled(), s.fillDisplay())
		sa.GET("/flow-preview", s.apiSQLFlowPreview)
		sa.GET("/:id/copy", s.apiSQLCopy)
		sa.POST("/:id/schedule/cancel", s.apiSQLScheduleCancel)
		sa.GET("/:id/progress", s.apiSQLProgress)
		sa.POST("/:id/mark-exception", s.apiSQLMarkException)
		sa.POST("/:id/cancel-run", s.apiSQLCancelRun)
	})
}

// applyExtraButtons 让详情里新增的“能不能操作”标记同时满足更细的按钮权限（服务层的标记按资源组 / 提交人 / 状态算）。
func (s *Server) applyExtraButtons(c *gin.Context, d *sqlflow.Detail) {
	d.CanCancelSchedule = d.CanCancelSchedule && (s.HasButton(c, "sqlworkflow.schedule") || s.HasButton(c, "sqlworkflow.cancel_own") || s.HasButton(c, "sqlworkflow.reject"))
	d.CanMarkException = d.CanMarkException && (s.HasButton(c, "sqlworkflow.cancel_own") || s.HasButton(c, "sqlworkflow.reject"))
	if d.IsExport {
		d.CanCopy = d.CanCopy && (s.HasButton(c, "export.submit") || s.HasButton(c, "export.submit_batch"))
	} else {
		d.CanCopy = d.CanCopy && (s.HasButton(c, "sqlworkflow.submit") || s.HasButton(c, "sqlworkflow.submit_batch"))
	}
}

// apiSQLFlowPreview 提交页的审批流预览：GET /api/v1/sqlworkflow/flow-preview?group_id=
func (s *Server) apiSQLFlowPreview(c *gin.Context) {
	gid, err := strconv.ParseUint(c.Query("group_id"), 10, 32)
	if err != nil || gid == 0 {
		apiFail(c, http.StatusBadRequest, "bad_request", "请选择资源组")
		return
	}
	res, err := s.sql.FlowPreview(actorOf(c), uint(gid))
	if err != nil {
		writeErr(c, err)
		return
	}
	if langOf(c) != i18n.ZH { // 提示与每一级的告警是我们拼出来的中文：按请求语言翻译
		for i := range res.Warnings {
			res.Warnings[i] = sqlTrText(c, res.Warnings[i])
		}
		for i := range res.Levels {
			res.Levels[i].Warning = sqlTrText(c, res.Levels[i].Warning)
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// apiSQLCopy 终态工单“复制为新工单”：返回提交页要预填的内容。
func (s *Server) apiSQLCopy(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	res, err := s.sql.CopySource(actorOf(c), id)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// apiSQLScheduleCancel 取消定时执行（工单回到“审核通过”）。
func (s *Server) apiSQLScheduleCancel(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if !s.ButtonOrFail(c, "sqlworkflow.schedule", "sqlworkflow.cancel_own", "sqlworkflow.reject") {
		return
	}
	w, err := s.sql.CancelSchedule(actorOf(c), id)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": w.Status, "status_label": trMsg(c, sqlflow.StatusLabel(w.Status))}})
}

// apiSQLProgress 执行 / 导出进度（详情页进度条轮询）。
func (s *Server) apiSQLProgress(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	res, err := s.sql.Progress(actorOf(c), id)
	if err != nil {
		writeErr(c, err)
		return
	}
	res.Label = trMsg(c, res.Label)
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// apiSQLCancelRun 取消正在执行的工单（DBA / 超级用户；需要“取消执行”按钮权限，原因必填）。
func (s *Server) apiSQLCancelRun(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if !s.ButtonOrFail(c, "sqlworkflow.cancel_run") {
		return
	}
	var req sqlRemarkReq
	_ = c.ShouldBindJSON(&req)
	w, err := s.sql.CancelRun(actorOf(c), id, req.Remark)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": w.Status, "status_label": trMsg(c, sqlflow.StatusLabel(w.Status)), "message": tr(c, "已发出取消请求，工单会在语句中断后变为「执行有异常」")}})
}

// apiSQLMarkException 把仍显示“排队中 / 执行中”的工单标记为“执行有异常”（进程失联后用；不重试，服务层先检查执行锁）。
// 沿用终止类按钮：终止自己的（sqlworkflow.cancel_own）或驳回 / 终止他人的（sqlworkflow.reject）；服务层再按提交人 / 资源组执行权限 / 审批权限判断。
func (s *Server) apiSQLMarkException(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if !s.ButtonOrFail(c, "sqlworkflow.cancel_own", "sqlworkflow.reject") {
		return
	}
	var req sqlRemarkReq
	_ = c.ShouldBindJSON(&req)
	w, err := s.sql.MarkException(actorOf(c), id, req.Remark)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": w.Status, "status_label": trMsg(c, sqlflow.StatusLabel(w.Status))}})
}
