// @author: kevinkelly
// @license: Apache Licence
// @file: objectinfo_test.go
// @time: 2026/10/28

package dict

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 在线查询工作台的“对象信息”与“可编程对象”：有 query.objinfo 按钮 + 能访问实例即可。

func TestHTTPObjectInfoForQueryWorkbench(t *testing.T) {
	h := newHTTPEnv(t)
	const base = "/api/v1/query/instances/1/databases/shop/object-info"
	if w := h.web.Anonymous("GET", base+"?name=orders"); w.Code != 401 {
		t.Errorf("未登录应 401: %d", w.Code)
	}
	bob := h.web.Login("bob") // 普通用户，只有对象信息按钮
	h.grant(2, btnObjInfo)
	h.q.on("TABLE_ROWS, AVG_ROW_LENGTH", row("orders", "InnoDB", "Dynamic", int64(1376), int64(182), int64(360448), int64(81920), int64(0), uint64(1377), "utf8mb4_0900_ai_ci", "2024-03-12 10:00:00", nil, "订单主表"))
	h.q.on("FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", row("id", "bigint", "NO", nil, "PRI", "auto_increment", "主键"))
	h.q.on("FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", row("orders", "PRIMARY", int64(0), "BTREE", "id"))
	h.q.on("SHOW CREATE TABLE", row("orders", "CREATE TABLE `orders` (\n  `id` bigint NOT NULL\n) ENGINE=InnoDB"))
	code, r := get(bob, base+"?name=orders")
	if code != 200 {
		t.Fatalf("登录即可看对象信息: %d %+v", code, r)
	}
	var det Detail
	_ = json.Unmarshal(r.Data, &det)
	if got := sectionTitles(det); got != "表信息,列信息,索引信息,建表语句" {
		t.Fatalf("应有基本信息、列信息、索引信息、建表语句: %s", got)
	}
	if last := det.Sections[3]; last.Type != "code" || !strings.Contains(last.Text, "CREATE TABLE `orders`") {
		t.Fatalf("建表语句: %+v", last)
	}
	if idx := det.Sections[2]; len(idx.Rows) != 1 || idx.Rows[0][0] != "PRIMARY" {
		t.Fatalf("索引信息: %+v", idx)
	}
	pairs := map[string]string{}
	for _, p := range det.Sections[0].Pairs {
		pairs[p.Key] = p.Value
	}
	if pairs["表行数（估算）"] != "1376" || pairs["表说明"] != "订单主表" || pairs["引擎"] != "InnoDB" {
		t.Errorf("基本信息: %+v", pairs)
	}
	// 原来独立的数据字典页面和接口已经去掉
	if code, _ := get(bob, "/api/v1/dict/object?instance_id=1&db=shop&kind=table&name=orders"); code != 404 {
		t.Errorf("数据字典接口应已不存在: %d", code)
	}
	if code, _ := bob.Page("/data_dictionary"); code != 404 {
		t.Errorf("数据字典页面应已不存在: %d", code)
	}
	// 错误：对象类别、系统库、未关联实例、实例 id、对象不存在
	if code, r = get(bob, base+"?name=orders&kind=bogus"); code != 400 {
		t.Errorf("未知类别: %d %+v", code, r)
	}
	if code, r = get(bob, "/api/v1/query/instances/1/databases/mysql/object-info?name=user"); code != 403 || r.Code != "db_not_allowed" {
		t.Errorf("系统库: %d %+v", code, r)
	}
	if code, r = get(bob, "/api/v1/query/instances/6/databases/shop/object-info?name=orders"); code != 403 || r.Code != "no_privilege" {
		t.Errorf("未关联实例: %d %+v", code, r)
	}
	if code, r = get(bob, "/api/v1/query/instances/0/databases/shop/object-info?name=orders"); code != 400 {
		t.Errorf("实例 id 非法: %d %+v", code, r)
	}
	h.q.rules = nil
	if code, r = get(bob, base+"?name=nope"); code != 404 {
		t.Errorf("对象不存在: %d %+v", code, r)
	}
}

