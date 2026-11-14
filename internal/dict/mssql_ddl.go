// @author: kevinkelly
// @license: Apache Licence
// @file: mssql_ddl.go
// @time: 2026/10/28

package dict

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// SQL Server 的建表语句：没有 SHOW CREATE TABLE，按系统目录视图（sys.columns / sys.indexes / sys.foreign_keys /
// sys.check_constraints / sys.triggers）还原，格式如下：
//
//	CREATE TABLE [dbo].[t]
//	(
//	    [id] int IDENTITY(1,1) NOT NULL,
//	    ...
//	)
//
//	-- INDEXES
//	ALTER TABLE [dbo].[t] ADD CONSTRAINT [PK_t] PRIMARY KEY ([id]);
//	CREATE UNIQUE NONCLUSTERED INDEX [ux] ON [dbo].[t] ([a]);
//
// 还原不是精确的：不含文件组 / 分区方案 / 压缩 / 填充因子 / 统计信息 / 加密列等物理选项，所以只供查看参考，不保证可直接执行。
// 任何一类对象取不到（常见原因是没有 VIEW DEFINITION 权限）不让整个详情失败，只在语句里留一行注释。
// 所有查询都用 object_id 作参数，不拼接用户输入；标识符输出前按 [ ] 转义。

const msDDLTitle = "建表语句（含索引、触发器）"
const msDDLNote = "由系统目录还原，仅供查看参考，不保证可直接执行。"

// msQuote 把标识符写成 [name]，名字里的 ] 加倍。
func msQuote(s string) string { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }

func msQuoteList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = msQuote(c)
	}
	return strings.Join(q, ", ")
}

func truthy(v any) bool {
	f, ok := toFloat(v)
	return ok && f != 0
}

// createTableSection 返回“建表语句”小节；整体失败时返回带原因的说明小节。
func (d mssqlDialect) createTableSection(ctx context.Context, q Querier, id int64, schema, name string) Section {
	ddl, err := d.buildCreateTable(ctx, q, id, schema, name)
	if err != nil {
		return noteSec(msDDLTitle, fmt.Sprintf("获取失败：%s", err.Error()))
	}
	if ddl == "" {
		return noteSec(msDDLTitle, "没有取到列定义（可能没有 VIEW DEFINITION 权限）；列与索引信息见上。")
	}
	s := codeSec(msDDLTitle, ddl)
	s.Note = msDDLNote
	return s
}

