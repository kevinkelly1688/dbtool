// @author: kevinkelly
// @license: Apache Licence
// @file: twofa_test.go
// @time: 2026/10/28

package authx

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// totpCode 用 SHA1、6 位、30 秒的参数生成某一时刻的动态口令。
func totpCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	c, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// RFC 6238 附录 B 的测试向量：密钥 "12345678901234567890"，T=59 秒，SHA1，6 位 → 287082。
func TestTOTPCompatibleWithPyotp(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	e := newTestEnv(t)
	e.clk.t = time.Unix(59, 0)
	if got := totpCode(t, secret, time.Unix(59, 0)); got != "287082" {
		t.Fatalf("测试向量: %s", got)
	}
	if !e.svc.checkTOTP(2, secret, "287082") {
		t.Error("已绑定的密钥与口令应能直接验证")
	}
	// 32 个 base32 字符、无填充的密钥
	if !e.svc.checkTOTP(3, "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP", totpCode(t, "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP", e.clk.Now())) {
		t.Error("32 位 base32 密钥应可用")
	}
}

func TestTOTPWindowsReplayAndFormat(t *testing.T) {
	e := newTestEnv(t)
	secret := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	now := e.clk.Now()
	if e.svc.checkTOTP(2, secret, totpCode(t, secret, now.Add(-90*time.Second))) {
		t.Error("三个窗口以前的口令应失效")
	}
	if e.svc.checkTOTP(2, secret, "12345") || e.svc.checkTOTP(2, secret, "") || e.svc.checkTOTP(2, secret, "abcdef") {
		t.Error("格式不对应拒绝")
	}
	if e.svc.checkTOTP(2, "not base32!", "123456") {
		t.Error("坏密钥应拒绝")
	}
	code := totpCode(t, secret, now)
	if !e.svc.checkTOTP(2, secret, code) {
		t.Fatal("当前窗口应通过")
	}
	if e.svc.checkTOTP(2, secret, code) {
		t.Error("同一窗口的口令不能重复使用（防重放）")
	}
	// 前一个窗口的口令（时钟漂移容忍）在已用窗口之后不能再用
	if e.svc.checkTOTP(2, secret, totpCode(t, secret, now.Add(-30*time.Second))) {
		t.Error("早于已用窗口的口令应拒绝")
	}
	e.clk.Add(30 * time.Second)
	if !e.svc.checkTOTP(2, secret, totpCode(t, secret, e.clk.Now())) {
		t.Error("下一个窗口应通过")
	}
}

func (e *testEnv) pending(uid uint, mode TwoFAMode) *Pending {
	u, _ := e.repo.UserByID(uid)
	v, err := e.svc.NewPending(&AuthOK{User: u}, mode, "/query")
	if err != nil {
		e.t.Fatal(err)
	}
	pd := e.svc.OpenPending(v)
	if pd == nil {
		e.t.Fatal("中间态应可解开")
	}
	return pd
}

func TestTwoFAModeRules(t *testing.T) {
	e := newTestEnv(t)
	u, _ := e.repo.UserByID(2)
	a := &AuthOK{User: u}
	if m, _ := e.svc.TwoFAModeFor(a); m != ModeNone {
		t.Errorf("没绑定也没强制：%q", m)
	}
	e.repo.cfg[cfgEnforce2FA] = "true"
	e.svc.settings.Invalidate()
	if m, _ := e.svc.TwoFAModeFor(a); m != ModeBind {
		t.Errorf("强制且未绑定应先绑定：%q", m)
	}
	e.repo.tfa[2] = []TwoFAConfig{{Type: "totp", Secret: "JBSWY3DPEHPK3PXP"}}
	if m, _ := e.svc.TwoFAModeFor(a); m != ModeVerify {
		t.Errorf("已绑定应验证：%q", m)
	}
	// 外部登录默认不再做两步验证（认为 IdP 已验证），开启开关后才做
	ext := &AuthOK{User: u, External: true, Federated: true, ProviderID: 7}
	if m, _ := e.svc.TwoFAModeFor(ext); m != ModeNone {
		t.Errorf("外部登录默认跳过：%q", m)
	}
	e.repo.kv[settingExt2FA] = "true"
	e.svc.settings.Invalidate()
	if m, _ := e.svc.TwoFAModeFor(ext); m != ModeVerify {
		t.Errorf("开启后外部登录也要验证：%q", m)
	}
	// LDAP 走账号密码表单：与本地账号一样受 2FA 约束，不受“外部登录后仍要求两步验证”开关影响
	e.repo.tfa[2] = []TwoFAConfig{{Type: "totp", Secret: "JBSWY3DPEHPK3PXP"}}
	if m, _ := e.svc.TwoFAModeFor(&AuthOK{User: u, External: true, ProviderID: 7}); m != ModeVerify {
		t.Errorf("LDAP 登录应做两步验证：%q", m)
	}
	e.repo.kv[settingExt2FA] = "false"
	e.svc.settings.Invalidate()
	e.repo.tfa[2] = nil
	if m, _ := e.svc.TwoFAModeFor(ext); m != ModeNone {
		t.Errorf("外部登录且不要求 2FA：%q", m)
	}
}

