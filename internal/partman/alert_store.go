// @author: kevinkelly
// @license: Apache Licence
// @file: alert_store.go
// @time: 2026/10/28

package partman

import (
	"errors"
	"time"

	drv "github.com/go-sql-driver/mysql"
	"gorm.io/gorm/clause"
)

// store 对 AlertRepo 的实现。dbt_partman_alert / dbt_partman_alert_log 是 deploy/sql/280_partman_alert.sql 建的扩展表，
// 表不存在（MySQL 1146）时各方法返回 ErrAlertNotReady，调用方据此降级。

var _ AlertRepo = (*store)(nil)

// alertErr 把“表不存在”换成 ErrAlertNotReady。
func alertErr(err error) error {
	var me *drv.MySQLError
	if errors.As(err, &me) && me.Number == 1146 {
		return ErrAlertNotReady
	}
	return err
}

func (r *store) AlertReady() bool {
	m := r.db.Migrator()
	return m.HasTable(&AlertSetting{}) && m.HasTable(&AlertLog{})
}

func (r *store) ListAlertSettings(ids []int64) (map[int64]AlertSetting, error) {
	out := map[int64]AlertSetting{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []AlertSetting
	if err := r.db.Where("cnf_id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, alertErr(err)
	}
	for _, a := range rows {
		out[a.CnfID] = a
	}
	return out, nil
}

// upsertAlert 插入一行，已有时只更新 cols 这几列（其余列保持原样）。
func (r *store) upsertAlert(row AlertSetting, cols ...string) error {
	err := r.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "cnf_id"}}, DoUpdates: clause.AssignmentColumns(cols)}).Create(&row).Error
	return alertErr(err)
}

func (r *store) SetAlertThreshold(cnfID int64, threshold *int) error {
	return r.upsertAlert(AlertSetting{CnfID: cnfID, Threshold: threshold}, "threshold")
}

func (r *store) SetAlertSilence(cnfID int64, until *time.Time) error {
	return r.upsertAlert(AlertSetting{CnfID: cnfID, SilenceUntil: until}, "silence_until")
}

func (r *store) SaveAlertState(cnfID int64, alerting bool, lastAlertAt *time.Time) error {
	if lastAlertAt != nil {
		return r.upsertAlert(AlertSetting{CnfID: cnfID, Alerting: alerting, LastAlertAt: lastAlertAt}, "alerting", "last_alert_at")
	}
	return r.upsertAlert(AlertSetting{CnfID: cnfID, Alerting: alerting}, "alerting")
}

func (r *store) InsertAlertLog(l *AlertLog) error { return alertErr(r.db.Create(l).Error) }

func (r *store) ListAlertLogs(cnfIDs []int64, offset, limit int) ([]AlertLog, int64, error) {
	if len(cnfIDs) == 0 {
		return nil, 0, nil
	}
	q := r.db.Model(&AlertLog{}).Where("cnf_id IN ?", cnfIDs)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, alertErr(err)
	}
	var out []AlertLog
	err := q.Order("id DESC").Offset(offset).Limit(limit).Find(&out).Error
	return out, total, alertErr(err)
}

func (r *store) DeleteAlertData(cnfID int64) error {
	if err := r.db.Where("cnf_id = ?", cnfID).Delete(&AlertSetting{}).Error; err != nil {
		return alertErr(err)
	}
	return alertErr(r.db.Where("cnf_id = ?", cnfID).Delete(&AlertLog{}).Error)
}
