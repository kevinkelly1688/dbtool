// @author: kevinkelly
// @license: Apache Licence
// @file: meta.go
// @time: 2026/10/28

package mongo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// 库表元数据（数据字典用）。“库”= database，“表”= collection，“列”= 采样得到的顶层字段。

const (
	// SampleDocs 是推断字段时采样的文档数。
	SampleDocs = 100
	// MaxStatsCollections 是 TableInfos 里最多逐个执行 collStats 的集合数；超出的集合只返回名称 / 类型（HasStats=false）。
	MaxStatsCollections = 200
	sampleMaxBytes      = 8 << 20
)

// TableInfo 是集合信息。
type TableInfo struct {
	Name         string
	Comment      string // MongoDB 没有表注释；视图写“视图，源集合 xxx”
	Engine       string // 集合形态明细：capped / timeseries / view，普通集合为空
	Type         string // collection / view / timeseries
	RowsEstimate int64  // collStats.count（元数据计数，非精确）
	DataBytes    int64  // collStats.size（未压缩的数据大小）
	IndexBytes   int64  // collStats.totalIndexSize
	StorageBytes int64  // collStats.storageSize（磁盘占用）
	IndexCount   int
	CreateTime   *time.Time // MongoDB 不记录，恒为 nil
	UpdateTime   *time.Time // 同上
	HasStats     bool       // 是否取到了 collStats
	Options      string     // 集合选项（JSON，没有则为空）
}

// ColumnInfo 是采样得到的字段信息。
type ColumnInfo struct {
	Name     string
	Type     string // BSON 类型名，出现多种类型时按出现次数排序用 | 连接
	Nullable bool   // 采样里有文档缺这个字段，或值为 null
	Default  *string
	Comment  string // “采样 N 个文档，M 个含该字段”
	IsPK     bool   // 只有 _id
	KeySeq   int
}

// IndexKey 是索引的一个键。
type IndexKey struct {
	Field string
	Order string // "1" / "-1" / "text" / "2dsphere" / "hashed" ...
}

// IndexInfo 是索引信息。
type IndexInfo struct {
	Name       string
	Keys       []IndexKey
	Unique     bool
	Sparse     bool
	TTLSeconds *int64
	Partial    string // partialFilterExpression 的 JSON
	Hidden     bool
	Definition string // 完整的 listIndexes 文档（JSON）
}

func (e *Engine) collection(ctx context.Context, db, table string) (Collection, error) {
	if err := ValidDBName(db); err != nil {
		return nil, err
	}
	if err := validCollName(table); err != nil {
		return nil, err
	}
	return e.be.Database(db).Collection(table), nil
}

