// @author: kevinkelly
// @license: Apache Licence
// @file: menu.go
// @time: 2026/10/28

package server

import (
	"fmt"
	"sort"

	"dbtool/internal/audit"
)

// 侧栏菜单：分组 + 条目，各模块在 init() 里用 addMenu 登记自己的入口，
// 谁能看到哪个入口由 Show 决定（nil 表示所有登录用户可见）。分组内按 Order 升序。

type menuGroupDef struct{ Key, Label string }

// 分组顺序即侧栏顺序。分组键（check / query / opt / perf / inst）沿用旧名，只有显示名换了；
// 原来的“系统管理”按用途拆成 access（访问控制）/ secure（安全合规）/ plat（平台设置）三组。
var menuGroups = []menuGroupDef{
	{"check", "变更发布"},
	{"query", "数据服务"},
	{"opt", "性能优化"},
	{"perf", "性能治理"},
	{"inst", "实例运维"},
	{"access", "访问控制"},
	{"secure", "安全合规"},
	{"plat", "平台设置"},
	{"help", "帮助中心"},
}

type menuItem struct {
	Group string
	Key   string // 与 pageData.Nav 对应，用来高亮当前页
	Href  string
	Label string
	Order int
	Show  func(s *Server, a audit.Actor) bool
}

var menuItems []menuItem

func addMenu(m menuItem) { menuItems = append(menuItems, m) }

// NavLink / NavGroup 是渲染给模板的结果。
type NavLink struct {
	Key, Href, Label string
	Icon             string // 侧栏条目前的小图标（字符），见 navIcons
	On               bool
	Badge            string // 右侧角标文字（待办数，空表示不显示），见 navbadge.go
	BadgeWarn        bool   // 角标用橙色
	BadgeTitle       string // 角标的鼠标悬停提示
}

type NavGroup struct {
	Key, Label string
	On         bool
	Items      []NavLink
	Badge      string // 组内条目角标之和：分组折叠时显示在组标题上
	BadgeWarn  bool
}

// navIcons 是侧栏条目的小图标，按菜单 Key 取；没登记的条目不显示图标。
var navIcons = map[string]string{
	"sql": "⎘", "sqlanalyze": "⌕",
	"query": "⌘", "slowlogs": "◷", "partman": "◫", "archive": "⇱", "export": "⇩", "applies": "✎", "privileges": "⚿",
	"sqladvisor": "⚡", "slowquery": "☰",
	"perf-list": "⚑", "perf-board": "♛", "perf-collect": "⇲", "perf-team": "☺", "perf-sla": "⏱",
	"inst-list": "▤", "inst-sessions": "⇄", "inst-databases": "⛁", "inst-accounts": "♙", "inst-params": "⚙", "inst-inspect": "✚",
	"inst-binlog": "▦", "sys-audit-flow": "⇶", "sys-res-group": "⧉", "sys-users": "♟", "sys-auth-groups": "⛨", "sys-auth-providers": "⚷",
	"sys-security": "⛊", "sys-config": "☷", "sys-redis-rules": "⚖", "sys-env": "◈", "sys-audit": "☑", "sys-goinception": "⌬", "sys-masking": "◐", "sys-firewall": "⊘", "sys-duty": "◴", "notify": "✉",
	"help": "?",
}

func superOnly(_ *Server, a audit.Actor) bool { return a.IsSuperuser }

func init() {
	addMenu(menuItem{"check", "sql", "/sqlworkflow", "变更工单", 10, func(s *Server, a audit.Actor) bool {
		return s.sql != nil && (s.sql.CanAccessPage(a) || s.sql.CanSubmit(a) || s.sql.CanExport(a))
	}})
	addMenu(menuItem{"query", "query", "/query", "在线查询", 10, nil})
	// 入口是导出工单列表；只有提交权限、没有查看权限的人，列表页会把他带到提交页（见 sqlExportListPage）
	addMenu(menuItem{"query", "export", "/sqlworkflow/export", "数据导出", 20, func(s *Server, a audit.Actor) bool {
		if s.sql == nil {
			return false
		}
		if v, err := s.sql.ExportViewOf(a); err == nil && (v.Single || v.Batch) {
			return true
		}
		return s.sql.CanExport(a)
	}})
	addMenu(menuItem{"query", "applies", "/query/applies", "权限申请", 60, func(s *Server, a audit.Actor) bool {
		return s.apply != nil && s.apply.CanAccessPage(a)
	}})
	addMenu(menuItem{"query", "privileges", "/query/privileges", "授权清单", 70, func(s *Server, a audit.Actor) bool {
		return s.apply != nil && s.apply.CanAccessPrivPage(a)
	}})
	addMenu(menuItem{"plat", "notify", "/settings/notify", "通知渠道", 20, func(s *Server, a audit.Actor) bool {
		return s.admin != nil && a.IsSuperuser
	}})
	addMenu(menuItem{"help", "help", "/help", "使用文档", 10, nil})
}

// menuFor 生成当前用户可见的菜单；active 是当前页的 Nav 键。没有可见条目的分组不显示。
// 登记了角标的条目（navbadge.go）会带上“等我处理的数量”；角标算不出来时只是不显示。
func (s *Server) menuFor(a audit.Actor, active string) []NavGroup {
	items := append([]menuItem(nil), menuItems...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].Order < items[j].Order })
	var out []NavGroup
	var badgeKeys []string
	for _, g := range menuGroups {
		ng := NavGroup{Key: g.Key, Label: g.Label}
		for _, it := range items {
			if it.Group != g.Key || (it.Show != nil && !it.Show(s, a)) {
				continue
			}
			on := it.Key == active
			ng.On = ng.On || on
			ng.Items = append(ng.Items, NavLink{Key: it.Key, Href: it.Href, Label: it.Label, Icon: navIcons[it.Key], On: on})
			if _, ok := navBadgeDefs[it.Key]; ok {
				badgeKeys = append(badgeKeys, it.Key)
			}
		}
		if len(ng.Items) > 0 {
			out = append(out, ng)
		}
	}
	if len(badgeKeys) == 0 {
		return out
	}
	badges := s.navBadges(a, badgeKeys)
	for gi := range out {
		sum, warn := 0, false
		for ii := range out[gi].Items {
			it := &out[gi].Items[ii]
			if e, ok := badges[it.Key]; ok {
				it.Badge, it.BadgeWarn = badgeText(e.n), e.warn
				it.BadgeTitle = fmt.Sprintf(navBadgeDefs[it.Key].title, e.n)
				sum += e.n
				warn = warn || e.warn
			}
		}
		if sum > 0 {
			out[gi].Badge, out[gi].BadgeWarn = badgeText(sum), warn
		}
	}
	return out
}
