// @author: kevinkelly
// @license: Apache Licence
// @file: sql_handlers.go
// @time: 2026/10/28

package server

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/i18n"
	"dbtool/internal/model"
	"dbtool/internal/query"
	"dbtool/internal/review"
	"dbtool/internal/sqlflow"
)

// sqlEnabled 在没有配置 SQL 工单服务时（例如测试里的最小仓库）让相关路由返回 404。
func (s *Server) sqlEnabled() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.sql == nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// maxBody 工单相关请求体的上限（SQL 文本本身另有 200 万字符的限制，这里挡住更大的恶意请求体）。
const maxBody = 8 << 20

func limitBody(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBody)
}

// fillDisplay 令牌（JWT）里没有显示名：工单要记录提交人/审批人的显示名，这里补上。
func (s *Server) fillDisplay() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := principalOf(c)
		if p.Display == "" && p.Username != "" {
			if u, err := s.repo.GetUserByName(p.Username); err == nil && u != nil && u.Display != "" {
				p.Display = u.Display
			} else {
				p.Display = p.Username
			}
			c.Set(principalKey, p)
		}
		c.Next()
	}
}

var localLayouts = []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02T15:04"}

// parseLocalTime 解析页面传来的本地时间（无时区）；空串返回 nil。
func parseLocalTime(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	for _, l := range localLayouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return &t, nil
		}
	}
	return nil, query.NewError(400, "bad_request", "时间格式应为 YYYY-MM-DD HH:MM")
}

// sqlStatusClass 把工单状态映射成样式类名。
func sqlStatusClass(st string) string {
	switch st {
	case model.WfManReviewing:
		return "waiting"
	case model.WfReviewPass, model.WfFinish:
		return "passed"
	case model.WfTimingTask, model.WfQueuing, model.WfExecuting:
		return "running"
	case model.WfAutoReviewWrong, model.WfException:
		return "rejected"
	}
	return "aborted"
}

// ---- 界面语言：结果行 / 提示的翻译 ----
//
// 检查与执行结果里的文字有两种来源：goInception / 数据库引擎返回的原文（不是我们的字面量，查不到目录，原样保留），
// 以及我们自己生成的中文（规则提示、阶段状态……，库里存的也是中文）。后者在输出时按请求语言翻译。
// 多条提示常常用换行或“。”拼在同一个字符串里，整段交给 i18n.TP 会让带 %s 的格式串跨行匹配、译文错乱，所以先逐行、再逐句翻译。

// sqlTrText 翻译一段我们自己拼出来的提示（可能是多行 / 多句）；中文界面原样返回。
func sqlTrText(c *gin.Context, s string) string {
	if lang := langOf(c); lang != i18n.ZH {
		return sqlTrTextLang(lang, s)
	}
	return s
}

func sqlTrTextLang(lang, s string) string {
	if s == "" || !sqlHasHan(s) {
		return s
	}
	if strings.Contains(s, "\n") {
		lines := strings.Split(s, "\n")
		changed := false
		for i, l := range lines {
			if out := sqlTrLine(lang, l); out != l {
				lines[i], changed = out, true
			}
		}
		if changed {
			return strings.Join(lines, "\n")
		}
		return i18n.TP(lang, s) // 逐行都没命中：可能整段本身就是一条多行的格式串
	}
	return sqlTrLine(lang, s)
}

// sqlTrLine 翻译一行：整行按目录（含格式串）翻译；不命中就按“。”切成句子逐句翻译（整句的键优先于拆开的碎片）。
// 没有任何命中时原样返回。
func sqlTrLine(lang, s string) string {
	if s == "" {
		return s
	}
	out, changed := sqlTrSentences(lang, s)
	if !changed {
		return s
	}
	return out
}

