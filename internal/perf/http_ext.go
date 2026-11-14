// @author: kevinkelly
// @license: Apache Licence
// @file: http_ext.go
// @time: 2026/10/28

package perf

import (
	"github.com/gin-gonic/gin"

	"dbtool/internal/server"
)

// 本文件是列表筛选 / 路由测试 / 采集窗口 / 升级通知矩阵这几个后加接口的处理函数。

// apiRouteTest 路由测试（只读，沿用“查看配置”按钮）。
func (h *handlers) apiRouteTest(c *gin.Context) {
	var in RouteTestInput
	if !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.TestRoute(in)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, res)
}

// apiWindowSave 修改采集窗口（沿用“修改采集配置”按钮）。
func (h *handlers) apiWindowSave(c *gin.Context) {
	var in struct {
		Minutes int `json:"minutes"`
	}
	if !bindJSON(c, &in) {
		return
	}
	v, err := h.viewer(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	w, err := h.svc.SaveCollectWindow(v, in.Minutes)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, w)
}

// listSummary 是列表接口 summary 字段：兼容旧的 todo / overdue，另带各页签与 KPI 的计数。
func listSummary(res *ListResult) gin.H {
	out := gin.H{"todo": res.Todo, "overdue": res.Overdue}
	if res.Counts != nil {
		out["counts"] = res.Counts
	}
	return out
}
