// @author: kevinkelly
// @license: Apache Licence
// @file: readonly_batch_test.go
// @time: 2026/10/28

package tsql

import (
	"strings"
	"testing"
)

// 在线查询放开的“只读批处理”：DECLARE / SET @变量 打头，最后 SELECT 输出结果。
func TestReadOnlyBatchAllowed(t *testing.T) {
	ok := []string{
		"DECLARE @SiteId INT = 1008;\nDECLARE @c NVARCHAR(20) = (SELECT TOP 1 v FROM s);\nIF ISNULL(@c, N'') = N'' THROW 50001, N'未配置', 1;\nSELECT @c AS c, @SiteId AS s",
		"DECLARE @a INT = 1; SET @a = @a + 1; SELECT @a AS a",
		"DECLARE @a INT = 1; IF @a = 1 BEGIN SELECT @a AS a END ELSE BEGIN SELECT 0 AS a END",
		"DECLARE @a INT = 1; BEGIN TRY SELECT @a AS a END TRY BEGIN CATCH THROW; END CATCH",
		// 字符串 / 注释里的关键字不算
		"DECLARE @s NVARCHAR(MAX) = N'';\nSELECT N'INSERT INTO t SELECT 1; COMMIT; BEGIN TRAN; WITH (UPDLOCK)' AS x -- RAISERROR\n",
		"SELECT * FROM t WITH (NOLOCK)",
	}
	for _, s := range ok {
		if why := ReadOnlyViolation(s); why != "" {
			t.Errorf("应放行: %s\n原因: %s", s, why)
		}
	}
}

// 往表变量里攒中间结果：DECLARE @t TABLE (…) 声明过的表变量允许 INSERT。
func TestReadOnlyTableVarInsertAllowed(t *testing.T) {
	ok := []string{
		"DECLARE @areas TABLE (Area varchar(10));\nINSERT INTO @areas\nSELECT DISTINCT REPLACE(JSON_VALUE(j.value, '$.Area'), '+', '')\nFROM s CROSS APPLY OPENJSON(s.Value2) j;\nSELECT a.Area FROM @areas a",
		"DECLARE @t TABLE (a INT); INSERT INTO @t VALUES (1), (2); SELECT a FROM @t",
		"DECLARE @t AS TABLE (a INT); INSERT @t SELECT 1; SELECT a FROM @t",
		"DECLARE @t TABLE (a INT, b INT); INSERT INTO @t (a, b) SELECT 1, 2 UNION ALL SELECT 3, 4; SELECT a, b FROM @t",
		"DECLARE @T TABLE (a INT); INSERT INTO @t SELECT 1; SELECT a FROM @T",
		// 字符串 / 注释里的 INSERT 不算
		"DECLARE @a INT = 1; SELECT N'INSERT INTO t SELECT 1' AS x -- INSERT INTO t",
	}
	for _, s := range ok {
		if why := ReadOnlyViolation(s); why != "" {
			t.Errorf("应放行: %s\n原因: %s", s, why)
		}
	}
}

