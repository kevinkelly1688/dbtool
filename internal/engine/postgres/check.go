// @author: kevinkelly
// @license: Apache Licence
// @file: check.go
// @time: 2026/10/28

package postgres

import (
	"fmt"
	"strconv"
	"strings"

	"dbtool/internal/sqlx"
)

// 在线查询的静态检查（第一层防护）。
//
// 默认拒绝：只有同时满足下面全部条件的语句才放行——
//  1. 词法合法，且恰好一条语句（注释、字符串、引号标识符、美元引号串都按 PostgreSQL 规则整体识别）；
//  2. 语句类型在白名单内：SELECT / WITH … SELECT / VALUES / TABLE / EXPLAIN（不带 ANALYZE）/ SHOW；
//  3. 没有任何写操作关键字、SELECT … INTO、行级锁子句；
//  4. 没有黑名单里的函数调用（睡眠、读写服务器文件、large object、dblink、取消/终止会话、改配置、序列、咨询锁、
//     把字符串当 SQL 执行的 *_to_xml / ts_stat 等）；
//  5. 没有引用敏感系统视图 / 目录表（pg_authid、pg_shadow、pg_stat_activity …）。
//
// 通过检查后还会抽取语句涉及的 schema.table（供上层做库表级权限）。真正的只读保证来自执行层：
// 连接参数 default_transaction_read_only=on + 每次 BEGIN READ ONLY … ROLLBACK（见 run.go）。

// 语句类型。
const (
	KindSelect  = "select"  // SELECT / WITH … SELECT / VALUES / TABLE（可带外层括号、UNION 等）
	KindExplain = "explain" // EXPLAIN [(选项)] <上面的查询>，不含 ANALYZE
	KindShow    = "show"    // SHOW <参数>
)

// 默认值与上限。
const (
	DefaultSchema         = "public"
	DefaultMaxStatement   = 64 << 10 // 语句文本上限
	maxStatementHardLimit = 1 << 20
)

// CheckOptions 是 CheckUserQuery 的选项，零值可用。
type CheckOptions struct {
	// DefaultSchema 是未写 schema 的表归属的 schema（缺省 public）。
	// 默认执行时会把 search_path 固定为 "<DefaultSchema>", pg_catalog，使权限校验看到的对象与实际访问的对象一致。
	DefaultSchema string
	// KeepServerSearchPath 为 true 时不固定 search_path（按账号 / 库的默认 search_path 解析未限定的表名）。
	// 这会让“权限校验用的 schema”与“实际访问的 schema”可能不一致，除非上层自己保证，否则不要打开。
	KeepServerSearchPath bool
	// DBName 非空时，三段式名称 db.schema.table 的 db 必须等于它，否则拒绝（PostgreSQL 本身也不支持跨库引用）。
	DBName string
	// MaxStatementBytes 语句文本上限，缺省 64KB。
	MaxStatementBytes int
}

// Checked 是通过静态检查的语句。
type Checked struct {
	Kind          string          // select / explain / show
	SQL           string          // 去掉注释和结尾分号后的单条语句（用户原样的语句，不含自动补的 LIMIT）
	Tables        []sqlx.TableRef // 涉及的表 / 视图（按首次出现顺序；系统 schema 也在里面，见 IsSystemSchema）
	Functions     []string        // FROM 子句里调用的函数（如 generate_series、unnest）
	NeedAllPriv   bool            // FROM 里调用了无法判断会访问哪些表的函数（非内置的集合返回函数）；上层应要求更高权限或拒绝
	DefaultSchema string
	PinSearchPath bool // 执行时是否固定 search_path
	hasLimit      bool // 顶层已经有 LIMIT / FETCH FIRST
}

// ExecSQL 返回实际发给服务端的语句：select 类语句顶层没有 LIMIT 时补上 LIMIT maxRows+1（多取 1 行用来判断是否被截断）。
func (c *Checked) ExecSQL(maxRows int) string {
	if c.Kind == KindSelect && !c.hasLimit && maxRows > 0 {
		return c.SQL + " LIMIT " + strconv.Itoa(maxRows+1)
	}
	return c.SQL
}

