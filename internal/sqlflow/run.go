// @author: kevinkelly
// @license: Apache Licence
// @file: run.go
// @time: 2026/10/28

package sqlflow

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine"
	"dbtool/internal/goinception"
	"dbtool/internal/model"
	"dbtool/internal/review"
	"dbtool/internal/settings"
)

// maxRunTime 是单个工单的执行时间上限，防止卡死的连接永远占着“执行中”。
const maxRunTime = 24 * time.Hour

// run 在后台执行一张工单。actor 为 nil 表示由定时任务触发。
// 状态流转：排队中/定时执行 → 执行中 → 执行结束/执行有异常。
func (s *Service) run(id uint, actor *audit.Actor, executor string) {
	// 执行期间持有该工单的跨进程锁：别的进程据此判断“还有人在执行”（见 recover.go）。
	// 锁最后释放（defer 先登记），即使下面 panic 后的收尾也在锁内完成。
	release, locked, _, lerr := s.lockRun(id)
	switch {
	case lerr != nil:
		// 取锁出错（库抖动）不应让已批准的工单卡住：照常执行，只是失去“可靠判断”。
		slog.Warn("取工单执行锁失败，继续执行", "workflow", id, "err", lerr)
	case !locked:
		slog.Warn("工单执行锁被占用，说明已有进程在执行，跳过", "workflow", id)
		return
	}
	defer release()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("工单执行协程异常退出", "workflow", id, "panic", r, "stack", string(debug.Stack()))
			s.finish(id, failedSet("", fmt.Sprintf("执行过程中发生内部错误：%v", r)))
		}
	}()
	w, au, ok := s.begin(id, actor, executor)
	if !ok {
		return
	}
	s.progBegin(id)
	defer s.progEnd(id)
	if s.phaseOn("Execute") {
		s.notifyExecuting(w, au, executor)
	}
	c, err := s.repo.GetContent(id)
	var set *review.Set
	var ctl *runCtl
	switch {
	case err != nil:
		set = failedSet("", fmt.Sprintf("读取工单内容失败：%s", err.Error()))
	case c == nil:
		set = failedSet("", "工单内容不存在")
	default:
		// 可被人工取消：取消句柄登记在 s.runs，CancelRun 取消的就是这个 context（见 cancelrun.go）
		base, stopRun := context.WithCancelCause(context.Background())
		ctx, cancel := context.WithTimeout(base, maxRunTime)
		ctl = &runCtl{cancel: stopRun}
		s.runs.Store(id, ctl)
		set = s.doExecute(ctx, w, c.SQLContent, executor)
		s.runs.Delete(id)
		cancel()
		stopRun(nil)
		if w.Instance != nil && w.Instance.DBType == dbTypeRedis {
			s.auditRedisExec(actor, w, set)
		}
	}
	if ctl != nil {
		if by, remark, asked := ctl.state(); asked {
			if hasFailure(set) {
				set = cancelledSet(set, by, remark)
			} else { // 取消请求到得太晚：语句已经全部执行成功，不能把它标成异常
				_ = s.repo.InTx(func(tx Repo) error {
					return s.addLog(tx, au.AuditID, opExecute, "取消执行", "取消请求到达时语句已全部执行完成，没有生效", nil)
				})
			}
		}
	}
	s.finish(id, set)
}

// begin 把工单置为“执行中”。状态不对（已被终止/重复触发）时返回 ok=false，什么也不做。
func (s *Service) begin(id uint, actor *audit.Actor, executor string) (*model.SqlWorkflow, *model.WorkflowAudit, bool) {
	expect := model.WfTimingTask
	if actor != nil {
		expect = model.WfQueuing
	}
	var w *model.SqlWorkflow
	var au *model.WorkflowAudit
	started := false
	err := s.repo.InTx(func(tx Repo) error {
		var err error
		if w, au, err = lockedWorkflow(tx, id); err != nil {
			return err
		}
		if w.Status != expect {
			slog.Warn("工单状态不对，跳过执行", "workflow", id, "status", w.Status, "expect", expect)
			return nil
		}
		// 排队期间可能有别的 DDL 先开始了：退回“审核通过”，让人重新发起，而不是并发执行。
		if msg, err := s.ddlConflict(tx, w); err != nil {
			return err
		} else if msg != "" {
			w.Status = model.WfReviewPass
			if err := tx.SaveWorkflow(w); err != nil {
				return err
			}
			return s.addLog(tx, au.AuditID, opExecute, "执行工单", fmt.Sprintf("未能开始执行，已退回待执行：%s", msg), actor)
		}
		w.Status = model.WfExecuting
		if err := tx.SaveWorkflow(w); err != nil {
			return err
		}
		info := fmt.Sprintf("工单开始执行（执行方式：%s）", executorLabel(executor))
		if actor == nil {
			info = fmt.Sprintf("系统定时执行工单（执行方式：%s）", executorLabel(executor))
		}
		started = true
		return s.addLog(tx, au.AuditID, opExecute, "执行工单", info, actor)
	})
	if err != nil {
		slog.Error("工单开始执行失败", "workflow", id, "err", err)
		return nil, nil, false
	}
	return w, au, started
}

