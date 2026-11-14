// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package authx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"dbtool/internal/audit"
	"dbtool/internal/crypto"
	"dbtool/internal/i18n"
	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/settings"
)

// 本文件是 authx 与 server 的接线：登录扩展（server.LoginExt）、SSO 与两步验证的公开路由、管理页与接口。
// 认证逻辑都在 Service 里，这里只做 HTTP 的事：Cookie、重定向、表单与 JSON。

const (
	pendingCookie    = "dba_2fa"
	loginCSRFCookie  = "dba_login_csrf" // 与 server 登录页的 CSRF Cookie 同名（server/session.go），注册表单复用它
	ssoCookiePath    = "/login/sso"
	pendingCookiePth = "/login"
)

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, m, ok := s.DB()
		if !ok {
			return // 仓库不是 store.Store（测试 / 演示）：不装配，登录走内置的本地流程
		}
		repo := newStore(db, m)
		if !repo.ready() {
			slog.Warn("登录方式 / 安全设置功能未启用：请先在元数据库执行 deploy/sql/018_auth.sql，然后重启服务")
			return
		}
		cfg := s.Config()
		svc, err := NewService(repo, Options{Key: []byte(cfg.SecretKey), PublicURL: cfg.PublicURL})
		if err != nil {
			slog.Error("初始化登录扩展失败", "err", err)
			return
		}
		register(s, rt, svc)
	})
	server.AddMenu(server.MenuItem{Group: "access", Key: "sys-auth-providers", Href: "/settings/auth-providers", Label: "登录认证", Order: 40, Show: showFor})
	server.AddMenu(server.MenuItem{Group: "access", Key: "sys-security", Href: "/settings/security", Label: "安全策略", Order: 50, Show: showFor})
}

var (
	regMu    sync.Mutex
	registry = map[*server.Server]*Service{}
)

// showFor 只有装配了 authx 的服务、且是超级用户才显示菜单。
func showFor(s *server.Server, a audit.Actor) bool {
	regMu.Lock()
	defer regMu.Unlock()
	_, ok := registry[s]
	return ok && a.IsSuperuser
}

// For 返回某个 server 实例上装配的 authx 服务（没有装配返回 nil）。
func For(s *server.Server) *Service {
	regMu.Lock()
	defer regMu.Unlock()
	return registry[s]
}

// CheckPassword 按当前密码策略校验新密码，供 sysadmin 模块在创建用户 / 修改密码时调用：
//
//	if err := authx.CheckPassword(s, username, newPassword, display); err != nil { 返回 400，err.Error() 可直接展示 }
//
// 没有装配 authx 时使用默认策略（最小长度 9，非常见密码、非纯数字、与用户名不太像）。
func CheckPassword(s *server.Server, username, password string, userAttrs ...string) error {
	pol := DefaultPasswordPolicy()
	if svc := For(s); svc != nil {
		pol = svc.Settings().Policy()
	}
	return ValidatePassword(pol, password, append([]string{username}, userAttrs...)...)
}

type handlers struct {
	s   *server.Server
	svc *Service
	su  *signupLimiter
	wm  *tamperLimiter
}

// register 装配登录扩展与路由。测试里传入假仓库构造的 Service。
func register(s *server.Server, rt server.Routes, svc *Service) {
	h := &handlers{s: s, svc: svc, su: &signupLimiter{hits: map[string][]time.Time{}}, wm: &tamperLimiter{hits: map[uint][]time.Time{}}}
	regMu.Lock()
	registry[s] = svc
	regMu.Unlock()
	s.SetLoginExt(&loginExt{h: h})

	// 登录流程里的公开路由
	pub := rt.Public
	pub.GET("/login/sso/:id", h.ssoStart)
	pub.GET("/login/sso/:id/callback", h.ssoCallback)
	pub.GET("/login/2fa", h.twofaPage)
	pub.POST("/login/2fa", h.twofaSubmit)
	pub.POST("/login/signup", h.signup)

	// 管理页（仅超级用户）
	rt.Pages.GET("/settings/auth-providers", s.SuperuserPage(), h.providersPage)
	rt.Pages.GET("/settings/security", s.SuperuserPage(), h.securityPage)

	// 个人账号：自助修改密码、外部账号绑定
	rt.Pages.GET("/account/password", h.passwordPage)
	rt.Pages.POST("/account/password", h.passwordForm)
	rt.Pages.GET("/account/bindings", h.bindingsPage)

	su := s.SuperuserAPI()
	a := rt.API
	a.GET("/settings/auth-providers", su, h.apiOverview)
	a.POST("/settings/auth-providers", su, h.apiCreate)
	a.GET("/settings/auth-providers/:id", su, h.apiGet)
	a.PUT("/settings/auth-providers/:id", su, h.apiUpdate)
	a.DELETE("/settings/auth-providers/:id", su, h.apiDelete)
	a.POST("/settings/auth-providers/:id/enable", su, h.apiEnable)
	a.POST("/settings/auth-providers/:id/disable", su, h.apiDisable)
	a.POST("/settings/auth-providers/:id/test", su, h.apiTest)
	a.PUT("/settings/auth-order", su, h.apiOrder)
	a.PUT("/settings/auth-rules", su, h.apiRules)
	a.GET("/settings/security", su, h.apiSecurity)
	a.PUT("/settings/security", su, h.apiSaveSecurity)
	a.POST("/settings/security/sms-test", su, h.apiSMSTest)

	// 登录用户自助管理两步验证
	a.GET("/auth/2fa", h.apiTwoFAState)
	a.POST("/auth/2fa/totp", h.apiTOTPBegin)
	a.POST("/auth/2fa/totp/confirm", h.apiTOTPConfirm)
	a.POST("/auth/2fa/sms/send", h.apiSMSSend)
	a.POST("/auth/2fa/sms/confirm", h.apiSMSConfirm)
	a.DELETE("/auth/2fa/:type", h.apiTwoFADisable)
	a.GET("/auth/ui", h.apiUI)
	a.POST("/auth/watermark-tamper", h.apiWatermarkTamper)
	a.GET("/account/info", h.apiAccountInfo)
	a.POST("/account/password", h.apiChangePassword)
	a.GET("/account/bindings", h.apiBindings)
	a.DELETE("/account/bindings/:id", h.apiUnbind)
}

