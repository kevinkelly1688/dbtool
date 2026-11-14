// @author: kevinkelly
// @license: Apache Licence
// @file: init_test.go
// @time: 2026/10/28

package partman

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// plainTable 是一张可以初始化的未分区 InnoDB 表（主键含分区字段）。
func plainTable(colType string) TableState {
	return TableState{Exists: true, IsBase: true, Dialect: DialectMySQL, ColumnType: colType, ColumnTypeFull: colType, Engine: "InnoDB",
		UniqueKeys: []UniqueKey{{Name: "PRIMARY", Columns: []string{"id", "created_at"}}}}
}

func initCfg(col, period string, interval, pre int) Config {
	cfg := baseCfg()
	cfg.ColType, cfg.Period, cfg.Interval, cfg.PreCreate, cfg.Retain = col, period, interval, pre, 0
	return cfg
}

var initNow = time.Date(2026, 10, 6, 12, 0, 0, 0, cst)

func TestInitPlanStatementShapePerColumnType(t *testing.T) {
	ms := func(d int) string {
		return strconv.FormatInt(time.Date(2026, 10, d, 0, 0, 0, 0, cst).UnixMilli(), 10)
	}
	sec := func(d int) string { return strconv.FormatInt(time.Date(2026, 10, d, 0, 0, 0, 0, cst).Unix(), 10) }
	cases := []struct {
		col, typ, want string
	}{
		{ColDatetime, "datetime", "ALTER TABLE `log`.`access_log` PARTITION BY RANGE COLUMNS(`created_at`) (PARTITION `p20261006` VALUES LESS THAN ('2026-10-07 00:00:00'), PARTITION `p20261007` VALUES LESS THAN ('2026-10-08 00:00:00'), PARTITION `p20261008` VALUES LESS THAN ('2026-10-09 00:00:00'))"},
		{ColDate, "date", "ALTER TABLE `log`.`access_log` PARTITION BY RANGE COLUMNS(`created_at`) (PARTITION `p20261006` VALUES LESS THAN ('2026-10-07'), PARTITION `p20261007` VALUES LESS THAN ('2026-10-08'), PARTITION `p20261008` VALUES LESS THAN ('2026-10-09'))"},
		{ColBigint, "bigint", "ALTER TABLE `log`.`access_log` PARTITION BY RANGE (`created_at`) (PARTITION `p20261006` VALUES LESS THAN (" + ms(7) + "), PARTITION `p20261007` VALUES LESS THAN (" + ms(8) + "), PARTITION `p20261008` VALUES LESS THAN (" + ms(9) + "))"},
		{ColInt, "int", "ALTER TABLE `log`.`access_log` PARTITION BY RANGE (`created_at`) (PARTITION `p20261006` VALUES LESS THAN (" + sec(7) + "), PARTITION `p20261007` VALUES LESS THAN (" + sec(8) + "), PARTITION `p20261008` VALUES LESS THAN (" + sec(9) + "))"},
	}
	for _, c := range cases {
		p, err := BuildPlan(initCfg(c.col, "day", 1, 2), plainTable(c.typ), initNow)
		if err != nil {
			t.Fatalf("%s: %v", c.col, err)
		}
		if p.Kind != KindInit || p.Init == nil || !p.Init.CanExecute || p.InitStmt == nil {
			t.Fatalf("%s: %+v", c.col, p)
		}
		wantSQL(t, p, c.want)
		st := p.Statements()[0]
		if st.Kind != "init" || !st.Danger || !st.Heavy || len(st.Partitions) != 3 {
			t.Errorf("%s: %+v", c.col, st)
		}
		if strings.Contains(strings.ToUpper(st.SQL), "MAXVALUE") {
			t.Errorf("%s: 初始化语句不应带 MAXVALUE: %s", c.col, st.SQL)
		}
		if p.Init.First != "p20261006" || p.Init.Last != "p20261008" || p.Init.PartitionCount != 3 {
			t.Errorf("%s: %+v", c.col, p.Init)
		}
	}
}

