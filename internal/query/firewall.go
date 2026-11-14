// @author: kevinkelly
// @license: Apache Licence
// @file: firewall.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/firewall"
	"dbtool/internal/masking"
	"dbtool/internal/model"
	"dbtool/internal/sqlx"
)

// SQL 防火墙在在线查询里的接线：
//
//   - 位置：语句检查（checkFor / check*）之后、库表权限校验之前。被拒绝的语句不会连到目标库，
//     也不会向用户暴露“你有没有这张表的权限”；
//   - 动作：deny 拒绝（403 firewall_denied）、reason 要求原因（428 reason_required，前端弹窗后带 reason 重发）、
//     limit 与权限算出的行数上限取小、mask 并入脱敏配置（按整体隐藏）、warn 放行并在结果里提示；
//   - 命中记录在这里写入，语句成功执行并写入 dbt_query_log 之后再把日志 ID 回填；
//   - 范围：MySQL 系与 SQL Server（prepare）、PostgreSQL 与 ClickHouse（prepareMulti，不支持强制脱敏）。
//     MongoDB 与 Redis 不进防火墙（各有自己的检查）。

// SetFirewall 接入 SQL 防火墙。传 nil（或不调用）= 不启用，行为与没有这个模块时完全一致。
func (s *Service) SetFirewall(e *firewall.Engine) { s.fw = e }

// Firewall 返回已接入的防火墙引擎（未启用时为 nil）。管理页保存规则后用它让规则快照立即失效。
func (s *Service) Firewall() *firewall.Engine { return s.fw }

// EnvOfTags 是实例标签推断环境的规则（prod / test / 空），与防火墙求值时用的一致；管理页的实例范围与测试台复用。
func EnvOfTags(tags []model.InstanceTag) string { return envFromTags(tags) }

// EnvOfInstance 是实例的有效环境（实例管理里设置的优先，没设置时由标签推断）。
func EnvOfInstance(in model.Instance) string { return firewall.EnvOfInstance(in.ID, in.Tags) }

// FirewallInfo 是防火墙对本次查询产生的、需要让用户知道的结果（放行但有提示 / 被收紧）。
type FirewallInfo struct {
	Warnings  []string `json:"warnings,omitempty"`   // 仅告警类规则的提示
	LimitedBy string   `json:"limited_by,omitempty"` // 收紧了返回行数的规则名
	MaskedBy  []string `json:"masked_by,omitempty"`  // 强制脱敏的规则名
}

// firewallGate 对一条已通过语句检查的查询求值。返回 (nil, nil) 表示防火墙未启用。
// sql 是用户提交的原文（含可能被忽略的后续语句），stmt 是实际要执行的那一条（库表解析用）。
func (s *Service) firewallGate(ctx context.Context, p Principal, inst *model.Instance, req Request, stmt string, caps firewall.Caps, tr *execTrace, tables func() ([]sqlx.TableRef, error), scan func() (int64, error)) (*firewall.Decision, error) {
	if s.fw == nil {
		return nil, nil
	}
	freq := firewall.Request{
		Scene: firewall.SceneQuery, UserID: p.ID, User: p.Username,
		InstanceID: inst.ID, InstanceName: inst.InstanceName, DBType: inst.DBType, Env: firewall.EnvOfInstance(inst.ID, inst.Tags), DB: req.DB,
		SQL: req.SQL, Reason: req.Reason, Caps: caps, AffectRows: -1, ScanRows: scan,
	}
	if tables != nil {
		freq.Tables = func() ([]firewall.Object, error) {
			refs, err := tables()
			if err != nil {
				return nil, err
			}
			out := make([]firewall.Object, 0, len(refs))
			for _, r := range refs {
				name := r.Name
				if i := strings.LastIndex(name, "."); i >= 0 { // PostgreSQL 的表名是 schema.table
					name = name[i+1:]
				}
				out = append(out, firewall.Object{Schema: r.Schema, Table: name})
			}
			return out, nil
		}
	}
	d := s.fw.Evaluate(ctx, freq)
	dry := tr != nil && tr.dry // 估算行数等“只看不执行”的调用：不写命中记录，也不要求填写原因
	switch {
	case d.Deny:
		if !dry {
			s.fw.Record(ctx, freq, d, req.Reason)
		}
		if d.Failed {
			return d, newErr(503, "firewall_unavailable", d.Message)
		}
		e := newErr(403, "firewall_denied", d.Message)
		if d.Rule != nil {
			e.Data = map[string]any{"rule": d.Rule.Name, "rule_id": d.Rule.ID}
		}
		return d, e
	case d.NeedReason && strings.TrimSpace(req.Reason) == "" && !dry:
		// 不记命中：用户填了原因重发之后才记一次，避免同一条语句记两遍
		e := newErr(428, "reason_required", strings.Join(d.ReasonMsg, "\n"))
		e.Data = map[string]any{"messages": d.ReasonMsg}
		return d, e
	}
	if !dry {
		s.fw.Record(ctx, freq, d, req.Reason)
	}
	return d, nil
}

