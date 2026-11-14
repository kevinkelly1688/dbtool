// @author: kevinkelly
// @license: Apache Licence
// @file: sysconfig_test.go
// @time: 2026/10/28

package sysconfig

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"dbtool/internal/config"
	"dbtool/internal/crypto"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
	"dbtool/internal/settings"
)

const testKey = "n3w-k3y-for-tests-0123456789abcdefghijklmnopqrstuvwxyz"

func newEnv(t *testing.T, svc *settings.Service, sec Security) *servertest.Env {
	t.Helper()
	return servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){
		func(s *server.Server, rt server.Routes) {
			register(s, rt, svc, func() Security { return sec })
		},
	}})
}

func memService(t *testing.T) (*settings.Service, *settings.MemStore) {
	t.Helper()
	m, err := crypto.NewMirage(testKey, "ECB", "1234567890abcdef")
	if err != nil {
		t.Fatal(err)
	}
	ms := settings.NewMemStore()
	return settings.New(ms, m), ms
}

func itemMap(items []settings.Item) map[string]settings.Item {
	out := map[string]settings.Item{}
	for _, it := range items {
		out[it.Key] = it
	}
	return out
}

func TestHTTPSuperuserOnlyAndNoSecretsInResponse(t *testing.T) {
	svc, _ := memService(t)
	if _, err := svc.Set(settings.Actor{Username: "seed"}, settings.KeyGoInceptionPassword, "P@ssw0rd-super-secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Set(settings.Actor{Username: "seed"}, settings.KeyQueryMaxExecSeconds, "30"); err != nil {
		t.Fatal(err)
	}
	env := newEnv(t, svc, Security{PublicSecretKey: true})

	if w := env.Anonymous("GET", "/api/v1/settings/config"); w.Code != 401 {
		t.Errorf("未登录应 401: %d", w.Code)
	}
	if w := env.Anonymous("GET", "/settings/config"); w.Code != 302 {
		t.Errorf("未登录页面应跳转登录: %d", w.Code)
	}
	bob := env.Login("bob")
	if w := bob.Do("GET", "/api/v1/settings/config", nil); w.Code != 403 {
		t.Errorf("普通用户应 403: %d", w.Code)
	}
	if code, _ := bob.Page("/settings/config"); code != 403 {
		t.Errorf("普通用户页面应 403: %d", code)
	}

	root := env.Login("root")
	w := root.Do("GET", "/api/v1/settings/config", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "P@ssw0rd-super-secret") || strings.Contains(body, testKey) || strings.Contains(body, config.DefaultSecretKey()) {
		t.Fatal("响应里不能出现密码 / 密钥")
	}
	var resp struct {
		Data View `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	v := resp.Data
	m := itemMap(v.Items)
	if !v.Ready || !v.Security.PublicSecretKey {
		t.Errorf("%+v", v)
	}
	if it := m[settings.KeyGoInceptionPassword]; it.Key != "" {
		t.Errorf("goInception 在专门页面维护，不应出现在系统配置页: %+v", it)
	}
	if it := m[settings.KeyQueryMaxExecSeconds]; it.Value != "30" || it.Effective != "30" || it.Default != "60" || !it.Modified || !it.Custom {
		t.Errorf("普通项: %+v", it)
	}
	if it := m[settings.KeySecurityLocalLastLogin]; it.Key != "" {
		t.Error("隐藏项不应出现在列表里")
	}
	if want := len(settings.Groups) - 1; len(v.Groups) != want {
		t.Errorf("分组数: %d，期望 %d（不含 goInception）", len(v.Groups), want)
	}
	for _, g := range v.Groups {
		if g.Key == settings.GroupGoInc {
			t.Error("goInception 分组不应出现在系统配置页")
		}
	}
	for _, g := range v.Groups {
		if g.Key == settings.GroupQuery && (g.Modified != 1 || g.Total == 0) {
			t.Errorf("在线查询分组计数: %+v", g)
		}
		if g.Key == settings.GroupGoInc && g.Modified != 1 {
			t.Errorf("goInception 分组计数: %+v", g)
		}
	}
	code, page := root.Page("/settings/config")
	if code != 200 || !strings.Contains(page, `id="st-main"`) || !strings.Contains(page, "/static/js/sysconfig/main.js") {
		t.Errorf("页面: %d", code)
	}
}

func TestHTTPTableMissing(t *testing.T) {
	ms := settings.NewMemStore()
	ms.NotReady = true
	svc := settings.New(ms, settings.PlainCipher{})
	env := newEnv(t, svc, Security{})
	var v View
	env.Login("root").GetJSON("/api/v1/settings/config", &v)
	if v.Ready || len(v.Items) != 0 {
		t.Errorf("表不存在: %+v", v)
	}
	w := env.Login("root").Do("PUT", "/api/v1/settings/config", map[string]any{"changes": map[string]any{settings.KeyQueryMaxExecSeconds: "10"}})
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != 409 || e.Code != "table_missing" {
		t.Errorf("表不存在时修改应 409 table_missing: %d %s", w.Code, w.Body.String())
	}
}

func TestHTTPSaveManyAtomicAndAudited(t *testing.T) {
	svc, ms := memService(t)
	env := newEnv(t, svc, Security{})
	bob := env.Login("bob")
	if w := bob.Do("PUT", "/api/v1/settings/config", map[string]any{"changes": map[string]any{settings.KeyQueryMaxExecSeconds: "10"}}); w.Code != 403 {
		t.Errorf("普通用户应 403: %d", w.Code)
	}
	root := env.Login("root")

	// 一项不合法 → 整体失败，逐项给出原因，什么都不写
	w := root.Do("PUT", "/api/v1/settings/config", map[string]any{"changes": map[string]any{
		settings.KeyQueryMaxExecSeconds: "20", settings.KeyFirewallMode: "bogus"}})
	if w.Code != 400 {
		t.Fatalf("应 400: %d %s", w.Code, w.Body.String())
	}
	var bad struct {
		Code string `json:"code"`
		Data struct {
			Fields map[string]string `json:"fields"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &bad)
	if bad.Code != "bad_value" || bad.Data.Fields[settings.KeyFirewallMode] == "" || len(bad.Data.Fields) != 1 {
		t.Errorf("逐项错误: %s", w.Body.String())
	}
	if _, ok := svc.Lookup(settings.KeyQueryMaxExecSeconds); ok {
		t.Fatal("整体失败后合法项也不应写入")
	}

	// 全部合法：写入、记变更、写审计
	w = root.Do("PUT", "/api/v1/settings/config", map[string]any{"changes": map[string]any{
		settings.KeyQueryMaxExecSeconds: "20", settings.KeyFirewallMode: "enforce", settings.KeyGoInceptionPassword: "S3cret"}})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "S3cret") {
		t.Error("响应不能回显敏感值")
	}
	var ok struct {
		Data struct {
			Changes []settings.Change `json:"changes"`
			Changed bool              `json:"changed"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &ok)
	if !ok.Data.Changed || len(ok.Data.Changes) != 3 {
		t.Errorf("变更: %+v", ok.Data)
	}
	if v, _ := svc.Lookup(settings.KeyGoInceptionPassword); v != "S3cret" {
		t.Errorf("敏感值应能读回: %q", v)
	}
	rows, _, _ := ms.Load()
	if r := rows[settings.KeyGoInceptionPassword]; !r.Encrypted || r.Value == "S3cret" {
		t.Errorf("敏感值应加密保存: %+v", r)
	}
	audits := ms.Audits()
	if len(audits) != 1 || audits[0].UserName != "root" || !strings.Contains(audits[0].ExtraInfo, settings.KeyQueryMaxExecSeconds) || strings.Contains(audits[0].ExtraInfo, "S3cret") {
		t.Errorf("审计: %+v", audits)
	}

	// 没有变化：changed=false，不写审计
	w = root.Do("PUT", "/api/v1/settings/config", map[string]any{"changes": map[string]any{settings.KeyQueryMaxExecSeconds: "20"}})
	_ = json.Unmarshal(w.Body.Bytes(), &ok)
	if w.Code != 200 || ok.Data.Changed || len(ms.Audits()) != 1 {
		t.Errorf("无变化: %d %s", w.Code, w.Body.String())
	}

	// null = 恢复默认
	w = root.Do("PUT", "/api/v1/settings/config", map[string]any{"changes": map[string]any{settings.KeyQueryMaxExecSeconds: nil}})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if _, has := svc.Lookup(settings.KeyQueryMaxExecSeconds); has {
		t.Error("null 应恢复默认")
	}

	// 请求格式 / 未知键 / 只读项
	cases := []struct {
		body any
		code int
		err  string
	}{
		{map[string]any{"changes": map[string]any{}}, 400, "bad_request"},
		{map[string]any{"other": 1}, 400, "bad_request"},
		{map[string]any{"changes": map[string]any{"no.such_key": "1"}}, 404, "not_found"},
		{map[string]any{"changes": map[string]any{settings.KeyOptimizerSoarPath: "/bin/sh"}}, 400, "bad_value"},
		{map[string]any{"changes": map[string]any{settings.KeySecurityLocalLastLogin: "x"}}, 400, "bad_value"},
	}
	for _, c := range cases {
		w := root.Do("PUT", "/api/v1/settings/config", c.body)
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != c.code || e.Code != c.err {
			t.Errorf("%v: %d %s %s", c.body, w.Code, e.Code, w.Body.String())
		}
	}
}

func TestHTTPSaveOneAndHistory(t *testing.T) {
	svc, _ := memService(t)
	env := newEnv(t, svc, Security{})
	root := env.Login("root")
	w := root.Do("PUT", "/api/v1/settings/config/"+settings.KeyQueryAdminRowLimit, map[string]any{"value": "777"})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if got := svc.Int(settings.KeyQueryAdminRowLimit); got != 777 {
		t.Errorf("得到 %d", got)
	}
	if w := root.Do("PUT", "/api/v1/settings/config/"+settings.KeyQueryAdminRowLimit, map[string]any{"other": 1}); w.Code != 400 {
		t.Errorf("缺 value 应 400: %d", w.Code)
	}
	if w := root.Do("PUT", "/api/v1/settings/config/"+settings.KeyQueryAdminRowLimit, map[string]any{"value": "x"}); w.Code != 400 {
		t.Errorf("非法值应 400: %d", w.Code)
	}

	var hist struct {
		Items []struct {
			Key    string `json:"key"`
			Label  string `json:"label"`
			New    string `json:"new"`
			Action string `json:"action"`
			By     string `json:"by"`
		} `json:"items"`
	}
	root.GetJSON("/api/v1/settings/config/history?key="+settings.KeyQueryAdminRowLimit, &hist)
	if len(hist.Items) != 1 || hist.Items[0].New != "777" || hist.Items[0].By != "root" || hist.Items[0].Label == "" {
		t.Errorf("变更记录: %+v", hist)
	}
	if w := root.Do("GET", "/api/v1/settings/config/history?key=no.such", nil); w.Code != 404 {
		t.Errorf("未知键应 404: %d", w.Code)
	}
}

// SOAR 的三项设置现在可以在页面里改：写法不合格、环境校验（Guard）不过都按 400 拒绝并指出是哪一项；通过后加密 / 明文保存且不回显密码。
func TestHTTPSoarSettings(t *testing.T) {
	svc, _ := memService(t)
	env := newEnv(t, svc, Security{})
	root := env.Login("root")

	settings.RegisterGuard(settings.KeyOptimizerSoarTestDSN, func(v string) error {
		if strings.Contains(v, "prod-db") {
			return errors.New("该地址是已登记的生产实例")
		}
		return nil
	})
	t.Cleanup(func() { settings.RegisterGuard(settings.KeyOptimizerSoarTestDSN, nil) })

	put := func(kv map[string]any) *httptest.ResponseRecorder {
		return root.Do("PUT", "/api/v1/settings/config", map[string]any{"changes": kv})
	}
	if w := put(map[string]any{settings.KeyOptimizerSoarTestDSN: "u:Secr3t@prod-db:3306/x"}); w.Code != 400 || !strings.Contains(w.Body.String(), "生产实例") {
		t.Fatalf("Guard 应拒绝: %d %s", w.Code, w.Body.String())
	}
	if _, has := svc.Lookup(settings.KeyOptimizerSoarTestDSN); has {
		t.Fatal("被拒绝的值不能保存")
	}
	w := put(map[string]any{
		settings.KeyOptimizerSoarPath:    "/opt/soar/bin/soar",
		settings.KeyOptimizerSoarConfig:  "/etc/soar.yaml",
		settings.KeyOptimizerSoarTestDSN: "u:Secr3t@test-db:3306/x",
	})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if got := svc.String(settings.KeyOptimizerSoarPath); got != "/opt/soar/bin/soar" {
		t.Errorf("得到 %q", got)
	}
	// 一批里有一项不合格，整批都不保存
	if w := put(map[string]any{settings.KeyOptimizerSoarPath: "/opt/other/soar", settings.KeyOptimizerSoarConfig: "etc/soar.yaml"}); w.Code != 400 {
		t.Fatalf("相对路径应拒绝: %d %s", w.Code, w.Body.String())
	}
	if got := svc.String(settings.KeyOptimizerSoarPath); got != "/opt/soar/bin/soar" {
		t.Errorf("整批应回滚，得到 %q", got)
	}
	// 列表：可编辑、标高危、密码不回显
	body := root.Do("GET", "/api/v1/settings/config", nil).Body.String()
	if strings.Contains(body, "Secr3t") {
		t.Error("测试库密码不能出现在页面数据里")
	}
	var data struct {
		Data struct {
			Items []struct {
				Key      string `json:"key"`
				Editable bool   `json:"editable"`
				Risk     string `json:"risk"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, it := range data.Data.Items {
		if strings.HasPrefix(it.Key, "optimizer.soar_") {
			seen++
			if !it.Editable || it.Risk == "" {
				t.Errorf("%s 应可编辑且标高危: %+v", it.Key, it)
			}
		}
	}
	if seen != 3 {
		t.Errorf("应有 3 个 SOAR 设置项，得到 %d", seen)
	}
}
