// @author: kevinkelly
// @license: Apache Licence
// @file: sigv4.go
// @time: 2026/10/28

package engine

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4（请求头方式）。只用标准库。

const (
	sigAlgo         = "AWS4-HMAC-SHA256"
	emptyPayloadSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// uriEncode 按 SigV4 规则编码：保留 A-Za-z0-9 - _ . ~，其余 %XX（大写）；encodeSlash=false 时保留 "/"。
func uriEncode(s string, encodeSlash bool) string {
	const hexd = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&15])
		}
	}
	return b.String()
}

// signingKey 派生签名密钥。
func signingKey(sk, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+sk), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

// sigV4 计算 Authorization 头的值。
//
//	uri   已按规则编码的规范 URI（S3 只编码一次）
//	query 已排序、已编码的规范查询串（没有则空）
//	hdrs  参与签名的全部请求头，键必须是小写（至少含 host、x-amz-date；S3 还要 x-amz-content-sha256），值已去掉首尾空白
func sigV4(method, uri, query string, hdrs map[string]string, payloadHash, ak, sk, region, service string, t time.Time) string {
	names := make([]string, 0, len(hdrs))
	for k := range hdrs {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHdrs strings.Builder
	for _, k := range names {
		canonHdrs.WriteString(k + ":" + strings.Join(strings.Fields(hdrs[k]), " ") + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{method, uri, query, canonHdrs.String(), signed, payloadHash}, "\n")
	amzDate := t.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	scope := date + "/" + region + "/" + service + "/aws4_request"
	toSign := strings.Join([]string{sigAlgo, amzDate, scope, sha256Hex([]byte(canonical))}, "\n")
	sig := hex.EncodeToString(hmacSHA256(signingKey(sk, date, region, service), toSign))
	return sigAlgo + " Credential=" + ak + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + sig
}
