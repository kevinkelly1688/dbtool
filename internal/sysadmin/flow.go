// @author: kevinkelly
// @license: Apache Licence
// @file: flow.go
// @time: 2026/10/28

package sysadmin

import (
	"strconv"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// 审批流存储格式（与 internal/audit 一致）：dbt_workflow_audit_setting.audit_auth_groups 是
// 逗号分隔的 dbt_auth_group.id，如 "3,5"，按顺序逐级审批，列宽 255。

const (
	maxFlowLevels = 10
	maxFlowLen    = 255
)

// ApproverView 是某一级的一个可审批用户。
type ApproverView struct {
	ID       uint   `json:"id"`
	Username string `json:"username"`
	Display  string `json:"display"`
	HasPerm  bool   `json:"has_perm"` // 是否有审批按钮（按按钮权限计算，含单独授权 / 禁用）；没有则审批时会被拒绝
	Locked   bool   `json:"locked"`   // 账号因登录失败已被临时锁定（锁定期内无法登录，也就审批不了）
}

// FlowLevel 是审批流里的一级。
type FlowLevel struct {
	Level         int            `json:"level"`
	AuthGroupID   uint           `json:"auth_group_id"`
	AuthGroupName string         `json:"auth_group_name"`
	Exists        bool           `json:"exists"` // false：权限组不存在（被删除或配置里是无效内容）
	Approvers     []ApproverView `json:"approvers"`
	Warning       string         `json:"warning,omitempty"` // 非空表示该级无人可审
}

// FlowView 是一个「资源组 × 工单类型」的审批流。
type FlowView struct {
	GroupID      uint        `json:"group_id"`
	GroupName    string      `json:"group_name"`
	WorkflowType int         `json:"workflow_type"`
	TypeLabel    string      `json:"type_label"`
	ReviewButton string      `json:"review_button"`
	Configured   bool        `json:"configured"`
	AuthGroupIDs []uint      `json:"auth_group_ids"`
	Text         string      `json:"text"`
	Levels       []FlowLevel `json:"levels"`
	Warnings     []string    `json:"warnings"`
}

// MatrixCell 是总览矩阵的一个单元格。
type MatrixCell struct {
	Type       int    `json:"type"`
	Configured bool   `json:"configured"`
	Empty      bool   `json:"empty"` // 已配置但没有任何一级（只有 SQL 上线工单有“空流程”的含义）
	Text       string `json:"text"`
	Warn       bool   `json:"warn"`
	WarnText   string `json:"warn_text,omitempty"`
	NoApprover bool   `json:"no_approver"` // 已配置，且至少有一级无人可审
}

type MatrixRow struct {
	GroupID    uint         `json:"group_id"`
	GroupName  string       `json:"group_name"`
	Cells      []MatrixCell `json:"cells"`
	Configured int          `json:"configured"` // 已配置的工单类型数（左侧列表的 n/3 徽标）
	Warns      int          `json:"warns"`      // 有告警的单元格数
}

// FlowSummary 是总览矩阵上方的三张 KPI。
type FlowSummary struct {
	Total        int `json:"total"`        // 资源组 × 工单类型
	Configured   int `json:"configured"`   // 已配置
	Unconfigured int `json:"unconfigured"` // 未配置（不能提交该类工单）
	NoApprover   int `json:"no_approver"`  // 已配置但某级无人可审
}

// FlowOverview 是审批流程配置页的总览数据。
type FlowOverview struct {
	Types      []WorkflowTypeInfo `json:"types"`
	Groups     []IDName           `json:"groups"`      // 未停用的资源组
	AuthGroups []IDName           `json:"auth_groups"` // 全部权限组
	Matrix     []MatrixRow        `json:"matrix"`
	Summary    FlowSummary        `json:"summary"`
}

// authGroupNames 返回全部权限组的 ID → 名称。
func (s *Service) authGroupNames() (map[uint]string, error) {
	rows, err := s.repo.ListAuthGroups()
	if err != nil {
		return nil, err
	}
	m := make(map[uint]string, len(rows))
	for _, r := range rows {
		m[r.ID] = r.Name
	}
	return m, nil
}

// parseFlowIDs 解析 audit_auth_groups。解析规则与 internal/audit 的 splitIDs 一致（逗号分隔、忽略空白与空项）；
// 不是正整数的项放进 invalid，原样返回给调用方提示。
func parseFlowIDs(s string) (ids []uint, invalid []string) {
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil || n == 0 {
			invalid = append(invalid, p)
			continue
		}
		ids = append(ids, uint(n))
	}
	return ids, invalid
}

