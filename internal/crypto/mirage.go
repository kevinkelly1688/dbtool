// @author: kevinkelly
// @license: Apache Licence
// @file: mirage.go
// @time: 2026/10/28

// Package crypto 实现 mirage 字段加密、pbkdf2_sha256 密码哈希与密钥更换。
package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
)

// Mirage 实现 mirage 加密字段的加解密（AES + PKCS7 + urlsafe base64）。
//
// 密钥派生：key = urlsafe_b64encode(SECRET_KEY)[:32]，注意是取 base64 文本的
// 前 32 字节作为 AES-256 密钥，而不是直接截取 SECRET_KEY。
type Mirage struct {
	block cipher.Block
	key   []byte // 派生后的 AES 密钥（只用于判断两个实例是否同一把密钥，见 sameAs）
	mode  string
	iv    []byte
}

// NewMirage mode 取 "ECB"(默认) 或 "CBC"；iv 仅 CBC 使用，需 16 字节。
func NewMirage(secretKey, mode string, iv string) (*Mirage, error) {
	if len(secretKey) < 32 {
		return nil, errors.New("MIRAGE_SECRET_KEY/SECRET_KEY 长度必须不少于 32")
	}
	key := []byte(base64.URLEncoding.EncodeToString([]byte(secretKey)))[:32]
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if mode == "" {
		mode = "ECB"
	}
	if mode != "ECB" && mode != "CBC" {
		return nil, fmt.Errorf("不支持的加密模式 %q", mode)
	}
	if mode == "CBC" && len(iv) != aes.BlockSize {
		return nil, errors.New("CBC 模式 IV 必须为 16 字节")
	}
	return &Mirage{block: block, key: key, mode: mode, iv: []byte(iv)}, nil
}

// sameAs 判断两个 Mirage 是否等价：密钥、模式相同，CBC 时 IV 也相同（等价则同一明文得到同一密文）。
func (m *Mirage) sameAs(o *Mirage) bool {
	if m == nil || o == nil {
		return false
	}
	if !bytes.Equal(m.key, o.key) || m.mode != o.mode {
		return false
	}
	return m.mode != "CBC" || bytes.Equal(m.iv, o.iv)
}

// Decrypt 解密。解密失败时原样返回（兼容未加密的历史明文）。
func (m *Mirage) Decrypt(s string) string {
	plain, err := m.tryDecrypt(s)
	if err != nil {
		return s
	}
	return plain
}

// LooksEncrypted 判断 s 的格式像 mirage 密文：url-safe base64，解码后至少 16 字节且是 16 的整数倍。
// 只看格式，不保证能解开；明文里偶尔也会碰巧符合（比如 24 位的字母数字串），所以只用于提示。
func LooksEncrypted(s string) bool {
	if len(s) < 24 {
		return false
	}
	raw, err := base64.URLEncoding.DecodeString(s)
	return err == nil && len(raw) >= aes.BlockSize && len(raw)%aes.BlockSize == 0
}

// Undecryptable 判断 s 是否“看起来是密文，却用当前密钥 / 模式 / IV 解不开”。
// 典型原因：这个值当初是用另一把 SECRET_KEY / MIRAGE_SECRET_KEY 保存的，或后来改过 MIRAGE_CIPHER_MODE / MIRAGE_CIPHER_IV。
// Decrypt 在这种情况下会原样返回密文（为了兼容明文），调用方拿到的是一串乱码，需要靠这个方法发现。
func (m *Mirage) Undecryptable(s string) bool {
	if !LooksEncrypted(s) {
		return false
	}
	_, err := m.tryDecrypt(s)
	return err != nil
}

// Encrypt 加密。若输入本身已是合法密文则原样返回，避免重复加密。
func (m *Mirage) Encrypt(s string) string {
	if _, err := m.tryDecrypt(s); err == nil {
		return s
	}
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

func (m *Mirage) tryDecrypt(s string) (string, error) {
	raw, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	if len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		return "", errors.New("密文长度非法")
	}
	out := make([]byte, len(raw))
	switch m.mode {
	case "CBC":
		cipher.NewCBCDecrypter(m.block, m.iv).CryptBlocks(out, raw)
	default:
		for i := 0; i < len(raw); i += aes.BlockSize {
			m.block.Decrypt(out[i:i+aes.BlockSize], raw[i:i+aes.BlockSize])
		}
	}
	plain, err := pkcs7Unpad(out, aes.BlockSize)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func pkcs7Pad(b []byte, size int) []byte {
	n := size - len(b)%size
	out := make([]byte, 0, len(b)+n)
	out = append(out, b...)
	for i := 0; i < n; i++ {
		out = append(out, byte(n))
	}
	return out
}

func pkcs7Unpad(b []byte, size int) ([]byte, error) {
	if len(b) == 0 || len(b)%size != 0 {
		return nil, errors.New("padding 长度非法")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > size || n > len(b) {
		return nil, errors.New("padding 非法")
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, errors.New("padding 非法")
		}
	}
	return b[:len(b)-n], nil
}
