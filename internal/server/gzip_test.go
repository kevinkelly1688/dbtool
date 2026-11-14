// @author: kevinkelly
// @license: Apache Licence
// @file: gzip_test.go
// @time: 2026/10/28

package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func gzipTestEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gzipAPI())
	big := gin.H{"data": strings.Repeat("订单日志 ", 2000)} // 一万多字节，压缩效果明显
	r.GET("/api/big", func(c *gin.Context) { c.JSON(http.StatusOK, big) })
	r.POST("/api/big", func(c *gin.Context) { c.JSON(http.StatusOK, big) })
	r.GET("/api/small", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	r.GET("/api/err", func(c *gin.Context) { c.JSON(http.StatusInternalServerError, big) })
	r.GET("/api/none", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	r.GET("/api/download", func(c *gin.Context) {
		c.Header("Content-Disposition", `attachment; filename="x.json"`)
		c.JSON(http.StatusOK, big)
	})
	r.GET("/api/text", func(c *gin.Context) { c.String(http.StatusOK, strings.Repeat("x", 5000)) })
	r.GET("/api/already", func(c *gin.Context) {
		c.Header("Content-Encoding", "br")
		c.JSON(http.StatusOK, big)
	})
	r.GET("/page/big", func(c *gin.Context) { c.JSON(http.StatusOK, big) })
	r.GET("/i18n/:file", func(c *gin.Context) {
		c.Header("ETag", `"v1"`)
		c.Data(http.StatusOK, "application/json; charset=utf-8", []byte(`{"k":"`+strings.Repeat("订单日志 ", 2000)+`"}`))
	})
	return r
}

func gzipDo(r *gin.Engine, method, path, acceptEncoding string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestGzipAPICompressesLargeJSON(t *testing.T) {
	r := gzipTestEngine()
	plain := gzipDo(r, "GET", "/api/big", "")
	if plain.Header().Get("Content-Encoding") != "" || plain.Body.Len() < 10000 {
		t.Fatalf("没带 Accept-Encoding 应原样返回：%q %d", plain.Header().Get("Content-Encoding"), plain.Body.Len())
	}
	for _, method := range []string{"GET", "POST"} {
		w := gzipDo(r, method, "/api/big", "gzip, deflate, br")
		if w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
			t.Fatalf("%s 应压缩：%d %v", method, w.Code, w.Header())
		}
		if w.Header().Get("Content-Length") != "" {
			t.Errorf("压缩后不应带原来的 Content-Length：%q", w.Header().Get("Content-Length"))
		}
		if w.Body.Len()*5 > plain.Body.Len() {
			t.Errorf("压缩效果太差：%d -> %d", plain.Body.Len(), w.Body.Len())
		}
		zr, err := gzip.NewReader(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != plain.Body.String() || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
			t.Errorf("解压后应与原文逐字节相同（%d 对 %d），Content-Type=%q", len(got), plain.Body.Len(), w.Header().Get("Content-Type"))
		}
	}
}

// 英文译文目录 /i18n/en.json 也要压缩，且解压后与原文逐字节相同、ETag 保留。
func TestGzipCoversI18nCatalog(t *testing.T) {
	r := gzipTestEngine()
	plain := gzipDo(r, "GET", "/i18n/en.json", "")
	w := gzipDo(r, "GET", "/i18n/en.json", "gzip, deflate, br")
	if w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("ETag") != `"v1"` || w.Body.Len()*5 > plain.Body.Len() {
		t.Fatalf("译文目录应压缩并保留 ETag：%v %d->%d", w.Header(), plain.Body.Len(), w.Body.Len())
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(zr); string(got) != plain.Body.String() {
		t.Errorf("解压后应与原文相同")
	}
}

func TestGzipAPILeavesOthersAlone(t *testing.T) {
	r := gzipTestEngine()
	cases := []struct{ name, method, path, ae string }{
		{"小响应", "GET", "/api/small", "gzip"},
		{"错误响应", "GET", "/api/err", "gzip"},
		{"204", "GET", "/api/none", "gzip"},
		{"附件下载", "GET", "/api/download", "gzip"},
		{"非 JSON", "GET", "/api/text", "gzip"},
		{"已经有 Content-Encoding", "GET", "/api/already", "gzip"},
		{"不在 /api/ 下", "GET", "/page/big", "gzip"},
		{"HEAD", "HEAD", "/api/big", "gzip"},
		{"gzip;q=0 表示不接受", "GET", "/api/big", "gzip;q=0, br"},
		{"只接受 br", "GET", "/api/big", "br"},
	}
	for _, c := range cases {
		w := gzipDo(r, c.method, c.path, c.ae)
		enc := w.Header().Get("Content-Encoding")
		if c.name == "已经有 Content-Encoding" {
			if enc != "br" {
				t.Errorf("%s：不能覆盖已有的编码，得到 %q", c.name, enc)
			}
			continue
		}
		if enc == "gzip" {
			t.Errorf("%s：不该压缩", c.name)
		}
	}
	// 不压缩时内容是可以直接读的 JSON
	if w := gzipDo(r, "GET", "/api/small", "gzip"); strings.TrimSpace(w.Body.String()) != `{"ok":true}` {
		t.Errorf("小响应应原样：%q", w.Body.String())
	}
}

func TestAcceptsGzip(t *testing.T) {
	for h, want := range map[string]bool{
		"gzip": true, "GZIP": true, "gzip, deflate, br": true, "deflate, gzip;q=0.8": true, " br , gzip ": true,
		"": false, "br": false, "identity": false, "gzip;q=0": false, "gzip; q=0.0, br": false, "x-gzip": false,
	} {
		if got := acceptsGzip(h); got != want {
			t.Errorf("acceptsGzip(%q)=%v 期望 %v", h, got, want)
		}
	}
}