// AutoLimited 返回 ExecSQL(maxRows) 是否会补 LIMIT。
func (c *Checked) AutoLimited(maxRows int) bool {
	return c.Kind == KindSelect && !c.hasLimit && maxRows > 0
}

// IsSystemSchema 判断是否系统 schema（pg_catalog、information_schema、pg_toast、pg_temp_N …）。
func IsSystemSchema(schema string) bool {
	s := strings.ToLower(schema)
	return s == "information_schema" || strings.HasPrefix(s, "pg_")
}

// ---- 拒绝清单 ----

// 语句开头关键字 → 拒绝原因。
var denyStmt = map[string]string{
	"insert": "INSERT 会写入数据", "update": "UPDATE 会修改数据", "delete": "DELETE 会删除数据", "merge": "MERGE 会修改数据",
	"truncate": "TRUNCATE 会清空表", "copy": "COPY 可以读写服务器文件或执行外部程序",
	"create": "CREATE 是 DDL", "alter": "ALTER 是 DDL", "drop": "DROP 是 DDL", "comment": "COMMENT 会修改对象注释", "security": "SECURITY LABEL 会修改安全标签",
	"grant": "GRANT 会修改权限", "revoke": "REVOKE 会修改权限", "reassign": "REASSIGN OWNED 会修改对象属主",
	"call": "CALL 会执行存储过程（可能有副作用）", "do": "DO 会执行匿名代码块",
	"set": "SET 会修改会话变量", "reset": "RESET 会修改会话变量", "discard": "DISCARD 会修改会话状态",
	"begin": "事务控制语句不允许（在线查询已经在只读事务里执行）", "start": "事务控制语句不允许（在线查询已经在只读事务里执行）",
	"commit": "事务控制语句不允许（可能逃出只读事务）", "end": "事务控制语句不允许（可能逃出只读事务）", "abort": "事务控制语句不允许",
	"rollback": "事务控制语句不允许", "savepoint": "事务控制语句不允许", "release": "事务控制语句不允许", "prepare": "PREPARE 不允许（可能 PREPARE TRANSACTION）",
	"execute": "EXECUTE 会执行预备语句", "deallocate": "DEALLOCATE 会修改会话状态",
	"declare": "DECLARE 游标不允许", "fetch": "FETCH 游标不允许", "move": "MOVE 游标不允许", "close": "CLOSE 游标不允许",
	"listen": "LISTEN 是通知机制", "unlisten": "UNLISTEN 是通知机制", "notify": "NOTIFY 是通知机制",
	"lock": "LOCK 会加表锁", "vacuum": "VACUUM 是维护命令", "analyze": "ANALYZE 会更新统计信息", "analyse": "ANALYZE 会更新统计信息",
	"cluster": "CLUSTER 会重写表", "reindex": "REINDEX 会重建索引", "refresh": "REFRESH MATERIALIZED VIEW 会写数据",
	"checkpoint": "CHECKPOINT 是维护命令", "load": "LOAD 会加载服务器上的共享库", "import": "IMPORT FOREIGN SCHEMA 会创建对象",
}

// 语句里任意位置出现（未加引号）就拒绝的关键字。写在 CTE 里的 INSERT / UPDATE / DELETE / MERGE、SELECT … INTO 靠它拦截；
// 如果这些词其实是列名 / 别名，用双引号括起来即可。
var denyWord = map[string]string{
	"insert":   "INSERT（可写 CTE 或写入语句）",
	"update":   "UPDATE（可写 CTE、FOR UPDATE 行锁）",
	"delete":   "DELETE（可写 CTE 或删除语句）",
	"merge":    "MERGE（可写 CTE 或合并语句）",
	"truncate": "TRUNCATE",
	"into":     "INTO（SELECT … INTO 会建表）",
	"uescape":  "UESCAPE（Unicode 转义）",
}

