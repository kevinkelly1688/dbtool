// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package duty

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// InputError 是输入有误 / 冲突 / 找不到的业务错误：Kind 决定 HTTP 状态（bad_request 400、conflict 409、not_found 404）。
// Msg 是中文原文（带 %s %d 的格式串已经代入），HTTP 出口按请求语言翻译。
type InputError struct {
	Kind string
	Msg  string
}

func (e *InputError) Error() string { return e.Msg }

func badReq(format string, args ...any) error {
	return &InputError{Kind: "bad_request", Msg: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) error {
	return &InputError{Kind: "conflict", Msg: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) error {
	return &InputError{Kind: "not_found", Msg: fmt.Sprintf(format, args...)}
}

// ShiftInput 是新建 / 修改班次的输入。
type ShiftInput struct {
	Name     string `json:"name"`
	Username string `json:"username"`
	Start    string `json:"start"`    // HH:MM
	End      string `json:"end"`      // HH:MM，24:00 = 次日 00:00
	Weekdays []int  `json:"weekdays"` // 0 周日 … 6 周六
	Contact  string `json:"contact"`
	Remark   string `json:"remark"`
	Enabled  *bool  `json:"enabled"` // 新建默认启用；修改时不给就保持原样
	// Version 是修改时带上打开编辑框那一刻的版本（ShiftView.Version）：别人在这期间改过这个班次就拒绝保存，免得悄悄盖掉别人的修改。
	// 不给就不校验（接口调用方自己负责）。
	Version string `json:"version"`
}

// versionOf 是班次的版本号：由所有可改的字段和最后修改时间算出来。库里的时间只到秒，
// 光看时间的话，同一秒里的两次修改分不开；把内容也算进去，这种情况也能发现。
func versionOf(sh *Shift) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%s|%s|%d|%d|%d|%s|%s|%t|%d", sh.ID, sh.Name, sh.Username, sh.StartMin, sh.EndMin, sh.Weekdays, sh.Contact, sh.Remark, sh.Enabled, sh.UpdatedAt.Unix())
	return strconv.FormatUint(h.Sum64(), 36)
}

// 为了与 1.67 的包里已建的表兼容，操作人写进 VARCHAR(64) 的列之前先截断（新建的表是 VARCHAR(150)）。
const byMaxRunes = 64

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// OverrideInput 是新建临时调班的输入；时间是服务器时区的 "2006-01-02 15:04"（也认 T 分隔）。
type OverrideInput struct {
	Username string `json:"username"`
	Start    string `json:"start"`
	End      string `json:"end"`
	Reason   string `json:"reason"`
	Contact  string `json:"contact"`
}

// Service 把存取、用户目录和时间计算串起来；读到的数据缓存 30 秒，写操作后立即失效。
type Service struct {
	store Store
	dir   Directory
	loc   *time.Location
	clock func() time.Time
	ttl   time.Duration

	mu sync.Mutex
	c  *cache

	wmu sync.Mutex // 串行化写操作：重叠校验和保存之间不被另一个请求插队
}

// cache 是一次取数的结果（班次、近期调班、涉及的账号）。
type cache struct {
	at        time.Time
	ready     bool
	err       error
	shifts    []Shift
	overrides []Override
	people    map[string]Person
}

const (
	cacheTTL        = 30 * time.Second
	retryAfter      = 5 * time.Second // 取数失败后，隔多久再试
	overrideHistory = 30              // 管理页里保留多少天内结束的调班
	daysShown       = 7               // 管理页的时间条预览天数
)

func New(store Store, dir Directory) *Service {
	return &Service{store: store, dir: dir, loc: time.Local, clock: time.Now, ttl: cacheTTL}
}

// SetLocation 指定时区（默认服务器本地时区；测试用）。
func (s *Service) SetLocation(loc *time.Location) *Service { s.loc = loc; return s }

// SetClock 指定“现在”（测试用）。
func (s *Service) SetClock(now func() time.Time) *Service { s.clock = now; return s }

func (s *Service) now() time.Time { return s.clock().In(s.loc) }

// Invalidate 丢掉缓存，下次读取重新取数。
func (s *Service) Invalidate() {
	s.mu.Lock()
	s.c = nil
	s.mu.Unlock()
}

func usernamesOf(shifts []Shift, overrides []Override) []string {
	seen := map[string]bool{}
	var out []string
	add := func(u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, sh := range shifts {
		add(sh.Username)
	}
	for _, o := range overrides {
		add(o.Username)
	}
	return out
}

func (s *Service) fetch(now time.Time) (*cache, error) {
	c := &cache{at: now}
	if !s.store.Ready() {
		return c, nil
	}
	var err error
	if c.shifts, err = s.store.ListShifts(); err == nil {
		c.overrides, err = s.store.ListOverrides(now.AddDate(0, 0, -overrideHistory))
	}
	if errors.Is(err, ErrTableMissing) {
		return &cache{at: now}, nil
	}
	if err != nil {
		return nil, err
	}
	c.ready = true
	if c.people, err = s.dir.Lookup(usernamesOf(c.shifts, c.overrides)); err != nil {
		return nil, err
	}
	return c, nil
}

// data 返回缓存的数据；取数失败时继续用旧的，没有旧的就返回带 err 的空缓存（几秒后再试）。
func (s *Service) data() *cache {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c != nil {
		if age := now.Sub(s.c.at); age >= 0 && age < s.ttl {
			return s.c
		}
	}
	c, err := s.fetch(now)
	if err != nil {
		if s.c != nil && s.c.err == nil {
			s.c.at = now.Add(-s.ttl + retryAfter)
			return s.c
		}
		c = &cache{at: now.Add(-s.ttl + retryAfter), err: err}
	}
	s.c = c
	return c
}

func (c *cache) person(u string) Person {
	if p, ok := c.people[u]; ok {
		return p
	}
	return Person{Username: u}
}

// effective 去掉值班人账号已停用 / 删除的班次与调班：那段时间按“没人值班”算。
func (c *cache) effective() ([]Shift, []Override) {
	var sh []Shift
	for _, x := range c.shifts {
		if p, ok := c.people[x.Username]; ok && p.Active {
			sh = append(sh, x)
		}
	}
	// 调班人的账号停用 / 删除：调班不能整条丢掉，否则这段时间会回落给被顶班的人（他多半正在请假）。
	// 留着并标成 Void：这段时间归这条调班，但没人值班，显示“暂无”，和管理页的红字提示一致。
	var ov []Override
	for _, o := range c.overrides {
		if p, ok := c.people[o.Username]; !ok || !p.Active {
			o.Void = true
		}
		ov = append(ov, o)
	}
	return sh, ov
}

// inUse 报告值班功能是不是在用：有启用的班次，或者还没结束的有效调班。都没有时顶栏不显示。
func (c *cache) inUse(at time.Time) bool {
	for _, sh := range c.shifts {
		if sh.Enabled {
			return true
		}
	}
	for _, o := range c.overrides {
		if !o.Canceled && o.End.After(at) {
			return true
		}
	}
	return false
}

// Now 是 at 这一刻的值班快照。功能没在用、表没建、取数失败时 Enabled 为 false。
func (s *Service) Now(at time.Time) NowView {
	at = at.In(s.loc)
	c := s.data()
	if c.err != nil || !c.ready || !c.inUse(at) {
		return NowView{}
	}
	shifts, ovs := c.effective()
	return s.nowView(c, Compute(shifts, ovs, at), shifts)
}

func (s *Service) nowView(c *cache, snap Snapshot, shifts []Shift) NowView {
	v := NowView{Enabled: true, Now: snap.Now.Unix(), NowMin: minuteOf(snap.Now), TZ: tzLabel(snap.Now), Date: snap.Day.Format("2006-01-02"),
		State: "none", NextAt: snap.NextAt.Unix(), Today: segViews(snap.Today, snap.Day, snap.Now, c.people)}
	if sl := snap.Cur.Slot; sl != nil {
		p := c.person(sl.Username)
		cv := &CurView{Kind: sl.Kind, Name: sl.Name, Reason: sl.Reason, User: sl.Username, Display: p.Name(), Contact: sl.Contact, Email: p.Email, EndsAt: snap.Cur.End.Unix()}
		if sl.Kind == KindShift {
			for _, sh := range shifts {
				if sh.ID == sl.ShiftID {
					cv.Start, cv.End = FormatClock(sh.StartMin), endClock(sh.EndMin)
				}
			}
		} else {
			cv.Start, cv.End = clockOrDate(sl.Start, snap.Now), clockOrDate(sl.End, snap.Now)
		}
		v.State, v.Cur = "on", cv
	}
	if n := snap.Next; n != nil {
		nv := &NextView{At: n.Start.Unix(), Time: clockOrDate(n.Start, snap.Now)}
		if sl := n.Slot; sl == nil {
			nv.Gap = true
		} else {
			nv.User, nv.Display, nv.Name, nv.Kind = sl.Username, c.person(sl.Username).Name(), sl.Name, sl.Kind
		}
		v.Next = nv
	}
	return v
}

// Overview 是管理页的全部数据。withUsers 为 true 时带上可选的值班人名单（只有能新增 / 修改班次或调班的人需要）。
func (s *Service) Overview(withUsers bool) (Overview, error) {
	at := s.now()
	c := s.data()
	ov := Overview{TZ: tzLabel(at), Now: at.Unix(), Gaps: []Gap{}, Shifts: []ShiftView{}, Overrides: []OverrideView{}, Days: []DayView{}, Users: []PersonView{},
		Limits: Limits{Name: MaxNameRunes, Contact: MaxContactRunes, Remark: MaxRemarkRunes, Shifts: MaxShifts, OverrideDays: MaxOverrideDays}}
	if c.err != nil {
		return ov, c.err
	}
	if !c.ready {
		return ov, nil
	}
	ov.Ready = true
	for _, sh := range c.shifts {
		ov.Shifts = append(ov.Shifts, shiftView(sh, c.people))
	}
	for i := len(c.overrides) - 1; i >= 0; i-- { // 新的在前
		ov.Overrides = append(ov.Overrides, overrideView(c.overrides[i], c.people, at))
	}
	if g := Gaps(c.shifts); g != nil {
		ov.Gaps = g
	}
	shifts, ovs := c.effective()
	snap := Compute(shifts, ovs, at)
	ov.Current = s.nowView(c, snap, shifts)
	ov.Current.Enabled = c.inUse(at)
	day := snap.Day
	for i := 0; i < daysShown; i++ {
		d := day.AddDate(0, 0, i)
		dv := DayView{Date: d.Format("2006-01-02"), Weekday: int(d.Weekday()), Today: i == 0}
		now := at
		if i > 0 { // 只有今天标“当前 / 已结束”
			now = d.Add(-time.Second)
		}
		dv.Segments = segViews(Timeline(shifts, ovs, d, d.AddDate(0, 0, 1)), d, now, c.people)
		ov.Days = append(ov.Days, dv)
	}
	if withUsers {
		if people, err := s.dir.List(); err == nil {
			for _, p := range people {
				ov.Users = append(ov.Users, PersonView{Username: p.Username, Display: p.Display})
			}
		}
	}
	return ov, nil
}

// ---------------------------------------------------------------- 写操作

func (s *Service) person(username string) (Person, bool, error) {
	m, err := s.dir.Lookup([]string{username})
	if err != nil {
		return Person{}, false, err
	}
	p, ok := m[username]
	return p, ok, nil
}

func runes(s string) int { return utf8.RuneCountInString(s) }

func (s *Service) buildShift(in ShiftInput, prev *Shift) (Shift, error) {
	sh := Shift{Enabled: true}
	if prev != nil {
		sh = *prev
	}
	name := strings.TrimSpace(in.Name)
	if n := runes(name); n == 0 || n > MaxNameRunes {
		return sh, badReq("班次名称不能为空，最长 %d 个字符", MaxNameRunes)
	}
	user := strings.TrimSpace(in.Username)
	if user == "" {
		return sh, badReq("请选择值班人")
	}
	// 值班人没换时不再校验账号：账号停用后，这个班次还得能停用 / 改时间 / 改联系方式（管理页本来就在红字提醒“换人或恢复账号”）
	if prev == nil || prev.Username != user {
		p, ok, err := s.person(user)
		if err != nil {
			return sh, err
		}
		if !ok || !p.Active {
			return sh, badReq("值班人不存在或已停用")
		}
	}
	start, err1 := ParseClock(in.Start, false)
	end, err2 := ParseClock(in.End, true)
	if err1 != nil || err2 != nil {
		return sh, badReq("开始 / 结束时间格式应为 HH:MM")
	}
	var mask uint8
	for _, d := range in.Weekdays {
		if d < 0 || d > 6 {
			return sh, badReq("适用星期只能是 0（周日）到 6（周六）")
		}
		mask |= 1 << uint(d)
	}
	if mask == 0 {
		return sh, badReq("至少选择一个适用星期")
	}
	contact, remark := strings.TrimSpace(in.Contact), strings.TrimSpace(in.Remark)
	if runes(contact) > MaxContactRunes {
		return sh, badReq("联系方式最长 %d 个字符", MaxContactRunes)
	}
	if runes(remark) > MaxRemarkRunes {
		return sh, badReq("备注最长 %d 个字符", MaxRemarkRunes)
	}
	sh.Name, sh.Username, sh.StartMin, sh.EndMin, sh.Weekdays, sh.Contact, sh.Remark = name, user, start, end, mask, contact, remark
	if in.Enabled != nil {
		sh.Enabled = *in.Enabled
	}
	return sh, nil
}

func (s *Service) checkConflict(sh Shift) error {
	others, err := s.store.ListShifts()
	if err != nil {
		return err
	}
	if c := FindConflict(sh, others); c != nil {
		return conflict("与班次「%s」时间重叠：%s %s–%s", c.With.Name, weekdayNames[c.Run.Day], c.Run.StartClock, c.Run.EndClock)
	}
	return nil
}

// CreateShift 新增班次。
func (s *Service) CreateShift(by string, in ShiftInput) (*Shift, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	sh, err := s.buildShift(in, nil)
	if err != nil {
		return nil, err
	}
	all, err := s.store.ListShifts()
	if err != nil {
		return nil, err
	}
	if len(all) >= MaxShifts {
		return nil, badReq("班次最多 %d 个", MaxShifts)
	}
	if err := s.checkConflict(sh); err != nil {
		return nil, err
	}
	now := s.now()
	by = trunc(by, byMaxRunes)
	sh.CreatedBy, sh.CreatedAt, sh.UpdatedBy, sh.UpdatedAt = by, now, by, now
	if err := s.store.SaveShift(&sh); err != nil {
		return nil, err
	}
	s.Invalidate()
	return &sh, nil
}

// UpdateShift 修改班次，返回修改前后的班次。
func (s *Service) UpdateShift(by string, id uint, in ShiftInput) (old, cur *Shift, err error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	prev, err := s.store.GetShift(id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, notFound("班次不存在")
	}
	if err != nil {
		return nil, nil, err
	}
	if in.Version != "" && in.Version != versionOf(prev) {
		return nil, nil, conflict("这个班次刚被别人修改过，请刷新页面后重新编辑")
	}
	sh, err := s.buildShift(in, prev)
	if err != nil {
		return nil, nil, err
	}
	if err := s.checkConflict(sh); err != nil {
		return nil, nil, err
	}
	sh.UpdatedBy, sh.UpdatedAt = trunc(by, byMaxRunes), s.now()
	if err := s.store.SaveShift(&sh); err != nil {
		return nil, nil, err
	}
	s.Invalidate()
	return prev, &sh, nil
}

// SetShiftEnabled 只改班次的“启用”状态（管理页的“停用 / 启用”按钮）：以库里的最新内容为准，
// 不会用页面上可能已经过期的整行去覆盖别人刚做的修改。启用时重新校验重叠。返回修改前后的班次。
func (s *Service) SetShiftEnabled(by string, id uint, enabled bool) (old, cur *Shift, err error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	prev, err := s.store.GetShift(id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, notFound("班次不存在")
	}
	if err != nil {
		return nil, nil, err
	}
	sh := *prev
	sh.Enabled = enabled
	if err := s.checkConflict(sh); err != nil {
		return nil, nil, err
	}
	sh.UpdatedBy, sh.UpdatedAt = trunc(by, byMaxRunes), s.now()
	if err := s.store.SaveShift(&sh); err != nil {
		return nil, nil, err
	}
	s.Invalidate()
	return prev, &sh, nil
}

// DeleteShift 删除班次，返回被删的班次。
func (s *Service) DeleteShift(id uint) (*Shift, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	prev, err := s.store.GetShift(id)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound("班次不存在")
	}
	if err != nil {
		return nil, err
	}
	if err := s.store.DeleteShift(id); err != nil {
		return nil, err
	}
	s.Invalidate()
	return prev, nil
}

// parseLocal 解析服务器时区的 "2006-01-02 15:04"（也认 T 分隔、带秒）。
func parseLocal(v string, loc *time.Location) (time.Time, bool) {
	v = strings.TrimSpace(v)
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, v, loc); err == nil {
			return t.Truncate(time.Minute), true
		}
	}
	return time.Time{}, false
}

