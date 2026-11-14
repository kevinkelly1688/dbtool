// @author: kevinkelly
// @license: Apache Licence
// @file: sqlgen_test.go
// @time: 2026/10/28

package engine

import (
	"strings"
	"testing"
	"time"
)

func TestSQLBuilders(t *testing.T) {
	where := "`created_at` < NOW() - INTERVAL 90 DAY"
	if got := buildReadSQL("shop", "orders", where, []string{"id"}, false, false); got !=
		"SELECT * FROM `shop`.`orders` WHERE (`created_at` < NOW() - INTERVAL 90 DAY) ORDER BY `id` LIMIT ?" {
		t.Errorf("read: %s", got)
	}
	if got := buildReadSQL("shop", "orders", where, []string{"id"}, true, false); got !=
		"SELECT * FROM `shop`.`orders` WHERE (`created_at` < NOW() - INTERVAL 90 DAY) AND `id` > ? ORDER BY `id` LIMIT ?" {
		t.Errorf("read after: %s", got)
	}
	if got := buildReadSQL("s", "t", "1=1", []string{"a", "b"}, true, false); got !=
		"SELECT * FROM `s`.`t` WHERE (1=1) AND (`a`, `b`) > (?, ?) ORDER BY `a`, `b` LIMIT ?" {
		t.Errorf("read tuple: %s", got)
	}
	// 不排序读取（归档并删除 / 仅清理）：没有 ORDER BY，没有断点参数，只有 LIMIT
	if got := buildReadSQL("shop", "orders", where, []string{"id"}, false, true); got !=
		"SELECT * FROM `shop`.`orders` WHERE (`created_at` < NOW() - INTERVAL 90 DAY) LIMIT ?" {
		t.Errorf("read unordered: %s", got)
	}
	if got := buildReadSQL("s", "t", "1=1", []string{"a", "b"}, true, true); strings.Contains(got, "ORDER BY") || strings.Contains(got, "> (") {
		t.Errorf("不排序读取即使带了断点也不应排序 / 翻页: %s", got)
	}
	if got := buildDeleteSQL("shop", "orders", where, []string{"id"}, 3); got !=
		"DELETE FROM `shop`.`orders` WHERE `id` IN (?, ?, ?) AND (`created_at` < NOW() - INTERVAL 90 DAY)" {
		t.Errorf("delete: %s", got)
	}
	if got := buildDeleteSQL("s", "t", "w", []string{"a", "b"}, 2); got !=
		"DELETE FROM `s`.`t` WHERE (`a`, `b`) IN ((?, ?), (?, ?)) AND (w)" {
		t.Errorf("delete tuple: %s", got)
	}
	if got := buildInsertSQL("INSERT", "arch", "o", []string{"id", "x"}, 2); got !=
		"INSERT INTO `arch`.`o` (`id`, `x`) VALUES (?, ?), (?, ?)" {
		t.Errorf("insert: %s", got)
	}
	if got := buildInsertSQL("INSERT IGNORE", "a", "o", []string{"id"}, 1); got != "INSERT IGNORE INTO `a`.`o` (`id`) VALUES (?)" {
		t.Errorf("insert ignore: %s", got)
	}
	if got := buildSelectByPK("a", "o", []string{"id", "x"}, []string{"id"}, 2); got != "SELECT `id`, `x` FROM `a`.`o` WHERE `id` IN (?, ?)" {
		t.Errorf("select by pk: %s", got)
	}
	if got := buildCountByPK("a", "o", []string{"id"}, 2); got != "SELECT COUNT(*) FROM `a`.`o` WHERE `id` IN (?, ?)" {
		t.Errorf("count by pk: %s", got)
	}
	if got := buildRangeSQL("a", "o", []string{"id"}, true); got !=
		"SELECT * FROM `a`.`o` WHERE `id` >= ? AND `id` <= ? AND `id` > ? ORDER BY `id` LIMIT ?" {
		t.Errorf("range: %s", got)
	}
	// 标识符里的反引号必须加倍
	if got := quoteIdent("a`b"); got != "`a``b`" {
		t.Errorf("quote: %s", got)
	}
}

