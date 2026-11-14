// @author: kevinkelly
// @license: Apache Licence
// @file: account_test.go
// @time: 2026/10/28

package authx

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/crypto"
)

func strp(s string) *string { return &s }
func intp(n int) *int       { return &n }

// ---- 页面水印设置 ----

func TestWatermarkDefaultsAndValidate(t *testing.T) {
	e := newTestEnv(t)
	set := e.svc.Settings()
	if set.WM != DefaultWatermark() || set.WM.Text != "{姓名}（{账号}）{时间}" {
		t.Fatalf("旧库没有水印项时应用默认值: %+v", set.WM)
	}
	good := DefaultWatermark()
	bad := map[string]func(w *WatermarkStyle){
		"空内容":       func(w *WatermarkStyle) { w.Text = "  " },
		"没有账号 / 姓名": func(w *WatermarkStyle) { w.Text = "{IP} {时间}" },
		"未知变量":      func(w *WatermarkStyle) { w.Text = "{账号}{foo}" },
		"花括号不成对":    func(w *WatermarkStyle) { w.Text = "{账号} {" },
		"内容过长":      func(w *WatermarkStyle) { w.Text = "{账号}" + strings.Repeat("字", 57) },
		"附加文字过长":    func(w *WatermarkStyle) { w.Extra = strings.Repeat("字", 41) },
		"附加文字未知变量":  func(w *WatermarkStyle) { w.Extra = "{abc}" },
		"透明度太小":     func(w *WatermarkStyle) { w.Opacity = 2 },
		"透明度太大":     func(w *WatermarkStyle) { w.Opacity = 31 },
		"字号太小":      func(w *WatermarkStyle) { w.FontSize = 11 },
		"字号太大":      func(w *WatermarkStyle) { w.FontSize = 29 },
		"角度太小":      func(w *WatermarkStyle) { w.Angle = -61 },
		"角度为正":      func(w *WatermarkStyle) { w.Angle = 1 },
		"疏密非法":      func(w *WatermarkStyle) { w.Gap = "huge" },
		"颜色非法":      func(w *WatermarkStyle) { w.Color = "pink" },
	}
	for name, mut := range bad {
		w := good
		mut(&w)
		if err := w.Validate(); err == nil {
			t.Errorf("%s 应被拒绝: %+v", name, w)
		}
	}
	for _, ok := range []func(w *WatermarkStyle){
		func(w *WatermarkStyle) { w.Angle = 0 }, func(w *WatermarkStyle) { w.Angle = -60 }, func(w *WatermarkStyle) { w.Opacity = 3 },
		func(w *WatermarkStyle) { w.Opacity = 30 }, func(w *WatermarkStyle) { w.Text = "{账号}" }, func(w *WatermarkStyle) { w.Text = "{姓名} {部门} {IP} {日期} {时间}" },
		func(w *WatermarkStyle) { w.Extra = "内部资料 严禁外传 {日期}" },
	} {
		w := good
		ok(&w)
		if err := w.Validate(); err != nil {
			t.Errorf("合法值被拒绝: %+v %v", w, err)
		}
	}
	// 读取时不合法的项各自回落默认值（不影响登录）
	w := wmFromKV(map[string]string{settingWMText: "没有变量", settingWMOpacity: "99", settingWMAngle: "-30", settingWMGap: "loose", settingWMColor: "x"})
	if w.Text != DefaultWatermark().Text || w.Opacity != 14 || w.Angle != -30 || w.Gap != "loose" || w.Color != "auto" {
		t.Errorf("回落: %+v", w)
	}
}

