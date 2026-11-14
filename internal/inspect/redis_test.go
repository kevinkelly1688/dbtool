// @author: kevinkelly
// @license: Apache Licence
// @file: redis_test.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dbtool/internal/engine/redis"
	"dbtool/internal/engine/redis/redistest"
	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// ---- 评估：检查项判定边界（不连任何实例） ----

var redisNow = ts("2026-10-06 08:00:00")

// healthyInfo 是一个各项都正常的 master 实例的 INFO。
func healthyInfo() map[string]string {
	return map[string]string{
		"redis_version": "7.0.11", "redis_mode": "standalone", "os": "Linux 5.15 x86_64", "uptime_in_seconds": "864000", "tcp_port": "6379", "role": "master", "connected_slaves": "1",
		"slave0": "ip=10.0.0.2,port=6379,state=online,offset=1000,lag=0", "master_repl_offset": "1000",
		"used_memory": "536870912", "used_memory_rss": "600000000", "used_memory_peak": "600000000", "maxmemory": "2147483648", "maxmemory_policy": "allkeys-lru",
		"mem_fragmentation_ratio": "1.12", "total_system_memory": "8589934592",
		"rdb_last_bgsave_status": "ok", "rdb_last_save_time": strconv.FormatInt(redisNow.Add(-time.Hour).Unix(), 10), "rdb_changes_since_last_save": "10", "aof_enabled": "1",
		"aof_last_write_status": "ok", "aof_last_bgrewrite_status": "ok", "aof_current_size": "1048576",
		"connected_clients": "50", "maxclients": "10000", "blocked_clients": "0", "rejected_connections": "0", "total_connections_received": "5000",
		"instantaneous_ops_per_sec": "1200", "keyspace_hits": "90000", "keyspace_misses": "10000", "evicted_keys": "0", "expired_keys": "300", "latest_fork_usec": "20000",
		"db0": "keys=100000,expires=60000,avg_ttl=3600000",
	}
}

func healthySnap() *redisSnapshot {
	return &redisSnapshot{
		Now: redisNow, Info: healthyInfo(), HasPassword: true,
		Config:   map[string]string{"requirepass": "x", "protected-mode": "yes", "bind": "10.0.0.1", "maxmemory": "2147483648", "maxmemory-policy": "allkeys-lru", "save": "3600 1", "appendonly": "yes", "maxclients": "10000"},
		CmdAvail: map[string]bool{"flushall": false, "flushdb": false, "keys": false, "config": false, "debug": false, "shutdown": false},
	}
}

func checkOf(t *testing.T, cs []redisCheck, id string) redisCheck {
	t.Helper()
	for _, c := range cs {
		if c.Section+"."+c.Key == id {
			return c
		}
	}
	t.Fatalf("没有检查项 %s", id)
	return redisCheck{}
}

func TestRedisHealthyAllNormal(t *testing.T) {
	cs := evalRedis(healthySnap())
	for _, c := range cs {
		if c.Level != LevelNormal {
			t.Errorf("健康实例的 %s.%s 应正常：level=%d %s", c.Section, c.Key, c.Level, c.Value)
		}
		if redisAdvice[c.Section+"."+c.Key] == "" && c.Key != "role" && c.Key != "ops" {
			t.Errorf("%s.%s 没有建议文案", c.Section, c.Key)
		}
	}
	if len(cs) < 20 {
		t.Errorf("检查项太少：%d", len(cs))
	}
	// 每一项都归属于已知分区
	known := map[string]bool{}
	for _, s := range redisSections {
		known[s[0]] = true
	}
	for _, c := range cs {
		if !known[c.Section] {
			t.Errorf("未知分区 %s", c.Section)
		}
	}
	h := RedisHealth(jmap{"checks": checksToJSON(cs)})
	if i64(h, "health_score") != 100 || sv(h, "status") != "healthy" {
		t.Errorf("健康实例应 100 分：%v", h)
	}
}

func checksToJSON(cs []redisCheck) []any {
	out := make([]any, 0, len(cs))
	for _, c := range cs {
		out = append(out, map[string]any{"section": c.Section, "key": c.Key, "name": c.Name, "value": c.Value, "level": float64(c.Level), "advice": c.Advice})
	}
	return out
}

// boundary 描述“把 INFO 的某个值改成 v，某个检查项应得到 want 级别”。
type boundary struct {
	id   string
	set  map[string]string
	want int
}

