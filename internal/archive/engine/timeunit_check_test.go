// @author: kevinkelly
// @license: Apache Licence
// @file: timeunit_check_test.go
// @time: 2026/10/28

package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dbtool/internal/archive/engine"
	"dbtool/internal/archive/engine/enginetest"
)

// epochHarness 造一张时间列是 bigint 的表（created_at 改成 bigint(20)，行里放毫秒或秒时间戳），有索引。
func epochHarness(rows int, unit string, base int64) (*enginetest.Harness, engine.Spec) {
	h := enginetest.New(enginetest.Options{Rows: 100})
	cols := enginetest.DefaultColumns()
	cols[1].Type = "bigint(20)"
	h.Src = enginetest.NewMemSource(h.Log, h.Clock, "shop", "orders", cols, []string{"id"})
	var seed []engine.Row
	for i := 1; i <= rows; i++ {
		seed = append(seed, engine.Row{int64(i), base + int64(i), int64(i % 5), "p"})
	}
	h.Src.Seed(seed...)
	h.Src.Info.Indexes["idx_created"] = []string{"created_at"}
	h.Src.ExplainRes = engine.ExplainResult{UsedIndex: "idx_created", ScanRows: 1000, Type: "range"}
	spec := h.Spec()
	spec.TimeUnit = unit
	return h, spec
}

func msProbe() engine.TimeProbe {
	return engine.TimeProbe{Sampled: 100, InMs: 100, Sample: 1700000000000, HasRange: true, Min: 1600000000000, Max: 1760000000000,
		OffsetSec: 8 * 3600, GuessUnit: engine.UnitEpochMs}
}

func TestCheckEpochUnitOK(t *testing.T) {
	h, spec := epochHarness(100, engine.UnitEpochMs, 1_700_000_000_000)
	h.Src.ProbeRes = msProbe()
	rep := runCheck(t, h, spec)
	if rep.HasError() {
		t.Fatalf("不应有红色项：%+v", rep.Items)
	}
	wantLevel(t, rep, "time_column", "ok", "created_at（bigint(20)）")
	wantLevel(t, rep, "time_unit", "ok", "13 位 Unix 时间戳")
	wantLevel(t, rep, "time_unit", "ok", "最早 2020-09-13 20:26:40") // 1600000000 秒 = 2020-09-13 12:26:40 UTC，+08:00
	if item(rep, "explain") == nil || rep.EstRows != 100 {
		t.Errorf("单位核对通过后应继续 EXPLAIN / 命中行数：%+v", rep.Items)
	}
	if h.Src.ProbeCalls != 1 || !h.Src.ProbeRangeOK {
		t.Errorf("时间列有索引时应允许取最早 / 最晚值：calls=%d rangeOK=%v", h.Src.ProbeCalls, h.Src.ProbeRangeOK)
	}
	if !strings.Contains(rep.SQL.Select, "`created_at` > 0 AND `created_at` < (UNIX_TIMESTAMP() - 7776000) * 1000") {
		t.Errorf("SQL 预览应是毫秒条件：%s", rep.SQL.Select)
	}
	// 秒
	h2, spec2 := epochHarness(100, engine.UnitEpochSec, 1_700_000_000)
	h2.Src.ProbeRes = engine.TimeProbe{Sampled: 100, InSec: 100, Sample: 1700000000, HasRange: true, Min: 1600000000, Max: 1760000000, GuessUnit: engine.UnitEpochSec}
	rep2 := runCheck(t, h2, spec2)
	if rep2.HasError() {
		t.Fatalf("秒：不应有红色项：%+v", rep2.Items)
	}
	wantLevel(t, rep2, "time_unit", "ok", "10 位 Unix 时间戳")
	wantLevel(t, rep2, "time_unit", "ok", "最早 2020-09-13 12:26:40")
}

