// @author: kevinkelly
// @license: Apache Licence
// @file: shell_help_test.go
// @time: 2026/10/28

package server

import (
	"fmt"
	"io/fs"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"dbtool/internal/btnperm"
	"dbtool/web"
)

func helpMD(t *testing.T, id string) string {
	t.Helper()
	b, err := fs.ReadFile(web.FS, "static/help/"+id+".md")
	if err != nil {
		t.Fatalf("章节 %s 缺少 web/static/help/%s.md: %v", id, id, err)
	}
	return string(b)
}

// 目录里的每一章都有内容；章节之间的站内链接都指向存在的章节；内容里不能留着原型的演示措辞。
func TestHelpChapters(t *testing.T) {
	known := map[string]bool{}
	for _, tp := range helpTopics {
		if known[tp.ID] {
			t.Errorf("章节 id 重复: %s", tp.ID)
		}
		known[tp.ID] = true
	}
	if len(helpTopics) != 20 {
		t.Errorf("使用文档应有 20 章，实际 %d", len(helpTopics))
	}
	link := regexp.MustCompile(`\]\(#help/([a-z0-9_]+)\)`)
	for _, tp := range helpTopics {
		src := helpMD(t, tp.ID)
		if !strings.HasPrefix(src, "# ") || len(src) < 200 {
			t.Errorf("%s 章内容不完整", tp.ID)
		}
		if strings.Contains(src, "\r") {
			t.Errorf("%s 章应使用 LF 换行", tp.ID)
		}
		for _, bad := range []string{"原型", "演示", "🧪", "模拟", "Dba@2026"} {
			if strings.Contains(src, bad) {
				t.Errorf("%s 章仍含原型措辞 %q", tp.ID, bad)
			}
		}
		for _, m := range link.FindAllStringSubmatch(src, -1) {
			if !known[m[1]] {
				t.Errorf("%s 章链接到不存在的章节 %s", tp.ID, m[1])
			}
		}
	}
}

