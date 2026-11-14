// @author: kevinkelly
// @license: Apache Licence
// @file: notify_e2e_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/sqlflow"
)

// 端到端：用工单流程里产生的真实事件，交给真实的通知器（Slack 渠道指向本地假 webhook），
// 看提交、通过、开始执行、执行完成、执行异常每个阶段是否都发出了消息。
type slackSink struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (s *slackSink) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		s.mu.Lock()
		s.bodies = append(s.bodies, m)
		s.mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
}

type e2eSrc struct{ ch []notify.Channel }

func (s e2eSrc) Channels(uint) ([]notify.Channel, error) { return s.ch, nil }

// e2eTpls 把每条 SQL 上线消息的“系统默认模板”当作全局模板（等价于管理员点了“定制”后直接保存）。
type e2eTpls struct{}

func (e2eTpls) ActiveTemplates() ([]notify.Template, error) {
	var out []notify.Template
	for _, m := range notify.Messages {
		if m.Family != notify.FamSQL {
			continue
		}
		d := m
		t := notify.DefaultTemplate(&d)
		t.Scope, t.Enabled = notify.ScopeGlobal, true
		out = append(out, t)
	}
	return out, nil
}

type e2eDir struct{}

func (e2eDir) UsersByName([]string) ([]notify.Person, error) { return nil, nil }
func (e2eDir) AuthGroupMembers(uint, uint) ([]notify.Person, error) {
	return nil, nil
}

func (s *slackSink) texts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, b := range s.bodies {
		var sb strings.Builder
		if t, ok := b["text"].(string); ok {
			sb.WriteString(t + "\n")
		}
		if bl, ok := b["blocks"].([]any); ok {
			for _, x := range bl {
				if m, ok := x.(map[string]any); ok {
					if t, ok := m["text"].(map[string]any); ok {
						if s, ok := t["text"].(string); ok {
							sb.WriteString(s + "\n")
						}
					}
				}
			}
		}
		out = append(out, sb.String())
	}
	return out
}

func TestNotifyEveryPhaseIsDeliveredToSlack(t *testing.T) {
	for _, withTpl := range []bool{false, true} {
		name := "系统默认文案"
		if withTpl {
			name = "套用系统默认模板"
		}
		t.Run(name, func(t *testing.T) { runDeliveryCase(t, withTpl) })
	}
}

func runDeliveryCase(t *testing.T, withTpl bool) {
	e := setup(t)
	var sink slackSink
	srv := sink.server()
	defer srv.Close()
	// 订阅为空 = 全部事件；一个启用的 Slack 渠道
	disp := notify.NewDispatcher(e2eSrc{[]notify.Channel{{ID: 1, Name: "slack", Type: notify.TypeSlack, WebhookURL: srv.URL, Enabled: true}}}, e2eDir{})
	if withTpl {
		disp.Templates = e2eTpls{}
	}

	deliver := func() {
		e.cap.mu.Lock()
		evs := append([]notify.Event(nil), e.cap.msgs...)
		e.cap.msgs = nil
		e.cap.mu.Unlock()
		for _, ev := range evs {
			disp.Notify(context.Background(), ev)
		}
	}

	// 提交 → 审批通过 → 执行成功
	id := e.submit(t, dev, "update orders set a=1 where id=1;\ndelete from orders where id=2")
	deliver()
	e.approve(t, id)
	deliver()
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if w := e.repo.Workflows[id]; w.Status != model.WfFinish {
		t.Fatalf("应执行成功: %s", w.Status)
	}
	deliver()

	// 再来一张：执行中失败 → 执行有异常
	e.eng.failOn = "boom"
	id2 := e.submit(t, dev, "update a set x=1; update boom set x=1; update c set x=1")
	e.approve(t, id2)
	if _, err := e.svc.Execute(context.Background(), dev, id2, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if w := e.repo.Workflows[id2]; w.Status != model.WfException {
		t.Fatalf("应执行有异常: %s", w.Status)
	}
	deliver()

	got := sink.texts()
	joined := strings.Join(got, "\n====\n")
	// 系统默认文案叫“执行有异常”，系统默认模板叫“执行异常”
	except := "工单执行有异常"
	if withTpl {
		except = "工单执行异常"
	}
	for _, want := range []string{"新的工单申请", "工单审核通过", "工单开始执行", "工单已正常结束", except} {
		if !strings.Contains(joined, want) {
			t.Errorf("没有发出“%s”的消息\n全部消息：\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "执行汇总") || !strings.Contains(joined, "执行记录") {
		t.Errorf("执行结束的消息里应有执行汇总与执行记录：\n%s", joined)
	}
}