func TestPendingTokenSecurity(t *testing.T) {
	e := newTestEnv(t)
	pd := e.pending(2, ModeVerify)
	u, _ := e.repo.UserByID(2)
	v, _ := e.svc.NewPending(&AuthOK{User: u}, ModeVerify, "/query")
	if e.svc.OpenPending(v+"x") != nil || e.svc.OpenPending("") != nil || e.svc.OpenPending("garbage") != nil {
		t.Error("篡改 / 乱写的中间态应无效")
	}
	// SSO 状态 Cookie 与中间态互不通用
	st, _ := e.svc.state.Seal(Pending{UID: 1, Mode: ModeVerify}, time.Minute)
	if e.svc.OpenPending(st) != nil {
		t.Error("不同用途的令牌不能互相冒充")
	}
	e.clk.Add(6 * time.Minute)
	if e.svc.OpenPending(v) != nil {
		t.Error("5 分钟后中间态应过期")
	}
	if pd.CSRF == "" || pd.Nonce == "" {
		t.Error("中间态应带 CSRF 与 nonce")
	}
}

func TestBindTOTPFlow(t *testing.T) {
	e := newTestEnv(t)
	pd := e.pending(2, ModeBind)
	setup, err := e.svc.BeginTOTP(pd)
	if err != nil {
		t.Fatal(err)
	}
	if len(setup.Secret) != 32 || !strings.HasPrefix(setup.URL, "otpauth://totp/") || !strings.Contains(setup.URL, "bob") || !strings.HasPrefix(setup.QR, "data:image/png;base64,") {
		t.Fatalf("绑定信息: %+v", setup)
	}
	again, _ := e.svc.BeginTOTP(pd)
	if again.Secret != setup.Secret {
		t.Error("同一次绑定应返回同一个密钥")
	}
	if err := e.svc.VerifyTwoFA(pd, Method2FATOTP, "000000", ""); !errors.Is(err, ErrTwoFABad) {
		t.Errorf("错误口令: %v", err)
	}
	if len(e.repo.tfa[2]) != 0 {
		t.Error("验证通过前不能保存")
	}
	if err := e.svc.VerifyTwoFA(pd, Method2FATOTP, totpCode(t, setup.Secret, e.clk.Now()), ""); err != nil {
		t.Fatal(err)
	}
	if got := e.repo.tfa[2]; len(got) != 1 || got[0].Type != "totp" || got[0].Secret != setup.Secret {
		t.Errorf("应保存 TOTP 配置: %+v", got)
	}
	if !strings.Contains(strings.Join(e.repo.auditActions(), ","), "2FA-绑定") {
		t.Error("绑定应写审计")
	}
	// 绑定模式不能用来验证别人（密钥来自服务端内存，不接受客户端提交）
	pd2 := e.pending(3, ModeBind)
	if err := e.svc.VerifyTwoFA(pd2, Method2FATOTP, totpCode(t, setup.Secret, e.clk.Now().Add(30*time.Second)), ""); err == nil {
		t.Error("没有生成过密钥就不能绑定")
	}
	// 密钥缓存中的绑定用完即丢
	if _, ok := e.svc.twofa.secret(pd.Nonce); ok {
		t.Error("绑定成功后应丢弃内存里的密钥")
	}
}

