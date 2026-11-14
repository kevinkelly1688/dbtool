// @author: kevinkelly
// @license: Apache Licence
// @file: compute_test.go
// @time: 2026/10/28

package duty

import (
	"testing"
	"time"
)

var cst = time.FixedZone("CST", 8*3600)

// at 返回 2026-10-22（周四）的某个时刻；day 偏移天数。
func at(day, h, m int) time.Time { return time.Date(2026, 10, 22+day, h, m, 0, 0, cst) }

// threeShifts 是需求里的例子：张三 08:00–19:00、李四 19:00–24:00、赵五 00:00–08:00，每天。
func threeShifts() []Shift {
	return []Shift{
		{ID: 1, Name: "白班", Username: "zhangsan", StartMin: 8 * 60, EndMin: 19 * 60, Weekdays: AllWeek, Enabled: true},
		{ID: 2, Name: "晚班", Username: "lisi", StartMin: 19 * 60, EndMin: 0, Weekdays: AllWeek, Enabled: true},
		{ID: 3, Name: "夜班", Username: "zhaowu", StartMin: 0, EndMin: 8 * 60, Weekdays: AllWeek, Enabled: true},
	}
}

func TestClockParseAndFormat(t *testing.T) {
	ok := map[string]int{"00:00": 0, "8:00": 480, "08:05": 485, "23:59": 1439}
	for in, want := range ok {
		if got, err := ParseClock(in, false); err != nil || got != want {
			t.Errorf("ParseClock(%q) = %d, %v，期望 %d", in, got, err, want)
		}
	}
	if got, err := ParseClock("24:00", true); err != nil || got != 0 {
		t.Errorf("结束时间 24:00 应等于次日 00:00：%d %v", got, err)
	}
	for _, bad := range []string{"", "24:00", "25:00", "12:60", "12", "ab:cd", "12:5", "-1:00", "1:2:3"} {
		if _, err := ParseClock(bad, false); err == nil {
			t.Errorf("ParseClock(%q) 应报错", bad)
		}
	}
	if _, err := ParseClock("24:30", true); err == nil {
		t.Error("24:30 不合法")
	}
	for _, c := range []struct {
		s, e int
		want string
	}{{480, 1140, "08:00–19:00"}, {1140, 0, "19:00–24:00"}, {0, 480, "00:00–08:00"}, {0, 0, "00:00–24:00"}, {1320, 360, "22:00–06:00"}} {
		if got := RangeText(c.s, c.e); got != c.want {
			t.Errorf("RangeText(%d,%d) = %q，期望 %q", c.s, c.e, got, c.want)
		}
	}
}

func TestShiftDuration(t *testing.T) {
	for _, c := range []struct {
		s, e, min int
		cross     bool
	}{{480, 1140, 660, false}, {1140, 0, 300, true}, {0, 480, 480, false}, {0, 0, 1440, true}, {480, 480, 1440, true}, {1320, 360, 480, true}} {
		sh := Shift{StartMin: c.s, EndMin: c.e}
		if sh.Minutes() != c.min || sh.Cross() != c.cross {
			t.Errorf("%d→%d：时长 %d 跨天 %v，期望 %d %v", c.s, c.e, sh.Minutes(), sh.Cross(), c.min, c.cross)
		}
	}
}

