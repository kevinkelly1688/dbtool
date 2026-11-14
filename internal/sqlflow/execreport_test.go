// @author: kevinkelly
// @license: Apache Licence
// @file: execreport_test.go
// @time: 2026/10/28

package sqlflow

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"dbtool/internal/review"
)

func okRow(id int, aff int64) review.Row {
	return review.Row{ID: review.Flex(id), Stage: "EXECUTED", StageStatus: "Execute Successfully", ErrorMessage: "None", AffectedRows: review.Flex(aff)}
}

// 非批量工单：每条语句一行；影响行数小于 0（SQL Server 的 DDL 返回 -1）按 0 算，合计也不受影响。
func TestExecReportSingleInstance(t *testing.T) {
	var rows []review.Row
	for i := 1; i <= 10; i++ {
		rows = append(rows, okRow(i, -1))
	}
	rep, ok := buildExecReport(rows)
	if !ok || rep.Total != 10 || rep.OK != 10 || rep.Fail != 0 || rep.Skip != 0 || rep.Affected != 0 || rep.Batch {
		t.Fatalf("全部成功: %+v", rep)
	}
	if got := rep.Summary(); got != "总数: 10 | 成功: 10 | 失败: 0 | 影响行数: 0" {
		t.Errorf("汇总: %q", got)
	}
	lines := strings.Split(rep.Log, "\n")
	if len(lines) != 10 || lines[0] != "ID: 1 | 状态: ✅ Execute Successfully | 影响行数: 0" {
		t.Errorf("执行记录: %q", rep.Log)
	}

	// 有失败、有未执行：失败带错误信息，后面的语句标未执行；影响行数只累加非负的
	rows = []review.Row{
		okRow(1, 3),
		{ID: 2, Stage: "EXECUTED", StageStatus: "Execute failed", ErrLevel: review.LevelError, ErrorMessage: "Lock wait timeout\nexceeded"},
		{ID: 3, Stage: "CHECKED", StageStatus: "Audit completed", ErrorMessage: "前序语句失败, 未执行"},
		{ID: 4, Stage: "SKIPPED", StageStatus: "未执行", ErrLevel: review.LevelWarning},
	}
	rep, _ = buildExecReport(rows)
	if rep.Total != 4 || rep.OK != 1 || rep.Fail != 1 || rep.Skip != 2 || rep.Affected != 3 {
		t.Fatalf("失败 / 未执行: %+v", rep)
	}
	if got := rep.Summary(); got != "总数: 4 | 成功: 1 | 失败: 1 | 未执行: 2 | 影响行数: 3" {
		t.Errorf("汇总: %q", got)
	}
	want := "ID: 1 | 状态: ✅ Execute Successfully | 影响行数: 3\n" +
		"ID: 2 | 状态: ❌ Execute failed | 影响行数: 0 | 错误: Lock wait timeout exceeded\n" +
		"ID: 3 | 状态: ⏭ 未执行\n" +
		"ID: 4 | 状态: ⏭ 未执行"
	if rep.Log != want {
		t.Errorf("执行记录:\n%s\n期望:\n%s", rep.Log, want)
	}
}

// 记录太长：不超过 1000 字，末尾用 ...，失败的优先保留，仍按原顺序。
func TestExecReportTruncatesWithEllipsis(t *testing.T) {
	var rows []review.Row
	for i := 1; i <= 120; i++ {
		rows = append(rows, okRow(i, 1))
	}
	rows[99] = review.Row{ID: 100, Stage: "EXECUTED", StageStatus: "Execute failed", ErrLevel: review.LevelError, ErrorMessage: strings.Repeat("e", 300)}
	rep, _ := buildExecReport(rows)
	if n := utf8.RuneCountInString(rep.Log); n > 1000 {
		t.Errorf("记录不应超过 1000 字: %d", n)
	}
	if !strings.HasSuffix(rep.Log, "\n...") {
		t.Errorf("超出后末尾应是 ...: %q", rep.Log[len(rep.Log)-30:])
	}
	if !strings.Contains(rep.Log, "ID: 100 | 状态: ❌ Execute failed") || !strings.Contains(rep.Log, strings.Repeat("e", 100)+"...") {
		t.Errorf("失败的那条应保留，错误信息最多 100 字: %q", rep.Log)
	}
	if rep.Total != 120 || rep.OK != 119 || rep.Fail != 1 {
		t.Errorf("汇总不受截断影响: %+v", rep)
	}
	// 显示的行保持原来的顺序
	prev := 0
	for _, l := range strings.Split(rep.Log, "\n") {
		var id int
		if _, err := fmt.Sscanf(l, "ID: %d", &id); err == nil {
			if id < prev {
				t.Fatalf("顺序乱了: %d 在 %d 之后", id, prev)
			}
			prev = id
		}
	}
}

// 批量工单：只给成功 / 失败多少个实例，不列出实例名，也没有逐条记录。
func TestExecReportBatchHasCountsOnly(t *testing.T) {
	var rows []review.Row
	add := func(name string, ok bool, closed bool) {
		r := okRow(1, 2)
		if !ok {
			r = review.Row{ID: 1, Stage: "Execute failed", StageStatus: "异常终止", ErrLevel: review.LevelError, ErrorMessage: "连接失败"}
		}
		r.InstanceName, r.BatchClosed = name, closed
		rows = append(rows, r, func() review.Row { x := okRow(2, 1); x.InstanceName, x.BatchClosed = name, closed; return x }())
	}
	add("vn-mssql-01", true, false)
	add("in-mssql-02", true, false)
	add("th-mssql-03", false, false)
	rows = append(rows, review.Row{IsSummary: true, InstanceName: "汇总", StageStatus: "执行完成"})
	rep, ok := buildExecReport(rows)
	if !ok || !rep.Batch || rep.Total != 3 || rep.OK != 2 || rep.Fail != 1 || rep.Affected != 7 || rep.Log != "" {
		t.Fatalf("批量: %+v", rep)
	}
	got := rep.Summary()
	if got != "共 3 个实例 | 成功 2 个实例 | 失败 1 个实例 | 影响行数: 7" {
		t.Errorf("汇总: %q", got)
	}
	for _, name := range []string{"vn-mssql-01", "in-mssql-02", "th-mssql-03"} {
		if strings.Contains(got, name) || strings.Contains(rep.Log, name) {
			t.Errorf("不应列出实例名 %s: %q %q", name, got, rep.Log)
		}
	}
	// 关闭的实例算“未执行”
	add("sg-mssql-04", true, true)
	rep, _ = buildExecReport(rows)
	if rep.Total != 4 || rep.Skip != 1 || !strings.Contains(rep.Summary(), "未执行 1 个实例") {
		t.Errorf("已关闭的实例: %+v %s", rep, rep.Summary())
	}
}

func TestExecReportNoRows(t *testing.T) {
	if _, ok := buildExecReport(nil); ok {
		t.Error("没有结果行不应生成报表")
	}
	if _, ok := buildExecReport([]review.Row{{IsSummary: true}}); ok {
		t.Error("只有汇总行不应生成报表")
	}
}
