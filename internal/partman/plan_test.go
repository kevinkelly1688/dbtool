// @author: kevinkelly
// @license: Apache Licence
// @file: plan_test.go
// @time: 2026/10/28

package partman

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var cst = time.FixedZone("CST", 8*3600)

func baseCfg() Config {
	return Config{ID: 1, CnfID: 1, InstanceID: 1, DBName: "log", TbName: "access_log", Column: "created_at", ColType: ColDatetime,
		Period: "day", Interval: 1, Retain: 30, PreCreate: 3, Timezone: "Asia/Shanghai", IsEnable: true}
}

func dtState(parts ...RawPartition) TableState {
	return TableState{Exists: true, IsBase: true, ColumnType: "datetime", ColumnTypeFull: "datetime", Method: "RANGE COLUMNS", Expression: "`created_at`", Partitions: parts}
}

func rp(name, desc string) RawPartition { return RawPartition{Name: name, Desc: desc} }

func sqls(p *Plan) []string {
	var out []string
	for _, s := range p.Statements() {
		out = append(out, s.SQL)
	}
	return out
}

func wantSQL(t *testing.T, p *Plan, want ...string) {
	t.Helper()
	got := sqls(p)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("语句不符\n got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != code {
		t.Fatalf("期望错误码 %s，实际 %v", code, err)
	}
}

func TestPlanDailyDatetimeAddsToPreCreate(t *testing.T) {
	st := dtState(rp("p20261005", "'2026-10-06 00:00:00'"), rp("p20261006", "'2026-10-07 00:00:00'"), rp("p20261007", "'2026-10-08 00:00:00'"))
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, cst)
	p, err := BuildPlan(baseCfg(), st, now)
	if err != nil {
		t.Fatal(err)
	}
	// 当前分区 p20261006，其后已有 p20261007，需要再预创建两个才满 3 个
	wantSQL(t, p,
		"ALTER TABLE `log`.`access_log` ADD PARTITION (PARTITION `p20261008` VALUES LESS THAN ('2026-10-09 00:00:00'))",
		"ALTER TABLE `log`.`access_log` ADD PARTITION (PARTITION `p20261009` VALUES LESS THAN ('2026-10-10 00:00:00'))")
	if len(p.Drops) != 0 || p.Reorganize != nil {
		t.Fatalf("不应有删除或重组: %+v", p)
	}
	// 已经满了就什么都不做（幂等：再次执行是空操作）
	st2 := dtState(append(st.Partitions, rp("p20261008", "'2026-10-09 00:00:00'"), rp("p20261009", "'2026-10-10 00:00:00'"))...)
	p2, err := BuildPlan(baseCfg(), st2, now)
	if err != nil || len(p2.Statements()) != 0 {
		t.Fatalf("第二次应无语句: %v %v", err, sqls(p2))
	}
}

func TestPlanCatchUpWhenAllExpired(t *testing.T) {
	st := dtState(rp("p20260930", "'2026-10-01 00:00:00'"))
	cfg := baseCfg()
	cfg.PreCreate = 2
	p, err := BuildPlan(cfg, st, time.Date(2026, 10, 3, 10, 0, 0, 0, cst))
	if err != nil {
		t.Fatal(err)
	}
	// 10-01、10-02、10-03（含当前时刻）三个补建 + 预创建 10-04、10-05
	if len(p.Adds) != 5 || p.Adds[0].Partitions[0] != "p20261001" || p.Adds[4].Partitions[0] != "p20261005" || p.Adds[4].Bound != "2026-10-06 00:00:00" {
		t.Fatalf("%v", sqls(p))
	}
}

