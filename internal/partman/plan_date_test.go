// @author: kevinkelly
// @license: Apache Licence
// @file: plan_date_test.go
// @time: 2026/10/28

package partman

import (
	"strings"
	"testing"
	"time"
)

// ---- DATE 分区字段：RANGE COLUMNS(字段) + 'YYYY-MM-DD' 字面量（与 datetime 同一套做法，MySQL 5.7 / 8 都合法）----

func dateCfg() Config {
	c := baseCfg()
	c.ColType = ColDate
	return c
}

func dateState(parts ...RawPartition) TableState {
	return TableState{Exists: true, IsBase: true, ColumnType: "date", ColumnTypeFull: "date", Method: "RANGE COLUMNS", Expression: "`created_at`", Partitions: parts}
}

func TestPlanDateAddsAndDrops(t *testing.T) {
	// information_schema.PARTITIONS 里 DATE 列的 RANGE COLUMNS 边界是 '2026-10-07'
	st := dateState(rp("p20261003", "'2026-10-04'"), rp("p20261004", "'2026-10-05'"), rp("p20261005", "'2026-10-06'"), rp("p20261006", "'2026-10-07'"), rp("p20261007", "'2026-10-08'"))
	cfg := dateCfg()
	cfg.Retain = 2
	p, err := BuildPlan(cfg, st, time.Date(2026, 10, 6, 12, 0, 0, 0, cst))
	if err != nil {
		t.Fatal(err)
	}
	// 当前分区 p20261006，其后已有 p20261007，预创建 3 个需再补两个；历史（已过去）3 个保留最近 2 个 => 删 p20261003
	wantSQL(t, p,
		"ALTER TABLE `log`.`access_log` ADD PARTITION (PARTITION `p20261008` VALUES LESS THAN ('2026-10-09'))",
		"ALTER TABLE `log`.`access_log` ADD PARTITION (PARTITION `p20261009` VALUES LESS THAN ('2026-10-10'))",
		"ALTER TABLE `log`.`access_log` DROP PARTITION `p20261003`")
	if p.ColumnType != "date" {
		t.Errorf("ColumnType: %s", p.ColumnType)
	}
	// 幂等：补满后再算一次没有语句
	st2 := dateState(append(st.Partitions, rp("p20261008", "'2026-10-09'"), rp("p20261009", "'2026-10-10'"))...)
	cfg.Retain = 0
	p2, err := BuildPlan(cfg, st2, time.Date(2026, 10, 6, 12, 0, 0, 0, cst))
	if err != nil || len(p2.Statements()) != 0 {
		t.Fatalf("第二次应无语句: %v %v", err, sqls(p2))
	}
}

func TestPlanDateCatchUpMonthWeekAndReorganize(t *testing.T) {
	// 全部过期：补建到包含当前时刻，再预创建
	cfg := dateCfg()
	cfg.PreCreate, cfg.Retain = 2, 0
	p, err := BuildPlan(cfg, dateState(rp("p20260930", "'2026-10-01'")), time.Date(2026, 10, 3, 10, 0, 0, 0, cst))
	if err != nil || len(p.Adds) != 5 || p.Adds[0].SQL != "ALTER TABLE `log`.`access_log` ADD PARTITION (PARTITION `p20261001` VALUES LESS THAN ('2026-10-02'))" ||
		!strings.HasSuffix(p.Adds[4].SQL, "VALUES LESS THAN ('2026-10-06'))") {
		t.Fatalf("补建: %v %v", err, sqls(p))
	}

	// 月：必须对齐月初，边界字面量只有日期
	cfg = dateCfg()
	cfg.Period, cfg.PreCreate, cfg.Retain = "month", 2, 0
	p, err = BuildPlan(cfg, dateState(rp("p202610", "'2026-11-01'")), time.Date(2026, 10, 6, 0, 0, 0, 0, cst))
	if err != nil {
		t.Fatal(err)
	}
	wantSQL(t, p, "ALTER TABLE `log`.`access_log` ADD PARTITION (PARTITION `p202611` VALUES LESS THAN ('2026-12-01'))",
		"ALTER TABLE `log`.`access_log` ADD PARTITION (PARTITION `p202612` VALUES LESS THAN ('2027-01-01'))")
	if _, err = BuildPlan(cfg, dateState(rp("p_x", "'2026-11-15'")), time.Date(2026, 10, 6, 0, 0, 0, 0, cst)); err == nil {
		t.Error("没有对齐月初应拒绝")
	} else {
		wantCode(t, err, "misaligned")
	}

	// 周
	cfg = dateCfg()
	cfg.Period, cfg.PreCreate, cfg.Retain = "week", 1, 0
	p, err = BuildPlan(cfg, dateState(rp("p_seed", "'2026-10-12'")), time.Date(2026, 10, 6, 0, 0, 0, 0, cst))
	if err != nil {
		t.Fatal(err)
	}
	wantSQL(t, p, "ALTER TABLE `log`.`access_log` ADD PARTITION (PARTITION `p20261018` VALUES LESS THAN ('2026-10-19'))")

	// MAXVALUE：REORGANIZE，字面量同样只有日期
	cfg = dateCfg()
	cfg.PreCreate, cfg.Retain = 1, 0
	p, err = BuildPlan(cfg, dateState(rp("p20261006", "'2026-10-07'"), rp("pmax", "MAXVALUE")), time.Date(2026, 10, 6, 12, 0, 0, 0, cst))
	if err != nil || p.Reorganize == nil {
		t.Fatalf("%v %+v", err, p)
	}
	wantSQL(t, p, "ALTER TABLE `log`.`access_log` REORGANIZE PARTITION `pmax` INTO (PARTITION `p20261007` VALUES LESS THAN ('2026-10-08'), PARTITION `pmax` VALUES LESS THAN (MAXVALUE))")
}

