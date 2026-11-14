// @author: kevinkelly
// @license: Apache Licence
// @file: sms.go
// @time: 2026/10/28

package authx

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // 阿里云 RPC 签名 v1 规定使用 HMAC-SHA1
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 短信服务：阿里云（Dysmsapi 2017-05-25，RPC 签名 v1）与腾讯云（SMS 2021-01-11，TC3-HMAC-SHA256），
// 都用标准库按各自的签名规范直接调 HTTP API，HTTP 客户端可注入（单测用 httptest 校验签名与参数）。

type smsFactory struct {
	env        *flowEnv
	aliyunURL  string
	tencentURL string
}

func newSMSFactory(env *flowEnv) *smsFactory {
	return &smsFactory{env: env, aliyunURL: "https://dysmsapi.aliyuncs.com/", tencentURL: "https://sms.tencentcloudapi.com/"}
}

// Sender 按设置选择服务商；缺必填项时报错。
func (f *smsFactory) Sender(set SMSSettings) (SMSSender, error) {
	switch set.Provider {
	case SMSAliyun:
		if set.AliyunKeyID == "" || set.AliyunKeySecret == "" || set.AliyunSign == "" || set.AliyunTemplate == "" {
			return nil, &FlowError{Code: "sms_cfg", Msg: "阿里云短信配置不完整"}
		}
		return &aliyunSMS{env: f.env, endpoint: f.aliyunURL, keyID: set.AliyunKeyID, secret: set.AliyunKeySecret,
			sign: set.AliyunSign, template: set.AliyunTemplate, varName: orDefault(set.AliyunVar, "code")}, nil
	case SMSTencent:
		if set.TencentSecretID == "" || set.TencentSecretKey == "" || set.TencentSign == "" || set.TencentTemplate == "" || set.TencentAppID == "" {
			return nil, &FlowError{Code: "sms_cfg", Msg: "腾讯云短信配置不完整"}
		}
		return &tencentSMS{env: f.env, endpoint: f.tencentURL, id: set.TencentSecretID, key: set.TencentSecretKey,
			sign: set.TencentSign, template: set.TencentTemplate, appID: set.TencentAppID, region: "ap-guangzhou"}, nil
	}
	return nil, ErrSMSDisabled
}

// ---------- 阿里云 ----------

type aliyunSMS struct {
	env                           *flowEnv
	endpoint, keyID, secret, sign string
	template, varName             string
}

// aliPercent 是阿里云规定的编码：RFC3986，空格 %20，* 编成 %2A，~ 不编码。
func aliPercent(s string) string {
	s = url.QueryEscape(s)
	s = strings.ReplaceAll(s, "+", "%20")
	s = strings.ReplaceAll(s, "*", "%2A")
	return strings.ReplaceAll(s, "%7E", "~")
}

// aliSign 计算 RPC 签名 v1：HMAC-SHA1(secret+"&", method+"&%2F&"+percent(规范化查询串))。
func aliSign(method, secret string, params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, aliPercent(k)+"="+aliPercent(params[k]))
	}
	sts := method + "&" + aliPercent("/") + "&" + aliPercent(strings.Join(parts, "&"))
	m := hmac.New(sha1.New, []byte(secret+"&"))
	m.Write([]byte(sts))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func (a *aliyunSMS) Send(ctx context.Context, phone, code string) error {
	phone = strings.TrimPrefix(phone, "+86") // 国内号码不带国家码；国际号码需要国际短信模板，这里只处理国内
	param, _ := json.Marshal(map[string]string{a.varName: code})
	params := map[string]string{
		"AccessKeyId": a.keyID, "Action": "SendSms", "Format": "JSON", "PhoneNumbers": phone, "RegionId": "cn-hangzhou",
		"SignName": a.sign, "SignatureMethod": "HMAC-SHA1", "SignatureNonce": randToken(16), "SignatureVersion": "1.0",
		"TemplateCode": a.template, "TemplateParam": string(param), "Timestamp": a.env.Now().UTC().Format("2006-01-02T15:04:05Z"), "Version": "2017-05-25",
	}
	params["Signature"] = aliSign(http.MethodPost, a.secret, params)
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	var out struct {
		Code    string `json:"Code"`
		Message string `json:"Message"`
	}
	res, err := doRequest(ctx, a.env.HTTP(false), http.MethodPost, a.endpoint, nil, form)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(res.Body, &out); err != nil {
		return fmt.Errorf("阿里云短信返回了无法解析的响应（HTTP %d）", res.Status)
	}
	if out.Code != "OK" {
		return fmt.Errorf("阿里云短信发送失败：%s %s", sanitizeCode(out.Code), truncRunes(out.Message, 80))
	}
	return nil
}

