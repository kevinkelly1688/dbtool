// @author: kevinkelly
// @license: Apache Licence
// @file: social_test.go
// @time: 2026/10/28

package authx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeSocial 同时扮演钉钉 / 飞书(Lark) / 企业微信的 API 服务器：按原始主机名（X-Orig-Host）与路径分发。
type fakeSocial struct {
	t   *testing.T
	srv *httptest.Server

	mu   sync.Mutex
	reqs []string // “主机 方法 路径”
	body map[string]map[string]any

	dingUnion, dingUserID string
	dingNotMember         bool
	feishuTenant          string
	feishuOpenID          string
	wecomUser, wecomOpen  string
	wecomTokenCalls       int
}

func newFakeSocial(t *testing.T) *fakeSocial {
	f := &fakeSocial{t: t, body: map[string]map[string]any{}, dingUnion: "union-1", dingUserID: "0123", feishuTenant: "tenant-1", feishuOpenID: "ou_abc", wecomUser: "zhangsan"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSocial) record(r *http.Request) map[string]any {
	var m map[string]any
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &m)
	}
	key := r.Header.Get("X-Orig-Host") + " " + r.Method + " " + r.URL.Path
	f.mu.Lock()
	f.reqs = append(f.reqs, key)
	f.body[key] = m
	f.mu.Unlock()
	return m
}

func (f *fakeSocial) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reqs {
		if r == key {
			n++
		}
	}
	return n
}

func (f *fakeSocial) handle(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	host := r.Header.Get("X-Orig-Host")
	enc := json.NewEncoder(w)
	switch {
	// ---- 钉钉 ----
	case host == "api.dingtalk.com" && r.URL.Path == "/v1.0/oauth2/userAccessToken":
		if body["clientId"] != "ding-key" || body["clientSecret"] != "ding-secret" || body["code"] != "ding-code" || body["grantType"] != "authorization_code" {
			w.WriteHeader(400)
			return
		}
		enc.Encode(map[string]any{"accessToken": "user-at", "expireIn": 7200})
	case host == "api.dingtalk.com" && r.URL.Path == "/v1.0/contact/users/me":
		if r.Header.Get("x-acs-dingtalk-access-token") != "user-at" {
			w.WriteHeader(401)
			return
		}
		enc.Encode(map[string]any{"unionId": f.dingUnion, "nick": "张三", "openId": "open-1"})
	case host == "oapi.dingtalk.com" && r.URL.Path == "/gettoken":
		if r.URL.Query().Get("appkey") != "ding-key" || r.URL.Query().Get("appsecret") != "ding-secret" {
			enc.Encode(map[string]any{"errcode": 40089})
			return
		}
		enc.Encode(map[string]any{"errcode": 0, "access_token": "app-at", "expires_in": 7200})
	case host == "oapi.dingtalk.com" && r.URL.Path == "/topapi/user/getbyunionid":
		if r.URL.Query().Get("access_token") != "app-at" {
			enc.Encode(map[string]any{"errcode": 40014})
			return
		}
		uid := f.dingUserID
		if f.dingNotMember {
			uid = ""
		}
		enc.Encode(map[string]any{"errcode": 0, "result": map[string]any{"userid": uid}})
	case host == "oapi.dingtalk.com" && r.URL.Path == "/topapi/v2/user/get":
		enc.Encode(map[string]any{"errcode": 0, "result": map[string]any{"userid": f.dingUserID, "name": "张三", "org_email": "zhang.san@corp.com", "email": "zs@gmail.com", "dept_id_list": []int{1, 22}}})
	// ---- 飞书 / Lark ----
	case strings.HasPrefix(host, "open.") && r.URL.Path == "/open-apis/authen/v2/oauth/token":
		if body["client_id"] != "cli_app" || body["client_secret"] != "fs-secret" || body["code"] != "fs-code" || body["code_verifier"] == "" || body["grant_type"] != "authorization_code" {
			enc.Encode(map[string]any{"code": 20024})
			return
		}
		enc.Encode(map[string]any{"code": 0, "access_token": "fs-at", "token_type": "Bearer"})
	case strings.HasPrefix(host, "open.") && r.URL.Path == "/open-apis/authen/v1/user_info":
		if r.Header.Get("Authorization") != "Bearer fs-at" {
			w.WriteHeader(401)
			return
		}
		enc.Encode(map[string]any{"code": 0, "data": map[string]any{"open_id": f.feishuOpenID, "union_id": "on_1", "user_id": "fs001", "name": "李四", "email": "li.si@corp.com", "tenant_key": f.feishuTenant}})
	case strings.HasPrefix(host, "open.") && r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
		if body["app_id"] != "cli_app" || body["app_secret"] != "fs-secret" {
			enc.Encode(map[string]any{"code": 10003, "msg": "invalid param"})
			return
		}
		enc.Encode(map[string]any{"code": 0, "tenant_access_token": "t-1"})
	// ---- 企业微信 ----
	case host == "qyapi.weixin.qq.com" && r.URL.Path == "/cgi-bin/gettoken":
		f.mu.Lock()
		f.wecomTokenCalls++
		f.mu.Unlock()
		if r.URL.Query().Get("corpid") != "ww123" || r.URL.Query().Get("corpsecret") != "wc-secret" {
			enc.Encode(map[string]any{"errcode": 40013})
			return
		}
		enc.Encode(map[string]any{"errcode": 0, "access_token": "wc-at", "expires_in": 7200})
	case host == "qyapi.weixin.qq.com" && r.URL.Path == "/cgi-bin/auth/getuserinfo":
		if r.URL.Query().Get("access_token") != "wc-at" || r.URL.Query().Get("code") != "wc-code" {
			enc.Encode(map[string]any{"errcode": 40029})
			return
		}
		if f.wecomOpen != "" {
			enc.Encode(map[string]any{"errcode": 0, "openid": f.wecomOpen})
			return
		}
		enc.Encode(map[string]any{"errcode": 0, "userid": f.wecomUser})
	case host == "qyapi.weixin.qq.com" && r.URL.Path == "/cgi-bin/user/get":
		enc.Encode(map[string]any{"errcode": 0, "name": "张三", "biz_mail": "zhangsan@corp.com", "department": []int{3}})
	default:
		f.t.Errorf("未预期的请求：%s %s", host, r.URL.Path)
		w.WriteHeader(404)
	}
}