func TestPlanRetainDropsOldestHistorical(t *testing.T) {
	st := dtState(rp("p20261001", "'2026-10-02 00:00:00'"), rp("p20261002", "'2026-10-03 00:00:00'"), rp("p20261003", "'2026-10-04 00:00:00'"),
		rp("p20261004", "'2026-10-05 00:00:00'"), rp("p20261005", "'2026-10-06 00:00:00'"), rp("p20261006", "'2026-10-07 00:00:00'"),
		rp("p20261007", "'2026-10-08 00:00:00'"), rp("p20261008", "'2026-10-09 00:00:00'"), rp("p20261009", "'2026-10-10 00:00:00'"))
	cfg := baseCfg()
	cfg.Retain = 2
	p, err := BuildPlan(cfg, st, time.Date(2026, 10, 6, 12, 0, 0, 0, cst))
	if err != nil {
		t.Fatal(err)
	}
	// 历史（已完全过去）分区：p20261001..p20261005 共 5 个，保留最近 2 个 => 删最早 3 个；当前分区 p20261006 不动
	if got := strings.Join(p.DropNames(), ","); got != "p20261001,p20261002,p20261003" {
		t.Fatalf("删除列表 %s", got)
	}
	for _, d := range p.Drops {
		if !d.Danger || d.Kind != "drop" {
			t.Fatalf("删除语句应标为危险: %+v", d)
		}
	}
	wantSQL(t, p,
		"ALTER TABLE `log`.`access_log` DROP PARTITION `p20261001`",
		"ALTER TABLE `log`.`access_log` DROP PARTITION `p20261002`",
		"ALTER TABLE `log`.`access_log` DROP PARTITION `p20261003`")
	// retain=0：全部保留
	cfg.Retain = 0
	p, err = BuildPlan(cfg, st, time.Date(2026, 10, 6, 12, 0, 0, 0, cst))
	if err != nil || len(p.Drops) != 0 || len(p.Warnings) == 0 {
		t.Fatalf("retain=0 不应删除，且有提示: %v %+v", err, p.Warnings)
	}
}

func TestPlanBoundaryEdgeNowEqualsBound(t *testing.T) {
	// 现在恰好等于某分区的上边界：该分区已经完全过去（数据 < 边界）
	st := dtState(rp("p20261005", "'2026-10-06 00:00:00'"), rp("p20261006", "'2026-10-07 00:00:00'"))
	cfg := baseCfg()
	cfg.PreCreate, cfg.Retain = 1, 1
	p, err := BuildPlan(cfg, st, time.Date(2026, 10, 6, 0, 0, 0, 0, cst))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Adds) != 1 || p.Adds[0].Partitions[0] != "p20261007" || len(p.Drops) != 0 {
		t.Fatalf("%v drops=%v", sqls(p), p.DropNames())
	}
}

func TestPlanMonthYearWeekHourMinute(t *testing.T) {
	cases := []struct {
		name   string
		period string
		n      int
		last   string // 最后一个分区边界
		now    time.Time
		pre    int
		want   []string // 新分区名
		bound  string   // 最后一个新分区的边界
	}{
		{"month", "month", 1, "2026-11-01 00:00:00", time.Date(2026, 10, 6, 0, 0, 0, 0, cst), 2, []string{"p202611", "p202612"}, "2027-01-01 00:00:00"},
		{"month×3", "month", 3, "2027-01-01 00:00:00", time.Date(2026, 10, 6, 0, 0, 0, 0, cst), 1, []string{"p202701"}, "2027-04-01 00:00:00"},
		{"year", "year", 1, "2027-01-01 00:00:00", time.Date(2026, 10, 6, 0, 0, 0, 0, cst), 1, []string{"p2027"}, "2028-01-01 00:00:00"},
		{"week", "week", 1, "2026-10-12 00:00:00", time.Date(2026, 10, 6, 0, 0, 0, 0, cst), 1, []string{"p20261018"}, "2026-10-19 00:00:00"},
		{"hour×6", "hour", 6, "2026-10-06 18:00:00", time.Date(2026, 10, 6, 13, 0, 0, 0, cst), 1, []string{"p2026100618"}, "2026-10-07 00:00:00"},
		{"minute×20", "minute", 20, "2026-10-06 13:20:00", time.Date(2026, 10, 6, 13, 5, 0, 0, cst), 2, []string{"p202610061320", "p202610061340"}, "2026-10-06 14:00:00"},
	}
	for _, c := range cases {
		cfg := baseCfg()
		cfg.Period, cfg.Interval, cfg.PreCreate, cfg.Retain = c.period, c.n, c.pre, 0
		// 现有分区只有 1 个（最后一个），边界即 c.last；用它的起点命名即可
		st := dtState(rp("p_seed", "'"+c.last+"'"))
		p, err := BuildPlan(cfg, st, c.now)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		// 现有 1 个分区边界在未来，future=1 => count = pre
		var names []string
		for _, a := range p.Adds {
			names = append(names, a.Partitions[0])
		}
		if strings.Join(names, ",") != strings.Join(c.want, ",") || p.Adds[len(p.Adds)-1].Bound != c.bound {
			t.Errorf("%s: 新分区 %v 末尾边界 %s，期望 %v %s", c.name, names, p.Adds[len(p.Adds)-1].Bound, c.want, c.bound)
		}
	}
}