func TestRedisThresholdBoundaries(t *testing.T) {
	gb := int64(1 << 30)
	pctOf := func(pct int64) int64 { return (2*gb*pct + 99) / 100 } // 向上取整，避免整数除法让边界值差 1 字节
	mem := func(pct int64) map[string]string {
		return map[string]string{"used_memory": strconv.FormatInt(pctOf(pct), 10)}
	}
	cases := []boundary{
		// 内存使用率（maxmemory 2GB）：79 / 80 / 89 / 90
		{"memory.usage", mem(79), LevelNormal}, {"memory.usage", mem(80), LevelAttention}, {"memory.usage", mem(89), LevelAttention}, {"memory.usage", mem(90), LevelAlert},
		// 碎片率：1.49 / 1.5 / 1.99 / 2.0 / 0.9（交换）
		{"memory.frag", map[string]string{"mem_fragmentation_ratio": "1.49"}, LevelNormal}, {"memory.frag", map[string]string{"mem_fragmentation_ratio": "1.5"}, LevelAttention},
		{"memory.frag", map[string]string{"mem_fragmentation_ratio": "1.99"}, LevelAttention}, {"memory.frag", map[string]string{"mem_fragmentation_ratio": "2.0"}, LevelAlert},
		{"memory.frag", map[string]string{"mem_fragmentation_ratio": "0.9"}, LevelAlert},
		// 数据量太小（< 32MB）不评估碎片率，即使比值很高
		{"memory.frag", map[string]string{"mem_fragmentation_ratio": "9", "used_memory": "1000000"}, LevelNormal},
		// 峰值：占 maxmemory 94 / 95
		{"memory.peak", map[string]string{"used_memory_peak": strconv.FormatInt(pctOf(94), 10)}, LevelNormal}, {"memory.peak", map[string]string{"used_memory_peak": strconv.FormatInt(pctOf(95), 10)}, LevelAttention},
		// 连接数（maxclients 10000）：79% / 80% / 90%
		{"clients.usage", map[string]string{"connected_clients": "7999"}, LevelNormal}, {"clients.usage", map[string]string{"connected_clients": "8000"}, LevelAttention},
		{"clients.usage", map[string]string{"connected_clients": "9000"}, LevelAlert},
		// 阻塞客户端 9 / 10 / 50
		{"clients.blocked", map[string]string{"blocked_clients": "9"}, LevelNormal}, {"clients.blocked", map[string]string{"blocked_clients": "10"}, LevelAttention}, {"clients.blocked", map[string]string{"blocked_clients": "50"}, LevelAlert},
		// 被拒绝的连接 0 / 1 / 99 / 100
		{"clients.rejected", map[string]string{"rejected_connections": "1"}, LevelAttention}, {"clients.rejected", map[string]string{"rejected_connections": "99"}, LevelAttention}, {"clients.rejected", map[string]string{"rejected_connections": "100"}, LevelAlert},
		// 淘汰
		{"stats.evicted", map[string]string{"evicted_keys": "1"}, LevelAttention}, {"stats.evicted", map[string]string{"evicted_keys": "1000000"}, LevelAlert},
		// 命中率：请求不足 1 万不评估；90% 正常边界；89.9%；69.9%
		{"stats.hit", map[string]string{"keyspace_hits": "10", "keyspace_misses": "90"}, LevelNormal},
		{"stats.hit", map[string]string{"keyspace_hits": "90000", "keyspace_misses": "10000"}, LevelNormal},
		{"stats.hit", map[string]string{"keyspace_hits": "89900", "keyspace_misses": "10100"}, LevelAttention},
		{"stats.hit", map[string]string{"keyspace_hits": "69900", "keyspace_misses": "30100"}, LevelAlert},
		// fork 耗时 0.499s / 0.5s / 2s
		{"stats.fork", map[string]string{"latest_fork_usec": "499999"}, LevelNormal}, {"stats.fork", map[string]string{"latest_fork_usec": "500000"}, LevelAttention}, {"stats.fork", map[string]string{"latest_fork_usec": "2000000"}, LevelAlert},
		// 持久化
		{"persistence.rdb", map[string]string{"rdb_last_bgsave_status": "err"}, LevelAlert},
		{"persistence.aof", map[string]string{"aof_last_write_status": "err"}, LevelAlert},
		{"persistence.aof", map[string]string{"aof_last_bgrewrite_status": "err"}, LevelAlert},
		{"persistence.rdb", map[string]string{"rdb_last_save_time": strconv.FormatInt(redisNow.Add(-23*time.Hour).Unix(), 10)}, LevelNormal},
		{"persistence.rdb", map[string]string{"rdb_last_save_time": strconv.FormatInt(redisNow.Add(-25*time.Hour).Unix(), 10)}, LevelAttention},
		{"persistence.rdb", map[string]string{"rdb_last_save_time": strconv.FormatInt(redisNow.Add(-73*time.Hour).Unix(), 10)}, LevelAlert},
		// 久没保存但没有新变更：不算问题
		{"persistence.rdb", map[string]string{"rdb_last_save_time": strconv.FormatInt(redisNow.Add(-200*time.Hour).Unix(), 10), "rdb_changes_since_last_save": "0"}, LevelNormal},
		// 刚启动
		{"basic.uptime", map[string]string{"uptime_in_seconds": "599"}, LevelAttention}, {"basic.uptime", map[string]string{"uptime_in_seconds": "600"}, LevelNormal}, {"basic.uptime", map[string]string{"loading": "1"}, LevelAttention},
		// 版本
		{"basic.version", map[string]string{"redis_version": "4.0.14"}, LevelAttention}, {"basic.version", map[string]string{"redis_version": "5.0.14"}, LevelNormal},
		// 主从：从库状态 / 延迟
		{"replication.link", map[string]string{"slave0": "ip=10.0.0.2,port=6379,state=wait_bgsave,offset=0,lag=0"}, LevelAttention},
		{"replication.link", map[string]string{"slave0": "ip=10.0.0.2,port=6379,state=disconnected,offset=0,lag=0"}, LevelAlert},
		{"replication.lag", map[string]string{"slave0": "ip=10.0.0.2,port=6379,state=online,offset=1000,lag=9"}, LevelNormal},
		{"replication.lag", map[string]string{"slave0": "ip=10.0.0.2,port=6379,state=online,offset=1000,lag=10"}, LevelAttention},
		{"replication.lag", map[string]string{"slave0": "ip=10.0.0.2,port=6379,state=online,offset=1000,lag=30"}, LevelAlert},
		{"replication.lag", map[string]string{"master_repl_offset": strconv.FormatInt(1000+101<<20, 10)}, LevelAlert}, // 偏移差 > 100MB
	}
	for i, c := range cases {
		s := healthySnap()
		for k, v := range c.set {
			s.Info[k] = v
		}
		got := checkOf(t, evalRedis(s), c.id)
		if got.Level != c.want {
			t.Errorf("#%d %s 改 %v：level=%d，期望 %d（%s）", i, c.id, c.set, got.Level, c.want, got.Value)
		}
	}
}

