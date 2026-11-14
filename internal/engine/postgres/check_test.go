// @author: kevinkelly
// @license: Apache Licence
// @file: check_test.go
// @time: 2026/10/28

package postgres

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"dbtool/internal/sqlx"
)

func TestCheckAllow(t *testing.T) {
	cases := []string{
		"select 1",
		"SELECT 1;",
		"select 1 ;  ;",
		"  \n select 1 -- trailing comment",
		"/* lead */ select 1 /* x */",
		"select * from t",
		"select a, b from public.t where a > 1 order by b limit 10",
		"select a from t1 join t2 on t1.id = t2.id left join t3 using (id)",
		"select count(*), max(created_at) from orders group by status having count(*) > 1",
		"select 'a;b'",
		"select 'it''s; fine'",
		"select \"weird;name\" from t",
		"select $$ ; drop table t; $$",
		"select $tag$ ; delete from t $tag$",
		"select E'a\\'; drop table t; --'",
		"select 1 /* ; drop table t */",
		"select 1 /* nested /* ; */ ; */",
		"select \"update\", \"delete\" from t",
		"select x as \"into\" from t",
		"select current_setting('server_version')",
		"select version(), now(), current_user",
		"select pg_backend_pid()",
		"select pg_database_size('postgres'), pg_total_relation_size('t')",
		"select txid_current_snapshot()",
		"select * from generate_series(1, 10) g",
		"select unnest(array[1,2,3])",
		"select * from unnest(array[1,2]) as u(x)",
		"select jsonb_build_object('a', 1) ->> 'a'",
		"select '{\"a\":1}'::jsonb @> '{\"a\":1}'",
		"select extract(year from created_at) from t",
		"select substring(name from 1 for 3) from t",
		"select trim(both ' ' from name) from t",
		"select a is distinct from b from t",
		"select a is not distinct from b from t",
		"select overlay(name placing 'x' from 2) from t",
		"select * from t where id in (select id from u)",
		"select * from t where exists (select 1 from u where u.id = t.id)",
		"select (select max(x) from u) from t",
		"select * from (select a from t) s",
		"select * from t, lateral (select * from u where u.id = t.id limit 1) l",
		"select * from t cross join lateral generate_series(1, t.n) g",
		"select * from t tablesample system (10)",
		"select * from only t",
		"with c as (select 1 as x) select * from c",
		"with recursive r(n) as (select 1 union all select n+1 from r where n < 5) select * from r",
		"with a as (select 1), b as (select * from a) select * from b",
		"with a as materialized (select 1) select * from a",
		"with a as not materialized (select 1) select * from a",
		"values (1, 'a'), (2, 'b')",
		"table t",
		"table public.t limit 5",
		"(select 1) union (select 2)",
		"select 1 union all select 2 order by 1",
		"select a from t fetch first 5 rows only",
		"select a from t offset 5",
		"select row_number() over (partition by a order by b) from t",
		"select a from t window w as (partition by b)",
		"select case when a > 1 then 'x' else 'y' end from t",
		"select a::text, cast(b as int), c::numeric(10,2) from t",
		"select array_agg(a order by b) filter (where a > 0) from t",
		"select * from information_schema.tables",
		"select * from pg_catalog.pg_class",
		"select * from pg_roles",
		"select * from pg_tables where schemaname = 'public'",
		"select * from pg_stat_user_tables",
		"select n_live_tup from pg_stat_all_tables",
		"select * from pg_locks",
		"select 1e5, 0x1F, 1_000, .5, 3.14",
		"select 'x' collate \"C\"",
		"select a #> '{b,c}' from t",
		"select a -> 'b' ->> 'c' from t",
		"select 1 where 1 = 1 and 2 <> 3 and 4 >= 4",
		"select a from t where b ~ '^x' or b !~* 'y' or b similar to 'z%'",
		"select a from t where b like '%\\%'",
		"select U from t",
		"select 'é', 'テスト' from t",
		"select * from \"Schema\".\"Table With Space\"",
		"select * from é.ü",
		"explain select * from t",
		"explain verbose select * from t",
		"explain (verbose, costs off) select * from t",
		"explain (format json) select * from t",
		"explain (format json, buffers, summary) select * from t",
		"explain (generic_plan) select * from t where id = 1",
		"explain with c as (select 1) select * from c",
		"show server_version",
		"show all",
		"show transaction isolation level",
		"show time zone",
		"show search_path",
		"SHOW \"TimeZone\"",
	}
	for _, sql := range cases {
		if _, err := CheckUserQuery(sql, CheckOptions{}); err != nil {
			t.Errorf("应放行 %q，被拒绝：%v", sql, err)
		}
	}
}

