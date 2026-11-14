// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

// Package firewalladmin 是“安全合规 › SQL 防火墙”（仅超级用户）：规则的增删改与启停、观察 / 生效切换、语句测试台、
// 历史回放、命中记录、例外名单、规则模板，以及全局模式与总开关。求值本身在 internal/firewall。
package firewalladmin

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/firewall"
	"dbtool/internal/instext"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// InstanceRepo 是需要的实例与系统配置读取能力（由 server.Repo 提供）。
type InstanceRepo interface {
	ListInstances() ([]model.Instance, error)
	ConfigValue(item string) (string, bool)
}

// Invalidator 让规则快照立即失效（*firewall.Engine 满足）。保存规则 / 例外 / 全局设置后调用；
// 多实例部署的其他进程最长 firewall.SnapshotTTL 后生效。
type Invalidator interface{ Invalidate() }

// 系统配置项（dbt_setting）。
const (
	cfgEnabled   = settings.KeyFirewallEnabled
	cfgMode      = settings.KeyFirewallMode
	cfgOnError   = settings.KeyFirewallOnError
	cfgStore     = settings.KeyFirewallStoreSQL
	cfgExplainMs = settings.KeyFirewallExplainTimeoutMs
)

// 审计动作名。统一以 audit.ActionPrefixFirewall 开头，审计页归到“系统管理 › SQL 防火墙”；
// ExtraInfo 以 audit.FirewallRiskyMark 开头的标记为高危（规则由观察切到生效 / 全局切到观察 / 总开关关闭）。
const (
	actRule      = audit.ActionPrefixFirewall + "规则变更" // i18n-ignore 审计文本，存库后不翻译
	actException = audit.ActionPrefixFirewall + "例外"   // i18n-ignore 审计文本，存库后不翻译
	actMode      = audit.ActionPrefixFirewall + "模式"   // i18n-ignore 审计文本，存库后不翻译
	actSwitch    = audit.ActionPrefixFirewall + "总开关"  // i18n-ignore 审计文本，存库后不翻译
)

// activeScenes 是已经接入执行的场景：在线查询（P2）、SQL 工单与数据导出（P4）。没接入的场景规则可以先保存，页面上标注“尚未接入”。
var activeScenes = map[string]bool{firewall.SceneQuery: true, firewall.SceneWorkflow: true, firewall.SceneExport: true}

// pendingConditions 是可以配置、但当前版本还没有事实来源（求值为“未评估”、不命中）的条件类型。
// P5 之后预估扫描行数（EXPLAIN）与预估影响行数（工单：goInception 检查结果 / EXPLAIN 上界）都有了事实来源，列表为空。
var pendingConditions = []string{}

var sceneLabels = map[string]string{
	firewall.SceneQuery:    "在线查询",
	firewall.SceneWorkflow: "SQL 工单",
	firewall.SceneExport:   "数据导出",
}

// StatsSource 是引擎可选提供的运行统计（*firewall.Engine 满足）：EXPLAIN 的使用情况与规则级求值统计。
type StatsSource interface {
	ExplainStats() firewall.ExplainStats
	RuleStats() map[uint]firewall.RuleStat
}

// 慢规则 / 未评估占比的判定：求值次数不足时不下结论。
const (
	statMinEvals  = 5
	slowRuleAvgMs = 100.0 // 设计 §11：含语法树的求值预算 100 毫秒；EXPLAIN 的耗时算在规则头上
	blindRulePct  = 20
)

// Service 是防火墙管理的业务层。
type Service struct {
	repo  InstanceRepo
	st    Store
	inv   Invalidator
	stats StatsSource
	now   func() time.Time
}

// NewService 创建服务。inv 可为 nil（没有引擎的测试 / 演示）。
func NewService(repo InstanceRepo, st Store, inv Invalidator) *Service {
	s := &Service{repo: repo, st: st, inv: inv, now: time.Now}
	if ss, ok := inv.(StatsSource); ok {
		s.stats = ss
	}
	return s
}

