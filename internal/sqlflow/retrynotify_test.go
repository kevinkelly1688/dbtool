// @author: kevinkelly
// @license: Apache Licence
// @file: retrynotify_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"strings"
	"testing"

	"dbtool/internal/model"
	"dbtool/internal/notify"
)

// 批量工单重试失败实例：重试结果（仍失败 → 执行异常；成功 → 执行完成）都要触发通知，仍失败时能看到错误原因。
func TestBatchRetryNotifies(t *testing.T) {
	be := setupBatch(t)
	id := submitBatch(t, be, 11, 12, 13)
	be.hosts["ms12"].failOn = "Orders"
	execBatch(t, be, id)
	if be.status(id) != model.WfException {
		t.Fatalf("部分失败 → 执行有异常: %s", be.status(id))
	}
	ctx := context.Background()

	be.cap.reset()
	if _, _, err := be.svc.RetryBatchInstance(ctx, dev, id, 12); err != nil {
		t.Fatal(err)
	}
	var got []notify.Event
	for _, m := range be.cap.msgs {
		got = append(got, m)
	}
	if len(got) == 0 {
		t.Fatalf("重试仍失败：应发“执行异常”通知，实际没有任何通知")
	}
	last := got[len(got)-1]
	if last.Name != notify.EventException || !strings.Contains(last.Title, "重试实例仍然失败") || !strings.Contains(last.Content, "重试实例：mssql-12") {
		t.Errorf("重试仍失败的通知应写明重试的实例与结果: %+v", last)
	}
	if first := got[0]; first.Name != notify.EventExecuting || !strings.Contains(first.Content, "重试实例：mssql-12") {
		t.Errorf("重试开始应发“开始执行”通知: %+v", first)
	}

	be.hosts["ms12"].failOn = ""
	be.cap.reset()
	if _, _, err := be.svc.RetryBatchInstance(ctx, dev, id, 12); err != nil {
		t.Fatal(err)
	}
	if len(be.cap.msgs) == 0 || be.cap.msgs[len(be.cap.msgs)-1].Name != notify.EventFinished || !strings.Contains(be.cap.msgs[len(be.cap.msgs)-1].Title, "重试实例成功") {
		t.Errorf("重试成功：应发“执行完成”通知: %+v", be.cap.msgs)
	}
}
