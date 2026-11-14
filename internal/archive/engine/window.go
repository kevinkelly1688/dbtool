// @author: kevinkelly
// @license: Apache Licence
// @file: window.go
// @time: 2026/10/28

package engine

import (
	"strconv"
	"strings"
	"time"
)

// ParseHM 解析 "HH:MM"，返回当天第几分钟。
func ParseHM(s string) (int, bool) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, ':')
	if i != 2 || len(s) != 5 {
		return 0, false
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// WindowUnlimited 判断窗口是否不限制（两端为空或相等）。
func (w Window) Unlimited() bool {
	return (w.Start == "" && w.End == "") || w.Start == w.End
}

// WindowMinutes 返回窗口时长（分钟，可跨零点）；不限制返回 1440。格式非法返回 -1。
func WindowMinutes(w Window) int {
	if w.Unlimited() {
		return 1440
	}
	a, ok1 := ParseHM(w.Start)
	b, ok2 := ParseHM(w.End)
	if !ok1 || !ok2 {
		return -1
	}
	return (b - a + 1440) % 1440
}

// InWindow 判断时刻 t（取其本地时钟的时分）是否在窗口内：[Start, End)，可跨零点。窗口不限制或格式非法时返回 true / false：
// 不限制 → true；格式非法 → false（保守：不运行）。
func InWindow(t time.Time, w Window) bool {
	if w.Unlimited() {
		return true
	}
	a, ok1 := ParseHM(w.Start)
	b, ok2 := ParseHM(w.End)
	if !ok1 || !ok2 {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if a < b {
		return m >= a && m < b
	}
	return m >= a || m < b
}
