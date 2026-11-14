// @author: kevinkelly
// @license: Apache Licence
// @file: schedule.go
// @time: 2026/10/28

package archive

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/i18n"
)

// 调度规则全部是纯函数（不读时钟、不连库），时区取传入时间自带的 Location（服务进程本地时区）。

const (
	schedDay   = "day"
	schedWeek  = "week"
	schedMonth = "month"
	schedCron  = "cron"

	minCronGapMinutes = 60
	minWindowMinutes  = 30
)

// weekdayKeys 是星期名的翻译键（0 = 周日），与前端 sched.js 的 WD 同键；中文界面展示 “日 一 二 …”。
var weekdayKeys = []string{"日|wd", "一|wd", "二|wd", "三|wd", "四|wd", "五|wd", "六|wd"}

// parseHM 解析 "HH:MM"，返回当天的第几分钟。
func parseHM(s string) (int, bool) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, ':')
	if i < 1 || i > 2 || len(s)-i-1 != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(s[:i])
	m, err2 := strconv.Atoi(s[i+1:])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// windowMinutes 返回窗口长度（分钟）。Start == End 表示不限制（24 小时）。格式错误返回 ok=false。
func windowMinutes(w engine.Window) (int, bool) {
	a, ok1 := parseHM(w.Start)
	b, ok2 := parseHM(w.End)
	if !ok1 || !ok2 {
		return 0, false
	}
	if a == b {
		return 1440, true
	}
	if b > a {
		return b - a, true
	}
	return b + 1440 - a, true
}

// windowUnlimited 窗口是否不限制。
func windowUnlimited(w engine.Window) bool {
	a, ok1 := parseHM(w.Start)
	b, ok2 := parseHM(w.End)
	return !ok1 || !ok2 || a == b
}

// inWindow 判断 t 的本地时刻是否在窗口内（可跨零点；半开区间 [Start, End)）。格式错误或不限制返回 true。
func inWindow(t time.Time, w engine.Window) bool {
	a, ok1 := parseHM(w.Start)
	b, ok2 := parseHM(w.End)
	if !ok1 || !ok2 || a == b {
		return true
	}
	m := t.Hour()*60 + t.Minute()
	if a < b {
		return m >= a && m < b
	}
	return m >= a || m < b
}

// windowSet 返回窗口覆盖的“一天中的分钟”集合，用来判断窗口是否被扩大。
func windowSet(w engine.Window) map[int]bool {
	s := map[int]bool{}
	a, ok1 := parseHM(w.Start)
	n, ok2 := windowMinutes(w)
	if !ok1 || !ok2 {
		return s
	}
	for k, m := 0, a; k < n; k, m = k+1, (m+1)%1440 {
		s[m] = true
	}
	return s
}

func windowText(w engine.Window) string { return windowTextL(i18n.ZH, w) }

// windowTextL 按界面语言生成执行窗口说明（与前端 sched.js 的 winText 同键）；中文与改造前逐字一致。
func windowTextL(lang string, w engine.Window) string {
	n, ok := windowMinutes(w)
	if !ok {
		return w.Start + " – " + w.End
	}
	if windowUnlimited(w) {
		return i18n.T(lang, "不限制")
	}
	a, _ := parseHM(w.Start)
	b, _ := parseHM(w.End)
	dur := i18n.T(lang, "{n} 分钟", "n", n)
	if n >= 60 {
		if n%60 == 0 {
			dur = i18n.T(lang, "{n} 小时", "n", n/60)
		} else {
			dur = i18n.T(lang, "{n} 小时", "n", fmt.Sprintf("%.1f", float64(n)/60))
		}
	}
	cross := ""
	if b <= a {
		cross = i18n.T(lang, "（跨零点）")
	}
	return i18n.T(lang, "{start} – {end}{cross}（{dur}）", "start", w.Start, "end", w.End, "cross", cross, "dur", dur)
}

func scheduleText(s Schedule) string { return scheduleTextL(i18n.ZH, s) }

// scheduleTextL 按界面语言生成调度说明（与前端 sched.js 的 schedText 同键）；中文与改造前逐字一致。
func scheduleTextL(lang string, s Schedule) string {
	switch s.Kind {
	case schedDay:
		return i18n.T(lang, "每天 {time}", "time", s.Time)
	case schedWeek:
		days := append([]int(nil), s.Weekdays...)
		sort.Ints(days)
		var names []string
		for _, d := range days {
			if d >= 0 && d <= 6 {
				names = append(names, i18n.T(lang, weekdayKeys[d]))
			}
		}
		sep := "、"
		if lang != i18n.ZH && lang != "" {
			sep = ", "
		}
		return i18n.T(lang, "每周{days} {time}", "days", strings.Join(names, sep), "time", s.Time)
	case schedMonth:
		return i18n.T(lang, "每月 {day} 日 {time}", "day", s.MonthDay, "time", s.Time)
	case schedCron:
		return "cron " + s.Cron
	}
	return ""
}