func TestReadOnlyTableVarInsertForbidden(t *testing.T) {
	bad := []struct{ name, sql, want string }{
		{"写真表", "DECLARE @a INT = 1; INSERT INTO t SELECT 1; SELECT @a", "INSERT 只能写入"},
		{"写架构.表", "DECLARE @a INT = 1; INSERT INTO dbo.t SELECT 1; SELECT @a", "INSERT 只能写入"},
		{"写临时表", "DECLARE @t TABLE (a INT); INSERT INTO #t SELECT 1; SELECT 1", "INSERT 只能写入"},
		{"变量没用 TABLE 声明", "DECLARE @a INT = 1; INSERT INTO @t SELECT 1; SELECT @a", "INSERT 只能写入"},
		{"标量变量", "DECLARE @t INT = 1; INSERT INTO @t SELECT 1; SELECT 1", "INSERT 只能写入"},
		{"INSERT TOP 带修饰", "DECLARE @t TABLE (a INT); INSERT TOP (1) INTO @t SELECT 1; SELECT a FROM @t", "INSERT 只能写入"},
		{"INSERT … EXEC", "DECLARE @t TABLE (a INT); INSERT INTO @t EXEC('select 1'); SELECT a FROM @t", "exec"},
		{"OUTPUT … INTO 真表", "DECLARE @t TABLE (a INT); INSERT INTO @t OUTPUT inserted.a INTO dbo.log SELECT 1; SELECT a FROM @t", "output"},
		// INSERT … OUTPUT（不带 INTO）会把写入的行当成第一个结果集返回，脱敏按 SELECT 列表匹配，换个别名就绕过去了
		{"INSERT … OUTPUT 返回结果集", "DECLARE @t TABLE (x VARCHAR(20), y INT); INSERT INTO @t (x, y) OUTPUT inserted.x AS leak SELECT phone, 1 FROM users; SELECT 1 AS a", "output"},
		{"INSERT … OUTPUT 小写", "declare @t table (x varchar(20)); insert into @t output inserted.x select phone from users; select x from @t", "output"},
		{"SELECT INTO 仍然拒绝", "DECLARE @t TABLE (a INT); SELECT 1 AS a INTO #x; SELECT a FROM @t", "into"},
		{"INSERT 夹带写真表", "DECLARE @t TABLE (a INT); INSERT INTO @t SELECT 1; INSERT INTO dbo.t SELECT 1; SELECT a FROM @t", "INSERT 只能写入"},
		{"表变量不放开 UPDATE", "DECLARE @t TABLE (a INT); UPDATE @t SET a = 1; SELECT a FROM @t", "update"},
		{"表变量不放开 DELETE", "DECLARE @t TABLE (a INT); DELETE FROM @t; SELECT a FROM @t", "delete"},
		{"表变量里夹带危险语句", "DECLARE @t TABLE (a INT); INSERT INTO @t SELECT 1 FROM OPENROWSET('a','b','c'); SELECT a FROM @t", "openrowset"},
	}
	for _, c := range bad {
		why := ReadOnlyViolation(c.sql)
		if why == "" {
			t.Errorf("%s: 应拒绝: %s", c.name, c.sql)
			continue
		}
		if !strings.Contains(strings.ToLower(why), strings.ToLower(c.want)) {
			t.Errorf("%s: 原因 %q 应含 %q", c.name, why, c.want)
		}
	}
}

func TestReadOnlyBatchForbidden(t *testing.T) {
	bad := []struct{ name, sql, want string }{
		{"事务", "DECLARE @a INT = 1; BEGIN TRAN; SELECT @a", "tran"},
		{"事务全称", "DECLARE @a INT = 1; BEGIN TRANSACTION; SELECT @a", "transaction"},
		{"提交", "DECLARE @a INT = 1; COMMIT; SELECT @a", "commit"},
		{"回滚", "DECLARE @a INT = 1; ROLLBACK; SELECT @a", "rollback"},
		{"RAISERROR", "DECLARE @a INT = 1; RAISERROR('x', 16, 1) WITH LOG; SELECT @a", "raiserror"},
		{"THROW 用变量带出值", "DECLARE @k NVARCHAR(MAX) = (SELECT TOP 1 pk FROM t); THROW 50001, @k, 1; SELECT 1", "THROW"},
		{"序列", "SELECT NEXT VALUE FOR dbo.seq AS n", "NEXT VALUE FOR"},
		{"更新锁", "SELECT * FROM t WITH (UPDLOCK)", "updlock"},
		{"排他表锁", "SELECT * FROM t WITH (TABLOCKX)", "tablockx"},
		{"持有锁", "SELECT * FROM t WITH (HOLDLOCK)", "holdlock"},
		{"共享表锁", "SELECT * FROM t WITH (TABLOCK)", "tablock"},
		{"持有读锁", "SELECT * FROM t WITH (REPEATABLEREAD)", "repeatableread"},
		{"覆盖会话隔离级别", "SELECT * FROM t WITH (READCOMMITTED)", "readcommitted"},
		{"覆盖会话隔离级别（加锁读）", "SELECT * FROM t WITH (READCOMMITTEDLOCK)", "readcommittedlock"},
		{"旧写法的表提示", "SELECT * FROM t (TABLOCK)", "tablock"},
		{"多个提示里夹带", "SELECT * FROM t WITH (NOLOCK, TABLOCK)", "tablock"},
		{"JOIN 的表提示", "SELECT a.x FROM a WITH (NOLOCK) JOIN b WITH (REPEATABLEREAD) ON a.id = b.id", "repeatableread"},
		{"TABLE HINT 查询提示", "SELECT a FROM t OPTION (TABLE HINT(t, TABLOCK))", "tablock"},
		{"写入夹带", "DECLARE @a INT = 1; DELETE FROM t; SELECT @a", "delete"},
		{"动态 SQL", "DECLARE @q NVARCHAR(MAX) = N'select 1'; EXEC(@q)", "exec"},
		{"SET 选项", "DECLARE @a INT = 1; SET NOCOUNT ON; SELECT @a", "SET @变量"},
		{"循环", "DECLARE @a INT = 1; WHILE @a < 10 SET @a = @a + 1; SELECT @a", "while"},
		{"游标", "DECLARE c CURSOR FOR SELECT 1", "DECLARE @变量"},
	}
	for _, c := range bad {
		why := ReadOnlyViolation(c.sql)
		if why == "" {
			t.Errorf("%s: 应拒绝: %s", c.name, c.sql)
			continue
		}
		if !strings.Contains(strings.ToLower(why), strings.ToLower(c.want)) {
			t.Errorf("%s: 原因 %q 应含 %q", c.name, why, c.want)
		}
	}
}

