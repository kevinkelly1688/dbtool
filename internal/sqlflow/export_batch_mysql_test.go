// @author: kevinkelly
// @license: Apache Licence
// @file: export_batch_mysql_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/review"
	"dbtool/internal/sqlflow"
)

// ---- 批量导出支持 MySQL 系实例（BATCH_DB_TYPES 加上 mysql 之后）----
//
// 默认值（只有 mssql）不变：不加 mysql 时批量导出 MySQL 实例仍被拒绝。

// withMySQLBatch 在导出测试环境里加入三个 MySQL 实例（21、22、23），并按 types 重建服务。
func withMySQLBatch(t *testing.T, types []string) *exportEnv {
	t.Helper()
	xe := setupExport(t)
	rd := []model.InstanceTag{{TagCode: "can_read", Active: true}}
	for _, id := range []uint{21, 22, 23} {
		host := fmt.Sprintf("my%d", id)
		xe.repo.Instances = append(xe.repo.Instances, model.Instance{ID: id, InstanceName: fmt.Sprintf("mysql-%d", id), DBType: "mysql", Host: host, Port: 3306, Tags: rd})
		xe.repo.InstRGs[id] = []uint{1}
		xe.hosts[host] = &qState{cols: []string{"id", "name"}, rows: [][]any{{int64(1), "张三"}, {int64(2), "李四"}}, count: 2}
	}
	s := sqlflow.NewService(xe.repo, sqlflow.Options{Notifier: xe.cap, ExportStorage: xe.store, BatchDBTypes: types, Open: func(_ string, c engine.Conn) (engine.Engine, error) {
		st := xe.hosts[c.Host]
		if st == nil {
			return nil, fmt.Errorf("没有这台假实例 %q", c.Host)
		}
		return &qEngine{host: c.Host, st: st}, nil
	}})
	s.SetAsync(func(f func()) { f() })
	s.SetSpawn(func(f func()) { f() })
	s.SetClock(func() time.Time { return xe.now })
	xe.svc = s
	return xe
}

func TestExportBatchMySQLRejectedByDefault(t *testing.T) {
	xe := withMySQLBatch(t, nil) // 默认：只有 mssql
	ctx := context.Background()
	if _, err := xe.svc.PreviewExport(ctx, dev, []uint{21, 22}, true, "shop", exportSQL); err == nil || !strings.Contains(err.Error(), "批量导出只支持：mssql") {
		t.Errorf("默认值不能变：批量导出不接受 MySQL 实例: %v", err)
	}
	if _, err := xe.svc.Submit(ctx, dev, xe.req(exportSQL, "csv", 21, 22)); err == nil || !strings.Contains(err.Error(), "批量导出只支持：mssql") {
		t.Errorf("提交也要拒绝: %v", err)
	}
	if got := xe.svc.BatchTypes(); len(got) != 1 || got[0] != "mssql" {
		t.Errorf("默认批量类型: %v", got)
	}
}

