// @author: kevinkelly
// @license: Apache Licence
// @file: navbadge.go
// @time: 2026/10/28

package privapply

import "dbtool/internal/audit"

// PendingReview 返回当前等待 p 审批的权限申请数，供侧栏“权限管理”角标使用。
// 只统计 p 现在就能审批的（与申请详情页的 CanReview 同一套规则）；没有审批权限的用户直接返回 0。
// 为了不拖慢页面，只检查最新的 pageSize*pages 条申请（足够覆盖正常积压），同一审批节点 × 资源组的判定结果会复用。
func (s *Service) PendingReview(p audit.Actor) (int, error) {
	f, err := s.applyScope(p)
	if err != nil {
		return 0, err
	}
	if f.Scope == ScopeOwn && !p.IsSuperuser {
		return 0, nil // 没有审批权限，只能看到自己的申请
	}
	type memoKey struct {
		node  string
		group uint
		self  bool
	}
	const pageSize, pages = 100, 3
	memo := map[memoKey]bool{}
	n := 0
	for pg := 0; pg < pages; pg++ {
		f.Offset, f.Limit = pg*pageSize, pageSize
		rows, total, err := s.repo.ListApplies(f)
		if err != nil {
			return n, err
		}
		for _, r := range rows {
			if r.Status != audit.StatusWaiting {
				continue
			}
			au, err := s.repo.AuditByWorkflow(audit.TypeQueryPrivilege, r.ApplyID, false)
			if err != nil {
				return n, err
			}
			if au == nil || au.CurrentStatus != audit.StatusWaiting {
				continue
			}
			k := memoKey{au.CurrentAudit, au.GroupID, au.CreateUser == p.Username}
			ok, seen := memo[k]
			if !seen {
				ok = audit.CanReview(s.repo, au, p)
				memo[k] = ok
			}
			if ok {
				n++
			}
		}
		if int64((pg+1)*pageSize) >= total {
			break
		}
	}
	return n, nil
}
