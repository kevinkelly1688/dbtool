// @author: kevinkelly
// @license: Apache Licence
// @file: home_test.go
// @time: 2026/10/28

package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHomePage(t *testing.T) {
	e := newEnv(t)

	// 未登录访问首页 → 跳登录页并带 next=/
	w := e.do(httptest.NewRequest("GET", "/", nil))
	if w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), "/login?next=") {
		t.Fatalf("未登录应跳转登录: %d %s", w.Code, w.Header().Get("Location"))
	}

	// 登录后：侧栏导航 + 首页内容 + 按权限生成的快捷入口
	sc, _, _ := e.login(t, "root", "pw-123")
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(sc)
	w = e.do(req)
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("首页应 200: %d", w.Code)
	}
	for _, want := range []string{`<aside class="side"`, `class="nav-home on"`, "数据服务", `href="/query"`, "快捷入口", `class="topbar"`} {
		if !strings.Contains(body, want) {
			t.Errorf("首页缺少 %q", want)
		}
	}
	// 侧栏必须在 </header> 之前（性能工单的菜单测试按这个位置截取导航区域）
	if strings.Index(body, `<aside class="side"`) > strings.Index(body, "</header>") {
		t.Error("侧栏应位于顶栏之前")
	}

	// 普通用户也能进首页，看到的是自己有权限的入口（在线查询对所有登录用户开放）
	sc, _, _ = e.login(t, "bob", "pw-123")
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(sc)
	w = e.do(req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `href="/query"`) || strings.Contains(w.Body.String(), `href="/settings/notify"`) {
		t.Errorf("普通用户首页: %d", w.Code)
	}
}

func TestLoginPageLayout(t *testing.T) {
	e := newEnv(t)
	w := e.do(httptest.NewRequest("GET", "/login", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "lgn-brand") || !strings.Contains(body, "lgn-pane") || strings.Contains(body, `<aside class="side"`) {
		t.Errorf("登录页应为左右分栏且没有侧栏: %d", w.Code)
	}
	for _, want := range []string{`/static/css/login.css`, `/static/js/auth/login.js`, `id="login-clock"`, `id="lgn-db"`, "每一条 SQL", "欢迎登录", "数据脱敏", "SQL 防火墙", "MySQL 系列",
		`data-skin-set="dark"`, `data-lang-set="en"`, `type="submit">登录</button>`} {
		if !strings.Contains(body, want) {
			t.Errorf("登录页缺少 %q", want)
		}
	}
}

func TestBuildChartBars(t *testing.T) {
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.Local)
	day := func(i int) time.Time { return now.AddDate(0, 0, i-6) }
	c := buildChart([]int{0, 1, 2, 0, 0, 100, 3}, now, day)
	if len(c.Bars) != 7 || c.Total != 106 || c.Bars[0].Label != "10-01" || c.Bars[6].Label != "10-07" || !c.Bars[6].Today || c.Bars[5].Today {
		t.Fatalf("柱图: %+v", c)
	}
	if c.Bars[0].H != 0 || c.Bars[1].H < 3 {
		t.Errorf("0 张不画柱，有数据的最矮 3px: %d %d", c.Bars[0].H, c.Bars[1].H)
	}
	tall := c.Bars[5]
	for i, b := range c.Bars {
		if b.H > tall.H || b.Y+b.H != tall.Y+tall.H || b.X < 0 || b.X+b.W > c.W {
			t.Errorf("第 %d 根柱子坐标不合理: %+v", i, b)
		}
	}
	if z := buildChart([]int{0, 0, 0, 0, 0, 0, 0}, now, day); z.Total != 0 || z.Bars[3].H != 0 {
		t.Errorf("全 0: %+v", z)
	}
	if clipHome("  "+strings.Repeat("字", 70), 60) != strings.Repeat("字", 60)+"…" || clipHome("abc", 60) != "abc" {
		t.Error("clipHome")
	}
}
