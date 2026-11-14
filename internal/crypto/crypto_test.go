// @author: kevinkelly
// @license: Apache Licence
// @file: crypto_test.go
// @time: 2026/10/28

package crypto

import "testing"

// 以下为固定的测试向量（mirage 加密与 pbkdf2_sha256 密码哈希），
// 用于保证编码格式字节级不变。
const testSecret = "hfusaf2m4ot#7)fkw#di2bu6(cv0@opwmafx5n#6=3d%x^hpl6"

func TestMirageECBCompat(t *testing.T) {
	m, err := NewMirage(testSecret, "ECB", "")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"root":               "mgc-Oh9sGUWwSFhtQFio4A==",
		"P@ssw0rd-密码!123456": "8D6yHINSS30xyyVA--NK4aohLTLkisU-Bvcp4blPaa0=",
	}
	for plain, enc := range cases {
		if got := m.Decrypt(enc); got != plain {
			t.Errorf("Decrypt(%q)=%q, want %q", enc, got, plain)
		}
		if got := m.Encrypt(plain); got != enc {
			t.Errorf("Encrypt(%q)=%q, want %q", plain, got, enc)
		}
	}
}

func TestMirageCBCCompat(t *testing.T) {
	m, err := NewMirage(testSecret, "CBC", "1234567890abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Decrypt("Igb2bDvP82KEF4doQbm9nw=="); got != "root" {
		t.Errorf("CBC Decrypt=%q", got)
	}
	if got := m.Encrypt("root"); got != "Igb2bDvP82KEF4doQbm9nw==" {
		t.Errorf("CBC Encrypt=%q", got)
	}
}

func TestMirageLegacyPlaintext(t *testing.T) {
	m, _ := NewMirage(testSecret, "ECB", "")
	// 历史明文无法解密时应原样返回，不报错。
	if got := m.Decrypt("plain-text-pwd"); got != "plain-text-pwd" {
		t.Errorf("got %q", got)
	}
	// 已是密文则不重复加密。
	enc := "mgc-Oh9sGUWwSFhtQFio4A=="
	if got := m.Encrypt(enc); got != enc {
		t.Errorf("重复加密: %q", got)
	}
}

func TestDjangoPassword(t *testing.T) {
	enc := "pbkdf2_sha256$390000$abcSalt123$gOJkAJYCxeOxs/++0iMGyvqQfGUdi4lF5vJYd4dHN+c="
	if !CheckDjangoPassword("admin123", enc) {
		t.Error("正确密码校验失败")
	}
	if CheckDjangoPassword("wrong", enc) {
		t.Error("错误密码校验通过")
	}
	got, err := MakeDjangoPassword("admin123", "abcSalt123", 390000)
	if err != nil || got != enc {
		t.Errorf("MakeDjangoPassword=%q err=%v", got, err)
	}
}

// 与当前密钥无关的值：格式像密文、却解不开 → Undecryptable；明文与能解开的密文都不是。
func TestMirageUndecryptable(t *testing.T) {
	m, err := NewMirage(testSecret, "ECB", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewMirage("another-secret-key-another-secret-key-0123456789", "ECB", "")
	if err != nil {
		t.Fatal(err)
	}
	enc := other.Encrypt("local") // 用另一把密钥保存的值
	if !LooksEncrypted(enc) || !m.Undecryptable(enc) {
		t.Errorf("另一把密钥保存的密文应判为解不开: %q looks=%v", enc, LooksEncrypted(enc))
	}
	if m.Decrypt(enc) != enc {
		t.Error("解不开时 Decrypt 仍原样返回（兼容明文）")
	}
	if good := m.Encrypt("local"); m.Undecryptable(good) || !LooksEncrypted(good) {
		t.Errorf("能解开的密文不应判为解不开: %q", good)
	}
	for _, plain := range []string{"", "local", "true", "sftp", "/data/exports", "mysql://u:p@h:3306/db"} {
		if LooksEncrypted(plain) || m.Undecryptable(plain) {
			t.Errorf("明文 %q 不应被当成密文", plain)
		}
	}
}
