// @author: kevinkelly
// @license: Apache Licence
// @file: scan_test.go
// @time: 2026/10/28

package binlog

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/replication"
)

const t0 = 1700000000 // 2023-11-14 22:13:20 UTC

func dmlTypes() map[string]bool {
	return map[string]bool{KindInsert: true, KindUpdate: true, KindDelete: true}
}

func baseSpec(mode string) scanSpec {
	return scanSpec{Mode: mode, StartFile: "bin.000001", Types: dmlTypes(), MaxRows: 100, MaxBytes: 1 << 20, MaxScan: 100000, Loc: time.UTC}
}

// scenario: 事务 1 = 插入 + 更新，事务 2 = 删除。
func scenario() *binBuilder {
	b := newBin(t0)
	b.fde()
	b.begin(t0 + 1)
	b.tableMap(ordersTbl, t0+1)
	b.insert(t0+1, []any{1, "apple", 200, nil})
	b.tableMap(ordersTbl, t0+2)
	b.update(t0+2, []any{1, "apple", 200, nil}, []any{1, "pear", 200, "x"})
	b.commit(t0 + 2)
	b.begin(t0 + 10)
	b.tableMap(ordersTbl, t0+10)
	b.delete(t0+10, []any{2, `bob's \ cat`, 5, "n"})
	b.commit(t0 + 10)
	return b
}

func runScan(t *testing.T, b *binBuilder, spec scanSpec, meta MetaSource) (*scanResult, error) {
	t.Helper()
	evs := append([]*replication.BinlogEvent{rotateEvent(t, "bin.000001")}, b.events(t)...)
	if meta == nil {
		meta = newFakeMeta()
	}
	return scan(context.Background(), spec, &memStream{evs: evs}, meta, &progress{})
}

// stmtsOf 取出结果里全部语句，按执行顺序（回滚结果是读到的相反顺序，还原结果是读到的顺序），Seq 从 1 起连续编号。
func stmtsOf(r *scanResult) []Stmt {
	out, err := r.Stmts.Page(0, max(r.Stmts.Len(), 1), r.mode == ModeRollback)
	if err != nil {
		panic(err)
	}
	return out
}

func sqls(r *scanResult) []string {
	var out []string
	for _, s := range stmtsOf(r) {
		out = append(out, s.SQL)
	}
	return out
}

func wantSQLs(t *testing.T, got *scanResult, want ...string) {
	t.Helper()
	g := sqls(got)
	if len(g) != len(want) {
		t.Fatalf("语句数 %d，期望 %d:\n%s", len(g), len(want), strings.Join(g, "\n"))
	}
	for i := range want {
		if g[i] != want[i] {
			t.Errorf("第 %d 条:\n got %s\nwant %s", i+1, g[i], want[i])
		}
	}
}

