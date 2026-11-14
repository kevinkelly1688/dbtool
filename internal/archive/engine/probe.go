// @author: kevinkelly
// @license: Apache Licence
// @file: probe.go
// @time: 2026/10/28

package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	drv "github.com/go-sql-driver/mysql"
)

// replicaProbeTimeout 是对单个从库读复制状态的超时：从库不可达时不能让每批的负载自检都卡很久。
const replicaProbeTimeout = 6 * time.Second

// mysqlProbe 是 LoadProbe 的 MySQL 实现。
//   - Threads_running：SHOW GLOBAL STATUS LIKE 'Threads_running'。
//   - 复制延迟：有从库连接（replicas）时逐个执行 SHOW REPLICA STATUS（旧版本 SHOW SLAVE STATUS）取 Seconds_Behind_Source 的最大值；
//     没有从库连接时只用 SHOW REPLICAS / SHOW SLAVE HOSTS 判断“有没有从库”，有则返回错误（延迟测不了，引擎标“未检测”）。
//   - 磁盘剩余：由上层提供（巡检指标），没有则 ok=false。
type mysqlProbe struct {
	db       *sql.DB
	replicas []*sql.DB
	disk     DiskFunc
}

// NewMySQLProbe 创建源库负载探针。replicas 是各从库的连接池（可为空），disk 可为 nil。
func NewMySQLProbe(db *sql.DB, replicas []*sql.DB, disk DiskFunc) LoadProbe {
	return &mysqlProbe{db: db, replicas: replicas, disk: disk}
}

func (p *mysqlProbe) ThreadsRunning(ctx context.Context) (int, error) {
	var name, val string
	if err := p.db.QueryRowContext(ctx, "SHOW GLOBAL STATUS LIKE 'Threads_running'").Scan(&name, &val); err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return 0, fmt.Errorf("Threads_running 取值异常：%s", quote(val))
	}
	return n, nil
}

// replicaLag 对一个从库取 Seconds_Behind_Source；SQL 线程 / IO 线程没跑（NULL）按“很大”处理。
func replicaLag(ctx context.Context, db *sql.DB) (sec int, isReplica bool, err error) {
	for _, q := range []string{"SHOW REPLICA STATUS", "SHOW SLAVE STATUS"} {
		rs, err := db.QueryContext(ctx, q)
		if err != nil {
			var me *drv.MySQLError
			if errors.As(err, &me) && me.Number == 1064 {
				continue // 老版本不认识 REPLICA 语法
			}
			return 0, false, err
		}
		cols, rows, err := scanAll(rs)
		rs.Close()
		if err != nil {
			return 0, false, err
		}
		if len(rows) == 0 {
			return 0, false, nil
		}
		for i, c := range cols {
			if strings.EqualFold(c, "Seconds_Behind_Source") || strings.EqualFold(c, "Seconds_Behind_Master") {
				s, null := valueText(rows[0][i], kindText)
				if null || s == "" {
					return 1 << 30, true, nil // 复制线程没在跑
				}
				n, err := strconv.Atoi(s)
				if err != nil {
					return 0, true, fmt.Errorf("Seconds_Behind_Source 取值异常：%s", quote(s))
				}
				return n, true, nil
			}
		}
		return 0, true, errors.New("复制状态里没有 Seconds_Behind_Source 列")
	}
	return 0, false, errors.New("无法读取复制状态")
}

func (p *mysqlProbe) ReplicaLag(ctx context.Context) (int, bool, error) {
	if len(p.replicas) > 0 {
		max := 0
		var firstErr error
		for _, r := range p.replicas {
			if r == nil {
				if firstErr == nil {
					firstErr = errors.New("有从库连不上，或没有在平台里登记（无法读取它的复制状态）")
				}
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, replicaProbeTimeout)
			sec, _, err := replicaLag(rctx, r)
			cancel()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if sec > max {
				max = sec
			}
		}
		if firstErr != nil {
			return 0, true, fmt.Errorf("从库复制延迟检测失败：%w", firstErr)
		}
		return max, true, nil
	}
	// 没有从库连接信息：只能判断有没有从库
	var lastErr error
	for _, q := range []string{"SHOW REPLICAS", "SHOW SLAVE HOSTS"} {
		rs, err := p.db.QueryContext(ctx, q)
		if err != nil {
			lastErr = err
			continue
		}
		_, rows, err := scanAll(rs)
		rs.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if len(rows) > 0 {
			return 0, true, errors.New("源实例有从库，但没有配置从库连接信息，复制延迟未检测")
		}
		return 0, false, nil
	}
	// 两条语句都失败（多半是账号没有 REPLICATION CLIENT 权限）：不能当作“没有从库”
	return 0, false, fmt.Errorf("无法判断源实例有没有从库：%w", lastErr)
}

func (p *mysqlProbe) DiskFreePct(ctx context.Context) (float64, bool, error) {
	if p.disk == nil {
		return 0, false, nil
	}
	return p.disk(ctx)
}