func stateOf(t *testing.T, authURL string) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	s := u.Query().Get("state")
	if s == "" { // CAS：state 放在 service URL 里
		if su, err := url.Parse(u.Query().Get("service")); err == nil {
			s = su.Query().Get("state")
		}
	}
	if s == "" {
		t.Fatalf("授权地址缺少 state: %s", authURL)
	}
	return s
}

// addKind 创建并（可选地）强制可用一个登录方式。
func (e *testEnv) addKind(kind Kind, name string, cfg Config, first string) *Provider {
	e.t.Helper()
	p, err := e.svc.CreateProvider(rootActor, kind, name)
	if err != nil {
		e.t.Fatal(err)
	}
	cfg.GroupRules = nil
	merged := p.Config
	merged.ClientID, merged.ClientSecret, merged.CorpID, merged.AgentID, merged.TenantKey, merged.Site, merged.TeamID = cfg.ClientID, cfg.ClientSecret, cfg.CorpID, cfg.AgentID, cfg.TenantKey, cfg.Site, cfg.TeamID
	merged.CASURL, merged.CASVersion, merged.ForceHTTPS = cfg.CASURL, cfg.CASVersion, cfg.ForceHTTPS
	if cfg.WeComMode != "" {
		merged.WeComMode = cfg.WeComMode
	}
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: name, FirstLogin: first, Config: merged, BtnLabel: name + "登录"}); err != nil {
		e.t.Fatal(err)
	}
	cp, _ := e.repo.GetProvider(p.ID)
	return cp
}

func newSocialEnv(t *testing.T) (*testEnv, *fakeSocial) {
	t.Helper()
	f := newFakeSocial(t)
	e := newTestEnvHTTP(t, rewriteClient(f.srv))
	return e, f
}

