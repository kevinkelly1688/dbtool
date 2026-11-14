// @author: kevinkelly
// @license: Apache Licence
// @file: loginext_test.go
// @time: 2026/10/28

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"dbtool/internal/config"
)

// fakeExt 是登录扩展的假实现：按字段返回预设结果。
type fakeExt struct {
	pwOut     LoginOutcome
	apiOut    LoginOutcome
	pol       SessionPolicy
	tokenTTL  time.Duration
	allowed   func(uint) bool
	gotPw     PasswordLoginReq
	gotAPI    APILoginReq
	viewCalls int
}

func (f *fakeExt) LoginView(*http.Request) any {
	f.viewCalls++
	return map[string]any{"ShowForm": true}
}
func (f *fakeExt) PasswordLogin(w http.ResponseWriter, r *http.Request, req PasswordLoginReq) LoginOutcome {
	f.gotPw = req
	http.SetCookie(w, &http.Cookie{Name: "ext_marker", Value: "1", Path: "/login"})
	return f.pwOut
}
func (f *fakeExt) APILogin(_ *http.Request, req APILoginReq) LoginOutcome {
	f.gotAPI = req
	return f.apiOut
}
func (f *fakeExt) SessionPolicy() SessionPolicy { return f.pol }
func (f *fakeExt) TokenTTL() time.Duration      { return f.tokenTTL }
func (f *fakeExt) APIAllowed(uid uint) bool {
	if f.allowed == nil {
		return true
	}
	return f.allowed(uid)
}

func serverCfgForTest() *config.Config {
	return &config.Config{SecretKey: "unit-test-secret-key-unit-test-secret-key"}
}

// extEnv 重建一个装配了登录扩展的服务（沿用 newEnv 的内存仓库与用户）。
func extEnv(t *testing.T, f *fakeExt) *env {
	t.Helper()
	e := newEnv(t)
	r, err := New(serverCfgForTest(), e.repo, Options{Hooks: []func(*Server, Routes){func(s *Server, rt Routes) { s.SetLoginExt(f) }}})
	if err != nil {
		t.Fatal(err)
	}
	e.h = r
	return e
}

func formLogin(t *testing.T, e *env, user, pw, next string) *httptest.ResponseRecorder {
	t.Helper()
	w := e.do(httptest.NewRequest("GET", "/login", nil))
	lc := cookieNamed(w, loginCSRFCookie)
	body := w.Body.String()
	i := strings.Index(body, `name="csrf" value="`)
	if lc == nil || i < 0 {
		t.Fatalf("登录页: %d", w.Code)
	}
	csrf := body[i+len(`name="csrf" value="`):]
	csrf = csrf[:strings.Index(csrf, `"`)]
	form := url.Values{"username": {user}, "password": {pw}, "csrf": {csrf}, "next": {next}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(lc)
	return e.do(req)
}

func TestLoginExtSessionOutcome(t *testing.T) {
	f := &fakeExt{pol: SessionPolicy{TTL: 10 * time.Minute, Persistent: true, Sliding: true}, tokenTTL: time.Hour,
		pwOut: LoginOutcome{Session: &Session{UID: 7, Username: "ext", Display: "扩展", IsSuperuser: false}}}
	e := extEnv(t, f)
	w := formLogin(t, e, "ext", "x", "/sqlworkflow")
	if w.Code != 303 || w.Header().Get("Location") != "/sqlworkflow" {
		t.Fatalf("应建立会话并跳转: %d %s", w.Code, w.Header().Get("Location"))
	}
	sc := cookieNamed(w, sessionCookie)
	if sc == nil || sc.MaxAge != 600 {
		t.Fatalf("会话策略应生效（有效期 10 分钟、带 Max-Age）: %+v", sc)
	}
	sess, err := decodeSession([]byte("unit-test-secret-key-unit-test-secret-key"), sc.Value, time.Now())
	if err != nil || sess.UID != 7 || sess.CSRF == "" {
		t.Fatalf("会话内容: %+v %v", sess, err)
	}
	if d := time.Until(time.Unix(sess.Exp, 0)); d > 11*time.Minute || d < 9*time.Minute {
		t.Errorf("Exp: %v", d)
	}
	if f.gotPw.Next != "/sqlworkflow" || f.gotPw.Username != "ext" || f.gotPw.Local {
		t.Errorf("传给扩展的请求: %+v", f.gotPw)
	}
	// 登录页 CSRF Cookie 被清除（StartSession），且每次登录换新 CSRF
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == loginCSRFCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("登录成功后应清掉登录页 CSRF Cookie")
	}
	w2 := formLogin(t, e, "ext", "x", "/query")
	s2, _ := decodeSession([]byte("unit-test-secret-key-unit-test-secret-key"), cookieNamed(w2, sessionCookie).Value, time.Now())
	if s2.CSRF == sess.CSRF {
		t.Error("每次登录都应是新的 CSRF")
	}
	// 未安全的 next 被拦截
	if w := formLogin(t, e, "ext", "x", "//evil.example.com"); w.Header().Get("Location") != "/" {
		t.Errorf("open redirect: %s", w.Header().Get("Location"))
	}
}

