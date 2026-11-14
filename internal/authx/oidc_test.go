// @author: kevinkelly
// @license: Apache Licence
// @file: oidc_test.go
// @time: 2026/10/28

package authx

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fakeOIDC 是一个最小的 OIDC IdP：discovery、JWKS、token（校验 PKCE 与客户端凭据）、userinfo。
type fakeOIDC struct {
	t      *testing.T
	srv    *httptest.Server
	rsaKey *rsa.PrivateKey
	ecKey  *ecdsa.PrivateKey
	clk    *fakeClock

	mu       sync.Mutex
	codes    map[string]idpCode
	useES    bool
	claims   map[string]any // 覆盖 / 追加的 id_token 声明
	mutate   func(map[string]any)
	signWith func(claims jwt.MapClaims) (string, error) // 自定义签名（攻击场景）
	userinfo map[string]any
	jwksHits int
	tokenErr bool
	issuer   string // 非空时覆盖 issuer（Slack 的 issuer 固定为 https://slack.com）
}

func (f *fakeOIDC) iss() string {
	if f.issuer != "" {
		return f.issuer
	}
	return f.srv.URL
}

type idpCode struct {
	challenge, nonce, clientID, redirect string
}

var (
	testRSAOnce sync.Once
	testRSA     *rsa.PrivateKey
)

func sharedRSA(t *testing.T) *rsa.PrivateKey {
	testRSAOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		testRSA = k
	})
	return testRSA
}

func newFakeOIDC(t *testing.T, clk *fakeClock) *fakeOIDC {
	f := &fakeOIDC{t: t, clk: clk, codes: map[string]idpCode{}, rsaKey: sharedRSA(t), userinfo: map[string]any{}}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.ecKey = ec
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": f.iss(), "authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint": f.srv.URL + "/token", "jwks_uri": f.srv.URL + "/jwks", "userinfo_endpoint": f.srv.URL + "/userinfo"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.jwksHits++
		f.mu.Unlock()
		b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
		pub := f.rsaKey.PublicKey
		ex := big.NewInt(int64(pub.E)).Bytes()
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{
			{"kty": "RSA", "kid": "rsa1", "use": "sig", "alg": "RS256", "n": b64(pub.N.Bytes()), "e": b64(ex)},
			{"kty": "EC", "kid": "ec1", "use": "sig", "alg": "ES256", "crv": "P-256", "x": b64(f.ecKey.X.FillBytes(make([]byte, 32))), "y": b64(f.ecKey.Y.FillBytes(make([]byte, 32)))},
		}})
	})
	mux.HandleFunc("/token", f.token)
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-123" {
			w.WriteHeader(401)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.userinfo)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOIDC) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ParseForm()
	user, pass, hasBasic := r.BasicAuth()
	cid, sec := r.Form.Get("client_id"), r.Form.Get("client_secret")
	if hasBasic {
		cid, _ = url.QueryUnescape(user)
		sec, _ = url.QueryUnescape(pass)
	}
	c, ok := f.codes[r.Form.Get("code")]
	delete(f.codes, r.Form.Get("code")) // 授权码一次性
	fail := func(code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": code})
	}
	if f.tokenErr || !ok || r.Form.Get("grant_type") != "authorization_code" || cid != "dbtool" || sec != "s3cret" || r.Form.Get("redirect_uri") != c.redirect {
		fail("invalid_grant")
		return
	}
	h := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(h[:]) != c.challenge {
		fail("invalid_grant") // PKCE 校验
		return
	}
	now := f.clk.Now()
	claims := jwt.MapClaims{"iss": f.iss(), "sub": "sub-001", "aud": "dbtool", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nonce": c.nonce,
		"preferred_username": "zhang.san", "name": "张三", "email": "zhang.san@example.com", "email_verified": true}
	for k, v := range f.claims {
		claims[k] = v
	}
	if f.mutate != nil {
		m := map[string]any(claims)
		f.mutate(m)
	}
	var tok string
	var err error
	switch {
	case f.signWith != nil:
		tok, err = f.signWith(claims)
	case f.useES:
		t := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
		t.Header["kid"] = "ec1"
		tok, err = t.SignedString(f.ecKey)
	default:
		t := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		t.Header["kid"] = "rsa1"
		tok, err = t.SignedString(f.rsaKey)
	}
	if err != nil {
		f.t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"access_token": "at-123", "token_type": "Bearer", "expires_in": 3600, "id_token": tok})
}

