// @author: kevinkelly
// @license: Apache Licence
// @file: resgroup_ext.go
// @time: 2026/10/28

package sysadmin

import (
	"strings"

	"dbtool/internal/audit"
	"dbtool/internal/query"
)

// ResGroupSummary 是资源组页顶部的 4 张 KPI。
type ResGroupSummary struct {
	Enabled        int `json:"enabled"`         // 启用中的资源组
	Disabled       int `json:"disabled"`        // 已停用
	NoInstance     int `json:"no_instance"`     // 启用中、但没有关联实例（用户进来什么也看不到）
	FlowIncomplete int `json:"flow_incomplete"` // 启用中、审批流不足 3 / 3 种类型
	NoBot          int `json:"no_bot"`          // 启用中、没有任何机器人：资源组上的旧版 Webhook 和“通知渠道”里钉钉 / 企业微信 / 飞书 / Lark / Slack / Telegram 渠道都没有
}

// ResGroupSummary 统计资源组 KPI。
func (s *Service) ResGroupSummary() (*ResGroupSummary, error) {
	groups, err := s.repo.ListResourceGroups(ResGroupFilter{IncludeDeleted: true})
	if err != nil {
		return nil, err
	}
	_, insts, err := s.repo.ResourceGroupCounts()
	if err != nil {
		return nil, err
	}
	settings, err := s.repo.ListAuditSettings()
	if err != nil {
		return nil, err
	}
	chans, err := s.ext().EnabledChannelTypes()
	if err != nil {
		return nil, err
	}
	configured := map[uint]int{}
	for _, st := range settings {
		if _, ok := typeInfo(st.WorkflowType); ok {
			configured[st.GroupID]++
		}
	}
	out := &ResGroupSummary{}
	for _, g := range groups {
		if g.IsDeleted != 0 {
			out.Disabled++
			continue
		}
		out.Enabled++
		if insts[g.GroupID] == 0 {
			out.NoInstance++
		}
		if configured[g.GroupID] < len(WorkflowTypes) {
			out.FlowIncomplete++
		}
		if g.DingWebhook == "" && g.FeishuWebhook == "" && g.SlackWebhook == "" {
			have := groupChannels(chans, g.GroupID)
			bot := false
			for _, typ := range botChannelTypes {
				bot = bot || have[typ] > 0
			}
			if !bot {
				out.NoBot++
			}
		}
	}
	return out, nil
}

// PurgeResourceGroup 彻底删除资源组：只允许已停用、且没有任何引用（关联用户 / 实例、历史工单、审批记录、权限申请、性能工单、通知渠道）的组。
// 必须输入组名确认；审批流配置与扩展字段随组一并删除；写审计。
func (s *Service) PurgeResourceGroup(a audit.Actor, id uint, confirmName string) error {
	return s.tx(a, "彻底删除资源组", func(r Repo) (string, error) {
		g, err := r.GetResourceGroup(id)
		if err != nil {
			return "", err
		}
		if g == nil {
			return "", notFound("资源组不存在")
		}
		if g.IsDeleted == 0 {
			return "", conflict("只有已停用的资源组才能彻底删除，请先停用")
		}
		if strings.TrimSpace(confirmName) != g.GroupName {
			return "", bad(errf("请完整输入资源组名称「%s」以确认彻底删除", g.GroupName))
		}
		refs, err := extOf(r).ResGroupRefs(id)
		if err != nil {
			return "", err
		}
		if blocking := refs.Blocking(); len(blocking) > 0 {
			e := query.NewError(409, "conflict", errf("资源组仍被引用，不能彻底删除：%s。有历史记录引用的资源组只能停用", strings.Join(blocking, "、")))
			e.Data = map[string]any{"refs": map[string]int64{
				"users": refs.Users, "instances": refs.Instances, "sql_workflows": refs.SQLWorkflows, "audit_flows": refs.AuditFlows,
				"priv_applies": refs.PrivApplies, "perf_issues": refs.PerfIssues, "notify_channels": refs.NotifyChannels}}
			return "", e
		}
		if err := extOf(r).PurgeResourceGroup(id); err != nil {
			return "", extMissingErr(err, "彻底删除")
		}
		return errf("资源组「%s」（#%d），同时删除 %d 条审批流配置", g.GroupName, id, refs.FlowSettings), nil // i18n-ignore 存库的审计说明
	})
}

