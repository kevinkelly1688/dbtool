// @author: kevinkelly
// @license: Apache Licence
// @file: mysqldest.go
// @time: 2026/10/28

package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// DiskFunc 返回目标端磁盘剩余百分比；ok=false 表示取不到。
type DiskFunc func(ctx context.Context) (pct float64, ok bool, err error)

// mysqlDest 是 DestDB 的 MySQL / MariaDB 实现。
type mysqlDest struct {
	db    *sql.DB
	disk  DiskFunc
	types typeCache
}

// NewMySQLDest 用已建好的连接池创建 DestDB。disk 可为 nil（磁盘剩余恒为“未检测”）。
func NewMySQLDest(db *sql.DB, disk DiskFunc) DestDB { return &mysqlDest{db: db, disk: disk} }

func (d *mysqlDest) Describe(ctx context.Context, db, table string) (TableInfo, error) {
	info, err := describe(ctx, d.db, db, table, false)
	if err == nil && info.Exists {
		d.types.set(db, table, info.Columns)
	}
	return info, err
}

func (d *mysqlDest) DBExists(ctx context.Context, db string) (bool, error) {
	var n int
	err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME=?", db).Scan(&n)
	return n > 0, err
}

var (
	reAutoInc   = regexp.MustCompile(`(?i)\s+AUTO_INCREMENT=\d+`)
	rePartition = regexp.MustCompile(`(?s)\s*/\*!\d+\s+PARTITION BY.*?\*/`)
	rePartBare  = regexp.MustCompile(`(?s)\s+PARTITION BY\s.*$`)
	// PolarDB-X（DRDS）的拆分子句：dbpartition / tbpartition 到结尾；BROADCAST / SINGLE 表标记；LOCALITY 属性。备份表不拆分。
	reXPartition = regexp.MustCompile(`(?is)\s+(?:DBPARTITION|TBPARTITION)\s+BY\s.*$`)
	reXTail      = regexp.MustCompile(`(?i)\s+(?:BROADCAST|SINGLE)\s*;?\s*$`)
	reXLocality  = regexp.MustCompile(`(?i)\s+LOCALITY\s*=\s*'[^']*'`)
	reFKLine     = regexp.MustCompile(`(?mi)^\s*(CONSTRAINT\s+\S+\s+)?FOREIGN KEY\b.*$`)
	reCreateHd   = regexp.MustCompile("(?i)^\\s*CREATE\\s+TABLE\\s+(IF NOT EXISTS\\s+)?`(?:[^`]|``)+`")
)

// rewriteCreateSQL 把源表的 SHOW CREATE TABLE 改写成在目标库里建同结构表的语句：换表名、去掉 AUTO_INCREMENT 起点、
// 分区定义和外键约束（归档表不需要，且外键会因父表不在而建不出来）。
func rewriteCreateSQL(ddl, db, table string) (string, error) {
	if !reCreateHd.MatchString(ddl) {
		return "", errors.New("无法解析源表的建表语句")
	}
	out := reCreateHd.ReplaceAllString(ddl, "CREATE TABLE IF NOT EXISTS "+qualified(db, table))
	out = rePartition.ReplaceAllString(out, "")
	out = rePartBare.ReplaceAllString(out, "")
	out = reXPartition.ReplaceAllString(out, "")
	out = reXTail.ReplaceAllString(out, "")
	out = reXLocality.ReplaceAllString(out, "")
	out = reAutoInc.ReplaceAllString(out, "")
	// 去掉外键行，并修复前一行行尾多出的逗号
	lines := strings.Split(out, "\n")
	var kept []string
	for _, ln := range lines {
		if reFKLine.MatchString(ln) {
			continue
		}
		kept = append(kept, ln)
	}
	for i := 0; i+1 < len(kept); i++ {
		if strings.HasSuffix(strings.TrimRight(kept[i], " \t"), ",") && strings.HasPrefix(strings.TrimSpace(kept[i+1]), ")") {
			kept[i] = strings.TrimSuffix(strings.TrimRight(kept[i], " \t"), ",")
		}
	}
	return strings.Join(kept, "\n"), nil
}

