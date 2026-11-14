// @author: kevinkelly
// @license: Apache Licence
// @file: server.go
// @time: 2026/10/28

// Package server 提供 HTTP 路由、会话鉴权与页面渲染。
package server

import (
	"bytes"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"dbtool/internal/btnperm"
	"dbtool/internal/config"
	"dbtool/internal/firewall"
	"dbtool/internal/i18n"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/privapply"
	"dbtool/internal/query"
	"dbtool/internal/sqlflow"
	"dbtool/web"
)

// Repo 汇总服务端需要的数据访问能力（store.Store 实现；测试/演示用假实现）。
type Repo interface {
	query.Repo
	GetUserByName(name string) (*model.User, error)
}

type Server struct {
	cfg   *config.Config
	repo  Repo
	key   []byte
	q     *query.Service
	tpl   map[string]map[string]*template.Template // 界面语言 → 页面 → 模板（按语言各编译一份，见 loadTemplates）
	limit *limiter
	apply *privapply.Service // 权限申请与审批；仓库不支持时为 nil，相关路由返回 404
	admin notify.Admin       // 通知渠道管理；仓库不支持时为 nil
	disp  *notify.Dispatcher // 通知发送器
	sql   *sqlflow.Service   // SQL 上线工单；未提供时相关路由返回 404
	login LoginExt           // 登录扩展（internal/authx 装配）；为 nil 时走内置的本地账号密码流程
	duty  DutyProvider       // 顶栏“值班”芯片的提供者（internal/duty 装配）；为 nil 时不显示

	btnChecker btnperm.Checker // 按钮权限检查器的显式注入（测试）；为空时取仓库提供的（见 button.go）

	auditSink func(model.AuditEntry) // 审计写入的显式注入（测试）；为空时写元数据库（见 audit_events.go）
	deny      denyDedup              // 无权限拒绝审计的去重（见 audit_events.go）

	nb badgeCache // 侧栏角标缓存（见 navbadge.go）

	ustat statusCache // 会话 / JWT 撤销用的用户状态缓存（见 userstatus.go）
	ready readyCache  // 就绪探针结果缓存（见 ready.go）
}

// Options 允许替换可变依赖（主要用于测试）。
type Options struct {
	Opener    query.Opener
	Auth      query.Authorizer
	PrivApply *privapply.Service           // 为空时，若仓库实现了 privapply.Repo 则自动创建
	SQLFlow   *sqlflow.Service             // SQL 上线工单服务（由调用方构造，因为它需要自己的仓库包装与后台调度）
	Hooks     []func(s *Server, rt Routes) // 额外登记的模块路由（测试里直接传入被测模块的登记函数）
	Buttons   btnperm.Checker              // 按钮权限检查器；为空时取仓库提供的（store.Store.Buttons），都没有则问仓库的 HasButton（测试 / 演示的内存仓库）
}

