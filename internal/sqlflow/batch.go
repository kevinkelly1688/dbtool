// @author: kevinkelly
// @license: Apache Licence
// @file: batch.go
// @time: 2026/10/28

package sqlflow

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/review"
	"dbtool/internal/settings"
)

// 批量上线：同一段 SQL 在工单关联的多个实例上执行，规则如下：
//   - 每个实例各自执行整段 SQL，单个实例失败不影响其余实例，逐实例回报；
//   - 结果行带 instance_id / instance_name，末尾追加一条“批量执行汇总”行；
//   - 失败的实例可以重试（在该实例上重新执行整段 SQL）或关闭（不再计入成败）；
//   - 只有 SQL Server 实例并发执行（默认 4 路，review.batch_parallel_workers 可调，最多 16），其余类型严格串行。

const (
	defaultBatchWorkers = 4
	maxBatchWorkers     = 16
)

// parseBatchIDs 解析 batch_instance_ids（逗号分隔），忽略空白与非法值；该字段可被 admin/API 直接写入，脏数据不应影响列表等共用接口。
func parseBatchIDs(raw *string) []uint {
	if raw == nil {
		return nil
	}
	var out []uint
	for _, tok := range strings.Split(*raw, ",") {
		if n, err := strconv.ParseUint(strings.TrimSpace(tok), 10, 32); err == nil && n > 0 {
			out = append(out, uint(n))
		}
	}
	return out
}

// workflowInstanceIDs 工单涉及的全部实例：批量工单取实例列表，普通工单取主实例。
func workflowInstanceIDs(w *model.SqlWorkflow) map[uint]bool {
	out := map[uint]bool{}
	for _, id := range parseBatchIDs(w.BatchInstanceIDs) {
		out[id] = true
	}
	if len(out) == 0 && w.InstanceID != 0 {
		out[w.InstanceID] = true
	}
	return out
}

func overlaps(a, b map[uint]bool) bool {
	for id := range a {
		if b[id] {
			return true
		}
	}
	return false
}

// batchWorkers 并发度：review.batch_parallel_workers，默认 4，不超过实例数与 16。
func (s *Service) batchWorkers(n int) int {
	w := defaultBatchWorkers
	if v, err := strconv.Atoi(cfgStr(s.repo, settings.KeyReviewBatchWorkers)); err == nil && v > 0 {
		w = v
	}
	if w > maxBatchWorkers {
		w = maxBatchWorkers
	}
	if w > n {
		w = n
	}
	if w < 1 {
		w = 1
	}
	return w
}

// batchInstances 按提交时的顺序载入批量工单的实例；已被删除的实例单独列出（不能静默少执行）。
func (s *Service) batchInstances(w *model.SqlWorkflow) (insts []*model.Instance, missing []uint) {
	for _, id := range parseBatchIDs(w.BatchInstanceIDs) {
		in, err := s.repo.GetInstance(id)
		if err != nil || in == nil {
			missing = append(missing, id)
			continue
		}
		insts = append(insts, in)
	}
	return insts, missing
}

func tag(rows []review.Row, in *model.Instance) []review.Row {
	for i := range rows {
		rows[i].InstanceID, rows[i].InstanceName = review.Flex(in.ID), in.InstanceName
	}
	return rows
}

type instOutcome struct {
	rows     []review.Row
	failed   bool
	elapsed  float64
	fileName string // 数据导出重试后新的压缩包名
}