func TestCheckEpochUnitWithoutRange(t *testing.T) {
	// 没有索引的大表：不取最早 / 最晚值（会全表扫描），只看抽样
	h, spec := epochHarness(100, engine.UnitEpochMs, 1_700_000_000_000)
	h.Src.RowsOverride = 5_000_000
	delete(h.Src.Info.Indexes, "idx_created")
	h.Src.ProbeRes = engine.TimeProbe{Sampled: 100, InMs: 100, Sample: 1700000000000, OffsetSec: 8 * 3600, GuessUnit: engine.UnitEpochMs}
	rep := runCheck(t, h, spec)
	wantLevel(t, rep, "index", "error", "没有索引")
	wantLevel(t, rep, "time_unit", "ok", "抽样 100 行")
	if h.Src.ProbeRangeOK {
		t.Error("没有索引的大表不应允许取最早 / 最晚值")
	}
	// 没有索引但表很小：可以取
	h2, spec2 := epochHarness(100, engine.UnitEpochMs, 1_700_000_000_000)
	delete(h2.Src.Info.Indexes, "idx_created")
	h2.Src.ProbeRes = msProbe()
	runCheck(t, h2, spec2)
	if !h2.Src.ProbeRangeOK {
		t.Error("小表应允许取最早 / 最晚值")
	}
	// 抽样里混着区间外的值：黄色提示
	h3, spec3 := epochHarness(100, engine.UnitEpochMs, 1_700_000_000_000)
	h3.Src.RowsOverride = 5_000_000
	delete(h3.Src.Info.Indexes, "idx_created")
	h3.Src.ProbeRes = engine.TimeProbe{Sampled: 100, InMs: 80, InSec: 20, Sample: 1700000000000, GuessUnit: engine.UnitEpochMs}
	wantLevel(t, runCheck(t, h3, spec3), "time_unit", "warn", "20 行的值不在 13 位时间戳的合理区间内")
}

func TestCheckEpochUnitMismatchIsError(t *testing.T) {
	// 列里是 10 位秒，任务选了 13 位毫秒：条件会让整张表都命中，必须是红色，而且不再统计命中行数
	h, spec := epochHarness(100, engine.UnitEpochMs, 1_700_000_000)
	h.Src.ProbeRes = engine.TimeProbe{Sampled: 100, InSec: 100, Sample: 1700000000, HasRange: true, Min: 1600000000, Max: 1700000100, GuessUnit: engine.UnitEpochSec}
	rep := runCheck(t, h, spec)
	wantLevel(t, rep, "time_unit", "error", "看起来是 10 位的 Unix 时间戳，与所选的 13 位不符")
	if !rep.HasError() {
		t.Error("应有红色项")
	}
	if item(rep, "explain") != nil || item(rep, "max_rows") != nil || rep.EstRows != 0 || len(rep.Sample) != 0 {
		t.Errorf("单位不符时不应继续统计命中行数：%+v", rep.Items)
	}
	// 反过来：列里是 13 位毫秒，选了 10 位秒
	h2, spec2 := epochHarness(100, engine.UnitEpochSec, 1_700_000_000_000)
	h2.Src.ProbeRes = msProbe()
	wantLevel(t, runCheck(t, h2, spec2), "time_unit", "error", "看起来是 13 位的 Unix 时间戳，与所选的 10 位不符")
	// 选错了列（例如自增 id）：不像时间戳
	h3, spec3 := epochHarness(100, engine.UnitEpochMs, 0)
	h3.Src.ProbeRes = engine.TimeProbe{Sampled: 100, Sample: 7, HasRange: true, Min: 1, Max: 100}
	wantLevel(t, runCheck(t, h3, spec3), "time_unit", "error", "不像 13 位的 Unix 时间戳")
}

func TestCheckEpochUnitWarnings(t *testing.T) {
	// 最小值落在秒级区间（混用了秒和毫秒）：黄色
	h, spec := epochHarness(100, engine.UnitEpochMs, 1_700_000_000_000)
	p := msProbe()
	p.Min = 1_500_000_000
	h.Src.ProbeRes = p
	rep := runCheck(t, h, spec)
	wantLevel(t, rep, "time_unit", "warn", "最小值 1500000000 小于 13 位时间戳的合理下限")
	if rep.EstRows != 100 {
		t.Error("只是黄色提示，应继续统计")
	}
	// 最大值晚于 2100 年：黄色
	h2, spec2 := epochHarness(100, engine.UnitEpochSec, 1_700_000_000)
	p2 := engine.TimeProbe{Sampled: 100, InSec: 100, Sample: 1700000000, HasRange: true, Min: 1600000000, Max: 1_700_000_000_000, GuessUnit: engine.UnitEpochSec}
	h2.Src.ProbeRes = p2
	wantLevel(t, runCheck(t, h2, spec2), "time_unit", "warn", "最大值 1700000000000 大于 10 位时间戳的合理上限")
	// 没有大于 0 的值：黄色，不拦截
	h3, spec3 := epochHarness(100, engine.UnitEpochMs, 1_700_000_000_000)
	h3.Src.ProbeRes = engine.TimeProbe{}
	rep3 := runCheck(t, h3, spec3)
	wantLevel(t, rep3, "time_unit", "warn", "没有大于 0 的值")
	if rep3.HasError() {
		t.Errorf("不应拦截：%+v", rep3.Items)
	}
	// 探测失败：黄色，不拦截
	h4, spec4 := epochHarness(100, engine.UnitEpochMs, 1_700_000_000_000)
	h4.Src.ProbeErr = errors.New("context deadline exceeded")
	rep4 := runCheck(t, h4, spec4)
	wantLevel(t, rep4, "time_unit", "warn", "无法核对时间单位：context deadline exceeded")
	if rep4.HasError() {
		t.Errorf("不应拦截：%+v", rep4.Items)
	}
}

