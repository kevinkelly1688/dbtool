// @author: kevinkelly
// @license: Apache Licence
// @file: udf.go
// @time: 2026/10/28

package masking

import "strings"

// 非内置函数（存储函数 / 自定义函数 / 自定义表值函数）：结果列的值可能来自函数体里读到的任何字段，语句文本里看不出来，
// 保守匹配（见 conservative.go 头部）只看列名与表达式里的字段引用，函数名被排除在外，所以 `select fn_get_phone(1) as p` 会漏。
// 这里的处理是：实例上配置了脱敏字段时，select 列表里出现非内置函数调用的结果列，整列按配置里最严的一条规则脱敏。
//
// 判断“是不是内置函数”用各方言的内置函数清单（下面几张表），不在清单里的 `名字(` 一律当作自定义函数：
//   - 带限定的名字（MySQL `库.fn(`、SQL Server `dbo.fn(`、PostgreSQL `schema.fn(`）是自定义函数，系统架构（sys、pg_catalog、
//     information_schema）和 SQL Server 的 xml / hierarchyid / geography 方法除外；
//   - 清单不全只会多脱敏（把个别内置函数的结果列当成自定义函数），不会漏；
//   - ClickHouse 的内置函数有上千个、清单列不全，目前不做这项判断（自定义函数由 CREATE FUNCTION 创建，同样没有静态办法区分）。
//
// 表位置的自定义表值函数（SQL Server `cross apply dbo.f(u.id) x`、PostgreSQL `from public.get_users(1) u`）同理：
// 引用了它的别名（或没有限定、无法确定来自哪个源）的结果列、以及 `*`，整列脱敏。

// 不是函数调用的关键字：后面跟括号很常见（in (…)、over (…)、values (…)、with (nolock)）。
const keywordsBeforeParen = `in not exists any all some and or as on using over within filter group order by partition values select from where
having when then else case end between like ilike similar is with table index key force use ignore limit offset fetch top distinct union
intersect except minus join inner outer cross natural lateral apply against pivot unpivot tablesample option for window qualify into
returning set update delete insert escape collate rlike regexp div mod xor unique primary references check constraint default rows range
groups unbounded preceding following current only separator interval row array path raw root auto explicit elements type
respect nulls first last asc desc null true false do sample final prewhere settings`

// 各方言通用的内置函数、聚合 / 窗口函数与类型名（类型名后面常跟精度：varchar(20)、decimal(10,2)）。
const commonBuiltin = `abs acos asin atan atan2 avg cast ceil ceiling char_length character_length coalesce concat convert cos cot count cume_dist
current_date current_time current_timestamp current_user date_add date_sub datediff day dense_rank exp extract first_value floor greatest
hour if ifnull isnull lag last_value lead least left length ln locate log log10 log2 lower lpad ltrim max md5 min minute mod month now
nth_value ntile nullif position power radians rand rank repeat replace reverse right round row_number rpad rtrim second sign sin sqrt
stddev stddev_pop stddev_samp substr substring sum tan trim truncate upper user variance var_pop var_samp year week quarter localtime
localtimestamp session_user system_user ascii char chr hex unhex degrees pi sha1 sha2 date_format concat_ws instr lcase ucase space format
weekday dayofweek dayofmonth dayofyear
int integer bigint smallint tinyint mediumint decimal numeric dec float double real bit bool boolean varchar nvarchar nchar text ntext
tinytext mediumtext longtext blob tinyblob mediumblob longblob binary varbinary date time timestamp datetime datetime2 smalldatetime
datetimeoffset money smallmoney uniqueidentifier xml json jsonb bytea varbit signed unsigned character national timestamptz timetz
inet cidr macaddr uuid point line lseg box polygon circle int2 int4 int8 float4 float8 oid regclass regtype`