// 拒绝的函数：精确名。
var denyFuncExact = map[string]string{
	"pg_sleep": "会让会话睡眠并占住连接", "pg_sleep_for": "会让会话睡眠并占住连接", "pg_sleep_until": "会让会话睡眠并占住连接",
	"pg_terminate_backend": "会终止别人的会话", "pg_cancel_backend": "会取消别人的查询", "pg_reload_conf": "会重载服务器配置",
	"set_config": "会修改会话 / 事务配置（可能关闭只读保护）", "nextval": "会推进序列（有副作用）", "setval": "会修改序列（有副作用）",
	"setseed": "会修改会话随机数种子", "pg_notify": "会发送通知", "pg_export_snapshot": "会导出快照并持有它",
	"pg_promote": "会提升备库", "pg_switch_wal": "会切换 WAL", "pg_switch_xlog": "会切换 WAL", "pg_rotate_logfile": "会轮转日志文件",
	"pg_extension_config_dump": "会修改扩展配置", "pg_stat_file": "可以读取服务器文件信息", "ts_stat": "会把字符串当 SQL 执行",
	"pg_stat_get_activity": "会泄露其它会话的 SQL 文本", "pg_show_all_file_settings": "会泄露配置文件内容",
	"loread": "large object 读写", "lowrite": "large object 读写",
	"pg_current_logfile": "涉及服务器文件", "pg_logdir_ls": "可以列出服务器目录",
	"pg_truncate_visibility_map": "会修改可见性映射", "pg_wal_replay_pause": "会暂停备库回放", "pg_wal_replay_resume": "会恢复备库回放",
}

// 拒绝的函数：前缀。
var denyFuncPrefix = []struct{ prefix, why string }{
	{"pg_read_", "可以读取服务器文件"}, {"pg_ls_", "可以列出服务器目录"}, {"pg_file_", "可以读写服务器文件"},
	{"lo_", "large object 读写（可读写服务器文件）"}, {"dblink", "dblink 可以连接任意库并执行 SQL"}, {"postgres_fdw", "外部数据包装器"},
	{"pg_advisory", "咨询锁会占住连接并污染连接池"}, {"pg_try_advisory", "咨询锁会占住连接并污染连接池"},
	{"pg_terminate_", "会终止别人的会话"}, {"pg_cancel_", "会取消别人的查询"},
	{"pg_logical_", "逻辑复制槽读写"}, {"pg_replication_", "复制槽 / 复制源管理"}, {"pg_create_", "会创建对象 / 复制槽 / 还原点"},
	{"pg_drop_", "会删除对象 / 复制槽"}, {"pg_copy_", "会复制复制槽"}, {"pg_switch_", "会切换 WAL"}, {"pg_rotate_", "会轮转日志"},
	{"pg_start_", "备份控制"}, {"pg_stop_", "备份控制"}, {"pg_backup_", "备份控制"}, {"pg_import_", "会导入数据 / 排序规则"},
	{"pg_log_backend_", "会让其它会话输出日志"}, {"pg_stat_reset", "会重置统计信息"}, {"pg_set_", "会修改统计信息等目录数据"},
	{"pg_clear_", "会清除统计信息等目录数据"}, {"pg_restore_", "会修改统计信息等目录数据"}, {"pg_get_wal_", "可以读取 WAL 内容"},
	{"pg_stat_get_backend_", "会泄露其它会话的 SQL 文本"}, {"pg_wal_replay_", "会控制备库回放"},
}

// 拒绝的函数：名字里含有的片段（把字符串当 SQL 执行：query_to_xml、table_to_xml、cursor_to_xml …）。
var denyFuncContains = []struct{ part, why string }{
	{"_to_xml", "把表名 / 查询字符串当 SQL 执行，会绕过表级权限检查"},
}

func denyFunction(name string) string {
	if why, ok := denyFuncExact[name]; ok {
		return why
	}
	for _, p := range denyFuncPrefix {
		if strings.HasPrefix(name, p.prefix) {
			return p.why
		}
	}
	for _, p := range denyFuncContains {
		if strings.Contains(name, p.part) {
			return p.why
		}
	}
	return ""
}

