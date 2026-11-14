// @author: kevinkelly
// @license: Apache Licence
// @file: auth_handlers.go
// @time: 2026/10/28

package server

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"

	"dbtool/internal/crypto"
)

const tokenTTL = sessionTTL

type claims struct {
	UID         uint   `json:"uid"`
	Username    string `json:"username"`
	IsSuperuser bool   `json:"su"`
	jwt.RegisteredClaims
}

func (s *Server) signToken(uid uint, username string, su bool) (string, error) {
	now := time.Now()
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		UID: uid, Username: username, IsSuperuser: su,
		RegisteredClaims: jwt.RegisteredClaims{IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(s.tokenDuration()))},
	})
	return t.SignedString(s.key)
}

func constTimeEq(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// safeNext 只允许站内相对路径，防止登录后被重定向到外部站点。
func safeNext(next string) string {
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") && !strings.Contains(next, "\\") {
		return next
	}
	return "/" // 默认登录后进入首页（工作台）
}

func safeNextEscape(uri string) string { return url.QueryEscape(safeNext(uri)) }

// ---- 登录失败限流：同一 用户名+IP 在窗口内失败次数过多则暂时拒绝 ----

type limiter struct {
	mu     sync.Mutex
	fails  map[string][]time.Time
	max    int
	window time.Duration
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{fails: map[string][]time.Time{}, max: max, window: window}
}

func (l *limiter) prune(key string, now time.Time) []time.Time {
	kept := l.fails[key][:0]
	for _, t := range l.fails[key] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, key)
	} else {
		l.fails[key] = kept
	}
	return kept
}

func (l *limiter) blocked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key, now)) >= l.max
}

func (l *limiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[key] = append(l.prune(key, now), now)
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

var dummyHashOnce sync.Once
var dummyHash string

// verifyLogin 校验账号密码。用户不存在时也做一次等价耗时的哈希计算，避免通过响应时间枚举账号。
func (s *Server) verifyLogin(username, password string) (*Session, error) {
	u, err := s.repo.GetUserByName(username)
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		dummyHashOnce.Do(func() { dummyHash, _ = crypto.MakeDjangoPassword("x", "dummysalt", 390000) })
		crypto.CheckDjangoPassword(password, dummyHash)
		return nil, errBadCredentials
	}
	if !u.IsActive || !crypto.CheckDjangoPassword(password, u.Password) {
		return nil, errBadCredentials
	}
	return &Session{UID: u.ID, Username: u.Username, Display: u.Display, IsSuperuser: u.IsSuperuser}, nil
}

var errBadCredentials = errors.New("用户名或密码错误")

func (s *Server) loginPage(c *gin.Context) {
	if s.sessionFrom(c.Request) != nil {
		c.Redirect(http.StatusFound, safeNext(c.Query("next")))
		return
	}
	s.renderLogin(c, http.StatusOK, "")
}

