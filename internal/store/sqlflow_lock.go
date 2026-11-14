// @author: kevinkelly
// @license: Apache Licence
// @file: sqlflow_lock.go
// @time: 2026/10/28

package store

import (
	"context"
	"database/sql"
	"time"

	"dbtool/internal/sqlflow"
)

var _ sqlflow.Locker = (*SQLRepo)(nil)

// TryLock 用 MySQL 的 GET_LOCK 做跨进程互斥（SQL 工单执行锁，见 sqlflow/recover.go）。
// 锁属于一条连接，所以占用一条连接直到 release；进程被杀 / 连接断开时 MySQL 自动释放。
func (r *SQLRepo) TryLock(ctx context.Context, name string) (func(), bool, error) {
	sqlDB, err := r.DB.DB()
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
