// @author: kevinkelly
// @license: Apache Licence
// @file: export_merge_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dbtool/internal/model"
	"dbtool/internal/review"
	"dbtool/internal/settings"
	"dbtool/internal/sqlflow"
)

// 批量导出“合并成一个文件”：提交时选择，各实例导出的数据按勾选顺序依次写进同一个文件（不加实例名称列、表头一份）；
// 各实例返回的列必须一致；重试失败实例时追加到文件末尾。

func mergeReq(xe *exportEnv, format string, ids ...uint) sqlflow.SubmitReq {
	r := xe.req(exportSQL, format, ids...)
	r.Export.MergeFiles = true
	return r
}

// mergeData 让三个 SQL Server 实例各返回自己的两行（一眼能看出是哪个实例的）。
func mergeData(xe *exportEnv) {
	for host, tag := range map[string]string{"ms11": "a", "ms12": "b", "ms13": "c"} {
		xe.hosts[host].rows = [][]any{{int64(1), tag + "-1"}, {int64(2), tag + "-2"}}
	}
}

func mergedName(id uint, format string) string { return fmt.Sprintf("%d/merged_%d.%s", id, id, format) }

func TestExportMergeOneFile(t *testing.T) {
	xe := setupExport(t)
	mergeData(xe)
	id := xe.submit(t, mergeReq(xe, "csv", 11, 12, 13))
	if e := xe.repo.ExportExts[id]; !e.MergeFiles || e.Unmasked {
		t.Fatalf("提交后应记下合并选项: %+v", e)
	}
	xe.run(t, id)
	if xe.status(id) != model.WfFinish {
		t.Fatalf("状态: %s %s", xe.status(id), xe.repo.Contents[id].ExecuteResult)
	}
	entries := xe.zipEntries(t, fileOf(xe, id))
	if len(entries) != 1 {
		t.Fatalf("合并后压缩包里只有一个文件: %v", entries)
	}
	// 就是导出的数据本身：一份表头，三个实例的行按勾选顺序接在一起，没有实例名称列
	want := "\"id\",\"name\"\r\n" +
		"\"1\",\"a-1\"\r\n\"2\",\"a-2\"\r\n" +
		"\"1\",\"b-1\"\r\n\"2\",\"b-2\"\r\n" +
		"\"1\",\"c-1\"\r\n\"2\",\"c-2\"\r\n"
	if got := entries[mergedName(id, "csv")]; got != want {
		t.Errorf("合并文件内容（按勾选顺序）:\n%q\n期望\n%q", got, want)
	}
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	seen := 0
	for _, r := range rows {
		if strings.Contains(r.ErrorMessage, fmt.Sprintf("已写入合并文件: merged_%d.csv", id)) {
			seen++
		}
	}
	if seen != 3 || !strings.Contains(rows[len(rows)-1].ErrorMessage, "共 3 个实例，成功 3 个") {
		t.Errorf("每个实例一行结果，另有汇总行: %d %+v", seen, rows)
	}
	// 详情页标出“合并成一个文件”；没选的工单没有这个标记
	d, err := xe.svc.Get(dev, id)
	if err != nil || !d.ExportMerge {
		t.Errorf("详情应有合并标记: %+v %v", d, err)
	}
	id2 := xe.submit(t, xe.req(exportSQL, "csv", 11, 12))
	if d2, _ := xe.svc.Get(dev, id2); d2 == nil || d2.ExportMerge {
		t.Errorf("没选合并的批量导出不应有合并标记: %+v", d2)
	}
	if _, ok := xe.repo.ExportExts[id2]; ok {
		t.Errorf("什么选项都没选时不留扩展记录")
	}
}

// 没选合并的批量导出照旧：每个实例一个文件。
func TestExportBatchWithoutMergeStillOneFilePerInstance(t *testing.T) {
	xe := setupExport(t)
	mergeData(xe)
	id := xe.submit(t, xe.req(exportSQL, "csv", 11, 12, 13))
	xe.run(t, id)
	entries := xe.zipEntries(t, fileOf(xe, id))
	if len(entries) != 3 || entries[fmt.Sprintf("%d/mssql-12.csv", id)] != "\"id\",\"name\"\r\n\"1\",\"b-1\"\r\n\"2\",\"b-2\"\r\n" {
		t.Errorf("不合并时应每个实例一个文件: %v", entries)
	}
}

