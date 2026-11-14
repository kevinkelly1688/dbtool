// @author: kevinkelly
// @license: Apache Licence
// @file: http_test.go
// @time: 2026/10/28

package btnhttp

import (
	"strings"
	"sync"
	"testing"
	"time"

	"dbtool/internal/btnperm"
	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// memRepo 是内存假仓库（只实现 HTTP 测试用到的方法；其余方法调用会因嵌入的 nil 接口而 panic，提醒补实现）。
type memRepo struct {
	btnperm.Repo
	mu        sync.Mutex
	setting   btnperm.Setting
	users     map[uint]*btnperm.UserInfo
	groups    map[uint]*btnperm.GroupInfo
	members   map[uint][]uint
	groupBtns map[uint]map[string]bool
	overrides map[uint][]btnperm.Override
	audits    []model.AuditEntry
}

func newMem() *memRepo {
	return &memRepo{setting: btnperm.DefaultSetting(),
		users: map[uint]*btnperm.UserInfo{
			1: {ID: 1, Username: "root", Display: "管理员", Active: true, Superuser: true},
			2: {ID: 2, Username: "bob", Display: "鲍勃", Active: true},
		},
		groups:    map[uint]*btnperm.GroupInfo{10: {ID: 10, Name: "RD", Members: 1}},
		members:   map[uint][]uint{2: {10}},
		groupBtns: map[uint]map[string]bool{10: {}},
		overrides: map[uint][]btnperm.Override{},
	}
}

func (m *memRepo) InTx(fn func(btnperm.Repo) error) error { return fn(m) }
func (m *memRepo) Setting() (btnperm.Setting, error)      { return m.setting, nil }
func (m *memRepo) Profile(uid uint) (*btnperm.Profile, error) {
	u := m.users[uid]
	p := &btnperm.Profile{GroupSource: map[string][]string{}}
	if u == nil || !u.Active {
		return p, nil
	}
	p.Active, p.Super = true, u.Superuser
	for _, g := range m.members[uid] {
		for c := range m.groupBtns[g] {
			p.GroupSource[c] = append(p.GroupSource[c], m.groups[g].Name)
		}
	}
	p.Overrides = append(p.Overrides, m.overrides[uid]...)
	return p, nil
}
func (m *memRepo) GetUser(id uint) (*btnperm.UserInfo, error) {
	if u := m.users[id]; u != nil {
		c := *u
		return &c, nil
	}
	return nil, nil
}
func (m *memRepo) GetGroup(id uint) (*btnperm.GroupInfo, error) {
	if g := m.groups[id]; g != nil {
		c := *g
		return &c, nil
	}
	return nil, nil
}
func (m *memRepo) ListGroups() ([]btnperm.GroupInfo, error) {
	var out []btnperm.GroupInfo
	for _, g := range m.groups {
		out = append(out, *g)
	}
	return out, nil
}
func (m *memRepo) GroupButtons(id uint) ([]string, error) {
	return btnperm.SortedKeys(m.groupBtns[id]), nil
}
func (m *memRepo) AllGroupButtons() (map[uint][]string, error) {
	out := map[uint][]string{}
	for g, s := range m.groupBtns {
		out[g] = btnperm.SortedKeys(s)
	}
	return out, nil
}
func (m *memRepo) ChangeGroupButtons(id uint, add, remove []string) error {
	for _, c := range remove {
		delete(m.groupBtns[id], c)
	}
	for _, c := range add {
		m.groupBtns[id][c] = true
	}
	return nil
}
func (m *memRepo) UserOverrides(uid uint) ([]btnperm.Override, error) {
	return append([]btnperm.Override(nil), m.overrides[uid]...), nil
}
func (m *memRepo) UpsertOverrides(uid uint, list []btnperm.Override) error {
	for _, o := range list {
		kept := m.overrides[uid][:0:0]
		for _, x := range m.overrides[uid] {
			if x.Code != o.Code {
				kept = append(kept, x)
			}
		}
		m.overrides[uid] = append(kept, o)
	}
	return nil
}
func (m *memRepo) DeleteOverrides(uid uint, codes []string) error {
	del := map[string]bool{}
	for _, c := range codes {
		del[c] = true
	}
	var keep []btnperm.Override
	for _, o := range m.overrides[uid] {
		if !del[o.Code] {
			keep = append(keep, o)
		}
	}
	m.overrides[uid] = keep
	return nil
}
func (m *memRepo) AllUserDirectCodes() (map[uint][]string, error) { return map[uint][]string{}, nil }
func (m *memRepo) AllUserGroups() (map[uint][]string, error) {
	out := map[uint][]string{}
	for uid, gs := range m.members {
		for _, g := range gs {
			out[uid] = append(out[uid], m.groups[g].Name)
		}
	}
	return out, nil
}
func (m *memRepo) SaveSetting(key, value, by string, at time.Time) error {
	switch key {
	case "mode":
		m.setting.Mode = value
	case "deny_display":
		m.setting.DenyDisplay = value
	}
	return nil
}
func (m *memRepo) InsertAudit(e *model.AuditEntry) error { m.audits = append(m.audits, *e); return nil }

func setup(t *testing.T) (*servertest.Env, *memRepo, *btnperm.Resolver) {
	t.Helper()
	mem := newMem()
	res := btnperm.New(mem)
	svc := btnperm.NewService(mem, res)
	reg := Register(svc, res)
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){func(s *server.Server, rt server.Routes) {
		s.SetButtonChecker(res)
		reg(s, rt)
	}}})
	return env, mem, res
}

