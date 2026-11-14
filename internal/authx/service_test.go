// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package authx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/crypto"
)

func TestProviderViewNeverEchoesSecrets(t *testing.T) {
	e := newTestEnv(t)
	p, _ := e.svc.CreateProvider(rootActor, KindOIDC, "sso")
	cfg := p.Config
	cfg.WellKnownURL, cfg.ClientID, cfg.ClientSecret = "https://sso.example.com/.well-known/openid-configuration", "cid", "TOP-SECRET-VALUE"
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: "sso", FirstLogin: FirstLoginExisting, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	cur, _ := e.repo.GetProvider(p.ID)
	v := e.svc.View(cur, "http://dba.test")
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "TOP-SECRET-VALUE") {
		t.Fatalf("接口不能回显密钥: %s", raw)
	}
	if !v.HasSecrets["client_secret"] || v.Config.ClientSecret != "" {
		t.Errorf("应只显示已配置: %+v", v.HasSecrets)
	}
	if v.Callback != "http://dba.test/login/sso/"+itoa(p.ID)+"/callback" {
		t.Errorf("回调地址: %s", v.Callback)
	}
	// 留空 = 不改
	cfg2 := cur.Config
	cfg2.ClientSecret, cfg2.ClientID = "", "cid2"
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: "sso", FirstLogin: FirstLoginExisting, Config: cfg2}); err != nil {
		t.Fatal(err)
	}
	cur, _ = e.repo.GetProvider(p.ID)
	if cur.Config.ClientSecret != "TOP-SECRET-VALUE" || cur.Config.ClientID != "cid2" {
		t.Errorf("留空应保持原密钥: %+v", cur.Config)
	}
	// 新值覆盖
	cfg2.ClientSecret = "NEW-SECRET"
	e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: "sso", FirstLogin: FirstLoginExisting, Config: cfg2})
	if cur, _ = e.repo.GetProvider(p.ID); cur.Config.ClientSecret != "NEW-SECRET" {
		t.Error("新密钥应覆盖")
	}
	// 清除（必填密钥清掉后校验不过）
	cfg2.ClientSecret = ""
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: "sso", FirstLogin: FirstLoginExisting, Config: cfg2, ClearSecrets: []string{"client_secret"}}); err == nil {
		t.Error("清掉必填密钥应报错")
	}
	// 审计里没有密钥
	for _, a := range e.repo.audits {
		if strings.Contains(a.ExtraInfo, "SECRET") || strings.Contains(a.Action, "SECRET") {
			t.Errorf("审计不得含密钥: %+v", a)
		}
	}
	// 列表与单个视图都不含
	list, _ := e.svc.ListProviders("http://dba.test")
	raw, _ = json.Marshal(list)
	if strings.Contains(string(raw), "NEW-SECRET") {
		t.Error("列表不能回显密钥")
	}
}

