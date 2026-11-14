// @author: kevinkelly
// @license: Apache Licence
// @file: planrun.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/settings"
)

const (
	missGrace      = 15 * time.Minute // 到点后超过这个时间才发现（服务没在运行 / 调度延迟），不再补跑：在业务高峰期补一轮巡检比漏掉一次更糟
	heartbeatEvery = 30 * time.Second
	staleRunAfter  = 5 * time.Minute // 运行记录心跳超过这个时间没更新，视为进程中断
	cleanupEvery   = time.Hour
	dueBatch       = 20
	cleanupBatch   = 500
	runKeepMin     = 30 * 24 * time.Hour // 运行记录至少保留 30 天，即使报告保留期更短
	lowScore       = 70                  // MySQL 系健康评分低于它算“低分”，通知里单独点名
)

// PlanView 是计划列表里的一行。
type PlanView struct {
	Plan
	Describe      string   `json:"describe"`
	InstanceNames []string `json:"instance_names"` // 可见的前几个
	InstanceTotal int      `json:"instance_total"` // 显式选择 + 标签当前命中（去重）
	Hidden        int      `json:"hidden"`         // 你看不到的实例数（资源组之外）
	TagMatched    int      `json:"tag_matched"`
	Running       bool     `json:"running"`
	LastRun       *Run     `json:"last_run"`
	ChannelNames  []string `json:"channel_names"`
}

// resolveInstances 解析计划实际要巡检的实例：显式选择 + 标签命中，去重，只保留存在且支持巡检的，按名称排序。
func (s *Service) resolveInstances(p *Plan) ([]model.Instance, error) {
	ids := append([]uint{}, p.InstanceIDs...)
	if len(p.TagCodes) > 0 {
		tagged, err := s.repo.InstanceIDsByTags(p.TagCodes)
		if err != nil {
			return nil, err
		}
		ids = append(ids, tagged...)
	}
	var out []model.Instance
	disabled := s.disabledIDs()
	for _, id := range uniqUint(ids) {
		if disabled[id] { // 实例已停用：定时巡检也不再连接它
			continue
		}
		inst, err := s.repo.GetInstance(id)
		if err != nil || inst == nil {
			continue
		}
		if _, ok := s.kindOf(inst.DBType); ok {
			out = append(out, *inst)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceName < out[j].InstanceName })
	return out, nil
}

func (s *Service) planReady() error {
	if !s.repo.PlanTablesReady() {
		return &Error{Status: 409, Code: "table_missing", Message: "定时巡检的表还没有创建，请先在数据库里执行 deploy/sql/013_inspect.sql"}
	}
	return nil
}

// PlansReady 报告定时计划相关的表是否已创建。
func (s *Service) PlansReady() bool { return s.repo.PlanTablesReady() }

func (s *Service) channelMap() map[uint]ChannelOption {
	out := map[uint]ChannelOption{}
	if s.notifier == nil {
		return out
	}
	list, err := s.notifier.Channels()
	if err != nil {
		slog.Warn("读取通知渠道失败", "err", err)
		return out
	}
	for _, c := range list {
		out[c.ID] = c
	}
	return out
}

