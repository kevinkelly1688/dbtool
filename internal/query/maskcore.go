// @author: kevinkelly
// @license: Apache Licence
// @file: maskcore.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"dbtool/internal/engine"
	mongox "dbtool/internal/engine/mongo"
	"dbtool/internal/engine/mssql"
	"dbtool/internal/engine/postgres"
	"dbtool/internal/masking"
	"dbtool/internal/model"
)

// 数据脱敏的查询侧接线（所有引擎共用）：加载实例的脱敏配置，把引擎给出的“结果列来源”与配置对照，再按规则替换值。
//
// 结果列怎么定位，各引擎能拿到的信息不同：
//   - 精确：goInception 语法树（MySQL / PolarDB / ADB）、SQL Server 的 sp_describe_first_result_set、PostgreSQL 结果描述里的表 OID + 列号；
//   - 保守：ClickHouse、SelectDB / Doris、PolarDB-X，以及上面几种拿不到来源时的降级（见 internal/masking/conservative.go）；
//   - 路径：MongoDB 在文档上按字段路径处理（见 mongomask.go）。
//
// 同一个结果里“有来源的列精确匹配、没有来源的列（表达式、视图列、来源未知）保守匹配”，两者合并，同一列取最严的规则。

// maskConfig 是某个实例当前生效的脱敏配置。
type maskConfig struct {
	cols  []masking.Column
	rules map[int]masking.Rule
}

func (c *maskConfig) empty() bool { return c == nil || len(c.cols) == 0 }

func toMaskColumn(c model.DataMaskingColumn) masking.Column {
	col := masking.Column{Schema: c.TableSchema, Table: c.TableName_, Column: c.ColumnName, RuleType: c.RuleType}
	if c.Kind != "" {
		col.Rule = &masking.Rule{Kind: c.Kind, Prefix: c.KeepPrefix, Suffix: c.KeepSuffix, HideGroup: c.HideGroup, Regex: c.RuleRegex}
	}
	return col
}

// loadMaskConfig 读取实例的脱敏配置。
func (s *Service) loadMaskConfig(inst *model.Instance) (*maskConfig, error) {
	dbCols, err := s.repo.ActiveMaskingColumns(inst.ID)
	if err != nil {
		return nil, fmt.Errorf("读取脱敏配置失败: %w", err)
	}
	cfg := &maskConfig{}
	for _, c := range dbCols {
		cfg.cols = append(cfg.cols, toMaskColumn(c))
	}
	if len(cfg.cols) == 0 {
		return cfg, nil
	}
	cfg.rules = masking.DefaultRules()
	return cfg, nil
}

// resultSource 是引擎给出的一个结果列的来源信息。
type resultSource struct {
	// Precise 是可精确对照配置的来源（视图列与它的基表列可以各有一份）。
	Precise []masking.SelectColumn
	// Conservative 为 true 表示这一列还要按列名 / 表达式引用保守匹配（表达式列、视图列、来源未知）。
	Conservative bool
}

// apply 对结果就地脱敏，返回被脱敏的结果列名与是否命中配置。srcs 为 nil 表示引擎没给任何来源，所有列都保守匹配。
func (c *maskConfig) apply(sql string, d masking.Dialect, views []masking.ViewBody, columns, types []string, rows [][]any, srcs []resultSource) ([]string, bool, error) {
	if c.empty() || len(columns) == 0 {
		return nil, false, nil
	}
	var precise []masking.SelectColumn
	for i := range columns {
		if i < len(srcs) {
			precise = append(precise, srcs[i].Precise...)
		}
	}
	byIdx := map[int]masking.Hit{}
	strict := func(h masking.Hit) int {
		r, ok := h.Resolve(c.rules)
		if !ok {
			return -1
		}
		return r.Strictness()
	}
	put := func(h masking.Hit) {
		if cur, ok := byIdx[h.Index]; !ok || strict(h) > strict(cur) {
			byIdx[h.Index] = h
		}
	}
	for _, h := range masking.Analyze(precise, c.cols) {
		put(h)
	}
	for _, h := range masking.ConservativeHitsViews(sql, columns, c.cols, d, views) {
		if srcs != nil && h.Index < len(srcs) && !srcs[h.Index].Conservative {
			continue // 有精确来源的普通列：不按列名误伤
		}
		put(h)
	}
	if len(byIdx) == 0 {
		return nil, false, nil
	}
	hits := make([]masking.Hit, 0, len(byIdx))
	for _, h := range byIdx {
		hits = append(hits, h)
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Index < hits[j].Index })
	masked, err := masking.ApplyHitsTyped(columns, types, rows, hits, c.rules)
	return masked, true, err
}