// runOnInstance 在一个实例上执行整段 SQL，永远返回结果（含 panic）。
func (s *Service) runOnInstance(ctx context.Context, w *model.SqlWorkflow, in *model.Instance, sqlText string) (oc instOutcome) {
	if w.IsOfflineExport != 0 {
		return s.retryExportInstance(ctx, w, in, sqlText)
	}
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("批量工单实例执行协程异常", "workflow", w.ID, "instance", in.InstanceName, "panic", r, "stack", string(debug.Stack()))
			oc = instOutcome{failed: true, rows: tag([]review.Row{{Stage: "Execute failed", ErrLevel: review.LevelError, StageStatus: "异常终止",
				ErrorMessage: fmt.Sprintf("执行异常: %v", r), SQL: sqlText}}, in)}
		}
		oc.elapsed = time.Since(start).Seconds()
	}()
	if msg := s.fwRecheck(ctx, w, in, sqlText); msg != "" { // SQL 防火墙：每个实例各自复核，被拒绝的实例失败，其余照常
		return instOutcome{failed: true, rows: tag([]review.Row{{Stage: "Execute failed", ErrLevel: review.LevelError, StageStatus: "异常终止",
			ErrorMessage: msg, SQL: sqlText}}, in)}
	}
	cp := *w
	cp.Instance = in
	set := s.execNative(ctx, &cp, in, sqlText)
	oc.failed = set.Error != "" || set.Warning != ""
	oc.rows = tag(append([]review.Row(nil), set.Rows...), in)
	if len(oc.rows) == 0 {
		oc.rows = tag([]review.Row{{Stage: "Executed", StageStatus: "执行完成", ErrorMessage: "执行未返回明细", SQL: sqlText,
			ExecuteTime: strconv.FormatFloat(oc.elapsed, 'f', 3, 64)}}, in)
	}
	return oc
}

// execBatch 在所有实例上执行，返回合并后的结果集：每个实例的结果行 + 汇总行。
func (s *Service) execBatch(ctx context.Context, w *model.SqlWorkflow, sqlText string) *review.Set {
	set := &review.Set{FullSQL: sqlText}
	insts, missing := s.batchInstances(w)
	if len(insts) == 0 {
		msg := "批量工单未关联任何有效实例。"
		set.Rows = []review.Row{{ID: 1, Stage: "Execute failed", ErrLevel: review.LevelError, StageStatus: "异常终止", ErrorMessage: msg, SQL: sqlText}}
		set.Error = msg
		return set
	}
	var rows []review.Row
	failedNames := []string{}
	for _, id := range missing {
		rows = append(rows, review.Row{Stage: "Execute failed", ErrLevel: review.LevelError, StageStatus: "异常终止",
			ErrorMessage: fmt.Sprintf("实例(id:%d)已不存在，跳过执行。", id), SQL: sqlText, InstanceID: review.Flex(id), InstanceName: fmt.Sprintf("(已删除 id:%d)", id)})
		failedNames = append(failedNames, fmt.Sprintf("已删除 id:%d", id))
	}

	parallel := len(insts) > 1
	for _, in := range insts {
		if in.DBType != dbTypeMSSQL {
			parallel = false
		}
	}
	outs := make([]instOutcome, len(insts))
	s.progTotal(w.ID, len(insts)+len(missing))
	for range missing {
		s.progDone(w.ID)
	}
	if parallel {
		sem := make(chan struct{}, s.batchWorkers(len(insts)))
		var wg sync.WaitGroup
		for i, in := range insts {
			wg.Add(1)
			go func(i int, in *model.Instance) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				outs[i] = s.runOnInstance(ctx, w, in, sqlText)
				s.progDone(w.ID)
			}(i, in)
		}
		wg.Wait()
	} else {
		for i, in := range insts {
			outs[i] = s.runOnInstance(ctx, w, in, sqlText)
			s.progDone(w.ID)
		}
	}
	success := 0
	for i, in := range insts { // 结果按实例顺序归并，与串行时的行序一致
		rows = append(rows, outs[i].rows...)
		if outs[i].failed {
			failedNames = append(failedNames, in.InstanceName)
		} else {
			success++
		}
	}
	total := len(parseBatchIDs(w.BatchInstanceIDs))
	if total == 0 {
		total = len(insts)
	}
	failed := total - success
	rows = append(rows, summaryRow(rows, sqlText, false, ""))
	for i := range rows {
		rows[i].ID = review.Flex(i + 1)
	}
	set.Rows = rows
	if failed > 0 {
		set.Warning = fmt.Sprintf("%d/%d 个实例执行失败：%s，详见执行结果。", failed, total, strings.Join(failedNames, "、"))
	}
	if success == 0 {
		set.Error = "所有实例均执行失败。"
	}
	return set
}

