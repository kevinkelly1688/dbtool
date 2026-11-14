// @author: kevinkelly
// @license: Apache Licence
// @file: meta.go
// @time: 2026/10/28

package dm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// 库表树 / 数据字典用的元数据：全部走 ALL_* 视图（达梦提供与 Oracle 兼容的静态数据字典视图，账号能看到的对象），名字都是库里的真实名字（大写居多）。

// TableInfo 是 schema 里一个表 / 视图 / 物化视图的概要。
type TableInfo struct {
	Name    string
	Comment string
	Kind    string // table / view / matview
	IsView  bool
	Rows    *int64 // 统计信息里的行数估算；没收集过统计信息为 nil
}

// ColumnInfo 是一列的信息。
type ColumnInfo struct {
	Name         string
	Type         string // 已格式化：DECIMAL(10,2)、VARCHAR(50)、TIMESTAMP(6) …
	Nullable     bool
	Comment      string
	IsPrimaryKey bool
	Position     int
}

// IndexInfo 是一条索引。
type IndexInfo struct {
	Name    string
	Unique  bool
	Columns []string // 按列序
}

// TableInfos 返回 schema 里账号看得到的全部表 / 视图 / 物化视图。
func (e *Engine) TableInfos(ctx context.Context, db string) ([]TableInfo, error) {
	if err := validSchemaName(db); err != nil {
		return nil, err
	}
	objs, err := e.queryAll(ctx, "SELECT object_name, object_type FROM all_objects WHERE owner = "+lit(db)+
		" AND object_type IN ('TABLE', 'VIEW', 'MATERIALIZED VIEW', 'MVIEW') ORDER BY object_name")
	if err != nil {
		return nil, err
	}
	comments := e.stringMap(ctx, "SELECT table_name, comments FROM all_tab_comments WHERE owner = "+lit(db)+" AND comments IS NOT NULL")
	rowCounts := map[string]int64{}
	if rs, err := e.queryAll(ctx, "SELECT table_name, num_rows FROM all_tables WHERE owner = "+lit(db)+" AND num_rows IS NOT NULL"); err == nil {
		for _, r := range rs {
			if len(r) >= 2 {
				if n, ok := asInt(r[1]); ok {
					rowCounts[asString(r[0])] = n
				}
			}
		}
	}
	byName := map[string]*TableInfo{}
	var order []string
	for _, r := range objs {
		if len(r) < 2 {
			continue
		}
		name, typ := asString(r[0]), asString(r[1])
		ti, ok := byName[name]
		if !ok {
			ti = &TableInfo{Name: name, Kind: "table", Comment: comments[name]}
			byName[name] = ti
			order = append(order, name)
		}
		switch typ {
		case "VIEW":
			ti.Kind, ti.IsView = "view", true
		case "MATERIALIZED VIEW", "MVIEW": // 物化视图可能在 ALL_OBJECTS 里还有一行同名的 TABLE（容器表）：以物化视图为准
			ti.Kind, ti.IsView = "matview", true
		}
	}
	out := make([]TableInfo, 0, len(order))
	for _, n := range order {
		ti := byName[n]
		if !ti.IsView {
			if c, ok := rowCounts[n]; ok {
				v := c
				ti.Rows = &v
			}
		}
		out = append(out, *ti)
	}
	return out, nil
}

