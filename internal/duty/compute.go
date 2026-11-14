// @author: kevinkelly
// @license: Apache Licence
// @file: compute.go
// @time: 2026/10/28

package duty

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- 钟点

// ParseClock 解析 "HH:MM"。allow24 为真时 "24:00" 合法（只用在结束时间，等于次日 00:00，返回 0）。
func ParseClock(s string, allow24 bool) (int, error) {
	s = strings.TrimSpace(s)
	h, m, ok := strings.Cut(s, ":")
	if !ok || len(h) == 0 || len(h) > 2 || len(m) != 2 {
		return 0, fmt.Errorf("bad clock %q", s)
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("bad clock %q", s)
	}
	if hh == 24 && mm == 0 && allow24 {
		return 0, nil
	}
	if hh > 23 {
		return 0, fmt.Errorf("bad clock %q", s)
	}
	return hh*60 + mm, nil
}

// FormatClock 把“第几分钟”写成 "HH:MM"；1440 写成 "24:00"。
func FormatClock(min int) string {
	return fmt.Sprintf("%02d:%02d", min/60, min%60)
}

// endClock 是班次结束时间的显示：结束在次日 00:00（EndMin == 0）显示成 24:00，其余照写；
// 开始 = 结束 = 0 的“24 小时”班因此显示成 00:00–24:00。
func endClock(endMin int) string {
	if endMin == 0 {
		return "24:00"
	}
	return FormatClock(endMin)
}

// RangeText 是班次时间段的显示，如 08:00–19:00、19:00–24:00、22:00–06:00。
func RangeText(startMin, endMin int) string {
	return FormatClock(startMin) + "–" + endClock(endMin)
}

// ---------------------------------------------------------------- 班次落到具体日期

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// occurrence 返回班次在 day 这一天开始的那一段（不检查适用星期）。
func (s Shift) occurrence(day time.Time) Slot {
	y, m, d := day.Date()
	loc := day.Location()
	start := time.Date(y, m, d, s.StartMin/60, s.StartMin%60, 0, 0, loc)
	endDay := d
	if s.Cross() {
		endDay = d + 1
	}
	end := time.Date(y, m, endDay, s.EndMin/60, s.EndMin%60, 0, 0, loc)
	return Slot{Kind: KindShift, ShiftID: s.ID, Name: s.Name, Username: s.Username, Contact: s.Contact, Start: start, End: end}
}

// slotsIn 列出与 [from, to) 有交集的全部班次 / 调班（已撤销的调班、停用的班次不算）。
func slotsIn(shifts []Shift, overrides []Override, from, to time.Time) []Slot {
	var slots []Slot
	for day := startOfDay(from).AddDate(0, 0, -1); day.Before(to); day = day.AddDate(0, 0, 1) {
		wd := day.Weekday()
		for _, sh := range shifts {
			if !sh.Enabled || !sh.On(wd) {
				continue
			}
			if sl := sh.occurrence(day); sl.End.After(from) && sl.Start.Before(to) {
				slots = append(slots, sl)
			}
		}
	}
	for _, o := range overrides {
		if o.Canceled || !o.End.After(from) || !o.Start.Before(to) {
			continue
		}
		slots = append(slots, Slot{Kind: KindOverride, OverrideID: o.ID, Username: o.Username, Contact: o.Contact, Reason: o.Reason, Start: o.Start, End: o.End, Void: o.Void})
	}
	return slots
}

// better 报告 a 是否比 b 更该值班：调班优先于班次；调班之间后建的（ID 大的）优先；
// 班次之间开始得晚的优先（刚交班的那个），再按 ID 小的。
func better(a, b Slot) bool {
	if (a.Kind == KindOverride) != (b.Kind == KindOverride) {
		return a.Kind == KindOverride
	}
	if a.Kind == KindOverride {
		return a.OverrideID > b.OverrideID
	}
	if !a.Start.Equal(b.Start) {
		return a.Start.After(b.Start)
	}
	return a.ShiftID < b.ShiftID
}

