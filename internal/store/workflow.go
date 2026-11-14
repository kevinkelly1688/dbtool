// @author: kevinkelly
// @license: Apache Licence
// @file: workflow.go
// @time: 2026/10/28

package store

import (
	"errors"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"dbtool/internal/model"
	"dbtool/internal/privapply"
)

// 编译期检查：Store 实现权限申请服务需要的全部数据访问。
var _ privapply.Repo = (*Store)(nil)

func notFoundToNil(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	return err
}

// InTx 在一个数据库事务里执行 fn。fn 收到的 Repo 在同一事务内；返回错误则整体回滚。
func (s *Store) InTx(fn func(privapply.Repo) error) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		return fn(&Store{DB: tx, Mirage: s.Mirage})
	})
}

// likePattern 生成 LIKE 的参数，转义 % _ \，避免用户输入的通配符改变查询含义。
func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// ---- 审批 ----

func (s *Store) AuditSetting(groupID uint, workflowType int) (*model.WorkflowAuditSetting, error) {
	var out model.WorkflowAuditSetting
	err := s.DB.Where("group_id = ? AND workflow_type = ?", groupID, workflowType).First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &out, err
}

func (s *Store) AuthGroup(id uint) (*model.AuthGroup, error) {
	var out model.AuthGroup
	err := s.DB.First(&out, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &out, err
}

func (s *Store) UserInAuthGroup(userID, authGroupID uint) (bool, error) {
	var n int64
	err := s.DB.Table("dbt_sql_users_groups").Where("users_id = ? AND group_id = ?", userID, authGroupID).Count(&n).Error
	return n > 0, err
}

// AuthGroupUsers 返回属于指定权限组（按组名）且属于指定资源组的启用用户。
func (s *Store) AuthGroupUsers(names []string, resourceGroupID uint) ([]model.User, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var out []model.User
	err := s.DB.Raw(`
SELECT DISTINCT u.* FROM dbt_sql_users u
JOIN dbt_sql_users_resource_group ur ON ur.users_id = u.id AND ur.resourcegroup_id = ?
JOIN dbt_sql_users_groups ug ON ug.users_id = u.id
JOIN dbt_auth_group g ON g.id = ug.group_id AND g.name IN ?
WHERE u.is_active = 1 ORDER BY u.id`, resourceGroupID, names).Scan(&out).Error
	return out, err
}

func (s *Store) InsertAudit(a *model.WorkflowAudit) error { return s.DB.Create(a).Error }
func (s *Store) SaveAudit(a *model.WorkflowAudit) error   { return s.DB.Save(a).Error }
func (s *Store) InsertDetail(d *model.WorkflowAuditDetail) error {
	return s.DB.Create(d).Error
}
func (s *Store) InsertLog(l *model.WorkflowLog) error { return s.DB.Create(l).Error }

// auditIsExportExpr 是 AuditByWorkflow 为 SQL 上线类审批记录附带的子查询列。
const auditIsExportExpr = "COALESCE((SELECT w.is_offline_export FROM dbt_sql_workflow w WHERE w.id = dbt_workflow_audit.workflow_id), 0) <> 0 AS is_export_flag"

// AuditByWorkflow 按业务工单取审批记录。forUpdate=true 时加行锁（必须在事务内使用），
// 防止两个审批人同时操作同一张工单。
func (s *Store) AuditByWorkflow(workflowType int, workflowID uint, forUpdate bool) (*model.WorkflowAudit, error) {
	var out model.WorkflowAudit
	q := s.DB.Where("workflow_type = ? AND workflow_id = ?", workflowType, workflowID)
	if workflowType == 2 { // SQL 上线类：带出“是否数据导出工单”（见 model.WorkflowAudit.IsExport）
		q = q.Select("dbt_workflow_audit.*, " + auditIsExportExpr)
	}
	if forUpdate {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	err := q.First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &out, err
}

func (s *Store) AuditLogs(auditID uint) ([]model.WorkflowLog, error) {
	var out []model.WorkflowLog
	err := s.DB.Where("audit_id = ?", auditID).Order("id").Find(&out).Error
	return out, err
}

// ---- 资源组 ----

func (s *Store) UserResourceGroups(userID uint, all bool) ([]model.ResourceGroup, error) {
	var out []model.ResourceGroup
	q := s.DB.Model(&model.ResourceGroup{}).Where("dbt_resource_group.is_deleted = 0")
	if !all {
		q = q.Joins("JOIN dbt_sql_users_resource_group ur ON ur.resourcegroup_id = dbt_resource_group.group_id AND ur.users_id = ?", userID)
	}
	err := q.Order("dbt_resource_group.group_id").Find(&out).Error
	return out, err
}

func (s *Store) ResourceGroupByName(name string) (*model.ResourceGroup, error) {
	var out model.ResourceGroup
	err := s.DB.Where("group_name = ? AND is_deleted = 0", name).First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &out, err
}

func (s *Store) ResourceGroupByID(id uint) (*model.ResourceGroup, error) {
	var out model.ResourceGroup
	err := s.DB.Where("group_id = ?", id).First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &out, err
}

// ---- 权限申请 ----

func (s *Store) InsertApply(a *model.QueryPrivilegesApply) error {
	return s.DB.Omit("Instance").Create(a).Error
}

func (s *Store) SaveApply(a *model.QueryPrivilegesApply) error {
	return s.DB.Omit("Instance").Save(a).Error
}

func (s *Store) GetApply(id uint) (*model.QueryPrivilegesApply, error) {
	var out model.QueryPrivilegesApply
	err := s.DB.Preload("Instance").Where("apply_id = ?", id).First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &out, err
}

func (s *Store) ListApplies(f privapply.ApplyFilter) ([]model.QueryPrivilegesApply, int64, error) {
	q := s.DB.Model(&model.QueryPrivilegesApply{})
	switch f.Scope {
	case privapply.ScopeOwn:
		q = q.Where("user_name = ?", f.UserName)
	case privapply.ScopeGroups:
		if len(f.GroupIDs) == 0 { // 没有任何资源组：只能看自己的
			q = q.Where("user_name = ?", f.UserName)
		} else {
			// 有审批权限的用户看资源组内的全部，同时也能看到自己提交的
			q = q.Where("group_id IN ? OR user_name = ?", f.GroupIDs, f.UserName)
		}
	}
	q = applyListFilters(q, f)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []model.QueryPrivilegesApply
	q = q.Preload("Instance").Order("apply_id DESC").Offset(f.Offset)
	if f.Limit > 0 {
		q = q.Limit(f.Limit)
	}
	return out, total, q.Find(&out).Error
}

func (s *Store) WaitingApplies(username string, instanceID uint) ([]model.QueryPrivilegesApply, error) {
	var out []model.QueryPrivilegesApply
	err := s.DB.Where("user_name = ? AND instance_id = ? AND status = 0", username, instanceID).Find(&out).Error
	return out, err
}

// ---- 已授予的查询权限 ----

func (s *Store) InsertPrivileges(ps []model.QueryPrivilege) error {
	if len(ps) == 0 {
		return nil
	}
	return s.DB.Create(&ps).Error
}

func (s *Store) ListPrivileges(f privapply.PrivFilter) ([]privapply.PrivRow, int64, error) {
	q := s.DB.Table("dbt_query_privileges AS qp").
		Joins("JOIN dbt_sql_instance i ON i.id = qp.instance_id").
		Where("qp.is_deleted = 0")
	switch f.Scope {
	case privapply.ScopeOwn:
		q = q.Where("qp.user_name = ?", f.UserName)
	case privapply.ScopeGroups:
		q = q.Where("qp.instance_id IN ?", f.InstanceIDs)
	}
	q = privListFilters(q, f)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	q = q.Select("qp.*, i.instance_name AS instance_name").Order("qp.privilege_id DESC").Offset(f.Offset)
	if f.Limit > 0 {
		q = q.Limit(f.Limit)
	}
	var out []privapply.PrivRow
	return out, total, q.Scan(&out).Error
}

func (s *Store) GetPrivilege(id uint) (*model.QueryPrivilege, error) {
	var out model.QueryPrivilege
	err := s.DB.Where("privilege_id = ?", id).First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &out, err
}

func (s *Store) SavePrivilege(p *model.QueryPrivilege) error { return s.DB.Save(p).Error }

// LockUser 在当前事务内锁住用户行（SELECT ... FOR UPDATE），必须在事务内调用。
func (s *Store) LockUser(username string) error {
	var id uint
	err := s.DB.Raw("SELECT id FROM dbt_sql_users WHERE username = ? FOR UPDATE", username).Scan(&id).Error
	return err
}