func TestWatermarkSaveAndAtomic(t *testing.T) {
	e := newTestEnv(t)
	cur := e.svc.Settings()
	in := cur.View().SecurityInput
	in.WatermarkPatch = WatermarkPatch{Text: strp("{账号}@{IP} {时间}"), Extra: strp("内部资料"), Opacity: intp(20), FontSize: intp(16), Angle: intp(0), Gap: strp("loose"), Color: strp("red")}
	set, err := SaveSecurity(e.repo, cur, in, "root")
	if err != nil {
		t.Fatal(err)
	}
	want := WatermarkStyle{Text: "{账号}@{IP} {时间}", Extra: "内部资料", Opacity: 20, FontSize: 16, Angle: 0, Gap: "loose", Color: "red"}
	if set.WM != want {
		t.Fatalf("保存后: %+v", set.WM)
	}
	if e.repo.kv[settingWMAngle] != "0" || e.repo.kv[settingWMColor] != "red" {
		t.Errorf("应写入系统设置: %v", e.repo.kv)
	}
	// 非法值：整体拒绝，别的修改也不落库
	cur = e.svc.Settings()
	in = cur.View().SecurityInput
	in.LockCount = 9
	in.WatermarkPatch = WatermarkPatch{Text: strp("只有文字没有账号")}
	if _, err := SaveSecurity(e.repo, cur, in, "root"); err == nil || !strings.Contains(err.Error(), "{账号}") {
		t.Fatalf("应拒绝: %v", err)
	}
	if got, _ := LoadSettings(e.repo); got.LockCount == 9 || got.WM != want {
		t.Errorf("非法水印配置不应有任何落库: lock=%d wm=%+v", got.LockCount, got.WM)
	}
	// 不带水印字段（旧页面 / 脚本）：保持不变
	cur = e.svc.Settings()
	in = cur.View().SecurityInput
	in.WatermarkPatch = WatermarkPatch{}
	in.LockCount = 6
	if set, err = SaveSecurity(e.repo, cur, in, "root"); err != nil || set.WM != want || set.LockCount != 6 {
		t.Errorf("不带水印字段应保持原值: %v %+v", err, set.WM)
	}
	// View 回显
	v := set.View()
	if v.Text == nil || *v.Text != want.Text || *v.Angle != 0 {
		t.Errorf("视图应回显水印配置: %+v", v.WatermarkPatch)
	}
}

func TestExpandWatermark(t *testing.T) {
	got := ExpandWatermark("{姓名}（{账号}）{部门} {IP} {时间} {foo}", map[string]string{"姓名": "张三", "账号": "zs", "部门": "", "IP": "10.0.0.1"})
	if got != "张三（zs） 10.0.0.1 {时间} {foo}" {
		t.Errorf("%q", got)
	}
	e := newTestEnv(t)
	e.repo.dept[2] = "数据库组"
	e.repo.cfg[cfgWatermark] = "true"
	e.repo.kv[settingWMText] = "{部门}-{账号}-{日期}"
	e.svc.settings.Invalidate()
	w := e.svc.WatermarkFor(2, "bob", "鲍勃", "10.9.9.9")
	if !w.Enabled || w.Text != "数据库组-bob-{日期}" || w.Vars["姓名"] != "鲍勃" || w.Vars["IP"] != "10.9.9.9" || w.Opacity != 14 || w.Gap != "normal" {
		t.Errorf("%+v", w)
	}
}

func TestWatermarkHTTP(t *testing.T) {
	he := newHTTPEnv(t)
	bob, root := he.loginAs("bob"), he.loginAs("root")
	code, out := bob.json("GET", "/api/v1/auth/ui", nil)
	d := out["data"].(map[string]any)
	if code != 200 || d["watermark"] != false || d["watermark_gap"] != "normal" {
		t.Fatalf("默认关闭且带样式默认值: %d %v", code, out)
	}
	he.repoCfgSet(cfgWatermark, "true")
	he.repo2().dept[2] = "数据组"
	_, out = bob.json("GET", "/api/v1/auth/ui", nil)
	d = out["data"].(map[string]any)
	if d["watermark"] != true || d["watermark_text"] != "bob（bob）{时间}" || d["watermark_opacity"].(float64) != 14 {
		t.Errorf("默认内容 = 姓名（账号）时间: %v", d)
	}
	// 普通用户不能改
	if code, _ = bob.json("PUT", "/api/v1/settings/security", map[string]any{}); code != 403 {
		t.Errorf("仅超级用户可改: %d", code)
	}
	// 超级用户改：非法 → 400 且不落库；合法 → 立即生效
	_, cur := root.json("GET", "/api/v1/settings/security", nil)
	settings := cur["data"].(map[string]any)["settings"].(map[string]any)
	if cur["data"].(map[string]any)["me"].(map[string]any)["账号"] != "root" {
		t.Errorf("安全设置接口应给出当前用户的水印变量: %v", cur["data"])
	}
	settings["watermark_text"] = "没有账号变量"
	if code, out = root.json("PUT", "/api/v1/settings/security", settings); code != 400 || !strings.Contains(out["message"].(string), "{账号}") {
		t.Fatalf("非法水印: %d %v", code, out)
	}
	if _, out = bob.json("GET", "/api/v1/auth/ui", nil); out["data"].(map[string]any)["watermark_text"] != "bob（bob）{时间}" {
		t.Error("非法值不能落库")
	}
	settings["watermark_text"] = "{部门}/{账号} {日期}"
	settings["watermark_angle"] = -45
	if code, out = root.json("PUT", "/api/v1/settings/security", settings); code != 200 {
		t.Fatalf("合法水印: %d %v", code, out)
	}
	_, out = bob.json("GET", "/api/v1/auth/ui", nil)
	d = out["data"].(map[string]any)
	if d["watermark_text"] != "数据组/bob {日期}" || d["watermark_angle"].(float64) != -45 {
		t.Errorf("应按当前用户展开: %v", d)
	}
	if !strings.Contains(strings.Join(he.repo2().auditActions(), ","), "安全设置-修改") {
		t.Error("修改水印应写审计")
	}
	last := he.repo2().audits[len(he.repo2().audits)-1]
	if !strings.Contains(last.ExtraInfo, "watermark_text") || !strings.Contains(last.ExtraInfo, "watermark_angle") {
		t.Errorf("审计应有水印项的差异: %s", last.ExtraInfo)
	}
}

