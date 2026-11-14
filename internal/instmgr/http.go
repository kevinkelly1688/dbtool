// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package instmgr

import (
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/i18n"
	"dbtool/internal/server"
)

// 菜单与页面权限：侧栏“实例运维”下的五个入口，各按对应页面的“打开 / 查看”按钮显示；没有权限就看不到。
type navDef struct {
	Key, Href, Label string
	Order            int
	Button           string
}

var navDefs = []navDef{
	{"inst-list", "/instances", "实例列表", 10, "instance.view"},
	{"inst-sessions", "/instances/sessions", "会话诊断", 20, "session.open"},
	{"inst-databases", "/instances/databases", "数据库", 30, "database.view"},
	{"inst-accounts", "/instances/accounts", "帐号维护", 40, "account.view"},
	{"inst-params", "/instances/params", "参数管理", 50, "param.open"},
}

// 前端按钮显隐用到的按钮码（登记表 internal/btnperm）：/api/v1/instmgr/caps 一次返回，键就是按钮码。
var capButtons = []string{
	"instance.view", "instance.add", "instance.edit", "instance.delete", "instance.test", "instance.purge_binlog",
	"session.open", "session.process", "session.tablespace", "session.trx", "session.locks", "session.kill",
	"database.view", "database.create", "database.edit",
	"account.view", "account.create", "account.edit", "account.grant", "account.pwd", "account.lock", "account.delete",
	"param.open", "param.view", "param.edit", "param.template",
}

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, mirage, ok := s.DB()
		if !ok {
			return
		}
		registerWith(s, rt, NewService(newStore(db, mirage), NewDialer()))
	})
	for _, n := range navDefs {
		n := n
		server.AddMenu(server.MenuItem{Group: "inst", Key: n.Key, Href: n.Href, Label: n.Label, Order: n.Order,
			Show: func(s *server.Server, a audit.Actor) bool {
				if _, _, ok := s.DB(); !ok {
					return false
				}
				return menuAllowed(s, a, n.Button)
			}})
	}
}

// menuAllowed 判断顶栏菜单是否对该用户可见。
func menuAllowed(s *server.Server, a audit.Actor, code string) bool { return s.ActorHasButton(a, code) }

type handlers struct {
	s   *server.Server
	svc *Service
}

// pageData 是五个页面共用的模板数据：左侧二级菜单。
type pageData struct {
	Side []sideLink
	// SelCount 是实例列表里“已选 N 个”的数字（带 id 的 <b>，脚本会刷新它）：句子整句交给 {{th}} 翻译，数字以 HTML 原样代入。
	SelCount template.HTML
}

// selCountHTML 是 SelCount 的初值。
const selCountHTML = template.HTML(`<b id="il-sel-n">0</b>`) // #nosec G203 -- 常量

type sideLink struct {
	Href, Label string
	On          bool
}

