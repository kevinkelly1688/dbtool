// @author: kevinkelly
// @license: Apache Licence
// @file: mysqlcommon_test.go
// @time: 2026/10/28

package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	drv "github.com/go-sql-driver/mysql"
)

// PolarDB-X（DRDS）的 SHOW CREATE TABLE 带 dbpartition / tbpartition / BROADCAST / LOCALITY：备份表不拆分，要去掉。
func TestRewriteCreateSQLStripsPolarDBXClauses(t *testing.T) {
	for name, ddl := range map[string]string{
		"dbpartition": "CREATE TABLE `orders` (\n  `id` bigint(20) NOT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4\ndbpartition by hash(`id`) tbpartition by hash(`id`) tbpartitions 4",
		"broadcast":   "CREATE TABLE `orders` (\n  `id` bigint(20) NOT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 BROADCAST",
		"single":      "CREATE TABLE `orders` (\n  `id` bigint(20) NOT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 SINGLE",
		"locality":    "CREATE TABLE `orders` (\n  `id` bigint(20) NOT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 LOCALITY='dn=dn-1'",
		"auto":        "CREATE TABLE `orders` (\n  `id` bigint(20) NOT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4\nPARTITION BY KEY(`id`)\nPARTITIONS 8",
	} {
		got, err := rewriteCreateSQL(ddl, "arch", "orders_bak")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, bad := range []string{"dbpartition", "tbpartition", "BROADCAST", "SINGLE", "LOCALITY", "PARTITION"} {
			if strings.Contains(strings.ToUpper(got), strings.ToUpper(bad)) {
				t.Errorf("%s：不应保留 %s：\n%s", name, bad, got)
			}
		}
		if !strings.Contains(got, "PRIMARY KEY (`id`)") || !strings.HasSuffix(strings.TrimSpace(got), "DEFAULT CHARSET=utf8mb4") {
			t.Errorf("%s：结构被破坏：\n%s", name, got)
		}
	}
}

func TestRewriteCreateSQL(t *testing.T) {
	ddl := "CREATE TABLE `orders` (\n" +
		"  `id` bigint(20) NOT NULL AUTO_INCREMENT,\n" +
		"  `user_id` bigint(20) NOT NULL,\n" +
		"  `created_at` datetime NOT NULL,\n" +
		"  PRIMARY KEY (`id`),\n" +
		"  KEY `idx_created` (`created_at`),\n" +
		"  CONSTRAINT `fk_user` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE\n" +
		") ENGINE=InnoDB AUTO_INCREMENT=9999 DEFAULT CHARSET=utf8mb4\n" +
		"/*!50100 PARTITION BY RANGE (`id`)\n(PARTITION p0 VALUES LESS THAN (100) ENGINE = InnoDB,\n PARTITION pmax VALUES LESS THAN MAXVALUE ENGINE = InnoDB) */"
	got, err := rewriteCreateSQL(ddl, "arch", "orders_2026")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "CREATE TABLE IF NOT EXISTS `arch`.`orders_2026` (") {
		t.Errorf("表名没有改写：%s", got)
	}
	for _, bad := range []string{"AUTO_INCREMENT=9999", "FOREIGN KEY", "PARTITION", "fk_user"} {
		if strings.Contains(got, bad) {
			t.Errorf("不应保留 %q：\n%s", bad, got)
		}
	}
	// 去掉外键后，前一行结尾的逗号要一并去掉，否则语法错误
	if strings.Contains(got, "(`created_at`),\n)") || strings.Contains(got, "),\n) ENGINE") {
		t.Errorf("悬空逗号：\n%s", got)
	}
	if !strings.Contains(got, "KEY `idx_created` (`created_at`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4") {
		t.Errorf("其余结构应保留：\n%s", got)
	}
	// 列名里带 AUTO_INCREMENT 属性要保留（只去掉表选项里的起点）
	if !strings.Contains(got, "`id` bigint(20) NOT NULL AUTO_INCREMENT,") {
		t.Errorf("列的 AUTO_INCREMENT 属性应保留")
	}
	// MariaDB 风格的裸 PARTITION BY
	mdb := "CREATE TABLE `t` (\n  `id` int(11) NOT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=latin1\n PARTITION BY HASH (`id`)\nPARTITIONS 4"
	got, err = rewriteCreateSQL(mdb, "a", "b")
	if err != nil || strings.Contains(got, "PARTITION") || !strings.HasSuffix(got, "DEFAULT CHARSET=latin1") {
		t.Errorf("MariaDB 分区：%v\n%s", err, got)
	}
	// 表名里的反引号
	got, _ = rewriteCreateSQL("CREATE TABLE `we``ird` (\n  `id` int\n)", "a", "b")
	if !strings.HasPrefix(got, "CREATE TABLE IF NOT EXISTS `a`.`b` (") {
		t.Errorf("带反引号的源表名：%s", got)
	}
	if _, err := rewriteCreateSQL("DROP TABLE x", "a", "b"); err == nil {
		t.Errorf("不是建表语句应报错")
	}
	if _, err := rewriteCreateSQL("CREATE VIEW `v` AS select 1", "a", "b"); err == nil {
		t.Errorf("视图不应被改写")
	}
}