// ---- 自助修改密码 ----

func TestChangePassword(t *testing.T) {
	e := newTestEnv(t)
	const newPw = "Brand-new-pass-77"
	cases := []struct {
		name               string
		cur, nw, cfm, want string
	}{
		{"空", "", "", "", "请填写"},
		{"两次不一致", pw, newPw, newPw + "x", "不一致"},
		{"与当前相同", pw, pw, pw, "不能与当前密码相同"},
		{"太短", pw, "Ab1!x", "Ab1!x", "太短"},
		{"全数字", pw, "1234567890123", "1234567890123", "全是数字"},
		{"与用户名相似", pw, "bob-bob-bob-1", "bob-bob-bob-1", "相似"},
	}
	for _, c := range cases {
		err := e.svc.ChangePassword(2, c.cur, c.nw, c.cfm, "10.0.0.1")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// 当前密码不对：统一信息，并写审计
	if err := e.svc.ChangePassword(2, "wrong-wrong-1", newPw, newPw, ""); err != ErrPwdCurrentBad {
		t.Fatalf("当前密码错误: %v", err)
	}
	if u, _ := e.repo.UserByID(2); !crypto.CheckDjangoPassword(pw, u.Password) {
		t.Fatal("失败不能改密码")
	}
	// 成功
	e.repo.mustChange[2] = true
	if err := e.svc.ChangePassword(2, pw, newPw, newPw, "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	u, _ := e.repo.UserByID(2)
	if !crypto.CheckDjangoPassword(newPw, u.Password) || crypto.CheckDjangoPassword(pw, u.Password) || !strings.HasPrefix(u.Password, "pbkdf2_sha256$600000$") {
		t.Errorf("应换成新的 Django 哈希: %s", u.Password)
	}
	if e.repo.mustChange[2] {
		t.Error("改密成功后应清除“必须修改密码”标记")
	}
	if a, err := e.login("bob", newPw); err != nil || a.User.ID != 2 {
		t.Errorf("新密码可登录: %v", err)
	}
	acts := strings.Join(e.repo.auditActions(), ",")
	if !strings.Contains(acts, "修改密码失败") || !strings.Contains(acts, "修改密码") {
		t.Errorf("审计: %s", acts)
	}
	for _, a := range e.repo.audits {
		if strings.Contains(a.ExtraInfo, newPw) || strings.Contains(a.ExtraInfo, pw) {
			t.Fatalf("审计里不能有密码: %+v", a)
		}
	}
	// 外部认证账号（不可用密码）：不能自助改
	e.repo.addUser(50, "ext.user", "", false)
	if err := e.svc.ChangePassword(50, "x", newPw, newPw, ""); err != ErrPwdExternal {
		t.Errorf("外部账号: %v", err)
	}
	if err := e.svc.ChangePassword(999, "x", newPw, newPw, ""); err == nil {
		t.Error("用户不存在")
	}
	// 密码策略复杂度生效
	e.repo.kv[settingPwdCplx] = "true"
	e.svc.settings.Invalidate()
	if err := e.svc.ChangePassword(2, newPw, "OnlyLettersAndNumbers12", "OnlyLettersAndNumbers12", ""); err == nil || !strings.Contains(err.Error(), "特殊字符") {
		t.Errorf("复杂度: %v", err)
	}
}

func TestChangePasswordRateLimit(t *testing.T) {
	e := newTestEnv(t)
	for i := 0; i < pwdMaxFail; i++ {
		if err := e.svc.ChangePassword(2, "wrong-wrong-1", "Brand-new-pass-77", "Brand-new-pass-77", ""); err != ErrPwdCurrentBad {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
	}
	// 之后即使当前密码对也拒绝（不让攻击者继续猜）
	if err := e.svc.ChangePassword(2, pw, "Brand-new-pass-77", "Brand-new-pass-77", ""); err != ErrPwdTooMany {
		t.Fatalf("应限频: %v", err)
	}
	e.clk.Add(pwdWindow + time.Second)
	if err := e.svc.ChangePassword(2, pw, "Brand-new-pass-77", "Brand-new-pass-77", ""); err != nil {
		t.Errorf("窗口过后恢复: %v", err)
	}
}

func TestPasswordPagesHTTP(t *testing.T) {
	he := newHTTPEnv(t)
	// 未登录：跳登录页
	if w := he.browser().get("/account/password"); w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Fatalf("未登录: %d", w.Code)
	}
	b := he.loginAs("bob")
	w := b.get("/account/password")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `id="pw-form"`) || !strings.Contains(body, "至少 9 个字符") || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("改密页: %d", w.Code)
	}
	if strings.Contains(body, "管理员要求你先修改密码") {
		t.Error("没被要求时不显示强制提示")
	}
	// JSON 接口
	const newPw = "Brand-new-pass-77"
	if code, out := b.json("POST", "/api/v1/account/password", map[string]string{"current": "wrong-wrong-1", "new": newPw, "confirm": newPw}); code != 401 || out["code"] != "pwd_current" {
		t.Errorf("当前密码错误: %d %v", code, out)
	}
	if code, out := b.json("POST", "/api/v1/account/password", map[string]string{"current": pw, "new": "short", "confirm": "short"}); code != 400 || !strings.Contains(out["message"].(string), "太短") {
		t.Errorf("策略: %d %v", code, out)
	}
	// 缺 CSRF 头
	b2 := he.browser()
	b2.cookies["dba_session"] = b.cookie("dba_session")
	rec := b2.do("POST", "/api/v1/account/password", nil, map[string]string{"Content-Type": "application/json"})
	if rec.Code != 403 {
		t.Errorf("缺 CSRF: %d", rec.Code)
	}
	if code, out := b.json("POST", "/api/v1/account/password", map[string]string{"current": pw, "new": newPw, "confirm": newPw}); code != 200 || out["data"].(map[string]any)["ok"] != true {
		t.Fatalf("成功: %d %v", code, out)
	}
	if code, _ := b.json("GET", "/api/v1/auth/2fa", nil); code != 200 {
		t.Errorf("当前会话应保持有效: %d", code)
	}
	if _, err := he.login("bob", newPw); err != nil {
		t.Errorf("新密码生效: %v", err)
	}
	// 表单提交（无脚本时）：CSRF 不对 → 403；对 → 成功提示
	b = he.browser()
	if w := b.postLogin("bob", newPw, "/query"); w.Code != 303 {
		t.Fatalf("新密码登录: %d", w.Code)
	}
	csrf := b.session().CSRF
	form := url.Values{"current": {newPw}, "new": {"Another-pass-88-x"}, "confirm": {"Another-pass-88-x"}, "csrf": {"forged"}}
	if w := b.do("POST", "/account/password", form, nil); w.Code != 403 {
		t.Errorf("表单缺 CSRF: %d", w.Code)
	}
	form.Set("csrf", csrf)
	if w := b.do("POST", "/account/password", form, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "密码已修改") {
		t.Errorf("表单成功: %d", w.Code)
	}
	// 外部认证账号：页面是说明，接口拒绝
	he.repo2().addUser(60, "sso.user", "", false)
	sb := he.browser()
	sb.cookies["dba_session"] = &http.Cookie{Name: "dba_session", Value: signSession(60, "sso.user", time.Now().Add(time.Hour), "c60"), Path: "/"}
	w = sb.get("/account/password")
	if w.Code != 200 || strings.Contains(w.Body.String(), `id="pw-form"`) || !strings.Contains(w.Body.String(), "外部认证") {
		t.Errorf("外部账号页面: %d", w.Code)
	}
	if code, out := sb.json("POST", "/api/v1/account/password", map[string]string{"current": "x", "new": newPw, "confirm": newPw}); code != 400 || out["code"] != "pwd_external" {
		t.Errorf("外部账号接口: %d %v", code, out)
	}
	// 账号信息接口
	if _, out := sb.json("GET", "/api/v1/account/info", nil); out["data"].(map[string]any)["local_password"] != false || out["data"].(map[string]any)["auth_method"] != "external" {
		t.Errorf("账号信息: %v", out)
	}
	if _, out := he.loginAs("root").json("GET", "/api/v1/account/info", nil); out["data"].(map[string]any)["local_password"] != true {
		t.Errorf("本地账号信息: %v", out)
	}
}

