// @author: kevinkelly
// @license: Apache Licence
// @file: views.go
// @time: 2026/10/28

package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"dbtool/internal/engine"
)

var _ engine.ViewDefiner = (*Engine)(nil)

// ViewDefs 读视图定义（sys.views ⨝ sys.sql_modules.definition，完整的 CREATE VIEW … AS …）。
// Catalog 为空的名字在 db 里找，写了三段名的到它的库里找（每个库查一次）；Schema 为空的名字在所有架构里按名字找（多找出来的同名视图只会多展开）。
// 没有 VIEW DEFINITION 权限或视图加密时 definition 为 NULL，原样返回（Definition 为空），由调用方按“读不到”处理。
// sys / INFORMATION_SCHEMA 架构里的视图不返回。
func (e *Engine) ViewDefs(ctx context.Context, db string, refs []engine.ViewRef) ([]engine.ViewDef, error) {
	byCatalog := map[string][]engine.ViewRef{}
	var order []string
	for _, r := range refs {
		if r.Name == "" {
			continue
		}
		cat := r.Catalog
		if cat == "" {
			cat = db
		}
		if _, ok := byCatalog[cat]; !ok {
			order = append(order, cat)
		}
		byCatalog[cat] = append(byCatalog[cat], r)
	}
	var out []engine.ViewDef
	for _, cat := range order {
		q, args := viewDefsQuery(cat, byCatalog[cat])
		rows, err := e.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, errors.New(errText(err))
		}
		for rows.Next() {
			var schema, name string
			var def sql.NullString
			if err := rows.Scan(&schema, &name, &def); err != nil {
				rows.Close()
				return nil, err
			}
			if len(def.String) > engine.MaxViewDefBytes {
				def.String = "" // 过大的定义当作读不到
			}
			vd := engine.ViewDef{Schema: schema, Name: name, Definition: def.String}
			if cat != db {
				vd.Catalog = cat // 视图在别的库里：它定义里没写库名的名字属于那个库
			}
			out = append(out, vd)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, errors.New(errText(err))
		}
	}
	return out, nil
}

// viewDefsQuery 拼一个库里的目录查询。cat 为空时用连接的默认库。
func viewDefsQuery(cat string, refs []engine.ViewRef) (string, []any) {
	prefix := ""
	if cat != "" {
		prefix = QuoteIdent(cat) + "."
	}
	var conds []string
	var args []any
	for _, r := range refs {
		n := len(args)
		if r.Schema != "" {
			conds = append(conds, fmt.Sprintf("(s.name = @p%d AND v.name = @p%d)", n+1, n+2))
			args = append(args, r.Schema, r.Name)
		} else {
			conds = append(conds, fmt.Sprintf("v.name = @p%d", n+1))
			args = append(args, r.Name)
		}
	}
	return "SELECT s.name, v.name, m.definition FROM " + prefix + "sys.views v JOIN " + prefix + "sys.schemas s ON s.schema_id = v.schema_id " +
		"LEFT JOIN " + prefix + "sys.sql_modules m ON m.object_id = v.object_id " +
		"WHERE s.name NOT IN ('sys', 'INFORMATION_SCHEMA') AND (" + strings.Join(conds, " OR ") + ")", args
}
