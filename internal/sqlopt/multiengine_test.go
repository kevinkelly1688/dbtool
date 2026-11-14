// @author: kevinkelly
// @license: Apache Licence
// @file: multiengine_test.go
// @time: 2026/10/28

package sqlopt

import "testing"

// SQL 分析 / 优化工具只对 MySQL 系开放执行计划。
func TestMultiEngineTypesUnsupported(t *testing.T) {
	for _, ty := range []string{"pgsql", "mongo", "clickhouse", "oracle", "dm"} {
		if explainDBTypes[ty] {
			t.Errorf("%s 不应支持 SQL 分析 / 优化", ty)
		}
	}
}
