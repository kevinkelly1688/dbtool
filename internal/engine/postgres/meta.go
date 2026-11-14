// @author: kevinkelly
// @license: Apache Licence
// @file: meta.go
// @time: 2026/10/28

package postgres

import (
	"context"
	"encoding/json"
	"strconv"
	"time"
)

// 数据字典用的元数据：全部来自 pg_catalog，只读事务里查询。需要 PostgreSQL 10 及以上。

// TableInfo 是一个表 / 视图的概要。PostgreSQL 不记录创建 / 修改时间，CreateTime / UpdateTime 恒为 nil。
type TableInfo struct {
	Schema       string     `json:"schema"`
	Name         string     `json:"name"`      // 不含 schema
	FullName     string     `json:"full_name"` // schema.table
	Type         string     `json:"type"`      // BASE TABLE / PARTITIONED TABLE / VIEW / MATERIALIZED VIEW / FOREIGN TABLE
	Engine       string     `json:"engine"`    // 访问方法（heap …）；视图等为空
	Comment      string     `json:"comment"`
	Owner        string     `json:"owner"`
	RowsEstimate int64      `json:"rows_estimate"` // pg_class.reltuples（估算）；-1 表示还没有统计信息
	DataBytes    int64      `json:"data_bytes"`    // 表本体（含 TOAST、FSM、VM）
	IndexBytes   int64      `json:"index_bytes"`
	CreateTime   *time.Time `json:"create_time"`
	UpdateTime   *time.Time `json:"update_time"`
}

// ColumnInfo 是一列的定义。
type ColumnInfo struct {
	Position     int     `json:"position"`
	Name         string  `json:"name"`
	Type         string  `json:"type"` // format_type 的结果，如 character varying(64)、numeric(10,2)、timestamp with time zone
	Nullable     bool    `json:"nullable"`
	Default      *string `json:"default"` // 默认值 / 生成表达式；无默认值为 nil
	Comment      string  `json:"comment"`
	IsPrimaryKey bool    `json:"is_primary_key"`
	PKOrdinal    int     `json:"pk_ordinal"` // 在主键里的次序（从 1 起），非主键列为 0
	Identity     string  `json:"identity"`   // always / by default / ""
	Generated    string  `json:"generated"`  // stored / ""
}

// IndexInfo 是一个索引。
type IndexInfo struct {
	Name       string   `json:"name"`
	Columns    []string `json:"columns"` // 键列（表达式索引为表达式文本）
	Unique     bool     `json:"unique"`
	Primary    bool     `json:"primary"`
	Valid      bool     `json:"valid"`
	Method     string   `json:"method"` // btree / hash / gin / gist / brin …
	Definition string   `json:"definition"`
	SizeBytes  int64    `json:"size_bytes"`
}

const relKinds = "('r', 'p', 'v', 'm', 'f')"

