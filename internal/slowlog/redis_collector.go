// @author: kevinkelly
// @license: Apache Licence
// @file: redis_collector.go
// @time: 2026/10/28

package slowlog

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dbtool/internal/engine"
	"dbtool/internal/engine/redis"
	"dbtool/internal/model"
)

// RedisCollector 定时对 redis 实例执行 SLOWLOG GET，增量写入 dbt_redis_slowlog。
//
//   - 接入现有的慢日志采集调度：Collector.Run 每轮结束后调用它（与 MySQL 采集一样默认开启，环境变量 SLOWLOG_COLLECT_ENABLE=0 整体关闭），
//     但 Redis 实例两次采集之间至少间隔 MinGap（默认 60 秒）——SLOWLOG 是内存环形缓冲，不需要像 mysql.slow_log 那样高频拉取；
//   - 采集开关是 dbt_sql_instance.is_enable；单实例失败只记录状态，连续失败指数退避；
//   - 游标 = 已采集的最大条目 id + 它的时间。Redis 重启后条目 id 会从 0 重新计数（SLOWLOG RESET 不会重置 id），
//     所以看到最新条目 id 比游标还小就认为实例重启过，改按时间过滤；写入还有 (实例, id, 开始时间) 唯一键兜底去重；
//   - 只发 SLOWLOG LEN / SLOWLOG GET，不改动实例上的任何数据。
type RedisCollector struct {
	repo   RedisRepo
	open   RedisOpener
	conn   func(inst *model.Instance) engine.Conn
	now    func() time.Time
	MinGap time.Duration
	// Workers 是并行采集的实例数。
	Workers int

	mu        sync.Mutex
	nextTry   map[uint]time.Time
	failures  map[uint]int
	lastPurge time.Time
	warned    sync.Once
}

// NewRedisCollector 创建采集器。conn 解密凭据（store.EngineConn）。
func NewRedisCollector(repo RedisRepo, open RedisOpener, conn func(*model.Instance) engine.Conn, now func() time.Time) *RedisCollector {
	if now == nil {
		now = time.Now
	}
	return &RedisCollector{repo: repo, open: open, conn: conn, now: now, MinGap: time.Minute, Workers: 2,
		nextTry: map[uint]time.Time{}, failures: map[uint]int{}}
}

// RunOnce 采集一轮：所有开启采集开关的 redis 实例，到了时间的才采。
func (c *RedisCollector) RunOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if !c.repo.RedisTablesReady() {
		c.warned.Do(func() {
			slog.Warn("Redis 慢日志采集：表还没有创建，请执行 deploy/sql/130_redis_slowlog.sql")
		})
		return
	}
	insts, err := c.repo.RedisInstances()
	if err != nil {
		slog.Error("读取待采集的 Redis 实例失败", "err", err)
		return
	}
	sem := make(chan struct{}, max(c.Workers, 1))
	var wg sync.WaitGroup
	for i := range insts {
		inst := insts[i]
		if !c.due(inst.ID) {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			c.collectInstance(ctx, &inst)
		}()
	}
	wg.Wait()
	c.purge()
}

func (c *RedisCollector) due(id uint) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.now().Before(c.nextTry[id])
}

// purge 清理超过保留期（与页面可查询的最长时间范围一致）的明细，限量、每小时最多一次。
func (c *RedisCollector) purge() {
	c.mu.Lock()
	if c.now().Sub(c.lastPurge) < redisPurgeEvery {
		c.mu.Unlock()
		return
	}
	c.lastPurge = c.now()
	c.mu.Unlock()
	cut := c.now().Add(-redisSlowRetention)
	for i := 0; i < 20; i++ {
		n, err := c.repo.PurgeRedisSlowlogs(cut, redisPurgeBatch)
		if err != nil {
			slog.Warn("清理过期的 Redis 慢日志失败", "err", err)
			return
		}
		if n < redisPurgeBatch {
			return
		}
	}
}

// CollectNow 立即采集一个实例（页面的“立即采集”按钮、重置前的保底采集）；返回新增条数。
func (c *RedisCollector) CollectNow(ctx context.Context, inst *model.Instance) (int, error) {
	return c.pull(ctx, inst)
}

