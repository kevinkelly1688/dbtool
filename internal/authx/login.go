// @author: kevinkelly
// @license: Apache Licence
// @file: login.go
// @time: 2026/10/28

package authx

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"dbtool/internal/audit"
	"dbtool/internal/crypto"
	"dbtool/internal/model"
)

// AuthOK 是认证通过的结果（会话还没有建立：可能还要过两步验证）。
type AuthOK struct {
	User         *model.User
	ProviderID   uint // 0 表示本地账号密码
	ProviderName string
	Created      bool // 本次登录自动创建了用户
	External     bool // 外部身份（SSO / LDAP 目录认证）
	Federated    bool // 经浏览器跳转的 IdP（SSO）登录；LDAP 走账号密码表单，不算
}

// PasswordOpts 是账号密码登录的上下文。
type PasswordOpts struct {
	Local bool   // 来自 /login?local=1（超级用户后门）
	IP    string // 客户端 IP，仅写审计
	API   bool   // JWT 登录：不受登录页表单显示方式的限制
}

var (
	dummyOnce sync.Once
	dummyHash string
)

// burn 做一次等价耗时的哈希计算，避免通过响应时间区分“用户不存在 / 被锁定 / 没有本地密码”。
func burn(password string) {
	dummyOnce.Do(func() { dummyHash, _ = crypto.MakeDjangoPassword("x", "dummysalt", 390000) })
	crypto.CheckDjangoPassword(password, dummyHash)
}

