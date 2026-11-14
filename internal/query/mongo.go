// @author: kevinkelly
// @license: Apache Licence
// @file: mongo.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"dbtool/internal/engine"
	mongox "dbtool/internal/engine/mongo"
	"dbtool/internal/sqlx"
)

// MongoDB 的在线查询接线。引擎见 internal/engine/mongo。
//
//   - “库”= database，“表”= collection；库级与表级权限都支持（dbt_query_privileges，tb_name 存集合名）；
//   - 语句涉及的集合来自 mongox.CheckUserQuery 的 Checked.Collections（含 $lookup / $graphLookup / $unionWith 的目标）；
//     db.getCollectionNames() / db.stats()（DBLevel）要求库级权限；
//   - 检查分两次：先不带行数上限检查以取得集合并授权，授权得出行数上限后再带上限检查一次（缺省 / 超限的 limit 被收窄并给出提示）。

type mongoRunner interface {
	RunUser(ctx context.Context, db string, c *mongox.Checked, lim mongox.QueryLimits) (*mongox.Shaped, error)
}

type mongoMeta interface {
	TableInfos(ctx context.Context, db string) ([]mongox.TableInfo, error)
	ColumnInfos(ctx context.Context, db, table string) ([]mongox.ColumnInfo, error)
}

var (
	_ mongoRunner = (*mongox.Engine)(nil)
	_ mongoMeta   = (*mongox.Engine)(nil)
)

type mongoPrep struct{ ck *mongox.Checked }

func mongoBadQuery(err error) error {
	var ue *mongox.UserError
	if errors.As(err, &ue) {
		return newErr(400, "bad_query", ue.Msg)
	}
	return newErr(400, "bad_query", err.Error())
}

func (s *Service) checkMongo(text, db string, mp *multiPrep) (Targets, string, error) {
	if err := mongox.ValidDBName(db); err != nil {
		return Targets{}, "", mongoBadQuery(err)
	}
	ck, err := mongox.CheckUserQuery(text)
	if err != nil {
		return Targets{}, "", mongoBadQuery(err)
	}
	mp.mg, mp.stmtKind = &mongoPrep{ck: ck}, string(ck.Kind)
	tg := Targets{DB: db}
	if ck.DBLevel {
		tg.RequireDB = fmt.Sprintf("该语句查看整个库的信息，需要库 %s 的库级查询权限；请申请库级权限，或改查具体集合。", db)
	}
	for _, c := range ck.Collections {
		tg.Objects = append(tg.Objects, sqlx.TableRef{Schema: db, Name: c})
	}
	return tg, ck.Statement, nil
}

// recheckMongo 带上行数上限重新检查（授权通过之后）。
func (s *Service) recheckMongo(text, db string, limit int, mp *multiPrep) error {
	ck, err := mongox.CheckUserQuery(text, mongox.CheckOptions{MaxRows: limit})
	if err != nil {
		return mongoBadQuery(err)
	}
	mp.mg = &mongoPrep{ck: ck}
	return nil
}

func classifyMongo(err error) (string, string) {
	var ue *mongox.UserError
	switch {
	case errors.Is(err, mongox.ErrTimeout):
		return "timeout", ""
	case errors.Is(err, context.Canceled):
		return "canceled", ""
	case errors.As(err, &ue):
		return "bad_query", ue.Msg
	}
	return "", ""
}

func (s *Service) runMongo(ctx context.Context, e engine.Engine, mp *multiPrep, limit int, timeout time.Duration, cfg *maskConfig) (*multiOut, error) {
	r, ok := e.(mongoRunner)
	if !ok {
		return nil, errors.New("MongoDB 引擎不可用")
	}
	lim := mongox.QueryLimits{MaxRows: limit, Timeout: timeout}
	var mk *mongoMasker
	var viewErr error
	if !cfg.empty() {
		colls := mp.mg.ck.Collections
		if len(colls) == 0 && mp.mg.ck.Collection != "" {
			colls = []string{mp.mg.ck.Collection}
		}
		pipeline := mp.mg.ck.Pipeline
		// 视图：配置写在源集合的字段路径上时，视图的管道可以把字段改名，要把视图的源集合和管道也算进来一起追踪（见 mongoviews.go）
		vx, verr := s.expandMongoViews(ctx, e, mp.db, colls)
		if verr != nil {
			viewErr = verr
		}
		if vx != nil {
			colls = append(append([]string(nil), colls...), vx.collections...)
			pipeline = append(append([]bson.D(nil), vx.pipeline...), pipeline...)
		}
		if mk = newMongoMasker(cfg.cols, cfg.rules, mp.db, colls, pipeline); mk != nil {
			lim.Mask = mk // 只在有适用配置时设置：不能把 nil 指针放进接口
		}
	}
	sh, err := r.RunUser(ctx, mp.db, mp.mg.ck, lim)
	if err != nil {
		return nil, err
	}
	out := &multiOut{Columns: sh.Columns, Rows: sh.Rows, Truncated: sh.Truncated, Warnings: sh.Warnings, Executed: sh.Statement, StmtKind: mp.stmtKind, ViewErr: viewErr}
	if mk != nil {
		out.MaskedCols = mk.Columns()
	}
	if sh.Truncated {
		out.TruncatedBy = "bytes"
		if limit > 0 && len(sh.Rows) >= limit {
			out.TruncatedBy = "rows"
		}
	}
	if out.Executed == "" {
		out.Executed = mp.mg.ck.Statement
	}
	return out, nil
}

// ---- 元数据 ----

func (s *Service) mongoTables(ctx context.Context, e engine.Engine, db string) ([]TableMeta, error) {
	m, ok := e.(mongoMeta)
	if !ok {
		return nil, errors.New("MongoDB 引擎不可用")
	}
	infos, err := m.TableInfos(ctx, db)
	if err != nil {
		return s.namesOnly(ctx, e, db)
	}
	out := make([]TableMeta, 0, len(infos))
	for _, i := range infos {
		t := TableMeta{Name: i.Name, Rows: -1, Comment: i.Comment, Kind: "collection"}
		switch i.Type {
		case "view":
			t.View, t.Kind = true, "view"
		case "timeseries":
			t.Kind = "timeseries"
		}
		if i.HasStats && !t.View {
			t.Rows = i.RowsEstimate
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *Service) mongoColumns(ctx context.Context, e engine.Engine, db, table string) ([]ColumnMeta, error) {
	m, ok := e.(mongoMeta)
	if !ok {
		return nil, errors.New("MongoDB 引擎不可用")
	}
	cols, err := m.ColumnInfos(ctx, db, table)
	if err != nil {
		return nil, err
	}
	out := make([]ColumnMeta, 0, len(cols))
	for _, c := range cols {
		cm := ColumnMeta{Name: c.Name, Type: c.Type, Nullable: c.Nullable, Comment: c.Comment}
		if c.IsPK {
			cm.Key = "PRI"
		}
		out = append(out, cm)
	}
	return out, nil
}