func (s *Service) ruleStatsSnapshot() map[uint]firewall.RuleStat {
	if s.stats == nil {
		return nil
	}
	return s.stats.RuleStats()
}

func (s *Service) invalidate() {
	if s.inv != nil {
		s.inv.Invalidate()
	}
}

func (s *Service) cfg(item string) string {
	v, _ := s.repo.ConfigValue(item)
	return strings.ToLower(strings.TrimSpace(v))
}

func (s *Service) globalMode() string {
	if s.cfg(cfgMode) == firewall.ModeObserve {
		return firewall.ModeObserve
	}
	return firewall.ModeEnforce
}

func (s *Service) switchOn() bool { return s.cfg(cfgEnabled) != "false" }

// ready 返回表是否都已建；没建时写操作给出明确提示。
func (s *Service) ready() error {
	if m := s.st.Missing(); len(m) > 0 {
		return conflict("not_ready", fmt.Sprintf("还没有建防火墙的表（%s）：请先在元数据库执行 deploy/sql/240_firewall.sql（或 dbt-admin bootstrap --only-sql --apply），然后重试。", strings.Join(m, "、")))
	}
	return nil
}

func (s *Service) auditEntry(actor audit.Actor, action, extra string, risky bool) *model.AuditEntry {
	if risky {
		extra = audit.FirewallRiskyMark + extra
	}
	return &model.AuditEntry{UserID: actor.ID, UserName: actor.Username, UserDisplay: actor.Display, Action: action, ExtraInfo: truncate(extra, 1800)}
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func timeText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04")
}

// ---- 选项 ----

// Meta 返回编辑器与测试台用的选项。
func (s *Service) Meta() (*Meta, error) {
	m := &Meta{Groups: []GroupItem{}, Instances: []InstanceItem{}, Envs: []EnvItem{}, Pending: pendingConditions,
		MaxRules: firewall.MaxRules, MaxConds: firewall.MaxConditions, BigInItems: firewall.BigInItems}
	groups, err := s.st.Groups()
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		m.Groups = append(m.Groups, GroupItem{ID: g.ID, Name: g.Name})
	}
	for _, d := range instext.EnvDefs() {
		m.Envs = append(m.Envs, EnvItem{Code: d.Code, Name: d.Name, Prod: d.Prod})
	}
	insts, err := s.instances()
	if err != nil {
		return nil, err
	}
	m.Instances = insts
	for _, sc := range firewall.Scenes {
		it := SceneItem{Key: sc, Label: sceneLabels[sc], Active: activeScenes[sc]}
		if !it.Active {
			it.Note = "尚未接入：规则可以先配置，当前版本不会对这个场景执行"
		}
		m.Scenes = append(m.Scenes, it)
	}
	for _, k := range []string{firewall.FeatNoWhere, firewall.FeatNoLimit, firewall.FeatCartesian, firewall.FeatMultiStmt,
		firewall.FeatSelectStar, firewall.FeatLeadingWildcard, firewall.FeatBigIn} {
		m.Features = append(m.Features, FeatureItem{Key: k, Label: firewall.FeatureLabel(k)})
	}
	return m, nil
}

