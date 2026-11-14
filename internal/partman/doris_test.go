// @author: kevinkelly
// @license: Apache Licence
// @file: doris_test.go
// @time: 2026/10/28

package partman

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// SHOW PARTITIONS 的 Range 列文本：原程序 selectdb.go 的 dorisKeyRe 取最后一个 keys 为上界；这里覆盖不同 Doris 版本的写法。
func TestDorisUpperParsing(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	cases := []struct {
		name    string
		rng     string
		colType string
		want    time.Time
		max     bool
		ok      bool
	}{
		{"datetime 完整 Range（lower..upper）", "[types: [DATETIME]; keys: [2026-06-19 00:00:00]; ..types: [DATETIME]; keys: [2026-06-20 00:00:00]; )", ColDatetime, time.Date(2026, 6, 20, 0, 0, 0, 0, loc), false, true},
		{"datetime 只有一个 keys", "keys: [2026-06-20 06:30:00]", ColDatetime, time.Date(2026, 6, 20, 6, 30, 0, 0, loc), false, true},
		{"bigint 毫秒只有一个 keys", "keys: [1781937000000]", ColBigint, time.UnixMilli(1781937000000).In(loc), false, true},
		{"bigint 多个 keys 取最后一个", "[types: [BIGINT]; keys: [1781850600000]; ..types: [BIGINT]; keys: [1781937000000]; )", ColBigint, time.UnixMilli(1781937000000).In(loc), false, true},
		{"int 秒", "[types: [INT]; keys: [1781850600]; ..types: [INT]; keys: [1781937000]; )", ColInt, time.Unix(1781937000, 0).In(loc), false, true},
		{"date", "[types: [DATE]; keys: [2026-06-19]; ..types: [DATE]; keys: [2026-06-20]; )", ColDate, time.Date(2026, 6, 20, 0, 0, 0, 0, loc), false, true},
		{"带引号", "keys: ['2026-06-20 00:00:00']", ColDatetime, time.Date(2026, 6, 20, 0, 0, 0, 0, loc), false, true},
		{"小数秒", "keys: [2026-06-20 00:00:00.000000]", ColDatetime, time.Date(2026, 6, 20, 0, 0, 0, 0, loc), false, true},
		{"datetime 列上界只有日期", "keys: [2026-06-20]", ColDatetime, time.Date(2026, 6, 20, 0, 0, 0, 0, loc), false, true},
		{"keys: 后空格", "keys:[2026-06-20 00:00:00]", ColDatetime, time.Date(2026, 6, 20, 0, 0, 0, 0, loc), false, true},
		{"老版本 [lo, hi) 写法", "[2026-06-19 00:00:00, 2026-06-20 00:00:00)", ColDatetime, time.Date(2026, 6, 20, 0, 0, 0, 0, loc), false, true},
		{"MAXVALUE 上界", "[types: [DATETIME]; keys: [2026-06-20 00:00:00]; ..types: [DATETIME]; keys: [MAXVALUE]; )", ColDatetime, time.Time{}, true, true},

		// 读不到 / 不可信 → ok=false（调用方视为未知，绝不删除）
		{"空文本", "", ColDatetime, time.Time{}, false, false},
		{"没有 keys", "[types: [DATETIME]; ]", ColDatetime, time.Time{}, false, false},
		{"多列分区键", "[types: [DATETIME, INT]; keys: [2026-06-19 00:00:00, 5]; ..types: [DATETIME, INT]; keys: [2026-06-20 00:00:00, 5]; )", ColDatetime, time.Time{}, false, false},
		{"字段类型对不上（bigint 配置，文本是日期）", "keys: [2026-06-20 00:00:00]", ColBigint, time.Time{}, false, false},
		{"字段类型对不上（datetime 配置，文本是毫秒）", "keys: [1781937000000]", ColDatetime, time.Time{}, false, false},
		{"秒当成毫秒（落在 1970 年）", "keys: [1781937000]", ColBigint, time.Time{}, false, false},
		{"LIST 分区的小数值", "keys: [3]", ColBigint, time.Time{}, false, false},
		{"垃圾", "keys: [abc]", ColBigint, time.Time{}, false, false},
		{"keys 为空", "keys: []", ColDatetime, time.Time{}, false, false},
	}
	for _, c := range cases {
		got, max, ok := dorisUpper(c.rng, c.colType, loc)
		if ok != c.ok || max != c.max || (ok && !max && !got.Equal(c.want)) {
			t.Errorf("%s: got (%v, max=%v, ok=%v)，期望 (%v, max=%v, ok=%v)", c.name, got, max, ok, c.want, c.max, c.ok)
		}
	}
}