// ---------- 腾讯云 ----------

type tencentSMS struct {
	env                                      *flowEnv
	endpoint, id, key, sign, template, appID string
	region                                   string
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// tc3Authorization 按 TC3-HMAC-SHA256 规范生成 Authorization 头。
func tc3Authorization(secretID, secretKey, service, host, action string, payload []byte, ts time.Time) string {
	date := ts.UTC().Format("2006-01-02")
	signedHeaders := "content-type;host;x-tc-action"
	canonical := strings.Join([]string{
		"POST", "/", "",
		"content-type:application/json; charset=utf-8\nhost:" + host + "\nx-tc-action:" + strings.ToLower(action) + "\n",
		signedHeaders, sha256Hex(payload),
	}, "\n")
	scope := date + "/" + service + "/tc3_request"
	sts := "TC3-HMAC-SHA256\n" + strconv.FormatInt(ts.Unix(), 10) + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	kDate := hmacSHA256([]byte("TC3"+secretKey), date)
	kService := hmacSHA256(kDate, service)
	kSigning := hmacSHA256(kService, "tc3_request")
	sig := hex.EncodeToString(hmacSHA256(kSigning, sts))
	return fmt.Sprintf("TC3-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", secretID, scope, signedHeaders, sig)
}

func (t *tencentSMS) Send(ctx context.Context, phone, code string) error {
	if !strings.HasPrefix(phone, "+") {
		phone = "+86" + phone // 腾讯云要求 E.164；不带 + 的按大陆号码处理
	}
	payload, _ := json.Marshal(map[string]any{
		"PhoneNumberSet": []string{phone}, "SmsSdkAppId": t.appID, "SignName": t.sign, "TemplateId": t.template, "TemplateParamSet": []string{code},
	})
	u, err := url.Parse(t.endpoint)
	if err != nil {
		return err
	}
	ts := t.env.Now()
	hdr := map[string]string{
		"Authorization": tc3Authorization(t.id, t.key, "sms", u.Host, "SendSms", payload, ts),
		"Content-Type":  "application/json; charset=utf-8",
		"X-TC-Action":   "SendSms", "X-TC-Version": "2021-01-11", "X-TC-Timestamp": strconv.FormatInt(ts.Unix(), 10), "X-TC-Region": t.region,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return cleanErr(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := t.env.HTTP(false).Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, cleanErr(err))
	}
	defer resp.Body.Close()
	var out struct {
		Response struct {
			Error *struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
			SendStatusSet []struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"SendStatusSet"`
		} `json:"Response"`
	}
	if err := json.NewDecoder(limitBody(resp.Body)).Decode(&out); err != nil {
		return fmt.Errorf("腾讯云短信返回了无法解析的响应（HTTP %d）", resp.StatusCode)
	}
	if e := out.Response.Error; e != nil {
		return fmt.Errorf("腾讯云短信发送失败：%s %s", sanitizeCode(e.Code), truncRunes(e.Message, 80))
	}
	if len(out.Response.SendStatusSet) == 0 {
		return errors.New("腾讯云短信没有返回发送状态")
	}
	if st := out.Response.SendStatusSet[0]; st.Code != "Ok" {
		return fmt.Errorf("腾讯云短信发送失败：%s %s", sanitizeCode(st.Code), truncRunes(st.Message, 80))
	}
	return nil
}