func failedSet(sqlText, msg string) *review.Set {
	return &review.Set{FullSQL: sqlText, Error: msg, Rows: []review.Row{{
		ID: 1, Stage: "Execute failed", ErrLevel: review.LevelError, StageStatus: "异常终止", ErrorMessage: msg, SQL: sqlText,
		BackupDBName: "None",
	}}}
}

// finish 写入执行结果并把工单置为“执行结束”或“执行有异常”。
func (s *Service) finish(id uint, set *review.Set) {
	var w *model.SqlWorkflow
	var au *model.WorkflowAudit
	err := s.repo.InTx(func(tx Repo) error {
		var err error
		if w, au, err = lockedWorkflow(tx, id); err != nil {
			return err
		}
		if w.Status != model.WfExecuting {
			return fmt.Errorf("工单 %d 状态为 %s，禁止重复更新执行结果", id, w.Status)
		}
		c, err := tx.GetContent(id)
		if err != nil {
			return err
		}
		if c == nil {
			c = &model.SqlWorkflowContent{WorkflowID: id}
		}
		c.ExecuteResult = set.JSON()
		now := s.now()
		w.FinishTime = &now
		if set.FileName != "" { // 数据导出：记下生成的文件
			fn := set.FileName
			w.FileName = &fn
		}
		w.Status = model.WfFinish
		if set.Error != "" || set.Warning != "" {
			w.Status = model.WfException
		}
		if err := tx.SaveContent(c); err != nil {
			return err
		}
		if err := tx.SaveWorkflow(w); err != nil {
			return err
		}
		return s.addLog(tx, au.AuditID, opFinish, "执行结束", fmt.Sprintf("执行结果：%s", StatusLabel(w.Status)), nil)
	})
	if err != nil {
		// 执行结果落库失败：SQL 其实已经执行了，必须让工单离开“执行中”，并提示去库里确认。
		slog.Error("保存工单执行结果失败", "workflow", id, "err", err)
		s.finishFallback(id, err)
		return
	}
	if s.phaseOn("Execute") {
		s.notifyExecute(w, au)
	}
}

func (s *Service) finishFallback(id uint, cause error) {
	// 先让工单离开“执行中”；结果内容单独保存，写不进去也不影响状态。
	_ = s.repo.InTx(func(tx Repo) error {
		w, err := tx.GetWorkflow(id, true)
		if err != nil || w == nil || w.Status != model.WfExecuting {
			return err
		}
		now := s.now()
		w.Status, w.FinishTime = model.WfException, &now
		return tx.SaveWorkflow(w)
	})
	_ = s.repo.InTx(func(tx Repo) error {
		c, _ := tx.GetContent(id)
		if c == nil {
			c = &model.SqlWorkflowContent{WorkflowID: id}
		}
		c.ExecuteResult = failedSet("", fmt.Sprintf("执行结果保存失败，请到数据库确认实际执行情况：%s", cause.Error())).JSON()
		return tx.SaveContent(c)
	})
}

// doExecute 按选定的执行方式执行，永远返回结果集（出错时带错误行）。
func (s *Service) doExecute(ctx context.Context, w *model.SqlWorkflow, sqlText, executor string) *review.Set {
	inst := w.Instance
	if inst == nil {
		return failedSet(sqlText, "工单的目标实例不存在")
	}
	if w.IsOfflineExport != 0 {
		return s.execExport(ctx, w, sqlText)
	}
	if w.IsBatch() {
		return s.execBatch(ctx, w, sqlText)
	}
	if inst.DBType == dbTypeRedis { // Redis 只有原生执行（写客户端），不经过 goInception
		return s.execRedis(ctx, w, inst, sqlText)
	}
	if msg := s.fwRecheck(ctx, w, inst, sqlText); msg != "" { // SQL 防火墙：执行前复核，只认拒绝
		return failedSet(sqlText, msg)
	}
	if w.IsManual == 0 && nativeCreateTable(inst.DBType, sqlText) { // 建表工单永远原生执行：不管提交 / 定时时记的是哪种执行方式，也不受“强制使用 goInception”限制
		return s.execNative(ctx, w, inst, sqlText)
	}
	switch executor {
	case ExecGoInception:
		return s.execInception(ctx, w, inst, sqlText)
	case ExecNative:
		if w.IsManual == 0 && s.requireInception && inst.DBType != dbTypeMSSQL {
			return failedSet(sqlText, "已开启强制使用 goInception，不允许原生执行")
		}
		return s.execNative(ctx, w, inst, sqlText)
	}
	return failedSet(sqlText, fmt.Sprintf("未知的执行方式：%s", executor))
}

