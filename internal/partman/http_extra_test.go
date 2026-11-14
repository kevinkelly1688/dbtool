// @author: kevinkelly
// @license: Apache Licence
// @file: http_extra_test.go
// @time: 2026/10/28

package partman

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHTTPStatusSummaryEnableAndCatalog(t *testing.T) {
	h := newHTTPEnv(t)
	root := h.web.Login("root")
	h.tgt.tables["log.access_log"] = okTable()
	cfg := h.addConfig(goodInput())
	id := itoa(cfg.ID)

	// 状态：读得到
	code, r := call(t, root, "GET", "/api/v1/partman/configs/"+id+"/status", nil)
	var sv StatusView
	_ = json.Unmarshal(r.Data, &sv)
	if code != 200 || sv.State != StateOK || sv.ID != cfg.ID {
		t.Fatalf("状态: %d %+v", code, r)
	}
	// 读不到目标库：仍是 200 + unknown，不会因为慢实例让接口报错
	h.tgt.dialErr = errAny("dial tcp: i/o timeout")
	code, r = call(t, root, "GET", "/api/v1/partman/configs/"+id+"/status?refresh=1", nil)
	_ = json.Unmarshal(r.Data, &sv)
	if code != 200 || sv.State != StateUnknown {
		t.Fatalf("未知: %d %+v", code, r)
	}
	h.tgt.dialErr = nil
	if code, _ = call(t, root, "GET", "/api/v1/partman/configs/abc/status", nil); code != 400 {
		t.Fatalf("编号非法: %d", code)
	}
	if code, _ = call(t, root, "GET", "/api/v1/partman/configs/999/status", nil); code != 404 {
		t.Fatalf("不存在: %d", code)
	}

	// 汇总
	code, r = call(t, root, "GET", "/api/v1/partman/summary", nil)
	var sm Summary
	_ = json.Unmarshal(r.Data, &sm)
	if code != 200 || sm.Total != 1 || sm.Enabled != 1 || len(sm.Items) != 1 {
		t.Fatalf("汇总: %d %+v", code, r)
	}

	// 启停：body 必须带 enable
	if code, _ = call(t, root, "POST", "/api/v1/partman/configs/"+id+"/enable", map[string]any{}); code != 400 {
		t.Fatalf("缺 enable: %d", code)
	}
	code, r = call(t, root, "POST", "/api/v1/partman/configs/"+id+"/enable", map[string]any{"enable": false})
	var cv ConfigView
	_ = json.Unmarshal(r.Data, &cv)
	if code != 200 || cv.IsEnable {
		t.Fatalf("停用: %d %+v", code, r)
	}
	if got := h.repo.auditActions(); len(got) != 1 || got[0] != "分区维护-停用配置" {
		t.Fatalf("审计: %v", got)
	}

	// 库表级联
	code, r = call(t, root, "GET", "/api/v1/partman/instances/1/databases", nil)
	var dbs []string
	_ = json.Unmarshal(r.Data, &dbs)
	if code != 200 || len(dbs) == 0 {
		t.Fatalf("库: %d %+v", code, r)
	}
	code, r = call(t, root, "GET", "/api/v1/partman/instances/1/tables?db=log", nil)
	var tbs []TableInfo
	_ = json.Unmarshal(r.Data, &tbs)
	if code != 200 || len(tbs) == 0 {
		t.Fatalf("表: %d %+v", code, r)
	}
	code, r = call(t, root, "GET", "/api/v1/partman/instances/1/columns?db=log&table=access_log", nil)
	var cols []ColumnInfo
	_ = json.Unmarshal(r.Data, &cols)
	if code != 200 || len(cols) != 4 { // datetime / date / int / bigint（date 是后加的候选）
		t.Fatalf("字段: %d %+v", code, r)
	}
	if code, _ = call(t, root, "GET", "/api/v1/partman/instances/1/tables?db=mysql", nil); code != 400 {
		t.Fatalf("系统库: %d", code)
	}
	h.tgt.inspectEr = errAny("denied")
	if code, r = call(t, root, "GET", "/api/v1/partman/instances/1/tables?db=log", nil); code != 502 || r.Code != "target_error" {
		t.Fatalf("目标库失败应提示: %d %+v", code, r)
	}
}