func sqlTrSentences(lang, s string) (string, bool) {
	if out := i18n.TP(lang, s); out != s {
		return out, true
	}
	var ends []int // 每个“。”之后（不含末尾）的切点
	for i, r := range s {
		if r == '。' && i+len("。") < len(s) {
			ends = append(ends, i+len("。"))
		}
	}
	if len(ends) == 0 {
		return s, false
	}
	join := func(head string, headDone bool, k int) (string, bool) {
		rest := s[k:]
		trimmed := strings.TrimLeft(rest, " ")
		lead := rest[:len(rest)-len(trimmed)]
		tail, tailDone := sqlTrSentences(lang, trimmed)
		if lead == "" && (headDone || tailDone) {
			lead = " " // 中文句子之间不留空格，译文里补一个
		}
		return head + lead + tail, headDone || tailDone
	}
	for j := len(ends) - 1; j >= 0; j-- { // 从最长的前缀开始
		head := s[:ends[j]]
		if out := i18n.TP(lang, head); out != head {
			return join(out, true, ends[j])
		}
	}
	return join(s[:ends[0]], false, ends[0]) // 第一句没有命中：保留原文，继续处理后面的句子
}

// sqlTrLogText 翻译工单日志里的操作详情。别的模块写日志时常用“固定前缀（以冒号结尾）+ 动态内容”的拼法，
// 整句查不到时，把目录里最长的、以冒号结尾的前缀翻译出来，后面的动态内容原样保留。
func sqlTrLogText(lang, s string) string {
	if out := sqlTrTextLang(lang, s); out != s || s == "" {
		return out
	}
	best := ""
	for k := range i18n.Messages(lang) {
		if len(k) > len(best) && strings.HasPrefix(s, k) && (strings.HasSuffix(k, "：") || strings.HasSuffix(k, ": ")) &&
			!strings.ContainsAny(k, "%{") && sqlHasHan(k) {
			best = k
		}
	}
	if best == "" {
		return s
	}
	return i18n.TP(lang, best) + s[len(best):]
}

// sqlHasHan 判断字符串里有没有汉字（没有就不用查目录）。
func sqlHasHan(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}

// sqlTrRows 翻译结果行里我们自己产出的中文（阶段状态、提示、汇总行的实例名）；返回新切片，不改动入参。
func sqlTrRows(c *gin.Context, rows []review.Row) []review.Row {
	lang := langOf(c)
	if lang == i18n.ZH || len(rows) == 0 {
		return rows
	}
	out := make([]review.Row, len(rows))
	for i, r := range rows {
		r.Stage = i18n.TP(lang, r.Stage)
		r.StageStatus = i18n.TP(lang, r.StageStatus)
		r.ErrorMessage = sqlTrTextLang(lang, r.ErrorMessage)
		r.InstanceName = i18n.TP(lang, r.InstanceName)
		out[i] = r
	}
	return out
}

// sqlTrDetail 把工单详情里 Go 产出的中文（状态名、审批时间线、日志、批量实例说明……）翻译成请求语言。
// 详情是每次请求新读出来的，直接改字段；日志切片先复制一份，避免改到仓库缓存的数据。
func sqlTrDetail(c *gin.Context, d *sqlflow.Detail) {
	lang := langOf(c)
	if lang == i18n.ZH || d == nil {
		return
	}
	tp := func(s string) string { return i18n.TP(lang, s) }
	d.StatusLabel, d.SyntaxLabel, d.Flow = tp(d.StatusLabel), tp(d.SyntaxLabel), tp(d.Flow)
	d.ParseError, d.StuckHint = tp(d.ParseError), tp(d.StuckHint)
	d.ReviewRows, d.ExecRows = sqlTrRows(c, d.ReviewRows), sqlTrRows(c, d.ExecRows)
	for i := range d.Steps {
		st := &d.Steps[i]
		st.Title, st.Who, st.Remark = tp(st.Title), tp(st.Who), sqlTrLogText(lang, st.Remark)
	}
	if len(d.Logs) > 0 {
		logs := make([]model.WorkflowLog, len(d.Logs))
		copy(logs, d.Logs)
		for i := range logs {
			logs[i].OperatorDisplay = tp(logs[i].OperatorDisplay)
			logs[i].OperationTypeDesc = tp(logs[i].OperationTypeDesc)
			logs[i].OperationInfo = sqlTrLogText(lang, logs[i].OperationInfo)
		}
		d.Logs = logs
	}
	for i := range d.BatchInstances {
		d.BatchInstances[i].Note = sqlTrTextLang(lang, d.BatchInstances[i].Note)
	}
	for i := range d.Executors {
		d.Executors[i].Label = tp(d.Executors[i].Label)
	}
}

