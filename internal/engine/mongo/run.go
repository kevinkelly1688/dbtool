// @author: kevinkelly
// @license: Apache Licence
// @file: run.go
// @time: 2026/10/28

package mongo

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"dbtool/internal/engine"
)

// QueryLimits 是 RunUser 的限量。并发由上层控制。
type QueryLimits struct {
	MaxRows      int           // 结果行数上限（<=0 取 DefaultMaxRows）
	MaxBytes     int           // 原始 BSON 总量上限（<=0 或超过 HardMaxBytes 取 HardMaxBytes）
	MaxCellBytes int           // 单个单元格文本上限（<=0 取 DefaultMaxCellBytes）
	Timeout      time.Duration // 执行时间上限（<=0 取 30 秒），同时作为服务端 maxTimeMS
	// Mask 非空时，结果文档在整形成表格之前先经它脱敏（嵌套字段、数组元素都在原文档上处理，整形之后就无从区分了）。
	Mask Masker
}

// Masker 在结果整形之前对返回的数据脱敏（数据脱敏，实现见 internal/query/mongomask.go）。
type Masker interface {
	// Doc 对一个结果文档脱敏，返回处理后的文档（可以就地修改）。
	Doc(d bson.D) bson.D
	// Distinct 对 distinct(field) 的一个取值脱敏。
	Distinct(field string, v any) any
}

// maskDocs 就地对一批文档脱敏。
func (l QueryLimits) maskDocs(docs []bson.D) {
	if l.Mask == nil {
		return
	}
	for i := range docs {
		docs[i] = l.Mask.Doc(docs[i])
	}
}

// Shaped 是整形后的结果。
type Shaped struct {
	Kind      Kind
	Statement string // 规范化后的语句
	Columns   []string
	Rows      [][]any
	Truncated bool // 行数 / 字节上限导致结果不完整
	ElapsedMs int64
	Warnings  []string // 静态改写提示 + 运行期提示（截断 / 二进制 / 单元格截断）
}

func (l QueryLimits) norm() QueryLimits {
	if l.MaxRows <= 0 {
		l.MaxRows = DefaultMaxRows
	}
	if l.MaxBytes <= 0 || l.MaxBytes > HardMaxBytes {
		l.MaxBytes = HardMaxBytes
	}
	if l.MaxCellBytes <= 0 {
		l.MaxCellBytes = DefaultMaxCellBytes
	}
	if l.Timeout <= 0 {
		l.Timeout = 30 * time.Second
	}
	return l
}

