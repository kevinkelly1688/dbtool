// @author: kevinkelly
// @license: Apache Licence
// @file: accesslog.go
// @time: 2026/10/28

package server

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
)

// accessLogger 是 gin 默认访问日志的等价实现，区别是：登录回调类路径（/login/sso/…）不记录查询串——
// 里面有 OAuth 授权码、CAS ticket 和 state，不应该进日志。
func accessLogger() gin.HandlerFunc {
	return gin.LoggerWithFormatter(func(p gin.LogFormatterParams) string {
		path := p.Path
		if strings.HasPrefix(path, "/login/sso/") {
			if i := strings.IndexByte(path, '?'); i >= 0 {
				path = path[:i] + "?…"
			}
		}
		return fmt.Sprintf("[GIN] %s | %3d | %13v | %15s | %-7s %#v\n%s",
			p.TimeStamp.Format("2006/01/02 - 15:04:05"), p.StatusCode, p.Latency, p.ClientIP, p.Method, path, p.ErrorMessage)
	})
}
