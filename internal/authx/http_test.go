// @author: kevinkelly
// @license: Apache Licence
// @file: http_test.go
// @time: 2026/10/28

package authx

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"dbtool/internal/config"
	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

const serverKey = "unit-test-secret-key-unit-test-secret-key"

type httpEnv struct {
	*testEnv
	h    http.Handler
	srv  *server.Server
	repo *servertest.Repo
}

func newHTTPEnv(t *testing.T) *httpEnv {
	t.Helper()
	e := newTestEnv(t)
	return mountHTTP(t, e)
}

func mountHTTP(t *testing.T, e *testEnv) *httpEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	he := &httpEnv{testEnv: e}
	// server 自己的仓库只用于权限查询；登录用户来自 authx 的仓库
	he.repo = &servertest.Repo{Users: map[string]*model.User{}, Perms: map[uint]map[string]bool{}}
	hook := func(s *server.Server, rt server.Routes) {
		he.srv = s
		register(s, rt, e.svc)
	}
	r, err := server.New(&config.Config{SecretKey: serverKey, PublicURL: "http://dba.test"}, he.repo, server.Options{Hooks: []func(*server.Server, server.Routes){hook}})
	if err != nil {
		t.Fatal(err)
	}
	he.h = r
	return he
}

// browser 是带 Cookie 的测试浏览器：按 Path 与 MaxAge 处理 Cookie，便于检查 Cookie 作用域。
type browser struct {
	t       *testing.T
	h       http.Handler
	cookies map[string]*http.Cookie
	last    *httptest.ResponseRecorder
}

func (he *httpEnv) browser() *browser {
	return &browser{t: he.t, h: he.h, cookies: map[string]*http.Cookie{}}
}

func (b *browser) do(method, path string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	b.attach(req)
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, req)
	b.absorb(w)
	b.last = w
	return w
}

func (b *browser) attach(req *http.Request) {
	for _, c := range b.cookies {
		if strings.HasPrefix(req.URL.Path, c.Path) {
			req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
		}
	}
}

func (b *browser) absorb(w *httptest.ResponseRecorder) {
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 || c.Value == "" {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
}

func (b *browser) get(path string) *httptest.ResponseRecorder { return b.do("GET", path, nil, nil) }

func (b *browser) cookie(name string) *http.Cookie { return b.cookies[name] }

// loginCSRF 取登录页 CSRF（页面 + Cookie 配对）。
func (b *browser) loginCSRF(path string) string {
	b.t.Helper()
	w := b.get(path)
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if w.Code != 200 || m == nil {
		b.t.Fatalf("登录页 %s: %d", path, w.Code)
	}
	return m[1]
}

func (b *browser) postLogin(user, pw, next string) *httptest.ResponseRecorder {
	csrf := b.loginCSRF("/login")
	return b.do("POST", "/login", url.Values{"username": {user}, "password": {pw}, "csrf": {csrf}, "next": {next}}, nil)
}

func (b *browser) session() *struct {
	UID  uint   `json:"uid"`
	User string `json:"u"`
	CSRF string `json:"c"`
	Exp  int64  `json:"e"`
	Su   bool   `json:"su"`
} {
	c := b.cookies["dba_session"]
	if c == nil {
		return nil
	}
	payload, _, _ := strings.Cut(c.Value, ".")
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		b.t.Fatal(err)
	}
	var s struct {
		UID  uint   `json:"uid"`
		User string `json:"u"`
		CSRF string `json:"c"`
		Exp  int64  `json:"e"`
		Su   bool   `json:"su"`
	}
	json.Unmarshal(raw, &s)
	return &s
}

func (b *browser) json(method, path string, body any) (int, map[string]any) {
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s := b.session(); s != nil && method != "GET" {
		req.Header.Set("X-CSRF-Token", s.CSRF)
	}
	b.attach(req)
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, req)
	b.absorb(w)
	b.last = w
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (he *httpEnv) loginAs(user string) *browser {
	he.t.Helper()
	b := he.browser()
	w := b.postLogin(user, pw, "/query")
	if w.Code != 303 || b.cookie("dba_session") == nil {
		he.t.Fatalf("登录 %s 失败: %d %s", user, w.Code, w.Body.String())
	}
	return b
}

// ---- 登录页与密码登录 ----

func TestLoginPageVariants(t *testing.T) {
	he := newHTTPEnv(t)
	b := he.browser()
	w := b.get("/login")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `name="username"`) || strings.Contains(body, "<details") || strings.Contains(body, "/login/sso/") {
		t.Fatalf("没有外部方式时只有表单: %d", w.Code)
	}
	if strings.Contains(body, "/login/signup") {
		t.Error("没开放注册时不应有注册表单")
	}
	// 一个 OIDC + 一个钉钉：按钮、折叠表单
	idp := newFakeOIDC(t, he.clk)
	oidc := he.addOIDC(idp, FirstLoginCreate)
	soc := newFakeSocial(t)
	_ = soc
	ding := he.addKind(KindDingTalk, "钉钉扫码", Config{ClientID: "ding-key", ClientSecret: "ding-secret"}, FirstLoginExisting)
	he.forceUsable(ding)
	w = b.get("/login?next=%2Fsqlworkflow")
	body = w.Body.String()
	for _, want := range []string{`href="/login/sso/` + itoa(oidc.ID) + `?next=%2Fsqlworkflow"`, "以公司账号登录", "钉钉扫码登录", `<details class="local-form"`, "或使用平台账号"} {
		if !strings.Contains(body, want) {
			t.Errorf("登录页缺少 %q", want)
		}
	}
	if strings.Contains(body, `<details class="local-form" open`) {
		t.Error("默认折叠")
	}
	if strings.Count(body, `class="btn sso primary"`) != 1 {
		t.Error("第一个按钮为主按钮")
	}
	if w = b.get("/login?local=1"); !strings.Contains(w.Body.String(), `<details class="local-form" open`) || !strings.Contains(w.Body.String(), `name="local" value="1"`) {
		t.Error("?local=1 展开表单并带 local 标记")
	}
	// 隐藏模式
	he.repo.Config = nil
	he.kvSet(settingLocalForm, FormHidden)
	w = b.get("/login")
	if strings.Contains(w.Body.String(), `name="password"`) {
		t.Error("隐藏模式不应渲染密码框")
	}
	if w = b.get("/login?local=1"); !strings.Contains(w.Body.String(), `name="password"`) {
		t.Error("后门永远可用")
	}
	// 注册入口
	he.kvSet(settingLocalForm, FormCollapse)
	he.repoCfgSet(cfgSignUp, "true")
	if !strings.Contains(b.get("/login").Body.String(), `action="/login/signup"`) {
		t.Error("开放注册后应有注册表单")
	}
	// ?err= 只显示固定文案，不回显任意文本
	w = b.get("/login?err=state")
	if !strings.Contains(w.Body.String(), "登录请求已失效") {
		t.Error("应显示固定文案")
	}
	w = b.get("/login?err=%3Cscript%3Ealert(1)%3C%2Fscript%3E&registered=1")
	if strings.Contains(w.Body.String(), "<script>alert") || !strings.Contains(w.Body.String(), "注册成功") {
		t.Error("未知错误码不回显")
	}
	// 页面不被缓存复用 CSRF：每次渲染重新下发 Cookie
	if b.cookie("dba_login_csrf") == nil {
		t.Error("应下发登录 CSRF Cookie")
	}
}