func (c *RedisCollector) collectInstance(ctx context.Context, inst *model.Instance) {
	start := c.now()
	prev, _ := c.repo.RedisState(inst.ID)
	st := RedisState{InstanceID: inst.ID, LastSlowID: -1, LastRunAt: start}
	if prev != nil {
		st = *prev
		st.LastRunAt = start
	}
	n, err := c.pull(ctx, inst)
	st.DurationMs = c.now().Sub(start).Milliseconds()
	c.mu.Lock()
	if err != nil {
		c.failures[inst.ID]++
		gap := c.MinGap
		for i := 1; i < c.failures[inst.ID] && gap < 10*time.Minute; i++ {
			gap *= 2
		}
		c.nextTry[inst.ID] = c.now().Add(min(gap, 10*time.Minute))
	} else {
		delete(c.failures, inst.ID)
		c.nextTry[inst.ID] = c.now().Add(c.MinGap)
	}
	c.mu.Unlock()
	if err != nil {
		st.Status, st.Error, st.LastRows = "error", errText(err), 0
		slog.Warn("Redis 慢日志采集失败", "instance", inst.InstanceName, "err", st.Error)
	} else {
		st.Status, st.Error, st.LastRows, st.LastOKAt = "ok", "", n, c.now()
		st.TotalRows += int64(n)
		if n > 0 {
			slog.Info("Redis 慢日志采集", "instance", inst.InstanceName, "rows", n)
		}
	}
	// pull 已经推进了游标；这里只写状态（游标以 pull 里保存的为准，再读一次避免覆盖）
	if cur, _ := c.repo.RedisState(inst.ID); cur != nil {
		st.LastSlowID, st.LastSlowTime = cur.LastSlowID, cur.LastSlowTime
	}
	if err := c.repo.SaveRedisState(st); err != nil {
		slog.Warn("保存 Redis 慢日志采集状态失败", "instance", inst.InstanceName, "err", err)
	}
}

// pull 对单个实例拉取并写入，返回新增条数。panic 转成错误，不影响其他实例。
func (c *RedisCollector) pull(ctx context.Context, inst *model.Instance) (n int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("采集异常：%v", r)
		}
	}()
	// 配置了 SSH 隧道的实例不再跳过：c.conn（store.EngineConn）已把 Host/Port 改写为本地转发器地址。
	ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	t, err := c.open(ictx, c.conn(inst))
	if err != nil {
		return 0, err
	}
	defer t.Close()
	total, err := t.SlowlogLen(ictx)
	if err != nil {
		return 0, fmt.Errorf("SLOWLOG LEN: %w", err)
	}
	st, _ := c.repo.RedisState(inst.ID)
	if st == nil {
		st = &RedisState{InstanceID: inst.ID, LastSlowID: -1}
	}
	if total == 0 {
		return 0, nil
	}
	entries, err := t.SlowlogGet(ictx, int(min(total, redisSlowFetchMax)))
	if err != nil {
		return 0, fmt.Errorf("SLOWLOG GET: %w", err)
	}
	fresh, newestID, newestTime := SelectNewEntries(entries, st.LastSlowID, st.LastSlowTime)
	if len(fresh) == 0 {
		return 0, nil
	}
	rows := make([]RedisSlowRow, 0, len(fresh))
	for _, e := range fresh {
		rows = append(rows, ToSlowRow(e))
	}
	inserted, err := c.repo.InsertRedisSlowlogs(inst.ID, rows)
	if err != nil {
		return 0, fmt.Errorf("写入 dbt_redis_slowlog: %w", err)
	}
	st.InstanceID, st.LastSlowID, st.LastSlowTime = inst.ID, newestID, newestTime
	if err := c.repo.SaveRedisState(*st); err != nil {
		return inserted, fmt.Errorf("保存游标: %w", err)
	}
	return inserted, nil
}

