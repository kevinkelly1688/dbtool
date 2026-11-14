// @author: kevinkelly
// @license: Apache Licence
// @file: http_test.go
// @time: 2026/10/28

package slowlog

import (
	"encoding/json"
	"strings"
	"testing"

	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// newEnv 起一个带慢日志模块的内存服务：servertest 里 root(ID=1) 是超级用户、bob(ID=2) 是普通用户；
// 模块自己的仓库是 fixture() 里的假仓库（bob 有菜单与优化权限、可见实例 1 和 3）。
func newEnv(t *testing.T) (*servertest.Env, *fakeRepo) {
	t.Helper()
	svc, repo := fixture()
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){
		func(s *server.Server, rt server.Routes) { register(s, rt, svc) },
	}})
	return env, repo
}

func grantServer(env *servertest.Env, uid uint, codes ...string) {
	if env.Repo.Perms[uid] == nil {
		env.Repo.Perms[uid] = map[string]bool{}
	}
	for _, c := range codes {
		env.Repo.Perms[uid][c] = true
	}
}

func TestHTTPAuthAndPermissions(t *testing.T) {
	env, _ := newEnv(t)
	for _, p := range []string{"/api/v1/slowlog/instances", "/api/v1/slowlog/slowlogs", "/api/v1/slowlog/review", "/api/v1/slowlog/collector"} {
		if w := env.Anonymous("GET", p); w.Code != 401 {
			t.Errorf("未登录 GET %s 应 401: %d", p, w.Code)
		}
	}
	if w := env.Anonymous("POST", "/api/v1/slowlog/advise"); w.Code != 401 {
		t.Errorf("未登录 POST advise 应 401: %d", w.Code)
	}
	for _, p := range []string{"/slowlogs", "/slowquery"} {
		if w := env.Anonymous("GET", p); w.Code == 200 || strings.Contains(w.Body.String(), "慢日志") {
			t.Errorf("未登录不能看页面 %s: %d", p, w.Code)
		}
	}

	bob := env.Login("bob")
	for _, p := range []string{"/api/v1/slowlog/instances", "/api/v1/slowlog/slowlogs", "/api/v1/slowlog/slowlogs/detail", "/api/v1/slowlog/collector",
		"/api/v1/slowlog/databases", "/api/v1/slowlog/review", "/api/v1/slowlog/review/history", "/api/v1/slowlog/review/trend"} {
		if code, _ := bob.JSON("GET", p, nil, nil); code != 403 {
			t.Errorf("没有 slowlogs.view/slowquery.view 的用户 GET %s 应 403: %d", p, code)
		}
	}
	if code, _ := bob.JSON("POST", "/api/v1/slowlog/advise", map[string]any{"instance_id": 1, "sql": "select 1"}, nil); code != 403 {
		t.Errorf("没有优化权限不能获取建议: %d", code)
	}
	for _, p := range []string{"/slowlogs", "/slowquery"} {
		if code, _ := bob.Page(p); code != 403 {
			t.Errorf("没有权限的用户打开 %s 应 403: %d", p, code)
		}
	}
}

func TestHTTPPages(t *testing.T) {
	env, _ := newEnv(t)
	grantServer(env, 2, "slowlogs.view", "slowquery.view")
	root, bob := env.Login("root"), env.Login("bob")

	code, body := root.Page("/slowlogs")
	for _, want := range []string{"慢日志检索", `id="sl-form"`, "/static/js/slowlog/slowlogs.js", "/static/css/slowlog.css", `id="sl-modal"`, `id="sl-crows"`} {
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("/slowlogs 缺少 %q (%d)", want, code)
		}
	}
	code, body = root.Page("/slowquery")
	for _, want := range []string{"慢 SQL 洞察", `id="sq-form"`, "/static/js/slowlog/slowquery.js", `data-tab="stat"`, `data-tab="det"`, `id="sq-advise"`, `data-can-advise="1"`} {
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("/slowquery 缺少 %q (%d)", want, code)
		}
	}
	// 菜单：两个入口都在“性能优化”下
	for _, want := range []string{`href="/slowlogs"`, `href="/slowquery"`, "慢日志检索", "慢 SQL 洞察"} {
		if !strings.Contains(body, want) {
			t.Errorf("顶栏菜单缺少 %q", want)
		}
	}
	// 只有菜单权限、没有优化权限：看得到页面，但没有“获取优化建议”按钮
	code, body = bob.Page("/slowquery")
	if code != 200 || strings.Contains(body, `id="sq-advise"`) || !strings.Contains(body, `data-can-advise=""`) {
		t.Errorf("没有优化权限不应出现按钮 (%d)", code)
	}
	grantServer(env, 2, "slowquery.advise")
	if _, body = bob.Page("/slowquery"); !strings.Contains(body, `id="sq-advise"`) {
		t.Error("有 slowquery.advise 应出现按钮")
	}
	// 页面里不能有内联脚本（CSP）
	for _, p := range []string{"/slowlogs", "/slowquery"} {
		_, b := root.Page(p)
		if strings.Contains(b, "onclick=") || strings.Contains(strings.ReplaceAll(b, `<script type="module" src=`, ""), "<script>") {
			t.Errorf("%s 不能有内联脚本", p)
		}
	}
}