// RunUser 在库 db 里执行已通过 CheckUserQuery 的语句并整形结果。
// 只读由三层保证：语句来自白名单子集；Backend 只暴露固定的读操作；每个请求都带 maxTimeMS，结果行数 / 字节有上限。
func (e *Engine) RunUser(ctx context.Context, db string, c *Checked, lim QueryLimits) (*Shaped, error) {
	if c == nil {
		return nil, uerr("语句未经过检查")
	}
	if err := ValidDBName(db); err != nil {
		return nil, err
	}
	lim = lim.norm()
	qctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()
	start := time.Now()

	out := &Shaped{Kind: c.Kind, Statement: c.Statement, Warnings: append([]string(nil), c.Notices...)}
	sh := newShaper(lim.MaxCellBytes)
	database := e.be.Database(db)
	var err error
	switch c.Kind {
	case KindFind:
		err = e.runFind(qctx, database, c, lim, sh, out)
	case KindAggregate:
		err = e.runAggregate(qctx, database, c, lim, sh, out)
	case KindCount:
		var n int64
		n, err = database.Collection(c.Collection).CountDocuments(qctx, c.Filter, lim.Timeout)
		if err == nil {
			out.Columns, out.Rows = []string{"count"}, [][]any{{n}}
		}
	case KindEstimatedCount:
		var n int64
		n, err = database.Collection(c.Collection).EstimatedCount(qctx, lim.Timeout)
		if err == nil {
			out.Columns, out.Rows = []string{"count"}, [][]any{{n}}
		}
	case KindDistinct:
		err = e.runDistinct(qctx, database, c, lim, sh, out)
	case KindListCollections:
		err = e.runListCollections(qctx, database, lim, out)
	case KindDBStats:
		var d bson.D
		if d, err = database.Stats(qctx); err == nil {
			out.Columns, out.Rows = sh.docRows([]bson.D{d})
		}
	case KindCollStats:
		var d bson.D
		if d, err = database.Collection(c.Collection).Stats(qctx); err == nil {
			out.Columns, out.Rows = sh.docRows([]bson.D{dropKeys(d, "wiredTiger", "indexDetails", "$clusterTime", "operationTime", "$gleStats", "$configServerState")})
		}
	case KindListIndexes:
		var cur Cursor
		if cur, err = database.Collection(c.Collection).Indexes(qctx); err == nil {
			var docs []bson.D
			var tb bool
			docs, tb, err = readDocs(qctx, cur, 1000, lim.MaxBytes)
			if err == nil {
				out.Columns, out.Rows = sh.docRows(docs)
				out.Truncated = tb
			}
		}
	default:
		return nil, uerr("不支持的语句种类 %q", c.Kind)
	}
	out.ElapsedMs = time.Since(start).Milliseconds()
	if err != nil {
		return nil, e.friendlyError(qctx, err)
	}
	if len(out.Rows) > lim.MaxRows { // 元数据类结果也遵守行数上限
		out.Rows, out.Truncated = out.Rows[:lim.MaxRows], true
	}
	if out.Truncated {
		out.Warnings = append(out.Warnings, fmt.Sprintf("结果已按上限截断（最多 %d 行 / %d MB 原始数据）；请加过滤条件、投影或用 skip / limit 分页。", lim.MaxRows, lim.MaxBytes>>20))
	}
	if sh.cut > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%d 个值超过 %d KB，已截断显示。", sh.cut, lim.MaxCellBytes>>10))
	}
	if sh.binary > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%d 个二进制值已按 BinData(子类型, \\xHH...) 显示。", sh.binary))
	}
	return out, nil
}

func dropKeys(d bson.D, keys ...string) bson.D {
	out := make(bson.D, 0, len(d))
next:
	for _, e := range d {
		for _, k := range keys {
			if e.Key == k {
				continue next
			}
		}
		out = append(out, e)
	}
	return out
}

func batchOf(n int64) int32 {
	if n < 1 {
		n = 1
	}
	if n > 1000 {
		n = 1000
	}
	return int32(n)
}

func (e *Engine) runFind(ctx context.Context, db Database, c *Checked, lim QueryLimits, sh *shaper, out *Shaped) error {
	capRows := int64(lim.MaxRows)
	fetch := capRows + 1 // 多取一条用来判断是否被截断
	if c.Limit > 0 && c.Limit <= capRows {
		fetch = c.Limit
	}
	cur, err := db.Collection(c.Collection).Find(ctx, FindSpec{
		Filter: c.Filter, Projection: c.Projection, Sort: c.Sort, Skip: c.Skip,
		Limit: fetch, BatchSize: batchOf(fetch), MaxTime: lim.Timeout,
	})
	if err != nil {
		return err
	}
	docs, tb, err := readDocs(ctx, cur, int(fetch), lim.MaxBytes)
	if err != nil {
		return err
	}
	if len(docs) > lim.MaxRows {
		docs, tb = docs[:lim.MaxRows], true
	}
	out.Truncated = tb
	lim.maskDocs(docs)
	out.Columns, out.Rows = sh.docRows(docs)
	return nil
}

func (e *Engine) runAggregate(ctx context.Context, db Database, c *Checked, lim QueryLimits, sh *shaper, out *Shaped) error {
	// 在用户 pipeline 末尾补 $limit(上限+1)：服务端提早结束，同时用多出的一条判断是否被截断。
	pipe := make([]bson.D, 0, len(c.Pipeline)+1)
	pipe = append(pipe, c.Pipeline...)
	pipe = append(pipe, bson.D{{Key: "$limit", Value: int64(lim.MaxRows) + 1}})
	cur, err := db.Collection(c.Collection).Aggregate(ctx, AggSpec{Pipeline: pipe, BatchSize: batchOf(int64(lim.MaxRows) + 1), MaxTime: lim.Timeout})
	if err != nil {
		return err
	}
	docs, tb, err := readDocs(ctx, cur, lim.MaxRows+1, lim.MaxBytes)
	if err != nil {
		return err
	}
	if len(docs) > lim.MaxRows {
		docs, tb = docs[:lim.MaxRows], true
	}
	out.Truncated = tb
	lim.maskDocs(docs)
	out.Columns, out.Rows = sh.docRows(docs)
	return nil
}

