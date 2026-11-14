// @author: kevinkelly
// @license: Apache Licence
// @file: mysqlcommon.go
// @time: 2026/10/28

package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	drv "github.com/go-sql-driver/mysql"
)

// 一次 INSERT / DELETE / IN 语句里最多的占位符个数（MySQL 上限 65535）和估算的最大字节数（远小于默认 max_allowed_packet）。
const (
	maxPlaceholders = 60000
	maxStmtBytes    = 1 << 20
)

// classify 把 MySQL 的死锁 / 锁等待超时包成 RetryableError。
func classify(err error) error {
	if err == nil {
		return nil
	}
	var me *drv.MySQLError
	if errors.As(err, &me) && (me.Number == 1205 || me.Number == 1213) {
		return &RetryableError{Err: err}
	}
	return err
}

// Fingerprint 计算表结构指纹：sha256(列名:类型 …|主键列)。
func Fingerprint(cols []ColumnInfo, pk []string) string {
	var b strings.Builder
	for _, c := range cols {
		b.WriteString(c.Name)
		b.WriteByte(':')
		b.WriteString(strings.ToLower(c.Type))
		b.WriteByte('\n')
	}
	b.WriteString("|")
	b.WriteString(strings.Join(pk, ","))
	s := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(s[:])
}

// typeCache 缓存各表列类型，把字符串形式的主键值转回正确的参数类型（整数列用整数参数，避免超过 2^53 的 BIGINT 经 double 比较丢精度）。
type typeCache struct {
	mu sync.Mutex
	m  map[string]map[string]string // "db.table" -> lower(col) -> type
}

func (c *typeCache) set(db, table string, cols []ColumnInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]map[string]string{}
	}
	m := make(map[string]string, len(cols))
	for _, col := range cols {
		m[strings.ToLower(col.Name)] = col.Type
	}
	c.m[db+"."+table] = m
}

func (c *typeCache) typeOf(db, table, col string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[db+"."+table][strings.ToLower(col)]
}

// pkArg 把字符串形式的主键值转成查询参数。
func pkArg(colType, v string) any {
	if kindOf(colType) == kindNumber {
		t := strings.ToLower(colType)
		if strings.HasPrefix(t, "decimal") || strings.HasPrefix(t, "numeric") || strings.HasPrefix(t, "float") || strings.HasPrefix(t, "double") || strings.HasPrefix(t, "real") {
			return v
		}
		if isUnsignedType(colType) {
			if n, err := strconv.ParseUint(v, 10, 64); err == nil {
				return n
			}
		} else if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
		return v
	}
	if kindOf(colType) == kindBinary && strings.HasPrefix(v, "0x") {
		if b, err := hex.DecodeString(v[2:]); err == nil {
			return b
		}
	}
	return v
}

func (c *typeCache) pkArgs(db, table string, pkCols []string, vals []string) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		t := ""
		if i < len(pkCols) {
			t = c.typeOf(db, table, pkCols[i])
		}
		out[i] = pkArg(t, v)
	}
	return out
}

// pkString 把读到的主键值转成字符串形式（与 pkArg 互逆）。
func pkString(v any, colType string) string {
	if b, ok := v.([]byte); ok && kindOf(colType) == kindBinary {
		return "0x" + hex.EncodeToString(b)
	}
	s, _ := valueText(v, kindText)
	return s
}

// scanAll 读完结果集：返回列名与全部行（每个值是驱动返回的原始类型，[]byte 已拷贝）。
func scanAll(rs *sql.Rows) ([]string, []Row, error) {
	cols, err := rs.Columns()
	if err != nil {
		return nil, nil, err
	}
	var out []Row
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		out = append(out, Row(vals))
	}
	return cols, out, rs.Err()
}

// colIndex 返回 cols 中各名字的下标（不区分大小写）。
func colIndex(cols, names []string) ([]int, error) {
	idx := make([]int, len(names))
	for i, n := range names {
		idx[i] = -1
		for j, c := range cols {
			if strings.EqualFold(c, n) {
				idx[i] = j
				break
			}
		}
		if idx[i] < 0 {
			return nil, fmt.Errorf("结果里没有列 %s", n)
		}
	}
	return idx, nil
}

