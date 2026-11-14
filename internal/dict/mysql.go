// @author: kevinkelly
// @license: Apache Licence
// @file: mysql.go
// @time: 2026/10/28

package dict

import (
	"context"
	"fmt"
	"strings"
)

// mysqlDialect 覆盖 MySQL、PolarDB、PolarDB-X 以及 SelectDB / Doris / ADB（走 MySQL 协议）。
// 只有前三者开放视图、触发器、存储过程、函数与事件，其余只看表（full 为 true 才开放）。
type mysqlDialect struct{ full bool }

var mysqlSystemDBs = map[string]bool{"information_schema": true, "performance_schema": true, "mysql": true, "sys": true, "test": true}

func (mysqlDialect) IsSystemDB(name string) bool { return mysqlSystemDBs[strings.ToLower(name)] }

func (d mysqlDialect) Kinds() []Kind {
	if d.full {
		return allKinds
	}
	return []Kind{KindTable}
}

func (d mysqlDialect) Databases(ctx context.Context, q Querier) ([]string, error) {
	rs, err := q.Query(ctx, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA ORDER BY SCHEMA_NAME")
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

// quoteMy 给 MySQL 标识符加反引号（名字已通过长度与控制字符校验；仍然转义反引号）。
func quoteMy(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

func (d mysqlDialect) List(ctx context.Context, q Querier, db string, kind Kind) ([]Item, error) {
	var sql string
	switch kind {
	case KindTable:
		sql = "SELECT TABLE_NAME, IFNULL(TABLE_COMMENT, ''), TABLE_ROWS, DATA_LENGTH, INDEX_LENGTH, IFNULL(CREATE_OPTIONS, '') FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_TYPE <> 'VIEW' ORDER BY TABLE_NAME"
	case KindView:
		sql = "SELECT TABLE_NAME, '' FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME"
	case KindTrigger:
		sql = "SELECT TRIGGER_NAME, CONCAT(ACTION_TIMING, ' ', EVENT_MANIPULATION, ' ON ', EVENT_OBJECT_TABLE) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? ORDER BY TRIGGER_NAME"
	case KindProcedure:
		sql = "SELECT ROUTINE_NAME, IFNULL(ROUTINE_COMMENT, '') FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'PROCEDURE' ORDER BY ROUTINE_NAME"
	case KindFunction:
		sql = "SELECT ROUTINE_NAME, IFNULL(ROUTINE_COMMENT, '') FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'FUNCTION' ORDER BY ROUTINE_NAME"
	case KindEvent:
		sql = "SELECT EVENT_NAME, CONCAT(STATUS, IF(EVENT_TYPE = 'RECURRING', CONCAT(' EVERY ', INTERVAL_VALUE, ' ', INTERVAL_FIELD), ' ONE TIME')) FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ? ORDER BY EVENT_NAME"
	default:
		return nil, badReq("未知的对象类别 %q", kind)
	}
	if !d.supports(kind) {
		return nil, newErr(501, "unsupported_kind", "该类型的实例暂不支持查看%s", kindLabel(kind))
	}
	rs, err := q.Query(ctx, sql, db)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(rs.Data))
	for _, r := range rs.Data {
		it := Item{Name: str(r[0]), Comment: str(r[1])}
		if kind == KindTable && len(r) >= 6 { // 估算行数 / 大小 / 分区表标记；取不到（NULL）就留空
			it.RowsEst = intPtr(r[2])
			it.SizeBytes = sumBytes(r[3], r[4])
			it.Partitioned = hasPartitionOption(str(r[5]))
		}
		out = append(out, it)
	}
	return out, nil
}

// ColumnsNamed 返回库里列名在 names 中的列（用于给对象列表统计敏感列数）。
func (d mysqlDialect) ColumnsNamed(ctx context.Context, q Querier, db string, names []string) ([]colRef, error) {
	if len(names) == 0 {
		return nil, nil
	}
	args := []any{db}
	for _, n := range names {
		args = append(args, n)
	}
	rs, err := q.Query(ctx, "SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND COLUMN_NAME IN ("+placeholders("?", len(names))+")", args...)
	if err != nil {
		return nil, err
	}
	out := make([]colRef, 0, len(rs.Data))
	for _, r := range rs.Data {
		out = append(out, colRef{Table: str(r[0]), Column: str(r[1])})
	}
	return out, nil
}

// hasPartitionOption 判断 information_schema.TABLES.CREATE_OPTIONS 是否含 partitioned（分区表）。
func hasPartitionOption(opts string) bool {
	return strings.Contains(strings.ToLower(opts), "partitioned")
}

// at 安全取一行的第 i 列（越界返回 nil）。
func at(r []any, i int) any {
	if i < 0 || i >= len(r) {
		return nil
	}
	return r[i]
}

// intPtr 把数值型的值转成 *int64（NULL / 非数值返回 nil）。
func intPtr(v any) *int64 {
	f, ok := toFloat(v)
	if !ok || f < 0 {
		return nil
	}
	n := int64(f)
	return &n
}

// sumBytes 把数据大小与索引大小相加；两者都取不到返回 nil。
func sumBytes(a, b any) *int64 {
	fa, oka := toFloat(a)
	fb, okb := toFloat(b)
	if !oka && !okb {
		return nil
	}
	n := int64(fa + fb)
	return &n
}

func (d mysqlDialect) supports(k Kind) bool {
	for _, x := range d.Kinds() {
		if x == k {
			return true
		}
	}
	return false
}

func kindLabel(k Kind) string {
	return map[Kind]string{KindTable: "表", KindView: "视图", KindTrigger: "触发器", KindProcedure: "存储过程", KindFunction: "函数", KindEvent: "定时任务"}[k]
}

func notFound(k Kind, name string) *Error {
	return newErr(404, "not_found", "%s %s 不存在，或当前账号没有权限查看", kindLabel(k), name)
}

func (d mysqlDialect) Detail(ctx context.Context, q Querier, db string, kind Kind, _ string, name string) (*Detail, error) {
	if !d.supports(kind) {
		return nil, newErr(501, "unsupported_kind", "该类型的实例暂不支持查看%s", kindLabel(kind))
	}
	det := &Detail{Kind: kind, Name: name}
	switch kind {
	case KindTable:
		return det, d.tableDetail(ctx, q, db, name, det)
	case KindView:
		return det, d.viewDetail(ctx, q, db, name, det)
	case KindTrigger:
		rs, err := q.Query(ctx, `SELECT TRIGGER_NAME, ACTION_TIMING, EVENT_MANIPULATION, EVENT_OBJECT_TABLE, ACTION_ORIENTATION, DEFINER, CREATED, SQL_MODE,
CHARACTER_SET_CLIENT, COLLATION_CONNECTION, ACTION_STATEMENT FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? AND TRIGGER_NAME = ?`, db, name)
		if err != nil {
			return nil, err
		}
		if len(rs.Data) == 0 {
			return nil, notFound(kind, name)
		}
		r := rs.Data[0]
		det.Sections = []Section{kvSec("触发器信息", []Pair{{"名称", str(r[0])}, {"触发时机", str(r[1])}, {"事件", str(r[2])}, {"所属表", str(r[3])}, {"粒度", str(r[4])},
			{"定义者", str(r[5])}, {"创建时间", str(r[6])}, {"SQL_MODE", str(r[7])}, {"客户端字符集", str(r[8])}, {"连接排序规则", str(r[9])}}), codeSec("触发器语句", str(r[10]))}
	case KindProcedure, KindFunction:
		typ, show := "PROCEDURE", "SHOW CREATE PROCEDURE "
		if kind == KindFunction {
			typ, show = "FUNCTION", "SHOW CREATE FUNCTION "
		}
		rs, err := q.Query(ctx, `SELECT ROUTINE_NAME, ROUTINE_SCHEMA, IFNULL(DTD_IDENTIFIER, ''), DEFINER, CREATED, LAST_ALTERED, SQL_MODE, SECURITY_TYPE, ROUTINE_COMMENT
FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ? AND ROUTINE_NAME = ? AND ROUTINE_TYPE = ?`, db, name, typ)
		if err != nil {
			return nil, err
		}
		if len(rs.Data) == 0 {
			return nil, notFound(kind, name)
		}
		r := rs.Data[0]
		pairs := []Pair{{"名称", str(r[0])}, {"所属库", str(r[1])}, {"定义者", str(r[3])}, {"创建时间", str(r[4])}, {"最后修改", str(r[5])}, {"SQL_MODE", str(r[6])}, {"安全类型", str(r[7])}, {"说明", str(r[8])}}
		if kind == KindFunction {
			pairs = append(pairs[:2], append([]Pair{{"返回类型", str(r[2])}}, pairs[2:]...)...)
		}
		det.Sections = []Section{kvSec("基本信息", pairs), d.showCreate(ctx, q, "定义", show+quoteMy(db)+"."+quoteMy(name), 2)}
	case KindEvent:
		rs, err := q.Query(ctx, `SELECT EVENT_NAME, EVENT_SCHEMA, DEFINER, EVENT_TYPE, INTERVAL_VALUE, INTERVAL_FIELD, STATUS, EXECUTE_AT, STARTS, ENDS, LAST_EXECUTED,
ON_COMPLETION, CREATED, LAST_ALTERED, EVENT_COMMENT FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ? AND EVENT_NAME = ?`, db, name)
		if err != nil {
			return nil, err
		}
		if len(rs.Data) == 0 {
			return nil, notFound(kind, name)
		}
		r := rs.Data[0]
		det.Sections = []Section{kvSec("定时任务信息", []Pair{{"名称", str(r[0])}, {"所属库", str(r[1])}, {"定义者", str(r[2])}, {"类型", str(r[3])}, {"间隔", strings.TrimSpace(str(r[4]) + " " + str(r[5]))},
			{"状态", str(r[6])}, {"执行时间点", str(r[7])}, {"开始", str(r[8])}, {"结束", str(r[9])}, {"最近执行", str(r[10])}, {"完成后", str(r[11])},
			{"创建时间", str(r[12])}, {"最后修改", str(r[13])}, {"说明", str(r[14])}}),
			d.showCreate(ctx, q, "定义", "SHOW CREATE EVENT "+quoteMy(db)+"."+quoteMy(name), 3)}
	}
	return det, nil
}

func kvSec(title string, pairs []Pair) Section {
	return Section{Title: title, Type: "kv", Pairs: pairs}
}
func codeSec(title, text string) Section { return Section{Title: title, Type: "code", Text: text} }
func noteSec(title, text string) Section { return Section{Title: title, Type: "note", Text: text} }

// showCreate 执行 SHOW CREATE ...，取第 col 列（从 0 数）。失败（常见原因是没有查看定义的权限）不让整个详情失败，只给一段说明。
func (d mysqlDialect) showCreate(ctx context.Context, q Querier, title, sql string, col int) Section {
	rs, err := q.Query(ctx, sql)
	if err != nil {
		return noteSec(title, fmt.Sprintf("获取失败：%s", err.Error()))
	}
	if len(rs.Data) == 0 || len(rs.Data[0]) <= col {
		return noteSec(title, "没有取到定义（可能没有查看定义的权限）")
	}
	return codeSec(title, str(rs.Data[0][col]))
}

func (d mysqlDialect) tableDetail(ctx context.Context, q Querier, db, name string, det *Detail) error {
	rs, err := q.Query(ctx, `SELECT TABLE_NAME, ENGINE, ROW_FORMAT, TABLE_ROWS, AVG_ROW_LENGTH, DATA_LENGTH, INDEX_LENGTH, DATA_FREE, AUTO_INCREMENT,
TABLE_COLLATION, CREATE_TIME, UPDATE_TIME, TABLE_COMMENT, IFNULL(CREATE_OPTIONS, '') FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND TABLE_TYPE <> 'VIEW'`, db, name)
	var infoSec Section
	if err != nil {
		// 兼容实现（PolarDB-X / DRDS 等）的 information_schema.TABLES 可能缺某些列：退到对象列表用的那几列，仍不行就只给说明，
		// 不让整个详情失败（列信息、索引、建表语句照样展示）。
		infoSec = d.tableInfoFallback(ctx, q, db, name, err)
	} else {
		if len(rs.Data) == 0 {
			return notFound(KindTable, name)
		}
		r := rs.Data[0]
		pairs := []Pair{{"表名", str(r[0])}, {"引擎", str(r[1])}, {"行格式", str(r[2])}, {"表行数（估算）", str(r[3])},
			{"平均行长度", str(r[4])}, {"数据大小(KB)", kb(r[5])}, {"索引大小(KB)", kb(r[6])}, {"碎片大小(KB)", kb(r[7])}, {"当前自增值", str(r[8])},
			{"字符集排序规则", str(r[9])}, {"创建时间", str(r[10])}, {"更新时间", str(r[11])}, {"表说明", str(r[12])}}
		if len(r) > 13 {
			part := "否"
			if hasPartitionOption(str(r[13])) {
				part = "是"
			}
			pairs = append(pairs, Pair{"分区表", part})
		}
		infoSec = kvSec("表信息", pairs)
	}
	det.Sections = append(det.Sections, infoSec)
	det.Sections = append(det.Sections, d.columnsSection(ctx, q, db, name))

	idx, err := d.indexes(ctx, q, db, name)
	if err != nil {
		det.Sections = append(det.Sections, noteSec("索引信息", fmt.Sprintf("获取失败：%s", err.Error())))
	} else {
		det.Sections = append(det.Sections, Section{Title: "索引信息", Type: "table", Columns: indexHeads, Rows: idx[name]})
	}
	det.Sections = append(det.Sections, d.createStatement(ctx, q, db, name))
	return nil
}

// createStatement 取“建表语句”。先用 SHOW CREATE TABLE；取不到（Doris / SelectDB / StarRocks 的物化视图在
// information_schema.TABLES 里和表一起列出，但 SHOW CREATE TABLE 对它报错或返回空）就改用
// SHOW CREATE MATERIALIZED VIEW。两种都取不到时给出两个原因，不让整个详情失败。
func (d mysqlDialect) createStatement(ctx context.Context, q Querier, db, name string) Section {
	target := quoteMy(db) + "." + quoteMy(name)
	text, firstErr := d.createText(ctx, q, "SHOW CREATE TABLE "+target)
	if firstErr == "" {
		return codeSec("建表语句", text)
	}
	text, mvErr := d.createText(ctx, q, "SHOW CREATE MATERIALIZED VIEW "+target)
	if mvErr == "" {
		return codeSec("建表语句（物化视图）", text)
	}
	return noteSec("建表语句", fmt.Sprintf("获取失败：%s（按物化视图读取也失败：%s）", firstErr, mvErr))
}

// createText 执行一条 SHOW CREATE ...，返回第 2 列的文本；失败返回原因（成功时原因为空串）。
func (d mysqlDialect) createText(ctx context.Context, q Querier, sql string) (string, string) {
	rs, err := q.Query(ctx, sql)
	if err != nil {
		return "", err.Error()
	}
	if len(rs.Data) == 0 || len(rs.Data[0]) < 2 || strings.TrimSpace(str(rs.Data[0][1])) == "" {
		return "", "没有取到定义（可能没有查看定义的权限）"
	}
	return str(rs.Data[0][1]), ""
}

// tableInfoFallback 在完整的表信息查询失败后，退回只取少数基础列；还失败就返回带原因的说明段。
func (d mysqlDialect) tableInfoFallback(ctx context.Context, q Querier, db, name string, first error) Section {
	rs, err := q.Query(ctx, `SELECT TABLE_NAME, TABLE_COMMENT, TABLE_ROWS, DATA_LENGTH, INDEX_LENGTH FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, db, name)
	if err != nil || len(rs.Data) == 0 {
		if err != nil {
			return noteSec("表信息", fmt.Sprintf("获取失败：%s；基础信息也读取失败：%s", first.Error(), err.Error()))
		}
		return noteSec("表信息", fmt.Sprintf("获取失败：%s", first.Error()))
	}
	r := rs.Data[0]
	return kvSec("表信息", []Pair{{"表名", str(r[0])}, {"表行数（估算）", str(r[2])}, {"数据大小(KB)", kb(r[3])}, {"索引大小(KB)", kb(r[4])}, {"表说明", str(r[1])}})
}

// columnsSection 取表 / 视图的列信息：先查 information_schema.COLUMNS，失败再用 SHOW FULL COLUMNS（PolarDB-X 等兼容实现的
// information_schema 不一定支持所有列 / 排序），都失败给出带原因的说明段，不让整个详情失败。
func (d mysqlDialect) columnsSection(ctx context.Context, q Querier, db, name string) Section {
	cols, err := q.Query(ctx, `SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT, COLUMN_KEY, EXTRA, COLUMN_COMMENT
FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, db, name)
	if err == nil {
		return Section{Title: "列信息", Type: "table", Columns: columnHeads, Rows: myColumnRows(cols.Data)}
	}
	rs, err2 := q.Query(ctx, "SHOW FULL COLUMNS FROM "+quoteMy(name)+" FROM "+quoteMy(db))
	if err2 != nil {
		return noteSec("列信息", fmt.Sprintf("获取失败：%s；改用 SHOW FULL COLUMNS 也失败：%s", err.Error(), err2.Error()))
	}
	idx := map[string]int{}
	for i, c := range rs.Columns {
		idx[strings.ToLower(c)] = i
	}
	get := func(r []any, k string) any {
		if i, ok := idx[k]; ok {
			return at(r, i)
		}
		return nil
	}
	rows := make([][]string, 0, len(rs.Data))
	for _, r := range rs.Data {
		rows = append(rows, []string{str(get(r, "field")), str(get(r, "type")), yesNo(get(r, "null")), strNull(get(r, "default"), "NULL"), str(get(r, "key")), str(get(r, "extra")), str(get(r, "comment"))})
	}
	return Section{Title: "列信息", Type: "table", Columns: columnHeads, Rows: rows}
}

func myColumnRows(data [][]any) [][]string {
	rows := make([][]string, 0, len(data))
	for _, r := range data {
		rows = append(rows, []string{str(r[0]), str(r[1]), yesNo(r[2]), strNull(r[3], "NULL"), str(r[4]), str(r[5]), str(r[6])})
	}
	return rows
}

// indexes 取一张表（name 非空）的索引；name 为空串时取整个库。
func (d mysqlDialect) indexes(ctx context.Context, q Querier, db, name string) (map[string][][]string, error) {
	sql := "SELECT TABLE_NAME, INDEX_NAME, NON_UNIQUE, INDEX_TYPE, COLUMN_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ?"
	args := []any{db}
	if name != "" {
		sql += " AND TABLE_NAME = ?"
		args = append(args, name)
	}
	rs, err := q.Query(ctx, sql+" ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX", args...)
	if err != nil {
		return nil, err
	}
	rows := make([]indexRow, 0, len(rs.Data))
	for _, r := range rs.Data {
		uniq := "是"
		if v, _ := toFloat(r[2]); v != 0 {
			uniq = "否"
		}
		rows = append(rows, indexRow{table: str(r[0]), name: str(r[1]), unique: uniq, typ: str(r[3]), col: str(r[4])})
	}
	return aggregateIndexes(rows), nil
}

func (d mysqlDialect) viewDetail(ctx context.Context, q Querier, db, name string, det *Detail) error {
	rs, err := q.Query(ctx, `SELECT TABLE_NAME, VIEW_DEFINITION, CHECK_OPTION, IS_UPDATABLE, DEFINER, SECURITY_TYPE, CHARACTER_SET_CLIENT, COLLATION_CONNECTION
FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, db, name)
	if err != nil {
		return err
	}
	if len(rs.Data) == 0 {
		return notFound(KindView, name)
	}
	r := rs.Data[0]
	pairs := []Pair{{"视图名", str(r[0])}, {"检查选项", str(r[2])}, {"可更新", yesNo(r[3])}, {"安全类型", str(r[5])}, {"定义者", str(r[4])}, {"客户端字符集", str(r[6])}, {"连接排序规则", str(r[7])}}
	det.Sections = append(det.Sections, kvSec("视图信息", pairs))
	det.Sections = append(det.Sections, d.columnsSection(ctx, q, db, name))
	if def := str(r[1]); def == "" {
		det.Sections = append(det.Sections, noteSec("视图定义", "没有取到定义（可能没有 SHOW VIEW 权限）"))
	} else {
		det.Sections = append(det.Sections, codeSec("视图定义", def))
	}
	return nil
}
