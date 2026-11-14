// @author: kevinkelly
// @license: Apache Licence
// @file: check_test.go
// @time: 2026/10/28

package clickhouse

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"dbtool/internal/sqlx"
)

func TestCheckAllowed(t *testing.T) {
	ok := []string{
		"SELECT 1",
		"select 1;",
		"SELECT 1 ;  ",
		"  -- 注释\nSELECT 1 /* c */ ",
		"SELECT * FROM t",
		"SELECT a, b FROM db1.t WHERE a > 1 ORDER BY b LIMIT 10",
		"SELECT count() FROM t GROUP BY a HAVING count() > 1",
		"SELECT a FROM t1 JOIN t2 ON t1.id = t2.id",
		"SELECT a FROM t1 LEFT JOIN db2.t2 USING (id)",
		"SELECT a FROM t1 GLOBAL ANY LEFT JOIN t2 USING id",
		"SELECT * FROM t1 ARRAY JOIN arr AS x",
		"SELECT * FROM t1 LEFT ARRAY JOIN arr1, arr2",
		"SELECT * FROM t FINAL",
		"SELECT * FROM t SAMPLE 0.1",
		"SELECT * FROM t PREWHERE a = 1 WHERE b = 2",
		"SELECT x FROM (SELECT a AS x FROM t) AS s",
		"SELECT a FROM t WHERE a IN (SELECT a FROM u)",
		"SELECT a FROM t WHERE a IN (1, 2, 3)",
		"SELECT a FROM t WHERE a IN [1, 2]",
		"SELECT a FROM t WHERE a GLOBAL NOT IN (SELECT a FROM u)",
		"SELECT * FROM t WHERE EXISTS (SELECT 1 FROM u WHERE u.id = t.id)",
		"SELECT a FROM t UNION ALL SELECT a FROM u",
		"SELECT a FROM t UNION DISTINCT SELECT a FROM u INTERSECT SELECT a FROM v EXCEPT SELECT a FROM w",
		"WITH x AS (SELECT 1 AS a) SELECT a FROM x",
		"WITH x AS (SELECT a FROM t), y AS (SELECT * FROM x) SELECT * FROM y",
		"WITH 1 AS one SELECT one",
		"WITH (SELECT max(a) FROM t) AS m SELECT m",
		"SELECT extract(year FROM d) FROM t",
		"SELECT trim(BOTH ' ' FROM s) FROM t",
		"SELECT substring(s FROM 1 FOR 2) FROM t",
		"SELECT a, count() FROM t GROUP BY a WITH TOTALS",
		"SELECT a FROM t GROUP BY a WITH ROLLUP",
		"SELECT a FROM t ORDER BY a WITH FILL FROM 1 TO 10",
		"SELECT a FROM t LIMIT 5 OFFSET 10",
		"SELECT a FROM t LIMIT 10, 20",
		"SELECT a FROM t ORDER BY a LIMIT 2 BY b",
		"SELECT a FROM t LIMIT 5 WITH TIES",
		"SELECT * FROM numbers(10)",
		"SELECT * FROM numbers(5, 10)",
		"SELECT number FROM numbers(100000000)",
		"SELECT * FROM values('a UInt8, b String', (1, 'x'), (2, 'y'))",
		"SELECT * FROM `my db`.`my table`",
		`SELECT * FROM "db"."t"`,
		"SELECT 'it''s', 'a\\'b', 'FROM system.users'",
		"SELECT 'x -- not a comment', 1",
		"SELECT '/* not a comment */', 1",
		"SELECT $$ ; DROP TABLE x $$",
		"SELECT $tag$ ' ; $tag$ AS s",
		"SELECT format('{}-{}', a, b) FROM t",
		"SELECT sum(a) OVER (PARTITION BY b ORDER BY c) FROM t",
		"SELECT * FROM t WHERE s LIKE '%FORMAT%'",
		"SELECT `settings`, `format` FROM t",
	}
	for _, q := range ok {
		if ck, err := CheckUserQuery(q); err != nil {
			t.Errorf("应放行 %q: %v", q, err)
		} else if ck.Stmt == "" {
			t.Errorf("%q: Stmt 为空", q)
		}
	}
	more := []string{
		"SHOW DATABASES",
		"SHOW DATABASES LIKE 'a%'",
		"SHOW TABLES",
		"SHOW TABLES FROM db1",
		"SHOW TABLES IN db1 LIKE '%x%' LIMIT 10",
		"SHOW TEMPORARY TABLES",
		"SHOW DICTIONARIES FROM db1",
		"SHOW CREATE TABLE db1.t",
		"SHOW CREATE TABLE t",
		"SHOW CREATE VIEW v",
		"SHOW CREATE DICTIONARY d",
		"SHOW CREATE DATABASE db1",
		"SHOW COLUMNS FROM t",
		"SHOW FULL COLUMNS FROM t FROM db1",
		"SHOW COLUMNS IN db1.t LIKE 'a%'",
		"SHOW INDEXES FROM t",
		"SHOW KEYS FROM t IN db1",
		"DESCRIBE t",
		"DESCRIBE TABLE db1.t",
		"DESC db1.t",
		"EXISTS t",
		"EXISTS TABLE db1.t",
		"EXISTS DATABASE db1",
		"EXISTS DICTIONARY d",
		"EXPLAIN SELECT 1",
		"EXPLAIN AST SELECT a FROM t",
		"EXPLAIN SYNTAX SELECT a FROM t",
		"EXPLAIN QUERY TREE SELECT a FROM t",
		"EXPLAIN PLAN SELECT a FROM t",
		"EXPLAIN PLAN actions = 1, header = 1 SELECT a FROM t",
		"EXPLAIN PIPELINE SELECT a FROM t",
		"EXPLAIN PIPELINE graph=1 SELECT a FROM t",
		"EXPLAIN ESTIMATE SELECT a FROM t",
		"EXPLAIN WITH x AS (SELECT 1) SELECT * FROM x",
		"SELECT * FROM system.databases",
		"SELECT * FROM system.tables WHERE database = 'a'",
		"SELECT * FROM system.columns",
		"SELECT * FROM system.parts",
		"SELECT * FROM system.one",
		"SELECT * FROM `system`.`tables`",
		"SELECT * FROM information_schema.tables",
		"SELECT * FROM INFORMATION_SCHEMA.COLUMNS",
		"SELECT dictGet('db1.dict', 'attr', toUInt64(1))",
		"SELECT dictGetOrDefault('dict', 'attr', 1, 'x')",
		"SELECT joinGet('db1.j', 'v', 1)",
	}
	for _, q := range more {
		if _, err := CheckUserQuery(q); err != nil {
			t.Errorf("应放行 %q: %v", q, err)
		}
	}
}

