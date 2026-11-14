// @author: kevinkelly
// @license: Apache Licence
// @file: fake.go
// @time: 2026/10/28

// Package mongotest 是 MongoDB 引擎的内存假后端（实现 mongo.Backend），供需要真实 mongo 引擎、又不能连真实库的测试使用
// （internal/query、internal/dict 等）。行为取自 internal/engine/mongo/fake_test.go 的精简版：按 skip / limit 返回预置文档
// （不做过滤条件求值），记录每次调用的参数。
package mongotest

import (
	"context"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"dbtool/internal/engine/mongo"
)

// FindCall / AggCall 是记录下来的一次调用。
type FindCall struct {
	DB, Coll string
	Spec     mongo.FindSpec
}
type AggCall struct {
	DB, Coll string
	Spec     mongo.AggSpec
}

// Fake 是假后端。用 mongo.NewWithBackend(f) 得到真实的引擎。
type Fake struct {
	mu      sync.Mutex
	dbs     map[string]*DB
	Version string
	PingErr error
	ListErr error

	Finds []FindCall
	Aggs  []AggCall
}

// New 创建空的假后端。
func New() *Fake { return &Fake{dbs: map[string]*DB{}, Version: "7.0.5"} }

// DB 是假库。
type DB struct {
	be    *Fake
	name  string
	colls map[string]*Coll
	Infos []mongo.CollectionInfo
	Stat  bson.D
	Err   error

	// ListCalls 是 listCollections 被调用的次数。
	ListCalls int
}

// Coll 是假集合。
type Coll struct {
	db       *DB
	name     string
	Docs     []bson.D
	IndexDoc []bson.D
	StatDoc  bson.D
	Err      error         // 所有操作返回的错误
	Block    bool          // Next 阻塞直到 ctx 结束
	Delay    time.Duration // 每次 Next 前的等待
}

// DB 取得（必要时创建）一个库。
func (f *Fake) DB(name string) *DB {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.dbs[name]
	if d == nil {
		d = &DB{be: f, name: name, colls: map[string]*Coll{}}
		f.dbs[name] = d
	}
	return d
}

// Coll 取得（必要时创建）一个集合并追加文档。
func (d *DB) Coll(name string, docs ...bson.D) *Coll {
	d.be.mu.Lock()
	defer d.be.mu.Unlock()
	c := d.colls[name]
	if c == nil {
		c = &Coll{db: d, name: name}
		d.colls[name] = c
		d.Infos = append(d.Infos, mongo.CollectionInfo{Name: name, Type: "collection"})
	}
	c.Docs = append(c.Docs, docs...)
	return c
}

// View 登记一个视图：listCollections 里它是 type=view，options 带 viewOn 与 pipeline。假后端不求值管道，
// docs 就是对这个视图查询时返回的文档（即已经过管道之后的样子）。
func (d *DB) View(name, viewOn string, pipeline []bson.D, docs ...bson.D) *Coll {
	c := d.Coll(name, docs...)
	d.be.mu.Lock()
	defer d.be.mu.Unlock()
	pl := make(bson.A, len(pipeline))
	for i, st := range pipeline {
		pl[i] = st
	}
	for i := range d.Infos {
		if d.Infos[i].Name == name {
			d.Infos[i].Type = "view"
			d.Infos[i].Options = bson.D{{Key: "viewOn", Value: viewOn}, {Key: "pipeline", Value: pl}}
		}
	}
	return c
}

func (f *Fake) Database(name string) mongo.Database { return f.DB(name) }
func (f *Fake) ListDatabases(context.Context) ([]mongo.DatabaseInfo, error) {
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []mongo.DatabaseInfo{{Name: "admin"}, {Name: "local"}, {Name: "config"}}
	for n := range f.dbs {
		out = append(out, mongo.DatabaseInfo{Name: n, SizeOnDisk: 1024})
	}
	return out, nil
}
func (f *Fake) Ping(context.Context) error                    { return f.PingErr }
func (f *Fake) ServerVersion(context.Context) (string, error) { return f.Version, nil }
func (f *Fake) Close(context.Context) error                   { return nil }