func (he *httpEnv) kvSet(k, v string) {
	he.repo2().kv[k] = v
	he.svc.settings.Invalidate()
}

func (he *httpEnv) repoCfgSet(k, v string) {
	he.repo2().cfg[k] = v
	he.svc.settings.Invalidate()
}

func (he *httpEnv) repo2() *memRepo { return he.testEnv.repo }

func TestPasswordLoginHTTP(t *testing.T) {
	he := newHTTPEnv(t)
	b := he.browser()
	w := b.postLogin("bob", "wrong-password-xyz", "/query")
	if w.Code != 401 || !strings.Contains(w.Body.String(), "用户名或密码错误") || b.cookie("dba_session") != nil {
		t.Fatalf("错误密码: %d", w.Code)
	}
	// 未知用户与已知用户错误信息完全一致
	w2 := b.postLogin("nobody", "wrong-password-xyz", "/query")
	strip := func(s string) string { return regexp.MustCompile(`(value|content)="[^"]+"`).ReplaceAllString(s, "") }
	if w2.Code != 401 || strip(w.Body.String()) != strip(w2.Body.String()) {
		t.Error("不能通过响应区分用户是否存在")
	}
	// CSRF
	b2 := he.browser()
	if w := b2.do("POST", "/login", url.Values{"username": {"bob"}, "password": {pw}, "csrf": {"forged"}}, nil); w.Code != 403 {
		t.Errorf("缺登录 CSRF: %d", w.Code)
	}
	// 成功
	b = he.browser()
	w = b.postLogin("bob", pw, "/query")
	if w.Code != 303 || w.Header().Get("Location") != "/query" {
		t.Fatalf("登录: %d %s", w.Code, w.Header().Get("Location"))
	}
	sc := b.cookie("dba_session")
	if !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode || sc.MaxAge != 0 {
		t.Errorf("会话 Cookie 属性: %+v", sc)
	}
	// open redirect
	for _, next := range []string{"//evil.example.com", "https://evil.example.com", "/\\evil.example.com", "javascript:alert(1)"} {
		bb := he.browser()
		if w := bb.postLogin("bob", pw, next); w.Header().Get("Location") != "/" {
			t.Errorf("next=%q 应回落到首页 /: %s", next, w.Header().Get("Location"))
		}
	}
	// 会话固定：带着旧会话再登录别的用户，拿到全新的会话与 CSRF
	old := b.session()
	b2 = he.browser()
	csrf2 := b2.loginCSRF("/login") // 先在没有会话的状态下取到登录页，再带上旧会话提交
	b2.cookies["dba_session"] = b.cookie("dba_session")
	w = b2.do("POST", "/login", url.Values{"username": {"root"}, "password": {pw}, "csrf": {csrf2}, "next": {"/query"}}, nil)
	nw := b2.session()
	if w.Code != 303 || nw.User != "root" || nw.CSRF == old.CSRF {
		t.Errorf("登录后应换新会话与 CSRF: %+v %+v", old, nw)
	}
	// 登录审计
	if !strings.Contains(strings.Join(he.repo2().auditActions(), ","), "登入") {
		t.Error("登录应写审计")
	}
	if u, _ := he.repo2().UserByID(2); u.LastLogin == nil {
		t.Error("应记录 last_login")
	}
}

func TestLoginRateLimitStillApplies(t *testing.T) {
	he := newHTTPEnv(t)
	b := he.browser()
	for i := 0; i < 5; i++ {
		b.postLogin("ghost", "wrong-password-xyz", "/query")
	}
	if w := b.postLogin("ghost", "wrong-password-xyz", "/query"); w.Code != 429 {
		t.Errorf("连续失败后应限流（对不存在的用户同样）: %d", w.Code)
	}
}

func TestAccountLockHTTP(t *testing.T) {
	he := newHTTPEnv(t)
	he.repoCfgSet(cfgLockCount, "3")
	for i := 0; i < 3; i++ {
		he.browser().postLogin("bob", "wrong-password-xyz", "/query")
	}
	b := he.browser()
	w := b.postLogin("bob", pw, "/query")
	if w.Code != 401 || b.cookie("dba_session") != nil || !strings.Contains(w.Body.String(), "用户名或密码错误") {
		t.Errorf("锁定后（换一个 IP / 浏览器）密码正确也应拒绝: %d", w.Code)
	}
	he.clk.Add(301 * time.Second)
	if w := he.browser().postLogin("bob", pw, "/query"); w.Code != 303 {
		t.Errorf("锁定时间过后应恢复: %d", w.Code)
	}
}

func TestSessionPolicyApplied(t *testing.T) {
	he := newHTTPEnv(t)
	he.kvSet(settingSessMin, "10")
	he.kvSet(settingSessClose, "false")
	b := he.browser()
	b.postLogin("bob", pw, "/query")
	s := b.session()
	if d := time.Until(time.Unix(s.Exp, 0)); d < 9*time.Minute || d > 11*time.Minute {
		t.Errorf("会话有效期应读设置（10 分钟）: %v", d)
	}
	if c := b.cookie("dba_session"); c.MaxAge != 600 {
		t.Errorf("关闭“关闭浏览器即退出”后 Cookie 应带 Max-Age: %d", c.MaxAge)
	}
	he.kvSet(settingSessMin, "300")
	he.kvSet(settingSessClose, "true")
	b = he.browser()
	b.postLogin("bob", pw, "/query")
	if d := time.Until(time.Unix(b.session().Exp, 0)); d < 299*time.Minute {
		t.Errorf("恢复默认 300 分钟: %v", d)
	}
	if b.cookie("dba_session").MaxAge != 0 {
		t.Error("默认是会话 Cookie")
	}
}

