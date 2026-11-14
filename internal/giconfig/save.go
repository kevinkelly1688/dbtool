// @author: kevinkelly
// @license: Apache Licence
// @file: save.go
// @time: 2026/10/28

package giconfig

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dbtool/internal/audit"
)

// SaveRequest 是“保存 / 保存并重启”的请求。
type SaveRequest struct {
	Content     string            `json:"content"`
	Secrets     map[string]string `json:"secrets"` // 页面上新填的密码类参数："段.参数名" → 新值
	BaseVersion int               `json:"base_version"`
	Summary     string            `json:"summary"`
	Restart     bool              `json:"restart"`
	Force       bool              `json:"force"`      // 有任务在执行时仍立即重启
	WaitTasks   bool              `json:"wait_tasks"` // 有任务在执行时，等它们完成后再重启
	AckWarnings bool              `json:"ack_warnings"`
	AckDrift    bool              `json:"ack_drift"` // 确认覆盖“被人手工改过”的磁盘文件
}

// SaveResult 是保存结果。Restarting 为真时重启流程已在后台开始，页面轮询 /restart/status。
type SaveResult struct {
	Version    int     `json:"version"`
	Status     string  `json:"status"`
	Restarting bool    `json:"restarting"`
	Warnings   []Issue `json:"warnings,omitempty"`
	Message    string  `json:"message"`
}

// lock 取得“同一时刻只有一个保存/重启流程”的锁：进程内互斥 + 数据库行锁（多实例部署时也成立）。
func (s *Service) lock(actor audit.Actor, d Deploy, restart bool) (func(), error) {
	if !s.opMu.TryLock() {
		return nil, conflict("busy_op", "已有保存或重启流程在进行，请稍后再试")
	}
	ttl := 3 * time.Minute
	if restart {
		ttl = flowTimeout(d) + time.Minute
	}
	token, ok, holder, err := s.repo.AcquireLock(actor.Username, ttl)
	if err != nil {
		s.opMu.Unlock()
		return nil, newErr(500, "lock_failed", "获取操作锁失败：%v", err)
	}
	if !ok {
		s.opMu.Unlock()
		if holder == "" {
			holder = "其他人"
		}
		return nil, conflict("busy_op", "%s 正在保存或重启 goInception，请稍后再试", holder)
	}
	return func() {
		_ = s.repo.ReleaseLock(token)
		s.opMu.Unlock()
	}, nil
}

// checkBusy 重启前检查 goInception 上有没有进行中的任务。返回是否需要先等待。
func (s *Service) checkBusy(ctx context.Context, c ConnInfo, force, wait bool) (bool, error) {
	if force {
		return false, nil
	}
	t, err := s.prober.Tasks(ctx, c)
	if err != nil || !t.Known || t.Running == 0 {
		return false, nil // 连不上或判断不了：不阻塞（连不上说明本来就没在跑任务）
	}
	if wait {
		return true, nil
	}
	e := conflict("busy", "goInception 上有 %d 个任务正在执行，立即重启会中断它们；请选择“等待任务完成后再重启”或确认强制重启", t.Running)
	e.Data = t
	return false, e
}

type commitInput struct {
	content     string
	summary     string
	kind        string // save_restart / rollback
	restart     bool
	force, wait bool
	ackDrift    bool
	baseVersion int
}

// Save 校验并保存新版本；Restart 为真时继续重启 goInception。
func (s *Service) Save(ctx context.Context, actor audit.Actor, req SaveRequest) (*SaveResult, error) {
	if err := s.needTables(); err != nil {
		return nil, err
	}
	path, err := s.needPath()
	if err != nil {
		return nil, err
	}
	d, err := s.loadDeploy()
	if err != nil {
		return nil, newErr(500, "load_failed", "读取部署设置失败：%v", err)
	}
	if req.Restart { // 容器名没有设置时不能重启；“仅保存”不受影响
		if err := needRestartable(d); err != nil {
			return nil, err
		}
	}
	release, err := s.lock(actor, d, req.Restart)
	if err != nil {
		return nil, err
	}
	handed := false
	defer func() {
		if !handed {
			release()
		}
	}()
	sn, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if sn.latestNum() != req.BaseVersion {
		return nil, conflict("stale", "配置已经被其他人修改（最新版本是 #%d），请刷新页面后在最新内容上重新修改", sn.latestNum())
	}
	content, err := s.mergeSecrets(req.Content, req.Secrets, sn)
	if err != nil {
		return nil, err
	}
	rep := Validate(content)
	if !rep.OK() {
		e := badReq("配置文件校验未通过：%s", firstError(rep))
		e.Code, e.Data = "invalid_config", rep
		return nil, e
	}
	if rep.Warnings > 0 && !req.AckWarnings {
		e := newErr(422, "warnings", "配置文件有 %d 条警告，确认无误后再提交", rep.Warnings)
		e.Data = rep
		return nil, e
	}
	res, err := s.commit(ctx, actor, path, d, sn, commitInput{content: content, summary: strings.TrimSpace(req.Summary), kind: "save_restart",
		restart: req.Restart, force: req.Force, wait: req.WaitTasks, ackDrift: req.AckDrift, baseVersion: req.BaseVersion}, release, &handed)
	if res != nil {
		res.Warnings = nonErrors(rep)
	}
	return res, err
}

