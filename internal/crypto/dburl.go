// @author: kevinkelly
// @license: Apache Licence
// @file: dburl.go
// @time: 2026/10/28

package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"
)

// 连接串里的加密密码：mysql://用户:ENC(密文)@主机:3306/库名。
// 密文就是 Mirage 的输出（密钥取 MIRAGE_SECRET_KEY，缺省 SECRET_KEY），写进配置文件，不让明文密码出现在 env / 进程环境里。
// 没有 ENC( ) 包裹的密码按明文处理，所以已有部署不用改。

const (
	encPre = "ENC("
	encSuf = ")"
)

// ErrNoKey 表示连接串里有加密密码，却没有可用的密钥。
var ErrNoKey = errors.New("连接串里的密码是加密的（ENC(...)），但没有可用的密钥：请设置 MIRAGE_SECRET_KEY（缺省取 SECRET_KEY）")

// IsEncryptedPassword 判断连接串里的密码是否是 ENC(密文) 形式。
func IsEncryptedPassword(p string) bool {
	return len(p) > len(encPre)+len(encSuf) && strings.HasPrefix(p, encPre) && strings.HasSuffix(p, encSuf)
}

// ResolvePassword 返回可用于连库的明文密码：ENC(密文) 先解密（解不开返回错误，不会把密文当密码去连库），其它原样返回。m 可以为 nil（只有遇到 ENC 才报错）。
func (m *Mirage) ResolvePassword(p string) (string, error) {
	if !IsEncryptedPassword(p) {
		return p, nil
	}
	if m == nil {
		return "", ErrNoKey
	}
	plain, err := m.tryDecrypt(p[len(encPre) : len(p)-len(encSuf)])
	if err == nil && !printablePassword(plain) {
		// 错误的密钥偶尔（约 1/256）也能碰巧通过 PKCS7 校验，得到一串乱码：密码应是可打印的 UTF-8，否则按解不开处理
		err = errors.New("not printable")
	}
	if err != nil {
		return "", errors.New("连接串里的加密密码用当前密钥解不开：MIRAGE_SECRET_KEY（缺省取 SECRET_KEY）、MIRAGE_CIPHER_MODE、MIRAGE_CIPHER_IV 与加密时不一致")
	}
	return plain, nil
}

func printablePassword(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return s != ""
}

// encryptAlways 无条件加密（Encrypt 遇到“本身像密文”的输入会原样返回，连接串密码不能走那条捷径）。
func (m *Mirage) encryptAlways(s string) string {
	padded := pkcs7Pad([]byte(s), aes.BlockSize)
	out := make([]byte, len(padded))
	switch m.mode {
	case "CBC":
		cipher.NewCBCEncrypter(m.block, m.iv).CryptBlocks(out, padded)
	default:
		for i := 0; i < len(padded); i += aes.BlockSize {
			m.block.Encrypt(out[i:i+aes.BlockSize], padded[i:i+aes.BlockSize])
		}
	}
	return base64.URLEncoding.EncodeToString(out)
}

// EncryptURL 把连接串里的明文密码换成 ENC(密文)。raw 里的密码要按 URL 规则编码（解码后才是真密码）；
// 已经是 ENC(...) 的原样返回；没有密码的连接串返回错误。
func (m *Mirage) EncryptURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("连接串格式错误（应为 mysql://用户:URL编码的密码@主机:3306/库名）") // url.Error 会带出原串（含密码），不回显
	}
	if u.User == nil {
		return "", errors.New("连接串里没有用户名和密码")
	}
	pass, ok := u.User.Password()
	if !ok || pass == "" {
		return "", errors.New("连接串里没有密码，无需加密")
	}
	if IsEncryptedPassword(pass) {
		return raw, nil
	}
	if m == nil {
		return "", ErrNoKey
	}
	u.User = url.UserPassword(u.User.Username(), encPre+m.encryptAlways(pass)+encSuf)
	// url.String 会把括号写成 %28 / %29；还原成 ENC(...)，便于阅读，也方便脚本识别（两种写法 url.Parse 都认）
	out := strings.Replace(u.String(), "ENC%28", encPre, 1)
	if i := strings.Index(out, "%29@"); i > 0 {
		out = out[:i] + encSuf + out[i+3:]
	}
	return out, nil
}
