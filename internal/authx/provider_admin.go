// @author: kevinkelly
// @license: Apache Licence
// @file: provider_admin.go
// @time: 2026/10/28

package authx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dbtool/internal/audit"
)

// 登录方式的管理：增删改、排序、启用 / 停用、测试。所有变更写 dbt_audit_log（不含密钥）。

// ProviderView 是返回给管理页的登录方式：Config 里的密钥已清空，只有“是否已配置”。
type ProviderView struct {
	ID         uint            `json:"id"`
	Kind       Kind            `json:"kind"`
	KindLabel  string          `json:"kind_label"`
	Name       string          `json:"name"`
	Enabled    bool            `json:"enabled"`
	Status     string          `json:"status"` // enabled / disabled / error
	Sort       int             `json:"sort"`
	BtnLabel   string          `json:"btn_label"`
	FirstLogin string          `json:"first_login"`
	AuthGroups []uint          `json:"auth_groups"`
	ResGroups  []uint          `json:"resource_groups"`
	Config     Config          `json:"config"`
	HasSecrets map[string]bool `json:"has_secrets"`
	TestOK     *bool           `json:"test_ok"`
	TestMsg    string          `json:"test_msg"`
	TestTime   string          `json:"test_time"`
	Stale      bool            `json:"test_stale"` // 测试通过之后配置又改过
	LastLogin  string          `json:"last_login"`
	Callback   string          `json:"callback_url,omitempty"`
	Users      int             `json:"users"` // 通过该方式绑定过的用户数
}

const timeFmt = "2006-01-02 15:04:05"

func fmtTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Format(timeFmt)
}

// View 把登录方式转成接口返回的结构（永不含密钥）。base 是对外地址，用来拼回调地址。
func (s *Service) View(p *Provider, base string) ProviderView {
	cfg := p.Config
	has := cfg.HasSecrets(p.Kind)
	for _, f := range secretFields(p.Kind) {
		*cfg.secretPtr(f) = ""
	}
	status := "disabled"
	switch {
	case p.Usable(s.fp):
		status = "enabled"
	case p.TestOK != nil && !*p.TestOK, p.Enabled:
		status = "error"
	}
	v := ProviderView{ID: p.ID, Kind: p.Kind, KindLabel: p.Kind.Label(), Name: p.Name, Enabled: p.Enabled, Status: status, Sort: p.Sort,
		BtnLabel: p.BtnLabel, FirstLogin: p.FirstLogin, AuthGroups: nonNilUints(p.AuthGroups), ResGroups: nonNilUints(p.ResGroups),
		Config: cfg, HasSecrets: has, TestOK: p.TestOK, TestMsg: p.TestMsg, TestTime: fmtTime(p.TestTime),
		Stale: p.TestOK != nil && *p.TestOK && p.TestedHash != s.fp(p), LastLogin: fmtTime(p.LastLogin)}
	if p.Kind.IsRedirect() {
		v.Callback = s.callbackURL(base, p.ID)
	}
	if p.Kind == KindFeishu && cfg.Site == "lark" {
		v.KindLabel = "Lark"
	}
	if n, err := s.repo.CountIdentities(p.ID); err == nil {
		v.Users = n
	}
	return v
}

func nonNilUints(in []uint) []uint {
	if in == nil {
		return []uint{}
	}
	return in
}

// ListProviders 返回全部登录方式（含停用的）。
func (s *Service) ListProviders(base string) ([]ProviderView, error) {
	list, err := s.repo.ListProviders()
	if err != nil {
		return nil, err
	}
	out := make([]ProviderView, 0, len(list))
	for i := range list {
		out = append(out, s.View(&list[i], base))
	}
	return out, nil
}

// LocalLastLogin 返回本地账号密码最近一次成功登录的时间。
func (s *Service) LocalLastLogin() string {
	kv, err := s.repo.AuthSettings()
	if err != nil {
		return ""
	}
	return kv[settingLocalLast]
}

// ProviderInput 是新增 / 修改登录方式的输入。Config 里的密钥留空表示不修改，ClearSecrets 列出要清空的密钥字段。
type ProviderInput struct {
	Name         string   `json:"name"`
	BtnLabel     string   `json:"btn_label"`
	FirstLogin   string   `json:"first_login"`
	AuthGroups   []uint   `json:"auth_groups"`
	ResGroups    []uint   `json:"resource_groups"`
	Config       Config   `json:"config"`
	ClearSecrets []string `json:"clear_secrets"`
}

