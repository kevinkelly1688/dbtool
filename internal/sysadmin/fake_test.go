// @author: kevinkelly
// @license: Apache Licence
// @file: fake_test.go
// @time: 2026/10/28

package sysadmin

import (
	"errors"
	"sort"
	"strings"
	"time"

	"dbtool/internal/btnperm"
	"dbtool/internal/model"
)

// fakeRepo 是 Repo 的内存实现，InTx 失败时整体回滚。数据全部放在导出字段之外的普通 map 里，测试直接构造和检查。
type fakeRepo struct {
	users      map[uint]model.User
	authGroups map[uint]string
	groupBtns  map[uint]map[string]bool // 权限组 → 被授予的按钮码
	userGroups map[uint]map[uint]bool   // 用户 → 权限组
	userRGs    map[uint]map[uint]bool   // 用户 → 资源组
	rgs        map[uint]model.ResourceGroup
	instances  []InstanceRef
	rgInst     map[uint]map[uint]bool // 资源组 → 实例
	settings   map[[2]int]model.WorkflowAuditSetting
	audits     []model.AuditEntry
	cfg        map[string]string
	authCfg    map[string]string
	twofa      map[uint][]string
	queryLogs  []model.QueryLog
	workflows  []fakeWorkflow
	openWF     map[uint]int64
	pending    map[uint]int64

	nextUser, nextGroup, nextRG, nextSetting, nextAudit uint
	failAudit                                           error // 注入故障：写审计失败
	inTx                                                int
}

type fakeWorkflow struct {
	SQLWorkflowRow
	GroupID uint
}

func newFake() *fakeRepo {
	f := &fakeRepo{
		users: map[uint]model.User{}, authGroups: map[uint]string{}, groupBtns: map[uint]map[string]bool{}, userGroups: map[uint]map[uint]bool{},
		userRGs: map[uint]map[uint]bool{}, rgs: map[uint]model.ResourceGroup{}, rgInst: map[uint]map[uint]bool{},
		settings: map[[2]int]model.WorkflowAuditSetting{}, cfg: map[string]string{}, authCfg: map[string]string{}, twofa: map[uint][]string{}, openWF: map[uint]int64{},
		pending: map[uint]int64{}, nextUser: 100, nextGroup: 100, nextRG: 100, nextSetting: 1, nextAudit: 1,
	}
	return f
}

func (f *fakeRepo) addUser(id uint, name string, active, super bool) {
	f.users[id] = model.User{ID: id, Username: name, Display: name + "名", IsActive: active, IsSuperuser: super}
}

func (f *fakeRepo) link(m map[uint]map[uint]bool, owner, other uint) {
	if m[owner] == nil {
		m[owner] = map[uint]bool{}
	}
	m[owner][other] = true
}

// grant 给权限组授予按钮（权限组的按钮授权在按钮权限页里管理，见 internal/btnperm；这里只是夹具里的内存版本）。
func (f *fakeRepo) grant(group uint, codes ...string) {
	if f.groupBtns[group] == nil {
		f.groupBtns[group] = map[string]bool{}
	}
	for _, c := range codes {
		f.groupBtns[group][c] = true
	}
}

// hasButton 是夹具里的按钮判定：超级用户全部放行；其余人只有所在权限组被授予的按钮，加上登记表里“登录即可”的基础按钮。
func (f *fakeRepo) hasButton(uid uint, code string) bool {
	if f.users[uid].IsSuperuser {
		return true
	}
	if b, ok := btnperm.Lookup(code); ok && b.Open {
		return true
	}
	for g := range f.userGroups[uid] {
		if f.groupBtns[g][code] {
			return true
		}
	}
	return false
}

// ---- 事务：整体快照 ----

func cloneSet(m map[uint]map[uint]bool) map[uint]map[uint]bool {
	out := make(map[uint]map[uint]bool, len(m))
	for k, v := range m {
		c := make(map[uint]bool, len(v))
		for x, b := range v {
			c[x] = b
		}
		out[k] = c
	}
	return out
}

