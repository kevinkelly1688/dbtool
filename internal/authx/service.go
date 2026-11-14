// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package authx

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/i18n"
	"dbtool/internal/model"
)

// Options 是 Service 的可注入依赖。
type Options struct {
	Key       []byte       // 服务端密钥（SECRET_KEY）：派生 Cookie 封装密钥与配置指纹
	PublicURL string       // 对外地址，用来拼 OAuth 回调地址（不含末尾斜杠）
	HTTP      *http.Client // 测试里注入；为空用带超时、不跟随重定向的默认客户端
	Dir       Directory    // 目录服务；为空用 go-ldap 实现
	SMS       SMSFactory   // 短信发送器工厂；为空用阿里云 / 腾讯云实现
	Now       func() time.Time
	// SettingsTTL 是设置与登录方式列表的缓存时间（保存时会立即失效）。
	SettingsTTL time.Duration
}

// Service 是认证核心：登录方式管理、账号密码 / 外部身份登录、安全设置、两步验证。
type Service struct {
	repo Repo
	opt  Options
	now  func() time.Time

	settings *settingsCache
	flows    map[Kind]Flow
	ldap     *ldapFlow

	state *sealer // SSO 状态 Cookie
	tfa   *sealer // 两步验证中间态 Cookie

	provMu    sync.Mutex
	provCache []Provider
	provAt    time.Time
	provOK    bool

	usedMu     sync.Mutex
	usedStates map[string]time.Time // 已用过的 SSO state（防回调重放）

	twofa *twofaState
	sms   SMSFactory

	pwd *pwdLimiter // 自助修改密码：当前密码输错的限频

	mcMu sync.Mutex // “必须修改密码”标记的短缓存（每个请求都要判断，不能每次查库）
	mc   map[uint]mustChangeEntry
}

