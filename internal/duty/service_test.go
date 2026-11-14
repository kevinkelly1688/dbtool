// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package duty

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixture struct {
	svc   *Service
	store *MemoryStore
	dir   StaticDirectory
	now   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		store: NewMemoryStore(),
		dir: StaticDirectory{
			{Username: "zhangsan", Display: "张三", Email: "zhangsan@x.example", Active: true},
			{Username: "lisi", Display: "李四", Email: "lisi@x.example", Active: true},
			{Username: "zhaowu", Display: "赵五", Email: "zhaowu@x.example", Active: true},
			{Username: "wangwu", Display: "王五", Active: true},
			{Username: "left", Display: "离职的人", Active: false},
		},
		now: at(0, 10, 30),
	}
	f.svc = New(f.store, f.dir).SetLocation(cst).SetClock(func() time.Time { return f.now })
	return f
}

func (f *fixture) seedThree(t *testing.T) {
	t.Helper()
	for _, in := range []ShiftInput{
		{Name: "白班", Username: "zhangsan", Start: "08:00", End: "19:00", Weekdays: allDays(), Contact: "分机 8001"},
		{Name: "晚班", Username: "lisi", Start: "19:00", End: "24:00", Weekdays: allDays()},
		{Name: "夜班", Username: "zhaowu", Start: "00:00", End: "08:00", Weekdays: allDays()},
	} {
		if _, err := f.svc.CreateShift("root", in); err != nil {
			t.Fatalf("创建 %s：%v", in.Name, err)
		}
	}
}

func allDays() []int { return []int{0, 1, 2, 3, 4, 5, 6} }

func wantInput(t *testing.T, err error, kind, contains string) {
	t.Helper()
	var ie *InputError
	if !errors.As(err, &ie) {
		t.Fatalf("应返回 InputError(%s)：%v", kind, err)
	}
	if ie.Kind != kind || !strings.Contains(ie.Msg, contains) {
		t.Fatalf("错误 = %s %q，期望 %s 含 %q", ie.Kind, ie.Msg, kind, contains)
	}
}

func TestCreateShiftValidation(t *testing.T) {
	f := newFixture(t)
	ok := ShiftInput{Name: "白班", Username: "zhangsan", Start: "08:00", End: "19:00", Weekdays: allDays()}
	mut := func(fn func(*ShiftInput)) ShiftInput { in := ok; fn(&in); return in }
	for _, c := range []struct {
		name string
		in   ShiftInput
		want string
	}{
		{"名称为空", mut(func(i *ShiftInput) { i.Name = "  " }), "班次名称"},
		{"名称太长", mut(func(i *ShiftInput) { i.Name = strings.Repeat("长", 41) }), "班次名称"},
		{"没选人", mut(func(i *ShiftInput) { i.Username = "" }), "请选择值班人"},
		{"人不存在", mut(func(i *ShiftInput) { i.Username = "nobody" }), "不存在或已停用"},
		{"人已停用", mut(func(i *ShiftInput) { i.Username = "left" }), "不存在或已停用"},
		{"开始时间格式", mut(func(i *ShiftInput) { i.Start = "8点" }), "HH:MM"},
		{"开始不能写 24:00", mut(func(i *ShiftInput) { i.Start = "24:00" }), "HH:MM"},
		{"结束时间格式", mut(func(i *ShiftInput) { i.End = "25:00" }), "HH:MM"},
		{"没选星期", mut(func(i *ShiftInput) { i.Weekdays = nil }), "至少选择一个适用星期"},
		{"星期超范围", mut(func(i *ShiftInput) { i.Weekdays = []int{7} }), "适用星期"},
		{"联系方式太长", mut(func(i *ShiftInput) { i.Contact = strings.Repeat("1", 101) }), "联系方式"},
		{"备注太长", mut(func(i *ShiftInput) { i.Remark = strings.Repeat("a", 201) }), "备注"},
	} {
		_, err := f.svc.CreateShift("root", c.in)
		var ie *InputError
		if !errors.As(err, &ie) || ie.Kind != "bad_request" || !strings.Contains(ie.Msg, c.want) {
			t.Errorf("%s：%v，期望 bad_request 含 %q", c.name, err, c.want)
		}
	}
	sh, err := f.svc.CreateShift("root", mut(func(i *ShiftInput) { i.Name = " 白班 "; i.Contact = " 8001 " }))
	if err != nil || sh.ID == 0 || sh.Name != "白班" || sh.Contact != "8001" || !sh.Enabled || sh.CreatedBy != "root" || sh.StartMin != 480 || sh.EndMin != 1140 {
		t.Fatalf("正常创建：%+v %v", sh, err)
	}
	if sh.Weekdays != AllWeek {
		t.Errorf("每天 = 127：%d", sh.Weekdays)
	}
}

