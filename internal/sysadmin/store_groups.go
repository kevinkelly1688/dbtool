// @author: kevinkelly
// @license: Apache Licence
// @file: store_groups.go
// @time: 2026/10/28

package sysadmin

import (
	"time"

	"dbtool/internal/model"
)

// ---- 资源组 ----

func (s *store) ListResourceGroups(f ResGroupFilter) ([]model.ResourceGroup, error) {
	q := s.db.Model(&model.ResourceGroup{})
	if !f.IncludeDeleted {
		q = q.Where("is_deleted = 0")
	}
	if f.Search != "" {
		q = q.Where("group_name LIKE ?", likePattern(f.Search))
	}
	var rows []model.ResourceGroup
	err := q.Order("group_id").Find(&rows).Error
	return rows, err
}

func (s *store) GetResourceGroup(id uint) (*model.ResourceGroup, error) {
	var g model.ResourceGroup
	if err := s.db.Where("group_id = ?", id).First(&g).Error; err != nil {
		return nil, nilIfMissing(err)
	}
	return &g, nil
}

func (s *store) ResourceGroupByName(name string) (*model.ResourceGroup, error) {
	var g model.ResourceGroup
	if err := s.db.Where("group_name = ?", name).First(&g).Error; err != nil {
		return nil, nilIfMissing(err)
	}
	return &g, nil
}

func (s *store) CreateResourceGroup(g *model.ResourceGroup) error { return s.quiet().Create(g).Error }

func (s *store) UpdateResourceGroup(id uint, fields map[string]any) error {
	return s.quiet().Model(&model.ResourceGroup{}).Where("group_id = ?", id).Updates(fields).Error
}

func (s *store) ResourceGroupCounts() (users, instances map[uint]int, err error) {
	count := func(q string) (map[uint]int, error) {
		var rows []struct {
			ID uint
			N  int
		}
		if err := s.db.Raw(q).Scan(&rows).Error; err != nil {
			return nil, err
		}
		m := make(map[uint]int, len(rows))
		for _, r := range rows {
			m[r.ID] = r.N
		}
		return m, nil
	}
	if users, err = count("SELECT resourcegroup_id AS id, COUNT(*) AS n FROM dbt_sql_users_resource_group GROUP BY resourcegroup_id"); err != nil {
		return nil, nil, err
	}
	instances, err = count("SELECT resourcegroup_id AS id, COUNT(*) AS n FROM dbt_sql_instance_resource_group GROUP BY resourcegroup_id")
	return users, instances, err
}

func (s *store) GroupUsers(groupID uint) ([]UserRef, error) {
	var rows []UserRef
	err := s.db.Raw("SELECT u.id AS id, u.username AS username, u.display AS display, u.is_active AS is_active FROM dbt_sql_users u "+
		"JOIN dbt_sql_users_resource_group ur ON ur.users_id = u.id WHERE ur.resourcegroup_id = ? ORDER BY u.username", groupID).Scan(&rows).Error
	return rows, err
}

func (s *store) GroupInstances(groupID uint) ([]InstanceRef, error) {
	var rows []InstanceRef
	err := s.db.Raw("SELECT i.id AS id, i.instance_name AS name, i.db_type AS db_type, i.type AS type FROM dbt_sql_instance i "+
		"JOIN dbt_sql_instance_resource_group ir ON ir.instance_id = i.id WHERE ir.resourcegroup_id = ? ORDER BY i.instance_name", groupID).Scan(&rows).Error
	return rows, err
}

func (s *store) AllUsers() ([]UserRef, error) {
	var rows []UserRef
	err := s.db.Raw("SELECT id, username, display, is_active FROM dbt_sql_users ORDER BY username").Scan(&rows).Error
	return rows, err
}

func (s *store) AllInstances() ([]InstanceRef, error) {
	var rows []InstanceRef
	err := s.db.Raw("SELECT id, instance_name AS name, db_type, type FROM dbt_sql_instance ORDER BY instance_name").Scan(&rows).Error
	return rows, err
}

func (s *store) AddGroupUsers(groupID uint, ids []uint) error {
	return s.insertLinks("dbt_sql_users_resource_group", "resourcegroup_id", "users_id", groupID, ids)
}

func (s *store) RemoveGroupUsers(groupID uint, ids []uint) error {
	return s.deleteLinks("dbt_sql_users_resource_group", "resourcegroup_id", "users_id", groupID, ids)
}