func i64(v any) int64 {
	switch n := v.(type) {
	case int32:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

func lookup(d bson.D, key string) (any, bool) {
	for _, e := range d {
		if e.Key == key {
			return e.Value, true
		}
	}
	return nil, false
}

// TableInfos 返回库里每个集合的信息（逐个 collStats，最多 MaxStatsCollections 个，视图不查）。
func (e *Engine) TableInfos(ctx context.Context, db string) ([]TableInfo, error) {
	infos, err := e.collections(ctx, db)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out := make([]TableInfo, 0, len(infos))
	for i, ci := range infos {
		kind := collKind(ci)
		ti := TableInfo{Name: ci.Name, Type: "collection"}
		switch kind {
		case "view", "timeseries":
			ti.Type, ti.Engine = kind, kind
		case "capped":
			ti.Engine = kind
		}
		if len(ci.Options) > 0 {
			ti.Options = jsonText(ci.Options, false)
		}
		if v, ok := lookup(ci.Options, "viewOn"); ok {
			ti.Comment = fmt.Sprintf("视图，源集合 %v", v)
		}
		if ci.Type != "view" && i < MaxStatsCollections && ctx.Err() == nil {
			sctx, scancel := context.WithTimeout(ctx, 8*time.Second)
			st, err := e.be.Database(db).Collection(ci.Name).Stats(sctx)
			scancel()
			if err == nil {
				ti.HasStats = true
				if v, ok := lookup(st, "count"); ok {
					ti.RowsEstimate = i64(v)
				}
				if v, ok := lookup(st, "size"); ok {
					ti.DataBytes = i64(v)
				}
				if v, ok := lookup(st, "totalIndexSize"); ok {
					ti.IndexBytes = i64(v)
				}
				if v, ok := lookup(st, "storageSize"); ok {
					ti.StorageBytes = i64(v)
				}
				if v, ok := lookup(st, "nindexes"); ok {
					ti.IndexCount = int(i64(v))
				}
			}
		}
		out = append(out, ti)
	}
	return out, nil
}

func bsonTypeName(t bson.Type) string {
	switch t {
	case bson.TypeDouble:
		return "double"
	case bson.TypeString:
		return "string"
	case bson.TypeEmbeddedDocument:
		return "object"
	case bson.TypeArray:
		return "array"
	case bson.TypeBinary:
		return "binData"
	case bson.TypeUndefined:
		return "undefined"
	case bson.TypeObjectID:
		return "objectId"
	case bson.TypeBoolean:
		return "bool"
	case bson.TypeDateTime:
		return "date"
	case bson.TypeNull:
		return "null"
	case bson.TypeRegex:
		return "regex"
	case bson.TypeDBPointer:
		return "dbPointer"
	case bson.TypeJavaScript:
		return "javascript"
	case bson.TypeSymbol:
		return "symbol"
	case bson.TypeCodeWithScope:
		return "javascriptWithScope"
	case bson.TypeInt32:
		return "int"
	case bson.TypeTimestamp:
		return "timestamp"
	case bson.TypeInt64:
		return "long"
	case bson.TypeDecimal128:
		return "decimal"
	case bson.TypeMinKey:
		return "minKey"
	case bson.TypeMaxKey:
		return "maxKey"
	}
	return fmt.Sprintf("type%d", byte(t))
}

// ColumnInfos 对集合采样最多 SampleDocs 个文档（find 的前 100 个，不是随机抽样），汇总顶层字段与 BSON 类型。
// 采样结果不代表 schema：没出现在样本里的字段不会被列出。
func (e *Engine) ColumnInfos(ctx context.Context, db, table string) ([]ColumnInfo, error) {
	coll, err := e.collection(ctx, db, table)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cur, err := coll.Find(ctx, FindSpec{Limit: SampleDocs, BatchSize: SampleDocs, MaxTime: 15 * time.Second})
	if err != nil {
		return nil, e.friendlyError(ctx, err)
	}
	defer func() {
		cctx, c2 := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer c2()
		_ = cur.Close(cctx)
	}()
	type fieldStat struct {
		order  int
		count  int
		types  map[string]int
		hasNil bool
	}
	stats := map[string]*fieldStat{}
	n, total := 0, 0
	for cur.Next(ctx) {
		raw := cur.Raw()
		if n > 0 && total+len(raw) > sampleMaxBytes {
			break
		}
		total += len(raw)
		els, err := raw.Elements()
		if err != nil {
			return nil, err
		}
		n++
		seen := map[string]bool{}
		for _, el := range els {
			k := el.Key()
			fs := stats[k]
			if fs == nil {
				fs = &fieldStat{order: len(stats), types: map[string]int{}}
				stats[k] = fs
			}
			if !seen[k] {
				seen[k] = true
				fs.count++
			}
			t := el.Value().Type
			fs.types[bsonTypeName(t)]++
			if t == bson.TypeNull || t == bson.TypeUndefined {
				fs.hasNil = true
			}
		}
		if n >= SampleDocs {
			break
		}
	}
	if err := cur.Err(); err != nil {
		return nil, e.friendlyError(ctx, err)
	}
	cols := make([]ColumnInfo, len(stats))
	for name, fs := range stats {
		tn := make([]string, 0, len(fs.types))
		for t := range fs.types {
			tn = append(tn, t)
		}
		sort.Slice(tn, func(i, j int) bool {
			if fs.types[tn[i]] != fs.types[tn[j]] {
				return fs.types[tn[i]] > fs.types[tn[j]]
			}
			return tn[i] < tn[j]
		})
		ci := ColumnInfo{
			Name:     name,
			Type:     strings.Join(tn, "|"),
			Nullable: fs.count < n || fs.hasNil,
			Comment:  fmt.Sprintf("采样 %d 个文档，%d 个含该字段", n, fs.count),
		}
		if name == "_id" {
			ci.IsPK, ci.KeySeq, ci.Nullable = true, 1, false
		}
		cols[fs.order] = ci
	}
	return cols, nil
}

// Indexes 返回集合的索引（listIndexes）。
func (e *Engine) Indexes(ctx context.Context, db, table string) ([]IndexInfo, error) {
	coll, err := e.collection(ctx, db, table)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cur, err := coll.Indexes(ctx)
	if err != nil {
		return nil, e.friendlyError(ctx, err)
	}
	docs, _, err := readDocs(ctx, cur, 1000, HardMaxBytes)
	if err != nil {
		return nil, e.friendlyError(ctx, err)
	}
	out := make([]IndexInfo, 0, len(docs))
	for _, d := range docs {
		ii := IndexInfo{Definition: jsonText(d, false)}
		for _, el := range d {
			switch el.Key {
			case "name":
				ii.Name, _ = el.Value.(string)
			case "key":
				if kd, ok := el.Value.(bson.D); ok {
					for _, k := range kd {
						ii.Keys = append(ii.Keys, IndexKey{Field: k.Key, Order: orderText(k.Value)})
					}
				}
			case "unique":
				ii.Unique, _ = el.Value.(bool)
			case "sparse":
				ii.Sparse, _ = el.Value.(bool)
			case "hidden":
				ii.Hidden, _ = el.Value.(bool)
			case "expireAfterSeconds":
				v := i64(el.Value)
				ii.TTLSeconds = &v
			case "partialFilterExpression":
				ii.Partial = jsonText(el.Value, false)
			}
		}
		out = append(out, ii)
	}
	return out, nil
}

func orderText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int32, int64, float64:
		return fmt.Sprintf("%d", i64(x))
	}
	return fmt.Sprintf("%v", v)
}

