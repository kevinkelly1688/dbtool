// @author: kevinkelly
// @license: Apache Licence
// @file: redis_collect.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/engine/redis"
)

// ---- 对 Redis 实例的只读访问 ----

// RedisTarget 是巡检需要的 Redis 只读能力；*redis.Client 满足它，单测里用进程内假 RESP 服务端。
type RedisTarget interface {
	Info(ctx context.Context, section string) (map[string]string, error)
	ConfigGet(ctx context.Context, patterns ...string) (map[string]string, error)
	SlowlogLen(ctx context.Context) (int64, error)
	SlowlogGet(ctx context.Context, n int) ([]redis.SlowEntry, error)
	CommandAvailable(ctx context.Context, names ...string) (map[string]bool, error)
	Scan(ctx context.Context, cursor string, count int) (string, []string, error)
	Type(ctx context.Context, key string) (string, error)
	MemoryUsage(ctx context.Context, key string) (int64, error)
	ElementCount(ctx context.Context, typ, key string) (int64, error)
	Close() error
}

// RedisOpener 打开到 Redis 实例的连接。
type RedisOpener func(ctx context.Context, c engine.Conn) (RedisTarget, error)

// OpenRedis 是 RedisOpener 的真实实现：短连接、命令白名单（只读）、全进程连接数上限，见 internal/engine/redis。
func OpenRedis(ctx context.Context, c engine.Conn) (RedisTarget, error) {
	cl, err := redis.Dial(ctx, redis.ConfigFromConn(c))
	if err != nil {
		return nil, err
	}
	return cl, nil
}

// ---- 采集 ----

const (
	redisSteps         = 5 // info / config / slowlog / commands / bigkey
	redisSlowTop       = 20
	redisBigKeyKeep    = 10
	redisBigKeyMax     = 5000 // 抽样 key 数上限（硬上限）
	redisBigKeyBatch   = 200  // SCAN COUNT
	redisBigKeyDefault = 1000 // 立即巡检勾选“大 key 抽样”时的默认数量
	redisBigKeyBudget  = 60 * time.Second
	redisBigKeyPause   = time.Millisecond // 每个 key 之间的停顿，避免对线上造成持续压力
)

// 被点名检查是否已改名 / 禁用的危险命令。
var redisDangerous = []string{"FLUSHALL", "FLUSHDB", "KEYS", "CONFIG", "DEBUG", "SHUTDOWN"}

// redisSnapshot 是一次采集到的原始数据；评估只依赖它，不再访问实例。
type redisSnapshot struct {
	Now         time.Time
	Info        map[string]string
	Config      map[string]string
	ConfigErr   string // 非空表示 CONFIG GET 不可用（被改名 / ACL 禁止）
	SlowLen     int64
	SlowTop     []redis.SlowEntry
	SlowErr     string
	CmdAvail    map[string]bool
	CmdErr      string
	HasPassword bool // 平台保存的连接信息里有密码（用于 CONFIG GET 读不到时推断）
	BigKeyN     int  // 计划抽样的 key 数；0 表示没开启
	BigKeys     *bigKeyResult
	BigKeyErr   string
}

type bigKey struct {
	DB    string
	Key   string
	Type  string
	Bytes int64 // MEMORY USAGE 的字节数；退路是元素个数时为 0
	Elems int64
}

type bigKeyResult struct {
	Scanned int
	Exact   bool // true 表示字节数来自 MEMORY USAGE；false 表示只有元素个数
	Top     []bigKey
}

// redisProgress 是采集过程的回调：每完成一步返回 false 表示应当停止（取消 / 超时）。
type redisProgress func(done int) bool

var errRedisStopped = errors.New("巡检被中止")

// collectRedis 采集一个实例。INFO 失败整体失败；其余步骤失败只记录在快照里，对应检查项显示“未采集”。
func collectRedis(ctx context.Context, t RedisTarget, conn engine.Conn, now time.Time, bigKeyN int, progress redisProgress) (*redisSnapshot, error) {
	snap := &redisSnapshot{Now: now, HasPassword: conn.Password != "", BigKeyN: min(max(bigKeyN, 0), redisBigKeyMax)}
	step := func(n int) error {
		if ctx.Err() != nil || (progress != nil && !progress(n)) {
			return errRedisStopped
		}
		return nil
	}
	info, err := t.Info(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("读取 INFO 失败：%s", errText(err))
	}
	snap.Info = info
	if err := step(1); err != nil {
		return nil, err
	}

	snap.Config, snap.ConfigErr = configGetEach(ctx, t, "requirepass", "protected-mode", "bind", "maxmemory", "maxmemory-policy", "save", "appendonly", "maxclients", "databases")
	if err := step(2); err != nil {
		return nil, err
	}

	if n, err := t.SlowlogLen(ctx); err != nil {
		snap.SlowErr = errText(err)
	} else {
		snap.SlowLen = n
		if n > 0 {
			if top, err := t.SlowlogGet(ctx, redisSlowTop); err != nil {
				snap.SlowErr = errText(err)
			} else {
				snap.SlowTop = top
			}
		}
	}
	if err := step(3); err != nil {
		return nil, err
	}

	if av, err := t.CommandAvailable(ctx, redisDangerous...); err != nil {
		snap.CmdErr = errText(err)
	} else {
		snap.CmdAvail = av
	}
	if err := step(4); err != nil {
		return nil, err
	}

	if snap.BigKeyN > 0 {
		res, err := sampleBigKeys(ctx, t, snap.BigKeyN, bigKeyDB(conn))
		if err != nil {
			snap.BigKeyErr = errText(err)
		}
		snap.BigKeys = res
	}
	if err := step(5); err != nil {
		return nil, err
	}
	return snap, nil
}

