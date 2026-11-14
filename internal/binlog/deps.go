// @author: kevinkelly
// @license: Apache Licence
// @file: deps.go
// @time: 2026/10/28

package binlog

import (
	"context"
	"time"

	"github.com/go-mysql-org/go-mysql/replication"

	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// Repo 是本模块需要的元数据访问（server.Repo() 已实现，测试用假实现）。
type Repo interface {
	ListInstances() ([]model.Instance, error)
	GetInstance(id uint) (*model.Instance, error)
	EngineConn(inst *model.Instance) engine.Conn
	HasButton(userID uint, code string) (bool, error)
	UserInstanceIDs(userID uint) (map[uint]bool, error)
	ConfigValue(item string) (string, bool) // 系统设置的生效值（binlog.max_rows / binlog.max_result_mb）
}

// AuditSink 写操作审计（dbt_audit_log）。
type AuditSink interface {
	Record(e *model.AuditEntry) error
}

// MetaSource 读取表结构（目标库的 information_schema）。表不存在时返回空列表而不是错误。
type MetaSource interface {
	Columns(ctx context.Context, schema, table string) ([]colMeta, error)
}

// EventStream 是一条 binlog 事件流。io.EOF 表示没有更多事件。
type EventStream interface {
	Next(ctx context.Context) (*replication.BinlogEvent, error)
	Close() error
}

// Session 是到目标实例的一个会话：读元信息 + 拉取 binlog。真实实现见 mysqlsource.go，测试用假实现或进程内的假 MySQL 服务。
type Session interface {
	MetaSource
	Info(ctx context.Context) (*ServerInfo, error)
	Databases(ctx context.Context) ([]string, error)
	// Stream 从指定文件和位置开始拉取事件；事件里的时间戳按 loc 展示。
	Stream(ctx context.Context, file string, pos uint32, loc *time.Location) (EventStream, error)
	Close() error
}

// Source 连接目标实例。
type Source interface {
	Connect(ctx context.Context, c engine.Conn) (Session, error)
}