func dorisCfg(col string) Config {
	cfg := baseCfg()
	cfg.InstanceID, cfg.ColType, cfg.Timezone = 6, col, "Asia/Shanghai"
	return cfg
}

func dorisState(colType string, parts ...RawPartition) TableState {
	return TableState{Exists: true, IsBase: true, Dialect: DialectDoris, ColumnType: colType, ColumnTypeFull: colType, Method: "RANGE", Partitions: parts}
}

func dorisRange(typ, lo, hi string) string {
	return "[types: [" + typ + "]; keys: [" + lo + "]; ..types: [" + typ + "]; keys: [" + hi + "]; )"
}

func TestDorisPlanAddAndDropStatements(t *testing.T) {
	cfg := dorisCfg(ColDatetime)
	cfg.PreCreate, cfg.Retain = 3, 2
	var parts []RawPartition
	for d := 1; d <= 6; d++ { // p20261001 ~ p20261006：边界到 10-07
		lo, hi := time.Date(2026, 10, d, 0, 0, 0, 0, cst), time.Date(2026, 10, d+1, 0, 0, 0, 0, cst)
		parts = append(parts, rp("p"+lo.Format("20060102"), dorisRange("DATETIME", lo.Format(dtLayout), hi.Format(dtLayout))))
	}
	p, err := BuildPlan(cfg, dorisState("datetime", parts...), time.Date(2026, 10, 6, 12, 0, 0, 0, cst))
	if err != nil {
		t.Fatal(err)
	}
	// 当前 p20261006 之后没有未来分区 → 补 p20261007、p20261008、p20261009；历史分区 5 个，保留 2 个 → 删前 3 个
	wantSQL(t, p,
		"ALTER TABLE `log`.`access_log` ADD PARTITION p20261007 VALUES [(\"2026-10-07 00:00:00\"), (\"2026-10-08 00:00:00\"))",
		"ALTER TABLE `log`.`access_log` ADD PARTITION p20261008 VALUES [(\"2026-10-08 00:00:00\"), (\"2026-10-09 00:00:00\"))",
		"ALTER TABLE `log`.`access_log` ADD PARTITION p20261009 VALUES [(\"2026-10-09 00:00:00\"), (\"2026-10-10 00:00:00\"))",
		"ALTER TABLE `log`.`access_log` DROP PARTITION p20261001",
		"ALTER TABLE `log`.`access_log` DROP PARTITION p20261002",
		"ALTER TABLE `log`.`access_log` DROP PARTITION p20261003")
	if p.Dialect != DialectDoris || p.Reorganize != nil || p.Kind != KindMaintain {
		t.Fatalf("%+v", p)
	}
	for _, s := range p.Statements() {
		if len(s.Partitions) != 1 {
			t.Fatalf("每条语句一个分区: %+v", s)
		}
	}
}