func TestLoginExtRedirectAndFailureOutcomes(t *testing.T) {
	f := &fakeExt{pol: SessionPolicy{TTL: time.Hour}, pwOut: LoginOutcome{Redirect: "/login/2fa"}}
	e := extEnv(t, f)
	w := formLogin(t, e, "u", "p", "/query")
	if w.Code != 303 || w.Header().Get("Location") != "/login/2fa" || cookieNamed(w, sessionCookie) != nil || cookieNamed(w, "ext_marker") == nil {
		t.Errorf("Redirect 结果不应建立会话，扩展设置的 Cookie 要保留: %d", w.Code)
	}
	// 失败：计入限流、用扩展给的状态与文案
	f.pwOut = LoginOutcome{Message: "自定义失败信息", Count: true, Status: http.StatusUnauthorized}
	for i := 0; i < 5; i++ {
		if w := formLogin(t, e, "u", "p", "/query"); w.Code != 401 || !strings.Contains(w.Body.String(), "自定义失败信息") {
			t.Fatalf("失败: %d", w.Code)
		}
	}
	if w := formLogin(t, e, "u", "p", "/query"); w.Code != 429 {
		t.Errorf("Count=true 应计入限流: %d", w.Code)
	}
	// 失败但不计数
	f2 := &fakeExt{pol: SessionPolicy{TTL: time.Hour}, pwOut: LoginOutcome{Message: "x", Status: 423}}
	e2 := extEnv(t, f2)
	for i := 0; i < 7; i++ {
		if w := formLogin(t, e2, "u", "p", "/query"); w.Code != 423 {
			t.Fatalf("自定义状态码: %d", w.Code)
		}
	}
	// 内部错误
	f3 := &fakeExt{pol: SessionPolicy{TTL: time.Hour}, pwOut: LoginOutcome{Err: http.ErrAbortHandler}}
	if w := formLogin(t, extEnv(t, f3), "u", "p", "/query"); w.Code != 500 || strings.Contains(w.Body.String(), "ErrAbortHandler") {
		t.Errorf("内部错误不暴露细节: %d", w.Code)
	}
	// 登录页拿得到扩展提供的数据
	if f.viewCalls == 0 {
		t.Error("渲染登录页时应调用 LoginView")
	}
}

func TestLoginExtAPIAndTokenTTL(t *testing.T) {
	f := &fakeExt{pol: SessionPolicy{TTL: time.Hour}, tokenTTL: 90 * time.Minute, apiOut: LoginOutcome{Session: &Session{UID: 3, Username: "bot"}}}
	e := extEnv(t, f)
	post := func(body map[string]string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		return e.do(httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewReader(raw)))
	}
	w := post(map[string]string{"username": "bot", "password": "x", "otp": "123456"})
	var out struct{ Token string }
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || out.Token == "" || f.gotAPI.OTP != "123456" || f.gotAPI.Username != "bot" {
		t.Fatalf("API 登录: %d %+v", w.Code, f.gotAPI)
	}
	tok, _, _ := jwt.NewParser().ParseUnverified(out.Token, jwt.MapClaims{})
	exp, _ := tok.Claims.(jwt.MapClaims).GetExpirationTime()
	if d := time.Until(exp.Time); d < 89*time.Minute || d > 91*time.Minute {
		t.Errorf("JWT 有效期应取自扩展: %v", d)
	}
	// 白名单：Bearer 调用受限，会话调用不受限
	f.allowed = func(uid uint) bool { return uid != 3 }
	req := httptest.NewRequest("GET", "/api/v1/query/instances", nil)
	req.Header.Set("Authorization", "Bearer "+out.Token)
	if w := e.do(req); w.Code != 403 || !strings.Contains(w.Body.String(), "白名单") {
		t.Errorf("白名单外的 Bearer 应 403: %d %s", w.Code, w.Body.String())
	}
	f.allowed = nil
	if w := e.do(req); w.Code != 200 {
		t.Errorf("白名单放行: %d", w.Code)
	}
	// 失败结果：状态码与错误码来自扩展，默认 401 + bad_credentials
	f.apiOut = LoginOutcome{Message: "需要动态口令", Code: "otp_required"}
	w = post(map[string]string{"username": "bot", "password": "x"})
	var fail struct{ Code, Message string }
	json.Unmarshal(w.Body.Bytes(), &fail)
	if w.Code != 401 || fail.Code != "otp_required" || fail.Message != "需要动态口令" {
		t.Errorf("失败: %d %+v", w.Code, fail)
	}
	f.apiOut = LoginOutcome{Message: "拒绝", Status: 403, Code: "forbidden"}
	if w := post(map[string]string{"username": "bot", "password": "x"}); w.Code != 403 {
		t.Errorf("自定义状态: %d", w.Code)
	}
	f.apiOut = LoginOutcome{Err: http.ErrAbortHandler}
	if w := post(map[string]string{"username": "bot", "password": "x"}); w.Code != 500 {
		t.Errorf("内部错误: %d", w.Code)
	}
}