// ---- cron ----

type cronSpec struct {
	min, hour, dom, mon, dow []int // 已排序的取值
	has                      [5]map[int]bool
	domStar, dowStar         bool
}

var cronRanges = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}

// parseCron 解析 5 段 cron：分 时 日 月 周；每段支持 *、数字、a-b、逗号列表、*/n、a-b/n、a/n（周 7 等同 0）。
func parseCron(expr string) (*cronSpec, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron 需要 5 段：分 时 日 月 周")
	}
	c := &cronSpec{}
	for i := 0; i < 5; i++ {
		lo, hi := cronRanges[i][0], cronRanges[i][1]
		set := map[int]bool{}
		for _, part := range strings.Split(f[i], ",") {
			base, stepS, hasStep := strings.Cut(part, "/")
			step := 1
			if hasStep {
				n, err := strconv.Atoi(stepS)
				if err != nil || n <= 0 {
					return nil, fmt.Errorf("第 %d 段「%s」的步长不合法", i+1, f[i])
				}
				step = n
			}
			var a, b int
			switch {
			case base == "*":
				a, b = lo, hi
			case strings.Contains(base, "-"):
				x, y, _ := strings.Cut(base, "-")
				var e1, e2 error
				a, e1 = strconv.Atoi(x)
				b, e2 = strconv.Atoi(y)
				if e1 != nil || e2 != nil {
					return nil, fmt.Errorf("第 %d 段「%s」不是有效的 cron 表达式", i+1, f[i])
				}
			default:
				n, err := strconv.Atoi(base)
				if err != nil {
					return nil, fmt.Errorf("第 %d 段「%s」不是有效的 cron 表达式", i+1, f[i])
				}
				a, b = n, n
				if hasStep {
					b = hi
				}
			}
			if a < lo || b > hi || a > b {
				return nil, fmt.Errorf("第 %d 段「%s」超出范围 %d–%d", i+1, f[i], lo, hi)
			}
			for v := a; v <= b; v += step {
				if i == 4 && v == 7 {
					set[0] = true
				} else {
					set[v] = true
				}
			}
		}
		c.has[i] = set
		vals := make([]int, 0, len(set))
		for v := range set {
			vals = append(vals, v)
		}
		sort.Ints(vals)
		switch i {
		case 0:
			c.min = vals
		case 1:
			c.hour = vals
		case 2:
			c.dom = vals
			c.domStar = strings.HasPrefix(f[i], "*")
		case 3:
			c.mon = vals
		case 4:
			c.dow = vals
			c.dowStar = strings.HasPrefix(f[i], "*")
		}
	}
	return c, nil
}

func (c *cronSpec) dayOK(d time.Time) bool {
	if !c.has[3][int(d.Month())] {
		return false
	}
	domOK, dowOK := c.has[2][d.Day()], c.has[4][int(d.Weekday())]
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dowOK
	case c.dowStar:
		return domOK
	}
	return domOK || dowOK // 标准 cron：日与周都限定时取并集
}

// scheduleTimes 返回严格晚于 from 的后 n 次触发时间（本地时区，秒归零）。
func scheduleTimes(s Schedule, from time.Time, n int) []time.Time {
	var out []time.Time
	loc := from.Location()
	day0 := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, loc)
	switch s.Kind {
	case schedCron:
		c, err := parseCron(s.Cron)
		if err != nil {
			return nil
		}
		for k := 0; k < 800 && len(out) < n; k++ {
			d := day0.AddDate(0, 0, k)
			if !c.dayOK(d) {
				continue
			}
			for _, h := range c.hour {
				for _, m := range c.min {
					t := time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, loc)
					if t.After(from) {
						out = append(out, t)
						if len(out) == n {
							return out
						}
					}
				}
			}
		}
	case schedDay, schedWeek, schedMonth:
		mins, ok := parseHM(s.Time)
		if !ok {
			return nil
		}
		wd := map[int]bool{}
		for _, d := range s.Weekdays {
			wd[d] = true
		}
		for k := 0; k < 800 && len(out) < n; k++ {
			d := day0.AddDate(0, 0, k)
			ok := s.Kind == schedDay || (s.Kind == schedWeek && wd[int(d.Weekday())]) || (s.Kind == schedMonth && d.Day() == s.MonthDay)
			if !ok {
				continue
			}
			t := time.Date(d.Year(), d.Month(), d.Day(), mins/60, mins%60, 0, 0, loc)
			if t.After(from) {
				out = append(out, t)
			}
		}
	}
	return out
}

