// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

// Package retention 是“数据增长清理”：给不会自己清理、会无限增长的数据提供可配置、默认全部关闭的清理任务。
//
// 清理目标：dbt_audit_log（连同 dbt_audit_ext）、dbt_query_log、dbt_slow_logs、dbt_query_event、dbt_query_reject、
// 归档运行记录（dbt_archive_run 及其 event / batch_stat，只清终态）、数据导出文件（保留工单记录）、
// 归档文件备份（按备份存储的“保留天数”，只删运行清单里记录的对象）。dbt_sql_workflow 相关表不碰。
//
// 约束：
//   - 总开关 retention.enabled 缺省关闭；每个目标的 retention.<target>_days 缺省 0 = 不清理；
//   - 分批删除（每批 ≤ 5000 行，批间休眠）、可限定时间窗口、每轮 GET_LOCK 保证单实例执行；
//   - 每轮写一条审计与日志汇总；某目标出错只停止该目标，继续下一个目标；
//   - 表 / 列不存在时跳过并记日志；所有 SQL 参数化；不对任何表做 ALTER；
//   - 预览接口 GET /api/v1/retention/preview（仅超级用户）只统计、不删除。
package retention

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/model"
)

const (
	lockName      = "dbt_retention"
	firstRunDelay = 5 * time.Minute
	pageSize      = 200 // 文件类目标每页取多少条
	previewCap    = 5000

	// 审计动作名与操作人（后台任务没有登录用户）。
	auditAction   = "数据清理"
	systemUser    = "system"
	systemDisplay = "数据清理定时任务"
)

// 目标状态。
const (
	StatusOK      = "ok"      // 做完（可能一行都没删）
	StatusPartial = "partial" // 中途停止：窗口结束 / 达到每轮上限 / 超时 / 被取消
	StatusSkipped = "skipped" // 未配置或缺表缺列等，没有执行
	StatusError   = "error"
)

// TargetResult 是一个目标在一轮里的结果。
type TargetResult struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Days    int    `json:"days"`
	Cutoff  string `json:"cutoff,omitempty"`
	Deleted int64  `json:"deleted"` // 行数 / 文件数 / 备份对象数
	Bytes   int64  `json:"bytes,omitempty"`
	Status  string `json:"status"`
	Note    string `json:"note,omitempty"`
	Err     string `json:"error,omitempty"`
}

// RoundResult 是一轮清理的结果。
type RoundResult struct {
	Trigger  string         `json:"trigger"`
	Started  time.Time      `json:"started_at"`
	Finished time.Time      `json:"finished_at"`
	Ran      bool           `json:"ran"`              // false = 没有执行（未启用 / 不在窗口 / 别的进程在跑）
	Reason   string         `json:"reason,omitempty"` // Ran=false 时的原因
	Targets  []TargetResult `json:"targets,omitempty"`
	Deleted  int64          `json:"deleted"`
	Problems []string       `json:"problems,omitempty"`
	Stopped  string         `json:"stopped,omitempty"` // 一轮提前结束的原因（窗口结束 / 超时 / 服务停止）
}

// Failed 是否有目标出错。
func (r RoundResult) Failed() bool {
	for _, t := range r.Targets {
		if t.Status == StatusError {
			return true
		}
	}
	return false
}

// Service 是清理服务。
type Service struct {
	repo       Repo
	exportBase string // EXPORT_BASE_DIR：导出目录为相对路径时的基准

	now        func() time.Time
	sleep      func(ctx context.Context, d time.Duration)
	openStore  func(engine.StoreConfig) (engine.FileStore, error)
	firstDelay time.Duration

	mu sync.Mutex // 本进程内同一时刻只跑一轮
}

// NewService 创建服务。exportBase 是 EXPORT_BASE_DIR。
func NewService(repo Repo, exportBase string) *Service {
	return &Service{repo: repo, exportBase: exportBase, now: time.Now, sleep: sleepCtx,
		openStore: engine.NewFileStore, firstDelay: firstRunDelay}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Worker 是登记给 server.AddWorker 的后台任务：启动 5 分钟后第一次检查，之后每隔 retention.interval_minutes 一轮；
// 设置每轮重新读取（改完不用重启）。总开关关闭时每轮都是空操作。
func (s *Service) Worker(ctx context.Context) {
	timer := time.NewTimer(s.firstDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("数据清理任务异常（已恢复，下一轮继续）", "panic", r)
				}
			}()
			s.RunOnce(ctx, "schedule")
		}()
		timer.Reset(loadSettings(s.repo).Interval)
	}
}

