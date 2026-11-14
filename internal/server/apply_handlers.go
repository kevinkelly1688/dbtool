// @author: kevinkelly
// @license: Apache Licence
// @file: apply_handlers.go
// @time: 2026/10/28

package server

import (
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/privapply"
	"dbtool/internal/query"
)

const pageSize = 20

func actorOf(c *gin.Context) audit.Actor {
	p := principalOf(c)
	return audit.Actor{ID: p.ID, Username: p.Username, Display: p.Display, IsSuperuser: p.IsSuperuser}
}

// applyEnabled 在没有配置权限申请服务时（例如测试里的最小仓库）让相关路由返回 404。
func (s *Server) applyEnabled() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.apply == nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

func pageParam(c *gin.Context) (page, offset int) {
	page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	return page, (page - 1) * pageSize
}

func pagesOf(total int64) int {
	n := int((total + pageSize - 1) / pageSize)
	if n < 1 {
		n = 1
	}
	return n
}

// ---- 页面 ----

// navData 是顶栏菜单；分组内容在 render 里按当前用户与当前页生成（见 menu.go）。
type navData struct {
	Groups []NavGroup
	Help   string // 顶栏“? 使用文档”按钮对应的文档章节 id（见 help.go）
}

func (s *Server) nav(audit.Actor) navData { return navData{} }

func (s *Server) renderForbidden(c *gin.Context, msg string) {
	p := principalOf(c)
	s.render(c, http.StatusForbidden, "error.html", pageData{Title: "没有权限", Username: p.Username, Display: p.Display,
		CSRF: sessionOf(c).CSRF, Error: msg, Nav2: s.nav(actorOf(c))})
}

func (s *Server) renderPageError(c *gin.Context, err error) {
	p := principalOf(c)
	status, msg := http.StatusInternalServerError, "服务异常，请稍后再试"
	if qe, ok := err.(*query.Error); ok {
		status, msg = qe.HTTP, qe.Message
	}
	s.render(c, status, "error.html", pageData{Title: "出错了", Username: p.Username, Display: p.Display,
		CSRF: sessionOf(c).CSRF, Error: msg, Nav2: s.nav(actorOf(c))})
}

type appliesPage struct {
	Rows           []privapply.ApplyRow
	Total          int64
	Page           int
	Pages          int
	Q              string
	Tab            string
	Status         string
	Counts         privapply.Counts
	CanTodo        bool
	CanAll         bool
	Statuses       []privapply.StatusOpt
	PrevPage       int
	NextPage       int
	ReasonRequired bool
}

func (s *Server) appliesPage(c *gin.Context) {
	a := actorOf(c)
	if !s.apply.CanAccessPage(a) {
		s.renderForbidden(c, "你没有查看权限申请的权限（缺少 qpriv.view），请联系管理员。")
		return
	}
	page, offset := pageParam(c)
	q := strings.TrimSpace(c.Query("q"))
	v, err := s.apply.ListApplies(a, privapply.ListQuery{Tab: c.Query("tab"), Status: c.Query("status"), Search: q, Offset: offset, Limit: pageSize})
	if err != nil {
		s.renderPageError(c, err)
		return
	}
	status := c.Query("status")
	known := false
	for _, o := range v.Statuses {
		known = known || o.Value == status
	}
	if !known {
		status = ""
	}
	pg := appliesPage{Rows: v.Rows, Total: v.Total, Page: page, Pages: pagesOf(v.Total), Q: q, Tab: v.Tab, Status: status, Counts: v.Counts,
		CanTodo: v.CanTodo, CanAll: v.CanAll, Statuses: v.Statuses, ReasonRequired: s.apply.ReasonRequired()}
	pg.PrevPage, pg.NextPage = page-1, page+1
	p := principalOf(c)
	s.render(c, http.StatusOK, "applies.html", pageData{Title: "权限申请", Nav: "applies", Username: p.Username, Display: p.Display,
		CSRF: sessionOf(c).CSRF, Data: pg, Nav2: s.nav(a)})
}

// flowText 翻译“审批流程”文字：审批流为空时审批引擎返回固定文案，其余是资源组名（用户数据），原样输出。
func flowText(c *gin.Context, s string) string {
	if s == "无需审批" { // i18n-ignore 与审批引擎的固定返回值比较
		return tr(c, "无需审批")
	}
	return s
}

