// @author: kevinkelly
// @license: Apache Licence
// @file: account.go
// @time: 2026/10/28

package authx

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/crypto"
	"dbtool/internal/model"
)

// 个人账号：自助修改密码、强制改密、外部账号绑定 / 解绑、页面水印的用户变量。

// ---- 自助修改密码 ----

var (
	// 以下错误信息对已登录的本人可以直接展示：不涉及账号是否存在。
	ErrPwdCurrentBad = &FlowError{Code: "pwd_current", Msg: "当前密码不正确"}
	ErrPwdTooMany    = &FlowError{Code: "pwd_limit", Msg: "尝试次数过多，请稍后再试"}
	ErrPwdExternal   = &FlowError{Code: "pwd_external", Msg: "该账号使用外部认证登录，没有平台密码，请到身份提供方修改密码"}
)

func pwdErr(msg string) *FlowError { return &FlowError{Code: "pwd_input", Msg: msg} }

// 自助改密时“当前密码”的失败限频：同一用户 10 分钟内最多 5 次。
const (
	pwdMaxFail = 5
	pwdWindow  = 10 * time.Minute
)

type pwdLimiter struct {
	mu    sync.Mutex
	now   func() time.Time
	fails map[uint][]time.Time
}

func newPwdLimiter(now func() time.Time) *pwdLimiter {
	return &pwdLimiter{now: now, fails: map[uint][]time.Time{}}
}

func (l *pwdLimiter) prune(uid uint) []time.Time {
	now := l.now()
	kept := l.fails[uid][:0]
	for _, t := range l.fails[uid] {
		if now.Sub(t) < pwdWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, uid)
	} else {
		l.fails[uid] = kept
	}
	return kept
}

func (l *pwdLimiter) blocked(uid uint) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(uid)) >= pwdMaxFail
}

func (l *pwdLimiter) fail(uid uint) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[uid] = append(l.prune(uid), l.now())
}

func (l *pwdLimiter) reset(uid uint) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, uid)
}

// hasLocalPassword 表示用户有可用的本地密码（pbkdf2_sha256 哈希）。外部认证自动创建的用户是“不可用密码”（以 ! 开头）。
func hasLocalPassword(u *model.User) bool { return strings.HasPrefix(u.Password, "pbkdf2_sha256$") }

// ChangePassword 是登录用户自助修改本地密码：校验当前密码、新密码走密码策略（CheckPassword 同一套）、两次输入一致、不能与当前相同。
// 成功后写审计并清除“必须修改密码”标记。会话是无状态的签名 Cookie，不能让其他设备上的会话失效，只保证当前会话不受影响。
// 所有失败信息都不含账号是否存在、哈希等细节；“当前密码”连续输错会被限频。
func (s *Service) ChangePassword(uid uint, current, newPwd, confirm, ip string) error {
	u, err := s.repo.UserByID(uid)
	if err != nil {
		return err
	}
	if u == nil || !u.IsActive {
		return &FlowError{Code: "pwd_user", Msg: "账号不可用"}
	}
	if !hasLocalPassword(u) {
		return ErrPwdExternal
	}
	if s.pwd.blocked(uid) {
		s.audit(u.ID, u.Username, u.Display, "修改密码失败", "尝试次数过多") // i18n-ignore 审计文本（存库）
		return ErrPwdTooMany
	}
	switch {
	case current == "" || newPwd == "":
		return pwdErr("请填写当前密码和新密码")
	case len(current) > 1024 || len(newPwd) > 1024:
		return pwdErr("密码过长")
	case newPwd != confirm:
		return pwdErr("两次输入的新密码不一致")
	}
	if !crypto.CheckDjangoPassword(current, u.Password) {
		s.pwd.fail(uid)
		s.audit(u.ID, u.Username, u.Display, "修改密码失败", "当前密码不正确") // i18n-ignore 审计文本（存库）
		return ErrPwdCurrentBad
	}
	if newPwd == current {
		return pwdErr("新密码不能与当前密码相同")
	}
	if err := ValidatePassword(s.Settings().Policy(), newPwd, u.Username, u.Display, strings.SplitN(u.Email, "@", 2)[0]); err != nil {
		return &FlowError{Code: "pwd_policy", Msg: err.Error()}
	}
	hash, err := crypto.MakeDjangoPassword(newPwd, randToken(12), 600000)
	if err != nil {
		return err
	}
	if err := s.repo.SetPassword(u.ID, hash); err != nil {
		return err
	}
	s.pwd.reset(uid)
	s.clearMustChange(u.ID)
	extra := "自助修改" // i18n-ignore 审计文本（存库）
	if ip != "" {
		extra = ip + "（自助修改）" // i18n-ignore 审计文本（存库）
	}
	s.audit(u.ID, u.Username, u.Display, "修改密码", extra) // i18n-ignore 审计文本（存库）
	return nil
}