// 初始化按原程序的网格对齐（与续建“从最后边界接着建”不同）。期望值来自 grid_golden_test.go 同一输入。
func TestInitAlignsToOriginalGrid(t *testing.T) {
	now := time.Date(2026, 6, 17, 13, 45, 1, 0, cst)
	cases := []struct {
		period   string
		n        int
		wantName string
		wantSQL  string
	}{
		{"day", 7, "p20260611", "VALUES LESS THAN ('2026-06-18 00:00:00')"},        // 1970-01-01 起算 7 天一个窗口
		{"week", 1, "p20260621", "VALUES LESS THAN ('2026-06-22 00:00:00')"},       // 窗口最后一天命名
		{"month", 1, "p202606", "VALUES LESS THAN ('2026-07-01 00:00:00')"},        //
		{"hour", 6, "p2026061712", "VALUES LESS THAN ('2026-06-17 18:00:00')"},     // 日内取整
		{"minute", 7, "p202606171339", "VALUES LESS THAN ('2026-06-17 13:46:00')"}, // 日内按分钟数取整（不是对齐到整点）
		{"year", 5, "p2025", "VALUES LESS THAN ('2030-01-01 00:00:00')"},           // 公元年的 5 的倍数
		{"month", 5, "p202604", "VALUES LESS THAN ('2026-09-01 00:00:00')"},        // 1970-01 起算每 5 个月
	}
	for _, c := range cases {
		p, err := BuildPlan(initCfg(ColDatetime, c.period, c.n, 0), plainTable("datetime"), now)
		if err != nil {
			t.Fatalf("%s×%d: %v", c.period, c.n, err)
		}
		if p.Init.First != c.wantName || !strings.Contains(p.InitStmt.SQL, c.wantSQL) || p.Init.PartitionCount != 1 {
			t.Errorf("%s×%d: %s %s", c.period, c.n, p.Init.First, p.InitStmt.SQL)
		}
	}
	// 对照 golden 表里同一输入（原程序输出）：month×5 在 2026-06-17 的窗口起点
	for _, g := range gridGolden {
		if g.tz == "Asia/Shanghai" && g.now == "2026-06-17 13:45:01" && g.period == "month" && g.interval == 5 && g.start != "2026-04-01 00:00:00" {
			t.Errorf("golden 与本测试的期望不一致: %+v", g)
		}
	}
}

func TestInitStartModes(t *testing.T) {
	cfg := initCfg(ColDatetime, "day", 1, 1)
	// 默认 / current：从当前窗口开始
	p, err := BuildPlan(cfg, plainTable("datetime"), initNow)
	if err != nil || p.Init.StartMode != StartCurrent || p.Init.First != "p20261006" || p.Init.PartitionCount != 2 {
		t.Fatalf("%+v %v", p.Init, err)
	}
	// 当前窗口方式要提示：旧数据都进第一个分区；以及没有 MAXVALUE 兜底
	joined := strings.Join(p.Warnings, "\n")
	if !strings.Contains(joined, "第一个分区 p20261006") || !strings.Contains(joined, "MAXVALUE") {
		t.Fatalf("warnings: %v", p.Warnings)
	}
	// earliest：从最早数据所在窗口开始
	opt := InitOptions{StartMode: StartEarliest, EarliestRead: true, HasEarliest: true, Earliest: time.Date(2026, 10, 1, 13, 30, 0, 0, cst)}
	p, err = BuildPlanWith(cfg, plainTable("datetime"), initNow, opt)
	if err != nil || p.Init.First != "p20261001" || p.Init.PartitionCount != 7 || p.Init.Earliest.IsZero() {
		t.Fatalf("%+v %v", p.Init, err)
	}
	for _, w := range p.Warnings {
		if strings.Contains(w, "落进第一个分区") {
			t.Fatalf("从最早数据开始不应有“旧数据都进第一个分区”的提示: %v", p.Warnings)
		}
	}
	// earliest 但表是空的：按当前窗口，并说明
	p, err = BuildPlanWith(cfg, plainTable("datetime"), initNow, InitOptions{StartMode: StartEarliest, EarliestRead: true})
	if err != nil || p.Init.First != "p20261006" || len(p.Init.Notes) != 1 {
		t.Fatalf("%+v %v", p.Init, err)
	}
	// earliest 的数据比当前窗口还新（未来时间）：不晚于当前窗口
	p, err = BuildPlanWith(cfg, plainTable("datetime"), initNow, InitOptions{StartMode: StartEarliest, EarliestRead: true, HasEarliest: true, Earliest: time.Date(2027, 1, 1, 0, 0, 0, 0, cst)})
	if err != nil || p.Init.First != "p20261006" {
		t.Fatalf("%+v %v", p.Init, err)
	}
	// 没读到最早数据却要求 earliest：拒绝（服务层读失败时也是这个码）
	_, err = BuildPlanWith(cfg, plainTable("datetime"), initNow, InitOptions{StartMode: StartEarliest})
	wantCode(t, err, "earliest_unavailable")
	// date：指定起始日期，向下对齐到网格
	p, err = BuildPlanWith(cfg, plainTable("datetime"), initNow, InitOptions{StartMode: StartDate, StartDate: "2026-10-03 15:00:00"})
	if err != nil || p.Init.First != "p20261003" || p.Init.PartitionCount != 5 {
		t.Fatalf("%+v %v", p.Init, err)
	}
	wk := initCfg(ColDatetime, "week", 1, 0)
	p, err = BuildPlanWith(wk, plainTable("datetime"), initNow, InitOptions{StartMode: StartDate, StartDate: "2026-09-30"}) // 周三 → 周一 9-28
	if err != nil || !strings.Contains(p.InitStmt.SQL, "PARTITION `p20261004` VALUES LESS THAN ('2026-10-05 00:00:00')") || p.Init.PartitionCount != 2 {
		t.Fatalf("%v %v", p.Init, err)
	}
	// 非法
	for _, o := range []InitOptions{{StartMode: StartDate, StartDate: "abc"}, {StartMode: StartDate, StartDate: "2026-10-09"}, {StartMode: "latest"}} {
		_, err = BuildPlanWith(cfg, plainTable("datetime"), initNow, o)
		wantCode(t, err, "bad_init_option")
	}
}