func TestCheckDenied(t *testing.T) {
	cases := []struct{ q, want string }{
		// 空 / 多语句
		{"", "为空"}, {"   ", "为空"}, {"-- only comment", "只有注释"}, {";", "为空"},
		{"SELECT 1; SELECT 2", "多条语句"}, {"SELECT 1;;", "多条语句"}, {"SELECT 1; DROP TABLE t", "多条语句"},
		{"SELECT 1 ; -- x\n SELECT 2", "多条语句"},
		// 写 / DDL / 管理
		{"INSERT INTO t VALUES (1)", "INSERT"}, {"insert into t select 1", "INSERT"}, {"ALTER TABLE t DELETE WHERE 1", "ALTER"},
		{"CREATE TABLE t (a Int8) ENGINE = Memory", "CREATE"}, {"DROP TABLE t", "DROP"}, {"TRUNCATE TABLE t", "TRUNCATE"},
		{"RENAME TABLE a TO b", "RENAME"}, {"OPTIMIZE TABLE t FINAL", "OPTIMIZE"}, {"KILL QUERY WHERE 1", "KILL"},
		{"SYSTEM RELOAD CONFIG", "SYSTEM"}, {"ATTACH TABLE t", "ATTACH"}, {"DETACH TABLE t", "DETACH"},
		{"GRANT SELECT ON *.* TO u", "GRANT"}, {"REVOKE SELECT ON *.* FROM u", "REVOKE"}, {"SET max_threads = 1", "SET"},
		{"USE db", "USE"}, {"CHECK TABLE t", "CHECK"}, {"BACKUP TABLE t TO Disk('b', 'x')", "BACKUP"}, {"RESTORE TABLE t FROM Disk('b','x')", "RESTORE"},
		{"EXCHANGE TABLES a AND b", "EXCHANGE"}, {"UNDROP TABLE t", "UNDROP"}, {"WATCH lv", "WATCH"}, {"DELETE FROM t WHERE 1", "DELETE"},
		{"UPDATE t SET a = 1", "UPDATE"}, {"REPLACE INTO t VALUES (1)", "REPLACE"},
		{"CALL p()", "CALL"}, {"BEGIN", "BEGIN"},
		{"FOO BAR", "白名单"}, {"(SELECT 1)", "括号开头"}, {"1 + 1", "必须以"}, {"EXECUTE x", "白名单"},
		// SETTINGS / FORMAT / INTO
		{"SELECT 1 SETTINGS max_threads = 1", "SETTINGS"}, {"SELECT 1 settings readonly=0", "SETTINGS"},
		{"SELECT * FROM (SELECT 1 SETTINGS max_result_rows = 1)", "SETTINGS"},
		{"SELECT 1 FORMAT JSON SETTINGS x=1", "FORMAT"}, {"SELECT settings FROM t", "SETTINGS"},
		{"SELECT 1 FORMAT JSON FORMAT CSV", "FORMAT"}, {"SELECT format FROM t", "FORMAT"}, {"SELECT * FROM (SELECT 1 FORMAT JSON)", "FORMAT"},
		{"SELECT 1 INTO OUTFILE '/tmp/x'", "INTO"}, {"SELECT 1 INTO OUTFILE '/tmp/x' FORMAT CSV", "INTO"},
		{"EXPLAIN SELECT 1 SETTINGS x = 1", "SETTINGS"},
		// 危险函数
		{"SELECT sleep(3)", "sleep"}, {"SELECT SLEEP(3)", "sleep"}, {"SELECT sleepEachRow(1) FROM numbers(3)", "sleepeachrow"},
		{"SELECT `sleep`(3)", "sleep"}, {"SELECT \"sleep\"(3)", "sleep"}, {"SELECT sleep /* x */ (3)", "sleep"},
		{"SELECT * APPLY(sleep) FROM t", "sleep"},
		{"SELECT hasColumnInTable('h', 'db', 't', 'c', 'u', 'p')", "hascolumnintable"},
		{"SELECT catboostEvaluate('/etc/model.bin', 1)", "catboostevaluate"},
		{"SELECT trap('x')", "trap"},
		// 表函数（表位置与 IN 位置）
		{"SELECT * FROM file('/etc/passwd', 'CSV', 'a String')", "file"},
		{"SELECT * FROM url('http://x/y', 'CSV', 'a String')", "url"},
		{"SELECT * FROM s3('http://x/y', 'CSV')", "s3"}, {"SELECT * FROM s3Cluster('c', 'http://x/y')", "s3cluster"},
		{"SELECT * FROM hdfs('hdfs://x/y', 'CSV')", "hdfs"},
		{"SELECT * FROM remote('10.0.0.1', system.users)", "remote"}, {"SELECT * FROM remoteSecure('10.0.0.1', db.t)", "remotesecure"},
		{"SELECT * FROM cluster('c', db.t)", "cluster"}, {"SELECT * FROM clusterAllReplicas('c', system.query_log)", "clusterallreplicas"},
		{"SELECT * FROM mysql('h:3306', 'db', 't', 'u', 'p')", "mysql"}, {"SELECT * FROM postgresql('h:5432', 'db', 't', 'u', 'p')", "postgresql"},
		{"SELECT * FROM mongodb('h', 'db', 't', 'u', 'p', 'a Int8')", "mongodb"}, {"SELECT * FROM jdbc('x', 'y')", "jdbc"}, {"SELECT * FROM odbc('x', 'y')", "odbc"},
		{"SELECT * FROM sqlite('/tmp/a.db', 't')", "sqlite"}, {"SELECT * FROM redis('h', 'k', 'a String')", "redis"},
		{"SELECT * FROM deltaLake('http://x')", "deltalake"}, {"SELECT * FROM iceberg('http://x')", "iceberg"}, {"SELECT * FROM hudi('http://x')", "hudi"},
		{"SELECT * FROM azureBlobStorage('x')", "azureblobstorage"}, {"SELECT * FROM gcs('x')", "gcs"},
		{"SELECT * FROM input('a Int8')", "input"}, {"SELECT * FROM executable('x.sh', 'TSV', 'a String')", "executable"},
		{"SELECT * FROM generateRandom('a Int8')", "generaterandom"}, {"SELECT * FROM numbers_mt(10)", "numbers_mt"},
		{"SELECT * FROM zeros(10)", "zeros"}, {"SELECT * FROM loop(numbers(3))", "loop"},
		{"SELECT * FROM merge('db', '^t')", "merge"},
		{"SELECT * FROM view(SELECT 1)", "view"},
		{"SELECT * FROM dictionary('d')", "dictionary"},
		{"SELECT * FROM someunknownfn(1)", "默认拒绝"},
		{"SELECT * FROM t1, file('x', 'CSV', 'a String')", "file"},
		{"SELECT * FROM t1 JOIN url('http://x', 'CSV', 'a String') AS u ON 1", "url"},
		{"SELECT * FROM (SELECT * FROM file('x', 'CSV', 'a String'))", "file"},
		{"WITH x AS (SELECT * FROM s3('u', 'CSV')) SELECT * FROM x", "s3"},
		{"SELECT a FROM t WHERE a IN file('x', 'CSV', 'a String')", "file"},
		{"SELECT a FROM t WHERE a IN (SELECT a FROM remote('h', db.t))", "remote"},
		{"SELECT * FROM `file`('x', 'CSV', 'a String')", "file"},
		{"SELECT * FROM FILE('x', 'CSV', 'a String')", "file"},
		{"SELECT * FROM numbers(1000000000)", "不能超过"}, {"SELECT * FROM numbers(1e9)", "不能超过"}, {"SELECT * FROM numbers(x)", "整数字面量"},
		{"SELECT * FROM numbers()", "1 到 3"}, {"SELECT * FROM numbers(1,2,3,4)", "1 到 3"}, {"SELECT * FROM numbers(0x10)", "不能超过"},
		{"SELECT * FROM {tbl:Identifier}", "参数化"},
		{"SELECT * FROM db.t.c", "两段"},
		{"SELECT * FROM db.fn(1)", "调用"},
		// system 库
		{"SELECT * FROM system.users", "system.users"}, {"SELECT * FROM system.roles", "system.roles"}, {"SELECT * FROM system.grants", "system.grants"},
		{"SELECT * FROM system.settings_profiles", "settings_profiles"}, {"SELECT * FROM system.quotas", "quotas"}, {"SELECT * FROM system.row_policies", "row_policies"},
		{"SELECT * FROM system.query_log", "query_log"}, {"SELECT * FROM system.processes", "processes"}, {"SELECT * FROM system.clusters", "clusters"},
		{"SELECT * FROM system.named_collections", "named_collections"}, {"SELECT * FROM system.zookeeper", "zookeeper"},
		{"SELECT * FROM system.numbers", "system.numbers"}, {"SELECT * FROM system.dictionaries", "dictionaries"}, {"SELECT * FROM system.disks", "disks"},
		{"SELECT * FROM system.mutations", "mutations"}, {"SELECT * FROM system.replicas", "replicas"}, {"SELECT * FROM system.metrics", "metrics"},
		{"SELECT * FROM SYSTEM.USERS", "USERS"},
		{"SELECT * FROM `system`.`users`", "users"}, {"SELECT * FROM \"system\".\"users\"", "users"}, {"SELECT * FROM system . /* c */ users", "users"},
		{"SELECT * FROM system.databases d JOIN system.users u ON 1", "users"},
		{"SELECT * FROM (SELECT * FROM system.users)", "users"},
		{"SELECT a FROM t WHERE a IN (SELECT name FROM system.users)", "users"},
		{"SELECT a FROM t WHERE a IN system.users", "users"},
		{"WITH x AS (SELECT * FROM system.users) SELECT * FROM x", "users"},
		{"SELECT * FROM t UNION ALL SELECT * FROM system.users", "users"},
		{"SELECT dictGet('system.users', 'a', 1)", "users"},
		{"SHOW CREATE TABLE system.users", "users"},
		{"DESCRIBE system.users", "users"},
		{"EXPLAIN SELECT * FROM system.users", "users"},
		{"EXISTS system.users", "users"},
		// dictGet / joinGet 参数
		{"SELECT dictGet(name, 'a', 1) FROM t", "字符串常量"}, {"SELECT dictGet(concat('d', 'x'), 'a', 1)", "字符串常量"},
		{"SELECT dictGet('d' || 'x', 'a', 1)", "单独的字符串常量"}, {"SELECT dictGet('d\\x2edict', 'a', 1)", "转义"},
		{"SELECT joinGet(j, 'a', 1)", "字符串常量"},
		// 标识符转义 / 词法歧义
		{"SELECT * FROM `sys\\x74em`.`users`", "转义"}, {"SELECT * FROM `a\\nb`", "转义"},
		{"SELECT 1 # comment", "#"}, {"SELECT 1 #! x", "#"},
		{"SELECT 'abc", "未结束"}, {"SELECT `abc", "未结束"}, {"SELECT 1 /* x", "未结束"}, {"SELECT $$ x", "未结束"}, {"SELECT $t$ x $u$", "未结束"},
		{"SELECT 1\x00", "控制字符"}, {"SELECT\x011", "控制字符"}, {"SELECT 中文 FROM t", "非 ASCII"}, {"SELECT 1 FROM t", "非 ASCII"},
		{"SELECT \xff", "UTF-8"},
		{"SELECT (1", "括号"}, {"SELECT 1)", "括号"},
		// SHOW / DESCRIBE / EXISTS / EXPLAIN 的非白名单形式
		{"SHOW PROCESSLIST", "PROCESSLIST"}, {"SHOW GRANTS", "GRANTS"}, {"SHOW USERS", "USERS"}, {"SHOW CLUSTERS", "CLUSTERS"}, {"SHOW SETTINGS", "SETTINGS"},
		{"SHOW CREATE USER u", "SHOW CREATE"}, {"SHOW CREATE ROLE r", "SHOW CREATE"}, {"SHOW CREATE QUOTA q", "SHOW CREATE"}, {"SHOW", "缺少"},
		{"SHOW CREATE TABLE", "参数"}, {"SHOW CREATE TABLE a.b.c", "参数"}, {"SHOW CREATE TABLE a, b", "参数"},
		{"SHOW TABLES WHERE name = (SELECT 1)", "不支持的写法"}, {"SHOW TABLES FROM (SELECT 1)", "缺少名称"}, {"SHOW TABLES LIKE name", "字符串"},
		{"SHOW TABLES LIMIT x", "数字"}, {"SHOW COLUMNS", "FROM"}, {"SHOW TABLES FROM a.b", "库名"},
		{"SHOW COLUMNS FROM t FROM a FROM b", "FROM"},
		{"DESCRIBE (SELECT 1)", "DESCRIBE"}, {"DESCRIBE url('http://x', 'CSV')", "url"}, {"DESCRIBE TABLE", "DESCRIBE"}, {"DESCRIBE t x", "DESCRIBE"},
		{"EXISTS", "EXISTS"}, {"EXISTS (SELECT 1)", "EXISTS"}, {"EXISTS DATABASE a.b", "库名"},
		{"EXPLAIN INSERT INTO t VALUES (1)", "INTO"}, {"EXPLAIN INSERT t VALUES (1)", "EXPLAIN"}, {"EXPLAIN TABLE OVERRIDE x", "TABLE"}, {"EXPLAIN QUERY SELECT 1", "TREE"},
		{"EXPLAIN", "SELECT"}, {"EXPLAIN AST", "SELECT"}, {"EXPLAIN AST CREATE TABLE t (a Int8) ENGINE=Memory", "SELECT"},
		{"EXPLAIN PLAN (SELECT 1)", "SELECT"}, {"EXPLAIN SYNTAX DROP TABLE t", "SELECT"},
	}
	for _, c := range cases {
		_, err := CheckUserQuery(c.q)
		if err == nil {
			t.Errorf("应拒绝 %q", c.q)
			continue
		}
		var ue *UserError
		if !errors.As(err, &ue) {
			t.Errorf("%q: 错误类型 %T", c.q, err)
			continue
		}
		if !strings.Contains(strings.ToLower(ue.Msg), strings.ToLower(c.want)) {
			t.Errorf("%q: 错误 %q 应包含 %q", c.q, ue.Msg, c.want)
		}
	}
}

