// @author: kevinkelly
// @license: Apache Licence
// @file: sqlflow.go
// @time: 2026/10/28

package store

import (
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"dbtool/internal/instext"
	"dbtool/internal/model"
	"dbtool/internal/sqlflow"
)

// SQLRepo 是 SQL 工单服务需要的数据访问：在 Store 的通用方法之上，加上工单、内容与定时计划。
// 单独成类型是因为 InTx 的签名与权限申请服务的不同。
type SQLRepo struct{ *Store }

var (
	_ sqlflow.Repo              = (*SQLRepo)(nil)
	_ sqlflow.StatusCounter     = (*SQLRepo)(nil)
	_ sqlflow.DownloadCounter   = (*SQLRepo)(nil)
	_ sqlflow.InstanceEnvLoader = (*SQLRepo)(nil)
)

// NewSQLRepo 包装 Store。
func NewSQLRepo(s *Store) *SQLRepo { return &SQLRepo{Store: s} }

func (r *SQLRepo) InTx(fn func(sqlflow.Repo) error) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		return fn(&SQLRepo{Store: &Store{DB: tx, Mirage: r.Mirage}})
	})
}

// GetInstance 取实例（带标签）；不存在返回 nil, nil。
func (r *SQLRepo) GetInstance(id uint) (*model.Instance, error) {
	var inst model.Instance
	err := r.DB.Preload("Tunnel").Preload("Tags").First(&inst, id).Error
	if err != nil {
		return nil, notFoundToNil(err)
	}
	return &inst, nil
}

func (r *SQLRepo) GroupInstances(groupID uint) ([]model.Instance, error) {
	var out []model.Instance
	// 已停用的实例（旁路表 dbt_instance_ext）不能再被选来提交工单；表不存在时不过滤。
	q := instext.ExcludeDisabled(r.DB.Model(&model.Instance{}), "dbt_sql_instance.id")
	err := q.
		Joins("JOIN dbt_sql_instance_resource_group ir ON ir.instance_id = dbt_sql_instance.id AND ir.resourcegroup_id = ?", groupID).
		Preload("Tags").Order("dbt_sql_instance.instance_name").Find(&out).Error
	return out, err
}

// ---- 工单 ----

func (r *SQLRepo) InsertWorkflow(w *model.SqlWorkflow, c *model.SqlWorkflowContent) error {
	if err := r.DB.Omit(clause.Associations).Create(w).Error; err != nil {
		return err
	}
	c.WorkflowID = w.ID
	return r.DB.Create(c).Error
}

