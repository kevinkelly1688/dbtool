// @author: kevinkelly
// @license: Apache Licence
// @file: audit_gap_test.go
// @time: 2026/10/28

package authx

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

func auditsNamed(he *httpEnv, action string) []model.AuditEntry {
	r := he.repo2()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.AuditEntry
	for _, a := range r.audits {
		if a.Action == action {
			out = append(out, a)
		}
	}
	return out
}

// S9-A3：前端发现水印被删除 / 隐藏 / 改样式后上报；服务端只认枚举，限流每用户每分钟 3 次，不返回敏感信息。
func TestWatermarkTamperReport(t *testing.T) {
	he := newHTTPEnv(t)
	bob := he.loginAs("bob")
	code, out := bob.json("POST", "/api/v1/auth/watermark-tamper", map[string]any{"kind": "removed"})
	if code != 200 {
		t.Fatalf("上报: %d %v", code, out)
	}
	if d, _ := out["data"].(map[string]any); len(d) != 1 || d["ok"] != true {
		t.Errorf("响应只应有 ok，不能带别的信息: %v", out)
	}
	as := auditsNamed(he, "水印篡改")
	if len(as) != 1 || as[0].UserName != "bob" || !strings.Contains(as[0].ExtraInfo, "removed") || !strings.Contains(as[0].ExtraInfo, "192.0.2.1") {
		t.Fatalf("审计应含用户、IP、kind: %+v", as)
	}
	for _, kind := range []string{"hidden", "style"} {
		if code, _ := bob.json("POST", "/api/v1/auth/watermark-tamper", map[string]any{"kind": kind}); code != 200 {
			t.Fatalf("%s: %d", kind, code)
		}
	}
	// 第 4 次：限流，不再写审计
	code, out = bob.json("POST", "/api/v1/auth/watermark-tamper", map[string]any{"kind": "removed"})
	if code != 429 {
		t.Fatalf("每分钟最多 3 次: %d %v", code, out)
	}
	if n := len(auditsNamed(he, "水印篡改")); n != 3 {
		t.Errorf("被限流的上报不应写审计: %d", n)
	}
	// 别的用户有自己的额度
	other := he.loginAs("root")
	if code, _ := other.json("POST", "/api/v1/auth/watermark-tamper", map[string]any{"kind": "style"}); code != 200 {
		t.Errorf("其他用户不受影响: %d", code)
	}
	// 窗口过去之后恢复
	he.clk.Add(61 * time.Second)
	if code, _ := bob.json("POST", "/api/v1/auth/watermark-tamper", map[string]any{"kind": "style"}); code != 200 {
		t.Errorf("1 分钟后应恢复: %d", code)
	}
}

func TestWatermarkTamperRejectsBadInput(t *testing.T) {
	he := newHTTPEnv(t)
	bob := he.loginAs("bob")
	for _, body := range []any{
		map[string]any{"kind": "deleted"},
		map[string]any{"kind": ""},
		map[string]any{"kind": "REMOVED"},
		map[string]any{"kind": 1},
		map[string]any{"kind": []string{"removed"}},
		map[string]any{"detail": "x"},
		map[string]any{"kind": "removed<script>"},
		nil,
	} {
		if code, _ := bob.json("POST", "/api/v1/auth/watermark-tamper", body); code != 400 {
			t.Errorf("%v 应 400，得到 %d", body, code)
		}
	}
	if n := len(auditsNamed(he, "水印篡改")); n != 0 {
		t.Errorf("非法输入不应写审计: %d", n)
	}
	// 非法输入不占限流额度
	for i := 0; i < 3; i++ {
		if code, _ := bob.json("POST", "/api/v1/auth/watermark-tamper", map[string]any{"kind": "hidden"}); code != 200 {
			t.Fatalf("第 %d 次: %d", i+1, code)
		}
	}
	// 其它字段不被信任：不会进审计
	he2 := newHTTPEnv(t)
	b2 := he2.loginAs("bob")
	if code, _ := b2.json("POST", "/api/v1/auth/watermark-tamper", map[string]any{"kind": "style", "user": "root", "note": "伪造的说明"}); code != 200 {
		t.Fatalf("多余字段被忽略: %d", code)
	}
	as := auditsNamed(he2, "水印篡改")
	if len(as) != 1 || as[0].UserName != "bob" || strings.Contains(as[0].ExtraInfo, "伪造") || strings.Contains(as[0].ExtraInfo, "root") {
		t.Errorf("审计只取会话里的用户和枚举 kind: %+v", as)
	}
	// 未登录
	if code, _ := he.browser().json("POST", "/api/v1/auth/watermark-tamper", map[string]any{"kind": "removed"}); code != 401 {
		t.Errorf("未登录: %d", code)
	}
}

