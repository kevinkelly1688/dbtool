// @author: kevinkelly
// @license: Apache Licence
// @file: rules.go
// @time: 2026/10/28

package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"dbtool/internal/archive/engine"
)

// TaskInput 是新建 / 修改 / 预览 / 试运行共用的请求体。
type TaskInput struct {
	TaskID          uint              `json:"task_id,omitempty"` // 仅 preview / check：针对已有任务
	Name            string            `json:"name"`
	ResourceGroupID uint              `json:"resource_group_id"`
	InstanceID      uint              `json:"instance_id"`
	DB              string            `json:"db"`
	Table           string            `json:"table"`
	TimeColumn      string            `json:"time_column"`
	TimeUnit        string            `json:"time_unit"` // 空 / datetime：date、datetime、timestamp 列；epoch_s / epoch_ms：int、bigint 的 10 位秒 / 13 位毫秒时间戳
	RetentionDays   int               `json:"retention_days"`
	ExtraWhere      string            `json:"extra_where"`
	Mode            string            `json:"mode"`
	PurgeReason     string            `json:"purge_reason"`
	DestTable       *engine.TableDest `json:"dest_table"`
	DestFile        *engine.FileDest  `json:"dest_file"`
	Params          engine.Params     `json:"params"`
	Schedule        Schedule          `json:"schedule"`

	Submit     bool   `json:"submit"`
	CheckToken string `json:"check_token"`
	AckWarn    bool   `json:"ack_warn"`
	AckBig     bool   `json:"ack_big"`
	Version    uint   `json:"version"`

	zeroSleep bool // normalize 前批间休眠为 0（0 在服务端表示“用策略默认值”，试运行里要明确说明）
}

const (
	nameMin, nameMax = 2, 50
	extraMax         = 500
	retentionMax     = 3650
	purgeReasonMin   = 5
	purgeReasonMax   = 500

	policyMaxRowsMin  = 1000       // 安全策略“单次运行最大行数”下限
	maxRowsLimit      = 2000000000 // 行数类上限（策略与任务共用）
	storeRetentionMax = 36500      // 备份存储保留天数上限
)

// ---- 安全策略 ----

// PolicyDefaults 是新任务的默认稳定性参数。
type PolicyDefaults struct {
	BatchSize              int `json:"batch_size"`
	SleepMs                int `json:"sleep_ms"`
	MaxThreadsRunning      int `json:"max_threads_running"`
	MaxLagSeconds          int `json:"max_lag_seconds"`
	MinDiskFreePct         int `json:"min_disk_free_pct"`
	TargetBatchMs          int `json:"target_batch_ms"`
	MaxWaitSeconds         int `json:"max_wait_seconds"`
	MaxConsecutiveFailures int `json:"max_consecutive_failures"`
}

// Policy 是全局安全策略（PolicyView）。
type Policy struct {
	MinRetentionDays         int            `json:"min_retention_days"`
	AllowPurgeNoBackup       bool           `json:"allow_purge_no_backup"`
	ProtectedPatterns        []string       `json:"protected_patterns"`
	MaxRowsPerRun            int64          `json:"max_rows_per_run"`
	MaxConcurrentPerInstance int            `json:"max_concurrent_per_instance"`
	Defaults                 PolicyDefaults `json:"defaults"`
	UpdatedBy                string         `json:"updated_by"`
	UpdatedAt                string         `json:"updated_at"`
	IsDefault                bool           `json:"is_default"`
}

func defaultPolicy() Policy {
	return Policy{
		MinRetentionDays: 30, AllowPurgeNoBackup: false,
		ProtectedPatterns: []string{"mysql", "information_schema", "performance_schema", "sys"},
		MaxRowsPerRun:     5000000, MaxConcurrentPerInstance: 2,
		Defaults: PolicyDefaults{BatchSize: 1000, SleepMs: 200, MaxThreadsRunning: 32, MaxLagSeconds: 5, MinDiskFreePct: 15,
			TargetBatchMs: 500, MaxWaitSeconds: 600, MaxConsecutiveFailures: 3},
		IsDefault: true,
	}
}