// ---- 通用 ----

func (h *handlers) baseURL(c *gin.Context) string { return h.baseURLOf(c.Request) }

func (h *handlers) baseURLOf(r *http.Request) string {
	if u := h.s.Config().PublicURL; u != "" {
		return strings.TrimRight(u, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func noStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
}

func sessionFor(u *model.User) *server.Session {
	return &server.Session{UID: u.ID, Username: u.Username, Display: u.Display, IsSuperuser: u.IsSuperuser}
}

func idParam(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数非法")
		return 0, false
	}
	return uint(n), true
}

func actorOf(c *gin.Context) audit.Actor { return server.ActorOf(c) }

// fail 把服务错误转成 JSON：校验类错误 400，其余 500（不暴露细节）。
func fail(c *gin.Context, err error) {
	var ue *UserError
	var iu *InUseError
	switch {
	case errors.As(err, &iu):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"code": "in_use", "message": localizeMsg(reqLang(c), iu.Error()), "data": gin.H{"users": iu.Users}})
	case errors.As(err, &ue):
		server.APIFail(c, http.StatusBadRequest, "bad_request", localizeMsg(reqLang(c), ue.Msg))
	default:
		slog.Error("登录方式 / 安全设置请求失败", "path", c.Request.URL.Path, "err", err)
		server.APIFail(c, http.StatusInternalServerError, "internal", "服务异常，请稍后再试")
	}
}

// ---- server.LoginExt ----

type loginExt struct{ h *handlers }

var _ server.LoginExt = (*loginExt)(nil)

// LoginView 是登录页模板里的 .Data。
type LoginView struct {
	Buttons      []LoginButtonView
	ShowForm     bool
	FormExpanded bool
	FormHint     string
	SignUp       bool
	Local        bool
	Notice       string
	Alert        string
}

// LoginButtonView 是登录页上的一个外部登录按钮。
type LoginButtonView struct {
	Label   string
	Href    string
	Primary bool
}

// 登录页上 ?err= 的固定文案：只显示这里列出的内容，绝不回显任何来自请求或 IdP 的文本。
var loginErrors = map[string]string{
	"state": ErrStateInvalid.Msg, "failed": ErrSSOFailed.Msg, "not_provisioned": ErrNotProvisioned.Msg, "superuser_local": ErrSuperuserLocal.Msg,
	"disabled": ErrDisabledUser.Msg, "provider_off": ErrProviderOff.Msg, "username_invalid": "无法根据外部账号生成合法的平台用户名，请联系管理员",
	"2fa_locked": ErrTwoFALocked.Msg, "2fa_expired": "两步验证已超时，请重新登录", "internal": "服务异常，请稍后再试",
}

func (x *loginExt) LoginView(r *http.Request) any {
	lang := i18n.FromContext(r.Context())
	local := r.URL.Query().Get("local") == "1" || r.FormValue("local") == "1"
	pg := x.h.svc.LoginPageInfo(local)
	next := server.SafeNext(r.URL.Query().Get("next"))
	v := LoginView{ShowForm: pg.ShowForm, FormExpanded: pg.FormExpanded, FormHint: i18n.TP(lang, pg.FormHint), SignUp: pg.SignUp, Local: pg.Local}
	for i, b := range pg.Buttons {
		href := "/login/sso/" + strconv.FormatUint(uint64(b.ID), 10)
		if next != server.SafeNext("") || r.URL.Query().Get("next") != "" {
			href += "?next=" + url.QueryEscape(next)
		}
		label := b.Label
		if b.Auto { // 默认按钮文案按界面语言重新生成；管理员自定义的文字原样显示
			label = i18n.T(lang, "使用{name}登录", "name", b.Name)
		}
		v.Buttons = append(v.Buttons, LoginButtonView{Label: label, Href: href, Primary: i == 0})
	}
	q := r.URL.Query()
	if q.Get("registered") == "1" {
		v.Notice = i18n.TP(lang, "注册成功，请输入密码登录。")
	}
	if code := q.Get("err"); code != "" {
		v.Alert = i18n.TP(lang, loginErrors[code]) // 未知代码为空，不显示
	}
	return v
}

func (x *loginExt) SessionPolicy() server.SessionPolicy {
	set := x.h.svc.Settings()
	return server.SessionPolicy{TTL: set.SessionTTL(), Persistent: !set.SessionBrowserClose, Sliding: true}
}

func (x *loginExt) TokenTTL() time.Duration { return x.h.svc.Settings().TokenTTL() }

func (x *loginExt) APIAllowed(uid uint) bool { return x.h.svc.Settings().APIAllowed(uid) }

// MustChangePassword 实现 server.PasswordGate：被管理员要求先改密的账号，登录后只能到改密页。
func (x *loginExt) MustChangePassword(uid uint) bool { return x.h.svc.MustChangePassword(uid) }

// LogoutRedirect 实现 server.LogoutRedirector：用 CAS 登录且开启了“退出时同时全局登出”时，退出后跳到 CAS 的登出地址。
func (x *loginExt) LogoutRedirect(r *http.Request, sess *server.Session) string {
	if sess == nil || sess.Prov == 0 {
		return ""
	}
	return x.h.svc.CASLogoutURL(sess.Prov, x.h.baseURLOf(r))
}

// OnLogout 实现 server.LogoutObserver：用户主动退出登录时写审计“退出登录”（含 IP）。
func (x *loginExt) OnLogout(r *http.Request, sess *server.Session, ip string) {
	if sess == nil {
		return
	}
	x.h.svc.audit(sess.UID, sess.Username, sess.Display, "退出登录", "IP："+ip) // i18n-ignore 审计文本（存库）
}

func failOutcome(err error, count bool) server.LoginOutcome {
	var fe *FlowError
	if errors.As(err, &fe) {
		return server.LoginOutcome{Message: fe.Msg, Count: count, Code: "bad_credentials"}
	}
	return server.LoginOutcome{Err: err}
}

