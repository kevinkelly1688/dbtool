// @author: kevinkelly
// @license: Apache Licence
// @file: export_purged_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/query"
	"dbtool/internal/sqlflow"
)

// 导出文件被数据保留策略清理后：下载接口返回 410 + 清理时间（而不是通用 404），详情页文件卡片带上清理时间；
// 手工删除（没有清理记录）仍是 404。
func TestExportDownloadAfterRetentionPurge(t *testing.T) {
	xe := setupExport(t)
	ctx := context.Background()
	res, err := xe.svc.Submit(ctx, dev, xe.req("select id,name from orders", "csv", 1))
	if err != nil {
		t.Fatal(err)
	}
	xe.approve(t, res.WorkflowID)
	if _, err := xe.svc.Execute(ctx, dev, res.WorkflowID, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	d, err := xe.svc.Get(dev, res.WorkflowID)
	if err != nil || d.ExportFile == nil {
		t.Fatalf("详情: %v", err)
	}
	if err := xe.store.Delete(d.ExportFile.Name); err != nil {
		t.Fatal(err)
	}

	// 没有清理记录：沿用 404
	_, err = xe.svc.Download(dev, res.WorkflowID)
	var qe *query.Error
	if !errors.As(err, &qe) || qe.HTTP != 404 {
		t.Fatalf("手工删除应 404: %v", err)
	}

	purgedAt := time.Date(2026, 9, 1, 3, 4, 5, 0, time.Local)
	sqlflow.SetExportPurgedLookup(func(id uint) (time.Time, bool) { return purgedAt, id == res.WorkflowID })
	t.Cleanup(func() { sqlflow.SetExportPurgedLookup(nil) })

	_, err = xe.svc.Download(dev, res.WorkflowID)
	if !errors.As(err, &qe) || qe.HTTP != 410 || qe.Code != "export_purged" {
		t.Fatalf("已清理应 410 export_purged: %v", err)
	}
	if !strings.Contains(qe.Message, "2026-09-01 03:04:05") || !strings.Contains(qe.Message, "重新提交导出") {
		t.Errorf("提示应含清理时间并建议重新提交: %q", qe.Message)
	}
	d, _ = xe.svc.Get(dev, res.WorkflowID)
	if !d.ExportFile.Cleaned || d.ExportFile.PurgedAt == nil || !d.ExportFile.PurgedAt.Equal(purgedAt) {
		t.Errorf("详情文件卡片应带清理时间: %+v", d.ExportFile)
	}
}
