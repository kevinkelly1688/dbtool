// @author: kevinkelly
// @license: Apache Licence
// @file: multiengine_test.go
// @time: 2026/10/28

package slowlog

import "testing"

// 慢日志只采集 MySQL 系（及 Redis 的专用通道）：PostgreSQL / MongoDB / ClickHouse 不在任何采集 / 优化建议白名单里。
func TestMultiEngineTypesUnsupported(t *testing.T) {
	for _, ty := range []string{"pgsql", "mongo", "clickhouse", "oracle", "dm"} {
		if collectTypes[ty] || platformTypes[ty] || adviseTypes[ty] {
			t.Errorf("%s 不应在慢日志白名单里", ty)
		}
	}
}