func (e *testEnv) callback(p *Provider, query func(state string) url.Values) (*SSOResult, error) {
	e.t.Helper()
	st, err := e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{Next: "/query", UserAgent: "Mozilla/5.0"})
	if err != nil {
		return nil, err
	}
	return e.svc.CompleteSSO(context.Background(), p.ID, query(stateOf(e.t, st.URL)), st.CookieValue, 0, "")
}

func TestDingTalkLogin(t *testing.T) {
	e, f := newSocialEnv(t)
	p := e.addKind(KindDingTalk, "钉钉扫码", Config{ClientID: "ding-key", ClientSecret: "ding-secret", CorpID: "ding-corp"}, FirstLoginCreate)
	// 授权地址
	st, err := e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{})
	if err == nil {
		t.Fatal("未启用不能发起")
	}
	_ = st
	e.enable(p.ID)
	st, err = e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(st.URL)
	q := u.Query()
	if u.Host != "login.dingtalk.com" || q.Get("client_id") != "ding-key" || q.Get("response_type") != "code" || q.Get("redirect_uri") != "http://dba.test/login/sso/"+itoa(p.ID)+"/callback" ||
		q.Get("state") == "" || q.Get("state") == "dddd" || !strings.Contains(q.Get("scope"), "corpid") || q.Get("corpId") != "ding-corp" {
		t.Fatalf("授权地址: %s", st.URL)
	}
	res, err := e.callback(p, func(s string) url.Values { return url.Values{"authCode": {"ding-code"}, "state": {s}} })
	if err != nil {
		t.Fatal(err)
	}
	a := res.Auth
	if !a.Created || a.User.Username != "zhang.san" || a.User.DingUserID != "" && false {
		t.Fatalf("%+v", a.User)
	}
	cur, _ := e.repo.UserByID(a.User.ID)
	if cur.DingUserID != "0123" {
		t.Errorf("应回填钉钉 UserID: %q", cur.DingUserID)
	}
	if a.User.Email != "zs@gmail.com" && a.User.Email != "zhang.san@corp.com" {
		t.Errorf("email: %q", a.User.Email)
	}
	if f.count("oapi.dingtalk.com GET /gettoken") != 1 {
		t.Error("企业应用令牌应被缓存")
	}
	// 不属于本企业：getbyunionid 查不到 userid
	f.dingNotMember = true
	if _, err := e.callback(p, func(s string) url.Values { return url.Values{"authCode": {"ding-code"}, "state": {s}} }); err == nil {
		t.Error("非本企业成员应失败")
	}
	// 授权码错误
	f.dingNotMember = false
	if _, err := e.callback(p, func(s string) url.Values { return url.Values{"authCode": {"wrong"}, "state": {s}} }); err == nil {
		t.Error("授权码错误应失败")
	}
	// 回调没有授权码
	if _, err := e.callback(p, func(s string) url.Values { return url.Values{"state": {s}} }); err == nil {
		t.Error("缺少授权码应失败")
	}
}

func TestDingTalkMatchByThirdID(t *testing.T) {
	e, _ := newSocialEnv(t)
	p := e.addKind(KindDingTalk, "钉钉", Config{ClientID: "ding-key", ClientSecret: "ding-secret"}, FirstLoginExisting)
	e.enable(p.ID)
	// 平台用户 bob 的钉钉 ID 是 0123，用户名完全不同
	e.repo.users[2].DingUserID = "0123"
	e.repo.kv[settingMatchBy] = MatchThirdID
	e.svc.settings.Invalidate()
	res, err := e.callback(p, func(s string) url.Values { return url.Values{"authCode": {"ding-code"}, "state": {s}} })
	if err != nil || res.Auth.User.ID != 2 {
		t.Fatalf("应按钉钉 ID 匹配到 bob: %v", err)
	}
	// 关闭按第三方 ID 匹配 → 找不到（用户名 zhang.san 不存在，仅已有用户）
	e.repo.idents = map[string]uint{}
	e.repo.kv[settingMatchBy] = MatchUsername
	e.svc.settings.Invalidate()
	if _, err := e.callback(p, func(s string) url.Values { return url.Values{"authCode": {"ding-code"}, "state": {s}} }); !errors.Is(err, ErrNotProvisioned) {
		t.Errorf("应拒绝: %v", err)
	}
}