func TestPlanDateRejects(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, cst)
	ok := dateState(rp("p20261006", "'2026-10-07'"))
	// 配置 date，表里实际是 datetime（反之亦然）：类型必须一致
	dt := dtState(rp("p20261006", "'2026-10-07 00:00:00'"))
	if _, err := BuildPlan(dateCfg(), dt, now); err == nil {
		t.Error("date 配置 + datetime 字段应拒绝")
	} else {
		wantCode(t, err, "type_mismatch")
	}
	if _, err := BuildPlan(baseCfg(), ok, now); err == nil {
		t.Error("datetime 配置 + date 字段应拒绝")
	} else {
		wantCode(t, err, "type_mismatch")
	}
	// date 只能用 RANGE COLUMNS；RANGE 分区（TO_DAYS 之类的表达式分区另外会被“表达式不一致”拦住）不支持
	toDays := ok
	toDays.Method = "RANGE"
	if _, err := BuildPlan(dateCfg(), toDays, now); err == nil || !strings.Contains(err.Error(), "date 字段需要 RANGE COLUMNS") {
		t.Errorf("RANGE: %v", err)
	}
	toDays.Expression = "to_days(`created_at`)"
	if _, err := BuildPlan(dateCfg(), toDays, now); err == nil {
		t.Error("TO_DAYS 表达式分区不支持")
	} else {
		wantCode(t, err, "expr_mismatch")
	}
	// 周期小于一天：date 表达不了
	for _, per := range []string{"minute", "hour"} {
		c := dateCfg()
		c.Period = per
		if _, err := BuildPlan(c, ok, now); err == nil {
			t.Errorf("%s 周期应拒绝", per)
		} else {
			wantCode(t, err, "bad_request")
		}
	}
	// 非法的 date 边界
	if _, err := BuildPlan(dateCfg(), dateState(rp("p1", "'abc'")), now); err == nil {
		t.Error("非法边界应拒绝")
	} else {
		wantCode(t, err, "bad_bound")
	}
}

func TestParseBoundDate(t *testing.T) {
	for _, desc := range []string{"'2026-10-07'", "2026-10-07"} {
		b, err := parseBound(desc, ColDate, cst)
		if err != nil || !b.Equal(time.Date(2026, 10, 7, 0, 0, 0, 0, cst)) {
			t.Errorf("%s: %v %v", desc, b, err)
		}
	}
	if _, err := parseBound("1790000000", ColDate, cst); err == nil {
		t.Error("时间戳不是 date 边界")
	}
	if got := partColType("DATE"); got != ColDate {
		t.Errorf("partColType: %q", got)
	}
	for _, other := range []string{"timestamp", "time", "year", "mediumint", "varchar"} {
		if partColType(other) != "" {
			t.Errorf("%s 不应作为分区候选", other)
		}
	}
}

func TestConfigValidateDate(t *testing.T) {
	in := ConfigInput{InstanceID: 1, DBName: "log", TbName: "t", Column: "d", ColType: ColDate, Period: "day", Interval: 1, Retain: 7, PreCreate: 3, Timezone: "UTC"}
	if err := in.validate(); err != nil {
		t.Fatalf("date + day: %v", err)
	}
	for _, per := range []string{"week", "month", "year"} {
		in.Period = per
		if err := in.validate(); err != nil {
			t.Errorf("date + %s: %v", per, err)
		}
	}
	for _, per := range []string{"hour", "minute"} {
		in.Period = per
		if err := in.validate(); err == nil || !strings.Contains(err.Error(), "date 字段只能精确到天") {
			t.Errorf("date + %s 应拒绝: %v", per, err)
		}
	}
	// datetime 的小时 / 分钟周期不受影响
	in.ColType, in.Period = ColDatetime, "hour"
	if err := in.validate(); err != nil {
		t.Errorf("datetime + hour: %v", err)
	}
	in.ColType = "timestamp"
	if err := in.validate(); err == nil || !strings.Contains(err.Error(), "datetime、date") {
		t.Errorf("提示: %v", err)
	}
}
