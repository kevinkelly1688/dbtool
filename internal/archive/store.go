// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package archive

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"dbtool/internal/crypto"
	"dbtool/internal/model"
	"dbtool/internal/store"
)

// ErrVersion 表示保存任务时版本号不一致（别人先改过）。
var ErrVersion = errors.New("archive: task version conflict")

// gormRepo 是 Repo 的 GORM 实现。审批相关的 audit.Repo 方法委托给 store.Store（与其他模块共用同一套审批流存储）。
type gormRepo struct {
	db     *gorm.DB
	st     *store.Store
	mirage *crypto.Mirage

	mu      *sync.Mutex
	ready   *bool
	checked *time.Time
}

func newGormRepo(db *gorm.DB, m *crypto.Mirage) *gormRepo {
	var ready bool
	var checked time.Time
	return &gormRepo{db: db, st: &store.Store{DB: db, Mirage: m}, mirage: m, mu: &sync.Mutex{}, ready: &ready, checked: &checked}
}

var _ Repo = (*gormRepo)(nil)

func (r *gormRepo) tx(db *gorm.DB) *gormRepo {
	c := *r
	c.db = db
	c.st = &store.Store{DB: db, Mirage: r.mirage}
	return &c
}

func (r *gormRepo) InTx(fn func(Repo) error) error {
	return r.db.Transaction(func(tx *gorm.DB) error { return fn(r.tx(tx)) })
}

// Ready 检查 dbt_archive_task 是否存在、且已有 time_unit 列（老库升级时 180_archive.sql 会补）；检查通过后不再查，没通过每 30 秒重查一次（执行 SQL 脚本后无需重启）。
func (r *gormRepo) Ready() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if *r.ready {
		return true
	}
	if !r.checked.IsZero() && time.Since(*r.checked) < 30*time.Second {
		return false
	}
	*r.checked = time.Now()
	var n int64
	err := r.db.Raw("SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'dbt_archive_task' AND COLUMN_NAME = 'time_unit'").Scan(&n).Error
	*r.ready = err == nil && n > 0
	return *r.ready
}

// ---- audit.Repo（委托）----

func (r *gormRepo) AuditSetting(g uint, t int) (*model.WorkflowAuditSetting, error) {
	return r.st.AuditSetting(g, t)
}
func (r *gormRepo) AuthGroup(id uint) (*model.AuthGroup, error) { return r.st.AuthGroup(id) }
func (r *gormRepo) UserInAuthGroup(u, g uint) (bool, error)     { return r.st.UserInAuthGroup(u, g) }
func (r *gormRepo) AuthGroupUsers(names []string, rg uint) ([]model.User, error) {
	return r.st.AuthGroupUsers(names, rg)
}
func (r *gormRepo) InsertAudit(a *model.WorkflowAudit) error { return r.st.InsertAudit(a) }
func (r *gormRepo) SaveAudit(a *model.WorkflowAudit) error   { return r.st.SaveAudit(a) }
func (r *gormRepo) InsertDetail(d *model.WorkflowAuditDetail) error {
	return r.st.InsertDetail(d)
}
func (r *gormRepo) InsertLog(l *model.WorkflowLog) error { return r.st.InsertLog(l) }
func (r *gormRepo) HasButton(uid uint, code string) (bool, error) {
	return r.st.HasButton(uid, code)
}
func (r *gormRepo) ConfigValue(item string) (string, bool) { return r.st.ConfigValue(item) }

func (r *gormRepo) UserResourceGroups(userID uint, all bool) ([]model.ResourceGroup, error) {
	return r.st.UserResourceGroups(userID, all)
}
func (r *gormRepo) ResourceGroupByID(id uint) (*model.ResourceGroup, error) {
	return r.st.ResourceGroupByID(id)
}
func (r *gormRepo) AuditByWorkflow(t int, id uint, forUpdate bool) (*model.WorkflowAudit, error) {
	return r.st.AuditByWorkflow(t, id, forUpdate)
}
func (r *gormRepo) AuditLogs(auditID uint) ([]model.WorkflowLog, error) {
	return r.st.AuditLogs(auditID)
}

func (r *gormRepo) AuditDetails(auditID uint) ([]model.WorkflowAuditDetail, error) {
	var out []model.WorkflowAuditDetail
	err := r.db.Where("audit_id = ?", auditID).Order("audit_detail_id").Find(&out).Error
	return out, err
}

func (r *gormRepo) InstanceResourceGroupIDs(instanceID uint) ([]uint, error) {
	var ids []uint
	err := r.db.Raw(`SELECT ir.resourcegroup_id FROM dbt_sql_instance_resource_group ir
JOIN dbt_resource_group rg ON rg.group_id = ir.resourcegroup_id AND rg.is_deleted = 0 WHERE ir.instance_id = ? ORDER BY ir.resourcegroup_id`, instanceID).Scan(&ids).Error
	return ids, err
}