func checkLevel(p *Plan, code string) string {
	for _, c := range p.Init.Checks {
		if c.Code == code {
			return c.Level
		}
	}
	return "(无)"
}

func TestInitPrechecks(t *testing.T) {
	cfg := initCfg(ColDatetime, "day", 1, 2)
	cases := []struct {
		name string
		mod  func(s *TableState)
		code string
		lvl  string
	}{
		{"引擎不支持分区", func(s *TableState) { s.Engine = "CSV" }, "engine", "error"},
		{"引擎 MyISAM 支持", func(s *TableState) { s.Engine = "MyISAM" }, "engine", "ok"},
		{"没读到引擎只提示", func(s *TableState) { s.Engine = "" }, "engine", "warn"},
		{"有外键", func(s *TableState) { s.ForeignKeys = []string{"外键 fk_a：本表引用 parent"} }, "foreign_key", "error"},
		{"被外键引用", func(s *TableState) { s.ForeignKeys = []string{"外键 fk_b：被表 child 引用"} }, "foreign_key", "error"},
		{"分区字段不存在", func(s *TableState) { s.ColumnType, s.ColumnTypeFull = "", "" }, "column", "error"},
		{"字段类型与配置不一致", func(s *TableState) { s.ColumnType = "bigint" }, "column_type", "error"},
		{"主键不含分区字段", func(s *TableState) { s.UniqueKeys = []UniqueKey{{"PRIMARY", []string{"id"}}} }, "unique_keys", "error"},
		{"唯一索引不含分区字段", func(s *TableState) {
			s.UniqueKeys = []UniqueKey{{"PRIMARY", []string{"id", "created_at"}}, {"uk_order", []string{"order_no"}}}
		}, "unique_keys", "error"},
		{"函数索引无法证明包含", func(s *TableState) { s.UniqueKeys = []UniqueKey{{"uk_fn", []string{"(表达式)"}}} }, "unique_keys", "error"},
		{"没有唯一键", func(s *TableState) { s.UniqueKeys = nil }, "unique_keys", "ok"},
		{"分区字段大小写不同", func(s *TableState) { s.UniqueKeys = []UniqueKey{{"PRIMARY", []string{"ID", "Created_At"}}} }, "unique_keys", "ok"},
	}
	for _, c := range cases {
		st := plainTable("datetime")
		c.mod(&st)
		p, err := BuildPlan(cfg, st, initNow)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := checkLevel(p, c.code); got != c.lvl {
			t.Errorf("%s: %s 检查结果 %s，期望 %s（%+v）", c.name, c.code, got, c.lvl, p.Init.Checks)
		}
		if (c.lvl == "error") == p.Init.CanExecute {
			t.Errorf("%s: CanExecute=%v", c.name, p.Init.CanExecute)
		}
		if c.lvl == "error" && (p.InitStmt != nil || len(p.Statements()) != 0) {
			t.Errorf("%s: 前置检查不通过不得生成语句: %v", c.name, sqls(p))
		}
	}
	// 分区数上限：5000 为 error，超过 200 为 warn
	over := initCfg(ColDatetime, "minute", 1, 2)
	p, err := BuildPlanWith(over, plainTable("datetime"), initNow, InitOptions{StartMode: StartDate, StartDate: "2026-01-01"})
	if err != nil || checkLevel(p, "partition_count") != "error" || p.Init.CanExecute || p.InitStmt != nil {
		t.Fatalf("%+v %v", p.Init, err)
	}
	warn := initCfg(ColDatetime, "day", 1, 2)
	p, err = BuildPlanWith(warn, plainTable("datetime"), initNow, InitOptions{StartMode: StartDate, StartDate: "2026-02-01"})
	if err != nil || checkLevel(p, "partition_count") != "warn" || !p.Init.CanExecute || p.Init.PartitionCount < 201 {
		t.Fatalf("%+v %v", p.Init, err)
	}
	// 恰好 5000 个可以，5001 个不行（边界）
	exact := initCfg(ColDatetime, "minute", 1, 0)
	p, err = BuildPlanWith(exact, plainTable("datetime"), time.Date(2026, 10, 6, 12, 0, 0, 0, cst),
		InitOptions{StartMode: StartDate, StartDate: time.Date(2026, 10, 6, 12, 0, 0, 0, cst).Add(-4999 * time.Minute).Format(dtLayout)})
	if err != nil || p.Init.PartitionCount != 5000 || !p.Init.CanExecute {
		t.Fatalf("%+v %v", p.Init, err)
	}
	p, err = BuildPlanWith(exact, plainTable("datetime"), time.Date(2026, 10, 6, 12, 0, 0, 0, cst),
		InitOptions{StartMode: StartDate, StartDate: time.Date(2026, 10, 6, 12, 0, 0, 0, cst).Add(-5000 * time.Minute).Format(dtLayout)})
	if err != nil || p.Init.CanExecute {
		t.Fatalf("5001 个应拒绝: %+v %v", p.Init, err)
	}
	// int 字段的上界超过 2038 → 前置失败而不是生成坏语句
	ic := initCfg(ColInt, "year", 1, 20)
	p, err = BuildPlan(ic, plainTable("int"), initNow)
	if err != nil || p.Init.CanExecute || p.InitStmt != nil {
		t.Fatalf("%+v %v", p.Init, err)
	}
	// 视图 / 不存在：仍然是原来的拒绝，不进入 init
	st := plainTable("datetime")
	st.IsBase = false
	_, err = BuildPlan(cfg, st, initNow)
	wantCode(t, err, "not_base_table")
}