func TestDorisLiteralsByColumnType(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, cst)
	lo, hi := time.Date(2026, 10, 7, 0, 0, 0, 0, cst), time.Date(2026, 10, 8, 0, 0, 0, 0, cst)
	cases := []struct {
		col, typ string
		lo, hi   string
		want     string
	}{
		{ColBigint, "BIGINT", i64s(lo.UnixMilli()), i64s(hi.UnixMilli()), `VALUES [("` + i64s(hi.UnixMilli()) + `"), ("` + i64s(time.Date(2026, 10, 9, 0, 0, 0, 0, cst).UnixMilli()) + `"))`},
		{ColInt, "INT", i64s(lo.Unix()), i64s(hi.Unix()), `VALUES [("` + i64s(hi.Unix()) + `"), ("` + i64s(time.Date(2026, 10, 9, 0, 0, 0, 0, cst).Unix()) + `"))`},
		{ColDate, "DATE", "2026-10-07", "2026-10-08", `VALUES [("2026-10-08"), ("2026-10-09"))`},
	}
	for _, c := range cases {
		cfg := dorisCfg(c.col)
		cfg.PreCreate, cfg.Retain = 1, 0
		p, err := BuildPlan(cfg, dorisState(strings.ToLower(c.typ), rp("p20261007", dorisRange(c.typ, c.lo, c.hi))), now)
		if err != nil {
			t.Fatalf("%s: %v", c.col, err)
		}
		if len(p.Adds) != 1 || !strings.HasSuffix(p.Adds[0].SQL, c.want) || !strings.Contains(p.Adds[0].SQL, "ADD PARTITION p20261008 ") {
			t.Errorf("%s: %v 期望以 %s 结尾", c.col, sqls(p), c.want)
		}
	}
}

func i64s(n int64) string { return strconv.FormatInt(n, 10) }

// SHOW PARTITIONS 不保证按 Range 排序：乱序输入得到同样的计划。
func TestDorisPlanIgnoresInputOrder(t *testing.T) {
	cfg := dorisCfg(ColDatetime)
	cfg.PreCreate, cfg.Retain = 2, 0
	mk := func(d int) RawPartition {
		lo, hi := time.Date(2026, 10, d, 0, 0, 0, 0, cst), time.Date(2026, 10, d+1, 0, 0, 0, 0, cst)
		return rp("p"+lo.Format("20060102"), dorisRange("DATETIME", lo.Format(dtLayout), hi.Format(dtLayout)))
	}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, cst)
	a, err := BuildPlan(cfg, dorisState("datetime", mk(5), mk(6), mk(7)), now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildPlan(cfg, dorisState("datetime", mk(7), mk(5), mk(6)), now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sqls(a), "\n") != strings.Join(sqls(b), "\n") {
		t.Fatalf("乱序输入的结果不同:\n%v\n%v", sqls(a), sqls(b))
	}
	if len(a.Adds) != 1 || !strings.Contains(a.Adds[0].SQL, "p20261008") {
		t.Fatal(sqls(a))
	}
}

// 读不到上界的分区必须当作“未知”：不删除（即使它名字看上去很旧），给 warning；其它分区照常处理。
func TestDorisUnknownBoundPartitionIsNeverDropped(t *testing.T) {
	cfg := dorisCfg(ColDatetime)
	cfg.PreCreate, cfg.Retain = 1, 1
	var parts []RawPartition
	for d := 1; d <= 5; d++ {
		lo, hi := time.Date(2026, 10, d, 0, 0, 0, 0, cst), time.Date(2026, 10, d+1, 0, 0, 0, 0, cst)
		parts = append(parts, rp("p"+lo.Format("20060102"), dorisRange("DATETIME", lo.Format(dtLayout), hi.Format(dtLayout))))
	}
	parts = append(parts, rp("p_legacy", "[types: [DATETIME]; ]"), rp("p_list", "keys: [3]"))
	st := dorisState("datetime", parts...)
	p, err := BuildPlan(cfg, st, time.Date(2026, 10, 6, 12, 0, 0, 0, cst))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range p.DropNames() {
		if d == "p_legacy" || d == "p_list" {
			t.Fatalf("读不到上界的分区不得删除: %v", p.DropNames())
		}
	}
	// 已知上界的历史分区 5 个（边界都 <= 现在），保留 1 个 → 删最旧的 4 个
	if len(p.Drops) != 4 || p.Drops[0].Partitions[0] != "p20261001" {
		t.Fatalf("删除列表: %v", p.DropNames())
	}
	warned := 0
	for _, w := range p.Warnings {
		if strings.Contains(w, "p_legacy") || strings.Contains(w, "p_list") {
			warned++
		}
	}
	if warned != 2 {
		t.Fatalf("每个未知分区一条 warning: %v", p.Warnings)
	}
	unknown := 0
	for _, e := range p.Existing {
		if e.Unknown {
			unknown++
		}
	}
	if unknown != 2 {
		t.Fatalf("Existing 里应标出未知分区: %+v", p.Existing)
	}
	// 全是未知：无法续建也无法清理
	_, err = BuildPlan(cfg, dorisState("datetime", rp("p_a", "x"), rp("p_b", "y")), time.Date(2026, 10, 6, 12, 0, 0, 0, cst))
	wantCode(t, err, "no_bound")
}

