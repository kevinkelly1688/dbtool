// @author: kevinkelly
// @license: Apache Licence
// @file: view.go
// @time: 2026/10/28

package duty

import (
	"fmt"
	"time"
)

// 这个文件是返回给页面 / 顶栏脚本的 JSON 结构，以及从内部数据生成它们的函数。

// SegView 是一天里的一段：当天第几分钟 [S, E)（E 最大 1440 = 24:00）。
type SegView struct {
	S          int    `json:"s"`
	E          int    `json:"e"`
	Start      string `json:"start"`
	End        string `json:"end"`
	Gap        bool   `json:"gap,omitempty"` // 这段时间没人值班
	Kind       string `json:"kind,omitempty"`
	Name       string `json:"name,omitempty"` // 班次名
	User       string `json:"user,omitempty"`
	Display    string `json:"display,omitempty"`
	Reason     string `json:"reason,omitempty"` // 调班原因
	ShiftID    uint   `json:"shift,omitempty"`
	OverrideID uint   `json:"override,omitempty"`
	Cur        bool   `json:"cur,omitempty"`  // 此刻正在这一段
	Past       bool   `json:"past,omitempty"` // 已经结束
	Prev       bool   `json:"prev,omitempty"` // 接着昨天的班
	Cont       bool   `json:"cont,omitempty"` // 持续到明天
}

// CurView 是此刻在岗的人。
type CurView struct {
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"`
	Reason  string `json:"reason,omitempty"`
	User    string `json:"user"`
	Display string `json:"display"`
	Contact string `json:"contact,omitempty"`
	Email   string `json:"email,omitempty"`
	Start   string `json:"start"` // 本班开始：班次是钟点；跨天的调班带日期
	End     string `json:"end"`
	EndsAt  int64  `json:"ends_at"` // 本班结束的 Unix 秒（前端算剩余时间用）
}

// NextView 是下一次交班。
type NextView struct {
	Gap     bool   `json:"gap,omitempty"` // 交班后没人值班
	User    string `json:"user,omitempty"`
	Display string `json:"display,omitempty"`
	Name    string `json:"name,omitempty"`
	Kind    string `json:"kind,omitempty"`
	At      int64  `json:"at"`   // Unix 秒
	Time    string `json:"time"` // 交班时间：当天是钟点，不是当天带日期
}

// NowView 是顶栏芯片和浮层用的快照（GET /api/v1/duty/now，页面渲染时也嵌在芯片的 data-duty 里）。
type NowView struct {
	Enabled bool      `json:"enabled"` // false：没有启用的班次 / 表没建 / 取数失败，前端不显示芯片
	Now     int64     `json:"now,omitempty"`
	NowMin  int       `json:"now_min"` // 服务器时区里此刻是当天第几分钟
	TZ      string    `json:"tz,omitempty"`
	Date    string    `json:"date,omitempty"`
	State   string    `json:"state,omitempty"` // on 有人值班 / none 当前时段没人
	Cur     *CurView  `json:"cur,omitempty"`
	Next    *NextView `json:"next,omitempty"`
	NextAt  int64     `json:"next_at,omitempty"` // 前端在这个时刻重新取
	Today   []SegView `json:"today,omitempty"`
}

// ShiftView 是管理页里的一个班次。
type ShiftView struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`
	User      string `json:"user"`
	Display   string `json:"display"`
	Email     string `json:"email,omitempty"`
	Active    bool   `json:"active"`  // 值班人账号可用
	Missing   bool   `json:"missing"` // 值班人账号已不存在
	Start     string `json:"start"`
	End       string `json:"end"`
	S         int    `json:"s"`
	E         int    `json:"e"`
	Cross     bool   `json:"cross"`
	Minutes   int    `json:"minutes"`
	Mask      int    `json:"mask"`
	Contact   string `json:"contact,omitempty"`
	Remark    string `json:"remark,omitempty"`
	Enabled   bool   `json:"enabled"`
	UpdatedBy string `json:"updated_by,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Version   string `json:"version"` // 修改时原样带回，别人改过就拒绝保存（见 ShiftInput.Version）
}

