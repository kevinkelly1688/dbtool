// @author: kevinkelly
// @license: Apache Licence
// @file: social.go
// @time: 2026/10/28

package authx

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// 钉钉、飞书（Lark）、企业微信：各家自己的 OAuth 变体。交互序列参照各家官方文档，
// 回调参数一律先核对 state（由 Service 在调用 Complete 前完成），这里不信任 IdP 回传的任何字段，只取用户标识与属性。

// ---------- 钉钉 ----------

type dingFlow struct {
	env  *flowEnv
	apps *ttlCache[string] // 企业内部应用 access_token 缓存
}

func newDingFlow(env *flowEnv) *dingFlow {
	return &dingFlow{env: env, apps: newTTLCache[string](env.Now)}
}

const (
	dingAuthURL  = "https://login.dingtalk.com/oauth2/auth"
	dingAPIBase  = "https://api.dingtalk.com"
	dingOAPIBase = "https://oapi.dingtalk.com"
)

func (f *dingFlow) validate(p *Provider) error {
	return requireFields("AppKey", p.Config.ClientID, "AppSecret", p.Config.ClientSecret)
}

func (f *dingFlow) Begin(_ context.Context, p *Provider, r BeginReq) (string, error) {
	if err := f.validate(p); err != nil {
		return "", err
	}
	q := url.Values{"response_type": {"code"}, "scope": {"openid"}, "client_id": {p.Config.ClientID},
		"redirect_uri": {r.RedirectURI}, "state": {r.State}, "prompt": {"consent"}}
	if c := strings.TrimSpace(p.Config.CorpID); c != "" {
		q.Set("scope", "openid corpid")
		q.Set("corpId", c)
	}
	return authURL(dingAuthURL, q), nil
}

// appToken 取企业内部应用的 access_token（用 unionId 换企业内 userid 需要）。
func (f *dingFlow) appToken(ctx context.Context, p *Provider) (string, error) {
	key := p.Config.ClientID
	if t, ok := f.apps.get(key); ok {
		return t, nil
	}
	var out struct {
		ErrCode int    `json:"errcode"`
		Token   string `json:"access_token"`
		Expires int    `json:"expires_in"`
	}
	u := dingOAPIBase + "/gettoken?" + url.Values{"appkey": {p.Config.ClientID}, "appsecret": {p.Config.ClientSecret}}.Encode()
	if err := getJSON(ctx, f.env.HTTP(false), u, nil, &out); err != nil {
		return "", fmt.Errorf("获取钉钉应用令牌失败：%w", err)
	}
	if out.Token == "" || out.ErrCode != 0 {
		return "", fmt.Errorf("获取钉钉应用令牌失败（errcode=%d），请检查 AppKey / AppSecret", out.ErrCode)
	}
	ttl := time.Duration(out.Expires-120) * time.Second
	if ttl <= 0 {
		ttl = time.Minute
	}
	f.apps.set(key, out.Token, ttl)
	return out.Token, nil
}

