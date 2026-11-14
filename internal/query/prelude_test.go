// @author: kevinkelly
// @license: Apache Licence
// @file: prelude_test.go
// @time: 2026/10/28

package query

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"dbtool/internal/model"
)

// ---- 语句检查 ----

func TestCheckMySQLScriptAccepts(t *testing.T) {
	s := newSvc(newRepo(), &fakeEngine{})
	cases := []struct {
		name, sql string
		sets      int
		stmt      string
	}{
		{"没有 SET：与原行为一致", "select 1; select 2", 0, "select 1"},
		{"用户变量与 SELECT", "SET @a := 1; select @a", 1, "select @a"},
		{"= 与 := 都行", "set @a = 1; set @b := 'x'; select @a, @b", 2, "select @a, @b"},
		{"一条 SET 赋多个", "set @a = 1, @b := 2; select @a + @b", 1, "select @a + @b"},
		{"白名单会话参数", "SET block_encryption_mode = 'aes-256-cbc'; select 1", 1, "select 1"},
		{"SESSION 前缀与 @@session.", "set session group_concat_max_len = 100000; set @@session.time_zone = '+08:00'; select 1", 2, "select 1"},
		{"带子查询的赋值", "set @org := (select org_id from tenant where id = @site); select * from t where org = @org", 1, "select * from t where org = @org"},
		{"注释被去掉", "-- 先设置\nset @a := 1; /* x */ select @a -- tail", 1, "select @a"},
		{"SET 打头的脚本：全部执行，最后一条是结果", "set @a := 1; select @a; select 2", 2, "select 2"},
	}
	for _, c := range cases {
		pre, stmt, _, err := s.checkMySQLScript(c.sql, "shop", Policy{})
		if err != nil {
			t.Errorf("%s: 不应报错: %v", c.name, err)
			continue
		}
		if len(pre.stmts) != c.sets || stmt != c.stmt {
			t.Errorf("%s: 前置 %d 条（%q）、查询 %q；期望 %d 条、%q", c.name, len(pre.stmts), pre.stmts, stmt, c.sets, c.stmt)
		}
	}
}

func TestCheckMySQLScriptRejects(t *testing.T) {
	s := newSvc(newRepo(), &fakeEngine{})
	cases := []struct{ name, sql string }{
		{"只有 SET", "set @a := 1"},
		{"SET GLOBAL", "set global read_only = 0; select 1"},
		{"SET @@global.", "set @@global.max_connections = 1; select 1"},
		{"SET PERSIST", "set persist max_connections = 1; select 1"},
		{"SET PASSWORD", "set password = 'x'; select 1"},
		{"SET ROLE", "set role all; select 1"},
		{"SET sql_mode", "set sql_mode = ''; select 1"},
		{"SET autocommit", "set autocommit = 0; select 1"},
		{"SET 事务读写", "set session transaction read write; select 1"},
		{"SET NAMES", "set names utf8mb4; select 1"},
		{"SET sql_log_bin", "set sql_log_bin = 0; select 1"},
		{"SET max_execution_time", "set max_execution_time = 0; select 1"},
		{"参数值不合法", "set block_encryption_mode = 'rot13'; select 1"},
		{"参数值是表达式", "set group_concat_max_len = 100 * 100; select 1"},
		{"参数值越界", "set group_concat_max_len = 99999999; select 1"},
		{"赋值写 INTO", "set @a := (select a into @b from t); select 1"},
		{"危险函数", "set @a := sleep(1000); select @a"},
		{"危险函数（嵌套）", "set @a := (select benchmark(100000000, md5('x'))); select @a"},
		{"读文件", "set @a := load_file('/etc/passwd'); select @a"},
		{"系统库", "set @a := (select user from mysql.user limit 1); select @a"},
		{"加锁读取", "set @a := (select id from t for update); select @a"},
		{"赋值里的写操作", "set @a := (delete from t); select @a"},
		{"没写赋值", "set @a; select 1"},
		{"末尾多逗号", "set @a = 1, ; select 1"},
		{"括号不匹配", "set @a := (select 1; select 1"},
		{"带引号的变量名", "set @`a b` := 1; select 1"},
		{"SET 之后跟的是写语句", "set @a := 1; insert into t values (@a)"},
		{"SET 之后跟 PREPARE", "set @s := 'drop table t'; prepare p from @s; execute p"},
		{"SET 之后跟 CALL", "set @a := 1; call p(@a)"},
	}
	for _, c := range cases {
		if _, _, _, err := s.checkMySQLScript(c.sql, "shop", Policy{}); err == nil || code(err) != "bad_query" {
			t.Errorf("%s: 应被拒绝（bad_query）: %v", c.name, err)
		}
	}
	// 条数上限
	var b strings.Builder
	for i := 0; i < maxPreludeSets+1; i++ {
		b.WriteString("set @a := 1;")
	}
	if _, _, _, err := s.checkMySQLScript(b.String()+" select @a", "shop", Policy{}); err == nil {
		t.Error("前置 SET 超过上限应被拒绝")
	}
}