func TestCheckDeny(t *testing.T) {
	cases := []struct{ sql, why string }{
		// 空 / 多语句
		{"", "没有有效"},
		{"   ", "没有有效"},
		{"-- only comment", "没有有效"},
		{";", "没有有效"},
		{"select 1; select 2", "2 条语句"},
		{"select 1; drop table t", "2 条语句"},
		{"select 1;\n-- c\ndelete from t", "2 条语句"},
		{"select 1 /* ; */; delete from t", "2 条语句"},
		{"select 1 -- x\n; delete from t", "2 条语句"},
		{"select '\\'; drop table t; --'", "2 条语句"}, // 普通字符串里反斜杠不是转义：'\' 在第二个引号结束
		{"select 1;; select 2", "2 条语句"},
		// 写 / DDL / 管理语句
		{"insert into t values (1)", "INSERT"},
		{"update t set a = 1", "UPDATE"},
		{"delete from t", "DELETE"},
		{"merge into t using u on true when matched then delete", "MERGE"},
		{"truncate t", "TRUNCATE"},
		{"create table t (a int)", "DDL"},
		{"create temp table t as select 1", "DDL"},
		{"alter table t add column x int", "DDL"},
		{"drop table t", "DDL"},
		{"comment on table t is 'x'", "COMMENT"},
		{"grant select on t to u", "GRANT"},
		{"revoke select on t from u", "REVOKE"},
		{"copy t to program 'ls'", "COPY"},
		{"copy (select 1) to '/tmp/x'", "COPY"},
		{"copy t from stdin", "COPY"},
		{"call p()", "CALL"},
		{"do $$ begin perform 1; end $$", "DO"},
		{"set statement_timeout = 0", "SET"},
		{"set local role admin", "SET"},
		{"set session characteristics as transaction read write", "SET"},
		{"set transaction read write", "SET"},
		{"reset all", "RESET"},
		{"discard all", "DISCARD"},
		{"begin", "事务控制"},
		{"begin read write", "事务控制"},
		{"start transaction", "事务控制"},
		{"commit", "事务控制"},
		{"end", "事务控制"},
		{"abort", "事务控制"},
		{"rollback", "事务控制"},
		{"savepoint a", "事务控制"},
		{"release savepoint a", "事务控制"},
		{"prepare t as select 1", "PREPARE"},
		{"prepare transaction 'x'", "PREPARE"},
		{"execute p", "EXECUTE"},
		{"deallocate p", "DEALLOCATE"},
		{"declare c cursor for select 1", "DECLARE"},
		{"fetch 1 from c", "FETCH"},
		{"move forward 1 in c", "MOVE"},
		{"close c", "CLOSE"},
		{"listen ch", "LISTEN"},
		{"notify ch", "NOTIFY"},
		{"unlisten *", "UNLISTEN"},
		{"lock table t", "LOCK"},
		{"vacuum t", "VACUUM"},
		{"analyze t", "ANALYZE"},
		{"analyse t", "ANALYZE"},
		{"cluster t", "CLUSTER"},
		{"reindex table t", "REINDEX"},
		{"refresh materialized view mv", "REFRESH"},
		{"checkpoint", "CHECKPOINT"},
		{"load 'x'", "LOAD"},
		{"import foreign schema s from server x into y", "IMPORT"},
		{"security label on table t is 'x'", "SECURITY"},
		{"reassign owned by a to b", "REASSIGN"},
		{"values_x", "不支持以"},
		{"foo bar", "不支持以"},
		{"'select' 1", "SELECT / WITH"},
		{"1", "SELECT / WITH"},
		{"(1)", "SELECT / WITH"},
		{"((insert into t values (1)))", "SELECT / WITH"},
		// 可写 CTE / SELECT INTO / 行锁
		{"with x as (insert into t values (1) returning *) select * from x", "INSERT"},
		{"with x as (update t set a = 1 returning *) select * from x", "UPDATE"},
		{"with x as (delete from t returning *) select * from x", "DELETE"},
		{"with x as (select 1) delete from t", "DELETE"},
		{"with x as (select 1) insert into t select * from x", "INSERT"},
		{"with x as (select 1) update t set a = 1", "UPDATE"},
		{"with x as (merge into t using u on true when matched then delete returning *) select * from x", "MERGE"},
		{"select * from (with x as (delete from t returning *) select * from x) s", "DELETE"},
		{"select * into new_t from t", "INTO"},
		{"select 1 into new_t", "INTO"},
		{"select a, b into temp x from t", "INTO"},
		{"select * from t for update", "UPDATE"},
		{"select * from t for no key update", "行级锁"},
		{"select * from t for share", "行级锁"},
		{"select * from t for key share", "行级锁"},
		{"select * from t for share nowait", "行级锁"},
		{"with x as (select 1) select * from x for update", "UPDATE"},
		{"select e'\\u0041' uescape '!'", "UESCAPE"},
		// EXPLAIN / SHOW
		{"explain analyze select * from t", "EXPLAIN ANALYZE"},
		{"explain analyse select * from t", "EXPLAIN ANALYZE"},
		{"explain (analyze) select * from t", "EXPLAIN ANALYZE"},
		{"explain (analyze true) select * from t", "EXPLAIN ANALYZE"},
		{"explain (analyze false) select * from t", "EXPLAIN ANALYZE"},
		{"explain (verbose, analyze) select 1", "EXPLAIN ANALYZE"},
		{"explain (format json, ANALYZE) select 1", "EXPLAIN ANALYZE"},
		{"explain verbose analyze select 1", "EXPLAIN ANALYZE"},
		{"explain (timing) select 1", "不在放行列表"},
		{"explain (wal) select 1", "不在放行列表"},
		{"explain (serialize) select 1", "不在放行列表"},
		{"explain (foo) select 1", "不在放行列表"},
		{"explain (verbose select 1", "没有闭合"},
		{"explain (verbose) ", "缺少语句"},
		{"explain", "缺少语句"},
		{"explain insert into t values (1)", "INSERT"},
		{"explain delete from t", "DELETE"},
		{"explain execute p", "只能解释"},
		{"explain create table x as select 1", "只能解释"},
		{"explain (verbose) show all", "只能解释"},
		{"explain explain select 1", "只能解释"},
		{"explain (verbose (x)) select 1", "嵌套括号"},
		{"(explain select 1)", "不能放在括号里"},
		{"show", "缺少参数名"},
		{"show (select 1)", "参数名"},
		{"show 'x'", "参数名"},
		{"show all; select 1", "2 条语句"},
		{"(show all)", "不能放在括号里"},
		// 危险函数
		{"select pg_sleep(10)", "pg_sleep"},
		{"select pg_catalog.pg_sleep(10)", "pg_sleep"},
		{"select \"pg_sleep\"(10)", "pg_sleep"},
		{"select pg_catalog.\"pg_sleep\"(10)", "pg_sleep"},
		{"select PG_SLEEP(10)", "PG_SLEEP"},
		{"select pg_sleep /* c */ (10)", "pg_sleep"},
		{"select pg_sleep\n(10)", "pg_sleep"},
		{"select * from pg_sleep(10)", "pg_sleep"},
		{"select pg_sleep_for('5 minutes')", "pg_sleep_for"},
		{"select pg_sleep_until('tomorrow')", "pg_sleep_until"},
		{"select pg_read_file('/etc/passwd')", "pg_read_file"},
		{"select pg_read_binary_file('/etc/passwd')", "pg_read_binary_file"},
		{"select pg_ls_dir('.')", "pg_ls_dir"},
		{"select * from pg_ls_logdir()", "pg_ls_logdir"},
		{"select * from pg_ls_waldir()", "pg_ls_waldir"},
		{"select pg_stat_file('x')", "pg_stat_file"},
		{"select lo_import('/etc/passwd')", "lo_import"},
		{"select lo_export(1, '/tmp/x')", "lo_export"},
		{"select lo_get(1)", "lo_get"},
		{"select lo_open(1, 131072)", "lo_open"},
		{"select loread(1, 10)", "loread"},
		{"select lowrite(1, 'x')", "lowrite"},
		{"select lo_from_bytea(0, 'x')", "lo_from_bytea"},
		{"select dblink('host=x', 'select 1')", "dblink"},
		{"select * from dblink('host=x', 'select 1') as t(a int)", "dblink"},
		{"select dblink_exec('host=x', 'drop table t')", "dblink_exec"},
		{"select dblink_connect('x')", "dblink_connect"},
		{"select pg_terminate_backend(123)", "pg_terminate_backend"},
		{"select pg_cancel_backend(123)", "pg_cancel_backend"},
		{"select pg_reload_conf()", "pg_reload_conf"},
		{"select set_config('statement_timeout', '0', false)", "set_config"},
		{"select pg_catalog.set_config('transaction_read_only', 'off', true)", "set_config"},
		{"select nextval('s')", "nextval"},
		{"select nextval ('s'::regclass)", "nextval"},
		{"select setval('s', 1)", "setval"},
		{"select setseed(0.5)", "setseed"},
		{"select pg_advisory_lock(1)", "pg_advisory_lock"},
		{"select pg_try_advisory_lock(1)", "pg_try_advisory_lock"},
		{"select pg_advisory_xact_lock(1)", "pg_advisory_xact_lock"},
		{"select pg_notify('c', 'p')", "pg_notify"},
		{"select query_to_xml('select 1', true, true, '')", "query_to_xml"},
		{"select query_to_xml_and_xmlschema('select 1', true, true, '')", "query_to_xml_and_xmlschema"},
		{"select table_to_xml('t', true, true, '')", "table_to_xml"},
		{"select cursor_to_xml('c', 1, true, true, '')", "cursor_to_xml"},
		{"select schema_to_xml('public', true, true, '')", "schema_to_xml"},
		{"select database_to_xml(true, true, '')", "database_to_xml"},
		{"select * from ts_stat('select tsv from t')", "ts_stat"},
		{"select pg_logical_slot_get_changes('s', null, null)", "pg_logical_slot_get_changes"},
		{"select pg_create_restore_point('x')", "pg_create_restore_point"},
		{"select pg_create_physical_replication_slot('x')", "pg_create_physical_replication_slot"},
		{"select pg_drop_replication_slot('x')", "pg_drop_replication_slot"},
		{"select pg_replication_slot_advance('x', '0/0')", "pg_replication_slot_advance"},
		{"select pg_switch_wal()", "pg_switch_wal"},
		{"select pg_rotate_logfile()", "pg_rotate_logfile"},
		{"select pg_promote()", "pg_promote"},
		{"select pg_backup_start('x')", "pg_backup_start"},
		{"select pg_start_backup('x')", "pg_start_backup"},
		{"select pg_stat_reset()", "pg_stat_reset"},
		{"select pg_stat_reset_shared('bgwriter')", "pg_stat_reset_shared"},
		{"select pg_wal_replay_pause()", "pg_wal_replay_pause"},
		{"select pg_import_system_collations('x')", "pg_import_system_collations"},
		{"select pg_log_backend_memory_contexts(1)", "pg_log_backend_memory_contexts"},
		{"select pg_export_snapshot()", "pg_export_snapshot"},
		{"select * from pg_get_wal_records_info('0/1', '0/2')", "pg_get_wal_records_info"},
		{"select pg_stat_get_activity(null)", "pg_stat_get_activity"},
		{"select pg_stat_get_backend_activity(1)", "pg_stat_get_backend_activity"},
		{"select pg_file_write('x', 'y', false)", "pg_file_write"},
		{"select pg_set_relation_stats('t'::regclass)", "pg_set_relation_stats"},
		{"select pg_restore_relation_stats()", "pg_restore_relation_stats"},
		{"select 1 where (select pg_sleep(1)) is null", "pg_sleep"},
		{"select * from t where id in (select nextval('s'))", "nextval"},
		{"with x as (select pg_sleep(1)) select * from x", "pg_sleep"},
		{"explain select pg_sleep(1)", "pg_sleep"},
		{"select ('x' || pg_read_file('f'))", "pg_read_file"},
		// 敏感系统对象
		{"select * from pg_authid", "pg_authid"},
		{"select * from pg_catalog.pg_authid", "pg_authid"},
		{"select * from \"pg_authid\"", "pg_authid"},
		{"select * from PG_AUTHID", "PG_AUTHID"},
		{"select rolpassword from pg_catalog.\"pg_authid\"", "pg_authid"},
		{"select * from pg_shadow", "pg_shadow"},
		{"select usename, passwd from pg_shadow", "pg_shadow"},
		{"select * from pg_largeobject", "pg_largeobject"},
		{"select * from pg_user_mappings", "pg_user_mappings"},
		{"select * from pg_user_mapping", "pg_user_mapping"},
		{"select * from pg_stat_activity", "pg_stat_activity"},
		{"select query from pg_catalog.pg_stat_activity", "pg_stat_activity"},
		{"select * from pg_stat_replication", "pg_stat_replication"},
		{"select * from pg_stat_wal_receiver", "pg_stat_wal_receiver"},
		{"select * from pg_stat_statements", "pg_stat_statements"},
		{"select * from pg_stat_statements_info", "pg_stat_statements_info"},
		{"select * from pg_statistic", "pg_statistic"},
		{"select * from pg_subscription", "pg_subscription"},
		{"select * from pg_foreign_server", "pg_foreign_server"},
		{"select * from pg_file_settings", "pg_file_settings"},
		{"select * from pg_hba_file_rules", "pg_hba_file_rules"},
		{"select * from pg_hba_file_rules()", "pg_hba_file_rules"},
		{"select * from pg_db_role_setting", "pg_db_role_setting"},
		{"select * from pg_auth_members", "pg_auth_members"},
		{"select * from pg_config", "pg_config"},
		{"table pg_authid", "pg_authid"},
		{"(table pg_authid)", "pg_authid"},
		{"select (select rolpassword from pg_authid limit 1)", "pg_authid"},
		{"with a as (select * from pg_authid) select * from a", "pg_authid"},
		{"explain select * from pg_authid", "pg_authid"},
		// 词法错误 / 不支持的写法
		{"select 'abc", "没有结束"},
		{"select \"abc", "没有结束"},
		{"select $$abc", "没有结束"},
		{"select $t$abc$u$", "没有结束"},
		{"select /* abc", "没有结束"},
		{"select /* a /* b */", "没有结束"},
		{"select e'abc\\'", "没有结束"},
		{"select \"\"", "零长度"},
		{"select $1", "参数占位符"},
		{"select * from t where a = $1", "参数占位符"},
		{"select $", "无法识别的 $"},
		{"select 1abc", "数字后面"},
		{"select 1_a", "数字后面"},
		{"select u&'\\0041'", "U&"},
		{"select U&\"pg\\0073leep\"(1)", "U&"},
		{"select * from u&\"pg_authid\"", "U&"},
		{"select \\x", "不允许的字符"},
		{"select {1}", "不允许的字符"},
		{"select 1\x00", "NUL"},
		{"select 'a\x00b'", "NUL"},
		{"select (1", "括号不匹配"},
		{"select 1)", "括号不匹配"},
		{"select [1", "括号不匹配"},
		{"select (1]", "括号不匹配"},
		{"with a as (select 1 select * from a", "括号不匹配"},
		{"with a (select 1) select 1", "缺少 AS"},
		{"with a as select 1", "缺少括号"},
		{"with as (select 1) select 1", "无法解析 WITH"},
		{"with a as (select 1)", "不完整"},
		{"with a as (select 1) show all", "WITH 的主语句"},
		{"select * from a.b.c.d", "不支持的对象名称"},
	}
	for _, c := range cases {
		_, err := CheckUserQuery(c.sql, CheckOptions{})
		if err == nil {
			t.Errorf("应拒绝 %q，却放行了", c.sql)
			continue
		}
		var ue *UserError
		if !errors.As(err, &ue) {
			t.Errorf("%q：错误类型应为 *UserError，得到 %T", c.sql, err)
			continue
		}
		if c.why != "" && !strings.Contains(err.Error(), c.why) {
			t.Errorf("%q：拒绝原因应含 %q，实际：%v", c.sql, c.why, err)
		}
	}
}