const mysqlBuiltin = `adddate addtime aes_decrypt aes_encrypt any_value benchmark bin bin_to_uuid bit_and bit_count bit_length bit_or bit_xor charset
coercibility collation compress conv convert_tz crc32 curdate curtime database day_name dayname decode des_decrypt des_encrypt elt encode
encrypt export_set extractvalue field find_in_set found_rows from_base64 from_days from_unixtime geomcollection geometrycollection
get_format get_lock group_concat grouping gtid_subset gtid_subtract icu_version inet_aton inet_ntoa inet6_aton inet6_ntoa insert
is_free_lock is_ipv4 is_ipv4_compat is_ipv4_mapped is_ipv6 is_used_lock is_uuid json_array json_array_append json_array_insert
json_arrayagg json_contains json_contains_path json_depth json_extract json_insert json_keys json_length json_merge json_merge_patch
json_merge_preserve json_object json_objectagg json_overlaps json_pretty json_quote json_remove json_replace json_schema_valid
json_schema_validation_report json_search json_set json_storage_free json_storage_size json_table json_type json_unquote json_valid
json_value last_day last_insert_id linestring load_file make_set makedate maketime master_pos_wait match mbrcontains mbrcoveredby mbrcovers
mbrdisjoint mbrequals mbrintersects mbroverlaps mbrtouches mbrwithin mid microsecond monthname multilinestring multipoint multipolygon
name_const oct octet_length old_password password period_add period_diff ps_current_thread_id ps_thread_id quote random_bytes
regexp_instr regexp_like regexp_replace regexp_substr release_all_locks release_lock row_count schema sec_to_time sleep soundex
source_pos_wait st_area st_asbinary st_asgeojson st_astext st_aswkb st_aswkt st_buffer st_centroid st_contains st_convexhull st_crosses
st_difference st_dimension st_disjoint st_distance st_distance_sphere st_endpoint st_envelope st_equals st_exteriorring st_geomcollfromtext
st_geomfromgeojson st_geomfromtext st_geomfromwkb st_geometrytype st_interiorringn st_intersection st_intersects st_isclosed st_isempty
st_issimple st_isvalid st_latfromgeohash st_latitude st_length st_longfromgeohash st_longitude st_makeenvelope st_numgeometries
st_numinteriorring st_numpoints st_overlaps st_pointfromgeohash st_pointfromtext st_pointfromwkb st_pointn st_simplify st_srid
st_startpoint st_swapxy st_symdifference st_touches st_transform st_union st_validate st_within st_x st_y std str_to_date strcmp subdate
substring_index subtime sysdate time_format time_to_sec timediff timestampadd timestampdiff to_base64 to_days to_seconds
uncompress uncompressed_length unix_timestamp updatexml utc_date utc_time utc_timestamp uuid_short uuid_to_bin validate_password_strength
value version wait_for_executed_gtid_set weight_string yearweek nvl nvl2 sys_guid
array_agg array_append array_concat array_contains array_distinct array_except array_exists array_filter array_intersect array_join
array_length array_max array_min array_position array_range array_remove array_slice array_sort array_sum array_union array_zip
approx_count_distinct approx_distinct approx_percentile arbitrary bitmap_and bitmap_contains bitmap_count bitmap_empty bitmap_from_string
bitmap_hash bitmap_max bitmap_min bitmap_or bitmap_to_string bitmap_union bitmap_union_count bitmap_xor bool_and bool_or count_if
date_trunc days_add days_diff days_sub from_unixtime_ms histogram hll_cardinality hll_empty hll_hash hll_union hll_union_agg hours_add
hours_diff hours_sub ifnull map_agg map_keys map_values max_by min_by minutes_add minutes_diff minutes_sub months_add months_diff
months_sub ndv null_or_empty not_null_or_empty percentile percentile_approx random regexp_extract regexp_extract_all seconds_add
seconds_diff seconds_sub split split_part strpos to_bitmap to_date to_hex to_unixtime try_cast typeof url_decode url_encode weeks_add
weeks_diff weeks_sub years_add years_diff years_sub`