func firstError(r Report) string {
	for _, i := range r.Issues {
		if i.Level == LevelError {
			return i.Message
		}
	}
	return ""
}

func nonErrors(r Report) []Issue {
	var out []Issue
	for _, i := range r.Issues {
		if i.Level == LevelWarn {
			out = append(out, i)
		}
	}
	return out
}

// commit 是保存与回滚共用的后半段：写版本记录、写文件，需要时启动后台重启流程。调用方已持有锁。
func (s *Service) commit(ctx context.Context, actor audit.Actor, path string, d Deploy, sn *snapshot, in commitInput, release func(), handed *bool) (*SaveResult, error) {
	dr := sn.drift()
	if dr.Kind == "error" {
		return nil, newErr(500, "file_error", "%s", dr.Message)
	}
	if dr.Kind == "modified" && !in.ackDrift {
		return nil, conflict("drift", "%s。保存会覆盖它（旧内容会备份为 %s）；如果要保留手工修改，请先在“部署与重启”里把磁盘文件导入为新版本", dr.Message, path+BackupSuffix)
	}
	newSum := sha256hex([]byte(in.content))
	if exp := sn.expected(); exp != nil && exp.ContentSHA256 == newSum && dr.Kind != "modified" && dr.Kind != "missing" {
		return nil, badReq("配置内容没有变化")
	} else if exp == nil && sn.diskInfo.Exists && sha256hex(sn.disk) == newSum {
		return nil, badReq("配置内容与磁盘上的文件相同，没有变化")
	}
	summary := in.summary
	if auto := Summarize(sn.baseline(), in.content); summary == "" {
		summary = auto
	} else if strings.HasPrefix(summary, "回滚") { // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
		summary = summary + "：" + auto
	} else {
		summary = summary + "（" + auto + "）"
	}
	if r := []rune(summary); len(r) > 250 {
		summary = string(r[:250]) + "…"
	}

	restart := in.restart
	var conn ConnInfo
	waitIdle := false
	if restart {
		if conn = s.loadConn("main"); conn.Host == "" {
			return nil, badReq("还没有配置 goInception 的连接信息，重启后无法做健康检查；请先在“连接信息”里填写并保存")
		}
		var err error
		if waitIdle, err = s.checkBusy(ctx, conn, in.force, in.wait); err != nil {
			return nil, err
		}
	}

	v := &Version{Content: in.content, ContentSHA256: newSum, ChangeSummary: summary, Operator: actor.Username, Status: StatusDraft, CreatedAt: s.now()}
	if err := s.repo.InsertVersion(v); err != nil {
		return nil, newErr(500, "save_failed", "保存版本失败：%v", err)
	}
	action := actSave
	if in.kind == "rollback" {
		action = actRollback
	}
	s.audit(actor, action, "版本 #%d：%s；重启方式：%s；%s", v.Version, summary, restartDesc(d), map[bool]string{true: "随后重启", false: "仅保存"}[in.restart]) // i18n-ignore 审计文本 / 存库的版本摘要，不翻译

	if !restart {
		if err := s.files.Write(path, []byte(in.content)); err != nil {
			_, _ = s.repo.UpdateVersionResult(v.ID, []string{StatusDraft}, StatusFailed, nil, fmt.Sprintf("写入配置文件失败：%s", err.Error()))
			return nil, newErr(500, "write_failed", "写入配置文件失败：%v", err)
		}
		_ = s.repo.PruneVersions(keepVersions)
		msg := fmt.Sprintf("已写入版本 #%d，未重启；goInception 要重启后才会使用新配置", v.Version)
		return &SaveResult{Version: v.Version, Status: StatusDraft, Message: msg}, nil
	}

	plan := &flowPlan{actor: actor, deploy: d, path: path, conn: conn, ver: v, waitIdle: waitIdle, release: release,
		prevDisk: sn.disk, hadDisk: sn.diskInfo.Exists}
	if sn.effective != nil && sn.effective.Version != v.Version {
		plan.recoverTo = sn.effective
	}
	plan.job = s.newJob(in.kind, v.Version, actor.Username)
	*handed = true
	s.startFlow(plan)
	return &SaveResult{Version: v.Version, Status: StatusDraft, Restarting: true, Message: fmt.Sprintf("版本 #%d 已保存，正在重启 goInception", v.Version)}, nil
}