// 权限码清单章与按钮权限登记表一致（登记表增删码时，使用文档要同步更新）。
func TestHelpPermcodesInSync(t *testing.T) {
	re := regexp.MustCompile("(?m)^\\| `([a-z_0-9.]+)` \\|")
	doc := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(helpMD(t, "permcodes"), -1) {
		doc[m[1]] = true
	}
	reg := map[string]bool{}
	for _, c := range btnperm.Codes() {
		reg[c] = true
	}
	var missing, extra []string
	for c := range reg {
		if !doc[c] {
			missing = append(missing, c)
		}
	}
	for c := range doc {
		if !reg[c] {
			extra = append(extra, c)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("权限码清单与登记表不一致：使用文档缺少 %v；使用文档多出 %v", missing, extra)
	}
}

func TestHelpTopicFor(t *testing.T) {
	valid := map[string]bool{}
	for _, tp := range helpTopics {
		valid[tp.ID] = true
	}
	cases := map[string]string{
		"home": "quickstart", "sql": "sqlworkflow", "sqlanalyze": "optimize", "query": "query", "slowlogs": "slowlog", "export": "export",
		"applies": "qpriv", "privileges": "qpriv", "sqladvisor": "optimize", "slowquery": "slowlog", "perf-list": "perf", "perf-sla": "perf",
		"inst-list": "instance", "inst-params": "instance", "inst-inspect": "inspect", "sys-users": "sysadmin", "sys-auth-groups": "btnperm",
		"sys-audit": "sysadmin", "notify": "sysadmin", "help": "quickstart", "": "quickstart", "no-such-page": "quickstart",
	}
	for nav, want := range cases {
		if got := helpTopicFor(nav); got != want {
			t.Errorf("helpTopicFor(%q)=%q, want %q", nav, got, want)
		}
	}
	// 所有登记了图标的菜单键都能映射到存在的章节
	for key := range navIcons {
		if !valid[helpTopicFor(key)] {
			t.Errorf("菜单 %s 映射到不存在的章节 %s", key, helpTopicFor(key))
		}
	}
}

func TestHelpPageAndTopbar(t *testing.T) {
	e := newEnv(t)
	if w := e.do(httptest.NewRequest("GET", "/help", nil)); w.Code != 302 {
		t.Fatalf("未登录访问使用文档应跳转登录: %d", w.Code)
	}
	for _, user := range []string{"root", "bob"} { // 所有登录用户都能打开
		sc, _, _ := e.login(t, user, "pw-123")
		get := func(path string) string {
			req := httptest.NewRequest("GET", path, nil)
			req.AddCookie(sc)
			w := e.do(req)
			if w.Code != 200 {
				t.Fatalf("%s %s: %d", user, path, w.Code)
			}
			return w.Body.String()
		}
		body := get("/help")
		for _, want := range []string{`id="help"`, `data-topic="quickstart"`, `data-topic="permcodes"`, `/static/js/help/help.js`, `/static/css/help.css`, `id="help-q"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s 使用文档页缺少 %q", user, want)
			}
		}
		// 侧栏有“帮助中心 › 使用文档”；顶栏“? 使用文档”按当前页面跳到对应章节
		home := get("/")
		for _, want := range []string{`<span>帮助中心</span>`, `href="/help"`, `</b> 使用文档</a>`, `href="/help#help/quickstart"`, `id="um-btn"`, `data-skin-set="sepia"`, `action="/logout"`, `id="um-profile"`, `/static/js/shell/usermenu.js`} {
			if !strings.Contains(home, want) {
				t.Errorf("%s 首页缺少 %q", user, want)
			}
		}
		req := httptest.NewRequest("GET", "/query", nil)
		req.AddCookie(sc)
		if w := e.do(req); w.Code == 200 && !strings.Contains(w.Body.String(), `href="/help#help/query"`) {
			t.Errorf("%s 在线查询页的帮助按钮应指向 query 章节", user)
		}
	}
}

func TestHelpStaticServed(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"quickstart", "faq"} {
		w := e.do(httptest.NewRequest("GET", "/static/help/"+id+".md", nil))
		if w.Code != 200 || !strings.HasPrefix(w.Body.String(), "# ") {
			t.Errorf("/static/help/%s.md: %d", id, w.Code)
		}
	}
}

// S9-C5：Redis 上线规则是系统管理页面，帮助指向“系统管理”章节（原型 NAV_TREE 里是 sysadmin），不是 SQL 上线。
func TestHelpTopicForRedisRulesIsSysadmin(t *testing.T) {
	if got := helpTopicFor("sys-redis-rules"); got != "sysadmin" {
		t.Fatalf("sys-redis-rules 的帮助章节 = %q，期望 sysadmin", got)
	}
}

// S9-C6：权限码清单章顶部的汇总数字（模块 / 页面 / 码数 / 类型 / 仅超级用户）与登记表一致，各模块小标题里的数量也一致。
func TestHelpPermcodesSummaryMatchesRegistry(t *testing.T) {
	doc := helpMD(t, "permcodes")
	pages, v, o, d := 0, 0, 0, 0
	perModule := map[string]int{}
	for _, m := range btnperm.Modules() {
		pages += len(m.Pages)
		for _, p := range m.Pages {
			for _, b := range p.Buttons {
				perModule[m.Name]++
				switch b.Kind {
				case btnperm.KindView:
					v++
				case btnperm.KindOp:
					o++
				case btnperm.KindDanger:
					d++
				}
			}
		}
	}
	total, grantable, _ := btnperm.Counts()
	want := fmt.Sprintf("共 **%d** 个模块、**%d** 个页面、**%d** 个权限码：查看 %d 个、操作 %d 个、高危 %d 个；其中 %d 个仅超级用户，%d 个可授予权限组。",
		len(btnperm.Modules()), pages, total, v, o, d, total-grantable, grantable)
	if !strings.Contains(doc, want) {
		t.Errorf("权限码清单汇总行应为：%s", want)
	}
	for name, n := range perModule {
		if h := fmt.Sprintf("## %s（%d）", name, n); !strings.Contains(doc, h) {
			t.Errorf("权限码清单缺少小标题 %q", h)
		}
	}
}