// S7a-A2：退出登录写审计（会话存在才记，含 IP）。
func TestLogoutAudited(t *testing.T) {
	he := newHTTPEnv(t)
	bob := he.loginAs("bob")
	// CSRF 不对：被拒绝，不记
	if w := bob.do("POST", "/logout", url.Values{"csrf": {"wrong"}}, nil); w.Code != 403 {
		t.Fatalf("CSRF 错误: %d", w.Code)
	}
	if n := len(auditsNamed(he, "退出登录")); n != 0 {
		t.Fatalf("没退出成功不应记: %d", n)
	}
	w := bob.do("POST", "/logout", url.Values{"csrf": {bob.session().CSRF}}, nil)
	if w.Code != 303 || bob.cookie("dba_session") != nil {
		t.Fatalf("退出: %d", w.Code)
	}
	as := auditsNamed(he, "退出登录")
	if len(as) != 1 || as[0].UserName != "bob" || as[0].UserID == 0 || !strings.Contains(as[0].ExtraInfo, "192.0.2.1") {
		t.Fatalf("应写退出登录审计（含用户与 IP）: %+v", as)
	}
	// 没有会话（已退出 / 过期）再 POST：跳登录页，不记
	if w := bob.do("POST", "/logout", url.Values{"csrf": {"x"}}, nil); w.Code == 200 {
		t.Errorf("无会话: %d", w.Code)
	}
	if n := len(auditsNamed(he, "退出登录")); n != 1 {
		t.Errorf("没有会话不应再记: %d", n)
	}
}

func lastAuditOf(e *testEnv, action string) (model.AuditEntry, bool) {
	e.repo.mu.Lock()
	defer e.repo.mu.Unlock()
	for i := len(e.repo.audits) - 1; i >= 0; i-- {
		if e.repo.audits[i].Action == action {
			return e.repo.audits[i], true
		}
	}
	return model.AuditEntry{}, false
}

// S7b-C6：修改登录方式的审计写变化字段的旧 → 新；密钥类字段只写“已修改”，不写值（也不写旧值）。
func TestUpdateProviderAuditDiffHidesSecrets(t *testing.T) {
	e := newTestEnv(t)
	p, err := e.svc.CreateProvider(rootActor, KindLDAP, "公司域账号")
	if err != nil {
		t.Fatal(err)
	}
	cfg := p.Config
	cfg.ServerURI, cfg.ConnSecurity, cfg.SearchBase, cfg.SearchFilter = "ldaps://ldap.example.com:636", "ldaps", "ou=people,dc=example,dc=com", "(sAMAccountName=%(user)s)"
	cfg.BindDN, cfg.BindPassword = "cn=reader,dc=example,dc=com", "OLD-bind-secret-111"
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: p.Name, FirstLogin: FirstLoginCreate, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	// 第二次：改地址、改名称、改入组范围、换绑定密码
	cur, _ := e.repo.GetProvider(p.ID)
	cfg2 := cur.Config
	cfg2.ServerURI = "ldaps://ldap2.example.com:636"
	cfg2.BindPassword = "NEW-bind-secret-222"
	in := ProviderInput{Name: "公司域账号-新", FirstLogin: FirstLoginExisting, Config: cfg2, AuthGroups: []uint{3}}
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, in); err != nil {
		t.Fatal(err)
	}
	a, ok := lastAuditOf(e, "登录方式-修改")
	if !ok {
		t.Fatal("应有修改审计")
	}
	x := a.ExtraInfo
	for _, want := range []string{"server_uri", "ldaps://ldap.example.com:636 → ldaps://ldap2.example.com:636", "name", "公司域账号 → 公司域账号-新",
		"first_login", "create → existing", "auth_groups", "bind_password: 已修改"} {
		if !strings.Contains(x, want) {
			t.Errorf("审计缺少 %q: %s", want, x)
		}
	}
	for _, leak := range []string{"OLD-bind-secret-111", "NEW-bind-secret-222"} {
		if strings.Contains(x, leak) {
			t.Errorf("审计不能含密钥值 %q: %s", leak, x)
		}
	}
	// 没改的字段不出现
	if strings.Contains(x, "search_base") || strings.Contains(x, "bind_dn") {
		t.Errorf("没变化的字段不应出现: %s", x)
	}
	// 密钥留空 = 不改：不写“已修改”
	cur, _ = e.repo.GetProvider(p.ID)
	cfg3 := cur.Config
	cfg3.BindPassword = ""
	cfg3.TimeoutSec = 15
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: cur.Name, FirstLogin: cur.FirstLogin, Config: cfg3, AuthGroups: []uint{3}}); err != nil {
		t.Fatal(err)
	}
	a, _ = lastAuditOf(e, "登录方式-修改")
	if strings.Contains(a.ExtraInfo, "bind_password") || !strings.Contains(a.ExtraInfo, "timeout_sec") {
		t.Errorf("留空不改密钥: %s", a.ExtraInfo)
	}
	// 清空密钥：只写“已修改”类字样，仍不含值
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: cur.Name, FirstLogin: cur.FirstLogin, Config: cfg3, AuthGroups: []uint{3}, ClearSecrets: []string{"bind_password"}}); err != nil {
		t.Fatal(err)
	}
	a, _ = lastAuditOf(e, "登录方式-修改")
	if !strings.Contains(a.ExtraInfo, "bind_password: 已修改") || strings.Contains(a.ExtraInfo, "NEW-bind-secret") {
		t.Errorf("清空密钥: %s", a.ExtraInfo)
	}
	// 类型 OIDC 的 client_secret 同样不泄露
	o, _ := e.svc.CreateProvider(rootActor, KindOIDC, "sso")
	oc := o.Config
	oc.WellKnownURL, oc.ClientID, oc.ClientSecret = "https://idp.example.com/.well-known/openid-configuration", "cid", "OIDC-secret-AAA"
	if _, _, err := e.svc.UpdateProvider(rootActor, o.ID, ProviderInput{Name: "sso", FirstLogin: FirstLoginCreate, Config: oc}); err != nil {
		t.Fatal(err)
	}
	a, _ = lastAuditOf(e, "登录方式-修改")
	if !strings.Contains(a.ExtraInfo, "client_secret: 已修改") || strings.Contains(a.ExtraInfo, "OIDC-secret-AAA") || !strings.Contains(a.ExtraInfo, "client_id") {
		t.Errorf("OIDC: %s", a.ExtraInfo)
	}
}

