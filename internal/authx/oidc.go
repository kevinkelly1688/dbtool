// @author: kevinkelly
// @license: Apache Licence
// @file: oidc.go
// @time: 2026/10/28

package authx

import (
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
)

// OIDC 授权码流程 + PKCE + state/nonce，自己做 discovery 与 id_token（RS256 / ES256）签名校验，
// 不引入 go-oidc：签名算法固定为管理员配置的那一个（防算法混淆），密钥只取 JWKS 里与算法匹配的类型。
// Slack（Sign in with Slack = OpenID Connect）复用同一套实现，只是端点固定。

type oidcMeta struct {
	Issuer      string `json:"issuer"`
	AuthURL     string `json:"authorization_endpoint"`
	TokenURL    string `json:"token_endpoint"`
	UserinfoURL string `json:"userinfo_endpoint"`
	JWKSURL     string `json:"jwks_uri"`
}

type jwkEntry struct {
	Kid string
	Alg string
	Use string
	Key crypto.PublicKey
}

type jwkSet struct {
	keys    []jwkEntry
	fetched time.Time
}

// oidcParams 是 OIDC 核心实现需要的参数（OIDC 与 Slack 各自从 Config 转换而来）。
type oidcParams struct {
	Wellknown    string
	ClientID     string
	ClientSecret string
	Scopes       []string
	Alg          string
	SkipVerify   bool
	AuthExtra    []oauth2.AuthCodeOption
}

type oidcCore struct {
	env  *flowEnv
	meta *ttlCache[*oidcMeta]
	jwks *ttlCache[*jwkSet]

	mu sync.Mutex // 串行化 JWKS 刷新，避免并发回调时重复拉取
}

func newOIDCCore(env *flowEnv) *oidcCore {
	return &oidcCore{env: env, meta: newTTLCache[*oidcMeta](env.Now), jwks: newTTLCache[*jwkSet](env.Now)}
}

const (
	oidcMetaTTL = 10 * time.Minute
	oidcJWKSTTL = 10 * time.Minute
)

func (p oidcParams) validate() error {
	if err := requireFields("Wellknown URL", p.Wellknown, "Client ID", p.ClientID, "Client Secret", p.ClientSecret); err != nil {
		return err
	}
	if p.Alg != "RS256" && p.Alg != "ES256" {
		return errors.New("签名算法只支持 RS256 或 ES256")
	}
	hasOpenID := false
	for _, s := range p.Scopes {
		hasOpenID = hasOpenID || s == "openid"
	}
	if !hasOpenID {
		return errors.New("Scopes 必须包含 openid")
	}
	return nil
}

func splitScopes(s, def string) []string {
	f := strings.Fields(strings.ReplaceAll(s, ",", " "))
	if len(f) == 0 {
		f = strings.Fields(def)
	}
	return f
}

// discover 取（带缓存的）discovery 文档并校验。fresh=true 时绕过缓存（测试连接）。
func (o *oidcCore) discover(ctx context.Context, p oidcParams, fresh bool) (*oidcMeta, error) {
	if !fresh {
		if m, ok := o.meta.get(p.Wellknown); ok {
			return m, nil
		}
	}
	wk, err := checkURL(p.Wellknown, "Wellknown URL")
	if err != nil {
		return nil, err
	}
	var m oidcMeta
	if err := getJSON(ctx, o.env.HTTP(p.SkipVerify), wk.String(), nil, &m); err != nil {
		return nil, fmt.Errorf("读取 discovery 文档失败：%w", err)
	}
	if m.Issuer == "" || m.AuthURL == "" || m.TokenURL == "" || m.JWKSURL == "" {
		return nil, errors.New("discovery 文档缺少 issuer / authorization_endpoint / token_endpoint / jwks_uri")
	}
	// 规范要求 issuer 必须与 discovery 地址的前缀一致，防止被指向别的 IdP。
	if base, ok := strings.CutSuffix(wk.Path, "/.well-known/openid-configuration"); ok {
		want := wk.Scheme + "://" + wk.Host + base
		if strings.TrimRight(m.Issuer, "/") != strings.TrimRight(want, "/") {
			return nil, errors.New("discovery 文档里的 issuer 与 Wellknown URL 不一致")
		}
	}
	for what, raw := range map[string]string{"authorization_endpoint": m.AuthURL, "token_endpoint": m.TokenURL, "jwks_uri": m.JWKSURL} {
		if _, err := checkURL(raw, what); err != nil {
			return nil, err
		}
	}
	if m.UserinfoURL != "" {
		if _, err := checkURL(m.UserinfoURL, "userinfo_endpoint"); err != nil {
			return nil, err
		}
	}
	o.meta.set(p.Wellknown, &m, oidcMetaTTL)
	return &m, nil
}

