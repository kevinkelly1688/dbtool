// @author: kevinkelly
// @license: Apache Licence
// @file: tree.go
// @time: 2026/10/28

package goinception

import (
	"sort"

	"dbtool/internal/sqlx"
)

// TableRefs 从语法树里取出引用的表（广度优先查找）：
// 先逐层找“最大的” TableRefs/Left/Right 子树；某一层再也找不到时，该层的 Source 节点就是表引用。
//
// Source 不是表（没有 Name，例如派生表的子查询）时跳过，由递归处理其内部的表；
// 结果去重并按库名、表名排序，保证输出稳定。
func TableRefs(root any, defaultDB string) []sqlx.TableRef {
	var out []sqlx.TableRef
	seen := map[sqlx.TableRef]bool{}
	queue := []any{root}
	for i := 0; i < len(queue); i++ {
		tree, ok := queue[i].(map[string]any)
		if !ok {
			continue
		}
		if nodes := findMax(tree, "TableRefs", "Left", "Right"); len(nodes) > 0 {
			for _, n := range nodes {
				if truthy(n.value) {
					queue = append(queue, n.value)
				}
			}
			continue
		}
		for _, n := range findMax(tree, "Source") {
			src, ok := n.value.(map[string]any)
			if !ok {
				continue
			}
			name := oField(src["Name"])
			if name == "" {
				continue
			}
			schema := oField(src["Schema"])
			if schema == "" {
				schema = defaultDB
			}
			ref := sqlx.TableRef{Schema: schema, Name: name}
			if !seen[ref] {
				seen[ref] = true
				out = append(out, ref)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Schema != out[j].Schema {
			return out[i].Schema < out[j].Schema
		}
		return out[i].Name < out[j].Name
	})
	return out
}

type node struct {
	key   string
	value any
}

// findMax 广度优先查找满足条件的最大子树：命中指定 key 的节点不再向下展开。
func findMax(root map[string]any, keys ...string) []node {
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	var fit []node
	queue := []map[string]any{root}
	for i := 0; i < len(queue); i++ {
		for k, v := range queue[i] {
			if want[k] {
				fit = append(fit, node{k, v})
				continue
			}
			switch x := v.(type) {
			case map[string]any:
				queue = append(queue, x)
			case []any:
				for _, e := range x {
					if m, ok := e.(map[string]any); ok {
						queue = append(queue, m)
					}
				}
			}
		}
	}
	return fit
}

// oField 读取 TiDB 标识符对象 {"O": 原文, "L": 小写} 里的 O。
func oField(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["O"].(string)
	return s
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case map[string]any:
		return len(x) > 0
	case []any:
		return len(x) > 0
	case string:
		return x != ""
	case bool:
		return x
	case float64:
		return x != 0
	}
	return true
}
