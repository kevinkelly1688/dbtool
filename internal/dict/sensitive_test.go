// @author: kevinkelly
// @license: Apache Licence
// @file: sensitive_test.go
// @time: 2026/10/28

package dict

import (
	"errors"
	"strings"
	"testing"

	"dbtool/internal/masking"
	"dbtool/internal/model"
)

// fakeMask 是假的脱敏配置来源，按实例 ID 返回预设的激活字段，并记录被查询的实例。
type fakeMask struct {
	cols  map[uint][]model.DataMaskingColumn
	err   error
	calls []uint
}

func (f *fakeMask) ActiveMaskingColumns(id uint) ([]model.DataMaskingColumn, error) {
	f.calls = append(f.calls, id)
	return f.cols[id], f.err
}

func mcol(schema, table, col string) model.DataMaskingColumn {
	return model.DataMaskingColumn{TableSchema: schema, TableName_: table, ColumnName: col, RuleType: 100, Active: true}
}

func TestSensitiveHas(t *testing.T) {
	ss := &sensitiveSet{db: "shop", cols: []masking.Column{
		{Schema: "shop", Table: "users", Column: "Phone"},
		{Schema: "*", Table: "*", Column: "id_card"},
		{Schema: "dbo", Table: "emp", Column: "ssn"},
	}}
	cases := []struct {
		schema, table, col string
		want               bool
	}{
		{"", "users", "phone", true},      // 不区分大小写
		{"", "USERS", "PHONE", true},      // 表名也不区分大小写
		{"", "orders", "phone", false},    // 表不同
		{"", "anything", "ID_CARD", true}, // 通配
		{"dbo", "emp", "ssn", true},       // SQL Server 的配置写成架构名
		{"", "emp", "ssn", false},         // MySQL 没有架构名，库名是 shop，不匹配 dbo
		{"", "users", "name", false},
	}
	for _, c := range cases {
		if got := ss.Has(c.schema, c.table, c.col); got != c.want {
			t.Errorf("Has(%q,%q,%q)=%v 期望 %v", c.schema, c.table, c.col, got, c.want)
		}
	}
	var nilSet *sensitiveSet
	if nilSet.Has("", "t", "c") || nilSet.names() != nil {
		t.Error("nil 规则集应安全地表示没有规则")
	}
}

func TestMySQLListSizeRowsPartitionedAndSensitive(t *testing.T) {
	e := newEnv()
	fm := &fakeMask{cols: map[uint][]model.DataMaskingColumn{
		1: {mcol("shop", "users", "phone"), mcol("shop", "users", "id_card"), mcol("other_db", "orders", "amount"), mcol("*", "*", "email")},
	}}
	e.svc.WithMasking(fm)
	e.q.on("SELECT TABLE_NAME, IFNULL(TABLE_COMMENT",
		row("orders", "订单", int64(1000), int64(4096), int64(1024), "partitioned"),
		row("users", "用户", int64(50), int64(2048), int64(0), ""),
		row("logs", "", nil, nil, nil, ""))
	e.q.on("FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND COLUMN_NAME IN",
		row("users", "phone"), row("users", "email"), row("orders", "email"), row("orders", "amount"), row("users", "name"))
	items, err := e.svc.Objects(ctx, root, 1, "shop", KindTable)
	if err != nil || len(items) != 3 {
		t.Fatalf("%v %+v", err, items)
	}
	by := map[string]Item{}
	for _, it := range items {
		by[it.Name] = it
	}
	o := by["orders"]
	if o.RowsEst == nil || *o.RowsEst != 1000 || o.SizeBytes == nil || *o.SizeBytes != 5120 || !o.Partitioned {
		t.Errorf("orders 估算行数/大小/分区表: %+v", o)
	}
	// orders.amount 配在别的库（other_db）上，不算；通配的 email 算 1 个
	if o.Sensitive != 1 {
		t.Errorf("orders 敏感列数: %d", o.Sensitive)
	}
	u := by["users"]
	if u.Partitioned || u.Sensitive != 2 || u.RowsEst == nil || *u.RowsEst != 50 { // phone、通配的 email
		t.Errorf("users: %+v", u)
	}
	l := by["logs"]
	if l.RowsEst != nil || l.SizeBytes != nil || l.Partitioned || l.Sensitive != 0 {
		t.Errorf("取不到的信息应留空: %+v", l)
	}
	// 按列名查找只带库内相关规则的列名（去重、小写），库名与列名都走占位符
	var found bool
	for _, c := range e.q.calls {
		if strings.Contains(c.sql, "COLUMN_NAME IN") {
			found = true
			if strings.Contains(c.sql, "phone") || len(c.args) != 4 || c.args[0] != "shop" { // shop + phone/id_card/email
				t.Errorf("列名应参数化且只含与本库相关的规则: %+v", c)
			}
		}
	}
	if !found {
		t.Error("应按列名查询敏感列")
	}
	if len(fm.calls) != 1 || fm.calls[0] != 1 {
		t.Errorf("应只读取本实例的脱敏配置: %v", fm.calls)
	}
}

