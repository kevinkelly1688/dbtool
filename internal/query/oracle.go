// @author: kevinkelly
// @license: Apache Licence
// @file: oracle.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/engine/oracle"
	"dbtool/internal/sqlx"
)

// Oracle 的在线查询接线。引擎见 internal/engine/oracle（纯 Go 驱动 go-ora，不需要 Oracle 客户端）。
//
//   - “库”= schema（owner），“表”= 表 / 视图 / 物化视图；库级与表级权限都支持（tb_name 存表名），名字按库里的真实名字（没加引号的是大写）精确匹配；
//   - 语句涉及的表来自 oracle.CheckUserQuery 的 Checked.Tables（Schema=owner，语句里可以引用别的 schema）；
//     DUAL 与 ALL_* / USER_* 里放行的数据字典视图免表级权限（引擎已拒绝 SYS 等系统 schema、DBA_* / V$* / X$*）；
//   - 只放行 SELECT / WITH / EXPLAIN，函数只放行内置函数（存储函数 / 包按定义者权限执行，会绕过库表授权），不支持数据库链接、绑定变量、FOR UPDATE；
//   - 没有会话级只读开关：引擎在 SET TRANSACTION READ ONLY 事务里执行，目标库账号仍须只授予 SELECT（见帮助文档）。

type oraRunner interface {
	RunUser(ctx context.Context, ck *oracle.Checked, lim oracle.QueryLimits) (*oracle.Shaped, error)
}

type oraMeta interface {
	TableInfos(ctx context.Context, db string) ([]oracle.TableInfo, error)
	ColumnInfos(ctx context.Context, db, table string) ([]oracle.ColumnInfo, error)
	SearchColumns(ctx context.Context, db, kw string) ([]oracle.ColumnHit, error)
}

var (
	_ oraRunner = (*oracle.Engine)(nil)
	_ oraMeta   = (*oracle.Engine)(nil)
)

type oraPrep struct{ ck *oracle.Checked }

func oraBadQuery(err error) error {
	var ue *oracle.UserError
	if errors.As(err, &ue) {
		return newErr(400, "bad_query", ue.Msg)
	}
	return newErr(400, "bad_query", err.Error())
}

func (s *Service) checkOra(text, db string, mp *multiPrep) (Targets, string, error) {
	ck, err := oracle.CheckUserQuery(text, oracle.CheckOptions{DB: db})
	if err != nil {
		return Targets{}, "", oraBadQuery(err)
	}
	mp.ora, mp.stmtKind = &oraPrep{ck: ck}, ck.Kind
	tg := Targets{DB: db}
	for _, t := range ck.Tables {
		tg.Objects = append(tg.Objects, sqlx.TableRef{Schema: t.Schema, Name: t.Name})
	}
	return tg, ck.Stmt, nil
}

func classifyOra(err error) (string, string) {
	var ue *oracle.UserError
	switch {
	case oracle.IsTimeout(err):
		return "timeout", ""
	case oracle.IsCanceled(err) || errors.Is(err, context.Canceled):
		return "canceled", ""
	case errors.As(err, &ue):
		return "bad_query", ue.Msg
	}
	return "", ""
}

func (s *Service) runOra(ctx context.Context, e engine.Engine, mp *multiPrep, limit int, timeout time.Duration) (*multiOut, error) {
	r, ok := e.(oraRunner)
	if !ok {
		return nil, errors.New("Oracle 引擎不可用")
	}
	sh, err := r.RunUser(ctx, mp.ora.ck, oracle.QueryLimits{MaxRows: limit, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	return &multiOut{Columns: sh.ColumnList, Types: sh.ColumnTypes, Rows: sh.Rows, Truncated: sh.Truncated, TruncatedBy: sh.TruncatedBy,
		Warnings: sh.Warnings, Notices: sh.Notices, Executed: mp.ora.ck.Stmt, StmtKind: mp.stmtKind}, nil
}

// ---- 元数据 ----

func (s *Service) oraTables(ctx context.Context, e engine.Engine, db string) ([]TableMeta, error) {
	m, ok := e.(oraMeta)
	if !ok {
		return nil, errors.New("Oracle 引擎不可用")
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

func (s *Service) oraColumns(ctx context.Context, e engine.Engine, db, table string) ([]ColumnMeta, error) {
	m, ok := e.(oraMeta)
	if !ok {
		return nil, errors.New("Oracle 引擎不可用")
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

func (s *Service) oraSearch(ctx context.Context, e engine.Engine, db, kw string) ([]ColumnHit, error) {
	m, ok := e.(oraMeta)
	if !ok {
		return nil, errors.New("Oracle 引擎不可用")
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