func TestHTTPObjectInfoRoutine(t *testing.T) {
	h := newHTTPEnv(t)
	bob := h.web.Login("bob")
	h.grant(2, btnObjInfo)
	// SQL Server 的函数：按 架构 + 名称 定位，定义取自 OBJECT_DEFINITION，列信息 / 索引信息不返回
	h.q.on("WHERE o.type IN ('FN','TF','IF','FS','FT') AND s.name = @p1", row(int64(1234)))
	h.q.on("FROM sys.objects o", row("2024-01-01", "2024-02-01", "SQL_SCALAR_FUNCTION", ""))
	h.q.on("SELECT OBJECT_DEFINITION(@p1)", row("ALTER FUNCTION [dbo].[GetMonthOfDays] (@month VARCHAR(20)) RETURNS INT AS BEGIN RETURN 1 END"))
	code, r := get(bob, "/api/v1/query/instances/2/databases/shopdb/object-info?kind=function&schema=dbo&name=GetMonthOfDays")
	if code != 200 {
		t.Fatalf("函数对象信息: %d %+v", code, r)
	}
	var det Detail
	_ = json.Unmarshal(r.Data, &det)
	if len(det.Sections) != 2 || det.Sections[0].Type != "kv" || det.Sections[1].Type != "code" || !strings.Contains(det.Sections[1].Text, "ALTER FUNCTION [dbo].[GetMonthOfDays]") {
		t.Fatalf("应有函数信息与定义: %+v", det.Sections)
	}
	// SQL Server 的对象必须给出架构
	if code, r = get(bob, "/api/v1/query/instances/2/databases/shopdb/object-info?kind=function&name=GetMonthOfDays"); code != 400 {
		t.Errorf("缺少架构: %d %+v", code, r)
	}
}

func TestProgrammablesListsSupportedKinds(t *testing.T) {
	e := newEnv()
	e.q.on("FROM information_schema.VIEWS", row("v_orders", ""))
	e.q.on("ROUTINE_TYPE = 'FUNCTION'", row("fn_a", "函数 a"), row("fn_b", ""))
	e.q.on("ROUTINE_TYPE = 'PROCEDURE'", row("sp_a", ""))
	e.q.fail("FROM information_schema.TRIGGERS", errors.New("denied"))
	groups, err := e.svc.Programmables(ctx, bob, 1, "shop")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	counts := map[Kind]int{}
	for _, g := range groups {
		kinds = append(kinds, string(g.Kind))
		counts[g.Kind] = len(g.Items)
	}
	if strings.Join(kinds, ",") != "view,function,procedure,trigger,event" {
		t.Fatalf("类别与顺序: %v", kinds)
	}
	if counts[KindView] != 1 || counts[KindFunction] != 2 || counts[KindProcedure] != 1 || counts[KindTrigger] != 0 {
		t.Errorf("数量: %v", counts)
	}
	for _, g := range groups {
		switch g.Kind {
		case KindTrigger:
			if g.Error == "" {
				t.Error("触发器列表取失败应在该类里写原因")
			}
		default:
			if g.Error != "" {
				t.Errorf("%s 不应有错误: %s", g.Kind, g.Error)
			}
		}
	}
	if len(e.opens) != 1 {
		t.Errorf("一次请求只连一次目标库: %d", len(e.opens))
	}
	// 系统库、未关联实例
	if _, err := e.svc.Programmables(ctx, bob, 1, "mysql"); err == nil {
		t.Error("系统库应被拒绝")
	}
	if _, err := e.svc.Programmables(ctx, bob, 6, "shop"); err == nil {
		t.Error("未关联实例应被拒绝")
	}
	// 只支持表的 MySQL 变体 / 不支持可编程对象的引擎：返回空列表，不连库
	e2 := newEnv()
	g2, err := e2.svc.Programmables(ctx, bob, 7, "shop") // doris
	if err != nil || len(g2) != 0 || len(e2.opens) != 0 {
		t.Errorf("Doris: %+v %v opens=%d", g2, err, len(e2.opens))
	}
}

func TestHTTPProgrammables(t *testing.T) {
	h := newHTTPEnv(t)
	const p = "/api/v1/query/instances/1/databases/shop/programmables"
	if w := h.web.Anonymous("GET", p); w.Code != 401 {
		t.Errorf("未登录应 401: %d", w.Code)
	}
	bob := h.web.Login("bob")
	h.grant(2, btnObjInfo)
	h.q.on("ROUTINE_TYPE = 'PROCEDURE'", row("sp_a", ""))
	code, r := get(bob, p)
	if code != 200 {
		t.Fatalf("%d %+v", code, r)
	}
	var out struct {
		Groups []ProgrammableGroup `json:"groups"`
	}
	_ = json.Unmarshal(r.Data, &out)
	if len(out.Groups) != 5 || out.Groups[2].Kind != KindProcedure || len(out.Groups[2].Items) != 1 || out.Groups[2].Items[0].Name != "sp_a" {
		t.Errorf("%+v", out.Groups)
	}
	if code, r = get(bob, "/api/v1/query/instances/1/databases/mysql/programmables"); code != 403 {
		t.Errorf("系统库: %d %+v", code, r)
	}
}

func sectionTitles(d Detail) string {
	var out []string
	for _, s := range d.Sections {
		out = append(out, s.Title)
	}
	return strings.Join(out, ",")
}

