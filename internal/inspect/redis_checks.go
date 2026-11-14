// @author: kevinkelly
// @license: Apache Licence
// @file: redis_checks.go
// @time: 2026/10/28

package inspect

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/engine/redis"
)

// Redis 巡检的检查项判定。评估只依赖 redisSnapshot（redis_collect.go 采集到的原始数据），不访问实例，便于边界测试。
// 每项有：分区、键、名称、当前值、阈值说明、级别（0 正常 / 1 关注 / 2 告警 / 3 未采集）、建议。

// 判定阈值。
const (
	redisUsageAttentionPct    = 80
	redisUsageAlertPct        = 90
	redisFragMinBytes         = 32 << 20 // 已用内存小于它不评估碎片率（数据太少，比值没有意义）
	redisFragAttention        = 1.5
	redisFragAlert            = 2.0
	redisFragSwap             = 1.0 // 低于 1 表示部分内存被换出到磁盘
	redisPeakAttentionPct     = 95
	redisRDBAgeAttention      = 24 * time.Hour
	redisRDBAgeAlert          = 72 * time.Hour
	redisClientsAttentionPct  = 80
	redisClientsAlertPct      = 90
	redisBlockedAttention     = 10
	redisBlockedAlert         = 50
	redisRejectedAlert        = 100
	redisEvictedAlert         = 1000000
	redisHitMinRequests       = 10000 // 命中率样本太少时不评估
	redisHitAttentionPct      = 90
	redisHitAlertPct          = 70
	redisSlowAttention        = 10
	redisSlowAlert            = 100
	redisForkAttentionUsec    = 500000  // 0.5 秒
	redisForkAlertUsec        = 2000000 // 2 秒
	redisReplLagAttention     = 10      // 秒
	redisReplLagAlert         = 30
	redisReplIOAttention      = 10
	redisReplIOAlert          = 60
	redisReplGapAlertBytes    = 100 << 20
	redisBigKeyAttentionBytes = 1 << 20
	redisBigKeyAlertBytes     = 10 << 20
	redisBigKeyAttentionElems = 10000
	redisBigKeyAlertElems     = 100000
	redisUptimeAttentionSec   = 600
	redisVolatileEvictableMin = 10 // 用 volatile-* 淘汰策略时，带过期时间的 key 占比低于它（%）算关注
)

// 报告分区（顺序即展示顺序）。
var redisSections = [][2]string{
	{"basic", "基本信息"}, {"replication", "主从复制"}, {"memory", "内存"}, {"persistence", "持久化"},
	{"clients", "客户端连接"}, {"stats", "性能与命中率"}, {"security", "安全"}, {"keyspace", "键空间"},
}

// redisCheck 是一个检查项的结论。
type redisCheck struct {
	Section   string
	Key       string
	Name      string
	Value     string
	Threshold string
	Level     int
	Advice    string
	Table     *DetailTable
}