func (s *Service) instances() ([]InstanceItem, error) {
	list, err := s.repo.ListInstances()
	if err != nil {
		return nil, err
	}
	out := make([]InstanceItem, 0, len(list))
	for _, in := range list {
		out = append(out, InstanceItem{ID: in.ID, Name: in.InstanceName, DBType: in.DBType, Env: envOf(in)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ---- 概览 ----

func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// Overview 返回顶部统计与全局状态。命中类统计只看今天；“长期零命中”看近 7 天。
func (s *Service) Overview() (*Overview, error) {
	now := s.now()
	ov := &Overview{Enabled: s.switchOn(), Mode: s.globalMode(), OnError: "allow", StoreSQL: "masked"}
	if s.cfg(cfgOnError) == "deny" {
		ov.OnError = "deny"
	}
	if s.cfg(cfgStore) == "raw" {
		ov.StoreSQL = "raw"
	}
	ov.ExplainTimeoutMs = int(firewall.DefaultExplainTimeout.Milliseconds())
	if v := s.cfg(cfgExplainMs); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ov.ExplainTimeoutMs = min(max(n, 200), 30000)
		}
	}
	if s.stats != nil {
		if es := s.stats.ExplainStats(); es.Calls > 0 || es.CacheHits > 0 || len(es.Open) > 0 {
			ov.Explain = &es
		}
		for _, st := range s.stats.RuleStats() {
			if st.Evals < statMinEvals {
				continue
			}
			if float64(st.TotalNs)/float64(st.Evals)/1e6 >= slowRuleAvgMs {
				ov.SlowRules++
			}
			if int(st.Unknown*100/st.Evals) >= blindRulePct {
				ov.BlindRules++
			}
		}
	}
	ov.Missing = s.st.Missing()
	ov.Ready = len(ov.Missing) == 0
	rules, err := s.st.ListRules()
	if err != nil {
		return nil, err
	}
	ov.RulesTotal = len(rules)
	if !ov.Ready {
		return ov, nil
	}
	hits7, err := s.st.DayHits(dayStart(now).AddDate(0, 0, -6))
	if err != nil {
		return nil, err
	}
	total7 := map[uint]int64{}
	for _, h := range hits7 {
		total7[h.RuleID] += h.N
	}
	for _, m := range rules {
		if !m.Enabled {
			continue
		}
		ov.RulesEnabled++
		if m.Mode == firewall.ModeObserve {
			ov.RulesObserving++
		}
		if total7[m.ID] == 0 && m.CreatedAt.Before(now.AddDate(0, 0, -7)) {
			ov.RulesIdle++
		}
	}
	sum, err := s.st.HitSummary(dayStart(now))
	if err != nil {
		return nil, err
	}
	ov.UsersToday, ov.RulesHitToday = sum.Users, sum.Rules
	for _, c := range sum.Counts {
		ov.HitsToday += c.N
		switch c.Effective {
		case firewall.ActDeny:
			ov.BlockedToday += c.N
		case firewall.ActReason:
			ov.ReasonToday += c.N
		case firewall.ActLimit, firewall.ActMask:
			ov.TightenToday += c.N
		case firewall.ActWarn:
			ov.WarnToday += c.N
		case "observe":
			ov.ObserveToday += c.N
			if c.WouldAction == firewall.ActDeny {
				ov.WouldDenyToday += c.N
			}
		}
	}
	return ov, nil
}

// ---- 规则 ----

func nz[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// sparkOf 把 (规则, 日期) 的命中次数整理成近 7 天的数组（最后一个是今天）。
func (s *Service) sparkOf(rows []DayHits) (map[uint][]int64, map[uint]int64) {
	today := dayStart(s.now())
	idx := map[string]int{}
	for i := 0; i < 7; i++ {
		idx[today.AddDate(0, 0, i-6).Format("2006-01-02")] = i
	}
	spark := map[uint][]int64{}
	total := map[uint]int64{}
	for _, r := range rows {
		i, ok := idx[r.Day]
		if !ok {
			continue
		}
		if spark[r.RuleID] == nil {
			spark[r.RuleID] = make([]int64, 7)
		}
		spark[r.RuleID][i] += r.N
		total[r.RuleID] += r.N
	}
	return spark, total
}

func (s *Service) viewOf(m model.FirewallRule, spark map[uint][]int64, total map[uint]int64, stats map[uint]firewall.RuleStat) RuleView {
	r := firewall.RuleFromModel(m)
	v := RuleView{
		ID: m.ID, Name: m.Name, Description: m.Description, Priority: m.Priority, Enabled: m.Enabled, Mode: m.Mode,
		Scenes: nz(r.Scenes), Env: m.Env, InstanceIDs: nz(r.InstanceIDs), GroupMode: m.GroupMode, GroupIDs: nz(r.GroupIDs),
		Match: m.MatchMode, Conditions: nz(r.Conditions), Action: m.Action, ActionArg: m.ActionArg, Message: m.Message,
		Notify: m.Notify, UpdatedBy: m.UpdatedBy, UpdatedAt: timeText(m.UpdatedAt), CreatedAt: timeText(m.CreatedAt),
		Spark: make([]int64, 7), Hits7: total[m.ID],
	}
	if sp := spark[m.ID]; sp != nil {
		v.Spark = sp
	}
	if st, ok := stats[m.ID]; ok && st.Evals > 0 {
		v.Evals = st.Evals
		v.AvgMs = float64(st.TotalNs) / float64(st.Evals) / 1e6
		v.MaxMs = float64(st.MaxNs) / 1e6
		v.UnknownPct = int(st.Unknown * 100 / st.Evals)
		if st.Evals >= statMinEvals {
			v.Slow = v.AvgMs >= slowRuleAvgMs
			v.Blind = v.UnknownPct >= blindRulePct
		}
	}
	if m.ObserveSince != nil {
		v.ObserveSince = timeText(*m.ObserveSince)
		if m.Mode == firewall.ModeObserve {
			v.ObserveDays = int(dayStart(s.now()).Sub(dayStart(*m.ObserveSince)).Hours()/24) + 1
		}
	}
	if _, err := firewall.Normalize(r); err != nil {
		v.Invalid = err.Error()
	}
	return v
}

// Rules 返回全部规则（含停用的）与近 7 天命中。
func (s *Service) Rules() (*RuleList, error) {
	list := &RuleList{Rules: []RuleView{}, MaxRules: firewall.MaxRules}
	list.Missing = s.st.Missing()
	list.Ready = len(list.Missing) == 0
	if !list.Ready {
		return list, nil
	}
	rows, err := s.st.ListRules()
	if err != nil {
		return nil, err
	}
	days, err := s.st.DayHits(dayStart(s.now()).AddDate(0, 0, -6))
	if err != nil {
		return nil, err
	}
	spark, total := s.sparkOf(days)
	stats := s.ruleStatsSnapshot()
	for _, m := range rows {
		list.Rules = append(list.Rules, s.viewOf(m, spark, total, stats))
	}
	return list, nil
}

// Rule 返回一条规则。
func (s *Service) Rule(id uint) (*RuleView, error) {
	m, err := s.st.GetRule(id)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, notFound("规则不存在")
	}
	days, err := s.st.DayHits(dayStart(s.now()).AddDate(0, 0, -6))
	if err != nil {
		return nil, err
	}
	spark, total := s.sparkOf(days)
	v := s.viewOf(*m, spark, total, s.ruleStatsSnapshot())
	return &v, nil
}

// toRule 校验并规整请求里的规则：用内核的 Normalize，再核对实例与权限组是否存在。
func (s *Service) toRule(in RuleInput) (firewall.Rule, error) {
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	r := firewall.Rule{
		Name: in.Name, Description: in.Description, Priority: in.Priority, Enabled: enabled, Mode: in.Mode, Scenes: in.Scenes,
		Env: in.Env, InstanceIDs: in.InstanceIDs, GroupMode: in.GroupMode, GroupIDs: in.GroupIDs, Match: in.Match,
		Conditions: in.Conditions, Action: in.Action, ActionArg: in.ActionArg, Message: in.Message, Notify: in.Notify,
	}
	n, err := firewall.Normalize(r)
	if err != nil {
		return n, badReq(err.Error())
	}
	if len(n.InstanceIDs) > 0 {
		insts, err := s.instances()
		if err != nil {
			return n, err
		}
		known := map[uint]bool{}
		for _, i := range insts {
			known[i.ID] = true
		}
		for _, id := range n.InstanceIDs {
			if !known[id] {
				return n, badReq(fmt.Sprintf("实例 %d 不存在", id))
			}
		}
	}
	if len(n.GroupIDs) > 0 {
		groups, err := s.st.Groups()
		if err != nil {
			return n, err
		}
		known := map[uint]bool{}
		for _, g := range groups {
			known[g.ID] = true
		}
		for _, id := range n.GroupIDs {
			if !known[id] {
				return n, badReq(fmt.Sprintf("权限组 %d 不存在", id))
			}
		}
	}
	return n, nil
}

func (s *Service) nameTaken(name string, except uint) (bool, error) {
	rows, err := s.st.ListRules()
	if err != nil {
		return false, err
	}
	for _, m := range rows {
		if m.ID != except && strings.EqualFold(strings.TrimSpace(m.Name), name) {
			return true, nil
		}
	}
	return false, nil
}

// CreateRule 新建规则。缺省观察模式、启用；保存后快照立即失效。
func (s *Service) CreateRule(actor audit.Actor, in RuleInput) (*RuleView, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	r, err := s.toRule(in)
	if err != nil {
		return nil, err
	}
	rows, err := s.st.ListRules()
	if err != nil {
		return nil, err
	}
	if len(rows) >= firewall.MaxRules {
		return nil, badReq(fmt.Sprintf("规则数量已达上限 %d，请先清理不再使用的规则", firewall.MaxRules))
	}
	return s.insert(actor, r, "新建规则") // i18n-ignore 审计文本，存库后不翻译
}

func (s *Service) insert(actor audit.Actor, r firewall.Rule, verb string) (*RuleView, error) {
	if taken, err := s.nameTaken(r.Name, 0); err != nil {
		return nil, err
	} else if taken {
		return nil, conflict("duplicate_name", fmt.Sprintf("已有同名规则「%s」，请换一个名称", r.Name))
	}
	now := s.now()
	r.UpdatedBy, r.CreatedAt, r.UpdatedAt = actor.Username, now, now
	if r.Mode == firewall.ModeObserve {
		r.ObserveSince = &now
	}
	m := firewall.RuleToModel(r)
	entry := s.auditEntry(actor, actRule, fmt.Sprintf("%s「%s」：%s", verb, r.Name, ruleSummary(r)), r.Mode == firewall.ModeEnforce && r.Enabled)
	if err := s.st.CreateRule(&m, entry); err != nil {
		if err == ErrDuplicateName {
			return nil, conflict("duplicate_name", fmt.Sprintf("已有同名规则「%s」，请换一个名称", r.Name))
		}
		return nil, err
	}
	s.invalidate()
	v := s.viewOf(m, nil, nil, nil)
	return &v, nil
}

// UpdateRule 整体修改一条规则。内容没有变化时不写库、不记审计。
func (s *Service) UpdateRule(actor audit.Actor, id uint, in RuleInput) (*RuleView, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	cur, err := s.st.GetRule(id)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, notFound("规则不存在")
	}
	old := firewall.RuleFromModel(*cur)
	r, err := s.toRule(in)
	if err != nil {
		return nil, err
	}
	if taken, err := s.nameTaken(r.Name, id); err != nil {
		return nil, err
	} else if taken {
		return nil, conflict("duplicate_name", fmt.Sprintf("已有同名规则「%s」，请换一个名称", r.Name))
	}
	diff := diffRules(old, r)
	r.ID, r.CreatedAt, r.ObserveSince = id, old.CreatedAt, old.ObserveSince
	if r.Mode == firewall.ModeObserve && (old.Mode != firewall.ModeObserve || old.ObserveSince == nil) {
		now := s.now()
		r.ObserveSince = &now
	}
	if len(diff) == 0 {
		v := s.viewOf(*cur, nil, nil, nil)
		return &v, nil
	}
	r.UpdatedBy, r.UpdatedAt = actor.Username, s.now()
	m := firewall.RuleToModel(r)
	risky := old.Mode != firewall.ModeEnforce && r.Mode == firewall.ModeEnforce
	entry := s.auditEntry(actor, actRule, fmt.Sprintf("修改规则「%s」：%s", old.Name, strings.Join(diff, "；")), risky) // i18n-ignore 审计文本，存库后不翻译
	if err := s.st.UpdateRule(&m, entry); err != nil {
		if err == ErrDuplicateName {
			return nil, conflict("duplicate_name", fmt.Sprintf("已有同名规则「%s」，请换一个名称", r.Name))
		}
		return nil, err
	}
	s.invalidate()
	return s.Rule(id)
}

// DeleteRule 删除规则及其例外；命中记录保留（记录里冗余了规则名）。
func (s *Service) DeleteRule(actor audit.Actor, id uint) (int64, error) {
	cur, err := s.st.GetRule(id)
	if err != nil {
		return 0, err
	}
	if cur == nil {
		return 0, notFound("规则不存在")
	}
	r := firewall.RuleFromModel(*cur)
	entry := s.auditEntry(actor, actRule, fmt.Sprintf("删除规则「%s」（%s）；命中记录保留，该规则的例外一并删除", r.Name, ruleSummary(r)), false) // i18n-ignore 审计文本，存库后不翻译
	n, err := s.st.DeleteRule(id, entry)
	if err != nil {
		return 0, err
	}
	s.invalidate()
	return n, nil
}

// ToggleRule 启用 / 停用一条规则。库里已经无效的规则不允许启用（启用了也不会生效，页面上要如实告知）。
func (s *Service) ToggleRule(actor audit.Actor, id uint, enabled bool) (*RuleView, error) {
	cur, err := s.st.GetRule(id)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, notFound("规则不存在")
	}
	r := firewall.RuleFromModel(*cur)
	if enabled {
		if _, err := firewall.Normalize(r); err != nil {
			return nil, badReq(fmt.Sprintf("这条规则当前无效，不能启用：%s。请先编辑修正。", err.Error()))
		}
	}
	if cur.Enabled != enabled {
		now := s.now()
		verb := map[bool]string{true: "启用", false: "停用"}[enabled]                                               // i18n-ignore 审计文本，存库后不翻译
		entry := s.auditEntry(actor, actRule, fmt.Sprintf("%s规则「%s」（%s）", verb, r.Name, ruleSummary(r)), false) // i18n-ignore 审计文本，存库后不翻译
		if err := s.st.PatchRule(id, map[string]any{"enabled": enabled, "updated_by": actor.Username, "updated_at": now}, entry); err != nil {
			return nil, err
		}
		s.invalidate()
	}
	return s.Rule(id)
}