// RollbackRequest 是回滚请求：回滚会以旧版本的内容生成一个新版本，并走“写文件 → 重启 → 健康检查”。
type RollbackRequest struct {
	BaseVersion int  `json:"base_version"`
	Force       bool `json:"force"`
	WaitTasks   bool `json:"wait_tasks"`
	AckDrift    bool `json:"ack_drift"`
}

// Rollback 回滚到历史版本 num。
func (s *Service) Rollback(ctx context.Context, actor audit.Actor, num int, req RollbackRequest) (*SaveResult, error) {
	if err := s.needTables(); err != nil {
		return nil, err
	}
	path, err := s.needPath()
	if err != nil {
		return nil, err
	}
	d, err := s.loadDeploy()
	if err != nil {
		return nil, newErr(500, "load_failed", "读取部署设置失败：%v", err)
	}
	if err := needRestartable(d); err != nil {
		return nil, err
	}
	release, err := s.lock(actor, d, true)
	if err != nil {
		return nil, err
	}
	handed := false
	defer func() {
		if !handed {
			release()
		}
	}()
	target, err := s.repo.GetVersion(num)
	if err != nil {
		return nil, newErr(500, "load_failed", "读取版本失败：%v", err)
	}
	if target == nil {
		return nil, newErr(404, "not_found", "版本不存在（可能已超过保留数量被清理）")
	}
	if target.Status == StatusFailed || target.Status == StatusRolledBack {
		return nil, badReq("版本 #%d 重启失败过，不能回滚到它", num)
	}
	sn, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if sn.latestNum() != req.BaseVersion {
		return nil, conflict("stale", "配置已经被其他人修改（最新版本是 #%d），请刷新页面后重试", sn.latestNum())
	}
	if rep := Validate(target.Content); !rep.OK() {
		return nil, badReq("版本 #%d 的内容校验不通过，不能回滚：%s", num, firstError(rep))
	}
	summary := fmt.Sprintf("回滚到 #%d", num) // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	return s.commit(ctx, actor, path, d, sn, commitInput{content: target.Content, summary: summary, kind: "rollback", restart: true,
		force: req.Force, wait: req.WaitTasks, ackDrift: req.AckDrift, baseVersion: req.BaseVersion}, release, &handed)
}

// RestartRequest 是“仅重启”的请求。
type RestartRequest struct {
	Force     bool `json:"force"`
	WaitTasks bool `json:"wait_tasks"`
	AckDrift  bool `json:"ack_drift"`
}