func TestCreateShiftEndClockForms(t *testing.T) {
	f := newFixture(t)
	// 结束写 24:00 和 00:00 等价，都是次日 00:00
	a, err := f.svc.CreateShift("root", ShiftInput{Name: "晚班", Username: "lisi", Start: "19:00", End: "24:00", Weekdays: []int{1}})
	if err != nil || a.EndMin != 0 {
		t.Fatalf("19:00–24:00：%+v %v", a, err)
	}
	b, err := f.svc.CreateShift("root", ShiftInput{Name: "晚班2", Username: "lisi", Start: "19:00", End: "00:00", Weekdays: []int{2}})
	if err != nil || b.EndMin != 0 {
		t.Fatalf("19:00–00:00：%+v %v", b, err)
	}
	// 开始 = 结束 = 24 小时
	c, err := f.svc.CreateShift("root", ShiftInput{Name: "全天", Username: "zhangsan", Start: "08:00", End: "08:00", Weekdays: []int{3}})
	if err != nil || c.Minutes() != 1440 {
		t.Fatalf("24 小时班：%+v %v", c, err)
	}
}

func TestCreateShiftOverlapAndLimit(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)
	// 与白班重叠（周一 18:00–19:00）
	_, err := f.svc.CreateShift("root", ShiftInput{Name: "加班", Username: "wangwu", Start: "18:00", End: "20:00", Weekdays: []int{1}})
	wantInput(t, err, "conflict", "与班次「白班」时间重叠：周一 18:00–19:00")
	// 保存成停用的不校验
	off := false
	if _, err := f.svc.CreateShift("root", ShiftInput{Name: "备用", Username: "wangwu", Start: "18:00", End: "20:00", Weekdays: []int{1}, Enabled: &off}); err != nil {
		t.Errorf("停用的班次不做重叠校验：%v", err)
	}
	// 首尾相接可以：给白班之后补一个 19:00 开始的周一特殊班之前先删晚班
	list, _ := f.store.ListShifts()
	if len(list) != 4 {
		t.Fatalf("班次数 = %d", len(list))
	}

	// 数量上限：新库里放满 50 个互不重叠的班次，第 51 个报错
	g := newFixture(t)
	for i := 0; i < MaxShifts; i++ {
		s := i * 28
		in := ShiftInput{Name: fmt.Sprintf("班%d", i), Username: "zhangsan", Start: FormatClock(s), End: FormatClock(s + 20), Weekdays: allDays()}
		if _, err := g.svc.CreateShift("root", in); err != nil {
			t.Fatalf("第 %d 个：%v", i, err)
		}
	}
	_, err = g.svc.CreateShift("root", ShiftInput{Name: "多余", Username: "zhangsan", Start: "23:40", End: "23:50", Weekdays: allDays()})
	wantInput(t, err, "bad_request", "班次最多 50 个")
}

func TestUpdateAndDeleteShift(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)
	list, _ := f.store.ListShifts()
	day, night := list[0], list[2]

	// 不改动保存：不与自己冲突
	in := ShiftInput{Name: day.Name, Username: day.Username, Start: "08:00", End: "19:00", Weekdays: allDays(), Contact: day.Contact}
	old, cur, err := f.svc.UpdateShift("root", day.ID, in)
	if err != nil || old.Name != "白班" || cur.UpdatedBy != "root" {
		t.Fatalf("不改动保存：%+v %+v %v", old, cur, err)
	}
	// 把白班拉长到与夜班重叠
	in.Start = "07:00"
	_, _, err = f.svc.UpdateShift("root", day.ID, in)
	wantInput(t, err, "conflict", "与班次「夜班」时间重叠")
	// 换人、改联系方式：保存成功，并带回新旧两份
	in.Start = "08:00"
	in.Username, in.Contact = "wangwu", "8888"
	old, cur, err = f.svc.UpdateShift("root", day.ID, in)
	if err != nil || old.Username != "zhangsan" || cur.Username != "wangwu" || cur.Contact != "8888" {
		t.Fatalf("换人：%+v %+v %v", old, cur, err)
	}
	// 停用再启用：启用时重新校验重叠
	off := false
	in.Enabled = &off
	if _, cur, err := f.svc.UpdateShift("root", night.ID, ShiftInput{Name: "夜班", Username: "zhaowu", Start: "00:00", End: "08:00", Weekdays: allDays(), Enabled: &off}); err != nil || cur.Enabled {
		t.Fatalf("停用：%+v %v", cur, err)
	}
	// 停用期间有人占了夜班的时间
	if _, err := f.svc.CreateShift("root", ShiftInput{Name: "临时夜班", Username: "lisi", Start: "02:00", End: "06:00", Weekdays: allDays()}); err != nil {
		t.Fatal(err)
	}
	_, _, err = f.svc.UpdateShift("root", night.ID, ShiftInput{Name: "夜班", Username: "zhaowu", Start: "00:00", End: "08:00", Weekdays: allDays()}) // Enabled 不给：保持原来的停用
	if err != nil {
		t.Errorf("保持停用不该报冲突：%v", err)
	}
	on := true
	_, _, err = f.svc.UpdateShift("root", night.ID, ShiftInput{Name: "夜班", Username: "zhaowu", Start: "00:00", End: "08:00", Weekdays: allDays(), Enabled: &on})
	wantInput(t, err, "conflict", "时间重叠")

	_, _, err = f.svc.UpdateShift("root", 999, in)
	wantInput(t, err, "not_found", "班次不存在")
	sh, err := f.svc.DeleteShift(day.ID)
	if err != nil || sh.ID != day.ID {
		t.Fatalf("删除：%+v %v", sh, err)
	}
	_, err = f.svc.DeleteShift(day.ID)
	wantInput(t, err, "not_found", "班次不存在")
}