func constEq(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// PasswordLogin 校验账号密码：先本地（pbkdf2_sha256 哈希），本地无此用户或密码不匹配时依次尝试启用的 LDAP 方式。
// 所有“凭据不对”的情形（用户不存在、密码错、被停用、被锁定）都返回同一个 ErrBadCredentials，不泄漏用户是否存在。
// 锁定规则：失败次数 >= security.login_lock_attempts 且 last_login_failed_at 在 security.login_lock_seconds 秒内时拒绝，
// 过了锁定时间下一次尝试会先清零计数。
func (s *Service) PasswordLogin(ctx context.Context, username, password string, o PasswordOpts) (*AuthOK, error) {
	set := s.Settings()
	name, ok := normUsername(username)
	if !ok || password == "" || len(password) > 1024 {
		burn(password)
		return nil, ErrBadCredentials
	}
	page := s.LoginPageInfo(o.Local)
	if !o.API && !page.ShowForm { // 表单被隐藏且不是后门：不受理
		burn(password)
		return nil, ErrBadCredentials
	}
	// 隐藏模式下表单只有 /login?local=1 能用，且只给超级用户（避免绕过“仅 SSO”的要求）。
	doorOnly := !o.API && o.Local && set.LocalFormMode == FormHidden && !page.HasLDAP && len(page.Buttons) > 0

	u, err := s.repo.UserByName(name)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if u != nil && u.FailedLoginCount > 0 && u.LastLoginFailedAt != nil && u.FailedLoginCount >= set.LockCount {
		if u.LastLoginFailedAt.Add(time.Duration(set.LockSeconds) * time.Second).After(now) {
			burn(password)
			s.audit(u.ID, u.Username, u.Display, "登入失败", "账号已被临时锁定") // i18n-ignore 审计文本（存库）
			return nil, ErrBadCredentials
		}
		if err := s.repo.ResetLoginFailures(u.ID); err != nil { // 锁已超时：清零
			return nil, err
		}
		u.FailedLoginCount = 0
	}

	// 1) 本地密码
	if u != nil && strings.HasPrefix(u.Password, "pbkdf2_sha256$") {
		if good := crypto.CheckDjangoPassword(password, u.Password); good && u.IsActive && (!doorOnly || u.IsSuperuser) {
			return &AuthOK{User: u}, nil
		}
	} else {
		burn(password)
	}

	// 2) 目录（LDAP）。超级用户只能本地登录；停用的本地用户不再尝试目录。
	if !doorOnly && (u == nil || (!u.IsSuperuser || !set.SuperuserLocalOnly)) && (u == nil || u.IsActive) {
		for _, p := range s.usable() {
			if p.Kind != KindLDAP {
				continue
			}
			p := p
			id, err := s.ldap.Login(ctx, &p, name, password)
			if err != nil {
				if !errors.Is(err, ErrBadCredentials) {
					slog.Warn("LDAP 认证出错", "provider", p.ID, "name", p.Name, "err", err)
				}
				continue
			}
			ok, err := s.completeExternal(ctx, &p, id, set, false)
			if err != nil {
				var fe *FlowError
				if errors.As(err, &fe) { // 目录认证通过但平台不让登录（停用 / 超级用户 / 不自动创建）：同样只给通用错误
					slog.Info("目录认证通过但平台拒绝登录", "provider", p.ID, "reason", fe.Code)
					break
				}
				return nil, err
			}
			return ok, nil
		}
	}

	s.recordFailure(u, name, o.IP, set, now)
	return nil, ErrBadCredentials
}

func (s *Service) recordFailure(u *model.User, name, ip string, set Settings, now time.Time) {
	if u == nil {
		s.audit(0, name, "", "登入失败", "") // i18n-ignore 审计文本（存库）
		return
	}
	n, err := s.repo.RecordLoginFailure(u.ID, now)
	if err != nil {
		slog.Error("记录登录失败次数出错", "err", err)
	}
	s.audit(u.ID, u.Username, u.Display, "登入失败", "") // i18n-ignore 审计文本（存库）
	if err == nil && n == set.LockCount {
		s.audit(u.ID, u.Username, u.Display, "账号锁定", fmt.Sprintf("连续失败 %d 次，锁定 %d 秒", n, set.LockSeconds)) // i18n-ignore 审计文本（存库）
	}
}

// ---- 外部身份 → 平台用户 ----

// completeExternal 把外部身份映射到平台用户：先查已绑定，再按“匹配依据”找已有用户，找不到再按首次登录策略自动创建；
// 停用用户一律拒绝，超级用户（按设置）只能本地登录。preview=true（测试登录）时不创建、不绑定、不写任何东西。
func (s *Service) completeExternal(ctx context.Context, p *Provider, id *Identity, set Settings, preview bool) (*AuthOK, error) {
	u, via, err := s.matchUser(p, id, set)
	if err != nil {
		return nil, err
	}
	created := false
	if u == nil {
		if p.FirstLogin != FirstLoginCreate {
			return nil, ErrNotProvisioned
		}
		if preview {
			return &AuthOK{User: nil, ProviderID: p.ID, ProviderName: p.Name, External: true}, nil
		}
		if u, err = s.createUser(p, id); err != nil {
			return nil, err
		}
		created = true
		via = "created"
	}
	if !u.IsActive {
		return nil, ErrDisabledUser
	}
	if u.IsSuperuser && set.SuperuserLocalOnly {
		return nil, ErrSuperuserLocal
	}
	if preview {
		return &AuthOK{User: u, ProviderID: p.ID, ProviderName: p.Name, External: true, Created: false}, nil
	}
	if via != "binding" {
		if err := s.repo.BindIdentity(p.ID, id.Subject, u.ID); err != nil {
			slog.Warn("绑定外部身份失败", "provider", p.ID, "uid", u.ID, "err", err)
		}
	}
	s.syncUser(p, id, u, created, set)
	return &AuthOK{User: u, ProviderID: p.ID, ProviderName: p.Name, Created: created, External: true, Federated: p.Kind.IsRedirect()}, nil
}

// matchUser 返回已绑定或按依据匹配到的用户；via 说明依据（binding / third_id / username / email）。
func (s *Service) matchUser(p *Provider, id *Identity, set Settings) (*model.User, string, error) {
	if id.Subject != "" {
		uid, err := s.repo.IdentityUser(p.ID, id.Subject)
		if err != nil {
			return nil, "", err
		}
		if uid != 0 {
			u, err := s.repo.UserByID(uid)
			if err != nil {
				return nil, "", err
			}
			if u != nil {
				return u, "binding", nil
			}
			_ = s.repo.DeleteIdentity(p.ID, id.Subject) // 用户已被删除：清掉悬空绑定
		}
	}
	by := set.MatchBy
	if len(p.Config.MatchBy) > 0 { // 登录方式自己设置了匹配依据：覆盖全局默认
		by = p.Config.MatchBy
	}
	enabled := map[string]bool{}
	for _, m := range by {
		enabled[m] = true
	}
	// 固定从最可靠到最不可靠：第三方 ID → 用户名 → 邮箱。
	if enabled[MatchThirdID] && id.ThirdID != "" && (id.ThirdKind == "ding" || id.ThirdKind == "feishu" || id.ThirdKind == "slack") {
		u, err := s.repo.UserByThirdID(id.ThirdKind, id.ThirdID)
		if err != nil || u != nil {
			return u, MatchThirdID, err
		}
	}
	if enabled[MatchUsername] && id.Username != "" {
		u, err := s.repo.UserByName(id.Username)
		if err != nil || u != nil {
			return u, MatchUsername, err
		}
	}
	// 邮箱：只在 IdP 没有声明“邮箱未验证”，且恰好匹配到一个用户时才采用（多个用户共用邮箱时不猜）。
	if enabled[MatchEmail] && id.Email != "" && id.EmailVerified {
		us, err := s.repo.UsersByEmail(id.Email)
		if err != nil {
			return nil, "", err
		}
		if len(us) == 1 {
			return &us[0], MatchEmail, nil
		}
	}
	// 手机号：与用户已绑定的短信两步验证号码比对，同样只在恰好匹配一个用户时采用。
	if enabled[MatchPhone] && id.Phone != "" {
		if phone, err := NormalizePhone(cleanPhone(id.Phone)); err == nil {
			us, err := s.repo.UsersByPhone(phone)
			if err != nil {
				return nil, "", err
			}
			if len(us) == 1 {
				return &us[0], MatchPhone, nil
			}
		}
	}
	return nil, "", nil
}

// cleanPhone 去掉外部返回的手机号里常见的国家码前缀（+86 / 0086）与分隔符。
func cleanPhone(p string) string {
	p = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(strings.TrimSpace(p))
	for _, pre := range []string{"+86", "0086"} {
		if strings.HasPrefix(p, pre) && len(p)-len(pre) == 11 {
			return p[len(pre):]
		}
	}
	return p
}

func matchLabel(via string) string {
	switch via {
	case "binding":
		return "已绑定的外部身份"
	case MatchThirdID:
		return "第三方 ID"
	case MatchUsername:
		return "用户名"
	case MatchEmail:
		return "邮箱"
	case MatchPhone:
		return "手机号"
	}
	return via
}

// createUser 按首次登录策略自动创建用户：随机不可用密码，写 display / email，入默认权限组与资源组，写审计。
func (s *Service) createUser(p *Provider, id *Identity) (*model.User, error) {
	name := strings.TrimSpace(id.Username)
	if err := validNewUsername(name); err != nil {
		return nil, &FlowError{Code: "username_invalid", Msg: "无法根据外部账号生成合法的平台用户名", Detail: err.Error()}
	}
	display := truncRunes(strings.TrimSpace(id.Display), 10) // dbt_sql_users.display 最长 10 个字符
	if display == "" {
		display = truncRunes(name, 10)
	}
	u := &model.User{Username: name, Password: unusablePassword(), IsActive: true, Display: display,
		Email: truncRunes(strings.TrimSpace(id.Email), 254), DateJoined: s.now()}
	if err := s.repo.CreateUser(u); err != nil {
		return nil, err
	}
	// 记一次登录时间：last_login 为空会触发“首次登录入默认组”，自动创建的用户已经在下面入过组，不要重复。
	now := s.now()
	u.LastLogin = &now
	if err := s.repo.MarkLogin(u.ID, now); err != nil {
		slog.Warn("记录登录时间失败", "uid", u.ID, "err", err)
	}
	s.auditUser(u, "自动创建用户", fmt.Sprintf("登录方式 %s（%s）", p.Name, p.Kind.Label())) // i18n-ignore 审计文本（存库）
	return u, nil
}

// syncUser 登录后的资料同步：入组、姓名邮箱同步、第三方 ID 回填。失败只记日志，不阻止登录。
func (s *Service) syncUser(p *Provider, id *Identity, u *model.User, created bool, set Settings) {
	var authIDs, resIDs []uint
	if created {
		authIDs, resIDs = s.defaultGroups(p, set)
	}
	if created || p.Config.SyncGroups {
		ra, rr := evalGroupRules(p.Config.GroupRules, id.Attrs)
		authIDs, resIDs = append(authIDs, ra...), append(resIDs, rr...)
	}
	if err := s.addGroups(u.ID, authIDs, resIDs); err != nil {
		slog.Warn("入组失败", "uid", u.ID, "err", err)
	}
	if !created && p.Config.UpdateOnLogin {
		d, e := truncRunes(strings.TrimSpace(id.Display), 10), truncRunes(strings.TrimSpace(id.Email), 254)
		if d == "" {
			d = u.Display
		}
		if e == "" {
			e = u.Email
		}
		if d != u.Display || e != u.Email {
			if err := s.repo.UpdateProfile(u.ID, d, e); err != nil {
				slog.Warn("同步用户资料失败", "uid", u.ID, "err", err)
			}
		}
	}
	if set.BackfillIDs && p.Config.Backfill && id.ThirdID != "" {
		cur := map[string]string{"ding": u.DingUserID, "feishu": u.FeishuOpenID, "slack": u.SlackID}
		max := map[string]int{"ding": 20, "feishu": 50, "slack": 50}
		if m, ok := max[id.ThirdKind]; ok && len(id.ThirdID) <= m && cur[id.ThirdKind] != id.ThirdID {
			if err := s.repo.SetThirdID(u.ID, id.ThirdKind, id.ThirdID); err != nil {
				slog.Warn("回填第三方 ID 失败", "uid", u.ID, "err", err)
			}
		}
	}
}

// defaultGroups 返回新用户的默认权限组 / 资源组：登录方式自己配置了就用它的，否则用全局默认（dbt_setting 里按名称）。
func (s *Service) defaultGroups(p *Provider, set Settings) (auth, res []uint) {
	auth, res = p.AuthGroups, p.ResGroups
	if len(auth) == 0 && len(set.DefaultAuthGroups) > 0 {
		auth, _ = s.repo.AuthGroupIDsByName(set.DefaultAuthGroups)
	}
	if len(res) == 0 && len(set.DefaultResourceGroups) > 0 {
		res, _ = s.repo.ResourceGroupIDsByName(set.DefaultResourceGroups)
	}
	return auth, res
}

func (s *Service) addGroups(uid uint, auth, res []uint) error {
	var errs []error
	if len(auth) > 0 {
		errs = append(errs, s.repo.AddUserAuthGroups(uid, uniqUint(auth)))
	}
	if len(res) > 0 {
		errs = append(errs, s.repo.AddUserResourceGroups(uid, uniqUint(res)))
	}
	return errors.Join(errs...)
}

// InitUser 给用户加上全局默认权限组 / 资源组，sysadmin 创建用户时也可以调用。
func (s *Service) InitUser(uid uint) error {
	set := s.Settings()
	auth, _ := s.repo.AuthGroupIDsByName(set.DefaultAuthGroups)
	res, _ := s.repo.ResourceGroupIDsByName(set.DefaultResourceGroups)
	return s.addGroups(uid, auth, res)
}

// CompleteLogin 在会话真正建立时调用（直接登录，或两步验证通过之后）：记录登录时间、清零失败计数、
// 首次登录入默认组、写审计。
func (s *Service) CompleteLogin(uid, providerID uint, ip string) error {
	u, err := s.repo.UserByID(uid)
	if err != nil || u == nil {
		return fmt.Errorf("用户不存在")
	}
	now := s.now()
	if u.LastLogin == nil {
		if err := s.InitUser(u.ID); err != nil {
			slog.Warn("首次登录入默认组失败", "uid", u.ID, "err", err)
		}
	}
	if err := s.repo.MarkLogin(u.ID, now); err != nil {
		return err
	}
	method := "本地" // i18n-ignore 审计文本（存库）
	if providerID != 0 {
		method = "外部" // i18n-ignore 审计文本（存库）
		if p, _ := s.provider(providerID); p != nil {
			method = p.Kind.Label() + ":" + p.Name
		}
		if err := s.repo.TouchProviderLogin(providerID, now); err != nil {
			slog.Warn("记录登录方式最近登录时间失败", "err", err)
		}
		s.invalidateProviders()
	} else if err := s.repo.SetAuthSettings(map[string]string{settingLocalLast: now.Format("2006-01-02 15:04:05")}, ""); err != nil {
		slog.Warn("记录本地登录时间失败", "err", err)
	}
	extra := method
	if ip != "" {
		extra = ip + "（" + method + "）"
	}
	s.audit(u.ID, u.Username, u.Display, "登入", extra) // i18n-ignore 审计文本（存库）
	return nil
}

// CompleteAPILogin 是 JWT 登录成功后的收尾：清零失败计数、写审计（不更新 last_login）。
func (s *Service) CompleteAPILogin(uid, providerID uint, ip string) error {
	u, err := s.repo.UserByID(uid)
	if err != nil || u == nil {
		return fmt.Errorf("用户不存在")
	}
	if u.FailedLoginCount > 0 {
		if err := s.repo.ResetLoginFailures(uid); err != nil {
			return err
		}
	}
	s.audit(u.ID, u.Username, u.Display, "登入", ip+"（API）") // i18n-ignore 审计文本（存库）
	if providerID != 0 {
		_ = s.repo.TouchProviderLogin(providerID, s.now())
		s.invalidateProviders()
	}
	return nil
}

// provider 取一个登录方式（走缓存）。
func (s *Service) provider(id uint) (*Provider, error) {
	list, err := s.providers()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == id {
			p := list[i]
			return &p, nil
		}
	}
	return nil, nil
}

