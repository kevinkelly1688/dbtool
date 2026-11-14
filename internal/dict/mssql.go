// @author: kevinkelly
// @license: Apache Licence
// @file: mssql.go
// @time: 2026/10/28

package dict

import (
	"context"
	"fmt"
	"strings"
)

// mssqlDialect 是 SQL Server：表、视图、触发器、存储过程、函数。
// SQL Agent 作业（“定时任务”，需要访问 msdb）不展示，明确提示暂不支持。
// 建表语句按系统目录还原（mssql_ddl.go），含索引 / 外键 / 检查约束 / 触发器，只供查看参考。
// 所有查询都读 sys.* 目录视图，参数走 @p1 占位符；对象用 object_id 定位，不拼接任何用户输入。
type mssqlDialect struct{}

var mssqlSystemDBs = map[string]bool{"master": true, "tempdb": true, "model": true, "msdb": true}

func (mssqlDialect) IsSystemDB(name string) bool { return mssqlSystemDBs[strings.ToLower(name)] }

func (mssqlDialect) Kinds() []Kind {
	return []Kind{KindTable, KindView, KindTrigger, KindProcedure, KindFunction}
}

func (d mssqlDialect) Databases(ctx context.Context, q Querier) ([]string, error) {
	rs, err := q.Query(ctx, "SELECT name FROM sys.databases WHERE database_id > 4 AND state = 0 AND HAS_DBACCESS(name) = 1 ORDER BY name")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rs.Data {
		if n := str(r[0]); !d.IsSystemDB(n) {
			out = append(out, n)
		}
	}
	return out, nil
}

const msComment = "ISNULL(CONVERT(nvarchar(4000), ep.value), '')"

// msJoinComment 给对象（major_id = 对象 id）左连接 MS_Description 说明。
func msJoinComment(idCol string) string {
	return " LEFT JOIN sys.extended_properties ep ON ep.major_id = " + idCol + " AND ep.minor_id = 0 AND ep.class = 1 AND ep.name = 'MS_Description'"
}

func (d mssqlDialect) List(ctx context.Context, q Querier, db string, kind Kind) ([]Item, error) {
	var sql string
	switch kind {
	case KindTable:
		sql = "SELECT s.name, t.name, " + msComment + " FROM sys.tables t JOIN sys.schemas s ON s.schema_id = t.schema_id" + msJoinComment("t.object_id") + " WHERE t.is_ms_shipped = 0 ORDER BY s.name, t.name"
	case KindView:
		sql = "SELECT s.name, v.name, " + msComment + " FROM sys.views v JOIN sys.schemas s ON s.schema_id = v.schema_id" + msJoinComment("v.object_id") + " WHERE v.is_ms_shipped = 0 ORDER BY s.name, v.name"
	case KindProcedure:
		sql = "SELECT s.name, p.name, " + msComment + " FROM sys.procedures p JOIN sys.schemas s ON s.schema_id = p.schema_id" + msJoinComment("p.object_id") + " WHERE p.is_ms_shipped = 0 ORDER BY s.name, p.name"
	case KindFunction:
		sql = "SELECT s.name, o.name, " + msComment + " FROM sys.objects o JOIN sys.schemas s ON s.schema_id = o.schema_id" + msJoinComment("o.object_id") +
			" WHERE o.type IN ('FN','TF','IF','FS','FT') AND o.is_ms_shipped = 0 ORDER BY s.name, o.name"
	case KindTrigger:
		sql = `SELECT s.name, tr.name, ISNULL(STUFF((SELECT ',' + te.type_desc FROM sys.trigger_events te WHERE te.object_id = tr.object_id FOR XML PATH('')), 1, 1, ''), '') + ' ON ' + ISNULL(OBJECT_NAME(tr.parent_id), '')
FROM sys.triggers tr JOIN sys.objects o ON o.object_id = tr.object_id JOIN sys.schemas s ON s.schema_id = o.schema_id WHERE tr.parent_class = 1 AND tr.is_ms_shipped = 0 ORDER BY s.name, tr.name`
	case KindEvent:
		return nil, newErr(501, "unsupported_kind", "SQL Server 的定时任务（SQL Server Agent 作业）暂不支持在数据字典里查看")
	default:
		return nil, badReq("未知的对象类别 %q", kind)
	}
	rs, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(rs.Data))
	for _, r := range rs.Data {
		out = append(out, Item{Schema: str(r[0]), Name: str(r[1]), Comment: str(r[2])})
	}
	if kind == KindTable {
		d.enrichTables(ctx, q, out)
	}
	return out, nil
}

