// @author: kevinkelly
// @license: Apache Licence
// @file: button_audit_test.go
// @time: 2026/10/28

package server_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// auditBox 收集通过 SetAuditSink 写出的审计。
type auditBox struct {
	mu sync.Mutex
	es []model.AuditEntry
}

func (b *auditBox) add(e model.AuditEntry) { b.mu.Lock(); b.es = append(b.es, e); b.mu.Unlock() }
func (b *auditBox) named(action string) []model.AuditEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []model.AuditEntry
	for _, e := range b.es {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// S9-A4：按钮校验返回 403 时写“拒绝（无权限）”审计；同一用户 / 按钮 / 路径 1 分钟内只记一次；普通 GET 查看按钮不记。
func TestNoPermissionAudited(t *testing.T) {
	box := &auditBox{}
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){func(s *server.Server, rt server.Routes) {
		s.SetAuditSink(box.add)
		rt.API.POST("/t/manage", s.RequireButtonAPI("qpriv.manage"), func(c *gin.Context) { server.APIOK(c, "ok") })
		rt.API.POST("/t/manage2", s.RequireButtonAPI("qpriv.manage"), func(c *gin.Context) { server.APIOK(c, "ok") })
		rt.API.POST("/t/multi", s.RequireButtonAPI("qpriv.manage", "export.download"), func(c *gin.Context) { server.APIOK(c, "ok") })
		rt.API.GET("/t/view", s.RequireButtonAPI("archive.view"), func(c *gin.Context) { server.APIOK(c, "ok") })
		rt.API.GET("/t/danger-get", s.RequireButtonAPI("instance.delete"), func(c *gin.Context) { server.APIOK(c, "ok") })
		rt.API.POST("/t/audited", s.RequireButtonAPIAudited("export.download"), func(c *gin.Context) { server.APIOK(c, "ok") })
		rt.API.POST("/t/orfail", func(c *gin.Context) {
			if s.ButtonOrFail(c, "partman.run") {
				server.APIOK(c, "ok")
			}
		})
	}}})
	bob, root := env.Login("bob"), env.Login("root")
	denied := func() []model.AuditEntry { return box.named(audit.ActionNoPerm) }

	if code, _ := bob.JSON("POST", "/api/v1/t/manage", nil, nil); code != 403 {
		t.Fatalf("应 403: %d", code)
	}
	es := denied()
	if len(es) != 1 || es[0].UserName != "bob" || es[0].UserID == 0 ||
		!strings.Contains(es[0].ExtraInfo, "qpriv.manage") || !strings.Contains(es[0].ExtraInfo, "POST /api/v1/t/manage") {
		t.Fatalf("审计应含用户、按钮码、方法、路径: %+v", es)
	}
	if c := audit.Classify(es[0].Action, es[0].ExtraInfo); c.Result != audit.ResultDenied || c.Code != "qpriv.manage" || !c.Risky {
		t.Errorf("归类: %+v", c)
	}

	// 去重：同一用户同一按钮同一路径，再点不记
	for i := 0; i < 5; i++ {
		bob.JSON("POST", "/api/v1/t/manage", nil, nil)
	}
	if n := len(denied()); n != 1 {
		t.Errorf("1 分钟内同一路径只记一次: %d", n)
	}
	// 路径不同 / 用户不同 / 按钮不同：各记一次
	bob.JSON("POST", "/api/v1/t/manage2", nil, nil)
	if n := len(denied()); n != 2 {
		t.Errorf("不同路径应再记一次: %d", n)
	}
	bob.JSON("POST", "/api/v1/t/orfail", nil, nil) // ButtonOrFail 的 403 也记
	if n := len(denied()); n != 3 {
		t.Errorf("ButtonOrFail 的 403 也应记: %d", n)
	}
	bob.JSON("POST", "/api/v1/t/audited", nil, nil) // RequireButtonAPIAudited 的 403 也记
	if n := len(denied()); n != 4 {
		t.Errorf("RequireButtonAPIAudited 的 403 也应记: %d", n)
	}
	// 多个可选按钮：权限码取第一个，其余写在可选里
	bob.JSON("POST", "/api/v1/t/multi", nil, nil)
	es = denied()
	if len(es) != 5 || !strings.Contains(es[4].ExtraInfo, "（qpriv.manage）") || !strings.Contains(es[4].ExtraInfo, "export.download") {
		t.Errorf("多按钮: %+v", es)
	}

	// GET 查看按钮不记；GET 的高危按钮（回滚语句）仍记
	if code, _ := bob.JSON("GET", "/api/v1/t/view", nil, nil); code != 403 {
		t.Fatalf("view: %d", code)
	}
	if n := len(denied()); n != 5 {
		t.Errorf("GET 查看按钮不应记: %d", n)
	}
	if code, _ := bob.JSON("GET", "/api/v1/t/danger-get", nil, nil); code != 403 {
		t.Fatalf("danger-get: %d", code)
	}
	if n := len(denied()); n != 6 {
		t.Errorf("GET 高危按钮应记: %d", n)
	}

	// 有权限的人 / 超级用户通过，不记
	if code, _ := root.JSON("POST", "/api/v1/t/manage", nil, nil); code != 200 {
		t.Fatalf("root: %d", code)
	}
	env.Repo.Perms[2] = map[string]bool{"qpriv.manage": true}
	if code, _ := bob.JSON("POST", "/api/v1/t/manage2", nil, nil); code != 200 {
		t.Fatalf("授予后: %d", code)
	}
	if n := len(denied()); n != 6 {
		t.Errorf("通过的请求不应记拒绝: %d", n)
	}
}

// 装配了按钮检查器后 ButtonOrFail 的 403 也记；去重按用户区分。
func TestNoPermissionAuditedWithChecker(t *testing.T) {
	box := &auditBox{}
	fb := &fakeButtons{allow: map[uint]map[string]bool{}}
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){func(s *server.Server, rt server.Routes) {
		s.SetButtonChecker(fb)
		s.SetAuditSink(box.add)
		rt.API.POST("/t/orfail", func(c *gin.Context) {
			if s.ButtonOrFail(c, "partman.run") {
				server.APIOK(c, "ok")
			}
		})
	}}})
	bob := env.Login("bob")
	if code, _ := bob.JSON("POST", "/api/v1/t/orfail", nil, nil); code != 403 {
		t.Fatalf("应 403: %d", code)
	}
	bob.JSON("POST", "/api/v1/t/orfail", nil, nil)
	es := box.named(audit.ActionNoPerm)
	if len(es) != 1 || !strings.Contains(es[0].ExtraInfo, "partman.run") {
		t.Fatalf("ButtonOrFail 的 403 应记且去重: %+v", es)
	}
	grant(fb, 2, "partman.run")
	if code, _ := bob.JSON("POST", "/api/v1/t/orfail", nil, nil); code != 200 {
		t.Fatalf("授予后: %d", code)
	}
	if n := len(box.named(audit.ActionNoPerm)); n != 1 {
		t.Errorf("通过不记: %d", n)
	}
}
