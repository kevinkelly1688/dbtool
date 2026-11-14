// @author: kevinkelly
// @license: Apache Licence
// @file: servertest.go
// @time: 2026/10/28

// Package servertest 给功能模块（internal/<模块>）的 HTTP 测试用：内存仓库 + 登录辅助，不连任何数据库。
//
//	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){register}})
//	c := env.Login("root")            // 超级用户；"bob" 为普通用户（无任何权限，可用 env.Repo.Perms 授予）
//	var out []Item
//	c.GetJSON("/api/v1/...", &out)    // 非 200 时 t.Fatal；c.JSON / c.Do 可拿状态码与原始响应
package servertest

import (
	"bytes"
	"dbtool/internal/btnperm"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"dbtool/internal/config"
	"dbtool/internal/crypto"
	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/server"
)

// Repo 是内存仓库：用户与权限可在测试里直接改。
type Repo struct {
	Users     map[string]*model.User
	Perms     map[uint]map[string]bool // 用户 ID → 按钮码
	Instances []model.Instance
	Config    map[string]string
}

func (r *Repo) ListInstances() ([]model.Instance, error) { return r.Instances, nil }
func (r *Repo) GetInstance(id uint) (*model.Instance, error) {
	for i := range r.Instances {
		if r.Instances[i].ID == id {
			return &r.Instances[i], nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}
func (r *Repo) EngineConn(*model.Instance) engine.Conn { return engine.Conn{} }
func (r *Repo) ConfigValue(k string) (string, bool)    { v, ok := r.Config[k]; return v, ok }
func (r *Repo) InsertQueryLog(*model.QueryLog) error   { return nil }
func (r *Repo) ActiveMaskingColumns(uint) ([]model.DataMaskingColumn, error) {
	return nil, nil
}
func (r *Repo) HasButton(uid uint, code string) (bool, error) {
	// Perms 里写按钮码；基础按钮（登记表里标了 Open）对所有人开放
	return r.Perms[uid][code] || btnperm.IsOpen(code), nil
}
func (r *Repo) UserInstanceIDs(uint) (map[uint]bool, error) { return nil, nil }
func (r *Repo) ValidQueryPrivileges(string, uint) ([]model.QueryPrivilege, error) {
	return nil, nil
}
func (r *Repo) GetUserByName(n string) (*model.User, error) {
	if u, ok := r.Users[n]; ok {
		return u, nil
	}
	return nil, gorm.ErrRecordNotFound
}

// Options 与 server.Options 对应。
type Options struct {
	Hooks  []func(s *server.Server, rt server.Routes)
	Opener func(dbType string, c engine.Conn) (engine.Engine, error)
	Repo   *Repo // 为空则用默认仓库（root 超级用户、bob 普通用户，密码均为 Password）
}

type Env struct {
	T    *testing.T
	H    http.Handler
	Repo *Repo
}

// Password 是默认仓库里所有用户的密码。
const Password = "pw-123"

func New(t *testing.T, o Options) *Env {
	t.Helper()
	repo := o.Repo
	if repo == nil {
		hash, err := crypto.MakeDjangoPassword(Password, "salty", 1000)
		if err != nil {
			t.Fatal(err)
		}
		repo = &Repo{
			Users: map[string]*model.User{
				"root": {ID: 1, Username: "root", Password: hash, IsActive: true, IsSuperuser: true, Display: "管理员"},
				"bob":  {ID: 2, Username: "bob", Password: hash, IsActive: true, Display: "鲍勃"},
			},
			Perms: map[uint]map[string]bool{},
		}
	}
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{SecretKey: "unit-test-secret-key-unit-test-secret-key"}
	r, err := server.New(cfg, repo, server.Options{Hooks: o.Hooks, Opener: o.Opener})
	if err != nil {
		t.Fatal(err)
	}
	return &Env{T: t, H: r, Repo: repo}
}

// Client 带会话 Cookie 与 CSRF 令牌的测试客户端。
type Client struct {
	e      *Env
	cookie *http.Cookie
	CSRF   string
}

func (e *Env) do(req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.H.ServeHTTP(w, req)
	return w
}

// Anonymous 返回不带会话的响应（用来测“未登录”）。
func (e *Env) Anonymous(method, path string) *httptest.ResponseRecorder {
	return e.do(httptest.NewRequest(method, path, nil))
}

// Login 以 user（密码 Password）登录；失败直接 t.Fatal。
func (e *Env) Login(user string) *Client {
	e.T.Helper()
	w := e.do(httptest.NewRequest("GET", "/login", nil))
	var lc *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "dba_login_csrf" {
			lc = c
		}
	}
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if lc == nil || m == nil {
		e.T.Fatal("登录页缺少 CSRF")
	}
	form := url.Values{"username": {user}, "password": {Password}, "csrf": {m[1]}, "next": {"/query"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(lc)
	w = e.do(req)
	var sc *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "dba_session" {
			sc = c
		}
	}
	if sc == nil {
		e.T.Fatalf("登录失败: %d %s", w.Code, w.Body.String())
	}
	cl := &Client{e: e, cookie: sc}
	pg := cl.Do("GET", "/query", nil)
	mm := regexp.MustCompile(`name="csrf-token" content="([^"]+)"`).FindStringSubmatch(pg.Body.String())
	if mm == nil {
		e.T.Fatalf("取不到 CSRF 令牌: %d", pg.Code)
	}
	cl.CSRF = mm[1]
	return cl
}

// Do 发请求；body 为 nil、[]byte/string（原样）或其他（按 JSON 编码）。非 GET 自动带 CSRF 头。
func (c *Client) Do(method, path string, body any) *httptest.ResponseRecorder {
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		j, err := json.Marshal(b)
		if err != nil {
			c.e.T.Fatal(err)
		}
		rd = bytes.NewReader(j)
	}
	req := httptest.NewRequest(method, path, rd)
	req.AddCookie(c.cookie)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != "GET" && method != "HEAD" {
		req.Header.Set("X-CSRF-Token", c.CSRF)
	}
	return c.e.do(req)
}

// JSON 发请求并把响应里的 data 解码进 out（out 可为 nil）；返回 HTTP 状态码与响应里的 message。
func (c *Client) JSON(method, path string, body, out any) (int, string) {
	c.e.T.Helper()
	w := c.Do(method, path, body)
	var env struct {
		Data    json.RawMessage `json:"data"`
		Message string          `json:"message"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if out != nil && w.Code == 200 && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			c.e.T.Fatalf("解码 %s %s 的 data 失败: %v\n%s", method, path, err, w.Body.String())
		}
	}
	return w.Code, env.Message
}

// GetJSON / PostJSON：要求 200，否则 t.Fatal。
func (c *Client) GetJSON(path string, out any) {
	c.e.T.Helper()
	if code, msg := c.JSON("GET", path, nil, out); code != 200 {
		c.e.T.Fatalf("GET %s = %d %s", path, code, msg)
	}
}

func (c *Client) PostJSON(path string, body, out any) {
	c.e.T.Helper()
	if code, msg := c.JSON("POST", path, body, out); code != 200 {
		c.e.T.Fatalf("POST %s = %d %s", path, code, msg)
	}
}

// Page 取页面 HTML。
func (c *Client) Page(path string) (int, string) {
	w := c.Do("GET", path, nil)
	return w.Code, w.Body.String()
}
