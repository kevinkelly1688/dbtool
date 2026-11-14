// @author: kevinkelly
// @license: Apache Licence
// @file: sql_export_test.go
// @time: 2026/10/28

package server_test

import (
	"strings"
	"testing"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/sqlflow"
)

func TestSQLExportHTTPFlow(t *testing.T) {
	e := newSQLEnv(t)
	dev, leader, other := e.login(t, "dev"), e.login(t, "leader"), e.login(t, "other")
	q := "select id, name from orders where id > 0"

	// 选项与页面
	o := dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/options", nil))
	ex := o["export"].(map[string]any)
	if ex["allowed"] != true || ex["batch_allowed"] != true || len(ex["formats"].([]any)) != 5 || ex["max_rows"].(float64) != 10000 {
		t.Errorf("导出选项: %v", ex)
	}
	if eg := o["groups"].([]any)[0].(map[string]any)["export_instances"].([]any); len(eg) != 3 {
		t.Errorf("可导出实例: %v", eg)
	}
	if lo := dataOf(t, leader.do("GET", "/api/v1/sqlworkflow/options", nil)); lo["export"].(map[string]any)["allowed"] != false {
		t.Error("没有 sqlexport_submit 不应开放导出")
	}
	if p := dev.get("/sqlworkflow/export/new"); p.Code != 200 || !strings.Contains(p.Body.String(), "提交数据导出工单") {
		t.Errorf("导出提交页: %d", p.Code)
	}
	if p := leader.get("/sqlworkflow/export/new"); p.Code != 403 {
		t.Errorf("没有权限的人打开导出提交页: %d", p.Code)
	}

	// 检查：行数统计、非查询语句
	w := dev.do("POST", "/api/v1/sqlworkflow/check", map[string]any{"export": true, "instance_id": 1, "db_name": "shop", "sql": q})
	cd := dataOf(t, w)
	if w.Code != 200 || cd["source"] != "export" || cd["error_count"].(float64) != 0 || cd["syntax_label"] != "离线导出" || !strings.Contains(w.Body.String(), "行数统计完成") {
		t.Fatalf("导出检查: %d %s", w.Code, w.Body.String())
	}
	if cd = dataOf(t, dev.do("POST", "/api/v1/sqlworkflow/check", map[string]any{"export": true, "instance_id": 1, "db_name": "shop", "sql": "delete from orders"})); cd["error_count"].(float64) != 1 {
		t.Errorf("DELETE 不能导出: %v", cd)
	}
	if cd = dataOf(t, dev.do("POST", "/api/v1/sqlworkflow/check", map[string]any{"export": true, "instance_ids": []int{11, 12}, "db_name": "shop", "sql": q})); len(cd["instances"].([]any)) != 2 {
		t.Errorf("批量导出检查: %v", cd)
	}

	// 提交校验
	sub := map[string]any{"name": "导出订单", "instance_id": 1, "db_name": "shop", "group_name": "研发组", "sql": q, "export_format": "csv"}
	if w := leader.do("POST", "/api/v1/sqlworkflow", sub); w.Code != 403 {
		t.Errorf("没有导出权限: %d", w.Code)
	}
	bad := map[string]any{}
	for k, v := range sub {
		bad[k] = v
	}
	bad["export_format"] = "pdf"
	if w := dev.do("POST", "/api/v1/sqlworkflow", bad); w.Code != 400 {
		t.Errorf("格式不合法: %d", w.Code)
	}

	// export:true 但没给格式 → 400（不会被当成普通上线工单）
	noFmt := map[string]any{"name": "x", "instance_id": 1, "db_name": "shop", "group_name": "研发组", "sql": q, "export": true}
	if w := dev.do("POST", "/api/v1/sqlworkflow", noFmt); w.Code != 400 || !strings.Contains(w.Body.String(), "导出格式") {
		t.Errorf("导出必须选格式: %d %s", w.Code, w.Body.String())
	}

	// 提交 → 审批 → 导出 → 下载
	w = dev.do("POST", "/api/v1/sqlworkflow", sub)
	if w.Code != 200 {
		t.Fatalf("提交: %d %s", w.Code, w.Body.String())
	}
	id := itoa(dataOf(t, w)["workflow_id"].(float64))
	dd := dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/"+id, nil))
	if dd["is_export"] != true || dd["export_format"] != "csv" || dd["can_download"] != false {
		t.Errorf("详情: %v", dd)
	}
	if w := dev.do("GET", "/api/v1/sqlworkflow/"+id+"/download", nil); w.Code != 409 {
		t.Errorf("导出前不能下载: %d", w.Code)
	}
	leader.do("POST", "/api/v1/sqlworkflow/"+id+"/pass", map[string]any{"remark": "ok"})
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/execute", map[string]any{"mode": "manual"}); w.Code != 400 {
		t.Errorf("导出不能手工执行: %d %s", w.Code, w.Body.String())
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/execute", map[string]any{"mode": "auto"}); w.Code != 200 {
		t.Fatalf("执行: %d %s", w.Code, w.Body.String())
	}
	dd = dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/"+id, nil))
	if dd["status"] != model.WfFinish || dd["can_download"] != true || !strings.HasSuffix(dd["file_name"].(string), ".zip") {
		t.Fatalf("导出完成后的详情: %v", dd)
	}
	page := dev.get("/sqlworkflow/" + id)
	if !strings.Contains(page.Body.String(), "下载文件") || !strings.Contains(page.Body.String(), "数据导出工单") || strings.Contains(page.Body.String(), "act-manual") {
		t.Error("详情页应显示下载按钮，且没有“手工执行”")
	}

	dl := dev.do("GET", "/api/v1/sqlworkflow/"+id+"/download", nil)
	if dl.Code != 200 || dl.Header().Get("Content-Type") != "application/zip" || !strings.Contains(dl.Header().Get("Content-Disposition"), `attachment; filename="shop_`) ||
		dl.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(dl.Body.String(), "PK") {
		t.Errorf("下载: %d %v", dl.Code, dl.Header())
	}
	if len(e.mem.AuditEntries) != 1 || e.mem.AuditEntries[0].Action != "离线下载" {
		t.Errorf("下载应记审计日志: %+v", e.mem.AuditEntries)
	}
	if w := leader.do("GET", "/api/v1/sqlworkflow/"+id+"/download", nil); w.Code != 403 {
		t.Errorf("没有 offline_download: %d", w.Code)
	}
	if w := other.do("GET", "/api/v1/sqlworkflow/"+id+"/download", nil); w.Code != 403 && w.Code != 404 {
		t.Errorf("无关的人: %d", w.Code)
	}

	// 列表：种类筛选与标记
	if lw := dev.get("/sqlworkflow?kind=export"); !strings.Contains(lw.Body.String(), "导出订单") || !strings.Contains(lw.Body.String(), "csv") {
		t.Error("列表应显示导出工单")
	}
	if lw := dev.do("GET", "/api/v1/sqlworkflow?kind=change", nil); strings.Contains(lw.Body.String(), "导出订单") {
		t.Error("kind=change 不应包含导出工单")
	}
	if lw := dev.get("/sqlworkflow"); !strings.Contains(lw.Body.String(), "/sqlworkflow/export/new") {
		t.Error("列表页应有“数据导出”入口")
	}

	// 批量导出：提交 → 执行 → 下载
	bw := dev.do("POST", "/api/v1/sqlworkflow", map[string]any{"name": "批量导出", "instance_ids": []int{11, 12}, "db_name": "shop", "group_name": "研发组", "sql": q, "export_format": "xlsx"})
	if bw.Code != 200 {
		t.Fatalf("批量导出提交: %d %s", bw.Code, bw.Body.String())
	}
	bid := itoa(dataOf(t, bw)["workflow_id"].(float64))
	leader.do("POST", "/api/v1/sqlworkflow/"+bid+"/pass", map[string]any{"remark": "ok"})
	dev.do("POST", "/api/v1/sqlworkflow/"+bid+"/execute", map[string]any{"mode": "auto"})
	if dl := dev.do("GET", "/api/v1/sqlworkflow/"+bid+"/download", nil); dl.Code != 200 || !strings.HasPrefix(dl.Body.String(), "PK") {
		t.Errorf("批量导出下载: %d", dl.Code)
	}
}