// SelectNewEntries 从 SLOWLOG GET 的结果（最新在前）里挑出还没采集的条目，按旧→新返回，并给出新的游标。
//
//   - 正常情况：条目 id 大于游标 id 的是新的；
//   - Redis 重启后 id 从 0 重新计数：最新条目 id 比游标小，改为按时间过滤（开始时间不早于游标时间）；
//   - 游标为 -1（第一次采集）：全部算新的。
func SelectNewEntries(entries []redis.SlowEntry, lastID int64, lastTime time.Time) (fresh []redis.SlowEntry, newestID int64, newestTime time.Time) {
	if len(entries) == 0 {
		return nil, lastID, lastTime
	}
	maxID := entries[0].ID
	for _, e := range entries {
		maxID = max(maxID, e.ID)
	}
	restarted := lastID >= 0 && maxID < lastID
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		switch {
		case lastID < 0:
		case restarted:
			if e.Time.Before(lastTime) {
				continue
			}
		default:
			if e.ID <= lastID {
				continue
			}
		}
		fresh = append(fresh, e)
	}
	if len(fresh) == 0 {
		return nil, lastID, lastTime
	}
	newestID, newestTime = fresh[0].ID, fresh[0].Time
	for _, e := range fresh {
		if e.Time.After(newestTime) || (e.Time.Equal(newestTime) && e.ID > newestID) {
			newestID, newestTime = e.ID, e.Time
		}
	}
	return fresh, newestID, newestTime
}

// ToSlowRow 把一条 SLOWLOG 条目整理成入库行：指纹化、命令文本脱敏并限长。
func ToSlowRow(e redis.SlowEntry) RedisSlowRow {
	args := MaskArgs(e.Args)
	fp := redis.Fingerprint(args)
	sum := md5.Sum([]byte(fp))
	return RedisSlowRow{SlowID: e.ID, StartTime: e.Time, DurationUs: e.DurationMicros, CommandName: redis.CommandName(args), Fingerprint: clipRunes(fp, 255),
		FpID: hex.EncodeToString(sum[:]), CommandText: clipRunes(CommandText(args), redisSlowTextMax), ClientAddr: clipRunes(e.Client, 64), ClientName: clipRunes(e.ClientName, 128)}
}

// CommandText 把命令参数还原成一行文本：含空白或为空的参数加双引号。
func CommandText(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\r\n\"") {
			a = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`).Replace(a) + `"`
		}
		parts[i] = a
	}
	return strings.Join(parts, " ")
}

// MaskArgs 抹掉命令里的密码类参数（Redis 7 以前的慢日志不会自己脱敏）：AUTH、HELLO … AUTH、MIGRATE … AUTH/AUTH2、
// CONFIG SET requirepass/masterauth/masteruser、ACL SETUSER 里的 >密码 / #哈希。
func MaskArgs(args []string) []string {
	if len(args) == 0 {
		return args
	}
	out := append([]string{}, args...)
	name := strings.ToUpper(out[0])
	switch name {
	case "AUTH":
		for i := 1; i < len(out); i++ {
			out[i] = "***"
		}
	case "HELLO", "MIGRATE":
		for i := 1; i < len(out); i++ {
			switch strings.ToUpper(out[i]) {
			case "AUTH":
				for j := i + 1; j <= i+2 && j < len(out); j++ {
					out[j] = "***"
				}
				i += 2
			case "AUTH2":
				for j := i + 1; j <= i+2 && j < len(out); j++ {
					out[j] = "***"
				}
				i += 2
			}
		}
	case "CONFIG":
		if len(out) >= 4 && strings.EqualFold(out[1], "SET") {
			for i := 2; i+1 < len(out); i += 2 {
				switch strings.ToLower(out[i]) {
				case "requirepass", "masterauth", "masteruser":
					out[i+1] = "***"
				}
			}
		}
	case "ACL":
		if len(out) >= 3 && strings.EqualFold(out[1], "SETUSER") {
			for i := 3; i < len(out); i++ {
				if strings.HasPrefix(out[i], ">") || strings.HasPrefix(out[i], "#") || strings.HasPrefix(out[i], "<") || strings.HasPrefix(out[i], "!") {
					out[i] = out[i][:1] + "***"
				}
			}
		}
	}
	return out
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
