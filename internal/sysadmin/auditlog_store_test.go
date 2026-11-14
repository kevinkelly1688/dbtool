// @author: kevinkelly
// @license: Apache Licence
// @file: auditlog_store_test.go
// @time: 2026/10/28

package sysadmin

import (
	"testing"
	"time"
)

// 逐个调用审计扩展的 store 方法，保证 SQL 能生成并执行，且关键语句符合预期（假驱动不连真实数据库）。
func TestAuditExtStoreExecutesStatements(t *testing.T) {
	s, d := newRecStore(t)
	now := time.Now()
	start := now.Add(-time.Hour)

	_, _, err := s.ListAuditLogsExt(AuditLogFilter{Action: "登入", User: "a_b%", Search: "x", Start: &start, End: &now, Limit: 10},
		AuditExtFilter{Category: "login", Result: "失败", Risky: true})
	must(t, "ListAuditLogsExt", err)
	for _, want := range []string{"LEFT JOIN dbt_audit_ext x ON x.audit_id = a.id", "x.category = ?", "x.result = ?", "x.risky = 1", "a.action_time >= ?"} {
		if !d.has(want) {
			t.Errorf("审计扩展查询缺少 %q: %v", want, d.logs)
		}
	}
	d.reset()
	if r, err := s.GetAuditRowExt(1); err != nil || r != nil {
		t.Errorf("空结果应返回 nil: %v %v", r, err)
	}
	_, err = s.RecentAuditsByUser("alice", 7, 6)
	must(t, "RecentAuditsByUser", err)
	if !d.has("a.user_name = ? AND a.id <> ?") {
		t.Errorf("同用户其他操作应排除本行: %v", d.logs)
	}
	d.reset()
	_, err = s.AuditOverviewData(now)
	must(t, "AuditOverviewData", err)
	if !d.has("GROUP BY d") || !d.has("action = ?") {
		t.Errorf("概览应按天汇总并统计登录失败: %v", d.logs)
	}
	d.reset()
	_, err = s.SQLWorkflowStatusCounts(SQLWorkflowFilter{Status: "workflow_finish", GroupID: 1, Search: "x", Start: &start, End: &now})
	must(t, "SQLWorkflowStatusCounts", err)
	if !d.has("GROUP BY `w`.`status`") && !d.has("GROUP BY w.status") {
		t.Errorf("状态分布应按状态分组: %v", d.logs)
	}

	// 被拒绝的语句表：假驱动里表“不存在”（HasTable 查询返回空结果集），所以只验证不报错
	if s.QueryRejectReady() || s.AuditExtReady() {
		t.Error("假驱动下表探测应为 false")
	}
	_, _, err = s.ListQueryRejects(QueryLogFilter{User: "u", Instance: "i", Search: "s", Start: &start, End: &now}, 20)
	must(t, "ListQueryRejects", err)
	if !d.has("LEFT JOIN dbt_sql_instance i ON i.id = r.instance_id") || !d.has("r.created_at >= ?") {
		t.Errorf("被拒绝语句应关联实例名并按时间过滤: %v", d.logs)
	}
	_, err = s.GetQueryReject(3)
	must(t, "GetQueryReject", err)
	_, err = s.GetQueryLog(3)
	must(t, "GetQueryLog", err)
	_, err = s.QueryAuditStats(QueryLogFilter{User: "u", Instance: "i", Search: "s", Start: &start, End: &now})
	must(t, "QueryAuditStats", err)
	if !d.has("CAST(cost_time AS DECIMAL(14,4))") {
		t.Errorf("耗时是文本，应按数值计算: %v", d.logs)
	}
}
