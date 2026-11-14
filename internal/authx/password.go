// @author: kevinkelly
// @license: Apache Licence
// @file: password.go
// @time: 2026/10/28

package authx

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// PasswordPolicy 是密码策略。默认：最小长度 9、不能是常见密码、
// 不能全是数字、不能与用户名太像；复杂度（字母 + 数字 + 特殊字符）是新增项，默认关闭。
type PasswordPolicy struct {
	MinLen     int
	Complexity bool
}

// DefaultPasswordPolicy 是没有任何设置时的策略。
func DefaultPasswordPolicy() PasswordPolicy { return PasswordPolicy{MinLen: 9} }

// 少量最常见的弱密码。
var commonPasswords = map[string]bool{}

func init() {
	for _, p := range []string{"password", "password1", "password123", "passw0rd", "123456", "1234567", "12345678", "123456789", "1234567890",
		"12345678910", "qwerty", "qwerty123", "qwertyuiop", "abc123", "abcd1234", "abc12345", "111111", "11111111", "000000", "00000000",
		"iloveyou", "admin", "admin123", "admin1234", "administrator", "root", "root1234", "letmein", "welcome", "welcome1", "monkey",
		"dragon", "master", "login", "princess", "sunshine", "football", "baseball", "superman", "trustno1", "changeme", "p@ssw0rd",
		"p@ssword", "zaq12wsx", "1q2w3e4r", "1qaz2wsx", "q1w2e3r4", "asdfghjkl", "zxcvbnm", "archery", "dbatool", "mysql123"} {
		commonPasswords[p] = true
	}
}

// ValidatePassword 按策略校验新密码；username 用来拒绝与用户名过于相似的密码（可传多个：用户名、显示名、邮箱前缀）。
// 返回的错误信息可以直接展示给用户。sysadmin 模块创建用户 / 修改密码时应调用它（见 CheckPassword）。
func ValidatePassword(p PasswordPolicy, password string, userAttrs ...string) error {
	if p.MinLen <= 0 {
		p.MinLen = 9
	}
	if n := len([]rune(password)); n < p.MinLen {
		return fmt.Errorf("密码太短，至少需要 %d 个字符", p.MinLen)
	}
	if len(password) > 256 {
		return errors.New("密码过长（不超过 256 字节）")
	}
	lower := strings.ToLower(password)
	if commonPasswords[lower] {
		return errors.New("密码太常见，请换一个更难猜的")
	}
	allDigit := true
	for _, r := range password {
		if !unicode.IsDigit(r) {
			allDigit = false
			break
		}
	}
	if allDigit {
		return errors.New("密码不能全是数字")
	}
	for _, a := range userAttrs {
		a = strings.ToLower(strings.TrimSpace(a))
		if len([]rune(a)) < 3 {
			continue
		}
		if lower == a || strings.Contains(lower, a) || (len(lower) >= 4 && strings.Contains(a, lower)) {
			return errors.New("密码与用户名太相似")
		}
	}
	if p.Complexity {
		var letter, digit, special bool
		for _, r := range password {
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
			return errors.New("密码需要同时包含字母、数字和特殊字符")
		}
	}
	return nil
}
