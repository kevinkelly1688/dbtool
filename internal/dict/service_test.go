// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package dict

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var ctx = context.Background()

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return "?" + err.Error()
}

func TestInstancesScopeAndSupport(t *testing.T) {
	e := newEnv()
	all, err := e.svc.Instances(root)
	if err != nil || len(all) != 8 {
		t.Fatalf("超级用户看到全部: %v %d", err, len(all))
	}
	got := map[string]InstanceView{}
	for _, v := range all {
		got[v.Name] = v
	}
	if v := got["prod-mysql-01"]; !v.Supported || len(v.Kinds) != 6 {
		t.Errorf("mysql: %+v", v)
	}
	if v := got["mssql-01"]; !v.Supported || len(v.Kinds) != 5 {
		t.Errorf("mssql 没有定时任务类别: %+v", v)
	}
	if v := got["doris-01"]; !v.Supported || len(v.Kinds) != 1 || v.Kinds[0] != KindTable {
		t.Errorf("doris 只开放表: %+v", v)
	}
	if v := got["cass-01"]; v.Supported || len(v.Kinds) != 0 {
		t.Errorf("cassandra 应标为暂不支持: %+v", v)
	}
	if v := got["rv-mysql"]; !v.Supported || len(v.Kinds) != 6 {
		t.Errorf("带旧标签 read_view 的实例与普通实例一样开放全部类别: %+v", v)
	}
	own, _ := e.svc.Instances(bob)
	for _, v := range own {
		if v.Name == "other-mysql" {
			t.Errorf("bob 不应看到未关联的实例")
		}
	}
	if len(own) != 7 {
		t.Errorf("bob 可见 %d", len(own))
	}
	// 未关联实例：所有读取入口都拒绝，也不会去连接
	if _, err := e.svc.Databases(ctx, bob, 6); code(err) != "no_privilege" {
		t.Errorf("%v", err)
	}
	if _, err := e.svc.Objects(ctx, bob, 6, "d", KindTable); code(err) != "no_privilege" {
		t.Errorf("%v", err)
	}
	if _, err := e.svc.Detail(ctx, bob, DetailRequest{InstanceID: 6, DB: "d", Kind: KindTable, Name: "t"}); code(err) != "no_privilege" {
		t.Errorf("%v", err)
	}
	if _, err := e.svc.Databases(ctx, root, 99); code(err) != "not_found" {
		t.Errorf("%v", err)
	}
	if _, err := e.svc.Databases(ctx, root, 3); code(err) != "unsupported_engine" {
		t.Errorf("cassandra 应提示暂不支持: %v", err)
	}
	if len(e.opens) != 0 {
		t.Errorf("以上都不应连接目标库: %+v", e.opens)
	}
}

func TestDatabasesFiltering(t *testing.T) {
	e := newEnv()
	e.q.on("FROM information_schema.SCHEMATA", row("information_schema"), row("mysql"), row("performance_schema"), row("sys"), row("test"), row("app_shop"), row("app_secret_x"), row("report"), row("app_a"))
	// 无过滤规则的实例：只去掉系统库
	dbs, err := e.svc.Databases(ctx, root, 1)
	if err != nil || strings.Join(dbs, ",") != "app_a,app_secret_x,app_shop,report" {
		t.Fatalf("%v %v", err, dbs)
	}
	// show 与 denied 正则（从开头匹配）
	dbs, err = e.svc.Databases(ctx, bob, 5)
	if err != nil || strings.Join(dbs, ",") != "app_a,app_shop" {
		t.Fatalf("%v %v", err, dbs)
	}
	if !e.q.closed {
		t.Error("连接用完必须关闭")
	}
	// 非法正则：拒绝而不是放行
	e.src.instances[0].DeniedDBNameRegex = "([unclosed"
	if _, err = e.svc.Databases(ctx, root, 1); code(err) != "bad_config" {
		t.Fatalf("%v", err)
	}
}