// 错误信息要指出是哪一条 SET，方便用户修改。
func TestCheckMySQLScriptErrorPointsAtStatement(t *testing.T) {
	s := newSvc(newRepo(), &fakeEngine{})
	_, _, _, err := s.checkMySQLScript("set @a := 1; set global x = 1; select 1", "shop", Policy{})
	if err == nil || !strings.Contains(err.Error(), "第 2 条语句") {
		t.Errorf("应指出第 2 条: %v", err)
	}
}

// 其它调用方（导出工单等）用的 Check 不受影响：仍然只认查询。
func TestPublicCheckStillRejectsSet(t *testing.T) {
	s := newSvc(newRepo(), &fakeEngine{})
	if _, _, err := s.Check("set @a := 1; select @a", "shop", Policy{}); code(err) != "bad_query" {
		t.Errorf("Check 不应放行 SET: %v", err)
	}
}

// ---- 执行 ----

func TestExecuteWithPrelude(t *testing.T) {
	r := newRepo()
	eng := &fakeEngine{rows: 2}
	script := "SET @SiteId := 1008;\nSET @OrgId := (SELECT OrgId FROM shopbasic.shop_info WHERE Id = @SiteId);\nSELECT @SiteId AS site, @OrgId AS org"
	res, err := newSvc(r, eng).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shopbasic", SQL: script})
	if err != nil {
		t.Fatal(err)
	}
	wantPre := []string{"SET @SiteId := 1008", "SET @OrgId := (SELECT OrgId FROM shopbasic.shop_info WHERE Id = @SiteId)"}
	if !reflect.DeepEqual(eng.gotOpt.Prelude, wantPre) {
		t.Errorf("引擎应收到前置语句: %q", eng.gotOpt.Prelude)
	}
	if !strings.HasPrefix(eng.gotSQL, "SELECT @SiteId AS site, @OrgId AS org") {
		t.Errorf("查询: %q", eng.gotSQL)
	}
	if !strings.Contains(res.Notice, "SET 2 条") {
		t.Errorf("应提示先执行了前置 SET: %q", res.Notice)
	}
	// 留痕：日志和“实际执行的 SQL”里都带上前置语句
	full := strings.Join(wantPre, ";\n") + ";\n" + eng.gotSQL
	if r.logs[0].SQLLog != full || res.ExecutedSQL != full {
		t.Errorf("日志 / 展示应包含前置语句:\n日志 %q\n展示 %q\n期望 %q", r.logs[0].SQLLog, res.ExecutedSQL, full)
	}
}

// 没有前置 SET 时，引擎收不到前置语句，行为不变。
func TestExecuteWithoutPreludeUnchanged(t *testing.T) {
	r := newRepo()
	eng := &fakeEngine{rows: 1}
	res, err := newSvc(r, eng).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select 1"})
	if err != nil || len(eng.gotOpt.Prelude) != 0 || r.logs[0].SQLLog != eng.gotSQL || res.ExecutedSQL != eng.gotSQL {
		t.Errorf("err=%v opt=%+v log=%q", err, eng.gotOpt, r.logs[0].SQLLog)
	}
}