// UserError 是校验类错误（参数不合法 / 状态不允许），信息可直接展示。
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErr(format string, a ...any) error { return &UserError{Msg: fmt.Sprintf(format, a...)} }

// defaultConfig 是新建某类型时的默认参数。
func defaultConfig(k Kind) Config {
	c := Config{Backfill: true}
	switch k {
	case KindLDAP:
		c.ConnSecurity, c.UpdateOnLogin, c.Backfill = "ldaps", true, false
	case KindOIDC:
		c.Scopes, c.SignAlg = "openid profile email", "RS256"
	case KindFeishu:
		c.Site = "feishu"
	case KindWeCom:
		c.WeComMode = "auto"
	case KindCAS:
		c.CASVersion, c.Backfill = "3", false
	case KindSlack:
	}
	return c
}

// CreateProvider 新增一个登录方式（停用状态，需要先配置并测试通过）。
func (s *Service) CreateProvider(actor audit.Actor, kind Kind, name string) (*Provider, error) {
	if !kind.Valid() {
		return nil, userErr("不支持的登录方式类型")
	}
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n == 0 || n > 50 {
		return nil, userErr("名称需要 1 到 50 个字符")
	}
	list, err := s.repo.ListProviders()
	if err != nil {
		return nil, err
	}
	sort := 10
	for _, p := range list {
		if p.Sort >= sort {
			sort = p.Sort + 10
		}
	}
	p := &Provider{Kind: kind, Name: name, Sort: sort, FirstLogin: FirstLoginExisting, Config: defaultConfig(kind), CreatedBy: actor.Username, CreateTime: s.now()}
	if err := s.repo.SaveProvider(p); err != nil {
		return nil, err
	}
	s.invalidateProviders()
	s.auditActor(actor, "登录方式-新增", fmt.Sprintf("%s（%s）", p.Name, kind.Label())) // i18n-ignore 审计文本（存库）
	return p, nil
}

// GetProvider 取一个登录方式（含明文密钥，仅服务内部使用）。不存在返回 nil。
func (s *Service) GetProvider(id uint) (*Provider, error) { return s.repo.GetProvider(id) }

// UpdateProvider 修改登录方式。配置里与连接相关的内容改动后，之前的测试结果作废并自动停用，需要重新测试。
func (s *Service) UpdateProvider(actor audit.Actor, id uint, in ProviderInput) (*Provider, string, error) {
	p, err := s.repo.GetProvider(id)
	if err != nil {
		return nil, "", err
	}
	if p == nil {
		return nil, "", userErr("登录方式不存在")
	}
	before := s.fp(p)
	old := *p // 修改前的快照（审计写变化字段）；下面对 p 的赋值都是替换整个字段，不会改到快照里的切片
	name := strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(name); n == 0 || n > 50 {
		return nil, "", userErr("名称需要 1 到 50 个字符")
	}
	if utf8.RuneCountInString(in.BtnLabel) > 50 {
		return nil, "", userErr("按钮文案不超过 50 个字符")
	}
	if in.FirstLogin != FirstLoginCreate && in.FirstLogin != FirstLoginExisting {
		return nil, "", userErr("首次登录策略不合法")
	}
	cfg := in.Config
	cleared := map[string]bool{}
	for _, f := range in.ClearSecrets {
		cleared[f] = true
	}
	for _, f := range secretFields(p.Kind) {
		switch {
		case cleared[f]:
			*cfg.secretPtr(f) = ""
		case *cfg.secretPtr(f) == "":
			*cfg.secretPtr(f) = *p.Config.secretPtr(f) // 留空 = 不改
		}
	}
	cfg = normalizeConfig(p.Kind, cfg)
	cand := *p
	cand.Config = cfg
	if err := validateProvider(&cand); err != nil {
		return nil, "", userErr("%s", err.Error())
	}
	if err := s.checkGroupIDs(in.AuthGroups, in.ResGroups, cfg.GroupRules); err != nil {
		return nil, "", err
	}
	p.Name, p.BtnLabel, p.FirstLogin = name, strings.TrimSpace(in.BtnLabel), in.FirstLogin
	p.AuthGroups, p.ResGroups, p.Config = uniqUint(in.AuthGroups), uniqUint(in.ResGroups), cfg
	notice := ""
	if s.fp(p) != before {
		if p.Enabled {
			notice = "连接配置已变更，该登录方式已自动停用，请重新测试通过后再启用"
		}
		p.Enabled, p.TestOK, p.TestTime, p.TestedHash, p.TestMsg = false, nil, nil, "", ""
	}
	if err := s.repo.SaveProvider(p); err != nil {
		return nil, "", err
	}
	s.invalidateProviders()
	s.auditActor(actor, "登录方式-修改", diffProvider(&old, p)) // i18n-ignore 审计文本（存库）
	return p, notice, nil
}