// ---- 强制改密 ----

func TestMustChangePasswordGate(t *testing.T) {
	he := newHTTPEnv(t)
	b := he.loginAs("bob")
	if w := b.get("/query"); w.Code == 302 && strings.Contains(w.Header().Get("Location"), "/account/password") {
		t.Fatal("没被要求时不拦截")
	}
	he.repo2().mustChange[2] = true
	he.clk.Add(time.Minute) // 越过 15 秒缓存
	w := b.get("/query")
	if w.Code != 302 || w.Header().Get("Location") != "/account/password?force=1" {
		t.Fatalf("页面应被拦到改密页: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := b.get("/ui/query/databases"); w.Code != 403 {
		t.Errorf("片段请求直接 403: %d", w.Code)
	}
	if code, out := b.json("GET", "/api/v1/auth/2fa", nil); code != 403 || out["code"] != "password_change_required" {
		t.Errorf("接口应拒绝: %d %v", code, out)
	}
	// 放行：改密页、退出、改密接口、水印配置
	w = b.get("/account/password?force=1")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "管理员要求你先修改密码") {
		t.Errorf("改密页: %d", w.Code)
	}
	if code, _ := b.json("GET", "/api/v1/auth/ui", nil); code != 200 {
		t.Errorf("水印配置仍可取: %d", code)
	}
	const newPw = "Brand-new-pass-77"
	if code, out := b.json("POST", "/api/v1/account/password", map[string]string{"current": pw, "new": newPw, "confirm": newPw}); code != 200 {
		t.Fatalf("改密: %d %v", code, out)
	}
	if he.repo2().mustChange[2] {
		t.Error("标记应清除")
	}
	if w := b.get("/account/password"); strings.Contains(w.Body.String(), "管理员要求你先修改密码") {
		t.Error("改密后不再提示")
	}
	if code, _ := b.json("GET", "/api/v1/auth/2fa", nil); code != 200 {
		t.Errorf("改密后恢复访问: %d", code)
	}
	// JWT 调用不受影响（脚本不可能打开改密页）
	he.repo2().mustChange[2] = true
	he.clk.Add(time.Minute)
	if code, _ := he.apiLogin("bob", newPw, ""); code != 200 {
		t.Errorf("JWT 登录: %d", code)
	}
}

