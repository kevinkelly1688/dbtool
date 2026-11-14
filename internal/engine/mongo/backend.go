// @author: kevinkelly
// @license: Apache Licence
// @file: backend.go
// @time: 2026/10/28

package mongo

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Backend 及其下的接口是本包与真实驱动之间唯一的缝：语句解析、白名单检查、限量与结果整形都只依赖这些接口，
// 单元测试用内存假实现（fake_test.go），真实 mongo-driver/v2 的适配在 driver.go。
//
// 注意这里刻意没有“原始 runCommand”：能发给服务端的只有下面列出的固定读操作。

// Cursor 逐个读取服务端返回的文档。Raw 返回的 bson.Raw 只在下一次 Next 之前有效。
type Cursor interface {
	Next(ctx context.Context) bool
	Raw() bson.Raw
	Err() error
	Close(ctx context.Context) error
}

// FindSpec 是 find 的全部参数。Limit 为 0 表示不限（引擎自己总会给出有限值）。
type FindSpec struct {
	Filter     bson.D
	Projection bson.D
	Sort       bson.D
	Skip       int64
	Limit      int64
	BatchSize  int32
	MaxTime    time.Duration // 服务端执行时间上限（maxTimeMS）
}

// AggSpec 是 aggregate 的全部参数；不开 allowDiskUse。
type AggSpec struct {
	Pipeline  []bson.D
	BatchSize int32
	MaxTime   time.Duration
}

// Collection 是集合句柄。
type Collection interface {
	Find(ctx context.Context, s FindSpec) (Cursor, error)
	Aggregate(ctx context.Context, s AggSpec) (Cursor, error)
	CountDocuments(ctx context.Context, filter bson.D, maxTime time.Duration) (int64, error)
	EstimatedCount(ctx context.Context, maxTime time.Duration) (int64, error)
	// Distinct 返回去重后的值数组（服务端单个回复本身有 16MB 上限）。
	Distinct(ctx context.Context, field string, filter bson.D, maxTime time.Duration) (bson.RawArray, error)
	// Indexes 对应 listIndexes。
	Indexes(ctx context.Context) (Cursor, error)
	// Stats 对应 collStats（固定命令，scale=1）。
	Stats(ctx context.Context) (bson.D, error)
}

// CollectionInfo 是 listCollections 的一项。
type CollectionInfo struct {
	Name     string
	Type     string // collection / view / timeseries
	ReadOnly bool
	Options  bson.D // capped、size、max、viewOn、pipeline、timeseries ...
}

// Database 是库句柄。
type Database interface {
	Collection(name string) Collection
	ListCollections(ctx context.Context) ([]CollectionInfo, error)
	// Stats 对应 dbStats（固定命令，scale=1）。
	Stats(ctx context.Context) (bson.D, error)
}

// DatabaseInfo 是 listDatabases 的一项。
type DatabaseInfo struct {
	Name       string
	SizeOnDisk int64
	Empty      bool
}

// Backend 是连接。
type Backend interface {
	Database(name string) Database
	ListDatabases(ctx context.Context) ([]DatabaseInfo, error)
	Ping(ctx context.Context) error
	ServerVersion(ctx context.Context) (string, error)
	Close(ctx context.Context) error
}
