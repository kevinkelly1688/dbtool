// @author: kevinkelly
// @license: Apache Licence
// @file: settings.go
// @time: 2026/10/28

package authx

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/settings"
)

// 本包读写的设置都放系统设置（dbt_setting，见 internal/settings）：键名是 settings.KeyXxx。
// cfg* 是“登录与安全 / 短信”的一组设置，setting* 是另一组（密码策略、会话、水印样式等）；
// 两组的区别只剩读写入口（ConfigValues / AuthSettings），Repo 接口保持不变。
const (
	cfgLockCount     = settings.KeySecurityLoginLockAttempts
	cfgLockSeconds   = settings.KeySecurityLoginLockSeconds
	cfgEnforce2FA    = settings.KeySecurityRequire2FA
	cfgSignUp        = settings.KeySecuritySelfSignup
	cfgWatermark     = settings.KeySecurityWatermarkEnabled
	cfgDefAuthGroup  = settings.KeySecurityDefaultAuthGroups
	cfgDefResGroup   = settings.KeySecurityDefaultResourceGroups
	cfgAPIWhitelist  = settings.KeySecurityAPIUserIDs
	cfgSMSProvider   = settings.KeySMSProvider
	cfgAliKeyID      = settings.KeySMSAliyunKeyID
	cfgAliKeySecret  = settings.KeySMSAliyunKeySecret
	cfgAliSign       = settings.KeySMSAliyunSign
	cfgAliTemplate   = settings.KeySMSAliyunTemplate
	cfgAliVar        = settings.KeySMSAliyunVar
	cfgTxSecretID    = settings.KeySMSTencentSecretID
	cfgTxSecretKey   = settings.KeySMSTencentSecretKey
	cfgTxSign        = settings.KeySMSTencentSign
	cfgTxTemplate    = settings.KeySMSTencentTemplate
	cfgTxSDKAppID    = settings.KeySMSTencentAppID
	settingPwdMinLen = settings.KeySecurityPasswordMinLength
	settingPwdCplx   = settings.KeySecurityPasswordComplexity
	settingSessMin   = settings.KeySecuritySessionMinutes
	settingSessClose = settings.KeySecuritySessionCloseOnExit
	settingJWTHours  = settings.KeySecurityJWTHours
	settingJWTOTP    = settings.KeySecurityAPIJWTRequireOTP
	settingLocalForm = settings.KeySecurityLocalForm
	settingMatchBy   = settings.KeySecurityExternalMatchBy
	settingSuLocal   = settings.KeySecuritySuperuserLocalOnly
	settingExt2FA    = settings.KeySecurityExternal2FA
	settingBackfill  = settings.KeySecurityBackfillIDs
	settingLocalLast = settings.KeySecurityLocalLastLogin
	settingSelfBind  = settings.KeySecurityAllowSelfBind
)

// authSettingKeys 是 AuthSettings() 返回的键（含水印样式）。
var authSettingKeys = []string{settingPwdMinLen, settingPwdCplx, settingSessMin, settingSessClose, settingJWTHours, settingJWTOTP,
	settingLocalForm, settingMatchBy, settingSuLocal, settingExt2FA, settingBackfill, settingLocalLast, settingSelfBind,
	settingWMText, settingWMExtra, settingWMOpacity, settingWMFontSize, settingWMAngle, settingWMGap, settingWMColor}

var sqlConfigKeys = []string{cfgLockCount, cfgLockSeconds, cfgEnforce2FA, cfgSignUp, cfgWatermark, cfgDefAuthGroup, cfgDefResGroup,
	cfgAPIWhitelist, cfgSMSProvider, cfgAliKeyID, cfgAliKeySecret, cfgAliSign, cfgAliTemplate, cfgAliVar,
	cfgTxSecretID, cfgTxSecretKey, cfgTxSign, cfgTxTemplate, cfgTxSDKAppID}

// 本地表单显示方式。
const (
	FormAlways   = "always"   // 始终显示
	FormCollapse = "collapse" // 折叠
	FormHidden   = "hidden"   // 隐藏（仅 /login?local=1）
)

