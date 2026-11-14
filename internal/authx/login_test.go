// @author: kevinkelly
// @license: Apache Licence
// @file: login_test.go
// @time: 2026/10/28

package authx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/audit"
)

var rootActor = audit.Actor{ID: 1, Username: "root", Display: "管理员", IsSuperuser: true}

const pw = "pw-123456789"

// addLDAP 添加一个已测试通过并启用的 LDAP 登录方式。
func (e *testEnv) addLDAP(first string) *Provider {
	e.t.Helper()
	p, err := e.svc.CreateProvider(rootActor, KindLDAP, "公司域账号")
	if err != nil {
		e.t.Fatal(err)
	}
	cfg := p.Config
	cfg.ServerURI, cfg.ConnSecurity, cfg.SearchBase, cfg.SearchFilter = "ldaps://ldap.example.com:636", "ldaps", "ou=people,dc=example,dc=com", "(sAMAccountName=%(user)s)"
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: p.Name, FirstLogin: first, Config: cfg}); err != nil {
		e.t.Fatal(err)
	}
	return e.enable(p.ID)
}

// enable 走正式流程：测试通过后启用。
func (e *testEnv) enable(id uint) *Provider {
	e.t.Helper()
	rep, err := e.svc.TestProvider(context.Background(), rootActor, id, TestInput{})
	if err != nil || !rep.OK {
		e.t.Fatalf("测试应通过: %v %+v", err, rep)
	}
	if err := e.svc.SetEnabled(rootActor, id, true); err != nil {
		e.t.Fatal(err)
	}
	p, _ := e.repo.GetProvider(id)
	return p
}

// forceUsable 直接把登录方式置为“已启用且测试通过”（用于 OIDC 等需要假 IdP 的场景，配置在调用前已设好）。
func (e *testEnv) forceUsable(p *Provider) *Provider {
	e.t.Helper()
	cp, _ := e.repo.GetProvider(p.ID)
	ok := true
	now := e.clk.Now()
	cp.Enabled, cp.TestOK, cp.TestTime, cp.TestedHash = true, &ok, &now, e.svc.fp(cp)
	if err := e.repo.SaveProvider(cp); err != nil {
		e.t.Fatal(err)
	}
	e.svc.invalidateProviders()
	return cp
}

func (e *testEnv) login(user, password string) (*AuthOK, error) {
	return e.svc.PasswordLogin(context.Background(), user, password, PasswordOpts{IP: "10.0.0.1"})
}

func TestLocalLogin(t *testing.T) {
	e := newTestEnv(t)
	a, err := e.login("bob", pw)
	if err != nil || a.User.ID != 2 || a.External {
		t.Fatalf("本地登录应成功: %v %+v", err, a)
	}
	for name, c := range map[string][2]string{"错误密码": {"bob", "nope-nope-nope"}, "不存在的用户": {"ghost", pw}, "停用用户": {"off", pw}, "空密码": {"bob", ""}, "空用户名": {"", pw}} {
		if _, err := e.login(c[0], c[1]); !errors.Is(err, ErrBadCredentials) {
			t.Errorf("%s 应返回统一的凭据错误: %v", name, err)
		}
	}
	if _, err := e.login("bob\x00", pw); !errors.Is(err, ErrBadCredentials) {
		t.Error("含控制字符的用户名应拒绝")
	}
	// 本地没有可用密码的用户（LDAP 创建的用户）不能用空 / 任意密码登录
	e.repo.addUser(9, "ldaponly", "", false)
	if _, err := e.login("ldaponly", "!unusable"); !errors.Is(err, ErrBadCredentials) {
		t.Error("不可用密码的账号不能本地登录")
	}
}

