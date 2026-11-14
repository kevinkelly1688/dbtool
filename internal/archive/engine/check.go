// @author: kevinkelly
// @license: Apache Licence
// @file: check.go
// @time: 2026/10/28

package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// 试运行相关的阈值。
const (
	CountCap          int64 = 10_000_000 // COUNT 的上限，超过则 Capped=true
	LargeBatchPercent       = 30.0       // 命中比例超过它视为大批量（key=large_batch，提交需确认）
	sampleRows              = 10
	checkQueryTimeout       = 60 * time.Second
)

// ExplainRowsLimit 是 EXPLAIN 预计扫描行数的告警阈值：全表扫描且超过它是红色，走索引但需要大范围排序 / 扫描超过它是黄色。
var ExplainRowsLimit int64 = 200_000

func (e *Engine) check(ctx context.Context, spec Spec, pol Policy, env *Env) (*CheckReport, error) {
	if env == nil || env.Src == nil {
		return nil, fmt.Errorf("Env.Src 未设置")
	}
	rep := &CheckReport{}
	add := addFn(func(key, title, level, format string, a ...any) {
		rep.Items = append(rep.Items, CheckItem{Key: key, Title: title, Level: level, Detail: fmt.Sprintf(format, a...)})
	})
	p := EffectiveParams(spec.Params, pol.Defaults)
	maxRows := EffectiveMaxRows(spec.Params, pol)

	// ---- 规格本身（纯函数校验）----
	issues := validateSpec(spec, pol, false)
	byField := func(prefix string) []Issue {
		var out []Issue
		for _, i := range issues {
			if i.Field == prefix || strings.HasPrefix(i.Field, prefix+".") {
				out = append(out, i)
			}
		}
		return out
	}
	addIssues := func(key, title string, list []Issue, okMsg string) bool {
		if len(list) == 0 {
			add(key, title, "ok", "%s", okMsg)
			return true
		}
		for _, i := range list {
			add(key, title, i.Level, "%s", i.Msg)
		}
		return !HasErrors(list)
	}
	protectedIssues := []Issue{}
	tableIssues := []Issue{}
	for _, i := range byField("table") {
		if strings.Contains(i.Msg, "受保护") { // i18n-ignore：与 validate.go 产出的中文原文做匹配
			protectedIssues = append(protectedIssues, i)
		} else {
			tableIssues = append(tableIssues, i)
		}
	}
	addIssues("protected", "受保护库表", protectedIssues, "不在受保护库表名单内")
	addIssues("retention", "保留天数", byField("retention_days"), fmt.Sprintf("保留 %d 天，不小于策略下限 %d 天", spec.RetentionDays, max(pol.MinRetentionDays, 1)))
	extraOK := addIssues("extra", "条件语法与安全", byField("extra_where"), map[bool]string{true: "没有附加条件", false: "附加条件通过检查（只含比较 / IN / LIKE / AND / OR / IS NULL）"}[strings.TrimSpace(spec.ExtraWhere) == ""])
	addIssues("mode", "模式与备份去向", byField("mode"), fmt.Sprintf("%s", modeName(spec.Mode)))
	if spec.Dest != nil {
		addIssues("dest_table_spec", "归档库表设置", byField("dest_table"), "目标库表设置合法")
	}
	if spec.File != nil {
		addIssues("dest_file_spec", "文件备份设置", byField("dest_file"), "文件备份设置合法")
	}
	addIssues("params", "稳定性参数", byField("params"), "参数在允许范围内")
	for _, i := range tableIssues {
		add("source", "来源", i.Level, "%s", i.Msg)
	}
	for _, i := range byField("db") {
		add("source", "来源", i.Level, "%s", i.Msg)
	}
	for _, i := range byField("time_column") {
		add("time_column", "时间列", i.Level, "%s", i.Msg)
	}
	for _, i := range byField("time_unit") {
		add("time_unit", "时间单位", i.Level, "%s", i.Msg)
	}
	unit, unitValid := NormalizeTimeUnit(spec.TimeUnit)
	if !validIdent(spec.DB) || !validIdent(spec.Table) {
		return rep, nil
	}
	where, werr := buildWhere(spec)
	if werr == nil {
		rep.SQL = BuildSQLPreview(spec, nil, nil, env.nowOr())
	}

	// ---- 源表结构 ----
	info, err := env.Src.Describe(ctx, spec.DB, spec.Table)
	if err != nil {
		return nil, fmt.Errorf("读取源表结构失败：%w", err)
	}
	if !info.Exists || info.IsView {
		what := "不存在"
		if info.IsView {
			what = "是视图，不能归档"
		}
		add("table", "源表", "error", "源表 %s.%s %s", spec.DB, spec.Table, what)
		return rep, nil
	}
	rep.Fingerprint = info.Fingerprint
	rep.PKCols = append([]string(nil), info.PrimaryKey...)
	rep.TableRows = info.Rows
	colNames := make([]string, len(info.Columns))
	for i, c := range info.Columns {
		colNames[i] = c.Name
	}
	if werr == nil {
		rep.SQL = buildPreviewFor(spec, info, colNames, env.nowOr())
	}
	unordered := readUnordered(spec.Mode, info) // 归档并删除 / 仅清理：不排序读取，没有断点

	_, hasPrimary := info.Indexes["PRIMARY"]
	switch {
	case len(info.PrimaryKey) == 0:
		add("pk", "主键", "error", "没有主键（含唯一非空键），不允许建任务")
	case unordered && hasPrimary:
		add("pk", "主键", "ok", "主键 (%s)：每批取走当前符合条件的前 N 行（不排序），备份校验后按主键删除", strings.Join(info.PrimaryKey, ", "))
	case unordered:
		add("pk", "主键", "ok", "没有 PRIMARY KEY，使用唯一非空键 (%s)：每批取走当前符合条件的前 N 行（不排序），备份校验后按它删除", strings.Join(info.PrimaryKey, ", "))
	case hasPrimary:
		add("pk", "主键", "ok", "主键 (%s)，按主键升序分批", strings.Join(info.PrimaryKey, ", "))
	default:
		add("pk", "主键", "ok", "没有 PRIMARY KEY，使用唯一非空键 (%s) 升序分批", strings.Join(info.PrimaryKey, ", "))
	}

	// 时间列
	var tcol *ColumnInfo
	for i := range info.Columns {
		if strings.EqualFold(info.Columns[i].Name, spec.TimeColumn) {
			tcol = &info.Columns[i]
		}
	}
	timeOK := false
	switch {
	case spec.TimeColumn == "" || !unitValid:
	case tcol == nil:
		add("time_column", "时间列", "error", "表里没有列 %s", spec.TimeColumn)
	case IsEpochUnit(unit) && !IsIntegerTimeType(tcol.Type):
		add("time_column", "时间列", "error", "时间单位选了 %d 位 Unix 时间戳，时间列必须是 int / bigint，而 %s 的类型是 %s", EpochDigits(unit), tcol.Name, tcol.Type)
	case !IsEpochUnit(unit) && IsIntegerTimeType(tcol.Type):
		add("time_column", "时间列", "error", "时间列 %s 的类型是 %s，必须是 date / datetime / timestamp；如果它存的是 Unix 时间戳，请把时间单位选为 10 位（秒）或 13 位（毫秒）", tcol.Name, tcol.Type)
	case !IsEpochUnit(unit) && !isTimeType(tcol.Type):
		add("time_column", "时间列", "error", "时间列 %s 的类型是 %s，必须是 date / datetime / timestamp", tcol.Name, tcol.Type)
	default:
		timeOK = true
		add("time_column", "时间列", "ok", "%s（%s）", tcol.Name, tcol.Type)
	}
	unitReady := true // 整型时间戳列的单位核对没有发现问题（datetime 列不涉及）
	if timeOK {
		indexed := false
		var idxName string
		for name, cols := range info.Indexes {
			if len(cols) > 0 && strings.EqualFold(cols[0], tcol.Name) {
				indexed, idxName = true, name
				break
			}
		}
		switch {
		case indexed:
			add("index", "时间列索引", "ok", "时间列是索引 %s 的最左列", idxName)
		case info.Rows > ExplainRowsLimit:
			add("index", "时间列索引", "error", "时间列 %s 上没有索引，表约 %d 行，每批读取会大范围扫描，会拖垮线上，请先建索引", tcol.Name, info.Rows)
		default:
			add("index", "时间列索引", "warn", "时间列 %s 上没有索引（表较小，约 %d 行，可以接受）", tcol.Name, info.Rows)
		}
		if IsEpochUnit(unit) {
			// 没有索引的大表不取最早 / 最晚值（会全表扫描）；抽样不受影响
			unitReady = checkTimeUnit(ctx, env, spec, unit, tcol.Name, indexed || info.Rows <= ExplainRowsLimit, add)
		}
	}
	// 附加条件里的列必须存在
	if extraOK && werr == nil {
		_, cols, _ := NormalizeExtra(spec.ExtraWhere)
		var unknown []string
		for _, c := range cols {
			found := false
			for _, ic := range info.Columns {
				if strings.EqualFold(ic.Name, c) {
					found = true
					break
				}
			}
			if !found {
				unknown = append(unknown, c)
			}
		}
		if len(unknown) > 0 {
			add("extra", "条件语法与安全", "error", "附加条件里的 %s 不是表 %s 的列", strings.Join(unknown, "、"), spec.Table)
			werr = fmt.Errorf("附加条件引用了不存在的列")
		}
	}

	// ---- 结构性提示 ----
	if len(info.FKParents) > 0 {
		add("fk", "外键", "error", "被外键子表引用：%s；本功能不处理有外键子表引用的表", strings.Join(info.FKParents, "、"))
	} else {
		add("fk", "外键", "ok", "没有外键子表引用")
	}
	if info.HasTriggers {
		add("trigger", "触发器", "warn", "表上有触发器：归档删除会触发 DELETE 触发器，请确认其副作用")
	} else {
		add("trigger", "触发器", "ok", "没有触发器")
	}
	if info.Partitioned {
		add("partition", "分区表", "warn", "这是分区表：按普通表处理分批删除；若按时间分区，更推荐用「分区维护」直接 DROP PARTITION，开销小得多")
	} else {
		add("partition", "分区表", "ok", "不是分区表")
	}
	if info.Engine != "" && !strings.EqualFold(info.Engine, "InnoDB") {
		add("engine", "存储引擎", "warn", "存储引擎是 %s，不是 InnoDB：删除不支持行级锁与回滚，请谨慎", info.Engine)
	}
	var big []string
	for _, c := range info.Columns {
		t := strings.ToLower(c.Type)
		if strings.Contains(t, "blob") || strings.Contains(t, "text") || strings.HasPrefix(t, "json") {
			big = append(big, c.Name)
		}
	}
	if len(big) > 0 {
		lvl := "ok"
		if p.BatchSize > 500 {
			lvl = "warn"
		}
		add("blob", "大字段", lvl, "含大字段列 %s，建议批大小不超过 500（当前 %d）", strings.Join(big, "、"), p.BatchSize)
	} else {
		add("blob", "大字段", "ok", "没有 BLOB / TEXT / JSON 大字段")
	}

	// ---- EXPLAIN、命中行数、样例 ----
	canQuery := werr == nil && timeOK && unitReady && len(info.PrimaryKey) > 0
	rowBytes := int64(120)
	if info.Rows > 0 && info.DataMB > 0 {
		if rb := (info.DataMB << 20) / info.Rows; rb > rowBytes {
			rowBytes = rb
		}
	}
	if canQuery {
		withAfter := !unordered && len(spec.Checkpoint) == len(info.PrimaryKey) && len(spec.Checkpoint) > 0
		// 实测首批读取：EXPLAIN 的预计扫描行数是不考虑 LIMIT 提前结束的上界估计，计划好不好只有真读一次才知道
		fr := measureFirstRead(ctx, env, spec, p, info, where, unordered, withAfter)
		frFast := fr.err == nil && fr.dur < firstReadWarn

		qctx, cancel := context.WithTimeout(ctx, checkQueryTimeout)
		q := buildReadSQL(spec.DB, spec.Table, where, info.PrimaryKey, withAfter, unordered)
		var qargs []any
		if withAfter {
			for _, v := range spec.Checkpoint {
				qargs = append(qargs, v)
			}
		}
		qargs = append(qargs, p.BatchSize)
		if ex, err := env.Src.Explain(qctx, q, qargs...); err != nil {
			add("explain", "EXPLAIN", "warn", "EXPLAIN 失败：%v", err)
		} else {
			usedNoIndex := ex.UsedIndex == "" || strings.EqualFold(ex.Type, "ALL")
			switch {
			case usedNoIndex && ex.ScanRows > ExplainRowsLimit:
				add("explain", "EXPLAIN", "error", "type=%s，未使用索引，预计扫描 ≈ %d 行，远超阈值 %d：会拖垮线上，请先建索引", ex.Type, ex.ScanRows, ExplainRowsLimit)
			case usedNoIndex:
				add("explain", "EXPLAIN", "warn", "type=%s，未使用索引，预计扫描 ≈ %d 行（表较小，可以接受）", ex.Type, ex.ScanRows)
			case ex.ScanRows > ExplainRowsLimit && strings.Contains(strings.ToLower(ex.Extra), "filesort"):
				add("explain", "EXPLAIN", "warn", "key=%s，预计扫描 ≈ %d 行且需要 filesort：每批可能都要对大量行排序，请观察首批耗时", ex.UsedIndex, ex.ScanRows)
			case ex.ScanRows > ExplainRowsLimit && frFast:
				add("explain", "EXPLAIN", "ok", "key=%s，type=%s，预计扫描 ≈ %d 行（优化器的上界估计，读够一批就会停；实测首批读取 %d 行只用了 %d ms）", ex.UsedIndex, ex.Type, ex.ScanRows, fr.rows, fr.dur.Milliseconds())
			case ex.ScanRows > ExplainRowsLimit:
				add("explain", "EXPLAIN", "warn", "key=%s，type=%s，预计扫描 ≈ %d 行，超过阈值 %d", ex.UsedIndex, ex.Type, ex.ScanRows, ExplainRowsLimit)
			default:
				add("explain", "EXPLAIN", "ok", "type=%s，key=%s，预计扫描 ≈ %d 行，未超过阈值", ex.Type, ex.UsedIndex, ex.ScanRows)
			}
		}
		fr.report(add)
		n, capped, err := env.Src.CountWhere(qctx, spec.DB, spec.Table, where, CountCap)
		if err != nil {
			add("est_rows", "命中行数", "error", "统计命中行数失败（可能超时，条件过宽或缺索引）：%v", err)
		} else {
			rep.EstRows, rep.Capped = n, capped
			if rep.TableRows < n {
				rep.TableRows = n
			}
			if rep.TableRows > 0 {
				rep.Percent = float64(n) * 100 / float64(rep.TableRows)
			}
			// 统计到上限时多一句“实际更多”：写成两套完整的格式串（不留空的 %s），翻译目录才能整句匹配
			switch {
			case n == 0:
				add("est_rows", "命中行数", "warn", "按当前条件没有符合条件的数据，任务运行后将什么也不做")
			case rep.Percent > LargeBatchPercent && capped:
				add("large_batch", "命中比例", "warn", "命中 %d 行（统计到上限，实际更多），占表约 %.1f%%，超过 %.0f%%：属于大批量，提交时需要勾选“已确认大批量”", n, rep.Percent, LargeBatchPercent)
			case rep.Percent > LargeBatchPercent:
				add("large_batch", "命中比例", "warn", "命中 %d 行，占表约 %.1f%%，超过 %.0f%%：属于大批量，提交时需要勾选“已确认大批量”", n, rep.Percent, LargeBatchPercent)
			case capped:
				add("large_batch", "命中比例", "ok", "命中 %d 行（统计到上限，实际更多），占表约 %.1f%%（未超过 %.0f%%）", n, rep.Percent, LargeBatchPercent)
			default:
				add("large_batch", "命中比例", "ok", "命中 %d 行，占表约 %.1f%%（未超过 %.0f%%）", n, rep.Percent, LargeBatchPercent)
			}
			if n > maxRows {
				add("max_rows", "单次总量上限", "warn", "命中 %d 行 > 单次上限 %d 行：需要约 %d 次运行才能清完，每次以“已达上限”结束", n, maxRows, (n+maxRows-1)/maxRows)
			} else {
				add("max_rows", "单次总量上限", "ok", "命中 %d 行 ≤ 单次上限 %d 行，一次可跑完", n, maxRows)
			}
		}
		rows, err := env.Src.Sample(qctx, spec.DB, spec.Table, where, colNames, sampleRows)
		if err != nil {
			add("sample", "样例", "warn", "取样例失败：%v", err)
		} else {
			rep.SampleCols, rep.Sample = colNames, rows
		}
		cancel()
	}
	eff := rep.EstRows
	if eff > maxRows {
		eff = maxRows
	}
	if eff > 0 {
		rep.EstBatches = int((eff + int64(p.BatchSize) - 1) / int64(p.BatchSize))
		perBatchMs := 120 + float64(p.BatchSize)*0.22
		rep.EstSeconds = int(math.Ceil(float64(rep.EstBatches) * (perBatchMs + float64(p.SleepMs)) / 1000))
		if isDeleting(spec.Mode) {
			rep.EstBinlogMB = int(math.Ceil(float64(eff) * float64(rowBytes) * 1.1 / (1 << 20)))
		}
	}

	// ---- 模式 ----
	if spec.Mode == ModePurge {
		add("purge", "不备份清理", "warn", "仅清理不备份，删除后无法恢复")
	}

	// ---- 备份去向 ----
	if spec.Dest != nil && spec.Mode != ModePurge {
		e.checkDest(ctx, env, spec, p, info, add)
	}
	if spec.File != nil && spec.Mode != ModePurge {
		e.checkFile(ctx, env, spec, rep, rowBytes, add)
	}

	// ---- 负载与从库 ----
	e.checkLoad(ctx, env, p, spec, add)

	if spec.Fingerprint != "" && spec.Fingerprint != info.Fingerprint {
		add("structure", "表结构", "warn", "源表结构与上次试运行 / 审批时不一致（列名、类型或主键有变化），本次试运行会生成新的结构指纹")
	}
	return rep, nil
}