// 前置 SET 里的子查询按独立查询校验库表权限：不能把没有权限的表读进变量再带出来。
func TestPreludeSubqueryNeedsTablePrivilege(t *testing.T) {
	r := devRepo() // dev 只有 shop.orders 与 report 库的权限
	eng := &fakeEngine{rows: 1}
	svc := newSvc(r, eng)
	req := func(sql string) Request { return Request{InstanceID: 1, DB: "shop", SQL: sql} }

	_, err := svc.Execute(context.Background(), dev, req("set @k := (select secret from vault.api_keys limit 1); select id from orders where id = 1 or '1' = @k"))
	if code(err) != "no_privilege" {
		t.Fatalf("子查询里的无权限表应被拒绝: %v", err)
	}
	if eng.gotSQL != "" {
		t.Error("被拒绝时不应连到库执行任何语句")
	}
	// 主查询没有表，只靠变量带出数据：同样要校验
	if _, err = svc.Execute(context.Background(), dev, req("set @k := (select secret from vault.api_keys limit 1); select @k")); code(err) != "no_privilege" {
		t.Errorf("没有表的主查询也不能绕过: %v", err)
	}
	// 有权限的表放行
	if _, err = svc.Execute(context.Background(), dev, req("set @id := (select max(id) from orders); select id from orders where id = @id")); err != nil {
		t.Errorf("有权限的表应放行: %v", err)
	}
	if len(eng.gotOpt.Prelude) != 1 {
		t.Errorf("前置语句应交给引擎: %q", eng.gotOpt.Prelude)
	}
}

// 变量把敏感字段带进结果列：脱敏要把前置 SET 一起纳入分析。
func TestPreludeVariableCarriesMaskedColumn(t *testing.T) {
	r := maskRepo()
	e := &maskEngine{cols: []string{"p", "n"}, data: [][]any{{"13812345678", 1}}}
	script := "set @p := (select phone from customers where id = 201); set @n := 1; select @p as p, @n as n"
	res, err := newMaskSvc(r, e, nil).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: script})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0]; got != "138****5678" || res.Rows[0][1] != 1 {
		t.Errorf("变量带出的手机号必须脱敏、无关列不动: %v", res.Rows)
	}
	if !reflect.DeepEqual(res.MaskedColumns, []string{"p"}) {
		t.Errorf("masked=%v", res.MaskedColumns)
	}
}

