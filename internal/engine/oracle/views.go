// @author: kevinkelly
// @license: Apache Licence
// @file: views.go
// @time: 2026/10/28

package oracle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"dbtool/internal/engine"
)

var _ engine.ViewDefiner = (*Engine)(nil)

// viewDefsChunk 是一条目录查询里最多带的视图个数（OR 条件不宜太长）。
const viewDefsChunk = 50

// ViewDefs 读视图定义（ALL_VIEWS.TEXT，LONG 类型）和视图的实际列名（ALL_TAB_COLUMNS，按列序）。
// 没写 schema 的名字按 db（当前 schema）解析；系统 schema 里的视图不查；refs 里不是视图的对象不返回。
// 数据脱敏据此顺着视图追到基表字段：视图体里的别名、表达式、嵌套视图，以及 CREATE VIEW v (列, …) 里重命名过的列。
// 账号看不到 ALL_VIEWS 时返回 engine.ErrViewDefsUnsupported。
func (e *Engine) ViewDefs(ctx context.Context, db string, refs []engine.ViewRef) ([]engine.ViewDef, error) {
	type key struct{ owner, name string }
	var want []key
	seen := map[key]bool{}
	for _, r := range refs {
		owner := r.Schema
		if owner == "" {
			owner = db
		}
		if owner == "" || r.Name == "" || isSystemSchema(owner) || validObjectName(owner) != nil || validObjectName(r.Name) != nil {
			continue
		}
		if k := (key{owner, r.Name}); !seen[k] {
			seen[k] = true
			want = append(want, k)
		}
	}
	var out []engine.ViewDef
	for start := 0; start < len(want); start += viewDefsChunk {
		part := want[start:min(start+viewDefsChunk, len(want))]
		conds := make([]string, len(part))
		for i, k := range part {
			conds[i] = "(owner = " + lit(k.owner) + " AND view_name = " + lit(k.name) + ")"
		}
		rows, err := e.queryAll(ctx, "SELECT owner, view_name, text FROM all_views WHERE "+strings.Join(conds, " OR "))
		if err != nil {
			return nil, viewDefsErr(err)
		}
		if len(rows) == 0 {
			continue
		}
		found := make([]engine.ViewDef, 0, len(rows))
		cond2 := make([]string, 0, len(rows))
		for _, r := range rows {
			if len(r) < 3 {
				continue
			}
			def := asString(r[2])
			if len(def) > engine.MaxViewDefBytes {
				def = "" // 过大的定义当作读不到
			}
			vd := engine.ViewDef{Schema: asString(r[0]), Name: asString(r[1]), Definition: def}
			found = append(found, vd)
			cond2 = append(cond2, "(owner = "+lit(vd.Schema)+" AND table_name = "+lit(vd.Name)+")")
		}
		cols := map[[2]string][]string{}
		if crows, err := e.queryAll(ctx, "SELECT owner, table_name, column_name FROM all_tab_columns WHERE "+strings.Join(cond2, " OR ")+
			" ORDER BY owner, table_name, column_id"); err == nil { // 读不到列名不影响主流程：只是少了按位置重命名的那一层
			for _, r := range crows {
				if len(r) >= 3 {
					k := [2]string{asString(r[0]), asString(r[1])}
					cols[k] = append(cols[k], asString(r[2]))
				}
			}
		}
		for i := range found {
			found[i].ColumnNames = cols[[2]string{found[i].Schema, found[i].Name}]
		}
		out = append(out, found...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Schema != out[j].Schema {
			return out[i].Schema < out[j].Schema
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// viewDefsErr：目录视图不存在（ORA-00942）按“没有视图定义目录”处理，其它错误原样返回。
func viewDefsErr(err error) error {
	var se *ServerError
	if errors.As(err, &se) && se.Code == 942 {
		return fmt.Errorf("%w: %v", engine.ErrViewDefsUnsupported, err)
	}
	return err
}