// 不会加锁（或者本来就是在线查询的默认行为）的表提示、以及名字里只是包含这些词的标识符，不能被误拦。
func TestReadOnlyAllowsHarmlessHints(t *testing.T) {
	ok := []string{
		"SELECT * FROM t WITH (NOLOCK)",
		"SELECT * FROM t WITH (READUNCOMMITTED)",
		"SELECT * FROM t WITH (NOLOCK, INDEX(1))",
		"SELECT * FROM t WITH (FORCESEEK)",
		"SELECT [tablock] AS tablock_cnt, readcommitted_flag FROM t WITH (NOLOCK)",
		"SELECT 'WITH (TABLOCK) READCOMMITTED' AS note FROM t -- REPEATABLEREAD\n",
	}
	for _, q := range ok {
		if why := ReadOnlyViolation(q); why != "" {
			t.Errorf("不应拒绝 %q：%s", q, why)
		}
	}
}

// INSERT INTO @表变量 … SELECT 是写数据，不是产出结果的那条查询：起点要落在后面真正输出结果的 SELECT / WITH 上。
func TestMainQueryStartSkipsTableVarInsert(t *testing.T) {
	const decl = "declare @t table (a int); "
	cases := []struct {
		name, sql string
		want      string // 起点处开始的文本前缀；"" 表示找不到
	}{
		{"insert 自己的 select 跳过", decl + "insert into @t select 1; select a from @t", "select a from @t"},
		{"UNION 分支也跳过", decl + "insert into @t select 1 union all select 2; select a from @t", "select a from @t"},
		{"没有分号直接跟着的 select 是新语句", decl + "insert into @t select 1 select a from @t", "select a from @t"},
		{"INSERT 前面的 CTE", decl + "with c as (select 1 as x) insert into @t select x from c; select a from @t", "select a from @t"},
		{"VALUES", decl + "insert into @t values (1); select a from @t", "select a from @t"},
		{"表提示里的 WITH 不算", decl + "insert into @t select a from x with (nolock); select a from @t", "select a from @t"},
		{"之后的 WITH 是输出查询", decl + "insert into @t select 1; with c as (select a from @t) select a from c", "with c as"},
		{"只有写入、没有输出", decl + "insert into @t select 1", ""},
		{"只有 CTE 写入、没有输出", decl + "with c as (select 1 as x) insert into @t select x from c", ""},
	}
	for _, c := range cases {
		sig := Significant(Tokenize(c.sql))
		i := MainQueryStart(sig)
		got := ""
		if i >= 0 {
			got = c.sql[sig[i].Start:]
		}
		if c.want == "" && got != "" || c.want != "" && !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: 起点 %q，期望以 %q 开头", c.name, got, c.want)
		}
	}
}

func TestMainQueryStart(t *testing.T) {
	cases := []struct {
		name, sql string
		want      string // 起点处的词；"" 表示找不到
	}{
		{"普通 select", "select 1", "select"},
		{"with 打头", "with c as (select 1 x) select x from c", "with"},
		{"declare 之后", "declare @a int = (select 1); select @a as a", "select"},
		{"括号里的 select 不算", "declare @a int = (select 1); set @a = (select 2)", ""},
		{"只给变量赋值的 select 跳过", "declare @a int; select @a = 1; select @a as a", "select"},
		{"带 CTE", "declare @a int = 1; with c as (select @a as x) select x from c", "with"},
		{"IF 之后", "declare @a int = 1; if @a = 1 select @a as a", "select"},
		{"空", "", ""},
	}
	for _, c := range cases {
		sig := Significant(Tokenize(c.sql))
		i := MainQueryStart(sig)
		got := ""
		if i >= 0 {
			got = strings.ToLower(sig[i].Text)
		}
		if got != c.want {
			t.Errorf("%s: 起点 %q，期望 %q", c.name, got, c.want)
		}
	}
}