// 同一条语句里先 @v := 敏感列、再读 @v 的绕过（原来就存在）也要被堵上。
func TestSameStatementVariableCarriesMaskedColumn(t *testing.T) {
	r := maskRepo()
	e := &maskEngine{cols: []string{"a", "b"}, data: [][]any{{"13812345678", "13812345678"}}}
	res, err := newMaskSvc(r, e, nil).Execute(context.Background(), admin,
		Request{InstanceID: 1, DB: "shop", SQL: "select @v := phone as a, @v as b from customers limit 1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][0] != "138****5678" || res.Rows[0][1] != "138****5678" {
		t.Errorf("两列都应脱敏: %v", res.Rows)
	}
}

// 防火墙的表对象条件要看到前置 SET 里的子查询。
func TestPreludeFirewallSeesSubqueryTables(t *testing.T) {
	r := newRepo()
	svc := newSvc(r, &fakeEngine{rows: 1})
	pre, stmt, _, err := svc.checkMySQLScript("set @a := (select x from secret_t limit 1); select @a", "shop", Policy{})
	if err != nil || len(pre.probes) != 1 || stmt != "select @a" {
		t.Fatalf("probes=%v stmt=%q err=%v", pre.probes, stmt, err)
	}
	refs, err := svc.fwTables(context.Background(), &model.Instance{DBType: "mysql"}, "shop", pre.probes[0])()
	if err != nil || len(refs) != 1 || refs[0].Name != "secret_t" {
		t.Errorf("探针语句应能解析出子查询里的表: %v %v", refs, err)
	}
}

// 用户贴过来的脚本（截图里那一段）的结构：6 条 SET + 加密模式校验 + 币种校验 + 最后的生成查询。
func TestUserScriptFromScreenshot(t *testing.T) {
	script := "SET block_encryption_mode = 'aes-256-cbc';\n" +
		"SET @SiteId := 1008;\n" +
		"SET @SrcCurrency := 'USD';  -- V1 源站点币种（生成时烧入，勿改）\n" +
		"SET @OrgId := (SELECT OrgId FROM shopbasic.shop_info WHERE Id = @SiteId);\n" +
		"SET @DstCurrency := (SELECT REPLACE(IFNULL(SupportSysCurrency,''),' ','') FROM shopbasic.shop_info WHERE Id = @SiteId);\n" +
		"SET @CurrencyOk := (@OrgId IS NOT NULL AND IFNULL(@DstCurrency,'') <> '' AND FIND_IN_SET(@SrcCurrency, @DstCurrency) <> 0);\n\n" +
		"-- ① 加密模式校验：必须最先跑。SET 是会话级的，换连接/重连即失效\n" +
		"SELECT @@session.block_encryption_mode AS 加密模式,\n" +
		"       IF(@@session.block_encryption_mode = 'aes-256-cbc', '模式正确-可执行',\n" +
		"          (SELECT '中断：block_encryption_mode 不是 aes-256-cbc,请在本连接重新执行第一条 SET'\n" +
		"             FROM (SELECT 1 UNION ALL SELECT 2) e)) AS 模式校验;\n\n" +
		"-- ② 币种校验\n" +
		"SELECT @SiteId AS SiteId, @OrgId AS OrgId, IF(@CurrencyOk, '币种一致-可执行',\n" +
		"          (SELECT '中断：源站点币种与目标站点币种不一致' FROM (SELECT 1 UNION ALL SELECT 2) e)) AS 币种校验;\n\n" +
		"SELECT t.PayCode, @OrgId AS OrgId FROM (SELECT 'A' AS PayCode UNION ALL SELECT 'B') t ORDER BY t.PayCode;\n"
	s := newSvc(newRepo(), &fakeEngine{})
	pre, stmt, notice, err := s.checkMySQLScript(script, "shopbasic", Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pre.stmts) != 8 || len(pre.queryIdx) != 2 || len(pre.probes) != 2 {
		t.Errorf("前置 %d 条（想要 8）、其中查询 %d 条（想要 2）、含子查询的赋值 %d 个（想要 2）", len(pre.stmts), len(pre.queryIdx), len(pre.probes))
	}
	if !strings.HasPrefix(stmt, "SELECT t.PayCode") {
		t.Errorf("结果应是最后一条查询: %q", stmt)
	}
	if !strings.Contains(notice, "9 条语句（SET 6 条、查询 3 条）") {
		t.Errorf("notice: %q", notice)
	}
}

// 脚本里只要有一条不是 SET / 查询，整份拒绝，一条都不执行（不能先执行前面的、再在写语句上失败）。
func TestScriptRejectedAsAWhole(t *testing.T) {
	r := newRepo()
	eng := &fakeEngine{rows: 1}
	svc := newSvc(r, eng)
	for name, sql := range map[string]string{
		"中间有 INSERT":    "set @a := 1; select @a; insert into t values (@a); select 2",
		"最后有 UPDATE":    "set @a := 1; select @a; update t set x = 1",
		"最后一条是 SET":     "set @a := 1; select @a; set @b := 2",
		"中间有 DELETE":    "set @a := 1; delete from t; select 1",
		"中间有 INTO @变量":  "set @a := 1; select phone into @p from t limit 1; select @p",
		"最后一条 INTO":     "set @a := 1; select phone into @p from t limit 1",
		"中间有 INTO 输出文件": "set @a := 1; select * from t into outfile '/tmp/x'; select 1",
		"中间有加锁读取":       "set @a := 1; select * from t for update; select 1",
		"中间读 mysql 库":   "set @a := 1; select user from mysql.user; select 1",
		"中间有 PREPARE":   "set @s := 'select 1'; prepare p from @s; select 1",
	} {
		_, err := svc.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: sql})
		if code(err) != "bad_query" {
			t.Errorf("%s: 应整份拒绝（bad_query）: %v", name, err)
		}
	}
	if eng.gotSQL != "" || len(r.logs) != 0 {
		t.Errorf("被拒绝的脚本不应连到库、不应写日志: sql=%q logs=%d", eng.gotSQL, len(r.logs))
	}
	// 没有 SET 打头的多语句仍然只执行第一条
	if res, err := svc.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select 1; delete from t"}); err != nil || !strings.Contains(res.Notice, "只执行第 1 条") || len(eng.gotOpt.Prelude) != 0 {
		t.Errorf("没有 SET 打头时保持原行为: %v %v", err, res)
	}
}

