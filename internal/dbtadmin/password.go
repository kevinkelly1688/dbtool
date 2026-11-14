// @author: kevinkelly
// @license: Apache Licence
// @file: password.go
// @time: 2026/10/28

package dbtadmin

import (
	"errors"
	"strconv"
	"strings"
	"unicode"
)

// 密码策略。与 internal/sysadmin/password.go 的 PasswordPolicy.Validate 保持一致（页面里创建用户 / 重置密码用的那份）。
//
// 为什么复制而不是直接 import sysadmin：sysadmin 依赖整个 HTTP 服务（gin、excelize 等），import 它会让 dbt-admin 的二进制
// 从约 6 MB 涨到 55 MB。两份实现是否一致由 password_test.go 守住：行为用例逐项对照 sysadmin，常见弱口令表直接从
// sysadmin/password.go 源码里解析出来做集合相等校验。改动密码策略时两处一起改。
const (
	defaultMinPasswordLen = 9
	maxPasswordLen        = 128
)

// commonPasswords 是一小份常见弱口令（同 sysadmin）。
var commonPasswords = map[string]bool{
	"password": true, "password1": true, "password123": true, "passw0rd": true, "p@ssw0rd": true, "p@ssword": true,
	"123456789": true, "1234567890": true, "12345678910": true, "qwertyuiop": true, "qwerty123": true, "qwerty1234": true,
	"1q2w3e4r5t": true, "1qaz2wsx3": true, "1qaz2wsx": true, "abc123456": true, "abcd12345": true, "abcd1234": true,
	"admin1234": true, "admin12345": true, "administrator": true, "letmein123": true, "welcome123": true,
	"iloveyou1": true, "iloveyou12": true, "changeme": true, "changeme1": true, "123123123": true, "111111111": true,
	"000000000": true, "987654321": true, "zaq12wsx": true, "asdfghjkl": true, "asdf1234": true, "root12345": true,
	"archery123": true, "dbatool123": true, "mysql12345": true, "test12345": true, "12345678a": true, "a12345678": true,
}

// PasswordPolicy 是密码策略：最小长度（默认 9）与可选的复杂度要求（字母 + 数字 + 特殊字符）。
type PasswordPolicy struct {
	MinLen     int
	Complexity bool
}

// DefaultPasswordPolicy 是默认策略：最小长度 9，不要求复杂度。
func DefaultPasswordPolicy() PasswordPolicy { return PasswordPolicy{MinLen: defaultMinPasswordLen} }

// Validate 校验新密码。related 是与用户有关的字符串（用户名、邮箱、姓名），密码不能包含它们。
// 返回的错误信息可以直接展示，不含密码本身。
func (p PasswordPolicy) Validate(pw string, related ...string) error {
	if strings.TrimSpace(pw) == "" {
		return errors.New("密码不能为空")
	}
	if p.MinLen <= 0 {
		p.MinLen = defaultMinPasswordLen
	}
	n := len([]rune(pw))
	if n < p.MinLen {
		return errors.New("密码太短，至少需要 " + strconv.Itoa(p.MinLen) + " 个字符")
	}
	if n > maxPasswordLen {
		return errors.New("密码太长，最多 128 个字符")
	}
	digits := true
	for _, r := range pw {
		if !unicode.IsDigit(r) {
			digits = false
			break
		}
	}
	if digits {
		return errors.New("密码不能全是数字")
	}
	low := strings.ToLower(pw)
	if commonPasswords[low] {
		return errors.New("密码过于常见，请换一个更难猜的")
	}
	for _, r := range related {
		r = strings.ToLower(strings.TrimSpace(r))
		if at := strings.IndexByte(r, '@'); at > 0 { // 邮箱只比较 @ 之前的部分
			r = r[:at]
		}
		if len([]rune(r)) >= 3 && strings.Contains(low, r) {
			return errors.New("密码不能包含用户名、邮箱或姓名")
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
			return errors.New("密码需要同时包含字母、数字和特殊字符")
		}
	}
	return nil
}