// ---- SSO ----

// ssoState 是发起登录时封进 Cookie 的内容。
type ssoState struct {
	PID      uint   `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"x"`
	Test     bool   `json:"t"`
	Tester   uint   `json:"u"`
	Bind     bool   `json:"b,omitempty"`  // 已登录用户发起的“绑定外部账号”
	BindUID  uint   `json:"bu,omitempty"` // 发起绑定的用户（回调时必须还是同一个会话）
}

const ssoTTL = 10 * time.Minute

// SSOCookieName 是某个登录方式的状态 Cookie 名。
func SSOCookieName(id uint) string { return "dba_sso_" + strconv.FormatUint(uint64(id), 10) }

// SSOStart 是发起登录的结果：把浏览器带到 URL，并写入状态 Cookie。
type SSOStart struct {
	URL         string
	CookieName  string
	CookieValue string
	MaxAge      int
}

// BeginSSOInput 是发起登录的参数。Next 由调用方先过 safeNext；Test 为管理员的“测试登录”，Tester 是管理员用户 ID。
type BeginSSOInput struct {
	Next      string
	Test      bool
	Tester    uint
	UserAgent string
	BaseURL   string // 回调地址的基准（对外地址）
	// Bind / BindUID：已登录用户发起“绑定外部账号”（需要管理员开启“允许用户自行绑定”）。
	Bind    bool
	BindUID uint
}