// formatFlowIDs 生成存库格式，如 "3,5"（无空格，与 internal/audit 的 strings.Join(groups, ",") 一致）。
func formatFlowIDs(ids []uint) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatUint(uint64(id), 10)
	}
	return strings.Join(parts, ",")
}

// flowText 是可读的审批流，如 “TL → DBA”；权限组已不存在时显示 #ID。
func flowText(ids []uint, names map[uint]string) string {
	if len(ids) == 0 {
		return "（空）"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		if n, ok := names[id]; ok {
			parts[i] = n
		} else {
			parts[i] = "#" + strconv.FormatUint(uint64(id), 10)
		}
	}
	return strings.Join(parts, " → ")
}

// approverCache 在一次请求里缓存「权限组 × 资源组」的审批人查询。
type approverCache struct {
	repo Repo
	m    map[string][]Approver
	// adjust 非空时，对每次查到的审批人名单再做一次调整（预览“假设调整了资源组成员”用）。
	adjust func(authGroupID uint, list []Approver) ([]Approver, error)

	lockSet   bool // 登录锁定策略只读一次
	lockCount int
	lockWin   time.Duration
}

func (c *approverCache) get(authGroupID, groupID uint) ([]Approver, error) {
	key := strconv.FormatUint(uint64(authGroupID), 10) + "/" + strconv.FormatUint(uint64(groupID), 10)
	if v, ok := c.m[key]; ok {
		return v, nil
	}
	v, err := c.repo.ReviewApprovers(authGroupID, groupID)
	if err != nil {
		return nil, err
	}
	if c.adjust != nil {
		if v, err = c.adjust(authGroupID, v); err != nil {
			return nil, err
		}
	}
	c.m[key] = v
	return v, nil
}

func (c *approverCache) lock(s *Service) (int, time.Duration) {
	if !c.lockSet {
		c.lockCount, c.lockWin = s.lockPolicy()
		c.lockSet = true
	}
	return c.lockCount, c.lockWin
}

// buildFlow 生成一个审批流的展示数据，并计算每一级是否无人可审。
func (s *Service) buildFlow(cache *approverCache, g IDName, wt WorkflowTypeInfo, raw string, configured bool, names map[uint]string) (*FlowView, error) {
	ids, invalid := parseFlowIDs(raw)
	v := &FlowView{GroupID: g.ID, GroupName: g.Name, WorkflowType: wt.Type, TypeLabel: wt.Label, ReviewButton: wt.ReviewButton,
		Configured: configured, AuthGroupIDs: ids, Levels: []FlowLevel{}, Warnings: []string{}}
	if v.AuthGroupIDs == nil {
		v.AuthGroupIDs = []uint{}
	}
	v.Text = flowText(ids, names)
	for i, id := range ids {
		lv := FlowLevel{Level: i + 1, AuthGroupID: id, Approvers: []ApproverView{}}
		name, ok := names[id]
		lv.AuthGroupName, lv.Exists = name, ok
		if !ok {
			lv.AuthGroupName = "#" + strconv.FormatUint(uint64(id), 10)
			lv.Warning = "权限组不存在（可能已被删除），该级无人可审，工单会卡在这一级"
		} else {
			ap, err := cache.get(id, g.ID)
			if err != nil {
				return nil, err
			}
			lockCount, lockWin := cache.lock(s)
			states := s.approverStates(ap, wt, lockCount, lockWin)
			withPerm, usable := 0, 0
			for i, a := range ap {
				st := states[i]
				lv.Approvers = append(lv.Approvers, ApproverView{ID: a.ID, Username: a.Username, Display: a.Display, HasPerm: st.Can, Locked: st.Locked})
				if st.Can {
					withPerm++
				}
				if st.ok() {
					usable++
				}
			}
			switch {
			case len(ap) == 0:
				lv.Warning = errf("该级无人可审：资源组「%s」内没有属于权限组「%s」的启用用户", g.Name, name)
			case withPerm == 0:
				lv.Warning = errf("该级无人可审：权限组「%s」的成员都没有审批按钮 %s，审批会被拒绝", name, wt.ReviewButton)
			case usable == 0:
				lv.Warning = errf("该级无人可审：权限组「%s」里有审核权限的审批人账号都已被锁定（登录失败次数过多）", name)
			}
		}
		if lv.Warning != "" {
			v.Warnings = append(v.Warnings, errf("第 %d 级：%s", lv.Level, lv.Warning))
		}
		v.Levels = append(v.Levels, lv)
	}
	if len(invalid) > 0 {
		v.Warnings = append(v.Warnings, errf("配置里含有无法识别的内容：%s，审批引擎会忽略它们", strings.Join(invalid, "、")))
	}
	return v, nil
}

