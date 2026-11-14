// @author: kevinkelly
// @license: Apache Licence
// @file: meta.go
// @time: 2026/10/28

package query

import (
	"context"
	"fmt"
	"strings"

	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// 库表树需要的元数据：表（类型、估算行数、注释、是否无权限、敏感列）、列（类型、主键、注释、敏感标记）、按字段名 / 注释搜索。
// 元数据查询用实例账号直接发给引擎（SHOW TABLE STATUS / SHOW FULL COLUMNS / information_schema），标识符一律反引号或方括号转义，
// 值一律字面量转义；暴露范围与已有的 /tables、/columns 接口相同（只要能访问该实例），但库名要通过实例的库名过滤规则并禁止 mysql 系统库。

// TableMeta 是库表树里的一张表或视图。
type TableMeta struct {
	Name      string   `json:"name"`
	View      bool     `json:"view"`
	Rows      int64    `json:"rows"` // 估算行数，-1 表示未知
	Comment   string   `json:"comment"`
	Locked    bool     `json:"locked"`    // 当前用户没有该表的查询权限
	Sensitive []string `json:"sensitive"` // 命中脱敏规则的字段

	// 以下仅 PostgreSQL / MongoDB / ClickHouse 返回。
	Schema string `json:"schema,omitempty"` // PostgreSQL：schema 名（Name 为 schema.table）
	Kind   string `json:"kind,omitempty"`   // table / view / matview / foreign / partitioned / dictionary / collection / timeseries
}

// DBMeta 是一个库的元数据。
type DBMeta struct {
	DB             string      `json:"db"`
	Tables         []TableMeta `json:"tables"`
	SensitiveTotal int         `json:"sensitive_total"`
	LockedTotal    int         `json:"locked_total"`
	Policy         PolicyInfo  `json:"policy"`
	Unmasked       bool        `json:"unmasked,omitempty"` // 仅 PostgreSQL / MongoDB / ClickHouse：系统脱敏总开关关闭，查询结果不做脱敏
}

// ColumnMeta 是表的一列。
type ColumnMeta struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Key       string `json:"key,omitempty"` // PRI / UNI / MUL
	Nullable  bool   `json:"nullable"`
	Comment   string `json:"comment,omitempty"`
	Sensitive bool   `json:"sensitive"`
}

// ColumnHit 是按字段名 / 注释搜索的一条命中。
type ColumnHit struct {
	Table   string `json:"table"`
	Column  string `json:"column"`
	Comment string `json:"comment,omitempty"`
}

