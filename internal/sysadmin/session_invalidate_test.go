// @author: kevinkelly
// @license: Apache Licence
// @file: session_invalidate_test.go
// @time: 2026/10/28

package sysadmin

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"dbtool/internal/config"
	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// liveRepo 在 servertest 的内存仓库上实现 server.UserStatusSource：用户状态读 sysadmin 的 fakeRepo（即“库”）。
type liveRepo struct {
	*servertest.Repo
	f *fakeRepo
}

func (r liveRepo) UserStatus(id uint) (server.UserStatus, error) {
	u, ok := r.f.users[id]
	if !ok {
		return server.UserStatus{}, nil
	}
	return server.UserStatus{Exists: true, Active: u.IsActive, Superuser: u.IsSuperuser}, nil
}

// newLiveEnv 启用会话撤销校验（带 10 秒缓存）。用户 ID：1 root（超级）、2 bob（servertest）= fake 里的 alice、5 adm（超级用户）。
func newLiveEnv(t *testing.T) (*servertest.Env, *fakeRepo) {
	t.Helper()
	svc, f := setup()
	f.addUser(5, "adm", true, true)
	base := servertest.New(t, servertest.Options{})
	base.Repo.Users["adm"] = &model.User{ID: 5, Username: "adm", Password: base.Repo.Users["bob"].Password, IsActive: true, IsSuperuser: true}
	probe := func(s *server.Server, rt server.Routes) {
		rt.API.GET("/t/ping", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
		rt.API.GET("/t/super", s.SuperuserAPI(), func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	}
	h, err := server.New(&config.Config{SecretKey: "unit-test-secret-key-unit-test-secret-key"}, liveRepo{base.Repo, f}, server.Options{Hooks: []func(*server.Server, server.Routes){probe, registerWith(svc)}})
	if err != nil {
		t.Fatal(err)
	}
	return &servertest.Env{T: t, H: h, Repo: base.Repo}, f
}

func TestDisableUserRevokesSessionImmediately(t *testing.T) {
	env, f := newLiveEnv(t)
	root, bob := env.Login("root"), env.Login("bob")
	if code, _, _ := call(t, bob, "GET", "/api/v1/t/ping", nil); code != http.StatusOK {
		t.Fatalf("停用前应放行: %d", code)
	}
	if code, _, body := call(t, root, "POST", "/api/v1/settings/users/2/disable", nil); code != 200 {
		t.Fatalf("停用: %d %s", code, body)
	}
	if f.users[2].IsActive {
		t.Fatal("库里应已停用")
	}
	if code, _, _ := call(t, bob, "GET", "/api/v1/t/ping", nil); code != http.StatusUnauthorized {
		t.Errorf("停用后下一次请求应立即 401（不应等缓存过期）: %d", code)
	}
	// 重新启用后立即恢复
	if code, _, body := call(t, root, "POST", "/api/v1/settings/users/2/enable", nil); code != 200 {
		t.Fatalf("启用: %d %s", code, body)
	}
	if code, _, _ := call(t, bob, "GET", "/api/v1/t/ping", nil); code != http.StatusOK {
		t.Errorf("启用后应立即恢复: %d", code)
	}
}

func TestUpdateUserRevokesSessionImmediately(t *testing.T) {
	env, f := newLiveEnv(t)
	root, adm, bob := env.Login("root"), env.Login("adm"), env.Login("bob")
	if code, _, _ := call(t, adm, "GET", "/api/v1/t/super", nil); code != http.StatusOK {
		t.Fatalf("超级用户应放行: %d", code)
	}
	// 取消 adm 的超级用户标记（经编辑接口）
	if code, _, body := call(t, root, "PUT", "/api/v1/settings/users/5", map[string]any{"display": "管理", "is_active": true, "is_superuser": false}); code != 200 {
		t.Fatalf("降级: %d %s", code, body)
	}
	if f.users[5].IsSuperuser {
		t.Fatal("库里应已取消超级用户")
	}
	if code, _, _ := call(t, adm, "GET", "/api/v1/t/super", nil); code != http.StatusForbidden {
		t.Errorf("取消超级用户后下一次请求应立即降权（403）: %d", code)
	}
	// 经编辑接口停用 bob
	if code, _, _ := call(t, bob, "GET", "/api/v1/t/ping", nil); code != http.StatusOK {
		t.Fatalf("停用前应放行: %d", code)
	}
	if code, _, body := call(t, root, "PUT", "/api/v1/settings/users/2", map[string]any{"display": "鲍勃", "is_active": false}); code != 200 {
		t.Fatalf("编辑停用: %d %s", code, body)
	}
	if code, _, _ := call(t, bob, "GET", "/api/v1/t/ping", nil); code != http.StatusUnauthorized {
		t.Errorf("经编辑接口停用后应立即 401: %d", code)
	}
}