// noProbeSource 把 MemSource 包一层，只露出 SourceDB 接口（没有 ProbeTime）。
type noProbeSource struct{ engine.SourceDB }

func TestCheckEpochUnitSourceCannotProbe(t *testing.T) {
	h, spec := epochHarness(100, engine.UnitEpochMs, 1_700_000_000_000)
	env := h.EnvFor(spec, t.TempDir())
	env.Src = noProbeSource{env.Src}
	rep, err := newEng().Check(t.Context(), spec, h.Policy(), env)
	if err != nil {
		t.Fatal(err)
	}
	wantLevel(t, rep, "time_unit", "warn", "数据源不支持核对时间单位")
	if rep.EstRows != 100 {
		t.Error("不能探测时只给黄色提示，应继续统计")
	}
}

func TestCheckEpochUnitColumnTypes(t *testing.T) {
	// 选了秒 / 毫秒，但列是 datetime
	h := tableOfMany(1000)
	spec := h.Spec()
	spec.TimeUnit = engine.UnitEpochMs
	rep := runCheck(t, h, spec)
	wantLevel(t, rep, "time_column", "error", "时间列必须是 int / bigint，而 created_at 的类型是 datetime")
	if h.Src.ProbeCalls != 0 || rep.EstRows != 0 {
		t.Errorf("类型不符时不应探测 / 统计：calls=%d rows=%d", h.Src.ProbeCalls, rep.EstRows)
	}
	// 没选单位（升级前的任务），但列是 bigint：提示改选单位，而不是默默按 NOW() 去比
	h2, spec2 := epochHarness(100, engine.UnitDatetime, 1_700_000_000_000)
	rep2 := runCheck(t, h2, spec2)
	wantLevel(t, rep2, "time_column", "error", "请把时间单位选为 10 位（秒）或 13 位（毫秒）")
	if item(rep2, "time_unit") != nil || rep2.EstRows != 0 {
		t.Errorf("不应统计：%+v", rep2.Items)
	}
	// 不支持的整型宽度
	h3, spec3 := epochHarness(100, engine.UnitEpochSec, 1_700_000_000)
	h3.Src.Info.Columns[1].Type = "mediumint(8)"
	wantLevel(t, runCheck(t, h3, spec3), "time_column", "error", "必须是 int / bigint")
	// 不认识的单位
	h4, spec4 := epochHarness(100, "ms", 1_700_000_000)
	rep4 := runCheck(t, h4, spec4)
	wantLevel(t, rep4, "time_unit", "error", "时间单位无效")
	if rep4.EstRows != 0 {
		t.Error("单位无效不应统计")
	}
	// datetime 列的检查报告里不应出现“时间单位”项（与升级前一致）
	h5 := tableOfMany(1000)
	if item(runCheck(t, h5, h5.Spec()), "time_unit") != nil {
		t.Error("datetime 列不应有时间单位项")
	}
}

// ---- 运行 ----

func TestRunEpochMs(t *testing.T) {
	h, spec := epochHarness(250, engine.UnitEpochMs, 1_600_000_000_000)
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultSuccess || res.Deleted != 250 || res.Err != "" {
		t.Fatalf("结果：%+v", res)
	}
	want := "`created_at` > 0 AND `created_at` < (UNIX_TIMESTAMP() - 7776000) * 1000"
	for _, w := range h.Src.Wheres {
		if w != want {
			t.Errorf("读取条件 %q", w)
		}
	}
	for _, d := range h.Src.DeleteReqs {
		if d.Where != want {
			t.Errorf("删除请求必须重复归档条件：%q", d.Where)
		}
	}
	checkOrder(t, h)
}

