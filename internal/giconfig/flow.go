// @author: kevinkelly
// @license: Apache Licence
// @file: flow.go
// @time: 2026/10/28

package giconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"dbtool/internal/audit"
)

// Step 是重启流程里的一个步骤，页面按顺序展示。
type Step struct {
	Name   string    `json:"name"`
	Status string    `json:"status"` // running / ok / failed / skipped
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// Job 是一次“保存并重启 / 回滚 / 仅重启”流程的进度。流程在后台执行，页面轮询它。
type Job struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"` // save_restart / rollback / restart
	Version    int       `json:"version"`
	Operator   string    `json:"operator"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Done       bool      `json:"done"`
	OK         bool      `json:"ok"`
	Result     string    `json:"result,omitempty"` // success / rolledback / failed
	Message    string    `json:"message,omitempty"`
	Steps      []Step    `json:"steps"`
}

// JobView 是 Job 的只读副本。
type JobView = Job

// JobView 返回最近一次（正在进行或刚结束的）流程。
func (s *Service) JobView() *JobView {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return nil
	}
	c := *s.job
	c.Steps = append([]Step(nil), s.job.Steps...)
	return &c
}

func (s *Service) newJob(kind string, version int, operator string) *Job {
	j := &Job{ID: fmt.Sprintf("%d", s.now().UnixNano()), Kind: kind, Version: version, Operator: operator, StartedAt: s.now(), Steps: []Step{}}
	s.mu.Lock()
	s.job = j
	s.mu.Unlock()
	return j
}

func (s *Service) begin(j *Job, name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	j.Steps = append(j.Steps, Step{Name: name, Status: "running", At: s.now()})
	return len(j.Steps) - 1
}

func (s *Service) endStep(j *Job, idx int, status, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j.Steps[idx].Status = status
	j.Steps[idx].Detail = truncate(detail, 800)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// flowPlan 是后台流程需要的全部上下文。
type flowPlan struct {
	actor     audit.Actor
	deploy    Deploy
	path      string
	conn      ConnInfo
	ver       *Version // 要生效的版本；nil 表示只重启
	recoverTo *Version // 失败后恢复到的版本（上一个生效版本）
	prevDisk  []byte   // 没有可恢复的版本时，恢复到保存前磁盘上的内容
	hadDisk   bool
	waitIdle  bool
	job       *Job
	release   func()
}

// flowTimeout 是整条流程的总时限，防止任何一步卡死后一直占着锁。
func flowTimeout(d Deploy) time.Duration {
	sec := d.WaitIdleMaxSec + 2*(d.RestartTimeoutSec+d.HealthTimeoutSec) + 120
	return time.Duration(sec) * time.Second
}

func (s *Service) startFlow(p *flowPlan) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer p.release()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("goInception 重启流程异常", "panic", r)
				s.conclude(p, time.Time{}, "failed", fmt.Sprintf("内部错误：%v", r))
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), flowTimeout(p.deploy))
		defer cancel()
		s.runFlow(ctx, p)
	}()
}

func (s *Service) runFlow(ctx context.Context, p *flowPlan) {
	start := s.now()
	j, d := p.job, p.deploy

	if p.waitIdle {
		idx := s.begin(j, "等待执行中的任务完成")
		if err := s.waitIdle(ctx, p.conn, d.WaitIdleMaxSec); err != nil {
			s.endStep(j, idx, "failed", err.Error())
			s.setVersion(p, StatusFailed, "等待任务完成超时，未重启，配置文件未改动")
			s.conclude(p, start, "failed", "等待任务完成超时，没有重启")
			return
		}
		s.endStep(j, idx, "ok", "")
	}
	if p.ver != nil {
		idx := s.begin(j, fmt.Sprintf("写入配置文件（版本 #%d）", p.ver.Version))
		if err := s.files.Write(p.path, []byte(p.ver.Content)); err != nil {
			s.endStep(j, idx, "failed", err.Error())
			s.setVersion(p, StatusFailed, fmt.Sprintf("写入配置文件失败：%s", err.Error()))
			s.conclude(p, start, "failed", fmt.Sprintf("写入配置文件失败：%s", err.Error()))
			return
		}
		s.endStep(j, idx, "ok", p.path)
	}

	err := s.restartAndCheck(ctx, p, "重启 goInception")
	if err == nil {
		if p.ver != nil {
			now := s.now()
			s.setVersionAt(p, StatusApplied, &now, "")
		}
		s.conclude(p, start, "success", "重启成功，健康检查通过")
		return
	}
	if p.ver == nil {
		s.conclude(p, start, "failed", err.Error())
		return
	}
	// 失败：尝试恢复到上一个生效版本
	if !d.AutoRollback {
		s.setVersion(p, StatusFailed, fmt.Sprintf("%s；未开启自动回滚，磁盘上仍是新配置，请人工处理", err.Error()))
		s.conclude(p, start, "failed", fmt.Sprintf("%s；未开启自动回滚，磁盘上仍是新配置，请人工处理", err.Error()))
		return
	}
	var restore []byte
	label := ""
	switch {
	case p.recoverTo != nil:
		restore, label = []byte(p.recoverTo.Content), fmt.Sprintf("上一个生效版本 #%d", p.recoverTo.Version)
	case p.hadDisk:
		restore, label = p.prevDisk, "保存前磁盘上的文件"
	default:
		msg := fmt.Sprintf("%s；没有可恢复的旧配置，磁盘上仍是新配置，请人工处理", err.Error())
		s.setVersion(p, StatusFailed, msg)
		s.conclude(p, start, "failed", msg)
		return
	}
	idx := s.begin(j, fmt.Sprintf("自动恢复：%s", label))
	if werr := s.files.Write(p.path, restore); werr != nil {
		s.endStep(j, idx, "failed", werr.Error())
		msg := fmt.Sprintf("%s；恢复旧配置时写文件失败：%v，请人工处理", err.Error(), werr)
		s.setVersion(p, StatusFailed, msg)
		s.conclude(p, start, "failed", msg)
		return
	}
	s.endStep(j, idx, "ok", "")
	if rerr := s.restartAndCheck(ctx, p, "重启 goInception（恢复后）"); rerr != nil {
		msg := fmt.Sprintf("%s；已恢复旧配置但再次重启仍失败：%v，请人工处理", err.Error(), rerr)
		s.setVersion(p, StatusFailed, msg)
		s.conclude(p, start, "failed", msg)
		return
	}
	msg := fmt.Sprintf("%s；已自动恢复到%s并重启成功", err.Error(), label)
	s.setVersion(p, StatusRolledBack, msg)
	s.conclude(p, start, "rolledback", msg)
}

// restartAndCheck 执行重启命令并等待 goInception 恢复可连接。
func (s *Service) restartAndCheck(ctx context.Context, p *flowPlan, name string) error {
	j, d := p.job, p.deploy
	idx := s.begin(j, name)
	rctx, cancel := context.WithTimeout(ctx, time.Duration(d.RestartTimeoutSec)*time.Second)
	out, err := s.restarter.Restart(rctx, d)
	cancel()
	if err != nil {
		detail := err.Error()
		if out != "" {
			detail += "\n" + out
		}
		s.endStep(j, idx, "failed", detail)
		return fmt.Errorf("重启命令失败：%s", truncate(detail, 600))
	}
	s.endStep(j, idx, "ok", out)

	hidx := s.begin(j, "健康检查")
	r, err := s.waitHealthy(ctx, p.conn, d.HealthTimeoutSec)
	if err != nil {
		s.endStep(j, hidx, "failed", err.Error())
		return fmt.Errorf("健康检查失败：重启后 %d 秒内无法连上 goInception（%v）", d.HealthTimeoutSec, err)
	}
	s.endStep(j, hidx, "ok", fmt.Sprintf("连接成功，延迟 %d ms", r.LatencyMS))
	return nil
}

// waitHealthy 反复连接 goInception，直到成功或超时。
func (s *Service) waitHealthy(ctx context.Context, c ConnInfo, timeoutSec int) (PingResult, error) {
	deadline := s.now().Add(time.Duration(timeoutSec) * time.Second)
	var last error
	for {
		pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		r, err := s.prober.Ping(pctx, c)
		cancel()
		if err == nil {
			return r, nil
		}
		last = err
		if ctx.Err() != nil || !s.now().Before(deadline) {
			return PingResult{}, last
		}
		if err := s.sleep(ctx, s.poll); err != nil {
			return PingResult{}, last
		}
	}
}

// waitIdle 等到 goInception 上没有进行中的任务，最多等 maxSec 秒。判断不了任务时不阻塞。
func (s *Service) waitIdle(ctx context.Context, c ConnInfo, maxSec int) error {
	deadline := s.now().Add(time.Duration(maxSec) * time.Second)
	for {
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		t, err := s.prober.Tasks(pctx, c)
		cancel()
		if err != nil || !t.Known || t.Running == 0 {
			return nil
		}
		if !s.now().Before(deadline) {
			return fmt.Errorf("等待 %d 秒后仍有 %d 个任务在执行", maxSec, t.Running)
		}
		if err := s.sleep(ctx, s.poll); err != nil {
			return err
		}
	}
}

func (s *Service) setVersion(p *flowPlan, status, msg string) { s.setVersionAt(p, status, nil, msg) }

func (s *Service) setVersionAt(p *flowPlan, status string, at *time.Time, msg string) {
	if p.ver == nil {
		return
	}
	if _, err := s.repo.UpdateVersionResult(p.ver.ID, []string{StatusDraft}, status, at, truncate(msg, 3000)); err != nil {
		slog.Error("更新 goInception 配置版本状态失败", "version", p.ver.Version, "err", err)
	}
}

// conclude 收尾：登记结果、写审计、记录最近一次重启。start 为零值表示没有走完整流程。
func (s *Service) conclude(p *flowPlan, start time.Time, result, msg string) {
	now := s.now()
	ok := result == "success"
	ver := 0
	if p.ver != nil {
		ver = p.ver.Version
	}
	rec := RestartRecord{Version: ver, OK: ok, Result: result, Message: truncate(msg, 800), Operator: p.actor.Username, At: now}
	if !start.IsZero() {
		rec.DurationMS = now.Sub(start).Milliseconds()
	}
	if b, err := json.Marshal(rec); err == nil {
		if err := s.repo.SaveState("last_restart", string(b)); err != nil {
			slog.Error("保存最近一次重启结果失败", "err", err)
		}
	}
	action := actRestart
	if p.job.Kind == "rollback" {
		action = actRollback
	}
	label := map[string]string{"success": "成功", "rolledback": "失败，已自动恢复旧配置", "failed": "失败"}[result]        // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	s.audit(p.actor, action, "版本 #%d，重启方式 %s：%s。%s", ver, restartDesc(p.deploy), label, truncate(msg, 500)) // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	if ok && p.ver != nil {
		if err := s.repo.PruneVersions(keepVersions); err != nil {
			slog.Error("清理 goInception 配置历史版本失败", "err", err)
		}
	}
	// 最后才对外宣布“流程结束”：页面轮询到 done 后立刻刷新概览时，最近一次重启结果、审计、历史清理都已经落库
	// （真库冒烟里发现：先置 done 再写库，刷新得到的 last_restart 可能还是上一次的）。
	s.mu.Lock()
	p.job.Done, p.job.OK, p.job.Result, p.job.Message, p.job.FinishedAt = true, ok, result, msg, now
	s.mu.Unlock()
}
