// @author: kevinkelly
// @license: Apache Licence
// @file: run.go
// @time: 2026/10/28

package oracle

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dbtool/internal/engine"
)

// 结果截断原因。
const (
	TruncRows  = "rows"
	TruncBytes = "bytes"
)

// jsSafeInt 是 JavaScript 能精确表示的最大整数；超出的整数转成字符串，避免前端丢精度。
const jsSafeInt = 1<<53 - 1

// 默认值与硬上限。
const (
	HardMaxRows         = 100000
	HardMaxBytes        = 64 << 20
	DefaultMaxBytes     = 16 << 20
	DefaultMaxCellBytes = 64 << 10
	blobPreviewBytes    = 64 // RAW / BLOB 只显示开头这几个字节
)

// QueryLimits 是一次查询的限额；零值用默认值。
type QueryLimits struct {
	MaxRows      int           // 最多返回的行数（超出的行丢弃并标记截断）；<=0 用 HardMaxRows
	MaxBytes     int           // 结果总字节数上限；<=0 用 DefaultMaxBytes
	MaxCellBytes int           // 单个单元格的字节数上限；<=0 用 DefaultMaxCellBytes
	Timeout      time.Duration // 整个执行（含建连、核对、取数）的期限；<=0 不另设期限（由调用方的 context 决定）
}

// Shaped 是整形后的查询结果。
type Shaped struct {
	ColumnList  []string
	ColumnTypes []string
	Rows        [][]any
	Truncated   bool
	TruncatedBy string // rows / bytes
	Warnings    []string
	Notices     []string
	Elapsed     time.Duration
}

func (l QueryLimits) norm() QueryLimits {
	if l.MaxRows <= 0 || l.MaxRows > HardMaxRows {
		l.MaxRows = HardMaxRows
	}
	if l.MaxBytes <= 0 || l.MaxBytes > HardMaxBytes {
		l.MaxBytes = DefaultMaxBytes
	}
	if l.MaxCellBytes <= 0 {
		l.MaxCellBytes = DefaultMaxCellBytes
	}
	return l
}

// RunUser 执行通过 CheckUserQuery 的语句：先把会话切到目标 schema，核对语句里的对象，再在只读事务里受限取数。
func (e *Engine) RunUser(ctx context.Context, ck *Checked, lim QueryLimits) (*Shaped, error) {
	if ck == nil {
		return nil, uerr("语句没有通过检查")
	}
	lim = lim.norm()
	if lim.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, lim.Timeout)
		defer cancel()
	}
	start := time.Now()
	conn, err := e.getConn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := e.setSchema(ctx, conn, ck.DB); err != nil {
		return nil, err
	}
	if err := e.verify(ctx, conn, ck); err != nil {
		return nil, err
	}
	var sh *Shaped
	if ck.Kind == KindExplain {
		sh, err = e.runExplain(ctx, conn, ck, lim)
	} else {
		sh, err = e.runSelect(ctx, conn, ck, lim)
	}
	if err != nil {
		return nil, err
	}
	sh.Elapsed = time.Since(start)
	return sh, nil
}

// setSchema 把会话的当前 schema 切到 db，让语句里没写 schema 的表按它解析（与授权时的补全一致）。
func (e *Engine) setSchema(ctx context.Context, conn *sql.Conn, db string) error {
	if err := validSchemaName(db); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `ALTER SESSION SET CURRENT_SCHEMA = "`+db+`"`); err != nil {
		if se, ok := mapError(err, e.conn.Password).(*ServerError); ok && se.Code == 1435 {
			return uerr("schema %s 不存在，或账号看不到它", db)
		}
		return mapError(err, e.conn.Password)
	}
	return nil
}

func litList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = lit(n)
	}
	return strings.Join(q, ", ")
}