func (s *Service) execInception(ctx context.Context, w *model.SqlWorkflow, inst *model.Instance, sqlText string) *review.Set {
	cfg, ok := s.inceptionConfig()
	if !ok {
		return failedSet(sqlText, "未配置 goInception")
	}
	rows, err := s.newInception(cfg).Execute(ctx, s.target(inst), w.DBName, sqlText, goinception.ExecOpts{Backup: w.IsBackup})
	set := &review.Set{FullSQL: sqlText, Rows: rows}
	if err != nil && len(rows) == 0 {
		return failedSet(sqlText, "goInception Error: "+err.Error())
	}
	if err != nil {
		set.Error = fmt.Sprintf("goInception 连接中断：%s", err.Error())
	}
	// 任何一行出现错误/警告级别、且阶段状态不是“执行成功”，整体就是有异常。
	for _, r := range rows {
		if (r.ErrLevel == review.LevelWarning || r.ErrLevel == review.LevelError) && !strings.Contains(r.StageStatus, "Execute Successfully") {
			set.Error = fmt.Sprintf("Line %d has error/warning: %s", r.ID, r.ErrorMessage)
			break
		}
	}
	return set
}

// readOnlyFlagUnreliable 判断该类型实例的 read_only 变量不能用来判断“实例是否只读”（见 execNative）。
func readOnlyFlagUnreliable(dbType string) bool {
	return dbType == "selectdb" || dbType == "doris"
}

func (s *Service) execNative(ctx context.Context, w *model.SqlWorkflow, inst *model.Instance, sqlText string) *review.Set {
	eng, err := s.open(inst.DBType, s.repo.EngineConn(inst))
	if err != nil {
		return failedSet(sqlText, fmt.Sprintf("连接实例失败：%s", err.Error()))
	}
	defer eng.Close()
	wr, ok := eng.(engine.Writer)
	if !ok {
		return failedSet(sqlText, fmt.Sprintf("%s 类型的实例暂不支持原生执行", inst.DBType))
	}
	// SQL Server：PRINT 默认只是信息；系统配置 review.mssql_print_as_error=true 时 PRINT 按失败处理并回滚该单元
	if pp, ok := eng.(interface{ SetPrintAsError(bool) }); ok {
		pp.SetPrintAsError(cfgBool(s.repo, settings.KeyReviewMSSQLPrintAsError))
	}
	// 执行前核对实例是不是只读（主从切换后误连从库）。SelectDB / Doris 的 @@global.read_only / super_read_only 是兼容 MySQL 协议的固定值 1，
	// 并不表示真的只读，读它必然误拦（实测两个 SelectDB 实例都返回 1、1）：这两类跳过这一步，真不能写时由数据库在执行语句时报错。
	if !readOnlyFlagUnreliable(inst.DBType) {
		if ro, err := wr.ReadOnly(ctx); err != nil {
			return failedSet(sqlText, fmt.Sprintf("检查实例只读状态失败：%s", err.Error()))
		} else if ro {
			return failedSet(sqlText, "实例read_only=1，禁止执行变更语句!")
		}
	}
	stmts := splitFor(inst.DBType, sqlText)
	res, err := wr.ExecEach(ctx, w.DBName, stmts)
	if err != nil {
		return failedSet(sqlText, fmt.Sprintf("执行失败：%s", err.Error()))
	}
	mssql := inst.DBType == dbTypeMSSQL
	set := &review.Set{FullSQL: sqlText}
	id := 0
	if mssql {
		// SQL Server 的执行结果第一行是 USE [库]
		id++
		set.Rows = append(set.Rows, review.Row{ID: review.Flex(id), Stage: "EXECUTED", StageStatus: "Execute Successfully", ErrorMessage: "None",
			SQL: "USE [" + strings.ReplaceAll(w.DBName, "]", "]]") + "]", BackupDBName: "None", ExecuteTime: "0"})
	}
	for i, r := range res {
		id++
		row := review.Row{ID: review.Flex(id), Stage: "EXECUTED", StageStatus: "Execute Successfully", ErrorMessage: "None", SQL: r.SQL,
			AffectedRows: review.Flex(r.Affected), Sequence: fmt.Sprintf("0_0_%d", i), BackupDBName: "None", ExecuteTime: fmt.Sprintf("%.3f", r.ElapsedMs/1000)}
		switch {
		case r.Err != "":
			row.ErrLevel, row.StageStatus, row.ErrorMessage = review.LevelError, "Execute failed", r.Err
			if set.Error == "" {
				set.Error = fmt.Sprintf("Line %d has error/warning: %s", row.ID, r.Err)
			}
		case r.Skipped && mssql:
			// SQL Server：后续语句标记为“审核通过、未执行”
			row.Stage, row.StageStatus, row.ErrorMessage = "CHECKED", "Audit completed", "前序语句失败, 未执行"
			row.ExecuteTime = "0"
		case r.Skipped:
			row.Stage, row.ErrLevel, row.StageStatus, row.ErrorMessage = "SKIPPED", review.LevelWarning, "未执行", "前面的语句执行失败，已停止执行"
			row.ExecuteTime = "0"
		case r.Note != "":
			row.ErrorMessage = r.Note
		}
		set.Rows = append(set.Rows, row)
	}
	return set
}

