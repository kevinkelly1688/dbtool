// @author: kevinkelly
// @license: Apache Licence
// @file: redis_http.go
// @time: 2026/10/28

package slowlog

import (
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/localize"
	"dbtool/internal/server"
)

// redisGapFromEnv 读取 REDIS_SLOWLOG_COLLECT_INTERVAL（秒，10–3600，默认 60）：同一个 Redis 实例两次采集之间的最小间隔。
func redisGapFromEnv(getenv func(string) string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(getenv("REDIS_SLOWLOG_COLLECT_INTERVAL")))
	if err != nil {
		return time.Minute
	}
	return time.Duration(clampInt(n, 10, 3600)) * time.Second
}

// registerRedis 登记 Redis 慢查询的接口（挂在 /api/v1/slowlog 下，与其它数据接口同一组权限：slowlogs.view / slowquery.view 查看按钮）。
func (h *handlers) registerRedis(s *server.Server, rt server.Routes, g *gin.RouterGroup) {
	g.GET("/redis/review", h.apiRedisReview)
	g.GET("/redis/history", h.apiRedisHistory)
	g.GET("/redis/trend", h.apiRedisTrend)
	g.GET("/redis/commands", h.apiRedisCommands)
	g.GET("/redis/collector", h.apiRedisCollector)
	g.POST("/redis/collect", h.apiRedisCollect)
	// SLOWLOG RESET 是高危按钮：只有持有 redis.slowlog_reset 的用户可以调用（默认仅超级用户），并写审计
	rt.API.POST("/slowlog/redis/reset", s.RequireButtonAPI(viewButtons...), s.RequireButtonAPI(redisResetButton), h.apiRedisReset)
}

func redisParams(c *gin.Context) (RedisParams, bool) {
	inst, ok1 := intQuery(c, "instance_id")
	limit, ok2 := intQuery(c, "limit")
	offset, ok3 := intQuery(c, "offset")
	if !ok1 || !ok2 || !ok3 {
		return RedisParams{}, false
	}
	return RedisParams{InstanceID: uint(inst), Command: c.Query("command"), FpID: c.Query("fp_id"), Search: c.Query("q"), Start: c.Query("start"), End: c.Query("end"),
		MinSeconds: c.Query("min_seconds"), Sort: c.Query("sort"), Order: c.Query("order"), Bucket: c.Query("bucket"), Limit: limit, Offset: offset}, true
}

func (h *handlers) apiRedisReview(c *gin.Context) {
	in, ok := redisParams(c)
	if !ok {
		return
	}
	page, err := h.svc.RedisReview(server.PrincipalOf(c), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	tpTexts(c, page.Notes)
	server.APIOK(c, page)
}

func (h *handlers) apiRedisHistory(c *gin.Context) {
	in, ok := redisParams(c)
	if !ok {
		return
	}
	page, err := h.svc.RedisHistory(server.PrincipalOf(c), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, page)
}

func (h *handlers) apiRedisTrend(c *gin.Context) {
	in, ok := redisParams(c)
	if !ok {
		return
	}
	tr, err := h.svc.RedisTrend(server.PrincipalOf(c), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, tr)
}

func (h *handlers) apiRedisCommands(c *gin.Context) {
	in, ok := redisParams(c)
	if !ok {
		return
	}
	list, err := h.svc.RedisCommands(server.PrincipalOf(c), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, list)
}

func (h *handlers) apiRedisCollector(c *gin.Context) {
	ov, err := h.svc.RedisCollectorOverview(server.PrincipalOf(c))
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, localize.Value(lang(c), ov, "instance_name"))
}

func (h *handlers) apiRedisCollect(c *gin.Context) {
	var req struct {
		InstanceID uint `json:"instance_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, badReq("请求格式错误"))
		return
	}
	n, err := h.svc.RedisCollectNow(c.Request.Context(), server.PrincipalOf(c), req.InstanceID)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"inserted": n})
}

func (h *handlers) apiRedisReset(c *gin.Context) {
	var req struct {
		InstanceID uint `json:"instance_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, badReq("请求格式错误"))
		return
	}
	res, err := h.svc.RedisReset(c.Request.Context(), server.PrincipalOf(c), req.InstanceID)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, res)
}
