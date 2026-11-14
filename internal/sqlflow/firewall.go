// @author: kevinkelly
// @license: Apache Licence
// @file: firewall.go
// @time: 2026/10/28

package sqlflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine"
	"dbtool/internal/firewall"
	"dbtool/internal/model"
	"dbtool/internal/review"
)

// SQL 防火墙在 SQL 工单与数据导出里的接线：
//
//   - 提交检查（预览与提交都会走）：对检查结果的每一行（一条语句）按“工单”场景求值。拒绝 → 该行标错误（走现有的
//     “有错误是否自动驳回”路径），仅告警 → 该行标警告；review.critical_ddl_regex 先求值，已经是错误的行保留它的文案。
//     观察态的命中只记录；
//   - 执行前复核：审批可能隔了几小时到几天，规则可能变了。真正执行之前对语句再求值一次，**只认拒绝**（观察态不拦截），
//     被拒绝的工单进入“执行有异常”并写明规则，不会静默跳过；
//   - 数据导出：提交检查时拒绝 → 检查不通过；限行与导出的行数上限取小；强制脱敏的列在执行时叠加到导出脱敏里。执行时每个实例各自复核；
//   - 工单场景没有“要求原因 / 限行 / 脱敏”的交互能力，这三种动作按“仅告警”处理（Caps 为空，命中说明里写明）；
//   - 范围：MySQL 系与 SQL Server。Redis 工单走自己的 redisrule，不进防火墙；线下手工执行的工单系统不执行，不检查。

// SetFirewall 接入 SQL 防火墙。传 nil（或不调用）= 不启用，行为与没有这个模块时完全一致。
func (s *Service) SetFirewall(e *firewall.Engine) { s.fw = e }

// ForcedResultMasker 是 ResultMasker 的可选扩展：叠加 SQL 防火墙要求强制脱敏的列（整体隐藏）。
// configured 为 false 时只脱敏强制的列（系统脱敏总开关或导出脱敏开关关闭时，已配置的规则不生效）。
type ForcedResultMasker interface {
	MaskExportForced(ctx context.Context, inst *model.Instance, db, sql string, e engine.Engine, rs *engine.ResultSet, force []firewall.ObjectRef, configured bool) ([]string, error)
}

// 检查结果里防火墙行的阶段状态。
const (
	fwDenyStatus = "驳回高危SQL（防火墙）"
	fwWarnStatus = "SQL 防火墙提示"
)

// fwSession 是一次检查 / 复核里的命中记录去重：脚本里成百上千条同一形态的语句（批量 INSERT 之类）不必各写一行命中，
// 同一 (用户, 实例, 场景, 指纹, 命中的规则与结果) 只记一次；一次最多记 fwMaxRecords 行，免得一个大脚本把命中表写爆。
type fwSession struct {
	seen map[string]bool
	n    int

	// EXPLAIN：同一次检查里复用到各实例的连接，并限制次数与总时间（脚本里几百条语句不能把检查拖成几分钟）
	engines map[uint]engine.Engine
	plans   int
	spent   time.Duration
}

const (
	fwMaxRecords    = 200
	fwMaxExplains   = 50               // 一次检查 / 复核最多真正下发的 EXPLAIN 次数
	fwExplainBudget = 20 * time.Second // 一次检查 / 复核里 EXPLAIN 的总耗时上限
)

// close 关闭会话里为 EXPLAIN 打开的连接。ss 为 nil 时什么也不做。
func (ss *fwSession) close() {
	if ss == nil {
		return
	}
	for _, e := range ss.engines {
		e.Close()
	}
	ss.engines = nil
}

// fwScan 取一条语句在实例上的预估扫描行数（EXPLAIN）：限时、缓存、熔断由 firewall.Engine.EstimateScan 负责，
// 这里提供到目标实例的连接，并给一次检查设次数与总时间上限。
func (s *Service) fwScan(ctx context.Context, ss *fwSession, in *model.Instance, db, stmt string) (int64, error) {
	return s.fw.EstimateScan(ctx, in.ID, db, stmt, func(c context.Context, q string, timeout time.Duration) ([]string, [][]any, error) {
		if ss != nil {
			if ss.plans >= fwMaxExplains {
				return nil, nil, fmt.Errorf("本次检查已下发 %d 次 EXPLAIN，达到上限，其余语句不再估算扫描行数", fwMaxExplains)
			}
			if ss.spent >= fwExplainBudget {
				return nil, nil, fmt.Errorf("本次检查里 EXPLAIN 已用时 %d 秒，达到上限，其余语句不再估算扫描行数", int(fwExplainBudget.Seconds()))
			}
			ss.plans++
		}
		var e engine.Engine
		if ss != nil && ss.engines[in.ID] != nil {
			e = ss.engines[in.ID]
		} else {
			var err error
			if e, err = s.open(in.DBType, s.repo.EngineConn(in)); err != nil {
				return nil, nil, fmt.Errorf("%w: %v", firewall.ErrExplainUnavailable, err)
			}
			if ss != nil {
				if ss.engines == nil {
					ss.engines = map[uint]engine.Engine{}
				}
				ss.engines[in.ID] = e
			} else {
				defer e.Close()
			}
		}
		start := time.Now()
		rs, err := e.Query(c, q, engine.QueryOptions{DBName: db, MaxExecMs: timeout.Milliseconds()})
		if ss != nil {
			ss.spent += time.Since(start)
		}
		if err != nil {
			return nil, nil, firewall.WrapPlanError(err)
		}
		if rs == nil {
			return nil, nil, errors.New("EXPLAIN 没有返回结果")
		}
		return rs.ColumnList, rs.Rows, nil
	})
}