func (x *loginExt) PasswordLogin(w http.ResponseWriter, r *http.Request, req server.PasswordLoginReq) server.LoginOutcome {
	a, err := x.h.svc.PasswordLogin(r.Context(), req.Username, req.Password, PasswordOpts{Local: req.Local, IP: req.IP})
	if err != nil {
		return failOutcome(err, true)
	}
	return x.h.afterAuth(w, r, a, req.Next, req.IP)
}

// afterAuth 密码 / 外部身份认证通过之后：需要两步验证则写中间态 Cookie 并跳转，否则直接建立会话。
func (h *handlers) afterAuth(w http.ResponseWriter, r *http.Request, a *AuthOK, next, ip string) server.LoginOutcome {
	mode, err := h.svc.TwoFAModeFor(a)
	if err != nil {
		return server.LoginOutcome{Err: err}
	}
	if mode != ModeNone {
		v, err := h.svc.NewPending(a, mode, next)
		if err != nil {
			return server.LoginOutcome{Err: err}
		}
		http.SetCookie(w, &http.Cookie{Name: pendingCookie, Value: v, Path: pendingCookiePth, MaxAge: PendingTTL(), HttpOnly: true,
			SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r)})
		return server.LoginOutcome{Redirect: "/login/2fa"}
	}
	if err := h.svc.CompleteLogin(a.User.ID, a.ProviderID, ip); err != nil {
		return server.LoginOutcome{Err: err}
	}
	sess := sessionFor(a.User)
	sess.Prov = a.ProviderID // 记下登录方式：退出时 CAS 可以同时全局登出
	return server.LoginOutcome{Session: sess}
}

// APILogin 是 JWT 登录。JWT 登录不走两步验证（只校验账号密码）；
// 新增可选开关 security.api_jwt_require_otp：开启后，已绑定两步验证的用户必须在请求里附带 TOTP（短信方式无法在非交互接口里完成）。
func (x *loginExt) APILogin(r *http.Request, req server.APILoginReq) server.LoginOutcome {
	svc := x.h.svc
	a, err := svc.PasswordLogin(r.Context(), req.Username, req.Password, PasswordOpts{IP: req.IP, API: true})
	if err != nil {
		return failOutcome(err, true)
	}
	set := svc.Settings()
	if !set.APIAllowed(a.User.ID) {
		return server.LoginOutcome{Status: http.StatusForbidden, Code: "forbidden", Message: "该用户不在 API 白名单内"}
	}
	if set.APIJWTRequireOTP {
		_, bound, err := svc.HasTOTP(a.User.ID)
		if err != nil {
			return server.LoginOutcome{Err: err}
		}
		if bound {
			if strings.TrimSpace(req.OTP) == "" {
				return server.LoginOutcome{Code: "otp_required", Message: "该账号已开启两步验证，请在请求里附带 otp（动态口令）"}
			}
			if err := svc.VerifyAPIOTP(a.User.ID, a.User.Username, req.OTP); err != nil {
				if errors.Is(err, ErrTwoFABad) || errors.Is(err, ErrTwoFALocked) {
					return server.LoginOutcome{Code: "bad_otp", Message: "动态口令不正确或尝试次数过多", Count: true}
				}
				return server.LoginOutcome{Err: err}
			}
		}
	}
	if err := svc.CompleteAPILogin(a.User.ID, a.ProviderID, req.IP); err != nil {
		return server.LoginOutcome{Err: err}
	}
	return server.LoginOutcome{Session: sessionFor(a.User)}
}

// ---- SSO 路由 ----

func setSSOCookie(c *gin.Context, name, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{Name: name, Value: value, Path: ssoCookiePath, MaxAge: maxAge, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: isHTTPS(c.Request)}) // Lax：IdP 跨站回跳时 Cookie 才会带上
}

func loginRedirect(c *gin.Context, code string) {
	c.Redirect(http.StatusFound, "/login?err="+url.QueryEscape(code))
}

func errCode(err error) string {
	var fe *FlowError
	if errors.As(err, &fe) {
		if _, ok := loginErrors[fe.Code]; ok {
			return fe.Code
		}
		return "failed"
	}
	return "internal"
}

func (h *handlers) ssoStart(c *gin.Context) {
	noStore(c)
	id, ok := idParamPlain(c)
	if !ok {
		c.Redirect(http.StatusFound, "/login")
		return
	}
	next := server.SafeNext(c.Query("next")) // 只允许站内路径
	in := BeginSSOInput{Next: next, UserAgent: c.Request.UserAgent(), BaseURL: h.baseURL(c)}
	sess := h.s.CurrentSession(c.Request)
	if c.Query("test") == "1" { // 管理员的“测试登录”：必须是超级用户会话
		if sess == nil || !sess.IsSuperuser {
			c.String(http.StatusForbidden, trc(c, "仅超级用户可以测试登录"))
			return
		}
		in.Test, in.Tester = true, sess.UID
	} else if c.Query("bind") == "1" { // 已登录用户绑定外部账号（管理员需开启“允许用户自行绑定”）
		if sess == nil {
			c.Redirect(http.StatusFound, "/login?next="+url.QueryEscape("/account/bindings"))
			return
		}
		in.Bind, in.BindUID, in.Next = true, sess.UID, "/account/bindings"
		st, err := h.svc.BeginSSO(c.Request.Context(), id, in)
		if err != nil {
			c.Redirect(http.StatusFound, "/account/bindings?err="+bindErrCode(err))
			return
		}
		setSSOCookie(c, st.CookieName, st.CookieValue, st.MaxAge)
		c.Redirect(http.StatusFound, st.URL)
		return
	} else if sess != nil {
		c.Redirect(http.StatusFound, next)
		return
	}
	st, err := h.svc.BeginSSO(c.Request.Context(), id, in)
	if err != nil {
		if in.Test {
			c.String(http.StatusBadGateway, trc(c, "无法发起测试登录：{reason}", "reason", localizeMsg(reqLang(c), errText(err))))
			return
		}
		loginRedirect(c, errCode(err))
		return
	}
	setSSOCookie(c, st.CookieName, st.CookieValue, st.MaxAge)
	c.Redirect(http.StatusFound, st.URL)
}

