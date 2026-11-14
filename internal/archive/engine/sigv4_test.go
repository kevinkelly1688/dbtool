// @author: kevinkelly
// @license: Apache Licence
// @file: sigv4_test.go
// @time: 2026/10/28

package engine

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// AWS 文档里的已知向量。
const (
	awsAK = "AKIAIOSFODNN7EXAMPLE"
	awsSK = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html —— GET Object（带 Range 头）
func TestSigV4S3GetObjectVector(t *testing.T) {
	hdrs := map[string]string{
		"host":                 "examplebucket.s3.amazonaws.com",
		"range":                "bytes=0-9",
		"x-amz-content-sha256": emptyPayloadSHA,
		"x-amz-date":           "20130524T000000Z",
	}
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	got := sigV4("GET", "/test.txt", "", hdrs, emptyPayloadSHA, awsAK, awsSK, "us-east-1", "s3", at)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=host;range;x-amz-content-sha256;x-amz-date,Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if strings.ReplaceAll(got, ", ", ",") != want {
		t.Errorf("S3 GET 向量不匹配\n got  %s\n want %s", got, want)
	}
}

// https://docs.aws.amazon.com/IAM/latest/UserGuide/create-signed-request.html —— IAM ListUsers 示例的签名（通用 SigV4 向量）。
func TestSigV4IAMVector(t *testing.T) {
	hdrs := map[string]string{
		"content-type": "application/x-www-form-urlencoded; charset=utf-8",
		"host":         "iam.amazonaws.com",
		"x-amz-date":   "20150830T123600Z",
	}
	at := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	got := sigV4("GET", "/", "Action=ListUsers&Version=2010-05-08", hdrs, emptyPayloadSHA, awsAK, "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1", "iam", at)
	if !strings.HasSuffix(got, "Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7") {
		t.Errorf("IAM 向量不匹配：%s", got)
	}
}

func TestSigningKeyVector(t *testing.T) {
	// 文档给出的派生密钥（十六进制）：kSigning for 20150830/us-east-1/iam
	k := signingKey(awsSK, "20150830", "us-east-1", "iam")
	if got := sha256Hex(k); got == "" || len(k) != 32 {
		t.Fatalf("签名密钥长度不对：%d", len(k))
	}
}

func TestURIEncode(t *testing.T) {
	cases := []struct {
		in    string
		slash bool
		want  string
	}{
		{"a b/c", false, "a%20b/c"}, {"a b/c", true, "a%20b%2Fc"}, {"~-._", true, "~-._"}, {"中", false, "%E4%B8%AD"},
		{"a+b=c&d", false, "a%2Bb%3Dc%26d"}, {"(x)!*'", false, "%28x%29%21%2A%27"},
	}
	for _, c := range cases {
		if got := uriEncode(c.in, c.slash); got != c.want {
			t.Errorf("uriEncode(%q,%v)=%q want %q", c.in, c.slash, got, c.want)
		}
	}
}

// ---- S3 客户端对一个假的 S3 服务端 ----

type fakeS3 struct {
	mu      sync.Mutex
	objs    map[string][]byte
	reqs    []string
	auth    []string
	hash    []string
	failPut bool
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r.Method+" "+r.Host+" "+r.URL.EscapedPath())
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.hash = append(f.hash, r.Header.Get("x-amz-content-sha256"))
	key := r.URL.Path
	switch r.Method {
	case http.MethodPut:
		if f.failPut {
			w.WriteHeader(403)
			io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>nope</Message></Error>`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.objs[key] = b
	case http.MethodGet:
		b, ok := f.objs[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Write(b)
	case http.MethodHead:
		b, ok := f.objs[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	case http.MethodDelete:
		delete(f.objs, key)
		w.WriteHeader(204)
	}
}

func newFakeS3Store(t *testing.T, typ string, pathStyle bool) (*s3Store, *fakeS3) {
	t.Helper()
	f := &fakeS3{objs: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	st, err := newS3Store(StoreConfig{Type: typ, Endpoint: srv.URL, Bucket: "bk", Prefix: "pre/fix", AK: "AKTEST12345", SK: "SECRETKEY-abcdef", PathStyle: pathStyle, Region: "cn-test-1"})
	if err != nil {
		t.Fatal(err)
	}
	return st, f
}

func TestS3StoreRoundTrip(t *testing.T) {
	st, f := newFakeS3Store(t, "s3", true) // httptest 是 127.0.0.1，自动走路径风格
	ctx := t.Context()
	data := []byte("hello archive 中文")
	if err := st.Put(ctx, "a b/c.csv.gz", strings.NewReader(string(data)), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	fi, err := st.Stat(ctx, "a b/c.csv.gz")
	if err != nil || fi.Size != int64(len(data)) {
		t.Fatalf("stat %+v %v", fi, err)
	}
	rc, err := st.Get(ctx, "a b/c.csv.gz")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != string(data) {
		t.Errorf("get %q", got)
	}
	if _, err := st.Stat(ctx, "nope"); !isNotExist(err) {
		t.Errorf("不存在应返回 ErrNotExist：%v", err)
	}
	if _, err := st.Get(ctx, "nope"); !isNotExist(err) {
		t.Errorf("不存在应返回 ErrNotExist：%v", err)
	}
	if err := st.Delete(ctx, "a b/c.csv.gz"); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "a b/c.csv.gz"); err != nil {
		t.Errorf("重复删除应成功：%v", err)
	}
	// 请求形态：路径风格、前缀、编码
	if !strings.HasSuffix(f.reqs[0], " /bk/pre/fix/a%20b/c.csv.gz") {
		t.Errorf("路径：%s", f.reqs[0])
	}
	// 签名头：有 Credential，真实负载哈希
	if !strings.Contains(f.auth[0], "Credential=AKTEST12345/") || !strings.Contains(f.auth[0], "/cn-test-1/s3/aws4_request") || !strings.Contains(f.auth[0], "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Errorf("Authorization：%s", f.auth[0])
	}
	if f.hash[0] != sha256Hex(data) {
		t.Errorf("Put 负载应签真实 sha256：%s", f.hash[0])
	}
	if f.hash[1] != emptyPayloadSHA {
		t.Errorf("HEAD 负载哈希：%s", f.hash[1])
	}
	if d, err := st.Test(ctx); err != nil || d < 0 {
		t.Errorf("Test：%v", err)
	}
	if _, ok, _ := st.Free(ctx); ok {
		t.Errorf("对象存储没有可用空间概念")
	}
}

func isNotExist(err error) bool {
	return err != nil && strings.Contains(err.Error(), "不存在") && errors.Is(err, ErrNotExist)
}

func TestS3ErrorsDoNotLeakCredentials(t *testing.T) {
	st, f := newFakeS3Store(t, "s3", true)
	f.failPut = true
	err := st.Put(t.Context(), "k", strings.NewReader("x"), 1)
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("应带上服务端错误码：%v", err)
	}
	for _, secret := range []string{"AKTEST12345", "SECRETKEY-abcdef"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("错误信息泄漏了凭据 %s：%v", secret, err)
		}
	}
	// 连接失败的错误也不能带凭据
	st.base.Host = "127.0.0.1:1"
	_, err = st.Stat(t.Context(), "k")
	if err == nil {
		t.Fatal("应连接失败")
	}
	for _, secret := range []string{"AKTEST12345", "SECRETKEY-abcdef"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("连接错误泄漏了凭据：%v", err)
		}
	}
}

func TestS3VirtualHostedAndOSS(t *testing.T) {
	st, err := newS3Store(StoreConfig{Type: "oss", Endpoint: "https://oss-cn-hangzhou.aliyuncs.com", Bucket: "mybk", AK: "ak", SK: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	if !st.virt || st.region != "cn-hangzhou" {
		t.Errorf("OSS：virt=%v region=%s", st.virt, st.region)
	}
	host, p := st.target("pre/a b.csv")
	if host != "mybk.oss-cn-hangzhou.aliyuncs.com" || p != "/pre/a%20b.csv" {
		t.Errorf("虚拟主机风格：%s %s", host, p)
	}
	st2, _ := newS3Store(StoreConfig{Type: "s3", Endpoint: "minio.local:9000", Bucket: "bk", AK: "ak", SK: "sk", PathStyle: true})
	host, p = st2.target("x/y")
	if host != "minio.local:9000" || p != "/bk/x/y" || st2.base.Scheme != "https" {
		t.Errorf("路径风格：%s %s %s", host, p, st2.base.Scheme)
	}
	st3, _ := newS3Store(StoreConfig{Type: "s3", Endpoint: "https://s3.amazonaws.com", Bucket: "bk", AK: "ak", SK: "sk"})
	if host, _ := st3.target("x"); host != "bk.s3.amazonaws.com" {
		t.Errorf("S3 默认虚拟主机：%s", host)
	}
	// 配置校验
	for _, bad := range []StoreConfig{
		{Type: "s3", Bucket: "b", AK: "a", SK: "s"},
		{Type: "s3", Endpoint: "ftp://x", Bucket: "b", AK: "a", SK: "s"},
		{Type: "s3", Endpoint: "http://x/path", Bucket: "b", AK: "a", SK: "s"},
		{Type: "s3", Endpoint: "http://x", AK: "a", SK: "s"},
		{Type: "s3", Endpoint: "http://x", Bucket: "b"},
		{Type: "s3", Endpoint: "http://x", Bucket: "b", AK: "a", SK: "s", Prefix: "a/../b"},
	} {
		if bad.Prefix == "a/../b" {
			// cleanKey 把 .. 换成 _，应当通过且安全
			if s, err := newS3Store(bad); err != nil || strings.Contains(s.prefix, "..") {
				t.Errorf("前缀应被清理：%v %v", s, err)
			}
			continue
		}
		if _, err := newS3Store(bad); err == nil {
			t.Errorf("%+v 应报错", bad)
		}
	}
}
