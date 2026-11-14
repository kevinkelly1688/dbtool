// @author: kevinkelly
// @license: Apache Licence
// @file: userstatus_test.go
// @time: 2026/10/28

package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/config"
	"dbtool/internal/crypto"
	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// statusRepo 在 fakeRepo 上实现 UserStatusSource：按 ID 读 users，可注入故障，并统计读库次数。
type statusRepo struct {
	*fakeRepo
	mu    sync.Mutex
	fail  error
	calls atomic.Int32
	delay time.Duration
}

func (r *statusRepo) UserStatus(id uint) (UserStatus, error) {
	r.calls.Add(1)
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return UserStatus{}, r.fail
	}
	for _, u := range r.users {
		if u.ID == id {
			return UserStatus{Exists: true, Active: u.IsActive, Superuser: u.IsSuperuser}, nil
		}
	}
	return UserStatus{}, nil
}

func (r *statusRepo) set(name string, f func(u *model.User)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(r.users[name])
}

func (r *statusRepo) setFail(err error) { r.mu.Lock(); r.fail = err; r.mu.Unlock() }

type statusEnv struct {
	*env
	repo2 *statusRepo
	now   time.Time
}

// newStatusEnv 带 /api/v1/t/who 与 /t/page 两条测试路由和可控时钟。
func newStatusEnv(t *testing.T) *statusEnv {
	t.Helper()
	hash, err := crypto.MakeDjangoPassword("pw-123", "salty", 1000)
	if err != nil {
		t.Fatal(err)
	}
	repo := &statusRepo{fakeRepo: &fakeRepo{users: map[string]*model.User{
		"root": {ID: 1, Username: "root", Password: hash, IsActive: true, IsSuperuser: true, Display: "管理员"},
		"bob":  {ID: 2, Username: "bob", Password: hash, IsActive: true},
	}}}
	cfg := &config.Config{SecretKey: "unit-test-secret-key-unit-test-secret-key"}
	hook := func(s *Server, rt Routes) {
		rt.API.GET("/t/who", func(c *gin.Context) { c.JSON(200, gin.H{"su": principalOf(c).IsSuperuser, "uid": principalOf(c).ID}) })
		rt.Pages.GET("/t/page", func(c *gin.Context) { c.String(200, "page-ok") })
	}
	r, err := New(cfg, repo, Options{Hooks: []func(*Server, Routes){hook},
		Opener: func(string, engine.Conn) (engine.Engine, error) { return &fakeEngine{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	se := &statusEnv{env: &env{h: r, repo: repo.fakeRepo}, repo2: repo, now: time.Now()}
	statusClock = func() time.Time { return se.now }
	t.Cleanup(func() { statusClock = time.Now })
	return se
}

func (se *statusEnv) get(sc *http.Cookie, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(sc)
	return se.do(req)
}

func (se *statusEnv) bearer(t *testing.T, user string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"username": user, "password": "pw-123"})
	w := se.do(httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewReader(b)))
	var tok struct{ Token string }
	_ = json.Unmarshal(w.Body.Bytes(), &tok)
	if tok.Token == "" {
		t.Fatalf("JWT 登录失败: %d %s", w.Code, w.Body.String())
	}
	return tok.Token
}

func (se *statusEnv) bearerGet(tok, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	return se.do(req)
}

func TestSessionRevokedWhenUserDeactivated(t *testing.T) {
	se := newStatusEnv(t)
	sc, _, _ := se.login(t, "bob", "pw-123")
	if w := se.get(sc, "/api/v1/t/who"); w.Code != 200 {
		t.Fatalf("启用时应放行: %d", w.Code)
	}
	if w := se.get(sc, "/t/page"); w.Code != 200 {
		t.Fatalf("页面应放行: %d", w.Code)
	}
	se.repo2.set("bob", func(u *model.User) { u.IsActive = false })
	se.now = se.now.Add(userStatusTTL + time.Second) // 缓存过期后的下一次请求
	w := se.get(sc, "/api/v1/t/who")
	if w.Code != 401 {
		t.Fatalf("停用后下一次请求应 401: %d %s", w.Code, w.Body.String())
	}
	if c := cookieNamed(w, sessionCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("应清掉会话 Cookie: %+v", c)
	}
	// 页面：按未登录处理（跳登录页），/ui/ 片段 401
	w = se.get(sc, "/t/page")
	if w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), "/login?next=") {
		t.Errorf("页面应跳登录: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := se.get(sc, "/ui/query/tree"); w.Code != 401 {
		t.Errorf("片段请求应 401: %d", w.Code)
	}
}

