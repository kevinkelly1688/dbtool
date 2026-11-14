// @author: kevinkelly
// @license: Apache Licence
// @file: http_test.go
// @time: 2026/10/28

package instmgr

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// 测试里 server 没有真实元数据库（DB() 不可用），init() 登记的菜单会被隐藏；这里另登记一份只看权限的，用来检查顶栏可见性。
func init() {
	for _, n := range navDefs {
		n := n
		server.AddMenu(server.MenuItem{Group: "inst", Key: n.Key, Href: n.Href, Label: n.Label, Order: n.Order,
			Show: func(s *server.Server, a audit.Actor) bool { return menuAllowed(s, a, n.Button) }})
	}
}

// fullEnv 把各页面用到的目标库假数据放在同一个假目标库里。
func fullEnv() *env {
	e := procEnv()
	for _, other := range []*env{accEnv(), dbEnv(), paramEnv()} {
		e.target.handlers = append(e.target.handlers, other.target.handlers...)
	}
	e.repo.tpls = paramEnv().repo.tpls
	return e
}

func newHTTP(t *testing.T, e *env) *servertest.Env {
	t.Helper()
	return servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){
		func(s *server.Server, rt server.Routes) { registerWith(s, rt, e.svc) },
	}})
}

func grant(env *servertest.Env, codes ...string) {
	m := map[string]bool{}
	for _, c := range codes {
		m[c] = true
	}
	env.Repo.Perms[2] = m
}

var sideRe = regexp.MustCompile(`(?s)<aside class="sidenav".*?</aside>`)

func TestPagesRenderAndPermissions(t *testing.T) {
	pages := []struct{ path, perm, text, script string }{
		{"/instances", "instance.view", "实例列表", "list.js"},
		{"/instances/sessions", "session.open", "会话诊断", "sessions.js"},
		{"/instances/databases", "database.view", "数据库", "databases.js"},
		{"/instances/accounts", "account.view", "帐号维护", "accounts.js"},
		{"/instances/params", "param.open", "参数管理", "params.js"},
	}
	env := newHTTP(t, newEnv())
	if w := env.Anonymous("GET", "/instances"); w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Fatalf("未登录应跳转登录页: %d %s", w.Code, w.Header().Get("Location"))
	}
	root, bob := env.Login("root"), env.Login("bob")
	for _, p := range pages {
		if code, _ := bob.Page(p.path); code != 403 {
			t.Errorf("%s：没有权限应 403，实际 %d", p.path, code)
		}
		code, body := root.Page(p.path)
		if code != 200 || !strings.Contains(body, p.text) || !strings.Contains(body, "/static/js/instmgr/"+p.script) || !strings.Contains(body, "/static/css/instmgr.css") {
			t.Errorf("%s：超级用户应能打开 (%d)", p.path, code)
		}
		// 超级用户的左侧二级菜单有全部五项，当前页高亮
		side := sideRe.FindString(body)
		for _, q := range pages {
			if !strings.Contains(side, `href="`+q.path+`"`) {
				t.Errorf("%s：侧栏缺少 %s", p.path, q.path)
			}
		}
		if !strings.Contains(side, `href="`+p.path+`" class="on" aria-current="page"`) {
			t.Errorf("%s：侧栏应高亮当前页: %s", p.path, side)
		}
		if strings.Contains(body, "onclick=") || strings.Contains(body, "<script>") {
			t.Errorf("%s：CSP 不允许内联脚本", p.path)
		}
	}
	// 普通用户只有两个页面的权限：侧栏与顶栏都只出现这两项
	grant(env, "instance.view", "param.open")
	code, body := bob.Page("/instances")
	if code != 200 {
		t.Fatal(code)
	}
	side := sideRe.FindString(body)
	if !strings.Contains(side, `href="/instances/params"`) || strings.Contains(side, "/instances/sessions") || strings.Contains(side, "/instances/accounts") || strings.Contains(side, "/instances/databases") {
		t.Fatalf("侧栏应只含有权限的页面: %s", side)
	}
	if strings.Contains(body, `href="/instances/sessions"`) || !strings.Contains(body, `href="/instances/params"`) {
		t.Fatal("顶栏“实例管理”下拉也应按权限显示")
	}
	if !strings.Contains(body, "实例管理") {
		t.Fatal("顶栏应有“实例管理”分组")
	}
}

