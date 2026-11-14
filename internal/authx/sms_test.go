// @author: kevinkelly
// @license: Apache Licence
// @file: sms_test.go
// @time: 2026/10/28

package authx

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 阿里云文档里的 RPC 签名示例（DescribeRegions，密钥 testsecret）：签名应为 OLeaidS1JvxuMvnyHOwuJ+uX5qY=。
func TestAliyunSignatureDocExample(t *testing.T) {
	params := map[string]string{"Action": "DescribeRegions", "Format": "XML", "AccessKeyId": "testid", "SignatureMethod": "HMAC-SHA1",
		"SignatureNonce": "3ee8c1b8-83d3-44af-a94f-4e0ad82fd6cf", "SignatureVersion": "1.0", "Timestamp": "2016-02-23T12:46:24Z", "Version": "2014-05-26"}
	if got := aliSign("GET", "testsecret", params); got != "OLeaidS1JvxuMvnyHOwuJ+uX5qY=" {
		t.Errorf("签名与文档示例不一致: %s", got)
	}
}

func TestAliyunSMSRequest(t *testing.T) {
	var got url.Values
	var method, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ctype = r.Method, r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		got, _ = url.ParseQuery(string(raw))
		json.NewEncoder(w).Encode(map[string]string{"Code": "OK", "Message": "OK", "RequestId": "r1"})
	}))
	defer srv.Close()
	clk := newClock()
	env := &flowEnv{HTTP: fixedHTTP(srv.Client()), Now: clk.Now}
	f := newSMSFactory(env)
	f.aliyunURL = srv.URL + "/"
	snd, err := f.Sender(SMSSettings{Provider: SMSAliyun, AliyunKeyID: "LTAI123", AliyunKeySecret: "ali-secret", AliyunSign: "阿里签名", AliyunTemplate: "SMS_1001", AliyunVar: "code"})
	if err != nil {
		t.Fatal(err)
	}
	if err := snd.Send(context.Background(), "13800000000", "123456"); err != nil {
		t.Fatal(err)
	}
	if method != "POST" || !strings.HasPrefix(ctype, "application/x-www-form-urlencoded") {
		t.Errorf("method=%s ctype=%s", method, ctype)
	}
	want := map[string]string{"Action": "SendSms", "Version": "2017-05-25", "AccessKeyId": "LTAI123", "PhoneNumbers": "13800000000", "SignName": "阿里签名",
		"TemplateCode": "SMS_1001", "TemplateParam": `{"code":"123456"}`, "Format": "JSON", "SignatureMethod": "HMAC-SHA1", "SignatureVersion": "1.0",
		"Timestamp": clk.Now().UTC().Format("2006-01-02T15:04:05Z")}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("参数 %s = %q，期望 %q", k, got.Get(k), v)
		}
	}
	if got.Get("SignatureNonce") == "" {
		t.Error("缺少 SignatureNonce")
	}
	// 独立复算签名（按文档的步骤另写一遍）
	var keys []string
	for k := range got {
		if k != "Signature" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var parts []string
	pe := func(s string) string {
		return strings.NewReplacer("+", "%20", "*", "%2A", "%7E", "~").Replace(url.QueryEscape(s))
	}
	for _, k := range keys {
		parts = append(parts, pe(k)+"="+pe(got.Get(k)))
	}
	m := hmac.New(sha1.New, []byte("ali-secret&"))
	m.Write([]byte("POST&%2F&" + pe(strings.Join(parts, "&"))))
	if sig := base64.StdEncoding.EncodeToString(m.Sum(nil)); got.Get("Signature") != sig {
		t.Errorf("签名不对：%s vs %s", got.Get("Signature"), sig)
	}
	// 密钥不在请求里
	for k, vs := range got {
		for _, v := range vs {
			if strings.Contains(v, "ali-secret") {
				t.Errorf("参数 %s 泄漏了 AccessKeySecret", k)
			}
		}
	}
}

func TestAliyunSMSFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"Code": "isv.BUSINESS_LIMIT_CONTROL", "Message": "触发分钟级流控"})
	}))
	defer srv.Close()
	f := newSMSFactory(&flowEnv{HTTP: fixedHTTP(srv.Client()), Now: time.Now})
	f.aliyunURL = srv.URL
	snd, _ := f.Sender(SMSSettings{Provider: SMSAliyun, AliyunKeyID: "k", AliyunKeySecret: "s", AliyunSign: "x", AliyunTemplate: "t"})
	if err := snd.Send(context.Background(), "13800000000", "1"); err == nil || !strings.Contains(err.Error(), "流控") {
		t.Errorf("应返回服务商的错误: %v", err)
	}
	if _, err := f.Sender(SMSSettings{Provider: SMSAliyun, AliyunKeyID: "k"}); err == nil {
		t.Error("配置不完整应报错")
	}
	if _, err := f.Sender(SMSSettings{Provider: SMSDisabled}); err != ErrSMSDisabled {
		t.Errorf("未配置: %v", err)
	}
}