func TestBackfillSwitches(t *testing.T) {
	e, _ := newSocialEnv(t)
	p := e.addKind(KindDingTalk, "钉钉", Config{ClientID: "ding-key", ClientSecret: "ding-secret"}, FirstLoginExisting)
	e.enable(p.ID)
	e.repo.addUser(20, "zhang.san", pw, false)
	e.repo.kv[settingBackfill] = "false" // 全局关闭回填
	e.svc.settings.Invalidate()
	if _, err := e.callback(p, func(s string) url.Values { return url.Values{"authCode": {"ding-code"}, "state": {s}} }); err != nil {
		t.Fatal(err)
	}
	if e.repo.users[20].DingUserID != "" {
		t.Error("全局关闭后不应回填")
	}
	e.repo.kv[settingBackfill] = "true"
	e.svc.settings.Invalidate()
	if _, err := e.callback(p, func(s string) url.Values { return url.Values{"authCode": {"ding-code"}, "state": {s}} }); err != nil {
		t.Fatal(err)
	}
	if e.repo.users[20].DingUserID != "0123" {
		t.Error("开启后应回填钉钉 UserID")
	}
}

func TestFeishuAndLark(t *testing.T) {
	e, f := newSocialEnv(t)
	for _, site := range []string{"feishu", "lark"} {
		p := e.addKind(KindFeishu, site, Config{ClientID: "cli_app", ClientSecret: "fs-secret", Site: site, TenantKey: "tenant-1"}, FirstLoginCreate)
		rep, err := e.svc.TestProvider(context.Background(), rootActor, p.ID, TestInput{})
		if err != nil || !rep.OK {
			t.Fatalf("%s 测试连接应通过: %+v", site, rep)
		}
		e.svc.SetEnabled(rootActor, p.ID, true)
		st, err := e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{})
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(st.URL)
		wantHost := map[string]string{"feishu": "accounts.feishu.cn", "lark": "accounts.larksuite.com"}[site]
		if u.Host != wantHost || u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("code_challenge") == "" || u.Query().Get("client_id") != "cli_app" {
			t.Fatalf("%s 授权地址: %s", site, st.URL)
		}
		res, err := e.callback(p, func(s string) url.Values { return url.Values{"code": {"fs-code"}, "state": {s}} })
		if err != nil {
			t.Fatalf("%s: %v", site, err)
		}
		if res.Auth.User.Username != "fs001" || res.Auth.User.Email != "li.si@corp.com" {
			t.Errorf("%s: %+v", site, res.Auth.User)
		}
		if got := e.repo.users[res.Auth.User.ID].FeishuOpenID; got != "ou_abc" {
			t.Errorf("应回填飞书 OpenID: %q", got)
		}
		// 租户不符
		f.feishuTenant = "tenant-other"
		if _, err := e.callback(p, func(s string) url.Values { return url.Values{"code": {"fs-code"}, "state": {s}} }); err == nil {
			t.Errorf("%s: 租户不符应拒绝", site)
		}
		f.feishuTenant = "tenant-1"
		// 换回调里 code 不对
		if _, err := e.callback(p, func(s string) url.Values { return url.Values{"code": {"bad"}, "state": {s}} }); err == nil {
			t.Error("授权码错误应失败")
		}
		// 用户拒绝授权
		if _, err := e.callback(p, func(s string) url.Values { return url.Values{"error": {"access_denied"}, "state": {s}} }); err == nil {
			t.Error("用户拒绝应失败")
		}
	}
	if f.count("open.larksuite.com POST /open-apis/authen/v2/oauth/token") == 0 || f.count("open.feishu.cn POST /open-apis/authen/v2/oauth/token") == 0 {
		t.Error("两个站点都应走到各自的域名")
	}
	// 凭据错误时测试连接失败
	p := e.addKind(KindFeishu, "bad", Config{ClientID: "cli_app", ClientSecret: "wrong"}, FirstLoginExisting)
	if rep, _ := e.svc.TestProvider(context.Background(), rootActor, p.ID, TestInput{}); rep.OK {
		t.Error("凭据错误应测试失败")
	}
}

