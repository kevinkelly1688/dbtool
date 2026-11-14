// @author: kevinkelly
// @license: Apache Licence
// @file: check_createtable_test.go
// @time: 2026/10/31

package sqlflow

import (
	"strings"
	"testing"
)

func TestNativeCreateTableScope(t *testing.T) {
	create := "create table t (id int)"
	for _, ty := range []string{"polardb-x", "polardbx", "selectdb", "doris", "adb"} {
		if !nativeCreateTable(ty, create) || !nativeCreateTable(ty, "CREATE TABLE a (id int);\ncreate external table b (x int);") {
			t.Errorf("%s 的建表语句应走原生", ty)
		}
		if nativeCreateTable(ty, create+"; alter table t add c int") || nativeCreateTable(ty, "insert into t values (1)") || nativeCreateTable(ty, "") {
			t.Errorf("%s 混了别的语句 / 不是建表，不该走原生建表流程", ty)
		}
	}
	for _, ty := range []string{"mysql", "polardb", "mssql", "redis", "pgsql", ""} {
		if nativeCreateTable(ty, create) {
			t.Errorf("%s 不在范围内", ty)
		}
	}
}

func TestCreateTableSyntaxError(t *testing.T) {
	ok := []string{
		"create table t (id int)",
		"CREATE TABLE IF NOT EXISTS `shop`.`orders` (\n  `id` bigint NOT NULL AUTO_INCREMENT COMMENT 'a, b (c)',\n  `n` varchar(64) DEFAULT NULL,\n  PRIMARY KEY (`id`),\n  KEY `idx_n` (`n`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='订单'",
		"create table shop.t like shop.s",
		// Doris / SelectDB
		"CREATE TABLE IF NOT EXISTS example_tbl (\n k1 TINYINT, k2 DECIMAL(10, 2) DEFAULT '10.5', k3 CHAR(10) COMMENT 'string column', k4 INT NOT NULL DEFAULT '1'\n) ENGINE=OLAP UNIQUE KEY(k1, k2) COMMENT 'my first table' DISTRIBUTED BY HASH(k1) BUCKETS 32 PROPERTIES ('replication_num' = '1', 'storage_format' = 'V2')",
		"create table t2 distributed by hash(id) buckets 4 properties('replication_num'='1') as select id from t1",
		"create table t3 as select * from t1",
		// PolarDB-X / ADB
		"CREATE TABLE t (id int, name varchar(20), PRIMARY KEY (id)) DBPARTITION BY HASH(id) TBPARTITION BY HASH(id) TBPARTITIONS 4",
		"CREATE TABLE t (id bigint, INDEX idx(id) USING INVERTED, PRIMARY KEY (id)) DISTRIBUTE BY HASH(id) INDEX_ALL='Y'",
		"create temporary table tmp (a int)",
	}
	for _, s := range ok {
		if msg := createTableSyntaxError(s); msg != "" {
			t.Errorf("应通过：%q → %s", s, msg)
		}
	}
	bad := map[string]string{
		"create table (id int)":                       "缺少表名",
		"create table t":                              "缺少列定义",
		"create table t (id int":                      "缺少右括号",
		"create table t (id int))":                    "多了一个右括号",
		"create table t ()":                           "至少要有一列",
		"create table t (id int,)":                    "最后多了一个逗号",
		"create table t (id int,, name varchar(10))":  "多余的逗号",
		"create table t (id)":                         "缺少数据类型",
		"create table t (id int, primary)":            "约束 / 索引定义不完整",
		"create table t (id int) comment 'abc":        "缺少结束的单引号",
		"create table `t (id int)":                    "缺少结束的引号",
		"create table t like":                         "LIKE 后面缺少表名",
		"create table t like s extra":                 "LIKE 其他表之后不能再写别的内容",
		"create table t engine=olap":                  "表名后面应是列定义",
		"create table if exists t (id int)":           "IF NOT EXISTS",
		"create table a. (id int)":                    "“.”后面缺少名字",
		"create table t (1 int)":                      "必须以列名开头",
		"create tabel t (id int)":                     "缺少 TABLE",
		"create table t (id 123)":                     "数据类型写法不对",
		"create table t (id int) properties ('a'='b'": "缺少右括号",
	}
	for s, want := range bad {
		if msg := createTableSyntaxError(s); !strings.Contains(msg, want) {
			t.Errorf("%q 应报“%s”，实得 %q", s, want, msg)
		}
	}
}
