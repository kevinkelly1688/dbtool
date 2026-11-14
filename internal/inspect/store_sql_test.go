// @author: kevinkelly
// @license: Apache Licence
// @file: store_sql_test.go
// @time: 2026/10/28

package inspect

import (
	"strings"
	"testing"
)

// Latest 在 SQL 里是“报告 ID 在各实例最大 ID 里”的子查询：只算成功和失败的，按各自的报告表分别查（没有真实 MySQL 可连时只核对语句）。
func TestReportWhereLatest(t *testing.T) {
	s := &store{}
	for kind, table := range map[string]string{KindMySQL: "dbt_sql_mysql_inspection_report", KindMSSQL: "dbt_sql_mssql_inspection_report", KindRedis: "dbt_inspect_redis_report"} {
		where, args := s.reportWhere(kind, ReportFilter{AllInst: true, Latest: true, Level: "alert"})
		want := "r.id IN (SELECT MAX(l.id) FROM " + table + " l WHERE l.status IN ('finish', 'failed') GROUP BY l.instance_id)"
		if !strings.Contains(where, want) || !strings.Contains(where, "r.alert_count > 0") || len(args) != 0 {
			t.Errorf("%s：%q %v", kind, where, args)
		}
	}
	if where, _ := s.reportWhere(KindMySQL, ReportFilter{AllInst: true}); strings.Contains(where, "MAX(") {
		t.Errorf("不要最近一次时不应带子查询：%q", where)
	}
}
