// @author: kevinkelly
// @license: Apache Licence
// @file: redis_types.go
// @time: 2026/10/28

package slowlog

import (
	"context"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/engine/redis"
	"dbtool/internal/model"
)

// Redis 慢查询：定时对 redis 实例执行 SLOWLOG GET 增量采集，指纹化后写入 dbt_redis_slowlog（deploy/sql/130_redis_slowlog.sql），
// 在“性能优化 › 慢 SQL 洞察”页面按实例查看统计 / 明细 / 趋势。

// 权限：页面与数据接口沿用 slowlogs.view / slowquery.view；SLOWLOG RESET 是高危按钮 redis.slowlog_reset（仅超级用户默认可用）。
const (
	redisResetButton = "redis.slowlog_reset"
	actionRedisReset = "Redis慢查询-重置慢日志"
)

// 采集参数。
const (
	redisSlowFetchMax     = 1000 // 单次最多读取的慢日志条数
	redisSlowTextMax      = 4000 // 入库的完整命令最大字符数
	redisSlowRetention    = MaxSpanDays * 24 * time.Hour
	redisPurgeEvery       = time.Hour
	redisPurgeBatch       = 5000
	redisManualCollectGap = 10 * time.Second // “立即采集”对同一实例的最小间隔
)

// RedisTarget 是采集 / 重置需要的 Redis 能力；*redis.Client 满足它，单测里用进程内假 RESP 服务端。
type RedisTarget interface {
	SlowlogLen(ctx context.Context) (int64, error)
	SlowlogGet(ctx context.Context, n int) ([]redis.SlowEntry, error)
	SlowlogReset(ctx context.Context) error
	Close() error
}

// RedisOpener 打开到 Redis 实例的连接。
type RedisOpener func(ctx context.Context, c engine.Conn) (RedisTarget, error)

// OpenRedisTarget 是真实实现：短连接、只读白名单（SLOWLOG RESET 走专用方法）、全进程连接上限，见 internal/engine/redis。
func OpenRedisTarget(ctx context.Context, c engine.Conn) (RedisTarget, error) {
	cl, err := redis.Dial(ctx, redis.ConfigFromConn(c))
	if err != nil {
		return nil, err
	}
	return cl, nil
}

// RedisSlowRow 是一条待入库的 Redis 慢日志。
type RedisSlowRow struct {
	SlowID      int64
	StartTime   time.Time
	DurationUs  int64
	CommandName string
	Fingerprint string
	FpID        string // 指纹的 md5
	CommandText string
	ClientAddr  string
	ClientName  string
}

// RedisState 是某个 Redis 实例的采集状态与游标。
type RedisState struct {
	InstanceID   uint      `json:"instance_id"`
	InstanceName string    `json:"instance_name,omitempty"`
	LastSlowID   int64     `json:"last_slow_id"` // 已采集到的最大条目 id（-1 表示还没采集过）
	LastSlowTime time.Time `json:"last_slow_time"`
	LastRunAt    time.Time `json:"last_run_at"`
	LastOKAt     time.Time `json:"last_ok_at"`
	Status       string    `json:"status"` // ok / error
	Error        string    `json:"error,omitempty"`
	LastRows     int       `json:"last_rows"`
	TotalRows    int64     `json:"total_rows"`
	DurationMs   int64     `json:"duration_ms"`
}

// RedisQuery 是统计 / 明细 / 趋势共用的已校验查询。
type RedisQuery struct {
	InstanceID uint
	From, To   string // [From, To)
	Command    string // 命令名（大写，可含子命令），空表示不限
	FpID       string
	Search     string
	MinMicros  int64 // 单次耗时下限（微秒）
	Sort       string
	Desc       bool
	Limit      int
	Offset     int
	Bucket     string // 趋势：day / hour
}