func (d mssqlDialect) buildCreateTable(ctx context.Context, q Querier, id int64, schema, name string) (string, error) {
	table := msQuote(schema) + "." + msQuote(name)

	cols, err := q.Query(ctx, `SELECT c.name, TYPE_NAME(c.user_type_id), c.max_length, c.precision, c.scale, c.is_nullable, c.is_identity, c.collation_name,
cc.definition, cc.is_persisted, dc.name, dc.definition, idc.seed_value, idc.increment_value
FROM sys.columns c
LEFT JOIN sys.computed_columns cc ON cc.object_id = c.object_id AND cc.column_id = c.column_id
LEFT JOIN sys.default_constraints dc ON dc.parent_object_id = c.object_id AND dc.parent_column_id = c.column_id
LEFT JOIN sys.identity_columns idc ON idc.object_id = c.object_id AND idc.column_id = c.column_id
WHERE c.object_id = @p1 ORDER BY c.column_id`, id)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, r := range cols.Data {
		if len(r) < 14 {
			continue
		}
		lines = append(lines, "    "+msColumnDDL(r))
	}
	if len(lines) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("CREATE TABLE " + table + "\n(\n" + strings.Join(lines, ",\n") + "\n)\n")

	// 索引 / 主键 / 唯一约束
	var idxLines []string
	if idx, err := d.indexDDL(ctx, q, id, table); err != nil {
		idxLines = append(idxLines, "-- 索引信息获取失败："+oneLine(err.Error())) // i18n-ignore: 写进还原出的 DDL 文本里的注释，属于生成的代码，不随界面语言变化
	} else {
		idxLines = idx
	}
	if len(idxLines) > 0 {
		b.WriteString("\n-- INDEXES\n" + strings.Join(idxLines, "\n") + "\n")
	}

	if fks, err := d.foreignKeyDDL(ctx, q, id, table); err != nil {
		b.WriteString("\n-- FOREIGN KEYS\n-- 外键信息获取失败：" + oneLine(err.Error()) + "\n") // i18n-ignore: DDL 文本里的注释
	} else if len(fks) > 0 {
		b.WriteString("\n-- FOREIGN KEYS\n" + strings.Join(fks, "\n") + "\n")
	}

	if ck, err := q.Query(ctx, "SELECT name, definition, is_disabled FROM sys.check_constraints WHERE parent_object_id = @p1 ORDER BY name", id); err != nil {
		b.WriteString("\n-- CHECK CONSTRAINTS\n-- 检查约束获取失败：" + oneLine(err.Error()) + "\n") // i18n-ignore: DDL 文本里的注释
	} else if len(ck.Data) > 0 {
		b.WriteString("\n-- CHECK CONSTRAINTS\n")
		for _, r := range ck.Data {
			if len(r) < 3 {
				continue
			}
			b.WriteString("ALTER TABLE " + table + " ADD CONSTRAINT " + msQuote(str(r[0])) + " CHECK " + str(r[1]) + ";\n")
			if truthy(r[2]) {
				b.WriteString("ALTER TABLE " + table + " NOCHECK CONSTRAINT " + msQuote(str(r[0])) + ";\n")
			}
		}
	}

	if trg, err := q.Query(ctx, "SELECT tr.name, OBJECT_DEFINITION(tr.object_id), tr.is_disabled FROM sys.triggers tr WHERE tr.parent_id = @p1 AND tr.parent_class = 1 ORDER BY tr.name", id); err != nil {
		b.WriteString("\n-- TRIGGERS\n-- 触发器获取失败：" + oneLine(err.Error()) + "\n") // i18n-ignore: DDL 文本里的注释
	} else if len(trg.Data) > 0 {
		b.WriteString("\n-- TRIGGERS\n")
		for _, r := range trg.Data {
			if len(r) < 2 {
				continue
			}
			def := strings.TrimSpace(str(r[1]))
			if def == "" {
				b.WriteString("-- 触发器 " + msQuote(str(r[0])) + " 没有取到定义（没有 VIEW DEFINITION 权限，或已加密）\n") // i18n-ignore: DDL 文本里的注释
				continue
			}
			b.WriteString(def + "\n")
			if len(r) > 2 && truthy(r[2]) {
				b.WriteString("DISABLE TRIGGER " + msQuote(str(r[0])) + " ON " + table + ";\n")
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}

// msColumnDDL 把 buildCreateTable 的列查询结果（14 列）写成一行列定义。
func msColumnDDL(r []any) string {
	name, typ := str(r[0]), str(r[1])
	maxLen, _ := toFloat(r[2])
	prec, _ := toFloat(r[3])
	scale, _ := toFloat(r[4])
	if def := str(r[8]); def != "" { // 计算列：只有表达式，没有类型 / 可空
		s := msQuote(name) + " AS " + def
		if truthy(r[9]) {
			s += " PERSISTED"
		}
		return s
	}
	s := msQuote(name) + " " + msFormatType(typ, int(maxLen), int(prec), int(scale))
	if coll := str(r[7]); coll != "" {
		s += " COLLATE " + coll
	}
	if truthy(r[6]) {
		seed, inc := str(r[12]), str(r[13])
		if seed == "" {
			seed = "1"
		}
		if inc == "" {
			inc = "1"
		}
		s += " IDENTITY(" + seed + "," + inc + ")"
	}
	if truthy(r[5]) {
		s += " NULL"
	} else {
		s += " NOT NULL"
	}
	if def := str(r[11]); def != "" {
		if cn := str(r[10]); cn != "" {
			s += " CONSTRAINT " + msQuote(cn)
		}
		s += " DEFAULT " + def
	}
	return s
}

type msIndexCol struct {
	name     string
	desc     bool
	included bool
}

type msIndex struct {
	id       int
	name     string
	typ      int
	unique   bool
	primary  bool
	uniqueCs bool
	filter   string
	cols     []msIndexCol
}

// indexDDL 返回索引 / 主键 / 唯一约束的语句（每条一行）。
func (d mssqlDialect) indexDDL(ctx context.Context, q Querier, id int64, table string) ([]string, error) {
	rs, err := q.Query(ctx, `SELECT i.index_id, i.name, i.type, i.is_unique, i.is_primary_key, i.is_unique_constraint, ISNULL(i.filter_definition, ''),
COL_NAME(ic.object_id, ic.column_id), ic.is_descending_key, ic.is_included_column
FROM sys.indexes i JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
WHERE i.object_id = @p1 AND i.index_id > 0 AND i.is_hypothetical = 0
ORDER BY i.index_id, ic.is_included_column, ic.key_ordinal, ic.index_column_id`, id)
	if err != nil {
		return nil, err
	}
	byID := map[int]*msIndex{}
	var order []int
	for _, r := range rs.Data {
		if len(r) < 10 {
			continue
		}
		f, _ := toFloat(r[0])
		iid := int(f)
		ix, ok := byID[iid]
		if !ok {
			t, _ := toFloat(r[2])
			ix = &msIndex{id: iid, name: str(r[1]), typ: int(t), unique: truthy(r[3]), primary: truthy(r[4]), uniqueCs: truthy(r[5]), filter: str(r[6])}
			byID[iid] = ix
			order = append(order, iid)
		}
		ix.cols = append(ix.cols, msIndexCol{name: str(r[7]), desc: truthy(r[8]), included: truthy(r[9])})
	}
	sort.Ints(order)
	var out []string
	for _, iid := range order {
		out = append(out, msIndexStatement(byID[iid], table))
	}
	return out, nil
}

func msKeyCols(cols []msIndexCol) (key, incl []string) {
	for _, c := range cols {
		s := msQuote(c.name)
		if c.desc && !c.included {
			s += " DESC"
		}
		if c.included {
			incl = append(incl, msQuote(c.name))
		} else {
			key = append(key, s)
		}
	}
	return key, incl
}

func msIndexStatement(ix *msIndex, table string) string {
	key, incl := msKeyCols(ix.cols)
	clustered := ix.typ == 1 || ix.typ == 5
	switch {
	case ix.primary: // 主键默认就是聚集；只在非聚集时写出来
		kind := ""
		if !clustered {
			kind = " NONCLUSTERED"
		}
		return "ALTER TABLE " + table + " ADD CONSTRAINT " + msQuote(ix.name) + " PRIMARY KEY" + kind + " (" + strings.Join(key, ", ") + ");"
	case ix.uniqueCs: // 唯一约束默认是非聚集；只在聚集时写出来
		kind := ""
		if clustered {
			kind = " CLUSTERED"
		}
		return "ALTER TABLE " + table + " ADD CONSTRAINT " + msQuote(ix.name) + " UNIQUE" + kind + " (" + strings.Join(key, ", ") + ");"
	case ix.typ == 3 || ix.typ == 4 || ix.typ == 7:
		label := map[int]string{3: "XML", 4: "SPATIAL", 7: "HASH"}[ix.typ]
		return "-- " + label + " 索引 " + msQuote(ix.name) + " 未还原（列：" + strings.Join(append(append([]string{}, key...), incl...), ", ") + "）" // i18n-ignore: DDL 文本里的注释
	}
	var b strings.Builder
	b.WriteString("CREATE ")
	if ix.unique {
		b.WriteString("UNIQUE ")
	}
	switch ix.typ {
	case 5:
		b.WriteString("CLUSTERED COLUMNSTORE INDEX " + msQuote(ix.name) + " ON " + table + ";")
		return b.String()
	case 6:
		b.WriteString("NONCLUSTERED COLUMNSTORE INDEX " + msQuote(ix.name) + " ON " + table + " (" + strings.Join(append(append([]string{}, key...), incl...), ", ") + ");")
		return b.String()
	case 1:
		b.WriteString("CLUSTERED ")
	default:
		b.WriteString("NONCLUSTERED ")
	}
	b.WriteString("INDEX " + msQuote(ix.name) + " ON " + table + " (" + strings.Join(key, ", ") + ")")
	if len(incl) > 0 {
		b.WriteString(" INCLUDE (" + strings.Join(incl, ", ") + ")")
	}
	if ix.filter != "" {
		b.WriteString(" WHERE " + ix.filter)
	}
	b.WriteString(";")
	return b.String()
}

// foreignKeyDDL 返回外键语句（每个外键一条）。
func (d mssqlDialect) foreignKeyDDL(ctx context.Context, q Querier, id int64, table string) ([]string, error) {
	rs, err := q.Query(ctx, `SELECT fk.name, SCHEMA_NAME(rt.schema_id), rt.name, fk.delete_referential_action_desc, fk.update_referential_action_desc,
COL_NAME(fkc.parent_object_id, fkc.parent_column_id), COL_NAME(fkc.referenced_object_id, fkc.referenced_column_id), fk.is_disabled
FROM sys.foreign_keys fk JOIN sys.foreign_key_columns fkc ON fkc.constraint_object_id = fk.object_id JOIN sys.tables rt ON rt.object_id = fk.referenced_object_id
WHERE fk.parent_object_id = @p1 ORDER BY fk.name, fkc.constraint_column_id`, id)
	if err != nil {
		return nil, err
	}
	type fkDef struct {
		name, refSchema, refTable, onDelete, onUpdate string
		cols, refCols                                 []string
		disabled                                      bool
	}
	byName := map[string]*fkDef{}
	var order []string
	for _, r := range rs.Data {
		if len(r) < 8 {
			continue
		}
		n := str(r[0])
		fk, ok := byName[n]
		if !ok {
			fk = &fkDef{name: n, refSchema: str(r[1]), refTable: str(r[2]), onDelete: str(r[3]), onUpdate: str(r[4]), disabled: truthy(r[7])}
			byName[n] = fk
			order = append(order, n)
		}
		fk.cols = append(fk.cols, str(r[5]))
		fk.refCols = append(fk.refCols, str(r[6]))
	}
	action := func(desc string) string {
		a := strings.ReplaceAll(strings.ToUpper(desc), "_", " ")
		if a == "" || a == "NO ACTION" {
			return ""
		}
		return a
	}
	var out []string
	for _, n := range order {
		fk := byName[n]
		s := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s.%s (%s)", table, msQuote(fk.name), msQuoteList(fk.cols),
			msQuote(fk.refSchema), msQuote(fk.refTable), msQuoteList(fk.refCols))
		if a := action(fk.onDelete); a != "" {
			s += " ON DELETE " + a
		}
		if a := action(fk.onUpdate); a != "" {
			s += " ON UPDATE " + a
		}
		out = append(out, s+";")
		if fk.disabled {
			out = append(out, "ALTER TABLE "+table+" NOCHECK CONSTRAINT "+msQuote(fk.name)+";")
		}
	}
	return out, nil
}

// oneLine 把错误文本压成一行，放进 SQL 注释里。
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
