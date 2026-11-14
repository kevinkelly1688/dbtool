// @author: kevinkelly
// @license: Apache Licence
// @file: home_data.go
// @time: 2026/10/28

package server

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"dbtool/internal/audit"
	"dbtool/internal/btnperm"
	"dbtool/internal/i18n"
	"dbtool/internal/model"
	"dbtool/internal/query"
	"dbtool/internal/sqlflow"
)

// 首页（工作台）的数据聚合：待办、近 7 日工单图、实例列表、最近动态、平台状态。
// 数据来自各服务已有的列表 / 统计接口（可见范围与各自列表页一致）；任何一块取不到（服务未启用、无权限、查询失败）
// 就不显示那一块，不影响首页其余部分。性能工单、实例健康（已缓存的巡检结果）由页面脚本通过各模块自己的接口异步补上。

// homeTodo 是待办聚合里的一行。
type homeTodo struct {
	Kind string // SQL 上线 / 查询权限
	Tag  string // 样式：brand
	Title,
	Sub,
	Href string
	Warn bool
}

type homeKPI struct {
	ID    string // 页面脚本补数据时用
	Label string
	Value string
	Hint  string
	Href  string
	Warn  bool
}

type homeBar struct {
	Label      string
	N          int
	X, Y, W, H int
	CX, NY, LY int
	Today      bool
}

// homeChart 是近 7 日工单柱图（服务端算好坐标，模板直接画 SVG，不引第三方库）。
type homeChart struct {
	W, H      int
	Bars      []homeBar
	Total     int
	Truncated bool // 数据量太大，只统计了最近一批
}

type homeInst struct {
	ID       uint
	Name     string
	DBType   string
	Role     string
	Env      string
	EnvLabel string
	EnvProd  bool
}

type homeActivity struct {
	User   string
	Action string
	Extra  string
	At     string
	Mine   bool
}

type homeStat struct {
	Label string
	Value string
	Class string // ok / warn / info
	Href  string
}

// homeDB 是首页要直接读的两张表（我提交的工单、审计日志）。真实环境由元数据库实现，测试里换假实现。
type homeDB interface {
	MyWorkflows(username string, limit int) ([]homeWorkflowRow, int64, error)
	RecentAudit(username string, limit int) ([]model.AuditEntry, error)
}

type homeWorkflowRow struct {
	ID           uint
	Name         string
	InstanceName string
	DBName       string
	Status       string
	CreateTime   time.Time
}

// homeDBFor 返回首页用的数据库读取器；仓库不是元数据库实现（测试 / 演示）时为 nil。测试里可以替换。
var homeDBFor = func(s *Server) homeDB {
	db, _, ok := s.DB()
	if !ok || db == nil {
		return nil
	}
	return gormHomeDB{db}
}

type gormHomeDB struct{ db *gorm.DB }

// 进行中的 SQL 工单状态：等待审批、审核通过待执行、定时执行、排队、执行中、执行有异常。
var homeActiveStatuses = []string{model.WfManReviewing, model.WfReviewPass, model.WfTimingTask, model.WfQueuing, model.WfExecuting, model.WfException}

func (g gormHomeDB) MyWorkflows(username string, limit int) ([]homeWorkflowRow, int64, error) {
	q := g.db.Table("dbt_sql_workflow AS w").Where("w.engineer = ? AND w.status IN ?", username, homeActiveStatuses)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []homeWorkflowRow
	err := q.Select("w.id AS id, w.workflow_name AS name, i.instance_name AS instance_name, w.db_name AS db_name, w.status AS status, w.create_time AS create_time").
		Joins("LEFT JOIN dbt_sql_instance AS i ON i.id = w.instance_id").Order("w.id DESC").Limit(limit).Scan(&rows).Error
	return rows, total, err
}

func (g gormHomeDB) RecentAudit(username string, limit int) ([]model.AuditEntry, error) {
	q := g.db.Model(&model.AuditEntry{})
	if username != "" {
		q = q.Where("user_name = ?", username)
	}
	var out []model.AuditEntry
	err := q.Order("id DESC").Limit(limit).Find(&out).Error
	return out, err
}

// auditLookup 是首页判断“这张单现在是不是等我审批”需要的仓库能力（store.Store 实现）。
type auditLookup interface {
	audit.Repo
	AuditByWorkflow(workflowType int, workflowID uint, forUpdate bool) (*model.WorkflowAudit, error)
}

// reviewMemo 复用“同一审批节点 × 资源组 × 是否自己提交”的判定结果，避免每张单都查一遍。
type reviewMemo struct {
	repo auditLookup
	who  audit.Actor
	m    map[string]bool
}