func TestStoreEncryptsSecretFields(t *testing.T) {
	m, err := crypto.NewMirage("0123456789abcdef0123456789abcdef", "ECB", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &store{m: m}
	in := Config{WellKnownURL: "https://x/.well-known/openid-configuration", ClientID: "cid", ClientSecret: "PLAIN-SECRET"}
	enc, err := s.encConfig(KindOIDC, in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, "PLAIN-SECRET") {
		t.Fatalf("库里不能出现密钥明文: %s", enc)
	}
	if in.ClientSecret != "PLAIN-SECRET" {
		t.Error("加密不能改动调用方的结构")
	}
	p := s.toProvider(providerRow{Kind: "oidc", Config: enc, AuthGroups: "1,3", ResGroups: ""})
	if p.Config.ClientSecret != "PLAIN-SECRET" || p.Config.ClientID != "cid" || len(p.AuthGroups) != 2 {
		t.Errorf("读出应解密: %+v", p)
	}
	// LDAP 的 bind_password 同理
	enc, _ = s.encConfig(KindLDAP, Config{ServerURI: "ldaps://x", BindPassword: "BIND-PW"})
	if strings.Contains(enc, "BIND-PW") {
		t.Error("bind_password 必须加密")
	}
	if s.toProvider(providerRow{Kind: "ldap", Config: enc}).Config.BindPassword != "BIND-PW" {
		t.Error("bind_password 解密")
	}
	// 过长的外部 ID 压成定长
	if long := strings.Repeat("a", 300); len(subj(long)) > 190 || subj("abc") != "abc" {
		t.Error("subj")
	}
}

func TestProviderLifecycle(t *testing.T) {
	e := newTestEnv(t)
	idp := newFakeOIDC(t, e.clk)
	a := e.addOIDC(idp, FirstLoginExisting)
	b := e.addLDAP(FirstLoginCreate)
	// 排序
	if err := e.svc.Reorder(rootActor, []uint{b.ID, a.ID}); err != nil {
		t.Fatal(err)
	}
	list, _ := e.repo.ListProviders()
	if list[0].ID != b.ID || list[1].ID != a.ID {
		t.Errorf("排序: %v", []uint{list[0].ID, list[1].ID})
	}
	if err := e.svc.Reorder(rootActor, []uint{a.ID, a.ID}); err == nil {
		t.Error("重复 ID 应拒绝")
	}
	if err := e.svc.Reorder(rootActor, []uint{999}); err == nil {
		t.Error("不存在的 ID 应拒绝")
	}
	// 必须先停用再删除
	if err := e.svc.DeleteProvider(rootActor, a.ID, true); err == nil {
		t.Error("启用中的方式不能删除")
	}
	e.svc.SetEnabled(rootActor, a.ID, false)
	e.repo.BindIdentity(a.ID, "s1", 2)
	e.repo.BindIdentity(a.ID, "s2", 3)
	err := e.svc.DeleteProvider(rootActor, a.ID, false)
	var iu *InUseError
	if !errors.As(err, &iu) || iu.Users != 2 {
		t.Fatalf("有绑定用户时要求确认: %v", err)
	}
	if err := e.svc.DeleteProvider(rootActor, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.repo.GetProvider(a.ID); p != nil {
		t.Error("应已删除")
	}
	if n, _ := e.repo.CountIdentities(a.ID); n != 0 {
		t.Error("绑定应一并清除")
	}
	// 本地登录始终保留：删光外部方式后登录页仍有表单
	e.svc.SetEnabled(rootActor, b.ID, false)
	e.svc.DeleteProvider(rootActor, b.ID, true)
	if pg := e.svc.LoginPageInfo(false); !pg.ShowForm || len(pg.Buttons) != 0 {
		t.Errorf("%+v", pg)
	}
	// 所有变更都写了审计
	acts := strings.Join(e.repo.auditActions(), ",")
	for _, want := range []string{"登录方式-新增", "登录方式-修改", "登录方式-启用", "登录方式-停用", "登录方式-删除", "登录方式-排序", "登录方式-测试"} {
		if !strings.Contains(acts, want) {
			t.Errorf("缺少审计 %s", want)
		}
	}
}

func TestProviderValidation(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.svc.CreateProvider(rootActor, "nope", "x"); err == nil {
		t.Error("未知类型")
	}
	if _, err := e.svc.CreateProvider(rootActor, KindOIDC, "  "); err == nil {
		t.Error("名称必填")
	}
	if _, err := e.svc.CreateProvider(rootActor, KindOIDC, strings.Repeat("长", 51)); err == nil {
		t.Error("名称过长")
	}
	p, _ := e.svc.CreateProvider(rootActor, KindLDAP, "d")
	base := p.Config
	base.ServerURI, base.SearchBase, base.SearchFilter = "ldaps://ldap.example.com", "ou=p,dc=e", "(uid=%(user)s)"
	ok := func(c Config) error {
		_, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: "d", FirstLogin: FirstLoginExisting, Config: c})
		return err
	}
	if err := ok(base); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(c *Config){
		"模板与过滤器二选一":         func(c *Config) { c.UserDNTemplate = "uid=%(user)s,ou=p" },
		"过滤器缺占位符":           func(c *Config) { c.SearchFilter = "(uid=admin)" },
		"过滤器括号不平衡":          func(c *Config) { c.SearchFilter = "(uid=%(user)s" },
		"ldaps 方式用了 ldap":   func(c *Config) { c.ServerURI = "ldap://x" },
		"starttls 用了 ldaps": func(c *Config) { c.ConnSecurity, c.ServerURI = "starttls", "ldaps://x" },
		"协议不对":              func(c *Config) { c.ServerURI = "http://x" },
		"超时过大":              func(c *Config) { c.TimeoutSec = 600 },
		"入组规则缺值":            func(c *Config) { c.GroupRules = []GroupRule{{Attr: "memberOf", Op: "eq"}} },
		"入组规则比较方式":          func(c *Config) { c.GroupRules = []GroupRule{{Attr: "a", Op: "regex", Value: "x"}} },
		"不存在的权限组": func(c *Config) {
			c.GroupRules = []GroupRule{{Attr: "a", Op: "eq", Value: "x", AuthGroupIDs: []uint{99}}}
		},
	}
	for name, mut := range bad {
		c := base
		mut(&c)
		if err := ok(c); err == nil {
			t.Errorf("%s 应拒绝", name)
		}
	}
	// DN 模板方式可以
	c := base
	c.SearchFilter, c.SearchBase, c.UserDNTemplate = "", "", "uid=%(user)s,ou=people,dc=example,dc=com"
	if err := ok(c); err != nil {
		t.Errorf("DN 模板方式: %v", err)
	}
	// 默认入组校验
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: "d", FirstLogin: FirstLoginCreate, Config: c, AuthGroups: []uint{99}}); err == nil {
		t.Error("不存在的默认权限组应拒绝")
	}
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: "d", FirstLogin: "weird", Config: c}); err == nil {
		t.Error("首次登录策略不合法")
	}
}

