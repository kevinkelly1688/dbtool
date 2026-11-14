// @author: kevinkelly
// @license: Apache Licence
// @file: sql_extra_test.go
// @time: 2026/10/28

package server_test

import (
	"strings"
	"testing"
	"time"

	"dbtool/internal/model"
)

func TestSQLListPageKPITabsFiltersAndSort(t *testing.T) {
	e := newSQLEnv(t)
	dev, leader, other := e.login(t, "dev"), e.login(t, "leader"), e.login(t, "other")
	idOf := func(name, sqlText string) string {
		b := submitBody(sqlText)
		b["name"] = name
		w := dev.do("POST", "/api/v1/sqlworkflow", b)
		if w.Code != 200 {
			t.Fatalf("提交 %s: %d %s", name, w.Code, w.Body.String())
		}
		return itoa(dataOf(t, w)["workflow_id"].(float64))
	}
	a := idOf("alpha-索引", "update orders set a=1 where id=1")
	b := idOf("beta-清理", "update orders set a=2 where id=2")

	// 轮到组长审批：页面有状态卡片、页签计数、资源组列、审批进度列（流转说明已移到使用文档，页面上不再有）
	body := leader.get("/sqlworkflow").Body.String()
	for _, want := range []string{"sf-kpis", "待我审批", "当前审批级", "研发组", "第 1 / 1 级 · 组长", "轮到你审批", "sf-filter"} {
		if !strings.Contains(body, want) {
			t.Errorf("组长列表页缺少 %q", want)
		}
	}
	if strings.Contains(body, "sf-help") {
		t.Error("列表页不应再带“流转说明”折叠面板")
	}
	if w := leader.get("/sqlworkflow?tab=approve"); !strings.Contains(w.Body.String(), "alpha-索引") || !strings.Contains(w.Body.String(), "beta-清理") {
		t.Error("待我审批页签应包含两张工单")
	}
	if w := dev.get("/sqlworkflow?tab=approve"); strings.Contains(w.Body.String(), "alpha-索引") {
		t.Error("提交人没有审批权限，待我审批页签应为空")
	}
	if w := dev.get("/sqlworkflow?tab=mine"); !strings.Contains(w.Body.String(), "alpha-索引") {
		t.Error("我提交的页签")
	}
	if w := leader.get("/sqlworkflow?tab=mine"); strings.Contains(w.Body.String(), "alpha-索引") {
		t.Error("组长没提交过")
	}
	// 搜索（工单号 / 名称 / 库 / 实例名）、筛选、排序参数
	if w := dev.get("/sqlworkflow?q=%23" + b); !strings.Contains(w.Body.String(), "beta-清理") || strings.Contains(w.Body.String(), "alpha-索引") {
		t.Error("按 #工单号 搜索")
	}
	if w := dev.get("/sqlworkflow?q=shop"); !strings.Contains(w.Body.String(), "alpha-索引") {
		t.Error("按库搜索")
	}
	if w := dev.get("/sqlworkflow?q=prod-mysql-01&inst=1&rg=1&status=workflow_manreviewing&sort=name&dir=asc"); !strings.Contains(w.Body.String(), "alpha-索引") {
		t.Error("组合筛选 + 排序")
	}
	asc := dev.get("/sqlworkflow?sort=name&dir=asc").Body.String()
	if strings.Index(asc, "alpha-索引") > strings.Index(asc, "beta-清理") {
		t.Error("名称升序 alpha 应在 beta 前")
	}
	desc := dev.get("/sqlworkflow?sort=name&dir=desc").Body.String()
	if strings.Index(desc, "alpha-索引") < strings.Index(desc, "beta-清理") {
		t.Error("名称倒序 beta 应在 alpha 前")
	}
	// 非法参数不影响页面
	if w := dev.get("/sqlworkflow?sort=id%3B+drop&status=bogus&inst=abc&tab=zzz&batch=9"); w.Code != 200 || !strings.Contains(w.Body.String(), "alpha-索引") {
		t.Errorf("非法参数: %d", w.Code)
	}
	if w := dev.do("GET", "/api/v1/sqlworkflow?tab=mine&sort=name&dir=asc", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"approval_level"`) || !strings.Contains(w.Body.String(), `"group_name":"研发组"`) {
		t.Errorf("JSON 列表: %s", w.Body.String())
	}
	if w := other.get("/sqlworkflow"); w.Code != 403 {
		t.Errorf("没有菜单权限: %d", w.Code)
	}
	_ = a
}

func TestSQLFlowPreviewCopyAndCancelScheduleHTTP(t *testing.T) {
	e := newSQLEnv(t)
	dev, leader, other := e.login(t, "dev"), e.login(t, "leader"), e.login(t, "other")

	// 审批流预览
	w := dev.do("GET", "/api/v1/sqlworkflow/flow-preview?group_id=1", nil)
	d := dataOf(t, w)
	lv := d["levels"].([]any)
	// 页面只显示审批组，可审批人员在鼠标悬停时显示，所以接口仍然返回每级的可审批人与人数
	if w.Code != 200 || d["configured"] != true || len(lv) != 1 || lv[0].(map[string]any)["group_name"] != "组长" ||
		lv[0].(map[string]any)["approvers"].([]any)[0].(map[string]any)["username"] != "leader" || lv[0].(map[string]any)["approver_count"] != float64(1) {
		t.Fatalf("审批流预览: %d %s", w.Code, w.Body.String())
	}
	if w := dev.do("GET", "/api/v1/sqlworkflow/flow-preview", nil); w.Code != 400 {
		t.Errorf("缺参数: %d", w.Code)
	}
	if w := other.do("GET", "/api/v1/sqlworkflow/flow-preview?group_id=99", nil); w.Code != 403 {
		t.Errorf("不在资源组内: %d", w.Code)
	}

	// 提交 → 复制（审批中的不能复制；撤回后可以）
	w = dev.do("POST", "/api/v1/sqlworkflow", submitBody("update orders set a=1 where id=1"))
	id := itoa(dataOf(t, w)["workflow_id"].(float64))
	if w := dev.do("GET", "/api/v1/sqlworkflow/"+id+"/copy", nil); w.Code != 409 {
		t.Errorf("审批中的工单不能复制: %d", w.Code)
	}
	if p := dev.get("/sqlworkflow/" + id); !strings.Contains(p.Body.String(), "sf-steps") || !strings.Contains(p.Body.String(), `title="可审批人员（1）：`) || strings.Contains(p.Body.String(), "复制为新工单") {
		t.Error("详情页应显示审批时间线，可审批人员放在悬停提示里，审批中没有复制按钮")
	}
	if p := dev.get("/sqlworkflow/" + id); strings.Contains(p.Body.String(), "现在可审批") || strings.Contains(p.Body.String(), `class="chip"`) {
		t.Error("时间线上不再列出人员名字，只在悬停提示里显示")
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/cancel", map[string]any{"remark": "撤回重提"}); w.Code != 200 {
		t.Fatalf("撤回: %d %s", w.Code, w.Body.String())
	}
	cd := dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/"+id+"/copy", nil))
	if cd["name"] != "给 orders 加索引" || cd["db_name"] != "shop" || cd["group_name"] != "研发组" || cd["sql"] != "update orders set a=1 where id=1" {
		t.Errorf("复制内容: %v", cd)
	}
	if p := dev.get("/sqlworkflow/" + id); !strings.Contains(p.Body.String(), "复制为新工单") || !strings.Contains(p.Body.String(), "?copy="+id) {
		t.Error("终态工单应有复制按钮")
	}
	if w := other.do("GET", "/api/v1/sqlworkflow/"+id+"/copy", nil); w.Code != 404 {
		t.Errorf("看不到的工单: %d", w.Code)
	}
	if p := dev.get("/sqlworkflow/new?copy=" + id); p.Code != 200 {
		t.Errorf("提交页带 copy 参数: %d", p.Code)
	}

	// 定时 → 取消定时
	w = dev.do("POST", "/api/v1/sqlworkflow", submitBody("update orders set a=2 where id=2"))
	id2 := itoa(dataOf(t, w)["workflow_id"].(float64))
	leader.do("POST", "/api/v1/sqlworkflow/"+id2+"/pass", map[string]any{"remark": "ok"})
	at := time.Now().Add(2 * time.Hour).Format("2006-01-02 15:04")
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id2+"/schedule", map[string]any{"run_at": at}); w.Code != 200 {
		t.Fatalf("定时: %d %s", w.Code, w.Body.String())
	}
	if p := dev.get("/sqlworkflow/" + id2); !strings.Contains(p.Body.String(), "act-sched-cancel") {
		t.Error("定时执行的工单应有取消定时按钮")
	}
	if w := other.do("POST", "/api/v1/sqlworkflow/"+id2+"/schedule/cancel", map[string]any{}); w.Code != 404 && w.Code != 403 {
		t.Errorf("无关的人取消定时: %d", w.Code)
	}
	w = dev.do("POST", "/api/v1/sqlworkflow/"+id2+"/schedule/cancel", map[string]any{})
	if w.Code != 200 || dataOf(t, w)["status"] != model.WfReviewPass {
		t.Fatalf("取消定时: %d %s", w.Code, w.Body.String())
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id2+"/schedule/cancel", map[string]any{}); w.Code != 409 {
		t.Errorf("重复取消: %d", w.Code)
	}

	// 进度
	pd := dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/"+id2+"/progress", nil))
	if pd["status"] != model.WfReviewPass || pd["active"] != false || pd["percent"].(float64) != -1 {
		t.Errorf("进度: %v", pd)
	}
	if w := other.do("GET", "/api/v1/sqlworkflow/"+id2+"/progress", nil); w.Code != 404 {
		t.Errorf("看不到的工单进度: %d", w.Code)
	}
}

