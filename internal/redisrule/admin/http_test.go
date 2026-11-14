// @author: kevinkelly
// @license: Apache Licence
// @file: http_test.go
// @time: 2026/10/28

package admin

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"dbtool/internal/btnperm"
	"dbtool/internal/btnperm/btnpermtest"
	"dbtool/internal/model"
	"dbtool/internal/redisrule"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

type memAudit struct {
	mu   sync.Mutex
	list []model.AuditEntry
}

func (m *memAudit) WriteLifecycleAudit(e *model.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.list = append(m.list, *e)
	return nil
}

func (m *memAudit) actions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, e := range m.list {
		out = append(out, e.Action)
	}
	return out
}

type env struct {
	*servertest.Env
	chk  *btnpermtest.Checker
	repo *redisrule.MemRepo
	au   *memAudit
}

func newEnv(t *testing.T) *env {
	t.Helper()
	h := &env{chk: btnpermtest.New(), repo: redisrule.NewMemRepo(), au: &memAudit{}}
	h.Env = servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){h.chk.Hook,
		func(s *server.Server, rt server.Routes) { Register(s, rt, redisrule.NewService(h.repo, h.au)) }}})
	return h
}

var goodRule = map[string]any{"name": "清理缓存", "enabled": true, "commands": []string{"del", "UNLINK"}, "key_patterns": []string{"cache:*"}, "max_keys": 50}

func with(m map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func TestAnonymousAndNormalUsersGet403(t *testing.T) {
	h := newEnv(t)
	if w := h.Anonymous("GET", "/api/v1/settings/redis-rules"); w.Code != http.StatusUnauthorized && w.Code != http.StatusFound {
		t.Errorf("未登录: %d", w.Code)
	}
	if w := h.Anonymous("GET", "/settings/redis-rules"); w.Code != http.StatusFound {
		t.Errorf("页面未登录应跳转: %d", w.Code)
	}
	// 即使把按钮“授予”普通用户也不行：两个按钮都是 locked，服务端只认超级用户
	h.chk.Grant(2, "redisrule.view", "redisrule.edit")
	bob := h.Login("bob")
	if code, _ := bob.Page("/settings/redis-rules"); code != 403 {
		t.Errorf("普通用户页面: %d", code)
	}
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/v1/settings/redis-rules"}, {"POST", "/api/v1/settings/redis-rules"}, {"PUT", "/api/v1/settings/redis-rules/1"},
		{"POST", "/api/v1/settings/redis-rules/1/enable"}, {"POST", "/api/v1/settings/redis-rules/1/delete"}, {"POST", "/api/v1/settings/redis-rules/test"},
	} {
		if code, _ := bob.JSON(c.m, c.p, with(goodRule, "confirm", true), nil); code != 403 {
			t.Errorf("%s %s 普通用户应是 403: %d", c.m, c.p, code)
		}
	}
	if n, _ := h.repo.List(); len(n) != 0 || len(h.au.actions()) != 0 {
		t.Error("被拒绝的请求不能改数据、不写审计")
	}
}

func TestButtonsAreLockedAndRegistered(t *testing.T) {
	for _, code := range []string{"redisrule.view", "redisrule.edit"} {
		b, ok := btnperm.Lookup(code)
		if !ok || !b.Locked || !b.Super || b.Grantable() {
			t.Errorf("%s 应登记为仅超级用户、不可授予: %+v", code, b)
		}
	}
	if b, _ := btnperm.Lookup("redisrule.view"); b.Kind != btnperm.KindView {
		t.Error("view 应是查看类")
	}
	if b, _ := btnperm.Lookup("redisrule.edit"); !b.Danger() {
		t.Error("edit 应是高危类")
	}
}

