// @author: kevinkelly
// @license: Apache Licence
// @file: timeutil.go
// @time: 2026/10/28

package partman

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // 内置 IANA 时区库：不依赖服务器上有没有 tzdata（America/New_York 等）
)

// 时区与窗口网格。移植自外部程序的 timeutil.go（alignDown / shiftWindows / partitionName），行为以原程序为准：
// grid_golden_test.go 里的期望值是用原程序对同一输入算出来的。

// maxTimezoneLen 是库列 dbt_partman_config.timezone（varchar(20)）放得下的最大长度。
const maxTimezoneLen = 20

// TZ 是解析后的时区。
type TZ struct {
	Loc      *time.Location
	Name     string   // 展示名：UTC / UTC+08:00 / America/New_York
	FollowDB bool     // 配置为空：跟随目标库会话时区（Loc 为 nil，要用 DBZone 补）
	Warnings []string // 夏令时等提示
}

var ianaNameRe = regexp.MustCompile(`^[A-Za-z0-9_+\-/]+$`)

// ParseTimezone 解析配置里的 timezone。接受：
//   - 空：跟随目标库会话时区（FollowDB=true）
//   - 固定偏移：+08:00 / -05:00 / +0800 / +8 / UTC+8 / GMT+08:00 / UTC-3 / UTC / GMT
//   - IANA 名字：Asia/Shanghai、America/New_York（内置 tzdata，区分大小写；不接受 Local）
//
// 与原程序的差别：偏移要求格式严格（原程序对 "+08:xx" 这类静默当 0 分钟），偏移限制在 ±14 小时内，并限制 20 个字符（库列长度）。
func ParseTimezone(spec string) (*TZ, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return &TZ{FollowDB: true, Name: "跟随目标库会话时区"}, nil
	}
	if n := len([]rune(s)); n > maxTimezoneLen {
		return nil, badReq("时区 %s 超过 %d 个字符，库里的 timezone 列放不下，请改用固定偏移（如 -03:00）", qstr(s), maxTimezoneLen)
	}
	if loc, name, matched, err := parseFixedOffset(s); matched {
		if err != nil {
			return nil, err
		}
		return &TZ{Loc: loc, Name: name}, nil
	}
	if s == "Asia/Shanghai" { // 沿用原有行为：固定 +08:00，没有历史夏令时
		return &TZ{Loc: time.FixedZone("CST", 8*3600), Name: s}, nil
	}
	if strings.EqualFold(s, "Local") || !ianaNameRe.MatchString(s) {
		return nil, badReq("无法识别的时区 %s（可填 IANA 名字如 Asia/Shanghai、固定偏移如 +08:00 / UTC+8，留空跟随目标库）", qstr(s))
	}
	loc, err := time.LoadLocation(s)
	if err != nil {
		return nil, badReq("无法识别的时区 %s（可填 IANA 名字如 Asia/Shanghai、固定偏移如 +08:00 / UTC+8，留空跟随目标库）", qstr(s))
	}
	tz := &TZ{Loc: loc, Name: s}
	if observesDST(loc, time.Now().Year()) {
		tz.Warnings = append(tz.Warnings, fmt.Sprintf("时区 %s 有夏令时：换季日前后按日的分区边界在 UTC 上会漂移 1 小时，按小时 / 分钟的分区还可能在换季时重复或缺失一个窗口。建议改用固定偏移（如 %s）", s, standardOffset(loc, time.Now().Year())))
	}
	return tz, nil
}

// observesDST 判断该年内偏移是否会变化（每月 1 日与 15 日采样）。
func observesDST(loc *time.Location, year int) bool {
	_, base := time.Date(year, 1, 1, 12, 0, 0, 0, loc).Zone()
	for m := 1; m <= 12; m++ {
		for _, d := range []int{1, 15} {
			if _, off := time.Date(year, time.Month(m), d, 12, 0, 0, 0, loc).Zone(); off != base {
				return true
			}
		}
	}
	return false
}

// standardOffset 返回该年内最小的偏移（标准时间），格式 +08:00，用于 warning 里的建议。
func standardOffset(loc *time.Location, year int) string {
	min := 1 << 30
	for m := 1; m <= 12; m++ {
		if _, off := time.Date(year, time.Month(m), 1, 12, 0, 0, 0, loc).Zone(); off < min {
			min = off
		}
	}
	return offsetClock(min)
}

func offsetClock(off int) string {
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	return fmt.Sprintf("%s%02d:%02d", sign, off/3600, (off%3600)/60)
}

// offsetName 把偏移秒数格式化成 UTC+08:00 这样的名字（0 为 UTC）。
func offsetName(off int) string {
	if off == 0 {
		return "UTC"
	}
	return "UTC" + offsetClock(off)
}

