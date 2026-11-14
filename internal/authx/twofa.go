// @author: kevinkelly
// @license: Apache Licence
// @file: twofa.go
// @time: 2026/10/28

package authx

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"image/png"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"dbtool/internal/audit"
)

// 两步验证：TOTP（SHA1、6 位、30 秒）与短信验证码（6 位、5 分钟有效、同号码 60 秒限频、错误次数限制）。
// 验证码放进程内存（单实例部署，重启后未使用的验证码作废），接口隔离在 twofaState 里，需要多实例时换成共享存储。

// TwoFAMode 是密码通过之后还需要做什么。
type TwoFAMode string

const (
	ModeNone   TwoFAMode = ""       // 直接登录
	ModeVerify TwoFAMode = "verify" // 已绑定：输入验证码
	ModeBind   TwoFAMode = "bind"   // 系统强制两步验证而用户还没绑定：先绑定
)

// 验证方式。
const (
	Method2FATOTP = "totp"
	Method2FASMS  = "sms"
)

const (
	pendingTTL    = 5 * time.Minute // 中间态会话 300 秒
	smsCodeTTL    = 5 * time.Minute
	smsMinGap     = 60 * time.Second
	smsMaxTries   = 5
	smsHourlyCap  = 10
	verifyMaxFail = 5
	verifyWindow  = 10 * time.Minute
	totpPeriod    = 30
)

var (
	ErrTwoFABad     = &FlowError{Code: "2fa_bad", Msg: "验证码不正确"}
	ErrTwoFAExpired = &FlowError{Code: "2fa_expired", Msg: "未获取验证码或验证码已过期"}
	ErrTwoFALocked  = &FlowError{Code: "2fa_locked", Msg: "验证失败次数过多，请稍后重新登录再试"}
	ErrSMSDisabled  = &FlowError{Code: "sms_off", Msg: "系统未配置短信服务商"}
	ErrPhoneInvalid = &FlowError{Code: "phone", Msg: "手机号码格式不正确"}
)

var phoneRe = regexp.MustCompile(`^\+?\d{6,15}$`)

// NormalizePhone 去掉空格和连字符并校验格式。
func NormalizePhone(p string) (string, error) {
	p = strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(p))
	if !phoneRe.MatchString(p) {
		return "", ErrPhoneInvalid
	}
	return p, nil
}

// MaskPhone 把手机号中间几位隐藏。
func MaskPhone(p string) string {
	if len(p) < 7 {
		return strings.Repeat("*", len(p))
	}
	return p[:3] + strings.Repeat("*", len(p)-7) + p[len(p)-4:]
}

// ---- 中间态 ----

// Pending 是“密码已通过、两步验证未完成”的中间态，封在短期 Cookie 里。它不是会话，业务页与 API 都不认它。
type Pending struct {
	UID        uint      `json:"u"`
	Username   string    `json:"n"`
	Mode       TwoFAMode `json:"m"`
	Next       string    `json:"x"`
	CSRF       string    `json:"c"`
	Nonce      string    `json:"k"`
	ProviderID uint      `json:"p"`
}

// NewPending 生成中间态 Cookie 的值。next 应已经过 safeNext。
func (s *Service) NewPending(a *AuthOK, mode TwoFAMode, next string) (string, error) {
	pd := Pending{UID: a.User.ID, Username: a.User.Username, Mode: mode, Next: next, CSRF: randToken(18), Nonce: randToken(18), ProviderID: a.ProviderID}
	return s.tfa.Seal(pd, pendingTTL)
}

// OpenPending 解开中间态 Cookie；无效或过期返回 nil。
func (s *Service) OpenPending(v string) *Pending {
	if v == "" {
		return nil
	}
	var pd Pending
	if s.tfa.Open(v, &pd) != nil || pd.UID == 0 || (pd.Mode != ModeVerify && pd.Mode != ModeBind) {
		return nil
	}
	return &pd
}

// PendingTTL 是中间态 Cookie 的有效期（秒）。
func PendingTTL() int { return int(pendingTTL.Seconds()) }