// jsonText 把 BSON 值转成 relaxed Extended JSON 文本（indent 为 true 时缩进）。
func jsonText(v any, indent bool) string {
	b, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: v}}, false, false)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	b = bytes.TrimSuffix(bytes.TrimPrefix(b, []byte(`{"v":`)), []byte(`}`))
	if indent {
		var buf bytes.Buffer
		if json.Indent(&buf, b, "", "  ") == nil {
			return buf.String()
		}
	}
	return string(b)
}

// CreateStatement 返回集合的“定义”：集合选项 + 索引定义的 JSON 文本。MongoDB 没有 DDL，这不是可执行语句，仅用于数据字典展示。
func (e *Engine) CreateStatement(ctx context.Context, db, table string) (string, error) {
	infos, err := e.collections(ctx, db)
	if err != nil {
		return "", err
	}
	if err := validCollName(table); err != nil {
		return "", err
	}
	var found *CollectionInfo
	for i := range infos {
		if infos[i].Name == table {
			found = &infos[i]
		}
	}
	if found == nil {
		return "", uerr("集合 %s.%s 不存在或无权访问", db, table)
	}
	idx, err := e.Indexes(ctx, db, table)
	if found.Type == "view" { // 视图没有索引
		idx, err = nil, nil
	}
	if err != nil {
		return "", err
	}
	opts := found.Options
	if opts == nil {
		opts = bson.D{}
	}
	defs := make(bson.A, 0, len(idx))
	for _, ii := range idx {
		var d bson.D
		if err := bson.UnmarshalExtJSON([]byte(ii.Definition), false, &d); err == nil {
			defs = append(defs, d)
		}
	}
	doc := bson.D{
		{Key: "note", Value: "MongoDB 没有 DDL：以下是集合选项与索引定义的 JSON 描述，不是可执行语句"}, // i18n-ignore: 写进 JSON 文本的 note 字段，属于生成的代码
		{Key: "database", Value: db},
		{Key: "collection", Value: table},
		{Key: "type", Value: collKind(*found)},
		{Key: "options", Value: opts},
		{Key: "indexes", Value: defs},
	}
	b, err := bson.MarshalExtJSONIndent(doc, false, false, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