func atoi64(s *string) int64 {
	if s == nil {
		return 0
	}
	n, _ := strconv.ParseInt(*s, 10, 64)
	return n
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func relType(kind string) string {
	switch kind {
	case "r":
		return "BASE TABLE"
	case "p":
		return "PARTITIONED TABLE"
	case "v":
		return "VIEW"
	case "m":
		return "MATERIALIZED VIEW"
	case "f":
		return "FOREIGN TABLE"
	}
	return kind
}

// TableInfos 返回库内账号有查询权限的表 / 视图概要（与 AllTables 同样的过滤：不含系统 schema 与分区子表）。
func (e *Engine) TableInfos(ctx context.Context, db string) ([]TableInfo, error) {
	var out []TableInfo
	err := e.readTx(ctx, e.dbOrDefault(db), metaTimeout, nil, func(s *session) error {
		rows, err := s.rows(ctx, `SELECT n.nspname, c.relname, c.relkind::text, COALESCE(am.amname, ''), COALESCE(d.description, ''),
       pg_catalog.pg_get_userbyid(c.relowner), c.reltuples::bigint::text,
       CASE WHEN c.relkind IN ('r', 'm') THEN pg_catalog.pg_table_size(c.oid) ELSE 0 END::text,
       CASE WHEN c.relkind IN ('r', 'm', 'p') THEN pg_catalog.pg_indexes_size(c.oid) ELSE 0 END::text
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_am am ON am.oid = c.relam AND c.relkind IN ('r', 'm')
LEFT JOIN pg_catalog.pg_description d ON d.objoid = c.oid AND d.classoid = 'pg_catalog.pg_class'::regclass AND d.objsubid = 0
WHERE c.relkind IN `+relKinds+` AND NOT c.relispartition
  AND n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
  AND pg_catalog.has_any_column_privilege(c.oid, 'SELECT')
ORDER BY n.nspname, c.relname`)
		if err != nil {
			return err
		}
		out = make([]TableInfo, 0, len(rows))
		for _, r := range rows {
			if len(r) < 9 {
				continue
			}
			ti := TableInfo{Schema: str(r[0]), Name: str(r[1]), Type: relType(str(r[2])), Engine: str(r[3]), Comment: str(r[4]), Owner: str(r[5]),
				RowsEstimate: atoi64(r[6]), DataBytes: atoi64(r[7]), IndexBytes: atoi64(r[8])}
			ti.FullName = ti.Schema + "." + ti.Name
			out = append(out, ti)
		}
		return nil
	})
	return out, err
}

// ColumnInfos 返回表的列定义。table 写成 schema.table（不带点按 public）。
func (e *Engine) ColumnInfos(ctx context.Context, db, table string) ([]ColumnInfo, error) {
	schema, name := splitTable(table)
	var out []ColumnInfo
	err := e.readTx(ctx, e.dbOrDefault(db), metaTimeout, nil, func(s *session) error {
		rows, err := s.rows(ctx, `SELECT a.attnum::text, a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), (NOT a.attnotnull)::text,
       pg_catalog.pg_get_expr(d.adbin, d.adrelid), COALESCE(pg_catalog.col_description(a.attrelid, a.attnum), ''),
       COALESCE((SELECT k.ord FROM pg_catalog.pg_index i, LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
                 WHERE i.indrelid = a.attrelid AND i.indisprimary AND k.attnum = a.attnum), 0)::text,
       COALESCE(pg_catalog.to_jsonb(a) ->> 'attidentity', ''), COALESCE(pg_catalog.to_jsonb(a) ->> 'attgenerated', '')
FROM pg_catalog.pg_attribute a
JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN `+relKinds+` AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`, schema, name)
		if err != nil {
			return err
		}
		out = make([]ColumnInfo, 0, len(rows))
		for _, r := range rows {
			if len(r) < 9 {
				continue
			}
			ci := ColumnInfo{Position: int(atoi64(r[0])), Name: str(r[1]), Type: str(r[2]), Nullable: str(r[3]) == "true",
				Default: r[4], Comment: str(r[5]), PKOrdinal: int(atoi64(r[6]))}
			ci.IsPrimaryKey = ci.PKOrdinal > 0
			switch str(r[7]) {
			case "a":
				ci.Identity = "always"
			case "d":
				ci.Identity = "by default"
			}
			if str(r[8]) == "s" {
				ci.Generated = "stored"
			}
			out = append(out, ci)
		}
		return nil
	})
	return out, err
}

// Indexes 返回表上的索引。
func (e *Engine) Indexes(ctx context.Context, db, table string) ([]IndexInfo, error) {
	schema, name := splitTable(table)
	var out []IndexInfo
	err := e.readTx(ctx, e.dbOrDefault(db), metaTimeout, nil, func(s *session) error {
		rows, err := s.rows(ctx, `SELECT ic.relname, i.indisunique::text, i.indisprimary::text, i.indisvalid::text, am.amname,
       pg_catalog.pg_get_indexdef(i.indexrelid), pg_catalog.pg_relation_size(i.indexrelid)::text,
       (SELECT pg_catalog.array_to_json(pg_catalog.array_agg(pg_catalog.pg_get_indexdef(i.indexrelid, k, true) ORDER BY k))::text
        FROM pg_catalog.generate_series(1, i.indnatts::int) AS k)
FROM pg_catalog.pg_index i
JOIN pg_catalog.pg_class c ON c.oid = i.indrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
JOIN pg_catalog.pg_am am ON am.oid = ic.relam
WHERE n.nspname = $1 AND c.relname = $2
ORDER BY i.indisprimary DESC, ic.relname`, schema, name)
		if err != nil {
			return err
		}
		out = make([]IndexInfo, 0, len(rows))
		for _, r := range rows {
			if len(r) < 8 {
				continue
			}
			ix := IndexInfo{Name: str(r[0]), Unique: str(r[1]) == "true", Primary: str(r[2]) == "true", Valid: str(r[3]) == "true",
				Method: str(r[4]), Definition: str(r[5]), SizeBytes: atoi64(r[6])}
			if r[7] != nil {
				_ = json.Unmarshal([]byte(*r[7]), &ix.Columns)
			}
			out = append(out, ix)
		}
		return nil
	})
	return out, err
}
