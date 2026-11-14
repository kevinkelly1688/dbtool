// @author: kevinkelly
// @license: Apache Licence
// @file: views.go
// @time: 2026/10/28

package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"dbtool/internal/engine"
)

var _ engine.ViewDefiner = (*Engine)(nil)

// viewEngines 是 system.tables 里带 as_select 的视图类引擎（普通视图、物化视图、实时视图、窗口视图）。
const viewEngines = "('View', 'MaterializedView', 'LiveView', 'WindowView')"

// ViewDefs 读视图的定义（system.tables.as_select：视图的 SELECT，物化视图是往目标表写数据的那条 SELECT）。
// 名字区分大小写；Schema 为空的按 db（再没有就用实例的默认库）。账号看不到的视图 system.tables 里没有，按“不是视图”处理。
// as_select 为空（没有权限看定义）原样返回（Definition 为空），由调用方按“读不到”处理。
// 服务端太旧、没有 as_select 列，或返回的列不对时返回 engine.ErrViewDefsUnsupported。
func (e *Engine) ViewDefs(ctx context.Context, db string, refs []engine.ViewRef) ([]engine.ViewDef, error) {
	if db == "" {
		db = e.cfg.Database
	}
	q := viewDefsQuery(db, refs)
	if q == "" {
		return nil, nil
	}
	lim := metaLimits
	lim.MaxRows = 1000
	lim.MaxBytes = HardMaxBytes
	lim.MaxCellBytes = engine.MaxViewDefBytes // 定义最长允许 1 MiB；更长的单元格会被截断，下面按“读不到”处理
	lim.Timeout = 20 * time.Second
	sh, err := e.run(ctx, q, reqOpts{QueryLimits: lim.Normalize()})
	if err != nil {
		var se *ServerError
		if errors.As(err, &se) && (se.Code == 47 || se.Code == 16) { // 没有 as_select 这一列（老版本）：未知标识符 / 表里没有这一列
			return nil, fmt.Errorf("%w: %v", engine.ErrViewDefsUnsupported, err)
		}
		return nil, err
	}
	if sh.Truncated {
		return nil, errors.New("视图定义的查询结果过大，已中止") // i18n-ignore 只进日志
	}
	rs := &rowset{cols: sh.ColumnList, rows: sh.Rows}
	if rs.idx("database") < 0 || rs.idx("name") < 0 || rs.idx("as_select") < 0 {
		return nil, fmt.Errorf("%w: 返回的列不是 database, name, as_select", engine.ErrViewDefsUnsupported) // i18n-ignore 只进日志
	}
	out := make([]engine.ViewDef, 0, len(rs.rows))
	for _, r := range rs.rows {
		def := rs.str(r, "as_select")
		if len(def) > engine.MaxViewDefBytes {
			def = "" // 过大（被截断）的定义当作读不到
		}
		out = append(out, engine.ViewDef{Schema: rs.str(r, "database"), Name: rs.str(r, "name"), Definition: def})
	}
	return out, nil
}

// viewDefsQuery 拼目录查询：每个名字一组 (库, 视图名) 条件，字符串字面量转义。没有可查的名字返回空串。
func viewDefsQuery(db string, refs []engine.ViewRef) string {
	var conds []string
	seen := map[[2]string]bool{}
	for _, r := range refs {
		schema := r.Schema
		if schema == "" {
			schema = db
		}
		if schema == "" || r.Name == "" || checkName("库", schema) != nil || checkName("视图", r.Name) != nil {
			continue
		}
		k := [2]string{schema, r.Name}
		if seen[k] {
			continue
		}
		seen[k] = true
		conds = append(conds, "(database = "+sqlString(schema)+" AND name = "+sqlString(r.Name)+")")
	}
	if len(conds) == 0 {
		return ""
	}
	return "SELECT database, name, engine, as_select FROM system.tables WHERE engine IN " + viewEngines +
		" AND database NOT IN " + hiddenDatabases + " AND (" + strings.Join(conds, " OR ") + ")"
}
