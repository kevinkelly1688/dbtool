// @author: kevinkelly
// @license: Apache Licence
// @file: catalog.go
// @time: 2026/10/28

package giconfig

import "fmt"

// 参数目录：goInception config.toml 里已知的配置项、类型与取值范围，用来校验和生成表单。
// 取自 goInception 官方文档的“配置文件”一节；不同版本的参数会有增减，所以目录里没有的参数只给警告、不拦截，
// 类型写错（把布尔写成字符串之类）则一定会让 goInception 起不来，按错误拦截。

type paramKind int

const (
	kBool paramKind = iota
	kInt
	kFloat
	kString
	kEnum
	kList // 字符串数组，如 custom_keywords = ["a", "b"]
)

func (k paramKind) String() string {
	switch k {
	case kBool:
		return "bool"
	case kInt:
		return "int"
	case kFloat:
		return "float"
	case kEnum:
		return "enum"
	case kList:
		return "list"
	}
	return "string"
}

type param struct {
	Section string
	Key     string
	Kind    paramKind
	Min     *float64
	Max     *float64
	Enum    []string
	Desc    string
	// Warn 在值有风险时返回警告文案（空表示没问题）。
	Warn func(v any) string
}

func rng(lo, hi float64) (*float64, *float64) { return &lo, &hi }

var params = map[string]*param{}

func reg(p param) {
	params[paramID(p.Section, p.Key)] = &p
}

func lookupParam(section, key string) *param { return params[paramID(section, key)] }

func warnIfTrue(msg string) func(any) string {
	return func(v any) string {
		if b, ok := v.(bool); ok && b {
			return msg
		}
		return ""
	}
}

func warnIfFalse(msg string) func(any) string {
	return func(v any) string {
		if b, ok := v.(bool); ok && !b {
			return msg
		}
		return ""
	}
}

func warnIfAbove(limit int64, msg string) func(any) string {
	return func(v any) string {
		if n, ok := v.(int64); ok && n > limit {
			return msg
		}
		return ""
	}
}

