// @author: kevinkelly
// @license: Apache Licence
// @file: pgsql.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/engine/postgres"
	"dbtool/internal/masking"
	"dbtool/internal/sqlx"
)

// PostgreSQL 的在线查询接线。引擎见 internal/engine/postgres。
//
//   - “库”= PostgreSQL database，表名 schema.table；库级与表级权限都支持（dbt_query_privileges，tb_name 存 schema.table）；
//   - 语句涉及的表来自 postgres.CheckUserQuery 的 Checked.Tables；pg_catalog / information_schema 免表级权限（敏感视图已被引擎拒绝），
//     其它系统 schema 拒绝；FROM 里调用了非内置函数（NeedAllPriv）时要求该库的库级权限；SHOW 参数名按结构查询处理。

// pgRunner / pgMeta 由 *postgres.Engine 实现（测试里可以换成替身）。
type pgRunner interface {
	RunUser(ctx context.Context, db string, c *postgres.Checked, lim postgres.QueryLimits) (*postgres.Shaped, error)
}

type pgMeta interface {
	TableInfos(ctx context.Context, db string) ([]postgres.TableInfo, error)
	ColumnInfos(ctx context.Context, db, table string) ([]postgres.ColumnInfo, error)
}

var (
	_ pgRunner = (*postgres.Engine)(nil)
	_ pgMeta   = (*postgres.Engine)(nil)
)

type pgPrep struct{ ck *postgres.Checked }

func pgBadQuery(err error) error {
	var ue *postgres.UserError
	if errors.As(err, &ue) {
		return newErr(400, "bad_query", ue.Msg)
	}
	return newErr(400, "bad_query", err.Error())
}

// checkPG 完成静态检查并整理出授权要看的对象。不访问数据库。
func (s *Service) checkPG(text, db string, mp *multiPrep) (Targets, string, error) {
	ck, err := postgres.CheckUserQuery(text, postgres.CheckOptions{DBName: db})
	if err != nil {
		return Targets{}, "", pgBadQuery(err)
	}
	mp.pg, mp.stmtKind = &pgPrep{ck: ck}, ck.Kind
	tg := Targets{DB: db}
	switch {
	case ck.Kind == postgres.KindShow:
		tg.Structure = true
	case ck.NeedAllPriv:
		fn := ""
		if len(ck.Functions) > 0 {
			fn = "（" + strings.Join(ck.Functions, "、") + "）"
		}
		tg.RequireDB = fmt.Sprintf("该语句在 FROM 里调用了自定义函数%s，无法确定实际访问的表，需要库 %s 的库级查询权限；请申请库级权限，或改用内置函数。", fn, db)
	}
	for _, t := range ck.Tables {
		if postgres.IsSystemSchema(t.Schema) {
			if ls := strings.ToLower(t.Schema); ls == "pg_catalog" || ls == "information_schema" {
				continue
			}
			return Targets{}, "", newErr(400, "bad_query", fmt.Sprintf("不允许查询系统 schema %s", t.Schema))
		}
		tg.Objects = append(tg.Objects, sqlx.TableRef{Schema: db, Name: t.Schema + "." + t.Name})
	}
	return tg, ck.SQL, nil
}

func classifyPG(err error) (string, string) {
	var ue *postgres.UserError
	switch {
	case postgres.IsTimeout(err):
		return "timeout", ""
	case errors.Is(err, context.Canceled):
		return "canceled", ""
	case errors.As(err, &ue):
		return "bad_query", ue.Msg
	}
	return "", ""
}