// verify 执行前用数据字典核对语句：
//   - 引用的每个表 / 视图 / 同义词确实在 ALL_OBJECTS 里（授权按“schema.表名”，不存在或看不到就拒绝，不让公有同义词把名字偷换成别的对象）；
//   - 没写 schema 的数据字典视图（免授权）没有被当前 schema 里的同名对象盖住；
//   - 语句里调用的内置函数 / 关键字没有被 schema 或 PUBLIC 里同名的自定义函数 / 包 / 类型 / 同义词盖住。
func (e *Engine) verify(ctx context.Context, conn *sql.Conn, ck *Checked) error {
	var parts []string
	if len(ck.Tables) > 0 {
		byOwner := map[string][]string{}
		for _, r := range ck.Tables {
			if err := validObjectName(r.Schema); err != nil {
				return err
			}
			if err := validObjectName(r.Name); err != nil {
				return err
			}
			byOwner[r.Schema] = append(byOwner[r.Schema], r.Name)
		}
		owners := make([]string, 0, len(byOwner))
		for o := range byOwner {
			owners = append(owners, o)
		}
		sort.Strings(owners)
		conds := make([]string, 0, len(owners))
		for _, o := range owners {
			conds = append(conds, "(owner = "+lit(o)+" AND object_name IN ("+litList(byOwner[o])+"))")
		}
		parts = append(parts, "SELECT 'T', owner, object_name FROM all_objects WHERE object_type IN ('TABLE', 'VIEW', 'MATERIALIZED VIEW', 'SYNONYM') AND ("+
			strings.Join(conds, " OR ")+")")
	}
	if len(ck.DictRefs) > 0 {
		parts = append(parts, "SELECT 'D', owner, object_name FROM all_objects WHERE owner = "+lit(ck.DB)+
			" AND object_type IN ('TABLE', 'VIEW', 'MATERIALIZED VIEW', 'SYNONYM') AND object_name IN ("+litList(ck.DictRefs)+")")
	}
	if len(ck.CallNames) > 0 {
		parts = append(parts, "SELECT 'C', owner, object_name FROM all_objects WHERE owner IN ("+lit(ck.DB)+", 'PUBLIC')"+
			" AND object_type IN ('FUNCTION', 'PACKAGE', 'TYPE', 'OPERATOR', 'SYNONYM') AND object_name IN ("+litList(ck.CallNames)+")")
	}
	for start := 0; start < len(ck.BareNames); start += bareChunk {
		end := start + bareChunk
		if end > len(ck.BareNames) {
			end = len(ck.BareNames)
		}
		in := litList(ck.BareNames[start:end])
		// 不带括号的名字：同名的函数（含指向函数的同义词）会被当成无参函数调用
		parts = append(parts, "SELECT 'B', owner, object_name FROM all_objects WHERE owner = "+lit(ck.DB)+" AND object_type = 'FUNCTION' AND object_name IN ("+in+")")
		parts = append(parts, "SELECT 'B', s.owner, s.synonym_name FROM all_synonyms s JOIN all_objects o ON o.owner = s.table_owner AND o.object_name = s.table_name"+
			" WHERE s.owner IN ("+lit(ck.DB)+", 'PUBLIC') AND o.object_type = 'FUNCTION' AND s.synonym_name IN ("+in+")")
	}
	if len(parts) == 0 {
		return nil
	}
	rows, err := conn.QueryContext(ctx, strings.Join(parts, " UNION ALL "))
	if err != nil {
		return mapError(err, e.conn.Password)
	}
	defer rows.Close()
	found := map[string]bool{}
	var shadow, collide, bare []string
	for rows.Next() {
		var k, owner, name any
		if err := rows.Scan(&k, &owner, &name); err != nil {
			return mapError(err, e.conn.Password)
		}
		ks, os, ns := asString(k), asString(owner), asString(name)
		switch ks {
		case "T":
			found[os+"."+ns] = true
		case "D":
			shadow = append(shadow, ns)
		case "C":
			collide = append(collide, ns)
		case "B":
			bare = append(bare, ns)
		}
	}
	if err := rows.Err(); err != nil {
		return mapError(err, e.conn.Password)
	}
	for _, r := range ck.Tables {
		if !found[r.Schema+"."+r.Name] {
			return uerr("对象 %s.%s 不存在，或账号没有访问它的权限（用了同义词时请写成 schema.对象名：在线查询按名字里的 schema 授权）", r.Schema, r.Name)
		}
	}
	if len(shadow) > 0 {
		sort.Strings(shadow)
		return uerr("当前 schema %s 里有和数据字典视图同名的对象 %s，查到的会是它而不是数据字典；请写成 SYS.%s", ck.DB, shadow[0], shadow[0])
	}
	if len(collide) > 0 {
		sort.Strings(collide)
		return uerr("名字 %s 与 schema / PUBLIC 里已有的函数、包、类型或同义词重名，会盖过内置函数，已拒绝", collide[0])
	}
	if len(bare) > 0 {
		sort.Strings(bare)
		return uerr("名字 %s 与 schema / PUBLIC 里的函数同名：不带括号时它会被当成无参函数来调用，已拒绝；如果它是列，请写成 表别名.%s", bare[0], bare[0])
	}
	return nil
}

// bareChunk 是核对“裸名字”时一条 IN 列表放多少个名字（Oracle 的 IN 列表最多 1000 项）。
const bareChunk = 400

// runSelect 在只读事务里执行查询并取数。
func (e *Engine) runSelect(ctx context.Context, conn *sql.Conn, ck *Checked, lim QueryLimits) (*Shaped, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, mapError(err, e.conn.Password)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET TRANSACTION READ ONLY"); err != nil {
		return nil, mapError(err, e.conn.Password)
	}
	rows, err := tx.QueryContext(ctx, ck.Stmt)
	if err != nil {
		return nil, wrapReadOnlyViolation(mapError(err, e.conn.Password))
	}
	defer rows.Close()
	sh, err := e.readRows(rows, lim)
	return sh, wrapReadOnlyViolation(err)
}

// wrapReadOnlyViolation 把只读事务的拒绝包装成 engine.ErrReadOnlyViolation（原错误仍可 errors.As 取出）：
// 语句通过了静态检查却被数据库挡下，说明检查有漏洞或有人在试探，在线查询据此记为“被拦截”。
func wrapReadOnlyViolation(err error) error {
	if err != nil && IsReadOnlyViolation(err) {
		return fmt.Errorf("%w: %w", engine.ErrReadOnlyViolation, err)
	}
	return err
}

