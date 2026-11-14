// @author: kevinkelly
// @license: Apache Licence
// @file: export.go
// @time: 2026/10/28

package sqlflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine"
	exportpkg "dbtool/internal/export"
	"dbtool/internal/firewall"
	"dbtool/internal/model"
	"dbtool/internal/review"
	"dbtool/internal/settings"
)

// 数据导出工单：对一条只读查询，审批通过后在目标实例上执行，把结果写成文件供下载；批量导出把同一条查询在多个实例上各执行一遍，
// 每个实例一个文件（提交时选了“合并成一个文件”则各实例的数据依次写进同一个文件），打成 zip。审批、定时执行、通知、批量实例的重试/关闭都复用 SQL 工单的流程。

// SourceExport 是导出工单的检查来源。
const SourceExport = "export"

// ExportSpec 是导出工单的专属参数。
type ExportSpec struct {
	Format string // csv / xlsx / json / xml / sql
	// Unmasked：导出原值（不做用户配置的数据脱敏）。仅超级用户可选，必须写理由；SQL 防火墙要求强制脱敏的列照样脱敏。
	Unmasked     bool
	UnmaskReason string
	// MergeFiles：批量导出合并成一个文件——各实例导出的数据按勾选顺序依次写进同一个文件（表头只有一份，各实例返回的列必须一致），
	// 不再每个实例一个文件。只能用于批量导出。
	MergeFiles bool
}

// 导出原值的理由长度（字符数）。
const (
	unmaskReasonMin = 5
	unmaskReasonMax = 200
)

// mergeReady 判断“合并成一个文件”能不能用：仓库支持、扩展表已有 merge_files / merge_col 两列。
func (s *Service) mergeReady() bool {
	g, ok := s.repo.(ExportMergeGate)
	return ok && g.ExportMergeReady()
}

// exportMerge 返回这张导出工单是不是“合并成一个文件”。读不出来（表没建、查询出错）按每个实例一个文件处理。
func (s *Service) exportMerge(w *model.SqlWorkflow) bool {
	if w == nil || w.IsOfflineExport == 0 || !w.IsBatch() {
		return false
	}
	e, err := s.repo.GetExportExt(w.ID)
	if err != nil {
		slog.Warn("读取合并导出选项失败，按每个实例一个文件导出", "workflow", w.ID, "err", err)
		return false
	}
	return e != nil && e.MergeFiles
}

// 提交前检查要不要核对各实例的列（合并导出）：用 context 带进 checkExport，不改共用的检查函数签名。
type mergeCheckKey struct{}

func withMergeCheck(ctx context.Context) context.Context {
	return context.WithValue(ctx, mergeCheckKey{}, true)
}

func mergeCheck(ctx context.Context) bool {
	v, _ := ctx.Value(mergeCheckKey{}).(bool)
	return v
}

func kindWord(export bool) string {
	if export {
		return "导出"
	}
	return "上线"
}

// 导出的默认上限：行数 10000、单次查询最长 60 秒（export.max_rows / export.max_seconds）。
const (
	defaultMaxExportRows = 10000
	defaultExportSeconds = 60
)

func cfgPositiveInt(r Repo, item string, def int) int {
	if n, err := strconv.Atoi(cfgStr(r, item)); err == nil && n > 0 {
		return n
	}
	return def
}

func (s *Service) exportCaps() (maxRows, seconds int) {
	return cfgPositiveInt(s.repo, settings.KeyExportMaxRows, defaultMaxExportRows), cfgPositiveInt(s.repo, settings.KeyExportMaxSeconds, defaultExportSeconds)
}

// ExportLimits 给页面展示：行数上限与单次查询超时。
func (s *Service) ExportLimits() (maxRows, seconds int) { return s.exportCaps() }

// ---- 存储 ----

// storage 返回导出文件的存放位置。目前只支持本地目录：export.storage_type 为空或 local，目录取 export.local_path
// （默认 downloads/DataExportFile/；相对路径相对于 EXPORT_BASE_DIR）。
// 配置成 sftp / s3c / azure 时明确报错，而不是悄悄写到本地。
func (s *Service) storage() (exportpkg.Storage, error) {
	if s.exportStore != nil {
		return s.exportStore, nil
	}
	if t := cfgStr(s.repo, settings.KeyExportStorageType); t != "" && t != "local" {
		return nil, fmt.Errorf("导出文件存储类型是 %s，新系统暂只支持本地存储（系统设置 export.storage_type=local），请联系管理员", t)
	}
	dir := cfgStr(s.repo, settings.KeyExportLocalPath)
	if dir == "" {
		dir = "downloads/DataExportFile/"
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(s.exportBase, dir)
	}
	return exportpkg.NewLocalStorage(dir)
}