const mssqlBuiltin = `app_name applock_mode applock_test assemblyproperty atn2 binary_checksum certencoded certprivatekey charindex checksum
checksum_agg choose col_length col_name columnproperty columns_updated compress connectionproperty context_info count_big
crypt_gen_random current_request_id current_transaction_id cursor_status database_principal_id databasepropertyex datalength date_bucket
dateadd datediff_big datefromparts datename datepart datetime2fromparts datetimefromparts datetimeoffsetfromparts datetrunc db_id db_name
decompress difference eomonth error_line error_message error_number error_procedure error_severity error_state file_id file_idex file_name
filegroup_id filegroup_name filegroupproperty fileproperty formatmessage fulltextcatalogproperty fulltextserviceproperty generate_series
get_filestream_transaction_context getansinull getdate getutcdate grouping_id hashbytes has_perms_by_name host_id host_name
ident_current ident_incr ident_seed iif index_col indexkey_property indexproperty is_member is_rolemember is_srvrolemember isdate isjson
isnumeric json_array json_modify json_object json_path_exists json_query json_value len newid newsequentialid object_definition object_id
object_name object_schema_name objectproperty objectpropertyex open_json openjson original_db_name original_login parse parsename patindex
percentile_cont percentile_disc permissions pwdcompare pwdencrypt quotename replicate rowcount_big schema_id schema_name scope_identity
serverproperty session_context sessionproperty smalldatetimefromparts soundex sql_variant_property square stats_date stdev stdevp str
string_agg string_escape string_split stuff suser_id suser_name suser_sid suser_sname switchoffset sysdatetime sysdatetimeoffset
sysutcdatetime textptr textvalid timefromparts todatetimeoffset translate try_cast try_convert try_parse type_id type_name typeproperty
unicode user_id user_name varp xact_state nchar
stgeomfromtext stgeomfromwkb stpointfromtext stlinefromtext stpolyfromtext stmpointfromtext stmlinefromtext stmpolyfromtext
stgeomcollfromtext stpointfromwkb stlinefromwkb stpolyfromwkb deserialize getroot`

// SQL Server 的 xml / hierarchyid / geography 实例方法：`col.value('/a', 'int')`、`h.GetLevel()`、`g.STAsText()`，写法上和 `架构.函数(` 一样。
const mssqlMethods = `value query exist nodes modify tostring getancestor getdescendant getlevel getroot isdescendantof reparent read write lat long z m
stasbinary stastext stasxml starea stboundary stbuffer stcentroid stcontains stconvexhull stcrosses stcurvetoline stdifference stdimension
stdisjoint stdistance stendpoint stenvelope stequals stexteriorring stgeometryn stgeometrytype stinteriorringn stintersection stintersects
stisclosed stisempty stisring stissimple stisvalid stlength stnumgeometries stnumpoints stoverlaps stpointn stpointonsurface stsrid
ststartpoint stsymdifference sttouches stunion stwithin stx sty`

const pgBuiltin = `acosd asind atan2d atand bit_length bool_and bool_or btrim cardinality cbrt clock_timestamp col_description convert_from convert_to
corr cosd cotd covar_pop covar_samp current_catalog current_database current_role current_schema current_schemas current_setting date_bin
date_part date_trunc decode div encode enum_first enum_last enum_range every factorial format_type gcd generate_series generate_subscripts
get_bit get_byte has_any_column_privilege has_column_privilege has_database_privilege has_foreign_data_wrapper_privilege
has_function_privilege has_language_privilege has_schema_privilege has_sequence_privilege has_server_privilege has_table_privilege
has_tablespace_privilege has_type_privilege host inet_client_addr inet_client_port inet_server_addr inet_server_port initcap isempty isfinite
isopen lastval lcm lower_inc lower_inf make_date make_interval make_time make_timestamp make_timestamptz masklen md5 mode netmask network
nextval num_nonnulls num_nulls obj_description octet_length overlay parse_ident percentile_cont percentile_disc pi plainto_tsquery
phraseto_tsquery quote_ident quote_literal quote_nullable random range_merge regexp_count regexp_like regexp_match regexp_matches
regexp_replace regexp_split_to_array regexp_split_to_table regexp_substr regr_avgx regr_avgy regr_count regr_intercept regr_r2 regr_slope
regr_sxx regr_sxy regr_syy repeat set_bit set_byte set_config set_masklen setseed setval setweight shobj_description split_part
starts_with statement_timestamp string_agg string_to_array string_to_table strpos substring timeofday to_ascii to_char to_date to_hex
to_json to_jsonb to_number to_regclass to_regnamespace to_regproc to_regprocedure to_regrole to_regtype to_timestamp to_tsquery to_tsvector
transaction_timestamp translate trim_array trim_scale ts_headline ts_rank ts_rank_cd tsvector_to_array unistr unnest upper_inc upper_inf
websearch_to_tsquery width_bucket xmlagg xmlcomment xmlconcat xmlelement xmlexists xmlforest xmlparse xmlpi xmlroot xmlserialize xpath
xpath_exists
array_append array_cat array_dims array_fill array_length array_lower array_ndims array_position array_positions array_prepend array_remove
array_replace array_to_json array_to_string array_to_tsvector array_upper array_agg
json_agg json_array_elements json_array_elements_text json_array_length json_build_array json_build_object json_each json_each_text
json_extract_path json_extract_path_text json_object json_object_agg json_object_keys json_populate_record json_populate_recordset
json_strip_nulls json_to_record json_to_recordset json_typeof row_to_json
jsonb_agg jsonb_array_elements jsonb_array_elements_text jsonb_array_length jsonb_build_array jsonb_build_object jsonb_each jsonb_each_text
jsonb_extract_path jsonb_extract_path_text jsonb_insert jsonb_object jsonb_object_agg jsonb_object_keys jsonb_path_exists jsonb_path_match
jsonb_path_query jsonb_path_query_array jsonb_path_query_first jsonb_populate_record jsonb_populate_recordset jsonb_pretty jsonb_set
jsonb_set_lax jsonb_strip_nulls jsonb_to_record jsonb_to_recordset jsonb_typeof
bit_and bit_or bit_xor int4range int8range numrange tsrange tstzrange daterange lower_inc upper_inc
txid_current txid_current_snapshot txid_snapshot_xmax txid_snapshot_xmin txid_snapshot_xip txid_visible_in_snapshot
pg_backend_pid pg_blocking_pids pg_column_size pg_conf_load_time pg_current_wal_lsn pg_database_size pg_get_constraintdef pg_get_expr
pg_get_functiondef pg_get_indexdef pg_get_ruledef pg_get_serial_sequence pg_get_triggerdef pg_get_userbyid pg_get_viewdef pg_has_role
pg_indexes_size pg_is_in_recovery pg_my_temp_schema pg_postmaster_start_time pg_relation_size pg_size_bytes pg_size_pretty
pg_table_is_visible pg_table_size pg_tablespace_size pg_total_relation_size pg_trigger_depth pg_typeof pg_sleep pg_sleep_for pg_sleep_until
pg_try_advisory_lock pg_advisory_lock pg_advisory_unlock pg_advisory_xact_lock pg_encoding_to_char pg_char_to_encoding
pg_collation_actual_version pg_describe_object pg_identify_object pg_input_is_valid pg_input_error_info pg_event_trigger_ddl_commands
pg_last_xact_replay_timestamp pg_last_wal_receive_lsn pg_last_wal_replay_lsn pg_wal_lsn_diff pg_current_xact_id pg_snapshot_xmin
version`