func TestNowThreeShiftExample(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)

	v := f.svc.Now(at(0, 10, 30))
	if !v.Enabled || v.State != "on" || v.Cur == nil || v.Cur.Display != "张三" || v.Cur.Start != "08:00" || v.Cur.End != "19:00" || v.Cur.Name != "白班" {
		t.Fatalf("10:30：%+v %+v", v, v.Cur)
	}
	if v.Cur.Contact != "分机 8001" || v.Cur.Email != "zhangsan@x.example" || v.Cur.EndsAt != at(0, 19, 0).Unix() || v.NextAt != at(0, 19, 0).Unix() {
		t.Errorf("联系方式 / 结束时间：%+v nextAt=%d", v.Cur, v.NextAt)
	}
	if v.Next == nil || v.Next.Display != "李四" || v.Next.Time != "19:00" || v.Next.At != at(0, 19, 0).Unix() {
		t.Errorf("下一次交班：%+v", v.Next)
	}
	if v.TZ != "UTC+08:00" || v.Date != "2026-10-22" || v.NowMin != 10*60+30 {
		t.Errorf("时区 / 日期 / 分钟：%s %s %d", v.TZ, v.Date, v.NowMin)
	}
	if len(v.Today) != 3 || !v.Today[0].Past || !v.Today[1].Cur || v.Today[2].Cur || v.Today[2].Past {
		t.Fatalf("今日排班标记：%+v", v.Today)
	}
	if v.Today[0].S != 0 || v.Today[0].E != 480 || v.Today[1].S != 480 || v.Today[1].E != 1140 || v.Today[2].S != 1140 || v.Today[2].E != 1440 || v.Today[2].End != "24:00" {
		t.Errorf("今日排班分钟数：%+v", v.Today)
	}

	night := f.svc.Now(at(0, 3, 0))
	if night.Cur == nil || night.Cur.Display != "赵五" || night.Cur.Start != "00:00" || night.Cur.End != "08:00" {
		t.Errorf("03:00：%+v", night.Cur)
	}
	late := f.svc.Now(at(0, 23, 30))
	if late.Cur == nil || late.Cur.Display != "李四" || late.Cur.Start != "19:00" || late.Cur.End != "24:00" {
		t.Errorf("23:30：%+v", late.Cur)
	}
	if late.Next == nil || late.Next.Display != "赵五" || late.Next.Time != "10-23 00:00" {
		t.Errorf("23:30 的下一次交班：%+v", late.Next)
	}
}

func TestNowInactivePersonIsAGap(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)
	// 张三的账号被停用
	for i := range f.dir {
		if f.dir[i].Username == "zhangsan" {
			f.dir[i].Active = false
		}
	}
	f.svc.Invalidate()
	v := f.svc.Now(at(0, 10, 30))
	if !v.Enabled || v.State != "none" || v.Cur != nil {
		t.Fatalf("值班人账号停用：这段时间应显示无人值班：%+v", v)
	}
	if v.Next == nil || v.Next.Display != "李四" {
		t.Errorf("下一次交班仍是李四：%+v", v.Next)
	}
	// 管理页要能看到哪个班次的人有问题
	ov, err := f.svc.Overview(true)
	if err != nil {
		t.Fatal(err)
	}
	bad := 0
	for _, s := range ov.Shifts {
		if !s.Active {
			bad++
			if s.User != "zhangsan" || s.Missing {
				t.Errorf("停用账号：%+v", s)
			}
		}
	}
	if bad != 1 {
		t.Errorf("应有 1 个班次的值班人不可用：%d", bad)
	}
}

