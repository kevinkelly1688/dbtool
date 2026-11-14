// @author: kevinkelly
// @license: Apache Licence
// @file: fake_test.go
// @time: 2026/10/28

package btnperm

import (
	"sort"
	"sync"
	"time"

	"dbtool/internal/model"
)

// fakeRepo 是内存假仓库：同时实现 Source 与 Repo。InTx 失败时回滚（快照恢复）。
type fakeRepo struct {
	mu sync.Mutex

	setting    Setting
	settingErr error
	users      map[uint]*UserInfo
	groups     map[uint]*GroupInfo
	members    map[uint][]uint          // 用户 → 权限组
	groupBtns  map[uint]map[string]bool // 权限组 → 按钮
	overrides  map[uint][]Override      // 用户 → 单独授权 / 禁用
	django     map[uint]map[string]bool // 用户 → 直接持有的原权限码（只给迁移测试用）
	groupDj    map[uint]map[string]bool // 权限组 → 原权限码（只给迁移测试用）
	audits     []model.AuditEntry
	profileHit int // Profile 调用次数（验证缓存）
	failTx     error
}

func newFake() *fakeRepo {
	return &fakeRepo{setting: DefaultSetting(), users: map[uint]*UserInfo{}, groups: map[uint]*GroupInfo{}, members: map[uint][]uint{},
		groupBtns: map[uint]map[string]bool{}, overrides: map[uint][]Override{}, django: map[uint]map[string]bool{}, groupDj: map[uint]map[string]bool{}}
}

func (f *fakeRepo) addUser(id uint, name string, active, super bool) *UserInfo {
	u := &UserInfo{ID: id, Username: name, Display: name, Active: active, Superuser: super}
	f.users[id] = u
	return u
}

func (f *fakeRepo) addGroup(id uint, name string, members ...uint) {
	f.groups[id] = &GroupInfo{ID: id, Name: name}
	f.groupBtns[id] = map[string]bool{}
	f.groupDj[id] = map[string]bool{}
	for _, m := range members {
		f.members[m] = append(f.members[m], id)
	}
}

func (f *fakeRepo) grant(gid uint, codes ...string) {
	for _, c := range codes {
		f.groupBtns[gid][c] = true
	}
}

func (f *fakeRepo) giveDjango(uid uint, codes ...string) {
	if f.django[uid] == nil {
		f.django[uid] = map[string]bool{}
	}
	for _, c := range codes {
		f.django[uid][c] = true
	}
}

// ---- Source ----

func (f *fakeRepo) Setting() (Setting, error) { return f.setting, f.settingErr }

func (f *fakeRepo) Profile(uid uint) (*Profile, error) {
	f.profileHit++
	u := f.users[uid]
	p := &Profile{GroupSource: map[string][]string{}}
	if u == nil || !u.Active {
		return p, nil
	}
	p.Active, p.Super = true, u.Superuser
	for _, g := range f.members[uid] {
		for c := range f.groupBtns[g] {
			p.GroupSource[c] = append(p.GroupSource[c], f.groups[g].Name)
		}
	}
	p.Overrides = append([]Override(nil), f.overrides[uid]...)
	return p, nil
}

// ---- Repo ----

type snapshot struct {
	setting   Setting
	groupBtns map[uint]map[string]bool
	overrides map[uint][]Override
	audits    int
}

func (f *fakeRepo) snap() snapshot {
	s := snapshot{setting: f.setting, groupBtns: map[uint]map[string]bool{}, overrides: map[uint][]Override{}, audits: len(f.audits)}
	for g, m := range f.groupBtns {
		s.groupBtns[g] = map[string]bool{}
		for c := range m {
			s.groupBtns[g][c] = true
		}
	}
	for u, l := range f.overrides {
		s.overrides[u] = append([]Override(nil), l...)
	}
	return s
}

func (f *fakeRepo) restore(s snapshot) {
	f.setting, f.groupBtns, f.overrides, f.audits = s.setting, s.groupBtns, s.overrides, f.audits[:s.audits]
}

func (f *fakeRepo) InTx(fn func(Repo) error) error {
	s := f.snap()
	if err := fn(f); err != nil {
		f.restore(s)
		return err
	}
	if f.failTx != nil {
		f.restore(s)
		return f.failTx
	}
	return nil
}

func (f *fakeRepo) GetGroup(id uint) (*GroupInfo, error) {
	g := f.groups[id]
	if g == nil {
		return nil, nil
	}
	n := 0
	for _, gs := range f.members {
		for _, x := range gs {
			if x == id {
				n++
			}
		}
	}
	return &GroupInfo{ID: g.ID, Name: g.Name, Members: n}, nil
}

func (f *fakeRepo) ListGroups() ([]GroupInfo, error) {
	var out []GroupInfo
	for id := range f.groups {
		g, _ := f.GetGroup(id)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeRepo) GroupButtons(id uint) ([]string, error) {
	return SortedKeys(f.groupBtns[id]), nil
}

func (f *fakeRepo) AllGroupButtons() (map[uint][]string, error) {
	out := map[uint][]string{}
	for g, m := range f.groupBtns {
		out[g] = SortedKeys(m)
	}
	return out, nil
}

func (f *fakeRepo) ChangeGroupButtons(id uint, add, remove []string) error {
	for _, c := range remove {
		delete(f.groupBtns[id], c)
	}
	for _, c := range add {
		f.groupBtns[id][c] = true
	}
	return nil
}

func (f *fakeRepo) GroupDjangoCodes(id uint) ([]string, error) { return SortedKeys(f.groupDj[id]), nil }

func (f *fakeRepo) GetUser(id uint) (*UserInfo, error) {
	if u := f.users[id]; u != nil {
		c := *u
		return &c, nil
	}
	return nil, nil
}

func (f *fakeRepo) UserOverrides(uid uint) ([]Override, error) {
	return append([]Override(nil), f.overrides[uid]...), nil
}

func (f *fakeRepo) UpsertOverrides(uid uint, list []Override) error {
	for _, o := range list {
		replaced := false
		for i, x := range f.overrides[uid] {
			if x.Code == o.Code {
				f.overrides[uid][i], replaced = o, true
			}
		}
		if !replaced {
			f.overrides[uid] = append(f.overrides[uid], o)
		}
	}
	return nil
}

func (f *fakeRepo) DeleteOverrides(uid uint, codes []string) error {
	del := map[string]bool{}
	for _, c := range codes {
		del[c] = true
	}
	var keep []Override
	for _, o := range f.overrides[uid] {
		if !del[o.Code] {
			keep = append(keep, o)
		}
	}
	f.overrides[uid] = keep
	return nil
}

func (f *fakeRepo) AllUserDirectCodes() (map[uint][]string, error) {
	out := map[uint][]string{}
	for uid, m := range f.django {
		if len(m) > 0 {
			out[uid] = SortedKeys(m)
		}
	}
	return out, nil
}

func (f *fakeRepo) AllUserGroups() (map[uint][]string, error) {
	out := map[uint][]string{}
	for uid, gs := range f.members {
		for _, g := range gs {
			out[uid] = append(out[uid], f.groups[g].Name)
		}
		sort.Strings(out[uid])
	}
	return out, nil
}

func (f *fakeRepo) SaveSetting(key, value, by string, at time.Time) error {
	switch key {
	case "mode":
		f.setting.Mode = value
	case "deny_display":
		f.setting.DenyDisplay = value
	}
	return nil
}

func (f *fakeRepo) InsertAudit(e *model.AuditEntry) error {
	f.audits = append(f.audits, *e)
	return nil
}