// Restart 只重启 goInception：有待生效的草稿版本时，先把它写入磁盘再重启（失败同样自动恢复）。
func (s *Service) Restart(ctx context.Context, actor audit.Actor, req RestartRequest) (*SaveResult, error) {
	if err := s.needTables(); err != nil {
		return nil, err
	}
	path, err := s.needPath()
	if err != nil {
		return nil, err
	}
	d, err := s.loadDeploy()
	if err != nil {
		return nil, newErr(500, "load_failed", "读取部署设置失败：%v", err)
	}
	if err := needRestartable(d); err != nil {
		return nil, err
	}
	release, err := s.lock(actor, d, true)
	if err != nil {
		return nil, err
	}
	handed := false
	defer func() {
		if !handed {
			release()
		}
	}()
	conn := s.loadConn("main")
	if conn.Host == "" {
		return nil, badReq("还没有配置 goInception 的连接信息，重启后无法做健康检查；请先在“连接信息”里填写并保存")
	}
	sn, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if sn.pending != nil {
		if dr := sn.drift(); dr.Kind == "modified" && !req.AckDrift {
			return nil, conflict("drift", "%s。重启会先把 #%d 写回磁盘，覆盖手工修改（旧内容会备份为 %s）", dr.Message, sn.pending.Version, path+BackupSuffix)
		}
	}
	waitIdle, err := s.checkBusy(ctx, conn, req.Force, req.WaitTasks)
	if err != nil {
		return nil, err
	}
	plan := &flowPlan{actor: actor, deploy: d, path: path, conn: conn, ver: sn.pending, waitIdle: waitIdle, release: release,
		prevDisk: sn.disk, hadDisk: sn.diskInfo.Exists}
	if sn.pending != nil && sn.effective != nil {
		plan.recoverTo = sn.effective
	}
	ver := 0
	if sn.pending != nil {
		ver = sn.pending.Version
	}
	plan.job = s.newJob("restart", ver, actor.Username)
	handed = true
	s.startFlow(plan)
	msg := "正在重启 goInception"
	if ver != 0 {
		msg = fmt.Sprintf("正在写入版本 #%d 并重启 goInception", ver)
	}
	return &SaveResult{Version: ver, Status: StatusDraft, Restarting: true, Message: msg}, nil
}

// ImportRequest 是“把磁盘文件导入为新版本”的请求。
type ImportRequest struct {
	AsApplied   bool `json:"as_applied"` // goInception 已经用这份文件重启过（有人绕过页面改了文件并自行重启）
	BaseVersion int  `json:"base_version"`
}

// ImportDisk 把磁盘上被人手工改过的文件记录为新版本，避免它在下次保存时被悄悄覆盖。
func (s *Service) ImportDisk(actor audit.Actor, req ImportRequest) (*SaveResult, error) {
	if err := s.needTables(); err != nil {
		return nil, err
	}
	d, err := s.loadDeploy()
	if err != nil {
		return nil, newErr(500, "load_failed", "读取部署设置失败：%v", err)
	}
	release, err := s.lock(actor, d, false)
	if err != nil {
		return nil, err
	}
	defer release()
	sn, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if sn.path == "" {
		return nil, newErr(400, "no_path", "还没有配置 goInception 配置文件的路径")
	}
	if sn.diskErr != nil {
		return nil, newErr(500, "file_error", "读取配置文件失败：%v", sn.diskErr)
	}
	if !sn.diskInfo.Exists {
		return nil, newErr(404, "not_found", "磁盘上没有这个配置文件")
	}
	if sn.latestNum() != req.BaseVersion {
		return nil, conflict("stale", "配置已经被其他人修改（最新版本是 #%d），请刷新页面后重试", sn.latestNum())
	}
	sum := sha256hex(sn.disk)
	if exp := sn.expected(); exp != nil && exp.ContentSHA256 == sum {
		return nil, badReq("磁盘文件与 #%d 一致，不需要导入", exp.Version)
	}
	summary := "导入磁盘文件（首次纳入版本管理）" // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	if sn.expected() != nil {
		summary = "导入磁盘文件（手工修改）：" + Summarize(sn.baseline(), string(sn.disk)) // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	}
	status := StatusDraft
	var at *time.Time
	if req.AsApplied {
		status = StatusApplied
		now := s.now()
		at = &now
	}
	v := &Version{Content: string(sn.disk), ContentSHA256: sum, ChangeSummary: truncate(summary, 250), Operator: actor.Username,
		Status: status, CreatedAt: s.now(), AppliedAt: at}
	if err := s.repo.InsertVersion(v); err != nil {
		return nil, newErr(500, "save_failed", "保存版本失败：%v", err)
	}
	s.audit(actor, actImport, "版本 #%d：%s；状态：%s", v.Version, v.ChangeSummary, status) // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	_ = s.repo.PruneVersions(keepVersions)
	msg := fmt.Sprintf("已把磁盘文件导入为版本 #%d（已生效）", v.Version)
	if !req.AsApplied {
		msg = fmt.Sprintf("已把磁盘文件导入为版本 #%d（待重启生效）", v.Version)
	}
	return &SaveResult{Version: v.Version, Status: status, Message: msg}, nil
}
