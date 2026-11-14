// @author: kevinkelly
// @license: Apache Licence
// @file: engine.go
// @time: 2026/10/28

package sysadmin

import (
	"time"

	"dbtool/internal/btnperm"
)

// 审批流配置页的“可审”判定：按按钮权限（SQL 上线 sqlworkflow.approve、查询权限 qpriv.review，含单独授权 / 禁用；
// 数据归档没有审批按钮）计算，并识别账号已锁定。只用于配置页的预览 / 告警；实际审批时的判定在 internal/audit。

// buttonEngine 是按钮权限引擎。
type buttonEngine interface {
	// HasButton 判断用户（非超级用户）当前是否拥有按钮。
	HasButton(uid uint, code string) (bool, error)
	// WhatIfGroup 假设权限组 groupName 的按钮授权换成 next，判断用户是否拥有按钮（不写库）。
	WhatIfGroup(uid uint, groupName string, next map[string]bool, code string) (bool, error)
}

type resolverEngine struct{ r *btnperm.Resolver }

func (e resolverEngine) HasButton(uid uint, code string) (bool, error) {
	return e.r.HasButton(uid, code)
}

func (e resolverEngine) WhatIfGroup(uid uint, groupName string, next map[string]bool, code string) (bool, error) {
	d, err := e.r.WhatIfGroup(uid, groupName, next, code)
	return d.Allowed, err
}

// SetButtonEngine 替换按钮权限引擎（测试用）。
func (s *Service) SetButtonEngine(e buttonEngine) { s.btn = e }

// engine 返回按钮权限引擎；没有（测试 / 演示）时为 nil，此时需要审批按钮的类型一律按“不可审”处理。
func (s *Service) engine() buttonEngine {
	if s.btn != nil {
		return s.btn
	}
	if r := btnperm.Default(); r != nil {
		return resolverEngine{r}
	}
	return nil
}

type approverState struct {
	Can    bool // 有审批按钮
	Locked bool // 账号因登录失败已被临时锁定
}

func (a approverState) ok() bool { return a.Can && !a.Locked }

// approverStates 计算一级里每位审批人的状态。lockCount/lockWin 是登录锁定策略。
// 超级用户直接可审；没有审批按钮的工单类型（数据归档）该级权限组 + 资源组内的成员都可审；其余按按钮权限引擎判断。
func (s *Service) approverStates(ap []Approver, wt WorkflowTypeInfo, lockCount int, lockWin time.Duration) []approverState {
	eng := s.engine()
	now := s.now()
	out := make([]approverState, len(ap))
	for i, a := range ap {
		var st approverState
		if a.FailedCount >= lockCount && a.LastFailedAt != nil && a.LastFailedAt.Add(lockWin).After(now) {
			st.Locked = true
		}
		switch {
		case a.Super, wt.ReviewButton == "":
			st.Can = true
		case eng != nil:
			if ok, err := eng.HasButton(a.ID, wt.ReviewButton); err == nil {
				st.Can = ok
			}
		}
		out[i] = st
	}
	return out
}

// GroupSaveWarnings 是 btnperm 保存权限组按钮前的预检：假设该权限组的按钮换成 next，
// 找出会因此“无人可审”的审批流级别（某级原本有可审批人，保存后一个都不剩）。只提示，不阻止保存。
func (s *Service) GroupSaveWarnings(groupID uint, groupName string, next []string) []string {
	eng := s.engine()
	if eng == nil {
		return nil
	}
	refs, err := s.flowRefs(groupID)
	if err != nil || len(refs) == 0 {
		return nil
	}
	nextSet := make(map[string]bool, len(next))
	for _, c := range next {
		nextSet[c] = true
	}
	lockCount, lockWin := s.lockPolicy()
	now := s.now()
	cache := &approverCache{repo: s.repo, m: map[string][]Approver{}}
	var out []string
	for _, ref := range refs {
		wt, ok := typeInfo(ref.WorkflowType)
		if !ok || wt.ReviewButton == "" {
			continue
		}
		btn := wt.ReviewButton
		g, err := s.repo.GetResourceGroup(ref.GroupID)
		if err != nil || g == nil || g.IsDeleted != 0 {
			continue
		}
		ap, err := cache.get(groupID, ref.GroupID)
		if err != nil {
			continue
		}
		before, after := 0, 0
		for _, a := range ap {
			if a.FailedCount >= lockCount && a.LastFailedAt != nil && a.LastFailedAt.Add(lockWin).After(now) {
				continue // 已锁定的本来就不算可审
			}
			if a.Super {
				before++
				after++
				continue
			}
			cur, err1 := eng.HasButton(a.ID, btn)
			nxt, err2 := eng.WhatIfGroup(a.ID, groupName, nextSet, btn)
			if err1 != nil || err2 != nil {
				continue
			}
			if cur {
				before++
			}
			if nxt {
				after++
			}
		}
		if before > 0 && after == 0 {
			out = append(out, errf("审批流「%s · %s」第 %d 级「%s」：保存后该级将无人可审（现有 %d 位可审批人会失去按钮 %s），新工单会卡在这一级",
				g.GroupName, wt.Label, ref.Level, groupName, before, btn))
		}
	}
	return out
}
