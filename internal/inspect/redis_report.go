// @author: kevinkelly
// @license: Apache Licence
// @file: redis_report.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// kindOf 返回实例类型对应的报告种类。比包级 KindOf 多认 Redis：只有配置了 Redis 打开函数（生产环境）才算支持，
// 单测里没配置时 Redis 仍按“暂不支持”处理，与引入 Redis 前一致。
func (s *Service) kindOf(dbType string) (string, bool) {
	if k, ok := KindOf(dbType); ok {
		return k, true
	}
	if s.openRedisFn != nil && strings.EqualFold(strings.TrimSpace(dbType), "redis") {
		return KindRedis, true
	}
	return "", false
}

func (s *Service) openRedis(ctx context.Context, c engine.Conn) (RedisTarget, error) {
	if s.openRedisFn == nil {
		return nil, errors.New("未启用 Redis 巡检")
	}
	return s.openRedisFn(ctx, c)
}

// runRedis 采集并评估一个 Redis 实例，结果与 MySQL 报告同形：result JSON + 评分。
func (s *Service) runRedis(ctx context.Context, t RedisTarget, inst model.Instance, ref ReportRef, opt RunOptions, conn engine.Conn) (Finish, error) {
	if err := s.repo.UpdateProgress(ref.Kind, ref.ID, 0, redisSteps); err != nil {
		return Finish{}, err
	}
	bigKeys := opt.RedisBigKeys
	if bigKeys <= 0 {
		bigKeys = s.redisBigKeyN
	}
	progress := func(done int) bool {
		if err := s.repo.UpdateProgress(ref.Kind, ref.ID, done, redisSteps); err != nil {
			slog.Warn("更新巡检进度失败", "report", ref.String(), "err", err)
		}
		return !s.interrupted(ctx, ref)
	}
	snap, err := collectRedis(ctx, t, conn, s.now(), bigKeys, progress)
	switch {
	case errors.Is(err, errRedisStopped):
		return Finish{}, s.stopError(ctx)
	case err != nil:
		return Finish{}, err
	}
	checks := evalRedis(snap)
	result := buildRedisResult(snap, checks, inst.ID, s.now())
	norm, err := normalizeJSON(result)
	if err != nil {
		return Finish{}, fmt.Errorf("巡检结果无法序列化：%v", err)
	}
	health := RedisHealth(norm)
	norm["inspection_report"] = health
	norm, err = normalizeJSON(norm)
	if err != nil {
		return Finish{}, fmt.Errorf("巡检结果无法序列化：%v", err)
	}
	secs := BuildRedisSections(norm)
	alert, attention, failed := countSections(secs)
	raw, _ := json.Marshal(norm)
	return Finish{Status: StatusFinish, HealthScore: int(i64(health, "health_score")), Alert: alert, Attention: attention, Failed: failed,
		Total: redisSteps, Done: redisSteps, Result: string(raw), At: s.now()}, nil
}

// buildRedisResult 生成报告的 result JSON（评分在其后补上）。
func buildRedisResult(snap *redisSnapshot, checks []redisCheck, instID uint, now time.Time) jmap {
	info := snap.Info
	var keys int64
	for _, d := range snap.keyspace() {
		keys += d.Keys
	}
	mode := info["redis_mode"]
	if mode == "" {
		mode = "standalone"
	}
	up, _ := infoInt(info, "uptime_in_seconds")
	used, _ := infoInt(info, "used_memory")
	maxmem, _ := snap.maxmemory()
	clients, _ := infoInt(info, "connected_clients")
	ops, _ := infoInt(info, "instantaneous_ops_per_sec")
	port, _ := infoInt(info, "tcp_port")
	list := make([]any, 0, len(checks))
	for _, c := range checks {
		m := map[string]any{"section": c.Section, "key": c.Key, "name": c.Name, "value": c.Value, "threshold": c.Threshold, "level": c.Level, "advice": c.Advice}
		if c.Table != nil {
			m["table"] = c.Table
		}
		list = append(list, m)
	}
	return jmap{
		"inspection_time": now.Format("2006-01-02T15:04:05"), "instance_id": instID, "engine": "redis",
		"server": jmap{"version": info["redis_version"], "mode": mode, "role": snap.role(), "uptime_seconds": up, "os": info["os"], "tcp_port": port,
			"used_memory": used, "maxmemory": maxmem, "connected_clients": clients, "keys": keys, "ops": ops, "bigkey_sample": snap.BigKeyN},
		"checks": list,
	}
}

// redisPenalty 是各检查项的扣分：{关注, 告警}（键为“分区.键”）。告警且扣分 ≥ 20 的算严重（status = critical）。
var redisPenalty = map[string][2]int{
	"basic.version": {2, 0}, "basic.uptime": {2, 0},
	"replication.link": {5, 20}, "replication.lag": {3, 10},
	"memory.usage": {10, 25}, "memory.maxmemory": {3, 0}, "memory.policy": {2, 0}, "memory.frag": {3, 10}, "memory.peak": {3, 0},
	"persistence.mode": {5, 0}, "persistence.rdb": {4, 20}, "persistence.aof": {0, 20},
	"clients.usage": {8, 20}, "clients.blocked": {3, 8}, "clients.rejected": {5, 10},
	"stats.hit": {4, 10}, "stats.evicted": {5, 10}, "stats.slowlog": {5, 10}, "stats.fork": {3, 8},
	"security.password": {0, 20}, "security.exposed": {5, 15}, "security.commands": {3, 0},
	"keyspace.summary": {3, 8}, "keyspace.bigkey": {3, 10},
}

