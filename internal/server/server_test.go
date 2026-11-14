// @author: kevinkelly
// @license: Apache Licence
// @file: server_test.go
// @time: 2026/10/28

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"dbtool/internal/btnperm"
	"dbtool/internal/config"
	"dbtool/internal/crypto"
	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/query"
	"dbtool/internal/settings"
)

type fakeRepo struct {
	users map[string]*model.User
	logs  int
	rows  []model.QueryLog
	perms map[uint]map[string]bool
	inst  map[uint]map[uint]bool
	privs []model.QueryPrivilege
}

func (r *fakeRepo) ListInstances() ([]model.Instance, error) {
	return []model.Instance{{ID: 1, InstanceName: "prod-mysql-01", DBType: "mysql", Type: "master",
		Tags: []model.InstanceTag{{TagCode: "prod", TagName: "生产", Active: true}}}}, nil
}
func (r *fakeRepo) GetInstance(id uint) (*model.Instance, error) {
	l, _ := r.ListInstances()
	if id == 1 {
		return &l[0], nil
	}
	return nil, gorm.ErrRecordNotFound
}
func (r *fakeRepo) EngineConn(*model.Instance) engine.Conn { return engine.Conn{} }
func (r *fakeRepo) ConfigValue(k string) (string, bool) {
	return map[string]string{settings.KeyQueryForbidSelectStar: "true"}[k], k == settings.KeyQueryForbidSelectStar
}
func (r *fakeRepo) InsertQueryLog(l *model.QueryLog) error {
	r.logs++
	l.ID = uint(r.logs)
	r.rows = append(r.rows, *l)
	return nil
}

// 执行历史：内存实现（语义同 store：用户精确匹配、关键词都包含且不区分大小写、新的在前）。
func (r *fakeRepo) ListQueryLogs(f query.QueryLogFilter) ([]model.QueryLog, int64, error) {
	var out []model.QueryLog
	for i := len(r.rows) - 1; i >= 0; i-- {
		l := r.rows[i]
		if f.Username != "" && l.Username != f.Username {
			continue
		}
		if f.UserLike != "" && !strings.Contains(strings.ToLower(l.Username), strings.ToLower(f.UserLike)) &&
			!strings.Contains(strings.ToLower(l.UserDisplay), strings.ToLower(f.UserLike)) {
			continue
		}
		hit := true
		for _, k := range f.Keywords {
			hit = hit && strings.Contains(strings.ToLower(l.SQLLog), strings.ToLower(k))
		}
		if hit {
			out = append(out, l)
		}
	}
	return out, int64(len(out)), nil
}
func (r *fakeRepo) QueryLogUsers() ([]model.QueryLogUser, error) {
	seen := map[string]int64{}
	for _, l := range r.rows {
		seen[l.Username]++
	}
	var out []model.QueryLogUser
	for u, n := range seen {
		out = append(out, model.QueryLogUser{Username: u, Count: n})
	}
	return out, nil
}
func (r *fakeRepo) ActiveMaskingColumns(uint) ([]model.DataMaskingColumn, error) { return nil, nil }
func (r *fakeRepo) HasButton(uid uint, code string) (bool, error) {
	// perms 里写按钮码；基础按钮（登记表里标了 Open）对所有人开放
	return r.perms[uid][code] || btnperm.IsOpen(code), nil
}
func (r *fakeRepo) UserInstanceIDs(uid uint) (map[uint]bool, error) { return r.inst[uid], nil }
func (r *fakeRepo) ValidQueryPrivileges(user string, inst uint) ([]model.QueryPrivilege, error) {
	return r.privs, nil
}
func (r *fakeRepo) GetUserByName(n string) (*model.User, error) {
	if u, ok := r.users[n]; ok {
		return u, nil
	}
	return nil, gorm.ErrRecordNotFound
}

type fakeEngine struct{ sql string }

