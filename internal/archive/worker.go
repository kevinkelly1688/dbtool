// @author: kevinkelly
// @license: Apache Licence
// @file: worker.go
// @time: 2026/10/28

package archive

import (
	"context"
	"log/slog"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/notify"
)

// 通知事件。订阅沿用通知渠道里已有的事件名（开始执行 / 执行完成 / 执行异常 / 待处理 / 审批通过 / 被驳回），
// Event.Kind 为 "archive"；这里的 ev* 是归档内部的细分，只用来决定标题和落到哪个已有事件名。
const (
	evStart     = "archive_start"
	evDone      = "archive_done"
	evInterrupt = "archive_interrupt"
	evFailed    = "archive_failed"
	evVerify    = "archive_verify_failed"
	evStructure = "archive_structure"
	evPaused    = "archive_paused"
	evLost      = "archive_lost" // 执行进程失联 / 被强杀，租约过期后被回收（归入“执行异常”）
)

// notifyName 把归档细分事件映射到通知渠道已有的事件名：
// 开始 → 开始执行；完成、被窗口 / 上限中断（预期内，下次接着跑）→ 执行完成；负载中断、失败、校验失败、结构变更、自动暂停 → 执行异常。
func notifyName(specific, result string) string {
	switch specific {
	case evStart:
		return notify.EventExecuting
	case evDone:
		return notify.EventFinished
	case evInterrupt:
		if result == engine.ResultLoadAbort {
			return notify.EventException
		}
		return notify.EventFinished
	}
	return notify.EventException
}

const (
	scheduleTick = 30 * time.Second
	firstDelay   = 15 * time.Second
)

// Worker 是登记给 server.AddWorker 的后台任务：每 30 秒扫描已启用的任务，到点且在窗口内就启动；同时回收租约过期的运行。
// 表不存在（没执行 180_archive.sql）时什么也不做，执行脚本后下一轮自动开始。
func (s *Service) Worker(ctx context.Context) {
	s.mu.Lock()
	s.base = ctx
	s.mu.Unlock()
	// 启动后立即回收一次上个进程遗留的、租约已过期的运行（不等首轮 Tick）；之后每轮 Tick 仍会回收。表没建时 ExpireStale 什么也不做。
	s.expireStaleAtStart()
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.Tick(ctx)
		timer.Reset(scheduleTick)
	}
}

func (s *Service) expireStaleAtStart() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("数据归档启动回收异常", "panic", r)
		}
	}()
	if n := s.ExpireStale(); n > 0 {
		slog.Warn("数据归档：启动时回收了租约过期的运行（上个进程可能被强杀）", "count", n)
	}
	s.scanSpoolAtStart() // 强杀后遗留在暂存目录的 .tmp：只提示，不自动处理
}

// Tick 做一轮调度（测试里直接调用）。
func (s *Service) Tick(ctx context.Context) {
	if !s.repo.Ready() {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("数据归档调度异常", "panic", r)
		}
	}()
	if n := s.ExpireStale(); n > 0 {
		slog.Warn("数据归档：租约过期的运行已标记为中断", "count", n)
	}
	tasks, _, err := s.repo.ListTasks(TaskFilter{AllGroups: true, Statuses: []string{StEnabled}})
	if err != nil {
		slog.Error("数据归档调度读取任务失败", "err", err)
		return
	}
	now := s.now()
	for i := range tasks {
		if ctx.Err() != nil {
			return
		}
		s.scheduleOne(&tasks[i], now)
	}
}

func (s *Service) scheduleOne(t *Task, now time.Time) {
	c := t.cfg()
	dec, next := decideDue(deref(t.NextRunAt), now, c.Schedule, c.Params.Window)
	switch dec {
	case dueNoSched:
		slog.Warn("数据归档任务的调度无法计算，已忽略", "task", t.ID, "schedule", t.Schedule)
	case dueNo:
		if t.NextRunAt == nil || !t.NextRunAt.Equal(next) {
			_, _ = s.repo.ClaimNextRun(t.ID, t.NextRunAt, ptrTime(next))
		}
	case dueMissed:
		if ok, _ := s.repo.ClaimNextRun(t.ID, t.NextRunAt, ptrTime(next)); ok {
			slog.Info("数据归档：到点时已错过执行窗口，本次不运行", "task", t.ID, "window", windowText(c.Params.Window))
		}
	case dueRun:
		ok, err := s.repo.ClaimNextRun(t.ID, t.NextRunAt, ptrTime(next))
		if err != nil || !ok {
			return // 别的进程已经认领了这次触发
		}
		run, err := s.startRun(t, startOpts{Trigger: TrigSchedule, By: systemCaller})
		if err != nil {
			if se, isSkip := err.(*skipError); isSkip {
				slog.Info("数据归档：本次调度跳过", "task", t.ID, "reason", se.Reason)
				s.skipRun(t, se.Reason)
				return
			}
			slog.Error("数据归档：启动运行失败", "task", t.ID, "err", err)
			return
		}
		s.notifyRun(t, run, evStart, "归档开始", "")
	}
}

func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
