// @author: kevinkelly
// @license: Apache Licence
// @file: multi.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/firewall"
	"dbtool/internal/model"
	"dbtool/internal/sqlx"
)

// PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦的在线查询。
//
//   - 语句检查、只读防护、限量、结果整形全部在各自的引擎包里（internal/engine/postgres | mongo | clickhouse | oracle | dm）；这里只做接线：
//     引擎静态检查 → 库表级授权（Targets）→ 并发闸 → 引擎 RunUser → 响应整形 → 查询日志；
//   - 查询日志、被拦截 / 失败记录、行数上限、超时、实例停用与资源组可见性沿用 MySQL / Redis 的流水线；
//   - 数据脱敏（见 maskcore.go / mongomask.go）：PostgreSQL 用结果列来源精确匹配，ClickHouse / Oracle / 达梦保守匹配，MongoDB 在文档上按字段路径处理；
//     系统脱敏总开关关闭时结果带 unmasked=true，页面据此提示。
//
// MySQL / SQL Server / Redis 的代码路径不受影响：只在实例类型为 pgsql / mongo / clickhouse / oracle / dm 时进入这里的分支。

const (
	dbTypePG     = "pgsql"
	dbTypeMongo  = "mongo"
	dbTypeCH     = "clickhouse"
	dbTypeOracle = "oracle"
	dbTypeDM     = "dm"
)

func isMulti(dbType string) bool {
	return dbType == dbTypePG || dbType == dbTypeMongo || dbType == dbTypeCH || dbType == dbTypeOracle || dbType == dbTypeDM
}

const (
	multiMaxStmtBytes   = 128 << 10 // 语句文本上限（各引擎自己还有更严的上限）
	multiUserConcurrent = 3         // 每个用户同时执行的此类查询数上限（这几种类型共用，Redis 另计）
)

// multiPrep 是通过引擎检查与授权的语句。
type multiPrep struct {
	typ      string
	db       string
	stmtKind string
	pg       *pgPrep
	mg       *mongoPrep
	ch       *chPrep
	ora      *oraPrep
	dm       *dmPrep
}

// prepareMulti 是 prepare 的分支：库名、引擎静态检查、库表授权、行数上限。
func (s *Service) prepareMulti(ctx context.Context, p Principal, req Request, inst *model.Instance, pol Policy, tr *execTrace) (*prepared, error) {
	db := strings.TrimSpace(req.DB)
	if db == "" {
		return nil, newErr(400, "bad_request", "请选择数据库")
	}
	if strings.TrimSpace(req.SQL) == "" {
		return nil, newErr(400, "bad_query", "语句为空")
	}
	if len(req.SQL) > multiMaxStmtBytes {
		return nil, newErr(400, "bad_query", fmt.Sprintf("语句过长（上限 %d KB）", multiMaxStmtBytes>>10))
	}
	var (
		mp  = &multiPrep{typ: inst.DBType, db: db}
		tg  Targets
		run string
		err error
	)
	switch inst.DBType {
	case dbTypePG:
		tg, run, err = s.checkPG(req.SQL, db, mp)
	case dbTypeMongo:
		tg, run, err = s.checkMongo(req.SQL, db, mp)
	case dbTypeCH:
		tg, run, err = s.checkCH(req.SQL, db, mp)
	case dbTypeOracle:
		tg, run, err = s.checkOra(req.SQL, db, mp)
	case dbTypeDM:
		tg, run, err = s.checkDM(req.SQL, db, mp)
	default:
		return nil, newErr(501, "unsupported_engine", "该类型的实例暂不支持在线查询")
	}
	if err != nil {
		return nil, err
	}
	// SQL 防火墙（PostgreSQL / ClickHouse / Oracle / 达梦；MongoDB 是文档管道，不进防火墙）。这几类引擎暂不支持强制脱敏，该动作会降级为告警。
	var fd *firewall.Decision
	if inst.DBType != dbTypeMongo {
		objs := func() ([]sqlx.TableRef, error) { return tg.Objects, nil }
		if fd, err = s.firewallGate(ctx, p, inst, req, run, firewall.Caps{Reason: true, Limit: true}, tr, objs, nil); err != nil {
			return nil, err
		}
	}
	var dec Decision
	if ta, ok := s.auth.(TargetAuthorizer); ok {
		dec, err = ta.AuthorizeTargets(ctx, p, inst, tg, pol.AdminLimit)
	} else {
		dec, err = s.auth.AuthorizeQuery(ctx, p, inst, db, run, pol.AdminLimit)
	}
	if err != nil {
		return nil, err
	}
	limit := dec.MaxRows
	if req.Limit > 0 && (limit <= 0 || req.Limit < limit) {
		limit = req.Limit
	}
	limit, dec, fwLimited := applyFirewallLimit(fd, limit, dec)
	if inst.DBType == dbTypeMongo { // Mongo 的 limit 在检查阶段按行数上限收窄（并给出提示），所以授权之后再检查一次
		if err := s.recheckMongo(req.SQL, db, limit, mp); err != nil {
			return nil, err
		}
		run = mp.mg.ck.Statement
	}
	return &prepared{inst: inst, pol: pol, stmt: run, run: run, kind: inst.DBType, limit: limit, dec: dec, multi: mp, fw: fd, fwLimited: fwLimited}, nil
}