// firstRead 是试运行里实测首批读取的结果。
type firstRead struct {
	rows     int
	dur      time.Duration
	err      error
	timedOut bool
}

// measureFirstRead 用每批真正会用的那条读取语句（只取少量行）读一次并计时。只读，不写不删；有自己的超时。
func measureFirstRead(ctx context.Context, env *Env, spec Spec, p Params, info TableInfo, where string, unordered, withAfter bool) firstRead {
	rctx, cancel := context.WithTimeout(ctx, checkQueryTimeout)
	defer cancel()
	req := ReadReq{DB: spec.DB, Table: spec.Table, Where: where, PKCols: info.PrimaryKey, Unordered: unordered, Limit: min(p.BatchSize, probeReadRows), LockWaitSe: p.LockWaitSeconds}
	if withAfter {
		req.After = spec.Checkpoint
	}
	t0 := env.nowOr()
	b, err := env.Src.ReadBatch(rctx, req)
	fr := firstRead{rows: len(b.Rows), dur: env.nowOr().Sub(t0), err: err}
	fr.timedOut = err != nil && (errors.Is(err, context.DeadlineExceeded) || rctx.Err() != nil)
	return fr
}

func (fr firstRead) report(add addFn) {
	const key, title = "first_read", "首批读取"
	switch {
	case fr.timedOut:
		add(key, title, "error", "首批读取超过 %d 秒仍没有返回：每批都会这样慢，会拖垮线上；请检查时间列索引与执行计划", int(checkQueryTimeout.Seconds()))
	case fr.err != nil:
		add(key, title, "warn", "首批读取失败：%v", fr.err)
	case fr.dur >= slowReadWarn:
		add(key, title, "error", "首批读取 %d 行耗时 %.1f 秒（超过 %d 秒）：每批都会这样慢，会拖垮线上；请检查时间列索引与执行计划", fr.rows, fr.dur.Seconds(), int(slowReadWarn.Seconds()))
	case fr.dur >= firstReadWarn:
		add(key, title, "warn", "首批读取 %d 行耗时 %.1f 秒（超过 %d 秒）：请在运行时观察每批的读取耗时", fr.rows, fr.dur.Seconds(), int(firstReadWarn.Seconds()))
	case fr.rows == 0:
		add(key, title, "ok", "首批读取没有取到行（耗时 %d ms）", fr.dur.Milliseconds())
	default:
		add(key, title, "ok", "首批读取 %d 行，耗时 %d ms", fr.rows, fr.dur.Milliseconds())
	}
}

