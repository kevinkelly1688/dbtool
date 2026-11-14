// @author: kevinkelly
// @license: Apache Licence
// @file: timeunit_test.go
// @time: 2026/10/28

package engine

import (
	"math"
	"strings"
	"testing"
)

func TestNormalizeTimeUnit(t *testing.T) {
	for in, want := range map[string]string{
		"": UnitDatetime, " ": UnitDatetime, "datetime": UnitDatetime, " DATETIME ": UnitDatetime,
		"epoch_s": UnitEpochSec, "EPOCH_S": UnitEpochSec, "epoch_ms": UnitEpochMs, " Epoch_Ms ": UnitEpochMs,
	} {
		got, ok := NormalizeTimeUnit(in)
		if !ok || got != want {
			t.Errorf("NormalizeTimeUnit(%q) = %q, %v，期望 %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"ms", "s", "epoch", "epoch_us", "seconds", "date", "timestamp", "13"} {
		if _, ok := NormalizeTimeUnit(bad); ok {
			t.Errorf("%q 不应被接受", bad)
		}
	}
	if IsEpochUnit(UnitDatetime) || !IsEpochUnit(UnitEpochSec) || !IsEpochUnit(UnitEpochMs) {
		t.Error("IsEpochUnit")
	}
	if EpochDigits(UnitEpochSec) != 10 || EpochDigits(UnitEpochMs) != 13 || EpochDigits(UnitDatetime) != 0 {
		t.Error("EpochDigits")
	}
}

func TestIsIntegerTimeType(t *testing.T) {
	yes := []string{"bigint", "bigint(20)", "bigint(20) unsigned", "BIGINT UNSIGNED", "int", "int(11)", "int(10) unsigned", "INT UNSIGNED", "integer", " bigint(20) "}
	no := []string{"", "tinyint(1)", "smallint(6)", "mediumint(8) unsigned", "decimal(20,0)", "double", "varchar(13)", "char(10)", "datetime", "timestamp", "date", "bit(1)", "interval"}
	for _, s := range yes {
		if !IsIntegerTimeType(s) {
			t.Errorf("%q 应是可存时间戳的整型", s)
		}
	}
	for _, s := range no {
		if IsIntegerTimeType(s) {
			t.Errorf("%q 不应算整型时间戳列", s)
		}
	}
}

func TestBuildWhereEpoch(t *testing.T) {
	s := Spec{TimeColumn: "CreateTime", TimeUnit: UnitEpochMs, RetentionDays: 180}
	got, err := BuildWhere(s)
	if err != nil || got != "`CreateTime` > 0 AND `CreateTime` < (UNIX_TIMESTAMP() - 15552000) * 1000" {
		t.Fatalf("毫秒：%q, %v", got, err)
	}
	s.TimeUnit = UnitEpochSec
	got, err = BuildWhere(s)
	if err != nil || got != "`CreateTime` > 0 AND `CreateTime` < UNIX_TIMESTAMP() - 15552000" {
		t.Fatalf("秒：%q, %v", got, err)
	}
	// 附加条件照常拼在后面；OR 必须被括号包住，不能和前面的时间条件混在一起
	s.ExtraWhere = "ReadTime > 0 OR Status = 1"
	got, err = BuildWhere(s)
	if err != nil || got != "`CreateTime` > 0 AND `CreateTime` < UNIX_TIMESTAMP() - 15552000 AND (`ReadTime` > 0 OR `Status` = 1)" {
		t.Fatalf("附加条件：%q, %v", got, err)
	}
	// datetime（含显式写 datetime）与升级前完全一致
	for _, u := range []string{"", "datetime", " DateTime "} {
		d := Spec{TimeColumn: "created_at", TimeUnit: u, RetentionDays: 180}
		if got, err := BuildWhere(d); err != nil || got != "`created_at` < NOW() - INTERVAL 180 DAY" {
			t.Errorf("unit=%q：%q, %v", u, got, err)
		}
	}
	for _, bad := range []string{"ms", "epoch", "epoch_us", "13", "`; drop table x"} {
		if _, err := BuildWhere(Spec{TimeColumn: "ts", TimeUnit: bad, RetentionDays: 30}); err == nil || !strings.Contains(err.Error(), "时间单位无效") {
			t.Errorf("unit=%q 应报“时间单位无效”：%v", bad, err)
		}
	}
	// 非常大的保留天数也不会溢出
	if got, _ := BuildWhere(Spec{TimeColumn: "ts", TimeUnit: UnitEpochMs, RetentionDays: 3650}); !strings.Contains(got, "315360000") {
		t.Errorf("3650 天应是 315360000 秒：%s", got)
	}
}

func TestValidateTimeUnit(t *testing.T) {
	base := Spec{Name: "events 归档", InstanceID: 1, DB: "shop", Table: "events", TimeColumn: "ts", RetentionDays: 90, Mode: ModePurge}
	pol := Policy{MinRetentionDays: 30, AllowPurgeNoBackup: true}
	for _, u := range []string{"", "datetime", UnitEpochSec, UnitEpochMs} {
		s := base
		s.TimeUnit = u
		for _, i := range validateSpec(s, pol, true) {
			if i.Field == "time_unit" {
				t.Errorf("unit=%q 不应有时间单位问题：%+v", u, i)
			}
		}
	}
	s := base
	s.TimeUnit = "ms"
	var found bool
	for _, i := range validateSpec(s, pol, true) {
		if i.Field == "time_unit" && i.Level == "error" && strings.Contains(i.Msg, "时间单位无效") {
			found = true
		}
	}
	if !found {
		t.Error("不认识的时间单位应报 time_unit 错误")
	}
}

func TestFormatEpoch(t *testing.T) {
	const cst = 8 * 3600
	for _, c := range []struct {
		v    int64
		unit string
		off  int
		want string
	}{
		{1700000000, UnitEpochSec, 0, "2023-11-14 22:13:20"},
		{1700000000, UnitEpochSec, cst, "2023-11-15 06:13:20"},
		{1700000000999, UnitEpochMs, cst, "2023-11-15 06:13:20"},
		{1700000000000, UnitEpochMs, -5 * 3600, "2023-11-14 17:13:20"},
		{math.MaxInt64, UnitEpochMs, 0, "9223372036854775807"}, // 超出可显示范围：原样输出数字
	} {
		if got := FormatEpoch(c.v, c.unit, c.off); got != c.want {
			t.Errorf("FormatEpoch(%d, %s, %d) = %q，期望 %q", c.v, c.unit, c.off, got, c.want)
		}
	}
}

func TestOffsetSeconds(t *testing.T) {
	for in, want := range map[string]int{"08:00:00": 28800, "-05:30:00": -19800, "+01:00:00": 3600, "00:00:00": 0, "": 0, "garbage": 0} {
		if got := offsetSeconds(in); got != want {
			t.Errorf("offsetSeconds(%q) = %d，期望 %d", in, got, want)
		}
	}
}

func TestParseEpochInt(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "1700000000000": 1700000000000, " 42 ": 42, "18446744073709551615": math.MaxInt64} {
		if got, ok := parseEpochInt(in); !ok || got != want {
			t.Errorf("parseEpochInt(%q) = %d, %v，期望 %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "abc", "1.5", "-99999999999999999999"} {
		if _, ok := parseEpochInt(in); ok {
			t.Errorf("parseEpochInt(%q) 应失败", in)
		}
	}
}

func TestBatchTimeMax(t *testing.T) {
	b := Batch{Cols: []string{"id", "CreateTime", "note"}, Rows: []Row{
		{int64(1), int64(1700000000000), "a"},
		{int64(2), []byte("1760000000000"), "b"},
		{int64(3), uint64(1650000000000), "c"},
		{int64(4), nil, "d"},
	}}
	if mx, ok := batchTimeMax(b, "createtime"); !ok || mx != 1760000000000 { // 列名不区分大小写
		t.Errorf("max = %d, %v", mx, ok)
	}
	if _, ok := batchTimeMax(b, "nope"); ok {
		t.Error("没有这一列应返回 ok=false")
	}
	if _, ok := batchTimeMax(Batch{Cols: []string{"ts"}, Rows: []Row{{nil}, {"x"}}}, "ts"); ok {
		t.Error("一个值都解析不了应返回 ok=false")
	}
	if mx, ok := batchTimeMax(Batch{Cols: []string{"ts"}, Rows: []Row{{uint64(math.MaxUint64)}}}, "ts"); !ok || mx != math.MaxInt64 {
		t.Errorf("无符号极端值应按 MaxInt64：%d, %v", mx, ok)
	}
}