func TestExportBatchMySQLWhenEnabled(t *testing.T) {
	xe := withMySQLBatch(t, []string{"mssql", "mysql"})
	ctx := context.Background()
	r, err := xe.svc.PreviewExport(ctx, dev, []uint{21, 22, 23}, true, "shop", exportSQL)
	if err != nil || r.ErrorCount != 0 || len(r.Rows) != 3 || len(r.Instances) != 3 || !strings.Contains(r.Rows[1].ErrorMessage, "实例[mysql-22]待导出 2 行") {
		t.Fatalf("批量检查: %+v %v", r, err)
	}
	// 行数超限、连不上都按实例报告
	xe.hosts["my23"].count = 20000
	xe.hosts["my22"].noConn = true
	r, _ = xe.svc.PreviewExport(ctx, dev, []uint{21, 22, 23}, true, "shop", exportSQL)
	if r.ErrorCount != 2 || !strings.Contains(r.Rows[1].ErrorMessage, "实例[mysql-22]检查失败") || !strings.Contains(r.Rows[2].ErrorMessage, "实例[mysql-23]导出数据行数(20000)超过阈值(10000)") {
		t.Errorf("逐实例报告: %+v", r.Rows)
	}
	xe.hosts["my23"].count, xe.hosts["my22"].noConn = 2, false
	for _, h := range []string{"my21", "my22", "my23"} {
		xe.hosts[h].rows = [][]any{{int64(1), h + "-data"}}
	}

	// 混合 MySQL 与 SQL Server 仍然拒绝（批量要求同一类型）
	if _, err := xe.svc.Submit(ctx, dev, xe.req(exportSQL, "csv", 21, 11)); code(err) != "bad_request" || !strings.Contains(err.Error(), "类型不一致") {
		t.Errorf("混合类型: %v", err)
	}
	// MySQL 的只读限制照常生效
	if r, err := xe.svc.PreviewExport(ctx, dev, []uint{21, 22}, true, "shop", "select * from orders into outfile '/tmp/x'"); err != nil || r.ErrorCount == 0 || !strings.Contains(r.Rows[0].ErrorMessage, "不允许") {
		t.Errorf("批量导出也不允许写出文件: %+v %v", r, err)
	}

	id := xe.submit(t, xe.req(exportSQL, "csv", 21, 22, 23))
	if w := xe.repo.Workflows[id]; w.BatchInstanceIDs == nil || *w.BatchInstanceIDs != "21,22,23" || w.IsOfflineExport != 1 {
		t.Errorf("批量导出工单: %+v", w)
	}
	xe.hosts["my22"].failQ = "id, name" // 提交时统计通过，执行时 mysql-22 的查询失败
	xe.approve(t, id)
	if _, err := xe.svc.Execute(ctx, dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if xe.status(id) != model.WfException {
		t.Fatalf("部分失败 → 执行有异常: %s %s", xe.status(id), xe.repo.Contents[id].ExecuteResult)
	}
	entries := xe.zipEntries(t, fileOf(xe, id))
	if len(entries) != 2 || !strings.Contains(entries[fmt.Sprintf("%d/mysql-21.csv", id)], "my21-data") || !strings.Contains(entries[fmt.Sprintf("%d/mysql-23.csv", id)], "my23-data") {
		t.Fatalf("成功实例的文件在包里: %v", entries)
	}
	// 每个实例的查询都实际发到了各自的库，且带行数上限
	for _, h := range []string{"my21", "my23"} {
		st := xe.hosts[h]
		if len(st.opts) == 0 || st.opts[len(st.opts)-1].Limit != 10001 || st.opts[len(st.opts)-1].DBName != "shop" {
			t.Errorf("%s 的查询选项: %+v", h, st.opts)
		}
	}
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	if sum := rows[len(rows)-1]; !strings.Contains(sum.ErrorMessage, "共 3 个实例，成功 2 个，失败 1 个") {
		t.Errorf("汇总: %+v", sum)
	}

	// 排除故障后重试 mysql-22：合并进同一压缩包
	xe.hosts["my22"].failQ = ""
	msg, status, err := xe.svc.RetryBatchInstance(ctx, dev, id, 22)
	if err != nil || status != model.WfFinish || !strings.Contains(msg, "重试成功") {
		t.Fatalf("重试: %q %q %v", msg, status, err)
	}
	if n := len(xe.repo.LifecycleAudits); n == 0 || xe.repo.LifecycleAudits[n-1].Action != "SQL上线-批量实例重试" || !strings.Contains(xe.repo.LifecycleAudits[n-1].ExtraInfo, "类型：批量导出") {
		t.Errorf("批量重试应写系统审计: %+v", xe.repo.LifecycleAudits)
	}
	merged := xe.zipEntries(t, fileOf(xe, id))
	if len(merged) != 3 || !strings.Contains(merged[fmt.Sprintf("%d/mysql-22.csv", id)], "my22-data") {
		t.Errorf("合并后的包: %v", merged)
	}
}