// signSession 按 server/session.go 的格式伪造一个会话 Cookie（测试里知道服务端密钥）。
func signSession(uid uint, name string, exp time.Time, csrf string) string {
	raw, _ := json.Marshal(map[string]any{"uid": uid, "u": name, "d": name, "su": false, "c": csrf, "e": exp.Unix()})
	p := base64.RawURLEncoding.EncodeToString(raw)
	m := hmac.New(sha256.New, []byte(serverKey))
	m.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func TestSlidingSession(t *testing.T) {
	he := newHTTPEnv(t)
	b := he.browser()
	b.cookies["dba_session"] = &http.Cookie{Name: "dba_session", Value: signSession(2, "bob", time.Now().Add(20*time.Minute), "csrf-1"), Path: "/"}
	code, _ := b.json("GET", "/api/v1/auth/2fa", nil)
	if code != 200 {
		t.Fatalf("%d", code)
	}
	s := b.session()
	if d := time.Until(time.Unix(s.Exp, 0)); d < 299*time.Minute || s.CSRF != "csrf-1" {
		t.Errorf("请求应续期并保持 CSRF 不变: exp 剩余 %v csrf=%s", d, s.CSRF)
	}
	// 刚续期过的不重复续期（不每个请求都 Set-Cookie）
	before := b.cookie("dba_session").Value
	b.json("GET", "/api/v1/auth/2fa", nil)
	if len(b.last.Result().Cookies()) != 0 {
		t.Error("一分钟内不应重复签发")
	}
	_ = before
}

// ---- JWT / API ----

func (he *httpEnv) apiLogin(user, pass, otp string) (int, map[string]any) {
	body := map[string]string{"username": user, "password": pass}
	if otp != "" {
		body["otp"] = otp
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	he.h.ServeHTTP(w, req)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestAPILoginPolicies(t *testing.T) {
	he := newHTTPEnv(t)
	code, out := he.apiLogin("bob", pw, "")
	if code != 200 || out["token"] == nil {
		t.Fatalf("JWT 登录: %d %v", code, out)
	}
	tok, _, err := jwt.NewParser().ParseUnverified(out["token"].(string), jwt.MapClaims{})
	if err != nil {
		t.Fatal(err)
	}
	exp, _ := tok.Claims.(jwt.MapClaims).GetExpirationTime()
	if d := time.Until(exp.Time); d < 3*time.Hour+50*time.Minute || d > 4*time.Hour+time.Minute {
		t.Errorf("默认 JWT 有效期 4 小时: %v", d)
	}
	// 有效期读设置
	he.kvSet(settingJWTHours, "1")
	_, out = he.apiLogin("bob", pw, "")
	tok, _, _ = jwt.NewParser().ParseUnverified(out["token"].(string), jwt.MapClaims{})
	exp, _ = tok.Claims.(jwt.MapClaims).GetExpirationTime()
	if d := time.Until(exp.Time); d > time.Hour+time.Minute {
		t.Errorf("JWT 有效期应为 1 小时: %v", d)
	}
	// 错误凭据
	if code, out := he.apiLogin("bob", "nope-nope-nope", ""); code != 401 || out["code"] != "bad_credentials" {
		t.Errorf("%d %v", code, out)
	}
	// 已绑定 2FA 的用户 JWT 登录默认不要求验证码
	secret := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	he.repo2().tfa[2] = []TwoFAConfig{{Type: "totp", Secret: secret}}
	if code, _ := he.apiLogin("bob", pw, ""); code != 200 {
		t.Errorf("默认不要求 OTP: %d", code)
	}
	// 开启开关后要求
	he.kvSet(settingJWTOTP, "true")
	if code, out := he.apiLogin("bob", pw, ""); code != 401 || out["code"] != "otp_required" {
		t.Errorf("缺 otp: %d %v", code, out)
	}
	if code, out := he.apiLogin("bob", pw, "000000"); code != 401 || out["code"] != "bad_otp" {
		t.Errorf("错误 otp: %d %v", code, out)
	}
	if code, _ := he.apiLogin("bob", pw, totpCode(t, secret, he.clk.Now())); code != 200 {
		t.Errorf("正确 otp: %d", code)
	}
	if code, _ := he.apiLogin("root", pw, ""); code != 200 {
		t.Errorf("没绑定的用户不受影响: %d", code)
	}
	// 隐藏本地表单不影响 API 登录
	he.kvSet(settingLocalForm, FormHidden)
	if code, _ := he.apiLogin("root", pw, ""); code != 200 {
		t.Errorf("API 登录不受登录页显示方式影响: %d", code)
	}
}

func TestAPIWhitelist(t *testing.T) {
	he := newHTTPEnv(t)
	_, out := he.apiLogin("bob", pw, "")
	bobTok := out["token"].(string)
	call := func(tok string) int {
		req := httptest.NewRequest("GET", "/api/v1/auth/2fa", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		he.h.ServeHTTP(w, req)
		return w.Code
	}
	if call(bobTok) != 200 {
		t.Fatal("白名单为空不限制")
	}
	he.repoCfgSet(cfgAPIWhitelist, "1") // 只有 root(id=1)
	if call(bobTok) != 403 {
		t.Error("不在白名单内的用户已有的令牌也应被拒绝")
	}
	if code, out := he.apiLogin("bob", pw, ""); code != 403 || out["code"] != "forbidden" {
		t.Errorf("不在白名单内不签发令牌: %d %v", code, out)
	}
	_, out = he.apiLogin("root", pw, "")
	if call(out["token"].(string)) != 200 {
		t.Error("白名单内用户可用")
	}
	// 浏览器会话不受白名单限制（页面自己的 AJAX 走会话）
	b := he.loginAs("bob")
	if code, _ := b.json("GET", "/api/v1/auth/2fa", nil); code != 200 {
		t.Errorf("会话访问不受 API 白名单限制: %d", code)
	}
}

// ---- 两步验证 ----

func (he *httpEnv) twofaPage(b *browser, m string) (int, string, string) {
	w := b.get("/login/2fa" + m)
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	c := ""
	if csrf != nil {
		c = csrf[1]
	}
	return w.Code, w.Body.String(), c
}

func TestTwoFAVerifyFlowHTTP(t *testing.T) {
	he := newHTTPEnv(t)
	secret := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	he.repo2().tfa[2] = []TwoFAConfig{{Type: "totp", Secret: secret}}
	b := he.browser()
	w := b.postLogin("bob", pw, "/sqlworkflow")
	if w.Code != 303 || w.Header().Get("Location") != "/login/2fa" || b.cookie("dba_session") != nil {
		t.Fatalf("密码通过后应先到两步验证、不能直接有会话: %d %s", w.Code, w.Header().Get("Location"))
	}
	pc := b.cookie("dba_2fa")
	if pc == nil || !pc.HttpOnly || pc.Path != "/login" || pc.MaxAge != 300 {
		t.Fatalf("中间态 Cookie: %+v", pc)
	}
	// 中间态不能当会话用
	if w := b.get("/query"); w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), "/login?next=") {
		t.Errorf("中间态不能访问业务页: %d", w.Code)
	}
	b2 := he.browser()
	b2.cookies["dba_session"] = &http.Cookie{Name: "dba_session", Value: pc.Value, Path: "/"}
	if code, _ := b2.json("GET", "/api/v1/auth/2fa", nil); code != 401 {
		t.Errorf("把中间态当会话 Cookie 用应 401: %d", code)
	}
	code, page, csrf := he.twofaPage(b, "")
	if code != 200 || !strings.Contains(page, "两步验证") || !strings.Contains(page, `name="code"`) || csrf == "" || strings.Contains(page, "tfa-qr") {
		t.Fatalf("验证页: %d", code)
	}
	// CSRF
	if w := b.do("POST", "/login/2fa", url.Values{"method": {"totp"}, "action": {"verify"}, "code": {totpCode(t, secret, he.clk.Now())}, "csrf": {"x"}}, nil); w.Code != 403 {
		t.Errorf("缺 CSRF: %d", w.Code)
	}
	// 错误验证码
	w = b.do("POST", "/login/2fa", url.Values{"method": {"totp"}, "action": {"verify"}, "code": {"000000"}, "csrf": {csrf}}, nil)
	if w.Code != 401 || !strings.Contains(w.Body.String(), "验证码不正确") || b.cookie("dba_session") != nil {
		t.Fatalf("错误验证码: %d", w.Code)
	}
	// 正确验证码 → 建立会话，跳转到最初的 next，中间态清除
	code6 := totpCode(t, secret, he.clk.Now())
	w = b.do("POST", "/login/2fa", url.Values{"method": {"totp"}, "action": {"verify"}, "code": {code6}, "csrf": {csrf}}, nil)
	if w.Code != 303 || w.Header().Get("Location") != "/sqlworkflow" || b.cookie("dba_session") == nil || b.cookie("dba_2fa") != nil {
		t.Fatalf("验证成功: %d %s", w.Code, w.Header().Get("Location"))
	}
	if code, _ := b.json("GET", "/api/v1/auth/2fa", nil); code != 200 {
		t.Error("会话应可用")
	}
	// 同一个口令不能再登录一次（防重放）
	b3 := he.browser()
	b3.postLogin("bob", pw, "/query")
	_, _, csrf3 := he.twofaPage(b3, "")
	if w := b3.do("POST", "/login/2fa", url.Values{"method": {"totp"}, "action": {"verify"}, "code": {code6}, "csrf": {csrf3}}, nil); w.Code != 401 {
		t.Errorf("口令重放: %d", w.Code)
	}
	// 没有中间态直接访问 → 回登录页
	b4 := he.browser()
	if w := b4.get("/login/2fa"); w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Errorf("没有中间态: %d", w.Code)
	}
	if w := b4.do("POST", "/login/2fa", url.Values{"code": {"123456"}}, nil); w.Code != 302 {
		t.Errorf("没有中间态 POST: %d", w.Code)
	}
}

func TestTwoFALockoutHTTP(t *testing.T) {
	he := newHTTPEnv(t)
	secret := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	he.repo2().tfa[2] = []TwoFAConfig{{Type: "totp", Secret: secret}}
	b := he.browser()
	b.postLogin("bob", pw, "/query")
	_, _, csrf := he.twofaPage(b, "")
	var last *httptest.ResponseRecorder
	for i := 0; i < 5; i++ {
		last = b.do("POST", "/login/2fa", url.Values{"method": {"totp"}, "action": {"verify"}, "code": {"000000"}, "csrf": {csrf}}, nil)
	}
	if last.Code != 302 || !strings.Contains(last.Header().Get("Location"), "err=2fa_locked") || b.cookie("dba_2fa") != nil {
		t.Errorf("连续失败应作废这次登录: %d %s", last.Code, last.Header().Get("Location"))
	}
	if !strings.Contains(b.get(last.Header().Get("Location")).Body.String(), "验证失败次数过多") {
		t.Error("登录页应提示")
	}
}

func TestEnforcedBindTOTPHTTP(t *testing.T) {
	he := newHTTPEnv(t)
	he.repoCfgSet(cfgEnforce2FA, "true")
	b := he.browser()
	w := b.postLogin("bob", pw, "/query")
	if w.Header().Get("Location") != "/login/2fa" {
		t.Fatalf("强制 2FA 且未绑定应跳到绑定: %d", w.Code)
	}
	code, page, csrf := he.twofaPage(b, "")
	if code != 200 || !strings.Contains(page, "先绑定") || !strings.Contains(page, `src="data:image/png;base64,`) || !strings.Contains(page, "tfa-secret") {
		t.Fatalf("绑定页应有二维码与密钥文本: %d", code)
	}
	secret := regexp.MustCompile(`<code class="tfa-secret">([A-Z2-7]+)</code>`).FindStringSubmatch(page)[1]
	// 刷新页面不换密钥
	if _, page2, _ := he.twofaPage(b, ""); !strings.Contains(page2, secret) {
		t.Error("刷新绑定页不应换密钥")
	}
	if w := b.do("POST", "/login/2fa", url.Values{"method": {"totp"}, "action": {"verify"}, "code": {"000000"}, "csrf": {csrf}}, nil); w.Code != 401 || len(he.repo2().tfa[2]) != 0 {
		t.Errorf("错误验证码不能绑定: %d", w.Code)
	}
	w = b.do("POST", "/login/2fa", url.Values{"method": {"totp"}, "action": {"verify"}, "code": {totpCode(t, secret, he.clk.Now())}, "csrf": {csrf}}, nil)
	if w.Code != 303 || b.cookie("dba_session") == nil {
		t.Fatalf("绑定并登录: %d", w.Code)
	}
	if got := he.repo2().tfa[2]; len(got) != 1 || got[0].Secret != secret {
		t.Errorf("应保存绑定: %+v", got)
	}
}

func TestBindSMSHTTP(t *testing.T) {
	he := newHTTPEnv(t)
	he.repoCfgSet(cfgEnforce2FA, "true")
	he.repoCfgSet(cfgSMSProvider, SMSAliyun)
	b := he.browser()
	b.postLogin("bob", pw, "/query")
	_, page, csrf := he.twofaPage(b, "?m=sms")
	if !strings.Contains(page, "获取验证码") || !strings.Contains(page, `name="phone"`) || !strings.Contains(page, `href="/login/2fa?m=totp"`) {
		t.Fatalf("短信绑定页")
	}
	w := b.do("POST", "/login/2fa", url.Values{"method": {"sms"}, "action": {"send"}, "phone": {"13800000000"}, "csrf": {csrf}}, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "验证码已发送") {
		t.Fatalf("发送: %d", w.Code)
	}
	_, code := he.sms.last()
	// 60 秒限频
	w = b.do("POST", "/login/2fa", url.Values{"method": {"sms"}, "action": {"send"}, "phone": {"13800000000"}, "csrf": {csrf}}, nil)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "太频繁") {
		t.Errorf("限频: %d", w.Code)
	}
	w = b.do("POST", "/login/2fa", url.Values{"method": {"sms"}, "action": {"verify"}, "phone": {"13800000000"}, "code": {code}, "csrf": {csrf}}, nil)
	if w.Code != 303 || b.cookie("dba_session") == nil || len(he.repo2().tfa[2]) != 1 || he.repo2().tfa[2][0].Phone != "13800000000" {
		t.Fatalf("短信绑定并登录: %d", w.Code)
	}
	// 之后登录：验证模式，号码脱敏，手机号不可由用户指定
	b2 := he.browser()
	b2.postLogin("bob", pw, "/query")
	_, page, csrf = he.twofaPage(b2, "")
	if !strings.Contains(page, "138****0000") || strings.Contains(page, `name="phone"`) {
		t.Error("验证模式应显示脱敏号码且不让改号码")
	}
	he.clk.Add(2 * time.Minute)
	b2.do("POST", "/login/2fa", url.Values{"method": {"sms"}, "action": {"send"}, "phone": {"13999999999"}, "csrf": {csrf}}, nil)
	if phone, _ := he.sms.last(); phone != "13800000000" {
		t.Errorf("验证码必须发往已绑定的号码: %s", phone)
	}
}

