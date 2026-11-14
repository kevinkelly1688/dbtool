// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

// Package authx 实现可自定义的登录验证方式（LDAP / OIDC / 钉钉 / CAS / 飞书(Lark) / Slack / 企业微信）、
// 登录与安全设置（账号锁定、密码策略、会话与 JWT 有效期等）以及两步验证（TOTP、短信）。
//
// 认证核心（身份映射、状态机、OIDC 校验、2FA）都在本包内、不依赖 server，可以用假仓库与假 IdP 单测；
// http.go 只负责把它们接到 server 的登录流程与管理页面上。
package authx

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Kind 是登录方式的类型。
type Kind string

const (
	KindLDAP     Kind = "ldap"
	KindOIDC     Kind = "oidc"
	KindDingTalk Kind = "dingtalk"
	KindCAS      Kind = "cas"
	KindFeishu   Kind = "feishu" // 含 Lark 国际版（Config.Site = "lark"）
	KindSlack    Kind = "slack"
	KindWeCom    Kind = "wecom"
)

// Kinds 是登录页与管理页里类型的显示顺序。
var Kinds = []Kind{KindLDAP, KindOIDC, KindDingTalk, KindCAS, KindFeishu, KindSlack, KindWeCom}

var kindLabels = map[Kind]string{
	KindLDAP: "LDAP / AD 域", KindOIDC: "OIDC", KindDingTalk: "钉钉", KindCAS: "CAS",
	KindFeishu: "飞书 / Lark", KindSlack: "Slack", KindWeCom: "企业微信",
}

// Label 返回类型的显示名。
func (k Kind) Label() string {
	if l, ok := kindLabels[k]; ok {
		return l
	}
	return string(k)
}

// Valid 判断是否是受支持的类型。
func (k Kind) Valid() bool { _, ok := kindLabels[k]; return ok }

// IsRedirect 表示该类型走浏览器跳转（登录页上是一个按钮）；LDAP 走账号密码表单。
func (k Kind) IsRedirect() bool { return k != KindLDAP }

// 首次登录策略。
const (
	FirstLoginCreate   = "create"   // 外部身份找不到平台用户时自动创建
	FirstLoginExisting = "existing" // 只允许已有用户登录
)

// GroupRule 是入组规则：外部属性满足条件时，把用户加入指定权限组 / 资源组（只增不减）。
type GroupRule struct {
	Attr           string `json:"attr"`            // 外部属性名（如 groups / memberOf / department）
	Op             string `json:"op"`              // eq（任一值等于）/ contains（任一值包含）
	Value          string `json:"value"`           // 比较值
	AuthGroupIDs   []uint `json:"auth_groups"`     // 加入的权限组
	ResourceGroups []uint `json:"resource_groups"` // 加入的资源组
}

