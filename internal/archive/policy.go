// @author: kevinkelly
// @license: Apache Licence
// @file: policy.go
// @time: 2026/10/28

package archive

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"dbtool/internal/archive/engine"
)

// limits 是接口层公开的取值范围（GET /meta 的 limits）。引擎 Validate 是权威校验，这里的数值与它保持一致，供前端提前提示。
type limitsView struct {
	NameLen              [2]int `json:"name_len"`
	RetentionMaxDays     int    `json:"retention_max_days"`
	ExtraWhereMax        int    `json:"extra_where_max"`
	BatchSize            [2]int `json:"batch_size"`
	SleepMs              [2]int `json:"sleep_ms"`
	SleepMsMinWhenDelete int    `json:"sleep_ms_min_when_delete"`
	MaxThreadsRunning    [2]int `json:"max_threads_running"`
	MaxLagSeconds        [2]int `json:"max_lag_seconds"`
	MinDiskFreePct       [2]int `json:"min_disk_free_pct"`
	MaxWaitSeconds       [2]int `json:"max_wait_seconds"`
	TargetBatchMs        [2]int `json:"target_batch_ms"`
	MaxConsecutiveFails  [2]int `json:"max_consecutive_failures"`
	WindowMinMinutes     int    `json:"window_min_minutes"`
	CronMinIntervalMin   int    `json:"cron_min_interval_minutes"`
	MonthDay             [2]int `json:"month_day"`
	PartMB               [2]int `json:"part_mb"`
	PurgeReason          [2]int `json:"purge_reason"`        // 不备份理由字数
	RunMaxRows           [2]int `json:"run_max_rows"`        // 任务级“单次运行最多处理行数”
	PolicyMaxRows        [2]int `json:"policy_max_rows"`     // 安全策略的“单次运行最大行数”
	StoreRetentionMax    int    `json:"store_retention_max"` // 备份存储保留天数上限（0 = 永久）
	CheckValidDays       int    `json:"check_valid_days"`
	BigPercent           int    `json:"big_percent"`
}

var limits = limitsView{
	NameLen: [2]int{nameMin, nameMax}, RetentionMaxDays: retentionMax, ExtraWhereMax: extraMax,
	BatchSize: [2]int{100, 10000}, SleepMs: [2]int{0, 5000}, SleepMsMinWhenDelete: 50,
	MaxThreadsRunning: [2]int{4, 1000}, MaxLagSeconds: [2]int{1, 3600}, MinDiskFreePct: [2]int{5, 50},
	MaxWaitSeconds: [2]int{60, 3600}, TargetBatchMs: [2]int{100, 5000}, MaxConsecutiveFails: [2]int{1, 10},
	WindowMinMinutes: minWindowMinutes, CronMinIntervalMin: minCronGapMinutes, MonthDay: [2]int{1, 28}, PartMB: [2]int{engine.PartMBMin, engine.PartMBMax},
	PurgeReason: [2]int{purgeReasonMin, purgeReasonMax}, RunMaxRows: [2]int{engine.RunMaxRowsMin, maxRowsLimit}, PolicyMaxRows: [2]int{policyMaxRowsMin, maxRowsLimit},
	StoreRetentionMax: storeRetentionMax,
	CheckValidDays:    checkValidDays, BigPercent: int(bigPercent),
}

var builtinProtected = []string{"mysql", "information_schema", "performance_schema", "sys"}

var patternRe = regexp.MustCompile(`^[A-Za-z0-9_$.*?\-]{1,128}$`)

// loadPolicy 读取当前策略；没有记录（或表未建）用内置默认值。
func (s *Service) loadPolicy() Policy {
	def := defaultPolicy()
	if !s.repo.Ready() {
		return def
	}
	row, err := s.repo.GetPolicy()
	if err != nil || row == nil {
		return def
	}
	p := def
	if err := json.Unmarshal([]byte(row.Content), &p); err != nil {
		return def
	}
	p.IsDefault, p.UpdatedBy = false, row.UpdatedBy
	p.UpdatedAt = row.UpdatedAt.Format(timeLayout)
	p.ProtectedPatterns = withBuiltin(p.ProtectedPatterns)
	return p
}

func withBuiltin(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range append(append([]string(nil), builtinProtected...), in...) {
		p = strings.TrimSpace(p)
		if p != "" && !seen[strings.ToLower(p)] {
			seen[strings.ToLower(p)] = true
			out = append(out, p)
		}
	}
	return out
}