// Channels 返回可选的通知渠道。
func (s *Service) Channels() []ChannelOption {
	m := s.channelMap()
	out := make([]ChannelOption, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ListPlans 返回计划列表；实例名只显示用户可见的部分。
func (s *Service) ListPlans(scope Scope) ([]PlanView, error) {
	if err := s.planReady(); err != nil {
		return nil, err
	}
	plans, err := s.repo.ListPlans()
	if err != nil {
		return nil, err
	}
	insts, err := s.repo.ListInstances()
	if err != nil {
		return nil, err
	}
	byID := map[uint]model.Instance{}
	for _, in := range insts {
		byID[in.ID] = in
	}
	channels := s.channelMap()
	out := make([]PlanView, 0, len(plans))
	for i := range plans {
		p := plans[i]
		v := PlanView{Plan: p, Describe: p.Describe(), InstanceNames: []string{}, ChannelNames: []string{}}
		union := map[uint]bool{}
		var visible []uint
		for _, id := range p.InstanceIDs {
			union[id] = true
			if scope.Has(id) {
				visible = append(visible, id)
			} else {
				v.Hidden++
			}
		}
		if len(p.TagCodes) > 0 {
			if tagged, err := s.repo.InstanceIDsByTags(p.TagCodes); err == nil {
				for _, id := range tagged {
					if _, ok := s.kindOf(byID[id].DBType); ok {
						v.TagMatched++
						union[id] = true
					}
				}
			}
		}
		v.InstanceTotal = len(union)
		v.Plan.InstanceIDs = visible
		if v.Plan.InstanceIDs == nil {
			v.Plan.InstanceIDs = []uint{}
		}
		var names []string
		for _, id := range visible {
			names = append(names, byID[id].InstanceName)
		}
		sort.Strings(names)
		if len(names) > 5 {
			names = names[:5]
		}
		v.InstanceNames = append(v.InstanceNames, names...)
		for _, cid := range p.NotifyChannels {
			if c, ok := channels[cid]; ok {
				v.ChannelNames = append(v.ChannelNames, c.Name)
			} else {
				v.ChannelNames = append(v.ChannelNames, fmt.Sprintf("#%d（已删除）", cid))
			}
		}
		if p.LastRunID != 0 {
			if r, err := s.repo.GetRun(p.LastRunID); err == nil {
				v.LastRun = r
			}
		}
		if r, err := s.repo.ActiveRun(p.ID, s.now().Add(-staleRunAfter)); err == nil && r != nil {
			v.Running = true
		}
		if v.Plan.Weekdays == nil {
			v.Plan.Weekdays = []int{}
		}
		if v.Plan.Databases == nil {
			v.Plan.Databases = []string{}
		}
		if v.Plan.TagCodes == nil {
			v.Plan.TagCodes = []string{}
		}
		if v.Plan.NotifyChannels == nil {
			v.Plan.NotifyChannels = []uint{}
		}
		out = append(out, v)
	}
	return out, nil
}

// SavePlan 新建（id=0）或修改计划。用户看不到的实例（资源组之外）保持原样，不会因为编辑被移除。
func (s *Service) SavePlan(actor Actor, scope Scope, id uint, in Plan) (*Plan, error) {
	if err := s.planReady(); err != nil {
		return nil, err
	}
	if err := in.Normalize(); err != nil {
		return nil, err
	}
	var old *Plan
	if id != 0 {
		var err error
		if old, err = s.repo.GetPlan(id); err != nil {
			return nil, err
		}
		if old == nil {
			return nil, errNotFound("定时巡检计划")
		}
	}
	if same, err := s.repo.PlanByName(in.Name); err != nil {
		return nil, err
	} else if same != nil && same.ID != id {
		return nil, errConflict(fmt.Sprintf("已有同名的计划：%s", in.Name))
	}
	oldIDs := map[uint]bool{}
	if old != nil {
		for _, x := range old.InstanceIDs {
			oldIDs[x] = true
		}
	}
	final := map[uint]bool{}
	for _, iid := range in.InstanceIDs {
		inst, err := s.repo.GetInstance(iid)
		if err != nil || inst == nil {
			return nil, badInput("实例 #%d 不存在", iid)
		}
		if _, ok := s.kindOf(inst.DBType); !ok {
			return nil, badInput("实例 %s 的类型（%s）暂不支持巡检", inst.InstanceName, inst.DBType)
		}
		if !scope.Has(iid) && !oldIDs[iid] {
			return nil, errForbidden(fmt.Sprintf("没有权限选择实例 %s", inst.InstanceName))
		}
		final[iid] = true
	}
	for x := range oldIDs {
		if !scope.Has(x) {
			final[x] = true // 看不到的实例不因编辑而移除
		}
	}
	in.InstanceIDs = in.InstanceIDs[:0]
	for x := range final {
		in.InstanceIDs = append(in.InstanceIDs, x)
	}
	sort.Slice(in.InstanceIDs, func(i, j int) bool { return in.InstanceIDs[i] < in.InstanceIDs[j] })
	if in.NotifyMode != NotifyNone {
		if s.notifier == nil {
			return nil, badInput("通知发送器未启用，不能配置通知")
		}
		known := s.channelMap()
		for _, c := range in.NotifyChannels {
			if _, ok := known[c]; !ok {
				return nil, badInput("通知渠道 #%d 不存在", c)
			}
		}
	}
	now := s.now()
	in.ID = id
	in.NextRunAt = nil
	if in.Enabled {
		next, err := in.NextRun(now)
		if err != nil {
			return nil, badInput("%v", err)
		}
		in.NextRunAt = &next
	}
	in.SysTime = now
	if old != nil {
		in.CreatedBy, in.CreateTime = old.CreatedBy, old.CreateTime
		in.LastRunAt, in.LastRunID = old.LastRunAt, old.LastRunID
	} else {
		in.CreatedBy, in.CreateTime = clip(actor.Username, 30), now
	}
	newID, err := s.repo.SavePlan(&in)
	if err != nil {
		return nil, err
	}
	in.ID = newID
	action := "新建计划"
	if old != nil {
		action = "修改计划"
	}
	s.audit(actor, action, fmt.Sprintf("计划：%s（#%d）；周期：%s；实例 %d 个，标签：%s；并发 %d，间隔 %d 秒；通知：%s；保留 %d 天；%s",
		in.Name, newID, in.Describe(), len(in.InstanceIDs), strings.Join(in.TagCodes, ","), in.Concurrency, in.StaggerSeconds, in.NotifyMode, in.RetentionDays, map[bool]string{true: "启用", false: "停用"}[in.Enabled]))
	return &in, nil
}

// SetPlanEnabled 启用 / 停用计划；启用时重新计算下次运行时间。
func (s *Service) SetPlanEnabled(actor Actor, id uint, enabled bool) error {
	if err := s.planReady(); err != nil {
		return err
	}
	p, err := s.repo.GetPlan(id)
	if err != nil {
		return err
	}
	if p == nil {
		return errNotFound("定时巡检计划")
	}
	var next *time.Time
	if enabled {
		n, err := p.NextRun(s.now())
		if err != nil {
			return badInput("%v", err)
		}
		next = &n
	}
	if err := s.repo.SetPlanEnabled(id, enabled, next); err != nil {
		return err
	}
	s.audit(actor, map[bool]string{true: "启用计划", false: "停用计划"}[enabled], fmt.Sprintf("计划：%s（#%d）", p.Name, id))
	return nil
}

// DeletePlan 删除计划及其运行记录；它产生过的报告保留，之后按“不属于任何计划”的规则处理（见 Cleanup）。
func (s *Service) DeletePlan(actor Actor, id uint) error {
	if err := s.planReady(); err != nil {
		return err
	}
	p, err := s.repo.GetPlan(id)
	if err != nil {
		return err
	}
	if p == nil {
		return errNotFound("定时巡检计划")
	}
	if err := s.repo.DeletePlan(id); err != nil {
		return err
	}
	s.audit(actor, "删除计划", fmt.Sprintf("计划：%s（#%d）", p.Name, id))
	return nil
}

// PreviewSchedule 按表单里的周期返回接下来几次运行时间，并校验表达式。
func (s *Service) PreviewSchedule(in Plan, n int) ([]time.Time, string, error) {
	p := in
	p.Name, p.Concurrency, p.TimeoutMinutes, p.RetentionDays, p.NotifyMode = "预览", 1, 1, 1, NotifyNone
	p.InstanceIDs, p.TagCodes = []uint{1}, nil
	if err := p.Normalize(); err != nil {
		return nil, "", err
	}
	times, err := p.NextRuns(s.now(), n)
	if err != nil {
		return nil, "", badInput("%v", err)
	}
	return times, p.Describe(), nil
}

// ListRuns 返回计划最近的运行记录。
func (s *Service) ListRuns(id uint, limit int) ([]Run, error) {
	if err := s.planReady(); err != nil {
		return nil, err
	}
	p, err := s.repo.GetPlan(id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, errNotFound("定时巡检计划")
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	runs, err := s.repo.ListRuns(id, limit)
	if runs == nil {
		runs = []Run{}
	}
	return runs, err
}

func padHHMM(s string) string {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return s
	}
	if len(h) == 1 {
		h = "0" + h
	}
	if len(m) >= 2 {
		m = m[:2]
	}
	return h + ":" + m
}

// ---- 运行 ----

// RunPlanNow 立即运行一个计划（手动触发，计划是否启用都可以运行）。返回运行记录 ID，实际巡检在后台进行。
func (s *Service) RunPlanNow(actor Actor, id uint) (uint, error) {
	if err := s.planReady(); err != nil {
		return 0, err
	}
	p, err := s.repo.GetPlan(id)
	if err != nil {
		return 0, err
	}
	if p == nil {
		return 0, errNotFound("定时巡检计划")
	}
	runID, err := s.startPlanRun(*p, TriggerManual, actor.Username, s.now())
	if err != nil {
		return 0, err
	}
	s.audit(actor, "立即运行计划", fmt.Sprintf("计划：%s（#%d）；运行记录 #%d", p.Name, id, runID))
	return runID, nil
}

// startPlanRun 建运行记录并在后台执行。同一个计划同一时刻只跑一份（本进程用内存标记，跨进程看运行记录的心跳）。
func (s *Service) startPlanRun(p Plan, trigger, operator string, scheduledAt time.Time) (uint, error) {
	insts, err := s.resolveInstances(&p)
	if err != nil {
		return 0, err
	}
	if len(insts) == 0 {
		return 0, badInput("这个计划当前没有可巡检的实例（实例已删除、类型不支持，或标签没有命中）")
	}
	s.mu.Lock()
	if s.planRuns[p.ID] {
		s.mu.Unlock()
		return 0, errConflict("该计划正在运行")
	}
	s.planRuns[p.ID] = true
	s.mu.Unlock()
	release := func() { s.mu.Lock(); delete(s.planRuns, p.ID); s.mu.Unlock() }

	if r, err := s.repo.ActiveRun(p.ID, s.now().Add(-staleRunAfter)); err != nil {
		release()
		return 0, err
	} else if r != nil {
		release()
		return 0, errConflict("该计划正在运行")
	}
	now := s.now()
	run := &Run{PlanID: p.ID, PlanName: p.Name, Trigger: trigger, Operator: clip(operator, 30), ScheduledAt: scheduledAt, StartedAt: now, Status: RunRunning, Total: len(insts), Heartbeat: now}
	runID, err := s.repo.CreateRun(run)
	if err != nil {
		release()
		return 0, err
	}
	run.ID = runID
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer release()
		s.runPlan(p, run, insts)
	}()
	return runID, nil
}

type planResult struct {
	item  RunItem
	score *int
}

func (s *Service) runPlan(p Plan, run *Run, insts []model.Instance) {
	ctx := s.base
	defer func() {
		if r := recover(); r != nil {
			slog.Error("定时巡检计划运行异常", "plan", p.Name, "panic", r)
			run.Status, run.Note = RunFailed, fmt.Sprintf("运行异常：%v", r)
			now := s.now()
			run.FinishedAt = &now
			_ = s.repo.UpdateRun(run)
		}
	}()
	stop := make(chan struct{})
	hb := make(chan struct{})
	go func() { // 心跳：其他进程据此判断这次运行是否还活着
		defer close(hb)
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_ = s.repo.TouchRun(run.ID, s.now())
			}
		}
	}()

	opt := RunOptions{Timeout: time.Duration(p.TimeoutMinutes) * time.Minute, Databases: p.Databases}
	var mu sync.Mutex
	var results []planResult
	sem := make(chan struct{}, max(p.Concurrency, 1))
	var wg sync.WaitGroup
	for i, inst := range insts {
		if i > 0 && p.StaggerSeconds > 0 && ctx.Err() == nil { // 每启动一个实例间隔一段时间，错开对业务库的压力
			s.sleep(ctx, time.Duration(p.StaggerSeconds)*time.Second)
		}
		if ctx.Err() != nil {
			mu.Lock()
			results = append(results, planResult{item: RunItem{InstanceID: inst.ID, InstanceName: inst.InstanceName, Status: ItemSkipped, Message: "服务退出，未执行"}})
			mu.Unlock()
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(inst model.Instance) {
			defer func() { <-sem; wg.Done() }()
			r := s.inspectInPlan(ctx, p, run, inst, opt)
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}(inst)
	}
	wg.Wait()
	close(stop)
	<-hb

	sort.SliceStable(results, func(i, j int) bool { return results[i].item.InstanceName < results[j].item.InstanceName })
	var failedNames, skippedNames []string
	for _, r := range results {
		it := r.item
		it.RunID = run.ID
		it.Message = clip(it.Message, 250)
		if err := s.repo.AddRunItem(&it); err != nil {
			slog.Error("保存运行明细失败", "run", run.ID, "err", err)
		}
		switch it.Status {
		case ItemOK:
			run.OK++
			run.Alert += it.Alert
			run.Attention += it.Attention
		case ItemSkipped:
			run.Skipped++
			skippedNames = append(skippedNames, fmt.Sprintf("%s %s", it.InstanceName, it.Message))
		default:
			run.Failed++
			failedNames = append(failedNames, fmt.Sprintf("%s %s", it.InstanceName, it.Message))
		}
	}
	var notes []string
	if len(failedNames) > 0 {
		notes = append(notes, fmt.Sprintf("失败：%s", strings.Join(limitStrs(failedNames, 3), "；")))
	}
	if len(skippedNames) > 0 {
		notes = append(notes, fmt.Sprintf("跳过：%s", strings.Join(limitStrs(skippedNames, 3), "；")))
	}
	run.Status = RunFinish
	if run.OK == 0 && run.Failed > 0 {
		run.Status = RunFailed
	}
	fin := s.now()
	run.FinishedAt = &fin
	run.Note = clip(strings.Join(notes, "。"), 1000)
	if msg := s.notifyRun(ctx, p, run, results); msg != "" {
		run.Note = clip(strings.TrimSpace(run.Note+" "+msg), 1000)
	}
	if err := s.repo.UpdateRun(run); err != nil {
		slog.Error("保存运行记录失败", "run", run.ID, "err", err)
	}
	if err := s.repo.MarkPlanRun(p.ID, run.ID, run.StartedAt); err != nil {
		slog.Error("更新计划的上次运行失败", "plan", p.ID, "err", err)
	}
}