func (d *DB) Collection(name string) mongo.Collection {
	d.be.mu.Lock()
	c := d.colls[name]
	d.be.mu.Unlock()
	if c == nil {
		c = &Coll{db: d, name: name}
	}
	return c
}
func (d *DB) ListCollections(context.Context) ([]mongo.CollectionInfo, error) {
	d.be.mu.Lock()
	d.ListCalls++
	d.be.mu.Unlock()
	if d.Err != nil {
		return nil, d.Err
	}
	return append([]mongo.CollectionInfo(nil), d.Infos...), nil
}
func (d *DB) Stats(context.Context) (bson.D, error) { return d.Stat, d.Err }

type cursor struct {
	ctx  context.Context
	c    *Coll
	raws []bson.Raw
	i    int
	cur  bson.Raw
}

func (c *cursor) Next(ctx context.Context) bool {
	if c.c.Delay > 0 {
		select {
		case <-time.After(c.c.Delay):
		case <-ctx.Done():
			return false
		}
	}
	if c.c.Block {
		<-ctx.Done()
		return false
	}
	if ctx.Err() != nil || c.i >= len(c.raws) {
		return false
	}
	c.cur = c.raws[c.i]
	c.i++
	return true
}
func (c *cursor) Raw() bson.Raw { return c.cur }
func (c *cursor) Err() error {
	if c.c.Block || c.c.Delay > 0 {
		return c.ctx.Err()
	}
	return nil
}
func (c *cursor) Close(context.Context) error { return nil }

func (c *Coll) cursor(ctx context.Context, docs []bson.D) mongo.Cursor {
	raws := make([]bson.Raw, len(docs))
	for i, d := range docs {
		b, err := bson.Marshal(d)
		if err != nil {
			panic(err)
		}
		raws[i] = b
	}
	return &cursor{ctx: ctx, c: c, raws: raws}
}

func (c *Coll) Find(ctx context.Context, s mongo.FindSpec) (mongo.Cursor, error) {
	c.db.be.mu.Lock()
	c.db.be.Finds = append(c.db.be.Finds, FindCall{DB: c.db.name, Coll: c.name, Spec: s})
	c.db.be.mu.Unlock()
	if c.Err != nil {
		return nil, c.Err
	}
	docs := c.Docs
	if s.Skip > 0 {
		if int(s.Skip) >= len(docs) {
			docs = nil
		} else {
			docs = docs[s.Skip:]
		}
	}
	if s.Limit > 0 && int(s.Limit) < len(docs) {
		docs = docs[:s.Limit]
	}
	return c.cursor(ctx, docs), nil
}

func (c *Coll) Aggregate(ctx context.Context, s mongo.AggSpec) (mongo.Cursor, error) {
	c.db.be.mu.Lock()
	c.db.be.Aggs = append(c.db.be.Aggs, AggCall{DB: c.db.name, Coll: c.name, Spec: s})
	c.db.be.mu.Unlock()
	if c.Err != nil {
		return nil, c.Err
	}
	docs := c.Docs
	if n := len(s.Pipeline); n > 0 && len(s.Pipeline[n-1]) > 0 && s.Pipeline[n-1][0].Key == "$limit" {
		var lim int64 = -1
		switch v := s.Pipeline[n-1][0].Value.(type) {
		case int32:
			lim = int64(v)
		case int64:
			lim = v
		case int:
			lim = int64(v)
		}
		if lim >= 0 && int(lim) < len(docs) {
			docs = docs[:lim]
		}
	}
	return c.cursor(ctx, docs), nil
}

func (c *Coll) CountDocuments(context.Context, bson.D, time.Duration) (int64, error) {
	if c.Err != nil {
		return 0, c.Err
	}
	return int64(len(c.Docs)), nil
}
func (c *Coll) EstimatedCount(context.Context, time.Duration) (int64, error) {
	return int64(len(c.Docs)), c.Err
}
func (c *Coll) Distinct(context.Context, string, bson.D, time.Duration) (bson.RawArray, error) {
	return nil, c.Err
}
func (c *Coll) Indexes(ctx context.Context) (mongo.Cursor, error) {
	if c.Err != nil {
		return nil, c.Err
	}
	return c.cursor(ctx, c.IndexDoc), nil
}
func (c *Coll) Stats(context.Context) (bson.D, error) {
	if c.Err != nil {
		return nil, c.Err
	}
	return c.StatDoc, nil
}