// ColumnInfos 返回表 / 视图的列（按列序），带注释与主键标记。
func (e *Engine) ColumnInfos(ctx context.Context, db, table string) ([]ColumnInfo, error) {
	if err := validSchemaName(db); err != nil {
		return nil, err
	}
	if err := validObjectName(table); err != nil {
		return nil, err
	}
	where := " FROM all_tab_columns WHERE owner = " + lit(db) + " AND table_name = " + lit(table) + " ORDER BY column_id"
	// char_length / char_used 不一定都有；读不到就退回只用 data_length
	rows, err := e.queryAll(ctx, "SELECT column_name, data_type, data_length, data_precision, data_scale, nullable, column_id, char_length, char_used"+where)
	withChars := err == nil
	if err != nil {
		rows, err = e.queryAll(ctx, "SELECT column_name, data_type, data_length, data_precision, data_scale, nullable, column_id"+where)
		if err != nil {
			return nil, err
		}
	}
	comments := e.stringMap(ctx, "SELECT column_name, comments FROM all_col_comments WHERE owner = "+lit(db)+" AND table_name = "+lit(table)+" AND comments IS NOT NULL")
	pk := map[string]bool{}
	if ps, err := e.strings(ctx, "SELECT cc.column_name FROM all_constraints c JOIN all_cons_columns cc ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name"+
		" WHERE c.owner = "+lit(db)+" AND c.table_name = "+lit(table)+" AND c.constraint_type = 'P'"); err == nil {
		for _, c := range ps {
			pk[c] = true
		}
	}
	out := make([]ColumnInfo, 0, len(rows))
	for _, r := range rows {
		if len(r) < 7 {
			continue
		}
		name := asString(r[0])
		var charLen *int64
		charUsed := ""
		if withChars && len(r) >= 9 {
			if n, ok := asInt(r[7]); ok {
				charLen = &n
			}
			charUsed = asString(r[8])
		}
		pos, _ := asInt(r[6])
		out = append(out, ColumnInfo{
			Name:         name,
			Type:         formatType(asString(r[1]), asIntPtr(r[2]), asIntPtr(r[3]), asIntPtr(r[4]), charLen, charUsed),
			Nullable:     asString(r[5]) != "N",
			Comment:      comments[name],
			IsPrimaryKey: pk[name],
			Position:     int(pos),
		})
	}
	return out, nil
}

// ColumnRow 是 schema 里一列的概要（脱敏字段目录用）。
type ColumnRow struct{ Table, Column, Type, Comment string }

// ColumnRows 一次取出 schema 里所有表 / 视图的列（按表名、列序），最多 max 行（超出的不取，调用方据此标记“已截断”：多取一行判断）。
func (e *Engine) ColumnRows(ctx context.Context, db string, max int) ([]ColumnRow, error) {
	if err := validSchemaName(db); err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 1
	}
	from := " FROM all_tab_columns c LEFT JOIN all_col_comments m ON m.owner = c.owner AND m.table_name = c.table_name AND m.column_name = c.column_name" +
		" WHERE c.owner = " + lit(db) + " ORDER BY c.table_name, c.column_id) WHERE ROWNUM <= " + strconv.Itoa(max)
	// char_length / char_used 不一定都有；读不到就退回只用 data_length
	rows, err := e.queryAll(ctx, "SELECT * FROM (SELECT c.table_name, c.column_name, c.data_type, c.data_length, c.data_precision, c.data_scale, m.comments, c.char_length, c.char_used"+from)
	withChars := err == nil
	if err != nil {
		rows, err = e.queryAll(ctx, "SELECT * FROM (SELECT c.table_name, c.column_name, c.data_type, c.data_length, c.data_precision, c.data_scale, m.comments"+from)
		if err != nil {
			return nil, err
		}
	}
	out := make([]ColumnRow, 0, len(rows))
	for _, r := range rows {
		if len(r) < 7 {
			continue
		}
		var charLen *int64
		charUsed := ""
		if withChars && len(r) >= 9 {
			if n, ok := asInt(r[7]); ok {
				charLen = &n
			}
			charUsed = asString(r[8])
		}
		out = append(out, ColumnRow{Table: asString(r[0]), Column: asString(r[1]),
			Type: formatType(asString(r[2]), asIntPtr(r[3]), asIntPtr(r[4]), asIntPtr(r[5]), charLen, charUsed), Comment: asString(r[6])})
	}
	return out, nil
}

