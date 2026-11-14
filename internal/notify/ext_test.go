// @author: kevinkelly
// @license: Apache Licence
// @file: ext_test.go
// @time: 2026/10/28

package notify

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func ptr(b bool) *bool { return &b }

// ---- 事件模型：旧订阅语义保持，细分事件按规则匹配 ----

func TestEventModelBackwardCompatible(t *testing.T) {
	overdue := Event{Kind: "perf_issue", Name: EventException, Specific: EventPerfOverdue}
	alert := Event{Name: EventException, Specific: EventInspectAlert}
	submitted := Event{Name: EventPending, Specific: EventSubmitted}
	plainPending := Event{Name: EventPending}

	legacy := func(events ...string) Channel { return Channel{Events: events} }
	v2 := func(events ...string) Channel { return Channel{Events: events, EventsV2: true} }

	cases := []struct {
		name string
		ch   Channel
		ev   Event
		want bool
	}{
		{"订阅为空=全部（旧）", legacy(), overdue, true},
		{"订阅为空=全部（新）", v2(), alert, true},
		{"旧渠道订阅 exception 仍收到性能超时", legacy(EventException), overdue, true},
		{"旧渠道订阅 exception 仍收到巡检告警", legacy(EventException), alert, true},
		{"旧渠道订阅 pending 仍收到新工单提交", legacy(EventPending), submitted, true},
		{"旧渠道没订阅 exception 不收性能超时", legacy(EventPassed), overdue, false},
		{"新渠道订阅 exception 不再收性能超时（独立事件）", v2(EventException), overdue, false},
		{"新渠道订阅 exception 不再收巡检告警", v2(EventException), alert, false},
		{"新渠道订阅 perf_overdue 收到", v2(EventPerfOverdue), overdue, true},
		{"新渠道订阅 perf_overdue 不收巡检告警", v2(EventPerfOverdue), alert, false},
		{"新渠道订阅 inspect_alert 收到", v2(EventInspectAlert), alert, true},
		{"新渠道订阅 pending 仍收到新工单提交（提交属于待处理）", v2(EventPending), submitted, true},
		{"新渠道只订阅 submitted 只收新提交", v2(EventSubmitted), submitted, true},
		{"新渠道只订阅 submitted 不收流转到下一级", v2(EventSubmitted), plainPending, false},
		{"普通事件按 Name", v2(EventPassed), Event{Name: EventPassed}, true},
	}
	for _, c := range cases {
		if got := c.ch.wants(c.ev); got != c.want {
			t.Errorf("%s: wants=%v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestDispatchHonoursSpecificEvents(t *testing.T) {
	var r recv
	srv := r.server("ok", 200)
	defer srv.Close()
	legacyExc := Channel{Name: "旧-exception", Type: TypeSlack, WebhookURL: srv.URL, Enabled: true, Events: []string{EventException}}
	newExc := Channel{Name: "新-exception", Type: TypeSlack, WebhookURL: srv.URL, Enabled: true, Events: []string{EventException}, EventsV2: true}
	newOverdue := Channel{Name: "新-overdue", Type: TypeSlack, WebhookURL: srv.URL, Enabled: true, Events: []string{EventPerfOverdue}, EventsV2: true}
	d := newDisp(legacyExc, newExc, newOverdue)
	d.Notify(context.Background(), Event{Kind: "perf_issue", Name: EventException, Specific: EventPerfOverdue, Title: "t", Content: "c"})
	if len(r.bodies) != 2 { // 旧-exception 与 新-overdue
		t.Fatalf("性能超时应发给旧-exception 与 新-overdue: %d", len(r.bodies))
	}
	r.bodies = nil
	d.Notify(context.Background(), Event{Name: EventException, Title: "t", Content: "c"}) // 真正的执行异常
	if len(r.bodies) != 2 {                                                               // 旧-exception 与 新-exception
		t.Fatalf("执行异常应发给两个订阅了 exception 的渠道: %d", len(r.bodies))
	}
}

func TestIsFirstAuditNode(t *testing.T) {
	cases := []struct {
		groups, cur string
		want        bool
	}{{"10,20", "10", true}, {"10,20", "20", false}, {"10", "10", true}, {"", "-1", false}, {" 10 , 20", "10", true}, {"10,20", "-1", false}}
	for _, c := range cases {
		if got := IsFirstAuditNode(c.groups, c.cur); got != c.want {
			t.Errorf("IsFirstAuditNode(%q,%q)=%v", c.groups, c.cur, got)
		}
	}
}

// ---- 工作时间、DBA 值班 ----

func TestInWorkHours(t *testing.T) {
	loc := time.FixedZone("x", 8*3600)
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, loc) } // 2026-10-05 是周一
	cases := []struct {
		t    time.Time
		want bool
	}{{at(5, 9, 0), true}, {at(5, 18, 59), true}, {at(5, 19, 0), false}, {at(5, 8, 59), false}, {at(9, 12, 0), true}, {at(10, 12, 0), false}, {at(11, 12, 0), false}}
	for _, c := range cases {
		if got := InWorkHours(c.t); got != c.want {
			t.Errorf("%v: %v", c.t, got)
		}
	}
}

func TestDutyAndWorkHoursRules(t *testing.T) {
	var r recv
	srv := r.server("ok", 200)
	defer srv.Close()
	ch := Channel{Type: TypeSlack, WebhookURL: srv.URL, Enabled: true, DutyUsers: []string{"alice"}, Rules: []Rule{
		{Target: TargetDBADuty},
		{Target: TargetUser, Value: "carol", WorkHours: true},
	}}
	d := newDisp(ch)
	ev := Event{Name: EventException, Title: "t", Content: "c", WorkTime: ptr(true)}
	d.Notify(context.Background(), ev)
	text := r.bodies[0]["text"].(string)
	if !strings.Contains(text, "<@U0ALICE1>") || !strings.Contains(text, "<@U0CAROL1>") {
		t.Errorf("工作时间：值班 alice 与限工作时间的 carol 都应 @: %s", text)
	}
	ev.WorkTime = ptr(false)
	d.Notify(context.Background(), ev)
	text = r.bodies[1]["text"].(string)
	if !strings.Contains(text, "<@U0ALICE1>") || strings.Contains(text, "CAROL") {
		t.Errorf("非工作时间：只剩不限时的值班 alice，消息照发: %s", text)
	}
	// 没有显式指定时取 Dispatcher.Now：2023-11-14 22:13 UTC 周二，换算到本地时区后按本地时间判断
	d.Now = func() time.Time { return time.Date(2026, 10, 10, 11, 0, 0, 0, time.Local) } // 周六
	ev.WorkTime = nil
	d.Notify(context.Background(), ev)
	if strings.Contains(r.bodies[2]["text"].(string), "CAROL") {
		t.Errorf("周六应视为非工作时间: %v", r.bodies[2])
	}
	// 没有值班人员：规则不生效，给出原因
	_, skips := d.resolveExplain(Channel{Rules: []Rule{{Target: TargetDBADuty}}}, ev)
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "没有配置 DBA 值班人员") {
		t.Errorf("%+v", skips)
	}
}

