// @author: kevinkelly
// @license: Apache Licence
// @file: query_handlers.go
// @time: 2026/10/28

package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"dbtool/internal/i18n"
	"dbtool/internal/query"
)

type queryPageData struct {
	Instances     []query.InstanceView
	Policy        query.Policy
	ApplyEnabled  bool // 本系统是否提供权限申请（是则无权限时链接到本系统的申请页）
	ExportEnabled bool // 本系统是否提供数据导出工单（是则结果被截断时链接到导出提交页）
}

func (s *Server) queryPage(c *gin.Context) {
	p := principalOf(c)
	if !s.q.CanAccessPage(p) {
		s.render(c, http.StatusForbidden, "error.html", pageData{
			Title: "没有权限", Username: p.Username, Display: p.Display, CSRF: sessionOf(c).CSRF,
			Error: "你没有使用在线查询的权限（缺少 query.view 权限），请联系管理员。",
		})
		return
	}
	insts, err := s.q.Instances(p)
	if err != nil {
		slog.Error("加载实例失败", "err", err)
	}
	s.render(c, http.StatusOK, "query/index.html", pageData{
		Title: "在线查询", Nav: "query", Username: p.Username, Display: p.Display, CSRF: sessionOf(c).CSRF,
		Data: queryPageData{Instances: insts, Policy: s.q.Policy(), ApplyEnabled: s.apply != nil, ExportEnabled: s.sql != nil},
		Nav2: s.nav(actorOf(c)),
	})
}

// writeErr 把业务错误转成 JSON；未知错误只记日志，不向客户端暴露细节。
func writeErr(c *gin.Context, err error) {
	var qe *query.Error
	if errors.As(err, &qe) {
		c.AbortWithStatusJSON(qe.HTTP, qe)
		return
	}
	slog.Error("请求失败", "path", c.Request.URL.Path, "err", err)
	apiFail(c, http.StatusInternalServerError, "internal", "服务异常，请稍后再试")
}

func uintParam(c *gin.Context, name string) (uint, bool) {
	n, err := strconv.ParseUint(c.Param(name), 10, 32)
	if err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "参数非法")
		return 0, false
	}
	return uint(n), true
}

func (s *Server) apiInstances(c *gin.Context) {
	list, err := s.q.Instances(principalOf(c))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": list})
}

func (s *Server) apiDatabases(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	list, err := s.q.Databases(c.Request.Context(), principalOf(c), id)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": nonNil(list)})
}

func (s *Server) apiTables(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	list, err := s.q.Tables(c.Request.Context(), principalOf(c), id, c.Param("db"))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": nonNil(list)})
}

func (s *Server) apiColumns(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	list, err := s.q.Columns(c.Request.Context(), principalOf(c), id, c.Param("db"), c.Param("table"))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": nonNil(list)})
}

