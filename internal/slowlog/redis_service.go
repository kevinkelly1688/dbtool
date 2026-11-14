// @author: kevinkelly
// @license: Apache Licence
// @file: redis_service.go
// @time: 2026/10/28

package slowlog

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// RedisSlow 是 Redis 慢查询的依赖。Options.Redis 为空表示未启用（单测默认）：Redis 实例在慢查日志页仍显示“暂不支持”。
type RedisSlow struct {
	Repo      RedisRepo
	Open      RedisOpener
	Conn      func(*model.Instance) engine.Conn
	Collector *RedisCollector
}

// redisModule 是 Service 里 Redis 部分：依赖加上“立即采集”的限频状态。
type redisModule struct {
	RedisSlow
	mu         sync.Mutex
	lastManual map[uint]time.Time
}

func newRedisModule(r *RedisSlow) *redisModule {
	if r == nil {
		return nil
	}
	return &redisModule{RedisSlow: *r, lastManual: map[uint]time.Time{}}
}

func (s *Service) redisOn() bool { return s.redis != nil }

// RedisParams 是统计 / 明细 / 趋势 / 命令列表共用的请求参数（原始字符串，由服务校验）。
type RedisParams struct {
	InstanceID uint
	Command    string
	FpID       string
	Search     string
	Start, End string
	MinSeconds string // 单次耗时下限（秒，可带小数）
	Sort       string
	Order      string
	Bucket     string
	Limit      int
	Offset     int
}

var (
	reRedisCmd = regexp.MustCompile(`^[A-Za-z][A-Za-z.\- ]{0,62}[A-Za-z]$|^[A-Za-z]$`)

	redisReviewSorts  = map[string]bool{"exec_count": true, "total_ms": true, "avg_ms": true, "max_ms": true, "first_seen": true, "last_seen": true, "command_name": true}
	redisHistorySorts = map[string]bool{"start_time": true, "duration_ms": true, "command_name": true}
)

var errRedisTables = newErr(http.StatusConflict, "table_missing", "Redis 慢查询的表还没有创建，请先在数据库里执行 deploy/sql/130_redis_slowlog.sql")

// redisInstance 取实例并确认：在用户可见范围内、是 Redis、表已创建。
func (s *Service) redisInstance(p Principal, id uint) (*model.Instance, error) {
	if !s.redisOn() {
		return nil, newErr(http.StatusBadRequest, "unsupported", "Redis 慢查询未启用")
	}
	if err := s.requirePerm(p, viewButtons...); err != nil {
		return nil, err
	}
	inst, err := s.instanceFor(p, id)
	if err != nil {
		return nil, err
	}
	if inst.DBType != "redis" {
		return nil, badReq("该实例不是 Redis 实例")
	}
	if !s.redis.Repo.RedisTablesReady() {
		return nil, errRedisTables
	}
	return inst, nil
}

func (s *Service) redisQuery(in RedisParams, defDays int) (RedisQuery, string, string, error) {
	from, to, startDay, endDay, err := s.parseRange(in.Start, in.End, defDays)
	if err != nil {
		return RedisQuery{}, "", "", err
	}
	search, err := validSearch(in.Search)
	if err != nil {
		return RedisQuery{}, "", "", err
	}
	q := RedisQuery{InstanceID: in.InstanceID, From: from, To: to, Search: search}
	if cmd := strings.TrimSpace(in.Command); cmd != "" {
		if !reRedisCmd.MatchString(cmd) {
			return RedisQuery{}, "", "", badReq("命令名不合法")
		}
		q.Command = strings.ToUpper(strings.Join(strings.Fields(cmd), " "))
	}
	if id := strings.TrimSpace(in.FpID); id != "" {
		if !reSQLID.MatchString(id) {
			return RedisQuery{}, "", "", badReq("命令指纹不合法")
		}
		q.FpID = id
	}
	if m := strings.TrimSpace(in.MinSeconds); m != "" {
		f, err := strconv.ParseFloat(m, 64)
		if err != nil || f < 0 || f > 3600 {
			return RedisQuery{}, "", "", badReq("耗时下限需是 0 到 3600 秒之间的数字")
		}
		q.MinMicros = int64(f * 1e6)
	}
	return q, startDay, endDay, nil
}