func TestResolveExplainReasons(t *testing.T) {
	d := newDisp()
	ch := Channel{Type: TypeSlack, EventsV2: true, Rules: []Rule{
		{Target: TargetApplicant, Events: []string{EventPassed}},
		{Target: TargetReviewers},
		{Target: TargetAuthGroup, Value: "99"},
		{Target: TargetUser, Value: "nobody"},
		{Target: TargetApplicant, WorkHours: true},
		{Target: "weird"},
	}}
	ev := Event{Name: EventPending, GroupID: 1, WorkTime: ptr(false)} // 没有 Applicant、没有审批级
	ments, skips := d.resolveExplain(ch, ev)
	if len(ments) != 0 || len(skips) != 6 {
		t.Fatalf("ments=%v skips=%+v", ments, skips)
	}
	want := []string{"只在「审批通过」时 @", "没有当前审批级", "没有匹配到可 @", "没有匹配到可 @", "非工作时间", "未知的 @ 目标类型"}
	for i, w := range want {
		if !strings.Contains(skips[i].Reason, w) {
			t.Errorf("#%d 原因应包含 %q: %q", i, w, skips[i].Reason)
		}
	}
}

// ---- 预览（不真发） ----

func TestPreviewDoesNotSend(t *testing.T) {
	var r recv
	srv := r.server("ok", 200)
	defer srv.Close()
	ch := Channel{Type: TypeSlack, WebhookURL: srv.URL, Secret: "s3cret", Enabled: true, EventsV2: true, Rules: []Rule{
		{Target: TargetApplicant},
		{Target: TargetReviewers},
		{Target: TargetUser, Value: "bob"}, // bob 没有 Slack ID：退化为文本
		{Target: TargetRaw, Value: "S0ARY4FQATB"},
		{Target: TargetAll, Events: []string{EventPassed}},
	}}
	d := newDisp()
	ev, err := SampleEvent(EventSubmitted, "alice", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := d.Preview(ch, ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.reqs) != 0 {
		t.Fatal("预览不应发送任何请求")
	}
	if pv.Event != EventSubmitted || !pv.Signed || pv.Platform != TypeSlack || len(pv.Payload) == 0 {
		t.Errorf("%+v", pv)
	}
	var body map[string]string
	if err := json.Unmarshal(pv.Payload, &body); err != nil || !strings.Contains(body["text"], "<@U0ALICE1>") || !strings.Contains(body["text"], "<!subteam^S0ARY4FQATB>") {
		t.Errorf("消息体应含 @ 与用户组: %v %v", body, err)
	}
	if strings.Contains(string(pv.Payload), "s3cret") || strings.Contains(string(pv.Payload), srv.URL) {
		t.Error("预览不应包含密钥与真实地址")
	}
	var textOnly, resolved int
	for _, m := range pv.Mentions {
		if m.Resolved {
			resolved++
		} else {
			textOnly++
			if !strings.Contains(m.Note, "没有此渠道的标识") {
				t.Errorf("退化为文本的 @ 应说明原因: %+v", m)
			}
		}
	}
	if resolved != 3 || textOnly != 1 {
		t.Errorf("mentions=%+v", pv.Mentions)
	}
	if len(pv.Skipped) != 1 || !strings.Contains(pv.Skipped[0].Rule, "所有人") || !strings.Contains(pv.Skipped[0].Reason, "审批通过") {
		t.Errorf("规则只在审批通过时 @ 所有人：当前事件应记为未 @: %+v", pv.Skipped)
	}
	for _, name := range []string{EventSubmitted, EventPending, EventPassed, EventRejected, EventAborted, EventScheduled, EventExecuting, EventFinished, EventException, EventPerfOverdue, EventInspectAlert} {
		e, err := SampleEvent(name, "alice", 1, 0)
		if err != nil || e.Title == "" || e.Content == "" || !e.Test {
			t.Errorf("SampleEvent(%s): %+v %v", name, e, err)
		}
		if _, err := d.Preview(Channel{Type: TypeDingTalk}, e); err != nil {
			t.Errorf("预览 %s: %v", name, err)
		}
	}
	if _, err := SampleEvent("nope", "a", 1, 0); err == nil {
		t.Error("未知事件应报错")
	}
	if !ValidEvent(EventInspectAlert) || ValidEvent("x") || EventLabel(EventPending) != "待处理" || EventLabel("x") != "x" {
		t.Error("ValidEvent / EventLabel")
	}
}

func TestEventsListHasEleven(t *testing.T) {
	if len(Events) != 11 {
		t.Errorf("事件应扩展到 11 项: %d", len(Events))
	}
	seen := map[string]bool{}
	for _, e := range Events {
		if seen[e.Name] {
			t.Errorf("事件重复: %s", e.Name)
		}
		seen[e.Name] = true
	}
}

// ---- 发送记录 ----

type memSink struct {
	mu   sync.Mutex
	logs []SendLog
}

func (m *memSink) RecordSend(l SendLog) { m.mu.Lock(); m.logs = append(m.logs, l); m.mu.Unlock() }

func TestDeliverRecordsResultWithoutLeakingURL(t *testing.T) {
	var r recv
	good := r.server("ok", 200)
	defer good.Close()
	bad := r.server("denied", 403)
	defer bad.Close()
	sink := &memSink{}
	d := newDisp()
	d.Sink = sink
	ev := Event{Kind: KindSQLWorkflow, Name: EventPending, Specific: EventSubmitted, Title: "t", Content: "c", Applicant: "alice", GroupID: 1}
	ok := Channel{ID: 7, Name: "好渠道", Type: TypeSlack, WebhookURL: good.URL + "/hook/TOKEN9", Rules: []Rule{{Target: TargetApplicant}}}
	ko := Channel{ID: 8, Name: "坏渠道", Type: TypeSlack, WebhookURL: bad.URL + "/hook/TOKEN9"}
	if err := d.SendTest(context.Background(), ok, ev); err != nil {
		t.Fatal(err)
	}
	if err := d.SendTest(context.Background(), ko, ev); err == nil {
		t.Fatal("坏渠道应失败")
	}
	// 没有 ID 的渠道（旧版 webhook、未保存的测试）不记录
	_ = d.SendTest(context.Background(), Channel{Name: "临时", Type: TypeSlack, WebhookURL: good.URL}, ev)
	if len(sink.logs) != 2 {
		t.Fatalf("应记录 2 条: %+v", sink.logs)
	}
	a, b := sink.logs[0], sink.logs[1]
	if !a.OK || a.ChannelID != 7 || a.Event != EventSubmitted || a.Mentions != 1 || a.ChannelName != "好渠道" || a.At.IsZero() {
		t.Errorf("%+v", a)
	}
	if b.OK || b.Error == "" || strings.Contains(b.Error, "TOKEN9") {
		t.Errorf("失败记录应有错误摘要且不含 token: %+v", b)
	}
	// 订阅过滤发生在发送之前：没订阅的渠道不会产生记录
	sink.logs = nil
	d2 := newDisp(Channel{ID: 1, Name: "x", Type: TypeSlack, WebhookURL: good.URL, Enabled: true, Events: []string{EventFinished}})
	d2.Sink = sink
	d2.Notify(context.Background(), ev)
	if len(sink.logs) != 0 {
		t.Errorf("未订阅不应记录: %+v", sink.logs)
	}
}

// Source 同时实现了 LogSink 时，NewDispatcher 自动取用。
type sinkSrc struct {
	fakeSrc
	memSink
}

func TestNewDispatcherPicksUpSink(t *testing.T) {
	s := &sinkSrc{}
	if d := NewDispatcher(s, fakeDir{}); d.Sink == nil {
		t.Error("Source 实现了 LogSink 时应自动取用")
	}
	if d := NewDispatcher(fakeSrc{}, fakeDir{}); d.Sink != nil {
		t.Error("没有实现时 Sink 为空")
	}
}