func TestAccountLockMatchesLegacy(t *testing.T) {
	e := newTestEnv(t)
	// 默认 5 次 / 300 秒
	for i := 0; i < 5; i++ {
		if _, err := e.login("bob", "bad-bad-bad-bad"); !errors.Is(err, ErrBadCredentials) {
			t.Fatal(err)
		}
	}
	u, _ := e.repo.UserByID(2)
	if u.FailedLoginCount != 5 || u.LastLoginFailedAt == nil {
		t.Fatalf("应写入 failed_login_count / last_login_failed_at: %+v", u)
	}
	// 锁定期间密码正确也拒绝，且错误与普通失败完全一样（不泄漏用户是否存在）
	_, err := e.login("bob", pw)
	if !errors.Is(err, ErrBadCredentials) || err.Error() != ErrBadCredentials.Error() {
		t.Fatalf("锁定期间应拒绝: %v", err)
	}
	if u, _ := e.repo.UserByID(2); u.FailedLoginCount != 5 {
		t.Error("锁定期间不再累加计数")
	}
	e.clk.Add(299 * time.Second)
	if _, err := e.login("bob", pw); err == nil {
		t.Error("299 秒仍应锁定")
	}
	e.clk.Add(2 * time.Second)
	a, err := e.login("bob", pw)
	if err != nil || a.User.ID != 2 {
		t.Fatalf("锁定时间过后应能登录: %v", err)
	}
	if err := e.svc.CompleteLogin(2, 0, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	if u, _ := e.repo.UserByID(2); u.FailedLoginCount != 0 || u.LastLogin == nil {
		t.Errorf("登录成功应清零失败计数并记录 last_login: %+v", u)
	}
	acts := strings.Join(e.repo.auditActions(), ",")
	if !strings.Contains(acts, "账号锁定") || !strings.Contains(acts, "登入失败") || !strings.Contains(acts, "登入") {
		t.Errorf("审计动作缺失: %s", acts)
	}
}

func TestLockUsesConfiguredThresholds(t *testing.T) {
	e := newTestEnv(t)
	e.repo.cfg[cfgLockCount], e.repo.cfg[cfgLockSeconds] = "2", "60"
	e.svc.settings.Invalidate()
	e.login("bob", "x-x-x-x-x-x")
	e.login("bob", "x-x-x-x-x-x")
	if _, err := e.login("bob", pw); err == nil {
		t.Fatal("2 次失败后应被锁定")
	}
	e.clk.Add(61 * time.Second)
	if _, err := e.login("bob", pw); err != nil {
		t.Fatalf("60 秒后应解锁: %v", err)
	}
	// 库里已有的计数与时间同样生效
	e.repo.users[2].FailedLoginCount = 9
	now := e.clk.Now()
	e.repo.users[2].LastLoginFailedAt = &now
	if _, err := e.login("bob", pw); err == nil {
		t.Error("已有的失败计数应被识别为锁定")
	}
}

func TestLDAPLoginCreatesUser(t *testing.T) {
	e := newTestEnv(t)
	e.repo.cfg[cfgDefAuthGroup], e.repo.cfg[cfgDefResGroup] = "Default", "研发组"
	e.svc.settings.Invalidate()
	e.dir.users["zhang.san"] = fakeDirUser{password: "ldap-pass-1", dn: "cn=zhang.san,ou=people,dc=example,dc=com",
		attrs: map[string][]string{"displayname": {"张三三三三三三三三三三三三"}, "mail": {"zhang.san@example.com"}, "memberof": {"cn=dba-team,ou=groups"}}}
	p := e.addLDAP(FirstLoginCreate)

	a, err := e.login("zhang.san", "ldap-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Created || !a.External || a.ProviderID != p.ID {
		t.Fatalf("应自动创建: %+v", a)
	}
	u := a.User
	if u.Username != "zhang.san" || u.Email != "zhang.san@example.com" || !u.IsActive {
		t.Errorf("用户资料: %+v", u)
	}
	if n := len([]rune(u.Display)); n != 10 {
		t.Errorf("display 应截断到 10 个字符（dbt_sql_users.display 的长度限制）: %q", u.Display)
	}
	if !strings.HasPrefix(u.Password, "!") {
		t.Error("自动创建的用户应是不可用密码")
	}
	if !e.repo.userAuth[u.ID][1] || !e.repo.userRes[u.ID][1] {
		t.Errorf("应加入全局默认权限组与资源组: %v %v", e.repo.userAuth[u.ID], e.repo.userRes[u.ID])
	}
	if !strings.Contains(strings.Join(e.repo.auditActions(), ","), "自动创建用户") {
		t.Error("自动创建应写审计")
	}
	// 本地密码登不进（不可用密码），LDAP 密码错误也登不进
	if _, err := e.login("zhang.san", "wrong-pass-1"); !errors.Is(err, ErrBadCredentials) {
		t.Error("LDAP 密码错误应失败")
	}
	// 再次登录：匹配到同一用户，不再创建
	a2, err := e.login("zhang.san", "ldap-pass-1")
	if err != nil || a2.Created || a2.User.ID != u.ID {
		t.Fatalf("第二次登录应复用用户: %v %+v", err, a2)
	}
	if n, _ := e.repo.CountIdentities(p.ID); n != 1 {
		t.Errorf("应记录外部身份绑定: %d", n)
	}
}

func TestLDAPLocalFirstThenDirectory(t *testing.T) {
	e := newTestEnv(t)
	e.addLDAP(FirstLoginExisting)
	e.dir.users["bob"] = fakeDirUser{password: "dir-pass-bob", dn: "cn=bob", attrs: map[string][]string{"mail": {"bob@example.com"}}}
	// 本地密码优先，且不会去问目录
	if _, err := e.login("bob", pw); err != nil || e.dir.calls != 0 {
		t.Fatalf("本地密码匹配时不应访问目录: %v calls=%d", err, e.dir.calls)
	}
	// 本地密码不匹配时尝试目录，目录通过则以本地同名用户登录
	a, err := e.login("bob", "dir-pass-bob")
	if err != nil || a.User.ID != 2 || !a.External {
		t.Fatalf("目录认证应映射到本地同名用户: %v %+v", err, a)
	}
	// 仅已有用户：目录里有、平台没有 → 拒绝，且统一报凭据错误
	e.dir.users["newbie"] = fakeDirUser{password: "dir-pass-new", dn: "cn=newbie"}
	if _, err := e.login("newbie", "dir-pass-new"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("仅已有用户时应拒绝新用户: %v", err)
	}
	if u, _ := e.repo.UserByName("newbie"); u != nil {
		t.Error("不应创建用户")
	}
}

func TestLDAPRules(t *testing.T) {
	e := newTestEnv(t)
	e.addLDAP(FirstLoginCreate)
	e.dir.users["root"] = fakeDirUser{password: "dir-pass-root", dn: "cn=root"}
	e.dir.users["off"] = fakeDirUser{password: "dir-pass-off", dn: "cn=off"}
	e.dir.users["pwless"] = fakeDirUser{password: "", dn: "cn=pwless"}
	// 超级用户只能本地登录：目录密码即使正确也不行，也不会访问目录
	calls := e.dir.calls
	if _, err := e.login("root", "dir-pass-root"); !errors.Is(err, ErrBadCredentials) || e.dir.calls != calls {
		t.Errorf("超级用户不走目录: %v", err)
	}
	// 停用用户不走目录
	if _, err := e.login("off", "dir-pass-off"); !errors.Is(err, ErrBadCredentials) {
		t.Error("停用用户不能经目录登录")
	}
	// 空密码永远拒绝（LDAP 空密码绑定会被当成匿名绑定而“成功”）
	if _, err := e.login("pwless", ""); !errors.Is(err, ErrBadCredentials) {
		t.Error("空密码必须拒绝")
	}
	// 目录不可用：不影响本地登录，目录账号登录失败
	e.dir.down = true
	if _, err := e.login("bob", pw); err != nil {
		t.Errorf("目录故障时本地登录应不受影响: %v", err)
	}
	if _, err := e.login("pwless", "x"); !errors.Is(err, ErrBadCredentials) {
		t.Error("目录故障时应登录失败")
	}
	// 关闭“超级用户仅本地”后允许
	e.repo.kv[settingSuLocal] = "false"
	e.svc.settings.Invalidate()
	e.dir.down = false
	if a, err := e.login("root", "dir-pass-root"); err != nil || !a.External {
		t.Errorf("关闭后超级用户可经目录登录: %v", err)
	}
}

func TestLDAPUsernameAttrAndGroupRules(t *testing.T) {
	e := newTestEnv(t)
	p := e.addLDAP(FirstLoginCreate)
	cp, _ := e.repo.GetProvider(p.ID)
	cfg := cp.Config
	cfg.AttrUsername = "samaccountname"
	cfg.GroupRules = []GroupRule{{Attr: "memberOf", Op: "contains", Value: "dba-team", AuthGroupIDs: []uint{3}, ResourceGroups: []uint{2}},
		{Attr: "department", Op: "eq", Value: "销售", AuthGroupIDs: []uint{2}}}
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: cp.Name, FirstLogin: FirstLoginCreate, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	cur, _ := e.repo.GetProvider(p.ID)
	if !cur.Enabled {
		t.Fatal("只改映射和入组规则不应让登录方式停用")
	}
	e.dir.users["Li.Si"] = fakeDirUser{password: "dir-pass-ls", dn: "cn=li", attrs: map[string][]string{"samaccountname": {"lisi"}, "memberof": {"cn=dba-team,ou=g"}, "department": {"研发"}}}
	a, err := e.login("Li.Si", "dir-pass-ls")
	if err != nil {
		t.Fatal(err)
	}
	if a.User.Username != "lisi" {
		t.Errorf("应使用映射的用户名属性: %q", a.User.Username)
	}
	if !e.repo.userAuth[a.User.ID][3] || e.repo.userAuth[a.User.ID][2] || !e.repo.userRes[a.User.ID][2] {
		t.Errorf("入组规则只应命中 dba-team: %v %v", e.repo.userAuth[a.User.ID], e.repo.userRes[a.User.ID])
	}
}

func TestFirstLoginInitGroupsForExistingLocalUser(t *testing.T) {
	e := newTestEnv(t)
	e.repo.cfg[cfgDefAuthGroup] = "RD"
	e.svc.settings.Invalidate()
	// 从没登录过的本地用户首次登录入默认组
	if err := e.svc.CompleteLogin(2, 0, ""); err != nil {
		t.Fatal(err)
	}
	if !e.repo.userAuth[2][2] {
		t.Error("首次登录应入默认权限组")
	}
	delete(e.repo.userAuth, 2)
	if err := e.svc.CompleteLogin(2, 0, ""); err != nil {
		t.Fatal(err)
	}
	if len(e.repo.userAuth[2]) != 0 {
		t.Error("非首次登录不应再入组")
	}
}

func TestHiddenLocalFormAndDoor(t *testing.T) {
	e := newTestEnv(t)
	p, _ := e.svc.CreateProvider(rootActor, KindOIDC, "sso")
	cfg := p.Config
	cfg.WellKnownURL, cfg.ClientID, cfg.ClientSecret = "https://sso.example.com/.well-known/openid-configuration", "cid", "sec"
	e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: "sso", FirstLogin: FirstLoginExisting, Config: cfg, BtnLabel: "公司账号"})
	e.forceUsable(p)
	pg := e.svc.LoginPageInfo(false)
	if len(pg.Buttons) != 1 || pg.Buttons[0].Label != "公司账号" || !pg.ShowForm || pg.FormExpanded {
		t.Fatalf("默认折叠: %+v", pg)
	}
	e.repo.kv[settingLocalForm] = FormHidden
	e.svc.settings.Invalidate()
	if pg := e.svc.LoginPageInfo(false); pg.ShowForm {
		t.Error("隐藏模式不应显示表单")
	}
	if pg := e.svc.LoginPageInfo(true); !pg.ShowForm || !pg.FormExpanded {
		t.Error("?local=1 永远显示表单")
	}
	if _, err := e.svc.PasswordLogin(context.Background(), "root", pw, PasswordOpts{}); !errors.Is(err, ErrBadCredentials) {
		t.Error("隐藏模式下直接 POST 不应受理")
	}
	if _, err := e.svc.PasswordLogin(context.Background(), "bob", pw, PasswordOpts{Local: true}); !errors.Is(err, ErrBadCredentials) {
		t.Error("后门只给超级用户")
	}
	if a, err := e.svc.PasswordLogin(context.Background(), "root", pw, PasswordOpts{Local: true}); err != nil || a.User.ID != 1 {
		t.Errorf("超级用户后门应可用: %v", err)
	}
	// 没有任何可用的外部方式时，无论怎么设置表单都在（不能把自己锁在外面）
	e.svc.SetEnabled(rootActor, p.ID, false)
	if pg := e.svc.LoginPageInfo(false); !pg.ShowForm || len(pg.Buttons) != 0 {
		t.Errorf("没有外部方式时表单必须显示: %+v", pg)
	}
}

