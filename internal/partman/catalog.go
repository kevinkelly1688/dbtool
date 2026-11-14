// @author: kevinkelly
// @license: Apache Licence
// @file: catalog.go
// @time: 2026/10/28

package partman

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"dbtool/internal/model"
)

// 新增 / 编辑配置表单的“库 -> 表 -> 分区字段”级联数据：从目标实例的 information_schema 现读，不落库。
// Catalog 是 Target 的可选能力（用类型断言取得），没有实现的 Target 返回 501，不影响维护本身。

// TableInfo 是库里的一张普通表。
type TableInfo struct {
	Name        string `json:"name"`
	Partitioned bool   `json:"partitioned"`      // 已经是分区表（本功能只续建 / 清理已有的 RANGE 分区）
	Method      string `json:"method,omitempty"` // RANGE / RANGE COLUMNS / LIST ...
	// Unknown 为 true 表示没有读分区信息（SelectDB / Doris 的库表列表只来自 information_schema.TABLES），partitioned 不可信。
	Unknown bool `json:"partition_unknown,omitempty"`
	// Configured 为 true 表示这张表在本系统里已经有分区维护配置（dbt_partman_config，同一实例的同库同表只能有一条）。
	// 由 Service.Tables 按元数据库填写，目标库里读不到。
	Configured bool `json:"configured,omitempty"`
}

// ColumnInfo 是可作分区字段的列：只有 datetime、date、int、bigint（与配置里的字段类型枚举一一对应）。
type ColumnInfo struct {
	Name       string `json:"name"`
	DataType   string `json:"data_type"`   // information_schema.COLUMNS.DATA_TYPE
	ColumnType string `json:"column_type"` // 完整类型，含 unsigned
	ColType    string `json:"col_type"`    // 对应配置里的字段类型：int / bigint / datetime / date
	Comment    string `json:"comment,omitempty"`
}

// Catalog 读取目标实例的库、表、列。
type Catalog interface {
	Databases(ctx context.Context) ([]string, error)
	Tables(ctx context.Context, db string) ([]TableInfo, error)
	Columns(ctx context.Context, db, table string) ([]ColumnInfo, error)
}

const maxCatalogTables = 3000

// partColType 把列类型换成配置里的字段类型；不能作分区字段的返回 ""。
// timestamp、mediumint 等不在列：BuildPlan 要求配置类型与列的实际类型完全一致，列了也建不出计划。
func partColType(dataType string) string {
	switch normDorisType(dataType) { // datetimev2 / datev2 是 Doris 的写法
	case "datetime":
		return ColDatetime
	case "date":
		return ColDate
	case "bigint":
		return ColBigint
	case "int":
		return ColInt
	}
	return ""
}

func (t *mysqlTarget) Databases(ctx context.Context) ([]string, error) {
	rows, err := t.db.QueryContext(ctx, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA ORDER BY SCHEMA_NAME")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Tables 只列普通表：information_schema.PARTITIONS 对非分区表也有一行（PARTITION_NAME 为 NULL），视图不在其中。
func (t *mysqlTarget) Tables(ctx context.Context, db string) ([]TableInfo, error) {
	rows, err := t.db.QueryContext(ctx, `SELECT TABLE_NAME, MAX(PARTITION_NAME IS NOT NULL), MAX(PARTITION_METHOD)
FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA = ? GROUP BY TABLE_NAME ORDER BY TABLE_NAME LIMIT ?`, db, maxCatalogTables)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TableInfo
	for rows.Next() {
		var name string
		var parted int
		var method sql.NullString
		if err := rows.Scan(&name, &parted, &method); err != nil {
			return nil, err
		}
		ti := TableInfo{Name: name, Partitioned: parted == 1}
		if ti.Partitioned {
			ti.Method = strings.ToUpper(method.String)
		}
		out = append(out, ti)
	}
	return out, rows.Err()
}

// Tables（Doris）：information_schema.PARTITIONS 在 Doris 上不可靠，只列普通表，是否分区不判断（Unknown）。
func (t *dorisTarget) Tables(ctx context.Context, db string) ([]TableInfo, error) {
	rows, err := t.db.QueryContext(ctx, `SELECT TABLE_NAME FROM information_schema.TABLES
WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_NAME LIMIT ?`, db, maxCatalogTables)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TableInfo
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, TableInfo{Name: name, Unknown: true})
	}
	return out, rows.Err()
}