func (f *dingFlow) Complete(ctx context.Context, p *Provider, r CompleteReq) (*Identity, error) {
	if err := f.validate(p); err != nil {
		return nil, err
	}
	code := r.Query.Get("authCode")
	if code == "" {
		code = r.Query.Get("code")
	}
	if code == "" {
		return nil, errors.New("回调缺少授权码")
	}
	hc := f.env.HTTP(false)
	// 1) 授权码 -> 用户 access token
	var tok struct {
		AccessToken string `json:"accessToken"`
	}
	if err := postJSON(ctx, hc, dingAPIBase+"/v1.0/oauth2/userAccessToken", nil, map[string]string{
		"clientId": p.Config.ClientID, "clientSecret": p.Config.ClientSecret, "code": code, "grantType": "authorization_code"}, &tok); err != nil {
		return nil, fmt.Errorf("换取钉钉令牌失败：%w", err)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("钉钉没有返回令牌")
	}
	// 2) 用户 token -> 个人信息（unionId）
	var me struct {
		UnionID string `json:"unionId"`
		OpenID  string `json:"openId"`
		Nick    string `json:"nick"`
		Email   string `json:"email"`
		Mobile  string `json:"mobile"`
	}
	if err := getJSON(ctx, hc, dingAPIBase+"/v1.0/contact/users/me", map[string]string{"x-acs-dingtalk-access-token": tok.AccessToken}, &me); err != nil {
		return nil, fmt.Errorf("获取钉钉用户信息失败：%w", err)
	}
	if me.UnionID == "" {
		return nil, errors.New("钉钉没有返回 unionId")
	}
	// 3) unionId -> 企业内 userid（只有本企业成员能查到，天然限制了企业范围）
	at, err := f.appToken(ctx, p)
	if err != nil {
		return nil, err
	}
	var byUnion struct {
		ErrCode int `json:"errcode"`
		Result  struct {
			UserID string `json:"userid"`
		} `json:"result"`
	}
	if err := postJSON(ctx, hc, dingOAPIBase+"/topapi/user/getbyunionid?"+url.Values{"access_token": {at}}.Encode(), nil,
		map[string]string{"unionid": me.UnionID}, &byUnion); err != nil {
		return nil, fmt.Errorf("钉钉 unionId 换 userid 失败：%w", err)
	}
	if byUnion.Result.UserID == "" {
		return nil, errors.New("该钉钉账号不属于本应用所在企业")
	}
	// 4) userid -> 企业通讯录详情
	var det struct {
		ErrCode int `json:"errcode"`
		Result  struct {
			UserID    string `json:"userid"`
			UnionID   string `json:"unionid"`
			Name      string `json:"name"`
			Email     string `json:"email"`
			OrgEmail  string `json:"org_email"`
			Mobile    string `json:"mobile"`
			Title     string `json:"title"`
			JobNumber string `json:"job_number"`
			DeptIDs   []int  `json:"dept_id_list"`
		} `json:"result"`
	}
	if err := postJSON(ctx, hc, dingOAPIBase+"/topapi/v2/user/get?"+url.Values{"access_token": {at}}.Encode(), nil,
		map[string]string{"userid": byUnion.Result.UserID}, &det); err != nil {
		return nil, fmt.Errorf("获取钉钉通讯录详情失败：%w", err)
	}
	d := det.Result
	attrs := map[string][]string{"userid": {byUnion.Result.UserID}, "unionid": {me.UnionID}, "name": {firstNonEmpty(d.Name, me.Nick)},
		"email": {firstNonEmpty(d.Email, me.Email)}, "org_email": {d.OrgEmail}, "mobile": {firstNonEmpty(d.Mobile, me.Mobile)},
		"title": {d.Title}, "job_number": {d.JobNumber}}
	for _, id := range d.DeptIDs {
		attrs["dept_id_list"] = append(attrs["dept_id_list"], fmt.Sprint(id))
		attrs["department"] = append(attrs["department"], fmt.Sprint(id))
	}
	return mapIdentity(p, me.UnionID, attrs, "ding", byUnion.Result.UserID, true), nil
}

func (f *dingFlow) Check(ctx context.Context, p *Provider) (string, error) {
	if err := f.validate(p); err != nil {
		return "", err
	}
	// 用 AppKey/AppSecret 取一次企业应用令牌：能验证凭据是否有效（不涉及任何用户）。
	f.apps.del(p.Config.ClientID)
	if _, err := f.appToken(ctx, p); err != nil {
		return "", err
	}
	return "AppKey / AppSecret 有效，已取得企业应用令牌。授权回调地址需在钉钉开放平台里登记；点“测试登录”可走一遍完整授权。", nil
}

// ---------- 飞书 / Lark ----------

type feishuFlow struct{ env *flowEnv }

func newFeishuFlow(env *flowEnv) *feishuFlow { return &feishuFlow{env: env} }

type feishuHosts struct{ Accounts, Open string }

func feishuSite(c Config) feishuHosts {
	if c.Site == "lark" {
		return feishuHosts{"https://accounts.larksuite.com", "https://open.larksuite.com"}
	}
	return feishuHosts{"https://accounts.feishu.cn", "https://open.feishu.cn"}
}

func (f *feishuFlow) validate(p *Provider) error {
	if s := p.Config.Site; s != "" && s != "feishu" && s != "lark" {
		return errors.New("站点只能是飞书或 Lark")
	}
	return requireFields("App ID", p.Config.ClientID, "App Secret", p.Config.ClientSecret)
}

func (f *feishuFlow) Begin(_ context.Context, p *Provider, r BeginReq) (string, error) {
	if err := f.validate(p); err != nil {
		return "", err
	}
	q := url.Values{"client_id": {p.Config.ClientID}, "redirect_uri": {r.RedirectURI}, "response_type": {"code"}, "state": {r.State},
		"code_challenge": {oauth2.S256ChallengeFromVerifier(r.Verifier)}, "code_challenge_method": {"S256"}}
	return authURL(feishuSite(p.Config).Accounts+"/open-apis/authen/v1/authorize", q), nil
}