func (o *oidcCore) oauthConf(p oidcParams, m *oidcMeta, redirect string) *oauth2.Config {
	return &oauth2.Config{
		ClientID: p.ClientID, ClientSecret: p.ClientSecret, RedirectURL: redirect, Scopes: p.Scopes,
		Endpoint: oauth2.Endpoint{AuthURL: m.AuthURL, TokenURL: m.TokenURL, AuthStyle: oauth2.AuthStyleAutoDetect},
	}
}

func (o *oidcCore) begin(ctx context.Context, p oidcParams, r BeginReq) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	if r.State == "" || r.Nonce == "" || r.Verifier == "" {
		return "", errors.New("内部错误：缺少 state / nonce / PKCE")
	}
	m, err := o.discover(ctx, p, false)
	if err != nil {
		return "", err
	}
	opts := append([]oauth2.AuthCodeOption{oauth2.S256ChallengeOption(r.Verifier), oauth2.SetAuthURLParam("nonce", r.Nonce)}, p.AuthExtra...)
	return o.oauthConf(p, m, r.RedirectURI).AuthCodeURL(r.State, opts...), nil
}

// oidcResult 是回调处理的结果：校验过的 id_token 声明（必要时合并了 userinfo）。
type oidcResult struct {
	Claims map[string]any
}

func (o *oidcCore) complete(ctx context.Context, p oidcParams, r CompleteReq, needUserinfo func(map[string]any) bool) (*oidcResult, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	q := r.Query
	if e := q.Get("error"); e != "" {
		return nil, fmt.Errorf("身份提供方拒绝了登录（error=%s）", sanitizeCode(e))
	}
	code := q.Get("code")
	if code == "" {
		return nil, errors.New("回调缺少授权码")
	}
	if r.Nonce == "" || r.Verifier == "" {
		return nil, errors.New("内部错误：缺少 nonce / PKCE")
	}
	m, err := o.discover(ctx, p, false)
	if err != nil {
		return nil, err
	}
	hc := o.env.HTTP(p.SkipVerify)
	cctx := context.WithValue(ctx, oauth2.HTTPClient, hc)
	tok, err := o.oauthConf(p, m, r.RedirectURI).Exchange(cctx, code, oauth2.VerifierOption(r.Verifier))
	if err != nil {
		return nil, fmt.Errorf("换取令牌失败：%s", retrieveErrText(err))
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, errors.New("令牌响应里没有 id_token")
	}
	claims, err := o.verifyIDToken(ctx, raw, m, p, r.Nonce)
	if err != nil {
		return nil, err
	}
	if m.UserinfoURL != "" && tok.AccessToken != "" && (needUserinfo == nil || needUserinfo(claims)) {
		var info map[string]any
		if err := getJSON(ctx, hc, m.UserinfoURL, map[string]string{"Authorization": "Bearer " + tok.AccessToken}, &info); err == nil {
			// userinfo 的 sub 必须与 id_token 一致才能合并（规范要求）；已有的声明不被覆盖。
			if s, _ := info["sub"].(string); s == claims["sub"] {
				for k, v := range info {
					if _, ok := claims[k]; !ok {
						claims[k] = v
					}
				}
			}
		}
	}
	return &oidcResult{Claims: claims}, nil
}

