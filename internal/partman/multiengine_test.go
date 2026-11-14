// @author: kevinkelly
// @license: Apache Licence
// @file: multiengine_test.go
// @time: 2026/10/28

package partman

import "testing"

// 分区维护支持 MySQL / PolarDB（MySQL 方言）与 SelectDB / Doris（Doris 方言）：PostgreSQL / MongoDB / ClickHouse 不能出现在它的实例下拉里，直接访问也提示“暂不支持”。
func TestMultiEngineTypesUnsupported(t *testing.T) {
	for _, ty := range []string{"pgsql", "mongo", "clickhouse", "mssql", "redis", "oracle", "dm", "polardb-x", "adb"} {
		if SupportedDBType(ty) {
			t.Errorf("%s 不应支持分区维护", ty)
		}
	}
	for _, ty := range []string{"mysql", "polardb", "selectdb", "doris", "SelectDB", " Doris "} {
		if !SupportedDBType(ty) {
			t.Errorf("%s 应支持", ty)
		}
	}
	if DialectOf("doris") != DialectDoris || DialectOf("selectdb") != DialectDoris || DialectOf("polardb") != DialectMySQL {
		t.Error("方言映射不对")
	}
}