// ---- 强制改密 ----

const mustChangeTTL = 15 * time.Second

type mustChangeEntry struct {
	v  bool
	at time.Time
}

// MustChangePassword 判断用户是否被要求先修改密码（系统管理用“系统生成临时密码”重置后置位，用户自己改密成功后清除）。
// 带 15 秒缓存，仓库不支持旁路表或表不存在时恒为 false。
func (s *Service) MustChangePassword(uid uint) bool {
	pr, ok := s.repo.(ProfileRepo)
	if !ok || uid == 0 {
		return false
	}
	s.mcMu.Lock()
	if e, ok := s.mc[uid]; ok && s.now().Sub(e.at) < mustChangeTTL {
		s.mcMu.Unlock()
		return e.v
	}
	s.mcMu.Unlock()
	v := pr.MustChangePassword(uid)
	s.mcMu.Lock()
	if s.mc == nil || len(s.mc) > 4096 {
		s.mc = map[uint]mustChangeEntry{}
	}
	s.mc[uid] = mustChangeEntry{v: v, at: s.now()}
	s.mcMu.Unlock()
	return v
}

func (s *Service) clearMustChange(uid uint) {
	if pr, ok := s.repo.(ProfileRepo); ok {
		if err := pr.ClearMustChange(uid); err != nil {
			slog.Warn("清除必须修改密码标记失败", "uid", uid, "err", err)
		}
	}
	s.mcMu.Lock()
	delete(s.mc, uid)
	s.mcMu.Unlock()
}

// ---- 账号信息（个人菜单 / 账号页用）----

// AccountInfo 描述当前用户的认证方式，个人菜单据此决定是否显示“修改密码”。
type AccountInfo struct {
	LocalPassword  bool     `json:"local_password"`   // 有本地密码：可以自助修改密码
	AuthMethod     string   `json:"auth_method"`      // local / external
	AuthMethodText string   `json:"auth_method_text"` // 本地账号密码 / 外部（LDAP：公司 LDAP …）
	Bound          []string `json:"bound_providers"`  // 已绑定的外部登录方式名称
	SelfBind       bool     `json:"self_bind"`        // 管理员允许用户自行绑定 / 解绑
	MustChange     bool     `json:"must_change_password"`
}

// Account 返回用户的认证方式摘要。
func (s *Service) Account(uid uint) (*AccountInfo, error) {
	u, err := s.repo.UserByID(uid)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, &FlowError{Code: "pwd_user", Msg: "账号不可用"}
	}
	info := &AccountInfo{LocalPassword: hasLocalPassword(u), SelfBind: s.Settings().AllowSelfBind, Bound: []string{}}
	ids, err := s.repo.IdentitiesByUser(uid)
	if err != nil {
		return nil, err
	}
	names := map[uint]string{}
	if list, err := s.providers(); err == nil {
		for _, p := range list {
			names[p.ID] = p.Name
		}
	}
	for _, b := range ids {
		if n := names[b.ProviderID]; n != "" {
			info.Bound = append(info.Bound, n)
		}
	}
	switch {
	case info.LocalPassword:
		info.AuthMethod, info.AuthMethodText = "local", "本地账号密码"
	default:
		info.AuthMethod, info.AuthMethodText = "external", "外部认证"
		if len(info.Bound) > 0 {
			info.AuthMethodText = fmt.Sprintf("外部认证（%s）", strings.Join(info.Bound, "、"))
		}
	}
	info.MustChange = info.LocalPassword && s.MustChangePassword(uid)
	return info, nil
}

