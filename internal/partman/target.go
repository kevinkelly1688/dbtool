// @author: kevinkelly
// @license: Apache Licence
// @file: target.go
// @time: 2026/10/28

package partman

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	drv "github.com/go-sql-driver/mysql"

	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// ExecResult 是一条语句的执行结果。
type ExecResult struct {
	SQL       string
	Err       string // 非空表示失败
	Skipped   bool   // 前面的语句失败，这条没有执行
	ElapsedMs int64
}

// Target 是一次维护操作用到的目标库连接（用完必须 Close）。测试里用假实现。
type Target interface {
	// Inspect 读取表与分区字段的当前状态。表不存在不是错误，返回 Exists=false。
	Inspect(ctx context.Context, db, table, column string) (TableState, error)
	// Exec 在同一个会话里按顺序执行，遇到第一条失败即停，其后的标记 Skipped；返回值与 stmts 等长。
	Exec(ctx context.Context, stmts []string) []ExecResult
	Close() error
}

// Dialer 按实例创建 Target。
type Dialer func(ctx context.Context, inst *model.Instance, conn engine.Conn) (Target, error)

// SupportedDBType 判断实例类型是否支持分区维护：MySQL / PolarDB（MySQL 方言）与 SelectDB / Doris（Doris 方言）。
func SupportedDBType(dbType string) bool { return DialectOf(dbType) != "" }

func unsupportedErr(dbType string) *Error {
	return newErr(501, "unsupported_engine", "该类型（%s）的实例暂不支持分区维护，目前支持 MySQL / PolarDB / SelectDB / Doris", dbType)
}

