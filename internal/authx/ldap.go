// @author: kevinkelly
// @license: Apache Licence
// @file: ldap.go
// @time: 2026/10/28

package authx

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// LDAP / AD：账号密码表单走目录绑定。目录访问封装成 Directory 接口，单测用假实现；
// 生产实现 ldapDirectory 用 go-ldap。两种方式：
//   - DN 模板：直接用 uid=%(user)s,ou=people,… 去绑定；
//   - 先搜后绑：用查询账号（可匿名）按过滤器搜到用户 DN，再用用户 DN + 密码绑定。

// DirEntry 是目录里的一个用户条目；Attrs 的键为小写属性名。
type DirEntry struct {
	DN    string
	Attrs map[string][]string
}

var (
	// ErrDirBadCredentials 表示用户不存在或密码不对（两者不区分）。
	ErrDirBadCredentials = errors.New("目录认证失败")
)

// Directory 是目录服务访问接口。
type Directory interface {
	// Authenticate 认证并返回条目；账号或密码不对返回 ErrDirBadCredentials，其他错误视为目录不可用。
	// attrs 是需要取回的属性名。调用方已保证 username、password 非空且 username 不含控制字符。
	Authenticate(ctx context.Context, cfg *Config, username, password string, attrs []string) (*DirEntry, error)
	// Ping 测试连接：连接、（StartTLS）、用查询账号绑定、在搜索 Base 下做一次搜索。
	Ping(ctx context.Context, cfg *Config) (string, error)
}

const userPlaceholder = "%(user)s"

// validateLDAPConfig 校验 LDAP 参数。
func validateLDAPConfig(c *Config) error {
	if err := requireFields("服务器地址", c.ServerURI); err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimSpace(c.ServerURI))
	if err != nil || u.Host == "" || (u.Scheme != "ldap" && u.Scheme != "ldaps") {
		return errors.New("服务器地址需要形如 ldaps://host:636 或 ldap://host:389")
	}
	switch c.ConnSecurity {
	case "", "ldaps":
		if u.Scheme != "ldaps" {
			return errors.New("连接方式为 LDAPS 时服务器地址需以 ldaps:// 开头")
		}
	case "starttls":
		if u.Scheme != "ldap" {
			return errors.New("连接方式为 StartTLS 时服务器地址需以 ldap:// 开头")
		}
	case "none":
		if u.Scheme != "ldap" {
			return errors.New("明文连接时服务器地址需以 ldap:// 开头")
		}
	default:
		return errors.New("连接方式不合法")
	}
	tmpl, filter := strings.TrimSpace(c.UserDNTemplate), strings.TrimSpace(c.SearchFilter)
	switch {
	case tmpl != "" && filter != "":
		return errors.New("“用户 DN 模板”与“用户搜索过滤”只能选一种")
	case tmpl != "":
		if !strings.Contains(tmpl, userPlaceholder) {
			return fmt.Errorf("用户 DN 模板里需要包含 %s", userPlaceholder)
		}
	case filter != "":
		if !strings.Contains(filter, userPlaceholder) {
			return fmt.Errorf("用户搜索过滤里需要包含 %s", userPlaceholder)
		}
		if _, err := ldap.CompileFilter(strings.ReplaceAll(filter, userPlaceholder, "x")); err != nil {
			return errors.New("用户搜索过滤不是合法的 LDAP 过滤器")
		}
		if err := requireFields("用户搜索 Base", c.SearchBase); err != nil {
			return err
		}
	default:
		return errors.New("需要填写“用户 DN 模板”或“用户搜索过滤”")
	}
	if c.TimeoutSec < 0 || c.TimeoutSec > 60 {
		return errors.New("超时时间需在 0 到 60 秒之间")
	}
	return nil
}

type ldapDirectory struct{}

// NewLDAPDirectory 返回基于 go-ldap 的目录实现。
func NewLDAPDirectory() Directory { return ldapDirectory{} }