// RelationPreview 是调整资源组关联对象前的预览。
type RelationPreview struct {
	Added     int      `json:"added"`
	Removed   int      `json:"removed"`
	NewBreaks []string `json:"new_breaks"` // 这次调整会让审批流新出现的“无人可审”级别
}

// PreviewRelationChange 预览调整关联对象的影响（不写库）：移除用户（或加入用户）后，本资源组的审批流是否出现新的断点。
// 只有调整用户才可能影响审批人；实例的调整不影响审批流。
func (s *Service) PreviewRelationChange(id uint, kind string, ch RelationChange) (*RelationPreview, error) {
	if kind != "users" && kind != "instances" {
		return nil, bad("未知的关联对象类型")
	}
	add, err := dedupIDs(ch.Add)
	if err != nil {
		return nil, err
	}
	remove, err := dedupIDs(ch.Remove)
	if err != nil {
		return nil, err
	}
	g, err := s.repo.GetResourceGroup(id)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, notFound("资源组不存在")
	}
	out := &RelationPreview{Added: len(add), Removed: len(remove), NewBreaks: []string{}}
	if kind != "users" || len(add)+len(remove) == 0 {
		return out, nil
	}
	names, err := s.authGroupNames()
	if err != nil {
		return nil, err
	}
	settings, err := s.repo.ListAuditSettings()
	if err != nil {
		return nil, err
	}
	removeSet := map[uint]bool{}
	for _, x := range remove {
		removeSet[x] = true
	}
	before := &approverCache{repo: s.repo, m: map[string][]Approver{}}
	after := &approverCache{repo: s.repo, m: map[string][]Approver{}, adjust: func(authGroupID uint, list []Approver) ([]Approver, error) {
		kept := make([]Approver, 0, len(list))
		have := map[uint]bool{}
		for _, a := range list {
			if !removeSet[a.ID] {
				kept = append(kept, a)
				have[a.ID] = true
			}
		}
		for _, uid := range add {
			if have[uid] {
				continue
			}
			u, err := s.repo.GetUser(uid)
			if err != nil {
				return nil, err
			}
			if u == nil || !u.IsActive {
				continue
			}
			ags, err := s.repo.UserAuthGroups([]uint{uid})
			if err != nil {
				return nil, err
			}
			inGroup := false
			for _, ag := range ags[uid] {
				if ag.ID == authGroupID {
					inGroup = true
				}
			}
			if !inGroup {
				continue
			}
			kept = append(kept, Approver{ID: u.ID, Username: u.Username, Display: u.Display,
				Super: u.IsSuperuser, FailedCount: u.FailedLoginCount, LastFailedAt: u.LastLoginFailedAt})
		}
		return kept, nil
	}}
	for _, st := range settings {
		if st.GroupID != id {
			continue
		}
		wt, ok := typeInfo(st.WorkflowType)
		if !ok {
			continue
		}
		b, err := s.buildFlow(before, IDName{g.GroupID, g.GroupName}, wt, st.AuditAuthGroups, true, names)
		if err != nil {
			return nil, err
		}
		a, err := s.buildFlow(after, IDName{g.GroupID, g.GroupName}, wt, st.AuditAuthGroups, true, names)
		if err != nil {
			return nil, err
		}
		for i, lv := range a.Levels {
			if lv.Warning != "" && i < len(b.Levels) && b.Levels[i].Warning == "" {
				out.NewBreaks = append(out.NewBreaks, errf("%s 第 %d 级「%s」将无人可审", wt.Label, lv.Level, lv.AuthGroupName))
			}
		}
	}
	return out, nil
}