func init() {
	pos := func(lo, hi float64) (*float64, *float64) { return rng(lo, hi) }
	b := func(sec, key, desc string, w func(any) string) {
		reg(param{Section: sec, Key: key, Kind: kBool, Desc: desc, Warn: w})
	}
	i := func(sec, key, desc string, lo, hi float64, w func(any) string) {
		mn, mx := pos(lo, hi)
		reg(param{Section: sec, Key: key, Kind: kInt, Min: mn, Max: mx, Desc: desc, Warn: w})
	}
	f := func(sec, key, desc string, lo, hi float64) {
		mn, mx := pos(lo, hi)
		reg(param{Section: sec, Key: key, Kind: kFloat, Min: mn, Max: mx, Desc: desc})
	}
	s := func(sec, key, desc string) { reg(param{Section: sec, Key: key, Kind: kString, Desc: desc}) }
	e := func(sec, key, desc string, vals ...string) {
		reg(param{Section: sec, Key: key, Kind: kEnum, Enum: vals, Desc: desc})
	}
	const maxInt = 2147483647

	// ---- [inc] 审核规则 ----
	b("inc", "check_primary_key", "表必须有主键", warnIfFalse("关闭“表必须有主键”：无主键的表将不再被拦截"))
	b("inc", "check_table_comment", "表必须有注释", nil)
	b("inc", "check_column_comment", "列必须有注释", nil)
	b("inc", "check_insert_field", "INSERT 必须指定列", nil)
	b("inc", "check_dml_where", "UPDATE/DELETE 必须有 WHERE", warnIfFalse("关闭“DML 必须有 WHERE”：无条件的 UPDATE/DELETE 将不再被拦截"))
	b("inc", "check_dml_limit", "DML 带 LIMIT", nil)
	b("inc", "check_dml_orderby", "DML 带 ORDER BY 时报警", nil)
	b("inc", "check_autoincrement_datatype", "自增列类型检查", nil)
	b("inc", "check_autoincrement_init_value", "自增列初始值检查", nil)
	b("inc", "check_autoincrement_name", "自增列名称检查", nil)
	b("inc", "check_column_default_value", "列必须有默认值", nil)
	b("inc", "check_column_position_change", "列位置变更检查", nil)
	b("inc", "check_column_type_change", "列类型变更检查", nil)
	b("inc", "check_float_double", "FLOAT/DOUBLE 类型检查", nil)
	b("inc", "check_identifier", "标识符字符检查", nil)
	b("inc", "check_implicit_type_conversion", "隐式类型转换检查", nil)
	b("inc", "check_index_prefix", "索引名前缀检查", nil)
	b("inc", "check_timestamp_count", "timestamp 列个数检查", nil)
	b("inc", "check_timestamp_default", "timestamp 列默认值检查", nil)
	b("inc", "column_name_candidate", "列名候选检查", nil)
	b("inc", "columns_must_have_index", "指定列必须有索引", nil)
	b("inc", "enable_autoincrement_unsigned", "自增列必须无符号", nil)
	b("inc", "enable_blob_not_null", "允许 BLOB/TEXT 列 NOT NULL", nil)
	b("inc", "enable_blob_type", "允许 BLOB/TEXT 类型", nil)
	b("inc", "enable_column_charset", "允许列单独指定字符集", nil)
	b("inc", "enable_drop_database", "允许 DROP DATABASE", warnIfTrue("允许 DROP DATABASE：误操作会直接删库，请确认确有需要"))
	b("inc", "enable_drop_table", "允许 DROP TABLE", warnIfTrue("允许 DROP TABLE：误操作会直接删表，请确认确有需要"))
	b("inc", "enable_enum_set_bit", "允许 ENUM/SET/BIT 类型", nil)
	b("inc", "enable_fingerprint", "启用 SQL 指纹", nil)
	b("inc", "enable_foreign_key", "允许外键", nil)
	b("inc", "enable_identifer_keyword", "允许使用关键字作标识符", nil)
	b("inc", "enable_json_type", "允许 JSON 类型", nil)
	b("inc", "enable_minimal_rollback", "最小化回滚", nil)
	b("inc", "enable_nullable", "允许列可为 NULL", nil)
	b("inc", "enable_null_index_name", "允许索引不指定名称", nil)
	b("inc", "enable_orderby_rand", "允许 ORDER BY RAND()", nil)
	b("inc", "enable_partition_table", "允许分区表", nil)
	b("inc", "enable_pk_columns_only_int", "主键列只能是整型", nil)
	b("inc", "enable_select_star", "允许 SELECT *", nil)
	b("inc", "enable_set_charset", "允许 SET CHARSET", nil)
	b("inc", "enable_set_collation", "允许 SET COLLATION", nil)
	b("inc", "enable_set_engine", "允许指定存储引擎", nil)
	b("inc", "enable_timestamp_type", "允许 timestamp 类型", nil)
	b("inc", "enable_use_view", "允许使用视图", nil)
	b("inc", "enable_zero_date", "允许零日期", warnIfTrue("允许零日期（0000-00-00）：与 MySQL 严格模式冲突，容易导致后续写入失败"))
	b("inc", "general_log", "记录 general log", nil)
	b("inc", "merge_alter_table", "合并同表的多条 ALTER", nil)
	// 整数：-1 / 0 / 1（-1 为不设置），不是布尔值（goInception 官方参数说明与 config 默认值均如此）
	i("inc", "sql_safe_updates", "sql_safe_updates（-1 / 0 / 1，-1 为不设置）", -1, 1, nil)
	// 以下取自 goInception 上游最新版的配置结构与参数说明；目录仍是子集，没收录的参数只给警告
	b("inc", "enable_alter_database", "允许 ALTER DATABASE", nil)
	b("inc", "enable_any_statement", "enable_any_statement", nil)
	b("inc", "enable_change_column", "允许 CHANGE COLUMN", nil)
	b("inc", "enable_ddl_instant", "enable_ddl_instant", nil)
	b("inc", "enable_sql_statistic", "enable_sql_statistic", nil)
	b("inc", "check_read_only", "check_read_only", nil)
	b("inc", "check_identifier_upper", "标识符必须大写", nil)
	b("inc", "check_identifier_lower", "标识符必须小写", nil)
	b("inc", "check_index_column_repeat", "check_index_column_repeat", nil)
	b("inc", "hex_blob", "hex_blob", nil)
	b("inc", "skip_grant_table", "skip_grant_table", nil)
	s("inc", "index_prefix", "普通索引名前缀")
	s("inc", "uniq_index_prefix", "唯一索引名前缀")
	s("inc", "sql_mode", "会话 sql_mode")
	s("inc", "disable_types", "禁用的数据类型（逗号分隔）")
	s("inc", "ignore_osc_alter_stmt", "不走在线改表的 ALTER 子句（逗号分隔）")
	reg(param{Section: "inc", Key: "custom_keywords", Kind: kList, Desc: "自定义关键字（字符串数组）"})
	i("inc", "max_execution_time", "max_execution_time", 0, maxInt, nil)
	b("inc", "enable_remote_backup", "启用远程备份库（不同 goInception 版本可能不识别，请对照官方文档）", nil)
	i("inc", "max_update_rows", "单条 DML 最大影响行数", 0, maxInt, warnIfAbove(1000000, "max_update_rows 超过 100 万：单条 DML 影响行数过大，容易造成长事务和主从延迟"))
	i("inc", "max_insert_rows", "单条 INSERT 最大行数", 0, maxInt, nil)
	i("inc", "max_keys", "单表最大索引数", 0, 64, nil)
	i("inc", "max_key_parts", "单个索引最大列数", 0, 16, nil)
	i("inc", "max_primary_key_parts", "主键最大列数", 0, 16, nil)
	i("inc", "max_char_length", "CHAR 列最大长度", 0, 255, nil)
	i("inc", "max_ddl_affect_rows", "DDL 最大影响行数", 0, maxInt, nil)
	i("inc", "max_allowed_packet", "max_allowed_packet", 0, 1<<30, nil)
	i("inc", "lock_wait_timeout", "lock_wait_timeout（秒）", 0, 31536000, nil)
	i("inc", "wait_timeout", "wait_timeout（秒）", 0, 31536000, nil)
	i("inc", "backup_port", "备份库端口", 1, 65535, nil)
	s("inc", "backup_host", "备份库地址")
	s("inc", "backup_user", "备份库账号")
	s("inc", "backup_password", "备份库密码")
	s("inc", "default_charset", "默认字符集")
	s("inc", "must_have_columns", "必须包含的列（逗号分隔）")
	s("inc", "skip_sqls", "跳过的语句（分号分隔）")
	s("inc", "support_charset", "允许的字符集（逗号分隔）")
	s("inc", "support_collation", "允许的排序规则（逗号分隔）")
	s("inc", "support_engine", "允许的存储引擎（逗号分隔）")
	s("inc", "table_prefix", "表名前缀")
	e("inc", "lang", "提示信息语言", "zh-CN", "en-US")
	e("inc", "explain_rule", "影响行数估算规则", "first", "max")

	// ---- [osc] pt-online-schema-change ----
	b("osc", "osc_on", "启用 pt-osc", nil)
	s("osc", "osc_bin_dir", "pt-online-schema-change 所在目录")
	i("osc", "osc_min_table_size", "触发 pt-osc 的最小表大小（MB）", 0, maxInt, nil)
	i("osc", "osc_check_interval", "检查间隔（秒）", 0, 86400, nil)
	i("osc", "osc_chunk_size", "chunk-size", 1, maxInt, nil)
	f("osc", "osc_chunk_time", "chunk-time（秒）", 0, 3600)
	f("osc", "osc_chunk_size_limit", "chunk-size-limit", 0, 1000)
	i("osc", "osc_critical_thread_connected", "critical Threads_connected 阈值", 0, maxInt, nil)
	i("osc", "osc_critical_thread_running", "critical Threads_running 阈值", 0, maxInt, nil)
	i("osc", "osc_max_thread_connected", "max Threads_connected 阈值", 0, maxInt, nil)
	i("osc", "osc_max_thread_running", "max Threads_running 阈值", 0, maxInt, nil)
	i("osc", "osc_max_lag", "最大主从延迟（秒）", 0, 86400, nil)
	b("osc", "osc_drop_new_table", "失败时删除新表", nil)
	b("osc", "osc_drop_old_table", "完成后删除旧表", nil)
	b("osc", "osc_check_alter", "检查 ALTER 语句", nil)
	b("osc", "osc_check_replication_filters", "检查复制过滤规则", nil)
	b("osc", "osc_check_unique_key_change", "检查唯一键变更", nil)
	b("osc", "osc_print_none", "不打印 pt-osc 输出", nil)
	b("osc", "osc_print_sql", "打印 pt-osc 的 SQL", nil)
	s("osc", "osc_alter_foreign_keys_method", "外键处理方式")
	s("osc", "osc_recursion_method", "从库发现方式")
	i("osc", "osc_max_flow_ctl", "osc_max_flow_ctl（默认 -1）", -1, maxInt, nil)
	f("osc", "osc_sleep", "osc_sleep", 0, 3600)
	i("osc", "osc_lock_wait_timeout", "锁等待超时（秒）", 0, maxInt, nil)

	// ---- [ghost] gh-ost ----
	b("ghost", "ghost_on", "启用 gh-ost", nil)
	s("ghost", "ghost_bin_dir", "gh-ost 所在目录")
	b("ghost", "ghost_allow_on_master", "允许在主库上运行", nil)
	b("ghost", "ghost_assume_rbr", "假定 binlog 为 ROW 格式", nil)
	i("ghost", "ghost_chunk_size", "chunk-size", 1, maxInt, nil)
	b("ghost", "ghost_concurrent_rowcount", "并发统计行数", nil)
	s("ghost", "ghost_cut_over", "cut-over 方式")
	i("ghost", "ghost_cut_over_lock_timeout_seconds", "cut-over 锁超时（秒）", 0, 3600, nil)
	i("ghost", "ghost_default_retries", "默认重试次数", 0, 1000, nil)
	b("ghost", "ghost_exact_rowcount", "精确统计行数", nil)
	i("ghost", "ghost_max_lag_millis", "最大主从延迟（毫秒）", 0, maxInt, nil)
	f("ghost", "ghost_nice_ratio", "nice-ratio", 0, 100)
	b("ghost", "ghost_timestamp_old_table", "旧表名带时间戳", nil)
	s("ghost", "ghost_postpone_cut_over_flag_file", "延后 cut-over 的标记文件")
	i("ghost", "ghost_heartbeat_interval_millis", "心跳间隔（毫秒）", 0, maxInt, nil)
	b("ghost", "ghost_approve_renamed_columns", "自动确认列重命名", nil)
	i("ghost", "ghost_exponential_backoff_max_interval", "指数退避最大间隔（秒）", 0, maxInt, nil)
	i("ghost", "ghost_dml_batch_size", "DML 批大小", 0, maxInt, nil)
	b("ghost", "ghost_ok_to_drop_table", "允许删除旧表", nil)
	b("ghost", "ghost_skip_foreign_key_checks", "跳过外键检查", nil)

	// ---- [log] ----
	e("log", "level", "日志级别", "debug", "info", "warn", "error", "fatal")
	s("log", "format", "日志格式")
	b("log", "disable-timestamp", "日志不带时间戳", nil)
	s("log.file", "filename", "日志文件")
	i("log.file", "max-size", "单个日志文件大小上限（MB）", 0, maxInt, nil)
	i("log.file", "max-days", "日志保留天数", 0, 36500, nil)
	i("log.file", "max-backups", "日志保留个数", 0, maxInt, nil)
	b("log.file", "log-rotate", "按大小滚动日志", nil)
}

