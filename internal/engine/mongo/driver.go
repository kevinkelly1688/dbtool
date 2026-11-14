// @author: kevinkelly
// @license: Apache Licence
// @file: driver.go
// @time: 2026/10/28

package mongo

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"dbtool/internal/engine"
)

// 真实 mongo-driver/v2 的适配层。只发 find / aggregate / count / distinct / listCollections / listIndexes /
// collStats / dbStats / buildInfo / listDatabases / ping 这些固定读命令，没有任何把用户文本直接当命令的出口。
//
// 驱动 v2 已没有逐操作的 MaxTime 选项：maxTimeMS 由 context 的截止时间推出（CSOT），所以这里用 context.WithTimeout 表达执行时间上限。

const (
	connectTimeout = 10 * time.Second
	authSource     = "admin" // 认证库固定为 admin
)

type driverBackend struct {
	c *mongo.Client
}

// newDriverBackend 创建客户端（懒连接：此时不发网络请求）。
func newDriverBackend(c engine.Conn) (Backend, error) {
	port := c.Port
	if port == 0 {
		port = 27017
	}
	o := options.Client().
		SetHosts([]string{fmt.Sprintf("%s:%d", c.Host, port)}).
		SetDirect(true). // 只连用户配置的这个节点，不做副本集拓扑发现
		SetAppName("dbtool").
		SetConnectTimeout(connectTimeout).
		SetServerSelectionTimeout(connectTimeout).
		SetMaxPoolSize(4).
		SetMaxConnIdleTime(2 * time.Minute).
		SetReadPreference(readpref.PrimaryPreferred())
	if c.User != "" {
		o.SetAuth(options.Credential{Username: c.User, Password: c.Password, PasswordSet: true, AuthSource: authSource})
	}
	if c.SSL {
		o.SetTLSConfig(&tls.Config{InsecureSkipVerify: !c.VerifySSL, MinVersion: tls.VersionTLS12}) //nolint:gosec // 由实例配置 VerifySSL 决定
	}
	cl, err := mongo.Connect(o)
	if err != nil {
		return nil, errors.New(scrubPlain(err.Error(), c.Password))
	}
	return &driverBackend{c: cl}, nil
}

func (b *driverBackend) Database(name string) Database {
	return &driverDB{db: b.c.Database(name)}
}

func (b *driverBackend) ListDatabases(ctx context.Context) ([]DatabaseInfo, error) {
	res, err := b.c.ListDatabases(ctx, bson.D{}, options.ListDatabases().SetAuthorizedDatabases(true))
	if err != nil {
		// 个别旧版本不认 authorizedDatabases，回落到普通调用
		var err2 error
		res, err2 = b.c.ListDatabases(ctx, bson.D{})
		if err2 != nil {
			return nil, err
		}
	}
	out := make([]DatabaseInfo, 0, len(res.Databases))
	for _, d := range res.Databases {
		out = append(out, DatabaseInfo{Name: d.Name, SizeOnDisk: d.SizeOnDisk, Empty: d.Empty})
	}
	return out, nil
}

func (b *driverBackend) Ping(ctx context.Context) error {
	return b.c.Ping(ctx, readpref.PrimaryPreferred())
}

func (b *driverBackend) ServerVersion(ctx context.Context) (string, error) {
	var res struct {
		Version string `bson:"version"`
	}
	if err := b.c.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&res); err != nil {
		return "", err
	}
	return res.Version, nil
}

func (b *driverBackend) Close(ctx context.Context) error { return b.c.Disconnect(ctx) }

type driverDB struct{ db *mongo.Database }

func (d *driverDB) Collection(name string) Collection {
	return &driverColl{c: d.db.Collection(name), db: d.db, name: name}
}

func (d *driverDB) ListCollections(ctx context.Context) ([]CollectionInfo, error) {
	return d.listCollections(ctx, bson.D{})
}

// ListCollectionsByName 只列出名字在 names 里的集合（listCollections 的 name 过滤；查视图定义用，不必把整个库的集合都列一遍）。
func (d *driverDB) ListCollectionsByName(ctx context.Context, names []string) ([]CollectionInfo, error) {
	return d.listCollections(ctx, bson.D{{Key: "name", Value: bson.D{{Key: "$in", Value: names}}}})
}

