// @author: kevinkelly
// @license: Apache Licence
// @file: multiengine_test.go
// @time: 2026/10/28

package archive

import "testing"

// 数据归档支持 MySQL 系（MySQL / MariaDB / PolarDB / PolarDB-X），其它类型不支持。
func TestArchiveSupportedTypes(t *testing.T) {
	for _, ty := range []string{"mysql", "mariadb", "polardb", "polardb-x", "polardbx"} {
		if !supportedDBTypes[ty] {
			t.Errorf("%s 应支持数据归档", ty)
		}
	}
	for _, ty := range []string{"pgsql", "mongo", "clickhouse", "mssql", "redis", "doris", "selectdb", "adb", "oracle", "dm"} {
		if supportedDBTypes[ty] {
			t.Errorf("%s 不应支持数据归档", ty)
		}
	}
	// 只有 PolarDB-X 走“兼容实现”降级；MySQL / MariaDB / PolarDB 保持严格
	for ty, want := range map[string]bool{"mysql": false, "mariadb": false, "polardb": false, "polardb-x": true, "polardbx": true} {
		if lenientDBTypes[ty] != want {
			t.Errorf("%s 的 lenient 应为 %v", ty, want)
		}
	}
}