// validate 规整并校验策略，返回错误列表（field, msg）。
func (p *Policy) validate() []policyErr {
	var errs []policyErr
	bad := func(field, format string, a ...any) { errs = append(errs, policyErr{field, fmt.Sprintf(format, a...)}) }
	rng := func(field, label string, v int, lo, hi int) {
		if v < lo || v > hi {
			bad(field, "%s需要在 %d–%d 之间", label, lo, hi)
		}
	}
	rng("min_retention_days", "最小保留天数", p.MinRetentionDays, 1, retentionMax)
	if p.MaxRowsPerRun < policyMaxRowsMin || p.MaxRowsPerRun > maxRowsLimit {
		bad("max_rows_per_run", "单次运行最大行数需要在 1,000–2,000,000,000 之间")
	}
	rng("max_concurrent_per_instance", "每实例最大并发运行", p.MaxConcurrentPerInstance, 1, 10)
	d := p.Defaults
	rng("defaults.batch_size", "默认批量", d.BatchSize, limits.BatchSize[0], limits.BatchSize[1])
	rng("defaults.sleep_ms", "默认批间休眠", d.SleepMs, limits.SleepMsMinWhenDelete, limits.SleepMs[1])
	rng("defaults.max_threads_running", "Threads_running 阈值", d.MaxThreadsRunning, limits.MaxThreadsRunning[0], limits.MaxThreadsRunning[1])
	rng("defaults.max_lag_seconds", "复制延迟阈值", d.MaxLagSeconds, limits.MaxLagSeconds[0], limits.MaxLagSeconds[1])
	rng("defaults.min_disk_free_pct", "磁盘剩余阈值", d.MinDiskFreePct, limits.MinDiskFreePct[0], limits.MinDiskFreePct[1])
	rng("defaults.target_batch_ms", "批耗时目标", d.TargetBatchMs, limits.TargetBatchMs[0], limits.TargetBatchMs[1])
	rng("defaults.max_wait_seconds", "负载等待上限", d.MaxWaitSeconds, limits.MaxWaitSeconds[0], limits.MaxWaitSeconds[1])
	rng("defaults.max_consecutive_failures", "连续失败暂停阈值", d.MaxConsecutiveFailures, limits.MaxConsecutiveFails[0], limits.MaxConsecutiveFails[1])
	var pats []string
	for _, x := range p.ProtectedPatterns {
		x = strings.TrimSpace(x)
		if x == "" {
			continue
		}
		if !patternRe.MatchString(x) || utf8.RuneCountInString(x) > 128 {
			bad("protected_patterns", "受保护库表模式「%s」不合法：只允许字母、数字、_ $ . - 与通配符 * ?，格式为 库 或 库.表", x)
			continue
		}
		pats = append(pats, x)
	}
	p.ProtectedPatterns = withBuiltin(pats)
	return errs
}

type policyErr struct {
	Field string `json:"field"`
	Msg   string `json:"msg"`
}

// PolicyChange 是策略差异的一项。
type PolicyChange struct {
	Field string `json:"field"`
	Label string `json:"label"`
	From  string `json:"from"`
	To    string `json:"to"`
	Risk  bool   `json:"risk"`
}

// PolicyDiff 是 POST /policy/diff 的结果。
type PolicyDiff struct {
	Changes []PolicyChange `json:"changes"`
	Errors  []policyErr    `json:"errors"`
	Risks   []string       `json:"risks"`
}

func joinSorted(l []string) string {
	c := append([]string(nil), l...)
	sort.Strings(c)
	if len(c) == 0 {
		return "（空）"
	}
	return strings.Join(c, " ")
}

