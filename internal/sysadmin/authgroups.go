// @author: kevinkelly
// @license: Apache Licence
// @file: authgroups.go
// @time: 2026/10/28

package sysadmin

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// AuthGroupItem 是权限组列表的一行。
type AuthGroupItem struct {
	ID      uint   `json:"id"`
	Name    string `json:"name"`
	Members int    `json:"members"`
	Flows   int    `json:"flows"` // 被多少条审批流引用

	Description string `json:"description"` // 说明（旁路表 dbt_auth_group_ext）
	Builtin     bool   `json:"builtin"`     // 内置权限组：不能改名、不能删除（dbt_auth_group_ext.builtin）
}

// FlowRef 是权限组被引用的一条审批流。
type FlowRef struct {
	GroupID      uint   `json:"group_id"`
	GroupName    string `json:"group_name"`
	WorkflowType int    `json:"workflow_type"`
	TypeLabel    string `json:"type_label"`
	Level        int    `json:"level"`
}

// AuthGroupDetail 是权限组详情：基本信息、成员、被引用的审批流。权限（按钮）在按钮权限页里管理，见 internal/btnperm。
type AuthGroupDetail struct {
	ID      uint      `json:"id"`
	Name    string    `json:"name"`
	Members []UserRef `json:"members"`
	Flows   []FlowRef `json:"flows"`

	Description string `json:"description"`
	Builtin     bool   `json:"builtin"`
}

// flowRefs 返回引用了某权限组的全部审批流（含已停用资源组上的配置）。
func (s *Service) flowRefs(authGroupID uint) ([]FlowRef, error) {
	settings, err := s.repo.ListAuditSettings()
	if err != nil {
		return nil, err
	}
	groups, err := s.repo.ListResourceGroups(ResGroupFilter{IncludeDeleted: true})
	if err != nil {
		return nil, err
	}
	gname := map[uint]string{}
	for _, g := range groups {
		gname[g.GroupID] = g.GroupName
	}
	out := []FlowRef{}
	for _, st := range settings {
		ids, _ := parseFlowIDs(st.AuditAuthGroups)
		for i, id := range ids {
			if id == authGroupID {
				label := strconv.Itoa(st.WorkflowType)
				if t, ok := typeInfo(st.WorkflowType); ok {
					label = t.Label
				}
				name := gname[st.GroupID]
				if name == "" {
					name = st.GroupName
				}
				out = append(out, FlowRef{GroupID: st.GroupID, GroupName: name, WorkflowType: st.WorkflowType, TypeLabel: label, Level: i + 1})
				break
			}
		}
	}
	return out, nil
}

// ListAuthGroups 返回权限组列表。
func (s *Service) ListAuthGroups() ([]AuthGroupItem, error) {
	rows, err := s.repo.ListAuthGroups()
	if err != nil {
		return nil, err
	}
	settings, err := s.repo.ListAuditSettings()
	if err != nil {
		return nil, err
	}
	refs := map[uint]int{}
	for _, st := range settings {
		ids, _ := parseFlowIDs(st.AuditAuthGroups)
		seen := map[uint]bool{}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				refs[id]++
			}
		}
	}
	descs, err := s.ext().AuthGroupDescs(nil)
	if err != nil {
		return nil, err
	}
	builtins := builtinGroups(s.ext(), nil)
	out := make([]AuthGroupItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, AuthGroupItem{ID: r.ID, Name: r.Name, Members: r.Members, Flows: refs[r.ID], Description: descs[r.ID], Builtin: builtins[r.ID]})
	}
	return out, nil
}

// GetAuthGroup 返回权限组详情。
func (s *Service) GetAuthGroup(id uint) (*AuthGroupDetail, error) {
	g, err := s.repo.GetAuthGroup(id)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, notFound("权限组不存在")
	}
	members, err := s.repo.AuthGroupMembers(id)
	if err != nil {
		return nil, err
	}
	flows, err := s.flowRefs(id)
	if err != nil {
		return nil, err
	}
	if members == nil {
		members = []UserRef{}
	}
	descs, err := s.ext().AuthGroupDescs([]uint{id})
	if err != nil {
		return nil, err
	}
	return &AuthGroupDetail{ID: g.ID, Name: g.Name, Members: members, Flows: flows, Description: descs[id], Builtin: isBuiltinGroup(s.ext(), id)}, nil
}

func validateAuthGroupName(name string) (string, error) {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	if n == 0 || n > 150 {
		return "", bad("权限组名称需要 1 到 150 个字符")
	}
	if strings.ContainsAny(name, ",\n\r\t") {
		return "", bad("权限组名称不能包含逗号或换行")
	}
	return name, nil
}

// AuthGroupInput 是新建 / 编辑权限组的输入。
type AuthGroupInput struct {
	Name string `json:"name"`
	// Description 为 nil 表示不修改；空串表示清空。
	Description *string `json:"description"`
}

// cleanGroupDesc 校验说明（≤200 字、不含换行）。
func cleanGroupDesc(p *string) (string, error) {
	if p == nil {
		return "", nil
	}
	d := strings.TrimSpace(*p)
	if utf8.RuneCountInString(d) > 200 {
		return "", bad("说明最多 200 个字符")
	}
	if strings.ContainsAny(d, "\r\n\t") {
		return "", bad("说明不能包含换行或制表符")
	}
	return d, nil
}

