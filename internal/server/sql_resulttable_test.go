// @author: kevinkelly
// @license: Apache Licence
// @file: sql_resulttable_test.go
// @time: 2026/10/28

package server_test

import (
	"regexp"
	"strings"
	"testing"
)

var (
	colgroupRe = regexp.MustCompile(`(?s)<colgroup>(.*?)</colgroup>`)
	colClassRe = regexp.MustCompile(`<col class="c-([a-z]+)">`)
	dataCRe    = regexp.MustCompile(`data-c="([a-z]+)"`)
	theadRe    = regexp.MustCompile(`(?s)<thead>(.*?)</thead>`)
	tbodyRowRe = regexp.MustCompile(`(?s)<tbody>\s*<tr[^>]*>(.*?)</tr>`)
)

// 结果表（详情页的“检查结果 / 执行结果”）是固定列宽：<colgroup> 里每一列的类名决定宽度（sqlflow.css），
// 表头和每一行的单元格带 data-c，detail.js 按它决定哪些列折叠。三者的列必须一一对应（顺序也一样），否则折叠会套错列。
func resultTableCols(t *testing.T, page string) (cols, heads, cells []string, class string) {
	t.Helper()
	m := regexp.MustCompile(`<table class="([^"]*)" id="result-table">`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("详情页里没有 #result-table: %.200s", page)
	}
	class = m[1]
	cg := colgroupRe.FindStringSubmatch(page)
	th := theadRe.FindStringSubmatch(page)
	tb := tbodyRowRe.FindStringSubmatch(page)
	if cg == nil || th == nil || tb == nil {
		t.Fatalf("结果表缺少 colgroup / thead / 数据行: %v %v %v", cg != nil, th != nil, tb != nil)
	}
	for _, x := range colClassRe.FindAllStringSubmatch(cg[1], -1) {
		cols = append(cols, x[1])
	}
	for _, x := range dataCRe.FindAllStringSubmatch(th[1], -1) {
		heads = append(heads, x[1])
	}
	for _, x := range dataCRe.FindAllStringSubmatch(tb[1], -1) {
		cells = append(cells, x[1])
	}
	return
}

func TestSQLDetailResultTableColumns(t *testing.T) {
	e := newSQLEnv(t)
	dev := e.login(t, "dev")

	// 单实例工单：没有“实例”列
	w := dev.do("POST", "/api/v1/sqlworkflow", submitBody("update orders set a=1 where id=1"))
	id := itoa(dataOf(t, w)["workflow_id"].(float64))
	cols, heads, cells, class := resultTableCols(t, dev.get("/sqlworkflow/"+id).Body.String())
	want := []string{"id", "stage", "lv", "msg", "sql", "aff"}
	for name, got := range map[string][]string{"colgroup": cols, "表头": heads, "数据行": cells} {
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("单实例工单的%s列 = %v，想要 %v", name, got, want)
		}
	}
	if strings.Contains(class, "rt-inst") {
		t.Errorf("单实例工单不应有 rt-inst: %q", class)
	}

	// 批量工单：多一列“实例”，表格带 rt-inst（最小宽度要把它算进去）
	w = dev.do("POST", "/api/v1/sqlworkflow", map[string]any{"name": "批量", "instance_ids": []int{11, 12}, "db_name": "shop", "group_name": "研发组", "sql": "update dbo.Orders set status = 1 where id = 5"})
	if w.Code != 200 {
		t.Fatalf("提交批量: %d %s", w.Code, w.Body.String())
	}
	bid := itoa(dataOf(t, w)["workflow_id"].(float64))
	cols, heads, cells, class = resultTableCols(t, dev.get("/sqlworkflow/"+bid).Body.String())
	want = []string{"id", "inst", "stage", "lv", "msg", "sql", "aff"}
	for name, got := range map[string][]string{"colgroup": cols, "表头": heads, "数据行": cells} {
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("批量工单的%s列 = %v，想要 %v", name, got, want)
		}
	}
	if !strings.Contains(class, "rt-inst") {
		t.Errorf("批量工单的表格应带 rt-inst: %q", class)
	}
}

// 没有脚本时（<noscript>）把结果表改回自动布局，内容照常完整显示；有脚本时默认就是固定列宽，不会先闪一下自动布局。
func TestSQLDetailResultTableNoscriptFallback(t *testing.T) {
	e := newSQLEnv(t)
	dev := e.login(t, "dev")
	w := dev.do("POST", "/api/v1/sqlworkflow", submitBody("update orders set a=1 where id=1"))
	page := dev.get("/sqlworkflow/" + itoa(dataOf(t, w)["workflow_id"].(float64))).Body.String()
	if !strings.Contains(page, "<noscript><style>#result-table { table-layout: auto;") {
		t.Error("详情页 head 里应有 <noscript> 把结果表改回自动布局")
	}
}
