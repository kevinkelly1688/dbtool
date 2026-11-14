// @author: kevinkelly
// @license: Apache Licence
// @file: multiengine_test.go
// @time: 2026/10/28

package inspect

import "testing"

// 巡检只支持 MySQL 系 / SQL Server / Redis：PostgreSQL / MongoDB / ClickHouse 没有巡检种类，不会出现在巡检的实例范围里。
func TestMultiEngineTypesHaveNoInspectionKind(t *testing.T) {
	for _, ty := range []string{"pgsql", "mongo", "clickhouse", "oracle", "dm"} {
		if k, ok := KindOf(ty); ok {
			t.Errorf("%s 不应有巡检种类，却得到 %q", ty, k)
		}
	}
	if _, ok := KindOf("mysql"); !ok {
		t.Error("mysql 应有巡检种类")
	}
}
