// @author: kevinkelly
// @license: Apache Licence
// @file: fake_ext_test.go
// @time: 2026/10/28

package sysadmin

import (
	"strings"
	"sync"
	"time"
)

// fakeExtState 是 fakeRepo 的旁路表状态（ExtRepo 的内存实现）。放在包级 map 里按 *fakeRepo 取，避免改 fakeRepo 结构体。
// missing=true 表示旁路表还没有创建。
type fakeExtState struct {
	missing  bool
	rg       map[uint]ResGroupExt
	profiles map[uint]UserProfile
	descs    map[uint]string
	builtin  map[uint]bool // 内置权限组；builtinColMissing 表示表在但没有 builtin 列
	kinds    map[uint][]string
	loginIP  map[string]string
	open     map[uint][]OpenWorkflow
	refs     map[uint]ResGroupRefs
	chans    map[uint]map[string]int // 启用的通知渠道：资源组 ID（0 = 全局）→ 类型 → 个数
	purged   []uint

	builtinColMissing bool
}

var fakeExtStates sync.Map

func (f *fakeRepo) xs() *fakeExtState {
	if v, ok := fakeExtStates.Load(f); ok {
		return v.(*fakeExtState)
	}
	x := &fakeExtState{rg: map[uint]ResGroupExt{}, profiles: map[uint]UserProfile{}, descs: map[uint]string{}, builtin: map[uint]bool{}, kinds: map[uint][]string{},
		loginIP: map[string]string{}, open: map[uint][]OpenWorkflow{}, refs: map[uint]ResGroupRefs{}}
	v, _ := fakeExtStates.LoadOrStore(f, x)
	return v.(*fakeExtState)
}

var _ ExtRepo = (*fakeRepo)(nil)

func (f *fakeRepo) TableReady(string) bool { return !f.xs().missing }

func (f *fakeRepo) ResGroupExts(ids []uint) (map[uint]ResGroupExt, error) {
	out := map[uint]ResGroupExt{}
	x := f.xs()
	if x.missing {
		return out, nil
	}
	if len(ids) == 0 {
		for k, v := range x.rg {
			out[k] = v
		}
		return out, nil
	}
	for _, id := range ids {
		if v, ok := x.rg[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

func (f *fakeRepo) UpsertResGroupExt(id uint, e ResGroupExt, by string, at time.Time) error {
	if f.xs().missing {
		return errExtMissing
	}
	f.xs().rg[id] = e
	return nil
}

func (f *fakeRepo) UserProfiles(ids []uint) (map[uint]UserProfile, error) {
	out := map[uint]UserProfile{}
	x := f.xs()
	if x.missing {
		return out, nil
	}
	for _, id := range ids {
		if v, ok := x.profiles[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

func (f *fakeRepo) UpsertUserProfile(id uint, p UserProfile, by string, at time.Time) error {
	if f.xs().missing {
		return errExtMissing
	}
	f.xs().profiles[id] = p
	return nil
}

func (f *fakeRepo) SetMustChangePassword(id uint, v bool, by string, at time.Time) error {
	if f.xs().missing {
		return errExtMissing
	}
	p := f.xs().profiles[id]
	p.MustChangePassword = v
	f.xs().profiles[id] = p
	return nil
}

func (f *fakeRepo) UserIDsByDept(q string, limit int) ([]uint, error) {
	var out []uint
	for id, p := range f.xs().profiles {
		if q != "" && strings.Contains(strings.ToLower(p.Dept), strings.ToLower(q)) {
			out = append(out, id)
		}
	}
	return out, nil
}

func (f *fakeRepo) AuthGroupDescs(ids []uint) (map[uint]string, error) {
	out := map[uint]string{}
	x := f.xs()
	if x.missing {
		return out, nil
	}
	for k, v := range x.descs {
		out[k] = v
	}
	return out, nil
}

func (f *fakeRepo) UpsertAuthGroupDesc(id uint, desc, by string, at time.Time) error {
	if f.xs().missing {
		return errExtMissing
	}
	f.xs().descs[id] = desc
	return nil
}

func (f *fakeRepo) DeleteAuthGroupExt(id uint) error { delete(f.xs().descs, id); return nil }

func (f *fakeRepo) UserAuthKinds(ids []uint) (map[uint][]string, error) {
	out := map[uint][]string{}
	for _, id := range ids {
		if k := f.xs().kinds[id]; len(k) > 0 {
			out[id] = k
		}
	}
	return out, nil
}

func (f *fakeRepo) UserLastLoginIP(username string) (string, error) {
	return f.xs().loginIP[username], nil
}

func (f *fakeRepo) OpenWorkflows(groupID uint, limit int) ([]OpenWorkflow, error) {
	return f.xs().open[groupID], nil
}

func (f *fakeRepo) ResGroupRefs(groupID uint) (ResGroupRefs, error) { return f.xs().refs[groupID], nil }

func (f *fakeRepo) EnabledChannelTypes() (map[uint]map[string]int, error) {
	out := map[uint]map[string]int{}
	for g, m := range f.xs().chans {
		out[g] = map[string]int{}
		for k, v := range m {
			out[g][k] = v
		}
	}
	return out, nil
}

func (f *fakeRepo) PurgeResourceGroup(groupID uint) error {
	if f.xs().missing {
		return errExtMissing
	}
	for k := range f.settings {
		if uint(k[0]) == groupID {
			delete(f.settings, k)
		}
	}
	delete(f.rgs, groupID)
	delete(f.xs().rg, groupID)
	f.xs().purged = append(f.xs().purged, groupID)
	return nil
}

// groupEngine 是 setup() 默认装的按钮权限引擎：按夹具里权限组被授予的按钮（fakeRepo.grant）判定。
// 预览“假设改了权限组”在这里不用，一律 false。
type groupEngine struct{ f *fakeRepo }

func (e groupEngine) HasButton(uid uint, code string) (bool, error) {
	return e.f.hasButton(uid, code), nil
}

func (e groupEngine) WhatIfGroup(uint, string, map[string]bool, string) (bool, error) {
	return false, nil
}

// fakeEngine 是按钮权限引擎的内存实现：has 按 "用户ID/按钮码" 给结果，没有登记的一律 false；whatif 同理（key 同 has）。
type fakeEngine struct {
	has    map[string]bool
	whatif map[string]bool
}

func (e *fakeEngine) HasButton(uid uint, code string) (bool, error) {
	return e.has[utoa(uid)+"/"+code], nil
}

func (e *fakeEngine) WhatIfGroup(uid uint, groupName string, next map[string]bool, code string) (bool, error) {
	return e.whatif[utoa(uid)+"/"+code], nil
}

func (f *fakeRepo) AuthGroupBuiltins(ids []uint) (map[uint]bool, error) {
	out := map[uint]bool{}
	x := f.xs()
	if x.missing || x.builtinColMissing {
		return out, nil
	}
	for id, v := range x.builtin {
		if v {
			out[id] = true
		}
	}
	return out, nil
}
