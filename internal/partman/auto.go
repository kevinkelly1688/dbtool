// @author: kevinkelly
// @license: Apache Licence
// @file: auto.go
// @time: 2026/10/28

package partman

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/query"
	"dbtool/internal/settings"
)

// 定时维护由 dbt_setting 里的三个开关控制（缺省全部关闭；外部 partman 程序已并入本模块，启用前请先停掉外部程序）：
//
//	partition.auto_run       true 才启用定时维护
//	partition.auto_drop      true 才允许定时维护删除过期分区（默认只续建）
//	partition.auto_interval_minutes  两轮之间的间隔（分钟），默认 180，范围 5 ~ 1440
const (
	cfgAutoRun      = settings.KeyPartitionAutoRun
	cfgAutoDrop     = settings.KeyPartitionAutoDrop
	cfgAutoInterval = settings.KeyPartitionAutoIntervalMinutes

	defaultAutoInterval = 180
	firstRunDelay       = time.Minute
)

// AutoSettings 是定时维护的当前设置。
type AutoSettings struct {
	Enabled         bool `json:"enabled"`
	Drop            bool `json:"drop"`
	IntervalMinutes int  `json:"interval_minutes"`
}

func (s *Service) autoSettings() AutoSettings {
	flag := func(item string) bool {
		v, ok := s.meta.ConfigValue(item)
		return ok && strings.EqualFold(strings.TrimSpace(v), "true")
	}
	a := AutoSettings{Enabled: flag(cfgAutoRun), Drop: flag(cfgAutoDrop), IntervalMinutes: defaultAutoInterval}
	if v, ok := s.meta.ConfigValue(cfgAutoInterval); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 5 && n <= 1440 {
			a.IntervalMinutes = n
		}
	}
	return a
}

// autoCaller 是定时任务的“操作人”，写进审计与日志。
var autoCaller = Caller{Principal: query.Principal{Username: "system", Display: "分区维护定时任务", IsSuperuser: true}} // i18n-ignore：写进审计表的操作人名

// AutoSummary 是一轮定时维护的统计。
type AutoSummary struct {
	Enabled  bool
	Checked  int
	Executed int      // 执行了语句的配置数
	Skipped  int      // 跳过：不支持的实例类型、最近刚有人维护过、拿不到锁、未分区
	Failed   int      // 计划失败或语句执行失败的配置数
	Notes    []string // 每个跳过 / 失败项的原因
}

// Worker 是登记给 server.AddWorker 的后台任务：每隔设定的间隔跑一轮 RunAuto，开关每轮重新读取，修改后无需重启。
func (s *Service) Worker(ctx context.Context) {
	timer := time.NewTimer(firstRunDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		sum := s.RunAuto(ctx)
		if sum.Enabled {
			slog.Info("分区定时维护完成", "checked", sum.Checked, "executed", sum.Executed, "skipped", sum.Skipped, "failed", sum.Failed)
			for _, n := range sum.Notes {
				slog.Warn("分区定时维护", "note", n)
			}
		}
		// 可用分区数量告警排在维护之后：续建成功就不会推；自动维护没开时同样检查（那正是最需要告警的情形）
		if as := s.RunAlerts(ctx); as.Enabled && as.Checked > 0 {
			slog.Info("分区可用数量检查完成", "checked", as.Checked, "alerted", as.Alerted, "repeated", as.Repeated, "recovered", as.Recovered, "failed", as.Failed)
			for _, n := range as.Notes {
				slog.Warn("分区可用数量检查", "note", n)
			}
		}
		timer.Reset(time.Duration(s.autoSettings().IntervalMinutes) * time.Minute)
	}
}