func TestCompatErr(t *testing.T) {
	src := TableInfo{Columns: []ColumnInfo{{Name: "id", Type: "bigint(20)"}, {Name: "Name", Type: "varchar(10)"}}, PrimaryKey: []string{"id"}}
	same := TableInfo{Columns: []ColumnInfo{{Name: "ID", Type: "bigint(20)"}, {Name: "name", Type: "VARCHAR(10)"}, {Name: "extra", Type: "int"}}, PrimaryKey: []string{"id"}}
	if err := compatErr(src, same); err != nil {
		t.Errorf("大小写不敏感，目标可以有多余的列：%v", err)
	}
	width := TableInfo{Columns: []ColumnInfo{{Name: "id", Type: "bigint"}, {Name: "name", Type: "varchar(10)"}}, PrimaryKey: []string{"id"}}
	if err := compatErr(src, width); err != nil {
		t.Errorf("整数显示宽度差异应忽略：%v", err)
	}
	missing := TableInfo{Columns: []ColumnInfo{{Name: "id", Type: "bigint(20)"}}, PrimaryKey: []string{"id"}}
	if err := compatErr(src, missing); err == nil || !strings.Contains(err.Error(), "缺少列 Name") {
		t.Errorf("缺列：%v", err)
	}
	diff := TableInfo{Columns: []ColumnInfo{{Name: "id", Type: "bigint(20)"}, {Name: "name", Type: "varchar(5)"}}, PrimaryKey: []string{"id"}}
	if err := compatErr(src, diff); err == nil || !strings.Contains(err.Error(), "类型不一致") {
		t.Errorf("类型不同：%v", err)
	}
	nopk := TableInfo{Columns: same.Columns}
	if err := compatErr(src, nopk); err == nil || !strings.Contains(err.Error(), "主键") {
		t.Errorf("主键不同：%v", err)
	}
}

func TestChunking(t *testing.T) {
	// 按占位符个数切
	rows := make([]Row, 10)
	for i := range rows {
		rows[i] = Row{int64(i), "x"}
	}
	if got := chunkRows(rows, 2); len(got) != 1 || len(got[0]) != 10 {
		t.Errorf("小批不应切分：%d 段", len(got))
	}
	many := make([]Row, 100000)
	for i := range many {
		many[i] = Row{int64(i), int64(i), int64(i)}
	}
	parts := chunkRows(many, 3)
	total := 0
	for _, p := range parts {
		total += len(p)
		if len(p)*3 > maxPlaceholders {
			t.Fatalf("一段 %d 行 × 3 列超过占位符上限", len(p))
		}
	}
	if total != len(many) || len(parts) < 5 {
		t.Errorf("切分后行数 %d，段数 %d", total, len(parts))
	}
	// 按字节切：每行约 600 KB 的大字段
	big := []Row{{make([]byte, 600<<10)}, {make([]byte, 600<<10)}, {make([]byte, 600<<10)}}
	if got := chunkRows(big, 1); len(got) != 3 {
		t.Errorf("大行应各自成段：%d", len(got))
	}
	if got := chunkRows(nil, 3); len(got) != 0 {
		t.Errorf("空输入：%d", len(got))
	}
	// 主键 IN 的切分
	sp := chunkSpans(25000, 3)
	if len(sp) != 2 || sp[0] != [2]int{0, 20000} || sp[1] != [2]int{20000, 25000} {
		t.Errorf("spans: %v", sp)
	}
	if got := chunkSpans(0, 1); len(got) != 0 {
		t.Errorf("空：%v", got)
	}
	if got := chunkSpans(10, 70000); len(got) != 10 {
		t.Errorf("一个主键就超过上限时也要继续（每段 1 个）：%v", got)
	}
}

