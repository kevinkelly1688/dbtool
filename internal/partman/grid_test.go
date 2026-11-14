// @author: kevinkelly
// @license: Apache Licence
// @file: grid_test.go
// @time: 2026/10/28

package partman

import (
	"strings"
	"testing"
	"time"
)

// 网格 / 命名与原程序一致：期望值来自 grid_golden_test.go（原程序 timeutil.go 对同一输入的输出）。
func TestGridMatchesOriginalProgram(t *testing.T) {
	if len(gridGolden) < 400 {
		t.Fatalf("期望值表太少: %d", len(gridGolden))
	}
	for _, c := range gridGolden {
		tz, err := ParseTimezone(c.tz)
		if err != nil {
			t.Fatalf("%s: %v", c.tz, err)
		}
		now, err := time.ParseInLocation(dtLayout, c.now, tz.Loc)
		if err != nil {
			t.Fatal(err)
		}
		cur := alignDown(now, c.period, c.interval, tz.Loc)
		upper := step(cur, c.period, c.interval, 1)
		name := partName(cur, upper, c.period)
		if got := cur.Format(dtLayout); got != c.start {
			t.Errorf("%s %s %s×%d：窗口起点 %s，原程序 %s", c.tz, c.now, c.period, c.interval, got, c.start)
		}
		if name != c.name {
			t.Errorf("%s %s %s×%d：分区名 %s，原程序 %s", c.tz, c.now, c.period, c.interval, name, c.name)
		}
		if got := upper.Format(dtLayout); got != c.upper {
			t.Errorf("%s %s %s×%d：上界 %s，原程序 %s", c.tz, c.now, c.period, c.interval, got, c.upper)
		}
	}
}

// 原程序 selftest（2026-06-17 13:45:01 Asia/Shanghai）的输出，逐行照抄：周期、跨度 → 当前窗口起点、分区名、上界。
func TestSelftestCasesOfOriginalProgram(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 6, 17, 13, 45, 1, 0, loc)
	cases := []struct {
		period          string
		n               int
		start, name, up string
	}{
		{"minute", 5, "2026-06-17 13:45:00", "p202606171345", "2026-06-17 13:50:00"},
		{"hour", 1, "2026-06-17 13:00:00", "p2026061713", "2026-06-17 14:00:00"},
		{"day", 1, "2026-06-17 00:00:00", "p20260617", "2026-06-18 00:00:00"},
		{"day", 7, "2026-06-11 00:00:00", "p20260611", "2026-06-18 00:00:00"},
		{"week", 1, "2026-06-15 00:00:00", "p20260621", "2026-06-22 00:00:00"}, // 周：窗口最后一天（周日），不是周一
		{"month", 1, "2026-06-01 00:00:00", "p202606", "2026-07-01 00:00:00"},
		{"year", 1, "2026-01-01 00:00:00", "p2026", "2027-01-01 00:00:00"},
	}
	for _, c := range cases {
		cur := alignDown(now, c.period, c.n, loc)
		up := step(cur, c.period, c.n, 1)
		if cur.Format(dtLayout) != c.start || partName(cur, up, c.period) != c.name || up.Format(dtLayout) != c.up {
			t.Errorf("%s×%d: %s %s %s", c.period, c.n, cur.Format(dtLayout), partName(cur, up, c.period), up.Format(dtLayout))
		}
	}
}

// week 命名：窗口最后一天（原程序），跨度 2 周时是第二周的周日。
func TestWeekNamingIsLastDayOfWindow(t *testing.T) {
	cfg := baseCfg()
	cfg.Period, cfg.Interval, cfg.PreCreate, cfg.Retain = "week", 2, 1, 0
	// 现有分区的上界在 2026-10-12（周一），未来已有 1 个 → count = preCreate - 0 = 1
	st := dtState(rp("p_seed", "'2026-10-26 00:00:00'"))
	p, err := BuildPlan(cfg, st, time.Date(2026, 10, 6, 12, 0, 0, 0, cst))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Adds) != 1 || p.Adds[0].Partitions[0] != "p20261108" || p.Adds[0].Bound != "2026-11-09 00:00:00" {
		t.Fatalf("%+v", p.Adds)
	}
}

// 已存在的分区只看边界，不看名字：现网分区是原程序建的，名字可以随意。
func TestExistingPartitionsMatchedByBoundNotName(t *testing.T) {
	cfg := baseCfg()
	cfg.PreCreate, cfg.Retain = 2, 0
	st := dtState(rp("part_a", "'2026-10-06 00:00:00'"), rp("part_b", "'2026-10-07 00:00:00'"), rp("part_c", "'2026-10-08 00:00:00'"), rp("part_d", "'2026-10-09 00:00:00'"))
	p, err := BuildPlan(cfg, st, time.Date(2026, 10, 6, 12, 0, 0, 0, cst))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Adds) != 0 {
		t.Fatalf("边界已满足 preCreate，名字不同不应再建: %v", sqls(p))
	}
}

// 将创建的分区若与现有分区重名但边界不同，报冲突。
func TestNameConflictWhenSameNameDifferentBound(t *testing.T) {
	cfg := baseCfg()
	cfg.PreCreate, cfg.Retain = 1, 0
	st := dtState(rp("p20261006", "'2026-10-07 00:00:00'"), rp("p20261007", "'2026-10-08 00:00:00'"))
	// 现有最后边界 10-08，要建的第一个是 p20261008；让一个旧分区占用这个名字（边界却是更早的）
	st.Partitions = append([]RawPartition{rp("p20261008", "'2026-10-06 00:00:00'")}, st.Partitions...)
	_, err := BuildPlan(cfg, st, time.Date(2026, 10, 7, 12, 0, 0, 0, cst))
	wantCode(t, err, "name_conflict")
	if !strings.Contains(err.Error(), "p20261008") {
		t.Fatal(err)
	}
}