func TestScanForwardSQL(t *testing.T) {
	res, err := runScan(t, scenario(), baseSpec(ModeSQL), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantSQLs(t, res,
		"INSERT INTO `shop`.`orders` (`id`,`name`,`qty`,`note`) VALUES (1,'apple',200,NULL);",
		"UPDATE `shop`.`orders` SET `name` = 'pear', `note` = 'x' WHERE `id` = 1;",
		"DELETE FROM `shop`.`orders` WHERE `id` = 2;")
	st := stmtsOf(res)
	if st[0].Type != KindInsert || st[1].Type != KindUpdate || st[2].Type != KindDelete {
		t.Errorf("类型不对: %+v", st)
	}
	if st[0].Trx != 1 || st[1].Trx != 1 || st[2].Trx != 2 || res.Trx != 2 {
		t.Errorf("事务序号不对: %d %d %d / %d", st[0].Trx, st[1].Trx, st[2].Trx, res.Trx)
	}
	if st[0].Time != "2023-11-14 22:13:21" || st[0].File != "bin.000001" || st[0].Schema != "shop" || st[0].Table != "orders" || st[0].Seq != 1 {
		t.Errorf("元信息不对: %+v", st[0])
	}
	if st[0].Pos == 0 || st[0].EndPos <= st[0].Pos {
		t.Errorf("位置不对: %+v", st[0])
	}
	if res.Truncated != "" || len(res.Warnings) != 0 || res.ByType[KindInsert] != 1 {
		t.Errorf("汇总不对: %+v", res)
	}
}

func TestScanRollbackSQL(t *testing.T) {
	res, err := runScan(t, scenario(), baseSpec(ModeRollback), nil)
	if err != nil {
		t.Fatal(err)
	}
	// 后发生的先撤销：删除 → 插入回去，更新 → 改回去，插入 → 删掉
	wantSQLs(t, res,
		"INSERT INTO `shop`.`orders` (`id`,`name`,`qty`,`note`) VALUES (2,'bob''s \\\\ cat',5,'n');",
		"UPDATE `shop`.`orders` SET `name` = 'apple', `note` = NULL WHERE `id` = 1;",
		"DELETE FROM `shop`.`orders` WHERE `id` = 1;")
	for i, s := range stmtsOf(res) {
		if s.Seq != i+1 {
			t.Errorf("反转后序号应重排: %+v", s)
		}
	}
	if stmtsOf(res)[0].Type != KindDelete {
		t.Errorf("Type 保留原始操作类型: %+v", stmtsOf(res)[0])
	}
}

func TestScanOptions(t *testing.T) {
	sp := baseSpec(ModeSQL)
	sp.NoDBPrefix, sp.FullColumns, sp.ExtraInfo = true, true, true
	res, err := runScan(t, scenario(), sp, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := sqls(res)
	if !strings.HasPrefix(g[0], "INSERT INTO `orders` (`id`") || !strings.Contains(g[0], " -- bin.000001:") || !strings.HasSuffix(g[0], "2023-11-14 22:13:21") {
		t.Errorf("无库名前缀 / 位置注释: %s", g[0])
	}
	if !strings.Contains(g[1], "SET `id` = 1, `name` = 'pear', `qty` = 200, `note` = 'x' WHERE `id` = 1 AND `name` = 'apple' AND `qty` = 200 AND `note` IS NULL") {
		t.Errorf("完整列: %s", g[1])
	}
}

func TestScanFilters(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*scanSpec)
		want int
	}{
		{"只要 INSERT", func(s *scanSpec) { s.Types = map[string]bool{KindInsert: true} }, 1},
		{"只要 DELETE", func(s *scanSpec) { s.Types = map[string]bool{KindDelete: true} }, 1},
		{"库不匹配", func(s *scanSpec) { s.Schemas = map[string]bool{"other": true} }, 0},
		{"库匹配（大小写不敏感）", func(s *scanSpec) { s.Schemas = map[string]bool{"shop": true} }, 3},
		{"表不匹配", func(s *scanSpec) { s.Tables = []tableFilter{{Table: "users"}} }, 0},
		{"库.表匹配", func(s *scanSpec) { s.Tables = []tableFilter{{Schema: "shop", Table: "orders"}} }, 3},
		{"库.表的库不对", func(s *scanSpec) { s.Tables = []tableFilter{{Schema: "x", Table: "orders"}} }, 0},
		{"不可见的库", func(s *scanSpec) { s.Visible = func(n string) bool { return n != "shop" } }, 0},
		{"开始时间晚于全部事件", func(s *scanSpec) { s.StartTS = t0 + 100 }, 0},
		{"开始时间在更新之后", func(s *scanSpec) { s.StartTS = t0 + 3 }, 1},
		{"结束时间在更新之前", func(s *scanSpec) { s.StopTS = t0 + 1 }, 1},
		{"结束时间在删除之前", func(s *scanSpec) { s.StopTS = t0 + 5 }, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sp := baseSpec(ModeSQL)
			c.mod(&sp)
			res, err := runScan(t, scenario(), sp, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(stmtsOf(res)) != c.want {
				t.Errorf("语句数 %d，期望 %d: %v", len(stmtsOf(res)), c.want, sqls(res))
			}
		})
	}
}

