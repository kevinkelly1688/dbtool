// @author: kevinkelly
// @license: Apache Licence
// @file: catalog.go
// @time: 2026/10/28

package maskadmin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/engine/dm"
	mongox "dbtool/internal/engine/mongo"
	"dbtool/internal/engine/oracle"
	"dbtool/internal/model"
	"dbtool/internal/query"
)

const (
	dbMSSQL  = "mssql"
	dbPG     = "pgsql"
	dbCH     = "clickhouse"
	dbMongo  = "mongo"
	dbOracle = "oracle"
	dbDM     = "dm"

	maxCatalogColumns = 30000 // 一个库最多取多少列（超出标记“已截断”，用粘贴清单补）
	maxMongoColls     = 100   // MongoDB 最多取多少个集合（每个集合要采样一次）
)

func baseMySQLInfo(label string) EngineInfo {
	return EngineInfo{Label: label, Hier: "库 › 表 › 字段", Path: "库.表.字段", Table: "表", TableN: "张表", Field: "字段", RowUnit: "行",
		Source: "目标库 information_schema.COLUMNS", Locate: "exact",
		How:  "goInception 语法树逐列溯源：每个结果列来自哪个 库.表.字段（含别名、子查询、JOIN、UNION）",
		Note: "实例没有配置 goInception 时自动降级为按结果列名保守匹配（函数 / 运算列可能多脱敏），页面会提示。"}
}

// engineInfo 返回某类数据库的层级与定位说明；不在脱敏范围内的类型返回 nil。
func engineInfo(dbType string) *EngineInfo {
	var e EngineInfo
	switch dbType {
	case "mysql":
		e = baseMySQLInfo("MySQL")
	case "polardb":
		e = baseMySQLInfo("PolarDB")
	case "adb":
		e = baseMySQLInfo("ADB")
	case "polardb-x", "polardbx":
		e = baseMySQLInfo("PolarDB-X")
		e.Locate = "name"
		e.How = "不使用语法树：按结果列名、SQL 里的别名和表达式引用匹配（保守）"
		e.Note = "字段读取与 MySQL 相同；因为无法精确溯源，引用了敏感字段的函数 / 运算结果列会整列脱敏，宁多勿漏；不同库表里恰好同名的列也会被脱敏。"
	case "selectdb", "doris":
		e = baseMySQLInfo("SelectDB / Doris")
		e.Locate = "name"
		e.How = "没有语法树：按结果列名、SQL 里的别名和表达式引用匹配（保守）"
		e.Note = "字段读取与 MySQL 相同；因为无法精确溯源，引用了敏感字段的函数 / 运算结果列会整列脱敏，宁多勿漏。"
	case dbMSSQL:
		e = baseMySQLInfo("SQL Server")
		e.Hier, e.Path, e.Field = "库 › 架构.表 › 列", "库.架构.表.列", "列"
		e.Source = "sys.schemas / sys.tables / sys.columns"
		e.How = "执行前用 sys.dm_exec_describe_first_result_set 取每个结果列的来源库 / 架构 / 表 / 列（视图与基表两层都对照）"
		e.Note = "临时表、动态 SQL 等无法静态推断结果集的语句，自动降级为按列名保守匹配。"
	case dbPG:
		e = baseMySQLInfo("PostgreSQL")
		e.Hier, e.Path, e.Field = "库 › 架构.表 › 列", "库.架构.表.列", "列"
		e.Source = "pg_catalog（pg_class / pg_attribute）"
		e.How = "结果集描述里自带每列的表 OID 与列号，反查即得 架构.表.列（含别名、JOIN）；表达式列与视图列再按列名保守补一遍"
		e.Note = "json / jsonb 列：对其中所有文本值逐个按规则处理。"
	case dbCH:
		e = baseMySQLInfo("ClickHouse")
		e.Hier, e.Path, e.Field = "库 › 表 › 列", "库.表.列", "列"
		e.Source = "system.columns"
		e.Locate = "name"
		e.How = "协议不返回列来源：按结果列名、别名和表达式引用匹配（保守）"
		e.Note = "Map / Array / Tuple 列：对其中的文本值逐个按规则处理。"
	case dbOracle:
		e = baseMySQLInfo("Oracle")
		e.Hier, e.Path, e.Field = "库（schema） › 表 › 列", "库.表.列", "列"
		e.Source = "ALL_TAB_COLUMNS / ALL_COL_COMMENTS"
		e.Locate = "name"
		e.How = "协议不返回列来源：按结果列名、别名和表达式引用匹配（保守）；语句用到视图时展开视图定义一起分析"
		e.Note = "没加引号的名字在 Oracle 里是大写：库 / 表 / 列都按库里的真实名字写。列名匹配不区分大小写，不同表里恰好同名的列也会被脱敏。"
	case dbDM:
		e = baseMySQLInfo("达梦 DM")
		e.Hier, e.Path, e.Field = "库（schema） › 表 › 列", "库.表.列", "列"
		e.Source = "ALL_TAB_COLUMNS / ALL_COL_COMMENTS"
		e.Locate = "name"
		e.How = "协议不返回列来源：按结果列名、别名和表达式引用匹配（保守）；语句用到视图时展开视图定义一起分析"
		e.Note = "没加引号的名字在达梦里是大写：库 / 表 / 列都按库里的真实名字写。列名匹配不区分大小写，不同表里恰好同名的列也会被脱敏。"
	case dbMongo:
		e = baseMySQLInfo("MongoDB")
		e.Hier, e.Path, e.Table, e.TableN, e.RowUnit = "库 › 集合 › 字段路径", "库.集合.字段路径", "集合", "个集合", "文档"
		e.Source = "采样每个集合最近的文档推断字段，可手动补充路径"
		e.Locate = "path"
		e.How = "按字段路径匹配返回的文档：嵌套（profile.phone）、数组元素（contacts[].mobile）逐层处理；管道里把敏感字段改名输出（{p: \"$profile.phone\"}）也一并脱敏"
		e.Note = "同一路径下类型不统一（有的文档是数字）时，数字会先转成文本再脱敏。"
	default:
		return nil
	}
	return &e
}