// enrichTables 补表的估算行数 / 大小 / 分区表标记（sys.dm_db_partition_stats 需要 VIEW DATABASE STATE 权限）。
// 属于锦上添花：任何失败都忽略，对应字段留空，不影响对象列表。
func (d mssqlDialect) enrichTables(ctx context.Context, q Querier, items []Item) {
	if len(items) == 0 {
		return
	}
	rs, err := q.Query(ctx, `SELECT s.name, t.name, ps.row_cnt, ps.byte_cnt,
CASE WHEN EXISTS (SELECT 1 FROM sys.partition_schemes psch JOIN sys.indexes i ON i.data_space_id = psch.data_space_id WHERE i.object_id = t.object_id AND i.index_id IN (0, 1)) THEN 1 ELSE 0 END,
(SELECT TOP 1 pc.name FROM sys.index_columns pic JOIN sys.indexes pi ON pi.object_id = pic.object_id AND pi.index_id = pic.index_id
  JOIN sys.columns pc ON pc.object_id = pic.object_id AND pc.column_id = pic.column_id
  WHERE pi.object_id = t.object_id AND pi.is_primary_key = 1 AND pc.is_identity = 1 ORDER BY pic.key_ordinal)
FROM sys.schemas s JOIN sys.tables t ON t.schema_id = s.schema_id
LEFT JOIN (SELECT object_id, SUM(CASE WHEN index_id IN (0, 1) THEN row_count ELSE 0 END) AS row_cnt, SUM(reserved_page_count) * 8 * 1024 AS byte_cnt
  FROM sys.dm_db_partition_stats GROUP BY object_id) ps ON ps.object_id = t.object_id
WHERE t.is_ms_shipped = 0`)
	if err != nil || rs == nil {
		return
	}
	index := make(map[string]int, len(items))
	for i := range items {
		index[items[i].Schema+"\x01"+items[i].Name] = i
	}
	for _, r := range rs.Data {
		if len(r) < 5 {
			continue
		}
		i, ok := index[str(r[0])+"\x01"+str(r[1])]
		if !ok {
			continue
		}
		items[i].RowsEst = intPtr(r[2])
		items[i].SizeBytes = intPtr(r[3])
		if f, _ := toFloat(r[4]); f != 0 {
			items[i].Partitioned = true
		}
		if len(r) > 5 {
			items[i].AutoIncPK = str(r[5])
		}
	}
}

// ColumnsNamed 返回库里列名在 names 中的表 / 视图列（用于给对象列表统计敏感列数）。
func (d mssqlDialect) ColumnsNamed(ctx context.Context, q Querier, _ string, names []string) ([]colRef, error) {
	if len(names) == 0 {
		return nil, nil
	}
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}
	rs, err := q.Query(ctx, "SELECT s.name, o.name, c.name FROM sys.columns c JOIN sys.objects o ON o.object_id = c.object_id JOIN sys.schemas s ON s.schema_id = o.schema_id"+
		" WHERE o.type IN ('U', 'V') AND o.is_ms_shipped = 0 AND c.name IN ("+placeholders("@p", len(names))+")", args...)
	if err != nil {
		return nil, err
	}
	out := make([]colRef, 0, len(rs.Data))
	for _, r := range rs.Data {
		out = append(out, colRef{Schema: str(r[0]), Table: str(r[1]), Column: str(r[2])})
	}
	return out, nil
}