func TestCheckTooLong(t *testing.T) {
	q := "SELECT '" + strings.Repeat("a", MaxQueryBytes) + "'"
	if _, err := CheckUserQuery(q); err == nil || !strings.Contains(err.Error(), "过长") {
		t.Errorf("err = %v", err)
	}
}

func TestCheckFormatStripped(t *testing.T) {
	cases := []struct{ q, stmt string }{
		{"SELECT 1 FORMAT JSON", "SELECT 1"},
		{"SELECT 1 format TabSeparated;", "SELECT 1"},
		{"SELECT a FROM t FORMAT Null", "SELECT a FROM t"},
		{"  -- c\n SELECT a /* x */ FROM t  -- tail\n", "SELECT a /* x */ FROM t"},
		{"SELECT 'x FORMAT y' ", "SELECT 'x FORMAT y'"},
		{"DESCRIBE t FORMAT JSON", "DESCRIBE t"},
	}
	for _, c := range cases {
		ck, err := CheckUserQuery(c.q)
		if err != nil {
			t.Errorf("%q: %v", c.q, err)
			continue
		}
		if ck.Stmt != c.stmt {
			t.Errorf("%q: Stmt = %q, want %q", c.q, ck.Stmt, c.stmt)
		}
		if strings.Contains(strings.ToUpper(c.q), " FORMAT J") || strings.Contains(c.q, "format Tab") || strings.Contains(c.q, "FORMAT Null") {
			if len(ck.Notices) == 0 {
				t.Errorf("%q: 应有 FORMAT 被忽略的提示", c.q)
			}
		}
	}
	// 不会把注释里的换行吃掉导致后面内容并入注释
	ck, err := CheckUserQuery("SELECT 1 -- a\n + 2")
	if err != nil || ck.Stmt != "SELECT 1 -- a\n + 2" {
		t.Errorf("ck=%+v err=%v", ck, err)
	}
}

