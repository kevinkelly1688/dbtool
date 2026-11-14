// @author: kevinkelly
// @license: Apache Licence
// @file: rekey_test.go
// @time: 2026/10/28

package crypto

import (
	"strings"
	"testing"
)

const (
	rkOldSecret   = "0123456789abcdef0123456789abcdef-old-secret"
	rkNewSecret   = "fedcba9876543210fedcba9876543210-new-secret"
	rkOtherSecret = "ffffffffffffffffffffffffffffffff-third-secret"
)

func rkMirage(t *testing.T, secret, mode, iv string) *Mirage {
	t.Helper()
	m, err := NewMirage(secret, mode, iv)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRekeyerConvert(t *testing.T) {
	oldM := rkMirage(t, rkOldSecret, "ECB", "")
	newM := rkMirage(t, rkNewSecret, "ECB", "")
	other := rkMirage(t, rkOtherSecret, "ECB", "")
	r := NewRekeyer(newM, oldM)

	cases := []struct {
		name string
		in   string
		kind RekeyKind
		want string // 期望的输出；空串表示与输入相同
	}{
		{"空值", "", RekeyEmpty, ""},
		{"旧密钥密文", oldM.Encrypt("root"), RekeyReencrypted, newM.Encrypt("root")},
		{"多行明文密钥", oldM.Encrypt("-----BEGIN KEY-----\nabc\n-----END KEY-----\n"), RekeyReencrypted, newM.Encrypt("-----BEGIN KEY-----\nabc\n-----END KEY-----\n")},
		{"旧密钥下的空串密文", oldM.Encrypt(""), RekeyReencrypted, newM.Encrypt("")},
		{"明文", "true", RekeyPlain, newM.Encrypt("true")},
		{"已是新密钥", newM.Encrypt("dba"), RekeyAlreadyNew, ""},
		{"第三把密钥的密文", other.Encrypt("secret-from-elsewhere"), RekeyUndecryptable, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, kind := r.Convert(c.in)
			if kind != c.kind {
				t.Fatalf("kind=%v want %v", kind, c.kind)
			}
			want := c.want
			if want == "" {
				want = c.in
			}
			if got != want {
				t.Fatalf("out=%q want %q", got, want)
			}
		})
	}
}

// 换过密钥的值再换一次不变：重复运行是幂等的。
func TestRekeyerIdempotent(t *testing.T) {
	oldM := rkMirage(t, rkOldSecret, "ECB", "")
	newM := rkMirage(t, rkNewSecret, "ECB", "")
	r := NewRekeyer(newM, oldM)
	first, k1 := r.Convert(oldM.Encrypt("P@ss-密码"))
	if k1 != RekeyReencrypted {
		t.Fatal(k1)
	}
	second, k2 := r.Convert(first)
	if k2 != RekeyAlreadyNew || second != first {
		t.Fatalf("二次转换应不变: kind=%v", k2)
	}
	if got := newM.Decrypt(first); got != "P@ss-密码" {
		t.Fatalf("新密钥应解出原文: %q", got)
	}
	// 明文第一次加密，第二次已是密文
	p1, kp := r.Convert("local")
	if kp != RekeyPlain {
		t.Fatal(kp)
	}
	if p2, kp2 := r.Convert(p1); kp2 != RekeyAlreadyNew || p2 != p1 {
		t.Fatalf("明文加密后再转换应不变: %v", kp2)
	}
}

func TestRekeyerSameKey(t *testing.T) {
	a := rkMirage(t, rkOldSecret, "ECB", "")
	b := rkMirage(t, rkOldSecret, "ECB", "")
	r := NewRekeyer(a, b)
	if !r.SameKey() || len(r.Olds()) != 0 {
		t.Fatal("等价的旧密钥应被丢掉")
	}
	enc := b.Encrypt("root")
	if got, k := r.Convert(enc); k != RekeyAlreadyNew || got != enc {
		t.Fatalf("同一把密钥的密文不应改动: %v", k)
	}
	if got, k := r.Convert("plain"); k != RekeyPlain || a.Decrypt(got) != "plain" {
		t.Fatalf("同一把密钥时明文仍应被加密: %v", k)
	}
	// 模式不同算不同的密钥
	cbc := rkMirage(t, rkOldSecret, "CBC", "1234567890abcdef")
	if NewRekeyer(a, cbc).SameKey() {
		t.Fatal("ECB 与 CBC 不是同一把密钥")
	}
}

func TestRekeyerCBCDeterministic(t *testing.T) {
	oldM := rkMirage(t, rkOldSecret, "CBC", "1234567890abcdef")
	newM := rkMirage(t, rkNewSecret, "CBC", "abcdef1234567890")
	r := NewRekeyer(newM, oldM)
	in := oldM.Encrypt("root")
	a, _ := r.Convert(in)
	b, _ := r.Convert(in)
	if a != b || newM.Decrypt(a) != "root" {
		t.Fatalf("CBC 固定 IV 应确定性: %q %q", a, b)
	}
}

// 多把旧密钥：先匹配到的解开即可；都解不开原样保留。
func TestRekeyerMultipleOlds(t *testing.T) {
	o1 := rkMirage(t, rkOldSecret, "ECB", "")
	o2 := rkMirage(t, rkOtherSecret, "ECB", "")
	newM := rkMirage(t, rkNewSecret, "ECB", "")
	r := NewRekeyer(newM, o1, o2, o1)
	if len(r.Olds()) != 2 {
		t.Fatalf("olds=%d", len(r.Olds()))
	}
	for _, m := range []*Mirage{o1, o2} {
		got, k := r.Convert(m.Encrypt("x-value"))
		if k != RekeyReencrypted || newM.Decrypt(got) != "x-value" {
			t.Fatalf("kind=%v", k)
		}
	}
}

// 乱码不能被当成明文解出来：用错误密钥解“碰巧通过填充”的结果必须是可读文本才算解开。
func TestRekeyerRejectsGarbage(t *testing.T) {
	oldM := rkMirage(t, rkOldSecret, "ECB", "")
	newM := rkMirage(t, rkNewSecret, "ECB", "")
	other := rkMirage(t, rkOtherSecret, "ECB", "")
	r := NewRekeyer(newM, oldM)
	n := 0
	for i := 0; i < 400; i++ {
		v := other.Encrypt(strings.Repeat("v", 1+i%40) + string(rune('a'+i%26)))
		if got, k := r.Convert(v); k != RekeyUndecryptable || got != v {
			t.Fatalf("第三把密钥的密文应原样保留: i=%d kind=%v", i, k)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no cases")
	}
}