func TestSessionRevokedWhenUserDeleted(t *testing.T) {
	se := newStatusEnv(t)
	sc, _, _ := se.login(t, "bob", "pw-123")
	se.repo2.mu.Lock()
	delete(se.repo2.users, "bob")
	se.repo2.mu.Unlock()
	se.now = se.now.Add(userStatusTTL + time.Second)
	if w := se.get(sc, "/api/v1/t/who"); w.Code != 401 {
		t.Errorf("用户已删除应 401: %d", w.Code)
	}
}

func TestJWTRevokedWhenUserDeactivated(t *testing.T) {
	se := newStatusEnv(t)
	tok := se.bearer(t, "bob")
	if w := se.bearerGet(tok, "/api/v1/t/who"); w.Code != 200 {
		t.Fatalf("启用时应放行: %d", w.Code)
	}
	se.repo2.set("bob", func(u *model.User) { u.IsActive = false })
	se.now = se.now.Add(userStatusTTL + time.Second)
	if w := se.bearerGet(tok, "/api/v1/t/who"); w.Code != 401 {
		t.Errorf("停用后 JWT 应 401: %d", w.Code)
	}
}

func TestSuperuserFlagFollowsDatabase(t *testing.T) {
	se := newStatusEnv(t)
	sc, _, _ := se.login(t, "root", "pw-123")
	tok := se.bearer(t, "root")
	su := func(w *httptest.ResponseRecorder) bool {
		var r struct{ Su bool }
		_ = json.Unmarshal(w.Body.Bytes(), &r)
		return r.Su
	}
	if !su(se.get(sc, "/api/v1/t/who")) || !su(se.bearerGet(tok, "/api/v1/t/who")) {
		t.Fatal("初始应是超级用户")
	}
	se.repo2.set("root", func(u *model.User) { u.IsSuperuser = false })
	se.now = se.now.Add(userStatusTTL + time.Second)
	if su(se.get(sc, "/api/v1/t/who")) || su(se.bearerGet(tok, "/api/v1/t/who")) {
		t.Error("取消超级用户后应立即失去超级用户身份（会话与 JWT 都是）")
	}
	// 反向：普通用户被授予超级用户后，旧会话也以库为准
	sc2, _, _ := se.login(t, "bob", "pw-123")
	se.repo2.set("bob", func(u *model.User) { u.IsSuperuser = true })
	se.now = se.now.Add(userStatusTTL + time.Second)
	if !su(se.get(sc2, "/api/v1/t/who")) {
		t.Error("库里最新值应覆盖会话里的旧值")
	}
}

func TestStatusCacheTTL(t *testing.T) {
	se := newStatusEnv(t)
	if userStatusTTL > 15*time.Second {
		t.Fatalf("TTL 必须 ≤ 15 秒: %v", userStatusTTL)
	}
	sc, _, _ := se.login(t, "bob", "pw-123")
	se.get(sc, "/api/v1/t/who")
	n := se.repo2.calls.Load()
	se.repo2.set("bob", func(u *model.User) { u.IsActive = false })
	// TTL 内：沿用缓存，不查库，仍放行
	se.now = se.now.Add(userStatusTTL - time.Second)
	for i := 0; i < 5; i++ {
		if w := se.get(sc, "/api/v1/t/who"); w.Code != 200 {
			t.Fatalf("TTL 内应沿用缓存: %d", w.Code)
		}
	}
	if got := se.repo2.calls.Load(); got != n {
		t.Errorf("TTL 内不应再查库: %d -> %d", n, got)
	}
	// TTL 外：重新读库
	se.now = se.now.Add(2 * time.Second)
	if w := se.get(sc, "/api/v1/t/who"); w.Code != 401 {
		t.Errorf("TTL 过后应读到停用: %d", w.Code)
	}
}