func TestHTTPNewEndpointsPermissionsAndLogsPage(t *testing.T) {
	h := newHTTPEnv(t)
	bob := h.web.Login("bob")
	for _, p := range []string{"/api/v1/partman/summary", "/api/v1/partman/logs", "/api/v1/partman/configs/1/status", "/api/v1/partman/instances/1/databases"} {
		if code, _ := call(t, bob, "GET", p, nil); code != 403 {
			t.Errorf("无菜单权限 %s 应 403: %d", p, code)
		}
		if w := h.web.Anonymous("GET", p); w.Code != 401 {
			t.Errorf("未登录 %s 应 401: %d", p, w.Code)
		}
	}
	if code, _ := bob.Page("/partman/logs"); code != 403 {
		t.Errorf("无菜单权限不能打开日志页: %d", code)
	}
	if w := h.web.Anonymous("GET", "/partman/logs"); w.Code != 302 {
		t.Errorf("未登录日志页应跳转: %d", w.Code)
	}
	h.grant(2, permMenu)
	if code, body := bob.Page("/partman/logs"); code != 200 || !strings.Contains(body, "/static/js/partman/logs.js") || !strings.Contains(body, `id="pg-rows"`) {
		t.Errorf("日志页: %d", code)
	}
	if code, body := bob.Page("/partman"); code != 200 || !strings.Contains(body, `id="pm-kpis"`) || !strings.Contains(body, `id="pe-hint"`) {
		t.Errorf("配置页: %d", code)
	}
	// 只读用户：能看状态与全局日志，不能启停、不能浏览库表
	if code, _ := call(t, bob, "GET", "/api/v1/partman/summary", nil); code != 200 {
		t.Errorf("summary: %d", code)
	}
	if code, _ := call(t, bob, "GET", "/api/v1/partman/logs", nil); code != 200 {
		t.Errorf("logs: %d", code)
	}
	if code, _ := call(t, bob, "POST", "/api/v1/partman/configs/1/enable", map[string]any{"enable": true}); code != 403 {
		t.Errorf("只读用户不能启停: %d", code)
	}
	if code, r := call(t, bob, "GET", "/api/v1/partman/instances/1/databases", nil); code != 403 || r.Code != "forbidden" {
		t.Errorf("只读用户不能浏览库表: %d %+v", code, r)
	}
	if code, r := call(t, bob, "GET", "/api/v1/partman/logs?source=bogus", nil); code != 400 {
		t.Errorf("非法来源: %d %+v", code, r)
	}
}

func TestHTTPGlobalLogsResponse(t *testing.T) {
	h := newHTTPEnv(t)
	root := h.web.Login("root")
	cfg := h.addConfig(goodInput())
	h.repo.InsertLog(cfg.CnfID, `[{"sql_text":"ADD","status":"success","source":"auto","operator":"system"}]`, fixedNow)
	code, r := call(t, root, "GET", "/api/v1/partman/logs?source=auto&config_id="+itoa(cfg.ID), nil)
	var rows []GlobalLogView
	_ = json.Unmarshal(r.Data, &rows)
	if code != 200 || r.Total != 1 || len(rows) != 1 || rows[0].Source != "auto" || rows[0].Operator != "system" || rows[0].Status != "success" || rows[0].Table != "log.access_log" {
		t.Fatalf("%d %+v", code, r)
	}
	var raw map[string]json.RawMessage
	w := root.Do("GET", "/api/v1/partman/logs", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	if _, ok := raw["configs"]; !ok {
		t.Fatalf("响应应带筛选用的 configs: %s", w.Body.String())
	}
}
