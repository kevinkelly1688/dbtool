// @author: kevinkelly
// @license: Apache Licence
// @file: navbadge.go
// @time: 2026/10/28

package sqlflow

import (
	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// PendingReview 返回当前等待 p 审批的工单数，供侧栏“SQL 上线”角标使用。
// 只统计 p 现在就能审批的（与工单详情页的 CanReview 同一套规则）；没有审批权限的用户直接返回 0，不查工单。
// 为了不拖慢页面，最多检查 limit 张待审工单（<=0 或超过 200 时取 200），同一审批节点 × 资源组的判定结果会复用。
func (s *Service) PendingReview(p audit.Actor, limit int) (int, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	pm, err := s.loadPerms(p)
	if err != nil {
		return 0, err
	}
	if !p.IsSuperuser && !pm.approve {
		return 0, nil
	}
	rows, _, err := s.List(p, ListQuery{Status: model.WfManReviewing, Limit: limit})
	if err != nil {
		return 0, err
	}
	type memoKey struct {
		node  string
		group uint
		self  bool
	}
	memo := map[memoKey]bool{}
	n := 0
	for _, r := range rows {
		au, err := s.repo.AuditByWorkflow(audit.TypeSQLReview, r.ID, false)
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
	return n, nil
}