// registerWith 登记路由；测试里直接传入用假实现构造的 Service。
func registerWith(s *server.Server, rt server.Routes, svc *Service) {
	h := &handlers{s: s, svc: svc}
	btn := s.RequireButtonAPI // 持有任一所列按钮即可（超级用户通过）

	pg := func(path, tpl, title, nav, code string) {
		rt.Pages.GET(path, s.RequireButtonPage(code), func(c *gin.Context) {
			s.Render(c, http.StatusOK, tpl, server.PageData{Title: title, Nav: nav, Data: pageData{Side: h.side(c, nav), SelCount: selCountHTML}})
		})
	}
	pg("/instances", "instmgr/list.html", "实例列表", "inst-list", "instance.view")
	pg("/instances/sessions", "instmgr/sessions.html", "会话诊断", "inst-sessions", "session.open")
	pg("/instances/databases", "instmgr/databases.html", "数据库", "inst-databases", "database.view")
	pg("/instances/accounts", "instmgr/accounts.html", "帐号维护", "inst-accounts", "account.view")
	pg("/instances/params", "instmgr/params.html", "参数管理", "inst-params", "param.open")

	a := rt.API
	a.GET("/instmgr/caps", h.apiCaps)
	a.GET("/instmgr/choices", h.apiChoices)

	// 实例列表
	a.GET("/instances", btn("instance.view"), h.apiList)
	a.GET("/instances/form-options", btn("instance.view", "instance.add", "instance.edit"), h.apiFormOptions)
	a.POST("/instances", btn("instance.add"), h.apiCreate)
	a.POST("/instances/test", btn("instance.add", "instance.edit", "instance.test"), h.apiTestForm)
	a.PUT("/instances/:id", btn("instance.edit"), h.apiUpdate)
	a.DELETE("/instances/:id", btn("instance.delete"), h.apiDelete)
	a.GET("/instances/:id/refs", btn("instance.delete"), h.apiRefs)
	// 批量操作：逐个走与单个实例相同的路径（范围、状态、引用检查、审计），按钮权限与单个操作一致；删除要“删除实例”按钮。
	a.POST("/instances/batch/disable", btn("instance.edit"), h.apiBatchDisable)
	a.POST("/instances/batch/collect", btn("instance.edit"), h.apiBatchCollect) // 批量开启 / 关闭慢日志采集
	a.POST("/instances/batch/env", btn("instance.edit"), h.apiBatchEnv)         // 批量设置环境（生产 / 测试 / 未设置）
	a.POST("/instances/batch/delete", btn("instance.delete"), h.apiBatchDelete)
	a.POST("/instances/:id/enable", btn("instance.edit"), h.apiEnable)
	a.POST("/instances/:id/disable", btn("instance.edit"), h.apiDisable) // 实例级停用 / 启用（旁路表），区别于上面的慢日志采集开关
	a.GET("/instances/:id/health", btn("instance.view"), h.apiHealth)    // 健康状态 / QPS / 连接 / 版本（页面逐行异步调用）
	a.POST("/instances/:id/test", btn("instance.add", "instance.edit", "instance.test"), h.apiTestSaved)
	a.GET("/instances/:id/binlogs", btn("instance.purge_binlog"), h.apiBinlogs)
	a.POST("/instances/:id/binlogs/preview", btn("instance.purge_binlog"), h.apiBinlogPreview)
	a.POST("/instances/:id/binlogs/purge", btn("instance.purge_binlog"), h.apiBinlogPurge)

	// 会话管理
	a.GET("/instances/:id/processlist", btn("session.process"), h.apiProcesslist)
	a.GET("/instances/:id/tablespace", btn("session.tablespace"), h.apiTablespace)
	a.GET("/instances/:id/transactions", btn("session.trx"), h.apiTransactions)
	a.GET("/instances/:id/lock-waits", btn("session.locks"), h.apiLockWaits)
	a.POST("/instances/:id/kill/preview", btn("session.kill"), h.apiKillPreview)
	a.POST("/instances/:id/kill", btn("session.kill"), h.apiKill)

	// 数据库管理
	a.GET("/instances/:id/databases", btn("database.view"), h.apiDatabases)
	a.GET("/instances/:id/databases/options", btn("database.view"), h.apiDatabaseOptions)
	a.POST("/instances/:id/databases", btn("database.create"), h.apiCreateDatabase)
	a.PUT("/instances/:id/databases/info", btn("database.edit"), h.apiSaveDatabaseInfo)

	// 账号管理
	a.GET("/instances/:id/accounts", btn("account.view"), h.apiAccounts)
	a.GET("/instances/:id/accounts/objects", btn("account.create", "account.grant"), h.apiAccountObjects)
	a.GET("/instances/:id/accounts/owners", btn("account.create", "account.edit"), h.apiAccountOwners)
	a.POST("/instances/:id/accounts", btn("account.create"), h.apiCreateAccounts)
	a.PUT("/instances/:id/accounts/info", btn("account.edit"), h.apiSaveAccountInfo)
	a.POST("/instances/:id/accounts/grant/preview", btn("account.grant"), h.apiGrantPreview)
	a.POST("/instances/:id/accounts/grant", btn("account.grant"), h.apiGrant)
	a.POST("/instances/:id/accounts/password", btn("account.pwd"), h.apiResetPassword)
	a.POST("/instances/:id/accounts/lock", btn("account.lock"), h.apiLockAccount)
	a.POST("/instances/:id/accounts/drop", btn("account.delete"), h.apiDropAccount)

	// 参数配置
	a.GET("/instances/:id/params", btn("param.view"), h.apiParams)
	a.POST("/instances/:id/params/preview", btn("param.edit"), h.apiParamPreview)
	a.PUT("/instances/:id/params", btn("param.edit"), h.apiSetParam)
	a.GET("/instances/:id/params/history", btn("param.view"), h.apiParamHistory)
	a.GET("/params/history", btn("param.view"), h.apiParamHistoryAll)
	a.GET("/params/templates", btn("param.view"), h.apiTemplates)
	a.POST("/params/templates", btn("param.template"), h.apiCreateTemplate)
	a.PUT("/params/templates/:id", btn("param.template"), h.apiUpdateTemplate)
}