// ---- SSO ----

func TestSSOHTTPFlow(t *testing.T) {
	he := newHTTPEnv(t)
	idp := newFakeOIDC(t, he.clk)
	p := he.addOIDC(idp, FirstLoginCreate)
	b := he.browser()
	// 发起：只允许站内 next
	w := b.get("/login/sso/" + itoa(p.ID) + "?next=https%3A%2F%2Fevil.example.com")
	if w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), idp.srv.URL+"/authorize?") {
		t.Fatalf("发起: %d %s", w.Code, w.Header().Get("Location"))
	}
	ck := b.cookie("dba_sso_" + itoa(p.ID))
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/login/sso" || ck.MaxAge != 600 {
		t.Fatalf("状态 Cookie 属性: %+v", ck)
	}
	if strings.Contains(w.Header().Get("Cache-Control"), "public") || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Error("不能缓存")
	}
	q := idp.authorize(w.Header().Get("Location"))
	cb := "/login/sso/" + itoa(p.ID) + "/callback?" + q.Encode()
	// Cookie 作用域：只有 /login/sso 下才发送
	if w := b.get("/query"); strings.Contains(w.Header().Get("Set-Cookie"), "dba_sso") {
		t.Error("无关")
	}
	w = b.get(cb)
	if w.Code != 302 || w.Header().Get("Location") != "/" || b.cookie("dba_session") == nil {
		t.Fatalf("回调应登录并回到首页 /（evil next 已被拦截）: %d %s", w.Code, w.Header().Get("Location"))
	}
	if b.cookie("dba_sso_"+itoa(p.ID)) != nil {
		t.Error("状态 Cookie 一次性，应清除")
	}
	if b.session().User != "zhang.san" || b.session().Su {
		t.Errorf("会话: %+v", b.session())
	}
	// 回调重放：攻击者拿着同一个回调链接 + 原 Cookie
	b2 := he.browser()
	b2.cookies[ck.Name] = ck
	w = b2.get(cb)
	if w.Code != 302 || !strings.Contains(w.Header().Get("Location"), "err=state") || b2.cookie("dba_session") != nil {
		t.Errorf("重放应拒绝: %d %s", w.Code, w.Header().Get("Location"))
	}
	// 没有状态 Cookie（登录 CSRF）
	st := idp.authorize(he.browser().get("/login/sso/" + itoa(p.ID)).Header().Get("Location"))
	b3 := he.browser()
	w = b3.get("/login/sso/" + itoa(p.ID) + "/callback?" + st.Encode())
	if !strings.Contains(w.Header().Get("Location"), "err=state") || b3.cookie("dba_session") != nil {
		t.Error("没有状态 Cookie 应拒绝")
	}
	// next 为站内路径被保留
	b4 := he.browser()
	w = b4.get("/login/sso/" + itoa(p.ID) + "?next=%2Fsqlworkflow%2Fnew")
	w = b4.get("/login/sso/" + itoa(p.ID) + "/callback?" + idp.authorize(w.Header().Get("Location")).Encode())
	if w.Header().Get("Location") != "/sqlworkflow/new" {
		t.Errorf("站内 next: %s", w.Header().Get("Location"))
	}
	// 已登录访问发起地址：直接回 next，不再跳 IdP
	if w := b4.get("/login/sso/" + itoa(p.ID)); w.Code != 302 || strings.HasPrefix(w.Header().Get("Location"), idp.srv.URL) {
		t.Errorf("已登录: %s", w.Header().Get("Location"))
	}
	// 未启用 / 不存在的登录方式
	if w := he.browser().get("/login/sso/999"); !strings.Contains(w.Header().Get("Location"), "err=provider_off") {
		t.Errorf("不存在: %s", w.Header().Get("Location"))
	}
	if w := he.browser().get("/login/sso/abc"); w.Code != 302 {
		t.Errorf("非法 ID: %d", w.Code)
	}
	// IdP 返回错误
	idp.tokenErr = true
	b5 := he.browser()
	w = b5.get("/login/sso/" + itoa(p.ID))
	w = b5.get("/login/sso/" + itoa(p.ID) + "/callback?" + idp.authorize(w.Header().Get("Location")).Encode())
	if !strings.Contains(w.Header().Get("Location"), "err=failed") || b5.cookie("dba_session") != nil {
		t.Errorf("IdP 失败: %s", w.Header().Get("Location"))
	}
	body := b5.get(w.Header().Get("Location")).Body.String()
	if !strings.Contains(body, "登录失败，请重试") || strings.Contains(body, "invalid_grant") {
		t.Error("登录页只显示通用文案")
	}
}

