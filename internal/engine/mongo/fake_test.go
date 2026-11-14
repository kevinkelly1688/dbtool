// @author: kevinkelly
// @license: Apache Licence
// @file: fake_test.go
// @time: 2026/10/28

package mongo

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// 内存假实现：记录每次调用的参数，按 skip / limit 返回预置文档（不做过滤条件求值）。

type findCall struct {
	DB, Coll string
	Spec     FindSpec
}
type aggCall struct {
	DB, Coll string
	Spec     AggSpec
}

type fakeBackend struct {
	mu      sync.Mutex
	dbs     map[string]*fakeDB
	version string
	pingErr error
	listErr error

	finds  []findCall
	aggs   []aggCall
	counts []string // "db.coll"
	other  []string // 其它调用名
}

type fakeDB struct {
	be    *fakeBackend
	name  string
	colls map[string]*fakeColl
	infos []CollectionInfo
	stats bson.D
	err   error
}

type fakeColl struct {
	db       *fakeDB
	name     string
	docs     []bson.D
	indexes  []bson.D
	stats    bson.D
	distinct bson.A
	count    int64
	err      error         // 所有操作返回的错误
	cursorEr error         // 游标读取中途的错误
	block    bool          // Next 阻塞直到 ctx 结束
	delay    time.Duration // 每次 Next 前的等待
}

func newFake() *fakeBackend { return &fakeBackend{dbs: map[string]*fakeDB{}, version: "7.0.5"} }

func (b *fakeBackend) db(name string) *fakeDB {
	b.mu.Lock()
	defer b.mu.Unlock()
	d := b.dbs[name]
	if d == nil {
		d = &fakeDB{be: b, name: name, colls: map[string]*fakeColl{}}
		b.dbs[name] = d
	}
	return d
}

func (d *fakeDB) coll(name string, docs ...bson.D) *fakeColl {
	d.be.mu.Lock()
	defer d.be.mu.Unlock()
	c := d.colls[name]
	if c == nil {
		c = &fakeColl{db: d, name: name}
		d.colls[name] = c
		d.infos = append(d.infos, CollectionInfo{Name: name, Type: "collection"})
	}
	c.docs = append(c.docs, docs...)
	return c
}

func (b *fakeBackend) Database(name string) Database { return b.db(name) }
func (b *fakeBackend) ListDatabases(ctx context.Context) ([]DatabaseInfo, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []DatabaseInfo{{Name: "admin"}, {Name: "local"}, {Name: "config"}}
	for n := range b.dbs {
		out = append(out, DatabaseInfo{Name: n, SizeOnDisk: 1024})
	}
	return out, nil
}
func (b *fakeBackend) Ping(ctx context.Context) error                    { return b.pingErr }
func (b *fakeBackend) ServerVersion(ctx context.Context) (string, error) { return b.version, nil }
func (b *fakeBackend) Close(ctx context.Context) error                   { return nil }

func (d *fakeDB) Collection(name string) Collection {
	d.be.mu.Lock()
	c := d.colls[name]
	d.be.mu.Unlock()
	if c == nil {
		c = &fakeColl{db: d, name: name}
	}
	return c
}
func (d *fakeDB) ListCollections(ctx context.Context) ([]CollectionInfo, error) {
	if d.err != nil {
		return nil, d.err
	}
	return append([]CollectionInfo(nil), d.infos...), nil
}
func (d *fakeDB) Stats(ctx context.Context) (bson.D, error) {
	d.be.mu.Lock()
	d.be.other = append(d.be.other, "dbStats")
	d.be.mu.Unlock()
	return d.stats, d.err
}

type fakeCursor struct {
	ctx    context.Context
	c      *fakeColl
	raws   []bson.Raw
	i      int
	cur    bson.Raw
	closed *bool
}