// 敏感的系统视图 / 目录表：凭据、密码哈希、其它会话的 SQL、配置文件内容、连接串、large object 内容、统计样本值。
var denyRelation = map[string]string{
	"pg_authid": "含角色密码哈希", "pg_shadow": "含角色密码哈希", "pg_auth_members": "角色成员关系", "pg_db_role_setting": "角色 / 库级配置",
	"pg_user_mapping": "含外部服务器凭据", "pg_user_mappings": "含外部服务器凭据", "pg_foreign_server": "含外部服务器连接选项",
	"pg_foreign_data_wrapper": "含外部数据包装器选项", "pg_foreign_table": "含外部表选项",
	"pg_largeobject": "large object 内容", "pg_largeobject_metadata": "large object 元数据",
	"pg_statistic": "含列的采样值（绕过表权限）", "pg_statistic_ext_data": "含扩展统计的采样值（绕过表权限）",
	"pg_subscription": "含订阅连接串", "pg_stat_subscription": "订阅状态", "pg_stat_wal_receiver": "含上游连接串",
	"pg_stat_activity": "含其它会话的 SQL 文本", "pg_stat_replication": "复制连接信息",
	"pg_file_settings": "配置文件内容", "pg_hba_file_rules": "pg_hba.conf 内容", "pg_ident_file_mappings": "pg_ident.conf 内容",
	"pg_replication_origin": "复制源", "pg_replication_origin_status": "复制源", "pg_config": "服务器编译与路径信息",
}

var denyRelationPrefix = []struct{ prefix, why string }{
	{"pg_stat_statements", "含其它会话的 SQL 文本"},
}

func denyRel(name string) string {
	if why, ok := denyRelation[name]; ok {
		return why
	}
	for _, p := range denyRelationPrefix {
		if strings.HasPrefix(name, p.prefix) {
			return p.why
		}
	}
	return ""
}

// EXPLAIN 允许的选项（都不会执行语句；ANALYZE / TIMING / WAL / SERIALIZE 会执行或依赖执行，一律不放行）。
var explainOptions = map[string]bool{
	"verbose": true, "costs": true, "settings": true, "generic_plan": true, "buffers": true, "summary": true, "memory": true, "format": true,
}

// 放行的内置集合返回函数（出现在 FROM 里不影响“涉及的表”判断）。
var safeSRF = setOf("generate_series", "generate_subscripts", "unnest", "regexp_matches", "regexp_split_to_table", "string_to_table",
	"json_each", "json_each_text", "jsonb_each", "jsonb_each_text", "json_array_elements", "json_array_elements_text", "jsonb_array_elements",
	"jsonb_array_elements_text", "json_object_keys", "jsonb_object_keys", "json_to_record", "json_to_recordset", "jsonb_to_record", "jsonb_to_recordset",
	"json_populate_record", "json_populate_recordset", "jsonb_populate_record", "jsonb_populate_recordset", "jsonb_path_query", "jsonb_path_query_tz",
	"json_table", "xmltable", "ts_debug", "ts_parse", "ts_token_type", "pg_get_keywords", "pg_show_all_settings", "pg_available_extensions",
	"pg_available_extension_versions", "pg_timezone_names", "pg_timezone_abbrevs", "current_setting", "version", "now", "unnest_multirange",
	"pg_options_to_table", "pg_tablespace_databases", "pg_listening_channels", "pg_get_catalog_foreign_keys")

// FROM 里不是“表子句”的 FROM：EXTRACT(x FROM y)、SUBSTRING(x FROM n)、TRIM(BOTH x FROM y)、OVERLAY(… FROM n)。
var noFromFuncs = setOf("extract", "substring", "trim", "overlay", "position")

// FROM 列表的结束标志。
var fromTerminators = setOf("where", "group", "having", "order", "limit", "offset", "fetch", "union", "intersect", "except", "window", "for",
	"returning", "select", "into")

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func rejectMsg(why string) error {
	return uerr("在线查询只允许只读语句（SELECT / WITH … SELECT / VALUES / TABLE / EXPLAIN / SHOW）：%s。数据变更请通过「SQL 上线」提交工单。", why)
}

