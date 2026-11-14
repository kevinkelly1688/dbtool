// @author: kevinkelly
// @license: Apache Licence
// @file: engine.go
// @time: 2026/10/28

package firewall

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/settings"
)

// ConfigSource 读 dbt_setting 里的项（query.Repo 已经满足）。
type ConfigSource interface {
	ConfigValue(item string) (string, bool)
}

// HitRow 是写入命中表的一行（存储层的形态，不依赖 GORM）。
type HitRow struct {
	RuleID       uint
	RuleName     string
	Scene        string
	UserName     string
	InstanceID   uint
	InstanceName string
	DB           string
	Effective    string
	WouldAction  string
	Fingerprint  string
	SQLText      string
	ReasonText   string
	Matched      string // JSON
}

// Store 是规则、例外与命中记录的存储。表不存在时 LoadRules / LoadExceptions 返回空（不是错误），等同没有规则。
type Store interface {
	LoadRules() ([]Rule, error)
	LoadExceptions(now time.Time) ([]Exception, error)
	UserGroupIDs(userID uint) ([]uint, error)
	// RecordHit 写入一条命中；窗口内同一 (规则, 用户, 指纹, 结果) 已有一行时累加 repeat_cnt 并返回该行 ID。
	RecordHit(h HitRow, window time.Duration) (uint, error)
	AttachQueryLog(ids []uint, queryLogID uint) error
}

// NameGroupStore 是 Store 可选实现的能力：只知道用户名时（工单执行前复核时只有提交人的用户名）按用户名取权限组。
// 没有实现时，没带 GroupIDs / UserID 的请求视为不属于任何权限组。
type NameGroupStore interface {
	UserGroupIDsByName(username string) ([]uint, error)
}

// Settings 是 dbt_setting 里的防火墙配置（随快照缓存）。
type Settings struct {
	Enabled        bool          // firewall.enabled，缺省开启
	Observe        bool          // firewall.mode = observe：所有规则只记录
	OnError        string        // firewall.on_error：allow（缺省）/ deny
	StoreSQL       string        // firewall.store_sql：masked（缺省，只存脱字面量形态）/ raw
	ExplainTimeout time.Duration // firewall.explain_timeout_ms：EXPLAIN 的时限（缺省 3 秒，200 毫秒 – 30 秒）
}

// snapshot 是已编译的规则快照。
type snapshot struct {
	at       time.Time
	rules    []*compiledRule
	exs      []Exception
	set      Settings
	invalid  int  // 读到但校验 / 编译失败被跳过的规则数
	needGrps bool // 是否有规则 / 例外依赖权限组
}

// Engine 是防火墙求值与记录入口。并发安全。
type Engine struct {
	st  Store
	cfg ConfigSource
	ttl time.Duration
	now func() time.Time

	mu   sync.Mutex
	snap *snapshot

	ex *explainState // EXPLAIN 事实的缓存 / 熔断 / 统计（见 explain.go）
	rs *ruleStats    // 规则级求值统计
}

// SnapshotTTL 是规则快照的缓存时长：多实例部署时，规则变更最长这么久后在所有实例上生效；本进程内保存规则后立即失效。
const SnapshotTTL = 3 * time.Second

// New 创建引擎。
func New(st Store, cfg ConfigSource) *Engine {
	return &Engine{st: st, cfg: cfg, ttl: SnapshotTTL, now: time.Now, ex: newExplainState(), rs: &ruleStats{m: map[uint]*RuleStat{}}}
}

// Invalidate 让规则快照立即失效（保存规则 / 例外后调用）。
func (e *Engine) Invalidate() {
	e.mu.Lock()
	e.snap = nil
	e.mu.Unlock()
}

func (e *Engine) settings() Settings {
	s := Settings{Enabled: true, OnError: "allow", StoreSQL: "masked", ExplainTimeout: DefaultExplainTimeout}
	if e.cfg == nil {
		return s
	}
	get := func(k string) string {
		v, _ := e.cfg.ConfigValue(k)
		return strings.ToLower(strings.TrimSpace(v))
	}
	if get(settings.KeyFirewallEnabled) == "false" {
		s.Enabled = false
	}
	if get(settings.KeyFirewallMode) == "observe" {
		s.Observe = true
	}
	if get(settings.KeyFirewallOnError) == "deny" {
		s.OnError = "deny"
	}
	if get(settings.KeyFirewallStoreSQL) == "raw" {
		s.StoreSQL = "raw"
	}
	if ms, err := strconv.Atoi(get(settings.KeyFirewallExplainTimeoutMs)); err == nil && ms > 0 {
		s.ExplainTimeout = time.Duration(ms) * time.Millisecond
	}
	return s
}

// cachedSettings 返回随规则快照缓存的配置（最多 SnapshotTTL 之前的值）；还没有快照时现读。
// dbt_setting 的读取是一次数据库查询，Evaluate / Record / EstimateScan 每次调用都读 4–5 项会让每条语句多出几次往返。
func (e *Engine) cachedSettings() Settings {
	e.mu.Lock()
	snap := e.snap
	e.mu.Unlock()
	if snap != nil {
		return snap.set
	}
	return e.settings()
}