func (f *feishuFlow) Complete(ctx context.Context, p *Provider, r CompleteReq) (*Identity, error) {
	if err := f.validate(p); err != nil {
		return nil, err
	}
	if e := r.Query.Get("error"); e != "" {
		return nil, fmt.Errorf("身份提供方拒绝了登录（error=%s）", sanitizeCode(e))
	}
	code := r.Query.Get("code")
	if code == "" {
		return nil, errors.New("回调缺少授权码")
	}
	host := feishuSite(p.Config).Open
	hc := f.env.HTTP(false)
	var tok struct {
		Code        int    `json:"code"`
		AccessToken string `json:"access_token"`
	}
	if err := postJSON(ctx, hc, host+"/open-apis/authen/v2/oauth/token", nil, map[string]string{
		"grant_type": "authorization_code", "client_id": p.Config.ClientID, "client_secret": p.Config.ClientSecret,
		"code": code, "redirect_uri": r.RedirectURI, "code_verifier": r.Verifier}, &tok); err != nil {
		return nil, fmt.Errorf("换取飞书令牌失败：%w", err)
	}
	if tok.Code != 0 || tok.AccessToken == "" {
		return nil, fmt.Errorf("换取飞书令牌失败（code=%d）", tok.Code)
	}
	var info struct {
		Code int `json:"code"`
		Data struct {
			OpenID          string `json:"open_id"`
			UnionID         string `json:"union_id"`
			UserID          string `json:"user_id"`
			Name            string `json:"name"`
			EnName          string `json:"en_name"`
			Email           string `json:"email"`
			EnterpriseEmail string `json:"enterprise_email"`
			Mobile          string `json:"mobile"`
			TenantKey       string `json:"tenant_key"`
		} `json:"data"`
	}
	if err := getJSON(ctx, hc, host+"/open-apis/authen/v1/user_info", map[string]string{"Authorization": "Bearer " + tok.AccessToken}, &info); err != nil {
		return nil, fmt.Errorf("获取飞书用户信息失败：%w", err)
	}
	d := info.Data
	if info.Code != 0 || d.OpenID == "" {
		return nil, fmt.Errorf("获取飞书用户信息失败（code=%d）", info.Code)
	}
	if t := strings.TrimSpace(p.Config.TenantKey); t != "" && d.TenantKey != t {
		return nil, errors.New("不是允许的飞书租户")
	}
	attrs := map[string][]string{"open_id": {d.OpenID}, "union_id": {d.UnionID}, "user_id": {d.UserID}, "name": {d.Name}, "en_name": {d.EnName},
		"email": {d.Email}, "enterprise_email": {d.EnterpriseEmail}, "mobile": {d.Mobile}, "tenant_key": {d.TenantKey}}
	return mapIdentity(p, d.OpenID, attrs, "feishu", d.OpenID, true), nil
}

func (f *feishuFlow) Check(ctx context.Context, p *Provider) (string, error) {
	if err := f.validate(p); err != nil {
		return "", err
	}
	// 用 App ID / App Secret 换一次 tenant_access_token：验证应用凭据。
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	host := feishuSite(p.Config).Open
	if err := postJSON(ctx, f.env.HTTP(false), host+"/open-apis/auth/v3/tenant_access_token/internal", nil,
		map[string]string{"app_id": p.Config.ClientID, "app_secret": p.Config.ClientSecret}, &out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("App ID / App Secret 无效（code=%d）", out.Code)
	}
	site := "飞书"
	if p.Config.Site == "lark" {
		site = "Lark"
	}
	return fmt.Sprintf("应用凭据有效（%s）。授权回调地址需在开发者后台“安全设置”里登记；点“测试登录”可走一遍完整授权。", site), nil
}

// ---------- 企业微信 ----------

type wecomFlow struct {
	env  *flowEnv
	toks *ttlCache[string]
}

func newWeComFlow(env *flowEnv) *wecomFlow {
	return &wecomFlow{env: env, toks: newTTLCache[string](env.Now)}
}

const (
	wecomAPIBase  = "https://qyapi.weixin.qq.com"
	wecomQRBase   = "https://login.work.weixin.qq.com/wwlogin/sso/login"
	wecomOAuthURL = "https://open.weixin.qq.com/connect/oauth2/authorize"
)

func (f *wecomFlow) validate(p *Provider) error {
	if m := p.Config.WeComMode; m != "" && m != "auto" && m != "qr" {
		return errors.New("授权方式只能是 auto 或 qr")
	}
	return requireFields("企业 CorpId", p.Config.CorpID, "应用 AgentId", p.Config.AgentID, "应用 Secret", p.Config.ClientSecret)
}