type jsonObj = map[string]any

func TestAuthAndRegistry(t *testing.T) {
	env, _, _ := setup(t)
	if w := env.Anonymous("GET", "/api/v1/settings/button-perms/registry"); w.Code != 401 {
		t.Fatalf("未登录: %d", w.Code)
	}
	if w := env.Anonymous("GET", "/api/v1/button-perms/me"); w.Code != 401 {
		t.Fatalf("未登录 me: %d", w.Code)
	}
	bob, root := env.Login("bob"), env.Login("root")
	for _, p := range []string{"/registry", "/groups/10", "/users/2", "/setting"} {
		if code, _ := bob.JSON("GET", "/api/v1/settings/button-perms"+p, nil, nil); code != 403 {
			t.Errorf("普通用户 GET %s = %d，应 403", p, code)
		}
	}
	if code, _ := bob.JSON("PUT", "/api/v1/settings/button-perms/groups/10", jsonObj{"codes": []string{}}, nil); code != 403 {
		t.Errorf("普通用户写: %d", code)
	}
	var reg struct {
		Modules     []btnperm.Module    `json:"modules"`
		Presets     map[string][]string `json:"presets"`
		PresetNames []string            `json:"preset_names"`
		Setting     btnperm.Setting     `json:"setting"`
		Total       int                 `json:"total"`
		TablesReady bool                `json:"tables_ready"`
	}
	root.GetJSON("/api/v1/settings/button-perms/registry", &reg)
	if len(reg.Modules) == 0 || len(reg.Presets["DBA"]) == 0 || reg.Total == 0 || reg.Setting.Mode != "compat" || len(reg.PresetNames) != 6 {
		t.Fatalf("registry = %+v", reg)
	}
	// 早期的“生效模式切换 / 严格模式预检 / 按组迁移”接口已经去掉
	for _, c := range []struct{ method, path string }{
		{"GET", "/setting/preflight"}, {"GET", "/migrate/groups/10"}, {"POST", "/migrate/groups/10/apply"},
	} {
		if code, _ := root.JSON(c.method, "/api/v1/settings/button-perms"+c.path, nil, nil); code != 404 {
			t.Errorf("%s %s 应已不存在: %d", c.method, c.path, code)
		}
	}
	// 登记表的 JSON 里不再带原权限码相关的字段
	body := root.Do("GET", "/api/v1/settings/button-perms/registry", nil).Body.String()
	for _, key := range []string{`"legacy"`, `"feeds"`, `"no_feed"`, `"menu"`} {
		if strings.Contains(body, key) {
			t.Errorf("registry 不应再输出 %s", key)
		}
	}
}

