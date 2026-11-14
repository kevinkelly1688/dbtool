// @author: kevinkelly
// @license: Apache Licence
// @file: mongomask.go
// @time: 2026/10/28

package query

import (
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	mongox "dbtool/internal/engine/mongo"
	"dbtool/internal/masking"
)

// MongoDB 的数据脱敏：在结果文档整形成表格之前，按字段路径就地处理。
//
//   - 配置里的 field_path 是点号路径（profile.phone），contacts[].mobile 表示数组元素里的 mobile；文档里穿过数组时不计下标；
//   - 路径按“后缀”匹配任意深度：profile.phone 既匹配根上的 profile.phone，也匹配 $lookup 之后嵌在别的字段里的 xxx.profile.phone
//     （多脱敏不会少脱敏）；字段名不区分大小写；
//   - 配置只取与本次语句有关的：库相同（或 *），集合是语句涉及的集合（含 $lookup / $graphLookup / $unionWith 的目标）或 *；
//   - 聚合管道里把敏感字段改名输出（{p: "$profile.phone"}、$group 的 _id、$replaceRoot 提升嵌套文档）时，沿着管道把新名字也当作敏感路径；
//   - 命中的值若是嵌套文档 / 数组，对其中每个叶子值逐个处理。

type mongoEntry struct {
	segs []string // 小写路径段
	rule masking.Rule
}

type mongoMasker struct {
	entries []mongoEntry
	masked  map[string]bool // 出现过脱敏的顶层字段（给“已脱敏列”展示）
}

var _ mongox.Masker = (*mongoMasker)(nil)

