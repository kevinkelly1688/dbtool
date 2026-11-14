// @author: kevinkelly
// @license: Apache Licence
// @file: tasks.go
// @time: 2026/10/28

package archive

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/notify"
)

// TaskResult 是创建 / 修改 / 提交的结果。
type TaskResult struct {
	Task    TaskView `json:"data"`
	Outcome string   `json:"outcome"` // draft | submitted | saved
	Changes []string `json:"changes,omitempty"`
}

func validationErr(issues []engine.Issue) *Error {
	msg := "校验未通过"
	for _, x := range issues {
		if x.Level == "error" {
			msg = x.Msg
			break
		}
	}
	return withData(newErr(400, "validation", "%s", msg), map[string]any{"issues": issues})
}

// specOf 把任务转成引擎 Spec。Dest.Table 留空，由引擎按运行时间展开模板。
func specOf(t *Task, runID uint) engine.Spec {
	c := t.cfg()
	return engine.Spec{TaskID: t.ID, RunID: runID, Name: t.Name, InstanceID: t.InstanceID, DB: t.DBName, Table: t.TblName,
		TimeColumn: t.TimeColumn, TimeUnit: t.TimeUnit, RetentionDays: t.RetentionDays, ExtraWhere: t.ExtraWhere, Mode: engine.Mode(t.Mode),
		Dest: c.DestTable, File: c.DestFile, Params: c.Params, Checkpoint: t.checkpoint(), Fingerprint: t.Fingerprint}
}

// ---- 需要连元数据的输入校验 ----

// checkInput 在 Validate 之外校验实例 / 资源组 / 名称 / 备份去向。返回的 error 是权限 / 内部错误；业务问题放在 issues 里。
func (s *Service) checkInput(c Caller, in *TaskInput, pol Policy, exceptID uint) ([]engine.Issue, error) {
	issues := in.issues(pol, s.now())
	add := func(field, msg string) { issues = append(issues, engine.Issue{Field: field, Level: "error", Msg: msg}) }
	if taken, err := s.repo.TaskNameTaken(in.Name, exceptID); err != nil {
		return nil, err
	} else if taken {
		add("name", "任务名称已存在，请换一个")
	}
	var inst *model.Instance
	if in.InstanceID == 0 {
		add("instance_id", "请选择实例")
	} else {
		var err error
		inst, err = s.instanceFor(c, in.InstanceID)
		if err != nil {
			var pe *Error
			if errors.As(err, &pe) && (pe.Status == 403 || pe.Status == 404) {
				add("instance_id", pe.Message)
			} else {
				return nil, err
			}
		} else if err := eligibleInstance(inst); err != nil {
			add("instance_id", err.Error())
			inst = nil
		}
	}
	if in.ResourceGroupID != 0 {
		rg, err := s.repo.ResourceGroupByID(in.ResourceGroupID)
		if err != nil {
			return nil, err
		}
		if rg == nil || rg.IsDeleted != 0 {
			add("resource_group_id", "资源组不存在")
		} else {
			if !c.viewAll() {
				mine, err := s.callerGroups(c)
				if err != nil {
					return nil, err
				}
				if !mine[rg.GroupID] {
					add("resource_group_id", "你不属于该资源组，无法为它建任务")
				}
			}
			if inst != nil {
				ids, err := s.repo.InstanceResourceGroupIDs(inst.ID)
				if err != nil {
					return nil, err
				}
				ok := false
				for _, id := range ids {
					ok = ok || id == rg.GroupID
				}
				if !ok {
					add("resource_group_id", fmt.Sprintf("资源组「%s」没有关联实例 %s", rg.GroupName, inst.InstanceName))
				}
			}
		}
	}
	if d := in.DestTable; d != nil {
		if di, err := s.instanceFor(c, d.InstanceID); err != nil {
			var pe *Error
			if errors.As(err, &pe) && (pe.Status == 403 || pe.Status == 404) {
				add("dest_table.instance_id", fmt.Sprintf("归档库表所在实例：%s", pe.Message))
			} else {
				return nil, err
			}
		} else if err := eligibleInstance(di); err != nil {
			add("dest_table.instance_id", fmt.Sprintf("归档库表所在实例只能是 MySQL / MariaDB / PolarDB / PolarDB-X 主库：%s", err.Error()))
		}
		if d.InstanceID == in.InstanceID && d.DB == in.DB && d.Template == in.Table {
			add("dest_table.template", "目标表不能就是源表")
		}
	}
	if f := in.DestFile; f != nil {
		st, err := s.repo.GetStore(f.StoreID)
		if err != nil {
			return nil, err
		}
		switch {
		case st == nil:
			add("dest_file.store_id", "请选择备份存储")
		case !st.Enabled:
			add("dest_file.store_id", fmt.Sprintf("备份存储「%s」已停用", st.Name))
		}
	}
	return dedupeIssues(issues), nil
}

func (s *Service) getTask(c Caller, id uint) (*Task, error) {
	t, err := s.repo.GetTask(id, false)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, notFound("归档任务不存在")
	}
	ok, err := s.canSeeGroup(c, t.ResourceGroupID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("归档任务不存在")
	}
	return t, nil
}