func limitStrs(l []string, n int) []string {
	if len(l) <= n {
		return l
	}
	return append(append([]string{}, l[:n]...), fmt.Sprintf("等 %d 个", len(l)))
}

// inspectInPlan 对计划里的一个实例建报告并巡检；按计划的设置处理“已有进行中的巡检”和失败重试。
func (s *Service) inspectInPlan(ctx context.Context, p Plan, run *Run, inst model.Instance, opt RunOptions) planResult {
	item := RunItem{InstanceID: inst.ID, InstanceName: inst.InstanceName}
	kind, _ := s.kindOf(inst.DBType)
	item.Kind = kind
	operator := "系统"
	trigger := TriggerSchedule
	if run.Trigger == TriggerManual {
		operator, trigger = clip(run.Operator, 30), TriggerManual
	}
	stale := opt.Timeout + 10*time.Minute
	deadline := s.now().Add(opt.Timeout)
	attempt := func() (Outcome, string, bool) { // 返回结果、不能开始的原因、是否放弃
		for {
			id, busy, err := s.repo.CreateReport(kind, inst.ID, trigger, operator, 0, s.now(), stale)
			if err != nil {
				return Outcome{}, fmt.Sprintf("创建报告失败：%s", truncate(err.Error(), 200)), true
			}
			if !busy {
				item.ReportID = id
				return s.execute(inst, ReportRef{kind, id}, opt), "", false
			}
			if p.SkipBusy {
				return Outcome{}, "已有进行中的巡检，已跳过", true
			}
			if ctx.Err() != nil || !s.now().Before(deadline) {
				return Outcome{}, "等待已有巡检结束超时，已跳过", true
			}
			s.sleep(ctx, s.busyPoll)
		}
	}
	out, why, giveUp := attempt()
	if giveUp {
		item.Status, item.Message = ItemSkipped, why
		return planResult{item: item}
	}
	if out.Status == StatusFailed && p.RetryOnce && ctx.Err() == nil {
		s.sleep(ctx, s.retryDelay)
		if ctx.Err() == nil {
			if out2, why2, giveUp2 := attempt(); giveUp2 {
				item.Status, item.Message = ItemFailed, truncate(out.Err, 200)+"（重试："+why2+"）"
				return planResult{item: item}
			} else {
				out = out2
				if out.Status == StatusFailed {
					out.Err += "（重试后仍失败）"
				}
			}
		}
	}
	switch out.Status {
	case StatusFinish:
		item.Status, item.Alert, item.Attention = ItemOK, out.Alert, out.Attention
	case StatusCancelled:
		item.Status, item.Message = ItemCancelled, "巡检被取消"
	default:
		item.Status, item.Message = ItemFailed, truncate(out.Err, 200)
	}
	return planResult{item: item, score: out.Score}
}