// TwoFAModeFor 判断密码通过之后是否需要两步验证。
// 规则：用户已绑定任一方式则必须验证；系统开启强制两步验证且用户未绑定则先绑定。
// 经浏览器跳转的外部登录（SSO）默认认为 IdP 已验证身份，除非开启“外部登录后仍要求两步验证”；
// LDAP 走账号密码表单，与本地账号一样受两步验证约束。
func (s *Service) TwoFAModeFor(a *AuthOK) (TwoFAMode, error) {
	set := s.Settings()
	if a.Federated && !set.External2FA {
		return ModeNone, nil
	}
	cfgs, err := s.repo.TwoFAConfigs(a.User.ID)
	if err != nil {
		return ModeNone, err
	}
	switch {
	case len(cfgs) > 0:
		return ModeVerify, nil
	case set.Enforce2FA:
		return ModeBind, nil
	}
	return ModeNone, nil
}

// ---- 可用方式 ----

// TwoFAMethod 是页面上可选的一种方式。
type TwoFAMethod struct {
	Code    string `json:"code"`
	Label   string `json:"label"`
	Phone   string `json:"phone,omitempty"` // 已绑定短信方式时的脱敏手机号
	Configd bool   `json:"configured"`
}

var methodLabels = map[string]string{Method2FATOTP: "Google 身份验证器（TOTP）", Method2FASMS: "短信验证码"}

// MethodsFor 返回当前中间态可选的方式：验证模式列出已绑定的，绑定模式列出可绑定的（短信需要先配置短信服务）。
func (s *Service) MethodsFor(pd *Pending) ([]TwoFAMethod, error) {
	cfgs, err := s.repo.TwoFAConfigs(pd.UID)
	if err != nil {
		return nil, err
	}
	var out []TwoFAMethod
	if pd.Mode == ModeVerify {
		for _, want := range []string{Method2FATOTP, Method2FASMS} {
			for _, c := range cfgs {
				if c.Type == want {
					m := TwoFAMethod{Code: want, Label: methodLabels[want], Configd: true}
					if want == Method2FASMS {
						m.Phone = MaskPhone(c.Phone)
					}
					out = append(out, m)
				}
			}
		}
		return out, nil
	}
	out = append(out, TwoFAMethod{Code: Method2FATOTP, Label: methodLabels[Method2FATOTP]})
	if s.Settings().SMS.Provider != SMSDisabled {
		out = append(out, TwoFAMethod{Code: Method2FASMS, Label: methodLabels[Method2FASMS]})
	}
	return out, nil
}

// ---- TOTP ----

// TOTPSetup 是绑定 TOTP 时展示给用户的内容。
type TOTPSetup struct {
	Secret string `json:"secret"`
	URL    string `json:"otpauth_url"`
	QR     string `json:"qr"` // data:image/png;base64,…
	// QRURL 是同一个二维码，类型为 template.URL：html/template 默认会把 data: 地址过滤掉，这里是我们自己生成的可信内容。
	QRURL template.URL `json:"-"`
}

const totpIssuer = "DBTOOL"

// BeginTOTP 为中间态生成（或返回已生成的）TOTP 密钥与二维码。密钥只放服务端内存，验证通过才写入 dbt_2fa_config。
func (s *Service) BeginTOTP(pd *Pending) (*TOTPSetup, error) {
	secret, ok := s.twofa.secret(pd.Nonce)
	var key *otp.Key
	var err error
	if !ok {
		key, err = totp.Generate(totp.GenerateOpts{Issuer: totpIssuer, AccountName: pd.Username, SecretSize: 20, Period: totpPeriod, Algorithm: otp.AlgorithmSHA1})
		if err != nil {
			return nil, err
		}
		s.twofa.setSecret(pd.Nonce, key.Secret())
	} else if key, err = otp.NewKeyFromURL(fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s", totpIssuer, pd.Username, secret, totpIssuer)); err != nil {
		return nil, err
	}
	img, err := key.Image(224, 224)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	qr := "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	return &TOTPSetup{Secret: key.Secret(), URL: key.URL(), QR: qr, QRURL: template.URL(qr)}, nil
}

// checkTOTP 校验 TOTP：允许前后各一个 30 秒窗口（时钟漂移），并拒绝同一个窗口里的重复使用（防重放）。
func (s *Service) checkTOTP(uid uint, secret, code string) bool {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return false
	}
	now := s.now()
	step := now.Unix() / totpPeriod
	opts := totp.ValidateOpts{Period: totpPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}
	matched := int64(-1)
	for _, d := range []int64{0, -1, 1} { // 先试当前窗口
		want, err := totp.GenerateCodeCustom(secret, time.Unix((step+d)*totpPeriod, 0), opts)
		if err != nil {
			return false // 密钥不是合法的 base32
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 && matched < 0 {
			matched = step + d
		}
	}
	if matched < 0 {
		return false
	}
	return s.twofa.useStep(uid, matched)
}

