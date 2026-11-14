// @author: kevinkelly
// @license: Apache Licence
// @file: auditip.go
// @time: 2026/10/28

package server

import (
	"strings"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
)

// auditIPBinder 把本次请求的客户端 IP 绑定到处理请求的 goroutine，供 dbt_audit_log 的扩展信息（来源 IP）使用。
// 见 internal/audit/ext.go：各模块写审计的代码不用传 IP，GORM 回调在同一个 goroutine 里读取。静态资源不需要，跳过。
func auditIPBinder() gin.HandlerFunc {
	return func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/static/") || c.Request.URL.Path == "/healthz" {
			c.Next()
			return
		}
		defer audit.BindIP(c.ClientIP())()
		c.Next()
	}
}
