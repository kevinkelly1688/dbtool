// @author: kevinkelly
// @license: Apache Licence
// @file: shell_http_test.go
// @time: 2026/10/28

package server_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// 个人菜单的数据接口：未登录 401；本地账号可改密码；仓库不是真库时只给基本信息。
func TestMyProfile(t *testing.T) {
	e := newSQLEnv(t)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/me/profile", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("未登录: %d", rec.Code)
	}

	dev := e.login(t, "dev")
	for _, path := range []string{"/api/v1/me/profile", "/api/v1/me/profile?full=1"} {
		w := dev.do("GET", path, nil)
		d := dataOf(t, w)
		if w.Code != 200 || d["username"] != "dev" || d["display"] != "开发者" || d["superuser"] != false {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		am, _ := d["auth_methods"].([]any)
		if len(am) != 1 || am[0] != "本地" || d["can_change_password"] != true {
			t.Errorf("%s 认证方式: %v / %v", path, d["auth_methods"], d["can_change_password"])
		}
		if strings.Contains(w.Body.String(), "password") && !strings.Contains(w.Body.String(), "can_change_password") {
			t.Error("响应不能带密码字段")
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Error("个人信息不应被缓存")
		}
	}
}

var navBadgeRe = regexp.MustCompile(`href="/sqlworkflow"[^>]*><span class="gl">[^<]*</span>变更工单<span class="badge nb( warn)?" title="[^"]*">(\d+)</span>`)

// 侧栏“变更工单”的待我审批角标：轮到我审批的人看得到，提交人看不到。
func TestNavBadgeSQLPending(t *testing.T) {
	e := newSQLEnv(t)
	dev := e.login(t, "dev")
	for i := 0; i < 2; i++ {
		if w := dev.do("POST", "/api/v1/sqlworkflow", submitBody("update orders set a=1 where id=1")); w.Code != 200 {
			t.Fatalf("提交: %d %s", w.Code, w.Body.String())
		}
	}
	// 登录过程会渲染页面并缓存角标（15 秒），所以要等工单提交之后再登录审批人
	leader := e.login(t, "leader")
	m := navBadgeRe.FindStringSubmatch(leader.get("/sqlworkflow").Body.String())
	if m == nil || m[2] != "2" {
		t.Errorf("组长应看到 2 个待审批角标: %v", m)
	}
	if body := dev.get("/sqlworkflow").Body.String(); strings.Contains(body, `class="badge nb`) {
		t.Error("提交人没有待审批，不应显示角标")
	}
}