func (s *Service) callbackURL(base string, id uint) string {
	if base == "" {
		base = s.opt.PublicURL
	}
	return strings.TrimRight(base, "/") + "/login/sso/" + strconv.FormatUint(uint64(id), 10) + "/callback"
}

// CallbackURL 返回登录方式的回调地址（管理页展示，需要填到身份提供方）。
func (s *Service) CallbackURL(base string, id uint) string { return s.callbackURL(base, id) }

// BeginSSO 发起 SSO：生成 state / nonce / PKCE，封进一次性 Cookie，返回授权地址。
func (s *Service) BeginSSO(ctx context.Context, id uint, in BeginSSOInput) (*SSOStart, error) {
	p, err := s.provider(id)
	if err != nil {
		return nil, err
	}
	if p == nil || !p.Kind.IsRedirect() || (!in.Test && !p.Usable(s.fp)) {
		return nil, ErrProviderOff
	}
	if in.Test && in.Tester == 0 {
		return nil, ErrProviderOff
	}
	if in.Bind {
		if in.Test || in.BindUID == 0 {
			return nil, ErrProviderOff
		}
		if !s.Settings().AllowSelfBind {
			return nil, ErrBindOff
		}
	}
	fl := s.flows[p.Kind]
	if fl == nil {
		return nil, ErrProviderOff
	}
	st := ssoState{PID: id, State: randToken(24), Nonce: randToken(24), Verifier: oauth2.GenerateVerifier(), Next: in.Next, Test: in.Test, Tester: in.Tester,
		Bind: in.Bind, BindUID: in.BindUID}
	u, err := fl.Begin(ctx, p, BeginReq{RedirectURI: s.callbackURL(in.BaseURL, id), State: st.State, Nonce: st.Nonce, Verifier: st.Verifier, UserAgent: in.UserAgent})
	if err != nil {
		slog.Warn("发起 SSO 失败", "provider", id, "err", err)
		return nil, &FlowError{Code: "failed", Msg: ErrSSOFailed.Msg, Detail: err.Error()}
	}
	cv, err := s.state.Seal(st, ssoTTL)
	if err != nil {
		return nil, err
	}
	return &SSOStart{URL: u, CookieName: SSOCookieName(id), CookieValue: cv, MaxAge: int(ssoTTL.Seconds())}, nil
}