// compatErr 检查目标表是否能装下源表的行：每个源列在目标表里都有同类型的列，并且主键一致（保证 INSERT 的幂等判断有依据）。返回 nil 表示兼容。
func compatErr(src, dst TableInfo) error {
	dcols := map[string]ColumnInfo{}
	for _, c := range dst.Columns {
		dcols[strings.ToLower(c.Name)] = c
	}
	var missing, diff []string
	for _, c := range src.Columns {
		dc, ok := dcols[strings.ToLower(c.Name)]
		switch {
		case !ok:
			missing = append(missing, c.Name)
		case !sameColType(c.Type, dc.Type):
			diff = append(diff, fmt.Sprintf("%s（源 %s / 目标 %s）", c.Name, c.Type, dc.Type))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("目标表缺少列 %s，结构不兼容", strings.Join(missing, "、"))
	}
	if len(diff) > 0 {
		return fmt.Errorf("目标表列类型不一致：%s", strings.Join(diff, "；"))
	}
	if !strings.EqualFold(strings.Join(src.PrimaryKey, ","), strings.Join(dst.PrimaryKey, ",")) {
		return fmt.Errorf("目标表的主键 (%s) 与源表 (%s) 不一致", strings.Join(dst.PrimaryKey, ", "), strings.Join(src.PrimaryKey, ", "))
	}
	return nil
}

var reDisplayWidth = regexp.MustCompile(`^((?:tiny|small|medium|big)?int|integer)\(\d+\)`)

// sameColType 比较列类型（忽略大小写和 5.7 的整数显示宽度，如 int(11) 与 int）。
func sameColType(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		return reDisplayWidth.ReplaceAllString(s, "$1")
	}
	return norm(a) == norm(b)
}

func (d *mysqlDest) EnsureTable(ctx context.Context, db, table string, src TableInfo) error {
	ok, err := d.DBExists(ctx, db)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("目标库 %s 不存在，请先建库", db)
	}
	dst, err := d.Describe(ctx, db, table)
	if err != nil {
		return err
	}
	if dst.IsView {
		return fmt.Errorf("目标 %s.%s 是视图", db, table)
	}
	if !dst.Exists {
		if src.CreateSQL == "" {
			return errors.New("缺少源表的建表语句，无法创建目标表")
		}
		ddl, err := rewriteCreateSQL(src.CreateSQL, db, table)
		if err != nil {
			return err
		}
		if _, err := d.db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("创建目标表 %s.%s 失败：%w", db, table, err)
		}
		if dst, err = d.Describe(ctx, db, table); err != nil {
			return err
		}
		if !dst.Exists {
			return fmt.Errorf("创建目标表 %s.%s 后仍查不到", db, table)
		}
	}
	if err := compatErr(src, dst); err != nil {
		return fmt.Errorf("目标表 %s.%s：%w", db, table, err)
	}
	return nil
}

// rowsByPK 读出目标端这些主键对应的行，键是 pkKey。
func (d *mysqlDest) rowsByPK(ctx context.Context, tx *sql.Tx, db, table string, cols, pkCols []string, pkIdx []int, rows []Row) (map[string]Row, error) {
	out := map[string]Row{}
	for _, sp := range chunkSpans(len(rows), len(pkCols)) {
		part := rows[sp[0]:sp[1]]
		args := make([]any, 0, len(part)*len(pkCols))
		for _, r := range part {
			for _, ix := range pkIdx {
				args = append(args, r[ix])
			}
		}
		rs, err := tx.QueryContext(ctx, buildSelectByPK(db, table, cols, pkCols, len(part)), args...)
		if err != nil {
			return nil, err
		}
		_, got, err := scanAll(rs)
		rs.Close()
		if err != nil {
			return nil, err
		}
		for _, g := range got {
			out[rowPKKey(g, pkIdx)] = g
		}
	}
	return out, nil
}

func rowPKKey(r Row, pkIdx []int) string {
	vals := make([]string, len(pkIdx))
	for i, ix := range pkIdx {
		vals[i] = valKey(r[ix])
	}
	return pkKeyOf(vals)
}