func (r *SQLRepo) GetWorkflow(id uint, forUpdate bool) (*model.SqlWorkflow, error) {
	var w model.SqlWorkflow
	q := r.DB
	if forUpdate {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := q.First(&w, id).Error; err != nil {
		return nil, notFoundToNil(err)
	}
	if in, err := r.GetInstance(w.InstanceID); err != nil {
		return nil, err
	} else {
		w.Instance = in
	}
	return &w, nil
}

// SaveWorkflow 只更新会变化的列，避免覆盖并发写入的其他列，也不去碰关联的实例。
func (r *SQLRepo) SaveWorkflow(w *model.SqlWorkflow) error {
	return r.DB.Model(&model.SqlWorkflow{}).Where("id = ?", w.ID).Updates(map[string]any{
		"status":            w.Status,
		"audit_auth_groups": w.AuditAuthGroups,
		"run_date_start":    w.RunDateStart,
		"run_date_end":      w.RunDateEnd,
		"finish_time":       w.FinishTime,
		"is_backup":         w.IsBackup,
		"file_name":         w.FileName,
		"syntax_type":       w.SyntaxType,
	}).Error
}

func (r *SQLRepo) GetContent(workflowID uint) (*model.SqlWorkflowContent, error) {
	var c model.SqlWorkflowContent
	if err := r.DB.Where("workflow_id = ?", workflowID).First(&c).Error; err != nil {
		return nil, notFoundToNil(err)
	}
	return &c, nil
}

func (r *SQLRepo) SaveContent(c *model.SqlWorkflowContent) error {
	if c.ID == 0 {
		return r.DB.Create(c).Error
	}
	return r.DB.Model(&model.SqlWorkflowContent{}).Where("id = ?", c.ID).Updates(map[string]any{
		"review_content": c.ReviewContent,
		"execute_result": c.ExecuteResult,
	}).Error
}

// workflowOrder 把列表的排序键映射成列（白名单；服务层已校验过一次，这里再兜一层）。
var workflowOrder = map[string]string{
	"id": "dbt_sql_workflow.id", "create_time": "dbt_sql_workflow.create_time", "status": "dbt_sql_workflow.status", "name": "dbt_sql_workflow.workflow_name",
	"instance": "dbt_sql_instance.instance_name", "db": "dbt_sql_workflow.db_name", "engineer": "dbt_sql_workflow.engineer", "group": "dbt_sql_workflow.group_name",
}

// workflowQuery 按筛选条件拼出工单查询（不含排序与分页）。ok=false 表示条件本身就不可能有结果。
func (r *SQLRepo) workflowQuery(f sqlflow.WorkflowFilter) (q *gorm.DB, ok bool) {
	q = r.DB.Table("dbt_sql_workflow").Joins("LEFT JOIN dbt_sql_instance ON dbt_sql_instance.id = dbt_sql_workflow.instance_id")
	switch f.Scope {
	case sqlflow.ScopeOwn:
		q = q.Where("dbt_sql_workflow.engineer = ?", f.Username)
	case sqlflow.ScopeGroups:
		if len(f.GroupIDs) == 0 {
			return nil, false
		}
		q = q.Where("dbt_sql_workflow.group_id IN ?", f.GroupIDs)
	}
	if f.Engineer != "" {
		q = q.Where("dbt_sql_workflow.engineer = ?", f.Engineer)
	}
	if len(f.IDs) > 0 {
		q = q.Where("dbt_sql_workflow.id IN ?", f.IDs)
	}
	if len(f.Statuses) > 0 {
		q = q.Where("dbt_sql_workflow.status IN ?", f.Statuses)
	}
	if f.Status != "" {
		q = q.Where("dbt_sql_workflow.status = ?", f.Status)
	}
	if f.InstanceID != 0 {
		// 批量工单的 instance_id 只记首个实例，真实范围在 batch_instance_ids 里；需整值匹配，避免 42 命中 142/420
		if f.Batch == "0" {
			q = q.Where("dbt_sql_workflow.instance_id = ?", f.InstanceID)
		} else {
			q = q.Where("(dbt_sql_workflow.instance_id = ? OR dbt_sql_workflow.batch_instance_ids REGEXP ?)", f.InstanceID, fmt.Sprintf("(^|,)%d(,|$)", f.InstanceID))
		}
	}
	switch f.Kind {
	case "export":
		q = q.Where("dbt_sql_workflow.is_offline_export = 1")
	case "change":
		q = q.Where("dbt_sql_workflow.is_offline_export = 0")
	}
	switch f.Batch {
	case "1":
		q = q.Where("(dbt_sql_workflow.batch_instance_ids IS NOT NULL AND dbt_sql_workflow.batch_instance_ids <> '')")
	case "0":
		q = q.Where("(dbt_sql_workflow.batch_instance_ids IS NULL OR dbt_sql_workflow.batch_instance_ids = '')")
	}
	if f.GroupID != 0 {
		q = q.Where("dbt_sql_workflow.group_id = ?", f.GroupID)
	}
	if len(f.SyntaxTypes) > 0 {
		q = q.Where("dbt_sql_workflow.syntax_type IN ?", f.SyntaxTypes)
	}
	if f.Search != "" {
		// 工单名称、提交人（显示名 / 登录名）、库、实例名，数字（可带 #）还按工单号精确匹配
		p := likePattern(f.Search)
		cond := "dbt_sql_workflow.engineer_display LIKE ? OR dbt_sql_workflow.workflow_name LIKE ? OR dbt_sql_workflow.engineer LIKE ? OR dbt_sql_workflow.db_name LIKE ? OR dbt_sql_instance.instance_name LIKE ?"
		args := []any{p, p, p, p, p}
		if id, isID := sqlflow.SearchID(f.Search); isID {
			cond += " OR dbt_sql_workflow.id = ?"
			args = append(args, id)
		}
		q = q.Where("("+cond+")", args...)
	}
	if f.StartDate != nil {
		q = q.Where("dbt_sql_workflow.create_time >= ?", *f.StartDate)
	}
	if f.EndDate != nil {
		q = q.Where("dbt_sql_workflow.create_time < ?", *f.EndDate)
	}
	return q, true
}

func (r *SQLRepo) ListWorkflows(f sqlflow.WorkflowFilter) ([]sqlflow.WorkflowRow, int64, error) {
	q, ok := r.workflowQuery(f)
	if !ok {
		return nil, 0, nil
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	order := "dbt_sql_workflow.id DESC"
	if col, found := workflowOrder[f.OrderBy]; found && f.OrderBy != "" {
		dir := "ASC"
		if f.Desc {
			dir = "DESC"
		}
		order = col + " " + dir
		if f.OrderBy != "id" {
			order += ", dbt_sql_workflow.id DESC"
		}
	}
	var rows []sqlflow.WorkflowRow
	err := q.Select("dbt_sql_workflow.*, dbt_sql_instance.instance_name AS instance_name").
		Order(order).Offset(f.Offset).Limit(f.Limit).Scan(&rows).Error
	return rows, total, err
}

// StatusCounts 按状态统计符合条件的工单数（忽略 Status / Statuses / 排序 / 分页）。
func (r *SQLRepo) StatusCounts(f sqlflow.WorkflowFilter) (map[string]int64, error) {
	f.Status, f.Statuses = "", nil
	q, ok := r.workflowQuery(f)
	if !ok {
		return map[string]int64{}, nil
	}
	var rows []struct {
		Status string
		N      int64
	}
	if err := q.Select("dbt_sql_workflow.status AS status, COUNT(*) AS n").Group("dbt_sql_workflow.status").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, x := range rows {
		out[x.Status] = x.N
	}
	return out, nil
}

// InstanceEnvs 一次查出一批实例的环境（旁路表 dbt_instance_ext；表不存在返回空集合）。
func (r *SQLRepo) InstanceEnvs(ids []uint) (map[uint]string, error) {
	return instext.Envs(r.DB, ids)
}

// DownloadCount 统计导出工单文件被成功下载的次数：dbt_audit_log 里“离线下载”且不带 error 的记录（见 sqlflow.Download 的写法）。
func (r *SQLRepo) DownloadCount(workflowID uint) (int, error) {
	var n int64
	err := r.DB.Table("dbt_audit_log").
		Where("action = ? AND extra_info LIKE ? AND extra_info NOT LIKE ?", "离线下载", fmt.Sprintf("工单id：%d，文件：%%", workflowID), "%error:%"). // i18n-ignore
		Count(&n).Error
	return int(n), err
}

func (r *SQLRepo) ExecutingDDL(dbName string, excludeID uint) ([]model.SqlWorkflow, error) {
	var out []model.SqlWorkflow
	err := r.DB.Where("status = ? AND db_name = ? AND syntax_type = ? AND id <> ?", model.WfExecuting, dbName, sqlflow.SyntaxDDL, excludeID).Find(&out).Error
	return out, err
}

// ---- 定时计划 ----

// ScheduleTableReady 判断 dbt_sql_schedule 表是否已创建（需要手动执行 deploy/sql/002_sql_schedule.sql）。
func (r *SQLRepo) ScheduleTableReady() bool {
	return r.DB.Migrator().HasTable(&model.SqlSchedule{})
}

func (r *SQLRepo) UpsertSchedule(s *model.SqlSchedule) error {
	return r.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "workflow_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"run_at", "created_by", "executor"}),
	}).Create(s).Error
}