// SQL Server 的表：对象信息要有基本信息、列信息（标注自增）、索引信息和创建表语句；创建语句里字段带 IDENTITY(种子,步长)，索引以语句形式列出。
func TestHTTPObjectInfoSQLServerTable(t *testing.T) {
	h := newHTTPEnv(t)
	bob := h.web.Login("bob")
	h.grant(2, btnObjInfo)
	q := h.q
	q.on("SELECT t.object_id FROM sys.tables t", row(int64(1001)))
	q.on("FROM sys.objects o LEFT JOIN sys.extended_properties ep ON ep.major_id = o.object_id AND ep.minor_id = 0 AND ep.class = 1 AND ep.name = 'MS_Description' WHERE o.object_id", row("2024-01-01", "2024-02-02", "USER_TABLE", "订单"))
	q.on("SUM(p.rows)", row(int64(1376), int64(432), int64(400), int64(1377)))
	q.on("i.is_primary_key = 1 AND ic.is_included_column = 0", row("OrderID", true)) // 主键列、是否自增
	// 建表语句用的列查询（14 列）排在通用列查询之前：规则按“语句包含的片段”先到先得
	q.on("sys.computed_columns",
		row("OrderID", "bigint", int64(8), int64(19), int64(0), false, true, nil, nil, nil, nil, nil, "100", "5"),
		row("Remark", "nvarchar", int64(200), int64(0), int64(0), true, false, "Chinese_PRC_CI_AS", nil, nil, nil, nil, nil, nil))
	q.on("FROM sys.columns c",
		row("OrderID", "bigint", int64(8), int64(19), int64(0), false, true, nil, "主键", "PRI"),
		row("Remark", "nvarchar", int64(200), int64(0), int64(0), true, false, nil, "", ""))
	q.on("FROM sys.indexes i JOIN sys.index_columns", // id, name, type, unique, pk, unique constraint, filter, column, desc, included
		row(int64(1), "PK_Orders", int64(1), true, true, false, "", "OrderID", false, false),
		row(int64(2), "ix_remark", int64(2), false, false, false, "", "Remark", false, false))
	q.on("FROM sys.indexes i JOIN sys.objects", row("dbo", "Orders", "PK_Orders", true, "CLUSTERED", true, "OrderID"), row("dbo", "Orders", "ix_remark", false, "NONCLUSTERED", false, "Remark"))

	code, r := get(bob, "/api/v1/query/instances/2/databases/shopdb/object-info?kind=table&schema=dbo&name=Orders")
	if code != 200 {
		t.Fatalf("%d %+v", code, r)
	}
	var det Detail
	_ = json.Unmarshal(r.Data, &det)
	if got := sectionTitles(det); got != "表信息,列信息,索引信息,"+msDDLTitle {
		t.Fatalf("SQL Server 表的对象信息应有表信息、列信息、索引信息、建表语句: %s", got)
	}
	// 基本信息：主键标注自增、当前自增值
	pairs := map[string]string{}
	for _, p := range det.Sections[0].Pairs {
		pairs[p.Key] = p.Value
	}
	if pairs["主键"] != "OrderID（自增）" || pairs["当前自增值"] != "1377" {
		t.Errorf("主键: %+v", pairs)
	}
	// 列信息：自增列在“额外”里标注
	cols := det.Sections[1]
	if len(cols.Rows) != 2 || cols.Rows[0][5] != "identity（自增）" || cols.Rows[1][5] != "" {
		t.Errorf("列信息应标注自增: %+v", cols.Rows)
	}
	// 索引信息：主键 + 普通索引
	idx := det.Sections[2]
	if len(idx.Rows) != 2 || idx.Rows[0][0] != "PK_Orders" || !strings.Contains(idx.Rows[0][3], "主键") || idx.Rows[1][0] != "ix_remark" {
		t.Errorf("索引信息: %+v", idx.Rows)
	}
	// 创建语句：字段带 IDENTITY(种子,步长)，主键与索引以语句形式列出
	ddl := det.Sections[3]
	for _, want := range []string{
		"CREATE TABLE [dbo].[Orders]",
		"[OrderID] bigint IDENTITY(100,5) NOT NULL",
		"ALTER TABLE [dbo].[Orders] ADD CONSTRAINT [PK_Orders] PRIMARY KEY ([OrderID]);",
		"CREATE NONCLUSTERED INDEX [ix_remark] ON [dbo].[Orders] ([Remark]);",
	} {
		if ddl.Type != "code" || !strings.Contains(ddl.Text, want) {
			t.Errorf("创建语句缺少 %q\n%s", want, ddl.Text)
		}
	}
}