// notifyRun 按计划的通知设置发汇总消息；返回要记进运行说明的发送问题（成功返回空串）。
func (s *Service) notifyRun(ctx context.Context, p Plan, run *Run, results []planResult) string {
	if p.NotifyMode == NotifyNone || len(p.NotifyChannels) == 0 || s.notifier == nil {
		return ""
	}
	var low []planResult // MySQL 系实例里评分偏低的
	for _, r := range results {
		if r.item.Status == ItemOK && r.score != nil && *r.score < lowScore {
			low = append(low, r)
		}
	}
	problem := run.Alert > 0 || run.Failed > 0 || len(low) > 0
	if p.NotifyMode == NotifyAlert && !problem {
		return ""
	}
	name := notify.EventFinished
	head := "已完成"
	if problem {
		name, head = notify.EventException, "发现问题"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "计划：%s（%s）\n", p.Name, p.Describe())
	fmt.Fprintf(&b, "实例 %d 个：成功 %d / 失败 %d / 跳过 %d\n", run.Total, run.OK, run.Failed, run.Skipped)
	fmt.Fprintf(&b, "告警合计 %d · 关注合计 %d\n", run.Alert, run.Attention)
	ok := make([]planResult, 0, len(results))
	for _, r := range results {
		if r.item.Status == ItemOK && r.item.Alert+r.item.Attention > 0 {
			ok = append(ok, r)
		}
	}
	sort.SliceStable(ok, func(i, j int) bool {
		if ok[i].item.Alert != ok[j].item.Alert {
			return ok[i].item.Alert > ok[j].item.Alert
		}
		return ok[i].item.Attention > ok[j].item.Attention
	})
	if len(ok) > 0 {
		b.WriteString("告警最多的实例：\n")
		for i, r := range ok {
			if i >= 5 {
				break
			}
			fmt.Fprintf(&b, "%d. %s 告警 %d 关注 %d", i+1, r.item.InstanceName, r.item.Alert, r.item.Attention)
			if s.baseURL != "" && r.item.ReportID != 0 {
				fmt.Fprintf(&b, " %s/inspection/%s", s.baseURL, ReportRef{r.item.Kind, r.item.ReportID}.String())
			}
			b.WriteString("\n")
		}
	}
	if len(low) > 0 {
		fmt.Fprintf(&b, "健康评分低于 %d 的实例：", lowScore)
		for i, r := range low {
			if i >= 5 {
				break
			}
			if i > 0 {
				b.WriteString("、")
			}
			fmt.Fprintf(&b, "%s（%d 分）", r.item.InstanceName, *r.score)
		}
		b.WriteString("\n")
	}
	n := 0
	for _, r := range results {
		if r.item.Status == ItemFailed {
			if n == 0 {
				b.WriteString("巡检失败的实例：\n")
			}
			n++
			if n <= 5 {
				fmt.Fprintf(&b, "- %s：%s\n", r.item.InstanceName, r.item.Message)
			}
		}
	}
	if s.baseURL != "" {
		fmt.Fprintf(&b, "查看全部报告：%s/inspection", s.baseURL)
	}
	ev := notify.Event{Kind: "inspect", Name: name, Title: fmt.Sprintf("[dbtool] 定时巡检「%s」%s", p.Name, head), Content: strings.TrimRight(b.String(), "\n"), Applicant: p.CreatedBy}
	if problem {
		ev.Specific = notify.EventInspectAlert // 巡检告警是独立事件；旧渠道的 @ 规则仍按“执行异常”匹配
	}
	sent, err := s.notifier.Send(ctx, p.NotifyChannels, ev)
	switch {
	case err != nil:
		slog.Warn("定时巡检通知发送失败", "plan", p.Name, "err", err)
		return fmt.Sprintf("通知发送失败：%s", truncate(err.Error(), 200))
	case sent == 0:
		return "通知没有发出（所选渠道已删除或已停用）"
	}
	return ""
}

