// @author: kevinkelly
// @license: Apache Licence
// @file: http_merge_test.go
// @time: 2026/10/28

package partman

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 契约里的字段名：用原始 JSON 断言，防止结构体改名后悄悄偏离。
func TestHTTPInitFlowFieldNames(t *testing.T) {
	h := newHTTPEnv(t)
	root := h.web.Login("root")
	h.plain("fresh")
	cfg := h.addConfig(initInput("fresh"))
	id := itoa(cfg.ID)

	w := root.Do("POST", "/api/v1/partman/preview", map[string]any{"id": cfg.ID, "init": map[string]any{"start_mode": "date", "start_date": "2026-10-04"}})
	var raw struct{ Data map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	d := raw.Data
	if w.Code != 200 || d["kind"] != "init" || d["dialect"] != "mysql" || d["timezone"] != "Asia/Shanghai" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	init := d["init"].(map[string]any)
	for _, k := range []string{"start_mode", "start_date", "start", "partition_count", "first_partition", "last_partition", "last_bound", "earliest_data", "can_execute", "checks", "notes", "confirm"} {
		if _, ok := init[k]; !ok {
			t.Errorf("init 缺少字段 %s", k)
		}
	}
	for _, k := range []string{"table", "now", "method", "expression", "existing", "statements", "drops", "reorganize", "warnings", "output"} {
		if _, ok := d[k]; !ok {
			t.Errorf("计划缺少字段 %s", k)
		}
	}
	out := d["output"].(map[string]any)
	if _, ok := out["lines"]; !ok || out["truncated"] != false {
		t.Errorf("output: %+v", out)
	}
	confirm := init["confirm"].(map[string]any)
	if confirm["table"] != "log.fresh" || confirm["partition_count"].(float64) != 5 || confirm["first_partition"] != "p20261004" {
		t.Fatalf("%+v", confirm)
	}
	stmt := d["statements"].([]any)[0].(map[string]any)
	if stmt["kind"] != "init" || stmt["danger"] != true || stmt["heavy"] != true {
		t.Fatalf("%+v", stmt)
	}

	// 不带确认：409 confirm_required，data 是重算后的计划
	w = root.Do("POST", "/api/v1/partman/configs/"+id+"/run", map[string]any{"init": map[string]any{"start_mode": "date", "start_date": "2026-10-04"}})
	var er struct {
		Code string
		Data map[string]any
	}
	_ = json.Unmarshal(w.Body.Bytes(), &er)
	if w.Code != 409 || er.Code != "confirm_required" || er.Data["kind"] != "init" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// 带确认：执行
	w = root.Do("POST", "/api/v1/partman/configs/"+id+"/run", map[string]any{
		"init":         map[string]any{"start_mode": "date", "start_date": "2026-10-04"},
		"confirm_init": map[string]any{"table": "log.fresh", "partition_count": 5, "first_partition": "p20261004", "last_partition": "p20261008"},
	})
	var run struct{ Data map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	if w.Code != 200 || run.Data["failed"] != false || run.Data["output"] == nil || run.Data["log_id"] == nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if h.tgt.names("log.fresh") != "p20261004,p20261005,p20261006,p20261007,p20261008" {
		t.Fatal(h.tgt.names("log.fresh"))
	}
	// 状态：已分区
	_, r := call(t, root, "GET", "/api/v1/partman/configs/"+id+"/status?refresh=1", nil)
	var st StatusView
	_ = json.Unmarshal(r.Data, &st)
	if st.State != StateOK {
		t.Fatalf("%+v", st)
	}
}

func TestHTTPOverviewNewFields(t *testing.T) {
	h := newHTTPEnv(t)
	root := h.web.Login("root")
	w := root.Do("GET", "/api/v1/partman/overview", nil)
	var raw struct{ Data map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	for _, k := range []string{"instances", "timezones", "timezone_hint", "notify_on", "can_webhook", "init", "auto", "can_run", "col_types", "periods"} {
		if _, ok := raw.Data[k]; !ok {
			t.Errorf("overview 缺少 %s: %s", k, w.Body.String())
		}
	}
	if raw.Data["can_webhook"] != true || raw.Data["notify_on"] != "always" {
		t.Fatalf("%v", raw.Data)
	}
	ins := raw.Data["instances"].([]any)[0].(map[string]any)
	for _, k := range []string{"id", "name", "db_type", "supported", "dialect", "can_init"} {
		if _, ok := ins[k]; !ok {
			t.Errorf("instances[] 缺少 %s", k)
		}
	}
	hint := raw.Data["timezone_hint"].(map[string]any)
	if hint["max_len"].(float64) != 20 {
		t.Fatalf("%v", hint)
	}
}

func TestHTTPWebhookRoutesAndPermissions(t *testing.T) {
	h := newHTTPEnv(t)
	root := h.web.Login("root")
	bob := h.web.Login("bob")
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/v1/partman/webhooks"}, {"POST", "/api/v1/partman/webhooks"}, {"POST", "/api/v1/partman/webhooks/test"},
		{"PUT", "/api/v1/partman/webhooks/1"}, {"DELETE", "/api/v1/partman/webhooks/1"}, {"POST", "/api/v1/partman/webhooks/1/enable"},
	} {
		if w := h.web.Anonymous(c.m, c.p); w.Code != 401 {
			t.Errorf("未登录 %s %s: %d", c.m, c.p, w.Code)
		}
		h.grant(2, permMenu, permAdd, permDelete) // 只有菜单 / 新增 / 删除，没有 partman.edit
		if code, r := call(t, bob, c.m, c.p, map[string]any{}); code != 403 {
			t.Errorf("没有 partman.edit %s %s 应 403: %d %+v", c.m, c.p, code, r)
		}
	}
	if len(h.repo.webhooks) != 0 || len(h.repo.audits) != 0 {
		t.Fatal("被拒绝的请求不应有任何写入")
	}

	// 新增
	srv := newHookServer(t, 200, replyFor(chDingtalk))
	body := map[string]any{"type": "dingtalk", "webhook_url": srv.URL + "/robot/send?access_token=" + secretToken, "isenabled": true,
		"mention_on": "always", "mention": []string{"13800000000"}, "keyword": "分区", "secret": "SIGN-SECRET-VALUE"}
	w := root.Do("POST", "/api/v1/partman/webhooks", body)
	var one struct{ Data map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &one)
	if w.Code != 200 || one.Data["secret_set"] != true || one.Data["isenabled"] != true || one.Data["mention_on"] != "always" ||
		strings.Contains(w.Body.String(), "SIGN-SECRET-VALUE") || strings.Contains(w.Body.String(), secretToken) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if _, ok := one.Data["secret"]; ok {
		t.Fatal("响应里不得有 secret 字段")
	}
	id := itoa(uint(one.Data["id"].(float64)))
	// 非法主机
	body2 := map[string]any{"type": "slack", "webhook_url": "https://evil.example.com/x"}
	if code, r := call(t, root, "POST", "/api/v1/partman/webhooks", body2); code != 400 || r.Code != "webhook_host_not_allowed" {
		t.Fatalf("%d %+v", code, r)
	}
	if code, r := call(t, root, "POST", "/api/v1/partman/webhooks", body); code != 409 || r.Code != "duplicate" {
		t.Fatalf("%d %+v", code, r)
	}
	// 列表
	w = root.Do("GET", "/api/v1/partman/webhooks", nil)
	var lst struct {
		Data struct {
			Items           []map[string]any `json:"items"`
			NotifyOn        string           `json:"notify_on"`
			NotifyOnOptions []string         `json:"notify_on_options"`
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &lst)
	if w.Code != 200 || len(lst.Data.Items) != 1 || lst.Data.NotifyOn != "always" || len(lst.Data.NotifyOnOptions) != 3 ||
		strings.Contains(w.Body.String(), secretToken) || lst.Data.Items[0]["webhook_url"] == nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// 启停 / 测试发送 / 修改 / 删除
	if code, _ := call(t, root, "POST", "/api/v1/partman/webhooks/"+id+"/enable", map[string]any{}); code != 400 {
		t.Fatalf("缺 enable: %d", code)
	}
	if code, _ := call(t, root, "POST", "/api/v1/partman/webhooks/"+id+"/enable", map[string]any{"enable": false}); code != 200 {
		t.Fatalf("%d", code)
	}
	w = root.Do("POST", "/api/v1/partman/webhooks/test", map[string]any{"id": 1})
	var tr struct{ Data TestResult }
	_ = json.Unmarshal(w.Body.Bytes(), &tr)
	if w.Code != 200 || len(tr.Data.Results) != 1 || !tr.Data.Results[0].OK || srv.count() != 1 || strings.Contains(w.Body.String(), secretToken) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w = root.Do("POST", "/api/v1/partman/webhooks/test", nil); w.Code != 409 {
		t.Fatalf("没有启用的通道: %d %s", w.Code, w.Body.String())
	}
	if code, r := call(t, root, "PUT", "/api/v1/partman/webhooks/"+id, map[string]any{"type": "dingtalk", "isenabled": true, "mention_on": "never", "clear_secret": true}); code != 200 {
		t.Fatalf("%d %+v", code, r)
	}
	if h.repo.webhooks[0].Secret != "" || h.repo.webhooks[0].URL == "" || h.repo.webhooks[0].MentionOn != "never" {
		t.Fatalf("%+v", h.repo.webhooks[0])
	}
	if code, _ := call(t, root, "DELETE", "/api/v1/partman/webhooks/"+id, nil); code != 200 {
		t.Fatalf("%d", code)
	}
	if code, r := call(t, root, "DELETE", "/api/v1/partman/webhooks/"+id, nil); code != 404 || r.Code != "not_found" {
		t.Fatalf("%d %+v", code, r)
	}
	for _, a := range h.repo.audits {
		if strings.Contains(a.ExtraInfo, secretToken) || strings.Contains(a.ExtraInfo, "SIGN-SECRET-VALUE") {
			t.Fatalf("审计里有完整地址 / 密钥: %+v", a)
		}
	}
}

func TestHTTPLogDetailAndOutputDownload(t *testing.T) {
	h := newHTTPEnv(t)
	root := h.web.Login("root")
	bob := h.web.Login("bob")
	h.grant(2, permMenu)
	h.tgt.tables["log.access_log"] = dailyTable("2026-10-06", "2026-10-07")
	in := goodInput()
	in.PreCreate, in.Retain = 3, 0
	cfg := h.addConfig(in)
	res, err := h.svc.Run(ctx, root0Caller(), cfg.ID, RunOptions{})
	if err != nil || res.LogID == 0 {
		t.Fatalf("%+v %v", res, err)
	}
	logID := itoa(res.LogID)

	// 列表只有行数
	_, r := call(t, bob, "GET", "/api/v1/partman/configs/"+itoa(cfg.ID)+"/logs", nil)
	var rows []map[string]any
	_ = json.Unmarshal(r.Data, &rows)
	if len(rows) != 1 || rows[0]["output_lines"].(float64) == 0 || rows[0]["kind"] != "maintenance" {
		t.Fatalf("%s", r.Data)
	}
	if _, has := rows[0]["output"]; has {
		t.Fatal("列表不带完整输出")
	}
	// 明细
	w := bob.Do("GET", "/api/v1/partman/logs/"+logID, nil)
	var d struct{ Data map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	out, _ := d.Data["output"].(map[string]any)
	lines, _ := out["lines"].([]any)
	if w.Code != 200 || len(lines) == 0 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	first := lines[0].(map[string]any)
	for _, k := range []string{"time", "level", "message"} {
		if _, ok := first[k]; !ok {
			t.Errorf("输出行缺少 %s: %+v", k, first)
		}
	}
	if d.Data["scope"] != "config" || d.Data["table"] != "log.access_log" || out["truncated"] != false {
		t.Fatalf("%v", d.Data)
	}
	// 下载：text/plain 附件，文件名 partman_<cnf_id>_<time>.log
	w = bob.Do("GET", "/api/v1/partman/logs/"+logID+"/output", nil)
	cd := w.Header().Get("Content-Disposition")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || !strings.HasPrefix(cd, `attachment; filename="partman_1_20261006_120000.log"`) ||
		!strings.Contains(w.Body.String(), "=== 分区维护开始 ") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body.String())
	}
	// 范围外 / 不存在 / 未登录
	if code, _ := call(t, bob, "GET", "/api/v1/partman/logs/9999/output", nil); code != 404 {
		t.Fatalf("%d", code)
	}
	if code, _ := call(t, bob, "GET", "/api/v1/partman/logs/abc", nil); code != 400 {
		t.Fatalf("%d", code)
	}
	if w := h.web.Anonymous("GET", "/api/v1/partman/logs/"+logID+"/output"); w.Code != 401 {
		t.Fatalf("%d", w.Code)
	}
	// 系统记录：只读用户 403，管理员可下载
	h.svc.writeSystemLog([]traceLine{{T: "2026-10-06 12:00:00.000", L: "INFO", M: "轮次"}}, logItem{Source: SourceAuto, Event: "auto_round", Operator: "system"})
	var sysID uint
	for _, l := range h.repo.logs {
		if l.CnfID == 0 {
			sysID = l.ID
		}
	}
	if code, _ := call(t, bob, "GET", "/api/v1/partman/logs/"+itoa(sysID), nil); code != 403 {
		t.Fatalf("%d", code)
	}
	if w := root.Do("GET", "/api/v1/partman/logs/"+itoa(sysID)+"/output", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "轮次") ||
		!strings.Contains(w.Header().Get("Content-Disposition"), "partman_system_") {
		t.Fatalf("%d %s %v", w.Code, w.Body.String(), w.Header())
	}
	// 全局日志 scope=system
	w = root.Do("GET", "/api/v1/partman/logs?scope=system", nil)
	var gl struct{ Data []map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &gl)
	if w.Code != 200 || len(gl.Data) != 1 || gl.Data[0]["scope"] != "system" || gl.Data[0]["kind"] != "auto_round" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if code, _ := call(t, bob, "GET", "/api/v1/partman/logs?scope=system", nil); code != 403 {
		t.Fatalf("%d", code)
	}
}

func root0Caller() Caller { return root }