// 需求里的三班倒：逐个边界时刻核对谁在值班、下一次交班是谁、什么时候交。
func TestComputeThreeShiftBoundaries(t *testing.T) {
	shifts := threeShifts()
	cases := []struct {
		name         string
		now          time.Time
		user         string
		segStart     time.Time
		segEnd       time.Time
		nextUser     string
		nextStartsAt time.Time
	}{
		{"07:59 夜班", at(0, 7, 59), "zhaowu", at(0, 0, 0), at(0, 8, 0), "zhangsan", at(0, 8, 0)},
		{"08:00 白班开始", at(0, 8, 0), "zhangsan", at(0, 8, 0), at(0, 19, 0), "lisi", at(0, 19, 0)},
		{"18:59 白班最后一分钟", at(0, 18, 59), "zhangsan", at(0, 8, 0), at(0, 19, 0), "lisi", at(0, 19, 0)},
		{"19:00 晚班开始", at(0, 19, 0), "lisi", at(0, 19, 0), at(1, 0, 0), "zhaowu", at(1, 0, 0)},
		{"23:59 晚班最后一分钟", at(0, 23, 59), "lisi", at(0, 19, 0), at(1, 0, 0), "zhaowu", at(1, 0, 0)},
		{"00:00 夜班开始", at(0, 0, 0), "zhaowu", at(0, 0, 0), at(0, 8, 0), "zhangsan", at(0, 8, 0)},
	}
	for _, c := range cases {
		s := Compute(shifts, nil, c.now)
		if s.Cur.User() != c.user || !s.Cur.Start.Equal(c.segStart) || !s.Cur.End.Equal(c.segEnd) || !s.NextAt.Equal(c.segEnd) {
			t.Errorf("%s：当前 %q [%v, %v) 刷新 %v，期望 %q [%v, %v)", c.name, s.Cur.User(), s.Cur.Start, s.Cur.End, s.NextAt, c.user, c.segStart, c.segEnd)
		}
		if s.Next == nil || s.Next.User() != c.nextUser || !s.Next.Start.Equal(c.nextStartsAt) {
			t.Errorf("%s：下一次交班 %+v，期望 %s %v", c.name, s.Next, c.nextUser, c.nextStartsAt)
		}
	}
}

func TestComputeTodaySegments(t *testing.T) {
	s := Compute(threeShifts(), nil, at(0, 10, 30))
	if len(s.Today) != 3 {
		t.Fatalf("今天应有 3 段：%+v", s.Today)
	}
	want := []struct {
		user       string
		start, end int
	}{{"zhaowu", 0, 8}, {"zhangsan", 8, 19}, {"lisi", 19, 24}}
	for i, w := range want {
		g := s.Today[i]
		if g.User() != w.user || g.Start.Hour() != w.start || (w.end < 24 && g.End.Hour() != w.end) || (w.end == 24 && !g.End.Equal(at(1, 0, 0))) {
			t.Errorf("第 %d 段：%q %v–%v，期望 %s %d–%d", i, g.User(), g.Start, g.End, w.user, w.start, w.end)
		}
	}
	// 昨天开始、今天结束的班（夜班在今天 00:00 才开始，不是接昨天的）：三班首尾相接，没有跨天的段
	for _, g := range s.Today {
		if g.FromPrev || g.ToNext {
			t.Errorf("三班倒不应有跨天的段：%+v", g)
		}
	}
}

// 跨午夜的班属于开始那天：周五 22:00–06:00，周六凌晨 3 点是它的班，周五凌晨 3 点不是。
func TestComputeCrossMidnightBelongsToStartDay(t *testing.T) {
	fri := at(1, 0, 0) // 2026-10-23 周五
	if fri.Weekday() != time.Friday {
		t.Fatalf("测试日期不对：%v", fri.Weekday())
	}
	shifts := []Shift{{ID: 1, Name: "夜班", Username: "zhaowu", StartMin: 22 * 60, EndMin: 6 * 60, Weekdays: 1 << uint(time.Friday), Enabled: true}}
	sat3 := time.Date(2026, 10, 24, 3, 0, 0, 0, cst)
	if g := Compute(shifts, nil, sat3).Cur; g.User() != "zhaowu" || !g.FromPrev {
		t.Errorf("周六 03:00 应由周五的夜班值班且是接昨天的班：%+v", g)
	}
	fri3 := time.Date(2026, 10, 23, 3, 0, 0, 0, cst)
	if g := Compute(shifts, nil, fri3).Cur; g.User() != "" {
		t.Errorf("周五 03:00 没有周四开始的班，应无人值班：%+v", g)
	}
	sat23 := time.Date(2026, 10, 24, 23, 0, 0, 0, cst)
	if g := Compute(shifts, nil, sat23).Cur; g.User() != "" {
		t.Errorf("周六 23:00 周六不在适用星期里，应无人值班：%+v", g)
	}
	fri2330 := time.Date(2026, 10, 23, 23, 30, 0, 0, cst)
	if g := Compute(shifts, nil, fri2330).Cur; g.User() != "zhaowu" || !g.End.Equal(time.Date(2026, 10, 24, 6, 0, 0, 0, cst)) {
		t.Errorf("周五 23:30 应是周五夜班，到周六 06:00 才结束：%+v", g)
	}
	// 周六那一天的时间线：00:00–06:00 接周五的班，之后没人
	segs := Timeline(shifts, nil, time.Date(2026, 10, 24, 0, 0, 0, 0, cst), time.Date(2026, 10, 25, 0, 0, 0, 0, cst))
	if len(segs) != 2 || !segs[0].FromPrev || segs[0].End.Hour() != 6 || segs[1].Slot != nil {
		t.Errorf("周六的时间线不对：%+v", segs)
	}
}