func TestCheckKinds(t *testing.T) {
	cases := map[string]string{
		"SELECT 1": "select", "WITH 1 AS a SELECT a": "select", "SHOW TABLES": "show", "DESC t": "describe",
		"EXISTS t": "exists", "EXPLAIN SELECT 1": "explain",
	}
	for q, want := range cases {
		ck, err := CheckUserQuery(q)
		if err != nil || ck.Kind != want {
			t.Errorf("%q: kind=%v err=%v want %s", q, ck, err, want)
		}
	}
}

func TestCheckTables(t *testing.T) {
	ref := func(db, name string) sqlx.TableRef { return sqlx.TableRef{Schema: db, Name: name} }
	cases := []struct {
		q    string
		db   string
		want []sqlx.TableRef
		dbs  []string
	}{
		{"SELECT 1", "", nil, nil},
		{"SELECT * FROM t", "", []sqlx.TableRef{ref("default", "t")}, []string{"default"}},
		{"SELECT * FROM t", "app", []sqlx.TableRef{ref("app", "t")}, []string{"app"}},
		{"SELECT * FROM a.t, b.u", "app", []sqlx.TableRef{ref("a", "t"), ref("b", "u")}, []string{"a", "b"}},
		{"SELECT * FROM t JOIN u ON t.id = u.id JOIN db2.v USING (id)", "app", []sqlx.TableRef{ref("app", "t"), ref("app", "u"), ref("db2", "v")}, []string{"app", "db2"}},
		{"SELECT * FROM t t1 JOIN t t2 ON 1", "app", []sqlx.TableRef{ref("app", "t")}, []string{"app"}},
		{"SELECT * FROM `a b`.`c d`", "", []sqlx.TableRef{ref("a b", "c d")}, []string{"a b"}},
		{`SELECT * FROM "a"."b"`, "", []sqlx.TableRef{ref("a", "b")}, []string{"a"}},
		{"SELECT * FROM (SELECT x FROM inner_t) s", "app", []sqlx.TableRef{ref("app", "inner_t")}, []string{"app"}},
		{"SELECT * FROM t WHERE a IN (SELECT a FROM u)", "app", []sqlx.TableRef{ref("app", "t"), ref("app", "u")}, []string{"app"}},
		{"SELECT * FROM t WHERE a IN u", "app", []sqlx.TableRef{ref("app", "t"), ref("app", "u")}, []string{"app"}},
		{"SELECT * FROM t WHERE a GLOBAL NOT IN db2.u", "app", []sqlx.TableRef{ref("app", "t"), ref("db2", "u")}, []string{"app", "db2"}},
		{"SELECT * FROM t WHERE a IN (1,2) AND b IN range(3)", "app", []sqlx.TableRef{ref("app", "t")}, []string{"app"}},
		// CTE 别名被排除
		{"WITH x AS (SELECT a FROM t) SELECT * FROM x", "app", []sqlx.TableRef{ref("app", "t")}, []string{"app"}},
		{"WITH x AS (SELECT a FROM t), y AS (SELECT * FROM x JOIN u ON 1) SELECT * FROM y", "app", []sqlx.TableRef{ref("app", "t"), ref("app", "u")}, []string{"app"}},
		// 同名：CTE 体里引用的 x 是真实表，CTE 定义之后的 x 才是别名
		{"WITH x AS (SELECT * FROM x) SELECT * FROM x", "app", []sqlx.TableRef{ref("app", "x")}, []string{"app"}},
		// 集合运算之后，CTE 不再可见（按真实表报，宁可多报）
		{"WITH x AS (SELECT 1) SELECT * FROM x UNION ALL SELECT * FROM x", "app", []sqlx.TableRef{ref("app", "x")}, []string{"app"}},
		{"WITH x AS (SELECT 1 AS a) SELECT * EXCEPT (a) FROM x", "app", nil, nil},
		// 限定名不受 CTE 影响
		{"WITH x AS (SELECT 1) SELECT * FROM db2.x", "app", []sqlx.TableRef{ref("db2", "x")}, []string{"db2"}},
		// CTE 作用域限于定义它的括号
		{"SELECT * FROM (WITH x AS (SELECT 1) SELECT * FROM x) a JOIN x ON 1", "app", []sqlx.TableRef{ref("app", "x")}, []string{"app"}},
		{"SELECT * FROM t WHERE a IN (WITH x AS (SELECT 1 AS a) SELECT a FROM x)", "app", []sqlx.TableRef{ref("app", "t")}, []string{"app"}},
		// 子查询、UNION、嵌套连接
		{"SELECT * FROM t UNION ALL SELECT * FROM u UNION ALL SELECT * FROM db2.v", "app", []sqlx.TableRef{ref("app", "t"), ref("app", "u"), ref("db2", "v")}, []string{"app", "db2"}},
		{"SELECT * FROM (t JOIN u ON t.id = u.id)", "app", []sqlx.TableRef{ref("app", "t"), ref("app", "u")}, []string{"app"}},
		{"SELECT (SELECT max(a) FROM s) FROM t", "app", []sqlx.TableRef{ref("app", "s"), ref("app", "t")}, []string{"app"}},
		// ARRAY JOIN 的表达式不是表
		{"SELECT * FROM t ARRAY JOIN arr AS a, arr2 AS b", "app", []sqlx.TableRef{ref("app", "t")}, []string{"app"}},
		{"SELECT * FROM t LEFT ARRAY JOIN arr LEFT JOIN u ON 1", "app", []sqlx.TableRef{ref("app", "t"), ref("app", "u")}, []string{"app"}},
		// 函数里的 FROM 不是表
		{"SELECT extract(year FROM d) FROM t", "app", []sqlx.TableRef{ref("app", "t")}, []string{"app"}},
		// 逗号：LIMIT / GROUP BY / ORDER BY 里的逗号不会把下一个词当成表
		{"SELECT a, b FROM t GROUP BY a, b ORDER BY a, b LIMIT 1, 2", "app", []sqlx.TableRef{ref("app", "t")}, []string{"app"}},
		{"SELECT a FROM t WHERE x = 1 AND y IN (1, 2), z", "app", []sqlx.TableRef{ref("app", "t")}, []string{"app"}},
		// 字典 / joinGet
		{"SELECT dictGet('d', 'a', 1) FROM t", "app", []sqlx.TableRef{ref("app", "d"), ref("app", "t")}, []string{"app"}},
		{"SELECT dictGetString('db2.d', 'a', 1), dictHas('d2', 1), joinGet('db3.j', 'v', 1)", "app", []sqlx.TableRef{ref("db2", "d"), ref("app", "d2"), ref("db3", "j")}, []string{"app", "db2", "db3"}},
		// numbers 不是表
		{"SELECT * FROM numbers(3) n JOIN t ON 1", "app", []sqlx.TableRef{ref("app", "t")}, []string{"app"}},
		// SHOW / DESCRIBE / EXISTS / EXPLAIN
		{"SHOW TABLES FROM db2", "app", nil, []string{"db2"}},
		{"SHOW CREATE TABLE t", "app", []sqlx.TableRef{ref("app", "t")}, []string{"app"}},
		{"SHOW CREATE TABLE db2.t", "app", []sqlx.TableRef{ref("db2", "t")}, []string{"db2"}},
		{"SHOW CREATE DATABASE db2", "app", nil, []string{"db2"}},
		{"SHOW COLUMNS FROM t FROM db2", "app", []sqlx.TableRef{ref("db2", "t")}, []string{"db2"}},
		{"SHOW COLUMNS FROM db2.t", "app", []sqlx.TableRef{ref("db2", "t")}, []string{"db2"}},
		{"SHOW INDEXES IN t", "app", []sqlx.TableRef{ref("app", "t")}, []string{"app"}},
		{"DESCRIBE db2.t", "app", []sqlx.TableRef{ref("db2", "t")}, []string{"db2"}},
		{"EXISTS TABLE t", "app", []sqlx.TableRef{ref("app", "t")}, []string{"app"}},
		{"EXISTS DATABASE db2", "app", nil, []string{"db2"}},
		{"EXPLAIN PLAN SELECT * FROM t JOIN db2.u ON 1", "app", []sqlx.TableRef{ref("app", "t"), ref("db2", "u")}, []string{"app", "db2"}},
		// 当前库是 system 时，未限定的表名按 system 校验
		{"SELECT * FROM tables", "system", []sqlx.TableRef{ref("system", "tables")}, []string{"system"}},
	}
	for _, c := range cases {
		ck, err := CheckUserQuery(c.q, CheckOptions{DB: c.db})
		if err != nil {
			t.Errorf("%q: %v", c.q, err)
			continue
		}
		if !reflect.DeepEqual(ck.Tables, c.want) {
			t.Errorf("%q: Tables = %v, want %v", c.q, ck.Tables, c.want)
		}
		if !reflect.DeepEqual(ck.Databases, c.dbs) {
			t.Errorf("%q: Databases = %v, want %v", c.q, ck.Databases, c.dbs)
		}
	}
	// 当前库是 system 时，未限定的敏感表仍被拒绝
	if _, err := CheckUserQuery("SELECT * FROM users", CheckOptions{DB: "system"}); err == nil {
		t.Error("当前库为 system 时 FROM users 应被拒绝")
	}
	if _, err := CheckUserQuery("SELECT * FROM users", CheckOptions{DB: "SYSTEM"}); err == nil {
		t.Error("大小写不同的 SYSTEM 也应被拒绝")
	}
}

