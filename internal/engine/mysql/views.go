// @author: kevinkelly
// @license: Apache Licence
// @file: views.go
// @time: 2026/10/28

package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	drv "github.com/go-sql-driver/mysql"

	"dbtool/internal/engine"
)

var _ engine.ViewDefiner = (*Engine)(nil)

// 系统库里的视图（sys 库有大量复杂视图）与脱敏无关，不查。
const viewDefsSystemSchemas = "('information_schema', 'performance_schema', 'mysql', 'sys')"

// ViewDefs 读视图定义（information_schema.VIEWS.VIEW_DEFINITION）。MySQL 存的是规范化后的 SELECT：列都带 AS 别名、对象名带库名。
// 账号没有 SHOW VIEW 权限时 VIEW_DEFINITION 为空，原样返回（Definition 为空），由调用方按“读不到”处理。
// SelectDB / Doris / PolarDB / ADB 走同一条查询；目录表不存在或列不兼容返回 engine.ErrViewDefsUnsupported。
func (e *Engine) ViewDefs(ctx context.Context, db string, refs []engine.ViewRef) ([]engine.ViewDef, error) {
	q, args := viewDefsQuery(db, refs)
	if q == "" {
		return nil, nil
	}
	rows, err := e.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapViewDefsErr(err)
	}
	defer rows.Close()
	var out []engine.ViewDef
	for rows.Next() {
		var schema, name string
		var def sql.NullString
		if err := rows.Scan(&schema, &name, &def); err != nil {
			return nil, err
		}
		if len(def.String) > engine.MaxViewDefBytes {
			def.String = "" // 过大的定义当作读不到
		}
		out = append(out, engine.ViewDef{Schema: schema, Name: name, Definition: def.String})
	}
	if err := rows.Err(); err != nil {
		return nil, mapViewDefsErr(err)
	}
	return out, nil
}

// viewDefsQuery 拼目录查询：每个名字一组 (库, 视图名) 条件。没写库名的按当前库；当前库也没有的名字 MySQL 自己也解析不了，跳过。
// 服务端在 lower_case_table_names=1 时库表名按小写存放，所以名字与它的小写形式都带上（多查到的同名异大小写对象只会多展开，不会漏）。
func viewDefsQuery(db string, refs []engine.ViewRef) (string, []any) {
	var conds []string
	var args []any
	seen := map[[2]string]bool{}
	add := func(schema, name string) {
		k := [2]string{schema, name}
		if seen[k] {
			return
		}
		seen[k] = true
		conds = append(conds, "(TABLE_SCHEMA = ? AND TABLE_NAME = ?)")
		args = append(args, schema, name)
	}
	for _, r := range refs {
		schema := r.Schema
		if schema == "" {
			schema = db
		}
		if schema == "" || r.Name == "" {
			continue
		}
		add(schema, r.Name)
		add(strings.ToLower(schema), strings.ToLower(r.Name))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return "SELECT TABLE_SCHEMA, TABLE_NAME, VIEW_DEFINITION FROM information_schema.VIEWS WHERE (" +
		strings.Join(conds, " OR ") + ") AND TABLE_SCHEMA NOT IN " + viewDefsSystemSchemas, args
}

// mapViewDefsErr 把“目录表 / 列不存在”之类（兼容协议的库没有 information_schema.VIEWS）映射成 ErrViewDefsUnsupported；
// 权限不足、超时、连接错误等照常返回，不能当作“没有视图”。
func mapViewDefsErr(err error) error {
	var me *drv.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1146, 1109, 1054: // ER_NO_SUCH_TABLE、ER_UNKNOWN_TABLE、ER_BAD_FIELD_ERROR
			return fmt.Errorf("%w: %v", engine.ErrViewDefsUnsupported, err)
		}
		msg := strings.ToLower(me.Message)
		if strings.Contains(msg, "unknown table") || strings.Contains(msg, "doesn't exist") || strings.Contains(msg, "does not exist") {
			return fmt.Errorf("%w: %v", engine.ErrViewDefsUnsupported, err)
		}
	}
	return err
}