func TestPlanIntegerTimestamps(t *testing.T) {
	cfg := baseCfg()
	cfg.ColType, cfg.PreCreate, cfg.Retain, cfg.Timezone = ColBigint, 1, 0, "UTC"
	end := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	st := TableState{Exists: true, IsBase: true, ColumnType: "bigint", ColumnTypeFull: "bigint(20)", Method: "RANGE", Expression: "`created_at`",
		Partitions: []RawPartition{rp("p20261006", "1791331200000")}}
	if end.UnixMilli() != 1791331200000 {
		t.Fatalf("测试数据有误 %d", end.UnixMilli())
	}
	p, err := BuildPlan(cfg, st, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	wantSQL(t, p, "ALTER TABLE `log`.`access_log` ADD PARTITION (PARTITION `p20261007` VALUES LESS THAN (1791417600000))")

	// int（秒）+ 有符号：2038 年之后的边界会溢出，必须拒绝
	cfg.ColType = ColInt
	st = TableState{Exists: true, IsBase: true, ColumnType: "int", ColumnTypeFull: "int(11)", Method: "RANGE", Expression: "created_at",
		Partitions: []RawPartition{rp("p20371230", "2145830400")}} // 2037-12-31 00:00:00 UTC
	cfg.PreCreate = 40
	_, err = BuildPlan(cfg, st, time.Date(2037, 12, 30, 0, 0, 0, 0, time.UTC))
	wantCode(t, err, "int_overflow")
	// unsigned 可以到 2106 年
	st.ColumnTypeFull = "int unsigned"
	if _, err = BuildPlan(cfg, st, time.Date(2037, 12, 30, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("unsigned 不应溢出: %v", err)
	}
}

func TestPlanReorganizeWhenMaxValue(t *testing.T) {
	st := dtState(rp("p20261006", "'2026-10-07 00:00:00'"), rp("pmax", "MAXVALUE"))
	cfg := baseCfg()
	cfg.PreCreate, cfg.Retain = 2, 0
	p, err := BuildPlan(cfg, st, time.Date(2026, 10, 6, 12, 0, 0, 0, cst))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Adds) != 0 || p.Reorganize == nil || !p.Reorganize.Heavy {
		t.Fatalf("应改用 REORGANIZE: %+v", p)
	}
	wantSQL(t, p, "ALTER TABLE `log`.`access_log` REORGANIZE PARTITION `pmax` INTO (PARTITION `p20261007` VALUES LESS THAN ('2026-10-08 00:00:00'), "+
		"PARTITION `p20261008` VALUES LESS THAN ('2026-10-09 00:00:00'), PARTITION `pmax` VALUES LESS THAN (MAXVALUE))")
	// MAXVALUE 分区永不被删除
	cfg.Retain = 1
	st = dtState(rp("p1", "'2026-10-01 00:00:00'"), rp("p2", "'2026-10-02 00:00:00'"), rp("p3", "'2026-10-07 00:00:00'"), rp("pmax", "MAXVALUE"))
	cfg.PreCreate = 0
	p, err = BuildPlan(cfg, st, time.Date(2026, 10, 6, 12, 0, 0, 0, cst))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.DropNames(), ","); got != "p1" {
		t.Fatalf("删除 %s", got)
	}
}

func TestPlanRejectsUnsafeTargets(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, cst)
	good := dtState(rp("p20261006", "'2026-10-07 00:00:00'"))
	mod := func(f func(*TableState)) TableState {
		s := good
		s.Partitions = append([]RawPartition(nil), good.Partitions...)
		f(&s)
		return s
	}
	cases := []struct {
		name string
		st   TableState
		code string
	}{
		{"表不存在", TableState{}, "table_missing"},
		{"不是普通表", mod(func(s *TableState) { s.IsBase = false }), "not_base_table"},
		{"子分区", mod(func(s *TableState) { s.Subpartitioned = true }), "subpartition"},
		{"LIST 分区", mod(func(s *TableState) { s.Method = "LIST" }), "bad_method"},
		{"字段不存在", mod(func(s *TableState) { s.ColumnType = "" }), "column_missing"},
		{"表达式不同", mod(func(s *TableState) { s.Expression = "to_days(`created_at`)" }), "expr_mismatch"},
		{"类型不符", mod(func(s *TableState) { s.ColumnType = "bigint" }), "type_mismatch"},
		{"datetime 用 RANGE", mod(func(s *TableState) { s.Method = "RANGE" }), "bad_method"},
		{"边界无法解析", mod(func(s *TableState) { s.Partitions[0].Desc = "to_days('x')" }), "bad_bound"},
		{"分区名含非法字符", mod(func(s *TableState) { s.Partitions[0].Name = "p`;drop" }), "bad_partition_name"},
		{"只有 MAXVALUE", mod(func(s *TableState) { s.Partitions = []RawPartition{rp("pmax", "MAXVALUE")} }), "no_bound"},
		{"MAXVALUE 不在末尾", mod(func(s *TableState) {
			s.Partitions = []RawPartition{rp("pmax", "MAXVALUE"), rp("p2", "'2026-10-08 00:00:00'")}
		}), "bad_partition"},
		{"边界不递增", mod(func(s *TableState) {
			s.Partitions = []RawPartition{rp("p1", "'2026-10-08 00:00:00'"), rp("p2", "'2026-10-07 00:00:00'")}
		}), "bad_partition"},
	}
	for _, c := range cases {
		_, err := BuildPlan(baseCfg(), c.st, now)
		if err == nil {
			t.Errorf("%s：应拒绝", c.name)
			continue
		}
		var pe *Error
		if !errors.As(err, &pe) || pe.Code != c.code {
			t.Errorf("%s：错误码 %v，期望 %s", c.name, err, c.code)
		}
	}
	// 配置本身不合法
	bad := baseCfg()
	bad.TbName = "t`; DROP TABLE x; --"
	if _, err := BuildPlan(bad, good, now); err == nil {
		t.Error("非法表名必须拒绝")
	}
	bad = baseCfg()
	bad.Timezone = "Mars/Base"
	if _, err := BuildPlan(bad, good, now); err == nil {
		t.Error("非法时区必须拒绝")
	}
}

