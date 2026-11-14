// @author: kevinkelly
// @license: Apache Licence
// @file: flow.go
// @time: 2026/10/28

package authx

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Flow 是需要浏览器跳转的登录方式（OIDC / 钉钉 / 飞书 / Slack / 企业微信 / CAS）。
// 各家 HTTP 交互都走 flowEnv 里可注入的 http.Client，单测用 httptest 假 IdP。
type Flow interface {
	// Begin 返回要把浏览器带去的授权地址。
	Begin(ctx context.Context, p *Provider, r BeginReq) (string, error)
	// Complete 处理 IdP 回跳：校验、换取令牌、取用户信息，返回已映射的身份。
	Complete(ctx context.Context, p *Provider, r CompleteReq) (*Identity, error)
	// Check 校验必填项并探测 IdP（测试连接），返回给管理员看的说明。不涉及任何用户凭据。
	Check(ctx context.Context, p *Provider) (string, error)
}

// BeginReq 是发起登录时的上下文。
type BeginReq struct {
	RedirectURI string // 回调地址（平台自己的 /login/sso/:id/callback）
	State       string
	Nonce       string
	Verifier    string // PKCE code_verifier
	UserAgent   string
}

// CompleteReq 是回调时的上下文；State/Nonce/Verifier 是发起时存在 Cookie 里的值（State 已与回跳参数核对过）。
type CompleteReq struct {
	RedirectURI string
	Query       url.Values
	State       string
	Nonce       string
	Verifier    string
}

// flowEnv 是各 Flow 共用的环境。
type flowEnv struct {
	HTTP func(skipVerify bool) *http.Client
	Now  func() time.Time
}

// newHTTPFactory 返回生产环境用的 http.Client 工厂：有超时、不跟随重定向、TLS ≥ 1.2。
func newHTTPFactory() func(bool) *http.Client {
	mk := func(skip bool) *http.Client {
		tr := &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 8 * time.Second}).DialContext,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: skip}, //nolint:gosec // 管理员显式选择“不校验证书”
			TLSHandshakeTimeout: 8 * time.Second,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     60 * time.Second,
		}
		return &http.Client{Transport: tr, Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	secure, insecure := mk(false), mk(true)
	return func(skip bool) *http.Client {
		if skip {
			return insecure
		}
		return secure
	}
}

// fixedHTTP 让所有请求用同一个 client（测试注入）。
func fixedHTTP(c *http.Client) func(bool) *http.Client { return func(bool) *http.Client { return c } }

const maxBody = 1 << 20 // IdP 响应最大读取 1MB

// cleanErr 去掉 url.Error 里带的完整 URL（回调的 code、access_token 可能在查询串里），只留主机名。
func cleanErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		host := ""
		if u, e := url.Parse(ue.URL); e == nil {
			host = u.Host
		}
		return fmt.Errorf("%s %s: %v", ue.Op, host, ue.Err)
	}
	return err
}

// checkURL 要求 https；本机回环地址（开发 / 测试）允许 http。
func checkURL(raw, what string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%s 不是合法的 URL", what)
	}
	switch u.Scheme {
	case "https":
	case "http":
		h := u.Hostname()
		if ip := net.ParseIP(h); !(h == "localhost" || (ip != nil && ip.IsLoopback())) {
			return nil, fmt.Errorf("%s 必须使用 https", what)
		}
	default:
		return nil, fmt.Errorf("%s 必须使用 https", what)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%s 不能包含用户名密码", what)
	}
	return u, nil
}

// httpResult 是一次 JSON 请求的结果。
type httpResult struct {
	Status int
	Body   []byte
}