// Oracle 的内置函数（与 internal/engine/oracle 在线查询放行的内置函数一致）：在线查询的语句本身不可能调用自定义函数（引擎静态检查已拒绝），
// 这里的清单用于视图定义——视图体里可能调用存储函数，那样的结果列整列脱敏。
const oracleBuiltin = `abs acos asin atan atan2 bitand ceil cos cosh exp floor ln log mod nanvl power remainder round sign sin sinh sqrt tan tanh trunc
width_bucket ascii chr concat initcap instr instrb length lengthb lengthc lower lpad ltrim nls_initcap nls_lower nls_upper nlssort
regexp_count regexp_instr regexp_like regexp_replace regexp_substr replace rpad rtrim soundex substr substrb substrc translate trim upper
unistr dump vsize to_nchar add_months current_date current_timestamp dbtimezone extract from_tz last_day localtimestamp months_between
new_time next_day numtodsinterval numtoyminterval sessiontimezone sys_extract_utc sysdate systimestamp to_dsinterval to_yminterval tz_offset
cast chartorowid convert hextoraw rawtohex rawtonhex rowidtochar to_binary_double to_binary_float to_char to_clob to_date to_number
to_timestamp to_timestamp_tz validate_conversion coalesce decode greatest least lnnvl nullif nvl nvl2 approx_count_distinct avg collect corr
count covar_pop covar_samp cume_dist dense_rank first_value lag last_value lead listagg max median min nth_value ntile percent_rank
percentile_cont percentile_disc rank ratio_to_report row_number stddev stddev_pop stddev_samp sum var_pop var_samp variance grouping
grouping_id group_id wm_concat sys_connect_by_path connect_by_root json_array json_arrayagg json_exists json_object json_objectagg
json_query json_table json_value json_serialize xmlagg xmlattributes xmlcdata xmlcolattval xmlcomment xmlconcat xmlelement xmlforest xmlpi
xmlroot xmlserialize ora_hash standard_hash sys_context sys_guid uid user userenv bin_to_num empty_blob empty_clob compose decompose`