func (f *fakeRepo) clone() *fakeRepo {
	c := *f
	c.users = map[uint]model.User{}
	for k, v := range f.users {
		c.users[k] = v
	}
	c.authGroups = map[uint]string{}
	for k, v := range f.authGroups {
		c.authGroups[k] = v
	}
	c.userGroups, c.userRGs, c.rgInst = cloneSet(f.userGroups), cloneSet(f.userRGs), cloneSet(f.rgInst)
	c.groupBtns = make(map[uint]map[string]bool, len(f.groupBtns))
	for k, v := range f.groupBtns {
		m := make(map[string]bool, len(v))
		for x, b := range v {
			m[x] = b
		}
		c.groupBtns[k] = m
	}
	c.rgs = map[uint]model.ResourceGroup{}
	for k, v := range f.rgs {
		c.rgs[k] = v
	}
	c.settings = map[[2]int]model.WorkflowAuditSetting{}
	for k, v := range f.settings {
		c.settings[k] = v
	}
	c.audits = append([]model.AuditEntry(nil), f.audits...)
	c.twofa = map[uint][]string{}
	for k, v := range f.twofa {
		c.twofa[k] = append([]string(nil), v...)
	}
	return &c
}

func (f *fakeRepo) InTx(fn func(Repo) error) error {
	saved := f.clone()
	f.inTx++
	defer func() { f.inTx-- }()
	if err := fn(f); err != nil {
		inTx := f.inTx
		*f = *saved
		f.inTx = inTx
		return err
	}
	return nil
}

func (f *fakeRepo) ConfigValue(item string) (string, bool) { v, ok := f.cfg[item]; return v, ok }
func (f *fakeRepo) AuthSetting(k string) (string, bool)    { v, ok := f.authCfg[k]; return v, ok }

// ---- 审计 ----

func (f *fakeRepo) InsertAuditLog(e *model.AuditEntry) error {
	if f.failAudit != nil {
		return f.failAudit
	}
	e.ID = f.nextAudit
	f.nextAudit++
	f.audits = append(f.audits, *e)
	return nil
}

