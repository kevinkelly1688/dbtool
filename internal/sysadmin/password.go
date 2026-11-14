// @author: kevinkelly
// @license: Apache Licence
// @file: password.go
// @time: 2026/10/28

package sysadmin

import (
	"crypto/rand"
	"math/big"
	"strconv"
	"strings"
	"unicode"

	"dbtool/internal/crypto"
	"dbtool/internal/settings"
)

// 密码规则：最短 9 位、不能全是数字、不能是常见弱口令、不能包含用户名等个人信息。
// 登录与安全设置里（系统设置 security.password_*）配置了最小长度 / 复杂度时按配置来，读不到就用默认值。
const (
	defaultMinPasswordLen = 9
	maxPasswordLen        = 128
)

// commonPasswords 是一小份常见弱口令（这里只放最常见的几十条作为兜底）。
var commonPasswords = map[string]bool{
	"password": true, "password1": true, "password123": true, "passw0rd": true, "p@ssw0rd": true, "p@ssword": true,
	"123456789": true, "1234567890": true, "12345678910": true, "qwertyuiop": true, "qwerty123": true, "qwerty1234": true,
	"1q2w3e4r5t": true, "1qaz2wsx3": true, "1qaz2wsx": true, "abc123456": true, "abcd12345": true, "abcd1234": true,
	"admin1234": true, "admin12345": true, "administrator": true, "letmein123": true, "welcome123": true,
	"iloveyou1": true, "iloveyou12": true, "changeme": true, "changeme1": true, "123123123": true, "111111111": true,
	"000000000": true, "987654321": true, "zaq12wsx": true, "asdfghjkl": true, "asdf1234": true, "root12345": true,
	"archery123": true, "dbatool123": true, "mysql12345": true, "test12345": true, "12345678a": true, "a12345678": true,
}

// PasswordPolicy 是密码策略。
type PasswordPolicy struct {
	MinLen     int
	Complexity bool // 要求同时包含字母、数字和特殊字符（新增项，默认关闭）
}

// DefaultPasswordPolicy 返回默认的密码策略（最短长度取 defaultMinPasswordLen，不要求复杂度）。
func DefaultPasswordPolicy() PasswordPolicy { return PasswordPolicy{MinLen: defaultMinPasswordLen} }

// policy 读取当前生效的密码策略（系统设置 security.password_min_length / security.password_complexity）。
func (s *Service) policy() PasswordPolicy {
	p := DefaultPasswordPolicy()
	if v, ok := s.repo.AuthSetting(settings.KeySecurityPasswordMinLength); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 && n <= maxPasswordLen {
			p.MinLen = n
		}
	}
	if v, ok := s.repo.AuthSetting(settings.KeySecurityPasswordComplexity); ok {
		p.Complexity = strings.EqualFold(strings.TrimSpace(v), "true") || strings.TrimSpace(v) == "1"
	}
	return p
}

// ValidatePassword 按默认策略校验新密码。related 是与用户有关的字符串（用户名、邮箱、姓名），密码不能包含它们。
func ValidatePassword(pw string, related ...string) error {
	return DefaultPasswordPolicy().Validate(pw, related...)
}

// Validate 按策略校验新密码。
func (p PasswordPolicy) Validate(pw string, related ...string) error {
	if strings.TrimSpace(pw) == "" {
		return bad("密码不能为空")
	}
	if p.MinLen <= 0 {
		p.MinLen = defaultMinPasswordLen
	}
	n := len([]rune(pw))
	if n < p.MinLen {
		return bad(errf("密码太短，至少需要 %d 个字符", p.MinLen))
	}
	if n > maxPasswordLen {
		return bad("密码太长，最多 128 个字符")
	}
	digits := true
	for _, r := range pw {
		if !unicode.IsDigit(r) {
			digits = false
			break
		}
	}
	if digits {
		return bad("密码不能全是数字")
	}
	low := strings.ToLower(pw)
	if commonPasswords[low] {
		return bad("密码过于常见，请换一个更难猜的")
	}
	for _, r := range related {
		r = strings.ToLower(strings.TrimSpace(r))
		if at := strings.IndexByte(r, '@'); at > 0 { // 邮箱只比较 @ 之前的部分
			r = r[:at]
		}
		if len([]rune(r)) >= 3 && strings.Contains(low, r) {
			return bad("密码不能包含用户名、邮箱或姓名")
		}
	}
	if p.Complexity {
		var letter, digit, special bool
		for _, r := range pw {
			switch {
			case unicode.IsLetter(r):
				letter = true
			case unicode.IsDigit(r):
				digit = true
			case unicode.IsSpace(r):
			default:
				special = true
			}
		}
		if !letter || !digit || !special {
			return bad("密码需要同时包含字母、数字和特殊字符")
		}
	}
	return nil
}

const saltChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// randomSalt 生成 22 位字母数字盐。
func randomSalt() (string, error) {
	out := make([]byte, 22)
	max := big.NewInt(int64(len(saltChars)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = saltChars[n.Int64()]
	}
	return string(out), nil
}

// hashPassword 生成 pbkdf2_sha256 格式（pbkdf2_sha256$迭代$盐$哈希）的密码哈希。
func (s *Service) hashPassword(pw string) (string, error) {
	salt, err := randomSalt()
	if err != nil {
		return "", err
	}
	iter := s.Iterations
	if iter <= 0 {
		iter = 390000
	}
	return crypto.MakeDjangoPassword(pw, salt, iter)
}