// ---- 页面 ----

func (s *Server) sqlNewPage(c *gin.Context) {
	a := actorOf(c)
	if !s.sql.CanSubmit(a) {
		s.renderForbidden(c, "你没有提交 SQL 工单的权限（缺少 sqlworkflow.submit），请联系管理员。")
		return
	}
	p := principalOf(c)
	s.render(c, http.StatusOK, "sql_new.html", pageData{Title: "提交 SQL 工单", Nav: "sql", Username: p.Username, Display: p.Display,
		CSRF: sessionOf(c).CSRF, Data: gin.H{"IsSuperuser": a.IsSuperuser}, Nav2: s.nav(a)})
}

func (s *Server) sqlExportNewPage(c *gin.Context) {
	a := actorOf(c)
	if !s.sql.CanExport(a) {
		s.renderForbidden(c, "你没有提交数据导出工单的权限（缺少 export.submit），请联系管理员。")
		return
	}
	p := principalOf(c)
	view, _ := s.sql.ExportViewOf(a) // 取不到就不显示“导出工单列表”入口
	s.render(c, http.StatusOK, "sql_export_new.html", pageData{Title: "提交数据导出工单", Nav: "export", Username: p.Username, Display: p.Display,
		CSRF: sessionOf(c).CSRF, Data: gin.H{"CanViewList": view.Single || view.Batch}, Nav2: s.nav(a)})
}

type sqlDetailPage struct {
	*sqlflow.Detail
	Rows       []review.Row // 页面主表：已结束显示执行结果，否则显示检查结果
	ShowExec   bool
	Runnable   bool // 是否显示“执行”区域
	Pending    bool // 状态还会变化（排队/执行中），页面自动刷新
	DefaultRun string
	MinRunAt   string
}

func (s *Server) sqlDetailPage(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		s.renderPageError(c, query.NewError(404, "not_found", "工单不存在"))
		return
	}
	a := actorOf(c)
	d, err := s.sql.Get(a, uint(id))
	if err != nil {
		s.renderPageError(c, err)
		return
	}
	s.applyDetailButtons(c, d)
	s.applyExtraButtons(c, d)
	sqlTrDetail(c, d)
	pg := sqlDetailPage{Detail: d, Rows: d.ReviewRows}
	if d.Workflow.Status == model.WfFinish || d.Workflow.Status == model.WfException {
		pg.ShowExec = true
		if len(d.ExecRows) > 0 {
			pg.Rows = d.ExecRows
		}
	}
	pg.Runnable = d.CanExecute
	pg.Pending = d.Workflow.Status == model.WfQueuing || d.Workflow.Status == model.WfExecuting
	pg.MinRunAt = time.Now().Add(time.Minute).Format("2006-01-02T15:04")
	p := principalOf(c)
	s.render(c, http.StatusOK, "sql_detail.html", pageData{Title: "SQL 工单详情", Nav: "sql", Username: p.Username, Display: p.Display,
		CSRF: sessionOf(c).CSRF, Data: pg, Nav2: s.nav(a)})
}

// ---- JSON 接口 ----

func (s *Server) apiSQLOptions(c *gin.Context) {
	o, err := s.sql.SubmitOptions(actorOf(c))
	if err != nil {
		writeErr(c, err)
		return
	}
	if langOf(c) != i18n.ZH { // 导出格式名里有中文（如“SQL（INSERT 语句）”）：按请求语言翻译
		fmts := append([]sqlflow.Option(nil), o.Export.Formats...)
		for i := range fmts {
			fmts[i].Label = trMsg(c, fmts[i].Label)
		}
		o.Export.Formats = fmts
	}
	groups := make([]gin.H, 0, len(o.Groups))
	for _, g := range o.Groups {
		ins := o.Instances[g.GroupID]
		if ins == nil {
			ins = []sqlflow.InstanceOption{}
		}
		exp := o.ExportInstances[g.GroupID]
		if exp == nil {
			exp = []sqlflow.InstanceOption{}
		}
		groups = append(groups, gin.H{"id": g.GroupID, "name": g.GroupName, "instances": ins, "export_instances": exp})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"groups": groups, "inception": o.Inception, "native_allowed": o.Native, "batch": o.Batch, "export": o.Export}})
}