// authorize 模拟用户在 IdP 登录授权：解析授权地址，返回要回跳的查询参数。
func (f *fakeOIDC) authorize(authURL string) url.Values {
	f.t.Helper()
	u, err := url.Parse(authURL)
	if err != nil || !strings.HasPrefix(authURL, f.srv.URL+"/authorize") {
		f.t.Fatalf("授权地址不对: %s", authURL)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("nonce") == "" {
		f.t.Fatalf("授权请求缺少 state / nonce / PKCE: %s", u.RawQuery)
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		f.t.Fatalf("scope: %s", q.Get("scope"))
	}
	code := randToken(10)
	f.mu.Lock()
	f.codes[code] = idpCode{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), clientID: q.Get("client_id"), redirect: q.Get("redirect_uri")}
	f.mu.Unlock()
	return url.Values{"code": {code}, "state": {q.Get("state")}}
}

// ---- 测试 ----

func (e *testEnv) addOIDC(idp *fakeOIDC, first string) *Provider {
	e.t.Helper()
	p, err := e.svc.CreateProvider(rootActor, KindOIDC, "公司 SSO")
	if err != nil {
		e.t.Fatal(err)
	}
	cfg := p.Config
	cfg.WellKnownURL, cfg.ClientID, cfg.ClientSecret = idp.srv.URL+"/.well-known/openid-configuration", "dbtool", "s3cret"
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: p.Name, FirstLogin: first, BtnLabel: "以公司账号登录", Config: cfg}); err != nil {
		e.t.Fatal(err)
	}
	return e.enable(p.ID)
}

// sso 走一遍 SSO：发起 → IdP 授权 → 回调。
func (e *testEnv) sso(idp *fakeOIDC, p *Provider) (*SSOResult, error) {
	e.t.Helper()
	st, err := e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{Next: "/query"})
	if err != nil {
		return nil, err
	}
	q := idp.authorize(st.URL)
	return e.svc.CompleteSSO(context.Background(), p.ID, q, st.CookieValue, 0, "")
}

func TestOIDCHappyPath(t *testing.T) {
	e := newTestEnv(t)
	idp := newFakeOIDC(t, e.clk)
	p := e.addOIDC(idp, FirstLoginCreate)
	rep, _ := e.repo.GetProvider(p.ID)
	if rep.TestOK == nil || !*rep.TestOK || !strings.Contains(rep.TestMsg, "discovery 正常") {
		t.Fatalf("测试连接结果: %+v", rep)
	}
	res, err := e.sso(idp, p)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Auth.Created || res.Auth.User.Username != "zhang.san" || res.Auth.User.Email != "zhang.san@example.com" || res.Next != "/query" {
		t.Fatalf("%+v", res.Auth)
	}
	// 第二次：绑定后即使用户名变了也认 sub
	idp.claims = map[string]any{"preferred_username": "zs-renamed"}
	res, err = e.sso(idp, p)
	if err != nil || res.Auth.Created || res.Auth.User.Username != "zhang.san" {
		t.Fatalf("已绑定的 sub 应优先: %v %+v", err, res.Auth)
	}
}

func TestOIDCES256AndUserinfoMerge(t *testing.T) {
	e := newTestEnv(t)
	idp := newFakeOIDC(t, e.clk)
	idp.useES = true
	idp.mutate = func(m map[string]any) { delete(m, "name"); delete(m, "email") } // id_token 里没有姓名邮箱，要靠 userinfo
	idp.userinfo = map[string]any{"sub": "sub-001", "name": "李四", "email": "li.si@example.com"}
	p := e.addOIDC(idp, FirstLoginCreate)
	cp, _ := e.repo.GetProvider(p.ID)
	cfg := cp.Config
	cfg.SignAlg = "ES256"
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: cp.Name, FirstLogin: FirstLoginCreate, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	e.enable(p.ID)
	res, err := e.sso(idp, p)
	if err != nil {
		t.Fatal(err)
	}
	if res.Auth.User.Display != "李四" || res.Auth.User.Email != "li.si@example.com" {
		t.Errorf("应合并 userinfo: %+v", res.Auth.User)
	}
	// userinfo 的 sub 与 id_token 不一致时不合并
	idp.userinfo = map[string]any{"sub": "evil", "name": "王五", "email": "w@example.com"}
	idp.claims = map[string]any{"sub": "sub-002", "preferred_username": "wang"}
	res, err = e.sso(idp, p)
	if err != nil || res.Auth.User.Display == "王五" {
		t.Errorf("sub 不一致的 userinfo 不能采信: %v %+v", err, res.Auth.User)
	}
}

