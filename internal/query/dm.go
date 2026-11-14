// @author: kevinkelly
// @license: Apache Licence
// @file: dm.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/engine/dm"
	"dbtool/internal/sqlx"
)

// 达梦（DM）的在线查询接线。引擎见 internal/engine/dm（达梦官方 Go 驱动，纯 Go，不需要达梦客户端）。
//
//   - “库”= schema，“表”= 表 / 视图 / 物化视图；库级与表级权限都支持（tb_name 存表名），名字按库里的真实名字（没加引号的是大写）精确匹配；
//   - 语句涉及的表来自 dm.CheckUserQuery 的 Checked.Tables（Schema=schema，语句里可以引用别的 schema）；
//     DUAL 与 ALL_* / USER_* 里放行的数据字典视图免表级权限（引擎已拒绝 SYS 等系统 schema、DBA_* / V$*）；
//   - 只放行 SELECT / WITH / EXPLAIN，函数只放行内置函数（存储函数 / 包按定义者权限执行，会绕过库表授权），不支持数据库链接、绑定变量、FOR UPDATE；
//   - 引擎在 SET TRANSACTION READ ONLY 事务里执行；目标库账号仍须只授予 SELECT（见帮助文档）。

type dmRunner interface {
	RunUser(ctx context.Context, ck *dm.Checked, lim dm.QueryLimits) (*dm.Shaped, error)
}

type dmMeta interface {
	TableInfos(ctx context.Context, db string) ([]dm.TableInfo, error)
	ColumnInfos(ctx context.Context, db, table string) ([]dm.ColumnInfo, error)
	SearchColumns(ctx context.Context, db, kw string) ([]dm.ColumnHit, error)
}

var (
	_ dmRunner = (*dm.Engine)(nil)
	_ dmMeta   = (*dm.Engine)(nil)
)

type dmPrep struct{ ck *dm.Checked }

func dmBadQuery(err error) error {
	var ue *dm.UserError
	if errors.As(err, &ue) {
		return newErr(400, "bad_query", ue.Msg)
	}
	return newErr(400, "bad_query", err.Error())
}

func (s *Service) checkDM(text, db string, mp *multiPrep) (Targets, string, error) {
	ck, err := dm.CheckUserQuery(text, dm.CheckOptions{DB: db})
	if err != nil {
		return Targets{}, "", dmBadQuery(err)
	}
	mp.dm, mp.stmtKind = &dmPrep{ck: ck}, ck.Kind
	tg := Targets{DB: db}
	for _, t := range ck.Tables {
		tg.Objects = append(tg.Objects, sqlx.TableRef{Schema: t.Schema, Name: t.Name})
	}
	return tg, ck.Stmt, nil
}

func classifyDM(err error) (string, string) {
	var ue *dm.UserError
	switch {
	case dm.IsTimeout(err):
		return "timeout", ""
	case dm.IsCanceled(err) || errors.Is(err, context.Canceled):
		return "canceled", ""
	case errors.As(err, &ue):
		return "bad_query", ue.Msg
	}
	return "", ""
}

func (s *Service) runDM(ctx context.Context, e engine.Engine, mp *multiPrep, limit int, timeout time.Duration) (*multiOut, error) {
	r, ok := e.(dmRunner)
	if !ok {
		return nil, errors.New("达梦引擎不可用")
	}
	sh, err := r.RunUser(ctx, mp.dm.ck, dm.QueryLimits{MaxRows: limit, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	return &multiOut{Columns: sh.ColumnList, Types: sh.ColumnTypes, Rows: sh.Rows, Truncated: sh.Truncated, TruncatedBy: sh.TruncatedBy,
		Warnings: sh.Warnings, Notices: sh.Notices, Executed: mp.dm.ck.Stmt, StmtKind: mp.stmtKind}, nil
}

// ---- 元数据 ----

func (s *Service) dmTables(ctx context.Context, e engine.Engine, db string) ([]TableMeta, error) {
	m, ok := e.(dmMeta)
	if !ok {
		return nil, errors.New("达梦引擎不可用")
	}
	infos, err := m.TableInfos(ctx, db)
	if err != nil {
		return s.namesOnly(ctx, e, db)
	}
	out := make([]TableMeta, 0, len(infos))
	for _, i := range infos {
		t := TableMeta{Name: i.Name, Rows: -1, Comment: i.Comment, Kind: i.Kind, View: i.IsView}
		if i.Rows != nil && !t.View {
			t.Rows = *i.Rows
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *Service) dmColumns(ctx context.Context, e engine.Engine, db, table string) ([]ColumnMeta, error) {
	m, ok := e.(dmMeta)
	if !ok {
		return nil, errors.New("达梦引擎不可用")
	}
	cols, err := m.ColumnInfos(ctx, db, table)
	if err != nil {
		return nil, err
	}
	out := make([]ColumnMeta, 0, len(cols))
	for _, c := range cols {
		cm := ColumnMeta{Name: c.Name, Type: c.Type, Nullable: c.Nullable, Comment: c.Comment}
		if c.IsPrimaryKey {
			cm.Key = "PRI"
		}
		out = append(out, cm)
	}
	return out, nil
}

func (s *Service) dmSearch(ctx context.Context, e engine.Engine, db, kw string) ([]ColumnHit, error) {
	m, ok := e.(dmMeta)
	if !ok {
		return nil, errors.New("达梦引擎不可用")
	}
	hits, err := m.SearchColumns(ctx, db, kw)
	if err != nil {
		return nil, err
	}
	out := make([]ColumnHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, ColumnHit{Table: h.Table, Column: h.Column, Comment: h.Comment})
	}
	return out, nil
}