// 脚本：所有语句按顺序交给引擎，最后一条是结果；中间的查询按行数上限改写，日志与展示包含整份脚本。
func TestExecuteScriptRunsAllInOrder(t *testing.T) {
	r := newRepo()
	eng := &fakeEngine{rows: 3}
	script := "SET @site := 1008;\nSELECT @site AS s;\nSELECT id FROM orders WHERE site = @site"
	res, err := newSvc(r, eng).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: script, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(eng.gotOpt.Prelude) != 2 || eng.gotOpt.Prelude[0] != "SET @site := 1008" || !strings.HasPrefix(eng.gotOpt.Prelude[1], "SELECT @site AS s") || !strings.HasSuffix(eng.gotOpt.Prelude[1], "LIMIT 100") {
		t.Errorf("中间的查询应在前置语句里并按行数上限改写: %q", eng.gotOpt.Prelude)
	}
	if !strings.HasPrefix(eng.gotSQL, "SELECT id FROM orders WHERE site = @site") || res.RowCount != 3 {
		t.Errorf("最后一条的结果: %q rows=%d", eng.gotSQL, res.RowCount)
	}
	if !strings.Contains(res.Notice, "3 条语句（SET 1 条、查询 2 条）") {
		t.Errorf("notice: %q", res.Notice)
	}
	for _, want := range []string{"SET @site := 1008", "SELECT @site AS s", "SELECT id FROM orders WHERE site = @site"} {
		if !strings.Contains(r.logs[0].SQLLog, want) || !strings.Contains(res.ExecutedSQL, want) {
			t.Errorf("日志 / 展示应包含整份脚本，缺 %q", want)
		}
	}
}

// 中间的查询逐条校验库表权限：不能把没权限的表藏在“被丢弃的结果”里。
func TestScriptIntermediateQueryNeedsPrivilege(t *testing.T) {
	r := devRepo() // dev 只有 shop.orders 与 report 库的权限
	eng := &fakeEngine{rows: 1}
	svc := newSvc(r, eng)
	_, err := svc.Execute(context.Background(), dev, Request{InstanceID: 1, DB: "shop", SQL: "set @a := 1; select * from users; select id from orders where id = @a"})
	if code(err) != "no_privilege" {
		t.Fatalf("中间的查询没有权限应拒绝: %v", err)
	}
	if eng.gotSQL != "" {
		t.Error("被拒绝时不应连到库")
	}
	if _, err = svc.Execute(context.Background(), dev, Request{InstanceID: 1, DB: "shop", SQL: "set @a := 1; select max(id) from orders; select id from orders where id = @a"}); err != nil {
		t.Errorf("都有权限应放行: %v", err)
	}
}

// 中间的查询里给变量赋敏感列的值，最后一条读变量：同样脱敏（结果丢弃不代表变量没带出数据）。
func TestScriptIntermediateQueryAssignsMaskedColumn(t *testing.T) {
	r := maskRepo()
	e := &maskEngine{cols: []string{"p"}, data: [][]any{{"13812345678"}}}
	script := "set @n := 1; select @v := phone from customers where id = 201; select @v as p"
	res, err := newMaskSvc(r, e, nil).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: script})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][0] != "138****5678" {
		t.Errorf("变量带出的手机号必须脱敏: %v", res.Rows)
	}
}

func TestScriptLimits(t *testing.T) {
	s := newSvc(newRepo(), &fakeEngine{})
	var b strings.Builder
	b.WriteString("set @a := 1;")
	for i := 0; i < maxScriptQuery+1; i++ {
		b.WriteString("select 1;")
	}
	if _, _, _, err := s.checkMySQLScript(b.String(), "shop", Policy{}); err == nil || !strings.Contains(err.Error(), "查询最多") {
		t.Errorf("查询条数超限应拒绝: %v", err)
	}
	b.Reset()
	for i := 0; i < maxScriptStmts+1; i++ {
		b.WriteString("set @a := 1;")
	}
	b.WriteString("select @a")
	if _, _, _, err := s.checkMySQLScript(b.String(), "shop", Policy{}); err == nil {
		t.Error("语句条数超限应拒绝")
	}
}