// RedisReview 返回 Redis 慢查询统计：按命令指纹聚合。
func (s *Service) RedisReview(p Principal, in RedisParams) (*RedisReviewPage, error) {
	if _, err := s.redisInstance(p, in.InstanceID); err != nil {
		return nil, err
	}
	q, startDay, endDay, err := s.redisQuery(in, 7)
	if err != nil {
		return nil, err
	}
	if q.Sort, err = validSort(in.Sort, "exec_count", redisReviewSorts); err != nil {
		return nil, err
	}
	if q.Desc, err = validOrder(in.Order); err != nil {
		return nil, err
	}
	if q.Limit, q.Offset, err = validPaging(in.Limit, in.Offset); err != nil {
		return nil, err
	}
	rows, total, sum, err := s.redis.Repo.RedisReview(q)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].SampleText, _ = clipText(rows[i].SampleText)
	}
	if rows == nil {
		rows = []RedisReviewRow{}
	}
	notes := []string{}
	if sum != nil && sum.ExecCount == 0 {
		notes = append(notes, "该时间范围内没有 Redis 慢查询记录。数据来自定时采集（SLOWLOG GET），可点“立即采集”拉取一次；采集器的开关与调度见“采集状态”。")
	}
	return &RedisReviewPage{Total: total, Rows: rows, Summary: sum, From: startDay, To: endDay, Notes: notes}, nil
}

// RedisHistory 返回 Redis 慢查询明细（每条慢命令一行，含客户端地址与完整命令）。
func (s *Service) RedisHistory(p Principal, in RedisParams) (*RedisHistoryPage, error) {
	if _, err := s.redisInstance(p, in.InstanceID); err != nil {
		return nil, err
	}
	q, startDay, endDay, err := s.redisQuery(in, 7)
	if err != nil {
		return nil, err
	}
	if q.Sort, err = validSort(in.Sort, "start_time", redisHistorySorts); err != nil {
		return nil, err
	}
	if q.Desc, err = validOrder(in.Order); err != nil {
		return nil, err
	}
	if q.Limit, q.Offset, err = validPaging(in.Limit, in.Offset); err != nil {
		return nil, err
	}
	rows, total, err := s.redis.Repo.RedisHistory(q)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].CommandText, _ = clipText(rows[i].CommandText)
	}
	if rows == nil {
		rows = []RedisHistoryRow{}
	}
	return &RedisHistoryPage{Total: total, Rows: rows, From: startDay, To: endDay}, nil
}

// RedisTrend 返回慢查次数 / 耗时趋势（按天或按小时）；可限定命令或指纹。
func (s *Service) RedisTrend(p Principal, in RedisParams) (*RedisTrend, error) {
	if _, err := s.redisInstance(p, in.InstanceID); err != nil {
		return nil, err
	}
	q, startDay, endDay, err := s.redisQuery(in, 7)
	if err != nil {
		return nil, err
	}
	switch in.Bucket {
	case "", "day":
		q.Bucket = "day"
	case "hour":
		q.Bucket = "hour"
	default:
		return nil, badReq("趋势粒度只能是 day 或 hour")
	}
	pts, err := s.redis.Repo.RedisTrend(q)
	if err != nil {
		return nil, err
	}
	if pts == nil {
		pts = []RedisTrendPoint{}
	}
	return &RedisTrend{Bucket: q.Bucket, Points: pts, From: startDay, To: endDay}, nil
}

// RedisCommands 返回时间范围内出现过的命令名（命令筛选下拉）。
func (s *Service) RedisCommands(p Principal, in RedisParams) ([]string, error) {
	if _, err := s.redisInstance(p, in.InstanceID); err != nil {
		return nil, err
	}
	q, _, _, err := s.redisQuery(in, 7)
	if err != nil {
		return nil, err
	}
	out, err := s.redis.Repo.RedisCommands(q)
	if out == nil {
		out = []string{}
	}
	return out, err
}

// RedisCollectorOverview 是采集器状态：开关来自本进程的采集器配置，游标与状态来自库。
type RedisCollectorOverview struct {
	Enabled     bool         `json:"enabled"` // 本进程的慢日志采集器是否在运行（默认开启，环境变量 SLOWLOG_COLLECT_ENABLE=0 可关闭）
	IntervalSec int          `json:"interval_sec"`
	TableReady  bool         `json:"table_ready"`
	Items       []RedisState `json:"items"`
}

func (s *Service) RedisCollectorOverview(p Principal) (*RedisCollectorOverview, error) {
	if !s.redisOn() {
		return nil, newErr(http.StatusBadRequest, "unsupported", "Redis 慢查询未启用")
	}
	if err := s.requirePerm(p, viewButtons...); err != nil {
		return nil, err
	}
	sc, err := s.scopeOf(p)
	if err != nil {
		return nil, err
	}
	ov := &RedisCollectorOverview{Items: []RedisState{}, TableReady: s.redis.Repo.RedisTablesReady()}
	if s.collector != nil {
		ov.Enabled = s.collector.Enabled()
	}
	if s.redis.Collector != nil {
		ov.IntervalSec = int(s.redis.Collector.MinGap / time.Second)
	}
	if !ov.TableReady {
		return ov, nil
	}
	states, err := s.redis.Repo.RedisStates()
	if err != nil {
		return nil, err
	}
	names := map[uint]string{}
	if list, err := s.repo.ListInstances(); err == nil {
		for _, in := range list {
			if in.DBType == "redis" {
				names[in.ID] = in.InstanceName
			}
		}
	}
	for _, st := range states {
		name, ok := names[st.InstanceID]
		if !ok || !sc.allows(st.InstanceID) {
			continue
		}
		st.InstanceName = name
		ov.Items = append(ov.Items, st)
	}
	sort.Slice(ov.Items, func(i, j int) bool { return ov.Items[i].InstanceName < ov.Items[j].InstanceName })
	return ov, nil
}

