// @author: kevinkelly
// @license: Apache Licence
// @file: engines.go
// @time: 2026/10/28

package dict

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/engine/clickhouse"
	mongox "dbtool/internal/engine/mongo"
	"dbtool/internal/engine/postgres"
)

// PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦的数据字典：不走 SQL 查询，直接用各引擎包导出的元数据 API
// （TableInfos / ColumnInfos / Indexes / CreateStatement，见 docs/engine-*.md）。
// 第一期只有“表”和“视图”两类对象；脱敏字段标记与 MySQL 系共用同一套配置。

const (
	typePG    = "pgsql"
	typeMongo = "mongo"
	typeCH    = "clickhouse"
)

// engineDialectType 判断该类型的数据字典走引擎元数据 API。
func engineDialectType(dbType string) bool {
	return dbType == typePG || dbType == typeMongo || dbType == typeCH || dbType == typeOracle || dbType == typeDM
}

// engineQuerier 是 Opener 为这几类实例返回的“连接”：持有引擎实例，不支持 SQL 查询。
type engineQuerier struct{ eng engine.Engine }

func (q *engineQuerier) Query(context.Context, string, ...any) (*Rows, error) {
	return nil, errors.New("该类型的元数据不通过 SQL 查询读取")
}

func (q *engineQuerier) Close() error {
	if q.eng == nil {
		return nil
	}
	return q.eng.Close()
}

// NewEngineQuerier 用已有的引擎实例构造 Querier（测试用假引擎、或调用方自行打开引擎时使用）。
func NewEngineQuerier(e engine.Engine) Querier { return &engineQuerier{eng: e} }

func openEngine(dbType string, c engine.Conn) (Querier, error) {
	e, err := engine.New(dbType, c)
	if err != nil {
		return nil, err
	}
	return &engineQuerier{eng: e}, nil
}

// objInfo 是一个表 / 视图 / 集合的概要（各引擎统一后的形态）。
type objInfo struct {
	Schema      string
	Name        string
	Comment     string
	Type        string
	IsView      bool
	Partitioned bool
	Rows        *int64
	Size        *int64
	Meta        []Pair
}

// engineDialect 是这几类引擎共用的 dialect 实现，差异全部在函数字段里。
type engineDialect struct {
	typ      string
	notes    []string
	systemDB func(string) bool
	dbs      func(ctx context.Context, e engine.Engine) ([]string, error)
	list     func(ctx context.Context, e engine.Engine, db string) ([]objInfo, error)
	columns  func(ctx context.Context, e engine.Engine, db string, o objInfo) ([][]string, error)
	indexes  func(ctx context.Context, e engine.Engine, db string, o objInfo) ([][]string, error)
	ddl      func(ctx context.Context, e engine.Engine, db string, o objInfo) (string, error)
}

func (d engineDialect) Kinds() []Kind            { return []Kind{KindTable, KindView} }
func (d engineDialect) IsSystemDB(n string) bool { return d.systemDB(n) }

// Notes 返回这类对象在列表 / 详情里需要附带的说明（行数是估算、字段是采样等）。
func (d engineDialect) Notes() []string { return append([]string(nil), d.notes...) }

func engineOf(q Querier) (engine.Engine, error) {
	eq, ok := q.(*engineQuerier)
	if !ok || eq.eng == nil {
		return nil, errors.New("引擎连接不可用")
	}
	return eq.eng, nil
}

func (d engineDialect) Databases(ctx context.Context, q Querier) ([]string, error) {
	e, err := engineOf(q)
	if err != nil {
		return nil, err
	}
	all, err := d.dbs(ctx, e)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(all))
	for _, n := range all {
		if !d.systemDB(n) {
			out = append(out, n)
		}
	}
	return out, nil
}

func (d engineDialect) List(ctx context.Context, q Querier, db string, kind Kind) ([]Item, error) {
	if kind != KindTable && kind != KindView {
		return nil, newErr(501, "unsupported_kind", "该类型（%s）的实例暂不支持查看%s", d.typ, kindLabel(kind))
	}
	e, err := engineOf(q)
	if err != nil {
		return nil, err
	}
	infos, err := d.list(ctx, e, db)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(infos))
	for _, o := range infos {
		if o.IsView != (kind == KindView) {
			continue
		}
		out = append(out, Item{Schema: o.Schema, Name: o.Name, Comment: o.Comment, RowsEst: o.Rows, SizeBytes: o.Size, Partitioned: o.Partitioned, Type: o.Type})
	}
	return out, nil
}

