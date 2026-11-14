// @author: kevinkelly
// @license: Apache Licence
// @file: accesslog_test.go
// @time: 2026/10/28

package server

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAccessLogHidesSSOQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var buf bytes.Buffer
	old := gin.DefaultWriter
	gin.DefaultWriter = &buf
	defer func() { gin.DefaultWriter = old }()
	r := gin.New()
	r.Use(accessLogger())
	r.GET("/login/sso/1/callback", func(c *gin.Context) { c.String(200, "ok") })
	r.GET("/query", func(c *gin.Context) { c.String(200, "ok") })
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/login/sso/1/callback?code=SECRETCODE&state=SECRETSTATE", nil))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/query?a=visible", nil))
	out := buf.String()
	if strings.Contains(out, "SECRETCODE") || strings.Contains(out, "SECRETSTATE") {
		t.Fatalf("授权码/state 进了访问日志: %s", out)
	}
	if !strings.Contains(out, "/login/sso/1/callback?…") || !strings.Contains(out, "a=visible") {
		t.Fatalf("日志格式不符: %s", out)
	}
}