// CreateOverride 新增临时调班。
func (s *Service) CreateOverride(by string, in OverrideInput) (*Override, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	user := strings.TrimSpace(in.Username)
	if user == "" {
		return nil, badReq("请选择值班人")
	}
	p, ok, err := s.person(user)
	if err != nil {
		return nil, err
	}
	if !ok || !p.Active {
		return nil, badReq("值班人不存在或已停用")
	}
	start, ok1 := parseLocal(in.Start, s.loc)
	end, ok2 := parseLocal(in.End, s.loc)
	if !ok1 || !ok2 {
		return nil, badReq("开始 / 结束时间格式应为 年-月-日 时:分")
	}
	now := s.now()
	switch {
	case !end.After(start):
		return nil, badReq("结束时间必须晚于开始时间")
	case end.Sub(start) > MaxOverrideDays*24*time.Hour:
		return nil, badReq("一条调班最长 %d 天", MaxOverrideDays)
	case !end.After(now):
		return nil, badReq("结束时间已经过去了")
	case start.After(now.AddDate(1, 0, 0)):
		return nil, badReq("开始时间最远只能排到一年以后")
	}
	reason, contact := strings.TrimSpace(in.Reason), strings.TrimSpace(in.Contact)
	if runes(reason) > MaxRemarkRunes {
		return nil, badReq("原因最长 %d 个字符", MaxRemarkRunes)
	}
	if runes(contact) > MaxContactRunes {
		return nil, badReq("联系方式最长 %d 个字符", MaxContactRunes)
	}
	// 重叠校验要看到所有可能重叠的调班：开始时间在过去的调班（“从昨天晚上起顶班”）会和已经结束的调班重叠，所以从较早的那个时刻查起
	since := now
	if start.Before(since) {
		since = start
	}
	existing, err := s.store.ListOverrides(since)
	if err != nil {
		return nil, err
	}
	open := 0
	for _, o := range existing {
		if !o.Canceled && o.End.After(now) {
			open++
		}
	}
	if open >= MaxOpenOverrides {
		return nil, badReq("未结束的调班最多 %d 条，请先撤销用不上的", MaxOpenOverrides)
	}
	for _, o := range existing {
		if !o.Canceled && o.Start.Before(end) && start.Before(o.End) {
			who := o.Username
			if q, ok, _ := s.person(o.Username); ok {
				who = q.Name()
			}
			return nil, conflict("该时段已有调班（%s，%s 至 %s），请先撤销它", who, o.Start.In(s.loc).Format(timeLayout), o.End.In(s.loc).Format(timeLayout))
		}
	}
	o := &Override{Username: user, Start: start, End: end, Reason: reason, Contact: contact, CreatedBy: trunc(by, byMaxRunes), CreatedAt: now}
	if err := s.store.CreateOverride(o); err != nil {
		return nil, err
	}
	s.Invalidate()
	return o, nil
}

// CancelOverride 撤销临时调班（只能撤销还没结束的），返回被撤销的调班。
func (s *Service) CancelOverride(by string, id uint) (*Override, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	o, err := s.store.GetOverride(id)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound("调班不存在")
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	switch {
	case o.Canceled:
		return nil, conflict("这条调班已经撤销过了")
	case !o.End.After(now):
		return nil, conflict("这条调班已经结束，不能再撤销")
	}
	if err := s.store.CancelOverride(id, trunc(by, byMaxRunes), now); err != nil {
		return nil, err
	}
	s.Invalidate()
	return o, nil
}
