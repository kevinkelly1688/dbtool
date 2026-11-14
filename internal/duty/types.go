// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

// Package duty 是 DBA 值班表：班次（每天 / 每周循环的值班时段）加临时调班，算出“此刻谁在值班”，
// 显示在每个页面顶栏时钟的左边，并提供「平台设置 › 值班表」管理页。
//
// 规则（页面和文档里的说法与这里一致）：
//   - 时间一律按服务器本地时区；起止用“一天里的第几分钟”（0–1439）存，结束 ≤ 开始表示跨到次日
//     （结束 0 = 次日 00:00，也就是当天 24:00；开始 = 结束表示 24 小时）；
//   - 班次可以只在每周的某几天生效；跨午夜的班属于“开始的那一天”（周五 22:00 开始、周六 06:00 结束的班算周五的班）；
//   - 临时调班的优先级高于班次：这段时间由调班指定的人值班，不改班次模板；
//   - 值班人的账号被停用 / 删除后，他的班次与调班不再算数（那段时间显示“暂无”），管理页会标红提醒；
//   - 同一星期里启用的班次时间不能重叠；有空档只提示，不拦（也可能本来就不是 24 小时值班）。
package duty

import (
	"time"
)

const (
	dayMinutes = 1440
	weekMinute = 7 * dayMinutes

	// AllWeek 是“每天”的星期位图（bit i 对应 time.Weekday(i)，周日为 0）。
	AllWeek uint8 = 0x7f

	MaxShifts        = 50  // 班次数量上限
	MaxOverrideDays  = 31  // 一条临时调班最长 31 天
	MaxOpenOverrides = 200 // 没撤销、还没结束的调班最多这么多条（每次取数都要全部载入）
	MaxNameRunes     = 40
	MaxContactRunes  = 100
	MaxRemarkRunes   = 200

	KindShift    = "shift"
	KindOverride = "override"
)

// Shift 是一个班次：每天（或每周几天）循环的值班时段。
type Shift struct {
	ID       uint
	Name     string
	Username string
	StartMin int   // 开始：一天里的第几分钟，0–1439
	EndMin   int   // 结束：同上；≤ StartMin 表示跨到次日
	Weekdays uint8 // 适用星期位图：bit i = time.Weekday(i)
	Contact  string
	Remark   string
	Enabled  bool

	CreatedBy, UpdatedBy string
	CreatedAt, UpdatedAt time.Time
}

// Cross 报告班次是否跨到次日（含“结束 = 次日 00:00”和“24 小时”）。
func (s Shift) Cross() bool { return s.EndMin <= s.StartMin }

// Minutes 是班次的时长（分钟）。
func (s Shift) Minutes() int {
	if s.Cross() {
		return dayMinutes - s.StartMin + s.EndMin
	}
	return s.EndMin - s.StartMin
}

// On 报告班次是否在 wd 这天开始。
func (s Shift) On(wd time.Weekday) bool { return s.Weekdays&(1<<uint(wd)) != 0 }

// Override 是一条临时调班：[Start, End) 这段时间由 Username 值班。
type Override struct {
	ID         uint
	Username   string
	Start, End time.Time
	Reason     string
	Contact    string
	Canceled   bool
	CanceledBy string
	CanceledAt *time.Time
	CreatedBy  string
	CreatedAt  time.Time

	// Void 不存库：调班人的账号已停用 / 删除时由 service 置位。这条调班仍然占着这段时间（不会让时间回落给被顶班的人），
	// 但没人值班——和班次里的值班人账号停用同样按“暂无”处理。
	Void bool
}

// Person 是用户目录里的一个账号。
type Person struct {
	Username string
	Display  string
	Email    string
	Active   bool
}

// Name 是显示用的名字：优先显示名，没有就用登录名。
func (p Person) Name() string {
	if p.Display != "" {
		return p.Display
	}
	return p.Username
}

// Slot 是班次 / 调班落在具体日期上的一段时间。
type Slot struct {
	Kind       string // KindShift / KindOverride
	ShiftID    uint
	OverrideID uint
	Name       string // 班次名；调班为空
	Username   string
	Contact    string
	Reason     string // 调班原因
	Start, End time.Time
	Void       bool // 调班人的账号已停用：占着这段时间但没人值班（见 Override.Void）
}

// Segment 是时间线上的一段：Slot 为 nil 表示这段时间没有人值班。
type Segment struct {
	Start, End time.Time
	Slot       *Slot
	FromPrev   bool // 这一段所属的班次 / 调班在窗口开始之前就开始了
	ToNext     bool // 所属的班次 / 调班在窗口结束之后才结束
}

// User 是这一段的值班人登录名（没人时为空）。
func (g Segment) User() string {
	if g.Slot == nil {
		return ""
	}
	return g.Slot.Username
}