// objectID 把（架构, 名称）解析成 object_id，不存在返回 404。
func (d mssqlDialect) objectID(ctx context.Context, q Querier, kind Kind, schema, name string) (int64, error) {
	var sql string
	switch kind {
	case KindTable:
		sql = "SELECT t.object_id FROM sys.tables t JOIN sys.schemas s ON s.schema_id = t.schema_id WHERE s.name = @p1 AND t.name = @p2"
	case KindView:
		sql = "SELECT v.object_id FROM sys.views v JOIN sys.schemas s ON s.schema_id = v.schema_id WHERE s.name = @p1 AND v.name = @p2"
	case KindProcedure:
		sql = "SELECT p.object_id FROM sys.procedures p JOIN sys.schemas s ON s.schema_id = p.schema_id WHERE s.name = @p1 AND p.name = @p2"
	case KindFunction:
		sql = "SELECT o.object_id FROM sys.objects o JOIN sys.schemas s ON s.schema_id = o.schema_id WHERE o.type IN ('FN','TF','IF','FS','FT') AND s.name = @p1 AND o.name = @p2"
	case KindTrigger:
		sql = "SELECT tr.object_id FROM sys.triggers tr JOIN sys.objects o ON o.object_id = tr.object_id JOIN sys.schemas s ON s.schema_id = o.schema_id WHERE tr.parent_class = 1 AND s.name = @p1 AND tr.name = @p2"
	default:
		return 0, newErr(501, "unsupported_kind", "SQL Server 暂不支持查看%s", kindLabel(kind))
	}
	rs, err := q.Query(ctx, sql, schema, name)
	if err != nil {
		return 0, err
	}
	if len(rs.Data) == 0 {
		return 0, notFound(kind, schema+"."+name)
	}
	f, _ := toFloat(rs.Data[0][0])
	return int64(f), nil
}

func (d mssqlDialect) Detail(ctx context.Context, q Querier, db string, kind Kind, schema, name string) (*Detail, error) {
	if kind == KindEvent {
		return nil, newErr(501, "unsupported_kind", "SQL Server 的定时任务（SQL Server Agent 作业）暂不支持在数据字典里查看")
	}
	if strings.TrimSpace(schema) == "" {
		return nil, badReq("SQL Server 的对象需要同时给出架构名")
	}
	id, err := d.objectID(ctx, q, kind, schema, name)
	if err != nil {
		return nil, err
	}
	det := &Detail{Kind: kind, Schema: schema, Name: name}
	switch kind {
	case KindTable:
		err = d.tableDetail(ctx, q, id, schema, name, det)
	case KindView:
		err = d.viewDetail(ctx, q, id, schema, name, det)
	case KindTrigger:
		err = d.triggerDetail(ctx, q, id, schema, det)
	default:
		err = d.routineDetail(ctx, q, id, schema, kind, det)
	}
	if err != nil {
		return nil, err
	}
	return det, nil
}

func (d mssqlDialect) objectMeta(ctx context.Context, q Querier, id int64) ([]any, error) {
	rs, err := q.Query(ctx, "SELECT o.create_date, o.modify_date, o.type_desc, "+msComment+" FROM sys.objects o"+msJoinComment("o.object_id")+" WHERE o.object_id = @p1", id)
	if err != nil {
		return nil, err
	}
	if len(rs.Data) == 0 {
		return nil, newErr(404, "not_found", "对象不存在")
	}
	return rs.Data[0], nil
}