// RunAuto 跑一轮定时维护。
//
// 外部 partman 程序已并入本模块，不再有“与外部程序并存”的问题；但多个 dbtool 实例、手动与定时仍可能撞车，所以保留：
//  1. 默认关闭（partition.auto_run）；
//  2. 计划永远基于目标表“此刻”的真实分区计算，已经建好的分区不会再建，重复执行是空操作；
//  3. 近期日志保护窗口：某配置最近刚有人（手动、上一轮、另一个进程）写过维护日志，就跳过本轮，避开几乎同时的触发；
//  4. 同一配置在多个 dbtool 进程之间用 GET_LOCK 互斥（本进程内用内存标记，与手动执行互斥）。
//
// 定时维护只做低风险的事：续建分区；删除过期分区仅在 partition.auto_drop=true 时执行；REORGANIZE 与初始化未分区表永远不自动执行。
// 每一轮把“检查了谁、跳过了谁及原因、执行 / 失败了谁”写成一条系统记录（cnf_id=0，source=auto），平台日志页可见。
func (s *Service) RunAuto(ctx context.Context) AutoSummary {
	set := s.autoSettings()
	sum := AutoSummary{Enabled: set.Enabled}
	if !set.Enabled {
		return sum
	}
	started := s.now()
	rt := newTracer(s.now)
	guard := time.Duration(set.IntervalMinutes) * time.Minute / 2
	if guard > 30*time.Minute {
		guard = 30 * time.Minute
	}
	rt.add("INFO", "=== 定时维护轮次开始 | 间隔=%d分钟 自动删除=%v 保护窗口=%s 推送时机=%s ===", set.IntervalMinutes, set.Drop, guard, s.notifyOn())
	nothing := 0
	round := &RoundSummary{GuardMinutes: int(guard / time.Minute), AutoDrop: set.Drop, NotifyOn: s.notifyOn(), Items: []RoundItem{}}
	const maxRoundItems = 100
	addItem := func(cnf int64, table, result, reason string) {
		if len(round.Items) >= maxRoundItems {
			round.ItemsCut++
			return
		}
		round.Items = append(round.Items, RoundItem{CnfID: cnf, Table: table, Result: result, Reason: reason})
	}
	finish := func() AutoSummary {
		round.Checked, round.Executed, round.Nothing, round.Skipped, round.Failed = sum.Checked, sum.Executed, nothing, sum.Skipped, sum.Failed
		rt.add("INFO", "=== 定时维护轮次结束 | 检查 %d，执行 %d，无需维护 %d，跳过 %d，失败 %d，耗时 %d ms ===",
			sum.Checked, sum.Executed, nothing, sum.Skipped, sum.Failed, s.now().Sub(started).Milliseconds())
		if sum.Checked > 0 || sum.Failed > 0 {
			status := "success"
			if sum.Failed > 0 {
				status = "failed"
			}
			s.writeSystemLog(rt.lines, logItem{Status: status, Operator: autoCaller.Username, Source: SourceAuto, ExecAt: started.Format(timeLayout),
				Event: "auto_round", Round: round})
		}
		return sum
	}
	insts, err := s.meta.ListInstances()
	if err != nil {
		sum.Failed++
		sum.Notes = append(sum.Notes, fmt.Sprintf("读取实例列表失败：%s", err.Error()))
		rt.add("ERROR", "读取实例列表失败：%v", err)
		return finish()
	}
	byID := make(map[uint]*model.Instance, len(insts))
	for i := range insts {
		byID[insts[i].ID] = &insts[i]
	}
	enabled := true
	cfgs, _, err := s.repo.ListConfigs(ConfigFilter{AllInstances: true, IsEnable: &enabled})
	if err != nil {
		sum.Failed++
		sum.Notes = append(sum.Notes, fmt.Sprintf("读取分区配置失败：%s", err.Error()))
		rt.add("ERROR", "读取分区配置失败：%v", err)
		return finish()
	}
	ids := make([]int64, 0, len(cfgs))
	for _, c := range cfgs {
		ids = append(ids, c.CnfID)
	}
	last, err := s.repo.LastLogTimes(ids)
	if err != nil {
		sum.Failed++
		sum.Notes = append(sum.Notes, fmt.Sprintf("读取维护日志失败：%s", err.Error()))
		rt.add("ERROR", "读取维护日志失败：%v", err)
		return finish()
	}
	rt.add("INFO", "启用的分区配置 %d 个", len(cfgs))
	for _, cfg := range cfgs {
		if ctx.Err() != nil {
			rt.add("WARN", "收到停止信号，本轮提前结束")
			return finish()
		}
		sum.Checked++
		label := fmt.Sprintf("#%d %s.%s", cfg.CnfID, cfg.DBName, cfg.TbName)
		inst := byID[cfg.InstanceID]
		switch {
		case inst == nil:
			sum.Skipped++
			sum.Notes = append(sum.Notes, fmt.Sprintf("%s：实例不存在", label))
			rt.addCnf("WARN", cfg.CnfID, "%s：跳过——实例 #%d 不存在", label, cfg.InstanceID)
			addItem(cfg.CnfID, tableLabel(&cfg), "skipped", fmt.Sprintf("实例 #%d 不存在", cfg.InstanceID))
			continue
		case !SupportedDBType(inst.DBType):
			sum.Skipped++
			rt.addCnf("INFO", cfg.CnfID, "%s：跳过——实例 %s 的类型 %s 不支持分区维护", label, inst.InstanceName, inst.DBType)
			addItem(cfg.CnfID, tableLabel(&cfg), "skipped", fmt.Sprintf("实例 %s 的类型 %s 不支持分区维护", inst.InstanceName, inst.DBType))
			continue
		}
		if t, ok := last[cfg.CnfID]; ok && s.now().Sub(t) < guard {
			sum.Skipped++
			why := fmt.Sprintf("%s 前已有维护日志（近期日志保护窗口 %s，上次 %s）", s.now().Sub(t).Round(time.Second), guard, t.Format(timeLayout))
			rt.addCnf("INFO", cfg.CnfID, "%s：跳过——%s", label, why)
			addItem(cfg.CnfID, tableLabel(&cfg), "skipped", why)
			continue
		}
		res, reason := s.autoOne(ctx, cfg, inst, set)
		switch res {
		case autoExecuted:
			sum.Executed++
			rt.addCnf("INFO", cfg.CnfID, "%s：已执行，%s", label, reason)
			addItem(cfg.CnfID, tableLabel(&cfg), "executed", reason)
		case autoSkipped:
			sum.Skipped++
			rt.addCnf("INFO", cfg.CnfID, "%s：跳过——%s", label, reason)
			addItem(cfg.CnfID, tableLabel(&cfg), "skipped", reason)
		case autoFailed:
			sum.Failed++
			sum.Notes = append(sum.Notes, fmt.Sprintf("%s：执行失败，详见维护日志与审计日志", label))
			rt.addCnf("ERROR", cfg.CnfID, "%s：失败——%s", label, reason)
			addItem(cfg.CnfID, tableLabel(&cfg), "failed", reason)
		default:
			nothing++
			if reason != "" {
				rt.addCnf("INFO", cfg.CnfID, "%s：无需维护——%s", label, reason)
				addItem(cfg.CnfID, tableLabel(&cfg), "nothing", reason)
			}
		}
	}
	return finish()
}