func TestDBAndNameValidation(t *testing.T) {
	e := newEnv()
	for name, db := range map[string]string{"系统库": "mysql", "系统库大小写": "Information_Schema", "空": " ", "控制字符": "a\x00b", "换行": "a\nb", "过长": strings.Repeat("x", 129)} {
		if _, err := e.svc.Objects(ctx, root, 1, db, KindTable); err == nil {
			t.Errorf("%s: 应拒绝", name)
		}
	}
	// 被实例过滤规则禁止的库，即使知道名字也不行
	if _, err := e.svc.Objects(ctx, root, 5, "app_secret_x", KindTable); code(err) != "db_not_allowed" {
		t.Errorf("denied: %v", err)
	}
	if _, err := e.svc.Objects(ctx, root, 5, "report", KindTable); code(err) != "db_not_allowed" {
		t.Errorf("不匹配 show 规则: %v", err)
	}
	if _, err := e.svc.Objects(ctx, root, 1, "shop", "nope"); code(err) != "bad_request" {
		t.Errorf("未知类别: %v", err)
	}
	if _, err := e.svc.Objects(ctx, root, 7, "shop", KindView); code(err) != "unsupported_kind" {
		t.Errorf("doris 不支持视图: %v", err)
	}
	for _, name := range []string{"", "  ", "a\x01b", strings.Repeat("n", 129)} {
		if _, err := e.svc.Detail(ctx, root, DetailRequest{InstanceID: 1, DB: "shop", Kind: KindTable, Name: name}); code(err) != "bad_request" {
			t.Errorf("对象名 %q: %v", name, err)
		}
	}
	if len(e.opens) != 0 {
		t.Errorf("校验失败不应连接目标库: %+v", e.opens)
	}
}