func TestSensitiveDegradesGracefully(t *testing.T) {
	// 规则读取失败：不报错、不标敏感
	e := newEnv()
	e.svc.WithMasking(&fakeMask{err: errors.New("db down")})
	e.q.on("SELECT TABLE_NAME, IFNULL(TABLE_COMMENT", row("users", "用户", int64(1), int64(1), int64(1), ""))
	items, err := e.svc.Objects(ctx, root, 1, "shop", KindTable)
	if err != nil || len(items) != 1 || items[0].Sensitive != 0 {
		t.Fatalf("读取失败应降级: %v %+v", err, items)
	}
	// 没有配置规则：不额外查列
	e = newEnv()
	e.svc.WithMasking(&fakeMask{})
	e.q.on("SELECT TABLE_NAME, IFNULL(TABLE_COMMENT", row("users", "用户", int64(1), int64(1), int64(1), ""))
	if _, err := e.svc.Objects(ctx, root, 1, "shop", KindTable); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.q.sqls(), "COLUMN_NAME IN") {
		t.Error("没有脱敏规则时不应查列")
	}
	// 查列失败：对象列表照常返回
	e = newEnv()
	e.svc.WithMasking(&fakeMask{cols: map[uint][]model.DataMaskingColumn{1: {mcol("shop", "users", "phone")}}})
	e.q.on("SELECT TABLE_NAME, IFNULL(TABLE_COMMENT", row("users", "用户", int64(1), int64(1), int64(1), ""))
	e.q.fail("COLUMN_NAME IN", errors.New("denied"))
	items, err = e.svc.Objects(ctx, root, 1, "shop", KindTable)
	if err != nil || len(items) != 1 || items[0].Sensitive != 0 {
		t.Fatalf("查列失败应降级: %v %+v", err, items)
	}
	// 老格式（只有名称与注释两列）的结果也能解析
	e = newEnv()
	e.q.on("SELECT TABLE_NAME, IFNULL(TABLE_COMMENT", row("a", "b"))
	items, err = e.svc.Objects(ctx, root, 1, "shop", KindTable)
	if err != nil || items[0].RowsEst != nil {
		t.Fatalf("%v %+v", err, items)
	}
}