// ---- 按实例聚合状态（执行结果里每行都带 instance_id） ----

type instState struct {
	ID     uint
	Name   string
	Closed bool
	Failed bool
}

// instanceStates 按出现顺序聚合每个实例的状态；汇总行与没有 instance_id 的行不参与。
func instanceStates(rows []review.Row) []*instState {
	idx := map[uint]*instState{}
	var order []*instState
	for _, r := range rows {
		if r.IsSummary || r.InstanceID == 0 {
			continue
		}
		id := uint(r.InstanceID)
		st := idx[id]
		if st == nil {
			name := r.InstanceName
			if name == "" {
				name = strconv.Itoa(int(id))
			}
			st = &instState{ID: id, Name: name}
			idx[id] = st
			order = append(order, st)
		}
		if r.BatchClosed {
			st.Closed = true
		}
		if r.ErrLevel == review.LevelError { // 一条 SQL 一行，有一行出错就算该实例失败
			st.Failed = true
		}
	}
	return order
}

// summaryRow 根据当前各实例状态重建汇总行。
// 导出工单的汇总用“批量导出汇总”，并带上压缩包文件名。
func summaryRow(rows []review.Row, sqlText string, export bool, fileName string) review.Row {
	states := instanceStates(rows)
	var closed, failed, success []string
	for _, st := range states {
		switch {
		case st.Closed:
			closed = append(closed, st.Name)
		case st.Failed:
			failed = append(failed, st.Name)
		default:
			success = append(success, st.Name)
		}
	}
	// 执行 / 导出各写一整句，不拼“批量”+“执行”碎片：这些文字存进结果行，显示时按整句查英文目录。
	status := "批量执行汇总"
	msg := fmt.Sprintf("批量执行汇总：共 %d 个实例，成功 %d 个，失败 %d 个，已关闭 %d 个。", len(states), len(success), len(failed), len(closed))
	if export {
		status = "批量导出汇总"
		msg = fmt.Sprintf("批量导出汇总：共 %d 个实例，成功 %d 个，失败 %d 个，已关闭 %d 个。", len(states), len(success), len(failed), len(closed))
	}
	if export && fileName != "" {
		msg += fmt.Sprintf("保存文件: %s。", fileName)
	}
	if len(failed) > 0 {
		msg += fmt.Sprintf("失败实例（%s）待重试或关闭。", strings.Join(failed, "、"))
	}
	if len(closed) > 0 {
		msg += fmt.Sprintf("已关闭实例（%s）不计入工单成败，需人工确认处理。", strings.Join(closed, "、"))
	}
	if export {
		if len(failed) > 0 {
			msg += "失败实例未包含在压缩包中，已成功实例的文件仍可下载。"
		}
	} else if len(failed) == 0 && len(success) > 0 {
		msg += "剩余实例均已执行成功。"
	} else if len(failed) > 0 && len(success) > 0 {
		msg += fmt.Sprintf("注意：已有 %d 个实例的变更已生效，失败实例与其余实例状态不一致，请人工核对。", len(success))
	}
	lv := review.LevelOK
	if len(failed) > 0 {
		lv = review.LevelWarning
	}
	return review.Row{Stage: "Executed", ErrLevel: review.Flex(lv), StageStatus: status, ErrorMessage: msg, SQL: sqlText,
		InstanceName: "汇总", IsSummary: true}
}

// refreshBatch 重建汇总行、重新编号，并按“失败实例是否已全部处理”回算工单状态：
// 没有待处理的失败实例、且至少有一个实例真的执行成功，才算正常结束；全部关闭时不置为成功，避免“一路关闭”把工单洗成已完成。
func refreshBatch(rows []review.Row, sqlText string, export bool, fileName string) ([]review.Row, string) {
	kept := make([]review.Row, 0, len(rows)+1)
	for _, r := range rows {
		if !r.IsSummary {
			kept = append(kept, r)
		}
	}
	kept = append(kept, summaryRow(kept, sqlText, export, fileName))
	for i := range kept {
		kept[i].ID = review.Flex(i + 1)
	}
	failed, success := 0, 0
	for _, st := range instanceStates(kept) {
		switch {
		case st.Closed:
		case st.Failed:
			failed++
		default:
			success++
		}
	}
	if failed == 0 && success > 0 {
		return kept, model.WfFinish
	}
	return kept, model.WfException
}