func TestTencentSMSRequestAndSignature(t *testing.T) {
	var hdr http.Header
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		hdr.Set("Host", r.Host)
		body, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"Response":{"SendStatusSet":[{"Code":"Ok","Message":"send success"}],"RequestId":"x"}}`))
	}))
	defer srv.Close()
	clk := newClock()
	f := newSMSFactory(&flowEnv{HTTP: fixedHTTP(srv.Client()), Now: clk.Now})
	f.tencentURL = srv.URL + "/"
	snd, _ := f.Sender(SMSSettings{Provider: SMSTencent, TencentSecretID: "AKIDtest", TencentSecretKey: "tx-secret", TencentSign: "腾讯签名", TencentTemplate: "1234", TencentAppID: "1400000"})
	if err := snd.Send(context.Background(), "13800000000", "654321"); err != nil {
		t.Fatal(err)
	}
	var p struct {
		PhoneNumberSet   []string
		SmsSdkAppId      string
		SignName         string
		TemplateId       string
		TemplateParamSet []string
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.PhoneNumberSet) != 1 || p.PhoneNumberSet[0] != "+8613800000000" || p.SmsSdkAppId != "1400000" || p.SignName != "腾讯签名" || p.TemplateId != "1234" || p.TemplateParamSet[0] != "654321" {
		t.Errorf("请求体: %s", body)
	}
	if hdr.Get("X-TC-Action") != "SendSms" || hdr.Get("X-TC-Version") != "2021-01-11" || hdr.Get("X-TC-Timestamp") != strconv.FormatInt(clk.Now().Unix(), 10) || hdr.Get("X-TC-Region") == "" {
		t.Errorf("公共头: %v", hdr)
	}
	// 独立复算 TC3-HMAC-SHA256（按腾讯云文档步骤）
	host := hdr.Get("Host")
	date := clk.Now().UTC().Format("2006-01-02")
	hp := sha256.Sum256(body)
	canonical := "POST\n/\n\ncontent-type:application/json; charset=utf-8\nhost:" + host + "\nx-tc-action:sendsms\n\ncontent-type;host;x-tc-action\n" + hex.EncodeToString(hp[:])
	ch := sha256.Sum256([]byte(canonical))
	sts := "TC3-HMAC-SHA256\n" + strconv.FormatInt(clk.Now().Unix(), 10) + "\n" + date + "/sms/tc3_request\n" + hex.EncodeToString(ch[:])
	h := func(key []byte, s string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(s))
		return m.Sum(nil)
	}
	sig := hex.EncodeToString(h(h(h(h([]byte("TC3tx-secret"), date), "sms"), "tc3_request"), sts))
	want := fmt.Sprintf("TC3-HMAC-SHA256 Credential=AKIDtest/%s/sms/tc3_request, SignedHeaders=content-type;host;x-tc-action, Signature=%s", date, sig)
	if hdr.Get("Authorization") != want {
		t.Errorf("Authorization 不对：\n got %s\nwant %s", hdr.Get("Authorization"), want)
	}
	if strings.Contains(string(body), "tx-secret") || strings.Contains(hdr.Get("Authorization"), "tx-secret") {
		t.Error("不能泄漏 SecretKey")
	}
	if !regexp.MustCompile(`^application/json`).MatchString(hdr.Get("Content-Type")) {
		t.Error("Content-Type")
	}
}

func TestTencentSMSErrors(t *testing.T) {
	for name, resp := range map[string]string{
		"接口错误":   `{"Response":{"Error":{"Code":"AuthFailure.SignatureFailure","Message":"签名失败"},"RequestId":"x"}}`,
		"发送失败":   `{"Response":{"SendStatusSet":[{"Code":"LimitExceeded.PhoneNumberDailyLimit","Message":"超限"}]}}`,
		"空状态":    `{"Response":{"SendStatusSet":[]}}`,
		"非 JSON": `<html>`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(resp)) }))
		f := newSMSFactory(&flowEnv{HTTP: fixedHTTP(srv.Client()), Now: time.Now})
		f.tencentURL = srv.URL
		snd, _ := f.Sender(SMSSettings{Provider: SMSTencent, TencentSecretID: "i", TencentSecretKey: "k", TencentSign: "s", TencentTemplate: "t", TencentAppID: "a"})
		if err := snd.Send(context.Background(), "+14155550123", "1"); err == nil {
			t.Errorf("%s 应失败", name)
		}
		srv.Close()
	}
	// 国际号码保持原样
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"Response":{"SendStatusSet":[{"Code":"Ok"}]}}`))
	}))
	defer srv.Close()
	f := newSMSFactory(&flowEnv{HTTP: fixedHTTP(srv.Client()), Now: time.Now})
	f.tencentURL = srv.URL
	snd, _ := f.Sender(SMSSettings{Provider: SMSTencent, TencentSecretID: "i", TencentSecretKey: "k", TencentSign: "s", TencentTemplate: "t", TencentAppID: "a"})
	snd.Send(context.Background(), "+14155550123", "1")
	if !strings.Contains(string(body), `"+14155550123"`) {
		t.Errorf("E.164 号码应原样发送: %s", body)
	}
}