type envelope struct {
	Data json.RawMessage `json:"data"`
	Code string          `json:"code"`
	Msg  string          `json:"message"`
}

func call(t *testing.T, c *servertest.Client, method, path string, body any) (int, envelope) {
	t.Helper()
	w := c.Do(method, path, body)
	var e envelope
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("%s %s 响应不是 JSON: %s", method, path, w.Body.String())
	}
	return w.Code, e
}

func TestHTTPDataEndpoints(t *testing.T) {
	env, repo := newEnv(t)
	grantServer(env, 2, "slowlogs.view", "slowquery.view")
	bob := env.Login("bob")

	// 实例下拉
	var insts []InstanceInfo
	bob.GetJSON("/api/v1/slowlog/instances?page=logs", &insts)
	if len(insts) != 2 || insts[0].Name != "prod-mysql-01" {
		t.Errorf("实例: %+v", insts)
	}
	if code, e := call(t, bob, "GET", "/api/v1/slowlog/instances?page=nope", nil); code != 400 {
		t.Errorf("未知 page: %d %s", code, e.Msg)
	}

	// 聚合列表
	repo.groupRows = []SlowLogGroup{{InstanceID: 1, InstanceName: "prod-mysql-01", DBName: "shop", SQLMD5: "m1", ExecDate: "2026-10-06", Count: 3, SQLText: "select * from orders"}}
	repo.groupTotal = 1
	var page GroupPage
	bob.GetJSON("/api/v1/slowlog/slowlogs?instance_id=1&start=2026-10-01&end=2026-10-06&q=orders&sort=count&order=asc&limit=10&offset=0", &page)
	if page.Total != 1 || len(page.Rows) != 1 || page.Rows[0].TableName != "orders" || page.From != "2026-10-01" {
		t.Errorf("聚合: %+v", page)
	}
	q := repo.lastGroup
	if q.Search != "orders" || q.Sort != "count" || q.Desc || q.Limit != 10 || q.InstanceIDs[0] != 1 {
		t.Errorf("参数传递: %+v", q)
	}
	// 参数校验：全部 400，且带中文说明
	for path, msg := range map[string]string{
		"/api/v1/slowlog/slowlogs?limit=abc":                          "limit",
		"/api/v1/slowlog/slowlogs?limit=-1":                           "limit",
		"/api/v1/slowlog/slowlogs?offset=x":                           "offset",
		"/api/v1/slowlog/slowlogs?instance_id=x":                      "instance_id",
		"/api/v1/slowlog/slowlogs?start=2026-10-01":                   "同时提供",
		"/api/v1/slowlog/slowlogs?start=2020-01-01&end=2026-10-01":    "最长 93 天",
		"/api/v1/slowlog/slowlogs?sort=sql_text":                      "不支持按",
		"/api/v1/slowlog/slowlogs?limit=1000":                         "每页条数",
		"/api/v1/slowlog/slowlogs?instance_id=2":                      "未关联",
		"/api/v1/slowlog/slowlogs/detail?instance_id=1":               "执行日期",
		"/api/v1/slowlog/slowlogs/detail?instance_id=1&exec_date=bad": "日期格式",
		"/api/v1/slowlog/review":                                      "选择实例",
		"/api/v1/slowlog/review?instance_id=1&sort=bad":               "不支持按",
		"/api/v1/slowlog/review?instance_id=4":                        "未关联",
		"/api/v1/slowlog/review/trend?instance_id=1":                  "SQL 指纹",
	} {
		code, e := call(t, bob, "GET", path, nil)
		if (code != 400 && code != 403) || !strings.Contains(e.Msg, msg) {
			t.Errorf("GET %s: 期望含 %q，实际 %d %q", path, msg, code, e.Msg)
		}
	}

	// 明细
	repo.detailRows = []SlowLogRow{{ID: 9, SQLText: "select 1"}}
	var d struct {
		Total int          `json:"total"`
		Rows  []SlowLogRow `json:"rows"`
	}
	bob.GetJSON("/api/v1/slowlog/slowlogs/detail?instance_id=1&db_name=shop&sql_md5=m1&exec_date=2026-10-06", &d)
	if d.Total != 1 || d.Rows[0].ID != 9 {
		t.Errorf("明细: %+v", d)
	}

	// 统计 / 明细 / 趋势 / 库列表（mysql 且没有 digest 表 -> 平台采集）
	repo.collReview = []ReviewRow{{SQLID: "m1", SQLText: "select * from t where id = 5", ExecCount: 2}}
	var rv ReviewPage
	bob.GetJSON("/api/v1/slowlog/review?instance_id=1&start=2026-10-01&end=2026-10-06", &rv)
	if rv.Source != SourceCollected || rv.Rows[0].SQLText != "select * from t where id=?" || rv.Label == "" {
		t.Errorf("统计: %+v", rv)
	}
	repo.collHist = []HistoryRow{{ID: 1, SQLID: "m1", ExecCount: 1}}
	var hp HistoryPage
	bob.GetJSON("/api/v1/slowlog/review/history?instance_id=1&sql_id=m1", &hp)
	if hp.Total != 1 || repo.lastHist.SQLID != "m1" {
		t.Errorf("明细: %+v %+v", hp, repo.lastHist)
	}
	repo.trend = &Trend{Points: []TrendPoint{{T: "2026-10-06", Count: 2, Pct95: 1.2}}}
	var tr Trend
	bob.GetJSON("/api/v1/slowlog/review/trend?instance_id=1&sql_id=m1&start=2026-10-01&end=2026-10-06", &tr)
	if tr.Bucket != "day" || len(tr.Points) != 1 || tr.Points[0].Count != 2 {
		t.Errorf("趋势: %+v", tr)
	}
	repo.dbs = []string{"shop"}
	var dbs []string
	bob.GetJSON("/api/v1/slowlog/databases?instance_id=1", &dbs)
	if len(dbs) != 1 || dbs[0] != "shop" {
		t.Errorf("库: %v", dbs)
	}

	// 采集状态
	var ov CollectorOverview
	bob.GetJSON("/api/v1/slowlog/collector", &ov)
	if ov.Enabled || len(ov.Items) != 2 {
		t.Errorf("采集状态: %+v", ov)
	}

	// 仓库出错：只返回通用错误，不泄露内部信息
	repo.err = errBoom
	code, e := call(t, bob, "GET", "/api/v1/slowlog/slowlogs", nil)
	if code != 500 || strings.Contains(e.Msg, "refused") {
		t.Errorf("内部错误不应泄露细节: %d %q", code, e.Msg)
	}
}

