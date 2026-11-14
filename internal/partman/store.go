// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package partman

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	drv "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"dbtool/internal/crypto"
	"dbtool/internal/model"
)

// store 是 Repo 的 GORM 实现。dbt_partman_config / dbt_partman_log 只读写既有列。
type store struct{ db *gorm.DB }

func newStore(db *gorm.DB, _ *crypto.Mirage) *store { return &store{db: db} }

var _ Repo = (*store)(nil)

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (r *store) ListConfigs(f ConfigFilter) ([]Config, int64, error) {
	q := r.db.Model(&Config{})
	if !f.AllInstances {
		if len(f.InstanceIDs) == 0 {
			return nil, 0, nil
		}
		q = q.Where("instance_id IN ?", f.InstanceIDs)
	}
	if f.InstanceID != 0 {
		q = q.Where("instance_id = ?", f.InstanceID)
	}
	if f.IsEnable != nil {
		q = q.Where("isenable = ?", *f.IsEnable)
	}
	if f.ColType != "" {
		q = q.Where("col_type = ?", f.ColType)
	}
	if f.Period != "" {
		q = q.Where("period = ?", f.Period)
	}
	if f.Search != "" {
		like := "%" + likeEscape(f.Search) + "%"
		q = q.Where("(dbname LIKE ? OR tbname LIKE ? OR `column` LIKE ? OR env LIKE ?)", like, like, like, like)
	}
	q = q.Session(&gorm.Session{}) // 让 q 可以先 Count 再 Find（否则 Count 的 SELECT 会带进后面的查询）
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	q = q.Order("id")
	if f.Limit > 0 {
		q = q.Offset(f.Offset).Limit(f.Limit)
	}
	var out []Config
	err := q.Find(&out).Error
	return out, total, err
}

func (r *store) GetConfig(id uint) (*Config, error) {
	var c Config
	err := r.db.Where("id = ?", id).Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &c, err
}

func (r *store) FindByKey(instanceID uint, db, table string) (*Config, error) {
	var c Config
	err := r.db.Where("instance_id = ? AND dbname = ? AND tbname = ?", instanceID, db, table).Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &c, err
}

func isDup(err error) bool {
	var me *drv.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// InsertConfig：cnf_id 取当前最大值 + 1。两个人同时新增时可能撞上唯一键，重试几次。
func (r *store) InsertConfig(c *Config) error {
	var err error
	for i := 0; i < 5; i++ {
		var max int64
		if err = r.db.Model(&Config{}).Select("COALESCE(MAX(cnf_id), 0)").Scan(&max).Error; err != nil {
			return err
		}
		c.ID, c.CnfID = 0, max+1
		if err = r.db.Create(c).Error; err == nil {
			return nil
		}
		if !isDup(err) {
			return err
		}
		if strings.Contains(err.Error(), "uniq_db_tb") {
			return newErr(409, "duplicate", "该实例上的 %s.%s 已经有分区配置", c.DBName, c.TbName)
		}
	}
	return err
}

func (r *store) UpdateConfig(c *Config) error {
	err := r.db.Model(&Config{}).Where("id = ?", c.ID).Updates(map[string]any{
		"instance_id": c.InstanceID, "dbname": c.DBName, "tbname": c.TbName, "column": c.Column, "col_type": c.ColType,
		"period": c.Period, "interval": c.Interval, "retain": c.Retain, "preCreate": c.PreCreate, "timezone": c.Timezone,
		"isenable": c.IsEnable, "env": c.Env, "lastest_time": c.LastestTime,
	}).Error
	if isDup(err) {
		return newErr(409, "duplicate", "该实例上的 %s.%s 已经有分区配置", c.DBName, c.TbName)
	}
	return err
}

func (r *store) DeleteConfig(id uint) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		var c Config
		if err := tx.Where("id = ?", id).Take(&c).Error; err != nil {
			return err
		}
		if err := tx.Where("cnf_id = ?", c.CnfID).Delete(&Log{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", id).Delete(&Config{}).Error
	})
}

func (r *store) InsertLog(cnfID int64, result string, at time.Time) (uint, error) {
	l := Log{CnfID: cnfID, ExecResult: result, CreateTime: at}
	err := r.db.Create(&l).Error
	return l.ID, err
}

func (r *store) ListLogs(cnfID int64, limit int) ([]Log, error) {
	var out []Log
	err := r.db.Where("cnf_id = ?", cnfID).Order("id DESC").Limit(limit).Find(&out).Error
	return out, err
}

// 本包写入的 exec_result 是紧凑 JSON，来源字段形如 "source":"manual"；字符串值里的引号会被转义成 \"，不会误匹配。
func sourceLike(src string) string { return `%"source":"` + src + `"%` }

func (r *store) ListLogsByCnf(cnfIDs []int64, source string, offset, limit int) ([]Log, int64, error) {
	if len(cnfIDs) == 0 {
		return nil, 0, nil
	}
	q := r.db.Model(&Log{}).Where("cnf_id IN ?", cnfIDs)
	switch source {
	case "manual", "auto":
		q = q.Where("exec_result LIKE ?", sourceLike(source))
	case "external":
		q = q.Where("exec_result NOT LIKE ? AND exec_result NOT LIKE ?", sourceLike("manual"), sourceLike("auto"))
	}
	q = q.Session(&gorm.Session{})
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []Log
	err := q.Order("id DESC").Offset(offset).Limit(limit).Find(&out).Error
	return out, total, err
}

func (r *store) LastLogTimes(cnfIDs []int64) (map[int64]time.Time, error) {
	out := map[int64]time.Time{}
	if len(cnfIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		CnfID int64
		T     time.Time
	}
	err := r.db.Model(&Log{}).Select("cnf_id, MAX(create_time) AS t").Where("cnf_id IN ?", cnfIDs).Group("cnf_id").Scan(&rows).Error
	for _, x := range rows {
		out[x.CnfID] = x.T
	}
	return out, err
}

func (r *store) InsertAudit(e *model.AuditEntry) error { return r.db.Create(e).Error }

// TryLock 用 MySQL 的 GET_LOCK 做跨进程互斥：锁属于一条连接，所以占用一条连接直到 release。
func (r *store) TryLock(ctx context.Context, name string) (func(), bool, error) {
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

func (r *store) GetLog(id uint) (*Log, error) {
	var l Log
	err := r.db.Where("id = ?", id).Take(&l).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &l, err
}

// ---- 推送通道（dbt_partman_webhook，不改结构） ----

func (r *store) ListWebhooks(onlyEnabled bool) ([]Webhook, error) {
	q := r.db.Model(&Webhook{})
	if onlyEnabled {
		q = q.Where("isenabled = ?", 1)
	}
	var out []Webhook
	err := q.Order("id").Find(&out).Error
	return out, err
}

func (r *store) InsertWebhook(w *Webhook) error {
	err := r.db.Create(w).Error
	if isDup(err) {
		return newErr(409, "duplicate", "该 webhook 地址已经配置过")
	}
	return err
}

func (r *store) UpdateWebhook(w *Webhook) error {
	err := r.db.Model(&Webhook{}).Where("id = ?", w.ID).Updates(map[string]any{
		"webhook_url": w.URL, "type": w.Type, "isenabled": w.IsEnabled, "mention_on": w.MentionOn,
		"mention": w.Mention, "keyword": w.Keyword, "secret": w.Secret,
	}).Error
	if isDup(err) {
		return newErr(409, "duplicate", "该 webhook 地址已经配置过")
	}
	return err
}

func (r *store) DeleteWebhook(id uint) error {
	return r.db.Where("id = ?", id).Delete(&Webhook{}).Error
}
