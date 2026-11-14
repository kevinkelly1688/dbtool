// @author: kevinkelly
// @license: Apache Licence
// @file: ext_fake_test.go
// @time: 2026/10/28

package instmgr

import (
	"errors"
	"sort"
	"strings"
	"time"

	"dbtool/internal/instext"
	"dbtool/internal/model"
)

// extFakeRepo 在 fakeRepo 之上实现 ExtRepo（旁路表）：每张表可以单独设为“没建”。
type extFakeRepo struct {
	*fakeRepo
	instReady, regReady, tplReady bool
	exts                          map[uint]instext.Row
	regs                          map[uint]map[string]AccountReg
	rec                           map[uint]string
	allHist                       []ParamHistoryRow
	envSets                       []string // "id=env"
	failExt                       bool     // InstanceExts 返回错误
}

func newExtEnv() (*env, *extFakeRepo) {
	e := newEnv()
	x := &extFakeRepo{fakeRepo: e.repo, instReady: true, regReady: true, tplReady: true,
		exts: map[uint]instext.Row{}, regs: map[uint]map[string]AccountReg{}, rec: map[uint]string{}}
	s := NewService(x, e.dialer)
	s.now = e.svc.now
	e.svc = s
	return e, x
}

func (r *extFakeRepo) InstanceExtReady() bool { return r.instReady }

func (r *extFakeRepo) InstanceExts(ids []uint) (map[uint]instext.Row, error) {
	if r.failExt {
		return nil, errors.New("ext broken")
	}
	out := map[uint]instext.Row{}
	for _, id := range ids {
		if x, ok := r.exts[id]; ok {
			out[id] = x
		}
	}
	return out, nil
}

func (r *extFakeRepo) SetInstanceDisabled(id uint, disabled bool, by string, at time.Time) error {
	x := r.exts[id]
	x.InstanceID, x.Disabled, x.UpdatedAt = id, disabled, at
	if disabled {
		x.DisabledBy, x.DisabledAt = by, &at
	}
	r.exts[id] = x
	return nil
}

func (r *extFakeRepo) SetInstanceEnv(id uint, env string, at time.Time) error {
	x := r.exts[id]
	x.InstanceID, x.Env, x.UpdatedAt = id, env, at
	r.exts[id] = x
	r.envSets = append(r.envSets, itoa(id)+"="+env)
	return nil
}

func (r *extFakeRepo) InstanceStats(scope map[uint]bool) (InstanceStats, error) {
	var st InstanceStats
	for id, in := range r.instances {
		if scope != nil && !scope[id] {
			continue
		}
		st.Total++
		if r.exts[id].Disabled {
			st.Disabled++
		} else if in.IsEnable {
			st.Collecting++
		}
	}
	return st, nil
}

// ListInstances 在基础实现上加上“状态 / 环境”筛选（与 store.extFilter 语义一致）。
func (r *extFakeRepo) ListInstances(f InstanceFilter, scope map[uint]bool) ([]model.Instance, int64, error) {
	all, _, err := r.fakeRepo.ListInstances(InstanceFilter{Role: f.Role, DBType: f.DBType, Tag: f.Tag, Enabled: f.Enabled, Q: f.Q, Limit: 1000}, scope)
	if err != nil {
		return nil, 0, err
	}
	var out []model.Instance
	for _, in := range all {
		x := r.exts[in.ID]
		switch f.State {
		case "disabled":
			if !x.Disabled {
				continue
			}
		case "enabled":
			if x.Disabled {
				continue
			}
		}
		if f.Env != "" && x.Env != f.Env {
			continue
		}
		out = append(out, in)
	}
	total := int64(len(out))
	if f.Offset >= len(out) {
		return nil, total, nil
	}
	end := f.Offset + f.Limit
	if end > len(out) {
		end = len(out)
	}
	return out[f.Offset:end], total, nil
}

func (r *extFakeRepo) AccountRegReady() bool { return r.regReady }

func (r *extFakeRepo) AccountRegs(id uint) (map[string]AccountReg, error) {
	out := map[string]AccountReg{}
	for k, v := range r.regs[id] {
		out[k] = v
	}
	return out, nil
}

func (r *extFakeRepo) SaveAccountReg(id uint, a AccountReg, by string, at time.Time) error {
	if r.regs[id] == nil {
		r.regs[id] = map[string]AccountReg{}
	}
	r.regs[id][regKey(a.User, a.Host)] = a
	return nil
}

func (r *extFakeRepo) DeleteAccountReg(id uint, user, host string) error {
	delete(r.regs[id], regKey(user, host))
	return nil
}

func (r *extFakeRepo) TemplateExtReady() bool { return r.tplReady }

func (r *extFakeRepo) TemplateRecommended() (map[uint]string, error) {
	out := map[uint]string{}
	for k, v := range r.rec {
		out[k] = v
	}
	return out, nil
}

func (r *extFakeRepo) SaveTemplateRecommended(id uint, value, by string, at time.Time) error {
	if value == "" {
		delete(r.rec, id)
		return nil
	}
	r.rec[id] = value
	return nil
}

func (r *extFakeRepo) ListAllParamHistory(scope map[uint]bool, q string, offset, limit int) ([]ParamHistoryRow, int64, error) {
	var out []ParamHistoryRow
	for _, h := range r.allHist {
		if scope != nil && !scope[h.InstanceID] {
			continue
		}
		if q != "" && !strings.Contains(h.VariableName, q) && !strings.Contains(h.InstanceName, q) {
			continue
		}
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	total := int64(len(out))
	if offset >= len(out) {
		return nil, total, nil
	}
	end := offset + limit
	if end > len(out) {
		end = len(out)
	}
	return out[offset:end], total, nil
}