func (ss *fwSession) shouldRecord(req *firewall.Request, d *firewall.Decision) bool {
	if len(d.Hits) == 0 {
		return false
	}
	if ss == nil {
		return true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%d|%s", req.User, req.Scene, req.InstanceID, d.Facts.Fingerprint)
	for _, h := range d.Hits {
		fmt.Fprintf(&b, "|%d:%s", h.Rule.ID, h.Effective)
	}
	key := b.String()
	if ss.seen == nil {
		ss.seen = map[string]bool{}
	}
	if ss.seen[key] || ss.n >= fwMaxRecords {
		return false
	}
	ss.seen[key] = true
	ss.n++
	return true
}

// fwEval 对一条语句在一个实例上求值并记录命中。affect < 0 表示影响行数未知。ss 为 nil 时每次命中都记录。
func (s *Service) fwEval(ctx context.Context, ss *fwSession, scene, user string, userID uint, in *model.Instance, db, stmt string, affect int64) *firewall.Decision {
	caps := firewall.Caps{}
	if scene == firewall.SceneExport {
		caps = firewall.Caps{Limit: true, Mask: true}
	}
	req := firewall.Request{
		Scene: scene, UserID: userID, User: user,
		InstanceID: in.ID, InstanceName: in.InstanceName, DBType: in.DBType, Env: firewall.EnvOfInstance(in.ID, in.Tags), DB: db,
		SQL: stmt, Caps: caps, AffectRows: affect,
		// 工单检查结果里没有表信息：用词法解析（宁多勿漏）；在线查询那边用 goInception 语法树
		Tables: func() ([]firewall.Object, error) { return firewall.LexicalObjects(stmt), nil },
	}
	if firewall.SupportsExplain(in.DBType) { // 预估扫描行数（scan_rows）与 UPDATE / DELETE 的影响行数上界：只在需要时才真正 EXPLAIN
		req.ScanRows = func() (int64, error) { return s.fwScan(ctx, ss, in, db, stmt) }
	}
	d := s.fw.Evaluate(ctx, req)
	if ss.shouldRecord(&req, d) {
		s.fw.Record(ctx, req, d, "")
	}
	return d
}

// fwDenyText 是给用户看的拒绝说明（带规则名；规则的文案已经提到规则名时不重复）。
func fwDenyText(d *firewall.Decision) string {
	if d.Rule == nil || strings.Contains(d.Message, d.Rule.Name) {
		return d.Message
	}
	return fmt.Sprintf("SQL 防火墙规则「%s」：%s", d.Rule.Name, d.Message)
}

// fwNotes 是“仅告警”命中的提示（含降级说明：限行 / 脱敏 / 要原因在工单场景里降级为告警）。
func fwNotes(d *firewall.Decision) []string {
	var out []string
	for _, h := range d.Hits {
		if h.Effective != firewall.ActWarn {
			continue
		}
		msg := h.Rule.Message
		if msg == "" {
			msg = "该语句命中规则。"
		}
		t := fmt.Sprintf("SQL 防火墙规则「%s」：%s", h.Rule.Name, msg)
		if h.Downgraded != "" {
			t += "（" + h.Downgraded + "）"
		}
		out = append(out, t)
	}
	return out
}

func refsText(refs []firewall.ObjectRef) string {
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = r.Schema + "." + r.Table + "." + r.Column
	}
	return strings.Join(parts, "、")
}

func joinNotes(existing string, notes []string) string {
	add := strings.Join(notes, "\n")
	if e := strings.TrimSpace(existing); e == "" || e == "None" {
		return add
	}
	return existing + "\n" + add
}

// fwSkipSource 这些检查来源不进防火墙：Redis 有自己的上线规则，线下手工执行系统不执行。
func fwSkipSource(src string) bool { return src == SourceRedis || src == SourceManual }