func TestGroupSaveDiffAndAudit(t *testing.T) {
	env, mem, res := setup(t)
	root, bob := env.Login("root"), env.Login("bob")

	var me struct {
		DenyDisplay string   `json:"deny_display"`
		Allowed     []string `json:"allowed"`
	}
	bob.GetJSON("/api/v1/button-perms/me", &me)
	if contains(me.Allowed, "sqlworkflow.approve") || contains(me.Allowed, "sqlworkflow.execute_group") || len(me.Allowed) != len(btnperm.BaselineButtons()) {
		t.Fatalf("权限组还没有授予任何按钮：bob 只有基础按钮: %+v", me)
	}

	var dr struct {
		Added, Removed []string
		Warnings       []string
	}
	if code, msg := root.JSON("POST", "/api/v1/settings/button-perms/groups/10/diff", jsonObj{"codes": []string{"sqlworkflow.execute_group"}}, &dr); code != 200 {
		t.Fatalf("diff: %d %s", code, msg)
	}
	if len(dr.Added) != 1 || len(dr.Warnings) != 1 || len(mem.audits) != 0 {
		t.Fatalf("diff 不写库: %+v audits=%d", dr, len(mem.audits))
	}
	var sr struct {
		Added   []string `json:"added"`
		Removed []string `json:"removed"`
		Changed bool     `json:"changed"`
	}
	root.PostJSON("/api/v1/settings/button-perms/groups/10/diff", jsonObj{"codes": []string{"sqlworkflow.view_own"}}, nil)
	if code, msg := root.JSON("PUT", "/api/v1/settings/button-perms/groups/10", jsonObj{"codes": []string{"sqlworkflow.view_own", "sqlworkflow.execute_group"}, "reason": "补权限"}, &sr); code != 200 || !sr.Changed || len(sr.Added) != 2 {
		t.Fatalf("保存: %d %s %+v", code, msg, sr)
	}
	if len(mem.audits) != 1 || mem.audits[0].UserName != "root" || !strings.Contains(mem.audits[0].ExtraInfo, "补权限") {
		t.Fatalf("审计: %+v", mem.audits)
	}
	// 缓存已失效：bob 立即拥有 execute_group
	bob.GetJSON("/api/v1/button-perms/me", &me)
	if !contains(me.Allowed, "sqlworkflow.execute_group") {
		t.Fatalf("保存后应立即生效: %+v", me)
	}
	var gv struct {
		Group btnperm.GroupInfo `json:"group"`
		Codes []string          `json:"codes"`
	}
	root.GetJSON("/api/v1/settings/button-perms/groups/10", &gv)
	if gv.Group.Name != "RD" || len(gv.Codes) != 2 {
		t.Fatalf("%+v", gv)
	}
	// 校验错误
	if code, _ := root.JSON("PUT", "/api/v1/settings/button-perms/groups/10", jsonObj{"codes": []string{"nope.x"}}, nil); code != 400 {
		t.Errorf("未知码: %d", code)
	}
	if code, _ := root.JSON("PUT", "/api/v1/settings/button-perms/groups/10", jsonObj{"codes": []string{"user.edit"}}, nil); code != 400 {
		t.Errorf("Locked 码: %d", code)
	}
	if code, _ := root.JSON("PUT", "/api/v1/settings/button-perms/groups/99", jsonObj{"codes": []string{}}, nil); code != 404 {
		t.Errorf("不存在的组: %d", code)
	}
	if code, _ := root.JSON("GET", "/api/v1/settings/button-perms/groups/abc", nil, nil); code != 400 {
		t.Errorf("非法 id: %d", code)
	}
	if code, _ := root.JSON("PUT", "/api/v1/settings/button-perms/groups/10", "not json", nil); code != 400 {
		t.Errorf("坏请求体: %d", code)
	}
	_ = res
}

func TestUserOverridesFlow(t *testing.T) {
	env, mem, _ := setup(t)
	mem.groupBtns[10]["sqlworkflow.approve"] = true // bob 所在的 RD 组授予了审批
	root, bob := env.Login("root"), env.Login("bob")
	base := "/api/v1/settings/button-perms/users/2"
	// 原因必填
	if code, _ := root.JSON("PUT", base+"/overrides", jsonObj{"effect": "deny", "codes": []string{"sqlworkflow.approve"}}, nil); code != 400 {
		t.Errorf("无原因: %d", code)
	}
	if code, _ := root.JSON("PUT", base+"/overrides", jsonObj{"effect": "deny", "codes": []string{"sqlworkflow.approve"}, "reason": "合规", "expire_at": "坏的"}, nil); code != 400 {
		t.Errorf("到期时间格式: %d", code)
	}
	if code, _ := root.JSON("PUT", base+"/overrides", jsonObj{"effect": "deny", "codes": []string{"sqlworkflow.approve"}, "reason": "合规", "expire_at": "2000-01-01"}, nil); code != 400 {
		t.Errorf("到期时间在过去: %d", code)
	}
	if code, msg := root.JSON("PUT", base+"/overrides", jsonObj{"effect": "deny", "codes": []string{"sqlworkflow.approve"}, "reason": "合规", "expire_at": "2099-12-31"}, nil); code != 200 {
		t.Fatalf("禁用: %d %s", code, msg)
	}
	var me struct {
		Allowed []string `json:"allowed"`
	}
	bob.GetJSON("/api/v1/button-perms/me", &me)
	if contains(me.Allowed, "sqlworkflow.approve") {
		t.Fatalf("被单独禁用的按钮不应在 me 里: %v", me.Allowed)
	}
	var uv struct {
		User      btnperm.UserInfo `json:"user"`
		Items     []btnperm.EffectiveItem
		Overrides []btnperm.OverrideView
	}
	root.GetJSON(base, &uv)
	var item btnperm.EffectiveItem
	for _, it := range uv.Items {
		if it.Code == "sqlworkflow.approve" {
			item = it
		}
	}
	if item.Allowed || item.Source != "deny" || len(uv.Overrides) != 1 || uv.Overrides[0].ExpireAt == nil || uv.Overrides[0].ExpireAt.Year() != 2099 {
		t.Fatalf("有效权限: %+v %+v", item, uv.Overrides)
	}
	if len(mem.audits) != 1 || mem.audits[0].Action != "单独禁用按钮" {
		t.Fatalf("审计: %+v", mem.audits)
	}
	// 撤销
	if code, _ := root.JSON("DELETE", base+"/overrides", jsonObj{"codes": []string{"sqlworkflow.approve"}}, nil); code != 400 {
		t.Errorf("撤销没有原因: %d", code)
	}
	if code, msg := root.JSON("DELETE", base+"/overrides", jsonObj{"codes": []string{"sqlworkflow.approve"}, "reason": "解除"}, nil); code != 200 {
		t.Fatalf("撤销: %d %s", code, msg)
	}
	bob.GetJSON("/api/v1/button-perms/me", &me)
	if !contains(me.Allowed, "sqlworkflow.approve") {
		t.Fatalf("撤销后回到权限组授予: %v", me.Allowed)
	}
	if code, _ := root.JSON("GET", "/api/v1/settings/button-perms/users/99", nil, nil); code != 404 {
		t.Errorf("不存在的用户: %d", code)
	}
	if code, _ := root.JSON("PUT", "/api/v1/settings/button-perms/users/1/overrides", jsonObj{"effect": "deny", "codes": []string{"perf.view"}, "reason": "x"}, nil); code != 400 {
		t.Errorf("超级用户不能单独禁用: %d", code)
	}
}