// ---- 检查 ----

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case uint64:
		return int64(x), true
	case float64:
		return int64(x), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n, err == nil
	case []byte:
		n, err := strconv.ParseInt(strings.TrimSpace(string(x)), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// countRows 把查询包成派生表统计行数。
func (s *Service) countRows(ctx context.Context, inst *model.Instance, db, countSQL string, seconds int) (int64, error) {
	e, err := s.open(inst.DBType, s.repo.EngineConn(inst))
	if err != nil {
		return 0, err
	}
	defer e.Close()
	rs, err := e.Query(ctx, countSQL, engine.QueryOptions{DBName: db, MaxExecMs: int64(seconds) * 1000})
	if err != nil {
		return 0, err
	}
	if len(rs.Rows) == 0 || len(rs.Rows[0]) == 0 {
		return 0, errors.New("统计行数没有返回结果")
	}
	n, ok := toInt64(rs.Rows[0][0])
	if !ok {
		return 0, fmt.Errorf("统计行数返回了无法识别的值：%v", rs.Rows[0][0])
	}
	return n, nil
}

// probeColumns 取查询返回的列名（一行都不取）。取不到（语句包成派生表后不合法，如结果里有重名列、SQL Server 的无名列）返回 nil：
// 这种情况不能提前核对，留到执行时按实际结果核对。
func (s *Service) probeColumns(ctx context.Context, inst *model.Instance, db, probeSQL string, seconds int) []string {
	if probeSQL == "" {
		return nil
	}
	e, err := s.open(inst.DBType, s.repo.EngineConn(inst))
	if err != nil {
		return nil
	}
	defer e.Close()
	rs, err := e.Query(ctx, probeSQL, engine.QueryOptions{DBName: db, MaxExecMs: int64(seconds) * 1000})
	if err != nil {
		slog.Debug("合并导出：取列名失败，提交前不核对这个实例的列", "instance", inst.InstanceName, "err", err)
		return nil
	}
	return append([]string{}, rs.ColumnList...)
}

// checkExport 检查导出：语句必须是单条只读查询；逐个实例（并发 8 路）统计行数，超过阈值判错。
func (s *Service) checkExport(ctx context.Context, p audit.Actor, insts []*model.Instance, db, sqlText string) (*CheckResult, error) {
	first := insts[0]
	res := &CheckResult{Source: SourceExport}
	res.FullSQL, res.SyntaxType = sqlText, SyntaxExport
	fail := func(msg string) (*CheckResult, error) {
		res.Rows = []review.Row{{ID: 1, Stage: "自动审核失败", ErrLevel: review.LevelError, StageStatus: "检查未通过！", ErrorMessage: msg, SQL: sqlText}}
		res.Count()
		return res, nil
	}
	prep, err := exportpkg.Prepare(first.DBType, sqlText)
	if err != nil {
		return fail(err.Error())
	}
	res.FullSQL = prep.SQL
	maxRows, seconds := s.exportCaps()
	// 库表权限：与在线查询一致，语句涉及的每张表都要有库权限或表权限（逐实例校验）
	denied := s.exportDenied(ctx, p, insts, db, prep.SQL)
	for i := range insts {
		if msg, ok := denied[i]; ok {
			who := ""
			if len(insts) > 1 {
				who = fmt.Sprintf("实例[%s]", insts[i].InstanceName)
			}
			res.Denied = who + msg
			break
		}
	}
	// SQL 防火墙（导出场景）：每个实例各求一次——规则的实例 / 环境范围可能不同
	decs := make([]*firewall.Decision, len(insts))
	if s.fw != nil {
		ss := &fwSession{}
		for i, in := range insts {
			decs[i] = s.fwEval(ctx, ss, firewall.SceneExport, p.Username, p.ID, in, db, prep.SQL, -1)
		}
		ss.close()
	}
	if prep.Batch {
		for i := range insts {
			if msg, ok := denied[i]; ok {
				who := ""
				if len(insts) > 1 {
					who = fmt.Sprintf("实例[%s]", insts[i].InstanceName)
				}
				return fail(who + msg)
			}
		}
		for i, d := range decs {
			if d != nil && d.Deny {
				who := ""
				if len(insts) > 1 {
					who = fmt.Sprintf("实例[%s]", insts[i].InstanceName)
				}
				return fail(who + fwDenyText(d))
			}
		}
		// T-SQL 批处理不能包进派生表，无法预先统计行数：提交时只校验只读，行数阈值在执行时逐实例兜底
		res.Rows = []review.Row{{ID: 1, ErrLevel: review.LevelWarning, StageStatus: "批处理语句，跳过行数预统计", SQL: prep.SQL,
			ErrorMessage: fmt.Sprintf("检测到 declare/set 批处理语句，无法预先统计行数；执行时若超过阈值(%d)将中止导出。", maxRows)}}
		res.Count()
		return res, nil
	}
	countSQL := prep.CountSQL(first.DBType)
	batch := len(insts) > 1
	counts := make([]int64, len(insts))
	errs := make([]error, len(insts))
	merge := batch && mergeCheck(ctx) // 合并成一个文件：各实例的列必须一致，提交前就核对
	cols := make([][]string, len(insts))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, in := range insts {
		wg.Add(1)
		go func(i int, in *model.Instance) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if _, no := denied[i]; no {
				return // 没有库表权限，不必统计行数
			}
			if decs[i] != nil && decs[i].Deny {
				return // 防火墙已拒绝，不必再统计行数
			}
			counts[i], errs[i] = s.countRows(ctx, in, db, countSQL, seconds)
			if merge && errs[i] == nil {
				cols[i] = s.probeColumns(ctx, in, db, prep.ProbeSQL(in.DBType), seconds)
			}
		}(i, in)
	}
	wg.Wait()
	colMsg := make([]string, len(insts))
	if merge {
		ref := -1
		for i := range insts {
			if cols[i] == nil {
				continue
			}
			if ref < 0 {
				ref = i
			} else if !exportpkg.SameColumns(cols[ref], cols[i]) {
				colMsg[i] = fmt.Sprintf("返回的列与第一个实例「%s」不一致（它的列：%s；这个实例的列：%s）", insts[ref].InstanceName,
					strings.Join(cols[ref], ", "), strings.Join(cols[i], ", "))
			}
		}
	}
	for i, in := range insts {
		row := review.Row{ID: review.Flex(i + 1), SQL: prep.SQL, ErrorMessage: "None"}
		who := ""
		if batch {
			who = fmt.Sprintf("实例[%s]", in.InstanceName)
		}
		ic := InstanceCheck{ID: in.ID, Name: in.InstanceName}
		limit, limitBy := maxRows, ""
		if d := decs[i]; d != nil && d.MaxRows > 0 && d.MaxRows < limit { // 防火墙的限行与导出的行数上限取小
			limit, limitBy = d.MaxRows, d.LimitedBy
		}
		switch {
		case denied[i] != "":
			row.Stage, row.ErrLevel, row.StageStatus = "自动审核失败", review.LevelError, "无库表查询权限"
			row.ErrorMessage = who + denied[i]
			ic.Error = row.ErrorMessage
		case decs[i] != nil && decs[i].Deny:
			row.Stage, row.ErrLevel, row.StageStatus = "自动审核失败", review.LevelError, fwDenyStatus
			row.ErrorMessage = who + fwDenyText(decs[i])
			ic.Error = row.ErrorMessage
		case errs[i] != nil:
			row.Stage, row.ErrLevel, row.StageStatus = "自动审核失败", review.LevelError, "检查未通过！"
			row.ErrorMessage = who + fmt.Sprintf("检查失败: %s", exportErrText(errs[i]))
			ic.Error = exportErrText(errs[i])
		case counts[i] > int64(limit):
			row.ErrLevel, row.StageStatus, row.AffectedRows = review.LevelError, "检查未通过！", review.Flex(counts[i])
			row.ErrorMessage = who + fmt.Sprintf("导出数据行数(%d)超过阈值(%d)。", counts[i], limit)
			if limitBy != "" {
				row.ErrorMessage = who + fmt.Sprintf("导出数据行数(%d)超过 SQL 防火墙规则「%s」限制的行数(%d)。", counts[i], limitBy, limit)
			}
			ic.Error = row.ErrorMessage
		case colMsg[i] != "":
			row.ErrLevel, row.StageStatus, row.AffectedRows = review.LevelError, "检查未通过！", review.Flex(counts[i])
			row.ErrorMessage = who + colMsg[i]
			ic.Error = row.ErrorMessage
		default:
			row.StageStatus, row.AffectedRows = "行数统计完成", review.Flex(counts[i])
			if batch {
				row.ErrorMessage = who + fmt.Sprintf("待导出 %d 行", counts[i])
			}
			ic.OK = true
		}
		if d := decs[i]; d != nil && !d.Deny && row.ErrLevel == review.LevelOK {
			notes := fwNotes(d)
			if len(notes) > 0 {
				row.ErrLevel, row.StageStatus = review.LevelWarning, fwWarnStatus
			}
			if len(d.ForceMask) > 0 {
				notes = append(notes, fmt.Sprintf("SQL 防火墙将对这些列强制脱敏：%s", refsText(d.ForceMask)))
			}
			if len(notes) > 0 {
				row.ErrorMessage = joinNotes(row.ErrorMessage, dedup(notes))
			}
		}
		if batch {
			row.InstanceID, row.InstanceName = review.Flex(in.ID), in.InstanceName
			res.Instances = append(res.Instances, ic)
		}
		res.Rows = append(res.Rows, row)
	}
	res.Count()
	return res, nil
}