// ---- 服务层 ----

func (e *env) plain(name string) *TableState {
	st := plainTable("datetime")
	e.tgt.tables["log."+name] = &st
	return &st
}

func initInput(tb string) ConfigInput {
	in := goodInput()
	in.TbName, in.Retain, in.PreCreate = tb, 0, 2
	return in
}

func TestInitPreviewAndRunFlow(t *testing.T) {
	e := newEnv()
	e.plain("fresh")
	cfg := e.addConfig(initInput("fresh"))

	pv, err := e.svc.Preview(ctx, root, cfg.ID, nil, nil)
	if err != nil || pv.Kind != KindInit || pv.Init == nil || !pv.Init.CanExecute || len(pv.Statements) != 1 || !pv.Statements[0].Danger || !pv.Statements[0].Heavy {
		t.Fatalf("%+v %v", pv, err)
	}
	if pv.Init.Confirm.Table != "log.fresh" || pv.Init.Confirm.PartitionCount != 3 || pv.Init.FirstPartition != "p20261006" {
		t.Fatalf("%+v", pv.Init)
	}
	if len(e.tgt.executed) != 0 || len(e.repo.audits) != 0 || len(e.repo.logs) != 0 {
		t.Fatal("预览是只读的")
	}
	good := pv.Init.Confirm

	// 没有确认值 / 没有键入库.表 / 数字不一致：409 confirm_required，且带回重算后的计划，目标表不变
	for name, ci := range map[string]*ConfirmInit{
		"无确认":   nil,
		"表名没键入": {Table: "", PartitionCount: good.PartitionCount, FirstPartition: good.FirstPartition, LastPartition: good.LastPartition},
		"表名不对":  {Table: "log.other", PartitionCount: good.PartitionCount, FirstPartition: good.FirstPartition, LastPartition: good.LastPartition},
		"个数不对":  {Table: good.Table, PartitionCount: good.PartitionCount + 1, FirstPartition: good.FirstPartition, LastPartition: good.LastPartition},
		"首分区不对": {Table: good.Table, PartitionCount: good.PartitionCount, FirstPartition: "p20260101", LastPartition: good.LastPartition},
		"末分区不对": {Table: good.Table, PartitionCount: good.PartitionCount, FirstPartition: good.FirstPartition, LastPartition: "p20270101"},
	} {
		_, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{ConfirmInit: ci})
		var pe *Error
		if code(err) != "confirm_required" || !asErr(err, &pe) || pe.Data == nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(e.tgt.executed) != 0 || len(e.repo.audits) != 0 {
		t.Fatal("确认不通过不得执行、不得写审计")
	}
	// 没有 partman.delete（与 DROP 的规则一致）：403
	if _, err := e.svc.Run(ctx, user(permMenu, btnRun), cfg.ID, RunOptions{ConfirmInit: &good}); code(err) != "forbidden" {
		t.Fatalf("%v", err)
	}
	// 没有执行权限（partman.run）：403
	if _, err := e.svc.Run(ctx, user(permMenu, permDelete), cfg.ID, RunOptions{ConfirmInit: &good}); code(err) != "forbidden" {
		t.Fatalf("%v", err)
	}
	// 审计写不进去：不执行
	e.repo.failAudit = true
	if _, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{ConfirmInit: &good}); code(err) != "audit_failed" {
		t.Fatalf("%v", err)
	}
	e.repo.failAudit = false
	if len(e.tgt.executed) != 0 {
		t.Fatal("审计先于执行")
	}

	res, err := e.svc.Run(ctx, user(permMenu, btnRun, permDelete), cfg.ID, RunOptions{ConfirmInit: &good})
	if err != nil || res.Failed || len(res.Statements) != 1 || res.Statements[0].Kind != "init" {
		t.Fatalf("%+v %v", res, err)
	}
	if len(e.repo.audits) != 1 || e.repo.audits[0].Action != "分区维护-执行初始化" || !strings.Contains(e.repo.audits[0].ExtraInfo, "PARTITION BY RANGE COLUMNS(`created_at`)") {
		t.Fatalf("审计: %+v", e.repo.audits)
	}
	if got := e.tgt.names("log.fresh"); got != "p20261006,p20261007,p20261008" {
		t.Fatalf("目标表分区: %s", got)
	}
	// 初始化后就是普通的已分区表：再预览是 maintain，什么都不用做
	pv, err = e.svc.Preview(ctx, root, cfg.ID, nil, nil)
	if err != nil || pv.Kind != KindMaintain || pv.Init != nil || len(pv.Statements) != 0 {
		t.Fatalf("%+v %v", pv, err)
	}
	// 日志里有 kind=init 的项与运行输出
	logs, _ := e.svc.Logs(root, cfg.ID, 5)
	if len(logs) != 1 || logs[0].Status != LogSuccess || logs[0].OutputLines == 0 {
		t.Fatalf("%+v", logs)
	}
	// 权限与 DROP 一致：用 root 也行；分区维护按钮缺失时拒绝
	e.plain("fresh2")
	c2 := e.addConfig(initInput("fresh2"))
	pv2, _ := e.svc.Preview(ctx, root, c2.ID, nil, nil)
	if _, err := e.svc.Run(ctx, root, c2.ID, RunOptions{ConfirmInit: &pv2.Init.Confirm}); err != nil {
		t.Fatal(err)
	}
}