func TestSSOExternal2FAHTTP(t *testing.T) {
	he := newHTTPEnv(t)
	idp := newFakeOIDC(t, he.clk)
	p := he.addOIDC(idp, FirstLoginExisting)
	he.repo2().addUser(30, "zhang.san", pw, false)
	he.repo2().tfa[30] = []TwoFAConfig{{Type: "totp", Secret: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"}}
	login := func() (*browser, string) {
		b := he.browser()
		w := b.get("/login/sso/" + itoa(p.ID))
		w = b.get("/login/sso/" + itoa(p.ID) + "/callback?" + idp.authorize(w.Header().Get("Location")).Encode())
		return b, w.Header().Get("Location")
	}
	if b, loc := login(); loc != "/" || b.cookie("dba_session") == nil {
		t.Errorf("默认 SSO 登录不再要求两步验证（认为 IdP 已验证）: %s", loc)
	}
	he.kvSet(settingExt2FA, "true")
	if b, loc := login(); loc != "/login/2fa" || b.cookie("dba_session") != nil || b.cookie("dba_2fa") == nil {
		t.Errorf("开启后 SSO 登录也要两步验证: %s", loc)
	}
}

func TestSSOTestLoginRequiresSuperuser(t *testing.T) {
	he := newHTTPEnv(t)
	idp := newFakeOIDC(t, he.clk)
	p := he.addOIDC(idp, FirstLoginCreate)
	if w := he.browser().get("/login/sso/" + itoa(p.ID) + "?test=1"); w.Code != 403 {
		t.Errorf("未登录不能测试登录: %d", w.Code)
	}
	if w := he.loginAs("bob").get("/login/sso/" + itoa(p.ID) + "?test=1"); w.Code != 403 {
		t.Errorf("普通用户不能测试登录: %d", w.Code)
	}
}

// ---- 注册 ----

func TestSignUpHTTP(t *testing.T) {
	he := newHTTPEnv(t)
	b := he.browser()
	form := func(csrf string) url.Values {
		return url.Values{"username": {"newguy"}, "password": {"correct-horse-9"}, "password2": {"correct-horse-9"}, "display": {"新人"}, "email": {"n@example.com"}, "csrf": {csrf}}
	}
	// 每次失败的响应都是一个新渲染的登录页（带新的 CSRF），所以每次提交前重新取
	post := func(mut func(url.Values)) *httptest.ResponseRecorder {
		f := form(b.loginCSRF("/login"))
		if mut != nil {
			mut(f)
		}
		return b.do("POST", "/login/signup", f, nil)
	}
	if w := post(nil); w.Code != 403 || !strings.Contains(w.Body.String(), "注册未启用") {
		t.Errorf("未开放: %d", w.Code)
	}
	he.repoCfgSet(cfgSignUp, "true")
	if w := post(func(f url.Values) { f.Set("csrf", "forged") }); w.Code != 403 || !strings.Contains(w.Body.String(), "页面已过期") {
		t.Errorf("CSRF: %d", w.Code)
	}
	if w := post(func(f url.Values) { f.Set("password2", "different") }); w.Code != 400 || !strings.Contains(w.Body.String(), "两次输入密码不一致") {
		t.Errorf("校验: %d", w.Code)
	}
	w := post(nil)
	if w.Code != 303 || w.Header().Get("Location") != "/login?registered=1" {
		t.Fatalf("注册: %d %s", w.Code, w.Body.String())
	}
	if w := he.browser().postLogin("newguy", "correct-horse-9", "/query"); w.Code != 303 {
		t.Errorf("注册后登录: %d", w.Code)
	}
	if w := post(nil); w.Code != 400 || !strings.Contains(w.Body.String(), "用户名已存在") {
		t.Errorf("重名: %d", w.Code)
	}
	// 限频：同一 IP 每小时最多 10 次尝试
	var got int
	for i := 0; i < 12; i++ {
		got = post(nil).Code
	}
	if got != 429 {
		t.Errorf("注册限频: %d", got)
	}
}

// ---- 管理接口 ----

func TestAdminAPIAccessControl(t *testing.T) {
	he := newHTTPEnv(t)
	anon := he.browser()
	for _, p := range []string{"/api/v1/settings/auth-providers", "/api/v1/settings/security"} {
		if code, _ := anon.json("GET", p, nil); code != 401 {
			t.Errorf("%s 未登录: %d", p, code)
		}
	}
	bob := he.loginAs("bob")
	for _, c := range []struct{ m, p string }{{"GET", "/api/v1/settings/auth-providers"}, {"POST", "/api/v1/settings/auth-providers"}, {"PUT", "/api/v1/settings/auth-providers/1"},
		{"DELETE", "/api/v1/settings/auth-providers/1"}, {"POST", "/api/v1/settings/auth-providers/1/enable"}, {"POST", "/api/v1/settings/auth-providers/1/test"},
		{"PUT", "/api/v1/settings/auth-order"}, {"PUT", "/api/v1/settings/auth-rules"}, {"GET", "/api/v1/settings/security"}, {"PUT", "/api/v1/settings/security"},
		{"POST", "/api/v1/settings/security/sms-test"}} {
		if code, _ := bob.json(c.m, c.p, map[string]any{}); code != 403 {
			t.Errorf("%s %s 普通用户应 403: %d", c.m, c.p, code)
		}
	}
	for _, p := range []string{"/settings/auth-providers", "/settings/security"} {
		if w := bob.get(p); w.Code != 403 {
			t.Errorf("页面 %s 普通用户应 403: %d", p, w.Code)
		}
		if w := anon.get(p); w.Code != 302 {
			t.Errorf("页面 %s 未登录应跳转: %d", p, w.Code)
		}
	}
	// CSRF：会话写操作必须带令牌
	root := he.loginAs("root")
	req := httptest.NewRequest("POST", "/api/v1/settings/auth-providers", strings.NewReader(`{"kind":"oidc","name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	root.attach(req)
	w := httptest.NewRecorder()
	he.h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Errorf("缺 CSRF 头应 403: %d", w.Code)
	}
}

func TestAdminProvidersCRUD(t *testing.T) {
	he := newHTTPEnv(t)
	idp := newFakeOIDC(t, he.clk)
	root := he.loginAs("root")
	data := func(out map[string]any) map[string]any { d, _ := out["data"].(map[string]any); return d }

	code, out := root.json("POST", "/api/v1/settings/auth-providers", map[string]any{"kind": "oidc", "name": "公司 SSO"})
	if code != 200 {
		t.Fatalf("创建: %d %v", code, out)
	}
	id := uint(data(out)["id"].(float64))
	ids := itoa(id)
	// lark 创建为飞书国际站点
	code, out = root.json("POST", "/api/v1/settings/auth-providers", map[string]any{"kind": "lark", "name": "Lark"})
	if code != 200 || data(out)["kind"] != "feishu" || data(out)["kind_label"] != "Lark" || data(out)["config"].(map[string]any)["site"] != "lark" {
		t.Errorf("Lark: %d %v", code, out)
	}
	if code, _ = root.json("POST", "/api/v1/settings/auth-providers", map[string]any{"kind": "nope", "name": "x"}); code != 400 {
		t.Errorf("未知类型: %d", code)
	}
	// 启用前必须测试通过
	if code, out = root.json("POST", "/api/v1/settings/auth-providers/"+ids+"/enable", nil); code != 400 || !strings.Contains(out["message"].(string), "测试通过") {
		t.Errorf("未测试不能启用: %d %v", code, out)
	}
	// 保存配置（含密钥），回显里没有密钥
	cfg := map[string]any{"wellknown_url": idp.srv.URL + "/.well-known/openid-configuration", "client_id": "dbtool", "client_secret": "s3cret", "scopes": "openid profile email", "sign_alg": "RS256"}
	code, out = root.json("PUT", "/api/v1/settings/auth-providers/"+ids, map[string]any{"name": "公司 SSO", "btn_label": "以公司账号登录", "first_login": "create", "config": cfg})
	if code != 200 {
		t.Fatalf("保存: %d %v", code, out)
	}
	if strings.Contains(root.last.Body.String(), "s3cret") {
		t.Fatal("响应不能回显密钥")
	}
	code, out = root.json("GET", "/api/v1/settings/auth-providers/"+ids, nil)
	if strings.Contains(root.last.Body.String(), "s3cret") || data(out)["has_secrets"].(map[string]any)["client_secret"] != true {
		t.Errorf("GET 不回显密钥: %s", root.last.Body.String())
	}
	// 测试 → 启用
	code, out = root.json("POST", "/api/v1/settings/auth-providers/"+ids+"/test", map[string]any{})
	if code != 200 || data(out)["report"].(map[string]any)["ok"] != true {
		t.Fatalf("测试: %d %v", code, out)
	}
	if code, out = root.json("POST", "/api/v1/settings/auth-providers/"+ids+"/enable", nil); code != 200 {
		t.Fatalf("启用: %d %v", code, out)
	}
	_, out = root.json("GET", "/api/v1/settings/auth-providers", nil)
	d := data(out)
	if len(d["providers"].([]any)) != 2 || len(d["kinds"].([]any)) != 8 || d["rules"] == nil || d["callback_base"] != "http://dba.test" {
		t.Errorf("概览: %v", d)
	}
	first := d["providers"].([]any)[0].(map[string]any)
	if first["status"] != "enabled" || first["callback_url"] != "http://dba.test/login/sso/"+ids+"/callback" {
		t.Errorf("状态/回调地址: %v", first)
	}
	// 登录页出现按钮
	if !strings.Contains(he.browser().get("/login").Body.String(), "以公司账号登录") {
		t.Error("启用后登录页应显示按钮")
	}
	// 改连接配置 → 自动停用并提示
	cfg["client_id"] = "other"
	code, out = root.json("PUT", "/api/v1/settings/auth-providers/"+ids, map[string]any{"name": "公司 SSO", "first_login": "create", "config": cfg})
	if code != 200 || !strings.Contains(data(out)["notice"].(string), "自动停用") || data(out)["provider"].(map[string]any)["status"] != "disabled" {
		t.Errorf("变更后应停用: %d %v", code, out)
	}
	if strings.Contains(he.browser().get("/login").Body.String(), "以公司账号登录") {
		t.Error("停用后登录页不应显示")
	}
	// 排序
	_, out = root.json("GET", "/api/v1/settings/auth-providers", nil)
	var order []uint
	for _, p := range data(out)["providers"].([]any) {
		order = append([]uint{uint(p.(map[string]any)["id"].(float64))}, order...)
	}
	if code, _ = root.json("PUT", "/api/v1/settings/auth-order", map[string]any{"ids": order}); code != 200 {
		t.Errorf("排序: %d", code)
	}
	if code, _ = root.json("PUT", "/api/v1/settings/auth-order", map[string]any{"ids": []uint{}}); code != 400 {
		t.Errorf("空排序: %d", code)
	}
	// 通用规则
	code, out = root.json("PUT", "/api/v1/settings/auth-rules", map[string]any{"local_form_mode": "always", "match_by": []string{"username"}, "superuser_local_only": true, "external_2fa": true, "backfill_ids": true})
	if code != 200 || !he.svc.Settings().External2FA || he.svc.Settings().LocalFormMode != FormAlways {
		t.Errorf("规则: %d %v", code, out)
	}
	if code, _ = root.json("PUT", "/api/v1/settings/auth-rules", map[string]any{"local_form_mode": "bogus"}); code != 400 {
		t.Errorf("规则校验: %d", code)
	}
	// 删除：先停用（已停用）→ 有绑定要确认
	he.repo2().BindIdentity(id, "s1", 2)
	code, out = root.json("DELETE", "/api/v1/settings/auth-providers/"+ids, nil)
	if code != 409 || out["code"] != "in_use" {
		t.Fatalf("有绑定用户要确认: %d %v", code, out)
	}
	if code, _ = root.json("DELETE", "/api/v1/settings/auth-providers/"+ids+"?confirm=1", nil); code != 200 {
		t.Errorf("确认后删除: %d", code)
	}
	if code, _ = root.json("GET", "/api/v1/settings/auth-providers/"+ids, nil); code != 404 {
		t.Errorf("已删除: %d", code)
	}
	if code, _ = root.json("GET", "/api/v1/settings/auth-providers/abc", nil); code != 400 {
		t.Errorf("非法 ID: %d", code)
	}
}

func TestAdminSecurityAPI(t *testing.T) {
	he := newHTTPEnv(t)
	root := he.loginAs("root")
	code, out := root.json("GET", "/api/v1/settings/security", nil)
	d := out["data"].(map[string]any)
	if code != 200 || d["settings"].(map[string]any)["lock_cnt_threshold"].(float64) != 5 || len(d["users"].([]any)) < 2 || len(d["auth_groups"].([]any)) != 3 {
		t.Fatalf("读取: %d %v", code, out)
	}
	s := d["settings"].(map[string]any)
	s["lock_cnt_threshold"], s["lock_time_threshold"] = 2, 60
	s["session_minutes"], s["jwt_hours"] = 45, 2
	s["password_min_len"], s["password_complexity"] = 12, true
	s["api_user_whitelist"] = []uint{1}
	s["default_auth_group"] = []string{"RD"}
	s["sms_provider"], s["aliyun_access_key_id"], s["aliyun_access_key_secret"] = "aliyun", "LTAI1", "ALI-SECRET-VALUE"
	s["aliyun_sign_name"], s["aliyun_template_code"] = "签名", "SMS_1"
	code, out = root.json("PUT", "/api/v1/settings/security", s)
	if code != 200 {
		t.Fatalf("保存: %d %v", code, out)
	}
	if strings.Contains(root.last.Body.String(), "ALI-SECRET-VALUE") {
		t.Fatal("响应不能回显短信密钥")
	}
	set := he.svc.Settings()
	if set.LockCount != 2 || set.SessionMinutes != 45 || set.TokenTTL() != 2*time.Hour || set.SMS.AliyunKeySecret != "ALI-SECRET-VALUE" || !set.Policy().Complexity {
		t.Errorf("应立即生效: %+v", set)
	}
	// 立即生效：下一次登录就按新会话有效期；新的锁定阈值
	b := he.browser()
	b.postLogin("bob", pw, "/query")
	if d := time.Until(time.Unix(b.session().Exp, 0)); d > 46*time.Minute {
		t.Errorf("会话有效期未生效: %v", d)
	}
	he.browser().postLogin("bob", "wrong-password-xyz", "/query")
	he.browser().postLogin("bob", "wrong-password-xyz", "/query")
	if w := he.browser().postLogin("bob", pw, "/query"); w.Code != 401 {
		t.Errorf("新锁定阈值（2 次）未生效: %d", w.Code)
	}
	// 再次保存（密钥留空）保持，且读取不含密钥
	s["aliyun_access_key_secret"] = ""
	s["lock_cnt_threshold"] = 5
	if code, _ = root.json("PUT", "/api/v1/settings/security", s); code != 200 || he.svc.Settings().SMS.AliyunKeySecret != "ALI-SECRET-VALUE" {
		t.Errorf("密钥留空应保持: %d", code)
	}
	code, out = root.json("GET", "/api/v1/settings/security", nil)
	if strings.Contains(root.last.Body.String(), "ALI-SECRET-VALUE") || out["data"].(map[string]any)["settings"].(map[string]any)["has_secrets"].(map[string]any)["aliyun_access_key_secret"] != true {
		t.Errorf("读取不回显: %s", root.last.Body.String())
	}
	// 校验
	s["jwt_hours"] = 0
	if code, _ = root.json("PUT", "/api/v1/settings/security", s); code != 400 {
		t.Errorf("校验: %d", code)
	}
	// 审计不含密钥
	for _, a := range he.repo2().audits {
		if strings.Contains(a.ExtraInfo, "ALI-SECRET-VALUE") {
			t.Fatalf("审计不得含密钥: %+v", a)
		}
	}
	if !strings.Contains(strings.Join(he.repo2().auditActions(), ","), "安全设置-修改") {
		t.Error("应写审计")
	}
	// 短信测试
	if code, _ = root.json("POST", "/api/v1/settings/security/sms-test", map[string]any{"phone": "bad"}); code != 400 {
		t.Errorf("号码: %d", code)
	}
	if code, _ = root.json("POST", "/api/v1/settings/security/sms-test", map[string]any{"phone": "13800000000"}); code != 200 {
		t.Errorf("短信测试: %d", code)
	}
	if phone, _ := he.sms.last(); phone != "13800000000" {
		t.Error("应发出测试短信")
	}
}

func TestSelfServiceTwoFAAPI(t *testing.T) {
	he := newHTTPEnv(t)
	he.repoCfgSet(cfgSMSProvider, SMSAliyun)
	bob := he.loginAs("bob")
	code, out := bob.json("GET", "/api/v1/auth/2fa", nil)
	if code != 200 || out["data"].(map[string]any)["bound"].(map[string]any)["totp"] != false || out["data"].(map[string]any)["sms_available"] != true {
		t.Fatalf("状态: %d %v", code, out)
	}
	code, out = bob.json("POST", "/api/v1/auth/2fa/totp", nil)
	secret, _ := out["data"].(map[string]any)["secret"].(string)
	if code != 200 || len(secret) != 32 || bob.last.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("开始绑定: %d", code)
	}
	if code, _ = bob.json("POST", "/api/v1/auth/2fa/totp/confirm", map[string]any{"code": "000000"}); code != 401 {
		t.Errorf("错误口令: %d", code)
	}
	if code, _ = bob.json("POST", "/api/v1/auth/2fa/totp/confirm", map[string]any{"code": totpCode(t, secret, he.clk.Now())}); code != 200 {
		t.Fatalf("确认: %d", code)
	}
	if code, out = bob.json("GET", "/api/v1/auth/2fa", nil); out["data"].(map[string]any)["bound"].(map[string]any)["totp"] != true {
		t.Errorf("应已绑定: %v", out)
	}
	if code, _ = bob.json("POST", "/api/v1/auth/2fa/sms/send", map[string]any{"phone": "13800000002"}); code != 200 {
		t.Fatalf("短信发送: %d", code)
	}
	_, c := he.sms.last()
	he.svc.twofa.reset(2)
	if code, _ = bob.json("POST", "/api/v1/auth/2fa/sms/confirm", map[string]any{"phone": "13800000002", "code": c}); code != 200 {
		t.Fatalf("短信确认: %d", code)
	}
	if code, _ = bob.json("DELETE", "/api/v1/auth/2fa/totp", nil); code != 200 {
		t.Errorf("解绑: %d", code)
	}
	if code, _ = bob.json("DELETE", "/api/v1/auth/2fa/email", nil); code != 400 {
		t.Errorf("未知方式: %d", code)
	}
	if code, out = bob.json("GET", "/api/v1/auth/ui", nil); code != 200 || out["data"].(map[string]any)["watermark"] != false {
		t.Errorf("ui: %d %v", code, out)
	}
	if code, _ = he.browser().json("GET", "/api/v1/auth/2fa", nil); code != 401 {
		t.Errorf("未登录: %d", code)
	}
}

func TestMenuVisibility(t *testing.T) {
	he := newHTTPEnv(t)
	rootPage := he.loginAs("root").get("/settings/auth-providers")
	if rootPage.Code != 200 || !strings.Contains(rootPage.Body.String(), "登录认证") || !strings.Contains(rootPage.Body.String(), "安全策略") {
		t.Errorf("超级用户应看到菜单与页面: %d", rootPage.Code)
	}
	secPage := he.loginAs("root").get("/settings/security")
	if secPage.Code != 200 || !strings.Contains(secPage.Body.String(), "安全策略") {
		t.Errorf("安全设置页: %d", secPage.Code)
	}
	if body := he.loginAs("bob").get("/query").Body.String(); strings.Contains(body, "/settings/auth-providers") || strings.Contains(body, "/settings/security") {
		t.Error("普通用户看不到菜单")
	}
}

func TestCheckPasswordExport(t *testing.T) {
	he := newHTTPEnv(t)
	if err := CheckPassword(he.srv, "zhangsan", "abcdefgh1"); err != nil {
		t.Errorf("默认策略（最小长度 9，不要求复杂度）: %v", err)
	}
	he.kvSet(settingPwdMinLen, "12")
	he.kvSet(settingPwdCplx, "true")
	if err := CheckPassword(he.srv, "zhangsan", "abcdefgh1"); err == nil {
		t.Error("策略变更应立即体现")
	}
	if err := CheckPassword(he.srv, "zhangsan", "abcdefgh-1234", "张三"); err != nil {
		t.Errorf("合格密码: %v", err)
	}
	if err := CheckPassword(nil, "zhangsan", "short"); err == nil {
		t.Error("没有 authx 时仍使用默认策略")
	}
}

func TestLDAPLoginRequiresTwoFAHTTP(t *testing.T) {
	he := newHTTPEnv(t)
	he.addLDAP(FirstLoginCreate)
	he.dir.users["ldapuser"] = fakeDirUser{password: "dir-pass-1", dn: "cn=ldapuser"}
	// 先走一次让用户被创建，然后给他绑定 TOTP
	if w := he.browser().postLogin("ldapuser", "dir-pass-1", "/query"); w.Code != 303 {
		t.Fatalf("LDAP 登录: %d", w.Code)
	}
	u, _ := he.repo2().UserByName("ldapuser")
	he.repo2().tfa[u.ID] = []TwoFAConfig{{Type: "totp", Secret: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"}}
	b := he.browser()
	w := b.postLogin("ldapuser", "dir-pass-1", "/query")
	if w.Header().Get("Location") != "/login/2fa" || b.cookie("dba_session") != nil {
		t.Errorf("LDAP 登录同样受两步验证约束: %d %s", w.Code, w.Header().Get("Location"))
	}
}
