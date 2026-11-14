// @author: kevinkelly
// @license: Apache Licence
// @file: detail.go
// @time: 2026/10/28

package sqlflow

import (
	"fmt"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/instext"
	"dbtool/internal/model"
	"dbtool/internal/review"
)

// Detail 是工单详情页的数据。
type Detail struct {
	Workflow     *model.SqlWorkflow
	InstanceName string
	DBType       string
	Env          string // 目标实例的环境（环境字典里的编码）/ 空；批量工单只要有任一按生产对待的实例就取它
	EnvLabel     string // 环境显示名
	EnvProd      bool   // 按生产对待
	IsRedis      bool   // Redis 工单：命令逐行执行，无自动回滚（页面显示提示与执行前值）
	Content      *model.SqlWorkflowContent
	Audit        *model.WorkflowAudit
	Logs         []model.WorkflowLog
	Nodes        []audit.Node
	Flow         string
	StatusLabel  string
	SyntaxLabel  string
	ReviewRows   []review.Row // 提交时的检查结果
	ExecRows     []review.Row // 执行结果（执行过才有）
	ParseError   string       // 结果 JSON 无法解析时的提示

	Schedule *model.SqlSchedule // 定时执行计划（状态为定时执行时；待审批的工单若提交时设定了计划，也在这里）

	// 审批时间线与当前级可审批人
	Steps            []FlowStep
	CurrentApprovers []ApproverView

	CanCancelSchedule bool // 状态为定时执行，且是提交人或有执行权限
	CanCopy           bool // 终态工单可以复制为新工单（带出名称、实例、库、SQL）

	CanReview    bool
	CanExecute   bool
	CanSchedule  bool
	CanCancel    bool
	CanRollback  bool
	CanAlterDate bool
	CanOsc       bool // 状态为执行中、有执行权限，且配置了 goInception

	CanCancelRun     bool   // 执行中、有权限（DBA / 超级用户）、且正由本进程执行：可以取消执行（见 cancelrun.go）
	CanMarkException bool   // 仍显示排队中 / 执行中、有权限：可以手工标记为“执行有异常”（进程失联时用，见 recover.go）
	StuckHint        string // 排队中 / 执行中且长时间没有任何进展时的提示

	// 数据导出
	IsExport       bool
	ExportFormat   string
	ExportMaxRows  int
	FileName       string // 已生成的文件（压缩包）
	CanDownload    bool
	ExportFile     *ExportFileInfo // 结果文件卡片：大小、生成时间、下载次数、是否已清理
	ExportRows     *int64          // 导出的行数（未结束时是检查阶段统计的预计行数）
	ExportRowsEst  bool            // ExportRows 是预计值
	ExportUnmasked *ExportUnmasked // 提交人（超级用户）选择了导出原值：理由、谁选的、执行时是否仍然生效
	ExportMerge    bool            // 批量导出选择了合并成一个文件：各实例的数据依次写在同一个文件里

	// 批量上线
	IsBatch        bool
	BatchTargets   []string            // 计划上线的实例名（按提交顺序）
	BatchInstances []BatchInstanceView // 执行后各实例的状态
	CanBatchOp     bool                // 能否重试/关闭失败的实例

	// 执行方式（立即执行/定时执行时由执行人选择）
	Executors       []Option
	DefaultExecutor string
	IsManualSubmit  bool // 提交时跳过了检查：只能原生执行
	OnWindow        bool // 当前是否在可执行时间窗内
}

// ExportUnmasked 是导出工单上“导出原值（不脱敏）”的说明（审批人必须能看到）。
type ExportUnmasked struct {
	Reason string    `json:"reason"`
	SetBy  string    `json:"set_by"`
	At     time.Time `json:"at"`
	// Active 为 false 表示提交时选了、但提交人现在已不是启用的超级用户：执行时不会导出原值，按脱敏设置导出。
	Active bool `json:"active"`
}

// executorOptions 返回执行时可选的执行方式及推荐默认值：配置了 goInception 就推荐它，否则推荐原生执行。
// 强制使用 goInception（REQUIRE_GOINCEPTION）时不提供原生执行；跳过检查的工单只能原生执行。
func (s *Service) executorOptions(w *model.SqlWorkflow) ([]Option, string) {
	return s.executorOptionsFor(w, "")
}