var openWfStatus = []string{model.WfReviewPass, model.WfTimingTask, model.WfQueuing, model.WfExecuting}

func (r *gormRepo) OpenWorkflows(instanceID uint, db, table string) ([]OpenWorkflow, error) {
	var rows []struct {
		ID           uint
		WorkflowName string
		Status       string
		SQLContent   string
	}
	err := r.db.Raw(`SELECT w.id AS id, w.workflow_name AS workflow_name, w.status AS status, SUBSTRING(c.sql_content, 1, 50000) AS sql_content
FROM dbt_sql_workflow w JOIN dbt_sql_workflow_content c ON c.workflow_id = w.id
WHERE w.instance_id = ? AND w.db_name = ? AND w.status IN ? AND c.sql_content LIKE ? ORDER BY w.id LIMIT 200`,
		instanceID, db, openWfStatus, "%"+likeEscape(table)+"%").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(`(?i)(^|[^A-Za-z0-9_$])` + regexp.QuoteMeta(table) + `($|[^A-Za-z0-9_$])`)
	if err != nil {
		return nil, err
	}
	var out []OpenWorkflow
	for _, x := range rows {
		if re.MatchString(x.SQLContent) {
			out = append(out, OpenWorkflow{ID: x.ID, Name: x.WorkflowName, Status: x.Status})
		}
	}
	return out, nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (r *gormRepo) WriteAudit(e *model.AuditEntry) error { return r.db.Create(e).Error }

// ---- 任务 ----

func (r *gormRepo) InsertTask(t *Task) error { t.ID = 0; return r.db.Create(t).Error }

func (r *gormRepo) SaveTask(t *Task) error {
	old := t.Version
	t.Version = old + 1
	res := r.db.Model(&Task{}).Where("id = ? AND version = ?", t.ID, old).Select("*").Omit("id", "created_at").Updates(t)
	if res.Error != nil {
		t.Version = old
		return res.Error
	}
	if res.RowsAffected == 0 {
		t.Version = old
		return ErrVersion
	}
	return nil
}

func (r *gormRepo) GetTask(id uint, forUpdate bool) (*Task, error) {
	q := r.db
	if forUpdate {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var t Task
	err := q.Where("id = ?", id).Take(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &t, err
}

func (r *gormRepo) taskQuery(f TaskFilter) *gorm.DB {
	q := r.db.Model(&Task{})
	if !f.AllGroups {
		if len(f.ResourceGroupIDs) == 0 {
			return q.Where("1 = 0")
		}
		q = q.Where("resource_group_id IN ?", f.ResourceGroupIDs)
	}
	if len(f.Statuses) > 0 {
		q = q.Where("status IN ?", f.Statuses)
	}
	if f.Mode != "" {
		q = q.Where("mode = ?", f.Mode)
	}
	if f.InstanceID != 0 {
		q = q.Where("instance_id = ?", f.InstanceID)
	}
	if len(f.IDs) > 0 {
		q = q.Where("id IN ?", f.IDs)
	}
	if f.Q != "" {
		like := "%" + likeEscape(f.Q) + "%"
		q = q.Where("(name LIKE ? OR db_name LIKE ? OR tbl_name LIKE ? OR CONCAT(db_name, '.', tbl_name) LIKE ? OR instance_id IN (SELECT id FROM dbt_sql_instance WHERE instance_name LIKE ?))", like, like, like, like, like)
	}
	return q
}

func (r *gormRepo) ListTasks(f TaskFilter) ([]Task, int64, error) {
	q := r.taskQuery(f).Session(&gorm.Session{})
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	q = q.Order("id")
	if f.Limit > 0 {
		q = q.Offset(f.Offset).Limit(f.Limit)
	}
	var out []Task
	err := q.Find(&out).Error
	return out, total, err
}

func (r *gormRepo) ActiveTaskOnTable(instanceID uint, db, table string, exceptID uint) (*Task, error) {
	q := r.db.Where("instance_id = ? AND db_name = ? AND tbl_name = ? AND status IN ?", instanceID, db, table, []string{StPending, StEnabled, StPaused})
	if exceptID != 0 {
		q = q.Where("id <> ?", exceptID)
	}
	var t Task
	err := q.Order("id").Take(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &t, err
}

func (r *gormRepo) TaskNameTaken(name string, exceptID uint) (bool, error) {
	var n int64
	q := r.db.Model(&Task{}).Where("name = ?", name)
	if exceptID != 0 {
		q = q.Where("id <> ?", exceptID)
	}
	err := q.Count(&n).Error
	return n > 0, err
}

func (r *gormRepo) DeleteTask(id uint) error { return r.db.Where("id = ?", id).Delete(&Task{}).Error }

func (r *gormRepo) UpdateCheckpoint(id uint, cp string) error {
	return r.db.Model(&Task{}).Where("id = ?", id).UpdateColumn("checkpoint", cp).Error
}

func (r *gormRepo) ClaimNextRun(id uint, old, nw *time.Time) (bool, error) {
	res := r.db.Model(&Task{}).Where("id = ? AND status = ? AND next_run_at <=> ?", id, StEnabled, old).UpdateColumn("next_run_at", nw)
	return res.RowsAffected > 0, res.Error
}

// ---- 运行 ----

func (r *gormRepo) InsertRun(x *Run) error { x.ID = 0; return r.db.Create(x).Error }

func (r *gormRepo) SaveRun(x *Run) error {
	return r.db.Model(&Run{}).Where("id = ?", x.ID).Select("*").Omit("id", "created_at").Updates(x).Error
}

func (r *gormRepo) GetRun(id uint) (*Run, error) {
	var x Run
	err := r.db.Where("id = ?", id).Take(&x).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &x, err
}

func (r *gormRepo) ListRuns(f RunFilter) ([]Run, int64, error) {
	q := r.db.Model(&Run{})
	if !f.AllGroups {
		if len(f.ResourceGroupIDs) == 0 {
			return nil, 0, nil
		}
		q = q.Where("resource_group_id IN ?", f.ResourceGroupIDs)
	}
	if f.TaskID != 0 {
		q = q.Where("task_id = ?", f.TaskID)
	}
	if f.Trigger != "" {
		q = q.Where("trigger_kind = ?", f.Trigger)
	}
	if f.Result != "" {
		q = q.Where("result = ?", f.Result)
	}
	if f.Q != "" {
		like := "%" + likeEscape(f.Q) + "%"
		q = q.Where("(task_name LIKE ? OR CONCAT(db_name, '.', tbl_name) LIKE ? OR by_user LIKE ? OR instance_name LIKE ?)", like, like, like, like)
	}
	q = q.Session(&gorm.Session{})
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	q = q.Order("id DESC")
	if f.Limit > 0 {
		q = q.Offset(f.Offset).Limit(f.Limit)
	}
	var out []Run
	err := q.Find(&out).Error
	return out, total, err
}

func (r *gormRepo) RecentRuns(taskID uint, n int) ([]Run, error) {
	var out []Run
	err := r.db.Where("task_id = ?", taskID).Order("id DESC").Limit(n).Find(&out).Error
	return out, err
}

func (r *gormRepo) RunningRuns(instanceID uint, leaseAfter time.Time) ([]Run, error) {
	q := r.db.Where("result IN ?", []string{ResRunning, ResPaused})
	if instanceID != 0 {
		q = q.Where("instance_id = ?", instanceID)
	}
	if !leaseAfter.IsZero() {
		q = q.Where("lease_until IS NOT NULL AND lease_until > ?", leaseAfter)
	}
	var out []Run
	err := q.Order("id").Find(&out).Error
	return out, err
}

func (r *gormRepo) RunsRestoredBy(runID uint) ([]uint, error) {
	var ids []uint
	err := r.db.Model(&Run{}).Where("restore_of = ?", runID).Order("id").Pluck("id", &ids).Error
	return ids, err
}

func (r *gormRepo) ExpireLeases(before, now time.Time) ([]Run, error) {
	var out []Run
	err := r.db.Where("result IN ? AND (lease_until IS NULL OR lease_until < ?)", []string{ResRunning, ResPaused}, before).Find(&out).Error
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Result, out[i].FinishedAt = ResInterrupted, &now
		if out[i].Note == "" {
			out[i].Note = "执行进程异常退出或心跳超时，运行被标记为中断；断点以最后一批为准，下次接着跑"
		}
		out[i].DurationMs = now.Sub(out[i].StartedAt).Milliseconds()
		if err := r.db.Model(&Run{}).Where("id = ? AND result IN ?", out[i].ID, []string{ResRunning, ResPaused}).
			Updates(map[string]any{"result": ResInterrupted, "finished_at": now, "note": out[i].Note, "duration_ms": out[i].DurationMs}).Error; err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---- 事件 / 采样 ----

func (r *gormRepo) InsertEvents(evs []Event) error {
	if len(evs) == 0 {
		return nil
	}
	return r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&evs).Error
}

func (r *gormRepo) ListEvents(runID uint, after uint, limit int) ([]Event, error) {
	var out []Event
	q := r.db.Where("run_id = ? AND seq > ?", runID, after).Order("seq")
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Find(&out).Error
	return out, err
}

func (r *gormRepo) TrimEvents(runID uint, keep int) error {
	var max sql.NullInt64
	if err := r.db.Model(&Event{}).Where("run_id = ?", runID).Select("MAX(seq)").Scan(&max).Error; err != nil {
		return err
	}
	if !max.Valid || int(max.Int64) <= keep {
		return nil
	}
	return r.db.Where("run_id = ? AND seq <= ?", runID, max.Int64-int64(keep)).Delete(&Event{}).Error
}

func (r *gormRepo) InsertBatchStats(runID uint, rows []BatchStat) error {
	if len(rows) == 0 {
		return nil
	}
	for i := range rows {
		rows[i].ID, rows[i].RunID = 0, runID
	}
	return r.db.Create(&rows).Error
}

func (r *gormRepo) ListBatchStats(runID uint) ([]BatchStat, error) {
	var out []BatchStat
	err := r.db.Where("run_id = ?", runID).Order("t_ms").Limit(2000).Find(&out).Error
	return out, err
}

// ---- 备份存储 ----

func (r *gormRepo) decrypt(s *StoreRow, withSK bool) {
	s.SKSet = s.SK != ""
	s.AK = r.mirage.Decrypt(s.AK)
	if withSK {
		s.SK = r.mirage.Decrypt(s.SK)
	} else {
		s.SK = ""
	}
}

func (r *gormRepo) ListStores() ([]StoreRow, error) {
	var out []StoreRow
	if err := r.db.Order("id").Find(&out).Error; err != nil {
		return nil, err
	}
	for i := range out {
		r.decrypt(&out[i], false)
	}
	return out, nil
}

func (r *gormRepo) GetStore(id uint) (*StoreRow, error) {
	var s StoreRow
	err := r.db.Where("id = ?", id).Take(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.decrypt(&s, true)
	return &s, nil
}

func (r *gormRepo) InsertStore(s *StoreRow) error {
	c := *s
	c.ID, c.AK, c.SK = 0, r.encrypt(s.AK), r.encrypt(s.SK)
	if err := r.db.Create(&c).Error; err != nil {
		return err
	}
	s.ID = c.ID
	return nil
}

func (r *gormRepo) encrypt(v string) string {
	if v == "" {
		return ""
	}
	return r.mirage.Encrypt(v)
}

func (r *gormRepo) SaveStore(s *StoreRow, keepSK bool) error {
	c := *s
	c.AK, c.SK = r.encrypt(s.AK), r.encrypt(s.SK)
	q := r.db.Model(&StoreRow{}).Where("id = ?", s.ID).Select("*").Omit("id", "created_at")
	if keepSK {
		q = q.Omit("sk", "id", "created_at")
	}
	return q.Updates(&c).Error
}

func (r *gormRepo) DeleteStore(id uint) error {
	return r.db.Where("id = ?", id).Delete(&StoreRow{}).Error
}

// StoreRefCount 统计引用该存储的任务数（dest_file JSON 里的 store_id）。任务数不会很大，在内存里解析。
func (r *gormRepo) StoreRefCount(id uint) (int, error) {
	var rows []Task
	if err := r.db.Select("id", "dest_file").Where("dest_file IS NOT NULL AND dest_file <> ''").Find(&rows).Error; err != nil {
		return 0, err
	}
	n := 0
	for i := range rows {
		if c := rows[i].cfg(); c.DestFile != nil && c.DestFile.StoreID == id {
			n++
		}
	}
	return n, nil
}

// ---- 策略 / 试运行 ----

func (r *gormRepo) GetPolicy() (*PolicyRow, error) {
	var p PolicyRow
	err := r.db.Where("id = 1").Take(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &p, err
}

func (r *gormRepo) SavePolicy(p *PolicyRow) error {
	p.ID = 1
	return r.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(p).Error
}

func (r *gormRepo) InsertCheck(c *CheckRow) error { c.ID = 0; return r.db.Create(c).Error }

func (r *gormRepo) GetCheckByToken(token string) (*CheckRow, error) {
	var c CheckRow
	err := r.db.Where("token = ?", token).Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &c, err
}

func (r *gormRepo) PurgeChecks(before time.Time) error {
	return r.db.Where("expires_at < ?", before).Delete(&CheckRow{}).Error
}

// TryLock 用 MySQL 的 GET_LOCK 做跨进程互斥：锁属于一条连接，所以占用一条连接直到 release。
func (r *gormRepo) TryLock(ctx context.Context, name string) (func(), bool, error) {
	sqlDB, err := r.db.DB()
	if err != nil {
		return nil, false, err
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", name).Scan(&got); err != nil {
		conn.Close()
		return nil, false, err
	}
	if !got.Valid || got.Int64 != 1 {
		conn.Close()
		return nil, false, nil
	}
	return func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(c, "DO RELEASE_LOCK(?)", name)
		conn.Close()
	}, true, nil
}