func TestRedisReplicaChecks(t *testing.T) {
	s := healthySnap()
	s.Info = map[string]string{"redis_version": "7.0.11", "uptime_in_seconds": "99999", "role": "slave", "master_host": "10.0.0.1", "master_port": "6379",
		"master_link_status": "up", "master_last_io_seconds_ago": "1", "master_sync_in_progress": "0"}
	cs := evalRedis(s)
	if c := checkOf(t, cs, "replication.link"); c.Level != LevelNormal || !strings.Contains(c.Value, "up") {
		t.Errorf("正常从库: %+v", c)
	}
	if c := checkOf(t, cs, "replication.role"); !strings.Contains(c.Value, "10.0.0.1:6379") {
		t.Errorf("从库角色应带主库地址: %s", c.Value)
	}
	for io, want := range map[string]int{"9": LevelNormal, "10": LevelAttention, "59": LevelAttention, "60": LevelAlert, "-1": LevelAlert} {
		s.Info["master_last_io_seconds_ago"] = io
		if c := checkOf(t, evalRedis(s), "replication.lag"); c.Level != want {
			t.Errorf("io=%s level=%d 期望 %d (%s)", io, c.Level, want, c.Value)
		}
	}
	s.Info["master_last_io_seconds_ago"] = "1"
	s.Info["master_link_status"] = "down"
	s.Info["master_link_down_since_seconds"] = "125"
	if c := checkOf(t, evalRedis(s), "replication.link"); c.Level != LevelAlert || !strings.Contains(c.Value, "断开") {
		t.Errorf("链路断开应告警: %+v", c)
	}
	s.Info["master_link_status"] = "up"
	s.Info["master_sync_in_progress"] = "1"
	if c := checkOf(t, evalRedis(s), "replication.link"); c.Level != LevelAttention {
		t.Errorf("正在全量同步应关注: %+v", c)
	}
}

func TestRedisMemoryConfigChecks(t *testing.T) {
	// 没设置 maxmemory：关注；使用率改按主机内存算
	s := healthySnap()
	s.Info["maxmemory"] = "0"
	s.Config["maxmemory"] = "0"
	cs := evalRedis(s)
	if c := checkOf(t, cs, "memory.maxmemory"); c.Level != LevelAttention {
		t.Errorf("未设置 maxmemory 应关注: %+v", c)
	}
	if c := checkOf(t, cs, "memory.usage"); !strings.Contains(c.Value, "主机内存") || c.Level != LevelNormal {
		t.Errorf("无 maxmemory 时按主机内存算: %+v", c)
	}
	s.Info["used_memory"] = strconv.FormatInt(8589934592*85/100, 10)
	if c := checkOf(t, evalRedis(s), "memory.usage"); c.Level != LevelAttention {
		t.Errorf("主机内存 85%%: %+v", c)
	}
	// 上限也不知道 → 未采集
	delete(s.Info, "total_system_memory")
	if c := checkOf(t, evalRedis(s), "memory.usage"); c.Level != LevelFailed {
		t.Errorf("无上限可比应为未采集: %+v", c)
	}
	// noeviction + maxmemory：关注；没有 maxmemory 的 noeviction 不算
	s = healthySnap()
	s.Info["maxmemory_policy"] = "noeviction"
	if c := checkOf(t, evalRedis(s), "memory.policy"); c.Level != LevelAttention {
		t.Errorf("noeviction: %+v", c)
	}
	s.Info["maxmemory"], s.Config["maxmemory"] = "0", "0"
	if c := checkOf(t, evalRedis(s), "memory.policy"); c.Level != LevelNormal {
		t.Errorf("无 maxmemory 的 noeviction 不应关注: %+v", c)
	}
	// 过期键占比：volatile-* 策略下
	s = healthySnap()
	s.Info["maxmemory_policy"] = "volatile-lru"
	s.Info["db0"] = "keys=1000,expires=0,avg_ttl=0"
	if c := checkOf(t, evalRedis(s), "keyspace.summary"); c.Level != LevelAlert {
		t.Errorf("volatile 策略下没有任何过期键应告警: %+v", c)
	}
	s.Info["db0"] = "keys=1000,expires=99,avg_ttl=0"
	if c := checkOf(t, evalRedis(s), "keyspace.summary"); c.Level != LevelAttention {
		t.Errorf("9.9%%: %+v", c)
	}
	s.Info["db0"] = "keys=1000,expires=100,avg_ttl=0"
	if c := checkOf(t, evalRedis(s), "keyspace.summary"); c.Level != LevelNormal {
		t.Errorf("10%%: %+v", c)
	}
	s.Info["maxmemory_policy"] = "allkeys-lru"
	s.Info["db0"] = "keys=1000,expires=0,avg_ttl=0"
	if c := checkOf(t, evalRedis(s), "keyspace.summary"); c.Level != LevelNormal {
		t.Errorf("allkeys 策略不看过期键占比: %+v", c)
	}
	// 持久化都没开
	s = healthySnap()
	s.Info["aof_enabled"] = "0"
	s.Config["appendonly"], s.Config["save"] = "no", ""
	if c := checkOf(t, evalRedis(s), "persistence.mode"); c.Level != LevelAttention {
		t.Errorf("没有持久化: %+v", c)
	}
	if c := checkOf(t, evalRedis(s), "persistence.rdb"); c.Level != LevelNormal || !strings.Contains(c.Value, "未开启") {
		t.Errorf("RDB 未开启: %+v", c)
	}
	// 只有 RDB 或只有 AOF 都算有持久化
	s.Config["save"] = "900 1"
	if c := checkOf(t, evalRedis(s), "persistence.mode"); c.Level != LevelNormal {
		t.Errorf("只有 RDB: %+v", c)
	}
}