func TestWeComLogin(t *testing.T) {
	e, f := newSocialEnv(t)
	p := e.addKind(KindWeCom, "企业微信", Config{CorpID: "ww123", AgentID: "1000002", ClientSecret: "wc-secret"}, FirstLoginCreate)
	e.enable(p.ID)
	// 浏览器扫码 vs 企业微信内免登
	st, _ := e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{UserAgent: "Mozilla/5.0 Chrome"})
	if !strings.HasPrefix(st.URL, "https://login.work.weixin.qq.com/wwlogin/sso/login?") || !strings.Contains(st.URL, "login_type=CorpApp") {
		t.Errorf("扫码地址: %s", st.URL)
	}
	st, _ = e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{UserAgent: "Mozilla/5.0 wxwork/4.1.0 MicroMessenger"})
	if !strings.HasPrefix(st.URL, "https://open.weixin.qq.com/connect/oauth2/authorize?") || !strings.HasSuffix(st.URL, "#wechat_redirect") {
		t.Errorf("免登地址: %s", st.URL)
	}
	res, err := e.callback(p, func(s string) url.Values { return url.Values{"code": {"wc-code"}, "state": {s}} })
	if err != nil {
		t.Fatal(err)
	}
	if res.Auth.User.Username != "zhangsan" || res.Auth.User.Email != "zhangsan@corp.com" {
		t.Errorf("%+v", res.Auth.User)
	}
	if _, err := e.callback(p, func(s string) url.Values { return url.Values{"code": {"wc-code"}, "state": {s}} }); err != nil {
		t.Fatal(err)
	}
	if f.wecomTokenCalls != 1 {
		t.Errorf("access_token 应缓存: %d", f.wecomTokenCalls)
	}
	// 企业外成员只返回 openid
	f.wecomOpen = "openid-1"
	if _, err := e.callback(p, func(s string) url.Values { return url.Values{"code": {"wc-code"}, "state": {s}} }); err == nil {
		t.Error("非企业成员应拒绝")
	}
	// 回调里 state 对不上
	f.wecomOpen = ""
	if _, err := e.callback(p, func(s string) url.Values { return url.Values{"code": {"wc-code"}, "state": {"forged"}} }); !errors.Is(err, ErrStateInvalid) {
		t.Errorf("state 不符: %v", err)
	}
	// 错误信息里不能出现密钥或授权码
	_, err = e.callback(p, func(s string) url.Values { return url.Values{"code": {"leak-me-code"}, "state": {s}} })
	if err == nil {
		t.Fatal("应失败")
	}
	var fe *FlowError
	if errors.As(err, &fe) && (strings.Contains(fe.Detail, "leak-me-code") || strings.Contains(fe.Detail, "wc-secret")) {
		t.Errorf("错误详情泄漏了敏感值: %s", fe.Detail)
	}
}

func TestSlackLogin(t *testing.T) {
	e := newTestEnv(t)
	idp := newFakeOIDC(t, e.clk)
	idp.issuer = "https://slack.com"
	e.svc.flows[KindSlack] = newSlackFlow(newOIDCCore(&flowEnv{HTTP: fixedHTTP(rewriteClient(idp.srv)), Now: e.clk.Now}))
	p := e.addKind(KindSlack, "Slack", Config{ClientID: "dbtool", ClientSecret: "s3cret", TeamID: "T0123ABCD"}, FirstLoginCreate)
	e.enable(p.ID)
	idp.claims = map[string]any{"iss": "https://slack.com", "https://slack.com/team_id": "T0123ABCD", "https://slack.com/user_id": "U012AB3CD", "email": "wang.wu@example.com", "name": "王五", "preferred_username": nil}
	st, _ := e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{})
	u, _ := url.Parse(st.URL)
	if u.Query().Get("team") != "T0123ABCD" || !strings.Contains(u.Query().Get("scope"), "openid") {
		t.Errorf("授权地址应限定工作区: %s", st.URL)
	}
	res, err := e.svc.CompleteSSO(context.Background(), p.ID, idp.authorize(st.URL), st.CookieValue, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Auth.User.Username != "wang.wu" || e.repo.users[res.Auth.User.ID].SlackID != "U012AB3CD" {
		t.Errorf("用户名取邮箱前缀，并回填 Slack ID: %+v", e.repo.users[res.Auth.User.ID])
	}
	// 工作区不符
	idp.claims["https://slack.com/team_id"] = "T-OTHER"
	st, _ = e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{})
	if _, err := e.svc.CompleteSSO(context.Background(), p.ID, idp.authorize(st.URL), st.CookieValue, 0, ""); err == nil {
		t.Error("工作区不符应拒绝")
	}
}