func (r *reviewMemo) can(typ int, id uint) bool {
	au, err := r.repo.AuditByWorkflow(typ, id, false)
	if err != nil || au == nil || au.CurrentStatus != audit.StatusWaiting {
		return false
	}
	k := fmt.Sprintf("%s|%d|%t", au.CurrentAudit, au.GroupID, au.CreateUser == r.who.Username)
	if v, ok := r.m[k]; ok {
		return v
	}
	v := audit.CanReview(r.repo, au, r.who)
	r.m[k] = v
	return v
}

const homeListMax = 8 // 每个页签最多列几行

// HomeTodoRow 是其他模块汇入首页“待我审批”的一行。
type HomeTodoRow struct {
	Kind, Title, Sub, Href string
}

// HomeApproveFunc 返回当前用户在某个模块里“等我审批”的单据：最多 limit 行用来展示，total 是实际总数（可大于行数）。
// 模块自己判断可见范围与权限（没有权限返回 0）；返回错误或 panic 只是这一块不显示。
type HomeApproveFunc func(s *Server, a audit.Actor, limit int) (rows []HomeTodoRow, total int, err error)

var homeApproveHooks []HomeApproveFunc

// AddHomeApprove 登记模块的“待我审批”来源（在 init() 里调用）；sqlflow / 权限申请是首页内置的，不走这里。
func AddHomeApprove(fn HomeApproveFunc) { homeApproveHooks = append(homeApproveHooks, fn) }

// homeApprove 返回“等我审批”的 SQL 上线工单与查询权限申请（最多检查最新的 100 张等待审批的单据），再加上各模块登记的来源（数据归档等）。
func (s *Server) homeApprove(a audit.Actor, lang string) (rows []homeTodo, total int) {
	add := func(t homeTodo) {
		total++
		if len(rows) < homeListMax {
			rows = append(rows, t)
		}
	}
	if al, ok := s.repo.(auditLookup); ok {
		s.homeApproveBuiltin(a, al, add, lang)
	}
	for _, fn := range homeApproveHooks {
		s.runHomeHook(fn, a, lang, &rows, &total)
	}
	return rows, total
}

// homeSubL 翻译模块登记的待办副标题（形如“实例 / 库.表 · 归档并删除 · 提交人 · 时间”）：
// 按“ · ”分段，逐段精确查目录，用户数据（实例名、人名、时间）查不到就原样保留；中文请求原样返回。
func homeSubL(lang, sub string) string {
	if lang == i18n.ZH || sub == "" {
		return sub
	}
	parts := strings.Split(sub, " · ")
	for i, p := range parts {
		parts[i] = i18n.TP(lang, p)
	}
	return strings.Join(parts, " · ")
}

func (s *Server) runHomeHook(fn HomeApproveFunc, a audit.Actor, lang string, rows *[]homeTodo, total *int) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("首页待我审批的模块来源异常", "panic", r)
		}
	}()
	list, n, err := fn(s, a, homeListMax)
	if err != nil {
		slog.Warn("首页统计模块待我审批失败", "err", err)
		return
	}
	if n < len(list) {
		n = len(list)
	}
	*total += n
	for _, r := range list {
		if len(*rows) < homeListMax {
			*rows = append(*rows, homeTodo{Kind: r.Kind, Tag: "brand", Title: r.Title, Sub: homeSubL(lang, r.Sub), Href: r.Href})
		}
	}
}

func (s *Server) homeApproveBuiltin(a audit.Actor, al auditLookup, add func(homeTodo), lang string) {
	memo := &reviewMemo{repo: al, who: a, m: map[string]bool{}}
	if s.sql != nil && s.sql.CanAccessPage(a) {
		if list, _, err := s.sql.List(a, sqlflow.ListQuery{Status: model.WfManReviewing, Limit: 100}); err != nil {
			slog.Warn("首页统计待我审批的工单失败", "err", err)
		} else {
			for _, w := range list {
				if memo.can(audit.TypeSQLReview, w.ID) {
					add(homeTodo{Kind: "SQL 上线", Tag: "brand", Title: w.Name, Href: fmt.Sprintf("/sqlworkflow/%d", w.ID),
						Sub: fmt.Sprintf("%s / %s · %s · %s", w.InstanceName, w.DBName, w.Engineer, w.CreateTime.Format("01-02 15:04"))})
				}
			}
		}
	}
	if s.apply != nil && s.apply.CanAccessPage(a) {
		if res, err := s.apply.List(a, "", 0, 100); err != nil {
			slog.Warn("首页统计待我审批的权限申请失败", "err", err)
		} else {
			for _, r := range res.Rows {
				if r.Status == audit.StatusWaiting && memo.can(audit.TypeQueryPrivilege, r.ApplyID) {
					add(homeTodo{Kind: "查询权限", Tag: "brand", Title: r.Title, Href: fmt.Sprintf("/query/applies/%d", r.ApplyID),
						Sub: i18n.T(lang, "{user} 申请 {inst} · {time}", "user", r.UserDisplay, "inst", r.InstanceName, "time", r.CreateTime.Format("01-02 15:04"))})
				}
			}
		}
	}
}

