// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package sqlopt

import (
	"gorm.io/gorm"

	"dbtool/internal/model"
)

// store 只负责写操作审计（dbt_audit_log）。本模块没有自己的表：任务保存在内存里，结束 30 分钟后清理。
type store struct{ db *gorm.DB }

func newStore(db *gorm.DB) *store { return &store{db: db} }

func (st *store) Record(e *model.AuditEntry) error { return st.db.Create(e).Error }