func TestNowVisibility(t *testing.T) {
	f := newFixture(t)
	if v := f.svc.Now(f.now); v.Enabled {
		t.Errorf("没有任何班次：不显示 %+v", v)
	}
	off := false
	sh, err := f.svc.CreateShift("root", ShiftInput{Name: "白班", Username: "zhangsan", Start: "08:00", End: "19:00", Weekdays: allDays(), Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	if v := f.svc.Now(f.now); v.Enabled {
		t.Errorf("只有停用的班次：不显示 %+v", v)
	}
	// 只有一条进行中的调班（没有启用的班次）：显示
	if _, err := f.svc.CreateOverride("root", OverrideInput{Username: "wangwu", Start: "2026-10-22 09:00", End: "2026-10-22 12:00", Reason: "顶班"}); err != nil {
		t.Fatal(err)
	}
	v := f.svc.Now(f.now)
	if !v.Enabled || v.State != "on" || v.Cur.Kind != KindOverride || v.Cur.Display != "王五" || v.Cur.Reason != "顶班" || v.Cur.Start != "09:00" || v.Cur.End != "12:00" {
		t.Errorf("调班期间：%+v %+v", v, v.Cur)
	}
	// 调班结束后、没有启用的班次：不显示
	if v := f.svc.Now(at(0, 12, 30)); v.Enabled {
		t.Errorf("调班结束且没有启用的班次：不显示 %+v", v)
	}
	// 启用班次后显示
	on := true
	if _, _, err := f.svc.UpdateShift("root", sh.ID, ShiftInput{Name: "白班", Username: "zhangsan", Start: "08:00", End: "19:00", Weekdays: allDays(), Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if v := f.svc.Now(at(0, 12, 30)); !v.Enabled || v.Cur.Display != "张三" {
		t.Errorf("启用后：%+v", v)
	}
}

func TestTableMissingAndFailures(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)
	f.store.NotReady = true
	f.svc.Invalidate()
	if v := f.svc.Now(f.now); v.Enabled {
		t.Errorf("表没建：不显示 %+v", v)
	}
	ov, err := f.svc.Overview(true)
	if err != nil || ov.Ready || len(ov.Shifts) != 0 {
		t.Errorf("表没建的概览：%+v %v", ov, err)
	}
	if _, err := f.svc.CreateShift("root", ShiftInput{Name: "x", Username: "zhangsan", Start: "01:00", End: "02:00", Weekdays: allDays()}); !errors.Is(err, ErrTableMissing) {
		t.Errorf("表没建时创建应返回 ErrTableMissing：%v", err)
	}
	// 建好表之后（缓存 30 秒内仍是旧结果，过期或失效后恢复）
	f.store.NotReady = false
	if v := f.svc.Now(f.now); v.Enabled {
		t.Error("缓存期内仍按旧结果")
	}
	f.now = f.now.Add(31 * time.Second)
	if v := f.svc.Now(f.now); !v.Enabled || v.Cur == nil {
		t.Errorf("缓存过期后应恢复：%+v", v)
	}
}

func TestCacheIsInvalidatedByWrites(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)
	if v := f.svc.Now(f.now); v.Cur.Display != "张三" {
		t.Fatal(v.Cur)
	}
	list, _ := f.store.ListShifts()
	in := ShiftInput{Name: "白班", Username: "wangwu", Start: "08:00", End: "19:00", Weekdays: allDays()}
	if _, _, err := f.svc.UpdateShift("root", list[0].ID, in); err != nil {
		t.Fatal(err)
	}
	if v := f.svc.Now(f.now); v.Cur.Display != "王五" {
		t.Errorf("修改后应立即生效：%+v", v.Cur)
	}
	// 绕过 service 直接改 store：缓存期内看不到，过期后看到
	list, _ = f.store.ListShifts()
	list[0].Username = "lisi"
	_ = f.store.SaveShift(&list[0])
	if v := f.svc.Now(f.now); v.Cur.Display != "王五" {
		t.Errorf("缓存期内保持旧值：%+v", v.Cur)
	}
	f.now = f.now.Add(time.Minute)
	if v := f.svc.Now(f.now); v.Cur.Display != "李四" {
		t.Errorf("缓存过期后读到新值：%+v", v.Cur)
	}
}

func TestCreateOverrideValidation(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)
	ok := OverrideInput{Username: "wangwu", Start: "2026-10-22 11:00", End: "2026-10-22 13:00", Reason: "请假"}
	mut := func(fn func(*OverrideInput)) OverrideInput { in := ok; fn(&in); return in }
	for _, c := range []struct {
		name string
		in   OverrideInput
		want string
	}{
		{"没选人", mut(func(i *OverrideInput) { i.Username = "" }), "请选择值班人"},
		{"人已停用", mut(func(i *OverrideInput) { i.Username = "left" }), "不存在或已停用"},
		{"时间格式", mut(func(i *OverrideInput) { i.Start = "明天" }), "年-月-日"},
		{"结束不晚于开始", mut(func(i *OverrideInput) { i.End = i.Start }), "结束时间必须晚于开始时间"},
		{"太长", mut(func(i *OverrideInput) { i.End = "2026-12-01 00:00" }), "最长 31 天"},
		{"已经过去", mut(func(i *OverrideInput) { i.Start = "2026-10-21 08:00"; i.End = "2026-10-22 10:00" }), "结束时间已经过去"},
		{"原因太长", mut(func(i *OverrideInput) { i.Reason = strings.Repeat("长", 201) }), "原因最长"},
		{"联系方式太长", mut(func(i *OverrideInput) { i.Contact = strings.Repeat("1", 101) }), "联系方式最长"},
	} {
		_, err := f.svc.CreateOverride("root", c.in)
		var ie *InputError
		if !errors.As(err, &ie) || ie.Kind != "bad_request" || !strings.Contains(ie.Msg, c.want) {
			t.Errorf("%s：%v，期望 bad_request 含 %q", c.name, err, c.want)
		}
	}
	o, err := f.svc.CreateOverride("root", mut(func(i *OverrideInput) { i.Start = "2026-10-22T11:00" })) // T 分隔也认
	if err != nil || o.ID == 0 || o.CreatedBy != "root" || !o.Start.Equal(at(0, 11, 0)) {
		t.Fatalf("正常创建：%+v %v", o, err)
	}
	// 开始时间已过、结束还没到：允许（“从现在起顶班”）
	if _, err := f.svc.CreateOverride("root", OverrideInput{Username: "lisi", Start: "2026-10-22 08:00", End: "2026-10-22 10:45"}); err != nil {
		t.Errorf("开始时间已过、结束还没到应允许：%v", err)
	}
	// 重叠
	_, err = f.svc.CreateOverride("root", OverrideInput{Username: "lisi", Start: "2026-10-22 12:00", End: "2026-10-22 14:00"})
	wantInput(t, err, "conflict", "该时段已有调班（王五，2026-10-22 11:00 至 2026-10-22 13:00）")
	// 首尾相接可以
	if _, err := f.svc.CreateOverride("root", OverrideInput{Username: "lisi", Start: "2026-10-22 13:00", End: "2026-10-22 14:00"}); err != nil {
		t.Errorf("首尾相接：%v", err)
	}
	// 撤销之后同一时段可以再建
	if _, err := f.svc.CancelOverride("root", o.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateOverride("root", OverrideInput{Username: "lisi", Start: "2026-10-22 12:00", End: "2026-10-22 12:30"}); err != nil {
		t.Errorf("撤销后可再建：%v", err)
	}
}

func TestCancelOverride(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)
	o, err := f.svc.CreateOverride("root", OverrideInput{Username: "wangwu", Start: "2026-10-22 09:00", End: "2026-10-22 12:00"})
	if err != nil {
		t.Fatal(err)
	}
	if v := f.svc.Now(f.now); v.Cur.Display != "王五" {
		t.Fatalf("调班生效：%+v", v.Cur)
	}
	got, err := f.svc.CancelOverride("bob", o.ID)
	if err != nil || got.ID != o.ID {
		t.Fatalf("撤销：%+v %v", got, err)
	}
	if v := f.svc.Now(f.now); v.Cur.Display != "张三" {
		t.Errorf("撤销后回到班次：%+v", v.Cur)
	}
	_, err = f.svc.CancelOverride("bob", o.ID)
	wantInput(t, err, "conflict", "已经撤销过了")
	_, err = f.svc.CancelOverride("bob", 999)
	wantInput(t, err, "not_found", "调班不存在")
	// 已经结束的不能撤销
	past := &Override{Username: "wangwu", Start: at(0, 1, 0), End: at(0, 2, 0), CreatedAt: at(0, 0, 30)}
	_ = f.store.CreateOverride(past)
	_, err = f.svc.CancelOverride("bob", past.ID)
	wantInput(t, err, "conflict", "已经结束")

	f.svc.Invalidate() // past 是绕过 service 直接写进 store 的
	ov, err := f.svc.Overview(true)
	if err != nil {
		t.Fatal(err)
	}
	states := map[uint]string{}
	for _, x := range ov.Overrides {
		states[x.ID] = x.State
	}
	if states[o.ID] != "canceled" || states[past.ID] != "past" {
		t.Errorf("调班状态：%+v", states)
	}
}

func TestOverviewContent(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)
	if _, err := f.svc.CreateOverride("root", OverrideInput{Username: "wangwu", Start: "2026-10-22 20:00", End: "2026-10-22 22:00", Reason: "请假"}); err != nil {
		t.Fatal(err)
	}
	ov, err := f.svc.Overview(true)
	if err != nil {
		t.Fatal(err)
	}
	if !ov.Ready || ov.TZ != "UTC+08:00" || len(ov.Shifts) != 3 || len(ov.Gaps) != 0 || len(ov.Days) != 7 || len(ov.Users) != 4 {
		t.Fatalf("概览：ready=%v shifts=%d gaps=%d days=%d users=%d", ov.Ready, len(ov.Shifts), len(ov.Gaps), len(ov.Days), len(ov.Users))
	}
	for _, u := range ov.Users {
		if u.Username == "left" {
			t.Error("停用的账号不能出现在可选名单里")
		}
	}
	d0 := ov.Days[0]
	if !d0.Today || d0.Date != "2026-10-22" || d0.Weekday != 4 {
		t.Errorf("第一天：%+v", d0)
	}
	// 今天 19:00–20:00 李四，20:00–22:00 调班王五，22:00–24:00 李四
	if len(d0.Segments) != 5 {
		t.Fatalf("今天应有 5 段：%+v", d0.Segments)
	}
	if g := d0.Segments[3]; g.Kind != KindOverride || g.Display != "王五" || g.Start != "20:00" || g.End != "22:00" || g.Reason != "请假" {
		t.Errorf("调班那一段：%+v", g)
	}
	if !d0.Segments[1].Cur || !d0.Segments[0].Past {
		t.Errorf("当前 / 已结束标记：%+v", d0.Segments)
	}
	for _, g := range ov.Days[1].Segments {
		if g.Cur || g.Past {
			t.Errorf("只有今天才标当前 / 已结束：%+v", g)
		}
	}
	// 只剩白班：每天 00:00–08:00 和 19:00–24:00 没人
	list, _ := f.store.ListShifts()
	_ = f.store.DeleteShift(list[1].ID)
	_ = f.store.DeleteShift(list[2].ID)
	f.svc.Invalidate()
	ov, _ = f.svc.Overview(true)
	if len(ov.Gaps) != 2 || ov.Gaps[0].Start != "00:00" || ov.Gaps[1].Start != "19:00" || ov.Gaps[1].End != "24:00" {
		t.Errorf("空档：%+v", ov.Gaps)
	}
}