func TestOIDCStateAttacks(t *testing.T) {
	e := newTestEnv(t)
	idp := newFakeOIDC(t, e.clk)
	p := e.addOIDC(idp, FirstLoginCreate)
	ctx := context.Background()

	st, _ := e.svc.BeginSSO(ctx, p.ID, BeginSSOInput{Next: "/query"})
	q := idp.authorize(st.URL)

	// 篡改 state
	bad := url.Values{"code": q["code"], "state": {q.Get("state") + "x"}}
	if _, err := e.svc.CompleteSSO(ctx, p.ID, bad, st.CookieValue, 0, ""); !errors.Is(err, ErrStateInvalid) {
		t.Errorf("state 被篡改应拒绝: %v", err)
	}
	// 没有 Cookie（登录 CSRF：别人构造的回调链接）
	if _, err := e.svc.CompleteSSO(ctx, p.ID, q, "", 0, ""); !errors.Is(err, ErrStateInvalid) {
		t.Errorf("没有状态 Cookie 应拒绝: %v", err)
	}
	// 乱写的 Cookie
	if _, err := e.svc.CompleteSSO(ctx, p.ID, q, st.CookieValue+"A", 0, ""); !errors.Is(err, ErrStateInvalid) {
		t.Errorf("Cookie 被篡改应拒绝: %v", err)
	}
	// 另一个登录方式的 Cookie 不能用在这个回调上
	p2 := e.addOIDC(idp, FirstLoginCreate)
	st2, _ := e.svc.BeginSSO(ctx, p2.ID, BeginSSOInput{})
	if _, err := e.svc.CompleteSSO(ctx, p.ID, idp.authorize(st2.URL), st2.CookieValue, 0, ""); !errors.Is(err, ErrStateInvalid) {
		t.Errorf("跨登录方式的状态应拒绝: %v", err)
	}
	// 正常一次
	if _, err := e.svc.CompleteSSO(ctx, p.ID, q, st.CookieValue, 0, ""); err != nil {
		t.Fatalf("正常回调应成功: %v", err)
	}
	// 回调重放：同一个 Cookie + 参数再来一次
	if _, err := e.svc.CompleteSSO(ctx, p.ID, q, st.CookieValue, 0, ""); !errors.Is(err, ErrStateInvalid) {
		t.Errorf("回调重放应拒绝: %v", err)
	}
	// state 过期
	st3, _ := e.svc.BeginSSO(ctx, p.ID, BeginSSOInput{})
	q3 := idp.authorize(st3.URL)
	e.clk.Add(11 * time.Minute)
	if _, err := e.svc.CompleteSSO(ctx, p.ID, q3, st3.CookieValue, 0, ""); !errors.Is(err, ErrStateInvalid) {
		t.Errorf("state 过期应拒绝: %v", err)
	}
}

