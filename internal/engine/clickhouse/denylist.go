// @author: kevinkelly
// @license: Apache Licence
// @file: denylist.go
// @time: 2026/10/28

package clickhouse

import "strings"

// 在线查询的拒绝 / 放行清单（固定在代码里，不可配置）。小写比较。

// denyTableFuncs 是已知的表函数里一律拒绝的那些：读服务器文件、访问外部网络 / 外部系统、
// 连接其它集群（绕过表级权限）、产生无限数据或可以执行外部程序。
// 表位置（FROM / JOIN / IN 之后）的表函数采用“默认拒绝”，只有 allowedTableFuncs 里的放行；
// 这份清单用来覆盖函数出现在表位置之外（如 IN file(...)）的情况，并给出明确的拒绝理由。
var denyTableFuncs = map[string]string{
	// 读服务器本地文件
	"file": "读服务器本地文件", "filecluster": "读服务器本地文件",
	// 访问外部网络 / 对象存储 / 外部数据库
	"url": "访问外部网络", "urlcluster": "访问外部网络",
	"s3": "访问对象存储", "s3cluster": "访问对象存储", "gcs": "访问对象存储",
	"hdfs": "访问 HDFS", "hdfscluster": "访问 HDFS",
	"azureblobstorage": "访问对象存储", "azureblobstoragecluster": "访问对象存储",
	"deltalake": "访问数据湖", "deltalakecluster": "访问数据湖", "deltalakes3": "访问数据湖", "deltalakes3cluster": "访问数据湖",
	"iceberg": "访问数据湖", "icebergcluster": "访问数据湖", "icebergs3": "访问数据湖", "icebergs3cluster": "访问数据湖",
	"icebergazure": "访问数据湖", "icebergazurecluster": "访问数据湖", "icebergbhdfs": "访问数据湖", "iceberghdfs": "访问数据湖",
	"icebergbhdfscluster": "访问数据湖", "iceberghdfscluster": "访问数据湖",
	"hudi": "访问数据湖", "hudicluster": "访问数据湖", "paimon": "访问数据湖", "paimons3": "访问数据湖", "paimoncluster": "访问数据湖",
	"hive":  "访问外部系统",
	"mysql": "访问外部数据库", "postgresql": "访问外部数据库", "mongodb": "访问外部数据库", "mongodbatlas": "访问外部数据库",
	"jdbc": "访问外部数据库", "odbc": "访问外部数据库", "sqlite": "读服务器本地文件", "redis": "访问外部系统",
	"meilisearch": "访问外部系统", "ytsaurus": "访问外部系统", "arrowflight": "访问外部系统", "prometheusquery": "访问外部系统",
	"prometheusqueryrange": "访问外部系统",
	// 连接其它节点 / 集群，可绕过库表级权限
	"remote": "连接其它服务器", "remotesecure": "连接其它服务器", "cluster": "连接其它集群", "clusterallreplicas": "连接其它集群",
	// 执行外部程序、读取调用方给的数据
	"executable": "执行外部程序", "input": "读取客户端输入",
	// 无限 / 超大数据源，或解析范围不可知
	"generaterandom": "产生无限数据", "numbers_mt": "多线程生成超大数据", "zeros": "产生无限数据", "zeros_mt": "产生无限数据",
	"loop": "产生无限数据", "generate_series": "产生超大数据", "generateseries": "产生超大数据", "null": "不在在线查询范围内",
	"merge": "按正则匹配任意多张表，无法静态确定涉及的表", "mergetreeindex": "读取内部索引", "mergetreeanalyzeindexes": "读取内部索引",
	"mergetreeprojection": "读取内部结构", "dictionary": "请直接 FROM 字典名", "view": "不在在线查询范围内", "viewifpermitted": "不在在线查询范围内",
	"viewexplain": "不在在线查询范围内", "format": "不在在线查询范围内",
	"fuzzjson": "不在在线查询范围内", "fuzzquery": "不在在线查询范围内",
	"timeseriesdata": "不在在线查询范围内", "timeseriesmetrics": "不在在线查询范围内", "timeseriestags": "不在在线查询范围内",
	"timeseriesselector": "不在在线查询范围内", "timeseriesrange": "不在在线查询范围内",
}

// 注意：format 既是表函数也是常用的字符串格式化函数 format('{}', x)。表位置之外的 format(...) 是普通函数，必须放行，
// 因此“位置无关”的函数拒绝只检查 denyScalarFuncs 与 denyTableFuncs 去掉 scalarOK 之后的部分。
var scalarOK = map[string]bool{"format": true}

// allowedTableFuncs 是表位置允许使用的表函数。numbers 的参数还要通过 checkNumbersArgs。
var allowedTableFuncs = map[string]bool{"numbers": true, "values": true}