func errText(err error) string { return strings.TrimSpace(err.Error()) }

// PreviewExport 提交前检查导出的查询（页面的“检查”按钮）。
func (s *Service) PreviewExport(ctx context.Context, p audit.Actor, ids []uint, batch bool, db, sqlText string) (*CheckResult, error) {
	return s.PreviewExportMerge(ctx, p, ids, batch, false, db, sqlText)
}

// PreviewExportMerge 同 PreviewExport；merge=true（批量导出选了“合并成一个文件”）时还会核对各实例返回的列是否一致。
func (s *Service) PreviewExportMerge(ctx context.Context, p audit.Actor, ids []uint, batch, merge bool, db, sqlText string) (*CheckResult, error) {
	if merge {
		ctx = withMergeCheck(ctx)
	}
	return s.previewTargets(ctx, p, ids, batch, true, db, sqlText, false)
}

// ---- 执行 ----

// fetch 在实例上执行导出查询并按格式写文件；返回行数。超过行数上限（export.max_rows）中止。
func (s *Service) fetch(ctx context.Context, w *model.SqlWorkflow, inst *model.Instance, prep *exportpkg.Prepared, write func(r exportpkg.Result) error) (int, error) {
	maxRows, seconds := s.exportCaps()
	// SQL 防火墙：执行前在这个实例上复核（审批期间规则可能变了）。拒绝 → 这个实例导出失败；限行与上限取小；强制脱敏的列叠加到下面的脱敏里。
	var force []firewall.ObjectRef
	limitBy := ""
	if d := s.fwExportRecheck(ctx, w, inst, prep.SQL); d != nil {
		if d.Deny {
			return 0, fmt.Errorf("执行前复核：%s；没有导出任何数据", fwDenyText(d))
		}
		if d.MaxRows > 0 && d.MaxRows < maxRows {
			maxRows, limitBy = d.MaxRows, d.LimitedBy
		}
		force = d.ForceMask
	}
	e, err := s.open(inst.DBType, s.repo.EngineConn(inst))
	if err != nil {
		return 0, fmt.Errorf("连接实例失败: %w", err)
	}
	defer e.Close()
	rs, err := e.Query(ctx, prep.SQL, engine.QueryOptions{DBName: w.DBName, Limit: maxRows + 1, MaxExecMs: int64(seconds) * 1000})
	if err != nil {
		return 0, err
	}
	if len(rs.Rows) > maxRows {
		if limitBy != "" {
			return 0, fmt.Errorf("导出数据行数超过 SQL 防火墙规则「%s」限制的行数(%d)，已中止导出。", limitBy, maxRows)
		}
		return 0, fmt.Errorf("导出数据行数超过阈值(%d)，已中止导出。", maxRows)
	}
	// 数据脱敏：与在线查询同一套配置与规则。脱敏失败不放行原值，直接中止这次导出。
	// 提交人（超级用户）选择了“导出原值”且执行时仍是超级用户：不做用户配置的脱敏，但 SQL 防火墙要求强制脱敏的列照样脱敏。
	unmasked, downgraded := s.exportUnmasked(w)
	if downgraded {
		slog.Warn("导出工单提交时选择了导出原值，但提交人现在已不是超级用户，按脱敏设置导出", "workflow", w.ID, "engineer", w.Engineer)
	}
	cfgMask := s.exportMaskingOn() && !unmasked
	var masked []string
	switch fm, canForce := s.masker.(ForcedResultMasker); {
	case len(force) > 0:
		// 防火墙要求强制脱敏：即使脱敏总开关没开也要做，做不了就中止，不能导出未脱敏的数据
		if s.masker == nil || !canForce {
			return 0, fmt.Errorf("SQL 防火墙要求强制脱敏，但脱敏组件不可用，已中止导出（不会导出未脱敏的数据）")
		}
		if masked, err = fm.MaskExportForced(ctx, inst, w.DBName, prep.SQL, e, rs, force, cfgMask); err != nil {
			return 0, fmt.Errorf("数据脱敏失败，已中止导出（不会导出未脱敏的数据）: %w", err)
		}
	case cfgMask:
		if masked, err = s.masker.MaskExport(ctx, inst, w.DBName, prep.SQL, e, rs); err != nil {
			return 0, fmt.Errorf("数据脱敏失败，已中止导出（不会导出未脱敏的数据）: %w", err)
		}
	}
	dialect := "mysql"
	if inst.DBType == dbTypeMSSQL {
		dialect = "mssql"
	}
	raw := unmasked && s.exportMaskingOn() // 本来会脱敏、因为选了导出原值而没有脱敏
	return len(rs.Rows), write(exportpkg.Result{Columns: rs.ColumnList, Rows: rs.Rows, Dialect: dialect, Masked: masked, Raw: raw})
}