// Indexes 返回表上的索引（含主键 / 唯一约束背后的索引）。
func (e *Engine) Indexes(ctx context.Context, db, table string) ([]IndexInfo, error) {
	if err := validSchemaName(db); err != nil {
		return nil, err
	}
	if err := validObjectName(table); err != nil {
		return nil, err
	}
	rows, err := e.queryAll(ctx, "SELECT i.index_name, i.uniqueness, ic.column_name, ic.column_position FROM all_indexes i"+
		" JOIN all_ind_columns ic ON ic.index_owner = i.owner AND ic.index_name = i.index_name"+
		" WHERE i.table_owner = "+lit(db)+" AND i.table_name = "+lit(table)+" ORDER BY i.index_name, ic.column_position")
	if err != nil {
		return nil, err
	}
	var out []IndexInfo
	idx := map[string]int{}
	for _, r := range rows {
		if len(r) < 3 {
			continue
		}
		name := asString(r[0])
		i, ok := idx[name]
		if !ok {
			out = append(out, IndexInfo{Name: name, Unique: asString(r[1]) == "UNIQUE"})
			i = len(out) - 1
			idx[name] = i
		}
		out[i].Columns = append(out[i].Columns, asString(r[2]))
	}
	return out, nil
}

// ColumnHit 是按字段名 / 注释搜索的一条命中。
type ColumnHit struct{ Table, Column, Comment string }

// likeEscape 转义 LIKE 里的通配符（配合 ESCAPE '\'）。
var likeEscape = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// SearchColumns 按字段名 / 字段注释模糊搜索 schema 里的列（不区分大小写，最多 500 条）。
func (e *Engine) SearchColumns(ctx context.Context, db, kw string) ([]ColumnHit, error) {
	if err := validSchemaName(db); err != nil {
		return nil, err
	}
	kw = strings.TrimSpace(kw)
	if kw == "" {
		return nil, nil
	}
	pat := lit("%" + likeEscape.Replace(strings.ToUpper(kw)) + "%")
	rows, err := e.queryAll(ctx, "SELECT * FROM (SELECT c.table_name, c.column_name, m.comments FROM all_tab_columns c"+
		" LEFT JOIN all_col_comments m ON m.owner = c.owner AND m.table_name = c.table_name AND m.column_name = c.column_name"+
		" WHERE c.owner = "+lit(db)+" AND (UPPER(c.column_name) LIKE "+pat+" ESCAPE '\\' OR UPPER(m.comments) LIKE "+pat+" ESCAPE '\\')"+
		" ORDER BY c.table_name, c.column_id) WHERE ROWNUM <= 500")
	if err != nil {
		return nil, err
	}
	out := make([]ColumnHit, 0, len(rows))
	for _, r := range rows {
		if len(r) >= 3 {
			out = append(out, ColumnHit{Table: asString(r[0]), Column: asString(r[1]), Comment: asString(r[2])})
		}
	}
	return out, nil
}