func (s *Service) resourceGroupRef(r Repo, id uint) (*model.ResourceGroup, error) {
	g, err := r.GetResourceGroup(id)
	if err != nil {
		return nil, err
	}
	if g == nil || g.IsDeleted != 0 {
		return nil, notFound("资源组不存在或已停用")
	}
	return g, nil
}

// FlowDetail 返回某资源组某工单类型的当前审批流（未配置时 Configured=false 且没有任何级）。
func (s *Service) FlowDetail(groupID uint, workflowType int) (*FlowView, error) {
	wt, ok := typeInfo(workflowType)
	if !ok {
		return nil, bad("不支持的工单类型")
	}
	g, err := s.resourceGroupRef(s.repo, groupID)
	if err != nil {
		return nil, err
	}
	names, err := s.authGroupNames()
	if err != nil {
		return nil, err
	}
	st, err := s.repo.GetAuditSetting(groupID, workflowType)
	if err != nil {
		return nil, err
	}
	raw := ""
	if st != nil {
		raw = st.AuditAuthGroups
	}
	return s.buildFlow(&approverCache{repo: s.repo, m: map[string][]Approver{}}, IDName{g.GroupID, g.GroupName}, wt, raw, st != nil, names)
}

// checkFlowInput 校验要保存的审批流：不能为空、权限组必须存在、同一条流里不重复、不超过列宽。
func (s *Service) checkFlowInput(workflowType int, ids []uint, names map[uint]string) (WorkflowTypeInfo, error) {
	wt, ok := typeInfo(workflowType)
	if !ok {
		return wt, bad("不支持的工单类型")
	}
	if len(ids) == 0 {
		return wt, bad("审批流程不能为空，请至少选择一个审批权限组")
	}
	if len(ids) > maxFlowLevels {
		return wt, bad(errf("审批流程最多 %d 级", maxFlowLevels))
	}
	seen := map[uint]bool{}
	for _, id := range ids {
		n, exists := names[id]
		if !exists {
			return wt, bad(errf("权限组 #%d 不存在", id))
		}
		if seen[id] {
			return wt, bad(errf("同一个权限组不能在同一条审批流里选两次：%s", n))
		}
		seen[id] = true
	}
	if len(formatFlowIDs(ids)) > maxFlowLen {
		return wt, bad("审批流程太长，存储上限是 255 个字符")
	}
	return wt, nil
}

// PreviewFlow 按未保存的权限组顺序生成审批流预览（含每级审批人与警告），不写库。
func (s *Service) PreviewFlow(groupID uint, workflowType int, ids []uint) (*FlowView, error) {
	names, err := s.authGroupNames()
	if err != nil {
		return nil, err
	}
	wt, err := s.checkFlowInput(workflowType, ids, names)
	if err != nil {
		return nil, err
	}
	g, err := s.resourceGroupRef(s.repo, groupID)
	if err != nil {
		return nil, err
	}
	return s.buildFlow(&approverCache{repo: s.repo, m: map[string][]Approver{}}, IDName{g.GroupID, g.GroupName}, wt, formatFlowIDs(ids), true, names)
}

// SaveFlowResult 是保存审批流的结果。
type SaveFlowResult struct {
	Changed bool      `json:"changed"`
	Before  string    `json:"before"`
	After   string    `json:"after"`
	Flow    *FlowView `json:"flow"`
}

// SaveFlow 保存（新增或修改）一条审批流，并写审计。已在审批中的工单不受影响（审批流在工单创建时已复制进 dbt_workflow_audit）。
func (s *Service) SaveFlow(a audit.Actor, groupID uint, workflowType int, ids []uint) (*SaveFlowResult, error) {
	names, err := s.authGroupNames()
	if err != nil {
		return nil, err
	}
	wt, err := s.checkFlowInput(workflowType, ids, names)
	if err != nil {
		return nil, err
	}
	res := &SaveFlowResult{}
	err = s.tx(a, "修改审批流", func(r Repo) (string, error) {
		g, err := s.resourceGroupRef(r, groupID)
		if err != nil {
			return "", err
		}
		cur, err := r.GetAuditSetting(groupID, workflowType)
		if err != nil {
			return "", err
		}
		res.Before = "（未配置）"
		if cur != nil {
			old, _ := parseFlowIDs(cur.AuditAuthGroups)
			res.Before = flowText(old, names)
		}
		res.After = flowText(ids, names)
		formatted := formatFlowIDs(ids)
		res.Changed = cur == nil || strings.TrimSpace(cur.AuditAuthGroups) != formatted
		if !res.Changed {
			return g.GroupName + " · " + wt.Label + "：无变化（" + res.After + "）", nil // i18n-ignore 存库的审计说明
		}
		if err := r.SaveAuditSetting(groupID, g.GroupName, workflowType, formatted); err != nil {
			return "", err
		}
		return g.GroupName + " · " + wt.Label + "：" + res.Before + " ⇒ " + res.After, nil
	})
	if err != nil {
		return nil, err
	}
	res.Flow, err = s.buildFlow(&approverCache{repo: s.repo, m: map[string][]Approver{}}, IDName{groupID, s.groupNameOr(groupID)}, wt, formatFlowIDs(ids), true, names)
	return res, err
}

