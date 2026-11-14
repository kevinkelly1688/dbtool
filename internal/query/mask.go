// @author: kevinkelly
// @license: Apache Licence
// @file: mask.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"dbtool/internal/engine"
	"dbtool/internal/masking"
	"dbtool/internal/model"
)

// SelectListProvider 给出语句每个结果列的来源（库、表、字段）。
type SelectListProvider interface {
	SelectList(ctx context.Context, inst *model.Instance, db, stmt string) ([]masking.SelectColumn, error)
}

// 这些引擎用 goInception 语法树精确定位结果列来源；其余引擎各用自己的方式（SQL Server 的结果集分析、
// PostgreSQL 的结果描述、没有来源信息的引擎按列名保守匹配），见 maskcore.go。
// PolarDB-X 不在此列：它的建表语句含 dbpartition / GLOBAL INDEX 等非 MySQL 语法，没有在真实实例上验证过
// goInception 能否取到列来源，所以与 SelectDB / Doris 一样完全按列名保守匹配（宁多勿漏）。
func usesSyntaxTree(dbType string) bool {
	switch dbType {
	case "mysql", "polardb", "adb":
		return true
	}
	return false
}

// maskOutcome 记录脱敏的执行情况，用于写入 dbt_query_log 和给用户的提示。
type maskOutcome struct {
	Ran     bool     // 脱敏流程是否执行（对应 dbt_query_log.masking）
	Hit     bool     // 是否命中脱敏规则（对应 dbt_query_log.hit_rule）
	Columns []string // 被脱敏的结果列名
	Mode    string   // "syntax-tree"（语法树）/ "source"（引擎给出的来源列）/ "column-name"（保守匹配）/ "path"（文档字段路径）
	Notice  string
}

// MaskExport 对导出查询的结果脱敏，规则与在线查询完全一致（供 sqlflow 的导出工单使用）。
// 返回被脱敏的结果列名；脱敏失败返回错误，调用方不应继续导出。
func (s *Service) MaskExport(ctx context.Context, inst *model.Instance, db, sql string, e engine.Engine, rs *engine.ResultSet) ([]string, error) {
	mo, err := s.maskResult(ctx, inst, db, sql, rs, e)
	if err != nil {
		return nil, err
	}
	return mo.Columns, nil
}

// maskResult 对 MySQL 系与 SQL Server 的查询结果脱敏（原地修改 rs.Rows）。仅处理 SELECT 语句。
// 返回的 error 表示脱敏失败，由调用方按 query.block_on_mask_error 配置决定阻断还是放行。e 是执行查询用的引擎，
// SQL Server 用它做结果列来源分析，所有引擎用它读视图定义（见 viewexp.go）。
// 视图定义没读全时（*viewDefError）其余的列已经照常脱敏：返回的 maskOutcome 带着这部分结果与说明，
// 只有经过那些视图的列可能没脱敏；其它错误返回时没有做任何脱敏。
func (s *Service) maskResult(ctx context.Context, inst *model.Instance, db, executedSQL string, rs *engine.ResultSet, e engine.Engine) (maskOutcome, error) {
	out := maskOutcome{Ran: true}
	cfg, err := s.loadMaskConfig(inst)
	if err != nil {
		return maskOutcome{}, err
	}
	if ctx.Value(onlyForcedKey{}) != nil {
		cfg = &maskConfig{} // 脱敏总开关关闭：已配置的规则不生效，只做防火墙要求的强制脱敏
	}
	cfg = cfg.withForced(ctx) // SQL 防火墙要求强制脱敏的列，优先于已配置的规则
	if cfg.empty() {
		return out, nil // 该实例没有配置任何脱敏字段
	}
	// 前置 SET 给用户变量赋的值可能来自敏感字段：脱敏的变量污染分析要看到前置语句（goInception 的列来源只针对查询本身）。
	taintSQL := executedSQL
	if pre, _ := ctx.Value(preludeKey{}).(string); pre != "" {
		taintSQL = pre + ";\n" + executedSQL
	}
	// 视图：配置写在基表字段上时，经过视图改名、套表达式之后，结果列的名字和来源都看不出它来自敏感字段，要把视图定义展开一起分析。
	vx, verr := s.expandViews(ctx, e, inst.DBType, db, taintSQL)
	out, err = s.maskColumns(ctx, inst, db, executedSQL, taintSQL, rs, e, cfg, vx, out)
	if err != nil {
		return out, err
	}
	if verr != nil {
		var ve *viewDefError
		if errors.As(verr, &ve) {
			out.Notice = joinNotice(out.Notice, ve.notice())
		}
		return out, verr
	}
	return out, nil
}

// maskColumns 定位要脱敏的结果列并就地脱敏。vx 是展开出来的视图（可为 nil）。
func (s *Service) maskColumns(ctx context.Context, inst *model.Instance, db, executedSQL, taintSQL string, rs *engine.ResultSet, e engine.Engine, cfg *maskConfig, vx *viewExpansion, out maskOutcome) (maskOutcome, error) {
	d := dialectOf(inst.DBType)
	views := vx.bodiesOf()
	switch {
	case usesSyntaxTree(inst.DBType):
		list, err := s.sel.SelectList(ctx, inst, db, executedSQL)
		switch {
		case err == nil:
			out.Mode = "syntax-tree"
			srcs, err := sourcesFromSelectList(list, len(rs.ColumnList), vx.isView)
			if err != nil {
				return maskOutcome{}, err
			}
			out.Columns, out.Hit, err = cfg.apply(taintSQL, d, views, rs.ColumnList, nil, rs.Rows, srcs)
			return out, err
		case errors.Is(err, ErrNoInception):
			// 降级：未配置 goInception（开发环境）时按结果列名保守匹配
			slog.Warn("未配置 goinception.host，数据脱敏降级为按结果列名保守匹配")
			out.Notice = "未配置 goInception，数据脱敏按结果列名与表达式引用保守匹配：可能多脱敏，请联系管理员配置 goInception 以精确定位。"
		default:
			return maskOutcome{}, err
		}
	case isMSSQL(inst.DBType):
		srcs, err := mssqlSources(ctx, e, db, executedSQL, len(rs.ColumnList))
		if err == nil {
			out.Mode = "source"
			out.Columns, out.Hit, err = cfg.apply(taintSQL, d, views, rs.ColumnList, nil, rs.Rows, srcs)
			return out, err
		}
		slog.Warn("SQL Server 结果列来源分析失败，数据脱敏降级为按结果列名保守匹配", "err", err)
		out.Notice = "无法分析该语句的结果列来源（临时表、动态 SQL 等），数据脱敏按结果列名与表达式引用保守匹配，可能多脱敏。"
	}
	out.Mode = "column-name"
	var err error
	out.Columns, out.Hit, err = cfg.apply(taintSQL, d, views, rs.ColumnList, nil, rs.Rows, nil)
	return out, err
}

// preludeKey 在 ctx 里带着前置 SET 的文本，只供 maskResult 做变量污染分析。
type preludeKey struct{}

func withPrelude(ctx context.Context, text string) context.Context {
	if text == "" {
		return ctx
	}
	return context.WithValue(ctx, preludeKey{}, text)
}

func joinNotice(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, "\n")
}