func (s *Service) runPG(ctx context.Context, e engine.Engine, mp *multiPrep, limit int, timeout time.Duration) (*multiOut, error) {
	r, ok := e.(pgRunner)
	if !ok {
		return nil, errors.New("PostgreSQL 引擎不可用")
	}
	sh, err := r.RunUser(ctx, mp.db, mp.pg.ck, postgres.QueryLimits{MaxRows: limit, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	out := &multiOut{Columns: sh.ColumnNames(), Rows: sh.Rows, Truncated: sh.Truncated, Warnings: sh.Warnings, Executed: sh.ExecutedSQL, StmtKind: mp.stmtKind}
	out.Types = make([]string, len(sh.Columns))
	for i, c := range sh.Columns {
		out.Types[i] = c.Type
	}
	switch sh.TruncatedBy {
	case postgres.TruncRows:
		out.TruncatedBy = "rows"
	case postgres.TruncBytes:
		out.TruncatedBy = "bytes"
	}
	if out.Executed == "" {
		out.Executed = mp.pg.ck.SQL
	}
	if sh.SourcesKnown {
		out.Sources = make([]resultSource, len(sh.Columns))
		for i, c := range sh.Columns {
			if c.Source == nil {
				out.Sources[i].Conservative = true // 表达式列
				continue
			}
			out.Sources[i].Precise = []masking.SelectColumn{{Index: i, Field: c.Source.Column, Table: c.Source.FullTable(), Schema: mp.db}}
			out.Sources[i].Conservative = c.Source.IsView() // 视图列：配置可能写在底层表上，再按列名保守补一遍
		}
	}
	return out, nil
}

// ---- 元数据 ----

func (s *Service) pgTables(ctx context.Context, e engine.Engine, db string) ([]TableMeta, error) {
	m, ok := e.(pgMeta)
	if !ok {
		return nil, errors.New("PostgreSQL 引擎不可用")
	}
	infos, err := m.TableInfos(ctx, db)
	if err != nil {
		return s.namesOnly(ctx, e, db)
	}
	out := make([]TableMeta, 0, len(infos))
	for _, i := range infos {
		t := TableMeta{Name: i.FullName, Schema: i.Schema, Rows: -1, Comment: i.Comment, Kind: "table"}
		switch i.Type {
		case "VIEW":
			t.View, t.Kind = true, "view"
		case "MATERIALIZED VIEW":
			t.View, t.Kind = true, "matview"
		case "FOREIGN TABLE":
			t.Kind = "foreign"
		case "PARTITIONED TABLE":
			t.Kind = "partitioned"
		}
		if t.Name == "" {
			t.Name = i.Schema + "." + i.Name
		}
		if !t.View && i.RowsEstimate >= 0 {
			t.Rows = i.RowsEstimate
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *Service) pgColumns(ctx context.Context, e engine.Engine, db, table string) ([]ColumnMeta, error) {
	m, ok := e.(pgMeta)
	if !ok {
		return nil, errors.New("PostgreSQL 引擎不可用")
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

// pgLit 是 PostgreSQL 的字符串字面量（standard_conforming_strings=on，只需把单引号加倍）。
func pgLit(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// pgLikeEsc 转义 LIKE 通配符（默认转义符是反斜杠）。
var pgLikeEsc = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (s *Service) pgSearch(ctx context.Context, e engine.Engine, db, kw string) ([]ColumnHit, error) {
	pat := pgLit("%" + pgLikeEsc.Replace(kw) + "%")
	q := "SELECT n.nspname || '.' || c.relname, a.attname, COALESCE(d.description, '') " +
		"FROM pg_catalog.pg_attribute a " +
		"JOIN pg_catalog.pg_class c ON c.oid = a.attrelid " +
		"JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace " +
		"LEFT JOIN pg_catalog.pg_description d ON d.objoid = a.attrelid AND d.objsubid = a.attnum " +
		"WHERE a.attnum > 0 AND NOT a.attisdropped AND c.relkind IN ('r', 'p', 'v', 'm', 'f') " +
		"AND n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\\_%' " +
		"AND has_any_column_privilege(c.oid, 'SELECT') " +
		"AND (a.attname ILIKE " + pat + " OR d.description ILIKE " + pat + ") " +
		"ORDER BY 1, 2 LIMIT 500"
	rs, err := e.Query(ctx, q, engine.QueryOptions{DBName: db, MaxExecMs: 10000, Limit: 500})
	if err != nil {
		return nil, err
	}
	out := make([]ColumnHit, 0, len(rs.Rows))
	for _, r := range rs.Rows {
		out = append(out, ColumnHit{Table: rowStr(r, 0), Column: rowStr(r, 1), Comment: rowStr(r, 2)})
	}
	return out, nil
}