func (d mssqlDialect) tableDetail(ctx context.Context, q Querier, id int64, schema, name string, det *Detail) error {
	m, err := d.objectMeta(ctx, q, id)
	if err != nil {
		return err
	}
	const pages = ` FROM sys.partitions p JOIN sys.allocation_units a ON (a.type IN (1,3) AND a.container_id = p.hobt_id) OR (a.type = 2 AND a.container_id = p.partition_id) WHERE p.object_id = @p1`
	sizes, err := q.Query(ctx, `SELECT (SELECT SUM(p.rows) FROM sys.partitions p WHERE p.object_id = @p1 AND p.index_id IN (0, 1)),
(SELECT SUM(a.total_pages) * 8`+pages+`), (SELECT SUM(a.used_pages) * 8`+pages+`), IDENT_CURRENT(QUOTENAME(@p2) + '.' + QUOTENAME(@p3))`, id, schema, name)
	if err != nil {
		return err
	}
	var rows, total, used, ident any
	if len(sizes.Data) > 0 {
		rows, total, used, ident = sizes.Data[0][0], sizes.Data[0][1], sizes.Data[0][2], sizes.Data[0][3]
	}
	det.Sections = append(det.Sections, kvSec("表信息", []Pair{{"架构", schema}, {"表名", name}, {"主键", d.primaryKeyText(ctx, q, id)}, {"表行数（估算）", str(rows)}, {"占用空间(KB)", str(total)}, {"已用空间(KB)", str(used)},
		{"当前自增值", str(ident)}, {"创建时间", str(m[0])}, {"更新时间", str(m[1])}, {"表说明", str(m[3])}}))
	if err := d.columnsAndIndexes(ctx, q, id, det, true); err != nil {
		return err
	}
	det.Sections = append(det.Sections, d.createTableSection(ctx, q, id, schema, name))
	return nil
}

// primaryKeyText 返回“主键”一行的文字：列名按键顺序；含自增（identity）列的标注“（自增）”；没有主键写“无主键”。
// 取不到（没有权限等）返回“—”，不影响详情。
func (d mssqlDialect) primaryKeyText(ctx context.Context, q Querier, id int64) string {
	rs, err := q.Query(ctx, `SELECT c.name, c.is_identity FROM sys.index_columns ic JOIN sys.indexes i ON i.object_id = ic.object_id AND i.index_id = ic.index_id
JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE i.object_id = @p1 AND i.is_primary_key = 1 AND ic.is_included_column = 0 ORDER BY ic.key_ordinal`, id)
	if err != nil {
		return "—"
	}
	return pkText(rs.Data)
}

// pkText 把（列名, 是否自增）行拼成显示文字；数据行不足两列的忽略。
func pkText(data [][]any) string {
	var cols []string
	for _, r := range data {
		if len(r) < 2 {
			continue
		}
		c := str(r[0])
		if truthy(r[1]) {
			c += "（自增）"
		}
		cols = append(cols, c)
	}
	if len(cols) == 0 {
		return "无主键"
	}
	return strings.Join(cols, ", ")
}

func (d mssqlDialect) columnsAndIndexes(ctx context.Context, q Querier, id int64, det *Detail, withIndexes bool) error {
	cols, err := q.Query(ctx, msColumnsSQL("c.object_id = @p1", false), id)
	if err != nil {
		return err
	}
	det.Sections = append(det.Sections, Section{Title: "列信息", Type: "table", Columns: columnHeads, Rows: msColumnRows(cols.Data, 0)})
	if !withIndexes {
		return nil
	}
	idx, err := d.indexes(ctx, q, "i.object_id = @p1", id)
	if err != nil {
		det.Sections = append(det.Sections, noteSec("索引信息", fmt.Sprintf("获取失败：%s", err.Error())))
	} else {
		var rows [][]string
		for _, v := range idx {
			rows = append(rows, v...)
		}
		det.Sections = append(det.Sections, Section{Title: "索引信息", Type: "table", Columns: indexHeads, Rows: rows})
	}
	return nil
}

