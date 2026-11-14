// @author: kevinkelly
// @license: Apache Licence
// @file: connparams.go
// @time: 2026/10/28

package dbtadmin

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"dbtool/internal/crypto"
)

// ConnParams 是从 DATABASE_URL / DDL_DATABASE_URL 解析出的连接参数。密码只存在于内存，不进日志和报告。
type ConnParams struct {
	Host, Port string
	User, Pass string
	DB         string
	TLS        string // 空 | true | skip-verify | preferred | false
}

var reDBName = regexp.MustCompile(`^[A-Za-z0-9_$]+$`)

// ParseURL 解析 mysql://user:pass@host:port/db?tls=...；密码需 URL 编码。密码是 ENC(密文) 时需要密钥，见 ParseURLWith。
func ParseURL(raw string) (ConnParams, error) { return ParseURLWith(raw, nil) }

// ParseURLWith 同 ParseURL；m 用来解开连接串里 ENC(密文) 形式的密码（m 为 nil 且密码是 ENC 时返回错误）。
func ParseURLWith(raw string, m *crypto.Mirage) (ConnParams, error) {
	if strings.TrimSpace(raw) == "" {
		return ConnParams{}, errors.New("连接串为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		// url.Error 会带出原始串（含密码），这里只给通用提示
		return ConnParams{}, errors.New("连接串格式错误（应为 mysql://用户:URL编码的密码@主机:3306/库名；@ : / ? # % 必须编码）")
	}
	if u.Scheme != "mysql" {
		return ConnParams{}, fmt.Errorf("连接串只支持 mysql:// ，当前为 %q", u.Scheme)
	}
	p := ConnParams{Host: u.Hostname(), Port: u.Port(), DB: strings.TrimPrefix(u.Path, "/")}
	if p.Host == "" {
		return ConnParams{}, errors.New("连接串缺少主机")
	}
	if p.Port == "" {
		p.Port = "3306"
	}
	if u.User == nil || u.User.Username() == "" {
		return ConnParams{}, errors.New("连接串缺少用户名")
	}
	p.User = u.User.Username()
	p.Pass, _ = u.User.Password()
	if crypto.IsEncryptedPassword(p.Pass) {
		pass, err := m.ResolvePassword(p.Pass)
		if err != nil {
			return ConnParams{}, err
		}
		p.Pass = pass
	}
	if !reDBName.MatchString(p.DB) {
		return ConnParams{}, errors.New("连接串缺少库名，或库名含不允许的字符（只允许字母数字下划线）")
	}
	q := u.Query()
	switch t := q.Get("tls"); t {
	case "", "true", "false", "skip-verify", "preferred":
		p.TLS = t
	default:
		return ConnParams{}, fmt.Errorf("tls 参数只支持 true / false / skip-verify / preferred，当前为 %q", t)
	}
	return p, nil
}

var reQuoted = regexp.MustCompile(`'[^']*'`)

// RedactMsg 抹掉错误信息里单引号包裹的内容（MySQL 的 Duplicate entry 'xxx' 等会带出行数据）。
func RedactMsg(s string) string {
	return reQuoted.ReplaceAllString(s, "'***'")
}