// homeMine 返回我提交的、进行中的 SQL 上线工单和等待审批的查询权限申请。
func (s *Server) homeMine(a audit.Actor, db homeDB, lang string) (rows []homeTodo, sqlN int, total int) {
	if db != nil && s.sql != nil && s.sql.CanAccessPage(a) {
		if list, n, err := db.MyWorkflows(a.Username, homeListMax); err != nil {
			slog.Warn("首页统计我提交的工单失败", "err", err)
		} else {
			sqlN, total = int(n), int(n)
			for _, w := range list {
				t := homeTodo{Kind: "SQL 上线", Tag: "brand", Title: w.Name, Href: fmt.Sprintf("/sqlworkflow/%d", w.ID),
					Sub: fmt.Sprintf("%s / %s · %s", w.InstanceName, w.DBName, i18n.TP(lang, sqlflow.StatusLabel(w.Status))), Warn: w.Status == model.WfException}
				rows = append(rows, t)
			}
		}
	}
	if s.apply != nil && s.apply.CanAccessPage(a) {
		if res, err := s.apply.List(a, "", 0, 100); err == nil {
			for _, r := range res.Rows {
				if r.UserName == a.Username && r.Status == audit.StatusWaiting {
					total++
					if len(rows) < homeListMax+3 {
						rows = append(rows, homeTodo{Kind: "查询权限", Tag: "brand", Title: r.Title, Href: fmt.Sprintf("/query/applies/%d", r.ApplyID),
							Sub: i18n.T(lang, "审批中 · {time}", "time", r.CreateTime.Format("01-02 15:04"))})
					}
				}
			}
		}
	}
	return rows, sqlN, total
}

// homeVisibleRunning 统计可见范围内进行中的 SQL 工单数（等待审批 / 审核通过 / 定时 / 排队 / 执行中）。
func (s *Server) homeVisibleRunning(a audit.Actor) (n int64, ok bool) {
	if s.sql == nil || !s.sql.CanAccessPage(a) {
		return 0, false
	}
	for _, st := range []string{model.WfManReviewing, model.WfReviewPass, model.WfTimingTask, model.WfQueuing, model.WfExecuting} {
		_, total, err := s.sql.List(a, sqlflow.ListQuery{Status: st, Limit: 1})
		if err != nil {
			slog.Warn("首页统计进行中工单失败", "status", st, "err", err)
			return 0, false
		}
		n += total
	}
	return n, true
}

// homeTrend 统计近 7 日（含今天）可见范围内每天提交的 SQL 工单数。
func (s *Server) homeTrend(a audit.Actor, now time.Time) (homeChart, bool) {
	if s.sql == nil || !s.sql.CanAccessPage(a) {
		return homeChart{}, false
	}
	day := func(i int) time.Time {
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, i-6)
	}
	counts := make([]int, 7)
	start, end := day(0).Format("2006-01-02"), day(6).Format("2006-01-02")
	rows, total, err := s.sql.List(a, sqlflow.ListQuery{StartDate: start, EndDate: end, Limit: 200})
	if err != nil {
		slog.Warn("首页统计近 7 日工单失败", "err", err)
		return homeChart{}, false
	}
	truncated := false
	if total > int64(len(rows)) { // 一批装不下：逐日只取总数
		for i := range counts {
			d := day(i).Format("2006-01-02")
			_, n, err := s.sql.List(a, sqlflow.ListQuery{StartDate: d, EndDate: d, Limit: 1})
			if err != nil {
				truncated = true
				continue
			}
			counts[i] = int(n)
		}
	} else {
		for _, r := range rows {
			for i := range counts {
				if r.CreateTime.Format("2006-01-02") == day(i).Format("2006-01-02") {
					counts[i]++
				}
			}
		}
	}
	c := buildChart(counts, now, day)
	c.Truncated = truncated
	return c, true
}