// ---- 短信 ----

// SMSSender 发送一条验证码短信。
type SMSSender interface {
	Send(ctx context.Context, phone, code string) error
}

// SMSFactory 按当前短信设置给出发送器；未配置时返回 ErrSMSDisabled。
type SMSFactory interface {
	Sender(set SMSSettings) (SMSSender, error)
}

func (s *Service) codeKey(uid uint, phone string) string { return fmt.Sprintf("%d|%s", uid, phone) }

func (s *Service) codeHash(uid uint, phone, code string) string {
	m := hmac.New(sha256.New, s.opt.Key)
	m.Write([]byte(fmt.Sprintf("sms|%d|%s|%s", uid, phone, code)))
	return hex.EncodeToString(m.Sum(nil))
}

// SendSMS 给手机号发验证码。验证模式下手机号取自已绑定的配置（参数 phone 被忽略）；绑定模式下用用户填写的号码。
func (s *Service) SendSMS(ctx context.Context, pd *Pending, phone string) error {
	if pd.Mode == ModeVerify {
		cfgs, err := s.repo.TwoFAConfigs(pd.UID)
		if err != nil {
			return err
		}
		phone = ""
		for _, c := range cfgs {
			if c.Type == Method2FASMS {
				phone = c.Phone
			}
		}
		if phone == "" {
			return ErrTwoFABad
		}
	} else {
		p, err := NormalizePhone(phone)
		if err != nil {
			return err
		}
		phone = p
	}
	sender, err := s.sms.Sender(s.Settings().SMS)
	if err != nil {
		return err
	}
	if wait := s.twofa.reserveSend(pd.UID, phone, s.now()); wait != nil {
		return wait
	}
	code := randDigits(6)
	if err := sender.Send(ctx, phone, code); err != nil {
		s.twofa.releaseSend(phone) // 没发出去不占用 60 秒限频
		return &FlowError{Code: "sms_send", Msg: "短信发送失败，请稍后再试", Detail: err.Error()}
	}
	s.twofa.putCode(s.codeKey(pd.UID, phone), s.codeHash(pd.UID, phone, code), s.now().Add(smsCodeTTL))
	return nil
}

// checkSMS 校验验证码：错误次数达到上限后该验证码作废；成功即销毁（一次一用）。
func (s *Service) checkSMS(uid uint, phone, code string) error {
	switch s.twofa.takeCode(s.codeKey(uid, phone), s.codeHash(uid, phone, strings.TrimSpace(code)), s.now()) {
	case codeOK:
		return nil
	case codeMissing:
		return ErrTwoFAExpired
	}
	return ErrTwoFABad
}

// ---- 校验入口 ----

// VerifyTwoFA 校验验证码。验证模式：校验已绑定的方式；绑定模式：校验通过后把该方式写入 dbt_2fa_config。
// phone 仅绑定短信时使用。连续失败次数过多会锁住这次登录（需重新输入密码）。
func (s *Service) VerifyTwoFA(pd *Pending, method, code, phone string) error {
	if s.twofa.locked(pd.UID, s.now()) {
		return ErrTwoFALocked
	}
	err := s.verify(pd, method, code, phone)
	if err != nil {
		if errors.Is(err, ErrTwoFABad) || errors.Is(err, ErrTwoFAExpired) {
			s.twofa.fail(pd.UID, s.now())
			if s.twofa.locked(pd.UID, s.now()) {
				s.audit(pd.UID, pd.Username, "", "2FA-验证失败过多", method) // i18n-ignore 审计文本（存库）
				return ErrTwoFALocked
			}
		}
		return err
	}
	s.twofa.reset(pd.UID)
	return nil
}