// executorOptionsFor 同 executorOptions；sqlText 是工单的 SQL（没有传 / 为空时按普通工单处理）：
// PolarDB-X / SelectDB / ADB / Doris 的 CREATE TABLE 工单只有原生执行（不走 goInception，也不受“强制使用 goInception”限制）。
func (s *Service) executorOptionsFor(w *model.SqlWorkflow, sqlText string) ([]Option, string) {
	if w.IsOfflineExport != 0 {
		return []Option{{ExecNative, "导出（在目标实例上执行查询并生成文件）"}}, ExecNative
	}
	if w.IsManual != 0 {
		return []Option{{ExecNative, "原生执行（直连实例逐条执行）"}}, ExecNative
	}
	// Redis：只有原生执行（直连逐条执行，遇错即停）
	if w.Instance != nil && w.Instance.DBType == dbTypeRedis {
		return []Option{{ExecNative, "原生执行（直连 Redis 逐条执行，遇错即停；无自动回滚）"}}, ExecNative
	}
	if w.IsManual == 0 && w.Instance != nil && nativeCreateTable(w.Instance.DBType, sqlText) {
		return []Option{{ExecNative, "原生执行（直连实例逐条执行建表语句，不走 goInception；不备份、无自动回滚）"}}, ExecNative
	}
	// SQL Server 没有 goInception：只有原生执行，每个单元一个事务，失败回滚并停止
	if w.Instance != nil && w.Instance.DBType == dbTypeMSSQL {
		return []Option{{ExecNative, "原生执行（逐个批次执行，每个批次一个事务，失败回滚并停止）"}}, ExecNative
	}
	var opts []Option
	def := ""
	if s.InceptionConfigured() {
		opts = append(opts, Option{ExecGoInception, "goInception（推荐：带审核、可备份回滚、支持在线改表）"})
		def = ExecGoInception
	}
	if !s.requireInception {
		opts = append(opts, Option{ExecNative, "原生执行（直连实例逐条执行，不备份、不支持在线改表）"})
		if def == "" {
			def = ExecNative
		}
	}
	return opts, def
}

// ExecutorOptions 对外暴露（页面渲染用）。
func (s *Service) ExecutorOptions(w *model.SqlWorkflow) ([]Option, string) {
	return s.executorOptions(w)
}

func (s *Service) load(id uint, forUpdate bool) (*model.SqlWorkflow, error) {
	w, err := s.repo.GetWorkflow(id, forUpdate)
	if err != nil {
		return nil, err
	}
	if w == nil {
		return nil, notFound("工单不存在")
	}
	return w, nil
}

// Get 返回工单详情。看不到的工单按不存在处理（不泄露其存在）。
func (s *Service) Get(p audit.Actor, id uint) (*Detail, error) {
	w, err := s.load(id, false)
	if err != nil {
		return nil, err
	}
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	if !pm.canView(p, w) {
		return nil, notFound("工单不存在")
	}
	c, err := s.repo.GetContent(id)
	if err != nil {
		return nil, err
	}
	if c == nil {
		c = &model.SqlWorkflowContent{WorkflowID: id}
	}
	au, err := s.repo.AuditByWorkflow(audit.TypeSQLReview, id, false)
	if err != nil {
		return nil, err
	}
	d := &Detail{Workflow: w, Content: c, Audit: au, StatusLabel: StatusLabel(w.Status), SyntaxLabel: SyntaxLabel(w.SyntaxType), IsManualSubmit: w.IsManual != 0}
	if w.Instance != nil {
		d.InstanceName, d.DBType = w.Instance.InstanceName, w.Instance.DBType
		d.IsRedis = w.Instance.DBType == dbTypeRedis
	}
	var perr1, perr2 error
	d.ReviewRows, perr1 = review.ParseRows(c.ReviewContent)
	d.ExecRows, perr2 = review.ParseRows(c.ExecuteResult)
	if perr1 != nil || perr2 != nil {
		d.ParseError = "检查/执行结果 JSON 解析失败，请联系管理员"
	}
	// 命中“数据脱敏”配置的字段，其值对非超级用户显示为脱敏后的结果（下面的批量实例状态等都由遮盖后的行派生）
	mk, err := s.maskerFor(&p, w)
	if err != nil {
		return nil, err
	}
	s.maskDetail(mk, d)
	if au != nil {
		if d.Logs, err = s.repo.AuditLogs(au.AuditID); err != nil {
			return nil, err
		}
		for i := range d.Logs {
			d.Logs[i].OperationInfo = mk.Text(d.Logs[i].OperationInfo)
		}
		d.Steps = s.buildSteps(newAuditCache(s.repo), w, au, d.Logs)
		for _, st := range d.Steps {
			if st.State == StepCurrent {
				d.CurrentApprovers = st.Approvers
			}
		}
		d.Nodes = audit.ReviewNodes(s.repo, au)
		d.Flow, _ = audit.FlowText(s.repo, au)
		if au.AuditAuthGroups == "" {
			d.Flow = "无需审批（系统自动）"
		}
		d.CanReview = w.Status == model.WfManReviewing && audit.CanReview(s.repo, au, p)
	}
	if w.IsOfflineExport != 0 {
		d.IsExport, d.ExportFormat = true, exportFormat(w)
		d.ExportMaxRows, _ = s.exportCaps()
		if w.FileName != nil {
			d.FileName = *w.FileName
		}
		d.CanDownload = d.FileName != "" && (pm.download || p.IsSuperuser) && (w.Status == model.WfFinish || w.Status == model.WfException)
		if d.FileName != "" {
			d.ExportFile = s.exportFileInfo(w, d.FileName)
		}
		if e, err := s.repo.GetExportExt(w.ID); err == nil && e != nil {
			if e.Unmasked {
				on, _ := s.exportUnmasked(w)
				d.ExportUnmasked = &ExportUnmasked{Reason: e.Reason, SetBy: e.SetBy, At: e.CreatedAt, Active: on}
			}
			d.ExportMerge = e.MergeFiles && w.IsBatch()
		}
		raw, est := d.ExecRows, false
		if w.Status != model.WfFinish && w.Status != model.WfException {
			raw, est = d.ReviewRows, true
		}
		if n, ok := rowsAffected(raw); ok {
			d.ExportRows, d.ExportRowsEst = &n, est
		}
	}
	if w.IsBatch() {
		d.IsBatch = true
		insts, missing := s.batchInstances(w)
		for _, in := range insts {
			d.BatchTargets = append(d.BatchTargets, in.InstanceName)
		}
		for _, id := range missing {
			d.BatchTargets = append(d.BatchTargets, fmt.Sprintf("(已删除 id:%d)", id))
		}
		d.BatchInstances = enrichBatchViews(batchViews(d.ExecRows), insts, d.ExecRows)
		d.CanBatchOp = (w.Status == model.WfException || w.Status == model.WfFinish) && pm.canOperateBatch(p, w)
	}
	d.Env = s.targetEnv(w)
	d.EnvLabel, d.EnvProd = instext.EnvLabel(d.Env), instext.EnvIsProd(d.Env)
	d.CanExecute = pm.canExecute(p, w)
	d.CanSchedule = d.CanExecute
	d.CanCancel = s.canCancel(p, pm, w, au)
	d.CanRollback = w.IsBackup && (w.Status == model.WfFinish || w.Status == model.WfException) && pm.canView(p, w)
	d.CanAlterDate = au != nil && w.Status == model.WfManReviewing && audit.IsCurrentApprover(s.repo, au, p)
	d.CanOsc = w.Status == model.WfExecuting && s.InceptionConfigured() && s.canControlOsc(p, pm, w)
	d.Executors, d.DefaultExecutor = s.executorOptionsFor(w, c.SQLContent)
	d.OnWindow = onWindow(w, s.now())
	if (w.Status == model.WfTimingTask || w.Status == model.WfManReviewing) && s.repo.ScheduleTableReady() {
		d.Schedule, _ = s.repo.GetSchedule(id)
	}
	d.CanCancelSchedule = s.canCancelSchedule(p, pm, w)
	d.CanCancelRun = s.canCancelRunNow(p, pm, w)
	d.CanMarkException = s.canMarkException(p, pm, w)
	if d.CanMarkException {
		d.StuckHint = s.stuckHint(w)
	}
	d.CanCopy = isTerminal(w.Status) && s.canSubmitKind(pm, d.IsExport, d.IsBatch) && !mk.Changed() // 含已脱敏的值：只有超级用户能复制
	return d, nil
}