func (p Policy) engineParams() engine.Params {
	d := p.Defaults
	return engine.Params{BatchSize: d.BatchSize, SleepMs: d.SleepMs, MaxRows: p.MaxRowsPerRun, MaxThreadsRunning: d.MaxThreadsRunning,
		MaxLagSeconds: d.MaxLagSeconds, MinDiskFreePct: d.MinDiskFreePct, TargetBatchMs: d.TargetBatchMs, MaxWaitSeconds: d.MaxWaitSeconds,
		MaxConsecutiveFailures: d.MaxConsecutiveFailures, LockWaitSeconds: 3, Window: engine.Window{Start: "01:00", End: "05:00"}}
}

func (p Policy) engine() engine.Policy {
	return engine.Policy{MinRetentionDays: p.MinRetentionDays, AllowPurgeNoBackup: p.AllowPurgeNoBackup,
		ProtectedPatterns: append([]string(nil), p.ProtectedPatterns...), MaxRowsPerRun: p.MaxRowsPerRun, Defaults: p.engineParams()}
}

// fillParams 把值为 0 / 空的字段补成策略默认值（引擎契约：零值 = 用默认）。
func (p Policy) fillParams(in engine.Params) engine.Params {
	d := p.engineParams()
	pick := func(v, def int) int {
		if v == 0 {
			return def
		}
		return v
	}
	out := in
	out.BatchSize, out.SleepMs = pick(in.BatchSize, d.BatchSize), pick(in.SleepMs, d.SleepMs)
	if out.MaxRows == 0 {
		out.MaxRows = d.MaxRows
	}
	out.MaxThreadsRunning, out.MaxLagSeconds = pick(in.MaxThreadsRunning, d.MaxThreadsRunning), pick(in.MaxLagSeconds, d.MaxLagSeconds)
	out.MinDiskFreePct, out.TargetBatchMs = pick(in.MinDiskFreePct, d.MinDiskFreePct), pick(in.TargetBatchMs, d.TargetBatchMs)
	out.MaxWaitSeconds, out.MaxConsecutiveFailures = pick(in.MaxWaitSeconds, d.MaxWaitSeconds), pick(in.MaxConsecutiveFailures, d.MaxConsecutiveFailures)
	out.LockWaitSeconds = pick(in.LockWaitSeconds, d.LockWaitSeconds)
	if strings.TrimSpace(out.Window.Start) == "" && strings.TrimSpace(out.Window.End) == "" {
		out.Window = d.Window
	}
	out.Window.Start, out.Window.End = strings.TrimSpace(out.Window.Start), strings.TrimSpace(out.Window.End)
	return out
}

// ---- 输入规整 ----

// normalize 规整输入（去空白、按模式清理不适用字段、补默认值）。不做校验。
func (in *TaskInput) normalize(pol Policy) {
	in.Name = strings.TrimSpace(in.Name)
	in.DB, in.Table, in.TimeColumn = strings.TrimSpace(in.DB), strings.TrimSpace(in.Table), strings.TrimSpace(in.TimeColumn)
	if u, ok := engine.NormalizeTimeUnit(in.TimeUnit); ok { // 不认识的值原样保留，交给引擎校验报错
		in.TimeUnit = u
	}
	in.ExtraWhere = strings.TrimSpace(in.ExtraWhere)
	in.Mode = strings.TrimSpace(strings.ToLower(in.Mode))
	in.PurgeReason = strings.TrimSpace(in.PurgeReason)
	in.CheckToken = strings.TrimSpace(in.CheckToken)
	in.zeroSleep = in.Params.SleepMs == 0
	in.Params = pol.fillParams(in.Params)
	if in.Mode == string(engine.ModePurge) {
		in.DestTable, in.DestFile = nil, nil
	} else {
		in.PurgeReason = ""
	}
	if in.DestTable != nil {
		d := *in.DestTable
		d.DB, d.Template, d.Table = strings.TrimSpace(d.DB), strings.TrimSpace(d.Template), ""
		if d.InstanceID == 0 {
			d.InstanceID = in.InstanceID
		}
		in.DestTable = &d
	}
	if in.DestFile != nil {
		f := *in.DestFile
		f.Format = strings.ToLower(strings.TrimSpace(f.Format))
		if f.Format == "" {
			f.Format = "csv"
		}
		f.PathTemplate = strings.TrimSpace(f.PathTemplate)
		if f.PartMB == 0 {
			f.PartMB = 256
		}
		in.DestFile = &f
	}
	s := in.Schedule
	s.Kind, s.Time, s.Cron = strings.ToLower(strings.TrimSpace(s.Kind)), strings.TrimSpace(s.Time), strings.TrimSpace(s.Cron)
	if s.Kind == schedWeek {
		seen := map[int]bool{}
		var days []int
		for _, d := range s.Weekdays {
			if !seen[d] {
				seen[d] = true
				days = append(days, d)
			}
		}
		sort.Ints(days)
		s.Weekdays = days
	} else {
		s.Weekdays = nil
	}
	if s.Kind != schedMonth {
		s.MonthDay = 0
	}
	if s.Kind == schedCron {
		s.Time = ""
	} else {
		s.Cron = ""
	}
	in.Schedule = s
}