func TestOIDCTokenAttacks(t *testing.T) {
	e := newTestEnv(t)
	idp := newFakeOIDC(t, e.clk)
	p := e.addOIDC(idp, FirstLoginCreate)

	now := func() int64 { return e.clk.Now().Unix() }
	cases := map[string]func(){
		"nonce 不匹配":    func() { idp.claims = map[string]any{"nonce": "other"} },
		"audience 不匹配": func() { idp.claims = map[string]any{"aud": "someone-else"} },
		"issuer 不匹配":   func() { idp.claims = map[string]any{"iss": "https://evil.example.com"} },
		"令牌已过期":        func() { idp.claims = map[string]any{"exp": now() - 600} },
		"签发时间在未来":      func() { idp.claims = map[string]any{"iat": now() + 3600} },
		"缺少 sub":       func() { idp.mutate = func(m map[string]any) { delete(m, "sub") } },
		"缺少 nonce":     func() { idp.mutate = func(m map[string]any) { delete(m, "nonce") } },
		"多 audience 无 azp": func() {
			idp.claims = map[string]any{"aud": []string{"dbtool", "other"}}
		},
		"alg=none": func() {
			idp.signWith = func(c jwt.MapClaims) (string, error) {
				return jwt.NewWithClaims(jwt.SigningMethodNone, c).SignedString(jwt.UnsafeAllowNoneSignatureType)
			}
		},
		"HS256 用公钥当密钥（算法混淆）": func() {
			idp.signWith = func(c jwt.MapClaims) (string, error) {
				pub := &idp.rsaKey.PublicKey
				der := append(pub.N.Bytes(), byte(pub.E))
				t := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
				t.Header["kid"] = "rsa1"
				return t.SignedString(der)
			}
		},
		"签名被替换（另一把 RSA 私钥）": func() {
			other, _ := rsa.GenerateKey(rand.Reader, 2048)
			idp.signWith = func(c jwt.MapClaims) (string, error) {
				t := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
				t.Header["kid"] = "rsa1"
				return t.SignedString(other)
			}
		},
		"未知 kid": func() {
			idp.signWith = func(c jwt.MapClaims) (string, error) {
				t := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
				t.Header["kid"] = "nope"
				return t.SignedString(idp.rsaKey)
			}
		},
		"算法与配置不符（ES256 令牌，配置 RS256）": func() {
			idp.signWith = func(c jwt.MapClaims) (string, error) {
				t := jwt.NewWithClaims(jwt.SigningMethodES256, c)
				t.Header["kid"] = "ec1"
				return t.SignedString(idp.ecKey)
			}
		},
		"令牌端点报错": func() { idp.tokenErr = true },
	}
	for name, setup := range cases {
		idp.claims, idp.mutate, idp.signWith, idp.tokenErr = nil, nil, nil, false
		setup()
		_, err := e.sso(idp, p)
		var fe *FlowError
		if err == nil || !errors.As(err, &fe) || fe.Code != "failed" {
			t.Errorf("%s：应失败: %v", name, err)
			continue
		}
		if fe.Msg != ErrSSOFailed.Msg {
			t.Errorf("%s：展示给用户的信息应是通用的: %q", name, fe.Msg)
		}
	}
	if u, _ := e.repo.UserByName("zhang.san"); u != nil {
		t.Error("攻击场景下不应创建用户")
	}
}

func TestOIDCPKCEIsEnforced(t *testing.T) {
	e := newTestEnv(t)
	idp := newFakeOIDC(t, e.clk)
	p := e.addOIDC(idp, FirstLoginCreate)
	ctx := context.Background()
	st, _ := e.svc.BeginSSO(ctx, p.ID, BeginSSOInput{})
	q := idp.authorize(st.URL)
	// 用另一次发起的 Cookie（verifier 不同）回调：state 就对不上；这里直接篡改 IdP 记住的 challenge 来模拟 verifier 不匹配
	idp.mu.Lock()
	for k, c := range idp.codes {
		c.challenge = "AAAA"
		idp.codes[k] = c
	}
	idp.mu.Unlock()
	if _, err := e.svc.CompleteSSO(ctx, p.ID, q, st.CookieValue, 0, ""); err == nil {
		t.Error("PKCE 校验不过应失败")
	}
}

func TestOIDCEmailMatching(t *testing.T) {
	e := newTestEnv(t)
	idp := newFakeOIDC(t, e.clk)
	p := e.addOIDC(idp, FirstLoginExisting)
	e.repo.users[2].Email = "zhang.san@example.com" // bob 的邮箱
	// 用户名 zhang.san 不存在 → 按邮箱匹配到 bob
	res, err := e.sso(idp, p)
	if err != nil || res.Auth.User.ID != 2 {
		t.Fatalf("应按邮箱匹配: %v", err)
	}
	// 邮箱未验证时不能按邮箱匹配（防止在 IdP 注册别人的邮箱）
	e.repo.idents = map[string]uint{}
	idp.claims = map[string]any{"email_verified": false}
	if _, err := e.sso(idp, p); !errors.Is(err, ErrNotProvisioned) {
		t.Errorf("未验证邮箱不应匹配: %v", err)
	}
	// 邮箱被多个用户共用时不猜
	idp.claims = nil
	e.repo.users[1].Email = "zhang.san@example.com"
	if _, err := e.sso(idp, p); !errors.Is(err, ErrNotProvisioned) {
		t.Errorf("邮箱不唯一不应匹配: %v", err)
	}
	// 匹配依据可配：只按用户名
	e.repo.users[1].Email = ""
	e.repo.kv[settingMatchBy] = MatchUsername
	e.svc.settings.Invalidate()
	if _, err := e.sso(idp, p); !errors.Is(err, ErrNotProvisioned) {
		t.Errorf("只按用户名时不应按邮箱匹配: %v", err)
	}
}

