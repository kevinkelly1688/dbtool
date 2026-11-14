// @author: kevinkelly
// @license: Apache Licence
// @file: dburl_test.go
// @time: 2026/10/28

package crypto

import (
	"net/url"
	"strings"
	"testing"
)

const testKey32 = "0123456789abcdef0123456789abcdef-TESTKEY"

func TestEncryptURLRoundTrip(t *testing.T) {
	m, err := NewMirage(testKey32, "ECB", "")
	if err != nil {
		t.Fatal(err)
	}
	raw := "mysql://dbtool:p%40ss%3Aw%2Frd@127.0.0.1:3306/dbtool?tls=skip-verify"
	enc, err := m.EncryptURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, "p%40ss") || strings.Contains(enc, "p@ss") {
		t.Fatalf("密文串里不能有明文密码: %s", enc)
	}
	u, err := url.Parse(enc)
	if err != nil || u.Host != "127.0.0.1:3306" || u.Path != "/dbtool" || u.RawQuery != "tls=skip-verify" || u.User.Username() != "dbtool" {
		t.Fatalf("其它部分不能被改动: %s %v", enc, err)
	}
	pass, _ := u.User.Password()
	if !IsEncryptedPassword(pass) {
		t.Fatalf("密码应是 ENC(...): %q", pass)
	}
	plain, err := m.ResolvePassword(pass)
	if err != nil || plain != "p@ss:w/rd" {
		t.Fatalf("解密: %q %v", plain, err)
	}
	// 已经是 ENC 的原样返回；明文密码原样通过 ResolvePassword
	if again, err := m.EncryptURL(enc); err != nil || again != enc {
		t.Errorf("重复加密应原样返回: %v", err)
	}
	if p, err := m.ResolvePassword("plain-pass"); err != nil || p != "plain-pass" {
		t.Errorf("明文原样: %q %v", p, err)
	}
}

func TestResolvePasswordWrongKeyOrMissing(t *testing.T) {
	m1, _ := NewMirage(testKey32, "ECB", "")
	m2, _ := NewMirage("another-key-another-key-another-key-0000", "ECB", "")
	enc, _ := m1.EncryptURL("mysql://u:secret@h:3306/d")
	u, _ := url.Parse(enc)
	pass, _ := u.User.Password()
	if _, err := m2.ResolvePassword(pass); err == nil {
		t.Error("换了密钥必须报错，不能把密文当密码去连库")
	}
	var nilM *Mirage
	if _, err := nilM.ResolvePassword(pass); err != ErrNoKey {
		t.Errorf("没有密钥: %v", err)
	}
	// 没有密码 / 格式错误的连接串不能加密
	if _, err := m1.EncryptURL("mysql://u@h:3306/d"); err == nil {
		t.Error("没有密码应报错")
	}
	if _, err := m1.EncryptURL("mysql://u:p@h:3306/d%zz"); err == nil || strings.Contains(err.Error(), "p@h") {
		t.Errorf("格式错误不能回显连接串: %v", err)
	}
}

// 密码本身“长得像密文”（或恰好是合法 base64）也必须真的被加密。
func TestEncryptURLPasswordThatLooksLikeCiphertext(t *testing.T) {
	m, _ := NewMirage(testKey32, "ECB", "")
	looks := m.Encrypt("whatever-secret")
	enc, err := m.EncryptURL("mysql://u:" + looks + "@h:3306/d")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(enc)
	pass, _ := u.User.Password()
	plain, err := m.ResolvePassword(pass)
	if err != nil || plain != looks {
		t.Errorf("应还原成原来的“长得像密文”的密码: %q %v", plain, err)
	}
}