// ---- 通用 ----

// caller 是当前用户；可见范围按 query.admin_run（查看 / 查询全部实例）决定（超级用户恒真）。
func (h *handlers) caller(c *gin.Context) Caller {
	p := server.PrincipalOf(c)
	return Caller{ID: p.ID, Username: p.Username, Display: p.Display, IsSuperuser: p.IsSuperuser, AllInstances: h.s.HasButton(c, "query.admin_run"),
		Lang: i18n.FromContext(c.Request.Context())}
}

// manager 是实例管理类操作的调用者：持有新增/修改/删除实例权限的人能看到全部实例。
func (h *handlers) manager(c *gin.Context) Caller {
	cl := h.caller(c)
	cl.AllInstances = true
	return cl
}

// side 生成左侧二级菜单：只含当前用户有权限的页面。
func (h *handlers) side(c *gin.Context, active string) []sideLink {
	var out []sideLink
	for _, n := range navDefs {
		if h.s.HasButton(c, n.Button) {
			out = append(out, sideLink{Href: n.Href, Label: n.Label, On: n.Key == active})
		}
	}
	return out
}

func (h *handlers) fail(c *gin.Context, err error) {
	var e *Error
	if errors.As(err, &e) {
		server.APIFail(c, e.Status, e.Code, e.Message)
		return
	}
	slog.Error("实例管理接口出错", "path", c.Request.URL.Path, "err", err)
	server.APIFail(c, http.StatusInternalServerError, "internal", "服务器内部错误，请稍后重试")
}

func bind(c *gin.Context, v any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	if err := c.ShouldBindJSON(v); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return false
	}
	return true
}

func idParam(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "编号不合法")
		return 0, false
	}
	return uint(n), true
}

func intQuery(c *gin.Context, key string, def int) int {
	n, err := strconv.Atoi(c.Query(key))
	if err != nil {
		return def
	}
	return n
}

func (h *handlers) apiCaps(c *gin.Context) {
	caps := map[string]bool{}
	for _, code := range capButtons { // 前端据此隐藏 / 置灰按钮
		caps[code] = h.s.HasButton(c, code)
	}
	server.APIOK(c, caps)
}

// featureButton 是各页面实例下拉对应的页面按钮。
var featureButton = map[string]string{"list": "instance.view", "sessions": "session.open", "databases": "database.view",
	"accounts": "account.view", "params": "param.open"}

func (h *handlers) apiChoices(c *gin.Context) {
	feature := c.Query("feature")
	code, ok := featureButton[feature]
	if !ok {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "未知的页面")
		return
	}
	if !h.s.HasButton(c, code) {
		server.APIFail(c, http.StatusForbidden, "forbidden", "没有权限执行该操作")
		return
	}
	list, err := h.svc.Choices(h.caller(c), feature)
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, list)
}

// ---- 实例列表 ----

func (h *handlers) apiList(c *gin.Context) {
	res, err := h.svc.ListInstances(h.listCaller(c), ListQuery{Role: c.Query("type"), DBType: c.Query("db_type"), Tag: c.Query("tag"), Status: c.Query("status"),
		State: c.Query("state"), Env: c.Query("env"), Q: c.Query("q"), Page: intQuery(c, "page", 1), PageSize: intQuery(c, "page_size", 20)})
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res) // {"total":…, "items":[…]}
}

// listCaller 是实例列表类接口的调用者：能新增/修改/删除实例的人看到全部实例。
func (h *handlers) listCaller(c *gin.Context) Caller {
	cl := h.caller(c)
	cl.AllInstances = cl.AllInstances || h.s.HasButton(c, "instance.add") || h.s.HasButton(c, "instance.edit") || h.s.HasButton(c, "instance.delete")
	return cl
}

func (h *handlers) apiFormOptions(c *gin.Context) {
	o, err := h.svc.FormOptions()
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, o)
}

func (h *handlers) apiCreate(c *gin.Context) {
	var in InstanceInput
	if !bind(c, &in) {
		return
	}
	it, err := h.svc.SaveInstance(h.manager(c), 0, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, it)
}

