// @author: kevinkelly
// @license: Apache Licence
// @file: query_ext.go
// @time: 2026/10/28

package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"dbtool/internal/query"
)

// 在线查询工作台的扩展接口：库表树元数据（表类型 / 估算行数 / 注释 / 无权限 / 敏感列 + 限制策略）、表的列详情、
// 按字段名或注释搜索、结果被行数上限截断时的总行数估算。业务全在 internal/query，这里只做参数解析与错误转换。

func init() {
	addRoutes(func(s *Server, rt Routes) {
		g := rt.API.Group("/query")
		g.GET("/instances/:id/databases/:db/meta", s.apiDBMeta)
		g.GET("/instances/:id/databases/:db/tables/:table/schema", s.apiTableSchema)
		g.GET("/instances/:id/databases/:db/search", s.apiColumnSearch)
		g.POST("/estimate", s.apiEstimate)
	})
}

func (s *Server) apiDBMeta(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	m, err := s.q.DBMeta(c.Request.Context(), principalOf(c), id, c.Param("db"))
	if err != nil {
		writeErr(c, err)
		return
	}
	if m != nil {
		m.Policy.Source = trMsg(c, m.Policy.Source) // 限制来源说明是服务端生成的中文
	}
	c.JSON(http.StatusOK, gin.H{"data": m})
}

func (s *Server) apiTableSchema(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	cols, err := s.q.TableColumns(c.Request.Context(), principalOf(c), id, c.Param("db"), c.Param("table"))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": cols})
}

func (s *Server) apiColumnSearch(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	hits, err := s.q.SearchColumns(c.Request.Context(), principalOf(c), id, c.Param("db"), strings.TrimSpace(c.Query("q")))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": hits})
}

func (s *Server) apiEstimate(c *gin.Context) {
	var req query.Request
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	est, err := s.q.EstimateRows(c.Request.Context(), principalOf(c), req)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": est})
}