func New(cfg *config.Config, repo Repo, opt Options) (*gin.Engine, error) {
	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}
	auth := opt.Auth
	var ext *query.AutoExtractor
	if auth == nil {
		ext = query.NewAutoExtractor(repo)
		ext.Strict = cfg.RequireGoInception
		auth = query.NewPrivAuthorizer(repo, ext)
	}
	svc := query.NewService(repo, opt.Opener, auth)
	if ext != nil {
		svc.SetSelectListProvider(ext) // 脱敏与授权共用同一份 goInception 配置和严格模式
	}
	if p, ok := repo.(DBProvider); ok { // 有元数据库才启用 SQL 防火墙（测试 / 演示的内存仓库没有）
		svc.SetFirewall(firewall.New(firewall.NewGormStore(p.GormDB()), repo))
	}
	if opt.SQLFlow != nil {
		opt.SQLFlow.SetResultMasker(svc)        // 导出工单的结果与在线查询用同一套脱敏
		opt.SQLFlow.SetFirewall(svc.Firewall()) // SQL 工单与数据导出也过防火墙规则（没有元数据库时为 nil = 不启用）
		if pa, ok := auth.(*query.PrivAuthorizer); ok {
			opt.SQLFlow.SetExportAuthorizer(pa) // 数据导出的实例 / 库列表与提交校验用在线查询的库表权限
		}
	}
	notify.SetPublicURL(cfg.PublicURL, cfg.PublicURLSet) // 邮件正文里的链接基准（只在显式配置 PUBLIC_URL 时才放链接）
	s := &Server{
		cfg:   cfg,
		repo:  repo,
		key:   []byte(cfg.SecretKey),
		q:     svc,
		limit: newLimiter(5, 10*time.Minute),
	}
	if a, ok := repo.(notify.Admin); ok {
		s.admin = a
	}
	var notifier notify.Notifier = notify.Noop{}
	if src, ok := repo.(notify.Source); ok {
		if dir, ok := repo.(notify.Directory); ok {
			s.disp = notify.NewDispatcher(src, dir)
			notifier = s.disp
		}
	}
	s.apply = opt.PrivApply
	if s.apply == nil {
		if pr, ok := repo.(privapply.Repo); ok {
			s.apply = privapply.NewService(pr, notifier)
		}
	}
	if s.apply != nil {
		s.apply.BaseURL = cfg.PublicURL
	}
	s.sql = opt.SQLFlow
	s.btnChecker = opt.Buttons
	i18n.SetDefault(cfg.DefaultLang)
	if err := s.loadTemplates(); err != nil {
		return nil, err
	}

	r := gin.New()
	r.Use(gin.Recovery(), accessLogger(), securityHeaders(), gzipAPI(), auditIPBinder(), s.langMiddleware())

	static, err := fs.Sub(web.FS, "static")
	if err != nil {
		return nil, err
	}
	r.GET("/static/*filepath", func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache")
		http.StripPrefix("/static", http.FileServer(http.FS(static))).ServeHTTP(c.Writer, c.Request)
	})
	r.GET("/i18n/:file", s.i18nCatalog) // 前端译文目录（界面文案，不含业务数据，登录页也要用）
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/readyz", s.readyz) // 就绪探针：元数据库可达 + 核心表存在，失败 503（见 ready.go）

	// 页面
	r.GET("/login", s.loginPage)
	r.POST("/login", s.loginSubmit)
	pages := r.Group("", s.requirePage())
	pages.GET("/", s.homePage)
	pages.POST("/logout", s.logout)
	pages.GET("/query", s.queryPage)
	pages.GET("/ui/query/databases", s.uiDatabases)
	pages.GET("/ui/query/tree", s.uiTree)
	pages.GET("/ui/query/policy", s.uiPolicy)
	sp := pages.Group("/sqlworkflow", s.sqlEnabled())
	sp.GET("", s.sqlListPage)
	sp.GET("/new", s.sqlNewPage)
	sp.GET("/export/new", s.sqlExportNewPage)
	sp.GET("/:id", s.sqlDetailPage)
	np := pages.Group("/settings/notify", s.notifyAdminEnabled(), s.requireSuperuserPage())
	np.GET("", s.notifySettingsPage)
	ap := pages.Group("", s.applyEnabled())
	ap.GET("/query/applies", s.appliesPage)
	ap.GET("/query/applies/:id", s.applyDetailPage)
	ap.GET("/query/privileges", s.privilegesPage)

	// JSON API：同时接受浏览器会话（需 CSRF 头）与 Bearer JWT
	api := r.Group("/api/v1")
	api.POST("/auth/login", s.apiLogin)
	qa := api.Group("/query", s.requireAPI())
	qa.GET("/instances", s.apiInstances)
	qa.GET("/instances/:id/databases", s.apiDatabases)
	qa.GET("/instances/:id/databases/:db/tables", s.apiTables)
	qa.GET("/instances/:id/databases/:db/tables/:table/columns", s.apiColumns)
	qa.POST("/execute", s.apiExecute)
	qa.GET("/history", s.apiHistory)
	qa.GET("/history/users", s.apiHistoryUsers)
	ns := api.Group("/settings/notify", s.requireAPI(), s.notifyAdminEnabled(), s.requireSuperuserAPI())
	ns.GET("", s.apiNotifyOverview)
	ns.POST("/channels", s.apiSaveChannel)
	ns.PUT("/channels/:id", s.apiSaveChannel)
	ns.DELETE("/channels/:id", s.apiDeleteChannel)
	ns.POST("/test", s.apiTestChannel)
	sa := api.Group("/sqlworkflow", s.requireAPI(), s.sqlEnabled(), s.fillDisplay())
	sa.GET("/options", s.apiSQLOptions)
	sa.GET("/instances/:id/databases", s.apiSQLDatabases)
	sa.POST("/check", s.apiSQLCheck)
	sa.POST("", s.apiSQLSubmit)
	sa.GET("", s.apiSQLList)
	sa.GET("/:id", s.apiSQLDetail)
	sa.POST("/:id/pass", s.apiSQLPass)
	sa.POST("/:id/cancel", s.apiSQLCancel)
	sa.POST("/:id/run-date", s.apiSQLRunDate)
	sa.POST("/:id/execute", s.apiSQLExecute)
	sa.POST("/:id/schedule", s.apiSQLSchedule)
	sa.GET("/:id/download", s.apiSQLDownload)
	sa.GET("/:id/rollback", s.apiSQLRollback)
	sa.GET("/:id/rollback/download", s.apiSQLRollbackDownload)
	sa.POST("/:id/osc", s.apiSQLOsc)
	sa.POST("/:id/batch/:instance_id/retry", s.apiSQLBatchRetry)
	sa.POST("/:id/batch/:instance_id/close", s.apiSQLBatchClose)
	qp := qa.Group("", s.applyEnabled())
	qp.GET("/apply/options", s.apiApplyOptions)
	qp.POST("/privilege-applies", s.apiCreateApply)
	qp.GET("/privilege-applies", s.apiListApplies)
	qp.GET("/privilege-applies/:id", s.apiApplyDetail)
	qp.POST("/privilege-applies/:id/audit", s.apiAuditApply)
	qp.POST("/privilege-applies/:id/abort", s.apiAbortApply)
	qp.GET("/privileges", s.apiListPrivileges)
	qp.PATCH("/privileges/:id", s.RequireButtonAPIAudited("qpriv.manage"), s.apiModifyPrivilege)
	qp.DELETE("/privileges/:id", s.RequireButtonAPIAudited("qpriv.manage"), s.apiDeletePrivilege)

	// 其余模块（系统管理、实例管理等）在各自文件的 init() 里登记路由。
	rt := Routes{Pages: pages, API: api.Group("", s.requireAPI()), Public: r.Group("")}
	for _, h := range routeHooks {
		h(s, rt)
	}
	for _, h := range opt.Hooks {
		h(s, rt)
	}
	return r, nil
}