// parseFixedOffset 解析固定偏移写法。matched=false 表示不是偏移写法（交给 IANA）；matched=true 且 err 非空表示像偏移但写错了。
func parseFixedOffset(s string) (loc *time.Location, name string, matched bool, err error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	hadPrefix := false
	switch {
	case strings.HasPrefix(t, "UTC"):
		t, hadPrefix = strings.TrimSpace(strings.TrimPrefix(t, "UTC")), true
	case strings.HasPrefix(t, "GMT"):
		t, hadPrefix = strings.TrimSpace(strings.TrimPrefix(t, "GMT")), true
	}
	if t == "" {
		if hadPrefix {
			return time.UTC, "UTC", true, nil
		}
		return nil, "", false, nil
	}
	if t[0] != '+' && t[0] != '-' {
		return nil, "", false, nil // 如 GMT0、Etc/GMT+8：交给 IANA
	}
	bad := func() (*time.Location, string, bool, error) {
		return nil, "", true, badReq("无法识别的时区偏移 %s（正确写法如 +08:00、+0800、+8、UTC+8、GMT-05:00，范围 -14:00 ~ +14:00）", qstr(s))
	}
	sign := 1
	if t[0] == '-' {
		sign = -1
	}
	body := t[1:]
	var hh, mm int
	switch {
	case regexp.MustCompile(`^\d{1,2}$`).MatchString(body):
		hh, _ = strconv.Atoi(body)
	case regexp.MustCompile(`^\d{4}$`).MatchString(body):
		hh, _ = strconv.Atoi(body[:2])
		mm, _ = strconv.Atoi(body[2:])
	case regexp.MustCompile(`^\d{1,2}:\d{2}$`).MatchString(body):
		p := strings.SplitN(body, ":", 2)
		hh, _ = strconv.Atoi(p[0])
		mm, _ = strconv.Atoi(p[1])
	default:
		return bad()
	}
	if mm >= 60 || hh*3600+mm*60 > 14*3600 {
		return bad()
	}
	off := sign * (hh*3600 + mm*60)
	if off == 0 {
		return time.UTC, "UTC", true, nil
	}
	n := offsetName(off)
	return time.FixedZone(n, off), n, true, nil
}

// resolveTimezone 取得计算用的时区：配置为空时用目标库会话偏移（TableState.SessionOffset）。
func resolveTimezone(spec string, st *TableState) (*TZ, error) {
	tz, err := ParseTimezone(spec)
	if err != nil {
		return nil, err
	}
	if !tz.FollowDB {
		return tz, nil
	}
	if !st.HasSessionOffset {
		return nil, conflict("timezone_unknown", "配置的时区为空，需要读取目标库的会话时区，但没有读到；请在配置里明确填写时区")
	}
	n := offsetName(st.SessionOffset)
	tz.Loc = time.FixedZone(n, st.SessionOffset)
	tz.Name = fmt.Sprintf("%s（跟随目标库）", n)
	return tz, nil
}

// ---- 窗口网格（原程序 timeutil.go） ----

// 1970-01-01 是 day 网格锚点；1970-01-05 是星期一，作为 week 网格锚点。
func epochDate(loc *time.Location) time.Time   { return time.Date(1970, 1, 1, 0, 0, 0, 0, loc) }
func epochMonday(loc *time.Location) time.Time { return time.Date(1970, 1, 5, 0, 0, 0, 0, loc) }

func floorDiv(a, b int) int {
	if a >= 0 {
		return a / b
	}
	return -((-a + b - 1) / b)
}

// daysBetween 返回 b 相对 a 的整天数（按日历日）。与原程序一致：用小时数 / 24 取整。
func daysBetween(a, b time.Time) int {
	a = time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, a.Location())
	b = time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, b.Location())
	return int(b.Sub(a).Hours() / 24)
}

// alignDown 把 t 向下对齐到所在窗口（interval 个 period）的起点。
// minute / hour 按日内取整；day 按 1970-01-01 起算；week 按 1970-01-05（周一）起算；month 按 1970-01 起算；year 按公元年的倍数。
func alignDown(t time.Time, period string, interval int, loc *time.Location) time.Time {
	if interval < 1 {
		interval = 1
	}
	t = t.In(loc)
	y, mo, d := t.Date()
	switch period {
	case "minute":
		dayStart := time.Date(y, mo, d, 0, 0, 0, 0, loc)
		mins := t.Hour()*60 + t.Minute()
		return dayStart.Add(time.Duration((mins/interval)*interval) * time.Minute)
	case "hour":
		dayStart := time.Date(y, mo, d, 0, 0, 0, 0, loc)
		return dayStart.Add(time.Duration((t.Hour()/interval)*interval) * time.Hour)
	case "day":
		d0 := time.Date(y, mo, d, 0, 0, 0, 0, loc)
		idx := daysBetween(epochDate(loc), d0)
		return epochDate(loc).AddDate(0, 0, floorDiv(idx, interval)*interval)
	case "week":
		weekday := int(t.Weekday()) // Sunday=0
		offset := (weekday + 6) % 7 // 距离本周一的天数
		monday := time.Date(y, mo, d, 0, 0, 0, 0, loc).AddDate(0, 0, -offset)
		idx := daysBetween(epochMonday(loc), monday) / 7
		return epochMonday(loc).AddDate(0, 0, floorDiv(idx, interval)*interval*7)
	case "month":
		mi := (y-1970)*12 + (int(mo) - 1)
		floored := floorDiv(mi, interval) * interval
		yy := 1970 + floorDiv(floored, 12)
		mm := time.Month(((floored%12)+12)%12 + 1)
		return time.Date(yy, mm, 1, 0, 0, 0, 0, loc)
	case "year":
		return time.Date(floorDiv(y, interval)*interval, 1, 1, 0, 0, 0, 0, loc)
	}
	return t
}
