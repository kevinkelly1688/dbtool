// @author: kevinkelly
// @license: Apache Licence
// @file: gzip.go
// @time: 2026/10/28

package server

import (
	"compress/gzip"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// 接口响应的 gzip 压缩：在线查询一次返回几 MB 的 JSON 很常见（大文本列），原样发出去在公网 / 隧道 / 反向代理上要等很久；JSON 文本一般能压到十分之一。
// 只压缩同时满足下面条件的响应，其它一律原样直通：
//   - 请求路径在 /api/ 或 /i18n/ 下（后者是约 1 MB 的英文译文目录，每次换版本后第一次打开都要整份下载），方法不是 HEAD，请求头 Accept-Encoding 声明接受 gzip（浏览器都会带；没带的客户端，如 MCP 的 Node https，拿到的还是原文）；
//   - 响应状态是 2xx（不含 204），Content-Type 是 application/json，没有 Content-Encoding，不是附件下载（没有 Content-Disposition）；
//   - 第一次写出的内容不小于 gzipMinBytes（小响应压缩没有意义，还要多花 CPU）。
//
// 页面 HTML、静态文件、下载、流式响应都不在范围内。
const gzipMinBytes = 1024

// acceptsGzip 判断 Accept-Encoding 是否接受 gzip（支持 q=0 表示不接受）。
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		params = strings.ReplaceAll(strings.ToLower(params), " ", "")
		if q, ok := strings.CutPrefix(params, "q="); ok {
			if f, err := strconv.ParseFloat(q, 64); err == nil && f == 0 {
				return false
			}
		}
		return true
	}
	return false
}

// gzipAPI 返回压缩接口 JSON 响应的中间件。要放在会包装 c.Writer 的中间件（如语言翻译）前面，让它们写进来的内容一起被压缩。
func gzipAPI() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := c.Request.URL.Path
		if c.Request.Method == "HEAD" || !(strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/i18n/")) || !acceptsGzip(c.GetHeader("Accept-Encoding")) {
			c.Next()
			return
		}
		w := &gzipWriter{ResponseWriter: c.Writer}
		c.Writer = w
		c.Next()
		w.finish()
	}
}

type gzipWriter struct {
	gin.ResponseWriter
	gz      *gzip.Writer
	decided bool
}

// decide 在第一次写出时决定要不要压缩（此时状态码和响应头都已经设好）。
func (w *gzipWriter) decide(first int) {
	if w.decided {
		return
	}
	w.decided = true
	h := w.Header()
	st := w.Status()
	if first < gzipMinBytes || st < 200 || st >= 300 || st == 204 ||
		h.Get("Content-Encoding") != "" || h.Get("Content-Disposition") != "" || !strings.HasPrefix(h.Get("Content-Type"), "application/json") {
		return
	}
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	h.Del("Content-Length") // 压缩后长度变了，交给服务器分块输出
	w.gz, _ = gzip.NewWriterLevel(w.ResponseWriter, gzip.BestSpeed)
}

func (w *gzipWriter) Write(b []byte) (int, error) {
	w.decide(len(b))
	if w.gz != nil {
		return w.gz.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *gzipWriter) WriteString(s string) (int, error) {
	w.decide(len(s))
	if w.gz != nil {
		return w.gz.Write([]byte(s))
	}
	return w.ResponseWriter.WriteString(s)
}

// Flush 先把压缩器里的数据冲出来，再冲底层连接。
func (w *gzipWriter) Flush() {
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	w.ResponseWriter.Flush()
}

// finish 在处理完成后写出 gzip 的结尾；没有压缩时什么也不做。
func (w *gzipWriter) finish() {
	if w.gz != nil {
		_ = w.gz.Close()
		w.gz = nil
	}
}