func ldapTimeout(c *Config) time.Duration {
	if c.TimeoutSec > 0 {
		return time.Duration(c.TimeoutSec) * time.Second
	}
	return 10 * time.Second
}

// dial 按配置建立连接（LDAPS 或 StartTLS 或明文），并设置超时。
func (ldapDirectory) dial(ctx context.Context, c *Config) (*ldap.Conn, error) {
	if err := validateLDAPConfig(c); err != nil {
		return nil, err
	}
	u, _ := url.Parse(strings.TrimSpace(c.ServerURI))
	tlsCfg := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.SkipVerify} //nolint:gosec // 管理员显式选择
	to := ldapTimeout(c)
	conn, err := ldap.DialURL(strings.TrimSpace(c.ServerURI),
		ldap.DialWithDialer(&net.Dialer{Timeout: to}), ldap.DialWithTLSConfig(tlsCfg))
	if err != nil {
		return nil, fmt.Errorf("%w: 连接目录服务失败", ErrUnavailable)
	}
	conn.SetTimeout(to)
	if c.ConnSecurity == "starttls" {
		if err := conn.StartTLS(tlsCfg); err != nil {
			conn.Close()
			return nil, fmt.Errorf("%w: StartTLS 失败", ErrUnavailable)
		}
	}
	go func() { // ctx 取消时关闭连接，避免请求被取消后还挂着
		<-ctx.Done()
		conn.Close()
	}()
	return conn, nil
}

func isBadCreds(err error) bool {
	return ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) || ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) ||
		ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidDNSyntax)
}

func (d ldapDirectory) Authenticate(ctx context.Context, c *Config, username, password string, attrs []string) (*DirEntry, error) {
	if username == "" || password == "" { // 空密码会被目录当成“匿名绑定”而成功，必须先拒绝
		return nil, ErrDirBadCredentials
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn, err := d.dial(ctx, c)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if tmpl := strings.TrimSpace(c.UserDNTemplate); tmpl != "" {
		dn := userDN(tmpl, username)
		if err := conn.Bind(dn, password); err != nil {
			if isBadCreds(err) {
				return nil, ErrDirBadCredentials
			}
			return nil, fmt.Errorf("%w: 绑定失败", ErrUnavailable)
		}
		res, err := conn.Search(ldap.NewSearchRequest(dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 1, 0, false, "(objectClass=*)", attrs, nil))
		if err != nil || len(res.Entries) != 1 {
			return &DirEntry{DN: dn, Attrs: map[string][]string{}}, nil // 能绑定即视为认证成功，取不到属性时只用登录名
		}
		return entryOf(res.Entries[0]), nil
	}

	// 先搜后绑
	if c.BindDN != "" {
		if err := conn.Bind(c.BindDN, c.BindPassword); err != nil {
			return nil, fmt.Errorf("%w: 查询账号绑定失败", ErrUnavailable)
		}
	} else if err := conn.UnauthenticatedBind(""); err != nil {
		return nil, fmt.Errorf("%w: 匿名绑定失败", ErrUnavailable)
	}
	filter := userFilter(c.SearchFilter, username)
	res, err := conn.Search(ldap.NewSearchRequest(strings.TrimSpace(c.SearchBase), ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 0, false, filter, attrs, nil))
	if err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
			return nil, ErrDirBadCredentials // 匹配到多个条目：不猜，当作认证失败
		}
		return nil, fmt.Errorf("%w: 搜索用户失败", ErrUnavailable)
	}
	if len(res.Entries) != 1 {
		return nil, ErrDirBadCredentials
	}
	ent := res.Entries[0]
	if err := conn.Bind(ent.DN, password); err != nil {
		if isBadCreds(err) {
			return nil, ErrDirBadCredentials
		}
		return nil, fmt.Errorf("%w: 绑定失败", ErrUnavailable)
	}
	return entryOf(ent), nil
}