func TestComputeOverridePriority(t *testing.T) {
	shifts := threeShifts()
	swap := Override{ID: 1, Username: "wangwu", Start: at(0, 10, 0), End: at(0, 12, 0), Reason: "请假"}
	if g := Compute(shifts, []Override{swap}, at(0, 11, 0)).Cur; g.User() != "wangwu" || g.Slot.Kind != KindOverride || g.Slot.Reason != "请假" {
		t.Errorf("调班期间应由调班的人值班：%+v", g)
	}
	if g := Compute(shifts, []Override{swap}, at(0, 12, 0)).Cur; g.User() != "zhangsan" {
		t.Errorf("调班结束（不含结束时刻）后回到班次：%+v", g)
	}
	if g := Compute(shifts, []Override{swap}, at(0, 9, 59)).Cur; g.User() != "zhangsan" {
		t.Errorf("调班开始前仍是班次：%+v", g)
	}
	canceled := swap
	canceled.Canceled = true
	if g := Compute(shifts, []Override{canceled}, at(0, 11, 0)).Cur; g.User() != "zhangsan" {
		t.Errorf("已撤销的调班不算：%+v", g)
	}
	// 重叠的两条调班：后建的（ID 大的）优先
	older := Override{ID: 1, Username: "a", Start: at(0, 10, 0), End: at(0, 14, 0)}
	newer := Override{ID: 2, Username: "b", Start: at(0, 11, 0), End: at(0, 12, 0)}
	if g := Compute(nil, []Override{older, newer}, at(0, 11, 30)).Cur; g.User() != "b" {
		t.Errorf("重叠的调班应后建的优先：%+v", g)
	}
	if g := Compute(nil, []Override{older, newer}, at(0, 12, 30)).Cur; g.User() != "a" {
		t.Errorf("后建的调班结束后应回到先建的：%+v", g)
	}
	// 下一次交班：调班结束后回到原来的人 → 对“当前是调班的人”来说就是交班
	s := Compute(shifts, []Override{swap}, at(0, 11, 0))
	if s.Next == nil || s.Next.User() != "zhangsan" || !s.Next.Start.Equal(at(0, 12, 0)) {
		t.Errorf("调班结束时交回给原来的人：%+v", s.Next)
	}
}

// Void 的调班（调班人账号已停用 / 删除）仍占着这段时间：这段时间没人值班，而不是回落给被顶班的人；调班结束后回到班次。
func TestComputeVoidOverrideLeavesAGap(t *testing.T) {
	shifts := threeShifts()
	swap := Override{ID: 1, Username: "gone", Start: at(0, 10, 0), End: at(0, 12, 0), Void: true}
	s := Compute(shifts, []Override{swap}, at(0, 11, 0))
	if s.Cur.Slot != nil || s.Cur.User() != "" {
		t.Fatalf("调班人账号停用：这段时间应无人值班：%+v", s.Cur)
	}
	if !s.NextAt.Equal(at(0, 12, 0)) || s.Next == nil || s.Next.User() != "zhangsan" || !s.Next.Start.Equal(at(0, 12, 0)) {
		t.Errorf("12:00 调班结束后回到班次：nextAt=%v next=%+v", s.NextAt, s.Next)
	}
	if g := Compute(shifts, []Override{swap}, at(0, 9, 59)).Cur; g.User() != "zhangsan" {
		t.Errorf("调班开始前仍是班次：%+v", g)
	}
	if g := Compute(shifts, []Override{swap}, at(0, 12, 0)).Cur; g.User() != "zhangsan" {
		t.Errorf("调班结束后回到班次：%+v", g)
	}
	// 今日的时间线里这一段是空档
	var gap bool
	for _, g := range Compute(shifts, []Override{swap}, at(0, 11, 0)).Today {
		if g.Slot == nil && g.Start.Equal(at(0, 10, 0)) && g.End.Equal(at(0, 12, 0)) {
			gap = true
		}
	}
	if !gap {
		t.Error("时间线里 10:00–12:00 应是一段空档")
	}
	// 同一时间有更晚建的、账号可用的调班：它优先（和普通调班的优先规则一致）
	live := Override{ID: 2, Username: "wangwu", Start: at(0, 11, 0), End: at(0, 11, 30)}
	if g := Compute(shifts, []Override{swap, live}, at(0, 11, 10)).Cur; g.User() != "wangwu" {
		t.Errorf("后建的可用调班优先：%+v", g)
	}
}

