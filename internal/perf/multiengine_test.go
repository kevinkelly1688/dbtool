// @author: kevinkelly
// @license: Apache Licence
// @file: multiengine_test.go
// @time: 2026/10/28

package perf

import "testing"

// 性能采集只支持 MySQL 协议系：PostgreSQL / MongoDB / ClickHouse 不能出现在实例下拉里，也不能创建采集配置。
func TestMultiEngineTypesUnsupported(t *testing.T) {
	for _, ty := range []string{"pgsql", "mongo", "clickhouse", "mssql", "redis", "oracle", "dm"} {
		if contains(SupportedDBTypes, ty) {
			t.Errorf("%s 不应支持性能采集", ty)
		}
	}
	if !contains(SupportedDBTypes, "mysql") {
		t.Error("mysql 应支持")
	}
}
