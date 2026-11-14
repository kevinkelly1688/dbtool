// @author: kevinkelly
// @license: Apache Licence
// @file: site_test.go
// @time: 2026/10/28

package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"dbtool/internal/config"
)

const siteTestKey = "unit-test-secret-key-unit-test-secret-key"

func TestSiteInfoOf(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"1.05", "v1.05"}, {"v1.05", "v1.05"}, {"dev", "dev"}, {"", ""}, {"2.0 正式版", "v2.0 正式版"},
	} {
		if got := siteInfoOf(&config.Config{Version: c.in}).Version; got != c.want {
			t.Errorf("版本 %q → %q，期望 %q", c.in, got, c.want)
		}
	}
	if got := siteInfoOf(&config.Config{ICP: "京ICP备1号"}).ICPURL; got != config.DefaultICPURL {
		t.Errorf("没配 icp_url 时应回落默认地址，得到 %q", got)
	}
}

// 版本号出现在登录后页面的侧栏底部，ICP 备案号（带链接）出现在登录页和登录后页面的底部；配置里的值要被转义。
func TestSiteInfoRendered(t *testing.T) {
	e := newEnvCfg(t, &config.Config{SecretKey: siteTestKey, Version: "1.05",
		ICP: "京ICP备12345678号-1<script>", ICPURL: "https://beian.miit.gov.cn/?a=1&b=2"})

	login := e.do(httptest.NewRequest("GET", "/login", nil)).Body.String()
	if !strings.Contains(login, `class="site-foot"`) || !strings.Contains(login, "京ICP备12345678号-1&lt;script&gt;") {
		t.Errorf("登录页应显示（转义后的）备案号")
	}
	if !strings.Contains(login, `href="https://beian.miit.gov.cn/?a=1&amp;b=2"`) || !strings.Contains(login, `rel="noopener noreferrer"`) {
		t.Errorf("备案号应是安全的外链")
	}
	if strings.Contains(login, "side-foot") {
		t.Errorf("登录页没有侧栏，不应出现版本号块")
	}

	sc, _, _ := e.login(t, "root", "pw-123")
	req := httptest.NewRequest("GET", "/query", nil)
	req.AddCookie(sc)
	body := e.do(req).Body.String()
	if !strings.Contains(body, `<b>v1.05</b>`) || !strings.Contains(body, `class="side-foot"`) {
		t.Errorf("登录后页面侧栏底部应显示版本号 v1.05")
	}
	if !strings.Contains(body, `class="site-foot"`) {
		t.Errorf("登录后页面底部应显示备案号")
	}
}

// 都没配时整块不输出，不留空的容器。
func TestSiteInfoOmittedWhenUnset(t *testing.T) {
	e := newEnv(t)
	sc, _, _ := e.login(t, "root", "pw-123")
	req := httptest.NewRequest("GET", "/query", nil)
	req.AddCookie(sc)
	body := e.do(req).Body.String()
	if strings.Contains(body, `class="side-foot"`) || strings.Contains(body, `class="site-foot"`) {
		t.Errorf("没配版本号 / 备案号时不应输出对应元素")
	}
}
