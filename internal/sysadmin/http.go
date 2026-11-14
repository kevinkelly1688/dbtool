// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package sysadmin

import (
	"encoding/csv"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	mysqldrv "github.com/go-sql-driver/mysql"

	"dbtool/internal/audit"
	"dbtool/internal/btnperm"
	_ "dbtool/internal/btnperm/btnhttp" // 按钮级权限的接口（/api/v1/settings/button-perms/*），页面在本模块的权限组管理里；cmd/server 已导入本包，借此一并启用
	"dbtool/internal/i18n"
	"dbtool/internal/query"
	"dbtool/internal/server"
)

const permAuditUser = "audit.view" // 按钮权限码：打开审计日志页、查看日志

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, mirage, ok := s.DB()
		if !ok {
			return
		}
		st := newStore(db, mirage)
		svc := NewService(st)
		setMustChangeStore(st)                           // 供登录流程读取“下次登录必须改密”标记（MustChangePassword）
		btnperm.SetGroupSaveCheck(svc.GroupSaveWarnings) // 权限组保存按钮权限前，预检会不会让某级审批流无人可审
		registerWith(svc)(s, rt)
	})
	super := func(s *server.Server, a audit.Actor) bool { return server.SuperOnly(s, a) }
	server.AddMenu(server.MenuItem{Group: "secure", Key: "sys-audit-flow", Href: "/settings/audit-flow", Label: "审批流程", Order: 10, Show: super})
	server.AddMenu(server.MenuItem{Group: "access", Key: "sys-res-group", Href: "/settings/resource-groups", Label: "资源组", Order: 30, Show: super})
	server.AddMenu(server.MenuItem{Group: "access", Key: "sys-users", Href: "/settings/users", Label: "用户", Order: 10, Show: super})
	server.AddMenu(server.MenuItem{Group: "access", Key: "sys-auth-groups", Href: "/settings/auth-groups", Label: "权限组", Order: 20, Show: super})
	server.AddMenu(server.MenuItem{Group: "secure", Key: "sys-audit", Href: "/audit", Label: "审计日志", Order: 50,
		Show: func(s *server.Server, a audit.Actor) bool {
			return s.ActorHasButton(a, permAuditUser)
		}})
}

type handlers struct {
	s   *server.Server
	svc *Service
}

