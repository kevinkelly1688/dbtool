// @author: kevinkelly
// @license: Apache Licence
// @file: home_handlers.go
// @time: 2026/10/28

package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/sqlflow"
)

// 首页（工作台）：当前用户的待办、最近的工单 / 申请、按权限生成的快捷入口。
// 数据全部来自已有服务的列表接口（可见范围与各自列表页一致）；某个服务未启用或用户无权访问时，对应区块不显示。

type homeCard struct {
	Label string
	Count int64
	Href  string
	Warn  bool // 有待办时高亮
}

type homeApply struct {
	ID          uint
	Title       string
	Instance    string
	StatusLabel string
	Class       string
	User        string
	Created     string
}

type homeData struct {
	Greeting string
	Today    string
	Cards    []homeCard
	Recent   []sqlflow.WorkflowItem
	Applies  []homeApply
	Quick    []NavLink
	HasPerf  bool // 有性能工单入口：页面脚本会请求 /api/v1/perf/ 接口补性能工单的 KPI 与页签

	KPIs        []homeKPI
	Approve     []homeTodo // 待我审批（最多几行）
	ApproveN    int        // 待我审批的总数
	ApproveMore int        // 列表里没列出的条数
	Mine        []homeTodo // 我提交的（进行中的工单 / 审批中的申请）
	MineN       int
	HasTodo     bool // 有任一待办来源：显示“我的待办”卡片
	Chart       *homeChart
	Insts       []homeInst
	InstN       int
	InstMore    int
	InstIDs     string // 可见实例的 ID（逗号分隔），页面脚本用来统计异常数
	Activity    []homeActivity
	ActAll      bool // 最近动态是全平台的（否则只有自己的）
	CanAudit    bool // 有“系统审计”入口
	Platform    []homeStat
}

func greeting(h int) string {
	switch {
	case h < 6:
		return "夜深了"
	case h < 12:
		return "早上好"
	case h < 14:
		return "中午好"
	case h < 19:
		return "下午好"
	}
	return "晚上好"
}