func TestLDAPFilterEscaping(t *testing.T) {
	got := userFilter("(&(objectClass=person)(sAMAccountName=%(user)s))", "x)(uid=*))(|(uid=*")
	if strings.Contains(got, ")(uid=*") || !strings.Contains(got, `\29\28uid=\2a`) {
		t.Errorf("过滤器必须转义: %s", got)
	}
	dn := userDN("uid=%(user)s,ou=people,dc=example,dc=com", "a,ou=admins+cn=x")
	if dn != `uid=a\,ou=admins\+cn=x,ou=people,dc=example,dc=com` {
		t.Errorf("DN 必须转义: %s", dn)
	}
}

func TestExternalSuperuserMatchRefused(t *testing.T) {
	e := newTestEnv(t)
	p := e.addLDAP(FirstLoginCreate)
	// 绑定到超级用户的外部身份也不能登录（superuser_local_only）
	if err := e.repo.BindIdentity(p.ID, "someone", 1); err != nil {
		t.Fatal(err)
	}
	id := &Identity{ProviderID: p.ID, Kind: KindLDAP, Subject: "someone", Username: "someone"}
	if _, err := e.svc.completeExternal(context.Background(), p, id, e.svc.Settings(), false); !errors.Is(err, ErrSuperuserLocal) {
		t.Errorf("应拒绝: %v", err)
	}
}