// SSOResult 是回调处理的结果：测试登录给出报告，正式登录给出认证结果。
type SSOResult struct {
	Test  *TestReport
	Auth  *AuthOK
	Next  string
	Bound bool // 已登录用户的“绑定外部账号”完成（Auth 为空）
}

// markUsed 记录 state 已使用；已用过返回 false（回调重放）。
func (s *Service) markUsed(state string) bool {
	s.usedMu.Lock()
	defer s.usedMu.Unlock()
	now := s.now()
	if len(s.usedStates) > 512 {
		for k, exp := range s.usedStates {
			if now.After(exp) {
				delete(s.usedStates, k)
			}
		}
	}
	if _, dup := s.usedStates[state]; dup {
		return false
	}
	s.usedStates[state] = now.Add(ssoTTL + time.Minute)
	return true
}

// CompleteSSO 处理 IdP 回跳。cookie 是状态 Cookie 的值；tester 是当前会话的用户 ID（测试登录时必须与发起者一致）。
// 任何校验不过都返回对用户安全的 *FlowError，细节只在 Detail 与日志里。
func (s *Service) CompleteSSO(ctx context.Context, id uint, q url.Values, cookie string, tester uint, base string) (*SSOResult, error) {
	return s.CompleteSSOFor(ctx, id, q, cookie, tester, 0, base)
}