func TestAPIRequiresLoginCSRFAndPermission(t *testing.T) {
	e := fullEnv()
	env := newHTTP(t, e)
	for _, p := range []string{"/api/v1/instances", "/api/v1/instmgr/caps", "/api/v1/instances/1/processlist"} {
		if w := env.Anonymous("GET", p); w.Code != 401 {
			t.Errorf("%s 未登录应 401: %d", p, w.Code)
		}
	}
	root, bob := env.Login("root"), env.Login("bob")
	root.CSRF = "wrong"
	code, _ := root.JSON("POST", "/api/v1/instances/1/kill", killReq{Threads: []KillTarget{{ID: 13}}, Confirm: true}, nil)
	if code != 403 || len(e.target.execs()) != 0 {
		t.Fatalf("CSRF 令牌错误应 403 且不执行: %d", code)
	}

	// 权限矩阵：没有对应权限一律 403；给了权限就不会是 403/401
	body := map[string]any{"threads": []map[string]any{{"id": 13}}, "confirm": true}
	routes := []struct {
		method, path, perm string
		body               any
	}{
		{"GET", "/api/v1/instances", "instance.view", nil},
		{"GET", "/api/v1/instances/form-options", "instance.add", nil},
		{"POST", "/api/v1/instances", "instance.add", goodInput()},
		{"POST", "/api/v1/instances/test", "instance.edit", goodInput()},
		{"PUT", "/api/v1/instances/1", "instance.edit", goodInput()},
		{"DELETE", "/api/v1/instances/1", "instance.delete", map[string]any{"confirm": false}},
		{"GET", "/api/v1/instances/1/refs", "instance.delete", nil},
		{"POST", "/api/v1/instances/1/enable", "instance.edit", map[string]any{"enable": true}},
		{"POST", "/api/v1/instances/1/disable", "instance.edit", map[string]any{"disabled": true}},
		{"POST", "/api/v1/instances/batch/disable", "instance.edit", map[string]any{"ids": []uint{1}, "disabled": true}},
		{"POST", "/api/v1/instances/batch/collect", "instance.edit", map[string]any{"ids": []uint{1}, "enable": true}},
		{"POST", "/api/v1/instances/batch/delete", "instance.delete", map[string]any{"ids": []uint{1}, "dry_run": true}},
		{"GET", "/api/v1/instances/1/health", "instance.view", nil},
		{"GET", "/api/v1/params/history", "param.view", nil},
		{"POST", "/api/v1/instances/1/test", "instance.add", nil},
		{"GET", "/api/v1/instances/1/binlogs", "instance.purge_binlog", nil},
		{"POST", "/api/v1/instances/1/binlogs/preview", "instance.purge_binlog", map[string]any{"to": "x"}},
		{"POST", "/api/v1/instances/1/binlogs/purge", "instance.purge_binlog", map[string]any{"to": "x"}},
		{"GET", "/api/v1/instances/1/processlist", "session.process", nil},
		{"GET", "/api/v1/instances/1/tablespace", "session.tablespace", nil},
		{"GET", "/api/v1/instances/1/transactions", "session.trx", nil},
		{"GET", "/api/v1/instances/1/lock-waits", "session.locks", nil},
		{"POST", "/api/v1/instances/1/kill/preview", "session.kill", body},
		{"POST", "/api/v1/instances/1/kill", "session.kill", body},
		{"GET", "/api/v1/instances/1/databases", "database.view", nil},
		{"GET", "/api/v1/instances/1/databases/options", "database.view", nil},
		{"POST", "/api/v1/instances/1/databases", "database.create", DBCreate{Name: "x1"}},
		{"PUT", "/api/v1/instances/1/databases/info", "database.edit", DBInfo{Name: "shop", Owner: "dev"}},
		{"GET", "/api/v1/instances/1/accounts", "account.view", nil},
		{"GET", "/api/v1/instances/1/accounts/objects", "account.grant", nil},
		{"GET", "/api/v1/instances/1/accounts/owners", "account.edit", nil},
		{"POST", "/api/v1/instances/1/accounts", "account.create", AccountCreate{}},
		{"PUT", "/api/v1/instances/1/accounts/info", "account.edit", AccountInfo{User: "a", Host: "b"}},
		{"POST", "/api/v1/instances/1/accounts/grant/preview", "account.grant", GrantReq{}},
		{"POST", "/api/v1/instances/1/accounts/grant", "account.grant", GrantReq{}},
		{"POST", "/api/v1/instances/1/accounts/password", "account.pwd", PasswordReq{}},
		{"POST", "/api/v1/instances/1/accounts/lock", "account.lock", LockReq{}},
		{"POST", "/api/v1/instances/1/accounts/drop", "account.delete", DropReq{}},
		{"GET", "/api/v1/instances/1/params", "param.view", nil},
		{"POST", "/api/v1/instances/1/params/preview", "param.edit", paramReq{Name: "max_connections", Value: "3000"}},
		{"PUT", "/api/v1/instances/1/params", "param.edit", paramReq{Name: "max_connections", Value: "3000"}},
		{"GET", "/api/v1/instances/1/params/history", "param.view", nil},
		{"GET", "/api/v1/params/templates?db_type=mysql", "param.view", nil},
		{"POST", "/api/v1/params/templates", "param.template", TemplateInput{}},
		{"PUT", "/api/v1/params/templates/1", "param.template", TemplateInput{}},
	}
	for _, r := range routes {
		grant(env) // 清空
		if code, _ := bob.JSON(r.method, r.path, r.body, nil); code != 403 {
			t.Errorf("%s %s 没有权限应 403，实际 %d", r.method, r.path, code)
		}
		grant(env, r.perm)
		if code, _ := bob.JSON(r.method, r.path, r.body, nil); code == 403 || code == 401 {
			t.Errorf("%s %s 有 %s 权限不应被拒绝，实际 %d", r.method, r.path, r.perm, code)
		}
	}
}

