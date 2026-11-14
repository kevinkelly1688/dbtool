// @author: kevinkelly
// @license: Apache Licence
// @file: mssql_batch_test.go
// @time: 2026/10/28

package query

import (
	"strings"
	"testing"
)

const mssqlBatch = "DECLARE @SiteId INT = 1008;   -- 换站点只改这里\n" +
	"DECLARE @Currency NVARCHAR(20) =\n    (SELECT TOP 1 LTRIM(RTRIM(s.Value1)) FROM shop_settings s WHERE s.SettingKey = 'CurrentCurrency');\n" +
	"IF ISNULL(@Currency, N'') = N''\n    THROW 50001, N'未配置 CurrentCurrency', 1;\n" +
	"SELECT @Currency AS c, p.Code FROM pay_channel p WHERE p.Currency = @Currency AND p.SiteId = @SiteId;"

func TestCheckMSSQLBatch(t *testing.T) {
	s := &Service{}
	stmt, notice, err := s.checkMSSQL(mssqlBatch, Policy{})
	if err != nil || notice != "" || !strings.HasPrefix(stmt, "DECLARE @SiteId") {
		t.Fatalf("DECLARE 打头的只读批处理应放行: stmt=%q notice=%q err=%v", stmt, notice, err)
	}
	// 只有声明、没有输出结果的批处理
	_, _, err = s.checkMSSQL("DECLARE @a INT = 1; SET @a = (SELECT 2)", Policy{})
	wantQueryErr(t, err, "没有输出结果的 SELECT")
	// 夹带写入 / 事务 / 动态 SQL
	for _, bad := range []string{
		"DECLARE @a INT = 1; DELETE FROM t; SELECT @a",
		"DECLARE @a INT = 1; BEGIN TRAN; SELECT @a",
		"DECLARE @q NVARCHAR(MAX) = N'x'; EXEC(@q); SELECT 1",
		"DECLARE @a INT = 1; SET NOCOUNT ON; SELECT @a",
		"DECLARE @a INT = 1; SELECT @a INTO #t",
	} {
		if _, _, err := s.checkMSSQL(bad, Policy{}); err == nil {
			t.Errorf("应拒绝: %s", bad)
		}
	}
	// 非 SELECT / WITH / DECLARE / SET 打头仍然拒绝，信息保持以前的提示
	_, _, err = s.checkMSSQL("UPDATE t SET a = 1", Policy{})
	wantQueryErr(t, err, "仅支持 SELECT / WITH")
	// 四部分名称（链接服务器）在批处理里同样拦截
	_, _, err = s.checkMSSQL("DECLARE @a INT = (SELECT TOP 1 x FROM srv.db.dbo.t); SELECT @a", Policy{})
	wantQueryErr(t, err, "链接服务器")
	// 禁用 SELECT * 对批处理同样生效
	_, _, err = s.checkMSSQL("DECLARE @a INT = 1; SELECT * FROM t WHERE id = @a", Policy{DisableStar: true})
	wantQueryErr(t, err, "SELECT *")
}

// 往表变量里攒中间结果再统计：INSERT INTO @t 放行，行数上限改写的是最后输出结果的那条查询。
const mssqlTableVarBatch = "DECLARE @TenantId nvarchar(50) = (SELECT Merchant FROM lottery.dbo.tab_ThirdGameVendor WHERE Id = 100);\n" +
	"DECLARE @areas TABLE (Area varchar(10));\n" +
	"INSERT INTO @areas\n" +
	"SELECT DISTINCT REPLACE(JSON_VALUE(j.value, '$.Area'), '+', '')\n" +
	"FROM lottery.dbo.tab_Settings s\n" +
	"CROSS APPLY OPENJSON(s.Value2) j\n" +
	"WHERE s.SettingKey = 'H5PhoneAreaCodeDictionary';\n" +
	"SELECT @TenantId AS TenantId, a.Area AS Area, COUNT(1) AS n\n" +
	"FROM lottery.dbo.tab_Users u WITH (NOLOCK)\n" +
	"CROSS APPLY (SELECT TOP 1 x.Area FROM @areas x WHERE u.UserName LIKE x.Area + '%' ORDER BY LEN(x.Area) DESC) a\n" +
	"GROUP BY a.Area"

