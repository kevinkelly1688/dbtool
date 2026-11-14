// @author: kevinkelly
// @license: Apache Licence
// @file: sla_test.go
// @time: 2026/10/28

package perf

import (
	"strings"
	"testing"
	"time"

	"dbtool/internal/notify"
	"dbtool/internal/settings"
)

func (f *fx) runSLA() SLAResult {
	f.t.Helper()
	r, err := f.svc.RunSLA()
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func TestSLAEscalationLevels(t *testing.T) {
	f := newFx(t)
	id := f.newIssue() // P0：受理 4h，解决 48h，超时 24h 升总监，催办间隔 4h
	t0 := f.now
	f.n.reset()

	f.now = t0.Add(time.Hour)
	if r := f.runSLA(); len(r.Actions) != 0 || r.Checked != 1 {
		t.Fatalf("没到时限不应有动作: %+v", r)
	}

	// 超过受理时限：L1 催办负责人
	f.now = t0.Add(5 * time.Hour)
	if r := f.runSLA(); r.Actions["L1"] != 1 {
		t.Fatalf("应 L1: %+v", r)
	}
	i := f.issue(id)
	if i.EscalationLevel != 1 || i.RemindCount != 1 || i.LastRemindAt == nil || i.SLABreached {
		t.Fatalf("L1 后状态: %+v", i)
	}
	ev := f.n.last()
	if ev.Name != notify.EventException || ev.GroupID != 10 || !hasUser(ev, "zhangsan") || hasUser(ev, "tl") ||
		!strings.Contains(ev.Title, "催办：超过受理时限(4h)仍未受理") || !strings.Contains(ev.Content, "剩余") {
		t.Fatalf("L1 通知: %+v", ev)
	}
	if got := f.repo.logActions(id); got[len(got)-1] != ActEscalate {
		t.Fatalf("日志: %v", got)
	}

	// 间隔内不重复催办
	f.now = t0.Add(5*time.Hour + 30*time.Minute)
	n := f.n.count()
	if r := f.runSLA(); len(r.Actions) != 0 || f.n.count() != n {
		t.Fatalf("间隔内不应再催: %+v", r)
	}

	// 超过间隔：同级别重复催办，不再写升级日志
	f.now = t0.Add(9*time.Hour + 30*time.Minute)
	logs := len(f.repo.logs)
	if r := f.runSLA(); r.Actions["L1"] != 1 {
		t.Fatalf("应重复 L1: %+v", r)
	}
	if i = f.issue(id); i.RemindCount != 2 || len(f.repo.logs) != logs {
		t.Fatalf("重复催办: %+v logs=%d/%d", i, len(f.repo.logs), logs)
	}
	if ev = f.n.last(); !strings.Contains(ev.Title, "（第2次）") {
		t.Fatalf("次数: %s", ev.Title)
	}

	// 超过解决时限：L2，升级团队 TL 与跟进 DBA，并记超时
	f.now = t0.Add(49 * time.Hour)
	if r := f.runSLA(); r.Actions["L2"] != 1 {
		t.Fatalf("应 L2: %+v", r)
	}
	i = f.issue(id)
	if i.EscalationLevel != 2 || !i.SLABreached {
		t.Fatalf("L2 后: %+v", i)
	}
	ev = f.n.last()
	if !hasUser(ev, "zhangsan") || !hasUser(ev, "tl") || !hasUser(ev, "dba1") || hasUser(ev, "boss") || !strings.Contains(ev.Title, "升级至团队负责人：已超过解决时限 1小时0分") {
		t.Fatalf("L2 通知: %v %s", ev.ExtraUsers, ev.Title)
	}

	// 再超过 24 小时：L3，升级总监
	f.now = t0.Add(73 * time.Hour)
	if r := f.runSLA(); r.Actions["L3"] != 1 {
		t.Fatalf("应 L3: %+v", r)
	}
	if ev = f.n.last(); !hasUser(ev, "boss") || !strings.Contains(ev.Title, "升级至总监") {
		t.Fatalf("L3 通知: %v %s", ev.ExtraUsers, ev.Title)
	}
	if i = f.issue(id); i.EscalationLevel != 3 {
		t.Fatal("level 3")
	}
	// 待验证后暂停计时
	f.mustAct(f.dev(), id, ActionInput{Action: ActFix, Remark: "ok"})
	n = f.n.count()
	f.now = t0.Add(200 * time.Hour)
	if r := f.runSLA(); r.Checked != 0 || f.n.count() != n {
		t.Fatalf("待验证不计时: %+v", r)
	}
}

func TestSLAPreWarn(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	f.accept(id) // 承诺明天
	t0 := f.now
	f.n.reset()
	f.now = t0.Add(30 * time.Hour) // 距 48h 截止还有 18h，> 48h 的 20% (9.6h)
	if r := f.runSLA(); len(r.Actions) != 0 {
		t.Fatalf("%+v", r)
	}
	f.now = t0.Add(39 * time.Hour) // 剩 9h ≤ 9.6h
	if r := f.runSLA(); r.Actions["pre_warn"] != 1 {
		t.Fatalf("应即将超时提醒: %+v", r)
	}
	ev := f.n.last()
	if !strings.Contains(ev.Title, "即将超时") || ev.Name != notify.EventPending || !hasUser(ev, "zhangsan") {
		t.Fatalf("通知: %+v", ev)
	}
	if !f.issue(id).PreWarned {
		t.Fatal("pre_warned")
	}
	f.now = t0.Add(40 * time.Hour)
	if r := f.runSLA(); len(r.Actions) != 0 {
		t.Fatalf("只提醒一次: %+v", r)
	}
	// 延期通过后重新计算提醒
	f.mustAct(f.dba(), id, ActionInput{Action: ActSeverity, Severity: SevP1})
	if f.issue(id).PreWarned {
		t.Fatal("调整级别后应清掉提醒标记")
	}
}

func TestSLAQuietTime(t *testing.T) {
	f := newFx(t)
	// P2，解决时限 14 天；周六静默
	f.ingest(f.digest(sqlOrderByUser, 100, 120))
	f.repo.cfg[settings.KeyPerfWeekendSilent] = "true"
	f.now = time.Date(2026, 10, 24, 10, 0, 0, 0, time.Local) // 周六，已超解决时限
	f.n.reset()
	f.runSLA()
	if f.n.count() != 0 {
		t.Fatal("周末对 P2 静默")
	}
	if !f.issue(1).SLABreached {
		t.Fatal("静默期间也要记超时")
	}
	if f.issue(1).EscalationLevel != 0 {
		t.Fatal("静默期间不升级，等恢复后再升")
	}
	f.now = time.Date(2026, 10, 26, 10, 0, 0, 0, time.Local) // 周一
	if r := f.runSLA(); r.Actions["L2"] != 1 {
		t.Fatalf("周一应升级: %+v", r)
	}

	// 免打扰时段 22-9：P1 静默，P0 不受限
	f2 := newFx(t)
	f2.repo.cfg[settings.KeyPerfQuietHours] = "22-9"
	f2.newIssue()                       // P0
	f2.now = f2.now.Add(13 * time.Hour) // 23:00，已超受理时限
	if r := f2.runSLA(); r.Actions["L1"] != 1 {
		t.Fatalf("P0 不受免打扰限制: %+v", r)
	}
	f2.ingest(f2.digest("select * from t_b where a = 1", 100, 90000)) // 日均 90000 → P0？改成 P1 的量级
	_ = f2
	for _, c := range []struct {
		hour  int
		sev   string
		quiet bool
	}{{23, SevP1, true}, {3, SevP1, true}, {9, SevP1, false}, {12, SevP1, false}, {23, SevP0, false}} {
		tm := time.Date(2026, 10, 7, c.hour, 0, 0, 0, time.Local) // 周三
		if got := f2.svc.inQuietTime(tm, c.sev); got != c.quiet {
			t.Errorf("%d点 %s 静默=%v，期望 %v", c.hour, c.sev, got, c.quiet)
		}
	}
	f2.repo.cfg[settings.KeyPerfQuietHours] = "bad"
	if f2.svc.inQuietTime(time.Date(2026, 10, 7, 23, 0, 0, 0, time.Local), SevP1) {
		t.Fatal("配置非法时不静默")
	}
}

func TestSLAConflictSkipsAndDoesNotNotify(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	f.now = f.now.Add(5 * time.Hour)
	f.n.reset()
	f.repo.beforeUpdate = func(i uint) {
		f.repo.beforeUpdate = nil
		f.repo.issues[i].UpdateTime = f.repo.issues[i].UpdateTime.Add(time.Second) // 研发刚好同时操作了
	}
	r := f.runSLA()
	if r.Actions["conflict"] != 1 || f.n.count() != 0 {
		t.Fatalf("冲突时放弃本轮且不发通知: %+v n=%d", r, f.n.count())
	}
	if f.issue(id).RemindCount != 0 {
		t.Fatal("冲突的更新不应落库")
	}
	// 下一轮正常催办
	if r = f.runSLA(); r.Actions["L1"] != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestSLAPolicyFromDB(t *testing.T) {
	f := newFx(t)
	if p := f.svc.policy(SevP1); p.AcceptHours != 24 || p.ResolveHours != 168 || p.MaxExtendTimes != 1 {
		t.Fatalf("默认: %+v", p)
	}
	f.repo.sla = []SLAPolicy{{ID: 1, Severity: SevP1, AcceptHours: 1, ResolveHours: 2, L3AfterHours: 3, RemindIntervalHours: 1, MaxExtendTimes: 5}}
	if p := f.svc.policy(SevP1); p.AcceptHours != 1 || p.MaxExtendTimes != 5 {
		t.Fatalf("库里的优先: %+v", p)
	}
	if p := f.svc.policy(SevP0); p.AcceptHours != 4 {
		t.Fatalf("没配的级别用默认: %+v", p)
	}
}

func TestTargetLevelAndDeadlines(t *testing.T) {
	now := time.Date(2026, 10, 6, 10, 0, 0, 0, time.Local)
	i := &Issue{Status: StAccepted}
	p := SLAPolicy{AcceptHours: 4, ResolveHours: 48, L3AfterHours: 24}
	setDeadlines(i, p, now, false)
	if !i.ResolveDeadline.Equal(now.Add(48 * time.Hour)) {
		t.Fatal("setDeadlines")
	}
	if targetLevel(i, p, now.Add(5*time.Hour)) != 0 {
		t.Fatal("已受理的不看受理时限")
	}
	i.Status = StNew
	if targetLevel(i, p, now.Add(5*time.Hour)) != 1 {
		t.Fatal("level 1")
	}
	// 只收紧不放宽
	i.EscalationLevel, i.PreWarned = 2, true
	setDeadlines(i, SLAPolicy{AcceptHours: 24, ResolveHours: 168}, now, true)
	if !i.ResolveDeadline.Equal(now.Add(48*time.Hour)) || i.EscalationLevel != 2 || !i.PreWarned {
		t.Fatalf("放宽应被忽略且不清升级状态: %v", i.ResolveDeadline)
	}
	setDeadlines(i, SLAPolicy{AcceptHours: 1, ResolveHours: 2}, now, true)
	if !i.ResolveDeadline.Equal(now.Add(2 * time.Hour)) {
		t.Fatal("收紧")
	}
	if humanDelta(25*time.Hour+30*time.Minute) != "1天1小时" || humanDelta(90*time.Minute) != "1小时30分" || humanDelta(-90*time.Minute) != "-1小时30分" {
		t.Fatal("humanDelta")
	}
}