// ---- 并发限制 ----

var multiSlots = struct {
	sync.Mutex
	n map[string]int
}{n: map[string]int{}}

// acquireMultiSlot 限制每个用户同时执行的 PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦查询数（这几种类型共用一个计数）。
func acquireMultiSlot(user, dbType string) (func(), error) {
	multiSlots.Lock()
	defer multiSlots.Unlock()
	if multiSlots.n[user] >= multiUserConcurrent {
		return nil, newErr(429, "busy", fmt.Sprintf("你同时执行的 PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦查询已达 %d 条，请等待其中一条完成后再试", multiUserConcurrent))
	}
	multiSlots.n[user]++
	return func() {
		multiSlots.Lock()
		defer multiSlots.Unlock()
		if multiSlots.n[user] <= 1 {
			delete(multiSlots.n, user)
		} else {
			multiSlots.n[user]--
		}
	}, nil
}

// ---- 执行 ----

// multiOut 是各引擎结果整形后的共同形态。
type multiOut struct {
	Columns     []string
	Types       []string
	Rows        [][]any
	Truncated   bool
	TruncatedBy string // rows / bytes / response
	Warnings    []string
	Notices     []string
	Executed    string
	StmtKind    string
	// Sources 是引擎给出的结果列来源（目前只有 PostgreSQL）；nil 表示引擎没给，数据脱敏按列名保守匹配。
	Sources []resultSource
	// MaskedCols 是引擎内部已完成脱敏时（MongoDB 在文档上处理）被脱敏的顶层字段。
	MaskedCols []string
	// ViewErr 非空表示语句用到的某些视图的定义没读全（*viewDefError）：这些视图没有展开，其余的字段已按配置脱敏（目前只有 MongoDB 在引擎内部脱敏）。
	ViewErr error
}

// multiFail 把执行 / 连接错误转成业务错误。engineErr 是各引擎对自己错误的分类（UserError → bad_query，超时，取消）。
func multiFail(ctx, qctx context.Context, err error, pol Policy, classify func(error) (code, msg string)) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return newErr(499, "canceled", "查询已取消")
	}
	timeoutMsg := fmt.Sprintf("查询超过 %d 秒已终止，请缩小范围或增加过滤条件。", pol.MaxExecSeconds)
	if errors.Is(qctx.Err(), context.DeadlineExceeded) {
		return newErr(504, "timeout", timeoutMsg)
	}
	if errors.Is(err, engine.ErrReadOnlyViolation) {
		// 语句通过了静态检查，却被数据库的只读事务挡下：检查有漏洞或有人在试探，留痕为“被拦截”。
		slog.Warn("在线查询的语句被数据库只读事务拒绝", "err", err)
		return newErr(403, "readonly_violation", "该语句会修改数据，已被数据库的只读事务拒绝。在线查询只允许查询；数据变更请提交 SQL 工单。")
	}
	switch code, msg := classify(err); code {
	case "timeout":
		return newErr(504, "timeout", timeoutMsg)
	case "canceled":
		return newErr(499, "canceled", "查询已取消")
	case "bad_query":
		return newErr(400, "bad_query", msg)
	}
	return newErr(statusTargetError, "engine_error", err.Error())
}

