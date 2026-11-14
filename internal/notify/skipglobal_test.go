// @author: kevinkelly
// @license: Apache Licence
// @file: skipglobal_test.go
// @time: 2026/10/28

package notify

import (
	"context"
	"testing"
)

// Event.SkipGlobal：为 true 时不发到全局渠道（未关联资源组的渠道）；默认 false = 行为与以前完全一致。
func TestSkipGlobalChannels(t *testing.T) {
	var r recv
	srv := r.server("ok", 200)
	defer srv.Close()
	global := Channel{Name: "global", Type: TypeSlack, WebhookURL: srv.URL, Enabled: true}
	team := Channel{Name: "team", GroupID: 5, Type: TypeSlack, WebhookURL: srv.URL, Enabled: true}
	multi := Channel{Name: "multi", GroupIDs: []uint{5, 6}, Type: TypeSlack, WebhookURL: srv.URL, Enabled: true}
	legacy := Channel{Name: "legacy", GroupID: 5, Legacy: true, Type: TypeSlack, WebhookURL: srv.URL, Enabled: true}
	d := newDisp(global, team, multi, legacy)

	if !global.IsGlobal() || team.IsGlobal() || multi.IsGlobal() || legacy.IsGlobal() {
		t.Fatalf("全局判定: %v %v %v %v", global.IsGlobal(), team.IsGlobal(), multi.IsGlobal(), legacy.IsGlobal())
	}
	d.Notify(context.Background(), evPending)
	if len(r.bodies) != 4 {
		t.Errorf("默认：全局渠道照常收到: %d", len(r.bodies))
	}
	ev := evPending
	ev.SkipGlobal = true
	d.Notify(context.Background(), ev)
	if len(r.bodies) != 4+3 {
		t.Errorf("SkipGlobal：只少了全局渠道: %d", len(r.bodies)-4)
	}
}