func TestListAPIShapeAndScope(t *testing.T) {
	e := fullEnv()
	env := newHTTP(t, e)
	root, bob := env.Login("root"), env.Login("bob")
	var out struct {
		Total int            `json:"total"`
		Items []InstanceItem `json:"items"`
	}
	root.GetJSON("/api/v1/instances", &out)
	if out.Total != 5 || len(out.Items) != 5 {
		t.Fatalf("%+v", out)
	}
	grant(env, "instance.view")
	bob.GetJSON("/api/v1/instances", &out)
	if out.Total != 3 {
		t.Fatalf("没有管理权限的普通用户按资源组可见: %d", out.Total)
	}
	grant(env, "instance.view", "instance.edit")
	bob.GetJSON("/api/v1/instances", &out)
	if out.Total != 5 {
		t.Fatalf("能修改实例的人看到全部: %d", out.Total)
	}
	grant(env, "instance.view", "query.admin_run")
	bob.GetJSON("/api/v1/instances?db_type=mssql&page_size=1", &out)
	if out.Total != 1 || out.Items[0].Name != "mssql-01" {
		t.Fatalf("%+v", out)
	}
	if code, msg := bob.JSON("GET", "/api/v1/instances?status=zz", nil, nil); code != 400 || !strings.Contains(msg, "状态") {
		t.Fatalf("%d %s", code, msg)
	}
	// 响应里没有密码
	w := root.Do("GET", "/api/v1/instances", nil)
	if strings.Contains(w.Body.String(), "secret-pw") || strings.Contains(strings.ToLower(w.Body.String()), "password") {
		t.Fatalf("列表响应不能含密码字段: %s", w.Body.String())
	}
}

func TestInstanceCRUDOverHTTP(t *testing.T) {
	e := fullEnv().withVersion("8.0.36")
	env := newHTTP(t, e)
	root := env.Login("root")
	var it InstanceItem
	root.PostJSON("/api/v1/instances", goodInput(), &it)
	if it.ID == 0 || it.Name != "prod-mysql-03" {
		t.Fatalf("%+v", it)
	}
	code, msg := root.JSON("POST", "/api/v1/instances", goodInput(), nil)
	if code != 409 || !strings.Contains(msg, "已存在") {
		t.Fatalf("%d %s", code, msg)
	}
	in := goodInput()
	in.Password = ""
	if code, msg := root.JSON("PUT", "/api/v1/instances/"+itoa(it.ID), in, nil); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	if code, _ := root.JSON("PUT", "/api/v1/instances/9999", in, nil); code != 404 {
		t.Fatalf("%d", code)
	}
	var tr TestResult
	root.PostJSON("/api/v1/instances/1/test", nil, &tr)
	if tr.Version != "8.0.36" {
		t.Fatalf("%+v", tr)
	}
	root.PostJSON("/api/v1/instances/test", map[string]any{"id": 1, "instance_name": "x", "db_type": "mysql", "host": "10.0.0.9", "port": 3306, "user": "u", "password": ""}, &tr)
	if e.dialer.dialed[len(e.dialer.dialed)-1].Password != "secret-pw" {
		t.Fatal("表单测试连接留空密码应沿用已保存的")
	}
	var opts FormOptions
	root.GetJSON("/api/v1/instances/form-options", &opts)
	hasWrite := false
	for _, tg := range opts.Tags {
		hasWrite = hasWrite || tg.Code == "can_write"
	}
	if !hasWrite || len(opts.Groups) != 2 || len(opts.DBTypes) < 10 {
		t.Fatalf("%+v", opts)
	}
	// 删除：先看引用，再确认
	e.repo.refs[it.ID] = []Ref{{Label: "SQL 工单", Count: 2, Blocking: true}}
	var refs []Ref
	root.GetJSON("/api/v1/instances/"+itoa(it.ID)+"/refs", &refs)
	if len(refs) != 1 {
		t.Fatalf("%+v", refs)
	}
	code, msg = root.JSON("DELETE", "/api/v1/instances/"+itoa(it.ID), map[string]any{"confirm": true}, nil)
	if code != 409 || !strings.Contains(msg, "SQL 工单 2 条") {
		t.Fatalf("%d %s", code, msg)
	}
	delete(e.repo.refs, it.ID)
	if code, msg := root.JSON("DELETE", "/api/v1/instances/"+itoa(it.ID), map[string]any{"confirm": true}, nil); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	root.PostJSON("/api/v1/instances/1/enable", map[string]any{"enable": true}, nil)
}

