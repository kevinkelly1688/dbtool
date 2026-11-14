// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package retention

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"dbtool/internal/archive/engine"
	"dbtool/internal/crypto"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// gormRepo 是 Repo 的 GORM 实现。表名 / 列名只来自 targets 里的常量，取值全部参数化。
type gormRepo struct {
	db     *gorm.DB
	mirage *crypto.Mirage
}

// NewGormRepo 创建生产用的 Repo。
func NewGormRepo(db *gorm.DB, m *crypto.Mirage) Repo { return &gormRepo{db: db, mirage: m} }

func q(ident string) string { return "`" + strings.ReplaceAll(ident, "`", "``") + "`" }

func (r *gormRepo) ConfigValue(item string) (string, bool) {
	var ci settings.Cipher
	if r.mirage != nil {
		ci = r.mirage
	}
	return settings.Open(r.db, ci).Value(item)
}

func (r *gormRepo) TableState(table string, cols ...string) (TableState, error) {
	var have []string
	err := r.db.Raw("SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?", table).Scan(&have).Error
	if err != nil {
		return TableState{}, err
	}
	if len(have) == 0 {
		return TableState{}, nil
	}
	set := make(map[string]bool, len(have))
	for _, c := range have {
		set[strings.ToLower(c)] = true
	}
	st := TableState{Exists: true}
	for _, c := range cols {
		if !set[strings.ToLower(c)] {
			st.Missing = append(st.Missing, c)
		}
	}
	return st, nil
}

// whereOlder 生成“时间早于 cutoff [AND 固定条件]”。
func whereOlder(t Target) string {
	w := q(t.TimeCol) + " < ?"
	if t.Where != "" {
		w += " AND (" + t.Where + ")"
	}
	return w
}

func countSQL(t Target) string {
	// MAX_EXECUTION_TIME 提示：预览在大表上最多算 20 秒（MariaDB 忽略该提示）。
	return "SELECT /*+ MAX_EXECUTION_TIME(20000) */ COUNT(*) FROM " + q(t.Table) + " WHERE " + whereOlder(t)
}

func selectIDsSQL(t Target) string {
	return "SELECT " + q(t.IDCol) + " FROM " + q(t.Table) + " WHERE " + whereOlder(t) + " ORDER BY " + q(t.IDCol) + " LIMIT ?"
}

func (r *gormRepo) CountOlder(ctx context.Context, t Target, cutoff time.Time) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Raw(countSQL(t), cutoff).Scan(&n).Error
	return n, err
}

func (r *gormRepo) DeleteBatch(ctx context.Context, t Target, cutoff time.Time, limit int) (int, int64, error) {
	if limit <= 0 || limit > maxBatchSize {
		limit = maxBatchSize
	}
	var ids []uint64
	if err := r.db.WithContext(ctx).Raw(selectIDsSQL(t), cutoff, limit).Scan(&ids).Error; err != nil {
		return 0, 0, err
	}
	if len(ids) == 0 {
		return 0, 0, nil
	}
	var deleted int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, c := range t.Children {
			if err := tx.Exec("DELETE FROM "+q(c.Table)+" WHERE "+q(c.FKCol)+" IN ?", ids).Error; err != nil {
				return fmt.Errorf("删除子表 %s: %w", c.Table, err)
			}
		}
		res := tx.Exec("DELETE FROM "+q(t.Table)+" WHERE "+q(t.IDCol)+" IN ?", ids)
		if res.Error != nil {
			return res.Error
		}
		deleted = res.RowsAffected
		return nil
	})
	if err != nil {
		return len(ids), 0, err
	}
	return len(ids), deleted, nil
}

// TryLock 用 GET_LOCK 做跨进程互斥：锁属于一条连接，所以占用一条连接直到 release。
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

// ---- 导出文件 ----

func (r *gormRepo) ExportMarkerReady() (bool, error) {
	st, err := r.TableState("dbt_export_purged", "workflow_id", "file_name", "size_bytes", "existed", "purged_at")
	return st.OK(), err
}

type exportRow struct {
	ID       uint      `gorm:"column:id"`
	FileName string    `gorm:"column:file_name"`
	RefTime  time.Time `gorm:"column:ref_time"`
}