// 达梦在 Oracle 内置函数之外的内置函数。
const dmBuiltin = `getdate now curdate curtime dateadd datediff datepart datename isnull ifnull len charindex substring left right space str replicate year
month day hour minute second dayofweek dayofyear weekday week quarter weeks_between days_between years_between hours_between minutes_between
seconds_between ceiling rand char_length character_length octet_length bit_length lcase ucase adddays addmonths addyears addhours addminutes
addseconds curr_timestamp group_concat to_bigint to_int to_decimal`

// 系统架构：下面的限定名（pg_catalog.lower(…)、sys.fn_…(…)）是系统内置的。
var systemSchemas = map[string]bool{"sys": true, "pg_catalog": true, "information_schema": true}

var (
	kwSet    = wordSet(keywordsBeforeParen)
	commonFn = wordSet(commonBuiltin)
	mysqlFn  = wordSet(mysqlBuiltin)
	mssqlFn  = wordSet(mssqlBuiltin)
	mssqlMth = wordSet(mssqlMethods)
	pgFn     = wordSet(pgBuiltin)
	oracleFn = wordSet(oracleBuiltin)
	dmFn     = wordSet(dmBuiltin)
)

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

// isBuiltinName 判断不带限定的名字是不是该方言的关键字 / 内置函数 / 类型名。
func isBuiltinName(name string, d Dialect) bool {
	if kwSet[name] || commonFn[name] {
		return true
	}
	switch d {
	case DialectMySQL:
		return mysqlFn[name]
	case DialectMSSQL:
		return mssqlFn[name]
	case DialectPostgres:
		return pgFn[name]
	case DialectOracle:
		return oracleFn[name]
	case DialectDM:
		return oracleFn[name] || dmFn[name]
	}
	return false
}

// isUserFunc 判断一个 `名字(` 是不是自定义函数。chain 是小写的名字链（dbo.fn → ["dbo","fn"]）。
func isUserFunc(chain []string, d Dialect) bool {
	if len(chain) == 0 || d == DialectClickHouse {
		return false
	}
	name := chain[len(chain)-1]
	if name == "" || name[0] == '@' || name[0] == '#' {
		return false
	}
	if len(chain) == 1 {
		return !isBuiltinName(name, d)
	}
	if systemSchemas[chain[len(chain)-2]] {
		return false
	}
	if d == DialectMSSQL && mssqlMth[name] {
		return false // col.value(…)、h.GetLevel()：实例方法
	}
	return true
}

// callChains 取 toks 里所有函数调用的名字链：`名字 (`，名字前面带 `.` 的限定一并算进来。
// `cast(x as varchar(20))` 里 as 后面的类型不是调用。
func callChains(toks []token) [][]string {
	var out [][]string
	for i, t := range toks {
		if !isNameTok(t) || i+1 >= len(toks) || toks[i+1].kind != tPunct || toks[i+1].text != "(" {
			continue
		}
		if i > 0 && toks[i-1].kind == tIdent && toks[i-1].lower == "as" {
			continue
		}
		chain := []string{t.lower}
		j := i
		for j >= 1 && toks[j-1].kind == tPunct && toks[j-1].text == "." {
			j--
			if j >= 1 && isNameTok(toks[j-1]) {
				chain = append([]string{toks[j-1].lower}, chain...)
				j--
			} else {
				chain = append([]string{""}, chain...) // db..fn：省略架构
			}
		}
		out = append(out, chain)
	}
	return out
}

// hasUserFunc 判断 toks（一个表达式，含其中的子查询）里有没有自定义函数调用。
func hasUserFunc(toks []token, d Dialect) bool {
	if d == DialectClickHouse {
		return false
	}
	for _, ch := range callChains(toks) {
		if isUserFunc(ch, d) {
			return true
		}
	}
	return false
}

// nameChains 取表达式里不是函数调用的名字链（a、u.phone、db.u.phone），小写。
func nameChains(expr []token) [][]string {
	var out [][]string
	for i := 0; i < len(expr); i++ {
		if !isNameTok(expr[i]) {
			continue
		}
		chain := []string{expr[i].lower}
		j := i
		for j+2 < len(expr) && expr[j+1].kind == tPunct && expr[j+1].text == "." && isNameTok(expr[j+2]) {
			chain = append(chain, expr[j+2].lower)
			j += 2
		}
		if j+1 < len(expr) && expr[j+1].kind == tPunct && expr[j+1].text == "(" {
			i = j
			continue // 函数调用
		}
		out = append(out, chain)
		i = j
	}
	return out
}