// BatchInstanceView 是详情页里一个实例的状态。
type BatchInstanceView struct {
	ID      uint   `json:"id"`
	Name    string `json:"name"`
	Failed  bool   `json:"failed"`
	Closed  bool   `json:"closed"`
	Success bool   `json:"success"`

	Address string  `json:"address,omitempty"` // host:port
	DBType  string  `json:"db_type,omitempty"`
	Elapsed float64 `json:"elapsed"`        // 该实例上各语句耗时之和（秒）
	Stmts   int     `json:"stmts"`          // 结果行数（语句数）
	Note    string  `json:"note,omitempty"` // 说明：失败时是第一条错误信息，成功时是影响行数 / 导出文件
}

// enrichBatchViews 给各实例补上地址、类型、耗时和说明（从实例信息与执行结果里算，不连库）。
func enrichBatchViews(views []BatchInstanceView, insts []*model.Instance, rows []review.Row) []BatchInstanceView {
	byID := map[uint]*model.Instance{}
	for _, in := range insts {
		byID[in.ID] = in
	}
	type agg struct {
		elapsed  float64
		stmts    int
		affected int64
		firstErr string
		file     string
	}
	sums := map[uint]*agg{}
	for _, r := range rows {
		if r.IsSummary || r.InstanceID == 0 {
			continue
		}
		a := sums[uint(r.InstanceID)]
		if a == nil {
			a = &agg{}
			sums[uint(r.InstanceID)] = a
		}
		a.stmts++
		if f, err := strconv.ParseFloat(strings.TrimSpace(r.ExecuteTime), 64); err == nil {
			a.elapsed += f
		}
		a.affected += int64(r.AffectedRows)
		if r.ErrLevel == review.LevelError && a.firstErr == "" && r.ErrorMessage != "" && r.ErrorMessage != "None" {
			a.firstErr = r.ErrorMessage
		}
		if strings.HasPrefix(r.ErrorMessage, "导出文件: ") { // i18n-ignore 与存库的结果文本做匹配
			a.file = strings.TrimPrefix(r.ErrorMessage, "导出文件: ") // i18n-ignore 与存库的结果文本做匹配
		}
	}
	for i := range views {
		v := &views[i]
		if in := byID[v.ID]; in != nil {
			v.Address, v.DBType = fmt.Sprintf("%s:%d", in.Host, in.Port), in.DBType
		}
		a := sums[v.ID]
		if a == nil {
			continue
		}
		v.Elapsed, v.Stmts = a.elapsed, a.stmts
		switch {
		case v.Closed:
			v.Note = "已关闭，不再计入工单成败"
		case a.firstErr != "":
			v.Note = a.firstErr
		case a.file != "":
			v.Note = fmt.Sprintf("%d 行 · %s", a.affected, a.file)
		default:
			v.Note = fmt.Sprintf("%d 条语句，影响 %d 行", a.stmts, a.affected)
		}
	}
	return views
}

func batchViews(rows []review.Row) []BatchInstanceView {
	var out []BatchInstanceView
	for _, st := range instanceStates(rows) {
		out = append(out, BatchInstanceView{ID: st.ID, Name: st.Name, Failed: st.Failed && !st.Closed, Closed: st.Closed, Success: !st.Failed && !st.Closed})
	}
	return out
}

// canOperateBatch 重试/关闭失败实例的用户维度权限：与执行工单一致（资源组执行权限且在组内，或提交人本人且有执行权限），
// 但没有执行工单的状态门槛——重试和关闭恰恰发生在工单已经执行完（执行有异常/已结束）之后。
func (pm perms) canOperateBatch(p audit.Actor, w *model.SqlWorkflow) bool {
	if pm.groups[w.GroupID] && pm.executeRG {
		return true
	}
	return w.Engineer == p.Username && pm.execute
}

