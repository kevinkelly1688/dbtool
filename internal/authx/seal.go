// @author: kevinkelly
// @license: Apache Licence
// @file: seal.go
// @time: 2026/10/28

package authx

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// sealer 把一小段 JSON 用 AES-256-GCM 加密成可放进 Cookie 的字符串（保密 + 防篡改 + 带过期）。
// 不同用途（SSO 状态、两步验证中间态）派生各自的密钥，并把用途作为附加认证数据，互相不能混用，
// 也不能拿去冒充 server 的会话 Cookie（那个是另一种格式与密钥）。
type sealer struct {
	aead    cipher.AEAD
	purpose string
	now     func() time.Time
}

func newSealer(secret []byte, purpose string, now func() time.Time) (*sealer, error) {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("dba-tool/authx/" + purpose))
	block, err := aes.NewCipher(m.Sum(nil))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: aead, purpose: purpose, now: now}, nil
}

var errSealBad = errors.New("令牌无效或已过期")

type sealed struct {
	Exp int64           `json:"e"`
	V   json.RawMessage `json:"v"`
}

// Seal 加密 v，ttl 之后失效。
func (s *sealer) Seal(v any, ttl time.Duration) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(sealed{Exp: s.now().Add(ttl).Unix(), V: raw})
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := s.aead.Seal(nonce, nonce, body, []byte(s.purpose))
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// Open 解密并校验过期，把内容解到 v。
func (s *sealer) Open(token string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < s.aead.NonceSize()+s.aead.Overhead() {
		return errSealBad
	}
	n := s.aead.NonceSize()
	body, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(s.purpose))
	if err != nil {
		return errSealBad
	}
	var sd sealed
	if err := json.Unmarshal(body, &sd); err != nil || sd.Exp < s.now().Unix() {
		return errSealBad
	}
	if err := json.Unmarshal(sd.V, v); err != nil {
		return errSealBad
	}
	return nil
}

// randToken 返回 n 字节随机数的 URL 安全 base64。
func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // 系统随机源不可用时不应继续运行
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// randDigits 返回 n 位随机数字（验证码用，无取模偏差）。
func randDigits(n int) string {
	out := make([]byte, n)
	for i := 0; i < n; {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic(err)
		}
		if b[0] < 250 { // 250 = 25*10，丢弃尾部避免偏差
			out[i] = '0' + b[0]%10
			i++
		}
	}
	return string(out)
}

// fingerprint 是用服务端密钥对内容做的 HMAC，用来判断“测试通过之后配置有没有再改过”。
func fingerprint(secret []byte, parts ...string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("dba-tool/authx/fingerprint"))
	for _, p := range parts {
		m.Write([]byte{0})
		m.Write([]byte(p))
	}
	return hex.EncodeToString(m.Sum(nil))
}