func TestBuildSQLPreview(t *testing.T) {
	spec := Spec{DB: "shop", Table: "orders", TimeColumn: "created_at", RetentionDays: 180, ExtraWhere: "status IN (3,4)", Mode: ModeMove,
		Dest:   &TableDest{InstanceID: 2, DB: "arch", Template: "{table}_{yyyy}"},
		Params: Params{BatchSize: 2000}}
	p := BuildSQLPreview(spec, []string{"id"}, []string{"id", "created_at", "status"}, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	for _, want := range []string{"SELECT `id`, `created_at`, `status` FROM `shop`.`orders`", "`created_at` < NOW() - INTERVAL 180 DAY AND (`status` IN (3, 4))", "LIMIT 2000", "不排序"} {
		if !strings.Contains(p.Select, want) {
			t.Errorf("Select 缺少 %q:\n%s", want, p.Select)
		}
	}
	for _, bad := range []string{"AND `id` > ?", "ORDER BY"} { // 归档并删除不排序、没有断点
		if strings.Contains(p.Select, bad) {
			t.Errorf("归档并删除的 Select 不应含 %q:\n%s", bad, p.Select)
		}
	}
	// 主键含字符串列（不能在本地比大小）时仍按主键排序读取
	infoStr := TableInfo{PrimaryKey: []string{"code"}, Columns: []ColumnInfo{{Name: "code", Type: "varchar(32)"}, {Name: "created_at", Type: "datetime"}}}
	if p := buildPreviewFor(spec, infoStr, nil, time.Now()); !strings.Contains(p.Select, "ORDER BY `code`") || !strings.Contains(p.Select, "AND `code` > ?") {
		t.Errorf("字符串主键应按主键排序读取：%s", p.Select)
	}
	infoInt := TableInfo{PrimaryKey: []string{"id"}, Columns: []ColumnInfo{{Name: "id", Type: "bigint(20) unsigned"}, {Name: "created_at", Type: "datetime"}}}
	if p := buildPreviewFor(spec, infoInt, nil, time.Now()); strings.Contains(p.Select, "ORDER BY") {
		t.Errorf("整数主键应不排序读取：%s", p.Select)
	}
	// 仅备份（不删数据）始终按主键翻页
	cp := spec
	cp.Mode = ModeCopy
	if p := buildPreviewFor(cp, infoInt, nil, time.Now()); !strings.Contains(p.Select, "ORDER BY `id`") || !strings.Contains(p.Select, "AND `id` > ?") {
		t.Errorf("仅备份应按主键翻页：%s", p.Select)
	}
	for _, want := range []string{"INSERT INTO `arch`.`orders_2026` (`id`, `created_at`, `status`)", "VALUES (?, ?, ?)"} {
		if !strings.Contains(p.Insert, want) {
			t.Errorf("Insert 缺少 %q:\n%s", want, p.Insert)
		}
	}
	for _, want := range []string{"DELETE FROM `shop`.`orders`", "`id` IN (?, ?, …)", "AND (`created_at` < NOW() - INTERVAL 180 DAY AND (`status` IN (3, 4)))"} {
		if !strings.Contains(p.Delete, want) {
			t.Errorf("Delete 缺少 %q:\n%s", want, p.Delete)
		}
	}
	// copy 没有 Delete；无库表备份没有 Insert；purge 都没有 Insert
	spec.Mode = ModeCopy
	if p := BuildSQLPreview(spec, nil, nil, time.Now()); p.Delete != "" || !strings.Contains(p.Select, "<主键>") || !strings.Contains(p.Select, "SELECT *") {
		t.Errorf("copy 预览不对：%+v", p)
	}
	spec.Mode, spec.Dest = ModePurge, nil
	if p := BuildSQLPreview(spec, []string{"id"}, nil, time.Now()); p.Insert != "" || p.Delete == "" {
		t.Errorf("purge 预览不对：%+v", p)
	}
	// 仅清理也不排序、没有断点（旧断点被忽略）
	spec.Checkpoint = []string{"a", "7"}
	if p := BuildSQLPreview(spec, []string{"tenant", "id"}, nil, time.Now()); strings.Contains(p.Select, "ORDER BY") || strings.Contains(p.Select, "'7'") {
		t.Errorf("仅清理不应排序 / 带断点：%s", p.Select)
	}
	// 仅备份：断点出现在 SELECT 里；复合主键用元组
	spec.Mode = ModeCopy
	p = BuildSQLPreview(spec, []string{"tenant", "id"}, nil, time.Now())
	if !strings.Contains(p.Select, "(`tenant`, `id`) > ('a', '7')") {
		t.Errorf("复合主键断点：%s", p.Select)
	}
	// 条件非法时给出提示而不是 panic
	spec.ExtraWhere = "a=1;drop"
	if p := BuildSQLPreview(spec, []string{"id"}, nil, time.Now()); !strings.Contains(p.Select, "条件无效") {
		t.Errorf("非法条件：%s", p.Select)
	}
}

func TestAdaptor(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	a := newAdaptor(1000, 200, ms(500))
	if ch, _, _ := a.observe(ms(100)); ch {
		t.Fatalf("正常批不应调整")
	}
	ch, slow, msg := a.observe(ms(900))
	if !ch || !slow || a.batch != 500 || a.sleep != 400 || msg == "" {
		t.Fatalf("慢批：batch=%d sleep=%d %v %v %q", a.batch, a.sleep, ch, slow, msg)
	}
	a.observe(ms(900))
	a.observe(ms(900))
	a.observe(ms(900))
	if a.batch != 100 || a.sleep != 3200 {
		t.Fatalf("连续变慢：batch=%d sleep=%d", a.batch, a.sleep)
	}
	a.observe(ms(900)) // 5000 封顶
	if a.sleep != 5000 {
		t.Fatalf("休眠封顶 5000：%d", a.sleep)
	}
	if ch, _, _ := a.observe(ms(900)); ch {
		t.Fatalf("已到下限 / 上限，不再变化")
	}
	// 恢复：连续 5 批正常才恢复一步
	for i := 0; i < 4; i++ {
		if ch, _, _ := a.observe(ms(100)); ch {
			t.Fatalf("第 %d 批正常不应恢复", i+1)
		}
	}
	ch, slow, _ = a.observe(ms(100))
	if !ch || slow || a.batch != 150 || a.sleep != 2500 {
		t.Fatalf("第一次恢复：batch=%d sleep=%d", a.batch, a.sleep)
	}
	// 一批慢会打断连续计数
	for i := 0; i < 4; i++ {
		a.observe(ms(100))
	}
	a.observe(ms(900))
	for i := 0; i < 4; i++ {
		if ch, _, _ := a.observe(ms(100)); ch {
			t.Fatalf("慢批后计数应重新开始")
		}
	}
	// 恢复到设定值后不再变化，且不会超过
	for i := 0; i < 500; i++ {
		a.observe(ms(100))
	}
	if a.batch != 1000 || a.sleep != 200 {
		t.Fatalf("最终应恢复到设定值：batch=%d sleep=%d", a.batch, a.sleep)
	}
	// 休眠基线为 0（仅备份）
	b := newAdaptor(100, 0, ms(500))
	b.observe(ms(900))
	if b.sleep != 100 || b.batch != 100 {
		t.Fatalf("0 基线：%d %d", b.batch, b.sleep)
	}
}

func TestInterpolateArgs(t *testing.T) {
	got, err := interpolateArgs("SELECT * FROM t WHERE a = ? AND b = 'x?y''?' AND `c?` > ? LIMIT ?", []any{int64(5), "o'k\\", uint64(18446744073709551615)})
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT * FROM t WHERE a = 5 AND b = 'x?y''?' AND `c?` > 'o\\'k\\\\' LIMIT 18446744073709551615"
	if got != want {
		t.Errorf("\n got  %s\n want %s", got, want)
	}
	if _, err := interpolateArgs("a = ? AND b = ?", []any{1}); err == nil {
		t.Errorf("参数不足应报错")
	}
	if _, err := interpolateArgs("a = ?", []any{1, 2}); err == nil {
		t.Errorf("参数过多应报错")
	}
	if _, err := interpolateArgs("a = ?", []any{1.5}); err == nil {
		t.Errorf("不支持的类型应报错")
	}
}