func (s *Server) homePage(c *gin.Context) {
	a := actorOf(c)
	p := principalOf(c)
	d := homeData{Greeting: greeting(time.Now().Hour())}

	if s.sql != nil && s.sql.CanAccessPage(a) {
		for _, w := range []struct{ label, status string }{
			{"待审核的 SQL 工单", model.WfManReviewing},
			{"可执行的 SQL 工单", model.WfReviewPass},
		} {
			_, total, err := s.sql.List(a, sqlflow.ListQuery{Status: w.status, Limit: 1})
			if err != nil {
				slog.Warn("首页统计工单失败", "status", w.status, "err", err)
				continue
			}
			d.Cards = append(d.Cards, homeCard{Label: w.label, Count: total, Href: "/sqlworkflow?status=" + w.status, Warn: total > 0 && w.status == model.WfManReviewing})
		}
		if rows, _, err := s.sql.List(a, sqlflow.ListQuery{Limit: 6}); err == nil {
			d.Recent = rows
		}
	}
	if s.apply != nil && s.apply.CanAccessPage(a) {
		if res, err := s.apply.List(a, "", 0, 100); err == nil {
			var waiting int64
			for _, r := range res.Rows {
				if r.Status == audit.StatusWaiting {
					waiting++
				}
			}
			d.Cards = append(d.Cards, homeCard{Label: "待处理的权限申请", Count: waiting, Href: "/query/applies", Warn: waiting > 0})
			for i, r := range res.Rows {
				if i >= 5 {
					break
				}
				d.Applies = append(d.Applies, homeApply{ID: r.ApplyID, Title: r.Title, Instance: r.InstanceName, StatusLabel: r.StatusLabel,
					Class: statusClassOf(r.Status), User: r.UserDisplay, Created: r.CreateTime.Format("2006-01-02 15:04")})
			}
		}
	}

	// ---- 待办聚合、KPI、近 7 日图、实例、最近动态、平台状态（取不到的块就不显示）----
	now := time.Now()
	db := homeDBFor(s)
	lang := langOf(c)
	d.Approve, d.ApproveN = s.homeApprove(a, lang)
	var mineSQL int
	d.Mine, mineSQL, d.MineN = s.homeMine(a, db, lang)
	sqlTodo := (s.sql != nil && s.sql.CanAccessPage(a)) || (s.apply != nil && s.apply.CanAccessPage(a))
	// 持有“查看数据归档”（archive.view）且登记了待审批来源（数据归档）的用户，即使没有 SQL 上线 / 权限申请页面权限也要有待办卡片。
	d.HasTodo = sqlTodo || (len(homeApproveHooks) > 0 && s.ActorHasButton(a, "archive.view"))
	if d.HasTodo {
		hint := ""
		if d.ApproveN == 0 {
			hint = "没有待审批事项"
		}
		d.KPIs = append(d.KPIs, homeKPI{ID: "k-approve", Label: "待我审批", Value: itoaHome(d.ApproveN), Hint: hint, Href: "#todo", Warn: d.ApproveN > 0})
	}
	if sqlTodo {
		if running, ok := s.homeVisibleRunning(a); ok {
			d.KPIs = append(d.KPIs, homeKPI{ID: "k-mine", Label: "进行中工单（我的 / 全部）", Value: itoaHome(mineSQL) + " / " + itoaHome(int(running)), Href: "/sqlworkflow"})
		} else {
			d.KPIs = append(d.KPIs, homeKPI{ID: "k-mine", Label: "我的进行中工单", Value: itoaHome(mineSQL), Href: "/sqlworkflow"})
		}
	}
	if chart, ok := s.homeTrend(a, now); ok {
		d.Chart = &chart
	}
	d.Insts, d.InstN, d.InstIDs = s.homeInstances(a)
	if d.InstN > len(d.Insts) {
		d.InstMore = d.InstN - len(d.Insts)
	}
	if d.ApproveN > len(d.Approve) {
		d.ApproveMore = d.ApproveN - len(d.Approve)
	}
	d.Activity, d.ActAll = s.homeRecent(a, db)
	d.CanAudit = s.ActorHasButton(a, "audit.view")
	d.Platform = s.homePlatform(a)
	d.Today = now.Format("2006-01-02")

	for _, g := range s.menuFor(a, "home") {
		if g.Key == "perf" {
			d.HasPerf = true
		}
		for _, it := range g.Items {
			if len(d.Quick) < 12 {
				d.Quick = append(d.Quick, it)
			}
		}
	}
	if d.HasPerf {
		d.KPIs = append(d.KPIs, homeKPI{ID: "k-perf", Label: "性能工单待办", Value: "—", Hint: "加载中…", Href: "/perf"})
	}
	if d.InstN > 0 {
		// 卡片点进去看实例：有实例列表权限去实例列表，没有就退到在线查询（所有登录用户都能进）；巡检有告警时由 home.js 改指向巡检列表
		href := "/query"
		if s.ActorHasButton(a, "instance.view") {
			href = "/instances"
		}
		d.KPIs = append(d.KPIs, homeKPI{ID: "k-inst", Label: "我可见的实例", Value: itoaHome(d.InstN), Href: href})
	}
	s.render(c, http.StatusOK, "home.html", pageData{Title: "首页", Nav: "home", Username: p.Username, Display: p.Display,
		CSRF: sessionOf(c).CSRF, Data: d})
}

// statusClassOf 与模板函数 statusClass 一致（审批状态 → 样式类）。
func statusClassOf(st int) string {
	switch st {
	case audit.StatusWaiting:
		return "waiting"
	case audit.StatusPassed:
		return "passed"
	case audit.StatusRejected:
		return "rejected"
	}
	return "aborted"
}

func itoaHome(n int) string { return strconv.Itoa(n) }