// Config 是各类型登录方式的参数（所有类型共用一个结构，按类型取用其中一部分）。
// 带 secret 标记的字段在库里用 mirage 加密保存，接口永不回显。
type Config struct {
	// ---- LDAP ----
	ServerURI      string `json:"server_uri,omitempty"`       // ldap://host:389 或 ldaps://host:636
	ConnSecurity   string `json:"conn_security,omitempty"`    // ldaps / starttls / none（none 即明文）
	BindDN         string `json:"bind_dn,omitempty"`          // 查询账号 DN（先搜后绑方式）
	BindPassword   string `json:"bind_password,omitempty"`    // secret
	SearchBase     string `json:"search_base,omitempty"`      // 用户搜索 Base
	SearchFilter   string `json:"search_filter,omitempty"`    // 如 (sAMAccountName=%(user)s)
	UserDNTemplate string `json:"user_dn_template,omitempty"` // 如 uid=%(user)s,ou=people,dc=example,dc=com（与搜索方式二选一）
	TimeoutSec     int    `json:"timeout_sec,omitempty"`
	// ---- 证书校验（LDAP / CAS / 其他 HTTPS 目标；默认校验）----
	SkipVerify bool `json:"skip_verify,omitempty"`
	// ---- OIDC ----
	WellKnownURL string `json:"wellknown_url,omitempty"`
	Scopes       string `json:"scopes,omitempty"`
	SignAlg      string `json:"sign_alg,omitempty"` // RS256 / ES256
	// ---- 通用 OAuth 类：OIDC / Slack / 钉钉 AppKey / 飞书 App ID ----
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"` // secret（钉钉 AppSecret / 飞书 App Secret / 企业微信应用 Secret）
	// ---- 各家限制 ----
	TeamID    string `json:"team_id,omitempty"`    // Slack 工作区
	CorpID    string `json:"corp_id,omitempty"`    // 钉钉企业 CorpId（可空）/ 企业微信 CorpId
	AgentID   string `json:"agent_id,omitempty"`   // 企业微信 AgentId
	TenantKey string `json:"tenant_key,omitempty"` // 飞书 tenant_key（可空）
	Site      string `json:"site,omitempty"`       // 飞书：feishu / lark
	WeComMode string `json:"wecom_mode,omitempty"` // auto（企业微信内免登 + 浏览器扫码）/ qr（仅扫码）
	// ---- CAS ----
	CASURL       string `json:"cas_url,omitempty"`
	CASVersion   string `json:"cas_version,omitempty"` // 1 / 2 / 3
	ForceHTTPS   bool   `json:"force_https,omitempty"` // 强制 service URL 使用 https
	GlobalLogout bool   `json:"global_logout,omitempty"`
	// ---- 属性映射与入组 ----
	AttrUsername string      `json:"attr_username,omitempty"` // 多个备选用 | 分隔
	AttrDisplay  string      `json:"attr_display,omitempty"`
	AttrEmail    string      `json:"attr_email,omitempty"`
	AttrPhone    string      `json:"attr_phone,omitempty"` // 手机号属性（匹配依据含“手机号”时用；留空用各类型默认）
	GroupRules   []GroupRule `json:"group_rules,omitempty"`
	// MatchBy 是本登录方式自己的“匹配依据”（username / email / third_id / phone）；为空 = 沿用“通用规则”里的全局设置。
	MatchBy       []string `json:"match_by,omitempty"`
	SyncGroups    bool     `json:"sync_groups,omitempty"`     // 每次登录按规则同步入组
	UpdateOnLogin bool     `json:"update_on_login,omitempty"` // 每次登录同步姓名 / 邮箱
	Backfill      bool     `json:"backfill,omitempty"`        // 登录后回填第三方 ID（还要受全局开关约束）
}

// secretFields 返回该类型里需要加密保存、不回显的字段名（json 键）。
func secretFields(k Kind) []string {
	switch k {
	case KindLDAP:
		return []string{"bind_password"}
	case KindCAS:
		return nil
	default:
		return []string{"client_secret"}
	}
}

// secretPtr 按 json 键取 Config 里的密钥字段。
func (c *Config) secretPtr(key string) *string {
	switch key {
	case "bind_password":
		return &c.BindPassword
	case "client_secret":
		return &c.ClientSecret
	}
	return nil
}

// HasSecrets 返回各密钥字段是否已配置（接口里用它代替明文）。
func (c *Config) HasSecrets(k Kind) map[string]bool {
	out := map[string]bool{}
	cc := *c
	for _, f := range secretFields(k) {
		out[f] = *cc.secretPtr(f) != ""
	}
	return out
}

// Provider 是一个已定义的登录方式。Config 为解密后的参数，只在服务内部使用；对外一律用 ProviderView。
type Provider struct {
	ID         uint
	Kind       Kind
	Name       string
	Enabled    bool
	Sort       int
	BtnLabel   string
	FirstLogin string
	AuthGroups []uint // 首次登录自动创建时加入的权限组（空 = 用全局默认）
	ResGroups  []uint // 同上，资源组
	Config     Config

	TestOK     *bool      // 最近一次测试是否通过；nil 表示从未测试
	TestMsg    string     // 最近一次测试的结果说明
	TestTime   *time.Time // 最近一次测试时间
	TestedHash string     // 测试通过时的配置指纹；配置改动后指纹不符，测试结果即失效
	LastLogin  *time.Time // 最近一次成功登录
	CreatedBy  string
	CreateTime time.Time
}

