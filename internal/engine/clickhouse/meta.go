// @author: kevinkelly
// @license: Apache Licence
// @file: meta.go
// @time: 2026/10/28

package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 数据字典用的元数据，来源全部是 system.databases / tables / columns / parts / data_skipping_indices。

// TableInfo 是一张表（或视图、字典）的概要。
type TableInfo struct {
	Name       string
	Comment    string
	Engine     string // 引擎名，如 MergeTree、ReplicatedMergeTree、View、Distributed
	EngineFull string // 带参数的完整引擎定义（可能很长）
	// Rows / DataBytes 取自 system.tables.total_rows / total_bytes：MergeTree 系列是活跃分区片的行数与磁盘字节数（精确值），
	// 其它引擎可能为 nil（取不到）。
	Rows      *int64
	DataBytes *int64
	// IndexBytes 是活跃分区片的 marks_bytes + primary_key_bytes_in_memory（稀疏主键索引的内存 / 标记文件大小）；取不到为 nil。
	IndexBytes *int64
	// CreateTime：ClickHouse 不记录表的创建时间，恒为 nil。
	CreateTime *time.Time
	// UpdateTime 取自 metadata_modification_time（元数据文件的最后修改时间，即最近一次 DDL）。
	UpdateTime   *time.Time
	PartitionKey string
	SortingKey   string
	PrimaryKey   string
	SamplingKey  string
}

// ColumnInfo 是一列的信息。
type ColumnInfo struct {
	Name         string
	Type         string
	Nullable     bool   // 类型是 Nullable(...)（含 LowCardinality(Nullable(...))）
	DefaultKind  string // DEFAULT / MATERIALIZED / ALIAS / EPHEMERAL，空表示没有
	Default      string // 默认值表达式
	Comment      string
	IsPrimaryKey bool
	// KeyOrdinal 是该列在主键（PRIMARY KEY，未显式指定时等于 ORDER BY）表达式里的序号（从 1 起），不在主键里为 0。
	KeyOrdinal     int
	IsSortingKey   bool
	IsPartitionKey bool
	IsSamplingKey  bool
	Codec          string
	Position       int64
}

// IndexInfo 是一条索引：主键（稀疏索引）、排序键或数据跳数索引。
type IndexInfo struct {
	Name        string
	Kind        string // primary / sorting / skipping
	Type        string // 跳数索引的类型：minmax / set(100) / bloom_filter(0.01) ...；primary / sorting 为空
	Expression  string
	Columns     []string // 主键 / 排序键的列（按顺序）；跳数索引为空
	Granularity int64
}

// TableInfos 返回库里所有表的概要（含视图、字典；排除物化视图内部表 .inner*）。
func (e *Engine) TableInfos(ctx context.Context, db string) ([]TableInfo, error) {
	if err := checkName("库", db); err != nil {
		return nil, err
	}
	rs, err := e.meta(ctx, "SELECT name, engine, engine_full, comment, total_rows, total_bytes, toUnixTimestamp(metadata_modification_time) AS mtime, "+
		"partition_key, sorting_key, primary_key, sampling_key FROM system.tables WHERE database = "+sqlString(db)+
		" AND NOT startsWith(name, '.inner') ORDER BY name")
	if err != nil {
		return nil, err
	}
	out := make([]TableInfo, 0, len(rs.rows))
	idx := map[string]int{}
	for _, r := range rs.rows {
		ti := TableInfo{Name: rs.str(r, "name"), Engine: rs.str(r, "engine"), EngineFull: rs.str(r, "engine_full"), Comment: rs.str(r, "comment"),
			PartitionKey: rs.str(r, "partition_key"), SortingKey: rs.str(r, "sorting_key"), PrimaryKey: rs.str(r, "primary_key"), SamplingKey: rs.str(r, "sampling_key")}
		if v, ok := rs.i64(r, "total_rows"); ok {
			ti.Rows = &v
		}
		if v, ok := rs.i64(r, "total_bytes"); ok {
			ti.DataBytes = &v
		}
		if v, ok := rs.i64(r, "mtime"); ok && v > 0 {
			t := time.Unix(v, 0).UTC()
			ti.UpdateTime = &t
		}
		idx[ti.Name] = len(out)
		out = append(out, ti)
	}
	// 索引大小：取不到（没有 system.parts 权限等）时忽略，不影响主体信息。
	if ps, err := e.meta(ctx, "SELECT `table` AS t, sum(marks_bytes) AS m, sum(primary_key_bytes_in_memory) AS p FROM system.parts WHERE database = "+
		sqlString(db)+" AND active GROUP BY `table`"); err == nil {
		for _, r := range ps.rows {
			i, ok := idx[ps.str(r, "t")]
			if !ok {
				continue
			}
			m, _ := ps.i64(r, "m")
			p, _ := ps.i64(r, "p")
			v := m + p
			out[i].IndexBytes = &v
		}
	}
	return out, nil
}