func idParamPlain(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	return uint(n), err == nil && n != 0
}

func (h *handlers) ssoCallback(c *gin.Context) {
	noStore(c)
	id, ok := idParamPlain(c)
	if !ok {
		c.Redirect(http.StatusFound, "/login")
		return
	}
	name := SSOCookieName(id)
	cookie, _ := c.Cookie(name)
	setSSOCookie(c, name, "", -1) // 状态 Cookie 一次性：无论成败都清掉
	var tester, sessUID uint
	sess := h.s.CurrentSession(c.Request)
	if sess != nil {
		sessUID = sess.UID
		if sess.IsSuperuser {
			tester = sess.UID
		}
	}
	res, err := h.svc.CompleteSSOFor(c.Request.Context(), id, c.Request.URL.Query(), cookie, tester, sessUID, h.baseURL(c))
	if err != nil {
		var fe *FlowError
		if !errors.As(err, &fe) {
			slog.Error("SSO 回调出错", "provider", id, "err", err)
		}
		var be *BindError
		if errors.As(err, &be) && sess != nil { // “绑定外部账号”失败：回到账号绑定页显示原因
			c.Redirect(http.StatusFound, "/account/bindings?err="+bindErrCode(err))
			return
		}
		loginRedirect(c, errCode(err))
		return
	}
	if res.Bound {
		c.Redirect(http.StatusFound, "/account/bindings?bound=1")
		return
	}
	if res.Test != nil {
		h.s.Render(c, http.StatusOK, "auth/sso_test.html", server.PageData{Title: "测试登录结果", Username: sess.Username, Display: sess.Display, CSRF: sess.CSRF, Data: localizedReport(reqLang(c), res.Test)})
		return
	}
	out := h.afterAuth(c.Writer, c.Request, res.Auth, res.Next, c.ClientIP())
	switch {
	case out.Err != nil:
		slog.Error("SSO 登录收尾出错", "provider", id, "err", out.Err)
		loginRedirect(c, "internal")
	case out.Session != nil:
		if err := h.s.StartSession(c.Writer, c.Request, *out.Session); err != nil {
			loginRedirect(c, "internal")
			return
		}
		c.Redirect(http.StatusFound, server.SafeNext(res.Next))
	default:
		c.Redirect(http.StatusFound, out.Redirect)
	}
}

// ---- 两步验证页 ----

type twofaPageData struct {
	CSRF     string
	Mode     string
	Methods  []TwoFAMethod
	Method   string
	Setup    *TOTPSetup
	Phone    string // 绑定短信时用户填写的号码
	Masked   string // 已绑定的脱敏号码
	Error    string
	Info     string
	SMSReady bool
}

func (h *handlers) pendingFrom(c *gin.Context) *Pending {
	v, err := c.Cookie(pendingCookie)
	if err != nil {
		return nil
	}
	return h.svc.OpenPending(v)
}

func clearPending(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{Name: pendingCookie, Value: "", Path: pendingCookiePth, MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isHTTPS(c.Request)})
}