// stepTitle 返回时间线节点的标题：审批节点按“{name}审批”整句翻译，其余是固定文案。
func stepTitle(c *gin.Context, st privapply.TLStep) string {
	if st.Node != "" {
		return tr(c, "{name}审批", "name", st.Node)
	}
	return trMsg(c, st.Title)
}

// applyDetailView 在 privapply.Detail 之外带上模板需要的 URL（Reapply 已是编码好的查询串，包成 template.URL 避免被二次转义）。
type applyDetailView struct {
	*privapply.Detail
	ReapplyHref template.URL
}

func (s *Server) applyDetailPage(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		s.renderPageError(c, query.NewError(404, "not_found", "工单不存在"))
		return
	}
	a := actorOf(c)
	d, err := s.apply.Detail(a, uint(id))
	if err != nil {
		s.renderPageError(c, err)
		return
	}
	d.FlowText = flowText(c, d.FlowText)
	dv := applyDetailView{Detail: d}
	if d.Reapply != "" {
		dv.ReapplyHref = template.URL("/query/applies?" + d.Reapply)
	}
	p := principalOf(c)
	s.render(c, http.StatusOK, "apply_detail.html", pageData{Title: "权限申请详情", Nav: "applies", Username: p.Username, Display: p.Display,
		CSRF: sessionOf(c).CSRF, Data: dv, Nav2: s.nav(a)})
}

type privilegesPage struct {
	Rows      []privapply.PrivRow
	Total     int64
	Page      int
	Pages     int
	Q         string
	CanManage bool
	CanGrant  bool
	PrevPage  int
	NextPage  int
	// 筛选与 KPI
	State     string
	User      string
	Inst      uint
	DB        string
	Stats     privapply.PrivStats
	HasStats  bool
	StateOpts []privapply.StatusOpt
}

func (s *Server) privilegesPage(c *gin.Context) {
	a := actorOf(c)
	if !s.apply.CanAccessPrivPage(a) {
		s.renderForbidden(c, "你没有查看查询权限的权限（缺少 qpriv.list），请联系管理员。")
		return
	}
	page, offset := pageParam(c)
	q := strings.TrimSpace(c.Query("q"))
	inst, _ := strconv.ParseUint(c.Query("inst"), 10, 32)
	pq := privapply.PrivQuery{Search: q, Offset: offset, Limit: pageSize, User: c.Query("user"), InstanceID: uint(inst), DB: c.Query("db"), State: c.Query("state")}
	res, err := s.apply.ListPrivileges(a, pq)
	if err != nil {
		s.renderPageError(c, err)
		return
	}
	state := pq.State
	if state != "soon" && state != "expired" && state != "all" {
		state = ""
	}
	pg := privilegesPage{Rows: res.Rows, Total: res.Total, Page: page, Pages: pagesOf(res.Total), Q: q, CanManage: res.CanManage, PrevPage: page - 1, NextPage: page + 1,
		CanGrant: res.CanManage && s.ActorHasButton(a, "qpriv.manage"), State: state, User: pq.User, Inst: pq.InstanceID, DB: pq.DB, Stats: res.Stats, HasStats: res.HasStats,
		StateOpts: []privapply.StatusOpt{{Value: "", Label: "有效（含即将到期）"}, {Value: "soon", Label: "7 天内到期"}, {Value: "expired", Label: "已过期"}, {Value: "all", Label: "全部"}}}
	p := principalOf(c)
	s.render(c, http.StatusOK, "privileges.html", pageData{Title: "授权清单", Nav: "privileges", Username: p.Username, Display: p.Display,
		CSRF: sessionOf(c).CSRF, Data: pg, Nav2: s.nav(a)})
}

// ---- JSON 接口 ----

type optionsResp struct {
	Instances      []optInstance `json:"instances"`
	Groups         []string      `json:"groups"`
	ReasonRequired bool          `json:"reason_required"` // 提交申请必须填写理由（旁路表就绪时）
}

type optInstance struct {
	ID     uint   `json:"id"`
	Name   string `json:"name"`
	DBType string `json:"db_type"`
}