func newTask(c Caller, in TaskInput, now time.Time) *Task {
	t := &Task{Name: in.Name, ResourceGroupID: in.ResourceGroupID, InstanceID: in.InstanceID, DBName: in.DB, TblName: in.Table, TimeColumn: in.TimeColumn, TimeUnit: in.TimeUnit,
		RetentionDays: in.RetentionDays, ExtraWhere: in.ExtraWhere, Mode: in.Mode, PurgeReason: in.PurgeReason, Status: StDraft, Version: 1,
		Creator: c.Username, CreatorID: c.ID, CreatorDisplay: c.Display, CreatedAt: now, UpdatedAt: now}
	t.setCfg(taskCfg{DestTable: in.DestTable, DestFile: in.DestFile, Params: in.Params, Schedule: in.Schedule})
	t.SpecHash = in.keyHash()
	return t
}

// applyInput 把输入写回已有任务（不碰状态 / 审批 / 断点 / 统计）。
func applyInput(t *Task, in TaskInput) {
	t.Name, t.ResourceGroupID, t.InstanceID, t.DBName, t.TblName = in.Name, in.ResourceGroupID, in.InstanceID, in.DB, in.Table
	t.TimeColumn, t.TimeUnit, t.RetentionDays, t.ExtraWhere, t.Mode, t.PurgeReason = in.TimeColumn, in.TimeUnit, in.RetentionDays, in.ExtraWhere, in.Mode, in.PurgeReason
	t.setCfg(taskCfg{DestTable: in.DestTable, DestFile: in.DestFile, Params: in.Params, Schedule: in.Schedule})
	t.SpecHash = in.keyHash()
}

// ---- 试运行 token ----

func (s *Service) verifyCheck(tx Repo, in TaskInput, taskID uint, now time.Time) (*CheckRow, error) {
	if in.CheckToken == "" {
		return nil, newErr(422, "check_required", "提交申请前必须先试运行")
	}
	row, err := tx.GetCheckByToken(in.CheckToken)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, newErr(422, "check_required", "试运行结果不存在，请重新试运行")
	}
	if now.After(row.ExpiresAt) {
		return nil, newErr(422, "check_expired", "试运行结果已超过 %d 天有效期，请重新试运行", checkValidDays)
	}
	if row.SpecHash != in.keyHash() || (row.TaskID != 0 && taskID != 0 && row.TaskID != taskID) {
		return nil, newErr(422, "check_mismatch", "试运行之后修改了来源 / 条件 / 模式 / 备份去向 / 总量上限，请重新试运行")
	}
	if row.Blocked {
		return nil, newErr(422, "check_failed", "试运行有红色检查项，不能提交")
	}
	if (row.WarnCount > 0 && !in.AckWarn) || (row.Big && !in.AckBig) {
		return nil, withData(newErr(422, "ack_required", "试运行有 %d 个黄色警告或命中比例超过 %d%%，请先勾选确认", row.WarnCount, int(bigPercent)),
			map[string]any{"need_warn": row.WarnCount, "need_big": row.Big})
	}
	return row, nil
}

// ---- 审批流 ----

func noFlowErr(rgName string) *Error {
	return newErr(422, "no_flow", "资源组「%s」没有配置“数据归档申请”审批流，不能提交；请联系管理员在「审批流程配置」里配置", rgName)
}

func translateAudit(err error, rgName string) error {
	var ae *audit.Error
	if errors.As(err, &ae) {
		switch ae.Kind {
		case audit.KindConflict:
			return conflict("%s", ae.Message)
		case audit.KindConfig:
			if rgName != "" {
				return noFlowErr(rgName)
			}
			return newErr(422, "no_flow", "%s", ae.Message)
		default:
			return forbidden("%s", ae.Message)
		}
	}
	return err
}