// auditValMax 是审计里单个字段值的最长字符数（入组规则等长内容截断）。
const auditValMax = 120

// auditVal 把字段值转成审计里的文字：空值写“（空）”，切片 / 结构体用 JSON，过长截断。
func auditVal(v reflect.Value) string {
	var s string
	switch v.Kind() {
	case reflect.String:
		s = v.String()
	case reflect.Slice, reflect.Array, reflect.Struct, reflect.Map:
		if v.Kind() == reflect.Slice && v.Len() == 0 {
			return "（空）" // i18n-ignore 审计文本（存库）
		}
		b, _ := json.Marshal(v.Interface())
		s = string(b)
	default:
		s = fmt.Sprint(v.Interface())
	}
	if s == "" {
		return "（空）" // i18n-ignore 审计文本（存库）
	}
	return truncRunes(s, auditValMax)
}

// diffProvider 生成“登录方式-修改”的审计文字：只列有变化的字段，格式“字段: 旧 → 新”（字段名同接口的 JSON 键）。
// 密钥类字段（bind_password / client_secret）只写“已修改”，不写旧值也不写新值。
func diffProvider(a, b *Provider) string {
	var out []string
	add := func(name string, x, y reflect.Value) {
		if !reflect.DeepEqual(x.Interface(), y.Interface()) {
			out = append(out, fmt.Sprintf("%s: %s → %s", name, auditVal(x), auditVal(y)))
		}
	}
	add("name", reflect.ValueOf(a.Name), reflect.ValueOf(b.Name))
	add("btn_label", reflect.ValueOf(a.BtnLabel), reflect.ValueOf(b.BtnLabel))
	add("first_login", reflect.ValueOf(a.FirstLogin), reflect.ValueOf(b.FirstLogin))
	add("auth_groups", reflect.ValueOf(nonNilUints(a.AuthGroups)), reflect.ValueOf(nonNilUints(b.AuthGroups)))
	add("resource_groups", reflect.ValueOf(nonNilUints(a.ResGroups)), reflect.ValueOf(nonNilUints(b.ResGroups)))
	secret := map[string]bool{}
	for _, k := range []Kind{KindLDAP, KindOIDC, KindDingTalk, KindCAS, KindFeishu, KindSlack, KindWeCom} {
		for _, f := range secretFields(k) {
			secret[f] = true
		}
	}
	va, vb := reflect.ValueOf(a.Config), reflect.ValueOf(b.Config)
	for i := 0; i < va.NumField(); i++ {
		name := strings.Split(va.Type().Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		if secret[name] {
			if va.Field(i).Interface() != vb.Field(i).Interface() {
				out = append(out, name+": 已修改") // i18n-ignore 审计文本（存库）
			}
			continue
		}
		add(name, va.Field(i), vb.Field(i))
	}
	if a.Enabled != b.Enabled {
		out = append(out, fmt.Sprintf("enabled: %v → %v（连接配置变更，已自动停用）", a.Enabled, b.Enabled)) // i18n-ignore 审计文本（存库）
	}
	head := fmt.Sprintf("%s（%s）", b.Name, b.Kind.Label())
	if len(out) == 0 {
		return head + "；无字段变化" // i18n-ignore 审计文本（存库）
	}
	return head + "；" + strings.Join(out, "；")
}

// normalizeConfig 清理空白、补默认值（不触碰密钥）。
func normalizeConfig(k Kind, c Config) Config {
	for _, f := range []*string{&c.ServerURI, &c.BindDN, &c.SearchBase, &c.SearchFilter, &c.UserDNTemplate, &c.WellKnownURL, &c.ClientID,
		&c.TeamID, &c.CorpID, &c.AgentID, &c.TenantKey, &c.CASURL, &c.AttrUsername, &c.AttrDisplay, &c.AttrEmail, &c.AttrPhone, &c.Scopes} {
		*f = strings.TrimSpace(*f)
	}
	if len(c.MatchBy) > 0 { // 去重并保持顺序；空 = 沿用全局
		seen := map[string]bool{}
		var by []string
		for _, m := range c.MatchBy {
			if !seen[m] {
				seen[m] = true
				by = append(by, m)
			}
		}
		c.MatchBy = by
	}
	switch k {
	case KindLDAP:
		if c.ConnSecurity == "" {
			c.ConnSecurity = "ldaps"
		}
	case KindOIDC:
		if c.SignAlg == "" {
			c.SignAlg = "RS256"
		}
		if c.Scopes == "" {
			c.Scopes = "openid profile email"
		}
	case KindFeishu:
		if c.Site == "" {
			c.Site = "feishu"
		}
	case KindWeCom:
		if c.WeComMode == "" {
			c.WeComMode = "auto"
		}
	case KindCAS:
		if c.CASVersion == "" {
			c.CASVersion = "3"
		}
	}
	return c
}

// validateProvider 离线校验必填项与格式（不联网）。
func validateProvider(p *Provider) error {
	c := &p.Config
	var err error
	switch p.Kind {
	case KindLDAP:
		err = validateLDAPConfig(c)
	case KindOIDC:
		err = oidcParamsOf(p).validate()
		if err == nil {
			_, err = checkURL(c.WellKnownURL, "Wellknown URL")
		}
	case KindSlack:
		err = requireFields("Client ID", c.ClientID, "Client Secret", c.ClientSecret)
	case KindDingTalk:
		err = newDingFlow(&flowEnv{Now: time.Now}).validate(p)
	case KindFeishu:
		err = (&feishuFlow{}).validate(p)
	case KindWeCom:
		err = (&wecomFlow{}).validate(p)
	case KindCAS:
		_, err = casBase(*c)
		if err == nil && c.CASVersion != "1" && c.CASVersion != "2" && c.CASVersion != "3" {
			err = errors.New("CAS 协议版本只能是 1 / 2 / 3")
		}
	default:
		err = errors.New("不支持的类型")
	}
	if err != nil {
		return err
	}
	if len(c.GroupRules) > 50 {
		return errors.New("入组规则最多 50 条")
	}
	for i, r := range c.GroupRules {
		if strings.TrimSpace(r.Attr) == "" || strings.TrimSpace(r.Value) == "" || len(r.Attr) > 100 || len(r.Value) > 200 {
			return fmt.Errorf("第 %d 条入组规则的属性与值都必须填写", i+1)
		}
		if r.Op != "eq" && r.Op != "contains" {
			return fmt.Errorf("第 %d 条入组规则的比较方式不合法", i+1)
		}
	}
	for _, spec := range []string{c.AttrUsername, c.AttrDisplay, c.AttrEmail, c.AttrPhone} {
		if len(spec) > 200 {
			return errors.New("属性映射过长")
		}
	}
	for _, m := range c.MatchBy {
		if !validMatchBy(m) {
			return fmt.Errorf("不支持的匹配依据 %s", strconv.Quote(m))
		}
	}
	return nil
}

func (s *Service) checkGroupIDs(auth, res []uint, rules []GroupRule) error {
	if len(auth) == 0 && len(res) == 0 && len(rules) == 0 {
		return nil
	}
	ag, err := s.repo.ListAuthGroups()
	if err != nil {
		return err
	}
	rg, err := s.repo.ListResourceGroups()
	if err != nil {
		return err
	}
	aSet, rSet := map[uint]bool{}, map[uint]bool{}
	for _, g := range ag {
		aSet[g.ID] = true
	}
	for _, g := range rg {
		rSet[g.ID] = true
	}
	check := func(ids []uint, set map[uint]bool, what string) error {
		for _, id := range ids {
			if !set[id] {
				return userErr("%s（ID %d）不存在", what, id)
			}
		}
		return nil
	}
	if err := errors.Join(check(auth, aSet, "权限组"), check(res, rSet, "资源组")); err != nil {
		return userErr("%s", err.Error())
	}
	for _, r := range rules {
		if err := errors.Join(check(r.AuthGroupIDs, aSet, "入组规则里的权限组"), check(r.ResourceGroups, rSet, "入组规则里的资源组")); err != nil {
			return userErr("%s", err.Error())
		}
	}
	return nil
}

// SetEnabled 启用 / 停用。启用前必须测试通过，且测试之后配置没有再改过。
func (s *Service) SetEnabled(actor audit.Actor, id uint, enable bool) error {
	p, err := s.repo.GetProvider(id)
	if err != nil {
		return err
	}
	if p == nil {
		return userErr("登录方式不存在")
	}
	if enable && !(p.TestOK != nil && *p.TestOK && p.TestedHash == s.fp(p)) {
		return userErr("启用前必须先测试通过（配置修改后需要重新测试）")
	}
	if err := s.repo.SetProviderEnabled(id, enable); err != nil {
		return err
	}
	s.invalidateProviders()
	act := "登录方式-停用" // i18n-ignore 审计文本（存库）
	if enable {
		act = "登录方式-启用" // i18n-ignore 审计文本（存库）
	}
	s.auditActor(actor, act, fmt.Sprintf("%s（%s）", p.Name, p.Kind.Label()))
	return nil
}

// InUseError 表示删除前需要确认：有用户通过该方式登录过。
type InUseError struct{ Users int }

func (e *InUseError) Error() string {
	return fmt.Sprintf("有 %d 个用户通过该登录方式登录过，删除后他们的外部身份绑定会清除（用户本身保留，之后需要重新匹配）", e.Users)
}

// DeleteProvider 删除登录方式。必须先停用；有绑定用户时要 confirm=true。本地账号密码是内置方式，不在此表里，因此始终保留。
func (s *Service) DeleteProvider(actor audit.Actor, id uint, confirm bool) error {
	p, err := s.repo.GetProvider(id)
	if err != nil {
		return err
	}
	if p == nil {
		return userErr("登录方式不存在")
	}
	if p.Enabled {
		return userErr("请先停用再删除")
	}
	n, err := s.repo.CountIdentities(id)
	if err != nil {
		return err
	}
	if n > 0 && !confirm {
		return &InUseError{Users: n}
	}
	if err := s.repo.DeleteProvider(id); err != nil {
		return err
	}
	s.invalidateProviders()
	s.auditActor(actor, "登录方式-删除", fmt.Sprintf("%s（%s）", p.Name, p.Kind.Label())) // i18n-ignore 审计文本（存库）
	return nil
}

// Reorder 按给定顺序重排（登录页按钮的显示顺序）。
func (s *Service) Reorder(actor audit.Actor, ids []uint) error {
	list, err := s.repo.ListProviders()
	if err != nil {
		return err
	}
	have := map[uint]bool{}
	for _, p := range list {
		have[p.ID] = true
	}
	seen := map[uint]bool{}
	for _, id := range ids {
		if !have[id] || seen[id] {
			return userErr("排序列表里有不存在或重复的登录方式")
		}
		seen[id] = true
	}
	if err := s.repo.SetProviderOrder(ids); err != nil {
		return err
	}
	s.invalidateProviders()
	s.auditActor(actor, "登录方式-排序", "") // i18n-ignore 审计文本（存库）
	return nil
}

// ---- 测试 ----

// TestReport 是测试结果（给管理员看，不含密码、令牌、授权码）。
type TestReport struct {
	OK    bool     `json:"ok"`
	Lines []string `json:"lines"`
}

func (r *TestReport) add(sym, format string, a ...any) {
	r.Lines = append(r.Lines, sym+" "+fmt.Sprintf(format, a...))
}

// TestInput 是测试登录的账号（只有 LDAP 需要；密码不保存、不写日志）。
type TestInput struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// recordTest 保存测试结果；通过时记录当前配置指纹。
func (s *Service) recordTest(p *Provider, r *TestReport) {
	hash := ""
	if r.OK {
		hash = s.fp(p)
	}
	msg := strings.Join(r.Lines, "\n")
	if utf8.RuneCountInString(msg) > 900 {
		msg = truncRunes(msg, 900)
	}
	if err := s.repo.SetProviderTest(p.ID, r.OK, msg, hash, s.now()); err != nil {
		return
	}
	s.invalidateProviders()
}

// TestProvider 做“测试连接 / 测试登录”。LDAP 给了账号则走完整的绑定 + 属性映射 + 用户匹配预览，否则只测连接；
// 需要浏览器跳转的方式在这里只做元数据 / 凭据探测，完整授权走 /login/sso/:id?test=1。
func (s *Service) TestProvider(ctx context.Context, actor audit.Actor, id uint, in TestInput) (*TestReport, error) {
	p, err := s.repo.GetProvider(id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, userErr("登录方式不存在")
	}
	rep := &TestReport{}
	if err := validateProvider(p); err != nil {
		rep.add("✖", "配置不完整：%s", err.Error())
		s.recordTest(p, rep)
		return rep, nil
	}
	set := s.Settings()
	t0 := s.now()
	if p.Kind == KindLDAP {
		msg, err := s.ldap.dir.Ping(ctx, &p.Config)
		if err != nil {
			rep.add("✖", "连接失败：%s", errText(err))
			s.recordTest(p, rep)
			return rep, nil
		}
		rep.add("✔", "%s（%d ms）", msg, s.now().Sub(t0).Milliseconds())
		rep.OK = true
		if in.Username != "" || in.Password != "" {
			ident, err := s.ldap.Login(ctx, p, strings.TrimSpace(in.Username), in.Password)
			if err != nil {
				rep.OK = false
				if errors.Is(err, ErrBadCredentials) {
					rep.add("✖", "认证失败：账号或密码不对，或搜索条件没有匹配到该用户")
				} else {
					rep.add("✖", "认证出错：%s", errText(err))
				}
			} else {
				s.describeIdentity(rep, p, ident, set)
			}
		} else {
			rep.add("ℹ", "只测试了连接；填写测试账号与密码可以验证用户搜索与属性映射")
		}
		s.recordTest(p, rep)
		s.auditActor(actor, "登录方式-测试", fmt.Sprintf("%s：%v", p.Name, rep.OK)) // i18n-ignore 审计文本（存库）
		return rep, nil
	}
	msg, err := s.flows[p.Kind].Check(ctx, p)
	if err != nil {
		rep.add("✖", "%s", errText(err))
	} else {
		rep.OK = true
		rep.add("✔", "%s（%d ms）", msg, s.now().Sub(t0).Milliseconds())
	}
	s.recordTest(p, rep)
	s.auditActor(actor, "登录方式-测试", fmt.Sprintf("%s：%v", p.Name, rep.OK)) // i18n-ignore 审计文本（存库）
	return rep, nil
}

// errText 把错误转成给管理员看的一句话（url.Error 已在产生处去掉 URL）。
func errText(err error) string {
	var fe *FlowError
	if errors.As(err, &fe) {
		return fe.Msg
	}
	return err.Error()
}

// describeIdentity 把认证结果与用户匹配预览写进报告。
func (s *Service) describeIdentity(rep *TestReport, p *Provider, id *Identity, set Settings) {
	rep.add("✔", "认证通过，取得属性：%s", describeIdentity(id))
	if id.Username == "" {
		rep.OK = false
		rep.add("✖", "取不到用户名，请检查属性映射（当前映射：%s）", orDefault(p.Config.AttrUsername, func() string { u, _, _ := defaultAttrSpec(p.Kind); return u }()))
		return
	}
	if len(p.Config.GroupRules) > 0 {
		a, r := evalGroupRules(p.Config.GroupRules, id.Attrs)
		rep.add("ℹ", "命中入组规则：权限组 %v、资源组 %v", nonNilUints(a), nonNilUints(r))
	}
	ok, err := s.completeExternal(context.Background(), p, id, set, true)
	switch {
	case err != nil:
		rep.add("⚠", "平台不会放行该账号：%s", errText(err))
	case ok.User == nil:
		rep.add("ℹ", "没有匹配到已有用户，首次登录会自动创建用户 %s", id.Username)
	default:
		_, via, _ := s.matchUser(p, id, set)
		rep.add("ℹ", "匹配到已有用户 %s（按%s）", ok.User.Username, matchLabel(via))
	}
}

// reportSSOTest 把测试登录回调的结果整理成报告并保存。
func (s *Service) reportSSOTest(ctx context.Context, p *Provider, id *Identity, ferr error, set Settings) *TestReport {
	rep := &TestReport{}
	if ferr != nil {
		rep.add("✖", "授权流程失败：%s", errText(ferr))
	} else {
		rep.OK = true
		rep.add("✔", "授权流程完成（回调、令牌与签名校验均通过）")
		s.describeIdentity(rep, p, id, set)
	}
	// 回调里的配置以数据库为准
	if cur, err := s.repo.GetProvider(p.ID); err == nil && cur != nil {
		s.recordTest(cur, rep)
	}
	return rep
}