func (r *SQLRepo) GetSchedule(workflowID uint) (*model.SqlSchedule, error) {
	var s model.SqlSchedule
	if err := r.DB.Where("workflow_id = ?", workflowID).First(&s).Error; err != nil {
		return nil, notFoundToNil(err)
	}
	return &s, nil
}

func (r *SQLRepo) DeleteSchedule(workflowID uint) error {
	if !r.ScheduleTableReady() {
		return nil // 还没建表就不可能有计划
	}
	return r.DB.Where("workflow_id = ?", workflowID).Delete(&model.SqlSchedule{}).Error
}

func (r *SQLRepo) ClaimSchedule(workflowID uint) (bool, error) {
	res := r.DB.Where("workflow_id = ?", workflowID).Delete(&model.SqlSchedule{})
	return res.RowsAffected > 0, res.Error
}

func (r *SQLRepo) DueSchedules(now time.Time, limit int) ([]model.SqlSchedule, error) {
	var out []model.SqlSchedule
	err := r.DB.Where("run_at <= ?", now).Order("run_at").Limit(limit).Find(&out).Error
	return out, err
}

func (r *SQLRepo) InsertAuditEntry(e *model.AuditEntry) error { return r.DB.Create(e).Error }

// WriteLifecycleAudit 写审批类动作（提交 / 审批 / 驳回 / 执行 / 定时…）的系统审计（audit.LifecycleWriter）。
func (r *SQLRepo) WriteLifecycleAudit(e *model.AuditEntry) error { return r.DB.Create(e).Error }