func TestExportMergeAllFormats(t *testing.T) {
	for _, format := range []string{"csv", "json", "xml", "xlsx", "sql"} {
		t.Run(format, func(t *testing.T) {
			xe := setupExport(t)
			mergeData(xe)
			id := xe.submit(t, mergeReq(xe, format, 11, 12, 13))
			xe.run(t, id)
			if xe.status(id) != model.WfFinish {
				t.Fatalf("状态: %s %s", xe.status(id), xe.repo.Contents[id].ExecuteResult)
			}
			entries := xe.zipEntries(t, fileOf(xe, id))
			content, ok := entries[mergedName(id, format)]
			if len(entries) != 1 || !ok || content == "" {
				t.Fatalf("压缩包里应只有 %s: %v", mergedName(id, format), entries)
			}
			if format != "xlsx" && (!strings.Contains(content, "a-1") || !strings.Contains(content, "c-2") || strings.Contains(content, "mssql-1")) {
				t.Errorf("内容不对（应有各实例的数据、不含实例名称）: %s", content)
			}
		})
	}
}

func TestExportMergeRejections(t *testing.T) {
	xe := setupExport(t)
	ctx := context.Background()
	before := len(xe.repo.Workflows)
	// 只有批量导出才能合并
	single := xe.req(exportSQL, "csv", 11)
	single.Export.MergeFiles = true
	if _, err := xe.svc.Submit(ctx, dev, single); code(err) != "bad_request" || !strings.Contains(err.Error(), "只有批量导出可以合并成一个文件") {
		t.Errorf("单实例导出不能合并: %v", err)
	}
	if len(xe.repo.Workflows) != before || len(xe.repo.ExportExts) != 0 {
		t.Errorf("被拒绝时不应留下工单 / 记录")
	}
	// 提交选项：有批量导出权限就能看到；没有权限的看不到
	if o, err := xe.svc.SubmitOptions(dev); err != nil || !o.Export.CanMerge {
		t.Errorf("提交选项应提供合并: %+v %v", o.Export, err)
	}
	if o, _ := xe.svc.SubmitOptions(leader); o.Export.CanMerge {
		t.Errorf("没有批量导出权限不应出现合并选项")
	}
	// 旧版 290 脚本建的表（没有 merge_files 列）：选项不出现，提交带这个选项被拒绝，不带的照常
	xe.repo.NoExportMergeCols = true
	if o, _ := xe.svc.SubmitOptions(dev); o.Export.CanMerge {
		t.Errorf("缺列时不应出现合并选项")
	}
	if _, err := xe.svc.Submit(ctx, dev, mergeReq(xe, "csv", 11, 12)); code(err) != "export_ext_table" || !strings.Contains(err.Error(), "290_export_unmasked.sql") {
		t.Errorf("缺列时提交合并应提示重新执行 290 脚本: %v", err)
	}
	xe.submit(t, xe.req(exportSQL, "csv", 11, 12))
	// 整张表没建
	xe.repo.NoExportMergeCols, xe.repo.NoExportExtTb = false, true
	if o, _ := xe.svc.SubmitOptions(dev); o.Export.CanMerge {
		t.Errorf("没建表时不应出现合并选项")
	}
}

func TestExportMergeColumnsMustMatchAtCheck(t *testing.T) {
	xe := setupExport(t)
	mergeData(xe)
	ctx := context.Background()
	xe.hosts["ms12"].cols = []string{"id", "title"}
	// 页面的检查带合并标记：核对各实例的列，不一致的实例报错
	r, err := xe.svc.PreviewExportMerge(ctx, dev, []uint{11, 12, 13}, true, true, "shop", exportSQL)
	if err != nil || r.ErrorCount != 1 || len(r.Instances) != 3 || r.Instances[1].OK || !r.Instances[0].OK || !r.Instances[2].OK {
		t.Fatalf("列不一致应只让 mssql-12 报错: %+v %v", r, err)
	}
	msg := r.Rows[1].ErrorMessage
	if !strings.Contains(msg, "实例[mssql-12]") || !strings.Contains(msg, "第一个实例「mssql-11」不一致") || !strings.Contains(msg, "id, name") || !strings.Contains(msg, "id, title") {
		t.Errorf("提示要写明参照实例和两边的列: %q", msg)
	}
	// 不合并时各实例列不同不影响（每个实例一个文件）
	if r, _ := xe.svc.PreviewExport(ctx, dev, []uint{11, 12, 13}, true, "shop", exportSQL); r.ErrorCount != 0 {
		t.Errorf("不合并时不核对列: %+v", r)
	}
	// 服务端提交时自己再核对（不信任页面）：被自动审核不通过，不会进审批
	res, err := xe.svc.Submit(ctx, dev, mergeReq(xe, "csv", 11, 12, 13))
	if err != nil || res.Status != model.WfAutoReviewWrong {
		t.Errorf("提交时列不一致应被自动审核不通过: %+v %v", res, err)
	}
	// 取不到列（探测语句失败）不能提前核对：不拦截，留到执行时
	xe.hosts["ms12"].cols = []string{"id", "name"}
	xe.hosts["ms13"].failQ = "SELECT TOP 0 * FROM (" // 只让探测列名的语句失败
	if r, _ := xe.svc.PreviewExportMerge(ctx, dev, []uint{11, 12, 13}, true, true, "shop", exportSQL); r.ErrorCount != 0 {
		t.Errorf("探测列失败时不应拦截: %+v", r)
	}
}

