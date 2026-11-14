// @author: kevinkelly
// @license: Apache Licence
// @file: oradm.go
// @time: 2026/10/28

package dict

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"dbtool/internal/engine"
	"dbtool/internal/engine/dm"
	"dbtool/internal/engine/oracle"
)

// Oracle / 达梦的数据字典：同 PostgreSQL / ClickHouse，直接用各引擎包导出的元数据 API。两个引擎各自独立，下面两套适配代码刻意分开写。
// 第一期只有“表”和“视图”（物化视图归在视图里）；脱敏字段标记与其它类型共用同一套配置。

const (
	typeOracle = "oracle"
	typeDM     = "dm"
)

// objTypeLabel 把引擎的对象种类（table / view / matview）转成展示用的类型名。
func objTypeLabel(kind string) string {
	switch kind {
	case "view":
		return "VIEW"
	case "matview":
		return "MATERIALIZED VIEW"
	}
	return "TABLE"
}

func rowsMeta(meta []Pair, rows *int64) []Pair {
	if rows == nil {
		return meta
	}
	return addPair(meta, "行数（估算）", strconv.FormatInt(*rows, 10))
}

// ---- Oracle ----

type oraDict interface {
	AllDatabases(ctx context.Context) ([]string, error)
	TableInfos(ctx context.Context, db string) ([]oracle.TableInfo, error)
	ColumnInfos(ctx context.Context, db, table string) ([]oracle.ColumnInfo, error)
	Indexes(ctx context.Context, db, table string) ([]oracle.IndexInfo, error)
	CreateStatement(ctx context.Context, db, name string, view bool) (string, error)
}

func oraOf(e engine.Engine) (oraDict, error) {
	m, ok := e.(oraDict)
	if !ok {
		return nil, errors.New("Oracle 引擎不可用")
	}
	return m, nil
}

func newOracleDialect() engineDialect {
	return engineDialect{
		typ:      typeOracle,
		notes:    []string{"“库”是 schema（owner）。行数是统计信息里的估算（ALL_TABLES.NUM_ROWS），没收集过统计信息的表不显示；Oracle 不在这里提供创建 / 修改时间。建表语句来自 DBMS_METADATA（读不到时是按列信息拼出的近似 DDL）。物化视图归在“视图”里。"},
		systemDB: oracle.IsSystemSchema,
		dbs: func(ctx context.Context, e engine.Engine) ([]string, error) {
			m, err := oraOf(e)
			if err != nil {
				return nil, err
			}
			return m.AllDatabases(ctx)
		},
		list: func(ctx context.Context, e engine.Engine, db string) ([]objInfo, error) {
			m, err := oraOf(e)
			if err != nil {
				return nil, err
			}
			infos, err := m.TableInfos(ctx, db)
			if err != nil {
				return nil, err
			}
			out := make([]objInfo, 0, len(infos))
			for _, i := range infos {
				o := objInfo{Name: i.Name, Comment: i.Comment, Type: objTypeLabel(i.Kind), IsView: i.IsView, Rows: i.Rows}
				o.Meta = addPair(o.Meta, "类型", o.Type)
				o.Meta = rowsMeta(o.Meta, i.Rows)
				out = append(out, o)
			}
			return out, nil
		},
		columns: func(ctx context.Context, e engine.Engine, db string, o objInfo) ([][]string, error) {
			m, err := oraOf(e)
			if err != nil {
				return nil, err
			}
			cols, err := m.ColumnInfos(ctx, db, o.Name)
			if err != nil {
				return nil, err
			}
			rows := make([][]string, 0, len(cols))
			for _, c := range cols {
				key := ""
				if c.IsPrimaryKey {
					key = "PRI"
				}
				rows = append(rows, []string{c.Name, c.Type, yn(c.Nullable), "", key, "", c.Comment})
			}
			return rows, nil
		},
		indexes: func(ctx context.Context, e engine.Engine, db string, o objInfo) ([][]string, error) {
			m, err := oraOf(e)
			if err != nil {
				return nil, err
			}
			idx, err := m.Indexes(ctx, db, o.Name)
			if err != nil {
				return nil, err
			}
			rows := make([][]string, 0, len(idx))
			for _, i := range idx {
				rows = append(rows, []string{i.Name, strings.Join(i.Columns, ", "), yn(i.Unique), ""})
			}
			return rows, nil
		},
		ddl: func(ctx context.Context, e engine.Engine, db string, o objInfo) (string, error) {
			m, err := oraOf(e)
			if err != nil {
				return "", err
			}
			return m.CreateStatement(ctx, db, o.Name, o.IsView)
		},
	}
}