func TestLDAPTestLoginReport(t *testing.T) {
	e := newTestEnv(t)
	p := e.addLDAP(FirstLoginCreate)
	e.dir.users["zs"] = fakeDirUser{password: "pw-zs-123", dn: "cn=zs", attrs: map[string][]string{"displayname": {"张三"}, "mail": {"zs@example.com"}}}
	rep, err := e.svc.TestProvider(context.Background(), rootActor, p.ID, TestInput{Username: "zs", Password: "pw-zs-123"})
	if err != nil || !rep.OK {
		t.Fatalf("%v %+v", err, rep)
	}
	txt := strings.Join(rep.Lines, "\n")
	if !strings.Contains(txt, "username=zs") || !strings.Contains(txt, "display=张三") || !strings.Contains(txt, "email=zs@example.com") || !strings.Contains(txt, "自动创建") {
		t.Errorf("报告: %s", txt)
	}
	if strings.Contains(txt, "pw-zs-123") {
		t.Error("报告里不能有密码")
	}
	if u, _ := e.repo.UserByName("zs"); u != nil {
		t.Error("测试不能创建用户")
	}
	// 匹配到已有用户
	e.dir.users["bob"] = fakeDirUser{password: "pw-bob-123", dn: "cn=bob"}
	rep, _ = e.svc.TestProvider(context.Background(), rootActor, p.ID, TestInput{Username: "bob", Password: "pw-bob-123"})
	if !strings.Contains(strings.Join(rep.Lines, ""), "匹配到已有用户 bob（按用户名）") {
		t.Errorf("%v", rep.Lines)
	}
	// 密码错误：失败，且作废之前的通过状态
	rep, _ = e.svc.TestProvider(context.Background(), rootActor, p.ID, TestInput{Username: "zs", Password: "bad"})
	if rep.OK {
		t.Error("认证失败应测试失败")
	}
	cur, _ := e.repo.GetProvider(p.ID)
	if cur.TestOK == nil || *cur.TestOK || cur.TestedHash != "" {
		t.Errorf("%+v", cur)
	}
	if v := e.svc.LoginPageInfo(false); v.HasLDAP {
		t.Error("测试失败后 LDAP 不应再参与登录")
	}
	// 连接失败
	e.dir.pingErr = errors.New("dial tcp: i/o timeout")
	rep, _ = e.svc.TestProvider(context.Background(), rootActor, p.ID, TestInput{})
	if rep.OK || !strings.Contains(strings.Join(rep.Lines, ""), "连接失败") {
		t.Errorf("%+v", rep)
	}
}