// 出现在表达式里、不是字段引用的关键字（判断“有没有没限定的字段引用”时忽略）。
var exprWords = wordSet(`case when then else end and or not null is in like between exists distinct all any some true false as interval cast asc desc by
over partition order rows range unbounded preceding following current row top percent ties with date time timestamp datetime varchar nvarchar char
nchar int integer bigint smallint tinyint decimal numeric float double real bit text`)

// 表值函数后面不能当别名用的词。
var aliasStop = wordSet(`on using join inner cross outer left right full natural straight_join where group having order limit union intersect except minus
window with apply lateral tablesample pivot unpivot for option offset fetch prewhere settings format qualify partition force use ignore`)

// tvfSources 返回 toks[at:] 这条 SELECT（括号层级 dp）的 FROM 子句里自定义表值函数的别名集合（没有别名时用函数名）。
// at 是 SELECT 列表结束的位置。没有 FROM、或 FROM 里没有自定义表值函数时返回 nil。
func tvfSources(toks []token, at, dp int, d Dialect) map[string]bool {
	if d == DialectClickHouse {
		return nil
	}
	end := func(tk token) bool { // 同层上结束 FROM 子句的词
		if tk.kind == tPunct {
			return tk.text == ";"
		}
		if tk.kind != tIdent {
			return false
		}
		switch tk.lower {
		case "where", "group", "having", "order", "limit", "union", "intersect", "except", "minus", "window", "qualify", "fetch", "offset",
			"for", "option", "settings", "format", "prewhere":
			return true
		}
		return false
	}
	k := at
	for ; k < len(toks); k++ {
		tk := toks[k]
		if tk.depth < dp {
			return nil
		}
		if tk.depth != dp {
			continue
		}
		if tk.kind == tIdent && tk.lower == "from" {
			break
		}
		if end(tk) {
			return nil
		}
	}
	var out map[string]bool
	for m := k; m < len(toks); m++ {
		tk := toks[m]
		if tk.depth < dp {
			break
		}
		if tk.depth != dp {
			continue
		}
		if m > k && end(tk) {
			break
		}
		intro := tk.kind == tPunct && tk.text == "," ||
			tk.kind == tIdent && (tk.lower == "from" || tk.lower == "join" || tk.lower == "apply")
		if !intro {
			continue
		}
		p := m + 1
		for p < len(toks) && toks[p].kind == tIdent && (toks[p].lower == "lateral" || toks[p].lower == "only") {
			p++
		}
		var chain []string
		for p < len(toks) && isNameTok(toks[p]) && toks[p].depth == dp {
			chain = append(chain, toks[p].lower)
			p++
			if p < len(toks) && toks[p].kind == tPunct && toks[p].text == "." {
				for p < len(toks) && toks[p].kind == tPunct && toks[p].text == "." {
					p++
				}
				continue
			}
			break
		}
		if len(chain) == 0 || p >= len(toks) || toks[p].kind != tPunct || toks[p].text != "(" || toks[p].depth != dp || !isUserFunc(chain, d) {
			continue
		}
		q := p + 1
		for q < len(toks) && !(toks[q].kind == tPunct && toks[q].text == ")" && toks[q].depth == dp) {
			q++
		}
		alias := chain[len(chain)-1]
		r := q + 1
		if r < len(toks) && toks[r].kind == tIdent && toks[r].lower == "as" {
			r++
		}
		if r < len(toks) && isNameTok(toks[r]) && toks[r].depth == dp && !(toks[r].kind == tIdent && aliasStop[toks[r].lower]) {
			alias = toks[r].lower
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[alias] = true
	}
	return out
}

// touchesTVF 判断 select 列表的一项是不是取自自定义表值函数（别名在 tv 里）：
// `*` 和没有限定的字段引用（分不清来自哪个源）算，`别名.*`、`别名.列` 只有别名是表值函数才算。
func (it selItem) touchesTVF(tv map[string]bool) bool {
	if it.star {
		return it.starQual == "" || tv[it.starQual]
	}
	for _, ch := range it.chains {
		if len(ch) == 1 {
			if !exprWords[ch[0]] {
				return true
			}
		} else if tv[ch[len(ch)-2]] {
			return true
		}
	}
	return false
}