// splitTopLevel 按顶层逗号切分键表达式（括号、引号内的逗号不算）。
func splitTopLevel(s string) []string {
	var out []string
	depth := 0
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '\'' || c == '`' || c == '"':
			quote = c
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case c == ',' && depth == 0:
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	if t := strings.TrimSpace(s[start:]); t != "" {
		out = append(out, t)
	}
	return out
}

// isWholeQuoted 判断 s 整个是一个带引号的标识符（`a b` 或 "a b"）。
func isWholeQuoted(s string) bool {
	if len(s) < 2 || (s[0] != '`' && s[0] != '"') || s[len(s)-1] != s[0] {
		return false
	}
	q := s[0]
	for i := 1; i < len(s)-1; i++ {
		if s[i] == '\\' {
			i++
		} else if s[i] == q {
			if i+1 < len(s)-1 && s[i+1] == q {
				i++
				continue
			}
			return false
		}
	}
	return true
}

func unquoteIdent(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '`' || s[0] == '"') && s[len(s)-1] == s[0] {
		return strings.ReplaceAll(s[1:len(s)-1], string(s[0])+string(s[0]), string(s[0]))
	}
	return s
}

// keyPositions 返回 key 表达式里“恰好是单个列名”的那些列的序号（从 1 起）。表达式（如 toYYYYMM(d)）不计入。
func keyPositions(key string) map[string]int {
	m := map[string]int{}
	for i, part := range splitTopLevel(key) {
		if isWholeQuoted(part) || !strings.ContainsAny(part, "()[] '`\"") {
			m[unquoteIdent(part)] = i + 1
		}
	}
	return m
}

func isNullableType(t string) bool {
	return parseColType(t).nullable
}

// tableKeys 取表的 partition/sorting/primary/sampling 键表达式。
func (e *Engine) tableKeys(ctx context.Context, db, table string) (part, sorting, primary string, err error) {
	rs, err := e.meta(ctx, "SELECT partition_key, sorting_key, primary_key FROM system.tables WHERE database = "+sqlString(db)+" AND name = "+sqlString(table))
	if err != nil {
		return "", "", "", err
	}
	if len(rs.rows) == 0 {
		return "", "", "", fmt.Errorf("表 %s.%s 不存在或无权限查看", db, table)
	}
	r := rs.rows[0]
	return rs.str(r, "partition_key"), rs.str(r, "sorting_key"), rs.str(r, "primary_key"), nil
}

