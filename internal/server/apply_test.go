// @author: kevinkelly
// @license: Apache Licence
// @file: apply_test.go
// @time: 2026/10/28

package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/config"
	"dbtool/internal/crypto"
	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/privapply/memrepo"
	"dbtool/internal/server"
)

// 完整装配的服务：用 memrepo 提供权限申请所需的数据，复用 server_test.go 里的查询假实现。
type applyEnv struct {
	h    http.Handler
	repo *appRepo
}

type appRepo struct {
	*memrepo.Repo
	users map[string]*model.User
}

func (r *appRepo) GetInstance(id uint) (*model.Instance, error) {
	for i := range r.Instances {
		if r.Instances[i].ID == id {
			return &r.Instances[i], nil
		}
	}
	return nil, nil
}
func (r *appRepo) EngineConn(*model.Instance) engine.Conn { return engine.Conn{} }
func (r *appRepo) InsertQueryLog(l *model.QueryLog) error { return nil }
func (r *appRepo) ActiveMaskingColumns(uint) ([]model.DataMaskingColumn, error) {
	return nil, nil
}
func (r *appRepo) GetUserByName(n string) (*model.User, error) {
	if u, ok := r.users[n]; ok {
		return u, nil
	}
	return nil, nil
}

func newApplyEnv(t *testing.T) *applyEnv {
	t.Helper()
	hash, err := crypto.MakeDjangoPassword("pw-123", "salty", 1000)
	if err != nil {
		t.Fatal(err)
	}
	m := memrepo.New()
	tag := []model.InstanceTag{{TagCode: "can_read", Active: true}}
	m.Instances = []model.Instance{{ID: 1, InstanceName: "prod-mysql-01", DBType: "mysql", Tags: tag}}
	m.RGs = []model.ResourceGroup{{GroupID: 1, GroupName: "研发组"}}
	m.InstRGs = map[uint][]uint{1: {1}}
	m.UserRGs = map[uint][]uint{1: {1}, 2: {1}, 3: {1}}
	m.Groups = map[uint]*model.AuthGroup{10: {ID: 10, Name: "组长"}}
	m.Members = map[uint]map[uint]bool{2: {10: true}}
	m.Users = map[uint]model.User{1: {ID: 1, Username: "dev"}, 2: {ID: 2, Username: "leader"}, 3: {ID: 3, Username: "other"}}
	m.Perms = map[uint]map[string]bool{
		1: {"qpriv.view": true, "qpriv.list": true, "query.view": true, "query.run": true},
		2: {"qpriv.view": true, "qpriv.review": true, "qpriv.manage": true, "qpriv.list": true, "query.view": true},
		3: {"qpriv.view": true, "query.view": true},
	}
	m.Settings[[2]int{1, 1}] = &model.WorkflowAuditSetting{GroupID: 1, AuditAuthGroups: "10"}
	repo := &appRepo{Repo: m, users: map[string]*model.User{
		"dev":    {ID: 1, Username: "dev", Display: "开发者", Password: hash, IsActive: true},
		"leader": {ID: 2, Username: "leader", Display: "组长", Password: hash, IsActive: true},
		"other":  {ID: 3, Username: "other", Display: "路人", Password: hash, IsActive: true},
		"root":   {ID: 100, Username: "root", Display: "管理员", Password: hash, IsActive: true, IsSuperuser: true},
	}}
	cfg := &config.Config{SecretKey: "unit-test-secret-key-unit-test-secret-key", PublicURL: "http://dba.example.com"}
	r, err := server.New(cfg, repo, server.Options{Opener: func(string, engine.Conn) (engine.Engine, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	return &applyEnv{h: r, repo: repo}
}

type session struct {
	e      *applyEnv
	cookie *http.Cookie
	csrf   string
}

func (e *applyEnv) login(t *testing.T, user string) *session {
	t.Helper()
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, httptest.NewRequest("GET", "/login", nil))
	var lc *http.Cookie
	for _, c := range w.Result().Cookies() {
		lc = c
	}
	tok := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(w.Body.String())[1]
	form := url.Values{"username": {user}, "password": {"pw-123"}, "csrf": {tok}, "next": {"/query"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(lc)
	w = httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	var sc *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "dba_session" {
			sc = c
		}
	}
	if sc == nil {
		t.Fatalf("%s 登录失败: %d", user, w.Code)
	}
	req = httptest.NewRequest("GET", "/query", nil)
	req.AddCookie(sc)
	w = httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	m := regexp.MustCompile(`name="csrf-token" content="([^"]+)"`).FindStringSubmatch(w.Body.String())
	csrf := ""
	if m != nil {
		csrf = m[1]
	}
	return &session{e: e, cookie: sc, csrf: csrf}
}

func (s *session) do(method, path string, body any) *httptest.ResponseRecorder {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", s.csrf)
	req.AddCookie(s.cookie)
	w := httptest.NewRecorder()
	s.e.h.ServeHTTP(w, req)
	return w
}

func tomorrowStr() string { return time.Now().AddDate(0, 0, 1).Format("2006-01-02") }

func createBody() map[string]any {
	return map[string]any{"instance_id": 1, "group_name": "研发组", "priv_type": 2, "db_name": "shop", "table_list": []string{"orders"}, "valid_date": tomorrowStr(), "limit_num": 100}
}

func applyID(t *testing.T, w *httptest.ResponseRecorder) float64 {
	t.Helper()
	var out struct {
		Data struct {
			ApplyID float64 `json:"apply_id"`
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || out.Data.ApplyID == 0 {
		t.Fatalf("创建失败: %d %s", w.Code, w.Body.String())
	}
	return out.Data.ApplyID
}

func TestApplyHTTPFlow(t *testing.T) {
	e := newApplyEnv(t)
	dev, leader, other := e.login(t, "dev"), e.login(t, "leader"), e.login(t, "other")

	// 申请选项：只列出带 can_read 标签、用户能访问的实例，以及用户所在的资源组
	w := dev.do("GET", "/api/v1/query/apply/options", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "prod-mysql-01") || !strings.Contains(w.Body.String(), "研发组") {
		t.Errorf("选项: %d %s", w.Code, w.Body.String())
	}
	// 提交
	id := applyID(t, dev.do("POST", "/api/v1/query/privilege-applies", createBody()))
	// 缺少 CSRF 头 → 403
	req := httptest.NewRequest("POST", "/api/v1/query/privilege-applies", strings.NewReader(`{}`))
	req.AddCookie(dev.cookie)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("缺少 CSRF 应 403: %d", rec.Code)
	}
	// 校验错误：结构化错误码
	bad := createBody()
	bad["limit_num"] = 0
	if w := dev.do("POST", "/api/v1/query/privilege-applies", bad); w.Code != 400 || !strings.Contains(w.Body.String(), "bad_request") {
		t.Errorf("校验错误: %d %s", w.Code, w.Body.String())
	}

	detail := "/api/v1/query/privilege-applies/" + itoa(id)
	// 详情可见范围：申请人、同组审批人可见；无审批权限的人看不到（404）
	if w := dev.do("GET", detail, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"can_abort":true`) {
		t.Errorf("申请人详情: %d %s", w.Code, w.Body.String())
	}
	if w := leader.do("GET", detail, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"can_review":true`) {
		t.Errorf("审批人详情: %d %s", w.Code, w.Body.String())
	}
	if w := other.do("GET", detail, nil); w.Code != 404 {
		t.Errorf("路人看详情应 404: %d", w.Code)
	}
	// 页面
	pg := func(s *session, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(s.cookie)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec
	}
	if w := pg(leader, "/query/applies"); w.Code != 200 || !strings.Contains(w.Body.String(), "#"+itoa(id)) || !strings.Contains(w.Body.String(), "待审核") {
		t.Errorf("列表页: %d", w.Code)
	}
	if w := pg(leader, "/query/applies/"+itoa(id)); w.Code != 200 || !strings.Contains(w.Body.String(), "act-pass") || !strings.Contains(w.Body.String(), "shop") {
		t.Errorf("详情页（审批人应看到审批按钮）: %d", w.Code)
	}
	if w := pg(dev, "/query/applies/"+itoa(id)); !strings.Contains(w.Body.String(), "act-abort") || strings.Contains(w.Body.String(), "act-pass") {
		t.Error("详情页（申请人只能看到撤回按钮）")
	}
	if w := pg(other, "/query/applies/"+itoa(id)); w.Code != 404 {
		t.Errorf("路人详情页应 404: %d", w.Code)
	}
	if w := pg(dev, "/query/applies/abc"); w.Code != 404 {
		t.Errorf("非法编号: %d", w.Code)
	}

	// 审批：申请人不能通过；驳回必须有原因；审批人通过
	if w := dev.do("POST", detail+"/audit", map[string]string{"action": "pass"}); w.Code != 403 {
		t.Errorf("申请人审批应 403: %d %s", w.Code, w.Body.String())
	}
	if w := leader.do("POST", detail+"/audit", map[string]string{"action": "reject"}); w.Code != 400 {
		t.Errorf("驳回无原因应 400: %d", w.Code)
	}
	if w := leader.do("POST", detail+"/audit", map[string]string{"action": "bogus"}); w.Code != 400 {
		t.Errorf("未知动作应 400: %d", w.Code)
	}
	if w := leader.do("POST", detail+"/audit", map[string]string{"action": "pass", "remark": "ok"}); w.Code != 200 {
		t.Fatalf("审批通过: %d %s", w.Code, w.Body.String())
	}
	if len(e.repo.Privs) != 1 || e.repo.Privs[0].TbName != "orders" {
		t.Errorf("通过后应授权: %+v", e.repo.Privs)
	}
	if w := leader.do("POST", detail+"/audit", map[string]string{"action": "pass"}); w.Code != 409 {
		t.Errorf("重复审批应 409: %d %s", w.Code, w.Body.String())
	}

	// 权限管理：普通申请人只读；审批人（有 qpriv.manage）可修改/回收
	if w := dev.do("GET", "/api/v1/query/privileges", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"can_manage":false`) {
		t.Errorf("申请人权限列表: %d %s", w.Code, w.Body.String())
	}
	if w := dev.do("DELETE", "/api/v1/query/privileges/1", nil); w.Code != 403 {
		t.Errorf("申请人回收应 403: %d", w.Code)
	}
	if w := leader.do("PATCH", "/api/v1/query/privileges/1", map[string]any{"valid_date": tomorrowStr(), "limit_num": 7}); w.Code != 200 || e.repo.Privs[0].LimitNum != 7 {
		t.Errorf("修改: %d %s", w.Code, w.Body.String())
	}
	if w := pg(leader, "/query/privileges"); w.Code != 200 || !strings.Contains(w.Body.String(), "data-act=\"delete\"") {
		t.Errorf("管理页应有回收按钮: %d", w.Code)
	}
	if w := pg(dev, "/query/privileges"); strings.Contains(w.Body.String(), "data-act=\"delete\"") {
		t.Error("申请人页面不应有回收按钮")
	}
	if w := leader.do("DELETE", "/api/v1/query/privileges/1", nil); w.Code != 200 || e.repo.Privs[0].IsDeleted != 1 {
		t.Errorf("回收: %d", w.Code)
	}
}

func TestApplyAbortAndPagePermissions(t *testing.T) {
	e := newApplyEnv(t)
	dev, other, root := e.login(t, "dev"), e.login(t, "other"), e.login(t, "root")
	id := applyID(t, dev.do("POST", "/api/v1/query/privilege-applies", createBody()))
	detail := "/api/v1/query/privilege-applies/" + itoa(id)
	if w := other.do("POST", detail+"/abort", map[string]string{}); w.Code == 200 {
		t.Error("路人不能撤回")
	}
	if w := root.do("POST", detail+"/abort", map[string]string{}); w.Code != 403 {
		t.Errorf("超级用户也不能替别人撤回: %d", w.Code)
	}
	if w := dev.do("POST", detail+"/abort", map[string]string{"remark": "不需要了"}); w.Code != 200 {
		t.Fatalf("撤回: %d %s", w.Code, w.Body.String())
	}
	if w := dev.do("GET", detail, nil); !strings.Contains(w.Body.String(), `"status":3`) {
		t.Errorf("撤回后状态: %s", w.Body.String())
	}
	// 页面权限：没有 qpriv.list 的用户不能打开“查询权限”页，导航里也不显示
	req := httptest.NewRequest("GET", "/query/privileges", nil)
	req.AddCookie(other.cookie)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "qpriv.list") {
		t.Errorf("无权限页面: %d", rec.Code)
	}
	req = httptest.NewRequest("GET", "/query", nil)
	req.AddCookie(other.cookie)
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `href="/query/applies"`) || strings.Contains(rec.Body.String(), `href="/query/privileges"`) {
		t.Error("导航应只显示用户有权限的入口")
	}
	// 未登录
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", "/query/applies", nil))
	if rec.Code != 302 {
		t.Errorf("未登录应跳转: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/query/privilege-applies", strings.NewReader("{}")))
	if rec.Code != 401 {
		t.Errorf("未登录 API: %d", rec.Code)
	}
}

func itoa(f float64) string { return strings.TrimSuffix(strings.TrimSuffix(jsonNum(f), ".0"), ".00") }

func jsonNum(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}