// fwScanFunc 返回 scan_rows 条件取事实用的函数（MySQL 系实例上的 SELECT；其他情况返回 nil = 未评估）。
//
// 它先确认用户有权查询这条语句（和后面的 AuthorizeQuery 同一个判断）：防火墙在库表权限校验之前求值，如果直接 EXPLAIN，
// 没有权限的用户就能从“要求填写原因 / 被拒绝”的差别里探测到某张表存在、有多大。没有权限的语句这个条件按未评估处理，
// 随后的 AuthorizeQuery 照常给出无权限的错误。
func (s *Service) fwScanFunc(ctx context.Context, p Principal, inst *model.Instance, db, stmt string, pol Policy, prelude []string) func() (int64, error) {
	if s.fw == nil || !firewall.SupportsExplain(inst.DBType) || sqlx.QueryKind(stmt) != "select" {
		return nil
	}
	return func() (int64, error) {
		if _, err := s.auth.AuthorizeQuery(ctx, p, inst, db, stmt, pol.AdminLimit); err != nil {
			return 0, errors.New("语句没有通过库表权限校验，不评估预估扫描行数")
		}
		return s.fw.EstimateScan(ctx, inst.ID, db, stmt, func(c context.Context, q string, timeout time.Duration) ([]string, [][]any, error) {
			e, err := s.open(inst.DBType, s.repo.EngineConn(inst))
			if err != nil {
				return nil, nil, fmt.Errorf("%w: %v", firewall.ErrExplainUnavailable, err)
			}
			defer e.Close()
			rs, err := e.Query(c, q, engine.QueryOptions{DBName: db, MaxExecMs: timeout.Milliseconds(), Prelude: prelude})
			if err != nil {
				return nil, nil, firewall.WrapPlanError(err)
			}
			if rs == nil {
				return nil, nil, errors.New("EXPLAIN 没有返回结果")
			}
			return rs.ColumnList, rs.Rows, nil
		})
	}
}

// applyFirewallLimit 把防火墙的行数限制并入权限算出的上限（取小），返回新的上限、限制来源说明与是否由防火墙收紧。
func applyFirewallLimit(fd *firewall.Decision, limit int, dec Decision) (int, Decision, bool) {
	if fd == nil || fd.MaxRows <= 0 || (limit > 0 && fd.MaxRows >= limit) {
		return limit, dec, false
	}
	dec.Source = fmt.Sprintf("SQL 防火墙规则「%s」", fd.LimitedBy)
	return fd.MaxRows, dec, true
}

// fwInfo 把防火墙决定整理成给用户看的结果；没有需要提示的内容时返回 nil。limited 为真表示行数上限确实是防火墙收紧的。
func fwInfo(d *firewall.Decision, limited bool) *FirewallInfo {
	if d == nil {
		return nil
	}
	info := &FirewallInfo{Warnings: d.Warnings}
	if limited {
		info.LimitedBy = d.LimitedBy
	}
	for _, h := range d.Hits {
		if h.Effective == firewall.ActMask {
			info.MaskedBy = append(info.MaskedBy, h.Rule.Name)
		}
	}
	if len(info.Warnings) == 0 && info.LimitedBy == "" && len(info.MaskedBy) == 0 {
		return nil
	}
	return info
}

// fwNotice 是 fwInfo 的文字形态，并入 Result.Notice（现有页面已经会显示它）。
func fwNotice(info *FirewallInfo) string {
	if info == nil {
		return ""
	}
	var parts []string
	for _, w := range info.Warnings {
		parts = append(parts, fmt.Sprintf("SQL 防火墙提示：%s", w))
	}
	if info.LimitedBy != "" {
		parts = append(parts, fmt.Sprintf("SQL 防火墙规则「%s」收紧了返回行数。", info.LimitedBy))
	}
	if len(info.MaskedBy) > 0 {
		parts = append(parts, fmt.Sprintf("SQL 防火墙规则「%s」对部分列做了强制脱敏。", strings.Join(info.MaskedBy, "、")))
	}
	return strings.Join(parts, "\n")
}

// ---- 强制脱敏：通过 ctx 把防火墙要求脱敏的列带进 maskResult ----

type forceMaskKey struct{}

// forcedColumns 把防火墙的强制脱敏对象转成脱敏配置里的列（整体隐藏）。
func forcedColumns(d *firewall.Decision) []masking.Column {
	if d == nil {
		return nil
	}
	return forcedColumnsOf(d.ForceMask)
}

func forcedColumnsOf(refs []firewall.ObjectRef) []masking.Column {
	if len(refs) == 0 {
		return nil
	}
	out := make([]masking.Column, 0, len(refs))
	for _, r := range refs {
		out = append(out, masking.Column{Schema: r.Schema, Table: r.Table, Column: r.Column, Rule: &masking.Rule{Kind: masking.KindWhole}})
	}
	return out
}