func (s *Server) apiSQLDatabases(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var dbs []string
	var err error
	if c.Query("export") == "1" { // 数据导出页：只列出有查询权限的库
		dbs, err = s.sql.ExportDatabases(c.Request.Context(), actorOf(c), id)
	} else {
		dbs, err = s.sql.Databases(c.Request.Context(), actorOf(c), id)
	}
	if err != nil {
		writeErr(c, err)
		return
	}
	if dbs == nil {
		dbs = []string{}
	}
	c.JSON(http.StatusOK, gin.H{"data": dbs})
}

type sqlCheckReq struct {
	InstanceID  uint   `json:"instance_id"`
	InstanceIDs []uint `json:"instance_ids"` // 批量上线：多个实例
	DBName      string `json:"db_name"`
	SQL         string `json:"sql"`
	SkipCheck   bool   `json:"skip_check"`
	Export      bool   `json:"export"`       // 数据导出：检查只读查询与待导出行数
	ExportMerge bool   `json:"export_merge"` // 批量导出合并成一个文件：同时核对各实例返回的列是否一致
}

func (s *Server) apiSQLCheck(c *gin.Context) {
	limitBody(c)
	var req sqlCheckReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	var res *sqlflow.CheckResult
	var err error
	switch {
	case req.Export:
		ids, batch := req.InstanceIDs, len(req.InstanceIDs) > 0
		if !batch {
			ids = []uint{req.InstanceID}
		}
		res, err = s.sql.PreviewExportMerge(c.Request.Context(), actorOf(c), ids, batch, req.ExportMerge, req.DBName, req.SQL)
	case len(req.InstanceIDs) > 0:
		res, err = s.sql.PreviewBatch(c.Request.Context(), actorOf(c), req.InstanceIDs, req.DBName, req.SQL)
	default:
		res, err = s.sql.Preview(c.Request.Context(), actorOf(c), req.InstanceID, req.DBName, req.SQL, req.SkipCheck)
	}
	if err != nil {
		writeErr(c, err)
		return
	}
	rows := sqlTrRows(c, res.Rows)
	if rows == nil {
		rows = []review.Row{}
	}
	instances := res.Instances
	if langOf(c) != i18n.ZH {
		instances = append([]sqlflow.InstanceCheck(nil), res.Instances...)
		for i := range instances {
			instances[i].Error = sqlTrText(c, instances[i].Error)
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"rows": rows, "warning_count": res.WarningCount, "error_count": res.ErrorCount, "syntax_type": res.SyntaxType,
		"syntax_label": trMsg(c, sqlflow.SyntaxLabel(res.SyntaxType)), "source": res.Source, "notice": sqlTrText(c, res.Notice), "instances": instances,
	}})
}

type sqlSubmitReq struct {
	Name         string `json:"name"`
	DemandURL    string `json:"demand_url"`
	InstanceID   uint   `json:"instance_id"`
	InstanceIDs  []uint `json:"instance_ids"` // 批量上线：多个实例（第一个是主实例）
	DBName       string `json:"db_name"`
	GroupName    string `json:"group_name"`
	SQL          string `json:"sql"`
	IsBackup     bool   `json:"is_backup"`
	SkipCheck    bool   `json:"skip_check"`
	RunStart     string `json:"run_date_start"`
	RunEnd       string `json:"run_date_end"`
	Export       bool   `json:"export"`        // 数据导出工单（格式必填）
	ExportFormat string `json:"export_format"` // 导出格式；非空也视为数据导出工单
	// 导出原值（不脱敏）：仅超级用户，必须写理由；其他用户带这个字段会被拒绝（403）
	ExportUnmasked     bool   `json:"export_unmasked"`
	ExportUnmaskReason string `json:"export_unmask_reason"`
	ExportMerge        bool   `json:"export_merge"` // 批量导出合并成一个文件：各实例导出的数据依次写进同一个文件
	PlanAt             string `json:"plan_run_at"`  // 提交时设定的定时执行 / 定时导出时间（审批全部通过后自动转入定时）
}