// SetRuleMode 切换一条规则的观察 / 生效。切到生效但近 7 天没有任何命中记录时，在结果里提示（不阻止）。
func (s *Service) SetRuleMode(actor audit.Actor, id uint, mode string) (*ModeResult, error) {
	if mode != firewall.ModeObserve && mode != firewall.ModeEnforce {
		return nil, badReq("模式只能是 observe 或 enforce")
	}
	cur, err := s.st.GetRule(id)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, notFound("规则不存在")
	}
	r := firewall.RuleFromModel(*cur)
	if mode == firewall.ModeEnforce {
		if _, err := firewall.Normalize(r); err != nil {
			return nil, badReq(fmt.Sprintf("这条规则当前无效，不能切到生效：%s。请先编辑修正。", err.Error()))
		}
	}
	days, err := s.st.DayHits(dayStart(s.now()).AddDate(0, 0, -6))
	if err != nil {
		return nil, err
	}
	_, total := s.sparkOf(days)
	res := &ModeResult{Observed: total[id]}
	if cur.Mode != mode {
		now := s.now()
		fields := map[string]any{"mode": mode, "updated_by": actor.Username, "updated_at": now}
		var text string
		if mode == firewall.ModeObserve {
			fields["observe_since"] = now
			text = fmt.Sprintf("规则「%s」由生效改为观察（只记录命中，不再执行动作「%s」）", r.Name, actionLabel(r.Action)) // i18n-ignore 审计文本，存库后不翻译
		} else {
			text = fmt.Sprintf("规则「%s」由观察切到生效（开始执行动作「%s」；近 7 天命中 %d 次）", r.Name, actionLabel(r.Action), res.Observed) // i18n-ignore 审计文本，存库后不翻译
		}
		if err := s.st.PatchRule(id, fields, s.auditEntry(actor, actRule, text, mode == firewall.ModeEnforce)); err != nil {
			return nil, err
		}
		s.invalidate()
		if mode == firewall.ModeEnforce && res.Observed == 0 {
			res.Warning = "这条规则近 7 天没有任何命中记录（没有观察数据），已按你的选择切到生效；建议留意命中记录，必要时改回观察。"
		}
	}
	v, err := s.Rule(id)
	if err != nil {
		return nil, err
	}
	res.Rule = *v
	return res, nil
}

