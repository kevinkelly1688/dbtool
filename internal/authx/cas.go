// @author: kevinkelly
// @license: Apache Licence
// @file: cas.go
// @time: 2026/10/28

package authx

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// CAS 1 / 2 / 3：重定向到 CAS 登录页，回跳带 ticket，服务端向 CAS 校验 ticket。
// CAS 协议本身没有 state，这里把 state 放进 service URL 的查询串里（CAS 校验时要求 service 与登录时完全一致），
// 回跳时先核对它，避免登录 CSRF（把别人的 ticket 塞进受害者浏览器）。
//
// XML 解析：Go 的 encoding/xml 不解析外部实体也不展开自定义实体，这里仍然限制响应大小并拒绝任何 DOCTYPE / ENTITY。

type casFlow struct{ env *flowEnv }

func newCASFlow(env *flowEnv) *casFlow { return &casFlow{env: env} }

func casBase(c Config) (string, error) {
	u, err := checkURL(c.CASURL, "CAS 服务地址")
	if err != nil {
		return "", err
	}
	s := u.String()
	if !strings.HasSuffix(s, "/") {
		s += "/"
	}
	return s, nil
}

func casVersion(c Config) string {
	switch c.CASVersion {
	case "1", "2":
		return c.CASVersion
	}
	return "3"
}

// serviceURL 是登录与校验时共用的 service 参数。
func (f *casFlow) serviceURL(p *Provider, redirectURI, state string) (string, error) {
	u, err := url.Parse(redirectURI)
	if err != nil || u.Host == "" {
		return "", errors.New("回调地址不合法")
	}
	if p.Config.ForceHTTPS {
		u.Scheme = "https"
	}
	q := u.Query()
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (f *casFlow) Begin(_ context.Context, p *Provider, r BeginReq) (string, error) {
	base, err := casBase(p.Config)
	if err != nil {
		return "", err
	}
	svc, err := f.serviceURL(p, r.RedirectURI, r.State)
	if err != nil {
		return "", err
	}
	return authURL(base+"login", url.Values{"service": {svc}}), nil
}

type casAttr struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type casResponse struct {
	XMLName xml.Name `xml:"serviceResponse"`
	Success *struct {
		User       string `xml:"user"`
		Attributes struct {
			Items []casAttr `xml:",any"`
		} `xml:"attributes"`
	} `xml:"authenticationSuccess"`
	Failure *struct {
		Code string `xml:"code,attr"`
	} `xml:"authenticationFailure"`
}

// parseCASXML 解析 serviceValidate 响应。
func parseCASXML(body []byte) (user string, attrs map[string][]string, err error) {
	if len(body) > 256*1024 {
		return "", nil, errors.New("CAS 响应过大")
	}
	up := bytes.ToUpper(body)
	if bytes.Contains(up, []byte("<!DOCTYPE")) || bytes.Contains(up, []byte("<!ENTITY")) {
		return "", nil, errors.New("CAS 响应包含不允许的 DTD / 实体声明")
	}
	var r casResponse
	if err := xml.Unmarshal(body, &r); err != nil {
		return "", nil, errors.New("CAS 响应不是合法的 XML")
	}
	if r.Failure != nil {
		return "", nil, fmt.Errorf("CAS 拒绝了该 ticket（%s）", sanitizeCode(r.Failure.Code))
	}
	if r.Success == nil || strings.TrimSpace(r.Success.User) == "" {
		return "", nil, errors.New("CAS 响应里没有用户")
	}
	attrs = map[string][]string{}
	for _, a := range r.Success.Attributes.Items {
		if v := strings.TrimSpace(a.Value); v != "" {
			k := a.XMLName.Local
			attrs[k] = append(attrs[k], v)
		}
	}
	return strings.TrimSpace(r.Success.User), attrs, nil
}

func (f *casFlow) Complete(ctx context.Context, p *Provider, r CompleteReq) (*Identity, error) {
	base, err := casBase(p.Config)
	if err != nil {
		return nil, err
	}
	ticket := r.Query.Get("ticket")
	if ticket == "" {
		return nil, errors.New("回调缺少 ticket")
	}
	if len(ticket) > 512 || strings.ContainsAny(ticket, " \r\n") {
		return nil, errors.New("ticket 不合法")
	}
	svc, err := f.serviceURL(p, r.RedirectURI, r.State)
	if err != nil {
		return nil, err
	}
	ver := casVersion(p.Config)
	ep := "serviceValidate"
	switch ver {
	case "1":
		ep = "validate"
	case "3":
		ep = "p3/serviceValidate"
	}
	res, err := doRequest(ctx, f.env.HTTP(p.Config.SkipVerify), "GET", authURL(base+ep, url.Values{"service": {svc}, "ticket": {ticket}}), map[string]string{"Accept": "*/*"}, nil)
	if err != nil {
		return nil, err
	}
	if res.Status != 200 {
		return nil, fmt.Errorf("%w: CAS HTTP %d", ErrUnavailable, res.Status)
	}
	var user string
	var attrs map[string][]string
	if ver == "1" {
		// CAS 1.0：第一行 yes/no，第二行用户名
		lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(string(res.Body)), "\r", ""), "\n")
		if len(lines) < 2 || strings.TrimSpace(lines[0]) != "yes" || strings.TrimSpace(lines[1]) == "" {
			return nil, errors.New("CAS 拒绝了该 ticket")
		}
		user, attrs = strings.TrimSpace(lines[1]), map[string][]string{}
	} else if user, attrs, err = parseCASXML(res.Body); err != nil {
		return nil, err
	}
	attrs["user"] = []string{user}
	return mapIdentity(p, user, attrs, "", "", true), nil
}

func (f *casFlow) Check(ctx context.Context, p *Provider) (string, error) {
	base, err := casBase(p.Config)
	if err != nil {
		return "", err
	}
	// 登录页可达即认为服务正常（不带 ticket 去校验接口没有意义）。
	res, err := doRequest(ctx, f.env.HTTP(p.Config.SkipVerify), "GET", base+"login", map[string]string{"Accept": "text/html"}, nil)
	if err != nil {
		return "", err
	}
	if res.Status >= 500 || res.Status == 404 {
		return "", fmt.Errorf("CAS 登录页返回 HTTP %d，请检查服务地址（需要以 /cas/ 之类的上下文路径结尾）", res.Status)
	}
	return fmt.Sprintf("CAS 登录页可访问（协议版本 %s）。点“测试登录”可走一遍完整认证。", casVersion(p.Config)), nil
}