func (s *Server) apiApplyOptions(c *gin.Context) {
	a := actorOf(c)
	insts, err := s.apply.ApplyInstances(a)
	if err != nil {
		writeErr(c, err)
		return
	}
	groups, err := s.apply.ApplyGroups(a)
	if err != nil {
		writeErr(c, err)
		return
	}
	out := optionsResp{Instances: []optInstance{}, Groups: []string{}, ReasonRequired: s.apply.ReasonRequired()}
	for _, i := range insts {
		out.Instances = append(out.Instances, optInstance{ID: i.ID, Name: i.InstanceName, DBType: i.DBType})
	}
	for _, g := range groups {
		out.Groups = append(out.Groups, g.GroupName)
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

type createReq struct {
	Title      string   `json:"title"`
	InstanceID uint     `json:"instance_id"`
	GroupName  string   `json:"group_name"`
	PrivType   int      `json:"priv_type"`
	DBName     string   `json:"db_name"`
	DBList     []string `json:"db_list"`
	TableList  []string `json:"table_list"`
	ValidDate  string   `json:"valid_date"`
	LimitNum   int      `json:"limit_num"`
	Reason     string   `json:"reason"`
}

func (s *Server) apiCreateApply(c *gin.Context) {
	if !s.ButtonOrFail(c, "qpriv.apply") {
		return
	}
	var req createReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	a, err := s.apply.Create(c.Request.Context(), actorOf(c), privapply.CreateReq(req))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"apply_id": a.ApplyID, "status": a.Status}})
}

func applyRowJSON(c *gin.Context, r privapply.ApplyRow) gin.H {
	h := gin.H{
		"apply_id": r.ApplyID, "title": r.Title, "instance": r.InstanceName, "priv_type": r.PrivType,
		"db_list": r.DBList, "table_list": r.TableList, "limit_num": r.LimitNum, "valid_date": r.ValidDate.Format("2006-01-02"),
		"status": r.Status, "eff_status": r.EffStatus, "status_label": trMsg(c, r.StatusLabel), "applicant": r.UserDisplay, "applicant_name": r.UserName, "group": r.GroupName,
		"create_time": r.CreateTime.Format("2006-01-02 15:04:05"), "reason": r.Reason, "can_abort": r.CanAbort,
	}
	if r.Progress != nil {
		h["progress"] = gin.H{"level": r.Progress.Level, "total": r.Progress.Total, "current": r.Progress.Current}
	}
	return h
}

// apiListApplies 返回权限申请列表。带 tab 参数（mine / todo / all）时按页签返回（含各页签数量、审批进度），不带时按可见范围列出（旧行为）。
func (s *Server) apiListApplies(c *gin.Context) {
	_, offset := pageParam(c)
	if tab := c.Query("tab"); tab != "" || c.Query("status") != "" {
		v, err := s.apply.ListApplies(actorOf(c), privapply.ListQuery{Tab: tab, Status: c.Query("status"), Search: c.Query("q"), Offset: offset, Limit: pageSize})
		if err != nil {
			writeErr(c, err)
			return
		}
		rows := make([]gin.H, 0, len(v.Rows))
		for _, r := range v.Rows {
			rows = append(rows, applyRowJSON(c, r))
		}
		c.JSON(http.StatusOK, gin.H{"data": rows, "total": v.Total, "tab": v.Tab,
			"counts": gin.H{"mine": v.Counts.Mine, "todo": v.Counts.Todo, "all": v.Counts.All}, "can_todo": v.CanTodo, "can_all": v.CanAll})
		return
	}
	res, err := s.apply.List(actorOf(c), c.Query("q"), offset, pageSize)
	if err != nil {
		writeErr(c, err)
		return
	}
	rows := make([]gin.H, 0, len(res.Rows))
	for _, r := range res.Rows {
		rows = append(rows, applyRowJSON(c, r))
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": res.Total})
}

func (s *Server) apiApplyDetail(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	d, err := s.apply.Detail(actorOf(c), id)
	if err != nil {
		writeErr(c, err)
		return
	}
	nodes := make([]gin.H, 0, len(d.Nodes))
	for _, n := range d.Nodes {
		nodes = append(nodes, gin.H{"name": n.Name, "current": n.Current, "passed": n.Passed})
	}
	logs := make([]gin.H, 0, len(d.Logs))
	for _, l := range d.Logs {
		logs = append(logs, gin.H{"operation": trMsg(c, l.OperationTypeDesc), "info": l.OperationInfo, "operator": l.OperatorDisplay, "time": l.OperationTime.Format("2006-01-02 15:04:05")})
	}
	reviewers := make([]string, 0, len(d.CurrentReviewers))
	for _, u := range d.CurrentReviewers {
		reviewers = append(reviewers, u.Username)
	}
	steps := make([]gin.H, 0, len(d.Timeline))
	for _, t := range d.Timeline {
		h := gin.H{"title": stepTitle(c, t), "state": t.State, "actor": t.Actor, "note": trMsg(c, t.Note), "reviewers": t.Reviewers}
		if !t.Time.IsZero() {
			h["time"] = t.Time.Format("2006-01-02 15:04:05")
		}
		steps = append(steps, h)
	}
	granted := make([]gin.H, 0, len(d.Granted))
	for _, g := range d.Granted {
		granted = append(granted, gin.H{"privilege_id": g.PrivilegeID, "db_name": g.DBName, "table_name": g.Table, "valid_date": g.ValidDate.Format("2006-01-02"),
			"limit_num": g.LimitNum, "revoked": g.Revoked, "state": g.State})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"apply_id": d.Apply.ApplyID, "title": d.Apply.Title, "instance": d.InstanceName, "status": d.Apply.Status, "eff_status": d.EffStatus, "status_label": trMsg(c, d.StatusLabel),
		"flow": flowText(c, d.FlowText), "nodes": nodes, "current_reviewers": reviewers, "logs": logs, "can_review": d.CanReview, "can_abort": d.CanAbort,
		"reason": d.Reason, "dept": d.Dept, "timeline": steps, "granted": granted, "reapply": d.Reapply,
	}})
}