// startApproval 为任务创建（或重置）审批流。dbt_workflow_audit 对 (workflow_id, workflow_type) 唯一，
// 所以被驳回 / 停用后重新提交时重置同一条审批记录（审批明细与日志保留，页面只展示最近一轮）。
func (s *Service) startApproval(tx Repo, c Caller, t *Task, now time.Time) (*model.WorkflowAudit, error) {
	rg, err := tx.ResourceGroupByID(t.ResourceGroupID)
	if err != nil {
		return nil, err
	}
	if rg == nil {
		return nil, badReq("资源组不存在")
	}
	setting, err := tx.AuditSetting(rg.GroupID, audit.TypeArchive)
	if err != nil {
		return nil, err
	}
	groups := splitGroupIDs("")
	if setting != nil {
		groups = splitGroupIDs(setting.AuditAuthGroups)
	}
	if len(groups) == 0 {
		return nil, noFlowErr(rg.GroupName)
	}
	existing, err := tx.AuditByWorkflow(audit.TypeArchive, t.ID, true)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		au, err := audit.Create(tx, now, audit.Spec{WorkflowType: audit.TypeArchive, WorkflowID: t.ID, GroupID: rg.GroupID, GroupName: rg.GroupName,
			Title: t.Name, CreateUser: c.Username, CreateUserDisplay: c.Display})
		if err != nil {
			return nil, translateAudit(err, rg.GroupName)
		}
		return au, nil
	}
	existing.GroupID, existing.GroupName, existing.WorkflowTitle = rg.GroupID, rg.GroupName, t.Name
	existing.AuditAuthGroups = strings.Join(groups, ",")
	existing.CurrentAudit, existing.NextAudit, existing.CurrentStatus = groups[0], "-1", audit.StatusWaiting
	if len(groups) > 1 {
		existing.NextAudit = groups[1]
	}
	existing.CreateUser, existing.CreateUserDisplay = c.Username, c.Display
	if err := tx.SaveAudit(existing); err != nil {
		return nil, err
	}
	flow, err := audit.FlowText(tx, existing)
	if err != nil {
		return nil, err
	}
	err = tx.InsertLog(&model.WorkflowLog{AuditID: existing.AuditID, OperationType: audit.ActionSubmit, OperationTypeDesc: "重新提交", // i18n-ignore：审批日志，写库后由审批模块展示
		OperationInfo: "等待审批，审批流程：" + flow, Operator: c.Username, OperatorDisplay: c.Display, OperationTime: now}) // i18n-ignore：审批日志
	return existing, err
}

// ---- 创建 / 修改 / 提交 ----

func (s *Service) viewResult(c Caller, t *Task, outcome string, changes []string) *TaskResult {
	v := s.newViewer(c)
	return &TaskResult{Task: v.view(t, true), Outcome: outcome, Changes: changes}
}

func (s *Service) busyErr(t *Task) error {
	return withData(newErr(409, "table_busy", "这张表已有进行中的任务「%s」（%s）；同一张表同时只能有一个进行中的任务", t.Name, statusLabel(t.Status)),
		map[string]any{"task_id": t.ID, "name": t.Name, "status": t.Status})
}

