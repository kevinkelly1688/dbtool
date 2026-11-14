// @author: kevinkelly
// @license: Apache Licence
// @file: clickhouse.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"strings"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/engine/clickhouse"
	"dbtool/internal/sqlx"
)

// ClickHouse 的在线查询接线。引擎见 internal/engine/clickhouse（HTTP 接口）。
//
//   - “库”= database，“表”= 表 / 视图 / 字典；库级与表级权限都支持（tb_name 存表名）；
//   - 语句涉及的表来自 clickhouse.CheckUserQuery 的 Checked.Tables（Schema=库名，语句里可以引用别的库）；
//     system 库里引擎放行的摘要表与 INFORMATION_SCHEMA 免表级权限（其余 system 表引擎已拒绝）；
//   - SHOW / DESCRIBE / EXISTS 按结构查询处理（不校验表权限，引用的库要通过实例的库名过滤规则）；
//   - merge() 等无法静态确定涉及对象的表函数由引擎直接拒绝。

type chRunner interface {
	RunUser(ctx context.Context, ck *clickhouse.Checked, lim clickhouse.QueryLimits) (*clickhouse.Shaped, error)
}

type chMeta interface {
	TableInfos(ctx context.Context, db string) ([]clickhouse.TableInfo, error)
	ColumnInfos(ctx context.Context, db, table string) ([]clickhouse.ColumnInfo, error)
}

var (
	_ chRunner = (*clickhouse.Engine)(nil)
	_ chMeta   = (*clickhouse.Engine)(nil)
)

type chPrep struct{ ck *clickhouse.Checked }

func chBadQuery(err error) error {
	var ue *clickhouse.UserError
	if errors.As(err, &ue) {
		return newErr(400, "bad_query", ue.Msg)
	}
	return newErr(400, "bad_query", err.Error())
}

// chExemptDB 判断该库里引擎放行的对象可以免表级权限（system 的摘要表、INFORMATION_SCHEMA）。
func chExemptDB(db string) bool {
	l := strings.ToLower(db)
	return l == "system" || l == "information_schema"
}

func (s *Service) checkCH(text, db string, mp *multiPrep) (Targets, string, error) {
	ck, err := clickhouse.CheckUserQuery(text, clickhouse.CheckOptions{DB: db})
	if err != nil {
		return Targets{}, "", chBadQuery(err)
	}
	mp.ch, mp.stmtKind = &chPrep{ck: ck}, ck.Kind
	tg := Targets{DB: db}
	switch ck.Kind {
	case "show", "describe", "exists":
		tg.Structure = true
		for _, d := range ck.Databases {
			if !chExemptDB(d) {
				tg.Dbs = append(tg.Dbs, d)
			}
		}
	}
	for _, t := range ck.Tables {
		if chExemptDB(t.Schema) {
			continue
		}
		tg.Objects = append(tg.Objects, sqlx.TableRef{Schema: t.Schema, Name: t.Name})
	}
	return tg, ck.Stmt, nil
}

func classifyCH(err error) (string, string) {
	var ue *clickhouse.UserError
	switch {
	case clickhouse.IsTimeout(err):
		return "timeout", ""
	case clickhouse.IsCanceled(err) || errors.Is(err, context.Canceled):
		return "canceled", ""
	case errors.As(err, &ue):
		return "bad_query", ue.Msg
	}
	return "", ""
}

func (s *Service) runCH(ctx context.Context, e engine.Engine, mp *multiPrep, limit int, timeout time.Duration) (*multiOut, error) {
	r, ok := e.(chRunner)
	if !ok {
		return nil, errors.New("ClickHouse 引擎不可用")
	}
	sh, err := r.RunUser(ctx, mp.ch.ck, clickhouse.QueryLimits{MaxRows: limit, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	return &multiOut{Columns: sh.ColumnList, Types: sh.ColumnTypes, Rows: sh.Rows, Truncated: sh.Truncated, TruncatedBy: sh.TruncatedBy,
		Warnings: sh.Warnings, Notices: sh.Notices, Executed: mp.ch.ck.Stmt, StmtKind: mp.stmtKind}, nil
}

// ---- 元数据 ----

func (s *Service) chTables(ctx context.Context, e engine.Engine, db string) ([]TableMeta, error) {
	m, ok := e.(chMeta)
	if !ok {
		return nil, errors.New("ClickHouse 引擎不可用")
	}
	infos, err := m.TableInfos(ctx, db)
	if err != nil {
		return s.namesOnly(ctx, e, db)
	}
	out := make([]TableMeta, 0, len(infos))
	for _, i := range infos {
		t := TableMeta{Name: i.Name, Rows: -1, Comment: i.Comment, Kind: "table"}
		switch i.Engine {
		case "View", "LiveView", "WindowView":
			t.View, t.Kind = true, "view"
		case "MaterializedView":
			t.View, t.Kind = true, "matview"
		case "Dictionary":
			t.Kind = "dictionary"
		}
		if i.Rows != nil && !t.View {
			t.Rows = *i.Rows
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *Service) chColumns(ctx context.Context, e engine.Engine, db, table string) ([]ColumnMeta, error) {
	m, ok := e.(chMeta)
	if !ok {
		return nil, errors.New("ClickHouse 引擎不可用")
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

// chLit 是 ClickHouse 的字符串字面量（反斜杠与单引号都要转义）。
func chLit(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(s, `\`, `\\`)
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

func (s *Service) chSearch(ctx context.Context, e engine.Engine, db, kw string) ([]ColumnHit, error) {
	lit := chLit(kw)
	q := "SELECT `table`, `name`, `comment` FROM system.columns WHERE `database` = " + chLit(db) +
		" AND (positionCaseInsensitive(`name`, " + lit + ") > 0 OR positionCaseInsensitive(`comment`, " + lit + ") > 0) ORDER BY `table`, `position` LIMIT 500"
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