// runMulti 执行已通过检查与授权的语句并整形结果（Execute 的分支）。
func (s *Service) runMulti(ctx context.Context, p Principal, req Request, pr *prepared, tr *execTrace) (*Result, error) {
	mp := pr.multi
	release, err := acquireMultiSlot(p.Username, mp.typ)
	if err != nil {
		return nil, err
	}
	defer release()
	e, err := s.open(pr.inst.DBType, s.repo.EngineConn(pr.inst))
	if err != nil {
		return nil, newErr(501, "unsupported_engine", err.Error())
	}
	defer e.Close()

	// 引擎自己带执行时限（服务端 + 客户端两层），这里的外层上限略宽，让引擎先报出更准确的超时原因。
	timeout := time.Duration(pr.pol.MaxExecSeconds) * time.Second
	qctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()

	// 数据脱敏：只处理读数据的语句。配置读不出来时，按 query.block_on_mask_error 决定阻断还是放行（同 MySQL 系）。
	var mcfg *maskConfig
	var mnotice string
	if pr.pol.DataMasking && multiMaskable(mp) {
		cfg, merr := s.loadMaskConfig(pr.inst)
		if merr != nil {
			if blk := s.maskFailure(p, pr.inst, pr.pol, merr); blk != nil {
				return nil, blk
			}
			mnotice = maskSkippedNotice
		} else {
			mcfg = cfg
		}
	}

	start := time.Now()
	var out *multiOut
	var classify func(error) (string, string)
	switch mp.typ {
	case dbTypePG:
		out, err = s.runPG(qctx, e, mp, pr.limit, timeout)
		classify = classifyPG
	case dbTypeMongo:
		out, err = s.runMongo(qctx, e, mp, pr.limit, timeout, mcfg)
		classify = classifyMongo
	case dbTypeCH:
		out, err = s.runCH(qctx, e, mp, pr.limit, timeout)
		classify = classifyCH
	case dbTypeOracle:
		out, err = s.runOra(qctx, e, mp, pr.limit, timeout)
		classify = classifyOra
	case dbTypeDM:
		out, err = s.runDM(qctx, e, mp, pr.limit, timeout)
		classify = classifyDM
	}
	cost := time.Since(start)
	tr.cost = cost
	if err != nil {
		return nil, multiFail(ctx, qctx, err, pr.pol, classify)
	}

	var mo maskOutcome
	switch {
	case mcfg == nil: // 没有开启脱敏，或这条语句不返回数据
	case mcfg.empty():
		mo.Ran = true // 开启了脱敏，该实例没有配置任何字段
	case mp.typ == dbTypeMongo:
		mo.Ran, mo.Mode, mo.Columns, mo.Hit = true, "path", out.MaskedCols, len(out.MaskedCols) > 0
		if out.ViewErr != nil { // 视图定义没读全：其余的字段已经按配置脱敏，只有经过那些视图的字段可能漏
			if blk := s.maskFailure(p, pr.inst, pr.pol, out.ViewErr); blk != nil {
				return nil, blk
			}
			var ve *viewDefError
			if errors.As(out.ViewErr, &ve) {
				mnotice = joinNotice(mnotice, ve.notice())
			}
		}
	default:
		mo.Ran, mo.Mode = true, "column-name"
		if out.Sources != nil {
			mo.Mode = "source"
		}
		var merr error
		// 视图：配置写在基表字段上时，经过视图改名、套表达式之后列名和来源都看不出来，要把视图定义展开一起分析（PostgreSQL、ClickHouse、Oracle、达梦提供视图定义；MongoDB 的视图在 runMongo 里处理）
		vx, verr := s.expandViews(ctx, e, pr.inst.DBType, mp.db, out.Executed)
		mo.Columns, mo.Hit, merr = mcfg.apply(out.Executed, dialectOf(mp.typ), vx.bodiesOf(), out.Columns, out.Types, out.Rows, out.Sources)
		if merr != nil {
			if blk := s.maskFailure(p, pr.inst, pr.pol, merr); blk != nil {
				return nil, blk
			}
			mo, mnotice = maskOutcome{}, maskSkippedNotice
		}
		if merr == nil && verr != nil { // 视图定义没读全：其余的列已经按配置脱敏，只有经过那些视图的列可能漏
			if blk := s.maskFailure(p, pr.inst, pr.pol, verr); blk != nil {
				return nil, blk
			}
			var ve *viewDefError
			if errors.As(verr, &ve) {
				mnotice = joinNotice(mnotice, ve.notice())
			}
		}
	}
	warnings := out.Warnings
	if mnotice != "" {
		warnings = append(append([]string(nil), warnings...), mnotice)
	}
	fwi := fwInfo(pr.fw, pr.fwLimited)
	if n := fwNotice(fwi); n != "" {
		warnings = append(append([]string(nil), warnings...), strings.Split(n, "\n")...)
	}
	res := &Result{
		Columns: out.Columns, Rows: out.Rows, RowCount: len(out.Rows), EffectiveLimit: pr.limit, LimitSource: pr.dec.Source,
		Truncated: out.Truncated, CostMs: cost.Milliseconds(), MaskedColumns: []string{}, ExecutedSQL: out.Executed,
		Kind: mp.typ, StmtKind: out.StmtKind, ColumnTypes: out.Types, Warnings: warnings, Notices: out.Notices,
		Unmasked: !pr.pol.DataMasking, MaskMode: mo.Mode, Firewall: fwi,
	}
	if mo.Columns != nil {
		res.MaskedColumns = mo.Columns
	}
	if out.Truncated {
		res.TruncatedBy = out.TruncatedBy
	}
	if res.Rows == nil {
		res.Rows = [][]any{}
	}
	if res.Columns == nil {
		res.Columns = []string{}
	}
	log := &model.QueryLog{
		InstanceName: pr.inst.InstanceName, DBName: mp.db, SQLLog: out.Executed, EffectRow: int64(len(out.Rows)),
		CostTime: strconv.FormatFloat(cost.Seconds(), 'f', 3, 64), Username: p.Username, UserDisplay: p.Display,
		PrivCheck: pr.dec.PrivCheck, HitRule: mo.Hit, Masking: mo.Ran,
	}
	if err := s.repo.InsertQueryLog(log); err != nil {
		slog.Error("写入 dbt_query_log 失败，已拒绝返回查询结果", "err", err, "user", p.Username, "instance", pr.inst.InstanceName, "type", mp.typ)
		return nil, newErr(500, "audit_failed", "查询已执行，但审计日志写入失败，结果未返回，请联系管理员。")
	}
	res.QueryLogID = log.ID
	if s.fw != nil {
		s.fw.AttachQueryLog(pr.fw, log.ID)
	}
	return res, nil
}
