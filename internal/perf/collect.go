// @author: kevinkelly
// @license: Apache Licence
// @file: collect.go
// @time: 2026/10/28

package perf

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// 采集编排：按采集配置取数据 → 入库 → 推进游标与采集结果。

const (
	defaultWindowMinutes = 60
	maxCatchupWindows    = 24 // 长时间停机后一次最多补采的窗口数
	collectLockTTL       = 30 * time.Minute
	collectParallel      = 3 // 同时采集的实例数上限
	runningPrefix        = "RUNNING@"
)

func (s *Service) collectWindow() time.Duration {
	n := s.cfgInt(settings.KeyPerfCollectWindowMinutes, defaultWindowMinutes)
	if n < 1 {
		n = defaultWindowMinutes
	}
	return time.Duration(n) * time.Minute
}

// acquire 本进程内同一配置同时只跑一个采集（定时采集与“试采集”之间互斥）。
func (s *Service) acquire(id uint) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.collecting[id] {
		return false
	}
	s.collecting[id] = true
	return true
}

func (s *Service) release(id uint) {
	s.mu.Lock()
	delete(s.collecting, id)
	s.mu.Unlock()
}

func (s *Service) collectorFor(name string) (Collector, error) {
	c, ok := s.collectors[name]
	if !ok || c == nil {
		return nil, fmt.Errorf("不支持的采集方式: %s", name)
	}
	return c, nil
}