// 各检查项的建议文案（关注 / 告警 / 未采集时展示）。
var redisAdvice = map[string]string{
	"basic.version":     "Redis 5.0 以前已停止维护，缺少 ACL、Stream、MEMORY USAGE 等能力；规划升级到仍在维护的版本。",
	"basic.uptime":      "实例刚启动或正在加载数据：确认重启原因（OOM 被杀 / 人为重启），并检查持久化文件是否完整、主从是否已重新同步。",
	"replication.role":  "单节点没有高可用：重要数据建议配置从库并启用哨兵 / 集群，至少保证持久化开启。",
	"replication.link":  "从库与主库的复制链路异常：检查网络、主库 repl-backlog-size 与 client-output-buffer-limit replica，必要时在从库上重新同步。",
	"replication.lag":   "复制延迟偏大：检查主库写入量和从库负载，网络带宽，以及是否有大 key / 慢命令阻塞了从库回放。",
	"memory.usage":      "内存使用率偏高：评估扩容或设置 maxmemory 与合适的淘汰策略，清理无用 key / 过期数据，排查大 key。",
	"memory.maxmemory":  "未设置 maxmemory：数据增长会一直占用主机内存，直到被系统 OOM 杀掉；按主机内存的 60%~70% 设置，并预留 fork 和复制缓冲的空间。",
	"memory.policy":     "maxmemory 已设置但淘汰策略是 noeviction：内存满后所有写入都会报错；缓存场景改用 allkeys-lru / allkeys-lfu，数据库场景则要做好容量告警。",
	"memory.frag":       "碎片率过高：开启 activedefrag yes（Redis 4.0+），或在低峰期重启 / 主从切换释放；低于 1 说明发生了内存交换，必须扩内存。",
	"memory.peak":       "历史内存峰值已接近上限：说明曾出现过内存压力，评估扩容或调整淘汰策略。",
	"persistence.mode":  "没有开启任何持久化：实例重启后数据全部丢失；纯缓存可忽略，否则开启 AOF（appendonly yes）或配置 RDB（save）。",
	"persistence.rdb":   "RDB 保存失败或太久没有成功：检查磁盘空间 / 权限、fork 是否失败（内存超配 vm.overcommit_memory=1）和 dir 配置。",
	"persistence.aof":   "AOF 写入或重写失败：检查磁盘空间与 IO，必要时 redis-check-aof 修复；磁盘写满会让 Redis 拒绝写入。",
	"clients.usage":     "连接数接近 maxclients：检查业务连接池配置是否泄漏，必要时调大 maxclients（同时留意系统文件描述符限制）。",
	"clients.blocked":   "阻塞中的客户端较多（BLPOP / BRPOP / WAIT 等）：确认是否是预期的消费者；异常堆积说明生产端停了或消费逻辑卡住。",
	"clients.rejected":  "有连接被拒绝（达到 maxclients）：检查连接泄漏，调大 maxclients 或使用连接池 / 代理收敛连接数。",
	"stats.ops":         "仅供参考：当前每秒命令数。",
	"stats.hit":         "命中率偏低：检查缓存 key 的过期时间是否过短、是否被淘汰、是否有大量穿透查询，必要时增加内存或预热缓存。",
	"stats.evicted":     "已有 key 因内存不足被淘汰：评估扩容，或确认缓存策略是否符合预期；非缓存实例出现淘汰要尽快处理。",
	"stats.slowlog":     "存在慢命令：到“性能优化 › 慢 SQL 洞察”查看 Redis 慢查询，避免 KEYS / HGETALL / SMEMBERS 等 O(N) 命令作用于大 key，改用 SCAN / HSCAN 或拆分 key。",
	"stats.fork":        "fork 耗时过长会让 Redis 在 RDB / AOF 重写时卡顿：减小实例内存、避开业务高峰、关闭透明大页（THP）。",
	"security.password": "实例没有设置访问密码：任何能连到端口的人都可读写全部数据；设置 requirepass（或 ACL 用户），并限制网络访问范围。",
	"security.exposed":  "实例监听了所有网卡且关闭了 protected-mode：只绑定内网地址，保持 protected-mode yes，并用防火墙 / 安全组限制来源。",
	"security.commands": "FLUSHALL / FLUSHDB / KEYS / CONFIG / DEBUG / SHUTDOWN 等危险命令仍可直接使用：用 rename-command 改名或置空，或用 ACL 禁止业务账号使用。",
	"keyspace.summary":  "使用 volatile-* 淘汰策略时，没有过期时间的 key 不会被淘汰：给缓存 key 设置过期时间，或改用 allkeys-* 策略。",
	"keyspace.bigkey":   "存在大 key：拆分为多个小 key（按范围 / hash 分片），避免对大 key 做 O(N) 命令；删除大 key 用 UNLINK。",
}

// ---- 取值辅助 ----

func infoInt(m map[string]string, k string) (int64, bool) {
	v, ok := m[k]
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if ferr != nil {
			return 0, false
		}
		return int64(f), true
	}
	return n, true
}

func infoFloat(m map[string]string, k string) (float64, bool) {
	v, ok := m[k]
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	return f, err == nil
}

// redisBytes 把字节数格式化成易读的文本。
func redisBytes(b int64) string {
	const (
		kb = 1 << 10
		mb = 1 << 20
		gb = 1 << 30
		tb = 1 << 40
	)
	f := float64(b)
	switch {
	case b >= tb:
		return fmt.Sprintf("%.2f TB", f/tb)
	case b >= gb:
		return fmt.Sprintf("%.2f GB", f/gb)
	case b >= mb:
		return fmt.Sprintf("%.1f MB", f/mb)
	case b >= kb:
		return fmt.Sprintf("%.1f KB", f/kb)
	}
	return fmt.Sprintf("%d B", b)
}

func durText(sec int64) string {
	switch {
	case sec >= 86400:
		return fmt.Sprintf("%d 天 %d 小时", sec/86400, sec%86400/3600)
	case sec >= 3600:
		return fmt.Sprintf("%d 小时 %d 分钟", sec/3600, sec%3600/60)
	case sec >= 60:
		return fmt.Sprintf("%d 分钟", sec/60)
	}
	return fmt.Sprintf("%d 秒", sec)
}

// cfg 取 CONFIG GET 的值；CONFIG 不可用（被改名 / 禁止）时 ok=false。
func (s *redisSnapshot) cfg(key string) (string, bool) {
	if v, ok := s.Config[key]; ok {
		return v, true
	}
	return "", false
}

func (s *redisSnapshot) role() string {
	r := s.Info["role"]
	if r == "" {
		return "master"
	}
	return r
}

// maxmemory 返回 maxmemory（字节）；读不到返回 ok=false。
func (s *redisSnapshot) maxmemory() (int64, bool) {
	if n, ok := infoInt(s.Info, "maxmemory"); ok {
		return n, true
	}
	if v, ok := s.cfg("maxmemory"); ok {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n, err == nil
	}
	return 0, false
}

func (s *redisSnapshot) policy() string {
	if v := s.Info["maxmemory_policy"]; v != "" {
		return v
	}
	v, _ := s.cfg("maxmemory-policy")
	return v
}

// keyspace 解析 INFO 里的 dbN 行。
type ksDB struct {
	Name          string
	Keys, Expires int64
	AvgTTLms      int64
}

