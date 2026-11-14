// @author: kevinkelly
// @license: Apache Licence
// @file: password_test.go
// @time: 2026/10/28

package dbtadmin

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"dbtool/internal/sysadmin"
)

// 本文件守住 password.go 与 sysadmin 的密码策略一致（dbt-admin 没有 import sysadmin，原因见 password.go 的注释）。

func TestPasswordPolicyMatchesSysadminBehavior(t *testing.T) {
	pws := []string{
		"", "   ", "short", "12345678", "123456789", "1234567890123", "password", "Password123", "PASSWORD123", "letmein123",
		"Tr0ub4dor&3-horse", "correct horse battery", "abcdefghi", "Abcdefghi1", "Abcdefghi1!", "ünïcödé-pässwörd-1", "with spaces 1 2 3 !",
		strings.Repeat("a1!", 43), strings.Repeat("x", 128), strings.Repeat("x", 129), "Op5Admin#2026", "xxbobbyxx1234", "alice-is-here-1",
	}
	related := [][]string{nil, {"bob"}, {"bobby", "bobby@example.com", "Bobby Tables"}, {"alice@x.com"}, {"ab"}, {"op5admin"}}
	pols := []PasswordPolicy{{}, {MinLen: 9}, {MinLen: 12}, {MinLen: 20}, {MinLen: 9, Complexity: true}, {MinLen: 128}}
	for _, p := range pols {
		sp := sysadmin.PasswordPolicy{MinLen: p.MinLen, Complexity: p.Complexity}
		for _, pw := range pws {
			for _, rel := range related {
				got, want := p.Validate(pw, rel...), sp.Validate(pw, rel...)
				if (got == nil) != (want == nil) {
					t.Fatalf("策略 %+v 密码 %q related %v：dbt-admin=%v sysadmin=%v", p, pw, rel, got, want)
				}
				if got != nil && got.Error() != want.Error() {
					t.Errorf("策略 %+v 密码 %q：错误信息不同\n  dbt-admin: %s\n  sysadmin:  %s", p, pw, got, want)
				}
			}
		}
	}
	if DefaultPasswordPolicy().MinLen != sysadmin.DefaultPasswordPolicy().MinLen {
		t.Error("默认最小长度不一致")
	}
}

// 从 sysadmin/password.go 源码里解析出常见弱口令表，与本地表做集合相等校验。
func TestCommonPasswordListMatchesSysadminSource(t *testing.T) {
	src, err := os.ReadFile("../sysadmin/password.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "var commonPasswords = map[string]bool{")
	if i < 0 {
		t.Fatal("在 sysadmin/password.go 里找不到 commonPasswords")
	}
	body := s[i:]
	body = body[:strings.Index(body, "\n}\n")]
	re := regexp.MustCompile(`"([^"]+)":\s*true`)
	want := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		want[m[1]] = true
	}
	if len(want) < 30 {
		t.Fatalf("解析出的弱口令太少（%d），解析规则可能失效", len(want))
	}
	for w := range want {
		if !commonPasswords[w] {
			t.Errorf("sysadmin 有而 dbt-admin 缺：%q", w)
		}
	}
	for w := range commonPasswords {
		if !want[w] {
			t.Errorf("dbt-admin 有而 sysadmin 没有：%q", w)
		}
	}
}