func (d engineDialect) find(ctx context.Context, e engine.Engine, db string, kind Kind, schema, name string) (objInfo, error) {
	infos, err := d.list(ctx, e, db)
	if err != nil {
		return objInfo{}, err
	}
	if d.typ == typePG && schema == "" {
		schema = "public"
	}
	for _, o := range infos {
		if o.Name == name && o.Schema == schema && o.IsView == (kind == KindView) {
			return o, nil
		}
	}
	shown := name
	if schema != "" {
		shown = schema + "." + name
	}
	return objInfo{}, notFound(kind, shown)
}

func (d engineDialect) Detail(ctx context.Context, q Querier, db string, kind Kind, schema, name string) (*Detail, error) {
	if kind != KindTable && kind != KindView {
		return nil, newErr(501, "unsupported_kind", "该类型（%s）的实例暂不支持查看%s", d.typ, kindLabel(kind))
	}
	e, err := engineOf(q)
	if err != nil {
		return nil, err
	}
	o, err := d.find(ctx, e, db, kind, schema, name)
	if err != nil {
		return nil, err
	}
	det := &Detail{Kind: kind, Schema: o.Schema, Name: o.Name, Notes: d.Notes()}
	pairs := append([]Pair(nil), o.Meta...)
	if o.Comment != "" {
		pairs = append(pairs, Pair{"注释", o.Comment})
	}
	det.Sections = append(det.Sections, kvSec("基本信息", pairs))
	cols, err := d.columns(ctx, e, db, o)
	if err != nil {
		return nil, err
	}
	det.Sections = append(det.Sections, Section{Title: "列信息", Type: "table", Columns: columnHeads, Rows: cols})
	if idx, err := d.indexes(ctx, e, db, o); err != nil {
		det.Sections = append(det.Sections, noteSec("索引信息", fmt.Sprintf("索引信息获取失败：%s", err.Error())))
	} else {
		det.Sections = append(det.Sections, Section{Title: "索引信息", Type: "table", Columns: indexHeads, Rows: idx})
	}
	if text, err := d.ddl(ctx, e, db, o); err != nil {
		det.Sections = append(det.Sections, noteSec("建表语句", fmt.Sprintf("获取失败：%s", err.Error())))
	} else {
		det.Sections = append(det.Sections, codeSec("建表语句", text))
	}
	return det, nil
}

// ---- 小工具 ----

func i64p(n int64) *int64 { return &n }

func addPair(ps []Pair, k, v string) []Pair {
	if strings.TrimSpace(v) == "" {
		return ps
	}
	return append(ps, Pair{k, v})
}

