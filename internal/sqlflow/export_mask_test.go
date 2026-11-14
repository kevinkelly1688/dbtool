// @author: kevinkelly
// @license: Apache Licence
// @file: export_mask_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/review"
	"dbtool/internal/settings"
	"dbtool/internal/sqlflow"
)

// 导出工单的数据脱敏：开关 export_data_masking 默认开；系统脱敏总开关 data_masking 关闭时不脱敏；脱敏失败中止导出。

type fakeMasker struct {
	calls int
	err   error
}

func (m *fakeMasker) MaskExport(_ context.Context, _ *model.Instance, _, _ string, _ engine.Engine, rs *engine.ResultSet) ([]string, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	for i := range rs.Rows {
		rs.Rows[i] = append([]any(nil), rs.Rows[i]...)
		rs.Rows[i][1] = "张***"
	}
	return []string{"name"}, nil
}

func runMaskedExport(t *testing.T, xe *exportEnv, m *fakeMasker) (id uint, content string) {
	t.Helper()
	xe.svc.SetResultMasker(m)
	id = xe.submit(t, xe.req(exportSQL, "csv", 1))
	xe.run(t, id)
	if name := fileOf(xe, id); name != "" {
		for _, c := range xe.zipEntries(t, name) {
			content = c
		}
	}
	return id, content
}

func TestExportMaskingDefaultOn(t *testing.T) {
	xe := setupExport(t)
	xe.repo.Cfg[settings.KeyQueryMaskEnabled] = "true" // export_data_masking 没配置：默认开
	m := &fakeMasker{}
	id, content := runMaskedExport(t, xe, m)
	if m.calls != 1 || !strings.Contains(content, `"1","张***"`) || strings.Contains(content, "张三") {
		t.Fatalf("calls=%d content=%q", m.calls, content)
	}
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	if len(rows) != 1 || !strings.Contains(rows[0].ErrorMessage, "（已脱敏：name）") {
		t.Errorf("结果行应说明已脱敏的列: %+v", rows)
	}
}

func TestExportMaskingSwitches(t *testing.T) {
	// 导出脱敏开关显式关闭
	xe := setupExport(t)
	xe.repo.Cfg[settings.KeyQueryMaskEnabled] = "true"
	xe.repo.Cfg[settings.KeyExportMaskEnabled] = "false"
	m := &fakeMasker{}
	if _, content := runMaskedExport(t, xe, m); m.calls != 0 || !strings.Contains(content, "张三") {
		t.Errorf("开关关闭不应脱敏: calls=%d %q", m.calls, content)
	}
	// 系统脱敏总开关关闭：导出也不脱敏
	xe2 := setupExport(t)
	xe2.repo.Cfg[settings.KeyExportMaskEnabled] = "true"
	m2 := &fakeMasker{}
	if _, content := runMaskedExport(t, xe2, m2); m2.calls != 0 || !strings.Contains(content, "张三") {
		t.Errorf("总开关关闭不应脱敏: calls=%d %q", m2.calls, content)
	}
}

func TestExportMaskingFailureAbortsExport(t *testing.T) {
	xe := setupExport(t)
	xe.repo.Cfg[settings.KeyQueryMaskEnabled] = "true"
	m := &fakeMasker{err: errors.New("读取脱敏配置失败")}
	xe.svc.SetResultMasker(m)
	id := xe.submit(t, xe.req(exportSQL, "csv", 1))
	xe.approve(t, id)
	_, _ = xe.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto})
	if xe.status(id) == model.WfFinish {
		t.Fatalf("脱敏失败不应正常结束: %s", xe.status(id))
	}
	if name := fileOf(xe, id); name != "" {
		t.Errorf("脱敏失败不应产生文件: %q", name)
	}
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	if len(rows) == 0 || !strings.Contains(rows[0].ErrorMessage, "不会导出未脱敏的数据") {
		t.Errorf("应说明中止原因: %+v", rows)
	}
}
