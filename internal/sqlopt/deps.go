// @author: kevinkelly
// @license: Apache Licence
// @file: deps.go
// @time: 2026/10/28

package sqlopt

import (
	"context"
	"fmt"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// Repo 是本模块需要的元数据访问（server.Repo() 已实现，测试用 servertest.Repo 或假实现）。
type Repo interface {
	ListInstances() ([]model.Instance, error)
	GetInstance(id uint) (*model.Instance, error)
	EngineConn(inst *model.Instance) engine.Conn
	ConfigValue(item string) (string, bool)
	HasButton(userID uint, code string) (bool, error)
	UserInstanceIDs(userID uint) (map[uint]bool, error)
}

// Session 是到目标库的一个会话（同一条连接，USE/SET 对后续语句有效）。真实实现见 mysqlopen.go。
type Session interface {
	// Query 执行语句并返回结果集（行数与单元格长度受限）。
	Query(ctx context.Context, q string, args ...any) (*Table, error)
	// Drain 执行语句并丢弃结果集，返回行数（SQLTuning 的 PROFILE 用）。超时会 KILL 该语句。
	Drain(ctx context.Context, q string) (int64, error)
	Close() error
}

// Opener 打开目标库会话。db 非空时先 USE。
type Opener interface {
	Open(ctx context.Context, c engine.Conn, db string) (Session, error)
}

// ExecSpec 描述一次外部程序调用。Args 由服务端按固定模板拼装，不经 shell。
type ExecSpec struct {
	Path      string
	Args      []string
	Timeout   time.Duration
	MaxOutput int
}

// ExecResult 是外部程序的运行结果。非零退出码不算错误，由调用方判断。
type ExecResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool // 输出超过上限被截断
	TimedOut  bool
}

// Runner 执行外部程序。真实实现是 osRunner（os/exec），测试用假实现。
type Runner interface {
	Run(ctx context.Context, spec ExecSpec) (*ExecResult, error)
}

// AuditSink 写操作审计（dbt_audit_log）。
type AuditSink interface {
	Record(e *model.AuditEntry) error
}

// cellString 把结果集里的单元格转成字符串。
func cellString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}