// ---- 调度器 ----

// RunDue 检查到点的计划并启动，返回启动的数量。多个进程同时运行时，next_run_at 的原子更新保证同一次只有一个进程领到。
func (s *Service) RunDue() int {
	if !s.repo.PlanTablesReady() {
		return 0
	}
	now := s.now()
	if n, err := s.repo.FailStaleRuns(now.Add(-staleRunAfter), now); err != nil {
		slog.Error("清理中断的计划运行失败", "err", err)
	} else if n > 0 {
		slog.Warn("发现中断的计划运行，已标记为失败", "count", n)
	}
	due, err := s.repo.DuePlans(now, dueBatch)
	if err != nil {
		slog.Error("读取到点的巡检计划失败", "err", err)
		return 0
	}
	started := 0
	for i := range due {
		p := due[i]
		next, err := p.NextRun(now)
		if err != nil {
			slog.Error("计划的 cron 表达式无法计算下次运行时间，已停用", "plan", p.Name, "err", err)
			_ = s.repo.SetPlanEnabled(p.ID, false, nil)
			continue
		}
		if p.NextRunAt == nil { // 启用了但没有下次运行时间（被手工改过库）：补上，下一轮再按时运行
			_ = s.repo.SetPlanEnabled(p.ID, true, &next)
			continue
		}
		scheduled := *p.NextRunAt
		claimed, err := s.repo.ClaimPlan(p.ID, scheduled, &next, now)
		if err != nil {
			slog.Error("领取计划失败", "plan", p.Name, "err", err)
			continue
		}
		if !claimed {
			continue // 别的进程领走了
		}
		if late := now.Sub(scheduled); late > missGrace {
			s.recordSkippedRun(p, scheduled, fmt.Sprintf("错过计划时间 %s（服务未运行或调度延迟 %s），本次不补跑", scheduled.Format("2006-01-02 15:04"), late.Round(time.Minute)))
			continue
		}
		if _, err := s.startPlanRun(p, TriggerSchedule, "系统", scheduled); err != nil {
			slog.Warn("定时巡检计划没有启动", "plan", p.Name, "err", err)
			s.recordSkippedRun(p, scheduled, fmt.Sprintf("没有启动：%s", err.Error()))
			continue
		}
		started++
		s.audit(Actor{Username: "系统"}, "定时运行计划", fmt.Sprintf("计划：%s（#%d）；计划时间：%s", p.Name, p.ID, scheduled.Format("2006-01-02 15:04")))
	}
	return started
}

