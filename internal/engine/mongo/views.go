// @author: kevinkelly
// @license: Apache Licence
// @file: views.go
// @time: 2026/10/28

package mongo

import (
	"context"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// 视图的定义（数据脱敏用）：MongoDB 的视图是“源集合 + 一条聚合管道”，listCollections 的 options 里带着 viewOn 与 pipeline。
// 对视图的查询等价于对源集合先跑视图的管道、再跑查询自己的管道，所以脱敏时把视图的管道接在语句的管道前面追踪改名（见 internal/query/mongoviews.go）。

// ViewInfo 是一个视图的定义。
type ViewInfo struct {
	Name     string
	ViewOn   string   // 源集合（也可能是另一个视图）
	Pipeline []bson.D // 视图的聚合管道，可以为空
	// Err 非空表示视图存在但定义读不到或看不懂（options 里没有 viewOn，或 pipeline 不是文档数组）。
	Err string
}

// nameLister 由能按名字过滤 listCollections 的库句柄实现（真实驱动）；没有实现的（测试替身）就列出全部再过滤。
type nameLister interface {
	ListCollectionsByName(ctx context.Context, names []string) ([]CollectionInfo, error)
}

// ViewInfos 返回 names 里是视图的集合的定义（listCollections 的 viewOn 与 pipeline）；不是视图 / 不存在的不返回。
func (e *Engine) ViewInfos(ctx context.Context, db string, names []string) ([]ViewInfo, error) {
	if err := ValidDBName(db); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, n := range names {
		if validCollName(n) == nil {
			want[n] = true
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	d := e.be.Database(db)
	var infos []CollectionInfo
	var err error
	if nl, ok := d.(nameLister); ok {
		list := make([]string, 0, len(want))
		for n := range want {
			list = append(list, n)
		}
		sort.Strings(list)
		infos, err = nl.ListCollectionsByName(ctx, list)
	} else {
		infos, err = d.ListCollections(ctx)
	}
	if err != nil {
		return nil, e.friendlyError(ctx, err)
	}
	var out []ViewInfo
	for _, ci := range infos {
		if ci.Type != "view" || !want[ci.Name] {
			continue
		}
		vi := ViewInfo{Name: ci.Name}
		on, _ := lookup(ci.Options, "viewOn")
		s, ok := on.(string)
		if !ok || s == "" {
			vi.Err = "options 里没有 viewOn" // i18n-ignore 只进日志
			out = append(out, vi)
			continue
		}
		vi.ViewOn = s
		if pv, ok := lookup(ci.Options, "pipeline"); ok {
			p, perr := pipelineOf(pv)
			if perr != "" {
				vi.Err = perr
			}
			vi.Pipeline = p
		}
		out = append(out, vi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// pipelineOf 把 options.pipeline（bson.A 的文档数组）转成 []bson.D。认不出的元素返回说明。
func pipelineOf(v any) ([]bson.D, string) {
	var items []any
	switch x := v.(type) {
	case bson.A:
		items = x
	case []any:
		items = x
	case []bson.D:
		return x, ""
	case nil:
		return nil, ""
	default:
		return nil, "pipeline 不是数组" // i18n-ignore 只进日志
	}
	out := make([]bson.D, 0, len(items))
	for _, it := range items {
		st, ok := toD(it).(bson.D)
		if !ok {
			return out, "pipeline 里有不是文档的阶段" // i18n-ignore 只进日志
		}
		out = append(out, st)
	}
	return out, ""
}

// toD 把嵌套的文档（bson.M / map）统一成有序的 bson.D：脱敏沿管道追踪改名时只认 bson.D。
func toD(v any) any {
	switch x := v.(type) {
	case bson.D:
		out := make(bson.D, len(x))
		for i, e := range x {
			out[i] = bson.E{Key: e.Key, Value: toD(e.Value)}
		}
		return out
	case bson.M:
		return mapToD(x)
	case map[string]any:
		return mapToD(x)
	case bson.A:
		out := make(bson.A, len(x))
		for i, e := range x {
			out[i] = toD(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = toD(e)
		}
		return out
	}
	return v
}

func mapToD(m map[string]any) bson.D {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(bson.D, 0, len(keys))
	for _, k := range keys {
		out = append(out, bson.E{Key: k, Value: toD(m[k])})
	}
	return out
}

// PipelineCollections 返回聚合管道（含 $facet / $lookup / $unionWith 的子管道）引用的其它集合：$lookup / $graphLookup 的 from，
// $unionWith 的集合名。宽松地遍历，不做语句检查那一套校验（视图的管道是库里已有的定义，不是用户输入）。去重、排序。
func PipelineCollections(pipeline []bson.D) []string {
	seen := map[string]bool{}
	collectPipelineCollections(pipeline, seen, 0)
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func collectPipelineCollections(p []bson.D, seen map[string]bool, depth int) {
	if depth > 8 {
		return
	}
	sub := func(v any) {
		if sp, _ := pipelineOf(v); len(sp) > 0 {
			collectPipelineCollections(sp, seen, depth+1)
		}
	}
	for _, stage := range p {
		for _, op := range stage {
			switch op.Key {
			case "$lookup", "$graphLookup":
				if d, ok := op.Value.(bson.D); ok {
					for _, kv := range d {
						switch kv.Key {
						case "from":
							if s, ok := kv.Value.(string); ok && s != "" {
								seen[s] = true
							}
						case "pipeline":
							sub(kv.Value)
						}
					}
				}
			case "$unionWith":
				switch u := op.Value.(type) {
				case string:
					if u != "" {
						seen[u] = true
					}
				case bson.D:
					for _, kv := range u {
						switch kv.Key {
						case "coll":
							if s, ok := kv.Value.(string); ok && s != "" {
								seen[s] = true
							}
						case "pipeline":
							sub(kv.Value)
						}
					}
				}
			case "$facet":
				if d, ok := op.Value.(bson.D); ok {
					for _, kv := range d {
						sub(kv.Value)
					}
				}
			}
		}
	}
}