func asErr(err error, out **Error) bool {
	pe, ok := err.(*Error)
	if ok {
		*out = pe
	}
	return ok
}

// 服务端重新读取并重算：预览之后表（这里是最早数据）变了，旧的确认值作废。
func TestInitConfirmIsRecomputedOnRun(t *testing.T) {
	e := newEnv()
	e.plain("t")
	cfg := e.addConfig(initInput("t"))
	min1 := time.Date(2026, 10, 3, 8, 0, 0, 0, cst)
	e.tgt.minVal = &min1
	req := &InitRequest{StartMode: StartEarliest}
	pv, err := e.svc.Preview(ctx, root, cfg.ID, nil, req)
	if err != nil || pv.Init.FirstPartition != "p20261003" || pv.Init.EarliestData == "" || pv.Init.PartitionCount != 6 {
		t.Fatalf("%+v %v", pv.Init, err)
	}
	min2 := time.Date(2026, 9, 20, 8, 0, 0, 0, cst) // 预览之后有人插入了更早的数据
	e.tgt.minVal = &min2
	_, err = e.svc.Run(ctx, root, cfg.ID, RunOptions{Init: req, ConfirmInit: &pv.Init.Confirm})
	var pe *Error
	if code(err) != "confirm_required" || !asErr(err, &pe) {
		t.Fatalf("%v", err)
	}
	if nv, ok := pe.Data.(*PlanView); !ok || nv.Init.FirstPartition != "p20260920" {
		t.Fatalf("应带回重算后的计划: %+v", pe.Data)
	}
	if len(e.tgt.executed) != 0 {
		t.Fatal("不应执行")
	}
	// 用重算后的确认值：通过
	nv := pe.Data.(*PlanView)
	if _, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{Init: req, ConfirmInit: &nv.Init.Confirm}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.tgt.names("log.t"), "p20260920") {
		t.Fatalf("%s", e.tgt.names("log.t"))
	}
}

