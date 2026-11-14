// @author: kevinkelly
// @license: Apache Licence
// @file: memrepo.go
// @time: 2026/10/28

// Package memrepo 是权限申请与审批所需数据访问的内存实现，带事务快照回滚。
// 仅用于单元测试和本地演示（cmd/demo），不在生产代码路径里使用。
package memrepo

import (
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/btnperm"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/privapply"
)

// Repo 把数据全部放在导出的字段里，测试可以直接构造和检查。
type Repo struct {
	Users     map[uint]model.User
	Groups    map[uint]*model.AuthGroup
	Members   map[uint]map[uint]bool // userID -> authGroupID
	RGs       []model.ResourceGroup
	UserRGs   map[uint][]uint // userID -> resourceGroupIDs
	Instances []model.Instance
	InstRGs   map[uint][]uint // instanceID -> resourceGroupIDs
	Perms     map[uint]map[string]bool
	Cfg       map[string]string
	Settings  map[[2]int]*model.WorkflowAuditSetting // [groupID, workflowType]

	Applies []*model.QueryPrivilegesApply
	Audits  []*model.WorkflowAudit
	Details []*model.WorkflowAuditDetail
	Logs    []*model.WorkflowLog
	Privs   []*model.QueryPrivilege

	FailInsertPrivs error // 注入故障：授权写入失败

	// 旁路扩展（privapply.ExtRepo）。ExtOn 为 false 时表示旁路表“没创建”：所有扩展功能降级。
	ExtOn        bool
	ApplyExtRows map[uint]privapply.ApplyExt
	PrivExtRows  map[uint]privapply.PrivExt
	AuditEntries []*model.AuditEntry
	// LifecycleAudits 是审批类动作的系统审计（audit.Lifecycle），与 AuditEntries（直接授予）分开。
	LifecycleAudits []*model.AuditEntry
	FailLifecycle   error // 注入故障：写审批审计失败
	UserDepts       map[string]string

	NotifyChannels []notify.Channel           // 机器人通知渠道（已解密形态）
	templates      map[string]notify.Template // 通知消息模板（键：消息键|范围），见 notify_template.go
	tplHist        []tplHistRow
	nextChannelID  uint
}

func New() *Repo {
	return &Repo{
		Users: map[uint]model.User{}, Groups: map[uint]*model.AuthGroup{}, Members: map[uint]map[uint]bool{},
		UserRGs: map[uint][]uint{}, InstRGs: map[uint][]uint{}, Perms: map[uint]map[string]bool{},
		Cfg: map[string]string{}, Settings: map[[2]int]*model.WorkflowAuditSetting{},
	}
}

var _ privapply.Repo = (*Repo)(nil)

type snapshot struct {
	applies  []model.QueryPrivilegesApply
	applyExt map[uint]privapply.ApplyExt
	privExt  map[uint]privapply.PrivExt
	audits   []model.WorkflowAudit
	details  int
	logs     int
	privs    []model.QueryPrivilege
}

func (r *Repo) snap() snapshot {
	s := snapshot{details: len(r.Details), logs: len(r.Logs)}
	s.applyExt, s.privExt = map[uint]privapply.ApplyExt{}, map[uint]privapply.PrivExt{}
	for k, v := range r.ApplyExtRows {
		s.applyExt[k] = v
	}
	for k, v := range r.PrivExtRows {
		s.privExt[k] = v
	}
	for _, a := range r.Applies {
		s.applies = append(s.applies, *a)
	}
	for _, a := range r.Audits {
		s.audits = append(s.audits, *a)
	}
	for _, p := range r.Privs {
		s.privs = append(s.privs, *p)
	}
	return s
}

func (r *Repo) restore(s snapshot) {
	for i := range r.Applies {
		if i < len(s.applies) {
			*r.Applies[i] = s.applies[i]
		}
	}
	r.Applies = r.Applies[:len(s.applies)]
	for i := range r.Audits {
		if i < len(s.audits) {
			*r.Audits[i] = s.audits[i]
		}
	}
	r.Audits = r.Audits[:len(s.audits)]
	r.Details, r.Logs = r.Details[:s.details], r.Logs[:s.logs]
	for i := range r.Privs {
		if i < len(s.privs) {
			*r.Privs[i] = s.privs[i]
		}
	}
	r.Privs = r.Privs[:len(s.privs)]
	r.ApplyExtRows, r.PrivExtRows = s.applyExt, s.privExt
}