// registerWith 登记本模块的页面与接口（测试里传入假仓库的 Service）。
func registerWith(svc *Service) func(s *server.Server, rt server.Routes) {
	return func(s *server.Server, rt server.Routes) {
		h := &handlers{s: s, svc: svc}

		// 页面
		page := func(path, tpl, title, nav string, mw ...gin.HandlerFunc) {
			rt.Pages.GET(path, append(mw, func(c *gin.Context) {
				s.Render(c, http.StatusOK, "sysadmin/"+tpl, server.PageData{Title: title, Nav: nav})
			})...)
		}
		page("/settings/audit-flow", "audit_flow.html", "审批流程", "sys-audit-flow", s.SuperuserPage())
		page("/settings/resource-groups", "resource_groups.html", "资源组", "sys-res-group", s.SuperuserPage())
		page("/settings/users", "users.html", "用户", "sys-users", s.SuperuserPage())
		page("/settings/auth-groups", "auth_groups.html", "权限组", "sys-auth-groups", s.SuperuserPage())
		page("/audit", "audit.html", "审计日志", "sys-audit", s.RequireButtonPage(permAuditUser))

		// 接口：仅超级用户，且每次请求都用数据库里的最新状态再确认一次
		st := rt.API.Group("/settings", s.SuperuserAPI(), h.freshAdmin(), invalidateButtonCache())

		st.GET("/audit-flow", h.flowOverview)
		st.GET("/audit-flow/detail", h.flowDetail)
		st.POST("/audit-flow/preview", h.flowPreview)
		st.PUT("/audit-flow", h.flowSave)
		st.DELETE("/audit-flow", h.flowDelete)

		st.GET("/resource-groups", h.rgList)
		st.POST("/resource-groups", h.rgCreate)
		st.GET("/resource-groups/:id", h.rgGet)
		st.PUT("/resource-groups/:id", h.rgUpdate)
		st.DELETE("/resource-groups/:id", h.rgDisable)
		st.POST("/resource-groups/:id/restore", h.rgRestore)
		st.GET("/resource-groups/:id/relations", h.rgRelations)
		st.PUT("/resource-groups/:id/users", h.rgChangeRelations("users"))
		st.PUT("/resource-groups/:id/instances", h.rgChangeRelations("instances"))

		st.GET("/users", h.userList)
		st.POST("/users", h.userCreate)
		st.GET("/users/options", h.userOptions)
		st.GET("/users/:id", h.userGet)
		st.PUT("/users/:id", h.userUpdate)
		st.POST("/users/:id/enable", h.userActive(true))
		st.POST("/users/:id/disable", h.userActive(false))
		st.POST("/users/:id/unlock", h.userUnlock)
		st.POST("/users/:id/reset-password", h.userResetPassword)
		st.POST("/users/:id/reset-2fa", h.userReset2FA)

		st.GET("/auth-groups", h.agList)
		st.POST("/auth-groups", h.agCreate)
		st.GET("/auth-groups/:id", h.agGet)
		st.PUT("/auth-groups/:id", h.agUpdate)
		st.DELETE("/auth-groups/:id", h.agDelete)

		h.registerExt(st) // 资源组 KPI / 彻底删除 / 关联预览、用户 KPI（http_ext.go）

		// 系统审计：超级用户或拥有 audit.view 按钮
		au := rt.API.Group("/audit", s.RequireButtonAPI(permAuditUser))
		au.GET("/options", h.auditOptions)
		au.GET("/log", h.auditLog)
		au.GET("/log/export", s.RequireButtonAPI("audit.export"), h.auditExport)
		au.GET("/sql-workflows", h.auditSQLWorkflows)
		au.GET("/query-logs", h.auditQueryLogs)
		registerAuditExt(h, au) // 概览、详情、状态分布、查询审计汇总与完整 SQL（auditlog_http.go）
	}
}

// invalidateButtonCache 在改变用户 / 权限组 / 权限的写操作成功后，使按钮权限的缓存失效（成员、启停等变化立即生效）。
func invalidateButtonCache() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if c.Request.Method != http.MethodGet && c.Writer.Status() < http.StatusBadRequest {
			if r := btnperm.Default(); r != nil {
				r.InvalidateAll()
			}
		}
	}
}

// ---- 公共辅助 ----

const actorKey = "sysadmin.actor"

// freshAdmin 用数据库里的最新状态确认操作者仍是启用的超级用户（会话里的标记在有效期内不会自动失效）。
func (h *handlers) freshAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		a, err := h.svc.Admin(server.PrincipalOf(c).ID)
		if err != nil {
			writeErr(c, err)
			return
		}
		c.Set(actorKey, a)
		c.Next()
	}
}

func actorOf(c *gin.Context) audit.Actor {
	v, _ := c.Get(actorKey)
	a, _ := v.(audit.Actor)
	return a
}

func writeErr(c *gin.Context, err error) {
	var qe *query.Error
	if errors.As(err, &qe) {
		c.AbortWithStatusJSON(qe.HTTP, localizeErr(reqLang(c), qe))
		return
	}
	// 并发提交同名记录时，两个事务都先查到“不存在”，后到的 INSERT 在唯一索引上撞车（MySQL 1062）：这是冲突，不是服务异常。
	var me *mysqldrv.MySQLError
	if errors.As(err, &me) && me.Number == 1062 {
		server.APIFail(c, http.StatusConflict, "conflict", "已存在同名或重复的记录（可能刚被其他人创建），请刷新后重试")
		return
	}
	slog.Error("系统管理请求失败", "path", c.Request.URL.Path, "err", err)
	server.APIFail(c, http.StatusInternalServerError, "internal", "服务异常，请稍后再试")
}