// targetEnv 工单目标实例的环境，一次查询取回：批量工单只要有任一目标实例是生产就是 prod，否则有测试实例就是 test；
// 找不到环境（仓库不支持、旁路表不存在、查询失败、实例没有设置）为空——环境只是提示信息，取不到不能让详情打不开。
func (s *Service) targetEnv(w *model.SqlWorkflow) string {
	l, ok := s.repo.(InstanceEnvLoader)
	if !ok {
		return ""
	}
	ids := []uint{w.InstanceID}
	if w.IsBatch() {
		ids = parseBatchIDs(w.BatchInstanceIDs)
	}
	envs, err := l.InstanceEnvs(ids)
	if err != nil {
		return ""
	}
	out := ""
	for _, id := range ids {
		e := envs[id]
		if e == "" || !instext.ValidEnv(e) {
			continue
		}
		if instext.EnvIsProd(e) {
			return e
		}
		out = e
	}
	return out
}

// canCancel 终止权限：审核中的工单，当前审批人和提交人可以终止；审核通过/定时执行的工单，有执行权限的人和提交人可以终止。
func (s *Service) canCancel(p audit.Actor, pm perms, w *model.SqlWorkflow, au *model.WorkflowAudit) bool {
	switch w.Status {
	case model.WfManReviewing:
		// 审核中：提交人可撤回；别人要能“驳回”（sqlworkflow.reject + 当前节点的审批人），与 Cancel 里 audit.Operate 的校验一致
		return w.Engineer == p.Username || (au != nil && audit.CanOperate(s.repo, au, audit.ActionReject, p) == nil)
	case model.WfReviewPass, model.WfTimingTask:
		return w.Engineer == p.Username || pm.canExecute(p, w)
	}
	return false
}

// canControlOsc 控制在线改表（暂停/终止/恢复）需要执行权限：资源组执行权限 + 组内，或提交人本人 + 执行权限。
func (s *Service) canControlOsc(p audit.Actor, pm perms, w *model.SqlWorkflow) bool {
	if p.IsSuperuser {
		return true
	}
	if pm.groups[w.GroupID] && pm.executeRG {
		return true
	}
	return w.Engineer == p.Username && pm.execute
}

// onWindow 当前时刻是否在工单设定的可执行时间窗内。
func onWindow(w *model.SqlWorkflow, at time.Time) bool {
	return !((w.RunDateStart != nil && w.RunDateStart.After(at)) || (w.RunDateEnd != nil && w.RunDateEnd.Before(at)))
}