// ---- 定时执行调度 ----

// RunDue 把到点的定时计划转入执行，返回启动的数量。
func (s *Service) RunDue() int {
	if !s.repo.ScheduleTableReady() {
		return 0
	}
	list, err := s.repo.DueSchedules(s.now(), 20)
	if err != nil {
		slog.Error("读取定时执行计划失败", "err", err)
		return 0
	}
	n := 0
	for _, sc := range list {
		// 提交时设定的计划，工单还在审批中：到点仍没批完就作废（留着会一直占着队列），工单日志里记一笔。
		if w, err := s.repo.GetWorkflow(sc.WorkflowID, false); err == nil && w != nil && w.Status == model.WfManReviewing {
			if claimed, err := s.repo.ClaimSchedule(sc.WorkflowID); err == nil && claimed {
				s.noteExpiredPlan(w, sc)
			}
			continue
		}
		// 删除成功的那个实例才有权执行：多个 dbtool 实例同时运行时不会重复触发。
		claimed, err := s.repo.ClaimSchedule(sc.WorkflowID)
		if err != nil || !claimed {
			continue
		}
		id, executor := sc.WorkflowID, sc.Executor
		if executor == "" {
			executor = s.defaultExecutorFor(id)
		}
		n++
		s.spawn(func() { s.run(id, nil, executor) })
	}
	return n
}

// noteExpiredPlan 提交时设定的计划到点时工单仍在审批中：计划作废，在工单日志里说明。
func (s *Service) noteExpiredPlan(w *model.SqlWorkflow, sc model.SqlSchedule) {
	_ = s.repo.InTx(func(tx Repo) error {
		au, err := tx.AuditByWorkflow(audit.TypeSQLReview, w.ID, false)
		if err != nil || au == nil {
			return err
		}
		return s.addLog(tx, au.AuditID, opSchedule, "定时执行", fmt.Sprintf("提交时设定的计划时间 %s 到了，工单仍在审批中，计划已作废；审批通过后请手动执行或重新设置定时", sc.RunAt.Format("2006-01-02 15:04")), nil)
	})
}

func (s *Service) defaultExecutorFor(id uint) string {
	if w, err := s.repo.GetWorkflow(id, false); err == nil && w != nil {
		if _, def := s.executorOptionsFor(w, s.createTableSQL(s.repo, w)); def != "" {
			return def
		}
	}
	return ExecNative
}

// StartScheduler 每隔 interval 检查一次到点的定时计划，直到 ctx 结束。
func (s *Service) StartScheduler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	every := s.recoverEvery
	if every <= 0 {
		every = recoverInterval
	}
	// 启动后先扫一次（上次进程被杀留下的“执行中”工单），之后每隔 every 一次。
	lastRecover := s.now()
	s.RecoverStuck()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RunDue()
			if s.now().Sub(lastRecover) >= every {
				lastRecover = s.now()
				s.RecoverStuck()
			}
		}
	}
}