// DefaultDialer 用 database/sql + go-sql-driver/mysql 连接目标库（Doris 也走 MySQL 协议）。
func DefaultDialer(ctx context.Context, inst *model.Instance, conn engine.Conn) (Target, error) {
	d := DialectOf(inst.DBType)
	if d == "" {
		return nil, unsupportedErr(inst.DBType)
	}
	cfg := drv.NewConfig()
	cfg.User, cfg.Passwd = conn.User, conn.Password
	cfg.Net, cfg.Addr = "tcp", fmt.Sprintf("%s:%d", conn.Host, conn.Port)
	cfg.Timeout = 10 * time.Second // 建连超时；语句超时由 ctx 控制
	charset := conn.Charset
	if charset == "" {
		charset = "utf8mb4"
	}
	cfg.Params = map[string]string{"charset": charset}
	if conn.SSL {
		cfg.TLSConfig = "skip-verify"
		if conn.VerifySSL {
			cfg.TLSConfig = "true"
		}
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	if d == DialectDoris {
		return &dorisTarget{mysqlTarget{db: db}}, nil
	}
	return &mysqlTarget{db: db}, nil
}

type mysqlTarget struct{ db *sql.DB }

func (t *mysqlTarget) Close() error { return t.db.Close() }

// sessionOffset 读目标库当前会话相对 UTC 的偏移秒数（配置时区为空时用）。读不到返回 ok=false，不算错误。
func (t *mysqlTarget) sessionOffset(ctx context.Context) (int, bool) {
	var sec sql.NullInt64
	if err := t.db.QueryRowContext(ctx, "SELECT TIMESTAMPDIFF(SECOND, UTC_TIMESTAMP(), NOW())").Scan(&sec); err != nil || !sec.Valid {
		return 0, false
	}
	return int(sec.Int64), true
}

func (t *mysqlTarget) Inspect(ctx context.Context, db, table, column string) (TableState, error) {
	var st TableState
	var typ string
	var eng sql.NullString
	err := t.db.QueryRowContext(ctx, "SELECT TABLE_TYPE, ENGINE FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", db, table).Scan(&typ, &eng)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Exists, st.IsBase, st.Engine = true, typ == "BASE TABLE", eng.String
	st.Dialect = DialectMySQL
	st.SessionOffset, st.HasSessionOffset = t.sessionOffset(ctx)

	err = t.db.QueryRowContext(ctx, "SELECT DATA_TYPE, COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?",
		db, table, column).Scan(&st.ColumnType, &st.ColumnTypeFull)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}

	rows, err := t.db.QueryContext(ctx, `SELECT PARTITION_NAME, SUBPARTITION_NAME, PARTITION_METHOD, PARTITION_EXPRESSION, PARTITION_DESCRIPTION
FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND PARTITION_NAME IS NOT NULL ORDER BY PARTITION_ORDINAL_POSITION, SUBPARTITION_ORDINAL_POSITION`, db, table)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		var sub, method, expr, desc sql.NullString
		if err := rows.Scan(&name, &sub, &method, &expr, &desc); err != nil {
			return st, err
		}
		if sub.Valid {
			st.Subpartitioned = true
		}
		st.Method, st.Expression = strings.ToUpper(method.String), expr.String
		if !seen[name] { // 有子分区时同一个分区会出现多行
			seen[name] = true
			st.Partitions = append(st.Partitions, RawPartition{Name: name, Desc: desc.String})
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	rows.Close()
	if len(st.Partitions) == 0 && st.IsBase { // 未分区：读“初始化为分区表”的前置检查要用的事实（唯一键、外键）
		if err := t.loadInitFacts(ctx, db, table, &st); err != nil {
			return st, err
		}
	}
	return st, nil
}

// loadInitFacts 读唯一键（含主键）与外键关系。
func (t *mysqlTarget) loadInitFacts(ctx context.Context, db, table string, st *TableState) error {
	rows, err := t.db.QueryContext(ctx, `SELECT INDEX_NAME, COLUMN_NAME FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND NON_UNIQUE = 0 ORDER BY INDEX_NAME, SEQ_IN_INDEX`, db, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	idx := map[string]int{}
	for rows.Next() {
		var name string
		var col sql.NullString
		if err := rows.Scan(&name, &col); err != nil {
			return err
		}
		c := col.String
		if !col.Valid {
			c = "(表达式)" // 函数索引：无法证明包含分区字段
		}
		i, ok := idx[name]
		if !ok {
			i = len(st.UniqueKeys)
			idx[name] = i
			st.UniqueKeys = append(st.UniqueKeys, UniqueKey{Name: name})
		}
		st.UniqueKeys[i].Columns = append(st.UniqueKeys[i].Columns, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	fk, err := t.db.QueryContext(ctx, `SELECT CONSTRAINT_NAME, TABLE_NAME, REFERENCED_TABLE_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS
WHERE (CONSTRAINT_SCHEMA = ? AND TABLE_NAME = ?) OR (UNIQUE_CONSTRAINT_SCHEMA = ? AND REFERENCED_TABLE_NAME = ?)`, db, table, db, table)
	if err != nil {
		return err
	}
	defer fk.Close()
	for fk.Next() {
		var name, child, parent string
		if err := fk.Scan(&name, &child, &parent); err != nil {
			return err
		}
		if strings.EqualFold(child, table) {
			st.ForeignKeys = append(st.ForeignKeys, fmt.Sprintf("外键 %s：本表引用 %s", name, parent))
		} else {
			st.ForeignKeys = append(st.ForeignKeys, fmt.Sprintf("外键 %s：被表 %s 引用", name, child))
		}
	}
	return fk.Err()
}

// MinTime 读分区字段的最小值（初始化“从最早数据开始”用）。带 MAX_EXECUTION_TIME(20000) 提示与 ctx 超时，超时 / 失败返回 error。
func (t *mysqlTarget) MinTime(ctx context.Context, db, table, column, colType string, loc *time.Location) (time.Time, bool, error) {
	if !validIdent(db) || !validIdent(table) || !validIdent(column) {
		return time.Time{}, false, errors.New("库名、表名或字段名不合法")
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	q := fmt.Sprintf("SELECT /*+ MAX_EXECUTION_TIME(20000) */ MIN(%s) FROM %s.%s", quote(column), quote(db), quote(table))
	var v sql.NullString
	if err := t.db.QueryRowContext(ctx, q).Scan(&v); err != nil {
		return time.Time{}, false, err
	}
	if !v.Valid || strings.TrimSpace(v.String) == "" {
		return time.Time{}, false, nil
	}
	s := strings.TrimSpace(v.String)
	switch colType {
	case ColInt, ColBigint:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("无法解析最小值 %s", qstr(s))
		}
		if colType == ColInt {
			return time.Unix(n, 0).In(loc), true, nil
		}
		return time.UnixMilli(n).In(loc), true, nil
	default:
		for _, layout := range []string{dtLayout, "2006-01-02 15:04:05.999999", "2006-01-02"} {
			if tm, err := time.ParseInLocation(layout, s, loc); err == nil {
				return tm, true, nil
			}
		}
		return time.Time{}, false, fmt.Errorf("无法解析最小值 %s", qstr(s))
	}
}

// lockWaitTimeout 是 ALTER 等待元数据锁的上限（秒）。拿不到锁就失败，等下一轮再试：
// 无限等待会让后面所有访问这张表的查询在它后面排队。
const lockWaitTimeout = 15

func (t *mysqlTarget) Exec(ctx context.Context, stmts []string) []ExecResult {
	return t.execStmts(ctx, stmts, fmt.Sprintf("SET SESSION lock_wait_timeout = %d", lockWaitTimeout))
}

// execStmts 在同一个会话里按顺序执行；preamble 非空时先执行它（失败忽略）。
func (t *mysqlTarget) execStmts(ctx context.Context, stmts []string, preamble string) []ExecResult {
	out := make([]ExecResult, len(stmts))
	for i, s := range stmts {
		out[i].SQL = s
	}
	conn, err := t.db.Conn(ctx)
	if err != nil {
		for i := range out {
			out[i].Skipped = true
		}
		if len(out) > 0 {
			out[0].Skipped, out[0].Err = false, err.Error()
		}
		return out
	}
	defer conn.Close()
	if preamble != "" {
		_, _ = conn.ExecContext(ctx, preamble)
	}
	failed := false
	for i, s := range stmts {
		if failed {
			out[i].Skipped = true
			continue
		}
		start := time.Now()
		_, err := conn.ExecContext(ctx, s)
		out[i].ElapsedMs = time.Since(start).Milliseconds()
		if err != nil {
			out[i].Err = err.Error()
			failed = true
		}
	}
	return out
}

// ---- SelectDB / Doris ----

// dorisTarget 走 MySQL 协议；读分区用 SHOW PARTITIONS，不设 lock_wait_timeout。
type dorisTarget struct{ mysqlTarget }

func (t *dorisTarget) Exec(ctx context.Context, stmts []string) []ExecResult {
	return t.execStmts(ctx, stmts, "")
}

// isNotPartitionedErr 判断 SHOW PARTITIONS 的错误是不是“这不是分区表”（而不是连接 / 权限问题，后者要原样返回）。
func isNotPartitionedErr(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "not a partitioned") || strings.Contains(m, "not partitioned") || strings.Contains(m, "unpartitioned")
}

func (t *dorisTarget) Inspect(ctx context.Context, db, table, column string) (TableState, error) {
	var st TableState
	if !validIdent(db) || !validIdent(table) {
		return st, badReq("库名或表名不合法")
	}
	var typ string
	err := t.db.QueryRowContext(ctx, "SELECT TABLE_TYPE FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", db, table).Scan(&typ)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Exists, st.IsBase, st.Dialect = true, strings.EqualFold(typ, "BASE TABLE"), DialectDoris
	st.SessionOffset, st.HasSessionOffset = t.sessionOffset(ctx)

	var dt, full sql.NullString
	err = t.db.QueryRowContext(ctx, "SELECT DATA_TYPE, COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?",
		db, table, column).Scan(&dt, &full)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	st.ColumnType, st.ColumnTypeFull = normDorisType(dt.String), full.String

	rows, err := t.db.QueryContext(ctx, fmt.Sprintf("SHOW PARTITIONS FROM %s.%s", quote(db), quote(table)))
	if err != nil {
		if isNotPartitionedErr(err) {
			return st, nil
		}
		return st, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return st, err
	}
	nameIdx, rangeIdx, keyIdx := -1, -1, -1
	for i, c := range cols {
		switch c {
		case "PartitionName":
			nameIdx = i
		case "Range":
			rangeIdx = i
		case "PartitionKey":
			keyIdx = i
		}
	}
	if nameIdx < 0 || rangeIdx < 0 {
		return st, fmt.Errorf("SHOW PARTITIONS 的结果里没有 PartitionName / Range 列（实际：%s）", strings.Join(cols, ","))
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return st, err
		}
		name := cellToStr(vals[nameIdx])
		if name == "" {
			continue
		}
		st.Partitions = append(st.Partitions, RawPartition{Name: name, Desc: cellToStr(vals[rangeIdx])})
		if keyIdx >= 0 && st.PartitionKey == "" {
			st.PartitionKey = cellToStr(vals[keyIdx])
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	if len(st.Partitions) > 0 {
		st.Method = "RANGE"
		st.Expression = st.PartitionKey
	}
	return st, nil
}

func cellToStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case string:
		return x
	default:
		return fmt.Sprintf("%v", x)
	}
}

// normDorisType 把 Doris 的 DATA_TYPE（datetimev2 / datev2 …）归一成配置里的类型名。
func normDorisType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	for _, suf := range []string{"v2", "v3"} {
		t = strings.TrimSuffix(t, suf)
	}
	return t
}