// 列里其实是 10 位秒，任务却按 13 位毫秒算：条件会命中整张表。第一批读到数据就必须停下，
// 不备份、不删除，并且自动暂停任务。
func TestRunEpochUnitGuardStopsBeforeAnyWrite(t *testing.T) {
	h, spec := epochHarness(250, engine.UnitEpochMs, 1_700_000_000) // 秒级数据
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultFailed || !res.AutoPause || !strings.Contains(res.Err, "时间单位疑似选错") {
		t.Fatalf("应失败并自动暂停：%+v", res)
	}
	if h.Src.Deletes != 0 || h.Src.Len() != 250 || res.Deleted != 0 || res.Backed != 0 {
		t.Errorf("不应删除任何数据：deletes=%d 剩 %d 行 结果 %+v", h.Src.Deletes, h.Src.Len(), res)
	}
	if rows := h.Dst.Table("shop_arch", "orders_bak"); len(rows) != 0 {
		t.Errorf("不应写任何备份：%d 行", len(rows))
	}
	// 同样的数据按秒算则正常（条件里不会再命中毫秒区间之外的行）
	h2, spec2 := epochHarness(250, engine.UnitEpochSec, 1_600_000_000)
	if res := mustRun(t, h2, spec2, h2.Policy(), nil); res.Result != engine.ResultSuccess || res.Deleted != 250 {
		t.Fatalf("按秒应正常：%+v", res)
	}
}

// ---- 真实的 MySQL 数据源（假的 MySQL 服务端）----

func TestProbeTimeThroughRealSource(t *testing.T) {
	f := newFakeMy(10)
	ev := &fakeTbl{cols: []string{"id", "ts_ms", "ts_s", "mixed", "zeros"}, types: []string{"bigint(20)", "bigint(20)", "int(10) unsigned", "bigint(20)", "bigint(20)"},
		null: []string{"NO", "NO", "NO", "NO", "NO"}, pk: []string{"id"}}
	for i := 1; i <= 50; i++ {
		mixed := int64(1_700_000_000_000 + i)
		if i%2 == 0 {
			mixed = int64(1_700_000_000 + i)
		}
		ev.rows = append(ev.rows, []any{int64(i), int64(1_700_000_000_000 + i*1000), int64(1_700_000_000 + i), mixed, int64(0)})
	}
	f.tables["shop.events"] = ev
	op, _ := openerFor(f, t)
	env, err := op.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	pr, ok := env.Src.(engine.TimeProber)
	if !ok {
		t.Fatal("MySQL 数据源应实现 TimeProber")
	}
	ctx := t.Context()

	p, err := pr.ProbeTime(ctx, "shop", "events", "ts_ms", true)
	if err != nil {
		t.Fatal(err)
	}
	if p.Sampled != 50 || p.InMs != 50 || p.InSec != 0 || p.GuessUnit != engine.UnitEpochMs || !p.HasRange ||
		p.Min != 1_700_000_001_000 || p.Max != 1_700_000_050_000 || p.OffsetSec != 8*3600 || p.Sample != 1_700_000_001_000 {
		t.Errorf("毫秒列：%+v", p)
	}
	if !f.saw("`ts_ms` > 0 LIMIT 1000") || !f.saw("SELECT MIN(`ts_ms`), MAX(`ts_ms`)") {
		t.Errorf("探测语句：%v", f.queries)
	}

	p, err = pr.ProbeTime(ctx, "shop", "events", "ts_s", false) // 不允许取最早 / 最晚值
	if err != nil {
		t.Fatal(err)
	}
	if p.Sampled != 50 || p.InSec != 50 || p.GuessUnit != engine.UnitEpochSec || p.HasRange {
		t.Errorf("秒列：%+v", p)
	}
	if f.saw("MIN(`ts_s`)") {
		t.Error("rangeOK=false 时不应发 MIN / MAX")
	}

	p, err = pr.ProbeTime(ctx, "shop", "events", "mixed", true) // 一半秒一半毫秒：猜不出
	if err != nil {
		t.Fatal(err)
	}
	if p.InSec != 25 || p.InMs != 25 || p.GuessUnit != "" {
		t.Errorf("混用列：%+v", p)
	}

	p, err = pr.ProbeTime(ctx, "shop", "events", "zeros", true) // 全是 0：没有可核对的值
	if err != nil {
		t.Fatal(err)
	}
	if p.Sampled != 0 || p.HasRange || p.GuessUnit != "" {
		t.Errorf("全 0 列：%+v", p)
	}

	if _, err := pr.ProbeTime(ctx, "shop", "events", "bad`col", true); err == nil {
		t.Error("非法列名应报错")
	}
	if _, err := pr.ProbeTime(ctx, "shop", "nope", "ts_ms", true); err == nil {
		t.Error("表不存在应报错")
	}
	// 探测期间 ctx 已取消
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := pr.ProbeTime(cctx, "shop", "events", "ts_ms", true); err == nil {
		t.Error("ctx 已取消应报错")
	}
}