// 外部账号匹配已有用户的依据。
const (
	MatchUsername = "username"
	MatchEmail    = "email"
	MatchThirdID  = "third_id"
	MatchPhone    = "phone" // 手机号：与用户已绑定的短信两步验证号码比对
)

// validMatchBy 判断是否是受支持的匹配依据。
func validMatchBy(m string) bool {
	return m == MatchUsername || m == MatchEmail || m == MatchThirdID || m == MatchPhone
}

// 短信服务商。
const (
	SMSDisabled = "disabled"
	SMSAliyun   = "aliyun"
	SMSTencent  = "tencent"
)

// SMSSettings 是短信服务配置（密钥读出后是明文，仅服务内部使用）。
type SMSSettings struct {
	Provider string

	AliyunKeyID, AliyunKeySecret, AliyunSign, AliyunTemplate, AliyunVar           string
	TencentSecretID, TencentSecretKey, TencentSign, TencentTemplate, TencentAppID string
}

// Settings 是登录与安全设置的快照。
type Settings struct {
	// dbt_setting 里的设置
	LockCount             int
	LockSeconds           int
	Enforce2FA            bool
	SignUp                bool
	Watermark             bool
	DefaultAuthGroups     []string // 权限组名称
	DefaultResourceGroups []string // 资源组名称
	APIWhitelist          []uint   // 用户 ID
	SMS                   SMSSettings

	// 密码策略、会话、水印样式等
	PasswordMinLen      int
	PasswordComplexity  bool
	SessionMinutes      int
	SessionBrowserClose bool
	JWTHours            int
	APIJWTRequireOTP    bool
	LocalFormMode       string
	MatchBy             []string
	SuperuserLocalOnly  bool
	External2FA         bool
	BackfillIDs         bool
	AllowSelfBind       bool // 允许用户自行绑定 / 解绑外部账号（/account/bindings）

	// 页面水印的内容与样式（开关是上面 dbt_setting 里的 Watermark）
	WM WatermarkStyle
}

// DefaultSettings 返回没有任何配置时的默认值（新增项取保守缺省）。
func DefaultSettings() Settings {
	return Settings{
		LockCount: 5, LockSeconds: 300,
		SMS:            SMSSettings{Provider: SMSDisabled, AliyunVar: "code"},
		PasswordMinLen: 9, SessionMinutes: 300, SessionBrowserClose: true, JWTHours: 4,
		LocalFormMode: FormCollapse, MatchBy: []string{MatchUsername, MatchEmail}, SuperuserLocalOnly: true, BackfillIDs: true,
		WM: DefaultWatermark(),
	}
}

// Policy 返回密码策略。
func (s Settings) Policy() PasswordPolicy {
	return PasswordPolicy{MinLen: s.PasswordMinLen, Complexity: s.PasswordComplexity}
}

// SessionTTL / TokenTTL 是会话与 JWT 的有效期。
func (s Settings) SessionTTL() time.Duration { return time.Duration(s.SessionMinutes) * time.Minute }
func (s Settings) TokenTTL() time.Duration   { return time.Duration(s.JWTHours) * time.Hour }

// APIAllowed 判断用户是否在 API 白名单里；白名单为空表示不限制，
// 避免新装环境里没人能用 JWT；要收紧只需配置白名单。
func (s Settings) APIAllowed(uid uint) bool {
	if len(s.APIWhitelist) == 0 {
		return true
	}
	for _, id := range s.APIWhitelist {
		if id == uid {
			return true
		}
	}
	return false
}

// ---- 解析 ----

// parseBool 解析布尔配置：true/True 为真，其余（含空）为假。
func parseBool(s string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	}
	return def
}

