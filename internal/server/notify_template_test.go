// @author: kevinkelly
// @license: Apache Licence
// @file: notify_template_test.go
// @time: 2026/10/28

package server_test

import (
	"encoding/json"
	"strings"
	"testing"
)

const tplBase = "/api/v1/settings/notify/templates"

func tplBody(mut func(m map[string]any)) map[string]any {
	m := map[string]any{
		"event": "sql.finished", "scope": "global", "name": "执行成功 · 简报", "enabled": true,
		"title": "✅ 工单 #{{ticket_id}} 执行成功", "tone": "ok", "icon": "",
		"blocks": []map[string]any{
			{"t": "fields", "rows": []map[string]any{{"l": "工单名", "v": "{{title}}"}, {"l": "耗时", "v": "{{elapsed}}", "short": true}}},
			{"t": "code", "l": "SQL 内容", "v": "{{sql}}", "mode": "mask", "n": 500},
			{"t": "link", "l": "查看详情", "v": "{{url}}", "as": "button"},
		},
		"note": "首次定制",
	}
	if mut != nil {
		mut(m)
	}
	return m
}

// 消息模板只对超级用户开放；目录里有消息、变量与渠道。
func TestNotifyTemplatePermissionsAndCatalog(t *testing.T) {
	e := newApplyEnv(t)
	dev, root := e.login(t, "dev"), e.login(t, "root")
	if w := dev.do("GET", tplBase+"/catalog", nil); w.Code != 403 {
		t.Fatalf("普通用户应 403: %d", w.Code)
	}
	if w := dev.do("PUT", tplBase+"/item", tplBody(nil)); w.Code != 403 {
		t.Fatalf("普通用户不能保存: %d", w.Code)
	}
	w := root.do("GET", tplBase+"/catalog", nil)
	if w.Code != 200 {
		t.Fatalf("目录: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"sql.finished"`, `"sql.submitted"`, `"priv.changed"`, `"archive.archive_done"`, `"key":"sql"`, `"ready":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("目录缺少 %s", want)
		}
	}
}