func sameSlot(a, b *Slot) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Kind == b.Kind && a.ShiftID == b.ShiftID && a.OverrideID == b.OverrideID && a.Start.Equal(b.Start)
}

// Timeline 返回 [from, to) 里每一段时间由谁值班（相邻、同一个班次 / 调班的合并成一段；没人值班的也成一段）。
func Timeline(shifts []Shift, overrides []Override, from, to time.Time) []Segment {
	if !to.After(from) {
		return nil
	}
	slots := slotsIn(shifts, overrides, from, to)
	bounds := []time.Time{from, to}
	for _, sl := range slots {
		if sl.Start.After(from) && sl.Start.Before(to) {
			bounds = append(bounds, sl.Start)
		}
		if sl.End.After(from) && sl.End.Before(to) {
			bounds = append(bounds, sl.End)
		}
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i].Before(bounds[j]) })
	uniq := bounds[:1]
	for _, b := range bounds[1:] {
		if !b.Equal(uniq[len(uniq)-1]) {
			uniq = append(uniq, b)
		}
	}
	var segs []Segment
	for i := 0; i+1 < len(uniq); i++ {
		a, b := uniq[i], uniq[i+1]
		var best *Slot
		for k := range slots {
			sl := &slots[k]
			if sl.Start.After(a) || sl.End.Before(b) {
				continue
			}
			if best == nil || better(*sl, *best) {
				best = sl
			}
		}
		if best != nil && best.Void {
			best = nil // 调班人的账号已停用：这段时间归这条调班，但没人值班（不回落给被顶班的人）
		}
		if n := len(segs); n > 0 && sameSlot(segs[n-1].Slot, best) {
			segs[n-1].End = b
			continue
		}
		segs = append(segs, Segment{Start: a, End: b, Slot: best})
	}
	for i := range segs {
		if sl := segs[i].Slot; sl != nil {
			segs[i].FromPrev = sl.Start.Before(from)
			segs[i].ToNext = sl.End.After(to)
		}
	}
	return segs
}

// ---------------------------------------------------------------- 此刻谁值班

// lookAheadDays 是找“下一次交班”往后看的天数；一个人 / 一个班连续值班超过它时，下一次交班为空。
const lookAheadDays = 8

// Snapshot 是某一时刻的值班情况。
type Snapshot struct {
	Now    time.Time
	Day    time.Time // 今天 00:00
	Cur    Segment   // 此刻所在的一段（Slot 为 nil = 无人值班）
	Next   *Segment  // 下一次交班后的那一段（值班人变了，或变成无人值班）；没有则为 nil
	NextAt time.Time // 此刻所在这一段的结束：到点要重新计算
	Today  []Segment // 今天 00:00–24:00 的各段
}

// Compute 算出 now 这一刻的值班情况。shifts / overrides 里值班人账号已停用的，调用方要先去掉。
func Compute(shifts []Shift, overrides []Override, now time.Time) Snapshot {
	day := startOfDay(now)
	snap := Snapshot{Now: now, Day: day}
	snap.Today = Timeline(shifts, overrides, day, day.AddDate(0, 0, 1))
	long := Timeline(shifts, overrides, day, day.AddDate(0, 0, lookAheadDays))
	for i, g := range long {
		if g.Start.After(now) || !now.Before(g.End) {
			continue
		}
		snap.Cur, snap.NextAt = g, g.End
		for _, n := range long[i+1:] {
			if n.User() != g.User() {
				n := n
				snap.Next = &n
				break
			}
		}
		break
	}
	return snap
}

// ---------------------------------------------------------------- 一周的占用：重叠与空档

// weekSet 是一周 7×1440 分钟的占用位图（周日 00:00 = 0）。
type weekSet [(weekMinute + 63) / 64]uint64

func (w *weekSet) set(i int)      { i %= weekMinute; w[i/64] |= 1 << uint(i%64) }
func (w *weekSet) has(i int) bool { return w[i/64]&(1<<uint(i%64)) != 0 }

func (w *weekSet) intersects(o *weekSet) bool {
	for i := range w {
		if w[i]&o[i] != 0 {
			return true
		}
	}
	return false
}

