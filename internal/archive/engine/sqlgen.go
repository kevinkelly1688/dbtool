// @author: kevinkelly
// @license: Apache Licence
// @file: sqlgen.go
// @time: 2026/10/28

package engine

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 引擎实际执行的 SQL 都由这里生成：标识符一律反引号，值一律占位符；WHERE 来自 buildWhere（已规范化）。

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// tupleExpr 返回主键元组表达式：单列 `a`，多列 (`a`, `b`)。
func tupleExpr(pk []string) string {
	if len(pk) == 1 {
		return quoteIdent(pk[0])
	}
	return "(" + quoteIdents(pk) + ")"
}

func tuplePlaceholder(n int) string {
	if n == 1 {
		return "?"
	}
	return "(" + placeholders(n) + ")"
}

// buildReadSQL 生成一批读取语句。参数顺序：[断点 n 个]，LIMIT。始终带 LIMIT ?，使驱动走服务端预编译（二进制协议，数值类型不失真）。
// unordered=true（归档并删除 / 仅清理）：不排序、没有断点，只取当前符合条件的前 LIMIT 行——读完备份校验再删掉，下一批自然取到剩下的。
// 没有 ORDER BY 就不会有 filesort：在几十亿行的表上不会因为优化器选错索引而每批排序几亿行。
func buildReadSQL(db, table, where string, pk []string, withAfter, unordered bool) string {
	if unordered {
		return fmt.Sprintf("SELECT * FROM %s WHERE (%s) LIMIT ?", qualified(db, table), where)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "SELECT * FROM %s WHERE (%s)", qualified(db, table), where)
	if withAfter {
		fmt.Fprintf(&b, " AND %s > %s", tupleExpr(pk), tuplePlaceholder(len(pk)))
	}
	fmt.Fprintf(&b, " ORDER BY %s LIMIT ?", quoteIdents(pk))
	return b.String()
}

// buildDeleteSQL 生成删除语句：主键 IN (...) 并重复归档条件。nrows 为本条语句里的主键个数。
func buildDeleteSQL(db, table, where string, pk []string, nrows int) string {
	list := make([]string, nrows)
	for i := range list {
		list[i] = tuplePlaceholder(len(pk))
	}
	return fmt.Sprintf("DELETE FROM %s WHERE %s IN (%s) AND (%s)", qualified(db, table), tupleExpr(pk), strings.Join(list, ", "), where)
}

func buildInsertSQL(verb, db, table string, cols []string, nrows int) string {
	row := "(" + placeholders(len(cols)) + ")"
	rows := make([]string, nrows)
	for i := range rows {
		rows[i] = row
	}
	return fmt.Sprintf("%s INTO %s (%s) VALUES %s", verb, qualified(db, table), quoteIdents(cols), strings.Join(rows, ", "))
}

// buildSelectByPK 按主键集合读取指定列（对比已存在行 / 校验用）。
func buildSelectByPK(db, table string, cols, pk []string, nrows int) string {
	list := make([]string, nrows)
	for i := range list {
		list[i] = tuplePlaceholder(len(pk))
	}
	return fmt.Sprintf("SELECT %s FROM %s WHERE %s IN (%s)", quoteIdents(cols), qualified(db, table), tupleExpr(pk), strings.Join(list, ", "))
}

func buildCountByPK(db, table string, pk []string, nrows int) string {
	list := make([]string, nrows)
	for i := range list {
		list[i] = tuplePlaceholder(len(pk))
	}
	return fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s IN (%s)", qualified(db, table), tupleExpr(pk), strings.Join(list, ", "))
}

// buildRangeSQL 恢复用：读备份表中 Min <= pk <= Max（且 > After）的行。
func buildRangeSQL(db, table string, pk []string, withAfter bool) string {
	var b strings.Builder
	t := tupleExpr(pk)
	ph := tuplePlaceholder(len(pk))
	fmt.Fprintf(&b, "SELECT * FROM %s WHERE %s >= %s AND %s <= %s", qualified(db, table), t, ph, t, ph)
	if withAfter {
		fmt.Fprintf(&b, " AND %s > %s", t, ph)
	}
	fmt.Fprintf(&b, " ORDER BY %s LIMIT ?", quoteIdents(pk))
	return b.String()
}

// BuildSQLPreview 生成“将执行的 SQL”（示例参数；仅用于展示）。pkCols / cols 可为空（取不到表结构时），此时用占位名。
// Spec.Dest 的表名取 Dest.Table，为空则按 Template 用 now 展开。
//
// 取不到表结构时（没有 info），归档并删除 / 仅清理按“不排序读取”展示；主键含字符串等不能在本地比大小的列时，实际运行仍按主键排序读取
// （见 readUnordered），试运行里的预览（带表结构）是准确的。
func BuildSQLPreview(spec Spec, pkCols, cols []string, now time.Time) SQLPreview {
	return buildPreview(spec, pkCols, cols, now, isDeleting(spec.Mode))
}

// buildPreviewFor 是带表结构的预览：读取方式按主键列的类型决定。
func buildPreviewFor(spec Spec, info TableInfo, cols []string, now time.Time) SQLPreview {
	return buildPreview(spec, info.PrimaryKey, cols, now, readUnordered(spec.Mode, info))
}

