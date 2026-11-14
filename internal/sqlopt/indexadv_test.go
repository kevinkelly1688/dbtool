// @author: kevinkelly
// @license: Apache Licence
// @file: indexadv_test.go
// @time: 2026/10/28

package sqlopt

import (
	"context"
	"strings"
	"testing"
)

func ordersMeta() *tableMeta {
	return &tableMeta{Schema: "shop", Name: "orders", Rows: 8000000,
		Cols: []colMeta{
			{"id", "bigint", "bigint(20)"}, {"user_id", "bigint", "bigint(20)"}, {"status", "varchar", "varchar(16)"},
			{"created_at", "datetime", "datetime"}, {"phone", "varchar", "varchar(20)"}, {"note", "text", "text"},
		},
		Indexes: []idxMeta{{Name: "PRIMARY", Unique: true, Cols: []string{"id"}}, {Name: "idx_user", Cols: []string{"user_id"}}}}
}

func metaFor(sql string, m *tableMeta) (*pstmt, *metaSet) {
	p := parseStmt(sql)
	refs := p.tableRefs()
	ms := &metaSet{refs: refs, metas: make([]*tableMeta, len(refs))}
	for i := range refs {
		ms.metas[i] = m
	}
	return p, ms
}

func TestIndexAdvice(t *testing.T) {
	cases := []struct {
		name, sql string
		meta      *tableMeta
		want      string // 期望的 ADD INDEX 列清单（空表示不建议）
	}{
		{"等值+范围", "select * from orders where status = 'paid' and created_at >= '2026-01-01'", nil, "`status`, `created_at`"},
		{"等值+排序+limit", "select * from orders where status = 'paid' order by created_at desc limit 20", nil, "`status`, `created_at`"},
		{"仅排序+limit", "select id from orders order by created_at limit 10", nil, "`created_at`"},
		{"已有索引", "select * from orders where user_id = 5", nil, ""},
		{"主键等值", "select * from orders where id = 5", nil, ""},
		{"主键in", "select * from orders where id in (1,2,3)", nil, ""},
		{"多列等值顺序无关", "select * from orders where status = 'a' and phone = '1'", nil, "`status`, `phone`"},
		{"含OR不建议", "select * from orders where status = 'a' or phone = '1'", nil, ""},
		{"函数条件不当作列", "select * from orders where date(created_at) = '2026-01-01' and status = 'a'", nil, "`status`"},
		{"between", "select * from orders where status = 'a' and created_at between '2026-01-01' and '2026-02-01'", nil, "`status`, `created_at`"},
		{"前缀like", "select * from orders where phone like '138%'", nil, "`phone`"},
		{"前导通配like不算", "select * from orders where phone like '%138'", nil, ""},
		{"不等于不算", "select * from orders where status != 'a'", nil, ""},
		{"text列不能建索引", "select * from orders where note = 'x'", nil, ""},
		{"update", "update orders set note = 'x' where status = 'a' and phone = '1'", nil, "`status`, `phone`"},
		{"delete", "delete from orders where created_at < '2020-01-01'", nil, "`created_at`"},
		{"别名限定", "select o.id from orders o where o.status = 'a'", nil, "`status`"},
		{"列比较列不算", "select * from orders where status = phone", nil, ""},
		{"无where无order", "select * from orders", nil, ""},
		{"联合索引已覆盖", "select * from orders where status = 'a' and created_at > '2026'", &tableMeta{Schema: "shop", Name: "orders", Rows: 100000,
			Cols: ordersMeta().Cols, Indexes: []idxMeta{{Name: "idx_sc", Cols: []string{"status", "created_at"}}}}, ""},
		{"联合索引前缀够用", "select * from orders where status = 'a'", &tableMeta{Schema: "shop", Name: "orders", Rows: 100000,
			Cols: ordersMeta().Cols, Indexes: []idxMeta{{Name: "idx_sc", Cols: []string{"status", "created_at"}}}}, ""},
		{"联合索引顺序不对", "select * from orders where status = 'a' and phone = '1'", &tableMeta{Schema: "shop", Name: "orders", Rows: 100000,
			Cols: ordersMeta().Cols, Indexes: []idxMeta{{Name: "idx_sp", Cols: []string{"phone"}}}}, "`status`, `phone`"},
	}
	for _, c := range cases {
		m := c.meta
		if m == nil {
			m = ordersMeta()
		}
		p, ms := metaFor(c.sql, m)
		fs, ddl := indexAdvice(p, ms)
		if c.want == "" {
			if len(fs) != 0 {
				t.Errorf("%s：不应有索引建议，实际 %v", c.name, ddl)
			}
			continue
		}
		if len(fs) != 1 || len(ddl) != 1 || !strings.Contains(ddl[0], "ADD INDEX") || !strings.Contains(ddl[0], "("+c.want+")") {
			t.Errorf("%s：期望列 (%s)，实际 %v", c.name, c.want, ddl)
		}
	}
}

