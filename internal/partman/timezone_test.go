// @author: kevinkelly
// @license: Apache Licence
// @file: timezone_test.go
// @time: 2026/10/28

package partman

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// 偏移（秒）取 2026-01-01 的值；期望值来自原程序 loadLocation 的输出（见 grid_golden_test.go 的说明）。
func TestParseTimezoneMatchesOriginalForAcceptedForms(t *testing.T) {
	cases := []struct {
		spec string
		off  int
		name string
	}{
		{"UTC", 0, "UTC"}, {"GMT", 0, "UTC"}, {"UTC+8", 28800, "UTC+08:00"}, {"GMT+08:00", 28800, "UTC+08:00"},
		{"+0800", 28800, "UTC+08:00"}, {"+8", 28800, "UTC+08:00"}, {"-05:00", -18000, "UTC-05:00"}, {"UTC-0", 0, "UTC"},
		{"+05:30", 19800, "UTC+05:30"}, {"+14:00", 50400, "UTC+14:00"}, {"UTC+8:30", 30600, "UTC+08:30"},
		{"Asia/Shanghai", 28800, "Asia/Shanghai"}, {"America/New_York", -18000, "America/New_York"}, {"utc+8", 28800, "UTC+08:00"},
		{"Asia/Tokyo", 32400, "Asia/Tokyo"}, {"Europe/London", 0, "Europe/London"}, {"-3", -10800, "UTC-03:00"},
	}
	for _, c := range cases {
		tz, err := ParseTimezone(c.spec)
		if err != nil {
			t.Errorf("%q: %v", c.spec, err)
			continue
		}
		if tz.FollowDB || tz.Loc == nil {
			t.Errorf("%q: 不应跟随目标库", c.spec)
			continue
		}
		_, off := time.Date(2026, 1, 1, 0, 0, 0, 0, tz.Loc).Zone()
		if off != c.off || tz.Name != c.name {
			t.Errorf("%q: 偏移 %d 名字 %s，期望 %d %s", c.spec, off, tz.Name, c.off, c.name)
		}
	}
}

func TestParseTimezoneRejects(t *testing.T) {
	long := "America/Argentina/Buenos_Aires" // 30 个字符，超过 timezone varchar(20)
	cases := map[string]string{
		"Europe/Nowhere": "无法识别",
		"bogus":          "无法识别",
		"+abc":           "无法识别的时区偏移",
		"+25:00":         "无法识别的时区偏移",
		"+08:99":         "无法识别的时区偏移",
		"UTC+9999":       "无法识别的时区偏移",
		"Local":          "无法识别",
		"../etc/passwd":  "无法识别",
		long:             "超过 20 个字符",
	}
	for spec, want := range cases {
		_, err := ParseTimezone(spec)
		if err == nil {
			t.Errorf("%q 应拒绝", spec)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v 不含 %q", spec, err, want)
		}
		wantCode(t, err, "bad_request")
	}
}

func TestParseTimezoneFollowDBAndDSTWarning(t *testing.T) {
	tz, err := ParseTimezone("")
	if err != nil || !tz.FollowDB {
		t.Fatalf("空值应跟随目标库: %+v %v", tz, err)
	}
	// 有夏令时：warning 要说清楚建议用固定偏移
	ny, err := ParseTimezone("America/New_York")
	if err != nil || len(ny.Warnings) != 1 || !strings.Contains(ny.Warnings[0], "夏令时") || !strings.Contains(ny.Warnings[0], "固定偏移") || !strings.Contains(ny.Warnings[0], "-05:00") {
		t.Fatalf("夏令时 warning: %+v %v", ny, err)
	}
	// 没有夏令时 / 固定偏移：没有 warning
	for _, s := range []string{"Asia/Shanghai", "UTC", "+08:00", "Asia/Tokyo", "Asia/Singapore"} {
		z, err := ParseTimezone(s)
		if err != nil || len(z.Warnings) != 0 {
			t.Errorf("%s 不应有 warning: %+v %v", s, z, err)
		}
	}
}