// ---- 数据导出扩展：导出原值（不脱敏）----

// ExportExtReady 判断 dbt_sql_export_ext 表是否已创建（需要执行 deploy/sql/290_export_unmasked.sql）。
func (r *SQLRepo) ExportExtReady() bool {
	return r.DB.Migrator().HasTable(&model.SqlExportExt{})
}

// ExportMergeReady 判断 dbt_sql_export_ext 是否已有 merge_files 列（旧版脚本建的表没有，要重新执行 290_export_unmasked.sql 补上）。
func (r *SQLRepo) ExportMergeReady() bool {
	m := r.DB.Migrator()
	return m.HasTable(&model.SqlExportExt{}) && m.HasColumn(&model.SqlExportExt{}, "merge_files")
}

func (r *SQLRepo) UpsertExportExt(e *model.SqlExportExt) error {
	cols := []string{"unmasked", "reason", "set_by"}
	db := r.DB
	if r.ExportMergeReady() {
		cols = append(cols, "merge_files")
	} else {
		db = db.Omit("merge_files") // 旧表没有这一列：只写旧的列，不让导出原值也跟着失败
	}
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "workflow_id"}},
		DoUpdates: clause.AssignmentColumns(cols),
	}).Create(e).Error
}

func (r *SQLRepo) GetExportExt(workflowID uint) (*model.SqlExportExt, error) {
	if !r.ExportExtReady() {
		return nil, nil
	}
	var e model.SqlExportExt
	if err := r.DB.Where("workflow_id = ?", workflowID).First(&e).Error; err != nil {
		return nil, notFoundToNil(err)
	}
	return &e, nil
}

// ExportUnmaskedIDs 实现 sqlflow.ExportUnmaskedLoader：一次查出一批工单里选择了导出原值的。
func (r *SQLRepo) ExportUnmaskedIDs(ids []uint) (map[uint]bool, error) {
	out := map[uint]bool{}
	if len(ids) == 0 || !r.ExportExtReady() {
		return out, nil
	}
	var rows []model.SqlExportExt
	if err := r.DB.Where("workflow_id IN ? AND unmasked = ?", ids, true).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, e := range rows {
		out[e.WorkflowID] = true
	}
	return out, nil
}

// UserIsSuperuser 查登录名对应的启用用户当前是不是超级用户；用户不存在或已停用返回 false。
func (r *SQLRepo) UserIsSuperuser(username string) (bool, error) {
	var n int64
	err := r.DB.Model(&model.User{}).Where("username = ? AND is_active = ? AND is_superuser = ?", username, true, true).Count(&n).Error
	return n > 0, err
}