// checkTimeUnit 核对整型时间戳列里的值与所选单位是否相符（选成 13 位的却是 10 位秒，条件会让整张表都命中）。
// 返回 false 表示发现了错误（单位或列选错）：此时不再跑 EXPLAIN / 命中行数 / 样例，按错误的单位算出来的数没有意义。
// 取不到探测结果只给黄色提示（运行时每批还会再核对一次）。
func checkTimeUnit(ctx context.Context, env *Env, spec Spec, unit, col string, rangeOK bool, add addFn) bool {
	const key, title = "time_unit", "时间单位"
	digits := EpochDigits(unit)
	pr, ok := env.Src.(TimeProber)
	if !ok {
		add(key, title, "warn", "数据源不支持核对时间单位，请自行确认列 %s 存的是 %d 位 Unix 时间戳", col, digits)
		return true
	}
	qctx, cancel := context.WithTimeout(ctx, checkQueryTimeout)
	defer cancel()
	pb, err := pr.ProbeTime(qctx, spec.DB, spec.Table, col, rangeOK)
	if err != nil {
		add(key, title, "warn", "无法核对时间单位：%v", err)
		return true
	}
	if pb.Sampled == 0 && !pb.HasRange {
		add(key, title, "warn", "列 %s 里没有大于 0 的值（表是空的，或该列全是 0 / NULL），无法核对时间单位；按这个条件也不会命中任何行", col)
		return true
	}
	lo, hi := EpochRange(unit)
	inUnit := pb.InSec
	if unit == UnitEpochMs {
		inUnit = pb.InMs
	}
	if pb.Sampled > 0 && inUnit*2 < pb.Sampled {
		if pb.GuessUnit != "" && pb.GuessUnit != unit {
			add(key, title, "error", "列 %s 的值（例如 %d）看起来是 %d 位的 Unix 时间戳，与所选的 %d 位不符，请改选对应的时间单位", col, pb.Sample, EpochDigits(pb.GuessUnit), digits)
		} else {
			add(key, title, "error", "列 %s 的值（例如 %d）不像 %d 位的 Unix 时间戳：抽样 %d 行里只有 %d 行在 1980–2100 年之间，请确认选对了列", col, pb.Sample, digits, pb.Sampled, inUnit)
		}
		return false
	}
	show := func(v int64) string { return FormatEpoch(v, unit, pb.OffsetSec) }
	if pb.HasRange {
		add(key, title, "ok", "按 %d 位 Unix 时间戳解读：最早 %s，最晚 %s（服务器时区；取值 %d … %d）", digits, show(pb.Min), show(pb.Max), pb.Min, pb.Max)
		if pb.Min < lo {
			add(key, title, "warn", "最小值 %d 小于 %d 位时间戳的合理下限（1980 年）：这些行会被当作很早的数据一起归档；若该列里混用了秒和毫秒，请先核对", pb.Min, digits)
		}
		if pb.Max > hi {
			add(key, title, "warn", "最大值 %d 大于 %d 位时间戳的合理上限（2100 年）：这些行不会被归档；若该列里混用了秒和毫秒，请先核对", pb.Max, digits)
		}
		return true
	}
	add(key, title, "ok", "按 %d 位 Unix 时间戳解读：抽样 %d 行，例如 %d 对应 %s（服务器时区；列上没有索引的大表不取最早 / 最晚值）", digits, pb.Sampled, pb.Sample, show(pb.Sample))
	if pb.Sampled > inUnit {
		add(key, title, "warn", "抽样 %d 行里有 %d 行的值不在 %d 位时间戳的合理区间内：该列里可能混用了秒和毫秒，请先核对", pb.Sampled, pb.Sampled-inUnit, digits)
	}
	return true
}

func isTimeType(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	return strings.HasPrefix(t, "datetime") || strings.HasPrefix(t, "timestamp") || t == "date"
}

func (env *Env) nowOr() time.Time {
	if env != nil && env.Now != nil {
		return env.Now()
	}
	return time.Now()
}