type auditReq struct {
	Action string `json:"action"` // pass / reject
	Remark string `json:"remark"`
}

func (s *Server) apiAuditApply(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var req auditReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	if !s.ButtonOrFail(c, "qpriv.review") {
		return
	}
	action := map[string]int{"pass": audit.ActionPass, "reject": audit.ActionReject}[req.Action]
	if err := s.apply.Review(c.Request.Context(), actorOf(c), id, action, req.Remark); err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

func (s *Server) apiAbortApply(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if !s.ButtonOrFail(c, "qpriv.apply") {
		return
	}
	var req auditReq
	_ = c.ShouldBindJSON(&req)
	if err := s.apply.Abort(c.Request.Context(), actorOf(c), id, req.Remark); err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

func (s *Server) apiListPrivileges(c *gin.Context) {
	_, offset := pageParam(c)
	inst, _ := strconv.ParseUint(c.Query("inst"), 10, 32)
	res, err := s.apply.ListPrivileges(actorOf(c), privapply.PrivQuery{Search: c.Query("q"), Offset: offset, Limit: pageSize,
		User: c.Query("user"), InstanceID: uint(inst), DB: c.Query("db"), State: c.Query("state")})
	if err != nil {
		writeErr(c, err)
		return
	}
	rows := make([]gin.H, 0, len(res.Rows))
	for _, r := range res.Rows {
		rows = append(rows, gin.H{
			"privilege_id": r.PrivilegeID, "user": r.UserDisplay, "user_name": r.UserName, "instance": r.InstanceName, "instance_id": r.InstanceID,
			"db_name": r.DBName, "table_name": r.TbName, "priv_type": r.PrivType, "limit_num": r.LimitNum, "valid_date": r.ValidDate.Format("2006-01-02"),
			"state": r.State, "state_label": trMsg(c, privapply.StateLabel(r.State)), "days_left": r.DaysLeft,
			"source": r.Source, "source_apply_id": r.ApplyID, "granted_by": r.GrantedBy, "grant_reason": r.GrantNote,
		})
	}
	out := gin.H{"data": rows, "total": res.Total, "can_manage": res.CanManage}
	if res.HasStats {
		out["stats"] = gin.H{"valid": res.Stats.Valid, "soon": res.Stats.Soon, "expired": res.Stats.Expired, "users": res.Stats.Users, "instances": res.Stats.Insts}
	}
	c.JSON(http.StatusOK, out)
}

type modifyReq struct {
	ValidDate string `json:"valid_date"`
	LimitNum  int    `json:"limit_num"`
}

func (s *Server) apiModifyPrivilege(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var req modifyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	if err := s.apply.ModifyPrivilege(actorOf(c), id, req.ValidDate, req.LimitNum); err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

func (s *Server) apiDeletePrivilege(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if err := s.apply.DeletePrivilege(actorOf(c), id); err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}