func TestPlanMonthMisalignedAndTooMany(t *testing.T) {
	cfg := baseCfg()
	cfg.Period = "month"
	_, err := BuildPlan(cfg, dtState(rp("p1", "'2026-11-15 00:00:00'")), time.Date(2026, 10, 6, 0, 0, 0, 0, cst))
	wantCode(t, err, "misaligned")

	// 按分钟分区却一年没维护：补建数量远超上限，拒绝
	cfg = baseCfg()
	cfg.Period, cfg.Interval = "minute", 1
	_, err = BuildPlan(cfg, dtState(rp("p1", "'2025-10-01 00:00:00'")), time.Date(2026, 10, 6, 0, 0, 0, 0, cst))
	wantCode(t, err, "too_many")
}

func TestPlanNameConflictAndIntervalZero(t *testing.T) {
	// 现有分区名与将创建的重名但边界不连续
	st := dtState(rp("p20261007", "'2026-10-01 00:00:00'"), rp("p20261006", "'2026-10-06 00:00:00'"))
	_, err := BuildPlan(baseCfg(), st, time.Date(2026, 10, 6, 12, 0, 0, 0, cst))
	wantCode(t, err, "name_conflict")

	st = dtState(rp("p20261008", "'2026-10-07 00:00:00'"))
	cfg := baseCfg()
	cfg.PreCreate = 2
	// 将创建 p20261007（<10-08）、p20261008（<10-09）：后者与现有分区重名
	_, err = BuildPlan(cfg, st, time.Date(2026, 10, 6, 12, 0, 0, 0, cst))
	wantCode(t, err, "name_conflict")

	// 旧数据里 interval=0：按 1 处理并给出提示
	cfg = baseCfg()
	cfg.Interval, cfg.PreCreate, cfg.Retain = 0, 1, 0
	p, err := BuildPlan(cfg, dtState(rp("p20261006", "'2026-10-07 00:00:00'")), time.Date(2026, 10, 6, 12, 0, 0, 0, cst))
	if err != nil || len(p.Adds) != 1 || len(p.Warnings) == 0 {
		t.Fatalf("interval=0: %v %+v", err, p)
	}
}

func TestLocation(t *testing.T) {
	for _, tz := range []string{"UTC", "Asia/Shanghai", "+08:00", "-05:30", "America/New_York", "+8", "UTC+8", "GMT+08:00", "+0800", "Europe/Paris"} {
		if _, err := Location(tz); err != nil {
			t.Errorf("%s: %v", tz, err)
		}
	}
	for _, tz := range []string{"", "Europe/Nowhere", "+25:00", "UTC+abc", "Local", "+08:99", "America/Argentina/Buenos_Aires"} {
		if _, err := Location(tz); err == nil {
			t.Errorf("%q 应拒绝", tz)
		}
	}
}

func TestParseBoundFormats(t *testing.T) {
	for _, desc := range []string{"'2026-10-07 00:00:00'", "2026-10-07 00:00:00", "'2026-10-07'"} {
		b, err := parseBound(desc, ColDatetime, cst)
		if err != nil || !b.Equal(time.Date(2026, 10, 7, 0, 0, 0, 0, cst)) {
			t.Errorf("%s: %v %v", desc, b, err)
		}
	}
	if _, err := parseBound("abc", ColBigint, cst); err == nil {
		t.Error("非数字应报错")
	}
}