func TestOIDCDisabledAndSuperuser(t *testing.T) {
	e := newTestEnv(t)
	idp := newFakeOIDC(t, e.clk)
	p := e.addOIDC(idp, FirstLoginCreate)
	idp.claims = map[string]any{"preferred_username": "off"}
	if _, err := e.sso(idp, p); !errors.Is(err, ErrDisabledUser) {
		t.Errorf("停用用户一律拒绝: %v", err)
	}
	idp.claims = map[string]any{"preferred_username": "root", "sub": "sub-root"}
	if _, err := e.sso(idp, p); !errors.Is(err, ErrSuperuserLocal) {
		t.Errorf("超级用户只能本地登录: %v", err)
	}
	// 非法用户名不创建
	idp.claims = map[string]any{"preferred_username": "bad name<script>", "sub": "sub-x"}
	_, err := e.sso(idp, p)
	var fe *FlowError
	if !errors.As(err, &fe) || fe.Code != "username_invalid" {
		t.Errorf("非法用户名应拒绝: %v", err)
	}
}

func TestOIDCNotUsableUntilTested(t *testing.T) {
	e := newTestEnv(t)
	idp := newFakeOIDC(t, e.clk)
	p, _ := e.svc.CreateProvider(rootActor, KindOIDC, "x")
	cfg := p.Config
	cfg.WellKnownURL, cfg.ClientID, cfg.ClientSecret = idp.srv.URL+"/.well-known/openid-configuration", "dbtool", "s3cret"
	e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: "x", FirstLogin: FirstLoginCreate, Config: cfg})
	if err := e.svc.SetEnabled(rootActor, p.ID, true); err == nil {
		t.Fatal("没测试过不能启用")
	}
	if _, err := e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{}); !errors.Is(err, ErrProviderOff) {
		t.Errorf("未启用的方式不能发起登录: %v", err)
	}
	e.enable(p.ID)
	if _, err := e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{}); err != nil {
		t.Fatal(err)
	}
	// 改配置 → 自动停用，需重新测试
	cp, _ := e.repo.GetProvider(p.ID)
	cfg = cp.Config
	cfg.ClientID = "other"
	_, notice, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: "x", FirstLogin: FirstLoginCreate, Config: cfg})
	if err != nil || notice == "" {
		t.Fatalf("连接配置变更应提示停用: %v %q", err, notice)
	}
	if _, err := e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{}); !errors.Is(err, ErrProviderOff) {
		t.Error("配置变更后应不可用")
	}
	// 数据库里被人手工改成 enabled 但测试指纹对不上：仍然不可用
	cp, _ = e.repo.GetProvider(p.ID)
	cp.Enabled = true
	ok := true
	cp.TestOK, cp.TestedHash = &ok, "stale"
	e.repo.SaveProvider(cp)
	e.svc.invalidateProviders()
	if _, err := e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{}); !errors.Is(err, ErrProviderOff) {
		t.Error("指纹对不上的“已启用”应不可用")
	}
}