func TestDetailMarksSensitiveColumns(t *testing.T) {
	e := newEnv()
	e.svc.WithMasking(&fakeMask{cols: map[uint][]model.DataMaskingColumn{1: {mcol("shop", "orders", "phone")}}})
	e.q.on("TABLE_ROWS, AVG_ROW_LENGTH", row("orders", "InnoDB", "Dynamic", int64(1), int64(1), int64(1024), int64(0), int64(0), nil, "c", nil, nil, "", "partitioned"))
	e.q.on("FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?",
		row("id", "bigint", "NO", nil, "PRI", "", ""), row("Phone", "varchar(20)", "YES", nil, "", "", "手机号"))
	det, err := e.svc.Detail(ctx, root, DetailRequest{InstanceID: 1, DB: "shop", Kind: KindTable, Name: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if len(det.SensitiveColumns) != 1 || det.SensitiveColumns[0] != "Phone" {
		t.Fatalf("%+v", det.SensitiveColumns)
	}
	var cols *Section
	var info *Section
	for i := range det.Sections {
		switch det.Sections[i].Title {
		case "列信息":
			cols = &det.Sections[i]
		case "表信息":
			info = &det.Sections[i]
		}
	}
	if cols == nil || len(cols.Columns) != 8 || cols.Columns[7] != sensitiveHead {
		t.Fatalf("列信息应追加“脱敏”列: %+v", cols)
	}
	if cols.Rows[0][7] != "" || cols.Rows[1][7] != sensitiveMark || len(cols.Rows[1]) != 8 {
		t.Errorf("%+v", cols.Rows)
	}
	if len(columnHeads) != 7 {
		t.Fatalf("包级 columnHeads 不能被追加污染: %v", columnHeads)
	}
	var part string
	for _, p := range info.Pairs {
		if p.Key == "分区表" {
			part = p.Value
		}
	}
	if part != "是" {
		t.Errorf("详情应显示分区表: %+v", info.Pairs)
	}

	// 没有规则：列信息保持 7 列，不带 sensitive_columns
	e = newEnv()
	e.q.on("TABLE_ROWS, AVG_ROW_LENGTH", row("orders", "InnoDB", "Dynamic", int64(1), int64(1), int64(1024), int64(0), int64(0), nil, "c", nil, nil, "", ""))
	e.q.on("FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", row("id", "bigint", "NO", nil, "PRI", "", ""))
	det, err = e.svc.Detail(ctx, root, DetailRequest{InstanceID: 1, DB: "shop", Kind: KindTable, Name: "orders"})
	if err != nil || len(det.SensitiveColumns) != 0 {
		t.Fatalf("%v %+v", err, det)
	}
	for _, s := range det.Sections {
		if s.Title == "列信息" && len(s.Columns) != 7 {
			t.Errorf("无规则时列信息保持原样: %v", s.Columns)
		}
	}
}

func TestMSSQLListEnrichAndSensitive(t *testing.T) {
	e := newEnv()
	e.svc.WithMasking(&fakeMask{cols: map[uint][]model.DataMaskingColumn{2: {mcol("dbo", "orders", "phone")}}})
	e.q.on("FROM sys.tables t JOIN sys.schemas s", row("dbo", "orders", "订单"), row("sales", "orders", ""))
	e.q.on("FROM sys.dm_db_partition_stats", row("dbo", "orders", int64(500), int64(8192), int64(1)), row("sales", "orders", nil, nil, int64(0)))
	e.q.on("FROM sys.columns c JOIN sys.objects o", row("dbo", "orders", "phone"), row("sales", "orders", "phone"))
	items, err := e.svc.Objects(ctx, bob, 2, "shop", KindTable)
	if err != nil || len(items) != 2 {
		t.Fatalf("%v %+v", err, items)
	}
	a, b := items[0], items[1]
	if a.RowsEst == nil || *a.RowsEst != 500 || a.SizeBytes == nil || *a.SizeBytes != 8192 || !a.Partitioned || a.Sensitive != 1 {
		t.Errorf("dbo.orders: %+v", a)
	}
	if b.RowsEst != nil || b.Partitioned || b.Sensitive != 0 {
		t.Errorf("sales.orders 的规则写的是 dbo，不应命中: %+v", b)
	}
	// 统计查询失败（缺 VIEW DATABASE STATE 权限）：列表照常
	e = newEnv()
	e.q.on("FROM sys.tables t JOIN sys.schemas s", row("dbo", "orders", "订单"))
	e.q.fail("FROM sys.dm_db_partition_stats", errors.New("VIEW DATABASE STATE permission denied"))
	items, err = e.svc.Objects(ctx, bob, 2, "shop", KindTable)
	if err != nil || len(items) != 1 || items[0].RowsEst != nil {
		t.Fatalf("%v %+v", err, items)
	}
}