// 导出文件被保留策略清理后：详情页隐藏顶部下载链接并写明清理时间，下载接口返回 410（含清理时间）而不是通用 404。
func TestSQLExportPurgedByRetention(t *testing.T) {
	e := newSQLEnv(t)
	dev, leader := e.login(t, "dev"), e.login(t, "leader")
	sub := map[string]any{"name": "导出订单", "instance_id": 1, "db_name": "shop", "group_name": "研发组", "sql": "select id, name from orders where id > 0", "export_format": "csv"}
	w := dev.do("POST", "/api/v1/sqlworkflow", sub)
	if w.Code != 200 {
		t.Fatalf("提交: %d %s", w.Code, w.Body.String())
	}
	id := itoa(dataOf(t, w)["workflow_id"].(float64))
	leader.do("POST", "/api/v1/sqlworkflow/"+id+"/pass", map[string]any{"remark": "ok"})
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/execute", map[string]any{"mode": "auto"}); w.Code != 200 {
		t.Fatalf("执行: %d %s", w.Code, w.Body.String())
	}
	name := dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/"+id, nil))["file_name"].(string)
	if body := dev.get("/sqlworkflow/" + id).Body.String(); strings.Count(body, "/download") != 1 || strings.Contains(body, "file-card") {
		t.Fatalf("清理前只有“导出文件”一行里的一个下载链接，不再有结果文件卡片: %d", strings.Count(body, "/download"))
	}
	if err := e.xstore.Delete(name); err != nil {
		t.Fatal(err)
	}
	purgedAt := time.Date(2026, 9, 1, 3, 4, 5, 0, time.Local)
	sqlflow.SetExportPurgedLookup(func(uint) (time.Time, bool) { return purgedAt, true })
	t.Cleanup(func() { sqlflow.SetExportPurgedLookup(nil) })

	body := dev.get("/sqlworkflow/" + id).Body.String()
	if strings.Contains(body, "/download") {
		t.Error("文件已清理：详情页不应再有下载链接")
	}
	if !strings.Contains(body, "文件已于 2026-09-01 03:04:05 按保留策略清理，请重新提交导出") {
		t.Error("详情页应写明清理时间和处理办法")
	}
	dl := dev.do("GET", "/api/v1/sqlworkflow/"+id+"/download", nil)
	if dl.Code != 410 || !strings.Contains(dl.Body.String(), "2026-09-01 03:04:05") || !strings.Contains(dl.Body.String(), "export_purged") {
		t.Errorf("下载应返回 410 和清理时间: %d %s", dl.Code, dl.Body.String())
	}
}