// load 读取并编译规则。读取失败时返回错误（调用方决定沿用旧快照还是按 on_error 处理）。
func (e *Engine) load() (*snapshot, error) {
	now := e.now()
	rules, err := e.st.LoadRules()
	if err != nil {
		return nil, fmt.Errorf("读取防火墙规则失败: %w", err) // i18n-ignore 只进日志，不面向用户
	}
	exs, err := e.st.LoadExceptions(now)
	if err != nil {
		return nil, fmt.Errorf("读取防火墙例外失败: %w", err) // i18n-ignore 只进日志，不面向用户
	}
	s := &snapshot{at: now, exs: exs, set: e.settings()}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		c, err := compile(r)
		if err != nil {
			s.invalid++
			slog.Warn("防火墙规则无效，已跳过", "rule_id", r.ID, "name", r.Name, "err", err)
			continue
		}
		c.Enabled = true
		s.rules = append(s.rules, c)
		if c.GroupMode != "all" {
			s.needGrps = true
		}
	}
	for _, x := range exs {
		if x.SubjectKind == SubjectGroup {
			s.needGrps = true
		}
	}
	sort.SliceStable(s.rules, func(i, j int) bool {
		if s.rules[i].Priority != s.rules[j].Priority {
			return s.rules[i].Priority < s.rules[j].Priority
		}
		return s.rules[i].ID < s.rules[j].ID
	})
	return s, nil
}

// snapshot 返回当前快照：过期就重读；读取失败时沿用旧快照（并记日志），从未读取成功才返回错误。
func (e *Engine) snapshot() (*snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.snap != nil && e.now().Sub(e.snap.at) < e.ttl {
		return e.snap, nil
	}
	s, err := e.load()
	if err != nil {
		if e.snap != nil {
			slog.Error("防火墙规则刷新失败，沿用上一份快照", "err", err)
			e.snap.at = e.now() // 不要每次求值都重试
			return e.snap, nil
		}
		return nil, err
	}
	e.snap = s
	return s, nil
}

// Evaluate 对一条语句求值，返回决定。永不返回 nil；求值本身出错（读规则失败 / panic）时按 firewall.on_error 处理，
// Decision.Failed 为真。
func (e *Engine) Evaluate(ctx context.Context, req Request) (d *Decision) {
	onErr := e.cachedSettings().OnError
	defer func() {
		if r := recover(); r != nil {
			slog.Error("防火墙求值 panic，已按 firewall.on_error 处理", "panic", r, "user", req.User)
			d = failed(onErr, fmt.Errorf("内部错误: %v", r)) // i18n-ignore 只进日志，不面向用户
		}
	}()
	snap, err := e.snapshot()
	if err != nil {
		slog.Error("防火墙规则不可用", "err", err)
		return failed(onErr, err)
	}
	if !snap.set.Enabled || len(snap.rules) == 0 {
		return &Decision{}
	}
	now := req.Now
	if now.IsZero() {
		now = e.now()
	}
	groups := map[uint]bool{}
	ids := req.GroupIDs
	if ids == nil && snap.needGrps {
		switch ns, byName := e.st.(NameGroupStore); {
		case req.UserID != 0:
			ids, err = e.st.UserGroupIDs(req.UserID)
		case req.User != "" && byName:
			ids, err = ns.UserGroupIDsByName(req.User)
		}
		if err != nil {
			slog.Error("防火墙读取用户权限组失败", "user", req.User, "err", err)
			return failed(snap.set.OnError, err)
		}
	}
	for _, id := range ids {
		groups[id] = true
	}
	return evaluate(snap.rules, snap.exs, &req, groups, snap.set.Observe, now, e.rs)
}

func failed(onError string, err error) *Decision {
	d := &Decision{Failed: true}
	if onError == "deny" {
		d.Deny = true
		d.Message = "SQL 防火墙暂时不可用，已按安全策略拒绝本次语句，请稍后重试或联系管理员。"
	}
	_ = err
	return d
}

// Record 把决定里的命中写入命中表，并把行 ID 回填到 d.Hits[i].ID。写库失败只记日志，不改变决定。
// reasonText 是用户为 reason 动作填写的原因。没有命中时什么也不做。
func (e *Engine) Record(ctx context.Context, req Request, d *Decision, reasonText string) {
	if d == nil || len(d.Hits) == 0 {
		return
	}
	set := e.cachedSettings()
	text := Redact(req.SQL)
	if set.StoreSQL == "raw" {
		text = strings.TrimSpace(req.SQL)
	}
	text = truncateRunes(text, MaxHitSQLRunes)
	reasonText = truncateRunes(strings.TrimSpace(reasonText), 500)
	for i := range d.Hits {
		h := &d.Hits[i]
		matched, _ := json.Marshal(map[string]any{"matched": h.Matched, "unknown": h.Unknown, "downgraded": h.Downgraded})
		row := HitRow{
			RuleID: h.Rule.ID, RuleName: h.Rule.Name, Scene: req.Scene, UserName: req.User,
			InstanceID: req.InstanceID, InstanceName: req.InstanceName, DB: req.DB,
			Effective: h.Effective, WouldAction: h.WouldAction, Fingerprint: d.Facts.Fingerprint,
			SQLText: text, Matched: string(matched),
		}
		if h.Effective == ActReason {
			row.ReasonText = reasonText
		}
		id, err := e.st.RecordHit(row, HitMergeWindow)
		if err != nil {
			slog.Warn("写入防火墙命中记录失败", "rule", h.Rule.Name, "user", req.User, "err", err)
			continue
		}
		h.ID = id
	}
}

// AttachQueryLog 把成功执行的语句对应的 dbt_query_log.id 回填到命中记录里。
func (e *Engine) AttachQueryLog(d *Decision, queryLogID uint) {
	if d == nil || queryLogID == 0 {
		return
	}
	var ids []uint
	for _, h := range d.Hits {
		if h.ID != 0 {
			ids = append(ids, h.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	if err := e.st.AttachQueryLog(ids, queryLogID); err != nil {
		slog.Warn("回填防火墙命中的查询日志 ID 失败", "err", err)
	}
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
