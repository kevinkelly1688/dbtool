// @author: kevinkelly
// @license: Apache Licence
// @file: sql_listview.go
// @time: 2026/10/28

package server

import (
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"dbtool/internal/model"
	"dbtool/internal/sqlflow"
)

// SQL 工单列表页与数据导出列表页共用的页面数据与链接拼装。

// sqlListParams 是列表页会带在链接里的筛选参数（顺序固定，方便阅读和测试）。
var sqlListParams = []string{"tab", "status", "kind", "batch", "inst", "rg", "q", "sort", "dir"}

type sqlListPage struct {
	Export   bool   // 数据导出列表
	Path     string // 页面路径，用于拼链接
	Rows     []sqlflow.WorkflowItem
	Total    int64
	Page     int
	Pages    int
	PrevPage int
	NextPage int

	Q, Status, Batch, Kind, Tab, Sort, Dir string
	InstID, GroupID                        uint

	Statuses []sqlflow.Option
	Stats    *sqlflow.ListStats // 取不到时为 nil，页面不显示卡片和计数
	Groups   []model.ResourceGroup
	Insts    []sqlflow.InstanceOption
	CanSub   bool
	CanExp   bool
	View     sqlflow.ExportView // 导出列表：能看哪些导出工单

	vals url.Values
}

// Href 返回在当前筛选基础上改几个参数后的链接，键值成对给出，值为空表示去掉该参数；翻页以外的改动都会回到第 1 页。
func (p *sqlListPage) Href(kv ...string) template.URL {
	v := url.Values{}
	for _, k := range sqlListParams {
		if x := p.vals.Get(k); x != "" {
			v.Set(k, x)
		}
	}
	page := false
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] == "page" {
			page = true
		}
		if kv[i+1] == "" {
			v.Del(kv[i])
		} else {
			v.Set(kv[i], kv[i+1])
		}
	}
	if !page {
		v.Del("page")
	}
	if enc := v.Encode(); enc != "" {
		return template.URL(p.Path + "?" + enc)
	}
	return template.URL(p.Path)
}

// Toggle 同一个值再点一次就取消筛选（状态卡片、页签用）。
func (p *sqlListPage) Toggle(key, val string) template.URL {
	if p.vals.Get(key) == val {
		return p.Href(key, "")
	}
	return p.Href(key, val)
}

// SortHref 点列标题排序：同一列再点一次反向；新选一列时默认倒序（时间 / 编号）或正序（文字列）。
func (p *sqlListPage) SortHref(key string) template.URL {
	dir := "desc"
	switch {
	case p.Sort == key && p.Dir == "desc":
		dir = "asc"
	case p.Sort != key && key != "id" && key != "create_time" && key != "status":
		dir = "asc"
	}
	return p.Href("sort", key, "dir", dir)
}

// SortMark 列标题上的排序箭头。
func (p *sqlListPage) SortMark(key string) string {
	switch {
	case p.Sort != key:
		return "↕"
	case p.Dir == "asc":
		return "▲"
	}
	return "▼"
}

// StatusCount 某状态的工单数（没有统计时为 0）。
func (p *sqlListPage) StatusCount(st string) int64 {
	if p.Stats == nil {
		return 0
	}
	return p.Stats.ByStatus[st]
}

func (p *sqlListPage) Filtered() bool {
	for _, k := range []string{"status", "inst", "rg", "q"} {
		if p.vals.Get(k) != "" {
			return true
		}
	}
	return false
}

