// @author: kevinkelly
// @license: Apache Licence
// @file: engine.go
// @time: 2026/10/28

// Package mongo 是 MongoDB 引擎的 Go 实现（db_type "mongo"）：在线查询（mongo shell 风格的只读子集）+ 库表元数据（数据字典）。
//
// 分层：
//   - parser.go  词法 + 递归下降解析器，把 shell 语句转成 Extended JSON；不使用 eval；
//   - check.go   CheckUserQuery：方法 / aggregate stage 白名单、操作符黑名单、限量改写、涉及集合；
//   - run.go     RunUser：经 Backend 接口执行并整形结果；
//   - backend.go 与真实驱动之间的接口缝；driver.go 是 mongo-driver/v2 的适配层；
//   - meta.go    库 / 集合 / 字段(采样) / 索引 / CreateStatement。
package mongo

import (
	"context"
	"sort"
	"strings"
	"time"

	"dbtool/internal/engine"
)

const dbType = "mongo"

func init() {
	engine.Register(dbType, func(c engine.Conn) (engine.Engine, error) { return New(c) })
}

// Engine 是 MongoDB 引擎。线程安全（底层驱动客户端自带连接池）。
type Engine struct {
	be        Backend
	password  string // 仅用于从错误文本里抹掉
	defaultDB string
}

// New 创建引擎（懒连接，不发网络请求）。c.DBName 仅作 Query 未指定库时的缺省库；认证库固定为 admin。
func New(c engine.Conn) (*Engine, error) {
	be, err := newDriverBackend(c)
	if err != nil {
		return nil, err
	}
	return &Engine{be: be, password: c.Password, defaultDB: c.DBName}, nil
}

// NewWithBackend 用给定的 Backend 创建引擎（测试 / 上层单测用内存假实现）。
func NewWithBackend(be Backend) *Engine { return &Engine{be: be} }

func (e *Engine) Name() string { return dbType }

func (e *Engine) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return e.be.Close(ctx)
}

func (e *Engine) TestConnection(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := e.be.Ping(ctx); err != nil {
		return e.friendlyError(ctx, err)
	}
	return nil
}

func (e *Engine) ServerVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	v, err := e.be.ServerVersion(ctx)
	return v, e.friendlyError(ctx, err)
}

// AllDatabases 返回账号可见的业务库（排除 admin / local / config），按名称排序。
func (e *Engine) AllDatabases(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	infos, err := e.be.ListDatabases(ctx)
	if err != nil {
		return nil, e.friendlyError(ctx, err)
	}
	out := make([]string, 0, len(infos))
	for _, d := range infos {
		if !systemDBs[d.Name] {
			out = append(out, d.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// AllTables 返回库里的集合名（含 view / timeseries，排除 system.*），按名称排序。
func (e *Engine) AllTables(ctx context.Context, db string) ([]string, error) {
	infos, err := e.collections(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(infos))
	for _, ci := range infos {
		out = append(out, ci.Name)
	}
	return out, nil
}

// AllColumns 返回集合的字段名：对最多 100 个文档采样取顶层字段并集（首次出现顺序），不是 schema。
func (e *Engine) AllColumns(ctx context.Context, db, table string) ([]string, error) {
	cols, err := e.ColumnInfos(ctx, db, table)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out, nil
}

func (e *Engine) collections(ctx context.Context, db string) ([]CollectionInfo, error) {
	if err := ValidDBName(db); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	infos, err := e.be.Database(db).ListCollections(ctx)
	if err != nil {
		return nil, e.friendlyError(ctx, err)
	}
	out := infos[:0:0]
	for _, ci := range infos {
		if !strings.HasPrefix(ci.Name, "system.") {
			out = append(out, ci)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