func yn(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

// ---- PostgreSQL ----

type pgDict interface {
	AllDatabases(ctx context.Context) ([]string, error)
	TableInfos(ctx context.Context, db string) ([]postgres.TableInfo, error)
	ColumnInfos(ctx context.Context, db, table string) ([]postgres.ColumnInfo, error)
	Indexes(ctx context.Context, db, table string) ([]postgres.IndexInfo, error)
	CreateStatement(ctx context.Context, db, table string) (string, error)
}

func pgOf(e engine.Engine) (pgDict, error) {
	m, ok := e.(pgDict)
	if !ok {
		return nil, errors.New("PostgreSQL 引擎不可用")
	}
	return m, nil
}

func newPGDialect() engineDialect {
	return engineDialect{
		typ:      typePG,
		notes:    []string{"行数是 PostgreSQL 的统计估算（pg_class.reltuples），尚未统计过的表不显示；PostgreSQL 不记录创建 / 修改时间，因此没有这两项。建表语句是根据系统目录拼出的近似 DDL。"},
		systemDB: func(n string) bool { l := strings.ToLower(n); return l == "template0" || l == "template1" },
		dbs:      func(ctx context.Context, e engine.Engine) ([]string, error) { return e.AllDatabases(ctx) },
		list: func(ctx context.Context, e engine.Engine, db string) ([]objInfo, error) {
			m, err := pgOf(e)
			if err != nil {
				return nil, err
			}
			infos, err := m.TableInfos(ctx, db)
			if err != nil {
				return nil, err
			}
			out := make([]objInfo, 0, len(infos))
			for _, i := range infos {
				o := objInfo{Schema: i.Schema, Name: i.Name, Comment: i.Comment, Type: i.Type,
					IsView: i.Type == "VIEW" || i.Type == "MATERIALIZED VIEW", Partitioned: i.Type == "PARTITIONED TABLE"}
				o.Meta = addPair(o.Meta, "类型", i.Type)
				o.Meta = addPair(o.Meta, "属主", i.Owner)
				o.Meta = addPair(o.Meta, "访问方法", i.Engine)
				if i.RowsEstimate >= 0 && !o.IsView {
					o.Rows = i64p(i.RowsEstimate)
					o.Meta = addPair(o.Meta, "行数（估算）", strconv.FormatInt(i.RowsEstimate, 10))
				}
				if i.DataBytes > 0 || i.IndexBytes > 0 {
					o.Size = i64p(i.DataBytes + i.IndexBytes)
					o.Meta = addPair(o.Meta, "数据大小(KB)", kb(i.DataBytes))
					o.Meta = addPair(o.Meta, "索引大小(KB)", kb(i.IndexBytes))
				}
				out = append(out, o)
			}
			return out, nil
		},
		columns: func(ctx context.Context, e engine.Engine, db string, o objInfo) ([][]string, error) {
			m, err := pgOf(e)
			if err != nil {
				return nil, err
			}
			cols, err := m.ColumnInfos(ctx, db, o.Schema+"."+o.Name)
			if err != nil {
				return nil, err
			}
			rows := make([][]string, 0, len(cols))
			for _, c := range cols {
				def := ""
				if c.Default != nil {
					def = *c.Default
				}
				key := ""
				if c.IsPrimaryKey {
					key = "PRI"
				}
				var extra []string
				if c.Identity != "" {
					extra = append(extra, "identity "+c.Identity)
				}
				if c.Generated != "" {
					extra = append(extra, "generated "+c.Generated)
				}
				rows = append(rows, []string{c.Name, c.Type, yn(c.Nullable), def, key, strings.Join(extra, "，"), c.Comment})
			}
			return rows, nil
		},
		indexes: func(ctx context.Context, e engine.Engine, db string, o objInfo) ([][]string, error) {
			m, err := pgOf(e)
			if err != nil {
				return nil, err
			}
			idx, err := m.Indexes(ctx, db, o.Schema+"."+o.Name)
			if err != nil {
				return nil, err
			}
			rows := make([][]string, 0, len(idx))
			for _, i := range idx {
				typ := i.Method
				if i.Primary {
					typ += "（主键）"
				}
				if !i.Valid {
					typ += "（无效）"
				}
				rows = append(rows, []string{i.Name, strings.Join(i.Columns, ", "), yn(i.Unique), typ})
			}
			return rows, nil
		},
		ddl: func(ctx context.Context, e engine.Engine, db string, o objInfo) (string, error) {
			m, err := pgOf(e)
			if err != nil {
				return "", err
			}
			return m.CreateStatement(ctx, db, o.Schema+"."+o.Name)
		},
	}
}

// ---- MongoDB ----

type mongoDict interface {
	AllDatabases(ctx context.Context) ([]string, error)
	TableInfos(ctx context.Context, db string) ([]mongox.TableInfo, error)
	ColumnInfos(ctx context.Context, db, table string) ([]mongox.ColumnInfo, error)
	Indexes(ctx context.Context, db, table string) ([]mongox.IndexInfo, error)
	CreateStatement(ctx context.Context, db, table string) (string, error)
}

func mongoOf(e engine.Engine) (mongoDict, error) {
	m, ok := e.(mongoDict)
	if !ok {
		return nil, errors.New("MongoDB 引擎不可用")
	}
	return m, nil
}

func newMongoDialect() engineDialect {
	return engineDialect{
		typ:      typeMongo,
		notes:    []string{"MongoDB 没有固定 schema：列信息是对集合采样前 100 个文档推断的字段，没出现在样本里的字段不会列出；文档数是元数据计数，非精确。没有表注释，也不记录创建 / 修改时间。"},
		systemDB: func(n string) bool { l := strings.ToLower(n); return l == "admin" || l == "local" || l == "config" },
		dbs:      func(ctx context.Context, e engine.Engine) ([]string, error) { return e.AllDatabases(ctx) },
		list: func(ctx context.Context, e engine.Engine, db string) ([]objInfo, error) {
			m, err := mongoOf(e)
			if err != nil {
				return nil, err
			}
			infos, err := m.TableInfos(ctx, db)
			if err != nil {
				return nil, err
			}
			out := make([]objInfo, 0, len(infos))
			for _, i := range infos {
				o := objInfo{Name: i.Name, Comment: i.Comment, Type: i.Type, IsView: i.Type == "view"}
				o.Meta = addPair(o.Meta, "类型", i.Type)
				o.Meta = addPair(o.Meta, "集合选项", i.Options)
				if i.HasStats && !o.IsView {
					o.Rows = i64p(i.RowsEstimate)
					o.Size = i64p(i.DataBytes + i.IndexBytes)
					o.Meta = addPair(o.Meta, "文档数（元数据计数）", strconv.FormatInt(i.RowsEstimate, 10))
					o.Meta = addPair(o.Meta, "数据大小(KB)", kb(i.DataBytes))
					o.Meta = addPair(o.Meta, "存储大小(KB)", kb(i.StorageBytes))
					o.Meta = addPair(o.Meta, "索引大小(KB)", kb(i.IndexBytes))
					o.Meta = addPair(o.Meta, "索引数", strconv.Itoa(i.IndexCount))
				}
				out = append(out, o)
			}
			return out, nil
		},
		columns: func(ctx context.Context, e engine.Engine, db string, o objInfo) ([][]string, error) {
			m, err := mongoOf(e)
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
				if c.IsPK {
					key = "PRI"
				}
				def := ""
				if c.Default != nil {
					def = *c.Default
				}
				rows = append(rows, []string{c.Name, c.Type, yn(c.Nullable), def, key, "", c.Comment})
			}
			return rows, nil
		},
		indexes: func(ctx context.Context, e engine.Engine, db string, o objInfo) ([][]string, error) {
			m, err := mongoOf(e)
			if err != nil {
				return nil, err
			}
			idx, err := m.Indexes(ctx, db, o.Name)
			if err != nil {
				return nil, err
			}
			rows := make([][]string, 0, len(idx))
			for _, i := range idx {
				keys := make([]string, len(i.Keys))
				for j, k := range i.Keys {
					keys[j] = k.Field + "(" + k.Order + ")"
				}
				var flags []string
				if i.Sparse {
					flags = append(flags, "稀疏")
				}
				if i.Hidden {
					flags = append(flags, "隐藏")
				}
				if i.TTLSeconds != nil {
					flags = append(flags, fmt.Sprintf("TTL %d 秒", *i.TTLSeconds))
				}
				if i.Partial != "" {
					flags = append(flags, "部分索引")
				}
				typ := strings.Join(flags, "，")
				rows = append(rows, []string{i.Name, strings.Join(keys, ", "), yn(i.Unique), typ})
			}
			return rows, nil
		},
		ddl: func(ctx context.Context, e engine.Engine, db string, o objInfo) (string, error) {
			m, err := mongoOf(e)
			if err != nil {
				return "", err
			}
			return m.CreateStatement(ctx, db, o.Name)
		},
	}
}