func badRequest(c *gin.Context, msg string) {
	server.APIFail(c, http.StatusBadRequest, "bad_request", msg)
}

func idParam(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		badRequest(c, "参数非法")
		return 0, false
	}
	return uint(n), true
}

func queryUint(c *gin.Context, name string) (uint, bool) {
	v := strings.TrimSpace(c.Query(name))
	if v == "" {
		return 0, true
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		badRequest(c, errf("参数 %s 非法", name))
		return 0, false
	}
	return uint(n), true
}

func queryInt(c *gin.Context, name string, def int) int {
	n, err := strconv.Atoi(c.Query(name))
	if err != nil {
		return def
	}
	return n
}

// bind 解析 JSON 请求体；失败时直接响应 400。
func bind(c *gin.Context, v any) bool {
	if err := c.ShouldBindJSON(v); err != nil {
		badRequest(c, "请求格式错误")
		return false
	}
	return true
}

// done 统一处理“只返回成功与否”的写操作结果。
func done(c *gin.Context, err error) {
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

// ---- 审批流程配置 ----

func (h *handlers) flowOverview(c *gin.Context) {
	out, err := h.svc.FlowOverview()
	if err != nil {
		writeErr(c, err)
		return
	}
	localizeOverview(reqLang(c), out)
	server.APIOK(c, out)
}

func (h *handlers) flowKey(c *gin.Context) (groupID uint, wt int, ok bool) {
	groupID, ok = queryUint(c, "group_id")
	if !ok {
		return
	}
	wt = queryInt(c, "workflow_type", 0)
	return groupID, wt, true
}

func (h *handlers) flowDetail(c *gin.Context) {
	g, wt, ok := h.flowKey(c)
	if !ok {
		return
	}
	v, err := h.svc.FlowDetail(g, wt)
	if err != nil {
		writeErr(c, err)
		return
	}
	localizeFlow(reqLang(c), v)
	server.APIOK(c, v)
}

type flowReq struct {
	GroupID      uint   `json:"group_id"`
	WorkflowType int    `json:"workflow_type"`
	AuthGroupIDs []uint `json:"auth_group_ids"`
}

func (h *handlers) flowPreview(c *gin.Context) {
	var req flowReq
	if !bind(c, &req) {
		return
	}
	v, err := h.svc.PreviewFlow(req.GroupID, req.WorkflowType, req.AuthGroupIDs)
	if err != nil {
		writeErr(c, err)
		return
	}
	localizeFlow(reqLang(c), v)
	server.APIOK(c, v)
}

func (h *handlers) flowSave(c *gin.Context) {
	var req flowReq
	if !bind(c, &req) {
		return
	}
	res, err := h.svc.SaveFlow(actorOf(c), req.GroupID, req.WorkflowType, req.AuthGroupIDs)
	if err != nil {
		writeErr(c, err)
		return
	}
	localizeSaveResult(reqLang(c), res)
	server.APIOK(c, res)
}

func (h *handlers) flowDelete(c *gin.Context) {
	g, wt, ok := h.flowKey(c)
	if !ok {
		return
	}
	done(c, h.svc.DeleteFlow(actorOf(c), g, wt))
}

// ---- 资源组 ----

func (h *handlers) rgList(c *gin.Context) {
	list, err := h.svc.ListResourceGroups(c.Query("search"), c.Query("deleted") == "1")
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, list)
}

func (h *handlers) rgGet(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	d, err := h.svc.GetResourceGroup(id)
	if err != nil {
		writeErr(c, err)
		return
	}
	localizeResGroup(reqLang(c), d)
	server.APIOK(c, d)
}

func (h *handlers) rgCreate(c *gin.Context) {
	var in ResGroupInput
	if !bind(c, &in) {
		return
	}
	id, err := h.svc.CreateResourceGroup(actorOf(c), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"id": id})
}

func (h *handlers) rgUpdate(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in ResGroupInput
	if !bind(c, &in) {
		return
	}
	done(c, h.svc.UpdateResourceGroup(actorOf(c), id, in))
}

