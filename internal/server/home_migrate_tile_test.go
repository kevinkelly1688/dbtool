// @author: kevinkelly
// @license: Apache Licence
// @file: home_migrate_tile_test.go
// @time: 2026/10/28

package server_test

import (
	"strings"
	"testing"

	"dbtool/internal/btnperm"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// 首页“平台状态”里的权限迁移提醒：权限还没迁移（没执行过 dbt-admin perm migrate）时，只给超级用户一项“按钮权限迁移”；
// 迁移过（mode=strict）后不再显示；普通用户任何时候都看不到；没有按钮权限检查器（演示 / 测试）时也不显示。
func TestHomeMigrationReminder(t *testing.T) {
	const tile = "按钮权限迁移"
	cases := []struct {
		name    string
		mode    string
		checker bool
		root    bool
		want    bool
	}{
		{"没迁移，超级用户看到", btnperm.ModeCompat, true, true, true},
		{"没迁移，普通用户看不到", btnperm.ModeCompat, true, false, false},
		{"已迁移，超级用户看不到", btnperm.ModeStrict, true, true, false},
		{"没有检查器，超级用户看不到", btnperm.ModeCompat, false, true, false},
	}
	for _, c := range cases {
		fb := &fakeButtons{allow: map[uint]map[string]bool{}, set: btnperm.Setting{Mode: c.mode, DenyDisplay: btnperm.DenyHide}}
		var hooks []func(*server.Server, server.Routes)
		if c.checker {
			hooks = append(hooks, func(s *server.Server, _ server.Routes) { s.SetButtonChecker(fb) })
		}
		env := servertest.New(t, servertest.Options{Hooks: hooks})
		user := "bob"
		if c.root {
			user = "root"
		}
		code, body := env.Login(user).Page("/")
		if code != 200 {
			t.Errorf("%s：首页 %d", c.name, code)
			continue
		}
		if got := strings.Contains(body, tile); got != c.want {
			t.Errorf("%s：首页含“%s”= %v，期望 %v", c.name, tile, got, c.want)
		}
		if c.want && !strings.Contains(body, "dbt-admin perm migrate") {
			t.Errorf("%s：提醒里应写明要执行的命令", c.name)
		}
		if strings.Contains(body, "按钮权限模式") {
			t.Errorf("%s：旧的“按钮权限模式”不应再出现", c.name)
		}
	}
}