// retrieveErrText 只取 OAuth 错误码，不输出响应体（可能含敏感信息）。
func retrieveErrText(err error) string {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.ErrorCode != "" {
			return fmt.Sprintf("HTTP %d，error=%s", re.Response.StatusCode, sanitizeCode(re.ErrorCode))
		}
		return fmt.Sprintf("HTTP %d", re.Response.StatusCode)
	}
	return cleanErr(err).Error()
}

// sanitizeCode 把 IdP 回传的错误码限制为安全的短字符串（会进日志与测试结果）。
func sanitizeCode(s string) string {
	if len(s) > 40 {
		s = s[:40]
	}
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// verifyIDToken 校验 id_token 的签名与声明：算法（固定）、签名（JWKS）、iss、aud、azp、exp、iat、nonce、sub。
func (o *oidcCore) verifyIDToken(ctx context.Context, raw string, m *oidcMeta, p oidcParams, nonce string) (map[string]any, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{p.Alg}),
		jwt.WithIssuer(m.Issuer),
		jwt.WithAudience(p.ClientID),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(time.Minute),
		jwt.WithTimeFunc(o.env.Now),
	)
	claims := jwt.MapClaims{}
	_, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return o.findKey(ctx, m.JWKSURL, kid, p)
	})
	if err != nil {
		return nil, fmt.Errorf("id_token 校验失败：%s", jwtErrText(err))
	}
	got, _ := claims["nonce"].(string)
	if nonce == "" || got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(nonce)) != 1 {
		return nil, errors.New("id_token 校验失败：nonce 不匹配")
	}
	if sub, _ := claims["sub"].(string); strings.TrimSpace(sub) == "" {
		return nil, errors.New("id_token 校验失败：缺少 sub")
	}
	// 多个 audience 时，规范要求有 azp 且等于 client_id。
	if aud, ok := claims["aud"].([]any); ok && len(aud) > 1 {
		if azp, _ := claims["azp"].(string); azp != p.ClientID {
			return nil, errors.New("id_token 校验失败：azp 不匹配")
		}
	}
	return map[string]any(claims), nil
}

func jwtErrText(err error) string {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return "令牌已过期"
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return "签名无效"
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return "issuer 不匹配"
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return "audience 不匹配"
	case errors.Is(err, jwt.ErrTokenUsedBeforeIssued):
		return "签发时间在未来"
	case errors.Is(err, jwt.ErrTokenUnverifiable):
		return "无法验证签名"
	case errors.Is(err, jwt.ErrTokenMalformed):
		return "格式错误"
	case errors.Is(err, ErrUnavailable):
		return "无法获取签名密钥"
	}
	return "令牌不合法"
}

