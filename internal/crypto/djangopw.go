// @author: kevinkelly
// @license: Apache Licence
// @file: djangopw.go
// @time: 2026/10/28

package crypto

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"strings"
)

// CheckDjangoPassword 校验 pbkdf2_sha256 格式的密码哈希，
// 格式：pbkdf2_sha256$<iterations>$<salt>$<base64 hash>。
func CheckDjangoPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, []byte(parts[2]), iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// MakeDjangoPassword 生成 pbkdf2_sha256 格式的密码哈希。
func MakeDjangoPassword(password, salt string, iterations int) (string, error) {
	h, err := pbkdf2.Key(sha256.New, password, []byte(salt), iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	return "pbkdf2_sha256$" + strconv.Itoa(iterations) + "$" + salt + "$" +
		base64.StdEncoding.EncodeToString(h), nil
}