func TestSettingsLifecycle(t *testing.T) {
	e := newTestEnv(t)
	set := e.svc.Settings()
	d := DefaultSettings()
	if set.LockCount != 5 || set.LockSeconds != 300 || set.PasswordMinLen != 9 || set.SessionMinutes != 300 || set.JWTHours != 4 || !set.SessionBrowserClose || !set.SuperuserLocalOnly {
		t.Fatalf("默认值应为预期值: %+v", set)
	}
	_ = d
	// dbt_sql_config 里已有的值（含首字母大写的 True）
	e.repo.cfg[cfgEnforce2FA], e.repo.cfg[cfgSignUp] = "True", "true"
	e.repo.cfg[cfgLockCount], e.repo.cfg[cfgLockSeconds] = "3", "120"
	e.repo.cfg[cfgDefAuthGroup], e.repo.cfg[cfgAPIWhitelist] = "Default,RD", "1,7,x"
	e.repo.cfg[cfgSMSProvider] = "tencent"
	e.svc.settings.Invalidate()
	set = e.svc.Settings()
	if !set.Enforce2FA || !set.SignUp || set.LockCount != 3 || set.LockSeconds != 120 || len(set.DefaultAuthGroups) != 2 || len(set.APIWhitelist) != 2 || set.SMS.Provider != SMSTencent {
		t.Fatalf("解析 dbt_sql_config: %+v", set)
	}
	if set.APIAllowed(2) || !set.APIAllowed(7) {
		t.Error("API 白名单")
	}
	// 通过页面保存
	in := set.View().SecurityInput
	in.AliyunKeySecret, in.TencentSecretKey = "ALI-SEC", "TX-SEC"
	in.SessionMinutes, in.JWTHours, in.PasswordMinLen, in.PasswordComplexity = 120, 2, 12, true
	in.SMSProvider = SMSAliyun
	saved, err := SaveSecurity(e.repo, set, in, "root")
	if err != nil {
		t.Fatal(err)
	}
	if saved.SMS.AliyunKeySecret != "ALI-SEC" || saved.SessionMinutes != 120 || saved.SessionTTL() != 2*time.Hour || saved.TokenTTL() != 2*time.Hour || !saved.Policy().Complexity {
		t.Errorf("保存后: %+v", saved)
	}
	// 密钥留空 = 保持
	in.AliyunKeySecret, in.TencentSecretKey = "", ""
	saved2, err := SaveSecurity(e.repo, saved, in, "root")
	if err != nil || saved2.SMS.AliyunKeySecret != "ALI-SEC" || saved2.SMS.TencentSecretKey != "TX-SEC" {
		t.Fatalf("留空应保持密钥: %v", err)
	}
	// 视图不含密钥
	raw, _ := json.Marshal(saved2.View())
	if strings.Contains(string(raw), "ALI-SEC") || strings.Contains(string(raw), "TX-SEC") || !strings.Contains(string(raw), `"aliyun_access_key_secret":true`) {
		t.Errorf("视图: %s", raw)
	}
	// dbt_sql_config 里的键 + 新增键写在各自的位置
	if e.repo.cfg[cfgLockCount] != "3" || e.repo.kv[settingSessMin] != "120" || e.repo.cfg[cfgAliKeySecret] != "ALI-SEC" {
		t.Errorf("存放位置: cfg=%v kv=%v", e.repo.cfg, e.repo.kv)
	}
	// 校验
	for name, mut := range map[string]func(*SecurityInput){
		"锁定次数":    func(i *SecurityInput) { i.LockCount = 0 },
		"锁定时间":    func(i *SecurityInput) { i.LockSeconds = 5 },
		"最小长度":    func(i *SecurityInput) { i.PasswordMinLen = 3 },
		"会话有效期":   func(i *SecurityInput) { i.SessionMinutes = 1 },
		"JWT 有效期": func(i *SecurityInput) { i.JWTHours = 0 },
		"短信服务商":   func(i *SecurityInput) { i.SMSProvider = "twilio" },
	} {
		bad := in
		mut(&bad)
		if _, err := SaveSecurity(e.repo, saved2, bad, "root"); err == nil {
			t.Errorf("%s 应拒绝", name)
		}
	}
	// 缓存失效：保存后立即读到新值
	e.svc.settings.Set(saved2)
	if e.svc.Settings().SessionMinutes != 120 {
		t.Error("缓存应更新")
	}
}

