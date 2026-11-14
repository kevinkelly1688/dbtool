// @author: kevinkelly
// @license: Apache Licence
// @file: estimate.go
// @time: 2026/10/28

package query

import (
	"context"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/sqlx"
)

// Estimate 是结果被行数上限截断时，对“不加上限会有多少行”的估算。
type Estimate struct {
	Rows   int64  `json:"rows"`   // 估算行数，-1 表示无法估算
	Method string `json:"method"` // explain：由 EXPLAIN 的 rows × filtered 推算
}

// EstimateRows 用 EXPLAIN 估算一条 SELECT 不加行数上限时的结果行数（只读元信息，不执行语句、不写查询日志）。
// 走与 Execute 完全相同的语句检查和库表权限，所以用户拿不到自己无权查询的表的行数；
// 估算 = 主查询（最小 id 的计划行）各行 rows × filtered% 的乘积，仅供参考；无法估算（非 MySQL 系、非 SELECT、聚合等）返回 -1。
func (s *Service) EstimateRows(ctx context.Context, p Principal, req Request) (*Estimate, error) {
	tr := &execTrace{dry: true} // 只估算不执行：防火墙不写命中记录，也不要求填写原因
	pr, err := s.prepare(ctx, p, req, tr)
	if err != nil {
		return nil, err
	}
	unknown := &Estimate{Rows: -1}
	if pr.kind != "select" || isMSSQL(pr.inst.DBType) {
		return unknown, nil
	}
	// 去掉末尾 LIMIT 后再 EXPLAIN：估算的是“没有上限”的行数。
	base := sqlx.RewriteLimit(pr.stmt, 0)
	base = stripTrailingLimit(base)
	e, err := s.open(pr.inst.DBType, s.repo.EngineConn(pr.inst))
	if err != nil {
		return unknown, nil
	}
	defer e.Close()
	qctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	rs, err := e.Query(qctx, "EXPLAIN "+base, engine.QueryOptions{DBName: req.DB, MaxExecMs: 8000, Prelude: pr.pre.setStmts()})
	if err != nil || rs == nil {
		return unknown, nil
	}
	return &Estimate{Rows: estimateFromPlan(rs.ColumnList, rs.Rows), Method: "explain"}, nil
}

// stripTrailingLimit 去掉语句末尾（括号深度 0）的 LIMIT 子句。
func stripTrailingLimit(stmt string) string {
	sig := sqlx.Significant(sqlx.Tokenize(stmt))
	depth := 0
	idx := -1
	for _, t := range sig {
		switch {
		case t.Kind == sqlx.Punct && t.Text == "(":
			depth++
		case t.Kind == sqlx.Punct && t.Text == ")":
			depth--
		case depth == 0 && t.Kind == sqlx.Word && t.Lower() == "limit":
			idx = t.Start
		}
	}
	if idx < 0 {
		return stmt
	}
	return strings.TrimSpace(stmt[:idx])
}

// estimateFromPlan 由 EXPLAIN 结果推算行数：取 id 最小的那组计划行，rows × filtered/100 逐行相乘。
func estimateFromPlan(cols []string, rows [][]any) int64 {
	ii, ri, fi := colIndex(cols, "id"), colIndex(cols, "rows"), colIndex(cols, "filtered")
	if ri < 0 || len(rows) == 0 {
		return -1
	}
	minID := int64(1 << 62)
	for _, r := range rows {
		if ii >= 0 {
			if n, ok := toInt64(r[ii]); ok && n < minID {
				minID = n
			}
		}
	}
	total, any := 1.0, false
	for _, r := range rows {
		if ii >= 0 {
			if n, ok := toInt64(r[ii]); !ok || n != minID {
				continue
			}
		}
		n, ok := toInt64(r[ri])
		if !ok {
			continue
		}
		f := 100.0
		if fi >= 0 && r[fi] != nil {
			if v, ok := toFloat(r[fi]); ok {
				f = v
			}
		}
		total *= float64(n) * f / 100
		any = true
	}
	if !any {
		return -1
	}
	if total < 1 {
		total = 1
	}
	if total > 9e15 {
		total = 9e15
	}
	return int64(total)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	case []byte:
		return toFloat(string(x))
	}
	if n, ok := toInt64(v); ok {
		return float64(n), true
	}
	return 0, false
}