func TestSlidingSessionRenewal(t *testing.T) {
	f := &fakeExt{pol: SessionPolicy{TTL: time.Hour, Sliding: true}, tokenTTL: time.Hour}
	e := extEnv(t, f)
	key := []byte("unit-test-secret-key-unit-test-secret-key")
	mk := func(remaining time.Duration) *http.Cookie {
		v, _ := encodeSession(key, Session{UID: 2, Username: "bob", CSRF: "keep-me", Exp: time.Now().Add(remaining).Unix()})
		return &http.Cookie{Name: sessionCookie, Value: v}
	}
	// 剩余不到“有效期 - 1 分钟”：续期，并且 CSRF 不变
	req := httptest.NewRequest("GET", "/api/v1/query/instances", nil)
	req.AddCookie(mk(20 * time.Minute))
	w := e.do(req)
	c := cookieNamed(w, sessionCookie)
	if w.Code != 200 || c == nil {
		t.Fatalf("应续期: %d", w.Code)
	}
	s, _ := decodeSession(key, c.Value, time.Now())
	if s.CSRF != "keep-me" || time.Until(time.Unix(s.Exp, 0)) < 59*time.Minute {
		t.Errorf("续期后: %+v", s)
	}
	// 刚签发的不重复续期
	req = httptest.NewRequest("GET", "/api/v1/query/instances", nil)
	req.AddCookie(mk(time.Hour - 10*time.Second))
	if c := cookieNamed(e.do(req), sessionCookie); c != nil {
		t.Error("一分钟内不应重复签发")
	}
	// 页面请求同样续期
	req = httptest.NewRequest("GET", "/query", nil)
	req.AddCookie(mk(5 * time.Minute))
	if cookieNamed(e.do(req), sessionCookie) == nil {
		t.Error("页面请求也应续期")
	}
	// 未启用滑动续期（默认）时不动 Cookie
	f.pol.Sliding = false
	req = httptest.NewRequest("GET", "/api/v1/query/instances", nil)
	req.AddCookie(mk(5 * time.Minute))
	if cookieNamed(e.do(req), sessionCookie) != nil {
		t.Error("没开滑动续期不应重发 Cookie")
	}
}

// 页面自己定时发的请求（顶栏值班芯片）带 X-Background-Poll：会话照常校验，但不续期，免得一个没人看的页面让登录永不过期。
func TestBackgroundPollDoesNotRenewSession(t *testing.T) {
	f := &fakeExt{pol: SessionPolicy{TTL: time.Hour, Sliding: true}, tokenTTL: time.Hour}
	e := extEnv(t, f)
	key := []byte("unit-test-secret-key-unit-test-secret-key")
	v, _ := encodeSession(key, Session{UID: 2, Username: "bob", CSRF: "keep-me", Exp: time.Now().Add(20 * time.Minute).Unix()})
	get := func(cookie string, poll bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/v1/query/instances", nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		}
		if poll {
			req.Header.Set(BackgroundPollHeader, "1")
		}
		return e.do(req)
	}
	if w := get(v, true); w.Code != 200 || cookieNamed(w, sessionCookie) != nil {
		t.Errorf("后台轮询不应给会话续期：%d", w.Code)
	}
	if w := get(v, false); w.Code != 200 || cookieNamed(w, sessionCookie) == nil {
		t.Errorf("普通请求应续期：%d", w.Code)
	}
	if w := get("", true); w.Code != 401 {
		t.Errorf("后台轮询也要登录：%d", w.Code)
	}
}

func TestPublicRoutesGroup(t *testing.T) {
	var got Routes
	r, err := New(serverCfgForTest(), newEnv(t).repo, Options{Hooks: []func(*Server, Routes){func(s *Server, rt Routes) {
		got = rt
		rt.Public.GET("/login/probe", func(c *gin.Context) { c.String(200, "open") })
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Public == nil {
		t.Fatal("Routes.Public 应存在")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/login/probe", nil))
	if w.Code != 200 || w.Body.String() != "open" {
		t.Errorf("Public 路由不需要登录: %d", w.Code)
	}
}