func diffPolicy(o, n Policy) ([]PolicyChange, []string) {
	var ch []PolicyChange
	var risks []string
	add := func(field, label, from, to string, risk bool, riskMsg string) {
		if from == to {
			return
		}
		ch = append(ch, PolicyChange{Field: field, Label: label, From: from, To: to, Risk: risk})
		if risk && riskMsg != "" {
			risks = append(risks, riskMsg)
		}
	}
	itoa := func(v int) string { return fmt.Sprint(v) }
	add("min_retention_days", "最小保留天数", itoa(o.MinRetentionDays), itoa(n.MinRetentionDays), n.MinRetentionDays < o.MinRetentionDays,
		fmt.Sprintf("最小保留天数降低到 %d 天：将允许归档最近 %d 天内的数据", n.MinRetentionDays, n.MinRetentionDays))
	yn := func(b bool) string {
		if b {
			return "允许"
		}
		return "不允许"
	}
	add("allow_purge_no_backup", "允许不备份清理", yn(o.AllowPurgeNoBackup), yn(n.AllowPurgeNoBackup), n.AllowPurgeNoBackup && !o.AllowPurgeNoBackup,
		"打开“允许不备份清理”：任务可以不备份直接删除数据，删除后无法恢复")
	add("max_rows_per_run", "单次运行最大行数", fmt.Sprint(o.MaxRowsPerRun), fmt.Sprint(n.MaxRowsPerRun), n.MaxRowsPerRun > o.MaxRowsPerRun,
		"提高单次运行最大行数：单次运行的影响范围变大")
	add("max_concurrent_per_instance", "每实例最大并发运行", itoa(o.MaxConcurrentPerInstance), itoa(n.MaxConcurrentPerInstance), n.MaxConcurrentPerInstance > o.MaxConcurrentPerInstance,
		"提高每实例并发上限：同一实例上同时归档的任务变多，注意负载")
	shrunk := false
	on := map[string]bool{}
	for _, p := range o.ProtectedPatterns {
		on[strings.ToLower(p)] = true
	}
	nn := map[string]bool{}
	for _, p := range n.ProtectedPatterns {
		nn[strings.ToLower(p)] = true
	}
	for p := range on {
		if !nn[p] {
			shrunk = true
		}
	}
	add("protected_patterns", "受保护库表", joinSorted(o.ProtectedPatterns), joinSorted(n.ProtectedPatterns), shrunk, "缩小了受保护库表范围：被移出名单的库表将允许创建归档任务")
	od, nd := o.Defaults, n.Defaults
	add("defaults.batch_size", "默认批量", itoa(od.BatchSize), itoa(nd.BatchSize), false, "")
	add("defaults.sleep_ms", "默认批间休眠(ms)", itoa(od.SleepMs), itoa(nd.SleepMs), false, "")
	add("defaults.max_threads_running", "Threads_running 阈值", itoa(od.MaxThreadsRunning), itoa(nd.MaxThreadsRunning), false, "")
	add("defaults.max_lag_seconds", "复制延迟阈值(秒)", itoa(od.MaxLagSeconds), itoa(nd.MaxLagSeconds), false, "")
	add("defaults.min_disk_free_pct", "磁盘剩余阈值(%)", itoa(od.MinDiskFreePct), itoa(nd.MinDiskFreePct), false, "")
	add("defaults.target_batch_ms", "批耗时目标(ms)", itoa(od.TargetBatchMs), itoa(nd.TargetBatchMs), false, "")
	add("defaults.max_wait_seconds", "负载等待上限(秒)", itoa(od.MaxWaitSeconds), itoa(nd.MaxWaitSeconds), false, "")
	add("defaults.max_consecutive_failures", "连续失败暂停阈值", itoa(od.MaxConsecutiveFailures), itoa(nd.MaxConsecutiveFailures), false, "")
	return ch, risks
}

// GetPolicy 读取安全策略（archive.cfg_view）。
func (s *Service) GetPolicy(c Caller) (*Policy, error) {
	if err := c.need("archive.cfg_view", "查看安全策略"); err != nil {
		return nil, err
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	p := s.loadPolicy()
	return &p, nil
}

// DiffPolicy 预览待保存策略的差异（不落库）。
func (s *Service) DiffPolicy(c Caller, in Policy) (*PolicyDiff, error) {
	if err := c.need("archive.cfg_view", "查看安全策略"); err != nil {
		return nil, err
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	d := &PolicyDiff{Changes: []PolicyChange{}, Errors: in.validate(), Risks: []string{}}
	if d.Errors == nil {
		d.Errors = []policyErr{}
	}
	ch, risks := diffPolicy(s.loadPolicy(), in)
	if ch != nil {
		d.Changes = ch
	}
	if risks != nil {
		d.Risks = risks
	}
	return d, nil
}

// SavePolicy 保存安全策略：锁定码，仅超级用户；需要二次确认；写审计（含差异）。
func (s *Service) SavePolicy(c Caller, in Policy, confirm bool) (*Policy, error) {
	if err := c.need("archive.policy_edit", "编辑安全策略"); err != nil {
		return nil, err
	}
	if !c.IsSuperuser {
		return nil, newErr(403, "super_only", "安全策略只有超级用户可以修改")
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	if errs := in.validate(); len(errs) > 0 {
		return nil, withData(newErr(400, "validation", "%s", errs[0].Msg), map[string]any{"issues": policyIssues(errs)})
	}
	if !confirm {
		return nil, confirmNeeded("修改安全策略需要二次确认（confirm=true）")
	}
	old := s.loadPolicy()
	ch, _ := diffPolicy(old, in)
	in.IsDefault = false
	body := in
	body.UpdatedBy, body.UpdatedAt, body.IsDefault = "", "", false
	raw, _ := json.Marshal(body)
	now := s.now()
	if err := s.repo.SavePolicy(&PolicyRow{Content: string(raw), UpdatedBy: c.Username, UpdatedAt: now}); err != nil {
		return nil, err
	}
	var parts []string
	for _, x := range ch {
		parts = append(parts, fmt.Sprintf("%s：%s → %s", x.Label, x.From, x.To))
	}
	if len(parts) == 0 {
		parts = append(parts, "无变化")
	}
	s.auditAt(c, "策略修改", strings.Join(parts, "；")) // i18n-ignore：审计 / 通知正文，不翻译
	p := s.loadPolicy()
	return &p, nil
}

func policyIssues(errs []policyErr) []map[string]string {
	out := make([]map[string]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, map[string]string{"field": e.Field, "level": "error", "msg": e.Msg})
	}
	return out
}