// onlyForcedKey 标记“只做防火墙要求的强制脱敏”：系统脱敏总开关（或导出脱敏开关）关闭时，已配置的脱敏规则不生效，
// 但防火墙规则明确要求隐藏的列仍要隐藏。
type onlyForcedKey struct{}

func withOnlyForced(ctx context.Context) context.Context {
	return context.WithValue(ctx, onlyForcedKey{}, true)
}

// MaskExportForced 在 MaskExport 的基础上叠加 SQL 防火墙要求强制脱敏的列（整体隐藏，供导出工单使用）。
// configured 为 false 时只脱敏强制的列。返回被脱敏的结果列名；脱敏失败返回错误，调用方不应继续导出。
func (s *Service) MaskExportForced(ctx context.Context, inst *model.Instance, db, sql string, e engine.Engine, rs *engine.ResultSet, force []firewall.ObjectRef, configured bool) ([]string, error) {
	ctx = withForcedMask(ctx, forcedColumnsOf(force))
	if !configured {
		ctx = withOnlyForced(ctx)
	}
	mo, err := s.maskResult(ctx, inst, db, sql, rs, e)
	if err != nil {
		return nil, err
	}
	return mo.Columns, nil
}

func withForcedMask(ctx context.Context, cols []masking.Column) context.Context {
	if len(cols) == 0 {
		return ctx
	}
	return context.WithValue(ctx, forceMaskKey{}, cols)
}

// withForced 返回把强制脱敏列放在最前面的配置（先匹配到的生效，所以强制的优先于已配置的规则）。
func (c *maskConfig) withForced(ctx context.Context) *maskConfig {
	forced, _ := ctx.Value(forceMaskKey{}).([]masking.Column)
	if len(forced) == 0 {
		return c
	}
	out := &maskConfig{cols: append([]masking.Column(nil), forced...)}
	if c != nil {
		out.cols = append(out.cols, c.cols...)
		out.rules = c.rules
	}
	if out.rules == nil {
		out.rules = masking.DefaultRules()
	}
	return out
}

// ---- 库表解析的记忆装饰器 ----

// memoExtractor 给 TableExtractor 加一层很短的缓存：键 (实例, 库, 语句)，只缓存成功结果。
// 防火墙与库表权限校验解析的是同一条语句，共用它就不会让一条语句调用两次 goInception。
type memoExtractor struct {
	inner TableExtractor
	ttl   time.Duration
	max   int
	now   func() time.Time

	mu sync.Mutex
	m  map[string]memoEntry
}

type memoEntry struct {
	refs []sqlx.TableRef
	at   time.Time
}

func newMemoExtractor(inner TableExtractor) *memoExtractor {
	return &memoExtractor{inner: inner, ttl: 10 * time.Second, max: 256, now: time.Now, m: map[string]memoEntry{}}
}

func (m *memoExtractor) Tables(ctx context.Context, inst *model.Instance, db, stmt string) ([]sqlx.TableRef, error) {
	key := fmt.Sprintf("%d\x00%s\x00%s", inst.ID, db, stmt)
	now := m.now()
	m.mu.Lock()
	if e, ok := m.m[key]; ok && now.Sub(e.at) < m.ttl {
		m.mu.Unlock()
		return append([]sqlx.TableRef(nil), e.refs...), nil
	}
	m.mu.Unlock()
	refs, err := m.inner.Tables(ctx, inst, db, stmt)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if len(m.m) >= m.max {
		m.m = map[string]memoEntry{} // 简单起见：满了就整体丢弃，条目本来就只活 10 秒
	}
	m.m[key] = memoEntry{refs: append([]sqlx.TableRef(nil), refs...), at: now}
	m.mu.Unlock()
	return refs, nil
}

// tableExtractor 返回在线查询用的库表解析器（与授权器共用同一个带缓存的实例）。
func (s *Service) tableExtractor() TableExtractor {
	if pa, ok := s.auth.(*PrivAuthorizer); ok {
		return pa.ext
	}
	return LexicalExtractor{}
}

// fwTables 给防火墙的 object 条件提供库表解析，选择逻辑与授权器一致：MySQL 协议系按 tableExtractorFor 选（能用 goInception 的走它），
// SQL Server 用 T-SQL 词法解析，其余用词法解析。
func (s *Service) fwTables(ctx context.Context, inst *model.Instance, db, stmt string) func() ([]sqlx.TableRef, error) {
	return func() ([]sqlx.TableRef, error) {
		switch {
		case isMSSQL(inst.DBType):
			refs, _ := mssqlTables(stmt, db)
			return refs, nil
		case usesSyntaxTree(inst.DBType):
			return s.tableExtractor().Tables(ctx, inst, db, stmt)
		}
		return LexicalExtractor{}.Tables(ctx, inst, db, stmt)
	}
}
