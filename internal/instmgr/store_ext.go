// @author: kevinkelly
// @license: Apache Licence
// @file: store_ext.go
// @time: 2026/10/28

package instmgr

import (
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"dbtool/internal/instext"
)

// store 对 ExtRepo 的实现：dbtool 自己的旁路表。表不存在时各 Ready 方法返回 false，调用方降级。

var _ ExtRepo = (*store)(nil)

// 另外两张旁路表的就绪缓存（与 instext 同样：存在则恒真，不存在每 30 秒重查一次）。
type tableGate struct {
	mu      sync.Mutex
	ready   bool
	checked time.Time
}

func (g *tableGate) ok(db *gorm.DB, table string) bool {
	g.mu.Lock()
	if g.ready {
		g.mu.Unlock()
		return true
	}
	if !g.checked.IsZero() && time.Since(g.checked) < 30*time.Second {
		g.mu.Unlock()
		return false
	}
	g.mu.Unlock()
	ok := db.Migrator().HasTable(table)
	g.mu.Lock()
	if ok {
		g.ready = true
	} else {
		g.checked = time.Now()
	}
	g.mu.Unlock()
	return ok
}

const extTable = instext.Table

// ---- dbt_instance_ext ----

func (r *store) InstanceExtReady() bool { return instext.Ready(r.db) }

func (r *store) InstanceExts(ids []uint) (map[uint]instext.Row, error) {
	out := map[uint]instext.Row{}
	if len(ids) == 0 || !instext.Ready(r.db) {
		return out, nil
	}
	var rows []instext.Row
	if err := r.db.Where("instance_id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.InstanceID] = x
	}
	return out, nil
}

func (r *store) SetInstanceDisabled(id uint, disabled bool, by string, at time.Time) error {
	row := instext.Row{InstanceID: id, Disabled: disabled, UpdatedAt: at}
	cols := []string{"disabled", "updated_at"}
	if disabled {
		row.DisabledBy, row.DisabledAt = by, &at
		cols = append(cols, "disabled_by", "disabled_at")
	}
	return r.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "instance_id"}}, DoUpdates: clause.AssignmentColumns(cols)}).Create(&row).Error
}

func (r *store) SetInstanceEnv(id uint, env string, at time.Time) error {
	row := instext.Row{InstanceID: id, Env: env, UpdatedAt: at}
	err := r.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "instance_id"}}, DoUpdates: clause.AssignmentColumns([]string{"env", "updated_at"})}).Create(&row).Error
	if err == nil {
		instext.InvalidateEnvs() // 防火墙 / 查询用的“实例 -> 环境”缓存立即更新
	}
	return err
}

func (r *store) InstanceStats(scope map[uint]bool) (InstanceStats, error) {
	var st InstanceStats
	if scope != nil && len(scope) == 0 {
		return st, nil
	}
	where, args := "1=1", []any(nil)
	if scope != nil {
		ids := make([]uint, 0, len(scope))
		for id := range scope {
			ids = append(ids, id)
		}
		where, args = "i.id IN ?", []any{ids}
	}
	if err := r.db.Raw("SELECT COUNT(*) FROM dbt_sql_instance i WHERE "+where, args...).Scan(&st.Total).Error; err != nil {
		return st, err
	}
	if instext.Ready(r.db) {
		if err := r.db.Raw("SELECT COUNT(*) FROM dbt_sql_instance i WHERE "+where+" AND i.id IN (SELECT instance_id FROM "+extTable+" WHERE disabled = 1)", args...).Scan(&st.Disabled).Error; err != nil {
			return st, err
		}
	}
	coll := "SELECT COUNT(*) FROM dbt_sql_instance i WHERE " + where + " AND i.is_enable = 1"
	if instext.Ready(r.db) {
		coll += " AND i.id NOT IN (SELECT instance_id FROM " + extTable + " WHERE disabled = 1)"
	}
	err := r.db.Raw(coll, args...).Scan(&st.Collecting).Error
	return st, err
}

// extFilter 把“状态 / 环境”筛选加到实例列表查询上；none 为真表示旁路表不存在却要按它筛选，结果必然为空。
func (r *store) extFilter(q *gorm.DB, f InstanceFilter) (*gorm.DB, bool) {
	switch f.State {
	case "disabled":
		if !instext.Ready(r.db) {
			return q, true
		}
		q = q.Where("dbt_sql_instance.id IN (SELECT instance_id FROM " + extTable + " WHERE disabled = 1)")
	case "enabled":
		q = instext.ExcludeDisabled(q, "dbt_sql_instance.id")
	}
	if f.Env != "" {
		if !instext.Ready(r.db) {
			return q, true
		}
		q = q.Where("dbt_sql_instance.id IN (SELECT instance_id FROM "+extTable+" WHERE env = ?)", f.Env)
	}
	return q, false
}

// ---- dbt_account_reg ----