// runExplain 执行 EXPLAIN PLAN FOR 并用 DBMS_XPLAN 取出计划文本（不在只读事务里：EXPLAIN PLAN 要往 PLAN_TABLE 写，那是会话私有的临时表）。
func (e *Engine) runExplain(ctx context.Context, conn *sql.Conn, ck *Checked, lim QueryLimits) (*Shaped, error) {
	id := fmt.Sprintf("dbtool_%d", time.Now().UnixNano())
	if _, err := conn.ExecContext(ctx, "EXPLAIN PLAN SET STATEMENT_ID = '"+id+"' FOR "+ck.Stmt); err != nil {
		return nil, mapError(err, e.conn.Password)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(cctx, "ROLLBACK")
	}()
	rows, err := conn.QueryContext(ctx, "SELECT plan_table_output FROM TABLE(DBMS_XPLAN.DISPLAY(NULL, '"+id+"', 'TYPICAL'))")
	if err != nil {
		return nil, mapError(err, e.conn.Password)
	}
	defer rows.Close()
	sh, err := e.readRows(rows, lim)
	if err != nil {
		return nil, err
	}
	sh.Notices = append(sh.Notices, "执行计划只是估算：没有真正执行这条语句")
	return sh, nil
}

// readRows 按限额取数并整形。
func (e *Engine) readRows(rows *sql.Rows, lim QueryLimits) (*Shaped, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, mapError(err, e.conn.Password)
	}
	sh := &Shaped{ColumnList: cols, ColumnTypes: make([]string, len(cols)), Rows: [][]any{}}
	if cts, err := rows.ColumnTypes(); err == nil {
		for i, ct := range cts {
			if i < len(sh.ColumnTypes) {
				sh.ColumnTypes[i] = ct.DatabaseTypeName()
			}
		}
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	total, cellsCut := 0, 0
	for rows.Next() {
		if len(sh.Rows) >= lim.MaxRows {
			sh.Truncated, sh.TruncatedBy = true, TruncRows
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, mapError(err, e.conn.Password)
		}
		row := make([]any, len(cols))
		size := 0
		for i, v := range vals {
			cv, n, cut := shapeValue(v, sh.ColumnTypes[i], lim.MaxCellBytes)
			row[i] = cv
			size += n + 4
			if cut {
				cellsCut++
			}
		}
		if total+size > lim.MaxBytes && len(sh.Rows) > 0 {
			sh.Truncated, sh.TruncatedBy = true, TruncBytes
			break
		}
		total += size
		sh.Rows = append(sh.Rows, row)
	}
	if !sh.Truncated {
		if err := rows.Err(); err != nil {
			return nil, mapError(err, e.conn.Password)
		}
	}
	if cellsCut > 0 {
		sh.Warnings = append(sh.Warnings, fmt.Sprintf("有 %d 个单元格超过 %d KB，已截断显示", cellsCut, lim.MaxCellBytes>>10))
	}
	return sh, nil
}

// shapeValue 把驱动返回的值整形成 JSON 能表示的值，并返回它占用的字节数、是否被截断。
func shapeValue(v any, typ string, maxCell int) (any, int, bool) {
	switch x := v.(type) {
	case nil:
		return nil, 0, false
	case string:
		s, cut := cutString(x, maxCell)
		return s, len(s), cut
	case []byte:
		n := len(x)
		show := x
		if len(show) > blobPreviewBytes {
			show = show[:blobPreviewBytes]
		}
		s := "0x" + strings.ToUpper(hex.EncodeToString(show))
		if n > len(show) {
			s += fmt.Sprintf("…（共 %d 字节）", n)
		}
		return s, len(s), false
	case int64:
		if x > jsSafeInt || x < -jsSafeInt {
			str := strconv.FormatInt(x, 10)
			return str, len(str), false
		}
		return x, 8, false
	case int32:
		return int64(x), 8, false
	case int:
		return int64(x), 8, false
	case bool:
		return x, 8, false
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Sprint(x), 8, false
		}
		return x, 8, false
	case float32:
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Sprint(f), 8, false
		}
		return f, 8, false
	case time.Time:
		s := formatTime(x, typ)
		return s, len(s), false
	}
	s, cut := cutString(fmt.Sprint(v), maxCell)
	return s, len(s), cut
}

// formatTime 按列类型格式化时间：DATE 到秒，TIMESTAMP 带小数秒，带时区的类型带上偏移。
func formatTime(t time.Time, typ string) string {
	up := strings.ToUpper(typ)
	switch {
	case strings.Contains(up, "TIME ZONE") || strings.Contains(up, "TZ"):
		return t.Format("2006-01-02 15:04:05.999999999 -07:00")
	case strings.HasPrefix(up, "TIMESTAMP"):
		return t.Format("2006-01-02 15:04:05.999999999")
	}
	return t.Format("2006-01-02 15:04:05")
}

// cutString 把超过 max 字节的字符串截到 UTF-8 字符边界并加省略号。
func cutString(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…", true
}