// buildSQLListPage 读取筛选参数并取数。export=true 时是数据导出列表（只含导出工单，并按 export.view / export.view_batch 过滤）。
func (s *Server) buildSQLListPage(c *gin.Context, export bool) (*sqlListPage, error) {
	a := actorOf(c)
	page, offset := pageParam(c)
	pg := &sqlListPage{Export: export, Path: "/sqlworkflow", Page: page, Statuses: sqlflow.StatusOptions(), vals: url.Values{},
		CanSub: s.sql.CanSubmit(a), CanExp: s.sql.CanExport(a)}
	if export {
		pg.Path = "/sqlworkflow/export"
	}
	q := strings.TrimSpace(c.Query("q"))
	if r := []rune(q); len(r) > 100 {
		q = string(r[:100])
	}
	pg.Q = q
	if st := strings.TrimSpace(c.Query("status")); st != "" {
		for _, o := range pg.Statuses {
			if o.Value == st {
				pg.Status = st
			}
		}
	}
	if b := strings.TrimSpace(c.Query("batch")); b == "0" || b == "1" {
		pg.Batch = b
	}
	if k := strings.TrimSpace(c.Query("kind")); !export && (k == "export" || k == "change") {
		pg.Kind = k
	}
	if export {
		pg.Kind = "export"
	}
	switch t := strings.TrimSpace(c.Query("tab")); t {
	case sqlflow.TabMine, sqlflow.TabApprove, sqlflow.TabExec:
		pg.Tab = t
	}
	if sort := sqlflow.NormalizeOrder(c.Query("sort")); sort != "" {
		pg.Sort, pg.Dir = sort, "desc"
		if c.Query("dir") == "asc" {
			pg.Dir = "asc"
		}
	}
	if v, err := strconv.ParseUint(c.Query("inst"), 10, 32); err == nil {
		pg.InstID = uint(v)
	}
	if v, err := strconv.ParseUint(c.Query("rg"), 10, 32); err == nil {
		pg.GroupID = uint(v)
	}
	set := func(k, v string) {
		if v != "" {
			pg.vals.Set(k, v)
		}
	}
	set("tab", pg.Tab)
	set("status", pg.Status)
	if !export {
		set("kind", pg.Kind)
	}
	set("batch", pg.Batch)
	if pg.InstID != 0 {
		set("inst", strconv.Itoa(int(pg.InstID)))
	}
	if pg.GroupID != 0 {
		set("rg", strconv.Itoa(int(pg.GroupID)))
	}
	set("q", pg.Q)
	set("sort", pg.Sort)
	set("dir", pg.Dir)

	lq := sqlflow.ListQuery{Search: pg.Q, Status: pg.Status, Batch: pg.Batch, Kind: pg.Kind, Tab: pg.Tab, InstanceID: pg.InstID, GroupID: pg.GroupID,
		OrderBy: pg.Sort, Desc: pg.Dir != "asc", Offset: offset, Limit: pageSize}
	var err error
	if export {
		if pg.View, err = s.sql.ExportViewOf(a); err != nil {
			return nil, err
		}
		pg.Rows, pg.Total, err = s.sql.ListExports(a, lq)
	} else {
		pg.View, _ = s.sql.ExportViewOf(a) // 决定要不要显示“数据导出工单”入口
		pg.Rows, pg.Total, err = s.sql.List(a, lq)
	}
	if err != nil {
		return nil, err
	}
	pg.Pages, pg.PrevPage, pg.NextPage = pagesOf(pg.Total), page-1, page+1
	// 卡片、页签计数与筛选下拉只是辅助信息：取不到就不显示，不拖垮列表本身
	if export {
		pg.Stats, err = s.sql.ExportStats(a)
	} else {
		pg.Stats, err = s.sql.Stats(a, pg.Kind)
	}
	if err != nil {
		slog.Warn("工单列表统计失败", "err", err)
		pg.Stats = nil
	}
	if opt, err := s.sql.ListFilterOptions(a); err != nil {
		slog.Warn("工单列表筛选项失败", "err", err)
	} else {
		pg.Groups, pg.Insts = opt.Groups, opt.Instances
	}
	return pg, nil
}

func (s *Server) sqlListPage(c *gin.Context) {
	a := actorOf(c)
	if !s.sql.CanAccessPage(a) {
		s.renderForbidden(c, "你没有查看 SQL 工单的权限（缺少 sqlworkflow.view_own / export.view），请联系管理员。")
		return
	}
	pg, err := s.buildSQLListPage(c, false)
	if err != nil {
		s.renderPageError(c, err)
		return
	}
	p := principalOf(c)
	s.render(c, http.StatusOK, "sql_list.html", pageData{Title: "变更工单", Nav: "sql", Username: p.Username, Display: p.Display,
		CSRF: sessionOf(c).CSRF, Data: pg, Nav2: s.nav(a)})
}

func (s *Server) sqlExportListPage(c *gin.Context) {
	a := actorOf(c)
	// 菜单入口指向这里：只有提交权限、没有查看权限的人直接去提交页，而不是看到 403
	if v, err := s.sql.ExportViewOf(a); err == nil && !v.Single && !v.Batch && s.sql.CanExport(a) {
		c.Redirect(http.StatusFound, "/sqlworkflow/export/new")
		return
	}
	pg, err := s.buildSQLListPage(c, true)
	if err != nil {
		s.renderPageError(c, err)
		return
	}
	p := principalOf(c)
	s.render(c, http.StatusOK, "sql_export_list.html", pageData{Title: "数据导出工单", Nav: "export", Username: p.Username, Display: p.Display,
		CSRF: sessionOf(c).CSRF, Data: pg, Nav2: s.nav(a)})
}

// Sum 几个状态的工单数之和（导出列表的“待导出 / 导出中 / 已终止”卡片用）。
func (p *sqlListPage) Sum(statuses ...string) int64 {
	var n int64
	for _, st := range statuses {
		n += p.StatusCount(st)
	}
	return n
}