// RunOnce 跑一轮清理。trigger 为 schedule（受时间窗口约束）或 manual（不受窗口约束，但仍要总开关打开）。
func (s *Service) RunOnce(ctx context.Context, trigger string) RoundResult {
	set := loadSettings(s.repo)
	rr := RoundResult{Trigger: trigger, Started: s.now(), Problems: set.Problems}
	finish := func(reason string) RoundResult {
		rr.Reason, rr.Finished = reason, s.now()
		return rr
	}
	if !set.Enabled {
		return finish("未启用（系统设置 retention.enabled 不是 true）")
	}
	if trigger == "schedule" && !set.windowOpen(rr.Started) {
		return finish(fmt.Sprintf("当前不在允许的时间窗口内（retention.window=%s）", set.windowText()))
	}
	if !s.mu.TryLock() {
		return finish("本进程已有一轮清理在执行")
	}
	defer s.mu.Unlock()
	release, ok, err := s.repo.TryLock(ctx, lockName)
	if err != nil {
		slog.Warn("数据清理取锁失败", "err", err)
		return finish(fmt.Sprintf("取锁失败：%s", err.Error()))
	}
	if !ok {
		return finish("别的进程正在执行清理")
	}
	defer release()

	rr.Ran = true
	for _, p := range set.Problems {
		slog.Warn("数据清理配置问题", "problem", p)
	}
	deadline := rr.Started.Add(set.MaxDuration)
	halt := func() string {
		switch {
		case ctx.Err() != nil:
			return "服务停止"
		case !s.now().Before(deadline):
			return fmt.Sprintf("已达到一轮最长执行时间（%d 分钟）", int(set.MaxDuration/time.Minute))
		case trigger == "schedule" && !set.windowOpen(s.now()):
			return "时间窗口结束"
		}
		return ""
	}
	for _, t := range allTargets() {
		if reason := halt(); reason != "" {
			rr.Stopped = reason // 剩下的目标这一轮不做了，下一轮继续
			break
		}
		var res TargetResult
		switch t.Kind {
		case KindTable:
			res = s.runTable(ctx, t, set, halt)
		case KindExports:
			res = s.runExports(ctx, t, set, halt)
		case KindBackups:
			res = s.runBackups(ctx, t, set, halt)
		}
		if res.Status == StatusSkipped && res.Note == "" {
			continue // 没配置的目标不出现在汇总里
		}
		rr.Targets = append(rr.Targets, res)
		rr.Deleted += res.Deleted
		switch res.Status {
		case StatusError:
			slog.Error("数据清理目标出错（已停止该目标，继续下一个）", "target", res.Key, "deleted", res.Deleted, "err", res.Err)
		case StatusSkipped:
			slog.Warn("数据清理目标已跳过", "target", res.Key, "note", res.Note)
		default:
			slog.Info("数据清理目标完成", "target", res.Key, "status", res.Status, "deleted", res.Deleted, "days", res.Days, "note", res.Note)
		}
	}
	rr.Finished = s.now()
	s.record(&rr)
	return rr
}

// record 写汇总：服务日志一条、审计一条（有删除或出错才写，避免每小时一条空记录）、dbt_retention_run 一行。
func (s *Service) record(rr *RoundResult) {
	text := summaryText(rr)
	slog.Info("数据清理一轮结束", "trigger", rr.Trigger, "deleted", rr.Deleted, "failed", rr.Failed(), "summary", text)
	if rr.Deleted == 0 && !rr.Failed() {
		return
	}
	status := "ok"
	if rr.Failed() {
		status = "partial"
		if rr.Deleted == 0 {
			status = "failed"
		}
	}
	extra := text
	if rr.Failed() {
		extra = fmt.Sprintf("失败项：%s。%s", failedLabels(rr), text)
	}
	e := &model.AuditEntry{UserName: systemUser, UserDisplay: systemDisplay, Action: auditAction, ExtraInfo: extra}
	if err := s.repo.InsertAudit(e); err != nil {
		slog.Error("写数据清理审计失败", "err", err)
	}
	rec := &RoundRecord{StartedAt: rr.Started, FinishedAt: rr.Finished, Trigger: rr.Trigger, Status: status, Deleted: rr.Deleted, Summary: text}
	if err := s.repo.SaveRound(rec); err != nil {
		slog.Warn("保存数据清理汇总失败", "err", err)
	}
}