func (h *handlers) rgDisable(c *gin.Context) {
	if id, ok := idParam(c); ok {
		done(c, h.svc.DisableResourceGroup(actorOf(c), id))
	}
}

func (h *handlers) rgRestore(c *gin.Context) {
	if id, ok := idParam(c); ok {
		done(c, h.svc.RestoreResourceGroup(actorOf(c), id))
	}
}

func (h *handlers) rgRelations(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	r, err := h.svc.GroupRelations(id)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, r)
}

func (h *handlers) rgChangeRelations(kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := idParam(c)
		if !ok {
			return
		}
		var ch RelationChange
		if !bind(c, &ch) {
			return
		}
		done(c, h.svc.ChangeGroupRelations(actorOf(c), id, kind, ch))
	}
}

// ---- 用户 ----

func (h *handlers) userList(c *gin.Context) {
	ag, ok := queryUint(c, "auth_group")
	if !ok {
		return
	}
	rg, ok := queryUint(c, "resource_group")
	if !ok {
		return
	}
	page, err := h.svc.ListUsers(UserQuery{Search: c.Query("search"), Status: c.Query("status"), Sort: c.Query("sort"), AuthGroupID: ag, ResourceGroupID: rg,
		Page: queryInt(c, "page", 1), PageSize: queryInt(c, "page_size", 20)})
	if err != nil {
		writeErr(c, err)
		return
	}
	localizeUsers(reqLang(c), page.Rows)
	server.APIOK(c, page)
}

func (h *handlers) userOptions(c *gin.Context) {
	o, err := h.svc.UserOptions()
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, o)
}

func (h *handlers) userGet(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	d, err := h.svc.GetUser(id)
	if err != nil {
		writeErr(c, err)
		return
	}
	if lang := reqLang(c); lang != i18n.ZH {
		d.AuthLabel = authLabelIn(lang, d.AuthMethods)
	}
	server.APIOK(c, d)
}

func (h *handlers) userCreate(c *gin.Context) {
	var in UserInput
	if !bind(c, &in) {
		return
	}
	id, err := h.svc.CreateUser(actorOf(c), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"id": id})
}

func (h *handlers) userUpdate(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in UserInput
	if !bind(c, &in) {
		return
	}
	in.Password = "" // 编辑接口不接受密码
	err := h.svc.UpdateUser(actorOf(c), id, in)
	h.s.InvalidateUserStatus(id) // 停用 / 取消超级用户立即对该用户已有会话生效，不等 10 秒缓存过期
	done(c, err)
}

func (h *handlers) userActive(active bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if id, ok := idParam(c); ok {
			err := h.svc.SetUserActive(actorOf(c), id, active)
			h.s.InvalidateUserStatus(id) // 立即对该用户已有会话生效
			done(c, err)
		}
	}
}

func (h *handlers) userUnlock(c *gin.Context) {
	if id, ok := idParam(c); ok {
		done(c, h.svc.UnlockUser(actorOf(c), id))
	}
}

func (h *handlers) userResetPassword(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var req ResetPasswordOpts
	if !bind(c, &req) {
		return
	}
	res, err := h.svc.ResetPasswordEx(actorOf(c), id, req)
	if err != nil {
		writeErr(c, err)
		return
	}
	// 系统生成的临时密码只在这一次响应里返回，不缓存、不落日志
	c.Header("Cache-Control", "no-store")
	out := gin.H{"ok": true, "force_change": res.ForceChange}
	if res.Password != "" {
		out["password"] = res.Password
	}
	server.APIOK(c, out)
}

func (h *handlers) userReset2FA(c *gin.Context) {
	if id, ok := idParam(c); ok {
		done(c, h.svc.ResetTwoFA(actorOf(c), id))
	}
}

// ---- 权限组 ----

func (h *handlers) agList(c *gin.Context) {
	list, err := h.svc.ListAuthGroups()
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, list)
}

func (h *handlers) agGet(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	d, err := h.svc.GetAuthGroup(id)
	if err != nil {
		writeErr(c, err)
		return
	}
	localizeFlowRefs(reqLang(c), d.Flows)
	server.APIOK(c, d)
}