func (d *mysqlDest) Insert(ctx context.Context, db, table string, cols []string, rows []Row, pkCols []string) (inserted, existed int64, err error) {
	if len(rows) == 0 {
		return 0, 0, nil
	}
	pkIdx, err := colIndex(cols, pkCols)
	if err != nil {
		return 0, 0, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, classify(err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	have, err := d.rowsByPK(ctx, tx, db, table, cols, pkCols, pkIdx, rows)
	if err != nil {
		return 0, 0, classify(err)
	}
	var fresh []Row
	for _, r := range rows {
		if len(r) != len(cols) {
			return 0, 0, errors.New("行的列数与列名个数不一致")
		}
		if g, ok := have[rowPKKey(r, pkIdx)]; ok {
			if rowKey(g) != rowKey(r) {
				return 0, 0, fmt.Errorf("%w：目标表 %s.%s 已有主键相同但内容不一致的行", ErrMismatch, db, table)
			}
			existed++
			continue
		}
		fresh = append(fresh, r)
	}
	for _, part := range chunkRows(fresh, len(cols)) {
		args := make([]any, 0, len(part)*len(cols))
		for _, r := range part {
			args = append(args, r...)
		}
		if _, err = tx.ExecContext(ctx, buildInsertSQL("INSERT", db, table, cols, len(part)), args...); err != nil {
			return 0, 0, classify(err)
		}
		inserted += int64(len(part))
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, classify(err)
	}
	return inserted, existed, nil
}

func (d *mysqlDest) VerifyPKs(ctx context.Context, db, table string, pkCols []string, pks [][]string) (int64, error) {
	var found int64
	for _, sp := range chunkSpans(len(pks), len(pkCols)) {
		part := pks[sp[0]:sp[1]]
		args := make([]any, 0, len(part)*len(pkCols))
		for _, pk := range part {
			if len(pk) != len(pkCols) {
				return 0, errors.New("主键值个数与主键列数不一致")
			}
			args = append(args, d.types.pkArgs(db, table, pkCols, pk)...)
		}
		var n int64
		if err := d.db.QueryRowContext(ctx, buildCountByPK(db, table, pkCols, len(part)), args...).Scan(&n); err != nil {
			return 0, classify(err)
		}
		found += n
	}
	return found, nil
}

func (d *mysqlDest) ReadRange(ctx context.Context, req RangeReq) (Batch, error) {
	n := len(req.PKCols)
	if n == 0 || len(req.Min) != n || len(req.Max) != n || (len(req.After) != 0 && len(req.After) != n) {
		return Batch{}, errors.New("主键范围与主键列数不一致")
	}
	args := append([]any{}, d.types.pkArgs(req.DB, req.Table, req.PKCols, req.Min)...)
	args = append(args, d.types.pkArgs(req.DB, req.Table, req.PKCols, req.Max)...)
	if len(req.After) > 0 {
		args = append(args, d.types.pkArgs(req.DB, req.Table, req.PKCols, req.After)...)
	}
	args = append(args, req.Limit)
	rs, err := d.db.QueryContext(ctx, buildRangeSQL(req.DB, req.Table, req.PKCols, len(req.After) > 0), args...)
	if err != nil {
		return Batch{}, classify(err)
	}
	defer rs.Close()
	cols, rows, err := scanAll(rs)
	if err != nil {
		return Batch{}, classify(err)
	}
	b := Batch{Cols: cols, Rows: rows}
	if len(rows) > 0 {
		idx, err := colIndex(cols, req.PKCols)
		if err != nil {
			return Batch{}, err
		}
		b.PKs = make([][]string, len(rows))
		for i, r := range rows {
			pk := make([]string, len(idx))
			for j, ix := range idx {
				pk[j] = pkString(r[ix], d.types.typeOf(req.DB, req.Table, req.PKCols[j]))
			}
			b.PKs[i] = pk
		}
	}
	return b, nil
}

func (d *mysqlDest) DiskFreePct(ctx context.Context) (float64, bool, error) {
	if d.disk == nil {
		return 0, false, nil
	}
	return d.disk(ctx)
}

func (d *mysqlDest) Close() error { return d.db.Close() }