// msColumnsSQL 生成取列信息的语句；withName 为 true 时前两列是（架构, 对象名），用于整库导出。
func msColumnsSQL(where string, withName bool) string {
	sel, from, order := "", " FROM sys.columns c", " ORDER BY c.column_id"
	if withName {
		sel = "s.name, o.name, "
		from = " FROM sys.objects o JOIN sys.schemas s ON s.schema_id = o.schema_id JOIN sys.columns c ON c.object_id = o.object_id"
		order = " ORDER BY s.name, o.name, c.column_id"
	}
	return "SELECT " + sel + `c.name, TYPE_NAME(c.user_type_id), c.max_length, c.precision, c.scale, c.is_nullable, c.is_identity, dc.definition, ` + msComment + `,
CASE WHEN EXISTS (SELECT 1 FROM sys.index_columns ic JOIN sys.indexes i ON i.object_id = ic.object_id AND i.index_id = ic.index_id
  WHERE ic.object_id = c.object_id AND ic.column_id = c.column_id AND i.is_primary_key = 1) THEN 'PRI' ELSE '' END` + from +
		" LEFT JOIN sys.default_constraints dc ON dc.parent_object_id = c.object_id AND dc.parent_column_id = c.column_id" +
		" LEFT JOIN sys.extended_properties ep ON ep.major_id = c.object_id AND ep.minor_id = c.column_id AND ep.class = 1 AND ep.name = 'MS_Description'" +
		" WHERE " + where + order
}

// msColumnRows 把 msColumnsSQL 的结果转成展示行；off 是前面多出来的列数（0 或 2）。
func msColumnRows(data [][]any, off int) [][]string {
	out := make([][]string, 0, len(data))
	for _, r := range data {
		extra := ""
		if v, _ := toFloat(r[off+6]); v != 0 {
			extra = "identity（自增）"
		}
		maxLen, _ := toFloat(r[off+2])
		prec, _ := toFloat(r[off+3])
		scale, _ := toFloat(r[off+4])
		out = append(out, []string{str(r[off]), msFormatType(str(r[off+1]), int(maxLen), int(prec), int(scale)), yesNo(r[off+5]),
			strNull(r[off+7], "NULL"), str(r[off+9]), extra, str(r[off+8])})
	}
	return out
}

// msFormatType 把系统目录里的类型信息还原成 DDL 里的写法。
func msFormatType(typ string, maxLen, prec, scale int) string {
	switch strings.ToLower(typ) {
	case "nvarchar", "nchar":
		if maxLen == -1 {
			return typ + "(MAX)"
		}
		return typ + "(" + itoa(maxLen/2) + ")" // 这两类 max_length 是字节数
	case "varchar", "char", "varbinary", "binary":
		if maxLen == -1 {
			return typ + "(MAX)"
		}
		return typ + "(" + itoa(maxLen) + ")"
	case "decimal", "numeric":
		return typ + "(" + itoa(prec) + "," + itoa(scale) + ")"
	case "datetime2", "time", "datetimeoffset":
		return typ + "(" + itoa(scale) + ")"
	case "float":
		return typ + "(" + itoa(prec) + ")"
	}
	return typ
}

func itoa(n int) string { return str(n) }

// indexes 返回 map[架构\x01对象名][]行（索引名、列、唯一、类型）；where 里引用 i（sys.indexes）。
func (d mssqlDialect) indexes(ctx context.Context, q Querier, where string, args ...any) (map[string][][]string, error) {
	rs, err := q.Query(ctx, `SELECT s.name, o.name, i.name, i.is_unique, i.type_desc, i.is_primary_key, COL_NAME(ic.object_id, ic.column_id)
FROM sys.indexes i JOIN sys.objects o ON o.object_id = i.object_id JOIN sys.schemas s ON s.schema_id = o.schema_id
JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id AND ic.is_included_column = 0
WHERE i.index_id > 0 AND `+where+` ORDER BY s.name, o.name, i.index_id, ic.key_ordinal`, args...)
	if err != nil {
		return nil, err
	}
	rows := make([]indexRow, 0, len(rs.Data))
	for _, r := range rs.Data {
		uniq := "否"
		if v, _ := toFloat(r[3]); v != 0 {
			uniq = "是"
		}
		typ := str(r[4])
		if v, _ := toFloat(r[5]); v != 0 {
			typ += " · 主键"
		}
		rows = append(rows, indexRow{table: str(r[0]) + "\x01" + str(r[1]), name: str(r[2]), unique: uniq, typ: typ, col: str(r[6])})
	}
	return aggregateIndexes(rows), nil
}