// 用户脚本的完整骨架（三条查询里最后一条是带窗口函数 / 派生表 / 关联子查询的生成语句；凭证数据用占位值）：
// 整份脚本应通过检查、不被当成写语句，最后一条是要返回的结果。
func TestUserFullScriptSkeleton(t *testing.T) {
	script := `SET block_encryption_mode = 'aes-256-cbc';
SET @SiteId := 1008;
SET @SrcCurrency := 'USD';  -- V1 源站点币种（生成时烧入，勿改）
SET @OrgId := (SELECT OrgId FROM shopbasic.shop_info WHERE Id = @SiteId);
SET @DstCurrency := (SELECT REPLACE(IFNULL(SupportSysCurrency,''),' ','') FROM shopbasic.shop_info WHERE Id = @SiteId);
SET @CurrencyOk := (@OrgId IS NOT NULL AND IFNULL(@DstCurrency,'') <> '' AND FIND_IN_SET(@SrcCurrency, @DstCurrency) <> 0);

SELECT @@session.block_encryption_mode AS 加密模式,
       IF(@@session.block_encryption_mode = 'aes-256-cbc', '模式正确-可执行',
          (SELECT '中断：block_encryption_mode 不是 aes-256-cbc，请在本连接重新执行第一条 SET'
             FROM (SELECT 1 UNION ALL SELECT 2) e)) AS 模式校验;

-- ② 币种校验：源/目标站点币种不一致时直接报错中断，禁止继续往下执行
SELECT @SiteId AS SiteId, @OrgId AS OrgId, @SrcCurrency AS 源站点币种, @DstCurrency AS 目标站点币种,
       IF(@CurrencyOk, '币种一致-可执行',
          (SELECT '中断：源站点币种与目标站点币种不一致' FROM (SELECT 1 UNION ALL SELECT 2) e)) AS 币种校验;

SELECT t.PayCode                                                        AS PayCode,
       @OrgId                                                           AS OrgId,
       IF(t.IsNew = 1, CONCAT(t.BaseName, IF(t.NameSeq = 1, '', CONCAT('-', t.NameSeq))), em.CustomName) AS CustomName,
       t.ApiCode                                                   AS ApiCode,
       t.MK                                                             AS ApiKey_明文,
       IF(t.MK  = '', '', TO_BASE64(AES_ENCRYPT(t.MK,  MD5(t.Salt), LEFT(MD5(t.Salt),16)))) AS ApiKey_密文,
       t.PRI                                                            AS PrivateKey_明文,
       IF(t.PRI = '', '', TO_BASE64(AES_ENCRYPT(t.PRI, MD5(t.Salt), LEFT(MD5(t.Salt),16)))) AS PrivateKey_密文,
       IF(t.MK = '' AND t.PUB = '' AND t.PRI = '', '', t.Salt)   AS SaltValue,
       0                                                                AS Balance,
       -1                                                               AS BalanceUpdateTime,
       1                                                                AS State,
       ''                                                               AS Remark,
       'system'                                                         AS Creator,
       0                                                                AS IsDeleted,
       t.ExistId                                                        AS 已存在密钥Id,
       CASE WHEN @@session.block_encryption_mode <> 'aes-256-cbc' THEN '中断-加密模式不对'
            WHEN NOT IFNULL(@CurrencyOk, 0) THEN '中断-币种不一致'
            WHEN t.DictOk = 0               THEN '跳过-厂商未在字典登记'
            WHEN t.ExistId IS NOT NULL      THEN '跳过-凭证已存在'
            ELSE '将插入' END                                          AS 执行判定
FROM (
  SELECT a.*,
         IF(@@session.block_encryption_mode = 'aes-256-cbc' AND IFNULL(@CurrencyOk, 0) AND a.DictOk = 1 AND a.ExistId IS NULL, 1, 0) AS IsNew,
         a.MaxNameSeq + SUM(IF(@@session.block_encryption_mode = 'aes-256-cbc' AND IFNULL(@CurrencyOk, 0) AND a.DictOk = 1 AND a.ExistId IS NULL, 1, 0))
                        OVER (PARTITION BY a.PayCode ORDER BY a.Seq ROWS UNBOUNDED PRECEDING) AS NameSeq
  FROM (
    SELECT s.*,
           CONCAT(s.PayCode, '（', @SiteId, '）') AS BaseName,
           IF(EXISTS (SELECT 1 FROM shopbasic.pay_code_dict d WHERE d.PayCode = s.PayCode), 1, 0) AS DictOk,
           (SELECT MIN(m.Id) FROM shopbasic.merchant_account m
             WHERE m.OrgId = @OrgId AND m.IsDeleted = 0 AND m.PayCode = s.PayCode
               AND IFNULL(m.ApiCode,'') = s.ApiCode
               AND IFNULL(IF(IFNULL(m.SaltValue,'')='', m.ApiKey, CONVERT(AES_DECRYPT(FROM_BASE64(m.ApiKey), MD5(m.SaltValue), LEFT(MD5(m.SaltValue),16)) USING utf8mb4)),'') = s.MK) AS ExistId,
           (SELECT IFNULL(MAX(IF(x.CustomName = CONCAT(s.PayCode, '（', @SiteId, '）'), 1,
                   CAST(SUBSTRING(x.CustomName, CHAR_LENGTH(CONCAT(s.PayCode, '（', @SiteId, '）')) + 2) AS UNSIGNED))), 0)
              FROM shopbasic.merchant_account x
             WHERE x.OrgId = @OrgId AND x.DeleteTime = 0   -- 与 uq_OrgId_CustomName 同口径
               AND (x.CustomName = CONCAT(s.PayCode, '（', @SiteId, '）')
                    OR (x.CustomName LIKE CONCAT(s.PayCode, '（', @SiteId, '）-%')
                        AND SUBSTRING(x.CustomName, CHAR_LENGTH(CONCAT(s.PayCode, '（', @SiteId, '）')) + 2) REGEXP '^[0-9]+$'))) AS MaxNameSeq
    FROM (
    SELECT 'DemoPayINR' AS PayCode, 1 AS Seq, 'demo-merchant' AS ApiCode, '' AS MK, '' AS PUB, 'demo-private-key' AS PRI, 'c2FsdA==' AS Salt
  UNION ALL
    SELECT 'DemoPay2INR' AS PayCode, 1 AS Seq, 'demo-2' AS ApiCode, 'demo-mk' AS MK, '' AS PUB, '' AS PRI, 'c2FsdDI=' AS Salt
    ) s
  ) a
) t
LEFT JOIN shopbasic.merchant_account em ON em.Id = t.ExistId
ORDER BY 执行判定, t.PayCode, t.Seq;
`
	s := newSvc(newRepo(), &fakeEngine{rows: 2})
	pre, stmt, notice, err := s.checkMySQLScript(script, "shopbasic", Policy{})
	if err != nil {
		t.Fatalf("整份脚本应通过检查: %v", err)
	}
	if len(pre.stmts) != 8 || len(pre.queryIdx) != 2 {
		t.Errorf("前置 %d 条（想要 8）、查询 %d 条（想要 2）", len(pre.stmts), len(pre.queryIdx))
	}
	if !strings.HasPrefix(stmt, "SELECT t.PayCode") || !strings.HasSuffix(stmt, "ORDER BY 执行判定, t.PayCode, t.Seq") {
		t.Errorf("最后一条应是生成查询: %q…%q", stmt[:20], stmt[len(stmt)-30:])
	}
	if !strings.Contains(notice, "9 条语句") {
		t.Errorf("notice: %q", notice)
	}
	// 实际执行：所有语句按顺序交给引擎，最后一条的行数是返回结果
	eng := &fakeEngine{rows: 2}
	res, err := newSvc(newRepo(), eng).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shopbasic", SQL: script, Limit: 500})
	if err != nil || res.RowCount != 2 || len(eng.gotOpt.Prelude) != 8 {
		t.Fatalf("执行: err=%v res=%+v prelude=%d", err, res, len(eng.gotOpt.Prelude))
	}
}