func TestResolveTimezoneFollowsSessionOffset(t *testing.T) {
	st := &TableState{SessionOffset: 3600*5 + 1800, HasSessionOffset: true}
	tz, err := resolveTimezone("", st)
	if err != nil {
		t.Fatal(err)
	}
	if _, off := time.Date(2026, 1, 1, 0, 0, 0, 0, tz.Loc).Zone(); off != 19800 || !strings.Contains(tz.Name, "跟随目标库") {
		t.Fatalf("%+v", tz)
	}
	// 读不到会话偏移：明确报错，不猜
	_, err = resolveTimezone("", &TableState{})
	wantCode(t, err, "timezone_unknown")
	// 配置了时区就不看会话偏移
	tz, err = resolveTimezone("UTC", &TableState{})
	if err != nil || tz.Loc != time.UTC {
		t.Fatalf("%+v %v", tz, err)
	}
}

// 空时区的配置按目标库会话时区算边界（Doris 同理）。
func TestPlanWithEmptyTimezoneUsesSessionOffset(t *testing.T) {
	cfg := baseCfg()
	cfg.Timezone, cfg.PreCreate, cfg.Retain = "", 1, 0
	st := dtState(rp("p20261007", "'2026-10-08 00:00:00'"))
	st.SessionOffset, st.HasSessionOffset = 8*3600, true
	// UTC 2026-10-06 18:30 = 目标库本地 2026-10-07 02:30：当前窗口是 10-07（边界 10-08），要预创建 10-08 那个
	p, err := BuildPlan(cfg, st, time.Date(2026, 10, 6, 18, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	wantSQL(t, p, "ALTER TABLE `log`.`access_log` ADD PARTITION (PARTITION `p20261008` VALUES LESS THAN ('2026-10-09 00:00:00'))")
	if !strings.Contains(p.Timezone, "UTC+08:00") {
		t.Fatalf("展示的时区: %s", p.Timezone)
	}
	// bigint 边界是该偏移下的毫秒
	cfg.ColType = ColBigint
	ms := func(y, m, d int) string {
		return strconv.FormatInt(time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.FixedZone("x", 8*3600)).UnixMilli(), 10)
	}
	st = TableState{Exists: true, IsBase: true, ColumnType: "bigint", ColumnTypeFull: "bigint", Method: "RANGE", Expression: "`created_at`",
		Partitions: []RawPartition{rp("p20261007", ms(2026, 10, 8))}, SessionOffset: 8 * 3600, HasSessionOffset: true}
	p, err = BuildPlan(cfg, st, time.Date(2026, 10, 6, 18, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	wantSQL(t, p, "ALTER TABLE `log`.`access_log` ADD PARTITION (PARTITION `p20261008` VALUES LESS THAN ("+ms(2026, 10, 9)+"))")
}

func TestConfigValidateTimezone(t *testing.T) {
	in := goodInput()
	for _, tz := range []string{"", "UTC", "Asia/Shanghai", "America/New_York", "UTC+8", "GMT+08:00", "+0800", "+8", " +08:00 "} {
		in.Timezone = tz
		if err := in.validate(); err != nil {
			t.Errorf("%q: %v", tz, err)
		}
	}
	in.Timezone = "America/New_York"
	_ = in.validate()
	if len(in.warnings) != 1 || !strings.Contains(in.warnings[0], "固定偏移") {
		t.Fatalf("warnings: %v", in.warnings)
	}
	in.Timezone = "America/Argentina/Buenos_Aires"
	if err := in.validate(); err == nil || !strings.Contains(err.Error(), "超过 20 个字符") {
		t.Fatalf("%v", err)
	}
	// 通过 Create：响应带 warnings
	e := newEnv()
	in = goodInput()
	in.Timezone = "Europe/London"
	v, err := e.svc.Create(root, in)
	if err != nil || len(v.Warnings) != 1 || v.Timezone != "Europe/London" {
		t.Fatalf("%+v %v", v, err)
	}
	// 夏令时 warning 也出现在 draft 预览里
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-07")
	d := goodInput()
	d.Timezone = "America/New_York"
	pv, err := e.svc.Preview(ctx, root, 0, &d, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range pv.Warnings {
		found = found || strings.Contains(w, "夏令时")
	}
	if !found || pv.Timezone != "America/New_York" {
		t.Fatalf("%+v", pv)
	}
}