func (f *fakeRepo) ListAuditLogs(fl AuditLogFilter) ([]model.AuditEntry, int64, error) {
	var out []model.AuditEntry
	for _, e := range f.audits {
		if fl.Action != "" && e.Action != fl.Action {
			continue
		}
		if fl.User != "" && !strings.Contains(e.UserName, fl.User) {
			continue
		}
		if fl.Search != "" && !strings.Contains(e.UserName+e.UserDisplay+e.Action+e.ExtraInfo, fl.Search) {
			continue
		}
		if fl.Start != nil && e.ActionTime.Before(*fl.Start) {
			continue
		}
		if fl.End != nil && !e.ActionTime.Before(*fl.End) {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return page(out, fl.Offset, fl.Limit), int64(len(out)), nil
}

func page[T any](in []T, off, lim int) []T {
	if off >= len(in) {
		return nil
	}
	end := off + lim
	if end > len(in) {
		end = len(in)
	}
	return in[off:end]
}

func (f *fakeRepo) AuditActions() ([]string, error) {
	set := map[string]bool{}
	for _, e := range f.audits {
		set[e.Action] = true
	}
	var out []string
	for a := range set {
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeRepo) ListQueryLogs(fl QueryLogFilter) ([]model.QueryLog, int64, error) {
	var out []model.QueryLog
	for _, l := range f.queryLogs {
		if fl.User != "" && l.Username != fl.User {
			continue
		}
		if fl.Instance != "" && l.InstanceName != fl.Instance {
			continue
		}
		if fl.Search != "" && !strings.Contains(l.SQLLog+l.UserDisplay+l.Alias, fl.Search) {
			continue
		}
		if fl.Start != nil && l.CreateTime.Before(*fl.Start) {
			continue
		}
		if fl.End != nil && !l.CreateTime.Before(*fl.End) {
			continue
		}
		out = append(out, l)
	}
	return page(out, fl.Offset, fl.Limit), int64(len(out)), nil
}

func (f *fakeRepo) ListSQLWorkflows(fl SQLWorkflowFilter) ([]SQLWorkflowRow, int64, error) {
	var out []SQLWorkflowRow
	for _, w := range f.workflows {
		if fl.Status != "" && w.Status != fl.Status {
			continue
		}
		if fl.GroupID != 0 && w.GroupID != fl.GroupID {
			continue
		}
		if fl.Search != "" && !strings.Contains(w.Name+w.EngineerDisplay+w.Engineer, fl.Search) {
			continue
		}
		out = append(out, w.SQLWorkflowRow)
	}
	return page(out, fl.Offset, fl.Limit), int64(len(out)), nil
}

// ---- 用户 ----

func (f *fakeRepo) GetUser(id uint) (*model.User, error) {
	if u, ok := f.users[id]; ok {
		return &u, nil
	}
	return nil, nil
}

func (f *fakeRepo) GetUserByName(name string) (*model.User, error) {
	for _, u := range f.users {
		if strings.EqualFold(u.Username, name) {
			c := u
			return &c, nil
		}
	}
	return nil, nil
}

func (f *fakeRepo) ListUsers(fl UserFilter) ([]model.User, int64, error) {
	var out []model.User
	for _, u := range f.users {
		if fl.Search != "" && !strings.Contains(u.Username+u.Display+u.Email, fl.Search) {
			byDept := false
			for _, id := range fl.DeptUserIDs {
				byDept = byDept || id == u.ID
			}
			if !byDept {
				continue
			}
		}
		if fl.AuthGroupID != 0 && !f.userGroups[u.ID][fl.AuthGroupID] {
			continue
		}
		if fl.ResourceGroupID != 0 && !f.userRGs[u.ID][fl.ResourceGroupID] {
			continue
		}
		switch fl.Status {
		case StatusSuper:
			if !u.IsSuperuser {
				continue
			}
		case StatusActive:
			if !u.IsActive {
				continue
			}
		case StatusInactive:
			if u.IsActive {
				continue
			}
		case StatusLocked:
			if !(u.FailedLoginCount >= fl.LockCount && u.LastLoginFailedAt != nil && u.LastLoginFailedAt.After(fl.Now.Add(-fl.LockWindow))) {
				continue
			}
		case StatusNo2FA:
			if len(f.twofa[u.ID]) > 0 {
				continue
			}
		}
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return page(out, fl.Offset, fl.Limit), int64(len(out)), nil
}

func (f *fakeRepo) CreateUser(u *model.User) error {
	u.ID = f.nextUser
	f.nextUser++
	f.users[u.ID] = *u
	return nil
}

func (f *fakeRepo) UpdateUser(id uint, fields map[string]any) error {
	u := f.users[id]
	for k, v := range fields {
		switch k {
		case "display":
			u.Display = v.(string)
		case "email":
			u.Email = v.(string)
		case "ding_user_id":
			u.DingUserID = v.(string)
		case "slack_id":
			u.SlackID = v.(string)
		case "feishu_open_id":
			u.FeishuOpenID = v.(string)
		case "is_active":
			u.IsActive = v.(bool)
		case "is_superuser":
			u.IsSuperuser = v.(bool)
		case "is_staff":
			u.IsStaff = v.(bool)
		case "failed_login_count":
			u.FailedLoginCount = v.(int)
		case "password":
			u.Password = v.(string)
		default:
			panic("fake 不支持字段 " + k)
		}
	}
	f.users[id] = u
	return nil
}

func (f *fakeRepo) CountOtherSuperusers(ex uint) (int64, error) {
	var n int64
	for _, u := range f.users {
		if u.ID != ex && u.IsSuperuser && u.IsActive {
			n++
		}
	}
	return n, nil
}

func (f *fakeRepo) UserAuthGroups(ids []uint) (map[uint][]IDName, error) {
	out := map[uint][]IDName{}
	for _, id := range ids {
		for g := range f.userGroups[id] {
			out[id] = append(out[id], IDName{g, f.authGroups[g]})
		}
		sort.Slice(out[id], func(i, j int) bool { return out[id][i].Name < out[id][j].Name })
	}
	return out, nil
}

func (f *fakeRepo) UserResourceGroups(ids []uint) (map[uint][]IDName, error) {
	out := map[uint][]IDName{}
	for _, id := range ids {
		for g := range f.userRGs[id] {
			if rg := f.rgs[g]; rg.IsDeleted == 0 {
				out[id] = append(out[id], IDName{g, rg.GroupName})
			}
		}
		sort.Slice(out[id], func(i, j int) bool { return out[id][i].Name < out[id][j].Name })
	}
	return out, nil
}

func (f *fakeRepo) UserTwoFATypes(ids []uint) (map[uint][]string, error) {
	out := map[uint][]string{}
	for _, id := range ids {
		out[id] = f.twofa[id]
	}
	return out, nil
}

func (f *fakeRepo) SetUserAuthGroups(id uint, ids []uint) error {
	f.userGroups[id] = map[uint]bool{}
	for _, g := range ids {
		f.userGroups[id][g] = true
	}
	return nil
}

func (f *fakeRepo) SetUserResourceGroups(id uint, ids []uint) error {
	keep := map[uint]bool{}
	for g := range f.userRGs[id] {
		if f.rgs[g].IsDeleted != 0 { // 已停用的组保持不变
			keep[g] = true
		}
	}
	for _, g := range ids {
		keep[g] = true
	}
	f.userRGs[id] = keep
	return nil
}

func (f *fakeRepo) DeleteUserTwoFA(id uint) (int64, error) {
	n := int64(len(f.twofa[id]))
	delete(f.twofa, id)
	return n, nil
}

// ---- 权限组 ----

func (f *fakeRepo) ListAuthGroups() ([]AuthGroupRow, error) {
	var out []AuthGroupRow
	for id, name := range f.authGroups {
		members := 0
		for _, gs := range f.userGroups {
			if gs[id] {
				members++
			}
		}
		out = append(out, AuthGroupRow{ID: id, Name: name, Members: members})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeRepo) GetAuthGroup(id uint) (*model.AuthGroup, error) {
	if n, ok := f.authGroups[id]; ok {
		return &model.AuthGroup{ID: id, Name: n}, nil
	}
	return nil, nil
}

func (f *fakeRepo) AuthGroupByName(name string) (*model.AuthGroup, error) {
	for id, n := range f.authGroups {
		if strings.EqualFold(n, name) {
			return &model.AuthGroup{ID: id, Name: n}, nil
		}
	}
	return nil, nil
}

func (f *fakeRepo) CreateAuthGroup(g *model.AuthGroup) error {
	g.ID = f.nextGroup
	f.nextGroup++
	f.authGroups[g.ID] = g.Name
	return nil
}

func (f *fakeRepo) RenameAuthGroup(id uint, name string) error { f.authGroups[id] = name; return nil }

func (f *fakeRepo) DeleteAuthGroup(id uint) error {
	delete(f.authGroups, id)
	delete(f.groupBtns, id)
	for _, gs := range f.userGroups {
		delete(gs, id)
	}
	return nil
}

func (f *fakeRepo) AuthGroupMembers(id uint) ([]UserRef, error) {
	var out []UserRef
	for uid, gs := range f.userGroups {
		if gs[id] {
			u := f.users[uid]
			out = append(out, UserRef{u.ID, u.Username, u.Display, u.IsActive})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

func (f *fakeRepo) PendingAuditsUsingGroup(id uint) (int64, error) { return f.pending[id], nil }

// ---- 资源组 ----

func (f *fakeRepo) ListResourceGroups(fl ResGroupFilter) ([]model.ResourceGroup, error) {
	var out []model.ResourceGroup
	for _, g := range f.rgs {
		if g.IsDeleted != 0 && !fl.IncludeDeleted {
			continue
		}
		if fl.Search != "" && !strings.Contains(g.GroupName, fl.Search) {
			continue
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GroupID < out[j].GroupID })
	return out, nil
}

func (f *fakeRepo) GetResourceGroup(id uint) (*model.ResourceGroup, error) {
	if g, ok := f.rgs[id]; ok {
		return &g, nil
	}
	return nil, nil
}

func (f *fakeRepo) ResourceGroupByName(name string) (*model.ResourceGroup, error) {
	for _, g := range f.rgs {
		if g.GroupName == name {
			c := g
			return &c, nil
		}
	}
	return nil, nil
}

func (f *fakeRepo) CreateResourceGroup(g *model.ResourceGroup) error {
	g.GroupID = f.nextRG
	f.nextRG++
	f.rgs[g.GroupID] = *g
	return nil
}

func (f *fakeRepo) UpdateResourceGroup(id uint, fields map[string]any) error {
	g := f.rgs[id]
	for k, v := range fields {
		switch k {
		case "group_name":
			g.GroupName = v.(string)
		case "ding_webhook":
			g.DingWebhook = v.(string)
		case "feishu_webhook":
			g.FeishuWebhook = v.(string)
		case "slack_webhook":
			g.SlackWebhook = v.(string)
		case "is_deleted":
			g.IsDeleted = v.(int)
		case "sys_time":
			g.SysTime = v.(time.Time)
		default:
			panic("fake 不支持字段 " + k)
		}
	}
	f.rgs[id] = g
	return nil
}

func (f *fakeRepo) ResourceGroupCounts() (map[uint]int, map[uint]int, error) {
	u, i := map[uint]int{}, map[uint]int{}
	for _, gs := range f.userRGs {
		for g := range gs {
			u[g]++
		}
	}
	for g, is := range f.rgInst {
		i[g] = len(is)
	}
	return u, i, nil
}

func (f *fakeRepo) GroupUsers(id uint) ([]UserRef, error) {
	var out []UserRef
	for uid, gs := range f.userRGs {
		if gs[id] {
			u := f.users[uid]
			out = append(out, UserRef{u.ID, u.Username, u.Display, u.IsActive})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

func (f *fakeRepo) GroupInstances(id uint) ([]InstanceRef, error) {
	var out []InstanceRef
	for _, i := range f.instances {
		if f.rgInst[id][i.ID] {
			out = append(out, i)
		}
	}
	return out, nil
}

func (f *fakeRepo) AllUsers() ([]UserRef, error) {
	var out []UserRef
	for _, u := range f.users {
		out = append(out, UserRef{u.ID, u.Username, u.Display, u.IsActive})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

func (f *fakeRepo) AllInstances() ([]InstanceRef, error) { return f.instances, nil }

func (f *fakeRepo) AddGroupUsers(id uint, ids []uint) error {
	for _, u := range ids {
		f.link(f.userRGs, u, id)
	}
	return nil
}

func (f *fakeRepo) RemoveGroupUsers(id uint, ids []uint) error {
	for _, u := range ids {
		delete(f.userRGs[u], id)
	}
	return nil
}

func (f *fakeRepo) AddGroupInstances(id uint, ids []uint) error {
	for _, i := range ids {
		f.link(f.rgInst, id, i)
	}
	return nil
}

func (f *fakeRepo) RemoveGroupInstances(id uint, ids []uint) error {
	for _, i := range ids {
		delete(f.rgInst[id], i)
	}
	return nil
}

func (f *fakeRepo) OpenWorkflowCount(id uint) (int64, error) { return f.openWF[id], nil }

// ---- 审批流配置 ----

func (f *fakeRepo) ListAuditSettings() ([]model.WorkflowAuditSetting, error) {
	var out []model.WorkflowAuditSetting
	for _, s := range f.settings {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GroupID != out[j].GroupID {
			return out[i].GroupID < out[j].GroupID
		}
		return out[i].WorkflowType < out[j].WorkflowType
	})
	return out, nil
}

func (f *fakeRepo) GetAuditSetting(g uint, wt int) (*model.WorkflowAuditSetting, error) {
	if s, ok := f.settings[[2]int{int(g), wt}]; ok {
		return &s, nil
	}
	return nil, nil
}

func (f *fakeRepo) SaveAuditSetting(g uint, name string, wt int, groups string) error {
	k := [2]int{int(g), wt}
	s, ok := f.settings[k]
	if !ok {
		s = model.WorkflowAuditSetting{AuditSettingID: f.nextSetting, GroupID: g, GroupName: name, WorkflowType: wt}
		f.nextSetting++
	}
	s.AuditAuthGroups = groups
	f.settings[k] = s
	return nil
}

func (f *fakeRepo) DeleteAuditSetting(g uint, wt int) error {
	delete(f.settings, [2]int{int(g), wt})
	return nil
}

func (f *fakeRepo) RenameAuditSettingGroup(g uint, name string) error {
	for k, s := range f.settings {
		if s.GroupID == g {
			s.GroupName = name
			f.settings[k] = s
		}
	}
	return nil
}

func (f *fakeRepo) ReviewApprovers(authGroupID, rgID uint) ([]Approver, error) {
	var out []Approver
	for uid, u := range f.users {
		if u.IsActive && f.userRGs[uid][rgID] && f.userGroups[uid][authGroupID] {
			out = append(out, Approver{ID: uid, Username: u.Username, Display: u.Display,
				Super: u.IsSuperuser, FailedCount: u.FailedLoginCount, LastFailedAt: u.LastLoginFailedAt})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

var errBoom = errors.New("boom")
