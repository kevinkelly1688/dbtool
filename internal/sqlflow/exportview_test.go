// @author: kevinkelly
// @license: Apache Licence
// @file: exportview_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"testing"
)

// S3-B5：导出工单（含详情）对非提交人的可见性按 export.view（单实例）/ export.view_batch（批量）；提交人始终能看自己提交的。
func TestExportDetailVisibilityFollowsViewCodes(t *testing.T) {
	xe := setupExport(t)
	ctx := context.Background()
	single, err := xe.svc.Submit(ctx, dev, xe.req("select id,name from orders", "csv", 1))
	if err != nil {
		t.Fatal(err)
	}
	batch, err := xe.svc.Submit(ctx, dev, xe.req("select id,name from orders", "csv", 11, 12))
	if err != nil {
		t.Fatal(err)
	}
	// 提交人本人不受影响
	for _, id := range []uint{single.WorkflowID, batch.WorkflowID} {
		if _, err := xe.svc.Get(dev, id); err != nil {
			t.Fatalf("提交人应能看自己的导出工单 #%d: %v", id, err)
		}
	}
	// 同资源组的审批人（有审核按钮，没有任何导出查看按钮）
	rev := leader
	grantTo(xe.repo.Perms[2], grantReview)
	for _, id := range []uint{single.WorkflowID, batch.WorkflowID} {
		if _, err := xe.svc.Get(rev, id); code(err) != "not_found" {
			t.Errorf("没有 export.view / export.view_batch 不应看到导出工单 #%d: %v", id, err)
		}
	}
	// 只有 export.view：能看单实例，看不到批量
	xe.repo.Perms[2]["export.view"] = true
	if _, err := xe.svc.Get(rev, single.WorkflowID); err != nil {
		t.Errorf("有 export.view 应能看单实例导出: %v", err)
	}
	if _, err := xe.svc.Get(rev, batch.WorkflowID); code(err) != "not_found" {
		t.Errorf("只有 export.view 不应看到批量导出: %v", err)
	}
	// 再加 export.view_batch
	xe.repo.Perms[2]["export.view_batch"] = true
	if _, err := xe.svc.Get(rev, batch.WorkflowID); err != nil {
		t.Errorf("有 export.view_batch 应能看批量导出: %v", err)
	}
	// 普通上线工单不受导出查看码影响
	delete(xe.repo.Perms[2], "export.view")
	delete(xe.repo.Perms[2], "export.view_batch")
	w := xe.env.submit(t, dev, "update t set a=1")
	if _, err := xe.svc.Get(rev, w); err != nil {
		t.Errorf("普通工单应照常可见: %v", err)
	}
}

// S3-B6：批量导出至少选 2 个实例。
func TestBatchExportNeedsAtLeastTwoInstances(t *testing.T) {
	xe := setupExport(t)
	req := xe.req("select id,name from orders", "csv", 11)
	req.InstanceID = 0
	req.BatchInstanceIDs = []uint{11}
	_, err := xe.svc.Submit(context.Background(), dev, req)
	if code(err) != "bad_request" {
		t.Fatalf("批量导出只选 1 个实例应拒绝: %v", err)
	}
	if _, err := xe.svc.Submit(context.Background(), dev, xe.req("select id,name from orders", "csv", 11, 12)); err != nil {
		t.Fatalf("2 个实例应通过: %v", err)
	}
}