func failedLabels(rr *RoundResult) string {
	var out []string
	for _, t := range rr.Targets {
		if t.Status == StatusError {
			out = append(out, t.Key)
		}
	}
	return strings.Join(out, "、")
}

func summaryText(rr *RoundResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s 触发，共删除 %d", rr.Trigger, rr.Deleted)
	for _, t := range rr.Targets {
		fmt.Fprintf(&b, "；%s：", t.Key)
		switch t.Status {
		case StatusSkipped:
			fmt.Fprintf(&b, "跳过（%s）", t.Note)
			continue
		case StatusError:
			fmt.Fprintf(&b, "出错，已删除 %d（%s）", t.Deleted, t.Err)
			continue
		}
		fmt.Fprintf(&b, "删除 %d", t.Deleted)
		if t.Cutoff != "" {
			fmt.Fprintf(&b, "（早于 %s，保留 %d 天）", t.Cutoff, t.Days)
		}
		if t.Status == StatusPartial {
			fmt.Fprintf(&b, "，未做完：%s", t.Note)
		} else if t.Note != "" {
			fmt.Fprintf(&b, "，%s", t.Note)
		}
	}
	if rr.Stopped != "" {
		fmt.Fprintf(&b, "；本轮提前结束：%s", rr.Stopped)
	}
	return b.String()
}

const timeLayout = "2006-01-02 15:04:05"

func (s *Service) cutoff(days int) time.Time { return s.now().AddDate(0, 0, -days) }

// runTable 清理一个表目标。
func (s *Service) runTable(ctx context.Context, t Target, set Settings, halt func() string) TargetResult {
	res := TargetResult{Key: t.Key, Label: t.Label, Status: StatusSkipped}
	days := set.Days[t.Key]
	if days <= 0 {
		return res // 未配置
	}
	res.Days = days
	t, note, err := s.checkTable(t)
	if err != nil {
		res.Status, res.Err = StatusError, fmt.Sprintf("检查表结构失败：%s", err.Error())
		return res
	}
	if note != "" {
		res.Note = note
		return res
	}
	cut := s.cutoff(days)
	res.Cutoff = cut.Format(timeLayout)
	res.Status = StatusOK
	for {
		if reason := halt(); reason != "" {
			res.Status, res.Note = StatusPartial, reason
			return res
		}
		remain := set.MaxRows - res.Deleted
		if remain <= 0 {
			res.Status, res.Note = StatusPartial, fmt.Sprintf("已达到每轮上限 %d 行，剩余部分下一轮继续", set.MaxRows)
			return res
		}
		limit := set.BatchSize
		if int64(limit) > remain {
			limit = int(remain)
		}
		sel, del, err := s.repo.DeleteBatch(ctx, t, cut, limit)
		res.Deleted += del
		if err != nil {
			res.Status, res.Err = StatusError, err.Error()
			return res
		}
		if sel > 0 && del == 0 {
			res.Status, res.Err = StatusError, "选中了待删除的行但一行都没删掉（并发修改？），已停止该目标"
			return res
		}
		if sel < limit {
			return res
		}
		s.sleep(ctx, set.BatchSleep)
	}
}

// checkTable 确认主表（含所需列）、依赖表、子表是否存在；返回去掉缺失子表后的目标。note 非空表示应跳过。
func (s *Service) checkTable(t Target) (Target, string, error) {
	cols := append([]string{t.IDCol, t.TimeCol}, t.Need...)
	st, err := s.repo.TableState(t.Table, cols...)
	if err != nil {
		return t, "", err
	}
	if !st.Exists {
		return t, fmt.Sprintf("表 %s 不存在，已跳过", t.Table), nil
	}
	if len(st.Missing) > 0 {
		return t, fmt.Sprintf("表 %s 缺少列 %s，已跳过", t.Table, strings.Join(st.Missing, "、")), nil
	}
	for _, d := range t.Deps {
		ds, err := s.repo.TableState(d)
		if err != nil {
			return t, "", err
		}
		if !ds.Exists {
			return t, fmt.Sprintf("依赖的表 %s 不存在，已跳过", d), nil
		}
	}
	var kids []Child
	for _, c := range t.Children {
		cs, err := s.repo.TableState(c.Table, c.FKCol)
		if err != nil {
			return t, "", err
		}
		if cs.OK() {
			kids = append(kids, c)
		} else {
			slog.Info("数据清理：子表不存在或缺列，不随父表清理", "parent", t.Table, "child", c.Table)
		}
	}
	t.Children = kids
	return t, "", nil
}
