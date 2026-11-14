// @author: kevinkelly
// @license: Apache Licence
// @file: query_redis.go
// @time: 2026/10/28

package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"dbtool/internal/query"
)

// 在线查询的 Redis 扩展接口：库下拉（带键数）与 key 浏览。业务全在 internal/query/redis.go，这里只做参数解析与错误转换。

func init() {
	addRoutes(func(s *Server, rt Routes) {
		g := rt.API.Group("/query")
		g.GET("/instances/:id/redis/databases", s.apiRedisDatabases)
		g.GET("/instances/:id/databases/:db/keys", s.apiRedisKeys)
	})
}

func (s *Server) apiRedisDatabases(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	list, err := s.q.RedisDatabases(c.Request.Context(), principalOf(c), id)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": list})
}

func (s *Server) apiRedisKeys(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	count, _ := strconv.Atoi(c.Query("count"))
	res, err := s.q.RedisKeys(c.Request.Context(), principalOf(c), id, c.Param("db"),
		query.RedisKeysReq{Match: strings.TrimSpace(c.Query("match")), Cursor: strings.TrimSpace(c.Query("cursor")), Count: count})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}