func TestPKArgs(t *testing.T) {
	if v := pkArg("bigint(20)", "9223372036854775807"); v != int64(9223372036854775807) {
		t.Errorf("bigint: %#v", v)
	}
	if v := pkArg("bigint(20) unsigned", "18446744073709551615"); v != uint64(18446744073709551615) {
		t.Errorf("bigint unsigned: %#v", v)
	}
	if v := pkArg("int(11)", "-5"); v != int64(-5) {
		t.Errorf("int: %#v", v)
	}
	if v := pkArg("int(11)", "abc"); v != "abc" {
		t.Errorf("非法整数保持字符串：%#v", v)
	}
	if v := pkArg("decimal(10,2)", "1.50"); v != "1.50" {
		t.Errorf("decimal: %#v", v)
	}
	if v := pkArg("varchar(20)", "abc"); v != "abc" {
		t.Errorf("varchar: %#v", v)
	}
	if v, ok := pkArg("varbinary(16)", "0x0a0b").([]byte); !ok || len(v) != 2 || v[0] != 0x0a {
		t.Errorf("binary: %#v", v)
	}
	if v := pkArg("", "5"); v != "5" {
		t.Errorf("类型未知保持字符串：%#v", v)
	}
	var tc typeCache
	tc.set("d", "t", []ColumnInfo{{Name: "ID", Type: "bigint"}, {Name: "k", Type: "varchar(5)"}})
	args := tc.pkArgs("d", "t", []string{"id", "k"}, []string{"7", "x"})
	if args[0] != int64(7) || args[1] != "x" {
		t.Errorf("pkArgs: %#v", args)
	}
	if s := pkString([]byte("abc"), "varchar(5)"); s != "abc" {
		t.Errorf("pkString: %s", s)
	}
	if s := pkString([]byte{1, 2}, "varbinary(5)"); s != "0x0102" {
		t.Errorf("pkString binary: %s", s)
	}
	if s := pkString(int64(42), "bigint"); s != "42" {
		t.Errorf("pkString int: %s", s)
	}
}

func TestFmtOffset(t *testing.T) {
	for in, want := range map[string]string{"08:00:00": "+08:00", "00:00:00": "+00:00", "-05:00:00": "-05:00", "05:30:00": "+05:30", "-00:30:00": "-00:30", "+09:00:00": "+09:00"} {
		got, err := fmtOffset(in)
		if err != nil || got != want {
			t.Errorf("fmtOffset(%q)=%q,%v want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "99:00:00", "08:99:00", "8"} {
		if _, err := fmtOffset(bad); err == nil {
			t.Errorf("fmtOffset(%q) 应报错", bad)
		}
	}
}

func TestFingerprintAndClassify(t *testing.T) {
	a := Fingerprint([]ColumnInfo{{Name: "id", Type: "BIGINT"}, {Name: "x", Type: "int"}}, []string{"id"})
	b := Fingerprint([]ColumnInfo{{Name: "id", Type: "bigint"}, {Name: "x", Type: "int"}}, []string{"id"})
	if a != b || len(a) != 64 {
		t.Errorf("类型大小写不影响指纹")
	}
	for name, other := range map[string]string{
		"列顺序":  Fingerprint([]ColumnInfo{{Name: "x", Type: "int"}, {Name: "id", Type: "bigint"}}, []string{"id"}),
		"列类型":  Fingerprint([]ColumnInfo{{Name: "id", Type: "bigint"}, {Name: "x", Type: "bigint"}}, []string{"id"}),
		"主键不同": Fingerprint([]ColumnInfo{{Name: "id", Type: "bigint"}, {Name: "x", Type: "int"}}, []string{"x"}),
		"多一列":  Fingerprint([]ColumnInfo{{Name: "id", Type: "bigint"}, {Name: "x", Type: "int"}, {Name: "y", Type: "int"}}, []string{"id"}),
	} {
		if other == a {
			t.Errorf("%s变化应改变指纹", name)
		}
	}
	for _, n := range []uint16{1205, 1213} {
		var re *RetryableError
		if err := classify(fmt.Errorf("wrap: %w", &drv.MySQLError{Number: n, Message: "x"})); !errors.As(err, &re) {
			t.Errorf("%d 应是可重试错误", n)
		}
	}
	if err := classify(&drv.MySQLError{Number: 1062, Message: "dup"}); errors.As(err, new(*RetryableError)) {
		t.Errorf("1062 不应重试")
	}
	if classify(nil) != nil {
		t.Errorf("nil")
	}
}