// 已知的配置段；其余的段给警告。
// inc_level 是审核级别段：键是错误码名（如 er_table_must_have_pk），值是整数级别，由 Validate 单独检查。
var knownSections = map[string]bool{
	"inc": true, "inc_level": true, "osc": true, "ghost": true, "log": true, "log.file": true,
	"status": true, "performance": true, "security": true, "prepared-plan-cache": true,
	"proxy-protocol": true, "tikv-client": true, "binlog": true, "txn-local-latches": true,
}

// 顶层与其他段里常见的、非 goInception 特有的参数（继承自 TiDB 的服务配置），不值得给“未知参数”警告。
var knownGeneric = map[string]bool{
	"host": true, "port": true, "socket": true, "path": true, "lease": true, "token-limit": true,
	"advertise-address": true, "ignore_sighup": true, "skip_grant_table": true, "security.skip_grant_table": true,
	"status.report-status": true, "status.status-host": true, "status.status-port": true, "status.metrics-addr": true,
	"status.metrics-interval": true, "performance.max-procs": true, "performance.tcp-keep-alive": true,
	"security.ssl-ca": true, "security.ssl-cert": true, "security.ssl-key": true,
}

// FormItem / FormGroup 是“常用参数”表单的描述，由服务端下发，前端不再重复维护一份。
type FormItem struct {
	Section string   `json:"section"`
	Key     string   `json:"key"`
	Kind    string   `json:"kind"`
	Label   string   `json:"label"`
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	Enum    []string `json:"enum,omitempty"`
	Secret  bool     `json:"secret,omitempty"`
}