func TestSuperuserCRUDWithConfirmAndAudit(t *testing.T) {
	h := newEnv(t)
	root := h.Login("root")
	code, body := root.Page("/settings/redis-rules")
	if code != 200 || !strings.Contains(body, `id="rr"`) || !strings.Contains(body, "/static/js/redisrule/redisrule.js") {
		t.Fatalf("页面: %d", code)
	}
	var list struct {
		Ready     bool                 `json:"table_ready"`
		Rules     []redisrule.Rule     `json:"rules"`
		Supported []string             `json:"supported_commands"`
		Forbidden []string             `json:"hard_forbidden"`
		Templates []redisrule.Template `json:"templates"`
	}
	root.GetJSON("/api/v1/settings/redis-rules", &list)
	if !list.Ready || len(list.Rules) != 0 || len(list.Templates) < 3 {
		t.Fatalf("预置 0 条规则，有模板: %+v", list)
	}
	has := func(l []string, s string) bool {
		for _, x := range l {
			if x == s {
				return true
			}
		}
		return false
	}
	if !has(list.Forbidden, "FLUSHALL") || !has(list.Supported, "DEL") || has(list.Supported, "FLUSHALL") {
		t.Errorf("命令清单: %v / %v", list.Forbidden, list.Supported)
	}

	// 不带 confirm 不能写
	if code, msg := root.JSON("POST", "/api/v1/settings/redis-rules", goodRule, nil); code != 400 || !strings.Contains(msg, "二次确认") {
		t.Errorf("未确认: %d %s", code, msg)
	}
	if n, _ := h.repo.List(); len(n) != 0 {
		t.Error("未确认不应创建")
	}
	var created redisrule.Rule
	root.PostJSON("/api/v1/settings/redis-rules", with(goodRule, "confirm", true), &created)
	if created.ID == 0 || strings.Join(created.Commands, ",") != "DEL,UNLINK" || created.CreatedBy != "root" || created.MaxKeys != 50 {
		t.Fatalf("创建: %+v", created)
	}
	// 同名
	if code, _ := root.JSON("POST", "/api/v1/settings/redis-rules", with(goodRule, "confirm", true), nil); code != 409 {
		t.Errorf("同名: %d", code)
	}
	// 修改
	path := "/api/v1/settings/redis-rules/" + itoa(created.ID)
	var upd redisrule.Rule
	if code, _ := root.JSON("PUT", path, with(goodRule, "key_patterns", []string{"cache:*", "tmp:*"}), nil); code != 400 {
		t.Errorf("修改未确认: %d", code)
	}
	if code, msg := root.JSON("PUT", path, with(goodRule, "key_patterns", []string{"cache:*", "tmp:*"}, "confirm", true), &upd); code != 200 {
		t.Fatalf("修改: %d %s", code, msg)
	}
	if len(upd.KeyPatterns) != 2 {
		t.Errorf("修改结果: %+v", upd)
	}
	// 停用 / 启用
	var en redisrule.Rule
	root.PostJSON(path+"/enable", map[string]any{"enabled": false, "confirm": true}, &en)
	if en.Enabled {
		t.Error("应已停用")
	}
	if code, _ := root.JSON("POST", path+"/enable", map[string]any{"enabled": true}, nil); code != 400 {
		t.Errorf("启停未确认: %d", code)
	}
	// 删除
	if code, _ := root.JSON("POST", path+"/delete", map[string]any{}, nil); code != 400 {
		t.Errorf("删除未确认: %d", code)
	}
	root.PostJSON(path+"/delete", map[string]any{"confirm": true}, nil)
	if code, _ := root.JSON("POST", path+"/delete", map[string]any{"confirm": true}, nil); code != 404 {
		t.Errorf("重复删除: %d", code)
	}
	if code, _ := root.JSON("PUT", "/api/v1/settings/redis-rules/abc", with(goodRule, "confirm", true), nil); code != 400 {
		t.Errorf("编号不合法: %d", code)
	}
	if got := strings.Join(h.au.actions(), ","); got != "Redis上线规则-新增,Redis上线规则-修改,Redis上线规则-停用,Redis上线规则-删除" {
		t.Errorf("审计: %s", got)
	}
}