func TestSQLExportListPageAndPlanHTTP(t *testing.T) {
	e := newSQLEnv(t)
	dev, leader := e.login(t, "dev"), e.login(t, "leader")
	q := "select id, name from orders where id > 0"
	sub := map[string]any{"name": "导出订单", "instance_id": 1, "db_name": "shop", "group_name": "研发组", "sql": q, "export_format": "csv"}
	at := time.Now().Add(3 * time.Hour).Format("2006-01-02T15:04")
	sub["plan_run_at"] = at
	w := dev.do("POST", "/api/v1/sqlworkflow", sub)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "自动定时执行") {
		t.Fatalf("带计划的导出提交: %d %s", w.Code, w.Body.String())
	}
	id := itoa(dataOf(t, w)["workflow_id"].(float64))
	dev.do("POST", "/api/v1/sqlworkflow", submitBody("update orders set a=1 where id=1")) // 一张上线工单

	// 没有 export.view：不能打开导出列表
	if p := dev.get("/sqlworkflow/export"); p.Code != 302 || p.Header().Get("Location") != "/sqlworkflow/export/new" {
		t.Errorf("只有提交权限：菜单入口带到提交页: %d %s", p.Code, p.Header().Get("Location"))
	}
	if p := leader.get("/sqlworkflow/export"); p.Code != 403 {
		t.Errorf("既不能提交也不能查看: %d", p.Code)
	}
	e.mem.Perms[1]["export.view"] = true
	p := dev.get("/sqlworkflow/export")
	body := p.Body.String()
	if p.Code != 200 || !strings.Contains(body, "导出订单") || strings.Contains(body, "给 orders 加索引") || !strings.Contains(body, "行数") || !strings.Contains(body, "只看待我审批") || !strings.Contains(body, "⏱") {
		t.Fatalf("导出列表: %d\n%s", p.Code, body)
	}
	if strings.Contains(body, "批量导出</a>") {
		t.Error("只有 export.view 时不应出现批量页签")
	}
	// 批量导出工单：只有 export.view 的人看不到，URL 参数也绕不过去
	bw := dev.do("POST", "/api/v1/sqlworkflow", map[string]any{"name": "批量导出单", "instance_ids": []int{11, 12}, "db_name": "shop", "group_name": "研发组", "sql": q, "export_format": "csv"})
	if bw.Code != 200 {
		t.Fatalf("批量导出提交: %d %s", bw.Code, bw.Body.String())
	}
	for _, path := range []string{"/sqlworkflow/export", "/sqlworkflow/export?batch=1", "/sqlworkflow/export?tab=mine"} {
		if p := dev.get(path); !strings.Contains(p.Body.String(), "导出订单") || strings.Contains(p.Body.String(), "批量导出单") {
			t.Errorf("只有 export.view：%s 不应出现批量导出工单", path)
		}
	}
	e.mem.Perms[1]["export.view_batch"] = true
	if p := dev.get("/sqlworkflow/export?batch=1"); !strings.Contains(p.Body.String(), "批量导出单") || strings.Contains(p.Body.String(), "导出订单</a>") {
		t.Error("两种权限都有时，batch=1 只看批量导出")
	}
	delete(e.mem.Perms[1], "export.view_batch")
	// 详情：计划、文件前的进度
	if dp := dev.get("/sqlworkflow/" + id); !strings.Contains(dp.Body.String(), "计划定时") {
		t.Error("待审批导出工单应显示提交时设定的计划")
	}
	leader.do("POST", "/api/v1/sqlworkflow/"+id+"/pass", map[string]any{"remark": "ok"})
	if dd := dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/"+id, nil)); dd["status"] != model.WfTimingTask {
		t.Errorf("审批通过后转为定时: %v", dd["status"])
	}
	// 非法计划时间
	sub["plan_run_at"] = "明天"
	if w := dev.do("POST", "/api/v1/sqlworkflow", sub); w.Code != 400 {
		t.Errorf("计划时间格式: %d", w.Code)
	}
	sub["plan_run_at"] = time.Now().Add(-time.Hour).Format("2006-01-02T15:04")
	if w := dev.do("POST", "/api/v1/sqlworkflow", sub); w.Code != 400 {
		t.Errorf("计划时间在过去: %d", w.Code)
	}
}