// buildChart 把 7 天的数量画成柱图坐标。
func buildChart(counts []int, now time.Time, day func(int) time.Time) homeChart {
	const w, h, left, base, top = 420, 170, 16, 140, 24
	slot := (w - 2*left) / len(counts)
	max := 1
	for _, n := range counts {
		if n > max {
			max = n
		}
	}
	c := homeChart{W: w, H: h}
	for i, n := range counts {
		bh := 0
		if n > 0 {
			bh = (base - top) * n / max
			if bh < 3 {
				bh = 3
			}
		}
		bw := slot * 6 / 10
		x := left + i*slot + (slot-bw)/2
		c.Bars = append(c.Bars, homeBar{Label: day(i).Format("01-02"), N: n, X: x, Y: base - bh, W: bw, H: bh, CX: x + bw/2, NY: base - bh - 6, LY: base + 16, Today: i == len(counts)-1})
		c.Total += n
	}
	return c
}

// homeInstances 返回当前用户可见的实例（在线查询能用到的实例：与“我可见的实例”一致），只取前几个展示。
func (s *Server) homeInstances(a audit.Actor) (list []homeInst, total int, ids string) {
	views, err := s.q.Instances(principalFromActor(a))
	if err != nil {
		slog.Warn("首页读取可见实例失败", "err", err)
		return nil, 0, ""
	}
	var idList []string
	for i, v := range views {
		if i < 2000 {
			idList = append(idList, strconv.FormatUint(uint64(v.ID), 10))
		}
		if i < 9 {
			list = append(list, homeInst{ID: v.ID, Name: v.Name, DBType: v.DBType, Role: v.Role, Env: v.Env, EnvLabel: v.EnvLabel, EnvProd: v.EnvProd})
		}
	}
	return list, len(views), strings.Join(idList, ",")
}

// homeRecent 返回最近动态：有“查看审计日志”权限看全平台，否则只看自己。
func (s *Server) homeRecent(a audit.Actor, db homeDB) (list []homeActivity, all bool) {
	if db == nil {
		return nil, false
	}
	all = s.ActorHasButton(a, "audit.view")
	user := a.Username
	if all {
		user = ""
	}
	rows, err := db.RecentAudit(user, 7)
	if err != nil {
		slog.Warn("首页读取最近动态失败", "err", err)
		return nil, all
	}
	for _, r := range rows {
		name := r.UserDisplay
		if name == "" {
			name = r.UserName
		}
		list = append(list, homeActivity{User: name, Action: r.Action, Extra: clipHome(r.ExtraInfo, 60), At: r.ActionTime.Format("01-02 15:04"), Mine: r.UserName == a.Username})
	}
	return list, all
}

func clipHome(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// homePlatform 返回平台状态卡片的各项；某项取不到就不列出。
func (s *Server) homePlatform(a audit.Actor) []homeStat {
	var out []homeStat
	if s.sql != nil {
		if s.sql.InceptionConfigured() {
			out = append(out, homeStat{Label: "goInception 检查服务", Value: "已配置", Class: "ok"})
		} else if s.sql.RequireInception() {
			out = append(out, homeStat{Label: "goInception 检查服务", Value: "未配置（要求必须配置）", Class: "warn"})
		} else {
			out = append(out, homeStat{Label: "goInception 检查服务", Value: "未配置，使用原生检查", Class: "info"})
		}
	}
	if s.admin != nil && s.admin.NotifyTableReady() {
		if chs, err := s.admin.ListChannelViews(); err == nil {
			on := 0
			for _, c := range chs {
				if c.Enabled {
					on++
				}
			}
			cls := "ok"
			if on == 0 {
				cls = "warn"
			}
			out = append(out, homeStat{Label: "通知渠道", Value: fmt.Sprintf("启用 %d / 共 %d 个", on, len(chs)), Class: cls})
		}
	}
	if s.sql != nil && s.sql.CanAccessPage(a) {
		if _, n, err := s.sql.List(a, sqlflow.ListQuery{Status: model.WfTimingTask, Limit: 1}); err == nil {
			out = append(out, homeStat{Label: "定时调度器", Value: fmt.Sprintf("待执行的定时工单 %d 个", n), Class: "ok"})
		}
	}
	// 原权限码还没迁移成按钮授权（没执行过 dbt-admin perm migrate）：普通用户此时没有任何按钮权限，只提醒超级用户
	if ck := s.buttons(); ck != nil && a.IsSuperuser {
		if set, err := ck.Setting(); err == nil && set.Mode != btnperm.ModeStrict {
			out = append(out, homeStat{Label: "按钮权限迁移", Value: "未迁移（执行 dbt-admin perm migrate）", Class: "warn"})
		}
	}
	return out
}

func principalFromActor(a audit.Actor) query.Principal {
	return query.Principal{ID: a.ID, Username: a.Username, Display: a.Display, IsSuperuser: a.IsSuperuser}
}