// 保存前校验：不存在的变量 / 非法范围 / 未知消息被拒绝；合法的保存后能取回、有历史、可删除。
func TestNotifyTemplateSaveGetDelete(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")

	for name, mut := range map[string]func(m map[string]any){
		"未知变量":  func(m map[string]any) { m["title"] = "{{nope}}" },
		"未知消息":  func(m map[string]any) { m["event"] = "sql.nope" },
		"非法范围":  func(m map[string]any) { m["scope"] = "g:1" },
		"渠道不存在": func(m map[string]any) { m["scope"] = "c:9999" },
		"标题为空":  func(m map[string]any) { m["title"] = " " },
		"链接不是 http": func(m map[string]any) {
			m["blocks"] = []map[string]any{{"t": "link", "l": "x", "v": "javascript:alert(1)", "as": "text"}}
		},
	} {
		w := root.do("PUT", tplBase+"/item", tplBody(mut))
		if w.Code != 400 {
			t.Errorf("%s 应 400: %d %s", name, w.Code, w.Body.String())
		}
	}
	if len(e.repo.NotifyTemplates()) != 0 {
		t.Fatalf("被拒绝的模板不应保存")
	}

	// 非法变量的响应带上问题列表
	w := root.do("PUT", tplBase+"/item", tplBody(func(m map[string]any) { m["title"] = "{{nope}}" }))
	if jsonField(t, w.Body.String(), "code") != "invalid_template" || !strings.Contains(w.Body.String(), "不存在的变量") {
		t.Errorf("应返回 invalid_template 与具体问题: %s", w.Body.String())
	}

	// 保存：用了这条消息里没有的变量只是提示，不拦
	w = root.do("PUT", tplBase+"/item", tplBody(func(m map[string]any) { m["title"] = "{{ticket_id}} {{remark}}" }))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "没有变量") {
		t.Fatalf("应保存并提示: %d %s", w.Code, w.Body.String())
	}
	w = root.do("PUT", tplBase+"/item", tplBody(nil))
	if w.Code != 200 {
		t.Fatalf("保存: %d %s", w.Code, w.Body.String())
	}

	w = root.do("GET", tplBase+"/item?event=sql.finished&scope=global", nil)
	if w.Code != 200 || jsonField(t, w.Body.String(), "data", "template") == nil {
		t.Fatalf("取回: %d %s", w.Code, w.Body.String())
	}
	if got := jsonField(t, w.Body.String(), "data", "template").(map[string]any); got["title"] != "✅ 工单 #{{ticket_id}} 执行成功" || got["updated_by"] != "root" {
		t.Errorf("取回的模板: %+v", got)
	}
	// 另一个范围还没有定制：template 为空，parent 是系统默认
	w = root.do("GET", tplBase+"/item?event=sql.finished&scope=global", nil)
	w = root.do("GET", tplBase+"/item?event=sql.passed&scope=global", nil)
	if jsonField(t, w.Body.String(), "data", "template") != nil || jsonField(t, w.Body.String(), "data", "parent_scope") != "default" {
		t.Errorf("没有定制时应沿用系统默认: %s", w.Body.String())
	}

	// 目录摘要与历史
	w = root.do("GET", tplBase+"/catalog", nil)
	if !strings.Contains(w.Body.String(), `"event":"sql.finished"`) {
		t.Errorf("目录里应有已保存模板的摘要: %s", w.Body.String())
	}
	w = root.do("GET", tplBase+"/history?event=sql.finished&scope=global", nil)
	var hist struct {
		Data struct {
			History []map[string]any `json:"history"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &hist)
	if len(hist.Data.History) != 2 || hist.Data.History[0]["note"] != "首次定制" {
		t.Errorf("历史: %s", w.Body.String())
	}

	// 删除：恢复到系统默认，并留下一条删除历史
	if w := root.do("DELETE", tplBase+"/item?event=sql.finished&scope=global&note=不要了", nil); w.Code != 200 {
		t.Fatalf("删除: %d %s", w.Code, w.Body.String())
	}
	w = root.do("GET", tplBase+"/item?event=sql.finished&scope=global", nil)
	if jsonField(t, w.Body.String(), "data", "template") != nil {
		t.Errorf("删除后不应还有模板: %s", w.Body.String())
	}
	w = root.do("GET", tplBase+"/history?event=sql.finished&scope=global", nil)
	if !strings.Contains(w.Body.String(), `"action":"delete"`) {
		t.Errorf("应记一条删除历史: %s", w.Body.String())
	}
}

// 预览：按草稿渲染模拟事件，返回各渠道的原生载荷；SQL 里的字符串常量被隐藏，样例里的虚构密钥不会出现在请求体里。
func TestNotifyTemplatePreview(t *testing.T) {
	e := newApplyEnv(t)
	root := e.login(t, "root")
	for _, typ := range []string{"slack", "dingtalk", "wecom", "feishu", "lark", "telegram", "email"} {
		w := root.do("POST", tplBase+"/preview", tplBody(func(m map[string]any) { m["channel_type"] = typ }))
		if w.Code != 200 {
			t.Fatalf("%s 预览: %d %s", typ, w.Code, w.Body.String())
		}
		body := w.Body.String()
		if strings.Contains(body, "demo-secret-0123456789") {
			t.Errorf("%s：预览里不应出现 SQL 的字符串常量原文: %s", typ, body)
		}
		if !strings.Contains(body, "secret_key = '***'") && !strings.Contains(body, `secret_key = '***'`) && !strings.Contains(body, `secret_key = &#39;***&#39;`) {
			t.Errorf("%s：应能看到被隐藏的常量: %s", typ, body)
		}
	}
	// 草稿还有硬伤：只返回问题，不渲染
	w := root.do("POST", tplBase+"/preview", tplBody(func(m map[string]any) { m["title"] = "{{nope}}"; m["channel_type"] = "slack" }))
	if w.Code != 200 || jsonField(t, w.Body.String(), "data", "preview") != nil || !strings.Contains(w.Body.String(), "不存在的变量") {
		t.Errorf("有硬伤时只返回问题: %d %s", w.Code, w.Body.String())
	}
	// 渠道类型不合法
	if w := root.do("POST", tplBase+"/preview", tplBody(func(m map[string]any) { m["channel_type"] = "sms" })); w.Code != 400 {
		t.Errorf("非法渠道类型应 400: %d", w.Code)
	}
}