func buildPreview(spec Spec, pkCols, cols []string, now time.Time, unordered bool) SQLPreview {
	var p SQLPreview
	where, err := buildWhere(spec)
	if err != nil {
		where = fmt.Sprintf("/* 条件无效：%s */", err.Error())
	}
	if len(pkCols) == 0 {
		pkCols = []string{"<主键>"}
	}
	colList := "*"
	if len(cols) > 0 {
		colList = quoteIdents(cols)
	}
	src := qualified(spec.DB, spec.Table)
	pkq := make([]string, len(pkCols))
	for i, c := range pkCols {
		pkq[i] = quoteIdent(c)
	}
	after := "?"
	if len(spec.Checkpoint) == len(pkCols) && len(spec.Checkpoint) > 0 {
		lits := make([]string, len(spec.Checkpoint))
		for i, v := range spec.Checkpoint {
			lits[i] = sqlLiteralString(v)
		}
		after = lits[0]
		if len(lits) > 1 {
			after = "(" + strings.Join(lits, ", ") + ")"
		}
	} else if len(pkCols) > 1 {
		after = "(" + placeholders(len(pkCols)) + ")"
	}
	pkExpr := pkq[0]
	if len(pkq) > 1 {
		pkExpr = "(" + strings.Join(pkq, ", ") + ")"
	}
	batch := spec.Params.BatchSize
	if batch == 0 {
		batch = defBatch
	}
	if unordered {
		p.Select = fmt.Sprintf("SELECT %s FROM %s\n WHERE %s\n LIMIT %d; -- 不排序、没有断点：取走当前符合条件的前几行，备份校验并删除后，下一批自然取到剩下的",
			colList, src, where, batch)
	} else {
		p.Select = fmt.Sprintf("SELECT %s FROM %s\n WHERE %s\n   AND %s > %s -- 断点：上一批最大主键（首批无此条件）\n ORDER BY %s\n LIMIT %d;",
			colList, src, where, pkExpr, after, strings.Join(pkq, ", "), batch)
	}
	if spec.Dest != nil {
		t := spec.Dest.Table
		if t == "" {
			t = ExpandTableName(spec.Dest.Template, spec.Table, now)
		}
		vals := "(?, ?, …)"
		if len(cols) > 0 {
			vals = "(" + placeholders(len(cols)) + ")"
		}
		p.Insert = fmt.Sprintf("-- 目标：实例 #%d\nINSERT INTO %s (%s)\nVALUES %s, …; -- 本批读到的行；主键已存在且内容一致视为“已存在”，不一致 = 校验失败",
			spec.Dest.InstanceID, qualified(spec.Dest.DB, t), colList, vals)
	}
	if isDeleting(spec.Mode) {
		p.Delete = fmt.Sprintf("DELETE FROM %s\n WHERE %s IN (?, ?, …) -- 本批读到的主键，备份校验通过后才执行\n   AND (%s); -- 重复一遍归档条件，避免删到期间被改过的行",
			src, pkExpr, where)
	}
	return p
}

// sqlLiteralString 把值按字符串字面量输出（仅用于展示 / 备份文件，不用于执行）。
func sqlLiteralString(s string) string {
	return "'" + escapeSQLString(s) + "'"
}

func escapeSQLString(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case 0:
			b.WriteString(`\0`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '"':
			b.WriteString(`\"`)
		case 0x1a:
			b.WriteString(`\Z`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// interpolateArgs 把 query 里引号之外的 ? 依次替换成 args 的字面量（只支持整数和字符串，字符串按 MySQL 规则转义）。
// 仅用于“服务器不支持预编译的语句”（个别版本的 EXPLAIN）的降级路径；引擎自己生成的语句，? 一定只出现在引号之外。
func interpolateArgs(query string, args []any) (string, error) {
	var b strings.Builder
	n := 0
	inSingle, inBacktick := false, false
	rs := []rune(query)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case inSingle:
			b.WriteRune(c)
			if c == '\'' {
				if i+1 < len(rs) && rs[i+1] == '\'' { // '' 是转义
					b.WriteRune('\'')
					i++
				} else {
					inSingle = false
				}
			}
		case inBacktick:
			b.WriteRune(c)
			if c == '`' {
				inBacktick = false
			}
		case c == '\'':
			inSingle = true
			b.WriteRune(c)
		case c == '`':
			inBacktick = true
			b.WriteRune(c)
		case c == '?':
			if n >= len(args) {
				return "", fmt.Errorf("参数个数不足")
			}
			switch v := args[n].(type) {
			case int:
				b.WriteString(strconv.Itoa(v))
			case int32:
				b.WriteString(strconv.FormatInt(int64(v), 10))
			case int64:
				b.WriteString(strconv.FormatInt(v, 10))
			case uint32:
				b.WriteString(strconv.FormatUint(uint64(v), 10))
			case uint64:
				b.WriteString(strconv.FormatUint(v, 10))
			case string:
				b.WriteString(sqlLiteralString(v))
			case []byte:
				b.WriteString(sqlLiteralString(string(v)))
			default:
				return "", fmt.Errorf("不支持的参数类型 %T", args[n])
			}
			n++
		default:
			b.WriteRune(c)
		}
	}
	if n != len(args) {
		return "", fmt.Errorf("参数个数过多")
	}
	return b.String(), nil
}
