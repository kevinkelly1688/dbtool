// @author: kevinkelly
// @license: Apache Licence
// @file: ext.go
// @time: 2026/10/28

package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"dbtool/internal/audit"
	"dbtool/internal/config"
	"dbtool/internal/crypto"
	"dbtool/internal/firewall"
	"dbtool/internal/query"
)

// 本文件把 server 包里模块（系统管理、实例管理、SQL 优化、性能工单等）需要的能力导出。
// 模块放在自己的包里（internal/<模块>），在 init() 里调用 AddRoutes / AddMenu 登记，
// 由 cmd/server 与 cmd/demo 空导入即可启用；模块自己判断仓库是否具备所需能力，不具备就不登记路由。

// DBProvider 由 store.Store 实现：模块通过它拿到元数据库句柄与字段加解密器。
type DBProvider interface {
	GormDB() *gorm.DB
	MirageCrypto() *crypto.Mirage
}

type PageData = pageData
type MenuItem = menuItem

// AddRoutes 登记一个模块的路由（在 init() 里调用）。
func AddRoutes(h func(s *Server, rt Routes)) { addRoutes(h) }

// AddMenu 登记顶栏菜单条目。Group 取 check/query/opt/perf/inst/sys。
func AddMenu(m MenuItem) { addMenu(m) }

// SuperOnly 是“仅超级用户可见”的菜单可见性函数。
func SuperOnly(s *Server, a audit.Actor) bool { return superOnly(s, a) }

// Firewall 返回 SQL 防火墙引擎；没有元数据库（测试 / 演示）时为 nil。
func (s *Server) Firewall() *firewall.Engine { return s.q.Firewall() }

func (s *Server) Config() *config.Config { return s.cfg }
func (s *Server) Repo() Repo             { return s.repo }

// DB 返回元数据库句柄；仓库不是 store.Store（测试/演示）时 ok=false。
func (s *Server) DB() (*gorm.DB, *crypto.Mirage, bool) {
	p, ok := s.repo.(DBProvider)
	if !ok {
		return nil, nil, false
	}
	return p.GormDB(), p.MirageCrypto(), true
}

func PrincipalOf(c *gin.Context) query.Principal { return principalOf(c) }
func ActorOf(c *gin.Context) audit.Actor         { return actorOf(c) }

// CSRFOf 返回当前会话的 CSRF 令牌（Bearer JWT 调用没有会话时为空）。
func CSRFOf(c *gin.Context) string {
	if sess := sessionOf(c); sess != nil {
		return sess.CSRF
	}
	return ""
}

// APIFail 以统一格式中止并返回错误：{"code":…, "message":…}。
func APIFail(c *gin.Context, status int, code, msg string) { apiFail(c, status, code, msg) }

// APIOK 返回 {"data": data}。
func APIOK(c *gin.Context, data any) { c.JSON(http.StatusOK, gin.H{"data": data}) }

// Render 渲染页面；Username/Display/CSRF 为空时按当前会话补齐。
func (s *Server) Render(c *gin.Context, status int, page string, d PageData) {
	p := principalOf(c)
	if d.Username == "" {
		d.Username, d.Display = p.Username, p.Display
	}
	if d.CSRF == "" {
		d.CSRF = CSRFOf(c)
	}
	s.render(c, status, page, d)
}

// Forbidden 渲染 403 页面。
func (s *Server) Forbidden(c *gin.Context, msg string) { s.renderForbidden(c, msg) }

// RequireSuperuserAPI / RequireSuperuserPage：仅超级用户。
func (s *Server) SuperuserAPI() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !principalOf(c).IsSuperuser {
			apiFail(c, http.StatusForbidden, "forbidden", "仅超级用户可以操作")
			return
		}
		c.Next()
	}
}

func (s *Server) SuperuserPage() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !principalOf(c).IsSuperuser {
			s.renderForbidden(c, "仅超级用户可以访问该页面。")
			c.Abort()
			return
		}
		c.Next()
	}
}

// ---- 登录扩展（internal/authx）----
//
// 登录流程里与“验证方式”有关的部分（账号锁定、LDAP、两步验证、会话 / JWT 有效期、API 白名单）由 internal/authx 实现，
// 它在路由登记时调用 SetLoginExt 装配；没有装配时（测试、演示）server 走内置的本地账号密码流程，行为与原来完全一致。

// SessionPolicy 是会话策略。
type SessionPolicy struct {
	TTL        time.Duration
	Persistent bool // 为 true 时 Cookie 带 Max-Age（关闭浏览器后仍有效）；false 为会话 Cookie
	Sliding    bool // 滑动续期
}

// PasswordLoginReq 是页面账号密码登录的请求。
type PasswordLoginReq struct {
	Username, Password string
	Next               string // 已经过 SafeNext 的回跳地址
	IP                 string
	Local              bool // /login?local=1 超级用户后门
}

// LoginOutcome 是一次登录尝试的结果，四选一：Err（内部错误）、Session（建立会话）、Redirect（已设置中间态 Cookie，跳转继续）、其余为失败。
type LoginOutcome struct {
	Session  *Session
	Redirect string
	Err      error
	Status   int    // 失败时的 HTTP 状态，默认 401
	Code     string // 失败时的错误码（JSON 接口）
	Message  string // 失败时展示给用户的信息
	Count    bool   // 失败是否计入登录限流
}

