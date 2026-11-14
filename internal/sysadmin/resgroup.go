// @author: kevinkelly
// @license: Apache Licence
// @file: resgroup.go
// @time: 2026/10/28

package sysadmin

import (
	"net/url"
	"strings"
	"unicode/utf8"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/query"
)

// ResGroupItem 是资源组列表的一行。Webhook 只给出“是否已配置”，不返回内容。
type ResGroupItem struct {
	ID              uint   `json:"id"`
	Name            string `json:"name"`
	Users           int    `json:"users"`
	Instances       int    `json:"instances"`
	FlowsConfigured int    `json:"flows_configured"`
	FlowsTotal      int    `json:"flows_total"`
	DingSet         bool   `json:"ding_set"` // 旧版 Webhook 是否已配置（资源组上的三个字段；不含“通知渠道”）
	FeishuSet       bool   `json:"feishu_set"`
	SlackSet        bool   `json:"slack_set"`
	// Channels 是对该资源组生效的启用中的通知渠道（“通知渠道”页配置，含全局渠道）：渠道类型 → 个数。没有就是空。
	Channels      map[string]int `json:"channels"`
	Deleted       bool           `json:"deleted"`
	Leader        string         `json:"leader"`         // 负责人用户名（旁路表 dbt_resource_group_ext）
	LeaderDisplay string         `json:"leader_display"` // 负责人姓名
	Remark        string         `json:"remark"`
}

// FlowBrief 是资源组上某类工单审批流的一句话描述。
type FlowBrief struct {
	Type       int    `json:"type"`
	Label      string `json:"label"`
	Configured bool   `json:"configured"`
	Text       string `json:"text"`
}

// ResGroupDetail 是编辑资源组时用的详情。
type ResGroupDetail struct {
	ResGroupItem
	Flows []FlowBrief `json:"flows"`
}

// ResGroupInput 是新建 / 编辑资源组的输入。Webhook 留空表示不修改，要清空用 Clear*。
type ResGroupInput struct {
	Name          string `json:"name"`
	DingWebhook   string `json:"ding_webhook"`
	FeishuWebhook string `json:"feishu_webhook"`
	SlackWebhook  string `json:"slack_webhook"`
	ClearDing     bool   `json:"clear_ding"`
	ClearFeishu   bool   `json:"clear_feishu"`
	ClearSlack    bool   `json:"clear_slack"`
	// Leader / Remark 为 nil 表示不修改；空串表示清空。
	Leader *string `json:"leader"`
	Remark *string `json:"remark"`
}

func resGroupItem(g model.ResourceGroup) ResGroupItem {
	return ResGroupItem{ID: g.GroupID, Name: g.GroupName, DingSet: g.DingWebhook != "", FeishuSet: g.FeishuWebhook != "",
		SlackSet: g.SlackWebhook != "", Deleted: g.IsDeleted != 0, FlowsTotal: len(WorkflowTypes)}
}

// groupChannels 返回对资源组 id 生效的启用渠道：该组自己的 + 全局的（all[0]），渠道类型 → 个数。
func groupChannels(all map[uint]map[string]int, id uint) map[string]int {
	out := map[string]int{}
	for _, k := range []uint{id, 0} {
		for typ, n := range all[k] {
			out[typ] += n
		}
	}
	return out
}

// botChannelTypes 是机器人类的渠道类型（邮件不算机器人）。
var botChannelTypes = []string{"dingtalk", "wecom", "feishu", "lark", "slack", "telegram"}

func validateGroupName(name string) (string, error) {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	if n == 0 || n > 100 {
		return "", bad("资源组名称需要 1 到 100 个字符")
	}
	return name, nil
}

func validateWebhook(label, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if len(v) > 255 {
		return "", bad(errf("%s Webhook 地址过长（最多 255 个字符）", label))
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", bad(errf("%s Webhook 地址必须是 http:// 或 https:// 开头的完整地址", label))
	}
	return v, nil
}