// 页面渲染（Now）和管理操作（创建 / 修改 / 删除 / 调班）同时发生：不能 panic、不能出现数据竞争（-race 下跑）、最后的数据是自洽的。
func TestServiceConcurrentUse(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if v := f.svc.Now(at(0, 10, 30)); !v.Enabled {
					t.Error("读取过程中顶栏不应消失")
					return
				}
				if _, err := f.svc.Overview(true); err != nil {
					t.Errorf("Overview：%v", err)
					return
				}
			}
		}()
	}
	var writers sync.WaitGroup
	off := false // 停用的班次不参与重叠校验（三个基础班次已经 24 小时全覆盖），但照样走完整的写入 / 缓存失效路径
	for w := 0; w < 3; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 0; i < 20; i++ {
				s := 1000 + w*100
				sh, err := f.svc.CreateShift("w", ShiftInput{Name: fmt.Sprintf("临时%d", w), Username: "wangwu", Start: FormatClock(s), End: FormatClock(s + 50), Weekdays: []int{w}, Enabled: &off})
				if err != nil {
					t.Errorf("新增：%v", err)
					return
				}
				if _, err := f.svc.CreateOverride("w", OverrideInput{Username: "lisi", Start: fmt.Sprintf("2026-10-%02d 12:00", 23+w), End: fmt.Sprintf("2026-10-%02d 13:00", 23+w)}); err != nil && !strings.Contains(err.Error(), "已有调班") {
					t.Errorf("调班：%v", err)
					return
				}
				if _, err := f.svc.DeleteShift(sh.ID); err != nil {
					t.Errorf("删除：%v", err)
					return
				}
			}
		}(w)
	}
	writers.Wait()
	close(stop)
	wg.Wait()
	list, _ := f.store.ListShifts()
	if len(list) != 3 {
		t.Errorf("写入者各自删掉了自己建的班次，应只剩 3 个：%d", len(list))
	}
}