func (h *handlers) twofaPageData(pd *Pending, method, phone string) (*twofaPageData, error) {
	methods, err := h.svc.MethodsFor(pd)
	if err != nil {
		return nil, err
	}
	d := &twofaPageData{CSRF: pd.CSRF, Mode: string(pd.Mode), Methods: methods, Phone: phone}
	for _, m := range methods {
		if m.Code == method {
			d.Method = method
		}
	}
	if d.Method == "" && len(methods) > 0 {
		d.Method = methods[0].Code
	}
	for _, m := range methods {
		if m.Code == Method2FASMS {
			d.Masked, d.SMSReady = m.Phone, true
		}
	}
	if pd.Mode == ModeBind && d.Method == Method2FATOTP {
		if d.Setup, err = h.svc.BeginTOTP(pd); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (h *handlers) render2FA(c *gin.Context, status int, pd *Pending, method, phone, errMsg, info string) {
	d, err := h.twofaPageData(pd, method, phone)
	if err != nil {
		slog.Error("渲染两步验证页失败", "err", err)
		clearPending(c)
		loginRedirect(c, "internal")
		return
	}
	d.Error, d.Info = errMsg, info
	noStore(c)
	h.s.Render(c, status, "auth/login_2fa.html", server.PageData{Title: "两步验证", CSRF: pd.CSRF, Data: d})
}

func (h *handlers) twofaPage(c *gin.Context) {
	pd := h.pendingFrom(c)
	if pd == nil {
		c.Redirect(http.StatusFound, "/login?err=2fa_expired")
		return
	}
	h.render2FA(c, http.StatusOK, pd, c.Query("m"), "", "", "")
}

func (h *handlers) twofaSubmit(c *gin.Context) {
	pd := h.pendingFrom(c)
	if pd == nil {
		c.Redirect(http.StatusFound, "/login?err=2fa_expired")
		return
	}
	if !constEq(c.PostForm("csrf"), pd.CSRF) {
		h.render2FA(c, http.StatusForbidden, pd, c.PostForm("method"), "", "页面已过期，请重新提交", "")
		return
	}
	method, phone, code := c.PostForm("method"), strings.TrimSpace(c.PostForm("phone")), strings.TrimSpace(c.PostForm("code"))
	switch c.PostForm("action") {
	case "send":
		if method != Method2FASMS {
			h.render2FA(c, http.StatusBadRequest, pd, method, phone, "该验证方式不需要获取验证码", "")
			return
		}
		if err := h.svc.SendSMS(c.Request.Context(), pd, phone); err != nil {
			var fe *FlowError
			if errors.As(err, &fe) {
				h.render2FA(c, http.StatusBadRequest, pd, method, phone, fe.Msg, "")
				return
			}
			slog.Error("发送短信验证码失败", "err", err)
			h.render2FA(c, http.StatusInternalServerError, pd, method, phone, "服务异常，请稍后再试", "")
			return
		}
		h.render2FA(c, http.StatusOK, pd, method, phone, "", "验证码已发送，5 分钟内有效")
		return
	}
	if err := h.svc.VerifyTwoFA(pd, method, code, phone); err != nil {
		var fe *FlowError
		if errors.Is(err, ErrTwoFALocked) {
			clearPending(c) // 失败次数过多：作废这次登录，需要重新输入密码
			loginRedirect(c, "2fa_locked")
			return
		}
		if errors.As(err, &fe) {
			status := http.StatusUnauthorized
			if fe.Code != ErrTwoFABad.Code && fe.Code != ErrTwoFAExpired.Code {
				status = http.StatusBadRequest
			}
			h.render2FA(c, status, pd, method, phone, fe.Msg, "")
			return
		}
		slog.Error("两步验证校验出错", "err", err)
		h.render2FA(c, http.StatusInternalServerError, pd, method, phone, "服务异常，请稍后再试", "")
		return
	}
	u, err := h.svc.repo.UserByID(pd.UID)
	if err != nil || u == nil || !u.IsActive {
		clearPending(c)
		loginRedirect(c, "failed")
		return
	}
	if err := h.svc.CompleteLogin(u.ID, pd.ProviderID, c.ClientIP()); err != nil {
		slog.Error("登录收尾失败", "err", err)
		clearPending(c)
		loginRedirect(c, "internal")
		return
	}
	clearPending(c)
	sess := sessionFor(u)
	sess.Prov = pd.ProviderID
	if err := h.s.StartSession(c.Writer, c.Request, *sess); err != nil {
		loginRedirect(c, "internal")
		return
	}
	c.Redirect(http.StatusSeeOther, server.SafeNext(pd.Next))
}

// ---- 自助注册 ----

type signupLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

// allow 同一 IP 每小时最多 10 次注册尝试。
func (l *signupLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	if len(kept) >= 10 {
		l.hits[ip] = kept
		return false
	}
	l.hits[ip] = append(kept, now)
	if len(l.hits) > 4096 {
		for k, v := range l.hits {
			if len(v) == 0 || now.Sub(v[len(v)-1]) > time.Hour {
				delete(l.hits, k)
			}
		}
	}
	return true
}

func (h *handlers) signup(c *gin.Context) {
	ck, err := c.Request.Cookie(loginCSRFCookie)
	if err != nil || !constEq(c.PostForm("csrf"), ck.Value) {
		h.s.RenderLogin(c, http.StatusForbidden, "页面已过期，请重新提交")
		return
	}
	if !h.su.allow(c.ClientIP(), time.Now()) {
		h.s.RenderLogin(c, http.StatusTooManyRequests, "尝试次数过多，请稍后再试")
		return
	}
	name, err := h.svc.SignUp(SignUpInput{Username: c.PostForm("username"), Password: c.PostForm("password"), Password2: c.PostForm("password2"),
		Display: c.PostForm("display"), Email: c.PostForm("email"), IP: c.ClientIP()})
	if err != nil {
		var fe *FlowError
		if errors.As(err, &fe) {
			status := http.StatusBadRequest
			if fe.Code == "signup_off" {
				status = http.StatusForbidden
			}
			h.s.RenderLogin(c, status, fe.Msg)
			return
		}
		slog.Error("注册失败", "err", err)
		h.s.RenderLogin(c, http.StatusInternalServerError, "服务异常，请稍后再试")
		return
	}
	_ = name
	c.Redirect(http.StatusSeeOther, "/login?registered=1")
}

// SignUpInput 是注册表单。
type SignUpInput struct {
	Username, Password, Password2, Display, Email, IP string
}

// SignUp 自助注册：需要 security.self_signup；校验（必填、不重名、两次密码一致、
// 必须有中文名、密码策略），创建后用户为启用状态，首次登录时按默认组入组。
func (s *Service) SignUp(in SignUpInput) (string, error) {
	if !s.Settings().SignUp {
		return "", &FlowError{Code: "signup_off", Msg: "注册未启用，请联系管理员开启"}
	}
	name := strings.TrimSpace(in.Username)
	switch {
	case name == "" || in.Password == "":
		return "", &FlowError{Code: "signup", Msg: "用户名和密码不能为空"}
	case in.Password != in.Password2:
		return "", &FlowError{Code: "signup", Msg: "两次输入密码不一致"}
	case strings.TrimSpace(in.Display) == "":
		return "", &FlowError{Code: "signup", Msg: "请填写中文名"}
	case len([]rune(strings.TrimSpace(in.Display))) > 10:
		return "", &FlowError{Code: "signup", Msg: "中文名不超过 10 个字符"}
	case len(in.Email) > 254:
		return "", &FlowError{Code: "signup", Msg: "邮箱过长"}
	}
	if err := validNewUsername(name); err != nil {
		return "", &FlowError{Code: "signup", Msg: err.Error()}
	}
	if u, err := s.repo.UserByName(name); err != nil {
		return "", err
	} else if u != nil {
		return "", &FlowError{Code: "signup", Msg: "用户名已存在"}
	}
	if err := ValidatePassword(s.Settings().Policy(), in.Password, name, in.Display, strings.SplitN(in.Email, "@", 2)[0]); err != nil {
		return "", &FlowError{Code: "signup", Msg: err.Error()}
	}
	hash, err := crypto.MakeDjangoPassword(in.Password, randToken(12), 600000)
	if err != nil {
		return "", err
	}
	u := &model.User{Username: name, Password: hash, IsActive: true, IsStaff: true, Display: strings.TrimSpace(in.Display), Email: strings.TrimSpace(in.Email), DateJoined: s.now()}
	if err := s.repo.CreateUser(u); err != nil {
		return "", err
	}
	s.audit(u.ID, u.Username, u.Display, "注册用户", in.IP) // i18n-ignore 审计文本（存库）
	return name, nil
}

// ---- 页面 ----

func (h *handlers) providersPage(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "auth/providers.html", server.PageData{Title: "登录认证", Nav: "sys-auth-providers", CSRF: server.CSRFOf(c)})
}

func (h *handlers) securityPage(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "auth/security.html", server.PageData{Title: "安全策略", Nav: "sys-security", CSRF: server.CSRFOf(c)})
}

