// @author: kevinkelly
// @license: Apache Licence
// @file: help.go
// @time: 2026/10/28

package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// 使用文档：章节是 web/static/help/<id>.md（唯一来源，由 web/static/js/help/ 在浏览器里渲染），
// 页面本身只负责给出目录和“当前页面 → 文档章节”的映射。

type helpTopic struct{ Group, ID, Title string }

// helpTopics 是目录，顺序即显示顺序；id 必须有同名的 .md 文件（help_test.go 校验）。
var helpTopics = []helpTopic{
	{"入门", "quickstart", "快速入门"},
	{"入门", "login", "登录、两步验证与账号安全"},
	{"变更发布", "sqlworkflow", "变更工单（SQL 上线）"},
	{"数据服务", "query", "在线查询"},
	{"数据服务", "export", "数据导出"},
	{"数据服务", "archive", "数据归档"},
	{"数据服务", "partman", "分区维护"},
	{"数据服务", "qpriv", "权限申请与授权清单"},
	{"性能优化", "optimize", "SQL 体检与智能调优"},
	{"性能优化", "slowlog", "慢日志检索与慢 SQL 洞察"},
	{"性能治理", "perf", "性能工单"},
	{"实例运维", "instance", "实例管理"},
	{"实例运维", "inspect", "性能巡检"},
	{"平台管理", "sysadmin", "管理员指南"},
	{"平台管理", "masking", "数据脱敏"},
	{"平台管理", "firewall", "SQL 防火墙"},
	{"平台管理", "btnperm", "按钮级权限"},
	{"平台管理", "duty", "值班表"},
	{"附录", "permcodes", "权限码清单"},
	{"附录", "faq", "常见问题"},
}

// helpByNav 是菜单 Key → 文档章节（对应原型 perm.js 的 NAV_TREE.help）。没登记的页面按前缀推断，再不行回到快速入门。
var helpByNav = map[string]string{
	"home": "quickstart", "help": "quickstart",
	"sql": "sqlworkflow", "sqlanalyze": "optimize",
	"query": "query", "slowlogs": "slowlog", "partman": "partman", "archive": "archive", "export": "export", "applies": "qpriv", "privileges": "qpriv",
	"sqladvisor": "optimize", "slowquery": "slowlog",
	"inst-binlog":     "instance",
	"inst-inspect":    "inspect",
	"sys-auth-groups": "btnperm",
	"sys-redis-rules": "sysadmin",
	"sys-masking":     "masking",
	"sys-firewall":    "firewall",
	"sys-config":      "sysadmin",
	"sys-duty":        "duty",
}

var helpByPrefix = []struct{ prefix, topic string }{
	{"perf", "perf"},
	{"inst", "instance"},
	{"sys", "sysadmin"},
	{"notify", "sysadmin"},
}

func helpTopicFor(nav string) string {
	if t, ok := helpByNav[nav]; ok {
		return t
	}
	for _, p := range helpByPrefix {
		if strings.HasPrefix(nav, p.prefix) {
			return p.topic
		}
	}
	return "quickstart"
}

type helpGroup struct {
	Name   string
	Topics []helpTopic
}

type helpData struct {
	Groups []helpGroup
}

func init() {
	addRoutes(func(s *Server, rt Routes) { rt.Pages.GET("/help", s.helpPage) })
}

func (s *Server) helpPage(c *gin.Context) {
	p := principalOf(c)
	var d helpData
	for _, t := range helpTopics {
		if n := len(d.Groups); n == 0 || d.Groups[n-1].Name != t.Group {
			d.Groups = append(d.Groups, helpGroup{Name: t.Group})
		}
		g := &d.Groups[len(d.Groups)-1]
		g.Topics = append(g.Topics, t)
	}
	s.render(c, http.StatusOK, "help.html", pageData{Title: "使用文档", Nav: "help", Username: p.Username, Display: p.Display,
		CSRF: sessionOf(c).CSRF, Data: d})
}