// exportUnmasked 判断这张导出工单是否导出原值：提交时由超级用户选择并写了理由（dbt_sql_export_ext），而且执行时提交人仍是启用的超级用户。
// downgraded=true 表示提交时选了、但现在不再生效（提交人已被降级 / 停用），调用方要按脱敏设置导出并留下告警。
// 读不出来（表没建、查询出错）一律按“不导出原值”处理：宁可多脱敏，不能误给原值。
func (s *Service) exportUnmasked(w *model.SqlWorkflow) (on, downgraded bool) {
	if w == nil || w.IsOfflineExport == 0 {
		return false, false
	}
	e, err := s.repo.GetExportExt(w.ID)
	if err != nil {
		slog.Warn("读取导出原值记录失败，按脱敏设置导出", "workflow", w.ID, "err", err)
		return false, false
	}
	if e == nil || !e.Unmasked {
		return false, false
	}
	super, err := s.repo.UserIsSuperuser(w.Engineer)
	if err != nil {
		slog.Warn("复核提交人是否超级用户失败，按脱敏设置导出", "workflow", w.ID, "err", err)
		return false, true
	}
	if !super {
		return false, true
	}
	return true, false
}

// ResultMasker 对导出查询的结果脱敏；实现是在线查询用的同一套脱敏（query.Service.MaskExport）。
type ResultMasker interface {
	// MaskExport 就地脱敏 rs，返回被脱敏的结果列名；e 是执行导出查询用的引擎。
	MaskExport(ctx context.Context, inst *model.Instance, db, sql string, e engine.Engine, rs *engine.ResultSet) ([]string, error)
}

// SetResultMasker 设置导出脱敏的实现。没有设置时导出不脱敏。
func (s *Service) SetResultMasker(m ResultMasker) { s.masker = m }

// exportMaskingOn 判断导出是否脱敏：系统的脱敏总开关（query.mask_enabled）开着，且导出脱敏开关（export.mask_enabled，默认开）没有关。
func (s *Service) exportMaskingOn() bool {
	if s.masker == nil || !cfgBool(s.repo, settings.KeyQueryMaskEnabled) {
		return false
	}
	v, ok := s.repo.ConfigValue(settings.KeyExportMaskEnabled)
	return !ok || !strings.EqualFold(strings.TrimSpace(v), "false")
}

// savedNote 是单实例导出结果行里的“保存文件”说明，有脱敏列时带上“（已脱敏：…）”。
// 这些文字存进结果行，详情页显示时按整句查英文目录，所以脱敏与否各写一整句，不拼接碎片。
func savedNote(name string, masked []string, raw bool) string {
	switch {
	case raw && len(masked) == 0:
		return fmt.Sprintf("保存文件: %s（导出原值，未脱敏）", name)
	case raw:
		return fmt.Sprintf("保存文件: %s（导出原值；SQL 防火墙强制脱敏：%s）", name, strings.Join(masked, "、"))
	case len(masked) == 0:
		return fmt.Sprintf("保存文件: %s", name)
	}
	return fmt.Sprintf("保存文件: %s（已脱敏：%s）", name, strings.Join(masked, "、"))
}