// describe 读取表信息。full=false 时跳过分区 / 触发器 / 外键 / 建表语句（每批前取指纹用）。
func describe(ctx context.Context, db *sql.DB, schema, table string, full bool) (TableInfo, error) {
	info := TableInfo{DB: schema, Name: table}
	var engine sql.NullString
	var rows, size sql.NullInt64
	var ttype sql.NullString
	err := db.QueryRowContext(ctx, "SELECT ENGINE, TABLE_ROWS, DATA_LENGTH+INDEX_LENGTH, TABLE_TYPE FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?", schema, table).
		Scan(&engine, &rows, &size, &ttype)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// 兼容实现（PolarDB-X 等）的 TABLES 可能缺列：只取表类型，行数 / 大小留空
		err = db.QueryRowContext(ctx, "SELECT TABLE_TYPE FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?", schema, table).Scan(&ttype)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return info, nil
	}
	if err != nil {
		return info, err
	}
	info.Exists = true
	info.Engine = engine.String
	info.Rows = rows.Int64
	info.DataMB = size.Int64 >> 20
	if ttype.String == "VIEW" || ttype.String == "SYSTEM VIEW" {
		info.IsView = true
	}

	colRows, err := loadColumns(ctx, db, schema, table)
	if err != nil {
		return info, err
	}
	nullable := map[string]bool{}
	for _, c := range colRows {
		info.Columns = append(info.Columns, c)
		nullable[strings.ToLower(c.Name)] = c.Nullable
	}

	idxRows, err := loadIndexes(ctx, db, schema, table)
	if err != nil {
		return info, err
	}
	info.Indexes = map[string][]string{}
	type idxMeta struct {
		unique, prefix, nullCol bool
	}
	meta := map[string]*idxMeta{}
	for _, r := range idxRows {
		m := meta[r.name]
		if m == nil {
			m = &idxMeta{unique: r.nonUnique == 0}
			meta[r.name] = m
		}
		if r.prefix || r.col == "" { // 前缀索引或函数索引都不能保证唯一性
			m.prefix = true
		}
		if nullable[strings.ToLower(r.col)] {
			m.nullCol = true
		}
		info.Indexes[r.name] = append(info.Indexes[r.name], r.col)
	}
	// 主键：PRIMARY；否则第一个“唯一、非空、无前缀”的索引（列数少的优先，再按名字）
	if cols, ok := info.Indexes["PRIMARY"]; ok {
		info.PrimaryKey = cols
	} else {
		var names []string
		for n, m := range meta {
			if m.unique && !m.prefix && !m.nullCol {
				names = append(names, n)
			}
		}
		sort.Slice(names, func(i, j int) bool {
			a, b := info.Indexes[names[i]], info.Indexes[names[j]]
			if len(a) != len(b) {
				return len(a) < len(b)
			}
			return names[i] < names[j]
		})
		if len(names) > 0 {
			info.PrimaryKey = info.Indexes[names[0]]
		}
	}
	info.Fingerprint = Fingerprint(info.Columns, info.PrimaryKey)
	if !full {
		return info, nil
	}

	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND PARTITION_NAME IS NOT NULL", schema, table).Scan(&n); err == nil {
		info.Partitioned = n > 0
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE EVENT_OBJECT_SCHEMA=? AND EVENT_OBJECT_TABLE=?", schema, table).Scan(&n); err == nil {
		info.HasTriggers = n > 0
	}
	fr, err := db.QueryContext(ctx, "SELECT DISTINCT CONSTRAINT_SCHEMA, TABLE_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE UNIQUE_CONSTRAINT_SCHEMA=? AND REFERENCED_TABLE_NAME=?", schema, table)
	if err == nil {
		for fr.Next() {
			var s, t string
			if fr.Scan(&s, &t) == nil {
				info.FKParents = append(info.FKParents, s+"."+t)
			}
		}
		fr.Close()
		sort.Strings(info.FKParents)
	}
	var tname, ddl string
	if err := db.QueryRowContext(ctx, "SHOW CREATE TABLE "+qualified(schema, table)).Scan(&tname, &ddl); err == nil {
		info.CreateSQL = ddl
	}
	return info, nil
}

// chunkRows 把行按“占位符个数 + 估算字节数”切成多段，每段可放进一条语句。
func chunkRows(rows []Row, ncols int) [][]Row {
	if ncols < 1 {
		ncols = 1
	}
	perStmt := maxPlaceholders / ncols
	if perStmt < 1 {
		perStmt = 1
	}
	var out [][]Row
	start, bytes := 0, 0
	for i, r := range rows {
		sz := 0
		for _, v := range r {
			switch x := v.(type) {
			case []byte:
				sz += len(x) + 8
			case string:
				sz += len(x) + 8
			default:
				sz += 16
			}
		}
		if i > start && (i-start >= perStmt || bytes+sz > maxStmtBytes) {
			out = append(out, rows[start:i])
			start, bytes = i, 0
		}
		bytes += sz
	}
	if start < len(rows) {
		out = append(out, rows[start:])
	}
	return out
}

// chunkInts 把 n 个主键按占位符上限切段，返回每段 [lo, hi)。
func chunkSpans(n, perItem int) [][2]int {
	if perItem < 1 {
		perItem = 1
	}
	step := maxPlaceholders / perItem
	if step < 1 {
		step = 1
	}
	var out [][2]int
	for i := 0; i < n; i += step {
		j := i + step
		if j > n {
			j = n
		}
		out = append(out, [2]int{i, j})
	}
	return out
}