// RedisCollectNow 立即对一个实例采集一次（SLOWLOG GET），返回新增条数。同一实例至少间隔 redisManualCollectGap。
func (s *Service) RedisCollectNow(ctx context.Context, p Principal, instanceID uint) (int, error) {
	inst, err := s.redisInstance(p, instanceID)
	if err != nil {
		return 0, err
	}
	if s.redis.Collector == nil {
		return 0, newErr(http.StatusBadRequest, "unsupported", "Redis 慢查询采集器未启用")
	}
	if !s.redis.allowManual(inst.ID, s.now()) {
		return 0, newErr(http.StatusTooManyRequests, "too_frequent", "操作过于频繁，请 %d 秒后再试", int(redisManualCollectGap/time.Second))
	}
	n, err := s.redis.Collector.CollectNow(ctx, inst)
	if err != nil {
		return 0, newErr(http.StatusBadGateway, "collect_failed", "采集失败：%s", errText(err))
	}
	return n, nil
}

func (r *redisModule) allowManual(id uint, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.lastManual[id]; ok && now.Sub(last) < redisManualCollectGap {
		return false
	}
	r.lastManual[id] = now
	return true
}

// RedisResetResult 是重置慢日志的结果。
type RedisResetResult struct {
	Cleared   int64 `json:"cleared"`   // 重置前实例上的慢日志条数
	Collected int   `json:"collected"` // 重置前先采集入库的新增条数
}

// RedisReset 执行 SLOWLOG RESET。高危：只有 redis.slowlog_reset 按钮（接口层校验）。
// 重置前先采集一次入库，避免丢数据；采集失败则取消重置。写审计。
func (s *Service) RedisReset(ctx context.Context, p Principal, instanceID uint) (*RedisResetResult, error) {
	inst, err := s.redisInstance(p, instanceID)
	if err != nil {
		return nil, err
	}
	if s.redis.Collector == nil || s.redis.Open == nil {
		return nil, newErr(http.StatusBadRequest, "unsupported", "Redis 慢查询采集器未启用")
	}
	collected, err := s.redis.Collector.CollectNow(ctx, inst)
	if err != nil {
		s.auditRedis(p, inst, fmt.Sprintf("结果：失败（重置前的保底采集失败，已取消重置：%s）", errText(err)))
		return nil, newErr(http.StatusBadGateway, "collect_failed", "重置前的保底采集失败，已取消重置：%s", errText(err))
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	t, err := s.redis.Open(cctx, s.redis.Conn(inst))
	if err != nil {
		s.auditRedis(p, inst, fmt.Sprintf("结果：失败（无法连接实例，未重置：%s）", errText(err)))
		return nil, newErr(http.StatusBadGateway, "connect_failed", "无法连接实例：%s", errText(err))
	}
	defer t.Close()
	n, _ := t.SlowlogLen(cctx)
	if err := t.SlowlogReset(cctx); err != nil {
		s.auditRedis(p, inst, fmt.Sprintf("结果：失败（%s）", errText(err)))
		return nil, newErr(http.StatusBadGateway, "reset_failed", "SLOWLOG RESET 失败：%s", errText(err))
	}
	s.auditRedis(p, inst, fmt.Sprintf("结果：成功；重置前实例上有 %d 条慢日志，其中新采集入库 %d 条（%s）", n, collected, redisResetButton))
	return &RedisResetResult{Cleared: n, Collected: collected}, nil
}

func (s *Service) auditRedis(p Principal, inst *model.Instance, detail string) {
	e := &model.AuditEntry{UserID: p.ID, UserName: p.Username, UserDisplay: p.Display, Action: actionRedisReset,
		ExtraInfo: fmt.Sprintf("实例：%s（#%d）；%s", inst.InstanceName, inst.ID, detail), ActionTime: s.now()}
	if err := s.repo.InsertAudit(e); err != nil {
		// 审计写失败不影响已完成的操作，但要留痕
		slog.Error("写 Redis 慢日志重置审计失败", "err", err)
	}
}