// occupancy 是班次在一周里占用的分钟（按适用星期逐天铺开，跨午夜的接到下一天，周六接周日）。
func (s Shift) occupancy() *weekSet {
	var w weekSet
	n := s.Minutes()
	for d := 0; d < 7; d++ {
		if s.Weekdays&(1<<uint(d)) == 0 {
			continue
		}
		base := d*dayMinutes + s.StartMin
		for m := 0; m < n; m++ {
			w.set(base + m)
		}
	}
	return &w
}

// Run 是一周里的一段连续时间（不跨天；跨天的在午夜处切开）。
type Run struct {
	Day        int // time.Weekday：0 周日 … 6 周六
	StartMin   int
	EndMin     int // 1440 = 24:00
	StartClock string
	EndClock   string
}

// runsOf 把位图里“值 == want”的连续分钟切成不跨天的段。
func runsOf(w *weekSet, want bool) []Run {
	var out []Run
	for d := 0; d < 7; d++ {
		m := 0
		for m < dayMinutes {
			if w.has(d*dayMinutes+m) != want {
				m++
				continue
			}
			s := m
			for m < dayMinutes && w.has(d*dayMinutes+m) == want {
				m++
			}
			out = append(out, Run{Day: d, StartMin: s, EndMin: m})
		}
	}
	return out
}

// Conflict 描述两个班次重叠的地方。
type Conflict struct {
	With Shift
	Run  Run
}

// FindConflict 返回 sh（启用时）与 others 里第一个重叠的班次；没有重叠返回 nil。others 里停用的、sh 自己（同 ID）都跳过。
func FindConflict(sh Shift, others []Shift) *Conflict {
	if !sh.Enabled {
		return nil
	}
	mine := sh.occupancy()
	for _, o := range others {
		if !o.Enabled || (sh.ID != 0 && o.ID == sh.ID) {
			continue
		}
		theirs := o.occupancy()
		if !mine.intersects(theirs) {
			continue
		}
		var both weekSet
		for i := range both {
			both[i] = mine[i] & theirs[i]
		}
		if runs := runsOf(&both, true); len(runs) > 0 {
			r := runs[0]
			r.StartClock, r.EndClock = FormatClock(r.StartMin), endClockOfDay(r.EndMin)
			return &Conflict{With: o, Run: r}
		}
	}
	return nil
}

func endClockOfDay(endMin int) string {
	if endMin >= dayMinutes {
		return "24:00"
	}
	return FormatClock(endMin)
}

// Gap 是每周都没有人值班的一段时间；几天的空档时段相同的合并成一条（Weekdays 是这些天的位图）。
type Gap struct {
	Weekdays uint8  `json:"mask"`
	Start    string `json:"start"`
	End      string `json:"end"`
	S        int    `json:"s"`
	E        int    `json:"e"`
}

// Gaps 返回启用的班次一周里没覆盖到的时段。一个启用的班次都没有时返回 nil（还没开始排班，不算空档）。
func Gaps(shifts []Shift) []Gap {
	var all weekSet
	any := false
	for _, sh := range shifts {
		if !sh.Enabled {
			continue
		}
		any = true
		o := sh.occupancy()
		for i := range all {
			all[i] |= o[i]
		}
	}
	if !any {
		return nil
	}
	type key struct{ s, e int }
	group := map[key]uint8{}
	var order []key
	for _, r := range runsOf(&all, false) {
		k := key{r.StartMin, r.EndMin}
		if _, ok := group[k]; !ok {
			order = append(order, k)
		}
		group[k] |= 1 << uint(r.Day)
	}
	out := make([]Gap, 0, len(order))
	for _, k := range order {
		out = append(out, Gap{Weekdays: group[k], Start: FormatClock(k.s), End: endClockOfDay(k.e), S: k.s, E: k.e})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].S != out[j].S {
			return out[i].S < out[j].S
		}
		return out[i].E < out[j].E
	})
	return out
}

// weekdayNames 是星期的中文名（下标 = time.Weekday）；翻译在目录里（周日 → Sun …）。
var weekdayNames = [7]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}