func (r *Repo) InTx(fn func(privapply.Repo) error) error {
	s := r.snap()
	if err := fn(r); err != nil {
		r.restore(s)
		return err
	}
	return nil
}

// ---- audit.Repo ----

func (r *Repo) AuditSetting(g uint, t int) (*model.WorkflowAuditSetting, error) {
	return r.Settings[[2]int{int(g), t}], nil
}
func (r *Repo) AuthGroup(id uint) (*model.AuthGroup, error) { return r.Groups[id], nil }
func (r *Repo) UserInAuthGroup(u, g uint) (bool, error)     { return r.Members[u][g], nil }
func (r *Repo) AuthGroupUsers(names []string, rg uint) ([]model.User, error) {
	var out []model.User
	for uid, gs := range r.Members {
		in := false
		for _, rid := range r.UserRGs[uid] {
			in = in || rid == rg
		}
		if !in {
			continue
		}
		for gid := range gs {
			for _, n := range names {
				if r.Groups[gid] != nil && r.Groups[gid].Name == n {
					out = append(out, r.Users[uid])
				}
			}
		}
	}
	return out, nil
}
func (r *Repo) InsertAudit(a *model.WorkflowAudit) error {
	a.AuditID = uint(len(r.Audits) + 1)
	r.Audits = append(r.Audits, a)
	return nil
}
func (r *Repo) SaveAudit(*model.WorkflowAudit) error { return nil }
func (r *Repo) InsertDetail(d *model.WorkflowAuditDetail) error {
	r.Details = append(r.Details, d)
	return nil
}
func (r *Repo) InsertLog(l *model.WorkflowLog) error {
	l.ID = uint(len(r.Logs) + 1)
	r.Logs = append(r.Logs, l)
	return nil
}

// HasButton：Perms 里放按钮码；基础按钮（登记表里标了 Open）对所有人开放。
func (r *Repo) HasButton(u uint, c string) (bool, error) {
	return r.Perms[u][c] || btnperm.IsOpen(c), nil
}
func (r *Repo) ConfigValue(k string) (string, bool) { v, ok := r.Cfg[k]; return v, ok }

// ---- privapply.Repo ----

func (r *Repo) ListInstances() ([]model.Instance, error) { return r.Instances, nil }
func (r *Repo) UserInstanceIDs(u uint) (map[uint]bool, error) {
	out := map[uint]bool{}
	for _, rid := range r.UserRGs[u] {
		for iid, rgs := range r.InstRGs {
			for _, x := range rgs {
				if x == rid {
					out[iid] = true
				}
			}
		}
	}
	return out, nil
}
func (r *Repo) UserResourceGroups(u uint, all bool) ([]model.ResourceGroup, error) {
	var out []model.ResourceGroup
	for _, g := range r.RGs {
		if all {
			out = append(out, g)
			continue
		}
		for _, rid := range r.UserRGs[u] {
			if rid == g.GroupID {
				out = append(out, g)
			}
		}
	}
	return out, nil
}
func (r *Repo) ResourceGroupByName(n string) (*model.ResourceGroup, error) {
	for i := range r.RGs {
		if r.RGs[i].GroupName == n {
			return &r.RGs[i], nil
		}
	}
	return nil, nil
}
func (r *Repo) ResourceGroupByID(id uint) (*model.ResourceGroup, error) {
	for i := range r.RGs {
		if r.RGs[i].GroupID == id {
			return &r.RGs[i], nil
		}
	}
	return nil, nil
}

func startOfToday() time.Time {
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location())
}