func TestOverviewWithoutShifts(t *testing.T) {
	f := newFixture(t)
	ov, err := f.svc.Overview(true)
	if err != nil || !ov.Ready || len(ov.Shifts) != 0 || len(ov.Gaps) != 0 || len(ov.Days) != 7 {
		t.Fatalf("没有班次的概览：%+v %v", ov, err)
	}
	if ov.Current.Enabled {
		t.Errorf("没有班次：当前值班卡片按未启用处理 %+v", ov.Current)
	}
}

func shiftViewOf(t *testing.T, f *fixture, id uint) ShiftView {
	t.Helper()
	ov, err := f.svc.Overview(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ov.Shifts {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("概览里没有班次 %d", id)
	return ShiftView{}
}

// 值班人名单（全部账号）只在被要求时给；名单里只有登录名和显示名，不带邮箱。
func TestOverviewUsersOnlyWhenAsked(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)
	ov, err := f.svc.Overview(false)
	if err != nil || !ov.Ready || len(ov.Shifts) != 3 || len(ov.Users) != 0 {
		t.Fatalf("不要名单：ready=%v shifts=%d users=%d %v", ov.Ready, len(ov.Shifts), len(ov.Users), err)
	}
	ov, _ = f.svc.Overview(true)
	if len(ov.Users) != 4 {
		t.Fatalf("要名单：%d", len(ov.Users))
	}
	b, _ := json.Marshal(ov.Users)
	if strings.Contains(string(b), "@") || strings.Contains(strings.ToLower(string(b)), "email") {
		t.Errorf("名单里不能带邮箱：%s", b)
	}
}

// 调班人的账号被停用：这段时间归这条调班但没人值班，不能回落给被顶班的人（他多半正在请假）。
func TestInactiveOverridePersonIsAGapNotTheShiftPerson(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)
	if _, err := f.svc.CreateOverride("root", OverrideInput{Username: "wangwu", Start: "2026-10-22 09:00", End: "2026-10-22 12:00", Reason: "顶班"}); err != nil {
		t.Fatal(err)
	}
	if v := f.svc.Now(f.now); v.Cur == nil || v.Cur.Display != "王五" {
		t.Fatalf("账号可用时调班生效：%+v", v.Cur)
	}
	for i := range f.dir {
		if f.dir[i].Username == "wangwu" {
			f.dir[i].Active = false
		}
	}
	f.svc.Invalidate()
	v := f.svc.Now(f.now) // 10:30
	if !v.Enabled || v.State != "none" || v.Cur != nil {
		t.Fatalf("调班人账号停用：应显示无人值班，而不是回落给张三：%+v %+v", v, v.Cur)
	}
	if v.Next == nil || v.Next.Gap || v.Next.Display != "张三" || v.Next.Time != "12:00" {
		t.Errorf("12:00 调班结束后回到张三：%+v", v.Next)
	}
	if after := f.svc.Now(at(0, 12, 30)); after.Cur == nil || after.Cur.Display != "张三" {
		t.Errorf("调班结束后：%+v", after.Cur)
	}
	ov, _ := f.svc.Overview(false)
	if len(ov.Overrides) != 1 || ov.Overrides[0].Active || ov.Overrides[0].State != "active" {
		t.Errorf("管理页要能看出这条调班的人不可用：%+v", ov.Overrides)
	}
}