// ListResourceGroups 返回资源组列表（含关联用户数、实例数、审批流配置情况）。
func (s *Service) ListResourceGroups(search string, includeDeleted bool) ([]ResGroupItem, error) {
	// 搜索范围含负责人 / 备注（在旁路表里），所以先取全部再在内存里过滤（资源组数量很少）。
	groups, err := s.repo.ListResourceGroups(ResGroupFilter{IncludeDeleted: includeDeleted})
	if err != nil {
		return nil, err
	}
	exts, err := s.ext().ResGroupExts(nil)
	if err != nil {
		return nil, err
	}
	leaders := s.leaderNames(exts)
	if q := strings.ToLower(strings.TrimSpace(search)); q != "" {
		kept := groups[:0:0]
		for _, g := range groups {
			e := exts[g.GroupID]
			hay := strings.ToLower(g.GroupName + "\n" + e.Leader + "\n" + leaders[e.Leader] + "\n" + e.Remark)
			if strings.Contains(hay, q) {
				kept = append(kept, g)
			}
		}
		groups = kept
	}
	users, insts, err := s.repo.ResourceGroupCounts()
	if err != nil {
		return nil, err
	}
	settings, err := s.repo.ListAuditSettings()
	if err != nil {
		return nil, err
	}
	configured := map[uint]int{}
	for _, st := range settings {
		if _, ok := typeInfo(st.WorkflowType); ok {
			configured[st.GroupID]++
		}
	}
	chans, err := s.ext().EnabledChannelTypes()
	if err != nil {
		return nil, err
	}
	out := make([]ResGroupItem, 0, len(groups))
	for _, g := range groups {
		it := resGroupItem(g)
		it.Channels = groupChannels(chans, g.GroupID)
		it.Users, it.Instances, it.FlowsConfigured = users[g.GroupID], insts[g.GroupID], configured[g.GroupID]
		e := exts[g.GroupID]
		it.Leader, it.LeaderDisplay, it.Remark = e.Leader, leaders[e.Leader], e.Remark
		out = append(out, it)
	}
	return out, nil
}

// leaderNames 返回负责人用户名 → 姓名（只在有人设置了负责人时才查用户表）。
func (s *Service) leaderNames(exts map[uint]ResGroupExt) map[string]string {
	out := map[string]string{}
	need := false
	for _, e := range exts {
		if e.Leader != "" {
			need = true
			break
		}
	}
	if !need {
		return out
	}
	all, err := s.repo.AllUsers()
	if err != nil {
		return out
	}
	for _, u := range all {
		out[u.Username] = u.Display
	}
	return out
}

// GetResourceGroup 返回一个资源组的详情，含各类工单的审批流概览。
func (s *Service) GetResourceGroup(id uint) (*ResGroupDetail, error) {
	g, err := s.repo.GetResourceGroup(id)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, notFound("资源组不存在")
	}
	names, err := s.authGroupNames()
	if err != nil {
		return nil, err
	}
	settings, err := s.repo.ListAuditSettings()
	if err != nil {
		return nil, err
	}
	d := &ResGroupDetail{ResGroupItem: resGroupItem(*g)}
	for _, t := range WorkflowTypes {
		fb := FlowBrief{Type: t.Type, Label: t.Label}
		for _, st := range settings {
			if st.GroupID == id && st.WorkflowType == t.Type {
				fb.Configured = true
				ids, _ := parseFlowIDs(st.AuditAuthGroups)
				fb.Text = flowText(ids, names)
				d.FlowsConfigured++
			}
		}
		d.Flows = append(d.Flows, fb)
	}
	users, insts, err := s.repo.ResourceGroupCounts()
	if err != nil {
		return nil, err
	}
	d.Users, d.Instances = users[id], insts[id]
	chans, err := s.ext().EnabledChannelTypes()
	if err != nil {
		return nil, err
	}
	d.Channels = groupChannels(chans, id)
	exts, err := s.ext().ResGroupExts([]uint{id})
	if err != nil {
		return nil, err
	}
	e := exts[id]
	d.Leader, d.Remark = e.Leader, e.Remark
	d.LeaderDisplay = s.leaderNames(exts)[e.Leader]
	return d, nil
}

func webhookState(b bool) string {
	if b {
		return "已配置"
	}
	return "未配置"
}