func TestRedisSecurityChecks(t *testing.T) {
	s := healthySnap()
	// 没有密码：告警
	s.HasPassword = false
	s.Config["requirepass"] = ""
	if c := checkOf(t, evalRedis(s), "security.password"); c.Level != LevelAlert {
		t.Errorf("无密码: %+v", c)
	}
	// requirepass 为空但平台是带密码连上的（ACL 管理）：不告警
	s.HasPassword = true
	if c := checkOf(t, evalRedis(s), "security.password"); c.Level != LevelNormal {
		t.Errorf("ACL: %+v", c)
	}
	// CONFIG GET 不可用：有密码 → 正常；没密码 → 告警
	s.Config, s.ConfigErr = nil, "ERR unknown command"
	if c := checkOf(t, evalRedis(s), "security.password"); c.Level != LevelNormal || !strings.Contains(c.Value, "CONFIG GET 不可用") {
		t.Errorf("config 不可用 + 有密码: %+v", c)
	}
	s.HasPassword = false
	if c := checkOf(t, evalRedis(s), "security.password"); c.Level != LevelAlert {
		t.Errorf("config 不可用 + 无密码: %+v", c)
	}
	if c := checkOf(t, evalRedis(s), "security.exposed"); c.Level != LevelFailed {
		t.Errorf("config 不可用时 protected-mode 未采集: %+v", c)
	}
	// 绑定所有网卡 + protected-mode no
	s = healthySnap()
	s.Config["bind"], s.Config["protected-mode"] = "0.0.0.0", "no"
	if c := checkOf(t, evalRedis(s), "security.exposed"); c.Level != LevelAttention {
		t.Errorf("暴露但有密码: %+v", c)
	}
	s.Config["requirepass"], s.HasPassword = "", false
	if c := checkOf(t, evalRedis(s), "security.exposed"); c.Level != LevelAlert {
		t.Errorf("暴露且无密码: %+v", c)
	}
	s.Config["protected-mode"] = "yes"
	if c := checkOf(t, evalRedis(s), "security.exposed"); c.Level != LevelNormal {
		t.Errorf("protected-mode yes: %+v", c)
	}
	s = healthySnap()
	s.Config["bind"], s.Config["protected-mode"] = "* -::*", "no" // Redis 7 的默认 bind
	if c := checkOf(t, evalRedis(s), "security.exposed"); c.Level != LevelAttention {
		t.Errorf("bind * -::*: %+v", c)
	}
	// 危险命令
	s = healthySnap()
	s.CmdAvail["flushall"], s.CmdAvail["keys"] = true, true
	c := checkOf(t, evalRedis(s), "security.commands")
	if c.Level != LevelAttention || !strings.Contains(c.Value, "FLUSHALL") || !strings.Contains(c.Value, "KEYS") || strings.Contains(c.Value, "SHUTDOWN") {
		t.Errorf("危险命令: %+v", c)
	}
	s.CmdAvail, s.CmdErr = nil, "ERR unknown command"
	if c := checkOf(t, evalRedis(s), "security.commands"); c.Level != LevelFailed {
		t.Errorf("COMMAND 不可用: %+v", c)
	}
}

func TestRedisSlowlogCheck(t *testing.T) {
	s := healthySnap()
	mk := func(n int64) {
		s.SlowLen = n
		s.SlowTop = []redis.SlowEntry{{ID: 1, Time: redisNow, DurationMicros: 250000, Args: []string{"HGETALL", "user:secret-1"}, Client: "10.0.0.9:1"}}
	}
	for n, want := range map[int64]int{0: LevelNormal, 9: LevelNormal, 10: LevelAttention, 99: LevelAttention, 100: LevelAlert} {
		mk(n)
		if c := checkOf(t, evalRedis(s), "stats.slowlog"); c.Level != want {
			t.Errorf("slowlog len=%d level=%d 期望 %d", n, c.Level, want)
		}
	}
	mk(12)
	c := checkOf(t, evalRedis(s), "stats.slowlog")
	if !strings.Contains(c.Value, "250.0 ms") || !strings.Contains(c.Value, "HGETALL") {
		t.Errorf("值里应有最慢命令: %s", c.Value)
	}
	if c.Table == nil || len(c.Table.Rows) != 1 {
		t.Fatalf("应有明细表: %+v", c.Table)
	}
	row := fmt.Sprint(c.Table.Rows[0])
	if strings.Contains(row, "secret") || !strings.Contains(row, "HGETALL ?") {
		t.Errorf("巡检报告里的慢命令只能是指纹，不能含 key 名: %s", row)
	}
	s.SlowErr = "NOPERM"
	if c := checkOf(t, evalRedis(s), "stats.slowlog"); c.Level != LevelFailed {
		t.Errorf("SLOWLOG 不可用应为未采集: %+v", c)
	}
}

func TestRedisMissingInfoIsNotCollected(t *testing.T) {
	s := &redisSnapshot{Now: redisNow, Info: map[string]string{"redis_version": "6.2.0"}, SlowErr: "x", CmdErr: "x", ConfigErr: "x"}
	cs := evalRedis(s)
	failed := 0
	for _, c := range cs {
		if c.Level == LevelFailed {
			failed++
		}
		if c.Level == LevelAlert && c.Section+"."+c.Key != "security.password" {
			t.Errorf("缺少数据的检查项不应告警：%s.%s %s", c.Section, c.Key, c.Value)
		}
	}
	if failed < 8 {
		t.Errorf("缺数据的检查项应标为未采集: %d", failed)
	}
	h := RedisHealth(jmap{"checks": checksToJSON(cs)})
	if score := i64(h, "health_score"); score < 50 {
		t.Errorf("未采集的项不应扣分过多: %d", score)
	}
}