func TestCheckMSSQLTableVarInsert(t *testing.T) {
	s := &Service{}
	stmt, _, err := s.checkMSSQL(mssqlTableVarBatch, Policy{})
	if err != nil || !strings.HasPrefix(stmt, "DECLARE @TenantId") {
		t.Fatalf("写表变量的只读批处理应放行: stmt=%q err=%v", stmt, err)
	}
	// 只有写入、没有输出结果的 SELECT
	_, _, err = s.checkMSSQL("DECLARE @t TABLE (a INT); INSERT INTO @t SELECT 1", Policy{})
	wantQueryErr(t, err, "没有输出结果的 SELECT")
	// 写真表 / 临时表仍然拒绝，并且提示说清楚只能写表变量
	for _, bad := range []string{
		"DECLARE @t TABLE (a INT); INSERT INTO dbo.t SELECT 1; SELECT a FROM @t",
		"DECLARE @t TABLE (a INT); INSERT INTO #t SELECT 1; SELECT a FROM @t",
	} {
		_, _, err := s.checkMSSQL(bad, Policy{})
		wantQueryErr(t, err, "INSERT 只能写入")
	}
	// INSERT … OUTPUT 会把写入的行当成第一个结果集返回，脱敏按 SELECT 列表匹配，OUTPUT 里换个别名就绕过去了：一律拒绝
	_, _, err = s.checkMSSQL("DECLARE @t TABLE (x VARCHAR(20), y INT); INSERT INTO @t (x, y) OUTPUT inserted.x AS leak SELECT phone, 1 FROM users; SELECT 1 AS a", Policy{})
	wantQueryErr(t, err, "不允许出现 output")
	// 写入语句里的 SELECT * 同样受“禁用 SELECT *”约束
	_, _, err = s.checkMSSQL("DECLARE @t TABLE (a INT); INSERT INTO @t SELECT * FROM x; SELECT a FROM @t", Policy{DisableStar: true})
	wantQueryErr(t, err, "SELECT *")
	// 开头不是 DECLARE / SET / SELECT / WITH 的 INSERT 仍然是原来的提示
	_, _, err = s.checkMSSQL("INSERT INTO t SELECT 1", Policy{})
	wantQueryErr(t, err, "仅支持 SELECT / WITH")
}

func TestMSSQLRewriteTopTableVarInsert(t *testing.T) {
	got := mssqlRewriteTop(mssqlTableVarBatch, 500)
	if !strings.Contains(got, "SELECT TOP 500 @TenantId AS TenantId") {
		t.Errorf("行数上限应加在最后输出结果的 SELECT 上:\n%s", got)
	}
	if strings.Contains(got, "SELECT DISTINCT TOP") || strings.Count(got, "TOP 500") != 1 {
		t.Errorf("INSERT 里的 SELECT 不应被改写:\n%s", got)
	}
}

func wantQueryErr(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("期望错误包含 %q，实际 %v", contains, err)
	}
}

func TestMSSQLRewriteTopBatch(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"单条 select", "SELECT a FROM t", "SELECT TOP 500 a FROM t"},
		{"批处理改写最后的查询而不是 DECLARE", "DECLARE @a INT = 1; SELECT x FROM t WHERE id = @a",
			"DECLARE @a INT = 1; SELECT TOP 500 x FROM t WHERE id = @a"},
		{"变量初始化里的子查询不动", "DECLARE @a INT = (SELECT TOP 1 v FROM s); SELECT x FROM t",
			"DECLARE @a INT = (SELECT TOP 1 v FROM s); SELECT TOP 500 x FROM t"},
		{"已有更大的 TOP 压到上限", "DECLARE @a INT = 1; SELECT TOP 100000 x FROM t",
			"DECLARE @a INT = 1; SELECT TOP 500 x FROM t"},
		{"已有更小的 TOP 不动", "DECLARE @a INT = 1; SELECT TOP 10 x FROM t",
			"DECLARE @a INT = 1; SELECT TOP 10 x FROM t"},
		{"CTE", "DECLARE @a INT = 1; WITH c AS (SELECT 1 x) SELECT x FROM c",
			"DECLARE @a INT = 1; WITH c AS (SELECT 1 x) SELECT TOP 500 x FROM c"},
		{"赋值型 select 跳过", "DECLARE @a INT; SELECT @a = 1; SELECT x FROM t",
			"DECLARE @a INT; SELECT @a = 1; SELECT TOP 500 x FROM t"},
		{"UNION 不改写（引擎兜底截断）", "DECLARE @a INT = 1; SELECT 1 AS x UNION ALL SELECT 2",
			"DECLARE @a INT = 1; SELECT 1 AS x UNION ALL SELECT 2"},
	}
	for _, c := range cases {
		if got := mssqlRewriteTop(c.in, 500); got != c.want {
			t.Errorf("%s:\n  得到 %s\n  期望 %s", c.name, got, c.want)
		}
	}
}