func sqlLit(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(s, `\`, `\\`)
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func tsqlLit(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// litFor 按实例类型选字符串字面量转义：MySQL 系把反斜杠当转义符，T-SQL 不是。
func litFor(dbType string) func(string) string {
	if isMSSQL(dbType) {
		return tsqlLit
	}
	return sqlLit
}

func quoteBT(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
func quoteBR(s string) string { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }

var likeEsc = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// maskMatcher 判断某个 库.表.字段 是否命中脱敏字段配置（规则同 masking.Analyze：库表字段不分大小写精确匹配，或库表都是 * 的通配）。
type maskMatcher struct{ cols []model.DataMaskingColumn }

func (m maskMatcher) names() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range m.cols {
		if n := strings.ToLower(c.ColumnName); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, c.ColumnName)
		}
	}
	return out
}

func (m maskMatcher) match(db, table, col string) bool {
	for _, c := range m.cols {
		// MongoDB 的字段是点号路径，配置里的数组标记 [] 比较时去掉；PostgreSQL / SQL Server 的表是 架构.表，旧配置可能只写了表名
		if !strings.EqualFold(strings.ReplaceAll(c.ColumnName, "[]", ""), col) {
			continue
		}
		if c.TableSchema == "*" && c.TableName_ == "*" {
			return true
		}
		if !strings.EqualFold(c.TableSchema, db) {
			continue
		}
		if strings.EqualFold(c.TableName_, table) {
			return true
		}
		if i := strings.LastIndex(table, "."); i >= 0 && !strings.Contains(c.TableName_, ".") && strings.EqualFold(c.TableName_, table[i+1:]) {
			return true
		}
	}
	return false
}

func (s *Service) maskMatcher(inst *model.Instance) maskMatcher {
	cols, err := s.repo.ActiveMaskingColumns(inst.ID)
	if err != nil {
		return maskMatcher{}
	}
	return maskMatcher{cols: cols}
}

// checkMetaDB 校验库名可用于元数据查询：符合实例的库名过滤规则，且不是 mysql 系统库。
func (s *Service) checkMetaDB(inst *model.Instance, db string) error {
	if strings.TrimSpace(db) == "" {
		return newErr(400, "bad_request", "请选择数据库")
	}
	if strings.EqualFold(db, "mysql") {
		return newErr(403, "no_privilege", "不允许访问 mysql 系统库")
	}
	ok, err := filterDBs([]string{db}, inst.ShowDBNameRegex, inst.DeniedDBNameRegex)
	if err != nil {
		return err
	}
	if len(ok) == 0 {
		return newErr(403, "no_privilege", "该数据库不在你可访问的范围内")
	}
	return nil
}

func rowStr(row []any, i int) string { return cellStr(row, i) }

// DBMeta 返回库里的全部表 / 视图及其附加信息。
func (s *Service) DBMeta(ctx context.Context, p Principal, id uint, db string) (out *DBMeta, err error) {
	err = s.withEngine(ctx, p, id, func(e engine.Engine, inst *model.Instance) error {
		if isRedis(inst.DBType) { // Redis 没有表，只返回限制策略
			out, err = s.redisMeta(ctx, p, inst, db)
			return err
		}
		if isMulti(inst.DBType) { // PostgreSQL / MongoDB / ClickHouse：元数据来自各引擎的 TableInfos
			out, err = s.multiMeta(ctx, p, e, inst, db)
			return err
		}
		if err := s.checkMetaDB(inst, db); err != nil {
			return err
		}
		tables, err := s.listTables(ctx, e, inst, db)
		if err != nil {
			return err
		}
		out = &DBMeta{DB: db, Tables: tables}
		if pp, ok := s.auth.(PolicyProvider); ok {
			if info, err := pp.PolicyInfo(ctx, p, inst, db, s.Policy().AdminLimit); err == nil {
				out.Policy = info
			}
		}
		if out.Policy.Mode == "" {
			out.Policy = PolicyInfo{Mode: "unknown", Tables: []string{}}
		}
		sens := s.sensitiveInDB(ctx, e, inst, db)
		for i := range out.Tables {
			t := &out.Tables[i]
			t.Locked = !out.Policy.HasTable(t.Name) && out.Policy.Mode != "unknown"
			if t.Locked {
				out.LockedTotal++
			}
			t.Sensitive = sens[strings.ToLower(t.Name)]
			if t.Sensitive == nil {
				t.Sensitive = []string{}
			}
			out.SensitiveTotal += len(t.Sensitive)
		}
		return nil
	})
	return
}

func (s *Service) listTables(ctx context.Context, e engine.Engine, inst *model.Instance, db string) ([]TableMeta, error) {
	var q string
	if isMSSQL(inst.DBType) {
		q = "SELECT s.name + '.' + o.name AS name, CASE o.type WHEN 'V' THEN 'VIEW' ELSE 'TABLE' END AS type, " +
			"(SELECT SUM(ps.row_count) FROM " + quoteBR(db) + ".sys.dm_db_partition_stats ps WHERE ps.object_id = o.object_id AND ps.index_id IN (0,1)) AS cnt " +
			"FROM " + quoteBR(db) + ".sys.objects o JOIN " + quoteBR(db) + ".sys.schemas s ON s.schema_id = o.schema_id WHERE o.type IN ('U','V') ORDER BY s.name, o.name"
		rs, err := e.Query(ctx, q, engine.QueryOptions{MaxExecMs: 15000})
		if err != nil {
			// 没有 VIEW DATABASE STATE 权限等：退回只取名字
			return s.namesOnly(ctx, e, db)
		}
		out := make([]TableMeta, 0, len(rs.Rows))
		for _, r := range rs.Rows {
			t := TableMeta{Name: rowStr(r, 0), View: rowStr(r, 1) == "VIEW", Rows: -1}
			if n, ok := toInt64(r[2]); ok && r[2] != nil && !t.View {
				t.Rows = n
			}
			out = append(out, t)
		}
		return out, nil
	}
	rs, err := e.Query(ctx, "SHOW TABLE STATUS FROM "+quoteBT(db), engine.QueryOptions{MaxExecMs: 15000})
	if err != nil {
		return s.namesOnly(ctx, e, db)
	}
	ni, ri, ci := colIndex(rs.ColumnList, "Name"), colIndex(rs.ColumnList, "Rows"), colIndex(rs.ColumnList, "Comment")
	if ni < 0 {
		return s.namesOnly(ctx, e, db)
	}
	out := make([]TableMeta, 0, len(rs.Rows))
	for _, r := range rs.Rows {
		t := TableMeta{Name: rowStr(r, ni), Rows: -1, Comment: rowStr(r, ci)}
		if t.Comment == "VIEW" {
			t.View, t.Comment = true, ""
		} else if ri >= 0 && ri < len(r) && r[ri] != nil {
			if n, ok := toInt64(r[ri]); ok {
				t.Rows = n
			}
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *Service) namesOnly(ctx context.Context, e engine.Engine, db string) ([]TableMeta, error) {
	names, err := e.AllTables(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make([]TableMeta, 0, len(names))
	for _, n := range names {
		out = append(out, TableMeta{Name: n, Rows: -1})
	}
	return out, nil
}

// sensitiveInDB 返回库里命中脱敏配置的字段：表名（小写）→ 字段名。取不到时返回空（不影响页面）。
func (s *Service) sensitiveInDB(ctx context.Context, e engine.Engine, inst *model.Instance, db string) map[string][]string {
	out := map[string][]string{}
	m := s.maskMatcher(inst)
	names := m.names()
	if len(names) == 0 {
		return out
	}
	if len(names) > 300 {
		names = names[:300]
	}
	lits := make([]string, len(names))
	for i, n := range names {
		lits[i] = litFor(inst.DBType)(strings.ToLower(n))
	}
	var q string
	if isMSSQL(inst.DBType) {
		q = "SELECT s.name + '.' + t.name, c.name FROM " + quoteBR(db) + ".sys.columns c JOIN " + quoteBR(db) + ".sys.objects t ON t.object_id = c.object_id JOIN " +
			quoteBR(db) + ".sys.schemas s ON s.schema_id = t.schema_id WHERE t.type IN ('U','V') AND LOWER(c.name) IN (" + strings.Join(lits, ",") + ")"
	} else {
		q = "SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = " + litFor(inst.DBType)(db) +
			" AND LOWER(COLUMN_NAME) IN (" + strings.Join(lits, ",") + ")"
	}
	rs, err := e.Query(ctx, q, engine.QueryOptions{MaxExecMs: 10000})
	if err != nil {
		return out
	}
	for _, r := range rs.Rows {
		tbl, col := rowStr(r, 0), rowStr(r, 1)
		match := tbl
		if i := strings.LastIndex(tbl, "."); i >= 0 && isMSSQL(inst.DBType) {
			match = tbl[i+1:]
		}
		if m.match(db, match, col) || m.match(db, tbl, col) {
			k := strings.ToLower(tbl)
			out[k] = append(out[k], col)
		}
	}
	return out
}

// TableColumns 返回一张表的列信息（类型、主键、注释、敏感标记）。
func (s *Service) TableColumns(ctx context.Context, p Principal, id uint, db, table string) (out []ColumnMeta, err error) {
	err = s.withEngine(ctx, p, id, func(e engine.Engine, inst *model.Instance) error {
		if isMulti(inst.DBType) {
			out, err = s.multiColumns(ctx, e, inst, db, table)
			return err
		}
		if err := s.checkMetaDB(inst, db); err != nil {
			return err
		}
		m := s.maskMatcher(inst)
		var rs *engine.ResultSet
		var err error
		if isMSSQL(inst.DBType) {
			schema, name := "dbo", table
			if i := strings.LastIndex(table, "."); i >= 0 {
				schema, name = table[:i], table[i+1:]
			}
			q := "SELECT c.name, ty.name + CASE WHEN ty.name IN ('varchar','nvarchar','char','nchar','varbinary','binary') THEN '(' + CASE WHEN c.max_length = -1 THEN 'max' WHEN ty.name LIKE 'n%' THEN CAST(c.max_length/2 AS varchar(10)) ELSE CAST(c.max_length AS varchar(10)) END + ')' " +
				"WHEN ty.name IN ('decimal','numeric') THEN '(' + CAST(c.precision AS varchar(10)) + ',' + CAST(c.scale AS varchar(10)) + ')' ELSE '' END AS type, " +
				"c.is_nullable, CASE WHEN EXISTS (SELECT 1 FROM " + quoteBR(db) + ".sys.index_columns ic JOIN " + quoteBR(db) + ".sys.indexes i ON i.object_id = ic.object_id AND i.index_id = ic.index_id " +
				"WHERE ic.object_id = c.object_id AND ic.column_id = c.column_id AND i.is_primary_key = 1) THEN 'PRI' ELSE '' END AS k " +
				"FROM " + quoteBR(db) + ".sys.columns c JOIN " + quoteBR(db) + ".sys.objects t ON t.object_id = c.object_id JOIN " + quoteBR(db) + ".sys.schemas s ON s.schema_id = t.schema_id " +
				"JOIN " + quoteBR(db) + ".sys.types ty ON ty.user_type_id = c.user_type_id WHERE s.name = " + tsqlLit(schema) + " AND t.name = " + tsqlLit(name) + " ORDER BY c.column_id"
			rs, err = e.Query(ctx, q, engine.QueryOptions{MaxExecMs: 10000})
			if err != nil {
				return err
			}
			for _, r := range rs.Rows {
				c := ColumnMeta{Name: rowStr(r, 0), Type: rowStr(r, 1), Key: rowStr(r, 3), Nullable: rowStr(r, 2) == "true" || rowStr(r, 2) == "1"}
				c.Sensitive = m.match(db, name, c.Name) || m.match(db, table, c.Name)
				out = append(out, c)
			}
			return nil
		}
		rs, err = e.Query(ctx, "SHOW FULL COLUMNS FROM "+quoteBT(db)+"."+quoteBT(table), engine.QueryOptions{MaxExecMs: 10000})
		if err != nil {
			return err
		}
		fi, ti, ni, ki, ci := colIndex(rs.ColumnList, "Field"), colIndex(rs.ColumnList, "Type"), colIndex(rs.ColumnList, "Null"), colIndex(rs.ColumnList, "Key"), colIndex(rs.ColumnList, "Comment")
		if fi < 0 {
			return fmt.Errorf("无法解析列信息")
		}
		for _, r := range rs.Rows {
			c := ColumnMeta{Name: rowStr(r, fi), Type: rowStr(r, ti), Key: rowStr(r, ki), Nullable: strings.EqualFold(rowStr(r, ni), "YES"), Comment: rowStr(r, ci)}
			c.Sensitive = m.match(db, table, c.Name)
			out = append(out, c)
		}
		return nil
	})
	if out == nil {
		out = []ColumnMeta{}
	}
	return
}

// SearchColumns 按字段名 / 字段注释模糊搜索库里的列（最多 500 条）。
func (s *Service) SearchColumns(ctx context.Context, p Principal, id uint, db, kw string) (out []ColumnHit, err error) {
	kw = strings.TrimSpace(kw)
	if kw == "" {
		return []ColumnHit{}, nil
	}
	if len([]rune(kw)) > 64 {
		kw = string([]rune(kw)[:64])
	}
	err = s.withEngine(ctx, p, id, func(e engine.Engine, inst *model.Instance) error {
		if isMulti(inst.DBType) {
			out, err = s.multiSearch(ctx, e, inst, db, kw)
			return err
		}
		if err := s.checkMetaDB(inst, db); err != nil {
			return err
		}
		pat := sqlLit("%" + likeEsc.Replace(kw) + "%")
		var q string
		if isMSSQL(inst.DBType) {
			// T-SQL 的 LIKE 用方括号转义通配符
			tp := tsqlLit("%" + strings.NewReplacer("[", "[[]", "%", "[%]", "_", "[_]").Replace(kw) + "%")
			q = "SELECT TOP 500 s.name + '.' + t.name, c.name, '' FROM " + quoteBR(db) + ".sys.columns c JOIN " + quoteBR(db) + ".sys.objects t ON t.object_id = c.object_id JOIN " +
				quoteBR(db) + ".sys.schemas s ON s.schema_id = t.schema_id WHERE t.type IN ('U','V') AND c.name LIKE " + tp + " ORDER BY 1, 2"
		} else {
			q = "SELECT TABLE_NAME, COLUMN_NAME, COLUMN_COMMENT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = " + litFor(inst.DBType)(db) +
				" AND (COLUMN_NAME LIKE " + pat + " OR COLUMN_COMMENT LIKE " + pat + ") ORDER BY TABLE_NAME, ORDINAL_POSITION LIMIT 500"
		}
		rs, err := e.Query(ctx, q, engine.QueryOptions{MaxExecMs: 10000})
		if err != nil {
			return err
		}
		for _, r := range rs.Rows {
			out = append(out, ColumnHit{Table: rowStr(r, 0), Column: rowStr(r, 1), Comment: rowStr(r, 2)})
		}
		return nil
	})
	if out == nil {
		out = []ColumnHit{}
	}
	return
}