func (e *fakeEngine) Name() string                                  { return "mysql" }
func (e *fakeEngine) TestConnection(context.Context) error          { return nil }
func (e *fakeEngine) ServerVersion(context.Context) (string, error) { return "8.0", nil }
func (e *fakeEngine) AllDatabases(context.Context) ([]string, error) {
	return []string{"shop", "<b>x</b>"}, nil
}
func (e *fakeEngine) AllTables(context.Context, string) ([]string, error) {
	return []string{"orders", "users"}, nil
}
func (e *fakeEngine) AllColumns(context.Context, string, string) ([]string, error) {
	return []string{"id", "name"}, nil
}
func (e *fakeEngine) Close() error { return nil }
func (e *fakeEngine) Query(_ context.Context, sql string, _ engine.QueryOptions) (*engine.ResultSet, error) {
	e.sql = sql
	return &engine.ResultSet{ColumnList: []string{"id"}, Rows: [][]any{{1}, {2}}}, nil
}

type env struct {
	h    http.Handler
	eng  *fakeEngine
	repo *fakeRepo
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvCfg(t, &config.Config{SecretKey: "unit-test-secret-key-unit-test-secret-key"})
}

// newEnvCfg 同 newEnv，但配置由调用方给（SecretKey 须仍是 newEnv 用的那个，login 要用它解会话）。
func newEnvCfg(t *testing.T, cfg *config.Config) *env {
	t.Helper()
	hash, err := crypto.MakeDjangoPassword("pw-123", "salty", 1000)
	if err != nil {
		t.Fatal(err)
	}
	repo := &fakeRepo{users: map[string]*model.User{
		"root": {ID: 1, Username: "root", Password: hash, IsActive: true, IsSuperuser: true, Display: "管理员"},
		"bob":  {ID: 2, Username: "bob", Password: hash, IsActive: true},
		"off":  {ID: 3, Username: "off", Password: hash, IsActive: false},
	}}
	eng := &fakeEngine{}
	r, err := New(cfg, repo, Options{Opener: func(string, engine.Conn) (engine.Engine, error) { return eng, nil }})
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	return &env{h: r, eng: eng, repo: repo}
}