// CheckUserQuery 对用户输入的一条查询做静态检查与规范化，不访问数据库。
// 通过时返回 *Checked；拒绝时返回 *UserError（原因是中文，可直接展示，上层按 bad_query 处理）。
// 多于一条语句（空语句不算）一律拒绝，由上层决定是否让用户选中其中一条再运行。
func CheckUserQuery(text string, opt CheckOptions) (*Checked, error) {
	maxBytes := opt.MaxStatementBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxStatement
	}
	if maxBytes > maxStatementHardLimit {
		maxBytes = maxStatementHardLimit
	}
	if len(text) > maxBytes {
		return nil, uerr("SQL 过长（上限 %d KB）", maxBytes>>10)
	}
	toks, comments, err := lex(text)
	if err != nil {
		return nil, err
	}
	stmts := splitStatements(toks)
	switch {
	case len(stmts) == 0:
		return nil, uerr("没有有效的 SQL 语句")
	case len(stmts) > 1:
		return nil, uerr("检测到 %d 条语句，在线查询一次只能执行一条；请选中其中一条再运行。", len(stmts))
	}
	st := toks[stmts[0].from:stmts[0].to]
	if err := checkBrackets(text, st); err != nil {
		return nil, err
	}

	c := &Checked{SQL: stripComments(text, st[0].start, st[len(st)-1].end, comments)}
	c.DefaultSchema = opt.DefaultSchema
	if c.DefaultSchema == "" {
		c.DefaultSchema = DefaultSchema
	}
	c.PinSearchPath = !opt.KeepServerSearchPath

	// 语句类型
	lead := 0
	for lead < len(st) && st[lead].isPunct("(") {
		lead++
	}
	if lead >= len(st) || st[lead].kind != tWord {
		return nil, rejectMsg("语句必须以 SELECT / WITH / VALUES / TABLE / EXPLAIN / SHOW 开头")
	}
	first := st[lead]
	innerFrom := 0 // 查询主体在 st 中的起点（EXPLAIN 时跳过 EXPLAIN 头）
	switch first.low {
	case "select", "with", "values", "table":
		c.Kind = KindSelect
	case "explain":
		if lead != 0 {
			return nil, rejectMsg("EXPLAIN 不能放在括号里")
		}
		from, err := parseExplainHeader(st)
		if err != nil {
			return nil, err
		}
		c.Kind, innerFrom = KindExplain, from
	case "show":
		if lead != 0 {
			return nil, rejectMsg("SHOW 不能放在括号里")
		}
		if err := checkShow(st); err != nil {
			return nil, err
		}
		c.Kind = KindShow
	default:
		if why, ok := denyStmt[first.low]; ok {
			return nil, rejectMsg(why)
		}
		return nil, rejectMsg(fmt.Sprintf("不支持以 %s 开头的语句", strings.ToUpper(first.text)))
	}

	if err := scanDenied(st); err != nil {
		return nil, err
	}
	if c.Kind == KindShow {
		return c, nil
	}

	// 查询主体
	body := st[innerFrom:]
	bl := 0
	for bl < len(body) && body[bl].isPunct("(") {
		bl++
	}
	if bl >= len(body) || !body[bl].isOneOf("select", "with", "values", "table") {
		return nil, rejectMsg("EXPLAIN 只能解释 SELECT / WITH / VALUES / TABLE 语句")
	}
	sc := &tableScanner{toks: st, dbName: opt.DBName}
	if err := sc.scan(innerFrom); err != nil {
		return nil, err
	}
	c.hasLimit = sc.hasLimit
	seen := map[sqlx.TableRef]bool{}
	for _, parts := range sc.refs {
		var ref sqlx.TableRef
		switch len(parts) {
		case 1:
			ref = sqlx.TableRef{Schema: c.DefaultSchema, Name: parts[0]}
		case 2:
			ref = sqlx.TableRef{Schema: parts[0], Name: parts[1]}
		default: // 3 段，scan 已校验过库名
			ref = sqlx.TableRef{Schema: parts[1], Name: parts[2]}
		}
		if !seen[ref] {
			seen[ref] = true
			c.Tables = append(c.Tables, ref)
		}
	}
	for _, f := range sc.funcs {
		c.Functions = append(c.Functions, strings.Join(f.parts, "."))
		last := f.parts[len(f.parts)-1]
		builtin := len(f.parts) == 1 || (len(f.parts) == 2 && f.parts[0] == "pg_catalog")
		if !(builtin && safeSRF[last]) {
			c.NeedAllPriv = true
		}
	}
	return c, nil
}

