// @author: kevinkelly
// @license: Apache Licence
// @file: ext_http_test.go
// @time: 2026/10/28

package perf

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHTTPListExtraParamsAndCounts(t *testing.T) {
	env, f := newHTTP(t)
	id := f.newIssue()
	grantDev(env, 2)
	bob := env.Login("bob")

	var raw struct {
		Total   int `json:"total"`
		Summary struct {
			Todo   int `json:"todo"`
			Counts *struct {
				Assigned  int `json:"assigned"`
				Open      int `json:"open"`
				All       int `json:"all"`
				Created7d int `json:"created_7d"`
			} `json:"counts"`
		} `json:"summary"`
	}
	w := bob.Do("GET", "/api/v1/perf/issues?view=assigned&source=collector&status=assigned&scope=mine", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Total != 1 || raw.Summary.Counts == nil || raw.Summary.Counts.Assigned != 1 || raw.Summary.Counts.Open != 1 || raw.Summary.Counts.All != 1 || raw.Summary.Counts.Created7d != 1 || raw.Summary.Todo != 1 {
		t.Fatalf("响应: %+v", raw)
	}
	_ = id
	for _, q := range []string{"?status=zz", "?source=zz", "?scope=zz"} {
		if w := bob.Do("GET", "/api/v1/perf/issues"+q, nil); w.Code != 400 {
			t.Fatalf("%s 应 400: %d", q, w.Code)
		}
	}
	var o OptionsDTO
	bob.GetJSON("/api/v1/perf/options", &o)
	if len(o.Sources) != 3 {
		t.Fatalf("来源选项: %+v", o.Sources)
	}
}

func TestHTTPConfigExtEndpoints(t *testing.T) {
	env, f := newHTTP(t)
	f.newIssue()
	f.repo.routes = []Route{{ID: 1, DBRegex: "order", TeamID: 10, AssigneeID: ptr(uint(uDev)), Priority: 1}}
	root := env.Login("root")

	// 路由测试
	var rt RouteTestResult
	root.PostJSON("/api/v1/perf/config/route-test", map[string]any{"instance_id": 1, "db": "order", "table": "t_order"}, &rt)
	if !rt.Matched || rt.Rule.ID != 1 || rt.Assignee != "张三" || rt.Team != "订单研发" {
		t.Fatalf("路由测试: %+v", rt)
	}
	if code, _ := root.JSON("POST", "/api/v1/perf/config/route-test", map[string]any{"instance_id": 0}, nil); code != 400 {
		t.Fatalf("缺实例: %d", code)
	}

	// 采集窗口
	var w WindowInfo
	root.JSON("PUT", "/api/v1/perf/config/collect-window", map[string]any{"minutes": 30}, &w)
	if w.Minutes != 30 || !w.Custom {
		t.Fatalf("窗口: %+v", w)
	}
	if code, _ := root.JSON("PUT", "/api/v1/perf/config/collect-window", map[string]any{"minutes": 1}, nil); code != 400 {
		t.Fatalf("窗口太小: %d", code)
	}
	var co CollectOverview
	root.GetJSON("/api/v1/perf/config/collect", &co)
	if co.Window.Minutes != 30 {
		t.Fatalf("总览应带窗口: %+v", co.Window)
	}

	// SLA + 升级矩阵
	var sla SLAOverview
	root.GetJSON("/api/v1/perf/config/sla", &sla)
	if !sla.EscalationReady || !sla.Escalation.equal(defaultEscMatrix()) {
		t.Fatalf("矩阵默认值: %+v", sla)
	}
	m := defaultEscMatrix()
	m["L2"] = EscTargets{Owner: true, TL: true}
	body := map[string]any{"policies": sla.Policies, "escalation": m}
	if code, _ := root.JSON("PUT", "/api/v1/perf/config/sla", body, nil); code != 200 {
		t.Fatalf("保存: %d", code)
	}
	root.GetJSON("/api/v1/perf/config/sla", &sla)
	if !sla.Escalation.equal(m) {
		t.Fatalf("保存后: %+v", sla.Escalation)
	}
	// 矩阵不合法时，时限也不应保存
	bad := defaultEscMatrix()
	bad["L1"] = EscTargets{TL: true}
	body["escalation"] = bad
	body["policies"] = []SLAPolicy{{Severity: "P0", AcceptHours: 1, ResolveHours: 2, L3AfterHours: 1, RemindIntervalHours: 1}, {Severity: "P1", AcceptHours: 1, ResolveHours: 2, L3AfterHours: 1, RemindIntervalHours: 1},
		{Severity: "P2", AcceptHours: 1, ResolveHours: 2, L3AfterHours: 1, RemindIntervalHours: 1}, {Severity: "P3", AcceptHours: 1, ResolveHours: 2, L3AfterHours: 1, RemindIntervalHours: 1}}
	code, msg := root.JSON("PUT", "/api/v1/perf/config/sla", body, nil)
	if code != 400 || !strings.Contains(msg, "负责人") {
		t.Fatalf("矩阵不合法: %d %s", code, msg)
	}
	if ps, _ := f.repo.ListSLA(); len(ps) > 0 && ps[0].AcceptHours == 1 {
		t.Fatal("矩阵不合法时不应保存时限")
	}
	// 路由规则带表名正则
	var created struct{ ID uint }
	root.PostJSON("/api/v1/perf/config/routes", map[string]any{"db_regex": "pay", "team_id": 10, "priority": 9, "tbl_regex": "refund.*"}, &created)
	var team TeamOverview
	root.GetJSON("/api/v1/perf/config/team", &team)
	found := false
	for _, r := range team.Routes {
		found = found || r.TblRegex == "refund.*"
	}
	if !found {
		t.Fatalf("路由总览应带表名正则: %+v", team.Routes)
	}
}

func TestHTTPExtEndpointsNeedPermissions(t *testing.T) {
	env, f := newHTTP(t)
	f.newIssue()
	grantDev(env, 2)
	bob := env.Login("bob")
	for _, c := range []struct{ method, path string }{{"POST", "/api/v1/perf/config/route-test"}, {"PUT", "/api/v1/perf/config/collect-window"}} {
		if w := bob.Do(c.method, c.path, map[string]any{"instance_id": 1, "minutes": 10}); w.Code != 403 {
			t.Fatalf("%s %s 需要配置权限: %d", c.method, c.path, w.Code)
		}
	}
}

func TestHTTPNewPageControls(t *testing.T) {
	env, f := newHTTP(t)
	f.newIssue()
	root := env.Login("root")
	pages := map[string][]string{
		"/perf":                {`id="pl-kpis"`, `id="pl-status"`, `id="pl-src"`, `id="pl-scope"`, `data-view="assigned"`, `data-view="approve"`, `data-count="overdue"`},
		"/perf/1":              {`id="pa-wf"`, `id="pa-effect"`, `id="pd-effect-card"`},
		"/perf/config/collect": {`id="pcc-window"`, `id="pcc-win-min"`, `id="pcc-ro"`, `data-btn="perfcfg.collect"`},
		"/perf/config/team":    {`id="rt-go"`, `id="mr-tbl"`, `id="pt-ro"`, `data-btn="perfcfg.team"`},
		"/perf/config/sla":     {`id="ps-esc"`, `id="ps-ro"`, `data-btn="perfcfg.sla"`},
	}
	for path, wants := range pages {
		code, body := root.Page(path)
		if code != 200 {
			t.Fatalf("%s = %d", path, code)
		}
		for _, w := range wants {
			if !strings.Contains(body, w) {
				t.Errorf("%s 缺少 %q", path, w)
			}
		}
	}
	// 普通研发看不到“可见范围”（只有 DBA 才能在两种范围间切换）
	grantDev(env, 2)
	if _, body := env.Login("bob").Page("/perf"); strings.Contains(body, `id="pl-scope"`) {
		t.Error("非 DBA 不应有可见范围选择")
	}
}