// S7b-C6：管理员发起的 SSO 测试登录回调也写审计（成功 / 失败都记，不含令牌与授权码）。
func TestSSOTestLoginCallbackAudited(t *testing.T) {
	e := newTestEnv(t)
	idp := newFakeOIDC(t, e.clk)
	p := e.addOIDC(idp, FirstLoginCreate)
	ctx := context.Background()
	before := len(e.repo.auditActions())
	st, err := e.svc.BeginSSO(ctx, p.ID, BeginSSOInput{Test: true, Tester: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 别人拿到回调链接完成：被拒绝，不记成测试结果
	if _, err := e.svc.CompleteSSO(ctx, p.ID, idp.authorize(st.URL), st.CookieValue, 2, ""); err == nil {
		t.Fatal("应拒绝")
	}
	if _, ok := lastAuditOf(e, "登录方式-测试"); ok && len(e.repo.auditActions()) != before {
		for _, a := range e.repo.auditActions()[before:] {
			if a == "登录方式-测试" {
				t.Fatal("被拒绝的回调不应写测试审计")
			}
		}
	}
	st, _ = e.svc.BeginSSO(ctx, p.ID, BeginSSOInput{Test: true, Tester: 1})
	q := idp.authorize(st.URL)
	res, err := e.svc.CompleteSSO(ctx, p.ID, q, st.CookieValue, 1, "")
	if err != nil || res.Test == nil || !res.Test.OK {
		t.Fatalf("%v %+v", err, res)
	}
	a, ok := lastAuditOf(e, "登录方式-测试")
	if !ok || a.UserName != "root" || !strings.Contains(a.ExtraInfo, p.Name) || !strings.Contains(a.ExtraInfo, "测试登录回调") || !strings.HasSuffix(a.ExtraInfo, "true") {
		t.Fatalf("成功的测试登录回调应写审计: %+v", a)
	}
	if code := q.Get("code"); code != "" && strings.Contains(a.ExtraInfo, code) {
		t.Errorf("审计不能含授权码")
	}
	// 失败的回调也记，结果为失败
	idp.claims = map[string]any{"nonce": "x"}
	st, _ = e.svc.BeginSSO(ctx, p.ID, BeginSSOInput{Test: true, Tester: 1})
	res, _ = e.svc.CompleteSSO(ctx, p.ID, idp.authorize(st.URL), st.CookieValue, 1, "")
	if res == nil || res.Test == nil || res.Test.OK {
		t.Fatal("应失败")
	}
	a, _ = lastAuditOf(e, "登录方式-测试")
	if !strings.HasSuffix(a.ExtraInfo, "false") {
		t.Errorf("失败的回调: %+v", a)
	}
	if got := audit.Classify(a.Action, a.ExtraInfo); got.Result != audit.ResultFailed {
		t.Errorf("归类: %+v", got)
	}
}
