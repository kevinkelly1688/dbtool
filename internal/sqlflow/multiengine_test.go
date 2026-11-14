// @author: kevinkelly
// @license: Apache Licence
// @file: multiengine_test.go
// @time: 2026/10/28

package sqlflow

import "testing"

// 回归：workflowSupports 曾借用 query.SupportsQuery 表示“MySQL 协议系”。在线查询接入 PostgreSQL / MongoDB / ClickHouse 之后，
// 这三类实例不能因此进入 SQL 工单（它们第一期只开放在线查询与数据字典）。
func TestWorkflowSupportsIsExplicitWhitelist(t *testing.T) {
	for _, ty := range []string{"mysql", "polardb", "polardb-x", "polardbx", "selectdb", "doris", "adb", "mssql", "redis"} {
		if !workflowSupports(ty) {
			t.Errorf("%s 应支持 SQL 工单", ty)
		}
	}
	for _, ty := range []string{"pgsql", "mongo", "clickhouse", "oracle", "dm", "cassandra", "elasticsearch", "opensearch", "memcached", "tdengine", ""} {
		if workflowSupports(ty) {
			t.Errorf("%s 不应支持 SQL 工单", ty)
		}
	}
}