func (s *Service) verify(pd *Pending, method, code, phone string) error {
	cfgs, err := s.repo.TwoFAConfigs(pd.UID)
	if err != nil {
		return err
	}
	find := func(t string) *TwoFAConfig {
		for i := range cfgs {
			if cfgs[i].Type == t {
				return &cfgs[i]
			}
		}
		return nil
	}
	switch method {
	case Method2FATOTP:
		if pd.Mode == ModeVerify {
			c := find(Method2FATOTP)
			if c == nil || c.Secret == "" {
				return ErrTwoFABad
			}
			if !s.checkTOTP(pd.UID, c.Secret, code) {
				return ErrTwoFABad
			}
			return nil
		}
		secret, ok := s.twofa.secret(pd.Nonce)
		if !ok {
			return &FlowError{Code: "2fa_nosetup", Msg: "绑定已过期，请刷新页面重新获取二维码"}
		}
		if !s.checkTOTP(pd.UID, secret, code) {
			return ErrTwoFABad
		}
		if err := s.repo.SaveTwoFA(pd.UID, pd.Username, TwoFAConfig{Type: Method2FATOTP, Secret: secret}); err != nil {
			return err
		}
		s.twofa.dropSecret(pd.Nonce)
		s.audit(pd.UID, pd.Username, "", "2FA-绑定", Method2FATOTP) // i18n-ignore 审计文本（存库）
		return nil
	case Method2FASMS:
		if pd.Mode == ModeVerify {
			c := find(Method2FASMS)
			if c == nil || c.Phone == "" {
				return ErrTwoFABad
			}
			return s.checkSMS(pd.UID, c.Phone, code)
		}
		p, err := NormalizePhone(phone)
		if err != nil {
			return err
		}
		if err := s.checkSMS(pd.UID, p, code); err != nil {
			return err
		}
		if err := s.repo.SaveTwoFA(pd.UID, pd.Username, TwoFAConfig{Type: Method2FASMS, Phone: p}); err != nil {
			return err
		}
		s.audit(pd.UID, pd.Username, "", "2FA-绑定", Method2FASMS) // i18n-ignore 审计文本（存库）
		return nil
	}
	return &FlowError{Code: "2fa_method", Msg: "不支持的验证方式"}
}

// ResetTwoFA 管理员重置某个用户的两步验证（删除其全部绑定），写审计。
func (s *Service) ResetTwoFA(actor audit.Actor, uid uint, username string) error {
	cfgs, err := s.repo.TwoFAConfigs(uid)
	if err != nil {
		return err
	}
	for _, c := range cfgs {
		if err := s.repo.DeleteTwoFA(uid, c.Type); err != nil {
			return err
		}
	}
	s.auditActor(actor, "2FA-重置", username) // i18n-ignore 审计文本（存库）
	return nil
}

// TwoFAState 返回用户当前已绑定的方式（用户自助页面用）。
func (s *Service) TwoFAState(uid uint) (map[string]bool, error) {
	cfgs, err := s.repo.TwoFAConfigs(uid)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{Method2FATOTP: false, Method2FASMS: false}
	for _, c := range cfgs {
		out[c.Type] = true
	}
	return out, nil
}

// HasTOTP 返回用户的 TOTP 密钥（没有为空）与是否绑定了任一方式（JWT 登录要求 OTP 时用）。
func (s *Service) HasTOTP(uid uint) (string, bool, error) {
	cfgs, err := s.repo.TwoFAConfigs(uid)
	if err != nil {
		return "", false, err
	}
	bound := len(cfgs) > 0
	for _, c := range cfgs {
		if c.Type == Method2FATOTP {
			return c.Secret, bound, nil
		}
	}
	return "", bound, nil
}

// VerifyAPIOTP 校验 API 登录时附带的 TOTP（短信无法在非交互的 API 里完成）。
func (s *Service) VerifyAPIOTP(uid uint, username, code string) error {
	if s.twofa.locked(uid, s.now()) {
		return ErrTwoFALocked
	}
	secret, bound, err := s.HasTOTP(uid)
	if err != nil {
		return err
	}
	if !bound {
		return nil
	}
	if secret == "" || !s.checkTOTP(uid, secret, code) {
		s.twofa.fail(uid, s.now())
		return ErrTwoFABad
	}
	s.twofa.reset(uid)
	return nil
}

// ---- 进程内状态 ----

type smsEntry struct {
	hash  string
	exp   time.Time
	tries int
}

type attemptEntry struct {
	n     int
	first time.Time
}

type twofaState struct {
	mu       sync.Mutex
	now      func() time.Time
	codes    map[string]*smsEntry
	lastSent map[string]time.Time // 手机号 → 上次发送时间
	sentHist map[uint][]time.Time // 用户 → 最近一小时的发送时间
	attempts map[uint]*attemptEntry
	secrets  map[string]secretEntry
	lastStep map[uint]int64
}

type secretEntry struct {
	secret string
	exp    time.Time
}