func (r *Repo) ValidQueryPrivileges(user string, inst uint) ([]model.QueryPrivilege, error) {
	var out []model.QueryPrivilege
	for _, p := range r.Privs {
		if p.UserName == user && p.InstanceID == inst && p.IsDeleted == 0 && !p.ValidDate.Before(startOfToday()) {
			out = append(out, *p)
		}
	}
	return out, nil
}
func (r *Repo) InsertApply(a *model.QueryPrivilegesApply) error {
	a.ApplyID = uint(len(r.Applies) + 1)
	a.CreateTime = time.Now()
	r.Applies = append(r.Applies, a)
	return nil
}
func (r *Repo) SaveApply(a *model.QueryPrivilegesApply) error { return nil }
func (r *Repo) withInstance(a *model.QueryPrivilegesApply) *model.QueryPrivilegesApply {
	for i := range r.Instances {
		if r.Instances[i].ID == a.InstanceID {
			a.Instance = &r.Instances[i]
		}
	}
	return a
}
func (r *Repo) GetApply(id uint) (*model.QueryPrivilegesApply, error) {
	for _, a := range r.Applies {
		if a.ApplyID == id {
			return r.withInstance(a), nil
		}
	}
	return nil, nil
}
func (r *Repo) ListApplies(f privapply.ApplyFilter) ([]model.QueryPrivilegesApply, int64, error) {
	var out []model.QueryPrivilegesApply
	for i := len(r.Applies) - 1; i >= 0; i-- {
		a := r.Applies[i]
		switch f.Scope {
		case privapply.ScopeOwn:
			if a.UserName != f.UserName {
				continue
			}
		case privapply.ScopeGroups:
			ok := a.UserName == f.UserName
			for _, g := range f.GroupIDs {
				ok = ok || g == a.GroupID
			}
			if !ok {
				continue
			}
		}
		r.withInstance(a)
		if !f.MatchApply(a) {
			continue
		}
		out = append(out, *a)
	}
	total := int64(len(out))
	if f.Offset > 0 {
		if f.Offset >= len(out) {
			out = nil
		} else {
			out = out[f.Offset:]
		}
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, total, nil
}
func (r *Repo) WaitingApplies(user string, inst uint) ([]model.QueryPrivilegesApply, error) {
	var out []model.QueryPrivilegesApply
	for _, a := range r.Applies {
		if a.UserName == user && a.InstanceID == inst && a.Status == audit.StatusWaiting {
			out = append(out, *a)
		}
	}
	return out, nil
}
func (r *Repo) AuditByWorkflow(t int, id uint, _ bool) (*model.WorkflowAudit, error) {
	for _, a := range r.Audits {
		if a.WorkflowType == t && a.WorkflowID == id {
			return a, nil
		}
	}
	return nil, nil
}
func (r *Repo) AuditLogs(id uint) ([]model.WorkflowLog, error) {
	var out []model.WorkflowLog
	for _, l := range r.Logs {
		if l.AuditID == id {
			out = append(out, *l)
		}
	}
	return out, nil
}
func (r *Repo) InsertPrivileges(ps []model.QueryPrivilege) error {
	if r.FailInsertPrivs != nil {
		return r.FailInsertPrivs
	}
	for i := range ps { // 同 GORM：把生成的主键写回调用方的切片
		p := ps[i]
		p.PrivilegeID = uint(len(r.Privs) + 1)
		ps[i].PrivilegeID = p.PrivilegeID
		r.Privs = append(r.Privs, &p)
	}
	return nil
}
func (r *Repo) instanceName(id uint) string {
	for _, i := range r.Instances {
		if i.ID == id {
			return i.InstanceName
		}
	}
	return ""
}
func (r *Repo) ListPrivileges(f privapply.PrivFilter) ([]privapply.PrivRow, int64, error) {
	var out []privapply.PrivRow
	for i := len(r.Privs) - 1; i >= 0; i-- {
		p := r.Privs[i]
		if p.IsDeleted != 0 || !f.MatchPriv(p) {
			continue
		}
		switch f.Scope {
		case privapply.ScopeOwn:
			if p.UserName != f.UserName {
				continue
			}
		case privapply.ScopeGroups:
			ok := false
			for _, id := range f.InstanceIDs {
				ok = ok || id == p.InstanceID
			}
			if !ok {
				continue
			}
		}
		out = append(out, privapply.PrivRow{QueryPrivilege: *p, InstanceName: r.instanceName(p.InstanceID)})
	}
	total := int64(len(out))
	if f.Offset > 0 {
		if f.Offset >= len(out) {
			out = nil
		} else {
			out = out[f.Offset:]
		}
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, total, nil
}
func (r *Repo) GetPrivilege(id uint) (*model.QueryPrivilege, error) {
	for _, p := range r.Privs {
		if p.PrivilegeID == id {
			return p, nil
		}
	}
	return nil, nil
}
func (r *Repo) SavePrivilege(*model.QueryPrivilege) error { return nil }

// LockUser 内存实现里没有并发，无需加锁。
func (r *Repo) LockUser(string) error { return nil }