// ---- 管理接口：登录方式 ----

type overview struct {
	Providers    []ProviderView `json:"providers"`
	Kinds        []kindOption   `json:"kinds"`
	Rules        RulesInput     `json:"rules"`
	LocalLast    string         `json:"local_last_login"`
	AuthGroups   []IDName       `json:"auth_groups"`
	ResGroups    []IDName       `json:"resource_groups"`
	CallbackBase string         `json:"callback_base"`
}

type kindOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

func (h *handlers) apiOverview(c *gin.Context) {
	base := h.baseURL(c)
	list, err := h.svc.ListProviders(base)
	if err != nil {
		fail(c, err)
		return
	}
	ag, err := h.svc.repo.ListAuthGroups()
	if err != nil {
		fail(c, err)
		return
	}
	rg, err := h.svc.repo.ListResourceGroups()
	if err != nil {
		fail(c, err)
		return
	}
	for i := range list {
		localizeView(c, &list[i])
	}
	kinds := []kindOption{{"ldap", "LDAP / AD 域"}, {"oidc", "OIDC（标准 OpenID Connect）"}, {"dingtalk", "钉钉"}, {"cas", "CAS"},
		{"feishu", "飞书"}, {"lark", "Lark（国际版飞书）"}, {"slack", "Slack"}, {"wecom", "企业微信"}}
	server.APIOK(c, overview{Providers: list, Kinds: kinds, Rules: h.svc.Settings().Rules(), LocalLast: h.svc.LocalLastLogin(),
		AuthGroups: nonNilIDs(ag), ResGroups: nonNilIDs(rg), CallbackBase: base})
}

func nonNilIDs(in []IDName) []IDName {
	if in == nil {
		return []IDName{}
	}
	return in
}

func (h *handlers) viewOf(c *gin.Context, id uint) (*ProviderView, error) {
	p, err := h.svc.GetProvider(id)
	if err != nil || p == nil {
		return nil, err
	}
	v := h.svc.View(p, h.baseURL(c))
	localizeView(c, &v)
	return &v, nil
}

func (h *handlers) apiCreate(c *gin.Context) {
	var in struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数错误")
		return
	}
	kind, site := Kind(in.Kind), ""
	if in.Kind == "lark" { // “Lark”是飞书的国际站点
		kind, site = KindFeishu, "lark"
	}
	p, err := h.svc.CreateProvider(actorOf(c), kind, in.Name)
	if err != nil {
		fail(c, err)
		return
	}
	if site != "" {
		cfg := p.Config
		cfg.Site = site
		if _, _, err := h.svc.UpdateProvider(actorOf(c), p.ID, ProviderInput{Name: p.Name, FirstLogin: p.FirstLogin, Config: cfg}); err != nil {
			// 必填项还没填，校验会失败；站点直接写进去
			p.Config.Site = site
			if err := h.svc.repo.SaveProvider(p); err != nil {
				fail(c, err)
				return
			}
			h.svc.invalidateProviders()
		}
	}
	v, err := h.viewOf(c, p.ID)
	if err != nil || v == nil {
		fail(c, err)
		return
	}
	server.APIOK(c, v)
}

func (h *handlers) apiGet(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	v, err := h.viewOf(c, id)
	if err != nil {
		fail(c, err)
		return
	}
	if v == nil {
		server.APIFail(c, http.StatusNotFound, "not_found", "登录方式不存在")
		return
	}
	server.APIOK(c, v)
}

func (h *handlers) apiUpdate(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in ProviderInput
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数错误")
		return
	}
	p, notice, err := h.svc.UpdateProvider(actorOf(c), id, in)
	if err != nil {
		fail(c, err)
		return
	}
	v := h.svc.View(p, h.baseURL(c))
	localizeView(c, &v)
	server.APIOK(c, gin.H{"provider": v, "notice": localizeMsg(reqLang(c), notice)})
}