// configGetEach 逐项 CONFIG GET：Redis 7 以前一条 CONFIG GET 只能带一个参数名。一项都读不到（CONFIG 被改名 / ACL 禁止）时返回错误文本；
// 只有个别项失败（例如 ACL 只放行部分参数）时，读到的照常使用。
func configGetEach(ctx context.Context, t RedisTarget, keys ...string) (map[string]string, string) {
	out := map[string]string{}
	var firstErr error
	for _, k := range keys {
		m, err := t.ConfigGet(ctx, k)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for kk, v := range m {
			out[kk] = v
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, errText(firstErr)
	}
	return out, ""
}

func bigKeyDB(c engine.Conn) string {
	if n, err := strconv.Atoi(strings.TrimSpace(c.DBName)); err == nil && n > 0 {
		return "db" + strconv.Itoa(n)
	}
	return "db0"
}

// sampleBigKeys 用 SCAN 取不超过 n 个 key（只看连接所在的库），逐个读 TYPE 和 MEMORY USAGE，保留最大的几个。
// 严格限量：key 数硬上限、总时间上限、key 之间停顿；SCAN 的 COUNT 只是提示，单批返回量由 Redis 决定。
func sampleBigKeys(ctx context.Context, t RedisTarget, n int, db string) (*bigKeyResult, error) {
	n = min(n, redisBigKeyMax)
	ctx, cancel := context.WithTimeout(ctx, redisBigKeyBudget)
	defer cancel()
	res := &bigKeyResult{Exact: true}
	cursor := "0"
	useMemory := true
	for res.Scanned < n {
		next, keys, err := t.Scan(ctx, cursor, min(redisBigKeyBatch, n-res.Scanned))
		if err != nil {
			return finishBigKeys(res), err
		}
		for _, k := range keys {
			if res.Scanned >= n {
				break
			}
			if ctx.Err() != nil {
				return finishBigKeys(res), ctx.Err()
			}
			typ, err := t.Type(ctx, k)
			if err != nil {
				return finishBigKeys(res), err
			}
			res.Scanned++
			if typ == "none" { // 扫描途中过期了
				continue
			}
			bk := bigKey{DB: db, Key: k, Type: typ}
			if useMemory {
				b, err := t.MemoryUsage(ctx, k)
				switch {
				case err == nil && b >= 0:
					bk.Bytes = b
				case err == nil: // -1：key 刚好过期
					continue
				case redis.IsUnknownCommand(err) || redis.IsNoPerm(err):
					useMemory, res.Exact = false, false // Redis 4.0 以前没有 MEMORY USAGE：退回元素个数
				default:
					return finishBigKeys(res), err
				}
			}
			if !useMemory {
				c, err := t.ElementCount(ctx, typ, k)
				if err != nil {
					continue
				}
				bk.Elems = c
			}
			res.Top = append(res.Top, bk)
			if len(res.Top) > redisBigKeyKeep*4 { // 不让候选无限增长：定期裁剪
				res.Top = trimBigKeys(res.Top)
			}
			select {
			case <-ctx.Done():
				return finishBigKeys(res), ctx.Err()
			case <-time.After(redisBigKeyPause):
			}
		}
		cursor = next
		if cursor == "0" {
			break
		}
	}
	return finishBigKeys(res), nil
}

func bigKeyLess(a, b bigKey) bool {
	if a.Bytes != b.Bytes {
		return a.Bytes > b.Bytes
	}
	return a.Elems > b.Elems
}

func trimBigKeys(l []bigKey) []bigKey {
	sort.SliceStable(l, func(i, j int) bool { return bigKeyLess(l[i], l[j]) })
	if len(l) > redisBigKeyKeep {
		l = l[:redisBigKeyKeep]
	}
	return l
}

func finishBigKeys(r *bigKeyResult) *bigKeyResult {
	r.Top = trimBigKeys(r.Top)
	return r
}