// ---- 账号绑定 ----

// BindingView 是已绑定的一个外部账号。
type BindingView struct {
	ID           uint   `json:"id"`
	ProviderID   uint   `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	KindLabel    string `json:"kind_label"`
	BoundAt      string `json:"bound_at"`
}

// BindChoice 是当前可以绑定的登录方式（需要浏览器跳转的、已启用且测试通过的、用户还没绑定过的）。
type BindChoice struct {
	ProviderID uint   `json:"provider_id"`
	Name       string `json:"name"`
	KindLabel  string `json:"kind_label"`
	Href       string `json:"href"`
}

// BindingsPage 是 /account/bindings 页面的数据。
type BindingsPage struct {
	Allowed   bool          `json:"allowed"` // 管理员是否允许用户自行绑定 / 解绑
	Items     []BindingView `json:"items"`
	Available []BindChoice  `json:"available"`
}

// Bindings 列出用户已绑定的外部账号，以及还能绑定的登录方式。
func (s *Service) Bindings(uid uint) (*BindingsPage, error) {
	set := s.Settings()
	pg := &BindingsPage{Allowed: set.AllowSelfBind, Items: []BindingView{}, Available: []BindChoice{}}
	ids, err := s.repo.IdentitiesByUser(uid)
	if err != nil {
		return nil, err
	}
	all, err := s.repo.ListProviders()
	if err != nil {
		return nil, err
	}
	byID := map[uint]*Provider{}
	for i := range all {
		byID[all[i].ID] = &all[i]
	}
	bound := map[uint]bool{}
	for _, b := range ids {
		bound[b.ProviderID] = true
		v := BindingView{ID: b.ID, ProviderID: b.ProviderID, BoundAt: b.CreateTime.Format(timeFmt)}
		if p := byID[b.ProviderID]; p != nil {
			v.ProviderName, v.KindLabel = p.Name, p.Kind.Label()
		} else {
			v.ProviderName, v.KindLabel = "（登录方式已删除）", ""
		}
		pg.Items = append(pg.Items, v)
	}
	if set.AllowSelfBind {
		for _, p := range s.usable() {
			if p.Kind.IsRedirect() && !bound[p.ID] {
				pg.Available = append(pg.Available, BindChoice{ProviderID: p.ID, Name: p.Name, KindLabel: p.Kind.Label(),
					Href: "/login/sso/" + strconv.FormatUint(uint64(p.ID), 10) + "?bind=1"})
			}
		}
	}
	return pg, nil
}

var (
	ErrBindOff      = &FlowError{Code: "bind_off", Msg: "管理员未允许用户自行绑定 / 解绑外部账号"}
	ErrBindNotFound = &FlowError{Code: "bind_nf", Msg: "绑定记录不存在"}
)

// Unbind 解除用户自己的一个外部账号绑定（只能解自己的）。
func (s *Service) Unbind(actor audit.Actor, id uint) error {
	if !s.Settings().AllowSelfBind {
		return ErrBindOff
	}
	ids, err := s.repo.IdentitiesByUser(actor.ID)
	if err != nil {
		return err
	}
	var name string
	for _, b := range ids {
		if b.ID == id {
			if p, _ := s.provider(b.ProviderID); p != nil {
				name = p.Name
			}
		}
	}
	ok, err := s.repo.DeleteIdentityByID(id, actor.ID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrBindNotFound
	}
	s.auditActor(actor, "解绑外部账号", name) // i18n-ignore 审计文本（存库）
	return nil
}

// completeBind 在已登录状态下完成 SSO 回调时绑定外部身份：session 用户必须是发起绑定的用户；外部账号不能已绑定给别人；
// 同一登录方式下每个用户只绑一个外部账号（换绑请先解绑）；超级用户按“超级用户只能本地登录”的设置不允许绑定。
func (s *Service) completeBind(p *Provider, id *Identity, st *ssoState, sessUID uint, set Settings) error {
	if !set.AllowSelfBind {
		return ErrBindOff
	}
	if sessUID == 0 || sessUID != st.BindUID {
		return ErrStateInvalid
	}
	u, err := s.repo.UserByID(sessUID)
	if err != nil {
		return err
	}
	if u == nil || !u.IsActive {
		return ErrDisabledUser
	}
	if u.IsSuperuser && set.SuperuserLocalOnly {
		return ErrSuperuserLocal
	}
	if id.Subject == "" {
		return &FlowError{Code: "failed", Msg: ErrSSOFailed.Msg, Detail: "身份提供方没有返回唯一标识"} // i18n-ignore Detail 只进日志与审计
	}
	owner, err := s.repo.IdentityUser(p.ID, id.Subject)
	if err != nil {
		return err
	}
	if owner != 0 && owner != sessUID {
		return &FlowError{Code: "bind_taken", Msg: "该外部账号已绑定到其他平台账号"}
	}
	if owner == sessUID {
		return nil // 已经绑定过：当作成功
	}
	mine, err := s.repo.IdentitiesByUser(sessUID)
	if err != nil {
		return err
	}
	for _, b := range mine {
		if b.ProviderID == p.ID {
			return &FlowError{Code: "bind_dup", Msg: "你已经绑定过这个登录方式的账号，请先解绑再绑定新的"}
		}
	}
	if err := s.repo.BindIdentity(p.ID, id.Subject, sessUID); err != nil {
		return err
	}
	s.auditUser(u, "绑定外部账号", fmt.Sprintf("%s（%s）", p.Name, p.Kind.Label())) // i18n-ignore 审计文本（存库）
	return nil
}

// BindError 标记“绑定外部账号”流程里的失败（回调处理出错时页面回到账号绑定页，而不是登录页）。
type BindError struct{ Err error }

func (e *BindError) Error() string { return e.Err.Error() }
func (e *BindError) Unwrap() error { return e.Err }

// bindErrCode 把绑定失败转成 /account/bindings 页面上 ?err= 的固定代码。
func bindErrCode(err error) string {
	var fe *FlowError
	if errors.As(err, &fe) {
		switch fe.Code {
		case "bind_off", "bind_taken", "bind_dup", "superuser_local", "state", "disabled":
			return fe.Code
		}
	}
	return "failed"
}

// ---- 水印里的用户变量 ----

// WatermarkFor 按当前设置与用户信息生成页面水印配置（供 /api/v1/auth/ui）。ip 是本次请求的客户端地址。
func (s *Service) WatermarkFor(uid uint, username, display, ip string) UserWatermark {
	dept := ""
	if pr, ok := s.repo.(ProfileRepo); ok {
		dept = pr.UserDept(uid)
	}
	if display == "" {
		display = username
	}
	return UserWatermarkFor(s.Settings(), map[string]string{"账号": username, "姓名": display, "部门": dept, "IP": ip}) // i18n-ignore 水印变量名（存库的模板里用）
}

// CASLogoutURL 返回用户用 CAS 登录并且登录方式开启了“退出时同时全局登出”时，应该跳转的 CAS 登出地址；否则返回空。
// provID 是建立会话时记下的登录方式 ID，base 是平台对外地址（登出后回到登录页）。
func (s *Service) CASLogoutURL(provID uint, base string) string {
	if provID == 0 {
		return ""
	}
	p, _ := s.provider(provID)
	if p == nil || p.Kind != KindCAS || !p.Config.GlobalLogout {
		return ""
	}
	cb, err := casBase(p.Config)
	if err != nil {
		return ""
	}
	if base == "" {
		base = s.opt.PublicURL
	}
	return authURL(cb+"logout", url.Values{"service": {strings.TrimRight(base, "/") + "/login"}})
}