// ---- 全局模式与总开关 ----

// SetMode 设置全局模式：observe = 所有规则只记录命中（新规则上线前的评估闸门）/ enforce = 按各规则自己的模式。
func (s *Service) SetMode(actor audit.Actor, mode string) (*SwitchResult, error) {
	if mode != firewall.ModeObserve && mode != firewall.ModeEnforce {
		return nil, badReq("全局模式只能是 observe 或 enforce")
	}
	res := &SwitchResult{Enabled: s.switchOn(), Mode: s.globalMode()}
	if res.Mode == mode {
		return res, nil
	}
	var text string
	if mode == firewall.ModeObserve {
		text = "全局模式：拦截 → 观察（所有规则只记录命中，不再拦截任何语句）" // i18n-ignore 审计文本，存库后不翻译
	} else {
		text = "全局模式：观察 → 拦截（“生效”状态的规则开始执行动作）" // i18n-ignore 审计文本，存库后不翻译
	}
	if err := s.st.SetConfig(cfgMode, mode, "SQL 防火墙全局模式：observe 只记录 / enforce 按规则执行", s.auditEntry(actor, actMode, text, mode == firewall.ModeObserve)); err != nil { // i18n-ignore 审计文本，存库后不翻译
		return nil, err
	}
	s.invalidate()
	return &SwitchResult{Enabled: res.Enabled, Mode: mode, Changed: true}, nil
}