func TestVerifyTOTPAndLockout(t *testing.T) {
	e := newTestEnv(t)
	secret := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	e.repo.tfa[2] = []TwoFAConfig{{Type: "totp", Secret: secret}}
	pd := e.pending(2, ModeVerify)
	for i := 0; i < 4; i++ {
		if err := e.svc.VerifyTwoFA(pd, Method2FATOTP, "000000", ""); !errors.Is(err, ErrTwoFABad) {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
	}
	if err := e.svc.VerifyTwoFA(pd, Method2FATOTP, "000000", ""); !errors.Is(err, ErrTwoFALocked) {
		t.Errorf("第 5 次错误应锁定: %v", err)
	}
	// 锁定后即使口令正确也拒绝
	if err := e.svc.VerifyTwoFA(pd, Method2FATOTP, totpCode(t, secret, e.clk.Now()), ""); !errors.Is(err, ErrTwoFALocked) {
		t.Errorf("锁定期间应拒绝: %v", err)
	}
	e.clk.Add(11 * time.Minute)
	if err := e.svc.VerifyTwoFA(e.pending(2, ModeVerify), Method2FATOTP, totpCode(t, secret, e.clk.Now()), ""); err != nil {
		t.Errorf("窗口过后应恢复: %v", err)
	}
	// 没绑定该方式
	e.repo.tfa[3] = []TwoFAConfig{{Type: "totp", Secret: secret}}
	if err := e.svc.VerifyTwoFA(e.pending(3, ModeVerify), Method2FASMS, "123456", ""); !errors.Is(err, ErrTwoFABad) {
		t.Errorf("未绑定的方式: %v", err)
	}
	if err := e.svc.VerifyTwoFA(e.pending(3, ModeVerify), "email", "123456", ""); err == nil {
		t.Error("不支持的方式")
	}
}

func (e *testEnv) enableSMS() {
	e.repo.cfg[cfgSMSProvider] = SMSAliyun
	e.svc.settings.Invalidate()
}

func TestSMSFlowAndLimits(t *testing.T) {
	e := newTestEnv(t)
	e.repo.tfa[2] = []TwoFAConfig{{Type: "sms", Phone: "13800000000"}}
	pd := e.pending(2, ModeVerify)
	if err := e.svc.SendSMS(context.Background(), pd, ""); !errors.Is(err, ErrSMSDisabled) {
		t.Fatalf("未配置短信服务: %v", err)
	}
	e.enableSMS()
	ctx := context.Background()
	if err := e.svc.SendSMS(ctx, pd, "99999999999"); err != nil { // 验证模式忽略客户端传来的号码
		t.Fatal(err)
	}
	phone, code := e.sms.last()
	if phone != "13800000000" || !regexp.MustCompile(`^\d{6}$`).MatchString(code) {
		t.Fatalf("应发往绑定的号码，6 位数字: %q %q", phone, code)
	}
	// 同号码 60 秒限频
	err := e.svc.SendSMS(ctx, pd, "")
	var fe *FlowError
	if !errors.As(err, &fe) || fe.Code != "sms_rate" || !strings.Contains(fe.Msg, "秒后再试") {
		t.Errorf("60 秒内不能重复获取: %v", err)
	}
	e.clk.Add(61 * time.Second)
	if err := e.svc.SendSMS(ctx, pd, ""); err != nil {
		t.Fatalf("60 秒后可再获取: %v", err)
	}
	_, code2 := e.sms.last()
	// 旧验证码随新验证码作废（新的覆盖旧的）
	if code2 != code {
		if err := e.svc.VerifyTwoFA(pd, Method2FASMS, code, ""); !errors.Is(err, ErrTwoFABad) {
			t.Errorf("旧验证码应失效: %v", err)
		}
	}
	pd = e.pending(2, ModeVerify)
	e.svc.twofa.reset(2)
	if err := e.svc.VerifyTwoFA(pd, Method2FASMS, code2, ""); err != nil {
		t.Fatalf("正确验证码: %v", err)
	}
	if err := e.svc.VerifyTwoFA(pd, Method2FASMS, code2, ""); !errors.Is(err, ErrTwoFAExpired) {
		t.Errorf("验证码一次一用: %v", err)
	}
}

func TestSMSExpiryAndTries(t *testing.T) {
	e := newTestEnv(t)
	e.enableSMS()
	e.repo.tfa[2] = []TwoFAConfig{{Type: "sms", Phone: "13800000000"}}
	pd := e.pending(2, ModeVerify)
	ctx := context.Background()
	e.svc.SendSMS(ctx, pd, "")
	_, code := e.sms.last()
	e.clk.Add(5*time.Minute + time.Second)
	if err := e.svc.VerifyTwoFA(pd, Method2FASMS, code, ""); !errors.Is(err, ErrTwoFAExpired) {
		t.Errorf("5 分钟后验证码过期: %v", err)
	}
	// 错误次数限制：验证码最多容错 smsMaxTries-1 次后作废（此时账号级锁定也会介入）
	e.clk.Add(2 * time.Minute)
	e.svc.twofa.reset(2)
	e.svc.SendSMS(ctx, pd, "")
	_, code = e.sms.last()
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for i := 0; i < 4; i++ {
		if err := e.svc.VerifyTwoFA(pd, Method2FASMS, wrong, ""); !errors.Is(err, ErrTwoFABad) {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
	}
	if err := e.svc.VerifyTwoFA(pd, Method2FASMS, wrong, ""); !errors.Is(err, ErrTwoFALocked) {
		t.Errorf("第 5 次错误应锁定这次登录: %v", err)
	}
	e.svc.twofa.reset(2)
	if err := e.svc.VerifyTwoFA(pd, Method2FASMS, code, ""); !errors.Is(err, ErrTwoFAExpired) {
		t.Errorf("错误次数用尽后验证码作废: %v", err)
	}
}

func TestSMSSendFailureAndHourlyCap(t *testing.T) {
	e := newTestEnv(t)
	e.enableSMS()
	e.repo.tfa[2] = []TwoFAConfig{{Type: "sms", Phone: "13800000000"}}
	pd := e.pending(2, ModeVerify)
	ctx := context.Background()
	e.sms.err = errors.New("provider down")
	err := e.svc.SendSMS(ctx, pd, "")
	var fe *FlowError
	if !errors.As(err, &fe) || fe.Code != "sms_send" || strings.Contains(fe.Msg, "provider") {
		t.Fatalf("发送失败应给通用提示: %v", err)
	}
	// 发送失败不占用 60 秒限频
	e.sms.err = nil
	if err := e.svc.SendSMS(ctx, pd, ""); err != nil {
		t.Fatalf("失败后应能立即重试: %v", err)
	}
	// 同一用户每小时最多 10 条
	var capped bool
	for i := 0; i < 12; i++ {
		e.clk.Add(61 * time.Second)
		if err := e.svc.SendSMS(ctx, pd, ""); err != nil {
			if errors.As(err, &fe) && fe.Code == "sms_cap" {
				capped = true
			}
			break
		}
	}
	if !capped {
		t.Error("应有每小时发送上限")
	}
}

func TestBindSMSFlow(t *testing.T) {
	e := newTestEnv(t)
	e.enableSMS()
	pd := e.pending(2, ModeBind)
	ms, _ := e.svc.MethodsFor(pd)
	if len(ms) != 2 || ms[0].Code != "totp" || ms[1].Code != "sms" {
		t.Fatalf("绑定模式可选: %+v", ms)
	}
	ctx := context.Background()
	if err := e.svc.SendSMS(ctx, pd, "abc"); !errors.Is(err, ErrPhoneInvalid) {
		t.Errorf("号码格式: %v", err)
	}
	if err := e.svc.SendSMS(ctx, pd, " 138-0000-0001 "); err != nil {
		t.Fatal(err)
	}
	phone, code := e.sms.last()
	if phone != "13800000001" {
		t.Errorf("号码应规整: %q", phone)
	}
	// 验证码绑定到发送时的号码：换号码提交不行
	if err := e.svc.VerifyTwoFA(pd, Method2FASMS, code, "13999999999"); err == nil {
		t.Error("换号码不能用这个验证码")
	}
	e.svc.twofa.reset(2)
	if err := e.svc.VerifyTwoFA(pd, Method2FASMS, code, "13800000001"); err != nil {
		t.Fatal(err)
	}
	if got := e.repo.tfa[2]; len(got) != 1 || got[0].Type != "sms" || got[0].Phone != "13800000001" {
		t.Errorf("应保存短信配置: %+v", got)
	}
	// 别的用户拿不到这个验证码
	pd3 := e.pending(3, ModeBind)
	if err := e.svc.VerifyTwoFA(pd3, Method2FASMS, code, "13800000001"); err == nil {
		t.Error("验证码按用户 + 号码隔离")
	}
	// 短信没配置时绑定模式只有 TOTP
	e.repo.cfg[cfgSMSProvider] = SMSDisabled
	e.svc.settings.Invalidate()
	if ms, _ := e.svc.MethodsFor(pd); len(ms) != 1 {
		t.Errorf("没配短信服务时只能绑 TOTP: %+v", ms)
	}
}

func TestVerifyMethodsListMasksPhone(t *testing.T) {
	e := newTestEnv(t)
	e.repo.tfa[2] = []TwoFAConfig{{Type: "sms", Phone: "13800000000"}, {Type: "totp", Secret: "X"}}
	ms, _ := e.svc.MethodsFor(e.pending(2, ModeVerify))
	if len(ms) != 2 || ms[0].Code != "totp" || ms[1].Phone != "138****0000" {
		t.Errorf("%+v", ms)
	}
}

func TestResetAndAPIOTP(t *testing.T) {
	e := newTestEnv(t)
	secret := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	e.repo.tfa[2] = []TwoFAConfig{{Type: "totp", Secret: secret}, {Type: "sms", Phone: "1"}}
	if err := e.svc.VerifyAPIOTP(2, "bob", "000000"); !errors.Is(err, ErrTwoFABad) {
		t.Errorf("%v", err)
	}
	if err := e.svc.VerifyAPIOTP(2, "bob", totpCode(t, secret, e.clk.Now())); err != nil {
		t.Errorf("%v", err)
	}
	if err := e.svc.VerifyAPIOTP(3, "x", ""); err != nil {
		t.Errorf("没绑定的用户不需要 OTP: %v", err)
	}
	if err := e.svc.ResetTwoFA(rootActor, 2, "bob"); err != nil {
		t.Fatal(err)
	}
	if len(e.repo.tfa[2]) != 0 || !strings.Contains(strings.Join(e.repo.auditActions(), ","), "2FA-重置") {
		t.Error("重置应清空并写审计")
	}
	// 仅绑定了短信的用户，API 没法验证 → 拒绝
	e.repo.tfa[2] = []TwoFAConfig{{Type: "sms", Phone: "1"}}
	if err := e.svc.VerifyAPIOTP(2, "bob", "123456"); !errors.Is(err, ErrTwoFABad) {
		t.Errorf("只绑短信的用户 API 登录无法提供 TOTP: %v", err)
	}
}

func TestPhoneHelpers(t *testing.T) {
	for in, ok := range map[string]bool{"13800000000": true, "+8613800000000": true, "138 0000 0000": true, "138-0000-0000": true, "12345": false, "abc": false, "": false, "+1234567890123456": false} {
		if _, err := NormalizePhone(in); (err == nil) != ok {
			t.Errorf("NormalizePhone(%q) ok=%v", in, err == nil)
		}
	}
	if MaskPhone("13800000000") != "138****0000" || MaskPhone("123") != "***" {
		t.Error("MaskPhone")
	}
}

func TestSealerAndDigits(t *testing.T) {
	clk := newClock()
	s1, _ := newSealer([]byte("k1-k1-k1-k1-k1-k1-k1-k1"), "a", clk.Now)
	s2, _ := newSealer([]byte("k1-k1-k1-k1-k1-k1-k1-k1"), "b", clk.Now)
	s3, _ := newSealer([]byte("k2-k2-k2-k2-k2-k2-k2-k2"), "a", clk.Now)
	tok, _ := s1.Seal(map[string]string{"x": "y"}, time.Minute)
	var out map[string]string
	if err := s1.Open(tok, &out); err != nil || out["x"] != "y" {
		t.Fatal("正常往返")
	}
	if s2.Open(tok, &out) == nil || s3.Open(tok, &out) == nil {
		t.Error("不同用途 / 不同密钥不能解开")
	}
	if strings.Contains(tok, "y") && strings.Contains(tok, `"x"`) {
		t.Error("内容应加密")
	}
	clk.Add(2 * time.Minute)
	if s1.Open(tok, &out) == nil {
		t.Error("过期应失败")
	}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		d := randDigits(6)
		if !regexp.MustCompile(`^\d{6}$`).MatchString(d) {
			t.Fatalf("验证码格式: %q", d)
		}
		seen[d] = true
	}
	if len(seen) < 150 {
		t.Error("验证码应随机")
	}
}