func TestInitEarliestReadOnlyWhenRequestedAndFailsClosed(t *testing.T) {
	e := newEnv()
	e.plain("t")
	cfg := e.addConfig(initInput("t"))
	// current（默认）不读 MIN：不对全表做扫描
	if _, err := e.svc.Preview(ctx, root, cfg.ID, nil, nil); err != nil || e.tgt.minCalls != 0 {
		t.Fatalf("%v calls=%d", err, e.tgt.minCalls)
	}
	// date 也不读
	if _, err := e.svc.Preview(ctx, root, cfg.ID, nil, &InitRequest{StartMode: StartDate, StartDate: "2026-10-01"}); err != nil || e.tgt.minCalls != 0 {
		t.Fatalf("%v calls=%d", err, e.tgt.minCalls)
	}
	// earliest：读；失败 / 超时 → 拒绝并让用户手填起始日期，不生成计划
	e.tgt.minErr = context_deadline{}
	_, err := e.svc.Preview(ctx, root, cfg.ID, nil, &InitRequest{StartMode: StartEarliest})
	if code(err) != "earliest_unavailable" || !strings.Contains(err.Error(), "指定起始日期") || e.tgt.minCalls != 1 {
		t.Fatalf("%v calls=%d", err, e.tgt.minCalls)
	}
	_, err = e.svc.Run(ctx, root, cfg.ID, RunOptions{Init: &InitRequest{StartMode: StartEarliest}, ConfirmInit: &ConfirmInit{Table: "log.t"}})
	if code(err) != "earliest_unavailable" || len(e.tgt.executed) != 0 {
		t.Fatalf("%v", err)
	}
	// 前置检查已经不通过（字段类型不对）时不去扫表
	e.tgt.minErr = nil
	e.tgt.minCalls = 0
	e.tgt.tables["log.t"].ColumnType = "bigint"
	pv, err := e.svc.Preview(ctx, root, cfg.ID, nil, &InitRequest{StartMode: StartEarliest})
	if e.tgt.minCalls != 0 {
		t.Fatalf("字段类型不对时不应读 MIN: calls=%d", e.tgt.minCalls)
	}
	// 没读到最早数据 → earliest_unavailable（计划拒绝）；或者是不可执行的 init 计划，两者都不会生成语句
	if err == nil && (pv.Init.CanExecute || len(pv.Statements) != 0) {
		t.Fatalf("%+v", pv)
	}
}

