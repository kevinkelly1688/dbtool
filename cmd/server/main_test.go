// @author: kevinkelly
// @license: Apache Licence
// @file: main_test.go
// @time: 2026/10/28

package main

import (
	"slices"
	"testing"

	"dbtool/internal/engine"
)

// 回归：曾因漏掉引擎包的空导入，线上所有查询都返回“暂不支持的数据库类型”，
// 而单元测试用的是假引擎，没能发现。
func TestEnginesRegisteredInServerBinary(t *testing.T) {
	got := engine.Supported()
	for _, want := range []string{"mysql", "polardb", "selectdb", "doris", "adb", "mssql", "redis", "pgsql", "mongo", "clickhouse", "oracle", "dm"} {
		if !slices.Contains(got, want) {
			t.Errorf("服务二进制里没有注册引擎 %q（已注册: %v）", want, got)
		}
	}
}