func (f *wecomFlow) Begin(_ context.Context, p *Provider, r BeginReq) (string, error) {
	if err := f.validate(p); err != nil {
		return "", err
	}
	c := p.Config
	if orDefault(c.WeComMode, "auto") == "auto" && strings.Contains(strings.ToLower(r.UserAgent), "wxwork") {
		// 企业微信内置浏览器：免登授权
		q := url.Values{"appid": {c.CorpID}, "redirect_uri": {r.RedirectURI}, "response_type": {"code"}, "scope": {"snsapi_base"},
			"state": {r.State}, "agentid": {c.AgentID}}
		return wecomOAuthURL + "?" + q.Encode() + "#wechat_redirect", nil
	}
	q := url.Values{"login_type": {"CorpApp"}, "appid": {c.CorpID}, "agentid": {c.AgentID}, "redirect_uri": {r.RedirectURI}, "state": {r.State}}
	return authURL(wecomQRBase, q), nil
}

func (f *wecomFlow) token(ctx context.Context, p *Provider, fresh bool) (string, error) {
	key := p.Config.CorpID + "|" + p.Config.AgentID
	if !fresh {
		if t, ok := f.toks.get(key); ok {
			return t, nil
		}
	}
	var out struct {
		ErrCode int    `json:"errcode"`
		Token   string `json:"access_token"`
		Expires int    `json:"expires_in"`
	}
	u := wecomAPIBase + "/cgi-bin/gettoken?" + url.Values{"corpid": {p.Config.CorpID}, "corpsecret": {p.Config.ClientSecret}}.Encode()
	if err := getJSON(ctx, f.env.HTTP(false), u, nil, &out); err != nil {
		return "", fmt.Errorf("获取企业微信令牌失败：%w", err)
	}
	if out.ErrCode != 0 || out.Token == "" {
		return "", fmt.Errorf("获取企业微信令牌失败（errcode=%d），请检查 CorpId / 应用 Secret", out.ErrCode)
	}
	ttl := time.Duration(out.Expires-120) * time.Second
	if ttl <= 0 {
		ttl = time.Minute
	}
	f.toks.set(key, out.Token, ttl)
	return out.Token, nil
}

func (f *wecomFlow) Complete(ctx context.Context, p *Provider, r CompleteReq) (*Identity, error) {
	if err := f.validate(p); err != nil {
		return nil, err
	}
	code := r.Query.Get("code")
	if code == "" {
		return nil, errors.New("回调缺少授权码")
	}
	at, err := f.token(ctx, p, false)
	if err != nil {
		return nil, err
	}
	hc := f.env.HTTP(false)
	var who struct {
		ErrCode int    `json:"errcode"`
		UserID  string `json:"userid"`
		OpenID  string `json:"openid"`
	}
	if err := getJSON(ctx, hc, wecomAPIBase+"/cgi-bin/auth/getuserinfo?"+url.Values{"access_token": {at}, "code": {code}}.Encode(), nil, &who); err != nil {
		return nil, fmt.Errorf("获取企业微信用户失败：%w", err)
	}
	if who.ErrCode != 0 {
		return nil, fmt.Errorf("获取企业微信用户失败（errcode=%d）", who.ErrCode)
	}
	if who.UserID == "" {
		return nil, errors.New("该账号不是本企业成员") // 企业外成员只会返回 openid
	}
	attrs := map[string][]string{"userid": {who.UserID}}
	var det struct {
		ErrCode    int    `json:"errcode"`
		Name       string `json:"name"`
		Email      string `json:"email"`
		BizMail    string `json:"biz_mail"`
		Mobile     string `json:"mobile"`
		Position   string `json:"position"`
		Department []int  `json:"department"`
	}
	// 通讯录详情需要应用有“通讯录”可见范围与权限；拿不到时只用 userid 登录，不让整个登录失败。
	if err := getJSON(ctx, hc, wecomAPIBase+"/cgi-bin/user/get?"+url.Values{"access_token": {at}, "userid": {who.UserID}}.Encode(), nil, &det); err == nil && det.ErrCode == 0 {
		attrs["name"], attrs["email"], attrs["biz_mail"] = []string{det.Name}, []string{det.Email}, []string{det.BizMail}
		attrs["mobile"], attrs["position"] = []string{det.Mobile}, []string{det.Position}
		for _, d := range det.Department {
			attrs["department"] = append(attrs["department"], fmt.Sprint(d))
		}
	}
	return mapIdentity(p, who.UserID, attrs, "wecom", who.UserID, true), nil
}

func (f *wecomFlow) Check(ctx context.Context, p *Provider) (string, error) {
	if err := f.validate(p); err != nil {
		return "", err
	}
	if _, err := f.token(ctx, p, true); err != nil {
		return "", err
	}
	return "CorpId / 应用 Secret 有效。可信域名与授权回调域需在企业微信管理后台登记；点“测试登录”可走一遍完整授权。", nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