func TestRuleValidationErrors(t *testing.T) {
	h := newEnv(t)
	root := h.Login("root")
	bad := map[string]map[string]any{
		"硬禁止 FLUSHALL": with(goodRule, "commands", []string{"FLUSHALL"}),
		"硬禁止 CONFIG":   with(goodRule, "commands", []string{"DEL", "config"}),
		"硬禁止 EVAL":     with(goodRule, "commands", []string{"EVAL"}),
		"无键命令 PUBLISH": with(goodRule, "commands", []string{"PUBLISH"}),
		"读命令 GET":      with(goodRule, "commands", []string{"GET"}),
		"没有命令":         with(goodRule, "commands", []string{}),
		"没有键模式":        with(goodRule, "key_patterns", []string{}),
		"纯通配符":         with(goodRule, "key_patterns", []string{"*"}),
		"键数上限超限":       with(goodRule, "max_keys", 10001),
		"db 超范围":       with(goodRule, "dbs", []int{2000}),
		"名称为空":         with(goodRule, "name", " "),
	}
	for name, body := range bad {
		code, msg := root.JSON("POST", "/api/v1/settings/redis-rules", with(body, "confirm", true), nil)
		if code != 400 {
			t.Errorf("%s 应是 400: %d %s", name, code, msg)
		}
		if strings.HasPrefix(name, "硬禁止") && !strings.Contains(msg, "硬禁止") {
			t.Errorf("%s 的提示应说明是硬禁止: %s", name, msg)
		}
	}
	if n, _ := h.repo.List(); len(n) != 0 || len(h.au.actions()) != 0 {
		t.Error("校验失败不应落库、不写审计")
	}
}

func TestTableMissing(t *testing.T) {
	h := newEnv(t)
	h.repo.NotReady = true
	root := h.Login("root")
	var list struct {
		Ready bool             `json:"table_ready"`
		Rules []redisrule.Rule `json:"rules"`
	}
	root.GetJSON("/api/v1/settings/redis-rules", &list) // 不是 500
	if list.Ready || len(list.Rules) != 0 {
		t.Errorf("表不存在: %+v", list)
	}
	if code, msg := root.JSON("POST", "/api/v1/settings/redis-rules", with(goodRule, "confirm", true), nil); code != 503 || !strings.Contains(msg, "170_redis_rules.sql") {
		t.Errorf("表不存在时新增: %d %s", code, msg)
	}
	if code, _ := root.Page("/settings/redis-rules"); code != 200 {
		t.Errorf("页面仍应能打开: %d", code)
	}
}

func TestRuleTestEndpoint(t *testing.T) {
	h := newEnv(t)
	root := h.Login("root")
	root.PostJSON("/api/v1/settings/redis-rules", with(goodRule, "confirm", true), nil)
	var out []struct {
		Line    int    `json:"line"`
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	root.PostJSON("/api/v1/settings/redis-rules/test", map[string]any{"instance_id": 1, "db": 0, "text": "DEL cache:a\nDEL other:x\nFLUSHALL"}, &out)
	if len(out) != 3 || out[0].Status != "通过" || out[1].Status != "拒绝" || out[2].Status != "拒绝" || !strings.Contains(out[2].Message, "硬禁止") {
		t.Errorf("测试结果: %+v", out)
	}
	if code, _ := root.JSON("POST", "/api/v1/settings/redis-rules/test", map[string]any{"db": 5000, "text": "DEL a"}, nil); code != 400 {
		t.Errorf("db 超范围: %d", code)
	}
	if code, _ := root.JSON("POST", "/api/v1/settings/redis-rules/test", map[string]any{"text": " "}, nil); code != 400 {
		t.Errorf("空命令: %d", code)
	}
	if len(h.au.actions()) != 1 {
		t.Errorf("测试不写审计，只有创建那一条: %v", h.au.actions())
	}
}

func itoa(n uint) string {
	const d = "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{d[n%10]}, b...)
	}
	return string(b)
}