func TestDorisRefusals(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, cst)
	lo, hi := "2026-10-06 00:00:00", "2026-10-07 00:00:00"
	// 未分区：不初始化，提示先建为 RANGE 分区表
	st := dorisState("datetime")
	st.Partitions, st.Method = nil, ""
	_, err := BuildPlan(dorisCfg(ColDatetime), st, now)
	wantCode(t, err, "not_partitioned")
	if !strings.Contains(err.Error(), "RANGE 分区表") {
		t.Fatal(err)
	}
	// MAXVALUE：不能追加
	_, err = BuildPlan(dorisCfg(ColDatetime), dorisState("datetime",
		rp("p1", dorisRange("DATETIME", lo, hi)), rp("pmax", dorisRange("DATETIME", hi, "MAXVALUE"))), now)
	wantCode(t, err, "max_partition")
	// 类型不符 / 字段不存在 / 表不存在 / 视图
	_, err = BuildPlan(dorisCfg(ColBigint), dorisState("datetime", rp("p1", dorisRange("DATETIME", lo, hi))), now)
	wantCode(t, err, "type_mismatch")
	_, err = BuildPlan(dorisCfg(ColDatetime), dorisState("", rp("p1", dorisRange("DATETIME", lo, hi))), now)
	wantCode(t, err, "column_missing")
	_, err = BuildPlan(dorisCfg(ColDatetime), TableState{Dialect: DialectDoris}, now)
	wantCode(t, err, "table_missing")
	v := dorisState("datetime", rp("p1", dorisRange("DATETIME", lo, hi)))
	v.IsBase = false
	_, err = BuildPlan(dorisCfg(ColDatetime), v, now)
	wantCode(t, err, "not_base_table")
	// 非法分区名
	_, err = BuildPlan(dorisCfg(ColDatetime), dorisState("datetime", rp("p`x", dorisRange("DATETIME", lo, hi))), now)
	wantCode(t, err, "bad_partition_name")
	// 分区键与配置不一致只是 warning
	st = dorisState("datetime", rp("p20261006", dorisRange("DATETIME", lo, hi)))
	st.PartitionKey = "other_col"
	cfg := dorisCfg(ColDatetime)
	cfg.PreCreate, cfg.Retain = 0, 0
	p, err := BuildPlan(cfg, st, now)
	if err != nil || len(p.Warnings) == 0 {
		t.Fatalf("%+v %v", p, err)
	}
	// int 超 2038 对 Doris 也拒绝
	_, err = BuildPlan(func() Config { c := dorisCfg(ColInt); c.PreCreate = 100000; return c }(), dorisState("int"), now)
	if err == nil {
		t.Fatal("空分区表应拒绝")
	}
}

// ---- 服务层：预览 / 状态 / 执行 / 定时 / 确认 / 概览对 Doris 方言工作 ----

func (e *env) dorisTable(name string, days ...int) {
	st := &TableState{Exists: true, IsBase: true, ColumnType: "datetime", ColumnTypeFull: "datetime", Method: "RANGE"}
	for _, d := range days {
		lo, hi := time.Date(2026, 10, d, 0, 0, 0, 0, cst), time.Date(2026, 10, d+1, 0, 0, 0, 0, cst)
		st.Partitions = append(st.Partitions, rp("p"+lo.Format("20060102"), dorisRange("DATETIME", lo.Format(dtLayout), hi.Format(dtLayout))))
	}
	e.tgt.tables["log."+name] = st
}

func dorisInput(tb string) ConfigInput {
	in := goodInput()
	in.InstanceID, in.TbName = 6, tb
	return in
}