func (h *handlers) apiDelete(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteProvider(actorOf(c), id, c.Query("confirm") == "1"); err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiEnable(c *gin.Context)  { h.setEnabled(c, true) }
func (h *handlers) apiDisable(c *gin.Context) { h.setEnabled(c, false) }

func (h *handlers) setEnabled(c *gin.Context, on bool) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.SetEnabled(actorOf(c), id, on); err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiTest(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in TestInput
	_ = c.ShouldBindJSON(&in) // 允许空请求体
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	rep, err := h.svc.TestProvider(ctx, actorOf(c), id, in)
	if err != nil {
		fail(c, err)
		return
	}
	v, _ := h.viewOf(c, id)
	server.APIOK(c, gin.H{"report": localizedReport(reqLang(c), rep), "provider": v})
}

func (h *handlers) apiOrder(c *gin.Context) {
	var in struct {
		IDs []uint `json:"ids"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || len(in.IDs) == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数错误")
		return
	}
	if err := h.svc.Reorder(actorOf(c), in.IDs); err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiRules(c *gin.Context) {
	var in RulesInput
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数错误")
		return
	}
	old := h.svc.Settings().Rules()
	set, err := SaveRules(h.svc.repo, in, actorOf(c).Username)
	if err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", localizeMsg(reqLang(c), err.Error()))
		return
	}
	h.svc.settings.Set(set)
	h.svc.auditActor(actorOf(c), "登录方式-通用规则", fmt.Sprintf("%+v → %+v", old, set.Rules())) // i18n-ignore 审计文本（存库）
	server.APIOK(c, set.Rules())
}

// ---- 管理接口：登录与安全设置 ----

func (h *handlers) apiSecurity(c *gin.Context) {
	set := h.svc.Settings()
	ag, err := h.svc.repo.ListAuthGroups()
	if err != nil {
		fail(c, err)
		return
	}
	rg, err := h.svc.repo.ListResourceGroups()
	if err != nil {
		fail(c, err)
		return
	}
	users, err := h.svc.repo.ListUsers(2000)
	if err != nil {
		fail(c, err)
		return
	}
	list, err := h.svc.ListProviders(h.baseURL(c))
	if err != nil {
		fail(c, err)
		return
	}
	type brief struct {
		ID    uint   `json:"id"`
		Name  string `json:"name"`
		Kind  string `json:"kind"`
		State string `json:"status"`
	}
	var provs []brief
	for _, p := range list {
		provs = append(provs, brief{p.ID, p.Name, p.KindLabel, p.Status})
	}
	out := gin.H{"settings": set.View(), "auth_groups": nonNilIDs(ag), "resource_groups": nonNilIDs(rg), "users": nonNilIDs(users), "providers": provs}
	if bound, total, err := h.svc.repo.CountTwoFA(); err == nil { // 概览 KPI：已绑定两步验证的账号数（取不到就不显示）
		out["twofa"] = gin.H{"bound": bound, "total": total}
	}
	out["me"] = h.svc.WatermarkFor(server.PrincipalOf(c).ID, server.PrincipalOf(c).Username, server.PrincipalOf(c).Display, c.ClientIP()).Vars
	server.APIOK(c, out)
}

func (h *handlers) apiSaveSecurity(c *gin.Context) {
	var in SecurityInput
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数错误")
		return
	}
	cur := h.svc.Settings()
	set, err := SaveSecurity(h.svc.repo, cur, in, actorOf(c).Username)
	if err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", localizeMsg(reqLang(c), err.Error()))
		return
	}
	h.svc.settings.Set(set)                                         // 立即生效：会话 / JWT 有效期、锁定、密码策略等下一次请求就按新值
	h.svc.auditActor(actorOf(c), "安全设置-修改", diffSettings(cur, set)) // i18n-ignore 审计文本（存库）
	server.APIOK(c, set.View())
}

// diffSettings 列出变化的设置项（密钥只写“已修改”，不写值）。
func diffSettings(a, b Settings) string {
	var out []string
	add := func(name string, x, y any) {
		if fmt.Sprint(x) != fmt.Sprint(y) {
			out = append(out, fmt.Sprintf("%s: %v → %v", name, x, y))
		}
	}
	add(settings.KeySecurityLoginLockAttempts, a.LockCount, b.LockCount)
	add(settings.KeySecurityLoginLockSeconds, a.LockSeconds, b.LockSeconds)
	add(settings.KeySecurityRequire2FA, a.Enforce2FA, b.Enforce2FA)
	add(settings.KeySecuritySelfSignup, a.SignUp, b.SignUp)
	add(settings.KeySecurityWatermarkEnabled, a.Watermark, b.Watermark)
	add(settings.KeySecurityWatermarkText, a.WM.Text, b.WM.Text)
	add(settings.KeySecurityWatermarkExtra, a.WM.Extra, b.WM.Extra)
	add(settings.KeySecurityWatermarkOpacity, a.WM.Opacity, b.WM.Opacity)
	add(settings.KeySecurityWatermarkFontSize, a.WM.FontSize, b.WM.FontSize)
	add(settings.KeySecurityWatermarkAngle, a.WM.Angle, b.WM.Angle)
	add(settings.KeySecurityWatermarkGap, a.WM.Gap, b.WM.Gap)
	add(settings.KeySecurityWatermarkColor, a.WM.Color, b.WM.Color)
	add(settings.KeySecurityPasswordMinLength, a.PasswordMinLen, b.PasswordMinLen)
	add(settings.KeySecurityPasswordComplexity, a.PasswordComplexity, b.PasswordComplexity)
	add(settings.KeySecuritySessionMinutes, a.SessionMinutes, b.SessionMinutes)
	add(settings.KeySecuritySessionCloseOnExit, a.SessionBrowserClose, b.SessionBrowserClose)
	add(settings.KeySecurityJWTHours, a.JWTHours, b.JWTHours)
	add(settings.KeySecurityAPIJWTRequireOTP, a.APIJWTRequireOTP, b.APIJWTRequireOTP)
	add(settings.KeySecurityAPIUserIDs, a.APIWhitelist, b.APIWhitelist)
	add(settings.KeySecurityDefaultAuthGroups, a.DefaultAuthGroups, b.DefaultAuthGroups)
	add(settings.KeySecurityDefaultResourceGroups, a.DefaultResourceGroups, b.DefaultResourceGroups)
	add(settings.KeySMSProvider, a.SMS.Provider, b.SMS.Provider)
	add(settings.KeySMSAliyunKeyID, a.SMS.AliyunKeyID, b.SMS.AliyunKeyID)
	add(settings.KeySMSAliyunSign, a.SMS.AliyunSign, b.SMS.AliyunSign)
	add(settings.KeySMSAliyunTemplate, a.SMS.AliyunTemplate, b.SMS.AliyunTemplate)
	add(settings.KeySMSTencentSecretID, a.SMS.TencentSecretID, b.SMS.TencentSecretID)
	add(settings.KeySMSTencentSign, a.SMS.TencentSign, b.SMS.TencentSign)
	add(settings.KeySMSTencentTemplate, a.SMS.TencentTemplate, b.SMS.TencentTemplate)
	add(settings.KeySMSTencentAppID, a.SMS.TencentAppID, b.SMS.TencentAppID)
	if a.SMS.AliyunKeySecret != b.SMS.AliyunKeySecret {
		out = append(out, "sms.aliyun_access_key_secret: 已修改") // i18n-ignore 审计文本（存库）
	}
	if a.SMS.TencentSecretKey != b.SMS.TencentSecretKey {
		out = append(out, "sms.tencent_secret_key: 已修改") // i18n-ignore 审计文本（存库）
	}
	if len(out) == 0 {
		return "无变化" // i18n-ignore 审计文本（存库）
	}
	return strings.Join(out, "；")
}

// apiSMSTest 给指定手机号发一条测试验证码，用来验证短信服务配置（走已保存的配置）。
func (h *handlers) apiSMSTest(c *gin.Context) {
	var in struct {
		Phone string `json:"phone"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数错误")
		return
	}
	phone, err := NormalizePhone(in.Phone)
	if err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	sender, err := h.svc.sms.Sender(h.svc.Settings().SMS)
	if err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", localizeMsg(reqLang(c), errText(err)))
		return
	}
	if err := sender.Send(c.Request.Context(), phone, randDigits(6)); err != nil {
		slog.Warn("短信服务测试失败", "err", err)
		server.APIFail(c, http.StatusBadGateway, "sms_failed", fmt.Sprintf("短信发送失败：%s", localizeMsg(reqLang(c), truncRunes(errText(err), 120))))
		return
	}
	h.svc.auditActor(actorOf(c), "安全设置-短信测试", MaskPhone(phone)) // i18n-ignore 审计文本（存库）
	server.APIOK(c, gin.H{"ok": true})
}