func (s *Service) recordSkippedRun(p Plan, scheduled time.Time, note string) {
	now := s.now()
	r := &Run{PlanID: p.ID, PlanName: p.Name, Trigger: TriggerSchedule, Operator: "系统", ScheduledAt: scheduled, StartedAt: now, FinishedAt: &now, Status: RunSkipped, Note: note, Heartbeat: now}
	if _, err := s.repo.CreateRun(r); err != nil {
		slog.Error("记录跳过的运行失败", "plan", p.Name, "err", err)
	}
}

// Worker 是登记给 server.AddWorker 的后台任务：定时检查到点的计划、清理过期报告；ctx 取消后停止调度，并等待在跑的巡检结束。
func (s *Service) Worker(ctx context.Context) {
	t := time.NewTicker(s.tick)
	defer t.Stop()
	var lastCleanup time.Time
	for {
		select {
		case <-ctx.Done():
			s.Shutdown(10 * time.Minute) // 正在跑的巡检不丢：等它们结束，最多 10 分钟
			return
		case <-t.C:
			s.RunDue()
			if now := s.now(); now.Sub(lastCleanup) >= cleanupEvery {
				lastCleanup = now
				s.Cleanup()
			}
		}
	}
}

// Cleanup 清理过期报告：计划产生的报告按计划的保留天数；其余（手动、非计划产生的）只在
// “系统配置”里设置了 *_inspection_retention_days 时才清理，没设置就不删。返回删除的报告数。
func (s *Service) Cleanup() int {
	total := 0
	now := s.now()
	del := func(refs []ReportRef) {
		if len(refs) == 0 {
			return
		}
		deleted, _, err := s.repo.DeleteReports(refs)
		if err != nil {
			slog.Error("清理过期巡检报告失败", "err", err)
			return
		}
		total += len(deleted)
	}
	if s.repo.PlanTablesReady() {
		plans, err := s.repo.ListPlans()
		if err != nil {
			slog.Error("清理报告时读取计划失败", "err", err)
		}
		for _, p := range plans {
			cutoff := now.AddDate(0, 0, -p.RetentionDays)
			for {
				refs, err := s.repo.PlanReportRefsBefore(p.ID, cutoff, cleanupBatch)
				if err != nil {
					slog.Error("清理报告时查询失败", "plan", p.Name, "err", err)
					break
				}
				if len(refs) == 0 {
					break
				}
				before := total
				del(refs)
				if total == before { // 都是进行中的或删除失败，避免死循环
					break
				}
			}
			runCut := now.Add(-max(time.Duration(p.RetentionDays)*24*time.Hour, runKeepMin))
			if err := s.repo.DeleteRunsBefore(p.ID, runCut); err != nil {
				slog.Error("清理运行记录失败", "plan", p.Name, "err", err)
			}
		}
	}
	// 不属于任何计划的报告：保留天数只在“系统配置”里设置了 inspect.*_retention_days 时才清理（缺省永不清理）
	retention := []struct{ Kind, RetentionItem string }{
		{KindMSSQL, settings.KeyInspectMSSQLRetentionDays},
		{KindMySQL, settings.KeyInspectMySQLRetentionDays},
		{KindRedis, settings.KeyInspectRedisRetentionDays},
	}
	for _, d := range retention {
		v, ok := s.repo.ConfigValue(d.RetentionItem)
		if !ok {
			continue
		}
		days, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || days <= 0 {
			continue
		}
		cutoff := now.AddDate(0, 0, -days)
		for {
			refs, err := s.repo.UnlinkedReportRefsBefore(d.Kind, cutoff, cleanupBatch)
			if err != nil {
				slog.Error("清理报告时查询失败", "kind", d.Kind, "err", err)
				break
			}
			if len(refs) == 0 {
				break
			}
			before := total
			del(refs)
			if total == before {
				break
			}
		}
	}
	if total > 0 {
		slog.Info("已清理过期的巡检报告", "count", total)
	}
	return total
}