// CreateAuthGroup 新建权限组（名称与说明）；权限在按钮权限页里授予。
func (s *Service) CreateAuthGroup(a audit.Actor, in AuthGroupInput) (uint, error) {
	name, err := validateAuthGroupName(in.Name)
	if err != nil {
		return 0, err
	}
	desc, err := cleanGroupDesc(in.Description)
	if err != nil {
		return 0, err
	}
	var id uint
	err = s.tx(a, "新建权限组", func(r Repo) (string, error) {
		if ex, err := r.AuthGroupByName(name); err != nil {
			return "", err
		} else if ex != nil {
			return "", conflict("已存在同名的权限组")
		}
		g := &model.AuthGroup{Name: name}
		if err := r.CreateAuthGroup(g); err != nil {
			return "", err
		}
		id = g.ID
		extra := ""
		if desc != "" {
			if err := extOf(r).UpsertAuthGroupDesc(g.ID, desc, a.Username, s.now()); err != nil {
				return "", extMissingErr(err, "设置权限组说明")
			}
			extra = "；说明：" + desc // i18n-ignore 存库的审计说明
		}
		return errf("权限组「%s」（#%d）%s", name, g.ID, extra), nil // i18n-ignore 存库的审计说明
	})
	return id, err
}

// UpdateAuthGroup 修改权限组名称与说明。
func (s *Service) UpdateAuthGroup(a audit.Actor, id uint, in AuthGroupInput) error {
	name, err := validateAuthGroupName(in.Name)
	if err != nil {
		return err
	}
	desc, err := cleanGroupDesc(in.Description)
	if err != nil {
		return err
	}
	return s.tx(a, "修改权限组", func(r Repo) (string, error) {
		g, err := r.GetAuthGroup(id)
		if err != nil {
			return "", err
		}
		if g == nil {
			return "", notFound("权限组不存在")
		}
		var changes []string
		if name != g.Name {
			if isBuiltinGroup(extOf(r), id) {
				return "", errBuiltinRename(g.Name)
			}
			if ex, err := r.AuthGroupByName(name); err != nil {
				return "", err
			} else if ex != nil && ex.ID != id {
				return "", conflict("已存在同名的权限组")
			}
			if err := r.RenameAuthGroup(id, name); err != nil {
				return "", err
			}
			changes = append(changes, "名称："+g.Name+" → "+name) // i18n-ignore 存库的审计说明
		}
		if in.Description != nil {
			descs, err := extOf(r).AuthGroupDescs([]uint{id})
			if err != nil {
				return "", err
			}
			if old := descs[id]; old != desc {
				if err := extOf(r).UpsertAuthGroupDesc(id, desc, a.Username, s.now()); err != nil {
					return "", extMissingErr(err, "设置权限组说明")
				}
				changes = append(changes, "说明："+orDash(old)+" → "+orDash(desc)) // i18n-ignore 存库的审计说明
			}
		}
		if len(changes) == 0 {
			return errf("权限组「%s」（#%d）：无变化", g.Name, id), nil // i18n-ignore 存库的审计说明
		}
		return errf("权限组「%s」（#%d）：%s", name, id, strings.Join(changes, "；")), nil // i18n-ignore 存库的审计说明
	})
}

// DeleteAuthGroup 删除权限组。被审批流引用、或有待审批工单正在使用时拒绝；成员关系会一并解除。
func (s *Service) DeleteAuthGroup(a audit.Actor, id uint) error {
	return s.tx(a, "删除权限组", func(r Repo) (string, error) {
		g, err := r.GetAuthGroup(id)
		if err != nil {
			return "", err
		}
		if g == nil {
			return "", notFound("权限组不存在")
		}
		if isBuiltinGroup(extOf(r), id) {
			return "", errBuiltinDelete(g.Name)
		}
		refs, err := s.flowRefs(id)
		if err != nil {
			return "", err
		}
		if len(refs) > 0 {
			var parts []string
			for _, f := range refs {
				parts = append(parts, f.GroupName+"·"+f.TypeLabel)
			}
			return "", conflict(errf("该权限组还被 %d 条审批流引用（%s），请先到“审批流程配置”里移除", len(refs), trunc(strings.Join(parts, "、"), 120)))
		}
		n, err := r.PendingAuditsUsingGroup(id)
		if err != nil {
			return "", err
		}
		if n > 0 {
			return "", conflict(errf("有 %d 个待审批的工单正在使用该权限组，处理完再删除", n))
		}
		members, err := r.AuthGroupMembers(id)
		if err != nil {
			return "", err
		}
		if err := r.DeleteAuthGroup(id); err != nil {
			return "", err
		}
		if err := extOf(r).DeleteAuthGroupExt(id); err != nil { // 说明随权限组一起删除
			return "", err
		}
		return errf("权限组「%s」（#%d），解除 %d 个成员关系", g.Name, id, len(members)), nil // i18n-ignore 存库的审计说明
	})
}