func (s *Service) batchPrecheck(p audit.Actor, id uint) (*model.SqlWorkflow, perms, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, pm, err
	}
	w, err := s.load(id, false)
	if err != nil {
		return nil, pm, err
	}
	if !pm.canView(p, w) {
		return nil, pm, notFound("工单不存在")
	}
	if !pm.canOperateBatch(p, w) {
		return nil, pm, forbid("你无权操作当前工单！")
	}
	if !w.IsBatch() {
		return nil, pm, bad("该工单不是批量工单")
	}
	return w, pm, nil
}

func loadRows(tx Repo, id uint) (*model.SqlWorkflowContent, []review.Row, error) {
	c, err := tx.GetContent(id)
	if err != nil {
		return nil, nil, err
	}
	if c == nil {
		return nil, nil, notFound("工单内容不存在")
	}
	rows, err := review.ParseRows(c.ExecuteResult)
	if err != nil {
		return nil, nil, bad("执行结果无法解析")
	}
	return c, rows, nil
}

func stateOf(states []*instState, id uint) *instState {
	for _, st := range states {
		if st.ID == id {
			return st
		}
	}
	return nil
}

// CloseBatchInstance 关闭失败的实例：该实例不再计入工单成败，也不能再重试。
func (s *Service) CloseBatchInstance(p audit.Actor, id, instID uint) (string, string, error) {
	if _, _, err := s.batchPrecheck(p, id); err != nil {
		return "", "", err
	}
	var msg, status, info string
	err := s.repo.InTx(func(tx Repo) error {
		w, au, err := lockedWorkflow(tx, id)
		if err != nil {
			return err
		}
		if w.Status != model.WfException && w.Status != model.WfFinish {
			return conflict(fmt.Sprintf("当前工单状态（%s）不允许该操作", StatusLabel(w.Status)))
		}
		info = wfAuditInfo(w)
		c, rows, err := loadRows(tx, id)
		if err != nil {
			return err
		}
		st := stateOf(instanceStates(rows), instID)
		switch {
		case st == nil:
			return bad("该实例不属于本工单的执行结果")
		case st.Closed:
			return bad("该实例已关闭")
		}
		for i := range rows {
			if uint(rows[i].InstanceID) == instID {
				rows[i].BatchClosed = true
			}
		}
		var sqlText string
		if sc, err := tx.GetContent(id); err == nil && sc != nil {
			sqlText = sc.SQLContent
		}
		fn := ""
		if w.FileName != nil {
			fn = *w.FileName
		}
		rows, status = refreshBatch(rows, sqlText, w.IsOfflineExport != 0, fn)
		c.ExecuteResult = (&review.Set{Rows: rows}).JSON()
		if err := tx.SaveContent(c); err != nil {
			return err
		}
		w.Status = status
		if err := tx.SaveWorkflow(w); err != nil {
			return err
		}
		msg = fmt.Sprintf("实例 %s 已关闭。", st.Name)
		return s.addLog(tx, au.AuditID, opExecute, "执行工单", fmt.Sprintf("关闭实例：%s", msg), &p)
	})
	if err == nil {
		s.sysAudit(p, "批量实例关闭", info+"，"+msg) // i18n-ignore 审计动作名
	}
	return msg, status, translate(err)
}