// 绕过尝试：把危险内容藏进注释 / 字符串 / 引号，或者用词法歧义让两边理解不一致。
func TestCheckBypassAttempts(t *testing.T) {
	denied := []string{
		"SELECT 1 /* */ SETTINGS /* */ max_threads = 1",
		"SELECT 1\nSETTINGS\nmax_threads=1",
		"SELECT 1 -- x\nSETTINGS max_threads=1",
		"SELECT * FROM /* system.databases */ system.users",
		"SELECT * FROM system.databases; SELECT * FROM system.users",
		"SELECT '--'; DROP TABLE t",
		"SELECT 1 -- ; \n; DROP TABLE t",
		"SELECT 'a'' ; DROP TABLE t; --'; DROP TABLE t",
		"SELECT 1 /* ; */ ; INSERT INTO t VALUES (1)",
		"SeLeCt * FrOm FiLe('x', 'CSV', 'a String')",
		"SELECT * FROM\tfile\n('x', 'CSV', 'a String')",
		"SELECT * FROM file /* c */ ('x', 'CSV', 'a String')",
		"SELECT * FROM t FORMAT /* c */ JSON; SELECT 1",
		"SELECT * FROM system.users FORMAT JSON",
		"SELECT * FROM t WHERE x = 'a' SETTINGS readonly = 0",
		"SELECT * FROM system.users WHERE name = ' FROM system.databases'",
		"SELECT * FROM t1 JOIN system.users ON 1",
		"SELECT * FROM t1 , system.users",
		"SELECT * FROM t1 CROSS JOIN system.users",
		"SELECT * FROM t1 ASOF JOIN system.users ON 1",
		"SELECT * FROM (t1 JOIN (system.users) ON 1)",
		"SELECT * FROM ((SELECT 1) a JOIN system.users b ON 1)",
		"SELECT * FROM t WHERE x IN (SELECT 1 UNION ALL SELECT name FROM system.users)",
		"SELECT * FROM t ARRAY JOIN arr, (SELECT name FROM system.users)",
		"SELECT * FROM numbers(1) UNION ALL SELECT * FROM system.users",
		"SELECT 1 UNION ALL SELECT sleep(1)",
		"SELECT if(1, sleep(1), 0)",
		"SELECT arrayMap(x -> sleep(x), [1])",
		"SELECT * FROM t WHERE sleep(1)",
		"SELECT * FROM t PREWHERE sleep(1)",
		"SELECT * FROM t ORDER BY sleep(1)",
		"WITH sleep(1) AS s SELECT s",
		"WITH x AS (SELECT sleep(1)) SELECT * FROM x",
		"SELECT * FROM (SELECT * FROM (SELECT * FROM (SELECT * FROM system.users)))",
		"EXPLAIN SELECT sleep(1)",
		"EXPLAIN PLAN actions = 1 SELECT * FROM remote('h', db.t)",
		"SHOW CREATE TABLE `system`.`users`",
		"SELECT * FROM system.tables t JOIN system.columns c ON 1 JOIN system.role_grants g ON 1",
	}
	for _, q := range denied {
		if _, err := CheckUserQuery(q); err == nil {
			t.Errorf("绕过尝试应被拒绝：%q", q)
		}
	}
}