func TestCheckSizeLimit(t *testing.T) {
	big := "select '" + strings.Repeat("a", DefaultMaxStatement) + "'"
	if _, err := CheckUserQuery(big, CheckOptions{}); err == nil || !strings.Contains(err.Error(), "过长") {
		t.Fatalf("超长 SQL 应被拒绝：%v", err)
	}
	if _, err := CheckUserQuery(big, CheckOptions{MaxStatementBytes: 1 << 20}); err != nil {
		t.Fatalf("放宽上限后应放行：%v", err)
	}
}

func TestCheckCleanSQL(t *testing.T) {
	c, err := CheckUserQuery("  -- hi\n select /* a */ 1 -- tail\n ;", CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 注释换成空格，结尾的行注释不会吞掉后面补的 LIMIT
	if strings.Contains(c.SQL, "--") || strings.Contains(c.SQL, "/*") || strings.Contains(c.SQL, ";") {
		t.Fatalf("SQL 应去掉注释和分号：%q", c.SQL)
	}
	got := c.ExecSQL(10)
	if !strings.HasSuffix(got, " LIMIT 11") || strings.Contains(got, "--") {
		t.Fatalf("补 LIMIT 不对：%q", got)
	}
	// 字符串里的 -- 与 /* 不是注释
	c, err = CheckUserQuery("select '-- not a comment', '/* nor this */'", CheckOptions{})
	if err != nil || c.SQL != "select '-- not a comment', '/* nor this */'" {
		t.Fatalf("字符串里的注释符号被误处理：%v %q", err, c.SQL)
	}
}

func TestExecSQLLimit(t *testing.T) {
	cases := []struct {
		sql      string
		max      int
		want     string
		autoLimt bool
	}{
		{"select * from t", 100, "select * from t LIMIT 101", true},
		{"select * from t limit 5", 100, "select * from t limit 5", false},
		{"select * from t LIMIT ALL", 100, "select * from t LIMIT ALL", false},
		{"select * from t fetch first 5 rows only", 100, "select * from t fetch first 5 rows only", false},
		{"select * from t offset 5", 100, "select * from t offset 5 LIMIT 101", true},
		{"select * from t order by a", 100, "select * from t order by a LIMIT 101", true},
		{"select a from t union select a from u", 100, "select a from t union select a from u LIMIT 101", true},
		{"(select * from t limit 5)", 100, "(select * from t limit 5) LIMIT 101", true}, // 括号里的 limit 不算顶层
		{"select * from (select * from t limit 5) s", 100, "select * from (select * from t limit 5) s LIMIT 101", true},
		{"with c as (select * from t limit 5) select * from c", 100, "with c as (select * from t limit 5) select * from c LIMIT 101", true},
		{"values (1), (2)", 10, "values (1), (2) LIMIT 11", true},
		{"table t", 10, "table t LIMIT 11", true},
		{"select * from t", 0, "select * from t", false},
		{"explain select * from t", 100, "explain select * from t", false},
		{"show all", 100, "show all", false},
		{"select 'limit' from t", 100, "select 'limit' from t LIMIT 101", true},
		{"select \"limit\" from t", 100, "select \"limit\" from t LIMIT 101", true},
	}
	for _, c := range cases {
		ck, err := CheckUserQuery(c.sql, CheckOptions{})
		if err != nil {
			t.Errorf("%q：%v", c.sql, err)
			continue
		}
		if got := ck.ExecSQL(c.max); got != c.want {
			t.Errorf("ExecSQL(%q, %d) = %q，want %q", c.sql, c.max, got, c.want)
		}
		if ck.AutoLimited(c.max) != c.autoLimt {
			t.Errorf("AutoLimited(%q, %d) = %v", c.sql, c.max, !c.autoLimt)
		}
	}
}

func refs(pairs ...string) []sqlx.TableRef {
	var out []sqlx.TableRef
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, sqlx.TableRef{Schema: pairs[i], Name: pairs[i+1]})
	}
	return out
}