// NewService 构造服务。
func NewService(repo Repo, o Options) (*Service, error) {
	if len(o.Key) < 16 {
		return nil, errors.New("authx: 服务端密钥过短") // i18n-ignore 启动配置错误，只进日志
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.SettingsTTL <= 0 {
		o.SettingsTTL = 10 * time.Second
	}
	s := &Service{repo: repo, opt: o, now: o.Now, usedStates: map[string]time.Time{}}
	s.settings = newSettingsCache(repo, o.SettingsTTL, o.Now)
	env := &flowEnv{Now: o.Now}
	if o.HTTP != nil {
		env.HTTP = fixedHTTP(o.HTTP)
	} else {
		env.HTTP = newHTTPFactory()
	}
	core := newOIDCCore(env)
	s.flows = map[Kind]Flow{
		KindOIDC: newOIDCFlow(core), KindSlack: newSlackFlow(core), KindDingTalk: newDingFlow(env),
		KindFeishu: newFeishuFlow(env), KindWeCom: newWeComFlow(env), KindCAS: newCASFlow(env),
	}
	dir := o.Dir
	if dir == nil {
		dir = NewLDAPDirectory()
	}
	s.ldap = newLDAPFlow(dir)
	var err error
	if s.state, err = newSealer(o.Key, "sso-state", o.Now); err != nil {
		return nil, err
	}
	if s.tfa, err = newSealer(o.Key, "2fa-pending", o.Now); err != nil {
		return nil, err
	}
	s.sms = o.SMS
	if s.sms == nil {
		s.sms = newSMSFactory(env)
	}
	s.twofa = newTwofaState(o.Now)
	s.pwd = newPwdLimiter(o.Now)
	return s, nil
}

// Settings 返回当前设置快照（带缓存）。
func (s *Service) Settings() Settings { return s.settings.Get() }

// Repo 返回底层仓库（http.go 里取用户列表等需要）。
func (s *Service) Repo() Repo { return s.repo }

// ---- 登录方式列表缓存 ----

const providerCacheTTL = 5 * time.Second

func (s *Service) providers() ([]Provider, error) {
	s.provMu.Lock()
	defer s.provMu.Unlock()
	if s.provOK && s.now().Sub(s.provAt) < providerCacheTTL {
		return s.provCache, nil
	}
	list, err := s.repo.ListProviders()
	if err != nil {
		if s.provOK {
			return s.provCache, nil // 数据库抖动时沿用旧列表
		}
		return nil, err
	}
	s.provCache, s.provAt, s.provOK = list, s.now(), true
	return list, nil
}

func (s *Service) invalidateProviders() {
	s.provMu.Lock()
	s.provOK = false
	s.provMu.Unlock()
}

// fp 是登录方式配置的指纹：测试通过时记录，配置之后再改动则不再匹配，测试结果随之失效。
func (s *Service) fp(p *Provider) string {
	c := p.Config
	// 属性映射、入组规则、同步开关不影响“能不能连上 IdP”，改它们不需要重新测试。
	c.AttrUsername, c.AttrDisplay, c.AttrEmail, c.GroupRules = "", "", "", nil
	c.SyncGroups, c.UpdateOnLogin, c.Backfill = false, false, false
	c.AttrPhone, c.MatchBy = "", nil // 匹配依据同样只影响“登录后怎么找用户”，不影响连通性
	raw, _ := json.Marshal(c)
	return fingerprint(s.opt.Key, string(p.Kind), string(raw))
}

// usable 返回当前可用于登录的方式（启用 + 测试通过 + 配置未变）。
func (s *Service) usable() []Provider {
	list, err := s.providers()
	if err != nil {
		slog.Error("读取登录方式失败", "err", err)
		return nil
	}
	var out []Provider
	for i := range list {
		if list[i].Usable(s.fp) {
			out = append(out, list[i])
		}
	}
	return out
}

// LoginButton 是登录页上的一个外部登录按钮。
type LoginButton struct {
	ID    uint
	Label string
	Kind  Kind
	Name  string // 登录方式名称
	Auto  bool   // Label 是按名称生成的默认文案（“使用{名称}登录”），展示时按界面语言重新生成
}

// LoginPage 是登录页要展示的内容。
type LoginPage struct {
	Buttons      []LoginButton
	HasLDAP      bool
	ShowForm     bool // 是否渲染账号密码表单
	FormExpanded bool // 表单默认展开（否则折叠在“使用平台账号登录”下）
	SignUp       bool
	Local        bool // 本次是 /login?local=1
	FormHint     string
}

// LoginPageInfo 计算登录页内容。local 为 true 表示 /login?local=1（超级用户后门，表单一定显示）。
func (s *Service) LoginPageInfo(local bool) LoginPage {
	set := s.Settings()
	pg := LoginPage{SignUp: set.SignUp, Local: local}
	for _, p := range s.usable() {
		if p.Kind == KindLDAP {
			pg.HasLDAP = true
			continue
		}
		label := p.BtnLabel
		auto := label == ""
		if auto {
			label = i18n.T(i18n.ZH, "使用{name}登录", "name", p.Name)
		}
		pg.Buttons = append(pg.Buttons, LoginButton{ID: p.ID, Label: label, Kind: p.Kind, Name: p.Name, Auto: auto})
	}
	pg.ShowForm = formVisible(set, pg.HasLDAP, len(pg.Buttons) > 0, local)
	pg.FormExpanded = local || len(pg.Buttons) == 0 || set.LocalFormMode == FormAlways
	if pg.HasLDAP {
		pg.FormHint = "本地 / LDAP 域账号"
	}
	return pg
}

// formVisible 决定账号密码表单是否可用：
//   - /login?local=1 永远可用；没有任何可用的外部方式时也一定显示（不能把自己锁在外面）；
//   - 有 LDAP 时必须显示（LDAP 就走这个表单）；
//   - 否则按“本地表单显示方式”：隐藏则只有 /login?local=1 能用。
func formVisible(set Settings, hasLDAP, hasButtons, local bool) bool {
	if local || hasLDAP || !hasButtons {
		return true
	}
	return set.LocalFormMode != FormHidden
}

// ---- 审计 ----

func (s *Service) audit(uid uint, username, display, action, extra string) {
	e := &model.AuditEntry{UserID: uid, UserName: truncRunes(username, 30), UserDisplay: truncRunes(display, 50), Action: truncRunes(action, 255), ExtraInfo: extra}
	if err := s.repo.InsertAudit(e); err != nil {
		slog.Error("写审计日志失败", "action", action, "err", err)
	}
}

func (s *Service) auditActor(a audit.Actor, action, extra string) {
	s.audit(a.ID, a.Username, a.Display, action, extra)
}

func (s *Service) auditUser(u *model.User, action, extra string) {
	s.audit(u.ID, u.Username, u.Display, action, extra)
}

// unusablePassword 返回“不可用密码”（以 ! 开头，任何密码都校验不过）。
func unusablePassword() string { return "!" + randToken(30) }

func (s *Service) String() string { return fmt.Sprintf("authx.Service(%d flows)", len(s.flows)) }