func TestRedisHealthScoring(t *testing.T) {
	s := healthySnap()
	s.Info["used_memory"] = strconv.FormatInt(2<<30*95/100, 10) // 内存告警 -25
	s.Info["blocked_clients"] = "10"                            // 关注 -3
	s.Config["bind"], s.Config["protected-mode"] = "", "no"     // 暴露 -5（有密码）
	h := RedisHealth(jmap{"checks": checksToJSON(evalRedis(s))})
	if got := i64(h, "health_score"); got != 100-25-3-5 {
		t.Errorf("评分 %d，期望 %d: %v", got, 100-25-3-5, h)
	}
	if sv(h, "status") != "critical" {
		t.Errorf("内存告警扣 25 分应为 critical: %v", h["status"])
	}
	if len(strs(h, "issues")) != 1 || len(strs(h, "warnings")) != 2 || len(strs(h, "suggestions")) == 0 {
		t.Errorf("问题 / 警告 / 建议: %v", h)
	}
	// 评分不低于 0
	for _, k := range []string{"used_memory", "mem_fragmentation_ratio"} {
		s.Info[k] = "99999999999"
	}
	s.Info["rdb_last_bgsave_status"], s.Info["aof_last_write_status"] = "err", "err"
	s.Info["slave0"] = "ip=a,port=1,state=disconnected,offset=0,lag=0"
	s.Info["rejected_connections"], s.Info["evicted_keys"], s.Info["latest_fork_usec"] = "999", "9999999", "9999999"
	s.Config["requirepass"], s.HasPassword = "", false
	if got := i64(RedisHealth(jmap{"checks": checksToJSON(evalRedis(s))}), "health_score"); got != 0 {
		t.Errorf("评分应夹到 0: %d", got)
	}
}

// ---- 假 Redis（RESP）：端到端 ----

type fakeRedis struct {
	mu       sync.Mutex
	info     map[string]string
	config   map[string]string
	slow     []redis.SlowEntry
	cmdAvail map[string]bool // 没有的键视为已改名
	keys     map[string]fakeKey
	noMemory bool // MEMORY USAGE 不可用（Redis < 4）
	noConfig bool
	commands []string
}

type fakeKey struct {
	typ   string
	bytes int64
	elems int64
}

func (f *fakeRedis) infoText() string {
	keys := make([]string, 0, len(f.info))
	for k := range f.info {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Server\r\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s:%s\r\n", k, f.info[k])
	}
	return b.String()
}

func (f *fakeRedis) handler() redistest.Handler {
	return func(a []string) any {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.commands = append(f.commands, strings.Join(a, " "))
		switch strings.ToUpper(a[0]) {
		case "AUTH", "SELECT":
			return redistest.Simple("OK")
		case "PING":
			return redistest.Simple("PONG")
		case "INFO":
			return f.infoText()
		case "CONFIG":
			if f.noConfig {
				return redistest.Err("ERR unknown command 'CONFIG'")
			}
			var out []any
			for _, p := range a[2:] {
				if v, ok := f.config[p]; ok {
					out = append(out, p, v)
				}
			}
			return out
		case "SLOWLOG":
			if strings.EqualFold(a[1], "LEN") {
				return int64(len(f.slow))
			}
			var out []any
			for _, e := range f.slow {
				args := make([]any, len(e.Args))
				for i, x := range e.Args {
					args[i] = x
				}
				out = append(out, []any{e.ID, e.Time.Unix(), e.DurationMicros, args, e.Client, e.ClientName})
			}
			return out
		case "COMMAND":
			out := make([]any, 0, len(a)-2)
			for _, n := range a[2:] {
				if f.cmdAvail[strings.ToLower(n)] {
					out = append(out, []any{strings.ToLower(n), int64(-1)})
				} else {
					out = append(out, nil)
				}
			}
			return out
		case "SCAN":
			var names []any
			var all []string
			for k := range f.keys {
				all = append(all, k)
			}
			sort.Strings(all)
			for _, k := range all {
				names = append(names, k)
			}
			return []any{"0", names}
		case "TYPE":
			if k, ok := f.keys[a[1]]; ok {
				return redistest.Simple(k.typ)
			}
			return redistest.Simple("none")
		case "MEMORY":
			if f.noMemory {
				return redistest.Err("ERR unknown command 'MEMORY'")
			}
			return f.keys[a[2]].bytes
		case "STRLEN", "LLEN", "HLEN", "SCARD", "ZCARD", "XLEN":
			return f.keys[a[1]].elems
		}
		return redistest.Err("ERR unknown command '" + a[0] + "'")
	}
}

func newFakeRedis() *fakeRedis {
	info := healthyInfo()
	info["rdb_last_save_time"] = strconv.FormatInt(redisNow.Add(-time.Hour).Unix(), 10)
	return &fakeRedis{
		info:     info,
		config:   map[string]string{"requirepass": "x", "protected-mode": "yes", "bind": "10.0.0.1", "maxmemory": "2147483648", "maxmemory-policy": "allkeys-lru", "save": "3600 1", "appendonly": "yes", "maxclients": "10000", "databases": "16"},
		cmdAvail: map[string]bool{},
		keys:     map[string]fakeKey{},
	}
}

type redisInspectEnv struct {
	*env
	fake *fakeRedis
	srv  *redistest.Server
}

// newRedisInspectEnv 在 newEnv 的基础上换一个启用 Redis 巡检的服务，实例 4 指向进程内的假 Redis。
func newRedisInspectEnv(t *testing.T) *redisInspectEnv {
	t.Helper()
	e := newEnv()
	fr := newFakeRedis()
	srv, err := redistest.New(fr.handler())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	e.repo.insts[3] = model.Instance{ID: 4, InstanceName: "redis-01", DBType: "redis", Host: srv.Host(), Port: srv.Port()}
	old := e.svc
	e.svc = NewService(e.repo, Options{Now: e.clk.Now, Notifier: e.nt, BaseURL: "https://dba.example.com/", Open: old.open, OpenRedis: OpenRedis, Sleep: old.sleep,
		Manual: 4, BusyPoll: time.Second, RetryDelay: time.Minute})
	return &redisInspectEnv{env: e, fake: fr, srv: srv}
}