func (s *Server) apiSQLSubmit(c *gin.Context) {
	limitBody(c)
	var req sqlSubmitReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	start, err := parseLocalTime(req.RunStart)
	if err != nil {
		writeErr(c, err)
		return
	}
	end, err := parseLocalTime(req.RunEnd)
	if err != nil {
		writeErr(c, err)
		return
	}
	plan, err := parseLocalTime(req.PlanAt)
	if err != nil {
		writeErr(c, err)
		return
	}
	sr := sqlflow.SubmitReq{Name: req.Name, DemandURL: req.DemandURL, InstanceID: req.InstanceID, BatchInstanceIDs: req.InstanceIDs,
		DBName: req.DBName, GroupName: req.GroupName, SQL: req.SQL, IsBackup: req.IsBackup, SkipCheck: req.SkipCheck, RunStart: start, RunEnd: end, PlanAt: plan}
	if req.Export || req.ExportFormat != "" {
		sr.Export = &sqlflow.ExportSpec{Format: req.ExportFormat, Unmasked: req.ExportUnmasked, UnmaskReason: req.ExportUnmaskReason, MergeFiles: req.ExportMerge}
	}
	res, err := s.sql.Submit(c.Request.Context(), actorOf(c), sr)
	if err != nil {
		writeErr(c, err)
		return
	}
	res.Message = sqlTrText(c, res.Message)
	c.JSON(http.StatusOK, gin.H{"data": res})
}