// 设置：只能改“无权限按钮的呈现方式”；迁移状态 mode 是 dbt-admin perm migrate 写的标记，接口不接受。
func TestSettingDenyDisplayOnly(t *testing.T) {
	env, mem, _ := setup(t)
	root, bob := env.Login("root"), env.Login("bob")
	base := "/api/v1/settings/button-perms"
	var set btnperm.Setting
	if code, msg := root.JSON("PUT", base+"/setting", jsonObj{"deny_display": "disable", "reason": "给研发看提示", "mode": "strict"}, &set); code != 200 || set.DenyDisplay != "disable" || set.Mode != "compat" {
		t.Fatalf("改呈现方式（带上 mode 也只改呈现方式）: %d %s %+v", code, msg, set)
	}
	if mem.setting.Mode != "compat" || mem.setting.DenyDisplay != "disable" {
		t.Fatalf("mode 不应被接口改掉: %+v", mem.setting)
	}
	var me struct {
		DenyDisplay string `json:"deny_display"`
	}
	bob.GetJSON("/api/v1/button-perms/me", &me)
	if me.DenyDisplay != "disable" {
		t.Fatalf("缓存应已失效: %+v", me)
	}
	if code, _ := root.JSON("PUT", base+"/setting", jsonObj{"deny_display": "x"}, nil); code != 400 {
		t.Errorf("非法呈现方式: %d", code)
	}
	if code, _ := bob.JSON("PUT", base+"/setting", jsonObj{"deny_display": "hide"}, nil); code != 403 {
		t.Errorf("普通用户不能改设置: %d", code)
	}
	var cur btnperm.Setting
	root.GetJSON(base+"/setting", &cur)
	if cur.DenyDisplay != "disable" || cur.Mode != "compat" {
		t.Fatalf("%+v", cur)
	}
}

// 管理接口每次请求都回库确认超级用户：会话里还是超级用户、库里已被降级的，被拒绝。
func TestDemotedAdminRejected(t *testing.T) {
	env, mem, _ := setup(t)
	root := env.Login("root")
	mem.users[1].Superuser = false
	if code, _ := root.JSON("GET", "/api/v1/settings/button-perms/registry", nil, nil); code != 403 {
		t.Fatalf("被降级的管理员应被拒绝: %d", code)
	}
	mem.users[1].Superuser, mem.users[1].Active = true, false
	if code, _ := root.JSON("GET", "/api/v1/settings/button-perms/registry", nil, nil); code != 403 {
		t.Fatalf("被停用的管理员应被拒绝: %d", code)
	}
}

func TestParseExpire(t *testing.T) {
	if got, err := parseExpire(""); err != nil || got != nil {
		t.Fatal("空表示长期")
	}
	got, err := parseExpire("2026-10-31")
	if err != nil || got.Hour() != 23 || got.Minute() != 59 || got.Day() != 31 {
		t.Fatalf("只给日期取当天结束: %v %v", got, err)
	}
	got, err = parseExpire("2026-10-31 18:30")
	if err != nil || got.Hour() != 18 || got.Minute() != 30 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := parseExpire("31/10/2026"); err == nil {
		t.Fatal("非法格式应报错")
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