func (h *handlers) apiUpdate(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in InstanceInput
	if !bind(c, &in) {
		return
	}
	it, err := h.svc.SaveInstance(h.manager(c), id, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, it)
}

type confirmReq struct {
	Confirm bool `json:"confirm"`
}

func (h *handlers) apiDelete(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in confirmReq
	if !bind(c, &in) {
		return
	}
	if err := h.svc.DeleteInstance(h.manager(c), id, in.Confirm); err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

type batchReq struct {
	IDs      []uint  `json:"ids"`
	Disabled bool    `json:"disabled"` // 停用（true）/ 启用（false）
	Enable   bool    `json:"enable"`   // 批量采集：开启（true）/ 关闭（false）慢日志采集
	Confirm  bool    `json:"confirm"`
	DryRun   bool    `json:"dry_run"`
	Env      *string `json:"env"` // 批量设置环境：prod / test / 空串（未设置）；缺省（null）表示没有给，拒绝
}

func (h *handlers) apiBatchEnv(c *gin.Context) {
	var in batchReq
	if !bind(c, &in) {
		return
	}
	if in.Env == nil {
		h.fail(c, bad("请选择环境"))
		return
	}
	res, err := h.svc.BatchSetEnv(h.manager(c), in.IDs, *in.Env)
	if err != nil {
		h.fail(c, err)
		return
	}
	trBatch(c, res)
	server.APIOK(c, res)
}

func (h *handlers) apiBatchCollect(c *gin.Context) {
	var in batchReq
	if !bind(c, &in) {
		return
	}
	res, err := h.svc.BatchCollect(h.manager(c), in.IDs, in.Enable)
	if err != nil {
		h.fail(c, err)
		return
	}
	trBatch(c, res)
	server.APIOK(c, res)
}

func (h *handlers) apiBatchDisable(c *gin.Context) {
	var in batchReq
	if !bind(c, &in) {
		return
	}
	res, err := h.svc.BatchDisable(h.manager(c), in.IDs, in.Disabled)
	if err != nil {
		h.fail(c, err)
		return
	}
	trBatch(c, res)
	server.APIOK(c, res)
}

func (h *handlers) apiBatchDelete(c *gin.Context) {
	var in batchReq
	if !bind(c, &in) {
		return
	}
	res, err := h.svc.BatchDelete(h.manager(c), in.IDs, in.Confirm, in.DryRun)
	if err != nil {
		h.fail(c, err)
		return
	}
	trBatch(c, res)
	server.APIOK(c, res)
}

func (h *handlers) apiRefs(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	refs, err := h.svc.InstanceRefs(h.manager(c), id)
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, refs)
}

func (h *handlers) apiEnable(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in struct {
		Enable bool `json:"enable"`
	}
	if !bind(c, &in) {
		return
	}
	if err := h.svc.SetInstanceEnable(h.manager(c), id, in.Enable); err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiDisable(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in struct {
		Disabled bool `json:"disabled"`
	}
	if !bind(c, &in) {
		return
	}
	if err := h.svc.SetInstanceDisabled(h.manager(c), id, in.Disabled); err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiHealth(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	res, err := h.svc.Health(c.Request.Context(), h.listCaller(c), id, c.Query("force") == "1")
	if err != nil {
		h.fail(c, err)
		return
	}
	out := *res                         // 探测结果在缓存里存的是中文原文，翻译只作用在这份拷贝上
	out.Message = trMsg(c, out.Message) // 未知 / 告警的原因
	server.APIOK(c, &out)
}

func (h *handlers) apiTestSaved(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	res, err := h.svc.TestInstance(c.Request.Context(), h.manager(c), id)
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res)
}

type testReq struct {
	ID uint `json:"id"`
	InstanceInput
}

func (h *handlers) apiTestForm(c *gin.Context) {
	var in testReq
	if !bind(c, &in) {
		return
	}
	res, err := h.svc.TestForm(c.Request.Context(), h.manager(c), in.ID, in.InstanceInput)
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) apiBinlogs(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	list, err := h.svc.ListBinlogs(c.Request.Context(), h.caller(c), id)
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, list)
}

type purgeReq struct {
	To      string `json:"to"`
	Confirm string `json:"confirm"`
}