func (h *handlers) agCreate(c *gin.Context) {
	var in AuthGroupInput
	if !bind(c, &in) {
		return
	}
	id, err := h.svc.CreateAuthGroup(actorOf(c), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, gin.H{"id": id})
}

func (h *handlers) agUpdate(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in AuthGroupInput
	if !bind(c, &in) {
		return
	}
	done(c, h.svc.UpdateAuthGroup(actorOf(c), id, in))
}

func (h *handlers) agDelete(c *gin.Context) {
	if id, ok := idParam(c); ok {
		done(c, h.svc.DeleteAuthGroup(actorOf(c), id))
	}
}

// ---- 系统审计 ----

func (h *handlers) auditOptions(c *gin.Context) {
	o, err := h.svc.AuditOptions()
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, o)
}

func auditQueryOf(c *gin.Context) AuditQuery {
	return AuditQuery{Action: c.Query("action"), User: c.Query("user"), Search: c.Query("search"), StartDate: c.Query("start_date"),
		EndDate: c.Query("end_date"), Page: queryInt(c, "page", 1), PageSize: queryInt(c, "page_size", 20),
		Category: c.Query("category"), Result: c.Query("result"), Risky: truthy(c.Query("risky"))}
}

func (h *handlers) auditLog(c *gin.Context) {
	p, err := h.svc.ListAuditLogs(auditQueryOf(c))
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, p)
}

// csvSafe 防止 CSV 公式注入：以 = + - @ 制表符 回车开头的单元格前面补一个单引号。
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (h *handlers) auditExport(c *gin.Context) {
	p := server.PrincipalOf(c)
	op, err := h.svc.Operator(p.ID)
	if err != nil {
		writeErr(c, err)
		return
	}
	rows, truncated, err := h.svc.ExportAuditLogs(op, auditQueryOf(c))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="audit-log-`+time.Now().Format("20060102-150405")+`.csv"`)
	if truncated {
		c.Header("X-Export-Truncated", "1")
	}
	c.Status(http.StatusOK)
	_, _ = c.Writer.WriteString("\xEF\xBB\xBF") // UTF-8 BOM，Excel 才能正确识别中文
	// 表头与系统定义的类别 / 动作 / 结果名按请求语言输出；用户名、额外信息等存库的内容原样。
	lang := reqLang(c)
	w := csv.NewWriter(c.Writer)
	header := []string{"用户名", "姓名", "来源IP", "类别", "模块", "权限码", "动作", "结果", "高危", "额外信息", "操作时间"}
	for i, k := range header {
		header[i] = i18n.T(lang, k)
	}
	_ = w.Write(header)
	for _, r := range rows {
		risky := ""
		if r.Risky {
			risky = i18n.T(lang, "是")
		}
		_ = w.Write([]string{csvSafe(r.UserName), csvSafe(r.UserDisplay), csvSafe(r.IP), csvSafe(i18n.TP(lang, r.CategoryLabel)), csvSafe(i18n.TP(lang, r.Module)), csvSafe(r.Code),
			csvSafe(i18n.TP(lang, r.Action)), csvSafe(i18n.TP(lang, r.Result)), risky, csvSafe(r.ExtraInfo), r.ActionTime})
	}
	w.Flush()
}

func (h *handlers) auditSQLWorkflows(c *gin.Context) {
	gid, ok := queryUint(c, "group_id")
	if !ok {
		return
	}
	p, err := h.svc.ListSQLWorkflows(SQLWorkflowQuery{Status: c.Query("status"), GroupID: gid, Search: c.Query("search"),
		StartDate: c.Query("start_date"), EndDate: c.Query("end_date"), Page: queryInt(c, "page", 1), PageSize: queryInt(c, "page_size", 20)})
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, p)
}

func (h *handlers) auditQueryLogs(c *gin.Context) {
	p, err := h.svc.ListQueryLogs(queryAuditOf(c))
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, p)
}