// checkBrackets 校验 ( ) [ ] 配对。
func checkBrackets(src string, st []token) error {
	var stack []token
	for _, t := range st {
		if t.kind != tPunct {
			continue
		}
		switch t.text {
		case "(", "[":
			stack = append(stack, t)
		case ")", "]":
			want := "("
			if t.text == "]" {
				want = "["
			}
			if len(stack) == 0 || stack[len(stack)-1].text != want {
				return lexError(src, t.start, "括号不匹配（多余的 %s）", t.text)
			}
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) > 0 {
		return lexError(src, stack[len(stack)-1].start, "括号不匹配（%s 没有闭合）", stack[len(stack)-1].text)
	}
	return nil
}

// parseExplainHeader 校验 EXPLAIN [ANALYZE|VERBOSE…] / EXPLAIN (选项…) 头，返回被解释语句在 st 里的起点。
func parseExplainHeader(st []token) (int, error) {
	i := 1
	if i >= len(st) {
		return 0, rejectMsg("EXPLAIN 后面缺少语句")
	}
	if st[i].isPunct("(") {
		depth := 0
		j := i
		var cur []token
		flush := func() error {
			if len(cur) == 0 {
				return rejectMsg("EXPLAIN 选项列表为空")
			}
			name := cur[0]
			if name.kind != tWord {
				return rejectMsg("EXPLAIN 选项必须是关键字")
			}
			if name.low == "analyze" || name.low == "analyse" {
				return rejectMsg("EXPLAIN ANALYZE 会真正执行语句，不允许；请使用不带 ANALYZE 的 EXPLAIN")
			}
			if !explainOptions[name.low] {
				return rejectMsg(fmt.Sprintf("EXPLAIN 选项 %s 不在放行列表内（放行：VERBOSE / COSTS / SETTINGS / GENERIC_PLAN / BUFFERS / SUMMARY / MEMORY / FORMAT）", strings.ToUpper(name.text)))
			}
			if len(cur) > 2 {
				return rejectMsg("EXPLAIN 选项的写法不合法")
			}
			if len(cur) == 2 && cur[1].kind != tWord && cur[1].kind != tNumber && cur[1].kind != tString {
				return rejectMsg("EXPLAIN 选项的值不合法")
			}
			cur = nil
			return nil
		}
		for ; j < len(st); j++ {
			t := st[j]
			switch {
			case t.isPunct("("):
				depth++
				if depth > 1 {
					return 0, rejectMsg("EXPLAIN 选项里不允许嵌套括号")
				}
			case t.isPunct(")"):
				depth--
				if depth == 0 {
					if err := flush(); err != nil {
						return 0, err
					}
					i = j + 1
					goto done
				}
			case t.isPunct(",") && depth == 1:
				if err := flush(); err != nil {
					return 0, err
				}
			default:
				cur = append(cur, t)
			}
		}
		return 0, rejectMsg("EXPLAIN 选项括号没有闭合")
	}
	for i < len(st) && st[i].isOneOf("analyze", "analyse", "verbose") {
		if st[i].low != "verbose" {
			return 0, rejectMsg("EXPLAIN ANALYZE 会真正执行语句，不允许；请使用不带 ANALYZE 的 EXPLAIN")
		}
		i++
	}
done:
	if i >= len(st) {
		return 0, rejectMsg("EXPLAIN 后面缺少语句")
	}
	return i, nil
}

// checkShow：SHOW 后面只能是参数名（单词、点号、带引号的名字），不能有括号、字符串等。
func checkShow(st []token) error {
	if len(st) < 2 {
		return rejectMsg("SHOW 后面缺少参数名")
	}
	for _, t := range st[1:] {
		if t.kind == tWord || t.kind == tQuoted || t.isPunct(".") {
			continue
		}
		return rejectMsg("SHOW 后面只能是参数名")
	}
	return nil
}

// scanDenied 扫描整条语句里的黑名单：写操作关键字、行锁、函数、敏感系统视图。
func scanDenied(st []token) error {
	for i, t := range st {
		if t.kind == tWord {
			if why, ok := denyWord[t.low]; ok {
				return rejectMsg(fmt.Sprintf("语句中含有 %s；如果它是列名或别名，请用双引号括起来", why))
			}
			if t.low == "for" && i+1 < len(st) && st[i+1].isOneOf("share", "no", "key") {
				return rejectMsg("不允许行级锁子句（FOR SHARE / FOR KEY SHARE）")
			}
		}
		if !t.isIdentLike() {
			continue
		}
		if why := denyRel(t.low); why != "" {
			return rejectMsg(fmt.Sprintf("不允许访问系统对象 %s（%s）", t.text, why))
		}
		if i+1 < len(st) && st[i+1].isPunct("(") {
			if why := denyFunction(t.low); why != "" {
				return rejectMsg(fmt.Sprintf("不允许调用函数 %s（%s）", t.text, why))
			}
		}
	}
	return nil
}

// ---- 涉及的表 ----

type withState int

const (
	wNone      withState = iota
	wName                // 期待 CTE 名（或 RECURSIVE）
	wColsOrAs            // 已读到名字，期待 (列名…) 或 AS
	wBody                // 已读到 AS，期待 [NOT] [MATERIALIZED] (
	wAfterBody           // CTE 主体已闭合，期待 , 或主语句
)

type scope struct {
	kind      byte // 0 根 / '(' / '['
	first     int  // 本层第一个 token 的下标（WITH 只能出现在这里）
	inFrom    bool // 处在 FROM 列表中
	expect    bool // 下一个标识符应是表名 / 函数名
	noFrom    bool // 本层是 EXTRACT / SUBSTRING / TRIM / OVERLAY 的括号，里面的 FROM 不是表子句
	ctes      map[string]bool
	with      withState
	recursive bool
	pending   string // 正在定义的 CTE 名
	bodyOf    string // 本层是哪个 CTE 的主体
}

type funcRef struct{ parts []string }

type tableScanner struct {
	toks     []token
	dbName   string
	refs     [][]string
	funcs    []funcRef
	hasLimit bool
	stack    []*scope
}

func copyCTEs(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func fold(t token) string {
	if t.kind == tQuoted {
		return t.text
	}
	return t.low
}

func (s *tableScanner) top() *scope { return s.stack[len(s.stack)-1] }

func (s *tableScanner) tok(i int) (token, bool) {
	if i < 0 || i >= len(s.toks) {
		return token{}, false
	}
	return s.toks[i], true
}

// scan 词法级抽取 FROM / JOIN / TABLE 后的对象；CTE 名按 WITH 的作用域排除。
// 宁可多报（多报只会让权限校验更严）；遇到无法确定语义的 WITH 写法按“无法解析”拒绝。
func (s *tableScanner) scan(from int) error {
	s.stack = []*scope{{first: from, ctes: map[string]bool{}}}
	for i := from; i < len(s.toks); i++ {
		t := s.toks[i]
		sc := s.top()

		// WITH 子句状态机：CTE 名、列名表、AS [NOT] [MATERIALIZED] (主体)
		if sc.with != wNone {
			switch sc.with {
			case wName:
				if t.isWord("recursive") && sc.pending == "" && !sc.recursive {
					sc.recursive = true
					continue
				}
				if t.isIdentLike() {
					sc.pending, sc.with = fold(t), wColsOrAs
					continue
				}
				return uerr("无法解析 WITH 子句：CTE 名称位置不是标识符")
			case wColsOrAs:
				if t.isPunct("(") {
					depth := 0
					for ; i < len(s.toks); i++ {
						if s.toks[i].isPunct("(") {
							depth++
						} else if s.toks[i].isPunct(")") {
							depth--
							if depth == 0 {
								break
							}
						}
					}
					continue
				}
				if t.isWord("as") {
					sc.with = wBody
					continue
				}
				return uerr("无法解析 WITH 子句：CTE 名称后面缺少 AS")
			case wBody:
				if t.isOneOf("not", "materialized") {
					continue
				}
				if t.isPunct("(") {
					child := &scope{kind: '(', first: i + 1, ctes: copyCTEs(sc.ctes), bodyOf: sc.pending}
					if sc.recursive {
						child.ctes[sc.pending] = true
					}
					sc.with = wAfterBody
					s.stack = append(s.stack, child)
					continue
				}
				return uerr("无法解析 WITH 子句：AS 后面缺少括号")
			case wAfterBody:
				if t.isPunct(",") {
					sc.with, sc.pending = wName, ""
					continue
				}
				sc.with = wNone
				if !(t.isOneOf("select", "values", "table") || t.isPunct("(")) {
					return rejectMsg("WITH 的主语句必须是 SELECT / VALUES / TABLE")
				}
			}
		}

		switch {
		case t.isPunct("(") || t.isPunct("["):
			child := &scope{kind: t.text[0], first: i + 1, ctes: copyCTEs(sc.ctes)}
			if t.text == "(" {
				next, _ := s.tok(i + 1)
				derived := sc.expect && next.isOneOf("select", "with", "values", "table")
				nested := sc.expect && !derived
				child.inFrom, child.expect = nested, nested
				if prev, ok := s.tok(i - 1); ok && !sc.expect && prev.kind == tWord && noFromFuncs[prev.low] {
					child.noFrom = true
				}
				sc.expect = false
			}
			s.stack = append(s.stack, child)
		case t.isPunct(")") || t.isPunct("]"):
			if len(s.stack) <= 1 || (t.text == ")") != (sc.kind == '(') {
				return uerr("括号不匹配")
			}
			if sc.with != wNone {
				return uerr("无法解析 WITH 子句：不完整")
			}
			s.stack = s.stack[:len(s.stack)-1]
			if sc.bodyOf != "" {
				s.top().ctes[sc.bodyOf] = true
			}
		case t.isPunct(","):
			if sc.inFrom {
				sc.expect = true
			}
		case t.kind == tWord:
			switch t.low {
			case "with":
				if i == sc.first {
					sc.with, sc.pending, sc.recursive = wName, "", false
					continue
				}
			case "from":
				prev, _ := s.tok(i - 1)
				prev2, _ := s.tok(i - 2)
				isDistinctFrom := prev.isWord("distinct") && (prev2.isWord("is") || prev2.isWord("not"))
				if sc.noFrom || isDistinctFrom {
					continue
				}
				sc.inFrom, sc.expect = true, true
				continue
			case "join":
				sc.inFrom, sc.expect = true, true
				continue
			case "select":
				sc.noFrom = false
			case "table":
				prev, hasPrev := s.tok(i - 1)
				if i == sc.first || (hasPrev && prev.isOneOf("union", "intersect", "except", "all", "distinct")) {
					sc.expect = true
					continue
				}
			case "lateral", "only":
				if sc.expect {
					continue
				}
			case "rows":
				if next, ok := s.tok(i + 1); ok && sc.expect && next.isWord("from") {
					continue
				}
			case "limit", "fetch":
				if len(s.stack) == 1 {
					s.hasLimit = true
				}
			}
			if sc.inFrom && fromTerminators[t.low] {
				sc.inFrom, sc.expect = false, false
				continue
			}
			if sc.expect {
				var err error
				if i, err = s.takeName(i); err != nil {
					return err
				}
			}
		case t.kind == tQuoted:
			if sc.expect {
				var err error
				if i, err = s.takeName(i); err != nil {
					return err
				}
			}
		}
	}
	if len(s.stack) != 1 {
		return uerr("括号不匹配")
	}
	if s.top().with != wNone {
		return uerr("无法解析 WITH 子句：不完整")
	}
	return nil
}

// takeName 从 toks[i] 起读取一个（可带 schema / 库名前缀的）名字，记为表或函数；返回最后一个 token 的下标。
func (s *tableScanner) takeName(i int) (int, error) {
	sc := s.top()
	parts := []string{fold(s.toks[i])}
	j := i
	for j+2 < len(s.toks) && s.toks[j+1].isPunct(".") && s.toks[j+2].isIdentLike() {
		parts = append(parts, fold(s.toks[j+2]))
		j += 2
	}
	sc.expect = false
	if j+1 < len(s.toks) && s.toks[j+1].isPunct("(") { // 函数（集合返回函数）
		s.funcs = append(s.funcs, funcRef{parts: parts})
		return j, nil
	}
	switch len(parts) {
	case 1:
		if sc.ctes[parts[0]] {
			return j, nil // CTE 引用，不是表
		}
	case 2:
	case 3:
		if s.dbName != "" && parts[0] != s.dbName {
			return j, uerr("不支持跨库引用 %s.%s.%s（PostgreSQL 的连接绑定在一个库上，请切换到目标库后查询）", parts[0], parts[1], parts[2])
		}
	default:
		return j, uerr("不支持的对象名称：%s", strings.Join(parts, "."))
	}
	s.refs = append(s.refs, parts)
	return j, nil
}