func itoa(n uint) string { return utoa(n) }

func TestSessionAndKillOverHTTP(t *testing.T) {
	e := fullEnv()
	env := newHTTP(t, e)
	bob := env.Login("bob")
	grant(env, "session.process", "session.kill")
	var res ProcResult
	bob.GetJSON("/api/v1/instances/1/processlist?command=Not+Sleep&min_time=10", &res)
	if res.Self != 10 || len(res.Rows) != 5 {
		t.Fatalf("%+v", res)
	}
	var plan KillPlan
	bob.PostJSON("/api/v1/instances/1/kill/preview", killReq{Threads: []KillTarget{{ID: 13}, {ID: 10}}}, &plan)
	if len(plan.Statements) != 1 || plan.Statements[0] != "KILL 13;" {
		t.Fatalf("%+v", plan)
	}
	// 不接受前端传整条语句：多余字段被忽略，语句仍由服务端生成
	raw := `{"threads":[{"id":13}],"confirm":true,"sql":"KILL 1; DROP USER x"}`
	var kr []KillResult
	if code, msg := bob.JSON("POST", "/api/v1/instances/1/kill", raw, &kr); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	if ex := e.target.execs(); len(ex) != 1 || ex[0].sql != "KILL 13" {
		t.Fatalf("%+v", ex)
	}
	// 不在自己资源组的实例
	if code, msg := bob.JSON("POST", "/api/v1/instances/2/kill", killReq{Threads: []KillTarget{{ID: 13}}, Confirm: true}, nil); code != 403 || !strings.Contains(msg, "未关联") {
		t.Fatalf("%d %s", code, msg)
	}
	if code, _ := bob.JSON("POST", "/api/v1/instances/1/kill", "{bad json", nil); code != 400 {
		t.Fatalf("坏 JSON 应 400: %d", code)
	}
	if code, _ := bob.JSON("GET", "/api/v1/instances/abc/processlist", nil, nil); code != 400 {
		t.Fatalf("坏编号应 400: %d", code)
	}
	if code, _ := bob.JSON("GET", "/api/v1/instances/0/processlist", nil, nil); code != 400 {
		t.Fatalf("0 号应 400: %d", code)
	}
}