// exportedNote 是批量导出里一个实例的“导出文件”说明（同上）。
func exportedNote(name string, masked []string, raw bool) string {
	switch {
	case raw && len(masked) == 0:
		return fmt.Sprintf("导出文件: %s（导出原值，未脱敏）", name)
	case raw:
		return fmt.Sprintf("导出文件: %s（导出原值；SQL 防火墙强制脱敏：%s）", name, strings.Join(masked, "、"))
	case len(masked) == 0:
		return fmt.Sprintf("导出文件: %s", name)
	}
	return fmt.Sprintf("导出文件: %s（已脱敏：%s）", name, strings.Join(masked, "、"))
}

func exportFormat(w *model.SqlWorkflow) string {
	if w.ExportFormat != nil {
		return *w.ExportFormat
	}
	return ""
}

func (s *Service) execExport(ctx context.Context, w *model.SqlWorkflow, sqlText string) *review.Set {
	if !exportpkg.ValidFormat(exportFormat(w)) {
		return failedSet(sqlText, fmt.Sprintf("工单的导出格式不合法：%s", exportFormat(w)))
	}
	if _, err := s.storage(); err != nil {
		return failedSet(sqlText, err.Error())
	}
	if w.IsBatch() {
		return s.execExportBatch(ctx, w, sqlText)
	}
	return s.execExportSingle(ctx, w, sqlText)
}