func TestSettingsCacheFallback(t *testing.T) {
	e := newTestEnv(t)
	c := newSettingsCache(&flakyRepo{memRepo: e.repo}, time.Second, e.clk.Now)
	if c.Get().LockCount != 5 {
		t.Fatal("初始")
	}
	e.repo.cfg[cfgLockCount] = "9"
	if c.Get().LockCount != 5 {
		t.Error("TTL 内用缓存")
	}
	e.clk.Add(2 * time.Second)
	if c.Get().LockCount != 9 {
		t.Error("过期后重新加载")
	}
	c.repo.(*flakyRepo).fail = true
	e.clk.Add(2 * time.Second)
	if c.Get().LockCount != 9 {
		t.Error("数据库故障时沿用上一份设置")
	}
	// 从没成功过时用默认值
	c2 := newSettingsCache(&flakyRepo{memRepo: e.repo, fail: true}, time.Second, e.clk.Now)
	if c2.Get().LockCount != 5 {
		t.Error("故障且无缓存时用默认值")
	}
}

type flakyRepo struct {
	*memRepo
	fail bool
}

func (f *flakyRepo) ConfigValues(items []string) (map[string]string, error) {
	if f.fail {
		return nil, errors.New("db down")
	}
	return f.memRepo.ConfigValues(items)
}

func TestRulesSave(t *testing.T) {
	e := newTestEnv(t)
	set, err := SaveRules(e.repo, RulesInput{LocalFormMode: FormHidden, MatchBy: []string{"third_id", "username", "username"}, SuperuserLocalOnly: false, External2FA: true, BackfillIDs: false}, "root")
	if err != nil {
		t.Fatal(err)
	}
	if set.LocalFormMode != FormHidden || len(set.MatchBy) != 2 || set.SuperuserLocalOnly || !set.External2FA || set.BackfillIDs {
		t.Errorf("%+v", set)
	}
	if _, err := SaveRules(e.repo, RulesInput{LocalFormMode: "x"}, "root"); err == nil {
		t.Error("显示方式不合法")
	}
	if _, err := SaveRules(e.repo, RulesInput{LocalFormMode: FormAlways, MatchBy: []string{"fax"}}, "root"); err == nil {
		t.Error("匹配依据不合法")
	}
	// 允许清空匹配依据：只认已绑定的外部身份
	set, err = SaveRules(e.repo, RulesInput{LocalFormMode: FormAlways}, "root")
	if err != nil || len(set.MatchBy) != 0 {
		t.Errorf("清空匹配依据: %v %+v", err, set.MatchBy)
	}
}

