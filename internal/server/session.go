// @author: kevinkelly
// @license: Apache Licence
// @file: session.go
// @time: 2026/10/28

package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	sessionCookie   = "dba_session"
	loginCSRFCookie = "dba_login_csrf"
	skinCookie      = "dbtool_skin" // 主题选择
	sessionTTL      = 5 * time.Hour // 300 分钟
)

// Session 是写入 Cookie 的会话内容。采用无状态的签名 Cookie（HMAC-SHA256），
// 不依赖服务端存储；代价是无法服务端主动踢出，后续引入 Redis 后可改为服务端会话。
type Session struct {
	UID         uint   `json:"uid"`
	Username    string `json:"u"`
	Display     string `json:"d"`
	IsSuperuser bool   `json:"su"`
	CSRF        string `json:"c"`
	Exp         int64  `json:"e"`
	Prov        uint   `json:"p,omitempty"` // 登录用的外部登录方式 ID（0 = 本地账号密码）；退出时据此决定是否同时登出 CAS
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // 系统随机源不可用时不应继续运行
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func sign(key []byte, payload string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func encodeSession(key []byte, s Session) (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	p := base64.RawURLEncoding.EncodeToString(raw)
	return p + "." + sign(key, p), nil
}

var errBadSession = errors.New("会话无效") // i18n-ignore

func decodeSession(key []byte, v string, now time.Time) (*Session, error) {
	p, sig, ok := strings.Cut(v, ".")
	if !ok || subtle.ConstantTimeCompare([]byte(sig), []byte(sign(key, p))) != 1 {
		return nil, errBadSession
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return nil, errBadSession
	}
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil || s.Exp < now.Unix() || s.UID == 0 {
		return nil, errBadSession
	}
	return &s, nil
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, sess Session) error {
	pol := s.sessionPolicy()
	sess.Exp = time.Now().Add(pol.TTL).Unix()
	if sess.CSRF == "" { // 新登录时没有 CSRF：每次登录都换一个新的会话与令牌（防会话固定）
		sess.CSRF = randToken(24)
	}
	v, err := encodeSession(s.key, sess)
	if err != nil {
		return err
	}
	ck := &http.Cookie{
		Name: sessionCookie, Value: v, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		// 默认不设置 Expires：关闭浏览器即失效；服务端另有 Exp 校验。
	}
	if pol.Persistent {
		ck.MaxAge = int(pol.TTL.Seconds())
	}
	http.SetCookie(w, ck)
	return nil
}

// sessionPolicy 返回会话策略：安装了登录扩展（internal/authx）时读它的设置，否则用内置默认值。
func (s *Server) sessionPolicy() SessionPolicy {
	if s.login != nil {
		if p := s.login.SessionPolicy(); p.TTL > 0 {
			return p
		}
	}
	return SessionPolicy{TTL: sessionTTL}
}

// tokenDuration 是 JWT 有效期。
func (s *Server) tokenDuration() time.Duration {
	if s.login != nil {
		if d := s.login.TokenTTL(); d > 0 {
			return d
		}
	}
	return tokenTTL
}

// BackgroundPollHeader 标记“页面自己定时发的请求”（如顶栏值班芯片刷新）：带这个请求头的 API 请求不给滑动会话续期，
// 否则一个放着没人看的页面会让会话永不过期。会话本身是否有效照常校验。
const BackgroundPollHeader = "X-Background-Poll"

// touchSession 实现会话滑动续期：距上次签发超过一分钟就重新签发，最多每分钟一次。
func (s *Server) touchSession(c *gin.Context, sess *Session) {
	pol := s.sessionPolicy()
	if !pol.Sliding {
		return
	}
	if time.Until(time.Unix(sess.Exp, 0)) < pol.TTL-time.Minute {
		_ = s.setSession(c.Writer, c.Request, *sess)
	}
}

func clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// sessionFrom 从请求 Cookie 解出会话，失败返回 nil。
func (s *Server) sessionFrom(r *http.Request) *Session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	sess, err := decodeSession(s.key, c.Value, time.Now())
	if err != nil {
		return nil
	}
	return sess
}

// skinOf 读取主题 Cookie，只接受白名单值，避免被注入到模板属性里。
func skinOf(r *http.Request) string {
	if c, err := r.Cookie(skinCookie); err == nil {
		switch c.Value {
		case "light", "sepia", "dark":
			return c.Value
		}
	}
	return "dark"
}