// APILoginReq 是 JWT 登录请求。
type APILoginReq struct {
	Username, Password, OTP, IP string
}

// LoginExt 是登录扩展点。
type LoginExt interface {
	// LoginView 返回登录页模板用的数据（模板里的 .Data）。
	LoginView(r *http.Request) any
	// PasswordLogin 处理页面账号密码登录。
	PasswordLogin(w http.ResponseWriter, r *http.Request, req PasswordLoginReq) LoginOutcome
	// APILogin 处理 JWT 登录。
	APILogin(r *http.Request, req APILoginReq) LoginOutcome
	SessionPolicy() SessionPolicy
	TokenTTL() time.Duration
	// APIAllowed 判断 Bearer JWT 调用方是否在 API 白名单内。
	APIAllowed(uid uint) bool
}

// SetLoginExt 装配登录扩展（路由登记时调用）。
func (s *Server) SetLoginExt(e LoginExt) { s.login = e }

// PasswordGate 是登录扩展的可选能力：被要求先修改密码的账号（管理员用“系统生成临时密码”重置后），
// 登录后除了改密页与退出外，页面一律跳到 /account/password?force=1，接口一律返回 403 password_change_required。
type PasswordGate interface {
	MustChangePassword(uid uint) bool
}

// LogoutRedirector 是登录扩展的可选能力：退出登录后需要跳转到外部地址时（如 CAS 全局登出）返回该地址，否则返回空。
// sess 是退出前的会话（其中 Prov 记着用哪个登录方式登录的）。
type LogoutRedirector interface {
	LogoutRedirect(r *http.Request, sess *Session) string
}

// LogoutObserver 是登录扩展的可选能力：用户主动退出登录（会话有效、CSRF 校验通过）时被调用，用来写“退出登录”审计。
// ip 是客户端 IP。会话过期 / 被撤销不会走到这里（那时没有人在操作）。
type LogoutObserver interface {
	OnLogout(r *http.Request, sess *Session, ip string)
}

// passwordGateAllowed 是强制改密期间仍可访问的路径。
func passwordGateAllowed(path string) bool {
	switch path {
	case "/account/password", "/logout", "/api/v1/account/password", "/api/v1/auth/ui":
		return true
	}
	return false
}

// passwordGate 在已确认会话有效之后调用：该用户被要求先改密且访问的不是放行路径时，写出响应并返回 true（调用方应直接返回）。
func (s *Server) passwordGate(c *gin.Context, sess *Session, page bool) bool {
	g, ok := s.login.(PasswordGate)
	if !ok || !g.MustChangePassword(sess.UID) || passwordGateAllowed(c.Request.URL.Path) {
		return false
	}
	switch {
	case !page:
		apiFail(c, http.StatusForbidden, "password_change_required", "管理员要求你先修改密码，请到「修改密码」页完成后再继续")
	case strings.HasPrefix(c.Request.URL.Path, "/ui/"):
		c.AbortWithStatus(http.StatusForbidden)
	default:
		c.Redirect(http.StatusFound, "/account/password?force=1")
		c.Abort()
	}
	return true
}

// StartSession 为登录成功的用户建立新会话：签发新的会话 Cookie 与 CSRF 令牌（不沿用任何旧会话），并清掉登录页的 CSRF Cookie。
func (s *Server) StartSession(w http.ResponseWriter, r *http.Request, sess Session) error {
	sess.CSRF, sess.Exp = "", 0
	http.SetCookie(w, &http.Cookie{Name: loginCSRFCookie, Value: "", Path: "/login", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return s.setSession(w, r, sess)
}

// CurrentSession 解析请求里的会话 Cookie（公开路由里判断“是否已登录”用），无效返回 nil。
func (s *Server) CurrentSession(r *http.Request) *Session { return s.sessionFrom(r) }

// SafeNext 只允许站内相对路径作为登录后的回跳地址。
func SafeNext(next string) string { return safeNext(next) }

// RenderLogin 渲染登录页（同时刷新登录页的 CSRF Cookie）。errMsg 非空时显示错误。
func (s *Server) RenderLogin(c *gin.Context, status int, errMsg string) {
	s.renderLogin(c, status, errMsg)
}

// ---- 后台任务 ----

var (
	workerMu sync.Mutex
	workers  []func(ctx context.Context)
)

// AddWorker 登记后台任务（定时巡检、采集等）。通常在模块的路由登记函数里调用（此时已拿到数据库句柄）。
// 任务应在 ctx 取消时返回；cmd/server 在 server.New 之后调用 RunWorkers 启动，退出时等待它们结束。
func AddWorker(w func(ctx context.Context)) {
	workerMu.Lock()
	workers = append(workers, w)
	workerMu.Unlock()
}

// RunWorkers 启动所有已登记的后台任务，返回等待它们结束的函数。
func RunWorkers(ctx context.Context) (wait func()) {
	workerMu.Lock()
	list := append([]func(context.Context){}, workers...)
	workerMu.Unlock()
	var wg sync.WaitGroup
	for _, w := range list {
		wg.Add(1)
		go func() { defer wg.Done(); w(ctx) }()
	}
	return wg.Wait
}