// spec 把输入转成引擎 Spec（试运行 / 预览用）。
func (in *TaskInput) spec() engine.Spec {
	return engine.Spec{TaskID: in.TaskID, Name: in.Name, InstanceID: in.InstanceID, DB: in.DB, Table: in.Table,
		TimeColumn: in.TimeColumn, TimeUnit: in.TimeUnit, RetentionDays: in.RetentionDays, ExtraWhere: in.ExtraWhere, Mode: engine.Mode(in.Mode),
		Dest: in.DestTable, File: in.DestFile, Params: in.Params}
}

// issues 做不依赖外部状态的校验：引擎 Validate + 服务层规则（名称、不备份理由、调度）。
func (in *TaskInput) issues(pol Policy, now time.Time) []engine.Issue {
	var out []engine.Issue
	add := func(field, msg string) { out = append(out, engine.Issue{Field: field, Level: "error", Msg: msg}) }
	if n := utf8.RuneCountInString(in.Name); n < nameMin || n > nameMax {
		add("name", fmt.Sprintf("任务名称需要 %d–%d 个字（当前 %d）", nameMin, nameMax, n))
	}
	if in.ResourceGroupID == 0 {
		add("resource_group_id", "请选择所属资源组")
	}
	if in.RetentionDays > retentionMax {
		add("retention_days", fmt.Sprintf("保留天数最大 %d 天", retentionMax))
	}
	if n := utf8.RuneCountInString(in.ExtraWhere); n > extraMax {
		add("extra_where", fmt.Sprintf("附加条件最长 %d 字（当前 %d 字）", extraMax, n))
	}
	switch engine.Mode(in.Mode) {
	case engine.ModeMove, engine.ModeCopy, engine.ModePurge:
	default:
		add("mode", "模式只能是 move（归档并删除）/ copy（仅备份）/ purge（仅清理）")
	}
	if in.Mode == string(engine.ModePurge) {
		if !pol.AllowPurgeNoBackup {
			add("mode", "安全策略当前不允许“不备份清理”（仅超级用户可在「安全策略」里放开）")
		}
		if n := utf8.RuneCountInString(in.PurgeReason); n < purgeReasonMin {
			add("purge_reason", fmt.Sprintf("仅清理必须填写“不备份理由”（至少 %d 个字），审批人会看到", purgeReasonMin))
		} else if n > purgeReasonMax {
			add("purge_reason", fmt.Sprintf("不备份理由最多 %d 字", purgeReasonMax))
		}
	}
	out = append(out, engine.Validate(in.spec(), pol.engine())...)
	out = append(out, validateSchedule(in.Schedule, in.Params.Window, now)...)
	return dedupeIssues(out)
}