func (s *Service) execExportSingle(ctx context.Context, w *model.SqlWorkflow, sqlText string) *review.Set {
	start := time.Now()
	set := &review.Set{FullSQL: sqlText}
	prep, err := exportpkg.Prepare(w.Instance.DBType, sqlText)
	if err != nil {
		return failedSet(sqlText, err.Error())
	}
	set.FullSQL = prep.SQL
	st, _ := s.storage()
	format := exportFormat(w)
	zipName, entry := exportpkg.SingleArchiveName(w.DBName, s.now())
	tmp, err := os.CreateTemp("", "dbt-export-*.zip")
	if err != nil {
		return failedSet(prep.SQL, fmt.Sprintf("创建临时文件失败: %s", err.Error()))
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	var masked []string
	var raw bool
	n, err := s.fetch(ctx, w, w.Instance, prep, func(r exportpkg.Result) error {
		masked, raw = r.Masked, r.Raw
		return exportpkg.ZipSingle(tmp, entry+"."+format, func(wr io.Writer) error { return exportpkg.Write(wr, format, r) })
	})
	if err == nil {
		if _, err = tmp.Seek(0, io.SeekStart); err == nil {
			err = st.Save(zipName, tmp)
		}
	}
	if err != nil {
		return failedSet(prep.SQL, exportErrText(err))
	}
	set.FileName = zipName
	set.Rows = []review.Row{{ID: 1, Stage: "Executed", StageStatus: "执行正常", ErrorMessage: savedNote(zipName, masked, raw), SQL: prep.SQL,
		AffectedRows: review.Flex(n), BackupDBName: "None", ExecuteTime: strconv.FormatFloat(time.Since(start).Seconds(), 'f', 3, 64)}}
	return set
}

// exportToDir 在一个实例上导出并把文件写进目录；返回行数。
func (s *Service) exportToDir(ctx context.Context, w *model.SqlWorkflow, in *model.Instance, prep *exportpkg.Prepared, dir, fileName string) (int, []string, bool, error) {
	format := exportFormat(w)
	var masked []string
	var raw bool
	n, err := s.fetch(ctx, w, in, prep, func(r exportpkg.Result) error {
		masked, raw = r.Masked, r.Raw
		f, err := os.OpenFile(filepath.Join(dir, fileName), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if err != nil {
			return err
		}
		if err := exportpkg.Write(f, format, r); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	})
	return n, masked, raw, err
}

// zipAndSave 把目录打成工单号命名的 zip 并存进存储，返回压缩包名。
func (s *Service) zipAndSave(w *model.SqlWorkflow, st exportpkg.Storage, exportDir string) (string, error) {
	tmp, err := os.CreateTemp("", "dbt-export-*.zip")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := exportpkg.ZipDir(tmp, exportDir, strconv.Itoa(int(w.ID))); err != nil {
		return "", err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	name := exportpkg.BatchArchiveName(w.ID, s.now())
	return name, st.Save(name, tmp)
}

func exportRow(sqlText, msg string, rows int, elapsed float64) review.Row {
	return review.Row{Stage: "Executed", StageStatus: "执行正常", ErrorMessage: msg, SQL: sqlText, AffectedRows: review.Flex(rows), BackupDBName: "None",
		ExecuteTime: strconv.FormatFloat(elapsed, 'f', 3, 64)}
}

func exportFailRow(sqlText, msg string) review.Row {
	return review.Row{Stage: "Execute failed", ErrLevel: review.LevelError, StageStatus: "异常终止", ErrorMessage: msg, SQL: sqlText, BackupDBName: "None"}
}

func (s *Service) execExportBatch(ctx context.Context, w *model.SqlWorkflow, sqlText string) *review.Set {
	set := &review.Set{FullSQL: sqlText}
	insts, missing := s.batchInstances(w)
	if len(insts) == 0 {
		msg := "批量导出工单未关联任何实例。"
		set.Rows = []review.Row{{ID: 1, Stage: "Execute failed", ErrLevel: review.LevelError, StageStatus: "异常终止", ErrorMessage: msg, SQL: sqlText}}
		set.Error = msg
		return set
	}
	prep, err := exportpkg.Prepare(insts[0].DBType, sqlText)
	if err != nil {
		return failedSet(sqlText, err.Error())
	}
	set.FullSQL = prep.SQL
	st, _ := s.storage()
	tmp, err := os.MkdirTemp("", "dbt-export-*")
	if err != nil {
		return failedSet(prep.SQL, fmt.Sprintf("创建临时目录失败: %s", err.Error()))
	}
	defer os.RemoveAll(tmp)
	dir := filepath.Join(tmp, strconv.Itoa(int(w.ID)))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return failedSet(prep.SQL, err.Error())
	}

	var rows []review.Row
	for _, id := range missing { // 工单批准后实例被删除：明确记录，不静默少导出
		rows = append(rows, review.Row{Stage: "Execute failed", ErrLevel: review.LevelError, StageStatus: "异常终止", ErrorMessage: "实例已不存在，跳过导出。",
			SQL: prep.SQL, InstanceID: review.Flex(id), InstanceName: fmt.Sprintf("(已删除 id:%d)", id)})
	}
	if s.exportMerge(w) { // 提交时选了“合并成一个文件”
		return s.execExportBatchMerged(ctx, w, set, insts, rows, len(missing), prep, st, dir)
	}
	// 文件名去重必须在并发前算好，否则竞争会让重名文件互相覆盖
	used := map[string]bool{}
	names := make([]string, len(insts))
	for i, in := range insts {
		names[i] = exportpkg.InstanceFileName(in.InstanceName, in.ID, exportFormat(w), used)
	}
	type outcome struct {
		rows    int
		masked  []string
		raw     bool
		elapsed float64
		err     error
	}
	outs := make([]outcome, len(insts))
	s.progTotal(w.ID, len(insts)+len(missing))
	for range missing {
		s.progDone(w.ID)
	}
	one := func(i int) {
		defer s.progDone(w.ID)
		defer func() {
			if r := recover(); r != nil {
				outs[i].err = fmt.Errorf("导出异常: %v", r)
			}
		}()
		start := time.Now()
		n, masked, raw, err := s.exportToDir(ctx, w, insts[i], prep, dir, names[i])
		outs[i] = outcome{rows: n, masked: masked, raw: raw, elapsed: time.Since(start).Seconds(), err: err}
	}
	parallel := len(insts) > 1
	for _, in := range insts {
		if in.DBType != dbTypeMSSQL {
			parallel = false
		}
	}
	if parallel {
		sem := make(chan struct{}, s.batchWorkers(len(insts)))
		var wg sync.WaitGroup
		for i := range insts {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				one(i)
			}(i)
		}
		wg.Wait()
	} else {
		for i := range insts {
			one(i)
		}
	}
	success := 0
	for i, in := range insts {
		var r review.Row
		if outs[i].err != nil {
			slog.Warn("批量导出实例失败", "workflow", w.ID, "instance", in.InstanceName, "err", outs[i].err)
			r = exportFailRow(prep.SQL, fmt.Sprintf("导出失败: %s", exportErrText(outs[i].err)))
		} else {
			success++
			r = exportRow(prep.SQL, exportedNote(names[i], outs[i].masked, outs[i].raw), outs[i].rows, outs[i].elapsed)
		}
		r.InstanceID, r.InstanceName = review.Flex(in.ID), in.InstanceName
		rows = append(rows, r)
	}
	return s.finishBatchExport(w, st, set, rows, dir, prep.SQL, success, len(insts), nil)
}

// finishBatchExport 是批量导出的收尾：全部失败不生成文件；有成功的把目录打成 zip 存进存储，补汇总行，部分失败时给工单打上警告。
// writeErr 非空表示合并文件没写完整（磁盘错误等），按“保存导出文件失败”处理。
func (s *Service) finishBatchExport(w *model.SqlWorkflow, st exportpkg.Storage, set *review.Set, rows []review.Row, dir, sqlText string, success, nInsts int, writeErr error) *review.Set {
	finish := func() {
		for i := range rows {
			rows[i].ID = review.Flex(i + 1)
		}
		set.Rows = rows
	}
	if success == 0 && writeErr == nil {
		finish()
		set.Error = "所有实例均导出失败。"
		return set
	}
	zipName, err := "", writeErr
	if err == nil {
		zipName, err = s.zipAndSave(w, st, dir)
	}
	if err != nil {
		rows = append(rows, review.Row{Stage: "Execute failed", ErrLevel: review.LevelError, StageStatus: "异常终止", ErrorMessage: fmt.Sprintf("保存导出文件失败: %s", err.Error()), SQL: sqlText, InstanceName: "汇总", IsSummary: true})
		finish()
		set.Error = err.Error()
		return set
	}
	set.FileName = zipName
	total := len(parseBatchIDs(w.BatchInstanceIDs))
	if total == 0 {
		total = nInsts
	}
	rows = append(rows, summaryRow(rows, sqlText, true, zipName))
	finish()
	if success < total { // 部分失败：工单标记为异常，但已成功实例的文件仍可下载
		set.Warning = fmt.Sprintf("%d/%d 个实例导出失败，详见执行结果。", total-success, total)
	}
	return set
}

// execExportBatchMerged 是“合并成一个文件”的批量导出：各实例取数（SQL Server 并行，其它依次），按勾选顺序把导出的数据依次写进同一个文件。
// rows 里已有“实例已不存在”的记录，missing 是这类记录的个数（进度条要算上）。列与第一个写入的实例不一致的实例按失败处理、不写入。
func (s *Service) execExportBatchMerged(ctx context.Context, w *model.SqlWorkflow, set *review.Set, insts []*model.Instance, rows []review.Row, missing int,
	prep *exportpkg.Prepared, st exportpkg.Storage, dir string) *review.Set {
	format := exportFormat(w)
	fname := exportpkg.MergedFileName(w.ID, format)
	f, err := os.OpenFile(filepath.Join(dir, fname), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return failedSet(prep.SQL, fmt.Sprintf("创建合并文件失败: %s", err.Error()))
	}
	mw, err := exportpkg.NewMergeWriter(f, format)
	if err != nil {
		f.Close()
		return failedSet(prep.SQL, err.Error())
	}
	type fetched struct {
		res     exportpkg.Result
		rows    int
		elapsed float64
		err     error
	}
	got := make([]fetched, len(insts))
	ready := make([]chan struct{}, len(insts))
	for i := range ready {
		ready[i] = make(chan struct{})
	}
	s.progTotal(w.ID, len(insts)+missing)
	for i := 0; i < missing; i++ {
		s.progDone(w.ID)
	}
	one := func(i int) {
		defer close(ready[i])
		defer func() {
			if r := recover(); r != nil {
				got[i].err = fmt.Errorf("导出异常: %v", r)
			}
		}()
		start := time.Now()
		n, err := s.fetch(ctx, w, insts[i], prep, func(r exportpkg.Result) error { got[i].res = r; return nil })
		got[i].rows, got[i].err, got[i].elapsed = n, err, time.Since(start).Seconds()
	}
	parallel := len(insts) > 1
	for _, in := range insts {
		if in.DBType != dbTypeMSSQL {
			parallel = false
		}
	}
	workers := 1 // SQL Server 并行取数，其它类型依次取数
	if parallel {
		workers = s.batchWorkers(len(insts))
	}
	// 取出来还没写进文件的实例数设上限：写文件慢时，取数不会一路跑到底、把所有实例的结果都堆在内存里。
	// 占位按实例顺序进行，最早没写的那个实例一定拿得到位置，所以不会互相等死。
	slots := make(chan struct{}, workers*2)
	go func() { // 取数在后台跑，下面按实例顺序等结果、依次写文件，保证文件里的顺序和勾选顺序一致
		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup
		for i := range insts {
			slots <- struct{}{}
			sem <- struct{}{}
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				one(i)
			}(i)
		}
		wg.Wait()
	}()

	success := 0
	var writeErr error
	for i, in := range insts {
		<-ready[i]
		fr := &got[i]
		var r review.Row
		switch {
		case writeErr != nil:
			r = exportFailRow(prep.SQL, "导出失败: 合并文件写入出错，这个实例没有写入。")
		case fr.err != nil:
			slog.Warn("批量导出实例失败", "workflow", w.ID, "instance", in.InstanceName, "err", fr.err)
			r = exportFailRow(prep.SQL, fmt.Sprintf("导出失败: %s", exportErrText(fr.err)))
		default:
			switch err := mw.Add(fr.res); {
			case err == nil:
				success++
				r = exportRow(prep.SQL, mergedNote(fname, fr.res.Masked, fr.res.Raw), fr.rows, fr.elapsed)
			case exportpkg.Recoverable(err):
				slog.Warn("合并导出：实例没有写入合并文件", "workflow", w.ID, "instance", in.InstanceName, "err", err)
				r = exportFailRow(prep.SQL, fmt.Sprintf("导出失败: %s", errText(err)))
			default:
				writeErr = err
				slog.Error("合并导出：写合并文件出错", "workflow", w.ID, "instance", in.InstanceName, "err", err)
				r = exportFailRow(prep.SQL, fmt.Sprintf("导出失败: 合并文件写入出错：%s", errText(err)))
			}
		}
		fr.res = exportpkg.Result{} // 这个实例的数据已经写完（或放弃），及时释放内存
		<-slots                     // 让出一个取数的位置
		r.InstanceID, r.InstanceName = review.Flex(in.ID), in.InstanceName
		rows = append(rows, r)
		s.progDone(w.ID)
	}
	if writeErr == nil {
		writeErr = mw.Close()
	}
	if cerr := f.Close(); writeErr == nil {
		writeErr = cerr
	}
	return s.finishBatchExport(w, st, set, rows, dir, prep.SQL, success, len(insts), writeErr)
}