// doRequest 发请求并读取（限长的）响应体。body 为 nil、url.Values（表单）或其他（JSON）。
func doRequest(ctx context.Context, c *http.Client, method, rawURL string, hdr map[string]string, body any) (*httpResult, error) {
	var rd io.Reader
	ctype := ""
	switch b := body.(type) {
	case nil:
	case url.Values:
		rd, ctype = strings.NewReader(b.Encode()), "application/x-www-form-urlencoded"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		rd, ctype = bytes.NewReader(raw), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, cleanErr(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, cleanErr(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("%w: 读取响应失败", ErrUnavailable)
	}
	return &httpResult{Status: resp.StatusCode, Body: raw}, nil
}

// getJSON / postJSON 请求并把 2xx 响应的 JSON 解到 out。非 2xx 返回错误（不带响应体，避免泄漏）。
func getJSON(ctx context.Context, c *http.Client, rawURL string, hdr map[string]string, out any) error {
	return reqJSON(ctx, c, http.MethodGet, rawURL, hdr, nil, out)
}

func postJSON(ctx context.Context, c *http.Client, rawURL string, hdr map[string]string, body, out any) error {
	return reqJSON(ctx, c, http.MethodPost, rawURL, hdr, body, out)
}

func reqJSON(ctx context.Context, c *http.Client, method, rawURL string, hdr map[string]string, body, out any) error {
	res, err := doRequest(ctx, c, method, rawURL, hdr, body)
	if err != nil {
		return err
	}
	if res.Status < 200 || res.Status > 299 {
		return fmt.Errorf("%w: HTTP %d", ErrUnavailable, res.Status)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(res.Body, out); err != nil {
		return fmt.Errorf("响应不是合法的 JSON")
	}
	return nil
}

// ---- 简单的带过期缓存（IdP 的 access_token、discovery、JWKS 共用）----

type ttlCache[V any] struct {
	mu  sync.Mutex
	m   map[string]ttlItem[V]
	now func() time.Time
}

type ttlItem[V any] struct {
	v   V
	exp time.Time
}

func newTTLCache[V any](now func() time.Time) *ttlCache[V] {
	return &ttlCache[V]{m: map[string]ttlItem[V]{}, now: now}
}

func (c *ttlCache[V]) get(k string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.m[k]
	if !ok || !c.now().Before(it.exp) {
		delete(c.m, k)
		var z V
		return z, false
	}
	return it.v, true
}

func (c *ttlCache[V]) set(k string, v V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) > 256 { // 防止无限增长：简单清掉过期项
		for kk, it := range c.m {
			if !c.now().Before(it.exp) {
				delete(c.m, kk)
			}
		}
	}
	c.m[k] = ttlItem[V]{v, c.now().Add(ttl)}
}

func (c *ttlCache[V]) del(k string) {
	c.mu.Lock()
	delete(c.m, k)
	c.mu.Unlock()
}

// toAttrs 把 JSON 解出来的 map 拍平成 属性 → 字符串列表（字符串、数字、布尔、字符串数组；嵌套对象忽略）。
func toAttrs(m map[string]any) map[string][]string {
	out := map[string][]string{}
	for k, v := range m {
		switch t := v.(type) {
		case string:
			out[k] = []string{t}
		case bool:
			out[k] = []string{fmt.Sprint(t)}
		case float64:
			if t == float64(int64(t)) {
				out[k] = []string{fmt.Sprint(int64(t))}
			} else {
				out[k] = []string{fmt.Sprint(t)}
			}
		case json.Number:
			out[k] = []string{t.String()}
		case []any:
			var vs []string
			for _, e := range t {
				switch x := e.(type) {
				case string:
					vs = append(vs, x)
				case float64:
					vs = append(vs, fmt.Sprint(int64(x)))
				}
			}
			if len(vs) > 0 {
				out[k] = vs
			}
		}
	}
	return out
}

// requireFields 检查必填项，缺少时给出中文提示。
func requireFields(pairs ...string) error {
	var miss []string
	for i := 0; i+1 < len(pairs); i += 2 {
		if strings.TrimSpace(pairs[i+1]) == "" {
			miss = append(miss, pairs[i])
		}
	}
	if len(miss) > 0 {
		return fmt.Errorf("缺少必填项：%s", strings.Join(miss, "、"))
	}
	return nil
}

// authURL 在授权地址上追加查询参数（保留地址里已有的参数）。
func authURL(base string, params url.Values) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	for k, vs := range params {
		q[k] = vs
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// limitBody 限制读取 IdP / 短信服务响应的最大长度。
func limitBody(r io.Reader) io.Reader { return io.LimitReader(r, maxBody) }