func (h *handlers) apiBinlogPreview(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in purgeReq
	if !bind(c, &in) {
		return
	}
	plan, err := h.svc.PreviewPurge(c.Request.Context(), h.caller(c), id, in.To)
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, plan)
}

func (h *handlers) apiBinlogPurge(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in purgeReq
	if !bind(c, &in) {
		return
	}
	plan, err := h.svc.PurgeBinlogs(c.Request.Context(), h.caller(c), id, in.To, in.Confirm)
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, plan)
}

// ---- 会话管理 ----

func (h *handlers) apiProcesslist(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	res, err := h.svc.Processlist(c.Request.Context(), h.caller(c), id, ProcFilter{Command: c.Query("command"), User: c.Query("user"),
		DB: c.Query("db"), MinTime: intQuery(c, "min_time", 0), Q: c.Query("q")})
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) apiTablespace(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	res, err := h.svc.Tablespace(c.Request.Context(), h.caller(c), id, intQuery(c, "offset", 0), intQuery(c, "limit", 20))
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) apiTransactions(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	res, err := h.svc.Transactions(c.Request.Context(), h.caller(c), id, TrxFilter{MinSeconds: intQuery(c, "min_seconds", 0), IdleOnly: c.Query("idle_only") == "1"})
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) apiLockWaits(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if c.Query("chains") == "1" { // 带阻塞链的新形态；默认形态（纯数组）保持不变
		view, err := h.svc.LockWaitsWithChains(c.Request.Context(), h.caller(c), id)
		if err != nil {
			h.fail(c, err)
			return
		}
		server.APIOK(c, view)
		return
	}
	res, err := h.svc.LockWaits(c.Request.Context(), h.caller(c), id)
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res)
}

type killReq struct {
	Threads []KillTarget `json:"threads"`
	Confirm bool         `json:"confirm"`
}

func (h *handlers) apiKillPreview(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in killReq
	if !bind(c, &in) {
		return
	}
	plan, err := h.svc.PreviewKill(c.Request.Context(), h.caller(c), id, in.Threads)
	if err != nil {
		h.fail(c, err)
		return
	}
	for i := range plan.Items {
		plan.Items[i].Reason = trMsg(c, plan.Items[i].Reason)
	}
	server.APIOK(c, plan)
}

func (h *handlers) apiKill(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in killReq
	if !bind(c, &in) {
		return
	}
	res, err := h.svc.Kill(c.Request.Context(), h.caller(c), id, in.Threads, in.Confirm)
	if err != nil {
		h.fail(c, err)
		return
	}
	for i := range res {
		res[i].Message = trMsg(c, res[i].Message)
	}
	server.APIOK(c, res)
}

// ---- 数据库管理 ----