// splitPath 把 profile.address[].detail 拆成小写路径段（去掉数组标记）。
func splitPath(p string) []string {
	p = strings.ReplaceAll(p, "[]", "")
	var out []string
	for _, s := range strings.Split(p, ".") {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// newMongoMasker 返回 nil 表示这条语句不需要脱敏（没有适用的配置）。
func newMongoMasker(cols []masking.Column, rules map[int]masking.Rule, db string, collections []string, pipeline []bson.D) *mongoMasker {
	m := &mongoMasker{masked: map[string]bool{}}
	for _, c := range cols {
		if c.Schema != "*" && !strings.EqualFold(c.Schema, db) {
			continue
		}
		if c.Table != "*" {
			ok := false
			for _, coll := range collections {
				if strings.EqualFold(c.Table, coll) {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		r, ok := c.ResolveColumn(rules)
		if !ok {
			continue
		}
		if segs := splitPath(c.Column); len(segs) > 0 {
			m.entries = append(m.entries, mongoEntry{segs: segs, rule: r})
		}
	}
	if len(m.entries) == 0 {
		return nil
	}
	m.propagate(pipeline)
	return m
}

// collectRefs 收集表达式里引用的字段路径（"$a.b" → [a b]；"$$ROOT.a" → [a]；"$$ROOT" → []）。
func collectRefs(v any, out *[][]string) {
	switch x := v.(type) {
	case string:
		switch {
		case strings.HasPrefix(x, "$$ROOT"), strings.HasPrefix(x, "$$CURRENT"):
			rest := x[len("$$ROOT"):]
			if strings.HasPrefix(x, "$$CURRENT") {
				rest = x[len("$$CURRENT"):]
			}
			*out = append(*out, splitPath(strings.TrimPrefix(rest, ".")))
		case strings.HasPrefix(x, "$$"):
		case strings.HasPrefix(x, "$"):
			*out = append(*out, splitPath(x[1:]))
		}
	case bson.D:
		for _, e := range x {
			collectRefs(e.Value, out)
		}
	case bson.A:
		for _, e := range x {
			collectRefs(e, out)
		}
	case []any:
		for _, e := range x {
			collectRefs(e, out)
		}
	case bson.M:
		for _, e := range x {
			collectRefs(e, out)
		}
	case map[string]any:
		for _, e := range x {
			collectRefs(e, out)
		}
	}
}

func hasPrefix(segs, prefix []string) bool {
	if len(prefix) > len(segs) {
		return false
	}
	for i := range prefix {
		if segs[i] != prefix[i] {
			return false
		}
	}
	return true
}

const maxMongoEntries = 2000

// propagate 沿聚合管道追踪改名：某个输出字段的表达式引用了敏感路径（或其上层路径），把输出字段下对应的路径也加入配置。
func (m *mongoMasker) propagate(pipeline []bson.D) {
	derive := func(outKey []string, refs [][]string) {
		for _, ref := range refs {
			for _, ent := range append([]mongoEntry(nil), m.entries...) {
				if len(m.entries) >= maxMongoEntries {
					return
				}
				if !hasPrefix(ent.segs, ref) {
					continue
				}
				segs := append(append([]string(nil), outKey...), ent.segs[len(ref):]...)
				if len(segs) > 0 {
					m.entries = append(m.entries, mongoEntry{segs: segs, rule: ent.rule})
				}
			}
		}
	}
	for _, stage := range pipeline {
		for _, op := range stage {
			switch op.Key {
			case "$project", "$addFields", "$set", "$group":
				doc, ok := op.Value.(bson.D)
				if !ok {
					continue
				}
				for _, kv := range doc {
					var refs [][]string
					collectRefs(kv.Value, &refs)
					derive(splitPath(kv.Key), refs)
				}
			case "$replaceRoot":
				if doc, ok := op.Value.(bson.D); ok {
					for _, kv := range doc {
						if kv.Key == "newRoot" {
							var refs [][]string
							collectRefs(kv.Value, &refs)
							derive(nil, refs)
						}
					}
				}
			case "$replaceWith":
				var refs [][]string
				collectRefs(op.Value, &refs)
				derive(nil, refs)
			}
		}
	}
}

// match 返回路径命中的最严规则。
func (m *mongoMasker) match(path []string) (masking.Rule, bool) {
	var best masking.Rule
	found := false
	for _, ent := range m.entries {
		n := len(ent.segs)
		if n > len(path) {
			continue
		}
		ok := true
		for i := 0; i < n; i++ {
			if path[len(path)-n+i] != ent.segs[i] {
				ok = false
				break
			}
		}
		if ok && (!found || ent.rule.Strictness() > best.Strictness()) {
			best, found = ent.rule, true
		}
	}
	return best, found
}

// maskTree 对命中路径下的整棵值逐个叶子脱敏。规则出错时宁可整体隐藏，也不放行原值。
func maskTree(v any, r masking.Rule) any {
	switch x := v.(type) {
	case bson.D:
		out := make(bson.D, len(x))
		for i, e := range x {
			out[i] = bson.E{Key: e.Key, Value: maskTree(e.Value, r)}
		}
		return out
	case bson.A:
		out := make(bson.A, len(x))
		for i, e := range x {
			out[i] = maskTree(e, r)
		}
		return out
	case bson.M:
		out := make(bson.M, len(x))
		for k, e := range x {
			out[k] = maskTree(e, r)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = maskTree(e, r)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = maskTree(e, r)
		}
		return out
	}
	got, err := masking.Apply(v, r)
	if err != nil {
		return masking.Mask
	}
	return got
}

func (m *mongoMasker) walk(v any, path []string, top string) any {
	if r, ok := m.match(path); ok {
		m.masked[top] = true
		return maskTree(v, r)
	}
	child := func(key string) []string { return append(append([]string(nil), path...), strings.ToLower(key)) }
	switch x := v.(type) {
	case bson.D:
		out := make(bson.D, len(x))
		for i, e := range x {
			out[i] = bson.E{Key: e.Key, Value: m.walk(e.Value, child(e.Key), top)}
		}
		return out
	case bson.M:
		out := make(bson.M, len(x))
		for k, e := range x {
			out[k] = m.walk(e, child(k), top)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = m.walk(e, child(k), top)
		}
		return out
	case bson.A:
		out := make(bson.A, len(x))
		for i, e := range x {
			out[i] = m.walk(e, path, top)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = m.walk(e, path, top)
		}
		return out
	}
	return v
}

// Doc 对一个结果文档脱敏。
func (m *mongoMasker) Doc(d bson.D) bson.D {
	out := make(bson.D, len(d))
	for i, e := range d {
		out[i] = bson.E{Key: e.Key, Value: m.walk(e.Value, []string{strings.ToLower(e.Key)}, e.Key)}
	}
	return out
}

// Distinct 对 distinct(field) 的一个取值脱敏。
func (m *mongoMasker) Distinct(field string, v any) any {
	return m.walk(v, splitPath(field), field)
}

// Columns 返回出现过脱敏的顶层字段名（排序）。
func (m *mongoMasker) Columns() []string {
	out := make([]string, 0, len(m.masked))
	for k := range m.masked {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
