// @author: kevinkelly
// @license: Apache Licence
// @file: audit_export_test.go
// @time: 2026/10/28

package store

import (
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

var auditCols = []string{"audit_id", "group_id", "group_name", "workflow_id", "workflow_type", "workflow_title", "workflow_remark",
	"audit_auth_groups", "current_audit", "next_audit", "current_status", "create_user", "create_user_display", "create_time", "sys_time", "is_export_flag"}

func auditRow(typ int64, export int64) []driver.Value {
	return []driver.Value{int64(5), int64(1), "g", int64(77), typ, "t", "", "10", "10", "-1", int64(0), "dev", "开发", time.Now(), time.Now(), export}
}

// SQL 上线类审批记录要带出“是否数据导出工单”（供审批引擎对导出单应用严格的禁止自审规则）；其他类型不查。
func TestAuditByWorkflowCarriesExportFlag(t *testing.T) {
	d := &fakeDB{results: []fakeResult{{match: "FROM `dbt_workflow_audit`", cols: auditCols, rows: [][]driver.Value{auditRow(2, 1)}}}}
	s := newNotifyStore(t, d)
	a, err := s.AuditByWorkflow(audit.TypeSQLReview, 77, false)
	if err != nil || a == nil {
		t.Fatalf("%v %v", a, err)
	}
	if !a.IsExport {
		t.Error("导出工单应带出 IsExport")
	}
	d.mu.Lock()
	q := d.logs[len(d.logs)-1]
	d.mu.Unlock()
	if !strings.Contains(q, "is_offline_export") || !strings.Contains(q, "dbt_sql_workflow") {
		t.Errorf("应使用 dbt_sql_workflow.is_offline_export 子查询: %s", q)
	}

	d2 := &fakeDB{results: []fakeResult{{match: "FROM `dbt_workflow_audit`", cols: auditCols, rows: [][]driver.Value{auditRow(2, 0)}}}}
	a, _ = newNotifyStore(t, d2).AuditByWorkflow(audit.TypeSQLReview, 77, false)
	if a == nil || a.IsExport {
		t.Errorf("普通 SQL 上线工单不应是导出: %+v", a)
	}

	d3 := &fakeDB{results: []fakeResult{{match: "FROM `dbt_workflow_audit`", cols: auditCols[:15], rows: [][]driver.Value{auditRow(1, 0)[:15]}}}}
	a, _ = newNotifyStore(t, d3).AuditByWorkflow(audit.TypeQueryPrivilege, 77, false)
	if a == nil || a.IsExport {
		t.Errorf("查询权限申请: %+v", a)
	}
	for _, l := range d3.logs {
		if strings.Contains(l, "is_offline_export") {
			t.Errorf("非 SQL 上线类型不应带子查询: %s", l)
		}
	}
}

// 保存审批记录不能把只读的 IsExport 当列写出去。
func TestSaveAuditIgnoresExportFlag(t *testing.T) {
	d := &fakeDB{}
	s := newNotifyStore(t, d)
	a := &model.WorkflowAudit{AuditID: 5, WorkflowType: 2, CreateUser: "dev", IsExport: true}
	_ = s.SaveAudit(a)
	for _, l := range d.logs {
		if strings.Contains(l, "is_export_flag") {
			t.Errorf("不应写入 is_export_flag: %s", l)
		}
	}
}