func (t *mysqlTarget) Columns(ctx context.Context, db, table string) ([]ColumnInfo, error) {
	rows, err := t.db.QueryContext(ctx, `SELECT COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, COLUMN_COMMENT
FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, db, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColumnInfo
	for rows.Next() {
		var c ColumnInfo
		if err := rows.Scan(&c.Name, &c.DataType, &c.ColumnType, &c.Comment); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- 服务层 ----

const catalogTimeout = 15 * time.Second

// openCatalog 校验权限与实例、取得目标库的 Catalog。release 必须调用。
func (s *Service) openCatalog(ctx context.Context, c Caller, instID uint) (qctx context.Context, cat Catalog, inst *model.Instance, release func(), err error) {
	if !c.has(permAdd) && !c.has(permChange) {
		return nil, nil, nil, nil, newErr(403, "forbidden", "没有权限选择库表（需要 %s 或 %s）", permAdd, permChange)
	}
	inst, err = s.instance(c, instID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if !SupportedDBType(inst.DBType) {
		return nil, nil, nil, nil, unsupportedErr(inst.DBType)
	}
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	slot, err := s.acquireSlots(ctx, inst.ID)
	if err != nil {
		cancel()
		return nil, nil, nil, nil, newErr(503, "busy", "该实例上正在读取的任务较多，请稍后重试")
	}
	tgt, err := s.dial(ctx, inst, s.meta.EngineConn(inst))
	if err != nil {
		slot()
		cancel()
		return nil, nil, nil, nil, targetErr("连接目标实例失败：%v", err)
	}
	cat, ok := tgt.(Catalog)
	if !ok {
		tgt.Close()
		slot()
		cancel()
		return nil, nil, nil, nil, newErr(501, "unsupported_engine", "该实例的连接不支持浏览库表，请手工填写")
	}
	return ctx, cat, inst, func() { tgt.Close(); slot(); cancel() }, nil
}

// Databases 列出实例上可选的库：去掉系统库与被实例库名过滤规则排除的库，名字必须满足标识符白名单。
func (s *Service) Databases(ctx context.Context, c Caller, instID uint) ([]string, error) {
	qctx, cat, inst, release, err := s.openCatalog(ctx, c, instID)
	if err != nil {
		return nil, err
	}
	defer release()
	all, err := cat.Databases(qctx)
	if err != nil {
		return nil, targetErr("读取库列表失败：%v", err)
	}
	out := []string{}
	for _, d := range all {
		if !validIdent(d) || systemDBs[strings.ToLower(d)] || dbAllowed(inst, d) != nil {
			continue
		}
		out = append(out, d)
	}
	sort.Strings(out)
	return out, nil
}

// Tables 列出库里的普通表并标注是否分区表。
func (s *Service) Tables(ctx context.Context, c Caller, instID uint, db string) ([]TableInfo, error) {
	db = strings.TrimSpace(db)
	if !validIdent(db) || systemDBs[strings.ToLower(db)] {
		return nil, badReq("库名 %s 不合法或是系统库", qstr(db))
	}
	qctx, cat, inst, release, err := s.openCatalog(ctx, c, instID)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := dbAllowed(inst, db); err != nil {
		return nil, err
	}
	all, err := cat.Tables(qctx, db)
	if err != nil {
		return nil, targetErr("读取表列表失败：%v", err)
	}
	out := []TableInfo{}
	for _, t := range all {
		if validIdent(t.Name) {
			out = append(out, t)
		}
	}
	// 标出已经有分区维护配置的表（唯一键 uniq_db_tb 按库默认校验规则比较，通常不区分大小写，这里同样不区分）。
	cfgs, _, err := s.repo.ListConfigs(ConfigFilter{InstanceIDs: []uint{inst.ID}, InstanceID: inst.ID})
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for _, cf := range cfgs {
		if strings.EqualFold(cf.DBName, db) {
			taken[strings.ToLower(cf.TbName)] = true
		}
	}
	for i := range out {
		out[i].Configured = taken[strings.ToLower(out[i].Name)]
	}
	return out, nil
}

// Columns 列出表里能作分区字段的列（datetime / date / int / bigint）。
func (s *Service) Columns(ctx context.Context, c Caller, instID uint, db, table string) ([]ColumnInfo, error) {
	db, table = strings.TrimSpace(db), strings.TrimSpace(table)
	if !validIdent(db) || !validIdent(table) || systemDBs[strings.ToLower(db)] {
		return nil, badReq("库名或表名不合法")
	}
	qctx, cat, inst, release, err := s.openCatalog(ctx, c, instID)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := dbAllowed(inst, db); err != nil {
		return nil, err
	}
	all, err := cat.Columns(qctx, db, table)
	if err != nil {
		return nil, targetErr("读取字段列表失败：%v", err)
	}
	out := []ColumnInfo{}
	for _, col := range all {
		ct := partColType(col.DataType)
		if ct == "" || !validIdent(col.Name) {
			continue
		}
		col.ColType = ct
		out = append(out, col)
	}
	return out, nil
}
