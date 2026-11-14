// @author: kevinkelly
// @license: Apache Licence
// @file: schedule_test.go
// @time: 2026/10/28

package archive

import (
	"strings"
	"testing"
	"time"

	"dbtool/internal/archive/engine"
)

var loc = time.FixedZone("CST", 8*3600)

func at(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, loc) }

func TestParseHM(t *testing.T) {
	for in, want := range map[string]int{"00:00": 0, "01:30": 90, "1:05": 65, "23:59": 1439} {
		if got, ok := parseHM(in); !ok || got != want {
			t.Errorf("parseHM(%q)=%d,%v want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "24:00", "12:60", "12", "ab:cd", "12:5", "-1:00"} {
		if _, ok := parseHM(in); ok {
			t.Errorf("parseHM(%q) 应失败", in)
		}
	}
}

func TestWindow(t *testing.T) {
	w := engine.Window{Start: "01:00", End: "05:00"}
	if n, ok := windowMinutes(w); !ok || n != 240 {
		t.Errorf("窗口长度 %d,%v", n, ok)
	}
	cross := engine.Window{Start: "22:00", End: "02:00"}
	if n, _ := windowMinutes(cross); n != 240 {
		t.Errorf("跨零点窗口长度 %d", n)
	}
	cases := []struct {
		w    engine.Window
		h, m int
		in   bool
	}{
		{w, 0, 59, false}, {w, 1, 0, true}, {w, 4, 59, true}, {w, 5, 0, false},
		{cross, 23, 0, true}, {cross, 1, 59, true}, {cross, 2, 0, false}, {cross, 12, 0, false}, {cross, 22, 0, true},
		{engine.Window{Start: "03:00", End: "03:00"}, 12, 0, true}, // Start==End 不限制
	}
	for _, c := range cases {
		if got := inWindow(at(2026, 10, 8, c.h, c.m), c.w); got != c.in {
			t.Errorf("%v %02d:%02d inWindow=%v want %v", c.w, c.h, c.m, got, c.in)
		}
	}
	if !windowUnlimited(engine.Window{Start: "03:00", End: "03:00"}) || windowUnlimited(w) {
		t.Error("windowUnlimited")
	}
	if got := windowText(cross); !strings.Contains(got, "跨零点") || !strings.Contains(got, "4 小时") {
		t.Errorf("windowText=%q", got)
	}
	// 窗口集合：扩大判断
	if looserParts(engine.Params{Window: w}, engine.Params{Window: engine.Window{Start: "01:00", End: "04:00"}}) != nil {
		t.Error("缩小窗口不是放宽")
	}
	if l := looserParts(engine.Params{Window: w}, engine.Params{Window: engine.Window{Start: "00:30", End: "05:00"}}); len(l) != 1 || l[0] != "执行窗口扩大" {
		t.Errorf("扩大窗口应被识别: %v", l)
	}
	if l := looserParts(engine.Params{Window: w}, engine.Params{Window: engine.Window{Start: "03:00", End: "03:00"}}); len(l) == 0 {
		t.Error("改成不限制是扩大")
	}
}

func TestParseCron(t *testing.T) {
	good := []string{"0 2 * * *", "*/15 * * * *", "0 1-5 * * 1-5", "30 3 1,15 * *", "0 0 * * 7", "5/20 * * * *"}
	for _, s := range good {
		if _, err := parseCron(s); err != nil {
			t.Errorf("%q 应合法: %v", s, err)
		}
	}
	bad := map[string]string{"0 2 * *": "5 段", "60 * * * *": "超出范围", "* 24 * * *": "超出范围", "0 0 0 * *": "超出范围", "* * * 13 *": "超出范围",
		"a * * * *": "不是有效", "*/0 * * * *": "步长", "5-1 * * * *": "超出范围"}
	for s, msg := range bad {
		if _, err := parseCron(s); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q 应报含 %q 的错: %v", s, msg, err)
		}
	}
}

func TestScheduleTimes(t *testing.T) {
	from := at(2026, 10, 8, 11, 20) // 周四
	fmtL := func(ts []time.Time) []string {
		var o []string
		for _, x := range ts {
			o = append(o, x.Format("01-02 15:04"))
		}
		return o
	}
	eq := func(name string, got []time.Time, want ...string) {
		t.Helper()
		g := fmtL(got)
		if strings.Join(g, ",") != strings.Join(want, ",") {
			t.Errorf("%s: got %v want %v", name, g, want)
		}
	}
	eq("每天", scheduleTimes(Schedule{Kind: "day", Time: "02:00"}, from, 3), "10-09 02:00", "10-10 02:00", "10-11 02:00")
	eq("每天-今天还没到", scheduleTimes(Schedule{Kind: "day", Time: "23:00"}, from, 2), "10-08 23:00", "10-09 23:00")
	eq("每周一三", scheduleTimes(Schedule{Kind: "week", Time: "01:30", Weekdays: []int{1, 3}}, from, 3), "10-12 01:30", "10-14 01:30", "10-19 01:30")
	eq("每周日", scheduleTimes(Schedule{Kind: "week", Time: "03:00", Weekdays: []int{0}}, from, 2), "10-11 03:00", "10-18 03:00")
	eq("每月1日", scheduleTimes(Schedule{Kind: "month", Time: "02:00", MonthDay: 1}, from, 3), "11-01 02:00", "12-01 02:00", "01-01 02:00")
	eq("每月28日", scheduleTimes(Schedule{Kind: "month", Time: "02:00", MonthDay: 28}, from, 2), "10-28 02:00", "11-28 02:00")
	eq("cron 每2小时", scheduleTimes(Schedule{Kind: "cron", Cron: "0 */2 * * *"}, from, 3), "10-08 12:00", "10-08 14:00", "10-08 16:00")
	eq("cron 工作日", scheduleTimes(Schedule{Kind: "cron", Cron: "0 9 * * 1-5"}, at(2026, 10, 9, 10, 0), 2), "10-12 09:00", "10-13 09:00")
	eq("cron 日与周并集", scheduleTimes(Schedule{Kind: "cron", Cron: "0 4 15 * 1"}, at(2026, 10, 8, 0, 0), 3), "10-12 04:00", "10-15 04:00", "10-19 04:00")
	eq("cron 周日用 7", scheduleTimes(Schedule{Kind: "cron", Cron: "0 4 * * 7"}, from, 1), "10-11 04:00")
	eq("严格晚于 from", scheduleTimes(Schedule{Kind: "day", Time: "11:20"}, from, 1), "10-09 11:20")
	if scheduleTimes(Schedule{Kind: "cron", Cron: "bad"}, from, 3) != nil || scheduleTimes(Schedule{Kind: "day", Time: "99:99"}, from, 3) != nil {
		t.Error("非法调度应返回空")
	}
	if !nextRun(Schedule{Kind: "day", Time: "02:00"}, from).Equal(at(2026, 10, 9, 2, 0)) {
		t.Error("nextRun")
	}
	if got := scheduleText(Schedule{Kind: "week", Time: "01:30", Weekdays: []int{3, 1}}); got != "每周一、三 01:30" {
		t.Errorf("scheduleText=%q", got)
	}
}

func TestValidateSchedule(t *testing.T) {
	now := at(2026, 10, 8, 11, 20)
	w := engine.Window{Start: "01:00", End: "05:00"}
	msgs := func(s Schedule, w engine.Window) string {
		var o []string
		for _, i := range validateSchedule(s, w, now) {
			o = append(o, i.Field+":"+i.Msg)
		}
		return strings.Join(o, "|")
	}
	if got := msgs(Schedule{Kind: "day", Time: "02:00"}, w); got != "" {
		t.Errorf("合法调度报错: %s", got)
	}
	for name, c := range map[string]struct {
		s    Schedule
		w    engine.Window
		want string
	}{
		"时间格式":       {Schedule{Kind: "day", Time: "2am"}, w, "schedule.time"},
		"周缺星期":       {Schedule{Kind: "week", Time: "02:00"}, w, "schedule.weekdays"},
		"周越界":        {Schedule{Kind: "week", Time: "02:00", Weekdays: []int{7}}, w, "schedule.weekdays"},
		"月日期":        {Schedule{Kind: "month", Time: "02:00", MonthDay: 31}, w, "schedule.month_day"},
		"未知类型":       {Schedule{Kind: "hourly"}, w, "schedule.kind"},
		"cron 非法":    {Schedule{Kind: "cron", Cron: "* *"}, w, "schedule.cron"},
		"cron 太密":    {Schedule{Kind: "cron", Cron: "*/30 * * * *"}, w, "最短间隔"},
		"开始不在窗口":     {Schedule{Kind: "day", Time: "12:00"}, w, "不在执行窗口"},
		"cron 全在窗口外": {Schedule{Kind: "cron", Cron: "0 12 * * *"}, w, "窗口之外"},
	} {
		if got := msgs(c.s, c.w); !strings.Contains(got, c.want) {
			t.Errorf("%s: 期望含 %q，得到 %q", name, c.want, got)
		}
	}
	if got := msgs(Schedule{Kind: "cron", Cron: "0 */2 * * *"}, engine.Window{Start: "00:00", End: "00:00"}); got != "" {
		t.Errorf("不限制窗口 + 每 2 小时 cron 应合法: %s", got)
	}
	// 跨零点窗口里的 23:30 合法
	if got := msgs(Schedule{Kind: "day", Time: "23:30"}, engine.Window{Start: "22:00", End: "02:00"}); got != "" {
		t.Errorf("跨零点窗口: %s", got)
	}
}

func TestDecideDue(t *testing.T) {
	s := Schedule{Kind: "day", Time: "02:00"}
	w := engine.Window{Start: "01:00", End: "05:00"}
	now := at(2026, 10, 9, 2, 0)

	if d, n := decideDue(time.Time{}, now, s, w); d != dueNo || !n.Equal(at(2026, 10, 10, 2, 0)) {
		t.Errorf("未计算过：只算下次 %v %v", d, n)
	}
	if d, n := decideDue(at(2026, 10, 9, 2, 0).Add(time.Minute), now, s, w); d != dueNo || !n.Equal(at(2026, 10, 9, 2, 1)) {
		t.Errorf("还没到点 %v %v", d, n)
	}
	if d, n := decideDue(at(2026, 10, 9, 2, 0), now, s, w); d != dueRun || !n.Equal(at(2026, 10, 10, 2, 0)) {
		t.Errorf("到点且在窗口内 %v %v", d, n)
	}
	// 服务停了一阵，03:10 才回来：窗口内补跑
	if d, _ := decideDue(at(2026, 10, 9, 2, 0), at(2026, 10, 9, 3, 10), s, w); d != dueRun {
		t.Error("窗口内补跑")
	}
	// 06:00 才回来：已错过窗口，不跑，推进到明天
	if d, n := decideDue(at(2026, 10, 9, 2, 0), at(2026, 10, 9, 6, 0), s, w); d != dueMissed || !n.Equal(at(2026, 10, 10, 2, 0)) {
		t.Errorf("错过窗口 %v %v", d, n)
	}
	if d, _ := decideDue(time.Time{}, now, Schedule{Kind: "cron", Cron: "bad"}, w); d != dueNoSched {
		t.Error("坏调度")
	}
}