// ---- CAS ----

type fakeCAS struct {
	srv      *httptest.Server
	body     func(service, ticket string) string
	lastSvc  string
	lastPath string
}

func newFakeCAS(t *testing.T) *fakeCAS {
	f := &fakeCAS{}
	mux := http.NewServeMux()
	mux.HandleFunc("/cas/login", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>login</html>")) })
	handler := func(w http.ResponseWriter, r *http.Request) {
		f.lastSvc, f.lastPath = r.URL.Query().Get("service"), r.URL.Path
		w.Write([]byte(f.body(r.URL.Query().Get("service"), r.URL.Query().Get("ticket"))))
	}
	for _, p := range []string{"/cas/serviceValidate", "/cas/p3/serviceValidate", "/cas/validate"} {
		mux.HandleFunc(p, handler)
	}
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

const casOK = `<cas:serviceResponse xmlns:cas="http://www.yale.edu/tp/cas"><cas:authenticationSuccess><cas:user>zhao.liu</cas:user>
<cas:attributes><cas:displayName>赵六</cas:displayName><cas:mail>zhao.liu@example.com</cas:mail><cas:groups>dba</cas:groups></cas:attributes></cas:authenticationSuccess></cas:serviceResponse>`

func TestCASLogin(t *testing.T) {
	e := newTestEnv(t)
	cas := newFakeCAS(t)
	cas.body = func(service, ticket string) string {
		if ticket != "ST-1-abc" {
			return `<cas:serviceResponse xmlns:cas="http://www.yale.edu/tp/cas"><cas:authenticationFailure code="INVALID_TICKET">Ticket not recognized</cas:authenticationFailure></cas:serviceResponse>`
		}
		return casOK
	}
	p := e.addKind(KindCAS, "CAS", Config{CASURL: cas.srv.URL + "/cas", CASVersion: "3"}, FirstLoginCreate)
	rep, _ := e.svc.TestProvider(context.Background(), rootActor, p.ID, TestInput{})
	if !rep.OK {
		t.Fatalf("测试: %+v", rep)
	}
	e.svc.SetEnabled(rootActor, p.ID, true)
	st, _ := e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{})
	u, _ := url.Parse(st.URL)
	svc := u.Query().Get("service")
	if !strings.HasPrefix(st.URL, cas.srv.URL+"/cas/login?") || !strings.Contains(svc, "/login/sso/"+itoa(p.ID)+"/callback?state=") {
		t.Fatalf("CAS 登录地址: %s", st.URL)
	}
	state := stateOf(t, svc)
	res, err := e.svc.CompleteSSO(context.Background(), p.ID, url.Values{"state": {state}, "ticket": {"ST-1-abc"}}, st.CookieValue, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if cas.lastPath != "/cas/p3/serviceValidate" || cas.lastSvc != svc {
		t.Errorf("校验 ticket 时 service 必须与登录时一致: %q vs %q（%s）", cas.lastSvc, svc, cas.lastPath)
	}
	if res.Auth.User.Username != "zhao.liu" || res.Auth.User.Display != "赵六" || res.Auth.User.Email != "zhao.liu@example.com" {
		t.Errorf("%+v", res.Auth.User)
	}
	// ticket 无效
	st, _ = e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{})
	u, _ = url.Parse(st.URL)
	if _, err := e.svc.CompleteSSO(context.Background(), p.ID, url.Values{"state": {stateOf(t, u.Query().Get("service"))}, "ticket": {"ST-bad"}}, st.CookieValue, 0, ""); err == nil {
		t.Error("无效 ticket 应失败")
	}
	// 缺少 ticket
	st, _ = e.svc.BeginSSO(context.Background(), p.ID, BeginSSOInput{})
	u, _ = url.Parse(st.URL)
	if _, err := e.svc.CompleteSSO(context.Background(), p.ID, url.Values{"state": {stateOf(t, u.Query().Get("service"))}}, st.CookieValue, 0, ""); err == nil {
		t.Error("缺少 ticket 应失败")
	}
}

func TestCASVersionsAndXMLHardening(t *testing.T) {
	// CAS 1.0
	e := newTestEnv(t)
	cas := newFakeCAS(t)
	cas.body = func(service, ticket string) string { return "yes\nlegacy.user\n" }
	p := e.addKind(KindCAS, "CAS1", Config{CASURL: cas.srv.URL + "/cas/", CASVersion: "1"}, FirstLoginCreate)
	e.enable(p.ID)
	res, err := e.callback(p, func(s string) url.Values { return url.Values{"state": {s}, "ticket": {"ST-1"}} })
	if err != nil || res.Auth.User.Username != "legacy.user" || cas.lastPath != "/cas/validate" {
		t.Fatalf("CAS1: %v %+v %s", err, res, cas.lastPath)
	}
	cas.body = func(service, ticket string) string { return "no\n\n" }
	if _, err := e.callback(p, func(s string) url.Values { return url.Values{"state": {s}, "ticket": {"ST-1"}} }); err == nil {
		t.Error("CAS1 拒绝应失败")
	}
	// XML：DTD / 外部实体、过大、畸形都拒绝
	for name, body := range map[string]string{
		"DOCTYPE":    `<?xml version="1.0"?><!DOCTYPE foo [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><cas:serviceResponse xmlns:cas="x"><cas:authenticationSuccess><cas:user>&xxe;</cas:user></cas:authenticationSuccess></cas:serviceResponse>`,
		"小写 doctype": `<!doctype x><cas:serviceResponse xmlns:cas="x"><cas:authenticationSuccess><cas:user>a</cas:user></cas:authenticationSuccess></cas:serviceResponse>`,
		"畸形":         `<cas:serviceResponse xmlns:cas="x"><cas:authenticationSuccess>`,
		"没有用户":       `<cas:serviceResponse xmlns:cas="x"><cas:authenticationSuccess><cas:user> </cas:user></cas:authenticationSuccess></cas:serviceResponse>`,
		"过大":         `<cas:serviceResponse xmlns:cas="x"><cas:authenticationSuccess><cas:user>` + strings.Repeat("a", 300*1024) + `</cas:user></cas:authenticationSuccess></cas:serviceResponse>`,
	} {
		if _, _, err := parseCASXML([]byte(body)); err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
	if u, _, err := parseCASXML([]byte(casOK)); err != nil || u != "zhao.liu" {
		t.Errorf("正常响应: %v %q", err, u)
	}
}

func TestSocialValidationAndErrors(t *testing.T) {
	e, _ := newSocialEnv(t)
	for _, c := range []struct {
		kind Kind
		cfg  Config
	}{
		{KindDingTalk, Config{ClientID: "k"}},
		{KindFeishu, Config{ClientSecret: "s"}},
		{KindWeCom, Config{CorpID: "ww", ClientSecret: "s"}},
		{KindSlack, Config{ClientID: "c"}},
		{KindCAS, Config{CASURL: "http://cas.example.com/cas/"}},
	} {
		p, _ := e.svc.CreateProvider(rootActor, c.kind, "x")
		cfg := p.Config
		cfg.ClientID, cfg.ClientSecret, cfg.CorpID, cfg.AgentID, cfg.CASURL = c.cfg.ClientID, c.cfg.ClientSecret, c.cfg.CorpID, c.cfg.AgentID, c.cfg.CASURL
		if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: "x", FirstLogin: FirstLoginExisting, Config: cfg}); err == nil {
			t.Errorf("%s 缺必填项 / 非 https 应拒绝保存", c.kind)
		}
	}
	_ = fmt.Sprint
}