// nextRun 返回晚于 from 的下一次触发时间；没有则零值。
func nextRun(s Schedule, from time.Time) time.Time {
	if ts := scheduleTimes(s, from, 1); len(ts) > 0 {
		return ts[0]
	}
	return time.Time{}
}

// validateSchedule 校验调度，并检查它与执行窗口的关系。field 前缀用 "schedule."。
func validateSchedule(s Schedule, w engine.Window, now time.Time) []engine.Issue {
	var out []engine.Issue
	bad := func(field, format string, a ...any) {
		out = append(out, engine.Issue{Field: field, Level: "error", Msg: fmt.Sprintf(format, a...)})
	}
	switch s.Kind {
	case schedDay, schedWeek, schedMonth:
		if _, ok := parseHM(s.Time); !ok {
			bad("schedule.time", "开始时间格式应为 HH:MM")
		}
		if s.Kind == schedWeek {
			if len(s.Weekdays) == 0 {
				bad("schedule.weekdays", "请至少选择一个星期")
			}
			for _, d := range s.Weekdays {
				if d < 0 || d > 6 {
					bad("schedule.weekdays", "星期取值 0（周日）到 6（周六）")
					break
				}
			}
		}
		if s.Kind == schedMonth && (s.MonthDay < 1 || s.MonthDay > 28) {
			bad("schedule.month_day", "每月的日期取 1–28（避开月末差异）")
		}
	case schedCron:
		c, err := parseCron(s.Cron)
		if err != nil {
			bad("schedule.cron", "%s", err.Error())
			return out
		}
		_ = c
		ts := scheduleTimes(s, now, 24)
		if len(ts) < 2 {
			bad("schedule.cron", "该 cron 在未来一段时间内触发不足 2 次")
		} else {
			gap := ts[1].Sub(ts[0])
			for i := 2; i < len(ts); i++ {
				if g := ts[i].Sub(ts[i-1]); g < gap {
					gap = g
				}
			}
			if gap < minCronGapMinutes*time.Minute {
				bad("schedule.cron", "cron 最短间隔 %d 分钟（当前最短 %d 分钟）", minCronGapMinutes, int(gap/time.Minute))
			}
		}
	default:
		bad("schedule.kind", "调度类型只能是 day / week / month / cron")
		return out
	}
	if len(out) > 0 {
		return out
	}
	// 与窗口的关系（窗口本身的格式 / 时长由引擎 Validate 报告）
	if _, ok := windowMinutes(w); ok && !windowUnlimited(w) {
		if s.Kind != schedCron {
			if m, _ := parseHM(s.Time); !inWindow(time.Date(2000, 1, 1, m/60, m%60, 0, 0, time.UTC), w) {
				bad("schedule.time", "开始时间 %s 不在执行窗口 %s–%s 内，任务永远不会被调度", s.Time, w.Start, w.End)
			}
		} else if ts := scheduleTimes(s, now, 5); len(ts) > 0 {
			all := true
			for _, t := range ts {
				if inWindow(t, w) {
					all = false
					break
				}
			}
			if all {
				bad("schedule.cron", "cron 未来 5 次触发都落在执行窗口之外，任务会一直被跳过")
			}
		}
	}
	return out
}

// dueDecision 是调度器对一个已启用任务在 now 时刻的决定。
type dueDecision int

const (
	dueNo      dueDecision = iota // 还没到点
	dueRun                        // 到点且在窗口内：启动
	dueMissed                     // 到点但已错过窗口：不启动，推进到下一次
	dueNoSched                    // 调度无法计算（配置损坏）
)

// decideDue 根据任务保存的下次运行时间决定要不要启动。nextAt 为零值表示尚未计算（视为到点后重算）。
func decideDue(nextAt, now time.Time, s Schedule, w engine.Window) (dueDecision, time.Time) {
	if nextAt.IsZero() {
		n := nextRun(s, now)
		if n.IsZero() {
			return dueNoSched, time.Time{}
		}
		return dueNo, n
	}
	if nextAt.After(now) {
		return dueNo, nextAt
	}
	n := nextRun(s, now)
	if inWindow(now, w) {
		return dueRun, n
	}
	return dueMissed, n
}