func TestIndexAdviceLevelAndName(t *testing.T) {
	p, ms := metaFor("select * from orders where status = 'a'", ordersMeta())
	fs, ddl := indexAdvice(p, ms)
	if len(fs) != 1 || fs[0].Level != LevelHigh || fs[0].ID != "S002" {
		t.Fatalf("大表应是高级别：%+v", fs)
	}
	if want := "ALTER TABLE `shop`.`orders` ADD INDEX `idx_status` (`status`);"; ddl[0] != want {
		t.Fatalf("ddl=%s", ddl[0])
	}
	small := ordersMeta()
	small.Rows = 50
	p, ms = metaFor("select * from orders where status = 'a'", small)
	fs, _ = indexAdvice(p, ms)
	if len(fs) != 1 || fs[0].Level != LevelLow {
		t.Fatalf("小表应降为低级别：%+v", fs)
	}
	// 多表查询不给索引建议
	p = parseStmt("select * from orders o join users u on u.id = o.user_id where o.status = 'a'")
	refs := p.tableRefs()
	ms = &metaSet{refs: refs, metas: []*tableMeta{ordersMeta(), ordersMeta()}}
	if fs, _ := indexAdvice(p, ms); len(fs) != 0 {
		t.Fatalf("多表不应给索引建议：%+v", fs)
	}
	// 表不存在
	ms = &metaSet{refs: []tableRef{{Name: "x"}}, metas: []*tableMeta{nil}}
	if fs, _ := indexAdvice(parseStmt("select * from x where a = 1"), ms); len(fs) != 0 {
		t.Fatal("没有元数据不应给建议")
	}
}

func TestImplicitConversion(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
	}{
		{"select * from orders where phone = 13800000000", true},
		{"select * from orders where status in (1, 2)", true},
		{"select * from orders where phone = '13800000000'", false},
		{"select * from orders where user_id = 5", false},
		{"select * from orders o where o.phone > 5", true},
		{"select * from orders where phone = 1 and status = 2", true},
		{"select phone from orders where id = 5", false},
	}
	for _, c := range cases {
		p, ms := metaFor(c.sql, ordersMeta())
		got := implicitConversion(p, ms)
		if (len(got) > 0) != c.want {
			t.Errorf("%q：命中=%v 期望 %v", c.sql, len(got) > 0, c.want)
		}
		for _, f := range got {
			if f.ID != "S001" || f.Level != LevelMedium {
				t.Errorf("%+v", f)
			}
		}
	}
	// 多列只对各列分别报告
	p, ms := metaFor("select * from orders where phone = 1 and status = 2", ordersMeta())
	if n := len(implicitConversion(p, ms)); n != 2 {
		t.Errorf("两个字符串列各报一次：%d", n)
	}
}

func TestLoadTableMeta(t *testing.T) {
	db := newFakeDB()
	db.tables["orders"] = fakeTable{
		cols:    [][3]string{{"id", "bigint", "bigint(20)"}, {"status", "varchar", "varchar(16)"}},
		indexes: [][4]string{{"PRIMARY", "0", "1", "id"}, {"idx_s", "1", "1", "status"}, {"idx_s", "1", "2", "id"}},
		rows:    "1234",
	}
	sess, _ := db.Open(context.Background(), engineConn(), "")
	m, err := loadTableMeta(context.Background(), sess, "shop", "Orders")
	if err != nil || m == nil {
		t.Fatalf("%v %v", m, err)
	}
	if len(m.Cols) != 2 || m.Rows != 1234 || len(m.Indexes) != 2 || !m.Indexes[0].Unique || m.Indexes[1].Unique || len(m.Indexes[1].Cols) != 2 {
		t.Fatalf("%+v", m)
	}
	if m, err := loadTableMeta(context.Background(), sess, "shop", "nope"); err != nil || m != nil {
		t.Fatalf("不存在的表应返回 nil：%v %v", m, err)
	}
}