func (s *Server) apiSQLList(c *gin.Context) {
	_, offset := pageParam(c)
	q := sqlflow.ListQuery{Search: c.Query("q"), Status: c.Query("status"), Batch: c.Query("batch"), Kind: c.Query("kind"), Tab: c.Query("tab"), OrderBy: c.Query("sort"), Desc: c.Query("dir") != "asc", StartDate: c.Query("start_date"), EndDate: c.Query("end_date"), Offset: offset, Limit: pageSize}
	if v, err := strconv.ParseUint(c.Query("instance_id"), 10, 32); err == nil {
		q.InstanceID = uint(v)
	}
	if v, err := strconv.ParseUint(c.Query("group_id"), 10, 32); err == nil {
		q.GroupID = uint(v)
	}
	for _, t := range c.QueryArray("syntax_type") {
		if n, err := strconv.Atoi(t); err == nil {
			q.SyntaxTypes = append(q.SyntaxTypes, n)
		}
	}
	rows, total, err := s.sql.List(actorOf(c), q)
	if err != nil {
		writeErr(c, err)
		return
	}
	if langOf(c) != i18n.ZH { // 状态名、审批级、批量实例数等是我们产出的中文：按请求语言翻译
		for i := range rows {
			rows[i].StatusLabel = trMsg(c, rows[i].StatusLabel)
			rows[i].SyntaxLabel = trMsg(c, rows[i].SyntaxLabel)
			rows[i].Level = trMsg(c, rows[i].Level)
			rows[i].InstanceName = trMsg(c, rows[i].InstanceName)
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": total})
}

func (s *Server) apiSQLDetail(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	d, err := s.sql.Get(actorOf(c), id)
	if err != nil {
		writeErr(c, err)
		return
	}
	s.applyDetailButtons(c, d)
	s.applyExtraButtons(c, d)
	sqlTrDetail(c, d)
	logs := make([]gin.H, 0, len(d.Logs))
	for _, l := range d.Logs {
		logs = append(logs, gin.H{"operation": l.OperationTypeDesc, "info": l.OperationInfo, "operator": l.OperatorDisplay, "time": l.OperationTime.Format("2006-01-02 15:04:05")})
	}
	nodes := make([]gin.H, 0, len(d.Nodes))
	for _, n := range d.Nodes {
		nodes = append(nodes, gin.H{"name": n.Name, "current": n.Current, "passed": n.Passed})
	}
	w := d.Workflow
	var finish string
	if w.FinishTime != nil {
		finish = w.FinishTime.Format("2006-01-02 15:04:05")
	}
	if d.ReviewRows == nil {
		d.ReviewRows = []review.Row{}
	}
	if d.ExecRows == nil {
		d.ExecRows = []review.Row{}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"id": w.ID, "name": w.WorkflowName, "status": w.Status, "status_label": d.StatusLabel, "instance": d.InstanceName, "db_name": w.DBName,
		"group": w.GroupName, "env": d.Env, "env_label": d.EnvLabel, "env_prod": d.EnvProd, "engineer": w.EngineerDisplay, "syntax_label": d.SyntaxLabel, "is_backup": w.IsBackup,
		"create_time": w.CreateTime.Format("2006-01-02 15:04:05"), "finish_time": finish, "flow": d.Flow, "nodes": nodes, "logs": logs,
		"review_rows": d.ReviewRows, "exec_rows": d.ExecRows,
		"can_review": d.CanReview, "can_execute": d.CanExecute, "can_cancel": d.CanCancel, "can_rollback": d.CanRollback, "can_osc": d.CanOsc,
		"is_batch": d.IsBatch, "batch_targets": d.BatchTargets, "batch_instances": d.BatchInstances, "can_batch_op": d.CanBatchOp,
		"is_export": d.IsExport, "export_format": d.ExportFormat, "file_name": d.FileName, "can_download": d.CanDownload,
		"steps": d.Steps, "current_approvers": d.CurrentApprovers, "can_cancel_schedule": d.CanCancelSchedule, "can_copy": d.CanCopy, "can_mark_exception": d.CanMarkException, "can_cancel_run": d.CanCancelRun, "stuck_hint": d.StuckHint, "export_file": d.ExportFile, "export_unmasked": d.ExportUnmasked, "export_merge": d.ExportMerge, "export_rows": d.ExportRows,
	}})
}

type sqlRemarkReq struct {
	Remark string `json:"remark"`
}

func (s *Server) apiSQLPass(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if !s.ButtonOrFail(c, "sqlworkflow.approve") {
		return
	}
	var req sqlRemarkReq
	_ = c.ShouldBindJSON(&req)
	w, err := s.sql.Pass(actorOf(c), id, req.Remark)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": w.Status, "status_label": trMsg(c, sqlflow.StatusLabel(w.Status))}})
}

func (s *Server) apiSQLCancel(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if !s.cancelAllowed(c, id) {
		return
	}
	var req sqlRemarkReq
	_ = c.ShouldBindJSON(&req)
	w, err := s.sql.Cancel(actorOf(c), id, req.Remark)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": w.Status, "status_label": trMsg(c, sqlflow.StatusLabel(w.Status))}})
}

type sqlRunDateReq struct {
	Start string `json:"run_date_start"`
	End   string `json:"run_date_end"`
}