// OverrideView 是管理页里的一条临时调班。
type OverrideView struct {
	ID         uint   `json:"id"`
	User       string `json:"user"`
	Display    string `json:"display"`
	Email      string `json:"email,omitempty"`
	Active     bool   `json:"active"`
	Missing    bool   `json:"missing"`
	Start      string `json:"start"`
	End        string `json:"end"`
	StartTS    int64  `json:"start_ts"`
	EndTS      int64  `json:"end_ts"`
	Reason     string `json:"reason,omitempty"`
	Contact    string `json:"contact,omitempty"`
	Canceled   bool   `json:"canceled"`
	CanceledBy string `json:"canceled_by,omitempty"`
	CanceledAt string `json:"canceled_at,omitempty"`
	CreatedBy  string `json:"created_by,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	State      string `json:"state"` // upcoming 未开始 / active 进行中 / past 已结束 / canceled 已撤销
}

// DayView 是某一天的值班安排。
type DayView struct {
	Date     string    `json:"date"`
	Weekday  int       `json:"weekday"`
	Today    bool      `json:"today"`
	Segments []SegView `json:"segments"`
}

// PersonView 是下拉里的一个账号（只给登录名和显示名；邮箱不在名单里暴露）。
type PersonView struct {
	Username string `json:"username"`
	Display  string `json:"display"`
}

// Limits 是输入的上限（页面用来限制输入框）。
type Limits struct {
	Name         int `json:"name"`
	Contact      int `json:"contact"`
	Remark       int `json:"remark"`
	Shifts       int `json:"shifts"`
	OverrideDays int `json:"override_days"`
}

// Overview 是管理页的全部数据。
type Overview struct {
	Ready     bool           `json:"ready"` // false：值班表还没建
	TZ        string         `json:"tz"`
	Now       int64          `json:"now"`
	Current   NowView        `json:"current"`
	Shifts    []ShiftView    `json:"shifts"`
	Overrides []OverrideView `json:"overrides"`
	Gaps      []Gap          `json:"gaps"`
	Days      []DayView      `json:"days"`
	Users     []PersonView   `json:"users"`
	Limits    Limits         `json:"limits"`
}

const timeLayout = "2006-01-02 15:04"

// tzLabel 把时区写成 UTC+08:00。
func tzLabel(t time.Time) string {
	_, off := t.Zone()
	sign := '+'
	if off < 0 {
		sign, off = '-', -off
	}
	return fmt.Sprintf("UTC%c%02d:%02d", sign, off/3600, off%3600/60)
}

// clockOrDate 是时间点的显示：和 ref 同一天只写钟点，否则带上月日。
func clockOrDate(t, ref time.Time) string {
	t = t.In(ref.Location())
	ty, tm, td := t.Date()
	ry, rm, rd := ref.Date()
	if ty == ry && tm == rm && td == rd {
		return t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

func minuteOf(t time.Time) int { return t.Hour()*60 + t.Minute() }

// segView 把一天里的一段转成 SegView；day 是这一天的 00:00。
func segView(g Segment, day, now time.Time, people map[string]Person) SegView {
	s, e := 0, dayMinutes
	if g.Start.After(day) {
		s = minuteOf(g.Start)
	}
	if g.End.Before(day.AddDate(0, 0, 1)) {
		e = minuteOf(g.End)
	}
	v := SegView{S: s, E: e, Start: FormatClock(s), End: endClockOfDay(e), Prev: g.FromPrev, Cont: g.ToNext}
	if sl := g.Slot; sl == nil {
		v.Gap = true
	} else {
		p := people[sl.Username]
		if p.Username == "" {
			p.Username = sl.Username
		}
		v.Kind, v.Name, v.User, v.Display, v.Reason = sl.Kind, sl.Name, sl.Username, p.Name(), sl.Reason
		v.ShiftID, v.OverrideID = sl.ShiftID, sl.OverrideID
	}
	if !now.Before(g.Start) && now.Before(g.End) {
		v.Cur = true
	} else if !now.Before(g.End) {
		v.Past = true
	}
	return v
}

func segViews(segs []Segment, day, now time.Time, people map[string]Person) []SegView {
	out := make([]SegView, 0, len(segs))
	for _, g := range segs {
		out = append(out, segView(g, day, now, people))
	}
	return out
}

func shiftView(sh Shift, people map[string]Person) ShiftView {
	p, ok := people[sh.Username]
	v := ShiftView{ID: sh.ID, Name: sh.Name, User: sh.Username, Display: sh.Username, Active: ok && p.Active, Missing: !ok,
		Start: FormatClock(sh.StartMin), End: endClock(sh.EndMin), S: sh.StartMin, E: sh.EndMin, Cross: sh.Cross(), Minutes: sh.Minutes(),
		Mask: int(sh.Weekdays), Contact: sh.Contact, Remark: sh.Remark, Enabled: sh.Enabled, UpdatedBy: sh.UpdatedBy, Version: versionOf(&sh)}
	if ok {
		v.Display, v.Email = p.Name(), p.Email
	}
	if !sh.UpdatedAt.IsZero() {
		v.UpdatedAt = sh.UpdatedAt.Format(timeLayout)
	}
	return v
}

func overrideView(o Override, people map[string]Person, now time.Time) OverrideView {
	p, ok := people[o.Username]
	v := OverrideView{ID: o.ID, User: o.Username, Display: o.Username, Active: ok && p.Active, Missing: !ok,
		Start: o.Start.Format(timeLayout), End: o.End.Format(timeLayout), StartTS: o.Start.Unix(), EndTS: o.End.Unix(),
		Reason: o.Reason, Contact: o.Contact, Canceled: o.Canceled, CanceledBy: o.CanceledBy, CreatedBy: o.CreatedBy}
	if ok {
		v.Display, v.Email = p.Name(), p.Email
	}
	if o.CanceledAt != nil {
		v.CanceledAt = o.CanceledAt.Format(timeLayout)
	}
	if !o.CreatedAt.IsZero() {
		v.CreatedAt = o.CreatedAt.Format(timeLayout)
	}
	switch {
	case o.Canceled:
		v.State = "canceled"
	case !now.Before(o.End):
		v.State = "past"
	case !now.Before(o.Start):
		v.State = "active"
	default:
		v.State = "upcoming"
	}
	return v
}