// Routes 是模块登记路由时用的分组：Pages 需要登录会话，API 同时接受会话（带 CSRF）与 Bearer JWT，Public 不需要登录。
type Routes struct {
	Pages  *gin.RouterGroup
	API    *gin.RouterGroup
	Public *gin.RouterGroup // 不需要登录的路由（登录流程本身：SSO 回调、两步验证页等），模块自己负责鉴权与 CSRF
}

var routeHooks []func(s *Server, rt Routes)

// addRoutes 由各模块在 init() 里调用。模块自己判断仓库是否具备所需能力，不具备就不登记（路由即 404）。
func addRoutes(h func(s *Server, rt Routes)) { routeHooks = append(routeHooks, h) }

func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		// 不使用内联脚本；ACE 会注入 <style>，故样式允许 'unsafe-inline'；其语法高亮 worker 用 blob。
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; worker-src 'self' blob:; frame-ancestors 'none'")
		c.Next()
	}
}

// ---- 模板 ----

type pageData struct {
	Title    string
	Skin     string
	Lang     string // 界面语言（zh-CN / en），render 填写；模板里 <html lang="{{.Lang}}">
	Nav      string
	Username string
	Display  string
	CSRF     string
	Error    string
	Next     string
	Data     any
	Nav2     navData   // 顶栏导航里哪些入口对当前用户可见
	Btn      *BtnCtx   // 按钮级权限：{{if .Btn.Can "sqlworkflow.approve"}}（见 button.go）
	Site     siteInfo  // 侧栏底部的版本号与页面底部的 ICP 备案号（来自配置文件 [site]），render 填写
	Duty     *DutyChip // 顶栏时钟左边的“值班”芯片（见 duty.go）；nil 不显示，render 填写
}

// siteInfo 是布局里展示的站点信息；字段为空的部分不显示。
type siteInfo struct {
	Version string // 已加好 v 前缀的版本号，例如 v1.05
	ICP     string
	ICPURL  string
}

// siteInfoOf 从配置生成站点信息。版本号以数字开头时补一个 v（1.05 → v1.05），dev、v1.05 这类原样显示。
func siteInfoOf(cfg *config.Config) siteInfo {
	si := siteInfo{Version: cfg.Version, ICP: cfg.ICP, ICPURL: cfg.ICPURL}
	if v := si.Version; v != "" && v[0] >= '0' && v[0] <= '9' {
		si.Version = "v" + v
	}
	if si.ICPURL == "" {
		si.ICPURL = config.DefaultICPURL // 测试 / 演示直接构造 Config 时不会经过 LoadFile
	}
	return si
}