// dbt_account_reg
type accRegRow struct {
	InstanceID   uint       `gorm:"primaryKey;column:instance_id"`
	UserName     string     `gorm:"primaryKey;column:user_name"`
	Host         string     `gorm:"primaryKey;column:host"`
	Owner        string     `gorm:"column:owner"`
	OwnerDisplay string     `gorm:"column:owner_display"`
	Purpose      string     `gorm:"column:purpose"`
	ExpireDate   *time.Time `gorm:"column:expire_date"`
	UpdatedBy    string     `gorm:"column:updated_by"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

func (accRegRow) TableName() string { return "dbt_account_reg" }

func (r *store) AccountRegReady() bool { return r.regGate.ok(r.db, "dbt_account_reg") }

func (r *store) AccountRegs(instanceID uint) (map[string]AccountReg, error) {
	out := map[string]AccountReg{}
	if !r.AccountRegReady() {
		return out, nil
	}
	var rows []accRegRow
	if err := r.db.Where("instance_id = ?", instanceID).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, x := range rows {
		a := AccountReg{User: x.UserName, Host: x.Host, Owner: x.Owner, OwnerDisplay: x.OwnerDisplay, Purpose: x.Purpose}
		if x.ExpireDate != nil {
			a.ExpireDate = x.ExpireDate.Format("2006-01-02")
		}
		out[regKey(x.UserName, x.Host)] = a
	}
	return out, nil
}

func (r *store) SaveAccountReg(instanceID uint, a AccountReg, by string, at time.Time) error {
	row := accRegRow{InstanceID: instanceID, UserName: a.User, Host: a.Host, Owner: a.Owner, OwnerDisplay: a.OwnerDisplay, Purpose: a.Purpose, UpdatedBy: by, UpdatedAt: at}
	if a.ExpireDate != "" {
		d, err := time.ParseInLocation("2006-01-02", a.ExpireDate, time.Local)
		if err != nil {
			return err
		}
		row.ExpireDate = &d
	}
	return r.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "instance_id"}, {Name: "user_name"}, {Name: "host"}},
		DoUpdates: clause.AssignmentColumns([]string{"owner", "owner_display", "purpose", "expire_date", "updated_by", "updated_at"})}).Create(&row).Error
}

func (r *store) DeleteAccountReg(instanceID uint, user, host string) error {
	if !r.AccountRegReady() {
		return nil
	}
	return r.db.Where("instance_id = ? AND user_name = ? AND host = ?", instanceID, user, host).Delete(&accRegRow{}).Error
}

// ---- dbt_param_template_ext ----

// dbt_param_template_ext
type tplExtRow struct {
	TemplateID       uint      `gorm:"primaryKey;column:template_id"`
	RecommendedValue string    `gorm:"column:recommended_value"`
	UpdatedBy        string    `gorm:"column:updated_by"`
	UpdatedAt        time.Time `gorm:"column:updated_at"`
}

func (tplExtRow) TableName() string { return "dbt_param_template_ext" }

func (r *store) TemplateExtReady() bool { return r.tplGate.ok(r.db, "dbt_param_template_ext") }

func (r *store) TemplateRecommended() (map[uint]string, error) {
	out := map[uint]string{}
	if !r.TemplateExtReady() {
		return out, nil
	}
	var rows []tplExtRow
	if err := r.db.Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, x := range rows {
		if x.RecommendedValue != "" {
			out[x.TemplateID] = x.RecommendedValue
		}
	}
	return out, nil
}

func (r *store) SaveTemplateRecommended(templateID uint, value, by string, at time.Time) error {
	row := tplExtRow{TemplateID: templateID, RecommendedValue: value, UpdatedBy: by, UpdatedAt: at}
	return r.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "template_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"recommended_value", "updated_by", "updated_at"})}).Create(&row).Error
}

// ---- 跨实例参数修改历史 ----

func (r *store) ListAllParamHistory(scope map[uint]bool, q string, offset, limit int) ([]ParamHistoryRow, int64, error) {
	if scope != nil && len(scope) == 0 {
		return nil, 0, nil
	}
	base := r.db.Table("dbt_param_history ph").Joins("JOIN dbt_sql_instance i ON i.id = ph.instance_id")
	if scope != nil {
		ids := make([]uint, 0, len(scope))
		for id := range scope {
			ids = append(ids, id)
		}
		base = base.Where("ph.instance_id IN ?", ids)
	}
	if q = strings.TrimSpace(q); q != "" {
		like := "%" + likeEscape(q) + "%"
		base = base.Where("(ph.variable_name LIKE ? ESCAPE '!' OR i.instance_name LIKE ? ESCAPE '!')", like, like)
	}
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []ParamHistoryRow
	err := base.Select("ph.*, i.instance_name AS instance_name").Order("ph.id DESC").Offset(offset).Limit(limit).Scan(&out).Error
	return out, total, err
}