func TestDorisServiceFlow(t *testing.T) {
	e := newEnv()
	// 概览：selectdb / doris 都 supported，方言 doris，不能初始化
	ov, err := e.svc.Overview(root)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]InstanceView{}
	for _, iv := range ov.Instances {
		seen[iv.DBType] = iv
	}
	for _, ty := range []string{"selectdb", "doris"} {
		if iv := seen[ty]; !iv.Supported || iv.Dialect != DialectDoris || iv.CanInit {
			t.Errorf("%s: %+v", ty, iv)
		}
	}
	if iv := seen["mysql"]; !iv.Supported || iv.Dialect != DialectMySQL || !iv.CanInit {
		t.Errorf("mysql: %+v", iv)
	}
	if iv := seen["mssql"]; iv.Supported || iv.Dialect != "" {
		t.Errorf("mssql: %+v", iv)
	}

	// 新增配置（Doris 实例可选）
	in := dorisInput("events")
	in.Retain, in.PreCreate = 2, 2
	cv, err := e.svc.Create(root, in)
	if err != nil || !cv.Supported || cv.Dialect != DialectDoris || cv.CanInit {
		t.Fatalf("%+v %v", cv, err)
	}
	e.dorisTable("events", 1, 2, 3, 4, 5, 6) // 边界到 10-07；现在是 10-06 12:00

	// 预览
	pv, err := e.svc.Preview(ctx, root, cv.ID, nil, nil)
	if err != nil || pv.Dialect != DialectDoris || pv.Kind != KindMaintain || len(pv.Statements) != 2+3 {
		t.Fatalf("%+v %v", pv, err)
	}
	if pv.Statements[0].Kind != "add" || !strings.Contains(pv.Statements[0].SQL, "ADD PARTITION p20261007 VALUES [") {
		t.Fatalf("%+v", pv.Statements)
	}
	if len(pv.Drops) != 3 {
		t.Fatalf("历史 5 个保留 2 个应删 3 个: %v", pv.Drops)
	}
	// 状态
	st := statusOf(t, e, cv.ID)
	if st.State != StatePending || st.Adds != 2 || st.Drops != 3 || st.Dialect != DialectDoris {
		t.Fatalf("%+v", st)
	}
	// 执行：DROP 需要二次确认（服务端重算）
	if _, err := e.svc.Run(ctx, root, cv.ID, RunOptions{}); code(err) != "confirm_required" {
		t.Fatalf("%v", err)
	}
	if len(e.tgt.executed) != 0 {
		t.Fatal("没确认不应执行")
	}
	res, err := e.svc.Run(ctx, root, cv.ID, RunOptions{ConfirmDrops: pv.Drops})
	if err != nil || res.Failed || len(res.Statements) != 5 {
		t.Fatalf("%+v %v", res, err)
	}
	if got := e.tgt.names("log.events"); got != "p20261004,p20261005,p20261006,p20261007,p20261008" {
		t.Fatalf("目标表分区: %s", got)
	}
	if len(e.repo.audits) != 2 || !strings.Contains(e.repo.audits[1].ExtraInfo, "VALUES [(") {
		t.Fatalf("审计应含完整语句: %+v", e.repo.audits)
	}
	// 再预览：幂等（新增的分区被识别，什么都不做）
	pv, err = e.svc.Preview(ctx, root, cv.ID, nil, nil)
	if err != nil || len(pv.Statements) != 0 {
		t.Fatalf("%+v %v", pv, err)
	}
}

func TestDorisAutoAddsAndDropsOnlyWhenEnabled(t *testing.T) {
	e := newEnv()
	e.enableAuto(false)
	in := dorisInput("events")
	in.Retain, in.PreCreate = 2, 2
	e.addConfig(in)
	e.dorisTable("events", 1, 2, 3, 4, 5, 6)
	sum := e.svc.RunAuto(ctx)
	if sum.Executed != 1 || len(e.tgt.executed) != 2 {
		t.Fatalf("auto_drop 关闭只续建: %+v %v", sum, e.tgt.executed)
	}
	for _, s := range e.tgt.executed {
		if strings.Contains(s, "DROP") {
			t.Fatalf("不应删除: %v", e.tgt.executed)
		}
	}
	e.src.cfg[cfgAutoDrop] = "true"
	e.repo.logs = nil // 清掉保护窗口
	sum = e.svc.RunAuto(ctx)
	if sum.Executed != 1 || len(e.tgt.executed) != 5 {
		t.Fatalf("%+v %v", sum, e.tgt.executed)
	}
}