// ---- 匹配依据：每个登录方式可覆盖全局，含手机号 ----

func TestMatchByPerProviderAndPhone(t *testing.T) {
	e := newTestEnv(t)
	e.repo.tfa[2] = []TwoFAConfig{{Type: "sms", Phone: "13800000000"}}
	e.repo.users[2].Email = "bob@example.com"
	set := e.svc.Settings() // 默认全局：用户名 + 邮箱
	id := &Identity{Username: "bob", Email: "x@nomatch.com", EmailVerified: true, Phone: "+86 138-0000-0000"}
	u, via, err := e.svc.matchUser(&Provider{ID: 1}, id, set)
	if err != nil || u == nil || via != MatchUsername {
		t.Fatalf("全局默认按用户名: %v %v", via, err)
	}
	only := &Provider{ID: 1, Config: Config{MatchBy: []string{MatchPhone}}}
	if u, via, _ = e.svc.matchUser(only, &Identity{Username: "bob", Phone: "+86 138-0000-0000"}, set); u == nil || u.ID != 2 || via != MatchPhone {
		t.Fatalf("覆盖为只按手机号（国家码与分隔符被规整）: %v %v", u, via)
	}
	if u, _, _ = e.svc.matchUser(only, &Identity{Username: "bob", Phone: "13911112222"}, set); u != nil {
		t.Error("手机号不匹配时，覆盖后不再退回按用户名匹配")
	}
	if u, _, _ = e.svc.matchUser(only, &Identity{Username: "bob"}, set); u != nil {
		t.Error("外部没返回手机号")
	}
	// 手机号被两个用户绑定：不猜
	e.repo.tfa[1] = []TwoFAConfig{{Type: "sms", Phone: "13800000000"}}
	if u, _, _ = e.svc.matchUser(only, &Identity{Phone: "13800000000"}, set); u != nil {
		t.Error("多个用户命中同一手机号不应采用")
	}
	// 全局也可以选手机号
	e.repo.tfa[1] = nil
	e.repo.kv[settingMatchBy] = "phone"
	e.svc.settings.Invalidate()
	if u, via, _ = e.svc.matchUser(&Provider{ID: 1}, &Identity{Phone: "13800000000"}, e.svc.Settings()); u == nil || via != MatchPhone || matchLabel(via) != "手机号" {
		t.Errorf("全局手机号: %v %v", u, via)
	}
	if got := e.svc.Settings().MatchBy; len(got) != 1 || got[0] != MatchPhone {
		t.Errorf("全局设置应接受 phone: %v", got)
	}
}