// CreateStatement 返回对象的建表 / 建视图语句：优先用 DBMS_METADATA.GET_DDL（要有对象的访问权限），读不到就按列信息拼一个近似的建表语句。
func (e *Engine) CreateStatement(ctx context.Context, db, name string, view bool) (string, error) {
	if err := validSchemaName(db); err != nil {
		return "", err
	}
	if err := validObjectName(name); err != nil {
		return "", err
	}
	typ := "TABLE"
	if view {
		typ = "VIEW"
	}
	rows, err := e.queryAll(ctx, "SELECT DBMS_METADATA.GET_DDL("+lit(typ)+", "+lit(name)+", "+lit(db)+") FROM DUAL")
	if err == nil && len(rows) > 0 && len(rows[0]) > 0 {
		if s := strings.TrimSpace(asString(rows[0][0])); s != "" {
			return s, nil
		}
	}
	if !view { // 达梦自己的建表语句函数
		if rows, err2 := e.queryAll(ctx, "SELECT SP_TABLEDEF("+lit(db)+", "+lit(name)+") FROM DUAL"); err2 == nil && len(rows) > 0 && len(rows[0]) > 0 {
			if s := strings.TrimSpace(asString(rows[0][0])); s != "" {
				return s, nil
			}
		}
	}
	if view {
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("读不到视图 %s.%s 的定义", db, name)
	}
	cols, cerr := e.ColumnInfos(ctx, db, name)
	if cerr != nil {
		return "", cerr
	}
	return approxDDL(db, name, cols), nil
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// approxDDL 按列信息拼一个近似的建表语句（没有索引、其它约束与存储参数）。
func approxDDL(db, name string, cols []ColumnInfo) string {
	var b strings.Builder
	b.WriteString("-- 近似的建表语句（读不到 DBMS_METADATA / SP_TABLEDEF，按列信息拼出，不含索引、其它约束与存储参数）\n")
	fmt.Fprintf(&b, "CREATE TABLE %s.%s (\n", quoteIdent(db), quoteIdent(name))
	parts := make([]string, 0, len(cols)+1)
	var pk []string
	for _, c := range cols {
		p := "  " + quoteIdent(c.Name) + " " + c.Type
		if !c.Nullable {
			p += " NOT NULL"
		}
		parts = append(parts, p)
		if c.IsPrimaryKey {
			pk = append(pk, quoteIdent(c.Name))
		}
	}
	if len(pk) > 0 {
		parts = append(parts, "  PRIMARY KEY ("+strings.Join(pk, ", ")+")")
	}
	b.WriteString(strings.Join(parts, ",\n"))
	b.WriteString("\n)")
	return b.String()
}

// stringMap 执行“键, 值”两列的元数据查询；出错（没有权限、视图不存在）就返回空 map，不影响主流程。
func (e *Engine) stringMap(ctx context.Context, q string) map[string]string {
	out := map[string]string{}
	rows, err := e.queryAll(ctx, q)
	if err != nil {
		return out
	}
	for _, r := range rows {
		if len(r) >= 2 && r[0] != nil && r[1] != nil {
			out[asString(r[0])] = asString(r[1])
		}
	}
	return out
}

// formatType 把 ALL_TAB_COLUMNS 里的类型信息整理成人看得懂的写法（达梦的类型名：VARCHAR、DECIMAL、INT、BIGINT …）。
func formatType(dt string, length, prec, scale, charLen *int64, charUsed string) string {
	switch dt {
	case "NUMBER", "DECIMAL", "DEC", "NUMERIC":
		switch {
		case prec != nil && scale != nil && *scale != 0:
			return fmt.Sprintf("%s(%d,%d)", dt, *prec, *scale)
		case prec != nil:
			return fmt.Sprintf("%s(%d)", dt, *prec)
		}
		return dt
	case "FLOAT":
		if prec != nil {
			return fmt.Sprintf("FLOAT(%d)", *prec)
		}
		return "FLOAT"
	case "VARCHAR", "VARCHAR2", "NVARCHAR2", "CHAR", "CHARACTER", "NCHAR":
		if charLen != nil && *charLen > 0 {
			return fmt.Sprintf("%s(%d)", dt, *charLen)
		}
		if length != nil {
			return fmt.Sprintf("%s(%d)", dt, *length)
		}
	case "BINARY", "VARBINARY", "RAW":
		if length != nil {
			return fmt.Sprintf("%s(%d)", dt, *length)
		}
	case "TIMESTAMP", "DATETIME", "TIME": // 达梦把小数秒位数放在 DATA_SCALE 里
		if scale != nil {
			return fmt.Sprintf("%s(%d)", dt, *scale)
		}
	}
	return dt
}

// asInt 把驱动返回的数值转成 int64：达梦的 INT / SMALLINT / TINYINT / BIGINT 是定长整数，DECIMAL 是 *DmDecimal（有 String 方法）。
func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case int64:
		return x, true
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case int16:
		return int64(x), true
	case int8:
		return int64(x), true
	case uint8:
		return int64(x), true
	case float64:
		return int64(x), true
	case float32:
		return int64(x), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			if f, ferr := strconv.ParseFloat(strings.TrimSpace(x), 64); ferr == nil {
				return int64(f), true
			}
			return 0, false
		}
		return n, true
	case []byte:
		return asInt(string(x))
	case fmt.Stringer:
		return asInt(x.String())
	}
	return 0, false
}

func asIntPtr(v any) *int64 {
	if n, ok := asInt(v); ok {
		return &n
	}
	return nil
}