func (e *env) do(req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return w
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// login 走一遍页面登录流程，返回会话 Cookie 与其中的 CSRF token。
func (e *env) login(t *testing.T, user, pw string) (*http.Cookie, string, *httptest.ResponseRecorder) {
	t.Helper()
	w := e.do(httptest.NewRequest("GET", "/login", nil))
	if w.Code != 200 {
		t.Fatalf("GET /login=%d", w.Code)
	}
	lc := cookieNamed(w, loginCSRFCookie)
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if lc == nil || m == nil {
		t.Fatal("登录页缺少 CSRF")
	}
	form := url.Values{"username": {user}, "password": {pw}, "csrf": {m[1]}, "next": {"/query"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(lc)
	w = e.do(req)
	sc := cookieNamed(w, sessionCookie)
	if sc == nil {
		return nil, "", w
	}
	sess, err := decodeSession([]byte("unit-test-secret-key-unit-test-secret-key"), sc.Value, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return sc, sess.CSRF, w
}

func TestLoginFlowAndPages(t *testing.T) {
	e := newEnv(t)
	// 未登录访问页面 → 跳登录并带 next
	w := e.do(httptest.NewRequest("GET", "/query", nil))
	if w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), "/login?next=") {
		t.Fatalf("未登录应跳转: %d %s", w.Code, w.Header().Get("Location"))
	}
	// 错误密码
	if sc, _, w := e.login(t, "root", "bad"); sc != nil || w.Code != 401 {
		t.Errorf("错误密码应 401: %d", w.Code)
	}
	// 停用账号
	if sc, _, _ := e.login(t, "off", "pw-123"); sc != nil {
		t.Error("停用账号不应登录成功")
	}
	// 成功登录
	sc, csrf, w := e.login(t, "root", "pw-123")
	if sc == nil || w.Code != 303 || w.Header().Get("Location") != "/query" {
		t.Fatalf("登录失败: %d", w.Code)
	}
	if !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode {
		t.Error("会话 Cookie 需 HttpOnly + SameSite=Lax")
	}
	// 工作台页面渲染，含实例并带安全头
	req := httptest.NewRequest("GET", "/query", nil)
	req.AddCookie(sc)
	w = e.do(req)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "prod-mysql-01") || !strings.Contains(body, `content="`+csrf+`"`) {
		t.Fatalf("工作台渲染异常: %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("缺少安全响应头")
	}
}

func TestOpenRedirectBlocked(t *testing.T) {
	for in, want := range map[string]string{"/query": "/query", "//evil.com": "/", "https://evil.com": "/", "/\\evil": "/", "/a?b=1": "/a?b=1"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestLoginCSRF(t *testing.T) {
	e := newEnv(t)
	form := url.Values{"username": {"root"}, "password": {"pw-123"}, "csrf": {"forged"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if w := e.do(req); w.Code != 403 || cookieNamed(w, sessionCookie) != nil {
		t.Errorf("缺少登录 CSRF 应被拒绝: %d", w.Code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		e.login(t, "root", "bad")
	}
	if sc, _, w := e.login(t, "root", "pw-123"); sc != nil || w.Code != 429 {
		t.Errorf("连续失败后应限流: %d", w.Code)
	}
}

func TestExecuteAPI(t *testing.T) {
	e := newEnv(t)
	sc, csrf, _ := e.login(t, "root", "pw-123")
	post := func(csrfHdr, sql string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"instance_id": 1, "db": "shop", "sql": sql, "limit": 10})
		req := httptest.NewRequest("POST", "/api/v1/query/execute", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(sc)
		if csrfHdr != "" {
			req.Header.Set("X-CSRF-Token", csrfHdr)
		}
		return e.do(req)
	}
	if w := post("", "select id from orders"); w.Code != 403 {
		t.Errorf("缺少 CSRF 头应 403: %d", w.Code)
	}
	if w := post("wrong", "select id from orders"); w.Code != 403 {
		t.Errorf("错误 CSRF 头应 403: %d", w.Code)
	}
	w := post(csrf, "select id from orders")
	var out struct {
		Data struct {
			RowCount    int    `json:"row_count"`
			ExecutedSQL string `json:"executed_sql"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || out.Data.RowCount != 2 || e.eng.sql != "select id from orders LIMIT 10" {
		t.Errorf("执行失败: %d %s sql=%q", w.Code, w.Body.String(), e.eng.sql)
	}
	// 规则拦截的错误码
	w = post(csrf, "select * from orders")
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"star_forbidden"`) {
		t.Errorf("SELECT * 应被拦截: %d %s", w.Code, w.Body.String())
	}
	w = post(csrf, "drop table orders")
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"bad_query"`) {
		t.Errorf("DDL 应被拦截: %d %s", w.Code, w.Body.String())
	}
}

func TestNonAdminForbidden(t *testing.T) {
	e := newEnv(t)
	sc, csrf, _ := e.login(t, "bob", "pw-123")
	b, _ := json.Marshal(map[string]any{"instance_id": 1, "sql": "select 1"})
	req := httptest.NewRequest("POST", "/api/v1/query/execute", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(sc)
	if w := e.do(req); w.Code != 403 || !strings.Contains(w.Body.String(), "no_privilege") {
		t.Errorf("普通用户应 403: %d %s", w.Code, w.Body.String())
	}
}

func TestBearerAndUnauthorized(t *testing.T) {
	e := newEnv(t)
	if w := e.do(httptest.NewRequest("GET", "/api/v1/query/instances", nil)); w.Code != 401 {
		t.Errorf("无凭据应 401: %d", w.Code)
	}
	b, _ := json.Marshal(map[string]string{"username": "root", "password": "pw-123"})
	w := e.do(httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewReader(b)))
	var tok struct{ Token string }
	_ = json.Unmarshal(w.Body.Bytes(), &tok)
	if w.Code != 200 || tok.Token == "" {
		t.Fatalf("JWT 登录失败: %d", w.Code)
	}
	req := httptest.NewRequest("GET", "/api/v1/query/instances", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	if w := e.do(req); w.Code != 200 || !strings.Contains(w.Body.String(), "prod-mysql-01") {
		t.Errorf("Bearer 访问失败: %d", w.Code)
	}
	req = httptest.NewRequest("GET", "/api/v1/query/instances", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token+"x")
	if w := e.do(req); w.Code != 401 {
		t.Errorf("篡改令牌应 401: %d", w.Code)
	}
}

func TestSessionTamperAndExpiry(t *testing.T) {
	key := []byte("k")
	v, _ := encodeSession(key, Session{UID: 1, Username: "a", Exp: time.Now().Add(time.Hour).Unix()})
	if _, err := decodeSession(key, v, time.Now()); err != nil {
		t.Error("有效会话解码失败")
	}
	if _, err := decodeSession([]byte("other"), v, time.Now()); err == nil {
		t.Error("不同密钥应校验失败")
	}
	p, sig, _ := strings.Cut(v, ".")
	if _, err := decodeSession(key, p+"A."+sig, time.Now()); err == nil {
		t.Error("篡改内容应校验失败")
	}
	if _, err := decodeSession(key, v, time.Now().Add(2*time.Hour)); err == nil {
		t.Error("过期会话应失效")
	}
}

func TestFragmentsEscapeHTML(t *testing.T) {
	e := newEnv(t)
	sc, _, _ := e.login(t, "root", "pw-123")
	req := httptest.NewRequest("GET", "/ui/query/databases?instance=1", nil)
	req.AddCookie(sc)
	w := e.do(req)
	if w.Code != 200 || strings.Contains(w.Body.String(), "<b>x</b>") || !strings.Contains(w.Body.String(), "&lt;b&gt;x&lt;/b&gt;") {
		t.Errorf("片段必须转义库名: %s", w.Body.String())
	}
	req = httptest.NewRequest("GET", "/ui/query/tree?instance=1&db=shop&q=ord", nil)
	req.AddCookie(sc)
	w = e.do(req)
	if !strings.Contains(w.Body.String(), `data-table="orders"`) || strings.Contains(w.Body.String(), `data-table="users"`) {
		t.Errorf("表搜索过滤异常: %s", w.Body.String())
	}
	req = httptest.NewRequest("GET", "/ui/query/policy?instance=1&db=shop", nil)
	req.AddCookie(sc)
	if w := e.do(req); !strings.Contains(w.Body.String(), "已禁用 SELECT *") || !strings.Contains(w.Body.String(), "管理员") {
		t.Errorf("策略片段异常: %s", w.Body.String())
	}
	// 未登录的片段请求返回 401 而不是跳转
	if w := e.do(httptest.NewRequest("GET", "/ui/query/tree?instance=1&db=shop", nil)); w.Code != 401 {
		t.Errorf("片段未登录应 401: %d", w.Code)
	}
}

func TestStaticAssets(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/static/css/app.css", "/static/js/query/app.js", "/static/vendor/ace/ace.js", "/static/img/icons.svg"} {
		if w := e.do(httptest.NewRequest("GET", p, nil)); w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("静态资源 %s: %d", p, w.Code)
		}
	}
}

func TestPageRequiresMenuPermission(t *testing.T) {
	e := newEnv(t)
	sc, _, _ := e.login(t, "bob", "pw-123")
	req := httptest.NewRequest("GET", "/query", nil)
	req.AddCookie(sc)
	if w := e.do(req); w.Code != 403 || !strings.Contains(w.Body.String(), "query.view") {
		t.Errorf("无 query.view 应 403: %d %s", w.Code, w.Body.String())
	}
}

func TestNonAdminWithPrivileges(t *testing.T) {
	e := newEnv(t)
	repo := e.repo
	repo.perms = map[uint]map[string]bool{2: {"query.view": true, "query.run": true}}
	repo.inst = map[uint]map[uint]bool{2: {1: true}}
	repo.privs = []model.QueryPrivilege{{UserName: "bob", InstanceID: 1, PrivType: 2, DBName: "shop", TbName: "orders", LimitNum: 1}}

	sc, csrf, _ := e.login(t, "bob", "pw-123")
	exec := func(sql string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"instance_id": 1, "db": "shop", "sql": sql})
		req := httptest.NewRequest("POST", "/api/v1/query/execute", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		req.AddCookie(sc)
		return e.do(req)
	}
	req := httptest.NewRequest("GET", "/query", nil)
	req.AddCookie(sc)
	if w := e.do(req); w.Code != 200 || !strings.Contains(w.Body.String(), "prod-mysql-01") {
		t.Fatalf("有 query.view 的用户应能打开页面: %d", w.Code)
	}
	if w := exec("select id from orders"); w.Code != 200 || e.eng.sql != "select id from orders LIMIT 1" {
		t.Errorf("有表权限应放行并限行: %d %s sql=%q", w.Code, w.Body.String(), e.eng.sql)
	}
	w := exec("select id from users")
	var out struct {
		Code string
		Data map[string]any
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 403 || out.Code != "no_privilege" || out.Data["database"] != "shop" || out.Data["table"] != "users" {
		t.Errorf("无权限应返回结构化错误: %d %s", w.Code, w.Body.String())
	}
}

func TestHistoryAPI(t *testing.T) {
	e := newEnv(t)
	e.repo.rows = []model.QueryLog{
		{ID: 1, Username: "bob", UserDisplay: "鲍勃", InstanceName: "prod-mysql-01", DBName: "shop", SQLLog: "select id from orders limit 10", EffectRow: 2, CostTime: "0.010"},
		{ID: 2, Username: "root", InstanceName: "prod-mysql-01", DBName: "shop", SQLLog: "select name from users limit 5", EffectRow: 1, CostTime: "0.020"},
		{ID: 3, Username: "bob", InstanceName: "prod-mysql-01", DBName: "shop", SQLLog: "select * from ORDERS_BAK limit 1", EffectRow: 0, CostTime: "0.030"},
	}
	e.repo.logs = 3
	get := func(sc *http.Cookie, path string) (int, map[string]any) {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(sc)
		w := e.do(req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	items := func(out map[string]any) []any { return out["data"].(map[string]any)["items"].([]any) }

	bob, _, _ := e.login(t, "bob", "pw-123")
	if code, out := get(bob, "/api/v1/query/history"); code != 200 || len(items(out)) != 2 || out["data"].(map[string]any)["admin"] != false {
		t.Fatalf("普通用户只看自己的: %d %v", code, out)
	}
	if _, out := get(bob, "/api/v1/query/history?user=root"); len(items(out)) != 2 {
		t.Errorf("普通用户传 user 无效: %v", out)
	}
	if _, out := get(bob, "/api/v1/query/history?q=orders"); len(items(out)) != 2 {
		t.Errorf("模糊搜索（不区分大小写）: %v", out)
	}
	first := items(func() map[string]any { _, o := get(bob, "/api/v1/query/history"); return o }())[0].(map[string]any)
	if first["sql"] != "select * from ORDERS_BAK limit 1" || first["username"] != "bob" || first["instance"] != "prod-mysql-01" {
		t.Errorf("记录字段与排序: %v", first)
	}
	if code, _ := get(bob, "/api/v1/query/history/users"); code != 403 {
		t.Errorf("普通用户不能取用户列表: %d", code)
	}
	if code, _ := get(bob, "/api/v1/query/history?start=bad"); code != 400 {
		t.Errorf("日期格式错误: %d", code)
	}

	root, _, _ := e.login(t, "root", "pw-123")
	if code, out := get(root, "/api/v1/query/history"); code != 200 || len(items(out)) != 3 || out["data"].(map[string]any)["admin"] != true {
		t.Errorf("管理员看所有人: %d %v", code, out)
	}
	if _, out := get(root, "/api/v1/query/history?user=bob&q=users"); len(items(out)) != 0 {
		t.Errorf("用户 + 关键词: %v", out)
	}
	if code, out := get(root, "/api/v1/query/history/users"); code != 200 || len(out["data"].([]any)) != 2 {
		t.Errorf("用户列表: %d %v", code, out)
	}
	if code, _ := get(nil2(), "/api/v1/query/history"); code != 401 && code != 302 {
		t.Errorf("未登录: %d", code)
	}
}

func nil2() *http.Cookie { return &http.Cookie{Name: sessionCookie, Value: "bad"} }
