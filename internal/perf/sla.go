// @author: kevinkelly
// @license: Apache Licence
// @file: sla.go
// @time: 2026/10/28

package perf

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/settings"
)

// ---- 级别评估 ----

// defaultSeverityRules 是 perf.severity_rules 的默认值。
const defaultSeverityRules = `{"P0":{"daily_sec":3600,"share":30,"avg_ms":10000,"per_day":1000},"P1":{"daily_sec":600,"share":10,"avg_ms":5000,"per_day":100},"P2":{"daily_sec":60,"share":3,"avg_ms":3000,"per_day":10}}`

// SeverityRule 是一个级别的判定条件，任一条满足即命中。缺项视为无穷大（该条不起作用）。
type SeverityRule struct {
	DailySec float64 `json:"daily_sec"` // 日均浪费DB时间(s)
	Share    float64 `json:"share"`     // 占实例慢SQL总耗时%
	AvgMs    float64 `json:"avg_ms"`    // 平均耗时(ms)，需同时满足 PerDay
	PerDay   float64 `json:"per_day"`   // 日均执行次数
}

const never = 1e18

func parseSeverityRules(raw string) (map[string]SeverityRule, error) {
	var m map[string]map[string]float64
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	out := map[string]SeverityRule{}
	for sev, kv := range m {
		get := func(k string) float64 {
			if v, ok := kv[k]; ok {
				return v
			}
			return never
		}
		out[sev] = SeverityRule{DailySec: get("daily_sec"), Share: get("share"), AvgMs: get("avg_ms"), PerDay: get("per_day")}
	}
	return out, nil
}

// SeverityRules 返回当前生效的级别规则：系统配置 perf.severity_rules（JSON）；不合法时用默认值。
func (s *Service) SeverityRules() map[string]SeverityRule {
	raw := s.cfgStr(settings.KeyPerfSeverityRules, defaultSeverityRules)
	rules, err := parseSeverityRules(raw)
	if err != nil {
		slog.Error("perf.severity_rules 配置不是合法 JSON，使用默认值", "err", err)
		rules, _ = parseSeverityRules(defaultSeverityRules)
	}
	return rules
}

// calcSeverity 按影响量化评估级别。
func calcSeverity(rules map[string]SeverityRule, impactSec, share, avgMs, perDay float64) string {
	for _, sev := range []string{SevP0, SevP1, SevP2} {
		r, ok := rules[sev]
		if !ok {
			r = SeverityRule{never, never, never, never}
		}
		if impactSec >= r.DailySec {
			return sev
		}
		// 占比规则需要有一定的绝对量，避免“实例上只有一条慢SQL所以占100%”被判为P0
		if share >= r.Share && impactSec >= r.DailySec*0.3 {
			return sev
		}
		if avgMs >= r.AvgMs && perDay >= r.PerDay {
			return sev
		}
	}
	return SevP3
}

func (s *Service) evaluateSeverity(i *Issue) string {
	return calcSeverity(s.SeverityRules(), i.ImpactScore, i.LoadShare, i.AvgTimeMs, i.PerDayCount)
}

// ---- SLA 策略与时限 ----

// policies 返回各级别的 SLA 策略；库里没有配置的级别用默认值。
func (s *Service) policies() map[string]SLAPolicy {
	out := map[string]SLAPolicy{}
	for _, p := range defaultSLA {
		out[p.Severity] = p
	}
	list, err := s.repo.ListSLA()
	if err != nil {
		slog.Error("读取 SLA 策略失败，使用默认值", "err", err)
		return out
	}
	for _, p := range list {
		out[p.Severity] = p
	}
	return out
}

func (s *Service) policy(sev string) SLAPolicy {
	p, ok := s.policies()[sev]
	if !ok {
		return defaultSLA[2]
	}
	return p
}

// setDeadlines 按级别重算受理/解决截止。onlyTighten 时只收紧不放宽（升级级别用），否则整体重置并清掉升级状态。
func setDeadlines(i *Issue, p SLAPolicy, now time.Time, onlyTighten bool) {
	accept := now.Add(time.Duration(p.AcceptHours) * time.Hour)
	resolve := now.Add(time.Duration(p.ResolveHours) * time.Hour)
	if onlyTighten && i.ResolveDeadline != nil {
		if i.AcceptDeadline != nil && i.AcceptDeadline.Before(accept) {
			accept = *i.AcceptDeadline
		}
		if i.ResolveDeadline.Before(resolve) {
			resolve = *i.ResolveDeadline
		}
		i.AcceptDeadline, i.ResolveDeadline = &accept, &resolve
		return
	}
	i.AcceptDeadline, i.ResolveDeadline = &accept, &resolve
	i.EscalationLevel, i.PreWarned = 0, false
}

// ---- SLA 巡检 ----

var levelName = map[int]string{1: "催办", 2: "升级至团队负责人", 3: "升级至总监"}

// inQuietTime 免打扰：P0 不受限；周末对 P2/P3 静默；perf.quiet_hours（如 22-9）时段内静默。
func (s *Service) inQuietTime(now time.Time, sev string) bool {
	if sev == SevP0 {
		return false
	}
	if s.cfgBool(settings.KeyPerfWeekendSilent, true) && (now.Weekday() == time.Saturday || now.Weekday() == time.Sunday) && (sev == SevP2 || sev == SevP3) {
		return true
	}
	parts := strings.Split(s.cfgStr(settings.KeyPerfQuietHours, "22-9"), "-")
	if len(parts) != 2 {
		return false
	}
	st, e1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	en, e2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if e1 != nil || e2 != nil {
		return false
	}
	h := now.Hour()
	if st > en {
		return st <= h || h < en
	}
	return st <= h && h < en
}