type autoResult int

const (
	autoNothing autoResult = iota // 分区已是最新
	autoExecuted
	autoSkipped
	autoFailed
)

// autoOne 处理一个配置，返回结果与一句话原因（写进轮次摘要）。
func (s *Service) autoOne(ctx context.Context, cfg Config, inst *model.Instance, set AutoSettings) (autoResult, string) {
	if err := dbAllowed(inst, cfg.DBName); err != nil {
		slog.Warn("分区定时维护跳过：库名被实例的过滤规则禁止", "cnf_id", cfg.CnfID, "err", err)
		return autoSkipped, fmt.Sprintf("库名被实例的过滤规则禁止（%s）", err.Error())
	}
	if !s.acquire(cfg.CnfID) {
		return autoSkipped, "本进程里该配置正在维护（手动执行或上一轮尚未结束）"
	}
	defer s.release(cfg.CnfID)
	release, ok, err := s.repo.TryLock(ctx, lockName(cfg.CnfID))
	if err != nil {
		slog.Warn("分区定时维护取锁失败", "cnf_id", cfg.CnfID, "err", err)
		return autoFailed, fmt.Sprintf("取锁失败：%s", err.Error())
	}
	if !ok {
		return autoSkipped, "另一个进程正在维护该配置（GET_LOCK 被占用）"
	}
	defer release()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	started := s.now()
	tr := newTracer(s.now)
	p, err := s.plan(ctx, inst, cfg, nil)
	if err != nil {
		slog.Warn("分区定时维护：计划失败", "cnf_id", cfg.CnfID, "table", tableLabel(&cfg), "err", err)
		tr.addAt(started, "INFO", 0, "%s", startLine(&cfg, inst, SourceAuto, autoCaller.Username, tzDescOf(&cfg, nil)))
		tr.add("ERROR", "对账过程出错: %v", err)
		s.recordAutoFailure(ctx, &cfg, tr, started, err)
		return autoFailed, fmt.Sprintf("计划失败：%s", clipMsg(err.Error(), 200))
	}
	tr.addAt(started, "INFO", 0, "%s", startLine(&cfg, inst, SourceAuto, autoCaller.Username, tzDescOf(&cfg, p)))
	tr.addPlan(p)
	if p.Kind == KindInit {
		// 定时维护永远不初始化：只提示，不写维护日志、不推送。
		slog.Warn("分区定时维护：表未分区，定时维护不会初始化，请在页面上手动初始化", "cnf_id", cfg.CnfID, "table", tableLabel(&cfg))
		return autoSkipped, "表未分区：定时维护不会初始化，请在页面上手动“初始化为分区表”"
	}
	var stmts []Statement
	var notes []string
	stmts = append(stmts, p.Adds...)
	if len(p.Drops) > 0 {
		if set.Drop {
			stmts = append(stmts, p.Drops...)
		} else {
			n := fmt.Sprintf("有 %d 个待清理分区，partition.auto_drop 未开启，本轮不删除", len(p.Drops))
			notes = append(notes, n)
			tr.add("INFO", "%s", n)
		}
	}
	if p.Reorganize != nil {
		n := "表里有 MAXVALUE 分区，续建需要人工执行 REORGANIZE，定时维护不会执行"
		notes = append(notes, n)
		tr.add("WARN", "%s", n)
		slog.Warn("分区定时维护：表里有 MAXVALUE 分区，续建需要人工执行 REORGANIZE", "cnf_id", cfg.CnfID, "table", tableLabel(&cfg))
	}
	if len(stmts) == 0 {
		return autoNothing, strings.Join(notes, "；")
	}
	res, err := s.execute(ctx, autoCaller, &cfg, inst, stmts, SourceAuto, tr, started)
	if err != nil {
		return autoFailed, fmt.Sprintf("执行前失败：%s", clipMsg(err.Error(), 200))
	}
	if res.Failed {
		return autoFailed, "有语句执行失败，详见该配置的维护日志"
	}
	return autoExecuted, fmt.Sprintf("%d 条语句成功", len(stmts))
}