// 变异检查的“活体”：读不到上界的 Doris 分区，在服务层也不会出现在 DROP 里。
func TestDorisUnknownPartitionNotDroppedViaRun(t *testing.T) {
	e := newEnv()
	in := dorisInput("events")
	in.Retain, in.PreCreate = 1, 0
	cv, err := e.svc.Create(root, in)
	if err != nil {
		t.Fatal(err)
	}
	e.dorisTable("events", 1, 2, 3, 4)
	e.tgt.tables["log.events"].Partitions = append(e.tgt.tables["log.events"].Partitions, rp("p_old_weird", "[types: [DATETIME]; ]"))
	pv, err := e.svc.Preview(ctx, root, cv.ID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range pv.Drops {
		if d == "p_old_weird" {
			t.Fatalf("未知分区不得删除: %v", pv.Drops)
		}
	}
	res, err := e.svc.Run(ctx, root, cv.ID, RunOptions{ConfirmDrops: pv.Drops})
	if err != nil || res.Failed {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(e.tgt.names("log.events"), "p_old_weird") {
		t.Fatalf("未知分区被删了: %s", e.tgt.names("log.events"))
	}
	var unknown bool
	for _, w := range pv.Warnings {
		unknown = unknown || strings.Contains(w, "p_old_weird")
	}
	if !unknown {
		t.Fatalf("应有 warning: %v", pv.Warnings)
	}
}

func TestDorisUnpartitionedIsErrorNotInit(t *testing.T) {
	e := newEnv()
	cv, err := e.svc.Create(root, dorisInput("plain"))
	if err != nil {
		t.Fatal(err)
	}
	e.tgt.tables["log.plain"] = &TableState{Exists: true, IsBase: true, ColumnType: "datetime", ColumnTypeFull: "datetime"}
	if _, err := e.svc.Preview(ctx, root, cv.ID, nil, &InitRequest{StartMode: StartCurrent}); code(err) != "not_partitioned" {
		t.Fatalf("%v", err)
	}
	if st := statusOf(t, e, cv.ID); st.State != StateError || !strings.Contains(st.Message, "RANGE 分区表") {
		t.Fatalf("%+v", st)
	}
	if _, err := e.svc.Run(ctx, root, cv.ID, RunOptions{Init: &InitRequest{}, ConfirmInit: &ConfirmInit{Table: "log.plain"}}); code(err) != "not_partitioned" {
		t.Fatalf("%v", err)
	}
	if len(e.tgt.executed) != 0 || len(e.repo.audits) != 1 { // 只有“新增配置”的审计
		t.Fatalf("不应执行: %v %v", e.tgt.executed, e.repo.auditActions())
	}
}

func TestDorisTargetHelpers(t *testing.T) {
	for in, want := range map[string]string{"datetimev2": "datetime", "DATEV2": "date", "bigint": "bigint", " int ": "int", "datetime": "datetime"} {
		if got := normDorisType(in); got != want {
			t.Errorf("%q → %q，期望 %q", in, got, want)
		}
		if got := partColType(in); got != want {
			t.Errorf("partColType(%q) = %q", in, got)
		}
	}
	if !isNotPartitionedErr(errString("errCode = 2, detailMessage = Table[t] is not a partitioned table")) || isNotPartitionedErr(errString("connection refused")) {
		t.Error("isNotPartitionedErr 判断不对")
	}
	if dorisName("p20261007") != "p20261007" || dorisName("p$x") != "`p$x`" {
		t.Errorf("%s %s", dorisName("p20261007"), dorisName("p$x"))
	}
}

type errString string

func (e errString) Error() string { return string(e) }
