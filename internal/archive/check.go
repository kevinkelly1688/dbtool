// @author: kevinkelly
// @license: Apache Licence
// @file: check.go
// @time: 2026/10/28

package archive

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/audit"
)

// PreviewResult 是 POST /preview 的结果：纯计算，不连库。
type PreviewResult struct {
	Issues        []engine.Issue    `json:"issues"`
	Where         string            `json:"where"`
	SQL           engine.SQLPreview `json:"sql"`
	DestTableName string            `json:"dest_table_name"`
	DestFilePath  string            `json:"dest_file_path"`
	ScheduleText  string            `json:"schedule_text"`
	WindowText    string            `json:"window_text"`
	NextRuns      []string          `json:"next_runs"`
	NeedsApproval bool              `json:"needs_approval"`
	Changes       []string          `json:"changes"`
}

// Preview 校验输入并生成 WHERE / 将执行的 SQL / 调度预览，不连库（archive.view）。
func (s *Service) Preview(c Caller, in TaskInput) (*PreviewResult, error) {
	if err := c.need("archive.view", "预览归档任务"); err != nil {
		return nil, err
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	pol := s.loadPolicy()
	in.normalize(pol)
	var existing *Task
	if in.TaskID != 0 {
		t, err := s.getTask(c, in.TaskID)
		if err != nil {
			return nil, err
		}
		existing = t
	}
	var exceptID uint
	if existing != nil {
		exceptID = existing.ID
	}
	issues, err := s.checkInput(c, &in, pol, exceptID)
	if err != nil {
		return nil, err
	}
	spec := in.spec()
	now := s.now()
	r := &PreviewResult{Issues: issues, ScheduleText: scheduleTextL(c.Lang, in.Schedule), WindowText: windowTextL(c.Lang, in.Params.Window), NextRuns: []string{}, Changes: []string{}, NeedsApproval: true}
	if issues == nil {
		r.Issues = []engine.Issue{}
	}
	if w, err := engine.BuildWhere(spec); err == nil {
		r.Where = w
	}
	r.SQL = engine.BuildSQLPreview(spec, nil, nil, now)
	if in.DestTable != nil && in.DestTable.Template != "" {
		r.DestTableName = engine.ExpandTableName(in.DestTable.Template, in.Table, now)
	}
	if in.DestFile != nil && in.DestFile.PathTemplate != "" {
		inst := ""
		if i, err := s.instanceByID(in.InstanceID); err == nil {
			inst = i.InstanceName
		}
		r.DestFilePath = engine.ExpandPath(in.DestFile.PathTemplate, spec, inst, now)
	}
	for _, t := range scheduleTimes(in.Schedule, now, 5) {
		r.NextRuns = append(r.NextRuns, t.Format("2006-01-02 15:04"))
	}
	if existing != nil {
		old := inputOf(existing)
		r.NeedsApproval, _ = needsApproval(existing, old, in)
		r.Changes = describeChanges(old, in)
	}
	return r, nil
}

// CheckResponse 是 POST /check 的结果。
type CheckResponse struct {
	Report      *engine.CheckReport `json:"report"`
	CheckToken  string              `json:"check_token"`
	SpecHash    string              `json:"spec_hash"`
	ExpiresAt   string              `json:"expires_at"`
	CheckID     uint                `json:"check_id"`
	NeedAckWarn int                 `json:"need_ack_warn"`
	NeedAckBig  bool                `json:"need_ack_big"`
	Blocked     bool                `json:"blocked"`
}

func newToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

const checkTimeout = 3 * time.Minute

// Check 试运行（archive.dryrun）：只读，连源 / 目标做检查并给出预估，保存 7 天有效的 token。
func (s *Service) Check(ctx context.Context, c Caller, in TaskInput) (*CheckResponse, error) {
	if err := c.need("archive.dryrun", "试运行"); err != nil {
		return nil, err
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	pol := s.loadPolicy()
	in.normalize(pol)
	var existing *Task
	if in.TaskID != 0 {
		t, err := s.getTask(c, in.TaskID)
		if err != nil {
			return nil, err
		}
		existing = t
	}
	var exceptID uint
	if existing != nil {
		exceptID = existing.ID
	}
	issues, err := s.checkInput(c, &in, pol, exceptID)
	if err != nil {
		return nil, err
	}
	if hasErrorIssue(issues) {
		return nil, validationErr(issues)
	}
	spec := in.spec()
	spec.Name = in.Name
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	env, err := s.open(cctx, spec)
	if err != nil {
		return nil, newErr(502, "source_error", "连接源 / 目标失败：%s", sanitizeErr(err))
	}
	defer env.Close()
	rep, err := s.eng.Check(cctx, spec, pol.engine(), env)
	if err != nil {
		return nil, newErr(502, "source_error", "试运行失败：%s", sanitizeErr(err))
	}
	s.addServiceChecks(rep, &in, pol, exceptID)
	big := rep.Percent > bigPercent
	blocked, warns := false, 0
	for _, it := range rep.Items {
		switch it.Level {
		case "error":
			if it.Key != "large_batch" { // 大批量不是阻断项：勾选“已确认大批量”即可提交
				blocked = true
			}
		case "warn":
			if it.Key != "large_batch" { // 大批量单独用 need_ack_big 确认
				warns++
			}
		}
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	now := s.now()
	hash := in.keyHash()
	row := &CheckRow{Token: token, TaskID: in.TaskID, SpecHash: hash, CreatedBy: c.Username, CreatedAt: now, ExpiresAt: now.AddDate(0, 0, checkValidDays),
		Blocked: blocked, WarnCount: warns, Big: big, Fingerprint: rep.Fingerprint, Report: mustJSON(rep)}
	if err := s.repo.InsertCheck(row); err != nil {
		return nil, err
	}
	// 试运行也是一条运行记录（trigger=dryrun）
	instName := ""
	if i, err := s.instanceByID(in.InstanceID); err == nil {
		instName = i.InstanceName
	}
	result := engine.ResultSuccess
	if blocked {
		result = engine.ResultFailed
	}
	note := fmt.Sprintf("预估命中 %d 行（占表 %.1f%%），%d 个警告", rep.EstRows, rep.Percent, warns)
	if blocked {
		note = fmt.Sprintf("预估命中 %d 行（占表 %.1f%%），%d 个警告，存在红色检查项", rep.EstRows, rep.Percent, warns)
	}
	run := &Run{TaskID: in.TaskID, TaskName: in.Name, InstanceID: in.InstanceID, InstanceName: instName, DBName: in.DB, TblName: in.Table,
		ResourceGroupID: in.ResourceGroupID, Trigger: TrigDryrun, Result: result, ByUser: c.Username, StartedAt: now, FinishedAt: &now,
		DurationMs: s.now().Sub(now).Milliseconds(), Scanned: rep.EstRows, CheckID: row.ID, PKCols: mustJSON(rep.PKCols),
		Note:      note,
		CreatedAt: now}
	if blocked {
		run.Err = firstErrorDetail(rep)
	}
	if err := s.repo.InsertRun(run); err != nil {
		return nil, err
	}
	if s.now().Sub(s.lastGCTime()) > time.Hour {
		s.setGC(now)
		_ = s.repo.PurgeChecks(now.AddDate(0, 0, -1))
	}
	s.auditAt(c, "试运行", fmt.Sprintf("试运行归档任务「%s」：预估命中 %d 行（占表 %.1f%%），%d 个警告，%s", in.Name, rep.EstRows, rep.Percent, warns, map[bool]string{true: "有红色项", false: "通过"}[blocked])) // i18n-ignore：审计日志正文
	return &CheckResponse{Report: rep, CheckToken: token, SpecHash: hash, ExpiresAt: fmtT(row.ExpiresAt), CheckID: row.ID, NeedAckWarn: warns, NeedAckBig: big, Blocked: blocked}, nil
}

func (s *Service) lastGCTime() time.Time { s.mu.Lock(); defer s.mu.Unlock(); return s.lastGC }
func (s *Service) setGC(t time.Time)     { s.mu.Lock(); s.lastGC = t; s.mu.Unlock() }

func firstErrorDetail(rep *engine.CheckReport) string {
	for _, it := range rep.Items {
		if it.Level == "error" && it.Key != "large_batch" {
			return truncRunes(it.Title+"："+it.Detail, 400)
		}
	}
	return ""
}

// addServiceChecks 补上引擎不知道的检查项：未结束的 SQL 上线工单、实例并发、审批流是否存在。
func (s *Service) addServiceChecks(rep *engine.CheckReport, in *TaskInput, pol Policy, exceptTask uint) {
	add := func(key, title, level, detail string) {
		rep.Items = append(rep.Items, engine.CheckItem{Key: key, Title: title, Level: level, Detail: detail})
	}
	if in.zeroSleep {
		add("sleep", "批间休眠", "warn", fmt.Sprintf("批间休眠填了 0：系统把 0 当作“使用安全策略默认值”（当前 %d ms），而不是“不休眠”；想要最短休眠请填 1", in.Params.SleepMs))
	}
	if wfs, err := s.repo.OpenWorkflows(in.InstanceID, in.DB, in.Table); err != nil {
		add("workflow", "SQL 上线工单", "warn", fmt.Sprintf("无法读取 SQL 上线工单：%s", sanitizeErr(err)))
	} else if len(wfs) > 0 {
		var parts []string
		for _, w := range wfs {
			parts = append(parts, fmt.Sprintf("#%d「%s」（%s）", w.ID, w.Name, wfStatusLabel(w.Status)))
		}
		add("workflow", "SQL 上线工单", "warn", fmt.Sprintf("源表有未结束的 SQL 上线工单：%s；工单结束前，定时运行会被跳过", strings.Join(parts, "、")))
	} else {
		add("workflow", "SQL 上线工单", "ok", "源表没有待执行 / 执行中的 SQL 上线工单")
	}
	if running, err := s.repo.RunningRuns(in.InstanceID, s.now()); err == nil {
		n := 0
		for _, r := range running {
			if r.TaskID != exceptTask || exceptTask == 0 {
				n++
			}
		}
		if n >= pol.MaxConcurrentPerInstance {
			add("concurrency", "实例并发", "warn", fmt.Sprintf("该实例当前已有 %d 个归档运行在跑（上限 %d），定时运行可能被推迟", n, pol.MaxConcurrentPerInstance))
		} else {
			add("concurrency", "实例并发", "ok", fmt.Sprintf("该实例当前 %d 个归档运行（上限 %d）", n, pol.MaxConcurrentPerInstance))
		}
	}
	if in.ResourceGroupID != 0 {
		if st, err := s.repo.AuditSetting(in.ResourceGroupID, audit.TypeArchive); err == nil {
			if st == nil || len(splitGroupIDs(st.AuditAuthGroups)) == 0 {
				add("flow", "审批流", "error", "该资源组没有配置“数据归档申请”审批流，不能提交；请联系管理员在「审批流程配置」里配置")
			} else {
				add("flow", "审批流", "ok", "已配置“数据归档申请”审批流")
			}
		}
	}
}

// reportOfCheck 解析保存的试运行报告（调试 / 追溯用）。
func reportOfCheck(row *CheckRow) *engine.CheckReport {
	var rep engine.CheckReport
	if json.Unmarshal([]byte(row.Report), &rep) != nil {
		return nil
	}
	return &rep
}