// RedisReviewRow 是统计的一行（按指纹聚合）。
type RedisReviewRow struct {
	FpID        string  `json:"fp_id"`
	Fingerprint string  `json:"fingerprint"`
	CommandName string  `json:"command_name"`
	ExecCount   int64   `json:"exec_count"`
	TotalMs     float64 `json:"total_ms"`
	AvgMs       float64 `json:"avg_ms"`
	MaxMs       float64 `json:"max_ms"`
	FirstSeen   string  `json:"first_seen"`
	LastSeen    string  `json:"last_seen"`
	SampleText  string  `json:"sample_text,omitempty"` // 最近一条的完整命令
	Spark       []int64 `json:"spark,omitempty"`       // 范围内每天的慢查次数（从开始日期起逐日）
}

// RedisSummary 是统计页顶部 KPI。
type RedisSummary struct {
	Fingerprints int64   `json:"fingerprints"`
	ExecCount    int64   `json:"exec_count"`
	TotalMs      float64 `json:"total_ms"`
	MaxMs        float64 `json:"max_ms"`
}

// RedisReviewPage 是统计的一页。
type RedisReviewPage struct {
	Total   int64            `json:"total"`
	Rows    []RedisReviewRow `json:"rows"`
	Summary *RedisSummary    `json:"summary"`
	From    string           `json:"from"`
	To      string           `json:"to"`
	Notes   []string         `json:"notes,omitempty"`
}

// RedisHistoryRow 是明细的一行。
type RedisHistoryRow struct {
	ID          uint    `json:"id"`
	SlowID      int64   `json:"slow_id"`
	StartTime   string  `json:"start_time"`
	DurationMs  float64 `json:"duration_ms"`
	CommandName string  `json:"command_name"`
	Fingerprint string  `json:"fingerprint"`
	FpID        string  `json:"fp_id"`
	CommandText string  `json:"command_text"`
	ClientAddr  string  `json:"client_addr"`
	ClientName  string  `json:"client_name"`
}

// RedisHistoryPage 是明细的一页。
type RedisHistoryPage struct {
	Total int64             `json:"total"`
	Rows  []RedisHistoryRow `json:"rows"`
	From  string            `json:"from"`
	To    string            `json:"to"`
}

// RedisTrendPoint 是趋势的一个时间桶。
type RedisTrendPoint struct {
	T       string  `json:"t"`
	Count   int64   `json:"count"`
	AvgMs   float64 `json:"avg_ms"`
	MaxMs   float64 `json:"max_ms"`
	TotalMs float64 `json:"total_ms"`
}

// RedisTrend 是趋势。
type RedisTrend struct {
	Bucket string            `json:"bucket"`
	Points []RedisTrendPoint `json:"points"`
	From   string            `json:"from"`
	To     string            `json:"to"`
}

// RedisRepo 是 Redis 慢查询依赖的元数据访问；GORM 实现见 redis_store.go，测试里用内存假实现。
type RedisRepo interface {
	// RedisTablesReady 报告 dbt_redis_slowlog 与状态表是否已创建（130_redis_slowlog.sql）。
	RedisTablesReady() bool
	// RedisInstances 返回开启了采集开关（is_enable=1）、类型为 redis、且未被实例管理停用的实例。
	RedisInstances() ([]model.Instance, error)
	RedisState(instanceID uint) (*RedisState, error)
	RedisStates() ([]RedisState, error)
	SaveRedisState(st RedisState) error
	// InsertRedisSlowlogs 批量写入，按 (instance_id, slow_id, start_time) 去重，返回实际新增的行数。
	InsertRedisSlowlogs(instanceID uint, rows []RedisSlowRow) (int, error)
	RedisReview(q RedisQuery) ([]RedisReviewRow, int64, *RedisSummary, error)
	RedisHistory(q RedisQuery) ([]RedisHistoryRow, int64, error)
	RedisTrend(q RedisQuery) ([]RedisTrendPoint, error)
	RedisCommands(q RedisQuery) ([]string, error)
	PurgeRedisSlowlogs(before time.Time, limit int) (int64, error)
}