const expiredExportsSQL = "SELECT w.id AS id, w.file_name AS file_name, COALESCE(w.finish_time, w.create_time) AS ref_time FROM dbt_sql_workflow w" +
	" WHERE w.is_offline_export = 1 AND w.status IN ('workflow_finish','workflow_exception')" +
	" AND w.file_name IS NOT NULL AND w.file_name <> ''" +
	" AND (w.finish_time < ? OR (w.finish_time IS NULL AND w.create_time < ?))" +
	" AND w.id > ? AND NOT EXISTS (SELECT 1 FROM dbt_export_purged p WHERE p.workflow_id = w.id)" +
	" ORDER BY w.id LIMIT ?"

func (r *gormRepo) ExpiredExports(ctx context.Context, cutoff time.Time, afterID uint, limit int) ([]ExportFile, error) {
	var rows []exportRow
	if err := r.db.WithContext(ctx).Raw(expiredExportsSQL, cutoff, cutoff, afterID, limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ExportFile, len(rows))
	for i, x := range rows {
		out[i] = ExportFile{WorkflowID: x.ID, FileName: x.FileName, RefTime: x.RefTime}
	}
	return out, nil
}

func (r *gormRepo) MarkExportPurged(ctx context.Context, p ExportPurge) error {
	return r.db.WithContext(ctx).Exec("INSERT INTO dbt_export_purged (workflow_id, file_name, size_bytes, existed, purged_at) VALUES (?,?,?,?,?)"+
		" ON DUPLICATE KEY UPDATE file_name = VALUES(file_name), size_bytes = VALUES(size_bytes), existed = VALUES(existed), purged_at = VALUES(purged_at)",
		p.WorkflowID, p.FileName, p.SizeBytes, p.Existed, p.PurgedAt).Error
}

// ExportPurgedAt 返回某导出工单的文件被清理的时间；没被清理（或标记表不存在）返回 ok=false。
// 供工单详情 / 下载接口把“文件不存在”换成“文件已按保留期清理”。
func ExportPurgedAt(db *gorm.DB, workflowID uint) (at time.Time, ok bool) {
	var t sql.NullTime
	row := db.Raw("SELECT purged_at FROM dbt_export_purged WHERE workflow_id = ?", workflowID).Row()
	if row == nil || row.Scan(&t) != nil || !t.Valid {
		return time.Time{}, false
	}
	return t.Time, true
}

// ---- 归档文件备份 ----

type storeRow struct {
	ID            uint   `gorm:"column:id"`
	Name          string `gorm:"column:name"`
	Type          string `gorm:"column:type"`
	Path          string `gorm:"column:path"`
	Endpoint      string `gorm:"column:endpoint"`
	Region        string `gorm:"column:region"`
	Bucket        string `gorm:"column:bucket"`
	Prefix        string `gorm:"column:prefix"`
	AK            string `gorm:"column:ak"`
	SK            string `gorm:"column:sk"`
	PathStyle     bool   `gorm:"column:path_style"`
	RetentionDays int    `gorm:"column:retention_days"`
}

func (r *gormRepo) decrypt(s string) string {
	if r.mirage == nil || s == "" {
		return s
	}
	return r.mirage.Decrypt(s)
}

func (r *gormRepo) BackupStores(ctx context.Context) ([]BackupStore, error) {
	var rows []storeRow
	err := r.db.WithContext(ctx).Raw("SELECT id, name, type, path, endpoint, region, bucket, prefix, ak, sk, path_style, retention_days" +
		" FROM dbt_archive_store WHERE retention_days > 0 ORDER BY id").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]BackupStore, len(rows))
	for i, x := range rows {
		out[i] = BackupStore{ID: x.ID, Name: x.Name, RetentionDays: x.RetentionDays, Config: engine.StoreConfig{
			ID: x.ID, Name: x.Name, Type: x.Type, Path: x.Path, Endpoint: x.Endpoint, Region: x.Region, Bucket: x.Bucket, Prefix: x.Prefix,
			AK: r.decrypt(x.AK), SK: r.decrypt(x.SK), PathStyle: x.PathStyle, RetentionDays: x.RetentionDays}}
	}
	return out, nil
}

type backupRunRow struct {
	ID         uint       `gorm:"column:id"`
	TaskName   string     `gorm:"column:task_name"`
	StartedAt  time.Time  `gorm:"column:started_at"`
	FinishedAt *time.Time `gorm:"column:finished_at"`
	Manifest   string     `gorm:"column:manifest"`
	Backups    string     `gorm:"column:backups"`
}