type context_deadline struct{}

func (context_deadline) Error() string { return "context deadline exceeded" }

func TestInitPrecheckFailureRefusesRun(t *testing.T) {
	e := newEnv()
	st := e.plain("t")
	st.UniqueKeys = []UniqueKey{{"PRIMARY", []string{"id"}}}
	cfg := e.addConfig(initInput("t"))
	pv, err := e.svc.Preview(ctx, root, cfg.ID, nil, nil)
	if err != nil || pv.Init.CanExecute || len(pv.Statements) != 0 || pv.Init.Checks == nil {
		t.Fatalf("%+v %v", pv, err)
	}
	var found bool
	for _, c := range pv.Init.Checks {
		found = found || (c.Code == "unique_keys" && c.Level == "error" && strings.Contains(c.Message, "PRIMARY"))
	}
	if !found {
		t.Fatalf("%+v", pv.Init.Checks)
	}
	_, err = e.svc.Run(ctx, root, cfg.ID, RunOptions{ConfirmInit: &ConfirmInit{Table: "log.t", PartitionCount: 3, FirstPartition: "p20261006", LastPartition: "p20261008"}})
	if code(err) != "init_precheck_failed" {
		t.Fatalf("%v", err)
	}
	if len(e.tgt.executed) != 0 || len(e.repo.audits) != 0 {
		t.Fatal("前置检查不过：不发 ALTER、不写审计")
	}
	// 状态：未分区但不能初始化
	sv := statusOf(t, e, cfg.ID)
	if sv.State != StateUnpartitioned || sv.CanInit || !strings.Contains(sv.Message, "不能初始化") {
		t.Fatalf("%+v", sv)
	}
}

func TestInitStatusUnpartitionedCanInit(t *testing.T) {
	e := newEnv()
	e.plain("t")
	cfg := e.addConfig(initInput("t"))
	sv := statusOf(t, e, cfg.ID)
	if sv.State != StateUnpartitioned || !sv.CanInit || sv.Adds != 0 || sv.Drops != 0 || sv.Dialect != DialectMySQL {
		t.Fatalf("%+v", sv)
	}
}

// 定时维护永远不初始化：未分区的表只记日志提示，不执行、不写审计、不写维护日志。
func TestAutoNeverInitializesUnpartitionedTable(t *testing.T) {
	e := newEnv()
	e.enableAuto(true) // 连自动删除都开着也一样
	e.plain("fresh")
	e.addConfig(initInput("fresh"))
	sum := e.svc.RunAuto(ctx)
	if sum.Checked != 1 || sum.Skipped != 1 || sum.Executed != 0 || sum.Failed != 0 {
		t.Fatalf("%+v", sum)
	}
	if len(e.tgt.executed) != 0 {
		t.Fatalf("定时维护不得初始化: %v", e.tgt.executed)
	}
	if len(e.repo.audits) != 0 {
		t.Fatalf("不应写审计: %v", e.repo.auditActions())
	}
	for _, l := range e.repo.logs {
		if l.CnfID != 0 {
			t.Fatalf("不应给该配置写维护日志: %+v", l)
		}
	}
	if st := e.tgt.tables["log.fresh"]; len(st.Partitions) != 0 {
		t.Fatalf("表被改了: %+v", st)
	}
	// 该轮摘要里能看到原因
	var round string
	for _, l := range e.repo.logs {
		if l.CnfID == 0 {
			round = l.ExecResult
		}
	}
	if !strings.Contains(round, "表未分区：定时维护不会初始化") {
		t.Fatalf("轮次摘要里应说明跳过原因: %s", round)
	}
}