func (s *Service) groupNameOr(id uint) string {
	if g, err := s.repo.GetResourceGroup(id); err == nil && g != nil {
		return g.GroupName
	}
	return "#" + strconv.FormatUint(uint64(id), 10)
}

// DeleteFlow 删除一条审批流配置，删除后该资源组不能再提交这类工单（“未配置审流”）。
func (s *Service) DeleteFlow(a audit.Actor, groupID uint, workflowType int) error {
	wt, ok := typeInfo(workflowType)
	if !ok {
		return bad("不支持的工单类型")
	}
	names, err := s.authGroupNames()
	if err != nil {
		return err
	}
	return s.tx(a, "删除审批流", func(r Repo) (string, error) {
		g, err := r.GetResourceGroup(groupID)
		if err != nil {
			return "", err
		}
		if g == nil {
			return "", notFound("资源组不存在")
		}
		cur, err := r.GetAuditSetting(groupID, workflowType)
		if err != nil {
			return "", err
		}
		if cur == nil {
			return "", notFound("该审批流没有配置")
		}
		old, _ := parseFlowIDs(cur.AuditAuthGroups)
		if err := r.DeleteAuditSetting(groupID, workflowType); err != nil {
			return "", err
		}
		return g.GroupName + " · " + wt.Label + "：" + flowText(old, names) + " ⇒ （未配置）", nil // i18n-ignore 存库的审计说明
	})
}

// FlowOverview 返回总览矩阵（资源组 × 工单类型）。
func (s *Service) FlowOverview() (*FlowOverview, error) {
	groups, err := s.repo.ListResourceGroups(ResGroupFilter{})
	if err != nil {
		return nil, err
	}
	authRows, err := s.repo.ListAuthGroups()
	if err != nil {
		return nil, err
	}
	settings, err := s.repo.ListAuditSettings()
	if err != nil {
		return nil, err
	}
	names := make(map[uint]string, len(authRows))
	out := &FlowOverview{Types: WorkflowTypes, Groups: []IDName{}, AuthGroups: []IDName{}, Matrix: []MatrixRow{}}
	for _, r := range authRows {
		names[r.ID] = r.Name
		out.AuthGroups = append(out.AuthGroups, IDName{r.ID, r.Name})
	}
	byKey := map[[2]int]model.WorkflowAuditSetting{}
	for _, st := range settings {
		byKey[[2]int{int(st.GroupID), st.WorkflowType}] = st
	}
	cache := &approverCache{repo: s.repo, m: map[string][]Approver{}}
	for _, g := range groups {
		out.Groups = append(out.Groups, IDName{g.GroupID, g.GroupName})
		row := MatrixRow{GroupID: g.GroupID, GroupName: g.GroupName}
		for _, wt := range WorkflowTypes {
			cell := MatrixCell{Type: wt.Type}
			if st, ok := byKey[[2]int{int(g.GroupID), wt.Type}]; ok {
				v, err := s.buildFlow(cache, IDName{g.GroupID, g.GroupName}, wt, st.AuditAuthGroups, true, names)
				if err != nil {
					return nil, err
				}
				cell.Configured, cell.Text = true, v.Text
				cell.Empty = len(v.AuthGroupIDs) == 0
				if len(v.Warnings) > 0 {
					cell.Warn, cell.WarnText = true, strings.Join(v.Warnings, "；")
				}
				for _, lv := range v.Levels {
					if lv.Warning != "" {
						cell.NoApprover = true
					}
				}
				// 查询权限申请和归档申请必须至少一级，空流程会让工单永远卡在待审核。
				if cell.Empty && wt.Type != WorkflowSQL {
					cell.Warn, cell.WarnText = true, "审批流为空，提交的工单无人审批（审批引擎会拒绝创建）"
				}
			}
			out.Summary.Total++
			if cell.Configured {
				out.Summary.Configured++
				row.Configured++
			} else {
				out.Summary.Unconfigured++
			}
			if cell.NoApprover {
				out.Summary.NoApprover++
			}
			if cell.Warn {
				row.Warns++
			}
			row.Cells = append(row.Cells, cell)
		}
		out.Matrix = append(out.Matrix, row)
	}
	return out, nil
}