// numbersMax 是 numbers(...) 允许的最大数值。
const numbersMax = 100_000_000

// denyScalarFuncs 是在任何位置都拒绝的（标量）函数：睡眠 / 阻塞、可连接远端、调试用崩溃函数。
// 未在此列的函数不做限制（由 readonly=1 + 各类限额兜底）。管理员配置的可执行 UDF 不在静态检查能力范围内。
var denyScalarFuncs = map[string]string{
	"sleep":              "会让查询睡眠，占住连接和线程",
	"sleepeachrow":       "会让查询睡眠，占住连接和线程",
	"hascolumnintable":   "带远端参数时会连接其它服务器",
	"catboostevaluate":   "会读取服务器上的模型文件",
	"trap":               "调试用函数，会使服务异常",
	"modelevaluate":      "会读取服务器上的模型文件",
	"executesqlfromfile": "会读取服务器上的文件",
}

// dictGet 系列函数的第一个参数是字典名（字符串常量），joinGet 的第一个参数是 Join 表名；它们读取的对象要计入“涉及的表”。
func isDictFunc(lname string) bool {
	return strings.HasPrefix(lname, "dictget") || lname == "dicthas" || lname == "dictisin"
}

func isJoinGetFunc(lname string) bool { return lname == "joinget" || lname == "joingetornull" }

// allowedSystemTables：system 库里在线查询只放行这些库表元数据的只读摘要。
// 其余（users / roles / grants / settings_profiles / quotas / row_policies / query_log / processes / clusters /
// named_collections / zookeeper / dictionaries / disks / mutations / replicas / settings / numbers …）一律拒绝，
// 因为它们含有他人的 SQL、账号权限、凭据、集群拓扑或文件路径，或者能产生无限数据。
var allowedSystemTables = map[string]bool{
	"one": true, "databases": true, "tables": true, "columns": true, "parts": true, "parts_columns": true,
	"data_skipping_indices": true, "data_type_families": true, "table_engines": true, "time_zones": true,
}

// information_schema 是对上面这些 system 表的视图，整库放行。
func isSystemDB(db string) bool { return strings.EqualFold(db, "system") }
func isInfoSchemaDB(db string) bool {
	return strings.EqualFold(db, "information_schema")
}

// denyEngines：这些引擎的表会读服务器文件、访问外部网络 / 外部系统、执行外部程序或产生无限数据。
// 语句里引用的表在执行前用 system.tables 查一次引擎，命中则拒绝。
var denyEngines = map[string]string{
	"file": "读服务器本地文件", "filelog": "读服务器本地文件", "url": "访问外部网络", "s3": "访问对象存储", "s3queue": "访问对象存储",
	"hdfs": "访问 HDFS", "azureblobstorage": "访问对象存储", "azurequeue": "访问对象存储",
	"deltalake": "访问数据湖", "deltalakes3": "访问数据湖", "iceberg": "访问数据湖", "icebergs3": "访问数据湖", "hudi": "访问数据湖", "hive": "访问外部系统",
	"mysql": "访问外部数据库", "postgresql": "访问外部数据库", "materializedpostgresql": "访问外部数据库", "mongodb": "访问外部数据库",
	"jdbc": "访问外部数据库", "odbc": "访问外部数据库", "sqlite": "读服务器本地文件", "redis": "访问外部系统",
	"kafka": "消费外部消息队列（会移动消费位点）", "rabbitmq": "消费外部消息队列（会移动消费位点）", "nats": "消费外部消息队列（会移动消费位点）",
	"executable": "执行外部程序", "executablepool": "执行外部程序", "generaterandom": "产生无限数据", "ytsaurus": "访问外部系统",
	"arrowflight": "访问外部系统", "meilisearch": "访问外部系统",
}

// 语句首词分类，用于给出明确的拒绝原因。
var denyFirstWord = map[string]string{
	"insert": "写入", "alter": "修改表结构", "create": "创建对象", "drop": "删除对象", "truncate": "清空表", "rename": "重命名",
	"optimize": "触发合并（OPTIMIZE）", "kill": "终止查询 / 变更", "system": "服务器管理命令", "attach": "挂载对象", "detach": "卸载对象",
	"grant": "授权", "revoke": "回收权限", "set": "修改会话设置", "use": "切换库（库由页面顶部的下拉框选择）", "check": "校验表（CHECK）",
	"backup": "备份", "restore": "恢复", "exchange": "交换表", "undrop": "恢复被删除的表", "watch": "实时视图订阅（WATCH）",
	"delete": "删除数据", "update": "修改数据", "replace": "写入", "move": "移动数据", "call": "调用过程", "begin": "事务控制",
	"commit": "事务控制", "rollback": "事务控制", "unlock": "锁", "lock": "锁", "analyze": "分析", "vacuum": "维护",
}