func TestProviderMatchByConfig(t *testing.T) {
	e := newTestEnv(t)
	p := e.addLDAP(FirstLoginExisting)
	cur, _ := e.svc.GetProvider(p.ID)
	cfg := cur.Config
	cfg.MatchBy = []string{"bogus"}
	if _, _, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: cur.Name, FirstLogin: cur.FirstLogin, Config: cfg}); err == nil {
		t.Error("匹配依据不合法应被拒绝")
	}
	cfg.MatchBy, cfg.AttrPhone = []string{"phone", "phone", "email"}, " mobile "
	_, notice, err := e.svc.UpdateProvider(rootActor, p.ID, ProviderInput{Name: cur.Name, FirstLogin: cur.FirstLogin, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := e.svc.GetProvider(p.ID)
	if notice != "" || !after.Enabled || len(after.Config.MatchBy) != 2 || after.Config.MatchBy[0] != "phone" || after.Config.AttrPhone != "mobile" {
		t.Errorf("匹配依据 / 手机号属性不属于连接配置：改它不应让测试结果失效或自动停用: %q %+v", notice, after.Config)
	}
	if !after.Usable(e.svc.fp) {
		t.Error("仍应可用")
	}
	if v := e.svc.View(after, "http://dba.test"); len(v.Config.MatchBy) != 2 {
		t.Errorf("视图应带 match_by: %+v", v.Config)
	}
	// 手机号属性映射：默认属性与自定义
	id := mapIdentity(&Provider{Kind: KindOIDC}, "s", map[string][]string{"phone_number": {"+8613800000000"}}, "", "", true)
	if id.Phone != "+8613800000000" {
		t.Errorf("OIDC 默认取 phone_number: %q", id.Phone)
	}
	id = mapIdentity(&Provider{Kind: KindLDAP, Config: Config{AttrPhone: "pager"}}, "s", map[string][]string{"pager": {"139"}, "mobile": {"138"}}, "", "", true)
	if id.Phone != "139" {
		t.Errorf("自定义属性: %q", id.Phone)
	}
	if !contains(attrNamesNeeded(&Provider{Kind: KindLDAP}), "mobile") {
		t.Error("LDAP 查询应取回 mobile")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- 允许用户自行绑定 / 解绑 ----

func TestSelfBindRule(t *testing.T) {
	e := newTestEnv(t)
	if e.svc.Settings().AllowSelfBind || e.svc.Settings().Rules().SelfBind {
		t.Fatal("默认关闭")
	}
	set, err := SaveRules(e.repo, RulesInput{LocalFormMode: FormCollapse, MatchBy: []string{"username"}, SuperuserLocalOnly: true, SelfBind: true}, "root")
	if err != nil || !set.AllowSelfBind || !set.Rules().SelfBind {
		t.Fatalf("%v %+v", err, set.Rules())
	}
}

func TestUnbindAndBindings(t *testing.T) {
	e := newTestEnv(t)
	idp := newFakeOIDC(t, e.clk)
	p := e.forceUsable(e.addOIDC(idp, FirstLoginExisting))
	_ = e.repo.BindIdentity(p.ID, "sub-bob", 2)
	_ = e.repo.BindIdentity(p.ID, "sub-root", 1)
	bob := audit.Actor{ID: 2, Username: "bob", Display: "bob"}
	pg, err := e.svc.Bindings(2)
	if err != nil || pg.Allowed || len(pg.Items) != 1 || pg.Items[0].ProviderName != p.Name || len(pg.Available) != 0 {
		t.Fatalf("未开放时只读: %v %+v", err, pg)
	}
	if err := e.svc.Unbind(bob, pg.Items[0].ID); err != ErrBindOff {
		t.Fatalf("未开放不能解绑: %v", err)
	}
	e.repo.kv[settingSelfBind] = "true"
	e.svc.settings.Invalidate()
	// 只能解自己的：root 的那条编号换成 bob 去解
	rootPg, _ := e.svc.Bindings(1)
	if err := e.svc.Unbind(bob, rootPg.Items[0].ID); err != ErrBindNotFound {
		t.Errorf("不能解别人的: %v", err)
	}
	if err := e.svc.Unbind(bob, pg.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	if uid, _ := e.repo.IdentityUser(p.ID, "sub-bob"); uid != 0 {
		t.Error("应已解绑")
	}
	if uid, _ := e.repo.IdentityUser(p.ID, "sub-root"); uid != 1 {
		t.Error("别人的绑定不受影响")
	}
	pg, _ = e.svc.Bindings(2)
	if !pg.Allowed || len(pg.Items) != 0 || len(pg.Available) != 1 || pg.Available[0].Href != "/login/sso/"+itoa(p.ID)+"?bind=1" {
		t.Errorf("开放后可再绑定: %+v", pg)
	}
	if !strings.Contains(strings.Join(e.repo.auditActions(), ","), "解绑外部账号") {
		t.Error("解绑应写审计")
	}
	if info, _ := e.svc.Account(2); info == nil || info.SelfBind != true || info.AuthMethod != "local" {
		t.Errorf("账号信息: %+v", info)
	}
}

func TestBindExternalViaSSO(t *testing.T) {
	he := newHTTPEnv(t)
	idp := newFakeOIDC(t, he.clk)
	p := he.forceUsable(he.addOIDC(idp, FirstLoginExisting))
	bind := func(b *browser, id uint) string {
		w := b.get("/login/sso/" + itoa(id) + "?bind=1")
		if w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), idp.srv.URL+"/authorize?") {
			return w.Header().Get("Location") // 被拒绝，回到绑定页
		}
		w = b.get("/login/sso/" + itoa(id) + "/callback?" + idp.authorize(w.Header().Get("Location")).Encode())
		return w.Header().Get("Location")
	}
	// 未登录：去登录
	if w := he.browser().get("/login/sso/" + itoa(p.ID) + "?bind=1"); w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), "/login?next=") {
		t.Errorf("未登录发起绑定: %d %s", w.Code, w.Header().Get("Location"))
	}
	b := he.loginAs("bob")
	if loc := bind(b, p.ID); loc != "/account/bindings?err=bind_off" {
		t.Errorf("管理员未开放: %s", loc)
	}
	he.kvSet(settingSelfBind, "true")
	if loc := bind(b, p.ID); loc != "/account/bindings?bound=1" {
		t.Fatalf("绑定: %s", loc)
	}
	if uid, _ := he.repo2().IdentityUser(p.ID, "sub-001"); uid != 2 {
		t.Fatalf("应绑到 bob: %d", uid)
	}
	if !strings.Contains(strings.Join(he.repo2().auditActions(), ","), "绑定外部账号") {
		t.Error("绑定应写审计")
	}
	if b.session().User != "bob" {
		t.Error("绑定不能切换当前会话")
	}
	// 同一个外部账号再绑一次：当作成功；换了另一个外部账号：要先解绑
	if loc := bind(b, p.ID); loc != "/account/bindings?bound=1" {
		t.Errorf("重复绑定同一账号: %s", loc)
	}
	idp.claims = map[string]any{"sub": "sub-002"}
	if loc := bind(b, p.ID); loc != "/account/bindings?err=bind_dup" {
		t.Errorf("同一登录方式只能绑一个外部账号: %s", loc)
	}
	idp.claims = nil
	// 该外部账号已绑给 bob，别人不能再绑
	other := he.loginAs("root")
	if loc := bind(other, p.ID); loc != "/account/bindings?err=superuser_local" {
		t.Errorf("超级用户只能本地登录，不能绑定: %s", loc)
	}
	he.repo2().addUser(70, "carol", pw, false)
	cb := he.loginAs("carol")
	if loc := bind(cb, p.ID); loc != "/account/bindings?err=bind_taken" {
		t.Errorf("外部账号已被占用: %s", loc)
	}
	// 页面与接口
	w := b.get("/account/bindings?err=bind_taken")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "该外部账号已绑定到其他平台账号") {
		t.Errorf("绑定页: %d", w.Code)
	}
	if w := b.get("/account/bindings?err=%3Cscript%3E"); strings.Contains(w.Body.String(), "<script>") {
		t.Error("未知错误码不回显")
	}
	code, out := b.json("GET", "/api/v1/account/bindings", nil)
	items := out["data"].(map[string]any)["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("接口: %d %v", code, out)
	}
	id := int(items[0].(map[string]any)["id"].(float64))
	if code, _ = b.json("DELETE", "/api/v1/account/bindings/"+itoa(uint(id)), nil); code != 200 {
		t.Errorf("解绑: %d", code)
	}
	if code, _ = b.json("DELETE", "/api/v1/account/bindings/"+itoa(uint(id)), nil); code != 404 {
		t.Errorf("重复解绑: %d", code)
	}
	if code, _ = b.json("DELETE", "/api/v1/account/bindings/abc", nil); code != 400 {
		t.Errorf("非法编号: %d", code)
	}
	// 绑定的回调必须在发起绑定的同一个会话里完成
	b2 := he.loginAs("bob")
	w = b2.get("/login/sso/" + itoa(p.ID) + "?bind=1")
	q := idp.authorize(w.Header().Get("Location"))
	swap := he.loginAs("carol")
	swap.cookies["dba_sso_"+itoa(p.ID)] = b2.cookie("dba_sso_" + itoa(p.ID))
	if w := swap.get("/login/sso/" + itoa(p.ID) + "/callback?" + q.Encode()); !strings.Contains(w.Header().Get("Location"), "err=state") {
		t.Errorf("换了会话的回调应拒绝: %s", w.Header().Get("Location"))
	}
	if uid, _ := he.repo2().IdentityUser(p.ID, "sub-001"); uid != 0 {
		t.Error("不应被绑定")
	}
}

