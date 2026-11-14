// @author: kevinkelly
// @license: Apache Licence
// @file: mysqlsource.go
// @time: 2026/10/28

package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	drv "github.com/go-sql-driver/mysql"
)

// mysqlSource 是 SourceDB 的 MySQL / MariaDB 实现。会话参数（lock_wait_timeout 等）在 Opener 建连时设置。
// 说明：ReadReq.LockWaitSe / DeleteReq.LockWaitSe 在本实现里不逐请求生效，锁等待超时取 Opener.Open 时 Spec.Params.LockWaitSeconds。
type mysqlSource struct {
	db    *sql.DB
	types typeCache
}

// NewMySQLSource 用已建好的连接池创建 SourceDB。
func NewMySQLSource(db *sql.DB) SourceDB { return &mysqlSource{db: db} }

func (s *mysqlSource) Describe(ctx context.Context, db, table string) (TableInfo, error) {
	info, err := describe(ctx, s.db, db, table, true)
	if err == nil && info.Exists {
		s.types.set(db, table, info.Columns)
	}
	return info, err
}

func (s *mysqlSource) Fingerprint(ctx context.Context, db, table string) (string, error) {
	info, err := describe(ctx, s.db, db, table, false)
	if err != nil {
		return "", err
	}
	if !info.Exists {
		return "", fmt.Errorf("源表 %s.%s 不存在", db, table)
	}
	s.types.set(db, table, info.Columns)
	return info.Fingerprint, nil
}

func (s *mysqlSource) Explain(ctx context.Context, query string, args ...any) (ExplainResult, error) {
	res, err := s.explain(ctx, "EXPLAIN "+query, args...)
	if err != nil && isUnsupportedPS(err) { // 该版本不支持预编译 EXPLAIN：把参数安全地内联后重试
		if q2, ierr := interpolateArgs(query, args); ierr == nil {
			return s.explain(ctx, "EXPLAIN "+q2)
		}
	}
	return res, err
}

func (s *mysqlSource) explain(ctx context.Context, query string, args ...any) (ExplainResult, error) {
	rs, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return ExplainResult{}, err
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return ExplainResult{}, err
	}
	var res ExplainResult
	first := true
	for rs.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return res, err
		}
		get := func(name string) string {
			for i, c := range cols {
				if strings.EqualFold(c, name) {
					return vals[i].String
				}
			}
			return ""
		}
		rows, _ := strconv.ParseInt(get("rows"), 10, 64)
		if first {
			res = ExplainResult{UsedIndex: get("key"), ScanRows: rows, Type: get("type"), Extra: get("Extra")}
			first = false
		} else if rows > res.ScanRows { // 多行（联表 / 子查询）取扫描行数最大的一行，最保守
			res = ExplainResult{UsedIndex: get("key"), ScanRows: rows, Type: get("type"), Extra: get("Extra")}
		}
	}
	return res, rs.Err()
}

func (s *mysqlSource) CountWhere(ctx context.Context, db, table, where string, limit int64) (int64, bool, error) {
	if limit < 1 {
		limit = 1
	}
	q := fmt.Sprintf("SELECT COUNT(*) FROM (SELECT 1 FROM %s WHERE (%s) LIMIT ?) AS t", qualified(db, table), where)
	var n int64
	if err := s.db.QueryRowContext(ctx, q, limit+1).Scan(&n); err != nil {
		return 0, false, err
	}
	if n > limit {
		return limit, true, nil
	}
	return n, false, nil
}

