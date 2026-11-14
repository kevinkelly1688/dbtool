// @author: kevinkelly
// @license: Apache Licence
// @file: verify_test.go
// @time: 2026/10/28

package perf

import (
	"strings"
	"testing"
	"time"

	"dbtool/internal/notify"
	"dbtool/internal/settings"
)

// fixedIssue 造一张已标记修复的工单（修复前平均 4000ms、日均 4000s），返回编号。
func (f *fx) fixedIssue() uint {
	id := f.newIssue()
	f.accept(id)
	f.mustAct(f.dev(), id, ActionInput{Action: ActFix, Remark: "加索引已上线"})
	return id
}

func (f *fx) addDay(id uint, daysAfter int, cnt int64, total float64) {
	f.repo.AddStat(id, dateOf(f.issue(id).FixedAt.AddDate(0, 0, daysAfter)), StatDelta{ExecCount: cnt, TotalTime: total})
}

func TestVerifyPassWhenImproved(t *testing.T) {
	f := newFx(t)
	id := f.fixedIssue()
	f.now = f.now.AddDate(0, 0, 4)
	f.addDay(id, 1, 50, 50) // 修复后三天平均 1000ms ≤ 2000ms
	f.addDay(id, 2, 50, 50)
	f.addDay(id, 3, 0, 0)
	f.n.reset()
	r, err := f.svc.RunVerify()
	if err != nil || r.Pass != 1 || r.Fail != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	i := f.issue(id)
	if i.Status != StClosed || i.CloseReason != CloseVerified {
		t.Fatalf("%+v", i)
	}
	logs := f.repo.logs[len(f.repo.logs)-1]
	if logs.Operator != "system" || !strings.Contains(logs.Content, "自动验证通过：平均耗时 4000ms -> 1000ms") {
		t.Fatalf("日志: %+v", logs)
	}
	if ev := f.n.last(); ev.Name != notify.EventFinished {
		t.Fatalf("通知: %+v", ev)
	}
}

func TestVerifyFailWhenNotImproved(t *testing.T) {
	f := newFx(t)
	id := f.fixedIssue()
	f.now = f.now.AddDate(0, 0, 4)
	f.addDay(id, 1, 1000, 15000)
	f.addDay(id, 2, 1000, 15000)
	f.n.reset()
	r, _ := f.svc.RunVerify()
	if r.Fail != 1 {
		t.Fatalf("%+v", r)
	}
	i := f.issue(id)
	if i.Status != StAccepted || i.ReopenCount != 1 {
		t.Fatalf("应退回处理中: %+v", i)
	}
	ev := f.n.last()
	if !strings.Contains(ev.Title, "验证未通过") || !hasUser(ev, "zhangsan") || !strings.Contains(ev.Content, "效果不明显") || !strings.Contains(ev.Content, "要求下降≥50%") {
		t.Fatalf("通知: %s %s", ev.Title, ev.Content)
	}
	if got := f.repo.logActions(id); got[len(got)-1] != ActVerifyFail {
		t.Fatalf("日志: %v", got)
	}
}

func TestVerifyPassWhenGoneAndWaitsObservationDays(t *testing.T) {
	f := newFx(t)
	id := f.fixedIssue()
	f.now = f.now.AddDate(0, 0, 2) // 观察期 3 天未到
	if r, _ := f.svc.RunVerify(); r.Pass+r.Fail != 0 {
		t.Fatalf("观察期内不验证: %+v", r)
	}
	f.now = f.now.AddDate(0, 0, 1)
	r, _ := f.svc.RunVerify()
	if r.Pass != 1 {
		t.Fatalf("修复后没再出现应通过: %+v", r)
	}
	if l := f.repo.logs[len(f.repo.logs)-1]; !strings.Contains(l.Content, "未再出现") {
		t.Fatalf("%+v", l)
	}
	_ = id
}

func TestVerifyConfigurable(t *testing.T) {
	f := newFx(t)
	f.repo.cfg[settings.KeyPerfVerifyDays] = "1"
	f.repo.cfg[settings.KeyPerfVerifyImproveRatio] = "0.9"
	id := f.fixedIssue()
	f.now = f.now.AddDate(0, 0, 1)
	f.addDay(id, 1, 100, 100) // 平均 1000ms，下降 75% < 90%；日均 100s 下降 97.5% ≥ 90% → 日均达标也算通过
	if r, _ := f.svc.RunVerify(); r.Pass != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestVerifyConflictSkipped(t *testing.T) {
	f := newFx(t)
	id := f.fixedIssue()
	f.now = f.now.AddDate(0, 0, 4)
	f.repo.beforeUpdate = func(i uint) {
		f.repo.beforeUpdate = nil
		f.repo.issues[i].UpdateTime = f.repo.issues[i].UpdateTime.Add(time.Second)
	}
	if r, err := f.svc.RunVerify(); err != nil || r.Pass != 0 {
		t.Fatalf("冲突应放弃: %+v %v", r, err)
	}
	if f.issue(id).Status != StFixed {
		t.Fatal("不应改状态")
	}
	if r, _ := f.svc.RunVerify(); r.Pass != 1 {
		t.Fatalf("下一轮应通过: %+v", r)
	}
}

func TestAutoCloseAbsent(t *testing.T) {
	f := newFx(t)
	id := f.newIssue()
	f.ingest(f.digest("select * from t_manual where a = 1", 1000, 4000))
	manual := f.issue(2)
	f.repo.issues[2].Source = "manual"
	_ = manual
	old := f.now.AddDate(0, 0, -20)
	f.repo.issues[id].LastSeen = &old
	f.repo.issues[2].LastSeen = &old
	// 已修复待验证的不在自动关闭范围
	f.ingest(f.digest("select * from t_fixed where a = 1", 1000, 4000))
	f.accept(3)
	f.mustAct(f.dev(), 3, ActionInput{Action: ActFix, Remark: "ok"})
	f.repo.issues[3].LastSeen = &old
	f.n.reset()
	r, err := f.svc.RunVerify()
	if err != nil || r.AbsentClosed != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	i := f.issue(id)
	if i.Status != StClosed || i.CloseReason != CloseDisappeared {
		t.Fatalf("%+v", i)
	}
	if f.issue(2).Status == StClosed || f.issue(3).Status != StFixed {
		t.Fatal("手工建的与待验证的不应被自动关闭")
	}
	if f.n.count() != 0 {
		t.Fatal("自动关闭不发通知")
	}
	// 配置为 0 表示关闭该功能
	f.repo.issues[2].Source = "collector"
	f.repo.cfg[settings.KeyPerfAutoCloseAbsentDays] = "0"
	if r, _ = f.svc.RunVerify(); r.AbsentClosed != 0 {
		t.Fatal("配置为 0 时不自动关闭")
	}
	// 再次出现会自动重开（影响达到阈值）
	f.repo.cfg[settings.KeyPerfAutoCloseAbsentDays] = "14"
	f.ingest(f.digest(sqlOrderByUser, 1000, 4000))
	if f.issue(id).Status == StClosed {
		t.Fatal("再次出现应自动重开")
	}
}