func TestScanPositionBounds(t *testing.T) {
	b := scenario()
	evs := b.events(t)
	// 找到更新事件的结束位置，EndPos 设在它上面：只包含插入和更新
	var updEnd uint32
	for _, e := range evs {
		if re, ok := e.Event.(*replication.RowsEvent); ok && re.Type() == replication.EnumRowsEventTypeUpdate {
			updEnd = e.Header.LogPos
		}
	}
	sp := baseSpec(ModeSQL)
	sp.EndFile, sp.EndPos = "bin.000001", updEnd
	res, err := runScan(t, b, sp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmtsOf(res)) != 2 {
		t.Fatalf("结束位置之前应有 2 条: %v", sqls(res))
	}
	// 快照末尾：LastSize 设在更新之后，同样只读到更新
	sp = baseSpec(ModeSQL)
	sp.LastFile, sp.LastSize = "bin.000001", int64(updEnd)
	res, err = runScan(t, b, sp, nil)
	if err != nil || len(stmtsOf(res)) != 2 {
		t.Fatalf("快照末尾: %v %v", err, sqls(res))
	}
	// 结束文件早于当前文件：Rotate 之后立即停止
	sp = baseSpec(ModeSQL)
	sp.EndFile = "bin.000000"
	res, err = runScan(t, b, sp, nil)
	if err != nil || len(stmtsOf(res)) != 0 {
		t.Fatalf("结束文件更早: %v %v", err, sqls(res))
	}
}

func TestScanLimits(t *testing.T) {
	sp := baseSpec(ModeSQL)
	sp.MaxRows = 2
	res, err := runScan(t, scenario(), sp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmtsOf(res)) != 2 || res.Truncated != "rows" {
		t.Errorf("条数上限: %d %q", len(stmtsOf(res)), res.Truncated)
	}
	sp = baseSpec(ModeSQL)
	sp.MaxBytes = 300 // 一条 SQL 约 90 字节 + 开销 160
	res, err = runScan(t, scenario(), sp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmtsOf(res)) != 1 || res.Truncated != "bytes" {
		t.Errorf("体积上限: %d %q", len(stmtsOf(res)), res.Truncated)
	}
	sp = baseSpec(ModeSQL)
	sp.MaxScan = 3
	res, err = runScan(t, scenario(), sp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated != "scan" || res.Scanned != 3 {
		t.Errorf("扫描上限: %q %d", res.Truncated, res.Scanned)
	}
}

func TestScanCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	evs := append([]*replication.BinlogEvent{rotateEvent(t, "bin.000001")}, scenario().events(t)...)
	_, err := scan(ctx, baseSpec(ModeSQL), &memStream{evs: evs}, newFakeMeta(), nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("取消应返回 context.Canceled: %v", err)
	}
}