func (h *handlers) apiDatabases(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	res, err := h.svc.ListDatabasesScoped(c.Request.Context(), h.caller(c), id, c.Query("saved"), c.Query("q"))
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) apiDatabaseOptions(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	res, err := h.svc.DatabaseOptions(c.Request.Context(), h.caller(c), id, c.Query("owners_only") == "1")
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) apiCreateDatabase(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in DBCreate
	if !bind(c, &in) {
		return
	}
	if err := h.svc.CreateDatabase(c.Request.Context(), h.caller(c), id, in); err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiSaveDatabaseInfo(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in DBInfo
	if !bind(c, &in) {
		return
	}
	if err := h.svc.SaveDatabaseInfo(h.caller(c), id, in); err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

// ---- 账号管理 ----

func (h *handlers) apiAccounts(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	res, err := h.svc.ListAccountsFiltered(c.Request.Context(), h.caller(c), id, AccountFilter{Saved: c.Query("saved"), Risk: c.Query("risk") == "1",
		Locked: c.Query("locked") == "1", Q: c.Query("q")})
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) apiAccountObjects(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	res, err := h.svc.ListObjects(c.Request.Context(), h.caller(c), id, c.Query("db"), c.Query("table"))
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) apiAccountOwners(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	res, err := h.svc.AccountOwners(h.caller(c), id)
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) apiCreateAccounts(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in AccountCreate
	if !bind(c, &in) {
		return
	}
	res, err := h.svc.CreateAccounts(c.Request.Context(), h.caller(c), id, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res)
}

func (h *handlers) apiSaveAccountInfo(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in AccountInfo
	if !bind(c, &in) {
		return
	}
	if err := h.svc.SaveAccountInfo(h.caller(c), id, in); err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiGrantPreview(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in GrantReq
	if !bind(c, &in) {
		return
	}
	plan, err := h.svc.PreviewGrant(c.Request.Context(), h.caller(c), id, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	trWarnings(c, plan)
	server.APIOK(c, plan)
}

func (h *handlers) apiGrant(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in GrantReq
	if !bind(c, &in) {
		return
	}
	plan, err := h.svc.Grant(c.Request.Context(), h.caller(c), id, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	trWarnings(c, plan)
	server.APIOK(c, plan)
}

func (h *handlers) apiResetPassword(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in PasswordReq
	if !bind(c, &in) {
		return
	}
	if err := h.svc.ResetPassword(c.Request.Context(), h.caller(c), id, in); err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiLockAccount(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in LockReq
	if !bind(c, &in) {
		return
	}
	if err := h.svc.SetAccountLock(c.Request.Context(), h.caller(c), id, in); err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiDropAccount(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in DropReq
	if !bind(c, &in) {
		return
	}
	if err := h.svc.DropAccount(c.Request.Context(), h.caller(c), id, in); err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

// ---- 参数配置 ----

func (h *handlers) apiParams(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	res, err := h.svc.ListParamsEx(c.Request.Context(), h.caller(c), id, ParamFilter{Editable: c.Query("editable"), Q: c.Query("q"), Deviation: c.Query("deviation") == "1"})
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res)
}

type paramReq struct {
	Name    string `json:"variable_name"`
	Value   string `json:"value"`
	Confirm bool   `json:"confirm"`
}

func (h *handlers) apiParamPreview(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in paramReq
	if !bind(c, &in) {
		return
	}
	plan, err := h.svc.PreviewParam(c.Request.Context(), h.caller(c), id, in.Name, in.Value)
	if err != nil {
		h.fail(c, err)
		return
	}
	plan.Note = trMsg(c, plan.Note)
	server.APIOK(c, plan)
}

func (h *handlers) apiSetParam(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in paramReq
	if !bind(c, &in) {
		return
	}
	plan, err := h.svc.SetParam(c.Request.Context(), h.caller(c), id, in.Name, in.Value, in.Confirm)
	if err != nil {
		h.fail(c, err)
		return
	}
	plan.Note = trMsg(c, plan.Note)
	server.APIOK(c, plan)
}

func (h *handlers) apiParamHistory(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	res, err := h.svc.ListParamChanges(h.caller(c), id, c.Query("q"), intQuery(c, "page", 1), intQuery(c, "page_size", 20))
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res) // {"total":…, "rows":[…]}
}

func (h *handlers) apiParamHistoryAll(c *gin.Context) {
	res, err := h.svc.ListParamChangesAll(h.caller(c), c.Query("q"), intQuery(c, "page", 1), intQuery(c, "page_size", 20))
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, res) // {"total":…, "rows":[…]}
}

func (h *handlers) apiTemplates(c *gin.Context) {
	list, err := h.svc.ListTemplateViews(strings.TrimSpace(c.Query("db_type")))
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, list)
}

func (h *handlers) apiCreateTemplate(c *gin.Context) {
	var in TemplateInput
	if !bind(c, &in) {
		return
	}
	t, err := h.svc.SaveTemplate(h.caller(c), 0, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, h.svc.TemplateViewOf(*t))
}

func (h *handlers) apiUpdateTemplate(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in TemplateInput
	if !bind(c, &in) {
		return
	}
	t, err := h.svc.SaveTemplate(h.caller(c), id, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	server.APIOK(c, h.svc.TemplateViewOf(*t))
}

// ---- 界面语言 ----

// trMsg 把运行时生成的中文提示按请求语言翻译（精确匹配目录，或按带 %s %d 的格式串匹配）；中文请求原样返回。
func trMsg(c *gin.Context, msg string) string {
	return i18n.TP(i18n.FromContext(c.Request.Context()), msg)
}

// trBatch 翻译批量操作里每个实例的结果说明。
func trBatch(c *gin.Context, res *BatchResult) {
	for i := range res.Items {
		res.Items[i].Message = trMsg(c, res.Items[i].Message)
	}
}

// trWarnings 翻译授权预览里的风险提示。
func trWarnings(c *gin.Context, plan *GrantPlan) {
	for i := range plan.Warnings {
		plan.Warnings[i] = trMsg(c, plan.Warnings[i])
	}
}