func (s *mysqlSource) Sample(ctx context.Context, db, table, where string, cols []string, limit int) ([]map[string]string, error) {
	sel := "*"
	if len(cols) > 0 {
		sel = quoteIdents(cols)
	}
	q := fmt.Sprintf("SELECT %s FROM %s WHERE (%s) LIMIT ?", sel, qualified(db, table), where)
	rs, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	names, rows, err := scanAll(rs)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]string, 0, len(rows))
	for _, r := range rows {
		m := make(map[string]string, len(names))
		for i, n := range names {
			t := s.types.typeOf(db, table, n)
			v, null := valueText(r[i], kindOf(t))
			if null {
				v = "NULL"
			}
			if rs := []rune(v); len(rs) > 200 {
				v = string(rs[:200]) + "…"
			}
			m[n] = v
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *mysqlSource) ReadBatch(ctx context.Context, req ReadReq) (Batch, error) {
	if len(req.PKCols) == 0 {
		return Batch{}, errors.New("缺少主键列")
	}
	if req.Limit < 1 {
		return Batch{}, errors.New("批大小必须为正")
	}
	withAfter := len(req.After) > 0 && !req.Unordered
	q := buildReadSQL(req.DB, req.Table, req.Where, req.PKCols, withAfter, req.Unordered)
	var args []any
	if withAfter {
		if len(req.After) != len(req.PKCols) {
			return Batch{}, fmt.Errorf("断点列数 %d 与主键列数 %d 不一致", len(req.After), len(req.PKCols))
		}
		args = append(args, s.types.pkArgs(req.DB, req.Table, req.PKCols, req.After)...)
	}
	args = append(args, req.Limit)
	rs, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return Batch{}, classify(err)
	}
	defer rs.Close()
	cols, rows, err := scanAll(rs)
	if err != nil {
		return Batch{}, classify(err)
	}
	b := Batch{Cols: cols, Rows: rows}
	if len(rows) == 0 {
		return b, nil
	}
	idx, err := colIndex(cols, req.PKCols)
	if err != nil {
		return Batch{}, err
	}
	b.PKs = make([][]string, len(rows))
	for i, r := range rows {
		pk := make([]string, len(idx))
		for j, ix := range idx {
			pk[j] = pkString(r[ix], s.types.typeOf(req.DB, req.Table, req.PKCols[j]))
		}
		b.PKs[i] = pk
	}
	return b, nil
}

func (s *mysqlSource) DeleteBatch(ctx context.Context, req DeleteReq) (int64, error) {
	if len(req.PKs) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, classify(err)
	}
	var total int64
	for _, sp := range chunkSpans(len(req.PKs), len(req.PKCols)) {
		part := req.PKs[sp[0]:sp[1]]
		args := make([]any, 0, len(part)*len(req.PKCols))
		for _, pk := range part {
			if len(pk) != len(req.PKCols) {
				tx.Rollback()
				return 0, errors.New("主键值个数与主键列数不一致")
			}
			args = append(args, s.types.pkArgs(req.DB, req.Table, req.PKCols, pk)...)
		}
		res, err := tx.ExecContext(ctx, buildDeleteSQL(req.DB, req.Table, req.Where, req.PKCols, len(part)), args...)
		if err != nil {
			tx.Rollback()
			return 0, classify(err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if err := tx.Commit(); err != nil {
		return 0, classify(err)
	}
	return total, nil
}

func (s *mysqlSource) InsertIgnore(ctx context.Context, req InsertReq) (int64, error) {
	if len(req.Rows) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, classify(err)
	}
	var total int64
	for _, part := range chunkRows(req.Rows, len(req.Cols)) {
		args := make([]any, 0, len(part)*len(req.Cols))
		for _, r := range part {
			if len(r) != len(req.Cols) {
				tx.Rollback()
				return 0, errors.New("行的列数与列名个数不一致")
			}
			args = append(args, r...)
		}
		res, err := tx.ExecContext(ctx, buildInsertSQL("INSERT IGNORE", req.DB, req.Table, req.Cols, len(part)), args...)
		if err != nil {
			tx.Rollback()
			return 0, classify(err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if err := tx.Commit(); err != nil {
		return 0, classify(err)
	}
	return total, nil
}

// ProbeTime 实现 TimeProber：探测整型时间戳列（抽样 + 可选的最早 / 最晚值），只发 SELECT。
func (s *mysqlSource) ProbeTime(ctx context.Context, db, table, col string, rangeOK bool) (TimeProbe, error) {
	return ProbeEpochColumn(ctx, s.db, db, table, col, rangeOK)
}

func (s *mysqlSource) Close() error { return s.db.Close() }

// isUnsupportedPS 判断是不是“这条语句不能用预编译协议”（MySQL 错误 1295；中间件转发时只剩文字）。
func isUnsupportedPS(err error) bool {
	var me *drv.MySQLError
	if errors.As(err, &me) && me.Number == 1295 {
		return true
	}
	return strings.Contains(err.Error(), "not supported in the prepared statement protocol")
}