// applyFirewall 在检查结果上叠加防火墙（SQL 工单场景）。每条语句对每个目标实例各求一次（批量上线时规则范围可能因实例而异），
// 任一实例拒绝就整行拒绝。内容在 res 上原地修改。
func (s *Service) applyFirewall(ctx context.Context, p audit.Actor, insts []*model.Instance, db string, res *CheckResult) {
	if s.fw == nil || res == nil || len(res.Rows) == 0 || fwSkipSource(res.Source) || res.Source == SourceExport {
		return
	}
	ss := &fwSession{}
	defer ss.close()
	for i := range res.Rows {
		r := &res.Rows[i]
		stmt := strings.TrimSpace(r.SQL)
		if stmt == "" {
			continue
		}
		affect := int64(-1)
		if res.Source == SourceGoInception && r.AffectedRows > 0 {
			affect = int64(r.AffectedRows)
		}
		var deny string
		var notes []string
		for _, in := range insts {
			d := s.fwEval(ctx, ss, firewall.SceneWorkflow, p.Username, p.ID, in, db, stmt, affect)
			who := ""
			if len(insts) > 1 {
				who = fmt.Sprintf("实例[%s]", in.InstanceName)
			}
			switch {
			case d.Deny && deny == "":
				deny = who + fwDenyText(d)
			case !d.Deny:
				for _, n := range fwNotes(d) {
					notes = append(notes, who+n)
				}
			}
		}
		switch {
		case deny != "":
			if r.ErrLevel != review.LevelError { // review.critical_ddl_regex 等已有的错误优先：保留它的文案
				markError(r, fwDenyStatus, deny)
			}
		case len(notes) > 0 && r.ErrLevel == review.LevelOK:
			r.ErrLevel, r.StageStatus, r.ErrorMessage = review.LevelWarning, fwWarnStatus, joinNotes(r.ErrorMessage, dedup(notes))
		}
	}
	res.Count()
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range in {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// firewallWarnings 统计只由防火墙“仅告警”产生的警告行数（它们是提示，不能让“警告就驳回”的站点配置把工单驳回）。
func firewallWarnings(rows []review.Row) int {
	n := 0
	for _, r := range rows {
		if r.ErrLevel == review.LevelWarning && r.StageStatus == fwWarnStatus {
			n++
		}
	}
	return n
}

// storedAffected 取工单提交时检查结果里每条语句的影响行数（只有 goInception 的结果才有可靠的值，0 视为未知）。
func (s *Service) storedAffected(workflowID uint) map[string]int64 {
	c, err := s.repo.GetContent(workflowID)
	if err != nil || c == nil {
		return nil
	}
	rows, err := review.ParseRows(c.ReviewContent)
	if err != nil {
		return nil
	}
	m := map[string]int64{}
	for _, r := range rows {
		if r.AffectedRows > 0 {
			m[stmtKey(r.SQL)] = int64(r.AffectedRows)
		}
	}
	return m
}

func stmtKey(s string) string { return strings.TrimRight(strings.TrimSpace(s), "; \t\r\n") }

// fwRecheck 是工单执行前的复核：对工单的每条语句按“工单”场景在目标实例上再求值一次，只认拒绝。
// 返回非空表示被拒绝（说明里写明规则与语句）；观察态与告警不拦截。w.Engineer（提交人）是规则里“用户”所指的对象，
// 而不是点“执行”的人——审批通过后换人执行不能绕过规则。
func (s *Service) fwRecheck(ctx context.Context, w *model.SqlWorkflow, in *model.Instance, sqlText string) string {
	if s.fw == nil || in == nil || w.IsManual != 0 || in.DBType == dbTypeRedis {
		return ""
	}
	affect := s.storedAffected(w.ID)
	ss := &fwSession{}
	defer ss.close()
	deny := ""
	for _, st := range splitFor(in.DBType, sqlText) {
		a := int64(-1)
		if v, ok := affect[stmtKey(st)]; ok {
			a = v
		}
		d := s.fwEval(ctx, ss, firewall.SceneWorkflow, w.Engineer, 0, in, w.DBName, st, a)
		if d.Deny && deny == "" {
			deny = fmt.Sprintf("%s 被 %s；工单没有执行任何语句。提交之后规则可能发生了变化；如需继续，请联系管理员调整规则或为提交人添加例外，再重新提交。语句：%s",
				"执行前复核", fwDenyText(d), abbreviate(st, 120))
		}
	}
	return deny
}

func abbreviate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// fwExportRecheck 是导出在某个实例上执行前的复核：拒绝、行数上限与强制脱敏的列。没有启用防火墙时返回 nil。
func (s *Service) fwExportRecheck(ctx context.Context, w *model.SqlWorkflow, in *model.Instance, stmt string) *firewall.Decision {
	if s.fw == nil || in == nil || w.IsManual != 0 {
		return nil
	}
	ss := &fwSession{}
	defer ss.close()
	return s.fwEval(ctx, ss, firewall.SceneExport, w.Engineer, 0, in, w.DBName, stmt, -1)
}