func TestRedisInspectEndToEnd(t *testing.T) {
	e := newRedisInspectEnv(t)
	ref, out := e.run(t, KindRedis, 4, RunOptions{})
	if out.Status != StatusFinish || ref.String() != "redis-1" {
		t.Fatalf("outcome=%+v", out)
	}
	if out.Score == nil || *out.Score != 100 {
		t.Errorf("健康实例应 100 分: %v", out.Score)
	}
	view, err := e.svc.GetReport(all, ref)
	if err != nil {
		t.Fatal(err)
	}
	if view.Engine != "Redis" || len(view.Facts) < 5 || !strings.Contains(view.Facts[0].Value, "Redis 7.0.11") {
		t.Errorf("摘要: %+v engine=%s", view.Facts, view.Engine)
	}
	codes := []string{}
	total := 0
	for _, s := range view.Sections {
		codes = append(codes, s.Code)
		total += len(s.Items)
		for _, it := range s.Items {
			if it.Advice == "" && it.Level != LevelNormal {
				t.Errorf("%s 没有建议", it.Name)
			}
		}
	}
	if strings.Join(codes, ",") != "basic,replication,memory,persistence,clients,stats,security,keyspace" {
		t.Errorf("分区: %v", codes)
	}
	if total < 20 {
		t.Errorf("检查项数 %d", total)
	}
	// 只发了只读命令：没有任何写 / 危险命令；大 key 抽样默认关闭，不 SCAN
	for _, c := range e.fake.commands {
		name := strings.ToUpper(strings.Fields(c)[0])
		switch name {
		case "AUTH", "INFO", "CONFIG", "SLOWLOG", "COMMAND", "SELECT", "PING":
		default:
			t.Errorf("巡检不应发出命令 %s", c)
		}
		if strings.Contains(strings.ToUpper(c), "CONFIG SET") || strings.Contains(strings.ToUpper(c), "RESET") {
			t.Errorf("危险命令: %s", c)
		}
	}
	// 连接信息里有账号密码：AUTH u p
	if len(e.fake.commands) == 0 || e.fake.commands[0] != "AUTH u p" {
		t.Errorf("应先认证: %v", e.fake.commands[:1])
	}
	rep := e.repo.report(KindRedis, 1)
	if rep.Status != StatusFinish || rep.Total != redisSteps || rep.Done != redisSteps || rep.Result == "" {
		t.Errorf("报告落库: %+v", rep.ReportSummary)
	}
}

func TestRedisInspectProblemsAndBigKeys(t *testing.T) {
	e := newRedisInspectEnv(t)
	f := e.fake
	f.info["rdb_last_bgsave_status"] = "err"
	f.info["used_memory"] = strconv.FormatInt(2<<30*92/100, 10)
	f.cmdAvail["flushall"], f.cmdAvail["keys"] = true, true
	f.slow = []redis.SlowEntry{{ID: 5, Time: redisNow, DurationMicros: 400000, Args: []string{"KEYS", "*"}, Client: "10.0.0.7:1234"}}
	f.keys = map[string]fakeKey{"small": {"string", 100, 1}, "big:list": {"list", 12 << 20, 500000}, "mid:hash": {"hash", 2 << 20, 20000}}
	ref, out := e.run(t, KindRedis, 4, RunOptions{RedisBigKeys: 100})
	if out.Status != StatusFinish || out.Alert < 3 || out.Attention < 1 {
		t.Fatalf("outcome=%+v", out)
	}
	if out.Score == nil || *out.Score >= 70 {
		t.Errorf("问题实例评分应较低: %v", out.Score)
	}
	view, _ := e.svc.GetReport(all, ref)
	var bk *Item
	for _, s := range view.Sections {
		for i, it := range s.Items {
			if it.Key == "bigkey" {
				bk = &s.Items[i]
			}
		}
	}
	if bk == nil || bk.Level != LevelAlert || !strings.Contains(bk.Value, "12.0 MB") || bk.Table == nil || len(bk.Table.Rows) != 3 {
		t.Fatalf("大 key 抽样: %+v", bk)
	}
	if first := fmt.Sprint(bk.Table.Rows[0]); !strings.Contains(first, "big:list") {
		t.Errorf("应按大小降序: %s", first)
	}
	if len(view.Blocks) == 0 || view.Blocks[0].Kind != "err" {
		t.Errorf("应有“需处理的问题”: %+v", view.Blocks)
	}
	// 抽样只用了只读命令
	for _, c := range f.commands {
		switch strings.ToUpper(strings.Fields(c)[0]) {
		case "AUTH", "INFO", "CONFIG", "SLOWLOG", "COMMAND", "SELECT", "PING", "SCAN", "TYPE", "MEMORY":
		default:
			t.Errorf("意外的命令 %s", c)
		}
	}
}