func TestComputeDisabledAndOverlapTieBreak(t *testing.T) {
	shifts := threeShifts()
	shifts[0].Enabled = false
	s := Compute(shifts, nil, at(0, 10, 30))
	if s.Cur.User() != "" {
		t.Errorf("停用的班次不参与排班，10:30 应无人值班：%+v", s.Cur)
	}
	if s.Next == nil || s.Next.User() != "lisi" {
		t.Errorf("下一次交班应是晚班：%+v", s.Next)
	}
	// 数据里重叠（校验之外的途径造成）：后开始的优先，开始时间相同取 ID 小的
	a := Shift{ID: 7, Name: "A", Username: "a", StartMin: 8 * 60, EndMin: 20 * 60, Weekdays: AllWeek, Enabled: true}
	b := Shift{ID: 3, Name: "B", Username: "b", StartMin: 8 * 60, EndMin: 18 * 60, Weekdays: AllWeek, Enabled: true}
	c := Shift{ID: 9, Name: "C", Username: "c", StartMin: 12 * 60, EndMin: 14 * 60, Weekdays: AllWeek, Enabled: true}
	if g := Compute([]Shift{a, b, c}, nil, at(0, 9, 0)).Cur; g.User() != "b" {
		t.Errorf("开始时间相同取 ID 小的：%+v", g)
	}
	if g := Compute([]Shift{a, b, c}, nil, at(0, 13, 0)).Cur; g.User() != "c" {
		t.Errorf("后开始的优先：%+v", g)
	}
}

func TestComputeTwentyFourHourShift(t *testing.T) {
	shifts := []Shift{{ID: 1, Name: "全天", Username: "a", StartMin: 8 * 60, EndMin: 8 * 60, Weekdays: AllWeek, Enabled: true}}
	for _, h := range []int{0, 7, 8, 15, 23} {
		if g := Compute(shifts, nil, at(0, h, 0)).Cur; g.User() != "a" {
			t.Errorf("%d 点应由全天班值班：%+v", h, g)
		}
	}
	s := Compute(shifts, nil, at(0, 10, 0))
	if s.Next != nil {
		t.Errorf("同一个人一直值班，没有下一次交班：%+v", s.Next)
	}
	if !s.NextAt.Equal(at(1, 8, 0)) {
		t.Errorf("每个 24 小时的班在 08:00 结束，要重新计算：%v", s.NextAt)
	}
}

func TestComputeNoShiftsMeansNobody(t *testing.T) {
	s := Compute(nil, nil, at(0, 10, 0))
	if s.Cur.User() != "" || s.Next != nil || len(s.Today) != 1 || s.Today[0].Slot != nil {
		t.Errorf("没有班次：%+v", s)
	}
}