// loadTemplates 把 templates 目录下除 layout.html 与 partials 之外的每个 .html 当作一个页面，
// 键为相对 templates 的路径（如 "query/index.html"），新增页面不需要改这里。
// 每种界面语言各编译一份（t 函数绑定该语言），请求时直接取对应语言的模板，没有每请求克隆模板的开销。
func (s *Server) loadTemplates() error {
	s.tpl = map[string]map[string]*template.Template{}
	var pages []string
	err := fs.WalkDir(web.FS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, "templates/")
		if d.IsDir() || !strings.HasSuffix(rel, ".html") || rel == "layout.html" || strings.HasPrefix(rel, "partials/") {
			return nil
		}
		pages = append(pages, rel)
		return nil
	})
	if err != nil {
		return err
	}
	for _, l := range i18n.Langs {
		funcs := funcsFor(l.Code)
		set := map[string]*template.Template{}
		for _, page := range pages {
			t, err := template.New("").Funcs(funcs).ParseFS(web.FS, "templates/layout.html", "templates/"+page, "templates/partials/*.html")
			if err != nil {
				return err
			}
			set[page] = t
		}
		// 片段模板只需要 partials
		frag, err := template.New("").Funcs(funcs).ParseFS(web.FS, "templates/partials/*.html")
		if err != nil {
			return err
		}
		set["partials"] = frag
		s.tpl[l.Code] = set
	}
	return nil
}

// tplFor 取某语言的模板集；没有该语言时回落中文。
func (s *Server) tplFor(lang string) map[string]*template.Template {
	if set := s.tpl[lang]; set != nil {
		return set
	}
	return s.tpl[i18n.ZH]
}

func (s *Server) render(c *gin.Context, status int, page string, d pageData) {
	lang := langOf(c)
	d.Lang = lang
	d.Title = i18n.TP(lang, d.Title) // 常量标题 / 提示按原文查目录（含带 %s 的格式串）；带动态内容的 handler 也可自己用 tr 翻译
	d.Error = i18n.TP(lang, d.Error)
	d.Skin = skinOf(c.Request)
	d.Site = siteInfoOf(s.cfg)
	d.Btn = s.newBtnCtx(c)
	if d.Username != "" {
		d.Nav2 = navData{Groups: s.menuFor(actorOf(c), d.Nav), Help: helpTopicFor(d.Nav)}
		d.Duty = s.dutyChip()
	}
	tp := s.tplFor(lang)[page]
	if tp == nil {
		c.String(http.StatusInternalServerError, tr(c, "页面模板不存在"))
		return
	}
	var buf bytes.Buffer // 先渲染到内存，出错时不输出半截页面
	if err := tp.ExecuteTemplate(&buf, "layout", d); err != nil {
		c.String(http.StatusInternalServerError, tr(c, "页面渲染失败"))
		_ = c.Error(err)
		return
	}
	c.Data(status, "text/html; charset=utf-8", buf.Bytes())
}

func (s *Server) fragment(c *gin.Context, status int, name string, data any) {
	if msg, ok := data.(string); ok { // alert / tree_hint 这类提示片段：按请求语言翻译（含带 %s 的格式串）
		data = trMsg(c, msg)
	}
	var buf bytes.Buffer
	if err := s.tplFor(langOf(c))["partials"].ExecuteTemplate(&buf, name, data); err != nil {
		c.String(http.StatusInternalServerError, tr(c, "渲染失败"))
		_ = c.Error(err)
		return
	}
	c.Data(status, "text/html; charset=utf-8", buf.Bytes())
}

// ---- 认证中间件 ----

const principalKey = "principal"
const sessionKey = "session"

func principalOf(c *gin.Context) query.Principal {
	p, _ := c.Get(principalKey)
	pr, _ := p.(query.Principal)
	return pr
}

func sessionOf(c *gin.Context) *Session {
	v, _ := c.Get(sessionKey)
	sess, _ := v.(*Session)
	return sess
}