func (s *Server) renderLogin(c *gin.Context, status int, errMsg string) {
	tok := randToken(18)
	http.SetCookie(c.Writer, &http.Cookie{Name: loginCSRFCookie, Value: tok, Path: "/login", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https"})
	next := c.Query("next")
	if next == "" {
		next = c.PostForm("next")
	}
	var data any
	if s.login != nil {
		data = s.login.LoginView(c.Request)
	}
	s.render(c, status, "login.html", pageData{Title: "登录", CSRF: tok, Error: errMsg, Next: safeNext(next), Data: data})
}

func (s *Server) loginSubmit(c *gin.Context) {
	ck, err := c.Request.Cookie(loginCSRFCookie)
	if err != nil || !constTimeEq(c.PostForm("csrf"), ck.Value) {
		s.renderLogin(c, http.StatusForbidden, "页面已过期，请重新提交")
		return
	}
	username := strings.TrimSpace(c.PostForm("username"))
	key := strings.ToLower(username) + "|" + c.ClientIP()
	now := time.Now()
	if s.limit.blocked(key, now) {
		s.renderLogin(c, http.StatusTooManyRequests, "尝试次数过多，请稍后再试")
		return
	}
	if s.login != nil { // 安装了 internal/authx：锁定、LDAP、两步验证等都在那里
		next := safeNext(c.PostForm("next"))
		out := s.login.PasswordLogin(c.Writer, c.Request, PasswordLoginReq{Username: username, Password: c.PostForm("password"), Next: next, IP: c.ClientIP(), Local: c.PostForm("local") == "1"})
		switch {
		case out.Err != nil:
			slog.Error("登录校验失败", "err", out.Err)
			s.renderLogin(c, http.StatusInternalServerError, "服务异常，请稍后再试")
		case out.Session != nil:
			s.limit.reset(key)
			if err := s.StartSession(c.Writer, c.Request, *out.Session); err != nil {
				s.renderLogin(c, http.StatusInternalServerError, "服务异常，请稍后再试")
				return
			}
			c.Redirect(http.StatusSeeOther, next)
		case out.Redirect != "":
			s.limit.reset(key)
			c.Redirect(http.StatusSeeOther, out.Redirect)
		default:
			if out.Count {
				s.limit.fail(key, now)
			}
			status := out.Status
			if status == 0 {
				status = http.StatusUnauthorized
			}
			s.renderLogin(c, status, out.Message)
		}
		return
	}
	sess, err := s.verifyLogin(username, c.PostForm("password"))
	if err != nil {
		if errors.Is(err, errBadCredentials) {
			s.limit.fail(key, now)
			s.renderLogin(c, http.StatusUnauthorized, err.Error())
			return
		}
		slog.Error("登录校验失败", "err", err)
		s.renderLogin(c, http.StatusInternalServerError, "服务异常，请稍后再试")
		return
	}
	s.limit.reset(key)
	if err := s.setSession(c.Writer, c.Request, *sess); err != nil {
		s.renderLogin(c, http.StatusInternalServerError, "服务异常，请稍后再试")
		return
	}
	c.Redirect(http.StatusSeeOther, safeNext(c.PostForm("next")))
}

func (s *Server) logout(c *gin.Context) {
	if !constTimeEq(c.PostForm("csrf"), sessionOf(c).CSRF) {
		c.String(http.StatusForbidden, tr(c, "CSRF 校验失败"))
		return
	}
	if lo, ok := s.login.(LogoutObserver); ok && sessionOf(c) != nil { // 退出登录留痕（会话存在才记）
		lo.OnLogout(c.Request, sessionOf(c), c.ClientIP())
	}
	to := "/login"
	if lr, ok := s.login.(LogoutRedirector); ok { // CAS 全局登出：退出后跳到 CAS 的登出地址（由它再回到登录页）
		if u := lr.LogoutRedirect(c.Request, sessionOf(c)); u != "" {
			to = u
		}
	}
	clearSession(c.Writer)
	c.Redirect(http.StatusSeeOther, to)
}

type loginReq struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
	OTP      string `json:"otp"` // 仅 authx 开启“JWT 登录需要动态口令”时使用
}

// apiLogin 供脚本/第三方换取 JWT。
func (s *Server) apiLogin(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "参数错误")
		return
	}
	key := strings.ToLower(req.Username) + "|" + c.ClientIP()
	now := time.Now()
	if s.limit.blocked(key, now) {
		apiFail(c, http.StatusTooManyRequests, "too_many_attempts", "尝试次数过多，请稍后再试")
		return
	}
	if s.login != nil {
		out := s.login.APILogin(c.Request, APILoginReq{Username: req.Username, Password: req.Password, OTP: req.OTP, IP: c.ClientIP()})
		switch {
		case out.Err != nil:
			slog.Error("登录校验失败", "err", out.Err)
			apiFail(c, http.StatusInternalServerError, "internal", "服务异常")
		case out.Session != nil:
			s.limit.reset(key)
			tok, err := s.signToken(out.Session.UID, out.Session.Username, out.Session.IsSuperuser)
			if err != nil {
				apiFail(c, http.StatusInternalServerError, "internal", "签发令牌失败")
				return
			}
			c.JSON(http.StatusOK, gin.H{"token": tok})
		default:
			if out.Count {
				s.limit.fail(key, now)
			}
			status, code := out.Status, out.Code
			if status == 0 {
				status = http.StatusUnauthorized
			}
			if code == "" {
				code = "bad_credentials"
			}
			apiFail(c, status, code, out.Message)
		}
		return
	}
	sess, err := s.verifyLogin(req.Username, req.Password)
	if err != nil {
		if errors.Is(err, errBadCredentials) {
			s.limit.fail(key, now)
			apiFail(c, http.StatusUnauthorized, "bad_credentials", err.Error())
			return
		}
		slog.Error("登录校验失败", "err", err)
		apiFail(c, http.StatusInternalServerError, "internal", "服务异常")
		return
	}
	s.limit.reset(key)
	tok, err := s.signToken(sess.UID, sess.Username, sess.IsSuperuser)
	if err != nil {
		apiFail(c, http.StatusInternalServerError, "internal", "签发令牌失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": tok})
}