// ---- 登录用户自助管理两步验证 ----

func selfPending(c *gin.Context) *Pending {
	p := server.PrincipalOf(c)
	return &Pending{UID: p.ID, Username: p.Username, Mode: ModeBind, Nonce: "self:" + strconv.FormatUint(uint64(p.ID), 10)}
}

func (h *handlers) apiTwoFAState(c *gin.Context) {
	p := server.PrincipalOf(c)
	st, err := h.svc.TwoFAState(p.ID)
	if err != nil {
		fail(c, err)
		return
	}
	set := h.svc.Settings()
	server.APIOK(c, gin.H{"bound": st, "enforced": set.Enforce2FA, "sms_available": set.SMS.Provider != SMSDisabled})
}

func (h *handlers) apiTOTPBegin(c *gin.Context) {
	setup, err := h.svc.BeginTOTP(selfPending(c))
	if err != nil {
		fail(c, err)
		return
	}
	noStore(c)
	server.APIOK(c, setup)
}

func (h *handlers) twofaFail(c *gin.Context, err error) {
	var fe *FlowError
	if errors.As(err, &fe) {
		status := http.StatusBadRequest
		if fe.Code == ErrTwoFABad.Code || fe.Code == ErrTwoFALocked.Code {
			status = http.StatusUnauthorized
		}
		server.APIFail(c, status, fe.Code, fe.Msg)
		return
	}
	fail(c, err)
}

func (h *handlers) apiTOTPConfirm(c *gin.Context) {
	var in struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数错误")
		return
	}
	if err := h.svc.VerifyTwoFA(selfPending(c), Method2FATOTP, in.Code, ""); err != nil {
		h.twofaFail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiSMSSend(c *gin.Context) {
	var in struct {
		Phone string `json:"phone"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数错误")
		return
	}
	if err := h.svc.SendSMS(c.Request.Context(), selfPending(c), in.Phone); err != nil {
		h.twofaFail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiSMSConfirm(c *gin.Context) {
	var in struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数错误")
		return
	}
	if err := h.svc.VerifyTwoFA(selfPending(c), Method2FASMS, in.Code, in.Phone); err != nil {
		h.twofaFail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

func (h *handlers) apiTwoFADisable(c *gin.Context) {
	typ := c.Param("type")
	if typ != Method2FATOTP && typ != Method2FASMS {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "不支持的验证方式")
		return
	}
	p := server.PrincipalOf(c)
	if err := h.svc.repo.DeleteTwoFA(p.ID, typ); err != nil {
		fail(c, err)
		return
	}
	h.svc.audit(p.ID, p.Username, p.Display, "2FA-解绑", typ) // i18n-ignore 审计文本（存库）
	server.APIOK(c, gin.H{"ok": true})
}

// apiUI 给前端公共脚本用的界面配置：页面水印开关、内容与样式。
// 内容模板里的 {账号} {姓名} {部门} {IP} 已按当前用户展开，{日期} {时间} 留给页面脚本每分钟替换。
func (h *handlers) apiUI(c *gin.Context) {
	p := server.PrincipalOf(c)
	server.APIOK(c, h.svc.WatermarkFor(p.ID, p.Username, p.Display, c.ClientIP()))
}

// 水印被删除 / 隐藏 / 改样式后前端上报的类型（枚举）：服务端只认这三个，别的内容一律不信任、不记录。
var wmTamperKinds = map[string]string{"removed": "水印层被删除", "hidden": "水印层被隐藏", "style": "水印样式被改动"} // i18n-ignore 水印篡改类型的审计标签（存库）

const (
	wmTamperLimit  = 3 // 每用户每分钟最多上报次数
	wmTamperWindow = time.Minute
)

// tamperLimiter 按用户限制水印篡改上报的频率（内存，滑动窗口）。
type tamperLimiter struct {
	mu   sync.Mutex
	hits map[uint][]time.Time
}

func (l *tamperLimiter) allow(uid uint, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.hits[uid][:0]
	for _, t := range l.hits[uid] {
		if now.Sub(t) < wmTamperWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= wmTamperLimit {
		l.hits[uid] = kept
		return false
	}
	l.hits[uid] = append(kept, now)
	if len(l.hits) > 4096 { // 清理已经没有窗口内记录的用户
		for k, v := range l.hits {
			if len(v) == 0 || now.Sub(v[len(v)-1]) >= wmTamperWindow {
				delete(l.hits, k)
			}
		}
	}
	return true
}

// apiWatermarkTamper 接收前端的水印篡改上报（S9-A3）：任何登录用户可调，body 只认 {"kind": "removed"|"hidden"|"style"}，
// 其余字段忽略；每用户每分钟最多 3 次，超过返回 429（不写审计）。写审计“水印篡改”（用户来自会话，含 IP 与类型）。响应不含任何信息。
func (h *handlers) apiWatermarkTamper(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<10)
	var in struct {
		Kind string `json:"kind"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数非法")
		return
	}
	label, ok := wmTamperKinds[in.Kind]
	if !ok {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数非法")
		return
	}
	p := server.PrincipalOf(c)
	if !h.wm.allow(p.ID, h.svc.now()) {
		server.APIFail(c, http.StatusTooManyRequests, "too_many_requests", "上报过于频繁，请稍后再试")
		return
	}
	h.svc.audit(p.ID, p.Username, p.Display, "水印篡改", fmt.Sprintf("IP：%s；类型：%s（%s）", c.ClientIP(), in.Kind, label)) // i18n-ignore 审计文本（存库）
	server.APIOK(c, gin.H{"ok": true})
}

var _ = gorm.ErrRecordNotFound