// CollectDue 采集所有到期的启用配置：距上次采集窗口结束已满一个窗口（或从未采集）的才采。返回每个实例的结果。
func (s *Service) CollectDue(ctx context.Context) (map[uint]string, error) {
	cfgs, err := s.repo.ListCollectConfigs()
	if err != nil {
		return nil, err
	}
	now := s.now()
	window := s.collectWindow()
	var due []uint
	for _, c := range cfgs {
		if !c.Enabled || (c.LastCollectTime != nil && now.Sub(*c.LastCollectTime) < window) {
			continue
		}
		due = append(due, c.ID)
	}
	// 实例之间互不影响：最多同时采集 collectParallel 个，一个实例慢或失败不拖累其他实例
	out := map[uint]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, collectParallel)
	for _, id := range due {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(id uint) {
			defer func() { <-sem; wg.Done() }()
			msg, err := s.CollectOne(ctx, id, nil, nil)
			if err != nil {
				msg = "ERROR " + err.Error()
			}
			mu.Lock()
			out[id] = msg
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return out, nil
}

// CollectOne 采集一个实例。窗口从 last_collect_time 接续（保证不重不漏），一次最多补 24 个窗口。
// start/end 都给出时是补采：使用指定窗口，不推进 last_collect_time，也不动游标。
//
// 防重入：先在库里抢占（dbt_perf_collect_config.last_status 置为 RUNNING@时间，30 分钟过期），抢不到说明别的进程正在采。
func (s *Service) CollectOne(ctx context.Context, cfgID uint, start, end *time.Time) (string, error) {
	cfg, err := s.repo.GetCollectConfig(cfgID)
	if err != nil {
		return "", err
	}
	if cfg == nil {
		return "", notFound("采集配置不存在")
	}
	if !s.acquire(cfgID) {
		return "正在采集，跳过", nil
	}
	defer s.release(cfgID)
	now := s.now()
	tok := runningPrefix + now.Format("2006-01-02T15:04:05")
	stale := runningPrefix + now.Add(-collectLockTTL).Format("2006-01-02T15:04:05")
	ok, err := s.repo.LockCollect(cfgID, tok, stale)
	if err != nil {
		return "", err
	}
	if !ok {
		return "正在采集，跳过", nil
	}
	backfill := start != nil && end != nil
	finish := func(endTime *time.Time, msg string) string {
		if err := s.repo.FinishCollect(cfgID, endTime, msg); err != nil {
			slog.Error("写采集结果失败", "config", cfgID, "err", err)
		}
		return msg
	}
	fail := func(err error) (string, error) {
		slog.Error("性能工单采集失败", "config", cfgID, "err", err)
		return finish(nil, fmt.Sprintf("%s ERROR %s", now.Format("01-02 15:04"), clipRunes(err.Error(), 400))), nil
	}
	inst, err := s.repo.GetInstance(cfg.InstanceID)
	if err != nil || inst == nil {
		return fail(fmt.Errorf("实例不存在: %v", err))
	}
	col, err := s.collectorFor(cfg.Collector)
	if err != nil {
		return fail(err)
	}
	var ws, we time.Time
	if backfill {
		ws, we = *start, *end
	} else {
		cur := now.Truncate(time.Minute)
		window := s.collectWindow()
		if cfg.LastCollectTime != nil {
			ws = *cfg.LastCollectTime
		} else {
			ws = cur.Add(-window)
		}
		we = ws.Add(window * maxCatchupWindows)
		if cur.Before(we) {
			we = cur
		}
		if !we.After(ws) {
			return finish(nil, cfg.LastStatus), nil // 没有新窗口：恢复原来的状态文本，释放抢占
		}
	}
	res, err := col.Collect(ctx, CollectRequest{Config: cfg, Instance: inst, Start: ws, End: we, Backfill: backfill, Now: now})
	if err != nil {
		return fail(err)
	}
	ing, err := s.Ingest(ctx, cfg, inst, res.Digests, we)
	if err != nil {
		return fail(err)
	}
	if res.Commit != nil {
		if err := res.Commit(); err != nil {
			return fail(fmt.Errorf("提交游标失败: %v", err))
		}
	}
	msg := fmt.Sprintf("%s OK 窗口%s~%s 指纹%d 新建%d 更新%d", now.Format("01-02 15:04"), ws.Format("01-02 15:04"), we.Format("15:04"),
		len(res.Digests), ing.Created, ing.Updated)
	if res.Note != "" {
		msg += "（" + clipRunes(res.Note, 80) + "）"
	}
	var endTime *time.Time
	if !backfill {
		endTime = &we
	}
	return finish(endTime, msg), nil
}

// ---- 试采集（只预览不建单） ----

// PreviewRow 是试采集预览里的一个指纹。
type PreviewRow struct {
	DBName      string  `json:"db_name"`
	SQL         string  `json:"sql"`
	ExecCount   int64   `json:"exec_count"`
	AvgMs       float64 `json:"avg_ms"`
	MaxMs       float64 `json:"max_ms"`
	TotalSec    float64 `json:"total_sec"`
	WouldCreate bool    `json:"would_create"` // 达到建单阈值且没有被排除
	IssueID     uint    `json:"issue_id"`     // 已有工单的编号，没有为 0
}

// PreviewResult 是试采集的结果。
type PreviewResult struct {
	WindowStart string       `json:"window_start"`
	WindowEnd   string       `json:"window_end"`
	Total       int          `json:"total"`   // 采到的指纹数
	Matched     int          `json:"matched"` // 达到阈值的
	Rows        []PreviewRow `json:"rows"`    // 按总耗时倒序，最多 50 条
	Note        string       `json:"note"`
}

const previewMaxRows = 50

// PreviewCollect 用表单里的配置（可以是尚未保存的）采集最近一个窗口并展示结果，不建单、不写游标、不改采集状态。
func (s *Service) PreviewCollect(ctx context.Context, in CollectConfigInput) (*PreviewResult, error) {
	cfg, inst, err := s.buildCollectConfig(in, true)
	if err != nil {
		return nil, err
	}
	// 与同一配置的定时采集互斥（进程内）；还没保存的配置按实例占位。试采集不占用库里的抢占，也不写采集状态
	key := cfg.ID
	if key == 0 {
		key = ^uint(0) - cfg.InstanceID
	}
	if !s.acquire(key) {
		return nil, conflict("该实例正在采集或试采集，请稍后再试")
	}
	defer s.release(key)
	col, err := s.collectorFor(cfg.Collector)
	if err != nil {
		return nil, bad(err.Error())
	}
	end := s.now().Truncate(time.Minute)
	start := end.Add(-s.collectWindow())
	res, err := col.Collect(ctx, CollectRequest{Config: cfg, Instance: inst, Start: start, End: end, Backfill: true, RowLimit: 20000, Now: s.now()})
	if err != nil {
		return nil, newErr(502, "collect_failed", fmt.Sprintf("试采集失败：%s", clipRunes(err.Error(), 300)))
	}
	out := &PreviewResult{WindowStart: start.Format(tsLayout), WindowEnd: end.Format(tsLayout), Total: len(res.Digests), Note: res.Note, Rows: []PreviewRow{}}
	ds := res.Digests
	for i := range ds {
		ds[i].Normalize()
	}
	sort.SliceStable(ds, func(a, b int) bool { return ds[a].TotalSec > ds[b].TotalSec })
	for i := range ds {
		d := &ds[i]
		ok := d.ExecCount > 0 && passes(cfg, d)
		if ok {
			out.Matched++
		}
		if len(out.Rows) >= previewMaxRows {
			continue
		}
		row := PreviewRow{DBName: d.DBName, SQL: clipRunes(d.SampleSQL, 300), ExecCount: d.ExecCount, AvgMs: round(d.AvgMs(), 1), MaxMs: round(d.MaxMs, 1),
			TotalSec: round(d.TotalSec, 2), WouldCreate: ok}
		if old, _ := s.repo.FindIssue(cfg.InstanceID, d.DBName, d.Checksum); old != nil {
			row.IssueID = old.ID
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

// instanceOf 取实例并校验类型受支持。
func (s *Service) instanceOf(id uint) (*model.Instance, error) {
	inst, err := s.repo.GetInstance(id)
	if err != nil || inst == nil {
		return nil, bad("所选实例不存在")
	}
	if !contains(SupportedDBTypes, inst.DBType) {
		return nil, bad(fmt.Sprintf("暂不支持 %s 类型的实例（支持：%s）", inst.DBType, strings.Join(SupportedDBTypes, "、")))
	}
	return inst, nil
}

var errNoConfig = errors.New("没有采集配置")