func (c *fakeCursor) Next(ctx context.Context) bool {
	if c.c.delay > 0 {
		select {
		case <-time.After(c.c.delay):
		case <-ctx.Done():
			return false
		}
	}
	if c.c.block {
		<-ctx.Done()
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	if c.i >= len(c.raws) {
		return false
	}
	if c.c.cursorEr != nil && c.i >= 1 {
		return false
	}
	c.cur = c.raws[c.i]
	c.i++
	return true
}
func (c *fakeCursor) Raw() bson.Raw { return c.cur }
func (c *fakeCursor) Err() error {
	if c.c.cursorEr != nil && c.i >= 1 {
		return c.c.cursorEr
	}
	if c.c.block || c.c.delay > 0 {
		return c.ctx.Err()
	}
	return nil
}
func (c *fakeCursor) Close(ctx context.Context) error { *c.closed = true; return nil }

var fakeClosed []*bool
var fakeClosedMu sync.Mutex

func (c *fakeColl) cursor(ctx context.Context, docs []bson.D) Cursor {
	raws := make([]bson.Raw, len(docs))
	for i, d := range docs {
		b, err := bson.Marshal(d)
		if err != nil {
			panic(err)
		}
		raws[i] = b
	}
	cl := new(bool)
	fakeClosedMu.Lock()
	fakeClosed = append(fakeClosed, cl)
	fakeClosedMu.Unlock()
	return &fakeCursor{ctx: ctx, c: c, raws: raws, closed: cl}
}

func (c *fakeColl) rec(f func()) {
	c.db.be.mu.Lock()
	defer c.db.be.mu.Unlock()
	f()
}

func (c *fakeColl) Find(ctx context.Context, s FindSpec) (Cursor, error) {
	c.rec(func() { c.db.be.finds = append(c.db.be.finds, findCall{DB: c.db.name, Coll: c.name, Spec: s}) })
	if c.err != nil {
		return nil, c.err
	}
	docs := c.docs
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

func (c *fakeColl) Aggregate(ctx context.Context, s AggSpec) (Cursor, error) {
	c.rec(func() { c.db.be.aggs = append(c.db.be.aggs, aggCall{DB: c.db.name, Coll: c.name, Spec: s}) })
	if c.err != nil {
		return nil, c.err
	}
	docs := c.docs
	if n := len(s.Pipeline); n > 0 && s.Pipeline[n-1][0].Key == "$limit" {
		if lim, ok := numToInt(s.Pipeline[n-1][0].Value); ok && int(lim) < len(docs) {
			docs = docs[:lim]
		}
	}
	return c.cursor(ctx, docs), nil
}

func (c *fakeColl) CountDocuments(ctx context.Context, filter bson.D, maxTime time.Duration) (int64, error) {
	c.rec(func() { c.db.be.counts = append(c.db.be.counts, c.db.name+"."+c.name) })
	if c.err != nil {
		return 0, c.err
	}
	if c.count > 0 {
		return c.count, nil
	}
	return int64(len(c.docs)), nil
}
func (c *fakeColl) EstimatedCount(ctx context.Context, maxTime time.Duration) (int64, error) {
	c.rec(func() { c.db.be.other = append(c.db.be.other, "estimated") })
	return int64(len(c.docs)), c.err
}
func (c *fakeColl) Distinct(ctx context.Context, field string, filter bson.D, maxTime time.Duration) (bson.RawArray, error) {
	if c.err != nil {
		return nil, c.err
	}
	b, err := bson.Marshal(bson.D{{Key: "a", Value: c.distinct}})
	if err != nil {
		return nil, err
	}
	return bson.Raw(b).Lookup("a").Array(), nil
}
func (c *fakeColl) Indexes(ctx context.Context) (Cursor, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.cursor(ctx, c.indexes), nil
}
func (c *fakeColl) Stats(ctx context.Context) (bson.D, error) {
	c.rec(func() { c.db.be.other = append(c.db.be.other, "collStats:"+c.name) })
	if c.err != nil {
		return nil, c.err
	}
	return c.stats, nil
}

// codeErr 模拟驱动的 CommandError。
type codeErr struct {
	code int
	msg  string
}

func (e *codeErr) Error() string           { return e.msg }
func (e *codeErr) HasErrorCode(c int) bool { return c == e.code }

var _ = errors.New