type FormGroup struct {
	Title string     `json:"title"`
	Tag   string     `json:"tag"`
	Items []FormItem `json:"items"`
}

// FormGroups 与原型的“常用参数”四个分组一致。
func FormGroups() []FormGroup {
	mk := func(sec, key, label string) FormItem {
		p := lookupParam(sec, key)
		it := FormItem{Section: sec, Key: key, Label: label}
		if p != nil {
			it.Kind, it.Min, it.Max, it.Enum = p.Kind.String(), p.Min, p.Max, p.Enum
		}
		it.Secret = IsSensitiveKey(key)
		return it
	}
	return []FormGroup{
		{Title: "审核规则", Tag: "[inc]", Items: []FormItem{
			mk("inc", "check_primary_key", "表必须有主键"), mk("inc", "check_table_comment", "表必须有注释"),
			mk("inc", "check_column_comment", "列必须有注释"), mk("inc", "check_insert_field", "INSERT 必须指定列"),
			mk("inc", "check_dml_where", "UPDATE/DELETE 必须有 WHERE"), mk("inc", "check_dml_limit", "DML 带 LIMIT"),
			mk("inc", "max_update_rows", "单条 DML 最大影响行数"), mk("inc", "max_keys", "单表最大索引数"),
			mk("inc", "max_char_length", "max_char_length"), mk("inc", "max_primary_key_parts", "max_primary_key_parts"),
		}},
		{Title: "备份与回滚", Tag: "[inc]", Items: []FormItem{
			mk("inc", "backup_host", "backup_host"), mk("inc", "backup_port", "backup_port"), mk("inc", "backup_user", "backup_user"),
			mk("inc", "backup_password", "backup_password"), mk("inc", "enable_remote_backup", "enable_remote_backup"),
		}},
		{Title: "在线改表", Tag: "[osc] / [ghost]", Items: []FormItem{
			mk("osc", "osc_on", "osc_on　启用 pt-osc"), mk("osc", "osc_min_table_size", "osc_min_table_size（MB）"),
			mk("osc", "osc_bin_dir", "osc_bin_dir"), mk("ghost", "ghost_on", "ghost_on　启用 gh-ost"),
		}},
		{Title: "语言与日志", Tag: "[inc] / [log]", Items: []FormItem{
			mk("inc", "lang", "lang"), mk("log", "level", "log level"),
		}},
	}
}

func describeRange(p *param) string {
	if p.Min != nil && p.Max != nil {
		return fmt.Sprintf("%g 到 %g", *p.Min, *p.Max)
	}
	return ""
}