func TestCheckRowsOfUserError(t *testing.T) {
	_, err := CheckUserQuery("DROP TABLE t")
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "SQL 工单") {
		t.Errorf("err = %v", err)
	}
	if fmt.Sprint(err) != ue.Msg {
		t.Error("Error() 应等于 Msg")
	}
}

func TestLexer(t *testing.T) {
	toks, err := lex("SELECT 'a''b', \"x\"\"y\", `z`, 1.5e-3, 0x1F, $$a$$, $t$b$t$ /* c */ -- d\n;")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tk := range toks {
		got = append(got, fmt.Sprintf("%d:%s", tk.kind, tk.val+tk.text))
	}
	if len(toks) != 15 { // SELECT 'a''b' , "x""y" , `z` , 1.5e-3 , 0x1F , $$a$$ , $t$b$t$ ;
		t.Fatalf("记号数 = %d: %v", len(toks), got)
	}
	if toks[1].val != "a'b" || toks[3].val != `x"y` || toks[5].val != "z" || toks[11].val != "a" || toks[13].val != "b" {
		t.Errorf("值不对: %v", got)
	}
	if toks[7].text != "1.5e-3" || toks[9].text != "0x1F" {
		t.Errorf("数字: %v", got)
	}
}

// 随机拼接词法片段：检查器不能 panic，并且凡是放行的语句，其中都不应出现被拒绝的写入类首词或 SETTINGS 等关键字。
func TestCheckRandomSoup(t *testing.T) {
	frags := []string{"SELECT", "WITH", "FROM", "JOIN", "ARRAY", "IN", "AS", "(", ")", ",", ".", ";", "'", "''", "\"", "`", "--", "/*", "*/", "$$", "$a$",
		"system", "users", "t", "x", "numbers", "file", "sleep", "SETTINGS", "FORMAT", "INTO", "UNION", "ALL", "EXCEPT", "dictGet", "'a.b'", "1", "0x1", "*",
		"\n", " ", "\\", "{", "}", "EXPLAIN", "SHOW", "TABLES", "CREATE", "LIMIT", "WHERE", "=", "+"}
	seed := uint32(12345)
	next := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>8) % n
	}
	allowed := 0
	for i := 0; i < 20000; i++ {
		var sb strings.Builder
		for k, n := 0, 1+next(14); k < n; k++ {
			sb.WriteString(frags[next(len(frags))])
			sb.WriteByte(' ')
		}
		q := sb.String()
		ck, err := CheckUserQuery(q, CheckOptions{DB: "app"})
		if err != nil {
			continue
		}
		allowed++
		toks, lerr := lex(ck.Stmt) // 实际发给服务端的文本
		if lerr != nil {
			t.Fatalf("放行的语句词法分析失败 %q: %v", q, lerr)
		}
		for j, tk := range toks {
			if tk.kind == tkWord && (j == 0 || toks[j-1].text != ".") {
				switch tk.lower() {
				case "settings", "into", "outfile", "sleep":
					t.Fatalf("放行了含 %s 的语句 %q", tk.text, q)
				case "file", "url", "remote":
					if j+1 < len(toks) && toks[j+1].isPunct("(") {
						t.Fatalf("放行了表函数 %s(: %q", tk.text, q)
					}
				}
			}
			if tk.kind == tkQuoted && strings.EqualFold(tk.val, "sleep") {
				t.Fatalf("放行了含 sleep 的语句 %q", q)
			}
		}
		for _, r := range ck.Tables {
			if strings.EqualFold(r.Schema, "system") && !allowedSystemTables[strings.ToLower(r.Name)] {
				t.Fatalf("放行了 system.%s: %q", r.Name, q)
			}
		}
	}
	if allowed == 0 {
		t.Log("随机语句里没有被放行的（仅验证不 panic）")
	}
}