func TestExportMergeColumnsMismatchAtRunThenRetryAppends(t *testing.T) {
	xe := setupExport(t)
	mergeData(xe)
	id := xe.submit(t, mergeReq(xe, "csv", 11, 12, 13))
	// 提交通过之后 mssql-12 的结构变了：执行时它的列对不上，只有它失败
	xe.hosts["ms12"].cols = []string{"id", "title"}
	xe.run(t, id)
	if xe.status(id) != model.WfException {
		t.Fatalf("部分失败 → 执行有异常: %s %s", xe.status(id), xe.repo.Contents[id].ExecuteResult)
	}
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	var failed *review.Row
	for i := range rows {
		if rows[i].InstanceName == "mssql-12" {
			failed = &rows[i]
		}
	}
	if failed == nil || !strings.Contains(failed.ErrorMessage, "导出失败") || !strings.Contains(failed.ErrorMessage, "与合并文件不一致") {
		t.Fatalf("mssql-12 应因列不一致失败: %+v", rows)
	}
	first := "\"id\",\"name\"\r\n\"1\",\"a-1\"\r\n\"2\",\"a-2\"\r\n\"1\",\"c-1\"\r\n\"2\",\"c-2\"\r\n"
	if got := xe.zipEntries(t, fileOf(xe, id))[mergedName(id, "csv")]; got != first {
		t.Errorf("失败的实例不写入合并文件:\n%q", got)
	}
	// 实例恢复后重试：追加到合并文件末尾，其它实例的行不变
	xe.hosts["ms12"].cols = []string{"id", "name"}
	msg, status, err := xe.svc.RetryBatchInstance(context.Background(), dev, id, 12)
	if err != nil || status != model.WfFinish || !strings.Contains(msg, "重试成功") {
		t.Fatalf("重试: %q %q %v", msg, status, err)
	}
	entries := xe.zipEntries(t, fileOf(xe, id))
	want := first + "\"1\",\"b-1\"\r\n\"2\",\"b-2\"\r\n"
	if len(entries) != 1 || entries[mergedName(id, "csv")] != want {
		t.Errorf("重试后追加在末尾:\n%q\n期望\n%q", entries, want)
	}
}

// 全部格式的重试追加（Excel 要整个重写）：先让 mssql-12 查询失败，再恢复后重试。
func TestExportMergeRetryAppendsAllFormats(t *testing.T) {
	for _, format := range []string{"csv", "json", "xml", "xlsx", "sql"} {
		t.Run(format, func(t *testing.T) {
			xe := setupExport(t)
			mergeData(xe)
			id := xe.submit(t, mergeReq(xe, format, 11, 12, 13))
			xe.hosts["ms12"].failQ = "from orders"
			xe.run(t, id)
			if xe.status(id) != model.WfException {
				t.Fatalf("状态: %s", xe.status(id))
			}
			xe.hosts["ms12"].failQ = ""
			if _, status, err := xe.svc.RetryBatchInstance(context.Background(), dev, id, 12); err != nil || status != model.WfFinish {
				t.Fatalf("重试: %q %v", status, err)
			}
			entries := xe.zipEntries(t, fileOf(xe, id))
			content := entries[mergedName(id, format)]
			if len(entries) != 1 || content == "" {
				t.Fatalf("压缩包: %v", entries)
			}
			if format == "xlsx" {
				return // 单元格内容在 export 包的追加测试里核对
			}
			// 三个实例各两行都在，mssql-12 的行在最后
			for _, tag := range []string{"a-1", "a-2", "b-1", "b-2", "c-1", "c-2"} {
				if strings.Count(content, tag) != 1 {
					t.Errorf("%s 应只出现一次: %s", tag, content)
				}
			}
			if strings.LastIndex(content, "b-2") < strings.LastIndex(content, "c-2") {
				t.Errorf("重试的实例应追加在末尾: %s", content)
			}
		})
	}
}

