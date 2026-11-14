// @author: kevinkelly
// @license: Apache Licence
// @file: rekey.go
// @time: 2026/10/28

package crypto

import (
	"unicode"
	"unicode/utf8"
)

// RekeyKind 是一个加密字段的值在“换密钥”时的处理结果。
type RekeyKind int

const (
	RekeyEmpty         RekeyKind = iota // 空值：原样
	RekeyReencrypted                    // 旧密钥解得开：已用新密钥重新加密
	RekeyPlain                          // 明文：已用新密钥加密
	RekeyAlreadyNew                     // 新密钥已能解开（旧密钥解不开，或新旧是同一把）：不动
	RekeyAmbiguous                      // 新旧两把不同的密钥都解得开：不动，交给人确认
	RekeyUndecryptable                  // 看起来是密文，但新旧密钥都解不开：原样保留
)

func (k RekeyKind) String() string {
	switch k {
	case RekeyEmpty:
		return "empty"
	case RekeyReencrypted:
		return "reencrypted"
	case RekeyPlain:
		return "plain"
	case RekeyAlreadyNew:
		return "already-new"
	case RekeyAmbiguous:
		return "ambiguous"
	default:
		return "undecryptable"
	}
}

// Rekeyer 把 mirage 加密字段的值从“旧密钥”换成“新密钥”（dbt-admin reencrypt 使用）。
//
// 判断只看值本身，与处理顺序无关：
//   - 空值：原样；
//   - 不像密文（不是 url-safe base64 的整块密文）：当明文，用新密钥加密；
//   - 像密文：旧密钥解得开且新密钥解不开 → 解出明文再用新密钥加密；新密钥解得开而旧密钥解不开 → 已经是新密钥的密文，不动；
//     两把不同的密钥都解得开（几乎不会发生）→ 不动并交给人确认；谁都解不开 → 原样保留，不会被改坏。
//
// “解得开”要求解出的是可读文本（合法 UTF-8 且没有控制字符，换行 / 制表符除外）：mirage 在密钥错误时约 0.4% 的概率碰巧通过填充校验，
// 只看“没报错”会把乱码当成明文。ECB 与固定 IV 的 CBC 都是确定性加密：同一明文、同一密钥得到同一密文，所以换过密钥的值可以与目标库里的值直接比较，
// 重复运行是幂等的。
type Rekeyer struct {
	new  *Mirage
	olds []*Mirage // 与新密钥不同的旧密钥（去重）
}

// NewRekeyer 构造换密钥器。olds 里与新密钥等价（同密钥、同模式、同 IV）的会被丢掉：它们解出来的就是“已是新密钥”。
func NewRekeyer(newKey *Mirage, olds ...*Mirage) *Rekeyer {
	r := &Rekeyer{new: newKey}
	for _, o := range olds {
		if o == nil || o.sameAs(newKey) {
			continue
		}
		dup := false
		for _, x := range r.olds {
			if x.sameAs(o) {
				dup = true
				break
			}
		}
		if !dup {
			r.olds = append(r.olds, o)
		}
	}
	return r
}

// New 返回新密钥。
func (r *Rekeyer) New() *Mirage { return r.new }

// Olds 返回与新密钥不同的旧密钥（可能为空：旧密钥与新密钥相同时）。
func (r *Rekeyer) Olds() []*Mirage { return r.olds }

// SameKey 报告是否没有任何与新密钥不同的旧密钥（此时只会把明文加密，不会改动已有密文）。
func (r *Rekeyer) SameKey() bool { return len(r.olds) == 0 }

// Convert 返回换过密钥的值与处理结果。不是 RekeyReencrypted / RekeyPlain 时返回的值就是传入的 v。
func (r *Rekeyer) Convert(v string) (string, RekeyKind) {
	if v == "" {
		return v, RekeyEmpty
	}
	if !LooksEncrypted(v) {
		return r.new.Encrypt(v), RekeyPlain
	}
	newOK := false
	if _, ok := r.new.open(v); ok {
		newOK = true
	}
	var oldPlain string
	oldOK := false
	for _, o := range r.olds {
		if p, ok := o.open(v); ok {
			oldPlain, oldOK = p, true
			break
		}
	}
	switch {
	case oldOK && newOK:
		return v, RekeyAmbiguous
	case oldOK:
		return r.new.Encrypt(oldPlain), RekeyReencrypted
	case newOK:
		return v, RekeyAlreadyNew
	}
	return v, RekeyUndecryptable
}

// open 解密并要求结果是可读文本。
func (m *Mirage) open(v string) (string, bool) {
	p, err := m.tryDecrypt(v)
	if err != nil || !readable(p) {
		return "", false
	}
	return p, true
}

func readable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if c == '\n' || c == '\r' || c == '\t' {
			continue
		}
		if unicode.IsControl(c) || c == utf8.RuneError {
			return false
		}
	}
	return true
}
