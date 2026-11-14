// @author: kevinkelly
// @license: Apache Licence
// @file: servertest_test.go
// @time: 2026/10/28

package servertest_test

import (
	"testing"

	"github.com/gin-gonic/gin"

	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

func TestHooksAndLogin(t *testing.T) {
	hook := func(s *server.Server, rt server.Routes) {
		rt.API.GET("/ping", func(c *gin.Context) {
			server.APIOK(c, gin.H{"user": server.PrincipalOf(c).Username, "su": s.HasButton(c, "user.edit")})
		})
		rt.API.POST("/echo", s.SuperuserAPI(), func(c *gin.Context) { server.APIOK(c, gin.H{"ok": true}) })
	}
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){hook}})
	if w := env.Anonymous("GET", "/api/v1/ping"); w.Code != 401 {
		t.Fatalf("未登录应 401: %d", w.Code)
	}
	root := env.Login("root")
	var out struct {
		User string `json:"user"`
		Su   bool   `json:"su"`
	}
	root.GetJSON("/api/v1/ping", &out)
	if out.User != "root" || !out.Su {
		t.Fatalf("%+v", out)
	}
	bob := env.Login("bob")
	bob.GetJSON("/api/v1/ping", &out)
	if out.Su {
		t.Fatal("bob 没有权限")
	}
	if code, _ := bob.JSON("POST", "/api/v1/echo", map[string]any{}, nil); code != 403 {
		t.Fatalf("普通用户应 403: %d", code)
	}
	if code, _ := root.JSON("POST", "/api/v1/echo", map[string]any{}, nil); code != 200 {
		t.Fatalf("超级用户应 200: %d", code)
	}
}