// requirePage 保护页面：无有效会话则跳转登录页。
func (s *Server) requirePage() gin.HandlerFunc {
	return func(c *gin.Context) {
		sess := s.sessionFrom(c.Request)
		if sess == nil {
			if strings.HasPrefix(c.Request.URL.Path, "/ui/") {
				c.AbortWithStatus(http.StatusUnauthorized) // 片段请求不跳转，让前端提示登录过期
				return
			}
			c.Redirect(http.StatusFound, "/login?next="+safeNextEscape(c.Request.URL.RequestURI()))
			c.Abort()
			return
		}
		if !s.sessionStillValid(sess) { // 账号已被停用 / 删除：清会话，按未登录处理
			clearSession(c.Writer)
			if strings.HasPrefix(c.Request.URL.Path, "/ui/") {
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			c.Redirect(http.StatusFound, "/login?next="+safeNextEscape(c.Request.URL.RequestURI()))
			c.Abort()
			return
		}
		s.touchSession(c, sess)
		c.Set(sessionKey, sess)
		c.Set(principalKey, query.Principal{ID: sess.UID, Username: sess.Username, Display: sess.Display, IsSuperuser: sess.IsSuperuser})
		if s.passwordGate(c, sess, true) { // 被要求先改密的账号：只放行改密页与退出（见 ext.go）
			return
		}
		c.Next()
	}
}

// requireAPI 接受会话 Cookie（非 GET 需要 X-CSRF-Token）或 Bearer JWT。
func (s *Server) requireAPI() gin.HandlerFunc {
	return func(c *gin.Context) {
		if raw, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer "); ok {
			var cl claims
			tok, err := jwt.ParseWithClaims(raw, &cl, func(*jwt.Token) (any, error) { return s.key, nil },
				jwt.WithValidMethods([]string{"HS256"}))
			if err != nil || !tok.Valid {
				apiFail(c, http.StatusUnauthorized, "unauthorized", "令牌无效或已过期")
				return
			}
			live, super := s.liveStatus(cl.UID, cl.IsSuperuser) // 账号被停用 / 删除则令牌立即失效；超级用户标记以库里最新值为准
			if !live {
				apiFail(c, http.StatusUnauthorized, "unauthorized", "账号已停用或不存在，请联系管理员")
				return
			}
			cl.IsSuperuser = super
			if s.login != nil && !s.login.APIAllowed(cl.UID) { // API 用户白名单（security.api_user_ids）
				apiFail(c, http.StatusForbidden, "forbidden", "该用户不在 API 白名单内")
				return
			}
			c.Set(principalKey, query.Principal{ID: cl.UID, Username: cl.Username, IsSuperuser: cl.IsSuperuser})
			c.Next()
			return
		}
		sess := s.sessionFrom(c.Request)
		if sess == nil {
			apiFail(c, http.StatusUnauthorized, "unauthorized", "未登录或登录已过期")
			return
		}
		if !s.sessionStillValid(sess) {
			clearSession(c.Writer)
			apiFail(c, http.StatusUnauthorized, "unauthorized", "账号已停用或不存在，请重新登录")
			return
		}
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			if !constTimeEq(c.GetHeader("X-CSRF-Token"), sess.CSRF) {
				apiFail(c, http.StatusForbidden, "csrf", "CSRF 校验失败，请刷新页面后重试")
				return
			}
		}
		if c.GetHeader(BackgroundPollHeader) == "" { // 页面自己定时发的请求（顶栏值班芯片）不算用户在操作，不给会话续期，否则一个放着不管的页面会让会话永不过期
			s.touchSession(c, sess)
		}
		c.Set(sessionKey, sess)
		c.Set(principalKey, query.Principal{ID: sess.UID, Username: sess.Username, Display: sess.Display, IsSuperuser: sess.IsSuperuser})
		if s.passwordGate(c, sess, false) {
			return
		}
		c.Next()
	}
}

// sessionStillValid 回库（带短缓存）确认会话用户仍然启用，并把会话里的超级用户标记刷新为库里的最新值。
// 返回 false 表示账号已停用 / 不存在，调用方应清会话并按未登录处理。库不可用时放行（沿用会话里的值）。
func (s *Server) sessionStillValid(sess *Session) bool {
	live, super := s.liveStatus(sess.UID, sess.IsSuperuser)
	if !live {
		return false
	}
	sess.IsSuperuser = super
	return true
}

// apiFail 返回统一的错误 JSON。msg 是中文原文，按请求语言在这里翻译（带动态内容的先用 tr 翻译再传入）。
func apiFail(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"code": code, "message": trMsg(c, msg)})
}