// 修改班次要带打开编辑框时的版本：别人这期间改过（哪怕在同一秒里）就拒绝，免得悄悄盖掉别人的修改。
func TestUpdateShiftVersionConflict(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)
	list, _ := f.store.ListShifts()
	day := list[0]
	v0 := shiftViewOf(t, f, day.ID).Version
	if v0 == "" {
		t.Fatal("班次视图应带版本号")
	}
	in := ShiftInput{Name: "白班", Username: "zhangsan", Start: "08:00", End: "19:00", Weekdays: allDays(), Contact: "分机 8001", Version: v0}

	// A、B 同时打开了编辑框；B 先保存（时钟没动：同一秒里的修改也要能发现）
	inB := in
	inB.Contact = "分机 9999"
	if _, _, err := f.svc.UpdateShift("b", day.ID, inB); err != nil {
		t.Fatalf("B 保存：%v", err)
	}
	v1 := shiftViewOf(t, f, day.ID).Version
	if v1 == v0 {
		t.Fatal("内容变了，版本必须变")
	}
	inA := in
	inA.Remark = "A 的备注"
	_, _, err := f.svc.UpdateShift("a", day.ID, inA)
	wantInput(t, err, "conflict", "这个班次刚被别人修改过")
	if cur, _ := f.store.GetShift(day.ID); cur.Contact != "分机 9999" || cur.Remark != "" {
		t.Errorf("B 的修改应原样保留：%+v", cur)
	}
	// 重新读到最新版本后可以保存；不带版本（接口直接调用）不校验
	inA.Version = v1
	if _, _, err := f.svc.UpdateShift("a", day.ID, inA); err != nil {
		t.Errorf("带最新版本：%v", err)
	}
	inA.Version = ""
	inA.Remark = "不带版本"
	if _, _, err := f.svc.UpdateShift("a", day.ID, inA); err != nil {
		t.Errorf("不带版本：%v", err)
	}
}

// 停用 / 启用只改开关：以库里最新的内容为准；启用时重新校验重叠。
func TestSetShiftEnabled(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)
	list, _ := f.store.ListShifts()
	night := list[2]
	// 页面上看到的夜班已经过期：别人刚把联系方式改了。停用只动开关，不能把它改回去
	if _, _, err := f.svc.UpdateShift("b", night.ID, ShiftInput{Name: "夜班", Username: "zhaowu", Start: "00:00", End: "08:00", Weekdays: allDays(), Contact: "新电话"}); err != nil {
		t.Fatal(err)
	}
	old, cur, err := f.svc.SetShiftEnabled("a", night.ID, false)
	if err != nil || !old.Enabled || cur.Enabled || cur.Contact != "新电话" || cur.UpdatedBy != "a" {
		t.Fatalf("停用：%+v %+v %v", old, cur, err)
	}
	if v := f.svc.Now(at(0, 3, 0)); v.State != "none" {
		t.Errorf("夜班停用后凌晨没人值班：%+v", v.Cur)
	}
	// 停用期间这段时间被别的班次占了：启用报冲突，状态保持停用
	if _, err := f.svc.CreateShift("root", ShiftInput{Name: "临时夜班", Username: "lisi", Start: "02:00", End: "06:00", Weekdays: allDays()}); err != nil {
		t.Fatal(err)
	}
	_, _, err = f.svc.SetShiftEnabled("a", night.ID, true)
	wantInput(t, err, "conflict", "时间重叠")
	if got, _ := f.store.GetShift(night.ID); got.Enabled {
		t.Error("冲突时不能改成启用")
	}
	_, _, err = f.svc.SetShiftEnabled("a", 999, true)
	wantInput(t, err, "not_found", "班次不存在")
}