// unsupportedReason 说明某类实例为什么不在脱敏范围内。
func unsupportedReason(dbType string) string {
	switch dbType {
	case "redis", "memcached":
		return "键值结构，没有表 / 字段"
	case "elasticsearch", "opensearch":
		return "索引文档，不是 SQL 结果集"
	}
	return "暂无在线查询入口，脱敏无从生效；接入后沿用同一套规则"
}

// supported 判断该类实例是否在脱敏范围内：有字段层级、且在线查询已接入。
func supported(dbType string) bool { return engineInfo(dbType) != nil && query.SupportsQuery(dbType) }

// ---- 字段目录 ----

func lit(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func cellStr(row []any, i int) string {
	if i >= len(row) || row[i] == nil {
		return ""
	}
	switch v := row[i].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return fmt.Sprint(row[i])
}

// catalogOf 取一个库里所有表 / 视图及其字段。
func catalogOf(ctx context.Context, e engine.Engine, inst *model.Instance, db string) (*Catalog, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	switch inst.DBType {
	case dbMongo:
		return mongoCatalog(ctx, e, db)
	case dbOracle:
		return oracleCatalog(ctx, e, db)
	case dbDM:
		return dmCatalog(ctx, e, db)
	}
	var (
		q      string
		qdb    = db
		viewAt = -1 // 结果里“是否视图”所在列；-1 表示没有
	)
	switch inst.DBType {
	case dbMSSQL:
		// 注释取 MS_Description 扩展属性；类型带长度 / 精度
		q = "SELECT s.name + '.' + o.name, c.name, " +
			"ty.name + CASE WHEN ty.name IN ('varchar','char','varbinary','binary') THEN '(' + CASE WHEN c.max_length = -1 THEN 'max' ELSE CAST(c.max_length AS varchar(10)) END + ')' " +
			"WHEN ty.name IN ('nvarchar','nchar') THEN '(' + CASE WHEN c.max_length = -1 THEN 'max' ELSE CAST(c.max_length / 2 AS varchar(10)) END + ')' " +
			"WHEN ty.name IN ('decimal','numeric') THEN '(' + CAST(c.precision AS varchar(10)) + ',' + CAST(c.scale AS varchar(10)) + ')' ELSE '' END, " +
			"ISNULL(CAST(ep.value AS nvarchar(500)), ''), CASE o.type WHEN 'V' THEN 1 ELSE 0 END " +
			"FROM sys.columns c JOIN sys.objects o ON o.object_id = c.object_id AND o.type IN ('U', 'V') AND o.is_ms_shipped = 0 " +
			"JOIN sys.schemas s ON s.schema_id = o.schema_id JOIN sys.types ty ON ty.user_type_id = c.user_type_id " +
			"LEFT JOIN sys.extended_properties ep ON ep.class = 1 AND ep.major_id = c.object_id AND ep.minor_id = c.column_id AND ep.name = 'MS_Description' " +
			"ORDER BY s.name, o.name, c.column_id"
		viewAt = 4
	case dbPG:
		q = "SELECT n.nspname || '.' || c.relname, a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), COALESCE(d.description, ''), " +
			"CASE WHEN c.relkind IN ('v', 'm') THEN '1' ELSE '0' END " +
			"FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid = a.attrelid " +
			"JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace " +
			"LEFT JOIN pg_catalog.pg_description d ON d.objoid = a.attrelid AND d.objsubid = a.attnum " +
			"WHERE a.attnum > 0 AND NOT a.attisdropped AND c.relkind IN ('r', 'p', 'v', 'm', 'f') " +
			"AND n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\\_%' " +
			"ORDER BY 1, a.attnum"
		viewAt = 4
	case dbCH:
		q = "SELECT table, name, type, comment, 0 FROM system.columns WHERE database = " + lit(db) + " ORDER BY table, position"
		qdb = ""
	default: // MySQL 系
		q = "SELECT c.TABLE_NAME, c.COLUMN_NAME, c.COLUMN_TYPE, c.COLUMN_COMMENT, CASE WHEN t.TABLE_TYPE = 'VIEW' THEN 1 ELSE 0 END " +
			"FROM information_schema.COLUMNS c JOIN information_schema.TABLES t ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME " +
			"WHERE c.TABLE_SCHEMA = " + lit(db) + " ORDER BY c.TABLE_NAME, c.ORDINAL_POSITION"
		qdb = ""
		viewAt = 4
	}
	rs, err := e.Query(ctx, q, engine.QueryOptions{DBName: qdb, Limit: maxCatalogColumns + 1, MaxExecMs: 60000})
	if err != nil {
		return nil, wrapf(err, "读取库 %s 的字段清单失败: %s", db)
	}
	out := &Catalog{DB: db}
	if len(rs.Rows) > maxCatalogColumns {
		out.Truncated = true
		rs.Rows = rs.Rows[:maxCatalogColumns]
		out.Note = fmt.Sprintf("库里字段很多，只取了前 %d 个；其余字段可用“粘贴清单”按名字选。", maxCatalogColumns)
	}
	idx := map[string]int{}
	for _, r := range rs.Rows {
		tn := cellStr(r, 0)
		if tn == "" {
			continue
		}
		i, ok := idx[tn]
		if !ok {
			i = len(out.Tables)
			idx[tn] = i
			out.Tables = append(out.Tables, CatalogTable{Name: tn, Rows: -1, View: viewAt >= 0 && cellStr(r, viewAt) == "1", Columns: []CatalogColumn{}})
		}
		out.Tables[i].Columns = append(out.Tables[i].Columns, CatalogColumn{Name: cellStr(r, 1), Type: cellStr(r, 2), Comment: cellStr(r, 3)})
	}
	return out, nil
}

// oraCatalogSource 由 *oracle.Engine 实现。
type oraCatalogSource interface {
	TableInfos(ctx context.Context, db string) ([]oracle.TableInfo, error)
	ColumnRows(ctx context.Context, db string, max int) ([]oracle.ColumnRow, error)
}

// dmCatalogSource 由 *dm.Engine 实现。
type dmCatalogSource interface {
	TableInfos(ctx context.Context, db string) ([]dm.TableInfo, error)
	ColumnRows(ctx context.Context, db string, max int) ([]dm.ColumnRow, error)
}

func oracleCatalog(ctx context.Context, e engine.Engine, db string) (*Catalog, error) {
	m, ok := e.(oraCatalogSource)
	if !ok {
		return nil, fmt.Errorf("Oracle 引擎不可用")
	}
	rows, err := m.ColumnRows(ctx, db, maxCatalogColumns+1)
	if err != nil {
		return nil, wrapf(err, "读取库 %s 的字段清单失败: %s", db)
	}
	views := map[string]bool{}
	if infos, err := m.TableInfos(ctx, db); err == nil { // 读不到表清单只是分不出视图，不影响字段目录
		for _, i := range infos {
			views[i.Name] = i.IsView
		}
	}
	flat := make([][4]string, len(rows))
	for i, r := range rows {
		flat[i] = [4]string{r.Table, r.Column, r.Type, r.Comment}
	}
	return engineCatalog(db, flat, views), nil
}

func dmCatalog(ctx context.Context, e engine.Engine, db string) (*Catalog, error) {
	m, ok := e.(dmCatalogSource)
	if !ok {
		return nil, fmt.Errorf("达梦引擎不可用")
	}
	rows, err := m.ColumnRows(ctx, db, maxCatalogColumns+1)
	if err != nil {
		return nil, wrapf(err, "读取库 %s 的字段清单失败: %s", db)
	}
	views := map[string]bool{}
	if infos, err := m.TableInfos(ctx, db); err == nil {
		for _, i := range infos {
			views[i.Name] = i.IsView
		}
	}
	flat := make([][4]string, len(rows))
	for i, r := range rows {
		flat[i] = [4]string{r.Table, r.Column, r.Type, r.Comment}
	}
	return engineCatalog(db, flat, views), nil
}

// engineCatalog 把按表名、列序排好的 (表, 列, 类型, 注释) 整理成字段目录；rows 比上限多一行说明被截断了。
func engineCatalog(db string, rows [][4]string, views map[string]bool) *Catalog {
	out := &Catalog{DB: db}
	if len(rows) > maxCatalogColumns {
		out.Truncated = true
		rows = rows[:maxCatalogColumns]
		out.Note = fmt.Sprintf("库里字段很多，只取了前 %d 个；其余字段可用“粘贴清单”按名字选。", maxCatalogColumns)
	}
	idx := map[string]int{}
	for _, r := range rows {
		if r[0] == "" {
			continue
		}
		i, ok := idx[r[0]]
		if !ok {
			i = len(out.Tables)
			idx[r[0]] = i
			out.Tables = append(out.Tables, CatalogTable{Name: r[0], Rows: -1, View: views[r[0]], Columns: []CatalogColumn{}})
		}
		out.Tables[i].Columns = append(out.Tables[i].Columns, CatalogColumn{Name: r[1], Type: r[2], Comment: r[3]})
	}
	return out
}

// mongoMeta 由 *mongox.Engine 实现。
type mongoMeta interface {
	TableInfos(ctx context.Context, db string) ([]mongox.TableInfo, error)
	ColumnInfos(ctx context.Context, db, table string) ([]mongox.ColumnInfo, error)
}

func mongoCatalog(ctx context.Context, e engine.Engine, db string) (*Catalog, error) {
	m, ok := e.(mongoMeta)
	if !ok {
		return nil, fmt.Errorf("MongoDB 引擎不可用")
	}
	infos, err := m.TableInfos(ctx, db)
	if err != nil {
		return nil, wrapf(err, "读取库 %s 的集合失败: %s", db)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	out := &Catalog{DB: db}
	for n, ti := range infos {
		if n >= maxMongoColls {
			out.Truncated = true
			out.Note = fmt.Sprintf("库里集合很多，只取了前 %d 个；其余集合的字段可用“手动补充路径”添加。", maxMongoColls)
			break
		}
		t := CatalogTable{Name: ti.Name, Comment: ti.Comment, View: ti.Type == "view", Rows: -1, Columns: []CatalogColumn{}}
		if ti.HasStats && !t.View {
			t.Rows = ti.RowsEstimate
		}
		if !t.View {
			if cols, err := m.ColumnInfos(ctx, db, ti.Name); err == nil {
				for _, c := range cols {
					t.Columns = append(t.Columns, CatalogColumn{Name: c.Name, Type: c.Type, Comment: c.Comment})
				}
			}
		}
		out.Tables = append(out.Tables, t)
	}
	return out, nil
}