func TestAccountsDatabasesParamsOverHTTP(t *testing.T) {
	e := fullEnv()
	env := newHTTP(t, e)
	root := env.Login("root")

	var accs AccountList
	root.GetJSON("/api/v1/instances/1/accounts?q=app", &accs)
	if len(accs.Items) != 1 || accs.Items[0].User != "app_rw" {
		t.Fatalf("%+v", accs)
	}
	var plan GrantPlan
	g := GrantReq{User: "app_rw", Host: "10.20.%", Op: "grant", Scope: "db", DBs: []string{"shop"}, Privs: []string{"SELECT"}}
	root.PostJSON("/api/v1/instances/1/accounts/grant/preview", g, &plan)
	if len(plan.Statements) != 1 || !strings.HasPrefix(plan.Statements[0], "GRANT SELECT ON `shop`.*") {
		t.Fatalf("%+v", plan)
	}
	// 密码不会出现在任何响应里
	w := root.Do("POST", "/api/v1/instances/1/accounts", AccountCreate{User: "u1", Host: "%", Password: "Passw0rd!x", Password2: "Passw0rd!x", Confirm: true})
	if w.Code != 200 || strings.Contains(w.Body.String(), "Passw0rd") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	for _, a := range e.repo.audits {
		if strings.Contains(a.ExtraInfo, "Passw0rd") {
			t.Fatal("审计里不能有密码")
		}
	}

	var dbs []DBItem
	root.GetJSON("/api/v1/instances/1/databases?saved=1", &dbs)
	if len(dbs) != 0 {
		t.Fatalf("%+v", dbs)
	}
	root.PostJSON("/api/v1/instances/1/databases", DBCreate{Name: "newdb", Owner: "dev", Confirm: true}, nil)
	root.GetJSON("/api/v1/instances/1/databases?saved=1", &dbs)
	_ = dbs
	var dopts DBOptions
	root.GetJSON("/api/v1/instances/1/databases/options?owners_only=1", &dopts)
	if len(dopts.Owners) != 2 {
		t.Fatalf("%+v", dopts)
	}

	var rows []ParamRow
	root.GetJSON("/api/v1/instances/1/params?editable=yes", &rows)
	if len(rows) != 5 {
		t.Fatalf("%d", len(rows))
	}
	var pp ParamPlan
	root.PostJSON("/api/v1/instances/1/params/preview", paramReq{Name: "max_connections", Value: "3000"}, &pp)
	if pp.SQL != "SET GLOBAL max_connections = 3000" {
		t.Fatalf("%+v", pp)
	}
	root.JSON("PUT", "/api/v1/instances/1/params", paramReq{Name: "max_connections", Value: "3000", Confirm: true}, nil)
	var hist HistoryPage
	root.GetJSON("/api/v1/instances/1/params/history?page_size=5", &hist)
	if hist.Total != 1 || len(hist.Rows) != 1 || hist.Rows[0].VariableName != "max_connections" || hist.Rows[0].CreateTimeText != "2026-10-06 12:00:00" {
		t.Fatalf("%+v", hist)
	}
	var tpl ParamTemplate
	root.PostJSON("/api/v1/params/templates", TemplateInput{DBType: "mysql", VariableName: "innodb_io_capacity", Editable: true, ValidValues: "[100-20000]"}, &tpl)
	if tpl.ID == 0 {
		t.Fatalf("%+v", tpl)
	}
	var tpls []ParamTemplate
	root.GetJSON("/api/v1/params/templates?db_type=mysql", &tpls)
	if len(tpls) != 8 {
		t.Fatalf("%d", len(tpls))
	}
	if code, msg := root.JSON("GET", "/api/v1/params/templates?db_type=mssql", nil, nil); code != 400 || !strings.Contains(msg, "暂不支持") {
		t.Fatalf("%d %s", code, msg)
	}
}

func TestCapsAndChoices(t *testing.T) {
	e := fullEnv()
	env := newHTTP(t, e)
	root, bob := env.Login("root"), env.Login("bob")
	var caps map[string]bool
	grant(env, "session.process")
	bob.GetJSON("/api/v1/instmgr/caps", &caps)
	if !caps["session.process"] || caps["session.kill"] || caps["instance.delete"] {
		t.Fatalf("%+v", caps)
	}
	root.GetJSON("/api/v1/instmgr/caps", &caps)
	for _, c := range capButtons {
		if !caps[c] {
			t.Errorf("超级用户应拥有 %s", c)
		}
	}
	var list []InstanceChoice
	if code, _ := bob.JSON("GET", "/api/v1/instmgr/choices?feature=sessions", nil, nil); code != 403 {
		t.Fatalf("没有 session.open 不能取实例下拉: %d", code)
	}
	grant(env, "session.open")
	bob.GetJSON("/api/v1/instmgr/choices?feature=sessions", &list)
	if len(list) != 3 {
		t.Fatalf("%+v", list)
	}
	if code, _ := bob.JSON("GET", "/api/v1/instmgr/choices?feature=zzz", nil, nil); code != 400 {
		t.Fatalf("%d", code)
	}
}

// failingRepo 让列表查询返回内部错误，检查不会把底层错误信息返回给前端。
type failingRepo struct{ *fakeRepo }

func (failingRepo) ListInstances(InstanceFilter, map[uint]bool) ([]model.Instance, int64, error) {
	return nil, 0, errors.New("Error 1146: Table 'archery.sql_instance' doesn't exist")
}

func TestInternalErrorsAreNotLeaked(t *testing.T) {
	e := newEnv()
	e.svc.repo = failingRepo{e.repo}
	env := newHTTP(t, e)
	root := env.Login("root")
	w := root.Do("GET", "/api/v1/instances", nil)
	if w.Code != 500 || strings.Contains(w.Body.String(), "dbt_sql_instance") || strings.Contains(w.Body.String(), "1146") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != "internal" {
		t.Fatalf("%v", body)
	}
}