func dedupeIssues(in []engine.Issue) []engine.Issue {
	seen := map[engine.Issue]bool{}
	var out []engine.Issue
	for _, x := range in {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func hasErrorIssue(is []engine.Issue) bool {
	for _, x := range is {
		if x.Level == "error" {
			return true
		}
	}
	return false
}

// ---- 关键字段哈希 / 变更判断 ----

// keyHash 是“关键字段”（来源、条件、模式、备份去向、总量上限）的哈希：试运行 token 与它绑定，任一改动即失效；
// 修改任务时它变了就必须重新审批。
func (in *TaskInput) keyHash() string { return in.hashKey(true) }

// hashKeyNoRows 不含总量上限：判断“是否需要重新审批”时用（总量只降不升，降低不需要审批；提高由 looserParts 识别）。
func (in *TaskInput) hashKeyNoRows() string { return in.hashKey(false) }

func (in *TaskInput) hashKey(withRows bool) string {
	type key struct {
		Inst    uint
		RG      uint
		DB, T   string
		TimeCol string
		Unit    string `json:",omitempty"` // 空（datetime）时不进哈希：升级前已批准的任务哈希不变，不会被误判为“改过关键字段”
		Days    int
		Extra   string
		Mode    string
		Reason  string
		Dest    *engine.TableDest
		File    *engine.FileDest
		MaxRows int64
	}
	k := key{Inst: in.InstanceID, RG: in.ResourceGroupID, DB: in.DB, T: in.Table, TimeCol: in.TimeColumn, Unit: in.TimeUnit, Days: in.RetentionDays, Extra: in.ExtraWhere, Mode: in.Mode,
		Reason: in.PurgeReason, Dest: in.DestTable, File: in.DestFile, MaxRows: in.Params.MaxRows}
	if !withRows {
		k.MaxRows = 0
	}
	sum := sha256.Sum256([]byte(mustJSON(k)))
	return hex.EncodeToString(sum[:16])
}

// inputOf 把保存的任务还原成输入形态（用于对比、预览）。
func inputOf(t *Task) TaskInput {
	c := t.cfg()
	return TaskInput{TaskID: t.ID, Name: t.Name, ResourceGroupID: t.ResourceGroupID, InstanceID: t.InstanceID, DB: t.DBName, Table: t.TblName,
		TimeColumn: t.TimeColumn, TimeUnit: t.TimeUnit, RetentionDays: t.RetentionDays, ExtraWhere: t.ExtraWhere, Mode: t.Mode, PurgeReason: t.PurgeReason,
		DestTable: c.DestTable, DestFile: c.DestFile, Params: c.Params, Schedule: c.Schedule, Version: t.Version}
}

// looserParts 列出相对 old，new 放宽了的参数（设计 §6：批量只减不增、休眠只增不减、窗口只缩不扩、总量只降不升，阈值同理）。
func looserParts(o, n engine.Params) []string {
	var l []string
	if n.BatchSize > o.BatchSize {
		l = append(l, "批大小增大")
	}
	if n.SleepMs < o.SleepMs {
		l = append(l, "批间休眠减小")
	}
	if n.MaxRows > o.MaxRows {
		l = append(l, "单次总量上限提高")
	}
	if n.MaxThreadsRunning > o.MaxThreadsRunning {
		l = append(l, "Threads_running 阈值提高")
	}
	if n.MaxLagSeconds > o.MaxLagSeconds {
		l = append(l, "复制延迟阈值放宽")
	}
	if n.MinDiskFreePct < o.MinDiskFreePct {
		l = append(l, "磁盘剩余阈值降低")
	}
	if n.TargetBatchMs > o.TargetBatchMs {
		l = append(l, "批耗时目标放宽")
	}
	if n.MaxWaitSeconds > o.MaxWaitSeconds {
		l = append(l, "负载最长等待延长")
	}
	if n.MaxConsecutiveFailures > o.MaxConsecutiveFailures {
		l = append(l, "连续失败暂停阈值提高")
	}
	ow := windowSet(o.Window)
	for m := range windowSet(n.Window) {
		if !ow[m] {
			l = append(l, "执行窗口扩大")
			break
		}
	}
	return l
}

// describeChanges 生成给人看的变更说明（审计 / 变更历史用）。
func describeChanges(o, n TaskInput) []string {
	var d []string
	if o.Params.BatchSize != n.Params.BatchSize {
		d = append(d, fmt.Sprintf("批大小 %d → %d", o.Params.BatchSize, n.Params.BatchSize))
	}
	if o.Params.SleepMs != n.Params.SleepMs {
		d = append(d, fmt.Sprintf("批间休眠 %d → %d ms", o.Params.SleepMs, n.Params.SleepMs))
	}
	if o.Params.MaxRows != n.Params.MaxRows {
		d = append(d, fmt.Sprintf("单次上限 %d → %d 行", o.Params.MaxRows, n.Params.MaxRows))
	}
	if o.Params.MaxThreadsRunning != n.Params.MaxThreadsRunning {
		d = append(d, fmt.Sprintf("Threads_running 阈值 %d → %d", o.Params.MaxThreadsRunning, n.Params.MaxThreadsRunning))
	}
	if o.Params.MaxLagSeconds != n.Params.MaxLagSeconds {
		d = append(d, fmt.Sprintf("复制延迟阈值 %d → %d 秒", o.Params.MaxLagSeconds, n.Params.MaxLagSeconds))
	}
	if o.Params.MinDiskFreePct != n.Params.MinDiskFreePct {
		d = append(d, fmt.Sprintf("磁盘剩余阈值 %d%% → %d%%", o.Params.MinDiskFreePct, n.Params.MinDiskFreePct))
	}
	if o.Params.TargetBatchMs != n.Params.TargetBatchMs {
		d = append(d, fmt.Sprintf("批耗时目标 %d → %d ms", o.Params.TargetBatchMs, n.Params.TargetBatchMs))
	}
	if o.Params.MaxWaitSeconds != n.Params.MaxWaitSeconds {
		d = append(d, fmt.Sprintf("负载最长等待 %d → %d 秒", o.Params.MaxWaitSeconds, n.Params.MaxWaitSeconds))
	}
	if o.Params.MaxConsecutiveFailures != n.Params.MaxConsecutiveFailures {
		d = append(d, fmt.Sprintf("连续失败暂停 %d → %d 批", o.Params.MaxConsecutiveFailures, n.Params.MaxConsecutiveFailures))
	}
	if o.Params.Window != n.Params.Window {
		d = append(d, fmt.Sprintf("窗口 %s–%s → %s–%s", o.Params.Window.Start, o.Params.Window.End, n.Params.Window.Start, n.Params.Window.End))
	}
	if scheduleText(o.Schedule) != scheduleText(n.Schedule) {
		d = append(d, fmt.Sprintf("调度 %s → %s", scheduleText(o.Schedule), scheduleText(n.Schedule)))
	}
	if o.Name != n.Name {
		d = append(d, fmt.Sprintf("名称 %s → %s", o.Name, n.Name))
	}
	if o.hashKeyNoRows() != n.hashKeyNoRows() {
		d = append(d, "关键字段（来源 / 条件 / 模式 / 备份去向 / 总量上限）有修改")
	}
	return d
}

// needsApproval 判断把 old 改成 nw 是否需要重新审批；返回原因。
func needsApproval(old *Task, o, nw TaskInput) (bool, string) {
	switch old.Status {
	case StRejected, StDisabled, StDraft, StPending:
		return true, fmt.Sprintf("任务当前状态（%s）需要提交审批", statusLabel(old.Status))
	}
	if old.Status == StPaused && old.PauseKind == PauseStructure {
		return true, "源表结构变更后需要重新试运行并提交审批"
	}
	if o.hashKeyNoRows() != nw.hashKeyNoRows() {
		return true, "修改了关键字段（来源 / 条件 / 模式 / 备份去向 / 总量上限）"
	}
	if l := looserParts(o.Params, nw.Params); len(l) > 0 {
		return true, fmt.Sprintf("放宽了参数：%s", strings.Join(l, "、"))
	}
	return false, ""
}

func statusLabel(s string) string {
	switch s {
	case StDraft:
		return "草稿"
	case StPending:
		return "待审批"
	case StRejected:
		return "已驳回"
	case StEnabled:
		return "已启用"
	case StPaused:
		return "已暂停"
	case StDisabled:
		return "已停用"
	}
	return s
}

func modeLabel(m string) string {
	switch m {
	case "move":
		return "归档并删除"
	case "copy":
		return "仅备份"
	case "purge":
		return "仅清理"
	}
	return m
}