func TestFindConflict(t *testing.T) {
	base := threeShifts()
	// 与白班 08:00–19:00 重叠的周一班次
	mon := Shift{Name: "加班", Username: "x", StartMin: 18 * 60, EndMin: 20 * 60, Weekdays: 1 << uint(time.Monday), Enabled: true}
	c := FindConflict(mon, base)
	if c == nil || c.With.ID != 1 && c.With.ID != 2 {
		t.Fatalf("应与白班或晚班重叠：%+v", c)
	}
	// 周一 18:00–19:00 与白班重叠（晚班从 19:00 开始，不重叠）
	if c.With.ID != 1 || c.Run.Day != int(time.Monday) || c.Run.StartClock != "18:00" || c.Run.EndClock != "19:00" {
		t.Errorf("冲突位置不对：%+v", c)
	}
	// 首尾相接不算重叠
	touch := Shift{Name: "x", Username: "x", StartMin: 19 * 60, EndMin: 19*60 + 30, Weekdays: 0, Enabled: true}
	touch.Weekdays = 1 << uint(time.Monday)
	if c := FindConflict(touch, []Shift{base[0]}); c != nil {
		t.Errorf("首尾相接不应算重叠：%+v", c)
	}
	// 跨午夜：每天 22:00–06:00 与每天 04:00–10:00 在 04:00–06:00 重叠；第一个冲突段在周日（周六的夜班延续到周日）
	night := Shift{ID: 10, Name: "夜", Username: "n", StartMin: 22 * 60, EndMin: 6 * 60, Weekdays: AllWeek, Enabled: true}
	early := Shift{Name: "早", Username: "e", StartMin: 4 * 60, EndMin: 10 * 60, Weekdays: AllWeek, Enabled: true}
	c = FindConflict(early, []Shift{night})
	if c == nil || c.Run.Day != 0 || c.Run.StartClock != "04:00" || c.Run.EndClock != "06:00" {
		t.Errorf("跨午夜的冲突：%+v", c)
	}
	// 自己（同 ID）不算；停用的班次不参与
	if c := FindConflict(night, []Shift{night}); c != nil {
		t.Errorf("修改自己不应与自己冲突：%+v", c)
	}
	off := base[0]
	off.Enabled = false
	if c := FindConflict(mon, []Shift{off}); c != nil {
		t.Errorf("停用的班次不参与重叠校验：%+v", c)
	}
	mon.Enabled = false
	if c := FindConflict(mon, base); c != nil {
		t.Errorf("要保存成停用的班次不做重叠校验：%+v", c)
	}
}

func TestGaps(t *testing.T) {
	if g := Gaps(nil); g != nil {
		t.Errorf("没有班次不算空档：%+v", g)
	}
	if g := Gaps(threeShifts()); len(g) != 0 {
		t.Errorf("三班倒 24 小时全覆盖：%+v", g)
	}
	// 只有白班和晚班：每天 00:00–08:00 没人
	g := Gaps(threeShifts()[:2])
	if len(g) != 1 || g[0].Weekdays != AllWeek || g[0].Start != "00:00" || g[0].End != "08:00" {
		t.Errorf("缺夜班：%+v", g)
	}
	// 只有工作日白班：工作日 00:00–08:00 和 19:00–24:00 没人，周末（周六 + 周日，位图 0x41）整天没人
	weekday := []Shift{{ID: 1, Name: "白", Username: "a", StartMin: 8 * 60, EndMin: 19 * 60, Weekdays: 0x3e, Enabled: true}}
	g = Gaps(weekday)
	if len(g) != 3 {
		t.Fatalf("应有 3 段空档：%+v", g)
	}
	if g[0].Start != "00:00" || g[0].End != "08:00" || g[0].Weekdays != 0x3e ||
		g[1].Start != "00:00" || g[1].End != "24:00" || g[1].Weekdays != 0x41 ||
		g[2].Start != "19:00" || g[2].End != "24:00" || g[2].Weekdays != 0x3e {
		t.Errorf("空档内容不对：%+v", g)
	}
	// 停用的不算覆盖；全部停用 = 还没排班
	off := threeShifts()
	for i := range off {
		off[i].Enabled = false
	}
	if g := Gaps(off); g != nil {
		t.Errorf("全部停用等于没排班：%+v", g)
	}
	// 跨午夜的夜班（每天 22:00–06:00）+ 白班 06:00–22:00：全覆盖
	full := []Shift{
		{ID: 1, StartMin: 6 * 60, EndMin: 22 * 60, Weekdays: AllWeek, Enabled: true},
		{ID: 2, StartMin: 22 * 60, EndMin: 6 * 60, Weekdays: AllWeek, Enabled: true},
	}
	if g := Gaps(full); len(g) != 0 {
		t.Errorf("白班 + 跨午夜夜班应全覆盖：%+v", g)
	}
}

func TestTzLabelAndClockOrDate(t *testing.T) {
	if got := tzLabel(at(0, 1, 0)); got != "UTC+08:00" {
		t.Errorf("tzLabel = %q", got)
	}
	if got := tzLabel(time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("x", -(5*3600+30*60)))); got != "UTC-05:30" {
		t.Errorf("tzLabel = %q", got)
	}
	if got := clockOrDate(at(0, 19, 0), at(0, 10, 0)); got != "19:00" {
		t.Errorf("同一天只写钟点：%q", got)
	}
	if got := clockOrDate(at(1, 0, 0), at(0, 10, 0)); got != "10-23 00:00" {
		t.Errorf("不同天带月日：%q", got)
	}
}
