// @author: kevinkelly
// @license: Apache Licence
// @file: jobs.go
// @time: 2026/10/28

package perf

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"dbtool/internal/settings"
)

// 后台任务：采集建单、SLA 巡检、每日验证、周报。
//
// 开关与多实例：
//   - 去重键：工单靠 dbt_perf_issue 的唯一索引 (instance_id, db_name, checksum) 去重，并发采集也不会建出重复工单；
//   - 但采集是“累加当天指标”，同一窗口被重复采集会重复计数，所以本进程的任务默认全部关闭，
//     由系统配置项 perf.jobs（逗号分隔：collect,sla,verify,report）逐项开启；
//   - 同一个 dbtool 部署多个实例时：采集用库里的抢占（last_status=RUNNING@…），SLA/验证用工单乐观锁，每日验证与周报用 dbt_perf_job_run 的唯一键。

// 任务名。
const (
	JobCollect = "collect"
	JobSLA     = "sla"
	JobVerify  = "verify"
	JobReport  = "report"
)

// AllJobs 是全部后台任务及说明。
var AllJobs = []Option{
	{JobCollect, "采集建单（每分钟检查，到期的配置按窗口采集）"},
	{JobSLA, "SLA 巡检（每 30 分钟：即将超时提醒、L1/L2/L3 升级、重复催办）"},
	{JobVerify, "每日验证（每天 10:05 后：修复效果验证、长期未复现自动关闭）"},
	{JobReport, "周报（每周一 09:35 后：管理群与各团队群）"},
}

// JobsStatus 是后台任务的开关状态（页面上展示）。
type JobsStatus struct {
	Enabled []string `json:"enabled"` // 本进程已开启的任务
	All     []Option `json:"all"`
	Item    string   `json:"config_item"`
	JobRun  bool     `json:"job_run_table"` // dbt_perf_job_run 是否已创建
}

func (s *Service) goJobs() map[string]bool {
	out := map[string]bool{}
	for _, j := range splitComma(s.cfgStr(settings.KeyPerfJobs, "")) {
		out[j] = true
	}
	return out
}

func (s *Service) JobsStatus() JobsStatus {
	st := JobsStatus{Enabled: []string{}, All: AllJobs, Item: settings.KeyPerfJobs, JobRun: s.repo.JobRunsReady()}
	on := s.goJobs()
	for _, j := range AllJobs {
		if on[j.Value] {
			st.Enabled = append(st.Enabled, j.Value)
		}
	}
	return st
}

// worker 是后台循环的状态。
type worker struct {
	s       *Service
	lastSLA time.Time
	// 没有 dbt_perf_job_run 表时，每日任务用进程内记忆去重
	memKeys map[string]bool
	warned  map[string]bool
}

const (
	slaEvery      = 30 * time.Minute
	verifyHour    = 10
	verifyMinute  = 5
	reportWeekday = time.Monday
	reportHour    = 9
	reportMinute  = 35
)

// RunWorker 在 ctx 取消前每个 tick 检查一次要做的任务。登记方式：server.AddWorker(func(ctx){ svc.RunWorker(ctx, time.Minute) })。
func (s *Service) RunWorker(ctx context.Context, tick time.Duration) {
	w := &worker{s: s, memKeys: map[string]bool{}, warned: map[string]bool{}}
	t := time.NewTicker(tick)
	defer t.Stop()
	// 启动后先等一个 tick，让服务完成初始化
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *worker) tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("性能工单后台任务异常", "panic", r)
		}
	}()
	s := w.s
	on := s.goJobs()
	now := s.now()
	if on[JobCollect] {
		if res, err := s.CollectDue(ctx); err != nil {
			slog.Error("性能工单采集失败", "err", err)
		} else if len(res) > 0 {
			slog.Info("性能工单采集", "result", res)
		}
	}
	if on[JobSLA] && now.Sub(w.lastSLA) >= slaEvery {
		w.lastSLA = now
		if res, err := s.RunSLA(); err != nil {
			slog.Error("性能工单 SLA 巡检失败", "err", err)
		} else {
			slog.Info("性能工单 SLA 巡检", "checked", res.Checked, "actions", res.Actions)
		}
	}
	if on[JobVerify] && (now.Hour() > verifyHour || (now.Hour() == verifyHour && now.Minute() >= verifyMinute)) {
		key := "verify:" + now.Format(dateLayout)
		if w.claim(JobVerify, key, false) {
			res, err := s.RunVerify()
			w.finish(JobVerify, key, fmt.Sprintf("%+v", res), err)
		}
	}
	if on[JobReport] && now.Weekday() == reportWeekday && (now.Hour() > reportHour || (now.Hour() == reportHour && now.Minute() >= reportMinute)) {
		y, wk := now.ISOWeek()
		key := fmt.Sprintf("weekly:%d-W%02d", y, wk)
		if w.claim(JobReport, key, true) { // 周报会发消息，没有运行记录表时宁可不发，也不能重启一次发一次
			_, err := s.WeeklyReport(true)
			w.finish(JobReport, key, "sent", err)
		}
	}
}

// claim 抢占一次按天/按周的任务。需要持久去重（requireTable）而表还没建时返回 false 并提示一次。
func (w *worker) claim(job, key string, requireTable bool) bool {
	s := w.s
	if s.repo.JobRunsReady() {
		ok, err := s.repo.ClaimJobRun(job, key, s.now())
		if err != nil {
			slog.Error("抢占后台任务失败", "job", job, "err", err)
			return false
		}
		return ok
	}
	if requireTable {
		if !w.warned[job] {
			w.warned[job] = true
			slog.Warn("dbt_perf_job_run 表不存在，周报任务不会执行；请先执行 deploy/sql/017_perf.sql")
		}
		return false
	}
	if w.memKeys[key] {
		return false
	}
	w.memKeys[key] = true
	return true
}

func (w *worker) finish(job, key, result string, err error) {
	if err != nil {
		slog.Error("性能工单后台任务失败", "job", job, "err", err)
		result = "ERROR " + clipRunes(err.Error(), 400)
	} else {
		slog.Info("性能工单后台任务完成", "job", job, "key", key, "result", result)
	}
	if w.s.repo.JobRunsReady() {
		if e := w.s.repo.FinishJobRun(job, key, w.s.now(), clipRunes(result, 500)); e != nil {
			slog.Error("写后台任务结果失败", "job", job, "err", e)
		}
	}
}