func TestScanDDL(t *testing.T) {
	b := newBin(t0)
	b.fde()
	b.query("shop", "ALTER TABLE orders ADD COLUMN x INT", t0+1)
	b.query("shop", "SET @a=1", t0+1) // 不是数据 / 结构变更，忽略
	b.begin(t0 + 2)
	b.tableMap(ordersTbl, t0+2)
	b.insert(t0+2, []any{1, "a", 1, nil})
	b.commit(t0 + 2)
	b.query("other", "DROP TABLE t1", t0+3)

	sp := baseSpec(ModeSQL)
	res, err := runScan(t, b, sp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmtsOf(res)) != 1 || res.DDL != 2 {
		t.Fatalf("默认类型不含 DDL，但要统计并提示: %d stmts, %d ddl", len(stmtsOf(res)), res.DDL)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "2 条 DDL") {
		t.Errorf("应有 DDL 警告: %v", res.Warnings)
	}
	sp.Types[KindDDL] = true
	res, err = runScan(t, b, sp, nil)
	if err != nil || len(stmtsOf(res)) != 3 {
		t.Fatalf("选了 ddl: %v %d", err, len(stmtsOf(res)))
	}
	if stmtsOf(res)[0].Type != KindDDL || stmtsOf(res)[0].SQL != "ALTER TABLE orders ADD COLUMN x INT;" || !stmtsOf(res)[0].NoRollback || stmtsOf(res)[0].Schema != "shop" {
		t.Errorf("DDL 行: %+v", stmtsOf(res)[0])
	}
	// 回滚模式：DDL 只给提示注释
	sp.Mode = ModeRollback
	res, err = runScan(t, b, sp, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ddl []string
	for _, s := range stmtsOf(res) {
		if s.Type == KindDDL {
			ddl = append(ddl, s.SQL)
		}
	}
	if len(ddl) != 2 || !strings.HasPrefix(ddl[0], "-- [无法回滚的DDL] ") {
		t.Errorf("回滚模式的 DDL: %v", ddl)
	}
	// 库不可见：DDL 也不显示
	sp = baseSpec(ModeSQL)
	sp.Types[KindDDL] = true
	sp.Visible = func(n string) bool { return n == "shop" }
	res, _ = runScan(t, b, sp, nil)
	if res.DDL != 1 {
		t.Errorf("不可见库的 DDL 不应出现: %d", res.DDL)
	}
	// 表过滤对 DDL 按文本是否提到表名判断
	sp = baseSpec(ModeSQL)
	sp.Types[KindDDL] = true
	sp.Tables = []tableFilter{{Table: "t1"}}
	res, _ = runScan(t, b, sp, nil)
	if len(stmtsOf(res)) != 1 || !strings.Contains(stmtsOf(res)[0].SQL, "DROP TABLE t1") {
		t.Errorf("DDL 表过滤: %v", sqls(res))
	}
}

func TestScanStatementFormat(t *testing.T) {
	b := newBin(t0)
	b.fde()
	b.begin(t0)
	b.query("shop", "update orders set qty=1", t0)
	b.commit(t0)
	sp := baseSpec(ModeSQL)
	res, err := runScan(t, b, sp, nil)
	if err != nil || len(stmtsOf(res)) != 1 || stmtsOf(res)[0].Type != KindStatement || stmtsOf(res)[0].SQL != "update orders set qty=1;" {
		t.Fatalf("STATEMENT 格式: %v %+v", err, res)
	}
	sp.Types = map[string]bool{KindInsert: true}
	res, _ = runScan(t, b, sp, nil)
	if len(stmtsOf(res)) != 0 {
		t.Errorf("类型过滤应按语句关键字: %v", sqls(res))
	}
}

func TestScanRollbackRequiresFullImage(t *testing.T) {
	b := newBin(t0)
	b.fde()
	b.begin(t0)
	b.tableMap(ordersTbl, t0)
	// 只记录主键和变化的列（binlog_row_image=MINIMAL）：前镜像只有 id，后镜像只有 id、name
	b.rows(replication.UPDATE_ROWS_EVENTv2, t0, []bool{true, false, false, false}, []bool{true, true, false, false},
		[]any{1, "", 0, ""}, []any{1, "pear", 0, ""})
	b.commit(t0)

	res, err := runScan(t, b, baseSpec(ModeSQL), nil)
	if err != nil {
		t.Fatal(err)
	}
	// 还原模式仍可用：SET 只含后镜像里有的列
	wantSQLs(t, res, "UPDATE `shop`.`orders` SET `name` = 'pear' WHERE `id` = 1;")
	_, err = runScan(t, b, baseSpec(ModeRollback), nil)
	var be *Error
	if !errors.As(err, &be) || be.Code != "rollback_unsupported" || !strings.Contains(be.Message, "binlog_row_image") {
		t.Fatalf("不完整镜像应拒绝回滚: %v", err)
	}
}

func TestScanMissingTableMeta(t *testing.T) {
	// 表已被删除：还原模式用 @N 占位并提示，回滚模式拒绝
	meta := &fakeMeta{cols: map[string][]colMeta{}}
	res, err := runScan(t, scenario(), baseSpec(ModeSQL), meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmtsOf(res)) != 3 || !strings.Contains(stmtsOf(res)[0].SQL, "VALUES (1,'apple',-56,NULL)") || strings.Contains(stmtsOf(res)[0].SQL, "(`") {
		t.Errorf("无元数据的 INSERT: %v", sqls(res))
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "已不存在") {
		t.Errorf("应有表已不存在的警告: %v", res.Warnings)
	}
	if meta.queries != 1 {
		t.Errorf("同一张表只查一次结构: %d", meta.queries)
	}
	_, err = runScan(t, scenario(), baseSpec(ModeRollback), meta)
	var be *Error
	if !errors.As(err, &be) || be.Code != "rollback_unsupported" {
		t.Errorf("回滚应拒绝: %v", err)
	}
	// 列数对不上（表结构之后被改过）
	meta = &fakeMeta{cols: map[string][]colMeta{"shop.orders": append(append([]colMeta{}, ordersCols...), colMeta{Name: "extra", DataType: "int"})}}
	res, err = runScan(t, scenario(), baseSpec(ModeSQL), meta)
	if err != nil || len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "之后被修改过") {
		t.Errorf("列数不一致: %v %v", err, res.Warnings)
	}
	// 读取表结构失败
	meta = &fakeMeta{err: errors.New("boom")}
	res, err = runScan(t, scenario(), baseSpec(ModeSQL), meta)
	if err != nil || len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "读取目标库表结构失败") {
		t.Errorf("读取失败: %v %v", err, res.Warnings)
	}
}