func newTwofaState(now func() time.Time) *twofaState {
	return &twofaState{now: now, codes: map[string]*smsEntry{}, lastSent: map[string]time.Time{}, sentHist: map[uint][]time.Time{},
		attempts: map[uint]*attemptEntry{}, secrets: map[string]secretEntry{}, lastStep: map[uint]int64{}}
}

// gc 清理过期项（调用方持锁）。
func (t *twofaState) gc() {
	now := t.now()
	if len(t.codes)+len(t.secrets)+len(t.lastSent)+len(t.attempts) < 2048 {
		return
	}
	for k, c := range t.codes {
		if now.After(c.exp) {
			delete(t.codes, k)
		}
	}
	for k, s := range t.secrets {
		if now.After(s.exp) {
			delete(t.secrets, k)
		}
	}
	for k, at := range t.lastSent {
		if now.Sub(at) > time.Hour {
			delete(t.lastSent, k)
		}
	}
	for k, a := range t.attempts {
		if now.Sub(a.first) > verifyWindow {
			delete(t.attempts, k)
		}
	}
}

func (t *twofaState) secret(nonce string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.secrets[nonce]
	if !ok || t.now().After(e.exp) {
		return "", false
	}
	return e.secret, true
}

func (t *twofaState) setSecret(nonce, secret string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gc()
	t.secrets[nonce] = secretEntry{secret, t.now().Add(15 * time.Minute)}
}

func (t *twofaState) dropSecret(nonce string) {
	t.mu.Lock()
	delete(t.secrets, nonce)
	t.mu.Unlock()
}

// useStep 记录已使用的 TOTP 时间窗口；窗口不大于上次已用的返回 false（重放）。
func (t *twofaState) useStep(uid uint, step int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.lastStep[uid]; ok && step <= last {
		return false
	}
	t.lastStep[uid] = step
	return true
}

// reserveSend 检查并占用发送额度：同号码 60 秒限频、同用户每小时最多 smsHourlyCap 条。
func (t *twofaState) reserveSend(uid uint, phone string, now time.Time) *FlowError {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gc()
	if at, ok := t.lastSent[phone]; ok && now.Sub(at) < smsMinGap {
		wait := int((smsMinGap - now.Sub(at)).Seconds()) + 1
		return &FlowError{Code: "sms_rate", Msg: fmt.Sprintf("获取验证码太频繁，请于 %d 秒后再试", wait)}
	}
	hist := t.sentHist[uid][:0]
	for _, at := range t.sentHist[uid] {
		if now.Sub(at) < time.Hour {
			hist = append(hist, at)
		}
	}
	if len(hist) >= smsHourlyCap {
		t.sentHist[uid] = hist
		return &FlowError{Code: "sms_cap", Msg: "短信获取次数过多，请一小时后再试"}
	}
	t.sentHist[uid] = append(hist, now)
	t.lastSent[phone] = now
	return nil
}

func (t *twofaState) releaseSend(phone string) {
	t.mu.Lock()
	delete(t.lastSent, phone)
	t.mu.Unlock()
}

func (t *twofaState) putCode(key, hash string, exp time.Time) {
	t.mu.Lock()
	t.codes[key] = &smsEntry{hash: hash, exp: exp}
	t.mu.Unlock()
}

type codeResult int

const (
	codeOK codeResult = iota
	codeMissing
	codeWrong
)

func (t *twofaState) takeCode(key, hash string, now time.Time) codeResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.codes[key]
	if !ok || now.After(e.exp) {
		delete(t.codes, key)
		return codeMissing
	}
	if subtle.ConstantTimeCompare([]byte(e.hash), []byte(hash)) == 1 {
		delete(t.codes, key)
		return codeOK
	}
	e.tries++
	if e.tries >= smsMaxTries {
		delete(t.codes, key)
	}
	return codeWrong
}

func (t *twofaState) locked(uid uint, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	a, ok := t.attempts[uid]
	if !ok {
		return false
	}
	if now.Sub(a.first) > verifyWindow {
		delete(t.attempts, uid)
		return false
	}
	return a.n >= verifyMaxFail
}

func (t *twofaState) fail(uid uint, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a, ok := t.attempts[uid]
	if !ok || now.Sub(a.first) > verifyWindow {
		a = &attemptEntry{first: now}
		t.attempts[uid] = a
	}
	a.n++
}

func (t *twofaState) reset(uid uint) {
	t.mu.Lock()
	delete(t.attempts, uid)
	t.mu.Unlock()
}