// RedisHealth 按检查项结论扣分，生成评分、问题、警告和建议（形式与 MySQLHealth 一致：100 分起，不低于 0）。
// 未采集的检查项不扣分。
func RedisHealth(r jmap) jmap {
	score := 100
	status := "healthy"
	issues, warnings, suggestions := []any{}, []any{}, []any{}
	seen := map[string]bool{}
	for _, c := range jrows(r, "checks") {
		key := sv(c, "section") + "." + sv(c, "key")
		pen := redisPenalty[key]
		line := fmt.Sprintf("[%s] %s：%s", redisSectionName(sv(c, "section")), sv(c, "name"), sv(c, "value"))
		switch int(i64(c, "level")) {
		case LevelAlert:
			issues = append(issues, line)
			score -= pen[1]
			if pen[1] >= 20 {
				status = "critical"
			}
		case LevelAttention:
			warnings = append(warnings, line)
			score -= pen[0]
		default:
			continue
		}
		if adv := sv(c, "advice"); adv != "" && !seen[key] && len(suggestions) < 8 {
			seen[key] = true
			suggestions = append(suggestions, sv(c, "name")+"："+adv)
		}
	}
	if sv(sub(r, "server"), "bigkey_sample") == "0" {
		suggestions = append(suggestions, "未做大 key 抽样：低峰期可在“立即巡检”里勾选“大 key 抽样”（限量，默认 1000 个 key）。")
	}
	if score < 0 {
		score = 0
	}
	return jmap{"health_score": score, "issues": issues, "warnings": warnings, "suggestions": suggestions, "status": status}
}

func redisSectionName(code string) string {
	for _, s := range redisSections {
		if s[0] == code {
			return s[1]
		}
	}
	return code
}

// BuildRedisSections 把 Redis 报告的 result JSON 转成分区展示。
func BuildRedisSections(result jmap) []Section {
	grouped := map[string]*Section{}
	var order []string
	for _, s := range redisSections {
		grouped[s[0]] = &Section{Code: s[0], Name: s[1]}
		order = append(order, s[0])
	}
	for _, c := range jrows(result, "checks") {
		code := sv(c, "section")
		sec := grouped[code]
		if sec == nil {
			sec = &Section{Code: code, Name: code}
			grouped[code] = sec
			order = append(order, code)
		}
		item := Item{Key: sv(c, "key"), Name: sv(c, "name"), Value: sv(c, "value"), Threshold: sv(c, "threshold"), Level: int(i64(c, "level")), Advice: sv(c, "advice")}
		if level := item.Level; level == LevelFailed && item.Advice == "" {
			item.Advice = adviceFailed
		}
		if t := sub(c, "table"); len(t) > 0 {
			var dt DetailTable
			if b, err := json.Marshal(t); err == nil && json.Unmarshal(b, &dt) == nil && len(dt.Columns) > 0 {
				item.Table = &dt
			}
		}
		sec.Items = append(sec.Items, item)
	}
	var out []Section
	for _, code := range order {
		if sec := grouped[code]; len(sec.Items) > 0 {
			out = append(out, *sec)
		}
	}
	return finalize(out)
}

func (s *Service) viewRedis(v *ReportView, d *ReportDetail) {
	if d.Result == "" {
		if d.Status == StatusFailed || d.Status == StatusCancelled {
			v.Blocks = append(v.Blocks, Block{Kind: "err", Title: "巡检没有产出结果", Items: []string{nonEmpty(d.Error, "未知原因")}})
		}
		return
	}
	var result jmap
	if err := json.Unmarshal([]byte(d.Result), &result); err != nil {
		v.Blocks = append(v.Blocks, Block{Kind: "err", Title: "报告内容无法解析", Items: []string{err.Error()}})
		return
	}
	server := sub(result, "server")
	v.Facts = append(v.Facts,
		Fact{"版本", fmt.Sprintf("Redis %s · %s", nonEmpty(sv(server, "version"), "-"), nonEmpty(sv(server, "mode"), "standalone"))},
		Fact{"角色", nonEmpty(sv(server, "role"), "-")},
		Fact{"运行时长", uptimeText(i64(server, "uptime_seconds"))},
		Fact{"内存", redisMemoryFact(i64(server, "used_memory"), i64(server, "maxmemory"))},
		Fact{"key 数 / 连接数", fmt.Sprintf("%s / %d", fmtNum(i64(server, "keys")), i64(server, "connected_clients"))},
		Fact{"每秒命令数", fmtNum(i64(server, "ops"))},
	)
	rep := sub(result, "inspection_report")
	for _, b := range []struct{ kind, title, key string }{{"err", "需处理的问题", "issues"}, {"warn", "需要关注", "warnings"}, {"info", "优化建议", "suggestions"}} {
		if list := strs(rep, b.key); len(list) > 0 {
			v.Blocks = append(v.Blocks, Block{Kind: b.kind, Title: b.title, Items: list})
		}
	}
	if len(rep) == 0 {
		v.Note = "报告里没有评分信息。"
	}
	v.Sections = BuildRedisSections(result)
	v.Report.HealthScore = coalesceScore(v.Report.HealthScore, rep)
}

func redisMemoryFact(used, max int64) string {
	if max > 0 {
		return fmt.Sprintf("%s / maxmemory %s", redisBytes(used), redisBytes(max))
	}
	return fmt.Sprintf("%s（未设置 maxmemory）", redisBytes(used))
}