// recordAutoFailure 把定时维护里的计划失败（表不是分区表、字段类型不符、连不上目标库等）记进维护日志（页面可见原因）并推送。
func (s *Service) recordAutoFailure(ctx context.Context, cfg *Config, tr *tracer, started time.Time, planErr error) {
	ev := maintEvent{Env: cfg.Env, DB: cfg.DBName, Table: cfg.TbName, Err: planErr,
		Records: []pushRecord{{SQL: "(reconcile)", Result: "failed:" + planErr.Error()}}}
	tr.lines = append(tr.lines, s.pushMaintenance(ctx, ev)...)
	tr.add("INFO", "=== 分区维护结束 | 成功 0 / 失败 1 / 跳过 0，耗时 %d ms ===", s.now().Sub(started).Milliseconds())
	at := s.now()
	items := []logItem{{SQLText: "", Status: "failed", Error: planErr.Error(), Operator: autoCaller.Username, Source: SourceAuto, ExecAt: at.Format(timeLayout)}}
	body := buildLogBody(items, tr.lines, logItem{Operator: autoCaller.Username, Source: SourceAuto, ExecAt: at.Format(timeLayout)})
	if _, err := s.repo.InsertLog(cfg.CnfID, body, at); err != nil {
		slog.Error("写入 dbt_partman_log 失败", "cnf_id", cfg.CnfID, "err", err)
	}
}