func (d mssqlDialect) definition(ctx context.Context, q Querier, id int64, title string) Section {
	rs, err := q.Query(ctx, "SELECT OBJECT_DEFINITION(@p1)", id)
	if err != nil {
		return noteSec(title, fmt.Sprintf("获取失败：%s", err.Error()))
	}
	if len(rs.Data) == 0 || rs.Data[0][0] == nil || str(rs.Data[0][0]) == "" {
		return noteSec(title, "没有取到定义（可能没有 VIEW DEFINITION 权限，或对象已加密）")
	}
	return codeSec(title, str(rs.Data[0][0]))
}

func (d mssqlDialect) viewDetail(ctx context.Context, q Querier, id int64, schema, name string, det *Detail) error {
	m, err := d.objectMeta(ctx, q, id)
	if err != nil {
		return err
	}
	det.Sections = append(det.Sections, kvSec("视图信息", []Pair{{"架构", schema}, {"视图名", name}, {"创建时间", str(m[0])}, {"更新时间", str(m[1])}, {"说明", str(m[3])}}))
	if err := d.columnsAndIndexes(ctx, q, id, det, false); err != nil {
		return err
	}
	det.Sections = append(det.Sections, d.definition(ctx, q, id, "视图定义"))
	return nil
}

func (d mssqlDialect) triggerDetail(ctx context.Context, q Querier, id int64, schema string, det *Detail) error {
	rs, err := q.Query(ctx, `SELECT tr.name, ISNULL(OBJECT_SCHEMA_NAME(tr.parent_id) + '.' + OBJECT_NAME(tr.parent_id), ''),
CASE WHEN tr.is_instead_of_trigger = 1 THEN 'INSTEAD OF' ELSE 'AFTER' END,
ISNULL(STUFF((SELECT ',' + te.type_desc FROM sys.trigger_events te WHERE te.object_id = tr.object_id FOR XML PATH('')), 1, 1, ''), ''),
tr.create_date, tr.modify_date, tr.is_disabled FROM sys.triggers tr WHERE tr.object_id = @p1`, id)
	if err != nil {
		return err
	}
	if len(rs.Data) == 0 {
		return newErr(404, "not_found", "对象不存在")
	}
	r := rs.Data[0]
	status := "启用"
	if v, _ := toFloat(r[6]); v != 0 {
		status = "已禁用"
	}
	det.Sections = append(det.Sections, kvSec("触发器信息", []Pair{{"架构", schema}, {"名称", str(r[0])}, {"所属表", str(r[1])}, {"触发时机", str(r[2])}, {"事件", str(r[3])},
		{"创建时间", str(r[4])}, {"更新时间", str(r[5])}, {"状态", status}}), d.definition(ctx, q, id, "触发器语句"))
	return nil
}

func (d mssqlDialect) routineDetail(ctx context.Context, q Querier, id int64, schema string, kind Kind, det *Detail) error {
	m, err := d.objectMeta(ctx, q, id)
	if err != nil {
		return err
	}
	title := map[Kind]string{KindProcedure: "存储过程信息", KindFunction: "函数信息"}[kind]
	pairs := []Pair{{"架构", schema}, {"名称", det.Name}, {"类型", str(m[2])}, {"创建时间", str(m[0])}, {"最后修改", str(m[1])}, {"说明", str(m[3])}}
	det.Sections = append(det.Sections, kvSec(title, pairs), d.definition(ctx, q, id, "定义"))
	return nil
}