func (s *store) AddGroupInstances(groupID uint, ids []uint) error {
	return s.insertLinks("dbt_sql_instance_resource_group", "resourcegroup_id", "instance_id", groupID, ids)
}

func (s *store) RemoveGroupInstances(groupID uint, ids []uint) error {
	return s.deleteLinks("dbt_sql_instance_resource_group", "resourcegroup_id", "instance_id", groupID, ids)
}

// OpenWorkflowCount：待审批的审批流 + 已通过但尚未结束的 SQL 工单（审核通过、定时、排队、执行中）。
func (s *store) OpenWorkflowCount(groupID uint) (int64, error) {
	var n int64
	err := s.db.Raw("SELECT (SELECT COUNT(*) FROM dbt_workflow_audit WHERE group_id = ? AND current_status = 0) + "+
		"(SELECT COUNT(*) FROM dbt_sql_workflow WHERE group_id = ? AND status IN ?)",
		groupID, groupID, []string{model.WfReviewPass, model.WfTimingTask, model.WfQueuing, model.WfExecuting}).Scan(&n).Error
	return n, err
}

// ---- 审批流配置 ----

func (s *store) ListAuditSettings() ([]model.WorkflowAuditSetting, error) {
	var rows []model.WorkflowAuditSetting
	err := s.db.Order("group_id, workflow_type").Find(&rows).Error
	return rows, err
}

func (s *store) GetAuditSetting(groupID uint, workflowType int) (*model.WorkflowAuditSetting, error) {
	var st model.WorkflowAuditSetting
	if err := s.db.Where("group_id = ? AND workflow_type = ?", groupID, workflowType).First(&st).Error; err != nil {
		return nil, nilIfMissing(err)
	}
	return &st, nil
}

// SaveAuditSetting 新增或更新一条审批流配置（(group_id, workflow_type) 唯一）。更新时只改 audit_auth_groups 与 sys_time。
func (s *store) SaveAuditSetting(groupID uint, groupName string, workflowType int, authGroups string) error {
	cur, err := s.GetAuditSetting(groupID, workflowType)
	if err != nil {
		return err
	}
	if cur != nil {
		return s.db.Model(&model.WorkflowAuditSetting{}).Where("audit_setting_id = ?", cur.AuditSettingID).
			Updates(map[string]any{"audit_auth_groups": authGroups, "sys_time": time.Now()}).Error
	}
	return s.db.Create(&model.WorkflowAuditSetting{GroupID: groupID, GroupName: groupName, WorkflowType: workflowType, AuditAuthGroups: authGroups}).Error
}

func (s *store) DeleteAuditSetting(groupID uint, workflowType int) error {
	return s.db.Where("group_id = ? AND workflow_type = ?", groupID, workflowType).Delete(&model.WorkflowAuditSetting{}).Error
}

func (s *store) RenameAuditSettingGroup(groupID uint, name string) error {
	return s.db.Model(&model.WorkflowAuditSetting{}).Where("group_id = ?", groupID).Update("group_name", name).Error
}

// ReviewApprovers 返回某一级的审批人：资源组内、属于该权限组、启用的用户。是否持有审批按钮由调用方按按钮权限计算。
func (s *store) ReviewApprovers(authGroupID, resourceGroupID uint) ([]Approver, error) {
	var rows []struct {
		ID           uint
		Username     string
		Display      string
		IsSuper      bool
		FailedCount  int
		LastFailedAt *time.Time
	}
	err := s.db.Raw(`
SELECT u.id AS id, u.username AS username, u.display AS display,
  u.is_superuser AS is_super, u.failed_login_count AS failed_count, u.last_login_failed_at AS last_failed_at
FROM dbt_sql_users u
JOIN dbt_sql_users_resource_group ur ON ur.users_id = u.id AND ur.resourcegroup_id = ?
JOIN dbt_sql_users_groups ug ON ug.users_id = u.id AND ug.group_id = ?
WHERE u.is_active = 1 ORDER BY u.username`, resourceGroupID, authGroupID).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]Approver, len(rows))
	for i, r := range rows {
		out[i] = Approver{ID: r.ID, Username: r.Username, Display: r.Display, Super: r.IsSuper, FailedCount: r.FailedCount, LastFailedAt: r.LastFailedAt}
	}
	return out, nil
}
