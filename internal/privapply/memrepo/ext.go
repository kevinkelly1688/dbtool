// @author: kevinkelly
// @license: Apache Licence
// @file: ext.go
// @time: 2026/10/28

package memrepo

import (
	"sort"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/privapply"
)

var _ privapply.ExtRepo = (*Repo)(nil)

// ExtOn 为 false 时模拟“旁路表没创建”：就绪检查返回 false，其他读写返回空。

func (r *Repo) ApplyExtReady() bool { return r.ExtOn }
func (r *Repo) PrivExtReady() bool  { return r.ExtOn }

func (r *Repo) SaveApplyExt(e privapply.ApplyExt) error {
	if r.ApplyExtRows == nil {
		r.ApplyExtRows = map[uint]privapply.ApplyExt{}
	}
	r.ApplyExtRows[e.ApplyID] = e
	return nil
}

func (r *Repo) ApplyExts(ids []uint) (map[uint]privapply.ApplyExt, error) {
	out := map[uint]privapply.ApplyExt{}
	for _, id := range ids {
		if e, ok := r.ApplyExtRows[id]; ok {
			out[id] = e
		}
	}
	return out, nil
}

func (r *Repo) SavePrivExts(es []privapply.PrivExt) error {
	if r.PrivExtRows == nil {
		r.PrivExtRows = map[uint]privapply.PrivExt{}
	}
	for _, e := range es {
		r.PrivExtRows[e.PrivilegeID] = e
	}
	return nil
}

func (r *Repo) PrivExts(ids []uint) (map[uint]privapply.PrivExt, error) {
	out := map[uint]privapply.PrivExt{}
	for _, id := range ids {
		if e, ok := r.PrivExtRows[id]; ok {
			out[id] = e
		}
	}
	return out, nil
}

func (r *Repo) PrivExtsByApply(applyID uint) ([]privapply.PrivExt, error) {
	var out []privapply.PrivExt
	for _, e := range r.PrivExtRows {
		if e.ApplyID == applyID && e.Source == privapply.SourceApply {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PrivilegeID < out[j].PrivilegeID })
	return out, nil
}

// inScope 判断权限是否在范围内（与 ListPrivileges 一致）。
func inScope(f privapply.PrivFilter, p *model.QueryPrivilege) bool {
	switch f.Scope {
	case privapply.ScopeOwn:
		return p.UserName == f.UserName
	case privapply.ScopeGroups:
		for _, id := range f.InstanceIDs {
			if id == p.InstanceID {
				return true
			}
		}
		return false
	}
	return true
}

func (r *Repo) PrivStats(f privapply.PrivFilter, today time.Time) (privapply.PrivStats, error) {
	var st privapply.PrivStats
	users, insts, dbs := map[string]string{}, map[uint]bool{}, map[string]bool{}
	for _, p := range r.Privs {
		if p.IsDeleted != 0 || !inScope(f, p) {
			continue
		}
		switch privapply.PrivState(p.ValidDate, today) {
		case privapply.PrivExpired:
			st.Expired++
		case privapply.PrivSoon:
			st.Soon++
			st.Valid++
		default:
			st.Valid++
		}
		users[p.UserName] = p.UserDisplay
		insts[p.InstanceID] = true
		if f.FilterInst != 0 && p.InstanceID == f.FilterInst {
			dbs[p.DBName] = true
		}
	}
	st.Users, st.Insts = len(users), len(insts)
	for u, d := range users {
		st.UserOpts = append(st.UserOpts, privapply.UserOpt{UserName: u, Display: d})
	}
	sort.Slice(st.UserOpts, func(i, j int) bool { return st.UserOpts[i].UserName < st.UserOpts[j].UserName })
	for id := range insts {
		st.InstOpts = append(st.InstOpts, privapply.InstOpt{ID: id, Name: r.instanceName(id)})
	}
	sort.Slice(st.InstOpts, func(i, j int) bool { return st.InstOpts[i].Name < st.InstOpts[j].Name })
	for d := range dbs {
		st.DBOpts = append(st.DBOpts, d)
	}
	sort.Strings(st.DBOpts)
	return st, nil
}

func (r *Repo) UserByName(name string) (*model.User, error) {
	for _, u := range r.Users {
		if u.Username == name {
			u := u
			return &u, nil
		}
	}
	return nil, nil
}

func (r *Repo) GrantableUsers(groupIDs []uint, all bool) ([]model.User, error) {
	var out []model.User
	for id, u := range r.Users {
		if !u.IsActive {
			continue
		}
		if !all {
			in := false
			for _, rid := range r.UserRGs[id] {
				for _, g := range groupIDs {
					in = in || rid == g
				}
			}
			if !in {
				continue
			}
		}
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *Repo) UserDept(username string) string { return r.UserDepts[username] }

func (r *Repo) WriteLifecycleAudit(e *model.AuditEntry) error {
	if r.FailLifecycle != nil {
		return r.FailLifecycle
	}
	r.LifecycleAudits = append(r.LifecycleAudits, e)
	return nil
}

func (r *Repo) WriteAuditLog(e *model.AuditEntry) error {
	r.AuditEntries = append(r.AuditEntries, e)
	return nil
}

var _ privapply.GroupInstanceRepo = (*Repo)(nil)

// GroupInstanceIDs 返回资源组关联的实例（按 InstRGs）。
func (r *Repo) GroupInstanceIDs(groupID uint) (map[uint]bool, error) {
	out := map[uint]bool{}
	for iid, rgs := range r.InstRGs {
		for _, g := range rgs {
			if g == groupID {
				out[iid] = true
			}
		}
	}
	return out, nil
}