func TestOIDCTestLoginMode(t *testing.T) {
	e := newTestEnv(t)
	idp := newFakeOIDC(t, e.clk)
	p := e.addOIDC(idp, FirstLoginCreate)
	ctx := context.Background()
	if _, err := e.svc.BeginSSO(ctx, p.ID, BeginSSOInput{Test: true}); !errors.Is(err, ErrProviderOff) {
		t.Error("测试模式必须带发起者")
	}
	st, err := e.svc.BeginSSO(ctx, p.ID, BeginSSOInput{Test: true, Tester: 1})
	if err != nil {
		t.Fatal(err)
	}
	q := idp.authorize(st.URL)
	// 别的用户拿到这个回调链接也不能完成
	if _, err := e.svc.CompleteSSO(ctx, p.ID, q, st.CookieValue, 2, ""); !errors.Is(err, ErrStateInvalid) {
		t.Errorf("测试登录只允许发起者完成: %v", err)
	}
	st, _ = e.svc.BeginSSO(ctx, p.ID, BeginSSOInput{Test: true, Tester: 1})
	res, err := e.svc.CompleteSSO(ctx, p.ID, idp.authorize(st.URL), st.CookieValue, 1, "")
	if err != nil || res.Test == nil || !res.Test.OK || res.Auth != nil {
		t.Fatalf("%v %+v", err, res)
	}
	txt := strings.Join(res.Test.Lines, "\n")
	if !strings.Contains(txt, "username=zhang.san") || !strings.Contains(txt, "首次登录会自动创建") {
		t.Errorf("报告: %s", txt)
	}
	if u, _ := e.repo.UserByName("zhang.san"); u != nil {
		t.Error("测试登录不能创建用户")
	}
	if n, _ := e.repo.CountIdentities(p.ID); n != 0 {
		t.Error("测试登录不能写绑定")
	}
	// 失败的测试登录把结果写下来，且不让该方式继续可用
	idp.claims = map[string]any{"nonce": "x"}
	st, _ = e.svc.BeginSSO(ctx, p.ID, BeginSSOInput{Test: true, Tester: 1})
	res, _ = e.svc.CompleteSSO(ctx, p.ID, idp.authorize(st.URL), st.CookieValue, 1, "")
	if res.Test.OK {
		t.Fatal("应失败")
	}
	cp, _ := e.repo.GetProvider(p.ID)
	if cp.TestOK == nil || *cp.TestOK || cp.TestedHash != "" {
		t.Errorf("失败应记录并作废指纹: %+v", cp)
	}
	if v := e.svc.LoginPageInfo(false); len(v.Buttons) != 0 {
		t.Error("测试失败后登录页不应再显示该按钮")
	}
}

func TestOIDCDiscoveryValidation(t *testing.T) {
	e := newTestEnv(t)
	idp := newFakeOIDC(t, e.clk)
	// issuer 与 Wellknown 地址不一致 → 测试不通过
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": "https://other.example.com", "authorization_endpoint": idp.srv.URL + "/authorize",
			"token_endpoint": idp.srv.URL + "/token", "jwks_uri": idp.srv.URL + "/jwks"})
	})
	bad := httptest.NewServer(mux)
	defer bad.Close()
	p, _ := e.svc.CreateProvider(rootActor, KindOIDC, "bad")
	cfg := p.Config
	cfg.WellKnownURL, cfg.ClientID, cfg.ClientSecret = bad.URL+"/.well-known/openid-configuration", "dbtool", "s3cret"
	e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: "bad", FirstLogin: FirstLoginCreate, Config: cfg})
	rep, err := e.svc.TestProvider(context.Background(), rootActor, p.ID, TestInput{})
	if err != nil || rep.OK || !strings.Contains(strings.Join(rep.Lines, ""), "issuer") {
		t.Errorf("issuer 不一致应测试失败: %+v", rep)
	}
	// 非 https 且非回环地址
	cfg.WellKnownURL = "http://sso.example.com/.well-known/openid-configuration"
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: "bad", FirstLogin: FirstLoginCreate, Config: cfg}); err == nil {
		t.Error("http 的 Wellknown 地址应拒绝保存")
	}
	// 缺少必填项
	cfg.WellKnownURL, cfg.ClientID = "", ""
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: "bad", FirstLogin: FirstLoginCreate, Config: cfg}); err == nil {
		t.Error("缺少必填项应拒绝")
	}
}

func TestJWKRejectsWeakAndInvalid(t *testing.T) {
	if _, err := rsaKey(base64.RawURLEncoding.EncodeToString(big.NewInt(12345).Bytes()), "AQAB"); err == nil {
		t.Error("过短的 RSA 密钥应拒绝")
	}
	if _, err := ecKey("P-384", "AA", "AA"); err == nil {
		t.Error("非 P-256 应拒绝")
	}
	if _, err := ecKey("P-256", "AQ", "AQ"); err == nil {
		t.Error("不在曲线上的点应拒绝")
	}
	_ = crypto.SHA256
	_ = hmac.New
}