// targetLevel 当前应处的升级级别：0 正常；1 超过受理时限仍未受理；2 超过解决时限；3 超过解决时限 + l3_after_hours。
func targetLevel(i *Issue, p SLAPolicy, now time.Time) int {
	level := 0
	if (i.Status == StNew || i.Status == StAssigned) && i.AcceptDeadline != nil && now.After(*i.AcceptDeadline) {
		level = 1
	}
	if i.ResolveDeadline != nil && now.After(*i.ResolveDeadline) {
		level = 2
		if now.After(i.ResolveDeadline.Add(time.Duration(p.L3AfterHours) * time.Hour)) {
			level = 3
		}
	}
	return level
}

// SLAResult 是一次 SLA 巡检的统计。
type SLAResult struct {
	Checked int            `json:"checked"`
	Actions map[string]int `json:"actions"` // pre_warn / L1 / L2 / L3 / conflict
}

// RunSLA 巡检所有 SLA 计时中的工单：即将超时提醒、L1/L2/L3 升级、按间隔重复催办。
// 每张工单的状态更新是条件更新（update_time 乐观锁）：两个巡检进程同时跑，或与研发的操作撞上时，只有一方成功，
// 失败的一方放弃本轮（下一轮再检查），所以不会重复发催办。
func (s *Service) RunSLA() (SLAResult, error) {
	now := s.now()
	issues, err := s.repo.IssuesByStatus(slaRunning)
	if err != nil {
		return SLAResult{}, err
	}
	res := SLAResult{Checked: len(issues), Actions: map[string]int{}}
	pol := s.policies()
	for idx := range issues {
		r, err := s.checkIssue(&issues[idx], pol, now)
		if err != nil {
			slog.Error("SLA 检查失败", "issue", issues[idx].ID, "err", err)
			continue
		}
		if r != "" {
			res.Actions[r]++
		}
	}
	return res, nil
}

func (s *Service) checkIssue(i *Issue, pol map[string]SLAPolicy, now time.Time) (string, error) {
	p, ok := pol[i.Severity]
	if !ok {
		p = defaultSLA[2]
	}
	level := targetLevel(i, p, now)
	prev := i.UpdateTime
	changed := false
	if level >= 2 && !i.SLABreached {
		i.SLABreached = true
		changed = true
	}
	save := func() error {
		if !changed {
			return nil
		}
		return s.repo.UpdateIssue(i, prev)
	}
	if s.inQuietTime(now, i.Severity) {
		if err := save(); err != nil && err != ErrConflict {
			return "", err
		}
		return "", nil
	}

	// 即将超时提醒（只发一次）：距解决截止不足窗口的 20%（且至少 4 小时）
	if level == 0 && !i.PreWarned && i.ResolveDeadline != nil && i.Status == StAccepted {
		start := i.CreateTime
		if i.AcceptedAt != nil {
			start = *i.AcceptedAt
		}
		window := i.ResolveDeadline.Sub(start)
		left := i.ResolveDeadline.Sub(now)
		th := time.Duration(float64(window) * 0.2)
		if th < 4*time.Hour {
			th = 4 * time.Hour
		}
		if left <= th {
			i.PreWarned = true
			changed = true
			if err := save(); err != nil {
				return conflictOr(err, "conflict")
			}
			s.notifyEscalation(i.ID, 0, "即将超时", now)
			return "pre_warn", nil
		}
		return "", nil
	}
	if level == 0 {
		return "", nil
	}

	escalate := level > i.EscalationLevel
	repeat := level == i.EscalationLevel && (i.LastRemindAt == nil || now.Sub(*i.LastRemindAt) >= time.Duration(p.RemindIntervalHours)*time.Hour)
	if !escalate && !repeat {
		if err := save(); err != nil && err != ErrConflict {
			return "", err
		}
		return "", nil
	}
	var reason string
	if level == 1 {
		reason = fmt.Sprintf("超过受理时限(%dh)仍未受理", p.AcceptHours)
	} else {
		reason = "已超过解决时限 " + humanDelta(now.Sub(*i.ResolveDeadline))
	}
	i.EscalationLevel = level
	t := now
	i.LastRemindAt = &t
	i.RemindCount++
	changed = true
	if err := s.repo.UpdateIssue(i, prev); err != nil {
		return conflictOr(err, "conflict")
	}
	title := fmt.Sprintf("%s：%s", levelName[level], reason)
	if i.RemindCount > 1 {
		title += fmt.Sprintf("（第%d次）", i.RemindCount)
	}
	if escalate {
		s.record(i, ActEscalate, fmt.Sprintf("L%d %s：%s", level, levelName[level], reason), systemWho)
	}
	s.notifyEscalation(i.ID, level, title, now)
	return fmt.Sprintf("L%d", level), nil
}

// conflictOr 把乐观锁冲突记成一次“冲突”统计，其他错误原样返回。
func conflictOr(err error, tag string) (string, error) {
	if err == ErrConflict {
		return tag, nil
	}
	return "", err
}

// humanDelta 把时长写成“N天M小时”或“H小时M分”。
func humanDelta(d time.Duration) string {
	secs := int(d.Seconds())
	sign := ""
	if secs < 0 {
		sign, secs = "-", -secs
	}
	days, hours := secs/86400, (secs%86400)/3600
	if days > 0 {
		return sign + fmt.Sprintf("%d天%d小时", days, hours)
	}
	return sign + fmt.Sprintf("%d小时%d分", hours, (secs%3600)/60)
}
