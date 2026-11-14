// @author: kevinkelly
// @license: Apache Licence
// @file: views.go
// @time: 2026/10/28

package postgres

import (
	"context"
	"strings"

	"dbtool/internal/engine"
)

var _ engine.ViewDefiner = (*Engine)(nil)

// ViewDefs 读视图 / 物化视图的定义（pg_get_viewdef）。按名字在所有用户 schema 里找，再按 refs 里写的 Schema 过滤：
// Schema 为空的名字不限 schema（搜索路径不好在这里还原，多找出来的同名视图只会多展开）；
// 系统 schema（information_schema、pg_*）里的视图不返回。在只读事务里查询。
func (e *Engine) ViewDefs(ctx context.Context, db string, refs []engine.ViewRef) ([]engine.ViewDef, error) {
	want := map[string][]string{} // 视图名 → 要找的 schema（空串 = 不限）
	var names []string
	for _, r := range refs {
		if r.Name == "" {
			continue
		}
		if _, ok := want[r.Name]; !ok {
			names = append(names, r.Name)
		}
		want[r.Name] = append(want[r.Name], r.Schema)
	}
	if len(names) == 0 {
		return nil, nil
	}
	var out []engine.ViewDef
	err := e.readTx(ctx, e.dbOrDefault(db), metaTimeout, nil, func(s *session) error {
		rows, err := s.rows(ctx, `SELECT n.nspname, c.relname, pg_catalog.pg_get_viewdef(c.oid, true)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('v', 'm') AND n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
  AND c.relname = ANY($1::text[])
ORDER BY n.nspname, c.relname`, textArray(names))
		if err != nil {
			return err
		}
		out = make([]engine.ViewDef, 0, len(rows))
		for _, r := range rows {
			if len(r) < 3 || r[0] == nil || r[1] == nil || !wantSchema(want[*r[1]], *r[0]) {
				continue
			}
			def := str(r[2])
			if len(def) > engine.MaxViewDefBytes {
				def = "" // 过大的定义当作读不到
			}
			out = append(out, engine.ViewDef{Schema: *r[0], Name: *r[1], Definition: def})
		}
		return nil
	})
	return out, err
}

func wantSchema(schemas []string, schema string) bool {
	for _, s := range schemas {
		if s == "" || s == schema {
			return true
		}
	}
	return false
}

// textArray 把字符串列表写成 PostgreSQL 的数组字面量 {"a","b"}（作为文本参数传给 $n::text[]）。
func textArray(ss []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, s := range ss {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}