// ---- ClickHouse ----

type chDict interface {
	AllDatabases(ctx context.Context) ([]string, error)
	TableInfos(ctx context.Context, db string) ([]clickhouse.TableInfo, error)
	ColumnInfos(ctx context.Context, db, table string) ([]clickhouse.ColumnInfo, error)
	Indexes(ctx context.Context, db, table string) ([]clickhouse.IndexInfo, error)
	CreateStatement(ctx context.Context, db, table string) (string, error)
}

func chOf(e engine.Engine) (chDict, error) {
	m, ok := e.(chDict)
	if !ok {
		return nil, errors.New("ClickHouse 引擎不可用")
	}
	return m, nil
}

func newCHDialect() engineDialect {
	return engineDialect{
		typ:      typeCH,
		notes:    []string{"ClickHouse 不记录表的创建时间；“最近一次 DDL”取自元数据文件的修改时间。行数 / 大小对非 MergeTree 引擎可能为空，不显示。字典（Dictionary）归在“表”里。"},
		systemDB: func(n string) bool { l := strings.ToLower(n); return l == "system" || l == "information_schema" },
		dbs:      func(ctx context.Context, e engine.Engine) ([]string, error) { return e.AllDatabases(ctx) },
		list: func(ctx context.Context, e engine.Engine, db string) ([]objInfo, error) {
			m, err := chOf(e)
			if err != nil {
				return nil, err
			}
			infos, err := m.TableInfos(ctx, db)
			if err != nil {
				return nil, err
			}
			out := make([]objInfo, 0, len(infos))
			for _, i := range infos {
				o := objInfo{Name: i.Name, Comment: i.Comment, Type: i.Engine}
				switch i.Engine {
				case "View", "MaterializedView", "LiveView", "WindowView":
					o.IsView = true
				}
				o.Rows = i.Rows
				if i.DataBytes != nil {
					size := *i.DataBytes
					if i.IndexBytes != nil {
						size += *i.IndexBytes
					}
					o.Size = i64p(size)
				}
				o.Meta = addPair(o.Meta, "引擎", i.Engine)
				o.Meta = addPair(o.Meta, "完整引擎定义", i.EngineFull)
				o.Meta = addPair(o.Meta, "分区键", i.PartitionKey)
				o.Meta = addPair(o.Meta, "排序键", i.SortingKey)
				o.Meta = addPair(o.Meta, "主键", i.PrimaryKey)
				o.Meta = addPair(o.Meta, "采样键", i.SamplingKey)
				if i.Rows != nil {
					o.Meta = addPair(o.Meta, "行数", strconv.FormatInt(*i.Rows, 10))
				}
				if i.DataBytes != nil {
					o.Meta = addPair(o.Meta, "数据大小(KB)", kb(*i.DataBytes))
				}
				if i.IndexBytes != nil {
					o.Meta = addPair(o.Meta, "索引大小(KB)", kb(*i.IndexBytes))
				}
				o.Meta = addPair(o.Meta, "最近一次 DDL", fmtTime(i.UpdateTime))
				out = append(out, o)
			}
			return out, nil
		},
		columns: func(ctx context.Context, e engine.Engine, db string, o objInfo) ([][]string, error) {
			m, err := chOf(e)
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
				var extra []string
				if c.DefaultKind != "" {
					extra = append(extra, c.DefaultKind)
				}
				if c.IsPartitionKey {
					extra = append(extra, "分区键")
				}
				if c.IsSortingKey {
					extra = append(extra, "排序键")
				}
				if c.IsSamplingKey {
					extra = append(extra, "采样键")
				}
				if c.Codec != "" {
					extra = append(extra, "CODEC("+c.Codec+")")
				}
				rows = append(rows, []string{c.Name, c.Type, yn(c.Nullable), c.Default, key, strings.Join(extra, "，"), c.Comment})
			}
			return rows, nil
		},
		indexes: func(ctx context.Context, e engine.Engine, db string, o objInfo) ([][]string, error) {
			m, err := chOf(e)
			if err != nil {
				return nil, err
			}
			idx, err := m.Indexes(ctx, db, o.Name)
			if err != nil {
				return nil, err
			}
			rows := make([][]string, 0, len(idx))
			for _, i := range idx {
				cols := strings.Join(i.Columns, ", ")
				if cols == "" {
					cols = i.Expression
				}
				typ := i.Kind
				switch i.Kind {
				case "primary":
					typ = "主键（稀疏索引）"
				case "sorting":
					typ = "排序键"
				case "skipping":
					typ = fmt.Sprintf("跳数索引 %s，GRANULARITY %d", i.Type, i.Granularity)
				}
				rows = append(rows, []string{i.Name, cols, "否", typ})
			}
			return rows, nil
		},
		ddl: func(ctx context.Context, e engine.Engine, db string, o objInfo) (string, error) {
			m, err := chOf(e)
			if err != nil {
				return "", err
			}
			return m.CreateStatement(ctx, db, o.Name)
		},
	}
}