func TestCheckTables(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []sqlx.TableRef
	}{
		{"简单", "select * from t", refs("public", "t")},
		{"带 schema", "select * from s.t", refs("s", "t")},
		{"引号", `select * from "S"."A b"`, refs("S", "A b")},
		{"大小写折叠", "select * from S.TBL", refs("s", "tbl")},
		{"三段式", "select * from mydb.s.t", refs("s", "t")},
		{"逗号连接", "select * from a, b, c", refs("public", "a", "public", "b", "public", "c")},
		{"JOIN", "select * from a join s.b on a.id = b.id left join c using (id)", refs("public", "a", "s", "b", "public", "c")},
		{"ON 后接逗号", "select * from a join b on a.x = b.x, c", refs("public", "a", "public", "b", "public", "c")},
		{"子查询", "select * from (select * from a) s join b on true", refs("public", "a", "public", "b")},
		{"WHERE 里的子查询", "select * from a where id in (select id from b where exists (select 1 from c))", refs("public", "a", "public", "b", "public", "c")},
		{"标量子查询", "select (select max(x) from b) from a", refs("public", "b", "public", "a")},
		{"UNION", "select * from a union all select * from b", refs("public", "a", "public", "b")},
		{"LATERAL", "select * from a, lateral (select * from b where b.id = a.id) l", refs("public", "a", "public", "b")},
		{"ONLY", "select * from only a", refs("public", "a")},
		{"别名带列名", "select * from a as x(c1, c2) join b y on true", refs("public", "a", "public", "b")},
		{"TABLESAMPLE", "select * from a tablesample system (10), b", refs("public", "a", "public", "b")},
		{"TABLE 语句", "table a", refs("public", "a")},
		{"TABLE 在括号里", "(table s.a) union (table b)", refs("s", "a", "public", "b")},
		{"UNION TABLE", "select * from a union table b", refs("public", "a", "public", "b")},
		{"VALUES 不涉及表", "values (1), (2)", nil},
		{"FROM VALUES", "select * from (values (1)) v(a)", nil},
		{"EXTRACT 的 FROM", "select extract(year from ts) from a", refs("public", "a")},
		{"SUBSTRING 的 FROM", "select substring(x from 1 for 2) from a", refs("public", "a")},
		{"TRIM 的 FROM", "select trim(leading 'x' from c) from a", refs("public", "a")},
		{"IS DISTINCT FROM", "select a.x is distinct from a.y, a.x is not distinct from 1 from a", refs("public", "a")},
		{"SELECT DISTINCT FROM 是空列表", "select distinct from secret", refs("public", "secret")},
		{"SELECT FROM 空列表", "select from secret", refs("public", "secret")},
		{"CTE 排除", "with c as (select * from a) select * from c", refs("public", "a")},
		{"CTE 多个", "with c as (select 1), d as (select * from c) select * from d join e on true", refs("public", "e")},
		{"CTE 带列名", "with c(x) as (select 1) select * from c", nil},
		{"CTE 带 MATERIALIZED", "with c as materialized (select * from a) select * from c", refs("public", "a")},
		{"CTE 名为引号", `with "C" as (select 1) select * from "C"`, nil},
		{"CTE 引号名不匹配无引号名", `with "C" as (select 1) select * from c`, refs("public", "c")},
		{"CTE 不遮蔽带 schema 的名字", "with c as (select 1) select * from s.c", refs("s", "c")},
		{"递归 CTE 自引用", "with recursive r(n) as (select 1 union all select n+1 from r) select * from r", nil},
		{"非递归 CTE 主体里同名是真表", "with t as (select * from t) select * from t", refs("public", "t")},
		{"CTE 作用域：子查询里的 WITH 不遮蔽外面", "select * from (with w as (select 1) select * from w) x, w", refs("public", "w")},
		{"CTE 作用域：括号内定义的在括号外不可见", "select * from (select * from (with w as (select 1) select * from w) a) b join w on true", refs("public", "w")},
		{"CTE 作用域：后面的 CTE 主体可见前面的", "with a as (select 1), b as (select * from a) select * from b", nil},
		{"CTE 作用域：前面的看不到后面的", "with a as (select * from b), b as (select 1) select * from a", refs("public", "b")},
		{"WINDOW 子句的名字不是 CTE", "select * from w2 window w1 as (), w2 as ()", refs("public", "w2")},
		{"WINDOW 后的 FROM 列表终止", "select sum(x) over w from a window w as (partition by y) ", refs("public", "a")},
		{"函数 FROM 不是表", "select * from generate_series(1, 3) g, a", refs("public", "a")},
		{"系统目录也列出", "select * from pg_catalog.pg_class c join information_schema.tables t on true", refs("pg_catalog", "pg_class", "information_schema", "tables")},
		{"ORDER BY 逗号不是表", "select * from a order by x, y", refs("public", "a")},
		{"GROUP BY 逗号不是表", "select x, y from a group by x, y", refs("public", "a")},
		{"数组下标里的逗号", "select * from a join b on a.x = (array[1,2,3])[1], c", refs("public", "a", "public", "b", "public", "c")},
		{"EXPLAIN 里的表", "explain (verbose) select * from a join s.b on true", refs("public", "a", "s", "b")},
		{"嵌套连接", "select * from (a join b on true) join c on true", refs("public", "a", "public", "b", "public", "c")},
		{"ROWS FROM", "select * from rows from (generate_series(1,2), unnest(array[1])) r, a", refs("public", "a")},
		{"重复去重", "select * from a join a b on true", refs("public", "a")},
		{"字符串里的 FROM 不算", "select 'from secret' from a", refs("public", "a")},
		{"注释里的 FROM 不算", "select 1 from a /* from secret */ -- from secret2", refs("public", "a")},
		{"表名是 rows", "select * from rows", refs("public", "rows")},
		{"FETCH FIRST 终止 FROM", "select * from a fetch first 1 row only", refs("public", "a")},
		{"FOR 终止 FROM（含 SHARE 被拒绝，这里用 OF 之外的写法）", "select * from a where x = 1 limit 1", refs("public", "a")},
		{"同名 CTE 在递归 WITH 下", "with recursive a as (select 1), b as (select * from a) select * from b, c", refs("public", "c")},
	}
	for _, c := range cases {
		got, err := CheckUserQuery(c.sql, CheckOptions{DBName: "mydb"})
		if err != nil {
			t.Errorf("%s：%q 被拒绝：%v", c.name, c.sql, err)
			continue
		}
		if !reflect.DeepEqual(got.Tables, c.want) {
			t.Errorf("%s：%q\n got  %v\n want %v", c.name, c.sql, got.Tables, c.want)
		}
	}
}