func nonNil(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func (s *Server) apiExecute(c *gin.Context) {
	var req query.Request
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	res, err := s.q.Execute(c.Request.Context(), principalOf(c), req)
	if err != nil {
		writeErr(c, err)
		return
	}
	localizeResult(c, res)
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// localizeResult 把成功响应里面向用户的提示翻译成请求语言：Notice（多条提示按行拼接）、引擎的 warnings / notices、
// 执行计划的风险提示、限制来源。错误响应由 HTTP 边缘统一翻译；成功响应只能在这里按目录翻译（中文请求原样返回）。
func localizeResult(c *gin.Context, res *query.Result) {
	if res == nil || langOf(c) == i18n.ZH {
		return
	}
	res.Notice = trNotice(c, res.Notice)
	res.LimitSource = trMsg(c, res.LimitSource)
	for i, w := range res.Warnings {
		res.Warnings[i] = trMsg(c, w)
	}
	for i, n := range res.Notices {
		res.Notices[i] = trMsg(c, n)
	}
	for i := range res.PlanTips {
		res.PlanTips[i].Text = trMsg(c, res.PlanTips[i].Text)
	}
	if fw := res.Firewall; fw != nil {
		for i, w := range fw.Warnings {
			fw.Warnings[i] = trMsg(c, w)
		}
	}
}

// trNotice 翻译按行拼接的提示。整行查不到译文时，再按句号拆成句子逐句翻译（如结果字节上限的两句话是直接拼在一起的）。
func trNotice(c *gin.Context, s string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if out := trMsg(c, l); out != l || !strings.Contains(l, "。") {
			lines[i] = out
			continue
		}
		var parts []string
		for rest := l; rest != ""; {
			k := strings.Index(rest, "。")
			if k < 0 {
				parts = append(parts, trMsg(c, rest))
				break
			}
			end := k + len("。")
			parts = append(parts, trMsg(c, rest[:end]))
			rest = rest[end:]
		}
		lines[i] = strings.Join(parts, " ")
	}
	return strings.Join(lines, "\n")
}

// apiHistory 返回当前用户的 SQL 执行历史；管理员可用 user 参数按用户过滤。
func (s *Server) apiHistory(c *gin.Context) {
	off, _ := strconv.Atoi(c.Query("offset"))
	lim, _ := strconv.Atoi(c.Query("limit"))
	pg, err := s.q.History(principalOf(c), query.HistoryQuery{Search: c.Query("q"), User: c.Query("user"), Instance: c.Query("instance"),
		Start: c.Query("start"), End: c.Query("end"), Offset: off, Limit: lim, Status: c.Query("status")})
	if err != nil {
		writeErr(c, err)
		return
	}
	if pg != nil && langOf(c) != i18n.ZH { // 范围说明与失败原因是服务端生成的中文，按请求语言翻译
		pg.Scope = trMsg(c, pg.Scope)
		for i := range pg.Items {
			pg.Items[i].Reason = trMsg(c, pg.Items[i].Reason)
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": pg})
}

// apiHistoryUsers 管理员过滤下拉用：执行过查询的用户。
func (s *Server) apiHistoryUsers(c *gin.Context) {
	us, err := s.q.HistoryUsers(principalOf(c))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": us})
}

// ---- HTML 片段 ----

func (s *Server) fragErr(c *gin.Context, err error) {
	status, msg := http.StatusInternalServerError, "服务异常，请稍后再试"
	var qe *query.Error
	if errors.As(err, &qe) {
		status, msg = qe.HTTP, qe.Message
	} else {
		slog.Error("片段请求失败", "path", c.Request.URL.Path, "err", err)
	}
	s.fragment(c, status, "alert", msg)
}

func instanceID(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Query("instance"), 10, 32)
	return uint(n), err == nil && n > 0
}

func (s *Server) uiDatabases(c *gin.Context) {
	id, ok := instanceID(c)
	if !ok {
		s.fragment(c, http.StatusBadRequest, "alert", "请选择实例")
		return
	}
	dbs, err := s.q.Databases(c.Request.Context(), principalOf(c), id)
	if err != nil {
		s.fragErr(c, err)
		return
	}
	s.fragment(c, http.StatusOK, "db_options", dbs)
}

type treeData struct {
	DB     string
	Tables []string
	Q      string
}

func (s *Server) uiTree(c *gin.Context) {
	id, ok := instanceID(c)
	db := c.Query("db")
	if !ok || db == "" {
		s.fragment(c, http.StatusOK, "tree_hint", "选择实例和数据库后显示表")
		return
	}
	tables, err := s.q.Tables(c.Request.Context(), principalOf(c), id, db)
	if err != nil {
		s.fragErr(c, err)
		return
	}
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	var shown []string
	for _, t := range tables {
		if q == "" || strings.Contains(strings.ToLower(t), q) {
			shown = append(shown, t)
		}
	}
	s.fragment(c, http.StatusOK, "tree", treeData{DB: db, Tables: shown, Q: q})
}

type policyData struct {
	Pol     query.Policy
	Env     string
	Role    string
	DBType  string
	IsAdmin bool
}

func (s *Server) uiPolicy(c *gin.Context) {
	p := principalOf(c)
	pd := policyData{Pol: s.q.Policy(), IsAdmin: p.IsSuperuser}
	if id, ok := instanceID(c); ok {
		insts, _ := s.q.Instances(p)
		for _, i := range insts {
			if i.ID == id {
				pd.Env, pd.Role, pd.DBType = i.Env, i.Role, i.DBType
			}
		}
	}
	s.fragment(c, http.StatusOK, "policy", pd)
}