func TestSQLDetailPageProgressCardWhileExecuting(t *testing.T) {
	e := newSQLEnv(t)
	dev := e.login(t, "dev")
	w := dev.do("POST", "/api/v1/sqlworkflow", submitBody("update orders set a=1 where id=1"))
	id := dataOf(t, w)["workflow_id"].(float64)
	wf := e.mem.Workflows[uint(id)]
	wf.Status = model.WfExecuting
	e.mem.Workflows[uint(id)] = wf
	body := dev.get("/sqlworkflow/" + itoa(id)).Body.String()
	if !strings.Contains(body, `id="progress-card"`) || !strings.Contains(body, "pg-bar") {
		t.Error("执行中的工单应显示进度条")
	}
	pd := dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/"+itoa(id)+"/progress", nil))
	if pd["active"] != true || pd["status"] != model.WfExecuting {
		t.Errorf("进度: %v", pd)
	}
}

// 进程失联后工单停在“排队中”：详情页给出“标记为执行异常”，接口按终止类按钮 + 服务层权限判断，不重试执行。
func TestSQLMarkExceptionHTTP(t *testing.T) {
	e := newSQLEnv(t)
	dev, leader, other := e.login(t, "dev"), e.login(t, "leader"), e.login(t, "other")
	w := dev.do("POST", "/api/v1/sqlworkflow", submitBody("update orders set a=1 where id=1"))
	id := itoa(dataOf(t, w)["workflow_id"].(float64))
	if w := leader.do("POST", "/api/v1/sqlworkflow/"+id+"/pass", map[string]any{"remark": "ok"}); w.Code != 200 {
		t.Fatalf("审批: %d %s", w.Code, w.Body.String())
	}
	// 模拟“执行请求被接受，但进程在执行协程启动前被杀”
	e.svc.SetSpawn(func(func()) {})
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/execute", map[string]any{"mode": "auto"}); w.Code != 200 {
		t.Fatalf("执行: %d %s", w.Code, w.Body.String())
	}
	if p := dev.get("/sqlworkflow/" + id); !strings.Contains(p.Body.String(), "act-mark-exception") {
		t.Error("排队中的工单应有“标记为执行异常”入口")
	}
	if d := dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/"+id, nil)); d["can_mark_exception"] != true {
		t.Errorf("详情 JSON: %v", d["can_mark_exception"])
	}
	if w := other.do("POST", "/api/v1/sqlworkflow/"+id+"/mark-exception", map[string]any{"remark": "x"}); w.Code != 404 && w.Code != 403 {
		t.Errorf("无关的人: %d", w.Code)
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/mark-exception", map[string]any{}); w.Code != 400 {
		t.Errorf("原因必填: %d", w.Code)
	}
	w = dev.do("POST", "/api/v1/sqlworkflow/"+id+"/mark-exception", map[string]any{"remark": "服务被重启"})
	if w.Code != 200 || dataOf(t, w)["status"] != model.WfException {
		t.Fatalf("标记: %d %s", w.Code, w.Body.String())
	}
	if len(e.fake.ran) != 0 {
		t.Errorf("不能重试执行: %v", e.fake.ran)
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/mark-exception", map[string]any{"remark": "再来"}); w.Code != 409 {
		t.Errorf("重复标记: %d", w.Code)
	}
	if p := dev.get("/sqlworkflow/" + id); strings.Contains(p.Body.String(), "act-mark-exception") {
		t.Error("已标记后不再显示入口")
	}
}