// CreateResourceGroup 新建资源组。名称在库里是唯一的（含已停用的）。
func (s *Service) CreateResourceGroup(a audit.Actor, in ResGroupInput) (uint, error) {
	name, err := validateGroupName(in.Name)
	if err != nil {
		return 0, err
	}
	ding, err := validateWebhook("钉钉", in.DingWebhook)
	if err != nil {
		return 0, err
	}
	feishu, err := validateWebhook("飞书", in.FeishuWebhook)
	if err != nil {
		return 0, err
	}
	slack, err := validateWebhook("Slack", in.SlackWebhook)
	if err != nil {
		return 0, err
	}
	var want ResGroupExt
	if in.Leader != nil {
		want.Leader = strings.TrimSpace(*in.Leader)
	}
	if in.Remark != nil {
		want.Remark = strings.TrimSpace(*in.Remark)
	}
	if err := validateRGExt(want); err != nil {
		return 0, err
	}
	var id uint
	err = s.tx(a, "新建资源组", func(r Repo) (string, error) {
		if ex, err := r.ResourceGroupByName(name); err != nil {
			return "", err
		} else if ex != nil {
			if ex.IsDeleted != 0 {
				return "", conflict("已存在同名的已停用资源组，请先恢复它或换一个名称")
			}
			return "", conflict("已存在同名的资源组")
		}
		now := s.now()
		g := &model.ResourceGroup{GroupName: name, GroupParentID: 0, GroupSort: 1, GroupLevel: 1,
			DingWebhook: ding, FeishuWebhook: feishu, SlackWebhook: slack, CreateTime: now, SysTime: now}
		if err := r.CreateResourceGroup(g); err != nil {
			return "", err
		}
		id = g.GroupID
		extra := ""
		if want != (ResGroupExt{}) {
			if err := s.checkLeader(r, want.Leader); err != nil {
				return "", err
			}
			if err := extOf(r).UpsertResGroupExt(id, want, a.Username, now); err != nil {
				return "", extMissingErr(err, "设置负责人 / 备注")
			}
			extra = "；负责人 " + orDash(want.Leader) + "；备注 " + orDash(want.Remark) // i18n-ignore 存库的审计说明
		}
		return errf("资源组「%s」（#%d）；钉钉 %s、飞书 %s、Slack %s%s", name, id, webhookState(ding != ""), webhookState(feishu != ""), webhookState(slack != ""), extra), nil // i18n-ignore 存库的审计说明
	})
	return id, err
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func validateRGExt(e ResGroupExt) error {
	if utf8.RuneCountInString(e.Leader) > 30 {
		return bad("负责人用户名最多 30 个字符")
	}
	if utf8.RuneCountInString(e.Remark) > 100 {
		return bad("备注最多 100 个字符")
	}
	return nil
}

// checkLeader 确认负责人是存在且启用的用户（空表示不指定）。
func (s *Service) checkLeader(r Repo, username string) error {
	if username == "" {
		return nil
	}
	u, err := r.GetUserByName(username)
	if err != nil {
		return err
	}
	if u == nil || !u.IsActive {
		return bad(errf("负责人 %s 不存在或已停用", username))
	}
	return nil
}

// UpdateResourceGroup 修改资源组名称与通知群 webhook（webhook 留空表示不修改）。
func (s *Service) UpdateResourceGroup(a audit.Actor, id uint, in ResGroupInput) error {
	name, err := validateGroupName(in.Name)
	if err != nil {
		return err
	}
	ding, err := validateWebhook("钉钉", in.DingWebhook)
	if err != nil {
		return err
	}
	feishu, err := validateWebhook("飞书", in.FeishuWebhook)
	if err != nil {
		return err
	}
	slack, err := validateWebhook("Slack", in.SlackWebhook)
	if err != nil {
		return err
	}
	if in.Leader != nil {
		l := strings.TrimSpace(*in.Leader)
		in.Leader = &l
	}
	if in.Remark != nil {
		m := strings.TrimSpace(*in.Remark)
		in.Remark = &m
	}
	if err := validateRGExt(ResGroupExt{Leader: derefStr(in.Leader), Remark: derefStr(in.Remark)}); err != nil {
		return err
	}
	return s.tx(a, "修改资源组", func(r Repo) (string, error) {
		g, err := r.GetResourceGroup(id)
		if err != nil {
			return "", err
		}
		if g == nil {
			return "", notFound("资源组不存在")
		}
		fields := map[string]any{"sys_time": s.now()}
		var changes []string
		if name != g.GroupName {
			if ex, err := r.ResourceGroupByName(name); err != nil {
				return "", err
			} else if ex != nil && ex.GroupID != id {
				return "", conflict("已存在同名的资源组")
			}
			fields["group_name"] = name
			changes = append(changes, "名称："+g.GroupName+" → "+name) // i18n-ignore 存库的审计说明
		}
		hook := func(label, column, cur, val string, clear bool) {
			switch {
			case clear:
				if cur != "" {
					fields[column] = ""
					changes = append(changes, label+" Webhook：已清除") // i18n-ignore 存库的审计说明
				}
			case val != "":
				fields[column] = val
				changes = append(changes, label+" Webhook：已"+map[bool]string{true: "更新", false: "设置"}[cur != ""]) // i18n-ignore 存库的审计说明；不记录地址本身
			}
		}
		hook("钉钉", "ding_webhook", g.DingWebhook, ding, in.ClearDing)
		hook("飞书", "feishu_webhook", g.FeishuWebhook, feishu, in.ClearFeishu)
		hook("Slack", "slack_webhook", g.SlackWebhook, slack, in.ClearSlack)
		// 负责人 / 备注（旁路表）：只有传了字段且与现状不同才写
		var newExt *ResGroupExt
		if in.Leader != nil || in.Remark != nil {
			curExts, err := extOf(r).ResGroupExts([]uint{id})
			if err != nil {
				return "", err
			}
			cur := curExts[id]
			next := cur
			if in.Leader != nil {
				next.Leader = *in.Leader
			}
			if in.Remark != nil {
				next.Remark = *in.Remark
			}
			if next.Leader != cur.Leader {
				if err := s.checkLeader(r, next.Leader); err != nil {
					return "", err
				}
				changes = append(changes, "负责人："+orDash(cur.Leader)+" → "+orDash(next.Leader)) // i18n-ignore 存库的审计说明
			}
			if next.Remark != cur.Remark {
				changes = append(changes, "备注：已修改") // i18n-ignore 存库的审计说明
			}
			if next != cur {
				newExt = &next
			}
		}
		if len(changes) == 0 {
			return errf("资源组「%s」（#%d）：无变化", g.GroupName, id), nil // i18n-ignore 存库的审计说明
		}
		if len(fields) > 1 { // 除了 sys_time 还有别的字段
			if err := r.UpdateResourceGroup(id, fields); err != nil {
				return "", err
			}
		}
		if newExt != nil {
			if err := extOf(r).UpsertResGroupExt(id, *newExt, a.Username, s.now()); err != nil {
				return "", extMissingErr(err, "设置负责人 / 备注")
			}
		}
		if v, ok := fields["group_name"]; ok {
			if err := r.RenameAuditSettingGroup(id, v.(string)); err != nil {
				return "", err
			}
		}
		return errf("资源组「%s」（#%d）：%s", name, id, strings.Join(changes, "；")), nil // i18n-ignore 存库的审计说明
	})
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// DisableResourceGroup 停用（软删除）资源组。有进行中的工单时拒绝。
func (s *Service) DisableResourceGroup(a audit.Actor, id uint) error {
	return s.tx(a, "停用资源组", func(r Repo) (string, error) {
		g, err := r.GetResourceGroup(id)
		if err != nil {
			return "", err
		}
		if g == nil {
			return "", notFound("资源组不存在")
		}
		if g.IsDeleted != 0 {
			return "", conflict("该资源组已经停用")
		}
		n, err := r.OpenWorkflowCount(id)
		if err != nil {
			return "", err
		}
		if n > 0 {
			e := query.NewError(409, "conflict", errf("该资源组还有 %d 个进行中的工单（待审批或待执行），处理完再停用", n))
			// 附上工单明细（最多 50 条）供页面列出并跳转；明细查询失败不影响拒绝本身
			if list, derr := extOf(r).OpenWorkflows(id, 50); derr == nil && len(list) > 0 {
				e.Data = map[string]any{"open_workflows": list, "total": n}
			}
			return "", e
		}
		if err := r.UpdateResourceGroup(id, map[string]any{"is_deleted": 1, "sys_time": s.now()}); err != nil {
			return "", err
		}
		return errf("资源组「%s」（#%d）", g.GroupName, id), nil // i18n-ignore 存库的审计说明
	})
}

// RestoreResourceGroup 恢复已停用的资源组。
func (s *Service) RestoreResourceGroup(a audit.Actor, id uint) error {
	return s.tx(a, "恢复资源组", func(r Repo) (string, error) {
		g, err := r.GetResourceGroup(id)
		if err != nil {
			return "", err
		}
		if g == nil {
			return "", notFound("资源组不存在")
		}
		if g.IsDeleted == 0 {
			return "", conflict("该资源组没有被停用")
		}
		if err := r.UpdateResourceGroup(id, map[string]any{"is_deleted": 0, "sys_time": s.now()}); err != nil {
			return "", err
		}
		return errf("资源组「%s」（#%d）", g.GroupName, id), nil // i18n-ignore 存库的审计说明
	})
}

// RelationSide 是穿梭框一侧的对象。
type RelationUsers struct {
	Linked   []UserRef `json:"linked"`
	Unlinked []UserRef `json:"unlinked"`
}

type RelationInstances struct {
	Linked   []InstanceRef `json:"linked"`
	Unlinked []InstanceRef `json:"unlinked"`
}

// Relations 是资源组关联的用户 / 实例（已关联与未关联）。
type Relations struct {
	GroupID   uint              `json:"group_id"`
	GroupName string            `json:"group_name"`
	Users     RelationUsers     `json:"users"`
	Instances RelationInstances `json:"instances"`
}

// GroupRelations 返回资源组的关联对象。
func (s *Service) GroupRelations(id uint) (*Relations, error) {
	g, err := s.repo.GetResourceGroup(id)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, notFound("资源组不存在")
	}
	linkedU, err := s.repo.GroupUsers(id)
	if err != nil {
		return nil, err
	}
	allU, err := s.repo.AllUsers()
	if err != nil {
		return nil, err
	}
	linkedI, err := s.repo.GroupInstances(id)
	if err != nil {
		return nil, err
	}
	allI, err := s.repo.AllInstances()
	if err != nil {
		return nil, err
	}
	out := &Relations{GroupID: id, GroupName: g.GroupName}
	out.Users.Linked = append([]UserRef{}, linkedU...)
	out.Users.Unlinked = []UserRef{}
	inU := map[uint]bool{}
	for _, u := range linkedU {
		inU[u.ID] = true
	}
	for _, u := range allU {
		if !inU[u.ID] {
			out.Users.Unlinked = append(out.Users.Unlinked, u)
		}
	}
	out.Instances.Linked = append([]InstanceRef{}, linkedI...)
	out.Instances.Unlinked = []InstanceRef{}
	inI := map[uint]bool{}
	for _, i := range linkedI {
		inI[i.ID] = true
	}
	for _, i := range allI {
		if !inI[i.ID] {
			out.Instances.Unlinked = append(out.Instances.Unlinked, i)
		}
	}
	return out, nil
}

// RelationChange 是一次关联调整：要新增、要移除的对象 ID。
type RelationChange struct {
	Add    []uint `json:"add"`
	Remove []uint `json:"remove"`
}

// ChangeGroupRelations 调整资源组关联的用户（kind="users"）或实例（kind="instances"）。
func (s *Service) ChangeGroupRelations(a audit.Actor, id uint, kind string, ch RelationChange) error {
	if kind != "users" && kind != "instances" {
		return bad("未知的关联对象类型")
	}
	add, err := dedupIDs(ch.Add)
	if err != nil {
		return err
	}
	remove, err := dedupIDs(ch.Remove)
	if err != nil {
		return err
	}
	if len(add) == 0 && len(remove) == 0 {
		return bad("没有要调整的对象")
	}
	if len(add)+len(remove) > 2000 {
		return bad("一次最多调整 2000 个对象")
	}
	for _, x := range add {
		for _, y := range remove {
			if x == y {
				return bad("同一个对象不能同时新增和移除")
			}
		}
	}
	action := map[string]string{"users": "调整资源组关联用户", "instances": "调整资源组关联实例"}[kind]
	label := map[string]string{"users": "用户", "instances": "实例"}[kind]
	return s.tx(a, action, func(r Repo) (string, error) {
		g, err := r.GetResourceGroup(id)
		if err != nil {
			return "", err
		}
		if g == nil {
			return "", notFound("资源组不存在")
		}
		if g.IsDeleted != 0 {
			return "", conflict("资源组已停用，请先恢复")
		}
		names := map[uint]string{}
		if kind == "users" {
			all, err := r.AllUsers()
			if err != nil {
				return "", err
			}
			for _, u := range all {
				names[u.ID] = u.Username
			}
		} else {
			all, err := r.AllInstances()
			if err != nil {
				return "", err
			}
			for _, i := range all {
				names[i.ID] = i.Name
			}
		}
		pick := func(ids []uint) ([]string, error) {
			var out []string
			for _, x := range ids {
				n, ok := names[x]
				if !ok {
					return nil, bad(errf("%s #%d 不存在", label, x))
				}
				out = append(out, n)
			}
			return out, nil
		}
		addNames, err := pick(add)
		if err != nil {
			return "", err
		}
		removeNames, err := pick(remove)
		if err != nil {
			return "", err
		}
		if kind == "users" {
			if len(add) > 0 {
				err = r.AddGroupUsers(id, add)
			}
			if err == nil && len(remove) > 0 {
				err = r.RemoveGroupUsers(id, remove)
			}
		} else {
			if len(add) > 0 {
				err = r.AddGroupInstances(id, add)
			}
			if err == nil && len(remove) > 0 {
				err = r.RemoveGroupInstances(id, remove)
			}
		}
		if err != nil {
			return "", err
		}
		var parts []string
		if len(addNames) > 0 {
			parts = append(parts, "新增"+label+" "+joinNames(addNames)) // i18n-ignore 存库的审计说明
		}
		if len(removeNames) > 0 {
			parts = append(parts, "移除"+label+" "+joinNames(removeNames)) // i18n-ignore 存库的审计说明
		}
		return errf("资源组「%s」（#%d）：%s", g.GroupName, id, strings.Join(parts, "；")), nil // i18n-ignore 存库的审计说明
	})
}