func (s *redisSnapshot) keyspace() []ksDB {
	var out []ksDB
	for k, v := range s.Info {
		if !strings.HasPrefix(k, "db") {
			continue
		}
		if _, err := strconv.Atoi(k[2:]); err != nil {
			continue
		}
		d := ksDB{Name: k}
		for _, kv := range strings.Split(v, ",") {
			a, b, _ := strings.Cut(kv, "=")
			n, _ := strconv.ParseInt(b, 10, 64)
			switch a {
			case "keys":
				d.Keys = n
			case "expires":
				d.Expires = n
			case "avg_ttl":
				d.AvgTTLms = n
			}
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(out[i].Name[2:])
		b, _ := strconv.Atoi(out[j].Name[2:])
		return a < b
	})
	return out
}

// replicas 解析 master 视角下的从库（slave0:ip=...,port=...,state=...,offset=...,lag=...）。
type replicaLine struct {
	Addr   string
	State  string
	Offset int64
	Lag    int64
}

func (s *redisSnapshot) replicas() []replicaLine {
	var out []replicaLine
	for k, v := range s.Info {
		if !strings.HasPrefix(k, "slave") {
			continue
		}
		if _, err := strconv.Atoi(k[5:]); err != nil {
			continue
		}
		r := replicaLine{}
		var ip, port string
		for _, kv := range strings.Split(v, ",") {
			a, b, _ := strings.Cut(kv, "=")
			switch a {
			case "ip":
				ip = b
			case "port":
				port = b
			case "state":
				r.State = b
			case "offset":
				r.Offset, _ = strconv.ParseInt(b, 10, 64)
			case "lag":
				r.Lag, _ = strconv.ParseInt(b, 10, 64)
			}
		}
		r.Addr = ip + ":" + port
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

// ---- 评估 ----

// evalRedis 按分区顺序生成全部检查项。
func evalRedis(s *redisSnapshot) []redisCheck {
	var out []redisCheck
	add := func(c redisCheck) {
		c.Advice = redisAdvice[c.Section+"."+c.Key]
		out = append(out, c)
	}
	for _, c := range redisBasic(s) {
		add(c)
	}
	for _, c := range redisReplication(s) {
		add(c)
	}
	for _, c := range redisMemory(s) {
		add(c)
	}
	for _, c := range redisPersistence(s) {
		add(c)
	}
	for _, c := range redisClients(s) {
		add(c)
	}
	for _, c := range redisStats(s) {
		add(c)
	}
	for _, c := range redisSecurity(s) {
		add(c)
	}
	for _, c := range redisKeyspace(s) {
		add(c)
	}
	return out
}

func redisBasic(s *redisSnapshot) []redisCheck {
	ver := s.Info["redis_version"]
	mode := s.Info["redis_mode"]
	if mode == "" {
		mode = "standalone"
	}
	v := fmt.Sprintf("Redis %s · %s", nonEmpty(ver, "未知版本"), mode)
	if os := s.Info["os"]; os != "" {
		v += " · " + truncate(os, 60)
	}
	level := LevelNormal
	if major := majorVersion(ver); major > 0 && major < 5 {
		level = LevelAttention
	}
	up, _ := infoInt(s.Info, "uptime_in_seconds")
	uv := durText(up)
	ulevel := LevelNormal
	if up > 0 && up < redisUptimeAttentionSec {
		ulevel = LevelAttention
		uv += "（刚启动）"
	}
	if s.Info["loading"] == "1" {
		ulevel = LevelAttention
		uv += "（正在加载数据）"
	}
	return []redisCheck{
		{Section: "basic", Key: "version", Name: "版本与连通", Value: v, Threshold: "主版本 ≥ 5", Level: level},
		{Section: "basic", Key: "uptime", Name: "运行时长", Value: uv, Threshold: fmt.Sprintf("≥ %d 分钟", redisUptimeAttentionSec/60), Level: ulevel},
	}
}

func majorVersion(v string) int {
	head, _, _ := strings.Cut(v, ".")
	n, _ := strconv.Atoi(head)
	return n
}

func redisReplication(s *redisSnapshot) []redisCheck {
	var out []redisCheck
	role := s.role()
	if role == "slave" {
		master := s.Info["master_host"] + ":" + s.Info["master_port"]
		role = fmt.Sprintf("slave（主库 %s）", master)
	}
	reps := s.replicas()
	roleVal := role
	if s.Info["role"] == "" || s.Info["role"] == "master" {
		n, _ := infoInt(s.Info, "connected_slaves")
		roleVal = fmt.Sprintf("master，从库 %d 个", n)
	}
	out = append(out, redisCheck{Section: "replication", Key: "role", Name: "角色", Value: roleVal, Threshold: "—", Level: LevelNormal})

	if s.Info["role"] == "slave" {
		status := s.Info["master_link_status"]
		linkVal := fmt.Sprintf("链路 %s", nonEmpty(status, "未知"))
		level := LevelNormal
		if status != "up" {
			level = LevelAlert
			if down, ok := infoInt(s.Info, "master_link_down_since_seconds"); ok && down >= 0 {
				linkVal += fmt.Sprintf("，已断开 %s", durText(down))
			}
		} else if s.Info["master_sync_in_progress"] == "1" {
			level = LevelAttention
			linkVal += "，正在全量同步"
		}
		out = append(out, redisCheck{Section: "replication", Key: "link", Name: "主从链路", Value: linkVal, Threshold: "master_link_status = up", Level: level})
		lagVal, lagLevel := "—", LevelNormal
		if io, ok := infoInt(s.Info, "master_last_io_seconds_ago"); ok {
			switch {
			case io < 0: // 链路断开时是 -1
				lagVal, lagLevel = "与主库无通信", LevelAlert
			default:
				lagVal = fmt.Sprintf("距上次与主库通信 %d 秒", io)
				switch {
				case io >= redisReplIOAlert:
					lagLevel = LevelAlert
				case io >= redisReplIOAttention:
					lagLevel = LevelAttention
				}
			}
		} else {
			lagLevel, lagVal = LevelFailed, "未采集（INFO 里没有 master_last_io_seconds_ago）"
		}
		out = append(out, redisCheck{Section: "replication", Key: "lag", Name: "复制延迟", Value: lagVal,
			Threshold: fmt.Sprintf("关注 ≥ %d 秒 / 告警 ≥ %d 秒", redisReplIOAttention, redisReplIOAlert), Level: lagLevel})
		return out
	}

	// master：逐个从库看状态和延迟
	linkLevel, lagLevel := LevelNormal, LevelNormal
	linkVal, lagVal := "无从库", "—"
	var rows [][]any
	if len(reps) > 0 {
		masterOff, _ := infoInt(s.Info, "master_repl_offset")
		var bad []string
		maxLag, maxGap := int64(0), int64(0)
		online := 0
		for _, r := range reps {
			gap := max(masterOff-r.Offset, 0)
			rows = append(rows, []any{r.Addr, r.State, r.Lag, gap})
			if r.State == "online" {
				online++
				maxLag = max(maxLag, r.Lag)
				maxGap = max(maxGap, gap)
				continue
			}
			bad = append(bad, r.Addr+"("+r.State+")")
			if r.State == "wait_bgsave" || r.State == "send_bulk" || r.State == "bg_transfer" {
				linkLevel = max(linkLevel, LevelAttention) // 正在同步
			} else {
				linkLevel = LevelAlert
			}
		}
		linkVal = fmt.Sprintf("%d / %d 个从库在线", online, len(reps))
		if len(bad) > 0 {
			linkVal += "；异常：" + strings.Join(bad, "、")
		}
		lagVal = fmt.Sprintf("最大延迟 %d 秒，最大偏移差 %s", maxLag, redisBytes(maxGap))
		switch {
		case maxLag >= redisReplLagAlert || maxGap >= redisReplGapAlertBytes:
			lagLevel = LevelAlert
		case maxLag >= redisReplLagAttention:
			lagLevel = LevelAttention
		}
	}
	link := redisCheck{Section: "replication", Key: "link", Name: "从库连接状态", Value: linkVal, Threshold: "全部从库 state = online", Level: linkLevel}
	lag := redisCheck{Section: "replication", Key: "lag", Name: "复制延迟", Value: lagVal,
		Threshold: fmt.Sprintf("关注 ≥ %d 秒 / 告警 ≥ %d 秒或偏移差 ≥ %s", redisReplLagAttention, redisReplLagAlert, redisBytes(redisReplGapAlertBytes)), Level: lagLevel}
	if len(rows) > 0 {
		link.Table = &DetailTable{Columns: []string{"从库", "状态", "延迟(秒)", "偏移差(字节)"}, Rows: rows}
	}
	return append(out, link, lag)
}

func redisMemory(s *redisSnapshot) []redisCheck {
	var out []redisCheck
	used, haveUsed := infoInt(s.Info, "used_memory")
	maxmem, haveMax := s.maxmemory()

	// 使用率：有 maxmemory 以它为准，否则以主机总内存为准
	usage := redisCheck{Section: "memory", Key: "usage", Name: "内存使用率",
		Threshold: fmt.Sprintf("关注 ≥ %d%% / 告警 ≥ %d%%", redisUsageAttentionPct, redisUsageAlertPct)}
	limit, basis := int64(0), ""
	switch {
	case haveMax && maxmem > 0:
		limit, basis = maxmem, "maxmemory"
	default:
		if sys, ok := infoInt(s.Info, "total_system_memory"); ok && sys > 0 {
			limit, basis = sys, "主机内存"
		}
	}
	switch {
	case !haveUsed:
		usage.Level, usage.Value = LevelFailed, "未采集（INFO 里没有 used_memory）"
	case limit <= 0:
		usage.Level, usage.Value = LevelFailed, fmt.Sprintf("已用 %s；无法确定上限（未设置 maxmemory 且读不到主机内存）", redisBytes(used))
	default:
		pct := float64(used) * 100 / float64(limit)
		usage.Value = fmt.Sprintf("%s / %s（%s）= %.1f%%", redisBytes(used), redisBytes(limit), basis, pct)
		switch {
		case pct >= redisUsageAlertPct:
			usage.Level = LevelAlert
		case pct >= redisUsageAttentionPct:
			usage.Level = LevelAttention
		}
	}
	out = append(out, usage)

	// maxmemory
	mm := redisCheck{Section: "memory", Key: "maxmemory", Name: "maxmemory", Threshold: "> 0"}
	switch {
	case !haveMax:
		mm.Level, mm.Value = LevelFailed, "未采集（INFO 与 CONFIG GET 都读不到）"
	case maxmem == 0:
		mm.Level, mm.Value = LevelAttention, "0（未设置上限）"
	default:
		mm.Value = redisBytes(maxmem)
	}
	out = append(out, mm)

	pol := s.policy()
	pc := redisCheck{Section: "memory", Key: "policy", Name: "淘汰策略", Threshold: "设置 maxmemory 时不应为 noeviction（缓存场景）"}
	switch {
	case pol == "":
		pc.Level, pc.Value = LevelFailed, "未采集"
	case pol == "noeviction" && haveMax && maxmem > 0:
		pc.Level, pc.Value = LevelAttention, fmt.Sprintf("%s（内存满后写入会报错）", pol)
	default:
		pc.Value = pol
	}
	out = append(out, pc)

	// 碎片率
	frag := redisCheck{Section: "memory", Key: "frag", Name: "内存碎片率", Threshold: fmt.Sprintf("1.0–%.1f 为宜；≥ %.1f 告警；< 1 说明发生了内存交换", redisFragAttention, redisFragAlert)}
	ratio, haveRatio := infoFloat(s.Info, "mem_fragmentation_ratio")
	rss, _ := infoInt(s.Info, "used_memory_rss")
	switch {
	case !haveRatio:
		frag.Level, frag.Value = LevelFailed, "未采集"
	case used < redisFragMinBytes:
		frag.Value = fmt.Sprintf("%.2f（数据量 %s 太小，不评估）", ratio, redisBytes(used))
	default:
		frag.Value = fmt.Sprintf("%.2f（RSS %s）", ratio, redisBytes(rss))
		switch {
		case ratio < redisFragSwap:
			frag.Level = LevelAlert
			frag.Value += "，可能已使用交换分区"
		case ratio >= redisFragAlert:
			frag.Level = LevelAlert
		case ratio >= redisFragAttention:
			frag.Level = LevelAttention
		}
	}
	out = append(out, frag)

	// 峰值
	peak, havePeak := infoInt(s.Info, "used_memory_peak")
	pk := redisCheck{Section: "memory", Key: "peak", Name: "历史内存峰值", Threshold: fmt.Sprintf("占 maxmemory < %d%%", redisPeakAttentionPct)}
	switch {
	case !havePeak:
		pk.Level, pk.Value = LevelFailed, "未采集"
	case haveMax && maxmem > 0:
		pct := float64(peak) * 100 / float64(maxmem)
		pk.Value = fmt.Sprintf("%s（占 maxmemory %.1f%%）", redisBytes(peak), pct)
		if pct >= redisPeakAttentionPct {
			pk.Level = LevelAttention
		}
	default:
		pk.Value = redisBytes(peak)
	}
	return append(out, pk)
}

func redisPersistence(s *redisSnapshot) []redisCheck {
	var out []redisCheck
	// RDB 是否开启：save 配置非空；读不到 CONFIG 时，用“是否保存过”推断
	rdbOn, rdbKnown := false, false
	saveCfg := ""
	if v, ok := s.cfg("save"); ok {
		saveCfg, rdbOn, rdbKnown = strings.TrimSpace(v), strings.TrimSpace(v) != "", true
	}
	aofOn := s.Info["aof_enabled"] == "1"
	if v, ok := s.cfg("appendonly"); ok && !aofOn {
		aofOn = v == "yes"
	}
	if !rdbKnown {
		_, rdbOn = infoInt(s.Info, "rdb_last_save_time")
	}
	mode := redisCheck{Section: "persistence", Key: "mode", Name: "持久化方式", Threshold: "RDB 或 AOF 至少开启一种"}
	mode.Value = fmt.Sprintf("RDB：%s；AOF：%s", onOff(rdbOn, saveCfg), onOff(aofOn, ""))
	if !rdbOn && !aofOn {
		mode.Level = LevelAttention
	}
	out = append(out, mode)

	rdb := redisCheck{Section: "persistence", Key: "rdb", Name: "RDB 最近一次保存", Threshold: fmt.Sprintf("bgsave 状态 ok，且有变更时 %s 内保存过", durText(int64(redisRDBAgeAttention/time.Second)))}
	status := s.Info["rdb_last_bgsave_status"]
	lastSave, haveSave := infoInt(s.Info, "rdb_last_save_time")
	changes, _ := infoInt(s.Info, "rdb_changes_since_last_save")
	switch {
	case status == "":
		rdb.Level, rdb.Value = LevelFailed, "未采集（INFO 里没有 rdb_last_bgsave_status）"
	case !rdbOn && rdbKnown:
		rdb.Value = "RDB 未开启"
	default:
		rdb.Value = fmt.Sprintf("bgsave 状态 %s", status)
		var age time.Duration
		if haveSave && lastSave > 0 {
			age = s.Now.Sub(time.Unix(lastSave, 0))
			rdb.Value += fmt.Sprintf("，最近一次保存在 %s 前（%s）", durText(int64(age/time.Second)), time.Unix(lastSave, 0).Format("2006-01-02 15:04:05"))
		}
		switch {
		case status != "ok":
			rdb.Level = LevelAlert
		case rdbOn && changes > 0 && age > redisRDBAgeAlert:
			rdb.Level = LevelAlert
			rdb.Value += fmt.Sprintf("，此后有 %d 次变更未保存", changes)
		case rdbOn && changes > 0 && age > redisRDBAgeAttention:
			rdb.Level = LevelAttention
			rdb.Value += fmt.Sprintf("，此后有 %d 次变更未保存", changes)
		}
	}
	out = append(out, rdb)

	aof := redisCheck{Section: "persistence", Key: "aof", Name: "AOF 状态", Threshold: "最近一次写入和重写都是 ok"}
	if !aofOn {
		aof.Value = "AOF 未开启"
	} else {
		w, r := s.Info["aof_last_write_status"], s.Info["aof_last_bgrewrite_status"]
		aof.Value = fmt.Sprintf("最近写入 %s，最近重写 %s", nonEmpty(w, "未知"), nonEmpty(r, "未知"))
		if sz, ok := infoInt(s.Info, "aof_current_size"); ok {
			aof.Value += "，文件 " + redisBytes(sz)
		}
		if (w != "" && w != "ok") || (r != "" && r != "ok") {
			aof.Level = LevelAlert
		}
	}
	return append(out, aof)
}

func onOff(on bool, detail string) string {
	if !on {
		return "关"
	}
	if detail != "" {
		return fmt.Sprintf("开（%s）", truncate(detail, 60))
	}
	return "开"
}

func redisClients(s *redisSnapshot) []redisCheck {
	var out []redisCheck
	conn, haveConn := infoInt(s.Info, "connected_clients")
	maxc, haveMax := infoInt(s.Info, "maxclients") // Redis 7.0+ 的 INFO clients 里有
	if !haveMax {
		if v, ok := s.cfg("maxclients"); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			maxc, haveMax = n, err == nil
		}
	}
	use := redisCheck{Section: "clients", Key: "usage", Name: "连接数", Threshold: fmt.Sprintf("占 maxclients：关注 ≥ %d%% / 告警 ≥ %d%%", redisClientsAttentionPct, redisClientsAlertPct)}
	switch {
	case !haveConn:
		use.Level, use.Value = LevelFailed, "未采集"
	case !haveMax || maxc <= 0:
		use.Value = fmt.Sprintf("%d（maxclients 未知）", conn)
	default:
		pct := float64(conn) * 100 / float64(maxc)
		use.Value = fmt.Sprintf("%d / %d = %.1f%%", conn, maxc, pct)
		switch {
		case pct >= redisClientsAlertPct:
			use.Level = LevelAlert
		case pct >= redisClientsAttentionPct:
			use.Level = LevelAttention
		}
	}
	out = append(out, use)

	blocked, haveBlocked := infoInt(s.Info, "blocked_clients")
	bl := redisCheck{Section: "clients", Key: "blocked", Name: "阻塞中的客户端", Threshold: fmt.Sprintf("关注 ≥ %d / 告警 ≥ %d", redisBlockedAttention, redisBlockedAlert)}
	switch {
	case !haveBlocked:
		bl.Level, bl.Value = LevelFailed, "未采集"
	default:
		bl.Value = strconv.FormatInt(blocked, 10)
		switch {
		case blocked >= redisBlockedAlert:
			bl.Level = LevelAlert
		case blocked >= redisBlockedAttention:
			bl.Level = LevelAttention
		}
	}
	out = append(out, bl)

	rej, haveRej := infoInt(s.Info, "rejected_connections")
	rc := redisCheck{Section: "clients", Key: "rejected", Name: "被拒绝的连接数", Threshold: fmt.Sprintf("自启动累计：> 0 关注 / ≥ %d 告警", redisRejectedAlert)}
	switch {
	case !haveRej:
		rc.Level, rc.Value = LevelFailed, "未采集"
	default:
		total, _ := infoInt(s.Info, "total_connections_received")
		rc.Value = fmt.Sprintf("%d（累计接入 %d）", rej, total)
		switch {
		case rej >= redisRejectedAlert:
			rc.Level = LevelAlert
		case rej > 0:
			rc.Level = LevelAttention
		}
	}
	return append(out, rc)
}

func redisStats(s *redisSnapshot) []redisCheck {
	var out []redisCheck
	if ops, ok := infoInt(s.Info, "instantaneous_ops_per_sec"); ok {
		out = append(out, redisCheck{Section: "stats", Key: "ops", Name: "每秒命令数", Value: strconv.FormatInt(ops, 10), Threshold: "—", Level: LevelNormal})
	}
	hits, h1 := infoInt(s.Info, "keyspace_hits")
	miss, h2 := infoInt(s.Info, "keyspace_misses")
	hit := redisCheck{Section: "stats", Key: "hit", Name: "keyspace 命中率", Threshold: fmt.Sprintf("请求 ≥ %d 次时：关注 < %d%% / 告警 < %d%%", redisHitMinRequests, redisHitAttentionPct, redisHitAlertPct)}
	switch {
	case !h1 || !h2:
		hit.Level, hit.Value = LevelFailed, "未采集"
	case hits+miss < redisHitMinRequests:
		hit.Value = fmt.Sprintf("请求数 %d 太少，不评估", hits+miss)
	default:
		pct := float64(hits) * 100 / float64(hits+miss)
		hit.Value = fmt.Sprintf("%.2f%%（命中 %d / 未命中 %d）", pct, hits, miss)
		switch {
		case pct < redisHitAlertPct:
			hit.Level = LevelAlert
		case pct < redisHitAttentionPct:
			hit.Level = LevelAttention
		}
	}
	out = append(out, hit)

	ev := redisCheck{Section: "stats", Key: "evicted", Name: "被淘汰的 key", Threshold: fmt.Sprintf("自启动累计：> 0 关注 / ≥ %d 告警", redisEvictedAlert)}
	evicted, haveEv := infoInt(s.Info, "evicted_keys")
	expired, _ := infoInt(s.Info, "expired_keys")
	switch {
	case !haveEv:
		ev.Level, ev.Value = LevelFailed, "未采集"
	default:
		ev.Value = fmt.Sprintf("%d（已过期删除 %d）", evicted, expired)
		switch {
		case evicted >= redisEvictedAlert:
			ev.Level = LevelAlert
		case evicted > 0:
			ev.Level = LevelAttention
		}
	}
	out = append(out, ev)

	sl := redisCheck{Section: "stats", Key: "slowlog", Name: "慢日志条数（SLOWLOG LEN）", Threshold: fmt.Sprintf("关注 ≥ %d / 告警 ≥ %d", redisSlowAttention, redisSlowAlert)}
	if s.SlowErr != "" {
		sl.Level, sl.Value = LevelFailed, fmt.Sprintf("未采集：%s", s.SlowErr)
	} else {
		sl.Value = strconv.FormatInt(s.SlowLen, 10)
		if len(s.SlowTop) > 0 {
			top := s.SlowTop[0]
			for _, e := range s.SlowTop {
				if e.DurationMicros > top.DurationMicros {
					top = e
				}
			}
			sl.Value += fmt.Sprintf("，最慢 %.1f ms（%s）", float64(top.DurationMicros)/1000, redis.CommandName(top.Args))
			var rows [][]any
			for _, e := range s.SlowTop[:min(len(s.SlowTop), 5)] {
				rows = append(rows, []any{e.Time.Format("2006-01-02 15:04:05"), fmt.Sprintf("%.1f", float64(e.DurationMicros)/1000), redis.Fingerprint(e.Args), e.Client})
			}
			sl.Table = &DetailTable{Columns: []string{"时间", "耗时(ms)", "命令（指纹）", "客户端"}, Rows: rows}
		}
		switch {
		case s.SlowLen >= redisSlowAlert:
			sl.Level = LevelAlert
		case s.SlowLen >= redisSlowAttention:
			sl.Level = LevelAttention
		}
	}
	out = append(out, sl)

	fk := redisCheck{Section: "stats", Key: "fork", Name: "最近一次 fork 耗时", Threshold: fmt.Sprintf("关注 ≥ %d ms / 告警 ≥ %d ms", redisForkAttentionUsec/1000, redisForkAlertUsec/1000)}
	fork, haveFork := infoInt(s.Info, "latest_fork_usec")
	switch {
	case !haveFork:
		fk.Level, fk.Value = LevelFailed, "未采集"
	default:
		fk.Value = fmt.Sprintf("%.1f ms", float64(fork)/1000)
		switch {
		case fork >= redisForkAlertUsec:
			fk.Level = LevelAlert
		case fork >= redisForkAttentionUsec:
			fk.Level = LevelAttention
		}
	}
	return append(out, fk)
}

func redisSecurity(s *redisSnapshot) []redisCheck {
	var out []redisCheck
	pw := redisCheck{Section: "security", Key: "password", Name: "访问密码", Threshold: "必须设置 requirepass 或 ACL"}
	switch rp, ok := s.cfg("requirepass"); {
	case ok && rp != "":
		pw.Value = "已设置"
	case ok:
		// requirepass 为空：Redis 6+ 可能用 ACL 管理 default 用户，此时平台是带密码连上的就说明有认证
		if s.HasPassword {
			pw.Value = "requirepass 为空，但平台使用密码 / ACL 账号连接成功"
		} else {
			pw.Level, pw.Value = LevelAlert, "未设置（无密码即可访问）"
		}
	case s.HasPassword:
		pw.Value = "已配置连接密码（CONFIG GET 不可用，无法核对 requirepass）"
	default:
		pw.Level, pw.Value = LevelAlert, "平台未使用密码连接，且无法读取 requirepass：实例很可能没有设置密码"
	}
	out = append(out, pw)

	ex := redisCheck{Section: "security", Key: "exposed", Name: "protected-mode 与绑定地址", Threshold: "监听所有网卡时应开启 protected-mode"}
	pm, havePM := s.cfg("protected-mode")
	bind, haveBind := s.cfg("bind")
	switch {
	case s.ConfigErr != "" || (!havePM && !haveBind):
		ex.Level, ex.Value = LevelFailed, "未采集（CONFIG GET 不可用）"
	default:
		all := !haveBind || strings.TrimSpace(bind) == "" || strings.Contains(bind, "0.0.0.0") || strings.Contains(bind, "*") || strings.Contains(bind, "::")
		ex.Value = fmt.Sprintf("protected-mode %s；bind %s", nonEmpty(pm, "未知"), nonEmpty(strings.TrimSpace(bind), "(空)"))
		if all && pm == "no" {
			ex.Level = LevelAttention
			if pw.Level == LevelAlert {
				ex.Level = LevelAlert
			}
		}
	}
	out = append(out, ex)

	cm := redisCheck{Section: "security", Key: "commands", Name: "危险命令是否被改名", Threshold: "FLUSHALL / FLUSHDB / KEYS / CONFIG / DEBUG / SHUTDOWN 已改名或禁用"}
	switch {
	case s.CmdErr != "":
		cm.Level, cm.Value = LevelFailed, fmt.Sprintf("未采集：%s", s.CmdErr)
	default:
		var open []string
		for _, c := range redisDangerous {
			if s.CmdAvail[strings.ToLower(c)] {
				open = append(open, c)
			}
		}
		if len(open) == 0 {
			cm.Value = "全部已改名或禁用"
		} else {
			cm.Level = LevelAttention
			cm.Value = fmt.Sprintf("仍可直接使用：%s", strings.Join(open, "、"))
		}
	}
	return append(out, cm)
}

func redisKeyspace(s *redisSnapshot) []redisCheck {
	var out []redisCheck
	dbs := s.keyspace()
	sum := redisCheck{Section: "keyspace", Key: "summary", Name: "键数量与过期键占比", Threshold: fmt.Sprintf("volatile-* 策略下带过期时间的 key 占比 ≥ %d%%", redisVolatileEvictableMin)}
	var keys, exp int64
	var rows [][]any
	for _, d := range dbs {
		keys += d.Keys
		exp += d.Expires
		pct := 0.0
		if d.Keys > 0 {
			pct = float64(d.Expires) * 100 / float64(d.Keys)
		}
		rows = append(rows, []any{d.Name, d.Keys, d.Expires, fmt.Sprintf("%.1f%%", pct), d.AvgTTLms / 1000})
	}
	if len(rows) > 0 {
		sum.Table = &DetailTable{Columns: []string{"库", "key 数", "带过期时间", "占比", "平均 TTL(秒)"}, Rows: rows}
	}
	if keys == 0 {
		sum.Value = "没有 key"
	} else {
		pct := float64(exp) * 100 / float64(keys)
		sum.Value = fmt.Sprintf("%d 个 key，其中 %d 个带过期时间（%.1f%%）", keys, exp, pct)
		if pol := s.policy(); strings.HasPrefix(pol, "volatile-") {
			switch {
			case exp == 0:
				sum.Level = LevelAlert
				sum.Value += fmt.Sprintf("；淘汰策略 %s 只淘汰带过期时间的 key，当前没有可淘汰的 key", pol)
			case pct < redisVolatileEvictableMin:
				sum.Level = LevelAttention
				sum.Value += fmt.Sprintf("；淘汰策略 %s 下可淘汰的 key 太少", pol)
			}
		}
	}
	out = append(out, sum)

	bk := redisCheck{Section: "keyspace", Key: "bigkey", Name: "大 key 抽样", Threshold: fmt.Sprintf("单 key ≥ %s 关注 / ≥ %s 告警", redisBytes(redisBigKeyAttentionBytes), redisBytes(redisBigKeyAlertBytes))}
	switch {
	case s.BigKeyN <= 0:
		bk.Value = "未开启（默认关闭；立即巡检时可勾选，或设置环境变量 REDIS_INSPECT_BIGKEY_SAMPLE）"
	case s.BigKeyErr != "" && (s.BigKeys == nil || s.BigKeys.Scanned == 0):
		bk.Level, bk.Value = LevelFailed, fmt.Sprintf("未采集：%s", s.BigKeyErr)
	default:
		res := s.BigKeys
		bk.Value = fmt.Sprintf("抽样 %d 个 key（上限 %d），", res.Scanned, s.BigKeyN)
		if !res.Exact {
			bk.Threshold = fmt.Sprintf("Redis 不支持 MEMORY USAGE：按元素个数判断，≥ %d 关注 / ≥ %d 告警", redisBigKeyAttentionElems, redisBigKeyAlertElems)
		}
		if len(res.Top) == 0 {
			bk.Value += "没有发现 key"
		} else {
			top := res.Top[0]
			if res.Exact {
				bk.Value += fmt.Sprintf("最大 %s（%s）", redisBytes(top.Bytes), top.Type)
				switch {
				case top.Bytes >= redisBigKeyAlertBytes:
					bk.Level = LevelAlert
				case top.Bytes >= redisBigKeyAttentionBytes:
					bk.Level = LevelAttention
				}
			} else {
				bk.Value += fmt.Sprintf("最大 %d 个元素（%s）", top.Elems, top.Type)
				switch {
				case top.Elems >= redisBigKeyAlertElems:
					bk.Level = LevelAlert
				case top.Elems >= redisBigKeyAttentionElems:
					bk.Level = LevelAttention
				}
			}
			var rows [][]any
			for _, k := range res.Top {
				size := any(redisBytes(k.Bytes))
				if !res.Exact {
					size = fmt.Sprintf("%d 个元素", k.Elems)
				}
				rows = append(rows, []any{k.DB, truncate(k.Key, 120), k.Type, size})
			}
			bk.Table = &DetailTable{Columns: []string{"库", "key", "类型", "大小"}, Rows: rows}
		}
		if s.BigKeyErr != "" {
			bk.Value += "；抽样中途出错：" + s.BigKeyErr
		}
	}
	return append(out, bk)
}