func TestScanVirtualGeneratedColumn(t *testing.T) {
	// 目标库有 5 列（最后一列是虚拟生成列），binlog 只记录 4 列：按“去掉虚拟列”对齐
	cols := append(append([]colMeta{}, ordersCols...), colMeta{Name: "v", DataType: "int", Generated: true, Virtual: true})
	meta := &fakeMeta{cols: map[string][]colMeta{"shop.orders": cols}}
	res, err := runScan(t, scenario(), baseSpec(ModeSQL), meta)
	if err != nil || len(res.Warnings) != 0 {
		t.Fatalf("%v %v", err, res.Warnings)
	}
	if !strings.HasPrefix(stmtsOf(res)[0].SQL, "INSERT INTO `shop`.`orders` (`id`,`name`,`qty`,`note`)") {
		t.Errorf("%s", stmtsOf(res)[0].SQL)
	}
}

func TestScanNoPrimaryKeyUsesAllColumnsAndLimit(t *testing.T) {
	cols := make([]colMeta, len(ordersCols))
	copy(cols, ordersCols)
	cols[0].PK = false
	meta := &fakeMeta{cols: map[string][]colMeta{"shop.orders": cols}}
	res, err := runScan(t, scenario(), baseSpec(ModeSQL), meta)
	if err != nil {
		t.Fatal(err)
	}
	wantSQLs(t, res,
		"INSERT INTO `shop`.`orders` (`id`,`name`,`qty`,`note`) VALUES (1,'apple',200,NULL);",
		"UPDATE `shop`.`orders` SET `name` = 'pear', `note` = 'x' WHERE `id` = 1 AND `name` = 'apple' AND `qty` = 200 AND `note` IS NULL LIMIT 1;",
		"DELETE FROM `shop`.`orders` WHERE `id` = 2 AND `name` = 'bob''s \\\\ cat' AND `qty` = 5 AND `note` = 'n' LIMIT 1;")
}

func TestScanFromRawFile(t *testing.T) {
	// 把事件写成真正的 binlog 文件内容，再用 go-mysql 的文件解析器读回来：与 COM_BINLOG_DUMP 的字节流同构
	b := scenario()
	raw := b.fileBytes()
	p := replication.NewBinlogParser()
	var evs []*replication.BinlogEvent
	if err := p.ParseReader(strings.NewReader(string(raw[4:])), func(e *replication.BinlogEvent) error { evs = append(evs, e); return nil }); err != nil {
		t.Fatal(err)
	}
	evs = append([]*replication.BinlogEvent{rotateEvent(t, "bin.000001")}, evs...)
	res, err := scan(context.Background(), baseSpec(ModeSQL), &memStream{evs: evs}, newFakeMeta(), nil)
	if err != nil || len(stmtsOf(res)) != 3 {
		t.Fatalf("%v %v", err, sqls(res))
	}
}

func TestFileCmp(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"mysql-bin.000009", "mysql-bin.000010", -1}, {"mysql-bin.000010", "mysql-bin.000009", 1}, {"a.000001", "a.000001", 0},
		{"x.1", "x.01", 0}, {"abc", "abd", -1},
	}
	for _, c := range cases {
		if got := fileCmp(c.a, c.b); got != c.want {
			t.Errorf("fileCmp(%s,%s)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestFriendlyStreamErr(t *testing.T) {
	err := friendlyStreamErr(errors.New("invalid table id 109, no corresponding table map event"))
	var be *Error
	if !errors.As(err, &be) || be.Code != "bad_position" {
		t.Errorf("%v", err)
	}
}
