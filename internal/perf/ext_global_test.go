// @author: kevinkelly
// @license: Apache Licence
// @file: ext_global_test.go
// @time: 2026/10/28

package perf

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ---- 升级通知矩阵的“全局渠道”列 ----
//
// 现状（升级矩阵上线前后一直如此）：升级消息按团队资源组发送，同时也发到全局渠道（group_id=0）。
// 所以默认值 = 每一级都发全局渠道；EscTargets.NoGlobal 的零值就是“发”。

func TestEscalationGlobalDefaultsKeepLegacyBehavior(t *testing.T) {
	f := newFx(t)
	m := f.svc.escalationMatrix()
	for lv := 0; lv <= 3; lv++ {
		if m.targetsFor(lv).NoGlobal {
			t.Errorf("默认每一级都发全局渠道: level %d", lv)
		}
	}
	// 没有 global 字段的旧数据 / 旧客户端：缺省 = 发
	var old EscMatrix
	if err := json.Unmarshal([]byte(`{"L1":{"owner":true},"L2":{"owner":true,"tl":true},"L3":{"owner":true,"director":true}}`), &old); err != nil {
		t.Fatal(err)
	}
	if err := validateEscMatrix(old); err != nil || old["L2"].NoGlobal {
		t.Fatalf("旧格式: %+v %v", old, err)
	}
	if err := f.repo.SetSetting(settingEscMatrix, `{"L1":{"owner":true},"L2":{"owner":true,"tl":true},"L3":{"owner":true,"director":true}}`, "x"); err != nil {
		t.Fatal(err)
	}
	if f.svc.escalationMatrix().targetsFor(2).NoGlobal {
		t.Error("库里旧格式保存的矩阵不能因升级而停发全局渠道")
	}
	// JSON 往返：对外字段是正向的 global
	b, _ := json.Marshal(defaultEscMatrix())
	if !strings.Contains(string(b), `"L1":{"owner":true,"tl":false,"dba":false,"director":false,"global":true}`) {
		t.Errorf("默认矩阵 JSON: %s", b)
	}
	var back EscMatrix
	_ = json.Unmarshal([]byte(`{"L1":{"owner":true,"global":false},"L2":{"owner":true,"global":true},"L3":{"owner":true}}`), &back)
	if !back["L1"].NoGlobal || back["L2"].NoGlobal || back["L3"].NoGlobal {
		t.Errorf("global 字段解析: %+v", back)
	}
}

func TestEscalationGlobalOffSkipsGlobalChannels(t *testing.T) {
	f := newFx(t)
	f.newIssue()
	t0 := f.now

	// 默认：升级事件不带 SkipGlobal（全局渠道照常收到）
	f.n.reset()
	f.now = t0.Add(5 * time.Hour) // 超过受理时限 → L1
	f.runSLA()
	if ev := f.n.last(); ev.SkipGlobal {
		t.Fatalf("默认应发全局渠道: %+v", ev)
	}

	// L2 关闭全局渠道：只影响 L2
	custom := defaultEscMatrix()
	custom["L2"] = EscTargets{Owner: true, TL: true, DBA: true, NoGlobal: true}
	if err := f.svc.SaveEscalation(f.dba(), custom); err != nil {
		t.Fatal(err)
	}
	if a := f.repo.auditActions(); a[len(a)-1] != "性能工单配置-升级通知对象修改" {
		t.Fatalf("只改全局渠道也应算一次修改并写审计: %v", a)
	}
	f.n.reset()
	f.now = t0.Add(49 * time.Hour) // → L2
	f.runSLA()
	ev := f.n.last()
	if !ev.SkipGlobal || !hasUser(ev, "tl") {
		t.Fatalf("L2 不发全局渠道、通知对象不变: skip=%v users=%v", ev.SkipGlobal, ev.ExtraUsers)
	}
	f.now = t0.Add(80 * time.Hour) // → L3
	f.runSLA()
	if ev = f.n.last(); ev.SkipGlobal {
		t.Fatalf("L3 仍发全局渠道: %+v", ev)
	}

	// 审计文字里能看出来
	if txt := custom.text(); !strings.Contains(txt, "L2→负责人/团队TL/跟进DBA（不发全局渠道）") || strings.Contains(defaultEscMatrix().text(), "全局") {
		t.Errorf("text: %s | %s", txt, defaultEscMatrix().text())
	}
}

func TestEscalationGlobalInSLAOverview(t *testing.T) {
	f := newFx(t)
	var m EscMatrix
	if err := json.Unmarshal([]byte(`{"L1":{"owner":true,"global":false},"L2":{"owner":true},"L3":{"owner":true}}`), &m); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SaveEscalation(f.dba(), m); err != nil {
		t.Fatal(err)
	}
	o, err := f.svc.SLAOverview()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(o.Escalation)
	if !strings.Contains(string(b), `"L1":{"owner":true,"tl":false,"dba":false,"director":false,"global":false}`) || !strings.Contains(string(b), `"L2":{"owner":true,"tl":false,"dba":false,"director":false,"global":true}`) {
		t.Errorf("SLA 总览返回: %s", b)
	}
}