func (d *driverDB) listCollections(ctx context.Context, filter bson.D) ([]CollectionInfo, error) {
	specs, err := d.db.ListCollectionSpecifications(ctx, filter, options.ListCollections().SetAuthorizedCollections(true))
	if err != nil {
		return nil, err
	}
	out := make([]CollectionInfo, 0, len(specs))
	for _, s := range specs {
		ci := CollectionInfo{Name: s.Name, Type: s.Type, ReadOnly: s.ReadOnly}
		if len(s.Options) > 0 {
			_ = bson.Unmarshal(s.Options, &ci.Options)
		}
		out = append(out, ci)
	}
	return out, nil
}

func (d *driverDB) Stats(ctx context.Context) (bson.D, error) {
	var res bson.D
	err := d.db.RunCommand(ctx, bson.D{{Key: "dbStats", Value: 1}, {Key: "scale", Value: 1}}).Decode(&res)
	return res, err
}

type driverColl struct {
	c    *mongo.Collection
	db   *mongo.Database
	name string
}

func withMaxTime(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// cancelCursor 把 cancel 绑到游标关闭上：游标读取期间 context 必须有效。
type driverCursor struct {
	c      *mongo.Cursor
	cancel context.CancelFunc
}

func (c *driverCursor) Next(ctx context.Context) bool { return c.c.Next(ctx) }
func (c *driverCursor) Raw() bson.Raw                 { return c.c.Current }
func (c *driverCursor) Err() error                    { return c.c.Err() }
func (c *driverCursor) Close(ctx context.Context) error {
	defer c.cancel()
	return c.c.Close(ctx)
}

func (d *driverColl) Find(ctx context.Context, s FindSpec) (Cursor, error) {
	ctx, cancel := withMaxTime(ctx, s.MaxTime)
	o := options.Find()
	if len(s.Projection) > 0 {
		o.SetProjection(s.Projection)
	}
	if len(s.Sort) > 0 {
		o.SetSort(s.Sort)
	}
	if s.Skip > 0 {
		o.SetSkip(s.Skip)
	}
	if s.Limit > 0 {
		o.SetLimit(s.Limit)
	}
	if s.BatchSize > 0 {
		o.SetBatchSize(s.BatchSize)
	}
	filter := s.Filter
	if filter == nil {
		filter = bson.D{}
	}
	cur, err := d.c.Find(ctx, filter, o)
	if err != nil {
		cancel()
		return nil, err
	}
	return &driverCursor{c: cur, cancel: cancel}, nil
}

func (d *driverColl) Aggregate(ctx context.Context, s AggSpec) (Cursor, error) {
	ctx, cancel := withMaxTime(ctx, s.MaxTime)
	o := options.Aggregate()
	if s.BatchSize > 0 {
		o.SetBatchSize(s.BatchSize)
	}
	cur, err := d.c.Aggregate(ctx, s.Pipeline, o)
	if err != nil {
		cancel()
		return nil, err
	}
	return &driverCursor{c: cur, cancel: cancel}, nil
}

func (d *driverColl) CountDocuments(ctx context.Context, filter bson.D, maxTime time.Duration) (int64, error) {
	ctx, cancel := withMaxTime(ctx, maxTime)
	defer cancel()
	if filter == nil {
		filter = bson.D{}
	}
	return d.c.CountDocuments(ctx, filter)
}

func (d *driverColl) EstimatedCount(ctx context.Context, maxTime time.Duration) (int64, error) {
	ctx, cancel := withMaxTime(ctx, maxTime)
	defer cancel()
	return d.c.EstimatedDocumentCount(ctx)
}

func (d *driverColl) Distinct(ctx context.Context, field string, filter bson.D, maxTime time.Duration) (bson.RawArray, error) {
	ctx, cancel := withMaxTime(ctx, maxTime)
	defer cancel()
	if filter == nil {
		filter = bson.D{}
	}
	return d.c.Distinct(ctx, field, filter).Raw()
}

func (d *driverColl) Indexes(ctx context.Context) (Cursor, error) {
	cur, err := d.c.Indexes().List(ctx)
	if err != nil {
		return nil, err
	}
	return &driverCursor{c: cur, cancel: func() {}}, nil
}

func (d *driverColl) Stats(ctx context.Context) (bson.D, error) {
	var res bson.D
	err := d.db.RunCommand(ctx, bson.D{{Key: "collStats", Value: d.name}, {Key: "scale", Value: 1}}).Decode(&res)
	return res, err
}

func scrubPlain(s, password string) string {
	if password == "" {
		return s
	}
	return strings.ReplaceAll(s, password, "***")
}