func TestCheckTablesOptions(t *testing.T) {
	c, err := CheckUserQuery("select * from a join s.b on true", CheckOptions{DefaultSchema: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if want := refs("app", "a", "s", "b"); !reflect.DeepEqual(c.Tables, want) || c.DefaultSchema != "app" || !c.PinSearchPath {
		t.Fatalf("%v %+v", c.Tables, c)
	}
	c, _ = CheckUserQuery("select 1", CheckOptions{KeepServerSearchPath: true})
	if c.PinSearchPath || c.DefaultSchema != "public" {
		t.Fatalf("%+v", c)
	}
	// 跨库引用
	if _, err := CheckUserQuery("select * from other.s.t", CheckOptions{DBName: "mydb"}); err == nil || !strings.Contains(err.Error(), "跨库") {
		t.Fatalf("三段式名称的库名与当前库不同应拒绝：%v", err)
	}
	if _, err := CheckUserQuery("select * from mydb.s.t", CheckOptions{DBName: "mydb"}); err != nil {
		t.Fatalf("库名相同应放行：%v", err)
	}
}

func TestCheckFunctionsInFrom(t *testing.T) {
	cases := []struct {
		sql      string
		funcs    []string
		needAll  bool
		wantTabs []sqlx.TableRef
	}{
		{"select * from generate_series(1, 3)", []string{"generate_series"}, false, nil},
		{"select * from pg_catalog.generate_series(1, 3)", []string{"pg_catalog.generate_series"}, false, nil},
		{"select * from unnest(array[1]), jsonb_each('{}'::jsonb), a", []string{"unnest", "jsonb_each"}, false, refs("public", "a")},
		{"select * from my_func(1)", []string{"my_func"}, true, nil},
		{"select * from public.my_func(1) f, a", []string{"public.my_func"}, true, refs("public", "a")},
		{"select * from generate_series(1,2), evil.generate_series(1,2)", []string{"generate_series", "evil.generate_series"}, true, nil},
		{"select * from a, lateral get_rows(a.id) g", []string{"get_rows"}, true, refs("public", "a")},
		{"select now()", nil, false, nil},
		{"select my_func(1) from a", nil, false, refs("public", "a")}, // 表达式位置的函数无法静态判断，靠只读事务与账号权限兜底
	}
	for _, c := range cases {
		got, err := CheckUserQuery(c.sql, CheckOptions{})
		if err != nil {
			t.Errorf("%q：%v", c.sql, err)
			continue
		}
		if !reflect.DeepEqual(got.Functions, c.funcs) || got.NeedAllPriv != c.needAll || !reflect.DeepEqual(got.Tables, c.wantTabs) {
			t.Errorf("%q：funcs=%v needAll=%v tables=%v，want %v %v %v", c.sql, got.Functions, got.NeedAllPriv, got.Tables, c.funcs, c.needAll, c.wantTabs)
		}
	}
}

func TestCheckKinds(t *testing.T) {
	cases := map[string]string{
		"select 1": KindSelect, "with a as (select 1) select * from a": KindSelect, "values (1)": KindSelect, "table t": KindSelect,
		"(select 1)": KindSelect, "explain select 1": KindExplain, "show all": KindShow,
	}
	for sql, want := range cases {
		c, err := CheckUserQuery(sql, CheckOptions{})
		if err != nil || c.Kind != want {
			t.Errorf("%q：kind=%v err=%v，want %s", sql, c, err, want)
		}
	}
}

func TestIsSystemSchema(t *testing.T) {
	for s, want := range map[string]bool{"pg_catalog": true, "information_schema": true, "pg_toast": true, "pg_temp_3": true, "PG_CATALOG": true, "public": false, "app": false} {
		if IsSystemSchema(s) != want {
			t.Errorf("IsSystemSchema(%q) != %v", s, want)
		}
	}
}

// 回归：每个黑名单函数在“带引号 / 带 schema / 大小写 / 空白注释”各种写法下都被拒绝。
func TestDenyFunctionsObfuscation(t *testing.T) {
	names := []string{"pg_sleep", "pg_read_file", "lo_import", "dblink", "set_config", "nextval", "pg_terminate_backend", "query_to_xml"}
	forms := []string{"%s(1)", "pg_catalog.%s(1)", `"%s"(1)`, `pg_catalog."%s"(1)`, "%s /**/ (1)", "%s\n\t(1)", "%s -- c\n(1)", "  %s  (1)"}
	for _, n := range names {
		for _, f := range forms {
			for _, wrap := range []string{"select %s", "select * from %s", "select 1 where %s is null", "with x as (select %s) select * from x", "explain select %s"} {
				sql := strings.Replace(wrap, "%s", strings.Replace(f, "%s", n, 1), 1)
				if _, err := CheckUserQuery(sql, CheckOptions{}); err == nil {
					t.Errorf("应拒绝 %q", sql)
				}
				up := strings.ToUpper(sql)
				if strings.Contains(sql, `"`) { // 带引号时大小写是不同的标识符，但仍然拒绝（宁可错杀）
					continue
				}
				if _, err := CheckUserQuery(up, CheckOptions{}); err == nil {
					t.Errorf("应拒绝 %q", up)
				}
			}
		}
	}
}
