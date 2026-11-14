// @author: kevinkelly
// @license: Apache Licence
// @file: button_test.go
// @time: 2026/10/28

package slowlog

import (
	"testing"

	"dbtool/internal/btnperm/btnpermtest"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// 按钮级权限：查慢日志（slowlogs.view）与慢查日志（slowquery.view）两个页面分开；“获取优化建议”是单独的按钮。
func TestPageAndAdviseButtons(t *testing.T) {
	svc, _ := fixture()
	chk := btnpermtest.New()
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){chk.Hook,
		func(s *server.Server, rt server.Routes) { register(s, rt, svc) }}})
	grantServer(env, 2, "slowlogs.view", "slowquery.view", "slowquery.advise")
	bob := env.Login("bob")

	if code, _ := bob.Page("/slowlogs"); code != 403 {
		t.Errorf("没有 slowlogs.view: %d", code)
	}
	if code, _ := bob.JSON("GET", "/api/v1/slowlog/instances", nil, nil); code != 403 {
		t.Errorf("没有任何查看按钮的数据接口: %d", code)
	}
	chk.Grant(2, "slowlogs.view")
	if code, _ := bob.Page("/slowlogs"); code != 200 {
		t.Errorf("slowlogs.view 打开查慢日志: %d", code)
	}
	if code, _ := bob.Page("/slowquery"); code != 403 {
		t.Errorf("slowlogs.view 不能打开慢查日志: %d", code)
	}
	if code, _ := bob.JSON("GET", "/api/v1/slowlog/instances", nil, nil); code != 200 {
		t.Errorf("任一查看按钮即可取数据: %d", code)
	}
	if code, _ := bob.JSON("POST", "/api/v1/slowlog/advise", map[string]any{}, nil); code != 403 {
		t.Errorf("没有 slowquery.advise: %d", code)
	}
	chk.Grant(2, "slowquery.advise")
	if code, _ := bob.JSON("POST", "/api/v1/slowlog/advise", map[string]any{}, nil); code == 403 {
		t.Error("授予 slowquery.advise 后不应是 403")
	}
}
