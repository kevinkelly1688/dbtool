// @author: kevinkelly
// @license: Apache Licence
// @file: compat.go
// @time: 2026/10/28

package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 兼容实现（PolarDB-X / DRDS 等）的 information_schema 不一定完整：这里的读取函数先走标准查询，失败再退到 SHOW 语句，
// 两种都失败才把错误交给上层。MySQL / MariaDB / PolarDB 上第一种查询就成功，行为不变。

// idxRow 是索引的一列。
type idxRow struct {
	name      string
	nonUnique int64
	col       string // 函数索引为空串
	seq       int64
	prefix    bool // 前缀索引（SUB_PART / Sub_part 非空）
}

func cellText(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case []byte:
		return string(x), true
	case string:
		return x, true
	default:
		return fmt.Sprint(x), true
	}
}

func cellInt(v any) int64 {
	s, ok := cellText(v)
	if !ok {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// loadColumns 读表的列（名称、类型、是否可空），按列顺序。
func loadColumns(ctx context.Context, db *sql.DB, schema, table string) ([]ColumnInfo, error) {
	cols, err := loadColumnsIS(ctx, db, schema, table)
	if err == nil {
		return cols, nil
	}
	rs, qerr := db.QueryContext(ctx, "SHOW COLUMNS FROM "+qualified(schema, table))
	if qerr != nil {
		return nil, err // 返回标准查询的错误：它才是主因
	}
	defer rs.Close()
	names, rows, serr := scanAll(rs)
	if serr != nil {
		return nil, err
	}
	idx, ierr := colIndex(names, []string{"Field", "Type", "Null"})
	if ierr != nil {
		return nil, err
	}
	var out []ColumnInfo
	for _, r := range rows {
		n, _ := cellText(r[idx[0]])
		t, _ := cellText(r[idx[1]])
		nl, _ := cellText(r[idx[2]])
		out = append(out, ColumnInfo{Name: n, Type: t, Nullable: strings.EqualFold(nl, "YES")})
	}
	return out, nil
}

func loadColumnsIS(ctx context.Context, db *sql.DB, schema, table string) ([]ColumnInfo, error) {
	cr, err := db.QueryContext(ctx, "SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY ORDINAL_POSITION", schema, table)
	if err != nil {
		return nil, err
	}
	defer cr.Close()
	var out []ColumnInfo
	for cr.Next() {
		var n, t, nl string
		if err := cr.Scan(&n, &t, &nl); err != nil {
			return nil, err
		}
		out = append(out, ColumnInfo{Name: n, Type: t, Nullable: nl == "YES"})
	}
	return out, cr.Err()
}

// loadIndexes 读表的全部索引列，按索引名、列序排序。
func loadIndexes(ctx context.Context, db *sql.DB, schema, table string) ([]idxRow, error) {
	out, err := loadIndexesIS(ctx, db, schema, table)
	if err == nil {
		return out, nil
	}
	rs, qerr := db.QueryContext(ctx, "SHOW INDEX FROM "+qualified(schema, table))
	if qerr != nil {
		return nil, err
	}
	defer rs.Close()
	names, rows, serr := scanAll(rs)
	if serr != nil {
		return nil, err
	}
	idx, ierr := colIndex(names, []string{"Key_name", "Non_unique", "Seq_in_index", "Column_name"})
	if ierr != nil {
		return nil, err
	}
	sub := -1
	for i, c := range names {
		if strings.EqualFold(c, "Sub_part") {
			sub = i
		}
	}
	out = nil
	for _, r := range rows {
		name, _ := cellText(r[idx[0]])
		col, _ := cellText(r[idx[3]])
		x := idxRow{name: name, nonUnique: cellInt(r[idx[1]]), seq: cellInt(r[idx[2]]), col: col}
		if sub >= 0 {
			if _, ok := cellText(r[sub]); ok {
				x.prefix = true
			}
		}
		out = append(out, x)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].seq < out[j].seq
	})
	return out, nil
}

func loadIndexesIS(ctx context.Context, db *sql.DB, schema, table string) ([]idxRow, error) {
	ir, err := db.QueryContext(ctx, "SELECT INDEX_NAME, NON_UNIQUE, COLUMN_NAME, SEQ_IN_INDEX, SUB_PART FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY INDEX_NAME, SEQ_IN_INDEX", schema, table)
	if err != nil {
		return nil, err
	}
	defer ir.Close()
	var out []idxRow
	for ir.Next() {
		var name, col sql.NullString
		var nonUnique, seq, sub sql.NullInt64
		if err := ir.Scan(&name, &nonUnique, &col, &seq, &sub); err != nil {
			return nil, err
		}
		out = append(out, idxRow{name: name.String, nonUnique: nonUnique.Int64, col: col.String, seq: seq.Int64, prefix: sub.Valid})
	}
	return out, ir.Err()
}