// ColumnInfos 返回表的列信息（按列序）。
func (e *Engine) ColumnInfos(ctx context.Context, db, table string) ([]ColumnInfo, error) {
	if err := checkName("库", db); err != nil {
		return nil, err
	}
	if err := checkName("表", table); err != nil {
		return nil, err
	}
	_, sorting, primary, err := e.tableKeys(ctx, db, table)
	if err != nil {
		return nil, err
	}
	if primary == "" {
		primary = sorting
	}
	pk := keyPositions(primary)
	rs, err := e.meta(ctx, "SELECT name, type, default_kind, default_expression, comment, is_in_partition_key, is_in_sorting_key, is_in_primary_key, "+
		"is_in_sampling_key, compression_codec, position FROM system.columns WHERE database = "+sqlString(db)+" AND table = "+sqlString(table)+" ORDER BY position")
	if err != nil {
		return nil, err
	}
	out := make([]ColumnInfo, 0, len(rs.rows))
	for _, r := range rs.rows {
		c := ColumnInfo{Name: rs.str(r, "name"), Type: rs.str(r, "type"), DefaultKind: rs.str(r, "default_kind"), Default: rs.str(r, "default_expression"),
			Comment: rs.str(r, "comment"), Codec: rs.str(r, "compression_codec")}
		c.Nullable = isNullableType(c.Type)
		flag := func(n string) bool { v, _ := rs.i64(r, n); return v != 0 }
		c.IsPartitionKey, c.IsSortingKey, c.IsPrimaryKey, c.IsSamplingKey = flag("is_in_partition_key"), flag("is_in_sorting_key"), flag("is_in_primary_key"), flag("is_in_sampling_key")
		if c.IsPrimaryKey {
			c.KeyOrdinal = pk[c.Name]
		}
		c.Position, _ = rs.i64(r, "position")
		out = append(out, c)
	}
	return out, nil
}

// Indexes 返回表的索引：主键（稀疏索引）、排序键（与主键不同时单列一条）与数据跳数索引。
func (e *Engine) Indexes(ctx context.Context, db, table string) ([]IndexInfo, error) {
	if err := checkName("库", db); err != nil {
		return nil, err
	}
	if err := checkName("表", table); err != nil {
		return nil, err
	}
	_, sorting, primary, err := e.tableKeys(ctx, db, table)
	if err != nil {
		return nil, err
	}
	var out []IndexInfo
	cols := func(key string) []string {
		var cs []string
		for _, p := range splitTopLevel(key) {
			cs = append(cs, unquoteIdent(p))
		}
		return cs
	}
	if primary == "" {
		primary = sorting
	}
	if primary != "" {
		out = append(out, IndexInfo{Name: "PRIMARY", Kind: "primary", Expression: primary, Columns: cols(primary)})
	}
	if sorting != "" && sorting != primary {
		out = append(out, IndexInfo{Name: "ORDER BY", Kind: "sorting", Expression: sorting, Columns: cols(sorting)})
	}
	rs, err := e.meta(ctx, "SELECT name, type, expr, granularity FROM system.data_skipping_indices WHERE database = "+sqlString(db)+" AND table = "+sqlString(table)+" ORDER BY name")
	if err != nil {
		return nil, err
	}
	for _, r := range rs.rows {
		g, _ := rs.i64(r, "granularity")
		out = append(out, IndexInfo{Name: rs.str(r, "name"), Kind: "skipping", Type: rs.str(r, "type"), Expression: rs.str(r, "expr"), Granularity: g})
	}
	return out, nil
}

// CreateStatement 返回建表语句（system.tables.create_table_query，服务端生成的规范化 DDL；
// 服务端会按其配置对语句里的密码等做脱敏）。表不存在或无权限返回错误。
func (e *Engine) CreateStatement(ctx context.Context, db, table string) (string, error) {
	if err := checkName("库", db); err != nil {
		return "", err
	}
	if err := checkName("表", table); err != nil {
		return "", err
	}
	rs, err := e.meta(ctx, "SELECT create_table_query AS q FROM system.tables WHERE database = "+sqlString(db)+" AND name = "+sqlString(table))
	if err != nil {
		return "", err
	}
	if len(rs.rows) == 0 {
		return "", fmt.Errorf("表 %s.%s 不存在或无权限查看", db, table)
	}
	q := rs.str(rs.rows[0], "q")
	if q == "" {
		return "", fmt.Errorf("服务端没有返回 %s.%s 的建表语句", db, table)
	}
	return q, nil
}