// ---- 达梦 ----

type dmDict interface {
	AllDatabases(ctx context.Context) ([]string, error)
	TableInfos(ctx context.Context, db string) ([]dm.TableInfo, error)
	ColumnInfos(ctx context.Context, db, table string) ([]dm.ColumnInfo, error)
	Indexes(ctx context.Context, db, table string) ([]dm.IndexInfo, error)
	CreateStatement(ctx context.Context, db, name string, view bool) (string, error)
}

func dmOf(e engine.Engine) (dmDict, error) {
	m, ok := e.(dmDict)
	if !ok {
		return nil, errors.New("达梦引擎不可用")
	}
	return m, nil
}

func newDMDialect() engineDialect {
	return engineDialect{
		typ:      typeDM,
		notes:    []string{"“库”是 schema。行数是统计信息里的估算（ALL_TABLES.NUM_ROWS），没收集过统计信息的表不显示。建表语句来自 DBMS_METADATA / SP_TABLEDEF（读不到时是按列信息拼出的近似 DDL）。物化视图归在“视图”里。"},
		systemDB: dm.IsSystemSchema,
		dbs: func(ctx context.Context, e engine.Engine) ([]string, error) {
			m, err := dmOf(e)
			if err != nil {
				return nil, err
			}
			return m.AllDatabases(ctx)
		},
		list: func(ctx context.Context, e engine.Engine, db string) ([]objInfo, error) {
			m, err := dmOf(e)
			if err != nil {
				return nil, err
			}
			infos, err := m.TableInfos(ctx, db)
			if err != nil {
				return nil, err
			}
			out := make([]objInfo, 0, len(infos))
			for _, i := range infos {
				o := objInfo{Name: i.Name, Comment: i.Comment, Type: objTypeLabel(i.Kind), IsView: i.IsView, Rows: i.Rows}
				o.Meta = addPair(o.Meta, "类型", o.Type)
				o.Meta = rowsMeta(o.Meta, i.Rows)
				out = append(out, o)
			}
			return out, nil
		},
		columns: func(ctx context.Context, e engine.Engine, db string, o objInfo) ([][]string, error) {
			m, err := dmOf(e)
			if err != nil {
				return nil, err
			}
			cols, err := m.ColumnInfos(ctx, db, o.Name)
			if err != nil {
				return nil, err
			}
			rows := make([][]string, 0, len(cols))
			for _, c := range cols {
				key := ""
				if c.IsPrimaryKey {
					key = "PRI"
				}
				rows = append(rows, []string{c.Name, c.Type, yn(c.Nullable), "", key, "", c.Comment})
			}
			return rows, nil
		},
		indexes: func(ctx context.Context, e engine.Engine, db string, o objInfo) ([][]string, error) {
			m, err := dmOf(e)
			if err != nil {
				return nil, err
			}
			idx, err := m.Indexes(ctx, db, o.Name)
			if err != nil {
				return nil, err
			}
			rows := make([][]string, 0, len(idx))
			for _, i := range idx {
				rows = append(rows, []string{i.Name, strings.Join(i.Columns, ", "), yn(i.Unique), ""})
			}
			return rows, nil
		},
		ddl: func(ctx context.Context, e engine.Engine, db string, o objInfo) (string, error) {
			m, err := dmOf(e)
			if err != nil {
				return "", err
			}
			return m.CreateStatement(ctx, db, o.Name, o.IsView)
		},
	}
}