func TestPasswordPolicy(t *testing.T) {
	def := DefaultPasswordPolicy()
	cplx := PasswordPolicy{MinLen: 9, Complexity: true}
	cases := []struct {
		name string
		p    PasswordPolicy
		pw   string
		attr []string
		ok   bool
	}{
		{"合格", def, "correct-horse-9", nil, true},
		{"太短", def, "abc-1234", nil, false},
		{"刚好 9 位", def, "abcde-123", nil, true},
		{"纯数字", def, "123456789012", nil, false},
		{"常见密码", def, "Password123", nil, false},
		{"包含短用户名", def, "bob-secret-pw", []string{"bob"}, false},
		{"两个字符的用户名不参与比较", def, "ab-secret-pw", []string{"ab"}, true},
		{"包含长用户名", def, "zhangsan-2026", []string{"zhangsan"}, false},
		{"与用户名相同", def, "administrator", []string{"administrator"}, false},
		{"复杂度：缺特殊字符", cplx, "abcdefgh1234", nil, false},
		{"复杂度：缺数字", cplx, "abcdefgh-xyz", nil, false},
		{"复杂度：齐全", cplx, "abcdefgh-12", nil, true},
		{"最小长度可调", PasswordPolicy{MinLen: 12}, "abcde-123456", nil, true},
		{"最小长度可调（不够）", PasswordPolicy{MinLen: 12}, "abcde-12345", nil, false},
		{"中文按字符数", PasswordPolicy{MinLen: 6}, "密码密码密码密码", nil, true},
	}
	for _, c := range cases {
		err := ValidatePassword(c.p, c.pw, c.attr...)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v", c.name, err)
		}
	}
	if err := ValidatePassword(def, strings.Repeat("a1-", 200)); err == nil {
		t.Error("超长密码应拒绝")
	}
}

func TestInitUserAndDefaultGroups(t *testing.T) {
	e := newTestEnv(t)
	e.repo.cfg[cfgDefAuthGroup], e.repo.cfg[cfgDefResGroup] = "Default,RD", "研发组,不存在的组"
	e.svc.settings.Invalidate()
	if err := e.svc.InitUser(2); err != nil {
		t.Fatal(err)
	}
	if !e.repo.userAuth[2][1] || !e.repo.userAuth[2][2] || e.repo.userAuth[2][3] || !e.repo.userRes[2][1] {
		t.Errorf("%v %v", e.repo.userAuth[2], e.repo.userRes[2])
	}
	if err := e.svc.InitUser(2); err != nil {
		t.Error("幂等")
	}
}

func TestSignUp(t *testing.T) {
	e := newTestEnv(t)
	in := SignUpInput{Username: "newguy", Password: "correct-horse-9", Password2: "correct-horse-9", Display: "新人", Email: "n@example.com", IP: "1.1.1.1"}
	var fe *FlowError
	if _, err := e.svc.SignUp(in); !errors.As(err, &fe) || fe.Code != "signup_off" {
		t.Fatalf("未开放注册: %v", err)
	}
	e.repo.cfg[cfgSignUp] = "true"
	e.svc.settings.Invalidate()
	bad := map[string]func(*SignUpInput){
		"两次密码不一致": func(i *SignUpInput) { i.Password2 = "x" },
		"缺中文名":    func(i *SignUpInput) { i.Display = " " },
		"用户名为空":   func(i *SignUpInput) { i.Username = "" },
		"用户名已存在":  func(i *SignUpInput) { i.Username = "bob" },
		"非法字符":    func(i *SignUpInput) { i.Username = "a b<script>" },
		"弱密码":     func(i *SignUpInput) { i.Password, i.Password2 = "123456789", "123456789" },
		"中文名过长":   func(i *SignUpInput) { i.Display = "一二三四五六七八九十一" },
	}
	for name, mut := range bad {
		c := in
		mut(&c)
		if _, err := e.svc.SignUp(c); !errors.As(err, &fe) {
			t.Errorf("%s 应拒绝: %v", name, err)
		}
	}
	name, err := e.svc.SignUp(in)
	if err != nil || name != "newguy" {
		t.Fatal(err)
	}
	u, _ := e.repo.UserByName("newguy")
	if u == nil || !u.IsActive || !u.IsStaff || u.IsSuperuser || !strings.HasPrefix(u.Password, "pbkdf2_sha256$") || !crypto.CheckDjangoPassword("correct-horse-9", u.Password) {
		t.Errorf("注册用户: %+v", u)
	}
	// 注册后即可用密码登录
	if _, err := e.login("newguy", "correct-horse-9"); err != nil {
		t.Errorf("注册后应能登录: %v", err)
	}
}
