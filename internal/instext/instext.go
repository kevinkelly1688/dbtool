// @author: kevinkelly
// @license: Apache Licence
// @file: instext.go
// @time: 2026/10/28

// Package instext 是 dbt_sql_instance 的“旁路扩展”：不加列，
// 实例级的停用状态与环境（生产 / 测试）存在 dbtool 自己的 dbt_instance_ext 表里（deploy/sql/070_instance_ext.sql）。
//
// 这个包没有业务依赖（只依赖 gorm），好让存储层的共享查询入口（实例下拉、SQL 上线可选实例、定时采集等）
// 用一行代码排除“已停用”的实例。约定：表不存在、或没有任何停用记录时，所有函数的行为与引入本包之前完全一致。
package instext

import (
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// Table 是旁路表名。
const Table = "dbt_instance_ext"

// Row -> dbt_instance_ext：每个实例至多一行；没有记录等同于“启用、环境未设置”。
type Row struct {
	InstanceID uint       `gorm:"primaryKey;column:instance_id" json:"instance_id"`
	Disabled   bool       `gorm:"column:disabled" json:"disabled"`
	Env        string     `gorm:"column:env" json:"env"` // prod / test / 空
	DisabledBy string     `gorm:"column:disabled_by" json:"disabled_by"`
	DisabledAt *time.Time `gorm:"column:disabled_at" json:"disabled_at"`
	UpdatedAt  time.Time  `gorm:"column:updated_at" json:"updated_at"`
}

func (Row) TableName() string { return Table }

// 内置环境取值（环境字典里的 prod / test 不可删除；其余环境由系统管理维护，见 envdict.go）。
const (
	EnvProd = "prod"
	EnvTest = "test"
)

var (
	mu      sync.Mutex
	ready   bool      // 表已存在（一旦存在就不再检查）
	lastNot time.Time // 最近一次发现表不存在的时间
)

// notReadyRecheck 是表不存在时重新检查的间隔：运维执行了建表脚本后，最多等这么久就生效，不用重启。
const notReadyRecheck = 30 * time.Second

// Ready 判断旁路表是否已建。结果带缓存：存在则恒为 true，不存在每 30 秒重新检查一次。
func Ready(db *gorm.DB) bool {
	if db == nil {
		return false
	}
	mu.Lock()
	if ready {
		mu.Unlock()
		return true
	}
	if !lastNot.IsZero() && time.Since(lastNot) < notReadyRecheck {
		mu.Unlock()
		return false
	}
	mu.Unlock()
	// 用干净的会话检查：传进来的 db 可能已经带着调用方的 Where / Preload，不能让它们混进建表检查。
	ok := db.Session(&gorm.Session{NewDB: true}).Migrator().HasTable(Table)
	mu.Lock()
	if ok {
		ready = true
	} else {
		lastNot = time.Now()
	}
	mu.Unlock()
	return ok
}

// Reset 清掉缓存（单测用；建表后想立刻生效也可以调用）。
func Reset() {
	mu.Lock()
	ready, lastNot = false, time.Time{}
	mu.Unlock()
}

// disabledSub 是“已停用实例”的子查询。
const disabledSub = "(SELECT instance_id FROM " + Table + " WHERE disabled = 1)"

// ExcludeDisabled 给查询加上“排除已停用实例”的条件。col 是实例 ID 列（如 "dbt_sql_instance.id"，只能是代码里写死的字面量）。
// 旁路表不存在时原样返回 db。
func ExcludeDisabled(db *gorm.DB, col string) *gorm.DB {
	if !Ready(db) {
		return db
	}
	return db.Where(col + " NOT IN " + disabledSub)
}

// DisabledCond 返回可以拼进原生 SQL WHERE 的片段（形如 " AND col NOT IN (...)"）；旁路表不存在时返回空串。
func DisabledCond(db *gorm.DB, col string) string {
	if !Ready(db) {
		return ""
	}
	return " AND " + col + " NOT IN " + disabledSub
}

// DisabledSet 返回所有已停用实例的 ID；旁路表不存在返回空集合。
func DisabledSet(db *gorm.DB) (map[uint]bool, error) {
	out := map[uint]bool{}
	if !Ready(db) {
		return out, nil
	}
	var ids []uint
	if err := db.Session(&gorm.Session{NewDB: true}).Table(Table).Where("disabled = 1").Pluck("instance_id", &ids).Error; err != nil {
		if isMissingTable(err) {
			return out, nil
		}
		return nil, err
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// Envs 一次查出一批实例的环境（instance_id -> prod / test）；没有记录或环境为空的实例不在结果里。
// 旁路表不存在（或查询时发现表缺失）返回空集合，不报错。
func Envs(db *gorm.DB, ids []uint) (map[uint]string, error) {
	out := map[uint]string{}
	if len(ids) == 0 || !Ready(db) {
		return out, nil
	}
	var rows []struct {
		InstanceID uint
		Env        string
	}
	if err := db.Session(&gorm.Session{NewDB: true}).Table(Table).Select("instance_id, env").Where("instance_id IN ?", ids).Scan(&rows).Error; err != nil {
		if isMissingTable(err) {
			return out, nil
		}
		return nil, err
	}
	for _, r := range rows {
		if e := strings.TrimSpace(r.Env); e != "" {
			out[r.InstanceID] = e
		}
	}
	return out, nil
}

func isMissingTable(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "1146") || strings.Contains(m, "doesn't exist")
}