// findKey 在 JWKS 里按 kid 找验签公钥；找不到且缓存不新鲜时刷新一次。
func (o *oidcCore) findKey(ctx context.Context, jwksURL, kid string, p oidcParams) (any, error) {
	pick := func(set *jwkSet) (crypto.PublicKey, bool) {
		var cand []jwkEntry
		for _, k := range set.keys {
			if k.Use != "" && k.Use != "sig" {
				continue
			}
			if k.Alg != "" && k.Alg != p.Alg {
				continue
			}
			switch p.Alg {
			case "RS256":
				if _, ok := k.Key.(*rsa.PublicKey); !ok {
					continue
				}
			case "ES256":
				if _, ok := k.Key.(*ecdsa.PublicKey); !ok {
					continue
				}
			}
			if kid != "" && k.Kid != kid {
				continue
			}
			cand = append(cand, k)
		}
		if kid == "" && len(cand) != 1 { // 令牌没带 kid 时，只有唯一候选才接受
			return nil, false
		}
		if len(cand) == 0 {
			return nil, false
		}
		return cand[0].Key, true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	set, ok := o.jwks.get(jwksURL)
	if ok {
		if k, ok := pick(set); ok {
			return k, nil
		}
		// 缓存里没有：密钥可能刚轮换。至少间隔 30 秒才再拉，防止被伪造 kid 打爆 IdP。
		if o.env.Now().Sub(set.fetched) < 30*time.Second {
			return nil, errors.New("找不到对应的签名密钥")
		}
	}
	set, err := o.fetchJWKS(ctx, jwksURL, p.SkipVerify)
	if err != nil {
		return nil, err
	}
	o.jwks.set(jwksURL, set, oidcJWKSTTL)
	if k, ok := pick(set); ok {
		return k, nil
	}
	return nil, errors.New("找不到对应的签名密钥")
}

func (o *oidcCore) fetchJWKS(ctx context.Context, jwksURL string, skip bool) (*jwkSet, error) {
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			Crv string `json:"crv"`
			N   string `json:"n"`
			E   string `json:"e"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := getJSON(ctx, o.env.HTTP(skip), jwksURL, nil, &doc); err != nil {
		return nil, fmt.Errorf("获取 JWKS 失败：%w", err)
	}
	set := &jwkSet{fetched: o.env.Now()}
	for _, k := range doc.Keys {
		var pub crypto.PublicKey
		var err error
		switch k.Kty {
		case "RSA":
			pub, err = rsaKey(k.N, k.E)
		case "EC":
			pub, err = ecKey(k.Crv, k.X, k.Y)
		default:
			continue
		}
		if err != nil {
			continue // 坏密钥忽略，不影响其他密钥
		}
		set.keys = append(set.keys, jwkEntry{Kid: k.Kid, Alg: k.Alg, Use: k.Use, Key: pub})
	}
	return set, nil
}

func b64u(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func rsaKey(n, e string) (*rsa.PublicKey, error) {
	nb, err := b64u(n)
	if err != nil {
		return nil, err
	}
	eb, err := b64u(e)
	if err != nil || len(eb) == 0 || len(eb) > 4 {
		return nil, errors.New("bad e")
	}
	ei := 0
	for _, b := range eb {
		ei = ei<<8 | int(b)
	}
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: ei}
	if pub.N.BitLen() < 2048 || ei < 3 || ei%2 == 0 {
		return nil, errors.New("RSA 密钥强度不足")
	}
	return pub, nil
}

func ecKey(crv, x, y string) (*ecdsa.PublicKey, error) {
	if crv != "P-256" {
		return nil, errors.New("只支持 P-256")
	}
	xb, err := b64u(x)
	if err != nil {
		return nil, err
	}
	yb, err := b64u(y)
	if err != nil {
		return nil, err
	}
	if len(xb) > 32 || len(yb) > 32 {
		return nil, errors.New("bad point")
	}
	buf := make([]byte, 65)
	buf[0] = 4
	copy(buf[1+32-len(xb):33], xb)
	copy(buf[33+32-len(yb):], yb)
	if _, err := ecdh.P256().NewPublicKey(buf); err != nil { // 校验点在曲线上
		return nil, err
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)}, nil
}

// check 校验配置并探测 IdP（绕过缓存重新拉 discovery 与 JWKS）。
func (o *oidcCore) check(ctx context.Context, p oidcParams) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	m, err := o.discover(ctx, p, true)
	if err != nil {
		return "", err
	}
	o.mu.Lock()
	set, err := o.fetchJWKS(ctx, m.JWKSURL, p.SkipVerify)
	if err == nil {
		o.jwks.set(m.JWKSURL, set, oidcJWKSTTL)
	}
	o.mu.Unlock()
	if err != nil {
		return "", err
	}
	n := 0
	for _, k := range set.keys {
		switch p.Alg {
		case "RS256":
			if _, ok := k.Key.(*rsa.PublicKey); ok {
				n++
			}
		case "ES256":
			if _, ok := k.Key.(*ecdsa.PublicKey); ok {
				n++
			}
		}
	}
	if n == 0 {
		return "", fmt.Errorf("JWKS 里没有可用的 %s 签名密钥", p.Alg)
	}
	if m.UserinfoURL == "" {
		return fmt.Sprintf("discovery 正常，issuer=%s，取得 %d 个 %s 签名密钥（没有 userinfo 端点，姓名 / 邮箱只能来自 id_token）", m.Issuer, n, p.Alg), nil
	}
	return fmt.Sprintf("discovery 正常，issuer=%s，取得 %d 个 %s 签名密钥", m.Issuer, n, p.Alg), nil
}

// ---- OIDC 登录方式 ----

type oidcFlow struct{ core *oidcCore }

func newOIDCFlow(core *oidcCore) *oidcFlow { return &oidcFlow{core: core} }

func oidcParamsOf(p *Provider) oidcParams {
	c := p.Config
	return oidcParams{
		Wellknown: strings.TrimSpace(c.WellKnownURL), ClientID: strings.TrimSpace(c.ClientID), ClientSecret: c.ClientSecret,
		Scopes: splitScopes(c.Scopes, "openid profile email"), Alg: orDefault(c.SignAlg, "RS256"), SkipVerify: c.SkipVerify,
	}
}

func (f *oidcFlow) Begin(ctx context.Context, p *Provider, r BeginReq) (string, error) {
	return f.core.begin(ctx, oidcParamsOf(p), r)
}

func (f *oidcFlow) Check(ctx context.Context, p *Provider) (string, error) {
	return f.core.check(ctx, oidcParamsOf(p))
}

func (f *oidcFlow) Complete(ctx context.Context, p *Provider, r CompleteReq) (*Identity, error) {
	du, dd, de := defaultAttrSpec(p.Kind)
	need := func(c map[string]any) bool {
		a := withDerived(toAttrs(c))
		return pickAttr(a, orDefault(p.Config.AttrUsername, du)) == "" || pickAttr(a, orDefault(p.Config.AttrDisplay, dd)) == "" ||
			pickAttr(a, orDefault(p.Config.AttrEmail, de)) == ""
	}
	res, err := f.core.complete(ctx, oidcParamsOf(p), r, need)
	if err != nil {
		return nil, err
	}
	sub, _ := res.Claims["sub"].(string)
	verified := true
	switch v := res.Claims["email_verified"].(type) {
	case bool:
		verified = v
	case string:
		verified = strings.EqualFold(v, "true")
	}
	return mapIdentity(p, sub, toAttrs(res.Claims), "", "", verified), nil
}

// ---- Slack（Sign in with Slack，OpenID Connect）----

const (
	slackWellKnown = "https://slack.com/.well-known/openid-configuration"
	slackTeamClaim = "https://slack.com/team_id"
	slackUserClaim = "https://slack.com/user_id"
)

type slackFlow struct{ core *oidcCore }

func newSlackFlow(core *oidcCore) *slackFlow { return &slackFlow{core: core} }

func slackParams(p *Provider) oidcParams {
	c := p.Config
	op := oidcParams{Wellknown: slackWellKnown, ClientID: strings.TrimSpace(c.ClientID), ClientSecret: c.ClientSecret,
		Scopes: []string{"openid", "profile", "email"}, Alg: "RS256"}
	if t := strings.TrimSpace(c.TeamID); t != "" {
		op.AuthExtra = append(op.AuthExtra, oauth2.SetAuthURLParam("team", t)) // 授权页直接限定工作区
	}
	return op
}

func (f *slackFlow) Begin(ctx context.Context, p *Provider, r BeginReq) (string, error) {
	return f.core.begin(ctx, slackParams(p), r)
}

func (f *slackFlow) Check(ctx context.Context, p *Provider) (string, error) {
	return f.core.check(ctx, slackParams(p))
}

func (f *slackFlow) Complete(ctx context.Context, p *Provider, r CompleteReq) (*Identity, error) {
	res, err := f.core.complete(ctx, slackParams(p), r, nil)
	if err != nil {
		return nil, err
	}
	if t := strings.TrimSpace(p.Config.TeamID); t != "" {
		if got, _ := res.Claims[slackTeamClaim].(string); got != t {
			return nil, errors.New("不是允许的 Slack 工作区")
		}
	}
	sub, _ := res.Claims["sub"].(string)
	uid, _ := res.Claims[slackUserClaim].(string)
	if uid == "" {
		uid = sub
	}
	attrs := toAttrs(res.Claims)
	attrs["slack_id"] = []string{uid}
	verified := true
	if v, ok := res.Claims["email_verified"].(bool); ok {
		verified = v
	}
	return mapIdentity(p, sub, attrs, "slack", uid, verified), nil
}