func entryOf(e *ldap.Entry) *DirEntry {
	out := &DirEntry{DN: e.DN, Attrs: map[string][]string{}}
	for _, a := range e.Attributes {
		out.Attrs[strings.ToLower(a.Name)] = append([]string(nil), a.Values...)
	}
	return out
}

func (d ldapDirectory) Ping(ctx context.Context, c *Config) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn, err := d.dial(ctx, c)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	idx := 0 // 对应 pingOK 的组合：1 = StartTLS，2 = 查询账号绑定成功，4 = 搜索 Base 可访问
	if c.ConnSecurity == "starttls" {
		idx |= 1
	}
	if c.BindDN != "" {
		if err := conn.Bind(c.BindDN, c.BindPassword); err != nil {
			if isBadCreds(err) {
				return "", errors.New("查询账号 DN 或密码不对")
			}
			return "", fmt.Errorf("%w: 查询账号绑定失败", ErrUnavailable)
		}
		idx |= 2
	}
	if base := strings.TrimSpace(c.SearchBase); base != "" {
		if _, err := conn.Search(ldap.NewSearchRequest(base, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 1, 0, false, "(objectClass=*)", []string{"dn"}, nil)); err != nil {
			return "", errors.New("搜索 Base 不可访问，请检查 DN 与查询账号的权限")
		}
		idx |= 4
	}
	return pingOK[idx], nil
}

// pingOK 是“测试连接”成功信息的 8 种组合（StartTLS / 查询账号绑定成功 / 搜索 Base 可访问）：每种都写成整句，便于整句翻译。
var pingOK = [8]string{
	"连接成功",
	"连接成功（StartTLS）",
	"连接成功，查询账号绑定成功",
	"连接成功（StartTLS），查询账号绑定成功",
	"连接成功，搜索 Base 可访问",
	"连接成功（StartTLS），搜索 Base 可访问",
	"连接成功，查询账号绑定成功，搜索 Base 可访问",
	"连接成功（StartTLS），查询账号绑定成功，搜索 Base 可访问",
}

// ldapFlow 是 LDAP 登录方式：把 Directory 的结果映射成 Identity。
type ldapFlow struct{ dir Directory }

func newLDAPFlow(d Directory) *ldapFlow { return &ldapFlow{dir: d} }

// Login 用账号密码认证。
func (f *ldapFlow) Login(ctx context.Context, p *Provider, username, password string) (*Identity, error) {
	if username == "" || password == "" || strings.ContainsAny(username, "\x00\r\n") {
		return nil, ErrBadCredentials
	}
	if err := validateLDAPConfig(&p.Config); err != nil {
		return nil, fmt.Errorf("LDAP 配置不完整：%w", err)
	}
	ent, err := f.dir.Authenticate(ctx, &p.Config, username, password, attrNamesNeeded(p))
	if err != nil {
		if errors.Is(err, ErrDirBadCredentials) {
			return nil, ErrBadCredentials
		}
		return nil, err
	}
	attrs := map[string][]string{}
	for k, v := range ent.Attrs {
		attrs[k] = v
	}
	attrs["dn"] = []string{ent.DN}
	id := mapIdentity(p, strings.ToLower(username), attrs, "", "", true)
	if id.Username == "" || p.Config.AttrUsername == "" {
		id.Username = username // 没有显式映射用户名属性时，用登录名
	}
	return id, nil
}

// userFilter 把登录名代入搜索过滤器；登录名先按 RFC 4515 转义，防止 LDAP 注入（如输入 *)(uid=*）。
func userFilter(tmpl, username string) string {
	return strings.ReplaceAll(strings.TrimSpace(tmpl), userPlaceholder, ldap.EscapeFilter(username))
}

// userDN 把登录名代入 DN 模板；登录名按 DN 规则转义（逗号、加号等不能改变 DN 结构）。
func userDN(tmpl, username string) string {
	return strings.ReplaceAll(strings.TrimSpace(tmpl), userPlaceholder, ldap.EscapeDN(username))
}