func TestExportMergeAllInstancesFail(t *testing.T) {
	xe := setupExport(t)
	mergeData(xe)
	id := xe.submit(t, mergeReq(xe, "csv", 11, 12))
	xe.hosts["ms11"].failQ, xe.hosts["ms12"].failQ = "from orders", "from orders"
	xe.run(t, id)
	if xe.status(id) != model.WfException || fileOf(xe, id) != "" {
		t.Errorf("全部失败不应生成文件: %s %q", xe.status(id), fileOf(xe, id))
	}
}

func TestExportMergeExcelTotalRowLimit(t *testing.T) {
	xe := setupExport(t)
	xe.repo.Cfg[settings.KeyExportMaxRows] = "2000000"
	for _, h := range []string{"ms11", "ms12", "ms13"} {
		xe.hosts[h].count = 400000 // 每个实例都没超行数上限，但合并后 120 万行超过 Excel 的上限
	}
	if _, err := xe.svc.Submit(context.Background(), dev, mergeReq(xe, "xlsx", 11, 12, 13)); code(err) != "bad_request" || !strings.Contains(err.Error(), "超过 Excel 的上限") {
		t.Errorf("合并后的行数超过 Excel 上限应拒绝提交: %v", err)
	}
	// 同样的数据：CSV 可以；不合并的 Excel（每个实例一个文件）也可以
	xe.submit(t, mergeReq(xe, "csv", 11, 12, 13))
	xe.submit(t, xe.req(exportSQL, "xlsx", 11, 12, 13))
}

// 合并导出与“导出原值”可以同时选：两个选项都记在同一行扩展记录里。
func TestExportMergeWithUnmasked(t *testing.T) {
	xe := unmaskEnv(t)
	mergeData(xe)
	r := mergeReq(xe, "csv", 11, 12)
	r.Export.Unmasked, r.Export.UnmaskReason = true, unmaskReason
	res, err := xe.svc.Submit(context.Background(), root, r)
	if err != nil {
		t.Fatal(err)
	}
	if e := xe.repo.ExportExts[res.WorkflowID]; !e.MergeFiles || !e.Unmasked || e.Reason != unmaskReason || e.SetBy != "root" {
		t.Errorf("两个选项应记在同一行: %+v", e)
	}
	// 只选合并时，不带“导出原值”的理由和选择人
	id := xe.submit(t, mergeReq(xe, "csv", 11, 12))
	if e := xe.repo.ExportExts[id]; e.Unmasked || e.Reason != "" || e.SetBy != "" {
		t.Errorf("只选合并的记录里不应有导出原值的信息: %+v", e)
	}
}

// 取数的位置有上限（并行数的两倍）：实例比位置多时也要按顺序写完，不能互相等死。
func TestExportMergeFewerSlotsThanInstances(t *testing.T) {
	// SQL Server 并行，但只给 1 个并行数：同时最多 2 个实例在“取出来还没写”的状态
	xe := setupExport(t)
	mergeData(xe)
	xe.repo.Cfg[settings.KeyReviewBatchWorkers] = "1"
	id := xe.submit(t, mergeReq(xe, "csv", 11, 12, 13))
	xe.run(t, id)
	want := "\"id\",\"name\"\r\n\"1\",\"a-1\"\r\n\"2\",\"a-2\"\r\n\"1\",\"b-1\"\r\n\"2\",\"b-2\"\r\n\"1\",\"c-1\"\r\n\"2\",\"c-2\"\r\n"
	if xe.status(id) != model.WfFinish || contentOf(t, xe, id) != want {
		t.Errorf("并行数为 1 时的合并结果: %s\n%q", xe.status(id), contentOf(t, xe, id))
	}
	// MySQL 系依次取数（放开 BATCH_DB_TYPES 后），3 个实例 > 2 个位置
	my := withMySQLBatch(t, []string{"mssql", "mysql"})
	for _, h := range []string{"my21", "my22", "my23"} {
		my.hosts[h].rows = [][]any{{int64(1), h + "-data"}}
	}
	id2 := my.submit(t, mergeReq(my, "csv", 21, 22, 23))
	my.run(t, id2)
	want2 := "\"id\",\"name\"\r\n\"1\",\"my21-data\"\r\n\"1\",\"my22-data\"\r\n\"1\",\"my23-data\"\r\n"
	if my.status(id2) != model.WfFinish || contentOf(t, my, id2) != want2 {
		t.Errorf("MySQL 系依次取数的合并结果: %s\n%q", my.status(id2), contentOf(t, my, id2))
	}
}