func (e *Engine) runDistinct(ctx context.Context, db Database, c *Checked, lim QueryLimits, sh *shaper, out *Shaped) error {
	arr, err := db.Collection(c.Collection).Distinct(ctx, c.Field, c.Filter, lim.Timeout)
	if err != nil {
		return err
	}
	vals, err := arr.Values()
	if err != nil {
		return err
	}
	out.Columns = []string{c.Field}
	total := 0
	for _, rv := range vals {
		if len(out.Rows) >= lim.MaxRows || (len(out.Rows) > 0 && total+len(rv.Value) > lim.MaxBytes) {
			out.Truncated = true
			break
		}
		total += len(rv.Value)
		var v any
		if err := rv.Unmarshal(&v); err != nil {
			return err
		}
		if lim.Mask != nil {
			v = lim.Mask.Distinct(c.Field, v)
		}
		out.Rows = append(out.Rows, []any{sh.cell(v)})
	}
	return nil
}

func (e *Engine) runListCollections(ctx context.Context, db Database, lim QueryLimits, out *Shaped) error {
	infos, err := db.ListCollections(ctx)
	if err != nil {
		return err
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	out.Columns = []string{"collection", "type"}
	for _, ci := range infos {
		if strings.HasPrefix(ci.Name, "system.") {
			continue
		}
		out.Rows = append(out.Rows, []any{ci.Name, collKind(ci)})
	}
	return nil
}

// collKind 返回集合的类型标记：collection / view / timeseries / capped。
func collKind(ci CollectionInfo) string {
	if ci.Type == "view" || ci.Type == "timeseries" {
		return ci.Type
	}
	for _, e := range ci.Options {
		switch e.Key {
		case "timeseries":
			return "timeseries"
		case "capped":
			if b, ok := e.Value.(bool); ok && b {
				return "capped"
			}
		}
	}
	return orDefault(ci.Type, "collection")
}

// readDocs 从游标逐批读取至多 maxDocs 个文档，并限制原始 BSON 总量（第一个文档总是返回）。关闭游标。
// 第二个返回值表示因字节上限提前结束。
func readDocs(ctx context.Context, cur Cursor, maxDocs, maxBytes int) ([]bson.D, bool, error) {
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = cur.Close(cctx)
	}()
	var docs []bson.D
	total := 0
	truncated := false
	for cur.Next(ctx) {
		raw := cur.Raw()
		if len(docs) > 0 && total+len(raw) > maxBytes {
			truncated = true
			break
		}
		var d bson.D
		if err := bson.Unmarshal(raw, &d); err != nil {
			return nil, false, err
		}
		total += len(raw)
		docs = append(docs, d)
		if len(docs) >= maxDocs {
			break
		}
	}
	if err := cur.Err(); err != nil {
		return nil, false, err
	}
	return docs, truncated, nil
}

// Query 实现 engine.Engine：执行一条只读语句（供数据导出等使用）。语句先过 CheckUserQuery；
// opt.Limit>0 作为行数上限，opt.MaxExecMs>0 作为超时。
func (e *Engine) Query(ctx context.Context, text string, opt engine.QueryOptions) (*engine.ResultSet, error) {
	c, err := CheckUserQuery(text, CheckOptions{MaxRows: opt.Limit})
	if err != nil {
		return nil, err
	}
	db := opt.DBName
	if db == "" {
		db = e.defaultDB
	}
	sh, err := e.RunUser(ctx, db, c, QueryLimits{MaxRows: opt.Limit, Timeout: time.Duration(opt.MaxExecMs) * time.Millisecond})
	if err != nil {
		return nil, err
	}
	return &engine.ResultSet{Rows: sh.Rows, ColumnList: sh.Columns, CostTimeMs: sh.ElapsedMs}, nil
}
