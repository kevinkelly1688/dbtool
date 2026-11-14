// @author: kevinkelly
// @license: Apache Licence
// @file: button_test.go
// @time: 2026/10/28

package instmgr

import (
	"testing"

	"dbtool/internal/btnperm/btnpermtest"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// 每个接口只认自己的按钮：没有该按钮一律 403，授予后放行；账号 / 数据库 / 测试连接这些按钮之间互不替代。
func TestFinerButtonsAreChecked(t *testing.T) {
	e := fullEnv()
	chk := btnpermtest.New()
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){
		chk.Hook, func(s *server.Server, rt server.Routes) { registerWith(s, rt, e.svc) },
	}})
	bob := env.Login("bob")

	cases := []struct{ method, path, button string }{
		{"POST", "/api/v1/instances/1/accounts", "account.create"},
		{"PUT", "/api/v1/instances/1/accounts/info", "account.edit"},
		{"POST", "/api/v1/instances/1/accounts/grant/preview", "account.grant"},
		{"POST", "/api/v1/instances/1/accounts/grant", "account.grant"},
		{"POST", "/api/v1/instances/1/accounts/password", "account.pwd"},
		{"POST", "/api/v1/instances/1/accounts/lock", "account.lock"},
		{"POST", "/api/v1/instances/1/accounts/drop", "account.delete"},
		{"GET", "/api/v1/instances/1/accounts/objects", "account.grant"},
		{"POST", "/api/v1/instances/1/databases", "database.create"},
		{"PUT", "/api/v1/instances/1/databases/info", "database.edit"},
		{"POST", "/api/v1/instances/test", "instance.test"},
		{"POST", "/api/v1/instances/1/test", "instance.test"},
	}
	for _, c := range cases {
		if code, _ := bob.JSON(c.method, c.path, map[string]any{}, nil); code != 403 {
			t.Errorf("%s %s：没有按钮 %s 应 403，得到 %d", c.method, c.path, c.button, code)
		}
		chk.Grant(2, c.button)
		if code, _ := bob.JSON(c.method, c.path, map[string]any{}, nil); code == 403 {
			t.Errorf("%s %s：授予 %s 后不应再是 403", c.method, c.path, c.button)
		}
		chk.Revoke(2, c.button)
	}
	// 测试连接：instance.add / instance.edit 也算
	chk.Grant(2, "instance.edit")
	if code, _ := bob.JSON("POST", "/api/v1/instances/test", map[string]any{}, nil); code == 403 {
		t.Error("instance.edit 应能测试连接")
	}
	// caps 以按钮码为键
	chk.Grant(2, "account.pwd")
	var caps map[string]bool
	bob.GetJSON("/api/v1/instmgr/caps", &caps)
	if !caps["account.pwd"] || caps["account.delete"] || !caps["instance.edit"] {
		t.Errorf("caps 里的按钮标记: %v", caps)
	}
}