func TestMySQLObjectsAndTableDetail(t *testing.T) {
	e := newEnv()
	e.q.on("SELECT TABLE_NAME, IFNULL(TABLE_COMMENT", row("orders", "订单主表"), row("coupons", "优惠券"), row("users", ""))
	items, err := e.svc.Objects(ctx, root, 1, "shop", KindTable)
	if err != nil || len(items) != 3 || items[0].Name != "orders" || items[0].Comment != "订单主表" {
		t.Fatalf("%v %+v", err, items)
	}
	if e.opens[0].db != "shop" || e.opens[0].dbType != "mysql" {
		t.Fatalf("%+v", e.opens)
	}
	// 库名只能作为参数传递，不能拼进语句
	if c := e.q.calls[0]; strings.Contains(c.sql, "shop") || len(c.args) != 1 || c.args[0] != "shop" {
		t.Fatalf("库名必须参数化: %+v", c)
	}

	e = newEnv()
	e.q.on("TABLE_ROWS, AVG_ROW_LENGTH", row("orders", "InnoDB", "Dynamic", int64(88214990), int64(182), int64(15937331200), int64(6710886400), int64(524288), uint64(88300001), "utf8mb4_0900_ai_ci", "2024-03-12 10:00:00", nil, "订单主表"))
	e.q.on("FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?",
		row("id", "bigint unsigned", "NO", nil, "PRI", "auto_increment", "主键"),
		row("status", "varchar(16)", "NO", []byte("pending"), "", "", "状态"),
		row("note", "text", "YES", nil, "", "", ""))
	e.q.on("FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?",
		row("orders", "PRIMARY", int64(0), "BTREE", "id"),
		row("orders", "idx_us", int64(1), "BTREE", "user_id"), row("orders", "idx_us", int64(1), "BTREE", "status"))
	e.q.on("SHOW CREATE TABLE", row("orders", "CREATE TABLE `orders` (\n  `id` bigint unsigned NOT NULL\n) ENGINE=InnoDB"))
	det, err := e.svc.Detail(ctx, root, DetailRequest{InstanceID: 1, DB: "shop", Kind: KindTable, Name: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if len(det.Sections) != 4 || det.Sections[0].Title != "表信息" || det.Sections[3].Type != "code" || !strings.Contains(det.Sections[3].Text, "CREATE TABLE `orders`") {
		t.Fatalf("%+v", det.Sections)
	}
	pairs := map[string]string{}
	for _, p := range det.Sections[0].Pairs {
		pairs[p.Key] = p.Value
	}
	if pairs["表行数（估算）"] != "88214990" || pairs["数据大小(KB)"] != "15563800.00" || pairs["当前自增值"] != "88300001" || pairs["表说明"] != "订单主表" {
		t.Fatalf("表信息: %+v", pairs)
	}
	cols := det.Sections[1].Rows
	if len(cols) != 3 || cols[0][2] != "否" || cols[0][3] != "NULL" || cols[1][3] != "pending" || cols[2][2] != "是" {
		t.Fatalf("列: %+v", cols)
	}
	idx := det.Sections[2].Rows
	if len(idx) != 2 || idx[0][0] != "PRIMARY" || idx[0][2] != "是" || idx[1][1] != "user_id, status" || idx[1][2] != "否" {
		t.Fatalf("索引应按索引合并列: %+v", idx)
	}
	// SHOW CREATE 里的库名、表名要转义反引号
	e.q.calls = nil
	_, _ = e.svc.Detail(ctx, root, DetailRequest{InstanceID: 1, DB: "sh`op", Kind: KindTable, Name: "t`x"})
	if !strings.Contains(e.q.sqls(), "SHOW CREATE TABLE `sh``op`.`t``x`") {
		t.Fatalf("标识符转义: %s", e.q.sqls())
	}
}

func TestMySQLDetailDegradesGracefully(t *testing.T) {
	e := newEnv()
	e.q.on("TABLE_ROWS, AVG_ROW_LENGTH", row("t", "InnoDB", "Dynamic", int64(1), int64(1), int64(1024), int64(0), int64(0), nil, "c", nil, nil, ""))
	e.q.fail("FROM information_schema.STATISTICS", errors.New("denied"))
	e.q.fail("SHOW CREATE TABLE", errors.New("SHOW command denied to user"))
	det, err := e.svc.Detail(ctx, root, DetailRequest{InstanceID: 1, DB: "shop", Kind: KindTable, Name: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if det.Sections[2].Type != "note" || !strings.Contains(det.Sections[2].Text, "获取失败") || det.Sections[3].Type != "note" {
		t.Fatalf("索引或建表语句取不到时只应降级为说明: %+v", det.Sections)
	}
	// 对象不存在
	e = newEnv()
	if _, err := e.svc.Detail(ctx, root, DetailRequest{InstanceID: 1, DB: "shop", Kind: KindTable, Name: "nope"}); code(err) != "not_found" {
		t.Fatalf("%v", err)
	}
	// 连接失败 / 查询失败
	e = newEnv()
	e.openEr = errors.New("dial tcp 10.0.0.1:3306: i/o timeout")
	if _, err := e.svc.Objects(ctx, root, 1, "shop", KindTable); code(err) != "engine_error" || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("%v", err)
	}
	e = newEnv()
	e.q.fail("information_schema.TABLES", errors.New("Error 1142: SELECT command denied"))
	if _, err := e.svc.Objects(ctx, root, 1, "shop", KindTable); code(err) != "engine_error" {
		t.Fatalf("%v", err)
	}
}

func TestMySQLOtherKinds(t *testing.T) {
	e := newEnv()
	e.q.on("FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? AND", row("trg_a", "BEFORE", "INSERT", "orders", "ROW", "root@%", "2024-01-01", "STRICT", "utf8mb4", "utf8mb4_0900", "BEGIN SET NEW.x=1; END"))
	det, err := e.svc.Detail(ctx, root, DetailRequest{InstanceID: 1, DB: "shop", Kind: KindTrigger, Name: "trg_a"})
	if err != nil || len(det.Sections) != 2 || det.Sections[1].Text != "BEGIN SET NEW.x=1; END" {
		t.Fatalf("%v %+v", err, det)
	}
	e = newEnv()
	e.q.on("FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ? AND ROUTINE_NAME", row("fn", "shop", "int", "root@%", "c", "l", "m", "DEFINER", "说明"))
	e.q.on("SHOW CREATE FUNCTION", row("fn", "m", "CREATE FUNCTION `fn`() RETURNS int RETURN 1", "u", "c", "c"))
	det, err = e.svc.Detail(ctx, root, DetailRequest{InstanceID: 1, DB: "shop", Kind: KindFunction, Name: "fn"})
	if err != nil || det.Sections[1].Text != "CREATE FUNCTION `fn`() RETURNS int RETURN 1" {
		t.Fatalf("%v %+v", err, det)
	}
	hasRet := false
	for _, p := range det.Sections[0].Pairs {
		hasRet = hasRet || (p.Key == "返回类型" && p.Value == "int")
	}
	if !hasRet {
		t.Fatalf("函数应有返回类型: %+v", det.Sections[0])
	}
	e = newEnv()
	e.q.on("FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ? AND", row("ev", "shop", "root@%", "RECURRING", int64(1), "DAY", "ENABLED", nil, "s", nil, nil, "NOT PRESERVE", "c", "l", ""))
	e.q.on("SHOW CREATE EVENT", row("ev", "m", "SYSTEM", "CREATE EVENT `ev` ON SCHEDULE EVERY 1 DAY DO SELECT 1"))
	det, err = e.svc.Detail(ctx, root, DetailRequest{InstanceID: 1, DB: "shop", Kind: KindEvent, Name: "ev"})
	if err != nil || !strings.Contains(det.Sections[1].Text, "CREATE EVENT `ev`") {
		t.Fatalf("%v %+v", err, det)
	}
	e = newEnv()
	e.q.on("FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", row("v_a", "select 1", "NONE", "YES", "root@%", "DEFINER", "utf8mb4", "c"))
	e.q.on("FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", row("id", "int", "NO", nil, "", "", ""))
	det, err = e.svc.Detail(ctx, root, DetailRequest{InstanceID: 1, DB: "shop", Kind: KindView, Name: "v_a"})
	if err != nil || len(det.Sections) != 3 || det.Sections[2].Text != "select 1" {
		t.Fatalf("%v %+v", err, det)
	}
}

// 带旧标签 read_view 的实例与普通实例一致：不限制对象类别和视图名前缀，视图定义与定义者照常展示。
func TestLegacyReadViewTagIsNotRestricted(t *testing.T) {
	e := newEnv()
	// 表、触发器等非视图类别可以看
	e.q.on("SELECT TABLE_NAME, IFNULL(TABLE_COMMENT", row("orders", "订单"), row("v_users", ""))
	items, err := e.svc.Objects(ctx, root, 4, "shop", KindTable)
	if err != nil || len(items) != 2 || items[0].Name != "orders" {
		t.Fatalf("%v %+v", err, items)
	}
	// 视图列表不按 v_ 前缀过滤
	e.q.on("FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ? ORDER BY", row("v_orders", ""), row("V_upper", ""), row("orders_view", ""))
	items, err = e.svc.Objects(ctx, root, 4, "shop", KindView)
	if err != nil || len(items) != 3 {
		t.Fatalf("%v %+v", err, items)
	}
	// 非 v_ 视图的详情可以看，且展示定义与定义者
	e.q.on("FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", row("orders_view", "select id from base_table", "NONE", "NO", "root@%", "DEFINER", "utf8mb4", "c"))
	e.q.on("FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", row("id", "int", "NO", nil, "", "", ""))
	det, err := e.svc.Detail(ctx, root, DetailRequest{InstanceID: 4, DB: "shop", Kind: KindView, Name: "orders_view"})
	if err != nil {
		t.Fatal(err)
	}
	if len(det.Sections) != 3 || det.Sections[2].Type != "code" || det.Sections[2].Text != "select id from base_table" {
		t.Fatalf("应展示视图定义: %+v", det.Sections)
	}
	definer := false
	for _, p := range det.Sections[0].Pairs {
		definer = definer || (p.Key == "定义者" && p.Value == "root@%")
	}
	if !definer {
		t.Fatalf("应展示定义者: %+v", det.Sections[0])
	}
	// 非视图类别的详情也不再被拒绝
	if _, err := e.svc.Detail(ctx, root, DetailRequest{InstanceID: 4, DB: "shop", Kind: KindTrigger, Name: "trg_x"}); code(err) == "read_view_only" || code(err) == "forbidden" {
		t.Fatalf("%v", err)
	}
}

func TestLegacyReadViewTagMSSQLListsAllViews(t *testing.T) {
	e := newEnv()
	e.q.on("FROM sys.views v", row("dbo", "V_Orders", ""), row("dbo", "orders_v", ""), row("sales", "v_x", ""))
	items, err := e.svc.Objects(ctx, root, 8, "shop", KindView)
	if err != nil || len(items) != 3 {
		t.Fatalf("SQL Server 实例带旧标签也不过滤视图名: %v %+v", err, items)
	}
}

func TestMSSQLListAndDetail(t *testing.T) {
	e := newEnv()
	e.q.on("FROM sys.tables t JOIN sys.schemas s", row("dbo", "orders", "订单"), row("sales", "orders", ""))
	items, err := e.svc.Objects(ctx, bob, 2, "shop", KindTable)
	if err != nil || len(items) != 2 || items[0].Schema != "dbo" || items[1].Schema != "sales" {
		t.Fatalf("%v %+v", err, items)
	}
	if e.opens[0].db != "shop" || e.opens[0].dbType != "mssql" {
		t.Fatalf("SQL Server 需要连到具体的库: %+v", e.opens)
	}
	if _, err := e.svc.Objects(ctx, bob, 2, "shop", KindEvent); code(err) != "unsupported_kind" {
		t.Fatalf("定时任务应明确暂不支持: %v", err)
	}
	if _, err := e.svc.Detail(ctx, bob, DetailRequest{InstanceID: 2, DB: "shop", Kind: KindTable, Name: "orders"}); code(err) != "bad_request" {
		t.Fatalf("SQL Server 必须带架构名: %v", err)
	}

	e = newEnv()
	e.q.on("SELECT t.object_id FROM sys.tables t", row(int64(1001)))
	e.q.on("FROM sys.objects o LEFT JOIN sys.extended_properties ep ON ep.major_id = o.object_id AND ep.minor_id = 0 AND ep.class = 1 AND ep.name = 'MS_Description' WHERE o.object_id", row("2024-01-01", "2024-02-02", "USER_TABLE", "订单"))
	e.q.on("SUM(p.rows)", row(int64(12345), int64(800), int64(640), int64(7)))
	e.q.on("FROM sys.columns c", row("id", "bigint", int64(8), int64(19), int64(0), true, true, nil, "", "PRI"),
		row("name", "nvarchar", int64(100), int64(0), int64(0), true, false, "('x')", "名称", ""),
		row("amt", "decimal", int64(9), int64(18), int64(2), false, false, nil, "", ""))
	e.q.on("FROM sys.indexes i", row("dbo", "orders", "PK_orders", true, "CLUSTERED", true, "id"), row("dbo", "orders", "ix_n", false, "NONCLUSTERED", false, "name"), row("dbo", "orders", "ix_n", false, "NONCLUSTERED", false, "amt"))
	det, err := e.svc.Detail(ctx, bob, DetailRequest{InstanceID: 2, DB: "shop", Kind: KindTable, Schema: "dbo", Name: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	cols := det.Sections[1].Rows
	if cols[0][1] != "bigint" || cols[0][5] != "identity（自增）" || cols[0][4] != "PRI" || cols[1][1] != "nvarchar(50)" || cols[1][3] != "('x')" || cols[2][1] != "decimal(18,2)" || cols[2][2] != "否" {
		t.Fatalf("列: %+v", cols)
	}
	idx := det.Sections[2].Rows
	if len(idx) != 2 || !strings.Contains(idx[0][3], "主键") || idx[1][1] != "name, amt" {
		t.Fatalf("索引: %+v", idx)
	}
	if det.Sections[3].Title != msDDLTitle {
		t.Fatalf("应有建表语句小节（取不到列定义时降级为说明）: %+v", det.Sections[3])
	}
	// 架构名、对象名只作为参数
	if !strings.Contains(e.q.sqls(), "s.name = @p1 AND t.name = @p2") {
		t.Fatalf("%s", e.q.sqls())
	}
	for _, c := range e.q.calls {
		if strings.Contains(c.sql, "orders") || strings.Contains(c.sql, "'dbo'") {
			t.Fatalf("对象名不能拼进语句: %s", c.sql)
		}
	}
	// 不存在
	e = newEnv()
	if _, err := e.svc.Detail(ctx, bob, DetailRequest{InstanceID: 2, DB: "shop", Kind: KindView, Schema: "dbo", Name: "nope"}); code(err) != "not_found" {
		t.Fatalf("%v", err)
	}
	// 存储过程定义取不到（无权限 / 加密）：降级为说明
	e = newEnv()
	e.q.on("SELECT p.object_id FROM sys.procedures", row(int64(5)))
	e.q.on("FROM sys.objects o LEFT JOIN sys.extended_properties ep", row("2024-01-01", "2024-02-02", "SQL_STORED_PROCEDURE", ""))
	e.q.on("OBJECT_DEFINITION(@p1)", row(nil))
	det, err = e.svc.Detail(ctx, bob, DetailRequest{InstanceID: 2, DB: "shop", Kind: KindProcedure, Schema: "dbo", Name: "p"})
	if err != nil || det.Sections[1].Type != "note" {
		t.Fatalf("%v %+v", err, det)
	}
}

// ---- 导出 ----

// SQL Server 建表语句：按系统目录还原列（含计算列 / 标识 / 默认值 / 排序规则）、主键 / 唯一约束 / 索引、外键、检查约束、触发器。
func TestMSSQLCreateTableDDL(t *testing.T) {
	e := newEnv()
	e.q.on("SELECT t.object_id FROM sys.tables t", row(int64(1001)))
	e.q.on("FROM sys.objects o LEFT JOIN sys.extended_properties ep ON ep.major_id = o.object_id AND ep.minor_id = 0 AND ep.class = 1 AND ep.name = 'MS_Description' WHERE o.object_id", row("2024-01-01", "2024-02-02", "USER_TABLE", ""))
	e.q.on("SUM(p.rows)", row(int64(1), int64(72), int64(16), int64(1)))
	// 建表语句用的列查询（14 列）要排在通用列查询之前：规则按“语句包含的片段”先到先得
	e.q.on("sys.computed_columns", //  name, type, max_length, precision, scale, nullable, identity, collation, computed, persisted, dc name, dc def, seed, inc
		row("BankID", "int", int64(4), int64(10), int64(0), false, true, nil, nil, nil, nil, nil, "1", "1"),
		row("BankName", "nvarchar", int64(100), int64(0), int64(0), true, false, "Chinese_PRC_CI_AS", nil, nil, nil, nil, nil, nil),
		row("Qty", "int", int64(4), int64(10), int64(0), false, false, nil, nil, nil, "DF_orders_Qty", "((0))", nil, nil),
		row("Total", "int", int64(4), int64(10), int64(0), true, false, nil, "([Qty]*(2))", true, nil, nil, nil, nil))
	e.q.on("FROM sys.columns c", row("BankID", "int", int64(4), int64(10), int64(0), false, true, nil, "", "PRI"))
	// 索引：id, name, type, unique, pk, unique constraint, filter, column, desc, included
	e.q.on("FROM sys.indexes i JOIN sys.index_columns", //nolint
		row(int64(1), "PK_Banks", int64(1), true, true, false, "", "BankID", false, false),
		row(int64(2), "ux_name", int64(2), true, false, false, "", "BankName", false, false),
		row(int64(3), "ix_qty", int64(2), false, false, false, "([Qty]>(0))", "Qty", true, false),
		row(int64(3), "ix_qty", int64(2), false, false, false, "([Qty]>(0))", "BankName", false, true),
		row(int64(4), "uq_total", int64(2), true, false, true, "", "Total", false, false))
	e.q.on("FROM sys.indexes i JOIN sys.objects", row("dbo", "orders", "PK_Banks", true, "CLUSTERED", true, "BankID"))
	// 外键：name, ref schema, ref table, on delete, on update, col, ref col, disabled
	e.q.on("FROM sys.foreign_keys fk", row("FK_orders_users", "dbo", "users", "CASCADE", "NO_ACTION", "UserID", "ID", false))
	e.q.on("FROM sys.check_constraints", row("CK_orders_qty", "([Qty]>=(0))", false))
	e.q.on("FROM sys.triggers tr WHERE tr.parent_id", row("trg_orders", "CREATE TRIGGER [dbo].[trg_orders] ON [dbo].[orders] AFTER INSERT AS BEGIN SELECT 1 END", false))

	det, err := e.svc.Detail(ctx, bob, DetailRequest{InstanceID: 2, DB: "shop", Kind: KindTable, Schema: "dbo", Name: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	var sec *Section
	for i := range det.Sections {
		if det.Sections[i].Title == msDDLTitle {
			sec = &det.Sections[i]
		}
	}
	if sec == nil || sec.Type != "code" {
		t.Fatalf("应有建表语句（code）小节: %+v", det.Sections)
	}
	for _, want := range []string{
		"CREATE TABLE [dbo].[orders]\n(",
		"    [BankID] int IDENTITY(1,1) NOT NULL,",
		"    [BankName] nvarchar(50) COLLATE Chinese_PRC_CI_AS NULL,",
		"    [Qty] int NOT NULL CONSTRAINT [DF_orders_Qty] DEFAULT ((0)),",
		"    [Total] AS ([Qty]*(2)) PERSISTED\n)",
		"-- INDEXES",
		"ALTER TABLE [dbo].[orders] ADD CONSTRAINT [PK_Banks] PRIMARY KEY ([BankID]);",
		"CREATE UNIQUE NONCLUSTERED INDEX [ux_name] ON [dbo].[orders] ([BankName]);",
		"CREATE NONCLUSTERED INDEX [ix_qty] ON [dbo].[orders] ([Qty] DESC) INCLUDE ([BankName]) WHERE ([Qty]>(0));",
		"ALTER TABLE [dbo].[orders] ADD CONSTRAINT [uq_total] UNIQUE ([Total]);",
		"-- FOREIGN KEYS",
		"ALTER TABLE [dbo].[orders] ADD CONSTRAINT [FK_orders_users] FOREIGN KEY ([UserID]) REFERENCES [dbo].[users] ([ID]) ON DELETE CASCADE;",
		"-- CHECK CONSTRAINTS",
		"ALTER TABLE [dbo].[orders] ADD CONSTRAINT [CK_orders_qty] CHECK ([Qty]>=(0));",
		"-- TRIGGERS",
		"CREATE TRIGGER [dbo].[trg_orders] ON [dbo].[orders] AFTER INSERT",
	} {
		if !strings.Contains(sec.Text, want) {
			t.Errorf("建表语句缺少 %q\n%s", want, sec.Text)
		}
	}
	if !strings.Contains(sec.Note, "仅供查看参考") {
		t.Errorf("应带“仅供查看参考”的说明: %q", sec.Note)
	}
	// 对象名只作为参数，不拼进语句
	for _, c := range e.q.calls {
		if strings.Contains(c.sql, "orders") || strings.Contains(c.sql, "'dbo'") {
			t.Fatalf("对象名不能拼进语句: %s", c.sql)
		}
	}
}

// 标识符里的 ] 要加倍，避免名字破坏语句结构。
func TestMSQuote(t *testing.T) {
	if got := msQuote("a]b"); got != "[a]]b]" {
		t.Fatalf("%q", got)
	}
}

// SQL Server：主键含自增（identity）列的表在列表里带 AutoIncPK；详情的“表信息”写出主键并标注“（自增）”。
func TestMSSQLAutoIncrementPrimaryKeyMarked(t *testing.T) {
	e := newEnv()
	e.q.on("FROM sys.tables t JOIN sys.schemas s", row("dbo", "orders", ""), row("dbo", "logs", ""), row("dbo", "pair", ""))
	// 估算行数 / 大小 / 分区表标记 / 自增主键列：name, name, rows, bytes, partitioned, identity pk column
	e.q.on("FROM sys.schemas s JOIN sys.tables t", row("dbo", "orders", int64(12), int64(100), int64(0), "OrderID"),
		row("dbo", "logs", int64(5), int64(10), int64(0), nil), row("dbo", "pair", int64(1), int64(1), int64(0), "A"))
	items, err := e.svc.Objects(ctx, bob, 2, "shop", KindTable)
	if err != nil || len(items) != 3 {
		t.Fatalf("%v %+v", err, items)
	}
	got := map[string]string{}
	for _, it := range items {
		got[it.Name] = it.AutoIncPK
	}
	if got["orders"] != "OrderID" || got["logs"] != "" || got["pair"] != "A" {
		t.Fatalf("自增主键列标注: %+v", got)
	}
}

func TestPKText(t *testing.T) {
	cases := []struct {
		name string
		rows [][]any
		want string
	}{
		{"自增单列", [][]any{{"BankID", true}}, "BankID（自增）"},
		{"复合主键只标自增列", [][]any{{"TenantID", false}, {"ID", int64(1)}}, "TenantID, ID（自增）"},
		{"普通主键", [][]any{{"code", false}}, "code"},
		{"没有主键", nil, "无主键"},
		{"行不完整被忽略", [][]any{{"x"}}, "无主键"},
	}
	for _, c := range cases {
		if got := pkText(c.rows); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// Doris / SelectDB / StarRocks 的物化视图：SHOW CREATE TABLE 取不到时改用 SHOW CREATE MATERIALIZED VIEW。
func TestMySQLMaterializedViewCreateStatement(t *testing.T) {
	e := newEnv()
	e.q.on("TABLE_ROWS, AVG_ROW_LENGTH", row("rt_order_submit_10min", "MATERIALIZED_VIEW", "", int64(369), int64(1), int64(1024), int64(0), int64(0), nil, "", nil, nil, ""))
	e.q.fail("SHOW CREATE TABLE", errors.New("Table 'rt_order_submit_10min' is a materialized view"))
	e.q.on("SHOW CREATE MATERIALIZED VIEW", row("rt_order_submit_10min", "CREATE MATERIALIZED VIEW `rt_order_submit_10min` AS SELECT 1"))
	det, err := e.svc.Detail(ctx, root, DetailRequest{InstanceID: 1, DB: "pay_db", Kind: KindTable, Name: "rt_order_submit_10min"})
	if err != nil {
		t.Fatal(err)
	}
	last := det.Sections[len(det.Sections)-1]
	if last.Type != "code" || !strings.Contains(last.Text, "CREATE MATERIALIZED VIEW") || !strings.Contains(last.Title, "物化视图") {
		t.Fatalf("应回退到 SHOW CREATE MATERIALIZED VIEW: %+v", last)
	}
	if !strings.Contains(e.q.sqls(), "SHOW CREATE MATERIALIZED VIEW `pay_db`.`rt_order_submit_10min`") {
		t.Fatalf("%s", e.q.sqls())
	}

	// 普通表：SHOW CREATE TABLE 成功就不去试物化视图
	e = newEnv()
	e.q.on("TABLE_ROWS, AVG_ROW_LENGTH", row("t", "InnoDB", "Dynamic", int64(1), int64(1), int64(1024), int64(0), int64(0), nil, "c", nil, nil, ""))
	e.q.on("SHOW CREATE TABLE", row("t", "CREATE TABLE `t` (id int)"))
	det, err = e.svc.Detail(ctx, root, DetailRequest{InstanceID: 1, DB: "shop", Kind: KindTable, Name: "t"})
	if err != nil {
		t.Fatal(err)
	}
	last = det.Sections[len(det.Sections)-1]
	if last.Title != "建表语句" || strings.Contains(e.q.sqls(), "MATERIALIZED VIEW") {
		t.Fatalf("普通表不应触发物化视图查询: %+v", last)
	}

	// 两种都取不到：说明里带两个原因
	e = newEnv()
	e.q.on("TABLE_ROWS, AVG_ROW_LENGTH", row("t", "InnoDB", "Dynamic", int64(1), int64(1), int64(1024), int64(0), int64(0), nil, "c", nil, nil, ""))
	e.q.fail("SHOW CREATE TABLE", errors.New("denied A"))
	e.q.fail("SHOW CREATE MATERIALIZED VIEW", errors.New("denied B"))
	det, err = e.svc.Detail(ctx, root, DetailRequest{InstanceID: 1, DB: "shop", Kind: KindTable, Name: "t"})
	if err != nil {
		t.Fatal(err)
	}
	last = det.Sections[len(det.Sections)-1]
	if last.Type != "note" || !strings.Contains(last.Text, "denied A") || !strings.Contains(last.Text, "denied B") {
		t.Fatalf("%+v", last)
	}
}