// mergedNote 是合并导出里一个实例的“导出文件”说明（同 exportedNote，写明已并入哪个文件）。
func mergedNote(name string, masked []string, raw bool) string {
	switch {
	case raw && len(masked) == 0:
		return fmt.Sprintf("已写入合并文件: %s（导出原值，未脱敏）", name)
	case raw:
		return fmt.Sprintf("已写入合并文件: %s（导出原值；SQL 防火墙强制脱敏：%s）", name, strings.Join(masked, "、"))
	case len(masked) == 0:
		return fmt.Sprintf("已写入合并文件: %s", name)
	}
	return fmt.Sprintf("已写入合并文件: %s（已脱敏：%s）", name, strings.Join(masked, "、"))
}

// retryExportInstance 重试批量导出里失败的实例。导出的产物是一个 zip，所以必须把新文件合并回已有压缩包——先取回旧包解开、补上本实例的文件、
// 重新打包上传、再删旧包——否则其余实例已导出的文件会丢失。
func (s *Service) retryExportInstance(ctx context.Context, w *model.SqlWorkflow, in *model.Instance, sqlText string) (oc instOutcome) {
	start := time.Now()
	fail := func(msg string) instOutcome {
		return instOutcome{failed: true, elapsed: time.Since(start).Seconds(),
			rows: tag([]review.Row{exportFailRow(sqlText, fmt.Sprintf("重试异常: %s", msg))}, in)}
	}
	defer func() {
		if r := recover(); r != nil {
			oc = fail(fmt.Sprint(r))
		}
	}()
	st, err := s.storage()
	if err != nil {
		return fail(err.Error())
	}
	prep, err := exportpkg.Prepare(in.DBType, sqlText)
	if err != nil {
		return fail(err.Error())
	}
	tmp, err := os.MkdirTemp("", "dbt-export-*")
	if err != nil {
		return fail(err.Error())
	}
	defer os.RemoveAll(tmp)
	dir := filepath.Join(tmp, strconv.Itoa(int(w.ID)))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fail(err.Error())
	}
	old := ""
	if w.FileName != nil {
		old = *w.FileName
	}
	if old != "" && st.Exists(old) {
		prev := filepath.Join(tmp, "_prev.zip")
		src, _, err := st.Open(old)
		if err != nil {
			return fail(fmt.Sprintf("取回已有压缩包失败: %s", err.Error()))
		}
		dst, err := os.Create(prev)
		if err == nil {
			_, err = io.Copy(dst, src)
			dst.Close()
		}
		src.Close()
		if err != nil {
			return fail(err.Error())
		}
		if err := exportpkg.Unzip(prev, tmp); err != nil {
			return fail(fmt.Sprintf("解开已有压缩包失败: %s", err.Error()))
		}
		os.Remove(prev)
	}
	var fname string
	var n int
	var masked []string
	var raw bool
	note := exportedNote
	if s.exportMerge(w) {
		// 合并成一个文件：把这个实例的行追加到已有合并文件末尾（旧文件已被清理就新建，里面只有这个实例）
		fname, note = exportpkg.MergedFileName(w.ID, exportFormat(w)), mergedNote
		n, masked, raw, err = s.exportAppend(ctx, w, in, prep, filepath.Join(dir, fname))
	} else {
		used := map[string]bool{}
		if ents, err := os.ReadDir(dir); err == nil {
			for _, e := range ents {
				used[strings.ToLower(e.Name())] = true
			}
		}
		fname = exportpkg.InstanceFileName(in.InstanceName, in.ID, exportFormat(w), used)
		n, masked, raw, err = s.exportToDir(ctx, w, in, prep, dir, fname)
	}
	if err != nil {
		return fail(exportErrText(err))
	}
	newName, err := s.zipAndSave(w, st, dir)
	if err != nil {
		return fail(fmt.Sprintf("保存导出文件失败: %s", err.Error()))
	}
	if old != "" && old != newName {
		if err := st.Delete(old); err != nil {
			slog.Warn("删除旧导出文件失败", "file", old, "err", err)
		}
	}
	return instOutcome{fileName: newName, elapsed: time.Since(start).Seconds(),
		rows: tag([]review.Row{exportRow(prep.SQL, note(fname, masked, raw), n, time.Since(start).Seconds())}, in)}
}