func (s *Server) apiSQLRunDate(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if !s.ButtonOrFail(c, "sqlworkflow.alter_window") {
		return
	}
	var req sqlRunDateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	start, err := parseLocalTime(req.Start)
	if err != nil {
		writeErr(c, err)
		return
	}
	end, err := parseLocalTime(req.End)
	if err != nil {
		writeErr(c, err)
		return
	}
	if err := s.sql.AlterRunDate(actorOf(c), id, start, end); err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

type sqlExecuteReq struct {
	Mode     string `json:"mode"`
	Executor string `json:"executor"`
}

func (s *Server) apiSQLExecute(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var req sqlExecuteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	// 手工确认结束是单独的按钮；系统执行按资源组内 / 仅自己的拆成两个按钮（具体适用哪个由服务层按工单与资源组判断）
	if req.Mode == sqlflow.ModeManual {
		if !s.ButtonOrFail(c, "sqlworkflow.manual_finish") {
			return
		}
	} else if !s.ButtonOrFail(c, "sqlworkflow.execute_group", "sqlworkflow.execute_own") {
		return
	}
	w, err := s.sql.Execute(c.Request.Context(), actorOf(c), id, sqlflow.ExecuteReq{Mode: req.Mode, Executor: req.Executor})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": w.Status, "status_label": trMsg(c, sqlflow.StatusLabel(w.Status))}})
}

type sqlScheduleReq struct {
	RunAt    string `json:"run_at"`
	Executor string `json:"executor"`
}

func (s *Server) apiSQLSchedule(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if !s.ButtonOrFail(c, "sqlworkflow.schedule") {
		return
	}
	var req sqlScheduleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	at, err := parseLocalTime(req.RunAt)
	if err != nil {
		writeErr(c, err)
		return
	}
	if at == nil {
		writeErr(c, query.NewError(400, "bad_request", "时间不能为空"))
		return
	}
	w, err := s.sql.Schedule(actorOf(c), id, *at, req.Executor)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": w.Status, "status_label": trMsg(c, sqlflow.StatusLabel(w.Status))}})
}

func (s *Server) apiSQLRollback(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if !s.ButtonOrFail(c, "sqlworkflow.rollback") {
		return
	}
	_, items, err := s.sql.RollbackAudited(c.Request.Context(), actorOf(c), id, sqlflow.RollbackView)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": items})
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func (s *Server) apiSQLRollbackDownload(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if !s.ButtonOrFail(c, "sqlworkflow.rollback") {
		return
	}
	w, items, err := s.sql.RollbackAudited(c.Request.Context(), actorOf(c), id, sqlflow.RollbackDownload)
	if err != nil {
		writeErr(c, err)
		return
	}
	name := unsafeFileChars.ReplaceAllString(fmt.Sprintf("rollback-%d.sql", w.ID), "_")
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(sqlflow.RollbackScriptFor(langOf(c), w, items, time.Now())))
}

type sqlOscReq struct {
	Command string `json:"command"`
	SHA1    string `json:"sqlsha1"`
}

func (s *Server) apiSQLOsc(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if !s.ButtonOrFail(c, "sqlworkflow.osc") {
		return
	}
	var req sqlOscReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	rows, err := s.sql.OscControl(c.Request.Context(), actorOf(c), id, req.Command, req.SHA1)
	if err != nil {
		writeErr(c, err)
		return
	}
	if rows == nil {
		rows = []sqlflow.OscRow{}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows})
}

func (s *Server) apiSQLBatchRetry(c *gin.Context) { s.batchOp(c, true) }
func (s *Server) apiSQLBatchClose(c *gin.Context) { s.batchOp(c, false) }

// batchOp 重试 / 关闭批量工单里失败的实例。
func (s *Server) batchOp(c *gin.Context, retry bool) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	instID, ok := uintParam(c, "instance_id")
	if !ok {
		return
	}
	if !s.ButtonOrFail(c, "sqlworkflow.batch_op") {
		return
	}
	var msg, status string
	var err error
	if retry {
		msg, status, err = s.sql.RetryBatchInstance(c.Request.Context(), actorOf(c), id, instID)
	} else {
		msg, status, err = s.sql.CloseBatchInstance(actorOf(c), id, instID)
	}
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": sqlTrText(c, msg), "status": status, "status_label": trMsg(c, sqlflow.StatusLabel(status))}})
}

// apiSQLDownload 下载导出文件。文件名取工单自己记录的，不接受调用方传入。
func (s *Server) apiSQLDownload(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	f, err := s.sql.Download(actorOf(c), id)
	if err != nil {
		writeErr(c, err)
		return
	}
	defer f.Reader.Close()
	name := unsafeFileChars.ReplaceAllString(f.Name, "_")
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Header("Content-Type", "application/zip")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "no-store")
	http.ServeContent(c.Writer, c.Request, name, time.Time{}, f.Reader)
}