// ---- CAS 全局登出 ----

func TestCASGlobalLogout(t *testing.T) {
	he := newHTTPEnv(t)
	p := he.addKind(KindCAS, "旧 CAS", Config{CASURL: "https://cas.example.com/cas/", CASVersion: "3"}, FirstLoginExisting)
	he.forceUsable(p)
	if got := he.svc.CASLogoutURL(p.ID, "http://dba.test"); got != "" {
		t.Errorf("没开全局登出: %q", got)
	}
	cur, _ := he.repo2().GetProvider(p.ID)
	cur.Config.GlobalLogout = true
	_ = he.repo2().SaveProvider(cur)
	he.svc.invalidateProviders()
	want := "https://cas.example.com/cas/logout?service=" + url.QueryEscape("http://dba.test/login")
	if got := he.svc.CASLogoutURL(p.ID, "http://dba.test"); got != want {
		t.Errorf("CAS 登出地址:\n got %q\nwant %q", got, want)
	}
	if he.svc.CASLogoutURL(0, "") != "" || he.svc.CASLogoutURL(9999, "") != "" {
		t.Error("本地登录 / 不存在的登录方式不跳转")
	}
	// 退出登录：会话里记着 CAS → 303 到 CAS 登出地址；本地会话 → /login
	logout := func(prov uint) string {
		raw, _ := json.Marshal(map[string]any{"uid": 2, "u": "bob", "d": "bob", "su": false, "c": "csrf-9", "e": time.Now().Add(time.Hour).Unix(), "p": prov})
		sess := signRaw(raw)
		b := he.browser()
		b.cookies["dba_session"] = &http.Cookie{Name: "dba_session", Value: sess, Path: "/"}
		w := b.do("POST", "/logout", url.Values{"csrf": {"csrf-9"}}, nil)
		if w.Code != 303 {
			t.Fatalf("logout: %d", w.Code)
		}
		if b.cookie("dba_session") != nil {
			t.Error("会话应清除")
		}
		return w.Header().Get("Location")
	}
	if got := logout(p.ID); got != want {
		t.Errorf("退出应跳 CAS: %q", got)
	}
	if got := logout(0); got != "/login" {
		t.Errorf("本地登录退出: %q", got)
	}
	// 登录方式页的配置可以保存 global_logout
	if v := he.svc.View(cur, "http://dba.test"); !v.Config.GlobalLogout {
		t.Error("视图应带 global_logout")
	}
}

func signRaw(raw []byte) string {
	p := base64.RawURLEncoding.EncodeToString(raw)
	m := hmac.New(sha256.New, []byte(serverKey))
	m.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