// SetSwitch 打开 / 关闭 SQL 防火墙总开关。关闭是高危操作：必须写明原因，审计标记高危。
func (s *Service) SetSwitch(actor audit.Actor, enabled bool, reason string) (*SwitchResult, error) {
	reason = strings.TrimSpace(reason)
	cur := s.switchOn()
	res := &SwitchResult{Enabled: cur, Mode: s.globalMode()}
	if cur == enabled {
		return res, nil
	}
	var text string
	if enabled {
		text = "开启 SQL 防火墙总开关（firewall.enabled=true）" // i18n-ignore 审计文本，存库后不翻译
	} else {
		if n := len([]rune(reason)); n < 4 || n > 200 {
			return nil, badReq("关闭总开关需要填写原因（4–200 个字符）")
		}
		text = "关闭 SQL 防火墙总开关（firewall.enabled=false）：规则不再对任何场景生效，也不再产生命中记录。原因：" + reason // i18n-ignore 审计文本，存库后不翻译
	}
	if err := s.st.SetConfig(cfgEnabled, strconv.FormatBool(enabled), "SQL 防火墙总开关", // i18n-ignore 审计文本，存库后不翻译
		s.auditEntry(actor, actSwitch, text, !enabled)); err != nil {
		return nil, err
	}
	s.invalidate()
	return &SwitchResult{Enabled: enabled, Mode: res.Mode, Changed: true}, nil
}