func TestHTTPAdvise(t *testing.T) {
	env, repo := newEnv(t)
	grantServer(env, 2, "slowquery.advise")
	bob := env.Login("bob")

	var adv Advice
	bob.PostJSON("/api/v1/slowlog/advise", map[string]any{"instance_id": 1, "db_name": "shop", "sql": "select * from orders where status = 'paid'",
		"rows_examined": 9821004, "rows_sent": 5000, "query_time": 4.2}, &adv)
	if adv.Source != "rule" || len(adv.Items) == 0 {
		t.Fatalf("%+v", adv)
	}
	found := false
	for _, it := range adv.Items {
		found = found || it.Code == "select_star"
	}
	if !found {
		t.Errorf("应提示 SELECT *: %+v", adv.Items)
	}
	if len(repo.audits) != 1 {
		t.Errorf("应写操作审计: %d", len(repo.audits))
	}

	// CSRF：令牌不对 -> 403，且不会执行
	good := bob.CSRF
	bob.CSRF = "wrong-token"
	if code, _ := bob.JSON("POST", "/api/v1/slowlog/advise", map[string]any{"instance_id": 1, "sql": "select 1"}, nil); code != 403 {
		t.Errorf("CSRF 错误应 403: %d", code)
	}
	bob.CSRF = good
	if len(repo.audits) != 1 {
		t.Error("CSRF 失败不应产生审计")
	}

	for name, c := range map[string]struct {
		body any
		code int
		msg  string
	}{
		"坏 JSON": {"{not json", 400, "请求格式"},
		"空 SQL":  {map[string]any{"instance_id": 1, "sql": ""}, 400, "选择需要优化"},
		"越权实例":   {map[string]any{"instance_id": 2, "sql": "select 1"}, 403, "未关联"},
		"不支持的类型": {map[string]any{"instance_id": 3, "sql": "select 1"}, 400, "暂不支持优化建议"},
		"负数":     {map[string]any{"instance_id": 1, "sql": "select 1", "rows_examined": -5}, 400, "不能为负"},
		"类型错误":   {map[string]any{"instance_id": "x", "sql": "select 1"}, 400, "请求格式"},
		"超长 SQL": {map[string]any{"instance_id": 1, "sql": strings.Repeat("a", MaxAdviseSQL+1)}, 400, "过长"},
	} {
		code, e := call(t, bob, "POST", "/api/v1/slowlog/advise", c.body)
		if code != c.code || !strings.Contains(e.Msg, c.msg) {
			t.Errorf("%s: 期望 %d 含 %q，实际 %d %q", name, c.code, c.msg, code, e.Msg)
		}
	}
	// 菜单权限不是获取建议的前提（只看 optimize_*）
	if code, _ := call(t, bob, "POST", "/api/v1/slowlog/advise", map[string]any{"instance_id": 1, "sql": "select 1"}); code != 200 {
		t.Errorf("只有优化权限也应可用: %d", code)
	}
}

func TestHTTPRedisUnsupported(t *testing.T) {
	env, _ := newEnv(t)
	root := env.Login("root")
	code, e := call(t, root, "GET", "/api/v1/slowlog/review?instance_id=4", nil)
	if code != 400 || e.Code != "unsupported" || !strings.Contains(e.Msg, "Redis") {
		t.Errorf("Redis 应明确提示暂不支持: %d %+v", code, e)
	}
}