// 取消执行：只有 DBA / 超级用户（“取消执行”按钮权限）能用；不在执行中、原因没填都有明确的拒绝。
func TestSQLCancelRunHTTPPermissionsAndValidation(t *testing.T) {
	e := newSQLEnv(t)
	dev, root := e.login(t, "dev"), e.login(t, "dbauser")
	w := dev.do("POST", "/api/v1/sqlworkflow", submitBody("update orders set a=1 where id=1"))
	id := itoa(dataOf(t, w)["workflow_id"].(float64))

	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/cancel-run", map[string]any{"remark": "想停"}); w.Code != 403 {
		t.Errorf("提交人没有“取消执行”按钮权限应 403: %d %s", w.Code, w.Body.String())
	}
	if w := root.do("POST", "/api/v1/sqlworkflow/"+id+"/cancel-run", map[string]any{}); w.Code != 400 {
		t.Errorf("原因必填: %d %s", w.Code, w.Body.String())
	}
	if w := root.do("POST", "/api/v1/sqlworkflow/"+id+"/cancel-run", map[string]any{"remark": "太久了"}); w.Code != 409 {
		t.Errorf("不在执行中应 409: %d %s", w.Code, w.Body.String())
	}
	// 详情页没有取消按钮（只有执行中且有权限才出现）
	if p := root.get("/sqlworkflow/" + id); strings.Contains(p.Body.String(), "act-cancel-run") {
		t.Error("没在执行的工单不应显示“取消执行”")
	}
}