// RetryBatchInstance 重试失败的实例：在该实例上重新执行整段 SQL，用新结果替换它原有的结果行。
// 重试期间工单置为“执行中”，防止并发操作改写同一份结果。
func (s *Service) RetryBatchInstance(ctx context.Context, p audit.Actor, id, instID uint) (string, string, error) {
	w0, _, err := s.batchPrecheck(p, id)
	if err != nil {
		return "", "", err
	}
	_ = w0
	var w *model.SqlWorkflow
	var au *model.WorkflowAudit
	var target *model.Instance
	var sqlText string
	var prevStatus string
	err = s.repo.InTx(func(tx Repo) error {
		var err error
		if w, au, err = lockedWorkflow(tx, id); err != nil {
			return err
		}
		if w.Status != model.WfException && w.Status != model.WfFinish {
			return conflict(fmt.Sprintf("当前工单状态（%s）不允许该操作", StatusLabel(w.Status)))
		}
		c, rows, err := loadRows(tx, id)
		if err != nil {
			return err
		}
		sqlText = c.SQLContent
		st := stateOf(instanceStates(rows), instID)
		switch {
		case st == nil:
			return bad("该实例不属于本工单的执行结果")
		case st.Closed:
			return bad("该实例已关闭，无法重试")
		case !st.Failed:
			return bad("该实例已执行成功，无需重试")
		}
		in, err := tx.GetInstance(instID)
		if err != nil {
			return err
		}
		listed := false
		for _, bid := range parseBatchIDs(w.BatchInstanceIDs) {
			listed = listed || bid == instID
		}
		if in == nil || !listed {
			return bad("该实例已不存在，无法重试")
		}
		target = in
		if msg, err := s.ddlConflict(tx, w); err != nil {
			return err
		} else if msg != "" {
			return conflict(msg)
		}
		prevStatus = w.Status
		w.Status = model.WfExecuting
		if err := tx.SaveWorkflow(w); err != nil {
			return err
		}
		return s.addLog(tx, au.AuditID, opExecute, "执行工单", fmt.Sprintf("重试实例：%s", in.InstanceName), &p)
	})
	if err != nil {
		return "", "", translate(err)
	}

	rctx, cancel := context.WithTimeout(ctx, maxRunTime)
	defer cancel()
	if s.phaseOn("Execute") {
		s.notifyRetrying(w, au, target.InstanceName)
	}
	oc := s.runOnInstance(rctx, w, target, sqlText)

	var msg, status string
	err = s.repo.InTx(func(tx Repo) error {
		w2, au2, err := lockedWorkflow(tx, id)
		if err != nil {
			return err
		}
		c, rows, err := loadRows(tx, id)
		if err != nil {
			return err
		}
		kept := rows[:0]
		for _, r := range rows {
			if uint(r.InstanceID) != instID {
				kept = append(kept, r)
			}
		}
		kept = append(kept, oc.rows...)
		fn := oc.fileName
		if fn == "" && w2.FileName != nil {
			fn = *w2.FileName
		}
		kept, status = refreshBatch(kept, sqlText, w2.IsOfflineExport != 0, fn)
		c.ExecuteResult = (&review.Set{Rows: kept}).JSON()
		if err := tx.SaveContent(c); err != nil {
			return err
		}
		now := s.now()
		w2.Status, w2.FinishTime = status, &now
		if oc.fileName != "" {
			fn := oc.fileName
			w2.FileName = &fn
		}
		if err := tx.SaveWorkflow(w2); err != nil {
			return err
		}
		if oc.failed {
			msg = fmt.Sprintf("实例 %s 重试失败，可再次重试或关闭。", target.InstanceName)
		} else {
			msg = fmt.Sprintf("实例 %s 重试成功。", target.InstanceName)
		}
		w, au = w2, au2
		return s.addLog(tx, au2.AuditID, opFinish, "执行结束", fmt.Sprintf("重试结果：%s 工单状态：%s", msg, StatusLabel(status)), nil)
	})
	if err != nil {
		// 结果写不进去：不能让工单一直停在“执行中”
		slog.Error("保存批量工单重试结果失败", "workflow", id, "err", err)
		_ = s.repo.InTx(func(tx Repo) error {
			w3, err := tx.GetWorkflow(id, true)
			if err != nil || w3 == nil || w3.Status != model.WfExecuting {
				return err
			}
			w3.Status = prevStatus
			if prevStatus == "" {
				w3.Status = model.WfException
			}
			return tx.SaveWorkflow(w3)
		})
		return "", "", translate(err)
	}
	if s.phaseOn("Execute") {
		s.notifyRetried(w, au, target.InstanceName, !oc.failed)
	}
	s.sysAudit(p, "批量实例重试", wfAuditInfo(w)+"，"+msg) // i18n-ignore 审计动作名
	return msg, status, nil
}
