// @author: kevinkelly
// @license: Apache Licence
// @file: ready_db.go
// @time: 2026/10/28

package server

import (
	"context"

	"gorm.io/gorm"
)

// gormReady 用元数据库句柄实现 ReadyChecker：Ping + 查 information_schema（只读）。
type gormReady struct{ db *gorm.DB }

func (g gormReady) PingMetaDB(ctx context.Context) error {
	sqlDB, err := g.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}

func (g gormReady) ExistingTables(ctx context.Context, names []string) (map[string]bool, error) {
	var got []string
	err := g.db.WithContext(ctx).Raw("SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name IN ?", names).
		Scan(&got).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(got))
	for _, n := range got {
		out[n] = true
	}
	return out, nil
}
