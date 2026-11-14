// @author: kevinkelly
// @license: Apache Licence
// @file: button_test.go
// @time: 2026/10/28

package sysadmin

import (
	"testing"

	"dbtool/internal/btnperm"
	"dbtool/internal/btnperm/btnhttp"
	"dbtool/internal/btnperm/btnpermtest"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// 审计页的“导出”与“查看”是两个按钮；审计员有 audit.view、没有 audit.export 时不能导出。
func TestAuditExportButton(t *testing.T) {
	svc, _ := setup()
	chk := btnpermtest.New()
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){chk.Hook, registerWith(svc)}})
	chk.Grant(2, permAuditUser)
	bob := env.Login("bob")
	if w := bob.Do("GET", "/api/v1/audit/log/export", nil); w.Code != 403 {
		t.Errorf("没有 audit.export: %d", w.Code)
	}
	if w := bob.Do("GET", "/api/v1/audit/options", nil); w.Code != 200 {
		t.Errorf("有 audit.view 可以查看: %d", w.Code)
	}
	chk.Grant(2, "audit.export")
	if w := bob.Do("GET", "/api/v1/audit/log/export", nil); w.Code != 200 {
		t.Errorf("授予 audit.export 后: %d", w.Code)
	}
}

// stubSource 让解析器有一个可加载的用户，用来观察缓存是否被写操作清空。
type stubSource struct{ btnperm.Source }

func (stubSource) Setting() (btnperm.Setting, error) { return btnperm.DefaultSetting(), nil }
func (stubSource) Profile(uint) (*btnperm.Profile, error) {
	return &btnperm.Profile{Active: true, GroupSource: map[string][]string{}}, nil
}
func (stubSource) DjangoHas(uint, string) (bool, error)      { return false, nil }
func (stubSource) DjangoCodes(uint) (map[string]bool, error) { return map[string]bool{}, nil }

// 改变用户 / 权限组的写操作成功后，按钮权限缓存整体失效；失败的写操作和读操作不触发。
func TestWriteInvalidatesButtonCache(t *testing.T) {
	svc, _ := setup()
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){registerWith(svc)}})
	res := btnperm.New(stubSource{})
	btnperm.SetDefault(res)
	defer btnperm.SetDefault(nil)
	root := env.Login("root")
	prime := func() {
		if _, err := res.HasButton(2, "sqlworkflow.view_own"); err != nil || res.CacheSize() != 1 {
			t.Fatalf("预热缓存失败: %v size=%d", err, res.CacheSize())
		}
	}

	prime()
	if w := root.Do("GET", "/api/v1/settings/users", nil); w.Code != 200 || res.CacheSize() != 1 {
		t.Fatalf("读操作不应清缓存: %d size=%d", w.Code, res.CacheSize())
	}
	if w := root.Do("POST", "/api/v1/settings/auth-groups", map[string]any{"name": "", "perm_codes": []string{}}); w.Code == 200 || res.CacheSize() != 1 {
		t.Fatalf("失败的写操作不应清缓存: %d size=%d", w.Code, res.CacheSize())
	}
	if w := root.Do("POST", "/api/v1/settings/auth-groups", map[string]any{"name": "新权限组", "perm_codes": []string{}}); w.Code != 200 {
		t.Fatalf("创建权限组: %d %s", w.Code, w.Body.String())
	}
	if res.CacheSize() != 0 {
		t.Fatal("成功的写操作应使按钮权限缓存失效")
	}
}

// 按钮权限接口与系统管理的路由放在同一个服务里不冲突（gin 的路由树对同前缀的通配符很敏感）。
func TestRoutesCoexistWithButtonPerms(t *testing.T) {
	svc, _ := setup()
	res := btnperm.New(stubSource{})
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){registerWith(svc),
		btnhttp.Register(btnperm.NewService(nil, res), res)}})
	root := env.Login("root")
	if w := root.Do("GET", "/api/v1/settings/users", nil); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	if w := root.Do("GET", "/api/v1/button-perms/me", nil); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