func parseInt(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// LoadSettings 从仓库读取设置，缺项用默认值。
func LoadSettings(repo Repo) (Settings, error) {
	cfg, err := repo.ConfigValues(sqlConfigKeys)
	if err != nil {
		return Settings{}, err
	}
	kv, err := repo.AuthSettings()
	if err != nil {
		return Settings{}, err
	}
	return settingsFrom(cfg, kv), nil
}

func settingsFrom(cfg, kv map[string]string) Settings {
	s := DefaultSettings()
	if v, ok := cfg[cfgLockCount]; ok {
		if n := parseInt(v, s.LockCount); n > 0 {
			s.LockCount = n
		}
	}
	if v, ok := cfg[cfgLockSeconds]; ok {
		if n := parseInt(v, s.LockSeconds); n > 0 {
			s.LockSeconds = n
		}
	}
	s.Enforce2FA = parseBool(cfg[cfgEnforce2FA], false)
	s.SignUp = parseBool(cfg[cfgSignUp], false)
	s.Watermark = parseBool(cfg[cfgWatermark], false)
	s.DefaultAuthGroups = splitList(cfg[cfgDefAuthGroup])
	s.DefaultResourceGroups = splitList(cfg[cfgDefResGroup])
	for _, p := range splitList(cfg[cfgAPIWhitelist]) {
		if n, err := strconv.ParseUint(p, 10, 32); err == nil {
			s.APIWhitelist = append(s.APIWhitelist, uint(n))
		}
	}
	if v := strings.TrimSpace(cfg[cfgSMSProvider]); v == SMSAliyun || v == SMSTencent {
		s.SMS.Provider = v
	}
	s.SMS.AliyunKeyID, s.SMS.AliyunKeySecret = cfg[cfgAliKeyID], cfg[cfgAliKeySecret]
	s.SMS.AliyunSign, s.SMS.AliyunTemplate = cfg[cfgAliSign], cfg[cfgAliTemplate]
	if v := strings.TrimSpace(cfg[cfgAliVar]); v != "" {
		s.SMS.AliyunVar = v
	}
	s.SMS.TencentSecretID, s.SMS.TencentSecretKey = cfg[cfgTxSecretID], cfg[cfgTxSecretKey]
	s.SMS.TencentSign, s.SMS.TencentTemplate, s.SMS.TencentAppID = cfg[cfgTxSign], cfg[cfgTxTemplate], cfg[cfgTxSDKAppID]

	if v, ok := kv[settingPwdMinLen]; ok {
		if n := parseInt(v, 0); n >= 6 && n <= 64 {
			s.PasswordMinLen = n
		}
	}
	s.PasswordComplexity = parseBool(kv[settingPwdCplx], s.PasswordComplexity)
	if v, ok := kv[settingSessMin]; ok {
		if n := parseInt(v, 0); n >= 5 && n <= 60*24*30 {
			s.SessionMinutes = n
		}
	}
	s.SessionBrowserClose = parseBool(kv[settingSessClose], s.SessionBrowserClose)
	if v, ok := kv[settingJWTHours]; ok {
		if n := parseInt(v, 0); n >= 1 && n <= 24*30 {
			s.JWTHours = n
		}
	}
	s.APIJWTRequireOTP = parseBool(kv[settingJWTOTP], false)
	if v := kv[settingLocalForm]; v == FormAlways || v == FormCollapse || v == FormHidden {
		s.LocalFormMode = v
	}
	if v, ok := kv[settingMatchBy]; ok {
		var m []string
		for _, p := range splitList(v) {
			if validMatchBy(p) {
				m = append(m, p)
			}
		}
		s.MatchBy = m // 允许为空：不按任何依据匹配，只靠已绑定的外部身份
	}
	s.SuperuserLocalOnly = parseBool(kv[settingSuLocal], s.SuperuserLocalOnly)
	s.External2FA = parseBool(kv[settingExt2FA], false)
	s.BackfillIDs = parseBool(kv[settingBackfill], s.BackfillIDs)
	s.AllowSelfBind = parseBool(kv[settingSelfBind], false)
	s.WM = wmFromKV(kv)
	return s
}

// SecurityInput 是“登录与安全设置”页提交的内容。SMS 密钥留空表示不修改。
type SecurityInput struct {
	LockCount             int      `json:"lock_cnt_threshold"`
	LockSeconds           int      `json:"lock_time_threshold"`
	Enforce2FA            bool     `json:"enforce_2fa"`
	SignUp                bool     `json:"sign_up_enabled"`
	Watermark             bool     `json:"watermark_enabled"`
	PasswordMinLen        int      `json:"password_min_len"`
	PasswordComplexity    bool     `json:"password_complexity"`
	SessionMinutes        int      `json:"session_minutes"`
	SessionBrowserClose   bool     `json:"session_browser_close"`
	JWTHours              int      `json:"jwt_hours"`
	APIJWTRequireOTP      bool     `json:"api_jwt_require_otp"`
	APIWhitelist          []uint   `json:"api_user_whitelist"`
	DefaultAuthGroups     []string `json:"default_auth_group"`
	DefaultResourceGroups []string `json:"default_resource_group"`
	SMSProvider           string   `json:"sms_provider"`

	WatermarkPatch // 页面水印的内容与样式（缺省 = 不修改；见 watermark.go）

	AliyunKeyID      string `json:"aliyun_access_key_id"`
	AliyunKeySecret  string `json:"aliyun_access_key_secret"`
	AliyunSign       string `json:"aliyun_sign_name"`
	AliyunTemplate   string `json:"aliyun_template_code"`
	AliyunVar        string `json:"aliyun_variable_name"`
	TencentSecretID  string `json:"tencent_secret_id"`
	TencentSecretKey string `json:"tencent_secret_key"`
	TencentSign      string `json:"tencent_sign_name"`
	TencentTemplate  string `json:"tencent_template_id"`
	TencentAppID     string `json:"tencent_sdk_appid"`
}

// SecurityView 是返回给页面的设置：不含任何密钥，只有“是否已配置”。
type SecurityView struct {
	SecurityInput                 // 其中的两个短信密钥字段在视图里永远为空
	HasSecrets    map[string]bool `json:"has_secrets"`
}

// View 把设置转成页面用的结构。
func (s Settings) View() SecurityView {
	v := SecurityView{SecurityInput: SecurityInput{
		LockCount: s.LockCount, LockSeconds: s.LockSeconds, Enforce2FA: s.Enforce2FA, SignUp: s.SignUp, Watermark: s.Watermark,
		PasswordMinLen: s.PasswordMinLen, PasswordComplexity: s.PasswordComplexity, SessionMinutes: s.SessionMinutes,
		SessionBrowserClose: s.SessionBrowserClose, JWTHours: s.JWTHours, APIJWTRequireOTP: s.APIJWTRequireOTP,
		APIWhitelist: s.APIWhitelist, DefaultAuthGroups: s.DefaultAuthGroups, DefaultResourceGroups: s.DefaultResourceGroups,
		SMSProvider: s.SMS.Provider,
		AliyunKeyID: s.SMS.AliyunKeyID, AliyunSign: s.SMS.AliyunSign, AliyunTemplate: s.SMS.AliyunTemplate, AliyunVar: s.SMS.AliyunVar,
		TencentSecretID: s.SMS.TencentSecretID, TencentSign: s.SMS.TencentSign, TencentTemplate: s.SMS.TencentTemplate, TencentAppID: s.SMS.TencentAppID,
	}}
	if v.APIWhitelist == nil {
		v.APIWhitelist = []uint{}
	}
	if v.DefaultAuthGroups == nil {
		v.DefaultAuthGroups = []string{}
	}
	if v.DefaultResourceGroups == nil {
		v.DefaultResourceGroups = []string{}
	}
	v.WatermarkPatch = patchOf(s.WM)
	v.HasSecrets = map[string]bool{"aliyun_access_key_secret": s.SMS.AliyunKeySecret != "", "tencent_secret_key": s.SMS.TencentSecretKey != ""}
	return v
}

// validateSecurity 校验并规整输入。
func validateSecurity(in *SecurityInput) error {
	if in.LockCount < 1 || in.LockCount > 100 {
		return errors.New("登录失败锁定次数需在 1 到 100 之间")
	}
	if in.LockSeconds < 10 || in.LockSeconds > 86400 {
		return errors.New("锁定时间需在 10 到 86400 秒之间")
	}
	if in.PasswordMinLen < 6 || in.PasswordMinLen > 64 {
		return errors.New("密码最小长度需在 6 到 64 之间")
	}
	if in.SessionMinutes < 5 || in.SessionMinutes > 60*24*30 {
		return errors.New("会话有效期需在 5 分钟到 30 天之间")
	}
	if in.JWTHours < 1 || in.JWTHours > 24*30 {
		return errors.New("JWT 有效期需在 1 小时到 30 天之间")
	}
	switch in.SMSProvider {
	case "", SMSDisabled:
		in.SMSProvider = SMSDisabled
	case SMSAliyun, SMSTencent:
	default:
		return errors.New("不支持的短信服务商")
	}
	in.AliyunVar = strings.TrimSpace(in.AliyunVar)
	if in.AliyunVar == "" {
		in.AliyunVar = "code"
	}
	in.DefaultAuthGroups = cleanNames(in.DefaultAuthGroups)
	in.DefaultResourceGroups = cleanNames(in.DefaultResourceGroups)
	in.APIWhitelist = uniqUint(in.APIWhitelist)
	for _, f := range []*string{&in.AliyunKeyID, &in.AliyunSign, &in.AliyunTemplate, &in.TencentSecretID, &in.TencentSign, &in.TencentTemplate, &in.TencentAppID} {
		*f = strings.TrimSpace(*f)
		if len(*f) > 200 {
			return errors.New("短信服务参数过长")
		}
	}
	return nil
}

func cleanNames(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range in {
		n = strings.TrimSpace(n)
		if n != "" && !strings.Contains(n, ",") && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func uniqUint(in []uint) []uint {
	seen := map[uint]bool{}
	var out []uint
	for _, n := range in {
		if n != 0 && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func joinUints(ids []uint) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatUint(uint64(id), 10)
	}
	return strings.Join(s, ",")
}

// SaveSecurity 保存安全设置并返回新的快照。cur 是保存前的设置（用来保留留空的 SMS 密钥）。
func SaveSecurity(repo Repo, cur Settings, in SecurityInput, by string) (Settings, error) {
	if err := validateSecurity(&in); err != nil {
		return cur, err
	}
	// 水印内容 / 样式：先校验再写库，非法值整体拒绝（不落库）；没带这些字段时保持不变。
	base := cur.WM
	if base.Validate() != nil {
		base = DefaultWatermark()
	}
	wm, err := in.WatermarkPatch.apply(base)
	if err != nil {
		return cur, err
	}
	if in.AliyunKeySecret == "" {
		in.AliyunKeySecret = cur.SMS.AliyunKeySecret
	}
	if in.TencentSecretKey == "" {
		in.TencentSecretKey = cur.SMS.TencentSecretKey
	}
	cfg := map[string]string{
		cfgLockCount: strconv.Itoa(in.LockCount), cfgLockSeconds: strconv.Itoa(in.LockSeconds),
		cfgEnforce2FA: boolStr(in.Enforce2FA), cfgSignUp: boolStr(in.SignUp), cfgWatermark: boolStr(in.Watermark),
		cfgDefAuthGroup: strings.Join(in.DefaultAuthGroups, ","), cfgDefResGroup: strings.Join(in.DefaultResourceGroups, ","),
		cfgAPIWhitelist: joinUints(in.APIWhitelist), cfgSMSProvider: in.SMSProvider,
		cfgAliKeyID: in.AliyunKeyID, cfgAliKeySecret: in.AliyunKeySecret, cfgAliSign: in.AliyunSign, cfgAliTemplate: in.AliyunTemplate, cfgAliVar: in.AliyunVar,
		cfgTxSecretID: in.TencentSecretID, cfgTxSecretKey: in.TencentSecretKey, cfgTxSign: in.TencentSign, cfgTxTemplate: in.TencentTemplate, cfgTxSDKAppID: in.TencentAppID,
	}
	if err := repo.SetConfigValues(cfg); err != nil {
		return cur, err
	}
	kv := map[string]string{
		settingPwdMinLen: strconv.Itoa(in.PasswordMinLen), settingPwdCplx: boolStr(in.PasswordComplexity),
		settingSessMin: strconv.Itoa(in.SessionMinutes), settingSessClose: boolStr(in.SessionBrowserClose),
		settingJWTHours: strconv.Itoa(in.JWTHours), settingJWTOTP: boolStr(in.APIJWTRequireOTP),
	}
	if !in.WatermarkPatch.empty() {
		for k, v := range wm.kv() {
			kv[k] = v
		}
	}
	if err := repo.SetAuthSettings(kv, by); err != nil {
		return cur, err
	}
	return LoadSettings(repo)
}

// ---- 通用规则（登录方式页下半部分）----

// RulesInput 是“通用规则”：与登录方式相关的全局开关。
type RulesInput struct {
	LocalFormMode      string   `json:"local_form_mode"`
	MatchBy            []string `json:"match_by"`
	SuperuserLocalOnly bool     `json:"superuser_local_only"`
	External2FA        bool     `json:"external_2fa"`
	BackfillIDs        bool     `json:"backfill_ids"`
	SelfBind           bool     `json:"self_bind"` // 允许用户自行绑定 / 解绑外部账号
}

// Rules 返回当前通用规则。
func (s Settings) Rules() RulesInput {
	m := s.MatchBy
	if m == nil {
		m = []string{}
	}
	return RulesInput{LocalFormMode: s.LocalFormMode, MatchBy: m, SuperuserLocalOnly: s.SuperuserLocalOnly, External2FA: s.External2FA, BackfillIDs: s.BackfillIDs, SelfBind: s.AllowSelfBind}
}

// SaveRules 保存通用规则。
func SaveRules(repo Repo, in RulesInput, by string) (Settings, error) {
	switch in.LocalFormMode {
	case FormAlways, FormCollapse, FormHidden:
	default:
		return Settings{}, errors.New("本地表单显示方式不合法")
	}
	var match []string
	seen := map[string]bool{}
	for _, m := range in.MatchBy {
		if !validMatchBy(m) {
			return Settings{}, fmt.Errorf("不支持的匹配依据 %s", strconv.Quote(m))
		}
		if !seen[m] {
			seen[m] = true
			match = append(match, m)
		}
	}
	kv := map[string]string{
		settingLocalForm: in.LocalFormMode, settingMatchBy: strings.Join(match, ","), settingSuLocal: boolStr(in.SuperuserLocalOnly),
		settingExt2FA: boolStr(in.External2FA), settingBackfill: boolStr(in.BackfillIDs), settingSelfBind: boolStr(in.SelfBind),
	}
	if err := repo.SetAuthSettings(kv, by); err != nil {
		return Settings{}, err
	}
	return LoadSettings(repo)
}

// ---- 带缓存的设置读取 ----

// settingsCache 缓存设置快照：保存时立即失效，其他实例最多 ttl 后看到变化。
type settingsCache struct {
	mu   sync.RWMutex
	repo Repo
	ttl  time.Duration
	now  func() time.Time
	cur  Settings
	at   time.Time
	ok   bool
}

func newSettingsCache(repo Repo, ttl time.Duration, now func() time.Time) *settingsCache {
	return &settingsCache{repo: repo, ttl: ttl, now: now}
}

// Get 返回设置；读取失败时沿用上一份（没有就用默认值），不让数据库抖动影响登录。
func (c *settingsCache) Get() Settings {
	c.mu.RLock()
	if c.ok && c.now().Sub(c.at) < c.ttl {
		s := c.cur
		c.mu.RUnlock()
		return s
	}
	c.mu.RUnlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && c.now().Sub(c.at) < c.ttl {
		return c.cur
	}
	s, err := LoadSettings(c.repo)
	if err != nil {
		if !c.ok {
			return DefaultSettings()
		}
		c.at = c.now() // 失败时也推迟下次重试，避免数据库故障时每个请求都去读
		return c.cur
	}
	c.cur, c.at, c.ok = s, c.now(), true
	return s
}

// Set 用新快照直接替换缓存（保存之后调用）。
func (c *settingsCache) Set(s Settings) {
	c.mu.Lock()
	c.cur, c.at, c.ok = s, c.now(), true
	c.mu.Unlock()
}

// Invalidate 让下次读取重新加载。
func (c *settingsCache) Invalidate() {
	c.mu.Lock()
	c.ok = false
	c.mu.Unlock()
}
