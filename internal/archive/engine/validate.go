// @author: kevinkelly
// @license: Apache Licence
// @file: validate.go
// @time: 2026/10/28

package engine

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

func isDeleting(m Mode) bool { return m == ModeMove || m == ModePurge }

// validateSpec 校验规格。forSave=true 时包含名称等只和“保存 / 提交”有关的检查；Run / Check 传 false。
func validateSpec(spec Spec, pol Policy, forSave bool) []Issue {
	var out []Issue
	add := func(field, level, format string, a ...any) {
		out = append(out, Issue{Field: field, Level: level, Msg: fmt.Sprintf(format, a...)})
	}
	errf := func(field, format string, a ...any) { add(field, "error", format, a...) }

	if forSave {
		n := utf8.RuneCountInString(strings.TrimSpace(spec.Name))
		if n < nameMin || n > nameMax {
			errf("name", "任务名称需要 %d–%d 个字（当前 %d）", nameMin, nameMax, n)
		}
	}

	// ---- 来源 ----
	if spec.InstanceID == 0 {
		errf("instance_id", "请选择实例")
	}
	srcOK := true
	if !validIdent(spec.DB) {
		errf("db", "库名无效：%s（只允许字母、数字、_ $ -，最长 64）", quote(spec.DB))
		srcOK = false
	}
	if !validIdent(spec.Table) {
		errf("table", "表名无效：%s（只允许字母、数字、_ $ -，最长 64）", quote(spec.Table))
		srcOK = false
	}
	if srcOK && IsProtected(pol.ProtectedPatterns, spec.DB, spec.Table) {
		errf("table", "%s.%s 属于受保护库表（安全策略），永不允许归档", spec.DB, spec.Table)
	}

	// ---- 条件 ----
	if !validIdent(spec.TimeColumn) {
		errf("time_column", "请选择时间列（列名无效：%s）", quote(spec.TimeColumn))
	}
	if _, ok := NormalizeTimeUnit(spec.TimeUnit); !ok {
		errf("time_unit", "时间单位无效：%s（只能是 datetime / epoch_s / epoch_ms）", quote(spec.TimeUnit))
	}
	minRet := pol.MinRetentionDays
	if minRet < 1 {
		minRet = 1
	}
	switch {
	case spec.RetentionDays < 1:
		errf("retention_days", "保留天数必须是正整数")
	case spec.RetentionDays < minRet:
		errf("retention_days", "保留天数不得小于安全策略的最小保留天数 %d 天（想归档最近 %d 天内的数据，请联系管理员修改“安全策略”）", minRet, minRet)
	case spec.RetentionDays > maxRetentionDays:
		errf("retention_days", "保留天数最大 %d 天", maxRetentionDays)
	}
	if _, _, err := NormalizeExtra(spec.ExtraWhere); err != nil {
		errf("extra_where", "附加条件：%s", err.Error())
	}

	// ---- 模式与备份去向 ----
	switch spec.Mode {
	case ModeMove, ModeCopy:
		if spec.Dest == nil && spec.File == nil {
			errf("mode", "请至少选择一个备份去向（归档库表 / 文件存储）；“不备份”只能用模式“仅清理”")
		}
	case ModePurge:
		if spec.Dest != nil || spec.File != nil {
			errf("mode", "仅清理模式不备份，不能同时配置备份去向")
		}
		if !pol.AllowPurgeNoBackup {
			errf("mode", "安全策略当前不允许“不备份清理”（仅超级用户可在「安全策略」里放开）")
		}
	default:
		errf("mode", "模式无效：%s（move / copy / purge）", quote(string(spec.Mode)))
	}
	if d := spec.Dest; d != nil {
		if d.InstanceID == 0 {
			errf("dest_table.instance_id", "请选择归档库表所在的实例")
		}
		if !validIdent(d.DB) {
			errf("dest_table.db", "归档库名无效：%s", quote(d.DB))
		} else if IsProtected(pol.ProtectedPatterns, d.DB, "") {
			errf("dest_table.db", "归档库 %s 属于受保护库", d.DB)
		}
		tableName := d.Table
		if tableName == "" {
			if err := ValidTableTemplate(d.Template); err != nil {
				errf("dest_table.template", "%s", err.Error())
			} else {
				tableName = ExpandTableName(d.Template, spec.Table, refTime)
				if n := utf8.RuneCountInString(expandTableNameRaw(d.Template, spec.Table, refTime)); n > 64 {
					errf("dest_table.template", "模板展开后的表名超过 64 位（%d 位，源表名 %s）", n, spec.Table)
				}
			}
		} else if !validIdent(tableName) {
			errf("dest_table.table", "归档表名无效：%s", quote(tableName))
			tableName = ""
		}
		if d.Table != "" && d.Template != "" {
			if err := ValidTableTemplate(d.Template); err != nil {
				errf("dest_table.template", "%s", err.Error())
			}
		}
		if tableName != "" && srcOK && d.InstanceID == spec.InstanceID && d.DB == spec.DB && strings.EqualFold(tableName, spec.Table) {
			errf("dest_table.table", "目标表不能就是源表")
		}
		if tableName != "" && srcOK && d.InstanceID == spec.InstanceID && d.DB == spec.DB && d.Template != "" {
			// 模板展开后可能在某个月份 / 年份撞上源表：用几个代表性时间都试一下
			for _, t := range []int{1, 6, 12} {
				if strings.EqualFold(ExpandTableName(d.Template, spec.Table, refTime.AddDate(0, t-1, 0)), spec.Table) {
					errf("dest_table.template", "模板在某些日期会展开成源表本身，请换一个模板")
					break
				}
			}
		}
	}
	if f := spec.File; f != nil {
		if f.StoreID == 0 {
			errf("dest_file.store_id", "请选择备份存储")
		}
		if f.Format != "csv" && f.Format != "sql" {
			errf("dest_file.format", "文件格式只能是 csv 或 sql")
		}
		if err := ValidPathTemplate(f.PathTemplate); err != nil {
			errf("dest_file.path_template", "%s", err.Error())
		} else if !strings.Contains(f.PathTemplate, "{run_id}") {
			add("dest_file.path_template", "warn", "路径模板里没有 {run_id}：同一天多次运行会写到同一目录下（引擎会自动避开已存在的文件名，但不便于按运行查找）")
		}
		if f.PartMB != 0 && (f.PartMB < minPartMB || f.PartMB > maxPartMB) {
			errf("dest_file.part_mb", "单个文件大小上限需要在 %d–%d MB 之间（0 表示默认 %d）", minPartMB, maxPartMB, defPartMB)
		}
	}

	// ---- 稳定性参数（合并默认值后校验）----
	p := EffectiveParams(spec.Params, pol.Defaults)
	rng := func(field, name string, v, lo, hi int, unit string) {
		if v < lo || v > hi {
			if unit == "" { // 不留空的 %s：翻译时按格式串整句匹配，空参数匹配不上
				errf(field, "%s需要在 %d–%d 之间（当前 %d）", name, lo, hi, v)
			} else {
				errf(field, "%s需要在 %d–%d%s 之间（当前 %d）", name, lo, hi, unit, v)
			}
		}
	}
	rng("params.batch_size", "批大小", p.BatchSize, minBatch, maxBatch, " 行")
	rng("params.sleep_ms", "批间休眠", p.SleepMs, 0, maxSleepMs, " ms")
	if isDeleting(spec.Mode) && p.SleepMs < minDeleteSleepMs {
		errf("params.sleep_ms", "会删除源表数据的模式（归档并删除 / 仅清理）批间休眠不得低于 %d ms", minDeleteSleepMs)
	}
	if p.MaxRows < minRows {
		errf("params.max_rows", "单次运行最多处理行数不能小于 %d", minRows)
	} else if pol.MaxRowsPerRun > 0 && p.MaxRows > pol.MaxRowsPerRun {
		add("params.max_rows", "warn", "单次运行行数上限 %d 超过安全策略的硬上限 %d，实际运行将按 %d 截断", p.MaxRows, pol.MaxRowsPerRun, pol.MaxRowsPerRun)
	}
	rng("params.max_threads_running", "Threads_running 阈值", p.MaxThreadsRunning, minThreads, maxThreads, "")
	if p.MaxLagSeconds >= 0 {
		rng("params.max_lag_seconds", "复制延迟阈值", p.MaxLagSeconds, 1, maxLagLimit, " 秒")
	} else {
		add("params.max_lag_seconds", "warn", "已关闭复制延迟检查；若源实例有从库，运行会被拒绝")
	}
	rng("params.min_disk_free_pct", "磁盘剩余阈值", p.MinDiskFreePct, minDisk, maxDisk, " %")
	rng("params.target_batch_ms", "批耗时目标", p.TargetBatchMs, minTargetMs, maxTargetMs, " ms")
	rng("params.max_wait_seconds", "负载最长等待", p.MaxWaitSeconds, minWaitSec, maxWaitSec, " 秒")
	rng("params.max_consecutive_failures", "连续失败暂停阈值", p.MaxConsecutiveFailures, 1, maxFailLimit, " 批")
	rng("params.lock_wait_seconds", "锁等待超时", p.LockWaitSeconds, 1, maxLockWait, " 秒")

	// ---- 执行窗口 ----
	w := p.Window
	if !w.Unlimited() {
		d := WindowMinutes(w)
		switch {
		case d < 0:
			errf("params.window", "执行窗口格式应为 HH:MM")
		case d < minWindowMinutes:
			errf("params.window", "执行窗口至少 %d 分钟（当前 %d 分钟）", minWindowMinutes, d)
		}
	}
	if len(spec.Checkpoint) > 16 {
		errf("checkpoint", "断点格式不对")
	}
	return out
}

// HasErrors 判断问题列表里是否有 error 级。
func HasErrors(issues []Issue) bool {
	for _, i := range issues {
		if i.Level == "error" {
			return true
		}
	}
	return false
}

// IssuesError 把 error 级问题合并成一个 error（没有则返回 nil）。
func IssuesError(issues []Issue) error {
	var msgs []string
	for _, i := range issues {
		if i.Level == "error" {
			msgs = append(msgs, i.Msg)
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return &ValidationError{Issues: issues, msg: fmt.Sprintf("规格校验未通过：%s", strings.Join(msgs, "；"))}
}

// ValidationError 是 Run / Check 在规格非法时返回的错误。
type ValidationError struct {
	Issues []Issue
	msg    string
}

func (e *ValidationError) Error() string { return e.msg }
