// @author: kevinkelly
// @license: Apache Licence
// @file: funcs.go
// @time: 2026/10/28

package dm

import "strings"

func setOf(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// parenKeywords 是后面可以直接跟左括号、但不是函数调用的关键字（IN (...)、EXISTS (...)、OVER (...)、PARTITION BY (...) 等）。
// 其余“词 + (”一律当函数调用，必须在 builtinFuncs 里；类型名（NUMBER(10,2) 等）见 typeParenWords。
var parenKeywords = setOf(
	"select", "from", "where", "and", "or", "not", "in", "exists", "any", "all", "some", "on", "using", "join", "union", "intersect", "minus", "except",
	"over", "partition", "subpartition", "by", "within", "group", "keep", "distinct", "unique", "case", "when", "then", "else", "as", "with",
	"between", "like", "is", "order", "having", "connect", "start", "prior", "pivot", "unpivot", "for", "sample", "seed", "values", "lateral",
	"outer", "inner", "cross", "natural", "apply", "columns", "nested", "path", "top",
)

// collisionKeywords 是 parenKeywords 里不属于保留字的那些：它们和内置函数一样，理论上可能被同名的自定义函数 / 包 / 同义词盖过，
// 所以执行前要核对库里没有同名对象（见 Checked.CallNames）。
var collisionKeywords = setOf("over", "partition", "subpartition", "within", "keep", "pivot", "unpivot", "sample", "seed", "lateral", "join", "using",
	"apply", "outer", "inner", "cross", "natural", "columns", "nested", "path")

// typeParenWords 是带精度的类型名（CAST(x AS NUMBER(10,2))、INTERVAL '1' DAY(3) TO SECOND(6)）；只在类型位置放行（见 typeContext）。
var typeParenWords = setOf("number", "varchar2", "nvarchar2", "varchar", "char", "nchar", "float", "decimal", "dec", "numeric", "raw", "timestamp",
	"day", "second", "year", "month", "hour", "minute", "datetime", "time", "binary", "varbinary")

// dmExtraFuncs 是达梦在 Oracle 内置函数之外常用的内置函数（SQL Server / MySQL 风格的别名也在里面）。
var dmExtraFuncs = setOf(
	"getdate", "now", "curdate", "curtime", "dateadd", "datediff", "datepart", "datename", "isnull", "ifnull", "len", "charindex", "substring", "left", "right",
	"space", "str", "replicate", "year", "month", "day", "hour", "minute", "second", "dayofweek", "dayofyear", "weekday", "week", "quarter", "weeks_between",
	"days_between", "years_between", "hours_between", "minutes_between", "seconds_between", "ceiling", "rand", "char_length", "character_length",
	"octet_length", "bit_length", "lcase", "ucase", "adddays", "addmonths", "addyears", "addhours", "addminutes", "addseconds", "curr_timestamp",
	"group_concat", "to_bigint", "to_int", "to_decimal", "sysdate", "systimestamp",
)

// funcAllowed 判断函数名（小写）是不是放行的内置函数：Oracle 兼容的那一批，加上达梦自己的。
func funcAllowed(lname string) bool { return builtinFuncs[lname] || dmExtraFuncs[lname] }

// builtinFuncs 是在线查询放行的内置函数（小写）。默认拒绝：自定义函数、包里的函数（schema.包.函数）、类型方法不在其中。
// 原因：函数按定义者权限执行，调用别人 schema 里的存储函数等于借用对方的权限，可以绕过只读账号和库表授权。
var builtinFuncs = setOf(
	// 数值
	"abs", "acos", "asin", "atan", "atan2", "bitand", "ceil", "cos", "cosh", "exp", "floor", "ln", "log", "mod", "nanvl", "power", "remainder", "round",
	"sign", "sin", "sinh", "sqrt", "tan", "tanh", "trunc", "width_bucket",
	// 字符
	"ascii", "chr", "concat", "initcap", "instr", "instrb", "length", "lengthb", "lengthc", "lower", "lpad", "ltrim", "nls_initcap", "nls_lower", "nls_upper",
	"nlssort", "regexp_count", "regexp_instr", "regexp_like", "regexp_replace", "regexp_substr", "replace", "rpad", "rtrim", "soundex", "substr", "substrb",
	"substrc", "translate", "trim", "upper", "unistr", "dump", "vsize", "to_nchar",
	// 日期时间
	"add_months", "current_date", "current_timestamp", "dbtimezone", "extract", "from_tz", "last_day", "localtimestamp", "months_between", "new_time",
	"next_day", "numtodsinterval", "numtoyminterval", "sessiontimezone", "sys_extract_utc", "sysdate", "systimestamp", "to_dsinterval", "to_yminterval",
	"tz_offset",
	// 转换
	"cast", "chartorowid", "convert", "hextoraw", "rawtohex", "rawtonhex", "rowidtochar", "to_binary_double", "to_binary_float", "to_char", "to_clob",
	"to_date", "to_number", "to_timestamp", "to_timestamp_tz", "validate_conversion",
	// 空值与条件
	"coalesce", "decode", "greatest", "least", "lnnvl", "nullif", "nvl", "nvl2",
	// 聚合与分析
	"approx_count_distinct", "avg", "collect", "corr", "count", "covar_pop", "covar_samp", "cume_dist", "dense_rank", "first_value", "lag", "last_value", "lead",
	"listagg", "max", "median", "min", "nth_value", "ntile", "percent_rank", "percentile_cont", "percentile_disc", "rank", "ratio_to_report", "row_number",
	"stddev", "stddev_pop", "stddev_samp", "sum", "var_pop", "var_samp", "variance", "grouping", "grouping_id", "group_id", "wm_concat",
	// 层次查询
	"sys_connect_by_path", "connect_by_root",
	// JSON
	"json_array", "json_arrayagg", "json_exists", "json_object", "json_objectagg", "json_query", "json_table", "json_value", "json_serialize",
	// XML（只放行构造类；XMLQUERY / XMLTABLE / XMLEXISTS 可以通过 XQuery 的 ora:view 读任意表，拒绝）
	"xmlagg", "xmlattributes", "xmlcdata", "xmlcolattval", "xmlcomment", "xmlconcat", "xmlelement", "xmlforest", "xmlpi", "xmlroot", "xmlserialize",
	// 其它
	"ora_hash", "standard_hash", "sys_context", "sys_guid", "uid", "user", "userenv", "bin_to_num", "empty_blob", "empty_clob", "compose", "decompose",
)

// denyWords 是不管出现在哪里（不在字符串 / 引号标识符里）都拒绝的词：写操作、SELECT … INTO、序列取值。
// 这些在达梦里基本都是保留字（MERGE 除外），不加引号不可能是列名。
var denyWords = map[string]string{
	"insert":   "INSERT 会修改数据",
	"update":   "UPDATE / FOR UPDATE 会修改数据或加行锁",
	"delete":   "DELETE 会修改数据",
	"merge":    "MERGE 会修改数据",
	"drop":     "DROP 会修改结构",
	"create":   "CREATE 会修改结构",
	"alter":    "ALTER 会修改结构或会话",
	"truncate": "TRUNCATE 会清空数据",
	"grant":    "GRANT 会修改权限",
	"revoke":   "REVOKE 会修改权限",
	"into":     "INTO 子句（SELECT … INTO / INSERT INTO）不属于只读查询",
	"nextval":  "序列的 NEXTVAL 会推进序列，不是只读操作",
	"currval":  "序列的 CURRVAL 依赖会话里的 NEXTVAL，在线查询不支持序列",
}

// denyFirstWord 是语句开头不允许的词（给出更明确的提示）。
var denyFirstWord = map[string]string{
	"insert": "会修改数据", "update": "会修改数据", "delete": "会修改数据", "merge": "会修改数据", "create": "会修改结构", "alter": "会修改结构或会话",
	"drop": "会修改结构", "truncate": "会清空数据", "grant": "会修改权限", "revoke": "会修改权限", "commit": "会提交事务", "rollback": "会回滚事务",
	"savepoint": "会修改事务状态", "begin": "语句块可以执行任意操作", "declare": "语句块可以执行任意操作", "exec": "会调用存储过程",
	"execute": "会执行存储过程或动态 SQL", "call": "会调用存储过程", "lock": "会锁表", "set": "会修改会话", "analyze": "会收集统计信息",
	"audit": "会修改审计设置", "noaudit": "会修改审计设置", "comment": "会修改注释", "rename": "会修改结构", "flashback": "会恢复数据或结构",
	"purge": "会清理数据", "use": "会切换会话上下文", "desc": "是客户端命令，不是 SQL", "describe": "是客户端命令，不是 SQL",
	"show": "是客户端命令，不是 SQL", "spool": "是客户端命令，不是 SQL", "disconnect": "会断开连接", "checkpoint": "会触发检查点",
	"backup": "会备份数据库", "restore": "会还原数据库", "shutdown": "会关闭数据库",
}

// systemSchemas 是不允许在线查询的系统 schema（大写）：SYS 存放系统表（SYSOBJECTS、SYSUSERS……），SYSSSO / SYSAUDITOR 是安全 / 审计员，
// CTISYS 是全文检索。SYSDBA 是普通的 DBA 账号（业务表常放在它的 schema 里），不在其中。
var systemSchemas = setOf("SYS", "SYSSSO", "SYSAUDITOR", "CTISYS", "SYSTEM", "PUBLIC")

// isSystemSchema 判断是不是系统 schema。
func isSystemSchema(schema string) bool { return systemSchemas[strings.ToUpper(schema)] }

// dictViews 是免库表授权的数据字典视图（只返回账号自己能看到的对象的元数据）：未写 schema（或写 SYS.）时放行。
// 其余的 ALL_* / USER_*（ALL_SOURCE 的源码、ALL_DB_LINKS 等）与 DBA_* / V$* 一律拒绝。
var dictViews = setOf(
	"DUAL", "DICT", "DICTIONARY",
	"ALL_TABLES", "ALL_TAB_COLUMNS", "ALL_TAB_COMMENTS", "ALL_COL_COMMENTS", "ALL_OBJECTS", "ALL_INDEXES", "ALL_IND_COLUMNS", "ALL_CONSTRAINTS",
	"ALL_CONS_COLUMNS", "ALL_USERS", "ALL_VIEWS", "ALL_SEQUENCES", "ALL_SYNONYMS",
	"USER_TABLES", "USER_TAB_COLUMNS", "USER_TAB_COMMENTS", "USER_COL_COMMENTS", "USER_OBJECTS", "USER_INDEXES", "USER_IND_COLUMNS", "USER_CONSTRAINTS",
	"USER_CONS_COLUMNS", "USER_VIEWS", "USER_SEQUENCES", "USER_SYNONYMS",
)

// internalName 判断名字是不是数据库内部视图 / 表（DBA_*、V$*、GV$*、X$*、CDB_*）；未写 schema 或写 SYS. 时才用它。
func internalName(name string) bool {
	u := strings.ToUpper(name)
	for _, p := range []string{"DBA_", "V$", "GV$", "X$", "CDB_"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

// IsSystemSchema 判断是不是系统 schema（库表树里不展示，也不允许查询）。
func IsSystemSchema(schema string) bool { return isSystemSchema(schema) }