// 数据导出：实例 / 库列表只显示有查询权限的；提交时校验表权限；SQL 上线的列表不受影响。
func TestSQLExportOnlyPermittedInstancesDatabasesAndTables(t *testing.T) {
	e := newSQLEnv(t)
	// dev 在实例 12 上没有权限：回收；实例 1 只有 shop.orders 的表权限
	var keep []*model.QueryPrivilege
	for _, p := range e.repo.Privs {
		if p.InstanceID != 12 {
			keep = append(keep, p)
		}
	}
	e.repo.Privs = keep
	dev := e.login(t, "dev")

	o := dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/options", nil))
	g := o["groups"].([]any)[0].(map[string]any)
	ids := map[float64]bool{}
	for _, i := range g["export_instances"].([]any) {
		ids[i.(map[string]any)["id"].(float64)] = true
	}
	if len(ids) != 2 || !ids[1] || !ids[11] || ids[12] {
		t.Errorf("导出实例只应有 1 和 11: %v", ids)
	}
	if n := len(g["instances"].([]any)); n != 3 {
		t.Errorf("上线实例列表不应受影响: %d", n)
	}
	// 库列表：导出页（export=1）只有 shop；上线页是全部
	w := dev.do("GET", "/api/v1/sqlworkflow/instances/1/databases?export=1", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"shop"`) || strings.Contains(w.Body.String(), "report") {
		t.Errorf("导出库列表只应有 shop: %d %s", w.Code, w.Body.String())
	}
	if all := dev.do("GET", "/api/v1/sqlworkflow/instances/1/databases", nil); all.Code != 200 || !strings.Contains(all.Body.String(), "report") {
		t.Errorf("上线库列表不应被过滤: %d %s", all.Code, all.Body.String())
	}
	// 检查 / 提交：没权限的表被拒绝
	bad := dev.do("POST", "/api/v1/sqlworkflow/check", map[string]any{"export": true, "instance_id": 1, "db_name": "shop", "sql": "select id from secrets where id > 0"})
	if cd := dataOf(t, bad); cd["error_count"].(float64) != 1 || !strings.Contains(bad.Body.String(), "shop.secrets") {
		t.Errorf("检查应报没有表权限: %s", bad.Body.String())
	}
	sub := map[string]any{"name": "导出", "instance_id": 1, "db_name": "shop", "group_name": "研发组", "sql": "select id from secrets where id > 0", "export_format": "csv"}
	if w := dev.do("POST", "/api/v1/sqlworkflow", sub); w.Code != 403 || !strings.Contains(w.Body.String(), "shop.secrets") {
		t.Errorf("提交应被拒绝: %d %s", w.Code, w.Body.String())
	}
	sub["sql"] = "select id, name from orders where id > 0"
	if w := dev.do("POST", "/api/v1/sqlworkflow", sub); w.Code != 200 {
		t.Errorf("有表权限应能提交: %d %s", w.Code, w.Body.String())
	}
}

// 导出原值（不脱敏）只有超级用户能选：普通用户带这个字段提交是 403，提交页选项里也没有它。
func TestSQLExportUnmaskedIsSuperuserOnly(t *testing.T) {
	e := newSQLEnv(t)
	dev := e.login(t, "dev")
	q := "select id, name from orders where id > 0"
	sub := map[string]any{"name": "导出订单", "instance_id": 1, "db_name": "shop", "group_name": "研发组", "sql": q, "export_format": "csv",
		"export_unmasked": true, "export_unmask_reason": "法务取证需要完整的原始数据"}
	if w := dev.do("POST", "/api/v1/sqlworkflow", sub); w.Code != 403 || !strings.Contains(w.Body.String(), "超级用户") {
		t.Errorf("普通用户选导出原值应 403：%d %s", w.Code, w.Body.String())
	}
	o := dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/options", nil))
	if ex := o["export"].(map[string]any); ex["can_unmask"] != false {
		t.Errorf("普通用户的提交选项不应有导出原值：%v", ex)
	}
	// 没带这个字段的提交照常
	delete(sub, "export_unmasked")
	delete(sub, "export_unmask_reason")
	if w := dev.do("POST", "/api/v1/sqlworkflow", sub); w.Code != 200 {
		t.Errorf("普通导出提交应不受影响：%d %s", w.Code, w.Body.String())
	}
}

// 选了“导出原值”的导出工单：详情页显示标记与理由（提交人现在不是超级用户时还提示选择已不生效），列表上有标签；没选的没有。
func TestSQLExportUnmaskedShownOnDetailAndList(t *testing.T) {
	e := newSQLEnv(t)
	dev := e.login(t, "dev")
	q := "select id, name from orders where id > 0"
	sub := map[string]any{"name": "导出订单", "instance_id": 1, "db_name": "shop", "group_name": "研发组", "sql": q, "export_format": "csv"}
	picked := itoa(dataOf(t, dev.do("POST", "/api/v1/sqlworkflow", sub))["workflow_id"].(float64))
	plain := itoa(dataOf(t, dev.do("POST", "/api/v1/sqlworkflow", sub))["workflow_id"].(float64))
	var pid uint
	for _, c := range picked {
		pid = pid*10 + uint(c-'0')
	}
	// 直接写扩展表，模拟超级用户提交时的选择（提交时的校验在 sqlflow 的测试里覆盖）；dev 不是超级用户，所以选择已不生效
	e.mem.ExportExts = map[uint]model.SqlExportExt{pid: {WorkflowID: pid, Unmasked: true, Reason: "法务取证需要完整的原始数据", SetBy: "root", CreatedAt: time.Now()}}
	page := dev.get("/sqlworkflow/" + picked).Body.String()
	for _, want := range []string{"导出原值", "法务取证需要完整的原始数据", "不脱敏", "root", "提交人现在已不是超级用户，执行时不会导出原值"} {
		if !strings.Contains(page, want) {
			t.Errorf("详情页应含 %q", want)
		}
	}
	if strings.Contains(dev.get("/sqlworkflow/"+plain).Body.String(), "导出原值") {
		t.Error("没选导出原值的工单详情不应出现这个标记")
	}
	var detail map[string]any
	detail = dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/"+picked, nil))
	if eu, ok := detail["export_unmasked"].(map[string]any); !ok || eu["reason"] != "法务取证需要完整的原始数据" || eu["active"] != false {
		t.Errorf("详情接口应带 export_unmasked：%v", detail["export_unmasked"])
	}
	list := dev.get("/sqlworkflow?kind=export").Body.String()
	if strings.Count(list, "sf-warn") != 1 || !strings.Contains(list, "导出原值") {
		t.Errorf("列表里只有选了导出原值的那张有标签：%d", strings.Count(list, "sf-warn"))
	}
}

// 批量导出“合并成一个文件”：提交页有这个选项，接口带 export_merge，详情页和详情接口显示它；单实例导出不能选。
func TestSQLExportMergeHTTP(t *testing.T) {
	e := newSQLEnv(t)
	dev := e.login(t, "dev")
	q := "select id, name from orders where id > 0"

	o := dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/options", nil))
	if ex := o["export"].(map[string]any); ex["can_merge"] != true {
		t.Errorf("提交选项应提供合并：%v", ex)
	}
	if p := dev.get("/sqlworkflow/export/new").Body.String(); !strings.Contains(p, `id="wf-merge"`) || strings.Contains(p, "wf-merge-col") {
		t.Error("导出提交页应有合并选项，没有列名输入框")
	}
	// 页面检查带合并标记
	if w := dev.do("POST", "/api/v1/sqlworkflow/check", map[string]any{"export": true, "export_merge": true, "instance_ids": []int{11, 12}, "db_name": "shop", "sql": q}); w.Code != 200 {
		t.Errorf("带合并标记的检查: %d %s", w.Code, w.Body.String())
	}
	// 单实例导出不能合并
	one := map[string]any{"name": "导出", "instance_id": 1, "db_name": "shop", "group_name": "研发组", "sql": q, "export_format": "csv", "export_merge": true}
	if w := dev.do("POST", "/api/v1/sqlworkflow", one); w.Code != 400 || !strings.Contains(w.Body.String(), "只有批量导出可以合并成一个文件") {
		t.Errorf("单实例导出选合并应 400：%d %s", w.Code, w.Body.String())
	}
	// 批量导出：选合并
	sub := map[string]any{"name": "批量导出", "instance_ids": []int{11, 12}, "db_name": "shop", "group_name": "研发组", "sql": q, "export_format": "csv", "export_merge": true}
	w := dev.do("POST", "/api/v1/sqlworkflow", sub)
	if w.Code != 200 {
		t.Fatalf("批量合并导出提交: %d %s", w.Code, w.Body.String())
	}
	id := itoa(dataOf(t, w)["workflow_id"].(float64))
	var wid uint
	for _, c := range id {
		wid = wid*10 + uint(c-'0')
	}
	if x := e.mem.ExportExts[wid]; !x.MergeFiles || x.Unmasked {
		t.Errorf("扩展记录: %+v", x)
	}
	page := dev.get("/sqlworkflow/" + id).Body.String()
	for _, want := range []string{"合并导出", "合并成一个文件", "不含实例名称列"} {
		if !strings.Contains(page, want) {
			t.Errorf("详情页应含 %q", want)
		}
	}
	if dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/"+id, nil))["export_merge"] != true {
		t.Error("详情接口应带 export_merge: true")
	}
	// 没选合并的批量导出：详情页没有这个标记
	delete(sub, "export_merge")
	plain := itoa(dataOf(t, dev.do("POST", "/api/v1/sqlworkflow", sub))["workflow_id"].(float64))
	if strings.Contains(dev.get("/sqlworkflow/"+plain).Body.String(), "合并成一个文件") {
		t.Error("没选合并的工单详情不应有合并标记")
	}
}