func TestRedisBigKeySamplingLimits(t *testing.T) {
	e := newRedisInspectEnv(t)
	for i := 0; i < 50; i++ {
		e.fake.keys[fmt.Sprintf("k%02d", i)] = fakeKey{"string", int64(i + 1), 1}
	}
	cl, err := OpenRedis(context.Background(), e.repo.Conn(&e.repo.insts[3]))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	res, err := sampleBigKeys(context.Background(), cl, 20, "db0")
	if err != nil || res.Scanned != 20 || !res.Exact {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(res.Top) != redisBigKeyKeep || res.Top[0].Bytes < res.Top[1].Bytes {
		t.Errorf("应保留最大的 %d 个并降序: %+v", redisBigKeyKeep, res.Top)
	}
	// 数量上限：n 很大也不超过硬上限（这里只有 50 个 key）
	res, _ = sampleBigKeys(context.Background(), cl, 1<<30, "db0")
	if res.Scanned != 50 {
		t.Errorf("scanned=%d", res.Scanned)
	}
	// MEMORY USAGE 不可用：退回元素个数
	e.fake.noMemory = true
	e.fake.keys = map[string]fakeKey{"q": {"list", 0, 200000}, "r": {"list", 0, 5}}
	res, err = sampleBigKeys(context.Background(), cl, 10, "db0")
	if err != nil || res.Exact || res.Top[0].Elems != 200000 {
		t.Fatalf("降级: %+v err=%v", res, err)
	}
	// 已取消的 ctx：立即返回
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sampleBigKeys(ctx, cl, 10, "db0"); err == nil {
		t.Error("取消后应返回错误")
	}
	// 上限被夹住
	snap, _ := collectRedis(context.Background(), cl, e.repo.Conn(&e.repo.insts[3]), redisNow, 1<<30, nil)
	if snap.BigKeyN != redisBigKeyMax {
		t.Errorf("BigKeyN=%d，应夹到 %d", snap.BigKeyN, redisBigKeyMax)
	}
}

func TestRedisInspectFailures(t *testing.T) {
	e := newRedisInspectEnv(t)
	// CONFIG 被禁用：对应检查项未采集，报告照常完成
	e.fake.noConfig = true
	delete(e.fake.config, "requirepass")
	ref, out := e.run(t, KindRedis, 4, RunOptions{})
	if out.Status != StatusFinish || out.Failed == 0 {
		t.Fatalf("outcome=%+v", out)
	}
	view, _ := e.svc.GetReport(all, ref)
	found := false
	for _, s := range view.Sections {
		for _, it := range s.Items {
			if it.Key == "exposed" && it.Level == LevelFailed && strings.Contains(it.Value, "CONFIG GET 不可用") {
				found = true
			}
		}
	}
	if !found {
		t.Error("CONFIG 不可用时 protected-mode 项应为未采集")
	}

	// 实例连不上：报告失败，错误里没有密码
	e.srv.Close()
	ref, out = e.run(t, KindRedis, 4, RunOptions{})
	if out.Status != StatusFailed || out.Err == "" {
		t.Fatalf("连不上应失败: %+v", out)
	}
	if r := e.repo.report(KindRedis, ref.ID); r.Status != StatusFailed || r.Error == "" {
		t.Errorf("报告: %+v", r.ReportSummary)
	}
	v, _ := e.svc.GetReport(all, ref)
	if len(v.Blocks) == 0 || v.Blocks[0].Kind != "err" {
		t.Errorf("失败的报告应显示原因: %+v", v.Blocks)
	}
}

func TestRedisAuthFailure(t *testing.T) {
	srv, _ := redistest.New(func(a []string) any {
		if strings.EqualFold(a[0], "AUTH") {
			return redistest.Err("WRONGPASS invalid username-password pair or user is disabled.")
		}
		return redistest.Simple("OK")
	})
	defer srv.Close()
	e := newRedisInspectEnv(t)
	e.repo.insts[3].Host, e.repo.insts[3].Port = srv.Host(), srv.Port()
	ref, out := e.run(t, KindRedis, 4, RunOptions{})
	if out.Status != StatusFailed || !strings.Contains(out.Err, "认证失败") {
		t.Fatalf("认证失败应让报告失败并说明原因: %+v", out)
	}
	if r := e.repo.report(KindRedis, ref.ID); r.Status != StatusFailed || !strings.Contains(r.Error, "认证失败") {
		t.Errorf("报告: %+v", r.ReportSummary)
	}
}

// ---- 服务：支持范围、立即巡检、计划 ----

func TestRedisSupportOnlyWhenEnabled(t *testing.T) {
	// 没配置 Redis 打开函数（默认 / 旧行为）：Redis 实例暂不支持
	e := newEnv()
	if _, ok := e.svc.kindOf("redis"); ok {
		t.Error("未启用 Redis 巡检时 redis 应不支持")
	}
	// 启用后：支持；包级 KindOf 不变
	e2 := newRedisInspectEnv(t)
	if k, ok := e2.svc.kindOf("Redis"); !ok || k != KindRedis {
		t.Errorf("kindOf(Redis)=%s %v", k, ok)
	}
	if _, ok := KindOf("redis"); ok {
		t.Error("包级 KindOf 应保持不变")
	}
	if _, ok := e2.svc.kindOf("mongo"); ok {
		t.Error("mongo 仍不支持")
	}
	if KindLabel("redis") != "Redis" {
		t.Error("KindLabel")
	}
	if r, err := ParseRef("redis-7"); err != nil || r.Kind != KindRedis || r.ID != 7 || r.String() != "redis-7" {
		t.Errorf("%+v %v", r, err)
	}
	opts, _, err := e2.svc.Instances(all)
	if err != nil {
		t.Fatal(err)
	}
	hasRedis := false
	for _, o := range opts {
		if o.Kind == KindRedis && o.Label == "Redis" {
			hasRedis = true
		}
	}
	if !hasRedis {
		t.Errorf("实例选项里应有 Redis: %+v", opts)
	}
}

func TestRedisRunNowAndBigKeyOption(t *testing.T) {
	e := newRedisInspectEnv(t)
	e.fake.keys = map[string]fakeKey{"a": {"string", 10, 1}}
	res, err := e.svc.RunNow(admin, all, []uint{4})
	if err != nil || len(res.Created) != 1 || res.Created[0].Ref != "redis-1" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	e.waitIdle()
	if rep := e.repo.report(KindRedis, 1); rep.Status != StatusFinish {
		t.Fatalf("%+v", rep.ReportSummary)
	}
	for _, c := range e.fake.commands {
		if strings.HasPrefix(c, "SCAN") {
			t.Error("默认不应抽样大 key")
		}
	}
	// 勾选大 key 抽样
	res, err = e.svc.RunNowWith(admin, all, []uint{4}, RunNowOptions{RedisBigKeys: true})
	if err != nil || len(res.Created) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	e.waitIdle()
	scanned := false
	for _, c := range e.fake.commands {
		if strings.HasPrefix(c, "SCAN") {
			scanned = true
		}
	}
	if !scanned {
		t.Error("勾选后应抽样")
	}
	if acts := e.repo.auditActions(); len(acts) != 2 || acts[0] != "性能巡检·立即巡检" {
		t.Errorf("审计: %v", acts)
	}
}

func TestRedisInPlan(t *testing.T) {
	e := newRedisInspectEnv(t)
	p := basePlan()
	p.InstanceIDs, p.Enabled, p.StaggerSeconds = []uint{1, 4}, false, 0
	saved := e.savePlan(t, p)
	runID, err := e.svc.RunPlanNow(admin, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	run := e.runOf(t, runID)
	if run.OK != 2 {
		t.Fatalf("run=%+v", run)
	}
	items, _ := e.repo.RunItems(runID)
	kinds := map[string]bool{}
	for _, it := range items {
		kinds[it.Kind] = true
	}
	if !kinds[KindMySQL] || !kinds[KindRedis] {
		t.Errorf("运行明细应含 mysql 与 redis 报告: %+v", items)
	}
	if rep := e.repo.report(KindRedis, 1); rep == nil || rep.Status != StatusFinish || rep.Trigger != TriggerManual {
		t.Errorf("计划产生的 redis 报告: %+v", rep)
	}
}

func TestRedisCancelAndDelete(t *testing.T) {
	e := newRedisInspectEnv(t)
	ref, out := e.run(t, KindRedis, 4, RunOptions{})
	if out.Status != StatusFinish {
		t.Fatal(out)
	}
	res, err := e.svc.Delete(admin, all, []ReportRef{ref})
	if err != nil || res.Deleted != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if e.repo.report(KindRedis, ref.ID) != nil {
		t.Error("应已删除")
	}
	// 范围外的实例看不到
	ref, _ = e.run(t, KindRedis, 4, RunOptions{})
	if _, err := e.svc.GetReport(nobody, ref); err == nil {
		t.Error("范围外应按不存在处理")
	}
}

func TestRedisBigKeysEnv(t *testing.T) {
	for in, want := range map[string]int{"": 0, "abc": 0, "-5": 0, "1000": 1000, "999999": redisBigKeyMax} {
		t.Setenv("REDIS_INSPECT_BIGKEY_SAMPLE", in)
		if got := redisBigKeysFromEnv(); got != want {
			t.Errorf("env=%q → %d，期望 %d", in, got, want)
		}
	}
}

func TestRedisHTTP(t *testing.T) {
	e := newRedisInspectEnv(t)
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){registerWith(e.svc, testHooks())}})
	env.Repo.Perms[2] = map[string]bool{}
	grant(env, 2, "inspect.view")
	root, bob := env.Login("root"), env.Login("bob")

	// 选项里有 Redis 实例（root 看全部）；bob 的范围只有实例 1
	w := root.Do("GET", "/api/v1/inspection/options", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"kind":"redis"`) || !strings.Contains(w.Body.String(), `"label":"Redis"`) {
		t.Errorf("options: %d %s", w.Code, w.Body.String())
	}
	if w := bob.Do("GET", "/api/v1/inspection/options", nil); strings.Contains(w.Body.String(), `"kind":"redis"`) {
		t.Error("bob 看不到 redis 实例")
	}
	// 立即巡检需要 inspect.run（默认仅超级用户）
	if code, _ := status(bob, "POST", "/api/v1/inspection/reports/run", map[string]any{"instance_ids": []uint{4}, "redis_bigkey": true}); code != 403 {
		t.Errorf("bob 立即巡检应 403: %d", code)
	}
	w = root.Do("POST", "/api/v1/inspection/reports/run", map[string]any{"instance_ids": []uint{4}, "redis_bigkey": true})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ref":"redis-1"`) {
		t.Fatalf("root 立即巡检: %d %s", w.Code, w.Body.String())
	}
	e.waitIdle()
	// 列表含 redis 报告，带评分和类型
	list := getList(t, root, "/api/v1/inspection/reports")
	if len(list.Data) != 1 || list.Data[0].Ref != "redis-1" || list.Data[0].TypeLabel != "Redis" || list.Data[0].Status != StatusFinish {
		t.Fatalf("list=%+v", list)
	}
	// 详情
	w = root.Do("GET", "/api/v1/inspection/reports/redis-1", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"engine":"Redis"`) || !strings.Contains(w.Body.String(), "内存使用率") || !strings.Contains(w.Body.String(), `"health_score":100`) {
		t.Errorf("详情: %d %s", w.Code, truncate(w.Body.String(), 200))
	}
	// bob 看不到范围外实例的报告
	if code, _ := status(bob, "GET", "/api/v1/inspection/reports/redis-1", nil); code != 404 {
		t.Errorf("范围外的报告应 404: %d", code)
	}
	if code, p := bob.Page("/inspection/redis-1"); code != 200 {
		t.Errorf("详情页: %d %s", code, truncate(p, 60))
	}
	// 删除需要 inspect.delete（仅超级用户）
	if code, _ := status(bob, "POST", "/api/v1/inspection/reports/delete", map[string]any{"ids": []string{"redis-1"}}); code != 403 {
		t.Errorf("bob 删除应 403: %d", code)
	}
	if code, _ := status(root, "POST", "/api/v1/inspection/reports/delete", map[string]any{"ids": []string{"redis-1"}}); code != 200 {
		t.Errorf("root 删除: %d", code)
	}
}
