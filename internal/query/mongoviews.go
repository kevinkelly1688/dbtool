// @author: kevinkelly
// @license: Apache Licence
// @file: mongoviews.go
// @time: 2026/10/28

package query

import (
	"context"
	"fmt"
	"log/slog"

	"go.mongodb.org/mongo-driver/v2/bson"

	"dbtool/internal/engine"
	mongox "dbtool/internal/engine/mongo"
)

// MongoDB 视图的数据脱敏：视图是“源集合 + 一条聚合管道”（listCollections 的 options.viewOn / options.pipeline），
// 查询视图等价于先对源集合跑视图的管道、再跑查询自己的管道。配置写在源集合的字段路径上时，视图的管道可以把字段改名
// （{p: "$profile.phone"}）、提升嵌套文档，返回的文档里就看不出它来自敏感字段。做法同 SQL 引擎的视图展开（viewexp.go）：
// 读出语句用到的视图（连同它的源视图、管道里 $lookup 等引用的视图，递归），把源集合和管道里引用的集合也算进“语句涉及的集合”去匹配配置，
// 视图的管道接在语句自己的管道前面，交给 newMongoMasker 沿管道追踪改名。
// 失败语义同其它引擎：读不到视图定义按脱敏失败处理（*viewDefError），由调用方按 query.block_on_mask_error 决定阻断还是放行。

type mongoViews interface {
	ViewInfos(ctx context.Context, db string, names []string) ([]mongox.ViewInfo, error)
}

var _ mongoViews = (*mongox.Engine)(nil)

// mongoViewExpansion 是语句用到的视图展开出来的东西。
type mongoViewExpansion struct {
	// collections 是要再拿来匹配配置的集合：视图的源集合，以及视图管道里 $lookup / $graphLookup / $unionWith 的目标。
	collections []string
	// pipeline 是视图的管道，源在前；接在语句自己的管道前面。
	pipeline []bson.D
}

// expandMongoViews 读出 colls（语句涉及的集合）里是视图的那些的定义，并顺着源集合 / 管道引用继续读。
// 返回的展开在出错时也尽量带上已经读到的部分；没有视图、或引擎不提供视图定义时返回 nil。错误是 *viewDefError。
func (s *Service) expandMongoViews(ctx context.Context, e engine.Engine, db string, colls []string) (*mongoViewExpansion, error) {
	mv, ok := e.(mongoViews)
	if !ok || len(colls) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, viewDefsTimeout)
	defer cancel()
	verr := &viewDefError{}
	infos := map[string]*mongox.ViewInfo{} // 视图名 → 定义（不是视图的集合不在里面）
	asked := map[string]bool{}
	frontier := append([]string(nil), colls...)
	total, views := 0, 0
walk:
	for depth := 0; len(frontier) > 0; depth++ {
		var todo []string
		for _, n := range frontier {
			if !asked[n] {
				asked[n] = true
				todo = append(todo, n)
			}
		}
		if len(todo) == 0 {
			break
		}
		if depth >= maxViewDepth {
			verr.Reason = fmt.Sprintf("视图嵌套超过 %d 层", maxViewDepth) // i18n-ignore 只进日志
			break
		}
		if total += len(todo); total > maxViewRefs {
			verr.Reason = fmt.Sprintf("语句和视图定义里引用的集合超过 %d 个", maxViewRefs) // i18n-ignore 只进日志
			break
		}
		found, err := mv.ViewInfos(ctx, db, todo)
		if err != nil {
			slog.Error("读取视图定义失败", "dbType", dbTypeMongo, "db", db, "err", err)
			verr.Reason = "读取视图定义失败：" + err.Error() // i18n-ignore 只进日志
			break
		}
		frontier = nil
		for i := range found {
			v := found[i]
			infos[v.Name] = &v
			if v.Err != "" {
				slog.Warn("视图定义读不到或看不懂，数据脱敏无法展开它", "view", v.Name, "err", v.Err)
				verr.Views = append(verr.Views, v.Name)
				continue
			}
			if views++; views > maxViewCount {
				verr.Reason = fmt.Sprintf("语句涉及的视图超过 %d 个", maxViewCount) // i18n-ignore 只进日志
				break walk
			}
			frontier = append(frontier, v.ViewOn)
			frontier = append(frontier, mongox.PipelineCollections(v.Pipeline)...)
		}
	}
	x := &mongoViewExpansion{}
	done := map[string]bool{}
	var build func(name string)
	build = func(name string) {
		v := infos[name]
		if v == nil || v.Err != "" || done[name] { // done 防环（MongoDB 不允许视图成环，这里不依赖它）
			return
		}
		done[name] = true
		build(v.ViewOn) // 源视图的管道先执行
		lookups := mongox.PipelineCollections(v.Pipeline)
		for _, lc := range lookups {
			build(lc)
		}
		x.pipeline = append(x.pipeline, v.Pipeline...)
		x.collections = append(x.collections, v.ViewOn)
		x.collections = append(x.collections, lookups...)
	}
	for _, c := range colls {
		build(c)
	}
	var out *mongoViewExpansion
	if len(x.pipeline) > 0 || len(x.collections) > 0 {
		out = x
	}
	if len(verr.Views) > 0 || verr.Reason != "" {
		return out, verr
	}
	return out, nil
}