// dialectOf 返回数据库类型对应的语句方言（保守匹配用）。
func dialectOf(dbType string) masking.Dialect {
	switch dbType {
	case dbTypeMSSQL:
		return masking.DialectMSSQL
	case dbTypePG:
		return masking.DialectPostgres
	case dbTypeCH:
		return masking.DialectClickHouse
	case dbTypeOracle:
		return masking.DialectOracle
	case dbTypeDM:
		return masking.DialectDM
	}
	return masking.DialectMySQL
}

// sourcesFromSelectList 把 goInception 给出的结果列来源转成 resultSource：有库表字段的精确匹配，表达式列保守匹配。
// 来源表是视图（isView 为 true）的列也要保守匹配：配置可能写在视图的底层表上，视图把字段改了名、套了表达式，
// 精确匹配只看得到视图自己，要靠展开后的视图定义补一遍（同 PostgreSQL 的视图列）。isView 可以为 nil。
// 序号超出结果集范围时报错（无法确定该脱敏哪一列，必须当作失败处理，不能静默放行）。
func sourcesFromSelectList(list []masking.SelectColumn, n int, isView func(schema, table string) bool) ([]resultSource, error) {
	srcs := make([]resultSource, n)
	hasPrecise := make([]bool, n)
	viaView := make([]bool, n)
	for _, sc := range list {
		if sc.Index < 0 || sc.Index >= n {
			return nil, fmt.Errorf("%w（序号 %d，共 %d 列）", masking.ErrBadIndex, sc.Index, n)
		}
		if sc.Field != "" && sc.Table != "" {
			srcs[sc.Index].Precise = append(srcs[sc.Index].Precise, sc)
			hasPrecise[sc.Index] = true
			if isView != nil && isView(sc.Schema, sc.Table) {
				viaView[sc.Index] = true
			}
		}
	}
	for i := range srcs {
		srcs[i].Conservative = !hasPrecise[i] || viaView[i]
	}
	return srcs, nil
}

// mssqlDescriber 由 *mssql.Engine 实现（测试里可以换成替身）。
type mssqlDescriber interface {
	DescribeSources(ctx context.Context, db, q string) (base, views []mssql.ColumnSource, err error)
}

var _ mssqlDescriber = (*mssql.Engine)(nil)

// mssqlSources 让服务端分析第一个结果集每一列的来源。基表与视图两份都要对照（配置可能写在任何一层）。
func mssqlSources(ctx context.Context, e engine.Engine, db, q string, n int) ([]resultSource, error) {
	d, ok := e.(mssqlDescriber)
	if !ok {
		return nil, errors.New("引擎不支持结果列来源分析")
	}
	base, views, err := d.DescribeSources(ctx, db, q)
	if err != nil {
		return nil, err
	}
	if len(base) != n {
		return nil, fmt.Errorf("服务端给出 %d 列来源，结果集有 %d 列", len(base), n)
	}
	srcs := make([]resultSource, n)
	add := func(i int, c mssql.ColumnSource) {
		srcs[i].Precise = append(srcs[i].Precise, masking.SelectColumn{Index: i, Field: c.Column, Table: c.Schema + "." + c.Table, Schema: c.DB})
	}
	for i := 0; i < n; i++ {
		b := base[i]
		if b.Column == "" || b.Table == "" {
			srcs[i].Conservative = true // 表达式列、常量
			continue
		}
		add(i, b)
		if i < len(views) && views[i].Column != "" && views[i].Table != "" && views[i] != b {
			add(i, views[i])
		}
	}
	return srcs, nil
}

const maskSkippedNotice = "数据脱敏出错，本次结果未脱敏（系统设置 query.block_on_mask_error 为关闭状态）。"

// maskFailure 处理脱敏失败：严格模式下的业务错误、以及开启 query.block_on_mask_error 时的任何脱敏错误都阻断，不返回数据（返回非 nil）；
// query.block_on_mask_error 关闭时放行未（完整）脱敏的数据并留下告警（返回 nil）。
func (s *Service) maskFailure(p Principal, inst *model.Instance, pol Policy, merr error) error {
	slog.Error("数据脱敏失败", "err", merr, "user", p.Username, "instance", inst.InstanceName)
	var be *Error
	if errors.As(merr, &be) || pol.QueryCheck {
		return newErr(500, "masking_error", "数据脱敏异常，结果未返回，请联系管理员。")
	}
	return nil
}

// multiMaskable 判断 PostgreSQL / ClickHouse / Oracle / 达梦 / MongoDB 的这条语句是否返回业务数据（SHOW / EXPLAIN / 元数据类语句不脱敏）。
func multiMaskable(mp *multiPrep) bool {
	switch mp.typ {
	case dbTypePG:
		return mp.pg != nil && mp.pg.ck.Kind == postgres.KindSelect
	case dbTypeCH, dbTypeOracle, dbTypeDM:
		return mp.stmtKind == "select"
	case dbTypeMongo:
		switch mongox.Kind(mp.stmtKind) {
		case mongox.KindFind, mongox.KindAggregate, mongox.KindDistinct:
			return true
		}
	}
	return false
}
