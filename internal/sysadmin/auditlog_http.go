// @author: kevinkelly
// @license: Apache Licence
// @file: auditlog_http.go
// @time: 2026/10/28

package sysadmin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"dbtool/internal/i18n"
	"dbtool/internal/server"
)

// 系统审计的扩展接口（概览、详情、状态分布、查询审计汇总与完整 SQL）。与 http.go 里的审计接口同属 audit.view 权限组。
func registerAuditExt(h *handlers, au *gin.RouterGroup) {
	au.GET("/overview", h.auditOverview)
	au.GET("/log/:id", h.auditDetail)
	au.GET("/sql-workflows/stats", h.auditSQLStats)
	au.GET("/query-logs/stats", h.auditQueryStats)
	au.GET("/query-logs/:kind/:id", h.auditQueryDetail)
}

func (h *handlers) auditOverview(c *gin.Context) {
	o, err := h.svc.AuditOverview()
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, o)
}

func (h *handlers) auditDetail(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	d, err := h.svc.AuditDetail(id)
	if err != nil {
		writeErr(c, err)
		return
	}
	d.Hint = i18n.TP(reqLang(c), d.Hint)
	server.APIOK(c, d)
}

func (h *handlers) auditSQLStats(c *gin.Context) {
	gid, ok := queryUint(c, "group_id")
	if !ok {
		return
	}
	list, err := h.svc.SQLWorkflowStats(SQLWorkflowQuery{Status: c.Query("status"), GroupID: gid, Search: c.Query("search"),
		StartDate: c.Query("start_date"), EndDate: c.Query("end_date")})
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, list)
}

func queryAuditOf(c *gin.Context) QueryAuditQuery {
	return QueryAuditQuery{User: c.Query("user"), Instance: c.Query("instance"), Search: c.Query("search"),
		StartDate: c.Query("start_date"), EndDate: c.Query("end_date"), OnlyRejected: truthy(c.Query("only_rejected")),
		Page: queryInt(c, "page", 1), PageSize: queryInt(c, "page_size", 20)}
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (h *handlers) auditQueryStats(c *gin.Context) {
	st, err := h.svc.QueryAuditStats(queryAuditOf(c))
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, st)
}

func (h *handlers) auditQueryDetail(c *gin.Context) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数非法")
		return
	}
	d, err := h.svc.QueryAuditDetail(c.Param("kind"), uint(n))
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, d)
}