// 值班人的账号停用后，管理页提示“换人或恢复账号”，在此之前这个班次本身还得能管理（改联系方式、停用）；换成另一个不可用的账号才拒绝。
func TestShiftOfInactivePersonCanStillBeManaged(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)
	list, _ := f.store.ListShifts()
	day := list[0]
	for i := range f.dir {
		if f.dir[i].Username == "zhangsan" {
			f.dir[i].Active = false
		}
	}
	f.svc.Invalidate()
	in := ShiftInput{Name: "白班", Username: "zhangsan", Start: "08:00", End: "19:00", Weekdays: allDays(), Contact: "新分机"}
	if _, cur, err := f.svc.UpdateShift("root", day.ID, in); err != nil || cur.Contact != "新分机" {
		t.Fatalf("值班人没换：改联系方式应成功：%+v %v", cur, err)
	}
	if _, cur, err := f.svc.SetShiftEnabled("root", day.ID, false); err != nil || cur.Enabled {
		t.Fatalf("停用：%+v %v", cur, err)
	}
	in.Username = "left"
	_, _, err := f.svc.UpdateShift("root", day.ID, in)
	wantInput(t, err, "bad_request", "不存在或已停用")
	in.Username = "wangwu"
	if _, cur, err := f.svc.UpdateShift("root", day.ID, in); err != nil || cur.Username != "wangwu" {
		t.Fatalf("换成可用的人：%+v %v", cur, err)
	}
}

func TestCreateOverrideLimits(t *testing.T) {
	f := newFixture(t)
	f.seedThree(t)
	// 开始时间最远排到一年以后（现在是 2026-10-22）
	_, err := f.svc.CreateOverride("root", OverrideInput{Username: "wangwu", Start: "2027-11-01 00:00", End: "2027-11-01 08:00"})
	wantInput(t, err, "bad_request", "一年以后")
	if _, err := f.svc.CreateOverride("root", OverrideInput{Username: "wangwu", Start: "2027-10-01 00:00", End: "2027-10-01 08:00"}); err != nil {
		t.Errorf("一年以内：%v", err)
	}

	// 没撤销、没结束的调班最多 MaxOpenOverrides 条
	g := newFixture(t)
	g.seedThree(t)
	base := at(10, 0, 0)
	var first *Override
	for i := 0; i < MaxOpenOverrides; i++ {
		s := base.Add(time.Duration(i) * time.Hour)
		o, err := g.svc.CreateOverride("root", OverrideInput{Username: "wangwu", Start: s.Format("2006-01-02 15:04"), End: s.Add(30 * time.Minute).Format("2006-01-02 15:04")})
		if err != nil {
			t.Fatalf("第 %d 条：%v", i, err)
		}
		if first == nil {
			first = o
		}
	}
	extra := OverrideInput{Username: "wangwu", Start: "2027-01-01 00:00", End: "2027-01-01 01:00"}
	_, err = g.svc.CreateOverride("root", extra)
	wantInput(t, err, "bad_request", fmt.Sprintf("最多 %d 条", MaxOpenOverrides))
	if _, err := g.svc.CancelOverride("root", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := g.svc.CreateOverride("root", extra); err != nil {
		t.Errorf("撤销一条后可以再建：%v", err)
	}
}

// 新调班的开始时间在过去时，要和已经结束的调班比重叠（以前只查还没结束的，漏掉了这种情况）。
func TestCreateOverrideOverlapsEndedOverride(t *testing.T) {
	f := newFixture(t) // 现在 10:30
	f.seedThree(t)
	past := &Override{Username: "lisi", Start: at(0, 8, 0), End: at(0, 9, 0), CreatedAt: at(0, 7, 0)}
	if err := f.store.CreateOverride(past); err != nil {
		t.Fatal(err)
	}
	f.svc.Invalidate()
	_, err := f.svc.CreateOverride("root", OverrideInput{Username: "wangwu", Start: "2026-10-22 07:00", End: "2026-10-22 11:00"})
	wantInput(t, err, "conflict", "该时段已有调班")
	if _, err := f.svc.CreateOverride("root", OverrideInput{Username: "wangwu", Start: "2026-10-22 09:00", End: "2026-10-22 11:00"}); err != nil {
		t.Errorf("避开已结束的那条：%v", err)
	}
}

// 操作人写进库之前截断（旧版建的表里这几列只有 64 个字符宽）。
func TestOperatorNameIsTruncated(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("操", 150)
	sh, err := f.svc.CreateShift(long, ShiftInput{Name: "白班", Username: "zhangsan", Start: "08:00", End: "19:00", Weekdays: allDays()})
	if err != nil || runes(sh.CreatedBy) != byMaxRunes || runes(sh.UpdatedBy) != byMaxRunes {
		t.Fatalf("新增班次：%d %v", runes(sh.CreatedBy), err)
	}
	if _, cur, err := f.svc.UpdateShift(long, sh.ID, ShiftInput{Name: "白班", Username: "zhangsan", Start: "08:00", End: "19:00", Weekdays: allDays(), Contact: "1"}); err != nil || runes(cur.UpdatedBy) != byMaxRunes {
		t.Fatalf("修改班次：%v", err)
	}
	o, err := f.svc.CreateOverride(long, OverrideInput{Username: "wangwu", Start: "2026-10-22 11:00", End: "2026-10-22 12:00"})
	if err != nil || runes(o.CreatedBy) != byMaxRunes {
		t.Fatalf("新增调班：%v", err)
	}
	if _, err := f.svc.CancelOverride(long, o.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := f.store.GetOverride(o.ID)
	if runes(got.CanceledBy) != byMaxRunes {
		t.Errorf("撤销人：%d", runes(got.CanceledBy))
	}
}