// CompleteSSOFor 同 CompleteSSO，另外带上当前会话的用户 ID（sessUID，0 = 未登录）：用于完成“已登录用户绑定外部账号”的回调。
func (s *Service) CompleteSSOFor(ctx context.Context, id uint, q url.Values, cookie string, tester, sessUID uint, base string) (_ *SSOResult, err error) {
	var st ssoState
	defer func() {
		if err != nil && st.Bind {
			err = &BindError{Err: err} // 绑定失败：页面回到“账号绑定”而不是登录页
		}
	}()
	if cookie == "" || s.state.Open(cookie, &st) != nil || st.PID != id || !constEq(q.Get("state"), st.State) {
		return nil, ErrStateInvalid
	}
	if !s.markUsed(st.State) {
		return nil, ErrStateInvalid
	}
	if st.Test && (tester == 0 || tester != st.Tester) {
		return nil, ErrStateInvalid
	}
	if st.Bind && (st.Test || sessUID == 0 || sessUID != st.BindUID) {
		return nil, ErrStateInvalid // 绑定必须在发起绑定的同一个登录会话里完成
	}
	p, err := s.provider(id)
	if err != nil {
		return nil, err
	}
	if p == nil || (!st.Test && !p.Usable(s.fp)) {
		return nil, ErrProviderOff
	}
	set := s.Settings()
	ident, ferr := s.flows[p.Kind].Complete(ctx, p, CompleteReq{RedirectURI: s.callbackURL(base, id), Query: q, State: st.State, Nonce: st.Nonce, Verifier: st.Verifier})
	if st.Test {
		rep := s.reportSSOTest(ctx, p, ident, ferr, set)
		// 管理员发起的测试登录回调也留痕（只写登录方式名与结果；不含授权码、令牌与外部账号属性）
		var actor audit.Actor
		if u, _ := s.repo.UserByID(tester); u != nil {
			actor = audit.Actor{ID: u.ID, Username: u.Username, Display: u.Display, IsSuperuser: u.IsSuperuser}
		} else {
			actor = audit.Actor{ID: tester}
		}
		s.auditActor(actor, "登录方式-测试", fmt.Sprintf("%s（测试登录回调）：%v", p.Name, rep.OK)) // i18n-ignore 审计文本（存库）
		return &SSOResult{Test: rep}, nil
	}
	if ferr != nil {
		slog.Warn("SSO 回调处理失败", "provider", id, "kind", p.Kind, "err", ferr)
		s.audit(0, "", "", "登入失败", fmt.Sprintf("SSO %s：%s", p.Name, ferr.Error())) // i18n-ignore 审计文本（存库）
		return nil, &FlowError{Code: "failed", Msg: ErrSSOFailed.Msg, Detail: ferr.Error()}
	}
	if st.Bind {
		if err := s.completeBind(p, ident, &st, sessUID, set); err != nil {
			return nil, err
		}
		return &SSOResult{Bound: true, Next: st.Next}, nil
	}
	ok, err := s.completeExternal(ctx, p, ident, set, false)
	if err != nil {
		var fe *FlowError
		if errors.As(err, &fe) {
			s.audit(0, ident.Username, "", "登入失败", fmt.Sprintf("SSO %s：%s", p.Name, fe.Code)) // i18n-ignore 审计文本（存库）
		}
		return nil, err
	}
	return &SSOResult{Auth: ok, Next: st.Next}, nil
}