const backupRunsSQL = "SELECT id, task_name, started_at, finished_at, manifest, backups FROM dbt_archive_run" +
	" WHERE id > ? AND result NOT IN ('running','paused') AND trigger_kind NOT IN ('restore','dryrun')" +
	" AND manifest IS NOT NULL AND manifest <> '' AND (note IS NULL OR note NOT LIKE '" + BackupPurgedPrefix + "%')" +
	" AND started_at < ? ORDER BY id LIMIT ?"

func (r *gormRepo) BackupRuns(ctx context.Context, before time.Time, afterID uint, limit int) ([]BackupRun, error) {
	var rows []backupRunRow
	if err := r.db.WithContext(ctx).Raw(backupRunsSQL, afterID, before, limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]BackupRun, 0, len(rows))
	for _, x := range rows {
		ref := x.StartedAt
		if x.FinishedAt != nil {
			ref = *x.FinishedAt
		}
		br := BackupRun{ID: x.ID, RefTime: ref, TaskName: x.TaskName}
		br.Keys, br.Bytes = manifestKeys(x.Manifest)
		br.StoreID = fileStoreOf(x.Backups)
		out = append(out, br)
	}
	return out, nil
}

// manifestKeys 取清单记录的全部对象 Key（含清单自身）与总字节数。解析失败返回空：没有清单就不删任何东西。
func manifestKeys(manifest string) (keys []string, bytes int64) {
	var m engine.Manifest
	if json.Unmarshal([]byte(manifest), &m) != nil {
		return nil, 0
	}
	for _, f := range m.Files {
		keys = append(keys, f.Key)
		bytes += f.Size
	}
	if m.ManifestKey != "" {
		keys = append(keys, m.ManifestKey)
	}
	return keys, bytes
}

// fileStoreOf 从运行记录的 backups 里取文件备份用的存储 ID；取不到返回 0。
func fileStoreOf(backups string) uint {
	if strings.TrimSpace(backups) == "" {
		return 0
	}
	var recs []struct {
		Kind    string `json:"kind"`
		StoreID uint   `json:"store_id"`
	}
	if json.Unmarshal([]byte(backups), &recs) != nil {
		return 0
	}
	for _, x := range recs {
		if x.Kind == "file" && x.StoreID > 0 {
			return x.StoreID
		}
	}
	return 0
}

const markBackupPurgedSQL = "UPDATE dbt_archive_run SET note = LEFT(CONCAT(?, IF(note IS NULL OR note = '', '', CONCAT('；', note))), 500) WHERE id = ?"

func (r *gormRepo) MarkBackupPurged(ctx context.Context, runID uint, note string) error {
	return r.db.WithContext(ctx).Exec(markBackupPurgedSQL, note, runID).Error
}

// ---- 审计与汇总 ----

func (r *gormRepo) InsertAudit(e *model.AuditEntry) error { return r.db.Create(e).Error }

type roundRow struct {
	ID         uint      `gorm:"column:id;primaryKey"`
	StartedAt  time.Time `gorm:"column:started_at"`
	FinishedAt time.Time `gorm:"column:finished_at"`
	TriggerBy  string    `gorm:"column:trigger_by"`
	Status     string    `gorm:"column:status"`
	Deleted    int64     `gorm:"column:deleted"`
	Summary    string    `gorm:"column:summary"`
}

func (roundRow) TableName() string { return "dbt_retention_run" }

var errNoRoundTable = errors.New("dbt_retention_run 不存在（执行 deploy/sql/205_retention.sql 后可在接口里看到每轮汇总）")

func (r *gormRepo) SaveRound(rec *RoundRecord) error {
	st, err := r.TableState("dbt_retention_run")
	if err != nil {
		return err
	}
	if !st.Exists {
		return errNoRoundTable
	}
	row := roundRow{StartedAt: rec.StartedAt, FinishedAt: rec.FinishedAt, TriggerBy: rec.Trigger, Status: rec.Status, Deleted: rec.Deleted, Summary: rec.Summary}
	if err := r.db.Create(&row).Error; err != nil {
		return err
	}
	rec.ID = row.ID
	return nil
}

func (r *gormRepo) ListRounds(limit int) ([]RoundRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	st, err := r.TableState("dbt_retention_run")
	if err != nil {
		return nil, err
	}
	if !st.Exists {
		return nil, nil
	}
	var rows []roundRow
	if err := r.db.Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]RoundRecord, len(rows))
	for i, x := range rows {
		out[i] = RoundRecord{ID: x.ID, StartedAt: x.StartedAt, FinishedAt: x.FinishedAt, Trigger: x.TriggerBy, Status: x.Status, Deleted: x.Deleted, Summary: x.Summary}
	}
	return out, nil
}
