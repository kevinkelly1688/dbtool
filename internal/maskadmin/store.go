// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package maskadmin

import (
	"errors"

	gomysql "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// gormStore 是元数据库里的实现。dbt_masking_policy 由 deploy/sql/230_masking_policy.sql 建表，没建时 PolicyReady 为 false，
// 旧配置仍可查看，保存会提示先建表。
type gormStore struct{ db *gorm.DB }

func newStore(db *gorm.DB) *gormStore { return &gormStore{db: db} }

func noTable(err error) bool {
	var me *gomysql.MySQLError
	return errors.As(err, &me) && me.Number == 1146
}

func (s *gormStore) PolicyReady() bool {
	var n int64
	err := s.db.Model(&model.MaskingPolicy{}).Limit(1).Count(&n).Error
	return err == nil
}

func (s *gormStore) ListPolicies(instanceID uint) ([]model.MaskingPolicy, error) {
	var out []model.MaskingPolicy
	err := s.db.Where("instance_id = ?", instanceID).Order("id").Find(&out).Error
	if noTable(err) {
		return nil, nil
	}
	return out, err
}

func (s *gormStore) Counts() (map[uint]int, error) {
	type row struct {
		InstanceID uint
		N          int
	}
	out := map[uint]int{}
	var pol []row
	if err := s.db.Model(&model.MaskingPolicy{}).Select("instance_id, COUNT(*) AS n").Where("active = 1").Group("instance_id").Scan(&pol).Error; err != nil {
		if noTable(err) {
			return out, nil
		}
		return nil, err
	}
	for _, r := range pol {
		out[r.InstanceID] += r.N
	}
	return out, nil
}

func (s *gormStore) SetConfig(item, value, description string, entry *model.AuditEntry) error {
	svc := settings.Open(s.db, nil)
	by := ""
	if entry != nil {
		by = entry.UserName
	}
	ws, hs, err := svc.Prepare(by, map[string]string{item: value})
	if err != nil {
		return err
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := settings.WriteTx(tx, ws, hs); err != nil {
			return err
		}
		if entry != nil {
			return tx.Create(entry).Error
		}
		return nil
	})
	svc.Invalidate()
	return err
}

func (s *gormStore) Commit(ops []storeOp, entry *model.AuditEntry) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		for _, op := range ops {
			var err error
			switch op.kind {
			case opInsertPolicy:
				p := op.policy
				err = tx.Create(&p).Error
			case opUpdatePolicy:
				p := op.policy
				err = tx.Model(&model.MaskingPolicy{}).Where("id = ?", p.ID).Updates(map[string]any{
					"scope_db": p.ScopeDB, "scope_table": p.ScopeTable, "field_path": p.FieldPath,
					"rule_kind": p.RuleKind, "keep_prefix": p.KeepPrefix, "keep_suffix": p.KeepSuffix, "hide_group": p.HideGroup, "rule_regex": p.RuleRegex,
					"active": p.Active, "updated_by": p.UpdatedBy, "updated_at": p.UpdatedAt,
				}).Error
			case opDeletePolicy:
				err = tx.Delete(&model.MaskingPolicy{}, op.id).Error
			}
			if err != nil {
				return err
			}
		}
		if entry != nil {
			return tx.Create(entry).Error
		}
		return nil
	})
}
