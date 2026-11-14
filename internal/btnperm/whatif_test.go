// @author: kevinkelly
// @license: Apache Licence
// @file: whatif_test.go
// @time: 2026/10/28

package btnperm

import (
	"strings"
	"testing"
)

// WhatIfGroup：假设权限组的按钮换成 next，重算用户对某按钮的判定。
func TestWhatIfGroup(t *testing.T) {
	f := newFake()
	f.addUser(1, "alice", true, false) // 只在 TL 组，approve 来自权限组
	f.addUser(2, "bob", true, false)   // TL 组，另有早期的原权限码 sql_review（不再生效）
	f.addUser(3, "carl", true, false)  // TL + DBA 两个组都授予 approve
	f.addUser(4, "dan", true, false)   // TL 组，但被单独禁用 approve
	f.addGroup(10, "TL", 1, 2, 3, 4)
	f.addGroup(20, "DBA", 3)
	f.grant(10, "sqlworkflow.approve")
	f.grant(20, "sqlworkflow.approve")
	f.giveDjango(2, "sql_review")
	f.overrides[4] = []Override{{Code: "sqlworkflow.approve", Effect: EffectDeny, Reason: "x"}}
	r := newRes(f)
	const code = "sqlworkflow.approve"
	none := map[string]bool{}
	keep := map[string]bool{code: true}

	allowed := func(uid uint, groupName string, next map[string]bool) bool {
		t.Helper()
		return ok(r.WhatIfGroup(uid, groupName, next, code)).Allowed
	}
	// 现状：alice 有
	eq(t, "alice 现状", ok(r.HasButton(1, code)), true)
	// TL 去掉 approve → alice 失去；保留则仍有
	eq(t, "alice 去掉后", allowed(1, "TL", none), false)
	eq(t, "alice 保留", allowed(1, "TL", keep), true)
	// 原权限码不参与判定 → bob 去掉后同样失去
	eq(t, "bob 去掉后（原权限码不兜底）", allowed(2, "TL", none), false)
	// 另一个权限组也授予 → carl 去掉 TL 后仍有；两个组都去掉才失去（只改一个组的假设里另一个仍授予）
	eq(t, "carl 去掉 TL 后（DBA 仍授予）", allowed(3, "TL", none), true)
	eq(t, "carl 去掉 DBA 后（TL 仍授予）", allowed(3, "DBA", none), true)
	// 单独禁用优先
	eq(t, "dan 保留也被禁用", allowed(4, "TL", keep), false)
	// 假设新增授予：一个原本没有该按钮的组，加上后用户获得
	f.addGroup(30, "RD", 1)
	f.users[5] = &UserInfo{ID: 5, Username: "eve", Display: "eve", Active: true}
	f.members[5] = []uint{30}
	r.InvalidateAll()
	eq(t, "eve 现状", ok(r.HasButton(5, code)), false)
	eq(t, "eve 假设 RD 获得 approve", allowed(5, "RD", keep), true)
	// 不写库、不动缓存之外的状态
	if f.groupBtns[30][code] || ok(r.HasButton(5, code)) {
		t.Fatal("假设不应改变真实状态")
	}
	// 未登记的按钮 / 已停用用户
	eq(t, "未登记按钮", ok(r.WhatIfGroup(1, "TL", keep, "no.such")).Allowed, false)
	f.users[1].Active = false
	eq(t, "停用用户", allowed(1, "TL", keep), false)
}

// 钩子：保存 / 预检时调用，输出追加进 warnings，出错不影响保存。
func TestGroupSaveCheckHook(t *testing.T) {
	defer SetGroupSaveCheck(nil)
	f := newFake()
	f.addUser(1, "alice", true, false)
	f.addGroup(10, "TL", 1)
	f.grant(10, "sqlworkflow.view_own", "sqlworkflow.approve")
	r := newRes(f)
	svc := NewService(f, r)
	a := Actor{ID: 1, Username: "root"}

	var calls [][]string
	SetGroupSaveCheck(func(id uint, name string, next []string) []string {
		calls = append(calls, append([]string{name}, next...))
		return []string{"钩子警告：" + name}
	})
	// 没有变化：预检 / 保存都不调用钩子
	d, err := svc.GroupDiff(10, []string{"sqlworkflow.view_own", "sqlworkflow.approve"})
	if err != nil || len(calls) != 0 {
		t.Fatalf("无变化不应调用钩子: %v %v", err, calls)
	}
	for _, w := range d.Warnings {
		if strings.Contains(w, "钩子") {
			t.Fatal("无变化不应有钩子警告")
		}
	}
	// 有变化：预检返回钩子警告，且调用发生在写库之前
	d, err = svc.GroupDiff(10, []string{"sqlworkflow.view_own"})
	if err != nil || len(calls) != 1 || calls[0][0] != "TL" || len(calls[0]) != 2 {
		t.Fatalf("%v %v", err, calls)
	}
	if !strings.Contains(strings.Join(d.Warnings, "|"), "钩子警告：TL") {
		t.Fatalf("%v", d.Warnings)
	}
	// 保存：钩子在写库前看到的仍是旧状态（旧授予 2 个），结果里带钩子警告，数据确实已写入
	var seenBefore []string
	SetGroupSaveCheck(func(id uint, name string, next []string) []string {
		seenBefore, _ = f.GroupButtons(10)
		return []string{"保存警告"}
	})
	res, err := svc.SaveGroup(a, 10, []string{"sqlworkflow.view_own"}, "")
	if err != nil || !res.Changed {
		t.Fatalf("%v %+v", err, res)
	}
	if len(seenBefore) != 2 {
		t.Fatalf("钩子应在写库之前被调用: %v", seenBefore)
	}
	if !strings.Contains(strings.Join(res.Warnings, "|"), "保存警告") {
		t.Fatalf("%v", res.Warnings)
	}
	cur, _ := f.GroupButtons(10)
	if len(cur) != 1 {
		t.Fatalf("应已保存: %v", cur)
	}
	// 保存没有变化时不调用、不追加
	calls = nil
	SetGroupSaveCheck(func(uint, string, []string) []string { calls = append(calls, nil); return []string{"x"} })
	res, err = svc.SaveGroup(a, 10, []string{"sqlworkflow.view_own"}, "")
	if err != nil || res.Changed || len(calls) != 0 {
		t.Fatalf("%v %+v %v", err, res, calls)
	}
	// 钩子 panic：视为没有输出，保存照常
	SetGroupSaveCheck(func(uint, string, []string) []string { panic("boom") })
	res, err = svc.SaveGroup(a, 10, []string{"sqlworkflow.view_own", "sqlworkflow.approve"}, "")
	if err != nil || !res.Changed {
		t.Fatalf("钩子 panic 不应影响保存: %v %+v", err, res)
	}
	// 取消登记
	SetGroupSaveCheck(nil)
	if runGroupSaveCheck(10, "TL", nil) != nil {
		t.Fatal("取消后不应再调用")
	}
}