// exportAppend 在一个实例上导出，把结果追加到已有的合并文件末尾（文件不存在就新建）；返回行数。
func (s *Service) exportAppend(ctx context.Context, w *model.SqlWorkflow, in *model.Instance, prep *exportpkg.Prepared, path string) (int, []string, bool, error) {
	var masked []string
	var raw bool
	n, err := s.fetch(ctx, w, in, prep, func(r exportpkg.Result) error {
		masked, raw = r.Masked, r.Raw
		return exportpkg.AppendMerged(path, exportFormat(w), r)
	})
	return n, masked, raw, err
}

// ---- 下载 ----

// DownloadFile 是一个可下载的导出文件。
type DownloadFile struct {
	Name   string
	Size   int64
	Reader io.ReadSeekCloser
}

// Download 取导出文件。要求 export.download 权限、能看到这张工单、
// 且文件名必须是这张工单自己记录的文件（不接受调用方传入的文件名）。每次下载（含失败）都写操作审计日志。
func (s *Service) Download(p audit.Actor, id uint) (*DownloadFile, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	if !pm.download && !p.IsSuperuser {
		return nil, forbid("你没有下载导出文件的权限（缺少 export.download）")
	}
	w, err := s.load(id, false)
	if err != nil {
		return nil, err
	}
	if !pm.canView(p, w) {
		return nil, notFound("工单不存在")
	}
	if w.IsOfflineExport == 0 {
		return nil, bad("这不是数据导出工单")
	}
	if w.Status != model.WfFinish && w.Status != model.WfException {
		return nil, conflict(fmt.Sprintf("工单当前状态为「%s」，导出完成后才能下载", StatusLabel(w.Status)))
	}
	name := ""
	if w.FileName != nil {
		name = *w.FileName
	}
	record := func(extra string) {
		e := &model.AuditEntry{UserID: p.ID, UserName: p.Username, UserDisplay: p.Display, Action: "离线下载", // i18n-ignore 审计动作名与存档文本
			ExtraInfo: fmt.Sprintf("工单id：%d，文件：%s%s", id, name, extra)} // i18n-ignore
		if err := s.repo.InsertAuditEntry(e); err != nil {
			slog.Error("写下载审计日志失败", "workflow", id, "err", err)
		}
	}
	if name == "" {
		record("，error:没有可下载的文件。") // i18n-ignore 写进审计库的存档文本
		return nil, notFound("没有可下载的文件（导出没有成功）")
	}
	st, err := s.storage()
	if err != nil {
		record("，error:" + err.Error())
		return nil, newErr(500, "storage", err.Error())
	}
	f, size, err := st.Open(name)
	if err != nil {
		record("，error:" + err.Error())
		if errors.Is(err, exportpkg.ErrNotFound) {
			if at, ok := exportPurgedAt(id); ok {
				return nil, newErr(410, "export_purged", purgedMessage(at))
			}
			return nil, notFound("文件不存在（可能已被清理）")
		}
		slog.Error("打开导出文件失败", "workflow", id, "file", name, "err", err)
		return nil, newErr(500, "storage", "文件下载失败：请联系管理员。")
	}
	record("")
	return &DownloadFile{Name: name, Size: size, Reader: f}, nil
}