func TestInvalidateUserStatus(t *testing.T) {
	hash, _ := crypto.MakeDjangoPassword("pw-123", "salty", 1000)
	repo := &statusRepo{fakeRepo: &fakeRepo{users: map[string]*model.User{
		"bob": {ID: 2, Username: "bob", Password: hash, IsActive: true},
	}}}
	var srv *Server
	hook := func(s *Server, rt Routes) { srv = s }
	h, err := New(&config.Config{SecretKey: "unit-test-secret-key-unit-test-secret-key"}, repo, Options{Hooks: []func(*Server, Routes){hook}})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{h: h, repo: repo.fakeRepo}
	sc, _, _ := e.login(t, "bob", "pw-123")
	req := httptest.NewRequest("GET", "/api/v1/query/instances", nil)
	req.AddCookie(sc)
	if w := e.do(req); w.Code != 200 {
		t.Fatalf("启用时应放行: %d", w.Code)
	}
	repo.set("bob", func(u *model.User) { u.IsActive = false })
	req = httptest.NewRequest("GET", "/api/v1/query/instances", nil)
	req.AddCookie(sc)
	if w := e.do(req); w.Code != 200 {
		t.Fatalf("缓存内仍放行: %d", w.Code)
	}
	srv.InvalidateUserStatus(2)
	req = httptest.NewRequest("GET", "/api/v1/query/instances", nil)
	req.AddCookie(sc)
	if w := e.do(req); w.Code != 401 {
		t.Errorf("失效后应立即读到停用: %d", w.Code)
	}
}

func TestDatabaseOutageDoesNotKickUsers(t *testing.T) {
	se := newStatusEnv(t)
	sc, _, _ := se.login(t, "bob", "pw-123")
	tok := se.bearer(t, "bob")
	se.get(sc, "/api/v1/t/who") // 缓存一份
	se.repo2.setFail(errors.New("db down"))
	se.now = se.now.Add(userStatusTTL + time.Second)
	for i := 0; i < 3; i++ {
		if w := se.get(sc, "/api/v1/t/who"); w.Code != 200 {
			t.Fatalf("库不可用时应沿用缓存（过期也用）: %d", w.Code)
		}
	}
	// 失败后在退避期内不反复打库
	n := se.repo2.calls.Load()
	for i := 0; i < 10; i++ {
		se.get(sc, "/api/v1/t/who")
	}
	if got := se.repo2.calls.Load(); got != n {
		t.Errorf("退避期内不应再查库: %d -> %d", n, got)
	}
	// 从未缓存过的用户：沿用会话 / JWT 里的值
	hash, _ := crypto.MakeDjangoPassword("pw-123", "salty", 1000)
	se.repo2.mu.Lock()
	se.repo2.users["carol"] = &model.User{ID: 7, Username: "carol", Password: hash, IsActive: true}
	se.repo2.mu.Unlock()
	se.repo2.setFail(nil)
	scC, _, _ := se.login(t, "carol", "pw-123")
	se.repo2.setFail(errors.New("db down"))
	se.now = se.now.Add(time.Hour)
	if w := se.get(scC, "/api/v1/t/who"); w.Code != 200 {
		t.Errorf("库故障时应沿用会话值: %d", w.Code)
	}
	if w := se.bearerGet(tok, "/api/v1/t/who"); w.Code != 200 {
		t.Errorf("JWT 同理: %d", w.Code)
	}
	// 恢复后重新生效
	se.repo2.setFail(nil)
	se.repo2.set("bob", func(u *model.User) { u.IsActive = false })
	se.now = se.now.Add(userStatusTTL + userStatusBackoff + time.Second)
	if w := se.get(sc, "/api/v1/t/who"); w.Code != 401 {
		t.Errorf("库恢复后应读到停用: %d", w.Code)
	}
}

func TestStatusCacheConcurrent(t *testing.T) {
	se := newStatusEnv(t)
	sc, _, _ := se.login(t, "bob", "pw-123")
	se.now = se.now.Add(time.Hour)
	se.repo2.delay = 30 * time.Millisecond
	base := se.repo2.calls.Load()
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := se.get(sc, "/api/v1/t/who"); w.Code != 200 {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Errorf("并发请求失败 %d 个", bad.Load())
	}
	if got := se.repo2.calls.Load() - base; got != 1 {
		t.Errorf("同一用户并发未命中应只查一次库: %d", got)
	}
}

// 没有状态来源的仓库（测试 / 演示）：不做校验，行为与之前一致。
func TestNoStatusSourceKeepsLegacyBehavior(t *testing.T) {
	e := newEnv(t)
	sc, _, _ := e.login(t, "bob", "pw-123")
	e.repo.users["bob"].IsActive = false
	req := httptest.NewRequest("GET", "/api/v1/query/instances", nil)
	req.AddCookie(sc)
	if w := e.do(req); w.Code != 200 {
		t.Errorf("无状态来源时不校验: %d", w.Code)
	}
}