func TestLDAPUpdateOnLoginAndSyncGroups(t *testing.T) {
	e := newTestEnv(t)
	p := e.addLDAP(FirstLoginExisting)
	cp, _ := e.repo.GetProvider(p.ID)
	cfg := cp.Config
	cfg.UpdateOnLogin, cfg.SyncGroups = true, true
	cfg.GroupRules = []GroupRule{{Attr: "memberOf", Op: "contains", Value: "dba", AuthGroupIDs: []uint{3}}}
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: cp.Name, FirstLogin: FirstLoginExisting, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	e.dir.users["bob"] = fakeDirUser{password: "dir-pass-bob", dn: "cn=bob", attrs: map[string][]string{"displayname": {"鲍勃新名"}, "mail": {"bob@new.example.com"}, "memberof": {"cn=dba-team"}}}
	if _, err := e.login("bob", "dir-pass-bob"); err != nil {
		t.Fatal(err)
	}
	u, _ := e.repo.UserByID(2)
	if u.Display != "鲍勃新名" || u.Email != "bob@new.example.com" {
		t.Errorf("每次登录应同步姓名 / 邮箱: %+v", u)
	}
	if !e.repo.userAuth[2][3] {
		t.Error("已有用户也应按入组规则同步入组")
	}
	// 关闭同步后不再改资料
	cfg.UpdateOnLogin, cfg.SyncGroups = false, false
	e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: cp.Name, FirstLogin: FirstLoginExisting, Config: cfg})
	e.dir.users["bob"] = fakeDirUser{password: "dir-pass-bob", dn: "cn=bob", attrs: map[string][]string{"displayname": {"又改了"}}}
	if _, err := e.login("bob", "dir-pass-bob"); err != nil {
		t.Fatal(err)
	}
	if u, _ := e.repo.UserByID(2); u.Display != "鲍勃新名" {
		t.Errorf("关闭后不应改资料: %q", u.Display)
	}
}