// CreateTask 新建任务：Submit=false 存草稿；Submit=true 校验试运行 token 后提交审批。
func (s *Service) CreateTask(c Caller, in TaskInput) (*TaskResult, error) {
	if err := c.need("archive.apply", "新建归档任务"); err != nil {
		return nil, err
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	pol := s.loadPolicy()
	in.normalize(pol)
	issues, err := s.checkInput(c, &in, pol, 0)
	if err != nil {
		return nil, err
	}
	if hasErrorIssue(issues) {
		return nil, validationErr(issues)
	}
	now := s.now()
	t := newTask(c, in, now)
	t.addHistory(now, c.Username, "创建任务（草稿）")
	if !in.Submit {
		if err := s.repo.InsertTask(t); err != nil {
			return nil, err
		}
		s.auditAt(c, "保存草稿", fmt.Sprintf("任务「%s」：%s.%s，%s，保留 %d 天", t.Name, t.DBName, t.TblName, modeLabel(t.Mode), t.RetentionDays)) // i18n-ignore：审计 / 通知正文，不翻译
		return s.viewResult(c, t, "draft", nil), nil
	}
	var au *model.WorkflowAudit
	err = s.repo.InTx(func(tx Repo) error {
		if busy, err := tx.ActiveTaskOnTable(t.InstanceID, t.DBName, t.TblName, 0); err != nil {
			return err
		} else if busy != nil {
			return s.busyErr(busy)
		}
		cr, err := s.verifyCheck(tx, in, 0, now)
		if err != nil {
			return err
		}
		t.Fingerprint, t.Status = cr.Fingerprint, StPending
		t.History = ""
		t.addHistory(now, c.Username, fmt.Sprintf("提交申请（试运行于 %s）", fmtT(cr.CreatedAt)))
		if err := tx.InsertTask(t); err != nil {
			return err
		}
		if au, err = s.startApproval(tx, c, t, now); err != nil {
			return err
		}
		t.AuditID = au.AuditID
		return tx.SaveTask(t)
	})
	if err != nil {
		return nil, err
	}
	s.auditAt(c, "提交申请", fmt.Sprintf("新建任务「%s」并提交审批：%s.%s，%s，保留 %d 天", t.Name, t.DBName, t.TblName, modeLabel(t.Mode), t.RetentionDays)) // i18n-ignore：审计 / 通知正文，不翻译
	s.notifyApproval(t, au, notify.EventPending, "新的数据归档申请", "")                                                                         // i18n-ignore：审计 / 通知正文，不翻译
	return s.viewResult(c, t, "submitted", nil), nil
}

// UpdateTask 修改任务；是否需要重新审批见 needsApproval。
func (s *Service) UpdateTask(c Caller, id uint, in TaskInput) (*TaskResult, error) {
	if err := c.need("archive.apply", "修改归档任务"); err != nil {
		return nil, err
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	t, err := s.getTask(c, id)
	if err != nil {
		return nil, err
	}
	if s.taskRunning(t.ID) {
		return nil, withData(newErr(409, "running", "运行中的任务不能编辑，请先暂停或终止本次运行"), nil)
	}
	if in.Version != 0 && in.Version != t.Version {
		return nil, newErr(409, "version_conflict", "任务已被他人修改，请刷新后重试")
	}
	pol := s.loadPolicy()
	in.normalize(pol)
	in.TaskID = t.ID
	issues, err := s.checkInput(c, &in, pol, t.ID)
	if err != nil {
		return nil, err
	}
	if hasErrorIssue(issues) {
		return nil, validationErr(issues)
	}
	old := inputOf(t)
	need, reason := needsApproval(t, old, in)
	changes := describeChanges(old, in)
	now := s.now()
	if need && !in.Submit && t.Status != StDraft && t.Status != StRejected {
		return nil, conflict("%s，必须提交审批（submit=true，并带试运行 token）", reason)
	}
	var au *model.WorkflowAudit
	outcome := "saved"
	err = s.repo.InTx(func(tx Repo) error {
		cur, err := tx.GetTask(id, true)
		if err != nil {
			return err
		}
		if cur == nil {
			return notFound("归档任务不存在")
		}
		if in.Version != 0 && in.Version != cur.Version {
			return newErr(409, "version_conflict", "任务已被他人修改，请刷新后重试")
		}
		t = cur
		wasStatus := t.Status
		switch {
		case need && in.Submit:
			if busy, err := tx.ActiveTaskOnTable(in.InstanceID, in.DB, in.Table, t.ID); err != nil {
				return err
			} else if busy != nil {
				return s.busyErr(busy)
			}
			cr, err := s.verifyCheck(tx, in, t.ID, now)
			if err != nil {
				return err
			}
			applyInput(t, in)
			t.Fingerprint, t.Status, t.PauseKind, t.PauseReason, t.PauseAt, t.RejectReason, t.NextRunAt = cr.Fingerprint, StPending, "", "", nil, "", nil
			if keyChanged(old, in) {
				t.Checkpoint = "" // 来源 / 条件改了，旧断点不再适用
			}
			t.addHistory(now, c.Username, fmt.Sprintf("修改并重新提交审批（%s）", orNone(changes)))
			if au, err = s.startApproval(tx, c, t, now); err != nil {
				return err
			}
			t.AuditID = au.AuditID
			outcome = "submitted"
			_ = wasStatus
		case need:
			applyInput(t, in) // 草稿 / 已驳回：只保存，不提交
			t.addHistory(now, c.Username, "保存修改（未提交）")
			if t.Status == StDraft {
				outcome = "draft"
			}
		default:
			applyInput(t, in)
			t.addHistory(now, c.Username, fmt.Sprintf("调整参数（更保守，无需重新审批）：%s", orNone(changes)))
			if t.Status == StEnabled {
				t.NextRunAt = ptrTime(nextRun(in.Schedule, now))
			}
		}
		t.UpdatedAt = now
		if err := tx.SaveTask(t); err != nil {
			if errors.Is(err, ErrVersion) {
				return newErr(409, "version_conflict", "任务已被他人修改，请刷新后重试")
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	switch outcome {
	case "submitted":
		s.auditAt(c, "提交申请", fmt.Sprintf("修改任务「%s」并重新提交审批：%s", t.Name, orNone(changes))) // i18n-ignore：审计 / 通知正文，不翻译
		s.notifyApproval(t, au, notify.EventPending, "数据归档申请已重新提交", "")                  // i18n-ignore：审计 / 通知正文，不翻译
	case "saved":
		s.auditAt(c, "修改任务", fmt.Sprintf("调整任务「%s」参数（更保守，无需重新审批）：%s", t.Name, orNone(changes))) // i18n-ignore：审计 / 通知正文，不翻译
	default:
		s.auditAt(c, "保存草稿", fmt.Sprintf("修改任务「%s」", t.Name)) // i18n-ignore：审计 / 通知正文，不翻译
	}
	return s.viewResult(c, t, outcome, changes), nil
}

func keyChanged(o, n TaskInput) bool {
	return o.InstanceID != n.InstanceID || o.DB != n.DB || o.Table != n.Table || o.TimeColumn != n.TimeColumn || o.TimeUnit != n.TimeUnit || o.RetentionDays != n.RetentionDays || o.ExtraWhere != n.ExtraWhere
}

func orNone(l []string) string {
	if len(l) == 0 {
		return "无字段变化"
	}
	return strings.Join(l, "；")
}

func ptrTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// SubmitTask 不改内容，直接把草稿 / 已驳回 / 已停用 / 结构变更暂停的任务提交审批。
func (s *Service) SubmitTask(c Caller, id uint, token string, ackWarn, ackBig bool) (*TaskResult, error) {
	t, err := s.getTask(c, id)
	if err != nil {
		return nil, err
	}
	in := inputOf(t)
	in.Submit, in.CheckToken, in.AckWarn, in.AckBig = true, token, ackWarn, ackBig
	r, err := s.UpdateTask(c, id, in)
	if err != nil {
		return nil, err
	}
	if r.Outcome != "submitted" {
		return nil, conflict("该任务当前状态（%s）不需要提交审批", statusLabel(t.Status))
	}
	return r, nil
}

// ---- 审批 ----

func (s *Service) notifyApproval(t *Task, au *model.WorkflowAudit, name, title, extra string) {
	ev := notify.Event{Kind: "archive", Name: name, Title: "[数据归档申请]" + title, Content: taskContent(t, s.instNameOf(t.InstanceID), extra, s.taskLink(t)), GroupID: t.ResourceGroupID, Applicant: t.Creator} // i18n-ignore：通知标题，语言属于接收方
	if name == notify.EventPending && au != nil {
		if id := notify.ParseIDs(au.CurrentAudit); len(id) > 0 {
			ev.ReviewerAuthGroupID = id[0]
		}
		if notify.IsFirstAuditNode(au.AuditAuthGroups, au.CurrentAudit) {
			ev.Specific = notify.EventSubmitted
		}
	}
	s.notifyAsync(ev)
}

// Approve 审批通过当前一级；最后一级通过后任务自动启用。
func (s *Service) Approve(c Caller, id uint, remark string) (*TaskView, error) {
	return s.review(c, id, audit.ActionPass, strings.TrimSpace(remark))
}

// Reject 驳回，原因必填。
func (s *Service) Reject(c Caller, id uint, reason string) (*TaskView, error) {
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) < 2 {
		return nil, badReq("驳回时请填写驳回原因（至少 2 个字）")
	}
	return s.review(c, id, audit.ActionReject, reason)
}

func (s *Service) review(c Caller, id uint, action int, remark string) (*TaskView, error) {
	if !s.repo.Ready() {
		return nil, notReady()
	}
	if _, err := s.getTask(c, id); err != nil { // 可见范围（审批人必须在资源组内；超级用户全部可见）
		return nil, err
	}
	now := s.now()
	var t *Task
	var au *model.WorkflowAudit
	var enabled bool
	err := s.repo.InTx(func(tx Repo) error {
		var err error
		if t, err = tx.GetTask(id, true); err != nil {
			return err
		}
		if t == nil {
			return notFound("归档任务不存在")
		}
		if t.Status != StPending {
			return conflict("任务当前状态为「%s」，不在审批中", statusLabel(t.Status))
		}
		if au, err = tx.AuditByWorkflow(audit.TypeArchive, t.ID, true); err != nil {
			return err
		}
		if au == nil {
			return newErr(500, "data_error", "任务缺少审批流记录，请联系管理员")
		}
		if au.CreateUser == c.Username {
			return newErr(409, "self_approve", "不能审批自己提交的申请")
		}
		if _, err := audit.Operate(tx, now, au, action, c.actor(), remark); err != nil {
			return translateAudit(err, "")
		}
		t.UpdatedAt = now
		switch {
		case action == audit.ActionReject:
			t.Status, t.RejectReason = StRejected, remark
			t.addHistory(now, c.Username, fmt.Sprintf("驳回：%s", remark))
		case au.CurrentStatus == audit.StatusPassed:
			t.Status, t.RejectReason = StEnabled, ""
			t.NextRunAt = ptrTime(nextRun(t.cfg().Schedule, now))
			t.addHistory(now, c.Username, "审批通过，任务已启用")
			enabled = true
		default:
			t.addHistory(now, c.Username, "审批通过（进入下一级）")
		}
		return tx.SaveTask(t)
	})
	if err != nil {
		return nil, err
	}
	switch {
	case action == audit.ActionReject:
		s.auditAt(c, "驳回", fmt.Sprintf("驳回任务「%s」：%s", t.Name, remark))       // i18n-ignore：审计 / 通知正文，不翻译
		s.notifyApproval(t, au, notify.EventRejected, "被驳回", "驳回原因："+remark) // i18n-ignore：审计 / 通知正文，不翻译
	case enabled:
		s.auditAt(c, "审批通过", fmt.Sprintf("审批通过任务「%s」，审批已全部通过，任务已启用", t.Name)) // i18n-ignore：审计 / 通知正文，不翻译
		s.notifyApproval(t, au, notify.EventPassed, "审批通过，任务已启用", "")         // i18n-ignore：审计 / 通知正文，不翻译
	default:
		s.auditAt(c, "审批通过", fmt.Sprintf("审批通过任务「%s」，进入下一级审批", t.Name)) // i18n-ignore：审计 / 通知正文，不翻译
		s.notifyApproval(t, au, notify.EventPending, "流转到下一级审批", "")    // i18n-ignore：审计 / 通知正文，不翻译
	}
	v := s.newViewer(c).view(t, true)
	return &v, nil
}

// ---- 暂停 / 恢复 / 停用 / 删除 ----

func (s *Service) mutateTask(c Caller, id uint, fn func(t *Task, now time.Time) error) (*Task, error) {
	if !s.repo.Ready() {
		return nil, notReady()
	}
	if _, err := s.getTask(c, id); err != nil {
		return nil, err
	}
	var out *Task
	err := s.repo.InTx(func(tx Repo) error {
		t, err := tx.GetTask(id, true)
		if err != nil {
			return err
		}
		if t == nil {
			return notFound("归档任务不存在")
		}
		now := s.now()
		if err := fn(t, now); err != nil {
			return err
		}
		t.UpdatedAt = now
		out = t
		return tx.SaveTask(t)
	})
	return out, err
}

// PauseTask 暂停任务（不再按调度运行）。正在运行的运行请用 PauseRun。
func (s *Service) PauseTask(c Caller, id uint, reason string) (*TaskView, error) {
	if err := c.need("archive.pause", "暂停归档任务"); err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	t, err := s.mutateTask(c, id, func(t *Task, now time.Time) error {
		if t.Status != StEnabled {
			return conflict("只有已启用的任务可以暂停（当前：%s）", statusLabel(t.Status))
		}
		if s.taskRunning(t.ID) {
			return newErr(409, "running", "任务正在运行，请暂停或终止本次运行")
		}
		if reason == "" {
			reason = fmt.Sprintf("%s 手动暂停", c.Username)
		}
		t.Status, t.PauseKind, t.PauseReason, t.PauseAt, t.NextRunAt = StPaused, PauseManual, truncRunes(reason, 900), ptrTime(now), nil
		t.addHistory(now, c.Username, "暂停任务")
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.auditAt(c, "暂停任务", fmt.Sprintf("暂停归档任务「%s」", t.Name)) // i18n-ignore：审计 / 通知正文，不翻译
	v := s.newViewer(c).view(t, true)
	return &v, nil
}

// ResumeTask 恢复任务；结构变更暂停需要重新试运行并提交审批，不能直接恢复。
func (s *Service) ResumeTask(c Caller, id uint) (*TaskView, error) {
	if err := c.need("archive.pause", "恢复归档任务"); err != nil {
		return nil, err
	}
	t, err := s.mutateTask(c, id, func(t *Task, now time.Time) error {
		if t.Status != StPaused {
			return conflict("只有已暂停的任务可以恢复（当前：%s）", statusLabel(t.Status))
		}
		if t.PauseKind == PauseStructure {
			return conflict("源表结构变更导致的暂停需要重新试运行并提交审批（点“编辑 / 重新提交”），不能直接恢复")
		}
		t.Status, t.PauseKind, t.PauseReason, t.PauseAt = StEnabled, "", "", nil
		t.NextRunAt = ptrTime(nextRun(t.cfg().Schedule, now))
		t.addHistory(now, c.Username, "恢复任务（按调度继续，从断点接着跑）")
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.auditAt(c, "恢复任务", fmt.Sprintf("恢复归档任务「%s」", t.Name)) // i18n-ignore：审计 / 通知正文，不翻译
	v := s.newViewer(c).view(t, true)
	return &v, nil
}

// DisableTask 停用任务（待审批的审批流随之作废）。
func (s *Service) DisableTask(c Caller, id uint, confirm bool) (*TaskView, error) {
	if err := c.need("archive.delete", "停用归档任务"); err != nil {
		return nil, err
	}
	if !confirm {
		return nil, confirmNeeded("停用任务需要二次确认（confirm=true）")
	}
	now := s.now()
	t, err := s.mutateTaskTx(c, id, func(tx Repo, t *Task, now time.Time) error {
		switch t.Status {
		case StEnabled, StPaused, StPending:
		default:
			return conflict("任务当前状态（%s）不需要停用", statusLabel(t.Status))
		}
		if s.taskRunning(t.ID) {
			return newErr(409, "running", "该任务正在运行，请先终止运行")
		}
		if t.Status == StPending {
			if au, err := tx.AuditByWorkflow(audit.TypeArchive, t.ID, true); err != nil {
				return err
			} else if au != nil && au.CurrentStatus == audit.StatusWaiting {
				au.CurrentStatus, au.NextAudit = audit.StatusAborted, "-1"
				if err := tx.SaveAudit(au); err != nil {
					return err
				}
				if err := tx.InsertLog(&model.WorkflowLog{AuditID: au.AuditID, OperationType: audit.ActionAbort, OperationTypeDesc: audit.ActionLabel[audit.ActionAbort],
					OperationInfo: "任务被停用，审批作废", Operator: c.Username, OperatorDisplay: c.Display, OperationTime: now}); err != nil { // i18n-ignore：审批日志
					return err
				}
			}
		}
		t.Status, t.PauseKind, t.PauseReason, t.PauseAt, t.NextRunAt = StDisabled, "", "", nil, nil
		t.addHistory(now, c.Username, "停用任务")
		return nil
	})
	_ = now
	if err != nil {
		return nil, err
	}
	s.auditAt(c, "停用任务", fmt.Sprintf("停用归档任务「%s」", t.Name)) // i18n-ignore：审计 / 通知正文，不翻译
	v := s.newViewer(c).view(t, true)
	return &v, nil
}

func (s *Service) mutateTaskTx(c Caller, id uint, fn func(tx Repo, t *Task, now time.Time) error) (*Task, error) {
	if !s.repo.Ready() {
		return nil, notReady()
	}
	if _, err := s.getTask(c, id); err != nil {
		return nil, err
	}
	var out *Task
	err := s.repo.InTx(func(tx Repo) error {
		t, err := tx.GetTask(id, true)
		if err != nil {
			return err
		}
		if t == nil {
			return notFound("归档任务不存在")
		}
		now := s.now()
		if err := fn(tx, t, now); err != nil {
			return err
		}
		t.UpdatedAt = now
		out = t
		return tx.SaveTask(t)
	})
	return out, err
}

// DeleteTask 删除任务配置（仅草稿 / 已驳回 / 已停用）；运行记录与已备份的数据保留。confirmName 必须等于任务名。
func (s *Service) DeleteTask(c Caller, id uint, confirmName string) error {
	if err := c.need("archive.delete", "删除归档任务"); err != nil {
		return err
	}
	if !s.repo.Ready() {
		return notReady()
	}
	t, err := s.getTask(c, id)
	if err != nil {
		return err
	}
	if strings.TrimSpace(confirmName) != t.Name {
		return confirmNeeded("请输入任务名「%s」确认删除", t.Name)
	}
	if s.taskRunning(t.ID) {
		return newErr(409, "running", "该任务正在运行，请先终止运行")
	}
	if t.Status != StDraft && t.Status != StRejected && t.Status != StDisabled {
		return conflict("任务当前状态（%s）不能删除，请先停用", statusLabel(t.Status))
	}
	if err := s.repo.DeleteTask(t.ID); err != nil {
		return err
	}
	s.auditAt(c, "删除任务", fmt.Sprintf("删除归档任务「%s」（%s.%s）", t.Name, t.DBName, t.TblName)) // i18n-ignore：审计 / 通知正文，不翻译
	return nil
}

// ---- 查询 ----

// TaskListQuery 是任务列表的筛选参数。
type TaskListQuery struct {
	Status     string
	Mode       string
	InstanceID uint
	Q          string
	Mine       bool
	Offset     int
	Limit      int
}

type taskKPI struct {
	Total       int   `json:"total"`
	Enabled     int   `json:"enabled"`
	Running     int   `json:"running"`
	Pending     int   `json:"pending"`
	PendingMine int   `json:"pending_mine"`
	Paused      int   `json:"paused"`
	CumRows     int64 `json:"cum_rows"`
}

type instRef struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

// TaskList 是 GET /tasks 的结果。
type TaskList struct {
	Data      []TaskView `json:"data"`
	Total     int        `json:"total"`
	KPI       taskKPI    `json:"kpi"`
	Instances []instRef  `json:"instances"`
}

func clampLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func (s *Service) ListTasks(c Caller, q TaskListQuery) (*TaskList, error) {
	if err := c.need("archive.view", "查看归档任务"); err != nil {
		return nil, err
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	all, ids, err := s.scope(c)
	if err != nil {
		return nil, err
	}
	base := TaskFilter{AllGroups: all, ResourceGroupIDs: ids}
	everything, _, err := s.repo.ListTasks(base)
	if err != nil {
		return nil, err
	}
	v := s.newViewer(c)
	out := &TaskList{Data: []TaskView{}, Instances: []instRef{}}
	seenInst := map[uint]bool{}
	var matched []Task
	needle := strings.ToLower(strings.TrimSpace(q.Q))
	for i := range everything {
		t := &everything[i]
		out.KPI.Total++
		out.KPI.CumRows += t.CumRows
		switch t.Status {
		case StEnabled:
			out.KPI.Enabled++
		case StPending:
			out.KPI.Pending++
		case StPaused:
			out.KPI.Paused++
		}
		if s.liveOfTask(t.ID) != nil {
			out.KPI.Running++
		}
		mineOK := false
		if t.Status == StPending {
			if _, ok := v.approvalOf(t); ok {
				mineOK = true
				out.KPI.PendingMine++
			}
		}
		if !seenInst[t.InstanceID] {
			seenInst[t.InstanceID] = true
			out.Instances = append(out.Instances, instRef{ID: t.InstanceID, Name: v.insts[t.InstanceID]})
		}
		switch {
		case q.Status != "" && t.Status != q.Status:
			continue
		case q.Mode != "" && t.Mode != q.Mode:
			continue
		case q.InstanceID != 0 && t.InstanceID != q.InstanceID:
			continue
		case q.Mine && !mineOK:
			continue
		case needle != "" && !strings.Contains(strings.ToLower(t.Name+" "+t.DBName+"."+t.TblName+" "+v.insts[t.InstanceID]), needle):
			continue
		}
		matched = append(matched, *t)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		mi, mj := v.mineRank(&matched[i]), v.mineRank(&matched[j])
		if mi != mj {
			return mi < mj
		}
		ri, rj := s.liveOfTask(matched[i].ID) != nil, s.liveOfTask(matched[j].ID) != nil
		if ri != rj {
			return ri
		}
		return matched[i].ID < matched[j].ID
	})
	sort.Slice(out.Instances, func(i, j int) bool { return out.Instances[i].Name < out.Instances[j].Name })
	out.Total = len(matched)
	limit := clampLimit(q.Limit, 50, 200)
	off := q.Offset
	if off < 0 {
		off = 0
	}
	for i := off; i < len(matched) && i < off+limit; i++ {
		out.Data = append(out.Data, v.view(&matched[i], false))
	}
	return out, nil
}

// mineRank 待我审批的排最前。
func (v *viewer) mineRank(t *Task) int {
	if t.Status == StPending {
		if _, ok := v.approvalOf(t); ok {
			return 0
		}
	}
	return 1
}

// ApprovalStep 是审批时间线的一级。
type ApprovalStep struct {
	Level     int      `json:"level"`
	GroupName string   `json:"group_name"`
	State     string   `json:"state"` // approved | rejected | pending | skipped
	Users     []string `json:"users"`
	By        string   `json:"by"`
	At        string   `json:"at"`
	Remark    string   `json:"remark"`
}

// ApprovalView 是任务详情里的审批时间线。
type ApprovalView struct {
	AuditID     uint           `json:"audit_id"`
	Status      string         `json:"status"`
	Applicant   string         `json:"applicant"`
	AppliedAt   string         `json:"applied_at"`
	PurgeReason string         `json:"purge_reason"`
	Steps       []ApprovalStep `json:"steps"`
	Reject      *struct {
		By     string `json:"by"`
		At     string `json:"at"`
		Reason string `json:"reason"`
	} `json:"reject"`
}

func (s *Service) approvalDetail(t *Task) (*ApprovalView, error) {
	au, err := s.repo.AuditByWorkflow(audit.TypeArchive, t.ID, false)
	if err != nil || au == nil {
		return nil, err
	}
	logs, err := s.repo.AuditLogs(au.AuditID)
	if err != nil {
		return nil, err
	}
	details, err := s.repo.AuditDetails(au.AuditID)
	if err != nil {
		return nil, err
	}
	// 最近一轮提交的时间（重新提交会在同一条审批记录上追加“提交”日志）
	var round time.Time
	var applicant string
	for _, l := range logs {
		if l.OperationType == audit.ActionSubmit {
			round, applicant = l.OperationTime, l.Operator
		}
	}
	if applicant == "" {
		applicant = au.CreateUser
	}
	av := &ApprovalView{AuditID: au.AuditID, Status: auditStatusText(au.CurrentStatus), Applicant: applicant, AppliedAt: fmtT(round),
		PurgeReason: t.PurgeReason, Steps: []ApprovalStep{}}
	groups := splitGroupIDs(au.AuditAuthGroups)
	var passes []model.WorkflowAuditDetail
	var rejectD *model.WorkflowAuditDetail
	for i := range details {
		d := details[i]
		if d.AuditTime.Before(round) {
			continue
		}
		switch d.AuditStatus {
		case audit.StatusPassed:
			passes = append(passes, d)
		case audit.StatusRejected:
			rejectD = &details[i]
		}
	}
	cur := -1
	if au.CurrentStatus == audit.StatusWaiting {
		for i, g := range groups {
			if g == au.CurrentAudit {
				cur = i
			}
		}
	}
	for i := range groups {
		st := ApprovalStep{Level: i, GroupName: s.groupNameByID(groups, i), Users: s.levelUsers(au, i, "")}
		if st.Users == nil {
			st.Users = []string{}
		}
		switch {
		case i < len(passes):
			st.State, st.By, st.At, st.Remark = "approved", passes[i].AuditUser, fmtT(passes[i].AuditTime), passes[i].Remark
		case rejectD != nil && i == len(passes):
			st.State, st.By, st.At, st.Remark = "rejected", rejectD.AuditUser, fmtT(rejectD.AuditTime), rejectD.Remark
		case i == cur:
			st.State = "pending"
		default:
			st.State = "skipped"
		}
		av.Steps = append(av.Steps, st)
	}
	if rejectD != nil {
		av.Reject = &struct {
			By     string `json:"by"`
			At     string `json:"at"`
			Reason string `json:"reason"`
		}{rejectD.AuditUser, fmtT(rejectD.AuditTime), rejectD.Remark}
	}
	return av, nil
}

// TaskDetail 是 GET /tasks/:id 的结果。
type TaskDetail struct {
	TaskView
	RecentRuns       []RunView     `json:"recent_runs"`
	History          []HistoryItem `json:"history"`
	ApprovalDetail   *ApprovalView `json:"approval_detail"`
	FingerprintDrift bool          `json:"fingerprint_drift"`
}

func (s *Service) GetTask(c Caller, id uint) (*TaskDetail, error) {
	if err := c.need("archive.view", "查看归档任务"); err != nil {
		return nil, err
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	t, err := s.getTask(c, id)
	if err != nil {
		return nil, err
	}
	v := s.newViewer(c)
	d := &TaskDetail{TaskView: v.view(t, true), RecentRuns: []RunView{}, History: t.history()}
	if d.History == nil {
		d.History = []HistoryItem{}
	}
	runs, err := s.repo.RecentRuns(t.ID, 6)
	if err != nil {
		return nil, err
	}
	for i := range runs {
		d.RecentRuns = append(d.RecentRuns, s.runView(c, &runs[i], v))
	}
	if d.ApprovalDetail, err = s.approvalDetail(t); err != nil {
		return nil, err
	}
	return d, nil
}