// Usable 表示该方式当前可用于登录：已启用，且测试通过后配置没有再改过。
func (p *Provider) Usable(fp func(*Provider) string) bool {
	return p.Enabled && p.TestOK != nil && *p.TestOK && p.TestedHash != "" && p.TestedHash == fp(p)
}

// Identity 是外部登录成功后得到的身份（已按属性映射归一）。
type Identity struct {
	ProviderID uint
	Kind       Kind
	Subject    string // IdP 内稳定唯一的 ID（OIDC sub / 钉钉 unionId / 飞书 open_id / CAS user / LDAP DN …）
	Username   string
	Display    string
	Email      string
	Phone      string // 外部返回的手机号（匹配依据含“手机号”时与用户绑定的短信号码比对）
	// EmailVerified 为 false 时不允许按邮箱匹配已有用户（IdP 明确声明邮箱未验证）。
	EmailVerified bool
	// ThirdKind/ThirdID 是可回填到用户资料的第三方 ID：ding / feishu / slack / wecom（wecom 没有对应列，仅用于匹配与绑定）。
	ThirdKind string
	ThirdID   string
	Attrs     map[string][]string // 原始属性，入组规则用
}

// ---- 错误 ----

// FlowError 是登录流程里可以安全展示给用户的错误；Code 供页面映射固定文案，Detail 只进日志与测试结果。
type FlowError struct {
	Code   string
	Msg    string
	Detail string
}

func (e *FlowError) Error() string {
	if e.Detail != "" {
		return e.Msg + "：" + e.Detail
	}
	return e.Msg
}

func flowErr(code, msg string) *FlowError { return &FlowError{Code: code, Msg: msg} }

var (
	// ErrBadCredentials 用于所有“账号或密码不对”的情形，包括用户不存在、被停用、被锁定，避免泄漏用户是否存在。
	ErrBadCredentials = flowErr("bad_credentials", "用户名或密码错误")
	ErrNotProvisioned = flowErr("not_provisioned", "该外部账号尚未在平台开通，请联系管理员")
	ErrSuperuserLocal = flowErr("superuser_local", "超级用户只能使用本地账号密码登录")
	ErrDisabledUser   = flowErr("disabled", "该账号已停用")
	ErrStateInvalid   = flowErr("state", "登录请求已失效或不合法，请重新发起登录")
	ErrSSOFailed      = flowErr("failed", "登录失败，请重试或联系管理员")
	ErrProviderOff    = flowErr("provider_off", "该登录方式不可用")
	// ErrUnavailable 表示 IdP / 目录服务暂时不可达。
	ErrUnavailable = errors.New("外部认证服务不可用")
)

// 用户名规范：最长 150 个字符。
const maxUsernameLen = 150

var usernameCharRe = regexp.MustCompile(`^[\p{L}\p{N}@.+_\-]+$`)

// normUsername 清理并校验登录名；不合法返回 ok=false。
func normUsername(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len([]rune(s)) > maxUsernameLen {
		return "", false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return s, true
}

// validNewUsername 校验自动创建 / 注册时的用户名（比登录时更严格）。
func validNewUsername(s string) error {
	if s == "" || len([]rune(s)) > maxUsernameLen {
		return fmt.Errorf("用户名长度需在 1 到 %d 个字符之间", maxUsernameLen)
	}
	if !usernameCharRe.MatchString(s) {
		return errors.New("用户名只能包含字母、数字和 @ . + - _")
	}
	return nil
}

// truncRunes 按字符数截断（dbt_sql_users.display 最长 10 个字符，超长会被严格模式拒绝）。
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
